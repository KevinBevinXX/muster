package suggest

import (
	"strings"
	"testing"
)

// TestThePreGatePassesOnlyARepliesThatHandBack: the cheap check before any
// call. Each "yes" case passes by a different rule; each "no" case is a turn
// that reports and stops.
func TestThePreGatePassesOnlyRepliesThatHandBack(t *testing.T) {
	yes := map[string]string{
		"question":    "Fixed. Which branch should this go on?",
		"want me to":  "Fixed and pushed. Want me to open the PR.",
		"should i":    "All green. Should I merge",
		"your call":   "Both work; your call.",
		"options":     "Two ways to do it:\n(a) checkpoint in the jobs table\n(b) re-run keyed on batch id\nEither is fine.",
		"numbered":    "Options:\n1. keep the cache\n2. drop it\nI prefer 2.",
		"inline ab":   "We can (a) keep it or (b) drop it.",
		"let me know": "The report is in claudedocs. Let me know.",
	}
	for name, r := range yes {
		if !HandsBack(r) {
			t.Errorf("%s: %q should hand back", name, r)
		}
	}
	no := map[string]string{
		"report":         "Done. All 42 tests pass and the PR is merged.",
		"list no choice": "Changed:\n1. parser.go\n2. lexer.go\nAll green.",
		"old question":   "Why did it fail? " + strings.Repeat("Then I fixed it and everything passes now. ", 30),
	}
	for name, r := range no {
		if HandsBack(r) {
			t.Errorf("%s: %q should not hand back", name, r)
		}
	}
}
