package suggest

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/redact"
)

// THE POST-FILTER: what keeps a chip specific to THIS conversation and safe to
// put in a composer. The model's output is untrusted; every rule here is
// applied to every suggestion, and the first one it fails drops it with that
// rule's Drop reason (which a test names, so each rule is pinned on its own).
//
// The STRUCTURAL guards — the ones that hold whatever the model writes:
//   - DropKind: the kind is one of the five;
//   - DropCite: every cited id exists in the input (R, or an A<n> it carried);
//   - DropQuote: the quote is verbatim (whitespace folded) in a CITED source,
//     and at least MinQuoteBytes long unless it is that whole source;
//   - DropPickNotR / DropDecideNotR: a pick, and a decide, cite R and quote
//     it — an option or a proposed action must be the AGENT'S OWN, which is
//     what stops text quoted from elsewhere steering an approval;
//   - DropGapNoAsk: a gap cites an operator ask;
//   - DropActNotR: whatever its kind, a suggestion whose TEXT approves or
//     acts (approve, deploy, push, merge, delete, force, run, ship, release,
//     publish, execute, install, rollback, reset, rebase, go ahead, proceed,
//     yes…) cites R and quotes it — the model's label is not trusted to say
//     what a suggestion does;
//   - DropSlashBang: the text does not start with "/", "!" or "#" (in Claude
//     Code a leading "/" is a command, "!" is bash mode and "#" a memory
//     note; a paste passes each as typed);
//   - DropMultiline, DropURL, DropFence: one line, no URL, no code fence;
//   - DropSecret: redact.Secrets leaves the text (and quote) unchanged;
//   - DropUnrelated: the text shares a content word with what it cites (its
//     quote, or a cited source) — a quote proves only that IT is in the
//     input, so a chip about something else entirely, hung on an unrelated
//     quote, is dropped;
//   - DropUnknownTerm: every identifier-like token in the text (a --flag, a
//     path, a version, a name with a digit, dot, slash or underscore) appears
//     somewhere in the input. A term the model brought from nowhere — or from
//     text that was never sent, like tool output — is dropped. (A plain
//     hyphenated word — "re-reading", "integration-untested" — is English,
//     not a term, and is not checked);
//   - DropEmpty, DropTooLong; DropDuplicate.
//
// The HEURISTIC BACKSTOP — a prose rule, flagged as such, NOT a grounding
// guard (the citation checks above are):
//   - DropStock: a stock phrase ("continue", "looks good", "ok"…) is dropped.
//
// (A decide's "reason or condition" is asked of the model, not checked here:
// a word list for it matched almost any sentence.)

// Drop is why Filter dropped a suggestion.
type Drop string

const (
	DropKind        Drop = "kind"
	DropEmpty       Drop = "empty"
	DropTooLong     Drop = "too-long"
	DropSlashBang   Drop = "slash-or-bang"
	DropMultiline   Drop = "multiline"
	DropURL         Drop = "url"
	DropFence       Drop = "code-fence"
	DropSecret      Drop = "secret"
	DropCite        Drop = "cite"
	DropQuote       Drop = "quote"
	DropPickNotR    Drop = "pick-not-r"
	DropDecideNotR  Drop = "decide-not-r"
	DropGapNoAsk    Drop = "gap-no-ask"
	DropActNotR     Drop = "act-not-r"
	DropStock       Drop = "stock-phrase"
	DropUnrelated   Drop = "unrelated-to-quote"
	DropUnknownTerm Drop = "unknown-term"
	DropDuplicate   Drop = "duplicate"
	DropOverflow    Drop = "overflow"
)

// Dropped is one suggestion Filter refused, and why (for tests and the eval;
// never logged with its text).
type Dropped struct {
	Raw    Raw
	Reason Drop
}

// Filter keeps the suggestions in raw that pass every rule, at most
// MaxSuggestions, in the model's order (a gap whose ask R already seems to
// cover moves to the end: a ranking heuristic, never a drop). b is the input
// the model was given.
func Filter(raw []Raw, b Built) ([]Suggestion, []Dropped) {
	sources := b.Sources
	var kept []Suggestion
	var late []Suggestion
	var dropped []Dropped
	seen := map[string]bool{}
	for _, r := range raw {
		text := strings.TrimSpace(r.Text)
		reason := check(r, text, b)
		if reason == "" {
			norm := normalize(text)
			if seen[norm] {
				reason = DropDuplicate
			}
			seen[norm] = true
		}
		if reason != "" {
			dropped = append(dropped, Dropped{Raw: r, Reason: reason})
			continue
		}
		s := Suggestion{Text: text, Kind: r.Kind}
		if r.Kind == KindGap && gapCovered(r, sources) {
			late = append(late, s)
			continue
		}
		kept = append(kept, s)
	}
	kept = append(kept, late...)
	if len(kept) > MaxSuggestions {
		for _, s := range kept[MaxSuggestions:] {
			dropped = append(dropped, Dropped{Raw: Raw{Text: s.Text, Kind: s.Kind}, Reason: DropOverflow})
		}
		kept = kept[:MaxSuggestions]
	}
	return kept, dropped
}

