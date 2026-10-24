package plugins

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/authz"
)

// A MESSAGE THE CHIEF DID NOT DISPOSITION (migration 0050): a repliable inbound
// message (no_reply false) delivered to the chief must not silently go nowhere.
// The chief DISPOSITIONS it by routing it to a session (a session_routes row),
// drafting a reply (a plugin_reply_drafts row), or settling it
// (plugin_message_settle: settle_reason, its own judgement that the message
// needs nothing more, with why). One still not dispositioned UnroutedAskAfter
// after its delivery becomes an ask on the owner's Attention view, raised by
// muster's attention sweep (internal/api unrouted.go), once; a disposition
// after that closes the ask, and the owner's answer to it settles the message.
//
// 🔴 ONE RULE, ONE PLACE: dispositioned is the SQL every reader here uses — the
// sweep that raises an ask, the re-check under the message's lock, and the
// sweep that closes one — so "the chief acted on it" cannot mean two things.

// UnroutedAskAfter is how long after its delivery a message the chief has not
// dispositioned waits before it becomes an ask.
const UnroutedAskAfter = 15 * time.Minute

// MaxSettleReasonRunes bounds a settle reason (0050's CHECK).
const MaxSettleReasonRunes = 2000

// dispositioned is whether message m (the alias) has been acted on: routed to
// a session, answered with a reply draft, or settled — whoever did it, through
// whichever door.
const dispositioned = `(m.settle_reason IS NOT NULL
	OR EXISTS (SELECT 1 FROM session_routes r WHERE r.plugin_message_id = m.id)
	OR EXISTS (SELECT 1 FROM plugin_reply_drafts d WHERE d.message_id = m.id))`

// unroutedDue is whether message m is due an ask as of the cutoff $1: delivered
// to the chief at or before it, repliable, not dispositioned, and with no ask
// raised for it yet (whatever that ask's state now). settle_reason IS NULL is
// also inside dispositioned; it is spelled here too so the planner can use
// idx_plugin_messages_unsettled, whose predicate it is.
const unroutedDue = `m.status = 'delivered' AND NOT m.no_reply AND m.settle_reason IS NULL AND m.delivered_at <= $1
	AND NOT ` + dispositioned + `
	AND NOT EXISTS (SELECT 1 FROM attention_asks a WHERE a.plugin_message_id = m.id)`

// UnroutedAsk is an open ask raised for a message that has since been
// dispositioned: the ask to close, and whose it is.
type UnroutedAsk struct {
	AskID, OwnerID, MessageID int64
}

// cleanSettleReason trims a settle reason and checks it against 0050's CHECK.
func cleanSettleReason(reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	switch {
	case reason == "":
		return "", invalid("a reason is required: say why the message needs nothing more")
	case utf8.RuneCountInString(reason) > MaxSettleReasonRunes:
		return "", invalid("the reason is longer than %d characters", MaxSettleReasonRunes)
	case strings.ContainsRune(reason, 0):
		return "", invalid("the reason must be text without NUL")
	}
	return reason, nil
}

// SettleMessage records agent agentID's judgement that message messageID — one
// delivered to that agent of owner ownerID (DeliveredTo) — needs nothing more,
// with reason, and audits it (plugin.message.settled). fresh is false, and the
// FIRST reason is kept, when the message was already settled. Another owner's
// message (row-level security), a missing one and one not delivered to this
// agent are ErrNotFound alike.
func (s *PGStore) SettleMessage(ctx context.Context, agentID, ownerID, messageID int64, reason string) (Message, bool, error) {
	reason, err := cleanSettleReason(reason)
	if err != nil {
		return Message{}, false, err
	}
	var m Message
	fresh := false
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		// The message row lock, as every disposition takes it (lockMessage), so
		// a settle and the sweep's raise (RaiseUnrouted) are serialised.
		cur, err := scanMessage(tx.QueryRow(ctx, `SELECT `+messageCols+` FROM plugin_messages WHERE id = $1 FOR UPDATE`, messageID))
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !DeliveredTo(cur, agentID, ownerID)) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		m = cur
		if cur.SettleReason != nil {
			return nil
		}
		if m, err = scanMessage(tx.QueryRow(ctx, `UPDATE plugin_messages SET settle_reason = $2 WHERE id = $1 RETURNING `+messageCols,
			messageID, reason)); err != nil {
			return err
		}
		fresh = true
		return audit(ctx, tx, "plugin.message.settled", MessageObject(messageID), map[string]any{"agent_id": agentID, "by": "chief"})
	})
	if err != nil {
		return Message{}, false, err
	}
	return m, fresh, nil
}

