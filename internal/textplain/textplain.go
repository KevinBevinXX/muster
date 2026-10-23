// Package textplain turns a markdown-flavoured string into the plain text a
// reader of the rendered markdown would see, on one line. It is THE title rule:
// every surface that shows a title (cards, lists, detail headings, document
// <title>s, push notifications, attention, recents, focus titles, chat session
// titles, mirrored window titles) shows Title(s), never s.
//
// 🔴 ONE RULE, ONE PLACE. Before this package there were three strippers that
// disagreed: ui.markdownPlain (removed every ` * _ # > anywhere, so it mangled
// snake_case_names, "#123" and "C#" and left [text](url) links, ~~strike~~ and
// HTML tags untouched), api.notificationText (unwrapped links but stripped
// intraword underscores), and focus.oneLine (deleted every * ` # from a focus
// title BEFORE IT WAS STORED). The first two now call Title; the third strips
// no markdown at all any more, because it writes storage (see below). A title
// therefore reads the same on the card, in its <title> and on the phone, and
// TestTheTitleSinkLedger (internal/textplain) fails when a display sink reads a
// title without it.
//
// 🔴 APPLIED AT RENDER/EMIT TIME, NEVER TO STORED DATA. Titles are stored as the
// author wrote them; an edit form shows them raw. Stripping on the way out keeps
// the transformation reversible: a rule change re-renders every title, a
// stripped store could never be un-stripped.
//
// It is NOT a markdown parser. It is a set of conservative rewrites chosen so
// that ordinary non-markdown text survives unchanged: snake_case_names, a*b
// math, file_paths/with_underscores, C# and F#, "#123" issue references,
// emails, generic types like List<String>, and a bare URL that is the whole
// title. Each of those is a must-not-change case in textplain_test.go.
package textplain

import (
	"html"
	"regexp"
	"strconv"
	"strings"
)

// sentinel delimits a stashed span (code, escaped char, URL) so later rewrites
// cannot touch it. Any sentinel byte already in the input is removed first, so a
// title cannot forge a stash reference.
const sentinel = "\x00"

var (
	// Block level, per line.
	fenceLine     = regexp.MustCompile("^\\s*(```|~~~)")
	refDefinition = regexp.MustCompile(`^\s{0,3}\[[^\]]+\]:\s+\S+.*$`)
	thematicBreak = regexp.MustCompile(`^\s{0,3}([-*_])(\s*[-*_]){2,}\s*$`)
	setextLine    = regexp.MustCompile(`^\s{0,3}=+\s*$`)
	// One leading block marker: blockquote, ATX heading, bullet, ordered item,
	// GFM task-list box. Applied repeatedly ("> - [ ] item").
	leadQuote   = regexp.MustCompile(`^\s{0,3}>\s?`)
	leadHeading = regexp.MustCompile(`^\s{0,3}#{1,6}(\s+|$)`)
	leadBullet  = regexp.MustCompile(`^\s*[-*+]\s+`)
	leadOrdered = regexp.MustCompile(`^\s*\d{1,9}[.)]\s+`)
	leadTask    = regexp.MustCompile(`^\[[ xX]\]\s+`)
	// A closing ATX sequence: "## Title ##".
	trailHeading = regexp.MustCompile(`\s+#+\s*$`)
	// GFM table separator row: cells of dashes with optional alignment colons.
	tableSep = regexp.MustCompile(`^\s*(?:\|?\s*:?-{1,}:?\s*(\|\s*:?-{1,}:?\s*)+\|?|\|\s*:?-{1,}:?\s*\|)\s*$`)

	// Inline.
	// RE2 has no backreferences, so the two common fence widths are separate.
	codeSpan2   = regexp.MustCompile("``(.+?)``")
	codeSpan1   = regexp.MustCompile("`([^`]+)`")
	escapedChar = regexp.MustCompile("\\\\([!-/:-@\\[-`{-~])")
	// A link or image destination per CommonMark: no whitespace (or one in
	// <angle brackets>), then an optional quoted title. A bracket followed by a
	// parenthesis with SPACES in it is code — func Map[K comparable, V any](m
	// map[K]V) — not a link.
	image    = regexp.MustCompile(`!\[([^\]]*)\]` + linkDest)
	imageRef = regexp.MustCompile(`!\[([^\]]*)\]\[[^\]]*\]`)
	// A link's [ may not touch a letter, digit, _ or ] on its left: fns[0](arg)
	// and grid[0][1] are indexing, not links. The left character is captured
	// and put back (RE2 has no lookbehind).
	link        = regexp.MustCompile(`(^|[^\p{L}\p{N}_\]])\[([^\]]*)\]` + linkDest)
	linkRef     = regexp.MustCompile(`(^|[^\p{L}\p{N}_\]])\[([^\]]+)\]\[[^\]]*\]`)
	autolink    = regexp.MustCompile(`<((?i:https?|ftp|mailto):[^\s<>]+|[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,})>`)
	bareURL     = regexp.MustCompile(`(?i)\bhttps?://[^\s<>]+`)
	footnoteRef = regexp.MustCompile(`\[\^[^\]\s]+\]`)
	// HTML tags, limited to real HTML element names so that a generic type
	// (List<String>, Map<K, V>) or a comparison (a < b > c) is left alone.
	// A block-level tag renders as a break, so it becomes a space; an inline
	// one renders as nothing ("a<b>bold</b>c" reads "aboldc").
	htmlBlockTag  = regexp.MustCompile(`(?i)</?(blockquote|br|center|details|div|h[1-6]|hr|li|ol|p|pre|summary|table|tbody|td|th|thead|tr|ul)(\s[^<>]*)?/?>`)
	htmlInlineTag = regexp.MustCompile(`(?i)</?(a|abbr|b|big|cite|code|del|dfn|em|font|i|img|ins|kbd|mark|q|s|samp|small|span|strike|strong|sub|sup|tt|u|var)(\s[^<>]*)?/?>`)
	htmlComment   = regexp.MustCompile(`(?s)<!--.*?-->`)
	strike        = regexp.MustCompile(`~~([^~\s](?:[^~]*?[^~\s])?)~~`)
	// Emphasis. A marker must hug its content ("a * b" is left alone) and,
	// MORE STRICTLY THAN COMMONMARK, may not be intraword for `*` either: that is
	// what keeps 2*3*4 and x**2 + y**2 as written, and for `_` what keeps
	// snake_case_names and file_paths intact. The boundary characters are
	// captured and put back because RE2 has no lookbehind.
	emphasis = []*regexp.Regexp{emph(`\*`, 3), emph(`_`, 3), emph(`\*`, 2), emph(`_`, 2), emph(`\*`, 1), emph(`_`, 1)}
	stashRef = regexp.MustCompile(sentinel + `(\d+)` + sentinel)
)

