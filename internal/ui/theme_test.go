package ui

import (
	"bytes"
	"fmt"
	"go/scanner"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/taskstatus"
)

// ---------------------------------------------------------------------------
// THE PALETTE IS PARSED OUT OF THE SHIPPED STYLESHEET SOURCE, NOT RESTATED.
//
// 🔴 Every assertion below reads web/css/input.css itself. A fixture copy of the
// tokens would be a claim about a file nobody serves: the CSS could drift to a
// failing pair while a fixture kept passing. Parsing the real file means an edit
// to a token is checked the moment it is made.
// ---------------------------------------------------------------------------

type rgb struct{ r, g, b int }

func (c rgb) hex() string { return fmt.Sprintf("#%02X%02X%02X", c.r, c.g, c.b) }

// themeTokens is one theme's `--mu-*` set, keyed by name without the prefix.
type themeTokens map[string]rgb

var (
	tokenLineRe = regexp.MustCompile(`--mu-([a-z0-9-]+):\s*(\d+)\s+(\d+)\s+(\d+)\s*;\s*/\*\s*(#[0-9A-Fa-f]{6})\s*\*/`)
	anyTokenRe  = regexp.MustCompile(`--mu-([a-z0-9-]+)\s*:`)
)

// readInputCSS returns web/css/input.css.
func readInputCSS(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "web", "css", "input.css"))
	if err != nil {
		t.Fatalf("read web/css/input.css: %v", err)
	}
	return string(b)
}

// cssBlock returns the body of the first rule whose selector text is exactly
// sel (whitespace-normalised), up to its closing brace.
func cssBlock(t *testing.T, css, sel string) string {
	t.Helper()
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	i := 0
	for {
		j := strings.Index(css[i:], "{")
		if j < 0 {
			t.Fatalf("no rule with selector %q in input.css", sel)
		}
		head := css[i : i+j]
		// The selector is whatever follows the previous `}`, `;` or comment end.
		cut := strings.LastIndexAny(head, "};/")
		if norm(head[cut+1:]) == sel {
			body := css[i+j+1:]
			k := strings.Index(body, "}")
			if k < 0 {
				t.Fatalf("unterminated rule %q", sel)
			}
			return body[:k]
		}
		i += j + 1
	}
}

// parseTokens reads every `--mu-name: R G B; /* #HEX */` line of a block and
// fails if a custom property does not have that exact shape — a token the
// parser silently skipped would be a token the contrast table never checked.
func parseTokens(t *testing.T, block, theme string) themeTokens {
	t.Helper()
	out := themeTokens{}
	for _, m := range tokenLineRe.FindAllStringSubmatch(block, -1) {
		r, _ := strconv.Atoi(m[2])
		g, _ := strconv.Atoi(m[3])
		b, _ := strconv.Atoi(m[4])
		c := rgb{r, g, b}
		if !strings.EqualFold(c.hex(), m[5]) {
			t.Errorf("%s --mu-%s: the channels say %s but the comment says %s. The CHANNELS "+
				"are what ships; fix the comment (or the channels) so a reader is not misled.",
				theme, m[1], c.hex(), m[5])
		}
		out[m[1]] = c
	}
	all := anyTokenRe.FindAllStringSubmatch(block, -1)
	if len(all) != len(out) {
		var missed []string
		for _, m := range all {
			if _, ok := out[m[1]]; !ok {
				missed = append(missed, m[1])
			}
		}
		t.Fatalf("%s block declares %d --mu-* properties but only %d parse as `R G B; /* #HEX */`: %v",
			theme, len(all), len(out), missed)
	}
	return out
}

// themes returns the dark and light token sets from input.css.
func themes(t *testing.T) (dark, light themeTokens) {
	t.Helper()
	css := readInputCSS(t)
	return parseTokens(t, cssBlock(t, css, ":root, .dark"), "dark"),
		parseTokens(t, cssBlock(t, css, ":root:not(.dark)"), "light")
}

