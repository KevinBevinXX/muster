package k8s

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/ZacxDev/muster/internal/provision"
)

// Config is how the Kubernetes driver is configured.
//
// 🔴 TWO FIELDS ARE POINTERS BECAUSE "UNSET" AND "EMPTY" ARE DIFFERENT
// ANSWERS, AND NOTHING IN THE PROJECT THIS CAME FROM COULD TELL THEM APART —
// it used os.Getenv exclusively, os.LookupEnv appeared zero times repo-wide,
// and the consequence was a required image repository defaulting to the empty
// string and producing an incomprehensible pull failure instead of "you did not
// configure an agent image". WorkspaceStorageClass distinguishes "this cluster
// has no storage I may use" (nil) from "use the cluster's default class" ("").
type Config struct {
	// Client is the cluster API client. REQUIRED.
	//
	// It is an interface rather than a concrete clientset so a fake can be
	// injected, which is what makes every test in this package runnable without
	// a cluster.
	Client kubernetes.Interface

	// RESTConfig enables Exec. Optional: nil means Capabilities.Exec is false
	// and provision.Exec refuses, rather than the call failing later with a
	// transport error nobody can attribute.
	RESTConfig *rest.Config

	// NamespacePerInstance gives each instance its own namespace, named
	// NamespacePrefix+Name. It raises Capabilities.Isolation to
	// IsolationNamespace, and it adds two steps to Destroy: an ownership
	// PRE-FLIGHT over the namespace before anything is deleted (see
	// checkNamespaceOwned), and the namespace deletion at the end.
	//
	// ⚠ IT DOES NOT REPLACE THE OBJECT-BY-OBJECT TEARDOWN, and this comment
	// used to say it did ("what makes Destroy a namespace deletion rather than
	// an object enumeration"). Destroy enumerates and removes every object it
	// created under BOTH layouts; the namespace delete is an extra step here.
	// Reading it the old way is what left namespaced RBAC behind in a shared
	// namespace, where no namespace is ever deleted.
	NamespacePerInstance bool

	// NamespacePrefix prefixes per-instance namespaces. Empty means "muster-".
	NamespacePrefix string

	// Namespace is where instances go when NamespacePerInstance is false.
	// REQUIRED in that case.
	Namespace string

	// EndpointTemplate overrides DefaultEndpointTemplate. It is rendered with
	// provision.EndpointVars.
	EndpointTemplate string

	// EndpointScheme is the scheme Endpoint reports. Empty means "http".
	EndpointScheme string

	// WorkspaceStorageClass is the StorageClass for persistent workspaces.
	//
	//   nil -> this driver reports Capabilities.Persistence FALSE and refuses a
	//          spec asking for a persistent workspace.
	//   ""  -> use the cluster's default StorageClass.
	//   "x" -> use class x.
	WorkspaceStorageClass *string

	// NetworkPolicy enables Capabilities.NetworkIsolation: with it set, a spec
	// asking for [provision.Network] isolation gets a per-instance NetworkPolicy,
	// written BEFORE the instance's Deployment.
	//
	//   nil -> the driver reports NetworkIsolation FALSE, refuses such a spec, and
	//          makes no networking.k8s.io call for any instance.
	//
	// ⚠ SETTING IT MAKES THE DRIVER NEED RBAC ON NetworkPolicies
	// (NetworkPolicyRBACPrerequisite), AND NOT ONLY FOR ISOLATED INSTANCES.
	// Without it an isolated instance is refused at apply time, naming the missing
	// verb — see networkPolicyForbidden. And with this set, Destroy reads
	// networkpolicies for EVERY instance, isolated or not, so without `get` the
	// destroy of any instance fails after its Deployment is removed. See Destroy.
	NetworkPolicy *NetworkPolicyConfig

	// Logger is optional.
	Logger *log.Logger
}

// Driver provisions onto Kubernetes. See the package doc for what it renders
// and what it deliberately does not do.
type Driver struct {
	cfg  Config
	tmpl provision.EndpointTemplate
	log  *log.Logger
}

var (
	_ provision.Provisioner = (*Driver)(nil)
	_ provision.Execer      = (*Driver)(nil)
)

// New builds the driver, validating the configuration up front so a bad
// endpoint template is a startup failure naming itself rather than a
// per-request error naming DNS.
func New(cfg Config) (*Driver, error) {
	if cfg.Client == nil {
		return nil, errors.New("k8s: Config.Client is required")
	}
	if !cfg.NamespacePerInstance && strings.TrimSpace(cfg.Namespace) == "" {
		return nil, errors.New("k8s: Config.Namespace is required when NamespacePerInstance is false " +
			"(there is no safe default: guessing 'default' would put agents somewhere nobody chose)")
	}
	raw := cfg.EndpointTemplate
	if strings.TrimSpace(raw) == "" {
		raw = DefaultEndpointTemplate
	}
	tmpl, err := provision.ParseEndpointTemplate(raw)
	if err != nil {
		return nil, fmt.Errorf("k8s: %w", err)
	}
	if cfg.NetworkPolicy != nil {
		if err := cfg.NetworkPolicy.validate(); err != nil {
			return nil, err
		}
	}
	lg := cfg.Logger
	if lg == nil {
		lg = log.New(io.Discard, "", 0)
	}
	return &Driver{cfg: cfg, tmpl: tmpl, log: lg}, nil
}

// Driver implements provision.Provisioner.
func (d *Driver) Driver() string { return "kubernetes" }

// Capabilities implements provision.Provisioner.
//
// 🔴 TWO LOSSES THIS DRIVER HAS ARE NOT ON THIS TYPE, AND HAVE TO BE READ HERE:
//
//   - IT RESTRICTS NO EGRESS BY DNS NAME. That is the control that addresses
//     exfiltration by a prompt-injected model, and NetworkIsolation true is NOT
//     it: the NetworkPolicy this driver renders for an isolated instance
//     (network.go) is an ADDRESS-RANGE policy — DNS plus the spec's ports on
//     public addresses — so an isolated instance can still reach any public host
//     on those ports. What it keeps the instance off is every address in
//     nonPublicIPv4 EXCEPT THE CLUSTER'S DNS PODS (reachable on port 53, which
//     is itself a channel) — so the cluster and the LAN WHERE THOSE ARE PRIVATE
//     ADDRESSES, which is a property of the cluster and not something this
//     driver checks (see that variable's ⚠). It does not keep data in. And an
//     instance whose spec declares no isolation gets no NetworkPolicy at all.
//   - IT RUNS NO SIDECARS. One container plus an init container. The chart this
//     replaces could run three log tailers, and they were structurally
//     invisible to that project's own log reads anyway.
//
// Both were once Capabilities fields. They were deleted because nothing
// branched on them and no Spec field could ever ask for either, so they
// announced a loss to a caller that had no decision to make — which is a
// statement for humans, and it belongs in prose like this.
func (d *Driver) Capabilities() provision.Capabilities {
	isolation := provision.IsolationContainer
	if d.cfg.NamespacePerInstance {
		isolation = provision.IsolationNamespace
	}
	return provision.Capabilities{
		Isolation:      isolation,
		Secrets:        true,
		Files:          true,
		Persistence:    d.cfg.WorkspaceStorageClass != nil,
		ResourceLimits: true,
		Scale:          true,
		Exec:           d.cfg.RESTConfig != nil,
		// The capability is "I will write the policy, and refuse to start the
		// instance if I cannot" — see provision.Capabilities.NetworkIsolation on
		// why that is not a claim that anything enforces it.
		NetworkIsolation: d.cfg.NetworkPolicy != nil,
	}
}

