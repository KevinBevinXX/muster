package sendqueue

import (
	"context"
	"io"
	"log"
	"sync"
	"testing"
	"time"
)

// THE DISPATCHER'S STATE MACHINE, on the real store with a FAKE CLOCK, a fake
// target and a fake send path: idle-for-N, one at a time, busy-again blocks
// the next, a restart mid-send does not resend, an unknown target (an old
// host) is never sent to, and a cancelled message is never sent.

// fakeTarget is one target's state as the dispatcher's probe reads it.
type fakeTarget struct {
	mu    sync.Mutex
	st    AgentState
	sends []string // what reached the send path, in order
	// outcome is what the next send returns; zero is "sent".
	outcome *Outcome
}

func (f *fakeTarget) set(st AgentState) {
	f.mu.Lock()
	f.st = st
	f.mu.Unlock()
}

func (f *fakeTarget) probe(context.Context, Head) AgentState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}

func (f *fakeTarget) send(_ context.Context, it Item) Outcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, it.Body)
	if f.outcome != nil {
		return *f.outcome
	}
	return Outcome{Sent: true, Detail: "Sent."}
}

func (f *fakeTarget) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sends...)
}

// clock is a settable time.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

const idle = 30 * time.Second

func newDispatcher(s *PGStore, f *fakeTarget, c *clock) *Dispatcher {
	return &Dispatcher{Store: s, Probe: f.probe, Send: f.send, IdleFor: idle, TurnWait: -1, Now: c.now,
		Logger: log.New(io.Discard, "", 0)}
}

