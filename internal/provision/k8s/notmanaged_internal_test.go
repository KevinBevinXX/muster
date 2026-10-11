package k8s

// This file is IN-PACKAGE on purpose. It pins what the external
// ownership_test.go structurally cannot reach: the unexported notManagedError's
// own fields and its survival through errors.As out of every refusal site, and
// labelComplaint's exact output. The external file pins what a CALLER sees —
// the exported sentinels — and that is the right split: a guard on an
// unexported field says nothing about the contract, and a guard on the
// contract cannot see whether the type made it.

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ZacxDev/muster/internal/provision"
)

// newForeignDeployment is a Deployment somebody else made, with a replica count
// distinct from anything the driver renders so a rewrite would be visible.
func newForeignDeployment(name string) *appsv1.Deployment {
	replicas := int32(2)
	return &appsv1.Deployment{
		ObjectMeta: internalForeignMeta(name),
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}
}

const internalNS = "agents"

// internalNetworkPolicy is the isolation half of internalDriver's configuration.
// Its values are deliberately NOT the ones any real deployment or any default in
// network.go uses, so an assertion that pins them cannot be satisfied by a
// hardcoded literal.
func internalNetworkPolicy() *NetworkPolicyConfig {
	return &NetworkPolicyConfig{
		ControllerNamespace: "control-plane-7",
		ControllerPodLabels: map[string]string{"app": "muster-server", "tier": "control"},
	}
}

// internalDriver is configured for network isolation, which is the shape of a
// deployment that enables an isolated kind: every spec that declares NO isolation
// still goes through a driver that could render a policy, and must get none.
func internalDriver(t *testing.T) (*Driver, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset()
	empty := ""
	d, err := New(Config{Client: cs, Namespace: internalNS, WorkspaceStorageClass: &empty,
		NetworkPolicy: internalNetworkPolicy()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d, cs
}

func internalSpec(name string) provision.Spec {
	return provision.Spec{
		Ref:     provision.Ref{Name: name},
		Runtime: provision.Runtime{Image: "ghcr.io/muster-example/agent:1"},
		Ports:   []provision.Port{{Name: provision.DefaultPortName, Port: 8421}},
	}
}

func internalForeignMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: internalNS, Labels: map[string]string{
		"app.kubernetes.io/name":       "grafana",
		"app.kubernetes.io/managed-by": "Helm",
	}}
}

