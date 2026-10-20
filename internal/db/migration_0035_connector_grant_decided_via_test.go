package db_test

import (
	"strings"
	"testing"
)

// TestMigration0035RecordsWhereAConnectorDecisionWasMade: at version 34 a
// grant has no decision provenance (the control: the column is absent); 0035
// adds it with NO backfill — a decision recorded before it reads NULL, i.e.
// the Connectors tab — and its CHECK allows provenance only all-or-nothing,
// only 'mcp', only on a DECIDED grant (approved or denied: never pending or
// revoked), and only with the mirror wire's host and session shapes.
func TestMigration0035RecordsWhereAConnectorDecisionWasMade(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0035_decided_via", 34)
	const sid = "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b"
	agent := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'grant-agent', 'ns-grant') RETURNING id`)
	n := 0
	grant := func(status string) int64 {
		n++ // one live grant per (agent, connector): a fresh connector name each time
		return scanID(ctx, t, pool, `INSERT INTO connector_grants (owner_id, agent_id, connector_name, status) VALUES (1, $1, $2, $3) RETURNING id`,
			agent, "c"+strings.Repeat("x", n), status)
	}
	old := grant("approved")
	if _, err := pool.Exec(ctx, `UPDATE connector_grants SET decided_via = 'mcp' WHERE id = $1`, old); err == nil || !strings.Contains(err.Error(), "decided_via") {
		t.Fatalf("control: at version 34 the decided_via column already exists: %v", err)
	}

	applyThroughFrom(ctx, t, pool, 35)

	var via *string
	if err := pool.QueryRow(ctx, `SELECT decided_via FROM connector_grants WHERE id = $1`, old).Scan(&via); err != nil || via != nil {
		t.Fatalf("a decision from before 0035 has decided_via %v (%v); want NULL — no backfill", via, err)
	}
	set := `UPDATE connector_grants SET decided_via = $2, decided_via_host = $3, decided_via_session = $4 WHERE id = $1`
	refused := func(what string, id int64, v, host, sess any) {
		t.Helper()
		if _, err := pool.Exec(ctx, set, id, v, host, sess); err == nil || !strings.Contains(err.Error(), "connector_grants_decided_via_check") {
			t.Errorf("%s was not refused by the provenance CHECK: %v", what, err)
		}
	}
	refused("provenance on a pending grant", grant("pending"), "mcp", "ws", sid)
	refused("provenance on a revoked grant", grant("revoked"), "mcp", "ws", sid)
	refused("an unknown via", grant("approved"), "browser", "ws", sid)
	refused("via without a host", grant("approved"), "mcp", nil, sid)
	refused("via without a session", grant("approved"), "mcp", "ws", nil)
	refused("a host and session with no via", grant("approved"), nil, "ws", sid)
	refused("a host the wire would refuse", grant("approved"), "mcp", "WS ws", sid)
	refused("a session the wire would refuse", grant("approved"), "mcp", "ws", "../etc")
	for _, st := range []string{"approved", "denied"} {
		if _, err := pool.Exec(ctx, set, grant(st), "mcp", "ws", sid); err != nil {
			t.Errorf("after 0035, an MCP decision on a %s grant: %v", st, err)
		}
	}
}

// The statements a pre-0035 image runs on a grant (internal/connectors
// pgstore.go at 85e30d2, verbatim): none of them names the 0035 columns.
const (
	oldDecideSQL = `UPDATE connector_grants SET status=$2, decided_by=$3, decided_at=now(), source=$4
		WHERE id=$1 AND status='pending'`
	oldReopenSQL = `UPDATE connector_grants SET status='pending', decided_by='', decided_at=NULL, source='stored'
		WHERE id=$1 AND status='approved'`
	oldRevokeSQL = `UPDATE connector_grants SET status='revoked', decided_by=$2, decided_at=now()
		WHERE id=$1 AND status IN ('pending', 'approved')`
)

// readMigrationCheck is 0035's own ADD CONSTRAINT statement (the body, not the
// header): the CHECK a roll-forward must bring back.
func readMigrationCheck(t *testing.T) string {
	t.Helper()
	src := readMigration(t, "0035_connector_grant_decided_via.sql")
	i := strings.LastIndex(src, "ALTER TABLE public.connector_grants\n    ADD CONSTRAINT")
	if i < 0 {
		t.Fatal("migration 0035 has no ADD CONSTRAINT statement")
	}
	return src[i:]
}

// headerSQL is the SQL between "-- [tag]" and "-- [/tag]" in migration 0035's
// header, with each line's comment prefix removed: EXACTLY what the rollback
// note tells an operator to run.
func headerSQL(t *testing.T, tag string) string {
	t.Helper()
	src := readMigration(t, "0035_connector_grant_decided_via.sql")
	_, rest, ok := strings.Cut(src, "-- ["+tag+"]\n")
	body, _, ok2 := strings.Cut(rest, "-- [/"+tag+"]")
	if !ok || !ok2 {
		t.Fatalf("migration 0035's header has no [%s] block", tag)
	}
	var out []string
	for _, l := range strings.Split(body, "\n") {
		if l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "--")); l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// TestMigration0035RollbackAndRestore runs migration 0035's documented
// rollback and restore SQL, as written in its header, against a populated
// table, with the pre-0035 image's own statements in between:
//
//   - THE OVERLAP (measured here at the SQL level): with the CHECK in place, an
//     old image's revoke or re-open of a grant decided over MCP is refused.
//   - [rollback-sql] drops the CHECK and empties every provenance, so the old
//     image's revoke, and its re-open followed by a tab approval, work and leave
//     no stale "via MCP" on a tab decision.
//   - A new pod still serving after step 1 can record one more MCP decision,
//     which the old image then re-decides on the tab: a stale row. The CONTROL
//     shows it matters — the CHECK alone cannot be re-added over it — and
//     [restore-sql] empties it and brings back the very CHECK 0035 created.
func TestMigration0035RollbackAndRestore(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0035_rollback", 35)
	const sid = "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b"
	agent := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'rb-agent', 'ns-rb') RETURNING id`)
	n := 0
	mcpApproved := func() int64 {
		n++
		id := scanID(ctx, t, pool, `INSERT INTO connector_grants (owner_id, agent_id, connector_name) VALUES (1, $1, $2) RETURNING id`,
			agent, "rb"+strings.Repeat("x", n))
		mustExec(ctx, t, pool, `UPDATE connector_grants SET status = 'approved', decided_by = 'operator', decided_at = now(),
			decided_via = 'mcp', decided_via_host = 'ws', decided_via_session = $2 WHERE id = $1`, id, sid)
		return id
	}
	constraintDef := func() string {
		var def string
		_ = pool.QueryRow(ctx, `SELECT coalesce((SELECT pg_get_constraintdef(oid) FROM pg_constraint
			WHERE conname = 'connector_grants_decided_via_check'), '')`).Scan(&def)
		return def
	}
	stale := func() int {
		var c int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM connector_grants WHERE decided_via IS NOT NULL
			OR decided_via_host IS NOT NULL OR decided_via_session IS NOT NULL`).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	migrated := constraintDef()
	if migrated == "" {
		t.Fatal("control: at version 35 there is no provenance CHECK")
	}
	revoked, reopened := mcpApproved(), mcpApproved()

	// The overlap: an old pod's revoke and re-open, against the migrated schema.
	if _, err := pool.Exec(ctx, oldRevokeSQL, revoked, "operator"); err == nil || !strings.Contains(err.Error(), "connector_grants_decided_via_check") {
		t.Fatalf("an old image's revoke of an MCP-decided grant under the CHECK: %v, want refused", err)
	}
	if _, err := pool.Exec(ctx, oldReopenSQL, reopened); err == nil || !strings.Contains(err.Error(), "connector_grants_decided_via_check") {
		t.Fatalf("an old image's re-open of an MCP-decided grant under the CHECK: %v, want refused", err)
	}

	// Step 1, as documented.
	mustExec(ctx, t, pool, headerSQL(t, "rollback-sql"))
	if constraintDef() != "" || stale() != 0 {
		t.Fatalf("after [rollback-sql]: CHECK %q, %d rows still carry provenance", constraintDef(), stale())
	}
	// Step 2: the old image works, and its tab decisions carry no MCP source.
	mustExec(ctx, t, pool, oldRevokeSQL, revoked, "operator")
	mustExec(ctx, t, pool, oldReopenSQL, reopened)
	mustExec(ctx, t, pool, oldDecideSQL, reopened, "approved", "operator", "stored")
	if stale() != 0 {
		t.Fatalf("the old image's tab approval of a re-opened grant shows %d stale MCP sources", stale())
	}
	// New pods not yet gone record two more MCP decisions; the old image
	// re-opens both (ReconcileGrant) and approves one again on the tab.
	window, pendingWindow := mcpApproved(), mcpApproved()
	mustExec(ctx, t, pool, oldReopenSQL, window)
	mustExec(ctx, t, pool, oldDecideSQL, window, "approved", "operator", "stored")
	mustExec(ctx, t, pool, oldReopenSQL, pendingWindow)
	if stale() != 2 {
		t.Fatalf("control: the window's re-decided grants are not stale (%d)", stale())
	}
	// CONTROL: 0035's own CHECK cannot be re-added over them — the pending one
	// carries an MCP source — so the restore's UPDATE is load-bearing.
	if _, err := pool.Exec(ctx, readMigrationCheck(t)); err == nil || !strings.Contains(err.Error(), "connector_grants_decided_via_check") {
		t.Fatalf("control: 0035's CHECK was re-added over a stale pending row: %v", err)
	}

	// Step 3, as documented.
	mustExec(ctx, t, pool, headerSQL(t, "restore-sql"))
	if got := constraintDef(); got != migrated {
		t.Fatalf("[restore-sql] restored CHECK\n  %s\nwant the migrated one\n  %s", got, migrated)
	}
	if stale() != 0 {
		t.Fatalf("after [restore-sql] %d rows carry provenance", stale())
	}
	// The restored CHECK holds: an MCP source on a pending row is refused.
	mustExec(ctx, t, pool, oldReopenSQL, window)
	if _, err := pool.Exec(ctx, `UPDATE connector_grants SET decided_via = 'mcp', decided_via_host = 'ws', decided_via_session = $2 WHERE id = $1`,
		window, sid); err == nil || !strings.Contains(err.Error(), "connector_grants_decided_via_check") {
		t.Fatalf("the restored CHECK admitted provenance on a pending grant: %v", err)
	}
}
