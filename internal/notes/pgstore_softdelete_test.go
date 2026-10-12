package notes

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// Postgres-backed coverage for task soft delete.
//
// These are the tests that make the in-memory fakeNotes honest. The API-level
// regressions in internal/api assert "dismissing a task leaves its comment thread
// intact" against a fake; that assertion is only worth something if the fake
// models the REAL schema, whose note_comments FK is ON DELETE CASCADE.
// TestPGStoreDeleteCascadesComments below measures that cascade against live
// Postgres, so the fake's Delete-also-drops-comments modelling is pinned to
// reality rather than to what the fake's author believed.
//
// PG-gated like the rest of this package: skipped unless
// MUSTER_TEST_DATABASE_URL points at a disposable database.

// softDeleteTestStore opens the gated pool, migrates, and returns the store.
func softDeleteTestStore(t *testing.T) (*PGStore, *pgxpool.Pool, context.Context) {
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
	return NewPG(pool), pool, ctx
}

// rawCommentCount counts note_comments rows for a note WITHOUT going through the
// store, so it can see a dismissed task's surviving thread — and, on the hard
// delete, watch the thread genuinely disappear.
func rawCommentCount(t *testing.T, pool *pgxpool.Pool, ctx context.Context, noteID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM note_comments WHERE note_id=$1`, noteID).Scan(&n); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	return n
}

// TestPGStoreDeleteCascadesComments measures the destructive behaviour soft delete
// exists because of: a HARD Delete takes the whole comment thread with it, via the
// note_comments ON DELETE CASCADE FK.
//
// 🔴 This is not a test of the fix — it is the CONTROL that makes the fix's tests
// meaningful. fakeNotes.Delete drops f.comments[id] because of this cascade; if
// the FK were ever changed to RESTRICT or SET NULL, this test would go red and
// tell us the fake (and therefore every thread-survival assertion resting on it)
// had drifted from the schema.
func TestPGStoreDeleteCascadesComments(t *testing.T) {
	store, pool, ctx := softDeleteTestStore(t)

	n, err := store.Create(ctx, Note{Directory: "/work/orbit", Body: "hard delete control"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.AddComment(ctx, Comment{NoteID: n.ID, Author: "agent-x", Body: "the investigation"}); err != nil {
		t.Fatalf("add comment: %v", err)
	}
	if got := rawCommentCount(t, pool, ctx, n.ID); got != 1 {
		t.Fatalf("positive control: want 1 comment stored before the delete, got %d", got)
	}

	if err := store.Delete(ctx, n.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := rawCommentCount(t, pool, ctx, n.ID); got != 0 {
		t.Fatalf("the note_comments FK is expected to CASCADE on a hard delete — %d comment row(s) survived. "+
			"If the schema changed deliberately, fakeNotes.Delete must be updated to match.", got)
	}
}

// TestPGStoreSoftDeleteKeepsThreadAndHidesTask is the store-level statement of the
// fix: SoftDelete makes a task invisible to every read path while its comment rows
// stay in the table, and Restore brings back the task WITH its thread — asserted by
// comment CONTENT, not by a row count.
func TestPGStoreSoftDeleteKeepsThreadAndHidesTask(t *testing.T) {
	store, pool, ctx := softDeleteTestStore(t)

	n, err := store.Create(ctx, Note{Directory: "/work/orbit", Body: "soft delete subject", Tags: []string{"softdel-probe"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer store.Delete(ctx, n.ID) //nolint:errcheck // best-effort cleanup of the fixture
	for _, body := range []string{"found the leak", "fix is a defer"} {
		if _, err := store.AddComment(ctx, Comment{NoteID: n.ID, Author: "agent-x", Body: body}); err != nil {
			t.Fatalf("add comment %q: %v", body, err)
		}
	}

	inList := func() bool {
		list, err := store.List(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, x := range list {
			if x.ID == n.ID {
				return true
			}
		}
		return false
	}
	inSummaries := func() bool {
		list, err := store.ListSummaries(ctx)
		if err != nil {
			t.Fatalf("list summaries: %v", err)
		}
		for _, x := range list {
			if x.ID == n.ID {
				return true
			}
		}
		return false
	}
	inVocabulary := func() bool {
		v, err := store.TagVocabulary(ctx)
		if err != nil {
			t.Fatalf("tag vocabulary: %v", err)
		}
		for _, tc := range v {
			if tc.Tag == "softdel-probe" {
				return true
			}
		}
		return false
	}
	byTag := func() bool {
		list, err := store.ListByTags(ctx, []string{"softdel-probe"})
		if err != nil {
			t.Fatalf("list by tags: %v", err)
		}
		return len(list) > 0
	}

	// Positive controls — every "absent after dismiss" assertion below is worthless
	// unless the surface can be shown to contain the task while it is live.
	if !inList() || !inSummaries() || !inVocabulary() || !byTag() {
		t.Fatalf("positive control failed: live task missing from list=%v summaries=%v vocab=%v byTag=%v",
			inList(), inSummaries(), inVocabulary(), byTag())
	}

	if err := store.SoftDelete(ctx, n.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	if _, err := store.Get(ctx, n.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("Get on a dismissed task = %v, want pgx.ErrNoRows", err)
	}
	if inList() {
		t.Errorf("a dismissed task must be absent from List")
	}
	if inSummaries() {
		t.Errorf("a dismissed task must be absent from ListSummaries")
	}
	if inVocabulary() {
		t.Errorf("a dismissed task must be absent from TagVocabulary")
	}
	if byTag() {
		t.Errorf("a dismissed task must be absent from ListByTags")
	}

	// 🔴 The whole point: the thread is still there.
	if got := rawCommentCount(t, pool, ctx, n.ID); got != 2 {
		t.Fatalf("dismissing a task destroyed its comment thread: %d comment row(s) left, want 2", got)
	}
	// deleted_at is actually stamped (not merely "reads answer ErrNoRows for some
	// other reason") — the column is the mechanism, so read it directly.
	var deletedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT deleted_at FROM notes WHERE id=$1`, n.ID).Scan(&deletedAt); err != nil {
		t.Fatalf("read deleted_at: %v", err)
	}
	if deletedAt == nil {
		t.Fatalf("SoftDelete did not stamp notes.deleted_at")
	}

	restored, err := store.Restore(ctx, n.ID)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.Body != "soft delete subject" {
		t.Errorf("restored body = %q, want the original", restored.Body)
	}
	if len(restored.Comments) != 2 {
		t.Fatalf("restored task carries %d comments, want 2 (an empty shell is the failure this asserts against)", len(restored.Comments))
	}
	if restored.Comments[0].Body != "found the leak" || restored.Comments[1].Body != "fix is a defer" {
		t.Errorf("restored thread content = %q, %q — want the original bodies in order",
			restored.Comments[0].Body, restored.Comments[1].Body)
	}
	if !inList() {
		t.Errorf("a restored task must be back in List")
	}
}

