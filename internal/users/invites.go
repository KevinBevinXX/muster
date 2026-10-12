package users

// Accounts PR 5: general link invites, the admin's user management, and the
// audit log. Every query runs as authz.System through sys (see the package
// doc): invites are scope-all only under 0009's policies, an admin page reads
// every user, and redemption runs for an anonymous visitor holding a link.
// The CALLER is what restricts these to an admin — internal/api's
// requireAdmin — and every write records WHO in audit_log.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/authz"
)

var (
	// ErrEmailMismatch: the invite is bound to a different email. The
	// invitee's page shows the same "invalid or expired" as every other
	// failure; this error exists so the caller can count it.
	ErrEmailMismatch = errors.New("users: the invite is bound to a different email")
	// ErrEmailTaken: a muster account already has this email.
	ErrEmailTaken = errors.New("users: a muster account already uses this email")
	// ErrLastAdmin: the change would leave no enabled admin.
	ErrLastAdmin = errors.New("users: this is the last enabled admin")
	// ErrNotRevocable: the invite is not pending (redeemed, revoked, or a
	// claim link), or does not exist.
	ErrNotRevocable = errors.New("users: no pending invite with that id")
)

// Actor is who performed an audited action.
type Actor struct {
	UserID int64
	Via    authz.Via
}

// InviteState is an invite's lifecycle state.
type InviteState string

const (
	InvitePending  InviteState = "pending"
	InviteRedeemed InviteState = "redeemed"
	InviteExpired  InviteState = "expired"
	InviteRevoked  InviteState = "revoked"
)

// Invite is one general (non-claim) invite. Its token is never stored; only
// its sha256 is, and that never leaves this package.
type Invite struct {
	ID              int64
	Email           string // "" when the invite is not bound to an email
	Role            authz.Role
	CreatedBy       int64
	CreatedByEmail  string
	CreatedAt       time.Time
	ExpiresAt       time.Time
	RedeemedAt      *time.Time
	RedeemedBy      int64
	RedeemedByEmail string
	RevokedAt       *time.Time
}

// State is the invite's state at now. Redeemed and revoked are final; a
// pending invite past its expiry is expired.
func (i Invite) State(now time.Time) InviteState {
	switch {
	case i.RedeemedAt != nil:
		return InviteRedeemed
	case i.RevokedAt != nil:
		return InviteRevoked
	case !now.Before(i.ExpiresAt):
		return InviteExpired
	}
	return InvitePending
}

// AuditEntry is one audit_log row.
type AuditEntry struct {
	ID     int64
	At     time.Time
	UserID int64
	Via    string
	Action string
	Object string
	Detail map[string]any
}

// auditInsert writes one audit_log row inside tx.
func auditInsert(ctx context.Context, tx pgx.Tx, by Actor, action, object string, detail map[string]any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	var uid any
	if by.UserID > 0 {
		uid = by.UserID
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_log (user_id, via, action, object, detail) VALUES ($1, $2, $3, $4, $5::jsonb)`,
		uid, string(by.Via), action, object, string(b))
	return err
}

// AuditTx writes one audit_log row inside the CALLER's transaction tx, so an
// action and its record commit together. It is the account packages' one
// audit writer: apitokens (accounts PR 6) records token create/revoke through
// it, as the token's owner (0009's self_append policy).
func AuditTx(ctx context.Context, tx pgx.Tx, by Actor, action, object string, detail map[string]any) error {
	return auditInsert(ctx, tx, by, action, object, detail)
}

// inTx runs fn in one System transaction.
func (s *PGStore) inTx(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	ctx = sys(ctx)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Audit appends one audit_log row for an action taken outside this package's
// own transactions (a disable, a reset link).
func (s *PGStore) Audit(ctx context.Context, by Actor, action, object string, detail map[string]any) error {
	return s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return auditInsert(ctx, tx, by, action, object, detail)
	})
}

// Audited runs action fn with its audit row ALREADY WRITTEN in an open
// transaction, and commits the row only if fn succeeded — for an action that
// happens outside the database (a GoTrue call), so the log holds a row for it
// exactly when it happened. If the commit itself fails after fn succeeded, the
// error says so; the caller must then not hand out what fn produced.
func (s *PGStore) Audited(ctx context.Context, by Actor, action, object string, detail map[string]any, fn func() error) error {
	return s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := auditInsert(ctx, tx, by, action, object, detail); err != nil {
			return err
		}
		return fn()
	})
}

// RecentAudit returns the newest audit rows, newest first.
func (s *PGStore) RecentAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := s.pool.Query(sys(ctx), `SELECT id, at, COALESCE(user_id, 0), via, action, object, detail
		FROM audit_log ORDER BY at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var raw []byte
		if err := rows.Scan(&e.ID, &e.At, &e.UserID, &e.Via, &e.Action, &e.Object, &raw); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(raw, &e.Detail)
		out = append(out, e)
	}
	return out, rows.Err()
}

