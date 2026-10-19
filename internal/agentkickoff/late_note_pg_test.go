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

// TestANoteAppendedBeforeTheStampRidesTheFirstTurn (task routing, audit
// round 2): a forward appended to the pending note AFTER the delivery re-read
// the row but BEFORE it stamped kicked_off is in the turn that is sent. The
// delivery is parked at its last pre-stamp step (gatedPG) while the note grows.
func TestANoteAppendedBeforeTheStampRidesTheFirstTurn(t *testing.T) {
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
	store := &gatedPG{
		PGStore: agents.NewPG(pool),
		parked:  make(chan struct{}), release: make(chan struct{}), claimed: make(chan bool, 4),
	}
	prefix := "late-" + time.Now().Format("150405") + "-"
	row, err := store.Create(ctx, agents.Agent{
		Name: "late-heron", Namespace: prefix + "late-heron", Status: agents.StatusProvisioning,
		PendingNote: "fix the flaky test", HooksToken: "tok-heron-7f1a",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(dbtest.AsOperator(context.Background()), `DELETE FROM agents WHERE id=$1`, row.ID)
	})
	log := &recorder{}
	gw := &fakeGateway{log: log, reply: "on it"}
	d, err := New(Config{
		Store: store, Gateway: gw, NamespacePrefix: prefix, Owner: "late/1",
		Instances: &fakeInstances{insts: []provision.Instance{readyInstance("late-heron", "late-heron-pod", 0)}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := d.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	select {
	case <-store.parked:
	case <-ctx.Done():
		t.Fatal("instrument check FAILED: the delivery never parked before the stamp")
	}
	<-store.claimed
	const late = "\n\n[a forward stored while the agent was starting]"
	// What agents.PGStore.AppendPendingNote runs (spelled here so the test also
	// runs against a store without it): only while kicked_off is false.
	tag, err := pool.Exec(ctx, `UPDATE agents SET pending_note = pending_note || $2 WHERE id = $1 AND NOT kicked_off AND pending_note <> ''`, row.ID, late)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("append while parked: %v (%d rows); want appended (not kicked off yet)", err, tag.RowsAffected())
	}
	close(store.release)
	d.Wait()
	var sent []string
	for _, line := range log.snapshot() {
		if strings.HasPrefix(line, "Chat(") {
			sent = append(sent, line)
		}
	}
	if len(sent) != 1 || !strings.HasSuffix(sent[0], ",fix the flaky test"+late+")") {
		t.Fatalf("first turn(s) %q; want one carrying the note with the late forward", sent)
	}
}
