package agentkickoff

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// repoRow is a claude-code agent dispatched with a repository, owed its first
// turn. Distinct from ownedRow in every field an assertion reads.
func repoRow(dwell time.Duration) agents.Agent {
	return agents.Agent{
		ID: 7417, Name: "silver-avocet", Namespace: testPrefix + "silver-avocet",
		Kind: agents.KindClaudeCode, CCAccount: "acct-k3", Repo: "example-org/kelp-index",
		Status: agents.StatusRunning, PendingNote: "triage the failing kelp-index jobs",
		HooksToken: "tok-91be0d", UpdatedAt: fixedNow.Add(-dwell),
	}
}

func repoHarness(t *testing.T, dwell time.Duration) (*harness, *[]string) {
	t.Helper()
	h := newHarness(t, []provision.Instance{readyInstance("silver-avocet", "silver-avocet-55c-m8", 0)}, repoRow(dwell))
	var changed []string
	h.d.notify = func(name string) { changed = append(changed, name) }
	return h, &changed
}

var errBoom = errors.New("read checkout: dial tcp 192.0.2.47:8080: connect: connection refused")

var awaitingGrant = agents.CheckoutStatus{State: agents.CheckoutAwaitingGrant,
	Detail: "the repository example-org/kelp-index could not be cloned: denied. approve the request"}

// TestAFirstTurnWaitsUnstampedForTheGrantThenGoesOnce walks one held kickoff
// through its whole life against the deliverer: held while ccd says the grant is
// pending (no stamp, no turn, the card's marker written once), still held well
// past the 15-minute bound that would have failed it, then — once ccd reads
// ready — delivered exactly once, with the marker taken off by the stamp.
func TestAFirstTurnWaitsUnstampedForTheGrantThenGoesOnce(t *testing.T) {
	h, changed := repoHarness(t, 3*time.Minute)
	h.gw.setCheckout(awaitingGrant, nil)

	log := h.tick(t)
	r := h.store.row(7417)
	if r.KickedOff || countPrefix(log, "SetKickedOff") != 0 || h.gw.callCount() != 0 {
		t.Fatalf("a turn awaiting its grant was stamped or sent:%s", transcript(log))
	}
	if !agents.KickoffAwaitingGrant(r) || !strings.Contains(r.KickoffError, "could not be cloned") {
		t.Fatalf("kickoff_error = %q: the wait (and ccd's detail) is not recorded", r.KickoffError)
	}
	if indexOf(log, "Checkout(silver-avocet)") < indexOf(log, "ClaimKickoff(7417") {
		t.Fatalf("the checkout was read before the claim was won:%s", transcript(log))
	}
	if countPrefix(log, "ReleaseKickoffClaim(7417") != 1 || len(*changed) != 1 {
		t.Fatalf("claim released %d time(s), OnChange %v; want 1 / one broadcast:%s",
			countPrefix(log, "ReleaseKickoffClaim(7417"), *changed, transcript(log))
	}

	// Seven hours on: still held, NOT stuck, and nothing rewritten or re-broadcast.
	h.d.now = func() time.Time { return fixedNow.Add(7 * time.Hour) }
	log = h.tick(t)
	if countPrefix(log, "MarkKickoffStuck") != 0 || countPrefix(log, "SetKickoffError") != 1 ||
		h.gw.callCount() != 0 || len(*changed) != 1 {
		t.Fatalf("after 7h waiting: stuck=%d kickoff_error writes=%d turns=%d broadcasts=%d; want 0/1/0/1:%s",
			countPrefix(log, "MarkKickoffStuck"), countPrefix(log, "SetKickoffError"), h.gw.callCount(),
			len(*changed), transcript(log))
	}

	// Approved: ccd checks the repository out.
	h.gw.setCheckout(agents.CheckoutStatus{State: agents.CheckoutReady}, nil)
	log = h.tick(t)
	r = h.store.row(7417)
	if !r.KickedOff || h.gw.callCount() != 1 || r.KickoffError != "" {
		t.Fatalf("after the grant: kicked_off=%t turns=%d kickoff_error=%q; want true/1/\"\":%s",
			r.KickedOff, h.gw.callCount(), r.KickoffError, transcript(log))
	}
	if agents.KickoffAwaitingGrant(r) || agents.KickoffFailed(r) {
		t.Fatalf("a delivered row still reads awaiting=%t failed=%t", agents.KickoffAwaitingGrant(r), agents.KickoffFailed(r))
	}

	// Exactly once: further ticks pay nothing.
	h.tick(t)
	h.tick(t)
	if h.gw.callCount() != 1 {
		t.Fatalf("the first turn ran %d times", h.gw.callCount())
	}
}

