// Package users is muster's account store: the `users` and `invites` tables
// (migration 0006). It answers a user's ROLE for the request Principal
// (accounts PR 3b) and, for accounts PR 4, reads users by GoTrue subject,
// disables them, and runs the admin bootstrap claim that binds a new GoTrue
// user to users.id = 1. Accounts PR 5 adds general link invites, the admin's
// user management (role, last-admin guard) and the audit log (invites.go).
//
// 🔴 THE ROLE IS READ, NOT ASSUMED, AND IT IS READ IN ONE PLACE: Role, which
// internal/api's roleOf calls for every principal (failure → member). Nothing
// else in this package returns a role — the User row deliberately has no Role
// field, so a second, divergent role read cannot creep in beside roleOf.
//
// 🔴 BOTH TABLES ARE UNDER FORCE ROW-LEVEL SECURITY (migration 0009): a user
// principal may SELECT only its own users row, and invites are reachable only
// at scope 'all'. Every query here except Role therefore runs as authz.System,
// through the one helper sys (ledgered in internal/authz/ledger_test.go): these
// lookups are how a request LEARNS who it is (a token's sub → users row), or
// they run for an anonymous claim-link visitor — there is no principal to run
// as yet. Role takes the caller's context; api.roleOf hands it System for the
// same reason.
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

// Store answers a user's role.
type Store interface {
	// Role returns the role of user id. ErrNotFound when there is no such
	// user. An unknown role value is an error, never a guess.
	Role(ctx context.Context, id int64) (authz.Role, error)
}

// User is one muster account, as the session tier needs it. No Role: see the
// package doc.
type User struct {
	ID          int64
	GotrueSub   string // "" until claimed
	Email       string
	DisplayName string
	DisabledAt  *time.Time
	// SessionsNotBefore is the session cutoff (migration 0010): a token issued
	// at or before it is refused. Set by every disable and every password
	// reset, never cleared.
	SessionsNotBefore *time.Time
}

// Disabled reports whether the account is disabled.
func (u User) Disabled() bool { return u.DisabledAt != nil }

var (
	// ErrNotFound reports that no user has the id (or subject).
	ErrNotFound = errors.New("users: no such user")
	// ErrInvalidInvite: the token names no live claim invite (unknown,
	// expired, revoked, already redeemed, or its user is already claimed).
	// Deliberately ONE error, so the page a visitor sees cannot tell the cases
	// apart.
	ErrInvalidInvite = errors.New("users: invalid or expired invite")
	// ErrAlreadyClaimed: the user already has a GoTrue subject.
	ErrAlreadyClaimed = errors.New("users: this user is already bound to a login")
)

// PGStore is the Postgres store.
type PGStore struct {
	pool *pgxpool.Pool
	// commit commits a redeem transaction. A field so a test can make the
	// COMMIT's outcome ambiguous; nil means tx.Commit.
	commit func(context.Context, pgx.Tx) error
}

// NewPG returns the store over pool.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

var _ Store = (*PGStore)(nil)

// Role reads users.role. Under row-level security a user principal sees only
// its own row, so the api package reads it under authz.System: it is how a
// request's role is LEARNED, before any principal exists.
func (s *PGStore) Role(ctx context.Context, id int64) (authz.Role, error) {
	var role string
	err := s.pool.QueryRow(ctx, `SELECT role FROM users WHERE id=$1`, id).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	switch r := authz.Role(role); r {
	case authz.RoleAdmin, authz.RoleMember:
		return r, nil
	default:
		return "", fmt.Errorf("users: user %d has unknown role %q", id, role)
	}
}

// sys is the store's one authz.System call. See the package doc.
func sys(ctx context.Context) context.Context { return authz.System(ctx) }

const userCols = `id, COALESCE(gotrue_sub::text, ''), COALESCE(email, ''), display_name, disabled_at, sessions_not_before`

func scanUser(row pgx.Row) (User, error) {
	var u User
	if err := row.Scan(&u.ID, &u.GotrueSub, &u.Email, &u.DisplayName, &u.DisabledAt, &u.SessionsNotBefore); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, err
	}
	return u, nil
}

