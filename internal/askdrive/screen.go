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
//     options to match what muster was asked — or, continuing an answer an
//     earlier drive of muster's stopped part way through (DriveFrom), exactly
//     the screen that drive's own keys left;
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
//     Parse takes the box off and keeps each text's lines, and where each
//     ends on screen; Match reads them back together (wrapEq): a line break
//     stands for a space, or — only after a line that fills the pane, where
//     the CLI breaks a word — for a space or none.
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
	// Ends is, for the Label line and then each More line, the screen column
	// its text ends in: whether the line is FULL (fullLines), the one place
	// the CLI breaks a word.
	Ends []int
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
	// QuestionEnds and AnswerEnds are, per line of each, the screen column
	// its text ends in (Row.Ends).
	QuestionEnds []int
	AnswerEnds   []int
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
	// QuestionEnds is, per line of Question, the screen column its text ends
	// in (Row.Ends).
	QuestionEnds []int
	// Width is the pane's width in columns: the picker's rule, a line of "─"
	// drawn across the whole pane (the widest one on the capture). 0 when
	// there is none, and then no line is full (fullLines).
	Width int
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
	// PreviewCol is, on a question drawn with previews (the package doc), the
	// screen column the preview box starts in: every row line was cut there.
	// 0: no preview box.
	PreviewCol int
	// Notes is a preview question's notes line, as drawn under the box (from
	// PreviewCol, trimmed; "" when there is none): NotesUntouched when nobody
	// has added a note. NoteEditing says the footer offers "ctrl+g to edit":
	// the notes line is being typed into (measured 2.1.289: n opens it, keys —
	// digits too — then go INTO the note, and Escape leaves it, the note
	// kept).
	Notes       string
	NoteEditing bool
}

// NotesUntouched is a preview question's notes line while it has no note.
const NotesUntouched = "Notes: press n to add notes"

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
	editing := false
	switch {
	case footer >= 0:
		editing = strings.Contains(strings.Join(lines[footer:min(len(lines), footer+3)], " "), "ctrl+g to edit")
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
	s.Width = ruleWidth(lines)
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
	qtext, qends := unbox(q)
	s.Question, s.QuestionEnds = strings.Join(qtext, "\n"), qends
	if col := previewCol(body[i:]); col > 0 {
		s.PreviewCol, s.NoteEditing = col, editing
		for j := i; j < len(body) && !sepRE.MatchString(body[j]); j++ {
			if r := []rune(body[j]); len(r) > col {
				// The notes line is the box's column's, not a row's: kept
				// before the cut, so a note someone saved is not cut away.
				if right := strings.TrimSpace(string(r[col:])); strings.HasPrefix(right, "Notes:") {
					s.Notes = right
				}
				body[j] = trimRight(string(r[:col]))
			}
		}
	}
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
				r.Ends = append(r.Ends, cols(l))
			}
			continue
		}
		n, _ := strconv.Atoi(m[2])
		r := Row{Num: n, Label: m[4], Cursor: m[1] == "❯", Ends: []int{cols(l)}}
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

// previewTopRE is a preview box's top edge, from its first column to the
// end of the line.
var previewTopRE = regexp.MustCompile(`^┌─+┐\s*$`)

