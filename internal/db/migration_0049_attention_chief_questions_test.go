package db_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigration0049AddsTheChiefQuestionColumns: at version 48 an ask
// has no to_chief (the control: the column is absent); 0049 adds it, false for
// every existing ask, and the escalates link; its CHECK accepts answered_by
// 'chief' and 'operator', and nothing else.
func TestMigration0049AddsTheChiefQuestionColumns(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0049_chief_questions", 48)
	const sid = "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b"
	ask := func() int64 {
		return scanID(ctx, t, pool, `INSERT INTO attention_asks (owner_id, mirror_host, mirror_session, question, expires_at)
			VALUES (1, 'ws', $1, 'which way?', now() + interval '1 hour') RETURNING id`, sid)
	}
	old := ask()
	if _, err := pool.Exec(ctx, `UPDATE attention_asks SET to_chief = true WHERE id = $1`, old); err == nil || !strings.Contains(err.Error(), "to_chief") {
		t.Fatalf("control: at version 48 the to_chief column already exists: %v", err)
	}

	applyThroughFrom(ctx, t, pool, 49)

	var toChief bool
	if err := pool.QueryRow(ctx, `SELECT to_chief FROM attention_asks WHERE id = $1`, old).Scan(&toChief); err != nil || toChief {
		t.Fatalf("an ask from before 0049 has to_chief %v (%v); want false", toChief, err)
	}
	answer := `UPDATE attention_asks SET status = 'answered', answer = 'left', answered_by = $2, resolved_at = now() WHERE id = $1`
	if _, err := pool.Exec(ctx, answer, ask(), "someone"); err == nil || !strings.Contains(err.Error(), "attention_asks_answered_by_check") {
		t.Errorf("an unknown answerer was not refused: %v", err)
	}
	q := ask()
	mustExec(ctx, t, pool, `UPDATE attention_asks SET to_chief = true WHERE id = $1`, q)
	if _, err := pool.Exec(ctx, answer, q, "chief"); err != nil {
		t.Errorf("the chief's answer to a question put to it: %v", err)
	}
	q2 := ask()
	mustExec(ctx, t, pool, `UPDATE attention_asks SET to_chief = true WHERE id = $1`, q2)
	if _, err := pool.Exec(ctx, answer, q2, "operator"); err != nil {
		t.Errorf("the operator's answer to a question put to the chief: %v", err)
	}
	// Idempotent: the body applies a second time over itself.
	body, err := os.ReadFile(filepath.Join("migrations", "0049_attention_chief_questions.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Errorf("0049's body does not re-apply cleanly: %v", err)
	}
}
