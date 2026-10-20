package agents

import (
	"testing"
	"time"
)

// TestARaisedCheckoutWaitExtendsAnAgentAlreadyWaiting: MUSTER_CHECKOUT_WAIT_TIMEOUT
// (SetCheckoutWaitTimeout) moves DecideFirstTurn's bound, measured from the
// row's updated_at, so an agent already 30 h into a wait is held, not errored,
// once the bound is 72 h — and errors past it. Two points either side of each
// bound (24 h default, 72 h raised).
//
// Not parallel: it writes the package bound, and restores it.
func TestARaisedCheckoutWaitExtendsAnAgentAlreadyWaiting(t *testing.T) {
	t.Cleanup(func() { SetCheckoutWaitTimeout(0) })
	now := time.Date(2026, 3, 9, 14, 0, 0, 0, time.UTC)
	grant := KickoffAwaitingGrantReason + ": approve the github request"
	decide := func(dwell time.Duration) ReconcileAction {
		return DecideFirstTurn(repoAgent(now, dwell, grant), readyInst(), CheckoutAwaitingGrant, now)
	}
	if got := decide(30 * time.Hour); got != ActionErrorCheckoutWait {
		t.Fatalf("default bound, 30h: %s, want %s", got, ActionErrorCheckoutWait)
	}
	SetCheckoutWaitTimeout(72 * time.Hour)
	for _, c := range []struct {
		dwell time.Duration
		want  ReconcileAction
	}{
		{23 * time.Hour, ActionWaitCheckout},
		{30 * time.Hour, ActionWaitCheckout},
		{72*time.Hour - time.Minute, ActionWaitCheckout},
		{72*time.Hour + time.Minute, ActionErrorCheckoutWait},
	} {
		if got := decide(c.dwell); got != c.want {
			t.Errorf("bound 72h, dwell %s: %s, want %s", c.dwell, got, c.want)
		}
	}
	SetCheckoutWaitTimeout(0)
	if CheckoutWaitTimeout != DefaultCheckoutWaitTimeout {
		t.Fatalf("SetCheckoutWaitTimeout(0) left %s", CheckoutWaitTimeout)
	}
}
