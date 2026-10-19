package askdrive

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/askq"
)

// LONG QUESTIONS AND LONG ANSWERS. The long-* and multi-longlabel-* testdata
// screens are REAL: tmux capture-pane -p -J of Claude Code 2.1.289 in a
// scratch session on a private tmux server, synthetic text, at the width the
// name ends in (long-q2-clipped-80x20 is the whole 80x20 pane). They show what
// the short fixtures never did:
//
//   - a question too long for a line is wrapped INSIDE A BOX, "│ " down its
//     left edge (long-q2-80, -120), on the review screen too (" │ ● …");
//   - a long typed answer is wrapped over the "Type something" row's further
//     lines, never truncated or scrolled (long-q2-typed-80, -100 — the 100
//     column one breaks exactly where the live failure's did), mid-word when a
//     word is longer than the line (long-q2-midword-80: "…hyphens-ev" /
//     "erywhere-…");
//   - a long option label is wrapped the same way (multi-longlabel-80);
//   - a picker taller than the pane has its top — the header and the
//     question — scrolled off (long-q2-clipped-80x20).

var (
	lcolor = askq.Question{Question: "Which color do you prefer?", Header: "Color",
		Options: []askq.Option{{Label: "Red", Description: "Warm"}, {Label: "Blue", Description: "Cool"}}}
	lwhere = askq.Question{Question: "The shared widget package renders every tool call the same way regardless of its status, " +
		"so the spinner on a half-finished call never stops (see the notes in #12/#13 and the @example/widgets-chat package). " +
		"Should we fix it upstream, or patch our copy for now and file the upstream change separately?", Header: "Where",
		Options: []askq.Option{{Label: "Upstream", Description: "Package"}, {Label: "Patch", Description: "Local"}}}
	lname = askq.Question{Question: "What should it be called?", Header: "Name",
		Options: []askq.Option{{Label: "Alpha", Description: "First"}, {Label: "Beta", Description: "Second"}}}
	long3 = []askq.Question{lcolor, lwhere, lname}
	lfix  = askq.Question{Question: "Which fixes should we ship?", Header: "Fix", MultiSelect: true,
		Options: []askq.Option{{Label: "Patch the dashboard copy of the widget package now and file the upstream change in the shared repository later", Description: "Dashboard"},
			{Label: "Upstream", Description: "Package"}}}
	// longText is a 135-character answer in the answerer's own words (the live
	// failure's was about 130); longerText runs over several rows, with a URL
	// and a hyphenated word longer than a line.
	longText   = "The build agent needs to support running nightly batch-jobs, alongside the interactive sessions, where we are using models via a proxy."
	longerText = longText + " See https://example.com/a/very/long/path/that/does/not/fit/on/one/line/of/the/terminal/at/all/because/it/keeps/going/and-going-and-going-with-hyphens-everywhere-so-the-wrap-falls-on-one." +
		" Sentence number 1 adds more words so that the typed answer grows well past several screen rows." +
		" Sentence number 2 adds more words so that the typed answer grows well past several screen rows." +
		" Sentence number 3 adds more words so that it grows."
)

