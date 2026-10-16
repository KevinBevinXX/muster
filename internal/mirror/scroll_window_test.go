package mirror_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// winScroller is a per-window host (winBackend) that can scroll, and counts
// its full window reads: Windows is the read that also finds Claude Code (on
// real tmux a /proc scan, ~50 ms a call on a 75-window host). Wrapped in
// winPaner it also names a window's pane the cheap way
// (mirrorhost.WindowPaner).
type winScroller struct {
	*winBackend
	scrolls *scrollingBackend
	reads   atomic.Int64
}

func (b *winScroller) Windows(ctx context.Context) ([]mirrorhost.WindowInfo, error) {
	b.reads.Add(1)
	return b.winBackend.Windows(ctx)
}

func (b *winScroller) ScrollPane(ctx context.Context, pane string, lines int) (bool, bool, error) {
	return b.scrolls.ScrollPane(ctx, pane, lines)
}

func (b *winScroller) CancelCopyMode(ctx context.Context, pane string) error {
	return b.scrolls.CancelCopyMode(ctx, pane)
}

type winPaner struct{ *winScroller }

func (b winPaner) WindowPane(_ context.Context, session, window string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if w, ok := mirrorhost.FindWindow(b.windows, session, window); ok {
		return w.Pane, nil
	}
	return "", errors.New("gone")
}

// TestAScrollReadsNoFullWindowList: a scroll of a named window reaches THAT
// window's pane through the backend's cheap WindowPane, and never through the
// full window read (which finds Claude Code too) — a scroll comes once an
// animation frame. The control: a backend without WindowPane falls back to the
// full read, so the counter is seen to move.
func TestAScrollReadsNoFullWindowList(t *testing.T) {
	for name, tc := range map[string]struct {
		paner bool
		reads bool
	}{"a backend with WindowPane": {true, false}, "control: without it": {false, true}} {
		r := newRig(t)
		tok, _ := r.store.mint(t, 1, "ws")
		ws := &winScroller{winBackend: &winBackend{windows: goldWindows}, scrolls: newScrollingBackend()}
		var b mirrorhost.Backend = ws
		if tc.paner {
			b = winPaner{ws}
		}
		c, err := mirrorhost.New(mirrorhost.Config{URL: r.srv.URL, Token: tok, Host: "ws", Backend: b,
			Interactive: true, AllowHTTP: true, Refresh: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		ctx, stop := context.WithCancel(context.Background())
		go func() { _ = c.RunOnce(ctx) }()
		r.online(t, 1, "ws", "scratch2")
		before := ws.reads.Load()
		for i := 0; i < 5; i++ {
			if err := r.svc.ScrollWindow(asUser(1), "ws", "scratch2", "@9", 2); err != nil {
				t.Fatalf("%s: scroll: %v", name, err)
			}
		}
		calls, _, _ := ws.scrolls.snapshot()
		if len(calls) != 5 || calls[0] != "%33 2" {
			t.Errorf("%s: the host scrolled %q, want five scrolls of @9's pane %%33", name, calls)
		}
		if got := ws.reads.Load() - before; (got > 0) != tc.reads {
			t.Errorf("%s: five scrolls made %d full window read(s)", name, got)
		}
		stop()
	}
}
