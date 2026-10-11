package db_test

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/db"
)

// Migration 0005 drops the privilege and runbook schema: five tables and the
// notes.grant_profiles column. These tests are about the DATABASE, against real
// Postgres, in a SCRATCH database each — the package's own database was copied
// from an already-migrated template, so it is at the newest version by
// construction and cannot show a database that holds rows when 0005 arrives.

// removedTables is what 0005 must drop.
var removedTables = []string{
	"agent_privileges", "privilege_profiles", "privilege_requests", "runbook_runs", "runbooks",
}

// applyThrough applies the embedded-on-disk migrations with version <= max
// exactly the way db.Migrate does (one transaction each, recorded in
// schema_migrations), so a later db.Migrate sees a database that stopped at max.
func applyThrough(ctx context.Context, t *testing.T, pool *pgxpool.Pool, max int) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version     INTEGER     PRIMARY KEY,
			name        TEXT        NOT NULL,
			applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	files, err := filepath.Glob(filepath.Join("migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("list migrations: %v (%d files) — the fixture cannot build an old database", err, len(files))
	}
	sort.Strings(files)
	applied := 0
	for _, f := range files {
		name := filepath.Base(f)
		v, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			t.Fatalf("migration %s: %v", name, err)
		}
		if v > max {
			continue
		}
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, v, name); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		applied++
	}
	if applied != max {
		t.Fatalf("applied %d migrations through version %d, want %d — the fixture is not the old schema", applied, max, max)
	}
}

func tableExists(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var reg *string
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.' || $1)::text`, name).Scan(&reg); err != nil {
		t.Fatalf("to_regclass(%s): %v", name, err)
	}
	return reg != nil
}

func columnExists(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table, column string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema='public' AND table_name=$1 AND column_name=$2`, table, column).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func assertPrivilegeAndRunbookSchemaGone(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, tbl := range removedTables {
		if tableExists(ctx, t, pool, tbl) {
			t.Errorf("table %s still exists after migrating", tbl)
		}
	}
	if columnExists(ctx, t, pool, "notes", "grant_profiles") {
		t.Error("notes.grant_profiles still exists after migrating")
	}
	// Positive control for the probes: a table and a column that must survive.
	if !tableExists(ctx, t, pool, "notes") || !columnExists(ctx, t, pool, "notes", "tags") {
		t.Fatal("notes / notes.tags not found — the existence probes are not observing this database")
	}
}

// TestMigration0005DropsThePrivilegeAndRunbookSchemaFromAPopulatedDatabase is
// the deploy case: a database at 0004 holding rows in every table 0005 drops,
// and a task carrying grant_profiles. 0005 must apply, drop them, and leave the
// task itself — its body, tags and status — intact.
func TestMigration0005DropsThePrivilegeAndRunbookSchemaFromAPopulatedDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, scratchDatabase(ctx, t, "ms_0005_populated", ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	applyThrough(ctx, t, pool, 4)

	// Rows in every table 0005 drops, linked the way production links them.
	var agentID, profileID, runbookID, noteID int64
	mustScan := func(dst *int64, sql string, args ...any) {
		t.Helper()
		if err := pool.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	mustScan(&agentID, `INSERT INTO agents (name, namespace, status) VALUES ('grantee', 'muster-agent-grantee', 'running') RETURNING id`)
	mustScan(&profileID, `INSERT INTO privilege_profiles (name, spec) VALUES ('k8s-read', '{"clusterRules":[{"verbs":["get"]}]}') RETURNING id`)
	mustScan(&noteID, `INSERT INTO notes (body, tags, grant_profiles) VALUES ('keep me', ARRAY['runbook:deploy','api'], ARRAY[$1::bigint]) RETURNING id`, profileID)
	mustScan(&runbookID, `INSERT INTO runbooks (name, spec) VALUES ('deploy', '{"template":"x"}') RETURNING id`)
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO agent_privileges (agent_id, profile_id, granted_by) VALUES ($1, $2, 'operator')`, []any{agentID, profileID}},
		{`INSERT INTO privilege_requests (agent_id, agent_name, profile, status) VALUES ($1, 'grantee', 'k8s-read', 'pending')`, []any{agentID}},
		{`INSERT INTO runbook_runs (runbook_id, runbook_name, agent_id, note_id, rendered_body) VALUES ($1, 'deploy', $2, $3, 'x')`, []any{runbookID, agentID, noteID}},
	} {
		if _, err := pool.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("%s: %v", stmt.sql, err)
		}
	}
	// Fixture control: every table really holds a row before 0005 runs.
	for _, tbl := range removedTables {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+tbl).Scan(&n); err != nil || n == 0 {
			t.Fatalf("fixture: %s has %d rows (%v); want ≥1 so the drop is exercised on data", tbl, n, err)
		}
	}

	if err := db.Migrate(ctx, pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("migrating a populated 0004 database failed: %v", err)
	}
	assertPrivilegeAndRunbookSchemaGone(ctx, t, pool)

	var body, status string
	var tags []string
	if err := pool.QueryRow(ctx, `SELECT body, status, tags FROM notes WHERE id=$1`, noteID).Scan(&body, &status, &tags); err != nil {
		t.Fatalf("the task that carried grant_profiles is unreadable after 0005: %v", err)
	}
	if body != "keep me" || status != "open" || strings.Join(tags, ",") != "api,runbook:deploy" && strings.Join(tags, ",") != "runbook:deploy,api" {
		t.Errorf("task changed by 0005: body %q status %q tags %v", body, status, tags)
	}
	var agents int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agents WHERE id=$1`, agentID).Scan(&agents); err != nil || agents != 1 {
		t.Errorf("the agent that held a grant is gone after 0005 (%d, %v)", agents, err)
	}
}

// TestMigration0005OnAFreshDatabase: a brand-new deployment runs 0001..0005 in
// one go and ends with none of the removed schema.
func TestMigration0005OnAFreshDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, scratchDatabase(ctx, t, "ms_0005_fresh", ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	if err := db.Migrate(ctx, pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("migrating a fresh database failed: %v", err)
	}
	assertPrivilegeAndRunbookSchemaGone(ctx, t, pool)

	var max int
	if err := pool.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&max); err != nil || max < 5 {
		t.Fatalf("schema_migrations max version = %d (%v), want ≥ 5", max, err)
	}
}
