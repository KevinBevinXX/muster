package agentspec

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

const hermesImage = "ghcr.io/example-org/hermes-agent:hermes-5fc490e2"

func hermesConfig() Config {
	cfg := fixtureConfig()
	cfg.Hermes = &HermesConfig{Image: hermesImage}
	return cfg
}

func hermesAgent() agents.Agent {
	a := fixtureAgent()
	a.Kind = agents.KindHermes
	return a
}

func hermesEnv(t *testing.T, vars []provision.EnvVar) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, e := range vars {
		if _, dup := m[e.Name]; dup {
			t.Fatalf("%s set twice", e.Name)
		}
		m[e.Name] = e.Value
	}
	return m
}

// TestTheHermesSpecPinsItsProfile pins every value of the hermes profile as a
// LITERAL, none derived from the constant it checks.
func TestTheHermesSpecPinsItsProfile(t *testing.T) {
	cfg := hermesConfig()
	cfg.ConnectURL, cfg.ConnectPort = "http://muster-connect.example.test:18191", 18191
	spec := mustBuild(t, hermesAgent(), cfg, Options{Instructions: "the hermes instructions"})

	if spec.Runtime.Image != hermesImage || spec.Runtime.WorkingDir != "" || len(spec.Runtime.Command) != 0 {
		t.Fatalf("runtime %+v", spec.Runtime)
	}
	env := hermesEnv(t, spec.Env)
	want := map[string]string{
		"MUSTER_API_URL": "http://muster.example.test:8105", "GIT_TERMINAL_PROMPT": "0", "TERMINAL_HOME_MODE": "profile",
		// The agent's own model, its OpenRouter id (the provider prefix off).
		"HERMESD_MODEL":      "vendor-a/model-one",
		"MUSTER_CONNECT_URL": "http://muster-connect.example.test:18191",
		"HERMESD_REPO":       "example-org/tide-charts", "HERMESD_REPO_BRANCH": "release/7.2",
		"HERMESD_WORKSPACE": "/data/workspace/tide-charts",
	}
	if len(env) != len(want) {
		t.Fatalf("env %v, want exactly %v", env, want)
	}
	// 🔴 HERMESD_MODEL_BASE_URL is hermesd's TEST-ONLY override (D5): a
	// provisioned pod never carries it, whatever the config.
	for _, vars := range [][]provision.EnvVar{spec.Env, spec.Secrets} {
		if _, ok := hermesEnv(t, vars)["HERMESD_MODEL_BASE_URL"]; ok {
			t.Fatal("agentspec set the test-only HERMESD_MODEL_BASE_URL")
		}
	}
	for k, v := range want {
		if env[k] != v {
			t.Fatalf("env %s=%q, want %q", k, env[k], v)
		}
	}
	sec := hermesEnv(t, spec.Secrets)
	if len(sec) != 3 || sec["MUSTER_HOOK_TOKEN"] != "fixture-per-agent-token-9f31c7" || sec["HOOKS_TOKEN"] != "fixture-per-agent-token-9f31c7" ||
		sec["OPENROUTER_API_KEY"] != "fixture-shared-provider-key-2b84af" {
		t.Fatalf("secrets %v", sec)
	}
	if len(spec.Files) != 1 || spec.Files[0].Path != "/etc/hermesd/instructions.md" || string(spec.Files[0].Content) != "the hermes instructions" || spec.Files[0].Secret {
		t.Fatalf("files %+v", spec.Files)
	}
	if spec.Workspace != (provision.Workspace{Path: "/data", Size: "10Gi", Persist: true}) {
		t.Fatalf("workspace %+v", spec.Workspace)
	}
	if len(spec.Ports) != 1 || spec.Ports[0].Port != 18789 || spec.Ports[0].Name != provision.DefaultPortName ||
		spec.Health != (provision.Health{HTTPGetPath: "/", PortName: provision.DefaultPortName}) {
		t.Fatalf("ports %+v health %+v", spec.Ports, spec.Health)
	}
	if spec.Security != (provision.Security{RunAsUser: 1000, RunAsGroup: 1000, FSGroup: 1000, RunAsNonRoot: true, Restricted: true, NoServiceAccountToken: true, ReadOnlyRoot: true}) {
		t.Fatalf("security %+v", spec.Security)
	}
	if !spec.Network.Isolate || len(spec.Network.PublicEgressTCPPorts) != 1 || spec.Network.PublicEgressTCPPorts[0] != 443 ||
		len(spec.Network.ControllerEgressTCPPorts) != 1 || spec.Network.ControllerEgressTCPPorts[0] != 18191 {
		t.Fatalf("network %+v", spec.Network)
	}
	// 🔴 OVER HERMES'S 30 s SIGTERM DRAIN.
	if spec.TerminationGraceSeconds != 60 {
		t.Fatalf("termination grace %d", spec.TerminationGraceSeconds)
	}
	if spec.Labels[LabelKind] != "hermes" || spec.Repo != (provision.Repo{URL: "example-org/tide-charts", Branch: "release/7.2", Path: "/data/workspace/tide-charts"}) {
		t.Fatalf("labels %v repo %+v", spec.Labels, spec.Repo)
	}
	if spec.Resources != (provision.Resources{CPURequest: "250m", MemoryRequest: "512Mi", MemoryLimit: "3Gi"}) {
		t.Fatalf("resources %+v", spec.Resources)
	}
	// Nothing of the gateway kind's: no runtime config, no init, no AGENTS.md.
	if len(spec.Config) != 0 || len(spec.Init) != 0 || spec.Sidecar != nil {
		t.Fatalf("config %v init %v sidecar %v", spec.Config, spec.Init, spec.Sidecar)
	}
}

