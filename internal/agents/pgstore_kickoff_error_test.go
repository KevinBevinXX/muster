package agents

import (
	"context"
	"github.com/ZacxDev/muster/internal/dbtest"
	"testing"
	"time"
)

// TestSetKickoffErrorDoesNotMoveTheDwellClock pins the property
// DecideReconcile's ActionRetryKickoff → ActionError escalation depends on:
// recording a failed kickoff send must NOT reset agents.updated_at.
//
// 🔴 WHY IT MATTERS ONLY NOW: DecideReconcile bounds the retry branch by
// `now.Sub(a.UpdatedAt) > ProvisioningStuckTimeout` and nothing else. With
// SetKickoffError writing `updated_at=now()`, every failed retry restarted that
// clock, so an agent whose send failed on every tick was retried for ever and
// ActionError was unreachable. The defect was latent while nothing drove the
// table; the kickoff deliverer (internal/agentkickoff) drives it, which is what
// made it live.
//
// ⚠ RecordKickoffDelivery IS DELIBERATELY STILL A BUMP and the control below
// asserts it: a delivery IS a state transition, and the asymmetry is the point.
func TestSetKickoffErrorDoesNotMoveTheDwellClock(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	store := requirePGStore(t, ctx)

	name := "dwell-clock-" + time.Now().Format("150405.000000")
	created, err := store.Create(ctx, Agent{
		Name: name, Namespace: "muster-agent-" + name, Status: StatusProvisioning,
		PendingNote: "fixture first turn",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer cleanupAgent(ctx, store.pool, created.ID)

	// Pin updated_at well in the past, so "unchanged" and "bumped to now()" are
	// minutes apart rather than separated by clock resolution.
	if _, err := store.pool.Exec(ctx,
		`UPDATE agents SET updated_at = now() - interval '14 minutes' WHERE id=$1`, created.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	before, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	const n = 3
	for i := 0; i < n; i++ {
		if err := store.SetKickoffError(ctx, created.ID, "send failed, attempt "+string(rune('a'+i))); err != nil {
			t.Fatalf("SetKickoffError #%d: %v", i, err)
		}
	}
	after, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// INSTRUMENT CHECK: the writes landed — otherwise "unchanged" is vacuous.
	if after.KickoffError != "send failed, attempt c" {
		t.Fatalf("instrument check FAILED: kickoff_error = %q after %d writes, want the last one",
			after.KickoffError, n)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("%d SetKickoffError calls moved updated_at from %s to %s (Δ %s).\n"+
			"    DecideReconcile bounds ActionRetryKickoff ONLY by now − updated_at, so a send "+
			"that fails every tick resets the clock every tick and ActionError is unreachable: "+
			"the agent sits in provisioning for ever.", n, before.UpdatedAt, after.UpdatedAt,
			after.UpdatedAt.Sub(before.UpdatedAt))
	}

	// CONTROL: the same instrument DOES see a bump — RecordKickoffDelivery is meant
	// to move it. Without this the Equal above could pass over a Get that returned a
	// cached or wrong column.
	if err := store.RecordKickoffDelivery(ctx, created.ID, "pod-x", 0); err != nil {
		t.Fatalf("RecordKickoffDelivery: %v", err)
	}
	delivered, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// Measured against the BACKDATED value, not `after`, so the control's verdict
	// does not depend on whether the defect above is present.
	if !delivered.UpdatedAt.After(before.UpdatedAt.Add(10 * time.Minute)) {
		t.Errorf("control FAILED: RecordKickoffDelivery did not bump updated_at (%s → %s), so "+
			"the instrument cannot tell a bump from no bump", before.UpdatedAt, delivered.UpdatedAt)
	}
	if delivered.KickoffError != "" {
		t.Errorf("RecordKickoffDelivery left kickoff_error = %q, want it cleared", delivered.KickoffError)
	}
}
