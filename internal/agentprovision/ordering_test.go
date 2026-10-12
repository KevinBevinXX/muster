package agentprovision

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// TestDispatchWithoutAKickoffCreatesNothing is the guard for the defect this
// package's Dispatch doc is about, and it is the one the suite most conspicuously
// lacked: the behaviour changed from "creates a 1-replica instance" to "creates
// nothing" and the ENTIRE existing suite stayed green, in both directions.
//
// 🔴 WHAT IT PROTECTS IS A GUARD IN ANOTHER PACKAGE. internal/api's dispatch
// handler refuses a `gate:<reason>`-tagged task only when `action == "dispatch"`,
// and its comment gives the reason: "save for later" is still allowed because it
// "provisions nothing". `kickoff` IS `action == "dispatch"`, so an adapter that
// creates on kickoff=false makes that sentence false and walks straight through the
// gate — one pod per click, with the repository cloned into it and a model
// credential in a Secret, for a task explicitly marked not-ready-to-work.
//
// ⚠ A STATUS-ONLY ASSERTION WOULD NOT HAVE CAUGHT IT. `stopped` was written on
// both the old and the new behaviour; only the driver's own state distinguishes
// them, which is why this asserts the CREATE COUNT and the backend's instance list
// rather than the row.
func TestDispatchWithoutAKickoffCreatesNothing(t *testing.T) {
	a, store, driver := newAdapter(t, nil)
	if err := a.Dispatch(context.Background(), fixtureAgentID, false); err != nil {
		t.Fatalf("Dispatch(kickoff=false): %v", err)
	}

	if driver.creates != 0 {
		t.Errorf("Dispatch(kickoff=false) called the driver's Create %d time(s), want 0.\n"+
			"    The caller's other action is the UI's \"Save for later\", and internal/api's "+
			"gate check allows a save on a `gate:` task on the stated grounds that it "+
			"provisions nothing. Creating here bypasses that gate.\n  transcript: %s",
			driver.creates, store.transcript())
	}
	insts, err := driver.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(insts) != 0 {
		t.Errorf("the backend holds %d instance(s) after a save, want 0", len(insts))
	}
	// No instance means no credential should have been minted for one either.
	if store.called("SetHooksToken") {
		t.Errorf("Dispatch(kickoff=false) minted a hooks token, which is a secret written "+
			"for an instance that does not exist.\n  transcript: %s", store.transcript())
	}
	// And the stored status must be the one that is now TRUE.
	if !strings.Contains(store.transcript(), "UpdateStatus(4291,stopped,") {
		t.Errorf("Dispatch(kickoff=false) did not persist %s.\n  transcript: %s",
			agents.StatusStopped, store.transcript())
	}

	// POSITIVE CONTROL, and it is the whole reason this test can be trusted: the
	// SAME fixture with kickoff=true MUST create. Without it, "Create was not
	// called" would pass over an adapter whose Create never works at all.
	a2, _, driver2 := newAdapter(t, nil)
	if err := a2.Dispatch(context.Background(), fixtureAgentID, true); err != nil {
		t.Fatalf("Dispatch(kickoff=true): %v", err)
	}
	if driver2.creates != 1 {
		t.Fatalf("positive control FAILED: Dispatch(kickoff=true) called Create %d time(s), "+
			"want 1. The assertion above is then about an adapter that cannot create at all.",
			driver2.creates)
	}
}

