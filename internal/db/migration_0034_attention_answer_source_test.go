package db_test

import (
	"strings"
	"testing"
)

// TestMigration0034RecordsWhereAnAnswerCameFrom: at version 33 an ask has no
// answer provenance (the control: the column is absent); 0034 adds it with NO
// backfill — an answer recorded before it reads NULL, i.e. the Attention view —
// and its CHECK allows provenance only all-or-nothing, only 'mcp', only on an
// answered ask, only with the mirror wire's host and session shapes, and never
// naming the very session that raised the ask.
func TestMigration0034RecordsWhereAnAnswerCameFrom(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0034_answer_source", 33)
	const sid, other = "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b", "1c3d4e5f-6071-4829-9bac-1d2e3f4a5b6c"
	ask := func() int64 {
		return scanID(ctx, t, pool, `INSERT INTO attention_asks (owner_id, mirror_host, mirror_session, question, expires_at)
			VALUES (1, 'ws', $1, 'which way?', now() + interval '1 hour') RETURNING id`, sid)
	}
	answered := func() int64 {
		id := ask()
		mustExec(ctx, t, pool, `UPDATE attention_asks SET status = 'answered', answer = 'left', answered_by = 'operator', resolved_at = now() WHERE id = $1`, id)
		return id
	}
	old := answered()
	if _, err := pool.Exec(ctx, `UPDATE attention_asks SET answered_via = 'mcp' WHERE id = $1`, old); err == nil || !strings.Contains(err.Error(), "answered_via") {
		t.Fatalf("control: at version 33 the answered_via column already exists: %v", err)
	}

	applyThroughFrom(ctx, t, pool, 34)

	var via *string
	if err := pool.QueryRow(ctx, `SELECT answered_via FROM attention_asks WHERE id = $1`, old).Scan(&via); err != nil || via != nil {
		t.Fatalf("an answer from before 0034 has answered_via %v (%v); want NULL — no backfill", via, err)
	}
	set := `UPDATE attention_asks SET answered_via = $2, answered_via_host = $3, answered_via_session = $4 WHERE id = $1`
	refused := func(what string, id int64, v, host, sess any) {
		t.Helper()
		if _, err := pool.Exec(ctx, set, id, v, host, sess); err == nil || !strings.Contains(err.Error(), "attention_asks_answered_via_check") {
			t.Errorf("%s was not refused by the provenance CHECK: %v", what, err)
		}
	}
	refused("provenance on an open ask", ask(), "mcp", "ws", other)
	refused("an unknown via", answered(), "browser", "ws", other)
	refused("via without a host", answered(), "mcp", nil, other)
	refused("via without a session", answered(), "mcp", "ws", nil)
	refused("a host and session with no via", answered(), nil, "ws", other)
	refused("a host the wire would refuse", answered(), "mcp", "WS ws", other)
	refused("a session the wire would refuse", answered(), "mcp", "ws", "../etc")
	refused("the session that raised the ask", answered(), "mcp", "ws", sid)
	// The same session id on ANOTHER host is another session: allowed.
	for name, hs := range map[string][2]string{"another session of the host": {"ws", other}, "the same id on another host": {"laptop", sid}} {
		if _, err := pool.Exec(ctx, set, answered(), "mcp", hs[0], hs[1]); err != nil {
			t.Errorf("after 0034, %s: %v", name, err)
		}
	}
}
