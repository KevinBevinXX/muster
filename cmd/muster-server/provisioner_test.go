package main

import (
	"context"
	"log"
	"os"
	"reflect"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/api"
	k8sdriver "github.com/ZacxDev/muster/internal/provision/k8s"
	"github.com/ZacxDev/muster/internal/provision/provisiontest"
)

// stubStore is an agents.Store that exists only to be non-nil.
//
// ⚠ IT EMBEDS THE INTERFACE RATHER THAN IMPLEMENTING IT, so any method a test
// actually reaches is a nil-interface panic. That is the loud outcome: these tests
// are about WIRING, and a stub that answered zero values would let a behavioural
// assertion slip in here and pass over a fake.
type stubStore struct{ agents.Store }

// provisionerTestConfig is a config that passes validateProvisioner for the named
// driver, so each test below varies ONE thing.
func provisionerTestConfig(driver string) config {
	return config{
		Database:             "postgres://unused",
		AgentProvisioner:     driver,
		AgentImageRepo:       "registry.example.test/muster/agent-runtime",
		AgentAPIURL:          "http://muster.example.test:8105",
		AgentNamespacePrefix: agents.NamespacePrefix,
	}
}

// TestBuildingTheKubernetesProvisionerUsesTheRealDriver is the behavioural half of
// the linkage ledger's departure.
//
// 🔴 LINKAGE SAYS "REACHABLE", NOT "USED", AND THE LEDGER'S OWN HEADER SAYS SO: an
// import can be a blank `_` that executes nothing. internal/provision/k8s left the
// not-linked ledger because provisioner.go imports it — this is what makes that a
// claim about a CONSTRUCTED driver rather than about an import line.
//
// 🔴 IT BUILDS THE DRIVER FROM A FAKE CLIENTSET RATHER THAN SKIPPING WITHOUT A
// CLUSTER, and that is the whole reason k8s.Config.Client is an interface. A test
// that needed a real cluster would be a t.Skip on CI — which this project's own
// notes call worse than no test, because a suite that ran nothing still reports
// green.
func TestBuildingTheKubernetesProvisionerUsesTheRealDriver(t *testing.T) {
	logger := log.New(&strings.Builder{}, "", 0)
	dc := k8sDriverConfig(provisionerTestConfig(provisionerK8s), logger)
	dc.Client = fake.NewClientset()

	driver, err := k8sdriver.New(dc)
	if err != nil {
		t.Fatalf("k8sdriver.New over the mapped config: %v", err)
	}
	// 🔴 THE DRIVER'S OWN NAME, NOT A TYPE ASSERTION. provision.Provisioner.Driver
	// exists so an operator knows which set of capability losses applies; asserting
	// on it here means a future change that swapped the Kubernetes driver for a
	// stand-in with the same method set reddens, which a `*k8sdriver.Driver` type
	// check would too — but the name is also what the boot banner reports, so this
	// assertion and the banner cannot disagree.
	if got := driver.Driver(); got != "kubernetes" {
		t.Errorf("Driver() = %q, want %q", got, "kubernetes")
	}

	// NEGATIVE CONTROL: the noop branch must produce a DIFFERENT driver. Without
	// it, an assertion that a driver reports "kubernetes" would pass over a
	// buildDriver that ignored its argument and always built the same thing.
	noop, err := buildDriver(provisionerTestConfig(provisionerNoop), logger)
	if err != nil {
		t.Fatalf("buildDriver(noop): %v", err)
	}
	if got := noop.Driver(); got == driver.Driver() {
		t.Errorf("the noop branch built a driver reporting %q, the same as the kubernetes "+
			"branch — buildDriver is not branching on its argument", got)
	}

	// 🔴 AND buildDriver'S kubernetes BRANCH MUST ROUTE TO buildK8sDriver, which
	// the assertions above cannot see. Found by mutation: replacing
	// `case provisionerK8s: return buildK8sDriver(...)` with `return
	// provision.NewNoop()` SURVIVED, because this test constructs the driver from
	// k8sDriverConfig DIRECTLY and never asks buildDriver for one. Outside a pod
	// the real branch REFUSES, so a nil error here means it routed somewhere else.
	if _, err := buildDriver(provisionerTestConfig(provisionerK8s), logger); err == nil {
		t.Error("buildDriver(kubernetes) succeeded outside a cluster, so its kubernetes case " +
			"is not reaching buildK8sDriver — something else is answering for it.")
	}
}

