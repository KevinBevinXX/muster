// Package askdrive types an answer into Claude Code's own AskUserQuestion
// picker, the way a person would — one key at a time, READING THE PANE before
// and after every key — so a question asked in a mirrored session can be
// answered from muster while the terminal shows its picker too.
//
// It is the host half of "answered in both places": the picker is on screen at
// once (no hook holds it), muster shows the same question, and whichever is
// answered first wins. So nothing here may type into a picker that is not the
// one it expects, in the state it expects:
//
//   - before the first key it requires the picker UNTOUCHED (the first question,
//     nothing chosen, the cursor on the first row) and every question's text and
//     options to match what muster was asked;
//   - before EVERY key it re-reads the pane and requires the state the previous
//     key was meant to produce — a person answering in the terminal meanwhile
//     closes or changes the picker, and the driver stops without a key. It
//     reads it TWICE, Options.Steady apart, and requires the same screen both
//     times: a key someone typed that the CLI has not drawn yet shows up in the
//     second look (Claude Code's redraw lag is measured below);
//   - right before every key, when the pane can tell (Activity), it reads
//     when the terminal last had input (muster's own pasted keys do not
//     count); if a key from then might not be drawn by its second look, it
//     waits until it would be and looks again. Input that changed nothing —
//     a focus event, the mouse — does not stop it; a key that changed the
//     picker does;
//   - after every key it waits for the screen that key must produce; anything
//     else stops it.
//
// It never sends the final key (the submit) unless the screen shows exactly the
// answers it was asked to give — the review screen when there is one — so a
// stopped drive leaves at most an unsubmitted, partly filled picker, never a
// half answer.
//
// The picker's behaviour is MEASURED, Claude Code 2.1.289 (the PR's gate):
//
//   - redraw lag, key pasted to screen changed (capture-pane every 2 ms, an
//     80x23 pane): idle, median 8–9 ms and at most 19 ms over 55 keys; with
//     every core of a 24-core host busy, median 14 ms and at most 72 ms over 40
//     keys. Options.Steady (250 ms) is over three times the worst of those;
//
//   - single-select: a digit chooses its option; it answers the question (and
//     submits a one-question ask, which has no review screen);
//
//   - multi-select: a digit toggles its option and leaves the cursor where it
//     was; the rows are the options, the "Type something" row, then "Next"
//     (or "Submit" on a one-question ask), reached with Down and taken with
//     Enter;
//
//   - the "Type something" row (option k+1): on a single-select question its
//     digit focuses it for typing; on a multi-select question Down moves onto
//     it; typed text is the answer (and checks it, multi-select); in that row
//     digits and letters are TEXT;
//
//   - an ask with several questions (or a multi-select one) ends on a review
//     screen listing every "question → answer"; "1" submits it;
//
//   - LONG TEXT (measured 2.1.289, synthetic text at 80, 100 and 120
//     columns: testdata/long-*): a question too long for a line is wrapped
//     inside a box, "│ " down its left edge (on the review screen " │ ● …");
//     a long label, and a long answer typed into the "Type something" row,
//     wrap onto the row's further lines at its label's column — the answer
//     is never truncated or scrolled within the row (650+ characters drawn in
//     full), and a word longer than a line breaks mid-word; a picker taller
//     than the pane loses its TOP (header and question) off the screen.
//     Parse takes the box off and keeps each text's lines; Match reads them
//     back together (wrapEq), free only at the line breaks.
package askdrive

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Kind is what the pane shows.
type Kind int

const (
	// None: no picker (it was answered, cancelled, or was never there).
	None Kind = iota
	// Question: one question's options.
	Question
	// Review: the review screen of a multi-question (or multi-select) ask.
	Review
	// Unknown: something that looks like the picker (its footer's words, or
	// the review's Submit row) that does not parse as either screen — the
	// CLI's layout has probably changed (ErrUnrecognised).
	Unknown
	// Clipped: the BOTTOM of a picker — its footer (or the review's Submit
	// and Cancel rows) under numbered rows — whose header is not on screen: a
	// picker taller than the pane, its top scrolled off (measured 2.1.289: a
	// long typed answer in an 80x20 pane). Which question it is, and what the
	// rows above say, cannot be read, so nothing may be typed into it; it is
	// never what a key should produce (ErrUnreadable).
	Clipped
)

// Row is one numbered row of a question screen.
type Row struct {
	Num int
	// Label is the row's text: an option's label, or the "Type something" row's
	// placeholder or typed text.
	Label string
	// Box is "" for a single-select row, else "[ ]" / "[✔]".
	Box    string
	Cursor bool
	// More is the row's further lines, trimmed, in order: the rest of a label
	// (or of typed text) the CLI wrapped, then the option's description. The
	// screen does not say where the one ends and the other begins; Match
	// decides it against the label it expects (labelMatch). The "Type
	// something" row has no description, so all of its More is typed text.
	More []string
}