// TestParseReadsBoxedAndWrappedScreens: every long capture reads as the
// state it is — the box off the question, wrapped lines put back together —
// and not as its neighbour.
func TestParseReadsBoxedAndWrappedScreens(t *testing.T) {
	cases := []struct {
		file    string
		qs      []askq.Question
		want    Want
		answers map[string]string
	}{
		{"long-q2-80", long3, Want{Kind: Question, Q: 1, Cursor: 1}, nil},
		{"long-q2-120", long3, Want{Kind: Question, Q: 1, Cursor: 1}, nil},
		{"long-q2-typed-80", long3, Want{Kind: Question, Q: 1, Cursor: 3, Text: longText}, nil},
		{"long-q2-typed-100", long3, Want{Kind: Question, Q: 1, Cursor: 3, Text: longText}, nil},
		{"long-q2-midword-80", long3, Want{Kind: Question, Q: 1, Cursor: 3, Text: longerText}, nil},
		{"long-review-80", long3, Want{Kind: Review}, map[string]string{lcolor.Question: "Red", lwhere.Question: longerText, lname.Question: "Alpha"}},
		{"long-review-120", long3, Want{Kind: Review}, map[string]string{lcolor.Question: "Red", lwhere.Question: longerText, lname.Question: "Alpha"}},
		{"multi-longlabel-80", []askq.Question{lfix}, Want{Kind: Question, Q: 0, Cursor: 1}, nil},
		{"multi-longlabel-typed-80", []askq.Question{lfix}, Want{Kind: Question, Q: 0, Cursor: 3, Checked: []int{3}, Text: longText}, nil},
	}
	for _, c := range cases {
		s := Parse(fixture(t, c.file))
		if err := Match(s, c.want, c.qs, c.answers); err != nil {
			t.Errorf("%s: %v\n  parsed %+v", c.file, err, s)
		}
		if strings.ContainsAny(s.Question, vbars) {
			t.Errorf("%s: the box is still in the question: %q", c.file, s.Question)
		}
		for _, r := range s.Review {
			if strings.ContainsAny(r.Question, vbars) {
				t.Errorf("%s: the box is still in a review question: %q", c.file, r.Question)
			}
		}
		// The control: the same screen is NOT a neighbouring state.
		other := c.want
		other.Cursor = (c.want.Cursor + 1) % 4
		if c.want.Kind == Question && Match(s, other, c.qs, nil) == nil {
			t.Errorf("%s: also matched %s", c.file, other)
		}
	}
}

// TestAWrappedAnswerIsStillComparedCharacterForCharacter: reading the text
// row over its lines does not loosen it — a character a person added, changed
// or removed anywhere in a long answer, or an answer cut short, is refused.
func TestAWrappedAnswerIsStillComparedCharacterForCharacter(t *testing.T) {
	want := Want{Kind: Question, Q: 1, Cursor: 3, Text: longText}
	real := fixture(t, "long-q2-typed-100")
	if err := Match(Parse(real), want, long3, nil); err != nil {
		t.Fatalf("the control: the real screen does not match: %v", err)
	}
	for name, scr := range map[string]string{
		"a character added on the second line":  strings.Replace(real, "using models via", "using models viaa", 1),
		"a character changed on the first line": strings.Replace(real, "nightly batch-jobs", "nightly batch-jabs", 1),
		"a line more after the answer":          strings.Replace(real, "     sessions, where we are using models via a proxy.\n", "     sessions, where we are using models via a proxy.\n     zz\n", 1),
		"the second line gone":                  strings.Replace(real, "     sessions, where we are using models via a proxy.\n", "", 1),
	} {
		if scr == real {
			t.Fatalf("%s: the fixture edit did not apply", name)
		}
		if err := Match(Parse(scr), want, long3, nil); err == nil {
			t.Errorf("%s: matched", name)
		}
	}
	// Text it was not asked for: a shorter or longer answer than the screen's.
	for _, text := range []string{longText[:len(longText)-1], longText + "x", "x" + longText, strings.Replace(longText, "nightly batch", "nightlybatch", 1)} {
		w := want
		w.Text = text
		if err := Match(Parse(real), w, long3, nil); err == nil {
			t.Errorf("the screen matched the text %q", text)
		}
	}
	// A break is NOT a free space. This capture breaks between "interactive"
	// and "sessions" on a line that ends well short of the pane's width (91
	// of 100 columns), and the CLI breaks a word only on a FULL line (wrapEq):
	// so the break is a space, and text without one there is not this screen.
	w := want
	w.Text = strings.Replace(longText, "interactive sessions", "interactivesessions", 1)
	if err := Match(Parse(real), w, long3, nil); err == nil {
		t.Error("a break on a line short of the width was read as no space")
	}
	// The placeholder row with anything under it is not the placeholder.
	ph := strings.Replace(fixture(t, "long-q2-80"), "  3. Type something.\n", "  3. Type something.\n     zz\n", 1)
	if err := Match(Parse(ph), Want{Kind: Question, Q: 1, Cursor: 1}, long3, nil); err == nil {
		t.Error("a text row with more under its placeholder matched the placeholder")
	}
}

