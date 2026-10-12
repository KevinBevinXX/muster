package main

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agentprovision"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// THE SEAM BETWEEN THE GATEWAY THIS BINARY RESOLVES AND THE REFUSAL THE ADAPTER
// PERFORMS.
//
// 🔴 BOTH SIDES OF THIS SEAM ARE ALREADY TESTED IN ISOLATION AND THAT IS EXACTLY
// WHY IT NEEDS A TEST. internal/agentprovision pins the refusal against a bool it
// is handed; gateway_test.go pins that naming a runtime produces a gateway and not
// naming one produces nil. Neither builds the combined state, so the wire between
// them — buildAgentPlane passing `gw != nil` into the adapter — is owned by no
// test at all. A plane that always passed true, or always false, satisfies every
// existing assertion in both packages.
//
// ⚠ THE ASSERTIONS ARE BEHAVIOURAL, NOT STRUCTURAL. Config.KickoffDeliverable is
// unexported and reading it by reflection would type-check past a wiring that set
// it from the wrong thing. These tests DISPATCH through the constructed adapter and
// read what happened to the store, which is the only claim that cannot be satisfied
// by a correctly-shaped mistake.
// ---------------------------------------------------------------------------

// dispatchRecorder is an agents.Store that answers the two reads the lifecycle
// adapter makes on a dispatch and records every write.
//
// ⚠ IT EMBEDS agents.Store, so any method NOT implemented here is a nil-interface
// panic rather than a zero value — the same choice stubStore makes, for the same
// reason: a dispatch that started calling something new must fail loudly instead of
// passing over a stub.
type dispatchRecorder struct {
	agents.Store

	mu    sync.Mutex
	agent agents.Agent
	calls []string
	// listed, when non-nil, receives once per List call.
	listed chan struct{}
}

func (s *dispatchRecorder) record(f string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, f)
}

func (s *dispatchRecorder) transcript() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.calls, " -> ")
}

// List answers the kickoff deliverer's tick. It returns NO rows — the wiring test
// only needs to observe that a tick happened — and signals listed when set.
func (s *dispatchRecorder) List(context.Context) ([]agents.Agent, error) {
	s.record("List")
	if s.listed != nil {
		select {
		case s.listed <- struct{}{}:
		default:
		}
	}
	return nil, nil
}

func (s *dispatchRecorder) Get(context.Context, int64) (agents.Agent, error) {
	s.record("Get")
	return s.agent, nil
}

func (s *dispatchRecorder) UpdateStatus(_ context.Context, _ int64, status, _, errMsg string) error {
	s.record("UpdateStatus(" + status + "):" + errMsg)
	return nil
}

func (s *dispatchRecorder) SetHooksToken(_ context.Context, _ int64, _ string) error {
	s.record("SetHooksToken")
	return nil
}

func (s *dispatchRecorder) SetKickoffError(_ context.Context, _ int64, _ string) error {
	s.record("SetKickoffError")
	return nil
}

func newDispatchRecorder() *dispatchRecorder {
	return &dispatchRecorder{agent: agents.Agent{
		ID:          7712,
		Name:        "wiring-kestrel",
		Namespace:   agents.NamespaceFor(agents.NamespacePrefix, "wiring-kestrel"),
		PendingNote: "read the plan and report what is missing",
		Status:      agents.StatusProvisioning,
	}}
}

