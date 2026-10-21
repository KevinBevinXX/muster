package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/podagent"
)

// 🔴 THE RELAY TIMEOUT IS SHORTER THAN THE RENDERED approvals.timeout. The
// default holds it, the rendered config carries the timeout explicitly, and a
// configuration that would let Hermes time out first is refused at start.
func TestTheRelayTimeoutIsShorterThanApprovalsTimeout(t *testing.T) {
	cfg := testConfig(t)
	cfg.RelayTimeout = defaultRelayTimeout
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	rendered := renderedConfig(t, cfg)
	approvals, _ := rendered["approvals"].(map[string]any)
	timeout, _ := approvals["timeout"].(float64)
	if approvals["mode"] != "manual" || time.Duration(timeout)*time.Second != approvalsTimeout {
		t.Fatalf("rendered approvals %v; want manual with timeout %s", approvals, approvalsTimeout)
	}
	if defaultRelayTimeout >= time.Duration(timeout)*time.Second {
		t.Fatalf("THE DEFAULT RELAY TIMEOUT %s IS NOT SHORTER THAN THE RENDERED approvals.timeout %vs", defaultRelayTimeout, timeout)
	}
	for _, d := range []time.Duration{approvalsTimeout, approvalsTimeout + time.Second} {
		cfg.RelayTimeout = d
		if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "not shorter") {
			t.Fatalf("a relay timeout of %s was accepted (approvals.timeout %s): %v", d, approvalsTimeout, err)
		}
	}
	cfg.RelayTimeout = approvalsTimeout - time.Second
	if err := cfg.validate(); err != nil {
		t.Fatalf("a relay timeout just under approvals.timeout was refused: %v", err)
	}
}

func renderedConfig(t *testing.T, cfg config) map[string]any {
	t.Helper()
	if err := os.MkdirAll(cfg.HermesHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(cfg.configPath())
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(cfg.configPath()); fi.Mode().Perm() != 0o600 {
		t.Fatalf("config.yaml mode %v", fi.Mode().Perm())
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("config.yaml is not the JSON hermesd writes: %v", err)
	}
	return m
}

// The rendered config, key by key, as docs/design/hermes.md specifies it.
func TestTheRenderedConfig(t *testing.T) {
	cfg := testConfig(t)
	cfg.Workspace = "/data/workspace/tide-charts"
	cfg.Model = "anthropic/claude-sonnet-4.5"
	cfg.ConnectURL, cfg.AgentToken = "http://muster.example:18191", "agent-token"
	m := renderedConfig(t, cfg)
	b, _ := json.Marshal(m)
	got := string(b)
	for _, want := range []string{
		`"_config_version":51`,
		`"terminal":{"backend":"local","cwd":"/data/workspace/tide-charts"}`,
		`"updates":{"check":false}`,
		`"telemetry":{"shared_metrics":{"enabled":false,"send":false}}`,
		`"monitoring":{"export":{"otlp":{"enabled":false}},"gateway_health_export":{"enabled":false}}`,
		`"sessions":{"auto_prune":false}`,
		`"agent":{"disabled_toolsets":["browser","computer_use","tts"]}`,
		`"model":{"default":"anthropic/claude-sonnet-4.5","provider":"openrouter"}`,
		`"mcp_servers":{"muster":{"headers":{"X-Muster-Token":"${MUSTER_HOOK_TOKEN}"},"url":"http://muster.example:18191/mcp"}}`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("the rendered config lacks %s:\n%s", want, got)
		}
	}
	// No system prompt of any kind, no credential, and nothing that is the
	// agent token's VALUE: it is expanded by Hermes from the environment.
	for _, never := range []string{"system_prompt", "agent-token", "api_key", "yolo"} {
		if strings.Contains(strings.ToLower(got), never) {
			t.Fatalf("the rendered config holds %q:\n%s", never, got)
		}
	}
	// A stub model's base URL replaces OpenRouter, the key still expanded.
	cfg.ModelBaseURL = "http://127.0.0.1:18901/v1"
	m = renderedConfig(t, cfg)
	model, _ := m["model"].(map[string]any)
	if model["provider"] != "custom" || model["base_url"] != cfg.ModelBaseURL || model["api_key"] != "${OPENROUTER_API_KEY}" {
		t.Fatalf("custom model %v", model)
	}
}

