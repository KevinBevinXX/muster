package ui

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	g "maragu.dev/gomponents"

	"github.com/ZacxDev/muster/internal/textplain"
)

// markdown.go — a small, dependency-free, escape-FIRST markdown renderer used to
// format machine+user-authored task bodies (and comments) on the server.
//
// Safety: the source is HTML-escaped BEFORE any markup is applied, so no author
// content can ever inject HTML/JS — the only tags in the output are the ones we
// emit. This mirrors the client-side mdRender in agents_detail.go (escape →
// inline → block) so the two renderers behave the same; we keep a Go copy rather
// than pulling a markdown library (blackfriday is only an indirect dep) to keep
// the surface tiny and the escaping guarantee auditable.
//
// Supported: bold (**x**), italic (*x* / _x_), inline code (`x`), fenced code
// blocks (```lang … ```), blockquotes (> …), bullet lists (- / *), ordered
// lists (1.), ATX headings (# … ######), thematic breaks (--- / *** / ___),
// bare-URL autolinks, GFM pipe tables, and paragraphs.
// Anything else degrades to escaped paragraph text — never to raw HTML.

var (
	mdHeadingRe   = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	mdBulletRe    = regexp.MustCompile(`^\s*[-*]\s+`)
	mdOrderedRe   = regexp.MustCompile(`^\s*\d+\.\s+`)
	mdFenceRe     = regexp.MustCompile("^```")
	mdFenceEndRe  = regexp.MustCompile("^```\\s*$")
	mdBlockquote  = regexp.MustCompile(`^\s*>\s?`)
	mdBlankLineRe = regexp.MustCompile(`^\s*$`)
	mdInlineCode  = regexp.MustCompile("`([^`]+)`")
	// mdThematicBreak matches a horizontal rule on its own line: three or more
	// -, * or _, with at most three leading spaces and nothing else but trailing
	// whitespace.
	//
	// 🔴 IT IS CHECKED BEFORE THE LIST BRANCHES AND LISTED IN THE PARAGRAPH
	// GATHER'S EXCLUSION SET, for the same reason mdTableStartsAt is: the gather
	// runs LAST and swallows every contiguous line no earlier branch claimed, so a
	// rule sitting directly above a footer line was absorbed into that paragraph
	// and emitted as the literal text `---`. That is what every ClickUp-mirrored
	// task body looks like, so it was the commonest body on the board.
	//
	// It deliberately does NOT accept internal spaces (`- - -`, which CommonMark
	// allows): `- ` is also the bullet marker, and the two grammars overlap in a
	// way no author here writes. Narrow and unambiguous beats complete.
	mdThematicBreak = regexp.MustCompile(`^ {0,3}(-{3,}|\*{3,}|_{3,})[ \t]*$`)
	mdBold          = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	mdItalicStar    = regexp.MustCompile(`(^|[^*])\*([^*\n]+)\*`)
	mdItalicUnder   = regexp.MustCompile(`(^|[^_])_([^_\n]+)_`)
	// Link: [text](url). Matched on the ALREADY-ESCAPED string, so the captured
	// groups contain escaped text/url (e.g. & → &amp;); the URL scheme is then
	// re-checked against http(s) and anything else (javascript:, data:, …) is left
	// as inert literal text. The url group excludes ')' so the match is well-bounded
	// and excludes spaces/quotes so it can't break out of the href attribute.
	// Both groups also exclude \x00, the vault delimiter. Without that, a code span
	// inside a URL (`[a](https://x/?q=`+"`y`"+`)`) had its PLACEHOLDER captured into
	// the href: the code span stashes at a lower index than the anchor, so it was
	// already restored by the time the anchor was, and the token leaked into the
	// emitted href as a raw control byte while the code text vanished. Excluding it
	// makes such input stay literal markdown — which is exactly what it did before
	// the vault existed.
	//
	// NOTE: the JS renderer in agents_detail.go had the same hole — its AUTOLINK
	// excluded the sentinel but its markdown-link regex did not, so it leaked its
	// own sentinel into an href for the same input. Fixed in 0.7.79; both regexes
	// there now carry the exclusion. The two renderers use DIFFERENT sentinels
	// (\x00 here, U+E000 in the JS), so keep the fixes parallel in intent but do
	// not "sync" them by copying one into the other.
	mdLink = regexp.MustCompile("\\[([^\\]\\x00]+)\\]\\(([^)\\s\\x00]+)\\)")
	// mdHTTPScheme gates which link URLs become anchors: http:// or https:// only.
	// Case-insensitive so HTTPS:// etc. still pass; the escaped URL's scheme is
	// scheme:// ASCII, unaffected by mdEscape.
	mdHTTPScheme = regexp.MustCompile(`(?i)^https?://`)
	// mdBareURL matches a BARE http(s) URL — one an author wrote without
	// [text](url) around it. Agents paste pull-request and issue links into task
	// bodies and comments constantly, and every one of them rendered as inert text
	// here while the JS renderer in agents_detail.go had autolinked them since it
	// shipped: one document, two surfaces, two answers.
	//
	// The \x00 exclusion is the vault sentinel, for the same reason mdLink carries
	// it — a URL directly abutting a placeholder must not swallow it into the href.
	// Running AFTER mdLinkify is what stops a markdown link's URL being linked
	// twice: mdLinkify stashes the WHOLE anchor, so its href is not in the string
	// this pattern sees.
	mdBareURL = regexp.MustCompile(`(?i)https?://[^\s<\x00]+`)
	// mdURLTrail is the trailing punctuation that belongs to the SENTENCE, not to
	// the URL — `see https://example.com/x.` must not link the full stop. Mirrors
	// the JS renderer's set exactly.
	mdURLTrail = regexp.MustCompile(`[.,)\]!?:;"']+$`)
	// mdVaultToken matches a vault placeholder for the single-pass restore.
	mdVaultToken = regexp.MustCompile("\x00(\\d+)\x00")
	// mdTableSepCell matches ONE cell of a GFM separator row: dashes with an
	// optional colon at either end.
	//
	// ⚠ THE COLONS ARE PARSED AND THEN DISCARDED — THIS RENDERER DOES NOT HONOUR
	// ALIGNMENT, and that is stated rather than implied. GFM gives `:--`, `:-:`
	// and `--:` meaning; accepting them here only means the separator row is
	// RECOGNISED when an author writes one (so the table renders at all instead of
	// degrading to a paragraph full of pipes). Every cell is emitted `text-left`.
	// The JS renderer in agents_detail.go made the same trade; matching it is
	// deliberate, so the two surfaces do not disagree about one document.
	mdTableSepCell = regexp.MustCompile(`^:?-+:?$`)
)

