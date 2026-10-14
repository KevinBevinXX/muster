package agentprovision

import (
	"context"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

// 🔴 THIS FILE IS A SEAM GUARD, AND THE SEAM IS WHY IT EXISTS AT ALL.
//
// The subsystem-store integration is split across three surfaces that are each
// hermetically tested somewhere else: internal/agentspec builds the spec,
// internal/agentspec builds the prose, and THIS package decides which agent gets
// either. All three can be green in isolation and the combination still broken,
// because no test in either of the other two ever builds the combined state — the
// prose is an opaque input to Build, and Build is invisible to a prose test.
//
// The defect the seam admits is specific: a supervisor handed the WITH-store prose
// and a spec with no credential (or the reverse). The first tells the agent it has
// read+write access across every scope of the operator's knowledge store when it
// has none; the client then refuses locally, and the agent reports a working
// subsystem as broken. Nothing in agentspec can catch it, because agentspec is
// handed the prose already chosen.
//
// So the guards below pin RELATIONSHIPS — which agent, which prose, which spec, all
// in one assertion — plus a behavioural case, because a structural check
// type-checks past a wrong argument.

// storeSpecConfig is fixtureSpecConfig with the store configured.
//
// ⚠ THE COORDINATES ARE PAIRWISE DISTINCT AND MATCH NO CONSTANT THIS PACKAGE OR
// agentspec NAMES, so a mutant that hardcoded one cannot pass by coincidence.
func storeSpecConfig() agentspec.Config {
	cfg := fixtureSpecConfig()
	cfg.CairnURL = "https://store.example.test:19001"
	cfg.CairnToken = "seam-fixture-store-credential-51ac9d"
	return cfg
}

// supervisorRow is the one agent eligible for the credential.
func supervisorRow() agents.Agent {
	ag := fixtureAgent()
	ag.Name = agents.ChiefName
	ag.Namespace = agents.NamespaceFor(agents.NamespacePrefix, agents.ChiefName)
	return ag
}

// adapterWith builds an adapter over a recording store and the Noop driver with a
// given spec config.
func adapterWith(t *testing.T, spec agentspec.Config, ag agents.Agent) *Adapter {
	t.Helper()
	store := &recordingStore{agent: ag}
	a, err := New(Config{
		Driver: &flakyDriver{Noop: provision.MustNewNoop(), rec: store},
		Store:  store,
		Spec:   spec,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// hasCredential reports whether the spec carries the store credential file.
func hasCredential(spec provision.Spec) bool {
	for _, f := range spec.Files {
		if f.Path == agentspec.CairnConfigPath {
			return true
		}
	}
	return false
}

// claimsTheStore reports whether the instruction file in the spec makes the
// capability claim.
//
// 🔴 IT READS THE PROSE OUT OF THE BUILT SPEC, NOT OUT OF THE CONSTANT. The
// question this file asks is what the INSTANCE receives, and reading the constant
// directly would answer a different question — one agentspec's own tests already
// answer.
func claimsTheStore(t *testing.T, spec provision.Spec) bool {
	t.Helper()
	const theClaim = "Your credential is READ AND WRITE across ALL scopes."
	for _, f := range spec.Files {
		if !strings.HasSuffix(f.Path, agentspec.InstructionsFileName) {
			continue
		}
		return strings.Contains(string(f.Content), theClaim)
	}
	t.Fatalf("the built spec has no %s at all, so this guard cannot read the prose the instance "+
		"receives; files: %v", agentspec.InstructionsFileName, filePaths(spec))
	return false
}

func filePaths(spec provision.Spec) []string {
	out := make([]string, 0, len(spec.Files))
	for _, f := range spec.Files {
		out = append(out, f.Path)
	}
	return out
}

// TestTheSupervisorsProseAndItsSpecAgreeAboutTheStore is the seam guard proper.
//
// 🔴 IT ASSERTS ONE BICONDITIONAL OVER TWO SURFACES: the prose claims the
// credential exactly when the spec carries it. Either half alone is satisfiable by
// a broken build — a spec with a credential and worker prose is a wasted secret, and
// worker-eligible prose over a credential-bearing spec is a supervisor that is
// never told it can write.
//
// The matrix is deliberately 2x2 over the two INDEPENDENT axes: whether the row is
// the supervisor, and whether the installation configured a store. A test over one
// axis cannot see a build that gated on the wrong one.
func TestTheSupervisorsProseAndItsSpecAgreeAboutTheStore(t *testing.T) {
	worker := fixtureAgent()
	if worker.Name == agents.ChiefName {
		t.Fatal("the worker fixture IS the supervisor, so the two axes below collapse into one")
	}

	cases := []struct {
		name       string
		agent      agents.Agent
		spec       agentspec.Config
		wantStore  bool
		wantClaims bool
	}{
		{"supervisor, store configured", supervisorRow(), storeSpecConfig(), true, true},
		{"supervisor, no store", supervisorRow(), fixtureSpecConfig(), false, false},
		{"worker, store configured", worker, storeSpecConfig(), false, false},
		{"worker, no store", worker, fixtureSpecConfig(), false, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := adapterWith(t, c.spec, c.agent)
			spec, err := a.buildSpec(context.Background(), c.agent)
			if err != nil {
				t.Fatalf("buildSpec: %v", err)
			}

			gotStore := hasCredential(spec)
			gotClaims := claimsTheStore(t, spec)

			if gotStore != c.wantStore {
				t.Errorf("spec carries the store credential = %v, want %v", gotStore, c.wantStore)
			}
			if gotClaims != c.wantClaims {
				t.Errorf("the prose claims read+write store access = %v, want %v", gotClaims, c.wantClaims)
			}
			// 🔴 THE BICONDITIONAL, stated separately from the two expectations so
			// that a row whose expectations were both wrong in the same direction
			// still fails here.
			if gotStore != gotClaims {
				t.Errorf("the spec and the prose DISAGREE: credential present = %v, prose claims it "+
					"= %v.\nA supervisor told it holds a read+write key it does not have reports a "+
					"working subsystem as broken, and sends whoever reads that report to the store's "+
					"auth layer where there is nothing to find.", gotStore, gotClaims)
			}
		})
	}
}

// TestOnlyTheSupervisorIsEligibleForTheStoreCredential is the scoping half.
//
// 🔴 THE CREDENTIAL IS READ+WRITE ACROSS EVERY SCOPE. A dispatched worker runs a
// model over somebody's repository contents, so handing it one widens the blast
// radius of a bad turn or a prompt injection from "one branch on one repo" to "the
// operator's curated knowledge base" — which nothing in this system would notice
// and no test could restore.
//
// ⚠ IT CHECKS THE INSTALL STEP AND THE ENVIRONMENT TOO, not only the credential.
// An install without a credential is not a leak, but it is a startup budget spent
// on a command that can only refuse — and it would mean the gate had moved to one
// surface instead of covering all of them.
func TestOnlyTheSupervisorIsEligibleForTheStoreCredential(t *testing.T) {
	cfg := storeSpecConfig()

	// Positive control FIRST: the supervisor's spec must carry all of it, or the
	// absences below are indistinguishable from an integration that is simply off.
	sup := adapterWith(t, cfg, supervisorRow())
	supSpec, err := sup.buildSpec(context.Background(), supervisorRow())
	if err != nil {
		t.Fatalf("buildSpec(supervisor): %v", err)
	}
	if !hasCredential(supSpec) || len(supSpec.Init) == 0 {
		t.Fatalf("positive control: the supervisor's own spec has credential=%v init=%d, so the "+
			"worker assertions below would pass for a build that emits nothing at all",
			hasCredential(supSpec), len(supSpec.Init))
	}

	worker := fixtureAgent()
	a := adapterWith(t, cfg, worker)
	spec, err := a.buildSpec(context.Background(), worker)
	if err != nil {
		t.Fatalf("buildSpec(worker): %v", err)
	}

	if hasCredential(spec) {
		t.Errorf("a dispatched worker's spec carries the store credential at %s", agentspec.CairnConfigPath)
	}
	for _, e := range spec.Env {
		if e.Name == agentspec.EnvCairnConfig {
			t.Errorf("a dispatched worker's environment names %s", agentspec.EnvCairnConfig)
		}
	}
	if len(spec.Init) != 0 {
		t.Errorf("a dispatched worker got %d Init entr(ies); with no repository and no store "+
			"eligibility there is nothing imperative to run:\n%#v", len(spec.Init), spec.Init)
	}
	// And the credential's bytes reached nothing, including a surface this test does
	// not know the name of.
	for _, f := range spec.Files {
		if strings.Contains(string(f.Content), cfg.CairnToken) {
			t.Errorf("the store credential's bytes appear in a worker's file at %s", f.Path)
		}
	}
}

// TestAHalfConfiguredStoreGatesTheProseAndTheSpecTheSameWay is the behavioural half
// of the "one predicate" rule.
//
// 🔴 THE NAME SAYS WHAT THE BODY CHECKS, AND IT DID NOT USED TO. This was
// TestTheSupervisorGateIsReadOnceRatherThanComputedTwice, which claimed a
// SINGLE-READ relationship in the source. Nothing below reads the source, so the
// name was a coverage claim the body never made — and a guard that reads as covering
// something is worse than no guard, because it stops the next person looking.
//
// What it actually pins is BEHAVIOUR: across the inputs that DISCRIMINATE between
// the candidate expressions, the prose and the spec move together. The discriminating
// inputs are the half-configured ones — a URL with no token, a token with no URL,
// and each blank-but-present — for which `CairnConfigured` says false while
// `cfg.CairnURL != ""` or `cfg.CairnToken != ""` alone would say true. That is the
// hazard that matters: the prose gated by a DIFFERENT expression than the spec, so a
// supervisor is told it holds a credential the instance does not carry.
//
// ⚠ AND THE "COMPUTED TWICE" MUTANT IS NOT A HAZARD, WHICH IS WHY NO TEST HERE
// CATCHES IT. Replacing `store := a.spec.CairnConfigured()` + `ChiefInstructions(store)`
// with an inlined `ChiefInstructions(a.spec.CairnConfigured())` is the SAME
// EXPRESSION evaluated twice: it is semantically equivalent, changes no output, and
// a sweep confirmed it survives every test in these three packages. An equivalent
// mutant surviving is correct behaviour for a behavioural suite, not a gap — and the
// only way to kill it would be to grep this repository's own source text, which is a
// spelled guard on a formatting choice. Mutants that change the EXPRESSION do die
// here; those are the ones that change what an instance receives.
func TestAHalfConfiguredStoreGatesTheProseAndTheSpecTheSameWay(t *testing.T) {
	full := storeSpecConfig()

	for _, c := range []struct {
		name string
		tune func(*agentspec.Config)
	}{
		{"url only", func(c *agentspec.Config) { c.CairnToken = "" }},
		{"token only", func(c *agentspec.Config) { c.CairnURL = "" }},
		{"blank token", func(c *agentspec.Config) { c.CairnToken = "   " }},
		{"blank url", func(c *agentspec.Config) { c.CairnURL = "\t" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := full
			c.tune(&cfg)

			a := adapterWith(t, cfg, supervisorRow())
			spec, err := a.buildSpec(context.Background(), supervisorRow())
			if err != nil {
				t.Fatalf("buildSpec: %v", err)
			}
			if hasCredential(spec) {
				t.Errorf("a half-configured store (%s) produced a credential file; a URL with no "+
					"token has nothing to authenticate with and a token with no URL has nowhere to "+
					"go", c.name)
			}
			if claimsTheStore(t, spec) {
				t.Errorf("a half-configured store (%s) still produced prose claiming read+write "+
					"access — the prose is being gated by a DIFFERENT expression than the spec", c.name)
			}
		})
	}

	// Positive control: the fully-configured case must produce both, or every row
	// above passes because nothing ever produces anything.
	a := adapterWith(t, full, supervisorRow())
	spec, err := a.buildSpec(context.Background(), supervisorRow())
	if err != nil {
		t.Fatalf("buildSpec(full): %v", err)
	}
	if !hasCredential(spec) || !claimsTheStore(t, spec) {
		t.Fatalf("positive control: fully configured produced credential=%v claims=%v",
			hasCredential(spec), claimsTheStore(t, spec))
	}
}

// TestOnlyTheSupervisorIsToldAboutPluginsAndOnlyWhenTheyAreOn is the plugin
// section's seam: the prose the INSTANCE receives (read out of the built spec)
// names task_create_gated exactly when the row is the supervisor AND the
// deployment has plugins on. 2x2 over the two independent axes, for the same
// reason as the store matrix above.
func TestOnlyTheSupervisorIsToldAboutPluginsAndOnlyWhenTheyAreOn(t *testing.T) {
	worker := fixtureAgent()
	on := fixtureSpecConfig()
	on.Plugins = true
	const marker = "`task_create_gated`"
	for _, c := range []struct {
		name  string
		agent agents.Agent
		spec  agentspec.Config
		want  bool
	}{
		{"supervisor, plugins on", supervisorRow(), on, true},
		{"supervisor, plugins off", supervisorRow(), fixtureSpecConfig(), false},
		{"worker, plugins on", worker, on, false},
		{"worker, plugins off", worker, fixtureSpecConfig(), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			spec, err := adapterWith(t, c.spec, c.agent).buildSpec(context.Background(), c.agent)
			if err != nil {
				t.Fatalf("buildSpec: %v", err)
			}
			found := false
			for _, f := range spec.Files {
				if strings.HasSuffix(f.Path, agentspec.InstructionsFileName) {
					found = true
					if got := strings.Contains(string(f.Content), marker); got != c.want {
						t.Errorf("the instance's prose names %s = %v, want %v", marker, got, c.want)
					}
				}
			}
			if !found {
				t.Fatalf("no %s in the built spec; files: %v", agentspec.InstructionsFileName, filePaths(spec))
			}
		})
	}
}
