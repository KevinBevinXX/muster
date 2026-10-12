package db

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// rlsOffSQL is rollback/rls_off.sql: the documented way back from migration
// 0007 before an OLDER image is deployed. See that file's header.
//
//go:embed rollback/rls_off.sql
var rlsOffSQL string

// rlsMigrationVersion is the migration whose body RLSOn re-applies.
const rlsMigrationVersion = 7

// RLSOff disables row-level security on every owned table and restores
// owner_id DEFAULT 1 (rollback/rls_off.sql), so that an image built before
// accounts PR 3a can run against this schema. It runs as the migrator (the
// login role), never as AppRole. cmd/muster-migrate --rls-off calls it.
//
// 🔴 schema_migrations IS NOT TOUCHED. Version 7 stays recorded, so a newer
// image booting afterwards does NOT re-enable RLS on its own; RLSOn does that,
// explicitly.
func RLSOff(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(asMigrator(ctx), rlsOffSQL); err != nil {
		return fmt.Errorf("rls off: %w", err)
	}
	return nil
}

// RLSOn re-applies migration 0007's body (it is idempotent): RLS enabled and
// forced on every owned table, the policies, owner_id defaulting to the
// principal, the role and its grants. It undoes RLSOff.
func RLSOn(ctx context.Context, pool *pgxpool.Pool) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	for _, m := range ms {
		if m.version != rlsMigrationVersion {
			continue
		}
		ctx = asMigrator(ctx)
		tx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("rls on: begin: %w", err)
		}
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("rls on: apply %s: %w", m.name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("rls on: commit: %w", err)
		}
		return nil
	}
	return fmt.Errorf("rls on: migration %04d is not embedded", rlsMigrationVersion)
}