// TestTheKubernetesBranchRefusesOutsideAClusterRatherThanFallingBack pins the
// refusal, which is the safety property.
//
// 🔴 A KUBECONFIG FALLBACK WOULD PROVISION INTO WHICHEVER CLUSTER THE AMBIENT
// CONTEXT NAMES. That is the failure where a developer's test dispatch creates
// pods in production, and it is a failure of OMISSION — nothing would log, because
// from the binary's point of view it worked.
//
// ⚠ IT ASSERTS THE MESSAGE NAMES THE VARIABLE THAT CAUSED IT. An error that said
// only "unable to load in-cluster configuration" sends the reader to look at
// Kubernetes; the variable is what they actually set.
func TestTheKubernetesBranchRefusesOutsideAClusterRatherThanFallingBack(t *testing.T) {
	// This test process is not a pod: rest.InClusterConfig has no service-account
	// token to read.
	_, _, err := buildAgentPlane(provisionerTestConfig(provisionerK8s), stubStore{}, log.New(&strings.Builder{}, "", 0))
	if err == nil {
		t.Fatal("buildAgentPlane succeeded for the kubernetes driver outside a cluster, " +
			"which means it fell back to some other credential source. A fallback " +
			"provisions into whichever cluster the ambient context names.")
	}
	if !strings.Contains(err.Error(), envAgentProvisioner) {
		t.Errorf("the refusal does not name %s, so a reader is sent to debug Kubernetes "+
			"rather than the variable they set.\n  got: %v", envAgentProvisioner, err)
	}
}

// TestTheNoopProvisionerWiresAnAdapterAndNoneWiresNothing pins both ends of the
// knob, because the default is the one that must not change behaviour.
//
// 🔴 (nil, nil) FOR `none` IS A SUPPORTED RESULT, NOT AN ERROR CASE. Every
// previous revision of this binary had no provisioner at all, and a default that
// provisioned would change what an unchanged deployment does on the next image
// bump — where the thing it changes is "create pods in a cluster".
func TestTheNoopProvisionerWiresAnAdapterAndNoneWiresNothing(t *testing.T) {
	logger := log.New(&strings.Builder{}, "", 0)

	adapter, _, err := buildAgentPlane(provisionerTestConfig(provisionerNoop), stubStore{}, logger)
	if err != nil {
		t.Fatalf("buildAgentPlane(noop): %v", err)
	}
	if adapter == nil {
		t.Fatal("buildAgentPlane(noop) returned nil, so the lifecycle routes would keep refusing")
	}
	if got := adapter.Driver(); got != "noop" {
		t.Errorf("adapter.Driver() = %q, want %q", got, "noop")
	}

	none, _, err := buildAgentPlane(provisionerTestConfig(provisionerNone), stubStore{}, logger)
	if err != nil {
		t.Fatalf("buildAgentPlane(none): %v", err)
	}
	if none != nil {
		t.Error("buildAgentPlane(none) returned a provisioner. The default must leave " +
			"api.Extensions.Provisioner nil so every lifecycle route keeps refusing at the " +
			"door exactly as it did before this knob existed.")
	}

	// 🔴 THE ZERO-VALUE CONFIG IS ITS OWN CASE, AND IT WAS MISSING. Found by
	// mutation: changing config.agentProvisioner's empty-string default from
	// provisionerNone to provisionerNoop SURVIVED, because the case above passes
	// "none" EXPLICITLY and never exercises the resolver. buildApp accepts configs
	// that never went through loadConfig — every wiring test builds one — so this is
	// the shape a zero value actually reaches the wiring in, and a default that
	// provisioned would create pods for a deployment that asked for nothing.
	zero, _, err := buildAgentPlane(config{}, stubStore{}, logger)
	if err != nil {
		t.Fatalf("buildAgentPlane over a zero-value config: %v", err)
	}
	if zero != nil {
		t.Errorf("a zero-value config produced a %q provisioner. An unset "+
			"%s must resolve to %q: a default that provisions changes what an unchanged "+
			"deployment does on its next image bump, and what it changes is \"create pods "+
			"in a cluster\".", zero.Driver(), envAgentProvisioner, provisionerNone)
	}

	// A provisioner with no store could resolve no agent id, and would fail from a
	// background goroutine whose only trace is a log line.
	if _, _, err := buildAgentPlane(provisionerTestConfig(provisionerNoop), nil, logger); err == nil {
		t.Error("buildAgentPlane accepted a nil agents store")
	}
}

