package sendqueue

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE DISPATCHER AGAINST A SESSION UNDER A USAGE LIMIT, on the real store with
// a fake clock: the drain (every queued message sent into the limit, one per
// idle stretch) is held, the one message the limit bounced is marked failed for
// a Retry by hand, nothing is re-sent on its own, and the queue resumes after
// the reset — each with its own control.

// cli is a fake Claude Code behind the send path: each message pasted into it
// ends its turn at once (its status goes idle with a NEWER time, observed by
// muster now, as the real CLI's does), with the turn error reply names — nil
// for a real answer.
type cli struct {
	*fakeTarget
	c     *clock
	reply func(body string) *mirror.TurnError
}

// cliSession is the fake CLI's Claude Code session id (the window's).
const cliSession = "sess-1"

func newCLI(c *clock, start time.Time, reply func(string) *mirror.TurnError) *cli {
	st := idleSince(start)
	st.Session = cliSession
	return &cli{fakeTarget: &fakeTarget{st: st}, c: c, reply: reply}
}

func (l *cli) send(ctx context.Context, it Item, st AgentState) Outcome {
	o := l.fakeTarget.send(ctx, it, st)
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.reply(it.Body)
	if e != nil && e.Rejected && e.Prompt == "" {
		// The host names the refused prompt, and the newest record before it:
		// the previous turn's end, no later than the status it was pasted on.
		e.Prompt, e.PrevAt = mirror.PromptDigest(it.Body), st.Since
	}
	l.st = AgentState{Idle: true, Since: l.st.Since + 700, Observed: l.c.now(), TurnError: e, Session: l.st.Session}
	return o
}

// answer is a turn the person started by hand that the CLI answered for real
// (or with e): idle, newer, observed now.
func (l *cli) answer(e *mirror.TurnError) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.st = AgentState{Idle: true, Since: l.st.Since + 5000, Observed: l.c.now(), TurnError: e, Session: l.st.Session}
}

func limitDispatcher(s *PGStore, l *cli, c *clock) *Dispatcher {
	d := newDispatcher(s, l.fakeTarget, c)
	d.Send = l.send
	return d
}

// limited is the CLI's usage-limit refusal of every message: the limit is its
// only reply.
func limited(reset time.Time) func(string) *mirror.TurnError {
	return func(string) *mirror.TurnError { return limitErr(reset, true) }
}

func row(t *testing.T, s *PGStore, id int64) Item {
	t.Helper()
	it, err := s.Get(as(2), id)
	if err != nil {
		t.Fatal(err)
	}
	return it
}

// wantBounced: the row was marked failed for the limit, not delivered, and a
// Retry is open to it (failed, not yet retried, a person's own).
func wantBounced(t *testing.T, s *PGStore, id int64) {
	t.Helper()
	r := row(t, s, id)
	if r.State != Failed || r.Detail != RejectedLimitDetail || r.MayHaveDelivered || r.RetriedAs != 0 || r.RouteID != "" {
		t.Fatalf("the bounced message: %+v; want failed, %q, not delivered, retryable", r, RejectedLimitDetail)
	}
}

