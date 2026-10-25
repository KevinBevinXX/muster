//go:build tmuxit

package mirrorhost

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/ccdialog"
)

// The Claude prompt guard's tmux half against REAL tmux, on the rig's private
// socket (tmux_integration_test.go): nothing here reaches the operator's
// server.

// TestTmuxACheckedPasteWithholdsTheEnterWhenTheCheckRefuses: with the check
// refusing before the Enter, the text lands (the tty echoes it once) and is NOT
// submitted (cat echoes a line only once it is entered). The control, the check
// admitting both times, submits.
func TestTmuxACheckedPasteWithholdsTheEnterWhenTheCheckRefuses(t *testing.T) {
	r := newTmuxRig(t)
	b := Tmux{Socket: r.sock}
	paneOf := func(name string) string {
		r.session(name, "READY")
		waitFor(t, name+"'s marker", func() bool { return strings.Contains(r.pane(name), "READY") })
		return strings.TrimSpace(r.run("display-message", "-p", "-t", "="+name+":", "#{pane_id}"))
	}
	capture := func(pane string) string { return r.run("capture-pane", "-p", "-J", "-t", pane) }

	held := paneOf("held")
	calls := 0
	check := func(context.Context) error {
		calls++
		if calls == 2 {
			return ErrClaudeWaiting
		}
		return nil
	}
	err := b.PasteChecked(context.Background(), ClaudePane{Pane: held}, "held-back", check, check)
	var pe *ClaudePastedError
	if !errors.As(err, &pe) || calls != 2 {
		t.Fatalf("PasteChecked = %v after %d checks; want pasted-not-submitted after 2", err, calls)
	}
	waitFor(t, "the paste to land", func() bool { return strings.Count(capture(held), "held-back") >= 1 })
	time.Sleep(400 * time.Millisecond)
	if n := strings.Count(capture(held), "held-back"); n != 1 {
		t.Fatalf("the text appears %d times; want once (pasted, never entered):\n%s", n, capture(held))
	}

	sent := paneOf("sent")
	if err := b.PasteChecked(context.Background(), ClaudePane{Pane: sent}, "sent-on", func(context.Context) error { return nil }, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("PasteChecked admitted: %v", err)
	}
	waitFor(t, "the control to be pasted AND submitted", func() bool { return strings.Count(capture(sent), "sent-on") == 2 })
}

// TestTmuxClaudeScreenReadsWhatThePaneShows: ClaudeScreen is the pane's screen
// as ccdialog reads it — a pane showing the real CLI's permission prompt
// (cmd/ccd's capture of 2.1.296) is read as a dialog; one showing its idle
// prompt is not.
func TestTmuxClaudeScreenReadsWhatThePaneShows(t *testing.T) {
	r := newTmuxRig(t)
	b := Tmux{Socket: r.sock}
	for i, c := range []struct {
		fixture, last string
		dialog        bool
	}{{"pane_permission_2.1.296.txt", "Tab to amend", true}, {"pane_idle_2.1.296.txt", "for shortcuts", false}} {
		path, err := filepath.Abs("../../cmd/ccd/testdata/" + c.fixture)
		if err != nil {
			t.Fatal(err)
		}
		name := []string{"permission", "idle"}[i]
		r.run("new-session", "-d", "-s", name, "-x", "250", "-y", "60", "sh", "-c", "cat '"+path+"'; exec sleep 600")
		pane := strings.TrimSpace(r.run("display-message", "-p", "-t", "="+name+":", "#{pane_id}"))
		var got string
		waitFor(t, c.fixture+" on screen", func() bool {
			got, err = b.ClaudeScreen(context.Background(), pane)
			return err == nil && strings.Contains(got, c.last)
		})
		if what, shown := ccdialog.OnScreen(got); shown != c.dialog {
			t.Fatalf("%s: read as a dialog %t (%q), want %t:\n%s", c.fixture, shown, what, c.dialog, got)
		}
	}
	if _, err := b.ClaudeScreen(context.Background(), "permission"); err == nil {
		t.Fatal("a session name was accepted as a pane id")
	}
}
