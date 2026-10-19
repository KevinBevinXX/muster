package agentspec

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// 🔴 THIS FILE HAS TWO KINDS OF ASSERTION AND THEY ARE NOT INTERCHANGEABLE.
//
//  1. HAND-WRITTEN CONTRACT ASSERTIONS. Every literal below was written from the
//     wire contract — cmd/muster's env var names, provision.Spec's Env/Secrets
//     split, plan step 22a's requirements — and NOT by running Build and copying
//     what came out. That distinction is the whole point: an expectation derived
//     from the implementation passes for any implementation, including a wrong
//     one. These are what actually test the package.
//
//  2. ONE REGRESSION GOLDEN (TestTheBuiltSpecMatchesItsGolden). Its file WAS
//     produced by running the code, and it is therefore evidence about CHANGE,
//     never about correctness. It exists to make an unintended edit loud. It is
//     labelled as such in its own failure message so nobody reads a green golden
//     as a correctness claim.
//
// 🔴 THE FIXTURE'S VALUES ARE PAIRWISE DISTINCT AND DISTINCT FROM EVERY CONSTANT
// THIS FILE NAMES, and that is a requirement rather than tidiness. A fixture that
// can only ever produce a constant's own value cannot see a mutant that hardcodes
// that constant — it survives a fully green suite. See
// TestTheFixtureCanSeeAHardcodedConstant, which is the mechanical control for it:
// it feeds a value the constant CANNOT equal and watches the output move.

// fixtureAgent is the agent row every test builds from unless it says otherwise.
//
// Note what is deliberately unusual about it: the name is not "chief", the
// workspace is not the default, the repo is not muster's own, and no field
// repeats another field's value. Each of those is a place a mutant could hide
// behind a coincidence.
func fixtureAgent() agents.Agent {
	return agents.Agent{
		ID:          4291,
		Name:        "harbour-kestrel",
		Namespace:   "devpod-harbour-kestrel",
		DisplayName: "Harbour Kestrel",
		Repo:        "example-org/tide-charts",
		RepoBranch:  "release/7.2",
		Model:       "openrouter/vendor-a/model-one",
		HooksToken:  "fixture-per-agent-token-9f31c7",
	}
}

// fixtureConfig pairs with fixtureAgent. Every string differs from every other
// string here, and none of them equals DefaultWorkspacePath or DefaultImageTag —
// both of which are constants an assertion below names.
func fixtureConfig() Config {
	return Config{
		ImageRepo:        "ghcr.io/example-org/agent-runtime",
		ImageTag:         "3.11.0",
		APIBaseURL:       "http://muster.example.test:8105",
		OpenRouterAPIKey: "fixture-shared-provider-key-2b84af",
		Model:            "openrouter/vendor-b/model-two",
		ModelFallbacks:   []string{"openrouter/vendor-c/model-three", "openrouter/vendor-d/model-four"},
		MemoryRequest:    "384Mi",
		MemoryLimit:      "1536Mi",
		CPURequest:       "150m",
		CPULimit:         "1250m",
		NodeOptions:      "--max-old-space-size=3072",
		// 🔴 NOT DefaultGatewayPort, AND NOT A NEIGHBOUR OF IT. A fixture that
		// could only ever produce the constant's own value cannot see a mutant
		// that hardcodes the literal — the same property this file's
		// TestTheFixtureCanSeeAHardcodedConstant pins for the workspace path and
		// the image tag, extended to the one numeric field. It also differs from
		// every other number spelled in this fixture (the agent id, the heap size,
		// the base URL's port), so a mutant that read the wrong field moves the
		// output visibly rather than landing on a coincidence.
		GatewayPort:      21473,
		WorkspacePath:    "/srv/agent-work",
		WorkspaceSize:    "24Gi",
		WorkspacePersist: true,
		Labels:           map[string]string{"installation": "example-test"},
		// 🔴 THE BUNDLE IS IN THE SHARED FIXTURE, NOT ONLY IN ITS OWN TESTS, AND
		// THAT IS DELIBERATE: THIS IS THE SHAPE A DEPLOYMENT RUNS. Every deployment
		// that names a runtime is REQUIRED to supply it (cmd/muster-server's
		// validateProvisioner refuses otherwise), so a fixture without it pins a
		// spec no production deployment produces — and the golden's whole value is
		// that a one-field regression in the real shape is visible.
		//
		// ⚠ THE DERIVATION IS NOT sha256, DELIBERATELY. agentgateway.HooksSHA256 is
		// the real one and is not importable here (see runtimeconfig.go on the HTTP
		// client); a fixture that reproduced its formula would make this package's
		// assertions pass over a Build that called the WRONG function with the right
		// algorithm. A derivation that no production code can produce means the
		// asserted value can only have come from this field.
		RuntimeConfig: RuntimeConfig{
			Template:     []byte("{\"example\":{\"nested\":{\"credential\":\"<injected>\"}}}\n"),
			Install:      []byte("set -eu\nexec /example/entrypoint --flag\n"),
			DeriveBearer: fixtureDeriveBearer,
		},
	}
}

// fixtureDeriveBearer is the fixture's stand-in for a real credential scheme.
//
// ⚠ IT IS A PACKAGE-LEVEL FUNCTION RATHER THAN A CLOSURE IN fixtureConfig SO THAT
// A TEST CAN COMPUTE THE EXPECTED VALUE FROM THE SAME SOURCE. An assertion that
// re-spelled the prefix inline would pass over a Build that applied a different
// one, which is the whole failure class [RuntimeConfig.DeriveBearer] exists to
// remove. It is deliberately NOT a hash: see fixtureConfig.
func fixtureDeriveBearer(hooksToken string) string {
	return "fixture-derived-bearer<" + hooksToken + ">"
}

func mustBuild(t *testing.T, a agents.Agent, cfg Config, opts Options) provision.Spec {
	t.Helper()
	spec, err := Build(a, cfg, opts)
	if err != nil {
		t.Fatalf("Build: unexpected error: %v", err)
	}
	return spec
}

