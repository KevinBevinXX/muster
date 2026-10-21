package agentprovision

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// THE FAKE STORE RECORDS THE CALL *SEQUENCE*, NOT JUST THE FINAL STATE.
//
// 🔴 A FINAL-STATE FAKE CANNOT SEE THE DEFECT THIS PACKAGE'S HEADER IS ABOUT.
// "Did SetKickedOff get called" is a question about whether a call HAPPENED, and
// a fake that only remembers the last status written answers it with silence — a
// SetKickedOff(true) followed by anything else leaves no trace. So every mutating
// method appends its name and arguments, and the assertions read the transcript.
// ---------------------------------------------------------------------------

// recordingStore embeds agents.Store so only the methods this adapter actually
// calls have to exist. Any other call is a nil-interface panic, which is the
// loud outcome: a new store call added to the adapter without being considered
// here fails rather than passing over a stub that returns a zero value.
type recordingStore struct {
	agents.Store

	agent agents.Agent
	calls []string

	getErr        error
	updateErr     error
	deleteErr     error
	kickoffErrErr error
}

func (s *recordingStore) record(format string, args ...any) {
	s.calls = append(s.calls, fmt.Sprintf(format, args...))
}

// refuse is what makes every context-related assertion in this file non-vacuous.
//
// 🔴 A FAKE THAT IGNORES ITS ctx CANNOT SEE A CONTEXT DEFECT, AND THAT IS HOW THE
// FIX FOR ONE SHIPPED UNGUARDED. Every method here took `_ context.Context`, so a
// mutant that handed the status write an ALREADY-EXPIRED context — the exact defect
// the bookkeeping budget exists to remove — SURVIVED the whole suite. A store fake
// stands in for a database, and a database call on a dead context fails.
//
// 🔴 IT IS NOW WIRED INTO *EVERY* METHOD, WHICH IT WAS NOT WHEN THIS BANNER FIRST
// CLAIMED IT. Three of eight honoured their context while this paragraph said "every
// method here" — a description wider than its body, in the one file whose stated
// purpose is making context defects visible. The gap was not theoretical: a mutant
// handing store.Delete a cancelled context SURVIVED, and that call really was on the
// wrong budget (see Destroy). The fix was to widen the BODY to the sentence, never to
// narrow the sentence to the body.
//
// ⚠ WIRED IS NOT EXERCISED, AND THE COUNT BELOW IS THE SECOND ATTEMPT AT IT — the
// first said THREE and was measured wrong, which is worth more than the number itself:
// a coverage ledger that over-counts names an uncovered method as covered, and it is
// the exact artefact a later reader consults before deciding a context mutant is
// already handled.
//
// TWO sites are driven on a dead context by a test today:
//   - UpdateStatus, by TestATimedOutOperationStillRECORDSItsFailure
//   - Delete, by TestATimedOutDestroyStillDELETESTheRow
//
// SetKickoffError is NOT one of them, though the first draft claimed it: in that test
// Dispatch returns at create's error, so recordUndeliveredKickoff is never reached.
// Transcript, measured: Get -> SetHooksToken -> driver.Create -> UpdateStatus(error).
//
// So SIX of the eight are wired and unexercised. A mutant making SetHooksToken ignore
// its context SURVIVES, because every test reaches the mint while the operation budget
// is still alive. That is a coverage statement rather than a defect: SetHooksToken is
// on the OPERATION budget on purpose — minting is part of doing the work, not a record
// of it, so a dead context there should fail the operation and be recorded by fail().
// Do not read the wiring as proof the other six are covered.
func (s *recordingStore) refuse(ctx context.Context, what string) error {
	if err := ctx.Err(); err != nil {
		// 🔴 THE PREFIX MUST NOT COLLIDE WITH THE CALL IT REFUSED, AND IT DID FOR ONE
		// ROUND. This recorded "Delete(4291) REFUSED(…)", which shares the "Delete("
		// prefix that called() and indexOf() match on — so a REFUSED delete made
		// called("Delete") answer TRUE. That inverts every assertion phrased over
		// those two helpers: "the row was deleted" passes over a delete that was
		// refused (the exact defect the bookkeeping budget exists to prevent), and
		// "the row was NOT deleted" fails over a row that really was kept. Recording
		// the refusal under its OWN leading token makes a refusal unmistakably not a
		// call, and refused() below is how a test asserts one deliberately.
		s.record("REFUSED:%s(%v)", what, err)
		return err
	}
	return nil
}

// refused reports whether a call to name was refused by a dead context.
func (s *recordingStore) refused(name string) bool {
	for _, c := range s.calls {
		if strings.HasPrefix(c, "REFUSED:"+name+"(") {
			return true
		}
	}
	return false
}

func (s *recordingStore) Get(ctx context.Context, id int64) (agents.Agent, error) {
	if err := s.refuse(ctx, fmt.Sprintf("Get(%d)", id)); err != nil {
		return agents.Agent{}, err
	}
	s.record("Get(%d)", id)
	if s.getErr != nil {
		return agents.Agent{}, s.getErr
	}
	return s.agent, nil
}

func (s *recordingStore) UpdateStatus(ctx context.Context, id int64, status, lastOutput, errMsg string) error {
	if err := s.refuse(ctx, fmt.Sprintf("UpdateStatus(%d,%s)", id, status)); err != nil {
		return err
	}
	s.record("UpdateStatus(%d,%s,out=%q,err=%q)", id, status, lastOutput, errMsg)
	return s.updateErr
}

func (s *recordingStore) SetHooksToken(ctx context.Context, id int64, token string) error {
	if err := s.refuse(ctx, fmt.Sprintf("SetHooksToken(%d)", id)); err != nil {
		return err
	}
	// The token's VALUE is deliberately not recorded — it is a secret, and a test
	// transcript is a log. Its LENGTH is, because that is the property worth
	// pinning (32 random bytes as 64 hex characters).
	s.record("SetHooksToken(%d,len=%d)", id, len(token))
	s.agent.HooksToken = token
	return nil
}