// TestTheStoredNamespacePrefixIsWhatTheDriverIsConfiguredWith is the guard
// internal/agents.NamespacePrefix's own comment asks for.
//
// 🔴 IT PINS A RELATIONSHIP BETWEEN TWO SUBSYSTEMS THAT NEVER TALK, AND A
// DISAGREEMENT BETWEEN THEM IS SILENT. The HTTP handler writes
// api.Extensions.AgentNamespace(name) into the row; the driver decides where the
// instance really goes. Nothing fails if they differ — the value is recorded,
// served on GET /api/agents and labelled onto the instance's objects, and nothing
// places anything with it, so `kubectl -n <what the row says>` returns nothing
// and reads as "this agent was never provisioned" while the pod runs one
// namespace over.
//
// 🔴 IT CHECKS A *CONFIGURED* PREFIX AND NOT ONLY THE DEFAULT. This comment used
// to read "⚠ IT CHECKS THE *DEFAULT*, which is the case that can drift. An
// operator who sets MUSTER_AGENT_NAMESPACE_PREFIX has said what they want and
// owns the consequence" — so the guard was STRUCTURALLY BLIND to the only
// configuration anybody runs, and the defect shipped under it. Measured live on a
// deployment that sets MUSTER_AGENT_NAMESPACE_PREFIX=muster-agent-: a freshly
// provisioned agent's row said `devpod-lively-newt` while the driver had created
// `muster-agent-lively-newt`.
//
// 🔴 AND THE DRIVER SIDE IS *OBSERVED*, NOT READ OFF THE CONFIG. It used to
// compare `dc.NamespacePrefix + name` — the test re-implementing the driver's
// namespace rule, so it could only ever catch a wrong PREFIX and never a wrong
// RULE. This builds the real driver over a fake clientset, creates an instance
// and asks where it landed, which also covers the driver's own
// `prefix == "" → "muster-"` fallback and anything else namespaceFor does.
//
// ⚠ THE DEPLOYED VALUE IS ONE CASE, NOT THE CASE. A guard pinned to
// `muster-agent-` would pass for an implementation that special-cased that one
// string, so the table also drives an arbitrary prefix nothing else in this module
// spells, and every expectation is a LITERAL rather than a second call to the code
// under test.
func TestTheStoredNamespacePrefixIsWhatTheDriverIsConfiguredWith(t *testing.T) {
	const name = "harbour-kestrel"
	cases := []struct {
		label string
		env   string // MUSTER_AGENT_NAMESPACE_PREFIX
		want  string // the namespace BOTH sides must produce, spelled out
	}{
		// The case the old guard covered: nothing set.
		{label: "default", env: "", want: "devpod-harbour-kestrel"},
		// The DEPLOYED case, which the old guard exempted by design.
		{label: "deployed", env: "muster-agent-", want: "muster-agent-harbour-kestrel"},
		// An arbitrary prefix, so the assertion is not pinned to the one value
		// this installation happens to use.
		{label: "arbitrary", env: "qx7-pen-", want: "qx7-pen-harbour-kestrel"},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			cfg, err := loadConfig(func(n string) string {
				switch n {
				case envAgentProvisioner:
					return provisionerK8s
				case envAgentImageRepo:
					return "registry.example.test/muster/agent-runtime"
				case envAgentAPIURL:
					return "http://muster.example.test:8105"
				case envDatabase:
					return "postgres://unused"
				case envAgentNSPrefix:
					return tc.env
				}
				return ""
			})
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if err := cfg.validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}

			// THE ROW-WRITING PATH, through the same expression the handler
			// evaluates (internal/api/agents.go's Create call is
			// s.ext.AgentNamespace(name)). Wiring the Extensions here from
			// cfg.AgentNamespacePrefix is exactly what cmd/muster-server's newApp
			// does.
			stored := api.Extensions{AgentNamespacePrefix: cfg.AgentNamespacePrefix}.
				AgentNamespace(name)

			dc := k8sDriverConfig(cfg, nil)

			// 🔴 THE LAYOUT AND PREFIX CONTROLS COME *BEFORE* THE DRIVER IS BUILT,
			// AND THE ORDER IS LOAD-BEARING RATHER THAN TIDY. Measured by mutation:
			// with the controls below the driver construction, inverting
			// k8sDriverConfig's NamespacePerInstance killed this test at
			// `k8sdriver.New` — "Config.Namespace is required when
			// NamespacePerInstance is false" — so the mutant died on a DIFFERENT
			// guard's error and the layout assertion never executed. It would have
			// stayed green with itself deleted. These run first so each reports its
			// own failure.

			// 🔴 THE LAYOUT MUST BE PER-INSTANCE, OR THE PREFIX IS NEVER USED AT ALL.
			// k8s.Config.NamespacePrefix is read only when NamespacePerInstance is true, so
			// a prefix that matches while the layout is shared would be a prefix the driver
			// ignores, over instances that all land in one namespace no row ever names.
			if !dc.NamespacePerInstance {
				t.Errorf("the default layout is SHARED, so NamespacePrefix (%q) is ignored and "+
					"every instance lands in one namespace no row names. %s defaults to false, "+
					"which must map to NamespacePerInstance TRUE.", dc.NamespacePrefix,
					envAgentNSShared)
			}
			// Control: the prefix is non-empty, or the equality below holds trivially
			// for every possible name.
			if dc.NamespacePrefix == "" {
				t.Errorf("instrument check FAILED: the NamespacePrefix is empty, so the "+
					"comparison below would pass for any name. %s=%q should resolve to a "+
					"non-empty prefix (empty defaults to %q)",
					envAgentNSPrefix, tc.env, agents.NamespacePrefix)
			}

			// THE DRIVER SIDE, OBSERVED. Build the real driver from the mapped
			// config over a fake clientset, create the instance, and read the
			// namespace it actually landed in.
			dc.Client = fake.NewClientset()
			driver, err := k8sdriver.New(dc)
			if err != nil {
				t.Fatalf("k8sdriver.New over the mapped config: %v", err)
			}
			spec := provisiontest.MinimalSpec(name)
			if err := driver.Create(context.Background(), spec); err != nil {
				t.Fatalf("Create: %v", err)
			}
			inst, err := driver.Get(context.Background(), spec.Ref)
			if err != nil {
				t.Fatalf("Get after Create: %v", err)
			}
			driverSide := inst.Group

			if stored != driverSide {
				t.Errorf("%s=%q: the row would say namespace %q and the driver PUT THE INSTANCE IN %q.\n"+
					"    Nothing fails when these differ: the value is recorded, served on "+
					"GET /api/agents and labelled onto the instance's objects, and nothing places "+
					"anything with it — so the row names a namespace that holds nothing, which reads "+
					"as \"never provisioned\" while the instance runs.\n"+
					"    Both sides must come from cfg.AgentNamespacePrefix: k8sDriverConfig gives it "+
					"to the driver, newApp gives it to api.Extensions.AgentNamespacePrefix.",
					envAgentNSPrefix, tc.env, stored, driverSide)
			}
			// 🔴 AND BOTH MUST BE THE CONFIGURED VALUE, NOT MERELY EQUAL TO EACH
			// OTHER. Equality alone is satisfied by two sides that ignore the
			// configuration identically — a different bug with the same green.
			// These are literals, not a third call to the code under test.
			if stored != tc.want {
				t.Errorf("%s=%q: the row would say namespace %q, want %q — the stored namespace "+
					"ignores the configured prefix", envAgentNSPrefix, tc.env, stored, tc.want)
			}
			if driverSide != tc.want {
				t.Errorf("%s=%q: the driver created the instance in %q, want %q", envAgentNSPrefix,
					tc.env, driverSide, tc.want)
			}
		})
	}
}

