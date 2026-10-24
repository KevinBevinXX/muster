package db_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestTheLatestSessionKeyChecksSpellTheWirePattern: the LATEST migration that
// writes the four session-key CHECKs (agent_focus, agent_focus_history,
// session_routes, attention_asks.mirror_session) carries EXACTLY
// mirror.SessionKeyPattern, which is built from the runtime table
// (mirror.RuntimeSpec.KeyPrefix). So a runtime that starts keying its
// sessions changes the pattern and fails HERE until a new migration writes
// the wider CHECKs. An INVARIANT GUARD over two spellings.
func TestTheLatestSessionKeyChecksSpellTheWirePattern(t *testing.T) {
	names, err := filepath.Glob(filepath.Join("migrations", "*.sql"))
	if err != nil || len(names) < 40 {
		t.Fatalf("instrument check: %d migrations (%v)", len(names), err)
	}
	sort.Strings(names)
	latest, body := "", ""
	for _, n := range names {
		b, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "ADD CONSTRAINT attention_asks_mirror_session_check") {
			latest, body = filepath.Base(n), string(b)
		}
	}
	if latest == "" {
		t.Fatal("instrument check: no migration writes attention_asks_mirror_session_check")
	}
	var code []string
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			code = append(code, line)
		}
	}
	quoted := "'" + mirror.SessionKeyPattern + "'"
	if got := strings.Count(strings.Join(code, "\n"), quoted); got != 4 {
		t.Fatalf("%s, the latest migration to write the session-key CHECKs, spells mirror.SessionKeyPattern (%s) in %d CHECKs, want 4: a runtime's KeyPrefix changed the pattern without a migration",
			latest, mirror.SessionKeyPattern, got)
	}
}
