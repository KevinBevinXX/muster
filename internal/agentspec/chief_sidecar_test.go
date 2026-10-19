package agentspec

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

func envMap(env []provision.EnvVar) map[string]string {
	m := map[string]string{}
	for _, e := range env {
		m[e.Name] = e.Value
	}
	return m
}

// TestAClaudeCodeChiefRunsTheGatewayRuntimeBesideCcd pins the sidecar as
// LITERALS: the gateway kind's image, install command and runtime files; its own
// credentials (the derived bearer, the hooks token, the OpenRouter key) and NOT
// the Claude token; HOME and the loopback port; no working directory; and ccd
// told where it is. The instance's own Secrets hold neither gateway credential.
func TestAClaudeCodeChiefRunsTheGatewayRuntimeBesideCcd(t *testing.T) {
	a, cfg := ccAgent(), ccConfig()
	spec := mustBuild(t, a, cfg, Options{Instructions: "cc prose", GatewaySidecar: &GatewaySidecar{}})
	sc := spec.Sidecar
	if sc == nil {
		t.Fatal("no sidecar")
	}
	if sc.Name != "gateway" || sc.Runtime.Image != "ghcr.io/example-org/agent-runtime:3.11.0" ||
		strings.Join(sc.Runtime.Command, " ") != "sh -eu /muster/runtime/install" || sc.Runtime.WorkingDir != "" {
		t.Errorf("sidecar runtime %+v", sc.Runtime)
	}
	env := envMap(sc.Env)
	for name, want := range map[string]string{
		"HOME": "/home/node", "MUSTER_GATEWAY_PORT": "18791", "MUSTER_RUNTIME_CONFIG": "/muster/runtime/config",
		// The gateway kind's model config for this row, fallbacks included.
		"MUSTER_CONFIG": `{"model":{"fallbacks":["openrouter/vendor-c/model-three","openrouter/vendor-d/model-four"],"primary":"openrouter/vendor-b/model-two"}}`,
	} {
		if env[name] != want {
			t.Errorf("sidecar env %s = %q, want %q", name, env[name], want)
		}
	}
	sec := envMap(sc.Secrets)
	if sec["MUSTER_GATEWAY_BEARER"] != fixtureDeriveBearer(a.HooksToken) || sec["HOOKS_TOKEN"] != a.HooksToken ||
		sec["OPENROUTER_API_KEY"] != cfg.OpenRouterAPIKey || cfg.OpenRouterAPIKey == "" {
		t.Errorf("sidecar secrets %v", names(sc.Secrets))
	}
	if _, ok := sec[EnvClaudeOAuthToken]; ok {
		t.Error("the sidecar holds the Claude token")
	}
	var paths []string
	for _, f := range sc.Files {
		paths = append(paths, f.Path)
	}
	if strings.Join(paths, ",") != "/muster/runtime/config,/muster/runtime/install" {
		t.Errorf("sidecar files %v", paths)
	}
	if sc.Resources.MemoryLimit != "1536Mi" {
		t.Errorf("sidecar resources %+v", sc.Resources)
	}
	main := envMap(spec.Env)
	if main["CCD_GATEWAY_UPSTREAM"] != "http://127.0.0.1:18791" {
		t.Errorf("ccd is not told where the sidecar is: %v", names(spec.Env))
	}
	for _, e := range spec.Secrets {
		if e.Name == "OPENROUTER_API_KEY" || e.Name == "MUSTER_GATEWAY_BEARER" {
			t.Errorf("the instance's own Secrets hold the sidecar's %s", e.Name)
		}
	}
	// The Claude token appears once in the whole spec, sidecar included.
	blob, _ := json.Marshal(spec)
	if n := strings.Count(string(blob), ccTokenBeta); n != 1 {
		t.Errorf("the Claude token appears %d times in the spec; want once (the instance's own Secret)", n)
	}

	// Without the option: no sidecar, and ccd is told of none.
	plain := mustBuild(t, a, cfg, Options{Instructions: "cc prose"})
	if plain.Sidecar != nil || envMap(plain.Env)["CCD_GATEWAY_UPSTREAM"] != "" {
		t.Errorf("a chief without the option got a sidecar %+v", plain.Sidecar)
	}
}

// TestTheGatewaySidecarIsRefusedWhereItCannotBeBuilt: no runtime bundle, and a
// gateway-kind agent (whose own process is the runtime).
func TestTheGatewaySidecarIsRefusedWhereItCannotBeBuilt(t *testing.T) {
	cfg := ccConfig()
	cfg.RuntimeConfig = RuntimeConfig{}
	if _, err := Build(ccAgent(), cfg, Options{GatewaySidecar: &GatewaySidecar{}}); err == nil || !strings.Contains(err.Error(), "runtime bundle") {
		t.Errorf("no runtime bundle: %v", err)
	}
	g := fixtureAgent()
	g.Kind = agents.KindGateway
	if _, err := Build(g, ccConfig(), Options{GatewaySidecar: &GatewaySidecar{}}); err == nil {
		t.Error("a gateway-kind agent was given a gateway sidecar")
	}
}

// TestTheFunctionToolChiefProseNamesNoCLIAndNoMCP: the gateway-sidecar chief's
// prose names its tools as functions (no mcp__ prefix, no muster CLI), and
// carries the plugin section exactly when plugins are on.
func TestTheFunctionToolChiefProseNamesNoCLIAndNoMCP(t *testing.T) {
	for _, plugins := range []bool{false, true} {
		p := ChiefInstructionsFunctionTools(plugins)
		if strings.Contains(p, "mcp__") || strings.Contains(p, "muster --help") || !strings.Contains(p, "function tools") {
			t.Errorf("plugins=%v: the prose names MCP tools or the CLI", plugins)
		}
		if got := strings.Contains(p, "task_create_gated"); got != plugins {
			t.Errorf("plugins=%v: the plugin section present=%v", plugins, got)
		}
	}
}