// BySub returns the user bound to GoTrue subject sub.
func (s *PGStore) BySub(ctx context.Context, sub string) (User, error) {
	if !looksLikeUUID(sub) {
		// gotrue_sub is a UUID column; a non-UUID sub would be a cast error,
		// which must read as "no such user", not as a 500.
		return User{}, ErrNotFound
	}
	return scanUser(s.pool.QueryRow(sys(ctx), `SELECT `+userCols+` FROM users WHERE gotrue_sub = $1::uuid`, sub))
}

// ByID returns user id.
func (s *PGStore) ByID(ctx context.Context, id int64) (User, error) {
	return scanUser(s.pool.QueryRow(sys(ctx), `SELECT `+userCols+` FROM users WHERE id = $1`, id))
}

// SetDisabled sets (or clears) disabled_at and returns the updated row.
// Disabling an already-disabled user keeps its original timestamp.
//
// 🔴 DISABLING THE LAST ENABLED ADMIN IS REFUSED (ErrLastAdmin), for the reason
// SetRole refuses demoting it: nobody would be left who can reach /admin to undo
// it. The check runs under guardLastAdmin's lock (accounts PR 5).
//
// 🔴 A DISABLE ALSO MOVES sessions_not_before TO NOW, AND AN ENABLE LEAVES IT.
// A GoTrue unban resurrects every pre-ban session (migration 0010), so the
// session tier's cutoff is what keeps a disable from being undone by an enable.
//
// 🔴 A DISABLE ALSO REVOKES EVERY PERSONAL API TOKEN THE USER HOLDS, in the
// same transaction (SetDisabledAs → revokeAPITokensTx; accounts PR 6).
// Without it, re-enabling the user brought every old token back to life —
// including one that leaked, which is often WHY the user was disabled. An
// enable revokes nothing and restores nothing: the user mints new tokens.
func (s *PGStore) SetDisabled(ctx context.Context, id int64, disabled bool) (User, error) {
	return s.SetDisabledAs(ctx, nil, id, disabled)
}

// BumpSessionCutoff moves user id's session cutoff to clock_timestamp() and
// returns it: every token issued at or before it is refused from now on
// (a password reset; TestAResetEndsOtherDevicesAccessTokensAtOnce).
func (s *PGStore) BumpSessionCutoff(ctx context.Context, id int64) (time.Time, error) {
	var t time.Time
	err := s.pool.QueryRow(sys(ctx), `UPDATE users SET sessions_not_before = clock_timestamp() WHERE id = $1
		RETURNING sessions_not_before`, id).Scan(&t)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

// SetDisabledAs is SetDisabled that, when by is non-nil, writes the
// user.disable / user.enable audit row in the SAME transaction as the change.
// A disable also revokes the user's pending invites, in that transaction.
func (s *PGStore) SetDisabledAs(ctx context.Context, by *Actor, id int64, disabled bool) (User, error) {
	ctx = sys(ctx)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := `UPDATE users SET disabled_at = NULL WHERE id = $1 RETURNING ` + userCols
	action := "user.enable"
	if disabled {
		if err := guardLastAdmin(ctx, tx, id); err != nil {
			return User{}, err
		}
		// 🔴 clock_timestamp(), NOT now(): now() is the TRANSACTION's start,
		// taken before guardLastAdmin waited for its lock, so a token minted
		// during that wait would have been issued after the cutoff and
		// survived (TestTheDisableCutoffIsStampedAfterTheLockWait). No extra
		// margin: the session tier already refuses a token stamped with the
		// cutoff's own second (iat is whole seconds), and Postgres and GoTrue
		// share the cluster's NTP time; skew beyond a second is not
		// compensated.
		q = `UPDATE users SET disabled_at = COALESCE(disabled_at, clock_timestamp()), sessions_not_before = clock_timestamp()
			WHERE id = $1 RETURNING ` + userCols
		action = "user.disable"
	}
	u, err := scanUser(tx.QueryRow(ctx, q, id))
	if err != nil {
		return User{}, err
	}
	if disabled {
		actor := Actor{Via: "system"}
		if by != nil {
			actor = *by
		}
		if err := revokePendingInvitesOf(ctx, tx, actor, id, "creator disabled"); err != nil {
			return User{}, err
		}
		// Accounts PR 6: every live personal API token, in THIS transaction.
		if err := revokeAPITokensTx(ctx, tx, id); err != nil {
			return User{}, err
		}
	}
	if by != nil {
		if err := auditInsert(ctx, tx, *by, action, UserObject(id), nil); err != nil {
			return User{}, err
		}
	}
	return u, tx.Commit(ctx)
}

// revokeAPITokensTx revokes every live personal API token (accounts PR 6) of
// user id inside tx. EVERY disable path must call it in the disable's own
// transaction — it is one helper so a second disable writer is one call.
func revokeAPITokensTx(ctx context.Context, tx pgx.Tx, id int64) error {
	_, err := tx.Exec(ctx, `UPDATE api_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, id)
	return err
}

// IDByEmail returns the id of the user whose email is email (case-folded), and
// whether there is one. The sign-in limiter keys a KNOWN account's backoff on
// this id (internal/api/signin_limiter.go).
func (s *PGStore) IDByEmail(ctx context.Context, email string) (int64, bool, error) {
	var id int64
	err := s.pool.QueryRow(sys(ctx), `SELECT id FROM users WHERE lower(email) = lower($1)`, strings.TrimSpace(email)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// DisabledAmong reports, for each of ids that exists, whether it is disabled,
// in one query. An id with no row is absent from the result.
func (s *PGStore) DisabledAmong(ctx context.Context, ids []int64) (map[int64]bool, error) {
	rows, err := s.pool.Query(sys(ctx), `SELECT id, disabled_at IS NOT NULL FROM users WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]bool, len(ids))
	for rows.Next() {
		var id int64
		var disabled bool
		if err := rows.Scan(&id, &disabled); err != nil {
			return nil, err
		}
		out[id] = disabled
	}
	return out, rows.Err()
}

