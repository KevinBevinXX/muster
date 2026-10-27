package plugins

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/authz"
)

// THE AGENT-DISPATCH LEDGER: how many agents the agent may start on tasks
// (task_dispatch, internal/api/task_routing.go), and which task it already
// started one for.
//
// Starting an agent spends the owner's model credential and runs code against
// the owner's repositories, on the say-so of an agent that is reading text an
// outside party wrote. So the agent gets a small, fixed budget, and each task
// at most ONE agent-started agent per owner's destroy:
//
//   - at most MaxLiveDispatchedAgents of the agents it started are live at once;
//   - at most MaxDispatchesPerDay starts in any GatedTaskWindow-long day;
//   - a task it already started an agent for answers with that start again
//     (idempotent per task), whatever happened to the agent since — UNTIL ITS
//     OWNER DESTROYS THE AGENT THAT START MADE (the Agents page's Delete, or
//     their mirrored session's agent_destroy): that destroy RELEASES the start
//     (ActionTaskDispatchReleased) and the agent may start ONE new agent on
//     the task again. Operator decision ("Owner's destroy resets the agent's
//     allowance"); the agent re-dispatching on its own is still refused, and a
//     second agent alongside a live one is still the owner's call.
//   - when the task becomes `complete`, the agent the task's latest, unreleased
//     start made is STOPPED, not destroyed (api's stopDispatchedAgentOnComplete, via
//     DispatchStartedAgent): a stopped agent is not in liveAgentStatuses, so it
//     stops holding one of the MaxLiveDispatchedAgents slots. The start stays SPENT
//     — a stop writes no release — so the agent answers already_dispatched for
//     the task as before. `ready_for_review` stops nothing.
//
// 🔴 THE LEDGER IS THE AUDIT LOG, AS THE GATED-TASK BUDGET'S IS (gated.go). A
// reservation is one ActionTaskDispatch row naming the task; the agent it
// started is one ActionTaskDispatchAgent row. The caps are COUNTED from those
// rows, so the audit trail and the caps cannot disagree, and no new table is
// needed.
const (
	MaxLiveDispatchedAgents = 5
	MaxDispatchesPerDay     = 10
	TaskDispatchWindow      = 24 * time.Hour

	// ActionTaskDispatch is the reservation row (object NoteObject(task)).
	ActionTaskDispatch = "task.chief_dispatch"
	// ActionTaskDispatchAgent records the agent a reservation started.
	ActionTaskDispatchAgent = "task.chief_dispatch_agent"
	// ActionTaskDispatchReleased records the owner destroying the agent the
	// task's latest reservation started, which frees the task's one start
	// (ReleaseTaskDispatch). Nothing is deleted: a task's start is spent while
	// its latest ActionTaskDispatch row is newer (by id) than its latest
	// release row.
	ActionTaskDispatchReleased = "task.chief_dispatch_released"
)

var (
	// ErrDispatchLiveCap: MaxLiveDispatchedAgents agents the agent started are still live.
	ErrDispatchLiveCap = errors.New("plugins: the agent already has the most live agents it may start")
	// ErrDispatchDailyCap: MaxDispatchesPerDay starts in the last day.
	ErrDispatchDailyCap = errors.New("plugins: this owner's task-dispatch starts for the last day are spent")
)

// NoteObject is the audit_log `object` naming a task.
func NoteObject(id int64) string { return "note:" + strconv.FormatInt(id, 10) }

// TaskDispatch is what a reservation found or made.
type TaskDispatch struct {
	// Prior: this task already had an agent start; nothing was reserved.
	Prior bool
	// AgentID is the agent the prior start recorded, nil when it recorded none
	// (it failed before an agent existed).
	AgentID *int64
	// ReservationID is the audit row this call wrote (0 for Prior): what
	// ReleaseRefusedTaskDispatch names, so a release frees THIS reservation
	// and never a later one.
	ReservationID int64
	// Budget is the budget a cap refusal (ErrDispatchLiveCap, ErrDispatchDailyCap)
	// was decided on; nil otherwise.
	Budget *DispatchBudget
}

