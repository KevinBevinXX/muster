package mirrorhost

import (
	"context"
	"io"
	"log"
	"slices"
	"sync"
	"testing"
	"time"
)

// The scroller's idle cancel, against a played copy-mode. The same rules
// against REAL tmux are TestTmuxAScrolledPaneLeavesCopyModeWhenIdle (tagged
// tmuxit).

// modeFake plays copy-mode per pane: lines back from the bottom, absent = not
// in it; `-e` semantics (back to 0 leaves it).
type modeFake struct {
	mu      sync.Mutex
	pos     map[string]int
	cancels []string
}

func (f *modeFake) ScrollPane(_ context.Context, pane string, lines int) (bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, in := f.pos[pane]
	if !in && lines < 0 {
		return false, false, nil
	}
	if p += lines; p <= 0 {
		delete(f.pos, pane)
		return !in, false, nil
	}
	f.pos[pane] = p
	return !in, true, nil
}

func (f *modeFake) CancelCopyMode(_ context.Context, pane string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, in := f.pos[pane]; in {
		delete(f.pos, pane)
		f.cancels = append(f.cancels, pane)
	}
	return nil
}

func (f *modeFake) state() (map[string]int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := map[string]int{}
	for k, v := range f.pos {
		p[k] = v
	}
	return p, slices.Clone(f.cancels)
}

func newModeFake() *modeFake { return &modeFake{pos: map[string]int{}} }

func quietScroller(f *modeFake, idle time.Duration) *scroller {
	return newScroller(f, idle, log.New(io.Discard, "", 0))
}

func within(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAPaneThisHostScrolledLeavesCopyModeWhenIdle: one scroll back, then
// nothing: the pane is cancelled once the idle spell has passed, not before.
func TestAPaneThisHostScrolledLeavesCopyModeWhenIdle(t *testing.T) {
	f := newModeFake()
	s := quietScroller(f, 120*time.Millisecond)
	start := time.Now()
	if err := s.scroll(context.Background(), "%1", 5); err != nil {
		t.Fatal(err)
	}
	within(t, "the idle cancel", 3*time.Second, func() bool { _, c := f.state(); return len(c) == 1 })
	if el := time.Since(start); el < 120*time.Millisecond {
		t.Fatalf("cancelled after %s, before the %s idle spell", el, 120*time.Millisecond)
	}
	if pos, c := f.state(); len(pos) != 0 || !slices.Equal(c, []string{"%1"}) {
		t.Fatalf("after the idle cancel: in copy-mode %v, cancels %q", pos, c)
	}
}

// TestEachScrollResetsTheIdleTimer: scrolls closer together than the idle
// spell keep the pane in copy-mode for longer than one spell; the cancel comes
// one spell after the LAST.
func TestEachScrollResetsTheIdleTimer(t *testing.T) {
	f := newModeFake()
	idle := 150 * time.Millisecond
	s := quietScroller(f, idle)
	for i := 0; i < 8; i++ {
		if err := s.scroll(context.Background(), "%1", 1); err != nil {
			t.Fatal(err)
		}
		time.Sleep(idle / 3)
	}
	// 8 × idle/3 is well past one spell from the first scroll.
	if _, c := f.state(); len(c) != 0 {
		t.Fatalf("cancelled while scrolls kept coming: %q", c)
	}
	last := time.Now()
	within(t, "the idle cancel after the last scroll", 3*time.Second, func() bool { _, c := f.state(); return len(c) == 1 })
	if el := time.Since(last); el < idle-idle/3 {
		t.Fatalf("cancelled %s after the last scroll, sooner than its spell allows", el)
	}
}

// TestScrollingBackToTheBottomDisarmsTheTimer: -e left copy-mode at the
// bottom, so there is nothing to cancel — and a cancel then would land on
// whatever mode the pane is in next.
func TestScrollingBackToTheBottomDisarmsTheTimer(t *testing.T) {
	f := newModeFake()
	s := quietScroller(f, 60*time.Millisecond)
	_ = s.scroll(context.Background(), "%1", 5)
	_ = s.scroll(context.Background(), "%1", -50)
	if pos, _ := f.state(); len(pos) != 0 {
		t.Fatalf("fixture: the pane is still in copy-mode: %v", pos)
	}
	// The operator enters copy-mode themselves after that.
	f.mu.Lock()
	f.pos["%1"] = 3
	f.mu.Unlock()
	time.Sleep(250 * time.Millisecond)
	if pos, c := f.state(); len(c) != 0 || pos["%1"] != 3 {
		t.Fatalf("a disarmed timer cancelled the operator's copy-mode: pos %v, cancels %q", pos, c)
	}
}

// TestTheOperatorsOwnCopyModeIsNotCancelled: a pane already in copy-mode when
// the scroll arrived is scrolled, but its mode is not this host's to end.
func TestTheOperatorsOwnCopyModeIsNotCancelled(t *testing.T) {
	f := newModeFake()
	f.pos["%2"] = 4
	s := quietScroller(f, 60*time.Millisecond)
	if err := s.scroll(context.Background(), "%2", 3); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if pos, c := f.state(); len(c) != 0 || pos["%2"] != 7 {
		t.Fatalf("the operator's copy-mode: pos %v, cancels %q; want scrolled to 7 and not cancelled", pos, c)
	}
	// Control: a pane this host entered, in the same scroller, IS cancelled.
	_ = s.scroll(context.Background(), "%3", 2)
	within(t, "the control's cancel", 3*time.Second, func() bool { _, c := f.state(); return slices.Equal(c, []string{"%3"}) })
}

// TestCloseAllCancelsAtOnce: the connection's end cancels every pane this host
// put in copy-mode, without waiting for its timer — and only those.
func TestCloseAllCancelsAtOnce(t *testing.T) {
	f := newModeFake()
	f.pos["%9"] = 1 // the operator's
	s := quietScroller(f, time.Hour)
	_ = s.scroll(context.Background(), "%1", 2)
	_ = s.scroll(context.Background(), "%2", 2)
	s.closeAll()
	pos, c := f.state()
	slices.Sort(c)
	if !slices.Equal(c, []string{"%1", "%2"}) || pos["%9"] != 1 || len(pos) != 1 {
		t.Fatalf("after closeAll: pos %v, cancels %q", pos, c)
	}
	// A scroll that ends after closeAll leaves no copy-mode behind.
	_ = s.scroll(context.Background(), "%4", 2)
	if pos, _ := f.state(); pos["%4"] != 0 {
		t.Fatalf("a scroll after closeAll left %%4 in copy-mode: %v", pos)
	}
}
