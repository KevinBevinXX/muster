package sendqueue

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
)

// ageRow moves message id's created_at to at (Postgres's own column, as a row
// queued then would have it).
func ageRow(t *testing.T, pool *pgxpool.Pool, id int64, at time.Time) {
	t.Helper()
	if _, err := pool.Exec(authz.System(context.Background()), `UPDATE send_queue SET created_at = $2 WHERE id = $1`, id, at); err != nil {
		t.Fatal(err)
	}
}

// TestExpireFailsAPersonsMessageQueuedMoreThanSevenDaysAgo: Expire fails, as
// expired and never delivered, a person's message still QUEUED that was queued
// 7 days and a minute before now — so it offers Retry, and its slot frees —
// and leaves alone one queued 6 days 23 hours ago, a ROUTED one queued as long
// ago (its route gates it), and one being sent (its claimant finishes it).
// Each row is on its own window, so no other row's state decides its fate.
func TestExpireFailsAPersonsMessageQueuedMoreThanSevenDaysAgo(t *testing.T) {
	s, pool := pgStore(t)
	now := time.Now()
	on := func(w string) Target { return Target{Host: "laptop", Session: "scratch2", Window: w} }
	// Literal durations, never derived from AuthorizedFor: a bound moved in
	// the code must move a case here.
	old := 7*24*time.Hour + time.Minute
	expired := mustEnqueue(t, s, 2, Item{Target: on("@1"), Body: "too old", StepUpAt: now})
	young := mustEnqueue(t, s, 2, Item{Target: on("@2"), Body: "not yet", StepUpAt: now})
	routed := mustEnqueue(t, s, 2, Item{Target: on("@3"), Body: "routed", StepUpAt: now, RouteID: "rt_0123456789abcdef01234567"})
	sending := mustEnqueue(t, s, 2, Item{Target: on("@4"), Body: "being sent", StepUpAt: now})
	ageRow(t, pool, expired.ID, now.Add(-old))
	ageRow(t, pool, young.ID, now.Add(-(6*24+23)*time.Hour))
	ageRow(t, pool, routed.ID, now.Add(-old))
	ageRow(t, pool, sending.ID, now.Add(-old))
	if _, ok, err := s.Claim(context.Background(), sending.ID, now, claimFor("expire-test", now)); err != nil || !ok {
		t.Fatal(ok, err)
	}

	n, err := s.Expire(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("Expire failed %d rows; want exactly the one person's row queued 7 days and a minute ago", n)
	}
	got, err := s.Get(as(2), expired.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != Failed || got.Detail != ExpiredDetail || got.MayHaveDelivered || got.FinishedAt.Sub(now).Abs() > time.Millisecond {
		t.Fatalf("the expired row: %s %q (may have: %v, finished %v); want failed as expired at %v, never delivered",
			got.State, got.Detail, got.MayHaveDelivered, got.FinishedAt, now)
	}
	for _, c := range []struct {
		it   Item
		want State
	}{{young, Queued}, {routed, Queued}, {sending, Sending}} {
		if got, err := s.Get(as(2), c.it.ID); err != nil || got.State != c.want {
			t.Fatalf("%q after Expire: %s %q, %v; want %s", c.it.Body, got.State, got.Detail, err, c.want)
		}
	}
	// It is Retry-able: a Retry queues it again.
	if _, err := s.Retry(as(2), expired.ID, now, 0); err != nil {
		t.Fatalf("Retry of the expired row: %v", err)
	}
}
