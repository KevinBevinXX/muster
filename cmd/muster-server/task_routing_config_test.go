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
	if err != nil || cfg.TaskRouting || len(cfg.TaskDispatchOwners) != 0 {
		t.Fatalf("unset: %+v %v", cfg.TaskDispatchOwners, err)
	}
	cfg, err = load(map[string]string{envTaskRouting: "1", envTaskDispatchOwners: " 2, 7 ,"})
	if err != nil || !cfg.TaskRouting || len(cfg.TaskDispatchOwners) != 2 ||
		!cfg.TaskDispatchOwners[2] || !cfg.TaskDispatchOwners[7] {
		t.Fatalf("set: %+v %v", cfg.TaskDispatchOwners, err)
	}
	for _, bad := range []string{"x", "1,0", "-3", "1;2"} {
		if _, err := load(map[string]string{envTaskDispatchOwners: bad}); err == nil || !strings.Contains(err.Error(), envTaskDispatchOwners) {
			t.Errorf("%q: booted (%v)", bad, err)
		}
	}
}
