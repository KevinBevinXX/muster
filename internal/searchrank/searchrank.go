// Package searchrank is how well a label matches what was typed into a picker,
// as a TIER: the order the operator asked every search picker to list its
// matches in.
//
//	0  the label STARTS with the query            "gpt" → "gpt-4o"
//	1  a WORD in the label starts with it          "4o"  → "gpt-4o"
//	2  the query is anywhere else in the label     "pt"  → "gpt-4o"
//	3  its letters appear in the label, in order   "g4o" → "gpt-4o"
//	-1 no match
//
// Matching is case-insensitive; a word starts at the label's start or after any
// character that is not a letter or a digit; a query of only spaces is empty, and
// an empty query matches everything at tier 0.
//
// 🔴 TIER 3 IS KEPT ON PURPOSE. The pickers matched letters-in-order before
// they were ranked, and an operator who types "g4o" or "mstr" expects the entry
// they always found that way. Dropped, those entries would simply vanish; kept
// LAST, they cannot push a better match down.
//
// 🔴 THE BROWSER HAS THE SAME RULE (appScript's window.musterRank, which the
// dispatch dialog's task, repo and model pickers and the directory picker sort
// by), and testdata/cases.json is the ONE table both are held to: this
// package's test reads it, and e2e/tests/pickers.spec.ts runs the browser's
// copy over the same file. The server uses this copy to decide WHICH matches
// survive a capped answer (the model search), so a prefix match is never cut
// by a page of substrings.
package searchrank

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Tier is how well label matches query; see the package doc.
func Tier(label, query string) int {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return 0
	}
	l := strings.ToLower(label)
	if strings.HasPrefix(l, q) {
		return 0
	}
	found := false
	for from := 0; from+len(q) <= len(l); {
		i := strings.Index(l[from:], q)
		if i < 0 {
			break
		}
		at := from + i
		found = true
		if at > 0 {
			prev, _ := utf8.DecodeLastRuneInString(l[:at])
			if !unicode.IsLetter(prev) && !unicode.IsDigit(prev) {
				return 1
			}
		}
		// On by one rune, so an overlapping later occurrence is seen too.
		_, size := utf8.DecodeRuneInString(l[at:])
		from = at + size
	}
	if found {
		return 2
	}
	rest := []rune(q)
	for _, r := range l {
		if len(rest) > 0 && r == rest[0] {
			rest = rest[1:]
		}
	}
	if len(rest) == 0 {
		return 3
	}
	return -1
}
