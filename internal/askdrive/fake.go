package askdrive

import (
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/askq"
)

// FakePicker is a stand-in for Claude Code's AskUserQuestion picker: the same
// screens (laid out as the CLI draws them — Parse reads both, and the real
// screens are its test fixtures) and the same response to keys as measured on
// 2.1.289 (the package doc). The browser suite's fake host and the real-tmux
// test run it where no Claude Code is; it is never a substitute for the
// measured behaviour, only a copy of it.
type FakePicker struct {
	mu        sync.Mutex
	qs        []askq.Question
	tab       int
	cursor    int // 1-based row; 0 = the Next/Submit row
	checked   []map[int]bool
	text      []string
	review    bool
	done      bool
	cancelled bool
	answers   map[string]string
	pending   string // an incomplete escape sequence
}

// NewFakePicker shows qs, untouched.
func NewFakePicker(qs []askq.Question) *FakePicker {
	f := &FakePicker{qs: qs, cursor: 1, checked: make([]map[int]bool, len(qs)), text: make([]string, len(qs))}
	for i := range f.checked {
		f.checked[i] = map[int]bool{}
	}
	return f
}

// Result is the answers the picker submitted (nil while it is open), and
// whether it was cancelled instead.
func (f *FakePicker) Result() (answers map[string]string, cancelled, done bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.answers, f.cancelled, f.done
}

func (f *FakePicker) multiTabs() bool { return len(f.qs) > 1 || f.qs[0].MultiSelect }

// answered reports whether question i has an answer chosen.
func (f *FakePicker) answered(i int) bool {
	return len(f.checked[i]) > 0 || f.text[i] != ""
}

func (f *FakePicker) answerOf(i int) string {
	q := f.qs[i]
	var parts []string
	for j, o := range q.Options {
		if f.checked[i][j+1] {
			parts = append(parts, o.Label)
		}
	}
	if f.text[i] != "" && (!q.MultiSelect || f.checked[i][len(q.Options)+1]) {
		parts = append(parts, f.text[i])
	}
	return strings.Join(parts, askq.MultiJoin)
}

func (f *FakePicker) submit() {
	f.answers = map[string]string{}
	for i, q := range f.qs {
		f.answers[q.Question] = f.answerOf(i)
	}
	f.done = true
}

// advance moves past an answered question.
func (f *FakePicker) advance() {
	if f.tab+1 < len(f.qs) {
		f.tab++
		f.cursor = 1
		return
	}
	if f.multiTabs() {
		f.review = true
		return
	}
	f.submit()
}

// Type feeds keys (terminal bytes) to the picker.
func (f *FakePicker) Type(keys string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys = f.pending + keys
	f.pending = ""
	for keys != "" && !f.done {
		switch {
		case strings.HasPrefix(keys, "\x1b["):
			if len(keys) < 3 {
				f.pending = keys
				return
			}
			f.arrow(keys[2])
			keys = keys[3:]
			continue
		case keys[0] == 0x1b:
			if len(keys) == 1 {
				// A lone Esc cancels the ask.
				f.cancelled, f.done = true, true
				return
			}
		}
		r, n := utf8.DecodeRuneInString(keys)
		keys = keys[n:]
		f.key(r)
	}
}

func (f *FakePicker) arrow(c byte) {
	if f.review {
		return
	}
	q := f.qs[f.tab]
	k := len(q.Options)
	last := k + 1
	switch c {
	case 'B': // Down
		switch {
		case f.cursor == 0:
		case f.cursor < last:
			f.cursor++
		case q.MultiSelect:
			f.cursor = 0
		}
	case 'A': // Up
		switch {
		case f.cursor == 0:
			f.cursor = last
		case f.cursor > 1:
			f.cursor--
		}
	}
}

func (f *FakePicker) key(r rune) {
	if f.review {
		if r == '1' || (r == '\r') {
			f.submit()
		} else if r == '2' {
			f.cancelled, f.done = true, true
		}
		return
	}
	i := f.tab
	q := f.qs[i]
	k := len(q.Options)
	editing := f.cursor == k+1
	if editing && r != '\r' && r != 0x7f {
		if r >= 0x20 {
			f.text[i] += string(r)
			if q.MultiSelect {
				f.checked[i][k+1] = true
			}
		}
		return
	}
	switch {
	case r == 0x7f:
		if editing && f.text[i] != "" {
			_, n := utf8.DecodeLastRuneInString(f.text[i])
			f.text[i] = f.text[i][:len(f.text[i])-n]
		}
	case r == '\r':
		switch {
		case q.MultiSelect && f.cursor == 0:
			if f.answered(i) {
				f.advance()
			}
		case !q.MultiSelect && editing:
			if f.text[i] != "" {
				f.advance()
			}
		case !q.MultiSelect && f.cursor >= 1 && f.cursor <= k:
			f.checked[i] = map[int]bool{f.cursor: true}
			f.advance()
		}
	case r >= '1' && r <= '9':
		d := int(r - '0')
		switch {
		case q.MultiSelect && d <= k+1:
			f.checked[i][d] = !f.checked[i][d]
			if !f.checked[i][d] {
				delete(f.checked[i], d)
			}
		case !q.MultiSelect && d <= k:
			f.checked[i] = map[int]bool{d: true}
			f.text[i] = ""
			f.advance()
		case !q.MultiSelect && d == k+1:
			f.cursor = k + 1
		}
	}
}

