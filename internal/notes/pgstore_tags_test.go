package notes

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// tagTestStore spins up the Postgres-backed store for the tag tests. Like the
// other store tests it is SKIPPED unless MUSTER_TEST_DATABASE_URL points at a
// disposable database, so `go test ./...` stays green without Docker.
func tagTestStore(t *testing.T) (context.Context, *pgxpool.Pool, *PGStore) {
	t.Helper()
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 60*time.Second)
	t.Cleanup(cancel)

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Each test starts from a clean notes table so the vocabulary counts and the
	// filter assertions are deterministic regardless of test order.
	if _, err := pool.Exec(ctx, `DELETE FROM notes`); err != nil {
		t.Fatalf("truncate notes: %v", err)
	}
	return ctx, pool, NewPG(pool)
}

func joined(tags []string) string { return strings.Join(tags, ",") }

// TestPGStoreTagsRoundTrip asserts tags survive Create and are carried by EVERY
// read path (Get / List / ListSummaries) — a read path that silently drops tags
// would make routing tags invisible to the UI and to producers.
func TestPGStoreTagsRoundTrip(t *testing.T) {
	ctx, _, store := tagTestStore(t)

	// Un-normalized input on the way in: Create normalizes (lowercase, dedupe, sort).
	n, err := store.Create(ctx, Note{
		Directory: "/work/orbit",
		Title:     "Ship task tags",
		Body:      "spec §10",
		Tags:      []string{"Frontend", "bug", "frontend", "runbook:deploy"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	want := "bug,frontend,runbook:deploy"
	if joined(n.Tags) != want {
		t.Fatalf("Create tags = %q, want %q (normalized + sorted)", n.Tags, want)
	}
	if n.Title != "Ship task tags" {
		t.Fatalf("Create title = %q, want the stored title", n.Title)
	}

	got, err := store.Get(ctx, n.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if joined(got.Tags) != want {
		t.Fatalf("Get tags = %q, want %q", got.Tags, want)
	}
	if got.Title != "Ship task tags" {
		t.Fatalf("Get title = %q, want the stored title", got.Title)
	}

	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || joined(list[0].Tags) != want || list[0].Title != "Ship task tags" {
		t.Fatalf("List must carry tags + title; got %+v", list)
	}

	sums, err := store.ListSummaries(ctx)
	if err != nil {
		t.Fatalf("list summaries: %v", err)
	}
	if len(sums) != 1 || joined(sums[0].Tags) != want || sums[0].Title != "Ship task tags" {
		t.Fatalf("ListSummaries must carry tags + title; got %+v", sums)
	}
}

// TestPGStorePreMigrationRowReadsEmpty inserts a row the way a PRE-0018 producer
// would (no tags/title column mentioned) and asserts it reads back as an EMPTY,
// non-nil tag set and an empty title — no nil-vs-empty confusion, no backfill
// needed, no producer breakage.
func TestPGStorePreMigrationRowReadsEmpty(t *testing.T) {
	ctx, pool, store := tagTestStore(t)

	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO notes (directory, body) VALUES ($1,$2) RETURNING id`,
		"/legacy", "a task from before tags existed").Scan(&id); err != nil {
		t.Fatalf("legacy insert: %v", err)
	}

	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Tags == nil {
		t.Fatal("a pre-migration row must read back as an EMPTY slice, not nil")
	}
	if len(got.Tags) != 0 {
		t.Fatalf("pre-migration tags = %q, want empty", got.Tags)
	}
	if got.Title != "" {
		t.Fatalf("pre-migration title = %q, want empty", got.Title)
	}
}

// TestPGStoreUpdateNoteTagReplace covers the REPLACE semantics of UpdateNote:
// a set replaces, an explicit empty slice CLEARS, and nil leaves the set alone.
func TestPGStoreUpdateNoteTagReplace(t *testing.T) {
	ctx, _, store := tagTestStore(t)

	n, err := store.Create(ctx, Note{Body: "t", Tags: []string{"a", "b"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	repl := []string{"c"}
	got, err := store.UpdateNote(ctx, n.ID, NoteUpdate{Tags: &repl})
	if err != nil {
		t.Fatalf("update replace: %v", err)
	}
	if joined(got.Tags) != "c" {
		t.Fatalf("after replace, tags = %q, want [c]", got.Tags)
	}

	// nil pointer → unchanged (and the title patch does not disturb tags).
	title := "renamed"
	got, err = store.UpdateNote(ctx, n.ID, NoteUpdate{Title: &title})
	if err != nil {
		t.Fatalf("update title: %v", err)
	}
	if joined(got.Tags) != "c" {
		t.Fatalf("a nil Tags patch must leave tags unchanged; got %q", got.Tags)
	}
	if got.Title != "renamed" {
		t.Fatalf("title = %q, want renamed", got.Title)
	}

	empty := []string{}
	got, err = store.UpdateNote(ctx, n.ID, NoteUpdate{Tags: &empty})
	if err != nil {
		t.Fatalf("update clear: %v", err)
	}
	if len(got.Tags) != 0 {
		t.Fatalf("&[]string{} must CLEAR the tag set; got %q", got.Tags)
	}
	if got.Tags == nil {
		t.Fatal("a cleared set must still read back non-nil (the column is NOT NULL)")
	}
}

// TestPGStoreAddRemoveTags covers set-union / set-difference, idempotence, and
// order-independence.
func TestPGStoreAddRemoveTags(t *testing.T) {
	ctx, _, store := tagTestStore(t)

	n, err := store.Create(ctx, Note{Body: "t", Tags: []string{"b"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := store.AddTags(ctx, n.ID, []string{"c", "A"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if joined(got.Tags) != "a,b,c" {
		t.Fatalf("AddTags = %q, want [a b c] (union, normalized, sorted)", got.Tags)
	}

	// Idempotent: re-adding an existing tag changes nothing.
	got, err = store.AddTags(ctx, n.ID, []string{"a", "b"})
	if err != nil {
		t.Fatalf("add again: %v", err)
	}
	if joined(got.Tags) != "a,b,c" {
		t.Fatalf("AddTags must be idempotent; got %q", got.Tags)
	}

	// Order-independence: adding in the reverse order yields the same set.
	other, err := store.Create(ctx, Note{Body: "t2"})
	if err != nil {
		t.Fatalf("create 2: %v", err)
	}
	if _, err := store.AddTags(ctx, other.ID, []string{"c"}); err != nil {
		t.Fatalf("add other c: %v", err)
	}
	if _, err := store.AddTags(ctx, other.ID, []string{"b"}); err != nil {
		t.Fatalf("add other b: %v", err)
	}
	o2, err := store.AddTags(ctx, other.ID, []string{"a"})
	if err != nil {
		t.Fatalf("add other a: %v", err)
	}
	if joined(o2.Tags) != "a,b,c" {
		t.Fatalf("order-independence: got %q, want [a b c]", o2.Tags)
	}

	got, err = store.RemoveTags(ctx, n.ID, []string{"B"})
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if joined(got.Tags) != "a,c" {
		t.Fatalf("RemoveTags = %q, want [a c]", got.Tags)
	}
	// Idempotent: removing an absent tag is a no-op.
	got, err = store.RemoveTags(ctx, n.ID, []string{"zzz"})
	if err != nil {
		t.Fatalf("remove absent: %v", err)
	}
	if joined(got.Tags) != "a,c" {
		t.Fatalf("removing an absent tag must be a no-op; got %q", got.Tags)
	}
	// Removing everything leaves an empty, non-nil set.
	got, err = store.RemoveTags(ctx, n.ID, []string{"a", "c"})
	if err != nil {
		t.Fatalf("remove all: %v", err)
	}
	if got.Tags == nil || len(got.Tags) != 0 {
		t.Fatalf("removing all tags must yield an empty non-nil set; got %#v", got.Tags)
	}

	// Unknown id must be a clear ErrNoRows on both merge paths.
	if _, err := store.AddTags(ctx, 999999, []string{"x"}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("AddTags on unknown id = %v, want pgx.ErrNoRows", err)
	}
	if _, err := store.RemoveTags(ctx, 999999, []string{"x"}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("RemoveTags on unknown id = %v, want pgx.ErrNoRows", err)
	}
}

// TestPGStoreAddTagsTwoWritersNoLostUpdate is the reason AddTags exists. Two
// concurrent writers each add a DIFFERENT tag; both must survive. A
// read-modify-write (or UpdateNote's replace semantics) would let the later write
// clobber the earlier one — exactly the lost-update class that two author classes
// (machine producers + the human) create.
func TestPGStoreAddTagsTwoWritersNoLostUpdate(t *testing.T) {
	ctx, _, store := tagTestStore(t)

	n, err := store.Create(ctx, Note{Body: "contended"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	start := make(chan struct{})
	for _, tag := range []string{"writer-a", "writer-b"} {
		wg.Add(1)
		go func(tag string) {
			defer wg.Done()
			<-start // maximize overlap
			if _, err := store.AddTags(ctx, n.ID, []string{tag}); err != nil {
				errs <- err
			}
		}(tag)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent AddTags: %v", err)
	}

	got, err := store.Get(ctx, n.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if joined(got.Tags) != "writer-a,writer-b" {
		t.Fatalf("LOST UPDATE: tags = %q, want both writer-a and writer-b", got.Tags)
	}
}

// TestPGStoreListByTags covers the AND filter: single tag, multi-tag, a
// partially-matching multi-tag filter, and an unknown tag (empty, NOT an error).
func TestPGStoreListByTags(t *testing.T) {
	ctx, _, store := tagTestStore(t)

	if _, err := store.Create(ctx, Note{Body: "one", Tags: []string{"api", "bug"}}); err != nil {
		t.Fatalf("create 1: %v", err)
	}
	if _, err := store.Create(ctx, Note{Body: "two", Tags: []string{"api"}}); err != nil {
		t.Fatalf("create 2: %v", err)
	}
	if _, err := store.Create(ctx, Note{Body: "three"}); err != nil {
		t.Fatalf("create 3: %v", err)
	}

	cases := []struct {
		name string
		tags []string
		want int
	}{
		{"nil filter is List", nil, 3},
		{"single tag", []string{"api"}, 2},
		{"multi tag AND", []string{"api", "bug"}, 1},
		{"multi tag AND with a non-match", []string{"api", "nope"}, 0},
		{"unknown tag is empty, not an error", []string{"never-used"}, 0},
		{"un-normalized filter still matches", []string{"API"}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.ListByTags(ctx, tc.tags)
			if err != nil {
				t.Fatalf("ListByTags(%q): %v", tc.tags, err)
			}
			if got == nil {
				t.Fatal("ListByTags must never return nil")
			}
			if len(got) != tc.want {
				t.Fatalf("ListByTags(%q) returned %d notes, want %d", tc.tags, len(got), tc.want)
			}
		})
	}
}

// TestPGStoreTagVocabulary asserts the vocabulary counts tag usage and orders by
// count DESC (ties by tag ASC, so the chip row is stable).
func TestPGStoreTagVocabulary(t *testing.T) {
	ctx, _, store := tagTestStore(t)

	for _, tags := range [][]string{
		{"api", "bug"},
		{"api", "zeta"},
		{"api"},
		{"bug"},
	} {
		if _, err := store.Create(ctx, Note{Body: "t", Tags: tags}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	vocab, err := store.TagVocabulary(ctx)
	if err != nil {
		t.Fatalf("vocabulary: %v", err)
	}
	want := []TagCount{{"api", 3}, {"bug", 2}, {"zeta", 1}}
	if len(vocab) != len(want) {
		t.Fatalf("vocabulary = %+v, want %+v", vocab, want)
	}
	for i := range want {
		if vocab[i] != want[i] {
			t.Fatalf("vocabulary[%d] = %+v, want %+v (count DESC, tag ASC)", i, vocab[i], want[i])
		}
	}
}
