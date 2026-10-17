package focus

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// FOCUS HISTORY (migration 0022) against a real Postgres: what is appended,
// what is not, what is pruned and cascaded, who can read it, and how it pages.

// titles is sub's whole history as user uid reads it, newest first.
func titles(t *testing.T, s *PGStore, uid int64, sub Subject) []string {
	t.Helper()
	var out []string
	cursor := ""
	for i := 0; i < 100; i++ {
		page, err := s.History(as(uid), sub, cursor, MaxHistoryPage)
		if err != nil {
			t.Fatalf("history: %v", err)
		}
		for _, e := range page.Entries {
			out = append(out, e.Title+"|"+e.Description)
		}
		if page.Next == "" {
			return out
		}
		cursor = page.Next
	}
	t.Fatal("history never ended")
	return nil
}

func write(t *testing.T, s *PGStore, uid int64, sub Subject, title, desc, fp string) {
	t.Helper()
	if _, err := s.Upsert(as(uid), sub, Write{Title: title, Description: desc, Source: SourceSummarizer, Fingerprint: fp}); err != nil {
		t.Fatalf("upsert %q: %v", title, err)
	}
}

func eq(a, b []string) bool { return fmt.Sprint(a) == fmt.Sprint(b) }

// TestHistoryAppendsOnlyOnChange: a row per CHANGE of title or description,
// newest first — and a refresh that summarizes to the same focus (a moved
// tail, so a new fingerprint) writes none.
func TestHistoryAppendsOnlyOnChange(t *testing.T) {
	s, pool := pgStore(t)
	sub := AgentSubject(mustAgent(t, pool, 2, "focus-hist-change").ID)
	write(t, s, 2, sub, "Porting the importer", "Step one.", Fingerprint("a"))
	write(t, s, 2, sub, "Porting the importer", "Step one.", Fingerprint("b")) // identical: nothing
	write(t, s, 2, sub, "Porting the importer", "Step two.", Fingerprint("c")) // description changed
	write(t, s, 2, sub, "Porting the importer", "Step two.", Fingerprint("d")) // identical: nothing
	write(t, s, 2, sub, "Reviewing the PR", "Step two.", Fingerprint("e"))     // title changed
	write(t, s, 2, sub, "Porting the importer", "Step one.", Fingerprint("f")) // back to the first: a change from the newest
	want := []string{"Porting the importer|Step one.", "Reviewing the PR|Step two.", "Porting the importer|Step two.", "Porting the importer|Step one."}
	if got := titles(t, s, 2, sub); !eq(got, want) {
		t.Fatalf("history %q, want %q", got, want)
	}
}

// TestAnIdenticalRefreshWritesNoHistory: the same focus written again — by the
// store, and by a whole Refresher pass whose tail moved but whose summary did
// not — leaves exactly one history row; a RecordError writes none.
func TestAnIdenticalRefreshWritesNoHistory(t *testing.T) {
	s, pool := pgStore(t)
	sub := AgentSubject(mustAgent(t, pool, 2, "focus-hist-same").ID)
	write(t, s, 2, sub, "Kernel upgrade", "Draining first.", "")
	write(t, s, 2, sub, "Kernel upgrade", "Draining first.", "")
	if err := s.RecordError(as(2), sub, ReasonRateLimited); err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{tails: map[Subject][]Entry{sub: {{Role: "user", Text: "a new message"}}}}
	summ := &fakeSummarizer{sum: Summary{Title: "Kernel upgrade", Description: "Draining first."}}
	r := &Refresher{Store: s, Sessions: src, Summarizer: summ, AsOwner: func(ctx context.Context, _ int64) context.Context { return ctx }}
	if out, err := r.Refresh(as(2), 2, sub); err != nil || out != OutcomeUpdated || summ.calls != 1 {
		t.Fatalf("refresh: %v %v (calls %d): the pass must reach the store for this to test anything", out, err, summ.calls)
	}
	if got := titles(t, s, 2, sub); len(got) != 1 {
		t.Fatalf("history %q, want the one row", got)
	}
	// Positive control: the same pass with a different summary appends.
	src.tails[sub] = []Entry{{Role: "user", Text: "and another"}}
	summ.sum = Summary{Title: "Kernel upgrade", Description: "Rebooting now."}
	if _, err := r.Refresh(as(2), 2, sub); err != nil {
		t.Fatal(err)
	}
	if got := titles(t, s, 2, sub); len(got) != 2 || got[0] != "Kernel upgrade|Rebooting now." {
		t.Fatalf("history %q, want the changed focus on top", got)
	}
}

