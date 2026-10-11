package k8s_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/provision/provisiontest"
)

// This file is about ONE property: a name is not an identity. Every case here
// seeds an object that muster did not create, under a name an instance asks
// for, and asserts the driver neither reports it, nor writes to it, nor deletes
// it.
//
// 🔴 IT IS ONLY FULLY REACHABLE UNDER THE SHARED-NAMESPACE LAYOUT, which is why
// the harness had to be parameterised first. With a namespace per instance the
// collisions are rarer; in a namespace the operator also uses, every workload
// in it is a candidate.

// foreignLabels are what somebody ELSE's objects carry: a plausible set that
// does not include muster's pair. `app.kubernetes.io/name` is present on
// purpose with a different value, so a predicate that checked only for the
// KEY's existence would still pass this fixture.
func foreignLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "grafana",
		"app.kubernetes.io/managed-by": "Helm",
	}
}

// assertRefusal pins the SENTINELS of an ownership refusal, not its prose.
//
// 🔴 EVERY CASE IN THIS FILE USED TO ASSERT ONLY strings.Contains(err, "not
// managed by muster"), AND THAT IS WHY A WHOLE CLASS OF DEFECT WENT UNSEEN. A
// substring says nothing about what a caller BRANCHES on, so the five upserts
// in apply() could wrap a permanent refusal in provision.ErrBlind — "backend
// unreachable", a TRANSIENT fault — and every one of these tests stayed green:
// measured at 138d269, a Create over a foreign ServiceAccount gave
// errors.Is(err, ErrBlind) true, errors.Is(err, ErrNotFound) false and
// errors.As(err, **notManagedError) false. The prose was right the whole time.
//
// wantNotFound is the deliberate asymmetry: a by-name READ answers to
// ErrNotFound as well (a stranger's object is not this instance), while a WRITE
// or a TEARDOWN must NOT, because `!errors.Is(err, ErrNotFound)` is the idiom
// that would discard it.
func assertRefusal(t *testing.T, what string, err error, wantNotFound bool) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no error at all", what)
	}
	if !errors.Is(err, provision.ErrNotManaged) {
		t.Errorf("%s: must report provision.ErrNotManaged so a caller can branch on the refusal; got %v",
			what, err)
	}
	if errors.Is(err, provision.ErrBlind) {
		t.Errorf("%s: must NOT report provision.ErrBlind. This refusal is PERMANENT; ErrBlind means "+
			"'I could not reach the backend', so a caller retries forever and an alert pages for an "+
			"outage that is not happening. Got %v", what, err)
	}
	if got := errors.Is(err, provision.ErrNotFound); got != wantNotFound {
		if wantNotFound {
			t.Errorf("%s: a by-name READ must also report ErrNotFound — a stranger's co-named object "+
				"is not this instance; got %v", what, err)
		} else {
			t.Errorf("%s: must NOT report ErrNotFound. `if err != nil && !errors.Is(err, "+
				"provision.ErrNotFound)` is the idiom the contract invites, and it discards this "+
				"refusal in silence. Got %v", what, err)
		}
	}
	if !strings.Contains(err.Error(), "not managed by muster") {
		t.Errorf("%s: the refusal must say why it refused, got %q", what, err)
	}
}

// seedForeign puts a Deployment, a Service, a ConfigMap, a Secret and a
// ServiceAccount under `name` in `ns`, none of them muster's.
func seedForeign(t *testing.T, cs *fake.Clientset, ns, name string) {
	t.Helper()
	ctx := context.Background()
	meta := func(n string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: n, Namespace: ns, Labels: foreignLabels()}
	}
	replicas := int32(2)
	if _, err := cs.AppsV1().Deployments(ns).Create(ctx, &appsv1.Deployment{
		ObjectMeta: meta(name),
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed deployment: %v", err)
	}
	if _, err := cs.CoreV1().Services(ns).Create(ctx, &corev1.Service{
		ObjectMeta: meta(name),
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": name}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed service: %v", err)
	}
	if _, err := cs.CoreV1().ConfigMaps(ns).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: meta(name + "-files"),
		BinaryData: map[string][]byte{"dashboard.json": []byte(`{"panels":[]}`)},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed configmap: %v", err)
	}
	if _, err := cs.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
		ObjectMeta: meta(name + "-env"),
		Data:       map[string][]byte{"ADMIN_PASSWORD": []byte("someone-elses-value-70413")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	if _, err := cs.CoreV1().ServiceAccounts(ns).Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: meta(name),
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed serviceaccount: %v", err)
	}
}

// assertForeignSurvives reads back every object seedForeign created.
func assertForeignSurvives(t *testing.T, cs *fake.Clientset, ns, name string) {
	t.Helper()
	ctx := context.Background()
	if _, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Errorf("a Deployment muster did not create was removed: %v", err)
	}
	if _, err := cs.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Errorf("a Service muster did not create was removed: %v", err)
	}
	if _, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, name+"-files", metav1.GetOptions{}); err != nil {
		t.Errorf("a ConfigMap muster did not create was removed: %v", err)
	}
	if _, err := cs.CoreV1().Secrets(ns).Get(ctx, name+"-env", metav1.GetOptions{}); err != nil {
		t.Errorf("a Secret muster did not create was removed: %v", err)
	}
	if _, err := cs.CoreV1().ServiceAccounts(ns).Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Errorf("a ServiceAccount muster did not create was removed: %v", err)
	}
}

// TestDestroyLeavesWorkloadsItDidNotCreateAlone.
//
// 🔴 THE MEASURED DEFECT: Destroy deleted by NAME. Destroy(Ref{Name:"grafana"})
// in a shared namespace removed a Service, a ConfigMap and a ServiceAccount
// belonging to somebody else, and returned nil — a successful teardown of an
// instance that never existed.
func TestDestroyLeavesWorkloadsItDidNotCreateAlone(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		ctx := context.Background()
		d, cs := newDriver(t, m, nil)
		ns := m.ns("grafana")
		seedForeign(t, cs, ns, "grafana")

		err := d.Destroy(ctx, provision.Ref{Name: "grafana"})
		if err == nil {
			t.Fatal("Destroy reported success for a workload muster did not create")
		}
		assertRefusal(t, "Destroy over a foreign Deployment", err, false)
		assertForeignSurvives(t, cs, ns, "grafana")

		// 🔴 THE CONTROL THAT MAKES THE REFUSAL ABOUT OWNERSHIP RATHER THAN
		// ABOUT DESTROY BEING BROKEN: an instance muster DID create, in the same
		// namespace, is destroyed normally.
		spec := provisiontest.MinimalSpec("ours")
		if err := d.Create(ctx, spec); err != nil {
			t.Fatalf("control Create: %v", err)
		}
		if err := d.Destroy(ctx, spec.Ref); err != nil {
			t.Fatalf("control: Destroy of muster's own instance must succeed, got %v", err)
		}
		if _, err := cs.AppsV1().Deployments(ns).Get(ctx, "ours", metav1.GetOptions{}); err == nil {
			t.Error("control: muster's own Deployment survived its Destroy")
		}
		// And the foreign objects are STILL there after a successful destroy
		// next door.
		assertForeignSurvives(t, cs, ns, "grafana")
	})
}