// mdEscape HTML-escapes the five dangerous characters. Applied to ALL author
// text before any markup, so the renderer can never emit author-controlled tags.
//
// It also strips NUL, the vault delimiter (mdVaultSep). Doing it HERE rather than
// in mdInline covers every path — including fenced code blocks, which do not go
// through mdInline and so previously carried an author NUL straight into the
// output. NUL is not renderable content, so dropping it loses nothing. This
// mirrors the JS renderer, which strips its own  sentinel in mdEsc.
func mdEscape(s string) string {
	s = strings.ReplaceAll(s, mdVaultSep, "")
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	s = strings.ReplaceAll(s, "'", "&#39;")
	return s
}

// mdVaultSep delimits a placeholder token. NUL is used because mdEscape strips it
// from all author text and no pass emits it, so a token can never collide with
// content; the body between the
// delimiters is digits only, so no later pattern can match inside it.
const mdVaultSep = "\x00"

// mdVault holds already-emitted HTML during inline rendering so that later
// substitution passes cannot see it.
//
// This exists because every inline pass runs over the OUTPUT of the previous
// ones. Without it, emphasis chews on the renderer's own markup:
//
//	[a](u1) and [b](u2)  →  target="<em>blank" … target="</em>blank"
//
// i.e. any two links in one paragraph destroyed each other's attributes, and a
// single `_` in an href paired with the `_` in target="_blank". Emitted markup is
// not author text and must never be re-parsed as markdown.
type mdVault struct{ items []string }