func channelLum(v int) float64 {
	c := float64(v) / 255
	if c <= 0.04045 {
		return c / 12.92
	}
	return math.Pow((c+0.055)/1.055, 2.4)
}

func luminance(c rgb) float64 {
	return 0.2126*channelLum(c.r) + 0.7152*channelLum(c.g) + 0.0722*channelLum(c.b)
}

// contrast is the WCAG 2.x contrast ratio.
func contrast(a, b rgb) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

type contrastPair struct {
	fg, bg string
	min    float64
}

// contrastPairs is the table the palette was designed against (the theme
// proposal's contrast table, WCAG 2.2 AA): text roles on every surface at 4.5,
// the accent as link text, the two filled-button labels, the focus ring and the
// control edge at 3 (1.4.11 / 2.4.13), and every status's chip label at 4.5 and
// glyph on the card at 3.
//
// ⚠ `line` IS ABSENT ON PURPOSE: it is a decorative divider between regions,
// which 1.4.11 does not cover. A control's boundary is `edge`, which is here.
func contrastPairs() []contrastPair {
	var out []contrastPair
	for _, fg := range []string{"text", "text2", "muted"} {
		for _, bg := range []string{"bg", "s1", "s2", "s3"} {
			out = append(out, contrastPair{fg, bg, 4.5})
		}
	}
	for _, bg := range []string{"bg", "s1", "s2", "s3"} {
		out = append(out, contrastPair{"accent", bg, 4.5})
	}
	out = append(out, contrastPair{"on-accent", "accent", 4.5}, contrastPair{"on-danger", "danger", 4.5})
	for _, bg := range []string{"bg", "s1", "s2"} {
		out = append(out, contrastPair{"focus", bg, 3})
	}
	for _, bg := range []string{"bg", "s1"} {
		out = append(out, contrastPair{"edge", bg, 3})
	}
	out = append(out, contrastPair{"danger", "s1", 3})
	for _, st := range []string{"open", "progress", "review", "complete", "error", "warning", "running"} {
		out = append(out,
			contrastPair{"st-" + st + "-fg", "st-" + st + "-bg", 4.5},
			contrastPair{"st-" + st + "-fg", "s1", 3})
	}
	return out
}

// contrastFailures checks every pair against one theme and returns one line per
// failure (or per missing token). It is the instrument; the test below and its
// negative control both run it.
func contrastFailures(theme string, tok themeTokens) []string {
	var bad []string
	for _, p := range contrastPairs() {
		fg, okf := tok[p.fg]
		bg, okb := tok[p.bg]
		if !okf || !okb {
			bad = append(bad, fmt.Sprintf("%s: %s on %s — token missing", theme, p.fg, p.bg))
			continue
		}
		if r := contrast(fg, bg); r < p.min {
			bad = append(bad, fmt.Sprintf("%s: %s %s on %s %s = %.2f:1, needs %.1f:1",
				theme, p.fg, fg.hex(), p.bg, bg.hex(), r, p.min))
		}
	}
	return bad
}

