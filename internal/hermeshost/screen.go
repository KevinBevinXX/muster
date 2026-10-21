package hermeshost

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/width"
)

// THE SCREEN CHECK (docs/design/hermes.md, "The screen check"): what a Hermes
// CLI's pane must look like before muster pastes into it, and after.
//
// The screen is the only signal for states no hook reports (the /new confirm,
// the hook-consent prompt, the command palette, the model picker, the stash
// panel) and the backstop for the ones it does. The CLI draws its app at the
// bottom of the pane (P:hermes_cli/cli_tui_mixin.py:395-441), top to bottom:
// the overlays, the spinner, the hint line, the pet, the stash panel, the
// subagent dock, THE STATUS BAR, the upper input rule, the image bar, THE INPUT
// AREA, the lower input rule, the voice bar, the completions menu.
//
// 🔴 EVERY RULE HERE REFUSES ON DOUBT. A screen this does not recognise is
// refused, never guessed: a false refusal costs a person typing in the
// terminal, a false admit can approve a command (S11).
//
// What it checks, on the captured pane (CheckIdle):
//
//   - the input area's frame: a lower input rule as the last row drawn
//     (nothing below it: no voice bar, no completions menu), an upper rule of
//     the same width at most 9 rows above — the highest one with the real
//     status bar (its text and its background) directly above it — and that
//     status bar;
//   - 🔴 THE STATUS BAR IS REQUIRED, and must be Hermes's (its "☤ " model
//     marker). At the pin it renders NOTHING while the command palette or the
//     model picker is open (P:hermes_cli/cli_status_bar_mixin.py:1128-1133),
//     so those two overlays — the ones that leave the input line untouched —
//     are refused here, before any Escape. A status bar Hermes hides (after a
//     resize, until the next key; with display.statusbar off) is refused too;
//   - the stash panel: its status-bar mark, "📌 N ▲" (P:hermes_cli/prompt_stash.py:68-71),
//     or its header row above the bar (the mark is clipped in a titled session);
//   - a vi-mode label in the status bar, in any bundled language (ViLabels):
//     defence in depth only, see the design;
//   - the first input row: the idle prompt, `❯ ` optionally after a profile
//     name, then only the italic placeholder. Every other state Hermes draws
//     changes this prefix (⚠ approval and confirm, ? and ✎ clarify, 🔐 sudo,
//     🔑 secret, ⚙ connection, ☤ busy, ●/◉/🎤 voice, a spinner frame for a
//     slow command), and the compact chrome of a narrow pane drops the `❯`;
//   - an image bar between the upper rule and the prompt row (images
//     attached: Enter would send them too);
//   - every further input row empty. "Empty" is by STYLE: the placeholder is
//     italic, so any non-italic, non-space character is text in the box.
//
// It reads no word Hermes localises except the vi label: the prefixes are
// the skin's glyphs, the placeholder is matched by its italic style.

// Cell is one character of a captured screen, whether it is italic, and
// whether it has a background colour (Bg). A draft typed in Hermes's input
// box is drawn in the input's style, which has none at the pin (and an escape
// sequence typed or pasted into the box is drawn as text, `^[`, never
// obeyed), so a background is something a draft cannot draw.
type Cell struct {
	R      rune
	Italic bool
	Bg     bool
}

// Screen is a pane as `tmux capture-pane -p -e` prints it: its rows, top to
// bottom, as cells.
type Screen struct {
	Rows [][]Cell
}

// ParseScreen reads a `capture-pane -p -e` capture. Of SGR only italic (3
// on; 23, 0 or an empty SGR off) and whether a background colour is set (40-47,
// 100-107, 48;…; 49, 0 or an empty SGR off) are kept; every other escape
// sequence is dropped. A sequence it cannot read ends at the next letter,
// never later.
func ParseScreen(capture string) Screen {
	var s Screen
	// 🔴 THE STYLE IS CARRIED ACROSS ROWS: tmux's capture states an
	// attribute where it CHANGES (cmd-capture-pane.c keeps its last cell
	// across lines), so a row that continues an italic run starts with no
	// escape at all.
	var st style
	for _, line := range strings.Split(strings.TrimRight(capture, "\n"), "\n") {
		var row []Cell
		row, st = parseRow(line, st)
		s.Rows = append(s.Rows, row)
	}
	return s
}

// style is the SGR state ParseScreen keeps.
type style struct{ italic, bg bool }