// stash returns an inert placeholder standing in for html until restore.
func (v *mdVault) stash(html string) string {
	v.items = append(v.items, html)
	return mdVaultSep + strconv.Itoa(len(v.items)-1) + mdVaultSep
}

// restore swaps every placeholder back for its HTML in a SINGLE pass.
//
// Not a ReplaceAll per item: markdownHTML gathers a whole paragraph block into one
// mdInline call, so the vault scales with the block, and one scan of the (already
// expanding) output per vaulted item is quadratic. Measured on a body that fits
// well inside maxTaskBodyLen: 195 KB with 50k code spans took 33.5s that way
// versus 0.7s before this change — and internal/ui/notes.go renders every task's
// full body on the list page, so a single such row would have made the page cost
// tens of seconds of CPU for every viewer, permanently. This mirrors what the JS
// renderer in agents_detail.go already does.
//
// An index that is not ours (or out of range) yields "" rather than leaking a raw
// control byte into the output.
//
// INVARIANT: stashed HTML must not itself contain a placeholder. A single pass does
// not rescan what it inserts, so a nested token would be emitted raw (the old
// ascending loop happened to resolve one). Nothing can produce that today — the
// only route was a code span inside a link label, which mdLink's \x00 exclusion now
// refuses — but anything that stashes COMPOSED output in future must restore it
// first.
func (v *mdVault) restore(s string) string {
	return mdVaultToken.ReplaceAllStringFunc(s, func(tok string) string {
		m := mdVaultToken.FindStringSubmatch(tok)
		if m == nil {
			return ""
		}
		i, err := strconv.Atoi(m[1])
		if err != nil || i < 0 || i >= len(v.items) {
			return ""
		}
		return v.items[i]
	})
}

// mdInline applies inline markup to an ALREADY-ESCAPED string: inline code,
// links, bold, then italic.
//
// Each pass runs only over text the previous passes did NOT emit: generated
// markup is stashed in a vault and restored at the end. Order still matters for
// author text (code first, so markdown inside a code span stays literal; links
// before emphasis so `*` in link text isn't eaten) — but correctness no longer
// depends on later patterns failing to match earlier output, which is what made
// two links in one paragraph corrupt each other.
func mdInline(escaped string) string {
	// Defence in depth: mdEscape already strips NUL from all author text, but this
	// function is package-visible and takes an "already-escaped" string on trust,
	// so re-strip rather than rely on every future caller having gone through it.
	escaped = strings.ReplaceAll(escaped, mdVaultSep, "")
	v := &mdVault{}

	// Code spans are stashed WHOLE. Their content must stay literal — the previous
	// implementation ran emphasis over it, so `a*b*c` rendered as a<em>b</em>c
	// despite the comment claiming otherwise — and the emitted class contains
	// `text-[0.85em]`, whose `]` would terminate mdLink's text group early and stop
	// a later link on the same line from rendering at all.
	s := mdInlineCode.ReplaceAllStringFunc(escaped, func(m string) string {
		g := mdInlineCode.FindStringSubmatch(m)
		if g == nil {
			return m
		}
		return v.stash(`<code class="rounded bg-bg/60 px-1 py-0.5 font-mono text-[0.85em]">` +
			g[1] + `</code>`)
	})
	s = mdLinkify(s, v)
	s = mdAutolink(s, v)
	s = mdEmphasis(s)
	return v.restore(s)
}

// mdAnchor renders ONE anchor. Both link paths — `[text](url)` and a bare URL —
// go through it so the two can never disagree about target/rel/class; a
// `rel="noopener"` present on one shape and absent on the other is precisely the
// kind of drift a second spelling produces.
//
// href and label must ALREADY be mdEscape'd: `"` is then `&quot;`, so neither can
// break out of the attribute, and no author text can reach the browser as markup.
func mdAnchor(href, label string) string {
	return `<a href="` + href + `" target="_blank" rel="noopener" class="text-accent underline decoration-accent/40 underline-offset-2 hover:text-fg">` +
		label + `</a>`
}

