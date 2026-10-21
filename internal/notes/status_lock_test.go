package notes

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// SetStatus's conditions (an agent may not move a complete task; OnlyFrom's
// required status; not deleted) sit in a CTE that selects the row
// FOR NO KEY UPDATE. The lock is what applies those conditions to the NEWEST
// row version when another transaction is writing the same row: SetStatus
// waits for it, then re-checks. Without the lock, the CTE judges the version
// from before the other write, and the outer UPDATE (whose only condition is
// the id) writes over the newer one.
//
// Each test holds an uncommitted status write in transaction A on one
// connection, starts SetStatus on another, checks that the call is BLOCKED (so
// the two writes really overlap), commits A, and then asserts the outcome.
// Timeouts make a regression fail instead of hanging.

func lockFixture(t *testing.T) (context.Context, *pgxpool.Pool, *PGStore) {
	t.Helper()
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return ctx, pool, NewPG(pool)
}

// racedWrite holds `UPDATE notes SET status = held` uncommitted in its own
// transaction, runs write concurrently, asserts write is blocked by it, commits,
// and returns write's error.
func racedWrite(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64, held string, write func() error) error {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `UPDATE notes SET status = $2 WHERE id = $1`, id, held); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- write() }()
	select {
	case err := <-done:
		t.Fatalf("the concurrent write returned (%v) while A still held the row: the two writes did not overlap", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("the concurrent write never returned after A committed")
		return nil
	}
}

// TestAnAgentCannotReopenATaskCompletedConcurrently: A completes the task and
// holds the write; an agent's SetStatus(open) runs against it. After A commits,
// the agent's write answers ErrCompleteLocked and the task stays complete.
func TestAnAgentCannotReopenATaskCompletedConcurrently(t *testing.T) {
	ctx, pool, store := lockFixture(t)
	n, err := store.Create(ctx, Note{Body: "concurrent complete-lock fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetStatus(ctx, n.ID, StatusInProgress); err != nil {
		t.Fatal(err)
	}
	err = racedWrite(t, ctx, pool, n.ID, StatusComplete, func() error {
		_, err := store.SetStatus(ForAgent(ctx), n.ID, StatusOpen)
		return err
	})
	if !errors.Is(err, ErrCompleteLocked) {
		t.Errorf("the agent's write against a concurrent complete: %v; want ErrCompleteLocked", err)
	}
	if got, _ := store.Get(ctx, n.ID); got.Status != StatusComplete {
		t.Fatalf("the task reads %q after the agent's write; want complete (an agent reopened a completed task)", got.Status)
	}
}

// TestAnOnlyFromWriteLosesToAConcurrentMove: A moves the task away from
// in_progress and holds the write; an OnlyFrom(in_progress) SetStatus(open) runs
// against it. After A commits, it answers ErrStatusMoved and the task keeps A's
// status.
func TestAnOnlyFromWriteLosesToAConcurrentMove(t *testing.T) {
	ctx, pool, store := lockFixture(t)
	n, err := store.Create(ctx, Note{Body: "concurrent only-from fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetStatus(ctx, n.ID, StatusInProgress); err != nil {
		t.Fatal(err)
	}
	err = racedWrite(t, ctx, pool, n.ID, StatusReadyForReview, func() error {
		_, err := store.SetStatus(OnlyFrom(ctx, StatusInProgress), n.ID, StatusOpen)
		return err
	})
	if !errors.Is(err, ErrStatusMoved) {
		t.Errorf("the OnlyFrom write against a concurrent move: %v; want ErrStatusMoved", err)
	}
	if got, _ := store.Get(ctx, n.ID); got.Status != StatusReadyForReview {
		t.Fatalf("the task reads %q; want ready_for_review (A's write was overwritten)", got.Status)
	}
}
