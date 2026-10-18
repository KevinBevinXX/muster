package uploads

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
)

// Store is the uploads table (migration 0024).
//
// 🔴 EVERY METHOD BUT LinkTarget RUNS UNDER THE CALLER'S PRINCIPAL. The table is
// under FORCE row-level security, so a request creates, reads and revokes only
// its own user's rows, and the owner of a new row is the principal's user
// (owner_id's default), never a parameter.
type Store interface {
	Create(ctx context.Context, u Upload) (Upload, error)
	// Get is the caller's upload by public id; ErrNotFound otherwise.
	Get(ctx context.Context, publicID string) (Upload, error)
	// Revoke marks the caller's upload revoked and returns it (its object key
	// is then deleted by the caller). Revoking twice is not an error.
	Revoke(ctx context.Context, publicID string) (Upload, error)
	// LinkTarget is the upload a signed link names, read with NO principal:
	// the link's signature has already been checked, and it is how the
	// request learns anything at all. Only what serving needs is handed on.
	LinkTarget(ctx context.Context, publicID string) (Upload, error)
}

// PGStore is Store on Postgres.
type PGStore struct{ pool *pgxpool.Pool }

// NewPG returns a Store on pool.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

// sys is this package's ONE authz.System call (ledgered in
// internal/authz/ledger_test.go): LinkTarget's read of one row by its public
// id, made only after the link's HMAC has been verified.
func sys(ctx context.Context) context.Context { return authz.System(ctx) }

const uploadCols = `id, owner_id, public_id, object_key, filename, content_type, size_bytes,
	created_at, expires_at, revoked_at`

func scanUpload(row pgx.Row) (Upload, error) {
	var u Upload
	err := row.Scan(&u.ID, &u.OwnerID, &u.PublicID, &u.ObjectKey, &u.Filename, &u.ContentType, &u.Size,
		&u.CreatedAt, &u.ExpiresAt, &u.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Upload{}, ErrNotFound
	}
	return u, err
}

// Create inserts u (its ID, OwnerID, CreatedAt and RevokedAt are ignored).
func (s *PGStore) Create(ctx context.Context, u Upload) (Upload, error) {
	out, err := scanUpload(s.pool.QueryRow(ctx, `INSERT INTO uploads
		(public_id, object_key, filename, content_type, size_bytes, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+uploadCols,
		u.PublicID, u.ObjectKey, u.Filename, u.ContentType, u.Size, u.ExpiresAt.UTC()))
	if err != nil {
		return Upload{}, fmt.Errorf("uploads: create: %w", err)
	}
	return out, nil
}

// Get is the caller's upload by public id.
func (s *PGStore) Get(ctx context.Context, publicID string) (Upload, error) {
	if !ValidPublicID(publicID) {
		return Upload{}, ErrNotFound
	}
	return scanUpload(s.pool.QueryRow(ctx, `SELECT `+uploadCols+` FROM uploads WHERE public_id = $1`, publicID))
}

// Revoke marks the caller's upload revoked (first revocation's time kept).
func (s *PGStore) Revoke(ctx context.Context, publicID string) (Upload, error) {
	if !ValidPublicID(publicID) {
		return Upload{}, ErrNotFound
	}
	return scanUpload(s.pool.QueryRow(ctx, `UPDATE uploads SET revoked_at = coalesce(revoked_at, now())
		WHERE public_id = $1 RETURNING `+uploadCols, publicID))
}

// LinkTarget reads one row by public id under the system principal.
func (s *PGStore) LinkTarget(ctx context.Context, publicID string) (Upload, error) {
	if !ValidPublicID(publicID) {
		return Upload{}, ErrNotFound
	}
	return scanUpload(s.pool.QueryRow(sys(ctx), `SELECT `+uploadCols+` FROM uploads WHERE public_id = $1`, publicID))
}
