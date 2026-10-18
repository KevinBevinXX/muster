package askdrive

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/askq"
)

// The testdata screens are REAL: tmux capture-pane -p -J of Claude Code
// 2.1.289's picker in a scratch session, each the moment after the step its
// name says (the PR's gate drove them), cut to the picker.

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var (
	color = askq.Question{Question: "Which color do you prefer?", Header: "Color",
		Options: []askq.Option{{Label: "Red", Description: "Warm"}, {Label: "Blue", Description: "Cool"}, {Label: "Green", Description: "Natural"}}}
	color4 = askq.Question{Question: "Which color do you prefer?", Header: "Color",
		Options: []askq.Option{{Label: "Red", Description: "Warm"}, {Label: "Blue", Description: "Cool"}, {Label: "Green", Description: "Natural"}, {Label: "Yellow", Description: "Bright"}}}
	pets = askq.Question{Question: "Which pets do you have?", Header: "Pets", MultiSelect: true,
		Options: []askq.Option{{Label: "Cat", Description: "Feline"}, {Label: "Dog", Description: "Canine"}, {Label: "Fish", Description: "Aquatic"}}}
	name = askq.Question{Question: "What should the project be called?", Header: "Name",
		Options: []askq.Option{{Label: "Alpha", Description: "First"}, {Label: "Beta", Description: "Second"}}}
	three = []askq.Question{color, pets, name}
	// The gate's three-question answers: Q1 in its own words, Q2 Dog + its own
	// words, Q3 Beta.
	threeChoices = []Choice{{Other: "teal, mostly"}, {Picks: []int{1}, Other: "a hamster"}, {Picks: []int{1}}}
)

// TestParseReadsTheRealScreens: every captured screen reads as what it is.
func TestParseReadsTheRealScreens(t *testing.T) {
	cases := []struct {
		file string
		qs   []askq.Question
		want Want
	}{
		{"three-q1", three, Want{Kind: Question, Q: 0, Cursor: 1}},
		{"three-q1-other", three, Want{Kind: Question, Q: 0, Cursor: 4}},
		{"three-q1-typed", three, Want{Kind: Question, Q: 0, Cursor: 4, Text: "teal, mostly"}},
		{"three-q2", three, Want{Kind: Question, Q: 1, Cursor: 1}},
		{"three-q2-toggled", three, Want{Kind: Question, Q: 1, Cursor: 1, Checked: []int{2}}},
		{"three-q2-textrow", three, Want{Kind: Question, Q: 1, Cursor: 4, Checked: []int{2}}},
		{"three-q2-typed", three, Want{Kind: Question, Q: 1, Cursor: 4, Checked: []int{2, 4}, Text: "a hamster"}},
		{"three-q2-next", three, Want{Kind: Question, Q: 1, Cursor: 0, Checked: []int{2, 4}, Text: "a hamster"}},
		{"three-q3", three, Want{Kind: Question, Q: 2, Cursor: 1}},
		{"three-review", three, Want{Kind: Review}},
		{"three-done", three, Want{Kind: None}},
		{"single-q", []askq.Question{color4}, Want{Kind: Question, Q: 0, Cursor: 1}},
		{"single-done", []askq.Question{color4}, Want{Kind: None}},
		{"multi-q", []askq.Question{pets}, Want{Kind: Question, Q: 0, Cursor: 1}},
		{"multi-toggled", []askq.Question{pets}, Want{Kind: Question, Q: 0, Cursor: 1, Checked: []int{1, 3}}},
		{"multi-submitrow", []askq.Question{pets}, Want{Kind: Question, Q: 0, Cursor: 0, Checked: []int{1, 3}}},
		{"multi-review", []askq.Question{pets}, Want{Kind: Review}},
		{"multi-done", []askq.Question{pets}, Want{Kind: None}},
	}
	reviews := map[string]map[string]string{
		"three-review": Answers(three, threeChoices),
		"multi-review": {"Which pets do you have?": "Cat, Fish"},
	}
	for _, c := range cases {
		s := Parse(fixture(t, c.file))
		if err := Match(s, c.want, c.qs, reviews[c.file]); err != nil {
			t.Errorf("%s: %v\n  parsed %+v", c.file, err, s)
		}
		// The control: the same screen is NOT a neighbouring state.
		other := c.want
		other.Cursor = (c.want.Cursor + 1) % 5
		if c.want.Kind == Question && Match(s, other, c.qs, nil) == nil {
			t.Errorf("%s: also matched %s — the matcher cannot tell rows apart", c.file, other)
		}
	}
	if s := Parse(fixture(t, "three-q2-typed")); s.Rows[3].Label != "a hamster" || s.Rows[3].Box != "[✔]" || !s.Rows[3].Cursor {
		t.Errorf("the typed text row: %+v", s.Rows[3])
	}
	if s := Parse(fixture(t, "three-review")); !reflect.DeepEqual(s.Review, []ReviewItem{
		{"Which color do you prefer?", "teal, mostly"}, {"Which pets do you have?", "Dog, a hamster"}, {"What should the project be called?", "Beta"}}) {
		t.Errorf("the review: %+v", s.Review)
	}
}

