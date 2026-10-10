// Package provisiontest is the CONTRACT SUITE every muster provisioner driver
// must pass.
//
// 🔴 THE SUITE IS THE DELIVERABLE, NOT THE DRIVERS. An interface is a promise
// about behaviour that its method signatures cannot express: that Create is
// idempotent, that a blind driver errors instead of reporting emptiness, that
// Destroy's nil means something. Per-driver tests written by whoever wrote each
// driver assert what that author believed; this suite asserts what every CALLER
// is entitled to assume, and it is the reason a second driver can be dropped in
// without auditing the call sites.
//
// 🔴 IT DOES NOT SKIP. A [Harness] missing any hook fails immediately rather
// than quietly running a subset — a suite that skips is indistinguishable in
// the output from one that passed, and the whole point of this package is that
// its verdict means something.
//
// Add a case here rather than in a driver's own tests whenever the thing you
// are pinning is true of the CONTRACT. A case that only makes sense for one
// backend belongs in that backend's package.
package provisiontest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/provision"
)

// Harness is what a driver supplies to be tested.
//
// Name, New, Blind and Restricted are REQUIRED; GrantablePolicy is required
// only when the driver from New declares Capabilities.Policy, and its own
// comment says so. This said "all four fields are REQUIRED" while the struct
// had five, which read as a stricter contract than RunContract enforces — a
// driver author supplying a policy payload their driver cannot use would have
// been following the doc.
//
// In particular Blind and Restricted are not optional conveniences: they are
// the only way to exercise the two rules that matter most, and a driver that
// cannot produce them has not thought about either failure.
type Harness struct {
	// Name identifies the driver in subtest names.
	Name string

	// New returns a working driver with an empty backend. Called once per
	// subtest, so cases cannot contaminate each other.
	New func(t *testing.T) provision.Provisioner

	// Blind returns a driver whose backend CANNOT BE REACHED — a dropped
	// connection, a rejecting API, a fake with no store. It is how the
	// error-never-empty rule is tested on the driver rather than asserted about
	// it.
	Blind func(t *testing.T) provision.Provisioner

	// Restricted returns the driver configured with as few capabilities as it
	// can genuinely have. It is how CheckSpec's refusals are tested against a
	// driver that really refuses, rather than against a hand-built mock that
	// agrees with the test.
	Restricted func(t *testing.T) provision.Provisioner

	// GrantablePolicy is a policy the driver from New CAN apply. REQUIRED when
	// that driver declares Capabilities.Policy, and unused otherwise.
	//
	// ⚠ IT EXISTS BECAUSE A POLICY'S RULES ARE DRIVER-SPECIFIC BY DESIGN. A
	// single payload written into this suite would be uninterpretable to every
	// driver but one, so the grant-succeeds direction would be untestable and
	// the case would silently collapse to refusal-only — which passes for a
	// driver that refuses everything.
	GrantablePolicy provision.Policy
}

// MinimalSpec is a spec every driver must accept whatever its capabilities:
// a runtime and a port, and nothing that any capability gates.
//
// ⚠ IT IS DELIBERATELY THE FLOOR, NOT A REALISTIC SPEC. Cases that need a
// capability-gated field add it themselves, so a failure names the field that
// caused it instead of "the sample spec was rejected".
func MinimalSpec(name string) provision.Spec {
	return provision.Spec{
		Ref:     provision.Ref{Name: name},
		Runtime: provision.Runtime{Image: "ghcr.io/muster-example/agent:1"},
		Env:     []provision.EnvVar{{Name: "MUSTER_INSTANCE", Value: name}},
		Ports:   []provision.Port{{Name: provision.DefaultPortName, Port: 8421}},
	}
}

