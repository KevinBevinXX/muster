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
// at most ONE chief-started agent, ever:
//
//   - at most MaxChiefLiveAgents of the agents it started are live at once;
//   - at most MaxChiefDispatchesPerDay starts in any GatedTaskWindow-long day;
//   - a task it already started an agent for answers with that start again
//     (idempotent per task), whatever happened to the agent since. A second
//     agent on the same task is the owner's call, from the task's card.
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
	// passed all of them writes anything.
	ReserveChiefDispatch(ctx context.Context, taskID int64, check func() error) (ChiefDispatch, error)
	// RecordChiefDispatchAgent records the agent a reservation for taskID started.
	RecordChiefDispatchAgent(ctx context.Context, taskID, agentID int64) error
	// RecordTaskRouted records the chief routing taskID with tool (an
	// ActionTaskRouted row).
	RecordTaskRouted(ctx context.Context, taskID int64, tool string) error
	// UnroutedTaskEvents is the principal's task events, newest first, at most
	// limit, that the chief has not acted on: not being delivered right now,
	// and with no routing or start recorded for their task since the event.
	UnroutedTaskEvents(ctx context.Context, limit int) ([]Message, error)
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
func (s *PGStore) ReserveChiefDispatch(ctx context.Context, taskID int64, check func() error) (ChiefDispatch, error) {
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
		var prior int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = $1 AND object = $2`,
			ActionChiefDispatch, NoteObject(taskID)).Scan(&prior); err != nil {
			return err
		}
		if prior > 0 {
			out.Prior = true
			var agentID int64
			err := tx.QueryRow(ctx, `SELECT (detail->>'agent_id')::bigint FROM audit_log
				WHERE action = $1 AND object = $2 ORDER BY id DESC LIMIT 1`,
				ActionChiefDispatchAgent, NoteObject(taskID)).Scan(&agentID)
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
			      SELECT (detail->>'agent_id')::bigint FROM audit_log WHERE action = $1))
			+ (SELECT count(*) FROM audit_log r
			    WHERE r.action = $3 AND r.at > now() - make_interval(secs => $4)
			      AND NOT EXISTS (SELECT 1 FROM audit_log g WHERE g.action = $1 AND g.object = r.object))`,
			ActionChiefDispatchAgent, liveAgentStatuses, ActionChiefDispatch, pendingAgentWindow.Seconds()).Scan(&live); err != nil {
			return err
		}
		if live >= MaxChiefLiveAgents {
			return ErrChiefLiveCap
		}
		var today int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit_log
			WHERE action = $1 AND at > now() - make_interval(secs => $2)`,
			ActionChiefDispatch, ChiefDispatchWindow.Seconds()).Scan(&today); err != nil {
			return err
		}
		if today >= MaxChiefDispatchesPerDay {
			return ErrChiefDailyCap
		}
		return audit(ctx, tx, ActionChiefDispatch, NoteObject(taskID), map[string]any{})
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

// RecordTaskRouted: see DispatchStore.
func (s *PGStore) RecordTaskRouted(ctx context.Context, taskID int64, tool string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		return audit(ctx, tx, ActionTaskRouted, NoteObject(taskID), map[string]any{"tool": tool})
	})
}

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
// the task after the event.
func (s *PGStore) UnroutedTaskEvents(ctx context.Context, limit int) ([]Message, error) {
	if limit <= 0 {
		return []Message{}, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+prefixed("m", messageCols)+`
		  FROM plugin_messages m
		 WHERE m.task_id IS NOT NULL AND m.status <> 'delivering'
		   AND NOT EXISTS (SELECT 1 FROM audit_log a
		                    WHERE a.action IN ($1, $2) AND a.object = 'note:' || m.task_id AND a.at >= m.created_at)
		 ORDER BY m.id DESC LIMIT $3`, ActionTaskRouted, ActionChiefDispatchAgent, limit)
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
