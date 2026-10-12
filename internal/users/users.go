// Package users reads the users table (migration 0006): today only a user's
// ROLE, which the request Principal carries (accounts PR 3b).
//
// 🔴 THE ROLE IS READ, NOT ASSUMED. Until 3b every principal was admin by
// construction (api.roleOf returned admin for everyone). The first thing that
// BRANCHES on the role is the approval relay — only an admin's agents file
// cards on the operator's router — so a second user's agent acting "as admin"
// would put that user's prompts on the operator's phone. The role now comes
// from users.role, which is 'admin' for the seeded operator row (user 1) and
// 'member' for anyone an invite creates.
package users

import (
	"context"
	"errors"
	"fmt"

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

// ErrNotFound reports that no user has the id.
var ErrNotFound = errors.New("users: no such user")

// PGStore is the Postgres Store.
type PGStore struct{ pool *pgxpool.Pool }

// NewPG returns a Store over pool.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

var _ Store = (*PGStore)(nil)

// Role reads users.role. Under row-level security (migration 0009) a user
// principal sees only its own row, so the api package reads it under
// authz.System: it is how a request's role is LEARNED, before any principal
// exists.
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