// DispatchBudget is the agent's dispatch budget as the agent is shown it: every
// task_dispatch answer and refusal, and task_route_attention, carry it, so the
// caps are DATA read at the call — never numbers the agent remembers from an
// older conversation (its session survives restarts, and after the live cap
// was raised it kept quoting the old one, "at most 2").
type DispatchBudget struct {
	// Live is how many agent-started agents count as live now: len(LiveAgents).
	Live    int `json:"live"`
	LiveCap int `json:"live_cap"`
	// Today is the agent's starts (reservations) in the last TaskDispatchWindow.
	Today    int `json:"today"`
	DailyCap int `json:"daily_cap"`
	// LiveAgents is what Live counts: the recorded agents, oldest first, then
	// the starts whose agent is not recorded yet (ID and Name nil).
	LiveAgents []LiveDispatchedAgent `json:"live_agents"`
}

// LiveFull reports whether the live cap refuses another start. THE live-cap
// predicate: ReserveTaskDispatch refuses on it, Full offers on it.
func (b DispatchBudget) LiveFull() bool { return b.Live >= b.LiveCap }

// DailyFull reports whether the daily cap refuses another start.
func (b DispatchBudget) DailyFull() bool { return b.Today >= b.DailyCap }

// Full reports whether either cap refuses another start.
func (b DispatchBudget) Full() bool { return b.LiveFull() || b.DailyFull() }

// LiveDispatchedAgent is one entry the live count counts. A recorded agent has ID and
// Name; a reservation whose agent is not recorded yet — a start in flight, or
// one that failed before an agent existed and was not released, counted for
// pendingAgentWindow — has both nil (JSON null) and only Task.
type LiveDispatchedAgent struct {
	ID   *int64  `json:"id"`
	Name *string `json:"name"`
	// Task is the task the start was for (the ledger row's object).
	Task int64 `json:"task"`
}

