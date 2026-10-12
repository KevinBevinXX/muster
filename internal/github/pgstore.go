package github

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// operatorOwnerID is the owner every PGStore call reads and writes.
//
// ⚠ TEMPORARY, AND DELIBERATELY A CONSTANT RATHER THAN A PARAMETER. Migration
// 0006 keyed github_connection by owner_id (it used to be an id = 1 singleton)
// and backfilled the existing connection to user 1, the operator. Until the
// request principal exists (accounts PR 2/3) there is exactly one user, so this
// store keeps today's single-connection behaviour byte for byte by always
// naming owner 1. Accounts PR 3b replaces it with the principal's user id.
const operatorOwnerID int64 = 1

// PGStore persists the operator's GitHub connection, encrypting the token with
// the provided 32-byte AES key.
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
	ct, err := encrypt(s.key, []byte(token))
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO github_connection (owner_id, login, scopes, token_ciphertext)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (owner_id) DO UPDATE
		SET login=EXCLUDED.login, scopes=EXCLUDED.scopes,
		    token_ciphertext=EXCLUDED.token_ciphertext, updated_at=now()`,
		operatorOwnerID, login, scopes, ct)
	return err
}

// Get returns the connection with its decrypted token, or ok=false if none.
func (s *PGStore) Get(ctx context.Context) (Connection, bool, error) {
	var (
		c  Connection
		ct []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT login, scopes, token_ciphertext, connected_at FROM github_connection WHERE owner_id=$1`, operatorOwnerID).
		Scan(&c.Login, &c.Scopes, &ct, &c.ConnectedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Connection{}, false, nil
		}
		return Connection{}, false, err
	}
	tok, err := decrypt(s.key, ct)
	if err != nil {
		return Connection{}, false, err
	}
	c.Token = string(tok)
	return c, true, nil
}

// Clear removes the connection.
func (s *PGStore) Clear(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM github_connection WHERE owner_id=$1`, operatorOwnerID)
	return err
}

var _ Store = (*PGStore)(nil)
