package db_test

import (
	"testing"
)

// TestMigration0022SeedsTheHistoryFromTheCurrentFocusOnce: a database at
// version 21 holding focus rows arrives at 0022 with each TITLED focus as its
// session's first history row (a row that only recorded a failure has no
// focus to seed), and re-applying the body — what db.RLSOn does — adds none.
func TestMigration0022SeedsTheHistoryFromTheCurrentFocusOnce(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0022_history", 21)
	agent := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'seed-agent', 'ns-seed') RETURNING id`)
	mustExec(ctx, t, pool, `INSERT INTO agent_focus (owner_id, subject_kind, subject_key, agent_id, title, description, updated_at)
		VALUES (1, 'agent', $1::bigint::text, $1, 'Seeded focus', 'From before 0022.', '2026-10-01T00:00:00Z')`, agent)
	mustExec(ctx, t, pool, `INSERT INTO agent_focus (owner_id, subject_kind, subject_key, last_error, error_at)
		VALUES (1, 'mirror', 'failed-only', 'no OpenRouter key', now())`)

	applyThroughFrom(ctx, t, pool, 22)

	var title string
	var atUpdated bool
	if err := pool.QueryRow(ctx, `SELECT title || '|' || description, created_at = '2026-10-01T00:00:00Z'::timestamptz
		FROM agent_focus_history`).Scan(&title, &atUpdated); err != nil {
		t.Fatalf("after 0022: %v (want exactly one seeded row)", err)
	}
	if title != "Seeded focus|From before 0022." || !atUpdated {
		t.Fatalf("seeded %q (at its updated_at: %v), want the focus at its updated_at", title, atUpdated)
	}
	mustExec(ctx, t, pool, readMigration(t, "0022_agent_focus_history.sql"))
	if n := countRows(ctx, t, pool, "agent_focus_history"); n != 1 {
		t.Fatalf("%d history rows after re-applying 0022, want still 1", n)
	}
}
