// Package db opens muster's Postgres pool and applies its migrations.
//
// It is deliberately small. muster's domain lives elsewhere; this package owns
// exactly two things nothing else may own — the connection settings every
// caller inherits, and the migration runner.
package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// MigrationLockKey is the advisory-lock key Migrate takes.
//
// 🔴 ADVISORY LOCKS ARE DATABASE-SCOPED, so this shares no namespace with a key
// taken against any other database — including the one internal/dbtest takes in
// the `postgres` maintenance database. Mint every new muster key HERE so a
// collision is visible in one place rather than discovered as a deadlock.
const MigrationLockKey int64 = 0x6d_75_73_74_01

// Migrate applies every embedded migration whose numeric prefix is greater than
// the highest already recorded, each inside a transaction, under a session
// advisory lock so that two processes starting at once cannot both apply them.
// It REFUSES (ErrSkippedMigration) when an embedded migration below that
// highest version was never recorded — see checkNoSkippedMigration.
//
// It is idempotent: a second run with no new files is a no-op.
func Migrate(ctx context.Context, pool *pgxpool.Pool, logger *log.Logger) error {
	if logger == nil {
		logger = log.Default()
	}
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	// 🔴 THE MIGRATOR KEEPS THE LOGIN ROLE AND SEES EVERY ROW (scope.go): it
	// runs DDL, and a data migration must reach every user's rows. Without
	// this the pool would switch to AppRole — which does not exist before 0007
	// — with no principal, and every backfill would see nothing.
	ctx = asMigrator(ctx)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration conn: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, MigrationLockKey); err != nil {
		return fmt.Errorf("acquire advisory lock: %w", err)
	}
	// 🔴 RELEASE ON A CONTEXT THAT CANNOT ALREADY BE CANCELLED. pg_advisory_lock
	// is SESSION-scoped, so it does not end when this function returns — it ends
	// with the backend. This runs on a POOLED connection. Handed the caller's
	// ctx when that ctx is already done, pgx returns "context canceled" WITHOUT
	// sending anything to the server; the connection still looks healthy to the
	// pool, so Release() hands it back STILL HOLDING THE LOCK for the rest of
	// the process's life — and Migrate returns a silent success. The next
	// process to migrate then blocks until this one exits.
	//
	// context.WithoutCancel is the whole fix: the unlock is sent regardless of
	// what happened to the caller's context.
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, MigrationLockKey); err != nil {
			// 🔴 SAY SO. The lock is session-scoped, so the strand ends when this
			// process exits — but a maintainer debugging a peer that blocks on
			// startup needs this line to exist.
			logger.Printf("muster: could not release the migration advisory lock: %v", err)
		}
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version     INTEGER     PRIMARY KEY,
			name        TEXT        NOT NULL,
			applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	// Before the version check, on every boot: a restored backup is at the
	// latest version but has no application role (see appRoleSQL).
	if err := ensureAppRole(ctx, conn); err != nil {
		return err
	}

	var current int
	if err := conn.QueryRow(ctx,
		`SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}

	if err := checkNoSkippedMigration(ctx, conn.QueryRow, migrations, current); err != nil {
		return err
	}

	applied := 0
	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin %s: %w", m.name, err)
		}
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply %s: %w", m.name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`,
			m.version, m.name); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record %s: %w", m.name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit %s: %w", m.name, err)
		}
		logger.Printf("muster: applied migration %s", m.name)
		applied++
	}
	if applied == 0 {
		logger.Printf("muster: schema is at version %d; nothing to apply", current)
	}
	return nil
}

// ErrSkippedMigration: an embedded migration older than the newest applied one
// was never applied. Migrate refuses to boot rather than skip it forever.
var ErrSkippedMigration = errors.New("db: an embedded migration below the applied version was never applied")

