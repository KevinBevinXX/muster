package k8s_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/provision/k8s"
	"github.com/ZacxDev/muster/internal/provision/provisiontest"
)

func strptr(s string) *string { return &s }

// sharedNamespace is the namespace the shared-namespace mode puts every
// instance in. It is NOT prefixed with "muster-", so a test that accidentally
// derives a namespace the per-instance way cannot pass by coincidence.
const sharedNamespace = "agents"

// nsMode is one of the two namespace layouts Config expresses, as a value the
// tests iterate over.
//
// 🔴 THE HARNESS IS PARAMETERISED OVER BOTH MODES BECAUSE A SUITE THAT PINS A
// CONFIGURATION DIMENSION IS STRUCTURALLY BLIND TO THAT DIMENSION'S BUGS. This
// harness used to hardcode NamespacePerInstance: true, so every case ran under
// the layout where Destroy is a namespace deletion. Under the OTHER layout the
// namespace is never deleted, so anything the driver leaves behind survives —
// and nothing in this package could observe that. The cases that pin ownership
// and teardown below are only REACHABLE in shared-namespace mode.
type nsMode struct {
	// name is the subtest name.
	name string
	// config applies the layout to a driver configuration.
	config func(*k8s.Config)
	// ns is the namespace an instance of the given name lands in.
	ns func(instance string) string
	// perInstance is true for the layout that owns a namespace per instance.
	// A case branches on this only where the two layouts genuinely differ —
	// namespace creation and deletion — never to skip an assertion.
	perInstance bool
}

var nsModes = []nsMode{
	{
		name:        "namespace-per-instance",
		config:      func(c *k8s.Config) { c.NamespacePerInstance = true },
		ns:          func(instance string) string { return "muster-" + instance },
		perInstance: true,
	},
	{
		name: "shared-namespace",
		config: func(c *k8s.Config) {
			c.NamespacePerInstance = false
			c.Namespace = sharedNamespace
		},
		ns: func(string) string { return sharedNamespace },
	},
}

// eachMode runs fn once per namespace layout.
func eachMode(t *testing.T, fn func(*testing.T, nsMode)) {
	t.Helper()
	for _, m := range nsModes {
		t.Run(m.name, func(t *testing.T) { fn(t, m) })
	}
}

// newDriver builds the driver over a fake clientset in the given layout, with
// the settings the contract suite's "New" hook uses.
//
// ⚠ THERE IS DELIBERATELY NO MODE-LESS CONSTRUCTOR. Every call site names the
// layout it is testing, so a new case cannot silently inherit one.
func newDriver(t *testing.T, m nsMode, mutate func(*k8s.Config)) (*k8s.Driver, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset()
	cfg := k8s.Config{
		Client:                cs,
		WorkspaceStorageClass: strptr(""),
	}
	m.config(&cfg)
	if mutate != nil {
		mutate(&cfg)
	}
	d, err := k8s.New(cfg)
	if err != nil {
		t.Fatalf("k8s.New: %v", err)
	}
	return d, cs
}

// TestKubernetesSatisfiesTheContract is the point of the whole exercise: the
// same suite the in-memory driver passes, against a driver that renders real
// Kubernetes objects.
//
// 🔴 IT RUNS AGAINST A FAKE CLIENTSET AND NEEDS NO CLUSTER. What that
// structurally CANNOT see is stated in the package's own test for it —
// scheduling, image pulls, volume attachment, admission and the exec stream are
// all outside a fake's reach.
func TestKubernetesSatisfiesTheContract(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		provisiontest.RunContract(t, provisiontest.Harness{
			Name: "kubernetes-" + m.name,
			New: func(t *testing.T) provision.Provisioner {
				d, _ := newDriver(t, m, nil)
				return d
			},
			Blind: func(t *testing.T) provision.Provisioner {
				d, _ := newDriver(t, m, func(c *k8s.Config) { c.Client = brokenClient() })
				return d
			},
			Restricted: func(t *testing.T) provision.Provisioner {
				// The driver's genuinely minimal configuration: no storage class it
				// may use, no exec transport. Persistence is the capability it then
				// refuses.
				d, _ := newDriver(t, m, func(c *k8s.Config) {
					c.WorkspaceStorageClass = nil
				})
				return d
			},
		})
	})
}

// brokenClient is a clientset whose every call fails, standing in for an
// unreachable apiserver.
func brokenClient() kubernetes.Interface {
	cs := fake.NewClientset()
	cs.PrependReactor("*", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("dial tcp: connection refused")
	})
	return cs
}

// --------------------------------------------------------------------------
// Capabilities follow configuration
// --------------------------------------------------------------------------

