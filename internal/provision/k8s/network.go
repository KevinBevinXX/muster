package k8s

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/ZacxDev/muster/internal/provision"
)

// NetworkPolicyConfig is what the driver needs to know about the CLUSTER to turn
// a [provision.Network] into a NetworkPolicy. Its presence on [Config] is what
// raises Capabilities.NetworkIsolation.
//
// 🔴 A NetworkPolicy IS A DECLARATION, AND THIS DRIVER DOES NOT ENFORCE IT. The
// cluster's network plugin does, or does not: on a cluster whose plugin
// implements no NetworkPolicy the object below is accepted by the apiserver and
// changes nothing. Nothing in this package can observe which cluster it is on.
// Setting this field is the operator's statement that the plugin enforces.
type NetworkPolicyConfig struct {
	// ControllerNamespace is the namespace muster itself runs in — the ONLY
	// namespace an isolated instance accepts connections from. REQUIRED.
	ControllerNamespace string
	// ControllerPodLabels select muster's own pods inside that namespace.
	// REQUIRED and non-empty: an empty pod selector selects EVERY pod in the
	// namespace, which would admit muster's database and its backup jobs as
	// callers of an agent.
	ControllerPodLabels map[string]string
	// DNSNamespace and DNSPodLabels select the cluster's DNS pods. Empty means
	// [DefaultDNSNamespace] and [DefaultDNSPodLabels].
	DNSNamespace string
	DNSPodLabels map[string]string
}

// The cluster DNS selector used when [NetworkPolicyConfig] names none: the
// namespace and label CoreDNS carries on kubeadm, k3s and most managed clusters.
//
// ⚠ A POLICY PORT IS MATCHED AGAINST THE DNS POD'S OWN PORT, not the Service's
// (the rule is evaluated after the Service address is translated), and this
// renders 53. A cluster whose DNS pods listen elsewhere, or that resolves through
// a node-local cache on a link-local address, gets an instance that cannot
// resolve a name — a loud failure, in the closed direction.
const DefaultDNSNamespace = "kube-system"

// DefaultDNSPodLabels is the pod half of the default DNS selector.
var DefaultDNSPodLabels = map[string]string{"k8s-app": "kube-dns"}

// labelNamespaceName is the label the apiserver sets on every namespace, naming
// it. It is what lets a NetworkPolicy select a namespace BY NAME without anyone
// having to label it.
const labelNamespaceName = "kubernetes.io/metadata.name"

// dnsPort is the port name resolution is allowed on, UDP and TCP.
const dnsPort = 53

// nonPublicIPv4 is every IPv4 range an isolated instance's "public" egress
// EXCLUDES.
//
// 🔴 THE PORT ALONE IS NOT THE CONTROL, AND THIS LIST IS WHY. Inside a cluster
// the Kubernetes API is a ClusterIP on 443, so "TCP 443 to anywhere" leaves the
// apiserver reachable, along with every in-cluster HTTPS service and every HTTPS
// endpoint on the operator's LAN. Each range below is one of those:
//
//	10.0.0.0/8      RFC 1918 — and where pod and service CIDRs usually live
//	172.16.0.0/12   RFC 1918
//	192.168.0.0/16  RFC 1918 — the usual home/office LAN, and the node's own address
//	169.254.0.0/16  link-local — cloud metadata endpoints, node-local DNS caches
//	100.64.0.0/10   RFC 6598 shared address space — overlay and mesh networks
//
// ⚠ WHAT IS "OFF THE CLUSTER" DEPENDS ON THE CLUSTER'S ADDRESSES, AND NOTHING
// HERE READS THEM. Three shapes this list does not cover:
//
//   - a pod or service CIDR outside these ranges: in-cluster addresses would
//     count as public;
//   - an apiserver whose ENDPOINT is a public address on an allowed port (some
//     managed clusters'): the Service address is translated to it before the
//     policy is evaluated, so the API stays reachable;
//   - anything in-cluster that is ALSO published on a public address — an
//     ingress or load balancer — which is reachable there like any other
//     public host.
//
// Sorted, so the rendered object is byte-stable.
var nonPublicIPv4 = []string{
	"10.0.0.0/8",
	"100.64.0.0/10",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.168.0.0/16",
}