func (s *recordingStore) SetGatewaySecret(ctx context.Context, id int64, secret string) error {
	if err := s.refuse(ctx, fmt.Sprintf("SetGatewaySecret(%d)", id)); err != nil {
		return err
	}
	// The value is a secret: only its length is recorded.
	s.record("SetGatewaySecret(%d,len=%d)", id, len(secret))
	s.agent.GatewaySecret = secret
	return nil
}

func (s *recordingStore) SetKickedOff(ctx context.Context, id int64, v bool) error {
	if err := s.refuse(ctx, fmt.Sprintf("SetKickedOff(%d)", id)); err != nil {
		return err
	}
	s.record("SetKickedOff(%d,%t)", id, v)
	return nil
}

func (s *recordingStore) SetKickoffError(ctx context.Context, id int64, msg string) error {
	if err := s.refuse(ctx, fmt.Sprintf("SetKickoffError(%d)", id)); err != nil {
		return err
	}
	s.record("SetKickoffError(%d,%q)", id, msg)
	return s.kickoffErrErr
}

func (s *recordingStore) RecordKickoffDelivery(ctx context.Context, id int64, pod string, restarts int32) error {
	if err := s.refuse(ctx, fmt.Sprintf("RecordKickoffDelivery(%d)", id)); err != nil {
		return err
	}
	s.record("RecordKickoffDelivery(%d,%s,%d)", id, pod, restarts)
	return nil
}

func (s *recordingStore) ClearKickoffDelivery(ctx context.Context, id int64) error {
	if err := s.refuse(ctx, fmt.Sprintf("ClearKickoffDelivery(%d)", id)); err != nil {
		return err
	}
	s.record("ClearKickoffDelivery(%d)", id)
	return nil
}

func (s *recordingStore) Delete(ctx context.Context, id int64) error {
	if err := s.refuse(ctx, fmt.Sprintf("Delete(%d)", id)); err != nil {
		return err
	}
	s.record("Delete(%d)", id)
	return s.deleteErr
}

// indexOf returns the position of the first recorded call starting with name+"(",
// or -1. It is what makes an ORDER assertion expressible.
func (s *recordingStore) indexOf(name string) int {
	for i, c := range s.calls {
		if strings.HasPrefix(c, name+"(") {
			return i
		}
	}
	return -1
}

// transcript joins the recorded calls for a failure message.
func (s *recordingStore) transcript() string { return strings.Join(s.calls, " -> ") }

