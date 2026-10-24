package main

import (
	"strings"
	"testing"
	"time"
)

// 🔴 R1: MUSTER_ROUTE_RELAY_ALLOW binds each sender to a plugin. The old
// "<uid>:<address>" form is REFUSED at boot — never read as something else —
// and so is a non-ASCII address.
func TestTheRelayAllowlistNeedsAPluginAndAnASCIIAddress(t *testing.T) {
	load := func(v string) (config, error) {
		return loadConfig(func(n string) string {
			if n == envRouteRelayAllow {
				return v
			}
			return ""
		})
	}
	cfg, err := load("1:example-inbox:ada@example.invalid, 2:example-inbox:*@corp.example.invalid")
	if err != nil || cfg.RouteRelayAllow.Owners() != 2 {
		t.Fatalf("the plugin-bound form: %v, %d owners", err, cfg.RouteRelayAllow.Owners())
	}
	// An agent-mail owner wildcard (and an exact agent address) boots beside a
	// mail entry, and is read as an entry rather than refused.
	cfg, err = load("1:agent-mail:example-owner/*, 1:agent-mail:hq/tasks, 1:example-inbox:ada@example.invalid")
	if err != nil {
		t.Fatalf("the agent-mail form: %v", err)
	}
	if !cfg.RouteRelayAllow.Allows(1, "agent-mail", "example-owner/agent-infra", true) || !cfg.RouteRelayAllow.Allows(1, "agent-mail", "hq/tasks", true) {
		t.Fatal("the agent-mail entries parsed but match nothing")
	}
	for _, bad := range []string{"1:agent-mail:Example-owner/*", "1:agent-mail:*/*", "1:agent-mail:example-owner"} {
		if _, err := load(bad); err == nil || !strings.Contains(err.Error(), envRouteRelayAllow) {
			t.Errorf("%q: booted (%v)", bad, err)
		}
	}
	for _, bad := range []string{"1:ada@example.invalid", "1:example-inbox:Kate@example.invalid"} {
		if _, err := load(bad); err == nil || !strings.Contains(err.Error(), envRouteRelayAllow) {
			t.Errorf("%q: booted (%v)", bad, err)
		}
	}
}

// MUSTER_ROUTE_TIMEOUT: unset is api.DefaultRouteTimeout (30m, the deadline
// before the variable existed); a duration inside [1m, 24h] is taken as given
// and reaches the server's Routing; anything else refuses to boot, naming the
// variable.
func TestTheRouteTimeoutDefaultsParsesAndRefusesNonsense(t *testing.T) {
	load := func(v string) (config, error) {
		return loadConfig(func(n string) string {
			if n == envRouteTimeout {
				return v
			}
			return ""
		})
	}
	cfg, err := load("")
	if err != nil || cfg.RouteTimeout != 30*time.Minute {
		t.Fatalf("unset: %v, %v; want 30m", cfg.RouteTimeout, err)
	}
	// Values distinct from the default and from each other, at both bounds and
	// in the middle, so a parser that ignored the variable cannot pass.
	for v, want := range map[string]time.Duration{"1m": time.Minute, "45m": 45 * time.Minute, " 2h ": 2 * time.Hour, "24h": 24 * time.Hour} {
		cfg, err := load(v)
		if err != nil || cfg.RouteTimeout != want {
			t.Errorf("%q: %v, %v; want %v", v, cfg.RouteTimeout, err, want)
			continue
		}
		if got := sessionRouting(cfg).Timeout; got != want {
			t.Errorf("%q: the server's Routing.Timeout is %v, want %v", v, got, want)
		}
	}
	for _, bad := range []string{"soon", "30", "0", "-5m", "59s", "24h1s", "1000h"} {
		if _, err := load(bad); err == nil || !strings.Contains(err.Error(), envRouteTimeout) {
			t.Errorf("%q: booted (%v)", bad, err)
		}
	}
}

// With session routing on, a route deadline that is not LONGER than the send
// queue's idle wait refuses to boot — whichever variable is the one set too
// far (route short, or idle long), and at equality — naming both variables. A
// longer deadline boots, and with routing off a long idle wait alone boots.
func TestTheRouteTimeoutMustOutlastTheSendQueueIdleWait(t *testing.T) {
	load := func(routing bool, timeout, idle string) (config, error) {
		env := map[string]string{envRouteTimeout: timeout, envSendQueueIdle: idle}
		if routing {
			env[envSessionRouting] = "1"
		}
		return loadConfig(func(n string) string { return env[n] })
	}
	for _, c := range []struct {
		name, timeout, idle string
	}{
		{"the route deadline set below the idle wait", "2m", "5m"},
		{"the idle wait set above the default deadline", "", "45m"},
		{"equal", "10m", "10m"},
	} {
		_, err := load(true, c.timeout, c.idle)
		if err == nil || !strings.Contains(err.Error(), envRouteTimeout) || !strings.Contains(err.Error(), envSendQueueIdle) ||
			!strings.Contains(err.Error(), "must be longer than") {
			t.Errorf("%s (timeout %q, idle %q): booted or refused for another reason: %v", c.name, c.timeout, c.idle, err)
		}
	}
	for _, c := range []struct {
		name, timeout, idle string
		routing             bool
		want                time.Duration
	}{
		{"a deadline just past the idle wait", "6m", "5m", true, 6 * time.Minute},
		{"the defaults", "", "", true, 30 * time.Minute},
		{"routing off: a long idle wait alone", "", "45m", false, 30 * time.Minute},
	} {
		cfg, err := load(c.routing, c.timeout, c.idle)
		if err != nil || cfg.RouteTimeout != c.want {
			t.Errorf("%s: %v, %v; want %v", c.name, cfg.RouteTimeout, err, c.want)
		}
	}
}

// MUSTER_CHIEF_ASK_TIMEOUT: unset is api.DefaultChiefAskTimeout (30m); a
// duration inside [1m, 24h] is taken as given; anything else refuses to boot,
// naming the variable.
func TestTheChiefAskTimeoutDefaultsParsesAndRefusesNonsense(t *testing.T) {
	load := func(v string) (config, error) {
		return loadConfig(func(n string) string {
			if n == envChiefAskTimeout {
				return v
			}
			return ""
		})
	}
	cfg, err := load("")
	if err != nil || cfg.ChiefAskTimeout != 30*time.Minute {
		t.Fatalf("unset: %v, %v; want 30m", cfg.ChiefAskTimeout, err)
	}
	for v, want := range map[string]time.Duration{"1m": time.Minute, "7m": 7 * time.Minute, " 3h ": 3 * time.Hour, "24h": 24 * time.Hour} {
		if cfg, err := load(v); err != nil || cfg.ChiefAskTimeout != want {
			t.Errorf("%q: %v, %v; want %v", v, cfg.ChiefAskTimeout, err, want)
		}
	}
	for _, bad := range []string{"soon", "30", "0", "-5m", "59s", "24h1s"} {
		if _, err := load(bad); err == nil || !strings.Contains(err.Error(), envChiefAskTimeout) {
			t.Errorf("%q: booted (%v)", bad, err)
		}
	}
}