const fakeRule = "────────────────────────────────────────────────────────────────────────────────"

// Screen is the picker as a pane would show it ("" once it is closed: the
// session's prompt box instead).
func (f *FakePicker) Screen() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	line := func(s string) { b.WriteString(s + "\n") }
	if f.done {
		line(fakeRule)
		line("❯ ")
		line(fakeRule)
		line("  ⏸ manual mode on · ? for shortcuts")
		return b.String()
	}
	line(fakeRule)
	// The header.
	var tabs []string
	for i, q := range f.qs {
		mark := "☐"
		if f.answered(i) && (i < f.tab || f.review || (i == f.tab && q.MultiSelect)) {
			mark = "☒"
		}
		tabs = append(tabs, mark+" "+q.Header)
	}
	if f.multiTabs() {
		line("←  " + strings.Join(tabs, "  ") + "  ✔ Submit  →")
	} else {
		line(" " + tabs[0])
	}
	line("")
	if f.review {
		line("Review your answers")
		for i, q := range f.qs {
			lead, more := " ● ", "   "
			if boxed(q.Question) {
				lead, more = " │ ● ", " │   "
			}
			for j, l := range fakeWrap(q.Question, fakeWidth-len([]rune(lead))) {
				if j == 0 {
					line(lead + l)
				} else {
					line(more + l)
				}
			}
			for j, l := range fakeWrap(f.answerOf(i), fakeWidth-5) {
				if j == 0 {
					line("   → " + l)
				} else {
					line("   " + l)
				}
			}
		}
		line("Ready to submit your answers?")
		line("❯ 1. Submit answers")
		line("  2. Cancel")
		return b.String()
	}
	q := f.qs[f.tab]
	k := len(q.Options)
	if boxed(q.Question) {
		for _, l := range fakeWrap(q.Question, fakeWidth-2) {
			line("│ " + l)
		}
	} else {
		line(q.Question)
	}
	line("")
	cur := func(row int) string {
		if f.cursor == row {
			return "❯"
		}
		return " "
	}
	box := func(row int) string {
		if !q.MultiSelect {
			return ""
		}
		if f.checked[f.tab][row] {
			return "[✔] "
		}
		return "[ ] "
	}
	// row draws a numbered row, its label (or text) wrapped under itself.
	row := func(n int, label string) {
		lead := cur(n) + " " + itoa(n) + ". " + box(n)
		indent := strings.Repeat(" ", len([]rune(lead)))
		for j, l := range fakeWrap(label, fakeWidth-len(indent)) {
			if j == 0 {
				line(lead + l)
			} else {
				line(indent + l)
			}
		}
	}
	for j, o := range q.Options {
		row(j+1, o.Label)
		if o.Description != "" {
			line("     " + o.Description)
		}
	}
	textLabel := f.text[f.tab]
	if textLabel == "" {
		textLabel = "Type something."
		if q.MultiSelect {
			textLabel = "Type something"
		}
	}
	row(k+1, textLabel)
	if q.MultiSelect {
		nav := "Next"
		if f.tab == len(f.qs)-1 {
			nav = "Submit"
		}
		line(cur(0) + "    " + nav)
	}
	line(fakeRule)
	line("  " + itoa(k+2) + ". Chat about this")
	line("")
	line("Enter to select · ↑/↓ to navigate · Esc to cancel")
	return b.String()
}

// fakeWidth is the fake's pane width: the 80 columns its rule is.
const fakeWidth = 80

// boxed reports whether the CLI draws question q in a box: one too long for
// a line (measured 2.1.289: "│ " down its left edge, wrapped inside).
func boxed(q string) bool { return len([]rune(q)) > fakeWidth-2 }

// fakeWrap is s word-wrapped to width columns, a word longer than a line
// broken mid-word — as the CLI wraps (its exact break points differ; Parse
// and Match do not depend on them).
func fakeWrap(s string, width int) []string {
	if len([]rune(s)) <= width {
		return []string{s} // as typed, its own spaces kept
	}
	var out []string
	cur := ""
	for _, w := range strings.Fields(s) {
		for len([]rune(w)) > width {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			r := []rune(w)
			out = append(out, string(r[:width]))
			w = string(r[width:])
		}
		switch {
		case cur == "":
			cur = w
		case len([]rune(cur))+1+len([]rune(w)) <= width:
			cur += " " + w
		default:
			out = append(out, cur)
			cur = w
		}
	}
	if cur != "" || len(out) == 0 {
		out = append(out, cur)
	}
	return out
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}