// TestDestroyRefusalSurvivesTheIdiomTheContractInvites pins the CONSEQUENCE of
// the sentinel choice, not the sentinel.
//
// 🔴 A LOUD REFUSAL NOBODY HEARS IS NOT LOUD. Destroy's contract says nil means
// removed or already absent, so the natural caller is `if err != nil &&
// !errors.Is(err, provision.ErrNotFound)`. Measured at 138d269, Destroy's
// refusal over a foreign Deployment satisfied errors.Is(err, ErrNotFound) —
// so that idiom DISCARDED it, in silence, and the entire point of refusing
// loudly was defeated by the caller the contract itself suggests. This case
// runs the idiom rather than asserting the sentinel, so it stays true however
// the refusal is spelled.
func TestDestroyRefusalSurvivesTheIdiomTheContractInvites(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		ctx := context.Background()
		d, cs := newDriver(t, m, nil)
		ns := m.ns("grafana")
		seedForeign(t, cs, ns, "grafana")

		// The idiom, verbatim, as a caller would write it.
		report := func(err error) error {
			if err != nil && !errors.Is(err, provision.ErrNotFound) {
				return err
			}
			return nil
		}

		if report(d.Destroy(ctx, provision.Ref{Name: "grafana"})) == nil {
			t.Error("`if err != nil && !errors.Is(err, provision.ErrNotFound)` discarded Destroy's " +
				"refusal over somebody else's Deployment: the caller believes the teardown succeeded " +
				"and nothing reaches an operator")
		}

		// 🔴 THE CONTROL. The same idiom must STILL swallow a genuine
		// already-absent Destroy, or this case would pass for a driver whose
		// Destroy simply always errors.
		if err := report(d.Destroy(ctx, provision.Ref{Name: "never-existed"})); err != nil {
			t.Errorf("control: the idiom must swallow a genuinely absent instance — that is what it is "+
				"for; got %v", err)
		}
		// And muster's own instance is destroyed with no error at all.
		spec := provisiontest.MinimalSpec("ours")
		if err := d.Create(ctx, spec); err != nil {
			t.Fatalf("control Create: %v", err)
		}
		if err := report(d.Destroy(ctx, spec.Ref)); err != nil {
			t.Errorf("control: Destroy of muster's own instance must be nil, got %v", err)
		}
	})
}

// TestDestroyLeavesCoNamedObjectsOfOtherKindsAlone is the case that REACHES
// the per-object ownership check. When the Deployment is muster's, the teardown
// proceeds — and every other object it deletes is named after the instance, so
// a ConfigMap, Secret, Service or claim somebody else put under those names is
// in the blast radius. This spec asks for none of them, so every co-named
// object here belongs to somebody else.
func TestDestroyLeavesCoNamedObjectsOfOtherKindsAlone(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		ctx := context.Background()
		d, cs := newDriver(t, m, nil)
		ns := m.ns("ours")

		spec := provisiontest.MinimalSpec("ours")
		spec.Ports = nil // no Service, no files, no secrets: muster renders none of them
		if err := d.Create(ctx, spec); err != nil {
			t.Fatalf("Create: %v", err)
		}
		meta := func(n string) metav1.ObjectMeta {
			return metav1.ObjectMeta{Name: n, Namespace: ns, Labels: foreignLabels()}
		}
		if _, err := cs.CoreV1().ConfigMaps(ns).Create(ctx, &corev1.ConfigMap{
			ObjectMeta: meta("ours-files"),
			BinaryData: map[string][]byte{"theirs.json": []byte("{}")},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed configmap: %v", err)
		}
		if _, err := cs.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
			ObjectMeta: meta("ours-env"),
			Data:       map[string][]byte{"THEIR_TOKEN": []byte("someone-elses-value-70413")},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed secret: %v", err)
		}
		if _, err := cs.CoreV1().Services(ns).Create(ctx, &corev1.Service{
			ObjectMeta: meta("ours"),
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed service: %v", err)
		}
		if _, err := cs.CoreV1().PersistentVolumeClaims(ns).Create(ctx, &corev1.PersistentVolumeClaim{
			ObjectMeta: meta("ours-workspace"),
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed pvc: %v", err)
		}

		if err := d.Destroy(ctx, spec.Ref); err != nil {
			t.Fatalf("Destroy of muster's own instance: %v", err)
		}
		// Control: muster's own objects went.
		if _, err := cs.AppsV1().Deployments(ns).Get(ctx, "ours", metav1.GetOptions{}); err == nil {
			t.Error("control: muster's Deployment survived its own Destroy")
		}
		if _, err := cs.CoreV1().ServiceAccounts(ns).Get(ctx, "ours", metav1.GetOptions{}); err == nil {
			t.Error("control: muster's ServiceAccount survived its own Destroy")
		}
		// And nobody else's did.
		if _, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, "ours-files", metav1.GetOptions{}); err != nil {
			t.Errorf("a ConfigMap muster never created was deleted by name: %v", err)
		}
		if _, err := cs.CoreV1().Secrets(ns).Get(ctx, "ours-env", metav1.GetOptions{}); err != nil {
			t.Errorf("a Secret muster never created was deleted by name: %v", err)
		}
		if _, err := cs.CoreV1().Services(ns).Get(ctx, "ours", metav1.GetOptions{}); err != nil {
			t.Errorf("a Service muster never created was deleted by name: %v", err)
		}
		if _, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, "ours-workspace", metav1.GetOptions{}); err != nil {
			t.Errorf("a PersistentVolumeClaim muster never created was deleted by name — and a claim is "+
				"somebody's data: %v", err)
		}
	})
}

