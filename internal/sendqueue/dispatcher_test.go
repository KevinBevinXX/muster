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
	// probedAs and sentAs are, per hook call, the item's owner and the owner
	// its ctx was handed as.
	probedAs, sentAs [][2]int64
}

func (f *fakeTarget) set(st AgentState) {
	f.mu.Lock()
	f.st = st
	f.mu.Unlock()
}

// ownerKey carries the owner asOwner handed the hooks.
type ownerKey struct{}

// asOwner is the dispatcher's AsOwner: ctx marked with owner.
func (f *fakeTarget) asOwner(ctx context.Context, owner int64) context.Context {
	return context.WithValue(ctx, ownerKey{}, owner)
}

// ctxOwner is the owner ctx was handed as, 0 for none.
func ctxOwner(ctx context.Context) int64 { o, _ := ctx.Value(ownerKey{}).(int64); return o }

func (f *fakeTarget) probe(ctx context.Context, h Head) AgentState {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probedAs = append(f.probedAs, [2]int64{h.Owner, ctxOwner(ctx)})
	return f.st
}

func (f *fakeTarget) send(ctx context.Context, it Item) Outcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sentAs = append(f.sentAs, [2]int64{it.Owner, ctxOwner(ctx)})
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

// idleSince is a target idle since t, with no clock skew: the host's status
// time is t, and the server first saw it at t.
func idleSince(t time.Time) AgentState {
	return AgentState{Idle: true, Since: t.UnixMilli(), Observed: t}
}

func newDispatcher(s *PGStore, f *fakeTarget, c *clock) *Dispatcher {
	return &Dispatcher{Store: s, Probe: f.probe, Send: f.send, AsOwner: f.asOwner, IdleFor: idle, Now: c.now,
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
	f := &fakeTarget{st: idleSince(t0)}
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
	f.set(idleSince(idleAgain))
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
	f.set(idleSince(c.now()))
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
	// Its lease ran out at t0+30s and nobody renewed it.
	if _, ok, err := s.Claim(context.Background(), m1.ID, t0, Claim{By: "dead", Until: t0.Add(DefaultLease),
		Since: t0.Add(-time.Hour).UnixMilli()}); err != nil || !ok {
		t.Fatal(ok, err)
	}

	f := &fakeTarget{st: idleSince(t0.Add(-time.Hour))}
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
	f.set(idleSince(t0.Add(2 * time.Hour)))
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
	f := &fakeTarget{st: idleSince(t0)}
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
			f := &fakeTarget{st: idleSince(t0), outcome: &Outcome{MayHaveDelivered: c.may, Detail: "not sent"}}
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
		return idleSince(t0)
	}
	tick(t, d)
	wantSent(t, f, "one busy, one idle target", "to the idle window")
}

