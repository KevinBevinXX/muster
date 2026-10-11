package main

import (
	"bytes"
	"encoding/base64"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func connectorKey(b byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32))
}

// TestConnectorConfigurationIsRefusedAtBootWhenIncoherent: each refusal names
// its own reason, and a coherent configuration loads.
func TestConnectorConfigurationIsRefusedAtBootWhenIncoherent(t *testing.T) {
	load := func(env map[string]string) error {
		full := map[string]string{envDatabase: "postgres://unused", envStandalone: "1"}
		for k, v := range env {
			full[k] = v
		}
		cfg, err := loadConfig(func(n string) string { return full[n] })
		if err != nil {
			return err
		}
		return cfg.validate()
	}
	ok := map[string]string{envConnectorEncKey: connectorKey(1), envConnectPort: "18191",
		envAgentConnectURL: "http://muster.example.test:18191/"}
	if err := load(ok); err != nil {
		t.Fatalf("control: a coherent configuration is refused: %v", err)
	}
	for name, c := range map[string]struct {
		env  map[string]string
		want string
	}{
		"short key":        {map[string]string{envConnectorEncKey: base64.StdEncoding.EncodeToString([]byte("short"))}, envConnectorEncKey},
		"key shared":       {map[string]string{envConnectorEncKey: connectorKey(2), envGitHubEncKey: connectorKey(2)}, "same key"},
		"port without key": {map[string]string{envConnectPort: "18191"}, envConnectPort},
		"port is main port": {map[string]string{envConnectorEncKey: connectorKey(1), envConnectPort: "8105",
			envAgentConnectURL: "http://muster.example.test:8105"}, "same as the main port"},
		// The store on with no URL for agents: every agent-facing recipe would
		// name an empty $MUSTER_CONNECT_URL.
		"key without url":          {map[string]string{envConnectorEncKey: connectorKey(1)}, envAgentConnectURL},
		"key and port without url": {map[string]string{envConnectorEncKey: connectorKey(1), envConnectPort: "18191"}, envAgentConnectURL},
		"bad port":                 {map[string]string{envConnectorEncKey: connectorKey(1), envConnectPort: "0"}, envConnectPort},
		"url without port":         {map[string]string{envConnectorEncKey: connectorKey(1), envAgentConnectURL: "http://muster.example.test:18191"}, envAgentConnectURL},
		"url without key":          {map[string]string{envAgentConnectURL: "http://muster.example.test:18191"}, envAgentConnectURL},
		"url with path":            {map[string]string{envConnectorEncKey: connectorKey(1), envConnectPort: "18191", envAgentConnectURL: "http://muster.example.test:18191/x"}, envAgentConnectURL},
		"url with userinfo":        {map[string]string{envConnectorEncKey: connectorKey(1), envConnectPort: "18191", envAgentConnectURL: "http://u:p@muster.example.test:18191"}, envAgentConnectURL},
		"url not http":             {map[string]string{envConnectorEncKey: connectorKey(1), envConnectPort: "18191", envAgentConnectURL: "ftp://muster.example.test"}, envAgentConnectURL},
	} {
		err := load(c.env)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want a refusal naming %q", name, err, c.want)
		}
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestTheConnectListenerIsBoundAndServesOnlyTheConnectorSurface drives the
// binary's own listen path with MUSTER_CONNECT_PORT set: the second port answers
// the agent connector routes (503 here — no database, so no store) and 404s the
// operator UI, which the main port still serves.
func TestTheConnectListenerIsBoundAndServesOnlyTheConnectorSurface(t *testing.T) {
	cfg := testConfig()
	cfg.ConnectorEncKey = bytes.Repeat([]byte{1}, 32)
	cfg.ConnectPort = freePort(t)
	main := startServer(t, cfg)
	connect := "http://127.0.0.1:" + strconv.Itoa(cfg.ConnectPort)

	get := func(url string) int {
		t.Helper()
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := get(connect + "/agent/connectors"); c != http.StatusServiceUnavailable {
		t.Errorf("connect listener /agent/connectors = %d, want 503 (served, no store)", c)
	}
	for _, p := range []string{"/ui/connectors", "/tasks", "/api/tasks", "/login", "/metrics"} {
		if c := get(connect + p); c != http.StatusNotFound {
			t.Errorf("connect listener %s = %d, want 404", p, c)
		}
	}
	if c := get(main + "/login"); c == http.StatusNotFound {
		t.Error("control: the main port does not serve /login")
	}
}