// TestATimedOutOperationStillRECORDSItsFailure is the guard for the bookkeeping
// budget, and it was the last survivor of the fix round's own mutation sweep.
//
// 🔴 A RECORD WRITTEN ON THE EXPIRED BUDGET OF THE THING IT IS RECORDING CANNOT
// LAND IN THE CASE THAT NEEDS IT MOST. One context used to cover the driver call AND
// every store write after it: a driver call that consumed the budget left `fail`'s
// UpdateStatus running on a dead context, so the write failed, the branch only
// logged, and the net result was a cluster holding partial objects, a row still
// claiming whatever the handler wrote, and one line in this pod's stdout. That is
// the observable this package's header says it exists to remove.
//
// 🔴 IT ONLY WORKS BECAUSE THE FAKE HONOURS ITS CONTEXT. Every store method took
// `_ context.Context` when the fix landed, so the mutant that reverted it SURVIVED —
// the suite could not see a context defect at all. See recordingStore.refuse.
//
// ⚠ THIS IS ALSO WHAT EARNS Config.OpTimeout ITS PLACE. Round 0 proposed deleting
// the field as an injection point nothing injects; this is the test that needs to
// shorten the budget to observe the expiry, which is the condition its doc names.
func TestATimedOutOperationStillRECORDSItsFailure(t *testing.T) {
	store := &recordingStore{agent: fixtureAgent()}
	driver := &flakyDriver{Noop: provision.MustNewNoop(), rec: store, burnBudget: true}
	a, err := New(Config{
		Driver: driver,
		Store:  store,
		Spec:   fixtureSpecConfig(),
		// This test is about a TIMEOUT inside the driver call, so the dispatch has to
		// REACH the driver. Without this the undeliverable-kickoff refusal returns
		// first and Create never runs — which the positive control below caught
		// rather than letting the test pass for the wrong reason.
		KickoffDeliverable: true,
		OpTimeout:          20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := a.Dispatch(context.Background(), fixtureAgentID, true); err == nil {
		t.Fatal("Dispatch returned nil over a driver call that timed out")
	}

	// POSITIVE CONTROL: the driver really did burn the budget, so this is a test
	// about a timeout and not about some other failure.
	if driver.creates != 1 {
		t.Fatalf("positive control FAILED: Create ran %d time(s), want 1", driver.creates)
	}
	if !strings.Contains(store.transcript(), "driver.Create") {
		t.Fatalf("positive control FAILED: the driver was never reached.\n  transcript: %s",
			store.transcript())
	}

	// The record must have LANDED — not been refused by a dead context.
	if strings.Contains(store.transcript(), "REFUSED") {
		t.Errorf("a store write was refused by an expired context, so the failure was "+
			"recorded nowhere durable:\n  transcript: %s\n"+
			"    The bookkeeping write must derive its OWN budget (bookkeepingCtx), never "+
			"the operation's — the operation's is expired by definition in this case.",
			store.transcript())
	}
	if !strings.Contains(store.transcript(), "UpdateStatus(4291,error,") {
		t.Errorf("the timeout was not recorded as %s on the row, so the only trace is a log "+
			"line.\n  transcript: %s", agents.StatusError, store.transcript())
	}
	// And the recorded message must name the cause, not just the status.
	if !strings.Contains(store.transcript(), "deadline exceeded") {
		t.Errorf("the recorded failure does not name the timeout.\n  transcript: %s",
			store.transcript())
	}
}

// TestARefusedCallIsNotARecordedCall guards the INSTRUMENT, which is the thing that
// decides whether any of the other guards can see anything.
//
// 🔴 A REFUSAL THAT LOOKS LIKE A CALL INVERTS EVERY ASSERTION PHRASED OVER called().
// The refusal record used to begin with the refused call's own name, so
// called("Delete") answered TRUE for a delete that never happened: "the row was
// deleted" would pass over exactly the dead-context defect the bookkeeping budget
// exists to prevent, and "the row was NOT deleted" would fail over a row correctly
// kept. It was latent — no test reached it — which is precisely why it needs a guard
// rather than a comment: the next test to assert over a refused call inherits the
// inversion silently.
//
// ⚠ IT ASSERTS BOTH DIRECTIONS. A refusal must not read as a call, AND it must still
// be observable as a refusal — a fix that simply stopped recording refusals would
// satisfy the first half and blind TestATimedOutOperationStillRECORDSItsFailure.
func TestARefusedCallIsNotARecordedCall(t *testing.T) {
	store := &recordingStore{agent: fixtureAgent()}
	dead, cancel := context.WithCancel(context.Background())
	cancel()

	if err := store.Delete(dead, fixtureAgentID); err == nil {
		t.Fatal("the fake accepted a Delete on a cancelled context, so it cannot see a " +
			"context defect at all")
	}
	if store.called("Delete") {
		t.Errorf("called(\"Delete\") is TRUE after a REFUSED delete.\n"+
			"    Every assertion phrased over called()/indexOf() is then inverted for this "+
			"name: \"the row was deleted\" passes over a delete that did not happen.\n"+
			"  transcript: %s", store.transcript())
	}
	if store.indexOf("Delete") >= 0 {
		t.Errorf("indexOf(\"Delete\") resolved a REFUSED delete at %d, so the ordering "+
			"guard would order an event that never occurred.\n  transcript: %s",
			store.indexOf("Delete"), store.transcript())
	}
	if !store.refused("Delete") {
		t.Errorf("the refusal is not observable as a refusal either, which blinds the "+
			"timeout guards.\n  transcript: %s", store.transcript())
	}

	// POSITIVE CONTROL: a live Delete must read as a call, or the two assertions above
	// pass over a fake that records nothing at all.
	live := &recordingStore{agent: fixtureAgent()}
	if err := live.Delete(context.Background(), fixtureAgentID); err != nil {
		t.Fatalf("Delete on a live context: %v", err)
	}
	if !live.called("Delete") {
		t.Fatalf("positive control FAILED: a live Delete does not read as a call.\n"+
			"  transcript: %s", live.transcript())
	}
	if live.refused("Delete") {
		t.Errorf("a live Delete reads as REFUSED.\n  transcript: %s", live.transcript())
	}
}

// TestATimedOutDestroyStillDELETESTheRow is the second half of the bookkeeping-budget
// guarantee, and it exists because a mutant found the site bookkeepingCtx's own doc had
// predicted would be missed.
//
// 🔴 driver.Destroy IS THE SLOW CALL, SO THIS IS THE ONE OPERATION WHOSE BUDGET IS
// MOST LIKELY ALREADY GONE BY THE TIME THE ROW IS TOUCHED. On the operation context the
// sequence was: teardown succeeds, Delete runs on a dead context and fails, the error is
// only logged — after handleAgentDelete's empty 200 has already had htmx remove the
// card. The instance is gone and the row survives with its pre-delete status, so the
// card comes back pointing at nothing.
//
// ⚠ IT ASSERTS THE ROW WAS DELETED *AND* THAT NOTHING WAS REFUSED, because either alone
// is satisfiable by accident: a Delete that never ran records no refusal either.
func TestATimedOutDestroyStillDELETESTheRow(t *testing.T) {
	store := &recordingStore{agent: fixtureAgent()}
	driver := &flakyDriver{Noop: provision.MustNewNoop(), rec: store, burnBudgetOnDestroy: true}
	a, err := New(Config{
		Driver:    driver,
		Store:     store,
		Spec:      fixtureSpecConfig(),
		OpTimeout: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = a.Destroy(context.Background(), fixtureAgentID)

	// POSITIVE CONTROL: the driver really was reached and really did burn the budget.
	if driver.destroys != 1 {
		t.Fatalf("positive control FAILED: Destroy ran %d time(s), want 1", driver.destroys)
	}

	// 🔴 THE TRANSCRIPT ASSERTIONS RUN BEFORE THE RETURNED ERROR IS JUDGED, AND THE
	// ORDER WAS FOUND BY MUTATION FOR THE SECOND TIME IN THIS PACKAGE. With
	// `t.Fatalf("Destroy: %v", err)` first, the mutant that put the delete back on the
	// operation context stopped the test on the returned error and the two assertions
	// that name the actual defect never ran — a kill scored for an assertion that did
	// not execute, and still green with both of them deleted.
	if strings.Contains(store.transcript(), "REFUSED") {
		t.Errorf("a store write was refused by an expired context after the teardown "+
			"succeeded:\n  transcript: %s\n"+
			"    The instance is gone and the row survives with its pre-delete status, while "+
			"htmx has already removed the card. Delete must run on the bookkeeping budget.",
			store.transcript())
	}
	if !store.called("Delete") {
		t.Errorf("the row was not deleted after a teardown that succeeded on a burnt "+
			"budget.\n  transcript: %s", store.transcript())
	}
	if err != nil {
		t.Errorf("Destroy reported a failure over a teardown that SUCCEEDED: %v", err)
	}
}

// TestTheBackendLEADSTheRowInEveryMethod is the ordering guard, and it exists
// because a mutant that moved a status write BEFORE its driver call SURVIVED the
// whole suite.
//
// 🔴 THE ROW MUST NEVER CLAIM SOMETHING THE BACKEND HAS NOT DONE YET. A status
// written first is a window — however short — in which the card states an outcome
// that may never happen, and if the driver call then fails the row is left asserting
// it. Every method here does the cluster work first and records second.
//
// ⚠ THIS IS AN ORDER ASSERTION, WHICH THE OLD FAKE COULD NOT EXPRESS. The store
// and the driver now share one transcript (see flakyDriver.rec); membership checks
// over two separate lists cannot say "A before B" no matter how they are written.
//
// ⚠ "EVERY METHOD" MEANS EVERY METHOD THAT HAS BOTH HALVES TO ORDER — three of them.
// The name overstated it, so here is the account, over all EIGHT methods on the
// adapter (api.Provisioner's seven plus ReapplyProfiles). An earlier revision of this
// paragraph called itself exhaustive over the seven without saying so, and omitted the
// eighth — which is the one with a real store write:
//
//   - Dispatch(kickoff=true), Start, Stop — the three rows below.
//   - Destroy — covered by TestATimedOutDestroyStillDELETESTheRow and
//     TestDestroyRecordsWHYItKeptTheRow instead.
//   - Dispatch(kickoff=false) — makes NO driver call, so this test's own `di < 0`
//     positive control would fire. Its guard is
//     TestDispatchWithoutAKickoffCreatesNothing.
//   - Instances, TailLogs, StreamLogs — touch no store, so there is no order.
//   - ReapplyProfiles — DOES write before its driver call (ensureHooksToken, then
//     driver.Update), but writes no STATUS, so the `si < 0` control would fire. Its
//     guard is TestReapplyProfilesMintsAMissingToken. ⚠ The write-then-Update order
//     there has a recorded consequence rather than an asserted one: see
//     ensureHooksToken's own note — a failed Update leaves the row holding a token the
//     instance does not have, so the next Create returns provision.ErrDivergentSpec.
func TestTheBackendLEADSTheRowInEveryMethod(t *testing.T) {
	cases := []struct {
		name string
		live bool
		run  func(*Adapter) error
		// driverCall is the transcript entry that must come FIRST.
		driverCall string
	}{
		{
			name:       "Dispatch with kickoff",
			run:        func(a *Adapter) error { return a.Dispatch(context.Background(), fixtureAgentID, true) },
			driverCall: "driver.Create",
		},
		{
			name:       "Start",
			live:       true,
			run:        func(a *Adapter) error { return a.Start(context.Background(), fixtureAgentID) },
			driverCall: "driver.Scale",
		},
		{
			name:       "Stop",
			live:       true,
			run:        func(a *Adapter) error { return a.Stop(context.Background(), fixtureAgentID) },
			driverCall: "driver.Scale",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, store, driver := newAdapter(t, nil)
			if tc.live {
				seedInstance(t, a, store, driver)
			}
			if err := tc.run(a); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			di := store.indexOf(tc.driverCall)
			si := store.indexOf("UpdateStatus")
			if di < 0 {
				t.Fatalf("positive control FAILED: %s never reached %s, so the order "+
					"assertion below is over one entry.\n  transcript: %s",
					tc.name, tc.driverCall, store.transcript())
			}
			if si < 0 {
				t.Fatalf("positive control FAILED: %s never wrote a status.\n  transcript: %s",
					tc.name, store.transcript())
			}
			if di > si {
				t.Errorf("%s wrote the row BEFORE the backend acted (%s at %d, UpdateStatus "+
					"at %d).\n"+
					"    The row then claims an outcome the driver has not produced, and if "+
					"the driver call fails it is left asserting it.\n  transcript: %s",
					tc.name, tc.driverCall, di, si, store.transcript())
			}
		})
	}
}

// TestDestroyRecordsWHYItKeptTheRow closes the half of Destroy's strictness that
// was correct and invisible.
//
// 🔴 KEEPING THE ROW IS ONLY HALF AN ANSWER IF NOTHING SAYS WHY. handleAgentDelete
// runs Destroy in a background goroutine that only logs, and it has ALREADY
// answered an empty 200 — so htmx removed the card before Destroy ran. With no row
// write, the card simply REAPPEARS on the next render with no explanation on any
// surface, and the row we correctly kept is the only evidence the instance is still
// out there.
//
// ⚠ WHAT THE STORED `error` ACTUALLY BUYS IS IN Destroy'S OWN DOC, AND THIS COMMENT
// USED TO OVERSTATE IT HERE — the previous revision ended "turns 'the card came back'
// into a red card naming an ownership refusal or an unreachable backend", which is the
// sentence agentprovision.go retracted in the same commit that edited this file. Do
// not restate it; read it there.
func TestDestroyRecordsWHYItKeptTheRow(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"ownership refusal", fmt.Errorf("foreign object: %w", provision.ErrNotManaged)},
		{"backend unreachable", fmt.Errorf("dial tcp: %w", provision.ErrBlind)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, store, driver := newAdapter(t, nil)
			seedInstance(t, a, store, driver)
			driver.destroyErr = tc.err

			if err := a.Destroy(context.Background(), fixtureAgentID); err == nil {
				t.Fatal("Destroy returned nil while keeping the row")
			}
			if store.called("Delete") {
				t.Fatalf("Destroy deleted the row over %v", tc.err)
			}
			if !strings.Contains(store.transcript(), "UpdateStatus(4291,error,") {
				t.Errorf("Destroy kept the row and wrote NOTHING to it, so the card vanishes "+
					"(htmx already removed it) and reappears unexplained.\n  transcript: %s",
					store.transcript())
			}
			// The cause must be IN the recorded message, not just a status.
			if !strings.Contains(store.transcript(), tc.err.Error()) {
				t.Errorf("the recorded failure does not carry the driver's own reason %q, "+
					"so a human sees `error` with no cause.\n  transcript: %s",
					tc.err.Error(), store.transcript())
			}
		})
	}
}

