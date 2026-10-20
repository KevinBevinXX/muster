package plugins

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/authz"
)

// THE CHIEF-DISPATCH LEDGER: how many agents the chief may start on tasks
// (task_dispatch, internal/api/task_routing.go), and which task it already
// started one for.
//
// Starting an agent spends the owner's model credential and runs code against
// the owner's repositories, on the say-so of an agent that is reading text an
// outside party wrote. So the chief gets a small, fixed budget, and each task
// at most ONE chief-started agent per owner's destroy:
//
//   - at most MaxChiefLiveAgents of the agents it started are live at once;
//   - at most MaxChiefDispatchesPerDay starts in any GatedTaskWindow-long day;
//   - a task it already started an agent for answers with that start again
//     (idempotent per task), whatever happened to the agent since — UNTIL ITS
//     OWNER DESTROYS THE AGENT THAT START MADE (the Agents page's Delete, or
//     their mirrored session's agent_destroy): that destroy RELEASES the start
//     (ActionChiefDispatchReleased) and the chief may start ONE new agent on
//     the task again. Operator decision ("Owner's destroy resets the chief's
//     allowance"); the chief re-dispatching on its own is still refused, and a
//     second agent alongside a live one is still the owner's call.
//
// 🔴 THE LEDGER IS THE AUDIT LOG, AS THE GATED-TASK BUDGET'S IS (gated.go). A
// reservation is one ActionChiefDispatch row naming the task; the agent it
// started is one ActionChiefDispatchAgent row. The caps are COUNTED from those
// rows, so the audit trail and the caps cannot disagree, and no new table is
// needed.
const (
	MaxChiefLiveAgents       = 2
	MaxChiefDispatchesPerDay = 10
	ChiefDispatchWindow      = 24 * time.Hour

	// ActionChiefDispatch is the reservation row (object NoteObject(task)).
	ActionChiefDispatch = "task.chief_dispatch"
	// ActionChiefDispatchAgent records the agent a reservation started.
	ActionChiefDispatchAgent = "task.chief_dispatch_agent"
	// ActionChiefDispatchReleased records the owner destroying the agent the
	// task's latest reservation started, which frees the task's one start
	// (ReleaseChiefDispatch). Nothing is deleted: a task's start is spent while
	// its latest ActionChiefDispatch row is newer (by id) than its latest
	// release row.
	ActionChiefDispatchReleased = "task.chief_dispatch_released"
)

var (
	// ErrChiefLiveCap: MaxChiefLiveAgents agents the chief started are still live.
	ErrChiefLiveCap = errors.New("plugins: the chief already has the most live agents it may start")
	// ErrChiefDailyCap: MaxChiefDispatchesPerDay starts in the last day.
	ErrChiefDailyCap = errors.New("plugins: the chief's agent starts for the last day are spent")
)

// NoteObject is the audit_log `object` naming a task.
func NoteObject(id int64) string { return "note:" + strconv.FormatInt(id, 10) }

// ChiefDispatch is what a reservation found or made.
type ChiefDispatch struct {
	// Prior: this task already had a chief start; nothing was reserved.
	Prior bool
	// AgentID is the agent the prior start recorded, nil when it recorded none
	// (it failed before an agent existed).
	AgentID *int64
}

