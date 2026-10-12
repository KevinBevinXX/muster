package agentkickoff

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/provision"
)

// TestAFailureEveryTickReachesActionErrorAtTheDwellBoundary is the behavioural
// half of the SetKickoffError fix, over the REAL store: a row whose send fails on
// every tick must go red once it has dwelt past ProvisioningStuckTimeout.
//
// 🔴 WHY IT NEEDS POSTGRES: the defect was in the SQL — SetKickoffError bumped
// updated_at, and DecideReconcile's retry branch is bounded ONLY by now − updated_at.
// Every failed retry restarted the clock, so the escalation never fired. A fake
// store would only restate what its author believed the SQL does.
//
// SHAPE: the row is backdated to 1s short of the bound. Tick 1 (real now) sees a
// ready instance and retries; the send fails pre-stamp (no hooks token) and is
// recorded. Tick 2 runs 2s later: with the clock intact the row has dwelt 15m01s
// and is errored; with the clock reset by tick 1's write it has dwelt ~2s and is
// retried AGAIN — the forever-provisioning agent.
func TestAFailureEveryTickReachesActionErrorAtTheDwellBoundary(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := agents.NewPG(pool)

	prefix := "dwell-" + time.Now().Format("150405") + "-"
	row, err := store.Create(ctx, agents.Agent{
		Name: "quiet-ibis", Namespace: prefix + "quiet-ibis", Status: agents.StatusProvisioning,
		PendingNote: "count the open issues",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(dbtest.AsOperator(context.Background()), `DELETE FROM agents WHERE id=$1`, row.ID)
	})
	if _, err := pool.Exec(ctx, `UPDATE agents SET updated_at = now() - $2::interval WHERE id=$1`,
		row.ID, (agents.ProvisioningStuckTimeout - time.Second).String()); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	log := &recorder{}
	gw := &fakeGateway{log: log, reply: "never reached"}
	clock := time.Now()
	d, err := New(Config{
		Store: store, Gateway: gw, NamespacePrefix: prefix, Owner: "dwell-test/1",
		Instances: &fakeInstances{insts: []provision.Instance{readyInstance("quiet-ibis", "quiet-ibis-pod", 0)}},
		Now:       func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tick := func() agents.Agent {
		t.Helper()
		if err := d.Tick(ctx); err != nil {
			t.Fatalf("Tick: %v", err)
		}
		d.Wait()
		got, err := store.Get(ctx, row.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return got
	}

	first := tick()
	// INSTRUMENT CHECK: tick 1 really took the retry branch and really failed.
	if first.Status != agents.StatusProvisioning || !strings.Contains(first.KickoffError, "no hooks token") {
		t.Fatalf("instrument check FAILED: after tick 1 status=%s kickoff_error=%q; want "+
			"provisioning + the recorded send failure", first.Status, first.KickoffError)
	}

	clock = clock.Add(2 * time.Second)
	second := tick()
	if second.Status != agents.StatusError {
		t.Fatalf("a row whose send failed every tick is still %q after dwelling past %s "+
			"(updated_at now %s).\n    Its own recorded failure reset the dwell clock, so "+
			"ActionError is unreachable and the agent sits in provisioning for ever.",
			second.Status, agents.ProvisioningStuckTimeout, second.UpdatedAt)
	}
	if !strings.Contains(second.ErrorMessage, "last send error: kickoff not sent: the agent has no hooks token") {
		t.Errorf("the red card does not carry the recorded send failure: %q", second.ErrorMessage)
	}
	if gw.calls != 0 {
		t.Errorf("a turn ran for a row with no credential (%d calls)", gw.calls)
	}
}
