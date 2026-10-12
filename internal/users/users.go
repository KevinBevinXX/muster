// Package users is muster's account store: the `users` and `invites` tables
// (migration 0006). Accounts PR 4 reads users by GoTrue subject, disables
// them, and runs the ONE invite flow this PR ships — the admin bootstrap claim
// that binds a new GoTrue user to users.id = 1.
//
// 🔴 NEITHER TABLE IS UNDER ROW-LEVEL SECURITY, AND EVERY QUERY HERE RUNS AS
// authz.System. The lookups in this package are how a request LEARNS who it
// is (a token's `sub` → a user row), so they cannot run as that user; and the
// claim runs for an anonymous visitor holding a link. System (scope all)
// states that instead of leaving the queries principal-less, which internal/db
// would log as an unscoped read. sys is the one call site, ledgered in
// internal/authz/ledger_test.go.
package users

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
)

// User is one muster account.
type User struct {
	ID          int64
	GotrueSub   string // "" until claimed
	Email       string
	DisplayName string
	Role        authz.Role
	DisabledAt  *time.Time
}

// Disabled reports whether the account is disabled.
func (u User) Disabled() bool { return u.DisabledAt != nil }

var (
	// ErrNotFound: no such user.
	ErrNotFound = errors.New("users: no such user")
	// ErrInvalidInvite: the token names no live claim invite (unknown,
	// expired, revoked, already redeemed, or its user is already claimed).
	// Deliberately ONE error, so the page a visitor sees cannot tell the cases
	// apart.
	ErrInvalidInvite = errors.New("users: invalid or expired invite")
	// ErrAlreadyClaimed: the user already has a GoTrue subject.
	ErrAlreadyClaimed = errors.New("users: this user is already bound to a login")
)

// PG is the Postgres store.
type PG struct {
	pool *pgxpool.Pool
	// commit commits a redeem transaction. A field so a test can make the
	// COMMIT's outcome ambiguous; nil means tx.Commit.
	commit func(context.Context, pgx.Tx) error
}

// NewPG constructs the store.
func NewPG(pool *pgxpool.Pool) *PG { return &PG{pool: pool} }

// sys is the store's one authz.System call. See the package doc.
func sys(ctx context.Context) context.Context { return authz.System(ctx) }

const userCols = `id, COALESCE(gotrue_sub::text, ''), COALESCE(email, ''), display_name, role, disabled_at`

func scanUser(row pgx.Row) (User, error) {
	var u User
	var role string
	if err := row.Scan(&u.ID, &u.GotrueSub, &u.Email, &u.DisplayName, &role, &u.DisabledAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, err
	}
	u.Role = authz.Role(role)
	return u, nil
}

// BySub returns the user bound to GoTrue subject sub.
func (s *PG) BySub(ctx context.Context, sub string) (User, error) {
	if !looksLikeUUID(sub) {
		// gotrue_sub is a UUID column; a non-UUID sub would be a cast error,
		// which must read as "no such user", not as a 500.
		return User{}, ErrNotFound
	}
	return scanUser(s.pool.QueryRow(sys(ctx), `SELECT `+userCols+` FROM users WHERE gotrue_sub = $1::uuid`, sub))
}

// ByID returns user id.
func (s *PG) ByID(ctx context.Context, id int64) (User, error) {
	return scanUser(s.pool.QueryRow(sys(ctx), `SELECT `+userCols+` FROM users WHERE id = $1`, id))
}

// SetDisabled sets (or clears) disabled_at and returns the updated row.
// Disabling an already-disabled user keeps its original timestamp.
func (s *PG) SetDisabled(ctx context.Context, id int64, disabled bool) (User, error) {
	q := `UPDATE users SET disabled_at = COALESCE(disabled_at, now()) WHERE id = $1 RETURNING ` + userCols
	if !disabled {
		q = `UPDATE users SET disabled_at = NULL WHERE id = $1 RETURNING ` + userCols
	}
	return scanUser(s.pool.QueryRow(sys(ctx), q, id))
}

