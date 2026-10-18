package askdrive

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
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
)

// Activity is a Pane that can tell whether a person typed into it lately.
type Activity interface {
	// RecentInput reports whether someone typed into the pane's terminal in
	// the last couple of seconds — the driver's own keys not counted.
	RecentInput(ctx context.Context) (bool, error)
}

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
}

// Drive types choices into the picker in p, verifying the screen before and
// after every key (see the package doc). nil means every key landed and the
// final screen showed the picker gone — the CLI took the answers; that it took
// THESE answers is for the caller to confirm (the transcript's result).
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
		// 🔴 SOMEONE TYPING IN THE TERMINAL: their key may not be drawn yet.
		if act != nil {
			typing, err := act.RecentInput(ctx)
			if err != nil {
				return err
			}
			if typing {
				return refuse(errors.New("someone typed in the terminal in the last couple of seconds"))
			}
		}
		// 🔴 RE-READ RIGHT BEFORE THE KEY, TWICE, Steady apart: the terminal
		// may have answered (or moved) the picker since the last look, or be
		// drawing a key someone just typed.
		scr, err := p.Capture(ctx)
		if err != nil {
			return err
		}
		if err := Match(Parse(scr), want, qs, answers); err != nil {
			if i == 0 {
				switch Parse(scr).Kind {
				case None:
					return fmt.Errorf("%w: %v", ErrNoPicker, err)
				case Unknown:
					return fmt.Errorf("%w: %v", ErrUnrecognised, err)
				}
			}
			return refuse(err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(opt.Steady):
		}
		again, err := p.Capture(ctx)
		if err != nil {
			return err
		}
		// The PICKER must be unchanged; the rest of the pane (the CLI's own
		// status lines) may change on its own.
		if !reflect.DeepEqual(Parse(again), Parse(scr)) {
			return refuse(errors.New("the screen changed while it was being read — someone is typing in the terminal"))
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

// Match reports why s is not the screen w describes for qs (nil when it is).
// answers is what the review screen must list (Answers).
func Match(s Screen, w Want, qs []askq.Question, answers map[string]string) error {
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
			if norm(s.Review[i].Question) != norm(q.Question) {
				return fmt.Errorf("review item %d is %q, not %q", i+1, s.Review[i].Question, q.Question)
			}
			if norm(s.Review[i].Answer) != norm(answers[q.Question]) {
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
	if norm(s.Question) != norm(q.Question) {
		return fmt.Errorf("the screen asks %q, not %q", s.Question, q.Question)
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
			if norm(r.Label) != norm(q.Options[j].Label) {
				return fmt.Errorf("row %d is %q, not %q", j+1, r.Label, q.Options[j].Label)
			}
		} else if w.Text != "" {
			if r.Label != w.Text {
				return fmt.Errorf("the text row holds %q, not %q", r.Label, w.Text)
			}
		} else if r.Label != "Type something" && r.Label != "Type something." {
			return fmt.Errorf("the text row holds %q, not its placeholder", r.Label)
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