// TestADispatchIsRefusedExactlyWhenNoGatewayIsNamed is the seam guard.
//
// 🔴 ITS SECOND HALF HAS FLIPPED TWICE, AND BOTH FLIPS WERE THE POINT. It first
// asserted a gatewayed dispatch WENT THROUGH, which was measured wrong on the live
// deployment: nothing called the gateway, so the dispatch created a pod nobody ever
// told what to do. It then asserted the dispatch was REFUSED even with a gateway,
// and named itself TestEveryDeploymentRefusesAKickoffWhileNothingCanDeliverOne.
// internal/agentkickoff is the deliverer that was missing, KickoffDeliveryWired is
// true, and the gatewayed dispatch is ACCEPTED again — but now the acceptance is
// only half of what this half asserts: the same buildAgentPlane output must also
// yield a deliverer (buildKickoffDeliverer), or "accepted" is the old defect.
func TestADispatchIsRefusedExactlyWhenNoGatewayIsNamed(t *testing.T) {
	logger := log.New(&strings.Builder{}, "", 0)

	// MUSTER_AGENT_GATEWAY unset — still refused.
	t.Run("no gateway named", func(t *testing.T) {
		store := newDispatchRecorder()
		prov, gw, err := buildAgentPlane(provisionerTestConfig(provisionerNoop), store, logger)
		if err != nil {
			t.Fatalf("buildAgentPlane(noop, no gateway): %v", err)
		}
		// INSTRUMENT CHECK: this half is only meaningful if the gateway really is nil.
		if gw != nil {
			t.Fatalf("instrument check FAILED: a gateway was built for a config naming none, "+
				"so this case is not the no-gateway deployment: %T", gw)
		}
		if prov == nil {
			t.Fatal("instrument check FAILED: no lifecycle adapter was built, so nothing " +
				"below is exercised")
		}

		err = prov.Dispatch(context.Background(), store.agent.ID, true)
		if err == nil {
			t.Fatalf("a kickoff dispatch was ACCEPTED on a deployment with no gateway, so the "+
				"adapter was told a kickoff is deliverable when buildGateway returned nil.\n"+
				"  transcript: %s", store.transcript())
		}
		if !errors.Is(err, agentprovision.ErrKickoffUndeliverable) {
			t.Errorf("the dispatch failed for some reason OTHER than the undeliverable-kickoff "+
				"refusal, so this test is green for the wrong cause: %v", err)
		}
		if store.called("SetHooksToken") {
			t.Errorf("the refused dispatch still minted a token, so it reached the create "+
				"path.\n  transcript: %s", store.transcript())
		}
		if !strings.Contains(store.transcript(), "UpdateStatus("+agents.StatusError+")") {
			t.Errorf("the refusal was not recorded on the row as %s.\n  transcript: %s",
				agents.StatusError, store.transcript())
		}
		// The recorded reason must name the variable THIS BINARY reads.
		if !strings.Contains(store.transcript(), envAgentGateway) {
			t.Errorf("the recorded refusal does not name %s, so the operator is told "+
				"something was refused and not what to set.\n  transcript: %s",
				envAgentGateway, store.transcript())
		}
	})

	// A gateway named: accepted, instance created, NO "not delivered" record, and the
	// deliverer that will pay the first turn is built from the same plane.
	t.Run("a gateway is named: accepted, and a deliverer exists to pay it", func(t *testing.T) {
		store := newDispatchRecorder()
		cfg := gatewayTestConfig(provisionerNoop, gatewayHooksSHA256)
		prov, gw, err := buildAgentPlane(cfg, store, logger)
		if err != nil {
			t.Fatalf("buildAgentPlane(noop, %s): %v", gatewayHooksSHA256, err)
		}
		if gw == nil || prov == nil {
			t.Fatalf("instrument check FAILED: gateway=%v adapter=%v, so this is not the "+
				"gateway-configured deployment it claims to be", gw != nil, prov != nil)
		}
		if err := prov.Dispatch(context.Background(), store.agent.ID, true); err != nil {
			t.Fatalf("a kickoff dispatch was REFUSED on a deployment that names a gateway and "+
				"has a deliverer: %v\n  transcript: %s", err, store.transcript())
		}
		if !store.called("SetHooksToken") || !strings.Contains(store.transcript(), "UpdateStatus("+agents.StatusProvisioning+")") {
			t.Errorf("the accepted dispatch did not create the instance and store %s.\n  transcript: %s",
				agents.StatusProvisioning, store.transcript())
		}
		if store.called("SetKickoffError") {
			t.Errorf("an accepted dispatch on a delivering deployment wrote a NOT-delivered "+
				"record before any delivery was attempted.\n  transcript: %s", store.transcript())
		}
		d, err := buildKickoffDeliverer(cfg, store, prov, gw, nil, logger)
		if err != nil || d == nil {
			t.Fatalf("the plane accepted a kickoff dispatch and built NO deliverer (err %v): "+
				"that is the pod-nobody-told-what-to-do defect, one function over", err)
		}
	})
}

