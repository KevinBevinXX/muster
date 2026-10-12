package agentprovision

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// THE REFUSAL, AND THE THREE THINGS IT MUST NOT BREAK.
//
// 🔴 EVERY ASSERTION IN THIS FILE IS BEHAVIOURAL — the transcript of store and
// driver calls, the returned error, the driver's own create counter. None of them
// reads a field of Adapter or compares against KickoffRefusalReason itself, because
// an expectation derived from the implementation under test cannot see the
// implementation being wrong. The strings below are LITERAL for the same reason: a
// test asserting `strings.Contains(err, KickoffRefusalReason)` passes over any
// rewording, including one that dropped the variable name an operator needs.
//
// 🔴 AND THE POSITIVE CONTROL IS A TEST OF ITS OWN, NOT A COMMENT. A refusal that
// fired unconditionally would satisfy the refusal test, the save test and the start
// test — save and start take different branches — so
// TestAKickoffIsNOTRefusedWhenTheAdapterIsToldItCanBeDelivered is the only thing
// that separates "refuses when it cannot deliver" from "refuses always".
// ---------------------------------------------------------------------------

// literals the refusal must carry, spelled out rather than referenced.
const (
	// wantRefusalVariable is the ONE actionable token: without it the operator knows
	// something was refused and not what to set. cmd/muster-server's
	// TestTheKickoffRefusalNamesTheBinarysOwnGatewayVariable pins it against that
	// binary's own constant, so the two spellings cannot drift apart.
	wantRefusalVariable = "MUSTER_AGENT_GATEWAY"
	// wantRefusalRemedyValue is the value to set it to. A cause with no remedy reads
	// as "this is broken" rather than "this is off".
	wantRefusalRemedyValue = "hooks-sha256"
	// wantRefusalSaveEscape is the operation that still works, named in the refusal so
	// the operator is not left with no next move at all.
	wantRefusalSaveEscape = "Save for later"
)

