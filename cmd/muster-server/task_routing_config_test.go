package main

import (
	"strings"
	"testing"
)

// Task routing's switches: off unless spelled on, and the dispatch owners a
// list of positive user ids — anything else refuses to boot, naming the
// variable, rather than being read as fewer (or more) owners.
func TestTaskRoutingConfigIsOffByDefaultAndTheOwnersMustParse(t *testing.T) {
	load := func(env map[string]string) (config, error) {
		return loadConfig(func(n string) string { return env[n] })
	}
	cfg, err := load(nil)
	if err != nil || cfg.TaskRouting || len(cfg.ChiefDispatchOwners) != 0 {
		t.Fatalf("unset: %+v %v", cfg.ChiefDispatchOwners, err)
	}
	cfg, err = load(map[string]string{envTaskRouting: "1", envChiefDispatchOwners: " 2, 7 ,"})
	if err != nil || !cfg.TaskRouting || len(cfg.ChiefDispatchOwners) != 2 ||
		!cfg.ChiefDispatchOwners[2] || !cfg.ChiefDispatchOwners[7] {
		t.Fatalf("set: %+v %v", cfg.ChiefDispatchOwners, err)
	}
	for _, bad := range []string{"x", "1,0", "-3", "1;2"} {
		if _, err := load(map[string]string{envChiefDispatchOwners: bad}); err == nil || !strings.Contains(err.Error(), envChiefDispatchOwners) {
			t.Errorf("%q: booted (%v)", bad, err)
		}
	}
}