// previewCol is the screen column of the preview box drawn beside a
// question's rows (lines: the rows down to the separator, and below), 0 when
// there is none: the box's top edge on a numbered row's line, after at least
// two columns of white space (the CLI's gutter; a label is never cut).
func previewCol(lines []string) int {
	for _, l := range lines {
		if sepRE.MatchString(l) {
			return 0
		}
		if !rowRE.MatchString(l) {
			continue
		}
		at := strings.Index(l, "┌")
		if at < 0 {
			return 0 // the box's top is on the first row's line, or there is none
		}
		if !previewTopRE.MatchString(l[at:]) || !strings.HasSuffix(l[:at], "  ") {
			return 0
		}
		return utf8.RuneCountInString(l[:at])
	}
	return 0
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

// unbox is lines (a question's screen lines, right-trimmed) with the box the
// CLI draws round a long one taken off: a top or bottom edge, and a left
// (right) edge glyph with the padding beside it when EVERY line has one and
// the frame is really there — a left edge down two or more lines, or a left
// and a right edge (one line). A text whose own first character happens to
// be "│" is drawn inside the box and keeps it, and so does a one-line question
// that merely starts or ends with one. Each line comes back trimmed, with the
// screen column its text ends in (inside a right edge, when one is taken off).
func unbox(lines []string) ([]string, []int) {
	var ls []string
	var ends []int
	for _, l := range lines {
		if !hborderRE.MatchString(l) {
			ls = append(ls, strings.TrimSpace(l))
			ends = append(ends, cols(trimRight(l)))
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
	left := all(func(l string) bool { r, _ := utf8.DecodeRuneInString(l); return isBar(r) })
	right := all(func(l string) bool { r, _ := utf8.DecodeLastRuneInString(l); return isBar(r) })
	// 🔴 ONLY A FRAME THAT IS REALLY THERE: a left edge down two or more
	// lines (the CLI's box, measured), or both edges. One line with a bar at
	// one end — a short question's own text, which the CLI does not box — is
	// text, and so is a right edge with no left one.
	if !left || (len(ls) < 2 && !right) {
		return ls, ends
	}
	for i, l := range ls {
		_, n := utf8.DecodeRuneInString(l)
		ls[i] = strings.TrimSpace(l[n:])
	}
	if right {
		for i, l := range ls {
			_, n := utf8.DecodeLastRuneInString(l)
			t := strings.TrimSpace(l[:len(l)-n])
			// The edge and the padding before it are not text.
			ends[i] -= cols(l[len(strings.TrimRight(l[:len(l)-n], " \t")):])
			ls[i] = t
		}
	}
	return ls, ends
}

// cols is how many screen columns s takes: one per rune. A wide character
// (two columns) is counted as one, so a line holding one reads as shorter
// than it is — never as full when it is not (fullLines), only the reverse,
// which refuses a mid-word break muster could not be sure of.
func cols(s string) int { return utf8.RuneCountInString(s) }

// ruleWidth is the widest line of "─" in lines (right-trimmed): the pane's
// width, which the picker's rule spans. 0 when there is none.
func ruleWidth(lines []string) int {
	w := 0
	for _, l := range lines {
		if sepRE.MatchString(l) {
			w = max(w, cols(trimRight(l)))
		}
	}
	return w
}

// fullLines is, per line end (a screen column, as Row.Ends), whether that
// line is FULL: it ends in the pane's last or last-but-one column. Only a
// full line can end mid-word (measured 2.1.289: the text row's "…hyphens-ev"
// ends in column 79 of 80; the review's "…of/the/ter" in 80 of 80 and
// "…/at/all/be" in 119 of 120); a line that ends sooner was wrapped at white
// space. width 0 (unknown) makes no line full.
func fullLines(ends []int, width int) []bool {
	full := make([]bool, len(ends))
	for i, e := range ends {
		full[i] = width > 0 && e >= width-1
	}
	return full
}

func parseReview(s Screen, body []string) Screen {
	s.Kind = Review
	seen := false
	// more is where a further line of the item above goes: its question, its
	// answer, or nowhere (a blank line or anything else ends an item); ends
	// is where that line's end column goes.
	var more *string
	var ends *[]int
	for i := 0; i < len(body); i++ {
		l := body[i]
		if strings.TrimSpace(l) == "Review your answers" {
			seen = true
			more = nil
			continue
		}
		if m := reviewQ.FindStringSubmatch(l); m != nil {
			s.Review = append(s.Review, ReviewItem{Question: m[1], QuestionEnds: []int{cols(l)}})
			it := &s.Review[len(s.Review)-1]
			more, ends = &it.Question, &it.QuestionEnds
			continue
		}
		if m := reviewA.FindStringSubmatch(l); m != nil && len(s.Review) > 0 {
			it := &s.Review[len(s.Review)-1]
			it.Answer, it.AnswerEnds = m[1], []int{cols(l)}
			more, ends = &it.Answer, &it.AnswerEnds
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
			*ends = append(*ends, cols(l))
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
// read, and each stands for the white space the CLI wrapped at — or, after a
// FULL line (full[i]: fullLines), for that or for none at all: a word too long
// for its line is broken mid-word or after a "/" only where the line runs out
// (measured 2.1.289: "…hyphens-ev" / "erywhere-…", ".../of/the/" /
// "terminal/…"). Every character on a line must be want's, in order, and
// every character of want must be on a line: an extra or a different
// character anywhere — a person typing into the row — is a mismatch.
//
// 🔴 A BREAK IS NOT FREE AFTER A LINE THAT ENDS SHORT. A space someone types
// inside a word can become a break (the CLI re-wraps the row at it: "…the
// interactive sess" / "ions, …"), and every character on screen is then
// still want's; only the short line says a space is there. So a break after
// a line that is not full must be a space of want's, and a line whose end is
// unknown (full shorter than lines, or nil) is not full.
//
// exact compares the text as typed (the "Type something" row); otherwise both
// sides' runs of white space count as one space (norm), as question text and
// labels always have been.
func wrapEq(lines []string, full []bool, want string, exact bool) bool {
	if !exact {
		want = norm(want)
	}
	pos, first, prevFull := 0, true, false
	for i, l := range lines {
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
			if !prevFull && (pos >= len(want) || want[pos] != ' ') {
				return false // a break after a short line is a space
			}
			for pos < len(want) && want[pos] == ' ' {
				pos++
			}
		}
		if !strings.HasPrefix(want[pos:], l) {
			return false
		}
		pos += len(l)
		first = false
		prevFull = i < len(full) && full[i]
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
func labelMatch(r Row, width int, want string) bool {
	lines := append([]string{r.Label}, r.More...)
	full := fullLines(r.Ends, width)
	for n := 1; n <= len(lines); n++ {
		if wrapEq(lines[:n], full, want, false) {
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
