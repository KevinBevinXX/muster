package focus

import (
	"strings"
	"testing"
)

// TestAHermesFocusIsStoredInBothTables: a focus keyed by a Hermes
// conversation ("hermes:" and a 70-character id) is written — the focus row
// AND its history row, which the store writes in ONE transaction, so a CHECK
// widened on one table alone would fail the whole write — and read back by
// that key. Spelled with MirrorSubject and the key's literal form, so it
// reads the same against a tree from before HermesSubject existed. The
// control is a Claude Code id, stored as it always was.
func TestAHermesFocusIsStoredInBothTables(t *testing.T) {
	s, pool := pgStore(t)
	hermes := MirrorSubject("hermes:local-" + strings.Repeat("0123456789abcdef", 4))
	claude := MirrorSubject("0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b")
	for _, sub := range []Subject{claude, hermes} {
		if _, err := s.Upsert(as(2), sub, Write{Title: "Reading the session store", Description: "Tailing it.", Source: SourceSummarizer}); err != nil {
			t.Fatalf("Upsert %s (%d-character key): %v", sub, len(sub.Key), err)
		}
		f, ok, err := s.Get(as(2), sub)
		if err != nil || !ok || f.Subject != sub || f.Title != "Reading the session store" {
			t.Fatalf("Get %s = %+v, %t, %v", sub, f, ok, err)
		}
		var n int
		if err := pool.QueryRow(as(2), `SELECT count(*) FROM agent_focus_history WHERE subject_kind = 'mirror' AND subject_key = $1`, sub.Key).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s's history rows: %d (%v), want 1", sub, n, err)
		}
	}
}