// Tab is one question's tab in a multi-question header.
type Tab struct {
	Header   string
	Answered bool
}

// ReviewItem is one "question → answer" of the review screen: each is its
// screen lines joined by "\n" (a long one is wrapped, and its question boxed —
// see Screen.Question), so where the CLI wrapped it is kept.
type ReviewItem struct {
	Question string
	Answer   string
}

// Screen is the picker as the pane shows it.
type Screen struct {
	Kind Kind
	// Tabs are the question tabs (the header line), in order; Current is the
	// index of the one shown (Question), -1 when no tab is marked current.
	Tabs []Tab
	// Question is the shown question's text: its screen lines, the box border
	// and its padding stripped, joined by "\n" — the CLI wraps a long question
	// and draws it inside a box ("│ " down its left edge, measured 2.1.289),
	// and where it wrapped is kept so Match can tell a wrap from the text
	// (wrapEq).
	Question string
	// Rows are the numbered rows above the "Chat about this" separator.
	Rows []Row
	// Nav is the "Next"/"Submit" row's text ("" when there is none), and
	// NavCursor whether the cursor is on it.
	Nav       string
	NavCursor bool
	// Review is the review screen's items, and ReviewCursor the numbered row
	// the cursor is on there (1 = Submit answers).
	Review       []ReviewItem
	ReviewCursor int
}

var (
	// "←  ☐ Color  ☒ Pets  ✔ Submit  →" or, one single-select question, " ☐ Color".
	tabsRE = regexp.MustCompile(`^\s*(?:←\s+)?((?:[☐☒] .+?\s{2,})*[☐☒] .+?)(?:\s+✔ Submit\s+→)?\s*$`)
	tabRE  = regexp.MustCompile(`([☐☒]) (.+?)(?:\s{2,}|$)`)
	rowRE  = regexp.MustCompile(`^(❯| ) (\d+)\. (?:\[(✔| )\] )?(.*?)\s*$`)
	navRE  = regexp.MustCompile(`^(❯| ) {4}(Next|Submit)\s*$`)
	sepRE  = regexp.MustCompile(`^─{10,}\s*$`)
	// A review item's question, boxed when long (" │ ● …", its further lines
	// " │   …"); its answer, wrapped when long ("   → …", then "   …").
	reviewQ    = regexp.MustCompile(`^ (?:[│┃] )?● (.*?)\s*$`)
	reviewA    = regexp.MustCompile(`^ (?:[│┃] )?  → (.*?)\s*$`)
	reviewMore = regexp.MustCompile(`^ (?:[│┃] )?  (\S.*?)\s*$`)
	chatRE     = regexp.MustCompile(`^  \d+\. Chat about this\s*$`)
	footerRE   = regexp.MustCompile(`^Enter to select · `)
)

func trimRight(s string) string { return strings.TrimRight(s, " \t") }

// Parse reads the picker from a pane capture (tmux capture-pane -p -J). The
// picker is the bottom of the screen: its header line is the LAST tabs line
// above a footer ("Enter to select · …") or a review screen's Submit row.
// Anything that does not parse as one of the two screens is None.
func Parse(capture string) Screen {
	s := parse(capture)
	if s.Kind == None && looksLikeAPicker(capture) {
		s.Kind = Unknown
	}
	return s
}

// looksLikeAPicker reports whether capture carries the AskUserQuestion
// picker's STRUCTURE, not merely its words: its question tabs' ☐/☒ boxes AND,
// as one of the last three lines, a line that BEGINS with its footer's
// "Enter to select"; or its review screen (a "Review your answers"
// line and a "Submit answers" row). A permission prompt ends "Esc to cancel ·
// …" and prose can quote "Enter to select" (or draw ☐ boxes) — neither is a
// picker muster failed to read.
func looksLikeAPicker(capture string) bool {
	lines := strings.Split(strings.TrimRight(strings.ReplaceAll(capture, "\r", ""), "\n "), "\n")
	tabs, footer, review, submit := false, false, false, false
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.ContainsAny(t, "☐☒") {
			tabs = true
		}
		if i >= len(lines)-3 && strings.HasPrefix(t, "Enter to select") {
			footer = true
		}
		if t == "Review your answers" {
			review = true
		}
		if strings.HasSuffix(t, "Submit answers") {
			submit = true
		}
	}
	return (tabs && footer) || (review && submit)
}

