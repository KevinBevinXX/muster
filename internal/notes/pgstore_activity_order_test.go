package notes

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// The BOARD ORDERING tests (PGStore.ListPage).
//
// 🔴 WHY THEY MUST BE POSTGRES-BACKED. The whole change is a SQL expression —
// GREATEST over four columns owned by four different tables, reached through
// three LATERAL joins. There is no Go code to unit-test: an in-memory fake would
// be a SECOND implementation of the rule, and a green fake proves only that the
// fake agrees with itself. So these run against a real database or they skip.
//
// Like every other PG-gated test in this repo, they SKIP without
// MUSTER_TEST_DATABASE_URL — and a skipped test is not a passing one:
//
//	MUSTER_TEST_DATABASE_URL='postgres://postgres:x@localhost:55432/postgres?sslmode=disable' \
//	  go test ./internal/notes -run TestListPage
//
// 🔴 EVERY FIXTURE IS TAG-ISOLATED. `go test ./...` points several packages at
// ONE database, and other tests leave rows behind. Each test here tags its own
// tasks with a unique marker and queries through that tag, so it asserts an
// order over exactly its own rows and cannot be reddened — or, far worse,
// greened — by somebody else's data.

// activityFixture is a Postgres-backed test store plus the raw pool, so a test
// can write timestamps the STORE deliberately does not let it write (the store
// always stamps now()).
type activityFixture struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	store *PGStore
	tag   string
}

func newActivityFixture(t *testing.T, marker string) *activityFixture {
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
	return &activityFixture{t: t, ctx: ctx, pool: pool, store: NewPG(pool), tag: marker}
}

