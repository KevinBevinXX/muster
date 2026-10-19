package agents

import (
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/provision"
)

// repoAgent is a claude-code agent dispatched with a repository whose first turn
// is owed, last moved `dwell` before now.
func repoAgent(now time.Time, dwell time.Duration, kickoffErr string) Agent {
	return Agent{
		Name: "amber-heron", Kind: KindClaudeCode, CCAccount: "acct-r7", Repo: "example-org/tide-charts",
		Status: StatusRunning, PendingNote: "count the open issues in tide-charts",
		KickoffError: kickoffErr, UpdatedAt: now.Add(-dwell),
	}
}

func readyInst() *provision.Instance {
	return &provision.Instance{Ref: provision.Ref{Name: "amber-heron"}, InstanceID: "amber-heron-6d9-q2",
		Phase: provision.PhaseRunning, Ready: true, Replicas: 1}
}

// TestDecideFirstTurnHoldsATurnWhoseCheckoutIsComing is the decision table for a
// first turn that waits on its repository checkout. The dwell points straddle
// BOTH bounds — inside 15 min, between 15 min and 24 h (where the old table
// already errors), and past 24 h — so a mutant that reuses either bound for the
// other is visible.
func TestDecideFirstTurnHoldsATurnWhoseCheckoutIsComing(t *testing.T) {
	now := time.Date(2026, 3, 9, 14, 0, 0, 0, time.UTC)
	const (
		young  = 4 * time.Minute
		middle = 7 * time.Hour // > ProvisioningStuckTimeout, < CheckoutWaitTimeout
		old    = 31 * time.Hour
	)
	grant := KickoffAwaitingGrantReason + ": the repository could not be cloned"
	clone := KickoffAwaitingCloneReason
	for _, c := range []struct {
		name     string
		dwell    time.Duration
		marker   string
		checkout string
		want     ReconcileAction
	}{
		// Waiting for the grant (or the clone): no kickoff, not stuck, inside 24 h.
		{"awaiting grant, young", young, "", CheckoutAwaitingGrant, ActionWaitCheckout},
		{"awaiting grant, past 15m", middle, grant, CheckoutAwaitingGrant, ActionWaitCheckout},
		{"awaiting grant, first seen past 15m", middle, "", CheckoutAwaitingGrant, ActionWaitCheckout},
		{"cloning, young", young, "", CheckoutCloning, ActionWaitCheckout},
		{"cloning, past 15m", middle, clone, CheckoutCloning, ActionWaitCheckout},
		// Past 24 h: a visible error.
		{"awaiting grant, past 24h", old, grant, CheckoutAwaitingGrant, ActionErrorCheckoutWait},
		{"cloning, past 24h", old, clone, CheckoutCloning, ActionErrorCheckoutWait},
		// An unreadable checkout on a row that recorded a wait stays held …
		{"unread after a wait, past 15m", middle, grant, CheckoutUnknown, ActionWaitCheckout},
		{"unread after a wait, past 24h", old, grant, CheckoutUnknown, ActionErrorCheckoutWait},
		// … and on a row that never waited is decided as before.
		{"unread, never waited, young", young, "", CheckoutUnknown, ActionRetryKickoff},
		{"unread, never waited, past 15m", middle, "", CheckoutUnknown, ActionError},
		// Ready: the kickoff goes — inside 24 h after a wait, even past 15 min.
		{"ready, young", young, "", CheckoutReady, ActionRetryKickoff},
		{"ready after a grant wait, past 15m", middle, grant, CheckoutReady, ActionRetryKickoff},
		{"ready after a clone wait, past 15m", middle, clone, CheckoutReady, ActionRetryKickoff},
		{"ready after a wait, past 24h", old, grant, CheckoutReady, ActionError},
		{"ready, never waited, past 15m", middle, "", CheckoutReady, ActionError},
		// Failed (approved but git not caught up, denied …): sent as before, so
		// ccd's not_ready re-send and its recorded reason apply.
		{"failed, young", young, "", CheckoutFailed, ActionRetryKickoff},
		{"failed after a wait, past 15m", middle, grant, CheckoutFailed, ActionRetryKickoff},
		{"failed, never waited, past 15m", middle, "", CheckoutFailed, ActionError},
		// A recorded SEND failure is not a wait marker.
		{"ready, other pre-send error, past 15m", middle, "kickoff not sent: could not open a chat session: x", CheckoutReady, ActionError},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := DecideFirstTurn(repoAgent(now, c.dwell, c.marker), readyInst(), c.checkout, now); got != c.want {
				t.Fatalf("DecideFirstTurn = %s, want %s", got, c.want)
			}
		})
	}
}