// CreateClaimInvite stores a single-use claim invite binding a future GoTrue
// user to claimUserID, and REVOKES every earlier unredeemed claim invite for
// that user — so exactly one claim link is live at a time.
//
// It refuses (ErrAlreadyClaimed) when the user is already bound: re-binding
// the admin to a different login is not what a bootstrap is for.
func (s *PGStore) CreateClaimInvite(ctx context.Context, tokenHash []byte, claimUserID, createdBy int64, expiresAt time.Time) error {
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
func (s *PGStore) ClaimInviteLive(ctx context.Context, tokenHash []byte) (bool, error) {
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
func (s *PGStore) RedeemClaim(ctx context.Context, tokenHash []byte, email string,
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
	committed, err = s.commitDeciding(ctx, tx, sub, func(rctx context.Context) (bool, error) {
		var bound string
		err := s.pool.QueryRow(rctx, `SELECT COALESCE(gotrue_sub::text, '') FROM users WHERE id = $1`, uid).Scan(&bound)
		return bound == sub, err
	})
	if err != nil {
		return User{}, err
	}
	return u, nil
}

// commitDeciding commits a redeem transaction that has already created GoTrue
// user sub, and reports whether that user must be KEPT (true) or undone.
//
// 🔴 A FAILED COMMIT IS AN UNKNOWN OUTCOME, NOT A ROLLBACK. The server may have
// committed and the reply been lost; deleting the GoTrue user then would leave
// a muster row bound to a subject that no longer exists — that account locked
// out. So landed re-reads the row and the outcome is decided from what is there
// (TestAnAmbiguousCommitIsDecidedByRereadingTheRow): bound → kept, success;
// not bound → undo, the commit's error; re-read failed → still unknown, so the
// GoTrue user is kept (an orphan login is recoverable, a deleted bound one is
// not) and the error says so. Shared by the admin claim and invite redemption.
func (s *PGStore) commitDeciding(ctx context.Context, tx pgx.Tx, sub string, landed func(context.Context) (bool, error)) (bool, error) {
	commit := s.commit
	if commit == nil {
		commit = func(ctx context.Context, tx pgx.Tx) error { return tx.Commit(ctx) }
	}
	err := commit(ctx, tx)
	if err == nil {
		return true, nil
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	ok, rerr := landed(rctx)
	switch {
	case rerr == nil && ok:
		return true, nil
	case rerr != nil:
		return true, fmt.Errorf("users: commit outcome unknown (%v; re-read failed: %v) — the GoTrue user %s was kept", err, rerr, sub)
	}
	return false, err
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