// TestACreateWithAKickoffIsREFUSEDWhenNothingCanDeliverIt is the regression test for
// the behaviour this change exists to fix.
//
// 🔴 WHAT IT WAS, MEASURED ON THE DEPLOYED BUILD: a dispatch asking for a kickoff on
// a deployment with no gateway CREATED the instance — a pod, a namespace, a
// ServiceAccount, a Secret holding a freshly-minted token, the repository cloned in,
// a model credential handed over — and then recorded that the one thing the caller
// asked for had not happened, in a field no page renders. agents.ComputeStatus then
// refined the live ready instance to `running`, so the card read HEALTHY over an
// agent that was never told what to do. Every click cost another such pod.
//
// 🔴 THE ASSERTIONS ARE ORDERED BY WHAT THEY PROTECT, AND THE FIRST TWO ARE THE
// POINT: nothing was created, and no secret was minted. A refusal that still built
// the instance and merely returned an error would pass a test phrased only over the
// error — and would be the same defect with a louder log.
func TestACreateWithAKickoffIsREFUSEDWhenNothingCanDeliverIt(t *testing.T) {
	a, store, driver := newAdapterWithNoKickoffDelivery(t, nil)

	err := a.Dispatch(context.Background(), fixtureAgentID, true)
	if err == nil {
		t.Fatalf("Dispatch(kickoff=true) with no gateway returned nil, so the caller was told "+
			"the work had begun.\n  transcript: %s", store.transcript())
	}

	// INSTRUMENT CHECK: the adapter really ran and really reached the fake. Without
	// this, every "was not called" assertion below would pass over an adapter that
	// returned at its first line for an unrelated reason.
	if !store.called("Get") {
		t.Fatalf("instrument check FAILED: the adapter never loaded the row, so the "+
			"assertions below are over nothing.\n  transcript: %s", store.transcript())
	}

	// 1. NOTHING WAS BUILT.
	if driver.creates != 0 {
		t.Errorf("the driver was asked to create %d instance(s) for a kickoff that cannot be "+
			"delivered, want 0.\n  transcript: %s\n"+
			"    Creating it is the expensive half of a request whose only point — telling the "+
			"agent what to do — is impossible here.", driver.creates, store.transcript())
	}

	// 2. NO SECRET WAS MINTED. A token written for an instance that does not exist is
	//    a credential with no holder and no expiry.
	if store.called("SetHooksToken") {
		t.Errorf("a hooks token was minted for an agent that was never provisioned.\n"+
			"  transcript: %s", store.transcript())
	}

	// 3. THE ROW SAYS IT FAILED, TERMINALLY. `error` is the one status
	//    agents.ComputeStatus returns early on, so the card cannot be refined back to
	//    `running` by a live instance — which is exactly what used to happen.
	if !strings.Contains(store.transcript(), "UpdateStatus(4291,error,") {
		t.Errorf("the refusal was not recorded as %s on the row, so the operator's only "+
			"evidence is a log line.\n  transcript: %s", agents.StatusError, store.transcript())
	}
	if store.called("UpdateStatus(4291,provisioning") {
		t.Errorf("the row was left saying `provisioning` over an instance that does not exist "+
			"and never will.\n  transcript: %s", store.transcript())
	}

	// 4. THE RECORDED MESSAGE AND THE RETURNED ERROR BOTH CARRY THE CAUSE AND THE
	//    REMEDY. Two surfaces, because they are read by different people: the row by
	//    whoever looks at the agent, the error by whoever reads this pod's log.
	for _, want := range []string{wantRefusalVariable, wantRefusalRemedyValue, wantRefusalSaveEscape} {
		if !strings.Contains(store.transcript(), want) {
			t.Errorf("the message recorded on the row does not contain %q.\n  transcript: %s",
				want, store.transcript())
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the returned error does not contain %q.\n  error: %v", want, err)
		}
	}
	if !strings.Contains(err.Error(), fixtureAgentName) {
		t.Errorf("the returned error does not name the agent it refused: %v", err)
	}

	// 5. IT IS MATCHABLE. The caller that will answer the operator's POST has to tell
	//    a refusal from a driver failure without parsing prose.
	if !errors.Is(err, ErrKickoffUndeliverable) {
		t.Errorf("the refusal does not wrap ErrKickoffUndeliverable, so no caller can "+
			"distinguish it from a backend failure: %v", err)
	}

	// 6. IT IS NOT RECORDED AS A DELIVERY FAILURE. kickoff_error means "the instance
	//    exists and its first message was not delivered" — evidence
	//    agents.DecideReconcile reads as a reason to RETRY. There is no instance here,
	//    so writing it would point a retry at something that was never created.
	if store.called("SetKickoffError") {
		t.Errorf("the refusal was written to kickoff_error, which claims an undelivered "+
			"message against an instance that does not exist.\n  transcript: %s",
			store.transcript())
	}
	// And it must never claim the opposite.
	if store.called("SetKickedOff") {
		t.Errorf("the adapter claimed a kickoff it refused to attempt.\n  transcript: %s",
			store.transcript())
	}
}

