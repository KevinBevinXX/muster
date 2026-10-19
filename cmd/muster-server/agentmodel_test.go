package main

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
)

// ---------------------------------------------------------------------------
// THE MODEL AN OPERATOR PICKS MUST REACH THE CONTAINER.
//
// 🔴 WHY THIS LIVES HERE AND NOT ONLY IN internal/agentspec. The spec-level
// assertions in that package pin the precedence rule, but the thing that was
// MEASURED wrong on a live cluster is an environment variable inside a pod:
// agent 82's row held the model it was created with and its MUSTER_CONFIG held
// the deployment-wide default. Two packages stand between those facts —
// agentspec chooses the slug and writes it into the opaque Spec.Config, and the
// kubernetes driver marshals that map into MUSTER_CONFIG verbatim — and each was
// individually correct about its own half while the value an operator feels was
// wrong. A test scoped to either surface is structurally blind to that; this one
// runs this binary's OWN config mapping, this binary's OWN driver construction,
// and reads the slug back out of the rendered Deployment.
//
// ⚠ WHAT IT STILL CANNOT SEE, NAMED HONESTLY: that the agent runtime inside the
// container READS MUSTER_CONFIG and runs on that model. The clientset is a fake,
// nothing is listening and no kubelet runs. The gateway's boot banner reporting
// the slug is the live half, and it needs a cluster.
// ---------------------------------------------------------------------------

// agentModelTestAgent is an agent row that NAMES ITS OWN MODEL, which is the
// state the defect was measured in.
//
// 🔴 ITS SLUG IS NEITHER THE DEPLOYMENT DEFAULT BELOW NOR A NEIGHBOUR OF IT. A
// fixture whose row and default could coincide cannot see a build that reads the
// wrong one of the two, which is the entire bug.
func agentModelTestAgent() agents.Agent {
	return agents.Agent{
		ID:         82,
		Name:       "bright-finch",
		HooksToken: "fixture-per-agent-token-9f31c7",
		Model:      "provider/model-a",
	}
}

const agentModelTestDeploymentDefault = "provider/model-b"

// musterConfigOf pulls MUSTER_CONFIG out of the Deployment this binary rendered.
func musterConfigOf(t *testing.T, cfg config, a agents.Agent) (string, bool) {
	t.Helper()
	d, cs := reachabilityDriver(t, cfg)
	ctx := context.Background()

	spec, err := agentspec.Build(a, mustAgentSpecConfig(t, cfg), agentspec.Options{})
	if err != nil {
		t.Fatalf("agentspec.Build over this binary's own spec config: %v", err)
	}
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ns := agents.ResolveNamespacePrefix(cfg.AgentNamespacePrefix) + a.Name
	dep, err := cs.AppsV1().Deployments(ns).Get(ctx, a.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get the Deployment this binary's own spec created in %q: %v", ns, err)
	}
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "MUSTER_CONFIG" {
			return e.Value, true
		}
	}
	return "", false
}