// TestTheDefaultNamespacePrefixIsTheStoresOwnConstant pins the half of the pair
// that has no environment variable behind it.
//
// ⚠ IT IS AN INVARIANT GUARD, NOT A REGRESSION TEST — no measured defect made the
// default wrong. It exists because "empty resolves to agents.NamespacePrefix" is
// the sentence the whole defaulting story rests on, and it is now asserted at the
// ONE resolver both the config load and the HTTP wiring call.
func TestTheDefaultNamespacePrefixIsTheStoresOwnConstant(t *testing.T) {
	if got := agents.ResolveNamespacePrefix(""); got != "devpod-" {
		t.Errorf("an unset prefix resolved to %q, want %q", got, "devpod-")
	}
	if agents.NamespacePrefix != "devpod-" {
		t.Errorf("agents.NamespacePrefix is %q, want %q: %s's documented default and the "+
			"constant are one value", agents.NamespacePrefix, "devpod-", envAgentNSPrefix)
	}
	// Whitespace is not a prefix. An operator's trailing newline in a ConfigMap
	// would otherwise build a namespace Kubernetes refuses, inside a dispatch
	// goroutine.
	if got := agents.ResolveNamespacePrefix("  "); got != "devpod-" {
		t.Errorf("a blank prefix resolved to %q, want the default %q", got, "devpod-")
	}
	// And a configured prefix survives the resolver untouched, or the default
	// would be the only value it can ever return.
	if got := agents.ResolveNamespacePrefix("qx7-pen-"); got != "qx7-pen-" {
		t.Errorf("a configured prefix resolved to %q, want it unchanged", got)
	}
}