// TestCapabilitiesFollowConfiguration is what makes Config's pointer fields
// worth their awkwardness: each one CHANGES a declared capability, and the
// capability is what CheckSpec branches on.
func TestCapabilitiesFollowConfiguration(t *testing.T) {
	perInstance := nsModes[0]
	full, _ := newDriver(t, perInstance, func(c *k8s.Config) {
		c.RESTConfig = &rest.Config{Host: "https://cluster.example.test"}
	})
	caps := full.Capabilities()
	if !caps.Persistence {
		t.Error("a configured storage class must give Persistence")
	}
	if !caps.Exec {
		t.Error("a RESTConfig must give Exec")
	}
	if caps.Isolation != provision.IsolationNamespace {
		t.Errorf("namespace-per-instance is IsolationNamespace, got %v", caps.Isolation)
	}

	// 🔴 nil AND "" ARE DIFFERENT ANSWERS. This is the whole reason the field
	// is a pointer, and the only place in the package where the distinction is
	// observable.
	none, _ := newDriver(t, perInstance, func(c *k8s.Config) { c.WorkspaceStorageClass = nil })
	if none.Capabilities().Persistence {
		t.Error("a nil storage class means no persistence; it must not be read as the default class")
	}
	defaultSC, _ := newDriver(t, perInstance, func(c *k8s.Config) { c.WorkspaceStorageClass = strptr("") })
	if !defaultSC.Capabilities().Persistence {
		t.Error("an EMPTY storage class means the cluster default, which IS persistence")
	}

	shared, _ := newDriver(t, nsModes[1], nil)
	if shared.Capabilities().Isolation != provision.IsolationContainer {
		t.Error("a shared namespace is container isolation, not namespace isolation")
	}
}

func TestNewValidatesConfiguration(t *testing.T) {
	if _, err := k8s.New(k8s.Config{}); err == nil {
		t.Error("a nil Client must be refused at construction")
	}
	if _, err := k8s.New(k8s.Config{Client: fake.NewClientset()}); err == nil {
		t.Error("a shared-namespace driver with no namespace must be refused: guessing 'default' would " +
			"put agents somewhere nobody chose")
	}
	if _, err := k8s.New(k8s.Config{Client: fake.NewClientset(), NamespacePerInstance: true, EndpointTemplate: "{{.Name"}); err == nil {
		t.Error("an unparseable endpoint template must fail at construction, not per request")
	}
}

// TestRefusesACommandOnlyRuntime pins the one backend-specific requirement.
// Runtime is a union so a process driver is expressible; this driver is the
// half that needs an image.
func TestRefusesACommandOnlyRuntime(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		d, _ := newDriver(t, m, nil)
		spec := provisiontest.MinimalSpec("commandonly")
		spec.Runtime = provision.Runtime{Command: []string{"/usr/bin/agent"}}
		err := d.Create(context.Background(), spec)
		if !errors.Is(err, provision.ErrUnsupported) {
			t.Fatalf("want ErrUnsupported, got %v", err)
		}
		if !strings.Contains(err.Error(), "Runtime.Image") {
			t.Fatalf("the refusal must name the field, got %q", err)
		}
	})
}

// --------------------------------------------------------------------------
// What gets rendered
// --------------------------------------------------------------------------

// richSpec exercises every field the renderer touches, with pairwise distinct
// values so an assertion cannot pass by two fields happening to agree.
func richSpec(name string) provision.Spec {
	s := provisiontest.MinimalSpec(name)
	s.Ref.ID = 4217
	s.Runtime.Args = []string{"--serve"}
	s.Runtime.WorkingDir = "/data/workspace"
	s.Env = []provision.EnvVar{{Name: "MUSTER_ROLE", Value: "worker"}}
	s.Secrets = []provision.EnvVar{{Name: "MUSTER_CALLBACK_TOKEN", Value: "token-value-19731"}}
	s.Files = []provision.File{
		{Path: "/etc/muster/agent.json", Content: []byte(`{"tools":["tasks"]}`)},
		{Path: "/root/.config/creds", Content: []byte("credential-content-8421"), Secret: true},
	}
	s.Init = []string{"mkdir -p /data/workspace/cache"}
	s.Resources = provision.Resources{CPURequest: "250m", CPULimit: "2", MemoryRequest: "512Mi", MemoryLimit: "3Gi"}
	s.Workspace = provision.Workspace{Path: "/data/workspace", Size: "7Gi", Persist: true}
	s.Repo = provision.Repo{URL: "https://git.example.test/org/repo.git", Branch: "trunk", Path: "/data/workspace/repo"}
	s.Config = map[string]any{"model": "some-model"}
	s.Ports = []provision.Port{{Name: provision.DefaultPortName, Port: 8421}, {Name: "metrics", Port: 9102}}
	s.Labels = map[string]string{"team": "ops"}
	return s
}

func TestRendersTheMinimalObjectSet(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) { testRendersTheMinimalObjectSet(t, m) })
}

