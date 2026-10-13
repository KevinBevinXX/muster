package main

import (
	"strings"
	"testing"
	"time"
)

// TestTheStepUpWindowEnvIsBoundedAtBoot: MUSTER_STEPUP_WINDOW is read by
// loadConfig, which REFUSES a value outside one minute to 24 hours — so a
// deployment that typed 0 (meaning "off"?), a negative, a bare number or a
// week fails to start, naming the variable, rather than running with a gate
// nobody chose. Unset is 12 hours. The accepted points are both bounds and a
// middle.
func TestTheStepUpWindowEnvIsBoundedAtBoot(t *testing.T) {
	load := func(v string) (config, error) {
		return loadConfig(func(name string) string {
			if name == envStepUpWindow {
				return v
			}
			return ""
		})
	}
	if envStepUpWindow != "MUSTER_STEPUP_WINDOW" {
		t.Fatalf("the variable is spelled %q", envStepUpWindow)
	}
	for _, v := range []string{"0", "0s", "-12h", "59s", "24h1m", "168h", "12", "12 hours", "off"} {
		c, err := load(v)
		if err == nil {
			t.Errorf("%s=%q loaded as %v; want boot refused", envStepUpWindow, v, c.StepUpWindow)
			continue
		}
		if !strings.Contains(err.Error(), envStepUpWindow) {
			t.Errorf("%s=%q: the refusal does not name the variable: %v", envStepUpWindow, v, err)
		}
	}
	for v, want := range map[string]time.Duration{
		"": 12 * time.Hour, "1m": time.Minute, "5m": 5 * time.Minute, "8h30m": 8*time.Hour + 30*time.Minute, "24h": 24 * time.Hour,
	} {
		c, err := load(v)
		if err != nil || c.StepUpWindow != want {
			t.Errorf("%s=%q loaded as %v, %v; want %v", envStepUpWindow, v, c.StepUpWindow, err, want)
		}
	}
}