// mdAutolink turns a BARE http(s) URL into an anchor, stashing the whole anchor
// so later passes cannot chew on its attributes (the hazard mdVault's header
// records). Trailing sentence punctuation is left OUTSIDE the link.
//
// The URL is used as both href and label, which is safe for the same reason
// mdLinkify is: the string is already escaped, so `"` is `&quot;` and cannot
// terminate the attribute. The scheme needs no separate check here — unlike
// mdLinkify, the pattern itself only matches http:// and https://, so
// `javascript:` and `data:` are not merely rejected, they are unmatchable.
func mdAutolink(escaped string, v *mdVault) string {
	return mdBareURL.ReplaceAllStringFunc(escaped, func(m string) string {
		trail := mdURLTrail.FindString(m)
		if trail != "" {
			m = m[:len(m)-len(trail)]
		}
		if m == "" {
			return trail
		}
		return v.stash(mdAnchor(m, m)) + trail
	})
}

// mdEmphasis applies bold then italic to an already-escaped fragment.
func mdEmphasis(s string) string {
	s = mdBold.ReplaceAllString(s, `<strong>$1</strong>`)
	s = mdItalicStar.ReplaceAllString(s, `$1<em>$2</em>`)
	s = mdItalicUnder.ReplaceAllString(s, `$1<em>$2</em>`)
	return s
}

// mdLinkify turns `[text](url)` into an anchor — but ONLY for http(s) URLs. The
// input is already mdEscape'd, so text+url cannot inject markup; we additionally
// gate the scheme so a `javascript:`/`data:`/relative URL never becomes an href
// (it degrades to the original literal `[text](url)` text instead). target=_blank
// + rel=noopener is the safe new-tab pattern (no window.opener back-reference).
// The WHOLE anchor is stashed, with emphasis applied to the label first.
//
// Stashing only the opening tag (an earlier attempt) left link LABELS exposed to
// the later emphasis passes, so two links whose labels each contain a `_` still
// paired ACROSS the anchor boundary:
//
//	[my_host.example.com](u1) and [other_host.example.com](u2)
//	→ …>my<em>host.example.com</a> and …>other</em>host.example.com</a>
//
// — crossing <em>/</a>, i.e. malformed HTML. Emphasizing the label here, in
// isolation, both prevents that and preserves `[*x*](u)` italicizing as it always
// has (the JS renderer vaults the whole anchor without this and does NOT support
// emphasis in a label; matching it exactly would have been a silent regression).
func mdLinkify(escaped string, v *mdVault) string {
	return mdLink.ReplaceAllStringFunc(escaped, func(m string) string {
		g := mdLink.FindStringSubmatch(m)
		if g == nil {
			return m
		}
		text, href := g[1], g[2]
		if !mdHTTPScheme.MatchString(href) {
			// Not an http(s) link → leave the literal markdown text (inert).
			return m
		}
		return v.stash(mdAnchor(href, mdEmphasis(text)))
	})
}

// renderMarkdown converts a markdown source string to a safe HTML fragment node
// (g.Raw of escaped-then-marked-up output). It is the server-side counterpart of
// the JS mdRender. The returned node is safe to embed directly.
func renderMarkdown(src string) g.Node {
	return g.Raw(markdownHTML(src))
}

// plainOr is s through the title rule (internal/textplain), or fallback —
// drawn AS WRITTEN — when s is empty or is nothing but markup.
//
// 🔴 THE FALLBACK IS NEVER PLAINED. It is an identifier the operator types: an
// agent's slug, a raw tmux session name ("tmux attach -t my__proj"). Running
// the title rule over it would show a name that does not exist. And a title
// that plains to "" (a lone "**") falls back rather than drawing an empty
// heading. TestTheTitleSinkLedger treats plainOr as an application of the
// rule and checks it applies Title to s.
func plainOr(s, fallback string) string {
	if p := textplain.Title(s); p != "" {
		return p
	}
	return fallback
}

