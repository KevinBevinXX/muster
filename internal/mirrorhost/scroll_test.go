package mirrorhost

import (
	"context"
	"fmt"
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
	// over: panes with another mode (a menu) open OVER their copy-mode, as
	// tmux stacks them: a cancel reaches nothing and says so.
	over map[string]bool
	// tries counts every CancelCopyMode, refused ones included.
	tries int
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
	f.tries++
	if f.over[pane] {
		return fmt.Errorf("%w: pane %s is in tree-mode", ErrPaneInAnotherMode, pane)
	}
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

func newModeFake() *modeFake { return &modeFake{pos: map[string]int{}, over: map[string]bool{}} }

// setOver opens (or closes) another mode over pane's copy-mode.
func (f *modeFake) setOver(pane string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.over[pane] = on
}

func (f *modeFake) cancelTries() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tries
}

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

// handTimers stands in for time.AfterFunc (scroller.afterFunc): it records
// every timer started and fires one only when the test says so, so a test of
// WHICH timer cancels does not race a wall clock. Like a *time.Timer, a
// stopped timer that is fired does nothing.
type handTimers struct {
	mu sync.Mutex
	ts []*handTimer
}

type handTimer struct {
	d       time.Duration
	f       func()
	stopped bool
}

func (h *handTimers) afterFunc(d time.Duration, f func()) stopper {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := &handTimer{d: d, f: f}
	h.ts = append(h.ts, t)
	return &handStop{h, t}
}

type handStop struct {
	h *handTimers
	t *handTimer
}

func (s *handStop) Stop() bool {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	was := !s.t.stopped
	s.t.stopped = true
	return was
}

func (h *handTimers) count() int { h.mu.Lock(); defer h.mu.Unlock(); return len(h.ts) }

// fire runs timer i, unless it was stopped first (a *time.Timer's Stop).
func (h *handTimers) fire(i int) {
	h.mu.Lock()
	t := h.ts[i]
	run := !t.stopped
	t.stopped = true
	h.mu.Unlock()
	if run {
		t.f()
	}
}

// TestEachScrollResetsTheIdleTimer: each scroll restarts the pane's idle
// spell, so the cancel comes one spell after the LAST scroll, never one after
// the first. The timers are fired by hand (handTimers): every spell but the
// last, fired, cancels nothing; the last one's does. (The idle cancel against
// a real clock is TestAPaneThisHostScrolledLeavesCopyModeWhenIdle, and against
// real tmux TestTmuxAScrolledPaneLeavesCopyModeWhenIdle.)
func TestEachScrollResetsTheIdleTimer(t *testing.T) {
	f := newModeFake()
	idle := 150 * time.Millisecond
	s := quietScroller(f, idle)
	clock := &handTimers{}
	s.afterFunc = clock.afterFunc
	const scrolls = 8
	for i := 0; i < scrolls; i++ {
		if err := s.scroll(context.Background(), "%1", 1); err != nil {
			t.Fatal(err)
		}
	}
	if n := clock.count(); n != scrolls {
		t.Fatalf("%d scrolls started %d idle spells, want one each", scrolls, n)
	}
	for i := 0; i < scrolls; i++ {
		if d := clock.ts[i].d; d != idle {
			t.Fatalf("spell %d is %s, want the idle spell %s", i, d, idle)
		}
	}
	for i := 0; i < scrolls-1; i++ {
		clock.fire(i)
		if pos, c := f.state(); len(c) != 0 || pos["%1"] != scrolls {
			t.Fatalf("the spell of scroll %d (not the last) cancelled: pos %v, cancels %q", i+1, pos, c)
		}
	}
	clock.fire(scrolls - 1)
	if pos, c := f.state(); len(pos) != 0 || !slices.Equal(c, []string{"%1"}) {
		t.Fatalf("the last scroll's spell: in copy-mode %v, cancels %q; want one cancel of %%1", pos, c)
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

// TestAMenuOverThisHostsCopyModeDelaysTheCancelButDoesNotLoseIt: modes stack —
// someone opens choose-tree over the copy-mode this host entered — so the idle
// cancel cannot reach it. It is tried again each idle spell, not forgotten,
// and lands once the menu closes.
func TestAMenuOverThisHostsCopyModeDelaysTheCancelButDoesNotLoseIt(t *testing.T) {
	f := newModeFake()
	idle := 60 * time.Millisecond
	s := quietScroller(f, idle)
	if err := s.scroll(context.Background(), "%1", 5); err != nil {
		t.Fatal(err)
	}
	f.setOver("%1", true)
	// Several spells pass with the menu open: tried, refused, tried again.
	within(t, "three refused cancels", 3*time.Second, func() bool { return f.cancelTries() >= 3 })
	if pos, c := f.state(); len(c) != 0 || pos["%1"] != 5 {
		t.Fatalf("with the menu open: pos %v, cancels %q", pos, c)
	}
	f.setOver("%1", false)
	within(t, "the cancel once the menu closed", 3*time.Second, func() bool { _, c := f.state(); return slices.Equal(c, []string{"%1"}) })
	// And then it is forgotten: no more tries.
	n := f.cancelTries()
	time.Sleep(4 * idle)
	if got := f.cancelTries(); got != n {
		t.Fatalf("after the cancel landed it was tried %d more time(s)", got-n)
	}
}

// TestADisconnectWithAMenuOverCopyModeStillCancelsItLater: the connection's end
// cannot cancel a copy-mode under a menu either; it keeps trying after the end
// rather than leave the pane in it.
func TestADisconnectWithAMenuOverCopyModeStillCancelsItLater(t *testing.T) {
	f := newModeFake()
	s := quietScroller(f, 60*time.Millisecond)
	_ = s.scroll(context.Background(), "%1", 5)
	f.setOver("%1", true)
	s.closeAll()
	if _, c := f.state(); len(c) != 0 {
		t.Fatalf("fixture: cancelled through a menu: %q", c)
	}
	f.setOver("%1", false)
	within(t, "the cancel after the connection ended", 3*time.Second, func() bool { _, c := f.state(); return slices.Equal(c, []string{"%1"}) })
}

// TestAMessageSentIntoThisHostsCopyModeCancelsItFirst: a paste lands in the
// program under copy-mode but the pane goes on showing old history, so the
// copy-mode THIS host entered is cancelled before it — and the operator's own
// is not.
func TestAMessageSentIntoThisHostsCopyModeCancelsItFirst(t *testing.T) {
	f := newModeFake()
	f.pos["%9"] = 2 // the operator's own copy-mode
	s := quietScroller(f, time.Hour)
	_ = s.scroll(context.Background(), "%1", 5)
	_ = s.scroll(context.Background(), "%9", 1)
	s.leaveForPaste("%1")
	s.leaveForPaste("%9")
	s.leaveForPaste("%5") // never scrolled
	if pos, c := f.state(); !slices.Equal(c, []string{"%1"}) || pos["%9"] != 3 || len(pos) != 1 {
		t.Fatalf("after the pastes: pos %v, cancels %q; want only %%1 cancelled", pos, c)
	}
	// Its timer went with it: nothing is cancelled again later (the idle here
	// is an hour, so only a still-armed entry could be swept by closeAll).
	s.closeAll()
	if _, c := f.state(); !slices.Equal(c, []string{"%1"}) {
		t.Fatalf("closeAll found %%1 still armed: cancels %q", c)
	}
}
