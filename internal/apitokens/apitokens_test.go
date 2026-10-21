package apitokens

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/credentials"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/devices"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/users"
)

type world struct {
	s    *PGStore
	pool *pgxpool.Pool
	a, b int64 // two member users
}

func newWorld(t *testing.T) world {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	sys := authz.System(context.Background())
	w := world{s: NewPG(pool), pool: pool}
	for _, id := range []*int64{&w.a, &w.b} {
		if err := pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', 'tok') RETURNING id`).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		ids := []int64{w.a, w.b, authz.OperatorUserID}
		_, _ = pool.Exec(sys, `DELETE FROM api_tokens WHERE user_id = ANY($1)`, ids)
		_, _ = pool.Exec(sys, `DELETE FROM audit_log WHERE user_id = ANY($1)`, ids)
		_, _ = pool.Exec(sys, `DELETE FROM users WHERE id = ANY($1)`, []int64{w.a, w.b})
	})
	return w
}

func as(uid int64, role authz.Role) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: role, Via: authz.ViaSession})
}

func sysCtx() context.Context { return authz.System(context.Background()) }

func TestMintedTokensAreWellFormedAndDistinct(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		tok, err := New()
		if err != nil {
			t.Fatal(err)
		}
		if !ShapeOK(tok) || !strings.HasPrefix(tok, Prefix) || len(tok) != len(Prefix)+encodedLen {
			t.Fatalf("minted %q is not shaped like a token", tok)
		}
		if seen[tok] {
			t.Fatalf("minted %q twice", tok)
		}
		seen[tok] = true
	}
	for _, bad := range []string{"", "mst_", "mst_short", "MST_" + strings.Repeat("A", 43), "mst_" + strings.Repeat("A", 42),
		"mst_" + strings.Repeat("A", 44), "mst_" + strings.Repeat("!", 43), strings.Repeat("A", 47)} {
		if ShapeOK(bad) {
			t.Errorf("ShapeOK(%q) = true", bad)
		}
	}
}

// TestAMalformedTokenIsRefusedWithoutADatabaseRead: a store over a NIL pool
// panics on any query, so a malformed token answered ErrBadToken here never
// reached Postgres. The positive control is the well-formed token, which does
// reach the pool and panics.
func TestAMalformedTokenIsRefusedWithoutADatabaseRead(t *testing.T) {
	s := &PGStore{now: time.Now}
	for _, bad := range []string{"", "mst_x", "not-a-token", "mst_" + strings.Repeat("*", 43)} {
		if _, err := s.Authenticate(context.Background(), bad); !errors.Is(err, ErrBadToken) {
			t.Errorf("Authenticate(%q) = %v; want ErrBadToken", bad, err)
		}
	}
	good, _ := New()
	reached := func() (r bool) {
		defer func() { r = recover() != nil }()
		_, _ = s.Authenticate(context.Background(), good)
		return false
	}()
	if !reached {
		t.Fatal("CONTROL: a well-formed token did not reach the (nil) pool, so the refusals above prove nothing")
	}
}

func TestOnlyTheHashIsStoredAndTheTokenIsShownOnce(t *testing.T) {
	w := newWorld(t)
	tok, rec, err := w.s.Create(as(w.a, authz.RoleMember), "  laptop  ", 0)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Name != "laptop" || rec.UserID != w.a || rec.Revoked() || rec.LastUsedAt != nil {
		t.Fatalf("record = %+v", rec)
	}
	var stored []byte
	var row string
	if err := w.pool.QueryRow(sysCtx(), `SELECT token_hash, t::text FROM api_tokens t WHERE id = $1`, rec.ID).Scan(&stored, &row); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(tok))
	if string(stored) != string(sum[:]) {
		t.Fatalf("token_hash is not sha256(token)")
	}
	secret := strings.TrimPrefix(tok, Prefix)
	if strings.Contains(row, secret) {
		t.Fatalf("the stored row carries the token itself: %s", row)
	}
	// No read path returns it: the record type has no field for it, and the
	// list carries the same record.
	list, err := w.s.List(as(w.a, authz.RoleMember))
	if err != nil || len(list) != 1 || list[0].ID != rec.ID {
		t.Fatalf("list = %+v, %v", list, err)
	}
	var audit string
	if err := w.pool.QueryRow(sysCtx(), `SELECT coalesce(string_agg(a::text, ' '), '') FROM audit_log a WHERE user_id = $1`, w.a).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(audit, secret) {
		t.Fatalf("audit_log carries the token: %s", audit)
	}
}