// TestADeliverableAdapterAlwaysComesWithARunningDeliverer holds "a dispatch is
// accepted" and "something delivers it" together, in BOTH configurations, and then
// proves startBackgroundLoops actually RUNS the deliverer it was handed.
//
// 🔴 THE MODULEGATE LEDGER CANNOT SEE THIS. It counts CALLS to the delivery writes
// anywhere in the module, so a deliverer that is built and never started — or never
// built — satisfies it while every Dispatch is accepted and nothing is delivered.
func TestADeliverableAdapterAlwaysComesWithARunningDeliverer(t *testing.T) {
	logger := log.New(&strings.Builder{}, "", 0)
	for _, c := range []struct {
		label string
		cfg   config
	}{
		{"no gateway", provisionerTestConfig(provisionerNoop)},
		{"a gateway", gatewayTestConfig(provisionerNoop, gatewayHooksSHA256)},
	} {
		store := newDispatchRecorder()
		prov, gw, err := buildAgentPlane(c.cfg, store, logger)
		if err != nil || prov == nil {
			t.Fatalf("%s: buildAgentPlane: adapter=%v err=%v", c.label, prov != nil, err)
		}
		d, err := buildKickoffDeliverer(c.cfg, store, prov, gw, nil, logger)
		if err != nil {
			t.Fatalf("%s: buildKickoffDeliverer: %v", c.label, err)
		}
		accepts := prov.KickoffUndeliverableReason() == ""
		if accepts != (d != nil) {
			t.Errorf("%s: the adapter ACCEPTS a kickoff dispatch = %t but a deliverer was built = %t. "+
				"Accepted-with-no-deliverer creates pods nobody tells what to do; "+
				"refused-with-a-deliverer refuses work this process could do.",
				c.label, accepts, d != nil)
		}
	}

	// startBackgroundLoops runs it: the deliverer's first tick reads the agents list.
	store := newDispatchRecorder()
	store.listed = make(chan struct{}, 1)
	cfg := gatewayTestConfig(provisionerNoop, gatewayHooksSHA256)
	prov, gw, err := buildAgentPlane(cfg, store, logger)
	if err != nil {
		t.Fatalf("buildAgentPlane: %v", err)
	}
	d, err := buildKickoffDeliverer(cfg, store, prov, gw, nil, logger)
	if err != nil || d == nil {
		t.Fatalf("instrument check FAILED: no deliverer (err %v)", err)
	}
	a := &app{cfg: cfg, logger: logger, kickoff: d}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.startBackgroundLoops(ctx)
	select {
	case <-store.listed:
	case <-time.After(5 * time.Second):
		t.Fatal("startBackgroundLoops was handed a kickoff deliverer and it never ticked: " +
			"every dispatch would be accepted and none delivered")
	}
}