// checkNoSkippedMigration refuses a database where an EMBEDDED migration below
// the highest recorded version is absent from schema_migrations.
//
// 🔴 WHY: THE RUNNER APPLIES ONLY VERSIONS ABOVE max(version), SO A GAP IS
// PERMANENT AND SILENT. Two PRs that each add a migration can merge (or deploy)
// in either order; if 0011 reaches a database before 0010 exists in the image,
// that database records 11, and every later boot skips 0010 — #67's session
// revocation, in the case that prompted this (round-2 audit on #69).
//
// 🔴 REFUSE, NOT APPLY OUT OF ORDER. Every migration here is written against the
// schema its predecessors leave (0009 assumes 0007's policies; 0005 drops what
// 0001 made; 0011 re-keys a table 0003 created), so running an old file on a
// newer schema is unsafe in general, and which ones would happen to be safe is
// not decidable by the runner. The failure names the file, so the operator can
// apply it by hand (or redeploy an image whose order is right) and boot again.
// A normal database — every embedded version up to max recorded — is unchanged.
// TestMigrateRefusesToSkipAMigrationBelowTheAppliedVersion.
func checkNoSkippedMigration(ctx context.Context, queryRow func(context.Context, string, ...any) pgx.Row, migrations []migration, current int) error {
	var missing []migration
	for _, m := range migrations {
		if m.version >= current {
			break
		}
		var present bool
		if err := queryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, m.version).Scan(&present); err != nil {
			return fmt.Errorf("read schema_migrations: %w", err)
		}
		if !present {
			missing = append(missing, m)
		}
	}
	if len(missing) > 0 {
		names := make([]string, len(missing))
		cmds := make([]string, len(missing))
		for i, m := range missing {
			names[i], cmds[i] = m.name, ApplyByHandCommand(m.version, m.name)
		}
		return fmt.Errorf("%w: %s (the database is at version %d). From a checkout at the commit the running "+
			"image was built from (so each file is the one this image embeds), apply each, in this order: %s — or "+
			"deploy an image in which it precedes every applied migration; this server will not boot past it",
			ErrSkippedMigration, strings.Join(names, ", "), current, strings.Join(cmds, " ; then "))
	}
	return nil
}

// ApplyByHandCommand is the operator's remedy for one skipped migration: apply
// the file AND record it, in ONE transaction, through the deployment's own
// Postgres pod (the in-cluster form: the file is streamed on stdin, so it is
// read from the operator's checkout — which must be at the commit the running
// image was built from, the refusal says so).
//
// 🔴 -1 IS THE POINT (round-3 audit on #69). Without it psql runs the file in
// autocommit: every file begins `SET LOCAL lock_timeout`, which outside a
// transaction only warns — the 3 s guard is silently gone — and a failure part
// way through leaves the file half-applied and unrecorded. With -1 the file
// (-f -, stdin) and the -c INSERT run as one transaction; ON_ERROR_STOP makes
// the first error abort it. Measured on PostgreSQL 16 (psql 16.15) with this
// exact stdin form (`docker exec -i … psql … -1 -f - -c … < file`): a
// re-applied 0009 plus its row committed, rc 0, no SET LOCAL warning; a file
// failing on its third statement left neither its table nor the
// schema_migrations row, rc 3.
// Pinned by TestTheSkippedMigrationRefusalGivesTheSingleTransactionCommand.
func ApplyByHandCommand(version int, name string) string {
	return fmt.Sprintf(`kubectl -n muster exec -i deploy/muster-postgres -- psql -U muster -d muster `+
		`-v ON_ERROR_STOP=1 -1 -f - -c "INSERT INTO schema_migrations (version, name) VALUES (%d, '%s')" `+
		`< internal/db/migrations/%s`, version, name, name)
}

// sortMigrations orders migrations by version, ascending.
func sortMigrations(ms []migration) {
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
}

type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads and orders the embedded files, and REFUSES a set it
// cannot order unambiguously.
//
// 🔴 THE DUPLICATE-VERSION CHECK IS NOT PEDANTRY. Two files sharing a numeric
// prefix apply in an order decided by the filesystem walk, and the second one
// is recorded under a version that already exists — so on the next boot one of
// them is silently skipped forever. Refusing at load is the only point where
// that is still visible.
func loadMigrations() ([]migration, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	var out []migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			return nil, fmt.Errorf("migration %q must be named NNNN_description.sql", e.Name())
		}
		version, err := strconv.Atoi(prefix)
		if err != nil {
			return nil, fmt.Errorf("migration %q has a non-numeric version prefix: %w", e.Name(), err)
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("migrations %q and %q share version %d; the order they apply in "+
				"would be decided by the filesystem, and only ONE of them would ever be recorded",
				prev, e.Name(), version)
		}
		seen[version] = e.Name()
		body, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", e.Name(), err)
		}
		out = append(out, migration{version: version, name: e.Name(), sql: string(body)})
	}
	if len(out) == 0 {
		// A zero here is an embed that resolved to nothing, not a project with no
		// schema. Migrate would then report success having done nothing.
		return nil, errors.New("no migrations were embedded; the //go:embed pattern is not matching")
	}
	sortMigrations(out)
	return out, nil
}
