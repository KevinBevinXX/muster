package db_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/github"
)

// applyThroughFrom applies every on-disk migration NEWER than the database's
// recorded version and no newer than max, the way db.Migrate does (one
// transaction each, recorded). It lets a test stop a populated database at an
// exact version, which db.Migrate (always "latest") cannot.
func applyThroughFrom(ctx context.Context, t *testing.T, pool *pgxpool.Pool, max int) {
	t.Helper()
	var cur int
	if err := pool.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&cur); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join("migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("list migrations: %v (%d files)", err, len(files))
	}
	slices.Sort(files)
	for _, f := range files {
		name := filepath.Base(f)
		var v int
		if _, err := fmt.Sscanf(name, "%04d_", &v); err != nil {
			t.Fatalf("migration %s: %v", name, err)
		}
		if v <= cur || v > max {
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
	}
}

// Migration 0006 — accounts, schema only. These tests run against real
// Postgres in SCRATCH databases: the package database is copied from an
// already-migrated template, so it can never show a database that holds rows
// when 0006 arrives, and the cross-owner cases need a second user that must not
// leak into other tests.

// ownedTables is every table 0006 gives an owner_id. A LEDGER: the test that
// reads it fails if a table gains or loses the column without this list
// changing.
// ownedAfter0006 are owned tables a LATER migration created, in 0006's shape
// and under 0007's row-level security. They are not in ownedTables because the
// backfill fixture builds a version-5 database, where they do not exist yet;
// the owner-column and RLS ledgers check both (rlsTables).
//
//   - mirror_tokens (0008, the mirrored-agents plugin's per-host tokens).
//   - user_credentials (0011, members' own Claude/OpenRouter credentials).
//   - cc_account_marks (0011: SHARED until then — the environment pool's marks
//     — and owned since, because an account is (owner, name) once members bring
//     their own).
//   - flow_jobs, flow_runner_tokens (0012, browser flows' queue and per-host
//     runner tokens).
//   - push_subscriptions, push_preferences (0013, muster-native Web Push).
//   - flow_requests (0014, an agent's per-run browser-flow request).
//   - plugins, plugin_messages (0015, the plugin interface's tokens and
//     inbound messages); plugin_reply_drafts (0016, its reply drafts).
//   - pins (0017, the Agents list's pinned entries).
//   - recents (0018, the Agents list's recently viewed entries).
//   - send_queue (0019, the server-side send queue).
//   - session_routes (0021, the chief's routed plugin messages).
//   - agent_focus_history (0022, every focus a session has had).
//   - attention_asks (0023, the attention queue's explicit asks).
//   - agent_focus (0020, each session's focus).
var ownedAfter0006 = []string{"agent_focus", "agent_focus_history", "attention_asks", "cc_account_marks", "flow_jobs", "flow_requests", "flow_runner_tokens",
	"mirror_tokens", "pins", "plugin_messages", "plugin_reply_drafts", "plugins", "push_preferences",
	"push_subscriptions", "recents", "send_queue", "session_routes", "user_credentials"}

// rlsTables is every owned table today: ownedTables plus ownedAfter0006.
func rlsTables() []string { return append(slices.Clone(ownedTables), ownedAfter0006...) }

var ownedTables = []string{
	"agents",
	"chat_messages",
	"chat_sessions",
	"connector_audit",
	"connector_grants",
	"connectors",
	"github_connection",
	"note_attachments",
	"note_comments",
	"notes",
	"task_sessions",
}

// sharedTables deliberately have NO owner: the global agent-name ledger and
// the migration ledger. (cc_account_marks was here until 0011 made it owned.)
var sharedTables = []string{"agent_retired_names", "schema_migrations"}

