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
	for _, bad := range []string{"1:ada@example.invalid", "1:example-inbox:Kate@example.invalid"} {
		if _, err := load(bad); err == nil || !strings.Contains(err.Error(), envRouteRelayAllow) {
			t.Errorf("%q: booted (%v)", bad, err)
		}
	}
}