// TestTheRemedyDoesNotPromiseADeliveryTheDispatchPathCannotMake pins the
// operator-facing text as a WHOLE NORMALISED STRING.
//
// 🔴 A GUARD ON WORDS IS WALKABLE BY REWORDING, AND THIS ARTEFACT *IS* PROSE. The
// specific regression it protects against is already on the record: an earlier
// revision of KickoffRefusalReason ended "…and dispatch again", which promises a
// delivery the next dispatch did not make — at the time nothing called the gateway
// on the dispatch path (doc_seams.go entry 1 keeps that record; internal/agentkickoff
// has since closed it). An operator who followed that remedy would land in exactly the
// stranded-note behaviour this refusal exists to prevent, with no reason to look
// further. No word-level assertion catches the next paraphrase of that promise, so
// the whole string is pinned instead.
//
// ⚠ THE SENTENCE ABOVE ENDED "…plus two blockers ahead of that call site" AND THAT
// IS RETRACTED. Both are closed; see this file's own property 3 and
// TestEveryCapabilityClaimTheRefusalMakesIsTRUE. 🔴 IT IS WORTH RECORDING *WHY* IT
// SURVIVED THE ROUND THAT RETRACTED THE SAME CLAIM TWENTY LINES BELOW: that round
// swept the DELTA of its own commits, and this line pre-dated the delta — it was
// true when it was written and was falsified by a LATER commit of the same change,
// so it sat in no round's range. A claim can only be checked against the tree as it
// IS. Sweep at HEAD, never over a diff.
//
// ⚠ THE COST IS DELIBERATE AND IS THE POINT: any edit to the text fails here,
// including a cosmetic one. That is the price of a machine-readable claim about
// prose. WHEN IT FAILS, DO NOT COPY THE NEW VALUE IN REFLEXIVELY — read the new text
// against the three properties below first, then update the golden.
//
//  1. It must not promise a delivery the configuration it is read on cannot make.
//     ⚠ THIS READ "It must not tell the operator that setting the variable makes
//     the next dispatch deliver the note. It does not." That was true while
//     [KickoffDeliveryWired] was false. With internal/agentkickoff wired it DOES —
//     the refusal now fires only when no gateway is named, and naming one is
//     precisely what makes a dispatch deliverable — so the text says so.
//  2. It must name an operation that WORKS, not only a variable to set — otherwise
//     the remedy dead-ends. ⚠ THIS PROPERTY READ "it must name where a
//     non-delivery still lands afterwards (agents.kickoff_error)" AND THAT WAS A
//     PROPERTY OF THE OLD REMEDY, which told the operator to configure a gateway
//     and returned the deployment to create-then-record. There is no "afterwards"
//     on this path any more: the refusal creates nothing, so nothing lands in
//     kickoff_error, and the thing the operator needs named is the save-then-Start
//     route. Pinning a property of a superseded remedy is how a golden vouches for
//     text that no longer matches the code.
//  3. It must not forward the operator to a blocker that is CLOSED. Added after
//     this golden was measured GREEN over a stale clause — see below.
//  4. It must not name a REMEDY THE OPERATOR HAS ALREADY APPLIED. Added after the
//     third recorded regression: the text led with "REMEDY: set
//     MUSTER_AGENT_GATEWAY=hooks-sha256" while the deployment reading it HAD set it
//     and was refused anyway, because nothing called the gateway. ✅ THE SAME
//     REMEDY SATISFIES THIS PROPERTY NOW, and the reason is structural rather than a
//     reword: with KickoffDeliveryWired true the refusal fires ONLY when no gateway
//     was built, so a reader of this text has, by construction, not applied it.
//     cmd/muster-server's TestADispatchIsRefusedExactlyWhenNoGatewayIsNamed pins
//     that the gatewayed configuration is not refused.
//
// 🔴 THIS TEST CANNOT CHECK PROPERTY 3, AND SAYING SO IS THE HONEST ANSWER RATHER
// THAN A REASON TO DELETE IT. It pins BYTES. The clause "which also lists the two
// blockers above it (no port on the rendered spec, and the token name the container
// reads)" became false when agentspec started declaring the port and shipping the
// gateway token — and this test stayed GREEN, because the text had not changed.
// A golden is only ever evidence about CHANGE; it is structurally incapable of
// noticing that an unchanged sentence stopped being true. Its own doc named two
// properties to re-read on failure and neither was "does this still describe the
// right blockers", so the one instrument that could have caught it was vouching for
// it instead.
//
// ⚠ WHAT COVERS PROPERTY 3 IS A DIFFERENT TEST, AND ONLY PARTLY:
// TestEveryCapabilityClaimTheRefusalMakesIsTRUE below pairs each positive claim in
// the text with a predicate that executes. That genuinely pins the claims it
// ENUMERATES — revert the port and it reddens — and it cannot see a NEW false claim
// somebody adds. For that, this golden plus a human reading properties 1-3 is the
// whole of the mechanism. Stated plainly because the alternative is a reader
// believing the pair is complete.
func TestTheRemedyDoesNotPromiseADeliveryTheDispatchPathCannotMake(t *testing.T) {
	const golden = "dispatch REFUSED and NOTHING was provisioned: this deployment cannot " +
		"deliver an agent's first turn, so beginning the work is impossible and creating the " +
		"instance would only produce a pod that reads healthy and was never told what to do. " +
		"Use \"Save for later\", then Start the agent — that brings the instance up without " +
		"claiming a first turn nothing can deliver. CAUSE: this deployment names no agent " +
		"gateway, and a DELIVERED kickoff is a model turn sent through one. REMEDY: set " +
		"MUSTER_AGENT_GATEWAY=hooks-sha256 together with MUSTER_AGENT_GATEWAY_MODEL (and the agent " +
		"runtime-config bundle boot requires with them) and restart muster; the kickoff deliverer " +
		"(internal/agentkickoff) then hands each dispatched agent's pending note to its instance " +
		"once the instance is ready. See cmd/muster-server/doc_seams.go entry 1. Do not go looking " +
		"at the spec or the credential on the way: the rendered spec declares the gateway port and " +
		"the instance receives the token the bearer is derived from."

	if KickoffRefusalReason != golden {
		t.Errorf("the operator-facing refusal text changed.\n  got:  %q\n  want: %q\n"+
			"    Re-read the new text against the THREE properties in this test's doc BEFORE "+
			"updating the golden. Three regressions are on the record: a remedy that promises "+
			"the next dispatch will deliver the note (it will not), a remedy that forwarded the "+
			"operator to two blockers that had been CLOSED — which this golden was green over, "+
			"because the text had not changed — and a remedy the operator had ALREADY APPLIED "+
			"(\"set MUSTER_AGENT_GATEWAY\" on a deployment where it is set), which this golden "+
			"was green over for the same reason.", KickoffRefusalReason, golden)
	}
}

