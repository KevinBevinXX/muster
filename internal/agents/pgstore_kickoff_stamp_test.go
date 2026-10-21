package agents

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestTheStampClearsACheckoutWaitInTheSameWrite (#153): SetKickedOff(true) on a
// row holding a checkout-wait marker (either one) clears it in the stamp's own
// write — so no separate earlier clear exists for a failed stamp to strand —
// while any other kickoff_error survives the stamp, and un-stamping clears
// nothing.
func TestTheStampClearsACheckoutWaitInTheSameWrite(t *testing.T) {
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := NewPG(pool)
	suffix := time.Now().Format("150405.000000")

	cases := []struct {
		name, before, after string
		stamp               bool
	}{
		{"grant", KickoffAwaitingGrantReason + ": approve the request", "", true},
		{"clone", KickoffAwaitingCloneReason + " (attempt 3)", "", true},
		{"other", "kickoff not sent: could not resolve the agent's gateway: boom", "kickoff not sent: could not resolve the agent's gateway: boom", true},
		{"unstamp", KickoffAwaitingCloneReason, KickoffAwaitingCloneReason, false},
	}
	for _, c := range cases {
		name := "stamp-" + c.name + "-" + suffix
		row, err := store.Create(ctx, Agent{Name: name, Namespace: "devpod-" + name, Status: StatusRunning})
		if err != nil {
			t.Fatalf("%s: create: %v", c.name, err)
		}
		defer cleanupAgent(ctx, pool, row.ID)
		if err := store.SetKickoffError(ctx, row.ID, c.before); err != nil {
			t.Fatalf("%s: seed: %v", c.name, err)
		}
		if err := store.SetKickedOff(ctx, row.ID, c.stamp); err != nil {
			t.Fatalf("%s: stamp: %v", c.name, err)
		}
		got, err := store.Get(ctx, row.ID)
		if err != nil {
			t.Fatalf("%s: get: %v", c.name, err)
		}
		if got.KickedOff != c.stamp || got.KickoffError != c.after {
			t.Fatalf("%s: kicked_off=%t kickoff_error=%q; want %t/%q", c.name, got.KickedOff, got.KickoffError, c.stamp, c.after)
		}
	}
}
