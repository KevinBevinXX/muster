package agentprovision

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

// 🔴 TestAClaudeCodeAgentIsMintedATurnCredentialOfItsOwn: a claude-code row
// provisioned before the turn credential existed (hooks token set, no secret) is
// minted one on its next apply — a Start here, the path an existing agent takes —
// and its spec ships THAT as ccd's bearer, not the hooks-sha256 derivation of the
// hooks token its session holds, and no HOOKS_TOKEN at all. A second apply does
// not re-mint (the running pod holds the first). A gateway row is never minted
// one: its bearer stays the derived one its runtime computes.
func TestAClaudeCodeAgentIsMintedATurnCredentialOfItsOwn(t *testing.T) {
	row := fixtureAgent()
	row.Kind, row.CCAccount, row.HooksToken, row.Repo, row.RepoBranch, row.Model = agents.KindClaudeCode, "work", "fixture-hooks-mint", "", "", ""
	store := &recordingStore{agent: row}
	driver := &specCapture{Noop: provision.MustNewNoop()}
	cfg := fixtureSpecConfig()
	cfg.ClaudeCode = &agentspec.ClaudeCodeConfig{Image: "ghcr.io/example-org/claude-code-agent:1",
		Accounts: map[string]string{"work": "sk-ant-oat01-FAKE-mint"}}
	a, err := New(Config{Driver: driver, Store: store, Spec: cfg, KickoffDeliverable: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background(), row.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !strings.Contains(store.transcript(), fmt.Sprintf("SetGatewaySecret(%d,len=64)", row.ID)) {
		t.Fatalf("a claude-code apply did not mint a 64-character turn credential; store calls: %s", store.transcript())
	}
	minted := store.agent.GatewaySecret
	sec := map[string]string{}
	for _, e := range driver.last.Secrets {
		sec[e.Name] = e.Value
	}
	if minted == "" || sec[agentspec.EnvGatewayBearer] != minted {
		t.Errorf("the applied spec's %s is not the minted credential", agentspec.EnvGatewayBearer)
	}
	if minted == agentgateway.HooksSHA256().Bearer(row.HooksToken) || minted == row.HooksToken {
		t.Error("the minted credential is computable from the hooks token")
	}
	if _, ok := sec[agentspec.EnvGatewayToken]; ok {
		t.Errorf("the spec still ships %s", agentspec.EnvGatewayToken)
	}
	if err := a.ReapplyProfiles(context.Background(), row.ID); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(store.transcript(), "SetGatewaySecret("); n != 1 || store.agent.GatewaySecret != minted {
		t.Errorf("a second apply re-minted (%d mint(s)); the running pod holds the first", n)
	}

	gw := fixtureAgent()
	gw.Kind, gw.HooksToken = agents.KindGateway, "fixture-hooks-gw"
	gstore := &recordingStore{agent: gw}
	ga, err := New(Config{Driver: provision.MustNewNoop(), Store: gstore, Spec: fixtureSpecConfig(), KickoffDeliverable: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := ga.Dispatch(context.Background(), gw.ID, true); err != nil {
		t.Fatalf("gateway dispatch: %v", err)
	}
	if gstore.called("SetGatewaySecret") {
		t.Errorf("a gateway agent was minted a turn credential; its runtime derives its own bearer")
	}
}