// TestAUsageLimitHoldsTheQueueAndFailsTheBouncedMessageForARetryByHand is the
// operator's report end to end. Three messages queued; the session hits its
// limit. Before the fix every pass sent the next one (the CONTROL further
// down: the same CLI with no turn error drains). Now: the first is sent, the
// limit refuses it, it is marked failed "Rejected: usage limit" with its Retry
// open, and nothing else is sent until the reset plus the margin — and the
// bounced one is NEVER re-sent on its own, then or after.
func TestAUsageLimitHoldsTheQueueAndFailsTheBouncedMessageForARetryByHand(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now().Truncate(time.Second)
	c := &clock{t: t0}
	reset := t0.Add(2 * time.Hour)
	l := newCLI(c, t0.Add(-time.Hour), limited(reset))
	d := limitDispatcher(s, l, c)
	one := mustEnqueue(t, s, 2, mirrorItem("one"))
	two := mustEnqueue(t, s, 2, mirrorItem("two"))
	three := mustEnqueue(t, s, 2, mirrorItem("three"))

	for i := 1; i <= 20; i++ { // ten minutes of passes, each past IdleFor
		c.set(t0.Add(time.Duration(i) * idle))
		tick(t, d)
	}
	wantSent(t, l.fakeTarget, "under the limit", "one")
	wantBounced(t, s, one.ID)
	for _, id := range []int64{two.ID, three.ID} {
		if r := row(t, s, id); r.State != Queued {
			t.Fatalf("message %d: %s; want queued, never sent", id, r.State)
		}
	}
	h, ok, err := s.HeadOf(as(2), win)
	if err != nil || !ok || h.ID != two.ID {
		t.Fatalf("head = %+v, %v, %v; want the next message, held", h, ok, err)
	}
	if dec := d.Decide(h, l.probe(context.Background(), h)); dec.Send || !dec.Held || !dec.ResumeAt.Equal(reset) {
		t.Fatalf("Decide = %+v; want held until the reset %s", dec, reset)
	}

	// Just inside the margin: still held.
	c.set(reset.Add(LimitMargin - time.Second))
	tick(t, d)
	wantSent(t, l.fakeTarget, "inside the margin", "one")

	// Past it, the limit lifted: the rest go, one per turn — and the bounced
	// message is not among them.
	l.reply = func(string) *mirror.TurnError { return nil }
	for i := 0; i <= 6; i++ {
		c.set(reset.Add(LimitMargin + time.Duration(i)*idle))
		tick(t, d)
	}
	wantSent(t, l.fakeTarget, "the limit lifted", "one", "two", "three")
	wantBounced(t, s, one.ID)

	// Its Retry, by hand, queues it again as a new row, which is sent.
	again, err := s.Retry(as(2), one.ID, time.Now(), 0)
	if err != nil {
		t.Fatalf("Retry of the bounced message: %v", err)
	}
	for i := 7; i <= 9; i++ {
		c.set(reset.Add(LimitMargin + time.Duration(i)*idle))
		tick(t, d)
	}
	wantSent(t, l.fakeTarget, "after a Retry by hand", "one", "two", "three", "one")
	if r := row(t, s, again.ID); r.State != Sent {
		t.Fatalf("the retried row: %s; want sent", r.State)
	}
}

// TestWithoutATurnErrorTheSameCLIDrainsTheQueue is the control the test above
// needs: the very same CLI (each message ends its turn at once, idle, newer)
// with the host reporting nothing about how the turn ended — what every host
// sent before this — and the queue goes out one message per pass. This is the
// drain the operator saw; it is what a host without the turn error still does.
func TestWithoutATurnErrorTheSameCLIDrainsTheQueue(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now().Truncate(time.Second)
	c := &clock{t: t0}
	l := newCLI(c, t0.Add(-time.Hour), func(string) *mirror.TurnError { return nil })
	d := limitDispatcher(s, l, c)
	for _, b := range []string{"one", "two", "three"} {
		mustEnqueue(t, s, 2, mirrorItem(b))
	}
	for i := 1; i <= 6; i++ {
		c.set(t0.Add(time.Duration(i) * idle))
		tick(t, d)
	}
	wantSent(t, l.fakeTarget, "no turn error reported", "one", "two", "three")
}

// TestALimitHitMidTurnHoldsButFailsNothing: the host does not mark the turn
// Rejected when the limit came after real work (the message WAS processed). It
// stays sent — a Retry of it would run it twice — and the queue still holds.
func TestALimitHitMidTurnHoldsButFailsNothing(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now().Truncate(time.Second)
	c := &clock{t: t0}
	l := newCLI(c, t0.Add(-time.Hour), func(string) *mirror.TurnError { return limitErr(t0.Add(time.Hour), false) })
	d := limitDispatcher(s, l, c)
	one := mustEnqueue(t, s, 2, mirrorItem("one"))
	mustEnqueue(t, s, 2, mirrorItem("two"))
	for i := 1; i <= 6; i++ {
		c.set(t0.Add(time.Duration(i) * idle))
		tick(t, d)
	}
	wantSent(t, l.fakeTarget, "a limit mid-turn", "one")
	if r := row(t, s, one.ID); r.State != Sent {
		t.Fatalf("the processed message: %s %q; want sent", r.State, r.Detail)
	}
}

