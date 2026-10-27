package main

import (
	"reflect"
	"strings"
	"testing"
)

// MUSTER_AGENT_SESSION_RUNTIMES: off unless set; a comma list of the session
// runtimes; an unknown or repeated entry, or claude-code without that kind,
// refuses to boot naming the variable. And validate() reaches the check (the
// refusal is not only reachable by calling it directly).
func TestAgentSessionRuntimesConfig(t *testing.T) {
	load := func(env map[string]string) config {
		t.Helper()
		cfg, err := loadConfig(func(n string) string { return env[n] })
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	if cfg := load(nil); len(cfg.AgentSessionRuntimes) != 0 || cfg.validateSessionRuntimes() != nil {
		t.Fatalf("unset: %v", cfg.AgentSessionRuntimes)
	}
	cc := map[string]string{envAgentKinds: "gateway,claude-code", envAgentGateway: gatewayHooksSHA256}
	with := func(v string, kinds bool) config {
		env := map[string]string{envAgentSessionRuntimes: v}
		if kinds {
			for k, x := range cc {
				env[k] = x
			}
		}
		return load(env)
	}
	if cfg := with(" claude-code , gateway ", true); !reflect.DeepEqual(cfg.AgentSessionRuntimes, []string{"claude-code", "gateway"}) ||
		cfg.validateSessionRuntimes() != nil {
		t.Fatalf("both: %v %v", cfg.AgentSessionRuntimes, cfg.validateSessionRuntimes())
	}
	if err := with("gateway", false).validateSessionRuntimes(); err != nil {
		t.Errorf("gateway alone on the gateway kind: %v", err)
	}
	// A claude-code agent runs the gateway runtime BESIDE ccd, built from the
	// runtime bundle: with the kind on and no bundle, gateway sessions are refused.
	noBundle := load(map[string]string{envAgentSessionRuntimes: "gateway", envAgentKinds: "gateway,claude-code"})
	if err := noBundle.validateSessionRuntimes(); err == nil || !strings.Contains(err.Error(), envAgentGateway) {
		t.Errorf("gateway sessions on a claude-code deployment with no runtime bundle: %v; want a refusal naming %s", err, envAgentGateway)
	}
	for _, bad := range []struct {
		v     string
		kinds bool
		why   string
	}{
		{"webchat", false, "an unknown runtime"},
		{"gateway,gateway", false, "a repeated runtime"},
		{"claude-code", false, "claude-code with that kind off"},
	} {
		cfg := with(bad.v, bad.kinds)
		if err := cfg.validateSessionRuntimes(); err == nil || !strings.Contains(err.Error(), envAgentSessionRuntimes) {
			t.Errorf("%s (%q) was not refused naming the variable: %v", bad.why, bad.v, err)
		}
		cfg.Database = "postgres://example.invalid/db"
		if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), envAgentSessionRuntimes) {
			t.Errorf("%s (%q): validate() did not reach the refusal: %v", bad.why, bad.v, err)
		}
	}
}