// parseRow is one captured line's cells, starting with st and returning the
// style it ends with.
func parseRow(line string, st style) ([]Cell, style) {
	var row []Cell
	rs := []rune(strings.TrimRight(line, "\r"))
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if r != 0x1b {
			if r == '\t' {
				r = ' '
			}
			row = append(row, Cell{R: r, Italic: st.italic, Bg: st.bg})
			continue
		}
		if i+1 >= len(rs) {
			break
		}
		switch rs[i+1] {
		case '[':
			j := i + 2
			for j < len(rs) && !(rs[j] >= 0x40 && rs[j] <= 0x7e) {
				j++
			}
			if j < len(rs) && rs[j] == 'm' {
				st = sgrStyle(string(rs[i+2:j]), st)
			}
			i = j
		case ']':
			// OSC (a hyperlink): to BEL or ST.
			j := i + 2
			for j < len(rs) && rs[j] != 0x07 && !(rs[j] == 0x1b && j+1 < len(rs) && rs[j+1] == '\\') {
				j++
			}
			if j < len(rs) && rs[j] == 0x1b {
				j++
			}
			i = j
		default:
			i++
		}
	}
	return row, st
}

// sgrStyle applies one SGR's parameters to st.
func sgrStyle(params string, st style) style {
	if params == "" {
		return style{}
	}
	ps := strings.Split(params, ";")
	for k := 0; k < len(ps); k++ {
		n, err := strconv.Atoi(ps[k])
		if err != nil {
			continue
		}
		switch {
		case n == 0:
			st = style{}
		case n == 23:
			st.italic = false
		case n == 3:
			st.italic = true
		case n == 49:
			st.bg = false
		case n >= 40 && n <= 47, n >= 100 && n <= 107:
			st.bg = true
		case n == 38, n == 48, n == 58:
			if n == 48 {
				st.bg = true
			}
			// An extended colour: skip its arguments (5;n or 2;r;g;b).
			if k+1 < len(ps) {
				if ps[k+1] == "5" {
					k += 2
				} else if ps[k+1] == "2" {
					k += 4
				}
			}
		}
	}
	return st
}

// rowText is a row's characters with trailing white space trimmed — EVERY
// Unicode space (NBSP, U+3000, …), not just ASCII: a typed `❯ ` row ended by
// an NBSP must not keep its prefix where a plain space would be trimmed.
func rowText(row []Cell) string {
	var b strings.Builder
	for _, c := range row {
		b.WriteRune(c.R)
	}
	return strings.TrimRightFunc(b.String(), unicode.IsSpace)
}

// ScreenRefusal is why the screen check refused: Rule is one of the
// Screen* constants, the person's words.
type ScreenRefusal struct{ Rule string }

func (e *ScreenRefusal) Error() string { return e.Rule }

// The screen check's refusals, as a person reads them (after "Not sent: ").
const (
	ScreenNoLayout    = "the pane does not show Hermes's input area as muster knows it"
	ScreenBelow       = "something is drawn below Hermes's input area (a completions menu or the voice bar), or its lower rule is missing (a narrow pane's compact chrome)"
	ScreenNoStatusBar = "Hermes's status bar is blank or hidden: it is blank while the command palette or the model picker is open (and hidden after a resize, until a key is pressed, or when it is turned off)"
	ScreenStash       = "Hermes's prompt-stash panel is open"
	ScreenVi          = "Hermes's status bar shows a vi-mode label, and muster never sends to Hermes in vi mode"
	ScreenImages      = "images are attached in Hermes's input box, and Enter would send them with the message"
	ScreenNotIdle     = "Hermes's input line is not its idle prompt (a question, an approval, a password prompt or a turn is on screen)"
	ScreenText        = "Hermes's input box already holds text"
	ScreenNotPasted   = "Hermes's input box does not hold exactly the message after the paste"
)

// IsScreenRefusal reports whether err is a refusal of the screen check, and
// which.
func IsScreenRefusal(err error) (string, bool) {
	var sr *ScreenRefusal
	if errors.As(err, &sr) {
		return sr.Rule, true
	}
	return "", false
}

func refuse(rule string) error { return &ScreenRefusal{Rule: rule} }

// minRuleWidth is the narrowest input rule recognised. Below 64 columns
// Hermes uses its compact chrome, which hides the lower rule
// (P:hermes_cli/cli_tui_mixin.py:135-143; cli_status_bar_mixin.py:513-517), so
// a narrower pane never has the frame anyway.
const minRuleWidth = 20