// TestAPasteThroughMusterSinceFailsNothing: a person's own Send got in after
// the queued message (the hub's Pasted moved past it) and the limit answered
// THAT: the queue cannot tell what became of its own message, and leaves it
// sent.
func TestAPasteThroughMusterSinceFailsNothing(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now().Truncate(time.Second)
	c := &clock{t: t0}
	l := newCLI(c, t0.Add(-time.Hour), limited(t0.Add(time.Hour)))
	d := limitDispatcher(s, l, c)
	one := mustEnqueue(t, s, 2, mirrorItem("one"))
	c.set(t0.Add(idle))
	// The person's Send lands right after the queued paste: the hub records
	// the status the window held then.
	d.Send = func(ctx context.Context, it Item, st AgentState) Outcome {
		o := l.send(ctx, it, st)
		l.mu.Lock()
		l.st.Pasted = l.st.Since
		l.st.Since += 700
		l.mu.Unlock()
		return o
	}
	for i := 1; i <= 4; i++ {
		c.set(t0.Add(time.Duration(i) * idle))
		tick(t, d)
	}
	if r := row(t, s, one.ID); r.State != Sent {
		t.Fatalf("message: %s %q; want sent", r.State, r.Detail)
	}
}

// TestABouncedMessageIsRejectedOncePerSend: two dispatchers (two pods during a
// rolling update) both see the bounce; the row is failed ONCE — Reject moves
// only a row still sent — and is not sent again.
func TestABouncedMessageIsRejectedOncePerSend(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now().Truncate(time.Second)
	c := &clock{t: t0}
	l := newCLI(c, t0.Add(-time.Hour), limited(t0.Add(time.Hour)))
	d := limitDispatcher(s, l, c)
	one := mustEnqueue(t, s, 2, mirrorItem("one"))
	queued := mustEnqueue(t, s, 2, mirrorItem("queued"))
	c.set(t0.Add(idle))
	if _, err := d.Tick(context.Background()); err != nil { // sent, then refused
		t.Fatal(err)
	}
	other := limitDispatcher(s, l, c)
	other.Instance = "the-other-pod"
	var wg sync.WaitGroup
	for _, x := range []*Dispatcher{d, other, d, other} {
		wg.Add(1)
		go func(x *Dispatcher) { defer wg.Done(); x.rejectBounced(context.Background()) }(x)
	}
	wg.Wait()
	wantBounced(t, s, one.ID)
	for _, id := range []int64{one.ID, queued.ID} {
		if ok, err := s.Reject(context.Background(), id, RejectedLimitDetail); err != nil || ok {
			t.Fatalf("Reject of a row that is not sent (%d) = %v, %v; want refused", id, ok, err)
		}
	}
	if r := row(t, s, queued.ID); r.State != Queued {
		t.Fatalf("the queued row: %s; want untouched", r.State)
	}
	wantSent(t, l.fakeTarget, "bounced", "one")
}

// TestARealAnswerLiftsTheLimitAtOnce: the reset is hours away, but a message
// the person typed by hand was answered for real — the limit has lifted — so
// the next queued message goes at the next idle stretch, not at the reset; the
// bounced one stays failed for its Retry.
func TestARealAnswerLiftsTheLimitAtOnce(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now().Truncate(time.Second)
	c := &clock{t: t0}
	l := newCLI(c, t0.Add(-time.Hour), limited(t0.Add(5*time.Hour)))
	d := limitDispatcher(s, l, c)
	one := mustEnqueue(t, s, 2, mirrorItem("one"))
	mustEnqueue(t, s, 2, mirrorItem("two"))
	for i := 1; i <= 4; i++ {
		c.set(t0.Add(time.Duration(i) * idle))
		tick(t, d)
	}
	wantSent(t, l.fakeTarget, "held", "one")
	l.reply = func(string) *mirror.TurnError { return nil }
	l.answer(nil)
	c.set(c.now().Add(idle))
	tick(t, d)
	wantSent(t, l.fakeTarget, "after a real answer", "one", "two")
	wantBounced(t, s, one.ID)
}