// markdownSnippet returns a clean, single-line plain-text preview of a markdown
// body, truncated to n runes — textplain.Title plus the cap. Returns "" for an
// all-markup/empty body so the caller can omit the snippet line.
//
// 🔴 textplain.Title IS THE ONE STRIP. It replaced this file's markdownPlain,
// which removed every ` * _ # > anywhere (mangling snake_case, "#123", "C#")
// and left links, strikethrough and HTML tags in. Every title sink in this
// package calls textplain.Title directly; TestTheTitleSinkLedger
// (internal/textplain) fails on one that does not.
//
// ⚡ IT PLAINS ONLY THE FIRST LINES, enough to fill the snippet. The card shows
// n runes; plaining a 100KB body whole to show 120 of them cost ~25ms per card
// per board render (BenchmarkMarkdownSnippet). It plains the first k lines,
// doubling k until their plain text runs past n, then plains k+1 — the extra
// line is the separator row that decides whether line k is a table header
// (textplain reads tables locally, so no line's reading depends on anything
// further down). TestTheSnippetEqualsTheWholeBodysPlainText holds the result to
// "plain the whole body, then cut".
func markdownSnippet(src string, n int) string {
	return truncate(snippetPlain(src, n), n)
}

// snippetPlain is textplain.Title(src) as far as its first n runes go.
func snippetPlain(src string, n int) string {
	lines := strings.Split(src, "\n")
	for k := 8; k < len(lines); k *= 2 {
		if utf8.RuneCountInString(textplain.Title(strings.Join(lines[:k], "\n"))) > n {
			return textplain.Title(strings.Join(lines[:k+1], "\n"))
		}
	}
	return textplain.Title(src)
}

// --- GFM pipe tables ---------------------------------------------------------
//
// 🔴 PORTED FROM THE JS RENDERER'S LOGIC, NOT COPIED FROM ITS SOURCE. The
// client-side mdRender in agents_detail.go has had pipe tables since before this
// one did, and its behaviour is the specification followed here: a header row
// containing `|` immediately followed by a separator row, cells split on `|`
// with the optional outer pipes dropped, body rows gathered while they still
// contain a `|`, alignment discarded. The two renderers use DIFFERENT sentinels
// (\x00 here, U+E000 there) and different escape points (this one escapes per
// line, that one escapes the whole source up front), so keeping them parallel
// means keeping the INTENT parallel — see the note on mdLink above.
//
// ⚠ IT IS NOT A DUPLICATE OF A LIVE PATH. The JS renderer only ever walks
// `[data-md]` elements, which only chatBubble on /agents/{name} emits; the
// session and tmux chat views are rendered by THIS function and had no table
// support at all. Two implementations of one grammar is a known, accepted cost
// recorded here so the next reader does not mistake it for an oversight.

// mdTableCells splits one pipe-table row into trimmed cells, dropping the
// optional leading and trailing pipe so both `| a | b |` and `a | b` work.
func mdTableCells(line string) []string {
	s := strings.TrimSpace(line)
	s = strings.TrimPrefix(s, "|")
	s = strings.TrimSuffix(s, "|")
	cells := strings.Split(s, "|")
	for i, c := range cells {
		cells[i] = strings.TrimSpace(c)
	}
	return cells
}

// mdIsTableSep reports whether line is a GFM separator row — every cell dashes
// with optional surrounding colons, e.g. `| --- | :--: |` or `---|:--`.
func mdIsTableSep(line string) bool {
	if !strings.Contains(line, "|") {
		return false
	}
	for _, c := range mdTableCells(line) {
		if !mdTableSepCell.MatchString(c) {
			return false
		}
	}
	return true
}

