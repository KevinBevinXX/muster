package agentspec

import (
	"testing"

	"github.com/ZacxDev/muster/internal/provision"
)

// TestTheConnectorStoreReachesAClaudeCodeAgentOnlyWhenOffered: with ConnectURL
// set the pod gets MUSTER_CONNECT_URL and a network allowance to the connect
// port; without it, neither.
func TestTheConnectorStoreReachesAClaudeCodeAgentOnlyWhenOffered(t *testing.T) {
	plain := mustBuild(t, ccAgent(), ccConfig(), Options{})
	if _, ok := envValue(plain, EnvConnectURL); ok {
		t.Fatal("MUSTER_CONNECT_URL is set without a connector store")
	}
	if len(plain.Network.ControllerEgressTCPPorts) != 0 {
		t.Fatalf("controller egress without a connector store: %v", plain.Network.ControllerEgressTCPPorts)
	}

	cfg := ccConfig()
	cfg.ConnectURL, cfg.ConnectPort = "http://muster.example.test:18191", 18191
	spec := mustBuild(t, ccAgent(), cfg, Options{})
	if got, _ := envValue(spec, EnvConnectURL); got != "http://muster.example.test:18191" {
		t.Fatalf("MUSTER_CONNECT_URL = %q", got)
	}
	if p := spec.Network.ControllerEgressTCPPorts; len(p) != 1 || p[0] != 18191 {
		t.Fatalf("controller egress = %v, want [18191]", p)
	}
	if p := spec.Network.PublicEgressTCPPorts; len(p) != 1 || p[0] != 443 {
		t.Fatalf("public egress changed: %v", p)
	}
	if provision.Fingerprint(spec) == provision.Fingerprint(plain) {
		t.Fatal("offering the connector store did not move the fingerprint, so a running agent would never reconcile to it")
	}

	cfg.ConnectPort = 0
	if _, err := Build(ccAgent(), cfg, Options{}); err == nil {
		t.Fatal("a connect URL without a connect port was accepted for an isolated kind")
	}
}

func TestTheGatewayKindIsToldTheConnectURLAndMayNotShadowIt(t *testing.T) {
	cfg := fixtureConfig()
	cfg.ConnectURL = "http://muster.example.test:18191"
	spec := mustBuild(t, fixtureAgent(), cfg, Options{})
	if got, _ := envValue(spec, EnvConnectURL); got != cfg.ConnectURL {
		t.Fatalf("the gateway kind is told MUSTER_CONNECT_URL=%q", got)
	}
	_, err := Build(fixtureAgent(), cfg, Options{ExtraEnv: []provision.EnvVar{{Name: EnvConnectURL, Value: "http://elsewhere.example.test"}}})
	if err == nil {
		t.Fatal("ExtraEnv shadowed MUSTER_CONNECT_URL")
	}
}
