// Package recents is the Agents list's "recently viewed" entries (migration
// 0018): per user, server-stored, so the strip follows its owner across
// devices. An entry names a dispatched agent or a mirrored tmux window, keyed
// exactly as a pin is (pins.MirrorKey).
//
// 🔴 EVERY METHOD RUNS UNDER THE CALLER'S PRINCIPAL. recents is under FORCE
// row-level security, so a request reads and writes only its own user's rows;
// there is no system method. The owner of a row is the principal's user
// (owner_id's default), never a parameter.
package recents

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/pins"
)

// Entry is one recently opened subject: an agent (AgentID, Mirror zero) or a
// mirrored entry (Mirror, AgentID 0).
type Entry struct {
	AgentID int64
	Mirror  pins.MirrorKey
}

// IsMirror reports whether e names a mirrored entry.
func (e Entry) IsMirror() bool { return e.AgentID == 0 }

// MaxPerOwner bounds one user's rows. Each visit prunes the oldest past it, so
// a mirror row whose window is gone ages out rather than piling up. Two
// concurrent visits may leave one row past it until the next visit: a strip
// that shows six does not care.
const MaxPerOwner = 50

// Store is the recents persistence.
type Store interface {
	// List is the caller's entries, most recently visited first.
	List(ctx context.Context) ([]Entry, error)
	VisitAgent(ctx context.Context, agentID int64) error
	VisitMirror(ctx context.Context, k pins.MirrorKey) error
}

// PGStore is Store on Postgres.
type PGStore struct{ pool *pgxpool.Pool }

// NewPG returns a Store on pool.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

// List is the caller's entries, newest first — all of them, which the prune
// keeps to about MaxPerOwner. Ties (one transaction's two visits) break by id,
// the later row first.
func (s *PGStore) List(ctx context.Context) ([]Entry, error) {
	rows, err := s.pool.Query(ctx, `SELECT kind, agent_id, mirror_host, mirror_session, mirror_window
		FROM recents ORDER BY visited_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("recents: list: %w", err)
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var kind string
		var agentID *int64
		var host, session, window *string
		if err := rows.Scan(&kind, &agentID, &host, &session, &window); err != nil {
			return nil, fmt.Errorf("recents: list: %w", err)
		}
		switch {
		case kind == "agent" && agentID != nil:
			out = append(out, Entry{AgentID: *agentID})
		case kind == "mirror" && host != nil && session != nil && window != nil:
			out = append(out, Entry{Mirror: pins.MirrorKey{Host: *host, Session: *session, Window: *window}})
		}
	}
	return out, rows.Err()
}

// VisitAgent records that the caller opened agent id now. An agent the caller
// cannot see (absent, or another user's) is refused by the owner trigger or the
// foreign key, and that error is returned as is.
func (s *PGStore) VisitAgent(ctx context.Context, agentID int64) error {
	return s.visit(ctx, `INSERT INTO recents (kind, agent_id) VALUES ('agent', $1)
		ON CONFLICT (owner_id, agent_id) WHERE kind = 'agent' DO UPDATE SET visited_at = clock_timestamp()`, agentID)
}

// VisitMirror records that the caller opened the mirrored entry at k now. The
// entry need not be listed: a row is an address, drawn only while it is.
func (s *PGStore) VisitMirror(ctx context.Context, k pins.MirrorKey) error {
	if err := k.Valid(); err != nil {
		return err
	}
	return s.visit(ctx, `INSERT INTO recents (kind, mirror_host, mirror_session, mirror_window) VALUES ('mirror', $1, $2, $3)
		ON CONFLICT (owner_id, mirror_host, mirror_session, mirror_window) WHERE kind = 'mirror'
		DO UPDATE SET visited_at = clock_timestamp()`, k.Host, k.Session, k.Window)
}

// visit runs one upsert and prunes the caller's rows to MaxPerOwner, in one
// transaction. The prune orders by visited_at, so the row just upserted (the
// newest) is never the one it removes.
func (s *PGStore) visit(ctx context.Context, q string, args ...any) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, q, args...); err != nil {
			return fmt.Errorf("recents: visit: %w", err)
		}
		// Under RLS this sees only the caller's rows, so it prunes only them.
		if _, err := tx.Exec(ctx, `DELETE FROM recents WHERE id IN (
			SELECT id FROM recents ORDER BY visited_at DESC, id DESC OFFSET $1)`, MaxPerOwner); err != nil {
			return fmt.Errorf("recents: prune: %w", err)
		}
		return nil
	})
}
