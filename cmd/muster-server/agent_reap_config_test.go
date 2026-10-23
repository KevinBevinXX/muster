package main

import (
	"strings"
	"testing"
	"time"
)

// TestAgentReapStoppedAfterIsReadAndBounded: MUSTER_AGENT_REAP_STOPPED_AFTER
// reaches the config: 336h unset, a Go duration of at least 1h when set, 0
// for "0" or "off" (the reaper is off) — and a value under 1h, a negative one
// or one that is not a duration refuses to boot, naming the variable.
func TestAgentReapStoppedAfterIsReadAndBounded(t *testing.T) {
	base := map[string]string{envDatabase: "postgres://x/y", envUIPassword: strings.Repeat("p", 40)}
	load := func(v string, set bool) (config, error) {
		return loadConfig(func(k string) string {
			if k == envAgentReapStoppedAfter && set {
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
		{"", false, 336 * time.Hour},
		{"720h", true, 720 * time.Hour},
		{"90m", true, 90 * time.Minute},
		{"off", true, 0},
		{"OFF", true, 0},
		{"0", true, 0},
	} {
		cfg, err := load(c.v, c.set)
		if err != nil {
			t.Fatalf("%s=%q refused: %v", envAgentReapStoppedAfter, c.v, err)
		}
		if cfg.AgentReapStoppedAfter != c.want {
			t.Errorf("%s=%q (set %v) = %s, want %s", envAgentReapStoppedAfter, c.v, c.set, cfg.AgentReapStoppedAfter, c.want)
		}
	}
	for _, bad := range []string{"14m", "-336h", "14d", "two weeks"} {
		if _, err := load(bad, true); err == nil || !strings.Contains(err.Error(), envAgentReapStoppedAfter) {
			t.Errorf("%s=%q: err = %v, want a refusal naming the variable", envAgentReapStoppedAfter, bad, err)
		}
	}
}