// tick runs one pass and fails the test on an error.
func tick(t *testing.T, d *Dispatcher) {
	t.Helper()
	if _, err := d.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func wantSent(t *testing.T, f *fakeTarget, when string, want ...string) {
	t.Helper()
	got := f.sent()
	if len(got) != len(want) {
		t.Fatalf("%s: sent %q, want %q", when, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: sent %q, want %q", when, got, want)
		}
	}
}

// TestTheDispatcherSendsOneMessagePerIdleStretch is the whole cycle: nothing is
// sent before the target has been idle for IdleFor (29 s: nothing; 30 s: one);
// exactly ONE message per pass and per idle stretch — the second waits while
// the target stays idle from BEFORE the first was sent, waits while it is busy,
// and waits again until it has been idle for IdleFor after that busy; then it
// is sent, and the third waits for the next turn.
func TestTheDispatcherSendsOneMessagePerIdleStretch(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now().Truncate(time.Second)
	c := &clock{t: t0}
	f := &fakeTarget{st: AgentState{Idle: true, Since: t0}}
	d := newDispatcher(s, f, c)
	for _, b := range []string{"one", "two", "three"} {
		mustEnqueue(t, s, 2, mirrorItem(b))
	}

	c.set(t0.Add(idle - time.Second))
	tick(t, d)
	wantSent(t, f, "idle 29 s", nil...)
	c.set(t0.Add(idle))
	tick(t, d)
	tick(t, d)
	wantSent(t, f, "idle 30 s, two passes", "one")

	// Still idle, from before "one" was sent: its turn has not been seen.
	c.set(t0.Add(10 * idle))
	tick(t, d)
	wantSent(t, f, "idle since before the first was sent", "one")

	// The turn runs.
	busyAt := t0.Add(11 * idle)
	c.set(busyAt)
	f.set(AgentState{Why: "busy"})
	tick(t, d)
	wantSent(t, f, "busy", "one")

	// Idle again — but not for IdleFor yet.
	idleAgain := busyAt.Add(time.Minute)
	f.set(AgentState{Idle: true, Since: idleAgain})
	c.set(idleAgain.Add(idle - time.Second))
	tick(t, d)
	wantSent(t, f, "idle again 29 s", "one")
	c.set(idleAgain.Add(idle))
	tick(t, d)
	wantSent(t, f, "idle again 30 s", "one", "two")
	c.set(idleAgain.Add(5 * idle))
	tick(t, d)
	wantSent(t, f, "no turn seen since the second", "one", "two")
}

// TestAnUnknownTargetIsNeverSentTo: a target the server cannot read (an older
// muster-mirror that reports no status — the probe answers not idle, with the
// reason) is held however long it stays so, and the composer is told why
// (Decide's reason is the probe's).
func TestAnUnknownTargetIsNeverSentTo(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now().Truncate(time.Second)
	c := &clock{t: t0}
	const why = "this host's muster-mirror does not report whether Claude Code is busy"
	f := &fakeTarget{st: AgentState{Why: why}}
	d := newDispatcher(s, f, c)
	mustEnqueue(t, s, 2, mirrorItem("held"))
	for _, after := range []time.Duration{0, idle, time.Hour, 24 * time.Hour} {
		c.set(t0.Add(after))
		tick(t, d)
	}
	wantSent(t, f, "an unknown target, a day later", nil...)
	h, ok, err := s.HeadOf(as(2), win)
	if err != nil || !ok || h.State != Queued {
		t.Fatalf("the held message = %+v, %v, %v; want still queued", h, ok, err)
	}
	if dec := d.Decide(h, f.probe(context.Background(), h)); dec.Send || dec.Why != why {
		t.Fatalf("Decide = %+v; want held, saying %q", dec, why)
	}
	// The positive control: the same message, once the target is known idle.
	f.set(AgentState{Idle: true, Since: c.now()})
	c.set(c.now().Add(idle))
	tick(t, d)
	wantSent(t, f, "the same target, known idle", "held")
}

// TestARestartMidSendDoesNotResend: a process that claimed a message and died
// before finishing it leaves a row the next process's Start fails (may have
// been delivered) — and then never sends it, however idle the target. The next
// message waits for a turn after that claim, as after any send that may have
// landed.
func TestARestartMidSendDoesNotResend(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now().Truncate(time.Second)
	c := &clock{t: t0}
	m1 := mustEnqueue(t, s, 2, mirrorItem("in flight"))
	mustEnqueue(t, s, 2, mirrorItem("next"))
	// The first process: claimed, then died (no Finish).
	if _, ok, err := s.Claim(context.Background(), m1.ID, t0); err != nil || !ok {
		t.Fatal(ok, err)
	}

	f := &fakeTarget{st: AgentState{Idle: true, Since: t0.Add(-time.Hour)}}
	d := newDispatcher(s, f, c)
	c.set(t0.Add(time.Minute))
	if err := d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.set(t0.Add(time.Hour))
	tick(t, d)
	wantSent(t, f, "after the restart, target idle since before the claim", nil...)
	if got, _ := s.Get(as(2), m1.ID); got.State != Failed || !got.MayHaveDelivered {
		t.Fatalf("the message in flight at the crash = %+v; want failed, may have been delivered", got)
	}
	// A turn after the claim, then idle: the NEXT message goes; the recovered
	// one never does.
	f.set(AgentState{Idle: true, Since: t0.Add(2 * time.Hour)})
	c.set(t0.Add(2*time.Hour + idle))
	tick(t, d)
	tick(t, d)
	wantSent(t, f, "after a turn", "next")
}

// TestACancelledMessageIsNeverSent: cancelled before its target went idle, it
// is passed over and the one behind it is sent instead.
func TestACancelledMessageIsNeverSent(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now().Truncate(time.Second)
	c := &clock{t: t0}
	f := &fakeTarget{st: AgentState{Idle: true, Since: t0}}
	d := newDispatcher(s, f, c)
	m1 := mustEnqueue(t, s, 2, mirrorItem("cancel me"))
	mustEnqueue(t, s, 2, mirrorItem("keep me"))
	if _, err := s.Cancel(as(2), m1.ID, t0); err != nil {
		t.Fatal(err)
	}
	c.set(t0.Add(idle))
	tick(t, d)
	wantSent(t, f, "after a cancel", "keep me")
}

// TestAFailureThatDidNotDeliverDoesNotWaitForATurn: a send refused before the
// message could reach the target (an expired step-up) started no turn, so the
// next message does not wait for one — and a failure that MAY have delivered
// (the host never confirmed) does.
func TestAFailureThatDidNotDeliverDoesNotWaitForATurn(t *testing.T) {
	for _, c := range []struct {
		name      string
		may       bool
		wantAfter []string
	}{
		{"refused before the target", false, []string{"first", "second"}},
		{"unconfirmed", true, []string{"first"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := pgStore(t)
			t0 := time.Now().Truncate(time.Second)
			clk := &clock{t: t0}
			f := &fakeTarget{st: AgentState{Idle: true, Since: t0}, outcome: &Outcome{MayHaveDelivered: c.may, Detail: "not sent"}}
			d := newDispatcher(s, f, clk)
			mustEnqueue(t, s, 2, mirrorItem("first"))
			mustEnqueue(t, s, 2, mirrorItem("second"))
			clk.set(t0.Add(idle))
			tick(t, d)
			clk.set(t0.Add(2 * idle))
			tick(t, d)
			wantSent(t, f, c.name, c.wantAfter...)
		})
	}
}

// TestTargetsAreIndependent: one target busy holds only its own queue.
func TestTargetsAreIndependent(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now().Truncate(time.Second)
	c := &clock{t: t0.Add(idle)}
	busy := mirrorItem("to the busy window")
	busy.Target.Window = "@1"
	mustEnqueue(t, s, 2, busy)
	mustEnqueue(t, s, 2, mirrorItem("to the idle window"))
	f := &fakeTarget{}
	d := newDispatcher(s, f, c)
	d.Probe = func(_ context.Context, h Head) AgentState {
		if h.Target.Window == "@1" {
			return AgentState{Why: "busy"}
		}
		return AgentState{Idle: true, Since: t0}
	}
	tick(t, d)
	wantSent(t, f, "one busy, one idle target", "to the idle window")
}

// TestDecide is the rule, case by case, at its boundaries — including the one
// exception (turnWait): an idle target whose previous message's turn was never
// seen is taken to have finished it after turnWait, and not before.
func TestDecide(t *testing.T) {
	t0 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	queued := Head{Item: Item{State: Queued}}
	after := Head{Item: Item{State: Queued}, LastAttempt: t0.Add(time.Minute)}
	for _, c := range []struct {
		name     string
		h        Head
		st       AgentState
		now      time.Time
		turnWait time.Duration
		send     bool
	}{
		{"being sent", Head{Item: Item{State: Sending}}, AgentState{Idle: true, Since: t0}, t0.Add(time.Hour), 0, false},
		{"not idle", queued, AgentState{Why: "busy"}, t0.Add(time.Hour), 0, false},
		{"idle 29s", queued, AgentState{Idle: true, Since: t0}, t0.Add(29 * time.Second), 0, false},
		{"idle 30s", queued, AgentState{Idle: true, Since: t0}, t0.Add(30 * time.Second), 0, true},
		{"idle since before the last attempt", after, AgentState{Idle: true, Since: t0}, t0.Add(time.Hour), 0, false},
		{"idle since exactly the last attempt", after, AgentState{Idle: true, Since: t0.Add(time.Minute)}, t0.Add(time.Hour), 0, false},
		{"idle since after the last attempt", after, AgentState{Idle: true, Since: t0.Add(2 * time.Minute)}, t0.Add(2*time.Minute + 30*time.Second), 0, true},
		{"turn unseen, before turnWait", after, AgentState{Idle: true, Since: t0}, t0.Add(time.Minute + 10*time.Minute - time.Second), 10 * time.Minute, false},
		{"turn unseen, at turnWait", after, AgentState{Idle: true, Since: t0}, t0.Add(time.Minute + 10*time.Minute), 10 * time.Minute, true},
	} {
		got := Decide(c.h, c.st, c.now, idle, c.turnWait)
		if got.Send != c.send {
			t.Errorf("%s: Decide = %+v, want send=%v", c.name, got, c.send)
		}
		if !got.Send && got.Why == "" {
			t.Errorf("%s: held with no reason", c.name)
		}
	}
}