// namespaceFor resolves an instance's namespace.
func (d *Driver) namespaceFor(name string) string {
	if !d.cfg.NamespacePerInstance {
		return d.cfg.Namespace
	}
	prefix := d.cfg.NamespacePrefix
	if prefix == "" {
		prefix = "muster-"
	}
	return prefix + name
}

// blind wraps an API error as provision.ErrBlind.
//
// 🔴 EVERY "I COULD NOT REACH THE API" PATH GOES THROUGH HERE, AND THAT IS WHY
// List CANNOT ACCIDENTALLY RETURN AN EMPTY SLICE. A NotFound is explicitly NOT
// blindness and is handled at its own call site — collapsing the two is the
// mistake that turns "the cluster is unreachable" into "nothing is running".
//
// ⚠ THE %v IS DELIBERATE AND IS NOT A BUG TO FIX. This function's whole job is
// to assert ONE cause, so a %w that dragged the wrapped error's own sentinels
// into the result would let the same error claim to be two different things at
// once — an ownership refusal reporting BOTH ErrBlind and ErrNotFound is
// strictly worse than reporting only the wrong one, because the ErrBlind branch
// still fires and now nothing looks suspicious. What must not reach here is an
// error that is not unreachability; see applyFailed, which is the one place on
// the write path that decides. The cost of %v is that a caller cannot inspect
// the client-go error underneath; no caller in this repository does.
func blind(op string, err error) error {
	return fmt.Errorf("%w: %s: %v", provision.ErrBlind, op, err)
}

// applyFailed attributes a failure from an object upsert on a WRITE path —
// apply's six kinds and the NetworkPolicy applyNetworkPolicy writes ahead of them.
// (The policy-grant RBAC upserts that used to come through here too were
// removed with the privilege subsystem.)
//
// 🔴 AN OWNERSHIP REFUSAL IS NOT UNREACHABILITY, AND THIS IS THE ONE PLACE THE
// WRITE PATH DISTINGUISHES THEM. All five upserts in apply() used to wrap their
// error in blind() directly, so a PERMANENT refusal to write over somebody
// else's ServiceAccount, ConfigMap, Secret, Service or Deployment arrived at the
// caller as provision.ErrBlind — a TRANSIENT "backend unreachable". Measured at
// 138d269 under both namespace layouts: errors.Is(err, ErrBlind) true,
// errors.Is(err, ErrNotFound) false, errors.As(err, **notManagedError) false.
// A caller that retries on ErrBlind therefore never converges, one that alerts
// on it pages for an outage that is not happening, and the refusal's own type
// and sentinel were gone.
//
// 🔴 THE HEDGE THIS COMMENT USED TO CARRY — "nothing stops a SIXTH site calling
// blind() directly" — WAS ALREADY TRUE WHEN IT WAS WRITTEN, of the two upserts
// in the (since removed) policy grant path. Consolidating apply's five sites
// here left that RBAC path out of the consolidation entirely, and a predicate
// applied at some of its sites is wrong at the rest of them in the same
// direction. Measured at f373092 with a foreign co-named ClusterRole in the way:
// errors.Is(err, ErrBlind) true, errors.Is(err, ErrNotManaged) false. Every
// write in this driver that can collide with a stranger's object returns
// through this function.
//
// 🔴 IT KEYS ON THE SENTINEL, NOT ON THE CONCRETE TYPE, AND THAT IS THE WIDER
// PREDICATE. errors.As(err, **notManagedError) would misroute any refusal that
// reports provision.ErrNotManaged through a plain fmt.Errorf — ensureNamespace's
// does, for the reason stated there — straight back into blind(), which is the
// exact defect above wearing a different shape. What matters to a caller is the
// sentinel it branches on, so that is what decides here. No client-go error
// reports ErrNotManaged, so this cannot swallow a genuine transport failure.
func applyFailed(op string, err error) error {
	if errors.Is(err, provision.ErrNotManaged) {
		return fmt.Errorf("%s: %w", op, err)
	}
	return blind(op, err)
}

func notFound(name string) error {
	return fmt.Errorf("%w: %q", provision.ErrNotFound, name)
}

// notManagedError is the refusal every by-name path returns for an object this
// driver did not create.
//
// 🔴 IT ALWAYS REPORTS provision.ErrNotManaged. It reports ErrNotFound ONLY
// when a by-name READ produced it, and the asymmetry is the whole reason this
// type carries a flag instead of being one shape:
//
//   - A READ (Get, Scale, Endpoint, the log paths) asked "what is instance X".
//     A stranger's co-named object is not instance X, so "muster has no
//     instance by that name" is the right answer and ErrNotFound is the right
//     sentinel — reporting it as an instance is how a status read describes a
//     stranger's Deployment as an agent.
//   - An ACTION (an upsert inside apply, or Destroy) asked muster to WRITE or
//     REMOVE X. "There is no such instance" is not what happened. Worse,
//     `if err != nil && !errors.Is(err, provision.ErrNotFound)` is the idiom
//     Destroy's contract invites, so an ErrNotFound-flavoured refusal there is
//     SILENTLY DISCARDED — which is the opposite of being loud.
//
// 🔴 THE CONCRETE TYPE SURVIVES errors.As ON EVERY PATH THAT CONSTRUCTS IT, AND
// THAT IS NARROWER THAN "ON EVERY PATH" — which is what this comment used to
// say. One refusal about a foreign object is NOT this type: ensureNamespace
// returns a plain fmt.Errorf wrapping provision.ErrNotManaged, because its
// message has to carry a reason this type's fixed format cannot express ("Destroy
// deletes this namespace and everything in it"); ownership_test.go asserts the
// "refusing to adopt" half of that sentence. So errors.Is(err,
// provision.ErrNotManaged) holds there and errors.As(err, **notManagedError)
// does NOT.
//
// Nothing load-bearing depends on the difference: notManagedError is unexported,
// so no caller outside this package can errors.As for it at all, and the two
// in-package readers are Update's pre-flight branch and Destroy's — both of
// which read a refusal that getOwnedDeployment constructed. applyFailed keys on
// the SENTINEL for exactly this reason; see its comment.
//
// notmanaged_internal_test.go's ledger pins the type's survival at every site
// that constructs it, and names the one that does not.
type notManagedError struct {
	kind, name, ns string
	// labels is what the object ACTUALLY carries, so the message can name the
	// specific key that is missing or mismatched. See labelComplaint.
	labels map[string]string
	// fromRead is true when a by-name read produced this, which is the only
	// case that also answers to provision.ErrNotFound.
	fromRead bool
}

func (e *notManagedError) Error() string {
	sentinel, tail := provision.ErrNotManaged, "so muster will neither write to it nor remove it"
	if e.fromRead {
		sentinel, tail = provision.ErrNotFound, "so muster has no instance by that name"
	}
	where := ""
	if e.ns != "" {
		where = fmt.Sprintf(" in namespace %q", e.ns)
	}
	return fmt.Sprintf("%s: %s %q%s is not managed by muster (%s), %s",
		sentinel, e.kind, e.name, where, labelComplaint(e.labels), tail)
}

func (e *notManagedError) Is(target error) bool {
	return target == provision.ErrNotManaged || (e.fromRead && target == provision.ErrNotFound)
}

// forAction re-flavours a refusal a READ helper produced, for a caller that was
// asking to act rather than to look. Destroy needs it: it finds the foreign
// Deployment through getOwnedDeployment, whose refusal is read-flavoured.
func (e *notManagedError) forAction() *notManagedError {
	out := *e
	out.fromRead = false
	return &out
}

