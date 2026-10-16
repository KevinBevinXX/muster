package recents

import (
	"context"
	"fmt"
	"io"
	"log"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/pins"
)

// Users in this package's scratch database: 1 is the seeded operator; 2 and 3
// are members.
func pgStore(t *testing.T) (*PGStore, *pgxpool.Pool) {
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
	dbtest.OwnerExec(t, dsn, `TRUNCATE recents`)
	dbtest.OwnerExec(t, dsn, `DELETE FROM agents WHERE name LIKE 'recent-%'`)
	return NewPG(pool), pool
}

func as(uid int64) context.Context {
	role := authz.RoleMember
	if uid == authz.OperatorUserID {
		role = authz.RoleAdmin
	}
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: role, Via: authz.ViaSession})
}

func mustAgent(t *testing.T, pool *pgxpool.Pool, uid int64, name string) agents.Agent {
	t.Helper()
	a, err := agents.NewPG(pool).Create(as(uid), agents.Agent{Name: name, Namespace: "devpod-" + name, DisplayName: name,
		Status: agents.StatusRunning, HooksToken: "hooks-" + name})
	if err != nil {
		t.Fatalf("agent %s: %v", name, err)
	}
	return a
}

// order is a list as "a<id>" / "m<session><window>" labels, newest first.
func order(t *testing.T, s *PGStore, uid int64) []string {
	t.Helper()
	es, err := s.List(as(uid))
	if err != nil {
		t.Fatalf("list as %d: %v", uid, err)
	}
	out := []string{}
	for _, e := range es {
		if e.IsMirror() {
			out = append(out, "m"+e.Mirror.Session+e.Mirror.Window)
		} else {
			out = append(out, fmt.Sprintf("a%d", e.AgentID))
		}
	}
	return out
}

func eq(a, b []string) bool { return fmt.Sprint(a) == fmt.Sprint(b) }