// TestPGStoreSoftDeleteIsInvisibleToWrites asserts a dismissed task is unreachable
// by every MUTATOR too. Reads-only filtering would leave SetStatus/UpdateNote/
// AddTags landing on the dismissed row and then reporting ErrNoRows from the
// follow-up read — a write that succeeded while claiming to fail.
func TestPGStoreSoftDeleteIsInvisibleToWrites(t *testing.T) {
	store, pool, ctx := softDeleteTestStore(t)

	n, err := store.Create(ctx, Note{Directory: "/work/orbit", Body: "write guard subject", Tags: []string{"keep"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer store.Delete(ctx, n.ID) //nolint:errcheck // best-effort cleanup of the fixture
	if err := store.SoftDelete(ctx, n.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	body := "resurrected"
	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"SetStatus", func() error { _, err := store.SetStatus(ctx, n.ID, StatusComplete); return err }},
		{"UpdateNote", func() error { _, err := store.UpdateNote(ctx, n.ID, NoteUpdate{Body: &body}); return err }},
		{"AddTags", func() error { _, err := store.AddTags(ctx, n.ID, []string{"snuck-in"}); return err }},
		{"RemoveTags", func() error { _, err := store.RemoveTags(ctx, n.ID, []string{"keep"}); return err }},
		{"AddComment", func() error {
			_, err := store.AddComment(ctx, Comment{NoteID: n.ID, Author: "agent-x", Body: "into the void"})
			return err
		}},
		{"SoftDelete(again)", func() error { return store.SoftDelete(ctx, n.ID) }},
	} {
		if err := tc.run(); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("%s on a dismissed task = %v, want pgx.ErrNoRows", tc.name, err)
		}
	}

	// And none of the refused writes actually landed on the row.
	var status, storedBody string
	var tags []string
	if err := pool.QueryRow(ctx, `SELECT status, body, tags FROM notes WHERE id=$1`, n.ID).
		Scan(&status, &storedBody, &tags); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if status != StatusOpen {
		t.Errorf("a refused SetStatus still wrote: status = %q, want %q", status, StatusOpen)
	}
	if storedBody != "write guard subject" {
		t.Errorf("a refused UpdateNote still wrote: body = %q", storedBody)
	}
	if len(tags) != 1 || tags[0] != "keep" {
		t.Errorf("a refused tag edit still wrote: tags = %v, want [keep]", tags)
	}
	if got := rawCommentCount(t, pool, ctx, n.ID); got != 0 {
		t.Errorf("a refused AddComment still inserted %d comment row(s) into an invisible thread", got)
	}

	// A repeat SoftDelete must not re-stamp deleted_at either: the timestamp has to
	// record the FIRST dismissal for a timeline to be reconstructable.
	var first *time.Time
	if err := pool.QueryRow(ctx, `SELECT deleted_at FROM notes WHERE id=$1`, n.ID).Scan(&first); err != nil {
		t.Fatalf("read deleted_at: %v", err)
	}
	if first == nil {
		t.Fatalf("deleted_at unexpectedly NULL")
	}
	_ = store.SoftDelete(ctx, n.ID)
	var second *time.Time
	if err := pool.QueryRow(ctx, `SELECT deleted_at FROM notes WHERE id=$1`, n.ID).Scan(&second); err != nil {
		t.Fatalf("re-read deleted_at: %v", err)
	}
	if second == nil || !second.Equal(*first) {
		t.Errorf("a repeat SoftDelete re-stamped deleted_at: %v → %v", first, second)
	}
}

// TestPGStoreSoftDeleteAndRestoreUnknownID pins the error contract on ids the
// store cannot act on, so a caller can never read "nothing to do" as success.
func TestPGStoreSoftDeleteAndRestoreUnknownID(t *testing.T) {
	store, _, ctx := softDeleteTestStore(t)

	const missing = int64(-424242) // identity column never produces a negative id
	if err := store.SoftDelete(ctx, missing); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("SoftDelete of an unknown id = %v, want pgx.ErrNoRows", err)
	}
	if _, err := store.Restore(ctx, missing); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("Restore of an unknown id = %v, want pgx.ErrNoRows", err)
	}

	n, err := store.Create(ctx, Note{Directory: "/work/orbit", Body: "live task"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer store.Delete(ctx, n.ID) //nolint:errcheck // best-effort cleanup of the fixture
	if _, err := store.Restore(ctx, n.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("Restore of a LIVE task = %v, want pgx.ErrNoRows (a silent no-op would read as a successful undelete)", err)
	}
}
