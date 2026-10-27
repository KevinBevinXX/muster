package k8s

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ZacxDev/muster/internal/provision"
)

// The re-create-after-destroy case: an instance created under a name whose
// previous namespace is still terminating (the agent replacement deletes `agent`
// and creates `agent` at once).
//
// ⚠ THE FAKE CLIENTSET HAS NO NAMESPACE LIFECYCLE: it neither finalizes a
// namespace nor refuses writes into a terminating one. So these tests build the
// terminating namespace by hand and assert what ensureNamespace DOES with it
// (wait, then create a fresh one); the apiserver's refusal it avoids is not
// observable here.

func terminatingNamespace(t *testing.T, cs *fake.Clientset, name, instance string) {
	t.Helper()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: instanceLabels(instance)},
		Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceTerminating},
	}
	if _, err := cs.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func shortTerminatingPoll(t *testing.T) {
	t.Helper()
	old := namespaceTerminatingPoll
	namespaceTerminatingPoll = 10 * time.Millisecond
	t.Cleanup(func() { namespaceTerminatingPoll = old })
}

// TestCreateWaitsOutTheNamespaceAnEarlierDestroyLeftTerminating: Create returns
// only after the old namespace is gone, and the namespace it leaves is a FRESH
// one — not the terminating object adopted.
func TestCreateWaitsOutTheNamespaceAnEarlierDestroyLeftTerminating(t *testing.T) {
	shortTerminatingPoll(t)
	d, cs := perInstanceDriver(t)
	const ns = "muster-agent-agent"
	terminatingNamespace(t, cs, ns, "agent")

	gone := make(chan time.Time, 1)
	go func() {
		time.Sleep(80 * time.Millisecond)
		_ = cs.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{})
		gone <- time.Now()
	}()
	if err := d.Create(context.Background(), internalSpec("agent")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	returned := time.Now()
	deletedAt := <-gone
	if returned.Before(deletedAt) {
		t.Fatalf("Create returned %v BEFORE the old namespace was gone — it adopted the terminating one", deletedAt.Sub(returned))
	}
	cur, err := cs.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cur.Status.Phase == corev1.NamespaceTerminating {
		t.Fatal("the instance's namespace is the terminating object, not a fresh one")
	}
}

// TestCreateGivesUpOnANamespaceThatNeverFinishesTerminating: bounded by ctx,
// and the error names the namespace and says why, as ErrBlind (it clears by
// itself; it is not a refusal).
func TestCreateGivesUpOnANamespaceThatNeverFinishesTerminating(t *testing.T) {
	shortTerminatingPoll(t)
	d, cs := perInstanceDriver(t)
	terminatingNamespace(t, cs, "muster-agent-agent", "agent")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := d.Create(ctx, internalSpec("agent"))
	if err == nil || !errors.Is(err, provision.ErrBlind) || !strings.Contains(err.Error(), `"muster-agent-agent" is still terminating`) {
		t.Fatalf("Create = %v, want ErrBlind naming the terminating namespace", err)
	}
}

// TestALiveOwnedNamespaceIsStillAdoptedAtOnce: the control — an owned namespace
// that is NOT terminating is continued into without waiting (the pre-existing
// create-or-continue behaviour).
func TestALiveOwnedNamespaceIsStillAdoptedAtOnce(t *testing.T) {
	old := namespaceTerminatingPoll
	namespaceTerminatingPoll = time.Hour // a wait here would hang the test
	t.Cleanup(func() { namespaceTerminatingPoll = old })
	d, cs := perInstanceDriver(t)
	if _, err := cs.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "muster-agent-agent", Labels: instanceLabels("agent")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Create(ctx, internalSpec("agent")); err != nil {
		t.Fatalf("Create into a live owned namespace: %v", err)
	}
}
