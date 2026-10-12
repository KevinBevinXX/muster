package users

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

func storeFor(t *testing.T) (*PG, *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	reset := func() {
		sys := authz.System(context.Background())
		for _, q := range []string{
			`DELETE FROM invites`,
			`UPDATE users SET gotrue_sub = NULL, email = NULL, disabled_at = NULL WHERE id = 1`,
			`DELETE FROM users WHERE id <> 1`,
		} {
			if _, err := pool.Exec(sys, q); err != nil {
				t.Fatal(err)
			}
		}
	}
	reset()
	t.Cleanup(reset)
	return NewPG(pool), pool
}

func hash(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

const subA = "11111111-1111-4111-a111-111111111111"

func TestClaimInviteLifecycle(t *testing.T) {
	s, _ := storeFor(t)
	ctx := context.Background()
	exp := time.Now().Add(time.Hour)

	if err := s.CreateClaimInvite(ctx, hash("first"), 1, 1, exp); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateClaimInvite(ctx, hash("second"), 1, 1, exp); err != nil {
		t.Fatal(err)
	}
	// Minting a new link revokes the earlier one: only one is live.
	if live, _ := s.ClaimInviteLive(ctx, hash("first")); live {
		t.Fatal("the earlier claim link is still live after a new one was minted")
	}
	if live, _ := s.ClaimInviteLive(ctx, hash("second")); !live {
		t.Fatal("the new claim link is not live")
	}
	if _, err := s.RedeemClaim(ctx, hash("first"), "a@example.com", mustNotCreate(t), nil); !errors.Is(err, ErrInvalidInvite) {
		t.Fatalf("the revoked link redeemed: %v", err)
	}
	u, err := s.RedeemClaim(ctx, hash("second"), "a@example.com", func(context.Context) (string, error) { return subA, nil }, nil)
	if err != nil || u.ID != 1 || u.GotrueSub != subA || u.Email != "a@example.com" || u.Role != authz.RoleAdmin {
		t.Fatalf("redeem: %+v %v", u, err)
	}
	if got, err := s.BySub(ctx, subA); err != nil || got.ID != 1 {
		t.Fatalf("BySub after claim: %+v %v", got, err)
	}
	// Bound now: a new claim link for user 1 is refused.
	if err := s.CreateClaimInvite(ctx, hash("third"), 1, 1, exp); !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("minting a claim for a bound user: %v", err)
	}
}

func TestAnExpiredClaimDoesNotRedeem(t *testing.T) {
	s, _ := storeFor(t)
	ctx := context.Background()
	if err := s.CreateClaimInvite(ctx, hash("old"), 1, 1, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemClaim(ctx, hash("old"), "a@example.com", mustNotCreate(t), nil); !errors.Is(err, ErrInvalidInvite) {
		t.Fatalf("expired link: %v", err)
	}
}

// TestRedeemUndoesTheGoTrueUserWhenTheBindingFails: the GoTrue user exists
// before the muster row is written; a failure after it must call undo.
func TestRedeemUndoesTheGoTrueUserWhenTheBindingFails(t *testing.T) {
	s, pool := storeFor(t)
	ctx := context.Background()
	// Another user already holds the email: users_email_lower_key makes the
	// binding UPDATE fail after create has run.
	if _, err := pool.Exec(authz.System(ctx), `INSERT INTO users (role, email) VALUES ('member', 'Taken@example.com')`); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateClaimInvite(ctx, hash("tok"), 1, 1, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var undone string
	_, err := s.RedeemClaim(ctx, hash("tok"), "taken@example.com",
		func(context.Context) (string, error) { return subA, nil }, func(sub string) { undone = sub })
	if err == nil {
		t.Fatal("binding a taken email succeeded")
	}
	if undone != subA {
		t.Fatalf("undo was called with %q, want %q", undone, subA)
	}
	// The link survives a failed redemption (nothing was committed).
	if live, _ := s.ClaimInviteLive(ctx, hash("tok")); !live {
		t.Fatal("a failed redemption consumed the link")
	}
	// And a create failure does NOT call undo (there is nothing to undo).
	undone = ""
	_, err = s.RedeemClaim(ctx, hash("tok"), "fresh@example.com",
		func(context.Context) (string, error) { return "", errors.New("gotrue down") }, func(sub string) { undone = sub })
	if err == nil || undone != "" {
		t.Fatalf("create failure: err=%v undone=%q", err, undone)
	}
}

func TestSetDisabledAndLookups(t *testing.T) {
	s, pool := storeFor(t)
	ctx := context.Background()
	var id int64
	if err := pool.QueryRow(authz.System(ctx), `INSERT INTO users (role, gotrue_sub) VALUES ('member', $1) RETURNING id`, subA).Scan(&id); err != nil {
		t.Fatal(err)
	}
	u, err := s.SetDisabled(ctx, id, true)
	if err != nil || !u.Disabled() || u.Role != authz.RoleMember {
		t.Fatalf("disable: %+v %v", u, err)
	}
	first := *u.DisabledAt
	u, _ = s.SetDisabled(ctx, id, true)
	if !u.DisabledAt.Equal(first) {
		t.Fatal("re-disabling moved the original timestamp")
	}
	if u, _ = s.SetDisabled(ctx, id, false); u.Disabled() {
		t.Fatal("enable did not clear disabled_at")
	}
	if _, err := s.BySub(ctx, "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a non-UUID subject must be ErrNotFound, not a cast error: %v", err)
	}
	if _, err := s.BySub(ctx, "22222222-2222-4222-a222-222222222222"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown subject: %v", err)
	}
	if _, err := s.SetDisabled(ctx, 99999, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}

func mustNotCreate(t *testing.T) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		t.Error("create ran for an invalid invite")
		return "", errors.New("must not run")
	}
}

// TestAnAmbiguousCommitIsDecidedByRereadingTheRow: a COMMIT that errors may
// still have committed. The GoTrue user is deleted only when the re-read row
// shows it is NOT bound — never when it is.
func TestAnAmbiguousCommitIsDecidedByRereadingTheRow(t *testing.T) {
	s, pool := storeFor(t)
	ctx := context.Background()
	lost := errors.New("connection lost during COMMIT")

	// Committed, reply lost.
	if err := s.CreateClaimInvite(ctx, hash("a"), 1, 1, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.commit = func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return lost
	}
	undone := ""
	u, err := s.RedeemClaim(ctx, hash("a"), "a@example.com",
		func(context.Context) (string, error) { return subA, nil }, func(sub string) { undone = sub })
	if err != nil || u.GotrueSub != subA {
		t.Fatalf("a commit that landed must be reported as the success it was: %+v %v", u, err)
	}
	if undone != "" {
		t.Fatalf("the GoTrue user of a COMMITTED binding was deleted (%q): user 1 would be bound to nothing", undone)
	}

	// Not committed.
	if _, err := pool.Exec(authz.System(ctx), `UPDATE users SET gotrue_sub = NULL, email = NULL WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateClaimInvite(ctx, hash("b"), 1, 1, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.commit = func(ctx context.Context, tx pgx.Tx) error {
		_ = tx.Rollback(ctx)
		return lost
	}
	_, err = s.RedeemClaim(ctx, hash("b"), "b@example.com",
		func(context.Context) (string, error) { return subA, nil }, func(sub string) { undone = sub })
	if err == nil || undone != subA {
		t.Fatalf("a commit that did NOT land must undo the GoTrue user: err=%v undone=%q", err, undone)
	}
}
