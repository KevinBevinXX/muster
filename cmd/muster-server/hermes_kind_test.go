package main

import (
	"log"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/api"
)

const hermesTestImage = "ghcr.io/example-org/hermes-agent:hermes-5fc490e2"

func hermesTestConfig() config {
	c := provisionerTestConfig(provisionerNoop)
	c.AgentKinds = []string{agents.KindGateway, agents.KindHermes}
	c.AgentHermesImage = hermesTestImage
	return c
}

func hermesK8sTestConfig() config {
	c := hermesTestConfig()
	c.AgentProvisioner = provisionerK8s
	c.AgentNetpolFromNS = "control-plane-7"
	c.AgentNetpolFromLabels = "app=Muster-Server, tier=control"
	return c
}

func TestTheValidHermesConfigsBoot(t *testing.T) {
	for name, c := range map[string]config{"noop": hermesTestConfig(), "kubernetes": hermesK8sTestConfig()} {
		if err := c.validate(); err != nil {
			t.Fatalf("control: the valid %s hermes config is refused: %v", name, err)
		}
	}
	both := ccK8sTestConfig()
	both.AgentKinds = append(both.AgentKinds, agents.KindHermes)
	both.AgentHermesImage = hermesTestImage
	if err := both.validate(); err != nil {
		t.Fatalf("control: claude-code and hermes together are refused: %v", err)
	}
}

// Every hermes boot refusal, reached from a VALID config by one mutation and
// asserted by its own words.
func TestEveryHermesKindRefusalIsReachedOnItsOwn(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*config)
		want string
	}{
		{"no image", func(c *config) { c.AgentHermesImage = "" }, envAgentHermesImage + " is not set"},
		{"an unpinned image", func(c *config) { c.AgentHermesImage = "ghcr.io/example-org/hermes-agent" }, "must pin a tag or a digest"},
		{"a bad volume size", func(c *config) { c.AgentHermesStorage = "lots" }, "invalid " + envAgentHermesStorage},
		{"no driver", func(c *config) { c.AgentProvisioner = provisionerNone }, "a kind is something a DRIVER provisions"},
		{"the image without the kind", func(c *config) { c.AgentKinds = []string{agents.KindGateway} }, "armed switch"},
		{"the size without the kind", func(c *config) {
			c.AgentKinds, c.AgentHermesImage, c.AgentHermesStorage = []string{agents.KindGateway}, "", "20Gi"
		}, "armed switch"},
	} {
		c := hermesTestConfig()
		tc.edit(&c)
		if err := c.validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	// 🔴 A HERMES AGENT IS CONFINED LIKE A CLAUDE-CODE ONE: on the kubernetes
	// driver the NetworkPolicy selector is required for it too.
	c := hermesK8sTestConfig()
	c.AgentNetpolFromNS = ""
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "are not both set") {
		t.Fatalf("hermes on kubernetes without the selector: %v", err)
	}
}

// The kubernetes driver of a hermes deployment is configured for what the hermes
// spec declares — persistence and network isolation — and the spec config
// carries the kind; a deployment without the kind carries neither.
func TestAHermesDeploymentConfiguresItsDriverAndSpec(t *testing.T) {
	logger := log.New(&strings.Builder{}, "", 0)
	dc := k8sDriverConfig(hermesK8sTestConfig(), logger)
	if dc.NetworkPolicy == nil || dc.NetworkPolicy.ControllerNamespace != "control-plane-7" || dc.WorkspaceStorageClass == nil {
		t.Fatalf("hermes on kubernetes: network policy %+v, storage class %v", dc.NetworkPolicy, dc.WorkspaceStorageClass)
	}
	sc, err := agentSpecConfig(hermesTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	if sc.Hermes == nil || sc.Hermes.Image != hermesTestImage || sc.ClaudeCode != nil {
		t.Fatalf("spec config: hermes %+v claude-code %+v", sc.Hermes, sc.ClaudeCode)
	}
	gw, err := agentSpecConfig(provisionerTestConfig(provisionerNoop))
	if err != nil {
		t.Fatal(err)
	}
	if gw.Hermes != nil {
		t.Fatalf("a gateway-only deployment's spec config enables hermes: %+v", gw.Hermes)
	}
}

// The boot banner names the hermes kind's image on the `agent kinds:` line and
// labels the network line with it.
func TestTheKindsBannerLineNamesTheHermesImage(t *testing.T) {
	logger := log.New(&strings.Builder{}, "", 0)
	c := hermesTestConfig()
	prov, _, err := buildAgentPlaneWith(c, stubStore{}, nil, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	out := renderBanner(t, c, api.Extensions{Provisioner: prov})
	kinds := lineNaming(out, envAgentKinds)
	if !strings.Contains(kinds, "gateway, hermes") || !strings.Contains(kinds, hermesTestImage) || !strings.Contains(kinds, envAgentHermesImage) {
		t.Fatalf("agent kinds line: %q", kinds)
	}
	if net := lineNaming(out, "hermes network:"); !strings.Contains(net, "NOT CONFINED") {
		t.Fatalf("hermes network line: %q", net)
	}
}