// TestTheDeliverabilityPredicateNeedsBOTHConjuncts drives every row of
// kickoffDeliverable.
//
// 🔴 IT PINS THE REQUIREMENT INDEPENDENTLY OF THE CONSTANT'S CURRENT VALUE. While
// agentprovision.KickoffDeliveryWired was false three of these rows were unreachable
// through buildAgentPlane; it is true now, so the two gateway rows are reachable
// (TestADispatchIsRefusedExactlyWhenNoGatewayIsNamed) and the two call-site-less
// rows are the ones only this table can reach.
//
// ⚠ WHAT IT DOES *NOT* PIN: that the call site passes the real constant rather than
// a literal. That is unobservable from here by construction — the argument is a
// bool — and it is named in kickoffDeliverable's own doc instead.
func TestTheDeliverabilityPredicateNeedsBOTHConjuncts(t *testing.T) {
	driver, err := provision.NewNoop()
	if err != nil {
		t.Fatalf("building the recording driver for the fixture: %v", err)
	}
	built, err := agentgateway.New(agentgateway.Config{
		Driver:  driver,
		Runtime: agentgateway.HooksSHA256(),
		Model:   "fixture-model",
	})
	if err != nil {
		t.Fatalf("building a gateway for the fixture: %v", err)
	}
	// INSTRUMENT CHECK: a nil "built" gateway would make the two gateway rows
	// indistinguishable from the two nil ones, and every row would pass for the
	// wrong reason.
	if built == nil {
		t.Fatal("instrument check FAILED: agentgateway.New returned nil with no error, so " +
			"the rows below that claim to hold a gateway do not")
	}

	for _, c := range []struct {
		label string
		gw    *agentgateway.Gateway
		wired bool
		want  bool
	}{
		{label: "no gateway, no call site", gw: nil, wired: false, want: false},
		{label: "no gateway, a call site", gw: nil, wired: true, want: false},
		{label: "a gateway, no call site — the shape that shipped the defect", gw: built, wired: false, want: false},
		{label: "a gateway AND a call site", gw: built, wired: true, want: true},
	} {
		if got := kickoffDeliverable(c.gw, c.wired); got != c.want {
			t.Errorf("kickoffDeliverable(gw!=nil=%t, wired=%t) = %t, want %t — %s\n"+
				"    Delivering a kickoff needs a gateway to deliver it THROUGH and a call "+
				"site to deliver it FROM. A predicate that drops either conjunct is wrong in "+
				"one of two directions: dropping the gateway tells a gatewayless deployment "+
				"it can deliver, and dropping the call site is the shipped defect — a "+
				"configured gateway read as a delivered first turn.",
				c.gw != nil, c.wired, got, c.want, c.label)
		}
	}
}

func (s *dispatchRecorder) called(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if strings.HasPrefix(c, name) {
			return true
		}
	}
	return false
}

// TestTheKickoffRefusalNamesTheBinarysOwnGatewayVariable pins the one duplicated
// string in this change.
//
// 🔴 internal/agentprovision CANNOT IMPORT package main, so the environment
// variable's name is spelled as a literal there and as envAgentGateway here. A
// duplicated name is a name that goes stale in one place, and the stale half would
// be the one an operator reads: a refusal naming a variable that no longer exists
// is worse than a refusal naming none, because it sends them to set something with
// no effect and conclude the feature is broken.
//
// ⚠ IT ASSERTS THE REMEDY'S *VALUE* TOO, for the same reason and with the same
// mechanism: agentgateway.SchemeHooksSHA256 is what config accepts, and a refusal
// telling the operator to set a value validateProvisioner rejects at boot would
// turn one refusal into two.
func TestTheKickoffRefusalNamesTheBinarysOwnGatewayVariable(t *testing.T) {
	for _, want := range []string{envAgentGateway, envAgentGatewayModel, gatewayHooksSHA256} {
		if !strings.Contains(agentprovision.KickoffRefusalReason, want) {
			t.Errorf("agentprovision.KickoffRefusalReason does not contain %q.\n"+
				"    Either this binary's constant was renamed and the refusal text was not, "+
				"or the refusal never named it. Fix the TEXT, not this test — the text is "+
				"what the operator reads.\n  reason: %s", want, agentprovision.KickoffRefusalReason)
		}
	}

	// CONTROL: the assertion above must be able to fail. A value this binary does NOT
	// recognise must be absent, or the test would pass over a reason string that
	// happened to contain every token anyone looked for.
	if strings.Contains(agentprovision.KickoffRefusalReason, "MUSTER_AGENT_GATEWAY_RUNTIME") {
		t.Error("the refusal names MUSTER_AGENT_GATEWAY_RUNTIME, which this binary does not " +
			"read — the operator would set a variable with no effect")
	}
}