// CreateClaimInvite stores a single-use claim invite binding a future GoTrue
// user to claimUserID, and REVOKES every earlier unredeemed claim invite for
// that user — so exactly one claim link is live at a time.
//
// It refuses (ErrAlreadyClaimed) when the user is already bound: re-binding
// the admin to a different login is not what a bootstrap is for.
func (s *PG) CreateClaimInvite(ctx context.Context, tokenHash []byte, claimUserID, createdBy int64, expiresAt time.Time) error {
	if len(tokenHash) != 32 {
		return fmt.Errorf("users: token hash must be 32 bytes, got %d", len(tokenHash))
	}
	ctx = sys(ctx)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var unbound bool
	if err := tx.QueryRow(ctx, `SELECT gotrue_sub IS NULL FROM users WHERE id = $1 FOR UPDATE`, claimUserID).Scan(&unbound); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if !unbound {
		return ErrAlreadyClaimed
	}
	if _, err := tx.Exec(ctx, `UPDATE invites SET revoked_at = now()
		WHERE claim_user_id = $1 AND redeemed_at IS NULL AND revoked_at IS NULL`, claimUserID); err != nil {
		return err
	}
	var role string
	if err := tx.QueryRow(ctx, `SELECT role FROM users WHERE id = $1`, claimUserID).Scan(&role); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO invites (token_hash, role, created_by, expires_at, claim_user_id)
		VALUES ($1, $2, $3, $4, $5)`, tokenHash, role, createdBy, expiresAt, claimUserID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ClaimInviteLive reports whether tokenHash names a live claim invite. It is
// for rendering the form; RedeemClaim re-checks under a lock.
func (s *PG) ClaimInviteLive(ctx context.Context, tokenHash []byte) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(sys(ctx), `SELECT EXISTS (
		SELECT 1 FROM invites i JOIN users u ON u.id = i.claim_user_id
		WHERE i.token_hash = $1 AND i.redeemed_at IS NULL AND i.revoked_at IS NULL
		  AND i.expires_at > now() AND u.gotrue_sub IS NULL)`, tokenHash).Scan(&ok)
	return ok, err
}

// RedeemClaim redeems a claim invite in ONE transaction.
//
// 🔴 THE INVITE ROW IS LOCKED (FOR UPDATE) BEFORE create RUNS, AND THAT IS WHAT
// MAKES THE LINK SINGLE-USE UNDER CONCURRENCY. Two redemptions of one token
// serialize on the lock; the second sees redeemed_at set and gets
// ErrInvalidInvite, having never called create — so GoTrue gets exactly one
// user (TestTheClaimLinkIsSingleUseUnderConcurrency).
//
// ⚠ THE users ROW IS LOCKED TOO, AND FOR A CLAIM EITHER LOCK ALONE SUFFICES —
// measured by mutation: removing one leaves that test green, removing both
// turns it red. The users lock is what stops two DIFFERENT links bound to one
// user both binding it; the invite lock is what single-use rests on for an
// invite with no claim user (accounts PR 5). Neither is decoration.
//
// create makes the GoTrue user and returns its subject. If anything after it
// fails, undo(sub) is called so a GoTrue user is not left behind with no muster
// row bound to it.
func (s *PG) RedeemClaim(ctx context.Context, tokenHash []byte, email string,
	create func(context.Context) (string, error), undo func(sub string)) (User, error) {
	ctx = sys(ctx)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var inviteID, uid int64
	err = tx.QueryRow(ctx, `SELECT id, claim_user_id FROM invites
		WHERE token_hash = $1 AND claim_user_id IS NOT NULL
		  AND redeemed_at IS NULL AND revoked_at IS NULL AND expires_at > now()
		FOR UPDATE`, tokenHash).Scan(&inviteID, &uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrInvalidInvite
	}
	if err != nil {
		return User{}, err
	}
	var unbound bool
	if err := tx.QueryRow(ctx, `SELECT gotrue_sub IS NULL FROM users WHERE id = $1 FOR UPDATE`, uid).Scan(&unbound); err != nil {
		return User{}, err
	}
	if !unbound {
		return User{}, ErrInvalidInvite
	}
	sub, err := create(ctx)
	if err != nil {
		return User{}, err
	}
	committed := false
	defer func() {
		if !committed && undo != nil {
			undo(sub)
		}
	}()
	u, err := scanUser(tx.QueryRow(ctx, `UPDATE users SET gotrue_sub = $2::uuid, email = $3
		WHERE id = $1 AND gotrue_sub IS NULL RETURNING `+userCols, uid, sub, strings.TrimSpace(email)))
	if err != nil {
		return User{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE invites SET redeemed_at = now(), redeemed_by = $2 WHERE id = $1`, inviteID, uid); err != nil {
		return User{}, err
	}
	commit := s.commit
	if commit == nil {
		commit = func(ctx context.Context, tx pgx.Tx) error { return tx.Commit(ctx) }
	}
	if err := commit(ctx, tx); err != nil {
		// 🔴 A FAILED COMMIT IS AN UNKNOWN OUTCOME, NOT A ROLLBACK. The server
		// may have committed and the reply been lost; deleting the GoTrue user
		// then would leave user 1 bound to a subject that no longer exists —
		// the admin locked out. Re-read the row and decide from what is there
		// (TestAnAmbiguousCommitIsDecidedByRereadingTheRow).
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		var bound string
		rerr := s.pool.QueryRow(rctx, `SELECT COALESCE(gotrue_sub::text, '') FROM users WHERE id = $1`, uid).Scan(&bound)
		switch {
		case rerr == nil && bound == sub:
			committed = true
			return u, nil
		case rerr != nil:
			// Still unknown: keep the GoTrue user (an orphan login is
			// recoverable; a deleted bound one is not) and say so.
			committed = true
			return User{}, fmt.Errorf("users: claim commit outcome unknown (%v; re-read failed: %v) — the GoTrue user %s was kept", err, rerr, sub)
		}
		return User{}, err
	}
	committed = true
	return u, nil
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return false
			}
		}
	}
	return true
}
