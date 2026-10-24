package db_test

import (
	"strings"
	"testing"
)

// TestMigration0050SettlesThePreExistingMessages: at version 48 (the control:
// settle_reason does not exist yet) a delivered repliable message, an
// unconfirmed one, a delivered no_reply one and a queued one exist. After 0050
// only the delivered repliable one is settled 'pre-existing' — so the first
// sweep after the deploy raises no ask for it — while the queued one,
// delivered after the deploy, is judged like any new message (the sweep never
// judges an unconfirmed one, nor a no_reply one, so neither is touched). Then the schema's one-ask-per-message rule: a second ask
// naming the same message is refused.
//
// It starts at 48, not 49, so it runs unchanged whether or not 0049 (another
// branch's) is on the tree: applyThroughFrom applies whatever lies between.
func TestMigration0050SettlesThePreExistingMessages(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0050_unrouted", 48)
	agent := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'unrouted-chief', 'ns-unrouted') RETURNING id`)
	plugin := scanID(ctx, t, pool, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope)
		VALUES (1, 'plugin-unrouted', 'Example inbox', 'pluginhash-unrouted', 'example') RETURNING id`)
	msg := func(ext, status string, noReply bool) int64 {
		delivered := status != "queued"
		var agentID any
		if delivered {
			agentID = agent
		}
		return scanID(ctx, t, pool, `INSERT INTO plugin_messages (owner_id, plugin_id, external_id, body, status, agent_id, no_reply, delivered_at)
			VALUES (1, $1, $2, 'hello', $3, $4, $5, CASE WHEN $6 THEN now() - interval '1 day' END) RETURNING id`,
			plugin, ext, status, agentID, noReply, delivered)
	}
	repliable := msg("ext-repliable", "delivered", false)
	unconfirmed := msg("ext-unconfirmed", "unconfirmed", false)
	noReply := msg("ext-noreply", "delivered", true)
	queued := msg("ext-queued", "queued", false)
	if _, err := pool.Exec(ctx, `UPDATE plugin_messages SET settle_reason = 'x' WHERE id = $1`, repliable); err == nil || !strings.Contains(err.Error(), "settle_reason") {
		t.Fatalf("control: at version 48 settle_reason already exists: %v", err)
	}

	applyThroughFrom(ctx, t, pool, 50)

	reason := func(id int64) string {
		t.Helper()
		var r *string
		if err := pool.QueryRow(ctx, `SELECT settle_reason FROM plugin_messages WHERE id = $1`, id).Scan(&r); err != nil {
			t.Fatal(err)
		}
		if r == nil {
			return ""
		}
		return *r
	}
	if got := reason(repliable); got != "pre-existing" {
		t.Errorf("a repliable message delivered before 0050 has settle_reason %q; want pre-existing", got)
	}
	if got := reason(unconfirmed); got != "" {
		t.Errorf("an unconfirmed message was settled %q; the sweep never judges one, so it is left alone", got)
	}
	if got := reason(noReply); got != "" {
		t.Errorf("a no_reply message was settled %q; it never raises an ask, so it is left alone", got)
	}
	if got := reason(queued); got != "" {
		t.Errorf("a queued message was settled %q; it is delivered after the deploy and judged then", got)
	}

	ask := `INSERT INTO attention_asks (owner_id, system_asker, question, expires_at, plugin_message_id)
		VALUES (1, true, 'q', now() + interval '1 hour', $1)`
	mustExec(ctx, t, pool, ask, queued)
	if _, err := pool.Exec(ctx, ask, queued); err == nil || !strings.Contains(err.Error(), "attention_asks_one_per_plugin_message") {
		t.Fatalf("a second ask for one message was not refused: %v", err)
	}

	// Idempotent: the body applies a second time over itself.
	mustExec(ctx, t, pool, readMigration(t, "0050_plugin_message_unrouted.sql"))
	if got := reason(queued); got != "" {
		t.Errorf("re-applying 0050 settled the queued message: %q", got)
	}
}
