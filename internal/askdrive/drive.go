package askdrive

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/askq"
)

// Pane is a terminal pane the driver reads and types into.
type Pane interface {
	// Capture is the pane's screen, wrapped lines joined (tmux capture-pane
	// -p -J).
	Capture(ctx context.Context) (string, error)
	// Send writes keys to the program in the pane as typed bytes.
	Send(ctx context.Context, keys string) error
}

var (
	// ErrNoPicker: the question's picker is not on screen — it was answered or
	// cancelled in the terminal, or this is not the pane showing it.
	ErrNoPicker = errors.New("askdrive: the question's picker is not on screen")
	// ErrNotUntouched: the picker shows this question but someone has started
	// answering it in the terminal (or it shows another question).
	ErrNotUntouched = errors.New("askdrive: the picker is not untouched — someone is answering it in the terminal")
	// ErrLost: mid-drive, the screen is not what the last key should have
	// produced. Nothing more is sent; the picker is left unsubmitted.
	ErrLost = errors.New("askdrive: the picker did not show what the last key should have produced")
	// ErrUnrecognised: the pane shows something like the picker that does not
	// parse (Kind Unknown) — Claude Code's layout has probably changed.
	// Nothing is sent.
	ErrUnrecognised = errors.New("askdrive: could not recognise the terminal's picker (Claude Code may have changed)")
	// ErrUnreadable: muster could not read the question off the screen as the
	// one it registered — NOT that anyone is answering it. Before the first
	// key: the picker is untouched (every option, the cursor, the tabs as
	// expected) but its question's text does not read as the registered one;
	// or the picker is taller than the pane and its question is off screen
	// (Clipped). Nothing was sent. After a key it is wrapped in ErrLost too:
	// the picker went off the top of the pane mid-way, keys were sent, and
	// nothing was submitted.
	ErrUnreadable = errors.New("askdrive: could not read the question off the terminal's screen as the one muster registered")
)

// Activity is a Pane that can tell when a person last gave its terminal
// input.
type Activity interface {
	// LastInput is when a client that can type into the pane last had input
	// (zero: never) — the driver's own keys not counted. It need not be a
	// KEYSTROKE: tmux's only such clock (#{client_activity}) also moves for a
	// focus event or the mouse, and keeps whole seconds, rounded down
	// (Options.Grain). So the driver never refuses on it alone: it waits until
	// any key from that moment would be drawn, and looks again.
	LastInput(ctx context.Context) (time.Time, error)
}

// maxInputWaits bounds how often one key waits out new input in the
// terminal before the driver gives up: someone is using it.
const maxInputWaits = 4

// SteadyDefault is Options.Steady's default: over three times the worst
// redraw lag measured (72 ms, every core busy — see the package doc).
const SteadyDefault = 250 * time.Millisecond

// Options tunes a drive.
type Options struct {
	// Settle bounds the wait for one key's screen (default 3 s); Poll is how
	// often the pane is re-read meanwhile (default 40 ms).
	Settle, Poll time.Duration
	// Steady is how far apart the two looks before every key are (default
	// SteadyDefault): a key typed in the terminal but not drawn yet at the
	// first look is drawn by the second.
	Steady time.Duration
	// Grain is Activity's resolution (default 1 s: tmux's client_activity is
	// whole seconds, rounded down) — input stamped t happened before t+Grain.
	Grain time.Duration
}

// Drive types choices into the picker in p, verifying the screen before and
// after every key (see the package doc). nil means every key landed and the
// final screen showed the picker gone — the CLI took the answers; that it took
// THESE answers is for the caller to confirm (the transcript's result).
//
// 🔴 A REFUSAL AFTER THE FIRST KEY (ErrLost) LEAVES WHAT WAS TYPED IN THE
// PICKER, unsubmitted — a ticked box, a moved cursor, part or all of an
// answer's own words in its "Type something" row. Drive never sends a key to
// take it back: clearing it means typing into a picker it has just failed to
// recognise, the very thing it refuses. The person finishes, corrects or
// cancels it in the terminal — or answers again from muster, whose next drive
// continues it (DriveFrom).
func Drive(ctx context.Context, p Pane, qs []askq.Question, choices []Choice, opt Options) error {
	_, err := DriveFrom(ctx, p, qs, choices, nil, opt)
	return err
}