func parse(capture string) Screen {
	lines := strings.Split(strings.ReplaceAll(capture, "\r", ""), "\n")
	for i := range lines {
		lines[i] = trimRight(lines[i])
	}
	// The bottom of the screen: the last non-empty line.
	last := len(lines) - 1
	for last >= 0 && lines[last] == "" {
		last--
	}
	if last < 0 {
		return Screen{}
	}
	// A question screen ends with its footer; a review screen with
	// "2. Cancel" under "1. Submit answers".
	review := false
	// The footer may wrap (the CLI wraps it itself on a narrow pane: "… Esc
	// to" / "cancel"), so it is looked for in the last three lines.
	footer := -1
	for i := last; i >= 0 && i >= last-2; i-- {
		if footerRE.MatchString(lines[i]) {
			footer = i
			break
		}
	}
	switch {
	case footer >= 0:
		last = footer
	case last >= 1 && strings.TrimSpace(lines[last]) == "2. Cancel" && strings.HasSuffix(strings.TrimSpace(lines[last-1]), "1. Submit answers"):
		review = true
	default:
		return Screen{}
	}
	// The header: the last tabs line above the bottom.
	head := -1
	for i := last; i >= 0; i-- {
		if strings.ContainsAny(lines[i], "☐☒") && tabsRE.MatchString(lines[i]) {
			head = i
			break
		}
	}
	if head < 0 {
		if clipped(lines[:last+1], review) {
			return Screen{Kind: Clipped}
		}
		return Screen{}
	}
	var s Screen
	for _, m := range tabRE.FindAllStringSubmatch(tabsRE.FindStringSubmatch(lines[head])[1], -1) {
		s.Tabs = append(s.Tabs, Tab{Header: strings.TrimSpace(m[2]), Answered: m[1] == "☒"})
	}
	body := lines[head+1 : last+1]
	if review {
		return parseReview(s, body)
	}
	s.Kind = Question
	i := 0
	var q []string
	for ; i < len(body); i++ {
		if rowRE.MatchString(body[i]) {
			break
		}
		if sepRE.MatchString(body[i]) {
			return Screen{} // no option rows above the separator
		}
		if strings.TrimSpace(body[i]) != "" {
			q = append(q, body[i])
		}
	}
	s.Question = strings.Join(unbox(q), "\n")
	for ; i < len(body); i++ {
		l := body[i]
		if sepRE.MatchString(l) {
			break // "Chat about this" and the footer are below it
		}
		if m := navRE.FindStringSubmatch(l); m != nil {
			s.Nav, s.NavCursor = m[2], m[1] == "❯"
			continue
		}
		m := rowRE.FindStringSubmatch(l)
		if m == nil {
			// A row's further line: its wrapped label or typed text, or its
			// description.
			if t := strings.TrimSpace(l); t != "" && len(s.Rows) > 0 {
				r := &s.Rows[len(s.Rows)-1]
				r.More = append(r.More, t)
			}
			continue
		}
		n, _ := strconv.Atoi(m[2])
		r := Row{Num: n, Label: m[4], Cursor: m[1] == "❯"}
		if m[3] != "" {
			r.Box = "[" + m[3] + "]"
		}
		s.Rows = append(s.Rows, r)
	}
	if len(s.Rows) == 0 {
		return Screen{}
	}
	return s
}

// clipped reports whether lines — a screen down to its bottom, a question's
// footer or (review) the Submit/Cancel rows, with no header above — are the
// bottom of a picker whose top is off the pane: a question's numbered rows
// above its separator and "Chat about this" row, or a review's Submit row.
func clipped(lines []string, review bool) bool {
	if review {
		return true // "1. Submit answers" over "2. Cancel" at the bottom
	}
	for i := len(lines) - 1; i >= 1; i-- {
		if chatRE.MatchString(lines[i]) && sepRE.MatchString(lines[i-1]) {
			for j := i - 2; j >= 0; j-- {
				if rowRE.MatchString(lines[j]) {
					return true
				}
			}
			return false
		}
	}
	return false
}

// vbars are the box-drawing glyphs that can draw a box's left or right edge.
const vbars = "│┃║╎╏┆┇┊┋"

// hborderRE is a box's top or bottom edge.
var hborderRE = regexp.MustCompile(`^\s*[╭┌┏╔╰└┗╚][─━═]*[╮┐┓╗╯┘┛╝]\s*$`)

