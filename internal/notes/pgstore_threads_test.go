package notes

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// threadStore opens the disposable Postgres, migrates it, and returns a store.
// PG-gated exactly like the rest of this package's store tests: without
// MUSTER_TEST_DATABASE_URL these SKIP, and a skipped Postgres test is not a
// passing Postgres test — the gate report in the PR names the pass/skip pair.
func threadStore(t *testing.T) (context.Context, *pgxpool.Pool, *PGStore) {
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
	return ctx, pool, NewPG(pool)
}

// seedTask creates a throwaway task and registers its hard purge for cleanup, so
// the shared test database stays tidy across runs.
func seedTask(t *testing.T, ctx context.Context, s *PGStore, n Note) Note {
	t.Helper()
	if n.Body == "" {
		n.Body = "task-thread fixture"
	}
	out, err := s.Create(ctx, n)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Cleanup(func() { _ = s.Delete(dbtest.AsOperator(context.Background()), out.ID) })
	return out
}

// externalSessions is a stand-in for the OTHER service's record of a session.
//
// 🔴 IT IS A TEST DOUBLE BY NECESSITY, NOT BY PREFERENCE, AND THE REASON IS THE
// WHOLE POINT OF THIS FILE. The session/transcript record lives in a different
// service with a different database; this package is not allowed to read it, in
// SQL or in Go, and the guards in extraction_seam_test.go fail if it ever does.
// So the only shape a test at THIS tier can have is the production shape: a
// caller observes liveness somewhere else and hands the answer to LinkSession as
// an argument.
//
// 🔴 OBSERVATIONS ARE READS, NOT CONSTANTS. Every call site below takes its bool
// from observe() rather than writing `true`/`false` inline. Hardcoding it would
// disconnect the observation from the thing observed: a fixture whose Record()
// silently did nothing would still satisfy every detail_seen assertion. The
// negative controls (`observe must read false here, or the flip below proves
// nothing`) are what make that non-vacuous, and they only work because the
// double can genuinely report both answers.
type externalSessions struct{ live map[string]bool }

func newExternalSessions() *externalSessions { return &externalSessions{live: map[string]bool{}} }

// Record is the other service learning about a session.
func (e *externalSessions) Record(id string) { e.live[id] = true }

// Sweep is the other service's retention pass taking every record it holds.
// Modelled as "they are gone", which is all this package can observe.
func (e *externalSessions) Sweep() { e.live = map[string]bool{} }

// Observe is the caller's liveness read, the one whose answer reaches
// LinkSession.
func (e *externalSessions) Observe(t *testing.T, id string) bool {
	t.Helper()
	return e.live[id]
}

// ---------------------------------------------------------------------------
// 🔴 A TEST WAS DELETED HERE AT THE CARVE, AND THIS IS THE RECORD OF IT.
//
// The original suite ran the OTHER service's real retention sweep against its
// real session table and asserted this package's link rows survived it. What it
// was guarding was a schema mistake: had `task_sessions.session_id` carried a
// foreign key to that table, every task's thread would have silently emptied
// when the sweep ran, and a task that HAD five sessions would then have been
// indistinguishable from one that never had any.
//
// That mutation is UNREPRESENTABLE HERE. The table it referenced lives in
// another service's database; there is nothing in this schema to point a
// foreign key AT, so a ported version of the test would delete nothing, sweep
// nothing, and pass no matter what this package did. A guard that cannot fail
// is worse than a missing one — it reads as coverage and stops anyone looking.
//
// WHAT STILL HOLDS THE INVARIANT, all of it mechanical:
//
//   - TestNotesNeverNamesTheSessionTable and
//     TestNoSQLInThisPackageJoinsAnotherServicesTable (extraction_seam_test.go)
//     fail if any source here so much as NAMES another service's session table.
//   - TestSessionColsAndScanTargetsAgree, below, fails if the read projects a
//     column from anything but the `task_sessions` alias, or if a JOIN appears.
//   - TestPGLinkSessionDoesNotBlankDenormalisedContext pins the other half the
//     deleted test carried: the row stays self-sufficient, which is why
//     project/cwd/host are columns rather than a join.
//   - TestPGLinkSessionDetailSeenIsMonotonicAndNotCoalesced, below, pins the
//     stored half of the reaped-versus-never-recorded distinction, including
//     across the disappearance of the external record.
// ---------------------------------------------------------------------------