// publicIPv4 is the block the exceptions above are cut from.
const publicIPv4 = "0.0.0.0/0"

// networkPolicyName is the per-instance NetworkPolicy's name. A function, like
// the name functions in render.go and for the reason stated there: the render
// and Destroy have to agree on it.
func networkPolicyName(instance string) string { return instance + "-network" }

// validate refuses a configuration that would render a policy admitting more
// than it says.
func (c *NetworkPolicyConfig) validate() error {
	if strings.TrimSpace(c.ControllerNamespace) == "" {
		return errors.New("k8s: Config.NetworkPolicy.ControllerNamespace is required (it is the only namespace " +
			"an isolated instance accepts connections from)")
	}
	if len(c.ControllerPodLabels) == 0 {
		return errors.New("k8s: Config.NetworkPolicy.ControllerPodLabels is required and must not be empty " +
			"(an empty pod selector selects every pod in the controller's namespace)")
	}
	for k, v := range c.ControllerPodLabels {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			return fmt.Errorf("k8s: Config.NetworkPolicy.ControllerPodLabels has an empty key or value (%q=%q)", k, v)
		}
	}
	return nil
}

func (c *NetworkPolicyConfig) dnsNamespace() string {
	if c.DNSNamespace != "" {
		return c.DNSNamespace
	}
	return DefaultDNSNamespace
}

func (c *NetworkPolicyConfig) dnsPodLabels() map[string]string {
	if len(c.DNSPodLabels) > 0 {
		return copyLabels(c.DNSPodLabels)
	}
	return copyLabels(DefaultDNSPodLabels)
}

func copyLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// renderNetworkPolicy translates [provision.Network] for one instance. It returns
// nil when the spec declares no isolation.
//
// The policy, in words:
//
//   - it selects the instance's own pods, by the labels every pod of this
//     instance already carries (instanceLabels) — the same selector its Service
//     and its Deployment use;
//   - INGRESS: the spec's declared ports, from pods matching ControllerPodLabels
//     in ControllerNamespace. The two selectors are ONE peer, so they are ANDed:
//     written as two peers they would be ORed, and would admit every pod in the
//     controller's namespace plus every namespace's pods carrying those labels;
//   - EGRESS: UDP and TCP 53 to the cluster's DNS pods, each of the spec's
//     PublicEgressTCPPorts to [publicIPv4] except [nonPublicIPv4], and each of its
//     ControllerEgressTCPPorts to the controller's pods (the ingress peer).
//
// 🔴 BOTH policyTypes ARE ALWAYS LISTED, WHATEVER THE RULES. A policy that lists
// only the types it happens to have rules for leaves the other direction
// unrestricted: an instance with no declared port would get no Ingress type, and
// "nothing may reach it" would be rendered as "anything may".
//
// 🔴 THERE IS NO IPv6 RULE, WHICH DENIES IPv6 EGRESS, AND THAT IS THE DECISION.
// An ipBlock matches one address family, so the IPv4 block above says nothing
// about IPv6, and with the Egress type listed anything no rule allows is refused.
// Allowing `::/0` minus the private ranges would be a second allow-list whose
// exceptions nothing here could exercise on an IPv4-only cluster; everything an
// isolated instance is meant to reach is reachable over IPv4.
//
// ⚠ PROBES ARE NOT IN THE POLICY. The kubelet probes the pod from its own node,
// and whether node-originated traffic is subject to a NetworkPolicy is the
// network plugin's rule, not the API's.
func (d *Driver) renderNetworkPolicy(spec provision.Spec, ns string) (*networkingv1.NetworkPolicy, error) {
	if !spec.Network.Isolate {
		return nil, nil
	}
	cfg := d.cfg.NetworkPolicy
	if cfg == nil {
		// Unreachable through checkSpec: Capabilities.NetworkIsolation is false
		// exactly when this is nil, and CheckSpec refuses the spec first. It is an
		// error rather than a nil policy because the caller reads nil as "this
		// instance asks for none", and that is the one misreading that starts a
		// pod unconfined.
		return nil, fmt.Errorf("%w: the spec asks for network isolation and this driver has no "+
			"Config.NetworkPolicy to render it from", provision.ErrUnsupported)
	}

	ingressPorts := make([]networkingv1.NetworkPolicyPort, 0, len(spec.Ports))
	for _, p := range spec.Ports {
		proto := corev1.ProtocolTCP
		if strings.EqualFold(p.Protocol, "UDP") {
			proto = corev1.ProtocolUDP
		}
		port := intstr.FromInt32(int32(p.Port))
		ingressPorts = append(ingressPorts, networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &port})
	}
	sort.Slice(ingressPorts, func(i, j int) bool {
		if ingressPorts[i].Port.IntVal != ingressPorts[j].Port.IntVal {
			return ingressPorts[i].Port.IntVal < ingressPorts[j].Port.IntVal
		}
		return *ingressPorts[i].Protocol < *ingressPorts[j].Protocol
	})

	// 🔴 NO DECLARED PORT MEANS NO INGRESS RULE AT ALL, NOT A RULE WITH NO PORTS.
	// A NetworkPolicy rule whose `ports` is empty matches EVERY port, so rendering
	// the peer with an empty list would open the whole pod to the controller.
	ingress := []networkingv1.NetworkPolicyIngressRule{}
	if len(ingressPorts) > 0 {
		ingress = append(ingress, networkingv1.NetworkPolicyIngressRule{
			From: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{labelNamespaceName: cfg.ControllerNamespace},
				},
				PodSelector: &metav1.LabelSelector{MatchLabels: copyLabels(cfg.ControllerPodLabels)},
			}},
			Ports: ingressPorts,
		})
	}

	udp, tcp := corev1.ProtocolUDP, corev1.ProtocolTCP
	dns := intstr.FromInt32(dnsPort)
	egress := []networkingv1.NetworkPolicyEgressRule{{
		To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{labelNamespaceName: cfg.dnsNamespace()},
			},
			PodSelector: &metav1.LabelSelector{MatchLabels: cfg.dnsPodLabels()},
		}},
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &udp, Port: &dns},
			{Protocol: &tcp, Port: &dns},
		},
	}}

	// Same rule as ingress, for the same reason: no public port, no public rule.
	if len(spec.Network.PublicEgressTCPPorts) > 0 {
		public := append([]int(nil), spec.Network.PublicEgressTCPPorts...)
		sort.Ints(public)
		ports := make([]networkingv1.NetworkPolicyPort, 0, len(public))
		for _, p := range public {
			port := intstr.FromInt32(int32(p))
			proto := corev1.ProtocolTCP
			ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &port})
		}
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{
				IPBlock: &networkingv1.IPBlock{
					CIDR:   publicIPv4,
					Except: append([]string(nil), nonPublicIPv4...),
				},
			}},
			Ports: ports,
		})
	}

	// Controller egress: the instance may open these ports on muster's own pods —
	// the SAME one-peer selector the ingress rule uses (namespace AND pod labels),
	// so it reaches muster and not its database or anything else in that
	// namespace. No port, no rule, for the reason given above.
	if len(spec.Network.ControllerEgressTCPPorts) > 0 {
		ctrl := append([]int(nil), spec.Network.ControllerEgressTCPPorts...)
		sort.Ints(ctrl)
		ports := make([]networkingv1.NetworkPolicyPort, 0, len(ctrl))
		for _, p := range ctrl {
			port := intstr.FromInt32(int32(p))
			proto := corev1.ProtocolTCP
			ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &port})
		}
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{labelNamespaceName: cfg.ControllerNamespace},
				},
				PodSelector: &metav1.LabelSelector{MatchLabels: copyLabels(cfg.ControllerPodLabels)},
			}},
			Ports: ports,
		})
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      networkPolicyName(spec.Ref.Name),
			Namespace: ns,
			Labels:    mergeLabels(instanceLabels(spec.Ref.Name), spec.Labels),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: instanceLabels(spec.Ref.Name)},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: ingress,
			Egress:  egress,
		},
	}, nil
}