// maxInputRows is the most rows the input area takes (its TextArea's
// Dimension max, P:hermes_cli/cli_tui_mixin.py:2220), plus one for an image
// bar.
const maxInputRows = 8 + 1

// isRule reports whether row is an input rule: '─' from the first column to
// the last character drawn, at least minRuleWidth wide; its width.
func isRule(row []Cell) (int, bool) {
	t := rowText(row)
	n := 0
	for _, r := range t {
		if r != '─' {
			return 0, false
		}
		n++
	}
	return n, n >= minRuleWidth
}

func blank(row []Cell) bool { return rowText(row) == "" }

// frame is where the input area is on s.
type frame struct {
	status, top, bot int
	width            int
}

// frameOf finds the input area's frame on s, or the refusal.
func frameOf(s Screen) (frame, error) {
	last := len(s.Rows) - 1
	for last >= 0 && blank(s.Rows[last]) {
		last--
	}
	if last < 0 {
		return frame{}, refuse(ScreenNoLayout)
	}
	bot := -1
	for i := last; i >= 0; i-- {
		if _, ok := isRule(s.Rows[i]); ok {
			bot = i
			break
		}
	}
	switch {
	case bot < 0:
		return frame{}, refuse(ScreenNoLayout)
	case bot != last:
		return frame{}, refuse(ScreenBelow)
	}
	w, _ := isRule(s.Rows[bot])
	// The upper rule is the HIGHEST same-width rule within the box's rows
	// whose row above is the REAL status bar (statusBarAt: its text AND its
	// background), and only when there is none the highest same-width rule.
	//
	// 🔴 Never the closest: a draft can type a full-width rule (and a status
	// bar's text and an idle prompt under it) on its continuation rows, and
	// the real upper rule is above every row a draft can draw. 🔴 Never just
	// the highest either: a reply whose last line is a rule as wide as the
	// pane lands within the box's rows ABOVE the status bar (the reply box's
	// `╰─╯` and the bar between them), and every send was refused until the
	// next turn. A draft cannot fake the anchor — it draws no background —
	// so with the bar drawn the real rule wins, and with it blank or hidden
	// (the palette, the picker, after a resize) no rule is anchored, the
	// highest is taken, and its status row is refused.
	top, anchored := -1, -1
	for i := bot - 1; i >= 0 && i >= bot-1-maxInputRows; i-- {
		if tw, ok := isRule(s.Rows[i]); ok && tw == w {
			top = i
			if statusBarAt(s, i-1) {
				anchored = i
			}
		}
	}
	if anchored >= 0 {
		top = anchored
	}
	if top < 0 || top == bot-1 || top == 0 {
		return frame{}, refuse(ScreenNoLayout)
	}
	return frame{status: top - 1, top: top, bot: bot, width: w}, nil
}

// statusBarRE is Hermes's status bar: its model segment " ☤ <model>" first,
// or after a battery label (P:hermes_cli/cli_status_bar_mixin.py:1036-1041,
// 1162-1166). The trimmed form of a narrow pane keeps it.
var statusBarRE = regexp.MustCompile(`^ (?:[^│]{1,24} │ )?☤ \S`)

// statusBarAt reports whether row i is Hermes's status bar: its text
// (statusBarRE) and its style — the bar is drawn on a background
// (class:status-bar), which no row of the input box has. A skin whose bar has
// no background is never anchored, so the frame falls back to the highest
// rule: a transcript rule in range is then refused, never admitted.
func statusBarAt(s Screen, i int) bool {
	if i < 0 || len(s.Rows[i]) == 0 || !s.Rows[i][0].Bg {
		return false
	}
	return statusBarRE.MatchString(rowText(s.Rows[i]))
}

// stashOpenRE is the stash indicator while its panel is open.
var stashOpenRE = regexp.MustCompile(`📌 \d+ ▲`)

// stashPanelRows is how far above the status bar the stash panel's header is
// looked for: the panel (its rows are the stash, which Hermes bounds) and
// the subagent dock, which sits between the panel and the bar.
const stashPanelRows = 40

