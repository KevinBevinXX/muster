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
// messages open, two Enqueues race for the last place. Each is held between its
// insert and its count until the other reaches the same point (or a second
// passes): without the advisory lock both get there, each counts its own row
// and not the other's uncommitted one, and both pass; with it the second waits
// for the first to commit, counts both, and is refused. Exactly one fits.
func TestTwoConcurrentEnqueuesCannotBothPassTheBound(t *testing.T) {
	s, _ := pgStore(t)
	for i := 0; i < MaxOpenPerTarget-1; i++ {
		mustEnqueue(t, s, 2, mirrorItem("m"))
	}
	var mu sync.Mutex
	arrived := 0
	both := make(chan struct{})
	s.afterInsert = func() {
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
	s.afterInsert = nil
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

func (b *blockingSend) send(context.Context, Item) Outcome {
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

func (b *blockingSend) calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n
}

// pod is a dispatcher named instance on s, with a fast renewal.
func pod(s *PGStore, instance string, f *fakeTarget, c *clock, send Sender) *Dispatcher {
	d := newDispatcher(s, f, c)
	d.Instance, d.Lease, d.RenewEvery, d.Send = instance, 30*time.Second, 10*time.Millisecond, send
	return d
}

// TestARollingUpdateDoesNotFailAPeersLiveSend: pod A is mid-send when pod B
// starts. B's Start and its passes leave A's row sending — inside the lease,
// and still minutes later, because A renews it while the send runs — and B
// sends nothing to the target meanwhile. When A's send returns, A finishes
// its own row: sent.
func TestARollingUpdateDoesNotFailAPeersLiveSend(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now().Truncate(time.Second)
	c := &clock{t: t0.Add(idle)}
	f := &fakeTarget{st: idleSince(t0)}
	m := mustEnqueue(t, s, 2, mirrorItem("in flight on pod A"))
	bs := newBlockingSend()
	a := pod(s, "pod-a", f, c, bs.send)
	done := make(chan error, 1)
	go func() { _, err := a.Tick(context.Background()); done <- err }()
	select {
	case <-bs.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("pod A never started its send")
	}

	b := pod(s, "pod-b", f, c, f.send)
	state := func(when string, want State) Item {
		t.Helper()
		got, err := s.Get(as(2), m.ID)
		if err != nil || got.State != want {
			t.Fatalf("%s: the row = %+v, %v; want %s", when, got, err, want)
		}
		return got
	}
	c.set(t0.Add(idle + 5*time.Second))
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	tick(t, b)
	if got := state("pod B started inside A's lease", Sending); got.ClaimedBy != "pod-a" {
		t.Fatalf("the row is claimed by %q, want pod-a", got.ClaimedBy)
	}
	// Far past the first lease: A's renewals have carried it.
	later := t0.Add(idle + 5*time.Minute)
	c.set(later)
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := state("while A renews", Sending)
		if got.LeaseUntil.After(later) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pod A never renewed its lease past %v (lease until %v)", later, got.LeaseUntil)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	tick(t, b)
	state("pod B started minutes later, A still sending", Sending)
	if n := len(f.sent()); n != 0 {
		t.Fatalf("pod B sent %d message(s) while A was sending", n)
	}
	close(bs.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := state("A's send returned", Sent); got.Detail != "Sent." {
		t.Fatalf("A's outcome = %+v", got)
	}
}

// TestAnExpiredLeaseIsFailedMayHaveDelivered: pod A claimed a row and stopped
// (no renewal, no Finish). Inside the lease pod B's passes leave it, and B's
// own Finish of it (not B's row) changes nothing; once the lease has expired
// B's next pass fails it — may have been delivered, saying so — and nobody
// sends it again.
func TestAnExpiredLeaseIsFailedMayHaveDelivered(t *testing.T) {
	s, _ := pgStore(t)
	t0 := time.Now().Truncate(time.Second)
	c := &clock{t: t0}
	f := &fakeTarget{st: idleSince(t0.Add(-time.Hour))}
	m := mustEnqueue(t, s, 2, mirrorItem("A stopped mid-send"))
	if _, ok, err := s.Claim(context.Background(), m.ID, t0, Claim{By: "pod-a", Until: t0.Add(30 * time.Second)}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	b := pod(s, "pod-b", f, c, f.send)
	c.set(t0.Add(10 * time.Second))
	tick(t, b)
	if err := s.Finish(context.Background(), m.ID, "pod-b", Outcome{Sent: true, Detail: "not B's"}, c.now()); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(as(2), m.ID); got.State != Sending {
		t.Fatalf("inside the lease, after another pod's pass and Finish: %+v; want still sending", got)
	}
	c.set(t0.Add(31 * time.Second))
	tick(t, b)
	got, _ := s.Get(as(2), m.ID)
	if got.State != Failed || !got.MayHaveDelivered || got.Detail != RecoveredDetail {
		t.Fatalf("after the lease expired: %+v; want failed, may have been delivered, %q", got, RecoveredDetail)
	}
	c.set(t0.Add(time.Hour))
	tick(t, b)
	wantSent(t, f, "after the recovery", nil...)
}

// TestOnlyTheClaimantRenewsAndAClaimNeedsALease: Renew by another instance
// is refused and leaves the lease where it was; the claimant's moves it. A
// claim with no instance, or a lease not after now, is refused.
func TestOnlyTheClaimantRenewsAndAClaimNeedsALease(t *testing.T) {
	s, _ := pgStore(t)
	sys := context.Background()
	m := mustEnqueue(t, s, 2, mirrorItem("x"))
	now := time.Now().Truncate(time.Microsecond)
	for _, bad := range []Claim{{By: "", Until: now.Add(time.Minute)}, {By: "pod-a", Until: now}} {
		if _, ok, err := s.Claim(sys, m.ID, now, bad); err == nil || ok {
			t.Fatalf("claim %+v = %v, %v; want refused", bad, ok, err)
		}
	}
	until := now.Add(30 * time.Second)
	if _, ok, err := s.Claim(sys, m.ID, now, Claim{By: "pod-a", Until: until}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := s.Renew(sys, m.ID, "pod-b", now.Add(time.Hour)); err != nil || ok {
		t.Fatalf("pod-b renewing pod-a's claim = %v, %v; want refused", ok, err)
	}
	if got, _ := s.Get(as(2), m.ID); !got.LeaseUntil.Equal(until) {
		t.Fatalf("another instance's Renew moved the lease to %v (was %v)", got.LeaseUntil, until)
	}
	if ok, err := s.Renew(sys, m.ID, "pod-a", now.Add(time.Hour)); err != nil || !ok {
		t.Fatalf("CONTROL: the claimant's Renew = %v, %v", ok, err)
	}
	if got, _ := s.Get(as(2), m.ID); !got.LeaseUntil.Equal(now.Add(time.Hour)) {
		t.Fatalf("the claimant's Renew left the lease at %v", got.LeaseUntil)
	}
}