// TestASpaceTypedAtAWrapPointIsSeen is the #142 audit's case, on the real
// long-q2-typed-100 capture: someone in the terminal types a space inside
// "sessions", and the CLI re-wraps the row at it ("…the interactive sess" /
// "ions, where …"). Every character on screen is still the asked text's, in
// order, so a break read as "no space" would pass it; the first line ends
// short of the pane's width, where the CLI never breaks a word, so it is
// refused. The asked text against the real screen is the control, and the
// mid-word captures (TestParseReadsBoxedAndWrappedScreens: long-q2-midword-80,
// long-review-80 and -120) still read, their broken lines being full.
func TestASpaceTypedAtAWrapPointIsSeen(t *testing.T) {
	want := Want{Kind: Question, Q: 1, Cursor: 3, Text: longText}
	real := fixture(t, "long-q2-typed-100")
	if err := Match(Parse(real), want, long3, nil); err != nil {
		t.Fatalf("the control: the real screen does not match: %v", err)
	}
	scr := strings.Replace(real, "alongside the interactive\n     sessions, where", "alongside the interactive sess\n     ions, where", 1)
	if scr == real {
		t.Fatal("the fixture edit did not apply")
	}
	if err := Match(Parse(scr), want, long3, nil); err == nil {
		t.Fatal("a space typed inside a word, which the CLI wrapped at, matched the asked text")
	}
}

// TestABreakIsFreeOnlyAfterAFullLine pins wrapEq's rule: a break read as no
// space only when the line before it is full (it ends in the pane's last or
// last-but-one column, where the CLI breaks a word); any other break stands
// for a space. Exact and normalised alike.
func TestABreakIsFreeOnlyAfterAFullLine(t *testing.T) {
	for _, exact := range []bool{true, false} {
		if !wrapEq([]string{"hyphens-ev", "erywhere"}, []bool{true}, "hyphens-everywhere", exact) {
			t.Errorf("exact=%v: a mid-word break after a full line was refused", exact)
		}
		if wrapEq([]string{"hyphens-ev", "erywhere"}, []bool{false}, "hyphens-everywhere", exact) {
			t.Errorf("exact=%v: a mid-word break after a short line was read as no space", exact)
		}
		if !wrapEq([]string{"the interactive", "sessions"}, []bool{false}, "the interactive sessions", exact) {
			t.Errorf("exact=%v: a break at a space after a short line was refused", exact)
		}
		if !wrapEq([]string{"the interactive", "sessions"}, []bool{true}, "the interactive sessions", exact) {
			t.Errorf("exact=%v: a break at a space after a full line was refused", exact)
		}
		// No ends known (nil): every break is a space.
		if wrapEq([]string{"hyphens-ev", "erywhere"}, nil, "hyphens-everywhere", exact) {
			t.Errorf("exact=%v: with no widths known, a break was read as no space", exact)
		}
	}
}

// TestTheBoxIsStrippedOnEitherEdge: a left edge (as measured), and the right
// edge and corners too, with the padding inside them; a question that is
// not boxed keeps a leading "│" of its own.
func TestTheBoxIsStrippedOnEitherEdge(t *testing.T) {
	for name, in := range map[string][]string{
		"left edge":        {"│ one two", "│ three"},
		"both edges":       {"│ one two │", "│ three   │"},
		"heavy, corners":   {"┏━━━━━━━━━━┓", "┃ one two  ┃", "┃ three    ┃", "┗━━━━━━━━━━┛"},
		"rounded, corners": {"╭──────────╮", "│ one two  │", "│ three    │", "╰──────────╯"},
	} {
		if got, _ := unbox(in); strings.Join(got, "\n") != "one two\nthree" {
			t.Errorf("%s: %q", name, got)
		}
	}
	// Where each line's text ends, inside a right edge taken off: a line
	// is full by its text, never by the box drawn round it.
	if _, ends := unbox([]string{"│ one two  │", "│ three    │"}); !slices.Equal(ends, []int{9, 7}) {
		t.Errorf("the ends inside a right edge: %v, want [9 7]", ends)
	}
	if _, ends := unbox([]string{"│ one two", "│ three"}); !slices.Equal(ends, []int{9, 7}) {
		t.Errorf("the ends of a left-edged box: %v, want [9 7]", ends)
	}
	if got, _ := unbox([]string{"│ one", "two"}); got[0] != "│ one" {
		t.Errorf("a bar on one line only was stripped: %q", got)
	}
	// ONE line: a box is there only with both edges. A one-line question
	// whose own text starts (or ends) with a bar is not boxed — the CLI
	// boxes a question too long for a line, which is never one line with
	// only a left edge — and keeps it.
	if got, _ := unbox([]string{"│ one two │"}); strings.Join(got, "\n") != "one two" {
		t.Errorf("a one-line box with both edges: %q", got)
	}
	for _, in := range []string{"│ one two", "one two │"} {
		if got, _ := unbox([]string{in}); got[0] != in {
			t.Errorf("a one-line question %q lost its own bar: %q", in, got)
		}
	}
	// A right edge with no left edge is not a frame either.
	if got, _ := unbox([]string{"one │", "two │"}); strings.Join(got, "\n") != "one │\ntwo │" {
		t.Errorf("a right-only edge was stripped: %q", got)
	}
}

