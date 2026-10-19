package db_test

import (
	"strings"
	"testing"
)

// TestMigration0031AddsTheAnswersDelivery: at version 30 an ask has no delivery
// state (the control: the column is absent); 0031 adds it with NO backfill — an
// answer recorded before it reads NULL, so it is never pushed — and its CHECK
// allows a delivery only on an answer, a lease exactly while delivering, and
// only the known states.
func TestMigration0031AddsTheAnswersDelivery(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0031_delivery", 30)
	agent := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'deliv-agent', 'ns-deliv') RETURNING id`)
	ask := func() int64 {
		return scanID(ctx, t, pool, `INSERT INTO attention_asks (owner_id, agent_id, question, expires_at) VALUES (1, $1, 'which way?', now() + interval '1 hour') RETURNING id`, agent)
	}
	old := ask()
	if _, err := pool.Exec(ctx, `UPDATE attention_asks SET status = 'answered', answer = 'left', answered_by = 'operator', resolved_at = now() WHERE id = $1`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE attention_asks SET delivery = 'queued' WHERE id = $1`, old); err == nil || !strings.Contains(err.Error(), "delivery") {
		t.Fatalf("control: at version 30 the delivery column already exists: %v", err)
	}

	applyThroughFrom(ctx, t, pool, 31)

	var delivery *string
	if err := pool.QueryRow(ctx, `SELECT delivery FROM attention_asks WHERE id = $1`, old).Scan(&delivery); err != nil || delivery != nil {
		t.Fatalf("an answer from before 0031 has delivery %v (%v); want NULL — no backfill", delivery, err)
	}
	answered := func() int64 {
		id := ask()
		if _, err := pool.Exec(ctx, `UPDATE attention_asks SET status = 'answered', answer = 'left', answered_by = 'operator', resolved_at = now() WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	refused := func(what, q string, id int64) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, id); err == nil || !strings.Contains(err.Error(), "attention_asks_delivery_check") {
			t.Errorf("%s was not refused by the delivery CHECK: %v", what, err)
		}
	}
	refused("a delivery on an open ask", `UPDATE attention_asks SET delivery = 'queued' WHERE id = $1`, ask())
	refused("an unknown delivery state", `UPDATE attention_asks SET delivery = 'sent' WHERE id = $1`, answered())
	refused("delivering without a lease", `UPDATE attention_asks SET delivery = 'delivering' WHERE id = $1`, answered())
	refused("a lease while not delivering", `UPDATE attention_asks SET delivery = 'queued', delivery_claimed_by = 'p', delivery_lease_until = now() WHERE id = $1`, answered())
	for _, q := range []string{
		`UPDATE attention_asks SET delivery = 'queued' WHERE id = $1`,
		`UPDATE attention_asks SET delivery = 'delivering', delivery_claimed_by = 'p', delivery_lease_until = now() + interval '1 minute' WHERE id = $1`,
		`UPDATE attention_asks SET delivery = 'pull' WHERE id = $1`,
	} {
		if _, err := pool.Exec(ctx, q, answered()); err != nil {
			t.Errorf("after 0031, %q: %v", q, err)
		}
	}
}
