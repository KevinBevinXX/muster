package api

import (
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/ZacxDev/muster/internal/agentprovision"
	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// THE SEAM BETWEEN THE CONSUMER INTERFACE AND ITS FIRST REAL IMPLEMENTATION.
//
// 🔴 IT IS TESTED FROM *THIS* SIDE, WHICH IS THE SIDE THAT OWNS THE INTERFACE.
// internal/agentprovision deliberately does NOT name api.Provisioner in a
// compile-time assertion: this package declares the interface consumer-side
// precisely so the adapter's package does not import it, and an assertion over
// there would create the dependency the split exists to avoid. So the check lives
// here, where the import direction is already the right way round.
//
// 🔴 THIS IS AN "ISOLATION SEAM" CHECK AND THAT IS THE WHOLE REASON IT EXISTS.
// Both sides are separately tested to death — the adapter's package has a
// mutation-swept suite, this package's wrappers have a three-shape matrix — and
// neither suite ever builds the COMBINED state. The two things that can only be
// wrong together are the method set (a drift makes the wiring not compile, which
// only a test that performs the assignment can see) and the OPTIONAL type
// assertion, which leaves no trace at all when it fails.
// ---------------------------------------------------------------------------

// TestTheLifecycleAdapterSatisfiesTheConsumerInterface is the seam check.
//
// ⚠ IT ALSO ASSERTS WHAT THE ADAPTER MUST *NOT* SATISFY. api.Gateway is the chat
// half, and the whole argument for splitting the two interfaces was that a
// lifecycle adapter can implement the seven honestly and the two not at all. If
// this adapter ever satisfied Gateway, the chat routes would stop refusing at
// api.requireGatewayProvisioner and start answering 200 over a gateway that does
// not exist — which is the observable cmd/muster-server/doc_seams.go entry 1
// rejects, and nothing else would notice, because a satisfied interface is
// silent.
func TestTheLifecycleAdapterSatisfiesTheConsumerInterface(t *testing.T) {
	adapter, err := agentprovision.New(agentprovision.Config{
		Driver: provision.MustNewNoop(),
		Store:  stubAgentsStore{},
	})
	if err != nil {
		t.Fatalf("building the adapter: %v", err)
	}

	// The assignment IS the assertion for the lifecycle half: a method-set drift
	// on either side is a compile error here.
	var _ Provisioner = adapter

	// 🔴 THE OPTIONAL ONE IS A TYPE ASSERTION, AND doc_seams.go ENTRY 3 IS ABOUT
	// EXACTLY THIS. The model-change path type-asserts the Provisioner for
	// ProfileReapplier; an adapter without the method compiles,
	// wires, and takes the else branch for ever with no nil anywhere for a reader
	// to notice. "It compiles" is not this test — a method added to the wrong type
	// compiles just as well — so the assertion is performed.
	if _, ok := any(adapter).(ProfileReapplier); !ok {
		t.Errorf("*agentprovision.Adapter does NOT satisfy api.ProfileReapplier.\n" +
			"    The model-change roll (agents.go) type-asserts for it, and a failed " +
			"assertion is SILENT: it becomes a no-op, so a model change never reaches " +
			"the running instance and nothing anywhere says so.\n" +
			"    If dropping it is deliberate, invert this assertion and say why here; do " +
			"not delete it. See cmd/muster-server/doc_seams.go entry 3.")
	}

	if _, ok := any(adapter).(Gateway); ok {
		t.Error("*agentprovision.Adapter satisfies api.Gateway, which it must not.\n" +
			"    Gateway is the CHAT half. The adapter is built over " +
			"provision.Provisioner, a lifecycle contract with no notion of a chat turn, " +
			"so it cannot honestly answer one — and satisfying the interface is what " +
			"stops api.requireGatewayProvisioner refusing. The two chat routes would " +
			"answer 200 over a gateway that does not exist.\n" +
			"    If a real gateway is being added, wire it into Extensions.Gateway as its " +
			"own value; do not widen this type.")
	}
}

// TestAWiredProvisionerIsReadyWithoutAnyPrivilegeWiring: the deployed shape —
// a real lifecycle adapter, nothing privilege-related — is READY.
//
// ⚠ IT REPLACES A TEST THAT ASSERTED THE OPPOSITE FOR ONE COMBINATION. Until the
// privilege subsystem was removed, a wired Provisioner beside a wired privilege
// store with no applier was a readiness defect (doc_seams.go entry 2). That
// combination is no longer expressible, so what remains to pin is that removing
// the applier did not leave a provisioner-wired server unready — which is the
// state production deploys into the moment MUSTER_AGENT_PRIVILEGE_APPLY stops
// being read.
func TestAWiredProvisionerIsReadyWithoutAnyPrivilegeWiring(t *testing.T) {
	adapter, err := agentprovision.New(agentprovision.Config{
		Driver: provision.MustNewNoop(),
		Store:  stubAgentsStore{},
	})
	if err != nil {
		t.Fatalf("building the adapter: %v", err)
	}

	cases := []struct {
		name      string
		ext       Extensions
		wantReady bool
	}{
		{name: "provisioner wired", ext: Extensions{Provisioner: adapter}, wantReady: true},
		{name: "neither wired", ext: Extensions{}, wantReady: true},
		{
			// POSITIVE CONTROL: the harness CAN observe not-ready. A notes store
			// with no liveness probe is a standing defect.
			name: "control: notes without liveness", ext: Extensions{Provisioner: adapter, Notes: stubNotesStore{}},
			wantReady: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(nil, AuthConfig{}, log.New(os.Stderr, "", 0))
			s.UseExtensions(tc.ext)
			rec := httptest.NewRecorder()
			s.handleReady(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			gotReady := rec.Code == http.StatusOK
			if gotReady != tc.wantReady {
				t.Fatalf("/readyz answered %d (ready=%v), want ready=%v.\nbody: %s",
					rec.Code, gotReady, tc.wantReady, rec.Body.String())
			}
		})
	}
}
