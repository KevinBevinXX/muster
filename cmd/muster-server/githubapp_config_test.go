package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/github"
)

func testAppKeyPEM(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}

// TestGitHubAppConfigurationIsRefusedAtBootWhenIncoherent: half a pair, a key
// that does not parse, a bad ID, an installation without an App, and an App
// without a connector store each refuse to boot, naming the variable — and the
// key's VALUE never appears in the refusal.
func TestGitHubAppConfigurationIsRefusedAtBootWhenIncoherent(t *testing.T) {
	keyPEM := testAppKeyPEM(t)
	load := func(env map[string]string) (config, error) {
		full := map[string]string{envDatabase: "postgres://unused", envStandalone: "1"}
		for k, v := range env {
			full[k] = v
		}
		cfg, err := loadConfig(func(n string) string { return full[n] })
		if err != nil {
			return cfg, err
		}
		return cfg, cfg.validate()
	}
	store := map[string]string{envConnectorEncKey: connectorKey(1), envConnectPort: "18191",
		envAgentConnectURL: "http://muster.example.test:18191/"}
	with := func(extra map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range store {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	cfg, err := load(with(map[string]string{envGitHubAppID: "12345", envGitHubAppKey: keyPEM, envGitHubAppAccounts: "example-org, Second-Org"}))
	if err != nil {
		t.Fatalf("control: a coherent App configuration is refused: %v", err)
	}
	if cfg.GitHubAppID != 12345 || cfg.GitHubAppKey == nil || strings.Join(cfg.GitHubAppAccounts, ",") != "example-org,second-org" {
		t.Fatalf("control: loaded %d / key=%v / %q", cfg.GitHubAppID, cfg.GitHubAppKey != nil, cfg.GitHubAppAccounts)
	}
	// A PEM block of the right type holding bytes that are not a key, built at
	// run time, so no key-shaped literal sits in this public repository.
	garbage := string(pem.EncodeToMemory(&pem.Block{Type: "RSA " + "PRIVATE KEY", Bytes: []byte("not-a-key-synthetic")}))
	for name, c := range map[string]struct {
		env  map[string]string
		want string
	}{
		"no account allowlist":   {with(map[string]string{envGitHubAppID: "12345", envGitHubAppKey: keyPEM}), envGitHubAppAccounts},
		"bad account":            {with(map[string]string{envGitHubAppID: "12345", envGitHubAppKey: keyPEM, envGitHubAppAccounts: "ok,not a login"}), envGitHubAppAccounts},
		"id without key":         {with(map[string]string{envGitHubAppID: "12345", envGitHubAppAccounts: "example-org"}), envGitHubAppKey},
		"key without id":         {with(map[string]string{envGitHubAppKey: keyPEM, envGitHubAppAccounts: "example-org"}), envGitHubAppID},
		"id not a number":        {with(map[string]string{envGitHubAppID: "my-app", envGitHubAppKey: keyPEM, envGitHubAppAccounts: "example-org"}), envGitHubAppID},
		"key does not parse":     {with(map[string]string{envGitHubAppID: "12345", envGitHubAppKey: garbage, envGitHubAppAccounts: "example-org"}), envGitHubAppKey},
		"key is a token":         {with(map[string]string{envGitHubAppID: "12345", envGitHubAppKey: "ghp_synthetic0123456789", envGitHubAppAccounts: "example-org"}), envGitHubAppKey},
		"accounts without app":   {with(map[string]string{envGitHubAppAccounts: "example-org"}), envGitHubAppID},
		"app without connectors": {map[string]string{envGitHubAppID: "12345", envGitHubAppKey: keyPEM, envGitHubAppAccounts: "example-org"}, envConnectorEncKey},
	} {
		_, err := load(c.env)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want a refusal naming %q", name, err, c.want)
			continue
		}
		if strings.Contains(err.Error(), "not-a-key-synthetic") || strings.Contains(err.Error(), "ghp_synthetic") ||
			strings.Contains(err.Error(), "PRIVATE KEY-----\n") {
			t.Errorf("%s: the refusal quotes the key's value: %v", name, err)
		}
	}
}

// TestTheCheckoutWaitIsConfigurableWithinBounds.
func TestTheCheckoutWaitIsConfigurableWithinBounds(t *testing.T) {
	load := func(v string) (config, error) {
		return loadConfig(func(n string) string {
			if n == envCheckoutWaitTimeout {
				return v
			}
			return ""
		})
	}
	if c, err := load(""); err != nil || c.CheckoutWaitTimeout != 0 {
		t.Fatalf("unset: %v %v", c.CheckoutWaitTimeout, err)
	}
	if c, err := load("72h"); err != nil || c.CheckoutWaitTimeout != 72*time.Hour {
		t.Fatalf("72h: %v %v", c.CheckoutWaitTimeout, err)
	}
	for _, bad := range []string{"30m", "721h", "a day", "-5h"} {
		if _, err := load(bad); err == nil || !strings.Contains(err.Error(), envCheckoutWaitTimeout) {
			t.Errorf("%q accepted (err=%v)", bad, err)
		}
	}
}

// TestBuildAppPresentsTheAppAsTheGitHubConnector is the WIRING seam: the
// binary, configured with an App, answers an agent's GET
// /agent/connectors/github with exists=true (no stored row), so ccd's routing
// and the operator's Approve both see it. A buildApp that set ext.GitHubApp
// but forgot connectors.WithVirtual passes every api-package test and fails
// here.
func TestBuildAppPresentsTheAppAsTheGitHubConnector(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`)) // the boot check: no installations
	}))
	t.Cleanup(fake.Close)
	github.UseAPIBaseURL(fake.URL)
	t.Cleanup(func() { github.UseAPIBaseURL("https://api.github.com") })

	key, err := loadConfig(func(n string) string {
		switch n {
		case envGitHubAppID:
			return "12345"
		case envGitHubAppKey:
			return testAppKeyPEM(t)
		case envGitHubAppAccounts:
			return "example-org"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.Database = dbtest.DSN(t)
	cfg.ConnectorEncKey = []byte(strings.Repeat("k", 32))
	cfg.GitHubAppID, cfg.GitHubAppKey, cfg.GitHubAppAccounts = key.GitHubAppID, key.GitHubAppKey, key.GitHubAppAccounts
	const tok = "wiring-agent-own-token-0123456789"
	seedRelayAgent(t, cfg.Database, "wiring-agent", tok)
	base := startServer(t, cfg)
	code, body := relayCall(t, http.MethodGet, base+"/agent/connectors/github", "", tok)
	if code != http.StatusOK {
		t.Fatalf("GET /agent/connectors/github = %d %s", code, body)
	}
	var v struct {
		Exists bool
		Grant  string
	}
	_ = json.Unmarshal([]byte(body), &v)
	if !v.Exists || v.Grant != "none" {
		t.Fatalf("with the App configured the agent sees %s; want exists=true, grant none", body)
	}
	if agents.CheckoutWaitTimeout != agents.DefaultCheckoutWaitTimeout {
		t.Fatalf("an unset wait changed the package bound to %s", agents.CheckoutWaitTimeout)
	}
}