// notManagedRead is the refusal a by-name READ returns. It answers to
// provision.ErrNotFound as well as provision.ErrNotManaged.
func notManagedRead(kind, name, ns string, labels map[string]string) error {
	return &notManagedError{kind: kind, name: name, ns: ns, labels: labels, fromRead: true}
}

// notManagedAction is the refusal a WRITE or a TEARDOWN returns. It does NOT
// answer to provision.ErrNotFound — see the type's comment for why that would
// make it swallowable.
func notManagedAction(kind, name, ns string, labels map[string]string) error {
	return &notManagedError{kind: kind, name: name, ns: ns, labels: labels}
}

// labelComplaint names what is genuinely wrong with an object's labels, key by
// key, against every key of managedLabels.
//
// 🔴 IT REPORTS BOTH KEYS BECAUSE owned() REQUIRES BOTH, AND THE MESSAGE IS THE
// ENTIRE INTERFACE. The refusal used to print `app.kubernetes.io/managed-by=
// muster` and nothing else, so an object labelled managed-by=muster with
// app.kubernetes.io/name set to something else was refused with "it does not
// carry app.kubernetes.io/managed-by=muster" — a false statement about the
// object in front of the operator, whose only named remedy was already
// satisfied. Relabelling is the one documented escape from these refusals, so
// following that message looped.
func labelComplaint(labels map[string]string) string {
	keys := make([]string, 0, len(managedLabels))
	for k := range managedLabels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		want := managedLabels[k]
		got, ok := labels[k]
		switch {
		case !ok:
			parts = append(parts, fmt.Sprintf("it does not carry %s, which must be %q", k, want))
		case got != want:
			parts = append(parts, fmt.Sprintf("its %s is %q, not %q", k, got, want))
		}
	}
	if len(parts) == 0 {
		// Unreachable while owned() is the only predicate that produces this
		// error, because both read the same managedLabels: owned() returning
		// false means at least one key above mismatched. It is here as a
		// disagreement detector between the two derivations, not as a guard —
		// if it ever prints, owned() and this function have drifted apart.
		return "yet no key of managedLabels is missing or mismatched, so owned() and this message disagree"
	}
	return strings.Join(parts, "; ")
}

// labelsOf adapts a typed client Get to the labels-and-error pair the ownership
// helpers read. The error is checked FIRST, so a typed nil object is never
// dereferenced.
func labelsOf(o metav1.Object, err error) (map[string]string, error) {
	if err != nil {
		return nil, err
	}
	return o.GetLabels(), nil
}

// getOwnedDeployment reads an instance's Deployment and refuses one this driver
// did not create.
//
// 🔴 EVERY BY-NAME READ GOES THROUGH HERE — Get, Scale, Endpoint and the log
// paths — because the ownership check is one rule and a copy of it at each call
// site is the same bug waiting to be fixed four times. List needs no equivalent:
// it filters server-side with managedSelector().
func (d *Driver) getOwnedDeployment(ctx context.Context, ns, name string) (*appsv1.Deployment, error) {
	dep, err := d.cfg.Client.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, notFound(name)
	}
	if err != nil {
		return nil, blind("get deployment "+name, err)
	}
	if !owned(dep.Labels) {
		return nil, notManagedRead("deployment", name, ns, dep.Labels)
	}
	return dep, nil
}

// upsertOwned creates, and on AlreadyExists updates — but only after reading
// the existing object's labels and confirming this driver created it.
//
// 🔴 THE READ IS WHAT SEPARATES AN UPDATE FROM A HIJACK. `Create` then `Update`
// on AlreadyExists is a name-keyed write: in a shared namespace the object that
// already holds the name can be anything, and overwriting somebody else's
// Service with a selector pointing at muster's pods is a silent takeover of
// their traffic. It costs one extra GET, and only on the collision path.
func (d *Driver) upsertOwned(what, name, ns string, get func() (map[string]string, error), create, update func() error) error {
	err := create()
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	labels, gerr := get()
	if gerr != nil {
		return gerr
	}
	if !owned(labels) {
		return notManagedAction(what, name, ns, labels)
	}
	return update()
}

// createOwned creates an object that must not be UPDATED once it exists, and
// refuses one this driver did not create.
//
// 🔴 IT IS THE CREATE-ONLY HALF OF upsertOwned, AND IT EXISTS BECAUSE THE SAME
// EIGHT LINES WERE OPEN-CODED AT THREE SITES — apply's workspace claim and the
// two role bindings of the since-removed policy grant path — of which TWO WERE
// WRONG IN THE SAME DIRECTION: the bindings treated AlreadyExists as success
// outright, so a binding that was a stranger's co-named object was reported
// applied while the subject and the RoleRef were somebody else's choice. Only
// the workspace claim calls it now.
//
// An AlreadyExists over an object that IS ours is success, not an update: the
// caller's object is immutable in the fields that matter (a PVC's binding) and
// its name is deterministic, so an existing one of ours is already the object
// we want.
func (d *Driver) createOwned(what, name, ns string, get func() (map[string]string, error), create func() error) error {
	err := create()
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	labels, gerr := get()
	if gerr != nil {
		return gerr
	}
	if !owned(labels) {
		return notManagedAction(what, name, ns, labels)
	}
	return nil
}