// TestApplyRefusesToWriteOverACoNamedObjectOfAnotherKind reaches the ownership
// check on the WRITE path. There is no Deployment in the way here, so Create
// runs the whole of apply, and the first object it writes — the ServiceAccount
// — is one somebody else already owns.
func TestApplyRefusesToWriteOverACoNamedObjectOfAnotherKind(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		ctx := context.Background()

		// ⚠ THE SERVICEACCOUNT IS THE FIRST OBJECT apply WRITES, so a fixture
		// where it collides can never reach the checks on the objects after it.
		// Each kind below therefore gets its OWN fixture, colliding on that
		// kind alone. A mutation run is what showed this: with one combined
		// fixture, disabling the Service check killed nothing.
		t.Run("serviceaccount", func(t *testing.T) {
			d, cs := newDriver(t, m, nil)
			ns := m.ns("collide")
			if _, err := cs.CoreV1().ServiceAccounts(ns).Create(ctx, &corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{Name: "collide", Namespace: ns, Labels: foreignLabels()},
			}, metav1.CreateOptions{}); err != nil {
				t.Fatalf("seed serviceaccount: %v", err)
			}

			err := d.Create(ctx, provisiontest.MinimalSpec("collide"))
			if err == nil {
				t.Fatal("Create overwrote a ServiceAccount muster did not create")
			}
			assertRefusal(t, "Create over a foreign ServiceAccount", err, false)
			sa, gerr := cs.CoreV1().ServiceAccounts(ns).Get(ctx, "collide", metav1.GetOptions{})
			if gerr != nil {
				t.Fatalf("get serviceaccount: %v", gerr)
			}
			if sa.Labels["app.kubernetes.io/managed-by"] != "Helm" {
				t.Errorf("the foreign ServiceAccount was overwritten; labels are now %v", sa.Labels)
			}
			// A ServiceAccount is an identity: taking it over is taking over
			// whatever it can do. The Deployment must not exist either.
			if _, err := cs.AppsV1().Deployments(ns).Get(ctx, "collide", metav1.GetOptions{}); err == nil {
				t.Error("the refused Create still produced a Deployment")
			}
		})

		// 🔴 THE THREE KINDS BELOW WERE PINNED BY NOTHING. An isolated mutation
		// of each call site's label read to `return managedLabels, nil` —
		// configmap, both secrets, and the stale-service sweep — SURVIVED the
		// whole suite. So an Update could silently overwrite a stranger's Secret
		// named <instance>-env, or delete their co-named ConfigMap, with the
		// suite green. The methodology stated above was applied to two of the
		// five kinds; these are the rest.
		//
		// ⚠ EACH ONE'S SPEC ASKS FOR THAT OBJECT, which is what makes apply take
		// the UPSERT branch rather than the stale-sweep branch. The sweep half of
		// the same closure is pinned separately, in
		// TestApplyLeavesForeignCoNamedObjectsTheSpecDroppedAlone — one mutation
		// neutralises both, so both need a case.
		t.Run("configmap", func(t *testing.T) {
			d, cs := newDriver(t, m, nil)
			ns := m.ns("filer")
			if _, err := cs.CoreV1().ConfigMaps(ns).Create(ctx, &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "filer-files", Namespace: ns, Labels: foreignLabels()},
				BinaryData: map[string][]byte{"their-dashboard.json": []byte(`{"panels":[1]}`)},
			}, metav1.CreateOptions{}); err != nil {
				t.Fatalf("seed configmap: %v", err)
			}

			spec := provisiontest.MinimalSpec("filer")
			spec.Files = []provision.File{{Path: "/etc/muster/agent.json", Content: []byte(`{"tools":["tasks"]}`)}}
			err := d.Create(ctx, spec)
			if err == nil {
				t.Fatal("Create overwrote a ConfigMap muster did not create")
			}
			assertRefusal(t, "Create over a foreign ConfigMap", err, false)

			cm, gerr := cs.CoreV1().ConfigMaps(ns).Get(ctx, "filer-files", metav1.GetOptions{})
			if gerr != nil {
				t.Fatalf("get configmap: %v", gerr)
			}
			if cm.Labels["app.kubernetes.io/managed-by"] != "Helm" {
				t.Errorf("the foreign ConfigMap was overwritten; labels are now %v", cm.Labels)
			}
			// 🔴 THE CONSEQUENCE, NOT JUST THE LABELS: an Update writes the whole
			// object, so their keys are REPLACED by muster's — the file they
			// mount disappears from a running pod.
			if _, ok := cm.BinaryData["their-dashboard.json"]; !ok {
				t.Errorf("the foreign ConfigMap's content was replaced; keys are now %v",
					keysOf(cm.BinaryData))
			}
			if _, err := cs.AppsV1().Deployments(ns).Get(ctx, "filer", metav1.GetOptions{}); err == nil {
				t.Error("the refused Create still produced a Deployment")
			}

			// Control: muster's OWN ConfigMap is updated in place, so the refusal
			// is about ownership and not about the object already existing.
			clean := provisiontest.MinimalSpec("clean")
			clean.Files = []provision.File{{Path: "/etc/muster/agent.json", Content: []byte(`{"tools":["tasks"]}`)}}
			if err := d.Create(ctx, clean); err != nil {
				t.Fatalf("control Create: %v", err)
			}
			clean.Files = []provision.File{{Path: "/etc/muster/agent.json", Content: []byte(`{"tools":["tasks","runbooks"]}`)}}
			if err := d.Update(ctx, clean); err != nil {
				t.Fatalf("control: updating muster's own ConfigMap must succeed, got %v", err)
			}
			ours, gerr := cs.CoreV1().ConfigMaps(m.ns("clean")).Get(ctx, "clean-files", metav1.GetOptions{})
			if gerr != nil {
				t.Fatalf("control get configmap: %v", gerr)
			}
			var got []byte
			for _, v := range ours.BinaryData {
				got = v
			}
			if string(got) != `{"tools":["tasks","runbooks"]}` {
				t.Errorf("control: muster's own ConfigMap was not updated, content is %q", got)
			}
		})

		// The two Secrets share ONE closure in apply's loop, so one mutation
		// neutralises both — but they hold different things and are consumed
		// differently (envFrom versus a mount), so each gets its own fixture.
		t.Run("env secret", func(t *testing.T) {
			d, cs := newDriver(t, m, nil)
			ns := m.ns("enver")
			if _, err := cs.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "enver-env", Namespace: ns, Labels: foreignLabels()},
				Data:       map[string][]byte{"ADMIN_PASSWORD": []byte("someone-elses-value-70413")},
			}, metav1.CreateOptions{}); err != nil {
				t.Fatalf("seed secret: %v", err)
			}

			spec := provisiontest.MinimalSpec("enver")
			spec.Secrets = []provision.EnvVar{{Name: "MUSTER_CALLBACK_TOKEN", Value: "musters-value-51199"}}
			err := d.Create(ctx, spec)
			if err == nil {
				t.Fatal("Create overwrote a Secret muster did not create")
			}
			assertRefusal(t, "Create over a foreign env Secret", err, false)

			sec, gerr := cs.CoreV1().Secrets(ns).Get(ctx, "enver-env", metav1.GetOptions{})
			if gerr != nil {
				t.Fatalf("get secret: %v", gerr)
			}
			if sec.Labels["app.kubernetes.io/managed-by"] != "Helm" {
				t.Errorf("the foreign Secret was overwritten; labels are now %v", sec.Labels)
			}
			// 🔴 THE CONSEQUENCE. This object is consumed with envFrom, so
			// replacing its keys silently unsets every environment variable
			// somebody else's pod reads from it.
			if string(sec.Data["ADMIN_PASSWORD"]) != "someone-elses-value-70413" {
				t.Errorf("the foreign Secret's content was replaced; keys are now %v", keysOf(sec.Data))
			}
			if _, err := cs.AppsV1().Deployments(ns).Get(ctx, "enver", metav1.GetOptions{}); err == nil {
				t.Error("the refused Create still produced a Deployment")
			}

			// Control: muster's own env Secret is updated in place.
			clean := provisiontest.MinimalSpec("clean")
			clean.Secrets = []provision.EnvVar{{Name: "MUSTER_CALLBACK_TOKEN", Value: "first-value-30517"}}
			if err := d.Create(ctx, clean); err != nil {
				t.Fatalf("control Create: %v", err)
			}
			clean.Secrets = []provision.EnvVar{{Name: "MUSTER_CALLBACK_TOKEN", Value: "rotated-value-88231"}}
			if err := d.Update(ctx, clean); err != nil {
				t.Fatalf("control: updating muster's own Secret must succeed, got %v", err)
			}
			ours, gerr := cs.CoreV1().Secrets(m.ns("clean")).Get(ctx, "clean-env", metav1.GetOptions{})
			if gerr != nil {
				t.Fatalf("control get secret: %v", gerr)
			}
			if string(ours.Data["MUSTER_CALLBACK_TOKEN"]) != "rotated-value-88231" {
				t.Errorf("control: muster's own Secret was not updated; a rotation that does not land "+
					"is a revocation that is a comment. Got %q", ours.Data["MUSTER_CALLBACK_TOKEN"])
			}
		})

		t.Run("file secret", func(t *testing.T) {
			d, cs := newDriver(t, m, nil)
			ns := m.ns("filesec")
			if _, err := cs.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "filesec-secret-files", Namespace: ns, Labels: foreignLabels()},
				Data:       map[string][]byte{"their-tls.key": []byte("someone-elses-key-64907")},
			}, metav1.CreateOptions{}); err != nil {
				t.Fatalf("seed secret: %v", err)
			}

			spec := provisiontest.MinimalSpec("filesec")
			spec.Files = []provision.File{{Path: "/etc/muster/token", Content: []byte("musters-file-30517"), Secret: true}}
			err := d.Create(ctx, spec)
			if err == nil {
				t.Fatal("Create overwrote a Secret muster did not create")
			}
			assertRefusal(t, "Create over a foreign file Secret", err, false)

			sec, gerr := cs.CoreV1().Secrets(ns).Get(ctx, "filesec-secret-files", metav1.GetOptions{})
			if gerr != nil {
				t.Fatalf("get secret: %v", gerr)
			}
			if sec.Labels["app.kubernetes.io/managed-by"] != "Helm" {
				t.Errorf("the foreign Secret was overwritten; labels are now %v", sec.Labels)
			}
			if string(sec.Data["their-tls.key"]) != "someone-elses-key-64907" {
				t.Errorf("the foreign Secret's content was replaced; keys are now %v", keysOf(sec.Data))
			}
			if _, err := cs.AppsV1().Deployments(ns).Get(ctx, "filesec", metav1.GetOptions{}); err == nil {
				t.Error("the refused Create still produced a Deployment")
			}

			// Control: muster's own file Secret is updated in place.
			clean := provisiontest.MinimalSpec("clean")
			clean.Files = []provision.File{{Path: "/etc/muster/token", Content: []byte("first-file-11903"), Secret: true}}
			if err := d.Create(ctx, clean); err != nil {
				t.Fatalf("control Create: %v", err)
			}
			clean.Files = []provision.File{{Path: "/etc/muster/token", Content: []byte("rotated-file-74128"), Secret: true}}
			if err := d.Update(ctx, clean); err != nil {
				t.Fatalf("control: updating muster's own Secret must succeed, got %v", err)
			}
			ours, gerr := cs.CoreV1().Secrets(m.ns("clean")).Get(ctx, "clean-secret-files", metav1.GetOptions{})
			if gerr != nil {
				t.Fatalf("control get secret: %v", gerr)
			}
			var got []byte
			for _, v := range ours.Data {
				got = v
			}
			if string(got) != "rotated-file-74128" {
				t.Errorf("control: muster's own file Secret was not updated, content is %q", got)
			}
		})

		// The Service has its own upsert, because a Service's ClusterIP is
		// assigned by the API server and has to be carried over — so it has its
		// own copy of the ownership check, and its own way to be wrong.
		t.Run("service", func(t *testing.T) {
			d, cs := newDriver(t, m, nil)
			ns := m.ns("porty")
			if _, err := cs.CoreV1().Services(ns).Create(ctx, &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "porty", Namespace: ns, Labels: foreignLabels()},
				Spec: corev1.ServiceSpec{
					ClusterIP: "203.0.113.9",
					Selector:  map[string]string{"app": "porty"},
					Ports:     []corev1.ServicePort{{Name: "web", Port: 80}},
				},
			}, metav1.CreateOptions{}); err != nil {
				t.Fatalf("seed service: %v", err)
			}

			err := d.Create(ctx, provisiontest.MinimalSpec("porty"))
			if err == nil {
				t.Fatal("Create overwrote a Service muster did not create")
			}
			assertRefusal(t, "Create over a foreign Service", err, false)
			svc, gerr := cs.CoreV1().Services(ns).Get(ctx, "porty", metav1.GetOptions{})
			if gerr != nil {
				t.Fatalf("get service: %v", gerr)
			}
			if svc.Labels["app.kubernetes.io/managed-by"] != "Helm" {
				t.Errorf("the foreign Service was overwritten; labels are now %v", svc.Labels)
			}
			// 🔴 THE CONSEQUENCE, NOT JUST THE LABELS: rewriting this object's
			// selector and ports silently redirects somebody else's traffic to
			// muster's pods.
			if svc.Spec.Selector["app"] != "porty" || len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 80 {
				t.Errorf("the foreign Service's spec was rewritten: selector=%v ports=%+v",
					svc.Spec.Selector, svc.Spec.Ports)
			}

			// Control: muster's OWN Service is updated in place, ClusterIP and
			// all, so the refusal is about ownership rather than about the
			// Service already existing.
			clean := provisiontest.MinimalSpec("clean")
			if err := d.Create(ctx, clean); err != nil {
				t.Fatalf("control Create: %v", err)
			}
			clean.Ports = []provision.Port{{Name: provision.DefaultPortName, Port: 8421}, {Name: "metrics", Port: 9102}}
			if err := d.Update(ctx, clean); err != nil {
				t.Fatalf("control: updating muster's own Service must succeed, got %v", err)
			}
			ours, gerr := cs.CoreV1().Services(m.ns("clean")).Get(ctx, "clean", metav1.GetOptions{})
			if gerr != nil {
				t.Fatalf("control get service: %v", gerr)
			}
			if len(ours.Spec.Ports) != 2 {
				t.Errorf("control: muster's own Service was not updated: %+v", ours.Spec.Ports)
			}
		})
	})
}

