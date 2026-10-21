package main

import (
	"strings"
	"testing"
)

// TestAnMdvPrefixedSharedSecretIsRefusedAtBoot: every door but the device tier
// refuses an `mdv_` credential by shape, so a shared secret in the device
// credential's namespace would be refused on every route. Boot refuses it,
// naming the variable and the prefix.
func TestAnMdvPrefixedSharedSecretIsRefusedAtBoot(t *testing.T) {
	ok := validGoTrueConfig()
	ok.ServiceToken = strings.Repeat("s", 40)
	if err := ok.validate(); err != nil {
		t.Fatalf("CONTROL: the unprefixed config is refused: %v", err)
	}
	for name, mutate := range map[string]func(*config){
		"MUSTER_HOOK_TOKEN":    func(c *config) { c.HookToken = "mdv_" + strings.Repeat("h", 43) },
		"MUSTER_SERVICE_TOKEN": func(c *config) { c.ServiceToken = "mdv_" + strings.Repeat("s", 43) },
	} {
		c := ok
		mutate(&c)
		err := c.validate()
		if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "mdv_") {
			t.Errorf("an mdv_-prefixed %s: validate() = %v; want a refusal naming it", name, err)
		}
	}
}
