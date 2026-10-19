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
// cancels it in the terminal; the card says it may be partly filled in
// (internal/api askTypeFailure).
func Drive(ctx context.Context, p Pane, qs []askq.Question, choices []Choice, opt Options) error {
	steps, err := Plan(qs, choices)
	if err != nil {
		return err
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
	want := Start()
	act, _ := p.(Activity)
	for i, st := range steps {
		// refuse is the error for a screen (or a person) not as wanted before
		// key i: before the first key nothing of ours is in the picker.
		refuse := func(why error) error {
			if i == 0 {
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
				return err
			}
			if err := Match(Parse(scr), want, qs, answers); err != nil {
				s := Parse(scr)
				if i == 0 {
					switch {
					case s.Kind == None:
						return fmt.Errorf("%w: %v", ErrNoPicker, err)
					case s.Kind == Unknown:
						return fmt.Errorf("%w: %v", ErrUnrecognised, err)
					case s.Kind == Clipped:
						return fmt.Errorf("%w: %v", ErrUnreadable, err)
					// Untouched in every way the screen can show, only the
					// question's words differ: muster misread (or was told)
					// the question — nobody is answering it.
					case s.Kind == Question && match(s, want, qs, answers, false) == nil:
						return fmt.Errorf("%w: %v", ErrUnreadable, err)
					}
				} else if s.Kind == Clipped {
					return fmt.Errorf("%w (before key %d, wanted %s): %w: %v", ErrLost, i+1, want, ErrUnreadable, err)
				}
				return refuse(err)
			}
			if err := sleep(ctx, opt.Steady); err != nil {
				return err
			}
			second := time.Now()
			again, err := p.Capture(ctx)
			if err != nil {
				return err
			}
			// The PICKER must be unchanged; the rest of the pane (the CLI's own
			// status lines) may change on its own.
			if !reflect.DeepEqual(Parse(again), Parse(scr)) {
				return refuse(errors.New("the screen changed while it was being read — someone is typing in the terminal"))
			}
			if act == nil {
				break
			}
			last, err := act.LastInput(ctx)
			if err != nil {
				return err
			}
			// Input stamped last happened before last+Grain and is drawn
			// within Steady of it: the second look saw it if it was taken
			// after that.
			drawnBy := last.Add(opt.Grain + opt.Steady)
			if last.IsZero() || !second.Before(drawnBy) {
				break
			}
			if waits >= maxInputWaits {
				return refuse(errors.New("the terminal keeps getting input — someone is using it"))
			}
			if err := sleep(ctx, time.Until(drawnBy)); err != nil {
				return err
			}
		}
		if err := p.Send(ctx, st.Keys); err != nil {
			return err
		}
		deadline := time.Now().Add(opt.Settle)
		for {
			scr, err := p.Capture(ctx)
			if err != nil {
				return err
			}
			merr := Match(Parse(scr), st.Want, qs, answers)
			if merr == nil {
				break
			}
			if time.Now().After(deadline) {
				if Parse(scr).Kind == Clipped {
					return fmt.Errorf("%w (after key %d, wanted %s): %w: %v", ErrLost, i+1, st.Want, ErrUnreadable, merr)
				}
				return fmt.Errorf("%w (after key %d, wanted %s): %v", ErrLost, i+1, st.Want, merr)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(opt.Poll):
			}
		}
		want = st.Want
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
	k := len(q.Options)
	if len(s.Rows) != k+1 {
		return fmt.Errorf("%d rows, not %d options and the text row", len(s.Rows), k)
	}
	for j, r := range s.Rows {
		if r.Num != j+1 {
			return fmt.Errorf("row %d is numbered %d", j+1, r.Num)
		}
		checked := slices.Contains(w.Checked, j+1)
		switch {
		case q.MultiSelect && r.Box != "[ ]" && r.Box != "[✔]":
			return fmt.Errorf("row %d has no checkbox", j+1)
		case !q.MultiSelect && r.Box != "":
			return fmt.Errorf("row %d has a checkbox on a single-select question", j+1)
		case q.MultiSelect && (r.Box == "[✔]") != checked:
			return fmt.Errorf("row %d checked=%v, want %v", j+1, r.Box == "[✔]", checked)
		}
		if j < k {
			if !labelMatch(r, s.Width, q.Options[j].Label) {
				return fmt.Errorf("row %d is %q, not %q", j+1, r.Label, q.Options[j].Label)
			}
		} else if w.Text != "" {
			// The typed text, wrapped over the row's lines when long (the CLI
			// never truncates or scrolls it: measured 2.1.289, 650+ characters
			// drawn in full, wrapped).
			if !wrapEq(textLines(r), fullLines(r.Ends, s.Width), w.Text, true) {
				return fmt.Errorf("the text row holds %q, not %q", strings.Join(textLines(r), "\n"), w.Text)
			}
		} else if (r.Label != "Type something" && r.Label != "Type something.") || len(r.More) > 0 {
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