// TestEveryWriteSiteRefusalKeepsItsTypeAndItsSentinels is the LEDGER of every
// object kind apply and Destroy can refuse on.
//
// 🔴 IT IS A SEAM GUARD, NOT A PER-SITE ONE. Each site was individually
// reviewed and each refusal was individually correct where it was CONSTRUCTED;
// what was broken was the wrapping between the construction and the caller.
// Five of apply's six sites wrapped the refusal in blind(), which formats with
// %v, so at 138d269 errors.As(err, **notManagedError) was FALSE on all five and
// notManagedError's own doc comment — "the concrete type survives errors.As" —
// was a false statement about five of its six producers. A table asserting them
// all together is what makes a site added later visible: it either appears here
// or the ledger is short.
//
// 🔴 THE LEDGER WAS ONCE SHORT. Fixing apply's five sites left the (since
// removed) policy grant path out of the consolidation entirely: at f373092 its
// ClusterRole and Role upserts still went through blind(), so a permanent
// ownership refusal arrived as ErrBlind. That path and its rows are gone with
// the privilege subsystem. The rows below are the answer to "which write sites
// exist", and
// TestTheWriteHelperLedgerIsComplete is the answer to "are there sites this
// table has never heard of".
//
// ⚠ ONE REFUSAL IS DELIBERATELY ABSENT FROM THIS TABLE: ensureNamespace's. It
// is a plain fmt.Errorf wrapping provision.ErrNotManaged rather than a
// *notManagedError, because its message carries a reason the type's fixed
// format cannot express — see the type's own doc comment, which states that
// narrowing. A table about the TYPE cannot hold it. Its SENTINELS are pinned
// externally, by ownership_test.go's TestCreateDoesNotAdoptANamespaceItDidNot-
// Create, which runs the same assertRefusal every other caller-visible refusal
// goes through. This paragraph exists because the sentence "pins every write
// site" would otherwise be wider than what this table does.
func TestEveryWriteSiteRefusalKeepsItsTypeAndItsSentinels(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		site string
		kind string
		seed func(*fake.Clientset)
		act  func(*Driver) error
	}{
		{
			site: "apply/serviceaccount",
			kind: "serviceaccount",
			seed: func(cs *fake.Clientset) {
				cs.CoreV1().ServiceAccounts(internalNS).Create(ctx, //nolint:errcheck // fake cannot fail here
					&corev1.ServiceAccount{ObjectMeta: internalForeignMeta("sa")}, metav1.CreateOptions{})
			},
			act: func(d *Driver) error { return d.Create(ctx, internalSpec("sa")) },
		},
		{
			site: "apply/configmap",
			kind: "configmap",
			seed: func(cs *fake.Clientset) {
				cs.CoreV1().ConfigMaps(internalNS).Create(ctx, //nolint:errcheck
					&corev1.ConfigMap{ObjectMeta: internalForeignMeta("cm-files")}, metav1.CreateOptions{})
			},
			act: func(d *Driver) error {
				s := internalSpec("cm")
				s.Files = []provision.File{{Path: "/etc/muster/agent.json", Content: []byte("{}")}}
				return d.Create(ctx, s)
			},
		},
		{
			site: "apply/env secret",
			kind: "secret",
			seed: func(cs *fake.Clientset) {
				cs.CoreV1().Secrets(internalNS).Create(ctx, //nolint:errcheck
					&corev1.Secret{ObjectMeta: internalForeignMeta("es-env")}, metav1.CreateOptions{})
			},
			act: func(d *Driver) error {
				s := internalSpec("es")
				s.Secrets = []provision.EnvVar{{Name: "MUSTER_CALLBACK_TOKEN", Value: "v-51199"}}
				return d.Create(ctx, s)
			},
		},
		{
			site: "apply/file secret",
			kind: "secret",
			seed: func(cs *fake.Clientset) {
				cs.CoreV1().Secrets(internalNS).Create(ctx, //nolint:errcheck
					&corev1.Secret{ObjectMeta: internalForeignMeta("fs-secret-files")}, metav1.CreateOptions{})
			},
			act: func(d *Driver) error {
				s := internalSpec("fs")
				s.Files = []provision.File{{Path: "/etc/muster/token", Content: []byte("v"), Secret: true}}
				return d.Create(ctx, s)
			},
		},
		{
			site: "apply/pvc",
			kind: "persistentvolumeclaim",
			seed: func(cs *fake.Clientset) {
				cs.CoreV1().PersistentVolumeClaims(internalNS).Create(ctx, //nolint:errcheck
					&corev1.PersistentVolumeClaim{ObjectMeta: internalForeignMeta("pv-workspace")},
					metav1.CreateOptions{})
			},
			act: func(d *Driver) error {
				s := internalSpec("pv")
				s.Workspace = provision.Workspace{Path: "/data", Size: "5Gi", Persist: true}
				return d.Create(ctx, s)
			},
		},
		{
			site: "apply/service",
			kind: "service",
			seed: func(cs *fake.Clientset) {
				cs.CoreV1().Services(internalNS).Create(ctx, //nolint:errcheck
					&corev1.Service{ObjectMeta: internalForeignMeta("sv")}, metav1.CreateOptions{})
			},
			act: func(d *Driver) error { return d.Create(ctx, internalSpec("sv")) },
		},
		{
			// A stranger's co-named NetworkPolicy is NOT adopted and NOT
			// overwritten: muster's rules written over it would silently replace
			// whatever that policy was protecting, and leaving it as the instance's
			// "confinement" would start a pod under rules nobody here chose.
			site: "apply/networkpolicy",
			kind: "networkpolicy",
			seed: func(cs *fake.Clientset) {
				cs.NetworkingV1().NetworkPolicies(internalNS).Create(ctx, //nolint:errcheck
					&networkingv1.NetworkPolicy{ObjectMeta: internalForeignMeta("np-network")},
					metav1.CreateOptions{})
			},
			act: func(d *Driver) error {
				s := internalSpec("np")
				s.Network = provision.Network{Isolate: true, PublicEgressTCPPorts: []int{8443}}
				return d.Create(ctx, s)
			},
		},
		{
			// ⚠ THIS ROW REACHES UPDATE'S PRE-FLIGHT, NOT apply'S DEPLOYMENT
			// UPSERT, and it used to be labelled "apply/deployment (via
			// Update)" — a name for a site it never touches. Measured: mutating
			// the label getter apply passes to upsertOwned for the Deployment
			// (`return managedLabels, nil`) compiles and leaves the whole suite
			// green, INCLUDING this subtest, because Update refuses at its own
			// pre-flight read before apply runs at all. The next row is the one
			// that reaches the upsert.
			site: "Update/pre-flight deployment",
			kind: "deployment",
			seed: func(cs *fake.Clientset) {
				cs.AppsV1().Deployments(internalNS).Create(ctx, //nolint:errcheck
					newForeignDeployment("dp"), metav1.CreateOptions{})
			},
			act: func(d *Driver) error { return d.Update(ctx, internalSpec("dp")) },
		},
		{
			// 🔴 THE ROW THAT REACHES apply'S OWN DEPLOYMENT UPSERT. It needs a
			// foreign Deployment that is NOT there when Update pre-flights and
			// IS there when apply writes the Deployment last — which is exactly
			// the TOCTOU window that upsert's ownership check exists for, and
			// the only way to execute it. The reactor injects the stranger's
			// Deployment while apply is writing its FIRST object, so the
			// pre-flight saw nothing and the upsert collides.
			site: "apply/deployment (TOCTOU)",
			kind: "deployment",
			seed: func(cs *fake.Clientset) {
				injected := false
				cs.PrependReactor("create", "serviceaccounts",
					func(k8stesting.Action) (bool, runtime.Object, error) {
						if !injected {
							injected = true
							// handled=false below, so the tracker still
							// performs the ServiceAccount create itself.
							_ = cs.Tracker().Add(newForeignDeployment("tc"))
						}
						return false, nil, nil
					})
			},
			act: func(d *Driver) error { return d.Update(ctx, internalSpec("tc")) },
		},
		{
			site: "Destroy/deployment",
			kind: "deployment",
			seed: func(cs *fake.Clientset) {
				cs.AppsV1().Deployments(internalNS).Create(ctx, //nolint:errcheck
					newForeignDeployment("dz"), metav1.CreateOptions{})
			},
			act: func(d *Driver) error { return d.Destroy(ctx, provision.Ref{Name: "dz"}) },
		},
	}

	for _, c := range cases {
		t.Run(c.site, func(t *testing.T) {
			d, cs := internalDriver(t)
			c.seed(cs)
			err := c.act(d)
			if err == nil {
				t.Fatalf("%s: no refusal at all", c.site)
			}

			var foreign *notManagedError
			if !errors.As(err, &foreign) {
				t.Fatalf("%s: the concrete *notManagedError did NOT survive to the caller. Its own doc "+
					"comment claims it does, and Update's pre-flight branch reads it with errors.As. "+
					"Got %v", c.site, err)
			}
			if foreign.kind != c.kind {
				t.Errorf("%s: the refusal names kind %q, want %q — a refusal naming the wrong object "+
					"sends an operator to relabel something that is not in the way",
					c.site, foreign.kind, c.kind)
			}
			if foreign.fromRead {
				t.Errorf("%s: this refusal answered a WRITE or a TEARDOWN, so fromRead must be false: "+
					"an ErrNotFound-flavoured refusal here is discarded by `!errors.Is(err, "+
					"provision.ErrNotFound)`", c.site)
			}
			if !errors.Is(err, provision.ErrNotManaged) {
				t.Errorf("%s: must report provision.ErrNotManaged, got %v", c.site, err)
			}
			if errors.Is(err, provision.ErrBlind) {
				t.Errorf("%s: must NOT report provision.ErrBlind — a permanent refusal arriving as a "+
					"transient fault is the defect this ledger exists for. Got %v", c.site, err)
			}
			if errors.Is(err, provision.ErrNotFound) {
				t.Errorf("%s: must NOT report provision.ErrNotFound, got %v", c.site, err)
			}
		})
	}

	// The NAMESPACE anchor needs the other layout, so it cannot join the table
	// above — but leaving it out would make notManagedError's "survives
	// errors.As on every path" claim wider than what is pinned. Its refusal
	// travels through two more %w wrappings than any other (fail -> firstErr ->
	// "destroy %q did not complete"), which is exactly where a %v would break
	// the chain again.
	t.Run("Destroy/namespace", func(t *testing.T) {
		cs := fake.NewClientset()
		empty := ""
		d, err := New(Config{Client: cs, NamespacePerInstance: true, WorkspaceStorageClass: &empty})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: "muster-lonely", Labels: map[string]string{
				"app.kubernetes.io/name":       "grafana",
				"app.kubernetes.io/managed-by": "Helm",
			}},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed namespace: %v", err)
		}
		derr := d.Destroy(ctx, provision.Ref{Name: "lonely"})
		var foreign *notManagedError
		if !errors.As(derr, &foreign) {
			t.Fatalf("Destroy/namespace: the concrete type did not survive two levels of wrapping: %v", derr)
		}
		if foreign.kind != "namespace" || foreign.fromRead {
			t.Errorf("Destroy/namespace: refusal is kind=%q fromRead=%v, want namespace/false",
				foreign.kind, foreign.fromRead)
		}
		if foreign.ns != "" {
			t.Errorf("a Namespace is cluster-scoped, so its refusal must carry no namespace of its "+
				"own — an 'in namespace \"muster-lonely\"' clause about muster-lonely itself reads as "+
				"a second object; got %q", foreign.ns)
		}
		if !errors.Is(derr, provision.ErrNotManaged) || errors.Is(derr, provision.ErrNotFound) ||
			errors.Is(derr, provision.ErrBlind) {
			t.Errorf("Destroy/namespace sentinels are wrong: %v", derr)
		}
	})

	// 🔴 THE OTHER DIRECTION, so the assertions above cannot be satisfied by a
	// driver that flags every refusal the same way: a by-name READ must keep
	// ErrNotFound and must be read-flavoured.
	t.Run("read paths stay ErrNotFound", func(t *testing.T) {
		d, cs := internalDriver(t)
		cs.AppsV1().Deployments(internalNS).Create(ctx, //nolint:errcheck
			newForeignDeployment("rd"), metav1.CreateOptions{})
		_, err := d.Get(ctx, provision.Ref{Name: "rd"})
		var foreign *notManagedError
		if !errors.As(err, &foreign) {
			t.Fatalf("Get: the concrete type did not survive: %v", err)
		}
		if !foreign.fromRead {
			t.Error("Get's refusal must be read-flavoured: a status read that reported a stranger's " +
				"Deployment as an instance is the defect the ownership check exists for, and " +
				"'muster has no instance by that name' is the right answer to give")
		}
		if !errors.Is(err, provision.ErrNotFound) || !errors.Is(err, provision.ErrNotManaged) {
			t.Errorf("Get's refusal must report BOTH ErrNotFound and ErrNotManaged, got %v", err)
		}
		if errors.Is(err, provision.ErrBlind) {
			t.Errorf("Get's refusal must not report ErrBlind, got %v", err)
		}
	})
}