// deleteIfOwned deletes an object only when this driver created it. A missing
// object is success — absent is the state the caller asked for. A FOREIGN
// object is left alone and logged: it belongs to somebody else, and destroying
// an instance must not touch it.
//
// ⚠ EVERY CALLER OF THIS IS A SATELLITE OBJECT — a ConfigMap, a Secret, a
// Service, a claim, a ServiceAccount and a NetworkPolicy — and
// the quiet skip is right for those and WRONG for the instance's identity
// anchors. The Deployment and the per-instance Namespace do not come through
// here: a foreign one of those means the name is not muster's to operate on at
// all, and both refuse loudly. See checkNamespaceOwned, which is where that
// difference is argued (destroyNamespace used to call this, and used to be where
// the argument lived).
//
// ⚠ WHAT IT DOES NOT CLAIM: atomicity. The labels are read immediately before
// the delete, but nothing stops the object being replaced between the two
// calls. Closing that needs a delete precondition on the object's UID, which
// the fake clientset these tests run against does not enforce — so it would be
// code no test in this repository could exercise.
func (d *Driver) deleteIfOwned(what, name string, get func() (map[string]string, error), del func() error) error {
	labels, err := get()
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !owned(labels) {
		d.log.Printf("k8s: leaving %s %q alone: it is not managed by muster", what, name)
		return nil
	}
	if err := del(); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// checkSpec is the capability branch plus the one requirement that is specific
// to this backend.
func (d *Driver) checkSpec(spec provision.Spec) error {
	if err := provision.CheckSpec(d.Capabilities(), spec); err != nil {
		return err
	}
	// A pod needs an image. Spec.Runtime is a union so that a process driver is
	// expressible; this driver is the half of the union that needs the other
	// member.
	if spec.Runtime.Image == "" {
		return fmt.Errorf("%w: the kubernetes driver needs Runtime.Image; a Command-only runtime has "+
			"nothing to run it in", provision.ErrUnsupported)
	}
	return nil
}

// Create implements provision.Provisioner.
func (d *Driver) Create(ctx context.Context, spec provision.Spec) error {
	if err := d.checkSpec(spec); err != nil {
		return err
	}
	ns := d.namespaceFor(spec.Ref.Name)

	existing, err := d.cfg.Client.AppsV1().Deployments(ns).Get(ctx, spec.Ref.Name, metav1.GetOptions{})
	switch {
	case err == nil:
		// 🔴 A WORKLOAD THIS DRIVER DID NOT CREATE IS NOT AN INSTANCE TO
		// RECONCILE. It is refused here rather than read for a fingerprint,
		// because the fingerprint of a foreign object is absent and "absent"
		// would otherwise be reported as divergence — the right refusal for the
		// wrong reason, and a message nobody can act on.
		if !owned(existing.Labels) {
			// The label detail is here for the reason labelComplaint exists:
			// relabelling is the only documented escape from an ownership
			// refusal, so a refusal that declines to name which key is wrong
			// sends an operator to fix something it did not identify.
			return fmt.Errorf("%w: %q already exists in namespace %s and is not managed by muster "+
				"(%s); muster will neither adopt nor overwrite it",
				provision.ErrDivergentSpec, spec.Ref.Name, ns, labelComplaint(existing.Labels))
		}
		// 🔴 IDEMPOTENCE IS DECIDED BY THE RECORDED FINGERPRINT, NOT BY A DIFF
		// OF THE OBJECTS. The live Deployment has been through defaulting and
		// possibly through another controller, so comparing it field by field
		// reports divergence for changes nobody made.
		want := provision.Fingerprint(spec)
		if existing.Annotations[annFingerprint] == want {
			return nil
		}
		return fmt.Errorf("%w: %q exists with fingerprint %s, spec is %s",
			provision.ErrDivergentSpec, spec.Ref.Name,
			short(existing.Annotations[annFingerprint]), short(want))
	case apierrors.IsNotFound(err):
		// fall through and create
	default:
		return blind("get deployment "+spec.Ref.Name, err)
	}
	return d.apply(ctx, spec, ns)
}

// Update implements provision.Provisioner.
//
// ⚠ IT CREATES WHEN ABSENT, BUT IT DOES NOT ADOPT. A Deployment already holding
// this name that muster did not create is refused before anything is written:
// "make it look like this" is not a licence to take over somebody else's
// workload, and apply's first write would otherwise land before the Deployment
// upsert refused.
func (d *Driver) Update(ctx context.Context, spec provision.Spec) error {
	if err := d.checkSpec(spec); err != nil {
		return err
	}
	ns := d.namespaceFor(spec.Ref.Name)
	if _, err := d.getOwnedDeployment(ctx, ns, spec.Ref.Name); err != nil {
		var foreign *notManagedError
		switch {
		case errors.As(err, &foreign):
			// 🔴 forAction, FOR THE REASON Destroy DOES IT. This refusal arrived
			// through the shared by-name READ, so as received it answers to
			// ErrNotFound — and ErrNotFound from Update is incoherent on its
			// face, because Update's documented answer to an ABSENT instance is
			// to create it. A caller branching that way would retry the refused
			// Update forever. Leaving it read-flavoured would also have left
			// Update disagreeing with its own apply(), which refuses a foreign
			// ServiceAccount with ErrNotManaged alone.
			return foreign.forAction()
		case errors.Is(err, provision.ErrNotFound):
			// Absent. Update creates it, which is the documented behaviour.
		default:
			return err
		}
	}
	return d.apply(ctx, spec, ns)
}

// apply renders and upserts every object for spec.
//
// ⚠ ORDER IS LOAD-BEARING. The Deployment goes LAST, because it references the
// ServiceAccount, the ConfigMap, the Secret and the PVC; creating it first
// produces a pod that fails to start for a reason attributed to the wrong
// object. The NetworkPolicy goes FIRST (after the namespace it lives in), for a
// different reason: it is the one object whose absence does not stop the pod
// starting — it lets it start unconfined.
// 🔴 IT ALSO REMOVES WHAT THE SPEC NO LONGER ASKS FOR. A render function
// returning nil means "this instance has no such object any more", and until
// this swept them, that left the object in the cluster: a Secret still holding
// a credential the spec had dropped, a Service still selecting live pods, a
// ConfigMap still mounting a file that was deleted. Removing a credential from
// a spec has to remove it from the cluster, or the revocation is a comment.
//
// ⚠ THE PVC IS AN EXCEPTION, AND IT IS NOT SWEPT. A workspace claim holds
// the instance's data; deleting it because a spec edit turned Persist off would
// destroy that data on a reconcile. It is left for an operator to remove
// deliberately.
//
// ⚠ THE NetworkPolicy IS THE OTHER EXCEPTION. A spec that stops declaring
// isolation leaves its policy in place; applyNetworkPolicy says why.
func (d *Driver) apply(ctx context.Context, spec provision.Spec, ns string) error {
	c := d.cfg.Client
	name := spec.Ref.Name

	if d.cfg.NamespacePerInstance {
		if err := d.ensureNamespace(ctx, name, ns); err != nil {
			return err
		}
	}

	// 🔴 FIRST, BEFORE ANYTHING THAT CAN RUN OR HOLD A CREDENTIAL. See
	// applyNetworkPolicy: after a refusal here this call must WRITE no Deployment
	// and no Secret. (What an existing instance already has, it keeps.)
	if err := d.applyNetworkPolicy(ctx, spec, ns); err != nil {
		return err
	}

	sa := d.renderServiceAccount(spec, ns)
	if err := d.upsertOwned("serviceaccount", sa.Name, ns,
		func() (map[string]string, error) {
			return labelsOf(c.CoreV1().ServiceAccounts(ns).Get(ctx, sa.Name, metav1.GetOptions{}))
		},
		func() error {
			_, e := c.CoreV1().ServiceAccounts(ns).Create(ctx, sa, metav1.CreateOptions{})
			return e
		},
		func() error {
			_, e := c.CoreV1().ServiceAccounts(ns).Update(ctx, sa, metav1.UpdateOptions{})
			return e
		}); err != nil {
		return applyFailed("apply serviceaccount "+sa.Name, err)
	}

	cmName := configMapName(name)
	cmLabels := func() (map[string]string, error) {
		return labelsOf(c.CoreV1().ConfigMaps(ns).Get(ctx, cmName, metav1.GetOptions{}))
	}
	if cm := d.renderConfigMap(spec, ns); cm != nil {
		if err := d.upsertOwned("configmap", cm.Name, ns, cmLabels,
			func() error {
				_, e := c.CoreV1().ConfigMaps(ns).Create(ctx, cm, metav1.CreateOptions{})
				return e
			},
			func() error {
				_, e := c.CoreV1().ConfigMaps(ns).Update(ctx, cm, metav1.UpdateOptions{})
				return e
			}); err != nil {
			return applyFailed("apply configmap "+cm.Name, err)
		}
	} else if err := d.deleteIfOwned("configmap", cmName, cmLabels, func() error {
		return c.CoreV1().ConfigMaps(ns).Delete(ctx, cmName, metav1.DeleteOptions{})
	}); err != nil {
		return fmt.Errorf("delete stale configmap %s: %w", cmName, err)
	}

	for _, s := range []struct {
		what   string
		name   string
		render func(provision.Spec, string) *corev1.Secret
	}{
		{"secret", envSecretName(name), d.renderEnvSecret},
		{"secret", fileSecretName(name), d.renderFileSecret},
	} {
		secName := s.name
		labels := func() (map[string]string, error) {
			return labelsOf(c.CoreV1().Secrets(ns).Get(ctx, secName, metav1.GetOptions{}))
		}
		if sec := s.render(spec, ns); sec != nil {
			if err := d.upsertOwned(s.what, sec.Name, ns, labels,
				func() error {
					_, e := c.CoreV1().Secrets(ns).Create(ctx, sec, metav1.CreateOptions{})
					return e
				},
				func() error {
					_, e := c.CoreV1().Secrets(ns).Update(ctx, sec, metav1.UpdateOptions{})
					return e
				}); err != nil {
				return applyFailed("apply secret "+sec.Name, err)
			}
			continue
		}
		if err := d.deleteIfOwned(s.what, secName, labels, func() error {
			return c.CoreV1().Secrets(ns).Delete(ctx, secName, metav1.DeleteOptions{})
		}); err != nil {
			return fmt.Errorf("delete stale secret %s: %w", secName, err)
		}
	}

	pvc, err := d.renderPVC(spec, ns)
	if err != nil {
		return err
	}
	if pvc != nil {
		// ⚠ CREATE-ONLY. Almost every field of a bound PVC is immutable, so an
		// Update here fails on a claim that is working perfectly. An existing
		// claim is the state we want — PROVIDED it is ours. A claim somebody
		// else made under this name would otherwise be mounted into the
		// instance's pod, which hands its contents to the agent.
		//
		// 🔴 THE REFUSAL IS NOT notFound. A Create returning the sentinel that
		// means "this instance does not exist" is indistinguishable from the
		// caller's own precondition in a Get -> ErrNotFound -> Create loop,
		// which would retry the refused Create forever. createOwned returns
		// ErrNotManaged alone; applyFailed keeps it out of ErrBlind.
		if err := d.createOwned("persistentvolumeclaim", pvc.Name, ns,
			func() (map[string]string, error) {
				return labelsOf(c.CoreV1().PersistentVolumeClaims(ns).Get(ctx, pvc.Name, metav1.GetOptions{}))
			},
			func() error {
				_, e := c.CoreV1().PersistentVolumeClaims(ns).Create(ctx, pvc, metav1.CreateOptions{})
				return e
			}); err != nil {
			return applyFailed("apply persistentvolumeclaim "+pvc.Name, err)
		}
	}

	svcLabels := func() (map[string]string, error) {
		return labelsOf(c.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{}))
	}
	if svc := d.renderService(spec, ns); svc != nil {
		if err := d.upsertService(ctx, ns, svc); err != nil {
			return applyFailed("apply service "+svc.Name, err)
		}
	} else if err := d.deleteIfOwned("service", name, svcLabels, func() error {
		return c.CoreV1().Services(ns).Delete(ctx, name, metav1.DeleteOptions{})
	}); err != nil {
		return fmt.Errorf("delete stale service %s: %w", name, err)
	}

	dep, err := d.renderDeployment(spec, ns)
	if err != nil {
		return err
	}
	if err := d.upsertOwned("deployment", dep.Name, ns,
		func() (map[string]string, error) {
			return labelsOf(c.AppsV1().Deployments(ns).Get(ctx, dep.Name, metav1.GetOptions{}))
		},
		func() error {
			_, e := c.AppsV1().Deployments(ns).Create(ctx, dep, metav1.CreateOptions{})
			return e
		},
		func() error {
			_, e := c.AppsV1().Deployments(ns).Update(ctx, dep, metav1.UpdateOptions{})
			return e
		}); err != nil {
		return applyFailed("apply deployment "+dep.Name, err)
	}
	return nil
}