// TestTheShortRefusalIsAPrefixOfTheLongOne pins the one relationship between the
// two operator-facing strings.
//
// 🔴 THEY ARE TWO SURFACES FOR ONE REFUSAL AND THEY MUST NOT COME TO SAY DIFFERENT
// THINGS. [KickoffRefusalSummary] is what internal/api puts in the 409 body, which
// internal/ui toasts verbatim to the person who clicked; [KickoffRefusalReason] is
// what reaches the log, the agent row's error_message and the returned error. Two
// independently-written strings about one condition is the shape where one of them
// goes stale — and the stale one would be the short one, because it is the one a
// human reads and the long one is the one a test golden pins.
//
// ⚠ IT ASSERTS A PREFIX RATHER THAN "CONTAINS", on purpose: a prefix makes the
// summary the OPENING of the long form, so the long form cannot acquire a different
// first sentence while still technically containing the short one somewhere.
func TestTheShortRefusalIsAPrefixOfTheLongOne(t *testing.T) {
	if !strings.HasPrefix(KickoffRefusalReason, KickoffRefusalSummary) {
		t.Errorf("KickoffRefusalSummary is no longer the opening of KickoffRefusalReason, so "+
			"the toast an operator reads and the reason stored on the row can disagree.\n"+
			"  summary: %q\n  reason:  %q", KickoffRefusalSummary, KickoffRefusalReason)
	}

	// CONTROL: a prefix assertion is trivially satisfied by an EMPTY summary, which
	// would mean the 409 body is blank — a refusal nobody can read, which is the
	// whole defect this pair exists to close.
	if KickoffRefusalSummary == "" {
		t.Fatal("KickoffRefusalSummary is empty, so POST /agents answers 409 with no body " +
			"and the operator is told a number")
	}
	// And it must carry the remedy, which is the half the summary exists for: the
	// long form's mechanism paragraph is exactly what a toast loses.
	if !strings.Contains(KickoffRefusalSummary, wantRefusalSaveEscape) {
		t.Errorf("the short refusal does not name %q, so the toast tells the operator what "+
			"failed and not what still works.\n  summary: %q",
			wantRefusalSaveEscape, KickoffRefusalSummary)
	}
}

