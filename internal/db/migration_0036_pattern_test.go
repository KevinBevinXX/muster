package db_test

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestMigration0036SpellsTheWireSessionKeyPattern: the four CHECKs 0036 writes
// carry EXACTLY mirror.SessionKeyPattern — the one expression internal/focus
// validates with — so the code and the table cannot drift apart. An INVARIANT
// GUARD over two spellings, not a regression test.
func TestMigration0036SpellsTheWireSessionKeyPattern(t *testing.T) {
	body := readMigration(t, "0036_session_key_hermes.sql")
	var code []string
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			code = append(code, line)
		}
	}
	quoted := "'" + mirror.SessionKeyPattern + "'"
	if got := strings.Count(strings.Join(code, "\n"), quoted); got != 4 {
		t.Fatalf("0036 spells mirror.SessionKeyPattern (%s) in %d CHECKs, want 4", mirror.SessionKeyPattern, got)
	}
	if got := strings.Count(strings.Join(code, "\n"), "~ '"); got != 4 {
		t.Fatalf("0036 has %d regular-expression matches, want exactly the 4 session-key CHECKs", got)
	}
}