// check is the first rule r fails, or "".
func check(r Raw, text string, b Built) Drop {
	sources := b.Sources
	if !ValidKind(r.Kind) {
		return DropKind
	}
	if text == "" {
		return DropEmpty
	}
	if utf8.RuneCountInString(text) > MaxTextRunes {
		return DropTooLong
	}
	if startsSlashOrBang(text) {
		return DropSlashBang
	}
	if strings.ContainsAny(text, "\n\r\u2028\u2029\u0085") || hasControl(text) {
		return DropMultiline
	}
	if reURL.MatchString(text) {
		return DropURL
	}
	if strings.Contains(text, "```") {
		return DropFence
	}
	if redact.Secrets(text) != text || redact.Secrets(r.Quote) != r.Quote {
		return DropSecret
	}
	if len(r.Cites) == 0 {
		return DropCite
	}
	for _, c := range r.Cites {
		if _, ok := sources[c]; !ok {
			return DropCite
		}
	}
	if !quoted(r.Quote, r.Cites, sources) {
		return DropQuote
	}
	if !relatedToCited(text, r, sources) {
		return DropUnrelated
	}
	if t := unknownTerm(text, b.Prompt); t {
		return DropUnknownTerm
	}
	// 🔴 BY CONTENT, WHATEVER THE LABEL: a suggestion that approves or does
	// something — deploys, pushes, merges, deletes, forces, runs — must rest
	// on the agent's own words. The kind is the model's claim; a "redirect"
	// or "verify" that says "deploy it now" is an approval all the same.
	if actShaped(text) && (!citesR(r) || !quoted(r.Quote, []string{ReplyID}, sources)) {
		return DropActNotR
	}
	switch r.Kind {
	case KindPick:
		if !citesR(r) || !quoted(r.Quote, []string{ReplyID}, sources) {
			return DropPickNotR
		}
	case KindDecide:
		if !citesR(r) || !quoted(r.Quote, []string{ReplyID}, sources) {
			return DropDecideNotR
		}
	case KindGap:
		if !citesAsk(r) {
			return DropGapNoAsk
		}
	}
	if isStock(text) {
		return DropStock
	}
	return ""
}

// startsSlashOrBang reports whether text, past any leading space or invisible
// character, starts with "/", "!" or "#" (Claude Code's command, bash-mode and
// memory prefixes) — or a full-width look-alike of one.
func startsSlashOrBang(text string) bool {
	t := strings.TrimLeftFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) || unicode.IsControl(r)
	})
	if t == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(t)
	switch r {
	case '/', '!', '#', '／', '！', '＃':
		return true
	}
	return false
}

func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

var reURL = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://|\bwww\.`)

// quoted reports whether quote is verbatim (whitespace folded) in at least one
// of the cited sources, and long enough to mean something.
func quoted(quote string, cites []string, sources map[string]string) bool {
	q := foldSpace(quote)
	if q == "" {
		return false
	}
	for _, c := range cites {
		src, ok := sources[c]
		if !ok {
			continue
		}
		s := foldSpace(src)
		if (len(q) >= MinQuoteBytes || q == s) && strings.Contains(s, q) {
			return true
		}
	}
	return false
}

func citesR(r Raw) bool {
	for _, c := range r.Cites {
		if c == ReplyID {
			return true
		}
	}
	return false
}

func citesAsk(r Raw) bool {
	for _, c := range r.Cites {
		if strings.HasPrefix(c, "A") && len(c) > 1 {
			return true
		}
	}
	return false
}

// foldSpace collapses every run of whitespace to one space and trims.
func foldSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// stockPhrases are generic replies that fit any conversation.
// ⚠ HEURISTIC (prose), see the file comment.
var stockPhrases = map[string]bool{
	"continue": true, "please continue": true, "keep going": true, "go on": true,
	"looks good": true, "looks good to me": true, "lgtm": true, "sounds good": true,
	"thanks": true, "thank you": true, "thx": true, "great": true, "nice": true, "perfect": true,
	"ok": true, "okay": true, "k": true, "sure": true, "yes": true, "yes please": true, "no": true,
	"go ahead": true, "proceed": true, "do it": true, "yep": true, "yeah": true,
	"ok go ahead": true, "yes go ahead": true, "sounds great": true, "all good": true,
}

func isStock(text string) bool { return stockPhrases[normalize(text)] }