// TestHistoryPruneKeepsThirtyDaysAndTheCurrentFocus: PruneHistory deletes
// every owner's rows older than the cut — except a subject's NEWEST row while
// the subject still has a focus row (it IS the current focus); once the focus
// row is gone, the newest row ages out too.
func TestHistoryPruneKeepsThirtyDaysAndTheCurrentFocus(t *testing.T) {
	s, pool := pgStore(t)
	mine := AgentSubject(mustAgent(t, pool, 2, "focus-hist-prune2").ID)
	lone := AgentSubject(mustAgent(t, pool, 3, "focus-hist-prune3").ID)
	pair := AgentSubject(mustAgent(t, pool, 3, "focus-hist-prune3b").ID)
	gone := MirrorSubject("0b2c3d4e-hist-prune-gone")
	write(t, s, 2, mine, "old 2", "", "")
	write(t, s, 2, mine, "new 2", "", "")
	write(t, s, 3, lone, "only focus", "", "")
	write(t, s, 3, pair, "pair older", "", "")
	write(t, s, 3, pair, "pair newer", "", "")
	write(t, s, 2, gone, "gone session", "", "")
	dsn := dbtest.DSN(t)
	dbtest.OwnerExec(t, dsn, `UPDATE agent_focus_history SET created_at = now() - interval '40 days' WHERE title <> 'new 2'`)
	dbtest.OwnerExec(t, dsn, `UPDATE agent_focus_history SET created_at = now() - interval '29 days' WHERE title = 'new 2'`)
	// The pair's two rows must stay ordered: the newer one a minute newer.
	dbtest.OwnerExec(t, dsn, `UPDATE agent_focus_history SET created_at = created_at + interval '1 minute' WHERE title = 'pair newer'`)
	// The mirrored session's focus row was pruned: its history has no current focus.
	dbtest.OwnerExec(t, dsn, `DELETE FROM agent_focus WHERE subject_key = $1`, gone.Key)

	n, err := s.PruneHistory(context.Background(), time.Now().Add(-DefaultHistoryRetention))
	if err != nil || n != 3 {
		t.Fatalf("pruned %d (%v), want 3: owner 2's 40-day row, the pair's older row, the pruned session's row", n, err)
	}
	for _, c := range []struct {
		uid  int64
		sub  Subject
		want []string
	}{
		{2, mine, []string{"new 2|"}},
		{3, lone, []string{"only focus|"}}, // 40 days old, and still the current focus
		{3, pair, []string{"pair newer|"}},
		{2, gone, nil},
	} {
		if got := titles(t, s, c.uid, c.sub); !eq(got, c.want) {
			t.Errorf("%s: history %q, want %q", c.sub, got, c.want)
		}
	}
}

// TestRetainerPrunesHistoryAfterThirtyDays: the hourly sweep asks for history
// older than 720h — fixed, not configurable (operator decision).
func TestRetainerPrunesHistoryAfterThirtyDays(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	m := newMem()
	r := &Retainer{Store: m, Sessions: &fakeSource{}, AsOwner: func(ctx context.Context, _ int64) context.Context { return ctx },
		Now: func() time.Time { return now }}
	if _, err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := now.Sub(m.historyPrunedBefore); got != 720*time.Hour {
		t.Errorf("pruned history before now-%s, want now-720h", got)
	}
}

// TestHistoryOutlivesItsFocusRowButNotItsAgent (operator decision: "keep 30
// days regardless"): a mirrored session whose focus row the week-unseen sweep
// deletes at day 8 keeps its history, readable, until day 30; deleting an
// agent deletes its history at once.
func TestHistoryOutlivesItsFocusRowButNotItsAgent(t *testing.T) {
	s, pool := pgStore(t)
	dsn := dbtest.DSN(t)
	m := MirrorSubject("0b2c3d4e-hist-outlives")
	write(t, s, 2, m, "one", "", "")
	write(t, s, 2, m, "two", "", "")
	dbtest.OwnerExec(t, dsn, `UPDATE agent_focus SET seen_at = now() - interval '8 days' WHERE subject_key = $1`, m.Key)
	dbtest.OwnerExec(t, dsn, `UPDATE agent_focus_history SET created_at = created_at - interval '8 days' WHERE subject_key = $1`, m.Key)
	if n, err := s.PruneAll(context.Background(), time.Now().Add(-DefaultRetention)); err != nil || n != 1 {
		t.Fatalf("day 8: pruned %d focus rows (%v), want the session's one", n, err)
	}
	if _, ok, _ := s.Get(as(2), m); ok {
		t.Fatal("day 8: the focus row survived the week-unseen prune: this test proves nothing")
	}
	if _, err := s.PruneHistory(context.Background(), time.Now().Add(-DefaultHistoryRetention)); err != nil {
		t.Fatal(err)
	}
	if got := titles(t, s, 2, m); !eq(got, []string{"two|", "one|"}) {
		t.Fatalf("day 8, focus row pruned: history %q, want both rows kept", got)
	}
	// Day 31: the history's own 30 days are up.
	dbtest.OwnerExec(t, dsn, `UPDATE agent_focus_history SET created_at = created_at - interval '23 days' WHERE subject_key = $1`, m.Key)
	if n, err := s.PruneHistory(context.Background(), time.Now().Add(-DefaultHistoryRetention)); err != nil || n != 2 {
		t.Fatalf("day 31: pruned %d history rows (%v), want both", n, err)
	}

	a := mustAgent(t, pool, 2, "focus-hist-agent-del")
	keep := mustAgent(t, pool, 2, "focus-hist-agent-keep")
	for _, ag := range []Subject{AgentSubject(a.ID), AgentSubject(keep.ID)} {
		write(t, s, 2, ag, "focus of "+ag.Key, "", "")
	}
	if _, err := pool.Exec(as(2), `DELETE FROM agents WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM agent_focus_history WHERE subject_kind = 'agent' AND subject_key = $1`,
		AgentSubject(a.ID).Key).Scan(&left); err != nil || left != 0 {
		t.Fatalf("the deleted agent's history: %d rows (%v), want 0", left, err)
	}
	if got := titles(t, s, 2, AgentSubject(keep.ID)); len(got) != 1 {
		t.Fatalf("control: the kept agent's history %q, want its row", got)
	}
}

