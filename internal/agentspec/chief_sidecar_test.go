package agentspec

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// sidecarConfig is ccConfig with an install script that serves the gateway
// sidecar on the port muster names (installHonoursGatewayPort).
func sidecarConfig() Config {
	cfg := ccConfig()
	cfg.RuntimeConfig.Install = []byte("set -eu\nexec /example/entrypoint --flag --port \"${MUSTER_GATEWAY_PORT:-18789}\"\n")
	return cfg
}

func envMap(env []provision.EnvVar) map[string]string {
	m := map[string]string{}
	for _, e := range env {
		m[e.Name] = e.Value
	}
	return m
}

// TestAClaudeCodeChiefRunsTheGatewayRuntimeBesideCcd pins the sidecar as
// LITERALS: the gateway kind's image, install command and runtime files; its own
// credentials (its OWN token and the bearer derived from it, the OpenRouter key)
// and NOT the Claude token or the chief's hooks token; HOME and the loopback port; no working directory; and ccd
// told where it is. The instance's own Secrets hold neither gateway credential.
func TestAClaudeCodeChiefRunsTheGatewayRuntimeBesideCcd(t *testing.T) {
	a, cfg := ccAgent(), sidecarConfig()
	spec := mustBuild(t, a, cfg, Options{Instructions: "cc prose", GatewaySidecar: &GatewaySidecar{}})
	sc := spec.Sidecar
	if sc == nil {
		t.Fatal("no sidecar")
	}
	if sc.Name != "gateway" || sc.Runtime.Image != "ghcr.io/example-org/agent-runtime:3.11.0" ||
		len(sc.Runtime.Command) != 3 || sc.Runtime.Command[0] != "sh" || sc.Runtime.Command[1] != "-c" ||
		!strings.Contains(sc.Runtime.Command[2], "  sh -eu /muster/runtime/install &\n") || sc.Runtime.WorkingDir != "" {
		t.Errorf("sidecar runtime %+v", sc.Runtime)
	}
	env := envMap(sc.Env)
	for name, want := range map[string]string{
		"HOME": "/home/node", "MUSTER_GATEWAY_PORT": "18791", "MUSTER_RUNTIME_CONFIG": "/muster/runtime/config",
		// The gateway kind's model config for this row, fallbacks included.
		// No workspace: the sidecar mounts none.
		"MUSTER_CONFIG": `{"model":{"fallbacks":["openrouter/vendor-c/model-three","openrouter/vendor-d/model-four"],"primary":"openrouter/vendor-b/model-two"}}`,
	} {
		if env[name] != want {
			t.Errorf("sidecar env %s = %q, want %q", name, env[name], want)
		}
	}
	sec := envMap(sc.Secrets)
	// The sidecar's OWN token: sha256("muster/chief-gateway-sidecar/v1\x00" +
	// the fixture's token), computed outside Go with sha256sum. Never the chief's.
	const sidecarToken = "588121da36f719bd9da276c4aaba848d5f73b4e6d5ea83d42204d5522c8acc21"
	if a.HooksToken != "fixture-per-agent-token-9f31c7" {
		t.Fatalf("the fixture's token moved (%q); recompute sidecarToken", a.HooksToken)
	}
	if sec["MUSTER_GATEWAY_BEARER"] != fixtureDeriveBearer(sidecarToken) || sec["HOOKS_TOKEN"] != sidecarToken ||
		sec["OPENROUTER_API_KEY"] != cfg.OpenRouterAPIKey || cfg.OpenRouterAPIKey == "" || len(sec) != 3 {
		t.Errorf("sidecar secrets %v", names(sc.Secrets))
	}
	if envMap(spec.Secrets)["CCD_GATEWAY_UPSTREAM_BEARER"] != fixtureDeriveBearer(sidecarToken) {
		t.Errorf("ccd is not handed the sidecar's credential to forward with: %v", names(spec.Secrets))
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
	cfg := sidecarConfig()
	cfg.RuntimeConfig = RuntimeConfig{}
	if _, err := Build(ccAgent(), cfg, Options{GatewaySidecar: &GatewaySidecar{}}); err == nil || !strings.Contains(err.Error(), "runtime bundle") {
		t.Errorf("no runtime bundle: %v", err)
	}
	g := fixtureAgent()
	g.Kind = agents.KindGateway
	if _, err := Build(g, sidecarConfig(), Options{GatewaySidecar: &GatewaySidecar{}}); err == nil {
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

// TestTheGatewaySidecarIsRefusedWhenTheInstallScriptIgnoresItsPort: an install
// script that never expands MUSTER_GATEWAY_PORT would run the runtime on its
// default port — ccd's, in the same pod — so the build is refused, naming the
// variable and the port. Mentioning the name in a comment, or a longer variable
// that starts with it, is not reading it; ${…:-default} and "$…" are.
func TestTheGatewaySidecarIsRefusedWhenTheInstallScriptIgnoresItsPort(t *testing.T) {
	for _, c := range []struct {
		name, install string
		ok            bool
	}{
		{"the pre-sidecar script", "set -eu\nexec /example/entrypoint serve --bind lan\n", false},
		{"named only in a comment", "set -eu\n  # serves on $MUSTER_GATEWAY_PORT\nexec /example/entrypoint serve\n", false},
		{"a longer variable", "set -eu\nexec /example/entrypoint serve --port \"$MUSTER_GATEWAY_PORTS\"\n", false},
		{"a bare name, no expansion", "set -eu\necho MUSTER_GATEWAY_PORT\nexec /example/entrypoint serve\n", false},
		{"quoted expansion", "set -eu\nexec /example/entrypoint serve --port \"$MUSTER_GATEWAY_PORT\"\n", true},
		{"braced with a default", "set -eu\nexec /example/entrypoint serve --port \"${MUSTER_GATEWAY_PORT:-18789}\"\n", true},
	} {
		cfg := sidecarConfig()
		cfg.RuntimeConfig.Install = []byte(c.install)
		spec, err := Build(ccAgent(), cfg, Options{GatewaySidecar: &GatewaySidecar{}})
		if c.ok {
			if err != nil || spec.Sidecar == nil {
				t.Errorf("%s: refused (%v); want a sidecar", c.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: built a sidecar; want a refusal", c.name)
			continue
		}
		for _, want := range []string{"MUSTER_GATEWAY_PORT", "18789", "MUSTER_AGENT_RUNTIME_INSTALL"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the refusal does not name %s: %v", c.name, want, err)
			}
		}
	}
	// Without the sidecar the same script builds: the gateway KIND is unaffected.
	cfg := sidecarConfig()
	cfg.RuntimeConfig.Install = []byte("set -eu\nexec /example/entrypoint serve --bind lan\n")
	if _, err := Build(ccAgent(), cfg, Options{}); err != nil {
		t.Errorf("a claude-code agent with no sidecar was refused over the install script: %v", err)
	}
	g := fixtureAgent()
	g.Kind = agents.KindGateway
	if _, err := Build(g, cfg, Options{}); err != nil {
		t.Errorf("a gateway-kind agent was refused over the install script: %v", err)
	}
}

// TestTheGatewaySidecarOutlivesItsRuntime runs the sidecar's BUILT command, with
// the install script's path pointed at a stand-in, under a real shell: a runtime
// that exits is run again and the container's process does not exit with it (a
// container that exits takes the whole pod NotReady, ccd's endpoint included);
// TERM reaches the running runtime and the process then exits 0 promptly.
func TestTheGatewaySidecarOutlivesItsRuntime(t *testing.T) {
	spec := mustBuild(t, ccAgent(), sidecarConfig(), Options{GatewaySidecar: &GatewaySidecar{}})
	cmd := append([]string(nil), spec.Sidecar.Runtime.Command...)
	dir := t.TempDir()
	runs, stub, alive := filepath.Join(dir, "runs"), filepath.Join(dir, "install"), filepath.Join(dir, "alive")
	// The first two runs exit at once (a crash); the third stays up, as a serving
	// runtime does, until it is signalled.
	if err := os.WriteFile(stub, []byte(`echo run >> "`+runs+`"
[ "$(wc -l < "`+runs+`")" -ge 3 ] || exit 7
trap 'rm -f "`+alive+`"; exit 0' TERM
touch "`+alive+`"
while :; do sleep 0.05; done
`), 0o600); err != nil {
		t.Fatal(err)
	}
	last := cmd[len(cmd)-1]
	if !strings.Contains(last, RuntimeInstallPath) {
		t.Fatalf("the sidecar's command does not run %s: %q", RuntimeInstallPath, cmd)
	}
	cmd[len(cmd)-1] = strings.ReplaceAll(last, RuntimeInstallPath, stub)
	// The built delay, shortened so the test does not wait it out.
	cmd[len(cmd)-1] = strings.ReplaceAll(cmd[len(cmd)-1], "sleep 10", "sleep 0.1")
	p := exec.Command(cmd[0], cmd[1:]...)
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.Wait() }()
	t.Cleanup(func() { _ = p.Process.Kill() })

	deadline := time.After(10 * time.Second)
	for {
		if _, err := os.Stat(alive); err == nil {
			break
		}
		select {
		case err := <-done:
			b, _ := os.ReadFile(runs)
			t.Fatalf("the sidecar's process exited (%v) after the runtime did; runs: %q", err, b)
		case <-deadline:
			t.Fatal("the runtime was not run a third time within 10s")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err := p.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("after TERM the sidecar's process exited %v; want 0", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the sidecar's process did not exit within 5s of TERM")
	}
	if _, err := os.Stat(alive); err == nil {
		t.Error("TERM did not reach the running runtime (its trap never ran)")
	}
}