// Without a repository, none of the three repository variables is set and the
// workspace (terminal.cwd) is hermesd's default; with no connector surface, no
// MUSTER_CONNECT_URL and no controller egress.
func TestAHermesSpecWithoutARepoOrConnectorSurface(t *testing.T) {
	a := hermesAgent()
	a.Repo, a.RepoBranch = "", ""
	spec := mustBuild(t, a, hermesConfig(), Options{})
	env := hermesEnv(t, spec.Env)
	for _, k := range []string{"HERMESD_REPO", "HERMESD_REPO_BRANCH", "HERMESD_WORKSPACE", "MUSTER_CONNECT_URL"} {
		if _, ok := env[k]; ok {
			t.Fatalf("%s set without a repository or connector surface: %v", k, env)
		}
	}
	if len(spec.Network.ControllerEgressTCPPorts) != 0 || len(spec.Files) != 0 || spec.Repo != (provision.Repo{}) {
		t.Fatalf("network %+v files %+v repo %+v", spec.Network, spec.Files, spec.Repo)
	}
}

func TestHermesModels(t *testing.T) {
	for _, tc := range []struct{ agent, deployment, want string }{
		{"openrouter/vendor-a/model-one", "", "vendor-a/model-one"},
		{"", "openrouter/vendor-b/model-two", "vendor-b/model-two"},
		// OpenRouter's own router: its id IS "openrouter/auto".
		{"openrouter/auto", "", "openrouter/auto"},
	} {
		a := hermesAgent()
		a.Model = tc.agent
		cfg := hermesConfig()
		cfg.Model = tc.deployment
		if got := hermesEnv(t, mustBuild(t, a, cfg, Options{}).Env)["HERMESD_MODEL"]; got != tc.want {
			t.Fatalf("agent %q deployment %q: HERMESD_MODEL=%q, want %q", tc.agent, tc.deployment, got, tc.want)
		}
	}
}

