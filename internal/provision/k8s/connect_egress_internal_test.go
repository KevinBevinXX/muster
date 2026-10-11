package k8s

import (
	"context"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

// ccSpecWithConnect is ccSpec on a deployment that offers the connector store:
// MUSTER_AGENT_CONNECT_URL set, muster's connect listener on pod port 18191.
func ccSpecWithConnect(t *testing.T) provision.Spec {
	t.Helper()
	spec, err := agentspec.Build(agents.Agent{
		ID: 7, Name: "quiet-heron", Namespace: "agents", HooksToken: "fixture-hooks-token-k8s",
		Kind: agents.KindClaudeCode, CCAccount: "work",
	}, agentspec.Config{
		ImageRepo: "registry.example.test/unused", APIBaseURL: "http://muster.example.test:8105",
		ConnectURL: "http://muster.example.test:18191", ConnectPort: 18191,
		ClaudeCode: &agentspec.ClaudeCodeConfig{
			Image:    "ghcr.io/example-org/claude-code-agent:abc1234",
			Accounts: map[string]string{"work": fakeCCToken},
		},
	}, agentspec.Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return spec
}

// controllerEgress returns the egress rules whose peer selects the controller's
// namespace (the namespace the ingress rule admits from).
func controllerEgress(np *networkingv1.NetworkPolicy, cfg *NetworkPolicyConfig) []networkingv1.NetworkPolicyEgressRule {
	var out []networkingv1.NetworkPolicyEgressRule
	for _, r := range np.Spec.Egress {
		for _, p := range r.To {
			if p.NamespaceSelector != nil && p.NamespaceSelector.MatchLabels[labelNamespaceName] == cfg.ControllerNamespace {
				out = append(out, r)
			}
		}
	}
	return out
}

// TestAClaudeCodeAgentMayReachOnlyMustersConnectPort: with the connector store
// offered, the policy gains exactly ONE egress rule — to the controller's pods
// (namespace AND labels, one peer), on the connect port and no other. Without
// it, there is no egress to muster at all (the pinned policy in
// TestTheClaudeCodeAgentGetsExactlyThisNetworkPolicy is unchanged).
func TestAClaudeCodeAgentMayReachOnlyMustersConnectPort(t *testing.T) {
	cfg := internalNetworkPolicy()

	d, cs := perInstanceDriver(t)
	if err := d.Create(context.Background(), ccSpec(t)); err != nil {
		t.Fatal(err)
	}
	if got := controllerEgress(getPolicy(t, cs, "muster-agent-quiet-heron", "quiet-heron-network"), cfg); len(got) != 0 {
		t.Fatalf("control: without the connector store the policy already allows egress to muster: %+v", got)
	}

	d, cs = perInstanceDriver(t)
	if err := d.Create(context.Background(), ccSpecWithConnect(t)); err != nil {
		t.Fatal(err)
	}
	np := getPolicy(t, cs, "muster-agent-quiet-heron", "quiet-heron-network")
	rules := controllerEgress(np, cfg)
	if len(rules) != 1 {
		t.Fatalf("controller egress rules = %d, want 1:\n%s", len(rules), specJSON(t, np))
	}
	r := rules[0]
	if len(r.To) != 1 || r.To[0].PodSelector == nil {
		t.Fatalf("the rule's peers = %+v; the namespace and pod selectors must be ONE peer (ANDed)", r.To)
	}
	sel := r.To[0].PodSelector.MatchLabels
	if len(sel) != len(cfg.ControllerPodLabels) {
		t.Fatalf("the peer selects %v, want exactly muster's pods %v", sel, cfg.ControllerPodLabels)
	}
	for k, v := range cfg.ControllerPodLabels {
		if sel[k] != v {
			t.Fatalf("the peer does not select muster's pods: %v", sel)
		}
	}
	if len(r.Ports) != 1 || r.Ports[0].Port.IntVal != 18191 || string(*r.Ports[0].Protocol) != "TCP" {
		t.Fatalf("the rule's ports = %+v, want exactly TCP 18191", r.Ports)
	}
	// DNS and public 443 are still there, and nothing else was added.
	if len(np.Spec.Egress) != 3 {
		t.Fatalf("egress rules = %d, want DNS + public 443 + controller connect port", len(np.Spec.Egress))
	}
}
