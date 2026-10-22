package askdrive

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/askq"
)

// THE PREVIEW PICKER. The testdata/preview-* screens are REAL: Claude Code
// 2.1.289's picker for questions whose options carry a `preview`, captured in
// a scratch session in a private tmux server (100 and 80 columns), cut to the
// picker. The question and option texts are the synthetic ones below.

var (
	pvColor = askq.Question{Question: "Which color do you prefer?", Header: "Color", Options: []askq.Option{
		{Label: "Red", Description: "Warm", Preview: "Red is bold and energetic.\nIt evokes passion and urgency."},
		{Label: "Blue", Description: "Cool", Preview: "Blue is cool and steady.\nIt evokes trust and calm."},
		{Label: "Green", Description: "Natural", Preview: "Green is fresh and balanced.\nIt evokes forests and growth."}}}
	pvSize = askq.Question{Question: "Which size?", Header: "Size", Options: []askq.Option{
		{Label: "Small", Description: "Compact"}, {Label: "Large", Description: "Spacious"}}}
	// pvPlan is the shape of the live failure: a long question, long labels
	// wrapped within the preview layout's column.
	pvPlan = askq.Question{Question: "The nightly batch exporter writes every record twice when a retry overlaps the previous run, " +
		"and the downstream loader rejects the whole file on the first duplicate key. Which way should we handle it before the next release?",
		Header: "Plan", Options: []askq.Option{
			{Label: "Fix it in the exporter before merging (Recommended)", Preview: "Where:  exporter retry path\nWhen:   in this PR, pre-merge"},
			{Label: "Separate change, before release", Preview: "Where:  exporter, new PR"},
			{Label: "File it for later", Preview: "Where:  the backlog"}}}
	// pvMixed: a preview on ONE option is enough for the preview layout.
	pvMixed = askq.Question{Question: "Which color do you prefer?", Header: "Color", Options: []askq.Option{
		{Label: "Red", Preview: "Red is bold.\nRed is warm."}, {Label: "Blue"}, {Label: "Green"}}}
)

// TestParseReadsThePreviewScreens: every captured preview screen reads as the
// state it is in.
func TestParseReadsThePreviewScreens(t *testing.T) {
	two := []askq.Question{pvColor, pvSize}
	cases := []struct {
		file    string
		qs      []askq.Question
		want    Want
		answers map[string]string
	}{
		{"preview-q1-100", two, Want{Kind: Question, Q: 0, Cursor: 1}, nil},
		{"preview-q1-down-100", two, Want{Kind: Question, Q: 0, Cursor: 2}, nil},
		{"preview-q1-green-100", two, Want{Kind: Question, Q: 0, Cursor: 3}, nil},
		{"preview-q2-100", two, Want{Kind: Question, Q: 1, Cursor: 1}, nil},
		{"preview-review-100", two, Want{Kind: Review}, Answers(two, []Choice{{Picks: []int{2}}, {Picks: []int{1}}})},
		{"preview-long-80", []askq.Question{pvPlan, pvSize}, Want{Kind: Question, Q: 0, Cursor: 1}, nil},
		{"preview-mixed-100", []askq.Question{pvMixed, pets}, Want{Kind: Question, Q: 0, Cursor: 1}, nil},
		{"preview-single-80", []askq.Question{pvColor}, Want{Kind: Question, Q: 0, Cursor: 1}, nil},
		{"preview-single-down-80", []askq.Question{pvColor}, Want{Kind: Question, Q: 0, Cursor: 2}, nil},
	}
	for _, c := range cases {
		s := Parse(fixture(t, c.file))
		if err := Match(s, c.want, c.qs, c.answers); err != nil {
			t.Errorf("%s: %v\n%+v", c.file, err, s)
		}
	}
	// The rows are the options alone: the box beside them is cut off, and a
	// label wrapped within its column is read whole.
	s := Parse(fixture(t, "preview-long-80"))
	if s.PreviewCol != 34 || len(s.Rows) != 3 || s.Rows[0].Label != "Fix it in the exporter" ||
		!reflect.DeepEqual(s.Rows[0].More, []string{"before merging", "(Recommended)"}) {
		t.Fatalf("the preview rows: col %d, %+v", s.PreviewCol, s.Rows)
	}
	for _, r := range s.Rows {
		for _, l := range append([]string{r.Label}, r.More...) {
			if strings.ContainsAny(l, "│┌└") || strings.Contains(l, "Notes") {
				t.Fatalf("the preview box was read as a row: %q", l)
			}
		}
	}
}