// ensureNamespace creates the instance's namespace, and REFUSES one this driver
// did not create.
//
// 🔴 AN IGNORED AlreadyExists IS AN ADOPTION, AND Destroy DELETES THIS
// NAMESPACE WHOLE. Continuing past a namespace somebody else made therefore
// schedules the deletion of everything in it — the objects, the workloads and
// the secrets of whoever owned it. The label read below is what makes
// create-or-continue a decision rather than an assumption.
//
// 🔴 IT IS provision.ErrNotManaged, AND IT AGREES WITH Destroy'S NAMESPACE
// STEP. This comment used to read "the refusal is not one of the provision
// sentinels: none of them names 'somebody else's object'" — true when written,
// and no longer: ErrNotManaged names exactly that, and it is what every other
// foreign-object refusal in this driver reports. ErrInvalidSpec, the
// alternative that comment rejected, would still be wrong for the reason it
// gave — it would blame the spec for the state of the cluster.
func (d *Driver) ensureNamespace(ctx context.Context, instance, ns string) error {
	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   ns,
		Labels: instanceLabels(instance),
	}}
	_, err := d.cfg.Client.CoreV1().Namespaces().Create(ctx, nsObj, metav1.CreateOptions{})
	switch {
	case err == nil:
		return nil
	case !apierrors.IsAlreadyExists(err):
		return blind("create namespace "+ns, err)
	}
	cur, gerr := d.cfg.Client.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if gerr != nil {
		return blind("get namespace "+ns, gerr)
	}
	if !owned(cur.Labels) {
		return fmt.Errorf("%w: namespace %q already exists and is not managed by muster (%s); "+
			"refusing to adopt it, because Destroy deletes this namespace and everything in it",
			provision.ErrNotManaged, ns, labelComplaint(cur.Labels))
	}
	return nil
}

// checkNamespaceOwned is Destroy's PRE-FLIGHT over the per-instance namespace:
// it reads the namespace and refuses one this driver did not create, WITHOUT
// deleting anything.
//
// 🔴 IT RUNS BEFORE THE FIRST DELETE, AND THAT ORDER IS THE FIX FOR A MEASURED
// INCOHERENCE. The check used to live at the END of Destroy, so a foreign
// namespace holding an instance whose own objects WERE muster's produced this:
// the Deployment, the ServiceAccount and every satellite were removed, and then Destroy returned provision.ErrNotManaged — measured on
// three consecutive calls, with Get reporting ErrNotFound in between. A caller
// looking at that cannot tell a refusal that changed nothing from one that tore
// the instance down, and "Destroy until nil" never terminates. Checked first,
// the two states are the only two there are: muster owns the namespace and
// destroys everything (nil), or it does not and destroys NOTHING (a stable
// ErrNotManaged, with Get still reporting the instance).
//
// ⚠ THE COST IS THAT AN AGENT MUSTER CREATED KEEPS RUNNING. If somebody
// relabels the namespace out from under an instance, Destroy will not stop its
// pod until the label is put back or the namespace is removed by hand. That is
// the same answer this driver already gives for a foreign co-named DEPLOYMENT —
// the other identity anchor — and it is the price of the alternative being the
// non-convergent teardown above. It is NOT a claim that leaving the pod running
// is harmless.
//
// ⚠ WHY THE CO-NAMED SATELLITES ARE STILL SKIPPED QUIETLY. A foreign
// ConfigMap, Secret, Service, claim, ServiceAccount or NetworkPolicy under the
// instance's name
// stays a logged skip (deleteIfOwned). The distinction is which objects are the
// instance's IDENTITY ANCHORS: a foreign Deployment or a foreign per-instance
// Namespace means no instance can exist under this name under ANY spec — Create
// refuses both unconditionally — whereas a foreign satellite blocks only a spec
// that RENDERS that object, and Destroy has no spec to consult. Destroy cannot
// tell a satellite that would have been refused from one that would never have
// been touched, so it does not act on it.
//
// 🔴 WHAT THE SKIP DOES **NOT** MEAN: that the foreign satellite survives. Under
// NamespacePerInstance — the only layout in which this function runs — Destroy
// goes on to delete the namespace WHOLE, and ensureNamespace's own comment says
// namespace deletion "schedules the deletion of everything in it". So in this
// layout the quiet skip saves the object from being deleted BY NAME and not from
// being deleted at all. This paragraph previously claimed "leaving it alone is
// right either way", which is false here. ⚠ NOT MEASURED: the fake clientset
// these tests run against implements no namespace garbage collection, so the
// suite is structurally blind to the consequence and cannot tell the two
// readings apart; the statement above is Kubernetes semantics plus this file's
// own comment, NOT an observation from this repository. The place the skip is
// unambiguously right is the SHARED-namespace layout, where nothing deletes a
// namespace and this function never runs.
//
// ⚠ SO THIS IS NOT "LOUD WHEREVER CREATE REFUSES", which is the tidier sentence
// and is false: Create DOES refuse a foreign co-named ConfigMap when the spec
// carries files, and Destroy skips that same object. The split is between
// anchors and satellites, not between loud and quiet Creates.
func (d *Driver) checkNamespaceOwned(ctx context.Context, ns string) error {
	cur, err := d.cfg.Client.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return blind("get namespace "+ns, err)
	}
	if !owned(cur.Labels) {
		// The namespace is cluster-scoped, so it has no namespace of its own to
		// name: the empty string is what suppresses the "in namespace" clause.
		return notManagedAction("namespace", ns, "", cur.Labels)
	}
	return nil
}