// TestTheCircuitBreakerBacksOffAndLiftsByItself: "Not logged in" fails each
// message at once (the host counts the quick failures in a row). The first
// failure alone is not a loop; at BreakerAfter the queue holds 1 minute from
// when muster saw it, then lets ONE through; that one fails too, and it holds
// 2 minutes, then 5. A real answer resets it: the next goes at once. Each
// message the CLI refused (its only reply the error, nothing between its
// paste and the refusal) is marked failed with the reason and its Retry —
// the breaker's probes included. Send now still sends exactly one while it
// holds.
func TestTheCircuitBreakerBacksOffAndLiftsByItself(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now().Truncate(time.Second)
	c := &clock{t: t0}
	quick := 0
	l := newCLI(c, t0.Add(-time.Hour), func(string) *mirror.TurnError {
		quick++
		return &mirror.TurnError{Kind: "authentication_failed", Rejected: true, Quick: quick}
	})
	d := limitDispatcher(s, l, c)
	d.IdleFor = time.Second
	var items []Item
	for _, b := range []string{"m1", "m2", "m3", "m4", "m5", "m6", "m7"} {
		items = append(items, mustEnqueue(t, s, 2, mirrorItem(b)))
	}
	at := func(off time.Duration) { c.set(t0.Add(off)); tick(t, d) }
	at(2 * time.Second) // m1: one quick failure, no hold
	at(4 * time.Second) // m2: BreakerAfter in a row — held 1 minute from now
	tripped := t0.Add(4 * time.Second)
	at(4*time.Second + time.Minute - time.Second)
	wantSent(t, l.fakeTarget, "inside the first back-off", "m1", "m2")
	h, _, _ := s.HeadOf(as(2), win)
	if dec := d.Decide(h, l.probe(context.Background(), h)); !dec.Held || !dec.ResumeAt.Equal(tripped.Add(time.Minute)) ||
		dec.Why != BreakerHeldText(&mirror.TurnError{Kind: "authentication_failed", Quick: BreakerAfter}) {
		t.Fatalf("Decide = %+v; want held by the breaker until %s", dec, tripped.Add(time.Minute))
	}
	at(4*time.Second + time.Minute) // lifts by itself: m3, which fails too
	wantSent(t, l.fakeTarget, "after the first back-off", "m1", "m2", "m3")
	second := t0.Add(4*time.Second + time.Minute)
	c.set(second.Add(2*time.Minute - time.Second))
	tick(t, d)
	wantSent(t, l.fakeTarget, "inside the second back-off (2 min)", "m1", "m2", "m3")
	c.set(second.Add(2 * time.Minute))
	tick(t, d)
	wantSent(t, l.fakeTarget, "after the second back-off", "m1", "m2", "m3", "m4")
	third := second.Add(2 * time.Minute)
	c.set(third.Add(5*time.Minute - time.Second))
	tick(t, d)
	wantSent(t, l.fakeTarget, "inside the third back-off (5 min)", "m1", "m2", "m3", "m4")
	// Send now: the one message tapped, and only it.
	if _, o, err := d.SendNow(as(2), d.AsOwner(context.Background(), 2), items[6].ID, l.probe(context.Background(), h).Since, l.send); err != nil || !o.Sent {
		t.Fatalf("Send now = %+v, %v", o, err)
	}
	wantSent(t, l.fakeTarget, "a Send now while held", "m1", "m2", "m3", "m4", "m7")
	tick(t, d) // the refusal of m7 seen; still held
	wantSent(t, l.fakeTarget, "still held after the Send now", "m1", "m2", "m3", "m4", "m7")
	// A real answer resets the breaker: the next goes at once.
	l.reply = func(string) *mirror.TurnError { return nil }
	l.answer(nil)
	c.set(c.now().Add(2 * time.Second))
	tick(t, d)
	wantSent(t, l.fakeTarget, "after a real answer", "m1", "m2", "m3", "m4", "m7", "m5")
	for _, it := range append(items[:4:4], items[6]) {
		if r := row(t, s, it.ID); r.State != Failed || r.Detail != RejectedDetail("authentication_failed") || r.MayHaveDelivered || r.RetriedAs != 0 {
			t.Fatalf("%s: %+v; want failed, %q, retryable", it.Body, r, RejectedDetail("authentication_failed"))
		}
	}
}

