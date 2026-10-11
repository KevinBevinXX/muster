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

// TestPGStoreStatusAndComments verifies that a task's status defaults to open,
// round-trips through SetStatus, and that comments persist and read back in
// order via AddComment/ListComments (and surface on Get/List).
//
// It needs a real Postgres (the package's Store is Postgres-backed). It is
// skipped unless MUSTER_TEST_DATABASE_URL points at a disposable database
// (the e2e suite spins one up; locally `go test ./...` stays green by skipping).
func TestPGStoreStatusAndComments(t *testing.T) {
	dsn := dbtest.DSN(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

	n, err := store.Create(ctx, Note{Directory: "/work/orbit", Body: "phase 3 task"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if n.Status != StatusOpen {
		t.Fatalf("new note status = %q, want %q", n.Status, StatusOpen)
	}

	updated, err := store.SetStatus(ctx, n.ID, StatusReadyForReview)
	if err != nil {
		t.Fatalf("set status: %v", err)
	}
	if updated.Status != StatusReadyForReview {
		t.Fatalf("after SetStatus, status = %q, want %q", updated.Status, StatusReadyForReview)
	}

	if _, err := store.AddComment(ctx, Comment{NoteID: n.ID, Author: "agent-x", Body: "first"}); err != nil {
		t.Fatalf("add comment 1: %v", err)
	}
	if _, err := store.AddComment(ctx, Comment{NoteID: n.ID, Author: "user", Body: "second"}); err != nil {
		t.Fatalf("add comment 2: %v", err)
	}

	got, err := store.Get(ctx, n.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Comments) != 2 {
		t.Fatalf("got %d comments, want 2", len(got.Comments))
	}
	if got.Comments[0].Body != "first" || got.Comments[1].Body != "second" {
		t.Fatalf("comments out of order: %q, %q", got.Comments[0].Body, got.Comments[1].Body)
	}
	if got.Comments[0].Author != "agent-x" {
		t.Fatalf("comment author = %q, want agent-x", got.Comments[0].Author)
	}

	// Cleanup keeps the shared test DB tidy across runs.
	if err := store.Delete(ctx, n.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

// TestPGStoreDispatchConfigRoundTrips verifies the dispatch-config
// columns (model / repo / repo_branch) round-trip through Create→Get→List.
// PG-gated like the tests above.
func TestPGStoreDispatchConfigRoundTrips(t *testing.T) {
	dsn := dbtest.DSN(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

	n, err := store.Create(ctx, Note{
		Directory:  "/work/orbit",
		Body:       "dispatch config task",
		Model:      "openrouter/anthropic/claude-sonnet-4.6",
		Repo:       "acme/orbit",
		RepoBranch: "trunk",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer store.Delete(ctx, n.ID)

	assertConfig := func(label string, got Note) {
		if got.Model != "openrouter/anthropic/claude-sonnet-4.6" {
			t.Fatalf("%s: model = %q", label, got.Model)
		}
		if got.Repo != "acme/orbit" || got.RepoBranch != "trunk" {
			t.Fatalf("%s: repo/branch = %q/%q", label, got.Repo, got.RepoBranch)
		}
	}
	// The Create RETURNING must round-trip the config…
	assertConfig("create", n)

	got, err := store.Get(ctx, n.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	assertConfig("get", got)

	// …and every list scan (List + ListSummaries) too.
	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var fromList *Note
	for i := range list {
		if list[i].ID == n.ID {
			fromList = &list[i]
			break
		}
	}
	if fromList == nil {
		t.Fatalf("created note %d missing from List", n.ID)
	}
	assertConfig("list", *fromList)

	sums, err := store.ListSummaries(ctx)
	if err != nil {
		t.Fatalf("list summaries: %v", err)
	}
	var fromSums *Note
	for i := range sums {
		if sums[i].ID == n.ID {
			fromSums = &sums[i]
			break
		}
	}
	if fromSums == nil {
		t.Fatalf("created note %d missing from ListSummaries", n.ID)
	}
	assertConfig("summaries", *fromSums)

	// A config-less task defaults to empty config.
	bare, err := store.Create(ctx, Note{Directory: "/work/bare", Body: "no config"})
	if err != nil {
		t.Fatalf("create bare: %v", err)
	}
	defer store.Delete(ctx, bare.ID)
	if bare.Model != "" || bare.Repo != "" || bare.RepoBranch != "" {
		t.Fatalf("config-less task should have empty config, got %+v", bare)
	}
}

// TestPGStoreSourceProvenanceRoundTrips verifies the nullable
// source_type / source_session_id columns round-trip through Create→Get→List, and
// that a task created WITHOUT provenance reads back NULL (nil pointers) — the
// pre-0017-compatible default. PG-gated like the tests above.
func TestPGStoreSourceProvenanceRoundTrips(t *testing.T) {
	dsn := dbtest.DSN(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

	src, sess := "claude-code", "abc12345-def6-7890"
	n, err := store.Create(ctx, Note{
		Directory:       "/work/orbit",
		Body:            "provenance task",
		SourceType:      &src,
		SourceSessionID: &sess,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer store.Delete(ctx, n.ID)

	assertProv := func(label string, got Note) {
		if got.SourceType == nil || *got.SourceType != src {
			t.Fatalf("%s: source_type = %v, want %q", label, got.SourceType, src)
		}
		if got.SourceSessionID == nil || *got.SourceSessionID != sess {
			t.Fatalf("%s: source_session_id = %v, want %q", label, got.SourceSessionID, sess)
		}
	}
	assertProv("create", n) // RETURNING round-trips it

	got, err := store.Get(ctx, n.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	assertProv("get", got)

	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var fromList *Note
	for i := range list {
		if list[i].ID == n.ID {
			fromList = &list[i]
			break
		}
	}
	if fromList == nil {
		t.Fatalf("created note %d missing from List", n.ID)
	}
	assertProv("list", *fromList)

	// A provenance-less task reads back NULL (nil pointers) on every path.
	bare, err := store.Create(ctx, Note{Directory: "/work/bare", Body: "no provenance"})
	if err != nil {
		t.Fatalf("create bare: %v", err)
	}
	defer store.Delete(ctx, bare.ID)
	if bare.SourceType != nil || bare.SourceSessionID != nil {
		t.Fatalf("provenance-less create should be NULL, got type=%v session=%v", bare.SourceType, bare.SourceSessionID)
	}
	bareGot, err := store.Get(ctx, bare.ID)
	if err != nil {
		t.Fatalf("get bare: %v", err)
	}
	if bareGot.SourceType != nil || bareGot.SourceSessionID != nil {
		t.Fatalf("provenance-less Get should be NULL, got type=%v session=%v", bareGot.SourceType, bareGot.SourceSessionID)
	}
}

// TestPGStoreUpdateNote verifies the partial-edit path (migration-less: all
// columns already exist): a body-only patch changes ONLY the body and leaves the
// title/model/repo/branch intact; a full patch updates every
// editable field; updated_at bumps; and — critically — status,
// source_type/source_session_id, and created_at are NEVER changed by an edit. An
// unknown id → pgx.ErrNoRows. PG-gated like the tests above.
func TestPGStoreUpdateNote(t *testing.T) {
	dsn := dbtest.DSN(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

	src, sess := "claude-code", "sess-edit-1"
	n, err := store.Create(ctx, Note{
		Directory:       "/work/orbit",
		Body:            "original body",
		Model:           "openrouter/anthropic/claude-sonnet-4.6",
		Repo:            "acme/orbit",
		RepoBranch:      "trunk",
		SourceType:      &src,
		SourceSessionID: &sess,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer store.Delete(ctx, n.ID)

	// Give it a non-open status so the "edit never changes status" assertion is
	// non-vacuous (an edit must leave ready_for_review untouched).
	if _, err := store.SetStatus(ctx, n.ID, StatusReadyForReview); err != nil {
		t.Fatalf("set status: %v", err)
	}
	before, err := store.Get(ctx, n.ID)
	if err != nil {
		t.Fatalf("get before: %v", err)
	}
	// Ensure a measurable clock tick so the updated_at bump is observable even at
	// microsecond resolution.
	time.Sleep(5 * time.Millisecond)

	// ---- body-only patch: only the body changes; everything else is intact ----
	newBody := "edited body"
	got, err := store.UpdateNote(ctx, n.ID, NoteUpdate{Body: &newBody})
	if err != nil {
		t.Fatalf("update body: %v", err)
	}
	if got.Body != "edited body" {
		t.Errorf("body = %q, want %q", got.Body, "edited body")
	}
	if got.Directory != "/work/orbit" || got.Model != "openrouter/anthropic/claude-sonnet-4.6" ||
		got.Repo != "acme/orbit" || got.RepoBranch != "trunk" {
		t.Errorf("body-only patch must leave dir/model/repo/branch intact, got %+v", got)
	}
	// The load-bearing immutables:
	if got.Status != StatusReadyForReview {
		t.Errorf("edit must NOT change status, got %q want %q", got.Status, StatusReadyForReview)
	}
	if got.SourceType == nil || *got.SourceType != src {
		t.Errorf("edit must NOT change source_type, got %v", got.SourceType)
	}
	if got.SourceSessionID == nil || *got.SourceSessionID != sess {
		t.Errorf("edit must NOT change source_session_id, got %v", got.SourceSessionID)
	}
	if !got.CreatedAt.Equal(before.CreatedAt) {
		t.Errorf("edit must NOT change created_at, got %v want %v", got.CreatedAt, before.CreatedAt)
	}
	if !got.UpdatedAt.After(before.UpdatedAt) {
		t.Errorf("edit must bump updated_at, got %v not after %v", got.UpdatedAt, before.UpdatedAt)
	}

	// ---- full patch: every editable field updates ----
	dir2, body2, model2, repo2, branch2 := "/work/other", "full edit", "openrouter/x/y", "zacxdev/other", "feature"
	full, err := store.UpdateNote(ctx, n.ID, NoteUpdate{
		Directory: &dir2, Body: &body2, Model: &model2, Repo: &repo2, RepoBranch: &branch2,
	})
	if err != nil {
		t.Fatalf("full update: %v", err)
	}
	if full.Directory != dir2 || full.Body != body2 || full.Model != model2 ||
		full.Repo != repo2 || full.RepoBranch != branch2 {
		t.Errorf("full patch must update every editable field, got %+v", full)
	}
	if full.Status != StatusReadyForReview {
		t.Errorf("full patch must still not change status, got %q", full.Status)
	}

	// ---- unknown id → pgx.ErrNoRows ----
	if _, err := store.UpdateNote(ctx, 999999, NoteUpdate{Body: &newBody}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("unknown id should return pgx.ErrNoRows, got %v", err)
	}
}

// TestPGStoreListBatched verifies the batched List path attaches the correct
// attachments + comments to each note (and isolates them per note), exercising
// the constant-query rewrite. PG-gated like the test above.
func TestPGStoreListBatched(t *testing.T) {
	dsn := dbtest.DSN(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

	n1, err := store.Create(ctx, Note{Directory: "/work/a", Body: "note one"})
	if err != nil {
		t.Fatalf("create n1: %v", err)
	}
	n2, err := store.Create(ctx, Note{Directory: "/work/b", Body: "note two"})
	if err != nil {
		t.Fatalf("create n2: %v", err)
	}
	defer store.Delete(ctx, n1.ID)
	defer store.Delete(ctx, n2.ID)

	// n1: two attachments + two comments; n2: nothing.
	if _, err := store.AddAttachment(ctx, n1.ID, Attachment{Filename: "x.txt", ContentType: "text/plain", SizeBytes: 1, Data: []byte("a")}); err != nil {
		t.Fatalf("attach 1: %v", err)
	}
	if _, err := store.AddAttachment(ctx, n1.ID, Attachment{Filename: "y.txt", ContentType: "text/plain", SizeBytes: 1, Data: []byte("b")}); err != nil {
		t.Fatalf("attach 2: %v", err)
	}
	if _, err := store.AddComment(ctx, Comment{NoteID: n1.ID, Author: "user", Body: "c1"}); err != nil {
		t.Fatalf("comment 1: %v", err)
	}
	if _, err := store.AddComment(ctx, Comment{NoteID: n1.ID, Author: "agent", Body: "c2"}); err != nil {
		t.Fatalf("comment 2: %v", err)
	}

	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byID := map[int64]Note{}
	for _, n := range list {
		byID[n.ID] = n
	}

	got1 := byID[n1.ID]
	if len(got1.Attachments) != 2 {
		t.Fatalf("n1 attachments = %d, want 2", len(got1.Attachments))
	}
	if got1.Attachments[0].Filename != "x.txt" || got1.Attachments[1].Filename != "y.txt" {
		t.Fatalf("n1 attachments out of order: %+v", got1.Attachments)
	}
	if len(got1.Comments) != 2 || got1.Comments[0].Body != "c1" || got1.Comments[1].Body != "c2" {
		t.Fatalf("n1 comments wrong: %+v", got1.Comments)
	}

	got2 := byID[n2.ID]
	if len(got2.Attachments) != 0 || len(got2.Comments) != 0 {
		t.Fatalf("n2 should have no children, got %d att / %d comments", len(got2.Attachments), len(got2.Comments))
	}

	// The batched List must match the per-note Get for the same note.
	getN1, err := store.Get(ctx, n1.ID)
	if err != nil {
		t.Fatalf("get n1: %v", err)
	}
	if len(getN1.Attachments) != len(got1.Attachments) || len(getN1.Comments) != len(got1.Comments) {
		t.Fatalf("List vs Get mismatch: List %d/%d, Get %d/%d",
			len(got1.Attachments), len(got1.Comments), len(getN1.Attachments), len(getN1.Comments))
	}
}

// TestPGStoreListSummaries verifies the lightweight picker query: it returns
// id + directory + body for each note and never hydrates attachments/comments
// (the dispatch modal only renders an id + label). PG-gated like the tests above.
func TestPGStoreListSummaries(t *testing.T) {
	dsn := dbtest.DSN(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

	n, err := store.Create(ctx, Note{Directory: "/work/summary", Body: "pick me"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer store.Delete(ctx, n.ID)

	// Add a child to prove ListSummaries does NOT hydrate it.
	if _, err := store.AddComment(ctx, Comment{NoteID: n.ID, Author: "user", Body: "noise"}); err != nil {
		t.Fatalf("add comment: %v", err)
	}

	sums, err := store.ListSummaries(ctx)
	if err != nil {
		t.Fatalf("list summaries: %v", err)
	}
	var got *Note
	for i := range sums {
		if sums[i].ID == n.ID {
			got = &sums[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("created note %d missing from ListSummaries", n.ID)
	}
	if got.Directory != "/work/summary" || got.Body != "pick me" {
		t.Fatalf("summary fields wrong: dir=%q body=%q", got.Directory, got.Body)
	}
	if len(got.Attachments) != 0 || len(got.Comments) != 0 {
		t.Fatalf("ListSummaries hydrated children (%d att / %d comments); it must not",
			len(got.Attachments), len(got.Comments))
	}
}