func envValue(spec provision.Spec, name string) (string, bool) {
	for _, e := range spec.Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

func secretValue(spec provision.Spec, name string) (string, bool) {
	for _, e := range spec.Secrets {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// --------------------------------------------------------------------------
// The one assertion that would have caught the defect this package exists to
// prevent.
// --------------------------------------------------------------------------

// TestNoSecretIsCarriedInTheNonSensitiveEnvironment pins the Env/Secrets split.
//
// 🔴 IT ASSERTS A RELATIONSHIP OVER BOTH LISTS, NOT THE PRESENCE OF TWO NAMES.
// A test that only checked "MUSTER_HOOK_TOKEN is in Secrets" would stay green if
// a future edit ALSO left a copy in Env — which is the actual hazard, because
// provision.Spec's whole reason for having two fields is that a driver may treat
// them differently. So this walks Env looking for any secret VALUE, by value and
// not by name: a leak that renamed the variable would still be caught.
func TestNoSecretIsCarriedInTheNonSensitiveEnvironment(t *testing.T) {
	a, cfg := fixtureAgent(), fixtureConfig()
	spec := mustBuild(t, a, cfg, Options{})

	secretValues := map[string]string{
		"the agent's own per-agent token": a.HooksToken,
		"the shared provider key":         cfg.OpenRouterAPIKey,
	}
	// Control: both values must be non-empty, or the loop below proves nothing.
	for what, v := range secretValues {
		if v == "" {
			t.Fatalf("fixture is broken: %s is empty, so this test cannot detect it in Env", what)
		}
	}

	for _, e := range spec.Env {
		for what, v := range secretValues {
			if e.Value == v {
				t.Errorf("Env carries %s as %s=%q — it belongs in Secrets, where a driver cannot treat it as ordinary environment", what, e.Name, e.Value)
			}
			if strings.Contains(e.Value, v) {
				t.Errorf("Env entry %s embeds %s inside a larger value", e.Name, what)
			}
		}
	}

	// And the positive half: each one IS in Secrets, at its contracted name.
	if got, ok := secretValue(spec, EnvToken); !ok || got != a.HooksToken {
		t.Errorf("Secrets[%s] = %q, ok=%v; want the agent's own token", EnvToken, got, ok)
	}
	if got, ok := secretValue(spec, EnvOpenRouterKey); !ok || got != cfg.OpenRouterAPIKey {
		t.Errorf("Secrets[%s] = %q, ok=%v; want the shared provider key", EnvOpenRouterKey, got, ok)
	}
}

// --------------------------------------------------------------------------
// The wire contract with cmd/muster.
// --------------------------------------------------------------------------

// TestThePodEnvironmentUsesTheNamesTheInPodCLIActuallyReads is the assertion that
// makes a rename loud.
//
// The literals here are copied from cmd/muster/config.go's envAPIURL/envToken —
// deliberately spelled out rather than referenced through this package's own
// constants, because comparing EnvAPIURL to EnvAPIURL compares the
// implementation to itself and passes for any value, including a typo.
func TestThePodEnvironmentUsesTheNamesTheInPodCLIActuallyReads(t *testing.T) {
	if EnvAPIURL != "MUSTER_API_URL" {
		t.Errorf("EnvAPIURL = %q, want MUSTER_API_URL — the name cmd/muster's envAPIURL reads", EnvAPIURL)
	}
	if EnvToken != "MUSTER_HOOK_TOKEN" {
		t.Errorf("EnvToken = %q, want MUSTER_HOOK_TOKEN — the name cmd/muster's envToken reads", EnvToken)
	}

	cfg := fixtureConfig()
	spec := mustBuild(t, fixtureAgent(), cfg, Options{})
	if got, _ := envValue(spec, "MUSTER_API_URL"); got != cfg.APIBaseURL {
		t.Errorf("Env[MUSTER_API_URL] = %q, want Config.APIBaseURL %q", got, cfg.APIBaseURL)
	}
}

// TestThereIsExactlyOneBaseURLInThePodEnvironment pins the narrowing muster made
// deliberately: upstream injected a router URL AND a task URL, and the step-22
// cutover made them diverge. muster serves both families itself.
//
// 🔴 IT IS A LEDGER OVER THE WHOLE ENV, NOT A CHECK THAT ONE NAME IS ABSENT.
// Asserting the upstream's own task-side variable name is missing would be walkable by adding
// `MUSTER_TASK_API_URL` instead. This counts every variable whose value is a URL
// and requires the set to be exactly one.
func TestThereIsExactlyOneBaseURLInThePodEnvironment(t *testing.T) {
	spec := mustBuild(t, fixtureAgent(), fixtureConfig(), Options{})

	var urlVars []string
	for _, e := range spec.Env {
		if strings.HasPrefix(e.Value, "http://") || strings.HasPrefix(e.Value, "https://") {
			urlVars = append(urlVars, e.Name)
		}
	}
	if len(urlVars) != 1 {
		t.Errorf("env carries %d URL-valued variables (%v); want exactly 1. Two base URLs is the upstream shape muster narrowed away from — if a second is genuinely needed, the argument in cmd/muster/config.go has to be revisited first", len(urlVars), urlVars)
	}
	// Control: the count above is only meaningful if the detector can see the
	// one URL we know is there.
	if len(urlVars) == 0 {
		t.Fatal("detector found no URL-valued variable at all, so its zero says nothing about a second one")
	}
}

// TestGitTerminalPromptIsPinnedOffUnconditionally guards a hang, not a style.
func TestGitTerminalPromptIsPinnedOffUnconditionally(t *testing.T) {
	// Including for an agent with no repo at all: the guard costs nothing and a
	// conditional one is a guard that is absent exactly when someone clones by
	// hand.
	for _, name := range []string{"with a repo", "with no repo"} {
		a := fixtureAgent()
		if name == "with no repo" {
			a.Repo, a.RepoBranch = "", ""
		}
		spec := mustBuild(t, a, fixtureConfig(), Options{})
		got, ok := envValue(spec, "GIT_TERMINAL_PROMPT")
		if !ok || got != "0" {
			t.Errorf("%s: Env[GIT_TERMINAL_PROMPT] = %q, ok=%v; want \"0\" so a clone against an unreadable repo FAILS instead of blocking with nobody to answer", name, got, ok)
		}
	}
}

// --------------------------------------------------------------------------
// Files are data. This is the step's headline change.
// --------------------------------------------------------------------------

// TestInstructionsAreAFileAndNotAShellCommand is the assertion that would fail if
// anyone reintroduced the upstream base64 form.
func TestInstructionsAreAFileAndNotAShellCommand(t *testing.T) {
	const body = "# Instructions\n\nA distinctive sentence no constant in this package spells.\n"
	cfg := fixtureConfig()
	spec := mustBuild(t, fixtureAgent(), cfg, Options{Instructions: body})

	wantPath := "/srv/agent-work/AGENTS.md" // literal, not path.Join'd from the impl
	var found *provision.File
	for i := range spec.Files {
		if spec.Files[i].Path == wantPath {
			found = &spec.Files[i]
		}
	}
	if found == nil {
		var paths []string
		for _, f := range spec.Files {
			paths = append(paths, f.Path)
		}
		t.Fatalf("no file at %s; got %v", wantPath, paths)
	}
	if string(found.Content) != body {
		t.Errorf("instruction file content = %q, want the input verbatim", string(found.Content))
	}

	// 🔴 AND THE NEGATIVE HALF, WHICH IS THE ONE THAT PINS THE CHANGE: the
	// content must not have leaked into Init as a shell string. Checking only
	// that the File exists would stay green if Build emitted BOTH.
	//
	// ⚠ THIS GUARD WAS A SPELLED ONE AND IT FIRED FALSELY, WHICH IS RECORDED
	// RATHER THAN QUIETLY FIXED. It used to reject any Init entry CONTAINING the
	// string "AGENTS.md" — and the autosave daemon's invocation legitimately names
	// `/AGENTS.md` in its exclude list, so adding the daemon turned a correct
	// change into a red test. A guard on a WORD is walkable and also false-fires
	// on a word used for another purpose. The hazard is not the filename appearing;
	// it is content being WRITTEN by shell. So: reject the body's own bytes, and
	// reject a redirection into the instructions path.
	for _, cmd := range spec.Init {
		if strings.Contains(cmd, "base64") {
			t.Errorf("Init entry uses base64 to place content: %q — placing content belongs in Files, where it is data", cmd)
		}
		if strings.Contains(cmd, body) {
			t.Errorf("Init entry carries the instruction document's own content: %q", cmd)
		}
		if strings.Contains(cmd, "> "+wantPath) || strings.Contains(cmd, ">"+wantPath) {
			t.Errorf("Init entry writes the instruction document with shell redirection: %q", cmd)
		}
	}
}

// TestFileOrderIsDeterministicAcrossBuilds guards against the map-iteration
// flake that would make the golden intermittently red.
//
// ⚠ ONE BUILD CANNOT DETECT THIS, so it builds repeatedly. Go randomises map
// iteration per range statement, so a single comparison can agree by luck; the
// loop count is what turns luck into a vanishing probability (2 seeds would agree
// ~1/6 of the time for 3 keys, 32 effectively never).
func TestFileOrderIsDeterministicAcrossBuilds(t *testing.T) {
	opts := Options{
		Instructions: "instructions body",
		SeedFiles: map[string]string{
			"ZULU.md":   "zulu",
			"alpha.md":  "alpha",
			"MIKE.md":   "mike",
			"bravo.txt": "bravo",
		},
	}
	first := mustBuild(t, fixtureAgent(), fixtureConfig(), opts)
	firstPaths := filePaths(first)

	// Control: the fixture must produce more than one seed file, or "the order
	// is stable" is trivially true and this test proves nothing.
	if len(firstPaths) < 3 {
		t.Fatalf("fixture produced %d files; need at least 3 for ordering to be observable", len(firstPaths))
	}

	for i := 0; i < 32; i++ {
		got := filePaths(mustBuild(t, fixtureAgent(), fixtureConfig(), opts))
		if strings.Join(got, ",") != strings.Join(firstPaths, ",") {
			t.Fatalf("build %d produced a different file order:\n first: %v\n  this: %v", i, firstPaths, got)
		}
	}

	// And the order is the contracted one: instructions first, then seeds sorted
	// by name. Spelled out literally rather than recomputed with sort.Strings,
	// which would be the implementation checking itself.
	// ⚠ THE DAEMON IS LAST, AFTER THE SORTED SEEDS, AND THAT ORDER IS PART OF THE
	// CLAIM. It is appended by Build rather than sorted in with the workspace
	// files, so a reader of a spec sees "the caller's content, then the
	// installation's own machinery" — and the position is deterministic either way,
	// which is what this test exists to pin.
	// ⚠ AND THE RUNTIME-CONFIG BUNDLE SITS BETWEEN THE SEEDS AND THE DAEMON,
	// WHICH IS ALSO PART OF THE CLAIM RATHER THAN AN ACCIDENT OF APPEND ORDER.
	// buildFiles emits it last; Build then appends the daemon. Both are
	// "installation machinery" rather than caller content, so they follow the
	// sorted workspace files — and the bundle's paths are deliberately OUTSIDE
	// the workspace, which is why they are not sorted among them.
	want := []string{
		"/srv/agent-work/AGENTS.md",
		"/srv/agent-work/MIKE.md",
		"/srv/agent-work/ZULU.md",
		"/srv/agent-work/alpha.md",
		"/srv/agent-work/bravo.txt",
		RuntimeConfigPath,
		RuntimeInstallPath,
		"/usr/local/bin/muster-autosave",
	}
	if strings.Join(firstPaths, ",") != strings.Join(want, ",") {
		t.Errorf("file order = %v, want %v", firstPaths, want)
	}
}

func filePaths(spec provision.Spec) []string {
	var out []string
	for _, f := range spec.Files {
		out = append(out, f.Path)
	}
	return out
}

func TestASeedFileNameWithAPathSeparatorIsRefused(t *testing.T) {
	_, err := Build(fixtureAgent(), fixtureConfig(), Options{
		SeedFiles: map[string]string{"nested/thing.md": "x"},
	})
	if err == nil {
		t.Fatal("Build accepted a seed file name containing a path separator; want an error")
	}
	if !strings.Contains(err.Error(), "path separator") {
		t.Errorf("error does not explain the problem: %v", err)
	}
}

func TestASeedFileCollidingWithTheInstructionsFileIsRefused(t *testing.T) {
	_, err := Build(fixtureAgent(), fixtureConfig(), Options{
		Instructions: "body",
		SeedFiles:    map[string]string{"AGENTS.md": "a second body"},
	})
	if err == nil {
		t.Fatal("Build accepted a seed file at the instruction file's own path; want an error rather than one silently winning")
	}

	// The same key is FINE when there are no instructions to collide with —
	// otherwise this guard would forbid a legitimate caller.
	spec, err := Build(fixtureAgent(), fixtureConfig(), Options{
		SeedFiles: map[string]string{"AGENTS.md": "a hand-written body"},
	})
	if err != nil {
		t.Fatalf("Build refused a seed AGENTS.md with no Instructions set: %v", err)
	}
	// Find it by PATH rather than by index: Build also emits the autosave daemon,
	// so a positional assertion would be pinning the append order of unrelated
	// machinery instead of the caller's file.
	var seeded *provision.File
	for i := range spec.Files {
		if spec.Files[i].Path == "/srv/agent-work/AGENTS.md" {
			seeded = &spec.Files[i]
		}
	}
	if seeded == nil {
		t.Fatalf("the caller's own AGENTS.md is missing; spec carries %v", filePaths(spec))
	}
	if string(seeded.Content) != "a hand-written body" {
		t.Errorf("content = %q, want the caller's own body", string(seeded.Content))
	}
}

// --------------------------------------------------------------------------
// Errors that are better here than inside a driver.
// --------------------------------------------------------------------------

func TestTheTwoRequiredConfigFieldsAreRefusedWhenEmpty(t *testing.T) {
	for _, tc := range []struct {
		what string
		cfg  Config
		want string
	}{
		{"no image repo", Config{APIBaseURL: "http://x.test"}, "ImageRepo"},
		{"no base URL", Config{ImageRepo: "r.test/a/b"}, "APIBaseURL"},
		{"whitespace-only image repo", Config{ImageRepo: "   ", APIBaseURL: "http://x.test"}, "ImageRepo"},
	} {
		_, err := Build(fixtureAgent(), tc.cfg, Options{})
		if err == nil {
			t.Errorf("%s: Build succeeded; want an error naming %s", tc.what, tc.want)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not name %s", tc.what, err, tc.want)
		}
	}
}

// emittedEnvNames returns every variable name a built spec actually carries into
// the instance — Env and Secrets together, with every optional branch turned on.
//
// 🔴 IT DERIVES FROM BEHAVIOUR, AND A SOURCE-PARSING VERSION OF THIS WAS MEASURED
// TOO NARROW FOR ITS OWN DOCSTRING. That version read ONE file (`agentspec.go`)
// with ONE declaration form (`Env… = "literal"`) while claiming "the package's own
// Env* declarations". Three isolated mutants — a new constant in cairn.go, one in
// the typed form `EnvX string = "…"`, one whose value is a concatenation — each
// added an unreserved name and SURVIVED the whole suite, because none of them moved
// the fixed floor the instrument check used.
//
// 🔴 AND THE SET IT WAS DERIVING WAS THE WRONG ONE ANYWAY, WHICH IS THE MORE
// USEFUL HALF OF THAT FINDING. The hazard is a name this package COMPUTES being
// shadowable; a declared-but-never-emitted constant is not a shadowing hazard at
// all, so a declaration-based set would have reddened for it and been "fixed" by
// reserving it — silently narrowing what a caller may put in ExtraEnv, for no
// reason. Reading the names off a BUILT SPEC is both wider (any file, any
// declaration form, any computed name — a name that never had a constant at all)
// and narrower in the right direction (only what actually reaches the instance).
//
// ⚠ WHAT IT CANNOT SEE, STATED RATHER THAN IMPLIED: a name emitted only under a
// configuration this function does not build. It turns on every optional branch
// agentspec.Config has today — node options, the provider key, the cairn pair — and
// a future branch keyed on something else would need adding here. That is a real
// limit, and it is a smaller one than the source regex had.
func emittedEnvNames(t *testing.T) map[string]string {
	t.Helper()
	cfg := fixtureConfig()
	// Every optional branch ON, so no emitted name is missed because its gate was
	// off. cairnEnabled needs BOTH coordinates plus per-call eligibility.
	cfg.NodeOptions = "--max-old-space-size=3072"
	cfg.OpenRouterAPIKey = "fixture-shared-provider-key-2b84af"
	cfg.CairnURL = "https://cairn.example.test"
	cfg.CairnToken = "fixture-cairn-token-7c21de"
	a := fixtureAgent()
	a.HooksToken = "fixture-per-agent-token-9f31c7"

	spec, err := Build(a, cfg, Options{CairnEligible: true})
	if err != nil {
		t.Fatalf("Build with every optional branch on: %v", err)
	}
	out := map[string]string{}
	for _, e := range spec.Env {
		out[e.Name] = "Env"
	}
	for _, e := range spec.Secrets {
		out[e.Name] = "Secrets"
	}
	return out
}

// TestExtraEnvMayNotShadowAComputedVariable asserts the RELATIONSHIP, not a sample.
//
// 🔴 THIS TEST USED TO BE THE VERY SHAPE ITS OWN COMMENT WARNED AGAINST, AND HAD
// GONE STALE. It read "Every reserved name, not one sample: a per-name allowlist is
// exactly the shape that goes stale when a constant is added" over a hand-written
// list of five — and by the time anybody measured it, the list was missing
// EnvCairnConfig and EnvGatewayToken. Isolated single-entry mutants: deleting
// `EnvGatewayToken: true` or `EnvCairnConfig: true` from buildEnv's reserved map
// both SURVIVED the full suite; deleting `EnvToken: true` was KILLED. So two of the
// seven reserved names had no guard at all, and the one whose shadow is a CREDENTIAL
// failure — EnvGatewayToken changes the value the container hashes, so the bearer
// stops matching and every chat turn 401s — was one of them.
//
// 🔴 THE FIX IS DERIVATION, NOT TWO MORE NAMES. Adding the missing pair would have
// regenerated the same defect at the next constant. The expected set is read off a
// BUILT SPEC — see emittedEnvNames — so any name that actually reaches the instance
// is covered regardless of which file declares it, in what form, or whether it has
// a constant at all.
//
// ⚠ AN EARLIER FIX DERIVED FROM THE SOURCE AND WAS NARROWER THAN ITS OWN DOCSTRING;
// emittedEnvNames records the three mutants that survived it and why the
// behavioural set is the right one. The failure message below says COMPUTES and now
// means it: the loop enumerates what a spec emits, not what the package declares.
//
// ⚠ IT IS A PROPHYLACTIC GUARD AND THE REACHABILITY IS STATED HONESTLY:
// Options.ExtraEnv has no non-test producer today, so no caller can currently reach
// the shadow. The guard is for whoever writes the first one.
func TestExtraEnvMayNotShadowAComputedVariable(t *testing.T) {
	emitted := emittedEnvNames(t)

	// 🔴 POSITIVE CONTROL, AS A NUMBER. An empty set satisfies "every emitted name is
	// refused" perfectly and reads identically to a clean run — the same silent-zero
	// this file's other instrument checks exist for.
	if len(emitted) < 7 {
		t.Fatalf("instrument check FAILED: a spec built with every optional branch on emits "+
			"only %d variable(s) (%v), and this package computes at least 7. Either a branch "+
			"stopped firing or emittedEnvNames no longer turns them all on, so the loop below "+
			"is over a truncated set.", len(emitted), emitted)
	}
	// And the set must really contain the computed names, not just any names: these
	// two are the ones whose shadow is a CREDENTIAL failure rather than a wrong URL.
	for _, want := range []string{EnvGatewayToken, EnvToken} {
		if _, ok := emitted[want]; !ok {
			t.Fatalf("instrument check FAILED: %q is not among the names a built spec emits "+
				"(%v), so this test is not measuring what it claims", want, emitted)
		}
	}

	for name, list := range emitted {
		_, err := Build(fixtureAgent(), fixtureConfig(), Options{
			ExtraEnv: []provision.EnvVar{{Name: name, Value: "http://attacker.example.test"}},
		})
		if err == nil {
			t.Errorf("ExtraEnv was allowed to set %s (emitted in Spec.%s); want a refusal.\n"+
				"    Every name this package COMPUTES must be reserved in buildEnv: shadowing "+
				"one points the instance at the wrong server, the wrong credential file, or — "+
				"for %s — changes the value the container HASHES, so its half of the chat "+
				"bearer stops matching muster's and every turn is a 401 attributed to the "+
				"credential rather than to the shadow.\n"+
				"    Add it to buildEnv's `reserved` map. Do not add it to a list here; this "+
				"set is read off a built spec precisely so it cannot go stale.",
				name, list, EnvGatewayToken)
		}
	}

	// A name that is genuinely the caller's passes through.
	spec := mustBuild(t, fixtureAgent(), fixtureConfig(), Options{
		ExtraEnv: []provision.EnvVar{{Name: "EXAMPLE_CALLER_FLAG", Value: "on"}},
	})
	if got, ok := envValue(spec, "EXAMPLE_CALLER_FLAG"); !ok || got != "on" {
		t.Errorf("caller's own ExtraEnv entry did not survive: got %q ok=%v", got, ok)
	}
}

// TestAnAgentWhoseNameIsNotAUsableInstanceIdentityIsRefused pins the name check.
//
// 🔴 IT ASSERTS THIS GUARD'S OWN MESSAGE, NOT MERELY THAT AN ERROR HAPPENED, AND
// A MUTATION SWEEP IS WHY. The first version of this test checked only `err !=
// nil`, and deleting the ref.Validate() call in Build SURVIVED it: the final
// spec.Validate() rejects the same names anyway, so the suite stayed green while
// the diagnostic disappeared. Measured, the two messages differ —
//
//	with the guard:    agentspec: agent 4291 has an unusable name: …
//	without it:        agentspec: built an invalid spec for agent "Harbour-Kestrel": …
//
// — and the first is the one that names the ROW, which is what an operator needs
// when a provision attempt fails for an agent they did not create by hand. So the
// guard's whole contribution is the message, and a test that cannot see the
// message cannot see the guard. This is the recorded rule that a mutant must be
// confirmed to fail with THIS guard's specific error rather than any error.
func TestAnAgentWhoseNameIsNotAUsableInstanceIdentityIsRefused(t *testing.T) {
	for _, bad := range []string{"", "Harbour-Kestrel", "-leading", "trailing-", "has space", strings.Repeat("x", 64)} {
		a := fixtureAgent()
		a.Name = bad
		_, err := Build(a, fixtureConfig(), Options{})
		if err == nil {
			t.Errorf("Build accepted agent name %q; provision.Ref.Validate rejects it, and failing here names the agent instead of failing inside a driver", bad)
			continue
		}
		if !strings.Contains(err.Error(), "has an unusable name") {
			t.Errorf("agent name %q was rejected by something OTHER than the name guard: %v\n"+
				"The final spec.Validate() also rejects these, so this test must pin the name guard's own wording or deleting it is invisible.", bad, err)
		}
		// And it names the row, which is the guard's reason for existing.
		if !strings.Contains(err.Error(), "4291") {
			t.Errorf("agent name %q: error %q does not name the agent id, which is the diagnostic this guard adds over spec.Validate()", bad, err)
		}
	}
}

// --------------------------------------------------------------------------
// Aliasing: a built spec must not share mutable state with its inputs.
// --------------------------------------------------------------------------

// TestBuildingASpecDoesNotAliasTheCallersMutableInputs is the kind of defect that
// only ever shows up in production, because a test that builds one spec and
// inspects it immediately can never see it.
// ⚠ THE AGENT MUST NAME NO MODEL OF ITS OWN, AND THAT IS A PRECONDITION OF THE
// SUBJECT RATHER THAN A WORKAROUND. The aliasing this pins is over
// Config.ModelFallbacks, and a per-agent primary DROPS the deployment chain (see
// TestAPerAgentModelDropsTheDeploymentFallbackChain), so with fixtureAgent()'s
// own model set there is no fallback slice in the spec to alias and the test
// would assert nothing. Passing fixtureAgent() unchanged used to "work" only
// because the agent's model was ignored entirely — the defect.
func TestBuildingASpecDoesNotAliasTheCallersMutableInputs(t *testing.T) {
	cfg := fixtureConfig()
	a := fixtureAgent()
	a.Model = ""
	spec := mustBuild(t, a, cfg, Options{})

	// Mutating the caller's slice must not change the built spec.
	cfg.ModelFallbacks[0] = "openrouter/mutated/after-the-fact"
	model, ok := spec.Config[ConfigKeyModel].(map[string]any)
	if !ok {
		t.Fatalf("Spec.Config[%q] is not a map: %#v", ConfigKeyModel, spec.Config[ConfigKeyModel])
	}
	fallbacks, ok := model["fallbacks"].([]string)
	if !ok {
		t.Fatalf("fallbacks is not []string: %#v", model["fallbacks"])
	}
	if fallbacks[0] == "openrouter/mutated/after-the-fact" {
		t.Error("Spec.Config aliases Config.ModelFallbacks — a caller reusing its config would silently rewrite an already-built spec")
	}

	// And building must not write into the caller's label map.
	if _, exists := cfg.Labels["muster.agent/name"]; exists {
		t.Error("Build wrote its own labels into Config.Labels — the caller's map must be left alone")
	}
	if len(cfg.Labels) != 1 {
		t.Errorf("Config.Labels grew from 1 to %d entries during Build", len(cfg.Labels))
	}
}

func TestPerAgentLabelsWinOverInstallationLabels(t *testing.T) {
	cfg := fixtureConfig()
	cfg.Labels = map[string]string{"muster.agent/name": "not-the-agent", "installation": "example-test"}
	spec := mustBuild(t, fixtureAgent(), cfg, Options{})
	if got := spec.Labels["muster.agent/name"]; got != "harbour-kestrel" {
		t.Errorf("labels[muster.agent/name] = %q, want the agent's own name; installation labels must not override per-agent identity", got)
	}
	if got := spec.Labels["installation"]; got != "example-test" {
		t.Errorf("installation label was lost: %q", got)
	}
}

// --------------------------------------------------------------------------
// Repo is DECLARED, not cloned.
// --------------------------------------------------------------------------

func TestTheRepoIsDeclaredAndItsPathIsTheRepositoryName(t *testing.T) {
	a, cfg := fixtureAgent(), fixtureConfig()
	spec := mustBuild(t, a, cfg, Options{})

	if spec.Repo.URL != "example-org/tide-charts" {
		t.Errorf("Repo.URL = %q, want the row's value verbatim", spec.Repo.URL)
	}
	if spec.Repo.Branch != "release/7.2" {
		t.Errorf("Repo.Branch = %q", spec.Repo.Branch)
	}
	if spec.Repo.Path != "/srv/agent-work/tide-charts" {
		t.Errorf("Repo.Path = %q, want the workspace joined with the repository's last segment", spec.Repo.Path)
	}

	// 🔴 Nothing may clone it. provision.Repo's own doc says declared-not-cloned;
	// this asserts Build honours that rather than emitting a clone step.
	for _, cmd := range spec.Init {
		if strings.Contains(cmd, "git clone") || strings.Contains(cmd, "git -C") {
			t.Errorf("Init contains a clone step %q — provision.Repo is declared, not cloned", cmd)
		}
	}
}

func TestAnAgentWithNoRepoGetsAnEmptyRepoRatherThanAPathIntoTheWorkspaceRoot(t *testing.T) {
	a := fixtureAgent()
	a.Repo, a.RepoBranch = "", ""
	spec := mustBuild(t, a, fixtureConfig(), Options{})
	if spec.Repo != (provision.Repo{}) {
		t.Errorf("Repo = %#v, want the zero value", spec.Repo)
	}
}

func TestRepoDirNameHandlesTheShapesTheRowCanHold(t *testing.T) {
	// Each case is a shape that reached this code upstream, plus the trailing
	// slash, which is the one that would otherwise make Repo.Path equal the
	// workspace root and let an agent treat its whole workspace as the checkout.
	for _, tc := range []struct{ in, want string }{
		{"owner/name", "name"},
		{"name", "name"},
		{"owner/name/", "name"},
		{"deep/owner/name", "name"},
		{"", ""},
		{"/", ""},
	} {
		if got := repoDirName(tc.in); got != tc.want {
			t.Errorf("repoDirName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --------------------------------------------------------------------------
// WHICH MODEL A BUILT SPEC NAMES.
//
// 🔴 THE DEFECT THESE PIN, MEASURED ON A LIVE CLUSTER RATHER THAN REASONED
// ABOUT. An agent created with an explicit model had that slug in its database
// row's `model` column and the DEPLOYMENT-WIDE DEFAULT in its pod's
// MUSTER_CONFIG, with nothing anywhere reporting the substitution. The cause
// was structural: buildRuntimeConfig took only the deployment Config, so the
// agent row was not in scope at the one place the primary is chosen, and
// agents.ResolveModel — the function written to own exactly that precedence —
// had ZERO production callers.
// --------------------------------------------------------------------------

// specPrimaryModel digs the primary slug out of a built spec's opaque config.
//
// It returns ok=false for "no model key at all", which is a DIFFERENT state from
// an empty primary and the whole subject of
// TestNoModelMeansNoConfigKeyRatherThanAnEmptyOne.
func specPrimaryModel(t *testing.T, spec provision.Spec) (string, bool) {
	t.Helper()
	raw, present := spec.Config[ConfigKeyModel]
	if !present {
		return "", false
	}
	model, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("Spec.Config[%q] is not a map: %#v", ConfigKeyModel, raw)
	}
	primary, ok := model["primary"].(string)
	if !ok {
		return "", false
	}
	return primary, true
}

// specModelFallbacks digs the fallback chain out of a built spec's opaque config.
func specModelFallbacks(t *testing.T, spec provision.Spec) ([]string, bool) {
	t.Helper()
	raw, present := spec.Config[ConfigKeyModel]
	if !present {
		return nil, false
	}
	model, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("Spec.Config[%q] is not a map: %#v", ConfigKeyModel, raw)
	}
	fallbacks, ok := model["fallbacks"].([]string)
	if !ok {
		return nil, false
	}
	return fallbacks, true
}

// TestThePerAgentModelWinsOverTheDeploymentDefault is the regression guard.
//
// 🔴 RED AT origin/main WITH THE MEASURED SYMPTOM: there the primary is
// fixtureConfig().Model, i.e. the deployment default, exactly as the live pod's
// MUSTER_CONFIG read.
//
// 🔴 IT PINS THE RELATIONSHIP, AND THEN PINS THE LITERAL ANYWAY. The
// relationship assertion (equals agents.ResolveModel over the two inputs) is
// what makes this a statement about the precedence rule rather than about one
// pair of strings. On its own it would be satisfied by a Build and a ResolveModel
// that regressed TOGETHER — deriving an expectation from the implementation it
// tests — so the literal assertions below it say which of the two inputs must
// win, in the test's own words.
//
// ⚠ THE FIXTURE IS WHAT MAKES THIS OBSERVABLE, SO ITS PRECONDITION IS ASSERTED
// AND NOT ASSUMED. Both slugs must be non-empty and DIFFERENT: a fixture whose
// row and default could coincide cannot see this bug at all, and a later
// "tidy-up" that aligned them would leave a green test pinning nothing.
func TestThePerAgentModelWinsOverTheDeploymentDefault(t *testing.T) {
	a, cfg := fixtureAgent(), fixtureConfig()
	if a.Model == "" || cfg.Model == "" || a.Model == cfg.Model {
		t.Fatalf("the fixtures cannot discriminate: agent model %q, deployment default %q. "+
			"Both must be non-empty and different or the defect this test exists for is invisible.",
			a.Model, cfg.Model)
	}

	spec := mustBuild(t, a, cfg, Options{})
	got, ok := specPrimaryModel(t, spec)
	if !ok {
		t.Fatalf("the built spec names NO primary model; Spec.Config = %#v", spec.Config)
	}

	if want := agents.ResolveModel(a.Model, cfg.Model); got != want {
		t.Errorf("the built spec's primary model is %q, but agents.ResolveModel(%q, %q) = %q.\n"+
			"The precedence rule and the spec that is actually provisioned disagree, which is the "+
			"defect: the row's model is stored and served and then dropped at provision time.",
			got, a.Model, cfg.Model, want)
	}
	if got != a.Model {
		t.Errorf("primary model = %q, want the AGENT ROW's own model %q.\n"+
			"An operator who picks a model in the dispatch modal must get that model; silently "+
			"substituting another is the measured defect.", got, a.Model)
	}
	if got == cfg.Model {
		t.Errorf("primary model = %q, which is the DEPLOYMENT-WIDE DEFAULT. The agent row asked "+
			"for %q and was overruled with no warning anywhere.", got, a.Model)
	}
}

// TestAnAgentWithNoModelOfItsOwnGetsTheDeploymentDefault is the other side of the
// same precedence, and it is what stops the fix above from being "always use the
// row".
func TestAnAgentWithNoModelOfItsOwnGetsTheDeploymentDefault(t *testing.T) {
	a, cfg := fixtureAgent(), fixtureConfig()
	a.Model = ""

	spec := mustBuild(t, a, cfg, Options{})
	got, ok := specPrimaryModel(t, spec)
	if !ok {
		t.Fatalf("the built spec names NO primary model; Spec.Config = %#v", spec.Config)
	}
	if want := agents.ResolveModel(a.Model, cfg.Model); got != want {
		t.Errorf("primary model = %q, but agents.ResolveModel(%q, %q) = %q", got, a.Model, cfg.Model, want)
	}
	if got != cfg.Model {
		t.Errorf("primary model = %q, want the deployment default %q. An agent that chose no "+
			"model must inherit the installation's.", got, cfg.Model)
	}
}

// TestAPerAgentModelDropsTheDeploymentFallbackChain pins the FALLBACKS DECISION.
//
// 🔴 IT IS A DECISION AND EITHER ANSWER WOULD HAVE BEEN A REAL BEHAVIOUR, which
// is why it is asserted rather than left to whatever the code happened to do.
// Config.ModelFallbacks is deployment-wide and there is no per-agent fallbacks
// column, so keeping it would pair a hand-picked primary with a chain no
// operator authored — and that chain can route the turn to a DIFFERENT model,
// which defeats the only purpose the per-agent knob has. buildRuntimeConfig's
// comment carries the full reasoning.
func TestAPerAgentModelDropsTheDeploymentFallbackChain(t *testing.T) {
	a, cfg := fixtureAgent(), fixtureConfig()
	if len(cfg.ModelFallbacks) == 0 {
		t.Fatal("the fixture declares no deployment fallbacks, so this test cannot see whether " +
			"they are dropped or kept")
	}

	spec := mustBuild(t, a, cfg, Options{})
	if got, ok := specModelFallbacks(t, spec); ok {
		t.Errorf("an agent naming its own model %q was provisioned with the deployment's fallback "+
			"chain %v. Any entry of that chain is a different model, so a hiccup on the chosen one "+
			"silently reinstates whatever the operator picked %q to avoid.", a.Model, got, a.Model)
	}

	// CONTROL: the chain is NOT simply never written. An agent that chose no model
	// of its own still gets the installation's chain, so the assertion above is
	// about the per-agent case and not about a fallbacks key that went missing.
	noModel := fixtureAgent()
	noModel.Model = ""
	dspec := mustBuild(t, noModel, cfg, Options{})
	got, ok := specModelFallbacks(t, dspec)
	if !ok {
		t.Fatalf("an agent with no model of its own lost the deployment's fallback chain too; "+
			"Spec.Config = %#v", dspec.Config)
	}
	if len(got) != len(cfg.ModelFallbacks) || got[0] != cfg.ModelFallbacks[0] {
		t.Errorf("deployment fallbacks = %v, want %v", got, cfg.ModelFallbacks)
	}
}

// --------------------------------------------------------------------------
// "unset" and "empty" must stay distinguishable.
// --------------------------------------------------------------------------

// TestNoModelMeansNoConfigKeyRatherThanAnEmptyOne pins the contract that unset at
// BOTH levels writes no key, so the runtime decides.
//
// 🔴 IT NOW CLEARS THE AGENT'S MODEL TOO, AND THAT EDIT IS ITSELF EVIDENCE OF THE
// DEFECT. It used to clear only the deployment Config and pass fixtureAgent(),
// which carries a non-empty Model — so "unset at both levels" was never the state
// under test, and the test could only pass because the agent's model was being
// dropped. With the row honoured, an agent that names a model must get a config
// key; the no-key contract is about the case where NOBODY named one.
func TestNoModelMeansNoConfigKeyRatherThanAnEmptyOne(t *testing.T) {
	a := fixtureAgent()
	a.Model = ""
	cfg := fixtureConfig()
	cfg.Model, cfg.ModelFallbacks = "", nil
	spec := mustBuild(t, a, cfg, Options{})
	// Only the workspace (ConfigKeyWorkspace), which is not a model.
	if len(spec.Config) != 1 || spec.Config[ConfigKeyWorkspace] != cfg.WorkspacePath || cfg.WorkspacePath == "" {
		t.Errorf("Spec.Config = %#v, want only the workspace — writing {\"primary\":\"\"} hands the runtime a value it must special-case, which is how unset and empty stop being distinguishable", spec.Config)
	}
}

// TestTheRuntimeIsToldTheWorkspaceItsInstructionsAreIn: the runtime config
// names the workspace AGENTS.md is placed in — the configured path, or the
// default — because the runtime's own default workspace is elsewhere (under its
// HOME), where it never sees muster's instructions. The gateway sidecar, which
// mounts no workspace, is told none.
func TestTheRuntimeIsToldTheWorkspaceItsInstructionsAreIn(t *testing.T) {
	for _, ws := range []string{"/srv/agent-work", ""} {
		cfg := fixtureConfig()
		cfg.WorkspacePath = ws
		spec := mustBuild(t, fixtureAgent(), cfg, Options{Instructions: "prose"})
		want := ws
		if want == "" {
			want = "/data/workspace"
		}
		if got := spec.Config[ConfigKeyWorkspace]; got != want {
			t.Errorf("workspace %q: Spec.Config[%q] = %v, want %q", ws, ConfigKeyWorkspace, got, want)
		}
		var instr string
		for _, f := range spec.Files {
			if strings.HasSuffix(f.Path, "/"+InstructionsFileName) {
				instr = f.Path
			}
		}
		if instr != want+"/AGENTS.md" {
			t.Errorf("workspace %q: AGENTS.md is placed at %q, not in the workspace the runtime is told (%q)", ws, instr, want)
		}
	}
}

// TestAnAgentNamingAModelGetsAConfigKeyEvenWithTheDeploymentUnset is the
// complement, and it is the state the live defect was measured in reverse: a
// deployment that names no model must still provision the agent's own choice.
func TestAnAgentNamingAModelGetsAConfigKeyEvenWithTheDeploymentUnset(t *testing.T) {
	a := fixtureAgent()
	cfg := fixtureConfig()
	cfg.Model, cfg.ModelFallbacks = "", nil

	spec := mustBuild(t, a, cfg, Options{})
	got, ok := specPrimaryModel(t, spec)
	if !ok {
		t.Fatalf("agent %q names model %q and the built spec has no model key at all: %#v.\n"+
			"Omitting the key means \"the runtime decides\", which discards a choice the operator "+
			"made explicitly.", a.Name, a.Model, spec.Config)
	}
	if got != a.Model {
		t.Errorf("primary model = %q, want the agent's own %q", got, a.Model)
	}
}

func TestAnUnsetImageTagFallsBackRatherThanProducingATrailingColon(t *testing.T) {
	cfg := fixtureConfig()
	cfg.ImageTag = ""
	spec := mustBuild(t, fixtureAgent(), cfg, Options{})
	if strings.HasSuffix(spec.Runtime.Image, ":") {
		t.Fatalf("Runtime.Image = %q, which no registry can resolve", spec.Runtime.Image)
	}
	if spec.Runtime.Image != "ghcr.io/example-org/agent-runtime:latest" {
		t.Errorf("Runtime.Image = %q, want the repo with the default tag appended", spec.Runtime.Image)
	}
}

// --------------------------------------------------------------------------
// The control that makes the fixture's distinctness a measurement.
// --------------------------------------------------------------------------

// TestTheFixtureCanSeeAHardcodedConstant is the mechanical control for this
// file's fixture discipline.
//
// 🔴 A GREEN SUITE IS ONLY A CLAIM ABOUT THE MUTANTS SOMEBODY IMAGINED. The one
// that hides best is an implementation that returns a CONSTANT the fixture also
// happens to produce — it survives every assertion, because the expected and the
// wrong value are the same bytes. The defence is not vigilance, it is a fixture
// whose values the constant CANNOT equal.
//
// This test proves that property holds right now instead of trusting it: it feeds
// a workspace path and image tag that differ from DefaultWorkspacePath and
// DefaultImageTag, and asserts the OUTPUT MOVED with them. If someone later
// "simplifies" fixtureConfig to use the defaults, this fails and says why.
func TestTheFixtureCanSeeAHardcodedConstant(t *testing.T) {
	cfg := fixtureConfig()

	if cfg.WorkspacePath == DefaultWorkspacePath {
		t.Fatalf("fixtureConfig().WorkspacePath equals DefaultWorkspacePath (%q), so a mutant that hardcoded the default would SURVIVE every path assertion in this file", DefaultWorkspacePath)
	}
	if cfg.ImageTag == DefaultImageTag {
		t.Fatalf("fixtureConfig().ImageTag equals DefaultImageTag (%q), so a mutant that hardcoded the default tag would SURVIVE", DefaultImageTag)
	}
	if cfg.GatewayPort == DefaultGatewayPort {
		t.Fatalf("fixtureConfig().GatewayPort equals DefaultGatewayPort (%d), so a mutant that hardcoded the default port would SURVIVE every port assertion in this file", DefaultGatewayPort)
	}
	if cfg.GatewayPort == 0 {
		t.Fatalf("fixtureConfig().GatewayPort is zero, which buildPorts resolves TO DefaultGatewayPort (%d) — so the override path is never exercised and the previous check passes vacuously", DefaultGatewayPort)
	}

	// Watch the output move with the input, which is what "the fixture can see
	// it" actually means.
	spec := mustBuild(t, fixtureAgent(), cfg, Options{Instructions: "x"})
	if strings.Contains(spec.Files[0].Path, DefaultWorkspacePath) {
		t.Errorf("file path %q contains the DEFAULT workspace even though Config set %q — the config field is being ignored", spec.Files[0].Path, cfg.WorkspacePath)
	}
	if !strings.HasPrefix(spec.Files[0].Path, cfg.WorkspacePath) {
		t.Errorf("file path %q does not start with the configured workspace %q", spec.Files[0].Path, cfg.WorkspacePath)
	}
	if strings.HasSuffix(spec.Runtime.Image, ":"+DefaultImageTag) {
		t.Errorf("Runtime.Image %q ends in the DEFAULT tag even though Config set %q", spec.Runtime.Image, cfg.ImageTag)
	}
	if got := spec.PortNumber(provision.DefaultPortName); got == DefaultGatewayPort {
		t.Errorf("the declared %q port is DefaultGatewayPort (%d) even though Config set %d — the config field is being ignored", provision.DefaultPortName, DefaultGatewayPort, cfg.GatewayPort)
	}

	// The same control for the two fields most likely to be confused with each
	// other, since both are "a path in the workspace".
	if spec.Workspace.Path == spec.Repo.Path {
		t.Error("Workspace.Path equals Repo.Path, so a mutant swapping them would survive")
	}
}

// --------------------------------------------------------------------------
// The declared port, which is what makes an instance addressable at all.
// --------------------------------------------------------------------------

// TestTheBuiltSpecDeclaresTheGatewayPortUnderTheNameTheDriverResolves is the
// regression guard for "agentspec.Build declares no port".
//
// 🔴 IT IS A REGRESSION TEST AND NOT AN INVARIANT GUARD, AND THE MATRIX IS THE
// EVIDENCE: at the parent commit Build set Ports nil — its own committed golden
// said "Ports": null — so every assertion below was red there. What that nil cost
// is one hop away and mechanical: no Ports means k8s renderService returns nil
// (no Service), renderAnnotations writes no `muster.dev/port`, and
// provision.ResolveEndpoint answers provision.ErrNoEndpoint for every agent
// muster provisioned. cmd/muster-server's reachability test is the half that
// drives that chain; this is the half that pins the spec.
//
// 🔴 THE NAME IS ASSERTED, NOT JUST THE NUMBER, AND THE DIFFERENCE IS A LATENT
// BUG. Spec.PortNumber falls back to "the single declared port when there is
// exactly one", so a port named anything at all resolves while this list has
// length one — and silently stops resolving when a second port is added. The
// name is what makes resolution independent of the list's length.
func TestTheBuiltSpecDeclaresTheGatewayPortUnderTheNameTheDriverResolves(t *testing.T) {
	cfg := fixtureConfig()
	spec := mustBuild(t, fixtureAgent(), cfg, Options{Instructions: "body"})

	if len(spec.Ports) != 1 {
		t.Fatalf("Ports = %+v, want exactly one; a built spec with no port resolves no address, and this is the field that was nil", spec.Ports)
	}
	if got := spec.Ports[0].Name; got != provision.DefaultPortName {
		t.Errorf("the declared port is named %q, want %q — k8s renderAnnotations reads the "+
			"annotation Driver.Endpoint resolves from Spec.PortNumber(provision.DefaultPortName), "+
			"and any other name only resolves by PortNumber's single-port fallback",
			got, provision.DefaultPortName)
	}
	// The override, pinned to the LITERAL the fixture sets rather than to a second
	// read of cfg: an assertion spelled `== cfg.GatewayPort` passes for an
	// implementation that echoes whatever it was handed, which is what is wanted
	// here, but it also passes for one that read a different int field of the same
	// value. Nothing else in fixtureConfig is 21473.
	if got := spec.PortNumber(provision.DefaultPortName); got != 21473 {
		t.Errorf("PortNumber(%q) = %d, want 21473 (the fixture's Config.GatewayPort)", provision.DefaultPortName, got)
	}

	// 🔴 AND THE DEFAULT PATH, WHICH IS THE ONE EVERY DEPLOYMENT RUNS. An
	// assertion only over the override would be green for a Build that ignored the
	// zero value and declared nothing — the exact defect — because the fixture
	// never leaves the field unset.
	dflt := cfg
	dflt.GatewayPort = 0
	spec = mustBuild(t, fixtureAgent(), dflt, Options{Instructions: "body"})
	// The literal is deliberate: DefaultGatewayPort carries a MEASUREMENT against
	// a real runtime image, so changing the number has to make a human look at
	// that measurement rather than at a test that re-derives it.
	if got := spec.PortNumber(provision.DefaultPortName); got != 18789 {
		t.Errorf("an unset Config.GatewayPort produced port %d, want 18789 — see DefaultGatewayPort's measurement", got)
	}
	if DefaultGatewayPort != 18789 {
		t.Errorf("DefaultGatewayPort is %d, not 18789. If the measurement changed, update it here AND in the constant's comment — the number is only worth having because its provenance travels with it", DefaultGatewayPort)
	}

	// Protocol is empty, which provision.Port documents AS TCP. Pinned because
	// "TCP" written here would be a second statement of the same fact, and a later
	// reader is entitled to know the blank is a decision.
	if got := spec.Ports[0].Protocol; got != "" {
		t.Errorf("Protocol = %q, want empty (provision.Port documents empty as TCP, and both k8s render paths default it that way)", got)
	}
}

// TestABadGatewayPortIsRefusedByTheFieldThatHoldsIt pins the range refusal's own
// message.
//
// 🔴 provision.Spec.Validate WOULD ALSO REFUSE THESE, WHICH IS WHY THIS TEST
// ASSERTS THE TEXT AND NOT MERELY "an error". Deleting buildPorts' range check
// leaves every case below still failing — one layer down, with Validate's message
// naming the PORT ("port \"gateway\" number -1 out of range") instead of the knob
// somebody set. A test that only required a non-nil error would therefore SURVIVE
// that deletion, which is the shape a mutation sweep catches and an eyeball does
// not.
func TestABadGatewayPortIsRefusedByTheFieldThatHoldsIt(t *testing.T) {
	for _, port := range []int{-1, 65536, 1 << 20} {
		cfg := fixtureConfig()
		cfg.GatewayPort = port
		_, err := Build(fixtureAgent(), cfg, Options{Instructions: "body"})
		if err == nil {
			t.Errorf("Build accepted Config.GatewayPort=%d", port)
			continue
		}
		if !strings.Contains(err.Error(), "Config.GatewayPort") {
			t.Errorf("Config.GatewayPort=%d was refused without naming the field: %v\n"+
				"    provision.Spec.Validate refuses it too, naming the PORT rather than the "+
				"knob — so a refusal that does not name this field means the range check in "+
				"buildPorts is gone and Validate is answering for it.", port, err)
		}
	}

	// Control: the boundary values are ACCEPTED, or the check above would be
	// satisfied by one that refuses every port.
	for _, port := range []int{1, 65535} {
		cfg := fixtureConfig()
		cfg.GatewayPort = port
		if _, err := Build(fixtureAgent(), cfg, Options{Instructions: "body"}); err != nil {
			t.Errorf("Build refused the legal boundary port %d: %v", port, err)
		}
	}
}

// --------------------------------------------------------------------------
// A driver actually accepts what Build produces. This is 22a's closing condition.
// --------------------------------------------------------------------------

// TestARealDriverAcceptsASpecBuiltFromAnAgentRow is the difference between "the
// spec validates" and "the spec is usable".
//
// 🔴 Spec.Validate() IS NOT THIS CLAIM. Validate checks the spec's own
// invariants; a driver additionally runs CheckSpec against its capabilities and
// then has to do something with every field. Driving the full lifecycle is what
// exercises the second half — and the noop driver is the right instrument
// precisely because it has no backend to blame a failure on.
func TestARealDriverAcceptsASpecBuiltFromAnAgentRow(t *testing.T) {
	spec := mustBuild(t, fixtureAgent(), fixtureConfig(), Options{
		Instructions: "# Instructions\nbody\n",
		SeedFiles:    map[string]string{"IDENTITY.md": "who you are"},
	})

	drv := provision.MustNewNoop()
	ctx := context.Background()

	// provision.CheckSpec is a package-level function taking the driver's own
	// declared capabilities — not a method. Asking the driver for them rather
	// than writing a Capabilities literal is what keeps this honest: a literal
	// would be the test deciding what the driver can do.
	if err := provision.CheckSpec(drv.Capabilities(), spec); err != nil {
		t.Fatalf("the noop driver's capabilities refuse a built spec: %v", err)
	}
	if err := drv.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 🔴 Ref.ID IS DELIBERATELY DROPPED HERE. provision.Ref's doc says Name is
	// the key and ID is a correlation hint a driver MUST NOT require; looking the
	// instance up by name alone is what checks Build did not produce a spec that
	// only works when the id travels with it.
	inst, err := drv.Get(ctx, provision.Ref{Name: spec.Ref.Name})
	if err != nil {
		t.Fatalf("Get by name alone: %v — a built spec must not require Ref.ID to be usable", err)
	}
	if inst.Ref.Name != "harbour-kestrel" {
		t.Errorf("instance name = %q", inst.Ref.Name)
	}

	if err := drv.Scale(ctx, provision.Ref{Name: spec.Ref.Name}, 0); err != nil {
		t.Fatalf("Scale to zero: %v", err)
	}
	if err := drv.Destroy(ctx, provision.Ref{Name: spec.Ref.Name}); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	// Destroy is documented as nil when the instance is already absent, so a
	// second call is the idempotence check rather than an error case.
	if err := drv.Destroy(ctx, provision.Ref{Name: spec.Ref.Name}); err != nil {
		t.Errorf("second Destroy returned %v; the contract says nil when already absent", err)
	}
}

// TestARestrictedDriverRefusesTheFieldsItCannotHonourRatherThanIgnoringThem
// checks the other direction: a built spec sets capability-gated fields
// (resources, files, a persistent workspace), and a driver without those
// capabilities must SAY SO. A driver that silently ignored them would run an
// agent with no memory ceiling, which is how a box gets taken out.
func TestARestrictedDriverRefusesTheFieldsItCannotHonourRatherThanIgnoringThem(t *testing.T) {
	spec := mustBuild(t, fixtureAgent(), fixtureConfig(), Options{Instructions: "body"})

	restricted := provision.MustNewNoop(provision.NoopCapabilities(provision.Capabilities{}))

	// 🔴 GO THROUGH Create, NOT JUST CheckSpec. CheckSpec going red proves the
	// RULE exists; Create going red proves the driver actually reaches it. A
	// driver that declared the rule and forgot to call it would pass the first
	// check and silently provision anyway — which is the exact shape doc_seams
	// entry 1 records against the original noop driver, where every method
	// returned nil and reported success for work nobody did.
	err := restricted.Create(context.Background(), spec)
	if err == nil {
		t.Fatal("a driver with no capabilities CREATED from a spec setting resource limits, files and a persistent workspace; want a refusal naming what it cannot honour")
	}
	if err.Error() == "" {
		t.Error("refusal carried an empty message")
	}

	// And the rule itself is reachable with the same capabilities, so the red
	// above is attributable to capability refusal rather than to anything else
	// Create does.
	if direct := provision.CheckSpec(provision.Capabilities{}, spec); direct == nil {
		t.Error("provision.CheckSpec accepted the same spec under the same empty capabilities, so Create's error came from somewhere else and this test is not measuring what it claims")
	}
}

// --------------------------------------------------------------------------
// The regression golden. Evidence about CHANGE, never about correctness.
// --------------------------------------------------------------------------

const goldenPath = "testdata/spec.golden.json"

// goldenWrite regenerates the golden instead of comparing against it.
//
// 🔴 IT EXISTS BECAUSE THE FAILURE MESSAGE BELOW PROMISES IT, AND A PROMISE IN
// AN ERROR STRING IS A CLAIM LIKE ANY OTHER. The first draft of this file told
// the reader to run `-golden-write` without defining the flag, so the advice
// would have failed with "flag provided but not defined" — worse than no advice,
// because it reads as a supported workflow.
//
// ⚠ Regenerating is NOT how you check a change is correct. It records whatever
// the code currently produces. Read the diff first, confirm the hand-written
// assertions above still pass, and only then regenerate.
var goldenWrite = flag.Bool("golden-write", false, "rewrite testdata/spec.golden.json from the current implementation instead of comparing")

// TestTheBuiltSpecMatchesItsGolden pins the whole built spec byte-for-byte.
//
// ⚠ READ THIS BEFORE TRUSTING A GREEN RUN. The golden file was PRODUCED BY
// RUNNING THIS CODE, so it cannot tell you the spec is right — only that it has
// not changed. The assertions above are what test correctness. This exists
// because a spec has 15 fields and a one-field regression is otherwise silent.
//
// Secrets are redacted to their NAMES before serialisation, deliberately: a
// golden is a checked-in file, and pinning a credential's bytes in the repository
// would be a leak even for a fixture value. The names are what a regression would
// change anyway.
func TestTheBuiltSpecMatchesItsGolden(t *testing.T) {
	spec := mustBuild(t, fixtureAgent(), fixtureConfig(), Options{
		Instructions: "# Instructions\n\nbody\n",
		SeedFiles:    map[string]string{"IDENTITY.md": "who you are", "USER.md": "who they are"},
		ExtraEnv:     []provision.EnvVar{{Name: "EXAMPLE_CALLER_FLAG", Value: "on"}},
	})

	got, err := json.MarshalIndent(goldenView(spec), "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')

	if *goldenWrite {
		if err := os.MkdirAll(filepath.Dir(filepath.FromSlash(goldenPath)), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(filepath.FromSlash(goldenPath), got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		// Fail, do not pass. A regeneration run has verified nothing, and a
		// green line here would enter CI logs as evidence that it had.
		t.Fatalf("wrote %s (%d bytes). This run REGENERATED the golden and asserted nothing — re-run without -golden-write to actually compare.", goldenPath, len(got))
	}

	want, err := os.ReadFile(filepath.FromSlash(goldenPath))
	if err != nil {
		t.Fatalf("read golden: %v\n\nIf this is a new golden, write it with:\n  go test ./internal/agentspec -run TestTheBuiltSpecMatchesItsGolden -golden-write", err)
	}
	if string(got) != string(want) {
		t.Errorf("built spec differs from %s.\n\n🔴 THIS IS A REGRESSION SIGNAL, NOT A CORRECTNESS ONE. If the change is intended, read the diff, confirm the hand-written assertions in this file still pass, and regenerate the golden. If it is not, you have found the bug.\n\n--- got ---\n%s\n--- want ---\n%s", goldenPath, got, want)
	}
}

// goldenView is what actually gets serialised: the spec with secret values
// redacted and large file bodies reduced to a digest.
//
// 🔴 THE ELISION IS NOT COSMETIC, IT IS WHAT KEEPS THE GOLDEN A USEFUL SIGNAL.
// The autosave daemon is ~21 KB of shell, so embedding it verbatim made this file
// 31 KB of base64 in which the dozen lines a reviewer actually needs to read were
// invisible — and any one-character edit to the daemon rewrote 28 KB of golden,
// which trains a reviewer to regenerate without reading. A digest changes by
// exactly one line for the same edit.
//
// ⚠ THE COVERAGE IS NOT LOST, and that is the condition for eliding at all: the
// daemon's bytes are pinned FULL-LENGTH against the embedded script by
// TestTheDaemonIsDeliveredAsBytesRatherThanAsAShellPayload. Eliding here would be
// a real hole if that test did not exist — so if you ever delete it, this must go
// back to verbatim.
//
// It copies rather than editing in place so the caller's spec is unchanged — the
// same aliasing discipline Build itself follows.
func goldenView(spec provision.Spec) provision.Spec {
	out := spec

	out.Secrets = make([]provision.EnvVar, len(spec.Secrets))
	for i, s := range spec.Secrets {
		out.Secrets[i] = provision.EnvVar{Name: s.Name, Value: "«redacted in the golden; see goldenView»"}
	}

	// 🔴 A CONFIDENTIAL *FILE* IS REDACTED TOO, AND THIS USED TO COVER Secrets ONLY.
	// The paragraph above reasoned about Spec.Secrets — the confidential ENVIRONMENT
	// — and was complete while nothing produced a confidential FILE. cairn.go now
	// does, and a [provision.File] marked Secret carries its bytes in Content, which
	// the elision below would only have touched if it happened to be large. So a
	// store credential would have been written verbatim into a checked-in testdata
	// file. Redacting by the SAME flag a driver branches on is what keeps this
	// honest: a future secret file is covered without anybody remembering to add it.
	// TestTheGoldenViewRedactsAConfidentialFileRatherThanOnlyAConfidentialVariable
	// pins it.

	// The threshold is generous on purpose: small files — instructions, seeds —
	// stay verbatim, because their content IS what a reviewer wants to see change.
	const elideOver = 2048
	out.Files = make([]provision.File, len(spec.Files))
	for i, f := range spec.Files {
		out.Files[i] = f
		if f.Secret {
			out.Files[i].Content = []byte("«confidential file redacted in the golden; see goldenView»")
			continue
		}
		if len(f.Content) > elideOver {
			out.Files[i].Content = []byte(fmt.Sprintf(
				"«%d bytes elided; sha256=%x; full bytes pinned by TestTheDaemonIsDeliveredAsBytesRatherThanAsAShellPayload»",
				len(f.Content), sha256.Sum256(f.Content)))
		}
	}
	return out
}
