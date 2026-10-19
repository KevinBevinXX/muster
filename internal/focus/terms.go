package focus

import (
	_ "embed"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// DISTINCTIVE TERMS: the identifier-like words a message and a session's focus
// history must share before the chief's session hint names that session
// (MatchHistory).
//
// 🔴 ONE EXTRACTOR, BOTH SIDES. The message's terms and every history row's are
// computed by distinctiveTerms, and two terms match only when their NORMALISED
// keys are equal — so "PR-137", "PR 137", "pr137", "pull request 137" and "#137"
// are one term ("#137") wherever they are written. The tsvector parser cannot do
// that: it reads "PR-137" as `pr` and `-137`, and "#137" as a bare `137`.
//
// What is distinctive (and the key it normalises to):
//
//   - a number written AS A REFERENCE: `#137`, `PR 137`, `PR-137`, `PR #137`,
//     `pr137`, `pull request 137` → "#137". A BARE number is not: "137" alone,
//     or a message stuffed with 0–999, names nothing.
//   - a version: `v1.2` or longer with a v, `1.2.3` or longer without → "v1.2.3".
//   - a word holding letters AND digits: `node-7k`, `k8s`, `6a99a92`, `utf-8` →
//     itself, lower-cased — except an ordinal, a decade or a clock time
//     (`2nd`, `1990s`, `5pm`), which prose writes all the time.
//   - snake_case (`match_history`), camelCase (`matchHistory`, `MatchHistory`)
//     and a file name (`history_search.go`, `README.md`) → itself, lower-cased.
//   - a hyphenated word with a part (3+ letters) that is NOT a dictionary word
//     (`muster-followups`) → itself, lower-cased. One made only of dictionary
//     words (`follow-up`, `sign-up`, `self-hosted`) is prose, not an identifier.
//
// Plain words never are, whatever they are (a message's sender can add common
// words at will; TestGenericOverlapNamesNothing).
//
// 🔴 A MESSAGE CONTRIBUTES AT MOST MaxDistinctiveTerms, the FIRST distinct ones
// in its text. Without the cap a message stuffed with references (#1 #2 … #999)
// would carry one for every session's PR and sweep them all; with it, stuffing
// buys at most twenty. A history row has no cap: it is muster's own text, and
// its terms are only ever compared against the message's.

// MaxDistinctiveTerms bounds the distinctive terms one message contributes.
const MaxDistinctiveTerms = 20

var (
	// refRE: a number written as a reference. Group 1 is the number.
	// "pr" and "pull request" must start a word, so "spr 5" is not one; the
	// '#' form must not follow a word character, '&' (an HTML entity) or '/'.
	refRE = regexp.MustCompile(`(?i)(?:\bpull[ \t]+requests?|\bprs?)[ \t]*[-#]?[ \t]*(\d+)\b|(?:^|[^\w&#/])#(\d+)\b`)
	// versionRE: v1.2(.3…) or 1.2.3(.4…).
	versionRE = regexp.MustCompile(`(?i)\bv(\d+(?:\.\d+)+)\b|\b(\d+\.\d+\.\d+(?:\.\d+)*)\b`)
	// tokenRE: a word, with inner '.', '_' or '-' joining its parts.
	tokenRE = regexp.MustCompile(`[A-Za-z0-9]+(?:[._-][A-Za-z0-9]+)*`)
	// proseNumberRE: a number prose writes with letters, which is not an identifier.
	proseNumberRE = regexp.MustCompile(`^\d+(?:st|nd|rd|th|s|am|pm)$`)
	camelRE       = regexp.MustCompile(`[a-z][A-Z]`)
	lettersRE     = regexp.MustCompile(`^[a-z]+$`)
)

// dictText is the dictionary distinctiveTerms reads hyphenated words against:
// SCOWL's American English list at size 35 (scowl 2020.12.07, wamerican.35),
// the entries made only of lower-case ASCII letters, one per line; its
// copyright and permission notice are dict/COPYRIGHT. Two words are left out
// because this repository's leak gate (tests/leakscan.py) refuses them as
// private identifiers, which costs only that a compound built on them counts
// as distinctive.
//
//go:embed dict/words.txt
var dictText string

var dictWords = sync.OnceValue(func() map[string]struct{} {
	words := strings.Fields(dictText)
	m := make(map[string]struct{}, len(words))
	for _, w := range words {
		m[w] = struct{}{}
	}
	return m
})

// isDictWord reports whether w (lower-case) is a dictionary word.
func isDictWord(w string) bool {
	_, ok := dictWords()[w]
	return ok
}

// tokenKey is the key a single token normalises to, or "" when the token is
// not distinctive. Versions and references are found before tokens, so they
// never reach here.
func tokenKey(tok string) string {
	lower := strings.ToLower(tok)
	hasLetter, hasDigit := false, false
	for _, r := range lower {
		switch {
		case r >= 'a' && r <= 'z':
			hasLetter = true
		case r >= '0' && r <= '9':
			hasDigit = true
		}
	}
	if !hasLetter {
		return "" // a bare number (or 3.14): never on its own
	}
	if strings.Contains(lower, "_") {
		return lower // snake_case
	}
	if i := strings.LastIndexByte(lower, '.'); i > 0 {
		base, ext := lower[:i], lower[i+1:]
		if lettersRE.MatchString(ext) && len(ext) <= 8 && len(base) >= 2 && strings.IndexFunc(base, isLetter) >= 0 {
			return lower // a file name (or a domain)
		}
	}
	if camelRE.MatchString(tok) {
		return lower // camelCase
	}
	if hasDigit {
		if proseNumberRE.MatchString(lower) {
			return ""
		}
		return lower
	}
	if strings.Contains(lower, "-") {
		for _, part := range strings.Split(lower, "-") {
			if len(part) >= 3 && !isDictWord(part) {
				return lower
			}
		}
	}
	return ""
}

func isLetter(r rune) bool { return r >= 'a' && r <= 'z' }

// span is one distinctive term found in a text, at byte offsets [start, end).
type span struct {
	start, end int
	key        string
}

// distinctiveTerms is text's distinctive terms (see the top of this file) as
// normalised keys, in the order they first appear, at most max distinct ones
// (max <= 0: no cap) — and rest, text with EVERY distinctive term's span
// blanked out, capped or not: rest is what the generic word overlap is
// counted on, so an identifier is never counted twice (once as itself and
// again as the words the tsvector parser splits it into).
func distinctiveTerms(text string, max int) (keys []string, rest string) {
	var spans []span
	taken := make([]bool, len(text))
	claim := func(start, end int, key string) {
		for i := start; i < end; i++ {
			if taken[i] {
				return
			}
		}
		for i := start; i < end; i++ {
			taken[i] = true
		}
		spans = append(spans, span{start, end, key})
	}
	for _, m := range refRE.FindAllStringSubmatchIndex(text, -1) {
		g := 2
		if m[2] < 0 {
			g = 4
		}
		n, err := strconv.ParseUint(text[m[g]:m[g+1]], 10, 64)
		if err != nil {
			continue
		}
		start := m[0]
		if g == 4 { // the '#' form: the match may include the character before '#'
			start = m[g] - 1
		}
		claim(start, m[1], "#"+strconv.FormatUint(n, 10))
	}
	for _, m := range versionRE.FindAllStringSubmatchIndex(text, -1) {
		g := 2
		if m[2] < 0 {
			g = 4
		}
		claim(m[0], m[1], "v"+text[m[g]:m[g+1]])
	}
	for _, m := range tokenRE.FindAllStringIndex(text, -1) {
		tok := strings.TrimRight(text[m[0]:m[1]], "._-")
		if k := tokenKey(tok); k != "" {
			claim(m[0], m[0]+len(tok), k)
		}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	seen := map[string]bool{}
	b := []byte(text)
	for _, s := range spans {
		for i := s.start; i < s.end; i++ {
			b[i] = ' '
		}
		if seen[s.key] || (max > 0 && len(keys) >= max) {
			continue
		}
		seen[s.key] = true
		keys = append(keys, s.key)
	}
	return keys, string(b)
}

// keyCore is the substring every spelling of key contains (lower-cased): a
// cheap SQL prefilter for the rows that may carry it, confirmed in Go by
// distinctiveTerms. A reference's core is its number, a version's its dotted
// digits, anything else the key itself.
func keyCore(key string) string {
	switch {
	case strings.HasPrefix(key, "#"):
		return key[1:]
	case strings.HasPrefix(key, "v") && len(key) > 1 && key[1] >= '0' && key[1] <= '9':
		return key[1:]
	}
	return key
}