// TestEveryCapabilityClaimTheRefusalMakesIsTRUE is what the golden above cannot be.
//
// 🔴 IT PAIRS PROSE WITH A PREDICATE THAT EXECUTES, which is the only shape that can
// notice an UNCHANGED sentence becoming false. Each row is "if the text says this,
// then this must hold". The text currently tells an operator NOT to go looking at the
// port or the token; if either regressed, that instruction would walk them away from
// the real cause — so the claim and the behaviour are asserted together.
//
// ⚠ ITS LIMIT, NAMED RATHER THAN LEFT TO BE DISCOVERED: it covers the claims
// ENUMERATED here. A new false claim added to the text is invisible to it, and is
// caught only by the golden going red and a human applying properties 1-3. This is
// not a complete guard on prose accuracy; there is no such guard in this package.
//
// ⚠ THE PHRASE HALF IS SPELLED AND THEREFORE WALKABLE BY REWORDING — rewrite the
// sentence and the row goes dormant rather than red. That is why the golden stays:
// a reword cannot be silent while the golden pins the whole string. Neither test is
// sufficient alone, which is the honest description of the pair.
func TestEveryCapabilityClaimTheRefusalMakesIsTRUE(t *testing.T) {
	// A spec built the way a dispatch builds one, through this package's own
	// configuration type — so the predicates below read what an instance would
	// actually receive rather than a hand-written literal.
	cfg := agentspec.Config{
		ImageRepo:  "registry.example.test/muster/agent-runtime",
		APIBaseURL: "http://muster.example.test:8105",
	}
	row := agents.Agent{ID: 4291, Name: "harbour-kestrel", HooksToken: "fixture-token-9f31c7"}
	spec, err := agentspec.Build(row, cfg, agentspec.Options{})
	if err != nil {
		t.Fatalf("agentspec.Build: %v", err)
	}

	secret := func(name string) string {
		for _, e := range spec.Secrets {
			if e.Name == name {
				return e.Value
			}
		}
		return ""
	}

	claims := []struct {
		phrase string
		holds  func() bool
		why    string
	}{{
		phrase: "the rendered spec declares the gateway port",
		holds:  func() bool { return spec.PortNumber(provision.DefaultPortName) != 0 },
		why: "Spec.PortNumber(provision.DefaultPortName) is 0, so k8s renderService creates no " +
			"Service and Driver.Endpoint answers provision.ErrNoEndpoint. The refusal text tells " +
			"the operator not to look here, which would send them past the real cause.",
	}, {
		phrase: "the instance receives the token the bearer is derived from",
		holds:  func() bool { return secret(agentspec.EnvGatewayToken) == row.HooksToken },
		why: "the spec does not carry the row's token under agentspec.EnvGatewayToken, so the " +
			"container derives its half of the bearer from an unset variable and every turn is a " +
			"401. The refusal text tells the operator not to look here.",
	}}

	var checked int
	for _, c := range claims {
		if !strings.Contains(KickoffRefusalReason, c.phrase) {
			// NOT a failure: the sentence may legitimately be reworded. It IS reported,
			// because a dormant row looks exactly like a passing one.
			t.Logf("claim %q is no longer in the text, so its predicate was not applied. "+
				"If the capability claim was reworded rather than dropped, update the phrase.",
				c.phrase)
			continue
		}
		checked++
		if !c.holds() {
			t.Errorf("the refusal text claims %q and it is FALSE.\n    %s", c.phrase, c.why)
		}
	}

	// 🔴 POSITIVE CONTROL, AS A COUNT. Every row going dormant is byte-identical to
	// every row passing, and a reworded sentence would silently empty this test.
	if checked != len(claims) {
		t.Errorf("instrument check FAILED: %d of %d capability claims were actually applied. "+
			"A dormant row asserts nothing; re-point its phrase at the current wording.",
			checked, len(claims))
	}

	// And the retracted forwarding must not come back. SPELLED, hence walkable —
	// it survives a lazy re-golden, which is the one thing the golden cannot.
	for _, retracted := range []string{
		"two blockers above it",
		"no port on the rendered spec",
		"the token name the container reads",
	} {
		if strings.Contains(KickoffRefusalReason, retracted) {
			t.Errorf("the refusal text has regained the retracted clause %q. Both blockers it "+
				"forwards to are closed; this text is logged, stored in agents.error_message and "+
				"returned to the caller, so it would send an operator hunting fixed defects.",
				retracted)
		}
	}
}

