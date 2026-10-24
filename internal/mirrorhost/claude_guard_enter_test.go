package mirrorhost

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// The Claude prompt guard's check BEFORE THE ENTER (runCheckedPaste,
// Tmux.PasteChecked): new code, so these tests do not run against the code
// before the guard — they are pinned by mutation instead (the PR says which).

// scriptedPaste records what runCheckedPaste did to the pane.
type scriptedPaste struct{ did []string }

func (s *scriptedPaste) steps() checkedPasteSteps {
	return checkedPasteSteps{
		paste: func(context.Context) error { s.did = append(s.did, "paste"); return nil },
		enter: func(context.Context) error { s.did = append(s.did, "enter"); return nil },
		sleep: func(context.Context, time.Duration) error { s.did = append(s.did, "sleep"); return nil },
	}
}

// TestACheckedPasteChecksBeforeThePasteAndAgainBeforeTheEnter: a refusal at
// the first check types nothing; one at the second — a dialog that opened while
// the paste was landing — leaves the text pasted, withholds the Enter, and says
// "pasted, not submitted" (never "nothing typed", which would invite a resend
// into the box). Both passing: paste, the delay, Enter.
func TestACheckedPasteChecksBeforeThePasteAndAgainBeforeTheEnter(t *testing.T) {
	refuseAt := func(n int) func(context.Context) error {
		calls := 0
		return func(context.Context) error {
			calls++
			if calls == n {
				return ErrClaudeWaiting
			}
			return nil
		}
	}
	for _, c := range []struct {
		name     string
		refuse   int
		did      []string
		refusal  string
		pastedOK bool
	}{
		{"refused before the paste", 1, nil, mirror.PasteRefusalClaudeWaiting, false},
		{"refused before the Enter", 2, []string{"paste", "sleep"}, mirror.PasteRefusalClaudePasted, true},
		{"admitted", 0, []string{"paste", "sleep", "enter"}, "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := &scriptedPaste{}
			check := refuseAt(c.refuse)
			err := runCheckedPaste(context.Background(), s.steps(), check, check)
			if !slices.Equal(s.did, c.did) {
				t.Fatalf("did %q, want %q", s.did, c.did)
			}
			if got := pasteRefusalOf(err); got != c.refusal {
				t.Fatalf("refusal %q (%v), want %q", got, err, c.refusal)
			}
			var pe *ClaudePastedError
			if errors.As(err, &pe) != c.pastedOK {
				t.Fatalf("pasted-not-submitted = %t (%v), want %t", !c.pastedOK, err, c.pastedOK)
			}
			if c.refuse == 0 && err != nil {
				t.Fatalf("admitted, but %v", err)
			}
		})
	}
	// The words: what the person reads when Enter was withheld.
	want := "Pasted, not submitted: the text is in Claude Code's input box, and muster did not press Enter, because " +
		"Claude is waiting on a permission prompt — answer it first. Check the terminal."
	if got := (&ClaudePastedError{Why: ErrClaudeWaiting}).Error(); got != want {
		t.Fatalf("the pasted-not-submitted words:\n got %q\nwant %q", got, want)
	}
}

// checkedBackend is windowsBackend whose Claude paste is the checked one
// (ClaudeCheckedPaster, through the real runCheckedPaste), and whose paste step
// opens a permission prompt — the window turns waiting — as the text lands.
type checkedBackend struct {
	*windowsBackend
	enters *[]string
	// after is the window list once the text has landed: a prompt opened
	// (guardWindows(waiting)) unless set.
	after []WindowInfo
}

func (c checkedBackend) PasteChecked(ctx context.Context, p ClaudePane, text string, before, beforeEnter func(context.Context) error) error {
	return runCheckedPaste(ctx, checkedPasteSteps{
		paste: func(ctx context.Context) error {
			if err := c.PastePane(ctx, p.Pane, text); err != nil {
				return err
			}
			if c.after != nil {
				c.set(c.after...)
			} else {
				c.set(guardWindows(mirror.StatusWaiting)...)
			}
			return nil
		},
		enter: func(context.Context) error { *c.enters = append(*c.enters, p.Pane); return nil },
		sleep: func(context.Context, time.Duration) error { return nil },
	}, before, beforeEnter)
}

// TestAPermissionPromptThatOpensAsThePasteLandsGetsNoEnter: the host's
// checked paste re-reads the window before the Enter; the prompt that opened
// while the text was landing is seen, the Enter is withheld, and muster is
// told "pasted, not submitted".
func TestAPermissionPromptThatOpensAsThePasteLandsGetsNoEnter(t *testing.T) {
	d := newDrivenMuster(t)
	b := &windowsBackend{windows: guardWindows(mirror.StatusIdle)}
	var enters []string
	d.run(t, wrapped{checkedBackend{windowsBackend: b, enters: &enters}}, nil, true)
	a := pasteAnswer(t, d, mirror.Msg{ID: 1, Session: "scratch2", Window: "@7", Text: "hello"})
	if a.Refusal != mirror.PasteRefusalClaudePasted || a.Error == "" {
		t.Fatalf("answered %+v; want pasted-not-submitted", a)
	}
	if _, _, _, p, _ := b.snapshot(); !slices.Equal(p, []string{"%21 hello"}) {
		t.Fatalf("pasted %q; want the text, once", p)
	}
	if len(enters) != 0 {
		t.Fatalf("Enter was pressed into the prompt: %q", enters)
	}
}

// TestAClaudePaneThatMovedAsThePasteLandedGetsNoEnter: the check before the
// Enter re-resolves the window's Claude pane, and one that is no longer the
// pane the text went into (a split moved Claude Code to %22, idle) withholds
// the Enter — the Enter would go to whatever now runs in %21.
func TestAClaudePaneThatMovedAsThePasteLandedGetsNoEnter(t *testing.T) {
	d := newDrivenMuster(t)
	b := &windowsBackend{windows: guardWindows(mirror.StatusIdle)}
	moved := guardWindows(mirror.StatusIdle)
	moved[1].Pane, moved[1].Claude.Pane = "%22", "%22"
	var enters []string
	d.run(t, wrapped{checkedBackend{windowsBackend: b, enters: &enters, after: moved}}, nil, true)
	a := pasteAnswer(t, d, mirror.Msg{ID: 1, Session: "scratch2", Window: "@7", Text: "hello"})
	if a.Refusal != mirror.PasteRefusalClaudePasted || len(enters) != 0 {
		t.Fatalf("answered %+v, Enter pressed into %q; want pasted-not-submitted and no Enter", a, enters)
	}
}
