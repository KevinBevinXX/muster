package notes

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

// CreateOnceByTag and AddCommentOnce are the two idempotent writes behind
// POST /plugin/v1/tasks. Both are claims about what Postgres does under
// concurrency, so they are PG-gated through flagIdleStore like FlagIdle's.

func onceKey(t *testing.T) string {
	t.Helper()
	return "tracker:t" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// TestCreateOnceByTagCreatesOneUnderConcurrency: N callers racing on one key file
// ONE task, every caller gets that task back, and exactly one reports created.
// Concurrency, not repetition: a lookup-then-insert without the advisory lock
// passes every sequential case and duplicates here.
func TestCreateOnceByTagCreatesOneUnderConcurrency(t *testing.T) {
	store, ctx := flagIdleStore(t)
	const (
		trials  = 10
		workers = 4 // MaxConns is 8; each worker holds one for its transaction.
	)
	dupTrials := 0
	for trial := 0; trial < trials; trial++ {
		key := onceKey(t) + "-" + strconv.Itoa(trial)
		start := make(chan struct{})
		var wg sync.WaitGroup
		got := make([]Note, workers)
		created := make([]bool, workers)
		errs := make([]error, workers)
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				<-start
				got[w], created[w], errs[w] = store.CreateOnceByTag(ctx, key, Note{Title: "from upstream", Body: "body " + strconv.Itoa(w)})
			}(w)
		}
		close(start)
		wg.Wait()
		creators := 0
		for w := 0; w < workers; w++ {
			if errs[w] != nil {
				t.Fatalf("trial %d worker %d: %v", trial, w, errs[w])
			}
			if created[w] {
				creators++
			}
			if got[w].ID != got[0].ID {
				dupTrials++
				break
			}
		}
		rows, err := store.ListByTags(ctx, []string{key})
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range rows {
			defer store.Delete(ctx, n.ID)
		}
		if creators != 1 || len(rows) != 1 {
			dupTrials++
			t.Logf("trial %d: %d creators, %d tasks carry %s", trial, creators, len(rows), key)
		}
	}
	if dupTrials > 0 {
		t.Fatalf("%d of %d trials with %d concurrent CreateOnceByTag calls on one key did not produce exactly one task", dupTrials, trials, workers)
	}
}

// TestCreateOnceByTagIsSequentiallyIdempotent: a second call returns the first
// task unchanged (created=false, its own body, not the new one), the key is on
// the task's tags beside the caller's, a dismissed task no longer counts, and a
// key that is not a normalized tag is refused before anything is written.
func TestCreateOnceByTagIsSequentiallyIdempotent(t *testing.T) {
	store, ctx := flagIdleStore(t)
	key := onceKey(t)
	first, created, err := store.CreateOnceByTag(ctx, key, Note{Title: "one", Body: "first body", Tags: []string{"inbox"}})
	if err != nil || !created {
		t.Fatalf("first call: created=%v err=%v; want a new task", created, err)
	}
	defer store.Delete(ctx, first.ID)
	if !HasAllTags(first.Tags, []string{key, "inbox"}) || len(first.Tags) != 2 {
		t.Fatalf("tags %v; want exactly [%s inbox]", first.Tags, key)
	}
	again, created, err := store.CreateOnceByTag(ctx, key, Note{Title: "two", Body: "second body"})
	if err != nil || created || again.ID != first.ID || again.Body != "first body" {
		t.Fatalf("second call: id %d (first %d) created=%v body=%q err=%v; want the first task back, unchanged",
			again.ID, first.ID, created, again.Body, err)
	}
	// CONTROL: a different key is a different task.
	other, created, err := store.CreateOnceByTag(ctx, key+"x", Note{Body: "other"})
	if err != nil || !created || other.ID == first.ID {
		t.Fatalf("a different key: id %d created=%v err=%v; want a second task", other.ID, created, err)
	}
	defer store.Delete(ctx, other.ID)
	// A dismissed task is not live, so the key files a new one.
	if err := store.SoftDelete(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	fresh, created, err := store.CreateOnceByTag(ctx, key, Note{Body: "after dismissal"})
	if err != nil || !created || fresh.ID == first.ID {
		t.Fatalf("after dismissal: id %d created=%v err=%v; want a new task", fresh.ID, created, err)
	}
	defer store.Delete(ctx, fresh.ID)
	for _, bad := range []string{"", "TRACKER:abc", " tracker:abc"} {
		if _, _, err := store.CreateOnceByTag(ctx, bad, Note{Body: "x"}); err == nil {
			t.Errorf("key %q was accepted; want a refusal", bad)
		}
	}
}

// TestAddCommentOnceWritesOnceAndRespectsRetraction: the same comment twice is
// one comment; four racing callers write it once; a different body or author is
// a new comment; a RETRACTED comment is not written again; a dismissed task is
// ErrNoRows.
func TestAddCommentOnceWritesOnceAndRespectsRetraction(t *testing.T) {
	store, ctx := flagIdleStore(t)
	n, err := store.Create(ctx, Note{Body: "a task"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Delete(ctx, n.ID)
	c := Comment{NoteID: n.ID, Author: "plugin:example-inbox", Body: "[status] in progress (upstream message 12)"}

	const workers = 4
	start := make(chan struct{})
	var wg sync.WaitGroup
	wrote := make([]bool, workers)
	errs := make([]error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			wrote[w], errs[w] = store.AddCommentOnce(ctx, c)
		}(w)
	}
	close(start)
	wg.Wait()
	writers := 0
	for w := 0; w < workers; w++ {
		if errs[w] != nil {
			t.Fatalf("worker %d: %v", w, errs[w])
		}
		if wrote[w] {
			writers++
		}
	}
	cs, err := store.ListComments(ctx, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if writers != 1 || len(cs) != 1 {
		t.Fatalf("%d concurrent AddCommentOnce calls: %d writers, %d comments; want 1 and 1", workers, writers, len(cs))
	}
	if again, err := store.AddCommentOnce(ctx, c); err != nil || again {
		t.Fatalf("a repeat: wrote=%v err=%v; want no write", again, err)
	}
	other := c
	other.Body += "!"
	if w, err := store.AddCommentOnce(ctx, other); err != nil || !w {
		t.Fatalf("a different body: wrote=%v err=%v; want a write", w, err)
	}
	byOther := c
	byOther.Author = "plugin:other"
	if w, err := store.AddCommentOnce(ctx, byOther); err != nil || !w {
		t.Fatalf("a different author: wrote=%v err=%v; want a write", w, err)
	}
	if err := store.SoftDeleteComment(ctx, n.ID, cs[0].ID); err != nil {
		t.Fatal(err)
	}
	if w, err := store.AddCommentOnce(ctx, c); err != nil || w {
		t.Fatalf("after the owner retracted it: wrote=%v err=%v; want no write", w, err)
	}
	if cs, _ := store.ListComments(ctx, n.ID); len(cs) != 3 {
		t.Fatalf("%d comments; want 3 (one retracted, two distinct)", len(cs))
	}
	if err := store.SoftDelete(ctx, n.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddCommentOnce(ctx, Comment{NoteID: n.ID, Author: "plugin:x", Body: "late"}); err == nil {
		t.Fatal("a comment on a dismissed task was accepted; want ErrNoRows")
	}
}