// Progress is how far a drive got (askq.Progress): the choices it was typing,
// and how many keys of their Plan it sent.
type Progress = askq.Progress

// ErrOwnDifferent: the picker holds part of an answer muster typed earlier
// (DriveFrom's prior) that differs from this one. Nothing was sent.
var ErrOwnDifferent = errors.New("askdrive: the picker holds the answer muster started typing earlier, which differs from this one")

// DriveFrom is Drive for a picker an earlier drive may have left part of an
// answer in: prior is what that drive sent (nil: nothing). It returns how many
// keys of choices' Plan the picker has received from muster, this drive's and
// any it continued — what the next drive's prior is.
//
// 🔴 IT CONTINUES ONLY MUSTER'S OWN PARTIAL ANSWER. Before its first key it
// reads the picker; when that is EXACTLY the screen prior's own keys led to —
// after some k of them, k at most the keys prior sent — and choices' plan
// begins with those same k keys, it continues from key k+1. A picker in any
// other state is refused as before: untouched is required (ErrNotUntouched),
// and a picker in a state prior's keys reached that choices' plan does not
// pass through holds muster's earlier, different answer (ErrOwnDifferent).
//
// What the screen cannot show — WHICH answer an answered question was given —
// is what prior vouches for: muster's own keys answered it, and the screen
// confirmed each. Someone who changed such an answer in the terminal is caught
// before the submit: the review screen lists every answer, and Drive submits
// it only when they are exactly choices'.
func DriveFrom(ctx context.Context, p Pane, qs []askq.Question, choices []Choice, prior *Progress, opt Options) (int, error) {
	steps, err := Plan(qs, choices)
	if err != nil {
		return 0, err
	}
	if opt.Settle <= 0 {
		opt.Settle = 3 * time.Second
	}
	if opt.Poll <= 0 {
		opt.Poll = 40 * time.Millisecond
	}
	if opt.Steady <= 0 {
		opt.Steady = SteadyDefault
	}
	if opt.Grain <= 0 {
		opt.Grain = time.Second
	}
	answers := Answers(qs, choices)
	first := 0
	if prior != nil && prior.Keys > 0 {
		scr, err := p.Capture(ctx)
		if err != nil {
			return 0, err
		}
		if first, err = resumeAt(Parse(scr), qs, steps, prior); err != nil {
			return 0, err
		}
	}
	want := At(steps, first)
	act, _ := p.(Activity)
	for i := first; i < len(steps); i++ {
		st := steps[i]
		// refuse is the error for a screen (or a person) not as wanted before
		// key i: before this drive's first key nothing of ITS is in the
		// picker.
		refuse := func(why error) error {
			if i == first {
				return fmt.Errorf("%w: %v", ErrNotUntouched, why)
			}
			return fmt.Errorf("%w (before key %d, wanted %s): %v", ErrLost, i+1, want, why)
		}
		// 🔴 RE-READ RIGHT BEFORE THE KEY, TWICE, Steady apart: the terminal
		// may have answered (or moved) the picker since the last look, or be
		// drawing a key someone just typed. Then, right before the key, the
		// terminal's last input: if a key from that moment might not be drawn
		// by the second look, wait until it would be and look again — input
		// that did not change the picker (a focus event, the mouse, a key the
		// picker ignores) does not stop the drive; a key that did, does.
		for waits := 0; ; waits++ {
			scr, err := p.Capture(ctx)
			if err != nil {
				return i, err
			}
			if err := Match(Parse(scr), want, qs, answers); err != nil {
				s := Parse(scr)
				if i == first {
					switch {
					case s.Kind == None:
						return i, fmt.Errorf("%w: %v", ErrNoPicker, err)
					case s.Kind == Unknown:
						return i, fmt.Errorf("%w: %v", ErrUnrecognised, err)
					case s.Kind == Clipped:
						return i, fmt.Errorf("%w: %v", ErrUnreadable, err)
					// Untouched in every way the screen can show, only the
					// question's words differ: muster misread (or was told)
					// the question — nobody is answering it.
					case s.Kind == Question && match(s, want, qs, answers, false) == nil:
						return i, fmt.Errorf("%w: %v", ErrUnreadable, err)
					// One of these questions, its rows not drawn as its are —
					// in ANY state: not someone answering it, a picker muster
					// cannot read (a layout it does not know).
					case s.Kind == Question && errors.Is(shownLayout(s, qs), errLayout):
						return i, fmt.Errorf("%w: %v", ErrUnrecognised, shownLayout(s, qs))
					}
				} else if s.Kind == Clipped {
					return i, fmt.Errorf("%w (before key %d, wanted %s): %w: %v", ErrLost, i+1, want, ErrUnreadable, err)
				}
				return i, refuse(err)
			}
			if err := sleep(ctx, opt.Steady); err != nil {
				return i, err
			}
			second := time.Now()
			again, err := p.Capture(ctx)
			if err != nil {
				return i, err
			}
			// The PICKER must be unchanged; the rest of the pane (the CLI's own
			// status lines) may change on its own.
			if !reflect.DeepEqual(Parse(again), Parse(scr)) {
				return i, refuse(errors.New("the screen changed while it was being read — someone is typing in the terminal"))
			}
			if act == nil {
				break
			}
			last, err := act.LastInput(ctx)
			if err != nil {
				return i, err
			}
			// Input stamped last happened before last+Grain and is drawn
			// within Steady of it: the second look saw it if it was taken
			// after that.
			drawnBy := last.Add(opt.Grain + opt.Steady)
			if last.IsZero() || !second.Before(drawnBy) {
				break
			}
			if waits >= maxInputWaits {
				return i, refuse(errors.New("the terminal keeps getting input — someone is using it"))
			}
			if err := sleep(ctx, time.Until(drawnBy)); err != nil {
				return i, err
			}
		}
		if err := p.Send(ctx, st.Keys); err != nil {
			// Whether any of it arrived is not known: count it.
			return i + 1, err
		}
		deadline := time.Now().Add(opt.Settle)
		for {
			scr, err := p.Capture(ctx)
			if err != nil {
				return i + 1, err
			}
			merr := Match(Parse(scr), st.Want, qs, answers)
			if merr == nil {
				break
			}
			if time.Now().After(deadline) {
				if Parse(scr).Kind == Clipped {
					return i + 1, fmt.Errorf("%w (after key %d, wanted %s): %w: %v", ErrLost, i+1, st.Want, ErrUnreadable, merr)
				}
				return i + 1, fmt.Errorf("%w (after key %d, wanted %s): %v", ErrLost, i+1, st.Want, merr)
			}
			select {
			case <-ctx.Done():
				return i + 1, ctx.Err()
			case <-time.After(opt.Poll):
			}
		}
		want = st.Want
	}
	return len(steps), nil
}