// called reports whether any recorded call starts with name+"(".
func (s *recordingStore) called(name string) bool {
	for _, c := range s.calls {
		if strings.HasPrefix(c, name+"(") {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// A DRIVER THAT CAN BE MADE TO FAIL PER METHOD, WITH THE DRIVER'S OWN SENTINELS.
//
// ⚠ IT WRAPS provision.Noop RATHER THAN REIMPLEMENTING IT. Noop is a real driver
// with declared capabilities its own CheckSpec enforces and a Destroy that
// removes state, so the happy paths below run against something whose behaviour
// the driver contract suite already pins. Only the failure injection is new.
// ---------------------------------------------------------------------------

type flakyDriver struct {
	*provision.Noop

	// rec is the SHARED transcript, so driver calls and store calls interleave in
	// one ordered list.
	//
	// 🔴 WITHOUT THIS THE SEQUENCE WAS RECORDED AND NEVER READ AS ONE. The store
	// fake's header claimed it recorded the call SEQUENCE, but every assertion was
	// membership (a prefix match, or Contains over a joined string), so a mutant
	// that moved the `stopped` status write BEFORE the create — exactly the
	// row-leads-the-backend ordering error — SURVIVED. Two separate transcripts
	// cannot express "A happened before B" at all, whatever the assertions do.
	rec *recordingStore

	scaleErr   error
	destroyErr error
	createErr  error
	// burnBudget makes Create block until the operation's context expires.
	burnBudget bool
	// burnBudgetOnDestroy does the same for Destroy and then SUCCEEDS, which is the
	// shape that matters: the teardown worked, so the row must be deleted — on a
	// budget that is already gone.
	burnBudgetOnDestroy bool

	scaled   []int
	destroys int
	updates  int
	creates  int
}

func (d *flakyDriver) note(format string, args ...any) {
	if d.rec != nil {
		d.rec.record(format, args...)
	}
}

func (d *flakyDriver) Create(ctx context.Context, spec provision.Spec) error {
	d.creates++
	d.note("driver.Create(%s)", spec.Ref.Name)
	if d.burnBudget {
		// Consume the operation's whole budget, the way a wedged apiserver call
		// does, and report what the caller would see.
		<-ctx.Done()
		return fmt.Errorf("create timed out: %w", ctx.Err())
	}
	if d.createErr != nil {
		return d.createErr
	}
	return d.Noop.Create(ctx, spec)
}

func (d *flakyDriver) Update(ctx context.Context, spec provision.Spec) error {
	d.updates++
	d.note("driver.Update(%s)", spec.Ref.Name)
	return d.Noop.Update(ctx, spec)
}

func (d *flakyDriver) Scale(ctx context.Context, ref provision.Ref, replicas int) error {
	d.scaled = append(d.scaled, replicas)
	d.note("driver.Scale(%s,%d)", ref.Name, replicas)
	if d.scaleErr != nil {
		return d.scaleErr
	}
	return d.Noop.Scale(ctx, ref, replicas)
}

func (d *flakyDriver) Destroy(ctx context.Context, ref provision.Ref) error {
	d.destroys++
	d.note("driver.Destroy(%s)", ref.Name)
	if d.burnBudgetOnDestroy {
		<-ctx.Done()
		// SUCCEEDS despite the expiry — the instance really is gone, so the caller
		// owes the row a delete and has no budget left to do it on.
		return nil
	}
	if d.destroyErr != nil {
		return d.destroyErr
	}
	return d.Noop.Destroy(ctx, ref)
}

// ---------------------------------------------------------------------------
// FIXTURES.
//
// 🔴 EVERY VALUE IS PAIRWISE DISTINCT AND DISTINCT FROM ANY CONSTANT THE
// ASSERTIONS NAME. A fixture whose id happened to equal its replica count, or
// whose name equalled the namespace prefix, cannot see a mutant that swaps them —
// it would produce the asserted value by accident and SURVIVE a fully green
// suite. The control is mechanical: 4291 cannot be 0 or 1, and "harbour-kestrel"
// cannot be "devpod-".
// ---------------------------------------------------------------------------

const (
	fixtureAgentID   = int64(4291)
	fixtureAgentName = "harbour-kestrel"
)

func fixtureAgent() agents.Agent {
	return agents.Agent{
		ID:          fixtureAgentID,
		Name:        fixtureAgentName,
		Namespace:   agents.NamespaceFor(agents.NamespacePrefix, fixtureAgentName),
		DisplayName: "#633 rewire the seam",
		PendingNote: "read the plan and report what is missing",
		Status:      agents.StatusProvisioning,
	}
}

func fixtureSpecConfig() agentspec.Config {
	return agentspec.Config{
		ImageRepo:  "registry.example.test/muster/agent-runtime",
		APIBaseURL: "http://muster.example.test:8105",
	}
}

// newAdapter builds an adapter over a recording store and a wrapped Noop, for a
// process TOLD that a kickoff is deliverable.
//
// 🔴 THAT DEFAULT IS THE OPPOSITE POLARITY FROM Config's OWN ZERO VALUE, AND SAYING
// SO IS THE POINT. Config.KickoffDeliverable defaults to false because production
// must fail closed; this helper defaults to TRUE because every test written before
// the refusal existed was written against the create-then-record path, and silently
// re-pointing those assertions at a refusal would have changed what a dozen existing
// guards claim without anyone choosing to. They still assert exactly what they
// always did, and they now collectively double as a broad control that the refusal
// does NOT fire on the arm that can deliver.
//
// ⚠ THIS SAID "for a process that HAS a gateway", WHICH WAS WRONG WHILE NOTHING
// CALLED ONE — no deployment was on this arm. With internal/agentkickoff wired
// ([KickoffDeliveryWired] true) it is the arm of every deployment that names a
// gateway.
//
// ⚠ SO A NEW Dispatch TEST GETS THE DELIVERABLE PATH UNLESS IT ASKS OTHERWISE. Use
// newAdapterWithNoKickoffDelivery for a deployment's configuration; it is named
// rather than expressed as a bool at 20 call sites for exactly that reason.
func newAdapter(t *testing.T, tune func(*recordingStore, *flakyDriver)) (*Adapter, *recordingStore, *flakyDriver) {
	t.Helper()
	return newAdapterFor(t, true, tune)
}

// newAdapterWithNoKickoffDelivery builds an adapter for THE CONFIGURATION THIS
// REFUSAL EXISTS FOR: a provisioner wired and nothing in the process that could hand
// a note to a model gateway, because MUSTER_AGENT_GATEWAY is unset (buildGateway
// returns nil). (Before internal/agentkickoff it was every deployment, because no
// call site invoked a gateway that was built — see [KickoffDeliveryWired].)
func newAdapterWithNoKickoffDelivery(t *testing.T, tune func(*recordingStore, *flakyDriver)) (*Adapter, *recordingStore, *flakyDriver) {
	t.Helper()
	return newAdapterFor(t, false, tune)
}

func newAdapterFor(t *testing.T, kickoffDeliverable bool, tune func(*recordingStore, *flakyDriver)) (*Adapter, *recordingStore, *flakyDriver) {
	t.Helper()
	store := &recordingStore{agent: fixtureAgent()}
	driver := &flakyDriver{Noop: provision.MustNewNoop(), rec: store}
	if tune != nil {
		tune(store, driver)
	}
	a, err := New(Config{
		Driver:             driver,
		Store:              store,
		Spec:               fixtureSpecConfig(),
		KickoffDeliverable: kickoffDeliverable,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a, store, driver
}

// seedInstance puts the agent's instance in the driver the way a prior Dispatch
// would have, then clears the store transcript so the call under test is the only
// thing in it.
//
// 🔴 IT WRITES THROUGH THE EMBEDDED Noop, NOT THROUGH flakyDriver, so a test that
// injects a Create error still gets a seeded backend. Seeding through the
// injection point would make "Stop over a live instance" and "Stop over a driver
// that cannot create" the same fixture, and the first is the case that exists.
// The driver's own counters are reset for the same reason.
func seedInstance(t *testing.T, a *Adapter, store *recordingStore, driver *flakyDriver) {
	t.Helper()
	if store.agent.HooksToken == "" {
		// 🔴 THE TOKEN IS WRITTEN BACK TO THE ROW, NOT JUST INTO THE SEED SPEC,
		// AND THE FIRST DRAFT DID NOT DO THAT. The token is a Spec.Secrets entry,
		// so it is part of the spec's fingerprint: seeding with one and leaving the
		// row tokenless made the next call mint a DIFFERENT token, build a
		// different spec, and get provision.ErrDivergentSpec — a fixture artefact
		// that reads exactly like an adapter bug. A real dispatch leaves the row
		// and the backend agreeing; so does this.
		store.agent.HooksToken = strings.Repeat("7", 64)
	}
	ag := store.agent
	spec, err := a.buildSpec(context.Background(), ag)
	if err != nil {
		t.Fatalf("seed: buildSpec: %v", err)
	}
	if err := driver.Noop.Create(context.Background(), spec); err != nil {
		t.Fatalf("seed: Create: %v", err)
	}
	store.calls = nil
	driver.creates, driver.updates, driver.destroys, driver.scaled = 0, 0, 0, nil
}

// ---------------------------------------------------------------------------
// THE GUARD PLAN STEP 22b OWED.
// ---------------------------------------------------------------------------

// TestTheAdapterNeverClaimsAKickoffItCannotDeliver is the guard named in this
// package's header, and it is the deliverable of the step that added the package.
//
// 🔴 agents.KickedOff MEANS "THE MESSAGE WAS HANDED TO A READY GATEWAY", and this
// adapter has no gateway. Setting it would be a claim about a delivery that did
// not happen — the same class of lie as a 200 over a pod that does not exist, and
// worse to debug, because agents.DecideReconcile branches on the flag: an agent
// marked kicked-off is never retried, so a false flag converts "the first turn
// has not happened yet" into "the first turn happened and the agent chose to do
// nothing", permanently and with nothing logged.
//
// 🔴 IT SWEEPS THE WHOLE LIFECYCLE SET RATHER THAN CHECKING Dispatch, because the
// hazard is a call site, not a method. Start has the same branch on the same
// field, and a future method could too; a test naming one method would read as
// coverage of the rule while covering one instance of it.
//
// ⚠ IT IS AN ABSENCE ASSERTION, SO IT NEEDS A POSITIVE CONTROL, and the control
// is in the same test rather than in a comment: each case also asserts the
// transcript is NON-EMPTY and that the adapter recorded the non-delivery. A fake
// wired to nothing would record no calls at all, and "SetKickedOff was not
// called" would pass over it perfectly.
func TestTheAdapterNeverClaimsAKickoffItCannotDeliver(t *testing.T) {
	// 🔴 EVERY CASE RUNS ON BOTH ARMS. The adapter never claims a delivery on
	// EITHER; what differs is whether an owed first turn is RECORDED as undelivered.
	// With no deliverer in the process it must be (the row is the only place that
	// says so). With one, it must NOT be: internal/agentkickoff pays that turn, and
	// a "NOT delivered" written now would be a false record the delivery then has to
	// overwrite. Dispatch(kickoff=true) on the undeliverable arm is the refusal, which
	// writes `error` rather than kickoff_error and is pinned in kickoff_refusal_test.go.
	cases := []struct {
		name string
		// kickedOff is the agent's stored flag before the call.
		kickedOff bool
		// live seeds the backend with the agent's instance, for the verbs that
		// operate on one that already exists.
		live bool
		run  func(*Adapter) error
		// recordedWithoutDeliverer / recordedWithDeliverer: whether this path writes
		// kickoff_error on each arm.
		recordedWithoutDeliverer, recordedWithDeliverer bool
		// refusedWithoutDeliverer: the path returns ErrKickoffUndeliverable there.
		refusedWithoutDeliverer bool
	}{
		{
			name:                    "Dispatch with kickoff",
			run:                     func(a *Adapter) error { return a.Dispatch(context.Background(), fixtureAgentID, true) },
			refusedWithoutDeliverer: true,
		},
		{
			name: "Dispatch without kickoff",
			run:  func(a *Adapter) error { return a.Dispatch(context.Background(), fixtureAgentID, false) },
		},
		{
			name:                     "Start of an agent that never got its note",
			live:                     true,
			run:                      func(a *Adapter) error { return a.Start(context.Background(), fixtureAgentID) },
			recordedWithoutDeliverer: true,
		},
		{
			name:                     "Start of a never-provisioned agent",
			run:                      func(a *Adapter) error { return a.Start(context.Background(), fixtureAgentID) },
			recordedWithoutDeliverer: true,
		},
		{
			name:      "Start of an already-kicked-off agent",
			kickedOff: true,
			live:      true,
			run:       func(a *Adapter) error { return a.Start(context.Background(), fixtureAgentID) },
		},
		{
			name: "Stop",
			live: true,
			run:  func(a *Adapter) error { return a.Stop(context.Background(), fixtureAgentID) },
		},
		{
			name: "Destroy",
			live: true,
			run:  func(a *Adapter) error { return a.Destroy(context.Background(), fixtureAgentID) },
		},
	}

	for _, deliverable := range []bool{true, false} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s/deliverable=%t", tc.name, deliverable), func(t *testing.T) {
				a, store, driver := newAdapterFor(t, deliverable, func(s *recordingStore, _ *flakyDriver) {
					ag := fixtureAgent()
					ag.KickedOff = tc.kickedOff
					s.agent = ag
				})
				if tc.live {
					seedInstance(t, a, store, driver)
				}
				err := tc.run(a)
				if !deliverable && tc.refusedWithoutDeliverer {
					if !errors.Is(err, ErrKickoffUndeliverable) {
						t.Fatalf("%s with no deliverer: want the refusal, got %v", tc.name, err)
					}
				} else if err != nil {
					t.Fatalf("%s: %v", tc.name, err)
				}

				// POSITIVE CONTROL: the adapter must have talked to the store at all.
				if len(store.calls) == 0 {
					t.Fatalf("positive control FAILED: %s recorded ZERO store calls, so the "+
						"absence assertion below would pass over a fake wired to nothing", tc.name)
				}

				if store.called("SetKickedOff") {
					t.Errorf("%s called SetKickedOff.\n"+
						"    This package has NO gateway, so that flag would claim the kickoff "+
						"message was handed to one. agents.DecideReconcile never retries a "+
						"kicked-off agent, so the false flag is permanent: the first turn never "+
						"happens and nothing ever notices. Delivery is internal/agentkickoff's.\n"+
						"  store calls: %s", tc.name, store.transcript())
				}
				// RecordKickoffDelivery stamps WHICH instance received the message. It
				// is the same lie with a witness attached.
				if store.called("RecordKickoffDelivery") {
					t.Errorf("%s called RecordKickoffDelivery, which stamps the instance that "+
						"received a kickoff this package cannot send.\n  store calls: %s",
						tc.name, store.transcript())
				}

				want := tc.recordedWithoutDeliverer
				if deliverable {
					want = tc.recordedWithDeliverer
				}
				if got := store.called("SetKickoffError"); got != want {
					t.Errorf("%s (deliverable=%t) recorded the undelivered kickoff = %t, want %t.\n"+
						"    With no deliverer an owed first turn must be said on the row; with one, "+
						"a NOT-delivered record is false until a delivery actually fails.\n"+
						"  store calls: %s", tc.name, deliverable, got, want, store.transcript())
				}
			})
		}
	}
}

// TestTheRecordedNonDeliveryNamesTheBuildRatherThanTheAgent pins the message a
// human reads off the card.
//
// 🔴 IT PINS THE WHOLE NORMALISED STRING, NOT A KEYWORD. The artifact under test
// is prose, and a guard on words is walkable by rewording — "kickoff failed"
// would satisfy any keyword check while sending the reader to look at the agent's
// pod, its model and its credentials, none of which is why the turn did not run.
// A cosmetic reword therefore fails this test on purpose; that is the price of a
// machine-checkable claim.
func TestTheRecordedNonDeliveryNamesTheBuildRatherThanTheAgent(t *testing.T) {
	const want = "kickoff NOT delivered: this deployment names no agent gateway " +
		"(MUSTER_AGENT_GATEWAY), so nothing can hand the pending note to the instance's model " +
		"gateway. The instance WAS created and the note is still in agents.pending_note. Name a " +
		"gateway, restart muster and Start this agent again: the kickoff deliverer hands the note " +
		"over once the instance is ready. This is a property of the deployment, not a failure of " +
		"this agent: see cmd/muster-server/doc_seams.go entry 1."
	if UndeliveredKickoffReason != want {
		t.Errorf("UndeliveredKickoffReason changed.\n got: %q\nwant: %q", UndeliveredKickoffReason, want)
	}

	// And it must be what actually reaches the store, not just what the constant
	// says — a handler that wrote its own sentence would leave this constant
	// correct and the card wrong. ⚠ It is written by START on a deployment with no
	// gateway: that is the one path that still creates an instance owing a turn
	// nothing can deliver (a Dispatch there is refused; with a gateway, the
	// deliverer pays it and nothing is recorded).
	a, store, _ := newAdapterWithNoKickoffDelivery(t, nil)
	if err := a.Start(context.Background(), fixtureAgentID); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !strings.Contains(store.transcript(), want) {
		t.Errorf("the reason the adapter WROTE is not UndeliveredKickoffReason.\n  store calls: %s",
			store.transcript())
	}
}

// ---------------------------------------------------------------------------
// LIFECYCLE BEHAVIOUR.
// ---------------------------------------------------------------------------

// TestDispatchCreatesTheInstanceAndMintsTheAgentsOwnToken pins the order: the
// token exists before the spec is built, because agentspec.Build puts it in
// Spec.Secrets and an empty one is silently omitted — producing an instance that
// cannot authenticate to this server, with nothing refused anywhere.
func TestDispatchCreatesTheInstanceAndMintsTheAgentsOwnToken(t *testing.T) {
	a, store, driver := newAdapter(t, nil)
	if err := a.Dispatch(context.Background(), fixtureAgentID, true); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	// 64 hex characters from 32 random bytes.
	if !strings.Contains(store.transcript(), "SetHooksToken(4291,len=64)") {
		t.Errorf("Dispatch did not mint a 64-character token for a tokenless agent.\n  store calls: %s",
			store.transcript())
	}
	insts, err := driver.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(insts) != 1 {
		t.Fatalf("driver holds %d instances after one Dispatch, want 1", len(insts))
	}
	// The created instance is keyed on the agent's slug, which is what
	// api.Server.instanceIndex looks it up by.
	//
	// ⚠ THIS ASSERTION PINS agentspec.Build'S REF, NOT agents.RefOf — AND AN EARLIER
	// DRAFT OF THIS COMMENT CLAIMED OTHERWISE. Measured by mutation: changing
	// agents.RefOf to key on the namespace leaves this test GREEN, because Create's ref
	// comes from the spec the builder produced and agents.RefOf is used only by
	// Scale/Destroy/StreamLogs. The claim it used to make is checked by
	// TestTheDriverReferenceIsTheAgentsNameNotItsNamespace instead.
	if insts[0].Ref.Name != fixtureAgentName {
		t.Errorf("instance Ref.Name = %q, want %q (the agent's slug, NOT its namespace %q)",
			insts[0].Ref.Name, fixtureAgentName, agents.NamespaceFor(agents.NamespacePrefix, fixtureAgentName))
	}
	if insts[0].Ref.ID != fixtureAgentID {
		t.Errorf("instance Ref.ID = %d, want %d", insts[0].Ref.ID, fixtureAgentID)
	}

	// An agent that already has a token must not get a second one: re-minting
	// would invalidate the one the running instance is holding.
	a2, store2, _ := newAdapter(t, func(s *recordingStore, _ *flakyDriver) {
		ag := fixtureAgent()
		ag.HooksToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		s.agent = ag
	})
	if err := a2.Dispatch(context.Background(), fixtureAgentID, true); err != nil {
		t.Fatalf("Dispatch (token present): %v", err)
	}
	if store2.called("SetHooksToken") {
		t.Errorf("Dispatch re-minted the token of an agent that already had one, which "+
			"invalidates the credential the running instance holds.\n  store calls: %s",
			store2.transcript())
	}
}

// TestDispatchStatusFollowsWhetherAFirstTurnIsOwed pins the two stored statuses.
func TestDispatchStatusFollowsWhetherAFirstTurnIsOwed(t *testing.T) {
	for _, tc := range []struct {
		kickoff bool
		want    string
	}{
		{kickoff: true, want: agents.StatusProvisioning},
		{kickoff: false, want: agents.StatusStopped},
	} {
		t.Run(fmt.Sprintf("kickoff=%t", tc.kickoff), func(t *testing.T) {
			a, store, _ := newAdapter(t, nil)
			if err := a.Dispatch(context.Background(), fixtureAgentID, tc.kickoff); err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			want := fmt.Sprintf("UpdateStatus(4291,%s,out=%q,err=%q)", tc.want, "", "")
			if !strings.Contains(store.transcript(), want) {
				t.Errorf("Dispatch(kickoff=%t) did not persist %s.\n  want a call: %s\n  store calls: %s",
					tc.kickoff, tc.want, want, store.transcript())
			}
		})
	}
}

// TestStartCreatesOnlyWhenTheBackendSAYSTheInstanceIsAbsent is the sentinel
// discrimination this adapter added over the implementation it replaces.
//
// 🔴 THE TWO ERRORS BEING KEPT APART ARE "THERE IS NOTHING THERE" AND "I CANNOT
// SEE". Upstream re-provisioned on ANY scale failure, so an unreachable backend
// was answered by creating — which, on a backend that was in fact holding a live
// instance, is a second create against a name that already exists. Only
// provision.ErrNotFound is evidence of absence.
func TestStartCreatesOnlyWhenTheBackendSAYSTheInstanceIsAbsent(t *testing.T) {
	cases := []struct {
		name       string
		scaleErr   error
		wantCreate bool
		wantErr    bool
	}{
		{name: "scale succeeds", scaleErr: nil, wantCreate: false},
		{
			name:       "backend says not found",
			scaleErr:   fmt.Errorf("deployment gone: %w", provision.ErrNotFound),
			wantCreate: true,
		},
		{
			name:     "backend unreachable",
			scaleErr: fmt.Errorf("dial tcp: %w", provision.ErrBlind),
			wantErr:  true,
		},
		{
			name:     "ownership refusal",
			scaleErr: fmt.Errorf("someone else's: %w", provision.ErrNotManaged),
			wantErr:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 🔴 EVERY CASE SEEDS A LIVE INSTANCE, AND THAT IS WHAT MAKES THE
			// SCALE ERROR THE ONLY VARIABLE. Without it the unmodified Noop
			// answers ErrNotFound to Scale on its own, so "scale succeeds" and
			// "backend says not found" would run against the same backend state
			// and the injected sentinel would be testing nothing.
			a, store, driver := newAdapter(t, nil)
			seedInstance(t, a, store, driver)
			driver.scaleErr = tc.scaleErr

			err := a.Start(context.Background(), fixtureAgentID)
			if tc.wantErr {
				// 🔴 THE CREATE CHECK RUNS FIRST, AND THE ORDER WAS FOUND BY
				// MUTATION RATHER THAN BY REVIEW. With the nil-error Fatalf above
				// it, a mutant that dropped the sentinel branch entirely — so Start
				// both created AND returned nil — stopped the subtest on "returned
				// nil" and this assertion never executed. The guard was scored as a
				// kill for an assertion that had not run: green for the wrong
				// reason, and still green with this check deleted.
				if driver.creates != 0 {
					t.Errorf("Start created an instance over %v. Only ErrNotFound is evidence "+
						"that the backend holds nothing; this error means the driver could not "+
						"tell, or refused — and creating over a live instance a stranger owns "+
						"is what provision.ErrNotManaged exists to stop.", tc.scaleErr)
				}
				if err == nil {
					t.Errorf("Start returned nil for %v; a scale failure that is not "+
						"ErrNotFound must not be swallowed", tc.scaleErr)
					return
				}
				// The failure must be recorded on the row, or the card stays
				// whatever it was with the reason nowhere.
				if !strings.Contains(store.transcript(), "UpdateStatus(4291,error,") {
					t.Errorf("Start did not record the failure as %s on the row.\n  store calls: %s",
						agents.StatusError, store.transcript())
				}
				return
			}
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			// POSITIVE CONTROL: the scale-up must have been attempted, with one
			// replica, whatever happened next.
			if len(driver.scaled) != 1 || driver.scaled[0] != 1 {
				t.Fatalf("positive control FAILED: Start scaled to %v, want exactly [1]",
					driver.scaled)
			}
			if created := driver.creates > 0; created != tc.wantCreate {
				t.Errorf("after Start with scale error %v the adapter called Create %d time(s) "+
					"(created=%t), want created=%t",
					tc.scaleErr, driver.creates, created, tc.wantCreate)
			}
		})
	}
}