// TestApplyRefusesAForeignWorkspaceClaim is the same check on the PVC, where
// the consequence is different in kind: an existing claim is create-only, so
// without the check a claim somebody else made under this name would simply be
// MOUNTED into the agent's pod, handing it their data.
func TestApplyRefusesAForeignWorkspaceClaim(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		ctx := context.Background()
		d, cs := newDriver(t, m, nil)
		ns := m.ns("claimer")

		if _, err := cs.CoreV1().PersistentVolumeClaims(ns).Create(ctx, &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "claimer-workspace", Namespace: ns, Labels: foreignLabels()},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed pvc: %v", err)
		}

		spec := provisiontest.MinimalSpec("claimer")
		spec.Workspace = provision.Workspace{Path: "/data", Size: "5Gi", Persist: true}
		err := d.Create(ctx, spec)
		if err == nil {
			t.Fatal("Create mounted a PersistentVolumeClaim muster did not create")
		}
		// 🔴 NOT ErrNotFound. A Create returning the sentinel that means "this
		// instance does not exist" is indistinguishable from the caller's own
		// precondition in a Get -> ErrNotFound -> Create loop, so the loop
		// retries the refused Create forever.
		assertRefusal(t, "Create over a foreign workspace claim", err, false)
		if _, err := cs.AppsV1().Deployments(ns).Get(ctx, "claimer", metav1.GetOptions{}); err == nil {
			t.Error("the refused Create still produced a Deployment that would mount the foreign claim")
		}

		// Control: muster's OWN claim is reused without complaint, so the
		// refusal is about ownership and not about the claim already existing.
		if err := d.Create(ctx, provisiontest.MinimalSpec("clean")); err != nil {
			t.Fatalf("control Create: %v", err)
		}
		clean := provisiontest.MinimalSpec("clean")
		clean.Workspace = provision.Workspace{Path: "/data", Size: "5Gi", Persist: true}
		if err := d.Update(ctx, clean); err != nil {
			t.Fatalf("control: adding a workspace to muster's own instance: %v", err)
		}
		if err := d.Update(ctx, clean); err != nil {
			t.Fatalf("control: a second apply over muster's OWN existing claim must succeed, got %v", err)
		}
	})
}