// RunContract runs every contract case against h.
func RunContract(t *testing.T, h Harness) {
	t.Helper()
	if h.Name == "" {
		t.Fatal("provisiontest: Harness.Name is empty; the subtests would be unattributable")
	}
	if h.New == nil || h.Blind == nil || h.Restricted == nil {
		t.Fatalf("provisiontest: Harness %q must supply New, Blind and Restricted. "+
			"None of them is optional: Blind is the only way to test error-never-empty, "+
			"and Restricted is the only way to test that the capability refusals fire. "+
			"A harness that omitted them would run a subset and report the same green.", h.Name)
	}

	cases := []struct {
		name string
		fn   func(*testing.T, Harness)
	}{
		{"DriverIdentifiesItself", testDriverIdentifiesItself},
		{"CreateIsIdempotent", testCreateIsIdempotent},
		{"CreateRefusesADivergentSpec", testCreateRefusesADivergentSpec},
		{"CreateWorksWithoutACorrelationID", testCreateWorksWithoutACorrelationID},
		{"GetOfAMissingInstanceIsNotFound", testGetMissingIsNotFound},
		{"ListReportsWhatWasCreated", testListReportsWhatWasCreated},
		{"UpdateCreatesWhenAbsent", testUpdateCreatesWhenAbsent},
		{"UpdateThenCreateIsNotDivergent", testUpdateThenCreateIsNotDivergent},
		{"ScaleToZeroIsStoppedNotGone", testScaleToZeroIsStoppedNotGone},
		{"ScaleOfAMissingInstanceIsNotFound", testScaleMissingIsNotFound},
		{"DestroyRemovesTheInstance", testDestroyRemovesTheInstance},
		{"DestroyOfAnAbsentInstanceIsNil", testDestroyAbsentIsNil},
		{"EndpointHonoursThePerInstanceOverride", testEndpointOverride},
		{"EndpointOfAMissingInstanceIsNotFound", testEndpointMissingIsNotFound},
		{"LogsOfAMissingInstanceAreNotFound", testLogsMissingIsNotFound},
		{"BlindListErrorsAndReturnsNothing", testBlindListErrors},
		{"BlindGetIsBlindNotNotFound", testBlindGetIsNotNotFound},
		{"BlindDestroyDoesNotClaimSuccess", testBlindDestroyErrors},
		{"RestrictedDriverRefusesWhatItCannotDo", testRestrictedRefuses},
		{"PolicyIsRefusedRatherThanIgnored", testPolicyRefusedOrApplied},
		{"ExecIsRefusedWhenNotDeclared", testExecRefusedWhenNotDeclared},
	}
	for _, c := range cases {
		t.Run(h.Name+"/"+c.name, func(t *testing.T) { c.fn(t, h) })
	}
}

func testDriverIdentifiesItself(t *testing.T, h Harness) {
	p := h.New(t)
	if strings.TrimSpace(p.Driver()) == "" {
		t.Fatal("Driver() is empty; Instance.Driver and every refusal message would name nothing")
	}
}

func testCreateIsIdempotent(t *testing.T, h Harness) {
	p := h.New(t)
	ctx := context.Background()
	spec := MinimalSpec("idem")

	if err := p.Create(ctx, spec); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if err := p.Create(ctx, spec); err != nil {
		t.Fatalf("second Create with an IDENTICAL spec must succeed (idempotence), got: %v", err)
	}
	list, err := mustList(t, p)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("two identical Creates produced %d instances, want 1: %+v", len(list), list)
	}
}

func testCreateRefusesADivergentSpec(t *testing.T, h Harness) {
	p := h.New(t)
	ctx := context.Background()
	spec := MinimalSpec("diverge")
	if err := p.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 🔴 THE FIXTURE VALUES ARE PAIRWISE DISTINCT AND DISTINCT FROM THE
	// ORIGINAL'S, so a driver that compares the wrong field cannot pass by
	// comparing two values that happen to be equal.
	changed := spec
	changed.Runtime = provision.Runtime{Image: "ghcr.io/muster-example/other:2"}

	err := p.Create(ctx, changed)
	if !errors.Is(err, provision.ErrDivergentSpec) {
		t.Fatalf("Create with a DIFFERENT spec must return ErrDivergentSpec rather than "+
			"silently overwrite (the caller that wanted the overwrite has Update); got %v", err)
	}

	// And it must have changed nothing: a refusal that half-applied is worse
	// than an overwrite, because nobody knows which half.
	inst, err := p.Get(ctx, spec.Ref)
	if err != nil {
		t.Fatalf("Get after a refused Create: %v", err)
	}
	if inst.Ref.Name != spec.Ref.Name {
		t.Fatalf("Get returned %q, want %q", inst.Ref.Name, spec.Ref.Name)
	}
}