// TestDecide is the rule, case by case, at its boundaries. There is NO time
// fallback: a target whose status is no newer than the one held at the last
// paste is held however long it stays so (a day later here), and an idle
// target whose time is unknown — or that the server has not observed — is not
// idle for any time.
func TestDecide(t *testing.T) {
	m0 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	queued := Head{Item: Item{State: Queued}}
	// A paste was made while the window's status time was m0+1min.
	after := Head{Item: Item{State: Queued}, TurnAfter: m0.Add(time.Minute).UnixMilli()}
	for _, c := range []struct {
		name string
		h    Head
		st   AgentState
		now  time.Time
		send bool
	}{
		{"being sent", Head{Item: Item{State: Sending}}, idleSince(m0), m0.Add(time.Hour), false},
		{"not idle", queued, AgentState{Why: "busy"}, m0.Add(time.Hour), false},
		{"idle 29s", queued, idleSince(m0), m0.Add(29 * time.Second), false},
		{"idle 30s", queued, idleSince(m0), m0.Add(30 * time.Second), true},
		{"idle since an unknown time", queued, AgentState{Idle: true, Observed: m0}, m0.Add(time.Hour), false},
		{"idle, never observed", queued, AgentState{Idle: true, Since: m0.UnixMilli()}, m0.Add(time.Hour), false},
		{"status older than the last paste's", after, idleSince(m0), m0.Add(time.Hour), false},
		{"status older than the last paste's, a day later", after, idleSince(m0), m0.Add(24 * time.Hour), false},
		{"status exactly the last paste's", after, idleSince(m0.Add(time.Minute)), m0.Add(time.Hour), false},
		{"status newer than the last paste's, 29s", after, idleSince(m0.Add(2 * time.Minute)), m0.Add(2*time.Minute + 29*time.Second), false},
		{"status newer than the last paste's, 30s", after, idleSince(m0.Add(2 * time.Minute)), m0.Add(2*time.Minute + 30*time.Second), true},
		// The same, with the paste known only to the hub (a person's Send).
		{"hub paste: status exactly it", queued, withPasted(idleSince(m0.Add(time.Minute)), m0.Add(time.Minute)), m0.Add(time.Hour), false},
		{"hub paste: status newer, 30s", queued, withPasted(idleSince(m0.Add(2*time.Minute)), m0.Add(time.Minute)), m0.Add(2*time.Minute + 30*time.Second), true},
		// The larger of the two wins.
		{"hub paste newer than the table's", after, withPasted(idleSince(m0.Add(2*time.Minute)), m0.Add(3*time.Minute)), m0.Add(time.Hour), false},
	} {
		got := Decide(c.h, c.st, c.now, idle)
		if got.Send != c.send {
			t.Errorf("%s: Decide = %+v, want send=%v", c.name, got, c.send)
		}
		if !got.Send && got.Why == "" {
			t.Errorf("%s: held with no reason", c.name)
		}
	}
}

func withPasted(st AgentState, at time.Time) AgentState {
	st.Pasted = at.UnixMilli()
	return st
}

// TestDecideUnderClockSkew is why the rule never mixes the two clocks. A
// message is pasted at muster time P while the window holds status time S0
// (host clock); the turn it starts lasts L and ends with a status write at
// host time P+L+skew; the server observes that write 2 s later, at muster
// time O. With the host 60 s BEHIND (a 20 s turn and a 70 s turn), 60 s
// AHEAD, and in step: before the turn ends the next message is held; it is
// held 29 s after O and sent 30 s after O — IdleFor on muster's own clock —
// and a day later it is still sendable, so nothing stalls. Both ways the paste
// can be known: on the row (TurnAfter) and only to the hub (Pasted).
//
// The rule this replaced compared the host's time with muster's: 60 s behind,
// a 20 s turn's end read as "before the paste" and the queue stalled forever,
// and a 70 s turn released the next message at once instead of 30 s later; 60
// s ahead it waited 90 s.
func TestDecideUnderClockSkew(t *testing.T) {
	p := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		skew time.Duration // host clock minus muster's
		turn time.Duration
	}{
		{"in step, 20 s turn", 0, 20 * time.Second},
		{"in step, 70 s turn", 0, 70 * time.Second},
		{"host 60 s behind, 20 s turn", -time.Minute, 20 * time.Second},
		{"host 60 s behind, 70 s turn", -time.Minute, 70 * time.Second},
		{"host 60 s ahead, 20 s turn", time.Minute, 20 * time.Second},
		{"host 60 s ahead, 70 s turn", time.Minute, 70 * time.Second},
	} {
		host := func(m time.Time) int64 { return m.Add(c.skew).UnixMilli() }
		s0 := host(p.Add(-5 * time.Minute)) // the idle the paste was made in
		ended := host(p.Add(c.turn))        // the turn's own idle write
		observed := p.Add(c.turn + 2*time.Second)
		for _, known := range []string{"on the row", "to the hub only"} {
			h := Head{Item: Item{State: Queued}}
			pasted := int64(0)
			if known == "on the row" {
				h.TurnAfter = s0
			} else {
				pasted = s0
			}
			before := AgentState{Idle: true, Since: s0, Observed: p.Add(-5 * time.Minute), Pasted: pasted}
			afterTurn := AgentState{Idle: true, Since: ended, Observed: observed, Pasted: pasted}
			for _, step := range []struct {
				what string
				st   AgentState
				now  time.Time
				send bool
			}{
				{"the turn has not ended", before, p.Add(c.turn), false},
				{"the turn has not ended, a day later", before, p.Add(24 * time.Hour), false},
				{"29 s after the server saw it end", afterTurn, observed.Add(29 * time.Second), false},
				{"30 s after the server saw it end", afterTurn, observed.Add(30 * time.Second), true},
				{"a day after", afterTurn, observed.Add(24 * time.Hour), true},
			} {
				if got := Decide(h, step.st, step.now, idle); got.Send != step.send {
					t.Errorf("%s, paste known %s, %s: Decide = %+v, want send=%v", c.name, known, step.what, got, step.send)
				}
			}
		}
	}
}