// TestReadPathsRefuseAWorkloadTheyDidNotCreate covers every by-name read, not
// just Get: each one fetches a Deployment by name, and a foreign workload
// answering to that name is reported as an instance — with a phase, a pod and a
// restart count belonging to a stranger. Scale is the one that also WRITES.
func TestReadPathsRefuseAWorkloadTheyDidNotCreate(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		ctx := context.Background()
		d, cs := newDriver(t, m, nil)
		ns := m.ns("grafana")
		seedForeign(t, cs, ns, "grafana")
		ref := provision.Ref{Name: "grafana"}

		t.Run("Get", func(t *testing.T) {
			inst, err := d.Get(ctx, ref)
			if !errors.Is(err, provision.ErrNotFound) {
				t.Fatalf("Get of a foreign workload must be ErrNotFound, got instance %+v err %v", inst, err)
			}
			assertRefusal(t, "Get of a foreign workload", err, true)
		})

		t.Run("Scale", func(t *testing.T) {
			err := d.Scale(ctx, ref, 0)
			assertRefusal(t, "Scale of a foreign workload", err, true)
			// 🔴 THE STATE, NOT JUST THE ERROR. Scaling somebody else's
			// Deployment to zero is an outage nobody would attribute to muster.
			dep, gerr := cs.AppsV1().Deployments(ns).Get(ctx, "grafana", metav1.GetOptions{})
			if gerr != nil {
				t.Fatalf("get seeded deployment: %v", gerr)
			}
			if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 2 {
				t.Errorf("the foreign Deployment's replicas are %v, want the 2 it was seeded with",
					dep.Spec.Replicas)
			}
		})

		t.Run("Endpoint", func(t *testing.T) {
			_, err := d.Endpoint(ctx, ref)
			assertRefusal(t, "Endpoint of a foreign workload", err, true)
		})

		t.Run("TailLogs", func(t *testing.T) {
			_, err := d.TailLogs(ctx, ref, 10)
			assertRefusal(t, "TailLogs of a foreign workload — its output is not muster's to serve",
				err, true)
		})

		t.Run("List", func(t *testing.T) {
			spec := provisiontest.MinimalSpec("ours")
			if err := d.Create(ctx, spec); err != nil {
				t.Fatalf("Create: %v", err)
			}
			list, err := d.List(ctx)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			// The server-side selector and the client-side predicate must agree
			// about the same object set.
			if len(list) != 1 || list[0].Ref.Name != "ours" {
				t.Fatalf("List reported %+v; want exactly the one instance muster created", list)
			}
			// Control for the whole subtest group: the driver DOES answer for
			// its own instance on every path that just refused.
			if _, err := d.Get(ctx, spec.Ref); err != nil {
				t.Errorf("control: Get of muster's own instance: %v", err)
			}
			if err := d.Scale(ctx, spec.Ref, 2); err != nil {
				t.Errorf("control: Scale of muster's own instance: %v", err)
			}
			if _, err := d.Endpoint(ctx, spec.Ref); err != nil {
				t.Errorf("control: Endpoint of muster's own instance: %v", err)
			}
			if _, err := d.TailLogs(ctx, spec.Ref, 10); err != nil {
				t.Errorf("control: TailLogs of muster's own instance: %v", err)
			}
		})
	})
}

// TestCreateAndUpdateRefuseToAdoptAForeignWorkload. Create's refusal keeps the
// divergence sentinel — the caller's branch is unchanged, "refused, nothing
// written" — while Update, which creates what is ABSENT, must not create over
// what is FOREIGN.
func TestCreateAndUpdateRefuseToAdoptAForeignWorkload(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		ctx := context.Background()
		ns := m.ns("grafana")
		spec := provisiontest.MinimalSpec("grafana")

		t.Run("Create", func(t *testing.T) {
			d, cs := newDriver(t, m, nil)
			seedForeign(t, cs, ns, "grafana")
			err := d.Create(ctx, spec)
			if !errors.Is(err, provision.ErrDivergentSpec) {
				t.Fatalf("Create over a foreign workload must be refused with ErrDivergentSpec, got %v", err)
			}
			if !strings.Contains(err.Error(), "not managed by muster") {
				t.Errorf("the refusal must name the reason rather than reporting a missing fingerprint, got %q", err)
			}
			// ⚠ THIS ONE IS DELIBERATELY NOT assertRefusal. Create's refusal keeps
			// ErrDivergentSpec — "refused, nothing written" is the branch its
			// caller already has — so it is the one ownership refusal that does
			// not report ErrNotManaged. It must still not be ErrBlind or
			// ErrNotFound.
			if errors.Is(err, provision.ErrBlind) || errors.Is(err, provision.ErrNotFound) {
				t.Errorf("Create's divergence refusal must be neither ErrBlind nor ErrNotFound, got %v", err)
			}
			// 🔴 AND THE SENTENCE ABOVE IS NOW ASSERTED RATHER THAN STATED. It
			// said "the one ownership refusal that does not report ErrNotManaged"
			// and nothing checked it, so a change that added the sentinel here —
			// making Create's refusal claim to be both a divergence and an
			// ownership refusal, which is what provision.go's ErrNotManaged doc
			// wrongly described — would have survived a green suite. The
			// asymmetry is a documented decision; this is what pins it.
			if errors.Is(err, provision.ErrNotManaged) {
				t.Errorf("Create's refusal over a foreign workload must report ErrDivergentSpec ALONE. "+
					"Reporting ErrNotManaged as well makes one error claim to be two different "+
					"answers, and provision.ErrNotManaged's doc names Create as an exception for "+
					"exactly this reason. Got %v", err)
			}
			// The message must name the label that is wrong, because relabelling
			// is the only documented escape from an ownership refusal.
			if !strings.Contains(err.Error(), "app.kubernetes.io/managed-by") {
				t.Errorf("the refusal does not name which label is wrong, so its remedy is unstated: %q", err)
			}
			assertForeignSurvives(t, cs, ns, "grafana")
			assertForeignUntouched(t, cs, ns, "grafana")
		})

		t.Run("Update", func(t *testing.T) {
			d, cs := newDriver(t, m, nil)
			seedForeign(t, cs, ns, "grafana")
			err := d.Update(ctx, spec)
			if err == nil {
				t.Fatal("Update took over a workload muster did not create")
			}
			// NOT ErrNotFound: Update's documented answer to an ABSENT instance
			// is to CREATE it, so a refusal wearing that sentinel tells a caller
			// to do the very thing that was just refused.
			assertRefusal(t, "Update over a foreign release", err, false)
			assertForeignSurvives(t, cs, ns, "grafana")
			assertForeignUntouched(t, cs, ns, "grafana")
		})

		// 🔴 THE CASE THAT REACHES UPDATE'S OWN PRE-FLIGHT CHECK. With a whole
		// foreign release in the way, the first object apply writes already
		// collides and refuses — so that fixture cannot tell a pre-flight check
		// from a per-object one. Here ONLY the Deployment is foreign: apply
		// would create muster's ServiceAccount and Service first and refuse at
		// the very last object, leaving muster-labelled leftovers scattered
		// over somebody else's release.
		t.Run("Update with only the Deployment in the way", func(t *testing.T) {
			d, cs := newDriver(t, m, nil)
			replicas := int32(2)
			if _, err := cs.AppsV1().Deployments(ns).Create(ctx, &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "grafana", Namespace: ns, Labels: foreignLabels()},
				Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
			}, metav1.CreateOptions{}); err != nil {
				t.Fatalf("seed deployment: %v", err)
			}
			err := d.Update(ctx, spec)
			if err == nil {
				t.Fatal("Update took over a workload muster did not create")
			}
			assertRefusal(t, "Update with only the Deployment in the way", err, false)
			if _, gerr := cs.CoreV1().ServiceAccounts(ns).Get(ctx, "grafana", metav1.GetOptions{}); gerr == nil {
				t.Error("the refused Update created a ServiceAccount next to somebody else's Deployment")
			}
			if _, gerr := cs.CoreV1().Services(ns).Get(ctx, "grafana", metav1.GetOptions{}); gerr == nil {
				t.Error("the refused Update created a Service whose selector points at muster's pods")
			}
			dep, gerr := cs.AppsV1().Deployments(ns).Get(ctx, "grafana", metav1.GetOptions{})
			if gerr != nil {
				t.Fatalf("get seeded deployment: %v", gerr)
			}
			if dep.Labels["app.kubernetes.io/managed-by"] != "Helm" {
				t.Errorf("the foreign Deployment was overwritten; labels are now %v", dep.Labels)
			}
		})
	})
}