// HermesModelID is THE model rule (build, dispatch, model change): exactly
// openrouter/<vendor>/<model>, case-sensitive, or a one-part OpenRouter id.
func TestHermesModelID(t *testing.T) {
	for slug, want := range map[string]string{
		"openrouter/vendor-c/model-three": "vendor-c/model-three",
		"openrouter/router-x":             "openrouter/router-x",
	} {
		if got, err := HermesModelID(slug); err != nil || got != want {
			t.Fatalf("%q: %q, %v; want %q", slug, got, err, want)
		}
	}
	for _, bad := range []string{"", "openrouter/", "OpenRouter/vendor-c/model-three", "OPENROUTER/router-x",
		"openrouter//model-three", "openrouter/vendor-c/", "openrouter/vendor-c/model-three/extra", "anthropic/claude-x"} {
		if got, err := HermesModelID(bad); err == nil || !strings.Contains(err.Error(), "not an OpenRouter model") {
			t.Fatalf("%q: %q, %v; want a refusal", bad, got, err)
		}
	}
}

// Every missing or inapplicable input is a refusal naming it.
func TestAHermesSpecRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*agents.Agent, *Config, *Options)
		want string
	}{
		{"kind not enabled", func(_ *agents.Agent, c *Config, _ *Options) { c.Hermes = nil }, "has not enabled"},
		{"no image", func(_ *agents.Agent, c *Config, _ *Options) { c.Hermes.Image = " " }, "Image is required"},
		{"no base URL", func(_ *agents.Agent, c *Config, _ *Options) { c.APIBaseURL = "" }, "APIBaseURL is required"},
		{"no OpenRouter key", func(_ *agents.Agent, c *Config, _ *Options) { c.OpenRouterAPIKey = "" }, "no OpenRouter key"},
		{"no hooks token", func(a *agents.Agent, _ *Config, _ *Options) { a.HooksToken = "" }, "no hooks token"},
		{"a Claude account", func(a *agents.Agent, _ *Config, _ *Options) { a.CCAccount = "beta" }, "names a Claude account"},
		{"a non-OpenRouter model", func(a *agents.Agent, _ *Config, _ *Options) { a.Model = "anthropic/claude-x" }, "not an OpenRouter model"},
		{"seed files", func(_ *agents.Agent, _ *Config, o *Options) { o.SeedFiles = map[string]string{"x": "y"} }, "takes no seed files"},
		{"extra env", func(_ *agents.Agent, _ *Config, o *Options) { o.ExtraEnv = []provision.EnvVar{{Name: "X", Value: "y"}} }, "takes no seed files"},
		{"the store credential", func(_ *agents.Agent, _ *Config, o *Options) { o.CairnEligible = true }, "takes no seed files"},
		{"a gateway sidecar", func(_ *agents.Agent, _ *Config, o *Options) { o.GatewaySidecar = &GatewaySidecar{} }, "gateway sidecar"},
		{"a repo that is not owner/name", func(a *agents.Agent, _ *Config, _ *Options) { a.Repo = "x/.." }, "is not owner/name"},
		{"a connect URL without a port", func(_ *agents.Agent, c *Config, _ *Options) { c.ConnectURL = "http://m:1" }, "ConnectPort"},
	} {
		a, cfg, opts := hermesAgent(), hermesConfig(), Options{}
		tc.edit(&a, &cfg, &opts)
		if _, err := Build(a, cfg, opts); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

// 🔴 THE OPENROUTER KEY OF A HERMES AGENT IS IN ITS SECRETS ONLY: never in its
// plain environment, files, labels or anywhere else in the spec.
func TestTheOpenRouterKeyIsOnlyInAHermesAgentsSecret(t *testing.T) {
	spec := mustBuild(t, hermesAgent(), hermesConfig(), Options{Instructions: "x"})
	var rest []provision.EnvVar
	placed := 0
	for _, e := range spec.Secrets {
		if e.Name == EnvOpenRouterKey {
			placed++
			continue
		}
		rest = append(rest, e)
	}
	spec.Secrets = rest
	blob, _ := json.Marshal(spec)
	if placed != 1 || strings.Contains(string(blob), "fixture-shared-provider-key-2b84af") || strings.Contains(string(blob), EnvOpenRouterKey) {
		t.Fatalf("the key placed %d time(s) in Secrets; elsewhere in the spec: %t", placed, strings.Contains(string(blob), "fixture-shared-provider-key-2b84af"))
	}
}
