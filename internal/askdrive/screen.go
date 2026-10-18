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
//     closes or changes the picker, and the driver stops without a key;
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
//   - single-select: a digit chooses its option; it answers the question (and
//     submits a one-question ask, which has no review screen);
//   - multi-select: a digit toggles its option and leaves the cursor where it
//     was; the rows are the options, the "Type something" row, then "Next"
//     (or "Submit" on a one-question ask), reached with Down and taken with
//     Enter;
//   - the "Type something" row (option k+1): on a single-select question its
//     digit focuses it for typing; on a multi-select question Down moves onto
//     it; typed text is the answer (and checks it, multi-select); in that row
//     digits and letters are TEXT;
//   - an ask with several questions (or a multi-select one) ends on a review
//     screen listing every "question → answer"; "1" submits it.
package askdrive

import (
	"regexp"
	"strconv"
	"strings"
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
}

// Tab is one question's tab in a multi-question header.
type Tab struct {
	Header   string
	Answered bool
}

// ReviewItem is one "question → answer" of the review screen.
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
	// Question is the shown question's text (lines joined by one space).
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
	tabsRE   = regexp.MustCompile(`^\s*(?:←\s+)?((?:[☐☒] .+?\s{2,})*[☐☒] .+?)(?:\s+✔ Submit\s+→)?\s*$`)
	tabRE    = regexp.MustCompile(`([☐☒]) (.+?)(?:\s{2,}|$)`)
	rowRE    = regexp.MustCompile(`^(❯| ) (\d+)\. (?:\[(✔| )\] )?(.*?)\s*$`)
	navRE    = regexp.MustCompile(`^(❯| ) {4}(Next|Submit)\s*$`)
	sepRE    = regexp.MustCompile(`^─{10,}\s*$`)
	reviewQ  = regexp.MustCompile(`^ ● (.*?)\s*$`)
	reviewA  = regexp.MustCompile(`^ {3}→ (.*?)\s*$`)
	footerRE = regexp.MustCompile(`^Enter to select · `)
)

func trimRight(s string) string { return strings.TrimRight(s, " \t") }

// Parse reads the picker from a pane capture (tmux capture-pane -p -J). The
// picker is the bottom of the screen: its header line is the LAST tabs line
// above a footer ("Enter to select · …") or a review screen's Submit row.
// Anything that does not parse as one of the two screens is None.
func Parse(capture string) Screen {
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
		if t := strings.TrimSpace(body[i]); t != "" {
			q = append(q, t)
		}
	}
	s.Question = strings.Join(q, " ")
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
			continue // a description line
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

func parseReview(s Screen, body []string) Screen {
	s.Kind = Review
	seen := false
	for i := 0; i < len(body); i++ {
		l := body[i]
		if strings.TrimSpace(l) == "Review your answers" {
			seen = true
			continue
		}
		if m := reviewQ.FindStringSubmatch(l); m != nil {
			s.Review = append(s.Review, ReviewItem{Question: m[1]})
			continue
		}
		if m := reviewA.FindStringSubmatch(l); m != nil && len(s.Review) > 0 {
			s.Review[len(s.Review)-1].Answer = m[1]
			continue
		}
		if m := rowRE.FindStringSubmatch(l); m != nil && m[1] == "❯" {
			s.ReviewCursor, _ = strconv.Atoi(m[2])
		}
	}
	if !seen {
		return Screen{}
	}
	return s
}

// norm is s with runs of white space as one space, trimmed: how question text
// and labels are compared (the picker wraps a long question over lines).
func norm(s string) string { return strings.Join(strings.Fields(s), " ") }