// unbox is lines (a question's screen lines) with the box the CLI draws round
// a long one taken off: a top or bottom edge, and a left (right) edge glyph
// with the padding beside it when EVERY line has one — a text whose own
// first character happens to be "│" is drawn inside the box and keeps it.
// Each line comes back trimmed.
func unbox(lines []string) []string {
	var ls []string
	for _, l := range lines {
		if !hborderRE.MatchString(l) {
			ls = append(ls, strings.TrimSpace(l))
		}
	}
	all := func(f func(string) bool) bool {
		for _, l := range ls {
			if !f(l) {
				return false
			}
		}
		return len(ls) > 0
	}
	isBar := func(r rune) bool { return strings.ContainsRune(vbars, r) }
	if all(func(l string) bool { r, _ := utf8.DecodeRuneInString(l); return isBar(r) }) {
		for i, l := range ls {
			_, n := utf8.DecodeRuneInString(l)
			ls[i] = strings.TrimSpace(l[n:])
		}
	}
	if all(func(l string) bool { r, _ := utf8.DecodeLastRuneInString(l); return isBar(r) }) {
		for i, l := range ls {
			_, n := utf8.DecodeLastRuneInString(l)
			ls[i] = strings.TrimSpace(l[:len(l)-n])
		}
	}
	return ls
}

func parseReview(s Screen, body []string) Screen {
	s.Kind = Review
	seen := false
	// more is where a further line of the item above goes: its question, its
	// answer, or nowhere (a blank line or anything else ends an item).
	var more *string
	for i := 0; i < len(body); i++ {
		l := body[i]
		if strings.TrimSpace(l) == "Review your answers" {
			seen = true
			more = nil
			continue
		}
		if m := reviewQ.FindStringSubmatch(l); m != nil {
			s.Review = append(s.Review, ReviewItem{Question: m[1]})
			more = &s.Review[len(s.Review)-1].Question
			continue
		}
		if m := reviewA.FindStringSubmatch(l); m != nil && len(s.Review) > 0 {
			s.Review[len(s.Review)-1].Answer = m[1]
			more = &s.Review[len(s.Review)-1].Answer
			continue
		}
		if m := rowRE.FindStringSubmatch(l); m != nil {
			if m[1] == "❯" {
				s.ReviewCursor, _ = strconv.Atoi(m[2])
			}
			more = nil
			continue
		}
		if m := reviewMore.FindStringSubmatch(l); m != nil && more != nil {
			*more += "\n" + m[1]
			continue
		}
		more = nil
	}
	if !seen {
		return Screen{}
	}
	return s
}

// norm is s with runs of white space as one space, trimmed: how question text
// and labels are compared (the picker wraps a long question over lines).
func norm(s string) string { return strings.Join(strings.Fields(s), " ") }

// wrapEq reports whether lines — a text as the picker drew it, one screen line
// each (the box already off) — are want, wrapped. ONLY the line breaks are
// free: each stands for the white space the CLI wrapped at, or for none at
// all (a word too long for its line broken mid-word or after a "/", measured
// 2.1.289: "…hyphens-ev" / "erywhere-…", ".../of/the/" / "terminal/…").
// Every character on a line must be want's, in order, and every character of
// want must be on a line: an extra or a different character anywhere — a
// person typing into the row — is a mismatch. A break may be read as "no
// space" only because nothing a person types can put one there: Enter takes
// the row, it does not break it.
//
// exact compares the text as typed (the "Type something" row); otherwise both
// sides' runs of white space count as one space (norm), as question text and
// labels always have been.
func wrapEq(lines []string, want string, exact bool) bool {
	if !exact {
		want = norm(want)
	}
	pos, first := 0, true
	for _, l := range lines {
		if exact {
			if !first {
				l = strings.TrimLeft(l, " ")
			}
		} else {
			l = norm(l)
		}
		if l == "" {
			continue
		}
		if !first {
			for pos < len(want) && want[pos] == ' ' {
				pos++
			}
		}
		if !strings.HasPrefix(want[pos:], l) {
			return false
		}
		pos += len(l)
		first = false
	}
	return pos == len(want)
}

// labelMatch reports whether row r's label is want: its first line and as
// many of its further lines as the CLI wrapped the label over (the rest are
// its description). The screen cannot say where a wrapped label ends and its
// description begins, so a label "Red" over a description "Warm" also reads
// as a label "Red Warm" — accepted: it picks the same row with the same key,
// and an ask that differs from the registered one only so must also match
// its question, headers and every other row. Descriptions are not compared
// (they never were).
func labelMatch(r Row, want string) bool {
	lines := append([]string{r.Label}, r.More...)
	for n := 1; n <= len(lines); n++ {
		if wrapEq(lines[:n], want, false) {
			return true
		}
	}
	return false
}

// textLines is the "Type something" row's text as screen lines: every line of
// it (that row has no description).
func textLines(r Row) []string { return append([]string{r.Label}, r.More...) }

// splitLines is a "\n"-joined Screen text as its screen lines.
func splitLines(s string) []string { return strings.Split(s, "\n") }