// TestAOneLineQuestionThatStartsWithABarIsReadAsAsked: a short question whose
// own text begins "│ " (the real three-q1 capture, its question so) reads as
// asked, bar and all — not as an untouched picker asking something else.
func TestAOneLineQuestionThatStartsWithABarIsReadAsAsked(t *testing.T) {
	scr := strings.Replace(fixture(t, "three-q1"), "Which color do you prefer?", "│ Which color do you prefer?", 1)
	if scr == fixture(t, "three-q1") {
		t.Fatal("the fixture edit did not apply")
	}
	qs := slices.Clone(three)
	qs[0].Question = "│ Which color do you prefer?"
	if err := Match(Parse(scr), Want{Kind: Question, Q: 0, Cursor: 1}, qs, nil); err != nil {
		t.Fatalf("a one-line question starting with a bar: %v (parsed %q)", err, Parse(scr).Question)
	}
	// CONTROL: the question as it would read with the bar taken off is not it.
	if err := Match(Parse(scr), Want{Kind: Question, Q: 0, Cursor: 1}, three, nil); err == nil {
		t.Fatal("the bar-less question matched a screen asking it with a bar")
	}
}

// singleLong is the real long-q2-80 capture as the FIRST question of a
// one-question ask: its header line made the single-select form (single-q's).
func singleLong(t *testing.T) string {
	s := fixture(t, "long-q2-80")
	h := "←  ☒ Color  ☐ Where  ☐ Name  ✔ Submit  →"
	if !strings.Contains(s, h) {
		t.Fatal("long-q2-80 has no header to edit")
	}
	return strings.Replace(s, h, " ☐ Where", 1)
}

// scriptPane shows screens[0] until the first key, then screens[1], ….
type scriptPane struct {
	mu      sync.Mutex
	screens []string
	sent    []string
}

func (p *scriptPane) Capture(context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.screens[min(len(p.sent), len(p.screens)-1)], nil
}
func (p *scriptPane) Send(_ context.Context, k string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, k)
	return nil
}

// TestABoxedQuestionIsAnswered is the live failure: an untouched picker
// asking a long question, boxed, was refused as "someone is answering it in
// the terminal". It is answered.
func TestABoxedQuestionIsAnswered(t *testing.T) {
	qs := []askq.Question{lwhere}
	sp := &scriptPane{screens: []string{singleLong(t), fixture(t, "single-done")}}
	if err := Drive(context.Background(), sp, qs, []Choice{{Picks: []int{0}}}, fast); err != nil {
		t.Fatalf("a boxed question: %v", err)
	}
	if len(sp.sent) != 1 || sp.sent[0] != "1" {
		t.Fatalf("sent %q", sp.sent)
	}
}