// TestStopScalesToZeroAndDropsTheDeliveryProvenance pins both halves, because the
// second is the one that is invisible when it is missing.
//
// 🔴 agents.Store.ClearKickoffDelivery's OWN DOC IS THE ARGUMENT: a deliberate
// stop means the next start brings up an instance with a new id, which is
// indistinguishable from an eviction. Leaving the old recipient stamped makes
// agents.kickoffLost read the agent as having LOST its message, and a
// stop-then-start of a finished agent re-runs its original task — a paid model
// turn nobody asked for.
func TestStopScalesToZeroAndDropsTheDeliveryProvenance(t *testing.T) {
	a, store, driver := newAdapter(t, nil)
	seedInstance(t, a, store, driver)
	if err := a.Stop(context.Background(), fixtureAgentID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(driver.scaled) != 1 || driver.scaled[0] != 0 {
		t.Errorf("Stop scaled to %v, want exactly [0]", driver.scaled)
	}
	if !store.called("ClearKickoffDelivery") {
		t.Errorf("Stop did not clear the kickoff delivery provenance, so the next Start "+
			"would read the stopped instance's stamp as an eviction and re-send the "+
			"original task.\n  store calls: %s", store.transcript())
	}
	if !strings.Contains(store.transcript(), "UpdateStatus(4291,stopped,") {
		t.Errorf("Stop did not persist %s.\n  store calls: %s", agents.StatusStopped, store.transcript())
	}

	// A scale failure must not be followed by a stopped status: the instance is
	// still running and the row would say otherwise.
	a2, store2, driver2 := newAdapter(t, nil)
	seedInstance(t, a2, store2, driver2)
	driver2.scaleErr = errors.New("apiserver said no")
	if err := a2.Stop(context.Background(), fixtureAgentID); err == nil {
		t.Fatal("Stop returned nil over a failed scale-down")
	}
	if strings.Contains(store2.transcript(), "UpdateStatus(4291,stopped,") {
		t.Errorf("Stop recorded `stopped` after the scale-down FAILED, so the row claims a "+
			"stop that did not happen.\n  store calls: %s", store2.transcript())
	}
}

// TestDestroyKeepsTheRowWhenNothingWasRemoved is where this adapter is stricter
// than the implementation it replaces, and the strictness is the deliverable.
//
// 🔴 DELETING THE ROW OVER A FAILED TEARDOWN LOSES THE ONLY RECORD OF A RUNNING
// INSTANCE. provision.ErrNotManaged is a TERMINAL refusal that removed nothing —
// something under the instance's name is not muster's — and provision.ErrBlind
// means the driver knows nothing at all. In both cases the row is the last thing
// that can list, stop or re-destroy whatever is out there; deleting it makes the
// instance invisible to the very refusal that was protecting it.
func TestDestroyKeepsTheRowWhenNothingWasRemoved(t *testing.T) {
	cases := []struct {
		name       string
		destroyErr error
		wantDelete bool
	}{
		{name: "removed", destroyErr: nil, wantDelete: true},
		{
			name: "already absent",
			// The contract says Destroy returns nil when the instance was already
			// absent; a driver that reports ErrNotFound instead means the same
			// thing, and the backend holding nothing is what the delete needs.
			destroyErr: fmt.Errorf("gone: %w", provision.ErrNotFound),
			wantDelete: true,
		},
		{
			name:       "ownership refusal",
			destroyErr: fmt.Errorf("foreign object: %w", provision.ErrNotManaged),
			wantDelete: false,
		},
		{
			name:       "backend unreachable",
			destroyErr: fmt.Errorf("dial tcp: %w", provision.ErrBlind),
			wantDelete: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, store, driver := newAdapter(t, nil)
			seedInstance(t, a, store, driver)
			driver.destroyErr = tc.destroyErr
			err := a.Destroy(context.Background(), fixtureAgentID)

			// POSITIVE CONTROL: the driver's Destroy must have been attempted at
			// all, or "the row was kept" would be true for the wrong reason.
			if driver.destroys != 1 {
				t.Fatalf("positive control FAILED: the driver's Destroy ran %d times, want 1",
					driver.destroys)
			}

			if got := store.called("Delete"); got != tc.wantDelete {
				t.Errorf("Destroy over %v deleted the row = %t, want %t.\n"+
					"    A row deleted while the instance survives is unlistable, "+
					"unstoppable and invisible to the refusal that protected it.\n"+
					"  store calls: %s", tc.destroyErr, got, tc.wantDelete, store.transcript())
			}
			if tc.wantDelete && err != nil {
				t.Errorf("Destroy: %v", err)
			}
			if !tc.wantDelete {
				if err == nil {
					t.Error("Destroy returned nil while keeping the row, so the caller cannot " +
						"tell that the teardown did not happen")
				} else if !errors.Is(err, tc.destroyErr) {
					t.Errorf("Destroy error does not wrap the driver's: got %v", err)
				}
			}
		})
	}
}