func testRendersTheMinimalObjectSet(t *testing.T, m nsMode) {
	d, cs := newDriver(t, m, nil)
	ctx := context.Background()
	spec := richSpec("rich")
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ns := m.ns("rich")

	nsObj, err := cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	switch {
	case m.perInstance:
		if err != nil {
			t.Fatalf("namespace: %v", err)
		}
		if nsObj.Labels["app.kubernetes.io/managed-by"] != "muster" {
			t.Errorf("the namespace this driver created carries labels %v; without managed-by nothing "+
				"can tell it apart from a namespace muster must not delete", nsObj.Labels)
		}
	default:
		// 🔴 THE SHARED-NAMESPACE HALF. The driver must create NO namespace at
		// all here: the namespace is the operator's, and Destroy must never be
		// in a position to delete it.
		if err == nil {
			t.Errorf("a shared-namespace driver created namespace %q; it does not own that namespace", ns)
		}
		list, lerr := cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
		if lerr != nil {
			t.Fatalf("list namespaces: %v", lerr)
		}
		if len(list.Items) != 0 {
			t.Errorf("a shared-namespace driver created %d namespaces, want 0: %+v", len(list.Items), list.Items)
		}
	}
	if _, err := cs.CoreV1().ServiceAccounts(ns).Get(ctx, "rich", metav1.GetOptions{}); err != nil {
		t.Fatalf("serviceaccount: %v", err)
	}
	cm, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, "rich-files", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("configmap: %v", err)
	}
	if len(cm.BinaryData) != 1 {
		t.Errorf("configmap holds %d keys, want 1 (the non-secret file only)", len(cm.BinaryData))
	}
	// 🔴 TWO SECRETS, NOT ONE, AND THIS ASSERTION IS THE SEPARATION. It used to
	// read `len(sec.Data) == 2 // one env var, one secret file` — which PINNED
	// the defect: the env Secret is consumed with envFrom, so a secret file
	// sharing it is exported into the process environment.
	envSec, err := cs.CoreV1().Secrets(ns).Get(ctx, "rich-env", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("env secret: %v", err)
	}
	if len(envSec.Data) != 1 {
		t.Errorf("the envFrom'd secret holds %d keys (%v), want exactly the 1 declared env var", len(envSec.Data), keysOf(envSec.Data))
	}
	if _, leaked := envSec.Data[fileKeyFor("/root/.config/creds")]; leaked {
		t.Error("a secret FILE's key is in the secret that is envFrom'd; its content becomes an " +
			"environment variable, readable from /proc/self/environ and inherited by every child")
	}
	fileSec, err := cs.CoreV1().Secrets(ns).Get(ctx, "rich-secret-files", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("secret-file secret: %v", err)
	}
	if len(fileSec.Data) != 1 {
		t.Errorf("the mounted secret holds %d keys (%v), want exactly the 1 secret file", len(fileSec.Data), keysOf(fileSec.Data))
	}
	if string(fileSec.Data[fileKeyFor("/root/.config/creds")]) != "credential-content-8421" {
		t.Errorf("the secret file's content is not in the mounted secret: %v", keysOf(fileSec.Data))
	}
	pvc, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, "rich-workspace", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("pvc: %v", err)
	}
	if got := pvc.Spec.Resources.Requests.Storage().String(); got != "7Gi" {
		t.Errorf("pvc size %q, want 7Gi", got)
	}
	svc, err := cs.CoreV1().Services(ns).Get(ctx, "rich", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	if len(svc.Spec.Ports) != 2 {
		t.Errorf("service has %d ports, want 2", len(svc.Spec.Ports))
	}

	dep, err := cs.AppsV1().Deployments(ns).Get(ctx, "rich", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("deployment: %v", err)
	}
	pod := dep.Spec.Template.Spec
	if len(pod.Containers) != 1 {
		t.Errorf("deployment has %d containers, want exactly 1 — this driver runs no sidecars", len(pod.Containers))
	}
	if len(pod.InitContainers) != 1 {
		t.Fatalf("deployment has %d init containers, want 1 (Spec.Init is non-empty)", len(pod.InitContainers))
	}
	init := pod.InitContainers[0]
	// 🔴 `sh -eu`, NOT a wrapper that swallows failures. The whole reason
	// Spec.Files exists is so that Init holds only required, imperative steps —
	// and a required step that fails must stop the pod.
	if len(init.Command) < 2 || init.Command[0] != "sh" || init.Command[1] != "-eu" {
		t.Errorf("init container command is %v, want it to start with [sh -eu]", init.Command)
	}
	c := pod.Containers[0]
	if c.Resources.Limits.Memory().String() != "3Gi" || c.Resources.Requests.Cpu().String() != "250m" {
		t.Errorf("resources not rendered: %+v", c.Resources)
	}
	if len(c.EnvFrom) != 1 || c.EnvFrom[0].SecretRef == nil || c.EnvFrom[0].SecretRef.Name != "rich-env" {
		t.Errorf("secrets must arrive by envFrom, got %+v", c.EnvFrom)
	}

	envByName := map[string]string{}
	for _, e := range c.Env {
		envByName[e.Name] = e.Value
	}
	for k, want := range map[string]string{
		"MUSTER_ROLE":        "worker",
		"MUSTER_REPO_URL":    "https://git.example.test/org/repo.git",
		"MUSTER_REPO_BRANCH": "trunk",
		"MUSTER_REPO_PATH":   "/data/workspace/repo",
		"MUSTER_CONFIG":      `{"model":"some-model"}`,
	} {
		if envByName[k] != want {
			t.Errorf("env %s = %q, want %q", k, envByName[k], want)
		}
	}

	// The operator's own label survives; the driver's selector labels win.
	if dep.Labels["team"] != "ops" {
		t.Error("the operator's label was dropped")
	}
	if dep.Labels["app.kubernetes.io/instance"] != "rich" {
		t.Error("the driver's instance label is missing")
	}
}