// TestHistoryIsPerOwner: another owner reads none of your history and cannot
// write into it — not through the store, not with a raw insert naming your
// subject.
func TestHistoryIsPerOwner(t *testing.T) {
	s, pool := pgStore(t)
	sub := MirrorSubject("0b2c3d4e-hist-own")
	write(t, s, 2, sub, "Mine", "private", "")
	if got := titles(t, s, 2, sub); len(got) != 1 {
		t.Fatalf("positive control: the owner reads %q, want its row", got)
	}
	if got := titles(t, s, 3, sub); len(got) != 0 {
		t.Fatalf("user 3 reads user 2's history: %q", got)
	}
	var n int
	if err := pool.QueryRow(as(3), `SELECT count(*) FROM agent_focus_history`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("user 3 counts %d history rows (%v), want 0", n, err)
	}
	// A raw insert as user 3 naming owner 2 outright fails the policy; one
	// naming no owner lands as user 3's OWN row (owner_id defaults to the
	// principal), never in user 2's history.
	if _, err := pool.Exec(as(3), `INSERT INTO agent_focus_history (owner_id, subject_kind, subject_key, title) VALUES (2, 'mirror', $1, 'forged')`, sub.Key); err == nil {
		t.Fatal("user 3 appended to user 2's history")
	}
	if _, err := pool.Exec(as(3), `INSERT INTO agent_focus_history (subject_kind, subject_key, title) VALUES ('mirror', $1, 'forged')`, sub.Key); err != nil {
		t.Fatalf("user 3's insert into its own history: %v", err)
	}
	if got := titles(t, s, 2, sub); !eq(got, []string{"Mine|private"}) {
		t.Fatalf("user 2's history after user 3's attempts: %q", got)
	}
	if got := titles(t, s, 3, sub); !eq(got, []string{"forged|"}) {
		t.Fatalf("user 3's own row: %q", got)
	}
}

