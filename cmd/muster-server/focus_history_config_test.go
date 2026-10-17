package main

import (
	"strings"
	"testing"
	"time"
)

// TestFocusHistoryRetentionDefaultsToThirtyDaysAndIsBounded:
// MUSTER_FOCUS_HISTORY_RETENTION is 720h unset, a Go duration in 1h–2160h
// otherwise, and anything else refuses to boot naming the variable.
func TestFocusHistoryRetentionDefaultsToThirtyDaysAndIsBounded(t *testing.T) {
	load := func(v string) (config, error) {
		return loadConfig(func(k string) string {
			if k == envFocusHistoryRetention {
				return v
			}
			return ""
		})
	}
	c, err := load("")
	if err != nil || c.FocusHistoryRetention != 720*time.Hour {
		t.Fatalf("unset: %s (%v), want 720h", c.FocusHistoryRetention, err)
	}
	for v, want := range map[string]time.Duration{"1h": time.Hour, " 48h ": 48 * time.Hour, "2160h": 2160 * time.Hour} {
		if c, err := load(v); err != nil || c.FocusHistoryRetention != want {
			t.Errorf("%q: %s (%v), want %s", v, c.FocusHistoryRetention, err, want)
		}
	}
	for _, bad := range []string{"59m", "2161h", "0", "-1h", "a month"} {
		if _, err := load(bad); err == nil || !strings.Contains(err.Error(), envFocusHistoryRetention) {
			t.Errorf("%q: err=%v, want a boot refusal naming %s", bad, err, envFocusHistoryRetention)
		}
	}
}