// resumeAt is the key a drive of steps starts at, given that the picker shows
// s and an earlier drive sent prior: the number of keys of prior's plan whose
// screen s is — the most of them, at most prior.Keys — when steps begin with
// those same keys; 0 when s is no screen prior's keys led to (the first look
// then judges it as any picker). ErrOwnDifferent when s is a screen of prior's
// keys that steps do not pass through.
func resumeAt(s Screen, qs []askq.Question, steps []Step, prior *Progress) (int, error) {
	old, err := Plan(qs, prior.Choices)
	if err != nil {
		return 0, nil // not an answer to these questions: nothing to continue
	}
	// same is how many keys the two plans share.
	same := 0
	for same < len(old) && same < len(steps) && old[same].Keys == steps[same].Keys && reflect.DeepEqual(old[same].Want, steps[same].Want) {
		same++
	}
	// The review screen lists the answers, and prior's are what it must list
	// to be prior's screen.
	priorAnswers := Answers(qs, prior.Choices)
	for k := min(prior.Keys, len(old)); k > 0; k-- {
		w := At(old, k)
		if w.Kind == None || Match(s, w, qs, priorAnswers) != nil {
			continue
		}
		if k > same {
			return 0, fmt.Errorf("%w: it shows %s", ErrOwnDifferent, w)
		}
		return k, nil
	}
	return 0, nil
}

