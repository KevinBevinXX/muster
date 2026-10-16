package main

import (
	"strings"
	"testing"
)

// TestPluginTaskNamespacesAreReadAndValidated: MUSTER_PLUGIN_TASK_NAMESPACES is
// a trimmed, lower-cased comma list, and a namespace muster already uses — or
// one that is not a plain namespace word — refuses to boot, naming the variable.
func TestPluginTaskNamespacesAreReadAndValidated(t *testing.T) {
	env := map[string]string{envPluginTaskNamespaces: " Tracker , ,ext-ids "}
	c, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.PluginTaskNamespaces, ",") != "tracker,ext-ids" {
		t.Fatalf("PluginTaskNamespaces=%q; want [tracker ext-ids]", c.PluginTaskNamespaces)
	}
	base := memoBase()
	base.PluginTaskNamespaces = []string{"tracker"}
	if err := base.validate(); err != nil {
		t.Fatalf("control: a plain namespace is refused: %v", err)
	}
	for _, bad := range []string{"gate", "auto", "initiative", "project", "superseded-by", "a/b", "a.b", "9x", "a:b"} {
		c := memoBase()
		c.PluginTaskNamespaces = []string{"tracker", bad}
		if err := c.validate(); err == nil || !strings.Contains(err.Error(), envPluginTaskNamespaces) {
			t.Errorf("namespace %q: err=%v; want a boot refusal naming %s", bad, err, envPluginTaskNamespaces)
		}
	}
}