func testCreateWorksWithoutACorrelationID(t *testing.T, h Harness) {
	p := h.New(t)
	ctx := context.Background()
	spec := MinimalSpec("noid")
	if spec.Ref.ID != 0 {
		t.Fatal("MinimalSpec must leave Ref.ID zero; this case exists to prove drivers do not need one")
	}
	if err := p.Create(ctx, spec); err != nil {
		t.Fatalf("Create with Ref.ID == 0 must work — a driver that needs an id needs somebody's "+
			"database schema, which is the defect Ref exists to prevent; got: %v", err)
	}
	if _, err := p.Get(ctx, provision.Ref{Name: spec.Ref.Name}); err != nil {
		t.Fatalf("Get by NAME ALONE must work: %v", err)
	}
}

func testGetMissingIsNotFound(t *testing.T, h Harness) {
	p := h.New(t)
	_, err := p.Get(context.Background(), provision.Ref{Name: "absent"})
	if !errors.Is(err, provision.ErrNotFound) {
		t.Fatalf("Get of a missing instance must be ErrNotFound, got %v", err)
	}
	if errors.Is(err, provision.ErrBlind) {
		t.Fatal("Get of a missing instance must NOT also be ErrBlind: 'reached the backend, it is not there' " +
			"and 'could not reach the backend' are the two answers callers branch on")
	}
}

func testListReportsWhatWasCreated(t *testing.T, h Harness) {
	p := h.New(t)
	ctx := context.Background()
	for _, name := range []string{"alpha", "bravo"} {
		if err := p.Create(ctx, MinimalSpec(name)); err != nil {
			t.Fatalf("Create %q: %v", name, err)
		}
	}
	list, err := mustList(t, p)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := map[string]bool{}
	for _, inst := range list {
		got[inst.Ref.Name] = true
		if inst.Driver != p.Driver() {
			t.Errorf("instance %q reports driver %q, want %q", inst.Ref.Name, inst.Driver, p.Driver())
		}
	}
	for _, want := range []string{"alpha", "bravo"} {
		if !got[want] {
			t.Errorf("List omitted %q; got %+v", want, list)
		}
	}
}

func testUpdateCreatesWhenAbsent(t *testing.T, h Harness) {
	p := h.New(t)
	ctx := context.Background()
	spec := MinimalSpec("upsert")
	if err := p.Update(ctx, spec); err != nil {
		t.Fatalf("Update of an absent instance must create it: %v", err)
	}
	if _, err := p.Get(ctx, spec.Ref); err != nil {
		t.Fatalf("Get after Update: %v", err)
	}
}

func testUpdateThenCreateIsNotDivergent(t *testing.T, h Harness) {
	p := h.New(t)
	ctx := context.Background()
	spec := MinimalSpec("reconcile")
	if err := p.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	changed := spec
	changed.Runtime = provision.Runtime{Image: "ghcr.io/muster-example/rolled:3"}
	if err := p.Update(ctx, changed); err != nil {
		t.Fatalf("Update: %v", err)
	}
	// After an Update, the recorded spec must be the NEW one — otherwise the
	// next reconcile loop's Create sees divergence forever and the instance is
	// rebuilt on every pass.
	if err := p.Create(ctx, changed); err != nil {
		t.Fatalf("Create with the spec just Updated to must be idempotent, not divergent; got %v", err)
	}
}

