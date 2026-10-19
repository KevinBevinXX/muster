package agentprovision

import (
	"context"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

func sidecarSpecConfig() agentspec.Config {
	cfg := ccSpecConfig()
	cfg.ChiefGatewaySidecar = true
	cfg.OpenRouterAPIKey = "sk-or-v1-FAKE-sidecar-ENV-key-7d10"
	cfg.RuntimeConfig = agentspec.RuntimeConfig{
		Template: []byte("{}\n"), Install: []byte("set -eu\nexec /example/entrypoint --port \"$MUSTER_GATEWAY_PORT\"\n"),
		DeriveBearer: func(tok string) string { return "derived:" + tok },
	}
	return cfg
}

// TestAClaudeCodeChiefGetsTheGatewaySidecarAndNoOtherAgentDoes: with the
// deployment's switch on, the claude-code CHIEF's built spec carries the
// gateway runtime beside ccd; a claude-code worker's does not; and with the
// switch off the chief's does not either.
func TestAClaudeCodeChiefGetsTheGatewaySidecarAndNoOtherAgentDoes(t *testing.T) {
	for _, c := range []struct {
		name   string
		on     bool
		row    agents.Agent
		expect bool
	}{
		{"chief, switch on", true, ccRow(agents.ChiefName), true},
		{"worker, switch on", true, ccRow("plain-worker"), false},
		{"chief, switch off", false, ccRow(agents.ChiefName), false},
	} {
		cfg := sidecarSpecConfig()
		cfg.ChiefGatewaySidecar = c.on
		spec, err := adapterWith(t, cfg, c.row).buildSpec(context.Background(), c.row)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := spec.Sidecar != nil; got != c.expect {
			t.Errorf("%s: sidecar present=%v, want %v", c.name, got, c.expect)
		}
	}
}

// TestTheChiefSidecarCarriesItsOwnersOpenRouterKey: with per-owner credentials,
// the sidecar of a member's claude-code chief is built with the MEMBER's
// OpenRouter key (a claude-code spec otherwise gets none), and a member with no
// key still gets a chief — its gateway sessions fail at the model call.
func TestTheChiefSidecarCarriesItsOwnersOpenRouterKey(t *testing.T) {
	cfg := sidecarSpecConfig()
	cfg.ClaudeCode.Accounts = map[string]string{"acct-x": "sk-ant-oat01-FAKE-env"}
	creds := &ownerCreds{
		claude: map[int64]map[string]string{41: {"acct-x": "sk-ant-oat01-FAKE-member"}, 42: {"acct-x": "sk-ant-oat01-FAKE-other"}},
		or:     map[int64]string{41: "sk-or-v1-FAKE-member-key-41"},
	}
	a, err := New(Config{Driver: provision.MustNewNoop(), Store: &recordingStore{}, Spec: cfg, Credentials: creds, KickoffDeliverable: true})
	if err != nil {
		t.Fatal(err)
	}
	key := func(spec provision.Spec) string {
		for _, e := range spec.Sidecar.Secrets {
			if e.Name == agentspec.EnvOpenRouterKey {
				return e.Value
			}
		}
		return ""
	}
	row := ccRow(agents.ChiefName)
	row.OwnerID = 41
	spec, err := a.buildSpec(context.Background(), row)
	if err != nil || spec.Sidecar == nil || key(spec) != "sk-or-v1-FAKE-member-key-41" {
		t.Fatalf("member 41's chief: sidecar %v key %q (%v); want the member's key", spec.Sidecar != nil, key(spec), err)
	}
	row.OwnerID = 42
	spec, err = a.buildSpec(context.Background(), row)
	if err != nil || spec.Sidecar == nil || key(spec) != "" {
		t.Fatalf("member 42 (no key): sidecar %v key %q (%v); want a sidecar and no key — never the environment's", spec.Sidecar != nil, key(spec), err)
	}
}

// TestAChiefStartOnAPreSidecarInstallScriptFailsOnTheAgent is the wrong deploy
// order: the switch is on while the runtime bundle's install script still
// predates the sidecar's port. Start refuses to apply the spec, and the row
// says why — rather than a pod whose sidecar fights ccd for its port.
func TestAChiefStartOnAPreSidecarInstallScriptFailsOnTheAgent(t *testing.T) {
	cfg := sidecarSpecConfig()
	cfg.RuntimeConfig.Install = []byte("set -eu\nexec /example/entrypoint --bind lan\n")
	row := ccRow(agents.ChiefName)
	a := adapterWith(t, cfg, row)
	err := a.Start(context.Background(), row.ID)
	if err == nil || !strings.Contains(err.Error(), "MUSTER_GATEWAY_PORT") {
		t.Fatalf("Start: %v; want a refusal naming MUSTER_GATEWAY_PORT", err)
	}
	store := a.store.(*recordingStore)
	var recorded bool
	for _, c := range store.calls {
		if strings.HasPrefix(c, "UpdateStatus(") && strings.Contains(c, ","+agents.StatusError+",") &&
			strings.Contains(c, "build spec") && strings.Contains(c, "MUSTER_GATEWAY_PORT") {
			recorded = true
		}
		if strings.HasPrefix(c, "driver.") {
			t.Errorf("the driver was called after the refusal: %s", c)
		}
	}
	if !recorded {
		t.Errorf("the refusal is not on the agent's row: %v", store.calls)
	}
}
