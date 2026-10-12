package agentprovision

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
	k8sdriver "github.com/ZacxDev/muster/internal/provision/k8s"
)

// The SEAM between three things that are each tested on their own: agentspec
// declares the claude-code kind isolated, the kubernetes driver writes the policy
// first and refuses without it, and this adapter records what the driver said.
// Nothing in any of those three suites builds the combined state — the adapter's
// tests use the recording driver, which has no NetworkPolicy to be refused.
//
// ⚠ STILL A FAKE CLIENTSET. What is pinned is what muster WRITES and RECORDS.

const seamNamespace = "muster-agent-" + fixtureAgentName

// seamAdapter is an adapter over the REAL kubernetes driver and a fake cluster,
// configured the way a claude-code deployment is.
func seamAdapter(t *testing.T, row agents.Agent) (*Adapter, *recordingStore, *fake.Clientset, provision.Provisioner) {
	t.Helper()
	cs := fake.NewClientset()
	empty := ""
	driver, err := k8sdriver.New(k8sdriver.Config{
		Client: cs, NamespacePerInstance: true, NamespacePrefix: "muster-agent-", WorkspaceStorageClass: &empty,
		NetworkPolicy: &k8sdriver.NetworkPolicyConfig{
			ControllerNamespace: "control-plane-7",
			ControllerPodLabels: map[string]string{"app": "muster-server"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{agent: row}
	a, err := New(Config{Driver: driver, Store: store, Spec: seamSpecConfig(), KickoffDeliverable: true})
	if err != nil {
		t.Fatal(err)
	}
	return a, store, cs, driver
}

func seamSpecConfig() agentspec.Config {
	cfg := fixtureSpecConfig()
	cfg.ClaudeCode = &agentspec.ClaudeCodeConfig{
		Image:    "ghcr.io/example-org/claude-code-agent:1",
		Accounts: map[string]string{"work": "sk-ant-oat01-FAKE-seam-4d2e"},
	}
	return cfg
}

func seamRow() agents.Agent {
	row := fixtureAgent()
	row.Kind, row.CCAccount, row.HooksToken, row.Repo, row.RepoBranch, row.Model = agents.KindClaudeCode, "work", "fixture-hooks-seam", "", "", ""
	return row
}

func refuseNetworkPolicies(cs *fake.Clientset) {
	cs.PrependReactor("create", "networkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"}, "",
			fmt.Errorf("fixture RBAC: no rule for networkpolicies"))
	})
}

func seamCounts(t *testing.T, cs *fake.Clientset) (deployments, policies int) {
	t.Helper()
	ctx := context.Background()
	deps, err := cs.AppsV1().Deployments(seamNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	nps, err := cs.NetworkingV1().NetworkPolicies(seamNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return len(deps.Items), len(nps.Items)
}

// recordedError returns the message of the last `error` status write, or "".
func recordedError(s *recordingStore) string {
	for i := len(s.calls) - 1; i >= 0; i-- {
		if strings.HasPrefix(s.calls[i], fmt.Sprintf("UpdateStatus(%d,%s,", fixtureAgentID, agents.StatusError)) {
			return s.calls[i]
		}
	}
	return ""
}

// TestAClaudeCodeDispatchWritesItsNetworkPolicyAndIsRefusedWithoutOne: a
// claude-code dispatch through the real driver leaves a NetworkPolicy beside its
// Deployment; on a cluster where muster may not write one, the same dispatch
// leaves NEITHER, and the row says why, naming the verb.
func TestAClaudeCodeDispatchWritesItsNetworkPolicyAndIsRefusedWithoutOne(t *testing.T) {
	t.Run("permitted", func(t *testing.T) {
		a, store, cs, _ := seamAdapter(t, seamRow())
		if err := a.Dispatch(context.Background(), fixtureAgentID, true); err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if deps, nps := seamCounts(t, cs); deps != 1 || nps != 1 {
			t.Fatalf("after a claude-code dispatch: %d deployment(s), %d networkpolicy object(s); want 1 and 1", deps, nps)
		}
		if msg := recordedError(store); msg != "" {
			t.Fatalf("a permitted dispatch recorded an error: %s", msg)
		}
	})

	t.Run("refused", func(t *testing.T) {
		a, store, cs, _ := seamAdapter(t, seamRow())
		refuseNetworkPolicies(cs)
		err := a.Dispatch(context.Background(), fixtureAgentID, true)
		if err == nil {
			t.Fatal("the dispatch succeeded although muster may not write the agent's NetworkPolicy")
		}
		if !errors.Is(err, provision.ErrUnsupported) {
			t.Errorf("want the driver's ErrUnsupported through the adapter, got %v", err)
		}
		if deps, nps := seamCounts(t, cs); deps != 0 || nps != 0 {
			t.Fatalf("a refused dispatch left %d deployment(s) and %d policy object(s); the agent must not exist", deps, nps)
		}
		msg := recordedError(store)
		for _, want := range []string{"muster's create of networkpolicies.networking.k8s.io", "an instance that was not running was NOT started", `verbs [\"get\", \"create\", \"update\", \"delete\"]`} {
			if !strings.Contains(msg, want) {
				t.Errorf("the row's recorded error does not say %q:\n  %s", want, msg)
			}
		}
		if strings.Contains(msg, "sk-ant-oat01") || strings.Contains(err.Error(), "sk-ant-oat01") {
			t.Error("the refusal carries the account token")
		}
	})
}

// TestAStoppedClaudeCodeAgentFromBeforeIsolationIsConfinedOrNotStarted is the
// agent that already existed when this shipped: created by a build that wrote no
// policy, then stopped and started on the new one. Start either writes the policy
// and brings it up, or — where muster may not write one — leaves it stopped.
func TestAStoppedClaudeCodeAgentFromBeforeIsolationIsConfinedOrNotStarted(t *testing.T) {
	ctx := context.Background()
	// seedOldAgent provisions the agent the way the previous build did (the same
	// spec, with no network declaration) and stops it.
	seedOldAgent := func(t *testing.T) (*Adapter, *recordingStore, *fake.Clientset) {
		t.Helper()
		row := seamRow()
		row.KickedOff, row.PendingNote = true, ""
		a, store, cs, driver := seamAdapter(t, row)
		old, err := agentspec.Build(row, seamSpecConfig(), agentspec.Options{})
		if err != nil {
			t.Fatal(err)
		}
		old.Network = provision.Network{}
		if err := driver.Create(ctx, old); err != nil {
			t.Fatalf("seed the pre-isolation instance: %v", err)
		}
		if deps, nps := seamCounts(t, cs); deps != 1 || nps != 0 {
			t.Fatalf("premise: the pre-isolation agent has %d deployment(s) and %d policy object(s); want 1 and 0", deps, nps)
		}
		if err := a.Stop(context.Background(), fixtureAgentID); err != nil {
			t.Fatalf("stop: %v", err)
		}
		return a, store, cs
	}
	replicas := func(t *testing.T, cs *fake.Clientset) int32 {
		t.Helper()
		dep, err := cs.AppsV1().Deployments(seamNamespace).Get(ctx, fixtureAgentName, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if dep.Spec.Replicas == nil {
			t.Fatal("the Deployment has no replica count")
		}
		return *dep.Spec.Replicas
	}

	t.Run("permitted: start writes the policy", func(t *testing.T) {
		a, _, cs := seedOldAgent(t)
		if got := replicas(t, cs); got != 0 {
			t.Fatalf("premise: the stopped agent has %d replica(s)", got)
		}
		if err := a.Start(context.Background(), fixtureAgentID); err != nil {
			t.Fatalf("start: %v", err)
		}
		if _, nps := seamCounts(t, cs); nps != 1 {
			t.Fatalf("the started agent has %d policy object(s), want 1", nps)
		}
		if got := replicas(t, cs); got != 1 {
			t.Fatalf("the started agent has %d replica(s), want 1", got)
		}
	})

	t.Run("refused: it stays stopped", func(t *testing.T) {
		a, store, cs := seedOldAgent(t)
		refuseNetworkPolicies(cs)
		if err := a.Start(context.Background(), fixtureAgentID); !errors.Is(err, provision.ErrUnsupported) {
			t.Fatalf("start = %v, want the networkpolicies refusal", err)
		}
		if got := replicas(t, cs); got != 0 {
			t.Fatalf("the agent has %d replica(s) after a refused start; it came back up with no NetworkPolicy", got)
		}
		if _, nps := seamCounts(t, cs); nps != 0 {
			t.Fatalf("%d policy object(s) exist after a refused create", nps)
		}
		if msg := recordedError(store); !strings.Contains(msg, "muster's create of networkpolicies.networking.k8s.io") {
			t.Fatalf("the row's recorded error does not name the refused verb: %q", msg)
		}
	})
}

// TestAGatewayAgentGetsNoNetworkPolicyFromTheSameDeployment: on the very
// deployment that confines its claude-code agents, a gateway-kind DISPATCH writes
// no policy and makes no call against networkpolicies — so a dispatch keeps
// working where that RBAC rule is missing. The refusing reactor is installed (it
// refuses `create`, the one call a policy write starts with), and the action log
// is what shows no OTHER verb was called either.
//
// ⚠ DISPATCH ONLY. Destroying that agent on this deployment DOES read
// networkpolicies — the driver's
// TestDestroyDoesNotReportSuccessOverAPolicyItCouldNotRemove pins that cost.
func TestAGatewayAgentGetsNoNetworkPolicyFromTheSameDeployment(t *testing.T) {
	row := fixtureAgent()
	a, store, cs, _ := seamAdapter(t, row)
	refuseNetworkPolicies(cs)
	if err := a.Dispatch(context.Background(), fixtureAgentID, true); err != nil {
		t.Fatalf("a gateway-kind dispatch failed on a cluster that refuses networkpolicies: %v", err)
	}
	if msg := recordedError(store); msg != "" {
		t.Fatalf("recorded error: %s", msg)
	}
	// Read the action log BEFORE this test's own listing below adds to it.
	for _, act := range cs.Actions() {
		if act.GetResource().Resource == "networkpolicies" {
			t.Errorf("a gateway-kind dispatch made a %s call against networkpolicies", act.GetVerb())
		}
	}
	deps, nps := seamCounts(t, cs)
	if deps != 1 {
		t.Fatalf("instrument check: %d deployment(s) after the gateway dispatch, want 1", deps)
	}
	if nps != 0 {
		t.Fatalf("a gateway-kind agent got %d NetworkPolicy object(s)", nps)
	}
}