// stashPanelOpen reports whether the stash panel's HEADER is drawn above the
// status bar at row status: a row `╭─ 📌 …` (every bundled language, and the
// compact form, starts so: P:hermes_cli/cli_status_bar_mixin.py:1201, :1209).
//
// 🔴 THE PANEL, NOT THE STATUS-BAR MARK: once the session has a title,
// Hermes right-aligns the title badge and CLIPS the bar's left side to make
// room (_right_align_status_title_fragments), and the `📌 N ▲` mark is the
// first thing cut — measured: clipped at 80, 100 and 120 columns.
//
// 🔴 THE HEADER ALONE, NEVER ITS FOOTER TOO: the panel is an unbounded window
// drawn top-down (P:hermes_cli/cli_tui_mixin.py:2181-2184), so on a pane too
// short for it the rows that do not fit are cut from its BOTTOM — the footer
// first; measured in titled sessions with the footer gone at 100x12 (7
// drafts), 100x24 and 80x24 (20, Hermes's cap) — while the header stays on
// the panel's first row. (Requiring the footer added nothing besides: a
// reply box's own `╰─╯` matches it.) A reply line that starts `╭─ 📌` is
// refused too: a false refusal, the safe way.
func stashPanelOpen(s Screen, status int) bool {
	for i := status - 1; i >= 0 && i >= status-stashPanelRows; i-- {
		t := strings.TrimLeftFunc(rowText(s.Rows[i]), unicode.IsSpace)
		if strings.HasPrefix(t, "╭─ ") && strings.Contains(t, "📌") {
			return true
		}
	}
	return false
}

// idlePromptRE is the idle prompt at the start of the first input row: the
// skin's default `❯ `, after a profile name when the profile is not the
// default (P:hermes_cli/cli_tui_mixin.py:272-288; profile names,
// P:hermes_cli/profiles.py:340-343).
var idlePromptRE = regexp.MustCompile(`^(?:[a-z0-9][a-z0-9_-]{0,63} )?❯ `)

// statusChecks are the rules on the status bar row.
func statusChecks(s Screen, f frame) error {
	sb := rowText(s.Rows[f.status])
	if !statusBarRE.MatchString(sb) {
		return refuse(ScreenNoStatusBar)
	}
	if stashOpenRE.MatchString(sb) || stashPanelOpen(s, f.status) {
		return refuse(ScreenStash)
	}
	if HasViLabel(sb) {
		return refuse(ScreenVi)
	}
	return nil
}

// promptRow is the input area's first row, after the image-bar check, and
// the cells after its prompt.
func promptRow(s Screen, f frame) ([]Cell, error) {
	first := s.Rows[f.top+1]
	loc := idlePromptRE.FindStringIndex(rowText(first))
	if loc == nil {
		for i := f.top + 2; i < f.bot; i++ {
			if idlePromptRE.MatchString(rowText(s.Rows[i])) {
				return nil, refuse(ScreenImages)
			}
		}
		return nil, refuse(ScreenNotIdle)
	}
	n := len([]rune(rowText(first)[:loc[1]]))
	return first[n:], nil
}

// empty reports whether cells hold nothing typed: spaces and italic
// (placeholder) characters only.
func empty(cells []Cell) bool {
	for _, c := range cells {
		if !c.Italic && !unicode.IsSpace(c.R) {
			return false
		}
	}
	return true
}

// CheckIdle is the screen check before a paste: nil when s shows Hermes's
// idle, empty input line in its full frame, else a *ScreenRefusal.
func CheckIdle(s Screen) error {
	f, err := frameOf(s)
	if err != nil {
		return err
	}
	if err := statusChecks(s, f); err != nil {
		return err
	}
	rest, err := promptRow(s, f)
	if err != nil {
		return err
	}
	if !empty(rest) {
		return refuse(ScreenText)
	}
	for i := f.top + 2; i < f.bot; i++ {
		if !empty(s.Rows[i]) {
			return refuse(ScreenText)
		}
	}
	return nil
}

// placeholderRE is a collapsed paste's placeholder in the input box, with
// every space removed: "[Pasted text #N: L lines → <file>]"
// (P:hermes_cli/cli_tui_mixin.py:1677-1689). It is not localised.
var placeholderRE = regexp.MustCompile(`^\[Pastedtext#\d+:(\d+)lines→[^\]]+\]$`)

