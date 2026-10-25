package opencodehost

import (
	"strings"
	"testing"
)

func roots(titles ...string) []Session {
	var out []Session
	for i, t := range titles {
		out = append(out, Session{ID: "ses_fixture" + string(rune('a'+i)), Title: t})
	}
	return out
}

// TestMatchTitleNamesExactlyOneRootSession: the pane title is "OC | " and the
// session's title — whole, or cut with "…" (a prefix match) — and it names a
// session only when exactly ONE root session matches. Two sessions with the
// title (a default title) is no match: ambiguous, so none is shown. A
// subagent's child session is never matched, and a title without the prefix
// (the TUI's home screen) names nothing.
func TestMatchTitleNamesExactlyOneRootSession(t *testing.T) {
	long := "Refactor the fixture loader so it reads every synthetic file"
	ss := append(roots("Fix the flaky fixture", long, "New session", "New session", "Refactor the fixture loader too"),
		Session{ID: "ses_fixturechild", ParentID: "ses_fixturea", Title: "Explore the fixtures"})
	cases := []struct {
		pane, want, why string
		truncated       bool
	}{
		{"OC | Fix the flaky fixture", "ses_fixturea", "", false},
		{"OC | " + long[:36] + "…", "ses_fixtureb", "", true},
		// A cut title shared by two sessions' prefixes.
		{"OC | Refactor the fixture loader…", "", "2 opencode sessions match", true},
		{"OC | New session", "", "2 opencode sessions match", false},
		{"OC | Explore the fixtures", "", "no opencode session is titled", false},
		{"OC | Fix the flaky", "", "no opencode session is titled", false}, // not cut: exact only
		{"OC | ", "", "names no session", false},
		{"OC | …", "", "names no session", true},
		{"opencode", "", "no \"OC | \" prefix", false},
		{"Fix the flaky fixture", "", "no \"OC | \" prefix", false},
	}
	for _, c := range cases {
		m := MatchTitle(c.pane, ss)
		if m.Session != c.want || m.Truncated != c.truncated || (c.why == "") != (m.Why == "") || !strings.Contains(m.Why, c.why) {
			t.Errorf("MatchTitle(%q) = %+v, want session %q truncated %v why %q", c.pane, m, c.want, c.truncated, c.why)
		}
	}
	if m := MatchTitle("OC | Fix the flaky fixture", nil); m.Session != "" || !strings.Contains(m.Why, "no opencode session") {
		t.Errorf("against no sessions: %+v", m)
	}
}