// TestAFailedStampAfterAWaitKeepsTheTurnHeld (#153): the checkout reads ready
// on a row that waited two hours, and the stamp write fails. The wait marker
// must survive it — cleared before the stamp, or overwritten by the stamp's
// error, the row no longer reads as waited and the next tick, past the
// 15-minute ProvisioningStuckTimeout, errors a turn it could deliver. Instead
// the next tick delivers it, once.
func TestAFailedStampAfterAWaitKeepsTheTurnHeld(t *testing.T) {
	h, _ := repoHarness(t, 2*time.Hour)
	h.store.rows[7417].KickoffError = agents.KickoffAwaitingGrantReason + ": earlier detail"
	h.gw.setCheckout(agents.CheckoutStatus{State: agents.CheckoutReady}, nil)
	h.store.stampErrs = 1
	log := h.tick(t)
	r := h.store.row(7417)
	if r.KickedOff || h.gw.callCount() != 0 || r.Status == agents.StatusError || !agents.WaitedForCheckout(r) {
		t.Fatalf("after a failed stamp: kicked_off=%t turns=%d status=%q waited=%t kickoff_error=%q:%s",
			r.KickedOff, h.gw.callCount(), r.Status, agents.WaitedForCheckout(r), r.KickoffError, transcript(log))
	}
	log = h.tick(t)
	r = h.store.row(7417)
	if !r.KickedOff || h.gw.callCount() != 1 || r.Status == agents.StatusError || r.KickoffError != "" {
		t.Fatalf("the tick after: kicked_off=%t turns=%d status=%q kickoff_error=%q; want true/1/not error/\"\":%s",
			r.KickedOff, h.gw.callCount(), r.Status, r.KickoffError, transcript(log))
	}
}

// TestAStillCloningCheckoutHoldsTheTurnToo: `cloning` (ccd's first attempt has
// not finished) is held exactly as `awaiting_grant` is, under its own marker.
func TestAStillCloningCheckoutHoldsTheTurnToo(t *testing.T) {
	h, _ := repoHarness(t, 20*time.Second)
	h.gw.setCheckout(agents.CheckoutStatus{State: agents.CheckoutCloning}, nil)
	log := h.tick(t)
	r := h.store.row(7417)
	if r.KickedOff || h.gw.callCount() != 0 || !strings.HasPrefix(r.KickoffError, agents.KickoffAwaitingCloneReason) {
		t.Fatalf("cloning: kicked_off=%t turns=%d kickoff_error=%q:%s", r.KickedOff, h.gw.callCount(), r.KickoffError, transcript(log))
	}
	if agents.KickoffAwaitingGrant(r) {
		t.Fatal("a cloning checkout reads as awaiting the github approval")
	}
}

// TestAFirstTurnHeldPastADayFailsVisibly: past agents.CheckoutWaitTimeout the
// row goes `error` saying the github grant was not approved — written AFTER the
// claim is released (MarkKickoffStuck refuses a claimed row), and with nothing
// sent or stamped.
func TestAFirstTurnHeldPastADayFailsVisibly(t *testing.T) {
	h, changed := repoHarness(t, 25*time.Hour)
	h.store.rows[7417].KickoffError = agents.KickoffAwaitingGrantReason + ": earlier detail"
	h.gw.setCheckout(awaitingGrant, nil)
	log := h.tick(t)
	r := h.store.row(7417)
	if r.Status != agents.StatusError || !strings.Contains(r.ErrorMessage, "the github grant was not approved within 24h0m0s") ||
		!strings.Contains(r.ErrorMessage, "example-org/kelp-index") {
		t.Fatalf("status=%q error_message=%q; want error naming the unapproved grant:%s", r.Status, r.ErrorMessage, transcript(log))
	}
	if r.KickedOff || h.gw.callCount() != 0 {
		t.Fatalf("a timed-out wait stamped (%t) or sent (%d)", r.KickedOff, h.gw.callCount())
	}
	if rel, mark := indexOf(log, "ReleaseKickoffClaim(7417"), indexOf(log, "MarkKickoffStuck(7417"); rel < 0 || mark < rel {
		t.Fatalf("the stuck write was not after the claim's release:%s", transcript(log))
	}
	if len(*changed) != 1 {
		t.Fatalf("broadcasts %v, want one", *changed)
	}
}