func testScaleToZeroIsStoppedNotGone(t *testing.T, h Harness) {
	p := h.New(t)
	ctx := context.Background()
	spec := MinimalSpec("scaler")
	if err := p.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := p.Scale(ctx, spec.Ref, 0); err != nil {
		t.Fatalf("Scale to 0: %v", err)
	}
	inst, err := p.Get(ctx, spec.Ref)
	if err != nil {
		t.Fatalf("a scaled-to-zero instance must still EXIST (Scale is not Destroy); Get: %v", err)
	}
	if inst.Phase != provision.PhaseStopped {
		t.Fatalf("scaled to zero, Phase is %q, want %q — a deliberate stop reported as failure is an outage report", inst.Phase, provision.PhaseStopped)
	}
	if err := p.Scale(ctx, spec.Ref, 1); err != nil {
		t.Fatalf("Scale back to 1: %v", err)
	}
	inst, err = p.Get(ctx, spec.Ref)
	if err != nil {
		t.Fatalf("Get after scaling back: %v", err)
	}
	if inst.Phase == provision.PhaseStopped {
		t.Fatal("scaled back to 1 and still reports stopped")
	}
}

func testScaleMissingIsNotFound(t *testing.T, h Harness) {
	p := h.New(t)
	err := p.Scale(context.Background(), provision.Ref{Name: "absent"}, 1)
	if !errors.Is(err, provision.ErrNotFound) {
		t.Fatalf("Scale of a missing instance must be ErrNotFound, got %v", err)
	}
}

func testDestroyRemovesTheInstance(t *testing.T, h Harness) {
	p := h.New(t)
	ctx := context.Background()
	spec := MinimalSpec("doomed")
	if err := p.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := p.Destroy(ctx, spec.Ref); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	// 🔴 THE POINT OF THE CASE. Destroy's nil is a CLAIM that the instance is
	// gone, and this is the reading that checks the claim. A driver whose
	// Destroy is `return nil` passes every other case in this suite.
	if _, err := p.Get(ctx, spec.Ref); !errors.Is(err, provision.ErrNotFound) {
		t.Fatalf("Destroy returned nil but Get still finds the instance (err=%v). "+
			"nil from Destroy means removed or already absent, nothing else.", err)
	}
	list, err := mustList(t, p)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, inst := range list {
		if inst.Ref.Name == spec.Ref.Name {
			t.Fatalf("Destroy returned nil but List still reports %q", spec.Ref.Name)
		}
	}
}

func testDestroyAbsentIsNil(t *testing.T, h Harness) {
	p := h.New(t)
	if err := p.Destroy(context.Background(), provision.Ref{Name: "never-existed"}); err != nil {
		t.Fatalf("Destroy of an instance that was never created must be nil — already absent is the "+
			"state the caller asked for; got %v", err)
	}
}

func testEndpointOverride(t *testing.T, h Harness) {
	p := h.New(t)
	ctx := context.Background()
	spec := MinimalSpec("reachable")
	// Values chosen distinct from every default in the package, so a driver
	// that ignores the override cannot pass by coincidence.
	spec.Endpoint = &provision.Endpoint{Scheme: "https", Host: "agent.example.test", Port: 19731}

	if err := p.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ep, err := p.Endpoint(ctx, spec.Ref)
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if ep.Host != "agent.example.test" || ep.Port != 19731 || ep.Scheme != "https" {
		t.Fatalf("Endpoint ignored the per-instance override: got %+v. This override is the whole "+
			"reason Endpoint is a method rather than a hardcoded format string.", ep)
	}

	// Without the override the driver must still answer — from its own
	// configured template — rather than error.
	plain := MinimalSpec("default-endpoint")
	if err := p.Create(ctx, plain); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ep2, err := p.Endpoint(ctx, plain.Ref)
	if err != nil {
		t.Fatalf("Endpoint without an override must resolve from the driver's template: %v", err)
	}
	if ep2.Host == "" || ep2.Port == 0 {
		t.Fatalf("default endpoint is incomplete: %+v", ep2)
	}
	if ep2.Host == ep.Host {
		t.Fatalf("the default endpoint host %q equals the OVERRIDE host; this case could not tell "+
			"an honoured override from an ignored one", ep2.Host)
	}
	if ep2.Port != 8421 {
		t.Fatalf("default endpoint port is %d, want the spec's declared port 8421", ep2.Port)
	}
}

