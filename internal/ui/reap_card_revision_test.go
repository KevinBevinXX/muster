package ui

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/notes"
)

// TestAReapedTaskCardKeepsItsRevision is a SEAM test, and it is here rather than
// in internal/notes or internal/api because neither of those can make the claim.
//
// The two halves are each already covered in isolation — internal/notes proves
// FlagIdle leaves `updated_at` alone, internal/ui proves noteCard renders
// `updated_at` as data-card-rev — and "verified in isolation" is exactly the shape
// that ships a broken seam. What no component test builds is the COMBINED state:
// a real Postgres row, actually reaped, actually rendered. That is what this does.
//
// 🔴 WHY THE REVISION MUST NOT MOVE, stated as the relationship rather than as
// two facts. data-card-rev is the card's version, and resyncScript refuses a
// morph:outerHTML whose incoming rev is STRICTLY OLDER than the one on screen.
// internal/ui/notes.go's own argument for that guard is that "a write that
// legitimately does not move updated_at ... is never dropped, because ties always
// apply". The reaper's annotation is now precisely such a write, so this test is
// what turns that sentence from an assertion about a hypothetical into one about
// a real writer. If it ever fails, either FlagIdle started writing updated_at or
// the guard stopped applying ties, and BOTH are regressions.
//
// PG-gated: the property under test is a property of the SQL. A fake would only
// restate what the fake was written to do.
func TestAReapedTaskCardKeepsItsRevision(t *testing.T) {
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
	store := notes.NewPG(pool)

	n, err := store.Create(ctx, notes.Note{Directory: "/work/muster", Body: "an abandoned task"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer store.Delete(ctx, n.ID)

	before, err := store.Get(ctx, n.ID)
	if err != nil {
		t.Fatalf("get before: %v", err)
	}
	id := "task-" + strconv.FormatInt(n.ID, 10)
	revBefore, ok := cardRevOf(t, renderString(t, noteCard(TaskCardView{Note: before})), id)
	if !ok {
		t.Fatalf("the pre-reap card carries no data-card-rev on #%s — the seam cannot be measured", id)
	}

	// 🔴 A ZERO/negative rev WEDGES the card permanently: the guard refuses every
	// swap strictly older than what is on screen, and time.Time{}.UnixNano() is
	// -6795364578871345152. Nothing on the page recovers except a full reload. So
	// assert the rev is a sane positive number BEFORE using it as a baseline —
	// otherwise "the rev did not change" could be true of two equally broken cards.
	nsBefore, err := strconv.ParseInt(revBefore, 10, 64)
	if err != nil {
		t.Fatalf("data-card-rev %q is not an integer: %v", revBefore, err)
	}
	if nsBefore <= 0 {
		t.Fatalf("data-card-rev is %d (non-positive) — that value permanently wedges the card: "+
			"every later swap is refused and only a page reload recovers", nsBefore)
	}

	// Sleep past the timestamp resolution so a bump would be visible in the rev.
	time.Sleep(10 * time.Millisecond)

	flagged, err := store.FlagIdle(ctx, n.ID, "stale", "Task idle — tagged **stale** by the retention sweep.")
	if err != nil {
		t.Fatalf("FlagIdle: %v", err)
	}
	if !flagged {
		t.Fatal("FlagIdle reported no write, so the comparison below would be a no-op")
	}

	after, err := store.Get(ctx, n.ID)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	// The reap is VISIBLE on the card — without this the revision assertion could
	// pass simply because nothing was rendered differently, or at all.
	if !notes.HasAllTags(after.Tags, []string{"stale"}) {
		t.Fatalf("the task was not actually tagged, so this test is measuring nothing (tags %v)", after.Tags)
	}
	markupAfter := renderString(t, noteCard(TaskCardView{Note: after}))
	revAfter, ok := cardRevOf(t, markupAfter, id)
	if !ok {
		t.Fatalf("the post-reap card carries no data-card-rev on #%s — an ABSENT rev is the "+
			"safe state for the guard, but it means the card silently lost its version", id)
	}

	if revAfter != revBefore {
		t.Errorf("a reap MOVED the card revision: %s → %s.\n"+
			"data-card-rev is notes.updated_at in nanoseconds, so this means the reaper wrote "+
			"updated_at — the defect FlagIdle exists to remove. It also means the board's order "+
			"was rewritten by the janitor rather than by activity.", revBefore, revAfter)
	}
}
