package k8s

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

const fakeCCToken = "sk-ant-oat01-FAKE-k8s-render-91c2"

// ccSpec is a REAL claude-code spec, built by agentspec from a fake account.
func ccSpec(t *testing.T) provision.Spec {
	t.Helper()
	spec, err := agentspec.Build(agents.Agent{
		ID: 7, Name: "quiet-heron", Namespace: "agents", HooksToken: "fixture-hooks-token-k8s", GatewaySecret: "fixture-turn-credential-k8s",
		Kind: agents.KindClaudeCode, CCAccount: "work",
	}, agentspec.Config{
		ImageRepo: "registry.example.test/unused", APIBaseURL: "http://muster.example.test:8105",
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

// TestTheClaudeCodePodRendersItsProfile pins the rendered pod against literal
// values: the operator's live-checked shape (uid/gid/fsGroup 1000,
// runAsNonRoot, automountServiceAccountToken false, startup+liveness on / port
// 18789, NO readiness, a PVC at /data).
func TestTheClaudeCodePodRendersItsProfile(t *testing.T) {
	d, cs := internalDriver(t)
	spec := ccSpec(t)
	if err := d.Create(context.Background(), spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	dep, err := cs.AppsV1().Deployments(internalNS).Get(context.Background(), "quiet-heron", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod := dep.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Errorf("automountServiceAccountToken = %v, want false", pod.AutomountServiceAccountToken)
	}
	psc := pod.SecurityContext
	if psc == nil || psc.RunAsUser == nil || *psc.RunAsUser != 1000 || psc.RunAsGroup == nil || *psc.RunAsGroup != 1000 ||
		psc.FSGroup == nil || *psc.FSGroup != 1000 || psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot ||
		psc.SeccompProfile == nil || psc.SeccompProfile.Type != "RuntimeDefault" {
		t.Errorf("pod securityContext = %+v", psc)
	}
	c := pod.Containers[0]
	if c.SecurityContext == nil || c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation ||
		c.SecurityContext.Capabilities == nil || len(c.SecurityContext.Capabilities.Drop) != 1 || c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Errorf("container securityContext = %+v", c.SecurityContext)
	}
	if c.Image != "ghcr.io/example-org/claude-code-agent:abc1234" || len(c.Command) != 0 || c.WorkingDir != "" {
		t.Errorf("container image/command/workdir = %q %v %q", c.Image, c.Command, c.WorkingDir)
	}
	if len(c.Ports) != 1 || c.Ports[0].ContainerPort != 18789 || c.Ports[0].Name != "gateway" {
		t.Errorf("ports = %+v", c.Ports)
	}
	for _, p := range []struct {
		name string
		ok   bool
		path string
		port int32
	}{
		{"startup", c.StartupProbe != nil && c.StartupProbe.HTTPGet != nil, pathOf(c.StartupProbe), portOf(c.StartupProbe)},
		{"liveness", c.LivenessProbe != nil && c.LivenessProbe.HTTPGet != nil, pathOf(c.LivenessProbe), portOf(c.LivenessProbe)},
	} {
		if !p.ok || p.path != "/" || p.port != 18789 {
			t.Errorf("%s probe = path %q port %d (present %v), want GET / on 18789", p.name, p.path, p.port, p.ok)
		}
	}
	if c.ReadinessProbe != nil {
		t.Errorf("readiness probe = %+v, want none: health must never gate on auth or a rate limit", c.ReadinessProbe)
	}
	mounted := false
	for _, m := range c.VolumeMounts {
		if m.MountPath == "/data" && m.Name == workspaceVolumeName {
			mounted = true
		}
	}
	claim := ""
	for _, v := range pod.Volumes {
		if v.Name == workspaceVolumeName && v.PersistentVolumeClaim != nil {
			claim = v.PersistentVolumeClaim.ClaimName
		}
	}
	if !mounted || claim != "quiet-heron-workspace" {
		t.Errorf("the workspace volume is not a PVC at /data: mounted=%v claim=%q", mounted, claim)
	}
	pvc, err := cs.CoreV1().PersistentVolumeClaims(internalNS).Get(context.Background(), "quiet-heron-workspace", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("pvc: %v", err)
	}
	if q := pvc.Spec.Resources.Requests.Storage().String(); q != "10Gi" {
		t.Errorf("pvc size = %s", q)
	}
	if dep.Spec.Strategy.Type != "Recreate" {
		t.Errorf("strategy = %s, want Recreate (a ReadWriteOnce volume)", dep.Spec.Strategy.Type)
	}
}

func pathOf(p *corev1.Probe) string {
	if p == nil || p.HTTPGet == nil {
		return ""
	}
	return p.HTTPGet.Path
}

func portOf(p *corev1.Probe) int32 {
	if p == nil || p.HTTPGet == nil {
		return 0
	}
	return p.HTTPGet.Port.IntVal
}

// TestTheClaudeTokenRendersOnlyIntoTheEnvSecret is the render half of the token
// seam ledger (agentspec's TestTheClaudeTokenNameAppearsOnlyInTheClaudeCodeKindsSecret):
// every object the driver creates is serialised and searched; the token value
// may appear in the `-env` Secret and NOWHERE else — not the Deployment (pod
// spec, annotations, env literals), not a ConfigMap, Service, ServiceAccount or
// NetworkPolicy.
func TestTheClaudeTokenRendersOnlyIntoTheEnvSecret(t *testing.T) {
	d, cs := internalDriver(t)
	if err := d.Create(context.Background(), ccSpec(t)); err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx := context.Background()
	where := map[string]bool{}
	scan := func(kind, name string, obj any) {
		// Both spellings: a Secret's Data marshals as base64, so a raw-only search
		// reports a Secret holding the token as clean (measured — the first draft
		// of this test found the token nowhere at all).
		b, _ := json.Marshal(obj)
		if strings.Contains(string(b), fakeCCToken) || strings.Contains(string(b), base64.StdEncoding.EncodeToString([]byte(fakeCCToken))) {
			where[kind+"/"+name] = true
		}
	}
	deps, _ := cs.AppsV1().Deployments(internalNS).List(ctx, metav1.ListOptions{})
	for _, o := range deps.Items {
		scan("deployment", o.Name, o)
	}
	cms, _ := cs.CoreV1().ConfigMaps(internalNS).List(ctx, metav1.ListOptions{})
	for _, o := range cms.Items {
		scan("configmap", o.Name, o)
	}
	svcs, _ := cs.CoreV1().Services(internalNS).List(ctx, metav1.ListOptions{})
	for _, o := range svcs.Items {
		scan("service", o.Name, o)
	}
	sas, _ := cs.CoreV1().ServiceAccounts(internalNS).List(ctx, metav1.ListOptions{})
	for _, o := range sas.Items {
		scan("serviceaccount", o.Name, o)
	}
	nps, _ := cs.NetworkingV1().NetworkPolicies(internalNS).List(ctx, metav1.ListOptions{})
	for _, o := range nps.Items {
		scan("networkpolicy", o.Name, o)
	}
	if len(nps.Items) != 1 {
		t.Fatalf("instrument check: %d networkpolicy object(s), want the agent's one", len(nps.Items))
	}
	secs, _ := cs.CoreV1().Secrets(internalNS).List(ctx, metav1.ListOptions{})
	for _, o := range secs.Items {
		scan("secret", o.Name, o)
		if o.Name == "quiet-heron-env" && string(o.Data["CLAUDE_CODE_OAUTH_TOKEN"]) != fakeCCToken {
			t.Errorf("the env Secret's CLAUDE_CODE_OAUTH_TOKEN is not the account's token")
		}
	}
	if len(deps.Items) != 1 || len(secs.Items) == 0 {
		t.Fatalf("instrument check: %d deployment(s), %d secret(s) — the scan saw nothing to scan", len(deps.Items), len(secs.Items))
	}
	want := map[string]bool{"secret/quiet-heron-env": true}
	if len(where) != len(want) || !where["secret/quiet-heron-env"] {
		t.Fatalf("the token appears in %v, want exactly %v", where, want)
	}
}

// TestAZeroSecurityRendersNoSecurityAtAll: the gateway kind's specs declare no
// Security, and must render exactly what they rendered before the type existed —
// no pod or container securityContext, no automount field.
func TestAZeroSecurityRendersNoSecurityAtAll(t *testing.T) {
	d, _ := internalDriver(t)
	dep, err := d.renderDeployment(internalSpec("plain-otter"), internalNS)
	if err != nil {
		t.Fatal(err)
	}
	pod := dep.Spec.Template.Spec
	if pod.SecurityContext != nil || pod.Containers[0].SecurityContext != nil || pod.AutomountServiceAccountToken != nil {
		t.Fatalf("a zero Security rendered %+v / %+v / %v", pod.SecurityContext, pod.Containers[0].SecurityContext, pod.AutomountServiceAccountToken)
	}
}
