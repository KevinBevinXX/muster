package db_test

import (
	"strings"
	"testing"
)

// TestMigration0026WidensTheFocusTitleCapTo300: at version 22 a 120-character
// focus title is refused by both tables' CHECKs (the control: the old 80 cap
// is real); after 0026 it is stored in both, a 300-character one is too, and a
// 301-character one is still refused — the cap is a storage guard, not gone.
func TestMigration0026WidensTheFocusTitleCapTo300(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0026_title_cap", 22)
	agent := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'cap-agent', 'ns-cap') RETURNING id`)
	insFocus := func(title string) error {
		_, err := pool.Exec(ctx, `INSERT INTO agent_focus (owner_id, subject_kind, subject_key, agent_id, title)
			VALUES (1, 'agent', $1::bigint::text, $1, $2)
			ON CONFLICT (owner_id, subject_kind, subject_key) DO UPDATE SET title = EXCLUDED.title`, agent, title)
		return err
	}
	insHistory := func(title string) error {
		_, err := pool.Exec(ctx, `INSERT INTO agent_focus_history (owner_id, subject_kind, subject_key, agent_id, title)
			VALUES (1, 'agent', $1::bigint::text, $1, $2)`, agent, title)
		return err
	}
	t120, t300, t301 := strings.Repeat("a", 120), strings.Repeat("b", 300), strings.Repeat("c", 301)

	if err := insFocus(t120); err == nil || !strings.Contains(err.Error(), "agent_focus_title_cap") {
		t.Fatalf("control: at version 22 a 120-char focus title was not refused by the 80 cap: %v", err)
	}
	if err := insHistory(t120); err == nil || !strings.Contains(err.Error(), "agent_focus_history_title_check") {
		t.Fatalf("control: at version 22 a 120-char history title was not refused by the 80 cap: %v", err)
	}

	applyThroughFrom(ctx, t, pool, 26)

	for _, title := range []string{t120, t300} {
		if err := insFocus(title); err != nil {
			t.Errorf("after 0026, a %d-char focus title: %v", len(title), err)
		}
		if err := insHistory(title); err != nil {
			t.Errorf("after 0026, a %d-char history title: %v", len(title), err)
		}
	}
	if err := insFocus(t301); err == nil || !strings.Contains(err.Error(), "agent_focus_title_cap") {
		t.Errorf("after 0026, a 301-char focus title was not refused by its cap: %v", err)
	}
	if err := insHistory(t301); err == nil || !strings.Contains(err.Error(), "agent_focus_history_title_check") {
		t.Errorf("after 0026, a 301-char history title was not refused by its cap: %v", err)
	}
	if err := insHistory(""); err == nil {
		t.Error("after 0026, an empty history title was stored (its lower bound is gone)")
	}
}