// TestTheDriverReferenceIsTheAgentsNameNotItsNamespace pins agents.RefOf, the one
// place an agent row becomes a driver reference.
//
// 🔴 A NAMESPACE-KEYED REFERENCE DOES NOT FAIL — IT RESOLVES TO NOTHING. Every
// verb below would then operate on an instance that does not exist, and
// api.Server.instanceIndex (which keys on Ref.Name because
// agents.InstanceIndex's own doc says Group is empty for a driver with no
// namespacing concept) would find no live instance for any agent: the whole board
// renders stopped while every pod runs perfectly.
//
// ⚠ IT EXISTS BECAUSE A MUTANT SURVIVED THE TEST THAT CLAIMED TO COVER THIS.
// RefOf is NOT on Create's path — Create's reference comes out of
// agentspec.Build — so the dispatch test's Ref.Name assertion is blind to it.
// The four verbs below are this package's consumers of it, enumerated from its
// call sites rather than from memory, and the fixture's namespace differs from its
// name by construction (agents.NamespacePrefix is non-empty), which is what makes
// the distinction observable at all.
//
// ⚠ THE FUNCTION MOVED TO internal/agents AND THE FOUR VERBS ARE NO LONGER ALL OF
// ITS CONSUMERS. internal/agentgateway resolves a chat endpoint through the same
// mapping, and this test says nothing about that path — the gateway's own
// endpoint-resolution test does. What this test still owns is that THESE verbs
// use it: a namespace-keyed reference here breaks lifecycle whatever chat does.
func TestTheDriverReferenceIsTheAgentsNameNotItsNamespace(t *testing.T) {
	ag := fixtureAgent()
	if ag.Namespace == ag.Name {
		t.Fatalf("instrument check FAILED: the fixture's namespace and name are both %q, so "+
			"keying on either would look identical and every assertion below would pass "+
			"vacuously", ag.Name)
	}

	// The reference itself, directly.
	if got := agents.RefOf(ag); got.Name != ag.Name {
		t.Errorf("agents.RefOf(%q).Name = %q, want the agent's slug %q (NOT its namespace %q)",
			ag.Name, got.Name, ag.Name, ag.Namespace)
	}

	// And behaviourally, per verb: each must reach the instance that was seeded
	// under the agent's NAME. A structural check alone type-checks past a wrong
	// argument, so both halves are here.
	for _, verb := range []struct {
		name string
		run  func(*Adapter) error
	}{
		{"Stop", func(a *Adapter) error { return a.Stop(context.Background(), fixtureAgentID) }},
		{"Destroy", func(a *Adapter) error { return a.Destroy(context.Background(), fixtureAgentID) }},
		{"StreamLogs", func(a *Adapter) error {
			return a.StreamLogs(context.Background(), fixtureAgent(), func(string) {})
		}},
	} {
		t.Run(verb.name, func(t *testing.T) {
			a, store, driver := newAdapter(t, nil)
			seedInstance(t, a, store, driver)
			if err := verb.run(a); err != nil {
				t.Errorf("%s could not reach the instance seeded under the agent's name %q: %v\n"+
					"    If the reference were built from the namespace (%q) this is exactly how "+
					"it would read: not found, on an instance that is running.",
					verb.name, ag.Name, err, ag.Namespace)
			}
		})
	}
}

