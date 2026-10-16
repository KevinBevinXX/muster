package sendqueue

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// THE CLAIM'S LEASE AND THE BOUND UNDER CONCURRENCY: two muster pods overlap
// during a rolling update (RollingUpdate, maxSurge 1), so a dispatcher that
// starts must not fail the other's live send, and two Enqueues racing for the
// last place must not both fit.

// TestTwoConcurrentEnqueuesCannotBothPassTheBound: with MaxOpenPerTarget-1
// messages open, two Enqueues race for the last place. Each is held after its
// count and before its commit until the other reaches the same point (or a
// second passes): without the advisory lock both get there, each having counted
// its own row and not the other's uncommitted one, and both commit; with it the
// second waits for the first to commit, counts both, and is refused. Exactly
// one fits.
func TestTwoConcurrentEnqueuesCannotBothPassTheBound(t *testing.T) {
	s, _ := pgStore(t)
	for i := 0; i < MaxOpenPerTarget-1; i++ {
		mustEnqueue(t, s, 2, mirrorItem("m"))
	}
	var mu sync.Mutex
	arrived := 0
	both := make(chan struct{})
	s.afterCount = func() {
		mu.Lock()
		arrived++
		if arrived == 2 {
			close(both)
		}
		mu.Unlock()
		select {
		case <-both:
		case <-time.After(time.Second):
		}
	}
	errs := make(chan error, 2)
	for _, b := range []string{"racer one", "racer two"} {
		go func() {
			_, err := s.Enqueue(as(2), mirrorItem(b))
			errs <- err
		}()
	}
	var ok, full int
	for range 2 {
		switch err := <-errs; {
		case err == nil:
			ok++
		case errors.Is(err, ErrFull):
			full++
		default:
			t.Fatal(err)
		}
	}
	s.afterCount = nil
	items, err := s.List(as(2), win)
	if err != nil {
		t.Fatal(err)
	}
	if ok != 1 || full != 1 || len(items) != MaxOpenPerTarget {
		t.Fatalf("two racing Enqueues for the last place: %d queued, %d refused, %d open; want 1, 1, %d",
			ok, full, len(items), MaxOpenPerTarget)
	}
}