// TestThemeTokensMeetWCAG asserts every designed pair, in both themes, at its
// AA minimum — against the values input.css actually ships.
func TestThemeTokensMeetWCAG(t *testing.T) {
	// The instrument first. A contrast function that returned a constant would
	// pass every pair below; these are the two textbook anchors.
	if r := contrast(rgb{0, 0, 0}, rgb{255, 255, 255}); math.Abs(r-21) > 1e-9 {
		t.Fatalf("contrast(black, white) = %v, want 21", r)
	}
	if r := contrast(rgb{0x77, 0x77, 0x77}, rgb{255, 255, 255}); math.Abs(r-4.48) > 0.01 {
		t.Fatalf("contrast(#777, white) = %.3f, want 4.48 (the classic just-fails-AA grey)", r)
	}

	dark, light := themes(t)
	pairs := contrastPairs()
	// 38 pairs per theme is the size of the designed table (76 across both). A
	// table that shrank would be checking less while reporting the same green.
	if len(pairs) != 38 {
		t.Fatalf("the contrast table has %d pairs, want 38 per theme", len(pairs))
	}
	for _, f := range append(contrastFailures("dark", dark), contrastFailures("light", light)...) {
		t.Error(f)
	}

	// 🔴 NEGATIVE CONTROL: the checker must be able to go red on THESE tokens.
	// Copy the dark set, make body text equal to the card surface, and expect
	// exactly the pairs that involve `text` on s1 to fail. Without this, a
	// checker that never reached a comparison (an empty pair table, a skipped
	// loop) would print nothing — the same nothing a perfect palette prints.
	broken := themeTokens{}
	for k, v := range dark {
		broken[k] = v
	}
	// on-accent appears in exactly one pair, so this mutation must fail exactly
	// that one — a count of 1 rather than "some", so a checker that reported
	// every pair regardless would fail the control too.
	broken["on-accent"] = dark["accent"]
	got := contrastFailures("control", broken)
	if len(got) != 1 || !strings.Contains(got[0], "on-accent ") || !strings.Contains(got[0], " on accent ") {
		t.Fatalf("negative control: on-accent == accent should fail exactly the one `on-accent on "+
			"accent` pair, got %d: %v", len(got), got)
	}
}

// TestThemeTokenSetsMatch fails if one theme defines a token the other lacks —
// a role that exists only in dark resolves to nothing in light and the element
// silently loses its colour.
func TestThemeTokenSetsMatch(t *testing.T) {
	dark, light := themes(t)
	if len(dark) != 36 {
		t.Errorf("the dark block has %d tokens, want 36 (14 roles + 7 status pairs + the 8-step recency scale)", len(dark))
	}
	for k := range dark {
		if _, ok := light[k]; !ok {
			t.Errorf("--mu-%s is defined for dark but not for light", k)
		}
	}
	for k := range light {
		if _, ok := dark[k]; !ok {
			t.Errorf("--mu-%s is defined for light but not for dark", k)
		}
	}
}

// TestThemeColorsAreTheBackgroundTokens pins the Go constants that feed the
// theme-color metas and the manifest to the stylesheet's own `bg` token.
func TestThemeColorsAreTheBackgroundTokens(t *testing.T) {
	dark, light := themes(t)
	if got := dark["bg"].hex(); !strings.EqualFold(got, ThemeColorDark) {
		t.Errorf("ThemeColorDark = %s but input.css's dark --mu-bg is %s. The status bar would "+
			"no longer match the page under it.", ThemeColorDark, got)
	}
	if got := light["bg"].hex(); !strings.EqualFold(got, ThemeColorLight) {
		t.Errorf("ThemeColorLight = %s but input.css's light --mu-bg is %s.", ThemeColorLight, got)
	}
}

// TestEveryTailwindRoleHasATokenInBothThemes is the seam between
// tailwind.config.js and input.css, in BOTH directions: a role whose variable
// does not exist renders no colour, and a token no role reads is dead.
func TestEveryTailwindRoleHasATokenInBothThemes(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "tailwind.config.js"))
	if err != nil {
		t.Fatalf("read tailwind.config.js: %v", err)
	}
	roles := map[string]bool{}
	for _, m := range regexp.MustCompile(`role\('([a-z0-9-]+)'\)`).FindAllStringSubmatch(string(b), -1) {
		roles[m[1]] = true
	}
	if len(roles) < 20 {
		t.Fatalf("found only %d role('…') references in tailwind.config.js; the pattern no longer "+
			"matches the config and a clean result would mean nothing", len(roles))
	}
	dark, light := themes(t)
	for r := range roles {
		if _, ok := dark[r]; !ok {
			t.Errorf("tailwind.config.js reads --mu-%s, which the dark block does not define", r)
		}
		if _, ok := light[r]; !ok {
			t.Errorf("tailwind.config.js reads --mu-%s, which the light block does not define", r)
		}
	}
	for k := range dark {
		if !roles[k] {
			t.Errorf("--mu-%s is defined but no Tailwind role reads it", k)
		}
	}
}