// TestDecideFirstTurnLeavesEveryOtherRowToDecideReconcile: an agent without a
// repository, a gateway agent (even one carrying a repo), a kicked-off row, an
// errored row and a row with no ready instance are decided exactly as
// DecideReconcile decides them, whatever checkout state is passed.
func TestDecideFirstTurnLeavesEveryOtherRowToDecideReconcile(t *testing.T) {
	now := time.Date(2026, 3, 9, 14, 0, 0, 0, time.UTC)
	noRepo := repoAgent(now, 7*time.Hour, "")
	noRepo.Repo = ""
	gateway := repoAgent(now, 7*time.Hour, "")
	gateway.Kind, gateway.CCAccount = KindGateway, ""
	kicked := repoAgent(now, 7*time.Hour, "")
	kicked.KickedOff, kicked.KickoffPod = true, "amber-heron-6d9-q2"
	errored := repoAgent(now, 7*time.Hour, "")
	errored.Status = StatusError
	notReady := readyInst()
	notReady.Ready = false
	provisioning := repoAgent(now, 7*time.Hour, "")
	provisioning.Status = StatusProvisioning
	for _, c := range []struct {
		name string
		a    Agent
		inst *provision.Instance
	}{
		{"no repo", noRepo, readyInst()},
		{"gateway kind", gateway, readyInst()},
		{"kicked off", kicked, readyInst()},
		{"errored", errored, readyInst()},
		{"instance not ready", provisioning, notReady},
		{"no instance", provisioning, nil},
	} {
		for _, st := range []string{CheckoutUnknown, CheckoutCloning, CheckoutAwaitingGrant, CheckoutReady} {
			want, _ := DecideReconcile(c.a, c.inst, now)
			if got := DecideFirstTurn(c.a, c.inst, st, now); got != want {
				t.Errorf("%s, checkout %q: DecideFirstTurn = %s, DecideReconcile = %s", c.name, st, got, want)
			}
		}
	}
	// The control: the same `no repo` row WITH a repo is held, so the loop above
	// is not passing because DecideFirstTurn never holds anything.
	if got := DecideFirstTurn(repoAgent(now, 7*time.Hour, ""), readyInst(), CheckoutAwaitingGrant, now); got != ActionWaitCheckout {
		t.Fatalf("control: a repo agent awaiting its grant = %s, want wait-checkout", got)
	}
}

// TestKickoffAwaitingGrantTruthTable: the card's "waiting for your github
// approval" line is an owed, un-errored row whose marker is the GRANT one.
func TestKickoffAwaitingGrantTruthTable(t *testing.T) {
	now := time.Now()
	base := repoAgent(now, time.Hour, KickoffAwaitingGrantReason+": detail")
	if !KickoffAwaitingGrant(base) {
		t.Fatal("an owed row awaiting its grant does not report it")
	}
	clone := base
	clone.KickoffError = KickoffAwaitingCloneReason
	kicked := base
	kicked.KickedOff = true
	errored := base
	errored.Status = StatusError
	noNote := base
	noNote.PendingNote = ""
	failed := base
	failed.KickoffError = KickoffNotAcceptedReason
	for name, a := range map[string]Agent{"cloning": clone, "kicked off": kicked, "errored": errored,
		"no note": noNote, "other error": failed} {
		if KickoffAwaitingGrant(a) {
			t.Errorf("%s: reports awaiting the grant", name)
		}
	}
	// And a held row is never read as a FAILED kickoff.
	if KickoffFailed(base) {
		t.Error("a held (never kicked off) row reads as a failed kickoff")
	}
}