// TestPlanIsTheMeasuredKeySequence pins the exact keys for each shape the gate
// drove by hand.
func TestPlanIsTheMeasuredKeySequence(t *testing.T) {
	keys := func(qs []askq.Question, cs []Choice) []string {
		t.Helper()
		steps, err := Plan(qs, cs)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, s := range steps {
			out = append(out, s.Keys)
		}
		return out
	}
	D, E := KeyDown, KeyEnter
	cases := []struct {
		name string
		qs   []askq.Question
		cs   []Choice
		want []string
	}{
		{"single-select, one question: the digit answers and submits", []askq.Question{color}, []Choice{{Picks: []int{1}}}, []string{"2"}},
		{"single-select, own words", []askq.Question{color}, []Choice{{Other: "Gamma, the third"}}, []string{"4", "Gamma, the third", E}},
		{"multi-select: toggles, Down past the text row to Submit, Enter, then the review's 1",
			[]askq.Question{pets}, []Choice{{Picks: []int{2, 0}}}, []string{"1", "3", D, D, D, D, E, "1"}},
		{"multi-select with own words: Down to the text row, type, Down to Submit",
			[]askq.Question{pets}, []Choice{{Picks: []int{1}, Other: "a hamster"}}, []string{"2", D, D, D, "a hamster", D, E, "1"}},
		{"three questions (the gate's)", three, threeChoices,
			[]string{"4", "teal, mostly", E, "2", D, D, D, "a hamster", D, E, "2", "1"}},
	}
	for _, c := range cases {
		if got := keys(c.qs, c.cs); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestPlanRefusesWhatCannotBeTyped(t *testing.T) {
	cases := map[string][]Choice{
		"two picks, single-select":    {{Picks: []int{0, 1}}},
		"a pick and text, single":     {{Picks: []int{0}, Other: "x"}},
		"nothing, single":             {{}},
		"out of range":                {{Picks: []int{3}}},
		"text with a line break":      {{Other: "a\nb"}},
		"text with a tab":             {{Other: "a\tb"}},
		"text with an escape":         {{Other: "a\x1b[Bb"}},
		"text with outer white space": {{Other: " a"}},
		"wrong number of choices (0)": {},
	}
	for name, cs := range cases {
		if _, err := Plan([]askq.Question{color}, cs); !errors.Is(err, ErrUntypeable) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := Plan([]askq.Question{pets}, []Choice{{Picks: []int{0, 0}}}); !errors.Is(err, ErrUntypeable) {
		t.Errorf("a repeated pick: %v", err)
	}
}

// fakePane is a FakePicker as a Pane, recording every key it was sent; before,
// when set, runs before each Send (the "someone in the terminal" of a race).
type fakePane struct {
	mu     sync.Mutex
	p      *FakePicker
	sent   []string
	before func(n int)
}

func (f *fakePane) Capture(context.Context) (string, error) { return f.p.Screen(), nil }
func (f *fakePane) Send(_ context.Context, k string) error {
	f.mu.Lock()
	n := len(f.sent)
	f.sent = append(f.sent, k)
	f.mu.Unlock()
	if f.before != nil {
		f.before(n)
	}
	f.p.Type(k)
	return nil
}

var fast = Options{Settle: 500 * time.Millisecond, Poll: time.Millisecond}

// TestDriveAnswersTheFakePicker: every shape, end to end, submits exactly the
// answers asked for.
func TestDriveAnswersTheFakePicker(t *testing.T) {
	cases := []struct {
		qs []askq.Question
		cs []Choice
	}{
		{[]askq.Question{color}, []Choice{{Picks: []int{2}}}},
		{[]askq.Question{color}, []Choice{{Other: "teal"}}},
		{[]askq.Question{pets}, []Choice{{Picks: []int{0, 2}}}},
		{[]askq.Question{pets}, []Choice{{Other: "a hamster"}}},
		{three, threeChoices},
		{[]askq.Question{name, pets, color4}, []Choice{{Picks: []int{0}}, {Picks: []int{0, 1, 2}}, {Picks: []int{3}}}},
	}
	for i, c := range cases {
		fp := &fakePane{p: NewFakePicker(c.qs)}
		if err := Drive(context.Background(), fp, c.qs, c.cs, fast); err != nil {
			t.Fatalf("case %d: %v\nkeys %q\nscreen:\n%s", i, err, fp.sent, fp.p.Screen())
		}
		got, cancelled, done := fp.p.Result()
		if !done || cancelled || !reflect.DeepEqual(got, Answers(c.qs, c.cs)) {
			t.Fatalf("case %d: submitted %v (cancelled %v done %v), want %v", i, got, cancelled, done, Answers(c.qs, c.cs))
		}
	}
}

// TestTheFakeDrawsWhatTheCLIDraws: the fake picker, brought to each state the
// gate captured, PARSES to the same screen as the real capture — so a drive
// that passes against the fake would read the real one alike.
func TestTheFakeDrawsWhatTheCLIDraws(t *testing.T) {
	fp := NewFakePicker(three)
	steps := map[string]string{"three-q1": "", "three-q1-other": "4", "three-q1-typed": "teal, mostly", "three-q2": KeyEnter,
		"three-q2-toggled": "2", "three-q2-textrow": KeyDown + KeyDown + KeyDown, "three-q2-typed": "a hamster", "three-q2-next": KeyDown,
		"three-q3": KeyEnter, "three-review": "2"}
	for _, f := range []string{"three-q1", "three-q1-other", "three-q1-typed", "three-q2", "three-q2-toggled", "three-q2-textrow",
		"three-q2-typed", "three-q2-next", "three-q3", "three-review"} {
		fp.Type(steps[f])
		real, fake := Parse(fixture(t, f)), Parse(fp.Screen())
		if !reflect.DeepEqual(real, fake) {
			t.Errorf("%s: the fake draws\n %+v\nthe CLI drew\n %+v", f, fake, real)
		}
	}
}

// TestDriveNeverTypesIntoAPickerItDoesNotExpect is the race half: the picker
// gone, touched, or changed under the drive.
func TestDriveNeverTypesIntoAPickerItDoesNotExpect(t *testing.T) {
	// Answered in the terminal before muster's answer arrived: no picker.
	gone := NewFakePicker([]askq.Question{color})
	gone.Type("1")
	fp := &fakePane{p: gone}
	if err := Drive(context.Background(), fp, []askq.Question{color}, []Choice{{Picks: []int{1}}}, fast); !errors.Is(err, ErrNoPicker) || len(fp.sent) != 0 {
		t.Fatalf("a closed picker: %v, sent %q", err, fp.sent)
	}
	// Someone has started in the terminal (a box ticked): untouched it is not.
	touched := NewFakePicker([]askq.Question{pets})
	touched.Type("2")
	fp = &fakePane{p: touched}
	if err := Drive(context.Background(), fp, []askq.Question{pets}, []Choice{{Picks: []int{0}}}, fast); !errors.Is(err, ErrNotUntouched) || len(fp.sent) != 0 {
		t.Fatalf("a touched picker: %v, sent %q", err, fp.sent)
	}
	// Another question on screen (another ask): not this one.
	fp = &fakePane{p: NewFakePicker([]askq.Question{name})}
	if err := Drive(context.Background(), fp, []askq.Question{color}, []Choice{{Picks: []int{0}}}, fast); !errors.Is(err, ErrNotUntouched) || len(fp.sent) != 0 {
		t.Fatalf("another question: %v, sent %q", err, fp.sent)
	}
	// Mid-drive, the person in the terminal ticks a box too: the drive stops
	// at the next look, and never reaches the review's submit.
	p := NewFakePicker(three)
	fp = &fakePane{p: p}
	fp.before = func(n int) {
		if n == 4 { // as the drive moves down Q2
			p.Type("3")
		}
	}
	err := Drive(context.Background(), fp, three, threeChoices, fast)
	if !errors.Is(err, ErrLost) {
		t.Fatalf("a picker changed mid-drive: %v", err)
	}
	if _, _, done := p.Result(); done {
		t.Fatal("the drive submitted a picker that changed under it")
	}
	for _, k := range fp.sent {
		if k == "1" && len(fp.sent) > 10 {
			t.Fatalf("the review's submit was sent: %q", fp.sent)
		}
	}
}

// TestTheReviewMustShowTheAnswersBeforeTheSubmit: a review listing other
// answers (the person changed one in the terminal) is not submitted.
func TestTheReviewMustShowTheAnswersBeforeTheSubmit(t *testing.T) {
	p := NewFakePicker([]askq.Question{pets})
	fp := &fakePane{p: p}
	fp.before = func(n int) {
		if n == 6 { // the Enter that opens the review: tick Dog first
			p.mu.Lock()
			p.checked[0][2] = true
			p.mu.Unlock()
		}
	}
	err := Drive(context.Background(), fp, []askq.Question{pets}, []Choice{{Picks: []int{0, 2}}}, fast)
	if !errors.Is(err, ErrLost) || !strings.Contains(err.Error(), "review") {
		t.Fatalf("a review with other answers: %v", err)
	}
	if _, _, done := p.Result(); done {
		t.Fatal("a review with other answers was submitted")
	}
}

// lookPane is fakePane whose look hook runs before the n-th Capture (from 1):
// the terminal changing the picker BETWEEN two keys — after the drive saw its
// last key land, before it sends the next. drop, when set, names a Send (from
// 0) the picker never receives.
type lookPane struct {
	*fakePane
	looks int
	look  func(n int)
	drop  func(n int) bool
}

func (l *lookPane) Capture(ctx context.Context) (string, error) {
	l.looks++
	if l.look != nil {
		l.look(l.looks)
	}
	return l.fakePane.Capture(ctx)
}

func (l *lookPane) Send(ctx context.Context, k string) error {
	if l.drop != nil && l.drop(len(l.sent)) {
		l.sent = append(l.sent, k)
		return nil
	}
	return l.fakePane.Send(ctx, k)
}

// TestEveryKeyIsPrecededByALook: the person ticks a box in the terminal after
// the drive's second key has landed; the drive's look before its third key
// sees it and sends nothing more.
func TestEveryKeyIsPrecededByALook(t *testing.T) {
	p := NewFakePicker([]askq.Question{pets})
	lp := &lookPane{fakePane: &fakePane{p: p}}
	lp.look = func(n int) {
		if n == 5 { // looks: before key 1, after it, before key 2, after it, before key 3
			p.Type("2")
		}
	}
	err := Drive(context.Background(), lp, []askq.Question{pets}, []Choice{{Picks: []int{0, 2}}}, fast)
	if !errors.Is(err, ErrLost) || !strings.Contains(err.Error(), "before key 3") {
		t.Fatalf("a picker changed between keys: %v", err)
	}
	if !reflect.DeepEqual(lp.sent, []string{"1", "3"}) {
		t.Fatalf("keys sent into a picker that changed between them: %q", lp.sent)
	}
}

// TestALostLastKeyIsNotASuccess: the submit never reaches the picker (the
// pane dropped it); the drive says so instead of reporting it answered.
func TestALostLastKeyIsNotASuccess(t *testing.T) {
	p := NewFakePicker([]askq.Question{pets})
	lp := &lookPane{fakePane: &fakePane{p: p}}
	lp.drop = func(n int) bool { return n == 7 } // the review's "1"
	err := Drive(context.Background(), lp, []askq.Question{pets}, []Choice{{Picks: []int{0, 2}}}, fast)
	if !errors.Is(err, ErrLost) || !strings.Contains(err.Error(), "after key 8") {
		t.Fatalf("a submit that never landed: %v (sent %q)", err, lp.sent)
	}
	if _, _, done := p.Result(); done {
		t.Fatal("the control: the picker submitted")
	}
}

// TestNineRowsIsTheMostADigitReaches: own words go on the row after the
// options, chosen by its digit — so eight options is the most that can take
// own words, and nine refuse them.
func TestNineRowsIsTheMostADigitReaches(t *testing.T) {
	q := func(n int) askq.Question {
		qq := askq.Question{Question: "Which?", Header: "Which"}
		for i := 0; i < n; i++ {
			qq.Options = append(qq.Options, askq.Option{Label: string(rune('A' + i))})
		}
		return qq
	}
	if _, err := Plan([]askq.Question{q(8)}, []Choice{{Other: "mine"}}); err != nil {
		t.Fatalf("eight options and own words: %v", err)
	}
	if _, err := Plan([]askq.Question{q(9)}, []Choice{{Other: "mine"}}); !errors.Is(err, ErrUntypeable) {
		t.Fatalf("nine options and own words: %v", err)
	}
}