// wantForeignKeys is EVERY foreign key in the public schema after 0006, as
// `table|constraint|definition`. It pins the RELATIONSHIP — each owned
// parent->child link is composite over (parent_id, owner_id) — and fails when
// the set grows or shrinks, so a single-column FK surviving beside its
// composite replacement, a composite FK silently dropped, or a changed delete
// action all redden it by name.
var wantForeignKeys = []string{
	"agents|agents_note_owner_fkey|FOREIGN KEY (note_id, owner_id) REFERENCES notes(id, owner_id) ON DELETE SET NULL (note_id)",
	"agents|agents_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
	"cc_account_marks|cc_account_marks_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
	"api_tokens|api_tokens_user_id_fkey|FOREIGN KEY (user_id) REFERENCES users(id)",
	"audit_log|audit_log_user_id_fkey|FOREIGN KEY (user_id) REFERENCES users(id)",
	"chat_messages|chat_messages_agent_owner_fkey|FOREIGN KEY (agent_id, owner_id) REFERENCES agents(id, owner_id) ON DELETE CASCADE",
	"chat_messages|chat_messages_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
	"chat_messages|chat_messages_session_owner_fkey|FOREIGN KEY (session_id, owner_id) REFERENCES chat_sessions(id, owner_id) ON DELETE CASCADE",
	"chat_sessions|chat_sessions_agent_owner_fkey|FOREIGN KEY (agent_id, owner_id) REFERENCES agents(id, owner_id) ON DELETE CASCADE",
	"chat_sessions|chat_sessions_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
	"connector_audit|connector_audit_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
	"connector_grants|connector_grants_agent_owner_fkey|FOREIGN KEY (agent_id, owner_id) REFERENCES agents(id, owner_id) ON DELETE CASCADE",
	"connector_grants|connector_grants_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
	"connectors|connectors_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
	"github_connection|github_connection_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
	"invites|invites_claim_user_id_fkey|FOREIGN KEY (claim_user_id) REFERENCES users(id)",
	"invites|invites_created_by_fkey|FOREIGN KEY (created_by) REFERENCES users(id)",
	"invites|invites_redeemed_by_fkey|FOREIGN KEY (redeemed_by) REFERENCES users(id)",
	"flow_jobs|flow_jobs_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
	"flow_requests|flow_requests_agent_id_fkey|FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE SET NULL",
	"flow_requests|flow_requests_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
	"flow_runner_tokens|flow_runner_tokens_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
	"mirror_tokens|mirror_tokens_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
	"note_attachments|note_attachments_note_owner_fkey|FOREIGN KEY (note_id, owner_id) REFERENCES notes(id, owner_id) ON DELETE CASCADE",
	"note_attachments|note_attachments_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
	"note_comments|note_comments_note_owner_fkey|FOREIGN KEY (note_id, owner_id) REFERENCES notes(id, owner_id) ON DELETE CASCADE",
	"note_comments|note_comments_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
	"notes|notes_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
	"agent_focus|agent_focus_agent_id_fkey|FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE",
	"agent_focus|agent_focus_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
	"agent_focus_history|agent_focus_history_agent_id_fkey|FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE",
	"agent_focus_history|agent_focus_history_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
	"attention_asks|attention_asks_agent_id_fkey|FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE",
	"attention_asks|attention_asks_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
	"pins|pins_agent_id_fkey|FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE",
	"pins|pins_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
	"recents|recents_agent_id_fkey|FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE",
	"recents|recents_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
	"plugin_messages|plugin_messages_agent_id_fkey|FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE SET NULL",
	"plugin_messages|plugin_messages_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
	"plugin_messages|plugin_messages_plugin_owner_fkey|FOREIGN KEY (plugin_id, owner_id) REFERENCES plugins(id, owner_id) ON DELETE CASCADE",
	"plugin_reply_drafts|plugin_reply_drafts_agent_id_fkey|FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE SET NULL",
	"plugin_reply_drafts|plugin_reply_drafts_message_owner_fkey|FOREIGN KEY (message_id, owner_id) REFERENCES plugin_messages(id, owner_id) ON DELETE CASCADE",
	"plugin_reply_drafts|plugin_reply_drafts_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
	"plugins|plugins_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
	"push_preferences|push_preferences_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
	"push_subscriptions|push_subscriptions_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
	"session_routes|session_routes_agent_id_fkey|FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE SET NULL",
	"session_routes|session_routes_draft_id_fkey|FOREIGN KEY (draft_id) REFERENCES plugin_reply_drafts(id) ON DELETE SET NULL",
	"session_routes|session_routes_message_owner_fkey|FOREIGN KEY (plugin_message_id, owner_id) REFERENCES plugin_messages(id, owner_id) ON DELETE CASCADE",
	"session_routes|session_routes_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
	"send_queue|send_queue_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
	"task_sessions|task_sessions_note_owner_fkey|FOREIGN KEY (note_id, owner_id) REFERENCES notes(id, owner_id) ON DELETE CASCADE",
	"task_sessions|task_sessions_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
	"user_credentials|user_credentials_owner_id_fkey|FOREIGN KEY (owner_id) REFERENCES users(id)",
}