// assertForeignUntouched checks that the seeded objects still carry the
// foreign labels — i.e. that nothing overwrote them with muster's.
func assertForeignUntouched(t *testing.T, cs *fake.Clientset, ns, name string) {
	t.Helper()
	ctx := context.Background()
	sa, err := cs.CoreV1().ServiceAccounts(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get serviceaccount: %v", err)
	}
	if sa.Labels["app.kubernetes.io/managed-by"] != "Helm" {
		t.Errorf("the foreign ServiceAccount was overwritten; its labels are now %v", sa.Labels)
	}
	svc, err := cs.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get service: %v", err)
	}
	if svc.Labels["app.kubernetes.io/managed-by"] != "Helm" {
		t.Errorf("the foreign Service was overwritten; its labels are now %v", svc.Labels)
	}
	if svc.Spec.Selector["app"] != name {
		t.Errorf("the foreign Service's selector was rewritten to %v; its traffic now goes to muster's pods",
			svc.Spec.Selector)
	}
	cm, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, name+"-files", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get configmap: %v", err)
	}
	if _, ok := cm.BinaryData["dashboard.json"]; !ok {
		t.Errorf("the foreign ConfigMap's content was replaced; keys are now %v", keysOf(cm.BinaryData))
	}
	sec, err := cs.CoreV1().Secrets(ns).Get(ctx, name+"-env", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if string(sec.Data["ADMIN_PASSWORD"]) != "someone-elses-value-70413" {
		t.Errorf("the foreign Secret's content was replaced; keys are now %v", keysOf(sec.Data))
	}
}

// TestCreateDoesNotAdoptANamespaceItDidNotCreate.
//
// 🔴 MEASURED: a pre-existing namespace was adopted on AlreadyExists with no
// check, and Destroy deletes the namespace WHOLE — so adopting somebody's
// namespace schedules the deletion of everything in it.
//
// ⚠ PER-INSTANCE LAYOUT ONLY, because it is the only one that creates or
// deletes a namespace at all. The shared-namespace half of the same property —
// that the driver creates NO namespace there — is asserted in
// TestRendersTheMinimalObjectSet.
func TestCreateDoesNotAdoptANamespaceItDidNotCreate(t *testing.T) {
	perInstance := nsModes[0]
	ctx := context.Background()
	d, cs := newDriver(t, perInstance, nil)

	const ns = "muster-monitoring"
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: foreignLabels()},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed namespace: %v", err)
	}
	if _, err := cs.CoreV1().ConfigMaps(ns).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "alert-rules", Namespace: ns, Labels: foreignLabels()},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed configmap: %v", err)
	}

	err := d.Create(ctx, provisiontest.MinimalSpec("monitoring"))
	if err == nil {
		t.Fatal("Create adopted a namespace muster did not create")
	}
	if !strings.Contains(err.Error(), "refusing to adopt") {
		t.Errorf("the refusal must say what it refused, got %q", err)
	}
	assertRefusal(t, "Create into a foreign namespace", err, false)
	// The refusal happens BEFORE anything is written into that namespace.
	if _, gerr := cs.CoreV1().ServiceAccounts(ns).Get(ctx, "monitoring", metav1.GetOptions{}); gerr == nil {
		t.Error("the refused Create wrote a ServiceAccount into the namespace anyway")
	}
	if _, gerr := cs.CoreV1().ConfigMaps(ns).Get(ctx, "alert-rules", metav1.GetOptions{}); gerr != nil {
		t.Errorf("the namespace's existing content was disturbed: %v", gerr)
	}

	// 🔴 CONTROL. A namespace muster creates itself is reused on the next
	// Create without complaint, so the refusal above is about ownership and not
	// about AlreadyExists.
	spec := provisiontest.MinimalSpec("ours")
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("control Create: %v", err)
	}
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("control: a second Create over muster's own namespace must be idempotent, got %v", err)
	}
}

// TestDestroyDoesNotDeleteANamespaceItDidNotCreate is the other half of the
// adoption defect: even reached with no Deployment in the way, the namespace
// delete is ownership-checked.
//
// 🔴 IT IS ALSO WHERE Destroy USED TO CONTRADICT ITSELF. This case asserted
// Destroy returned **nil** for a foreign namespace, while
// TestDestroyLeavesWorkloadsItDidNotCreateAlone asserted it returned an error
// for a foreign Deployment — two answers to one question, in one function, with
// nothing reconciling them. The nil was the wrong half: ensureNamespace REFUSES
// to create an instance in a namespace muster does not own, so a nil here
// claimed a clean teardown of a name that could never have been provisioned.
// Destroy is loud exactly where Create would have refused outright; see
// destroyNamespace for why a co-named ConfigMap or Secret is a different case.
func TestDestroyDoesNotDeleteANamespaceItDidNotCreate(t *testing.T) {
	perInstance := nsModes[0]
	ctx := context.Background()
	d, cs := newDriver(t, perInstance, nil)

	const ns = "muster-lonely"
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: foreignLabels()},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed namespace: %v", err)
	}

	err := d.Destroy(ctx, provision.Ref{Name: "lonely"})
	if err == nil {
		t.Fatal("Destroy reported success over a namespace muster did not create; the one refusal " +
			"Create makes about a namespace has to be the one Destroy makes about it too")
	}
	assertRefusal(t, "Destroy over a foreign namespace", err, false)
	if _, err := cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); err != nil {
		t.Fatalf("a namespace muster did not create was deleted by Destroy: %v", err)
	}

	// 🔴 THE CONTROL THAT KEEPS THIS FROM MEANING "Destroy ALWAYS ERRORS". A
	// name with no namespace at all is still "already absent", and still nil.
	if err := d.Destroy(ctx, provision.Ref{Name: "never-existed"}); err != nil {
		t.Fatalf("control: Destroy of a name nothing holds must be nil — already absent is the state "+
			"the caller asked for; got %v", err)
	}

	// 🔴 CONTROL. The namespace muster DID create is deleted, so the case above
	// is measuring the ownership check rather than a Destroy that deletes no
	// namespaces at all.
	spec := provisiontest.MinimalSpec("ours")
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("control Create: %v", err)
	}
	if err := d.Destroy(ctx, spec.Ref); err != nil {
		t.Fatalf("control Destroy: %v", err)
	}
	if _, err := cs.CoreV1().Namespaces().Get(ctx, "muster-ours", metav1.GetOptions{}); err == nil {
		t.Error("control: muster's own namespace survived its Destroy")
	}
}

// --------------------------------------------------------------------------
// The RBAC path: a name is not an identity there either
// --------------------------------------------------------------------------

