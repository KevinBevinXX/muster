package notes

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// These are the AUTHORITATIVE tests for FlagIdle. The api-tier tests run against
// fakeNotes, and a fake can only restate what it was written to do — the two
// properties that matter here (that `updated_at` is not written, and that the
// at-most-once predicate is a real SQL predicate) are claims about Postgres, so
// only Postgres can be asked.
//
// PG-gated like the rest of this package:
//
//	MUSTER_TEST_DATABASE_URL='postgres://postgres:x@localhost:55432/postgres?sslmode=disable' \
//	  go test ./internal/notes -run FlagIdle
func flagIdleStore(t *testing.T) (*PGStore, context.Context) {
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
	return NewPG(pool), ctx
}

// TestFlagIdleDoesNotMoveUpdatedAt is the load-bearing one. The idle-task reaper
// SELECTS on `updated_at < now - TTL`, so a reaper that writes updated_at is
// writing to the field it measures: it resets the idle clock on everything it
// touches (self-perpetuating re-reaps) and, because the sweep iterates
// `updated_at DESC` and bumps in that order, it sorts the MOST idle task HIGHEST.
//
// The assertion is EXACT EQUALITY of the before/after timestamp, deliberately.
// The defect's signature is `updated_at = now()`, and a tolerance-based check
// ("still older than the cutoff", "within a second") would wave it through — the
// test would be green while the board's ordering stayed inverted.
//
// It also pins that the write ACTUALLY HAPPENED, because "the timestamp did not
// move" is trivially true of a no-op, and a test that cannot tell those apart
// would pass against FlagIdle deleted to `return false, nil`.
func TestFlagIdleDoesNotMoveUpdatedAt(t *testing.T) {
	store, ctx := flagIdleStore(t)

	n, err := store.Create(ctx, Note{Directory: "/work/orbit", Body: "an abandoned task"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer store.Delete(ctx, n.ID)

	before, err := store.Get(ctx, n.ID)
	if err != nil {
		t.Fatalf("get before: %v", err)
	}
	if before.UpdatedAt.IsZero() {
		t.Fatalf("fixture has a ZERO updated_at, so the comparison below would be vacuous")
	}

	// Sleep past the timestamp's resolution so a bump would be VISIBLE. Without
	// this, a same-microsecond bump could tie and the test would pass for the
	// wrong reason.
	time.Sleep(10 * time.Millisecond)

	ok, err := store.FlagIdle(ctx, n.ID, "stale", "Task idle — tagged **stale** by the retention sweep.")
	if err != nil {
		t.Fatalf("FlagIdle: %v", err)
	}
	if !ok {
		t.Fatalf("FlagIdle reported no write on a never-reaped task; the assertions below " +
			"would then be true of a no-op")
	}

	after, err := store.Get(ctx, n.ID)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	// The write landed...
	if !HasAllTags(after.Tags, []string{"stale"}) {
		t.Errorf("FlagIdle did not add the tag: tags %v", after.Tags)
	}
	cs, err := store.ListComments(ctx, n.ID)
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if len(cs) != 1 {
		t.Fatalf("FlagIdle wrote %d comments, want 1", len(cs))
	}
	if cs[0].Author != ReapCommentAuthor {
		t.Errorf("reap comment author = %q, want %q (FlagIdle's already-reaped predicate keys "+
			"on this exact value; a different author makes the reap invisible to itself and "+
			"the sweep starts re-commenting every pass)", cs[0].Author, ReapCommentAuthor)
	}
	// ...and the clock did NOT move.
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("FlagIdle MOVED updated_at: %s → %s.\n"+
			"That field is the reaper's own selection predicate AND the card revision behind "+
			"data-card-rev. Writing it here is the defect this method exists to remove — check "+
			"that `updated_at` has not been re-added to the UPDATE's SET list, and that no "+
			"BEFORE UPDATE trigger was added to `notes`.",
			before.UpdatedAt.Format(time.RFC3339Nano), after.UpdatedAt.Format(time.RFC3339Nano))
	}
}

// TestFlagIdleWritesAtMostOncePerIdlePeriod is the regression test for the design
// trap: the updated_at bump used to double as the "already reaped" marker, so
// removing it without a replacement predicate would make the sweep append a
// duplicate system comment on EVERY pass, forever — 48 a day per task at the new
// 30-minute cadence.
//
// Ten passes, not two: a predicate that is merely off-by-one would still pass a
// two-pass check.
func TestFlagIdleWritesAtMostOncePerIdlePeriod(t *testing.T) {
	store, ctx := flagIdleStore(t)

	n, err := store.Create(ctx, Note{Directory: "/work/orbit", Body: "an abandoned task"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer store.Delete(ctx, n.ID)

	wrote := 0
	for i := 0; i < 10; i++ {
		ok, err := store.FlagIdle(ctx, n.ID, "stale", "Task idle — tagged **stale** by the retention sweep.")
		if err != nil {
			t.Fatalf("FlagIdle pass %d: %v", i, err)
		}
		if ok {
			wrote++
		}
	}
	if wrote != 1 {
		t.Errorf("FlagIdle reported a write on %d of 10 passes, want exactly 1", wrote)
	}

	cs, err := store.ListComments(ctx, n.ID)
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if len(cs) != 1 {
		t.Errorf("ten sweeps of one idle task left %d comments, want exactly 1 — the reaper is "+
			"spamming the thread. The anti-spam guard is the `NOT EXISTS (a %q comment newer "+
			"than notes.updated_at)` clause in FlagIdle's UPDATE.", len(cs), ReapCommentAuthor)
	}
	after, err := store.Get(ctx, n.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got := 0
	for _, tag := range after.Tags {
		if tag == "stale" {
			got++
		}
	}
	if got != 1 {
		t.Errorf("tag 'stale' present %d times after ten sweeps, want 1 (tags %v)", got, after.Tags)
	}
}

// TestFlagIdleReFlagsAfterGenuineActivity pins the property that the OBVIOUS
// cheaper predicate — "skip tasks already carrying the `stale` tag" — would have
// silently destroyed.
//
// Nothing removes that tag AUTOMATICALLY — only a deliberate operator action
// does (the machine PATCH's removeTags, DELETE /tasks/{id}/tags/{tag}, or the
// Edit modal's whole-set replace). So under a tag predicate, a task flagged once
// could never be flagged again however long it later sat idle, unless a human
// cleared the chip by hand, and the failure would be invisible (a task that is
// never flagged looks exactly like a task that is not idle). Keying on the reap COMMENT
// instead makes re-flagging fall out for free: any real write bumps updated_at
// past the old comment's created_at, which re-arms the predicate.
//
// AddComment is the "genuine activity" here because it is a real, unrelated
// writer that bumps updated_at in the same statement as its own change.
func TestFlagIdleReFlagsAfterGenuineActivity(t *testing.T) {
	store, ctx := flagIdleStore(t)

	n, err := store.Create(ctx, Note{Directory: "/work/orbit", Body: "an abandoned task"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer store.Delete(ctx, n.ID)

	if ok, err := store.FlagIdle(ctx, n.ID, "stale", "reap 1"); err != nil || !ok {
		t.Fatalf("first FlagIdle: ok=%v err=%v, want true/nil", ok, err)
	}
	// Still suppressed while nothing has happened.
	if ok, err := store.FlagIdle(ctx, n.ID, "stale", "reap 1 dup"); err != nil || ok {
		t.Fatalf("second FlagIdle without activity: ok=%v err=%v, want false/nil", ok, err)
	}

	time.Sleep(10 * time.Millisecond)
	if _, err := store.AddComment(ctx, Comment{NoteID: n.ID, Author: "operator", Body: "picking this back up"}); err != nil {
		t.Fatalf("AddComment (genuine activity): %v", err)
	}

	ok, err := store.FlagIdle(ctx, n.ID, "stale", "reap 2")
	if err != nil {
		t.Fatalf("third FlagIdle: %v", err)
	}
	if !ok {
		t.Fatalf("a task that was flagged, then genuinely worked on, was NOT re-flagged. " +
			"Re-flagging is broken — which is exactly what a tag-presence predicate causes, " +
			"since nothing removes the `stale` tag except a deliberate operator edit.")
	}
	cs, err := store.ListComments(ctx, n.ID)
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	// reap 1 + the operator comment + reap 2.
	if len(cs) != 3 {
		t.Errorf("thread has %d comments, want 3 (reap, activity, re-reap)", len(cs))
	}
}

// TestFlagIdleStaysSuppressedByARetractedReapComment pins the direction chosen for
// retraction, which is not obvious and is easy to "fix" the wrong way.
//
// A retracted comment keeps its row and its created_at; only the
// body is redacted. FlagIdle's predicate scans note_comments UNFILTERED, so a
// retracted reap comment still suppresses. Filtering retracted rows out would let
// an operator who retracts the notice make the task eligible again — and the next
// sweep, 30 minutes later, would post a fresh one. That is a retract/repost loop,
// so suppression is the safe direction.
func TestFlagIdleStaysSuppressedByARetractedReapComment(t *testing.T) {
	store, ctx := flagIdleStore(t)

	n, err := store.Create(ctx, Note{Directory: "/work/orbit", Body: "an abandoned task"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer store.Delete(ctx, n.ID)

	if ok, err := store.FlagIdle(ctx, n.ID, "stale", "reap 1"); err != nil || !ok {
		t.Fatalf("first FlagIdle: ok=%v err=%v", ok, err)
	}
	cs, err := store.ListComments(ctx, n.ID)
	if err != nil || len(cs) != 1 {
		t.Fatalf("ListComments: %v (%d comments)", err, len(cs))
	}
	if err := store.SoftDeleteComment(ctx, n.ID, cs[0].ID); err != nil {
		t.Fatalf("SoftDeleteComment: %v", err)
	}

	ok, err := store.FlagIdle(ctx, n.ID, "stale", "reap 2")
	if err != nil {
		t.Fatalf("FlagIdle after retraction: %v", err)
	}
	if ok {
		t.Errorf("retracting the reap comment re-armed the reaper, so the next sweep posts a " +
			"new one — a retract/repost loop. FlagIdle's NOT EXISTS must scan note_comments " +
			"UNFILTERED (no `deleted_at IS NULL`).")
	}
	after, _ := store.ListComments(ctx, n.ID)
	if len(after) != 1 {
		t.Errorf("thread has %d comments after retract + sweep, want 1 (the tombstone)", len(after))
	}
}

// TestFlagIdleRefusesDismissedAndUnknownTasks pins the `liveOnly` half: a
// soft-deleted task is INVISIBLE to every other Store write, and the reaper must
// not be the one exception that writes into a thread nothing can see. Neither case
// is an error — "nothing to do" is not a failure for a best-effort sweep that logs
// every error it gets.
func TestFlagIdleRefusesDismissedAndUnknownTasks(t *testing.T) {
	store, ctx := flagIdleStore(t)

	n, err := store.Create(ctx, Note{Directory: "/work/orbit", Body: "dismissed task"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer store.Delete(ctx, n.ID)
	if err := store.SoftDelete(ctx, n.ID); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}

	ok, err := store.FlagIdle(ctx, n.ID, "stale", "reap")
	if err != nil {
		t.Fatalf("FlagIdle on a dismissed task returned an error: %v (want false, nil)", err)
	}
	if ok {
		t.Error("FlagIdle wrote into a DISMISSED task's thread; liveOnly is missing from the UPDATE")
	}

	if ok, err := store.FlagIdle(ctx, 0x7FFFFFFF, "stale", "reap"); err != nil || ok {
		t.Errorf("FlagIdle on an unknown id = (%v, %v), want (false, nil)", ok, err)
	}
}