// TestCancelOwnerCancelsOnlyThatOwnersQueuedMessages: a disable or a sign-out
// everywhere cancels every message the user still has queued, saying why — on
// every target — and leaves a message already being sent, and another user's
// queue, alone.
func TestCancelOwnerCancelsOnlyThatOwnersQueuedMessages(t *testing.T) {
	s, _ := pgStore(t)
	sys := context.Background()
	sending := mustEnqueue(t, s, 2, mirrorItem("being sent"))
	now := time.Now()
	if _, ok, err := s.Claim(sys, sending.ID, now, claimFor("a", now)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	q1 := mustEnqueue(t, s, 2, mirrorItem("queued"))
	o := mirrorItem("queued elsewhere")
	o.Target.Window = "@8"
	q2 := mustEnqueue(t, s, 2, o)
	theirs := mustEnqueue(t, s, 3, mirrorItem("another user's"))
	const why = "Cancelled: the account was disabled."
	n, err := s.CancelOwner(sys, 2, now, why)
	if err != nil || n != 2 {
		t.Fatalf("CancelOwner = %d, %v; want 2", n, err)
	}
	for _, c := range []struct {
		uid   int64
		id    int64
		state State
	}{{2, q1.ID, Cancelled}, {2, q2.ID, Cancelled}, {2, sending.ID, Sending}, {3, theirs.ID, Queued}} {
		got, err := s.Get(as(c.uid), c.id)
		if err != nil || got.State != c.state {
			t.Fatalf("message %d = %+v, %v; want %s", c.id, got, err, c.state)
		}
		if c.state == Cancelled && got.Detail != why {
			t.Fatalf("message %d detail = %q, want %q", c.id, got.Detail, why)
		}
	}
}

// blockingSend is a send path that blocks until released, saying when it has
// been entered.
type blockingSend struct {
	entered, release chan struct{}
	mu               sync.Mutex
	n                int
}

func newBlockingSend() *blockingSend {
	return &blockingSend{entered: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingSend) send(context.Context, Item, AgentState) Outcome {
	b.mu.Lock()
	b.n++
	first := b.n == 1
	b.mu.Unlock()
	if first {
		close(b.entered)
	}
	<-b.release
	return Outcome{Sent: true, Detail: "Sent."}
}

// pod is a dispatcher named instance on s with its own clock, lease and
// renewal period.
func pod(s *PGStore, instance string, f *fakeTarget, c *clock, send Sender, lease, renew time.Duration) *Dispatcher {
	d := newDispatcher(s, f, c)
	d.Instance, d.Lease, d.RenewEvery, d.Send = instance, lease, renew, send
	return d
}

// startSending runs d's Tick in the background until its send is entered, and
// returns where the Tick's error lands.
func startSending(t *testing.T, d *Dispatcher, bs *blockingSend) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { _, err := d.Tick(context.Background()); done <- err }()
	select {
	case <-bs.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the pod never started its send")
	}
	return done
}

// rowState fails the test unless message id is in state want.
func rowState(t *testing.T, s *PGStore, id int64, when string, want State) Item {
	t.Helper()
	got, err := s.Get(as(2), id)
	if err != nil || got.State != want {
		t.Fatalf("%s: the row = %+v, %v; want %s", when, got, err, want)
	}
	return got
}

// TestARollingUpdateDoesNotFailAPeersLiveSend: pod A is mid-send when pod B
// starts. B's Start and its passes leave A's row sending — inside the lease,
// and still several leases later, because A renews it while the send runs —
// and B sends nothing to the target meanwhile. When A's send returns, A
// finishes its own row: sent. (The lease runs on the database's clock, so the
// test waits real time: a 300 ms lease, renewed every 20 ms, watched for 1 s.)
func TestARollingUpdateDoesNotFailAPeersLiveSend(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now()
	c := &clock{t: t0}
	f := &fakeTarget{st: idleSince(t0.Add(-time.Hour))}
	m := mustEnqueue(t, s, 2, mirrorItem("in flight on pod A"))
	bs := newBlockingSend()
	const lease = 300 * time.Millisecond
	a := pod(s, "pod-a", f, c, bs.send, lease, 20*time.Millisecond)
	done := startSending(t, a, bs)

	b := pod(s, "pod-b", f, c, f.send, lease, 20*time.Millisecond)
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	tick(t, b)
	first := rowState(t, s, m.ID, "pod B started inside A's lease", Sending)
	if first.ClaimedBy != "pod-a" {
		t.Fatalf("the row is claimed by %q, want pod-a", first.ClaimedBy)
	}
	// Several leases later: A's renewals have carried it.
	time.Sleep(4 * lease)
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	tick(t, b)
	if got := rowState(t, s, m.ID, "pod B, four leases later, A still sending", Sending); !got.LeaseUntil.After(first.LeaseUntil.Add(2 * lease)) {
		t.Fatalf("pod A's lease did not move with its renewals: %v, first %v", got.LeaseUntil, first.LeaseUntil)
	}
	if n := len(f.sent()); n != 0 {
		t.Fatalf("pod B sent %d message(s) while A was sending", n)
	}
	close(bs.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := rowState(t, s, m.ID, "A's send returned", Sent); got.Detail != "Sent." {
		t.Fatalf("A's outcome = %+v", got)
	}
}

// TestALeaseIsOnTheDatabasesClockNotThePods: the lease is written and judged
// on Postgres's clock, so two pods whose clocks disagree by ten minutes judge
// each other's leases correctly. Pod A, ten minutes BEHIND, is mid-send: pod
// B, ten minutes AHEAD, leaves the row alone. Pod C, ten minutes ahead, claims
// and stops (no renewal): once the 300 ms lease has run out on the database's
// clock, an in-step pod D fails it — it does not wait ten minutes.
func TestALeaseIsOnTheDatabasesClockNotThePods(t *testing.T) {
	s, _ := pgStore(t)
	now := time.Now()
	behind, ahead, inStep := &clock{t: now.Add(-10 * time.Minute)}, &clock{t: now.Add(10 * time.Minute)}, &clock{t: now}
	f := &fakeTarget{st: idleSince(now.Add(-time.Hour))}

	live := mustEnqueue(t, s, 2, mirrorItem("pod A, behind, sending"))
	bs := newBlockingSend()
	a := pod(s, "pod-a", f, behind, bs.send, 5*time.Second, time.Hour)
	done := startSending(t, a, bs)
	tick(t, pod(s, "pod-b", f, ahead, f.send, 5*time.Second, time.Hour))
	rowState(t, s, live.ID, "a pod ten minutes ahead, inside A's lease", Sending)
	close(bs.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	rowState(t, s, live.ID, "A's send returned", Sent)

	o := mirrorItem("pod C, ahead, stopped")
	o.Target.Window = "@8"
	dead := mustEnqueue(t, s, 2, o)
	bs = newBlockingSend()
	cpod := pod(s, "pod-c", f, ahead, bs.send, 300*time.Millisecond, time.Hour)
	done = startSending(t, cpod, bs)
	time.Sleep(600 * time.Millisecond)
	tick(t, pod(s, "pod-d", f, inStep, f.send, 5*time.Second, time.Hour))
	if got := rowState(t, s, dead.ID, "C's lease ran out on the database's clock", Failed); got.Detail != RecoveredDetail {
		t.Fatalf("the stopped pod's row = %+v; want %q", got, RecoveredDetail)
	}
	close(bs.release)
	<-done
}

// TestAnExpiredLeaseIsFailedMayHaveDelivered: pod A claimed a row and stopped
// (no renewal, no Finish). Inside the lease pod B's passes leave it, and B's
// own Finish of it (not B's row) changes nothing; once the lease has expired
// B's next pass fails it — may have been delivered, saying so — and nobody
// sends it again.
func TestAnExpiredLeaseIsFailedMayHaveDelivered(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now()
	c := &clock{t: t0}
	f := &fakeTarget{st: idleSince(t0.Add(-time.Hour))}
	m := mustEnqueue(t, s, 2, mirrorItem("A stopped mid-send"))
	const lease = 300 * time.Millisecond
	if _, ok, err := s.Claim(context.Background(), m.ID, t0, Claim{By: "pod-a", Lease: lease}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	b := pod(s, "pod-b", f, c, f.send, lease, 20*time.Millisecond)
	tick(t, b)
	if err := s.Finish(context.Background(), m.ID, "pod-b", Outcome{Sent: true, Detail: "not B's"}, c.now()); err != nil {
		t.Fatal(err)
	}
	rowState(t, s, m.ID, "inside the lease, after another pod's pass and Finish", Sending)
	time.Sleep(2 * lease)
	tick(t, b)
	got := rowState(t, s, m.ID, "after the lease expired", Failed)
	if !got.MayHaveDelivered || got.Detail != RecoveredDetail {
		t.Fatalf("after the lease expired: %+v; want may have been delivered, %q", got, RecoveredDetail)
	}
	c.set(t0.Add(time.Hour))
	tick(t, b)
	wantSent(t, f, "after the recovery", nil...)
}

// TestOnlyTheClaimantRenewsAndAClaimNeedsALease: Renew by another instance
// is refused and leaves the lease where it was; the claimant's moves it. A
// claim with no instance, or no lease, is refused.
func TestOnlyTheClaimantRenewsAndAClaimNeedsALease(t *testing.T) {
	s, _ := pgStore(t)
	sys := context.Background()
	m := mustEnqueue(t, s, 2, mirrorItem("x"))
	now := time.Now()
	for _, bad := range []Claim{{By: "", Lease: time.Minute}, {By: "pod-a", Lease: 0}} {
		if _, ok, err := s.Claim(sys, m.ID, now, bad); err == nil || ok {
			t.Fatalf("claim %+v = %v, %v; want refused", bad, ok, err)
		}
	}
	if _, ok, err := s.Claim(sys, m.ID, now, Claim{By: "pod-a", Lease: 30 * time.Second}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	first := rowState(t, s, m.ID, "claimed", Sending)
	if ok, err := s.Renew(sys, m.ID, "pod-b", time.Hour); err != nil || ok {
		t.Fatalf("pod-b renewing pod-a's claim = %v, %v; want refused", ok, err)
	}
	if got := rowState(t, s, m.ID, "after pod-b's Renew", Sending); !got.LeaseUntil.Equal(first.LeaseUntil) {
		t.Fatalf("another instance's Renew moved the lease to %v (was %v)", got.LeaseUntil, first.LeaseUntil)
	}
	if ok, err := s.Renew(sys, m.ID, "pod-a", time.Hour); err != nil || !ok {
		t.Fatalf("CONTROL: the claimant's Renew = %v, %v", ok, err)
	}
	if got := rowState(t, s, m.ID, "after the claimant's Renew", Sending); !got.LeaseUntil.After(first.LeaseUntil.Add(59 * time.Minute)) {
		t.Fatalf("the claimant's Renew left the lease at %v (was %v)", got.LeaseUntil, first.LeaseUntil)
	}
}
