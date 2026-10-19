package sendqueue

import (
	"context"
	"errors"
	"testing"
	"time"
)

// failRow claims message id and finishes it failed (mayHave: it may have been
// delivered), as the dispatcher would.
func failRow(t *testing.T, s *PGStore, id int64, mayHave bool) {
	t.Helper()
	if _, ok, err := s.Claim(context.Background(), id, time.Now(), claimFor("retry-test", time.Now())); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := s.Finish(context.Background(), id, "retry-test", Outcome{MayHaveDelivered: mayHave, Detail: "Not sent: synthetic"}, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// TestRetryQueuesAFailedMessageAgainOnce: a failed message is retried as a NEW
// queued row — same target and body, the RETRYING sign-in's time, a new id —
// and the failed row stays failed, naming the new one. A second Retry of it is
// refused (no second copy), as is a Retry of a message that did not fail, and
// another owner's Retry is not found and changes nothing.
func TestRetryQueuesAFailedMessageAgainOnce(t *testing.T) {
	s, _ := pgStore(t)
	old := mustEnqueue(t, s, 2, Item{Target: win, Body: "try me again", StepUpAt: time.Now().Add(-time.Hour)})
	failRow(t, s, old.ID, false)

	// Another owner: not found, nothing queued, the row not marked.
	if _, err := s.Retry(as(3), old.ID, time.Now(), 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("user 3's Retry of user 2's message: %v; want not found", err)
	}
	if got, _ := s.Get(as(2), old.ID); got.RetriedAs != 0 {
		t.Fatalf("user 3's Retry marked the row retried as %d", got.RetriedAs)
	}

	at := time.Now().Add(-time.Minute).Truncate(time.Second)
	n, err := s.Retry(as(2), old.ID, at, 1792000009999)
	if err != nil {
		t.Fatal(err)
	}
	if n.ID == old.ID || n.State != Queued || n.Body != "try me again" || n.Target != win || n.Owner != 2 ||
		!n.StepUpAt.Equal(at) || n.AfterSince != 1792000009999 || n.RouteID != "" {
		t.Fatalf("the retry row: %+v; want a new queued row of user 2's, same text and window, signed in at %v", n, at)
	}
	got, err := s.Get(as(2), old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != Failed || got.RetriedAs != n.ID {
		t.Fatalf("the failed row after its Retry: %s retried as %d; want failed, retried as %d", got.State, got.RetriedAs, n.ID)
	}
	if _, err := s.Retry(as(2), old.ID, time.Now(), 0); !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("a second Retry: %v; want ErrNotRetryable", err)
	}
	// The queued retry row itself is not retryable (it did not fail).
	if _, err := s.Retry(as(2), n.ID, time.Now(), 0); !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("a Retry of a queued message: %v; want ErrNotRetryable", err)
	}
	items, err := s.List(as(2), win)
	if err != nil {
		t.Fatal(err)
	}
	if b := bodies(items); len(b) != 2 || b[0] != "queued:try me again" || b[1] != "failed:try me again" {
		t.Fatalf("user 2's list: %v; want the retry queued and the failed row", b)
	}
	// A Retry needs the retrying sign-in's time.
	other := mustEnqueue(t, s, 2, Item{Target: win, Body: "no time", StepUpAt: time.Now()})
	failRow(t, s, other.ID, true)
	if _, err := s.Retry(as(2), other.ID, time.Time{}, 0); err == nil {
		t.Fatal("a Retry with no sign-in time was queued")
	}
	if got, _ := s.Get(as(2), other.ID); got.RetriedAs != 0 {
		t.Fatalf("a refused Retry marked the row retried as %d", got.RetriedAs)
	}
}

// TestARoutedMessageIsNotRetried: a ROUTED row that failed is refused by Retry
// (ErrRoutedNotRetryable) — it is not requeued as the person's own, with no
// route and no session binding, which would bypass the route's gate — and the
// refusal queues nothing and leaves the row unretried. A failed route goes
// back to the chief instead. The control is the same kind of row with no
// route, which is retried.
func TestARoutedMessageIsNotRetried(t *testing.T) {
	s, _ := pgStore(t)
	routed := mustEnqueue(t, s, 2, Item{Target: win, Body: "routed answer", StepUpAt: time.Now(), RouteID: "rt_0123456789abcdef01234567"})
	failRow(t, s, routed.ID, false)
	if _, err := s.Retry(as(2), routed.ID, time.Now(), 0); !errors.Is(err, ErrRoutedNotRetryable) {
		t.Fatalf("Retry of a failed routed row: %v; want ErrRoutedNotRetryable", err)
	}
	if got, _ := s.Get(as(2), routed.ID); got.State != Failed || got.RetriedAs != 0 {
		t.Fatalf("the routed row after a refused Retry: %s retried as %d", got.State, got.RetriedAs)
	}
	items, err := s.List(as(2), win)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.State.Open() {
			t.Fatalf("a refused Retry of a routed row queued %+v", it)
		}
	}
	// CONTROL: the person's own failed row is retried.
	own := mustEnqueue(t, s, 2, Item{Target: win, Body: "my own", StepUpAt: time.Now()})
	failRow(t, s, own.ID, false)
	if _, err := s.Retry(as(2), own.ID, time.Now(), 0); err != nil {
		t.Fatalf("CONTROL: Retry of the person's own failed row: %v", err)
	}
}
