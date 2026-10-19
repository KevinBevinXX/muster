package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeClock is a settable clock shared by lanes and their servers.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// stuckRig is a cap-1 lanes set on a fake clock whose lane servers share one
// blocked reporter, as main.go wires them.
func stuckRig(t *testing.T) (*lanes, *fakeClock, *blockedReporter) {
	t.Helper()
	l := testLanes(t, 1)
	clk := &fakeClock{t: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	rep := &blockedReporter{ch: make(chan bool, 1)}
	mk := l.newServer
	l.newServer = func(term terminal) *server {
		s := mk(term)
		s.clock, s.blocked = clk.now, rep
		return s
	}
	l.now, l.stuckAfter = clk.now, 10*time.Minute
	return l, clk, rep
}

// atPrompt drives ln's server to a submitted prompt that stopped at a
// permission dialog: busy, and awaiting approval.
func atPrompt(ln *lane) {
	ln.srv.onHook(hookEvent{Event: "SessionStart", SessionID: laneUUID(ln.key)})
	ln.srv.onHook(hookEvent{Event: "UserPromptSubmit", PromptID: "p1", Prompt: "x"})
	ln.srv.onHook(hookEvent{Event: "Notification", NotificationType: "permission_prompt"})
}

// 🔴 A LANE STUCK AT A PERMISSION PROMPT DOES NOT HOLD ITS SLOT FOR EVER. Under
// stuckAfter it is still refused as busy (a prompt someone may be about to
// answer); past it, with no turn holding the lane, a new session stops it.
// /healthz says it is waiting, since when, and when it has become stuck.
func TestALaneStuckAtAPermissionPromptIsStoppedPastStuckAfter(t *testing.T) {
	l, clk, _ := stuckRig(t)
	ctx := context.Background()
	a, f := l.acquire(ctx, "sess-a", "")
	if f != nil {
		t.Fatal(f)
	}
	t.Cleanup(a.stop)
	atPrompt(a)
	l.release(a) // the turn that drove it gave up
	if a.srv.idle() {
		t.Fatal("precondition: a lane at a permission prompt is not idle")
	}

	clk.add(10*time.Minute - time.Second)
	if _, f := l.acquire(ctx, "sess-b", ""); f == nil || f.Type != failCapacity {
		t.Fatalf("under stuckAfter: %v; want %s", f, failCapacity)
	}
	h := l.health()
	if len(h) != 1 || !h[0].AwaitingApproval || h[0].AwaitingSince != "2026-10-19T12:00:00Z" || h[0].Stuck {
		t.Fatalf("/healthz under stuckAfter: %+v", h)
	}

	clk.add(time.Second)
	if h := l.health(); len(h) != 1 || !h[0].Stuck {
		t.Fatalf("/healthz at stuckAfter: %+v; want stuck", h)
	}
	b, f := l.acquire(ctx, "sess-b", "")
	if f != nil {
		t.Fatalf("past stuckAfter: %v; want the stuck lane stopped", f)
	}
	t.Cleanup(b.stop)
	if l.get(a.id) != nil {
		t.Fatal("the stuck lane is still in the set")
	}
}

// A stuck lane a turn STILL holds is never taken (lane.reserved wins).
func TestAStuckLaneATurnStillHoldsIsNotStopped(t *testing.T) {
	l, clk, _ := stuckRig(t)
	ctx := context.Background()
	a, f := l.acquire(ctx, "sess-a", "")
	if f != nil {
		t.Fatal(f)
	}
	t.Cleanup(a.stop)
	atPrompt(a)
	clk.add(time.Hour)
	if _, f := l.acquire(ctx, "sess-b", ""); f == nil || f.Type != failCapacity {
		t.Fatalf("a held stuck lane: %v; want %s", f, failCapacity)
	}
}

// 🔴 THE BADGE SHOWS A LANE'S PROMPT: the agent's one "awaiting approval" state
// is the OR of every CLI; it is reported when a lane reaches a prompt and
// withdrawn when that lane is stopped — while the primary's own state is kept.
func TestTheAwaitingApprovalBadgeCoversLanes(t *testing.T) {
	l, clk, rep := stuckRig(t)
	ctx := context.Background()
	report := func() (bool, bool) {
		select {
		case v := <-rep.ch:
			return v, true
		default:
			return false, false
		}
	}
	a, f := l.acquire(ctx, "sess-a", "")
	if f != nil {
		t.Fatal(f)
	}
	atPrompt(a)
	if v, ok := report(); !ok || !v {
		t.Fatalf("a lane at a permission prompt: reported %v (sent=%v); want true", v, ok)
	}
	// The primary reaches a prompt too, then answers it: the lane still waits.
	primary := newServer(serverConfig{}, nil, newAuthTracker())
	primary.blocked = rep
	primary.onHook(hookEvent{Event: "Notification", NotificationType: "permission_prompt"})
	primary.onHook(hookEvent{Event: "PostToolUse"})
	if v, ok := report(); ok {
		t.Fatalf("the primary's prompt came and went while a lane waits: reported %v; want nothing", v)
	}
	l.release(a)
	clk.add(time.Hour)
	b, f := l.acquire(ctx, "sess-b", "")
	if f != nil {
		t.Fatal(f)
	}
	t.Cleanup(b.stop)
	if v, ok := report(); !ok || v {
		t.Fatalf("the waiting lane was stopped: reported %v (sent=%v); want false", v, ok)
	}
}