func count(t *testing.T, pool *pgxpool.Pool, where string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM recents WHERE `+where).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

var win = pins.MirrorKey{Host: "laptop", Session: "scratch2", Window: "@7"}

// TestAVisitUpsertsAndMovesToTheFront: a visit to a subject already recorded
// is the SAME row with a newer visited_at — moved to the front, not added.
// Red before: there was no recents store.
func TestAVisitUpsertsAndMovesToTheFront(t *testing.T) {
	s, pool := pgStore(t)
	a := mustAgent(t, pool, 2, "recent-a")
	b := mustAgent(t, pool, 2, "recent-b")
	for _, step := range []func() error{
		func() error { return s.VisitAgent(as(2), a.ID) },
		func() error { return s.VisitMirror(as(2), win) },
		func() error { return s.VisitAgent(as(2), b.ID) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	ida, idb := fmt.Sprintf("a%d", a.ID), fmt.Sprintf("a%d", b.ID)
	if got, want := order(t, s, 2), []string{idb, "mscratch2@7", ida}; !eq(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	// Revisit the oldest two: each moves to the front, and no row is added.
	if err := s.VisitAgent(as(2), a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.VisitMirror(as(2), win); err != nil {
		t.Fatal(err)
	}
	if got, want := order(t, s, 2), []string{"mscratch2@7", ida, idb}; !eq(got, want) {
		t.Errorf("after revisiting, order = %v, want %v", got, want)
	}
	if n := count(t, pool, "owner_id = 2"); n != 3 {
		t.Errorf("%d rows after five visits to three subjects, want 3", n)
	}
	// A per-session entry (window "") is a different address from a window.
	sess := pins.MirrorKey{Host: win.Host, Session: win.Session}
	if err := s.VisitMirror(as(2), sess); err != nil {
		t.Fatal(err)
	}
	if got := order(t, s, 2); got[0] != "mscratch2" || len(got) != 4 {
		t.Errorf("a per-session visit = %v, want it a fourth entry, first", got)
	}
}

// TestARecentIsInvisibleToAnotherOwner: B reads none of A's rows, B's visits
// never touch them (not even its prune), and B cannot record a visit to A's
// agent. The positive control is A's own list, before and after.
func TestARecentIsInvisibleToAnotherOwner(t *testing.T) {
	s, pool := pgStore(t)
	a := mustAgent(t, pool, 2, "recent-iso")
	if err := s.VisitAgent(as(2), a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.VisitMirror(as(2), win); err != nil {
		t.Fatal(err)
	}
	want := []string{"mscratch2@7", fmt.Sprintf("a%d", a.ID)}
	if got := order(t, s, 2); !eq(got, want) {
		t.Fatalf("A's own list = %v, want %v (the positive control)", got, want)
	}
	if got := order(t, s, 3); len(got) != 0 {
		t.Fatalf("B sees A's recents: %v", got)
	}
	if err := s.VisitAgent(as(3), a.ID); err == nil {
		t.Fatal("B recorded a visit to A's agent")
	}
	// B visits the same window, and more than the bound of others: its prune
	// runs every time and must reach none of A's rows.
	for i := 0; i < MaxPerOwner+3; i++ {
		if err := s.VisitMirror(as(3), pins.MirrorKey{Host: "tower", Session: "b", Window: fmt.Sprintf("@%d", i+1)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.VisitMirror(as(3), win); err != nil {
		t.Fatal(err)
	}
	if got := order(t, s, 2); !eq(got, want) {
		t.Errorf("B's visits changed A's list: %v, want %v", got, want)
	}
	if n := count(t, pool, "owner_id = 3"); n != MaxPerOwner {
		t.Errorf("B owns %d rows, want the bound %d", n, MaxPerOwner)
	}
	var n int
	if err := pool.QueryRow(as(3), `SELECT count(*) FROM recents WHERE owner_id = 2`).Scan(&n); err != nil || n != 0 {
		t.Errorf("B's raw read sees %d of A's rows (%v), want 0", n, err)
	}
}

// TestTheOldestPastTheBoundArePruned: the bound is MaxPerOwner rows; each visit
// prunes the OLDEST past it, never the one just recorded. Fixture: the bound
// plus a margin that is not a multiple of anything — 7.
func TestTheOldestPastTheBoundArePruned(t *testing.T) {
	s, pool := pgStore(t)
	total := MaxPerOwner + 7
	for i := 1; i <= total; i++ {
		if err := s.VisitMirror(as(2), pins.MirrorKey{Host: "tower", Session: "w", Window: fmt.Sprintf("@%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(t, pool, "owner_id = 2"); n != MaxPerOwner {
		t.Fatalf("%d rows after %d visits, want the bound %d", n, total, MaxPerOwner)
	}
	got := order(t, s, 2)
	if got[0] != fmt.Sprintf("mw@%d", total) {
		t.Errorf("the newest visit is not first: %v", got[:3])
	}
	if last := got[len(got)-1]; last != fmt.Sprintf("mw@%d", total-MaxPerOwner+1) {
		t.Errorf("the oldest kept is %s, want mw@%d", last, total-MaxPerOwner+1)
	}
	if n := count(t, pool, "owner_id = 2 AND mirror_window IN ('@1','@7')"); n != 0 {
		t.Errorf("the oldest rows survived the prune")
	}
	// Revisiting the OLDEST kept entry moves it up rather than pruning it.
	oldest := pins.MirrorKey{Host: "tower", Session: "w", Window: fmt.Sprintf("@%d", total-MaxPerOwner+1)}
	if err := s.VisitMirror(as(2), oldest); err != nil {
		t.Fatal(err)
	}
	if got := order(t, s, 2); got[0] != "mw"+oldest.Window || len(got) != MaxPerOwner {
		t.Errorf("revisiting the oldest: first %s, %d rows", got[0], len(got))
	}
}

// TestARecentDiesWithItsAgent: the FK cascade. The mirror row beside it stays.
func TestARecentDiesWithItsAgent(t *testing.T) {
	s, pool := pgStore(t)
	gone := mustAgent(t, pool, 2, "recent-gone")
	if err := s.VisitAgent(as(2), gone.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.VisitMirror(as(2), win); err != nil {
		t.Fatal(err)
	}
	if err := agents.NewPG(pool).Delete(as(2), gone.ID); err != nil {
		t.Fatal(err)
	}
	if got := order(t, s, 2); !eq(got, []string{"mscratch2@7"}) {
		t.Errorf("after deleting the agent: %v, want only the window", got)
	}
}

// TestAVisitToAnAbsentAgentOrABadAddressIsRefused: no row for either.
func TestAVisitToAnAbsentAgentOrABadAddressIsRefused(t *testing.T) {
	s, pool := pgStore(t)
	if err := s.VisitAgent(as(2), 987654321); err == nil {
		t.Error("a visit to an absent agent was recorded")
	}
	for _, k := range []pins.MirrorKey{
		{Host: "", Session: "s"},
		{Host: "tower", Session: ""},
		{Host: "tower", Session: "s", Window: "7"},
	} {
		if err := s.VisitMirror(as(2), k); err == nil {
			t.Errorf("a bad address %+v was recorded", k)
		}
	}
	if n := count(t, pool, "owner_id = 2"); n != 0 {
		t.Errorf("%d rows after refused visits, want 0", n)
	}
}
