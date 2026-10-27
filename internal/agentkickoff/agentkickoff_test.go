package agentkickoff

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// Every fixture value below is pairwise distinct — ids, names, tokens, notes, pod
// names, restart counts, replies — so an assertion that finds one cannot be
// satisfied by another, and none equals a constant the code names.
const (
	testPrefix = "muster-agent-"
	ownerID    = "test-host/abc123"
)

var fixedNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func ownedRow() agents.Agent {
	return agents.Agent{
		ID:          7301,
		Name:        "lively-newt",
		Namespace:   testPrefix + "lively-newt",
		Status:      agents.StatusProvisioning,
		PendingNote: "read the repo and report the test count",
		HooksToken:  "tok-5c1e9a",
		UpdatedAt:   fixedNow.Add(-3 * time.Minute),
	}
}

func readyInstance(name, pod string, restarts int32) provision.Instance {
	return provision.Instance{
		Ref: provision.Ref{Name: name}, InstanceID: pod, Phase: provision.PhaseRunning,
		Ready: true, Replicas: 1, Restarts: restarts,
	}
}

type harness struct {
	log   *recorder
	store *fakeStore
	gw    *fakeGateway
	insts *fakeInstances
	d     *Deliverer
}

func newHarness(t *testing.T, insts []provision.Instance, rows ...agents.Agent) *harness {
	t.Helper()
	h := &harness{log: &recorder{}}
	h.store = newFakeStore(h.log, rows...)
	h.gw = &fakeGateway{log: h.log, reply: "READY — 412 tests"}
	h.insts = &fakeInstances{insts: insts}
	d, err := New(Config{
		Store: h.store, Instances: h.insts, Gateway: h.gw,
		NamespacePrefix: testPrefix, Owner: ownerID, Now: func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.d = d
	return h
}

func (h *harness) tick(t *testing.T) []string {
	t.Helper()
	if err := h.d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	h.d.Wait()
	return h.log.snapshot()
}

// TestAReadyOwedAgentIsToldItsTaskAndTheStampPrecedesTheTurn is the goal, at the
// unit tier: a dispatched agent whose instance is ready gets its pending note as a
// real turn through the gateway, the reply lands in its transcript, and
// kickoff_error is never written.
//
// 🔴 THE ORDER ASSERTION IS THE GUARD AGAINST A REPEATED PAID TURN. SetKickedOff
// must precede Chat in the shared transcript; a deliverer that stamped only after
// a successful reply would re-run the turn on every tick after a failure.
func TestAReadyOwedAgentIsToldItsTaskAndTheStampPrecedesTheTurn(t *testing.T) {
	row := ownedRow()
	h := newHarness(t, []provision.Instance{readyInstance("lively-newt", "lively-newt-7f9c-x2", 2)}, row)
	log := h.tick(t)

	wantChat := "Chat(lively-newt,sk-lively-newt,read the repo and report the test count)"
	if h.gw.calls != 1 || indexOf(log, wantChat) < 0 {
		t.Fatalf("the gateway was called %d time(s), want exactly one turn %q.%s",
			h.gw.calls, wantChat, transcript(log))
	}
	stamp, rec, chat := indexOf(log, "SetKickedOff(7301,true)"),
		indexOf(log, "RecordKickoffDelivery(7301,lively-newt-7f9c-x2,2)"), indexOf(log, "Chat(")
	if stamp < 0 || rec < 0 {
		t.Fatalf("delivery was not stamped (SetKickedOff at %d, RecordKickoffDelivery with the "+
			"recipient pod and restart count at %d).%s", stamp, rec, transcript(log))
	}
	if !(stamp < chat && rec < chat) {
		t.Errorf("KickedOff/delivery provenance were stamped AFTER the model turn (stamp %d, "+
			"record %d, chat %d): a turn that then fails would be re-run — and re-paid — on the "+
			"next tick.%s", stamp, rec, chat, transcript(log))
	}
	if indexOf(log, "ClaimKickoff(7301,"+ownerID+")") > stamp || indexOf(log, "ClaimKickoff(") < 0 {
		t.Errorf("the per-agent claim was not taken before the stamp.%s", transcript(log))
	}
	if indexOf(log, "AddChatMessage(7301,73010,user,read the repo") < 0 ||
		indexOf(log, "AddChatMessage(7301,73010,assistant,READY — 412 tests)") < 0 {
		t.Errorf("the turn is not in the agent's transcript (user note + assistant reply in the "+
			"session the machine chat route would use).%s", transcript(log))
	}
	if countPrefix(log, "SetKickoffError(") != 0 {
		t.Errorf("a successful delivery wrote kickoff_error.%s", transcript(log))
	}
	if indexOf(log, "ReleaseKickoffClaim(7301,"+ownerID+")") < 0 {
		t.Errorf("the claim was not released.%s", transcript(log))
	}
	if r := h.store.row(7301); !r.KickedOff || r.KickoffError != "" || r.KickoffAttempts != 1 {
		t.Errorf("row after delivery: kicked_off=%t kickoff_error=%q attempts=%d, want true/\"\"/1",
			r.KickedOff, r.KickoffError, r.KickoffAttempts)
	}
}

// TestAFailedTurnIsRecordedAndNeverRePaid: a turn that fails AFTER it was handed
// to the gateway is evidence in kickoff_error, and the next tick does NOT run it
// again.
func TestAFailedTurnIsRecordedAndNeverRePaid(t *testing.T) {
	h := newHarness(t, []provision.Instance{readyInstance("lively-newt", "lively-newt-7f9c-x2", 2)}, ownedRow())
	h.gw.err = errors.New("unexpected EOF from runtime")
	h.tick(t)
	log := h.tick(t)

	if h.gw.calls != 1 {
		t.Errorf("a failed first turn was run %d times across two ticks, want 1 — every repeat is "+
			"a paid model turn.%s", h.gw.calls, transcript(log))
	}
	if indexOf(log, "SetKickoffError(7301,kickoff turn failed after it was handed to the gateway") < 0 ||
		!strings.Contains(h.store.row(7301).KickoffError, "unexpected EOF from runtime") {
		t.Errorf("the turn failure is not in kickoff_error with its cause.%s", transcript(log))
	}
	// And the row is in the state the "kickoff failed" signal reads — the stamp
	// cleared "kickoff owed", so this is the only thing left saying it failed.
	if r := h.store.row(7301); agents.KickoffOwed(r) || !agents.KickoffFailed(r) {
		t.Errorf("after a failed post-stamp turn: KickoffOwed=%t KickoffFailed=%t, want false/true",
			agents.KickoffOwed(r), agents.KickoffFailed(r))
	}
}

// TestAShutdownCancelledTurnIsRecordedAsSuch: a turn in flight when the deliverer's
// ctx is cancelled (SIGTERM — a redeploy) is RECORDED, and recorded as a shutdown,
// not as a bare "context canceled" and not lost.
//
// 🔴 THE FAKE STORE REFUSES A WRITE ON A DONE ctx, AS pgx DOES, AND THAT IS WHAT
// MAKES THIS A TEST OF THE DETACHED WRITE. A deliverer that recorded the failure on
// the turn's own ctx would have its SetKickoffError refused here, exactly as the
// driver would refuse it in production.
//
// Fixture: the row's prior kickoff_error is a pre-send failure from an earlier tick
// — pairwise distinct from every constant the assertions name — so a record that
// silently did not happen leaves a value that cannot pass for the right one.
func TestAShutdownCancelledTurnIsRecordedAsSuch(t *testing.T) {
	row := ownedRow()
	h := newHarness(t, []provision.Instance{readyInstance("lively-newt", "lively-newt-7f9c-x2", 2)}, row)
	// The provenance write fails so it does NOT clear the prior error: a lost
	// record then leaves "pool busy kw19" rather than "", and neither passes.
	h.store.provenanceErr = errors.New("provenance write lost qv83")
	h.store.rows[row.ID].KickoffError = "kickoff not sent: could not open a chat session: pool busy kw19"
	h.gw.hold = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.d.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	select {
	case <-h.gw.hold:
	case <-time.After(5 * time.Second):
		t.Fatalf("instrument check FAILED: the turn never reached the gateway.%s", transcript(h.log.snapshot()))
	}
	cancel() // the process is shutting down, mid-turn
	h.d.Wait()
	log := h.log.snapshot()

	r := h.store.row(7301)
	if !strings.HasPrefix(r.KickoffError, ShutdownCancelledReason) {
		t.Fatalf("a turn cancelled by shutdown left kickoff_error = %q, want it to open with "+
			"ShutdownCancelledReason. Either the record was lost (written on the cancelled ctx "+
			"and refused) or it reads as a fault in the agent rather than as muster going "+
			"away mid-turn.%s", r.KickoffError, transcript(log))
	}
	if !strings.Contains(r.KickoffError, context.Canceled.Error()) {
		t.Errorf("the shutdown record drops the underlying cause: %q", r.KickoffError)
	}
	if !agents.KickoffFailed(r) || h.gw.callCount() != 1 {
		t.Errorf("after a shutdown-cancelled turn: KickoffFailed=%t, turns=%d; want true / 1",
			agents.KickoffFailed(r), h.gw.callCount())
	}
}

// TestATurnThatOutlivesItsBudgetIsRecordedAsATimeout separates the second cause
// turnFailure names from the first: the turn's OWN budget expiring is not a
// shutdown, and must not be recorded as one.
func TestATurnThatOutlivesItsBudgetIsRecordedAsATimeout(t *testing.T) {
	h := newHarness(t, []provision.Instance{readyInstance("lively-newt", "lively-newt-7f9c-x2", 2)}, ownedRow())
	d, err := New(Config{
		Store: h.store, Instances: h.insts, Gateway: h.gw, NamespacePrefix: testPrefix, Owner: ownerID,
		Now: func() time.Time { return fixedNow }, TurnTimeout: 70 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.gw.hold = make(chan struct{})
	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	d.Wait()
	got := h.store.row(7301).KickoffError
	if !strings.HasPrefix(got, "kickoff turn exceeded its 70ms budget") {
		t.Errorf("a turn that outlived its own 70ms budget recorded %q", got)
	}
	if strings.HasPrefix(got, ShutdownCancelledReason) {
		t.Errorf("a turn TIMEOUT was recorded as a shutdown: %q", got)
	}
}

// TestASucceededTurnDoesNotKeepAStaleErrorWhenProvenanceFailed: RecordKickoffDelivery
// is the write that clears kickoff_error. When it fails, a pre-send failure from an
// earlier tick stays on a row that is now kicked_off — agents.KickoffFailed's exact
// shape — and the card would badge a SUCCESSFUL turn as "kickoff failed".
func TestASucceededTurnDoesNotKeepAStaleErrorWhenProvenanceFailed(t *testing.T) {
	row := ownedRow()
	row.KickoffError = "kickoff not sent: could not open a chat session: pool busy kw19"
	h := newHarness(t, []provision.Instance{readyInstance("lively-newt", "lively-newt-7f9c-x2", 2)}, row)
	h.store.provenanceErr = errors.New("provenance write lost qv83")
	log := h.tick(t)

	if h.gw.callCount() != 1 || indexOf(log, "AddChatMessage(7301,73010,assistant,READY — 412 tests)") < 0 {
		t.Fatalf("instrument check FAILED: the turn did not succeed.%s", transcript(log))
	}
	if r := h.store.row(7301); agents.KickoffFailed(r) {
		t.Errorf("a turn that SUCCEEDED reads as a failed kickoff: kickoff_error=%q survived because "+
			"the provenance write that normally clears it failed.%s", r.KickoffError, transcript(log))
	}
}

// TestAnEmptyReplyIsAFailedDelivery: the reasoning-model shape (HTTP 200, no text)
// must land in kickoff_error and write no assistant message.
func TestAnEmptyReplyIsAFailedDelivery(t *testing.T) {
	h := newHarness(t, []provision.Instance{readyInstance("lively-newt", "lively-newt-7f9c-x2", 2)}, ownedRow())
	h.gw.reply = " \n\t "
	log := h.tick(t)

	if h.gw.calls != 1 {
		t.Fatalf("instrument check FAILED: no turn ran (%d calls).%s", h.gw.calls, transcript(log))
	}
	if got := h.store.row(7301).KickoffError; got != EmptyReplyReason {
		t.Errorf("kickoff_error after an empty reply = %q, want EmptyReplyReason — an empty turn "+
			"recorded as delivered reports an agent that was told its task when nothing says it "+
			"was.%s", got, transcript(log))
	}
	if countPrefix(log, "AddChatMessage(7301,73010,assistant,") != 0 {
		t.Errorf("an empty reply was written to the transcript as an answer.%s", transcript(log))
	}
}

// TestPreSendFailuresAreRetriedAndCostNoTurn: a row with no token yet is refused
// BEFORE the claim and the stamp, so it is retried next tick and no turn runs.
func TestPreSendFailuresAreRetriedAndCostNoTurn(t *testing.T) {
	row := ownedRow()
	row.HooksToken = ""
	h := newHarness(t, []provision.Instance{readyInstance("lively-newt", "lively-newt-7f9c-x2", 2)}, row)
	h.tick(t)
	log := h.tick(t)

	if n := countPrefix(log, "SetKickoffError(7301,kickoff not sent: the agent has no hooks token"); n != 2 {
		t.Errorf("a token-less row was recorded %d time(s) over two ticks, want 2 (one per retry).%s",
			n, transcript(log))
	}
	if h.gw.calls != 0 || countPrefix(log, "SetKickedOff(") != 0 {
		t.Errorf("a row with no credential got a turn or a stamp.%s", transcript(log))
	}
}

// TestALostClaimDoesNothing: another replica holds the claim, so this one neither
// runs the turn nor releases a claim it does not own.
func TestALostClaimDoesNothing(t *testing.T) {
	h := newHarness(t, []provision.Instance{readyInstance("lively-newt", "lively-newt-7f9c-x2", 2)}, ownedRow())
	h.store.claimLoses = true
	log := h.tick(t)

	if indexOf(log, "ClaimKickoff(7301,") < 0 {
		t.Fatalf("instrument check FAILED: no claim was attempted.%s", transcript(log))
	}
	for _, p := range []string{"SetKickedOff(", "Chat(", "ReleaseKickoffClaim(", "SetKickoffError(", "Get("} {
		if countPrefix(log, p) != 0 {
			t.Errorf("a LOST claim was followed by %s.%s", p, transcript(log))
		}
	}
}

// TestARowDeliveredByAnotherReplicaIsNotPaidAgain: the list said owed, the re-read
// under the claim says kicked off — the turn must not run.
func TestARowDeliveredByAnotherReplicaIsNotPaidAgain(t *testing.T) {
	row := ownedRow()
	h := newHarness(t, []provision.Instance{readyInstance("lively-newt", "lively-newt-7f9c-x2", 2)}, row)
	delivered := row
	delivered.KickedOff = true
	h.store.getOverride[row.ID] = delivered
	log := h.tick(t)

	if indexOf(log, "Get(7301)") < 0 {
		t.Fatalf("instrument check FAILED: the row was not re-read under the claim.%s", transcript(log))
	}
	if h.gw.calls != 0 || countPrefix(log, "SetKickedOff(") != 0 {
		t.Errorf("a row another replica already kicked off was delivered AGAIN.%s", transcript(log))
	}
}

// TestABlindBackendTakesNoAction: an instance-list failure is not an empty
// backend. Without this, an old owed row would be errored on no information.
func TestABlindBackendTakesNoAction(t *testing.T) {
	row := ownedRow()
	row.UpdatedAt = fixedNow.Add(-2 * time.Hour)
	h := newHarness(t, nil, row)
	h.insts.err = provision.ErrBlind
	err := h.d.Tick(context.Background())
	h.d.Wait()
	if err == nil {
		t.Errorf("a blind instance list was not reported")
	}
	if log := h.log.snapshot(); len(log) != 0 {
		t.Errorf("the deliverer acted on a tick whose backend it could not see.%s", transcript(log))
	}
}

// TestAStuckOwedRowGoesRedWithItsLastSendError: past the dwell bound with no
// ready instance, the table's ActionError is TAKEN, carrying the evidence.
func TestAStuckOwedRowGoesRedWithItsLastSendError(t *testing.T) {
	row := ownedRow()
	row.UpdatedAt = fixedNow.Add(-agents.ProvisioningStuckTimeout - time.Second)
	row.KickoffError = "dial tcp 192.0.2.17:18789: connect: connection refused"
	h := newHarness(t, nil, row)
	log := h.tick(t)

	r := h.store.row(7301)
	if r.Status != agents.StatusError {
		t.Fatalf("a row stuck past ProvisioningStuckTimeout was not errored (status %s).%s",
			r.Status, transcript(log))
	}
	if !strings.Contains(r.ErrorMessage, "kickoff never delivered") ||
		!strings.Contains(r.ErrorMessage, "last send error: dial tcp 192.0.2.17:18789") {
		t.Errorf("the red card does not carry the cause and the last send error: %q", r.ErrorMessage)
	}
}

// TestTheDelivererLeavesRowsItDoesNotOwnAndKickedOffRowsAlone pins scope, using
// the two shapes the live agents table holds (rows written by ANOTHER system,
// under that system's namespaces) plus the two shapes in which only the ownership
// check stands between a foreign row and an action.
//
//   - brave-heron: foreign namespace, status error, a pending note.
//   - agent:       foreign namespace, status running, kicked_off=t, a pending note,
//     a READY instance of the same name.
//   - amber-otter: foreign namespace, provisioning, owed, dwelt past the timeout,
//     no instance → the table says ActionError. Only ownership prevents it.
//   - silver-wren: foreign namespace, provisioning, owed, young, a READY instance
//     → the table says ActionRetryKickoff. Only ownership prevents a turn.
//
// POSITIVE CONTROL in the same tick: an OWNED row is delivered, so "nothing
// happened to the foreign rows" is not "nothing happened".
func TestTheDelivererLeavesRowsItDoesNotOwnAndKickedOffRowsAlone(t *testing.T) {
	braveHeron := agents.Agent{ID: 11, Name: "brave-heron", Namespace: "devpod-brave-heron",
		Status: agents.StatusError, PendingNote: "note for heron", HooksToken: "tok-heron",
		UpdatedAt: fixedNow.Add(-72 * time.Hour)}
	agent := agents.Agent{ID: 12, Name: "agent", Namespace: "devpod-agent",
		Status: agents.StatusRunning, KickedOff: true, PendingNote: "note for agent",
		HooksToken: "tok-agent", KickoffPod: "agent-old-pod", UpdatedAt: fixedNow.Add(-48 * time.Hour)}
	amberOtter := agents.Agent{ID: 13, Name: "amber-otter", Namespace: "devpod-amber-otter",
		Status: agents.StatusProvisioning, PendingNote: "note for otter", HooksToken: "tok-otter",
		UpdatedAt: fixedNow.Add(-5 * time.Hour)}
	silverWren := agents.Agent{ID: 14, Name: "silver-wren", Namespace: "devpod-silver-wren",
		Status: agents.StatusProvisioning, PendingNote: "note for wren", HooksToken: "tok-wren",
		UpdatedAt: fixedNow.Add(-1 * time.Minute)}

	h := newHarness(t, []provision.Instance{
		readyInstance("agent", "agent-new-pod", 4),
		readyInstance("silver-wren", "silver-wren-pod", 1),
		readyInstance("lively-newt", "lively-newt-7f9c-x2", 2),
	}, braveHeron, agent, amberOtter, silverWren, ownedRow())
	log := h.tick(t)

	if indexOf(log, "Chat(lively-newt,") < 0 {
		t.Fatalf("positive control FAILED: the owned row in the same tick was not delivered, so "+
			"the absence of writes below proves nothing.%s", transcript(log))
	}
	for _, id := range []string{"11", "12", "13", "14"} {
		for _, l := range log {
			if strings.Contains(l, "("+id+",") || strings.Contains(l, "("+id+")") {
				t.Errorf("the deliverer touched foreign/kicked-off row %s: %s%s", id, l, transcript(log))
			}
		}
	}
	for _, name := range []string{"brave-heron", "agent", "amber-otter", "silver-wren"} {
		if indexOf(log, "Chat("+name+",") >= 0 {
			t.Errorf("a turn was sent to %s, a row this deployment does not own.%s", name, transcript(log))
		}
	}
	if r := h.store.row(13); r.Status != agents.StatusProvisioning {
		t.Errorf("a foreign stuck row was escalated to %s.", r.Status)
	}
}

// TestNewRefusesADelivererThatCouldDeliverNothing pins the required fields.
func TestNewRefusesADelivererThatCouldDeliverNothing(t *testing.T) {
	log := &recorder{}
	s, g, i := newFakeStore(log), &fakeGateway{log: log}, &fakeInstances{}
	for _, c := range []struct {
		name string
		cfg  Config
	}{
		{"no store", Config{Instances: i, Gateway: g}},
		{"no instances", Config{Store: s, Gateway: g}},
		{"no gateway", Config{Store: s, Instances: i}},
	} {
		if _, err := New(c.cfg); err == nil {
			t.Errorf("%s: New accepted it", c.name)
		}
	}
	if _, err := New(Config{Store: s, Instances: i, Gateway: g}); err != nil {
		t.Errorf("control: a complete config was refused: %v", err)
	}
}
