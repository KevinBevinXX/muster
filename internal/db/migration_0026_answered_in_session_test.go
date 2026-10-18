package db_test

import (
	"strings"
	"testing"
)

// TestMigration0026AddsAnsweredInSession: at version 25 an ask cannot be moved
// to 'answered_in_session' (the control: the old status CHECK is real); after
// 0026 it can — with an answer and answered_by, never without — and a status
// outside the set is still refused.
func TestMigration0026AddsAnsweredInSession(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0026_in_session", 25)
	agent := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'ask-agent', 'ns-ask') RETURNING id`)
	ask := func() int64 {
		return scanID(ctx, t, pool, `INSERT INTO attention_asks (owner_id, agent_id, question, expires_at) VALUES (1, $1, 'which way?', now() + interval '1 hour') RETURNING id`, agent)
	}
	inSession := func(id int64, answer any) error {
		_, err := pool.Exec(ctx, `UPDATE attention_asks SET status = 'answered_in_session', answer = $2, answered_by = 'operator', resolved_at = now() WHERE id = $1`, id, answer)
		return err
	}

	// (Postgres may report either of 0025's two CHECKs the row breaks first.)
	if err := inSession(ask(), "go left"); err == nil ||
		!(strings.Contains(err.Error(), "attention_asks_status_check") || strings.Contains(err.Error(), "attention_asks_answered_check")) {
		t.Fatalf("control: at version 25 'answered_in_session' was not refused by 0025's CHECKs: %v", err)
	}

	applyThroughFrom(ctx, t, pool, 26)

	if err := inSession(ask(), "go left"); err != nil {
		t.Errorf("after 0026, an ask answered in session: %v", err)
	}
	if err := inSession(ask(), nil); err == nil || !strings.Contains(err.Error(), "attention_asks_answered_check") {
		t.Errorf("after 0026, 'answered_in_session' without an answer was not refused by the answered CHECK: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE attention_asks SET status = 'answered_elsewhere', resolved_at = now() WHERE id = $1`, ask()); err == nil ||
		!strings.Contains(err.Error(), "attention_asks_status_check") {
		t.Errorf("after 0026, an unknown status was not refused: %v", err)
	}
	// The original exits still hold.
	if _, err := pool.Exec(ctx, `UPDATE attention_asks SET status = 'answered', answer = 'x', answered_by = 'operator', resolved_at = now() WHERE id = $1`, ask()); err != nil {
		t.Errorf("after 0026, a plain answer: %v", err)
	}
}