func TestAuthenticateResolvesTheOwnerAndRefusesRevokedAndDisabled(t *testing.T) {
	w := newWorld(t)
	tokA, recA, err := w.s.Create(as(w.a, authz.RoleMember), "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	tokB, _, err := w.s.Create(as(w.b, authz.RoleMember), "b", 0)
	if err != nil {
		t.Fatal(err)
	}
	for tok, want := range map[string]int64{tokA: w.a, tokB: w.b} {
		got, err := w.s.Authenticate(sysCtx(), tok)
		if err != nil || got.UserID != want {
			t.Fatalf("Authenticate = %+v, %v; want user %d", got, err, want)
		}
	}
	unknown, _ := New()
	if _, err := w.s.Authenticate(sysCtx(), unknown); !errors.Is(err, ErrBadToken) {
		t.Fatalf("an unknown token: %v", err)
	}

	// The disabled FLAG is checked on every call too — the second wall behind
	// the product's disable, which REVOKES (TestDisablingAUserRevokesItsTokensForGood).
	// Set here by raw SQL, which revokes nothing, so clearing it again is
	// fixture cleanup, not a product path.
	if _, err := w.pool.Exec(sysCtx(), `UPDATE users SET disabled_at = now() WHERE id = $1`, w.a); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.Authenticate(sysCtx(), tokA); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("a disabled user's token: %v; want ErrUserDisabled", err)
	}
	if _, err := w.pool.Exec(sysCtx(), `UPDATE users SET disabled_at = NULL WHERE id = $1`, w.a); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.Authenticate(sysCtx(), tokA); err != nil {
		t.Fatalf("flag cleared by raw SQL: %v", err)
	}

	// Revoked: refused on the very next call.
	found, live, err := w.s.Revoke(as(w.a, authz.RoleMember), recA.ID)
	if err != nil || !found || !live {
		t.Fatalf("revoke = %v %v %v", found, live, err)
	}
	if _, err := w.s.Authenticate(sysCtx(), tokA); !errors.Is(err, ErrBadToken) {
		t.Fatalf("a revoked token: %v; want ErrBadToken", err)
	}
	// Revoking again: found, not live, and no second audit row.
	found, live, err = w.s.Revoke(as(w.a, authz.RoleMember), recA.ID)
	if err != nil || !found || live {
		t.Fatalf("second revoke = %v %v %v; want found, already revoked", found, live, err)
	}
	var actions []string
	rows, err := w.pool.Query(sysCtx(), `SELECT action || ' ' || object || ' ' || via FROM audit_log WHERE user_id = $1 ORDER BY id`, w.a)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		actions = append(actions, s)
	}
	rows.Close()
	want := []string{"api_token.create " + Object(recA.ID) + " session", "api_token.revoke " + Object(recA.ID) + " session"}
	if strings.Join(actions, "|") != strings.Join(want, "|") {
		t.Fatalf("A's audit rows = %q; want %q", actions, want)
	}
}

