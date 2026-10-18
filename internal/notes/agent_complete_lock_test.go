package notes

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestAnAgentsStatusWriteNeverMovesACompletedRow: under ForAgent, SetStatus on
// a complete task answers ErrCompleteLocked and leaves the row complete — the
// condition is in the UPDATE, so it holds whatever the caller read before. An
// agent still moves a task that is not complete, the operator (no ForAgent)
// still reopens a complete one, and an unknown id is still ErrNoRows.
func TestAnAgentsStatusWriteNeverMovesACompletedRow(t *testing.T) {
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
	agent := ForAgent(ctx)

	n, err := store.Create(ctx, Note{Body: "complete-lock fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.SetStatus(agent, n.ID, StatusInProgress); err != nil || got.Status != StatusInProgress {
		t.Fatalf("an agent moving an open task: %v %v", got.Status, err)
	}
	if _, err := store.SetStatus(ctx, n.ID, StatusComplete); err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{StatusInProgress, StatusOpen, StatusReadyForReview} {
		if _, err := store.SetStatus(agent, n.ID, to); !errors.Is(err, ErrCompleteLocked) {
			t.Fatalf("an agent moving a complete task to %s: %v", to, err)
		}
		if got, _ := store.Get(ctx, n.ID); got.Status != StatusComplete {
			t.Fatalf("the row moved to %s", got.Status)
		}
	}
	if got, err := store.SetStatus(ctx, n.ID, StatusOpen); err != nil || got.Status != StatusOpen {
		t.Fatalf("the operator reopening: %v %v", got.Status, err)
	}
	if _, err := store.SetStatus(agent, -1, StatusOpen); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("an unknown id: %v", err)
	}
}
