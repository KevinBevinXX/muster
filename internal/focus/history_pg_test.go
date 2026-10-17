package focus

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

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

// TestHistoryPruneIsByAgeAcrossOwners: PruneHistory deletes every owner's rows
// written before the cut, and only those.
func TestHistoryPruneIsByAgeAcrossOwners(t *testing.T) {
	s, pool := pgStore(t)
	mine := AgentSubject(mustAgent(t, pool, 2, "focus-hist-prune2").ID)
	theirs := AgentSubject(mustAgent(t, pool, 3, "focus-hist-prune3").ID)
	write(t, s, 2, mine, "old 2", "", "")
	write(t, s, 3, theirs, "old 3", "", "")
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE agent_focus_history SET created_at = now() - interval '31 days'`)
	write(t, s, 2, mine, "new 2", "", "")
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE agent_focus_history SET created_at = now() - interval '29 days' WHERE title = 'new 2'`)
	n, err := s.PruneHistory(context.Background(), time.Now().Add(-DefaultHistoryRetention))
	if err != nil || n != 2 {
		t.Fatalf("pruned %d (%v), want 2: one 31-day-old row per owner", n, err)
	}
	if got := titles(t, s, 2, mine); !eq(got, []string{"new 2|"}) {
		t.Fatalf("owner 2's history %q, want only the 29-day-old row", got)
	}
	if got := titles(t, s, 3, theirs); len(got) != 0 {
		t.Fatalf("owner 3's old row survived: %q", got)
	}
	// The focus rows themselves are not history: they stay.
	if _, ok, _ := s.Get(as(3), theirs); !ok {
		t.Fatal("pruning history deleted a focus")
	}
}

// TestRetainerPrunesHistoryAfterThirtyDaysByDefault: the hourly sweep asks for
// history older than 720h, or the configured HistoryRetention.
func TestRetainerPrunesHistoryAfterThirtyDaysByDefault(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		set, want time.Duration
	}{{0, 720 * time.Hour}, {48 * time.Hour, 48 * time.Hour}} {
		m := newMem()
		r := &Retainer{Store: m, Sessions: &fakeSource{}, AsOwner: func(ctx context.Context, _ int64) context.Context { return ctx },
			HistoryRetention: c.set, Now: func() time.Time { return now }}
		if _, err := r.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := now.Sub(m.historyPrunedBefore); got != c.want {
			t.Errorf("HistoryRetention %s: pruned history before now-%s, want now-%s", c.set, got, c.want)
		}
	}
}

// TestHistoryDiesWithItsFocusRow: both ways a focus row goes — the retention
// sweep pruning a mirrored session, and an agent's deletion cascading — take
// the subject's history with them, and nobody else's.
func TestHistoryDiesWithItsFocusRow(t *testing.T) {
	s, pool := pgStore(t)
	dead, live := MirrorSubject("0b2c3d4e-hist-dead"), MirrorSubject("0b2c3d4e-hist-live")
	a := mustAgent(t, pool, 2, "focus-hist-cascade")
	keep := mustAgent(t, pool, 2, "focus-hist-keep")
	for _, sub := range []Subject{dead, live, AgentSubject(a.ID), AgentSubject(keep.ID)} {
		write(t, s, 2, sub, "one "+sub.Key, "", "")
		write(t, s, 2, sub, "two "+sub.Key, "", "")
	}
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE agent_focus SET seen_at = now() - interval '10 days' WHERE subject_key = $1`, dead.Key)
	if n, err := s.PruneAll(context.Background(), time.Now().Add(-7*24*time.Hour)); err != nil || n != 1 {
		t.Fatalf("pruned %d (%v), want the one dead session", n, err)
	}
	if _, err := pool.Exec(as(2), `DELETE FROM agents WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	left := map[string]int{}
	rows, err := pool.Query(authz.System(context.Background()), `SELECT subject_key, count(*) FROM agent_focus_history GROUP BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			t.Fatal(err)
		}
		left[k] = n
	}
	rows.Close()
	want := map[string]int{live.Key: 2, AgentSubject(keep.ID).Key: 2}
	if fmt.Sprint(left) != fmt.Sprint(want) {
		t.Fatalf("history rows by subject %v, want %v (the pruned session's and the deleted agent's gone, the rest kept)", left, want)
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
	// A raw insert as user 3 naming user 2's subject: the row would be user 3's
	// (owner_id defaults to the principal), and user 3 has no focus row for it,
	// so the foreign key refuses it; naming owner 2 outright fails the policy.
	if _, err := pool.Exec(as(3), `INSERT INTO agent_focus_history (subject_kind, subject_key, title) VALUES ('mirror', $1, 'forged')`, sub.Key); err == nil {
		t.Fatal("user 3 appended to a history it has no focus for")
	}
	if _, err := pool.Exec(as(3), `INSERT INTO agent_focus_history (owner_id, subject_kind, subject_key, title) VALUES (2, 'mirror', $1, 'forged')`, sub.Key); err == nil {
		t.Fatal("user 3 appended to user 2's history")
	}
	if got := titles(t, s, 2, sub); !eq(got, []string{"Mine|private"}) {
		t.Fatalf("user 2's history after user 3's attempts: %q", got)
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