// TestAKickoffIsNOTRefusedWhenTheAdapterIsToldItCanBeDelivered IS THE POSITIVE
// CONTROL, and without it every other test in this file is satisfied by a refusal
// that fires unconditionally.
//
// 🔴 THE REFUSAL MUST BE CONDITIONAL ON WHAT THIS ADAPTER IS TOLD, NOT ON ITS OWN
// IDENTITY. A refusal keyed on anything else — a build tag, the package's name, a
// guess about what the binary contains — cannot be lifted by the thing that will
// eventually make a kickoff deliverable, and the only way to find that out is to
// pin the arm this test drives.
//
// ⚠ THIS TEST WAS CALLED TestAKickoffIsNOTRefusedWhenAGatewayIsConfigured AND ITS
// DOC ARGUED FROM A FALSE PREMISE. It read "the only reason a kickoff cannot be
// delivered on the deployment that motivated this change is that
// MUSTER_AGENT_GATEWAY is unset", and that was measured wrong: a configured gateway
// with nothing calling it delivers nothing either, so naming one does NOT make a
// kickoff deliverable. The old name said this arm was the gatewayed DEPLOYMENT;
// what it actually drives is an adapter TOLD a kickoff is deliverable, which today
// no deployment was until internal/agentkickoff landed (see [KickoffDeliveryWired]).
// The name stays a claim about what the adapter is TOLD, which is what it tests.
//
// ⚠ IT ASSERTS THE WHOLE PRE-EXISTING SEQUENCE, not merely a nil error. "Dispatch
// did not fail" is also true of a Dispatch that silently did nothing, which is the
// shape this package's own header calls the lie it exists to prevent.
func TestAKickoffIsNOTRefusedWhenTheAdapterIsToldItCanBeDelivered(t *testing.T) {
	a, store, driver := newAdapter(t, nil)

	if err := a.Dispatch(context.Background(), fixtureAgentID, true); err != nil {
		t.Fatalf("Dispatch(kickoff=true) was refused by an adapter TOLD a kickoff is "+
			"deliverable, so the refusal is firing on this package's own identity rather "+
			"than on what it was told — nothing a deliverer lands could lift it: %v\n"+
			"  transcript: %s", err, store.transcript())
	}

	if driver.creates != 1 {
		t.Errorf("the driver created %d instance(s) on the deliverable arm, want 1.\n"+
			"  transcript: %s", driver.creates, store.transcript())
	}
	if !store.called("SetHooksToken") {
		t.Errorf("no token was minted for an instance that was created.\n  transcript: %s",
			store.transcript())
	}
	if !strings.Contains(store.transcript(), "UpdateStatus(4291,provisioning,") {
		t.Errorf("the row does not say `provisioning` after a dispatch that created an "+
			"instance and owes a first turn.\n  transcript: %s", store.transcript())
	}
	// ⚠ THIS ASSERTION USED TO REQUIRE A SetKickoffError HERE ("the existing record
	// must remain"). With a deliverer in the process the record is false until a
	// delivery actually fails, so the arm now records NOTHING — and still never
	// claims the delivery itself, which is internal/agentkickoff's.
	if store.called("SetKickoffError") || store.called("SetKickedOff") {
		t.Errorf("the deliverable arm wrote kickoff bookkeeping that is not this package's to "+
			"write: a NOT-delivered record before any attempt, or a delivery claim.\n"+
			"  transcript: %s", store.transcript())
	}

	// 🔴 AND THE REFUSAL'S OWN TEXT MUST BE ABSENT. This is the assertion that fails
	// on a refusal that fires in both configurations while still creating the
	// instance — a mutant a nil-error check alone cannot see.
	if strings.Contains(store.transcript(), "REFUSED and NOTHING was provisioned") {
		t.Errorf("the undeliverable-kickoff refusal fired on the DELIVERABLE arm.\n"+
			"  transcript: %s", store.transcript())
	}
	if strings.Contains(store.transcript(), "UpdateStatus(4291,error,") {
		t.Errorf("the row was marked %s on a dispatch that succeeded.\n  transcript: %s",
			agents.StatusError, store.transcript())
	}
}