// TestAnUnreadableCheckoutAfterAWaitKeepsTheTurnHeld: a /healthz read that fails
// on a row that recorded a wait, past 15 minutes, neither errors it nor sends.
func TestAnUnreadableCheckoutAfterAWaitKeepsTheTurnHeld(t *testing.T) {
	h, _ := repoHarness(t, 2*time.Hour)
	h.store.rows[7417].KickoffError = agents.KickoffAwaitingGrantReason
	h.gw.setCheckout(agents.CheckoutStatus{}, errBoom)
	log := h.tick(t)
	r := h.store.row(7417)
	if r.Status == agents.StatusError || r.KickedOff || h.gw.callCount() != 0 || r.KickoffError != agents.KickoffAwaitingGrantReason {
		t.Fatalf("status=%q kicked_off=%t turns=%d kickoff_error=%q; want held unchanged:%s",
			r.Status, r.KickedOff, h.gw.callCount(), r.KickoffError, transcript(log))
	}
}

// TestAnAgentWithoutARepositoryIsDeliveredWithoutACheckoutRead: no repository,
// or a gateway agent — the deliverer never reads a checkout and the first turn
// goes on the first tick, exactly as before.
func TestAnAgentWithoutARepositoryIsDeliveredWithoutACheckoutRead(t *testing.T) {
	gatewayWithRepo := ownedRow()
	gatewayWithRepo.Repo = "example-org/reef-notes"
	ccNoRepo := ownedRow()
	ccNoRepo.Kind, ccNoRepo.CCAccount = agents.KindClaudeCode, "acct-p2"
	for name, row := range map[string]agents.Agent{"gateway, no repo": ownedRow(), "gateway with a repo": gatewayWithRepo,
		"claude-code, no repo": ccNoRepo} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, []provision.Instance{readyInstance("lively-newt", "lively-newt-7f9c-x2", 0)}, row)
			// Were it read, this checkout would hold the turn.
			h.gw.setCheckout(awaitingGrant, nil)
			log := h.tick(t)
			if countPrefix(log, "Checkout(") != 0 {
				t.Fatalf("a checkout was read:%s", transcript(log))
			}
			if !h.store.row(7301).KickedOff || h.gw.callCount() != 1 {
				t.Fatalf("not delivered on the first tick:%s", transcript(log))
			}
		})
	}
}

// TestAHeldRowsBoundStillAppliesWhenItsGatewayNeverResolves: Tick hands a row
// that waits on a checkout to deliver WITHOUT a verdict, so a failure before the
// checkout is read must still end in the table's bound. Never waited and past 15
// minutes: errored (after the claim is released), with the failure quoted. Waited
// (marker recorded) and 3 h in: held, its marker NOT overwritten by the
// transient failure — which would otherwise drop it to the 15-minute bound.
func TestAHeldRowsBoundStillAppliesWhenItsGatewayNeverResolves(t *testing.T) {
	resolveErr := errors.New("get deployments.apps silver-avocet: the server is currently unable to handle the request")

	h, _ := repoHarness(t, 20*time.Minute)
	h.gw.resolveErr = resolveErr
	log := h.tick(t)
	r := h.store.row(7417)
	if r.Status != agents.StatusError || !strings.Contains(r.ErrorMessage, "unable to handle the request") {
		t.Fatalf("never waited, past 15m, unresolvable: status=%q error_message=%q; want the stuck verdict:%s",
			r.Status, r.ErrorMessage, transcript(log))
	}
	if rel, mark := indexOf(log, "ReleaseKickoffClaim(7417"), indexOf(log, "MarkKickoffStuck(7417"); rel < 0 || mark < rel {
		t.Fatalf("the stuck write was not after the claim's release:%s", transcript(log))
	}

	h, _ = repoHarness(t, 3*time.Hour)
	h.store.rows[7417].KickoffError = agents.KickoffAwaitingGrantReason + ": recorded-wait-x4"
	h.gw.resolveErr = resolveErr
	log = h.tick(t)
	r = h.store.row(7417)
	if r.Status == agents.StatusError || r.KickoffError != agents.KickoffAwaitingGrantReason+": recorded-wait-x4" {
		t.Fatalf("a held row, 3h in, unresolvable once: status=%q kickoff_error=%q; want held, marker kept:%s",
			r.Status, r.KickoffError, transcript(log))
	}
}