// TestAQuestionItCannotReadIsNotBlamedOnTheTerminal: untouched, but the
// question does not read as the registered one — ErrUnreadable, NOT
// ErrNotUntouched, and nothing typed. A picker someone has touched is still
// touched, boxed question or not; a picker whose top is off the pane is
// unreadable, not gone.
func TestAQuestionItCannotReadIsNotBlamedOnTheTerminal(t *testing.T) {
	qs := []askq.Question{lwhere}
	other := lwhere
	other.Question = strings.Replace(lwhere.Question, "upstream change", "upstream fix", 1)
	sp := &staticPane{screen: singleLong(t)}
	err := Drive(context.Background(), sp, []askq.Question{other}, []Choice{{Picks: []int{0}}}, fast)
	if !errors.Is(err, ErrUnreadable) || errors.Is(err, ErrNotUntouched) || sp.sent != 0 {
		t.Fatalf("an untouched picker asking other words: %v, sent %d", err, sp.sent)
	}
	// The same through the fake (the browser suite's host).
	fp := &fakePane{p: NewFakePicker([]askq.Question{lwhere})}
	if err := Drive(context.Background(), fp, []askq.Question{other}, []Choice{{Picks: []int{0}}}, fast); !errors.Is(err, ErrUnreadable) || len(fp.sent) != 0 {
		t.Fatalf("the fake, other words: %v, sent %q", err, fp.sent)
	}
	// Touched: the cursor moved off the first row in the terminal.
	touched := strings.Replace(strings.Replace(singleLong(t), "❯ 1. Upstream", "  1. Upstream", 1), "  2. Patch", "❯ 2. Patch", 1)
	sp = &staticPane{screen: touched}
	if err := Drive(context.Background(), sp, qs, []Choice{{Picks: []int{0}}}, fast); !errors.Is(err, ErrNotUntouched) || errors.Is(err, ErrUnreadable) || sp.sent != 0 {
		t.Fatalf("a touched boxed picker: %v, sent %d", err, sp.sent)
	}
	// Touched AND other words: touched wins — someone is at it.
	sp = &staticPane{screen: touched}
	if err := Drive(context.Background(), sp, []askq.Question{other}, []Choice{{Picks: []int{0}}}, fast); !errors.Is(err, ErrNotUntouched) || sp.sent != 0 {
		t.Fatalf("a touched picker asking other words: %v, sent %d", err, sp.sent)
	}
	// A picker taller than the pane.
	sp = &staticPane{screen: fixture(t, "long-q2-clipped-80x20")}
	if s := Parse(sp.screen); s.Kind != Clipped {
		t.Fatalf("the clipped capture parses as %v", s.Kind)
	}
	if err := Drive(context.Background(), sp, long3, []Choice{{Picks: []int{0}}, {Picks: []int{0}}, {Picks: []int{0}}}, fast); !errors.Is(err, ErrUnreadable) || errors.Is(err, ErrNoPicker) || sp.sent != 0 {
		t.Fatalf("a clipped picker: %v, sent %d", err, sp.sent)
	}
}

// TestALongAnswerIsTyped is the second live failure: a multi-question ask,
// question 2 answered in ~130 characters of its own words — the CLI wraps the
// text row, and the driver read only its first line and stopped "after key 3"
// with the answer half typed. Driven through the fake (which wraps as the CLI
// does — TestTheFakeWrapsAsTheCLIDoes), with a long label and a long review
// too.
func TestALongAnswerIsTyped(t *testing.T) {
	cases := []struct {
		qs []askq.Question
		cs []Choice
	}{
		// The second live failure's own shape: question 2 short, its answer
		// ~130 characters of own words (the text row wraps, nothing boxed).
		{[]askq.Question{lcolor, lname}, []Choice{{Picks: []int{0}}, {Other: longText}}},
		{long3, []Choice{{Picks: []int{0}}, {Other: longText}, {Picks: []int{0}}}},
		{long3, []Choice{{Picks: []int{1}}, {Other: longerText}, {Other: longText}}},
		{[]askq.Question{lfix}, []Choice{{Picks: []int{0}, Other: longText}}},
	}
	for i, c := range cases {
		fp := &fakePane{p: NewFakePicker(c.qs)}
		if err := Drive(context.Background(), fp, c.qs, c.cs, fast); err != nil {
			t.Fatalf("case %d: %v\nkeys %q\nscreen:\n%s", i, err, fp.sent, fp.p.Screen())
		}
		got, cancelled, done := fp.p.Result()
		want := Answers(c.qs, c.cs)
		if !done || cancelled || len(got) != len(want) {
			t.Fatalf("case %d: submitted %v", i, got)
		}
		for q, a := range want {
			if got[q] != a {
				t.Fatalf("case %d: %q answered %q, want %q", i, q, got[q], a)
			}
		}
	}
}

