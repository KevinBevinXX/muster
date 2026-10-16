//go:build tmuxit

package mirrorhost

import (
	"context"
	"errors"
	"io"
	"log"
	"strconv"
	"strings"
	"testing"
	"time"
)

// DRAG-TO-SCROLL against REAL tmux (the tmuxRig of tmux_integration_test.go):
// copy-mode is entered, moves through the pane's history, leaves at the bottom
// and leaves when idle — with muster's read-only viewer attached, as it is
// whenever a page is scrolling.

// historySession starts a session whose pane has printed n numbered lines (so
// it has history) and then waits, and returns its pane id.
func (r *tmuxRig) historySession(name string, n int) string {
	r.t.Helper()
	r.run("new-session", "-d", "-s", name, "-x", "80", "-y", "20", "sh", "-c", "seq 1 "+strconv.Itoa(n)+"; exec cat")
	waitFor(r.t, name+"'s history", func() bool {
		h, _ := strconv.Atoi(strings.TrimSpace(r.run("display-message", "-p", "-t", "="+name+":", "#{history_size}")))
		return h >= n-40
	})
	return strings.TrimSpace(r.run("display-message", "-p", "-t", "="+name+":", "#{pane_id}"))
}

// mode is the pane's #{pane_in_mode} and #{scroll_position} ("" outside
// copy-mode).
func (r *tmuxRig) mode(pane string) (string, string) {
	r.t.Helper()
	in, pos, _ := strings.Cut(strings.TrimSpace(r.run("display-message", "-p", "-t", pane, "#{pane_in_mode} #{scroll_position}")), " ")
	return in, pos
}

// TestTmuxScrollPaneEntersCopyModeMovesAndLeavesAtTheBottom: a scroll back
// enters copy-mode (pane_in_mode=1) and the scroll position follows each
// scroll; a scroll forward past the bottom leaves it (-e), and a scroll forward
// of a pane in no mode does nothing.
func TestTmuxScrollPaneEntersCopyModeMovesAndLeavesAtTheBottom(t *testing.T) {
	r := newTmuxRig(t)
	pane := r.historySession("hist", 300)
	b := Tmux{Socket: r.sock}
	// muster's viewer: a READ-ONLY client on the pane (send-keys refuses some
	// commands for a read-only client; copy-mode's must still work).
	term, err := b.Attach(context.Background(), "hist", true, 80, 20)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	go func() { _, _ = io.Copy(io.Discard, term) }()
	waitFor(t, "the read-only viewer", func() bool { return strings.Contains(r.run("list-clients", "-F", "#{client_flags}"), "read-only") })

	ctx := context.Background()
	if in, _ := r.mode(pane); in != "0" {
		t.Fatalf("fixture: the pane starts in a mode (%s)", in)
	}
	step := func(lines int, wantEntered, wantIn bool, wantIn1, wantPos string) {
		t.Helper()
		entered, inMode, err := b.ScrollPane(ctx, pane, lines)
		if err != nil {
			t.Fatalf("ScrollPane(%d): %v", lines, err)
		}
		in, pos := r.mode(pane)
		if entered != wantEntered || inMode != wantIn || in != wantIn1 || pos != wantPos {
			t.Fatalf("ScrollPane(%d) = entered %v, inMode %v; tmux says pane_in_mode=%s scroll_position=%q; want %v %v %s %q",
				lines, entered, inMode, in, pos, wantEntered, wantIn, wantIn1, wantPos)
		}
	}
	step(-3, false, false, "0", "") // forward, not in a mode: nothing
	step(10, true, true, "1", "10")
	step(5, false, true, "1", "15")
	step(-5, false, true, "1", "10")
	step(-50, false, false, "0", "") // past the bottom: -e left copy-mode
	// And CancelCopyMode ends one this host entered; on a pane in no mode it
	// does nothing.
	step(4, true, true, "1", "4")
	if err := b.CancelCopyMode(ctx, pane); err != nil {
		t.Fatal(err)
	}
	if in, _ := r.mode(pane); in != "0" {
		t.Fatalf("CancelCopyMode left pane_in_mode=%s", in)
	}
	if err := b.CancelCopyMode(ctx, pane); err != nil {
		t.Fatalf("CancelCopyMode on a pane in no mode: %v", err)
	}
	if _, _, err := b.ScrollPane(ctx, "hist", 1); err == nil {
		t.Fatal("ScrollPane accepted a session name, not a pane id")
	}
}

