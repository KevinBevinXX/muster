package apitokens

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
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
	tok, rec, err := w.s.Create(as(w.a, authz.RoleMember), "  laptop  ")
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
	tokA, recA, err := w.s.Create(as(w.a, authz.RoleMember), "a")
	if err != nil {
		t.Fatal(err)
	}
	tokB, _, err := w.s.Create(as(w.b, authz.RoleMember), "b")
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
	tokA, _, err := w.s.Create(as(w.a, authz.RoleMember), "a")
	if err != nil {
		t.Fatal(err)
	}
	tokB, _, err := w.s.Create(as(w.b, authz.RoleMember), "b")
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
	fresh, _, err := w.s.Create(as(w.a, authz.RoleMember), "after")
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
	tokA, recA, err := w.s.Create(as(w.a, authz.RoleMember), "a")
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
	if _, _, err := w.s.Create(as(w.a, authz.RoleMember), "a"); err != nil {
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
		if _, _, err := w.s.Create(ctx, "x"); !errors.Is(err, ErrNoUser) {
			t.Errorf("%s Create: %v", name, err)
		}
		if _, err := w.s.List(ctx); !errors.Is(err, ErrNoUser) {
			t.Errorf("%s List: %v", name, err)
		}
		if _, _, err := w.s.Revoke(ctx, 1); !errors.Is(err, ErrNoUser) {
			t.Errorf("%s Revoke: %v", name, err)
		}
	}
	if _, _, err := w.s.Create(as(w.a, authz.RoleMember), strings.Repeat("é", MaxNameLen+1)); !errors.Is(err, ErrNameTooLong) {
		t.Errorf("an over-long name: %v", err)
	}
	if _, _, err := w.s.Create(as(w.a, authz.RoleMember), strings.Repeat("é", MaxNameLen)); err != nil {
		t.Errorf("a name of exactly %d characters: %v", MaxNameLen, err)
	}
}

// TestLastUsedIsStampedAndThrottled: the first use stamps last_used_at; a use
// within LastUsedGranularity does not rewrite it; a use after it does.
func TestLastUsedIsStampedAndThrottled(t *testing.T) {
	w := newWorld(t)
	tok, rec, err := w.s.Create(as(w.a, authz.RoleMember), "a")
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

// TestAMintRacingADisableNeverSurvivesIt (review round 3). The window: a
// disable has RUN its revoke statement but not committed, and a mint inserts
// meanwhile — that token was not there to be revoked, so it came back on
// re-enable. The test holds the real disable (users.SetDisabledAs) inside that
// window by locking audit_log, which the disable writes to AFTER its revoke;
// then starts a mint, releases, re-enables, and requires the minted token to
// be refused — the mint failed, or its token is revoked — never live.
func TestAMintRacingADisableNeverSurvivesIt(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	dsn := dbtest.DSN(t)
	blocker, err := pgx.Connect(ctx, dsn) // the login role: may LOCK the table
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close(ctx)
	btx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := btx.Exec(ctx, `LOCK TABLE audit_log IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	waiting := func() int {
		var n int
		if err := blocker.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND pid <> pg_backend_pid()`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	until := func(n int, what string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for waiting() < n {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s (%d lock waiter(s))", what, waiting())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	us := users.NewPG(w.pool)
	disabled := make(chan error, 1)
	go func() {
		_, err := us.SetDisabledAs(ctx, &users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}, w.a, true)
		disabled <- err
	}()
	until(1, "the disable to block on audit_log (its revoke has run)")
	type minted struct {
		tok string
		err error
	}
	mintc := make(chan minted, 1)
	go func() {
		tok, _, err := w.s.Create(as(w.a, authz.RoleMember), "racer")
		mintc <- minted{tok, err}
	}()
	until(2, "the mint to block")
	if err := btx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-disabled; err != nil {
		t.Fatalf("the disable: %v", err)
	}
	m := <-mintc
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