// destroyNamespace deletes the instance's own namespace, re-reading its
// ownership immediately before the delete.
//
// ⚠ THE RE-READ IS NOT REDUNDANT WITH Destroy'S PRE-FLIGHT. The pre-flight is
// about ORDER — refuse before anything is removed — and the whole teardown runs
// between the two reads. Without a second check, a namespace that appeared or
// was replaced during the teardown would be deleted on the strength of a read
// taken before it existed. It is the same predicate, called twice, not a second
// copy of it. (It still claims no atomicity: closing that needs a delete
// precondition on the object's UID, which the fake clientset cannot enforce —
// see deleteIfOwned.)
func (d *Driver) destroyNamespace(ctx context.Context, ns string) error {
	if err := d.checkNamespaceOwned(ctx, ns); err != nil {
		return err
	}
	if err := d.cfg.Client.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{}); err != nil &&
		!apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// upsertService is separate because a Service's ClusterIP is assigned by the
// API server and is immutable: a blind Update with an empty ClusterIP is
// rejected. The existing value has to be carried over — and the object it is
// carried over FROM has to be ours, for the reason upsertOwned states.
func (d *Driver) upsertService(ctx context.Context, ns string, svc *corev1.Service) error {
	c := d.cfg.Client
	_, err := c.CoreV1().Services(ns).Create(ctx, svc, metav1.CreateOptions{})
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	cur, err := c.CoreV1().Services(ns).Get(ctx, svc.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if !owned(cur.Labels) {
		return notManagedAction("service", svc.Name, ns, cur.Labels)
	}
	next := svc.DeepCopy()
	next.ResourceVersion = cur.ResourceVersion
	next.Spec.ClusterIP = cur.Spec.ClusterIP
	next.Spec.ClusterIPs = cur.Spec.ClusterIPs
	_, err = c.CoreV1().Services(ns).Update(ctx, next, metav1.UpdateOptions{})
	return err
}

// Scale implements provision.Provisioner.
func (d *Driver) Scale(ctx context.Context, ref provision.Ref, replicas int) error {
	if err := provision.CheckScale(d.Capabilities(), replicas); err != nil {
		return err
	}
	ns := d.namespaceFor(ref.Name)
	dep, err := d.getOwnedDeployment(ctx, ns, ref.Name)
	if err != nil {
		return err
	}
	n := int32(replicas)
	dep.Spec.Replicas = &n
	if _, err := d.cfg.Client.AppsV1().Deployments(ns).Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
		return blind("scale deployment "+ref.Name, err)
	}
	return nil
}