// TestApplyFailedKeysOnTheSentinelNotTheType pins applyFailed's predicate
// directly, because no behavioural case in this package can reach the
// difference: every refusal that currently arrives at applyFailed happens to BE
// a *notManagedError, so errors.As and errors.Is agree on all of them.
//
// 🔴 THE DIFFERENCE IS NOT HYPOTHETICAL. ensureNamespace returns its ownership
// refusal as a plain fmt.Errorf wrapping provision.ErrNotManaged (its message
// carries a reason the concrete type's fixed format cannot express, and
// ownership_test.go asserts that text). Under an errors.As predicate, any future
// write path that routed such a refusal through applyFailed would have it
// re-wrapped as ErrBlind — the f373092 defect in a new shape, and the shape
// nothing would catch. This case is the guard for the WIDER predicate, and it is
// reachable by construction rather than by luck.
func TestApplyFailedKeysOnTheSentinelNotTheType(t *testing.T) {
	// A refusal in the shape ensureNamespace produces: the sentinel, no type.
	sentinelOnly := fmt.Errorf("%w: namespace %q already exists and is not managed by muster",
		provision.ErrNotManaged, "muster-x")
	if errors.As(sentinelOnly, new(*notManagedError)) {
		t.Fatal("premise: this fixture must NOT carry the concrete type, or the case cannot tell the " +
			"two predicates apart")
	}
	got := applyFailed("apply namespace muster-x", sentinelOnly)
	if !errors.Is(got, provision.ErrNotManaged) {
		t.Errorf("applyFailed dropped the ownership sentinel: %v", got)
	}
	if errors.Is(got, provision.ErrBlind) {
		t.Errorf("applyFailed re-wrapped a PERMANENT ownership refusal as ErrBlind because the refusal "+
			"was not the concrete type. A caller retries ErrBlind forever: %v", got)
	}

	// 🔴 THE OTHER DIRECTION, or the assertion above would pass for a function
	// that never wraps anything: a genuine transport failure MUST become
	// ErrBlind.
	// 192.0.2.0/24 is RFC 5737 TEST-NET-1: a documentation range, so this
	// fixture cannot read as anybody's real network topology.
	transport := errors.New("dial tcp 192.0.2.10:443: connect: connection refused")
	blinded := applyFailed("apply serviceaccount x", transport)
	if !errors.Is(blinded, provision.ErrBlind) {
		t.Errorf("applyFailed must report an unreachable API as ErrBlind, got %v", blinded)
	}
	if errors.Is(blinded, provision.ErrNotManaged) {
		t.Errorf("a transport error must not claim to be an ownership refusal: %v", blinded)
	}
	// And the concrete type still passes through, which is the case every
	// behavioural row above exercises.
	typed := applyFailed("apply deployment x", notManagedAction("deployment", "x", internalNS, nil))
	if !errors.Is(typed, provision.ErrNotManaged) || errors.Is(typed, provision.ErrBlind) {
		t.Errorf("applyFailed mishandled a typed refusal: %v", typed)
	}
	if !errors.As(typed, new(*notManagedError)) {
		t.Errorf("applyFailed broke the concrete type's errors.As chain: %v", typed)
	}
}

