package plugins

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// THE GATED-TASK BUDGET: how many tasks one agent may file through
// task_create_gated (internal/api) in a rolling day.
//
// A gated task cannot run until its owner approves it, so the budget is not what
// keeps a plugin message from causing work. It bounds the OTHER cost: every gated
// task is a `created` push on the owner's phone and a card on their board, and the
// chief files them while reading text an outside party wrote. Twenty a day is
// more than a person triages from one inbox; a chief talked into a loop stops
// there.
const (
	MaxGatedTasksPerDay = 20
	GatedTaskWindow     = 24 * time.Hour
)

// ActionGatedTask is the audit_log action one reserved gated task appends. The
// budget is COUNTED from these rows (see [Store.ReserveGatedTask]), so the audit
// trail and the cap cannot disagree.
const ActionGatedTask = "plugin.task_gated"

// ErrGatedTaskCap: the agent already reserved MaxGatedTasksPerDay gated tasks in
// the last GatedTaskWindow.
var ErrGatedTaskCap = errors.New("plugins: the gated-task budget for the last day is spent")

// AgentObject is the audit_log `object` naming an agent.
func AgentObject(id int64) string { return "agent:" + strconv.FormatInt(id, 10) }

// DeliveredTo reports whether message m was handed to agent agentID of owner
// ownerID: it is that owner's, the agent it went to is agentID, and it is
// delivering, delivered or unconfirmed (each means the chief may have read it).
//
// 🔴 `delivering` IS THE CASE THE FEATURE EXISTS FOR. It is the state DURING the
// turn that hands the message to the chief — the turn on which the chief learns
// of it and files a task for it (Deliverer.Deliver writes it before the turn
// runs). Refusing it would refuse the core flow and accept only a citation made
// on some later turn. A `queued` message names no agent (0015's CHECK), so it is
// refused by the agent conjunct as well as by status.
//
// 🔴 THE OWNER CONJUNCT IS NOT REDUNDANT WITH ROW-LEVEL SECURITY. Under Postgres
// another owner's message is invisible anyway; a Store without row-level
// security (every fake, and any future one) would hand it over, and a caller
// that trusted the store's scoping would then file a task citing a stranger's
// message. The check is the caller's, so it holds whatever store is wired.
func DeliveredTo(m Message, agentID, ownerID int64) bool {
	if ownerID <= 0 || m.OwnerID != ownerID || m.AgentID == nil || *m.AgentID != agentID {
		return false
	}
	return m.Status == StatusDelivering || m.Status == StatusDelivered || m.Status == StatusUnconfirmed
}

// ReserveGatedTask spends one unit of agentID's gated-task budget, appending the
// ActionGatedTask audit row that IS the unit, or returns ErrGatedTaskCap.
//
// 🔴 IT RESERVES BEFORE THE TASK EXISTS, SO A CREATE THAT THEN FAILS STILL
// SPENDS THE UNIT. That is the fail-closed direction on purpose: counting after
// the create would need the count and the insert in one transaction across two
// stores, and without that two concurrent calls each see 19 and both land. The
// caller validates everything it can BEFORE reserving, so only an infrastructure
// failure costs a unit.
//
// The agent's row is locked first, so reservations for one agent are serialised
// and the count cannot be raced past the cap.
func (s *PGStore) ReserveGatedTask(ctx context.Context, agentID int64, messageID *int64) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var one int
		err := tx.QueryRow(ctx, `SELECT 1 FROM agents WHERE id = $1 FOR UPDATE`, agentID).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit_log
			WHERE action = $1 AND object = $2 AND at > now() - make_interval(secs => $3)`,
			ActionGatedTask, AgentObject(agentID), GatedTaskWindow.Seconds()).Scan(&n); err != nil {
			return err
		}
		if n >= MaxGatedTasksPerDay {
			return ErrGatedTaskCap
		}
		detail := map[string]any{}
		if messageID != nil {
			detail["message_id"] = *messageID
		}
		return audit(ctx, tx, ActionGatedTask, AgentObject(agentID), detail)
	})
}
