package agentprovision

import (
	"context"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

// A hermes worker is built with no instructions (WorkerInstructions teaches a
// CLI the image does not carry), on its owner's OpenRouter key; a hermes CHIEF is
// refused (its MCP client against muster's tools is unverified).
func TestAHermesWorkerIsBuiltAndAHermesChiefIsRefused(t *testing.T) {
	cfg := fixtureSpecConfig()
	cfg.Hermes = &agentspec.HermesConfig{Image: "ghcr.io/example-org/hermes-agent:hermes-5fc490e2"}
	creds := &ownerCreds{or: map[int64]string{1: envKey}}
	a, err := New(Config{Driver: provision.MustNewNoop(), Store: &recordingStore{}, Spec: cfg, Credentials: creds, KickoffDeliverable: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	w := fixtureAgent()
	w.OwnerID, w.Kind, w.HooksToken = 1, agents.KindHermes, "fixture-hooks-owner"
	spec, err := a.buildSpec(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Files) != 0 || spec.Runtime.Image != cfg.Hermes.Image || !strings.Contains(secretsOf(spec), agentspec.EnvOpenRouterKey+"="+envKey) {
		t.Fatalf("hermes worker: files %+v image %q secrets\n%s", spec.Files, spec.Runtime.Image, secretsOf(spec))
	}
	chief := w
	chief.Name = agents.ChiefNameFor(1)
	if _, err := a.buildSpec(ctx, chief); err == nil || !strings.Contains(err.Error(), "cannot be the chief") {
		t.Fatalf("a hermes chief: %v", err)
	}
}