// ---------------------------------------------------------------------------
// NO VIEW NAMES A HUE.
// ---------------------------------------------------------------------------

var (
	hexColourRe     = regexp.MustCompile(`#(?:[0-9a-fA-F]{8}|[0-9a-fA-F]{6}|[0-9a-fA-F]{3,4})\b`)
	paletteClassRe  = regexp.MustCompile(`\b(?:bg|text|border|ring|divide|outline|placeholder|fill|stroke|decoration|shadow|from|via|to|caret|accent)-(?:slate|gray|zinc|neutral|stone|red|orange|amber|yellow|lime|green|emerald|teal|cyan|sky|blue|indigo|violet|purple|fuchsia|pink|rose|white)\b`)
	colourScanFiles = []string{"../api/login.go"}
	jsLineCommentRe = regexp.MustCompile("(?m)^[ \t]*//.*$")
)

// colourLiterals scans the STRING literals of each Go file (comments are prose
// and may name a hue to explain a change) and returns one line per hex colour
// or Tailwind palette class found. theme.go is exempt: its two ThemeColor
// constants are the single sanctioned source, pinned to input.css above.
func colourLiterals(t *testing.T, files []string) []string {
	t.Helper()
	var out []string
	for _, path := range files {
		if filepath.Base(path) == "theme.go" {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		fset := token.NewFileSet()
		f := fset.AddFile(path, fset.Base(), len(src))
		var s scanner.Scanner
		s.Init(f, src, nil, 0) // mode 0: comments are skipped
		for {
			pos, tok, lit := s.Scan()
			if tok == token.EOF {
				break
			}
			if tok != token.STRING {
				continue
			}
			// A raw string here is often a whole inline <script>; its full-line
			// `//` comments are prose ("PR #727 …", a task number that happens
			// to be valid hex), exactly like Go comments. Drop them; code stays.
			if strings.HasPrefix(lit, "`") {
				lit = jsLineCommentRe.ReplaceAllString(lit, "")
			}
			for _, m := range hexColourRe.FindAllString(lit, -1) {
				out = append(out, fmt.Sprintf("%s: hex colour %s", fset.Position(pos), m))
			}
			for _, m := range paletteClassRe.FindAllString(lit, -1) {
				out = append(out, fmt.Sprintf("%s: palette class %s", fset.Position(pos), m))
			}
		}
	}
	return out
}

// uiSourceFiles is every non-test Go file in this package plus login.go.
func uiSourceFiles(t *testing.T) []string {
	t.Helper()
	m, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range m {
		if !strings.HasSuffix(p, "_test.go") {
			out = append(out, p)
		}
	}
	if len(out) < 15 {
		t.Fatalf("found only %d source files in internal/ui; the scan is pointed at the wrong place", len(out))
	}
	return append(out, colourScanFiles...)
}

// TestNoHardCodedColourInUI forbids a hex colour or a Tailwind palette class
// (slate-, emerald-, rose-, …) in any string the UI emits. Every colour is a
// role token; the palette lives in input.css.
func TestNoHardCodedColourInUI(t *testing.T) {
	// 🔴 POSITIVE CONTROL FIRST: the scanner must find a planted literal, and
	// must NOT count the same literal inside a comment. A scanner that read no
	// strings would report zero over the real tree exactly as a clean tree does.
	dir := t.TempDir()
	plant := filepath.Join(dir, "plant.go")
	planted := "package x\n\n// a comment may say #0b0f17 and bg-slate-950\n" +
		"var a = \"#0b0f17\"\n" +
		"var b = `p-2 bg-slate-950 text-fg`\n" +
		"var c = \"Task #\" + \"12\"\n" +
		"var d = `\n  // PR #727 fixed this\n  el.style.color = '#abcdef';\n`\n"
	if err := os.WriteFile(plant, []byte(planted), 0o600); err != nil {
		t.Fatal(err)
	}
	got := colourLiterals(t, []string{plant})
	if len(got) != 3 {
		t.Fatalf("positive control: the planted file has two hex literals and one palette class in "+
			"code (and three more in comments — a Go comment and a JS line comment — which must "+
			"not count); the scanner found %d: %v", len(got), got)
	}

	if bad := colourLiterals(t, uiSourceFiles(t)); len(bad) > 0 {
		t.Errorf("%d hard-coded colour(s) in the UI. Name a role token instead (bg-s1, "+
			"text-accent, bg-st-review-bg …; see tailwind.config.js), or, for the status bar "+
			"and manifest, use ThemeColorDark/ThemeColorLight:\n  %s", len(bad), strings.Join(bad, "\n  "))
	}
}

// ---------------------------------------------------------------------------
// THEME-COLOR METAS AND THE COMPLETED CARD.
// ---------------------------------------------------------------------------

var themeColorMetaRe = regexp.MustCompile(`<meta name="theme-color" content="([^"]*)" media="([^"]*)" data-theme-color="([^"]*)">`)

// TestEveryDocumentCarriesBothThemeColorMetas: each standalone document ships
// the dark meta live (media=all) and the light one parked (media="not all"),
// with the CONSTANTS as content, and no other theme-color meta.
func TestEveryDocumentCarriesBothThemeColorMetas(t *testing.T) {
	docs := shellDocuments(t, Features{GitHub: true})
	docs["task detail"] = renderString(t, TaskDetailPage(sampleTaskDetailView(), Features{}))
	for name, html := range docs {
		if n := strings.Count(html, `<meta name="theme-color"`); n != 2 {
			t.Errorf("%s: %d theme-color metas, want exactly 2", name, n)
			continue
		}
		m := themeColorMetaRe.FindAllStringSubmatch(html, -1)
		if len(m) != 2 {
			t.Errorf("%s: theme-color metas are not in the expected shape: %v", name, m)
			continue
		}
		want := [][3]string{{ThemeColorDark, "all", "dark"}, {ThemeColorLight, "not all", "light"}}
		for i, w := range want {
			if m[i][1] != w[0] || m[i][2] != w[1] || m[i][3] != w[2] {
				t.Errorf("%s: theme-color meta %d = content %q media %q theme %q, want %q %q %q",
					name, i, m[i][1], m[i][2], m[i][3], w[0], w[1], w[2])
			}
		}
		if !strings.Contains(html, "window.musterSetTheme") {
			t.Errorf("%s: the theme head script is missing, so a stored light opt-in is never applied", name)
		}
	}
}

// TestACompletedCardIsDimmedByColourNotOpacity: opacity scaled every text pair
// in the card below AA; the finished card now steps down a SURFACE instead.
func TestACompletedCardIsDimmedByColourNotOpacity(t *testing.T) {
	render := func(status string) string {
		var b bytes.Buffer
		if err := RenderNoteCard(&b, TaskCardView{Note: notes.Note{ID: 7, Title: "t", Status: status, CreatedAt: time.Unix(946684800, 0), UpdatedAt: time.Unix(946684800, 0)}}); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	done := classOf(between(render(notes.StatusComplete), "<article", ">"))
	open := classOf(between(render(notes.StatusOpen), "<article", ">"))
	if done == "" || open == "" {
		t.Fatalf("could not read the card's own class list (done=%q open=%q)", done, open)
	}
	for _, c := range strings.Fields(done) {
		if strings.HasPrefix(c, "opacity-") || strings.Contains(c, ":opacity-") {
			t.Errorf("the completed card is dimmed with %q; opacity scales every text pair inside it "+
				"below AA. Step the surface down instead.", c)
		}
	}
	if !hasClass(done, "bg-bg") || hasClass(done, "bg-s1") {
		t.Errorf("the completed card should sit on bg-bg (one step below a live card), got %q", done)
	}
	if !hasClass(open, "bg-s1") || hasClass(open, "bg-bg") {
		t.Errorf("a live card should sit on bg-s1, got %q", open)
	}
}

// ---------------------------------------------------------------------------
// STATUS GLYPHS.
// ---------------------------------------------------------------------------

// TestEveryGlyphKindHasAShape renders each kind and fails on an empty or a
// shared shape: two statuses with one shape are distinguishable by colour only.
func TestEveryGlyphKindHasAShape(t *testing.T) {
	seen := map[string]string{}
	for _, k := range glyphKinds {
		svg := glyphSVG[k]
		if svg == "" {
			t.Errorf("glyph kind %q has no shape", k)
		}
		if other, dup := seen[svg]; dup {
			t.Errorf("glyph kinds %q and %q draw the same shape", k, other)
		}
		seen[svg] = k
		for name, m := range map[string]map[string]string{"text": glyphText, "chip": glyphChip, "select": glyphSelect, "knock": glyphKnockVar} {
			if m[k] == "" {
				t.Errorf("glyph kind %q has no %s classes", k, name)
			}
		}
	}
	if len(glyphKinds) != len(glyphSVG) {
		t.Errorf("glyphKinds lists %d kinds but glyphSVG draws %d", len(glyphKinds), len(glyphSVG))
	}
}

// TestEveryTaskStatusHasItsOwnGlyph: the four task statuses map onto four
// DIFFERENT shapes, derived from the status vocabulary itself.
func TestEveryTaskStatusHasItsOwnGlyph(t *testing.T) {
	used := map[string]string{}
	all := taskstatus.All()
	if len(all) < 4 {
		t.Fatalf("taskstatus.All() returned %d statuses", len(all))
	}
	for _, s := range all {
		k := taskGlyphKind(s)
		if prev, dup := used[k]; dup {
			t.Errorf("task statuses %q and %q share the %q glyph", s, prev, k)
		}
		used[k] = s
	}
}

// TestStatusFilterChipsCarryTheirGlyph: every status chip draws its status's
// shape, and the selected one takes its status's chip colours.
func TestStatusFilterChipsCarryTheirGlyph(t *testing.T) {
	for _, active := range append([]string{""}, taskstatus.All()...) {
		html := renderString(t, statusFilterRow(active))
		for _, s := range taskstatus.All() {
			btn := between(html, `data-status-filter="`+s+`"`, "</button>")
			want := `data-glyph="` + taskGlyphKind(s) + `"`
			if !strings.Contains(btn, want) {
				t.Errorf("active=%q: the %q chip does not carry %s", active, s, want)
			}
		}
		if active != "" {
			btn := between(html, `data-status-filter="`+active+`"`, ">")
			for _, c := range strings.Fields(glyphChip[taskGlyphKind(active)]) {
				if !strings.Contains(btn, c) {
					t.Errorf("the selected %q chip lacks its chip class %q: %s", active, c, btn)
				}
			}
		}
	}
}

// TestTheStatusSelectShowsTheCurrentStatusGlyph: the glyph beside the select is
// the CURRENT status's shape, for every status.
func TestTheStatusSelectShowsTheCurrentStatusGlyph(t *testing.T) {
	var kinds []string
	for _, s := range taskstatus.All() {
		html := renderString(t, taskStatusSelect(1, s))
		m := regexp.MustCompile(`data-glyph="([a-z]+)"`).FindAllStringSubmatch(html, -1)
		if len(m) != 1 || m[0][1] != taskGlyphKind(s) {
			t.Errorf("status %q: want exactly one %q glyph beside the select, got %v", s, taskGlyphKind(s), m)
			continue
		}
		kinds = append(kinds, m[0][1])
	}
	sort.Strings(kinds)
	if len(kinds) != len(taskstatus.All()) {
		t.Errorf("only %d of %d statuses rendered a glyph", len(kinds), len(taskstatus.All()))
	}
}