func inviteObject(id int64) string { return "invite:" + strconv.FormatInt(id, 10) }

// UserObject is the audit_log object naming user id.
func UserObject(id int64) string { return "user:" + strconv.FormatInt(id, 10) }

func validRole(r authz.Role) bool { return r == authz.RoleAdmin || r == authz.RoleMember }

// CreateInvite stores a general invite: tokenHash (sha256 of a token the
// caller shows once), an optional bound email, a role and an expiry. A bound
// email that a muster account already uses is refused (ErrEmailTaken).
func (s *PGStore) CreateInvite(ctx context.Context, by Actor, tokenHash []byte, email string, expiresAt time.Time) (Invite, error) {
	// 🔴 AN INVITE ALWAYS MAKES A MEMBER (operator decision D1). There is no
	// role parameter to pass "admin" through: an admin is made only by
	// promoting a redeemed member on /admin/users, which is audited as such.
	role := authz.RoleMember
	if len(tokenHash) != 32 {
		return Invite{}, fmt.Errorf("users: token hash must be 32 bytes, got %d", len(tokenHash))
	}
	email = strings.TrimSpace(email)
	var inv Invite
	err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if email != "" {
			if taken, err := emailTaken(ctx, tx, email); err != nil {
				return err
			} else if taken {
				return ErrEmailTaken
			}
		}
		var em any
		if email != "" {
			em = email
		}
		if err := tx.QueryRow(ctx, `INSERT INTO invites (token_hash, email, role, created_by, expires_at)
			VALUES ($1, $2, $3, $4, $5) RETURNING id, created_at, expires_at`,
			tokenHash, em, string(role), by.UserID, expiresAt).Scan(&inv.ID, &inv.CreatedAt, &inv.ExpiresAt); err != nil {
			return err
		}
		inv.Email, inv.Role, inv.CreatedBy = email, role, by.UserID
		return auditInsert(ctx, tx, by, "invite.create", inviteObject(inv.ID),
			map[string]any{"email": email, "role": string(role), "expires_at": inv.ExpiresAt.UTC().Format(time.RFC3339)})
	})
	return inv, err
}

func emailTaken(ctx context.Context, tx pgx.Tx, email string) (bool, error) {
	var taken bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE lower(email) = lower($1))`, email).Scan(&taken)
	return taken, err
}

const inviteCols = `i.id, COALESCE(i.email, ''), i.role, i.created_by, COALESCE(cu.email, ''), i.created_at, i.expires_at,
	i.redeemed_at, COALESCE(i.redeemed_by, 0), COALESCE(ru.email, ''), i.revoked_at`

const inviteFrom = ` FROM invites i
	LEFT JOIN users cu ON cu.id = i.created_by
	LEFT JOIN users ru ON ru.id = i.redeemed_by`

func scanInvite(row pgx.Row) (Invite, error) {
	var i Invite
	var role string
	if err := row.Scan(&i.ID, &i.Email, &role, &i.CreatedBy, &i.CreatedByEmail, &i.CreatedAt, &i.ExpiresAt,
		&i.RedeemedAt, &i.RedeemedBy, &i.RedeemedByEmail, &i.RevokedAt); err != nil {
		return Invite{}, err
	}
	i.Role = authz.Role(role)
	return i, nil
}

// ListInvites returns the general invites (not admin-claim links), newest
// first.
func (s *PGStore) ListInvites(ctx context.Context) ([]Invite, error) {
	rows, err := s.pool.Query(sys(ctx), `SELECT `+inviteCols+inviteFrom+`
		WHERE i.claim_user_id IS NULL ORDER BY i.created_at DESC, i.id DESC LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		i, err := scanInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// RevokeInvite revokes a PENDING general invite. Anything else — redeemed,