// DispatchStore is the agent-dispatch ledger. Every method acts as ctx's
// principal (the task's owner): audit_log is under row-level security, so it
// reads and appends that owner's rows only.
type DispatchStore interface {
	// ReserveTaskDispatch reserves one agent start for taskID, or reports the
	// prior one (Prior), or refuses: with check's error (the caller's own
	// conditions on the task, run under the reservation's lock after the prior
	// check), or ErrDispatchLiveCap / ErrDispatchDailyCap. Only a reservation that
	// passed all of them writes anything. detail (nil-safe) is written into the
	// reservation's audit row: what the agent asked for (the kind, its relayed
	// instructions), as a ledger. ⚠ No muster page or route reads it back (#149):
	// it is read with SQL (docs/agent.md); what the agent was told is also its
	// first message, in its transcript.
	// A cap refusal returns, with its error, the budget it was decided on
	// (TaskDispatch.Budget).
	ReserveTaskDispatch(ctx context.Context, taskID int64, detail map[string]any, check func() error) (TaskDispatch, error)
	// TaskDispatchBudget is the principal's agent-dispatch budget now
	// (dispatchBudget, the count the caps compare). Read-only.
	TaskDispatchBudget(ctx context.Context) (DispatchBudget, error)
	// RecordTaskDispatchAgent records the agent a reservation for taskID started.
	RecordTaskDispatchAgent(ctx context.Context, taskID, agentID int64) error
	// ReleaseTaskDispatch records that the owner destroyed agentID, releasing
	// taskID's one agent start when — and only when — agentID is the agent the
	// task's latest, unreleased reservation started. It reports whether it
	// wrote a release.
	ReleaseTaskDispatch(ctx context.Context, taskID, agentID int64) (bool, error)
	// ReleaseRefusedTaskDispatch releases the reservation reservationID (a
	// TaskDispatch.ReservationID) of taskID when — and only when — it is still
	// the task's latest, unreleased reservation and recorded no agent: the
	// caller reserved, then the agent's create was REFUSED before any agent row
	// existed (no pool account, an uncredentialed model, a repository no longer
	// connected), so nothing was started and the task's one start must not stay
	// spent. The day's unit stays spent (fail-closed). It reports whether it
	// wrote a release.
	ReleaseRefusedTaskDispatch(ctx context.Context, taskID, reservationID int64) (bool, error)
	// DispatchStartedAgent is the agent the task's latest, unreleased agent start
	// made, for the principal (the task's owner). ok is false when the task has
	// no agent start, its latest start is released, or that start recorded no
	// agent. It is what a task becoming complete stops (api's
	// stopDispatchedAgentOnComplete).
	DispatchStartedAgent(ctx context.Context, taskID int64) (agentID int64, ok bool, err error)
	// RecordTaskRouted records the agent routing taskID with tool (an
	// ActionTaskRouted row).
	RecordTaskRouted(ctx context.Context, taskID int64, tool string) error
	// UnroutedTaskEvents is the principal's task events, newest first, at most
	// limit, that the agent has not acted on: not being delivered right now,
	// and with no routing or start recorded for their task at or after the
	// event's latest attempt, or its creation if it has had none (agentActedOn).
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
// RecordTaskDispatchAgent; bounded so a start that failed before any agent
// existed frees its live slot (its day's unit and its task's one start stay
// spent).
const pendingAgentWindow = 10 * time.Minute

// ActionTaskRouted records the agent routing a task without starting an agent
// (task_route_attention, task_forward, or a task_dispatch answered
// already_dispatched while an agent is running on the task): what takes the task off
// the attention queue's "Task to route" list once its event was delivered.
const ActionTaskRouted = "task.chief_routed"

// ReserveTaskDispatch: see DispatchStore.
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
func (s *PGStore) ReserveTaskDispatch(ctx context.Context, taskID int64, detail map[string]any, check func() error) (TaskDispatch, error) {
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return TaskDispatch{}, errors.New("plugins: an agent dispatch needs a user principal")
	}
	var out TaskDispatch
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
		// its latest release (ReleaseTaskDispatch).
		reserved, released, err := dispatchStartRows(ctx, tx, p.UserID, taskID)
		if err != nil {
			return err
		}
		if reserved > released {
			out.Prior = true
			// The agent of THIS (the latest) reservation only — its row is newer
			// than the reservation. An older agent row is a start that was
			// released (its agent destroyed), and naming it would answer with an
			// agent that no longer exists (audit round 2).
			agentID, ok, err := latestStartAgent(ctx, tx, p.UserID, taskID, reserved)
			if err != nil {
				return err
			}
			if ok {
				out.AgentID = &agentID
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
		// The caps are decided on the SAME budget the agent is shown
		// (dispatchBudget): the refusal carries it (out.Budget).
		b, err := dispatchBudget(ctx, tx, p.UserID)
		if err != nil {
			return err
		}
		if b.LiveFull() {
			out.Budget = &b
			return ErrDispatchLiveCap
		}
		if b.DailyFull() {
			out.Budget = &b
			return ErrDispatchDailyCap
		}
		if detail == nil {
			detail = map[string]any{}
		}
		if err := audit(ctx, tx, ActionTaskDispatch, NoteObject(taskID), detail); err != nil {
			return err
		}
		// Under the owner's lock: the newest reservation row for the task is
		// the one just written.
		written, _, err := dispatchStartRows(ctx, tx, p.UserID, taskID)
		out.ReservationID = written
		return err
	})
	if err != nil {
		// Only a cap refusal carries anything: the budget it was decided on.
		return TaskDispatch{Budget: out.Budget}, err
	}
	return out, nil
}

// TaskDispatchBudget: see DispatchStore. A read: it writes nothing and takes no
// reservation lock, so a reservation racing it can make it one start stale —
// it is what the agent is SHOWN, never what a reservation is decided on (that
// reads dispatchBudget again under the lock).
func (s *PGStore) TaskDispatchBudget(ctx context.Context) (DispatchBudget, error) {
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return DispatchBudget{}, errors.New("plugins: reading the agent's budget needs a user principal")
	}
	var b DispatchBudget
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		b, err = dispatchBudget(ctx, tx, p.UserID)
		return err
	})
	return b, err
}

