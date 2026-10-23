package main

import (
	"strings"
	"testing"
	"time"
)

// TestAgentsIdleAfterIsReadAndBounded: MUSTER_AGENTS_IDLE_AFTER reaches the
// config (and from there api.Extensions.AgentsIdleAfter): 14 days unset, a
// number of days or a Go duration when set, 0 for "off" — and a value outside
// the bounds or not a duration refuses to boot, naming the variable.
func TestAgentsIdleAfterIsReadAndBounded(t *testing.T) {
	base := map[string]string{envDatabase: "postgres://x/y", envUIPassword: strings.Repeat("p", 40)}
	load := func(v string, set bool) (config, error) {
		return loadConfig(func(k string) string {
			if k == envAgentsIdleAfter && set {
				return v
			}
			return base[k]
		})
	}
	for _, c := range []struct {
		v    string
		set  bool
		want time.Duration
	}{
		{"", false, 14 * 24 * time.Hour},
		{"30d", true, 30 * 24 * time.Hour},
		{"36h", true, 36 * time.Hour},
		{"off", true, 0},
		{"0", true, 0},
	} {
		cfg, err := load(c.v, c.set)
		if err != nil {
			t.Fatalf("%s=%q refused: %v", envAgentsIdleAfter, c.v, err)
		}
		if cfg.AgentsIdleAfter != c.want {
			t.Errorf("%s=%q (set %v) = %s, want %s", envAgentsIdleAfter, c.v, c.set, cfg.AgentsIdleAfter, c.want)
		}
	}
	for _, bad := range []string{"10m", "400d", "two weeks"} {
		if _, err := load(bad, true); err == nil || !strings.Contains(err.Error(), envAgentsIdleAfter) {
			t.Errorf("%s=%q: err = %v, want a refusal naming the variable", envAgentsIdleAfter, bad, err)
		}
	}
}
