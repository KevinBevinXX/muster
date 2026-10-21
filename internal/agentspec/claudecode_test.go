package agentspec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// Fake setup-tokens: pairwise distinct and obviously not real.
const (
	ccTokenAlpha = "sk-ant-oat01-FAKE-ccspec-alpha-7d1e"
	ccTokenBeta  = "sk-ant-oat01-FAKE-ccspec-beta-04af"
)

func ccConfig() Config {
	cfg := fixtureConfig()
	cfg.ClaudeCode = &ClaudeCodeConfig{
		Image:    "ghcr.io/example-org/claude-code-agent:0a1b2c3",
		Accounts: map[string]string{"alpha": ccTokenAlpha, "beta": ccTokenBeta},
	}
	// The hermes kind too, so the token ledger below builds EVERY kind.
	cfg.Hermes = &HermesConfig{Image: "ghcr.io/example-org/hermes-agent:hermes-5fc490e2"}
	return cfg
}

func ccAgent() agents.Agent {
	a := fixtureAgent()
	a.Kind = agents.KindClaudeCode
	a.CCAccount = "beta"
	a.Model = ""
	a.Repo, a.RepoBranch = "", ""
	return a
}

// TestTheClaudeCodeSpecPinsItsProfile pins every value of the claude-code
// profile as a LITERAL — none of them is derived from the constant it is
// checking, so a changed constant reddens here.
func TestTheClaudeCodeSpecPinsItsProfile(t *testing.T) {
	a := ccAgent()
	spec := mustBuild(t, a, ccConfig(), Options{})

	if spec.Runtime.Image != "ghcr.io/example-org/claude-code-agent:0a1b2c3" {
		t.Errorf("image = %q", spec.Runtime.Image)
	}
	if len(spec.Runtime.Command) != 0 || len(spec.Runtime.Args) != 0 || spec.Runtime.WorkingDir != "" {
		t.Errorf("runtime overrides the image's entrypoint/workdir: %+v", spec.Runtime)
	}
	if want := []provision.Port{{Name: "gateway", Port: 18789}}; !reflect.DeepEqual(spec.Ports, want) {
		t.Errorf("ports = %+v, want %+v (ccd listens on 18789; the gateway's configured port %d must not leak in)",
			spec.Ports, want, fixtureConfig().GatewayPort)
	}
	if want := (provision.Health{HTTPGetPath: "/", PortName: "gateway"}); spec.Health != want {
		t.Errorf("health = %+v, want %+v", spec.Health, want)
	}
	wantSec := provision.Security{RunAsUser: 1000, RunAsGroup: 1000, FSGroup: 1000, RunAsNonRoot: true, Restricted: true, NoServiceAccountToken: true}
	if spec.Security != wantSec {
		t.Errorf("security = %+v, want %+v", spec.Security, wantSec)
	}
	// 🔴 ISOLATED, WITH 443 AS THE ONE PUBLIC PORT. This is the declaration that
	// makes a driver write the agent's NetworkPolicy and refuse to start it without
	// one; a claude-code spec that lost it would be accepted by every driver and
	// run with the cluster and the LAN in reach.
	if want := (provision.Network{Isolate: true, PublicEgressTCPPorts: []int{443}}); !reflect.DeepEqual(spec.Network, want) {
		t.Errorf("network = %+v, want %+v", spec.Network, want)
	}
	if want := (provision.Workspace{Path: "/data", Size: "10Gi", Persist: true}); spec.Workspace != want {
		t.Errorf("workspace = %+v, want %+v (the PVC must mount at /data: CLAUDE_CONFIG_DIR and the workspace are under it)", spec.Workspace, want)
	}
	if want := (provision.Resources{CPURequest: "250m", MemoryRequest: "512Mi", MemoryLimit: "3Gi"}); spec.Resources != want {
		t.Errorf("resources = %+v, want %+v", spec.Resources, want)
	}
	if len(spec.Files) != 0 || len(spec.Init) != 0 || spec.Config != nil {
		t.Errorf("a claude-code spec carries files %d / init %d / config %v; it carries none", len(spec.Files), len(spec.Init), spec.Config)
	}
	wantEnv := []provision.EnvVar{{Name: "MUSTER_API_URL", Value: "http://muster.example.test:8105"}, {Name: "GIT_TERMINAL_PROMPT", Value: "0"}}
	if !reflect.DeepEqual(spec.Env, wantEnv) {
		t.Errorf("env = %+v, want %+v", spec.Env, wantEnv)
	}
	wantSecrets := []provision.EnvVar{
		{Name: "MUSTER_HOOK_TOKEN", Value: a.HooksToken},
		{Name: "HOOKS_TOKEN", Value: a.HooksToken},
		{Name: "CLAUDE_CODE_OAUTH_TOKEN", Value: ccTokenBeta},
	}
	if !reflect.DeepEqual(spec.Secrets, wantSecrets) {
		t.Errorf("secrets = %v, want %v (the row's account is beta; alpha's token must not be placed)", names(spec.Secrets), names(wantSecrets))
	}
	if spec.Labels["muster.agent/kind"] != "claude-code" || spec.Labels["muster.agent/name"] != a.Name {
		t.Errorf("labels = %v", spec.Labels)
	}
	if spec.Repo != (provision.Repo{}) {
		t.Errorf("repo = %+v, want none declared (the agent names no repository)", spec.Repo)
	}
}