func testEndpointMissingIsNotFound(t *testing.T, h Harness) {
	p := h.New(t)
	_, err := p.Endpoint(context.Background(), provision.Ref{Name: "absent"})
	if !errors.Is(err, provision.ErrNotFound) {
		t.Fatalf("Endpoint of a missing instance must be ErrNotFound, got %v", err)
	}
}

func testLogsMissingIsNotFound(t *testing.T, h Harness) {
	p := h.New(t)
	ctx := context.Background()
	if _, err := p.TailLogs(ctx, provision.Ref{Name: "absent"}, 10); !errors.Is(err, provision.ErrNotFound) {
		t.Fatalf("TailLogs of a missing instance must be ErrNotFound, got %v", err)
	}
	err := p.StreamLogs(ctx, provision.Ref{Name: "absent"}, func(string) {
		t.Error("StreamLogs emitted a line for an instance that does not exist")
	})
	if !errors.Is(err, provision.ErrNotFound) {
		t.Fatalf("StreamLogs of a missing instance must be ErrNotFound, got %v", err)
	}
}

func testBlindListErrors(t *testing.T, h Harness) {
	p := h.Blind(t)
	list, err := p.List(context.Background())
	if err == nil {
		t.Fatalf("a driver that cannot see its backend returned nil error and %d instances. "+
			"An empty List is a POSITIVE CLAIM that nothing is running, and the caller acts on it "+
			"by rewriting every stored status to stopped.", len(list))
	}
	if !errors.Is(err, provision.ErrBlind) {
		t.Fatalf("blind List must wrap ErrBlind so callers can branch, got %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("blind List returned an error AND %d instances; a partial answer with an error is "+
			"a third state nobody handles", len(list))
	}
}

func testBlindGetIsNotNotFound(t *testing.T, h Harness) {
	p := h.Blind(t)
	_, err := p.Get(context.Background(), provision.Ref{Name: "anything"})
	if !errors.Is(err, provision.ErrBlind) {
		t.Fatalf("blind Get must wrap ErrBlind, got %v", err)
	}
	if errors.Is(err, provision.ErrNotFound) {
		t.Fatal("blind Get reported ErrNotFound. That says the backend was reached and the instance " +
			"is not there — which is exactly the false statement that deletes a live instance's stored state.")
	}
}

func testBlindDestroyErrors(t *testing.T, h Harness) {
	p := h.Blind(t)
	if err := p.Destroy(context.Background(), provision.Ref{Name: "anything"}); err == nil {
		t.Fatal("a blind driver's Destroy returned nil. nil means removed or already absent, and a " +
			"driver that cannot reach its backend knows neither.")
	}
}

// capabilityProbe is one CheckSpec-gated capability: how to ask for it in a
// spec, and how to read whether the driver claims it.
//
// ⚠ ONLY CAPABILITIES CheckSpec ACTUALLY BRANCHES ON BELONG HERE. Listing one
// it does not branch on would produce a case that can never fail, and a case
// that can never fail is worse than no case: it reads as coverage.
var capabilityProbes = []struct {
	name  string
	claim func(provision.Capabilities) bool
	ask   func(*provision.Spec)
}{
	{
		name:  "Files",
		claim: func(c provision.Capabilities) bool { return c.Files },
		ask: func(s *provision.Spec) {
			s.Files = []provision.File{{Path: "/etc/muster/agent.json", Content: []byte(`{"probe":true}`)}}
		},
	},
	{
		name:  "Secrets",
		claim: func(c provision.Capabilities) bool { return c.Secrets },
		ask: func(s *provision.Spec) {
			s.Secrets = []provision.EnvVar{{Name: "MUSTER_PROBE_TOKEN", Value: "probe-value-not-a-real-credential"}}
		},
	},
	{
		name:  "Persistence",
		claim: func(c provision.Capabilities) bool { return c.Persistence },
		ask: func(s *provision.Spec) {
			s.Workspace = provision.Workspace{Path: "/data/workspace", Size: "3Gi", Persist: true}
		},
	},
	{
		name:  "ResourceLimits",
		claim: func(c provision.Capabilities) bool { return c.ResourceLimits },
		ask: func(s *provision.Spec) {
			s.Resources = provision.Resources{MemoryRequest: "512Mi", MemoryLimit: "1536Mi"}
		},
	},
	{
		name:  "Scale",
		claim: func(c provision.Capabilities) bool { return c.Scale },
		ask:   func(s *provision.Spec) { s.Replicas = 3 },
	},
	{
		name:  "NetworkIsolation",
		claim: func(c provision.Capabilities) bool { return c.NetworkIsolation },
		ask: func(s *provision.Spec) {
			s.Network = provision.Network{Isolate: true, PublicEgressTCPPorts: []int{8443}}
		},
	},
}

