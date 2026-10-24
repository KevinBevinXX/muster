package main

import (
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/api"
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

// With session routing on, a send-queue idle wait that is not SHORTER than a
// route's deadline (api.RouteTimeout, 30m) refuses to boot — above it and at
// equality — naming the variable. A shorter wait boots, and with routing off a
// long idle wait alone boots.
func TestTheSendQueueIdleWaitMustBeShorterThanARoutesDeadline(t *testing.T) {
	load := func(routing bool, idle string) (config, error) {
		env := map[string]string{envSendQueueIdle: idle}
		if routing {
			env[envSessionRouting] = "1"
		}
		return loadConfig(func(n string) string { return env[n] })
	}
	if api.RouteTimeout != 30*time.Minute {
		t.Fatalf("a route's deadline is %v, want 30m", api.RouteTimeout)
	}
	for _, idle := range []string{"45m", "30m"} {
		_, err := load(true, idle)
		if err == nil || !strings.Contains(err.Error(), envSendQueueIdle) || !strings.Contains(err.Error(), "must be shorter than") {
			t.Errorf("routing on, idle %q: booted or refused for another reason: %v", idle, err)
		}
	}
	for _, c := range []struct {
		idle    string
		routing bool
	}{{"29m", true}, {"", true}, {"45m", false}} {
		if _, err := load(c.routing, c.idle); err != nil {
			t.Errorf("routing %v, idle %q: %v", c.routing, c.idle, err)
		}
	}
}