// UnroutedDue is every owner's messages due an ask as of asOf (unroutedDue:
// delivered at or before asOf - UnroutedAskAfter), longest-waiting first, at
// most limit. Under the system scope (internal/authz's ledger names it): each
// is then raised as its owner (RaiseUnrouted).
func (s *PGStore) UnroutedDue(ctx context.Context, asOf time.Time, limit int) ([]Message, error) {
	rows, err := s.pool.Query(authz.System(ctx), `SELECT `+prefixed("m", messageCols)+` FROM plugin_messages m
		WHERE `+unroutedDue+` ORDER BY m.delivered_at, m.id LIMIT $2`, asOf.Add(-UnroutedAskAfter), limit)
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

// RaiseUnrouted calls raise for message id when — re-checked now, under the
// message's row lock — it is still due an ask as of asOf, and reports whether
// it did. raise's error is returned.
//
// 🔴 THE LOCK IS HELD WHILE raise RUNS, so a disposition racing the raise
// (route, draft, settle: each takes the message row FOR UPDATE first) lands
// either before it — and the re-check sees it, so no ask is raised — or after
// it, when the ask exists and the closing sweep (UnroutedAsksToClose) ends it.
// It is FOR NO KEY UPDATE, not FOR UPDATE: raise inserts an ask whose foreign
// key to this row takes FOR KEY SHARE on another connection, which FOR UPDATE
// would block for ever.
func (s *PGStore) RaiseUnrouted(ctx context.Context, id int64, asOf time.Time, raise func(Message) error) (bool, error) {
	raised := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		m, err := scanMessage(tx.QueryRow(ctx, `SELECT `+prefixed("m", messageCols)+` FROM plugin_messages m
			WHERE m.id = $2 AND `+unroutedDue+` FOR NO KEY UPDATE OF m`, asOf.Add(-UnroutedAskAfter), id))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := raise(m); err != nil {
			return err
		}
		raised = true
		return nil
	})
	return raised, err
}

// UnroutedAsksToClose is every owner's OPEN ask raised for a message that has
// since been dispositioned (routed, drafted or settled), oldest first, at most
// limit. Under the system scope (internal/authz's ledger names it): each is
// then cancelled as its owner.
func (s *PGStore) UnroutedAsksToClose(ctx context.Context, limit int) ([]UnroutedAsk, error) {
	rows, err := s.pool.Query(authz.System(ctx), `SELECT a.id, a.owner_id, m.id FROM attention_asks a
		JOIN plugin_messages m ON m.id = a.plugin_message_id
		WHERE a.status = 'open' AND a.expires_at > now() AND `+dispositioned+`
		ORDER BY a.id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (UnroutedAsk, error) {
		var u UnroutedAsk
		err := row.Scan(&u.AskID, &u.OwnerID, &u.MessageID)
		return u, err
	})
}

// SettleByAsk settles the message ask askID was raised for, with reason (cut
// to MaxSettleReasonRunes): the owner's answer to that ask. It reports whether
// it settled one — false for an ask raised for no message, or a message
// already settled (the first reason is kept). Row-level security keeps it to
// the caller's own asks and messages. Audited as plugin.message.settled.
func (s *PGStore) SettleByAsk(ctx context.Context, askID int64, reason string) (bool, error) {
	reason = cutRunes(strings.TrimSpace(reason), MaxSettleReasonRunes)
	settled := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var id int64
		err := tx.QueryRow(ctx, `UPDATE plugin_messages m SET settle_reason = $2 FROM attention_asks a
			WHERE a.id = $1 AND m.id = a.plugin_message_id AND m.settle_reason IS NULL RETURNING m.id`, askID, reason).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		settled = true
		return audit(ctx, tx, "plugin.message.settled", MessageObject(id), map[string]any{"ask_id": askID, "by": "owner"})
	})
	return settled, err
}