func names(env []provision.EnvVar) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		out = append(out, e.Name)
	}
	return out
}

// TestTheStorageSizeIsConfigurable feeds a value the default cannot equal.
func TestTheStorageSizeIsConfigurable(t *testing.T) {
	cfg := ccConfig()
	cfg.ClaudeCode.StorageSize = "3Gi"
	if got := mustBuild(t, ccAgent(), cfg, Options{}).Workspace.Size; got != "3Gi" {
		t.Fatalf("size = %q, want 3Gi", got)
	}
}

// TestTheClaudeCodeBuildRefusesEveryMissingInput reaches each refusal with a
// case no earlier check rejects (every other input valid), and asserts the
// refusal's OWN words.
func TestTheClaudeCodeBuildRefusesEveryMissingInput(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*agents.Agent, *Config, *Options)
		want string
	}{
		{"kind not enabled", func(_ *agents.Agent, c *Config, _ *Options) { c.ClaudeCode = nil }, "has not enabled"},
		{"no image", func(_ *agents.Agent, c *Config, _ *Options) { c.ClaudeCode.Image = " " }, "Image is required"},
		{"no account", func(a *agents.Agent, _ *Config, _ *Options) { a.CCAccount = "" }, "names no Claude account"},
		{"unknown account", func(a *agents.Agent, _ *Config, _ *Options) { a.CCAccount = "gamma" }, `"gamma", which is not configured`},
		{"no hooks token", func(a *agents.Agent, _ *Config, _ *Options) { a.HooksToken = "" }, "no hooks token"},
		{"a repository that is not owner/name", func(a *agents.Agent, _ *Config, _ *Options) { a.Repo = "example-org/../tide" }, "which is not owner/name"},
		{"seed files", func(_ *agents.Agent, _ *Config, o *Options) { o.SeedFiles = map[string]string{"x": "y"} }, "takes no seed files"},
		{"extra env", func(_ *agents.Agent, _ *Config, o *Options) { o.ExtraEnv = []provision.EnvVar{{Name: "X", Value: "y"}} }, "takes no seed files"},
		{"cairn", func(_ *agents.Agent, _ *Config, o *Options) { o.CairnEligible = true }, "subsystem-store credential"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, cfg, opts := ccAgent(), ccConfig(), Options{}
			c.mut(&a, &cfg, &opts)
			_, err := Build(a, cfg, opts)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
			for _, tok := range []string{ccTokenAlpha, ccTokenBeta} {
				if strings.Contains(err.Error(), tok) {
					t.Fatalf("the refusal carries a token: %v", err)
				}
			}
		})
	}
	// The control: unmutated, it builds.
	if _, err := Build(ccAgent(), ccConfig(), Options{}); err != nil {
		t.Fatalf("control: the unmutated claude-code agent does not build: %v", err)
	}
}