// TestSendNowDuringALimitSendsOneAndItsBounceIsFailed: the override sends the
// one message tapped, at once; the limit refuses it; it is marked failed like
// any bounced send, and nothing else is sent.
func TestSendNowDuringALimitSendsOneAndItsBounceIsFailed(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now().Truncate(time.Second)
	c := &clock{t: t0}
	l := newCLI(c, t0.Add(-time.Hour), limited(t0.Add(time.Hour)))
	l.st.TurnError = limitErr(t0.Add(time.Hour), true) // the session hit its limit already
	d := limitDispatcher(s, l, c)
	a := mustEnqueue(t, s, 2, mirrorItem("a"))
	b := mustEnqueue(t, s, 2, mirrorItem("b"))
	c.set(t0.Add(idle))
	tick(t, d)
	wantSent(t, l.fakeTarget, "held from the start", nil...)
	if _, o, err := d.SendNow(as(2), d.AsOwner(context.Background(), 2), b.ID, l.probe(context.Background(), Head{}).Since, l.send); err != nil || !o.Sent {
		t.Fatalf("Send now = %+v, %v", o, err)
	}
	for i := 2; i <= 6; i++ {
		c.set(t0.Add(time.Duration(i) * idle))
		tick(t, d)
	}
	wantSent(t, l.fakeTarget, "one Send now under the limit", "b")
	wantBounced(t, s, b.ID)
	if r := row(t, s, a.ID); r.State != Queued {
		t.Fatalf("a: %s; want still queued", r.State)
	}
}