// TestASaveIsNeverRefusedWhenNoKickoffCanBeDelivered guards the operation the
// refusal must not touch.
//
// 🔴 A SAVE ASKS FOR NOTHING THIS CONFIGURATION CANNOT DO. "Save for later" —
// action=save in the UI, kickoff=false here — deliberately provisions nothing and
// stores `stopped`; there is no kickoff to deliver, so there is nothing to refuse.
// Refusing it would remove a working operation to punish a missing gateway, and it
// is the operation the refusal's own remedy text points the operator at, so the
// remedy would become a lie.
//
// ⚠ IT ALSO PINS THAT THE SAVE PATH STILL CREATES NOTHING, which is a property the
// refusal branch sits directly next to in the source and could plausibly be moved
// above by a later edit.
func TestASaveIsNeverRefusedWhenNoKickoffCanBeDelivered(t *testing.T) {
	a, store, driver := newAdapterWithNoKickoffDelivery(t, nil)

	if err := a.Dispatch(context.Background(), fixtureAgentID, false); err != nil {
		t.Fatalf("Dispatch(kickoff=false) — \"Save for later\" — was refused on a deployment "+
			"with no gateway: %v\n  transcript: %s", err, store.transcript())
	}

	if !strings.Contains(store.transcript(), "UpdateStatus(4291,stopped,") {
		t.Errorf("a save did not store %s.\n  transcript: %s",
			agents.StatusStopped, store.transcript())
	}
	if driver.creates != 0 {
		t.Errorf("a save created %d instance(s), want 0.\n  transcript: %s",
			driver.creates, store.transcript())
	}
	if strings.Contains(store.transcript(), "UpdateStatus(4291,error,") {
		t.Errorf("a save was recorded as a failure.\n  transcript: %s", store.transcript())
	}
	if strings.Contains(store.transcript(), wantRefusalVariable) {
		t.Errorf("the undeliverable-kickoff refusal text was written for a save, which asked "+
			"for no kickoff.\n  transcript: %s", store.transcript())
	}
}