// TestTheSharedNamespaceLayoutIsRefusedWithoutANamespace pins the one boot refusal
// that is about a combination rather than a missing value.
func TestTheSharedNamespaceLayoutIsRefusedWithoutANamespace(t *testing.T) {
	cfg := provisionerTestConfig(provisionerK8s)
	cfg.AgentNamespaceShared = true
	if err := cfg.validate(); err == nil {
		t.Fatalf("validate accepted %s=1 with no %s: the kubernetes driver requires a "+
			"namespace in that layout, so every dispatch would be refused inside a "+
			"goroutine instead of at boot", envAgentNSShared, envAgentNamespace)
	}
	cfg.AgentNamespace = "muster-agents"
	if err := cfg.validate(); err != nil {
		t.Errorf("validate rejected the complete shared layout: %v", err)
	}
}

// TestAnIllegalProvisionerNameIsRefusedAtBoot pins the enumeration.
//
// ⚠ THE UNHANDLED BRANCH IN buildDriver IS THE OTHER HALF OF THIS, and it returns
// an error naming itself a wiring bug rather than a nil driver — because a nil
// there would present as a working server whose every dispatch panicked in a
// goroutine.
func TestAnIllegalProvisionerNameIsRefusedAtBoot(t *testing.T) {
	cfg := provisionerTestConfig("kubernets") // a plausible typo
	err := cfg.validate()
	if err == nil {
		t.Fatal("validate accepted an unknown provisioner name")
	}
	for _, want := range provisionerChoices {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not list the legal value %q, so the reader is told "+
				"they are wrong without being told what is right.\n  got: %v", want, err)
		}
	}

	// And the wiring branch must refuse rather than return a nil driver, for a
	// value validate would have caught.
	if _, err := buildDriver(cfg, nil); err == nil {
		t.Error("buildDriver returned a driver for an unknown provisioner name")
	}
}