// already revoked, a claim link, no such id — is ErrNotRevocable. An expired
// invite may still be revoked (it changes nothing but the record).
func (s *PGStore) RevokeInvite(ctx context.Context, by Actor, id int64) error {
	return s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE invites SET revoked_at = now()
			WHERE id = $1 AND claim_user_id IS NULL AND redeemed_at IS NULL AND revoked_at IS NULL`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrNotRevocable
		}
		return auditInsert(ctx, tx, by, "invite.revoke", inviteObject(id), nil)
	})
}

// liveInviteWhere is what makes a general invite redeemable. ONE predicate,
// used by OpenInvite (the form) and RedeemInvite (under the lock).
//
// 🔴 AN INVITE IS ITS CREATOR'S ADMIN AUTHORITY, DELEGATED, AND DIES WITH IT.
// Disabling or demoting an admin revokes their pending invites in the same
// transaction (revokePendingInvitesOf); the creator clause here is the belt
// to that brace — an invite whose creator is not, right now, an enabled admin
// is not live, however the creator's row came to change
// (TestAnInviteDiesWithItsCreatorsAdminRights). cu is inviteFrom's creator join.
const liveInviteWhere = `i.token_hash = $1 AND i.claim_user_id IS NULL
	AND i.redeemed_at IS NULL AND i.revoked_at IS NULL AND i.expires_at > now()
	AND cu.role = 'admin' AND cu.disabled_at IS NULL`

// revokePendingInvitesOf revokes every pending general invite creator made,
// inside tx, with one invite.revoke audit row each (actor by, detail reason).
// Called by a disable and by a demotion from admin.
func revokePendingInvitesOf(ctx context.Context, tx pgx.Tx, by Actor, creator int64, reason string) error {
	rows, err := tx.Query(ctx, `UPDATE invites SET revoked_at = clock_timestamp()
		WHERE created_by = $1 AND claim_user_id IS NULL AND redeemed_at IS NULL AND revoked_at IS NULL
		RETURNING id`, creator)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := auditInsert(ctx, tx, by, "invite.revoke", inviteObject(id),
			map[string]any{"reason": reason, "creator": creator}); err != nil {
			return err
		}
	}
	return nil
}

// OpenInvite returns the live general invite tokenHash names, for rendering
// the invitee's form (its bound email). ErrInvalidInvite for every other case.
// RedeemInvite re-checks under a lock.
func (s *PGStore) OpenInvite(ctx context.Context, tokenHash []byte) (Invite, error) {
	i, err := scanInvite(s.pool.QueryRow(sys(ctx), `SELECT `+inviteCols+inviteFrom+` WHERE `+liveInviteWhere, tokenHash))
	if errors.Is(err, pgx.ErrNoRows) {
		return Invite{}, ErrInvalidInvite
	}
	return i, err
}

// RedeemInvite redeems a general invite in ONE transaction: lock the invite
// row (FOR UPDATE), check the bound email, create the GoTrue user, insert the
// users row — ALWAYS a member, whatever the invite row says (D1) — mark the
// invite redeemed, write the audit row, commit.
//
// 🔴 THE INVITE ROW LOCK IS WHAT MAKES THE LINK SINGLE-USE UNDER CONCURRENCY.
// Two redemptions of one token serialize on it; the second re-evaluates the
// live predicate after the first commits, finds redeemed_at set, and gets
// ErrInvalidInvite having never called create — so GoTrue gets exactly one
// user (TestAnInviteLinkIsSingleUseUnderConcurrency).
//
// create makes the GoTrue user and returns its subject. undo(sub) is called
// when the muster side is known NOT to have committed (commitDeciding), so a
// GoTrue user is not left behind with no muster row bound to it.
func (s *PGStore) RedeemInvite(ctx context.Context, tokenHash []byte, email, displayName string,
	create func(context.Context) (string, error), undo func(sub string)) (User, error) {
	ctx = sys(ctx)
	email, displayName = strings.TrimSpace(email), strings.TrimSpace(displayName)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	inv, err := scanInvite(tx.QueryRow(ctx, `SELECT `+inviteCols+inviteFrom+` WHERE `+liveInviteWhere+` FOR UPDATE OF i`, tokenHash))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrInvalidInvite
	}
	if err != nil {
		return User{}, err
	}
	if inv.Email != "" {
		if !strings.EqualFold(inv.Email, email) {
			return User{}, ErrEmailMismatch
		}
		email = inv.Email
	}
	if taken, err := emailTaken(ctx, tx, email); err != nil {
		return User{}, err
	} else if taken {
		return User{}, ErrEmailTaken
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
	u, err := scanUser(tx.QueryRow(ctx, `INSERT INTO users (gotrue_sub, email, display_name, role)
		VALUES ($1::uuid, $2, $3, 'member') RETURNING `+userCols, sub, email, displayName))
	if err != nil {
		return User{}, err
	}
	// Belt to the row lock's braces: the UPDATE states the precondition it
	// relies on, and anything but exactly one row is a refusal.
	tag, err := tx.Exec(ctx, `UPDATE invites SET redeemed_at = now(), redeemed_by = $2
		WHERE id = $1 AND redeemed_at IS NULL AND revoked_at IS NULL`, inv.ID, u.ID)
	if err != nil {
		return User{}, err
	}
	if tag.RowsAffected() != 1 {
		return User{}, ErrInvalidInvite
	}
	if err := auditInsert(ctx, tx, Actor{UserID: u.ID, Via: "invite"}, "invite.redeem", inviteObject(inv.ID),
		map[string]any{"user_id": u.ID, "role": string(authz.RoleMember)}); err != nil {
		return User{}, err
	}
	committed, err = s.commitDeciding(ctx, tx, sub, func(rctx context.Context) (bool, error) {
		var n int
		err := s.pool.QueryRow(rctx, `SELECT count(*) FROM users WHERE gotrue_sub = $1::uuid`, sub).Scan(&n)
		return n == 1, err
	})
	if err != nil {
		return User{}, err
	}
	return u, nil
}

// ListUsers returns every user, oldest first.
func (s *PGStore) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.pool.Query(sys(ctx), `SELECT `+userCols+` FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// guardLastAdmin refuses (ErrLastAdmin) when id is an enabled admin and the
// only one. It locks EVERY enabled admin row first, so two admins demoting or
// disabling each other at once serialize here and the second sees the first's
// change — otherwise each would see the other still enabled and both commit.
func guardLastAdmin(ctx context.Context, tx pgx.Tx, id int64) error {
	rows, err := tx.Query(ctx, `SELECT id FROM users WHERE role = 'admin' AND disabled_at IS NULL ORDER BY id FOR UPDATE`)
	if err != nil {
		return err
	}
	var admins []int64
	for rows.Next() {
		var a int64
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			return err
		}
		admins = append(admins, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(admins) == 1 && admins[0] == id {
		return ErrLastAdmin
	}
	return nil
}

// SetRole changes user id's role. Demoting the last enabled admin is refused
// (ErrLastAdmin). The change is audited with its before and after.
func (s *PGStore) SetRole(ctx context.Context, by Actor, id int64, role authz.Role) (User, error) {
	if !validRole(role) {
		return User{}, fmt.Errorf("users: unknown role %q", role)
	}
	var u User
	err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if role != authz.RoleAdmin {
			if err := guardLastAdmin(ctx, tx, id); err != nil {
				return err
			}
		}
		var before string
		if err := tx.QueryRow(ctx, `SELECT role FROM users WHERE id = $1 FOR UPDATE`, id).Scan(&before); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		var err error
		u, err = scanUser(tx.QueryRow(ctx, `UPDATE users SET role = $2 WHERE id = $1 RETURNING `+userCols, id, string(role)))
		if err != nil {
			return err
		}
		if before == string(authz.RoleAdmin) && role != authz.RoleAdmin {
			if err := revokePendingInvitesOf(ctx, tx, by, id, "creator demoted"); err != nil {
				return err
			}
		}
		return auditInsert(ctx, tx, by, "user.role", UserObject(id), map[string]any{"from": before, "to": string(role)})
	})
	return u, err
}
