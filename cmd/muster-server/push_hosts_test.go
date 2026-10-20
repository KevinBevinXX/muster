package main

import (
	"strings"
	"testing"
)

// TestPushExtraHostsAreParsedAtBoot: MUSTER_PUSH_EXTRA_HOSTS reaches the
// store's allowlist as exact lower-case names, and an entry that is not a bare
// host name refuses boot, naming the variable.
func TestPushExtraHostsAreParsedAtBoot(t *testing.T) {
	load := func(v string) (config, error) {
		c, err := loadConfig(func(k string) string {
			switch k {
			case envDatabase:
				return "postgres://x/y"
			case envStandalone:
				return "1"
			case envPushExtraHosts:
				return v
			}
			return ""
		})
		if err != nil {
			return c, err
		}
		return c, c.validate()
	}
	c, err := load("Ntfy.Example.org, push.example.net")
	if err != nil {
		t.Fatal(err)
	}
	a := c.pushAllowlist()
	if !a.Allows("https://ntfy.example.org/upX") || a.Allows("https://a.ntfy.example.org/upX") {
		t.Errorf("allowlist from the variable: hosts %v", a.ExtraHosts())
	}
	c, err = load("")
	if err != nil || len(c.pushAllowlist().ExtraHosts()) != 0 {
		t.Fatalf("unset: %v, hosts %v", err, c.pushAllowlist().ExtraHosts())
	}
	for _, bad := range []string{"*.example.org", "https://ntfy.example.org", "ntfy.example.org:443"} {
		if _, err := load(bad); err == nil || !strings.Contains(err.Error(), envPushExtraHosts) {
			t.Errorf("%q: err = %v; want a refusal naming %s", bad, err, envPushExtraHosts)
		}
	}
}