// TestFilesArePlacedNativelyAtTheirPaths is the replacement for the base64
// shell coupling: each file is a subPath mount at its exact absolute path, with
// its own mode.
func TestFilesArePlacedNativelyAtTheirPaths(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) { testFilesArePlacedNativelyAtTheirPaths(t, m) })
}

func testFilesArePlacedNativelyAtTheirPaths(t *testing.T, m nsMode) {
	d, cs := newDriver(t, m, nil)
	ctx := context.Background()
	spec := provisiontest.MinimalSpec("files")
	spec.Files = []provision.File{
		{Path: "/etc/muster/agent.json", Content: []byte(`{"a":1}`)},
		{Path: "/usr/local/bin/hook", Content: []byte("#!/bin/sh\n"), Mode: 0o755},
		{Path: "/root/.config/creds", Content: []byte("secret-bytes"), Secret: true},
	}
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	dep, err := cs.AppsV1().Deployments(m.ns("files")).Get(ctx, "files", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	c := dep.Spec.Template.Spec.Containers[0]

	mountByPath := map[string]corev1.VolumeMount{}
	for _, m := range c.VolumeMounts {
		mountByPath[m.MountPath] = m
	}
	for _, want := range []string{"/etc/muster/agent.json", "/usr/local/bin/hook", "/root/.config/creds"} {
		m, ok := mountByPath[want]
		if !ok {
			t.Fatalf("no volumeMount at %q; the file is not placed where the spec says", want)
		}
		// 🔴 SUBPATH IS WHAT PLACES A SINGLE FILE AT AN ABSOLUTE PATH. Without
		// it the mount turns the file's DIRECTORY into a volume and everything
		// else in that directory disappears — /usr/local/bin being the obvious
		// disaster.
		if m.SubPath == "" {
			t.Errorf("mount at %q has no subPath; it would replace the whole directory", want)
		}
	}
	// Confidential and non-confidential files come from different volumes.
	if mountByPath["/root/.config/creds"].Name == mountByPath["/etc/muster/agent.json"].Name {
		t.Error("a secret file and a plain file share a volume; one of them is in the wrong store")
	}

	volByName := map[string]corev1.Volume{}
	for _, v := range dep.Spec.Template.Spec.Volumes {
		volByName[v.Name] = v
	}
	plain := volByName[mountByPath["/usr/local/bin/hook"].Name]
	if plain.ConfigMap == nil {
		t.Fatal("the plain-file volume is not a ConfigMap")
	}
	var found bool
	for _, item := range plain.ConfigMap.Items {
		if item.Key == mountByPath["/usr/local/bin/hook"].SubPath {
			found = true
			if item.Mode == nil || *item.Mode != 0o755 {
				t.Errorf("the executable file's mode is %v, want 0755", item.Mode)
			}
		}
	}
	if !found {
		t.Error("the executable file has no item entry, so it cannot carry a mode")
	}

	// The default modes differ between a plain and a secret file, and that
	// difference must survive the render.
	secretVol := volByName[mountByPath["/root/.config/creds"].Name]
	if secretVol.Secret == nil {
		t.Fatal("the secret-file volume is not a Secret")
	}
	for _, item := range secretVol.Secret.Items {
		if item.Mode == nil || *item.Mode != 0o600 {
			t.Errorf("a secret file defaulted to mode %v, want 0600", item.Mode)
		}
	}
}

// TestFileKeysDoNotDependOnOrdering — a key derived from a file's POSITION
// would make reordering Spec.Files rewrite every key and roll the pod for no
// reason.
func TestFileKeysDoNotDependOnOrdering(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) { testFileKeysDoNotDependOnOrdering(t, m) })
}

