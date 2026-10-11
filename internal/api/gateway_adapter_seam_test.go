package api

import (
	"testing"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// THE SEAM BETWEEN api.Gateway AND ITS FIRST REAL IMPLEMENTATION.
//
// 🔴 IT IS THE CHAT TWIN OF lifecycle_adapter_seam_test.go, AND IT EXISTS FOR THE
// SAME REASON AT THE OPPOSITE END. That file asserts the lifecycle adapter
// satisfies api.Provisioner and must NOT satisfy api.Gateway. This one asserts the
// gateway satisfies api.Gateway and must NOT satisfy api.Provisioner — because the
// hazard is symmetric and the shape of the mistake is identical: a type that
// satisfies both is a type that can be wired into either field, and the split's
// entire purpose was that a deployment can turn one tier on while the other keeps
// refusing honestly.
//
// 🔴 WHAT WOULD GO WRONG IF *Gateway SATISFIED api.Provisioner. Nine lifecycle
// routes stop refusing at api.requireLifecycleProvisioner. A dispatch would then
// answer 200, create nothing, and leave a row in `provisioning` for ever — which is
// the exact observable cmd/muster-server/doc_seams.go entry 1 was written about, and
// the one a satisfied interface reports in silence.
// ---------------------------------------------------------------------------

// TestTheChatGatewaySatisfiesTheConsumerInterface is the seam check.
func TestTheChatGatewaySatisfiesTheConsumerInterface(t *testing.T) {
	gw, err := agentgateway.New(agentgateway.Config{
		Driver:  provision.MustNewNoop(),
		Runtime: agentgateway.HooksSHA256(),
		Model:   "runtime-sentinel",
	})
	if err != nil {
		t.Fatalf("building the gateway: %v", err)
	}

	// The assignment IS the assertion for the chat half: a method-set drift on
	// either side is a compile error here.
	var _ Gateway = gw

	if _, ok := any(gw).(Provisioner); ok {
		t.Error("*agentgateway.Gateway satisfies api.Provisioner, which it must not.\n" +
			"    Provisioner is the LIFECYCLE half. A gateway holds an endpoint resolver — " +
			"deliberately narrower than the driver contract, so a chat path cannot destroy " +
			"the instance it is talking to — and satisfying this interface is what stops " +
			"api.requireLifecycleProvisioner refusing. Nine routes would answer 200 over a " +
			"provisioner that creates nothing.\n" +
			"    If lifecycle is being wired, wire agentprovision.Adapter into " +
			"Extensions.Provisioner as its own value; do not widen this type.")
	}

	// ⚠ ProfileReapplier IS ASSERTED ABSENT, AND IT IS NOT THE SAME KIND OF CLAIM AS
	// THE ONE ABOVE. It is an OPTIONAL interface: the model-change path
	// type-asserts the *Provisioner* for it, so a Gateway that happened to satisfy it would never be
	// asked. This pins that the method is not drifting onto the wrong type — the
	// mistake being guarded is a reapply implemented here, where the field that gets
	// type-asserted can never reach it, which reads as done and is inert.
	if _, ok := any(gw).(ProfileReapplier); ok {
		t.Error("*agentgateway.Gateway satisfies api.ProfileReapplier.\n" +
			"    Only Extensions.Provisioner is type-asserted for it (the model-change " +
			"roll in agents.go), so an implementation on the gateway is unreachable and " +
			"a reapply would silently never happen.")
	}
}

// TestAGatewayWithoutADriverIsRefusedAtConstruction pins that the gateway cannot be
// built in the state whose failure would surface inside a request handler.
//
// 🔴 THE TWO REFUSALS ARE CONSTRUCTION-TIME BECAUSE THEIR ALTERNATIVE IS A 500 PER
// TURN WITH NO BOOT-TIME TRACE. A gateway with no resolver reaches no agent; a
// gateway with no runtime has no bearer derivation and no model sentinel, so every
// turn comes back 401 or 400 from the runtime — failures that read as a bad
// credential or a bad model rather than as unwired configuration.
func TestAGatewayWithoutADriverIsRefusedAtConstruction(t *testing.T) {
	if _, err := agentgateway.New(agentgateway.Config{Runtime: agentgateway.HooksSHA256(), Model: "runtime-sentinel"}); err == nil {
		t.Error("agentgateway.New accepted a nil Driver, so the gateway would fail to resolve " +
			"any address from inside a chat handler rather than at boot")
	}
	if _, err := agentgateway.New(agentgateway.Config{Driver: provision.MustNewNoop(), Model: "runtime-sentinel"}); err == nil {
		t.Error("agentgateway.New accepted a nil Runtime. There is no neutral default for the " +
			"bearer derivation: a guessed one answers 401 on every turn, which reads as a rotated " +
			"credential.")
	}
	// 🔴 THE THIRD REQUIRED FIELD, AND IT WAS MISSING FROM THIS TEST — found by
	// mutation: deleting the `Config.Model == ""` refusal SURVIVED the whole suite,
	// because every other test supplies a sentinel. An empty one is not a missing
	// credential, it is a REQUEST the runtime rejects: both endpoints require the
	// wire's `model` field, so the failure is an HTTP 400 per turn from inside a
	// handler.
	if _, err := agentgateway.New(agentgateway.Config{
		Driver:  provision.MustNewNoop(),
		Runtime: agentgateway.HooksSHA256(),
	}); err == nil {
		t.Error("agentgateway.New accepted an empty Model. The runtime's passthrough sentinel is " +
			"REQUIRED on the wire by both endpoints, so this builds a gateway whose every turn " +
			"is a 400 naming the model field — which reads as a model misconfiguration rather " +
			"than as an unset variable.")
	}
}
