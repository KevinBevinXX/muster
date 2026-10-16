//go:build tmuxit

package mirrorhost

import (
	"context"
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
