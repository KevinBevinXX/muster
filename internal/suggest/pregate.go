package suggest

import (
	"regexp"
	"strings"
)

// THE PRE-GATE: a final reply that does not hand control back gets NO model
// call. It is cheap and deterministic, and deliberately generous — a false
// pass costs one call whose suggestions the filter may still drop; a false
// refusal costs only chips on a turn that did not ask for any.
//
// A reply hands control back when its END (the last HandsBackWindow bytes,
// where a question or an option list sits) does any of:
//   - asks a question (a "?");
//   - offers options: two or more lines that start like "(a)", "a)", "1.",
//     "1)", "Option A", or a reply that names "(a)" and "(b)" inline, beside
//     a choice word ("which", "prefer", "option", "either", "or");
//   - says "want me to", "should I", "shall I", "your call", "let me know",
//     "would you like", "do you want", "ready to", "ok to" / "okay to".
//
// ⚠ A HEURISTIC over prose, flagged as such: it decides only whether a call is
// worth making. Nothing a suggestion says is gated by it.

// HandsBackWindow is how much of the reply's end the pre-gate reads: the
// proposal's measurement ("a ? in the last 400 chars") used the same window.
const HandsBackWindow = 400

var (
	reOptionLine    = regexp.MustCompile(`(?im)^\s*(?:[-*]\s*)?(?:\(?[a-e]\)|\(?[1-9][.)]|\*{0,2}option\s+[a-e1-9]\b)`)
	reInlineAB      = regexp.MustCompile(`(?i)\(a\).*\(b\)`)
	reChoiceWord    = regexp.MustCompile(`(?i)\b(which|prefer|option|options|either|or)\b`)
	handBackPhrases = []string{
		"want me to", "should i ", "should i?", "shall i ", "your call", "let me know",
		"would you like", "do you want", "ready to ", "ok to ", "okay to ",
	}
)

// HandsBack reports whether reply hands control back to the operator.
func HandsBack(reply string) bool {
	end := reply
	if len(end) > HandsBackWindow {
		end = end[len(end)-HandsBackWindow:]
	}
	if strings.Contains(end, "?") {
		return true
	}
	lower := strings.ToLower(end)
	for _, p := range handBackPhrases {
		if strings.Contains(lower, p) {
			return true
		}
	}
	if !reChoiceWord.MatchString(end) {
		return false
	}
	return len(reOptionLine.FindAllStringIndex(end, -1)) >= 2 || reInlineAB.MatchString(end)
}
