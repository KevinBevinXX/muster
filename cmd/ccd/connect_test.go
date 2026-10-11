package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const fakeAgentToken = "fake-agent-token-for-ccd-tests"

func TestSeedRegistersMustersMCPServerOnlyWhenOffered(t *testing.T) {
	dir := t.TempDir()
	o := seedOpts{configDir: filepath.Join(dir, "claude"), workspace: filepath.Join(dir, "ws"),
		settingsTemplate: filepath.Join(dir, "settings.json")}
	if err := os.WriteFile(o.settingsTemplate, []byte(`{"hooks":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// An operator-registered server that must survive both directions.
	if err := os.MkdirAll(o.configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(o.configDir, ".claude.json"),
		[]byte(`{"mcpServers":{"other":{"type":"http","url":"https://mcp.example.com"}}}`), 0o600)

	t.Setenv(envConnectURL, "http://muster.example.test:18191/")
	t.Setenv(envAgentToken, fakeAgentToken)
	var stderr bytes.Buffer
	if err := seed(o, &stderr); err != nil {
		t.Fatal(err)
	}
	st, _ := readJSONObject(filepath.Join(o.configDir, ".claude.json"), nil)
	servers, _ := st["mcpServers"].(map[string]any)
	m, _ := servers["muster"].(map[string]any)
	if m["type"] != "http" || m["url"] != "http://muster.example.test:18191/mcp" {
		t.Fatalf("muster server = %v", m)
	}
	if h, _ := m["headers"].(map[string]any); h["X-Muster-Token"] != fakeAgentToken {
		t.Fatalf("headers = %v", m["headers"])
	}
	if servers["other"] == nil {
		t.Fatal("an operator's server was dropped")
	}
	if err := checkSeed(o); err != nil {
		t.Fatalf("check with the store offered: %v", err)
	}

	t.Setenv(envConnectURL, "")
	if err := checkSeed(o); err == nil {
		t.Fatal("check passed with a stale muster server registered and no store offered")
	}
	if err := seed(o, &stderr); err != nil {
		t.Fatal(err)
	}
	st, _ = readJSONObject(filepath.Join(o.configDir, ".claude.json"), nil)
	servers, _ = st["mcpServers"].(map[string]any)
	if _, ok := servers["muster"]; ok || servers["other"] == nil {
		t.Fatalf("without the store: servers = %v, want only the operator's", servers)
	}
	if err := checkSeed(o); err != nil {
		t.Fatalf("check without the store: %v", err)
	}
}

// fakeMuster answers /agent/connectors/{name} with a scripted grant, records any
// POST (connect-git must never file a request), and records whatever reaches
// /connect/ (git's smart-HTTP discovery request).
type fakeMuster struct {
	srv     *httptest.Server
	mu      sync.Mutex
	grant   string
	listed  bool // whether /agent/connectors lists the connector at all
	posted  bool
	proxied []*http.Request
	// onProxy, when set, answers /connect/ requests instead of a 404.
	onProxy func(w http.ResponseWriter, r *http.Request)
}

func newFakeMuster(t *testing.T) *fakeMuster {
	f := &fakeMuster{grant: "approved", listed: true}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/connect/") {
			f.proxied = append(f.proxied, r.Clone(r.Context()))
			if f.onProxy != nil {
				f.onProxy(w, r)
				return
			}
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-Muster-Token") != fakeAgentToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			f.posted = true
		}
		view := map[string]any{"name": "github", "exists": true,
			"hosts": []string{"api.github.com", "github.com"}, "grant": f.grant}
		if r.URL.Path == "/agent/connectors" {
			list := []any{}
			if f.listed {
				list = append(list, view)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"connectors": list})
			return
		}
		_ = json.NewEncoder(w).Encode(view)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func gitEnv(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("git is required (the image and CI both have it)")
	}
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	return cfg
}

// TestConnectGitRoutesGitThroughTheProxyOnlyWhenApproved: approved, a real `git
// ls-remote https://github.com/...` arrives at muster's proxy path carrying the
// agent's token; not approved, the routing is removed and git goes direct.
func TestConnectGitRoutesGitThroughTheProxyOnlyWhenApproved(t *testing.T) {
	cfgPath := gitEnv(t)
	f := newFakeMuster(t)
	t.Setenv(envConnectURL, f.srv.URL)
	t.Setenv(envAgentToken, fakeAgentToken)

	var out, errb bytes.Buffer
	if rc := runConnectGit(nil, &out, &errb); rc != 0 {
		t.Fatalf("approved: rc=%d %s %s", rc, out.String(), errb.String())
	}
	_ = exec.Command("git", "ls-remote", "https://github.com/example-org/repo.git").Run()
	f.mu.Lock()
	if len(f.proxied) == 0 {
		f.mu.Unlock()
		cfg, _ := os.ReadFile(cfgPath)
		t.Fatalf("git did not go through the proxy; config:\n%s", cfg)
	}
	got := f.proxied[0]
	f.mu.Unlock()
	if !strings.HasPrefix(got.URL.Path, "/connect/github/github.com/example-org/repo.git/info/refs") {
		t.Fatalf("git asked %s", got.URL.Path)
	}
	if got.Header.Get("X-Muster-Token") != fakeAgentToken {
		t.Fatal("git did not send the agent's token to the proxy")
	}
	// Idempotent: a second run leaves one insteadOf per host.
	if rc := runConnectGit(nil, &out, &errb); rc != 0 {
		t.Fatal("second run failed")
	}
	keys, _ := exec.Command("git", "config", "--global", "--get-all",
		"url."+f.srv.URL+"/connect/github/github.com/.insteadOf").Output()
	if strings.Count(string(keys), "https://github.com/") != 1 {
		t.Fatalf("insteadOf after two runs = %q", keys)
	}

	f.mu.Lock()
	f.grant = "revoked"
	f.mu.Unlock()
	out.Reset()
	if rc := runConnectGit(nil, &out, &errb); rc != exitNotApproved {
		t.Fatalf("revoked: rc=%d", rc)
	}
	cfg, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(cfg), "/connect/github/") || strings.Contains(string(cfg), fakeAgentToken) {
		t.Fatalf("routing (or the token) left behind after revocation:\n%s", cfg)
	}

	if !strings.Contains(out.String(), "connector_request tool") || strings.Contains(out.String(), "--request") {
		t.Fatalf("the not-approved message does not point at the one request path: %q", out.String())
	}
	if f.posted {
		t.Fatal("connect-git filed a request; asking is the connector_request tool's job only")
	}
	// The removed flags are gone, not silently accepted.
	if rc := runConnectGit([]string{"--request"}, &out, &errb); rc != 2 {
		t.Fatalf("--request: rc=%d, want 2 (unknown flag)", rc)
	}
}

func TestConnectGitRefusesWithoutTheStore(t *testing.T) {
	gitEnv(t)
	t.Setenv(envConnectURL, "")
	t.Setenv(envAgentToken, fakeAgentToken)
	var out, errb bytes.Buffer
	if rc := runConnectGit(nil, &out, &errb); rc != 1 || !strings.Contains(errb.String(), envConnectURL) {
		t.Fatalf("rc=%d %s", rc, errb.String())
	}
}
