package mirrorhost

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// The Claude prompt guard, host half (claude_guard.go). These tests use only
// what the host already had before the guard — the window list's Claude
// status, a backend method a wrapper can add — so that they compile, and FAIL,
// against the code before it. The refusal kind is its wire literal.

// guardWindows is one session: a shell in @4 and Claude Code in @7 (pane %21)
// with status, since t1.
func guardWindows(status string) []WindowInfo {
	return []WindowInfo{
		{Session: "scratch2", ID: "@4", Index: 1, Pane: "%14", PanePID: 1, Command: "zsh"},
		{Session: "scratch2", ID: "@7", Index: 8, Active: true, Pane: "%21", PanePID: 2, Command: "claude",
			Claude: &ClaudePane{Pane: "%21", Gen: "g7", Status: status, StatusSince: 1792000001111}},
	}
}

// screenBackend is windowsBackend with a Claude pane screen
// (ClaudeScreenBackend), read through screen.
type screenBackend struct {
	*windowsBackend
	screen func(pane string) (string, error)
}

func (s screenBackend) ClaudeScreen(_ context.Context, pane string) (string, error) {
	return s.screen(pane)
}

// pasteAnswer sends m and waits for its pasted answer.
func pasteAnswer(t *testing.T, d *drivenMuster, m mirror.Msg) mirror.Msg {
	t.Helper()
	m.Type = mirror.TypePaste
	d.send(t, m)
	var out mirror.Msg
	waitUntil(t, "the paste's answer", func() bool {
		for _, a := range d.of(mirror.TypePasted) {
			if a.ID == m.ID {
				out = a
				return true
			}
		}
		return false
	})
	return out
}

// TestAPasteIntoAClaudePaneThatTurnedWaitingAfterItsLastReportTypesNothing is
// the race the host's half closes: the host reported the window IDLE (its
// hello), so muster — the hub, the send queue — decided to send; a permission
// prompt then opened, and the window's status is WAITING when the paste
// arrives. The host reads the status at the moment of typing and refuses:
// nothing is typed, and the refusal names the kind muster holds a queued
// message on. Controls: the same paste once the prompt is answered (idle
// again) is typed; and the Terminal view's composer into the waiting pane — the
// person typing where they look — is not gated.
func TestAPasteIntoAClaudePaneThatTurnedWaitingAfterItsLastReportTypesNothing(t *testing.T) {
	d := newDrivenMuster(t)
	b := &windowsBackend{windows: guardWindows(mirror.StatusIdle)}
	d.run(t, wrapped{b}, nil, true)
	pasted := func() []string { _, _, _, p, _ := b.snapshot(); return p }

	// The prompt opens after the hello muster decided on.
	b.set(guardWindows(mirror.StatusWaiting)...)
	a := pasteAnswer(t, d, mirror.Msg{ID: 1, Session: "scratch2", Window: "@7", Text: "1"})
	if a.Error == "" || a.Refusal != "claude-waiting" {
		t.Fatalf("a paste into a Claude pane waiting on a prompt was answered %+v; want refused as claude-waiting", a)
	}
	if got := pasted(); len(got) != 0 {
		t.Fatalf("a paste into a Claude pane waiting on a prompt typed %q", got)
	}

	// Control: the Terminal composer into the same pane, waiting.
	if a := pasteAnswer(t, d, mirror.Msg{ID: 3, Session: "scratch2", Window: "@7", Text: "typed", AnyPane: true}); a.Error != "" {
		t.Fatalf("the Terminal composer's paste was refused: %s", a.Error)
	}
	// Control: the prompt answered, the same rendered-view paste is typed.
	b.set(guardWindows(mirror.StatusIdle)...)
	if a := pasteAnswer(t, d, mirror.Msg{ID: 4, Session: "scratch2", Window: "@7", Text: "hello"}); a.Error != "" {
		t.Fatalf("a paste into the idle Claude pane was refused: %s", a.Error)
	}
	if got := pasted(); !slices.Equal(got, []string{"%21 typed", "%21 hello"}) {
		t.Fatalf("typed %q; want the Terminal composer's paste, then the idle one", got)
	}
}

// TestAPasteIntoAClaudePaneWhoseScreenShowsADialogTypesNothing: the status
// says idle (a prompt the sessions file has not caught up with, or a picker
// the person opened), but the pane's SCREEN shows a permission prompt — a
// capture of the real CLI's. The host refuses on the screen. A screen that
// cannot be read is refused too. Control: the real CLI's idle prompt is typed
// into.
func TestAPasteIntoAClaudePaneWhoseScreenShowsADialogTypesNothing(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile("../../cmd/ccd/testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	permission, idle := read("pane_permission_2.1.296.txt"), read("pane_idle_2.1.296.txt")
	screen, screenErr := permission, error(nil)
	d := newDrivenMuster(t)
	b := &windowsBackend{windows: guardWindows(mirror.StatusIdle)}
	d.run(t, wrapped{screenBackend{windowsBackend: b, screen: func(pane string) (string, error) {
		if pane != "%21" {
			t.Errorf("the screen of %s was read; want the Claude pane %%21", pane)
		}
		return screen, screenErr
	}}}, nil, true)
	pasted := func() []string { _, _, _, p, _ := b.snapshot(); return p }

	// Refused for the DIALOG, in its words — not for some later check (the
	// prompt also hides the input box, which is refused on its own).
	if a := pasteAnswer(t, d, mirror.Msg{ID: 1, Session: "scratch2", Window: "@7", Text: "1"}); a.Refusal != "claude-waiting" || !strings.Contains(a.Error, "shows a dialog") {
		t.Fatalf("a paste into a pane showing a permission prompt was answered %+v; want refused as claude-waiting for the dialog", a)
	}
	screen, screenErr = "", errors.New("capture-pane: no such pane")
	if a := pasteAnswer(t, d, mirror.Msg{ID: 2, Session: "scratch2", Window: "@7", Text: "1"}); a.Error == "" || a.Refusal != "claude-waiting" {
		t.Fatalf("a paste into a pane whose screen could not be read was answered %+v; want refused", a)
	}
	if got := pasted(); len(got) != 0 {
		t.Fatalf("typed %q into a pane the guard refused", got)
	}
	screen, screenErr = idle, nil
	if a := pasteAnswer(t, d, mirror.Msg{ID: 3, Session: "scratch2", Window: "@7", Text: "hello"}); a.Error != "" {
		t.Fatalf("a paste into a pane at its idle prompt was refused: %s", a.Error)
	}
	if got := pasted(); !slices.Equal(got, []string{"%21 hello"}) {
		t.Fatalf("typed %q; want the one paste at the idle prompt", got)
	}
}
