package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/api"
)

// MUSTER_CHIEF_SESSION_RUNTIMES: off unless set; a comma list of the session
// runtimes (claude-code is the only one); an unknown or repeated entry, or
// claude-code without that kind, refuses to boot naming the variable. And validate() reaches the check (the
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
	cc := map[string]string{envAgentKinds: "gateway,claude-code", envAgentGateway: gatewayHooksSHA256}
	with := func(v string, kinds bool) config {
		env := map[string]string{envChiefSessionRuntimes: v}
		if kinds {
			for k, x := range cc {
				env[k] = x
			}
		}
		return load(env)
	}
	if cfg := with(" claude-code ", true); !reflect.DeepEqual(cfg.ChiefSessionRuntimes, []string{"claude-code"}) ||
		cfg.validateSessionRuntimes() != nil || cfg.ChiefSessionRuntimeGatewayIgnored {
		t.Fatalf("claude-code: %v %v", cfg.ChiefSessionRuntimes, cfg.validateSessionRuntimes())
	}
	for _, bad := range []struct {
		v     string
		kinds bool
		why   string
	}{
		{"webchat", false, "an unknown runtime"},
		{"openrouter", false, "another unknown runtime"},
		{"claude-code,claude-code", false, "a repeated runtime"},
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

// 🔴 THE RETIRED `gateway` ENTRY BOOTS, IGNORED, WITH ONE WARNING. The live
// deployment names "claude-code,gateway", and this image must be able to ship
// before that manifest changes: the entry is dropped (it never reaches the
// picker or validation), and the banner says so on exactly one line naming the
// variable and the entry. A value without it carries no warning. Everything
// else about the variable is as before: "claude-code" survives beside it.
func TestTheRetiredGatewaySessionRuntimeBootsIgnoredWithOneWarning(t *testing.T) {
	cc := map[string]string{envAgentKinds: "gateway,claude-code", envAgentGateway: gatewayHooksSHA256}
	for _, c := range []struct {
		v      string
		kinds  bool
		want   []string
		warned bool
		picker bool
	}{
		{"claude-code,gateway", true, []string{"claude-code"}, true, true},
		{"gateway", false, nil, true, false},
		{" gateway , claude-code ", true, []string{"claude-code"}, true, true},
		{"claude-code", true, []string{"claude-code"}, false, true},
	} {
		env := map[string]string{envChiefSessionRuntimes: c.v}
		if c.kinds {
			for k, x := range cc {
				env[k] = x
			}
		}
		cfg, err := loadConfig(func(n string) string { return env[n] })
		if err != nil {
			t.Fatalf("%q: %v", c.v, err)
		}
		cfg.Database = "postgres://example.invalid/db"
		if err := cfg.validateSessionRuntimes(); err != nil {
			t.Errorf("%q: refused: %v", c.v, err)
		}
		// The whole boot check, where the fixture is otherwise bootable (the
		// kinds fixture lacks the rest of a claude-code deployment's settings).
		if !c.kinds {
			if err := cfg.validate(); err != nil {
				t.Errorf("%q: validate() refused: %v", c.v, err)
			}
		}
		if len(cfg.ChiefSessionRuntimes) != len(c.want) || (len(c.want) > 0 && !reflect.DeepEqual(cfg.ChiefSessionRuntimes, c.want)) ||
			cfg.ChiefSessionRuntimeGatewayIgnored != c.warned {
			t.Errorf("%q: runtimes %v, ignored %v; want %v, %v", c.v, cfg.ChiefSessionRuntimes, cfg.ChiefSessionRuntimeGatewayIgnored, c.want, c.warned)
		}
		ext := api.Extensions{}
		if c.picker {
			ext.SessionRuntimes = cfg.ChiefSessionRuntimes
		}
		out := renderBanner(t, cfg, ext)
		n := 0
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, envChiefSessionRuntimes) && strings.Contains(l, `"gateway"`) && strings.Contains(l, "ignored") {
				n++
			}
		}
		if want := map[bool]int{true: 1, false: 0}[c.warned]; n != want {
			t.Errorf("%q: %d warning line(s) about the retired entry, want %d:\n%s", c.v, n, want, out)
		}
	}
}