// TestTheWriteHelperLedgerIsComplete is the STRUCTURAL half of the ledger
// above: a census of every call site of the ownership helpers, by enclosing
// function, asserted against a declared table.
//
// 🔴 IT EXISTS BECAUSE THE BEHAVIOURAL LEDGER CANNOT SEE A SITE NOBODY WROTE A
// ROW FOR. That is not hypothetical: the since-removed policy Grant had two upserts wrapped
// in blind() and two bindings with no ownership check at all, and it stayed that
// way through two review rounds because every test asked "is this site right?"
// and none asked "which sites are there?". apply's own comment said as much
// ("nothing stops a SIXTH site calling blind() directly") and was already
// describing a site that existed.
//
// 🔴 IT FAILS WHEN THE SET GROWS *OR* SHRINKS, and that is the point rather than
// brittleness. A new number here is not a chore: it is the moment to decide
// which helper the new site belongs to. The decision procedure is in the failure
// message.
//
// ⚠ WHAT IT DOES NOT CHECK: that a site is CORRECT. A census cannot tell a
// blind() wrapping a raw client-go error from one wrapping an ownership refusal;
// only the behavioural rows above can. The two halves are complementary and
// neither is sufficient — the counts were what nothing pinned, and the prose
// claims about them ("five upserts", "six kinds", "all six write sites") were
// therefore unfalsifiable.
func TestTheWriteHelperLedgerIsComplete(t *testing.T) {
	// The census, hand-derived by reading driver.go. Each row is
	// helper -> enclosing function -> number of call sites.
	want := map[string]map[string]int{
		// blind() must only ever wrap a RAW client-go error. Every entry here
		// is a read or a write that cannot collide with a stranger's object:
		// there is nothing for an ownership check to refuse. applyFailed's own
		// tail call is the one exception and is how a non-refusal reaches
		// blind() from a collision path.
		"blind": {
			"applyFailed":         1,
			"getOwnedDeployment":  1,
			"Create":              1,
			"ensureNamespace":     2,
			"checkNamespaceOwned": 1,
			"Scale":               1,
			"Destroy":             1,
			"List":                2,
			"Get":                 1,
			"podFor":              1,
			"TailLogs":            1,
			"StreamLogs":          1,
		},
		// Every write that CAN collide returns through applyFailed, which is
		// what keeps a permanent refusal out of ErrBlind. Six object kinds in
		// apply (ServiceAccount, ConfigMap, Secret, claim, Service, Deployment
		// — the Secret site serves both Secrets, through the loop).
		//
		// The NetworkPolicy is the seventh kind apply writes and its site is in
		// applyNetworkPolicy, which apply calls first. That function has TWO
		// calls: the default arm, and the arm for a 403 from a namespace that is
		// still terminating (a transient). Every OTHER 403 does not come through
		// here — networkPolicyForbidden names the verb instead.
		"applyFailed": {"apply": 6, "applyNetworkPolicy": 2},
		// The create-then-update-if-ours predicate.
		"upsertOwned": {"apply": 4, "applyNetworkPolicy": 1},
		// The create-only-if-ours predicate: a bound claim, immutable in the
		// fields that matter.
		"createOwned": {"apply": 1},
		// Every by-name DELETE. A satellite is a logged skip, so these are the
		// sites where a stranger's co-named object is left alone rather than
		// destroyed.
		//
		// Destroy's two: the loop over the six satellites, and the NetworkPolicy,
		// which is separate because it is deleted LAST and only when the driver
		// is configured for isolation.
		"deleteIfOwned": {"apply": 3, "Destroy": 2},
		// The predicate itself. Every by-name path that reads, writes or
		// deletes reaches exactly one of these.
		"owned": {
			"getOwnedDeployment":  1,
			"upsertOwned":         1,
			"createOwned":         1,
			"deleteIfOwned":       1,
			"Create":              1,
			"ensureNamespace":     1,
			"checkNamespaceOwned": 1,
			// ⚠ upsertService open-codes the upsert because a Service's
			// ClusterIP has to be carried over. It is the one deliberate
			// exception to "the predicate lives in a helper", and it is why
			// this row exists rather than being folded into upsertOwned.
			"upsertService": 1,
		},
	}

	got := censusOfHelperCalls(t, keysOfCensus(want))

	// 🔴 THE POSITIVE CONTROL, READ OUT LOUD. A census wired to nothing reports
	// zero for every helper, which is indistinguishable from a package that
	// calls none of them. Report the pair before comparing.
	total := 0
	for _, byFunc := range got {
		for _, n := range byFunc {
			total += n
		}
	}
	t.Logf("census found %d call sites across %d helpers", total, len(got))
	if total == 0 {
		t.Fatal("the census found NO call sites at all, so every assertion below would pass " +
			"vacuously: the parser is looking at the wrong files")
	}

	for _, helper := range keysOfCensus(want) {
		wantFuncs, gotFuncs := want[helper], got[helper]
		for _, fn := range union(keysOfCounts(wantFuncs), keysOfCounts(gotFuncs)) {
			if wantFuncs[fn] == gotFuncs[fn] {
				continue
			}
			t.Errorf("%s() is called %d time(s) in %s; the ledger says %d.\n"+
				"  This is a SEAM guard: the count moved, so a call site was added, removed or moved.\n"+
				"  Decide, then update the table:\n"+
				"    - a NEW write that can collide with a stranger's co-named object goes through\n"+
				"      upsertOwned/createOwned/deleteIfOwned and returns through applyFailed, and it\n"+
				"      needs a behavioural row in TestEveryWriteSiteRefusalKeepsItsTypeAndItsSentinels;\n"+
				"    - a blind() may only wrap a RAW client-go error. If the error it wraps can be an\n"+
				"      ownership refusal, this is the f373092 defect again: a permanent refusal\n"+
				"      arriving as a transient ErrBlind.",
				helper, gotFuncs[fn], fn, wantFuncs[fn])
		}
	}
}