// task creates one live task carrying the fixture's isolation tag, then forces
// its updated_at to `at`.
//
// 🔴 The forced UPDATE is the point of the whole helper: notes.updated_at is
// written as now() by every store path, so a test that used the store alone
// could only ever produce timestamps in creation order — which is exactly the
// dimension these tests need to vary independently of the activity columns.
func (f *activityFixture) task(body, status string, at time.Time) int64 {
	f.t.Helper()
	n, err := f.store.Create(f.ctx, Note{Body: body, Tags: []string{f.tag}})
	if err != nil {
		f.t.Fatalf("create %q: %v", body, err)
	}
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE notes SET status = $2, updated_at = $3 WHERE id = $1`, n.ID, status, at); err != nil {
		f.t.Fatalf("stamp note %d: %v", n.ID, err)
	}
	return n.ID
}

// comment inserts a comment row DIRECTLY, with an explicit created_at and
// WITHOUT touching notes.updated_at.
//
// 🔴 THE BYPASS IS DELIBERATE AND IT IS WHAT MAKES THE ASSERTION NON-VACUOUS.
// Store.AddComment bumps notes.updated_at in the same statement (the `bump` CTE
// in pgstore.go), so a comment added through the store floats its task under the
// OLD ordering too — a test using it would pass with the comment lateral deleted.
// Writing the row directly isolates the term.
func (f *activityFixture) comment(noteID int64, body string, at time.Time) int64 {
	f.t.Helper()
	var id int64
	err := f.pool.QueryRow(f.ctx,
		`INSERT INTO note_comments (note_id, author, body, created_at) VALUES ($1,'fixture',$2,$3) RETURNING id`,
		noteID, body, at).Scan(&id)
	if err != nil {
		f.t.Fatalf("insert comment on %d: %v", noteID, err)
	}
	return id
}

// agent links an agent row to a task with an explicit updated_at. No store path
// writes notes.updated_at when an agent moves, so this term is live in
// production, not merely defensive.
func (f *activityFixture) agent(noteID int64, name string, at time.Time) {
	f.t.Helper()
	_, err := f.pool.Exec(f.ctx,
		`INSERT INTO agents (name, namespace, note_id, updated_at) VALUES ($1,'workspace-fixture',$2,$3)`,
		name, noteID, at)
	if err != nil {
		f.t.Fatalf("insert agent %q: %v", name, err)
	}
}

// session links a coding-agent session to a task with an explicit last_seen_at.
// LinkSession likewise never bumps notes.updated_at.
func (f *activityFixture) session(noteID int64, sessionID string, at time.Time) {
	f.t.Helper()
	_, err := f.pool.Exec(f.ctx,
		`INSERT INTO task_sessions (note_id, session_id, role, first_seen_at, last_seen_at)
		 VALUES ($1,$2,'worked',$3,$3)`, noteID, sessionID, at)
	if err != nil {
		f.t.Fatalf("link session %q: %v", sessionID, err)
	}
}

// order returns the ids ListPage answers for the fixture's own tasks, in order.
func (f *activityFixture) order(filter ListFilter) []int64 {
	f.t.Helper()
	filter.Tags = append([]string{f.tag}, filter.Tags...)
	p, err := f.store.ListPage(f.ctx, filter)
	if err != nil {
		f.t.Fatalf("list page: %v", err)
	}
	ids := make([]int64, 0, len(p.Notes))
	for _, n := range p.Notes {
		ids = append(ids, n.ID)
	}
	return ids
}

func sameIDs(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// uniqueMarker builds a per-run isolation tag. It no longer separates this
// package from OTHER packages — internal/dbtest does that, with a database — but
// it still separates two concurrent runs of THIS package against the same
// server, and two tests in this file from each other.
func uniqueMarker(prefix string) string {
	return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
}

// TestListPageOrdersByActivityAcrossAllFourSources is the CENTRAL claim.
//
// 🔴 IT IS ONE ASSERTION OVER A LITERAL ORDER, AND THAT IS WHAT MAKES IT
// ATTRIBUTE. Five tasks, five DISTINCT notes.updated_at values, and three of
// them carrying exactly one kind of activity each:
//
//	A  updated 01:00, a COMMENT at 10:00   -> should be 1st
//	B  updated 02:00, an AGENT   at 09:00  -> should be 2nd
//	C  updated 03:00, a SESSION  at 08:00  -> should be 3rd
//	E  updated 05:00, nothing              -> should be 4th
//	D  updated 04:00, nothing              -> should be 5th
//
// Under the OLD rule (`ORDER BY updated_at DESC`) this is E,D,C,B,A — the exact
// REVERSE of the head of the expected order, so the test cannot pass by accident
// of the seed order, of insertion order, or of the primary key.
//
// Delete any ONE of the three laterals and the corresponding task falls to its
// updated_at position, changing the literal — so the single assertion is not a
// bundle: each term is independently pinned by it.
//
// The statuses are deliberately scrambled (A is COMPLETE and must still be
// FIRST). That is the flattening claim: order is activity, never status.
//
// 🔴 Every timestamp is pairwise distinct AND distinct from every other
// timestamp in the fixture, so no assertion can be satisfied by two values that
// merely happen to be equal.
func TestListPageOrdersByActivityAcrossAllFourSources(t *testing.T) {
	f := newActivityFixture(t, uniqueMarker("ordfix"))
	base := time.Date(2024, 3, 7, 0, 0, 0, 0, time.UTC)
	at := func(h int) time.Time { return base.Add(time.Duration(h) * time.Hour) }

	a := f.task("A: oldest note, newest comment", StatusComplete, at(1))
	b := f.task("B: an agent is working it", StatusOpen, at(2))
	c := f.task("C: a session picked it up", StatusReadyForReview, at(3))
	d := f.task("D: untouched, older", StatusInProgress, at(4))
	e := f.task("E: untouched, newer", StatusOpen, at(5))

	f.comment(a, "a direct comment that does NOT bump updated_at", at(10))
	f.agent(b, uniqueMarker("agent-"), at(9))
	f.session(c, uniqueMarker("sess-"), at(8))

	got := f.order(ListFilter{})
	want := []int64{a, b, c, e, d}
	if !sameIDs(got, want) {
		t.Fatalf("board order = %v, want %v\n"+
			"  A=%d (updated 01:00, comment 10:00)  B=%d (updated 02:00, agent 09:00)\n"+
			"  C=%d (updated 03:00, session 08:00)  D=%d (updated 04:00)  E=%d (updated 05:00)\n"+
			"Under the OLD `ORDER BY updated_at DESC` this is [E D C B A]. If you got that, the "+
			"GREATEST(...) ordering is not in effect at all. If exactly ONE task is out of place, the "+
			"lateral for THAT source (comment / agent / session) is missing or mis-joined.",
			got, want, a, b, c, d, e)
	}
}

// TestListPageCommentFloatsATaskUpdatedAtAloneWouldNot isolates the comment term
// to a two-task fixture, so a failure names the term without arithmetic.
//
// ⚠ HONEST SCOPE, because this one is easy to over-read: Store.AddComment ALSO
// bumps notes.updated_at, so in production a comment written through the store
// floats its task with or without this lateral. This test writes the row
// directly and therefore pins the SQL expression, not the live user-visible
// mechanism. It is worth having anyway — it is what stops a future comment
// producer (a backfill, an import, a second write path) from silently landing
// tasks at the bottom of the board.
func TestListPageCommentFloatsATaskUpdatedAtAloneWouldNot(t *testing.T) {
	f := newActivityFixture(t, uniqueMarker("cmtfix"))
	base := time.Date(2024, 5, 19, 0, 0, 0, 0, time.UTC)

	quiet := f.task("no comments, newer note", StatusOpen, base.Add(6*time.Hour))
	chatty := f.task("older note, fresh comment", StatusOpen, base.Add(2*time.Hour))

	// Baseline: with no comment, updated_at alone puts `quiet` first. This is the
	// NEGATIVE CONTROL — without it, "chatty is first" could be an artifact of
	// insertion order or the id tiebreak rather than of the comment.
	if got, want := f.order(ListFilter{}), []int64{quiet, chatty}; !sameIDs(got, want) {
		t.Fatalf("baseline order = %v, want %v (quiet has the newer updated_at)", got, want)
	}

	f.comment(chatty, "picked this up", base.Add(9*time.Hour))

	if got, want := f.order(ListFilter{}), []int64{chatty, quiet}; !sameIDs(got, want) {
		t.Fatalf("after a comment at 09:00 on the OLDER task, order = %v, want %v — a comment must "+
			"float its task above one whose notes.updated_at is newer", got, want)
	}
}

// TestListPageAgentActivityFloatsATask isolates the AGENT term. Unlike the
// comment term this one is the LIVE mechanism: every agent mutation writes
// agents.updated_at and none of them touch the note, so before this change a
// task an agent had been working for hours sat wherever its last human edit left
// it.
func TestListPageAgentActivityFloatsATask(t *testing.T) {
	f := newActivityFixture(t, uniqueMarker("agtfix"))
	base := time.Date(2024, 7, 3, 0, 0, 0, 0, time.UTC)

	idle := f.task("edited recently, nobody working it", StatusOpen, base.Add(7*time.Hour))
	worked := f.task("edited long ago, an agent is on it", StatusOpen, base.Add(1*time.Hour))

	if got, want := f.order(ListFilter{}), []int64{idle, worked}; !sameIDs(got, want) {
		t.Fatalf("baseline order = %v, want %v", got, want)
	}

	f.agent(worked, uniqueMarker("agent-"), base.Add(11*time.Hour))

	if got, want := f.order(ListFilter{}), []int64{worked, idle}; !sameIDs(got, want) {
		t.Fatalf("after agent activity at 11:00, order = %v, want %v — a task being worked RIGHT NOW "+
			"must outrank one whose only recency is an old edit", got, want)
	}
}

// TestListPageSessionLinkFloatsATask isolates the SESSION term — likewise live:
// LinkSession writes task_sessions.last_seen_at and never bumps the note.
func TestListPageSessionLinkFloatsATask(t *testing.T) {
	f := newActivityFixture(t, uniqueMarker("sesfix"))
	base := time.Date(2024, 9, 11, 0, 0, 0, 0, time.UTC)

	stale := f.task("edited recently, no session", StatusOpen, base.Add(8*time.Hour))
	picked := f.task("edited long ago, a session touched it", StatusOpen, base.Add(3*time.Hour))

	if got, want := f.order(ListFilter{}), []int64{stale, picked}; !sameIDs(got, want) {
		t.Fatalf("baseline order = %v, want %v", got, want)
	}

	f.session(picked, uniqueMarker("sess-"), base.Add(13*time.Hour))

	if got, want := f.order(ListFilter{}), []int64{picked, stale}; !sameIDs(got, want) {
		t.Fatalf("after a session link at 13:00, order = %v, want %v", got, want)
	}
}

// TestListPageRetractedCommentStillFloatsTheTask is the behavioural half of the
// note_comments ledger entry for ListPage.
//
// 🔴 The ledger CANNOT see this. Its liveOnly flag is an AST match on an
// identifier, and ListPage legitimately references that constant for the NOTES
// predicate — so a comment-level `deleted_at IS NULL` added to the activity
// lateral would leave the ledger green. This is what covers it: retracting a
// comment must not silently SINK its task back down the board, for the same
// reason a retracted comment tombstones instead of vanishing.
func TestListPageRetractedCommentStillFloatsTheTask(t *testing.T) {
	f := newActivityFixture(t, uniqueMarker("retfix"))
	base := time.Date(2024, 11, 23, 0, 0, 0, 0, time.UTC)

	other := f.task("newer note, no thread", StatusOpen, base.Add(4*time.Hour))
	floated := f.task("older note, one comment", StatusOpen, base.Add(2*time.Hour))
	cid := f.comment(floated, "worth reading", base.Add(12*time.Hour))

	if got, want := f.order(ListFilter{}), []int64{floated, other}; !sameIDs(got, want) {
		t.Fatalf("baseline (live comment) order = %v, want %v", got, want)
	}

	// Retract it through the store — the real path.
	if err := f.store.SoftDeleteComment(f.ctx, floated, cid); err != nil {
		t.Fatalf("retract comment: %v", err)
	}
	// Re-stamp updated_at: SoftDeleteComment is not supposed to move it, but
	// pinning it here makes the assertion below about the LATERAL rather than
	// about whatever the retraction happened to do to the note.
	if _, err := f.pool.Exec(f.ctx, `UPDATE notes SET updated_at = $2 WHERE id = $1`,
		floated, base.Add(2*time.Hour)); err != nil {
		t.Fatalf("re-stamp: %v", err)
	}

	if got, want := f.order(ListFilter{}), []int64{floated, other}; !sameIDs(got, want) {
		t.Fatalf("after RETRACTING the comment, order = %v, want %v — retracting a comment must not "+
			"sink the task: the activity lateral counts the comment's created_at and is deliberately "+
			"NOT filtered by deleted_at", got, want)
	}
}

// TestListPageTieBreaksByIDDescending.
//
// Two tasks with the BYTE-IDENTICAL activity timestamp must come back in a
// deterministic order, or an idiomorph swap reshuffles cards for no reason the
// user can see. The fixture forces the tie (same updated_at, no activity rows),
// which is not a hypothetical: a merge, a backfill or an import writes several
// rows inside one statement.
func TestListPageTieBreaksByIDDescending(t *testing.T) {
	f := newActivityFixture(t, uniqueMarker("tiefix"))
	tied := time.Date(2024, 2, 29, 12, 34, 56, 789000, time.UTC)

	first := f.task("tied one", StatusOpen, tied)
	second := f.task("tied two", StatusOpen, tied)
	third := f.task("tied three", StatusOpen, tied)

	got := f.order(ListFilter{})
	want := []int64{third, second, first} // id DESC
	if !sameIDs(got, want) {
		t.Fatalf("tied order = %v, want %v (id DESC) — without a deterministic tiebreak the plan "+
			"decides, and the board reshuffles on every refetch", got, want)
	}
	// Run it again: a NON-deterministic order would be free to differ, and a
	// single read cannot tell "deterministic" from "lucky".
	if again := f.order(ListFilter{}); !sameIDs(again, want) {
		t.Fatalf("a second identical query answered %v then %v — the order is not deterministic", got, again)
	}
}

// TestListPageLimitTruncatesButTotalCountsEverything.
//
// 🔴 BOTH HALVES OR NEITHER. A Limit that also shrank Total would make the board
// unable to tell a truncated list from a complete one — which is what the
// show-more control and its "N of M shown" line are built on, and is the same
// class of lie as a filter rendering "all clear". Five is chosen because it is
// not the default page size (50) and not a power of two: a mutant that clamped
// to a constant, or that returned Limit as the Total, cannot hide behind it.
func TestListPageLimitTruncatesButTotalCountsEverything(t *testing.T) {
	f := newActivityFixture(t, uniqueMarker("limfix"))
	base := time.Date(2025, 1, 6, 0, 0, 0, 0, time.UTC)

	ids := make([]int64, 0, 5)
	for i := 0; i < 5; i++ {
		// Descending updated_at, so the expected page is the first N in creation
		// order — a literal, not something re-derived from the returned slice.
		ids = append(ids, f.task(fmt.Sprintf("task %d", i), StatusOpen,
			base.Add(time.Duration(100-i)*time.Hour)))
	}

	p, err := f.store.ListPage(f.ctx, ListFilter{Tags: []string{f.tag}, Limit: 2})
	if err != nil {
		t.Fatalf("list page: %v", err)
	}
	if len(p.Notes) != 2 {
		t.Fatalf("Limit 2 returned %d notes, want 2", len(p.Notes))
	}
	if p.Total != 5 {
		t.Fatalf("Total = %d, want 5 — Total must count the whole MATCHING set, before Limit, or the "+
			"board cannot tell a truncated list from a complete one", p.Total)
	}
	if got := []int64{p.Notes[0].ID, p.Notes[1].ID}; !sameIDs(got, ids[:2]) {
		t.Fatalf("Limit 2 returned %v, want the first page %v", got, ids[:2])
	}

	// A limit at or above the total is a complete page — and Total must not move.
	whole, err := f.store.ListPage(f.ctx, ListFilter{Tags: []string{f.tag}, Limit: 5})
	if err != nil {
		t.Fatalf("list page (whole): %v", err)
	}
	if len(whole.Notes) != 5 || whole.Total != 5 {
		t.Fatalf("Limit 5 returned %d notes / Total %d, want 5 / 5", len(whole.Notes), whole.Total)
	}

	// Limit 0 means NO cap (that is what List/ListByTags pass).
	uncapped, err := f.store.ListPage(f.ctx, ListFilter{Tags: []string{f.tag}})
	if err != nil {
		t.Fatalf("list page (uncapped): %v", err)
	}
	if len(uncapped.Notes) != 5 {
		t.Fatalf("Limit 0 returned %d notes, want all 5 — 0 must mean no cap, not an empty page",
			len(uncapped.Notes))
	}
}

// TestListPageStatusFilterComposesWithTheTagFilter.
//
// 🔴 COMPOSITION IS THE CLAIM, not each filter separately. The board has three
// controls (project chips, tag chips, status chips) and a user who picks a
// project and then a status must get the INTERSECTION; a status filter that
// replaced the tag filter would silently widen the board back out. The fixture
// gives every cell a distinct occupant so a filter that ignored one dimension
// returns a visibly wrong SET, not merely a wrong count.
func TestListPageStatusFilterComposesWithTheTagFilter(t *testing.T) {
	f := newActivityFixture(t, uniqueMarker("cmpfix"))
	base := time.Date(2025, 4, 2, 0, 0, 0, 0, time.UTC)
	extra := uniqueMarker("proj")

	withTagOpen := f.task("tagged + open", StatusOpen, base.Add(1*time.Hour))
	withTagDone := f.task("tagged + complete", StatusComplete, base.Add(2*time.Hour))
	noTagOpen := f.task("untagged + open", StatusOpen, base.Add(3*time.Hour))
	noTagDone := f.task("untagged + complete", StatusComplete, base.Add(4*time.Hour))
	for _, id := range []int64{withTagOpen, withTagDone} {
		if _, err := f.store.AddTags(f.ctx, id, []string{extra}); err != nil {
			t.Fatalf("add tag to %d: %v", id, err)
		}
		// AddTags bumps updated_at; re-stamp so the ORDER stays the one the fixture
		// declares and a failure below is about membership, not sequence.
		if _, err := f.pool.Exec(f.ctx, `UPDATE notes SET updated_at = $2 WHERE id = $1`,
			id, base.Add(time.Duration(id%2+1)*time.Hour)); err != nil {
			t.Fatalf("re-stamp %d: %v", id, err)
		}
	}

	set := func(f2 ListFilter) map[int64]bool {
		got := f.order(f2)
		m := map[int64]bool{}
		for _, id := range got {
			m[id] = true
		}
		return m
	}

	// Tag only: both tagged tasks, neither untagged one.
	if got := set(ListFilter{Tags: []string{extra}}); len(got) != 2 || !got[withTagOpen] || !got[withTagDone] {
		t.Fatalf("tag-only filter = %v, want exactly the two tagged tasks (%d, %d)",
			got, withTagOpen, withTagDone)
	}
	// Status only: both complete tasks, neither open one.
	if got := set(ListFilter{Statuses: []string{StatusComplete}}); len(got) != 2 ||
		!got[withTagDone] || !got[noTagDone] {
		t.Fatalf("status-only filter = %v, want exactly the two complete tasks (%d, %d)",
			got, withTagDone, noTagDone)
	}
	// BOTH: the single intersection cell.
	got := set(ListFilter{Tags: []string{extra}, Statuses: []string{StatusComplete}})
	if len(got) != 1 || !got[withTagDone] {
		t.Fatalf("tag AND status = %v, want exactly task %d (tagged + complete). "+
			"Two results means the status filter is being ignored; two DIFFERENT results means it "+
			"REPLACED the tag filter instead of narrowing it", got, withTagDone)
	}
	// The unfiltered baseline sees all four — so the narrowings above are real
	// narrowings and not an empty fixture.
	if all := set(ListFilter{}); len(all) != 4 {
		t.Fatalf("unfiltered = %v, want all 4 fixture tasks — the assertions above prove nothing "+
			"against a fixture that was never fully visible", all)
	}
	_ = noTagOpen
}

// TestListPageInProgressLaneCoversReadyForReview ties the SQL filter to the lane
// table: the In-progress chip must return BOTH statuses in that lane, or a
// review-ready task disappears from the only chip a user would look under.
func TestListPageInProgressLaneCoversReadyForReview(t *testing.T) {
	f := newActivityFixture(t, uniqueMarker("lanefix"))
	base := time.Date(2025, 6, 14, 0, 0, 0, 0, time.UTC)

	working := f.task("in progress", StatusInProgress, base.Add(1*time.Hour))
	review := f.task("ready for review", StatusReadyForReview, base.Add(2*time.Hour))
	open := f.task("open", StatusOpen, base.Add(3*time.Hour))

	got := f.order(ListFilter{Statuses: []string{StatusInProgress, StatusReadyForReview}})
	want := []int64{review, working} // activity order: review updated later
	if !sameIDs(got, want) {
		t.Fatalf("in-progress lane = %v, want %v (both statuses, newest activity first); the open "+
			"task %d must not be in it", got, want, open)
	}
}

// TestReapedTaskDoesNotFloatToTheTopOfTheBoard is the CROSS-PR guard: it is the
// only test in the tree that exercises the activity ordering (#727) and the
// non-bumping idle reaper (#729) TOGETHER, and it is the reason the comment
// lateral carries `AND c.author <> ReapCommentAuthor`.
//
// 🔴 WHY NEITHER PR'S OWN SUITE COULD SEE THIS. The break needs BOTH sides and
// each side's tests were scoped to one surface. #727's ordering tests seed
// comments through the fixture (author 'fixture'), so no reap comment ever
// existed in them. #729's reaper tests assert on notes.updated_at and on the
// comment thread, and never call ListPage, so the ordering was never observed.
// Merged, PGStore.FlagIdle writes a ReapCommentAuthor comment with
// created_at = now() while deliberately NOT bumping notes.updated_at — so an
// unfiltered GREATEST(n.updated_at, cm.at, …) resolves to that comment's
// timestamp and REAPING A TASK NOBODY TOUCHED FLOATS IT TO THE TOP, reintroducing
// the inverted ordering the flat board exists to remove. Both suites stay green.
//
// 🔴 IT IS BEHAVIOURAL, NOT STRUCTURAL, ON PURPOSE. A test asserting the SQL
// string contains the filter would pass while proving nothing about the order the
// database actually returns. This reaps a real task through the real store method
// and asserts the observable position.
//
// The three phases each kill a different mutant:
//
//  1. baseline — `recent` outranks `reaped` before anything happens, so the
//     phase-2 assertion is not an artifact of seed or id order.
//  2. after FlagIdle — the reaped task must NOT have moved. This is the assertion
//     that goes RED with the author filter removed.
//  3. a HUMAN comment on the same task DOES float it. Without phase 3 the whole
//     test would still pass against a mutant that DELETED the comment lateral
//     outright (or filtered every author) — which would silently un-ship #727's
//     central feature. Phase 3 is what makes phase 2 a claim about the AUTHOR
//     rather than about comments in general.
func TestReapedTaskDoesNotFloatToTheTopOfTheBoard(t *testing.T) {
	f := newActivityFixture(t, uniqueMarker("reapordfix"))
	base := time.Date(2024, 3, 8, 0, 0, 0, 0, time.UTC)

	// `recent` is genuinely more recent; `reaped` is the old one the sweep picks up.
	recent := f.task("worked on lately", StatusOpen, base.Add(10*time.Hour))
	reaped := f.task("nobody has touched this in weeks", StatusOpen, base.Add(1*time.Hour))

	// Phase 1 — negative control on the fixture itself.
	if got, want := f.order(ListFilter{}), []int64{recent, reaped}; !sameIDs(got, want) {
		t.Fatalf("baseline order = %v, want %v (recent has the newer updated_at)", got, want)
	}

	// Phase 2 — reap it through the REAL store method. FlagIdle stamps the reap
	// comment with created_at = now(), which is years after every fixture
	// timestamp above, so an unfiltered lateral cannot fail to float it.
	wrote, err := f.store.FlagIdle(f.ctx, reaped, "stale", "No activity for 7 days.")
	if err != nil {
		t.Fatalf("FlagIdle: %v", err)
	}
	// Positive control on the reap: if FlagIdle wrote nothing there is no reap
	// comment, and the assertion below would pass vacuously — green for the wrong
	// reason, and still green with the author filter deleted.
	if !wrote {
		t.Fatalf("FlagIdle reported no write on task %d — the assertion below would be vacuous", reaped)
	}

	if got, want := f.order(ListFilter{}), []int64{recent, reaped}; !sameIDs(got, want) {
		t.Fatalf("after reaping task %d, order = %v, want %v — a REAP IS NOT ACTIVITY. "+
			"PGStore.FlagIdle deliberately does not bump notes.updated_at, so the board's comment "+
			"lateral must exclude ReapCommentAuthor (%q); without that filter GREATEST() picks up the "+
			"reap comment's created_at = now() and floats a task nobody touched to the top.",
			reaped, got, want, ReapCommentAuthor)
	}

	// Phase 3 — the same task, a REAL comment, still older than now(): it must
	// float. This is what proves the filter is author-scoped rather than the
	// lateral being dead.
	f.comment(reaped, "picking this back up", base.Add(20*time.Hour))
	if got, want := f.order(ListFilter{}), []int64{reaped, recent}; !sameIDs(got, want) {
		t.Fatalf("after a HUMAN comment on task %d, order = %v, want %v — the reap-author exclusion "+
			"must not blind the lateral to ordinary comments; if this fails the filter is too wide "+
			"(or the comment lateral was removed) and #727's central feature is gone", reaped, got, want)
	}

	// The board's other entry point delegates to the same query (List ->
	// ListByTags -> ListPage). Asserted rather than assumed, because "they share a
	// function today" is a claim about today's call graph.
	all, err := f.store.List(f.ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var seen []int64
	for _, n := range all {
		if n.ID == recent || n.ID == reaped {
			seen = append(seen, n.ID)
		}
	}
	if want := []int64{reaped, recent}; !sameIDs(seen, want) {
		t.Fatalf("List order over the fixture's tasks = %v, want %v — List must carry the same "+
			"reap-author exclusion as ListPage", seen, want)
	}
}