// TestAPreviewPickerIsNotAQuestionWithoutPreviews: the two layouts never read
// as each other — a preview picker for a question registered without
// previews, and the reverse, do not match, in any state.
func TestAPreviewPickerIsNotAQuestionWithoutPreviews(t *testing.T) {
	plain := pvColor
	plain.Options = []askq.Option{{Label: "Red"}, {Label: "Blue"}, {Label: "Green"}}
	if err := Match(Parse(fixture(t, "preview-q1-100")), Start(), []askq.Question{plain, pvSize}, nil); !errors.Is(err, errLayout) {
		t.Fatalf("a preview picker read as a plain question: %v", err)
	}
	withPreview := color
	withPreview.Options = []askq.Option{{Label: "Red", Preview: "x"}, {Label: "Blue"}, {Label: "Green"}}
	if err := Match(Parse(fixture(t, "three-q1")), Start(), []askq.Question{withPreview, pets, name}, nil); !errors.Is(err, errLayout) {
		t.Fatalf("a plain picker read as a preview question: %v", err)
	}
}

// screenPane plays a REAL picker from its captured screens: key → the screen
// the CLI drew after it, as measured. Any key it was not measured for leaves
// the screen as it is.
type screenPane struct {
	t      *testing.T
	screen string
	next   map[string]map[string]string // screen → key → screen
	sent   []string
}

func (s *screenPane) Capture(context.Context) (string, error) { return fixture(s.t, s.screen), nil }
func (s *screenPane) Send(_ context.Context, k string) error {
	s.sent = append(s.sent, k)
	if n, ok := s.next[s.screen][k]; ok {
		s.screen = n
	}
	return nil
}

// TestDriveAnswersARealPreviewPicker is the live failure (Claude Code
// 2.1.289, a four-question ask whose options carried previews): muster
// refused before its first key with "the picker is not untouched — someone is
// answering it in the terminal: 3 rows, not 3 options and the text row". The
// picker was untouched; it was drawn with previews. Driven on the captured
// screens, the answer is now typed — a digit moves the cursor, Enter takes
// the option — and submitted from the review.
func TestDriveAnswersARealPreviewPicker(t *testing.T) {
	two := []askq.Question{pvColor, pvSize}
	p := &screenPane{t: t, screen: "preview-q1-100", next: map[string]map[string]string{
		"preview-q1-100":       {"3": "preview-q1-green-100", "2": "preview-q1-down-100"},
		"preview-q1-green-100": {KeyEnter: "preview-q2-100"},
		"preview-q2-100":       {"2": "preview-review-100"},
		"preview-review-100":   {"1": "three-done"},
	}}
	if err := Drive(context.Background(), p, two, []Choice{{Picks: []int{2}}, {Picks: []int{1}}}, fast); err != nil {
		t.Fatalf("a preview picker: %v (keys %q, on %s)", err, p.sent, p.screen)
	}
	if !reflect.DeepEqual(p.sent, []string{"3", KeyEnter, "2", "1"}) || p.screen != "three-done" {
		t.Fatalf("keys %q, ended on %s", p.sent, p.screen)
	}
	// One question: Enter on it submits (measured: no review screen).
	one := &screenPane{t: t, screen: "preview-single-80", next: map[string]map[string]string{
		"preview-single-80":      {"2": "preview-single-down-80"},
		"preview-single-down-80": {KeyEnter: "three-done"},
	}}
	if err := Drive(context.Background(), one, []askq.Question{pvColor}, []Choice{{Picks: []int{1}}}, fast); err != nil {
		t.Fatalf("a one-question preview picker: %v (keys %q, on %s)", err, one.sent, one.screen)
	}
	if !reflect.DeepEqual(one.sent, []string{"2", KeyEnter}) {
		t.Fatalf("keys %q", one.sent)
	}
}