// testRestrictedRefuses is the case that makes Capabilities a guard rather than
// a field. For every capability the restricted driver does NOT claim, a spec
// asking for it must be refused with ErrUnsupported; for every one it does
// claim, the same spec must be accepted.
//
// 🔴 BOTH DIRECTIONS ARE ASSERTED ON PURPOSE. A refusal-only case passes for a
// driver that refuses everything, which is a different broken driver.
func testRestrictedRefuses(t *testing.T, h Harness) {
	p := h.Restricted(t)
	caps := p.Capabilities()
	ctx := context.Background()

	refused, accepted := 0, 0
	for i, probe := range capabilityProbes {
		spec := MinimalSpec(fmt.Sprintf("cap-%d", i))
		probe.ask(&spec)
		err := p.Create(ctx, spec)
		if probe.claim(caps) {
			if err != nil {
				t.Errorf("%s: driver claims %s and refused a spec using it: %v", h.Name, probe.name, err)
				continue
			}
			accepted++
			continue
		}
		if !errors.Is(err, provision.ErrUnsupported) {
			t.Errorf("%s: driver does NOT claim %s, and a spec asking for it returned %v — want ErrUnsupported. "+
				"Honouring part of a spec and dropping the rest produces something that looks like it worked.",
				h.Name, probe.name, err)
			continue
		}
		refused++
		// The refusal must have changed nothing.
		if _, gerr := p.Get(ctx, spec.Ref); !errors.Is(gerr, provision.ErrNotFound) {
			t.Errorf("%s: %s was refused but the instance exists anyway (err=%v)", h.Name, probe.name, gerr)
		}
	}

	// 🔴 THE POSITIVE CONTROL. A run in which nothing was refused AND nothing
	// was accepted is a run that measured nothing, and it is indistinguishable
	// from a passing one unless it says so.
	t.Logf("%s restricted: %d capabilities refused, %d accepted, of %d probed",
		h.Name, refused, accepted, len(capabilityProbes))
	if refused+accepted == 0 {
		t.Fatal("no capability probe produced a verdict; this case measured nothing")
	}
	if refused == 0 {
		t.Fatalf("the Restricted harness claims every CheckSpec-gated capability (%+v), so this case "+
			"cannot observe a refusal. Restricted must be the driver's genuinely minimal configuration.", caps)
	}
}

