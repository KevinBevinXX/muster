package agents

import (
	"context"
	"time"
)

// The stopped-agent reaper's half of the store (internal/api agent_reap.go):
// which agents have been `stopped` long enough to be destroyed, and the claim
// one pod takes on such an agent before it starts the teardown.
//
// agents.stopped_at is stamped by migration 0045's trigger whenever the row's
// status moves INTO `stopped` (the trigger also clears reap_claimed_at then).
// It is kept when the row leaves `stopped`, so it reads "last stopped at"; the
// predicate below pairs it with `status = 'stopped'`. A Start moves the row
// out of `stopped`, so the next Stop stamps a new time: Start resets the clock.
//
// 🔴 BOTH CLOCKS ARE THE DATABASE'S. The trigger stamps now(), and the
// threshold below is compared against now() in the same statement, so two pods
// with skewed clocks agree on which rows are due.

// StoppedReapClaimLease is how long a reaper's claim on a row excludes another
// claim. A claim is normally followed at once by the teardown, which deletes
// the row; the lease only matters when the teardown removed the instance but
// not the row, and then the row is claimable again after it.
const StoppedReapClaimLease = time.Hour

// StoppedAgent is a reap candidate: enough to claim it and to log the reap.
type StoppedAgent struct {
	ID        int64
	Name      string
	OwnerID   int64
	NoteID    *int64
	StoppedAt time.Time
}

// reapableStopped is THE reap predicate, the one spelling both the scan and the
// claim use, so they cannot disagree about which rows are due. $1 is the stopped
// window in microseconds, $2 the claim lease in microseconds.
//
//   - status = 'stopped': only the STORED status. `error`, `running`,
//     `provisioning` and `pending` are never reaped.
//   - an empty worker_pool: a pool worker is never reaped. Pool workers are
//     long-lived agents whose membership is fixed at creation (docs/jobs.md
//     "Worker pools"), and a stopped one is a pool member to Start again.
//   - first_started_at set: the agent ran at least once (migration 0045). An
//     agent saved for later and never started is never reaped.
//   - stopped_at <= now() - window: stopped for at least the window.
//   - no live claim: another pod is not already tearing it down.
//
// The agent is NOT excluded here: the agent rule is internal/api's
// chiefPredicate, and the reaper reaches the teardown only through
// destroyAgent, which refuses the agent before it claims anything.
const reapableStopped = `status = 'stopped'
	AND worker_pool = ''
	AND first_started_at IS NOT NULL
	AND stopped_at <= now() - ($1::bigint * interval '1 microsecond')
	AND (reap_claimed_at IS NULL OR reap_claimed_at <= now() - ($2::bigint * interval '1 microsecond'))`

// ListReapableStopped returns every agent the caller can see that matches
// reapableStopped for the window after, oldest stop first.
func (s *PGStore) ListReapableStopped(ctx context.Context, after time.Duration) ([]StoppedAgent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, owner_id, note_id, stopped_at FROM agents
		 WHERE `+reapableStopped+`
		 ORDER BY stopped_at, id`,
		after.Microseconds(), StoppedReapClaimLease.Microseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]StoppedAgent, 0)
	for rows.Next() {
		var a StoppedAgent
		if err := rows.Scan(&a.ID, &a.Name, &a.OwnerID, &a.NoteID, &a.StoppedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ClaimStoppedReap takes the reaper's claim on agent id and reports whether it
// won. It wins only while the row STILL matches reapableStopped for the window
// after, re-checked in the claim's own statement: an agent Started since the
// scan read it is `provisioning` or `running` (not `stopped`), and one Started
// and stopped again carries a stopped_at from moments ago (not past the
// window), so either way the claim loses. Two pods claiming at once serialise
// on the row lock, and the second re-reads the winner's reap_claimed_at, which
// is inside the lease: it loses.
func (s *PGStore) ClaimStoppedReap(ctx context.Context, id int64, after time.Duration) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE agents SET reap_claimed_at = now()
		 WHERE id = $3 AND `+reapableStopped,
		after.Microseconds(), StoppedReapClaimLease.Microseconds(), id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