// linkDest is a link or image destination: "(" then a destination with no
// whitespace (or <one in angle brackets>), an optional "title" / 'title' /
// (title), then ")".
const linkDest = `\(\s*(?:<[^<>\n]*>|[^\s()<>]*(?:\([^\s()]*\)[^\s()<>]*)*)(?:\s+(?:"[^"]*"|'[^']*'|\([^)]*\)))?\s*\)`

// Title returns s as the one-line plain text a reader of its rendered markdown
// would see: links become their text, images their alt text, emphasis, strike
// and code markers are removed, heading/list/blockquote/table markers removed,
// HTML tags removed, entities decoded once, whitespace collapsed. Fenced code
// blocks are dropped whole (a title has no use for a code block, and a body
// snippet built from this reads as prose).
func Title(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, sentinel, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = htmlComment.ReplaceAllString(s, " ")
	s = blocks(s)

	var vault []string
	stash := func(v string) string {
		vault = append(vault, v)
		return sentinel + strconv.Itoa(len(vault)-1) + sentinel
	}

	// Code spans first: their contents are literal, so nothing below may touch
	// them ("`**kwargs`" reads as **kwargs).
	for _, re := range []*regexp.Regexp{codeSpan2, codeSpan1} {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			return stash(strings.TrimSpace(re.FindStringSubmatch(m)[1]))
		})
	}
	// Backslash escapes are literal characters.
	s = escapedChar.ReplaceAllStringFunc(s, func(m string) string { return stash(m[1:]) })

	s = image.ReplaceAllString(s, "$1")
	s = imageRef.ReplaceAllString(s, "$1")
	s = link.ReplaceAllString(s, "$1$2")
	s = linkRef.ReplaceAllString(s, "$1$2")
	s = footnoteRef.ReplaceAllString(s, "")
	s = autolink.ReplaceAllStringFunc(s, func(m string) string { return stash(m[1 : len(m)-1]) })
	// A bare URL is shown as written, so emphasis rules must not reach into it
	// (https://x/a_b_c?q=*y*).
	s = bareURL.ReplaceAllStringFunc(s, stash)
	s = htmlBlockTag.ReplaceAllString(s, " ")
	s = htmlInlineTag.ReplaceAllString(s, "")
	s = strike.ReplaceAllString(s, "$1")
	// Nested emphasis (***x***, **_x_**) settles in a couple of passes; each
	// pass only ever removes markers, so it cannot loop.
	for range 8 {
		prev := s
		for _, re := range emphasis {
			s = re.ReplaceAllString(s, "$1$2$3")
		}
		if s == prev {
			break
		}
	}

	// Entities decoded ONCE, so "&lt;b&gt;" reads as the literal <b> the
	// rendered markdown shows rather than being decoded and then stripped — and
	// BEFORE the vault is restored, because a code span, an escaped character
	// and a URL are literal: "`&amp;`" reads "&amp;". (A decoded entity cannot
	// forge a vault reference: no entity decodes to the NUL sentinel.)
	s = html.UnescapeString(s)
	// Restore the vault (a stashed value can itself hold a reference only if a
	// code span contained a sentinel, which was stripped above; one pass suffices).
	s = stashRef.ReplaceAllStringFunc(s, func(m string) string {
		i, err := strconv.Atoi(m[1 : len(m)-1])
		if err != nil || i < 0 || i >= len(vault) {
			return ""
		}
		return vault[i]
	})
	return strings.Join(strings.Fields(s), " ")
}