// TestTmuxAScrolledPaneLeavesCopyModeWhenIdle: the scroller's idle timer, on a
// real pane: in copy-mode after the scroll, and out of it once the (short)
// spell has passed — not before.
func TestTmuxAScrolledPaneLeavesCopyModeWhenIdle(t *testing.T) {
	r := newTmuxRig(t)
	pane := r.historySession("idle", 200)
	idle := 400 * time.Millisecond
	s := newScroller(Tmux{Socket: r.sock}, idle, log.New(io.Discard, "", 0))
	start := time.Now()
	if err := s.scroll(context.Background(), pane, 8); err != nil {
		t.Fatal(err)
	}
	if in, pos := r.mode(pane); in != "1" || pos != "8" {
		t.Fatalf("after the scroll: pane_in_mode=%s scroll_position=%q, want 1 and 8", in, pos)
	}
	waitFor(t, "the idle cancel", func() bool { in, _ := r.mode(pane); return in == "0" })
	if el := time.Since(start); el < idle {
		t.Fatalf("copy-mode ended after %s, before the %s spell", el, idle)
	}
}

// TestTmuxAMenuOverThisHostsCopyModeIsWaitedOut: real tmux STACKS modes —
// choose-tree opened over the copy-mode this host entered is pane_in_mode=2
// with tree-mode on top, and `-X cancel` cannot reach the copy-mode under it.
// A scroll is refused as another mode; the idle cancel is tried again rather
// than forgotten, and once the menu closes the host's copy-mode is cancelled.
func TestTmuxAMenuOverThisHostsCopyModeIsWaitedOut(t *testing.T) {
	r := newTmuxRig(t)
	pane := r.historySession("menu", 200)
	b := Tmux{Socket: r.sock}
	// A client on the session: what closes a menu with a key, as the operator
	// does (send-keys to tree-mode needs one).
	term, err := b.Attach(context.Background(), "menu", false, 80, 20)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	go func() { _, _ = io.Copy(io.Discard, term) }()
	waitFor(t, "the client", func() bool { return strings.TrimSpace(r.run("list-clients", "-t", "=menu")) != "" })

	idle := 300 * time.Millisecond
	s := newScroller(b, idle, log.New(io.Discard, "", 0))
	if err := s.scroll(context.Background(), pane, 8); err != nil {
		t.Fatal(err)
	}
	r.run("choose-tree", "-t", pane)
	topMode := func() string {
		return strings.TrimSpace(r.run("display-message", "-p", "-t", pane, "#{pane_in_mode} #{pane_mode}"))
	}
	if got := topMode(); got != "2 tree-mode" {
		t.Fatalf("fixture: with the menu open the pane is %q, want \"2 tree-mode\"", got)
	}
	if _, _, err := b.ScrollPane(context.Background(), pane, 3); !errors.Is(err, ErrPaneInAnotherMode) {
		t.Fatalf("a scroll with a menu on top = %v, want ErrPaneInAnotherMode", err)
	}
	if err := b.CancelCopyMode(context.Background(), pane); !errors.Is(err, ErrPaneInAnotherMode) {
		t.Fatalf("a cancel with a menu on top = %v, want ErrPaneInAnotherMode", err)
	}
	// Three idle spells pass with the menu open: the host's copy-mode is still
	// under it, and the menu is untouched.
	time.Sleep(3 * idle)
	if got := topMode(); got != "2 tree-mode" {
		t.Fatalf("with the menu open for three spells the pane is %q, want \"2 tree-mode\"", got)
	}
	// The operator closes the menu: copy-mode is on top again, and the next
	// try cancels it.
	r.run("send-keys", "-t", pane, "q")
	waitFor(t, "the menu closed", func() bool { m := topMode(); return m == "1 copy-mode" || m == "0 " || m == "0" })
	waitFor(t, "the host's copy-mode cancelled after the menu closed", func() bool { return topMode() == "0" })
}

// TestTmuxWindowPaneNamesTheWindowsActivePaneOnly: WindowPane reads one
// session's windows and matches the id exactly — a window of ANOTHER session,
// an unknown id and a gone session are errors, never some other pane.
func TestTmuxWindowPaneNamesTheWindowsActivePaneOnly(t *testing.T) {
	r := newTmuxRig(t)
	r.session("one", "ONE")
	r.session("two", "TWO")
	r.run("split-window", "-d", "-t", "=one:")
	w1 := strings.TrimSpace(r.run("display-message", "-p", "-t", "=one:", "#{window_id}"))
	p1 := strings.TrimSpace(r.run("display-message", "-p", "-t", "=one:", "#{pane_id}"))
	w2 := strings.TrimSpace(r.run("display-message", "-p", "-t", "=two:", "#{window_id}"))
	b := Tmux{Socket: r.sock}
	ctx := context.Background()
	if got, err := b.WindowPane(ctx, "one", w1); err != nil || got != p1 {
		t.Fatalf("WindowPane(one, %s) = %q, %v; want the active pane %s", w1, got, err, p1)
	}
	for name, c := range map[string][2]string{
		"another session's window": {"one", w2},
		"an unknown window":        {"one", "@999"},
		"a gone session":           {"three", w1},
		"a session name prefix":    {"on", w1},
	} {
		if got, err := b.WindowPane(ctx, c[0], c[1]); err == nil {
			t.Errorf("%s: WindowPane(%s, %s) = %q, want an error", name, c[0], c[1], got)
		}
	}
}