// TestDestroyRefusesAForeignNamespaceBeforeRemovingAnything.
//
// 🔴 THE MEASURED INCOHERENCE: the namespace ownership check used to run at the
// END of Destroy, so with the namespace foreign and the instance's own objects
// muster's, Destroy #1, #2 and #3 all returned provision.ErrNotManaged AFTER
// removing the Deployment, the ServiceAccount and every satellite — with Get
// reporting ErrNotFound in between. A caller cannot tell that refusal from one
// that changed nothing, and "Destroy until nil" never terminates.
//
// Checked first, there are only two outcomes: muster owns the name and removes
// everything (nil), or it refuses and removes NOTHING. This case pins the second
// one, INCLUDING that the state is unchanged and that a second call says the
// same thing.
func TestDestroyRefusesAForeignNamespaceBeforeRemovingAnything(t *testing.T) {
	perInstance := nsModes[0]
	ctx := context.Background()
	d, cs := newDriver(t, perInstance, nil)
	const ns = "muster-relabelled"

	spec := provisiontest.MinimalSpec("relabelled")
	if err := d.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The instance is muster's; the namespace is relabelled out from under it,
	// which is the only way this state is reachable (ensureNamespace refuses to
	// create an instance in a namespace it does not own).
	nsObj, err := cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("premise: muster's own namespace must exist: %v", err)
	}
	nsObj.Labels = foreignLabels()
	if _, err := cs.CoreV1().Namespaces().Update(ctx, nsObj, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("relabel namespace: %v", err)
	}

	for pass := 1; pass <= 2; pass++ {
		err := d.Destroy(ctx, spec.Ref)
		assertRefusal(t, fmt.Sprintf("Destroy pass %d over a foreign namespace", pass), err, false)

		// 🔴 NOTHING WAS REMOVED. This is the half that distinguishes the fix
		// from the defect: the refusal and the teardown are never the same call.
		if _, gerr := cs.AppsV1().Deployments(ns).Get(ctx, "relabelled", metav1.GetOptions{}); gerr != nil {
			t.Fatalf("pass %d: Destroy removed the Deployment and THEN reported a permanent refusal, "+
				"which is the state a caller cannot converge out of: %v", pass, gerr)
		}
		if _, gerr := cs.CoreV1().ServiceAccounts(ns).Get(ctx, "relabelled", metav1.GetOptions{}); gerr != nil {
			t.Errorf("pass %d: the refused Destroy removed the ServiceAccount: %v", pass, gerr)
		}
		if _, gerr := cs.CoreV1().Services(ns).Get(ctx, "relabelled", metav1.GetOptions{}); gerr != nil {
			t.Errorf("pass %d: the refused Destroy removed the Service: %v", pass, gerr)
		}
		// And the instance is still THERE as far as every read is concerned, so
		// the refusal is consistent with what Get says.
		if _, gerr := d.Get(ctx, spec.Ref); gerr != nil {
			t.Errorf("pass %d: Destroy refused but Get no longer reports the instance (%v) — the two "+
				"disagree about whether it exists", pass, gerr)
		}
	}

	// 🔴 THE CONTROL. Put the labels back and the same Destroy completes, so the
	// refusal above is about the namespace's ownership and not about Destroy
	// being broken under this layout.
	nsObj, err = cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("control: get namespace: %v", err)
	}
	nsObj.Labels = map[string]string{
		"app.kubernetes.io/managed-by": "muster",
		"app.kubernetes.io/name":       "muster-agent",
		"app.kubernetes.io/instance":   "relabelled",
	}
	if _, err := cs.CoreV1().Namespaces().Update(ctx, nsObj, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("control: relabel namespace back: %v", err)
	}
	if err := d.Destroy(ctx, spec.Ref); err != nil {
		t.Fatalf("control: Destroy of muster's own instance in muster's own namespace: %v", err)
	}
	if _, err := cs.AppsV1().Deployments(ns).Get(ctx, "relabelled", metav1.GetOptions{}); err == nil {
		t.Error("control: the Deployment survived a Destroy that returned nil")
	}
	if _, err := cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); err == nil {
		t.Error("control: the namespace survived a Destroy that returned nil")
	}
}

// TestUpdateRemovesObjectsTheSpecNoLongerAsksFor.
//
// 🔴 A CREDENTIAL REMOVED FROM A SPEC HAS TO LEAVE THE CLUSTER. The render
// functions return nil for an object the spec no longer needs, and until the
// sweep in apply, nothing deleted it: the Secret kept the revoked token, the
// ConfigMap kept the file, and the Service kept selecting live pods.
func TestUpdateRemovesObjectsTheSpecNoLongerAsksFor(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		ctx := context.Background()
		d, cs := newDriver(t, m, nil)
		ns := m.ns("shrinking")

		full := provisiontest.MinimalSpec("shrinking")
		full.Secrets = []provision.EnvVar{{Name: "MUSTER_CALLBACK_TOKEN", Value: "revoked-value-51199"}}
		full.Files = []provision.File{
			{Path: "/etc/muster/agent.json", Content: []byte(`{"tools":["tasks"]}`)},
			{Path: "/etc/muster/token", Content: []byte("file-content-30517"), Secret: true},
		}
		if err := d.Create(ctx, full); err != nil {
			t.Fatalf("Create: %v", err)
		}

		// Control: everything exists first, so the absences below are removals.
		for _, o := range []struct {
			what string
			get  func() error
		}{
			{"configmap", func() error {
				_, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, "shrinking-files", metav1.GetOptions{})
				return err
			}},
			{"env secret", func() error {
				_, err := cs.CoreV1().Secrets(ns).Get(ctx, "shrinking-env", metav1.GetOptions{})
				return err
			}},
			{"file secret", func() error {
				_, err := cs.CoreV1().Secrets(ns).Get(ctx, "shrinking-secret-files", metav1.GetOptions{})
				return err
			}},
			{"service", func() error {
				_, err := cs.CoreV1().Services(ns).Get(ctx, "shrinking", metav1.GetOptions{})
				return err
			}},
		} {
			if err := o.get(); err != nil {
				t.Fatalf("control: the %s must exist before the update: %v", o.what, err)
			}
		}

		shrunk := provisiontest.MinimalSpec("shrinking")
		shrunk.Secrets = nil
		shrunk.Files = nil
		shrunk.Ports = nil
		if err := d.Update(ctx, shrunk); err != nil {
			t.Fatalf("Update: %v", err)
		}

		if _, err := cs.CoreV1().Secrets(ns).Get(ctx, "shrinking-env", metav1.GetOptions{}); err == nil {
			t.Error("the Secret holding the dropped credential survived the update; removing a credential " +
				"from a spec has to remove it from the cluster")
		}
		if _, err := cs.CoreV1().Secrets(ns).Get(ctx, "shrinking-secret-files", metav1.GetOptions{}); err == nil {
			t.Error("the Secret holding the dropped confidential file survived the update")
		}
		if _, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, "shrinking-files", metav1.GetOptions{}); err == nil {
			t.Error("the ConfigMap holding the dropped files survived the update")
		}
		if _, err := cs.CoreV1().Services(ns).Get(ctx, "shrinking", metav1.GetOptions{}); err == nil {
			t.Error("the Service survived a spec that declares no ports; its selector still matches live pods")
		}

		// The pod no longer references any of it either — a dangling reference
		// is a pod that cannot start.
		dep, err := cs.AppsV1().Deployments(ns).Get(ctx, "shrinking", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("deployment: %v", err)
		}
		if len(dep.Spec.Template.Spec.Containers[0].EnvFrom) != 0 {
			t.Errorf("the container still names a secret in envFrom: %+v",
				dep.Spec.Template.Spec.Containers[0].EnvFrom)
		}
		for _, v := range dep.Spec.Template.Spec.Volumes {
			if v.Secret != nil || v.ConfigMap != nil {
				t.Errorf("the pod still mounts %q after the files were dropped", v.Name)
			}
		}
	})
}

