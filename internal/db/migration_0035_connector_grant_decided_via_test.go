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
