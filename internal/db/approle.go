package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// appRoleSQL creates the application role (AppRole) if it is missing, lets the
// login role SET ROLE to it, and grants it DML on every table and sequence in
// public — now and, through default privileges, for tables created later.
//
// 🔴 MIGRATE RUNS IT ON EVERY BOOT, BEFORE THE VERSION CHECK — not only once
// inside migration 0007. A ROLE IS CLUSTER-GLOBAL AND pg_dump DOES NOT CARRY
// IT: restoring the backup CronJob's plain dump into a fresh Postgres leaves
// schema_migrations at 7 (so 0007 never re-runs) with no muster_app and with
// every `GRANT … TO muster_app` in the dump failed. Without this block every
// pooled query then fails "set connection scope (role muster_app): role does
// not exist" and /readyz answers 503 — muster down after a successful restore.
// Measured against a fresh PG16; TestMigrateRecreatesTheAppRolesGrants pins
// the grants half in-suite.
//
// It is idempotent and cheap. It is VERBATIM the same text as 0007's role
// block (TestTheAppRoleBlockIsMigration0007s pins that), so there is one rule
// in two places that cannot drift.
const appRoleSQL = `DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'muster_app') THEN
        BEGIN
            CREATE ROLE muster_app NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT;
        EXCEPTION WHEN duplicate_object THEN
            NULL;
        END;
    END IF;
END
$$;

-- The login role must be able to SET ROLE to it. A superuser always can; an
-- ordinary owner needs the membership.
DO $$
BEGIN
    IF NOT pg_has_role(current_user, 'muster_app', 'MEMBER') THEN
        EXECUTE format('GRANT muster_app TO %I', current_user);
    END IF;
END
$$;

GRANT USAGE ON SCHEMA public TO muster_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO muster_app;
GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA public TO muster_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO muster_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO muster_app;
`

// ensureAppRole runs appRoleSQL in its own transaction on conn's session (the
// login role).
func ensureAppRole(ctx context.Context, conn *pgxpool.Conn) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("ensure %s: begin: %w", AppRole, err)
	}
	if _, err := tx.Exec(ctx, appRoleSQL); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("ensure %s: %w", AppRole, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("ensure %s: commit: %w", AppRole, err)
	}
	return nil
}