// TestAPersonTypingIntoALongAnswerIsStillSeen: someone in the terminal adds
// to the long answer muster just typed — the drive stops before the Enter,
// nothing is submitted.
func TestAPersonTypingIntoALongAnswerIsStillSeen(t *testing.T) {
	p := NewFakePicker(long3)
	fp := &fakePane{p: p}
	cs := []Choice{{Picks: []int{0}}, {Other: longText}, {Picks: []int{0}}}
	steps, err := Plan(long3, cs)
	if err != nil {
		t.Fatal(err)
	}
	typed := -1
	for i, st := range steps {
		if st.Want.Text == longText {
			typed = i
			break
		}
	}
	if typed < 0 {
		t.Fatal("no step types the text")
	}
	lp := &lookPane{fakePane: fp}
	sawTyped := false
	lp.look = func(int) {
		if len(fp.sent) == typed+1 && !sawTyped {
			sawTyped = true
			p.Type(" zz") // after the text landed, before the next key
		}
	}
	err = Drive(context.Background(), lp, long3, cs, fast)
	if !errors.Is(err, ErrLost) || errors.Is(err, ErrUnreadable) {
		t.Fatalf("a long answer added to in the terminal: %v (sent %q)", err, fp.sent)
	}
	if !sawTyped {
		t.Fatal("the control: the extra characters were never typed")
	}
	if _, _, done := p.Result(); done {
		t.Fatal("submitted an answer someone added to")
	}
	if len(fp.sent) != typed+1 {
		t.Fatalf("keys after the text: %q", fp.sent)
	}
}

// TestAPickerThatScrollsOffMidDriveIsNamed: after the long answer lands, the
// pane shows only the picker's bottom (it grew taller than the pane): the
// drive stops — ErrLost (keys were sent, nothing submitted) AND ErrUnreadable
// (nobody changed it).
func TestAPickerThatScrollsOffMidDriveIsNamed(t *testing.T) {
	cs := []Choice{{Picks: []int{0}}, {Other: longerText}, {Picks: []int{0}}}
	steps, err := Plan(long3, cs)
	if err != nil {
		t.Fatal(err)
	}
	p := NewFakePicker(long3)
	fp := &fakePane{p: p}
	clipped := fixture(t, "long-q2-clipped-80x20")
	cp := &clipPane{fakePane: fp, after: len(steps) - 3, screen: clipped}
	for i, st := range steps {
		if st.Want.Text == longerText {
			cp.after = i + 1
			break
		}
	}
	err = Drive(context.Background(), cp, long3, cs, fast)
	if !errors.Is(err, ErrLost) || !errors.Is(err, ErrUnreadable) {
		t.Fatalf("a picker scrolled off mid-drive: %v (sent %q)", err, fp.sent)
	}
	if _, _, done := p.Result(); done {
		t.Fatal("submitted")
	}
}

// clipPane is fakePane that shows screen once after keys have been sent.
type clipPane struct {
	*fakePane
	after  int
	screen string
}

func (c *clipPane) Capture(ctx context.Context) (string, error) {
	c.mu.Lock()
	n := len(c.sent)
	c.mu.Unlock()
	if n >= c.after {
		return c.screen, nil
	}
	return c.fakePane.Capture(ctx)
}

// TestTheFakeWrapsAsTheCLIDoes: the fake brought to the long captures' states
// reads as the same state (its break points are its own; Parse and Match do
// not depend on them).
func TestTheFakeWrapsAsTheCLIDoes(t *testing.T) {
	fp := NewFakePicker(long3)
	for _, c := range []struct {
		keys, file string
		want       Want
		answers    map[string]string
	}{
		{"1", "long-q2-80", Want{Kind: Question, Q: 1, Cursor: 1}, nil},
		{"3" + longText, "long-q2-typed-80", Want{Kind: Question, Q: 1, Cursor: 3, Text: longText}, nil},
	} {
		fp.Type(c.keys)
		fake, real := Parse(fp.Screen()), Parse(fixture(t, c.file))
		if err := Match(fake, c.want, long3, c.answers); err != nil {
			t.Errorf("%s: the fake: %v\n%s", c.file, err, fp.Screen())
		}
		if err := Match(real, c.want, long3, c.answers); err != nil {
			t.Errorf("%s: the CLI: %v", c.file, err)
		}
		if !strings.Contains(fp.Screen(), "│ ") || len(splitLines(fake.Question)) < 2 {
			t.Errorf("%s: the fake did not box and wrap the question:\n%s", c.file, fp.Screen())
		}
	}
	if r := Parse(fp.Screen()).Rows[2]; len(r.More) == 0 {
		t.Errorf("the fake did not wrap the long answer: %+v", r)
	}
}