// TestInstancesPassesTheDriversErrorThrough pins the one place a degraded answer
// would be worse than an error.
//
// 🔴 AN EMPTY SLICE IS A POSITIVE CLAIM THAT NOTHING IS RUNNING.
// api.Server.instanceIndex distinguishes a nil index ("live state was not
// observed" — keep every stored status) from an empty one ("observed, nothing is
// running" — every agent is stopped). An adapter that swallowed the error and
// returned nil, nil would collapse those two and report a whole board stopped on
// one unreachable backend.
func TestInstancesPassesTheDriversErrorThrough(t *testing.T) {
	blind := provision.MustNewNoop(provision.NoopBlind())
	a, err := New(Config{
		Driver: &flakyDriver{Noop: blind},
		Store:  &recordingStore{agent: fixtureAgent()},
		Spec:   fixtureSpecConfig(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	insts, err := a.Instances(context.Background())
	if err == nil {
		t.Fatalf("Instances returned nil error over a blind driver, with %d instance(s). "+
			"That is a positive claim that nothing is running, and every agent on the "+
			"board would render stopped.", len(insts))
	}
	if !errors.Is(err, provision.ErrBlind) {
		t.Errorf("Instances error does not wrap provision.ErrBlind: %v", err)
	}
}

// TestNewRefusesAnAdapterThatCouldOnlyLie pins the two required dependencies.
//
// ⚠ AN ADAPTER OVER A nil DRIVER WOULD ANSWER EVERY LIFECYCLE CALL SUCCESSFULLY
// AND CREATE NOTHING — precisely the observable the provisioner wrappers were
// built to remove, reintroduced one layer down where no wrapper can see it.
func TestNewRefusesAnAdapterThatCouldOnlyLie(t *testing.T) {
	if _, err := New(Config{Store: &recordingStore{}, Spec: fixtureSpecConfig()}); err == nil {
		t.Error("New accepted a nil Driver")
	}
	if _, err := New(Config{Driver: provision.MustNewNoop(), Spec: fixtureSpecConfig()}); err == nil {
		t.Error("New accepted a nil Store")
	}
	a, err := New(Config{Driver: provision.MustNewNoop(), Store: &recordingStore{}, Spec: fixtureSpecConfig()})
	if err != nil {
		t.Fatalf("New rejected a complete config: %v", err)
	}
	if got := a.Driver(); got != "noop" {
		t.Errorf("Driver() = %q, want %q", got, "noop")
	}
	if a.timeout != DefaultOpTimeout {
		t.Errorf("timeout = %v, want the default %v", a.timeout, DefaultOpTimeout)
	}
}

// TestReapplyProfilesReconcilesRatherThanRecreates pins that the optional
// interface's method does the thing its name promises.
//
// 🔴 IT ASSERTS THE DRIVER CALL, NOT THAT THE METHOD RETURNS nil. A body that
// returned nil and did nothing would satisfy the type assertion
// cmd/muster-server/doc_seams.go entry 3 is about, compile, wire, and make every
// model change and every privilege re-apply a silent no-op — which is the "it
// will silently start working, or silently not" that entry names.
func TestReapplyProfilesReconcilesRatherThanRecreates(t *testing.T) {
	a, _, driver := newAdapter(t, nil)
	if err := a.ReapplyProfiles(context.Background(), fixtureAgentID); err != nil {
		t.Fatalf("ReapplyProfiles: %v", err)
	}
	if driver.updates != 1 {
		t.Errorf("ReapplyProfiles called the driver's Update %d times, want 1", driver.updates)
	}
}

// TestTheBuiltSpecCarriesTheWorkerInstructions is the seam between this adapter
// and internal/agentspec's carried-forward debt.
//
// 🔴 THE INSTRUCTION PROSE IS THE ONLY THING THAT TELLS A DISPATCHED INSTANCE THE
// AUTOSAVE RESCUE LOG EXISTS. agentspec's own
// TestTheInstructionsAndTheDaemonReferToEachOther pins the prose against the
// daemon; this pins that the adapter actually PASSES it. Both are needed and
// neither implies the other: the prose can be perfect and never placed.
func TestTheBuiltSpecCarriesTheWorkerInstructions(t *testing.T) {
	a, _, _ := newAdapter(t, nil)
	spec, err := a.buildSpec(context.Background(), fixtureAgent())
	if err != nil {
		t.Fatalf("buildSpec: %v", err)
	}
	var found bool
	for _, f := range spec.Files {
		if strings.HasSuffix(f.Path, agentspec.InstructionsFileName) {
			found = true
			if !strings.Contains(string(f.Content), "autosave") {
				t.Errorf("%s is placed but does not carry the worker instructions",
					agentspec.InstructionsFileName)
			}
		}
	}
	if !found {
		t.Errorf("the built spec places no %s, so a dispatched instance is never told what "+
			"it is for or that the rescue log exists.\n  files: %d",
			agentspec.InstructionsFileName, len(spec.Files))
	}
}