// censusOfHelperCalls counts calls to each named helper in this package's
// NON-TEST sources, attributed to the function they appear in.
func censusOfHelperCalls(t *testing.T, helpers []string) map[string]map[string]int {
	t.Helper()
	wanted := map[string]bool{}
	for _, h := range helpers {
		wanted[h] = true
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	out := map[string]map[string]int{}
	files := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files++
		f, perr := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var callee string
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					callee = fun.Name
				case *ast.SelectorExpr:
					callee = fun.Sel.Name
				}
				if !wanted[callee] {
					return true
				}
				if out[callee] == nil {
					out[callee] = map[string]int{}
				}
				out[callee][fn.Name.Name]++
				return true
			})
		}
	}
	if files == 0 {
		t.Fatal("the census parsed NO source files, so its counts are all zero for a reason that has " +
			"nothing to do with the code")
	}
	t.Logf("census parsed %d non-test source file(s)", files)
	return out
}

func keysOfCensus(m map[string]map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func keysOfCounts(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range append(append([]string{}, a...), b...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// TestLabelComplaintNamesTheKeyThatIsActuallyWrong.
//
// 🔴 THE MEASURED DEFECT: the refusal printed
// `app.kubernetes.io/managed-by=muster` and nothing else, while owned()
// requires BOTH keys of managedLabels. An object labelled managed-by=muster
// with app.kubernetes.io/name set to something else was therefore refused with
// "it does not carry app.kubernetes.io/managed-by=muster" — a FALSE statement
// about the object in front of the operator, whose only named remedy was
// already satisfied. Relabelling is the one documented escape from these
// refusals, so following that message looped.
func TestLabelComplaintNamesTheKeyThatIsActuallyWrong(t *testing.T) {
	// ⚠ THE EXPECTATIONS ARE WHOLE STRINGS, NOT KEYWORDS. A guard on words is
	// walkable by rewording; this one fails on a cosmetic change, which is the
	// price of a machine-readable claim about what an operator is told.
	cases := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{
			name:   "neither key present",
			labels: map[string]string{},
			want: `it does not carry app.kubernetes.io/managed-by, which must be "muster"; ` +
				`it does not carry app.kubernetes.io/name, which must be "muster-agent"`,
		},
		{
			name: "both keys present with somebody else's values",
			labels: map[string]string{
				"app.kubernetes.io/managed-by": "Helm",
				"app.kubernetes.io/name":       "grafana",
			},
			want: `its app.kubernetes.io/managed-by is "Helm", not "muster"; ` +
				`its app.kubernetes.io/name is "grafana", not "muster-agent"`,
		},
		{
			// 🔴 THE CASE THE OLD MESSAGE WAS FALSE ABOUT. managed-by is exactly
			// right; the message must not name it as the problem.
			name: "managed-by correct, name wrong",
			labels: map[string]string{
				"app.kubernetes.io/managed-by": "muster",
				"app.kubernetes.io/name":       "something-else",
			},
			want: `its app.kubernetes.io/name is "something-else", not "muster-agent"`,
		},
		{
			// The mirror image, so the function is not merely reporting whichever
			// key it happens to check second.
			name: "name correct, managed-by wrong",
			labels: map[string]string{
				"app.kubernetes.io/managed-by": "flux",
				"app.kubernetes.io/name":       "muster-agent",
			},
			want: `its app.kubernetes.io/managed-by is "flux", not "muster"`,
		},
		{
			name: "name correct, managed-by absent",
			labels: map[string]string{
				"app.kubernetes.io/name": "muster-agent",
			},
			want: `it does not carry app.kubernetes.io/managed-by, which must be "muster"`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// The premise: owned() really does reject this fixture. Without it a
			// case could assert a complaint about labels that are in fact fine.
			if owned(c.labels) {
				t.Fatalf("premise: owned(%v) is true, so this fixture never produces a refusal", c.labels)
			}
			if got := labelComplaint(c.labels); got != c.want {
				t.Errorf("labelComplaint(%v)\n got %q\nwant %q", c.labels, got, c.want)
			}
		})
	}

	// The disagreement detector: labelComplaint and owned() are derived from the
	// same managedLabels, so a fully-correct label set can only reach
	// labelComplaint if those two derivations have drifted apart. This asserts
	// the branch says so rather than printing an empty complaint.
	t.Run("a fully labelled object reports the disagreement", func(t *testing.T) {
		if !owned(managedLabels) {
			t.Fatal("premise: owned(managedLabels) must be true")
		}
		got := labelComplaint(managedLabels)
		if !strings.Contains(got, "disagree") {
			t.Errorf("labelComplaint over correct labels must name the disagreement rather than "+
				"printing an empty parenthesis; got %q", got)
		}
	})
}

// TestTheRefusalMessageCarriesTheComplaintToTheOperator is the behavioural half:
// labelComplaint being right is worth nothing if the message does not use it.
func TestTheRefusalMessageCarriesTheComplaintToTheOperator(t *testing.T) {
	ctx := context.Background()
	d, cs := internalDriver(t)
	cs.CoreV1().ServiceAccounts(internalNS).Create(ctx, //nolint:errcheck
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name: "half", Namespace: internalNS, Labels: map[string]string{
				"app.kubernetes.io/managed-by": "muster",
				"app.kubernetes.io/name":       "something-else",
			},
		}}, metav1.CreateOptions{})

	err := d.Create(ctx, internalSpec("half"))
	if err == nil {
		t.Fatal("Create overwrote a half-labelled ServiceAccount")
	}
	msg := err.Error()
	if !strings.Contains(msg, `its app.kubernetes.io/name is "something-else", not "muster-agent"`) {
		t.Errorf("the refusal does not name the label that is actually wrong: %q", msg)
	}
	// 🔴 AND IT MUST NOT NAME THE KEY THAT IS CORRECT. This is the whole finding:
	// the old message's only named remedy was already satisfied, so an operator
	// following it looped.
	if strings.Contains(msg, "does not carry app.kubernetes.io/managed-by") {
		t.Errorf("the refusal blames app.kubernetes.io/managed-by, which this object carries "+
			"correctly — the remedy it names is already done: %q", msg)
	}
}
