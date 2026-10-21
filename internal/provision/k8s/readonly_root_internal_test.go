package k8s

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

// byteIdentityConfig builds the gateway and claude-code specs these pins were
// measured on. Every value is a literal; nothing reads the constant under test.
func byteIdentityConfig() agentspec.Config {
	return agentspec.Config{
		ImageRepo: "ghcr.io/example-org/agent-runtime", ImageTag: "3.11.0", APIBaseURL: "http://muster.example.test:8105",
		OpenRouterAPIKey: "fixture-provider-key-77", Model: "openrouter/vendor-b/model-two",
		ConnectURL: "http://muster-connect.example.test:18191", ConnectPort: 18191,
		ClaudeCode: &agentspec.ClaudeCodeConfig{Image: "ghcr.io/example-org/claude-code-agent:0a1b2c3",
			Accounts: map[string]string{"work": "sk-ant-oat01-FAKE-byte-identity"}},
	}
}

func byteIdentitySpecs(t *testing.T) map[string]provision.Spec {
	t.Helper()
	cfg := byteIdentityConfig()
	gw := agents.Agent{ID: 41, Name: "byte-gateway", HooksToken: "fixture-hooks-gw-1", Repo: "example-org/tide", RepoBranch: "trunk"}
	cc := agents.Agent{ID: 42, Name: "byte-claude", Kind: agents.KindClaudeCode, CCAccount: "work", HooksToken: "fixture-hooks-cc-2",
		Repo: "example-org/tide", RepoBranch: "trunk"}
	out := map[string]provision.Spec{}
	for name, a := range map[string]agents.Agent{"gateway": gw, "claude-code": cc} {
		spec, err := agentspec.Build(a, cfg, agentspec.Options{Instructions: "the instructions"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out[name] = spec
	}
	return out
}

func renderDigest(t *testing.T, spec provision.Spec) (fingerprint, deployment string) {
	t.Helper()
	d, _ := internalDriver(t)
	dep, err := d.renderDeployment(spec, internalNS)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(dep)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return provision.Fingerprint(spec), hex.EncodeToString(sum[:])
}

// byteIdentityPins are the gateway and claude-code fingerprints and rendered
// Deployments, MEASURED ON origin/main (d879a50, before ReadOnlyRoot existed)
// with this file's own fixtures.
var byteIdentityPins = map[string][2]string{
	"gateway":     {"3aa6916b48b9f9342d0ca9b04cfc49aa4488f0058a78730cec139f72fd7acbba", "2d218a005f2d1d1c146a1c33ec381880c3f2dfb1e335a3e65b7a9994f34dfc35"},
	"claude-code": {"a541b4a947a4f0fcd5e50a7990e033152ca28ea5897a5d8b392272bfc7f3c188", "fe83599a1cacfbd67efcedccb368a031295a9eafd0e34cd5957f56a03f8e965e"},
}

// 🔴 THE READ-ONLY ROOT IS RENDERED FOR HERMES ONLY: a gateway and a
// claude-code pod render byte for byte, and fingerprint, as they did before the
// field existed — so adding it rolls no existing pod.
func TestTheReadOnlyRootLeavesOtherKindsByteIdentical(t *testing.T) {
	if os.Getenv("MUSTER_PRINT_BYTE_IDENTITY") != "" {
		for name, spec := range byteIdentitySpecs(t) {
			fp, dep := renderDigest(t, spec)
			t.Logf("%s %s %s", name, fp, dep)
		}
		return
	}
	for name, spec := range byteIdentitySpecs(t) {
		fp, dep := renderDigest(t, spec)
		if want := byteIdentityPins[name]; fp != want[0] || dep != want[1] {
			t.Fatalf("%s: fingerprint %s, Deployment %s; want the pre-ReadOnlyRoot %s, %s", name, fp, dep, want[0], want[1])
		}
	}
}

// The hermes pod renders the read-only root: readOnlyRootFilesystem on its
// container, and a writable emptyDir mounted at /tmp.
func TestTheHermesPodHasAReadOnlyRootAndAWritableTmp(t *testing.T) {
	cfg := byteIdentityConfig()
	cfg.Hermes = &agentspec.HermesConfig{Image: "ghcr.io/example-org/hermes-agent:hermes-5fc490e2"}
	spec, err := agentspec.Build(agents.Agent{ID: 43, Name: "byte-hermes", Kind: agents.KindHermes, HooksToken: "fixture-hooks-h-3",
		Model: "openrouter/vendor-a/model-one"}, cfg, agentspec.Options{})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := internalDriver(t)
	dep, err := d.renderDeployment(spec, internalNS)
	if err != nil {
		t.Fatal(err)
	}
	pod := dep.Spec.Template.Spec
	sc := pod.Containers[0].SecurityContext
	if sc == nil || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Fatalf("hermes container security %+v", sc)
	}
	mounted := false
	for _, m := range pod.Containers[0].VolumeMounts {
		mounted = mounted || (m.MountPath == "/tmp" && m.Name == tmpVolumeName)
	}
	emptyDir := false
	for _, v := range pod.Volumes {
		emptyDir = emptyDir || (v.Name == tmpVolumeName && v.EmptyDir != nil)
	}
	if !mounted || !emptyDir {
		t.Fatalf("no writable /tmp: mounts %+v volumes %+v", pod.Containers[0].VolumeMounts, pod.Volumes)
	}
}
