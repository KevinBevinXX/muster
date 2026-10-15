package pins

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
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
	dbtest.OwnerExec(t, dsn, `TRUNCATE pins`)
	dbtest.OwnerExec(t, dsn, `DELETE FROM agents WHERE name LIKE 'pin-%'`)
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

func mustList(t *testing.T, s *PGStore, uid int64) Set {
	t.Helper()
	set, err := s.List(as(uid))
	if err != nil {
		t.Fatalf("list as %d: %v", uid, err)
	}
	return set
}

var win = MirrorKey{Host: "laptop", Session: "scratch2", Window: "@7"}

// TestAPinIsInvisibleToAnotherOwner: user A's pins — of an agent and of a
// mirrored window — are not in B's list, B's unpin of the same subjects leaves
// them in place, and B cannot pin A's agent. The positive control is A's own
// list, read before and after B acted.
func TestAPinIsInvisibleToAnotherOwner(t *testing.T) {
	s, pool := pgStore(t)
	a := mustAgent(t, pool, 2, "pin-a")
	if err := s.PinAgent(as(2), a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.PinMirror(as(2), win); err != nil {
		t.Fatal(err)
	}
	if got := mustList(t, s, 2); !got.Agent(a.ID) || !got.Mirror(win) {
		t.Fatalf("A's own list = %+v, want both pins (the positive control)", got)
	}
	if got := mustList(t, s, 3); len(got.Agents) != 0 || len(got.Mirrors) != 0 {
		t.Fatalf("B sees A's pins: %+v", got)
	}
	// B's deletes of the same subjects touch nothing of A's.
	if err := s.UnpinAgent(as(3), a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UnpinMirror(as(3), win); err != nil {
		t.Fatal(err)
	}
	if got := mustList(t, s, 2); !got.Agent(a.ID) || !got.Mirror(win) {
		t.Fatalf("B's unpin removed A's pins: A now has %+v", got)
	}
	// B pinning A's agent is refused as "no such agent" (the owner trigger),
	// and writes no row.
	if err := s.PinAgent(as(3), a.ID); !errors.Is(err, ErrNoSuchAgent) {
		t.Fatalf("B pinning A's agent: %v, want ErrNoSuchAgent", err)
	}
	var n int
	if err := pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM pins WHERE owner_id = 3`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("B owns %d pins (%v), want 0", n, err)
	}
	// Raw SQL through the application pool, too: B's principal reads no row.
	if err := pool.QueryRow(as(3), `SELECT count(*) FROM pins`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("B's raw read sees %d pins (%v), want 0", n, err)
	}
	if err := pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM pins`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("System sees %d pins (%v), want 2", n, err)
	}
}

// TestPinningIsIdempotent: pinning twice is one row, unpinning twice is no
// error, and an unpinned subject reads as unpinned.
func TestPinningIsIdempotent(t *testing.T) {
	s, pool := pgStore(t)
	a := mustAgent(t, pool, 2, "pin-idem")
	for i := 0; i < 2; i++ {
		if err := s.PinAgent(as(2), a.ID); err != nil {
			t.Fatalf("pin %d: %v", i, err)
		}
		if err := s.PinMirror(as(2), win); err != nil {
			t.Fatalf("pin mirror %d: %v", i, err)
		}
	}
	var n int
	if err := pool.QueryRow(as(2), `SELECT count(*) FROM pins`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("%d rows after pinning each subject twice (%v), want 2", n, err)
	}
	for i := 0; i < 2; i++ {
		if err := s.UnpinAgent(as(2), a.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.UnpinMirror(as(2), win); err != nil {
			t.Fatal(err)
		}
	}
	if got := mustList(t, s, 2); got.Agent(a.ID) || got.Mirror(win) {
		t.Fatalf("still pinned after unpin: %+v", got)
	}
}

// TestDeletingAnAgentDeletesItsPin: the FK cascade. The mirror pin beside it,
// and another agent's pin, stay.
func TestDeletingAnAgentDeletesItsPin(t *testing.T) {
	s, pool := pgStore(t)
	gone := mustAgent(t, pool, 2, "pin-gone")
	kept := mustAgent(t, pool, 2, "pin-kept")
	for _, id := range []int64{gone.ID, kept.ID} {
		if err := s.PinAgent(as(2), id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PinMirror(as(2), win); err != nil {
		t.Fatal(err)
	}
	if err := agents.NewPG(pool).Delete(as(2), gone.ID); err != nil {
		t.Fatal(err)
	}
	got := mustList(t, s, 2)
	if got.Agent(gone.ID) || !got.Agent(kept.ID) || !got.Mirror(win) {
		t.Fatalf("after deleting agent %d: %+v, want only %d and the window pinned", gone.ID, got, kept.ID)
	}
	var n int
	if err := pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM pins WHERE agent_id = $1`, gone.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d rows still name the deleted agent (%v)", n, err)
	}
}

// TestAPinOfAnAbsentAgentOrABadAddressIsRefused: no row lands for either.
func TestAPinOfAnAbsentAgentOrABadAddressIsRefused(t *testing.T) {
	s, _ := pgStore(t)
	if err := s.PinAgent(as(2), 987654321); !errors.Is(err, ErrNoSuchAgent) {
		t.Fatalf("pin of an absent agent: %v, want ErrNoSuchAgent", err)
	}
	for _, k := range []MirrorKey{
		{Host: "Laptop", Session: "s", Window: "@1"},   // not a lowercase DNS label
		{Host: "laptop", Session: "", Window: "@1"},    // no session
		{Host: "laptop", Session: "s", Window: "1"},    // an index, not an id
		{Host: "laptop", Session: "s\x01", Window: ""}, // a control character
	} {
		if err := s.PinMirror(as(2), k); err == nil {
			t.Errorf("pin of %+v was accepted", k)
		}
	}
	if got := mustList(t, s, 2); len(got.Agents)+len(got.Mirrors) != 0 {
		t.Fatalf("refused pins left rows: %+v", got)
	}
	// Control: a per-session entry (window "") is a valid address.
	if err := s.PinMirror(as(2), MirrorKey{Host: "laptop", Session: "s"}); err != nil {
		t.Fatalf("per-session pin: %v", err)
	}
}

// TestThePerOwnerBoundHolds: the MaxPerOwner+1-th pin is refused, and another
// owner is unaffected.
func TestThePerOwnerBoundHolds(t *testing.T) {
	s, _ := pgStore(t)
	for i := 0; i < MaxPerOwner; i++ {
		if err := s.PinMirror(as(2), MirrorKey{Host: "laptop", Session: "s", Window: "@" + itoa(i)}); err != nil {
			t.Fatalf("pin %d: %v", i, err)
		}
	}
	if err := s.PinMirror(as(2), MirrorKey{Host: "laptop", Session: "s", Window: "@99999"}); !errors.Is(err, ErrTooMany) {
		t.Fatalf("pin past the bound: %v, want ErrTooMany", err)
	}
	// Re-pinning one already pinned is no new row, so it is not refused.
	if err := s.PinMirror(as(2), MirrorKey{Host: "laptop", Session: "s", Window: "@0"}); err != nil {
		t.Fatalf("re-pin at the bound: %v", err)
	}
	if got := mustList(t, s, 2); len(got.Mirrors) != MaxPerOwner {
		t.Fatalf("%d pins after the refused one, want %d", len(got.Mirrors), MaxPerOwner)
	}
	if err := s.PinMirror(as(3), win); err != nil {
		t.Fatalf("another owner's pin: %v", err)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

// TestAPinRacingTheAgentsDeleteIsNoSuchAgent: the agent is deleted by a
// transaction that is still open when the pin is inserted. The owner trigger
// still sees the row and passes; the foreign key check then waits on the
// deleter's row lock, and when the deleter commits it fails 23503. That is "no
// such agent" too — not a 500.
//
// Deterministic: the commit is issued only once the insert is observed
// WAITING on a lock (pg_stat_activity), so the insert cannot run before the
// delete nor after its commit. The 23503 is asserted on the store's wrapped
// error path: with the 23503 arm removed the error is not ErrNoSuchAgent.
func TestAPinRacingTheAgentsDeleteIsNoSuchAgent(t *testing.T) {
	s, pool := pgStore(t)
	a := mustAgent(t, pool, 2, "pin-race")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The deleter: the login role, so it sees and may delete the row.
	del, err := pgx.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer del.Close(context.Background())
	tx, err := del.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if tag, err := tx.Exec(ctx, `DELETE FROM agents WHERE id = $1`, a.ID); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("uncommitted delete: %v (%d rows)", err, tag.RowsAffected())
	}

	// The observer: a third connection, as the login role — pg_stat_activity
	// hides another session's query text from the application role.
	obs, err := pgx.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer obs.Close(context.Background())

	done := make(chan error, 1)
	go func() { done <- s.PinAgent(as(2), a.ID) }()

	// Wait until the insert is blocked on a lock — the FK check on the row
	// the open transaction deleted.
	waiting := false
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		select {
		case err := <-done:
			t.Fatalf("the pin returned (%v) before the delete committed: the race was not reproduced", err)
		default:
		}
		var n int
		if err := obs.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE '%INSERT INTO pins%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			waiting = true
			break
		}
	}
	if !waiting {
		t.Fatal("the pin never waited on the deleter's lock: the race was not reproduced")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	err = <-done
	if !errors.Is(err, ErrNoSuchAgent) {
		t.Fatalf("a pin racing its agent's delete: %v, want ErrNoSuchAgent", err)
	}
	if got := mustList(t, s, 2); got.Agent(a.ID) {
		t.Fatalf("the racing pin landed: %+v", got)
	}
}
