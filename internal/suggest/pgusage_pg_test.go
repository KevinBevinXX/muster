package suggest

import (
	"context"
	"io"
	"log"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

func pgUsage(t *testing.T) *PGUsage {
	t.Helper()
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(dbtest.AsOperator(ctx), pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member'), (3, 'member') ON CONFLICT (id) DO NOTHING`)
	dbtest.OwnerExec(t, dsn, `TRUNCATE suggest_usage`)
	return NewPGUsage(pool)
}

// showN has e serve and report n distinct sets (subjects prefix<i>) to owner, filling every
// fillEvery-th (0: none), all as owner.
func showN(t *testing.T, e *Engine, owner int64, prefix string, n, fillEvery int) {
	t.Helper()
	ctx := asUser(owner)
	for i := 0; i < n; i++ {
		sub := prefix + strconv.Itoa(i)
		set, out, err := e.Suggest(ctx, owner, sub, turnInput(t, turn1+" ("+prefix+" round "+strconv.Itoa(i)+")"), int64(1000+i))
		if err != nil || out != OutcomeGenerated {
			t.Fatalf("set %d: %v %v", i, out, err)
		}
		if !e.Record(ctx, owner, sub, set.Fingerprint, EventShown, 0) {
			t.Fatalf("shown %d did not count", i)
		}
		if fillEvery > 0 && i%fillEvery == 0 {
			e.Record(ctx, owner, sub, set.Fingerprint, EventFilled, 0)
			e.Record(ctx, owner, sub, set.Fingerprint, EventSent, 3)
		}
	}
}

// TestTheCountsMoveOnEventsAndAPauseSurvivesARestart: events move the
// persisted counts (shown, filled, sent, the edit-distance sum); a user paused
// under 5% after 200 shown is STILL paused by a new engine on the same
// database (a restart, a deploy), with nothing in its memory; turning them
// back on (Resume) clears it for that engine and the next.
func TestTheCountsMoveOnEventsAndAPauseSurvivesARestart(t *testing.T) {
	u := pgUsage(t)
	e := &Engine{Gen: &countingGen{}, Usage: u, PerOwner: 10_000, Global: 10_000}
	showN(t, e, 2, "a", 3, 1)
	if got, err := u.Get(asUser(2), 2); err != nil || got != (Tally{Shown: 3, Filled: 3, Sent: 3, EditSum: 9}) {
		t.Fatalf("after 3 shown and filled: %+v %v", got, err)
	}
	dbtest.OwnerExec(t, dbtest.DSN(t), `TRUNCATE suggest_usage`)
	showN(t, e, 2, "b", AutoPauseMinShown, 0)
	if o := e.Enabled(asUser(2), 2); o != OutcomePaused {
		t.Fatalf("not paused: %q", o)
	}
	restarted := &Engine{Gen: &countingGen{}, Usage: u}
	if o := restarted.Enabled(asUser(2), 2); o != OutcomePaused {
		t.Fatalf("the pause did not survive a restart: %q", o)
	}
	if st, _ := restarted.Stats(asUser(2), 2); !st.Paused || st.Shown != AutoPauseMinShown {
		t.Fatalf("restarted stats %+v", st)
	}
	// Another user is not paused by user 2's counts.
	if o := restarted.Enabled(asUser(3), 3); o != "" {
		t.Fatalf("user 3: %q", o)
	}
	restarted.Resume(asUser(2), 2)
	if o := restarted.Enabled(asUser(2), 2); o != "" {
		t.Fatalf("resumed: %q", o)
	}
	if o := (&Engine{Gen: &countingGen{}, Usage: u}).Enabled(asUser(2), 2); o != "" {
		t.Fatalf("the resume did not survive a restart: %q", o)
	}
}

// TestTheCountsArePerUser: row-level security — user 3 neither reads, adds
// to, pauses nor resets user 2's counts (each refused as not the owner, and
// the row is unchanged), and reading as nobody is refused.
func TestTheCountsArePerUser(t *testing.T) {
	u := pgUsage(t)
	if _, err := u.Add(asUser(2), 2, 5, 1, 1, 4); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"read":   func() error { _, err := u.Get(asUser(3), 2); return err }(),
		"add":    func() error { _, err := u.Add(asUser(3), 2, 1, 1, 1, 1); return err }(),
		"pause":  u.SetPaused(asUser(3), 2, true),
		"reset":  u.Reset(asUser(3), 2),
		"nobody": func() error { _, err := u.Get(context.Background(), 2); return err }(),
	} {
		if err == nil {
			t.Fatalf("%s: user 3 reached user 2's counts", name)
		}
	}
	// Past the Go check, the database itself: user 3's principal naming user
	// 2's row through SQL sees nothing and writes nothing.
	var n int
	if err := u.pool.QueryRow(asUser(3), `SELECT count(*) FROM suggest_usage WHERE owner_id = 2`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("RLS: user 3 sees %d of user 2's rows (%v)", n, err)
	}
	if tag, err := u.pool.Exec(asUser(3), `UPDATE suggest_usage SET shown = 999 WHERE owner_id = 2`); err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("RLS: user 3 updated user 2's row (%v, %v)", tag, err)
	}
	if _, err := u.pool.Exec(asUser(3), `INSERT INTO suggest_usage (owner_id, shown) VALUES (2, 1) ON CONFLICT (owner_id) DO UPDATE SET shown = 1`); err == nil {
		t.Fatal("RLS: user 3 wrote a row for user 2")
	}
	// Positive control: as user 2 the same read sees the row.
	if err := u.pool.QueryRow(asUser(2), `SELECT count(*) FROM suggest_usage WHERE owner_id = 2`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("control: user 2 sees %d (%v)", n, err)
	}
	if got, _ := u.Get(asUser(2), 2); got != (Tally{Shown: 5, Filled: 1, Sent: 1, EditSum: 4}) {
		t.Fatalf("user 2's counts moved: %+v", got)
	}
	if got, _ := u.Get(asUser(3), 3); got != (Tally{}) {
		t.Fatalf("user 3 has %+v", got)
	}
}