// TestATurnNobodySawBusyStillReleasesTheNext is why idle-since is Claude
// Code's own timestamp: after a message is sent the target may never be SEEN
// busy (its whole turn fell between two host refreshes), and still reports a
// newer idle-since — which alone releases the next message, IdleFor later.
// Without a newer one the next is held (a day later), never released by time.
func TestATurnNobodySawBusyStillReleasesTheNext(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now().Truncate(time.Second)
	c := &clock{t: t0.Add(idle)}
	f := &fakeTarget{st: idleSince(t0)}
	d := newDispatcher(s, f, c)
	mustEnqueue(t, s, 2, mirrorItem("one"))
	mustEnqueue(t, s, 2, mirrorItem("two"))
	tick(t, d)
	wantSent(t, f, "idle 30 s", "one")
	c.set(t0.Add(24 * time.Hour))
	tick(t, d)
	wantSent(t, f, "the same idle-since a day later", "one")
	// The turn "one" started ran and ended unseen: idle both times, newer time.
	turnEnded := t0.Add(24*time.Hour + time.Second)
	f.set(idleSince(turnEnded))
	c.set(turnEnded.Add(idle - time.Second))
	tick(t, d)
	wantSent(t, f, "a newer idle-since, 29 s", "one")
	c.set(turnEnded.Add(idle))
	tick(t, d)
	wantSent(t, f, "a newer idle-since, 30 s", "one", "two")
}

// TestTheHooksActAsTheMessagesOwner: the dispatcher reads every owner's heads
// under the system scope, and hands Probe and Send ctx AS EACH HEAD'S OWNER —
// two owners, two targets, each hook call as its own row's owner. And with no
// AsOwner it refuses to run at all rather than probe as the system.
func TestTheHooksActAsTheMessagesOwner(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now().Truncate(time.Second)
	c := &clock{t: t0.Add(idle)}
	f := &fakeTarget{st: idleSince(t0)}
	d := newDispatcher(s, f, c)
	mustEnqueue(t, s, 2, mirrorItem("user 2's"))
	mustEnqueue(t, s, 3, mirrorItem("user 3's"))
	tick(t, d)
	wantSent(t, f, "both owners' heads", "user 2's", "user 3's")
	f.mu.Lock()
	probed, sent := f.probedAs, f.sentAs
	f.mu.Unlock()
	if len(probed) != 2 || len(sent) != 2 {
		t.Fatalf("probed %v, sent %v; want two each", probed, sent)
	}
	for _, p := range append(probed, sent...) {
		if p[0] == 0 || p[0] != p[1] {
			t.Fatalf("a hook ran for owner %d as owner %d (probed %v, sent %v)", p[0], p[1], probed, sent)
		}
	}
	d.AsOwner = nil
	if _, err := d.Tick(context.Background()); err == nil {
		t.Fatal("a dispatcher with no AsOwner ticked")
	}
}