// TestStartStillWorksWhenNoKickoffCanBeDelivered guards the capability a wider
// refusal would have deleted.
//
// 🔴 THE REFUSAL IS ON THE CREATE-WITH-KICKOFF PATH ONLY, AND THIS IS WHERE THAT
// DECISION IS ASSERTED RATHER THAN ARGUED. Dispatch(kickoff=true) is the only call
// that promises "begin this work now"; Start promises "bring this instance up",
// which this build genuinely can do — the pod runs and its logs stream. Refusing
// Start would remove three working operations at once: restarting a stopped agent,
// recovering one whose instance was evicted, and provisioning an agent that was
// deliberately SAVED — the escape route the refusal's own text recommends.
//
// ⚠ AND NOTHING IS HIDDEN BY ALLOWING IT: the owed first turn is still recorded in
// kickoff_error, where it is honest, because an instance now exists for a retry to
// act on. Each case asserts that record, so a Start that quietly stopped reporting
// the non-delivery fails here.
func TestStartStillWorksWhenNoKickoffCanBeDelivered(t *testing.T) {
	t.Run("restart of a live instance", func(t *testing.T) {
		a, store, driver := newAdapterWithNoKickoffDelivery(t, nil)
		seedInstance(t, a, store, driver)

		if err := a.Start(context.Background(), fixtureAgentID); err != nil {
			t.Fatalf("Start was refused on a deployment with no gateway, so a stopped agent "+
				"cannot be restarted: %v\n  transcript: %s", err, store.transcript())
		}
		if len(driver.scaled) == 0 || driver.scaled[len(driver.scaled)-1] != 1 {
			t.Errorf("Start did not scale the instance up: scaled=%v\n  transcript: %s",
				driver.scaled, store.transcript())
		}
		if driver.creates != 0 {
			t.Errorf("Start re-created an instance the backend already has (%d create(s)).\n"+
				"  transcript: %s", driver.creates, store.transcript())
		}
		if !store.called("SetKickoffError") {
			t.Errorf("Start stopped recording the owed first turn in kickoff_error.\n"+
				"  transcript: %s", store.transcript())
		}
		if strings.Contains(store.transcript(), "UpdateStatus(4291,error,") {
			t.Errorf("Start marked the row %s on a deployment with no gateway. That is the "+
				"refusal leaking onto a path that works.\n  transcript: %s",
				agents.StatusError, store.transcript())
		}
	})

	// 🔴 THE SAVED-AGENT CASE IS THE ONE THE REMEDY TEXT DEPENDS ON. The refusal tells
	// the operator to save the agent and start it later; if Start refused to create
	// the instance a save deliberately did not, that advice would dead-end.
	t.Run("first start of a saved agent creates its instance", func(t *testing.T) {
		a, store, driver := newAdapterWithNoKickoffDelivery(t, nil)

		if err := a.Start(context.Background(), fixtureAgentID); err != nil {
			t.Fatalf("Start of a never-provisioned (saved) agent was refused, which dead-ends "+
				"the remedy the refusal recommends: %v\n  transcript: %s", err, store.transcript())
		}
		if driver.creates != 1 {
			t.Errorf("Start of a saved agent created %d instance(s), want 1.\n  transcript: %s",
				driver.creates, store.transcript())
		}
		if !store.called("SetHooksToken") {
			t.Errorf("Start created the instance without minting its token.\n  transcript: %s",
				store.transcript())
		}
		if !store.called("SetKickoffError") {
			t.Errorf("Start created an instance owing a first turn and recorded nothing.\n"+
				"  transcript: %s", store.transcript())
		}
	})

	// An agent that HAS been kicked off owes nothing, so its restart must land on
	// `running` and record no non-delivery — the branch the refusal is furthest from,
	// included so "Start is untouched" is a claim about both of its arms.
	t.Run("restart of an already-kicked-off agent owes nothing", func(t *testing.T) {
		a, store, driver := newAdapterWithNoKickoffDelivery(t, func(s *recordingStore, _ *flakyDriver) {
			s.agent.KickedOff = true
		})
		seedInstance(t, a, store, driver)

		if err := a.Start(context.Background(), fixtureAgentID); err != nil {
			t.Fatalf("Start of an already-kicked-off agent was refused: %v\n  transcript: %s",
				err, store.transcript())
		}
		if !strings.Contains(store.transcript(), "UpdateStatus(4291,running,") {
			t.Errorf("a restart of an agent that owes no first turn did not land on %s.\n"+
				"  transcript: %s", agents.StatusRunning, store.transcript())
		}
		if store.called("SetKickoffError") {
			t.Errorf("a non-delivery was recorded for an agent that was already kicked off.\n"+
				"  transcript: %s", store.transcript())
		}
	})
}

// TestARefusedKickoffReportsAFailedRecordRatherThanSwallowingIt pins the one place
// refuseKickoff deliberately differs from fail().
//
// 🔴 A REFUSAL NOBODY COULD RECORD IS A DIFFERENT OUTCOME FROM A REFUSAL. fail()
// logs its own bookkeeping failure and returns the underlying cause, because there
// the cause is what the caller needs. Here the refusal IS the outcome, so if the row
// could not be marked the caller's log line must say THAT — otherwise the operator
// is left with a row still reading `provisioning` and a log line claiming the
// refusal was recorded.
func TestARefusedKickoffReportsAFailedRecordRatherThanSwallowingIt(t *testing.T) {
	wantErr := errors.New("database is in recovery")
	a, store, driver := newAdapterWithNoKickoffDelivery(t, func(s *recordingStore, _ *flakyDriver) {
		s.updateErr = wantErr
	})

	err := a.Dispatch(context.Background(), fixtureAgentID, true)
	if err == nil {
		t.Fatalf("Dispatch returned nil when it could neither deliver the kickoff nor record "+
			"the refusal.\n  transcript: %s", store.transcript())
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("the error does not carry the store failure that prevented the refusal being "+
			"recorded: %v", err)
	}
	// The refusal still created nothing — the store failure must not fall through to
	// the create path.
	if driver.creates != 0 {
		t.Errorf("an instance was created after the refusal failed to record (%d create(s)).\n"+
			"  transcript: %s", driver.creates, store.transcript())
	}
}