// CheckPasted is the screen check after the paste, before Enter: s must
// show the same frame and status bar as CheckIdle, the idle prompt, and in
// the input box EXACTLY text — compared with every space removed, since the
// box wraps it — or exactly one collapse placeholder whose line count is
// text's. Anything else is a *ScreenRefusal (ScreenNotPasted when only the
// box's contents are wrong).
func CheckPasted(s Screen, text string) error {
	f, err := frameOf(s)
	if err != nil {
		return err
	}
	if err := statusChecks(s, f); err != nil {
		return err
	}
	rest, err := promptRow(s, f)
	if err != nil {
		return err
	}
	var got strings.Builder
	for _, c := range rest {
		if !unicode.IsSpace(c.R) {
			got.WriteRune(c.R)
		}
	}
	for i := f.top + 2; i < f.bot; i++ {
		for _, c := range s.Rows[i] {
			if !unicode.IsSpace(c.R) {
				got.WriteRune(c.R)
			}
		}
	}
	g := got.String()
	if g == squeeze(text) && g != "" {
		return nil
	}
	if m := placeholderRE.FindStringSubmatch(g); m != nil && m[1] == strconv.Itoa(strings.Count(text, "\n")+1) {
		return nil
	}
	return refuse(ScreenNotPasted)
}

// squeeze is s with every space removed.
func squeeze(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// InputWidth is the input area's width on s (its rules'), 0 when s has no
// frame.
func InputWidth(s Screen) int {
	f, err := frameOf(s)
	if err != nil {
		return 0
	}
	return f.width
}

// Collapses reports whether Hermes, at its default thresholds, replaces a
// bracketed paste of text with a placeholder: 5 or more newlines, or 2,000 or
// more characters (P:hermes_cli/cli_tui_mixin.py:1691-1696, :1724-1727;
// paste_collapse_threshold counts NEWLINES, so it is 6 lines, not 5).
func Collapses(text string) bool {
	return strings.Count(text, "\n") >= 5 || len([]rune(text)) >= 2000
}

// InputRows is how many rows Hermes's input area needs to show text whole
// after an idle prompt promptWidth cells wide, in a pane columns wide:
// Hermes's own estimate (P:hermes_cli/cli_terminal_input.py:548-569) without
// its cap of 8. A text needing more than 8 is scrolled in the box, its prompt
// row out of sight, so the post-paste check could not see it whole.
func InputRows(text string, promptWidth, columns int) int {
	if columns < 1 {
		columns = 1
	}
	rows := 0
	for i, line := range strings.Split(text, "\n") {
		w := cellWidth(line)
		if i == 0 {
			w += promptWidth
		}
		rows += max(1, (w+columns-1)/columns)
	}
	return rows
}

// MaxInputRows is the most rows Hermes's input area shows (its Dimension max).
const MaxInputRows = 8

// cellWidth is s's width in terminal cells, as prompt_toolkit's get_cwidth
// counts it closely enough to bound it from above: East Asian wide and
// fullwidth runes and emoji are 2, combining marks 0.
func cellWidth(s string) int {
	n := 0
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r):
		case r >= 0x1F000:
			n += 2
		default:
			switch width.LookupRune(r).Kind() {
			case width.EastAsianWide, width.EastAsianFullwidth:
				n += 2
			default:
				n++
			}
		}
	}
	return n
}

// PromptWidth is the idle prompt's width on s's first input row (`❯ ` is 2,
// `coder ❯ ` 8), 0 when s has none.
func PromptWidth(s Screen) int {
	f, err := frameOf(s)
	if err != nil {
		return 0
	}
	loc := idlePromptRE.FindStringIndex(rowText(s.Rows[f.top+1]))
	if loc == nil {
		return 0
	}
	return cellWidth(rowText(s.Rows[f.top+1])[:loc[1]])
}

// InputRow is the text of the input area's first row on s, "" when s has no
// frame: a diagnostic, never a gate.
func InputRow(s Screen) string {
	f, err := frameOf(s)
	if err != nil {
		return ""
	}
	return rowText(s.Rows[f.top+1])
}

// LooksLikeHermes is the paste guard's best-effort chrome check: s shows an
// input area framed by two rules of one width with a `❯` on its first row,
// AND Hermes's own ☤ — its status bar above the frame, or the busy prompt.
// 🔴 The ☤ is what keeps it off Claude Code, whose idle screen is also a `❯`
// between two rules (internal/askdrive's fixtures; TestLooksLikeHermes).
// ⚠ It FAILS OPEN when Hermes's chrome is not drawn (for 0.35 s after a
// resize; a narrow pane's compact chrome; the status bar turned off) — the
// process walk is the guard; this is an extra (docs/design/hermes.md, the
// paste guard).
func LooksLikeHermes(s Screen) bool {
	f, err := frameOf(s)
	if err != nil {
		return false
	}
	in := rowText(s.Rows[f.top+1])
	if !strings.ContainsRune(in, '❯') {
		return false
	}
	return statusBarRE.MatchString(rowText(s.Rows[f.status])) || strings.Contains(in, "☤ ❯")
}