// shownLayout reports why the question screen s is not one of qs as drawn
// (errLayout), nil when its rows are drawn as its question's — the question
// whose text it shows; nil too when it shows none of theirs.
func shownLayout(s Screen, qs []askq.Question) error {
	for _, q := range qs {
		if wrapEq(splitLines(s.Question), fullLines(s.QuestionEnds, s.Width), q.Question, false) {
			return layoutMatch(s, q)
		}
	}
	return nil
}

// sleep waits d, or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// Match reports why s is not the screen w describes for qs (nil when it is).
// answers is what the review screen must list (Answers).
func Match(s Screen, w Want, qs []askq.Question, answers map[string]string) error {
	return match(s, w, qs, answers, true)
}

// errQuestionText: the screen is the wanted one but for the question's text.
var errQuestionText = errors.New("the question's text differs")

// match is Match; with question false it does not compare the shown
// question's text (and only that) — how Drive tells "this picker, untouched,
// but its question does not read as the one registered" (ErrUnreadable) from
// a picker someone has touched.
func match(s Screen, w Want, qs []askq.Question, answers map[string]string, question bool) error {
	if s.Kind == Clipped && w.Kind != None {
		return errors.New("the picker's top is off the pane (it is taller than the terminal): its question cannot be read")
	}
	switch w.Kind {
	case None:
		if s.Kind != None {
			return errors.New("the picker is still on screen")
		}
		return nil
	case Review:
		if s.Kind != Review {
			return errors.New("not the review screen")
		}
		if len(s.Review) != len(qs) {
			return fmt.Errorf("the review lists %d answers for %d questions", len(s.Review), len(qs))
		}
		for i, q := range qs {
			if !wrapEq(splitLines(s.Review[i].Question), fullLines(s.Review[i].QuestionEnds, s.Width), q.Question, false) {
				return fmt.Errorf("review item %d is %q, not %q", i+1, s.Review[i].Question, q.Question)
			}
			if !wrapEq(splitLines(s.Review[i].Answer), fullLines(s.Review[i].AnswerEnds, s.Width), answers[q.Question], false) {
				return fmt.Errorf("review answer %d is %q, not %q", i+1, s.Review[i].Answer, answers[q.Question])
			}
		}
		if s.ReviewCursor != 1 {
			return fmt.Errorf("the review cursor is on row %d, not Submit answers", s.ReviewCursor)
		}
		return tabsMatch(s, qs, len(qs))
	}
	if s.Kind != Question {
		return errors.New("no question on screen")
	}
	if w.Q < 0 || w.Q >= len(qs) {
		return fmt.Errorf("no question %d", w.Q+1)
	}
	q := qs[w.Q]
	if question && !wrapEq(splitLines(s.Question), fullLines(s.QuestionEnds, s.Width), q.Question, false) {
		return fmt.Errorf("%w: the screen asks %q, not %q", errQuestionText, s.Question, q.Question)
	}
	if err := tabsMatch(s, qs, w.Q); err != nil {
		return err
	}
	if err := layoutMatch(s, q); err != nil {
		return err
	}
	k := len(q.Options)
	for j, r := range s.Rows {
		if checked := slices.Contains(w.Checked, j+1); q.MultiSelect && (r.Box == "[✔]") != checked {
			return fmt.Errorf("row %d checked=%v, want %v", j+1, r.Box == "[✔]", checked)
		}
		// The options' labels are layoutMatch's; the text row's text is the
		// state's.
		switch {
		case j < k:
		case w.Text != "":
			// The typed text, wrapped over the row's lines when long (the CLI
			// never truncates or scrolls it: measured 2.1.289, 650+ characters
			// drawn in full, wrapped).
			if !wrapEq(textLines(r), fullLines(r.Ends, s.Width), w.Text, true) {
				return fmt.Errorf("the text row holds %q, not %q", strings.Join(textLines(r), "\n"), w.Text)
			}
		case (r.Label != "Type something" && r.Label != "Type something.") || len(r.More) > 0:
			return fmt.Errorf("the text row holds %q, not its placeholder", strings.Join(textLines(r), "\n"))
		}
		if r.Cursor != (w.Cursor == j+1) {
			return fmt.Errorf("the cursor is not where wanted (row %d cursor=%v)", j+1, r.Cursor)
		}
	}
	if w.Cursor == 0 && !s.NavCursor {
		return errors.New("the cursor is not on the Next/Submit row")
	}
	if w.Cursor != 0 && s.NavCursor {
		return errors.New("the cursor is on the Next/Submit row")
	}
	return nil
}