// seed creates the layout (the agent shell's HOME included), never writes
// SOUL.md, .hermes.md or a .env, and refuses to start over a .env.
func TestSeed(t *testing.T) {
	cfg := testConfig(t)
	key, err := seed(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{cfg.agentHome(), cfg.tmpDir(), cfg.StateDir, cfg.Workspace} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Fatalf("seed did not create %s: %v", d, err)
		}
	}
	for _, never := range []string{filepath.Join(cfg.HermesHome, "SOUL.md"), filepath.Join(cfg.Workspace, ".hermes.md"), cfg.envFile()} {
		if _, err := os.Lstat(never); err == nil {
			t.Fatalf("seed wrote %s", never)
		}
	}
	// 🔴 THE KEY IS GENERATED ONCE, 0600, AND REUSED.
	fi, err := os.Stat(cfg.keyPath())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the API key file: %v %v", fi, err)
	}
	again, err := seed(cfg)
	if err != nil || again != key || len(key) < 32 {
		t.Fatalf("a second seed changed the key: %q -> %q (%v)", key, again, err)
	}
	if keyFingerprint(key) != keyFingerprint(again) || strings.Contains(keyFingerprint(key), key) {
		t.Fatalf("the fingerprint is unstable or contains the key")
	}
	// A loosened mode is put back.
	if err := os.Chmod(cfg.keyPath(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := seed(cfg); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(cfg.keyPath()); fi.Mode().Perm() != 0o600 {
		t.Fatalf("the key file stayed %v", fi.Mode().Perm())
	}
	// 🔴 A .env ON THE VOLUME: seed refuses, naming it, and deletes nothing.
	if err := os.WriteFile(cfg.envFile(), []byte("OPENROUTER_API_KEY=sk-or-x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := seed(cfg); !errors.Is(err, errDotEnv) || !strings.Contains(err.Error(), cfg.envFile()) {
		t.Fatalf("seed over a .env: %v", err)
	}
	if _, err := os.Stat(cfg.envFile()); err != nil {
		t.Fatalf("seed removed a .env it did not write: %v", err)
	}
}

// The gateway's environment: TMPDIR and the agent shell's HOME on the volume,
// TERMINAL_HOME_MODE=profile, the API server on loopback with the key; muster's
// bearer and raw hooks token never reach it, while the connector variables and
// the provider key do (Hermes strips the latter from the shell itself).
func TestTheGatewayEnvironment(t *testing.T) {
	cfg := testConfig(t)
	env := gatewayEnv(cfg, []string{
		"MUSTER_GATEWAY_BEARER=b", "HOOKS_TOKEN=h", "HERMES_YOLO_MODE=1", "API_SERVER_HOST=0.0.0.0",
		"HERMESD_REPO=o/r", "HOME=/somewhere", "TMPDIR=/tmp",
		"OPENROUTER_API_KEY=sk-or-x", "MUSTER_HOOK_TOKEN=agent", "MUSTER_CONNECT_URL=http://m", "PATH=/bin",
	}, "the-key")
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := got[k]; dup {
			t.Fatalf("%s set twice", k)
		}
		got[k] = v
	}
	want := map[string]string{
		"HOME": cfg.agentHome(), "TMPDIR": cfg.tmpDir(), "TERMINAL_HOME_MODE": "profile", "HERMES_HOME": cfg.HermesHome,
		"API_SERVER_ENABLED": "true", "API_SERVER_HOST": "127.0.0.1", "API_SERVER_PORT": "8642", "API_SERVER_KEY": "the-key",
		"OPENROUTER_API_KEY": "sk-or-x", "MUSTER_HOOK_TOKEN": "agent", "MUSTER_CONNECT_URL": "http://m", "PATH": "/bin",
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s=%q; want %q (env %v)", k, got[k], v, env)
		}
	}
	for _, k := range []string{"MUSTER_GATEWAY_BEARER", "HOOKS_TOKEN", "HERMES_YOLO_MODE", "HERMESD_REPO"} {
		if _, ok := got[k]; ok {
			t.Fatalf("%s reached the gateway", k)
		}
	}
	if !strings.HasPrefix(got["TMPDIR"], cfg.HermesHome+"/") {
		t.Fatalf("TMPDIR %q is not on the volume", got["TMPDIR"])
	}
}

// 🔴 THE CONNECTOR PROXY'S GIT CONFIG IS WRITTEN UNDER $HERMES_HOME/home, the
// agent shell's HOME (S8), by the same routing ccd uses (`ccd connect-git`,
// internal/podagent). hermesd points its own HOME there before the loop runs.
func TestGitRoutingIsWrittenUnderTheAgentShellsHome(t *testing.T) {
	cfg := testConfig(t)
	if _, err := seed(cfg); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	t.Setenv("HOME", elsewhere)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("XDG_CONFIG_HOME", "")
	if err := useAgentHome(cfg); err != nil {
		t.Fatal(err)
	}
	c := podagent.AgentConnector{Name: "github", Exists: true, Grant: "approved", Hosts: []string{"github.com"}}
	if _, err := podagent.ApplyGitRouting("http://muster.example:18191", "agent-token", c); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(cfg.agentHome(), ".gitconfig"))
	if err != nil {
		t.Fatalf("no .gitconfig under the agent shell's HOME: %v", err)
	}
	for _, want := range []string{
		`[url "http://muster.example:18191/connect/github/github.com/"]`, "insteadOf = https://github.com/",
		"followRedirects = false", "extraHeader = X-Muster-Token: agent-token",
	} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("the agent's .gitconfig lacks %q:\n%s", want, b)
		}
	}
	if _, err := os.Stat(filepath.Join(elsewhere, ".gitconfig")); err == nil {
		t.Fatal("git routing was written to hermesd's previous HOME")
	}
}