// TestApplyLeavesForeignCoNamedObjectsTheSpecDroppedAlone is the OTHER half of
// the closures the case above exercises.
//
// 🔴 ONE MUTATION NEUTRALISES BOTH HALVES, WHICH IS WHY BOTH NEED A CASE. The
// label read at each of apply's object sites is a single closure passed to the
// upsert AND to the stale-object sweep. Replacing one with `return
// managedLabels, nil` therefore disables the upsert's refusal — write over a
// stranger's object — and the sweep's foreign-skip — DELETE a stranger's object
// — at the same time. The upsert half is reached by a spec that asks for the
// object; this half is reached by a spec that does not. For the stale SERVICE
// sweep it is the only half there is: the Service's upsert has its own separate
// read, so nothing but this case reaches svcLabels at all.
//
// The consequence is worse than the upsert's. An overwrite is recoverable from
// whoever owns the object; a delete of somebody's Secret or ConfigMap is not,
// and apply reports success either way.
func TestApplyLeavesForeignCoNamedObjectsTheSpecDroppedAlone(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		ctx := context.Background()
		d, cs := newDriver(t, m, nil)

		// 🔴 THE REACHABILITY CONTROL, FIRST. A sweep that never ran would pass
		// every assertion below vacuously, so before asserting that foreign
		// objects SURVIVE it, prove in this same build that the sweep deletes
		// muster's OWN dropped objects.
		swept := provisiontest.MinimalSpec("swept")
		swept.Secrets = []provision.EnvVar{{Name: "MUSTER_CALLBACK_TOKEN", Value: "dropped-value-51199"}}
		swept.Files = []provision.File{
			{Path: "/etc/muster/agent.json", Content: []byte(`{"tools":["tasks"]}`)},
			{Path: "/etc/muster/token", Content: []byte("dropped-file-30517"), Secret: true},
		}
		if err := d.Create(ctx, swept); err != nil {
			t.Fatalf("control Create: %v", err)
		}
		bare := provisiontest.MinimalSpec("swept")
		bare.Ports = nil
		if err := d.Update(ctx, bare); err != nil {
			t.Fatalf("control Update: %v", err)
		}
		sweptNS := m.ns("swept")
		for _, o := range []struct{ what, name, kind string }{
			{"configmap", "swept-files", "configmap"},
			{"env secret", "swept-env", "secret"},
			{"file secret", "swept-secret-files", "secret"},
			{"service", "swept", "service"},
		} {
			var err error
			switch o.kind {
			case "configmap":
				_, err = cs.CoreV1().ConfigMaps(sweptNS).Get(ctx, o.name, metav1.GetOptions{})
			case "secret":
				_, err = cs.CoreV1().Secrets(sweptNS).Get(ctx, o.name, metav1.GetOptions{})
			case "service":
				_, err = cs.CoreV1().Services(sweptNS).Get(ctx, o.name, metav1.GetOptions{})
			}
			if err == nil {
				t.Fatalf("control: the sweep did not remove muster's own dropped %s, so this build's "+
					"sweep is not running and the assertions below would pass vacuously", o.what)
			}
		}

		// Now the guard. Nobody's objects but muster's were in the way above;
		// here every co-named object belongs to somebody else, and the spec asks
		// for none of them, so every sweep site meets a foreign object.
		ns := m.ns("tenant")
		meta := func(n string) metav1.ObjectMeta {
			return metav1.ObjectMeta{Name: n, Namespace: ns, Labels: foreignLabels()}
		}
		if _, err := cs.CoreV1().ConfigMaps(ns).Create(ctx, &corev1.ConfigMap{
			ObjectMeta: meta("tenant-files"),
			BinaryData: map[string][]byte{"their-dashboard.json": []byte(`{"panels":[1]}`)},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed configmap: %v", err)
		}
		if _, err := cs.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
			ObjectMeta: meta("tenant-env"),
			Data:       map[string][]byte{"ADMIN_PASSWORD": []byte("someone-elses-value-70413")},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed env secret: %v", err)
		}
		if _, err := cs.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
			ObjectMeta: meta("tenant-secret-files"),
			Data:       map[string][]byte{"their-tls.key": []byte("someone-elses-key-64907")},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed file secret: %v", err)
		}
		if _, err := cs.CoreV1().Services(ns).Create(ctx, &corev1.Service{
			ObjectMeta: meta("tenant"),
			Spec: corev1.ServiceSpec{
				ClusterIP: "203.0.113.9",
				Selector:  map[string]string{"app": "tenant"},
				Ports:     []corev1.ServicePort{{Name: "web", Port: 80}},
			},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed service: %v", err)
		}

		// No files, no secrets, no ports: muster renders none of these objects,
		// so apply takes the stale-sweep branch at all four sites.
		spec := provisiontest.MinimalSpec("tenant")
		spec.Ports = nil
		if err := d.Create(ctx, spec); err != nil {
			t.Fatalf("Create must succeed — a co-named object the spec does not render is nobody's "+
				"blocker, it is simply not muster's to touch; got %v", err)
		}
		// Control that apply ran to completion rather than bailing early.
		if _, err := cs.AppsV1().Deployments(ns).Get(ctx, "tenant", metav1.GetOptions{}); err != nil {
			t.Fatalf("control: apply did not reach the Deployment, so the sweeps after the first are "+
				"unreached: %v", err)
		}

		cm, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, "tenant-files", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("a ConfigMap muster never created was deleted by the stale-object sweep: %v", err)
		}
		if _, ok := cm.BinaryData["their-dashboard.json"]; !ok {
			t.Errorf("the foreign ConfigMap's content was replaced; keys are now %v", keysOf(cm.BinaryData))
		}
		envSec, err := cs.CoreV1().Secrets(ns).Get(ctx, "tenant-env", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("a Secret muster never created was deleted by the stale-object sweep — and it "+
				"held somebody's credential: %v", err)
		}
		if string(envSec.Data["ADMIN_PASSWORD"]) != "someone-elses-value-70413" {
			t.Errorf("the foreign env Secret's content was replaced; keys are now %v", keysOf(envSec.Data))
		}
		fileSec, err := cs.CoreV1().Secrets(ns).Get(ctx, "tenant-secret-files", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("a Secret holding somebody's confidential FILES was deleted by the stale-object "+
				"sweep: %v", err)
		}
		if string(fileSec.Data["their-tls.key"]) != "someone-elses-key-64907" {
			t.Errorf("the foreign file Secret's content was replaced; keys are now %v", keysOf(fileSec.Data))
		}
		svc, err := cs.CoreV1().Services(ns).Get(ctx, "tenant", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("a Service muster never created was deleted by the stale-object sweep — deleting it "+
				"takes somebody's traffic offline: %v", err)
		}
		if svc.Spec.Selector["app"] != "tenant" || len(svc.Spec.Ports) != 1 {
			t.Errorf("the foreign Service's spec was rewritten: selector=%v ports=%+v",
				svc.Spec.Selector, svc.Spec.Ports)
		}
	})
}

// TestUpdateLeavesTheWorkspaceClaimAlone is a DECLARED LIMIT, not regression
// coverage: the sweep above deliberately does not delete the PersistentVolume
// Claim, because it holds the instance's data and a spec edit that turns
// Persist off would otherwise destroy it on the next reconcile. Removing the
// claim is an operator's deliberate act.
func TestUpdateLeavesTheWorkspaceClaimAlone(t *testing.T) {
	eachMode(t, func(t *testing.T, m nsMode) {
		ctx := context.Background()
		d, cs := newDriver(t, m, nil)
		ns := m.ns("stateful")

		spec := provisiontest.MinimalSpec("stateful")
		spec.Workspace = provision.Workspace{Path: "/data", Size: "5Gi", Persist: true}
		if err := d.Create(ctx, spec); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, "stateful-workspace", metav1.GetOptions{}); err != nil {
			t.Fatalf("control: the claim must exist first: %v", err)
		}

		stateless := provisiontest.MinimalSpec("stateful")
		if err := d.Update(ctx, stateless); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if _, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(ctx, "stateful-workspace", metav1.GetOptions{}); err != nil {
			t.Fatalf("the workspace claim was deleted by a spec change; its data goes with it: %v", err)
		}
	})
}