// Destroy implements provision.Provisioner.
//
// 🔴 THE nil IS VERIFIED, NOT ASSUMED. After the deletes, the Deployment is
// re-read: if it is still there, this returns an error rather than a nil that
// means "I issued some requests". The contract says nil means removed or
// already absent, and this is what makes that a measurement.
//
// 🔴 EVERY DELETE IS OWNERSHIP-CHECKED, AND THE DEPLOYMENT IS CHECKED FIRST.
// The objects are named after the instance, and a name is not an identity: in a
// shared namespace a Service, a ConfigMap or a ServiceAccount already holding
// the instance's name belongs to whoever made it. Deleting by name alone
// destroys a stranger's objects and reports success. A foreign DEPLOYMENT stops
// the teardown entirely rather than being skipped, because proceeding would
// tidy up around a workload muster does not own.
//
// 🔴 A FOREIGN OBJECT'S REFUSAL IS provision.ErrNotManaged AND DELIBERATELY NOT
// ErrNotFound, so `if err != nil && !errors.Is(err, provision.ErrNotFound)` —
// the idiom this contract invites — cannot discard it. The refusal for the
// DEPLOYMENT and the refusal for the per-instance NAMESPACE now agree; they did
// not, and checkNamespaceOwned says what changed and why the co-named satellites
// are a different case.
//
// 🔴 BOTH IDENTITY ANCHORS ARE CHECKED BEFORE ANYTHING IS DELETED, so a refusal
// and a teardown are never the same call. Either muster owns the name and
// removes everything under it, or it refuses and removes NOTHING; there is no
// third outcome where the instance is gone AND the call reports a permanent
// failure. That third outcome is what a late namespace check produced, it was
// measured over three consecutive Destroys, and a caller could not converge out
// of it. The ErrNotManaged branch is terminal by design: it is a refusal about
// the cluster's state, and retrying it will return the same answer until an
// operator relabels or removes the object in the way.
//
// ⚠ ONE CASE ESCAPES THAT GUARANTEE, AND IT IS NAMED RATHER THAN PAPERED OVER:
// if the namespace is REPLACED between the pre-flight and the delete, the
// re-read in destroyNamespace refuses after the objects are gone — the state
// genuinely changed under us, so the report is accurate, but a caller sees the
// removed-then-refused shape again. Closing it needs a delete precondition on
// the object's UID, which the fake clientset cannot enforce (see deleteIfOwned),
// so it would be code no test here could exercise. It is a race window, not the
// ordinary path the measurement above found.
//
// ⚠ WHAT IT DOES NOT CLAIM: that everything has finished TERMINATING. A
// namespace deletion is asynchronous and pods linger. nil means the API has
// accepted removal and the Deployment is gone from the API's view.
func (d *Driver) Destroy(ctx context.Context, ref provision.Ref) error {
	ns := d.namespaceFor(ref.Name)
	c := d.cfg.Client
	name := ref.Name

	// Every object is attempted regardless of what came before. A failure to
	// delete one must not leave the pod running — the security-relevant order
	// is "stop the workload first, tidy after", which is why the Deployment
	// leads.
	var firstErr error
	fail := func(what string, err error) {
		if err == nil || apierrors.IsNotFound(err) {
			return
		}
		d.log.Printf("k8s: destroy %s: delete %s: %v", ref.Name, what, err)
		if firstErr == nil {
			firstErr = fmt.Errorf("delete %s: %w", what, err)
		}
	}

	// 🔴 THE NAMESPACE ANCHOR, PRE-FLIGHT. See checkNamespaceOwned: deciding
	// this at the END of the teardown is what made a refusal and a completed
	// removal indistinguishable.
	if d.cfg.NamespacePerInstance {
		if err := d.checkNamespaceOwned(ctx, ns); err != nil {
			return fmt.Errorf("refusing to destroy %q: %w", name, err)
		}
	}

	var foreignDep *notManagedError
	switch dep, err := d.getOwnedDeployment(ctx, ns, name); {
	case err == nil:
		fail("deployment", c.AppsV1().Deployments(ns).Delete(ctx, dep.Name, metav1.DeleteOptions{}))
	case errors.As(err, &foreignDep):
		// 🔴 forAction, NOT THE REFUSAL AS RECEIVED. getOwnedDeployment is the
		// shared BY-NAME READ, so its refusal answers to ErrNotFound — correct
		// for Get, fatal here: `if err != nil && !errors.Is(err,
		// provision.ErrNotFound)` is the idiom Destroy's own contract invites,
		// and it discarded this refusal in silence, which is the opposite of
		// being loud. Re-flavoured, it answers to ErrNotManaged alone.
		return fmt.Errorf("refusing to destroy %q: %w", name, foreignDep.forAction())
	case errors.Is(err, provision.ErrNotFound):
		// Already gone. The rest of the teardown still runs: an earlier
		// Destroy may have failed partway.
	default:
		return err
	}

	for _, o := range []struct {
		what string
		name string
		get  func() (map[string]string, error)
		del  func() error
	}{
		{"service", name,
			func() (map[string]string, error) {
				return labelsOf(c.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{}))
			},
			func() error { return c.CoreV1().Services(ns).Delete(ctx, name, metav1.DeleteOptions{}) }},
		{"configmap", configMapName(name),
			func() (map[string]string, error) {
				return labelsOf(c.CoreV1().ConfigMaps(ns).Get(ctx, configMapName(name), metav1.GetOptions{}))
			},
			func() error {
				return c.CoreV1().ConfigMaps(ns).Delete(ctx, configMapName(name), metav1.DeleteOptions{})
			}},
		{"secret", envSecretName(name),
			func() (map[string]string, error) {
				return labelsOf(c.CoreV1().Secrets(ns).Get(ctx, envSecretName(name), metav1.GetOptions{}))
			},
			func() error {
				return c.CoreV1().Secrets(ns).Delete(ctx, envSecretName(name), metav1.DeleteOptions{})
			}},
		{"secret", fileSecretName(name),
			func() (map[string]string, error) {
				return labelsOf(c.CoreV1().Secrets(ns).Get(ctx, fileSecretName(name), metav1.GetOptions{}))
			},
			func() error {
				return c.CoreV1().Secrets(ns).Delete(ctx, fileSecretName(name), metav1.DeleteOptions{})
			}},
		{"pvc", pvcName(name),
			func() (map[string]string, error) {
				return labelsOf(c.CoreV1().PersistentVolumeClaims(ns).Get(ctx, pvcName(name), metav1.GetOptions{}))
			},
			func() error {
				return c.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, pvcName(name), metav1.DeleteOptions{})
			}},
		{"serviceaccount", name,
			func() (map[string]string, error) {
				return labelsOf(c.CoreV1().ServiceAccounts(ns).Get(ctx, name, metav1.GetOptions{}))
			},
			func() error { return c.CoreV1().ServiceAccounts(ns).Delete(ctx, name, metav1.DeleteOptions{}) }},
	} {
		fail(o.what+" "+o.name, d.deleteIfOwned(o.what, o.name, o.get, o.del))
	}

	// 🔴 THE NetworkPolicy GOES AFTER EVERYTHING IT CONFINES. The Deployment's
	// delete was issued first, but its pod is still terminating; removing the
	// policy ahead of it would hand a shell-capable process its last seconds
	// unconfined.
	//
	// ⚠ "AFTER" IS AN ORDER OF REQUESTS, NOT A WAIT. Nothing here blocks until
	// the pod is gone, so a pod that outlives this call's remaining requests can
	// still see the policy removed under it.
	//
	// ⚠ ONLY WHEN THE DRIVER IS CONFIGURED FOR ISOLATION, for the reason
	// applyNetworkPolicy never reads without it: a deployment that enables no
	// isolated kind holds no RBAC on NetworkPolicies, and an unconditional read
	// here would fail the destroy of every instance it has. Two costs, named:
	//
	//   - a policy written while isolation was configured outlives a Destroy
	//     issued after it was unconfigured, in the SHARED-namespace layout. (Under
	//     NamespacePerInstance the namespace deletion below takes it.)
	//   - on a driver that IS configured, this read happens for EVERY instance,
	//     isolated or not: Destroy has no spec to consult. So there, a missing
	//     networkpolicies rule fails the destroy of an instance that never had a
	//     policy — after its Deployment's delete was issued, and loudly.
	//     TestDestroyDoesNotReportSuccessOverAPolicyItCouldNotRemove pins both.
	if d.cfg.NetworkPolicy != nil {
		npName := networkPolicyName(name)
		np := c.NetworkingV1().NetworkPolicies(ns)
		err := d.deleteIfOwned("networkpolicy", npName,
			func() (map[string]string, error) {
				return labelsOf(np.Get(ctx, npName, metav1.GetOptions{}))
			},
			func() error { return np.Delete(ctx, npName, metav1.DeleteOptions{}) })
		if apierrors.IsForbidden(err) {
			// The apiserver's own text names the verb; this adds the rule to
			// write, because the instance being destroyed may never have had a
			// policy and "forbidden: networkpolicies" then reads as a non sequitur.
			// Conditionally, for networkPolicyForbidden's reason: a 403 is usually
			// RBAC and this did not check.
			err = fmt.Errorf("%w — if this refusal is about RBAC, %s", err, networkPolicyRBACHint)
		}
		fail("networkpolicy "+npName, err)
	}

	if d.cfg.NamespacePerInstance {
		fail("namespace", d.destroyNamespace(ctx, ns))
	}

	if firstErr != nil {
		return fmt.Errorf("destroy %q did not complete: %w", ref.Name, firstErr)
	}

	// The verification. A driver whose Destroy is `return nil` passes every
	// other assertion anyone writes about it.
	_, err := c.AppsV1().Deployments(ns).Get(ctx, ref.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return blind("verify destroy of "+ref.Name, err)
	default:
		return fmt.Errorf("destroy %q reported no error but the deployment is still present; "+
			"nil from Destroy means removed or already absent", ref.Name)
	}
}

// List implements provision.Provisioner.
//
// 🔴 IT RETURNS AN ERROR, NEVER AN EMPTY SLICE, WHEN THE API CANNOT BE REACHED.
// Both reads below go through blind(). The caller's fallback to stored status
// depends on being able to tell "nothing is running" from "I cannot see".
func (d *Driver) List(ctx context.Context) ([]provision.Instance, error) {
	ns := d.listNamespace()
	deps, err := d.cfg.Client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{LabelSelector: managedSelector()})
	if err != nil {
		return nil, blind("list deployments", err)
	}
	pods, err := d.cfg.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: managedSelector()})
	if err != nil {
		return nil, blind("list pods", err)
	}

	byInstance := map[string][]corev1.Pod{}
	for i := range pods.Items {
		p := &pods.Items[i]
		byInstance[p.Labels[labelInstance]] = append(byInstance[p.Labels[labelInstance]], *p)
	}

	out := make([]provision.Instance, 0, len(deps.Items))
	for i := range deps.Items {
		dep := &deps.Items[i]
		out = append(out, d.instanceFrom(dep, byInstance[dep.Labels[labelInstance]]))
	}
	return out, nil
}

// listNamespace is metav1.NamespaceAll when the driver owns a namespace per
// instance, and the configured namespace otherwise.
func (d *Driver) listNamespace() string {
	if d.cfg.NamespacePerInstance {
		return metav1.NamespaceAll
	}
	return d.cfg.Namespace
}