// TestThePerAgentModelReachesThePodsMusterConfig is the end-to-end regression
// guard for the measured defect.
//
// 🔴 RED AT origin/main, WITH THE EXACT SYMPTOM MEASURED ON THE CLUSTER: there
// MUSTER_CONFIG reads {"model":{"primary":"provider/model-b"}} — the
// deployment-wide default — for an agent row whose model column says
// provider/model-a.
//
// 🔴 CLOSING CONDITION A REVIEWER CAN RUN WITHOUT A CLUSTER:
//
//	go test ./cmd/muster-server/ -run TestThePerAgentModelReachesThePodsMusterConfig -v
func TestThePerAgentModelReachesThePodsMusterConfig(t *testing.T) {
	a := agentModelTestAgent()
	cfg := provisionerTestConfig(provisionerK8s)
	cfg.AgentModel = agentModelTestDeploymentDefault

	if a.Model == "" || cfg.AgentModel == "" || a.Model == cfg.AgentModel {
		t.Fatalf("the fixtures cannot discriminate: row %q, deployment default %q", a.Model, cfg.AgentModel)
	}

	raw, ok := musterConfigOf(t, cfg, a)
	if !ok {
		t.Fatalf("the Deployment carries NO MUSTER_CONFIG, so the container is told no model at all")
	}

	// 🔴 THE WHOLE NORMALISED STRING, NOT A SUBSTRING. A `contains` check on the
	// wanted slug passes just as happily when BOTH slugs are present, and the
	// operator-visible value is these exact bytes.
	want := `{"model":{"primary":"` + a.Model + `"},"workspace":"/data/workspace"}`
	if raw != want {
		t.Errorf("MUSTER_CONFIG = %s\n            want %s\n"+
			"This is the variable measured wrong on the cluster: the agent row named %q and the pod "+
			"was told the deployment-wide default %q, with nothing reporting the substitution.",
			raw, want, a.Model, cfg.AgentModel)
	}

	// And decoded, so the assertion above cannot pass on key ORDER alone while
	// naming the wrong slug.
	var got struct {
		Model struct {
			Primary   string   `json:"primary"`
			Fallbacks []string `json:"fallbacks"`
		} `json:"model"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("MUSTER_CONFIG is not the JSON the runtime parses: %v (%s)", err, raw)
	}
	if got.Model.Primary != a.Model {
		t.Errorf("MUSTER_CONFIG model.primary = %q, want the agent row's own %q", got.Model.Primary, a.Model)
	}
	if got.Model.Primary == cfg.AgentModel {
		t.Errorf("MUSTER_CONFIG model.primary = %q, which is the deployment-wide default", got.Model.Primary)
	}
}

// TestAnAgentWithNoModelOfItsOwnIsToldTheDeploymentDefault is the control that
// stops the guard above from being satisfied by "always use the row".
//
// ⚠ IT IS A CONTROL, NOT A SECOND REGRESSION TEST: this case was already correct
// at origin/main and is expected to be green there. It exists so that a build
// which ignored cfg.AgentModel entirely — the mirror-image defect — fails
// something.
func TestAnAgentWithNoModelOfItsOwnIsToldTheDeploymentDefault(t *testing.T) {
	a := agentModelTestAgent()
	a.Model = ""
	cfg := provisionerTestConfig(provisionerK8s)
	cfg.AgentModel = agentModelTestDeploymentDefault

	raw, ok := musterConfigOf(t, cfg, a)
	if !ok {
		t.Fatalf("the Deployment carries NO MUSTER_CONFIG, so an agent that chose no model is told " +
			"nothing rather than the installation's default")
	}
	want := `{"model":{"primary":"` + cfg.AgentModel + `"},"workspace":"/data/workspace"}`
	if raw != want {
		t.Errorf("MUSTER_CONFIG = %s\n            want %s — an agent that named no model must "+
			"inherit the installation's default", raw, want)
	}
}

// TestNoModelAnywhereTellsTheContainerNothing pins the third tier of the
// contract at the observable: with neither the row nor the deployment naming a
// model, MUSTER_CONFIG carries no model key (only the workspace) and the runtime decides.
//
// 🔴 IT IS THE CONTRACT THIS FIX WAS NOT ALLOWED TO CHANGE, which is why it is
// asserted at the env-var level rather than trusted. agents.ResolveModel ends its
// own precedence chain in a hardcoded built-in slug; calling it unconditionally
// would have started stamping that slug into every deployment that sets no model,
// turning "the runtime decides" into "muster decides" with no variable changed.
// agentspec.resolvePrimaryModel short-circuits the both-empty case for exactly
// this reason and this is the test that says so.
func TestNoModelAnywhereTellsTheContainerNothing(t *testing.T) {
	a := agentModelTestAgent()
	a.Model = ""
	cfg := provisionerTestConfig(provisionerK8s)
	if cfg.AgentModel != "" {
		t.Fatalf("provisionerTestConfig names a model (%q); this test needs both levels unset", cfg.AgentModel)
	}

	// MUSTER_CONFIG carries the workspace (agentspec.ConfigKeyWorkspace) and NO
	// model key.
	if raw, _ := musterConfigOf(t, cfg, a); raw != `{"workspace":"/data/workspace"}` {
		t.Errorf("MUSTER_CONFIG = %s, want only the workspace and no model. An empty %s means the runtime "+
			"decides; rendering a slug nobody configured makes muster the decider instead.",
			raw, envAgentModel)
	}
}
