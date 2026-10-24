package agentprovision

import (
	"context"
	"errors"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// TestALegacyHermesRowCanBeStoppedAndDestroyedButNeverStarted: an agent row of
// the REMOVED hermes kind (migration 0038 still admits it) over a live
// instance. Start and ReapplyProfiles are refused with agents.ErrRetiredKind
// before the driver is touched — no scale, no apply, no create, no status
// write — while Stop scales it to zero and Destroy removes the instance and the
// row, exactly as for any other kind.
func TestALegacyHermesRowCanBeStoppedAndDestroyedButNeverStarted(t *testing.T) {
	a, store, driver := newAdapter(t, nil)
	seedInstance(t, a, store, driver)
	store.agent.Kind = agents.RetiredKindHermes

	if err := a.Start(context.Background(), fixtureAgentID); !errors.Is(err, agents.ErrRetiredKind) {
		t.Fatalf("Start on a legacy hermes row: %v, want ErrRetiredKind", err)
	}
	if len(driver.scaled) != 0 || driver.updates != 0 || driver.creates != 0 || store.called("UpdateStatus") {
		t.Fatalf("a refused Start touched something: scaled %v, updates %d, creates %d; store %s",
			driver.scaled, driver.updates, driver.creates, store.transcript())
	}
	if err := a.ReapplyProfiles(context.Background(), fixtureAgentID); !errors.Is(err, agents.ErrRetiredKind) || driver.updates != 0 {
		t.Fatalf("ReapplyProfiles on a legacy hermes row: %v, %d update(s)", err, driver.updates)
	}

	if err := a.Stop(context.Background(), fixtureAgentID); err != nil {
		t.Fatalf("Stop on a legacy hermes row: %v", err)
	}
	if len(driver.scaled) != 1 || driver.scaled[0] != 0 {
		t.Fatalf("Stop did not scale to zero: %v", driver.scaled)
	}
	if err := a.Destroy(context.Background(), fixtureAgentID); err != nil {
		t.Fatalf("Destroy on a legacy hermes row: %v", err)
	}
	if driver.destroys != 1 || !store.called("Delete") {
		t.Fatalf("Destroy did not remove the instance and the row: %d destroy(s); store %s", driver.destroys, store.transcript())
	}
}
