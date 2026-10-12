package notes

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestCountByStatusCountsLiveTasksInThatStatus pins the app badge's count
// against a real database: only the asked-for status, and never a dismissed
// task. The fixture adds 3 / 1 / 0 live tasks across three statuses plus 2
// dismissed ones in the counted status — pairwise-distinct deltas, so a query
// that ignored the status or the soft-delete predicate cannot land on the right
// number.
//
// ⚠ DELTAS, NOT ABSOLUTES: dbtest gives each PACKAGE one database, which other
// tests here (and earlier runs) have already written to, so the counts are
// measured before and after.
func TestCountByStatusCountsLiveTasksInThatStatus(t *testing.T) {
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := NewPG(pool)

	statuses := []string{StatusReadyForReview, StatusInProgress, StatusComplete, StatusOpen}
	before := map[string]int{}
	for _, st := range statuses {
		n, err := store.CountByStatus(ctx, st)
		if err != nil {
			t.Fatalf("CountByStatus(%s): %v", st, err)
		}
		before[st] = n
	}
	mk := func(status string) int64 {
		t.Helper()
		n, err := store.Create(ctx, Note{Body: "count fixture"})
		if err != nil {
			t.Fatal(err)
		}
		if status != StatusOpen {
			if _, err := store.SetStatus(ctx, n.ID, status); err != nil {
				t.Fatal(err)
			}
		}
		return n.ID
	}
	for i := 0; i < 3; i++ {
		mk(StatusReadyForReview)
	}
	mk(StatusInProgress)
	// Two more in review, then dismissed: they must not count.
	for i := 0; i < 2; i++ {
		if err := store.SoftDelete(ctx, mk(StatusReadyForReview)); err != nil {
			t.Fatal(err)
		}
	}

	for status, delta := range map[string]int{StatusReadyForReview: 3, StatusInProgress: 1, StatusComplete: 0, StatusOpen: 0} {
		got, err := store.CountByStatus(ctx, status)
		if err != nil || got-before[status] != delta {
			t.Errorf("CountByStatus(%s) moved by %d (%d → %d, err %v); want +%d",
				status, got-before[status], before[status], got, err, delta)
		}
	}
}
