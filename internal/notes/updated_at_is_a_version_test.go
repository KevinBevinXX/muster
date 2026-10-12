package notes

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestUpdatedAtIsAMonotonicCardVersion pins the property the UI's stale-card
// guard RESTS ON, in the one place that can actually make the claim: Postgres.
//
// 🔴 WHY IT MATTERS OUTSIDE THIS PACKAGE. internal/ui renders every task card
// with data-card-rev = notes.updated_at in nanoseconds, and resyncScript refuses
// a morph:outerHTML into #task-{id} whose rev is STRICTLY OLDER than the one on
// screen. That is what stops the SSE-triggered re-fetch of a dispatch — issued
// before the dispatch's own status write commits — from landing on top of the
// authoritative answer and putting `open` back on a task the store has as
// `in_progress`.
//
// The guard is only sound if updated_at moves FORWARD with the task's own data,
// and only the store can be asked. Both directions are checked:
//
//   - SetStatus and AddComment must ADVANCE it (they write `updated_at=now()` in
//     the same statement as the change), or a genuinely newer card would tie with
//     the one on screen and the ordering would be back to last-arrival-wins;
//   - and it must never go BACKWARDS, or the guard would drop the fresh card.
//
// PG-gated exactly like the rest of this file's tests: without
// MUSTER_TEST_DATABASE_URL there is no store to ask, and a fake would only
// restate what the fake was written to do.
func TestUpdatedAtIsAMonotonicCardVersion(t *testing.T) {
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

	n, err := store.Create(ctx, Note{Directory: "/work/orbit", Body: "a task whose card carries a version"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer store.Delete(ctx, n.ID)

	if n.UpdatedAt.IsZero() {
		t.Fatalf("a freshly created task has a ZERO updated_at, so its card's rev would be 0 and every " +
			"later card would tie with it — the guard would order nothing")
	}

	// Each step: a measurable clock tick, then the write, then BOTH the write's
	// own return value and an independent Get — because the card is rendered from
	// whichever of the two the route happened to have, and a bump visible in only
	// one of them orders only half the writers.
	steps := []struct {
		what string
		do   func() (Note, error)
	}{
		{"SetStatus open→in_progress (the dispatch advance itself)", func() (Note, error) {
			return store.SetStatus(ctx, n.ID, StatusInProgress)
		}},
		{"AddComment", func() (Note, error) {
			if _, err := store.AddComment(ctx, Comment{NoteID: n.ID, Author: "zach", Body: "a comment"}); err != nil {
				return Note{}, err
			}
			return store.Get(ctx, n.ID)
		}},
		{"SetStatus in_progress→complete", func() (Note, error) {
			return store.SetStatus(ctx, n.ID, StatusComplete)
		}},
	}

	prev := n.UpdatedAt
	for _, s := range steps {
		time.Sleep(5 * time.Millisecond) // a measurable tick at microsecond resolution
		got, err := s.do()
		if err != nil {
			t.Fatalf("%s: %v", s.what, err)
		}
		if !got.UpdatedAt.After(prev) {
			t.Fatalf("%s left updated_at at %v (was %v) — it did not advance.\n"+
				"🔴 The task card's data-card-rev IS this value: a change that does not move it renders "+
				"a card that TIES with the one already on screen, and a tie is applied, so the ordering "+
				"between the SSE re-fetch and the mutation response collapses back to last-arrival-wins "+
				"— the race behind the flaky dispatch e2e spec.", s.what, got.UpdatedAt, prev)
		}
		reread, err := store.Get(ctx, n.ID)
		if err != nil {
			t.Fatalf("%s: re-read: %v", s.what, err)
		}
		if !reread.UpdatedAt.Equal(got.UpdatedAt) {
			t.Errorf("%s: the write returned updated_at %v but a Get reads %v. The card is rendered from "+
				"EITHER — the mutation routes use the write's return value, the SSE re-fetch uses Get — "+
				"so the two must agree or the same task version gets two different revs.",
				s.what, got.UpdatedAt, reread.UpdatedAt)
		}
		if reread.UpdatedAt.Before(prev) {
			t.Errorf("%s: updated_at went BACKWARDS (%v < %v). The guard drops a card whose rev is older "+
				"than the one on screen, so a backwards step would drop the FRESH card and leave the "+
				"stale one up.", s.what, reread.UpdatedAt, prev)
		}
		prev = reread.UpdatedAt
	}
}