func testFileKeysDoNotDependOnOrdering(t *testing.T, m nsMode) {
	ctx := context.Background()
	a := provisiontest.MinimalSpec("ordered")
	a.Files = []provision.File{
		{Path: "/etc/muster/one.json", Content: []byte("1")},
		{Path: "/etc/muster/two.json", Content: []byte("2")},
	}
	b := a
	b.Files = []provision.File{a.Files[1], a.Files[0]}

	keysOf := func(spec provision.Spec) []string {
		d, cs := newDriver(t, m, nil)
		if err := d.Create(ctx, spec); err != nil {
			t.Fatalf("Create: %v", err)
		}
		cm, err := cs.CoreV1().ConfigMaps(m.ns("ordered")).Get(ctx, "ordered-files", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get configmap: %v", err)
		}
		out := make([]string, 0, len(cm.BinaryData))
		for k := range cm.BinaryData {
			out = append(out, k)
		}
		sortStrings(out)
		return out
	}
	ka, kb := keysOf(a), keysOf(b)
	if strings.Join(ka, ",") != strings.Join(kb, ",") {
		t.Fatalf("reordering Spec.Files changed the ConfigMap keys:\n  %v\n  %v", ka, kb)
	}
	// Positive control: the keys are not all identical to begin with.
	if len(ka) != 2 || ka[0] == ka[1] {
		t.Fatalf("control: expected two distinct keys, got %v", ka)
	}
	// And the fingerprint must agree, or a reorder rebuilds the instance.
	if provision.Fingerprint(a) != provision.Fingerprint(b) {
		t.Fatal("reordering Spec.Files moved the fingerprint")
	}
}

// fileKeyFor recomputes the ConfigMap/Secret key a file's PATH maps to,
// independently of the driver's own helper.
//
// ⚠ A SECOND IMPLEMENTATION ON PURPOSE. An expectation read out of the function
// under test proves only that the code calls itself. This one covers the simple
// case the fixtures use — a basename made of key-safe characters, short enough
// not to be truncated — and would be wrong for anything else, which is why the
// fixtures stay simple.
func fileKeyFor(p string) string {
	sum := sha256.Sum256([]byte(p))
	return hex.EncodeToString(sum[:6]) + "-" + path.Base(p)
}

// keysOf is for failure messages: a count alone does not say WHICH key is in
// the wrong object.
func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// TestRestartReasonSurvivesTheRestartThatHidesIt is the port of the single most
// load-bearing field on Instance.
//
// The fixture is the exact state the field exists for: a container that is
// RUNNING and READY, with a bumped restart count, whose last termination was an
// out-of-memory kill. Every other field says the instance is healthy.
func TestRestartReasonSurvivesTheRestartThatHidesIt(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) { testRestartReasonSurvivesTheRestartThatHidesIt(t, m) })
}

func testRestartReasonSurvivesTheRestartThatHidesIt(t *testing.T, m nsMode) {
	d, cs := newDriver(t, m, nil)
	ctx := context.Background()
	spec := provisiontest.MinimalSpec("oomer")
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ns := m.ns("oomer")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "oomer-7d4c8",
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "muster-agent",
				"app.kubernetes.io/instance":   "oomer",
				"app.kubernetes.io/managed-by": "muster",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "agent",
				Ready:        true,
				RestartCount: 3,
				State:        corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137},
				},
			}},
		},
	}
	if _, err := cs.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed pod: %v", err)
	}

	inst, err := d.Get(ctx, spec.Ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if inst.Phase != provision.PhaseRunning || !inst.Ready {
		t.Fatalf("control: the pod must read healthy in every other field, got phase=%q ready=%t", inst.Phase, inst.Ready)
	}
	if inst.Restarts != 3 {
		t.Errorf("Restarts = %d, want 3", inst.Restarts)
	}
	if inst.RestartReason != "OOMKilled" {
		t.Fatalf("RestartReason = %q, want %q. It is the ONLY field that says why a running, ready "+
			"instance last died — without it, an agent too small for its repository is indistinguishable "+
			"from a healthy one.", inst.RestartReason, "OOMKilled")
	}
	// Reason is the WAITING reason and must stay distinct: folding the two
	// makes a current problem and a past one the same field.
	if inst.Reason != "" {
		t.Errorf("Reason = %q, want empty — the container is not waiting", inst.Reason)
	}
	if inst.InstanceID != "oomer-7d4c8" {
		t.Errorf("InstanceID = %q, want the pod name", inst.InstanceID)
	}
	if inst.Ref.ID != 0 {
		t.Errorf("Ref.ID = %d, want 0 — MinimalSpec sets none and none must be invented", inst.Ref.ID)
	}
}

// TestCorrelationIDRoundTripsWhenSet is the other half: an id that WAS supplied
// comes back.
func TestCorrelationIDRoundTripsWhenSet(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) { testCorrelationIDRoundTripsWhenSet(t, m) })
}

func testCorrelationIDRoundTripsWhenSet(t *testing.T, m nsMode) {
	d, _ := newDriver(t, m, nil)
	ctx := context.Background()
	spec := provisiontest.MinimalSpec("correlated")
	spec.Ref.ID = 4217
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	inst, err := d.Get(ctx, provision.Ref{Name: "correlated"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if inst.Ref.ID != 4217 {
		t.Fatalf("Ref.ID = %d, want 4217", inst.Ref.ID)
	}
}

// TestDeploymentStrategyDependsOnTheWorkspace pins a choice whose failure mode
// reads as a cluster problem rather than a strategy one.
func TestDeploymentStrategyDependsOnTheWorkspace(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) { testDeploymentStrategyDependsOnTheWorkspace(t, m) })
}

