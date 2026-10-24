package db_test

import (
	"strings"
	"testing"
)

// TestMigration0036SpellsTheWireSessionKeyPattern: the four CHECKs 0036 writes
// carry EXACTLY the pattern it was written for — 0036 is history and never
// changes — and, while 0036 is the LATEST migration to write them
// (TestTheLatestSessionKeyChecksSpellTheWirePattern), that is
// mirror.SessionKeyPattern. An INVARIANT GUARD over two spellings, not a
// regression test.
func TestMigration0036SpellsTheWireSessionKeyPattern(t *testing.T) {
	body := readMigration(t, "0036_session_key_hermes.sql")
	const pattern0036 = `^([0-9A-Za-z][0-9A-Za-z_-]{0,63}|hermes:[0-9A-Za-z][0-9A-Za-z_-]{0,127})$`
	var code []string
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			code = append(code, line)
		}
	}
	quoted := "'" + pattern0036 + "'"
	if got := strings.Count(strings.Join(code, "\n"), quoted); got != 4 {
		t.Fatalf("0036 spells its pattern (%s) in %d CHECKs, want 4", pattern0036, got)
	}
	if got := strings.Count(strings.Join(code, "\n"), "~ '"); got != 4 {
		t.Fatalf("0036 has %d regular-expression matches, want exactly the 4 session-key CHECKs", got)
	}
}