// DispatchStore is the chief-dispatch ledger. Every method acts as ctx's
// principal (the task's owner): audit_log is under row-level security, so it
// reads and appends that owner's rows only.
type DispatchStore interface {
	// ReserveChiefDispatch reserves one chief start for taskID, or reports the
	// prior one (Prior), or refuses: with check's error (the caller's own
	// conditions on the task, run under the reservation's lock after the prior
	// check), or ErrChiefLiveCap / ErrChiefDailyCap. Only a reservation that
	// passed all of them writes anything. detail (nil-safe) is written into the
	// reservation's audit row: what the chief asked for (the kind, its relayed
	// instructions), for the owner to read.
	ReserveChiefDispatch(ctx context.Context, taskID int64, detail map[string]any, check func() error) (ChiefDispatch, error)
	// RecordChiefDispatchAgent records the agent a reservation for taskID started.
	RecordChiefDispatchAgent(ctx context.Context, taskID, agentID int64) error
	// ReleaseChiefDispatch records that the owner destroyed agentID, releasing
	// taskID's one chief start when — and only when — agentID is the agent the
	// task's latest, unreleased reservation started. It reports whether it
	// wrote a release.
	ReleaseChiefDispatch(ctx context.Context, taskID, agentID int64) (bool, error)
	// RecordTaskRouted records the chief routing taskID with tool (an
	// ActionTaskRouted row).
	RecordTaskRouted(ctx context.Context, taskID int64, tool string) error
	// UnroutedTaskEvents is the principal's task events, newest first, at most
	// limit, that the chief has not acted on: not being delivered right now,
	// and with no routing or start recorded for their task at or after the
	// event's latest attempt, or its creation if it has had none (chiefActedOn).
	UnroutedTaskEvents(ctx context.Context, limit int) ([]Message, error)
	// ReserveNoticeRoute / CompleteNoticeRoute / ReleaseNoticeRoute: task_events.go.
	ReserveNoticeRoute(ctx context.Context, pluginID, taskID int64, in MessageIn, to string) (Message, bool, error)
	CompleteNoticeRoute(ctx context.Context, id int64) error
	ReleaseNoticeRoute(ctx context.Context, id int64) error
}

var _ DispatchStore = (*PGStore)(nil)

// liveAgentStatuses are the agent statuses that count as live for the cap.
var liveAgentStatuses = []string{"pending", "provisioning", "running"}

// pendingAgentWindow is how long a reservation with no recorded agent counts
// as a live agent: long enough to cover the create between the reservation and
// RecordChiefDispatchAgent; bounded so a start that failed before any agent
// existed frees its live slot (its day's unit and its task's one start stay
// spent).
const pendingAgentWindow = 10 * time.Minute

// ActionTaskRouted records the chief routing a task without starting an agent
// (task_route_attention, task_forward, or a task_dispatch answered
// already_dispatched while an agent is running on the task): what takes the task off
// the attention queue's "Task to route" list once its event was delivered.
const ActionTaskRouted = "task.chief_routed"

// ReserveChiefDispatch: see DispatchStore.
//
// 🔴 SERIALISED PER OWNER ON A TRANSACTION ADVISORY LOCK, so the prior check,
// both counts and the insert are one decision: two concurrent calls for one task
// reserve once, and two for different tasks cannot both see one slot left —
// which holds for the LIVE cap only because a reservation whose agent is not
// recorded yet counts as live (pendingAgentWindow): the agent row is written
// after this transaction commits, so counting recorded agents alone let
// parallel calls for different tasks each see the same free slot (audit r1).
//
// 🔴 IT RESERVES BEFORE THE AGENT EXISTS, SO A START THAT THEN FAILS STILL
// SPENDS THE DAY'S UNIT AND THE TASK'S ONE START — the fail-closed direction, as
// for the gated-task budget. The caller checks everything else before it
// reserves.
func (s *PGStore) ReserveChiefDispatch(ctx context.Context, taskID int64, detail map[string]any, check func() error) (ChiefDispatch, error) {
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return ChiefDispatch{}, errors.New("plugins: a chief dispatch needs a user principal")
	}
	var out ChiefDispatch
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('muster.chief_dispatch:' || $1::text, 0))`,
			strconv.FormatInt(p.UserID, 10)); err != nil {
			return err
		}
		// 🔴 EVERY LEDGER READ NAMES THE OWNER (user_id = $n / owner_id = $n),
		// not only row-level security: with RLS rolled back (db.RLSOff) another
		// owner's start for the same task id read as this owner's, and its live
		// agents and day's starts spent this owner's caps (audit round 1).
		// The task's start is spent while its latest reservation is newer than
		// its latest release (ReleaseChiefDispatch).
		reserved, released, err := chiefStartRows(ctx, tx, p.UserID, taskID)
		if err != nil {
			return err
		}
		if reserved > released {
			out.Prior = true
			var agentID int64
			err := tx.QueryRow(ctx, `SELECT (detail->>'agent_id')::bigint FROM audit_log
				WHERE action = $1 AND object = $2 AND user_id = $3 ORDER BY id DESC LIMIT 1`,
				ActionChiefDispatchAgent, NoteObject(taskID), p.UserID).Scan(&agentID)
			switch {
			case err == nil:
				out.AgentID = &agentID
			case !errors.Is(err, pgx.ErrNoRows):
				return err
			}
			return nil
		}
		// The caller's conditions on the task, AFTER the prior check (a repeat
		// answers with the first start even though that start moved the task
		// out of open) and BEFORE anything is counted or written.
		if check != nil {
			if err := check(); err != nil {
				return err
			}
		}
		var live int
		if err := tx.QueryRow(ctx, `SELECT
			  (SELECT count(*) FROM agents
			    WHERE status = ANY($2) AND id IN (
			      SELECT (detail->>'agent_id')::bigint FROM audit_log WHERE action = $1 AND user_id = $5))
			+ (SELECT count(*) FROM audit_log r
			    WHERE r.action = $3 AND r.user_id = $5 AND r.at > now() - make_interval(secs => $4)
			      AND NOT EXISTS (SELECT 1 FROM audit_log g WHERE g.action = $1 AND g.user_id = $5 AND g.object = r.object AND g.id > r.id))`,
			ActionChiefDispatchAgent, liveAgentStatuses, ActionChiefDispatch, pendingAgentWindow.Seconds(), p.UserID).Scan(&live); err != nil {
			return err
		}
		if live >= MaxChiefLiveAgents {
			return ErrChiefLiveCap
		}
		var today int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit_log
			WHERE action = $1 AND user_id = $3 AND at > now() - make_interval(secs => $2)`,
			ActionChiefDispatch, ChiefDispatchWindow.Seconds(), p.UserID).Scan(&today); err != nil {
			return err
		}
		if today >= MaxChiefDispatchesPerDay {
			return ErrChiefDailyCap
		}
		if detail == nil {
			detail = map[string]any{}
		}
		return audit(ctx, tx, ActionChiefDispatch, NoteObject(taskID), detail)
	})
	if err != nil {
		return ChiefDispatch{}, err
	}
	return out, nil
}

