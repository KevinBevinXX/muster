// Package pins is the Agents list's pinned entries (migration 0017): per user,
// server-stored, so a pin follows its owner across devices. A pin names either
// a dispatched agent or a mirrored tmux window.
//
// 🔴 EVERY METHOD RUNS UNDER THE CALLER'S PRINCIPAL. pins is under FORCE
// row-level security, so a request sees, adds and removes only its own user's
// rows; there is no system method. The owner of a new row is the principal's
// user (owner_id's default), never a parameter.
package pins

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/mirror"
)

// MirrorKey is a mirrored entry's address — the one its card links by
// (ui.MirrorRef): host, tmux session, and tmux window id ("@12"), "" for a
// per-session host's entry. Never the window's index, which tmux renumbers.
type MirrorKey struct {
	Host    string
	Session string
	Window  string
}

// Valid reports whether k is an address the mirror wire can carry.
func (k MirrorKey) Valid() error {
	if err := mirror.ValidHost(k.Host); err != nil {
		return err
	}
	if err := mirror.ValidSession(k.Session); err != nil {
		return err
	}
	if k.Window != "" {
		return mirror.ValidWindowID(k.Window)
	}
	return nil
}

// Set is one user's pins.
type Set struct {
	Agents  map[int64]bool
	Mirrors map[MirrorKey]bool
}

// Agent reports whether agent id is pinned.
func (s Set) Agent(id int64) bool { return s.Agents[id] }

// Mirror reports whether the mirrored entry at k is pinned.
func (s Set) Mirror(k MirrorKey) bool { return s.Mirrors[k] }

// MaxPerOwner bounds one user's pins. Mirror pins whose windows are gone stay
// (dormant), so the bound is what keeps them from growing without limit.
const MaxPerOwner = 200

// ErrTooMany refuses a pin past MaxPerOwner.
var ErrTooMany = fmt.Errorf("pins: a user may pin at most %d entries; unpin one first", MaxPerOwner)

// ErrNoSuchAgent refuses a pin of an agent the caller cannot see (absent, or
// another user's — the two answer identically).
var ErrNoSuchAgent = errors.New("pins: no such agent")

// Store is the pin persistence.
type Store interface {
	List(ctx context.Context) (Set, error)
	PinAgent(ctx context.Context, agentID int64) error
	UnpinAgent(ctx context.Context, agentID int64) error
	PinMirror(ctx context.Context, k MirrorKey) error
	UnpinMirror(ctx context.Context, k MirrorKey) error
}

// PGStore is Store on Postgres.
type PGStore struct{ pool *pgxpool.Pool }

// NewPG returns a Store on pool.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

// List is the caller's pins.
func (s *PGStore) List(ctx context.Context) (Set, error) {
	set := Set{Agents: map[int64]bool{}, Mirrors: map[MirrorKey]bool{}}
	rows, err := s.pool.Query(ctx, `SELECT kind, agent_id, mirror_host, mirror_session, mirror_window FROM pins`)
	if err != nil {
		return set, fmt.Errorf("pins: list: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var agentID *int64
		var host, session, window *string
		if err := rows.Scan(&kind, &agentID, &host, &session, &window); err != nil {
			return set, fmt.Errorf("pins: list: %w", err)
		}
		switch {
		case kind == "agent" && agentID != nil:
			set.Agents[*agentID] = true
		case kind == "mirror" && host != nil && session != nil && window != nil:
			set.Mirrors[MirrorKey{Host: *host, Session: *session, Window: *window}] = true
		}
	}
	return set, rows.Err()
}

// PinAgent pins agent id for the caller. Idempotent. An agent the caller cannot
// see is ErrNoSuchAgent.
func (s *PGStore) PinAgent(ctx context.Context, agentID int64) error {
	return s.insert(ctx, `INSERT INTO pins (kind, agent_id) VALUES ('agent', $1)
		ON CONFLICT (owner_id, agent_id) WHERE kind = 'agent' DO NOTHING`, agentID)
}

// UnpinAgent removes the caller's pin of agent id. Idempotent.
func (s *PGStore) UnpinAgent(ctx context.Context, agentID int64) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM pins WHERE kind = 'agent' AND agent_id = $1`, agentID); err != nil {
		return fmt.Errorf("pins: unpin agent: %w", err)
	}
	return nil
}

// PinMirror pins the mirrored entry at k for the caller. Idempotent. The entry
// need not be listed now: a pin is an address, kept while its window is gone.
func (s *PGStore) PinMirror(ctx context.Context, k MirrorKey) error {
	if err := k.Valid(); err != nil {
		return err
	}
	return s.insert(ctx, `INSERT INTO pins (kind, mirror_host, mirror_session, mirror_window) VALUES ('mirror', $1, $2, $3)
		ON CONFLICT (owner_id, mirror_host, mirror_session, mirror_window) WHERE kind = 'mirror' DO NOTHING`,
		k.Host, k.Session, k.Window)
}

// UnpinMirror removes the caller's pin of the entry at k. Idempotent.
func (s *PGStore) UnpinMirror(ctx context.Context, k MirrorKey) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM pins WHERE kind = 'mirror'
		AND mirror_host = $1 AND mirror_session = $2 AND mirror_window = $3`, k.Host, k.Session, k.Window); err != nil {
		return fmt.Errorf("pins: unpin mirror: %w", err)
	}
	return nil
}

// insert runs one pin INSERT under the per-owner bound, in one transaction.
//
// The bound is checked AFTER the insert, in the same transaction, serialised
// per owner by an advisory lock: so two concurrent pins cannot both pass it,
// and re-pinning something already pinned (ON CONFLICT DO NOTHING, no new row)
// still succeeds at the bound.
func (s *PGStore) insert(ctx context.Context, q string, args ...any) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('pins.insert'),
			coalesce(nullif(current_setting('muster.uid', true), '')::int, 0))`); err != nil {
			return fmt.Errorf("pins: lock: %w", err)
		}
		if _, err := tx.Exec(ctx, q, args...); err != nil {
			var pg *pgconn.PgError
			// Measured, each mapped to ErrNoSuchAgent:
			//   - an absent agent: 23514, no constraint name, from the BEFORE
			//     INSERT owner trigger (plugin_rows_owner_is_agents_owner);
			//   - another owner's agent: 23514, the same trigger;
			//   - an agent deleted concurrently, between the trigger's read and
			//     the foreign key check: 23503 (pins_agent_id_fkey) —
			//     TestAPinRacingTheAgentsDeleteIsNoSuchAgent.
			if errors.As(err, &pg) && (pg.Code == "23503" || (pg.Code == "23514" && pg.ConstraintName == "")) {
				return ErrNoSuchAgent
			}
			return fmt.Errorf("pins: insert: %w", err)
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM pins`).Scan(&n); err != nil {
			return fmt.Errorf("pins: count: %w", err)
		}
		if n > MaxPerOwner {
			return ErrTooMany
		}
		return nil
	})
}