// TestStopTreatsAnAbsentInstanceAsAlreadyStopped pins the sentinel Stop was
// missing.
//
// 🔴 AN INSTANCE EVICTED OUT OF BAND COULD NEVER BE STOPPED FROM THE UI. Scale
// returned provision.ErrNotFound, Stop returned an error and wrote nothing, so the
// stored status stayed `running` for ever — and internal/api's model-change roll
// branches on the STORED status, so it kept trying to reconcile an instance that
// does not exist. Destroy already draws this distinction; Stop did not.
func TestStopTreatsAnAbsentInstanceAsAlreadyStopped(t *testing.T) {
	a, store, driver := newAdapter(t, nil)
	seedInstance(t, a, store, driver)
	driver.scaleErr = fmt.Errorf("deployment gone: %w", provision.ErrNotFound)

	if err := a.Stop(context.Background(), fixtureAgentID); err != nil {
		t.Fatalf("Stop over an absent instance returned %v; absence is what a stop is FOR", err)
	}
	if !strings.Contains(store.transcript(), "UpdateStatus(4291,stopped,") {
		t.Errorf("Stop did not persist %s for an absent instance, so the row stays `running` "+
			"with no way to correct it.\n  transcript: %s", agents.StatusStopped, store.transcript())
	}

	// NEGATIVE CONTROL: a scale error that is NOT absence must still refuse, or this
	// change would have swallowed every stop failure.
	a2, store2, driver2 := newAdapter(t, nil)
	seedInstance(t, a2, store2, driver2)
	driver2.scaleErr = fmt.Errorf("dial tcp: %w", provision.ErrBlind)
	if err := a2.Stop(context.Background(), fixtureAgentID); err == nil {
		t.Error("Stop swallowed a non-absence scale failure")
	}
	if strings.Contains(store2.transcript(), "UpdateStatus(4291,stopped,") {
		t.Errorf("Stop recorded `stopped` over an UNREACHABLE backend.\n  transcript: %s",
			store2.transcript())
	}
}

// TestReapplyProfilesMintsAMissingToken closes the call site a mutant showed was
// untested: dropping ensureHooksToken from ReapplyProfiles SURVIVED.
//
// ⚠ WHY IT MATTERS THERE TOO: agentspec.Build puts the token in Spec.Secrets, so a
// spec built without one is materially different from the instance's own. Update
// would then reconcile the live instance to a spec with no credential.
func TestReapplyProfilesMintsAMissingToken(t *testing.T) {
	a, store, driver := newAdapter(t, nil)
	if err := a.ReapplyProfiles(context.Background(), fixtureAgentID); err != nil {
		t.Fatalf("ReapplyProfiles: %v", err)
	}
	if !store.called("SetHooksToken") {
		t.Errorf("ReapplyProfiles did not mint a token for a tokenless agent, so it would "+
			"reconcile the instance to a spec with no credential in Spec.Secrets.\n"+
			"  transcript: %s", store.transcript())
	}
	if driver.updates != 1 {
		t.Errorf("ReapplyProfiles called Update %d time(s), want 1", driver.updates)
	}
}