// NetworkPolicyRBACPrerequisite is every verb the muster ServiceAccount must hold
// on `networkpolicies.networking.k8s.io` for an isolated instance to be created,
// reconciled and destroyed. It is the set of calls this driver makes on that
// resource — applyNetworkPolicy's here, and Destroy's get and delete in
// driver.go — and nothing else: no list, no watch, no patch.
// TestTheRBACPrerequisiteIsTheVerbsTheDriverCalls measures it off an instance's
// whole life.
//
// ⚠ ON A DRIVER CONFIGURED FOR ISOLATION, `get` IS NEEDED TO DESTROY ANY
// INSTANCE, isolated or not. See Destroy.
//
// As a ClusterRole rule:
//
//   - apiGroups: ["networking.k8s.io"]
//     resources: ["networkpolicies"]
//     verbs: ["get", "create", "update", "delete"]
var NetworkPolicyRBACPrerequisite = []string{"get", "create", "update", "delete"}

// networkPolicyRBACHint is the rule to add, as one sentence for an error message.
var networkPolicyRBACHint = "grant muster's ClusterRole apiGroups [\"networking.k8s.io\"] resources " +
	"[\"networkpolicies\"] verbs [" + quotedVerbs(NetworkPolicyRBACPrerequisite) + "]"

// networkPolicyForbidden is the refusal for an apiserver 403 on the instance's
// NetworkPolicy.
//
// 🔴 IT IS NOT blind(), AND THE DIFFERENCE IS THE WHOLE POINT OF THE FUNCTION.
// Every other upsert in apply reports an apiserver failure as
// provision.ErrBlind — "backend unreachable", a transient a caller retries. A
// 403 here is neither: the backend answered, and what it said is that muster's
// write of the one object that confines this instance is not allowed. The
// sentinel is ErrUnsupported: the driver cannot do what the spec asks and is
// refusing rather than doing part of it.
//
// ⚠ A 403 IS USUALLY RBAC AND NOT ALWAYS. A ResourceQuota denial and an
// admission webhook's refusal are 403s too, and the fix for those is not a
// ClusterRole rule. So the message carries the apiserver's own text, names the
// verb muster attempted, and offers the rule conditionally — "if the refusal is
// about RBAC" — instead of asserting a cause it did not check. (One 403 never
// reaches here at all: a namespace still terminating is a transient, and
// applyNetworkPolicy routes it to applyFailed like every other object's.)
//
// 🔴 "NOT STARTED" IS ONLY HALF OF WHAT CAN BE TRUE, SO THE MESSAGE SAYS BOTH
// HALVES — AND CLAIMS NEITHER MORE THAN THIS FUNCTION KNOWS. apply returned
// here, having written at most the instance's (empty) namespace, so an instance
// that was not running is still not running. An instance that was ALREADY
// running is still running, exactly as it was — and whether THAT one has a
// policy, and a current one, is not something a refusal establishes:
//
//   - one created before its kind was isolated may have none, and a message that
//     said only "was NOT started" told its operator there was nothing left to
//     stop;
//   - one refused on `update` has muster's own policy, which was not updated;
//   - one refused on `get` has SOME object under the policy's name (the create
//     answered AlreadyExists) that muster could not read, so not even its
//     ownership is known;
//   - one refused on `create` may have either: the apiserver authorises before
//     it looks, so a 403 on create says nothing about what exists.
//
// So the message asserts none of those. It says the running instance was left as
// it was and gives a command that names the OBJECT — not a listing of the
// namespace, which in the shared layout holds every other agent's policy and
// would answer "there are some" for an agent that has none.
func networkPolicyForbidden(verb, name, ns string, err error) error {
	return fmt.Errorf("%w: the apiserver refused (403 Forbidden) muster's %s of networkpolicies.networking.k8s.io %q "+
		"in namespace %q: %v. This instance's NetworkPolicy was NOT written and neither was anything after it, so an "+
		"instance that was not running was NOT started. One that was ALREADY RUNNING was left exactly as it was, and "+
		"this refusal does not establish that it has a current policy — one created before network isolation existed "+
		"may have none. Check `kubectl -n %s get networkpolicy %s`: if that is not found and the agent is running, it is "+
		"running unconfined; stop it. If the refusal is about RBAC (the apiserver's text above names the ServiceAccount "+
		"and the verb), %s, then start the agent again (if the UI shows it running, stop it first)",
		provision.ErrUnsupported, verb, name, ns, err, ns, name, networkPolicyRBACHint)
}

func quotedVerbs(verbs []string) string {
	out := make([]string, 0, len(verbs))
	for _, v := range verbs {
		out = append(out, fmt.Sprintf("%q", v))
	}
	return strings.Join(out, ", ")
}