// dispatchBudget is owner's agent-dispatch budget, counted from the ledger: THE
// one count, shared by ReserveTaskDispatch (whose caps compare it) and
// TaskDispatchBudget (what the agent is shown). Live is len(LiveAgents), so the
// list and the number cannot disagree.
//
//   - a RECORDED agent (an ActionTaskDispatchAgent row of owner's naming it)
//     whose stored status is live (liveAgentStatuses), once per agent;
//   - a reservation from the last pendingAgentWindow with no agent row and no
//     release row (ReleaseRefusedTaskDispatch) for its task newer than it: a
//     start in flight, or one that failed before any agent existed and was not
//     released — listed with ID and Name nil. A start released after a refused
//     create made no agent and holds no slot.
//
// Today is owner's reservations in the last TaskDispatchWindow.
//
// 🔴 EVERY LEDGER READ NAMES THE OWNER (user_id = $n), not only row-level
// security: see ReserveTaskDispatch.
func dispatchBudget(ctx context.Context, tx pgx.Tx, owner int64) (DispatchBudget, error) {
	b := DispatchBudget{LiveCap: MaxLiveDispatchedAgents, DailyCap: MaxDispatchesPerDay, LiveAgents: []LiveDispatchedAgent{}}
	rows, err := tx.Query(ctx, `SELECT a.id, a.name, l.object FROM agents a
		  JOIN LATERAL (SELECT object FROM audit_log
		                 WHERE action = $1 AND user_id = $3 AND (detail->>'agent_id')::bigint = a.id
		                 ORDER BY id DESC LIMIT 1) l ON true
		 WHERE a.status = ANY($2)
		 ORDER BY a.id`,
		ActionTaskDispatchAgent, liveAgentStatuses, owner)
	if err != nil {
		return DispatchBudget{}, err
	}
	for rows.Next() {
		var (
			id     int64
			name   string
			object string
		)
		if err := rows.Scan(&id, &name, &object); err != nil {
			rows.Close()
			return DispatchBudget{}, err
		}
		b.LiveAgents = append(b.LiveAgents, LiveDispatchedAgent{ID: &id, Name: &name, Task: noteObjectID(object)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return DispatchBudget{}, err
	}
	rows, err = tx.Query(ctx, `SELECT r.object FROM audit_log r
		 WHERE r.action = $1 AND r.user_id = $4 AND r.at > now() - make_interval(secs => $3)
		   AND NOT EXISTS (SELECT 1 FROM audit_log g WHERE g.action = $2 AND g.user_id = $4 AND g.object = r.object AND g.id > r.id)
		   -- a start released after a refused create made no agent: it holds no slot
		   AND NOT EXISTS (SELECT 1 FROM audit_log x WHERE x.action = $5 AND x.user_id = $4 AND x.object = r.object AND x.id > r.id)
		 ORDER BY r.id`,
		ActionTaskDispatch, ActionTaskDispatchAgent, pendingAgentWindow.Seconds(), owner, ActionTaskDispatchReleased)
	if err != nil {
		return DispatchBudget{}, err
	}
	for rows.Next() {
		var object string
		if err := rows.Scan(&object); err != nil {
			rows.Close()
			return DispatchBudget{}, err
		}
		b.LiveAgents = append(b.LiveAgents, LiveDispatchedAgent{Task: noteObjectID(object)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return DispatchBudget{}, err
	}
	b.Live = len(b.LiveAgents)
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit_log
		WHERE action = $1 AND user_id = $3 AND at > now() - make_interval(secs => $2)`,
		ActionTaskDispatch, TaskDispatchWindow.Seconds(), owner).Scan(&b.Today); err != nil {
		return DispatchBudget{}, err
	}
	return b, nil
}

// noteObjectID is the task id a NoteObject names, 0 when object is not one.
func noteObjectID(object string) int64 {
	id, err := strconv.ParseInt(strings.TrimPrefix(object, "note:"), 10, 64)
	if err != nil || !strings.HasPrefix(object, "note:") {
		return 0
	}
	return id
}

// RecordTaskDispatchAgent: see DispatchStore.
func (s *PGStore) RecordTaskDispatchAgent(ctx context.Context, taskID, agentID int64) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		return audit(ctx, tx, ActionTaskDispatchAgent, NoteObject(taskID), map[string]any{"agent_id": agentID})
	})
}

// dispatchStartRows is the ids of task taskID's latest reservation and latest
// release by owner (0 for none). The start is spent while reserved > released.
func dispatchStartRows(ctx context.Context, tx pgx.Tx, owner, taskID int64) (reserved, released int64, err error) {
	err = tx.QueryRow(ctx, `SELECT coalesce(max(id) FILTER (WHERE action = $1), 0), coalesce(max(id) FILTER (WHERE action = $2), 0)
		FROM audit_log WHERE action IN ($1, $2) AND object = $3 AND user_id = $4`,
		ActionTaskDispatch, ActionTaskDispatchReleased, NoteObject(taskID), owner).Scan(&reserved, &released)
	return reserved, released, err
}

// latestStartAgent is the agent recorded for owner's reservation row reserved
// on task taskID: the newest ActionTaskDispatchAgent row for the task that is
// newer than that reservation. ok is false when that start recorded no agent
// (it failed before one existed). An agent row OLDER than the reservation
// belongs to an earlier, released start and is never answered.
//
// The one reading of "the agent the agent's start made", shared by
// ReserveTaskDispatch (the already_dispatched answer), ReleaseTaskDispatch
// (whose destroy frees the start) and DispatchStartedAgent (whose agent a
// completed task stops).
func latestStartAgent(ctx context.Context, tx pgx.Tx, owner, taskID, reserved int64) (int64, bool, error) {
	var agentID int64
	err := tx.QueryRow(ctx, `SELECT (detail->>'agent_id')::bigint FROM audit_log
		WHERE action = $1 AND object = $2 AND user_id = $3 AND id > $4 ORDER BY id DESC LIMIT 1`,
		ActionTaskDispatchAgent, NoteObject(taskID), owner, reserved).Scan(&agentID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, err
	}
	return agentID, true, nil
}

// DispatchStartedAgent: see DispatchStore. A read: it writes nothing, and it takes
// no reservation lock — a reservation racing it is for ANOTHER start (this
// task's is spent while it is unreleased), and a release racing it means the
// owner is destroying the agent, which a Stop of it cannot make worse.
func (s *PGStore) DispatchStartedAgent(ctx context.Context, taskID int64) (int64, bool, error) {
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return 0, false, errors.New("plugins: reading an agent start needs a user principal")
	}
	var (
		agentID int64
		found   bool
	)
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		reserved, released, err := dispatchStartRows(ctx, tx, p.UserID, taskID)
		if err != nil {
			return err
		}
		if reserved == 0 || released > reserved {
			return nil // no agent start on the task, or its latest is released
		}
		agentID, found, err = latestStartAgent(ctx, tx, p.UserID, taskID, reserved)
		return err
	})
	if err != nil || !found {
		return 0, false, err
	}
	return agentID, true, nil
}

// ReleaseTaskDispatch: see DispatchStore. Under the owner's reservation lock,
// so it is one decision with any reservation of the same owner's.
func (s *PGStore) ReleaseTaskDispatch(ctx context.Context, taskID, agentID int64) (bool, error) {
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return false, errors.New("plugins: an agent-dispatch release needs a user principal")
	}
	wrote := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('muster.chief_dispatch:' || $1::text, 0))`,
			strconv.FormatInt(p.UserID, 10)); err != nil {
			return err
		}
		reserved, released, err := dispatchStartRows(ctx, tx, p.UserID, taskID)
		if err != nil {
			return err
		}
		if reserved == 0 || released > reserved {
			return nil // no agent start on the task, or its latest is released already
		}
		// The agent the LATEST reservation started: its agent row is newer.
		started, ok, err := latestStartAgent(ctx, tx, p.UserID, taskID, reserved)
		if err != nil {
			return err
		}
		if !ok {
			return nil // that start made no agent, so the owner destroyed someone else's
		}
		if started != agentID {
			return nil // another agent on the task: the agent's start is still in use
		}
		wrote = true
		return audit(ctx, tx, ActionTaskDispatchReleased, NoteObject(taskID), map[string]any{"agent_id": agentID})
	})
	return wrote, err
}

// ReleaseRefusedTaskDispatch: see DispatchStore. Under the owner's
// reservation lock, as ReleaseTaskDispatch.
func (s *PGStore) ReleaseRefusedTaskDispatch(ctx context.Context, taskID, reservationID int64) (bool, error) {
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return false, errors.New("plugins: an agent-dispatch release needs a user principal")
	}
	if reservationID <= 0 {
		return false, nil
	}
	wrote := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('muster.chief_dispatch:' || $1::text, 0))`,
			strconv.FormatInt(p.UserID, 10)); err != nil {
			return err
		}
		reserved, released, err := dispatchStartRows(ctx, tx, p.UserID, taskID)
		if err != nil {
			return err
		}
		if reserved != reservationID || released > reserved {
			return nil // not this task's latest unreleased reservation
		}
		if _, ok, err := latestStartAgent(ctx, tx, p.UserID, taskID, reserved); err != nil || ok {
			return err // an agent was started: only the owner's destroy releases it
		}
		wrote = true
		return audit(ctx, tx, ActionTaskDispatchReleased, NoteObject(taskID), map[string]any{"refused_reservation": reservationID})
	})
	return wrote, err
}

// RecordTaskRouted: see DispatchStore.
func (s *PGStore) RecordTaskRouted(ctx context.Context, taskID int64, tool string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		return audit(ctx, tx, ActionTaskRouted, NoteObject(taskID), map[string]any{"tool": tool})
	})
}

// agentActedOn is THE "the agent acted on this task event" predicate over
// plugin_messages aliased m: a routing (ActionTaskRouted) or a started agent
// (ActionTaskDispatchAgent — not the reservation, which a start that then
// failed also wrote) recorded by the event's owner for its task AT OR AFTER
// THE EVENT'S LATEST ATTEMPT (last_attempt_at), delivered or not: BeginDelivery
// stamps it as the agent's turn on this event starts, and RecordAttempt also
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
// the agent's forward on one event of a task counted as an action on a SECOND
// event of that task it had not yet been handed — whose turn could then be cut
// off, leaving it unconfirmed and refused by Retry for good. An action before
// this event's delivery is the agent acting on something else. (It also
// closes a window created_at left open: created_at is the inserting
// transaction's start, which can precede an action committed while the insert
// waited on the plugin-row lock.)
const agentActedOn = `EXISTS (SELECT 1 FROM audit_log a
		WHERE a.action IN ('` + ActionTaskRouted + `', '` + ActionTaskDispatchAgent + `') AND a.user_id = m.owner_id
		  AND a.object = 'note:' || m.task_id AND a.at >= coalesce(m.last_attempt_at, m.created_at))`

// UnroutedTaskEvents: see DispatchStore.
//
// 🔴 FILTERED TO TASK EVENTS IN SQL, BEFORE THE LIMIT. Reading the owner's
// queued messages and filtering afterwards let 50 ordinary messages hide every
// task (audit r1) — exactly the backlog an agent that is down leaves behind.
//
// 🔴 DELIVERED IS NOT ROUTED. An event the agent took and did nothing with (its
// turn failed, it called no tool, its call was refused) stays listed until a
// routing or a STARTED AGENT (ActionTaskDispatchAgent — not the reservation,
// which a start that then failed also wrote; delta audit r2) is recorded for
// the task at or after the event's latest attempt, or its creation when it
// has had none (agentActedOn).
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
		   AND NOT `+agentActedOn+`
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