// Get implements provision.Provisioner.
//
// 🔴 IT REFUSES A WORKLOAD THIS DRIVER DID NOT CREATE, rather than describing
// it as an instance. A Deployment is fetched BY NAME, so without the ownership
// check inside getOwnedDeployment any workload sharing an instance's name — a
// chart release, somebody else's app — comes back as a muster Instance, with a
// phase, a pod name and a restart count that belong to a stranger.
func (d *Driver) Get(ctx context.Context, ref provision.Ref) (provision.Instance, error) {
	ns := d.namespaceFor(ref.Name)
	dep, err := d.getOwnedDeployment(ctx, ns, ref.Name)
	if err != nil {
		return provision.Instance{}, err
	}
	pods, err := d.cfg.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: instanceSelector(ref.Name)})
	if err != nil {
		return provision.Instance{}, blind("list pods for "+ref.Name, err)
	}
	return d.instanceFrom(dep, pods.Items), nil
}

// instanceFrom folds a Deployment and its pods into one Instance.
func (d *Driver) instanceFrom(dep *appsv1.Deployment, pods []corev1.Pod) provision.Instance {
	inst := provision.Instance{
		Ref:    provision.Ref{Name: dep.Labels[labelInstance]},
		Driver: d.Driver(),
		Group:  dep.Namespace,
		Phase:  provision.PhasePending,
	}
	if inst.Ref.Name == "" {
		inst.Ref.Name = dep.Name
	}
	if raw := dep.Annotations[annCorrelationID]; raw != "" {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
			inst.Ref.ID = id
		}
	}
	inst.Replicas = int(dep.Status.ReadyReplicas)

	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	if desired == 0 {
		inst.Phase = provision.PhaseStopped
		inst.Reason = "scaled to zero"
		inst.Replicas = 0
		return inst
	}

	pod := newestPod(pods)
	if pod == nil {
		inst.Reason = "no pod yet"
		return inst
	}
	inst.InstanceID = pod.Name
	inst.Phase = phaseOf(pod)

	ready := len(pod.Status.ContainerStatuses) > 0
	for _, cs := range pod.Status.ContainerStatuses {
		inst.Restarts += cs.RestartCount
		if !cs.Ready {
			ready = false
		}
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			inst.Reason = cs.State.Waiting.Reason
		}
		// 🔴 LastTerminationState, NOT State. This is the field that says WHY a
		// container that is running RIGHT NOW last died. An out-of-memory kill
		// is invisible everywhere else once the kubelet restarts it: the pod
		// reads Running and Ready with a bumped restart count and nothing says
		// what happened. It is the only signal that lets an instance report
		// that it is simply too small for the repository it was given.
		if cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.Reason != "" {
			inst.RestartReason = cs.LastTerminationState.Terminated.Reason
		}
	}
	inst.Ready = ready
	return inst
}

// newestPod picks the pod to report on: the most recently created one, so a
// rolling update reports the replacement rather than the pod on its way out.
func newestPod(pods []corev1.Pod) *corev1.Pod {
	var best *corev1.Pod
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if best == nil || p.CreationTimestamp.Time.After(best.CreationTimestamp.Time) {
			best = p
		}
	}
	if best == nil && len(pods) > 0 {
		best = &pods[0]
	}
	return best
}

func phaseOf(p *corev1.Pod) provision.Phase {
	switch p.Status.Phase {
	case corev1.PodRunning:
		return provision.PhaseRunning
	case corev1.PodPending:
		return provision.PhasePending
	case corev1.PodSucceeded:
		return provision.PhaseSucceeded
	case corev1.PodFailed:
		return provision.PhaseFailed
	default:
		return provision.PhaseUnknown
	}
}

// Endpoint implements provision.Provisioner, honouring the per-instance
// override recorded at create time.
func (d *Driver) Endpoint(ctx context.Context, ref provision.Ref) (provision.Endpoint, error) {
	ns := d.namespaceFor(ref.Name)
	dep, err := d.getOwnedDeployment(ctx, ns, ref.Name)
	if err != nil {
		return provision.Endpoint{}, err
	}

	var override *provision.Endpoint
	if raw := dep.Annotations[annEndpoint]; raw != "" {
		var ep provision.Endpoint
		if err := json.Unmarshal([]byte(raw), &ep); err == nil && !ep.IsZero() {
			override = &ep
		} else {
			// A malformed override is reported, not silently ignored: silently
			// falling back to the template is how an operator's explicit
			// instruction disappears.
			d.log.Printf("k8s: %s has an unreadable %s annotation (%q); falling back to the template",
				ref.Name, annEndpoint, raw)
		}
	}

	port := 0
	if raw := dep.Annotations[AnnotationPort]; raw != "" {
		port, _ = strconv.Atoi(raw)
	}

	return provision.ResolveEndpoint(
		override, d.tmpl,
		provision.EndpointVars{Name: ref.Name, Group: ns, ID: ref.ID},
		port, d.cfg.EndpointScheme,
	)
}

// podFor resolves the pod to read logs from.
//
// ⚠ IT GOES THROUGH THE OWNERSHIP CHECK TOO. Reading the logs is reading the
// workload's output, and a foreign workload's output is not muster's to serve.
func (d *Driver) podFor(ctx context.Context, ref provision.Ref) (string, string, error) {
	ns := d.namespaceFor(ref.Name)
	if _, err := d.getOwnedDeployment(ctx, ns, ref.Name); err != nil {
		return "", "", err
	}
	pods, err := d.cfg.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: instanceSelector(ref.Name)})
	if err != nil {
		return "", "", blind("list pods for "+ref.Name, err)
	}
	pod := newestPod(pods.Items)
	if pod == nil {
		return ns, "", nil
	}
	return ns, pod.Name, nil
}

// TailLogs implements provision.Provisioner.
//
// An instance with no pod yet returns "" and no error: "there is nothing to
// read yet" is not a failure. An instance that does not exist is ErrNotFound.
func (d *Driver) TailLogs(ctx context.Context, ref provision.Ref, lines int64) (string, error) {
	ns, pod, err := d.podFor(ctx, ref)
	if err != nil {
		return "", err
	}
	if pod == "" {
		return "", nil
	}
	opts := &corev1.PodLogOptions{
		// 🔴 THE CONTAINER IS NAMED. The project this came from never passed
		// one, so every log read silently returned whichever container happened
		// to be first — which was fine until the day it was not.
		Container: containerName,
	}
	if lines > 0 {
		opts.TailLines = &lines
	}
	rc, err := d.cfg.Client.CoreV1().Pods(ns).GetLogs(pod, opts).Stream(ctx)
	if err != nil {
		return "", blind("stream logs for "+ref.Name, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return "", fmt.Errorf("read logs for %s: %w", ref.Name, err)
	}
	return strings.TrimRight(string(b), "\n"), nil
}

// StreamLogs implements provision.Provisioner.
func (d *Driver) StreamLogs(ctx context.Context, ref provision.Ref, emit func(string)) error {
	ns, pod, err := d.podFor(ctx, ref)
	if err != nil {
		return err
	}
	if pod == "" {
		return nil
	}
	tail := int64(100)
	rc, err := d.cfg.Client.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{
		Container: containerName,
		Follow:    true,
		TailLines: &tail,
	}).Stream(ctx)
	if err != nil {
		return blind("stream logs for "+ref.Name, err)
	}
	defer rc.Close()
	sc := bufio.NewScanner(rc)
	// A log line can be long. The default 64KiB token limit turns one long line
	// into a stream that stops with a "token too long" error, which reads as a
	// dead pod.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if emit != nil {
			emit(sc.Text())
		}
	}
	return sc.Err()
}

func short(fp string) string {
	if len(fp) > 12 {
		return fp[:12]
	}
	if fp == "" {
		return "(none)"
	}
	return fp
}
