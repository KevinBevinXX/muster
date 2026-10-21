package main

import (
	"strings"
	"testing"
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