// TestDisablingAUserRevokesItsTokensForGood (review round 1): the product's
// disable writer (users.SetDisabled, which every disable path goes through)
// revokes the user's tokens in its own transaction, so RE-ENABLING the user
// brings none of them back. Another user's token is untouched (control), and
// a token minted after the re-enable works.
func TestDisablingAUserRevokesItsTokensForGood(t *testing.T) {
	w := newWorld(t)
	tokA, _, err := w.s.Create(as(w.a, authz.RoleMember), "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	tokB, _, err := w.s.Create(as(w.b, authz.RoleMember), "b", 0)
	if err != nil {
		t.Fatal(err)
	}
	us := users.NewPG(w.pool)
	if _, err := us.SetDisabled(context.Background(), w.a, true); err != nil {
		t.Fatal(err)
	}
	var live int
	if err := w.pool.QueryRow(sysCtx(), `SELECT count(*) FROM api_tokens WHERE user_id = $1 AND revoked_at IS NULL`, w.a).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Fatalf("after the disable A has %d live token(s); want 0 (revoked with the disable)", live)
	}
	if _, err := us.SetDisabled(context.Background(), w.a, false); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.Authenticate(sysCtx(), tokA); !errors.Is(err, ErrBadToken) {
		t.Fatalf("A's token after disable + RE-ENABLE: %v; want ErrBadToken (it must not come back)", err)
	}
	if got, err := w.s.Authenticate(sysCtx(), tokB); err != nil || got.UserID != w.b {
		t.Fatalf("CONTROL: B's token after A's disable: %+v %v", got, err)
	}
	fresh, _, err := w.s.Create(as(w.a, authz.RoleMember), "after", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.Authenticate(sysCtx(), fresh); err != nil {
		t.Fatalf("a token minted after the re-enable: %v", err)
	}
}

// TestNobodyRevokesAnotherUsersToken: B — and the admin — revoking A's id is
// "no such token", and A's token still works.
func TestNobodyRevokesAnotherUsersToken(t *testing.T) {
	w := newWorld(t)
	tokA, recA, err := w.s.Create(as(w.a, authz.RoleMember), "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, ctx := range []context.Context{as(w.b, authz.RoleMember), as(authz.OperatorUserID, authz.RoleAdmin)} {
		found, live, err := w.s.Revoke(ctx, recA.ID)
		if err != nil || found || live {
			t.Fatalf("revoking another user's token = %v %v %v; want not found", found, live, err)
		}
	}
	if _, err := w.s.Authenticate(sysCtx(), tokA); err != nil {
		t.Fatalf("A's token after the others' revokes: %v", err)
	}
}

// TestRowLevelSecurityKeepsTokensToTheirOwner reads api_tokens with RAW SQL —
// not through List, whose WHERE clause would hide a broken policy — as B and
// as the admin: neither sees A's row, and neither can insert a row for A.
// The positive control is A, who sees its own.
func TestRowLevelSecurityKeepsTokensToTheirOwner(t *testing.T) {
	w := newWorld(t)
	if _, _, err := w.s.Create(as(w.a, authz.RoleMember), "a", 0); err != nil {
		t.Fatal(err)
	}
	count := func(ctx context.Context) int {
		var n int
		if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM api_tokens WHERE user_id = $1`, w.a).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(as(w.a, authz.RoleMember)); n != 1 {
		t.Fatalf("CONTROL: A sees %d of its own tokens; want 1", n)
	}
	if n := count(as(w.b, authz.RoleMember)); n != 0 {
		t.Errorf("B sees %d of A's tokens", n)
	}
	if n := count(as(authz.OperatorUserID, authz.RoleAdmin)); n != 0 {
		t.Errorf("the ADMIN sees %d of A's tokens — admins list only their own", n)
	}
	h := sha256.Sum256([]byte("forged"))
	if _, err := w.pool.Exec(as(w.b, authz.RoleMember), `INSERT INTO api_tokens (user_id, name, token_hash) VALUES ($1, 'x', $2)`, w.a, h[:]); err == nil {
		t.Error("B inserted a token owned by A")
	}
	if n, _ := w.s.List(as(w.b, authz.RoleMember)); len(n) != 0 {
		t.Errorf("B's List = %+v", n)
	}
}

func TestManagementNeedsAUserPrincipal(t *testing.T) {
	w := newWorld(t)
	for name, ctx := range map[string]context.Context{"none": context.Background(), "system": sysCtx()} {
		if _, _, err := w.s.Create(ctx, "x", 0); !errors.Is(err, ErrNoUser) {
			t.Errorf("%s Create: %v", name, err)
		}
		if _, err := w.s.List(ctx); !errors.Is(err, ErrNoUser) {
			t.Errorf("%s List: %v", name, err)
		}
		if _, _, err := w.s.Revoke(ctx, 1); !errors.Is(err, ErrNoUser) {
			t.Errorf("%s Revoke: %v", name, err)
		}
	}
	if _, _, err := w.s.Create(as(w.a, authz.RoleMember), strings.Repeat("é", MaxNameLen+1), 0); !errors.Is(err, ErrNameTooLong) {
		t.Errorf("an over-long name: %v", err)
	}
	if _, _, err := w.s.Create(as(w.a, authz.RoleMember), strings.Repeat("é", MaxNameLen), 0); err != nil {
		t.Errorf("a name of exactly %d characters: %v", MaxNameLen, err)
	}
}

// TestLastUsedIsStampedAndThrottled: the first use stamps last_used_at; a use
// within LastUsedGranularity does not rewrite it; a use after it does.
func TestLastUsedIsStampedAndThrottled(t *testing.T) {
	w := newWorld(t)
	tok, rec, err := w.s.Create(as(w.a, authz.RoleMember), "a", 0)
	if err != nil {
		t.Fatal(err)
	}
	read := func() *time.Time {
		var at *time.Time
		if err := w.pool.QueryRow(sysCtx(), `SELECT last_used_at FROM api_tokens WHERE id = $1`, rec.ID).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	if _, err := w.s.Authenticate(sysCtx(), tok); err != nil {
		t.Fatal(err)
	}
	first := read()
	if first == nil {
		t.Fatal("the first use did not stamp last_used_at")
	}
	if _, err := w.s.Authenticate(sysCtx(), tok); err != nil {
		t.Fatal(err)
	}
	if second := read(); !second.Equal(*first) {
		t.Fatalf("a use within %v rewrote last_used_at (%v → %v)", LastUsedGranularity, first, second)
	}
	// Age the stamp past the window; the next use moves it.
	if _, err := w.pool.Exec(sysCtx(), `UPDATE api_tokens SET last_used_at = now() - interval '2 minutes' WHERE id = $1`, rec.ID); err != nil {
		t.Fatal(err)
	}
	aged := read()
	if _, err := w.s.Authenticate(sysCtx(), tok); err != nil {
		t.Fatal(err)
	}
	if third := read(); !third.After(*aged) {
		t.Fatalf("a use after %v did not move last_used_at (%v → %v)", LastUsedGranularity, aged, third)
	}
}

// lockWatch is a login-role connection that can LOCK a table (the application
// role cannot) and count sessions waiting on a lock in this database.
type lockWatch struct {
	t    *testing.T
	conn *pgx.Conn
}

func newLockWatch(t *testing.T) *lockWatch {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dbtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return &lockWatch{t: t, conn: conn}
}

func (l *lockWatch) waiting() int {
	var n int
	if err := l.conn.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database() AND wait_event_type = 'Lock' AND pid <> pg_backend_pid()`).Scan(&n); err != nil {
		l.t.Fatal(err)
	}
	return n
}

// until waits for at least n sessions to be blocked on a lock.
func (l *lockWatch) until(n int, what string) {
	l.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for l.waiting() < n {
		if time.Now().After(deadline) {
			l.t.Fatalf("timed out waiting for %s (%d lock waiter(s))", what, l.waiting())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// raceMintAgainstDisable holds the REAL disable (users.SetDisabledAs) of w.a
// in flight by locking a table it writes to mid-transaction, starts a mint as
// w.a, releases, and re-enables. The disable must succeed, and the minted
// token must not be live: the mint failed ErrUserDisabled, or its token is
// revoked.
func raceMintAgainstDisable(t *testing.T, w world, role authz.Role, lockTable string) {
	t.Helper()
	ctx := context.Background()
	lw := newLockWatch(t)
	btx, err := lw.conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := btx.Exec(ctx, `LOCK TABLE `+lockTable+` IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	us := users.NewPG(w.pool)
	disabled := make(chan error, 1)
	go func() {
		_, err := us.SetDisabledAs(ctx, &users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}, w.a, true)
		disabled <- err
	}()
	lw.until(1, "the disable to block on "+lockTable)
	type minted struct {
		tok string
		err error
	}
	mintc := make(chan minted, 1)
	go func() {
		tok, _, err := w.s.Create(as(w.a, role), "racer", 0)
		mintc <- minted{tok, err}
	}()
	lw.until(2, "the mint to block")
	if err := btx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-disabled; err != nil {
		t.Fatalf("the disable FAILED: %v (a deadlock against the mint leaves the user enabled)", err)
	}
	m := <-mintc
	var isDisabled bool
	if err := w.pool.QueryRow(sysCtx(), `SELECT disabled_at IS NOT NULL FROM users WHERE id = $1`, w.a).Scan(&isDisabled); err != nil || !isDisabled {
		t.Fatalf("after the disable returned, the user is disabled = %v (%v)", isDisabled, err)
	}
	if _, err := us.SetDisabled(ctx, w.a, false); err != nil {
		t.Fatal(err)
	}
	if m.err != nil {
		if !errors.Is(m.err, ErrUserDisabled) {
			t.Fatalf("the racing mint failed with %v; want ErrUserDisabled", m.err)
		}
		return
	}
	if _, err := w.s.Authenticate(sysCtx(), m.tok); !errors.Is(err, ErrBadToken) {
		t.Fatalf("a token minted while a disable was in flight is LIVE after re-enable (%v); it must be refused or revoked", err)
	}
}

// TestAMintRacingADisableNeverSurvivesIt (review round 3). The window: a
// disable has RUN its revoke statement but not committed, and a mint inserts
// meanwhile — that token was not there to be revoked, so it came back on
// re-enable. audit_log is what the disable writes AFTER its revoke, so locking
// it holds the disable inside that window. A MEMBER target.
func TestAMintRacingADisableNeverSurvivesIt(t *testing.T) {
	raceMintAgainstDisable(t, newWorld(t), authz.RoleMember, "audit_log")
}

// TestAMintRacingAnAdminsDisableDoesNotDeadlock (review round 4). An ADMIN
// target: the disable's last-admin guard takes FOR UPDATE on every enabled
// admin row — the target's included — and the mint's INSERT needs FOR KEY
// SHARE on that row (its foreign key) while holding the credentials lock. With
// the disable locking rows BEFORE the credentials lock, the two deadlocked:
// Postgres killed the disable (SQLSTATE 40P01), the user stayed enabled and
// the minted token was live. `invites` is what the disable touches after its
// row locks, so locking it holds the disable there while the mint queues.
func TestAMintRacingAnAdminsDisableDoesNotDeadlock(t *testing.T) {
	w := newWorld(t)
	if _, err := w.pool.Exec(sysCtx(), `UPDATE users SET role = 'admin' WHERE id = $1`, w.a); err != nil {
		t.Fatal(err)
	}
	raceMintAgainstDisable(t, w, authz.RoleAdmin, "invites")
}

// TestTheCutoffIsStampedAfterTheCredentialsLockWait (review round 4): the
// session cutoff a disable or a reset stamps must be taken AFTER the wait for
// the credentials lock, or a session issued during that wait postdates the
// cutoff and survives. The test holds the lock, lets the writer queue behind
// it, reads the database clock, releases — and the cutoff must not be earlier
// than that reading.
func TestTheCutoffIsStampedAfterTheCredentialsLockWait(t *testing.T) {
	for name, write := range map[string]func(*users.PGStore, int64) (time.Time, error){
		"disable": func(us *users.PGStore, id int64) (time.Time, error) {
			u, err := us.SetDisabledAs(context.Background(), nil, id, true)
			if err != nil || u.SessionsNotBefore == nil {
				return time.Time{}, err
			}
			return *u.SessionsNotBefore, nil
		},
		"reset": func(us *users.PGStore, id int64) (time.Time, error) {
			return us.ResetCredentials(context.Background(), id)
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			lw := newLockWatch(t)
			hold, err := w.pool.Begin(sysCtx())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = hold.Rollback(context.Background()) }()
			if err := users.LockCredentialsTx(sysCtx(), hold, w.a); err != nil {
				t.Fatal(err)
			}
			type res struct {
				cut time.Time
				err error
			}
			done := make(chan res, 1)
			go func() {
				cut, err := write(users.NewPG(w.pool), w.a)
				done <- res{cut, err}
			}()
			lw.until(1, "the "+name+" to queue behind the credentials lock")
			time.Sleep(50 * time.Millisecond)
			var mark time.Time
			if err := lw.conn.QueryRow(context.Background(), `SELECT clock_timestamp()`).Scan(&mark); err != nil {
				t.Fatal(err)
			}
			if err := hold.Commit(sysCtx()); err != nil {
				t.Fatal(err)
			}
			r := <-done
			if r.err != nil || r.cut.IsZero() {
				t.Fatalf("the %s: cutoff %v, %v", name, r.cut, r.err)
			}
			if r.cut.Before(mark) {
				t.Fatalf("the %s stamped its session cutoff %v BEFORE it stopped waiting for the credentials lock (still waiting at %v)",
					name, r.cut.Format(time.RFC3339Nano), mark.Format(time.RFC3339Nano))
			}
		})
	}
}

// TestAMintQueuedBehindAResetIsRefused (review round 4): a reset revokes the
// tokens but leaves the user ENABLED, so a mint queued behind it used to
// insert a live token. The mint carries its session's issued-at, and under the
// credentials lock a session issued at or before the cutoff is refused — the
// session tier's own rule. The session the reset signs in (issued after the
// cutoff's second) can mint.
func TestAMintQueuedBehindAResetIsRefused(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	lw := newLockWatch(t)
	oldSession := time.Now().Add(-time.Minute).Unix() // a session from before the reset
	btx, err := lw.conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The reset's revoke writes api_tokens; locking it holds the reset in
	// flight, AFTER it moved the cutoff and while it holds the credentials lock.
	if _, err := btx.Exec(ctx, `LOCK TABLE api_tokens IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	type reset struct {
		cut time.Time
		err error
	}
	resetc := make(chan reset, 1)
	go func() {
		cut, err := users.NewPG(w.pool).ResetCredentials(ctx, w.a)
		resetc <- reset{cut, err}
	}()
	lw.until(1, "the reset to block on api_tokens")
	type minted struct {
		tok string
		err error
	}
	mintc := make(chan minted, 1)
	go func() {
		tok, _, err := w.s.Create(as(w.a, authz.RoleMember), "queued", oldSession)
		mintc <- minted{tok, err}
	}()
	lw.until(2, "the mint to queue behind the reset")
	if err := btx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-resetc
	if r.err != nil {
		t.Fatalf("the reset: %v", r.err)
	}
	m := <-mintc
	if !errors.Is(m.err, ErrSessionEnded) {
		live := "no token"
		if m.err == nil {
			_, aerr := w.s.Authenticate(sysCtx(), m.tok)
			live = fmt.Sprintf("a token whose Authenticate is %v", aerr)
		}
		t.Fatalf("a mint by a pre-reset session, queued behind the reset: %v (%s); want ErrSessionEnded", m.err, live)
	}
	// The boundary: a session stamped with the cutoff's own second is refused
	// (the session tier's rule); the next second's is the post-reset session.
	if _, _, err := w.s.Create(as(w.a, authz.RoleMember), "same-second", r.cut.Unix()); !errors.Is(err, ErrSessionEnded) {
		t.Fatalf("a session issued in the cutoff's own second: %v; want ErrSessionEnded", err)
	}
	tok, _, err := w.s.Create(as(w.a, authz.RoleMember), "after-reset", r.cut.Unix()+1)
	if err != nil {
		t.Fatalf("the POST-reset session cannot mint: %v", err)
	}
	if _, err := w.s.Authenticate(sysCtx(), tok); err != nil {
		t.Fatalf("the post-reset session's token: %v", err)
	}
}

// stressResult is what one unchoreographed run of the credential writers saw.
type stressResult struct {
	deadlocks int
	firstDead string
	ok        map[string]int // per op: calls that returned no error (or were refused by their own rule)
	attempts  map[string]int // per op: calls made
	rounds    int
}

// stressCredentialWriters runs every writer that takes the credentials lock or
// touches api_tokens — plus the readers and role changes that share their row
// locks — CONCURRENTLY and UNCHOREOGRAPHED against one ADMIN target (w.a),
// with a second admin (w.b) minting beside it, for up to `rounds` rounds each
// or `budget`. It counts SQLSTATE 40P01. The invite create/revoke by the
// target are here because a disable also revokes its target's pending invites.
// errRefusedNotFailed is a writer that ran and was REFUSED by the rule it
// enforces (a pairing after a disable): neither a success nor a deadlock.
var errRefusedNotFailed = errors.New("refused by its own rule")

func stressCredentialWriters(t *testing.T, w world, rounds int, budget time.Duration) stressResult {
	t.Helper()
	ctx := context.Background()
	sys := sysCtx()
	if _, err := w.pool.Exec(sys, `UPDATE users SET role = 'admin' WHERE id = ANY($1)`, []int64{w.a, w.b}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = w.pool.Exec(sysCtx(), `DELETE FROM invites WHERE created_by = ANY($1)`, []int64{w.a, w.b})
	})
	us := users.NewPG(w.pool)
	op := users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}
	target := users.Actor{UserID: w.a, Via: authz.ViaSession}
	asX, asY := as(w.a, authz.RoleAdmin), as(w.b, authz.RoleAdmin)
	ops := map[string]func(i int) error{
		"mint":                  func(int) error { _, _, err := w.s.Create(asX, "stress", 0); return err },
		"mint-by-another-admin": func(int) error { _, _, err := w.s.Create(asY, "stress", 0); return err },
		"revoke-one": func(int) error {
			list, err := w.s.List(asX)
			if err != nil {
				return err
			}
			for _, tk := range list {
				if !tk.Revoked() {
					_, _, err = w.s.Revoke(asX, tk.ID)
					return err
				}
			}
			return nil
		},
		"list":    func(int) error { _, err := w.s.List(asX); return err },
		"disable": func(int) error { _, err := us.SetDisabledAs(ctx, &op, w.a, true); return err },
		"enable":  func(int) error { _, err := us.SetDisabledAs(ctx, &op, w.a, false); return err },
		"reset":   func(int) error { _, err := us.ResetCredentials(ctx, w.a); return err },
		"demote": func(i int) error {
			role := authz.RoleMember
			if i%2 == 1 {
				role = authz.RoleAdmin
			}
			_, err := us.SetRole(ctx, op, w.a, role)
			return err
		},
	}
	// Bring-your-own credentials (#69): a save and a delete each write a
	// user_credentials row and an audit row, both keyed on the owner's users
	// row. They take neither lock, and need neither: nothing else writes
	// user_credentials, so they can wait for a disable but hold nothing it wants.
	creds, err := credentials.NewPG(w.pool, bytes.Repeat([]byte{9}, credentials.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = w.pool.Exec(sysCtx(), `DELETE FROM user_credentials WHERE owner_id = ANY($1)`, []int64{w.a, w.b})
	})
	ops["credential-save"] = func(i int) error {
		return creds.PutOpenRouter(asX, credentials.OpenRouterKeyPrefix+fmt.Sprintf("stress-key-%020d", i))
	}
	ops["credential-delete"] = func(int) error {
		_, err := creds.Delete(asX, credentials.KindOpenRouter, "")
		return err
	}
	{
		ops["invite-create"] = func(i int) error {
			h := sha256.Sum256([]byte(fmt.Sprintf("stress-%d-%d-%d", w.a, i, time.Now().UnixNano())))
			_, err := us.CreateInvite(ctx, target, h[:], "", time.Now().Add(time.Hour))
			return err
		}
		ops["invite-revoke"] = func(int) error {
			var id int64
			err := w.pool.QueryRow(sys, `SELECT id FROM invites WHERE created_by = $1 AND revoked_at IS NULL AND redeemed_at IS NULL
				ORDER BY id DESC LIMIT 1`, w.a).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if err := us.RevokeInvite(ctx, target, id); err != nil && !errors.Is(err, users.ErrNotRevocable) {
				return err
			}
			return nil
		}
	}
	// Paired phones (0038): pairing (an UNLOCKED read of the code's owner, then
	// the credentials lock, then the code row) and removing take the
	// credentials lock first; a disable, a reset and a sign-out everywhere
	// delete the user's phones under it.
	ds := devices.NewPG(w.pool)
	t.Cleanup(func() {
		_, _ = w.pool.Exec(sysCtx(), `DELETE FROM device_pairing_codes WHERE owner_id = ANY($1)`, []int64{w.a, w.b})
		_, _ = w.pool.Exec(sysCtx(), `DELETE FROM devices WHERE owner_id = ANY($1)`, []int64{w.a, w.b})
	})
	ops["device-pair"] = func(int) error {
		// A sign-in an hour ahead, so the stress's constant resets and
		// sign-outs never refuse it by the rule: what is measured here is the
		// lock order, and a pairing that is always refused takes no lock past
		// the code row.
		code, _, err := ds.CreatePairingCode(asX, time.Now().Add(time.Hour))
		if err != nil {
			return err
		}
		_, _, err = ds.Redeem(ctx, code, devices.PairIn{Name: "stress"})
		if errors.Is(err, devices.ErrSessionEnded) || errors.Is(err, devices.ErrUserDisabled) ||
			errors.Is(err, devices.ErrSignedOut) || errors.Is(err, devices.ErrBadCode) || errors.Is(err, devices.ErrTooMany) {
			return errRefusedNotFailed
		}
		return err
	}
	ops["device-remove"] = func(int) error {
		list, err := ds.List(asX)
		if err != nil || len(list) == 0 {
			return err
		}
		_, err = ds.Remove(asX, list[0].ID)
		return err
	}
	ops["sign-out-everywhere"] = func(int) error { _, err := us.RecordSignOutEverywhere(ctx, w.a); return err }
	// A phone registering its push endpoint (push.PGStore.ClaimNative): it
	// locks the device row and then writes push rows, the order a revocation's
	// DELETE FROM devices (cascading to push rows) takes.
	ps := push.NewPG(w.pool).WithAllowlist(push.NewAllowlist([]string{"ntfy.example.org"}))
	t.Cleanup(func() {
		_, _ = w.pool.Exec(sysCtx(), `DELETE FROM push_subscriptions WHERE owner_id = ANY($1)`, []int64{w.a, w.b})
	})
	ops["claim-native"] = func(i int) error {
		// Its own phone, paired just now, so most rounds have one to claim for
		// (the other writers remove and revoke them all the time).
		code, _, err := ds.CreatePairingCode(asX, time.Now().Add(time.Hour))
		if err != nil {
			return err
		}
		_, d, err := ds.Redeem(ctx, code, devices.PairIn{Name: "claimer"})
		if err != nil {
			return errRefusedNotFailed
		}
		list := []devices.Device{d}
		var sub push.Subscription
		sub.Endpoint = fmt.Sprintf("https://ntfy.example.org/upStress%d", i)
		sub.Keys.P256dh, sub.Keys.Auth = "k", "a"
		err = ps.ClaimNative(ctx, w.a, list[0].ID, sub, "stress")
		if errors.Is(err, push.ErrDeviceGone) {
			return errRefusedNotFailed
		}
		return err
	}
	var mu sync.Mutex
	res := stressResult{ok: map[string]int{}, attempts: map[string]int{}}
	// 🔴 LOCKSTEP, NOT A FREE-FOR-ALL. Each round starts EVERY writer at once,
	// one operation each, and the next round waits for all of them. Every
	// writer therefore makes exactly one attempt per round however slow it is —
	// the free-running version let the fast writers flood the user's row and
	// starve the demote's SELECT … FOR UPDATE, so its progress controls
	// collapsed intermittently (a fact about the test's flood, not a demote
	// stuck in production: here every writer completes every round). Starting
	// them together is also what makes the rounds collide.
	deadline := time.Now().Add(budget)
	for i := 0; i < rounds && time.Now().Before(deadline); i++ {
		var wg sync.WaitGroup
		for name, fn := range ops {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := fn(i)
				mu.Lock()
				defer mu.Unlock()
				var pg *pgconn.PgError
				switch {
				case err == nil:
					res.ok[name]++
				case errors.As(err, &pg) && pg.Code == "40P01":
					res.deadlocks++
					if res.firstDead == "" {
						res.firstDead = name + ": " + pg.Message + " — " + pg.Detail
					}
				}
				res.attempts[name]++
			}()
		}
		wg.Wait()
		res.rounds++
		if res.deadlocks > 0 {
			return res // one deadlock is the verdict; each costs deadlock_timeout
		}
	}
	// THEN FREE-RUNNING PHASES, for the verdict only. Lockstep starts every
	// writer together but lets each finish before the next round, so a window
	// between two statements of ONE writer rarely meets another writer's —
	// measured: Revoke with its credentials lock removed deadlocked in 0 of 5
	// lockstep-only runs. Free-running writers overlap at every offset. Two
	// phases: every writer, and then the writers that predate the paired
	// phones on their own — the phone writers all take the credentials lock and
	// serialise the rest, which dropped that Revoke mutant's catch rate to 0 of
	// 5 when they ran in the only free phase. Progress is NOT asserted here
	// (that is what starved the old controls), only that no deadlock occurs.
	phoneWriters := map[string]bool{"device-pair": true, "device-remove": true, "sign-out-everywhere": true, "claim-native": true}
	for _, onlyOld := range []bool{false, true} {
		freeDeadline := time.Now().Add(freeBudget)
		var wg sync.WaitGroup
		for name, fn := range ops {
			if onlyOld && phoneWriters[name] {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < freeRounds && time.Now().Before(freeDeadline); i++ {
					err := fn(rounds + i)
					var pg *pgconn.PgError
					mu.Lock()
					if errors.As(err, &pg) && pg.Code == "40P01" {
						res.deadlocks++
						if res.firstDead == "" {
							res.firstDead = name + " (free-running): " + pg.Message + " — " + pg.Detail
						}
					}
					stop := res.deadlocks > 0
					mu.Unlock()
					if stop {
						return
					}
				}
			}()
		}
		wg.Wait()
		if res.deadlocks > 0 {
			return res
		}
	}
	return res
}

// freeRounds and freeBudget bound stressCredentialWriters' free-running phase.
const (
	freeRounds = 700
	freeBudget = 8 * time.Second
)

// TestCredentialWritersNeverDeadlock (review round 5) is the guard on the
// ORDER RULE (users.LockCredentialsTx, users' account-writers lock): mint, mint
// by another admin, revoke, list, disable, enable, reset, demote/promote,
// invite create/revoke and credential save/delete (#69), all at once against
// an ADMIN, with nothing choreographed — and not one SQLSTATE 40P01.
//
// 🔴 IT ASSERTS AN OUTCOME (zero deadlocks), NOT AN INTERLEAVING. The
// choreographed tests above each pin one window; this is what fails when a
// lock moves to an order none of them happens to build — measured: with
// guardLastAdmin ahead of the credentials lock, with Revoke's lock removed, or
// with the account-writers lock removed from SetDisabledAs, SetRole or
// RevokeInvite, every choreographed test stays green and this one goes red.
//
// ⚠ BOUNDED: stressRounds LOCKSTEP rounds (every writer once per round, all
// started together; see stressCredentialWriters), with a generous time budget
// only as a backstop, and it stops at the first deadlock (each costs
// Postgres's 1 s deadlock_timeout). The controls are about PROGRESS, per
// writer, not about time: every round must have completed, every writer must
// have attempted every round, and every writer must have succeeded at least
// once — so a run that did nothing, or that starved one writer, fails.
// A free-running phase follows, asserting only the verdict (no 40P01); the
// two mutants re-measured on this shape — Revoke's credentials lock removed,
// and ClaimNative's — are each caught (see stressCredentialWriters).
const stressRounds = 150

func TestCredentialWritersNeverDeadlock(t *testing.T) {
	w := newWorld(t)
	res := stressCredentialWriters(t, w, stressRounds, 120*time.Second)
	if res.deadlocks != 0 {
		t.Fatalf("%d deadlock(s) (SQLSTATE 40P01) among the credential writers; first: %s", res.deadlocks, res.firstDead)
	}
	for _, name := range []string{"mint", "mint-by-another-admin", "revoke-one", "list", "disable", "enable", "reset", "demote",
		"invite-create", "invite-revoke", "credential-save", "credential-delete", "device-pair", "device-remove", "sign-out-everywhere",
		"claim-native"} {
		if res.ok[name] == 0 {
			t.Errorf("CONTROL: %q never succeeded, so its absence of deadlocks proves nothing (ok counts %v)", name, res.ok)
		}
		if res.attempts[name] != stressRounds {
			t.Errorf("CONTROL: %q attempted %d of %d rounds", name, res.attempts[name], stressRounds)
		}
	}
	if res.rounds != stressRounds {
		t.Errorf("CONTROL: %d of %d rounds completed inside the budget", res.rounds, stressRounds)
	}
	t.Logf("%d lockstep rounds, 0 deadlocks; successes per writer: %v", res.rounds, res.ok)
}