// TestPGLinkSessionIsIdempotent: the same session touching the same task twice is
// ONE row; last_seen_at moves and first_seen_at does not. Invariant guard.
//
// first_seen_at not moving is the half that is easy to get wrong and impossible to
// notice: it is simply absent from the DO UPDATE SET list, so adding it there
// would be a one-word change that silently rewrites when the session joined.
func TestPGLinkSessionIsIdempotent(t *testing.T) {
	ctx, _, s := threadStore(t)
	task := seedTask(t, ctx, s, Note{Body: "idempotent link"})
	const sid = "thread-idempotent-session"

	first, changed, err := s.LinkSession(ctx, task.ID, SessionLink{SessionID: sid, Role: RoleRead}, false)
	if err != nil || !changed {
		t.Fatalf("first link: err=%v changed=%v", err, changed)
	}
	time.Sleep(15 * time.Millisecond) // now() must observably advance

	second, changed, err := s.LinkSession(ctx, task.ID, SessionLink{SessionID: sid, Role: RoleRead}, false)
	if err != nil {
		t.Fatalf("second link: %v", err)
	}
	if changed {
		t.Fatalf("a repeat touch of the same role reported changed=true — callers broadcast on that, so every read would nudge every open tab")
	}

	links, err := s.SessionsForTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("got %d links after two touches by ONE session, want 1", len(links))
	}
	if !second.FirstSeenAt.Equal(first.FirstSeenAt) {
		t.Fatalf("first_seen_at MOVED (%s → %s); it records when the session joined and must never be rewritten",
			first.FirstSeenAt, second.FirstSeenAt)
	}
	// ⚠ last_seen_at deliberately does NOT move here — a repeat touch inside
	// lastSeenCoalesce writes nothing at all, so GET stays cheap. That behaviour has
	// its own test (TestPGLinkSessionCoalescesLastSeen); what THIS test owns is the
	// row count and the immutable head. A `worked` touch is outside the coalesce
	// rule (it is a real change), so use it to prove the tail can still move at all —
	// otherwise "one row, fixed head" would be satisfied by a store that never
	// updates anything.
	third, _, err := s.LinkSession(ctx, task.ID, SessionLink{SessionID: sid, Role: RoleWorked}, false)
	if err != nil {
		t.Fatalf("upgrade touch: %v", err)
	}
	if !third.LastSeenAt.After(first.LastSeenAt) {
		t.Fatalf("last_seen_at never advances, even on a real change (%s → %s); the reverse lookup orders by it, so a frozen tail makes the ordering meaningless",
			first.LastSeenAt, third.LastSeenAt)
	}
	if !third.FirstSeenAt.Equal(first.FirstSeenAt) {
		t.Fatalf("first_seen_at MOVED on the upgrade (%s → %s)", first.FirstSeenAt, third.FirstSeenAt)
	}
}

// TestPGLinkSessionRoleIsMonotonic exercises roleUpgradeExpr against a real
// database in BOTH directions, including `created` being terminal. Invariant
// guard; the pure Go twin is TestUpgradeRoleIsMonotonic.
func TestPGLinkSessionRoleIsMonotonic(t *testing.T) {
	ctx, _, s := threadStore(t)
	cases := []struct {
		name      string
		cur, next string
		want      string
	}{
		{"read_to_worked", RoleRead, RoleWorked, RoleWorked},
		{"read_to_created", RoleRead, RoleCreated, RoleCreated},
		{"worked_to_created", RoleWorked, RoleCreated, RoleCreated},
		{"worked_not_downgraded_by_read", RoleWorked, RoleRead, RoleWorked},
		{"created_not_downgraded_by_read", RoleCreated, RoleRead, RoleCreated},
		{"created_not_downgraded_by_worked", RoleCreated, RoleWorked, RoleCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := seedTask(t, ctx, s, Note{Body: "role " + tc.name})
			sid := "role-" + tc.name
			if _, _, err := s.LinkSession(ctx, task.ID, SessionLink{SessionID: sid, Role: tc.cur}, false); err != nil {
				t.Fatalf("seed link %q: %v", tc.cur, err)
			}
			got, changed, err := s.LinkSession(ctx, task.ID, SessionLink{SessionID: sid, Role: tc.next}, false)
			if err != nil {
				t.Fatalf("touch %q: %v", tc.next, err)
			}
			if got.Role != tc.want {
				t.Fatalf("%s then %s = role %q, want %q — the role model is monotonic (read < worked < created) and `created` is terminal",
					tc.cur, tc.next, got.Role, tc.want)
			}
			if wantChanged := tc.cur != tc.want; changed != wantChanged {
				t.Fatalf("changed = %v, want %v (a role UPGRADE is a change; a refused downgrade is not)", changed, wantChanged)
			}
		})
	}
}