// TestAGatewayAgentIsBuiltIdenticallyWhetherOrNotClaudeCodeIsEnabled: enabling
// the claude-code kind must not change one byte of a gateway-kind spec — nor its
// fingerprint, which is what decides whether a live instance rolls.
func TestAGatewayAgentIsBuiltIdenticallyWhetherOrNotClaudeCodeIsEnabled(t *testing.T) {
	for _, kind := range []string{"", agents.KindGateway} {
		a := fixtureAgent()
		a.Kind = kind
		off := mustBuild(t, a, fixtureConfig(), Options{Instructions: "x"})
		on := mustBuild(t, a, ccConfig(), Options{Instructions: "x"})
		if !reflect.DeepEqual(off, on) || provision.Fingerprint(off) != provision.Fingerprint(on) {
			t.Fatalf("kind %q: enabling claude-code changed the gateway spec", kind)
		}
		if !off.Security.IsZero() {
			t.Fatalf("kind %q: a gateway spec declares security %+v; it must declare none", kind, off.Security)
		}
		// The gateway kind gets NO NetworkPolicy, and this is where that is decided:
		// the driver renders one only for a spec that declares isolation.
		if !off.Network.IsZero() || !on.Network.IsZero() {
			t.Fatalf("kind %q: a gateway spec declares network %+v / %+v; it must declare none", kind, off.Network, on.Network)
		}
	}
}

func TestAGatewayAgentNamingAnAccountOrAnUnknownKindIsRefused(t *testing.T) {
	a := fixtureAgent()
	a.CCAccount = "alpha"
	if _, err := Build(a, ccConfig(), Options{}); err == nil || !strings.Contains(err.Error(), "names a Claude account") {
		t.Fatalf("gateway with account: err = %v", err)
	}
	a = fixtureAgent()
	a.Kind = "teleport"
	if _, err := Build(a, ccConfig(), Options{}); err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Fatalf("unknown kind: err = %v", err)
	}
}

func TestExtraEnvCannotHandAGatewayAgentTheClaudeToken(t *testing.T) {
	_, err := Build(fixtureAgent(), ccConfig(), Options{ExtraEnv: []provision.EnvVar{{Name: "CLAUDE_CODE_OAUTH_TOKEN", Value: ccTokenAlpha}}})
	if err == nil || !strings.Contains(err.Error(), "may not set CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("err = %v", err)
	}
}

// TestTheClaudeTokenNameAppearsOnlyInTheClaudeCodeKindsSecret is the SEAM
// LEDGER for the subscription token. It pins a RELATIONSHIP across three
// surfaces, so it fails if the set of places grows or shrinks:
//
//  1. Built specs, for EVERY kind in agents.Kinds, from a config that has the
//     claude-code kind enabled with tokens: the gateway kind's spec contains
//     neither the variable name nor any token value anywhere (env, secrets,
//     files, init, config, labels); the claude-code kind's spec contains its own
//     account's token exactly once, in Secrets, under exactly that name — and
//     neither name nor any token anywhere else.
//  2. The module's non-test Go source: the quoted literal "CLAUDE_CODE_OAUTH_TOKEN"
//     appears in exactly the ledgered files.
//  3. (Rendering — the k8s driver puts Secrets only in the `-env` Secret — is
//     pinned in internal/provision/k8s by
//     TestTheClaudeTokenRendersOnlyIntoTheEnvSecret.)
func TestTheClaudeTokenNameAppearsOnlyInTheClaudeCodeKindsSecret(t *testing.T) {
	cfg := ccConfig()
	for _, kind := range agents.Kinds {
		t.Run(kind, func(t *testing.T) {
			a := fixtureAgent()
			a.Kind = kind
			if kind == agents.KindClaudeCode {
				a = ccAgent()
			}
			spec := mustBuild(t, a, cfg, Options{Instructions: "the instructions"})

			placed := 0
			var rest []provision.EnvVar
			for _, e := range spec.Secrets {
				if e.Name == EnvClaudeOAuthToken {
					placed++
					if e.Value != ccTokenBeta {
						t.Errorf("%s carries the wrong account's token", e.Name)
					}
					continue
				}
				rest = append(rest, e)
			}
			wantPlaced := 0
			if kind == agents.KindClaudeCode {
				wantPlaced = 1
			}
			if placed != wantPlaced {
				t.Fatalf("kind %s: %s placed in Secrets %d time(s), want %d", kind, EnvClaudeOAuthToken, placed, wantPlaced)
			}
			spec.Secrets = rest
			blob, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			for _, needle := range []string{EnvClaudeOAuthToken, ccTokenAlpha, ccTokenBeta} {
				if strings.Contains(string(blob), needle) {
					t.Errorf("kind %s: %q appears in the spec OUTSIDE its one Secrets entry", kind, needle)
				}
			}
		})
	}

	// 2. The source ledger.
	want := []string{"cmd/ccd/server.go", "internal/agentspec/claudecode.go"}
	got := sourcesSpelling(t, `"CLAUDE_CODE_OAUTH_TOKEN"`)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("non-test Go files spelling \"CLAUDE_CODE_OAUTH_TOKEN\":\n got %v\nwant %v\n"+
			"Every other package must name agentspec.EnvClaudeOAuthToken (cmd/ccd is the in-pod reader). "+
			"A new spelling is a new place the subscription token can be written; decide it, then ledger it.", got, want)
	}
}

