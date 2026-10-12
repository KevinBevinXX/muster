package github

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
)

// actingOwner is the user whose connection a call reads or writes: the
// principal's user (accounts PR 3b — one GitHub connection PER USER; it was
// the constant 1 until then).
//
// 🔴 A SCOPE-ALL (System) OR PRINCIPAL-LESS CONTEXT HAS NO CONNECTION. It
// refuses rather than guessing a user: under row-level security System would
// see every user's row, and "the" connection would be whichever the query
// returned first.
func actingOwner(ctx context.Context) (int64, error) {
	p, ok := authz.From(ctx)
	if !ok || p.All() || p.UserID <= 0 {
		return 0, errNoOwner
	}
	return p.UserID, nil
}

var errNoOwner = errors.New("github: no acting user to own the connection")

// ownerAAD binds a sealed token to its owner. A ciphertext copied onto
// another user's row fails to open, so the composite row-level boundary
// holds even against a write that bypasses it (the plan's "ciphertext bound
// to owner").
//
// ⚠ THERE IS NO PRE-OWNER (no associated data) FALLBACK, AND THE LIVE TABLE
// HOLDS ONE ROW IT WILL NOT OPEN. The live github_connection table has the
// operator's row, sealed before accounts PR 3b with no associated data. GitHub
// is not enabled on that deployment (no MUSTER_GITHUB_ENCRYPTION_KEY), so no
// store is built and nothing reads it today. If the key is ever set, Get on
// that row returns an error (the Repos panel answers "could not load GitHub
// connection") until it is replaced: Disconnect (Clear needs no decrypt), then
// connect again. A fallback that opened it would be a second, weaker way to
// open a row — the binding exists to refuse exactly that shape.
func ownerAAD(owner int64) []byte {
	return []byte("muster/github_connection/owner=" + strconv.FormatInt(owner, 10))
}

// PGStore persists each user's GitHub connection (one row per owner),
// encrypting the token with the provided 32-byte AES key, bound to the owner.
type PGStore struct {
	pool *pgxpool.Pool
	key  []byte
}

// NewPG constructs a Postgres-backed GitHub-connection Store. key must be 32
// bytes (AES-256).
func NewPG(pool *pgxpool.Pool, key []byte) (*PGStore, error) {
	if len(key) != 32 {
		return nil, errors.New("github: encryption key must be 32 bytes")
	}
	return &PGStore{pool: pool, key: key}, nil
}

// Save upserts the connection, encrypting the token at rest.
func (s *PGStore) Save(ctx context.Context, login, scopes, token string) error {
	owner, err := actingOwner(ctx)
	if err != nil {
		return err
	}
	ct, err := encrypt(s.key, []byte(token), ownerAAD(owner))
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO github_connection (owner_id, login, scopes, token_ciphertext)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (owner_id) DO UPDATE
		SET login=EXCLUDED.login, scopes=EXCLUDED.scopes,
		    token_ciphertext=EXCLUDED.token_ciphertext, updated_at=now()`,
		owner, login, scopes, ct)
	return err
}

// Get returns the connection with its decrypted token, or ok=false if none.
func (s *PGStore) Get(ctx context.Context) (Connection, bool, error) {
	owner, err := actingOwner(ctx)
	if err != nil {
		return Connection{}, false, err
	}
	var (
		c  Connection
		ct []byte
	)
	err = s.pool.QueryRow(ctx, `
		SELECT login, scopes, token_ciphertext, connected_at FROM github_connection WHERE owner_id=$1`, owner).
		Scan(&c.Login, &c.Scopes, &ct, &c.ConnectedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Connection{}, false, nil
		}
		return Connection{}, false, err
	}
	tok, err := decrypt(s.key, ct, ownerAAD(owner))
	if err != nil {
		return Connection{}, false, err
	}
	c.Token = string(tok)
	return c, true, nil
}

// Clear removes the connection.
func (s *PGStore) Clear(ctx context.Context) error {
	owner, err := actingOwner(ctx)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `DELETE FROM github_connection WHERE owner_id=$1`, owner)
	return err
}

var _ Store = (*PGStore)(nil)
