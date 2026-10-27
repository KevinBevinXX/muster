package main

import "testing"

// TestTheChiefIsToldAboutPluginsExactlyWhenTheStoreIsWired: agentspec.Config's
// Plugins flag (which puts the plugin section in the agent's prose) comes from
// config.pluginsEnabled — the SAME predicate buildApp wires ext.Plugins on — so
// the prose cannot name task_create_gated on a deployment where the tool is not
// offered. The flag without a database is the case that discriminates it from
// the raw MUSTER_PLUGINS field.
func TestTheChiefIsToldAboutPluginsExactlyWhenTheStoreIsWired(t *testing.T) {
	for _, c := range []struct {
		name     string
		plugins  bool
		database string
		want     bool
	}{
		{"flag and database", true, "postgres://unused", true},
		{"flag, no database", true, "", false},
		{"database, no flag", false, "postgres://unused", false},
		{"neither", false, "", false},
	} {
		cfg := config{Plugins: c.plugins, Database: c.database}
		if cfg.pluginsEnabled() != c.want {
			t.Errorf("%s: pluginsEnabled() = %v, want %v", c.name, cfg.pluginsEnabled(), c.want)
		}
		got, err := agentSpecConfig(cfg)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got.Plugins != c.want {
			t.Errorf("%s: agentspec.Config.Plugins = %v, want %v", c.name, got.Plugins, c.want)
		}
	}
}