// sourcesSpelling lists the module's non-test .go files containing needle, as
// module-relative paths. It enumerates with filepath.WalkDir — never a
// .gitignore-honouring search — and has a positive control.
func sourcesSpelling(t *testing.T, needle string) []string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(here), "..", "..")
	var out []string
	scanned := 0
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "e2e":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		scanned++
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), needle) {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 100 {
		t.Fatalf("instrument check: only %d Go files scanned; the walk is not reaching the module", scanned)
	}
	sort.Strings(out)
	return out
}

// TestEveryClaudeCodeSpecIsIsolatedAndNoCallerCanShareItsPortList: the network
// declaration is unconditional — there is no input that builds a claude-code spec
// without it — and each spec gets its OWN port slice, so one caller mutating its
// spec cannot widen the next agent's.
func TestEveryClaudeCodeSpecIsIsolatedAndNoCallerCanShareItsPortList(t *testing.T) {
	first := mustBuild(t, ccAgent(), ccConfig(), Options{})
	first.Network.PublicEgressTCPPorts[0] = 22
	first.Network.PublicEgressTCPPorts = append(first.Network.PublicEgressTCPPorts, 25)
	cfg := ccConfig()
	cfg.ClaudeCode.StorageSize = "3Gi"
	second := mustBuild(t, ccAgent(), cfg, Options{Instructions: "anything"})
	if want := (provision.Network{Isolate: true, PublicEgressTCPPorts: []int{443}}); !reflect.DeepEqual(second.Network, want) {
		t.Fatalf("a second claude-code spec's network = %+v, want %+v: the first caller's edit leaked into it", second.Network, want)
	}
}

// TestAClaudeCodeAgentsInstructionsAreItsManagedClaudeMD: Options.Instructions is
// placed as exactly one file, at the CLI's managed-policy memory path (a literal
// here, not the constant), carrying the text verbatim, non-secret, with the
// driver's default mode — and with NO instructions there is no file at all. It is
// outside /data, so nothing on the agent's volume (its workspace, a repository's
// own CLAUDE.md) is shadowed.
func TestAClaudeCodeAgentsInstructionsAreItsManagedClaudeMD(t *testing.T) {
	const prose = "# a supervisor\nline two, distinct from every constant\n"
	spec := mustBuild(t, ccAgent(), ccConfig(), Options{Instructions: prose})
	want := []provision.File{{Path: "/etc/claude-code/CLAUDE.md", Content: []byte(prose)}}
	if !reflect.DeepEqual(spec.Files, want) {
		t.Fatalf("files = %+v, want %+v", spec.Files, want)
	}
	if strings.HasPrefix(spec.Files[0].Path, spec.Workspace.Path+"/") {
		t.Fatalf("the instructions land on the agent's own volume (%s), where they would shadow its files", spec.Workspace.Path)
	}
	if none := mustBuild(t, ccAgent(), ccConfig(), Options{}); len(none.Files) != 0 {
		t.Fatalf("no instructions, but files %+v", none.Files)
	}
	// The gateway kind still writes AGENTS.md in its workspace, not this path.
	gw := mustBuild(t, fixtureAgent(), ccConfig(), Options{Instructions: prose})
	for _, f := range gw.Files {
		if f.Path == "/etc/claude-code/CLAUDE.md" {
			t.Fatal("a gateway agent was given the claude-code managed CLAUDE.md")
		}
	}
	if gw.Files[0].Path != "/workspace/AGENTS.md" && !strings.HasSuffix(gw.Files[0].Path, "/AGENTS.md") {
		t.Fatalf("gateway instructions moved: %v", gw.Files[0].Path)
	}
}