// TestAPickerMusterCannotReadIsNotBlamedOnThePerson: before the first key, a
// picker showing one of the questions with its rows drawn otherwise than
// muster reads them (here: a preview picker for a question registered without
// its previews — an older registration, or a layout muster does not know) is
// refused as UNRECOGNISED, not as someone answering it in the terminal; and
// nothing is sent.
func TestAPickerMusterCannotReadIsNotBlamedOnThePerson(t *testing.T) {
	plain := pvColor
	plain.Options = []askq.Option{{Label: "Red"}, {Label: "Blue"}, {Label: "Green"}}
	p := &screenPane{t: t, screen: "preview-q1-100"}
	err := Drive(context.Background(), p, []askq.Question{plain, pvSize}, []Choice{{Picks: []int{0}}, {Picks: []int{0}}}, fast)
	if !errors.Is(err, ErrUnrecognised) || errors.Is(err, ErrNotUntouched) || len(p.sent) != 0 {
		t.Fatalf("an unreadable layout: %v, sent %q", err, p.sent)
	}
}

// TestPlanAPreviewQuestion: one option, by a digit that moves the cursor (none
// for the first) and Enter; an answer of one's own is not typeable.
func TestPlanAPreviewQuestion(t *testing.T) {
	steps, err := Plan([]askq.Question{pvColor, pvSize}, []Choice{{Picks: []int{0}}, {Picks: []int{1}}})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, s := range steps {
		keys = append(keys, s.Keys)
	}
	if !reflect.DeepEqual(keys, []string{KeyEnter, "2", "1"}) {
		t.Fatalf("keys %q", keys)
	}
	if _, err := Plan([]askq.Question{pvColor}, []Choice{{Other: "teal"}}); !errors.Is(err, ErrUntypeable) {
		t.Fatalf("own words on a preview question: %v", err)
	}
	if _, err := Plan([]askq.Question{pvColor}, []Choice{{Picks: []int{0}, Other: "teal"}}); !errors.Is(err, ErrUntypeable) {
		t.Fatalf("an option and own words on a preview question: %v", err)
	}
}

// TestTheFakeDrawsThePreviewPickerAsTheCLIDoes: the fake, in the states the
// gate captured at 80 columns, parses to the same screen as the capture.
func TestTheFakeDrawsThePreviewPickerAsTheCLIDoes(t *testing.T) {
	fp := NewFakePicker([]askq.Question{pvPlan, pvSize})
	if real, fake := textOf(Parse(fixture(t, "preview-long-80"))), textOf(Parse(fp.Screen())); !reflect.DeepEqual(real, fake) {
		t.Errorf("preview-long-80: the fake draws\n %+v\nthe CLI drew\n %+v", fake, real)
	}
	one := NewFakePicker([]askq.Question{pvColor})
	for _, step := range []struct{ keys, file string }{{"", "preview-single-80"}, {"2", "preview-single-down-80"}} {
		one.Type(step.keys)
		if real, fake := textOf(Parse(fixture(t, step.file))), textOf(Parse(one.Screen())); !reflect.DeepEqual(real, fake) {
			t.Errorf("%s: the fake draws\n %+v\nthe CLI drew\n %+v", step.file, fake, real)
		}
	}
	// And the fake takes an answer the way the CLI does.
	f := &fakePane{p: NewFakePicker([]askq.Question{pvColor, pvSize, pets})}
	cs := []Choice{{Picks: []int{2}}, {Picks: []int{0}}, {Picks: []int{1}}}
	if err := Drive(context.Background(), f, []askq.Question{pvColor, pvSize, pets}, cs, fast); err != nil {
		t.Fatalf("the fake preview picker: %v\n%s", err, f.p.Screen())
	}
	if got, _, _ := f.p.Result(); !reflect.DeepEqual(got, Answers([]askq.Question{pvColor, pvSize, pets}, cs)) {
		t.Fatalf("submitted %v", got)
	}
}
