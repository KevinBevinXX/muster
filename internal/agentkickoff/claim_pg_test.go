package agentkickoff

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/provision"
)

// gatedPG is the REAL Postgres store with two seams: it records each ClaimKickoff
// verdict, and the FIRST LatestOrCreateSession blocks until released. That call is
// the last step before the stamp, so holding it parks delivery #1 exactly where the
// claim is the ONLY thing between a second same-process tick and a second paid turn:
// claim held, row re-read, kicked_off still false.
type gatedPG struct {
	*agents.PGStore
	mu      sync.Mutex
	claims  []bool
	parked  chan struct{} // closed when delivery #1 is parked
	release chan struct{} // close to let delivery #1 continue
	gated   bool
	claimed chan bool // receives each claim verdict
}

func (g *gatedPG) ClaimKickoff(ctx context.Context, id int64, owner string, ttl time.Duration) (bool, error) {
	won, err := g.PGStore.ClaimKickoff(ctx, id, owner, ttl)
	g.mu.Lock()
	g.claims = append(g.claims, won)
	g.mu.Unlock()
	if err == nil {
		g.claimed <- won
	}
	return won, err
}

func (g *gatedPG) LatestOrCreateSession(ctx context.Context, agentID int64, name string) (agents.ChatSession, error) {
	g.mu.Lock()
	first := !g.gated
	g.gated = true
	g.mu.Unlock()
	if first {
		close(g.parked)
		<-g.release
	}
	return g.PGStore.LatestOrCreateSession(ctx, agentID, name)
}

// TestASecondTickInTheSameProcessDoesNotRunASecondTurn is the D2 guard (PR #37 review
// round 0): the in-process in-flight map was DELETED on the argument that the
// per-agent claim already makes a second same-process tick a no-op. This holds that
// argument to the REAL claim SQL rather than to a fake that merely says "lost".
//
// SHAPE: tick 1 starts delivery #1, which takes the claim and parks just before the
// stamp. Tick 2 — same Deliverer, same owner string — lists the row (still owed:
// not stamped yet) and starts delivery #2. ClaimKickoff wins only on an unheld or
// EXPIRED claim and does not special-case its own owner, so #2 must LOSE and return.
// Then #1 is released and pays the one turn.
//
// POSITIVE CONTROL: the claim verdicts are asserted as the pair [won, lost], so "one
// turn" cannot be satisfied by a tick 2 that never reached the claim at all.
func TestASecondTickInTheSameProcessDoesNotRunASecondTurn(t *testing.T) {
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

	prefix := "claim-" + time.Now().Format("150405") + "-"
	row, err := store.Create(ctx, agents.Agent{
		Name: "tidy-shrike", Namespace: prefix + "tidy-shrike", Status: agents.StatusProvisioning,
		PendingNote: "summarise the open pull requests", HooksToken: "tok-shrike-61d0",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(dbtest.AsOperator(context.Background()), `DELETE FROM agents WHERE id=$1`, row.ID)
	})

	log := &recorder{}
	gw := &fakeGateway{log: log, reply: "4 open pull requests"}
	d, err := New(Config{
		Store: store, Gateway: gw, NamespacePrefix: prefix, Owner: "same-process/1",
		Instances: &fakeInstances{insts: []provision.Instance{readyInstance("tidy-shrike", "tidy-shrike-pod", 0)}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := d.Tick(ctx); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	select {
	case <-store.parked:
	case <-ctx.Done():
		t.Fatal("instrument check FAILED: delivery #1 never reached the pre-stamp park point")
	}
	if won := <-store.claimed; !won {
		t.Fatal("instrument check FAILED: delivery #1 did not win the claim on an unclaimed row")
	}

	if err := d.Tick(ctx); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	select {
	case won := <-store.claimed:
		if won {
			t.Errorf("a SECOND tick in the same process WON the kickoff claim while delivery #1 " +
				"still held it — the claim does not single-flight a process against itself, " +
				"which is the guarantee the deleted in-flight map was removed on.")
		}
	case <-ctx.Done():
		t.Fatal("instrument check FAILED: tick 2 never attempted the claim, so this test " +
			"would pass over a second tick that did nothing for an unrelated reason")
	}

	close(store.release)
	d.Wait()

	if n := gw.callCount(); n != 1 {
		t.Errorf("two ticks in one process ran %d paid turn(s) for one agent, want exactly 1.%s",
			n, transcript(log.snapshot()))
	}
	store.mu.Lock()
	claims := append([]bool(nil), store.claims...)
	store.mu.Unlock()
	if len(claims) != 2 || !claims[0] || claims[1] {
		t.Errorf("claim verdicts = %v, want [true false] (delivery #1 won, delivery #2 lost)", claims)
	}
	got, err := store.Get(ctx, row.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.KickedOff || got.KickoffAttempts != 1 {
		t.Errorf("after the race: kicked_off=%t attempts=%d, want true / 1", got.KickedOff, got.KickoffAttempts)
	}
}
