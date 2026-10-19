package plugins

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// RETRYING AN UNCONFIRMED MESSAGE.
//
// A message settles `unconfirmed` when its delivery turn was cut off after the
// text may have been pasted (a rollout, a runtime error mid-turn). That state is
// "never sent again" BY DEFAULT, because a second paste could make the chief act
// twice. The owner may decide otherwise: Retry puts the message back in the
// queue, and the ordinary delivery path (session claim, chat turn, the chief's
// tools) takes it from there.
//
// 🔴 ONE PREDICATE DECIDES WHAT IS RETRYABLE, used by the page's list AND by the
// requeue's own UPDATE, so the button is never drawn for a message the write
// would refuse, and the write re-checks it atomically (a double click finds the
// message queued and is refused). Retryable means: `unconfirmed`, and nothing
// the first turn did is still live or done — no reply draft pending, approved,
// sent or itself unconfirmed (another turn would draft a second answer), and no
// session route that is routed, answered or relayed, no gated task filed citing
// it (gatedTaskCited) — and its plugin is not
// revoked (revokedPlugin): a revoked plugin's messages are never handed to the
// chief again, as Submit and QueuedForOwner already refuse them.

// liveDraft and liveRoute are the two "the first turn's work is still live or
// done" clauses; retryableWhere and Requeue's refusal reasons both read THESE,
// so the list, the reasons and the guarded UPDATE cannot disagree.
const (
	liveDraft = `EXISTS (SELECT 1 FROM plugin_reply_drafts d WHERE d.message_id = m.id
		AND d.status IN ('pending', 'approved', 'sent', 'unconfirmed'))`
	liveRoute = `EXISTS (SELECT 1 FROM session_routes r WHERE r.plugin_message_id = m.id
		AND r.state IN ('routed', 'answered', 'relayed'))`
)

// revokedPlugin: the message's plugin has been revoked. Revoking a plugin
// stops everything it sent from reaching the chief (Submit refuses new
// messages, QueuedForOwner skips queued ones, Deliver refuses to hand one
// over), so a retry must not be the one door that still opens.
const revokedPlugin = `EXISTS (SELECT 1 FROM plugins p WHERE p.id = m.plugin_id AND p.revoked_at IS NOT NULL)`

// gatedTaskCited: the chief already filed a gated task citing this message
// (task_create_gated with its message_id). The link is the budget's
// ActionGatedTask audit row, which carries the message_id and the owner; the
// task itself records the citation only as a line in its body, and
// plugin_messages.task_id is a different link (the task a task EVENT is about).
//
// ⚠ THE ROW IS THE RESERVATION, WRITTEN BEFORE THE TASK IS CREATED, so a create
// that then failed (an infrastructure error) also refuses the retry. That is the
// direction that never files a second task; the owner can still see the board.
// ⚠ A gated task filed WITHOUT citing the message is invisible here.
const gatedTaskCited = `EXISTS (SELECT 1 FROM audit_log a WHERE a.action = '` + ActionGatedTask + `'
		AND a.user_id = m.owner_id AND a.detail->>'message_id' = m.id::text)`

// chiefActedTask: a task event (task_id set) the chief already acted on
// (chiefActedOn, chief_dispatch.go): a retry would forward or ask twice.
const chiefActedTask = `(m.task_id IS NOT NULL AND ` + chiefActedOn + `)`

// retryableWhere is that predicate over plugin_messages aliased m.
const retryableWhere = `m.status = 'unconfirmed' AND NOT ` + liveDraft + ` AND NOT ` + liveRoute + ` AND NOT ` + chiefActedTask +
	` AND NOT ` + gatedTaskCited + ` AND NOT ` + revokedPlugin

// ActionRetried is the audit_log action a requeue appends (object
// MessageObject). Retries COUNTS these rows, so the audit trail and what the
// chief is told about a retry cannot disagree.
const ActionRetried = "plugin.retried"

// Retries is how many times the principal retried message id (its
// ActionRetried rows). Deliver reads it to tell the chief (retrySentence).
func (s *PGStore) Retries(ctx context.Context, id int64) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = $1 AND object = $2`,
		ActionRetried, MessageObject(id)).Scan(&n)
	return n, err
}

// ErrNotRetryable refuses a retry; the wrapped text says why (409).
var ErrNotRetryable = errors.New("plugins: not retryable")

// Retryable is the principal's retryable messages (see retryableWhere), newest
// first, at most limit.
func (s *PGStore) Retryable(ctx context.Context, limit int) ([]Message, error) {
	if limit <= 0 {
		return []Message{}, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+prefixed("m", messageCols)+` FROM plugin_messages m
		WHERE `+retryableWhere+` ORDER BY m.id DESC LIMIT $1`, limit)
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

// Requeue moves a retryable message back to `queued` (agent and settle time
// cleared, as a fresh message has them) and appends a `plugin.retried` audit row,
// in one transaction. ErrNotFound when the principal has no such message;
// ErrNotRetryable, saying why, when it is not retryable. The attempt itself is
// counted by the delivery that follows (BeginDelivery / RecordAttempt).
func (s *PGStore) Requeue(ctx context.Context, id int64) (Message, error) {
	var out Message
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var st string
		var drafts, routes, acted, gated, revoked bool
		err := tx.QueryRow(ctx, `SELECT m.status, `+liveDraft+`, `+liveRoute+`, `+chiefActedTask+`, `+gatedTaskCited+`, `+revokedPlugin+`
			FROM plugin_messages m WHERE m.id = $1 FOR UPDATE OF m`, id).Scan(&st, &drafts, &routes, &acted, &gated, &revoked)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		switch {
		case revoked:
			return fmt.Errorf("%w: its plugin has been revoked", ErrNotRetryable)
		case Status(st) == StatusQueued:
			return fmt.Errorf("%w: it is already queued for your chief (a retry is under way)", ErrNotRetryable)
		case Status(st) == StatusDelivering:
			return fmt.Errorf("%w: it is being delivered to your chief right now", ErrNotRetryable)
		case Status(st) == StatusDelivered:
			return fmt.Errorf("%w: your chief already received it", ErrNotRetryable)
		case drafts:
			return fmt.Errorf("%w: your chief already drafted a reply to it that is pending, approved or sent", ErrNotRetryable)
		case routes:
			return fmt.Errorf("%w: your chief already routed it to a session", ErrNotRetryable)
		case acted:
			return fmt.Errorf("%w: your chief already acted on this task", ErrNotRetryable)
		case gated:
			return fmt.Errorf("%w: your chief already filed a gated task citing it", ErrNotRetryable)
		}
		m, err := scanMessage(tx.QueryRow(ctx, `UPDATE plugin_messages m
			SET status = 'queued', agent_id = NULL, delivered_at = NULL
			WHERE m.id = $1 AND `+retryableWhere+`
			RETURNING `+prefixed("m", messageCols), id))
		if errors.Is(err, pgx.ErrNoRows) {
			// The checks above passed under the row lock, so this is a state the
			// switch does not name; refuse rather than guess.
			return fmt.Errorf("%w: it is %s", ErrNotRetryable, st)
		}
		if err != nil {
			return err
		}
		out = m
		return audit(ctx, tx, ActionRetried, MessageObject(id), map[string]any{"plugin_id": m.PluginID, "from": st})
	})
	return out, err
}
