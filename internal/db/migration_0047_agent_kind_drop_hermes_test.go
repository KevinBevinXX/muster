package db_test

import (
	"strings"
	"testing"
)

// TestMigration0047NarrowsTheKindCheckOverNonHermesRows: at version 46, a
// gateway and a claude-code row exist. 0047 applies, keeps both, and from then
// on a hermes row is refused by agents_kind_check (23514) while the remaining
// kinds are still admitted.
func TestMigration0047NarrowsTheKindCheckOverNonHermesRows(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0047_kind_ok", 46)
	mustExec(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace, status) VALUES (1, 'gw-0047', 'ns-a', 'stopped')`)
	mustExec(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace, status, kind, cc_account) VALUES (1, 'cc-0047', 'ns-b', 'stopped', 'claude-code', 'work')`)
	// The control for the refusal below: at 46 the CHECK still admits hermes.
	mustExec(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace, status, kind) VALUES (1, 'h-0047', 'ns-c', 'stopped', 'hermes')`)
	mustExec(ctx, t, pool, `DELETE FROM agents WHERE name = 'h-0047'`)

	applyThroughFrom(ctx, t, pool, 47)

	if n := countRows(ctx, t, pool, "agents"); n != 2 {
		t.Fatalf("0047 left %d agent rows, want the 2 it found", n)
	}
	_, err := pool.Exec(ctx, `INSERT INTO agents (owner_id, name, namespace, status, kind) VALUES (1, 'h2-0047', 'ns-d', 'stopped', 'hermes')`)
	if sqlState(err) != "23514" || !strings.Contains(err.Error(), "agents_kind_check") {
		t.Fatalf("a hermes row after 0047: %v, want an agents_kind_check violation", err)
	}
	mustExec(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace, status, kind, cc_account) VALUES (1, 'cc2-0047', 'ns-e', 'stopped', 'claude-code', 'work')`)
}

// TestMigration0047RefusesWhileAHermesRowExists: at version 46 with one hermes
// row, 0047's body fails with its own message (the count and the remedy) and
// changes nothing: the row is still there and still admitted, and the schema is
// still at 46.
func TestMigration0047RefusesWhileAHermesRowExists(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0047_kind_refused", 46)
	mustExec(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace, status, kind) VALUES (1, 'h-0047', 'ns-c', 'stopped', 'hermes')`)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, readMigration(t, "0047_agent_kind_drop_hermes.sql"))
	_ = tx.Rollback(ctx)
	if sqlState(err) != "23514" || !strings.Contains(err.Error(), "1 agent row(s) have kind 'hermes'") || !strings.Contains(err.Error(), "destroy them") {
		t.Fatalf("0047 over a hermes row: %v, want its own refusal naming the row count and the remedy", err)
	}
	if n := countRows(ctx, t, pool, "agents"); n != 1 {
		t.Fatalf("after the refused 0047, %d agent rows, want the hermes row kept", n)
	}
	var v int
	if err := pool.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&v); err != nil || v != 46 {
		t.Fatalf("schema version after the refused 0047: %d, %v; want 46", v, err)
	}
}