// RecordChiefDispatchAgent: see DispatchStore.
func (s *PGStore) RecordChiefDispatchAgent(ctx context.Context, taskID, agentID int64) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		return audit(ctx, tx, ActionChiefDispatchAgent, NoteObject(taskID), map[string]any{"agent_id": agentID})
	})
}

// chiefStartRows is the ids of task taskID's latest reservation and latest
// release by owner (0 for none). The start is spent while reserved > released.
func chiefStartRows(ctx context.Context, tx pgx.Tx, owner, taskID int64) (reserved, released int64, err error) {
	err = tx.QueryRow(ctx, `SELECT coalesce(max(id) FILTER (WHERE action = $1), 0), coalesce(max(id) FILTER (WHERE action = $2), 0)
		FROM audit_log WHERE action IN ($1, $2) AND object = $3 AND user_id = $4`,
		ActionChiefDispatch, ActionChiefDispatchReleased, NoteObject(taskID), owner).Scan(&reserved, &released)
	return reserved, released, err
}

// ReleaseChiefDispatch: see DispatchStore. Under the owner's reservation lock,
// so it is one decision with any reservation of the same owner's.
func (s *PGStore) ReleaseChiefDispatch(ctx context.Context, taskID, agentID int64) (bool, error) {
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return false, errors.New("plugins: a chief-dispatch release needs a user principal")
	}
	wrote := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('muster.chief_dispatch:' || $1::text, 0))`,
			strconv.FormatInt(p.UserID, 10)); err != nil {
			return err
		}
		reserved, released, err := chiefStartRows(ctx, tx, p.UserID, taskID)
		if err != nil {
			return err
		}
		if reserved == 0 || released > reserved {
			return nil // no chief start on the task, or its latest is released already
		}
		// The agent the LATEST reservation started: its agent row is newer.
		var started int64
		err = tx.QueryRow(ctx, `SELECT (detail->>'agent_id')::bigint FROM audit_log
			WHERE action = $1 AND object = $2 AND user_id = $3 AND id > $4 ORDER BY id DESC LIMIT 1`,
			ActionChiefDispatchAgent, NoteObject(taskID), p.UserID, reserved).Scan(&started)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil // that start made no agent, so the owner destroyed someone else's
		case err != nil:
			return err
		}
		if started != agentID {
			return nil // another agent on the task: the chief's start is still in use
		}
		wrote = true
		return audit(ctx, tx, ActionChiefDispatchReleased, NoteObject(taskID), map[string]any{"agent_id": agentID})
	})
	return wrote, err
}

// RecordTaskRouted: see DispatchStore.
func (s *PGStore) RecordTaskRouted(ctx context.Context, taskID int64, tool string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		return audit(ctx, tx, ActionTaskRouted, NoteObject(taskID), map[string]any{"tool": tool})
	})
}

// chiefActedOn is THE "the chief acted on this task event" predicate over
// plugin_messages aliased m: a routing (ActionTaskRouted) or a started agent
// (ActionChiefDispatchAgent — not the reservation, which a start that then
// failed also wrote) recorded by the event's owner for its task AT OR AFTER
// THE EVENT'S LATEST ATTEMPT (last_attempt_at), delivered or not: BeginDelivery
// stamps it as the chief's turn on this event starts, and RecordAttempt also
// stamps it on a failed attempt that left the event queued. With no attempt
// at all, the anchor is its creation (created_at). UnroutedTaskEvents lists
// the events it is false for; Retry (retry.go) refuses the ones it is true for
// — one rule, one place.
//
// ⚠ A CONSEQUENCE ON THE "TASK TO ROUTE" SIDE: a queued event hidden by an
// action can reappear there after a later failed attempt moves its anchor
// past that action. Retry is unaffected: an unconfirmed row always went
// through BeginDelivery, so its anchor is the turn that handed it over.
//
// 🔴 PER EVENT, NOT PER TASK (audit round 5). Measured from created_at alone,
// the chief's forward on one event of a task counted as an action on a SECOND
// event of that task it had not yet been handed — whose turn could then be cut
// off, leaving it unconfirmed and refused by Retry for good. An action before
// this event's delivery is the chief acting on something else. (It also
// closes a window created_at left open: created_at is the inserting
// transaction's start, which can precede an action committed while the insert
// waited on the plugin-row lock.)
const chiefActedOn = `EXISTS (SELECT 1 FROM audit_log a
		WHERE a.action IN ('` + ActionTaskRouted + `', '` + ActionChiefDispatchAgent + `') AND a.user_id = m.owner_id
		  AND a.object = 'note:' || m.task_id AND a.at >= coalesce(m.last_attempt_at, m.created_at))`

// UnroutedTaskEvents: see DispatchStore.
//
// 🔴 FILTERED TO TASK EVENTS IN SQL, BEFORE THE LIMIT. Reading the owner's
// queued messages and filtering afterwards let 50 ordinary messages hide every
// task (audit r1) — exactly the backlog a chief that is down leaves behind.
//
// 🔴 DELIVERED IS NOT ROUTED. An event the chief took and did nothing with (its
// turn failed, it called no tool, its call was refused) stays listed until a
// routing or a STARTED AGENT (ActionChiefDispatchAgent — not the reservation,
// which a start that then failed also wrote; delta audit r2) is recorded for
// the task at or after the event's latest attempt, or its creation when it
// has had none (chiefActedOn).
func (s *PGStore) UnroutedTaskEvents(ctx context.Context, limit int) ([]Message, error) {
	if limit <= 0 {
		return []Message{}, nil
	}
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return nil, errors.New("plugins: unrouted task events need a user principal")
	}
	// 🔴 THE OWNER IS IN THE WHERE, BEFORE THE LIMIT, not only in row-level
	// security: with RLS rolled back, other owners' newer events filled the
	// LIMIT and hid this owner's (audit round 1).
	rows, err := s.pool.Query(ctx, `SELECT `+prefixed("m", messageCols)+`
		  FROM plugin_messages m
		 WHERE m.owner_id = $2 AND m.task_id IS NOT NULL AND m.routed_to IS NULL AND m.status <> 'delivering'
		   AND NOT `+chiefActedOn+`
		 ORDER BY m.id DESC LIMIT $1`, limit, p.UserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
