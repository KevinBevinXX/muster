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

// mirrorTokensMigrationVersion is 0008: the mirror plugin's mirror_tokens, an
// owned table under the same row-level security. RLSOff turns it off with the
// rest and RLSOn re-applies its body (idempotent) — but only on a schema that
// HAS the table, so RLSOn never creates it outside Migrate.
const mirrorTokensMigrationVersion = 8

// userCredentialsMigrationVersion is 0011: user_credentials and the owned
// cc_account_marks, under the same row-level security. Same treatment as 0008:
// RLSOff turns them off, RLSOn re-applies 0011's (idempotent) body when the
// schema has user_credentials.
const userCredentialsMigrationVersion = 11

// flowJobsMigrationVersion is 0012: browser flows' flow_jobs and
// flow_runner_tokens, owned and under the same row-level security. Same
// treatment as 0008: RLSOff turns them off, RLSOn re-applies 0012's
// (idempotent) body when the schema has flow_jobs.
const flowJobsMigrationVersion = 12

// RLSOn re-applies migration 0007's body (it is idempotent): RLS enabled and
// forced on every owned table, the policies, owner_id defaulting to the
// principal, the role and its grants — and 0008's for mirror_tokens when that
// table exists. It undoes RLSOff.
func RLSOn(ctx context.Context, pool *pgxpool.Pool) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	ctx = asMigrator(ctx)
	var hasMirrorTokens, hasUserCredentials, hasFlowJobs bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.mirror_tokens') IS NOT NULL,
		to_regclass('public.user_credentials') IS NOT NULL,
		to_regclass('public.flow_jobs') IS NOT NULL`).Scan(&hasMirrorTokens, &hasUserCredentials, &hasFlowJobs); err != nil {
		return fmt.Errorf("rls on: %w", err)
	}
	applied := false
	for _, m := range ms {
		if m.version != rlsMigrationVersion && !(m.version == mirrorTokensMigrationVersion && hasMirrorTokens) &&
			!(m.version == userCredentialsMigrationVersion && hasUserCredentials) &&
			!(m.version == flowJobsMigrationVersion && hasFlowJobs) {
			continue
		}
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
		applied = applied || m.version == rlsMigrationVersion
	}
	if applied {
		return nil
	}
	return fmt.Errorf("rls on: migration %04d is not embedded", rlsMigrationVersion)
}

// RLSState is what RLSStatus reads back: per owned table, whether row-level
// security is enabled and forced, and owner_id's default expression.
type RLSState struct {
	Table        string
	Enabled      bool
	Forced       bool
	OwnerDefault string
}

// RLSStatus reads pg_class and information_schema for every table that has an
// owner_id column — the verification after RLSOff / RLSOn, so a run that did
// nothing (an image whose muster-migrate predates the flag ignores it and exits
// 0) cannot pass for one that worked.
func RLSStatus(ctx context.Context, pool *pgxpool.Pool) ([]RLSState, error) {
	rows, err := pool.Query(asMigrator(ctx), `
		SELECT c.relname::text, c.relrowsecurity, c.relforcerowsecurity, coalesce(col.column_default, '')
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
		  JOIN information_schema.columns col
		    ON col.table_schema = 'public' AND col.table_name = c.relname AND col.column_name = 'owner_id'
		 WHERE c.relkind = 'r'
		 ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("rls status: %w", err)
	}
	defer rows.Close()
	var out []RLSState
	for rows.Next() {
		var st RLSState
		if err := rows.Scan(&st.Table, &st.Enabled, &st.Forced, &st.OwnerDefault); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("rls status: no table has an owner_id column — this is not a muster schema at version >= 6")
	}
	return out, nil
}
