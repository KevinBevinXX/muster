package askdrive

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/askq"
)

// The keys, as the bytes a terminal sends for them.
const (
	KeyEnter = "\r"
	KeyDown  = "\x1b[B"
)

// Choice is the answer to one question (askq.Choice): the options chosen
// (0-based indexes into its Options; one for a single-select question, any
// number for a multi-select one) and the person's own text ("" for none). A
// single-select question takes exactly one of the two.
type Choice = askq.Choice

// Want is the screen a step must leave the pane in — and, re-read, the state
// the NEXT step requires before it sends anything.
type Want struct {
	Kind Kind
	// Q is the question shown (Question): its index.
	Q int
	// Cursor is the numbered row the cursor must be on (Question), 0 for the
	// Next/Submit row.
	Cursor int
	// Checked is the options that must be checked, as 1-based row numbers
	// (multi-select); Text the "Type something" row's typed text ("" = its
	// placeholder).
	Checked []int
	Text    string
}

func (w Want) String() string {
	switch w.Kind {
	case None:
		return "no picker"
	case Review:
		return "the review screen"
	}
	cur := "row " + strconv.Itoa(w.Cursor)
	if w.Cursor == 0 {
		cur = "the Next/Submit row"
	}
	s := fmt.Sprintf("question %d, cursor on %s", w.Q+1, cur)
	if len(w.Checked) > 0 {
		s += fmt.Sprintf(", %v checked", w.Checked)
	}
	if w.Text != "" {
		s += fmt.Sprintf(", %q typed", w.Text)
	}
	return s
}

// Step is one key (or one typed text) and the screen it must produce.
type Step struct {
	Keys string
	Want Want
}

// ErrUntypeable: an answer the picker cannot be given by keys (a choice out of
// range, text with a line break, several picks on a single-select question).
var ErrUntypeable = errors.New("askdrive: this answer cannot be typed into the picker")

func untypeable(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrUntypeable}, a...)...)
}

// typeableText reports whether t can be typed into the picker's one-line text
// row: non-empty, printable, no line break or tab.
func typeableText(t string) bool {
	if strings.TrimSpace(t) == "" || t != strings.TrimSpace(t) {
		return false
	}
	for _, r := range t {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return false
		}
	}
	return true
}

// Plan is the keys that answer qs with choices, each with the screen it must
// leave — the whole drive, from the untouched picker (Start) to the picker
// gone. It does not touch a pane.
func Plan(qs []askq.Question, choices []Choice) ([]Step, error) {
	if len(qs) == 0 || len(choices) != len(qs) {
		return nil, untypeable("%d choices for %d questions", len(choices), len(qs))
	}
	n := len(qs)
	var steps []Step
	review := n > 1 || qs[0].MultiSelect
	// next is the screen after question i is answered.
	next := func(i int) Want {
		switch {
		case i+1 < n:
			return Want{Kind: Question, Q: i + 1, Cursor: 1}
		case review:
			return Want{Kind: Review}
		}
		return Want{Kind: None}
	}
	for i, q := range qs {
		c := choices[i]
		k := len(q.Options)
		if k+1 > 9 {
			return nil, untypeable("question %d has more options than digit keys", i+1)
		}
		picks := slices.Clone(c.Picks)
		slices.Sort(picks)
		for j, p := range picks {
			if p < 0 || p >= k || (j > 0 && picks[j-1] == p) {
				return nil, untypeable("question %d: option %d", i+1, p+1)
			}
		}
		if c.Other != "" && !typeableText(c.Other) {
			return nil, untypeable("question %d: the text cannot be typed into one line", i+1)
		}
		if q.Previews() {
			// A digit moves the cursor to its row (none to move for the first,
			// where the cursor starts); Enter takes it. There is no row to type
			// an answer of one's own into.
			if len(picks) != 1 || c.Other != "" {
				return nil, untypeable("question %d shows previews: it takes exactly one of its options", i+1)
			}
			if picks[0] > 0 {
				steps = append(steps, Step{Keys: digit(picks[0] + 1), Want: Want{Kind: Question, Q: i, Cursor: picks[0] + 1}})
			}
			steps = append(steps, Step{Keys: KeyEnter, Want: next(i)})
			continue
		}
		if !q.MultiSelect {
			switch {
			case len(picks) == 1 && c.Other == "":
				steps = append(steps, Step{Keys: digit(picks[0] + 1), Want: next(i)})
			case len(picks) == 0 && c.Other != "":
				steps = append(steps,
					Step{Keys: digit(k + 1), Want: Want{Kind: Question, Q: i, Cursor: k + 1}},
					Step{Keys: c.Other, Want: Want{Kind: Question, Q: i, Cursor: k + 1, Text: c.Other}},
					Step{Keys: KeyEnter, Want: next(i)})
			default:
				return nil, untypeable("question %d takes exactly one answer", i+1)
			}
			continue
		}
		if len(picks) == 0 && c.Other == "" {
			return nil, untypeable("question %d is not answered", i+1)
		}
		var checked []int
		for _, p := range picks {
			checked = append(checked, p+1)
			steps = append(steps, Step{Keys: digit(p + 1), Want: Want{Kind: Question, Q: i, Cursor: 1, Checked: slices.Clone(checked)}})
		}
		// Down, one row at a time, to the text row (to type) or to Next.
		stop := k + 2 // the Next/Submit row
		if c.Other != "" {
			stop = k + 1
		}
		for row := 2; row <= stop; row++ {
			cur := row
			if row == k+2 {
				cur = 0
			}
			steps = append(steps, Step{Keys: KeyDown, Want: Want{Kind: Question, Q: i, Cursor: cur, Checked: slices.Clone(checked)}})
		}
		if c.Other != "" {
			withText := append(slices.Clone(checked), k+1)
			steps = append(steps,
				Step{Keys: c.Other, Want: Want{Kind: Question, Q: i, Cursor: k + 1, Checked: withText, Text: c.Other}},
				Step{Keys: KeyDown, Want: Want{Kind: Question, Q: i, Cursor: 0, Checked: withText, Text: c.Other}})
		}
		steps = append(steps, Step{Keys: KeyEnter, Want: next(i)})
	}
	if review {
		steps = append(steps, Step{Keys: "1", Want: Want{Kind: None}})
	}
	return steps, nil
}

// Start is the screen the first key requires: the first question, untouched.
func Start() Want { return Want{Kind: Question, Q: 0, Cursor: 1} }

// At is the screen steps leave the picker in after their first k keys: Start
// for none.
func At(steps []Step, k int) Want {
	if k <= 0 {
		return Start()
	}
	return steps[k-1].Want
}

func digit(n int) string { return strconv.Itoa(n) }

// Answers is the answers choices give, keyed by question text, as the CLI
// records them (askq.AnswersOf).
func Answers(qs []askq.Question, choices []Choice) map[string]string {
	return askq.AnswersOf(qs, choices)
}