// emph matches a run of n emphasis markers m around content that starts and
// ends with a non-space, non-marker character.
//
// The characters allowed immediately OUTSIDE the run are what keep ordinary
// text intact, and they are stricter than CommonMark on purpose:
//   - `*` may not touch a letter or digit outside (2*3*4, x**2 + y**2 stay),
//     except a CJK one (those scripts have no spaces between words);
//   - `_` may only open after start/space/an opening bracket or quote/another
//     marker, and only close before end/space/closing punctuation other than
//     '.' or '/' — which keeps snake_case_names, file_paths/with_underscores
//     and __init__.py as written.
//
// The outside characters are captured and put back (RE2 has no lookbehind).
func emph(m string, n int) *regexp.Regexp {
	run := strings.Repeat(m, n)
	// CJK scripts put no spaces between words, so a letter of theirs next to
	// the run must not block it ("**太字**テスト" is bold); Latin-style letters
	// and digits still do (2*3*4, x**2 stay).
	const cjk = `\p{Han}\p{Hiragana}\p{Katakana}\p{Hangul}`
	c, before, after := `*`, `(?:[^\p{L}\p{N}*]|[`+cjk+`])`, `(?:[^\p{L}\p{N}*]|[`+cjk+`])`
	if m == `_` {
		c, before, after = `_`, `[\s(\[{"'*~]`, `[\s,;:!?)\]}"'*~]`
	}
	// A single marker's content may not hold its own marker; a double or
	// triple run's may hold single ones ("**2*3**" is bold "2*3").
	inner := `[^` + c + `]*?`
	if n > 1 {
		inner = `.*?`
	}
	body := `[^` + c + `\s](?:` + inner + `[^` + c + `\s])?`
	return regexp.MustCompile(`(^|` + before + `)` + run + `(` + body + `)` + run + `($|` + after + `)`)
}

// blocks strips the block-level markup line by line and drops the lines that
// render as nothing (fenced code, rules, setext underlines, link reference
// definitions, table separator rows).
func blocks(s string) string {
	lines := strings.Split(s, "\n")
	// A table is LOCAL, as in GFM: the pipe line right above a separator row is
	// its header, and the pipe lines right below it are its rows. (It used to
	// be "any separator anywhere makes every pipe line a row", which also made
	// a line's reading depend on text far below it — and so a snippet built
	// from the first lines could disagree with the whole body.)
	tableRow := make([]bool, len(lines))
	for i, l := range lines {
		if !strings.Contains(l, "|") || !tableSep.MatchString(l) {
			continue
		}
		if i > 0 && strings.Contains(lines[i-1], "|") {
			tableRow[i-1] = true
		}
		for j := i + 1; j < len(lines) && strings.Contains(lines[j], "|") && !tableSep.MatchString(lines[j]); j++ {
			tableRow[j] = true
		}
	}
	out := make([]string, 0, len(lines))
	inFence := false
	for i, l := range lines {
		if fenceLine.MatchString(l) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if thematicBreak.MatchString(l) || setextLine.MatchString(l) || refDefinition.MatchString(l) {
			continue
		}
		if strings.Contains(l, "|") && tableSep.MatchString(l) {
			continue
		}
		heading := false
		for {
			prev := l
			l = leadQuote.ReplaceAllString(l, "")
			if h := leadHeading.ReplaceAllString(l, ""); h != l {
				l, heading = h, true
			}
			l = leadBullet.ReplaceAllString(l, "")
			l = leadOrdered.ReplaceAllString(l, "")
			l = leadTask.ReplaceAllString(l, "")
			if l == prev {
				break
			}
		}
		if heading {
			// Only on a heading line, and only a run of '#' after a space, is a
			// closing sequence: "## Title ##". "C#" and "issue #" elsewhere are text.
			l = trailHeading.ReplaceAllString(l, "")
		}
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "|") || tableRow[i] {
			cells := strings.Split(strings.Trim(trimmed, "|"), "|")
			for i := range cells {
				cells[i] = strings.TrimSpace(cells[i])
			}
			l = strings.Join(cells, " ")
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}
