// Package searchrank is how well a label matches what was typed into a search,
// as a TIER: the order the operator asked searches to list their matches in.
//
//	0  the label STARTS with the query            "gpt" → "gpt-4o"
//	1  a WORD in the label starts with it          "4o"  → "gpt-4o"
//	2  the query is anywhere else in the label     "pt"  → "gpt-4o"
//	3  its letters appear in the label, in order   "g4o" → "gpt-4o"
//	-1 no match
//
// Both sides are compared after Fold, and a word starts at the label's start or
// after any character that is not a letter or a number. An empty query (after
// Fold) matches everything at tier 0.
//
// 🔴 TIER 3 IS KEPT ON PURPOSE. The pickers matched letters-in-order before
// they were ranked, and an operator who types "g4o" or "mstr" expects the entry
// they always found that way. Dropped, those entries would simply vanish; kept
// LAST, they cannot push a better match down.
//
// 🔴 THE BROWSER HAS THE SAME RULE (appScript's window.musterRank), and
// testdata/cases.json is the ONE table both are held to: this package's test
// reads it, and e2e/tests/pickers.spec.ts runs the browser's copy over the same
// file. WHICH SURFACES ARE HELD TO IT:
//
//   - the dispatch dialog's task, repo and model pickers and the directory
//     picker (window.musterRank), and the model search's server-side cut
//     (this package) — all of them, through the table;
//   - the Agents list filter — window.musterRank over each card's titles;
//   - NOT the agent thread search, which ranks in SQL (agents.PGStore
//     .SearchSessions: ILIKE and a regexp, case-insensitive by Postgres's own
//     rules, with no Fold). It agrees with the table on ASCII; outside it the
//     differences Fold exists for (accents, final sigma, compatibility forms)
//     are not applied there.
package searchrank

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Fold is the ONE normalisation a label and a query go through before they are
// compared, spelled identically in the browser (window.musterFold):
//
//  1. NFKD — compatibility forms become their plain letters and digits ("²" →
//     "2", "𝐀" → "A") and accented letters split from their accents;
//  2. combining marks and U+FEFF are dropped (so "é" matches "e", and a BOM
//     pasted with a query is not a character);
//  3. lower case, letter by letter — then the final sigma "ς" is "σ" (the
//     browser lower-cases a word-final Σ to ς, Go never does);
//  4. surrounding white space is trimmed.
func Fold(s string) string {
	s = norm.NFKD.String(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\uFEFF' || unicode.Is(unicode.M, r) {
			continue
		}
		r = unicode.ToLower(r)
		if r == 'ς' {
			r = 'σ'
		}
		b.WriteRune(r)
	}
	return strings.TrimFunc(b.String(), unicode.IsSpace)
}

func isWord(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }

// Tier is how well label matches query; see the package doc.
func Tier(label, query string) int {
	q := []rune(Fold(query))
	if len(q) == 0 {
		return 0
	}
	l := []rune(Fold(label))
	at := func(i int) bool { // q occurs at rune i of l
		if i+len(q) > len(l) {
			return false
		}
		for j, r := range q {
			if l[i+j] != r {
				return false
			}
		}
		return true
	}
	if at(0) {
		return 0
	}
	found := false
	for i := 1; i+len(q) <= len(l); i++ {
		if at(i) {
			found = true
			if !isWord(l[i-1]) {
				return 1
			}
		}
	}
	if found {
		return 2
	}
	k := 0
	for _, r := range l {
		if k < len(q) && r == q[k] {
			k++
		}
	}
	if k == len(q) {
		return 3
	}
	return -1
}