// errLayout marks a question screen whose rows are not drawn as question q's
// would be — whatever state it is in: the wrong number of rows, other
// labels, checkboxes where there should be none (or none where there should
// be), previews where there should be none. Before the first key it is not
// someone answering in the terminal; it is a picker muster cannot read
// (ErrUnrecognised).
var errLayout = errors.New("the picker's rows are not drawn as this question's")

// layoutMatch reports why the rows of question screen s are not question q's
// (nil when they are), in any state: one numbered row per option and then the
// "Type something" row (none on a question with previews), with a checkbox on
// each (multi-select) or none, labelled as q's options.
func layoutMatch(s Screen, q askq.Question) error {
	k := len(q.Options)
	pv := q.Previews()
	switch {
	case pv && s.PreviewCol == 0:
		return fmt.Errorf("%w: no preview box beside the options of a question with previews", errLayout)
	case !pv && s.PreviewCol > 0:
		return fmt.Errorf("%w: a preview box beside the options of a question without previews", errLayout)
	}
	want, what := k+1, "options and the text row"
	if pv {
		want, what = k, "options (a question with previews has no text row)"
	}
	if len(s.Rows) != want {
		return fmt.Errorf("%w: %d rows, not %d %s", errLayout, len(s.Rows), k, what)
	}
	// A preview column wraps a label within itself, so its lines are never as
	// wide as the pane: none is full (fullLines).
	width := s.Width
	if pv {
		width = 0
	}
	for j, r := range s.Rows {
		switch {
		case r.Num != j+1:
			return fmt.Errorf("%w: row %d is numbered %d", errLayout, j+1, r.Num)
		case q.MultiSelect && r.Box != "[ ]" && r.Box != "[✔]":
			return fmt.Errorf("%w: row %d has no checkbox", errLayout, j+1)
		case !q.MultiSelect && r.Box != "":
			return fmt.Errorf("%w: row %d has a checkbox on a single-select question", errLayout, j+1)
		case j < k && !labelMatch(r, width, q.Options[j].Label):
			return fmt.Errorf("%w: row %d is %q, not %q", errLayout, j+1, r.Label, q.Options[j].Label)
		}
	}
	return nil
}

// tabsMatch checks the header: one tab per question (headers as asked), the
// questions before current answered and those after it not.
func tabsMatch(s Screen, qs []askq.Question, current int) error {
	if len(s.Tabs) != len(qs) {
		return fmt.Errorf("%d tabs for %d questions", len(s.Tabs), len(qs))
	}
	for j, t := range s.Tabs {
		if qs[j].Header != "" && norm(t.Header) != norm(qs[j].Header) {
			return fmt.Errorf("tab %d is %q, not %q", j+1, t.Header, qs[j].Header)
		}
		switch {
		case j < current && !t.Answered:
			return fmt.Errorf("question %d is not answered", j+1)
		case j > current && t.Answered:
			return fmt.Errorf("question %d is already answered", j+1)
		}
	}
	return nil
}