// mdTableStartsAt reports whether a pipe table begins at lines[i].
//
// 🔴 ONE PREDICATE, TWO CALL SITES, AND THE SECOND IS THE ONE THAT GETS MISSED.
// markdownHTML's paragraph gather runs LAST and swallows every contiguous line
// that no earlier branch claimed — so without teaching it this same predicate,
// a table's HEADER row is absorbed into the preceding paragraph and the table
// branch is never reached for it. The JS renderer carries the identical check in
// both places for the identical reason. Spelled once here so the two cannot
// drift into disagreeing about what a table is.
func mdTableStartsAt(lines []string, i int) bool {
	if i < 0 || i+1 >= len(lines) {
		return false
	}
	return strings.Contains(lines[i], "|") && mdIsTableSep(lines[i+1])
}

// markdownHTML is the pure string transform (testable without a DOM). The output
// contains ONLY tags this function emits; all author text passed through
// mdEscape first.
func markdownHTML(src string) string {
	// Normalize newlines so \r\n / \r split cleanly.
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = strings.ReplaceAll(src, "\r", "\n")
	lines := strings.Split(src, "\n")
	var out strings.Builder
	i := 0
	for i < len(lines) {
		line := lines[i]

		// Fenced code block: ``` … ``` — raw (escaped) content, no inline markup.
		// Rendered as a COLLAPSED <details> (not an always-expanded <pre>) so raw
		// JSON / code can never wall the card: the operator sees a one-line "code"
		// summary and opens it on demand. The fence info string (```json) becomes
		// the summary label ("json code") so the kind is still scannable while
		// closed; bare ``` falls back to "code".
		if mdFenceRe.MatchString(line) {
			lang := strings.TrimSpace(strings.TrimPrefix(line, "```"))
			i++
			var code []string
			for i < len(lines) && !mdFenceEndRe.MatchString(lines[i]) {
				code = append(code, mdEscape(lines[i]))
				i++
			}
			i++ // consume closing fence (if present)
			label := "code"
			if lang != "" {
				// lang is author-controlled → escape it before using as the summary text.
				label = mdEscape(lang) + " code"
			}
			out.WriteString(`<details class="group/code my-1 rounded-lg bg-bg/60 ring-1 ring-inset ring-line">`)
			out.WriteString(`<summary class="flex cursor-pointer list-none items-center gap-1.5 px-2 py-1 text-xs font-medium text-muted transition hover:text-fg marker:content-['']"><span class="transition group-open/code:rotate-90">▸</span>` + label + `</summary>`)
			out.WriteString(`<pre class="overflow-auto rounded-b-lg px-2 pb-2 font-mono text-[0.85em] leading-relaxed"><code>`)
			out.WriteString(strings.Join(code, "\n"))
			out.WriteString(`</code></pre>`)
			out.WriteString(`</details>`)
			continue
		}

		// Thematic break. Checked BEFORE the list branches so `***` is a rule
		// rather than the opening of a bullet, and before the paragraph gather
		// (which also excludes it) so it is never swallowed as literal text.
		if mdThematicBreak.MatchString(line) {
			out.WriteString(`<hr class="my-2 border-line">`)
			i++
			continue
		}

		// Heading.
		if m := mdHeadingRe.FindStringSubmatch(line); m != nil {
			size := "text-sm"
			if len(m[1]) <= 2 {
				size = "text-base"
			}
			out.WriteString(`<div class="mt-1 font-semibold text-fg ` + size + `">`)
			out.WriteString(mdInline(mdEscape(m[2])))
			out.WriteString(`</div>`)
			i++
			continue
		}

		// Blockquote: consecutive `> ` lines collapse into one block.
		if mdBlockquote.MatchString(line) {
			var quote []string
			for i < len(lines) && mdBlockquote.MatchString(lines[i]) {
				quote = append(quote, mdInline(mdEscape(mdBlockquote.ReplaceAllString(lines[i], ""))))
				i++
			}
			out.WriteString(`<blockquote class="my-1 border-l-2 border-accent/60 bg-s2 py-1 pl-3 text-fg2">`)
			out.WriteString(strings.Join(quote, "<br>"))
			out.WriteString(`</blockquote>`)
			continue
		}

		// Bullet list.
		if mdBulletRe.MatchString(line) {
			out.WriteString(`<ul class="my-1 list-disc space-y-0.5 pl-5">`)
			for i < len(lines) && mdBulletRe.MatchString(lines[i]) {
				item := mdBulletRe.ReplaceAllString(lines[i], "")
				out.WriteString(`<li>` + mdInline(mdEscape(item)) + `</li>`)
				i++
			}
			out.WriteString(`</ul>`)
			continue
		}

		// Ordered list.
		if mdOrderedRe.MatchString(line) {
			out.WriteString(`<ol class="my-1 list-decimal space-y-0.5 pl-5">`)
			for i < len(lines) && mdOrderedRe.MatchString(lines[i]) {
				item := mdOrderedRe.ReplaceAllString(lines[i], "")
				out.WriteString(`<li>` + mdInline(mdEscape(item)) + `</li>`)
				i++
			}
			out.WriteString(`</ol>`)
			continue
		}

		// GFM pipe table. Checked AFTER the list branches (a `- a | b` line is a
		// bullet, exactly as in the JS renderer) and BEFORE the blank-line and
		// paragraph branches, which would otherwise claim the header row.
		//
		// Cells run through mdInline(mdEscape(...)) like every other author text
		// in this function, so a cell containing `<b>` renders as characters and a
		// cell containing `**x**` renders bold. The wrapper scrolls horizontally
		// rather than forcing the card to: a six-column table on a 390px phone is
		// the shape this view is read at.
		if mdTableStartsAt(lines, i) {
			head := mdTableCells(lines[i])
			i += 2 // consume the header row AND the separator row
			out.WriteString(`<div class="overflow-x-auto my-1"><table class="my-1 w-full border-collapse text-xs"><thead><tr>`)
			for _, c := range head {
				out.WriteString(`<th class="border border-line px-2 py-1 text-left">` + mdInline(mdEscape(c)) + `</th>`)
			}
			out.WriteString(`</tr></thead><tbody>`)
			for i < len(lines) && strings.Contains(lines[i], "|") && !mdBlankLineRe.MatchString(lines[i]) {
				out.WriteString(`<tr>`)
				for _, c := range mdTableCells(lines[i]) {
					out.WriteString(`<td class="border border-line px-2 py-1">` + mdInline(mdEscape(c)) + `</td>`)
				}
				out.WriteString(`</tr>`)
				i++
			}
			out.WriteString(`</tbody></table></div>`)
			continue
		}

		// Blank line.
		if mdBlankLineRe.MatchString(line) {
			i++
			continue
		}

		// Paragraph: gather contiguous non-special lines, join with <br>.
		var para []string
		for i < len(lines) && !mdBlankLineRe.MatchString(lines[i]) &&
			!mdFenceRe.MatchString(lines[i]) &&
			mdHeadingRe.FindStringSubmatch(lines[i]) == nil &&
			// 🔴 THE SECOND HALF OF THE THEMATIC-BREAK CHANGE, and the half the
			// defect was actually made of: without it this gather eats the `---`
			// that sits directly above a task's footer and renders it as text.
			!mdThematicBreak.MatchString(lines[i]) &&
			!mdBulletRe.MatchString(lines[i]) &&
			!mdOrderedRe.MatchString(lines[i]) &&
			// 🔴 THE SECOND HALF OF THE TABLE CHANGE. Without this the gather below
			// eats a table's header row into the paragraph before it, the table
			// branch above never sees it, and the table renders as a line of pipes.
			!mdTableStartsAt(lines, i) &&
			!mdBlockquote.MatchString(lines[i]) {
			para = append(para, mdEscape(lines[i]))
			i++
		}
		out.WriteString(`<p class="my-1">` + mdInline(strings.Join(para, "<br>")) + `</p>`)
	}
	return out.String()
}