func testDeploymentStrategyDependsOnTheWorkspace(t *testing.T, m nsMode) {
	ctx := context.Background()

	d, cs := newDriver(t, m, nil)
	persistent := provisiontest.MinimalSpec("stateful")
	persistent.Workspace = provision.Workspace{Path: "/data", Size: "5Gi", Persist: true}
	if err := d.Create(ctx, persistent); err != nil {
		t.Fatalf("Create: %v", err)
	}
	dep, err := cs.AppsV1().Deployments(m.ns("stateful")).Get(ctx, "stateful", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if dep.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("a persistent workspace must use Recreate (a ReadWriteOnce volume deadlocks a rolling "+
			"update), got %q", dep.Spec.Strategy.Type)
	}

	stateless := provisiontest.MinimalSpec("stateless")
	if err := d.Create(ctx, stateless); err != nil {
		t.Fatalf("Create: %v", err)
	}
	dep, err = cs.AppsV1().Deployments(m.ns("stateless")).Get(ctx, "stateless", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if dep.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType {
		t.Fatalf("a stateless instance must roll, got %q", dep.Spec.Strategy.Type)
	}
	if dep.Spec.Strategy.RollingUpdate.MaxUnavailable.IntValue() != 0 {
		t.Error("maxUnavailable must be 0 so a roll is not an outage")
	}
}

// TestSecretValuesNeverAppearInThePodSpec. A Deployment is readable by anything
// that can read the namespace; a Secret is not.
func TestSecretValuesNeverAppearInThePodSpec(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) { testSecretValuesNeverAppearInThePodSpec(t, m) })
}

func testSecretValuesNeverAppearInThePodSpec(t *testing.T, m nsMode) {
	const secretValue = "token-value-19731"
	d, cs := newDriver(t, m, nil)
	ctx := context.Background()
	spec := richSpec("confidential")
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	dep, err := cs.AppsV1().Deployments(m.ns("confidential")).Get(ctx, "confidential", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	b, err := json.Marshal(dep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), secretValue) {
		t.Fatal("a secret value appears in the Deployment object")
	}
	if strings.Contains(string(b), "credential-content-8421") {
		t.Fatal("a secret FILE's content appears in the Deployment object")
	}
	// Positive control: the value IS somewhere, so the absence above is
	// placement and not omission.
	sec, err := cs.CoreV1().Secrets(m.ns("confidential")).Get(ctx, "confidential-env", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if string(sec.Data["MUSTER_CALLBACK_TOKEN"]) != secretValue {
		t.Fatalf("the secret value is not in the Secret either: %q", sec.Data["MUSTER_CALLBACK_TOKEN"])
	}
}

// envVarName is Kubernetes' own env-var name format (`envVarNameFmt` in
// apimachinery): letters, digits, `-`, `.` and `_`, and it MUST NOT START WITH
// A DIGIT. A key of an envFrom'd Secret that matches this becomes an
// environment variable; one that does not is skipped with an
// InvalidEnvironmentVariableNames warning.
var envVarName = regexp.MustCompile(`^[-._a-zA-Z][-._a-zA-Z0-9]*$`)

// TestSecretFilesAreNotExportedIntoTheEnvironment.
//
// 🔴 THE DEFECT THIS PINS: confidential environment and confidential FILES
// shared one Secret, and that Secret is consumed with `envFrom` — which exports
// EVERY key of it as an environment variable. A file declared Secret and
// mounted 0600 was therefore also in /proc/self/environ and inherited by every
// child process. The fixture's path is chosen so its key STARTS WITH A LETTER,
// which is the half of the key space Kubernetes accepts as an env var name: for
// the other half the same defect only produced a warning, which is how it could
// look like noise rather than a leak.
func TestSecretFilesAreNotExportedIntoTheEnvironment(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) { testSecretFilesAreNotExportedIntoTheEnvironment(t, m) })
}