func freshMigrated(ctx context.Context, t *testing.T, name string) *pgxpool.Pool {
	t.Helper()
	pool, err := db.Connect(ctx, scratchDatabase(ctx, t, name, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

func ctx0006(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(db.AsMigratorForTest(context.Background()), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func scanID(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return id
}

func mustExec(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func countRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// fkViolation asserts err is a foreign-key violation raised by EXACTLY the named
// constraint — not the owner->users FK, not some other check — so a test that
// goes red for the wrong reason cannot pass as this guard's coverage.
func fkViolation(t *testing.T, err error, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("want a 23503 from %s, got %v", constraint, err)
	}
	if pgErr.Code != "23503" || pgErr.ConstraintName != constraint {
		t.Fatalf("want 23503 from %s, got %s from %q: %s", constraint, pgErr.Code, pgErr.ConstraintName, pgErr.Message)
	}
}

// TestMigration0006BackfillsAPopulatedDatabaseToTheOperator is the deploy case:
// a database at 0005 with realistic rows in every table 0006 touches, linked the
// way production links them. Every row must survive, be owned by user 1, and
// the column must be NOT NULL.
func TestMigration0006BackfillsAPopulatedDatabaseToTheOperator(t *testing.T) {
	ctx := ctx0006(t)
	pool, err := db.Connect(ctx, scratchDatabase(ctx, t, "ms_0006_populated", ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	applyThrough(ctx, t, pool, 5)
	if columnExists(ctx, t, pool, "notes", "owner_id") {
		t.Fatal("fixture: notes.owner_id exists at version 5 — the fixture is not the old schema")
	}

	note := scanID(ctx, t, pool, `INSERT INTO notes (title, body, tags, status) VALUES ('Rotate the webhook secret', 'swap it', ARRAY['infra'], 'in_progress') RETURNING id`)
	note2 := scanID(ctx, t, pool, `INSERT INTO notes (title, body) VALUES ('Archive the doc', '') RETURNING id`)
	mustExec(ctx, t, pool, `INSERT INTO note_comments (note_id, author, body) VALUES ($1, 'operator', 'looking'), ($1, 'agent', 'done')`, note)
	mustExec(ctx, t, pool, `INSERT INTO note_attachments (note_id, filename, size_bytes, data) VALUES ($1, 'log.txt', 3, '\x616263')`, note)
	mustExec(ctx, t, pool, `INSERT INTO task_sessions (note_id, session_id, role) VALUES ($1, 'sess-a', 'created'), ($2, 'sess-a', 'read')`, note, note2)
	worker := scanID(ctx, t, pool, `INSERT INTO agents (name, namespace, status, note_id) VALUES ('brave-heron', 'muster-agent-brave-heron', 'running', $1) RETURNING id`, note)
	chief := scanID(ctx, t, pool, `INSERT INTO agents (name, namespace, status, kind, cc_account) VALUES ('chief', 'muster-agent-chief', 'running', 'claude-code', 'acct-a') RETURNING id`)
	session := scanID(ctx, t, pool, `INSERT INTO chat_sessions (agent_id, session_key) VALUES ($1, 'agent:brave-heron:main') RETURNING id`, worker)
	mustExec(ctx, t, pool, `INSERT INTO chat_messages (agent_id, session_id, role, content) VALUES ($1, $2, 'user', 'hi'), ($1, $2, 'assistant', 'hello')`, worker, session)
	mustExec(ctx, t, pool, `INSERT INTO chat_messages (agent_id, role, content) VALUES ($1, 'user', 'no session')`, chief)
	mustExec(ctx, t, pool, `INSERT INTO connectors (name, hosts, auth_scheme, secret_ciphertext) VALUES ('clickup', ARRAY['api.clickup.com'], 'bearer', '\x00')`)
	mustExec(ctx, t, pool, `INSERT INTO connector_grants (agent_id, connector_name, status) VALUES ($1, 'clickup', 'approved'), ($1, 'not-yet', 'pending')`, worker)
	mustExec(ctx, t, pool, `INSERT INTO connector_audit (kind, agent_id, agent_name, connector_name, method, host, path, status) VALUES ('proxy', $1, 'brave-heron', 'clickup', 'GET', 'api.clickup.com', '/v2/team', 200), ('grant', NULL, '', 'clickup', '', '', '', 0)`, worker)
	mustExec(ctx, t, pool, `INSERT INTO github_connection (id, login, scopes, token_ciphertext) VALUES (1, 'octo', 'repo', '\xdeadbeef')`)
	mustExec(ctx, t, pool, `INSERT INTO cc_account_marks (account, rate_limited_detail) VALUES ('acct-a', '429')`)
	gone := scanID(ctx, t, pool, `INSERT INTO agents (name, namespace) VALUES ('old-otter', 'muster-agent-old-otter') RETURNING id`)
	mustExec(ctx, t, pool, `DELETE FROM agents WHERE id=$1`, gone) // tombstones the name

	before := map[string]int{}
	for _, tbl := range append(slices.Clone(ownedTables), sharedTables...) {
		before[tbl] = countRows(ctx, t, pool, tbl)
		if before[tbl] == 0 {
			t.Fatalf("fixture: %s is empty; every affected table must hold a row so the backfill is exercised on data", tbl)
		}
	}

	// Through 0006 EXACTLY: this test is about 0006's backfill and its
	// temporary DEFAULT 1. 0007 replaces that default (TestRLS… in
	// migration_0007_rls_test.go covers the database after it).
	applyThroughFrom(ctx, t, pool, 6)

	for _, tbl := range ownedTables {
		if got := countRows(ctx, t, pool, tbl); got != before[tbl] {
			t.Errorf("%s: %d rows after 0006, %d before", tbl, got, before[tbl])
		}
		var notOne int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+tbl+` WHERE owner_id IS DISTINCT FROM 1`).Scan(&notOne); err != nil {
			t.Fatalf("%s: %v", tbl, err)
		}
		if notOne != 0 {
			t.Errorf("%s: %d rows are not owned by user 1 after the backfill", tbl, notOne)
		}
		var nullable, def string
		if err := pool.QueryRow(ctx, `SELECT is_nullable, coalesce(column_default, '') FROM information_schema.columns
			WHERE table_schema='public' AND table_name=$1 AND column_name='owner_id'`, tbl).Scan(&nullable, &def); err != nil {
			t.Fatalf("%s.owner_id: %v", tbl, err)
		}
		if nullable != "NO" || def != "1" {
			t.Errorf("%s.owner_id: is_nullable=%s default=%q, want NO and 1", tbl, nullable, def)
		}
	}
	for _, tbl := range sharedTables {
		if got := countRows(ctx, t, pool, tbl); tbl != "schema_migrations" && got != before[tbl] {
			t.Errorf("shared %s: %d rows after 0006, %d before", tbl, got, before[tbl])
		}
	}

	// The links survived the FK swap exactly as they were.
	var agentNote *int64
	if err := pool.QueryRow(ctx, `SELECT note_id FROM agents WHERE id=$1`, worker).Scan(&agentNote); err != nil || agentNote == nil || *agentNote != note {
		t.Errorf("worker's note_id after 0006 = %v (%v), want %d", agentNote, err, note)
	}
	var login string
	var ct []byte
	if err := pool.QueryRow(ctx, `SELECT login, token_ciphertext FROM github_connection WHERE owner_id=1`).Scan(&login, &ct); err != nil {
		t.Fatalf("the GitHub connection is not owner 1's after 0006: %v", err)
	}
	if login != "octo" || string(ct) != "\xde\xad\xbe\xef" {
		t.Errorf("github_connection changed: login %q ciphertext %x", login, ct)
	}

	// The seeded operator, and an identity that continues after it.
	var role, display string
	if err := pool.QueryRow(ctx, `SELECT role, display_name FROM users WHERE id=1`).Scan(&role, &display); err != nil {
		t.Fatalf("user 1: %v", err)
	}
	if role != "admin" || display != "operator" || countRows(ctx, t, pool, "users") != 1 {
		t.Errorf("users after 0006: id 1 role %q display %q, %d rows; want admin/operator/1", role, display, countRows(ctx, t, pool, "users"))
	}
	if next := scanID(ctx, t, pool, `INSERT INTO users (role) VALUES ('member') RETURNING id`); next != 2 {
		t.Errorf("first identity-minted user id = %d, want 2 (the sequence must continue after the seeded 1)", next)
	}
}

// TestMigration0006OwnerColumnLedger: exactly the owned tables carry owner_id,
// and the shared tables do not.
func TestMigration0006OwnerColumnLedger(t *testing.T) {
	ctx, pool := migrated(t)
	rows, err := pool.Query(ctx, `SELECT table_name FROM information_schema.columns
		WHERE table_schema='public' AND column_name='owner_id' ORDER BY table_name`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		got = append(got, n)
	}
	rows.Close()
	wantOwned := rlsTables()
	slices.Sort(wantOwned)
	if !equal(got, wantOwned) {
		t.Fatalf("tables with owner_id changed.\n got: %v\nwant: %v", got, wantOwned)
	}
	for _, tbl := range sharedTables {
		if slices.Contains(got, tbl) {
			t.Errorf("shared table %s has an owner_id", tbl)
		}
	}
}

// TestAccountsCompositeForeignKeyLedger pins every foreign key in the schema.
func TestAccountsCompositeForeignKeyLedger(t *testing.T) {
	ctx, pool := migrated(t)
	rows, err := pool.Query(ctx, `
		SELECT conrelid::regclass::text || '|' || conname || '|' || pg_get_constraintdef(oid)
		  FROM pg_constraint
		 WHERE contype = 'f' AND connamespace = 'public'::regnamespace
		 ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	rows.Close()
	if len(got) == 0 {
		t.Fatal("the catalogue returned no foreign keys at all; the query is not observing the schema")
	}
	want := slices.Clone(wantForeignKeys)
	slices.Sort(want)
	if !equal(got, want) {
		for _, g := range got {
			if !slices.Contains(want, g) {
				t.Errorf("unexpected FK: %s", g)
			}
		}
		for _, w := range want {
			if !slices.Contains(got, w) {
				t.Errorf("missing FK: %s", w)
			}
		}
	}
}

// TestCrossOwnerLinksAreRefusedByTheCompositeForeignKeys is the property the
// composite FKs exist for: for EVERY owned parent->child link, a child owned by
// user 2 cannot point at a parent owned by user 1. Each case asserts the
// violation comes from THAT link's constraint (user 2 exists, so the plain
// owner->users FK cannot be what fires), and then the same insert as owner 1
// succeeds — so the refusal is about the owner, not about the row.
func TestCrossOwnerLinksAreRefusedByTheCompositeForeignKeys(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshMigrated(ctx, t, "ms_0006_cross_owner")
	two := scanID(ctx, t, pool, `INSERT INTO users (role, display_name) VALUES ('member', 'b') RETURNING id`)

	note := scanID(ctx, t, pool, `INSERT INTO notes (title) VALUES ('a task') RETURNING id`) // owner 1 by DEFAULT
	agent := scanID(ctx, t, pool, `INSERT INTO agents (name, namespace) VALUES ('a-agent', 'ns-a') RETURNING id`)
	session := scanID(ctx, t, pool, `INSERT INTO chat_sessions (agent_id, session_key) VALUES ($1, 'k-a') RETURNING id`, agent)

	cases := []struct {
		fk   string
		sql  string
		args func(owner int64) []any
	}{
		{"note_comments_note_owner_fkey", `INSERT INTO note_comments (note_id, owner_id, body) VALUES ($1, $2, 'x')`,
			func(o int64) []any { return []any{note, o} }},
		{"note_attachments_note_owner_fkey", `INSERT INTO note_attachments (note_id, owner_id, filename, size_bytes, data) VALUES ($1, $2, 'f', 1, '\x00')`,
			func(o int64) []any { return []any{note, o} }},
		{"task_sessions_note_owner_fkey", `INSERT INTO task_sessions (note_id, owner_id, session_id, role) VALUES ($1, $2, 'sess-' || $2::bigint, 'read')`,
			func(o int64) []any { return []any{note, o} }},
		{"agents_note_owner_fkey", `INSERT INTO agents (note_id, owner_id, name, namespace) VALUES ($1, $2, 'agent-of-' || $2::bigint, 'ns')`,
			func(o int64) []any { return []any{note, o} }},
		{"chat_sessions_agent_owner_fkey", `INSERT INTO chat_sessions (agent_id, owner_id, session_key) VALUES ($1, $2, 'key-' || $2::bigint)`,
			func(o int64) []any { return []any{agent, o} }},
		{"chat_messages_agent_owner_fkey", `INSERT INTO chat_messages (agent_id, owner_id, role) VALUES ($1, $2, 'user')`,
			func(o int64) []any { return []any{agent, o} }},
		{"connector_grants_agent_owner_fkey", `INSERT INTO connector_grants (agent_id, owner_id, connector_name, status) VALUES ($1, $2, 'c-' || $2::bigint, 'denied')`,
			func(o int64) []any { return []any{agent, o} }},
	}
	for _, c := range cases {
		t.Run(c.fk, func(t *testing.T) {
			_, err := pool.Exec(ctx, c.sql, c.args(two)...)
			fkViolation(t, err, c.fk)
			if _, err := pool.Exec(ctx, c.sql, c.args(1)...); err != nil {
				t.Fatalf("control: the same insert as the parent's owner failed: %v", err)
			}
		})
	}

	// chat_messages.session_id: the agent link must agree for the insert to reach
	// the session check, so the message is owner 2 on an owner-2 agent and points
	// at owner 1's session.
	t.Run("chat_messages_session_owner_fkey", func(t *testing.T) {
		agentB := scanID(ctx, t, pool, `INSERT INTO agents (name, namespace, owner_id) VALUES ('b-agent', 'ns-b', $1) RETURNING id`, two)
		_, err := pool.Exec(ctx, `INSERT INTO chat_messages (agent_id, session_id, owner_id, role) VALUES ($1, $2, $3, 'user')`, agentB, session, two)
		fkViolation(t, err, "chat_messages_session_owner_fkey")
		sessionB := scanID(ctx, t, pool, `INSERT INTO chat_sessions (agent_id, session_key, owner_id) VALUES ($1, 'k-b', $2) RETURNING id`, agentB, two)
		mustExec(ctx, t, pool, `INSERT INTO chat_messages (agent_id, session_id, owner_id, role) VALUES ($1, $2, $3, 'user')`, agentB, sessionB, two)
	})

	// NULL parent references stay legal under MATCH SIMPLE (MATCH FULL would
	// refuse a NULL beside the NOT NULL owner_id).
	t.Run("null_parent_refs_are_unchecked", func(t *testing.T) {
		agentB := scanID(ctx, t, pool, `INSERT INTO agents (name, namespace, owner_id, note_id) VALUES ('b-free', 'ns-bf', $1, NULL) RETURNING id`, two)
		mustExec(ctx, t, pool, `INSERT INTO chat_messages (agent_id, session_id, owner_id, role) VALUES ($1, NULL, $2, 'user')`, agentB, two)
	})
}

// TestDeletingATaskDetachesItsAgentAndKeepsTheAgentsOwner: 0001's
// ON DELETE SET NULL, carried onto the composite FK. A bare SET NULL would null
// owner_id too and the DELETE would fail on NOT NULL.
func TestDeletingATaskDetachesItsAgentAndKeepsTheAgentsOwner(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshMigrated(ctx, t, "ms_0006_delete_task")
	note := scanID(ctx, t, pool, `INSERT INTO notes (title) VALUES ('t') RETURNING id`)
	agent := scanID(ctx, t, pool, `INSERT INTO agents (name, namespace, note_id) VALUES ('w', 'ns', $1) RETURNING id`, note)
	mustExec(ctx, t, pool, `INSERT INTO note_comments (note_id, body) VALUES ($1, 'c')`, note)

	if _, err := pool.Exec(ctx, `DELETE FROM notes WHERE id=$1`, note); err != nil {
		t.Fatalf("deleting a task an agent worked failed: %v", err)
	}
	var noteID *int64
	var owner int64
	if err := pool.QueryRow(ctx, `SELECT note_id, owner_id FROM agents WHERE id=$1`, agent).Scan(&noteID, &owner); err != nil {
		t.Fatalf("the agent was deleted with its task: %v", err)
	}
	if noteID != nil || owner != 1 {
		t.Errorf("agent after its task was deleted: note_id %v owner %d, want NULL and 1", noteID, owner)
	}
	if n := countRows(ctx, t, pool, "note_comments"); n != 0 {
		t.Errorf("comments of a deleted task: %d, want 0 (CASCADE)", n)
	}
}

// TestConnectorNamesAreUniquePerOwner.
func TestConnectorNamesAreUniquePerOwner(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshMigrated(ctx, t, "ms_0006_connectors")
	two := scanID(ctx, t, pool, `INSERT INTO users (role) VALUES ('member') RETURNING id`)
	const ins = `INSERT INTO connectors (owner_id, name, hosts, auth_scheme, secret_ciphertext) VALUES ($1, 'clickup', ARRAY['api.clickup.com'], 'bearer', '\x00')`
	mustExec(ctx, t, pool, ins, int64(1))
	if _, err := pool.Exec(ctx, ins, two); err != nil {
		t.Fatalf("a second owner could not create a connector with the same name: %v", err)
	}
	_, err := pool.Exec(ctx, ins, two)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != "connectors_owner_name_key" {
		t.Fatalf("the same owner's duplicate name: got %v, want 23505 from connectors_owner_name_key", err)
	}
}

// TestGitHubConnectionStoreKeepsItsSingleUserBehaviourAfter0006 drives the real
// internal/github.PGStore against the re-keyed table: Get on empty, Save, Get,
// upsert, Clear — all as owner 1, with one row — and shows the store is keyed by
// owner rather than "the only row": another owner's row is neither read nor
// cleared.
func TestGitHubConnectionStoreKeepsItsSingleUserBehaviourAfter0006(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshMigrated(ctx, t, "ms_0006_github")
	st, err := github.NewPG(pool, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	// The store acts for the PRINCIPAL's user since accounts PR 3b (it was the
	// constant 1); the operator's principal keeps this test's single-user
	// claims. The connection itself is still the migrator's.
	op := asUser(ctx, 1)
	if _, ok, err := st.Get(op); err != nil || ok {
		t.Fatalf("Get on an empty table: ok=%v err=%v, want false/nil", ok, err)
	}
	if err := st.Save(op, "octo", "repo", "gho_first"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := st.Save(op, "octo2", "repo,read:org", "gho_second"); err != nil {
		t.Fatalf("second Save (upsert): %v", err)
	}
	c, ok, err := st.Get(op)
	if err != nil || !ok || c.Login != "octo2" || c.Scopes != "repo,read:org" || c.Token != "gho_second" {
		t.Fatalf("Get after upsert: %+v ok=%v err=%v", c, ok, err)
	}
	var owner int64
	if err := pool.QueryRow(ctx, `SELECT owner_id FROM github_connection`).Scan(&owner); err != nil || owner != 1 {
		t.Fatalf("the store wrote owner %d (%v); want exactly one row, owner 1", owner, err)
	}

	two := scanID(ctx, t, pool, `INSERT INTO users (role) VALUES ('member') RETURNING id`)
	mustExec(ctx, t, pool, `INSERT INTO github_connection (owner_id, login, token_ciphertext) VALUES ($1, 'someone-else', '\x00')`, two)
	if c, _, err := st.Get(op); err != nil || c.Login != "octo2" {
		t.Fatalf("Get with a second owner's row present: %+v %v", c, err)
	}
	if err := st.Clear(op); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.Get(op); ok {
		t.Fatal("Clear left owner 1's connection")
	}
	if n := countRows(ctx, t, pool, "github_connection"); n != 1 {
		t.Fatalf("Clear removed %d rows of another owner; want that row kept", 1-n)
	}
}

// TestMigration0006BodyReRunsCleanly: 0006 follows the set's IF [NOT] EXISTS
// convention, so re-applying its body by hand (a restore, a rebuild) on an
// already-migrated database is a no-op, not an error, and leaves the FK ledger
// as it was.
func TestMigration0006BodyReRunsCleanly(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshMigrated(ctx, t, "ms_0006_rerun")
	body, err := os.ReadFile(filepath.Join("migrations", "0006_accounts_schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, string(body)); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("re-running 0006 failed: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countRows(ctx, t, pool, "users"); n != 1 {
		t.Errorf("users after a re-run: %d, want 1 (the seed must not duplicate)", n)
	}
	var fks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE contype='f' AND connamespace='public'::regnamespace`).Scan(&fks); err != nil {
		t.Fatal(err)
	}
	if fks != len(wantForeignKeys) {
		t.Errorf("FKs after a re-run: %d, want %d", fks, len(wantForeignKeys))
	}
}
