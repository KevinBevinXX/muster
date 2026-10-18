package askdrive

import (
	"context"
	"errors"
	"fmt"
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
)

// Options tunes a drive.
type Options struct {
	// Settle bounds the wait for one key's screen (default 3 s); Poll is how
	// often the pane is re-read meanwhile (default 40 ms).
	Settle, Poll time.Duration
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
	answers := Answers(qs, choices)
	want := Start()
	for i, st := range steps {
		// 🔴 RE-READ RIGHT BEFORE THE KEY: the terminal may have answered (or
		// moved) the picker since the last look.
		scr, err := p.Capture(ctx)
		if err != nil {
			return err
		}
		if err := Match(Parse(scr), want, qs, answers); err != nil {
			if i == 0 {
				if Parse(scr).Kind == None {
					return fmt.Errorf("%w: %v", ErrNoPicker, err)
				}
				return fmt.Errorf("%w: %v", ErrNotUntouched, err)
			}
			return fmt.Errorf("%w (before key %d, wanted %s): %v", ErrLost, i+1, want, err)
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