func testSecretFilesAreNotExportedIntoTheEnvironment(t *testing.T, m nsMode) {
	const (
		secretPath = "/etc/muster/token"
		fileBody   = "file-content-30517"
		envName    = "MUSTER_CALLBACK_TOKEN"
		envValue   = "env-value-24413"
	)
	key := fileKeyFor(secretPath)
	if !envVarName.MatchString(key) {
		t.Fatalf("control: the fixture's file key %q is NOT a valid env var name, so this case could "+
			"not observe the export it exists to forbid; choose a path whose digest starts with a letter", key)
	}

	cases := []struct {
		name           string
		secrets        []provision.EnvVar
		files          []provision.File
		wantEnvSecret  bool
		wantFileSecret bool
	}{
		{
			name:           "confidential env and confidential files together",
			secrets:        []provision.EnvVar{{Name: envName, Value: envValue}},
			files:          []provision.File{{Path: secretPath, Content: []byte(fileBody), Secret: true}},
			wantEnvSecret:  true,
			wantFileSecret: true,
		},
		{
			// 🔴 THE CASE THE OLD `len(spec.Secrets) > 0` GUARD GOT RIGHT, kept
			// because the fix must not regress it: a spec with only secret
			// FILES gets no envFrom at all.
			name:           "confidential files only",
			files:          []provision.File{{Path: secretPath, Content: []byte(fileBody), Secret: true}},
			wantFileSecret: true,
		},
		{
			name:          "confidential env only",
			secrets:       []provision.EnvVar{{Name: envName, Value: envValue}},
			wantEnvSecret: true,
		},
	}

	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			d, cs := newDriver(t, m, nil)
			name := fmt.Sprintf("split-%d", i)
			spec := provisiontest.MinimalSpec(name)
			spec.Secrets = c.secrets
			spec.Files = c.files
			if err := d.Create(ctx, spec); err != nil {
				t.Fatalf("Create: %v", err)
			}
			ns := m.ns(name)

			envSec, envErr := cs.CoreV1().Secrets(ns).Get(ctx, name+"-env", metav1.GetOptions{})
			if c.wantEnvSecret {
				if envErr != nil {
					t.Fatalf("env secret: %v", envErr)
				}
				for k := range envSec.Data {
					if k == key {
						t.Errorf("the secret FILE's key %q is in the envFrom'd secret; its content becomes "+
							"an environment variable", k)
					}
					if !envVarName.MatchString(k) {
						t.Errorf("key %q of the envFrom'd secret is not a valid env var name; nothing that "+
							"is not environment belongs in this object", k)
					}
				}
				if string(envSec.Data[envName]) != envValue {
					t.Errorf("the env secret does not hold the declared value; keys %v", keysOf(envSec.Data))
				}
			} else if envErr == nil {
				t.Errorf("an env secret was created for a spec declaring no confidential environment: keys %v",
					keysOf(envSec.Data))
			}

			fileSec, fileErr := cs.CoreV1().Secrets(ns).Get(ctx, name+"-secret-files", metav1.GetOptions{})
			if c.wantFileSecret {
				if fileErr != nil {
					t.Fatalf("secret-file secret: %v", fileErr)
				}
				if string(fileSec.Data[key]) != fileBody {
					t.Errorf("the secret file's content is not in the mounted secret; keys %v", keysOf(fileSec.Data))
				}
				if _, wrong := fileSec.Data[envName]; wrong {
					t.Error("a confidential ENV VAR is in the mounted file secret")
				}
			} else if fileErr == nil {
				t.Errorf("a secret-file secret was created for a spec declaring no confidential files: keys %v",
					keysOf(fileSec.Data))
			}

			dep, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("deployment: %v", err)
			}
			container := dep.Spec.Template.Spec.Containers[0]
			var envFromNames []string
			for _, ef := range container.EnvFrom {
				if ef.SecretRef != nil {
					envFromNames = append(envFromNames, ef.SecretRef.Name)
				}
			}
			switch {
			case c.wantEnvSecret:
				if len(envFromNames) != 1 || envFromNames[0] != name+"-env" {
					t.Errorf("envFrom is %v, want exactly the env secret", envFromNames)
				}
			default:
				if len(envFromNames) != 0 {
					t.Errorf("envFrom is %v for a spec with no confidential environment; every key of a "+
						"referenced secret becomes an environment variable", envFromNames)
				}
			}

			// The mounted volume must name the FILE secret, not the env one.
			for _, v := range dep.Spec.Template.Spec.Volumes {
				if v.Secret == nil {
					continue
				}
				if v.Secret.SecretName == name+"-env" {
					t.Errorf("volume %q mounts the envFrom'd secret; the two objects exist to be different", v.Name)
				}
			}
		})
	}
}

// TestUpdateReplacesTheSpecAndCreateIsThenIdempotent — the reconcile loop's
// actual sequence.
func TestUpdateReplacesTheSpecAndCreateIsThenIdempotent(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) { testUpdateReplacesTheSpecAndCreateIsThenIdempotent(t, m) })
}

