package main

import (
	"strings"
	"testing"
)

// TestTheHostPreferencesAreReadAndChecked: MUSTER_HOST_ORDER and
// MUSTER_HOST_ICONS reach the config as ui.HostPreferences (names
// lower-cased), unset is none, and a value that is not host=icon — or that
// ui.HostPreferences.Normalized refuses — stops the boot, naming the variable.
func TestTheHostPreferencesAreReadAndChecked(t *testing.T) {
	base := map[string]string{envDatabase: "postgres://x/y", envUIPassword: strings.Repeat("p", 40)}
	load := func(order, icons string) (config, error) {
		return loadConfig(func(k string) string {
			switch k {
			case envHostOrder:
				return order
			case envHostIcons:
				return icons
			}
			return base[k]
		})
	}
	cfg, err := load("", "")
	if err != nil {
		t.Fatalf("unset refused: %v", err)
	}
	if len(cfg.HostPrefs.Order) != 0 || len(cfg.HostPrefs.Icons) != 0 {
		t.Errorf("unset names hosts: %+v", cfg.HostPrefs)
	}
	cfg, err = load(" Tower, laptop ,", "Laptop=laptop, tower=server")
	if err != nil {
		t.Fatalf("a valid setting refused: %v", err)
	}
	if got := strings.Join(cfg.HostPrefs.Order, ","); got != "tower,laptop" {
		t.Errorf("order = %s, want tower,laptop", got)
	}
	if cfg.HostPrefs.Icons["laptop"] != "laptop" || cfg.HostPrefs.Icons["tower"] != "server" || len(cfg.HostPrefs.Icons) != 2 {
		t.Errorf("icons = %v", cfg.HostPrefs.Icons)
	}
	for _, c := range []struct{ order, icons, want string }{
		{"", "tower", `invalid MUSTER_HOST_ICONS: entry "tower" is not host=icon`},
		{"", "tower=", `invalid MUSTER_HOST_ICONS: entry "tower=" is not host=icon`},
		{"", "tower=server,Tower=laptop", `invalid MUSTER_HOST_ICONS: host "Tower" is given two icons`},
		{"", "tower=rocket", `invalid MUSTER_HOST_ORDER / MUSTER_HOST_ICONS: host icons: "rocket" is not an icon`},
		{"cluster", "", `invalid MUSTER_HOST_ORDER / MUSTER_HOST_ICONS: host order: "cluster" is the dispatched agents' host`},
		{"tower,tower", "", `invalid MUSTER_HOST_ORDER / MUSTER_HOST_ICONS: host order: "tower" is listed twice`},
		// A space or another separator where a comma belongs: not a host name.
		{"tower laptop", "", `invalid MUSTER_HOST_ORDER / MUSTER_HOST_ICONS: host order: "tower laptop" is not a host name`},
		{"tower;laptop", "", `invalid MUSTER_HOST_ORDER / MUSTER_HOST_ICONS: host order: "tower;laptop" is not a host name`},
		{"", "my laptop=laptop", `invalid MUSTER_HOST_ORDER / MUSTER_HOST_ICONS: host icons: "my laptop" is not a host name`},
	} {
		if _, err := load(c.order, c.icons); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("order %q icons %q: err = %v, want %q", c.order, c.icons, err, c.want)
		}
	}
}
