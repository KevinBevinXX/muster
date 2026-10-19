package main

import (
	"reflect"
	"strings"
	"testing"
)

// MUSTER_CHIEF_SESSION_RUNTIMES: off unless set; a comma list of the session
// runtimes; an unknown or repeated entry, or claude-code without that kind,
// refuses to boot naming the variable. And validate() reaches the check (the
// refusal is not only reachable by calling it directly).
func TestChiefSessionRuntimesConfig(t *testing.T) {
	load := func(env map[string]string) config {
		t.Helper()
		cfg, err := loadConfig(func(n string) string { return env[n] })
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	if cfg := load(nil); len(cfg.ChiefSessionRuntimes) != 0 || cfg.validateSessionRuntimes() != nil {
		t.Fatalf("unset: %v", cfg.ChiefSessionRuntimes)
	}
	cc := map[string]string{envAgentKinds: "gateway,claude-code"}
	with := func(v string, kinds bool) config {
		env := map[string]string{envChiefSessionRuntimes: v}
		if kinds {
			for k, x := range cc {
				env[k] = x
			}
		}
		return load(env)
	}
	if cfg := with(" claude-code , gateway ", true); !reflect.DeepEqual(cfg.ChiefSessionRuntimes, []string{"claude-code", "gateway"}) ||
		cfg.validateSessionRuntimes() != nil {
		t.Fatalf("both: %v %v", cfg.ChiefSessionRuntimes, cfg.validateSessionRuntimes())
	}
	if err := with("gateway", false).validateSessionRuntimes(); err != nil {
		t.Errorf("gateway alone on the gateway kind: %v", err)
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
		if err := cfg.validateSessionRuntimes(); err == nil || !strings.Contains(err.Error(), envChiefSessionRuntimes) {
			t.Errorf("%s (%q) was not refused naming the variable: %v", bad.why, bad.v, err)
		}
		cfg.Database = "postgres://example.invalid/db"
		if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), envChiefSessionRuntimes) {
			t.Errorf("%s (%q): validate() did not reach the refusal: %v", bad.why, bad.v, err)
		}
	}
}