// TestRecentSentIsEachTargetsLatestClaimOnly: a target whose latest claimed
// message FAILED has nothing to check, though an older one was sent; a send
// older than the window is not read.
func TestRecentSentIsEachTargetsLatestClaimOnly(t *testing.T) {
	s, _ := pgStore(t)
	now := time.Now()
	x := mustEnqueue(t, s, 2, mirrorItem("x"))
	y := mustEnqueue(t, s, 2, mirrorItem("y"))
	for i, id := range []int64{x.ID, y.ID} {
		if _, ok, err := s.Claim(context.Background(), id, now.Add(time.Duration(i)*time.Second), claimFor("p", now)); err != nil || !ok {
			t.Fatal(ok, err)
		}
		o := Outcome{Sent: true}
		if id == y.ID {
			o = Outcome{Detail: "Not sent: x"}
		}
		if err := s.Finish(context.Background(), id, "p", o, now); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.RecentSent(context.Background(), now.Add(-time.Minute))
	if err != nil || len(got) != 0 {
		t.Fatalf("RecentSent = %v, %v; want none (the latest claim failed)", bodies(got), err)
	}
	z := mustEnqueue(t, s, 2, mirrorItem("z"))
	if _, ok, err := s.Claim(context.Background(), z.ID, now.Add(5*time.Second), claimFor("p", now)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := s.Finish(context.Background(), z.ID, "p", Outcome{Sent: true}, now); err != nil {
		t.Fatal(err)
	}
	if got, err := s.RecentSent(context.Background(), now.Add(-time.Minute)); err != nil || len(got) != 1 || got[0].ID != z.ID {
		t.Fatalf("RecentSent = %v, %v; want z", bodies(got), err)
	}
	if got, err := s.RecentSent(context.Background(), now.Add(time.Minute)); err != nil || len(got) != 0 {
		t.Fatalf("RecentSent after the window = %v, %v; want none", bodies(got), err)
	}
}

// TestABreakerProbeThatWasNotRefusedOnItsOwnStaysSent: the same "Not logged
// in" loop, but the host does not report the turn Rejected (a reply came
// first): the messages stay sent, and only the hold applies.
func TestABreakerProbeThatWasNotRefusedOnItsOwnStaysSent(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now().Truncate(time.Second)
	c := &clock{t: t0}
	quick := 0
	l := newCLI(c, t0.Add(-time.Hour), func(string) *mirror.TurnError {
		quick++
		return &mirror.TurnError{Kind: "authentication_failed", Quick: quick}
	})
	d := limitDispatcher(s, l, c)
	d.IdleFor = time.Second
	m1 := mustEnqueue(t, s, 2, mirrorItem("m1"))
	m2 := mustEnqueue(t, s, 2, mirrorItem("m2"))
	mustEnqueue(t, s, 2, mirrorItem("m3"))
	for _, off := range []time.Duration{2 * time.Second, 4 * time.Second, 30 * time.Second} {
		c.set(t0.Add(off))
		tick(t, d)
	}
	wantSent(t, l.fakeTarget, "held by the breaker", "m1", "m2")
	for _, it := range []Item{m1, m2} {
		if r := row(t, s, it.ID); r.State != Sent {
			t.Fatalf("%s: %s %q; want sent", it.Body, r.State, r.Detail)
		}
	}
}

// TestAMessageClaudeAnsweredStaysSentWhenALaterTerminalPromptHitsTheLimit is
// the round-1 audit's case through the dispatcher: the queue sends M, Claude
// ANSWERS it, then a prompt typed in the terminal (not through muster: the
// hub's Pasted never moves) hits the limit. The host reports that turn
// Rejected — its only reply was the limit — but for the OTHER prompt, and
// with the newest record before it (M's answer) written after M's paste. M
// stays sent. Its twin with muster restarted in between (Pasted forgotten)
// is the same state, and stays sent too; the control is a true refusal.
func TestAMessageClaudeAnsweredStaysSentWhenALaterTerminalPromptHitsTheLimit(t *testing.T) {
	for _, restarted := range []bool{false, true} {
		s, _ := pgStore(t)
		t0 := time.Now().Truncate(time.Second)
		c := &clock{t: t0}
		l := newCLI(c, t0.Add(-time.Hour), func(string) *mirror.TurnError { return nil }) // M is answered
		d := limitDispatcher(s, l, c)
		m := mustEnqueue(t, s, 2, mirrorItem("M"))
		mustEnqueue(t, s, 2, mirrorItem("next"))
		c.set(t0.Add(idle))
		tick(t, d)
		wantSent(t, l.fakeTarget, "M", "M")
		pastedOn := row(t, s, m.ID).AfterSince
		// Then X, typed in the terminal, hits the limit. The host: Rejected, X's
		// digest, PrevAt = M's answer, after M's paste.
		l.mu.Lock()
		answer := l.st.Since
		l.st = AgentState{Idle: true, Since: answer + 60_000, Observed: c.now(), Session: cliSession, TurnError: &mirror.TurnError{
			Kind: mirror.TurnErrorLimit, Rejected: true, Prompt: mirror.PromptDigest("X"), PrevAt: answer, ResetsAt: t0.Add(time.Hour).UnixMilli()}}
		if restarted {
			l.st.Pasted = 0
		}
		l.mu.Unlock()
		if d2 := limitDispatcher(s, l, c); restarted {
			d = d2 // a new process
		}
		c.set(t0.Add(3 * idle))
		tick(t, d)
		if r := row(t, s, m.ID); r.State != Sent {
			t.Fatalf("restarted %t: M (pasted on %d) = %s %q; want sent — Claude answered it", restarted, pastedOn, r.State, r.Detail)
		}
		// The prompt digest alone would not save it: the same text typed again
		// is still not M's refusal while M's answer lies between.
		l.mu.Lock()
		l.st.TurnError.Prompt = mirror.PromptDigest("M")
		l.mu.Unlock()
		c.set(t0.Add(4 * idle))
		tick(t, d)
		if r := row(t, s, m.ID); r.State != Sent {
			t.Fatalf("restarted %t: M = %s; want sent — its answer was written after its paste", restarted, r.State)
		}
		wantSent(t, l.fakeTarget, "held on the limit", "M")
	}
}

// TestAFreshClaudeInTheSamePaneNeverFailsAnAnsweredMessage (F1): the queue
// sends M into session sess-1, and Claude answers it. Then a fresh `claude`
// starts in the same pane (a new session) and its FIRST prompt — M's exact
// text — hits the limit: nothing before it in that transcript (PrevAt 0), its
// digest M's. It is not M's refusal: M stays sent. A dispatcher that does not
// know which session M was pasted into (restarted) leaves even a true refusal
// sent. CONTROL: the same refusal in the session M was pasted into fails it.
func TestAFreshClaudeInTheSamePaneNeverFailsAnAnsweredMessage(t *testing.T) {
	for _, c := range []struct {
		name      string
		session   string
		restarted bool
		fails     bool
	}{
		{"a new session, its first prompt M's text", "sess-2", false, false},
		{"the same session, but muster restarted (no record)", cliSession, true, false},
		{"CONTROL: the same session, a true refusal", cliSession, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := pgStore(t)
			t0 := time.Now().Truncate(time.Second)
			clk := &clock{t: t0}
			l := newCLI(clk, t0.Add(-time.Hour), func(string) *mirror.TurnError { return nil })
			d := limitDispatcher(s, l, clk)
			m := mustEnqueue(t, s, 2, mirrorItem("M"))
			clk.set(t0.Add(idle))
			tick(t, d)
			wantSent(t, l.fakeTarget, "M", "M")
			pastedOn := row(t, s, m.ID).AfterSince
			l.mu.Lock()
			l.st = AgentState{Idle: true, Since: l.st.Since + 60_000, Observed: clk.now(), Session: c.session, TurnError: &mirror.TurnError{
				Kind: mirror.TurnErrorLimit, Rejected: true, Prompt: mirror.PromptDigest("M"), PrevAt: 0, ResetsAt: t0.Add(time.Hour).UnixMilli()}}
			if c.fails {
				l.st.TurnError.PrevAt = pastedOn
			}
			l.mu.Unlock()
			if c.restarted {
				d = limitDispatcher(s, l, clk)
			}
			clk.set(t0.Add(2 * idle))
			tick(t, d)
			if r := row(t, s, m.ID); (r.State == Failed) != c.fails {
				t.Fatalf("M = %s %q; want failed %t", r.State, r.Detail, c.fails)
			}
		})
	}
}

// TestThePasteRecordsAreForgottenAfterBounceWithin: the in-memory record of
// which session a message was pasted into is dropped once no send that old is
// checked again (RecentSent's window), so it does not grow without bound.
func TestThePasteRecordsAreForgottenAfterBounceWithin(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now().Truncate(time.Second)
	c := &clock{t: t0}
	l := newCLI(c, t0.Add(-time.Hour), func(string) *mirror.TurnError { return nil })
	d := limitDispatcher(s, l, c)
	m := mustEnqueue(t, s, 2, mirrorItem("M"))
	c.set(t0.Add(idle))
	tick(t, d)
	if d.pastedSession(m.ID) != cliSession {
		t.Fatalf("the record = %q; want %q", d.pastedSession(m.ID), cliSession)
	}
	c.set(t0.Add(idle + BounceWithin - time.Second))
	tick(t, d)
	if d.pastedSession(m.ID) != cliSession {
		t.Fatal("forgotten inside BounceWithin")
	}
	c.set(t0.Add(idle + BounceWithin + time.Second))
	tick(t, d)
	if got := d.pastedSession(m.ID); got != "" {
		t.Fatalf("the record past BounceWithin = %q; want forgotten", got)
	}
}