// TestNewAppWiresTheConfiguredNamespacePrefixIntoTheApiLayer closes the one hop
// no behavioural test in this module can reach.
//
// 🔴 THE CHAIN IS env → config → api.Extensions → the row, AND EVERY LINK BUT
// THIS ONE IS PINNED BEHAVIOURALLY. TestTheStoredNamespacePrefixIsWhatTheDriverIsConfiguredWith
// pins config → both sides; internal/api's
// TestTheCreatedAgentRowCarriesTheConfiguredNamespace pins Extensions → the row.
// What neither can see is newApp's api.Extensions literal actually carrying the
// field: dropping that one line reproduces the shipped defect exactly — the
// handler falls back to agents.NamespacePrefix while the driver uses the
// configured prefix — and no test in this repository would go red. Reaching it
// behaviourally needs a booted app with a database AND a provisioner, i.e. a
// cluster client.
//
// ⚠ IT IS A SPELLED GUARD, AND THE LIMITS ARE STATED RATHER THAN HIDDEN. It reads
// source text, so it cannot tell whether the assignment is inside the literal that
// is actually passed to UseExtensions, and a second api.Extensions literal
// elsewhere in main.go would satisfy it. What it does catch is the whole of the
// regression it was written for: deleting the line, or sourcing it from anything
// other than the config field the driver also reads. api.Extensions.defects is the
// structural half — it refuses /readyz when the prefix is empty beside a wired
// Provisioner — and this is what makes the omission visible in CI rather than at
// boot.
func TestNewAppWiresTheConfiguredNamespacePrefixIntoTheApiLayer(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	text := string(src)

	// 🔴 POSITIVE CONTROL. Without it a changed variable name (`ext := api.Extensions{`
	// → anything else) would make the assertion below vacuous, and a guard that
	// matches nothing reports the same green as a guard that passes.
	const literal = "api.Extensions{"
	if !strings.Contains(text, literal) {
		t.Fatalf("instrument check FAILED: main.go no longer contains %q, so this test is not "+
			"reading the wiring it claims to", literal)
	}

	const want = "AgentNamespacePrefix: cfg.AgentNamespacePrefix,"
	if !strings.Contains(text, want) {
		t.Errorf("main.go does not contain %q.\n"+
			"    Without it every agent row records agents.NamespacePrefix+name (the package "+
			"default) while the driver places the instance under the prefix k8sDriverConfig gave "+
			"it from the SAME config field. Nothing fails when those disagree — that is the "+
			"defect this guard exists for: a row saying `devpod-<name>` while the cluster holds "+
			"`muster-agent-<name>`.\n"+
			"    If the wiring moved, move this assertion with it; do not delete it.", want)
	}
}

// TestAStalePrivilegeApplyVariableChangesNothing: the live manifest still sets
// MUSTER_AGENT_PRIVILEGE_APPLY=1 beside MUSTER_AGENT_PROVISIONER=kubernetes. The
// privilege tier is removed, so the binary must load and validate that
// environment, and the resulting config must be IDENTICAL to the one built
// without the variable — it is ignored like any unknown MUSTER_* name.
func TestAStalePrivilegeApplyVariableChangesNothing(t *testing.T) {
	base := map[string]string{
		envAgentProvisioner: provisionerK8s,
		envAgentImageRepo:   "registry.example.test/muster/agent-runtime",
		envAgentAPIURL:      "http://muster.example.test:8105",
		envDatabase:         "postgres://unused",
	}
	load := func(extra map[string]string) config {
		t.Helper()
		cfg, err := loadConfig(func(n string) string {
			if v, ok := extra[n]; ok {
				return v
			}
			return base[n]
		})
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		if err := cfg.validate(); err != nil {
			t.Fatalf("validate: %v", err)
		}
		return cfg
	}
	with := load(map[string]string{"MUSTER_AGENT_PRIVILEGE_APPLY": "1"})
	without := load(nil)
	if !reflect.DeepEqual(with, without) {
		t.Fatalf("MUSTER_AGENT_PRIVILEGE_APPLY=1 changed the loaded config:\nwith:    %+v\nwithout: %+v", with, without)
	}
	if with.agentProvisioner() != provisionerK8s {
		t.Fatalf("control: the provisioner did not load (%q); the comparison is over an empty config", with.agentProvisioner())
	}
}
