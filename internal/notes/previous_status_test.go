package notes

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestSetStatusReportsTheStatusItReplaced: under WithPreviousStatus, a write
// that lands reports the status the row had immediately before it (the version
// its own UPDATE replaced) — including a re-save of the same status — and a
// write that moved nothing (an agent on a complete task, an OnlyFrom that no
// longer holds, an unknown id) reports none.
func TestSetStatusReportsTheStatusItReplaced(t *testing.T) {
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
	n, err := store.Create(ctx, Note{Body: "previous-status fixture"})
	if err != nil {
		t.Fatal(err)
	}
	write := func(c context.Context, id int64, to string) (string, bool, error) {
		wctx, prev := WithPreviousStatus(c)
		_, err := store.SetStatus(wctx, id, to)
		p, ok := prev()
		return p, ok, err
	}
	for _, step := range []struct{ to, want string }{
		{StatusReadyForReview, StatusOpen},
		{StatusComplete, StatusReadyForReview},
		{StatusComplete, StatusComplete}, // a re-save reports what it replaced: itself
		{StatusOpen, StatusComplete},
	} {
		p, ok, err := write(ctx, n.ID, step.to)
		if err != nil || !ok || p != step.want {
			t.Fatalf("write %s: previous (%q, %v, %v); want (%q, true, nil)", step.to, p, ok, err, step.want)
		}
	}
	if _, err := store.SetStatus(ctx, n.ID, StatusComplete); err != nil {
		t.Fatal(err)
	}
	for label, c := range map[string]context.Context{
		"an agent on a complete task":    ForAgent(ctx),
		"OnlyFrom a status it is not in": OnlyFrom(ctx, StatusInProgress),
	} {
		if p, ok, err := write(c, n.ID, StatusOpen); err == nil || ok {
			t.Errorf("%s: previous (%q, %v), err %v; want none reported and an error", label, p, ok, err)
		}
	}
	if p, ok, err := write(ctx, -1, StatusOpen); err == nil || ok {
		t.Errorf("an unknown id: previous (%q, %v), err %v; want none reported and an error", p, ok, err)
	}
}
