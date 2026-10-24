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

// A MESSAGE THE CHIEF DID NOT DISPOSITION (migration 0051): the rules, and why,
// are in docs/plugins.md#a-message-the-chief-does-not-act-on. The SQL below is
// every reader's: the sweep that raises an ask, the sweep that closes one, and
// (awaitsDisposition) migration 0051's backfill.

// UnroutedAskAfter is how long after its delivery a message the chief has not
// dispositioned waits before it becomes an ask.
const UnroutedAskAfter = 15 * time.Minute

// MaxSettleReasonRunes bounds a settle reason.
const MaxSettleReasonRunes = 2000

// UnroutedAskPlugin is the one plugin, by name (as muster names a plugin
// everywhere: the route allowlist's "<user>:<plugin>:…"), whose messages the
// sweep judges: agent mail. Every other plugin's — Discord DMs, which the owner
// already reads where they arrive — never raises an unrouted ask (operator
// decision).
const UnroutedAskPlugin = "agent-mail"

// awaitsDisposition is whether message m (the alias) is one the chief must act
// on: repliable and delivered to the chief. Migration 0051's backfill spells
// exactly this (TestTheBackfillIsTheSweepsPredicate), whatever the plugin, so
// widening the plugin scope later fires nothing from before 0051 either.
const awaitsDisposition = `m.status = 'delivered' AND NOT m.no_reply`

// fromUnroutedAskPlugin: message m came through UnroutedAskPlugin.
const fromUnroutedAskPlugin = `EXISTS (SELECT 1 FROM plugins p WHERE p.id = m.plugin_id AND p.name = '` + UnroutedAskPlugin + `')`

// dispositioned: settled, or the chief acted on it (chiefActed, retry.go).
const dispositioned = `(m.settle_reason IS NOT NULL OR ` + chiefActed + `)`

// unroutedDue is whether message m is due an ask as of the cutoff $1: it came
// through UnroutedAskPlugin, awaits a disposition, was delivered at or before
// $1, has none, and no ask was raised for it yet (whatever that ask's state now).
const unroutedDue = fromUnroutedAskPlugin + ` AND ` + awaitsDisposition + ` AND m.delivered_at <= $1 AND NOT ` + dispositioned + `
	AND NOT EXISTS (SELECT 1 FROM attention_asks a WHERE a.plugin_message_id = m.id)`

// UnroutedAsk is an open ask raised for a message that has since been
// dispositioned: the ask to close, and whose it is.
type UnroutedAsk struct {
	AskID, OwnerID, MessageID int64
}

// cleanSettleReason trims a settle reason and bounds it.
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
// is then raised as its owner.
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

// UnroutedAsksToClose is every owner's OPEN ask raised for a message that has
// since been dispositioned, oldest first, at most
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
