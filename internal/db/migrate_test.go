package db_test

import (
	"context"
	"io"
	"log"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// 🔴 THIS IS AN EXTERNAL TEST PACKAGE (`package db_test`) SO IT CAN IMPORT
// internal/dbtest. internal/dbtest deliberately does NOT import internal/db —
// that would be an import cycle for any internal test of this package — and the
// only reason it can advise about `schema_migrations` at all is that it names
// the table rather than calling the runner.

// tablesThisSchemaMustHave is the public schema after every migration has
// applied. (0005 dropped the five privilege and runbook tables 0001 created;
// 0006 added users, invites, api_tokens and audit_log; 0008 added mirror_tokens;
// 0011 added user_credentials; 0012 added flow_jobs and flow_runner_tokens;
// 0013 added push_subscriptions and push_preferences; 0014 added flow_requests; 0015 added plugins and plugin_messages; 0016 added plugin_reply_drafts; 0017 added pins; 0019 added send_queue.)
//
// 🔴 A LEDGER, NOT A SPOT CHECK. It fails when the set GROWS or SHRINKS, so a
// table quietly dropped from 0001 reddens here and a table added without a
// decision does too. A "does `notes` exist" check would pass against a
// migration that created one table and lost twelve.
var tablesThisSchemaMustHave = []string{
	"agent_retired_names",
	"agents",
	"api_tokens",
	"audit_log",
	"cc_account_marks",
	"chat_messages",
	"chat_sessions",
	"connector_audit",
	"connector_grants",
	"connectors",
	"flow_jobs",
	"flow_requests",
	"flow_runner_tokens",
	"github_connection",
	"invites",
	"mirror_tokens",
	"note_attachments",
	"note_comments",
	"notes",
	"pins",
	"plugin_messages",
	"plugin_reply_drafts",
	"plugins",
	"push_preferences",
	"push_subscriptions",
	"send_queue",
	"task_sessions",
	"user_credentials",
	"users",
}

// tablesWithNoIdentityColumn names the tables that deliberately have none, each
// for its own stated reason:
//
//   - task_sessions is keyed by a composite.
//   - github_connection is keyed by owner_id, one connection per user (0006;
//     it was a singleton pinned to id = 1 before that, and had no identity then
//     either).
//   - agent_retired_names is keyed by the NAME itself; a surrogate id there would
//     let one name be tombstoned twice.
//
// 🔴 IT IS A NAMED SET AND NOT A COUNT, AND THE DIFFERENCE IS THE WHOLE POINT.
// This used to be arithmetic — `n == len(tablesThisSchemaMustHave) - 2`, the
// two exemptions this schema had before agent_retired_names — and a
// count cannot see a COMPENSATING PAIR: one exempt table gaining an identity
// column while a non-exempt one loses its own leaves the total unchanged, so both
// defects ship under a green check. A count also cannot name the table in its
// failure message, which is the one thing the reader needs.
var tablesWithNoIdentityColumn = []string{
	"agent_retired_names",
	// cc_account_marks is keyed by (owner_id, account) — the account NAME
	// within its owner (0003, per owner since 0011), for the agent_retired_names
	// reason: one account, one record.
	"cc_account_marks",
	"github_connection",
	// push_preferences is keyed by owner_id: one preference row per user
	// (0013), for the github_connection reason.
	"push_preferences",
	"task_sessions",
}

func migrated(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	dsn := dbtest.DSN(t) // skips, or fails, per MUSTER_TEST_REQUIRE_DB
	ctx, cancel := context.WithTimeout(db.AsMigratorForTest(context.Background()), 2*time.Minute)
	t.Cleanup(cancel)

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	// Quiet: a passing package's log is discarded anyway, and a failing one is
	// easier to read without the per-migration lines.
	if err := db.Migrate(ctx, pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return ctx, pool
}

// TestMigrateCreatesEveryTableTheSchemaDeclares.
func TestMigrateCreatesEveryTableTheSchemaDeclares(t *testing.T) {
	ctx, pool := migrated(t)

	rows, err := pool.Query(ctx, `
		SELECT table_name FROM information_schema.tables
		 WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
		   AND table_name <> 'schema_migrations'
		 ORDER BY table_name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("the catalogue query returned no tables at all; it is not observing the database")
	}
	if !equal(got, tablesThisSchemaMustHave) {
		t.Fatalf("the public schema changed.\n got: %v\nwant: %v", got, tablesThisSchemaMustHave)
	}
}

// TestEveryIdentityColumnIsGeneratedALWAYS.
//
// 🔴 THIS IS THE ONE PROPERTY OF 0001 WHOSE VIOLATION IS SILENT FOR MONTHS. A
// column weakened from ALWAYS to BY DEFAULT behaves identically for every write
// muster makes; it only shows up when an importer hands in an explicit id and
// succeeds where it should have been refused, or when someone concludes from
// `BY DEFAULT` that a hand-written INSERT migrator is fine and writes one.
//
// It asserts the RELATIONSHIP — "every identity column in the schema is ALWAYS"
// — rather than naming columns, so a table added later is covered without
// anybody remembering to extend a list.
//
// 🔴 AND IT ASSERTS WHICH TABLES HOLD ONE, AS A SET. See
// tablesWithNoIdentityColumn: the observed set of identity-holding tables must
// EQUAL the schema ledger minus the named exemptions, so the check fails when a
// non-exempt table loses its identity column, when an exempt one gains one, and —
// the case the old arithmetic could not see — when both happen at once. Every
// failure names the table.
func TestEveryIdentityColumnIsGeneratedALWAYS(t *testing.T) {
	ctx, pool := migrated(t)

	rows, err := pool.Query(ctx, `
		SELECT table_name, column_name, is_identity, identity_generation
		  FROM information_schema.columns
		 WHERE table_schema = 'public' AND is_identity = 'YES'
		 ORDER BY table_name, column_name`)
	if err != nil {
		t.Fatalf("list identity columns: %v", err)
	}
	defer rows.Close()
	var withIdentity []string
	for rows.Next() {
		var table, column, isIdentity string
		var generation *string
		if err := rows.Scan(&table, &column, &isIdentity, &generation); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !slices.Contains(withIdentity, table) {
			withIdentity = append(withIdentity, table)
		}
		if generation == nil || *generation != "ALWAYS" {
			got := "<null>"
			if generation != nil {
				got = *generation
			}
			t.Errorf("%s.%s is GENERATED %s AS IDENTITY, want ALWAYS.\n\n"+
				"BY DEFAULT lets a caller supply an id, which means a hand-written INSERT "+
				"migrator appears to work and a `pg_dump`-based one is no longer the only "+
				"supported path. The restriction is what makes the sequence the single "+
				"authority on ids.", table, column, got)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	want := make([]string, 0, len(tablesThisSchemaMustHave))
	for _, tbl := range tablesThisSchemaMustHave {
		if !slices.Contains(tablesWithNoIdentityColumn, tbl) {
			want = append(want, tbl)
		}
	}
	// The exemption ledger must be a subset of the schema ledger, or an
	// exemption for a table that no longer exists would silently shrink `want`
	// and excuse a real regression.
	for _, tbl := range tablesWithNoIdentityColumn {
		if !slices.Contains(tablesThisSchemaMustHave, tbl) {
			t.Errorf("tablesWithNoIdentityColumn names %q, which is not in "+
				"tablesThisSchemaMustHave; an exemption for a table that does not exist "+
				"excuses nothing and hides the next real one", tbl)
		}
	}
	// Positive control: an empty expectation would make everything below vacuous.
	if len(want) == 0 {
		t.Fatal("no table is expected to hold an identity column; this check would pass against " +
			"a schema with none at all")
	}

	slices.Sort(withIdentity)
	slices.Sort(want)
	for _, tbl := range want {
		if !slices.Contains(withIdentity, tbl) {
			t.Errorf("%s has NO identity column, and it is not in tablesWithNoIdentityColumn.\n\n"+
				"Either 0001 lost the `GENERATED ALWAYS AS IDENTITY` on its id — in which case "+
				"nothing stops a caller supplying one — or the table is a deliberate exemption "+
				"and belongs in that ledger with its reason written down.", tbl)
		}
	}
	for _, tbl := range withIdentity {
		if !slices.Contains(want, tbl) {
			t.Errorf("%s HAS an identity column but is not expected to.\n\n"+
				"It is either listed in tablesWithNoIdentityColumn — whose reason it now "+
				"contradicts — or it is a table missing from tablesThisSchemaMustHave "+
				"entirely.", tbl)
		}
	}
	if !equal(withIdentity, want) {
		t.Fatalf("the set of tables holding an identity column changed.\n got: %v\nwant: %v",
			withIdentity, want)
	}
}

// TestMigrateIsIdempotent — a second run must be a no-op, not an error.
func TestMigrateIsIdempotent(t *testing.T) {
	ctx, pool := migrated(t)
	if err := db.Migrate(ctx, pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var versions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&versions); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if versions == 0 {
		t.Fatal("schema_migrations is empty after two migrate runs; nothing was recorded")
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