func testUpdateReplacesTheSpecAndCreateIsThenIdempotent(t *testing.T, m nsMode) {
	d, cs := newDriver(t, m, nil)
	ctx := context.Background()
	spec := provisiontest.MinimalSpec("rolling")
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	before, _ := cs.AppsV1().Deployments(m.ns("rolling")).Get(ctx, "rolling", metav1.GetOptions{})

	changed := spec
	changed.Runtime.Image = "ghcr.io/muster-example/rolled:3"
	if err := d.Update(ctx, changed); err != nil {
		t.Fatalf("Update: %v", err)
	}
	after, err := cs.AppsV1().Deployments(m.ns("rolling")).Get(ctx, "rolling", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.Spec.Template.Spec.Containers[0].Image != "ghcr.io/muster-example/rolled:3" {
		t.Fatalf("Update did not change the image: %q", after.Spec.Template.Spec.Containers[0].Image)
	}
	if before.Annotations["muster.dev/spec-fingerprint"] == after.Annotations["muster.dev/spec-fingerprint"] {
		t.Fatal("the recorded fingerprint did not move; the next Create would see divergence forever")
	}
	if err := d.Create(ctx, changed); err != nil {
		t.Fatalf("Create after Update must be idempotent: %v", err)
	}
}

// TestDestroyVerifiesRatherThanAssumes is the mutation-proof form of contract
// rule 2. The reactor makes DELETE succeed while removing nothing — exactly
// what a finalizer, a webhook or a permission quirk can do — and Destroy must
// NOT report success.
func TestDestroyVerifiesRatherThanAssumes(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) { testDestroyVerifiesRatherThanAssumes(t, m) })
}

func testDestroyVerifiesRatherThanAssumes(t *testing.T, m nsMode) {
	d, cs := newDriver(t, m, nil)
	ctx := context.Background()
	spec := provisiontest.MinimalSpec("stubborn")
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Control: without the reactor, Destroy succeeds. Without this the case
	// could pass because Destroy always fails.
	d2, _ := newDriver(t, m, nil)
	if err := d2.Create(ctx, spec); err != nil {
		t.Fatalf("control Create: %v", err)
	}
	if err := d2.Destroy(ctx, spec.Ref); err != nil {
		t.Fatalf("control: Destroy must succeed on a normal backend, got %v", err)
	}

	cs.PrependReactor("delete", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		// "Accepted" — and nothing is removed.
		return true, nil, nil
	})
	err := d.Destroy(ctx, spec.Ref)
	if err == nil {
		t.Fatal("Destroy returned nil while the deployment survived. nil means removed or already absent.")
	}
	if !strings.Contains(err.Error(), "still present") {
		t.Fatalf("the error must say what it found, got %q", err)
	}
}

// --------------------------------------------------------------------------
// Endpoint
// --------------------------------------------------------------------------

func TestEndpointTemplateIsConfigurable(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) { testEndpointTemplateIsConfigurable(t, m) })
}

func testEndpointTemplateIsConfigurable(t *testing.T, m nsMode) {
	ctx := context.Background()
	d, _ := newDriver(t, m, func(c *k8s.Config) {
		c.EndpointTemplate = "{{.Name}}.agents.example.test"
		c.EndpointScheme = "https"
	})
	spec := provisiontest.MinimalSpec("reachable")
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ep, err := d.Endpoint(ctx, spec.Ref)
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if ep.Host != "reachable.agents.example.test" || ep.Scheme != "https" || ep.Port != 8421 {
		t.Fatalf("the configured template was not used: %+v", ep)
	}

	// The default, for comparison — and to prove the case above is measuring a
	// change rather than a coincidence.
	dflt, _ := newDriver(t, m, nil)
	if err := dflt.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ep2, err := dflt.Endpoint(ctx, spec.Ref)
	if err != nil {
		t.Fatalf("Endpoint: %v", err)
	}
	if ep2.Host != "reachable."+m.ns("reachable")+".svc" {
		t.Fatalf("default endpoint host is %q", ep2.Host)
	}
	if ep2.Host == ep.Host {
		t.Fatal("the configured and default hosts are identical; this case could not tell them apart")
	}
}

func TestEndpointOfAPortlessInstance(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		d, _ := newDriver(t, m, nil)
		ctx := context.Background()
		spec := provisiontest.MinimalSpec("portless")
		spec.Ports = nil
		if err := d.Create(ctx, spec); err != nil {
			t.Fatalf("Create: %v", err)
		}
		_, err := d.Endpoint(ctx, spec.Ref)
		if !errors.Is(err, provision.ErrNoEndpoint) {
			t.Fatalf("an instance with no ports must be ErrNoEndpoint — distinct from ErrNotFound (it exists) "+
				"and from ErrBlind (we can see it); got %v", err)
		}
	})
}

// --------------------------------------------------------------------------
// Logs
// --------------------------------------------------------------------------

func TestTailLogsOfAnInstanceWithNoPodYet(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		d, _ := newDriver(t, m, nil)
		ctx := context.Background()
		spec := provisiontest.MinimalSpec("nopod")
		if err := d.Create(ctx, spec); err != nil {
			t.Fatalf("Create: %v", err)
		}
		out, err := d.TailLogs(ctx, spec.Ref, 10)
		if err != nil {
			t.Fatalf("an instance with no pod yet is not a failure: %v", err)
		}
		if out != "" {
			t.Fatalf("want empty output, got %q", out)
		}
	})
}