// normalize is text lowercased, its punctuation dropped and its spaces
// folded: what stock phrases and duplicates are compared by.
func normalize(text string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case unicode.IsSpace(r) || r == '-' || r == '_':
			b.WriteRune(' ')
		}
	}
	return foldSpace(b.String())
}

// gapCovered reports whether R already seems to mention every content word
// (five letters or more) of the asks a gap cites. ⚠ A RANKING HEURISTIC: it
// only moves the gap to the end of the set, never drops it.
func gapCovered(r Raw, sources map[string]string) bool {
	reply := strings.ToLower(sources[ReplyID])
	words := 0
	for _, c := range r.Cites {
		if c == ReplyID {
			continue
		}
		for _, w := range strings.Fields(normalize(sources[c])) {
			if len(w) < 5 {
				continue
			}
			words++
			if !strings.Contains(reply, w) {
				return false
			}
		}
	}
	return words > 0
}

// genericWords are content-free for relatedness: words any reply could share.
var genericWords = map[string]bool{
	"approve": true, "approved": true, "proceed": true, "should": true, "would": true, "could": true,
	"with": true, "that": true, "this": true, "then": true, "than": true, "them": true, "they": true,
	"first": true, "after": true, "before": true, "only": true, "also": true, "just": true, "please": true,
	"want": true, "make": true, "sure": true, "yes": true, "dont": true, "don't": true, "what": true,
	"have": true, "from": true, "into": true, "your": true, "mine": true, "here": true, "there": true,
	"okay": true, "good": true, "fine": true, "go": true, "ahead": true, "lets": true, "let's": true,
	"show": true, "it's": true, "its": true, "will": true, "when": true, "does": true, "done": true,
}

// contentWords is the normalized words of s at least four letters long that
// are not genericWords.
func contentWords(s string) []string {
	var out []string
	for _, w := range strings.Fields(normalize(s)) {
		if len(w) >= 4 && !genericWords[w] {
			out = append(out, w)
		}
	}
	return out
}

// related reports whether text shares at least one content word with quote
// (words sharing their first four letters count, so "retry" relates to
// "retries" and "test" to "tests").
func related(text, quote string) bool {
	qw := contentWords(quote)
	for _, t := range contentWords(text) {
		for _, q := range qw {
			if t == q || commonPrefix(t, q) >= 4 {
				return true
			}
		}
	}
	return false
}

// reTerm is an identifier-like token: a --flag (or -f), or a token holding a
// digit, a dot, a slash or an underscore. An inner dash alone is not enough.
var reTerm = regexp.MustCompile(`(?:^|\s)--?[A-Za-z][A-Za-z0-9_-]*|[A-Za-z0-9_-]*[0-9_/.][A-Za-z0-9_./-]*[A-Za-z0-9]`)

// unknownTerm reports whether text names an identifier-like token the input
// (prompt) does not contain anywhere.
func unknownTerm(text, prompt string) bool {
	lower := strings.ToLower(prompt)
	for _, t := range reTerm.FindAllString(text, -1) {
		t = strings.Trim(strings.TrimSpace(t), ".,;:")
		if len(t) < 3 {
			continue
		}
		// A token with a number in it ("12,400-order", "$26/month", "v2.4")
		// is checked by its NUMBERS: each must appear in the input as a number
		// of its own — "9" inside "429" does not count.
		if runs := reDigits.FindAllString(t, -1); len(runs) > 0 {
			for _, n := range runs {
				if !regexp.MustCompile(`(^|[^0-9])` + n + `([^0-9]|$)`).MatchString(prompt) {
					return true
				}
			}
			continue
		}
		if !strings.Contains(lower, strings.ToLower(t)) {
			return true
		}
	}
	return false
}

var reDigits = regexp.MustCompile(`[0-9]+`)

func commonPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// relatedToCited reports whether text shares a content word with r's quote or
// with any source r cites.
func relatedToCited(text string, r Raw, sources map[string]string) bool {
	if related(text, r.Quote) {
		return true
	}
	for _, c := range r.Cites {
		if related(text, sources[c]) {
			return true
		}
	}
	return false
}

// reAct is an approving or acting word: what makes a suggestion act-shaped.
var reAct = regexp.MustCompile(`(?i)\b(approv\w*|deploy\w*|push\w*|merg\w*|delet\w*|remov\w*|drop|forc\w*|run|runs|running|rerun|execut\w*|ship\w*|releas\w*|publish\w*|install\w*|uninstall\w*|rollback|roll\s+back|reset|rebase|apply|wipe|purge|truncat\w*|kill|restart\w*|send|sign\s+up|pay|yes|proceed|go\s+ahead|do\s+it)\b|--force`)

// actShaped reports whether text approves or acts (reAct).
func actShaped(text string) bool { return reAct.MatchString(text) }
