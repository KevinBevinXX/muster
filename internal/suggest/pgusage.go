package suggest

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
)

// PGUsage is UsageStore on suggest_usage (migration 0049), under row-level
// security: ctx must act as the owner it names (a request's, or the rendered
// view socket's, principal), and another owner's row is invisible. Numbers
// only: nothing it writes can hold text.
type PGUsage struct{ pool *pgxpool.Pool }

// NewPGUsage is UsageStore over pool.
func NewPGUsage(pool *pgxpool.Pool) *PGUsage { return &PGUsage{pool: pool} }

func asOwner(ctx context.Context, owner int64) error {
	if pr, ok := authz.From(ctx); !ok || pr.UserID != owner {
		return ErrNotOwner
	}
	return nil
}

// Get implements UsageStore. No row is all zero, not paused.
func (p *PGUsage) Get(ctx context.Context, owner int64) (Tally, error) {
	if err := asOwner(ctx, owner); err != nil {
		return Tally{}, err
	}
	var t Tally
	err := p.pool.QueryRow(ctx, `SELECT shown, filled, sent, edit_sum, paused FROM suggest_usage WHERE owner_id = $1`, owner).
		Scan(&t.Shown, &t.Filled, &t.Sent, &t.EditSum, &t.Paused)
	if errors.Is(err, pgx.ErrNoRows) {
		return Tally{}, nil
	}
	return t, err
}

// Add implements UsageStore: one statement, so two adds never lose a count.
func (p *PGUsage) Add(ctx context.Context, owner int64, shown, filled, sent, edit int64) (Tally, error) {
	if err := asOwner(ctx, owner); err != nil {
		return Tally{}, err
	}
	var t Tally
	err := p.pool.QueryRow(ctx, `INSERT INTO suggest_usage (owner_id, shown, filled, sent, edit_sum) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (owner_id) DO UPDATE SET shown = suggest_usage.shown + EXCLUDED.shown,
			filled = suggest_usage.filled + EXCLUDED.filled, sent = suggest_usage.sent + EXCLUDED.sent,
			edit_sum = suggest_usage.edit_sum + EXCLUDED.edit_sum, updated_at = now()
		RETURNING shown, filled, sent, edit_sum, paused`, owner, shown, filled, sent, edit).
		Scan(&t.Shown, &t.Filled, &t.Sent, &t.EditSum, &t.Paused)
	return t, err
}

// SetPaused implements UsageStore.
func (p *PGUsage) SetPaused(ctx context.Context, owner int64, paused bool) error {
	if err := asOwner(ctx, owner); err != nil {
		return err
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO suggest_usage (owner_id, paused) VALUES ($1, $2)
		ON CONFLICT (owner_id) DO UPDATE SET paused = EXCLUDED.paused, updated_at = now()`, owner, paused)
	return err
}

// Reset implements UsageStore.
func (p *PGUsage) Reset(ctx context.Context, owner int64) error {
	if err := asOwner(ctx, owner); err != nil {
		return err
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO suggest_usage (owner_id) VALUES ($1)
		ON CONFLICT (owner_id) DO UPDATE SET shown = 0, filled = 0, sent = 0, edit_sum = 0, paused = false, updated_at = now()`, owner)
	return err
}
