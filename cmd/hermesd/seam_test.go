package main

import (
	"net"
	"strconv"
	"testing"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
)

// 🔴 THE SEAM BETWEEN agentspec AND hermesd, BEHAVIOURALLY: the environment a
// built hermes spec gives the pod, read by hermesd's own configFromEnv, is the
// configuration the spec means — the repository, the workspace it is checked
// out into (terminal.cwd), the model, the instructions file's path, the port
// muster dials, the connector surface and the bearer muster sends. A name
// spelled differently on either side, or a default that moved on one side
// only, fails here.
func TestHermesdReadsTheVariablesAgentspecWrites(t *testing.T) {
	a := agents.Agent{ID: 77, Name: "seam-heron", Kind: agents.KindHermes, HooksToken: "seam-hooks-token-41c2",
		Repo: "example-org/tide-charts", RepoBranch: "trunk", Model: "openrouter/vendor-a/model-one"}
	cfg := agentspec.Config{
		ImageRepo: "unused", APIBaseURL: "http://muster.example.test:8105", OpenRouterAPIKey: "seam-provider-key",
		ConnectURL: "http://muster-connect.example.test:18191", ConnectPort: 18191,
		Hermes: &agentspec.HermesConfig{Image: "ghcr.io/example-org/hermes-agent:hermes-5fc490e2"},
	}
	spec, err := agentspec.Build(a, cfg, agentspec.Options{Instructions: "x"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range append(spec.Env, spec.Secrets...) {
		t.Setenv(e.Name, e.Value)
	}
	// The image's own environment (images/hermes-agent/flake.nix).
	t.Setenv("HERMES_HOME", "/data/hermes")
	got, err := configFromEnv()
	if err != nil {
		t.Fatalf("hermesd refuses the environment agentspec builds: %v", err)
	}
	_, port, _ := net.SplitHostPort(got.Listen)
	if got.Repo != "example-org/tide-charts" || got.RepoBranch != "trunk" || got.Workspace != spec.Repo.Path ||
		got.Model != "vendor-a/model-one" || got.Instructions != spec.Files[0].Path ||
		port != strconv.Itoa(spec.Ports[0].Port) || got.ConnectURL != cfg.ConnectURL || got.AgentToken != a.HooksToken {
		t.Fatalf("hermesd read %+v from the spec %+v", got, spec)
	}
	bearer, err := resolveBearerFromEnv()
	if err != nil || bearer != agentgateway.HooksSHA256().Bearer(a.HooksToken) {
		t.Fatalf("hermesd accepts %q (%v); muster sends the hooks-sha256 derivation of the row's token", bearer, err)
	}
	// The volume the image mounts is the spec's.
	if spec.Workspace.Path != "/data" || got.StateDir != "/data/hermesd" {
		t.Fatalf("workspace %q, state %q", spec.Workspace.Path, got.StateDir)
	}
}