// TestPGLinkSessionRefusesDismissedTask: a soft-deleted (dismissed) task must not
// be able to gain a session link. It mirrors AddComment's conditional insert
// exactly — the FK still points at the surviving row, so a bare upsert would write
// an edge into a thread nothing can see. Invariant guard.
//
// The two-part assertion matters: it is not enough that the call ERRORS, the row
// must actually be absent. An implementation that inserts and then reports an
// error would satisfy a one-part test while leaving the silent write behind.
func TestPGLinkSessionRefusesDismissedTask(t *testing.T) {
	ctx, pool, s := threadStore(t)
	task := seedTask(t, ctx, s, Note{Body: "dismissed cannot gain a link"})
	if err := s.SoftDelete(ctx, task.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	_, _, err := s.LinkSession(ctx, task.ID, SessionLink{SessionID: "after-dismiss", Role: RoleWorked}, false)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("linking a session to a DISMISSED task returned %v, want pgx.ErrNoRows — the liveOnly predicate must guard the write, not just the reads", err)
	}
	// Read the raw table: a dismissed task is invisible to SessionsForTask's
	// callers anyway, so only an unfiltered count can prove nothing was written.
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_sessions WHERE note_id=$1`, task.ID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d link row(s) written against a dismissed task — the refusal reported an error but the write still landed", n)
	}

	// And it works again once restored, so the guard is a predicate on liveness
	// rather than a permanent block.
	if _, err := s.Restore(ctx, task.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, _, err := s.LinkSession(ctx, task.ID, SessionLink{SessionID: "after-restore", Role: RoleWorked}, false); err != nil {
		t.Fatalf("link after restore: %v", err)
	}
}

// TestPGLinkSessionCapEvictsOldestReadAndNeverFails pins the ADVISORY cap.
//
// 🔴 THE SHAPE OF THIS TEST IS THE FIX. It used to assert that a 51st session was
// REFUSED with ErrTooManySessions, which is what production did — and that hard
// bound produced a permanent 409 on plain GETs once fifty sessions had read a
// task, plus a split write on PATCH …/status. The cap now evicts instead of
// refusing, so this asserts:
//
//   - the 51st NEW session links SUCCESSFULLY,
//   - the thread stays at the cap,
//   - the evicted row is the OLDEST `read`, and the newcomer is present,
//   - an already-linked session is (still) never refused.
func TestPGLinkSessionCapEvictsOldestReadAndNeverFails(t *testing.T) {
	ctx, _, s := threadStore(t)
	task := seedTask(t, ctx, s, Note{Body: "session cap eviction"})

	var firstSession string
	for i := 0; i < MaxTaskSessions; i++ {
		sid := "cap-session-" + strconv.Itoa(i)
		if _, _, err := s.LinkSession(ctx, task.ID, SessionLink{SessionID: sid, Role: RoleRead}, false); err != nil {
			t.Fatalf("seeding link %d/%d: %v", i+1, MaxTaskSessions, err)
		}
		if i == 0 {
			firstSession = sid // the OLDEST read — the one eviction must take
		}
		// Distinct last_seen_at values, so "oldest" is a real ordering rather than a
		// coin flip among identical timestamps.
		time.Sleep(time.Millisecond)
	}

	got, changed, err := s.LinkSession(ctx, task.ID, SessionLink{SessionID: "cap-overflow", Role: RoleRead}, false)
	if err != nil {
		t.Fatalf("linking session %d returned %v — the cap is ADVISORY and must never fail a link that has somewhere to evict to", MaxTaskSessions+1, err)
	}
	if !changed || got.SessionID != "cap-overflow" {
		t.Fatalf("the overflow link did not land: %+v changed=%v", got, changed)
	}

	links, err := s.SessionsForTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if len(links) != MaxTaskSessions {
		t.Fatalf("thread holds %d links after the overflow, want it trimmed back to %d", len(links), MaxTaskSessions)
	}
	byID := map[string]bool{}
	for _, l := range links {
		byID[l.SessionID] = true
	}
	if byID[firstSession] {
		t.Fatalf("the OLDEST read link (%q) survived; eviction must take the least recently active reader first", firstSession)
	}
	if !byID["cap-overflow"] {
		t.Fatalf("the new link was evicted instead of the oldest one — insert-then-trim must order by last_seen_at ASC")
	}

	// An already-linked session is never refused (it is an UPDATE, not a new row).
	if _, _, err := s.LinkSession(ctx, task.ID, SessionLink{SessionID: "cap-session-1", Role: RoleWorked}, false); err != nil {
		t.Fatalf("an ALREADY-LINKED session was refused at the cap: %v", err)
	}
}

// TestPGLinkSessionCapNeverEvictsWorkOnlySkips is the other half: `created` and
// `worked` links are NEVER evicted, so when a thread is full of them a new `read`
// breadcrumb is SKIPPED — reported as ErrTooManySessions for the caller to log and
// count, never as a request failure.
//
// It is the case that makes the "evict the oldest read" rule safe: without it, a
// heavily-worked task would start losing the records of who worked it.
func TestPGLinkSessionCapNeverEvictsWorkOnlySkips(t *testing.T) {
	ctx, _, s := threadStore(t)
	task := seedTask(t, ctx, s, Note{Body: "cap with no evictable reads"})

	for i := 0; i < MaxTaskSessions; i++ {
		sid := "worked-session-" + strconv.Itoa(i)
		if _, _, err := s.LinkSession(ctx, task.ID, SessionLink{SessionID: sid, Role: RoleWorked}, false); err != nil {
			t.Fatalf("seeding worked link %d: %v", i, err)
		}
	}

	_, changed, err := s.LinkSession(ctx, task.ID, SessionLink{SessionID: "skipped-reader", Role: RoleRead}, false)
	if !errors.Is(err, ErrTooManySessions) {
		t.Fatalf("a link with nothing evictable returned %v, want ErrTooManySessions (the SKIP signal)", err)
	}
	if changed {
		t.Fatalf("a skipped link reported changed=true; callers broadcast on that")
	}

	links, err := s.SessionsForTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if len(links) != MaxTaskSessions {
		t.Fatalf("thread holds %d links, want %d — a skip must not leave the new row behind", len(links), MaxTaskSessions)
	}
	for _, l := range links {
		if l.Role != RoleWorked {
			t.Fatalf("a %q link appeared in a worked-only thread (%+v); `worked` must never be evicted", l.Role, l)
		}
		if l.SessionID == "skipped-reader" {
			t.Fatalf("the skipped reader was left in the thread: %+v", l)
		}
	}

	// 🔴 THE ASYMMETRY, pinned. A new WORKED link on the same full thread is
	// RECORDED — the thread exceeds the advisory cap rather than dropping a record
	// of work. Losing "who worked this task" to housekeeping would be the wrong
	// trade, and without this assertion a future "fix" that made work evictable (or
	// that skipped it symmetrically with reads) would look like tidying.
	if _, changed, err := s.LinkSession(ctx, task.ID, SessionLink{SessionID: "extra-worker", Role: RoleWorked}, false); err != nil || !changed {
		t.Fatalf("a WORKED link past the cap was refused (err=%v changed=%v); work records must never be dropped for housekeeping", err, changed)
	}
	after, err := s.SessionsForTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if len(after) != MaxTaskSessions+1 {
		t.Fatalf("thread holds %d links after a worked link past the cap, want %d — only `read` links are evictable",
			len(after), MaxTaskSessions+1)
	}
}

// TestPGTrimThreadOnlyTouchesItsOwnTask pins the `note_id` scope of trimThread's
// DELETE.
//
// 🔴 THIS GUARDS AN UNBOUNDED DESTRUCTIVE DELETE, and until this test existed the
// only thing holding the scope was that nobody edited the line. Removing
// `WHERE note_id = $1` from the OUTER delete leaves the whole statement still
// plausible — the inner SELECT keeps its own `note_id = $1`, so it reads like a
// harmless de-duplication — while the delete matches by SESSION ID ALONE and wipes
// those sessions' links on EVERY OTHER TASK. Measured with that mutant: task B lost
// its link to a trim triggered on task A (0 links left).
//
// Session ids are shared across tasks BY DESIGN — one coding-agent session works
// several tasks; that is the entire feature — so the blast radius is not a corner
// case, it is the normal shape of the data. The fixture therefore shares ids
// deliberately: a fixture with disjoint session ids per task cannot see this
// mutation at all, which is exactly why the suite stayed green.
func TestPGTrimThreadOnlyTouchesItsOwnTask(t *testing.T) {
	ctx, _, s := threadStore(t)
	taskA := seedTask(t, ctx, s, Note{Body: "task A — will be driven over the cap"})
	taskB := seedTask(t, ctx, s, Note{Body: "task B — must be left alone"})

	// The SHARED ids: the same sessions are on both tasks' threads, and on B they
	// are `read` links, i.e. exactly the role trimThread is allowed to evict. If
	// the delete loses its note_id scope, these are what it takes.
	shared := []string{"shared-session-0", "shared-session-1", "shared-session-2"}
	for _, sid := range shared {
		if _, _, err := s.LinkSession(ctx, taskB.ID, SessionLink{SessionID: sid, Role: RoleRead}, false); err != nil {
			t.Fatalf("seed task B link %q: %v", sid, err)
		}
		if _, _, err := s.LinkSession(ctx, taskA.ID, SessionLink{SessionID: sid, Role: RoleRead}, false); err != nil {
			t.Fatalf("seed task A link %q: %v", sid, err)
		}
		time.Sleep(time.Millisecond) // distinct last_seen_at, so eviction order is real
	}

	// Positive control: task B really does have those links BEFORE the trim, so a
	// zero afterwards cannot be "they were never written".
	if got, err := s.SessionsForTask(ctx, taskB.ID); err != nil || len(got) != len(shared) {
		t.Fatalf("task B starts with %d links (err=%v), want %d — the assertion below would otherwise be vacuous", len(got), err, len(shared))
	}

	// Fill task A to the cap so the NEXT link triggers a trim. The shared ids
	// already count toward it.
	for i := len(shared); i < MaxTaskSessions; i++ {
		if _, _, err := s.LinkSession(ctx, taskA.ID, SessionLink{SessionID: "a-only-" + strconv.Itoa(i), Role: RoleRead}, false); err != nil {
			t.Fatalf("filling task A at %d: %v", i, err)
		}
		time.Sleep(time.Millisecond)
	}

	// The trigger: one more NEW session on task A ⇒ trimThread runs on task A.
	if _, _, err := s.LinkSession(ctx, taskA.ID, SessionLink{SessionID: "a-overflow", Role: RoleRead}, false); err != nil {
		t.Fatalf("overflow link on task A: %v", err)
	}

	// 🔴 THE ASSERTION. Task B is a different task and was never touched.
	afterB, err := s.SessionsForTask(ctx, taskB.ID)
	if err != nil {
		t.Fatalf("task B sessions: %v", err)
	}
	if len(afterB) != len(shared) {
		t.Fatalf("task B lost links to a trim on task A: %d left, want %d. trimThread's DELETE must be scoped by note_id — without it, it matches by session id alone and wipes every other task that shares a session.",
			len(afterB), len(shared))
	}
	stillThere := map[string]bool{}
	for _, l := range afterB {
		stillThere[l.SessionID] = true
	}
	for _, sid := range shared {
		if !stillThere[sid] {
			t.Fatalf("task B lost its link to session %q, which was evicted from task A's thread", sid)
		}
	}

	// And the trim DID do its job on its own task — otherwise this test would pass
	// against a trimThread that simply never deletes anything.
	afterA, err := s.SessionsForTask(ctx, taskA.ID)
	if err != nil {
		t.Fatalf("task A sessions: %v", err)
	}
	if len(afterA) != MaxTaskSessions {
		t.Fatalf("task A holds %d links, want it trimmed to %d — the trim must still work, not just be harmless", len(afterA), MaxTaskSessions)
	}
}

// TestPGLinkSessionCoalescesLastSeen pins the write-suppression that keeps
// GET /api/tasks/{id} from producing one dead tuple per read.
//
// 🔴 A "DO UPDATE" THAT ASSIGNS THE SAME VALUES STILL WRITES A ROW VERSION. That is
// why the store guards the update with a WHERE rather than just computing a
// coalesced value: only a genuinely conditional update avoids the dead tuple. The
// observable consequence — and what this asserts — is that a rapid repeat touch
// leaves last_seen_at EXACTLY where it was, while a role upgrade or new context
// moves it immediately regardless of the window.
func TestPGLinkSessionCoalescesLastSeen(t *testing.T) {
	ctx, _, s := threadStore(t)
	task := seedTask(t, ctx, s, Note{Body: "coalesced last_seen"})
	const sid = "coalesce-session"

	first, _, err := s.LinkSession(ctx, task.ID, SessionLink{SessionID: sid, Role: RoleRead}, false)
	if err != nil {
		t.Fatalf("first link: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	// Same role, no new context, well inside lastSeenCoalesce ⇒ NO write.
	second, changed, err := s.LinkSession(ctx, task.ID, SessionLink{SessionID: sid, Role: RoleRead}, false)
	if err != nil {
		t.Fatalf("coalesced link: %v", err)
	}
	if changed {
		t.Fatalf("a coalesced touch reported changed=true")
	}
	if !second.LastSeenAt.Equal(first.LastSeenAt) {
		t.Fatalf("last_seen_at moved (%s → %s) on a repeat touch inside the %s coalesce window; GET is a safe method and must not write on every read",
			first.LastSeenAt, second.LastSeenAt, lastSeenCoalesce)
	}
	// The row is otherwise intact — coalescing must not cost the caller its data.
	if second.SessionID != sid || second.Role != RoleRead || !second.FirstSeenAt.Equal(first.FirstSeenAt) {
		t.Fatalf("the coalesced read returned a different row: %+v vs %+v", second, first)
	}

	// A ROLE UPGRADE writes immediately, window or not.
	upgraded, changed, err := s.LinkSession(ctx, task.ID, SessionLink{SessionID: sid, Role: RoleWorked}, false)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if !changed || upgraded.Role != RoleWorked {
		t.Fatalf("the role upgrade was coalesced away: %+v changed=%v", upgraded, changed)
	}
	if !upgraded.LastSeenAt.After(first.LastSeenAt) {
		t.Fatalf("a role upgrade did not move last_seen_at (%s → %s); a real change must never be deferred",
			first.LastSeenAt, upgraded.LastSeenAt)
	}

	// NEW CONTEXT also writes immediately — otherwise the denormalised columns the
	// whole no-FK design rests on could sit stale for the whole window.
	//
	// 🔴 AND IT MUST REPORT changed=false. This is the one case that reaches the
	// `changed` computation on a WRITE whose role did not move: a context-only
	// update. Without it the mutation `changed := true` SURVIVES the whole suite —
	// measured — because every other same-role repeat touch is coalesced away
	// before the computation is reached, so the line is dead code for those tests.
	// A spurious `changed` means an SSE nudge to every open tab on every context
	// refresh.
	ctxed, changed, err := s.LinkSession(ctx, task.ID, SessionLink{SessionID: sid, Role: RoleWorked, Project: "orbit"}, false)
	if err != nil {
		t.Fatalf("context link: %v", err)
	}
	if ctxed.Project != "orbit" {
		t.Fatalf("new context was coalesced away: %+v", ctxed)
	}
	if changed {
		t.Fatalf("a context-only update (role unchanged) reported changed=true; callers broadcast on that, so every context refresh would nudge every open tab")
	}
}

// TestPGTaskSessionsCascadeOnNoteDelete: the FK to notes(id) IS a cascade, so a
// HARD purge takes the links with it. Invariant guard, and the thing that keeps
// fakeNotes.Delete honest (it deletes f.sessions for the same reason).
func TestPGTaskSessionsCascadeOnNoteDelete(t *testing.T) {
	ctx, pool, s := threadStore(t)
	n, err := s.Create(ctx, Note{Body: "cascade fixture"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := s.LinkSession(ctx, n.ID, SessionLink{SessionID: "cascade-session", Role: RoleRead}, false); err != nil {
		t.Fatalf("link: %v", err)
	}
	// Positive control: the row is really there before the delete, so a zero
	// afterwards cannot be "the insert never happened".
	var before int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_sessions WHERE note_id=$1`, n.ID).Scan(&before); err != nil {
		t.Fatalf("count before: %v", err)
	}
	if before != 1 {
		t.Fatalf("count before delete = %d, want 1 — the rest of this test would pass vacuously", before)
	}
	if err := s.Delete(ctx, n.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var after int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_sessions WHERE note_id=$1`, n.ID).Scan(&after); err != nil {
		t.Fatalf("count after: %v", err)
	}
	if after != 0 {
		t.Fatalf("%d link row(s) survived a HARD note delete — the notes(id) FK must cascade", after)
	}
}

// ---------------------------------------------------------------------------
// 🔴 A SECOND TEST WAS DELETED AT THE CARVE, AND THIS IS THE RECORD OF IT.
//
// It exercised a one-off DATA BACKFILL: the statement that gave every task
// predating the thread table a `created` link derived from its
// source_session_id. It read that statement OUT OF THE MIGRATION FILE rather
// than copying it, so the migration itself was the unit under test.
//
// THAT FILE DOES NOT EXIST HERE. This module's schema is a single statement of
// its end state, not a replay of the sequence that produced it, so there is no
// backfill migration to read and no legacy rows for one to fix — existing rows
// arrive by data-only dump, already backfilled by whoever ran it upstream. A
// ported version would have had to carry a COPY of the statement, which is
// precisely the arrangement the original's own comment rejected: a test about
// the copy, green while the real thing was deleted.
//
// ⚠ WHAT THIS LOSES, STATED RATHER THAN GLOSSED: if muster ever ships its own
// backfill migration, nothing here guards it. The guard to write then is the
// original's shape — read the statement from the migration file, run it twice,
// assert idempotence and that first_seen_at is the task's created_at rather
// than now().
// ---------------------------------------------------------------------------

// TestPGSessionsAreEmbeddedOnGetAndList pins that a task's thread rides along on
// the FULL reads exactly as its comments do — that is what makes the deliberate
// absence of a GET /api/tasks/{id}/sessions sub-route workable.
func TestPGSessionsAreEmbeddedOnGetAndList(t *testing.T) {
	ctx, _, s := threadStore(t)
	task := seedTask(t, ctx, s, Note{Body: "embedded thread " + time.Now().Format(time.RFC3339Nano)})
	if _, _, err := s.LinkSession(ctx, task.ID, SessionLink{SessionID: "embed-session", Role: RoleWorked}, false); err != nil {
		t.Fatalf("link: %v", err)
	}

	got, err := s.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Sessions) != 1 || got.Sessions[0].SessionID != "embed-session" {
		t.Fatalf("Get did not embed the thread: %+v", got.Sessions)
	}

	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found bool
	for _, n := range list {
		if n.ID != task.ID {
			continue
		}
		found = true
		if len(n.Sessions) != 1 || n.Sessions[0].SessionID != "embed-session" {
			t.Fatalf("List did not embed the thread for task %d: %+v", task.ID, n.Sessions)
		}
	}
	if !found {
		t.Fatalf("task %d absent from List", task.ID)
	}
}

// TestPGTasksForSessionIsTheReverseLookup: most-recently-touched first, and
// DISMISSED tasks excluded (a dismissed task answers 404 by id, so listing it would hand
// a caller an id it cannot fetch).
func TestPGTasksForSessionIsTheReverseLookup(t *testing.T) {
	ctx, _, s := threadStore(t)
	sid := "reverse-session-" + time.Now().Format("150405.000000000")

	older := seedTask(t, ctx, s, Note{Title: "older", Body: "older task"})
	newer := seedTask(t, ctx, s, Note{Title: "newer", Body: "newer task"})
	dismissed := seedTask(t, ctx, s, Note{Title: "dismissed", Body: "dismissed task"})

	for _, id := range []int64{older.ID, dismissed.ID} {
		if _, _, err := s.LinkSession(ctx, id, SessionLink{SessionID: sid, Role: RoleWorked}, false); err != nil {
			t.Fatalf("link %d: %v", id, err)
		}
	}
	time.Sleep(15 * time.Millisecond)
	if _, _, err := s.LinkSession(ctx, newer.ID, SessionLink{SessionID: sid, Role: RoleRead}, false); err != nil {
		t.Fatalf("link newer: %v", err)
	}
	if err := s.SoftDelete(ctx, dismissed.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	got, err := s.TasksForSession(ctx, sid)
	if err != nil {
		t.Fatalf("tasks for session: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("reverse lookup returned %d tasks, want 2 (the dismissed one must be excluded): %+v", len(got), got)
	}
	if got[0].NoteID != newer.ID {
		t.Fatalf("reverse lookup order = %d then %d, want most-recently-touched (%d) first", got[0].NoteID, got[1].NoteID, newer.ID)
	}
	if got[0].Role != RoleRead || got[1].Role != RoleWorked {
		t.Fatalf("roles = %q, %q; want read then worked", got[0].Role, got[1].Role)
	}
	if got[0].Title != "newer" || got[0].Status != StatusOpen {
		t.Fatalf("reverse row lost the task's identity/lifecycle: %+v", got[0])
	}
	for _, tl := range got {
		if tl.NoteID == dismissed.ID {
			t.Fatalf("the DISMISSED task appears in the reverse lookup: %+v", tl)
		}
	}

	// An unknown session is an empty slice, not an error.
	none, err := s.TasksForSession(ctx, "no-such-session-"+sid)
	if err != nil || len(none) != 0 {
		t.Fatalf("unknown session = %d tasks, err=%v; want an empty slice and no error", len(none), err)
	}
}

// TestPGLinkSessionRejectsBadInput: the store is a boundary, so an invalid role or
// an empty session id is refused BEFORE the statement runs — the CHECK constraint
// would catch the role but as an opaque driver error, and an empty session id
// would happily become a real row keyed on "".
func TestPGLinkSessionRejectsBadInput(t *testing.T) {
	ctx, _, s := threadStore(t)
	task := seedTask(t, ctx, s, Note{Body: "bad input"})
	if _, _, err := s.LinkSession(ctx, task.ID, SessionLink{SessionID: "x", Role: "owner"}, false); err == nil {
		t.Fatalf("an invalid role was accepted")
	}
	if _, _, err := s.LinkSession(ctx, task.ID, SessionLink{SessionID: "", Role: RoleRead}, false); err == nil {
		t.Fatalf("an empty session id was accepted")
	}
}

// TestPGLinkSessionDoesNotBlankDenormalisedContext: a later touch that knows
// nothing about the session's project must not erase what an earlier one recorded
// — which matters precisely because these columns are the thread's only memory
// once the external transcript record is reaped.
func TestPGLinkSessionDoesNotBlankDenormalisedContext(t *testing.T) {
	ctx, _, s := threadStore(t)
	task := seedTask(t, ctx, s, Note{Body: "context preservation"})
	const sid = "context-session"
	if _, _, err := s.LinkSession(ctx, task.ID, SessionLink{
		SessionID: sid, Role: RoleRead, Project: "orbit", Cwd: "/w/c", Host: "builder-1",
	}, false); err != nil {
		t.Fatalf("first link: %v", err)
	}
	got, _, err := s.LinkSession(ctx, task.ID, SessionLink{SessionID: sid, Role: RoleWorked}, false)
	if err != nil {
		t.Fatalf("second link: %v", err)
	}
	if got.Project != "orbit" || got.Cwd != "/w/c" || got.Host != "builder-1" {
		t.Fatalf("a context-less touch blanked the stored context: %+v", got)
	}
}

// TestSessionColsAndScanTargetsAgree pins the projection/scan contract the file's
// own comments call out. It is a pure test (no database), so it runs everywhere.
//
// 🔴 THE FAILURE IT CATCHES IS SILENT. sessionCols and scanSessionLinkInto are two
// ordered lists that must line up positionally; adding a column to one and not the
// other is a runtime scan error at best, and — for two adjacent columns of the SAME
// Go type, which is exactly what detail_available and detail_seen are — a clean
// compile, a clean scan, and two swapped booleans. Nothing else in the suite would
// notice, because most fixtures set both to the same value.
func TestSessionColsAndScanTargetsAgree(t *testing.T) {
	cols := strings.Split(sessionCols, ",")
	var l SessionLink
	dest := scanSessionLinkInto(&l)
	if len(cols) != len(dest) {
		t.Fatalf("sessionCols projects %d columns but scanSessionLinkInto offers %d targets:\n  %s",
			len(cols), len(dest), sessionCols)
	}
	// The last column is the one that can be transposed without any error at all, so
	// name it explicitly rather than trusting the count.
	if got := strings.TrimSpace(cols[len(cols)-1]); got != "ts.detail_seen" {
		t.Fatalf("the last projected column is %q, want ts.detail_seen — scanSessionLinkInto ends with &l.DetailSeen", got)
	}
	// 🔴 AND EVERY COLUMN MUST COME OFF task_sessions ALONE. This projection used to
	// end with `(cs.session_id IS NOT NULL)` off a LEFT JOIN onto the session table,
	// which is the cross-service dependency the carve removed. Re-adding
	// any column from another alias re-opens it, and would do so silently: the read
	// would work, the UI would look right, and the package would be un-carveable.
	for i, c := range cols {
		c = strings.TrimSpace(c)
		if !strings.HasPrefix(c, "ts.") {
			t.Fatalf("sessionCols[%d] = %q — every column must be projected from the task_sessions alias `ts`. A column from anywhere else means this package reads a table it may not: %s",
				i, c, sessionCols)
		}
	}
	if strings.Contains(sessionFrom, "JOIN") {
		t.Fatalf("sessionFrom = %q, want a bare `FROM task_sessions ts`. The JOIN onto the other service's session table is the dependency the carve deleted", sessionFrom)
	}
}

// TestPGLinkSessionDetailSeenIsMonotonicAndNotCoalesced is the STORED half of the
// three-state transcript row.
//
// 🔴 WHY THE COLUMN EXISTS. `DetailAvailable` is ONE live bit, asked to answer two
// questions: "the transcript was recorded and has since been reaped" and "no
// transcript was ever recorded". It cannot, so a reader resolved every absence to
// the first and printed "transcript expired" over sessions minutes old. On the
// deployment this package came from the producing hook was dead for months and the
// overwhelming majority of links had no transcript record at all — so the false
// sentence was the COMMON case, and it disguised the outage as ordinary retention.
// `detail_seen` is the stored bit that separates them, and it is only meaningful
// if it is MONOTONIC: once a transcript has been seen, that stays true forever,
// including after the record itself is gone.
//
// 🔴 THE OBSERVATION ARRIVES AS AN ARGUMENT, NOT AS A SQL PROBE. The record lives
// in another service; this package is not allowed to read it. So what this test
// can check is the four properties of how the store HANDLES the answer it is
// given — not whether the answer is correct, which is the caller's to get right.
// Each call below takes its bool from the double's Observe rather than a literal,
// which is what keeps the negative controls non-vacuous.
//
// Four properties, in one test because they are one lifecycle:
//
//  1. it starts FALSE when no transcript record exists (the control — without this
//     the flip below would be indistinguishable from a column that is always true);
//  2. it flips to TRUE once a record appears;
//  3. 🔴 the flip is NOT suppressed by the 5-minute lastSeenCoalesce window. This is
//     the half that is easy to get wrong and impossible to see: omit the detail_seen
//     clause from the upsert's `DO UPDATE … WHERE` and the transition is silently
//     dropped, because the touch that carries the new observation changes nothing
//     ELSE about the link. The read-back path then returns the OLD value and the row
//     keeps rendering "no transcript recorded" — for five minutes, and in practice
//     forever, since a task is rarely touched again.
//  4. it NEVER flips back, including on a touch AFTER the record is gone that
//     genuinely writes (a role upgrade, new context). A plain
//     `detail_seen = EXCLUDED.detail_seen` passes 1-3 and fails only here.
func TestPGLinkSessionDetailSeenIsMonotonicAndNotCoalesced(t *testing.T) {
	ctx, _, s := threadStore(t)
	ext := newExternalSessions()
	task := seedTask(t, ctx, s, Note{Body: "detail_seen lifecycle"})
	const sid = "detail-seen-session"

	// (1) No transcript record yet — the producing hook has not reported this
	// session. Positive control on the observation itself: it must read FALSE
	// here, or the flip in (2) is not a flip.
	if ext.Observe(t, sid) {
		t.Fatalf("the fixture already holds a session row for %q, so the false→true transition below would prove nothing", sid)
	}
	first, _, err := s.LinkSession(ctx, task.ID, SessionLink{SessionID: sid, Role: RoleRead},
		ext.Observe(t, sid))
	if err != nil {
		t.Fatalf("first link: %v", err)
	}
	if first.DetailSeen {
		t.Fatalf("a link written with no session row reports detailSeen=true — the whole point is that a never-recorded transcript is distinguishable")
	}
	if links, err := s.SessionsForTask(ctx, task.ID); err != nil || len(links) != 1 || links[0].DetailSeen {
		t.Fatalf("the READ path disagrees with the write path about detail_seen: %+v (err=%v)", links, err)
	}

	// (2)+(3) The producer reports the session. The very next touch is a REPEAT of
	// the same role with no new context, WELL inside lastSeenCoalesce — the exact
	// shape the coalesce clause exists to suppress.
	ext.Record(sid)
	if !ext.Observe(t, sid) {
		t.Fatalf("the double did not record %q, so the flip below would prove nothing", sid)
	}
	if time.Since(first.LastSeenAt) >= lastSeenCoalesce {
		t.Fatalf("the fixture drifted outside the %s coalesce window, so property (3) would pass vacuously", lastSeenCoalesce)
	}
	flipped, _, err := s.LinkSession(ctx, task.ID, SessionLink{SessionID: sid, Role: RoleRead},
		ext.Observe(t, sid))
	if err != nil {
		t.Fatalf("observing touch: %v", err)
	}
	if !flipped.DetailSeen {
		t.Fatalf("detail_seen did not flip to true inside the %s coalesce window — the transition was written off as a no-op touch, which is the one case the column exists to record",
			lastSeenCoalesce)
	}
	// 🔴 AND THE STORE STILL DID NOT SET DetailAvailable, even on the one call where
	// it holds a true observation in its hand. It is not this package's field any
	// more; api.withSessionLiveness owns it. A store that "helpfully" filled it here
	// would be right in-process and wrong the moment notes is carved out.
	if flipped.DetailAvailable {
		t.Fatalf("the store set DetailAvailable=true; this package must always leave it at the zero value — the API layer owns that field")
	}
	// PERSISTED, not merely returned: a value computed in the RETURNING projection
	// and never stored would satisfy the assertion above and be gone on the next read.
	after, err := s.SessionsForTask(ctx, task.ID)
	if err != nil || len(after) != 1 || !after[0].DetailSeen {
		t.Fatalf("detail_seen was not PERSISTED by the observing touch: %+v (err=%v)", after, err)
	}

	// (4) The other service's retention pass takes the transcript record.
	// detail_seen must survive it — and must survive a touch that genuinely
	// WRITES afterwards.
	ext.Sweep()
	// A ROLE UPGRADE is outside the coalesce window by construction, so this touch
	// definitely executes the DO UPDATE SET list — which is where a plain assignment
	// would reset detail_seen to the now-false EXISTS probe.
	if ext.Observe(t, sid) {
		t.Fatalf("negative control failed: the observation still reads true after the sweep, so the post-sweep writes below would carry a true and prove nothing")
	}
	upgraded, _, err := s.LinkSession(ctx, task.ID, SessionLink{SessionID: sid, Role: RoleWorked},
		ext.Observe(t, sid))
	if err != nil {
		t.Fatalf("post-sweep upgrade: %v", err)
	}
	if !upgraded.DetailSeen {
		t.Fatalf("detail_seen went BACK to false on a post-sweep write — the link now claims its transcript was never recorded, when in fact it expired. The merge must be `task_sessions.detail_seen OR EXCLUDED.detail_seen`, not an assignment")
	}
	// And again on a NEW-CONTEXT write, the other route through the DO UPDATE list.
	ctxed, _, err := s.LinkSession(ctx, task.ID, SessionLink{SessionID: sid, Role: RoleWorked, Host: "builder-1"},
		ext.Observe(t, sid))
	if err != nil {
		t.Fatalf("post-sweep context touch: %v", err)
	}
	if !ctxed.DetailSeen {
		t.Fatalf("detail_seen went back to false on a context-only write: %+v", ctxed)
	}
	final, err := s.SessionsForTask(ctx, task.ID)
	if err != nil || len(final) != 1 || !final[0].DetailSeen || final[0].DetailAvailable {
		t.Fatalf("final state is wrong: %+v (err=%v); want detailSeen=true and detailAvailable=false — detail_seen is the STORED half of the REAPED state and detailAvailable is never this package's to set", final, err)
	}
}