// TestHistoryPagesAtTheirBoundaries: an empty history, exactly one page, one
// past a page, a tie on created_at broken by id, the limit's bounds, and a
// cursor this package did not write.
func TestHistoryPagesAtTheirBoundaries(t *testing.T) {
	s, pool := pgStore(t)
	sub := AgentSubject(mustAgent(t, pool, 2, "focus-hist-pages").ID)
	page, err := s.History(as(2), sub, "", 3)
	if err != nil || len(page.Entries) != 0 || page.Next != "" {
		t.Fatalf("empty history: %+v %v", page, err)
	}
	for i := 1; i <= 3; i++ {
		write(t, s, 2, sub, fmt.Sprintf("focus %d", i), "", "")
	}
	// Exactly a page: three rows, and no next page.
	page, err = s.History(as(2), sub, "", 3)
	if err != nil || len(page.Entries) != 3 || page.Next != "" || page.Entries[0].Title != "focus 3" {
		t.Fatalf("exactly one page: %+v %v", page, err)
	}
	// One past a page: the fourth row is the next page, alone, and ends it.
	write(t, s, 2, sub, "focus 4", "", "")
	page, err = s.History(as(2), sub, "", 3)
	if err != nil || len(page.Entries) != 3 || page.Next == "" || page.Entries[0].Title != "focus 4" || page.Entries[2].Title != "focus 2" {
		t.Fatalf("first of two pages: %+v %v", page, err)
	}
	last, err := s.History(as(2), sub, page.Next, 3)
	if err != nil || len(last.Entries) != 1 || last.Entries[0].Title != "focus 1" || last.Next != "" {
		t.Fatalf("second page: %+v %v", last, err)
	}
	// A tie on created_at (two rows stamped the same microsecond) is ordered by
	// id and never skipped or repeated across the page boundary.
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE agent_focus_history SET created_at = '2026-10-01T00:00:00Z' WHERE subject_key = $1`, sub.Key)
	var got []string
	cursor := ""
	for {
		p, err := s.History(as(2), sub, cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range p.Entries {
			got = append(got, e.Title)
		}
		if p.Next == "" {
			break
		}
		cursor = p.Next
	}
	if !eq(got, []string{"focus 4", "focus 3", "focus 2", "focus 1"}) {
		t.Fatalf("tied rows paged one at a time: %q", got)
	}
	// The limit is bounded both ways.
	for i := 5; i <= MaxHistoryPage+5; i++ {
		write(t, s, 2, sub, fmt.Sprintf("focus %d", i), "", "")
	}
	if p, _ := s.History(as(2), sub, "", 0); len(p.Entries) != DefaultHistoryPage {
		t.Errorf("limit 0 read %d rows, want DefaultHistoryPage (%d)", len(p.Entries), DefaultHistoryPage)
	}
	if p, _ := s.History(as(2), sub, "", 10_000); len(p.Entries) != MaxHistoryPage || p.Next == "" {
		t.Errorf("limit 10000 read %d rows (next %q), want MaxHistoryPage (%d) and a next page", len(p.Entries), p.Next, MaxHistoryPage)
	}
	for _, bad := range []string{"x", "1", "1.x", "0.5", "-3.4", "12.0"} {
		if _, err := s.History(as(2), sub, bad, 3); !errors.Is(err, ErrBadCursor) {
			t.Errorf("cursor %q: %v, want ErrBadCursor", bad, err)
		}
	}
}

// TestTheNewestHistoryRowIsTheCurrentFocusUnderInterleavedWriters: two
// writers of one subject on two connections, the one that BEGAN first made to
// wait on the row lock the other holds, so it writes LAST. Its focus is the
// one that stands — so its history row must be the newest. That is what
// appendHistorySQL's clock_timestamp() is for: now() is when the transaction
// BEGAN, before the wait, and would stamp the last writer's row older than
// the row it waited for (the now() mutant is this test's control).
func TestTheNewestHistoryRowIsTheCurrentFocusUnderInterleavedWriters(t *testing.T) {
	s, pool := pgStore(t)
	sub := MirrorSubject("0b2c3d4e-hist-interleave")
	write(t, s, 2, sub, "start", "", "")
	ctx := as(2)
	upsert := func(tx pgx.Tx, title string) error {
		f, err := scan(tx.QueryRow(ctx, upsertFocusSQL, string(sub.Kind), sub.Key, nil, title, "", string(SourceSummarizer), ""))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, appendHistorySQL, string(sub.Kind), sub.Key, nil, f.Title, f.Description, f.Owner)
		return err
	}
	// The EARLY writer begins first; its transaction start is now() for it.
	early, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = early.Rollback(context.Background()) }()
	if _, err := early.Exec(ctx, `SELECT 1`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	// The LATE writer begins after it, takes the row lock and writes first.
	late, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = late.Rollback(context.Background()) }()
	if err := upsert(late, "written by the late starter"); err != nil {
		t.Fatal(err)
	}
	// The early writer now blocks on that lock…
	done := make(chan error, 1)
	go func() {
		if err := upsert(early, "written by the early starter"); err != nil {
			done <- err
			return
		}
		done <- early.Commit(ctx)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(authz.System(context.Background()),
			`SELECT count(*) FROM pg_locks WHERE NOT granted`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the early writer never waited on the row lock: the interleaving was not built")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// …until the late one commits; then it writes, and its focus stands.
	if err := late.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	cur, _, err := s.Get(ctx, sub)
	if err != nil || cur.Title != "written by the early starter" {
		t.Fatalf("control: the current focus is %q (%v), want the early starter's (it wrote last)", cur.Title, err)
	}
	page, err := s.History(ctx, sub, "", 1)
	if err != nil || len(page.Entries) != 1 {
		t.Fatalf("history: %+v %v", page, err)
	}
	if page.Entries[0].Title != cur.Title {
		t.Fatalf("the newest history row is %q but the current focus is %q: a writer that waited stamped its row older than the one it waited for",
			page.Entries[0].Title, cur.Title)
	}
}