// applyNetworkPolicy upserts the instance's NetworkPolicy when the spec asks for
// isolation, and does NOTHING AT ALL when it does not.
//
// 🔴 apply CALLS THIS BEFORE IT WRITES ANYTHING THAT COULD RUN OR LEAK, AND
// RETURNS ON ITS ERROR. That order is the fail-closed guarantee: the Deployment
// is the last object apply writes, so a Deployment whose spec asked for
// isolation exists only if this returned nil first. It runs ahead of the Secret
// too, so a refused FIRST apply writes no credential — only, in the per-instance
// layout, the (empty) namespace.
//
// ⚠ "EXISTS ONLY IF THIS RETURNED nil" IS A CLAIM ABOUT THIS DRIVER'S WRITES. An
// instance created by a muster that predates this function has a Deployment and
// no policy, and stays that way until its next SUCCESSFUL Update: Scale does not
// come through apply, and Create on an existing Deployment refuses on the changed
// fingerprint rather than reconciling. A REFUSED Update of such an instance
// leaves it exactly as it was — running, if it was running — which
// networkPolicyForbidden's message says, with the command that checks it.
//
// An instance that already had a Secret keeps it, for the same reason.
//
// ⚠ THE CONTROLLER SELECTOR IS NOT IN THE SPEC, SO IT IS NOT IN THE FINGERPRINT.
// Changing Config.NetworkPolicy changes what the NEXT apply writes; an existing
// instance's policy keeps the old selector until then, and nothing reports the
// difference.
//
// 🔴 A SPEC THAT DECLARES NO ISOLATION MAKES NO networking.k8s.io CALL, NOT EVEN
// A READ, AND THAT IS WHY THERE IS NO STALE-POLICY SWEEP HERE — unlike the
// ConfigMap, the Secrets and the Service, which apply removes when a spec stops
// asking for them. Two reasons, either sufficient:
//
//   - a deployment that enables no isolated kind has no RBAC on NetworkPolicies
//     and should not need any. A sweep is a GET on every apply of every
//     instance, so it would turn a missing rule into a failed apply for agents
//     that never asked for a policy;
//   - a reconcile that REMOVES confinement because a spec stopped declaring it
//     is the unsafe direction for a silent default. The policy stays until
//     Destroy, or until an operator deletes it deliberately.
func (d *Driver) applyNetworkPolicy(ctx context.Context, spec provision.Spec, ns string) error {
	np, err := d.renderNetworkPolicy(spec, ns)
	if err != nil {
		return err
	}
	if np == nil {
		return nil
	}
	c := d.cfg.Client.NetworkingV1().NetworkPolicies(ns)

	// The verb in flight, so a 403 can name the one that was refused: upsertOwned
	// runs up to three calls and returns one error.
	verb := ""
	err = d.upsertOwned("networkpolicy", np.Name, ns,
		func() (map[string]string, error) {
			verb = "get"
			return labelsOf(c.Get(ctx, np.Name, metav1.GetOptions{}))
		},
		func() error {
			verb = "create"
			_, e := c.Create(ctx, np, metav1.CreateOptions{})
			return e
		},
		func() error {
			verb = "update"
			_, e := c.Update(ctx, np, metav1.UpdateOptions{})
			return e
		})
	switch {
	case err == nil:
		return nil
	case apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause):
		// 🔴 A 403, AND NOT A REFUSAL TO REPORT AS PERMANENT. The apiserver answers
		// Forbidden for a CREATE into a namespace that is still being deleted —
		// here, an instance re-created under a name whose previous namespace has
		// not finished terminating. It clears by itself, so it is ErrBlind like the
		// same failure on every other object apply writes; naming an RBAC rule for
		// it would send an operator to fix something that is not broken.
		//
		// ⚠ NOT OBSERVED ON A CLUSTER FROM THIS TREE. The cause type is the API's
		// own constant; the fake clientset has no namespace lifecycle, so the test
		// for this branch builds the error by hand.
		return applyFailed("apply networkpolicy "+np.Name, err)
	case apierrors.IsForbidden(err):
		return networkPolicyForbidden(verb, np.Name, ns, err)
	default:
		return applyFailed("apply networkpolicy "+np.Name, err)
	}
}
