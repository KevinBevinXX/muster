package db_test

import (
	"strings"
	"testing"
)

// TestMigration0048RecordsWhereADraftDecisionWasMade: at version 47 a reply
// draft has no decision source (the control: the column is absent); 0048 adds
// it with NO backfill — a draft decided before it reads NULL — and its CHECK
// allows a host and session only with 'mcp' and then both, in the mirror wire's
// shapes; 'browser' with neither; no source on a pending draft; nothing else.
func TestMigration0048RecordsWhereADraftDecisionWasMade(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0048_draft_decided_via", 47)
	const sid = "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b"
	agent := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'draft-agent', 'ns-draft') RETURNING id`)
	plugin := scanID(ctx, t, pool, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope)
		VALUES (1, 'plugin-0048', 'Example inbox', 'pluginhash-0048', 'example') RETURNING id`)
	n := 0
	draft := func(status string) int64 {
		t.Helper()
		n++ // one message per draft: at most one pending draft per message
		message := scanID(ctx, t, pool, `INSERT INTO plugin_messages (owner_id, plugin_id, external_id, body, status, agent_id)
			VALUES (1, $1, $2, 'hello', 'delivering', $3) RETURNING id`, plugin, "ext-0048-"+strings.Repeat("x", n), agent)
		return scanID(ctx, t, pool, `INSERT INTO plugin_reply_drafts (owner_id, message_id, agent_id, agent_name, body, status, expires_at)
			VALUES (1, $1, $2, 'a', 'reply', $3, now() + interval '1 day') RETURNING id`, message, agent, status)
	}
	old := draft("rejected")
	if _, err := pool.Exec(ctx, `UPDATE plugin_reply_drafts SET decided_via = 'mcp' WHERE id = $1`, old); err == nil || !strings.Contains(err.Error(), "decided_via") {
		t.Fatalf("control: at version 47 the decided_via column already exists: %v", err)
	}

	applyThroughFrom(ctx, t, pool, 48)

	var via *string
	if err := pool.QueryRow(ctx, `SELECT decided_via FROM plugin_reply_drafts WHERE id = $1`, old).Scan(&via); err != nil || via != nil {
		t.Fatalf("a decision from before 0048 has decided_via %v (%v); want NULL — no backfill", via, err)
	}
	set := `UPDATE plugin_reply_drafts SET decided_via = $2, decided_via_host = $3, decided_via_session = $4 WHERE id = $1`
	refused := func(what string, id int64, v, host, sess any) {
		t.Helper()
		if _, err := pool.Exec(ctx, set, id, v, host, sess); err == nil || !strings.Contains(err.Error(), "plugin_reply_drafts_decided_via_check") {
			t.Errorf("%s was not refused by the source CHECK: %v", what, err)
		}
	}
	refused("an MCP source on a pending draft", draft("pending"), "mcp", "ws", sid)
	refused("a browser source on a pending draft", draft("pending"), "browser", nil, nil)
	refused("an unknown via", draft("rejected"), "standing", nil, nil)
	refused("mcp without a host", draft("rejected"), "mcp", nil, sid)
	refused("mcp without a session", draft("rejected"), "mcp", "ws", nil)
	refused("browser with a host and session", draft("rejected"), "browser", "ws", sid)
	refused("a host and session with no via", draft("rejected"), nil, "ws", sid)
	refused("a host the wire would refuse", draft("rejected"), "mcp", "WS ws", sid)
	refused("a session the wire would refuse", draft("rejected"), "mcp", "ws", "../etc")
	for _, c := range []struct {
		via        string
		host, sess any
	}{{"mcp", "ws", sid}, {"browser", nil, nil}} {
		if _, err := pool.Exec(ctx, set, draft("rejected"), c.via, c.host, c.sess); err != nil {
			t.Errorf("after 0048, a %s decision on a rejected draft: %v", c.via, err)
		}
	}
}