// testPolicyRefusedOrApplied is the security case, and it asserts the universal
// half for EVERY driver before branching on capability.
//
// 🔴 THE UNIVERSAL HALF: a policy whose rules the driver cannot interpret must
// be REFUSED. It does not matter whether the refusal comes from the type
// assertion, from the capability flag, or from the driver's own parser — what
// matters is that "granted" is never recorded for a policy nobody applied. A
// UI reading "granted" for an unapplied policy is a defect that reads as
// coverage, so nobody looks at it again.
func testPolicyRefusedOrApplied(t *testing.T, h Harness) {
	p := h.New(t)
	ctx := context.Background()
	spec := MinimalSpec("policied")
	if err := p.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Deliberately not any driver's shape: an object with a key no driver in
	// this repository declares.
	alien := provision.Policy{
		Name:  "contract-probe-alien",
		Rules: []byte(`{"musterContractProbeUnknownShape":{"verbs":["everything"]}}`),
	}
	if err := provision.Grant(ctx, p, spec.Ref, alien); !errors.Is(err, provision.ErrUnsupported) {
		t.Fatalf("a policy this driver cannot interpret must be refused with ErrUnsupported, got %v. "+
			"Applying the parts it understands and ignoring the rest records access that was never given.", err)
	}

	if !p.Capabilities().Policy {
		if err := provision.Revoke(ctx, p, spec.Ref, alien.Name); err != nil {
			t.Fatalf("Revoke against a driver that cannot grant must be a no-op, not an error: %v", err)
		}
		return
	}

	// The other direction. Without it, a driver that refuses EVERYTHING passes.
	if h.GrantablePolicy.Name == "" {
		t.Fatalf("%s declares Capabilities.Policy but the harness supplies no GrantablePolicy, so this "+
			"case can only observe refusals — which is exactly what a driver that refuses everything does", h.Name)
	}
	pol := h.GrantablePolicy
	if err := provision.Grant(ctx, p, spec.Ref, pol); err != nil {
		t.Fatalf("driver claims Policy and refused its own GrantablePolicy: %v", err)
	}
	// Idempotent: granting twice is one grant.
	if err := provision.Grant(ctx, p, spec.Ref, pol); err != nil {
		t.Fatalf("a second Grant of the same policy must succeed: %v", err)
	}
	if err := provision.Revoke(ctx, p, spec.Ref, pol.Name); err != nil {
		t.Fatalf("Revoke after a successful Grant: %v", err)
	}
	// Revoking twice is the "already absent" case again.
	if err := provision.Revoke(ctx, p, spec.Ref, pol.Name); err != nil {
		t.Fatalf("second Revoke must be a no-op: %v", err)
	}
}

// testExecRefusedWhenNotDeclared asserts BOTH directions, and deliberately does
// not skip: a driver that declares Exec must actually implement Execer, and one
// that does not must be refused by the helper rather than by a nil panic at the
// call site.
func testExecRefusedWhenNotDeclared(t *testing.T, h Harness) {
	p := h.New(t)
	_, implements := p.(provision.Execer)
	_, implementsTTY := p.(provision.TTYExecer)
	if p.Capabilities().Exec {
		if !implements {
			t.Fatal("driver declares Capabilities.Exec but does not implement provision.Execer; " +
				"every caller type-asserts, so this is a declaration with nothing behind it")
		}
		// The terminal is the same capability: a driver that declares Exec and
		// cannot attach a terminal would answer the terminal route with a
		// refusal that blames a capability it reports having.
		if !implementsTTY {
			t.Fatal("driver declares Capabilities.Exec but does not implement provision.TTYExecer; " +
				"Exec covers both entry points (see Capabilities), so the terminal would be refused " +
				"by a driver that reports it can exec")
		}
		return
	}
	err := provision.Exec(context.Background(), p, provision.Ref{Name: "anything"},
		[]string{"true"}, nil, nil, nil)
	if !errors.Is(err, provision.ErrUnsupported) {
		t.Fatalf("Exec against a driver that does not declare it must be ErrUnsupported, got %v", err)
	}
	err = provision.ExecTTY(context.Background(), p, provision.Ref{Name: "anything"},
		[]string{"true"}, nil, nil, nil)
	if !errors.Is(err, provision.ErrUnsupported) {
		t.Fatalf("ExecTTY against a driver that does not declare Exec must be ErrUnsupported, got %v", err)
	}
}

// mustList wraps List with the one assertion every caller of it makes: an error
// comes with no instances.
func mustList(t *testing.T, p provision.Provisioner) ([]provision.Instance, error) {
	t.Helper()
	list, err := p.List(context.Background())
	if err != nil && len(list) != 0 {
		t.Fatalf("List returned an error AND %d instances: %v", len(list), err)
	}
	return list, err
}
