package provision

import (
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Ref identifies one provisioned instance.
//
// 🔴 Name IS THE KEY. ID IS A CORRELATION HINT AND MAY BE ZERO. In the project
// muster was extracted from, the lifecycle methods took an `agentID int64`,
// loaded the row themselves, and decided from its contents what to do — which
// meant a second driver would have needed that project's Postgres schema to
// create a container. Ref is the fix: a driver may use ID to label or log, and
// MUST NOT require it. Every driver in this repository is exercised with ID
// zero by provisiontest.RunContract for exactly that reason.
type Ref struct {
	// Name is the instance's stable identity. It is expected to be
	// DNS-label-safe because several drivers use it directly as an object,
	// container or directory name; Validate enforces that rather than leaving
	// each driver to discover it.
	Name string
	// ID is an optional correlation id from whatever system asked for this
	// instance. Drivers may surface it in labels or logs. Nothing may depend on
	// it being non-zero, and nothing may look it up.
	ID int64
}

// String renders the ref for logs. The id is included only when set, so a log
// line does not carry a meaningless zero.
func (r Ref) String() string {
	if r.ID == 0 {
		return r.Name
	}
	return fmt.Sprintf("%s(#%d)", r.Name, r.ID)
}

// maxNameLen is the longest instance name a driver may be handed.
//
// 63 is the DNS label limit, and several drivers build names by SUFFIXING the
// instance name, so the budget has to leave room. 48 leaves 15 characters for a
// suffix, which fits every suffix this package's drivers use with room spare.
const maxNameLen = 48

// dnsLabel is the permitted shape of Ref.Name: lowercase alphanumerics and
// dashes, starting and ending alphanumeric.
//
// ⚠ IT ADMITS A LEADING DIGIT, AND A KUBERNETES *SERVICE* NAME DOES NOT — a
// Service is DNS-1035 (must start alphabetic) while this is DNS-1123. So a name
// like "1agent" passes here and the apiserver would refuse the Service.
//
// 🔴 THE PATH WAS DEAD AND IS NOW LIVE, WHICH IS WHY THIS NOTE EXISTS: until
// agentspec declared a port, k8s renderService returned nil for every agent spec
// and no Service was ever created. It is created now, and apply upserts it BEFORE
// the Deployment — so such a name would fail mid-apply with the ServiceAccount,
// ConfigMap and Secret already written.
//
// ⚠ NOT REACHABLE TODAY, AND NO UNIT TEST CAN SEE IT. Every name reaching a
// dispatch is either agents.ChiefName or one drawn from generateName's pool, and
// both are all-alphabetic; k8s.io/client-go/kubernetes/fake validates no names at
// all, so the refusal only exists against a real apiserver. Tightening this to
// DNS-1035 would be the fix, and it is deliberately NOT done here: it narrows a
// contract every driver shares for a hazard one driver has, and the drivers that
// do not create Services would be refusing names they can serve.
func isDNSLabel(s string) bool {
	if s == "" || len(s) > maxNameLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-':
			if i == 0 || i == len(s)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// Validate reports whether the ref can be used as an instance identity.
func (r Ref) Validate() error {
	if !isDNSLabel(r.Name) {
		return fmt.Errorf("%w: name %q must be 1-%d chars of [a-z0-9-], not starting or ending with '-'",
			ErrInvalidSpec, r.Name, maxNameLen)
	}
	return nil
}

// EnvVar is one name/value pair handed to the instance's process.
type EnvVar struct {
	Name  string
	Value string
}

// File is a file to place inside the instance before its process starts.
//
// 🔴 THIS TYPE REPLACES A SHELL-QUOTING HAZARD, AND THAT IS THE WHOLE POINT OF
// IT BEING DATA. In the project muster was extracted from, every file placed
// into an agent travelled as
//
//	printf '%s' '<base64>' | base64 -d > /path
//
// appended to a list of init commands, wrapped in `{ … } || :` because the
// surrounding startup script ran under `set -e` and any bare non-zero exit was
// a crash loop rather than a missing file. An audit found three shapes where
// the guard had been missed. Every backend muster targets — a cluster, a
// container daemon, a directory — can place a file natively, so the content is
// carried here and the driver decides how. [Spec.Init] survives for genuinely
// imperative steps.
type File struct {
	// Path is the absolute in-instance path.
	Path string
	// Content is the literal bytes. Not a string: this carries binaries and
	// archives as readily as configuration.
	Content []byte
	// Mode is the file mode. Zero means "let the driver choose", which is 0644
	// for a plain file and 0600 when Secret is set — stated here rather than
	// left to each driver, because a secret that lands world-readable because
	// one driver defaulted differently is the kind of difference nobody looks
	// for.
	Mode fs.FileMode
	// Secret marks content that must not be visible in a non-confidential
	// store. A driver whose Capabilities.Secrets is false MUST refuse a spec
	// carrying one rather than downgrade it silently — see CheckSpec.
	Secret bool
}

// EffectiveMode resolves Mode's zero value per the rule stated on the field.
func (f File) EffectiveMode() fs.FileMode {
	if f.Mode != 0 {
		return f.Mode
	}
	if f.Secret {
		return 0o600
	}
	return 0o644
}

// Runtime is what the instance actually runs.
//
// 🔴 IT IS A UNION OF Image AND Command, AND BOTH HALVES ARE LOAD-BEARING. An
// image-only Runtime cannot express a local-process driver, which has no images
// at all; a command-only Runtime cannot express the container case, where the
// image supplies its own entrypoint and the caller wants exactly that. Requiring
// both would make every caller invent one of them. So: at least one must be set,
// which is what Validate checks, and a driver refuses the half it cannot honour
// through CheckSpec rather than by ignoring it.
type Runtime struct {
	// Image is a container image reference. Empty for a driver that runs a
	// command directly.
	Image string
	// Command overrides the image's entrypoint, or IS the thing to run when
	// there is no image.
	Command []string
	// Args are appended after Command (or after the image's entrypoint when
	// Command is empty).
	Args []string
	// WorkingDir is the process's cwd. Empty means the runtime's default.
	WorkingDir string
}

// Resources is the instance's compute budget. Values are strings in the units
// the driver understands ("512Mi", "2", "1500m"); they are passed through rather
// than parsed here, because the set of legal spellings is a property of the
// backend and a parser in this package would be a second, wrong authority.
//
// Empty fields mean "unset" and the driver applies no limit of its own. A driver
// whose Capabilities.ResourceLimits is false refuses a spec that sets any of
// them — silently running an agent with no ceiling is how a box gets taken out
// by one repository that was bigger than the last one.
type Resources struct {
	CPURequest    string
	CPULimit      string
	MemoryRequest string
	MemoryLimit   string
}

// IsZero reports whether no budget is expressed at all.
func (r Resources) IsZero() bool {
	return r.CPURequest == "" && r.CPULimit == "" && r.MemoryRequest == "" && r.MemoryLimit == ""
}

// Workspace is the instance's working storage.
type Workspace struct {
	// Path is where it is mounted. Empty means the driver's default.
	Path string
	// Size is the requested capacity ("20Gi"). Ignored when Persist is false.
	Size string
	// Persist asks for storage that outlives the instance's process. A driver
	// whose Capabilities.Persistence is false refuses a spec that sets it,
	// because "your work was saved" and "your work was in a tmpfs" are not a
	// difference to discover after a restart.
	Persist bool
}

// Repo is the source repository the instance is meant to work in.
//
// ⚠ NOTHING IN THIS PACKAGE CLONES IT, AND THAT IS A DELIBERATE NARROWING. In
// the project muster was extracted from, the deployment template cloned the
// repository itself, roughly 250 lines BEFORE the init commands ran — which is
// why that project's credential handling had to work in two different
// environments at once and grew a documented fallback to survive it. Here the
// repository is DECLARED, passed to the instance as environment, and cloned by
// the instance's own runtime or by an explicit Init step. The driver's job ends
// at making the declaration and the credential available.
type Repo struct {
	// URL is the clone URL. It must NOT carry credentials; a driver that finds
	// a userinfo component in it refuses the spec (see CheckSpec), because a
	// URL is the one field of a spec that reliably ends up in a log.
	URL string
	// Branch is the ref to check out. Empty means the repository's default.
	Branch string
	// Path is where the instance should expect the checkout. Empty means the
	// driver's default, under the workspace.
	Path string
}

// Port is a network port the instance listens on.
type Port struct {
	// Name identifies the port to Endpoint. The name muster's own callers look
	// for is DefaultPortName.
	Name string
	// Port is the container/process port number.
	Port int
	// Protocol is "TCP" or "UDP". Empty means TCP.
	Protocol string
}

// DefaultPortName is the port Endpoint resolves when a spec declares several and
// the caller did not say which.
const DefaultPortName = "gateway"

// Health is how a driver can tell a SERVING instance from a merely RUNNING one.
//
// 🔴 IT EXISTS BECAUSE Instance.Ready WAS A CLAIM NOTHING MEASURED. Phase and
// Ready are separate fields precisely so that "the process is up" and "it is
// serving" can disagree — and with no health declaration anywhere in a spec, no
// driver had anything to decide the second one from. Measured against the agent
// runtime image muster's own deployment pins: an instance whose gateway refuses
// to start (`Missing config`, exit 78) sat in CrashLoopBackOff reporting 0/1
// indefinitely, with nothing in the cluster timing it out and nothing in muster
// saying why. The crash was loud in `kubectl logs` and invisible everywhere a
// person actually looks.
//
// 🔴 IT DECLARES A PROPERTY OF THE INSTANCE, NOT A BEHAVIOUR ASKED OF THE
// DRIVER, WHICH IS WHY IT HAS NO [Capabilities] GATE. "This instance answers
// HTTP GET <path> on port <name> when it is serving" is a fact about what is
// inside, in the same way [Spec.Ports] is — and Ports has no capability bit
// either. A driver with no supervision concept ignores it; a driver that has one
// translates it into its own idiom, with its own timings, because a period and a
// failure threshold are shaped by the backend and a portable spelling of them
// here would be a second, wrong authority (the argument [Resources] makes for
// keeping quantities as strings).
//
// ⚠ THERE IS DELIBERATELY NO WAY TO SAY "DO NOT PROBE ME" OTHER THAN LEAVING
// HTTPGetPath EMPTY, and no separate readiness/liveness split. Both were
// considered and both are knobs whose only use is to make a non-serving instance
// look fine again, which is the state this type exists to end.
type Health struct {
	// HTTPGetPath is the path a serving instance answers 2xx/3xx on. Empty
	// means "this spec declares no health signal", and a driver then reports
	// Ready from whatever its backend says about the process alone.
	//
	// ⚠ IT IS A PATH AND NOT A URL. The scheme is the backend's business and
	// the host is the instance itself; a URL here would invite an absolute one
	// pointing somewhere else entirely.
	HTTPGetPath string
	// PortName names the [Port] to probe. It must match a declared port's Name
	// — [Spec.Validate] refuses a name that resolves to nothing, because the
	// alternative is a driver silently dropping the probe and reporting the
	// pre-Health behaviour under a spec that asked for better.
	PortName string
}

// IsZero reports whether the spec declares no health signal at all.
func (h Health) IsZero() bool { return h.HTTPGetPath == "" && h.PortName == "" }

// Security is the instance's process isolation, as a property of what runs
// inside it (the image's uid, whether it needs cluster credentials).
//
// 🔴 THE ZERO VALUE IS "DECLARE NOTHING", AND THAT IS WHAT EVERY SPEC CARRIED
// BEFORE THIS TYPE EXISTED. A driver renders a zero Security exactly as it
// rendered a spec with no such field, and [Fingerprint] writes nothing for it, so
// introducing the type moved no existing instance's fingerprint and rolled no pod.
//
// ⚠ A ZERO ID MEANS UNSET, NOT ROOT. Nothing muster builds asks to run as uid 0
// explicitly, so the type does not spend a pointer on expressing it.
type Security struct {
	// RunAsUser, RunAsGroup and FSGroup are numeric ids; 0 = unset.
	RunAsUser  int64
	RunAsGroup int64
	FSGroup    int64
	// RunAsNonRoot asks the backend to refuse to start the process as root.
	RunAsNonRoot bool
	// Restricted drops every capability, forbids privilege escalation and uses
	// the runtime's default seccomp profile.
	Restricted bool
	// NoServiceAccountToken keeps the backend's own API credential out of the
	// instance (Kubernetes: automountServiceAccountToken false).
	NoServiceAccountToken bool
	// ReadOnlyRoot makes the instance's root filesystem read-only, with a
	// writable, empty /tmp of its own (Kubernetes: readOnlyRootFilesystem and an
	// emptyDir at /tmp); the workspace stays writable. The hermes kind declares
	// it. ⚠ It enters the fingerprint on a line of its own, only when set, so no
	// spec that does not declare it moves.
	ReadOnlyRoot bool `json:",omitempty"`
}

// IsZero reports whether nothing is declared.
func (s Security) IsZero() bool { return s == Security{} }

// Network is the instance's network confinement, as a property of what runs
// inside it: who may reach it, and what it may reach.
//
// 🔴 THE ZERO VALUE IS "DECLARE NOTHING", FOR THE REASON [Security]'S IS. Every
// spec built before this type existed carried no such field; a driver renders a
// zero Network exactly as it rendered those, and [Fingerprint] writes nothing
// for it, so introducing the type moved no existing instance's fingerprint.
//
// 🔴 UNLIKE [Health], IT IS A BEHAVIOUR ASKED OF THE DRIVER, SO IT HAS A
// [Capabilities] GATE. A driver that ignored Isolate would start an instance
// that LOOKS confined — the declaration is on the spec, the card says the kind
// is the confined one — and can reach everything. [CheckSpec] refuses it
// instead (Capabilities.NetworkIsolation).
//
// ⚠ WHAT "PUBLIC" MEANS IS THE DRIVER'S TO SPELL, in its own address-range
// idiom, for the reason [Resources] keeps quantities as strings. What is
// portable is the intent: the instance may open TCP connections on these ports
// to the public internet, and to nothing private — not the backend's own
// control plane, not its neighbours, not the operator's LAN.
type Network struct {
	// Isolate asks the backend to confine the instance:
	//
	//   - inbound: only the instance's declared [Spec.Ports], and only from
	//     whatever the driver is configured to treat as its controller (muster
	//     itself);
	//   - outbound: name resolution, plus PublicEgressTCPPorts on public
	//     addresses. Nothing else.
	Isolate bool
	// PublicEgressTCPPorts are the TCP ports the instance may reach on public
	// addresses. Empty with Isolate set means "name resolution only".
	//
	// ⚠ SETTING IT WITHOUT Isolate IS REFUSED by [Spec.Validate]: a port list on
	// an unconfined instance is an allow-list that allows nothing it was not
	// already allowed, and reads like confinement.
	PublicEgressTCPPorts []int
	// ControllerEgressTCPPorts are the TCP ports the instance may reach on its
	// CONTROLLER — muster's own pods, selected exactly as the inbound rule selects
	// them. It exists for muster's agent-facing connector surface, which an
	// isolated agent reaches on a dedicated port; nothing else of muster's is
	// reachable through it.
	//
	// ⚠ SETTING IT WITHOUT Isolate IS REFUSED, for PublicEgressTCPPorts' reason.
	ControllerEgressTCPPorts []int
}

// IsZero reports whether nothing is declared.
func (n Network) IsZero() bool {
	return !n.Isolate && len(n.PublicEgressTCPPorts) == 0 && len(n.ControllerEgressTCPPorts) == 0
}

// Spec is the complete desired state of one instance.
//
// It is the genericised form of what the original project assembled as a map of
// chart values: the same information, minus every field that only one specific
// deployment template understood, plus Config for everything that does not
// generalise.
type Spec struct {
	// Ref is the instance's identity. Required.
	Ref Ref

	// Runtime is what runs. Required (see the type's own note on the union).
	Runtime Runtime

	// Env is non-sensitive environment.
	Env []EnvVar

	// Secrets is sensitive environment. Kept separate from Env rather than
	// flagged inside it so that a driver CANNOT accidentally treat the two the
	// same: the type system makes the confidential set enumerable.
	Secrets []EnvVar

	// Files are placed before the process starts. See File.
	Files []File

	// Init are imperative steps to run before the main process, in order.
	//
	// ⚠ KEEP THIS FOR THINGS THAT ARE GENUINELY IMPERATIVE. It is a shell
	// contract and therefore the least portable field in the spec: every entry
	// is a string some shell has to parse. Placing content belongs in Files,
	// where it is data and can be asserted on.
	Init []string

	// Resources is the compute budget.
	Resources Resources

	// Workspace is the working storage.
	Workspace Workspace

	// Repo is the declared source repository. See the type's note: declared,
	// not cloned.
	Repo Repo

	// Config is OPAQUE to the provisioner. Whatever runtime ends up inside the
	// instance gets it verbatim; no driver may interpret a key of it, and no
	// driver may fail because of its contents. It exists so that runtime-specific
	// configuration does not have to become provisioner-specific fields — the
	// exact pressure that grew the original project's value map to a shape only
	// one template could read.
	Config map[string]any

	// Ports the instance listens on.
	Ports []Port

	// Health is how a driver tells serving from running. See the type.
	Health Health

	// Security is the instance's process isolation. See the type.
	Security Security

	// Network is the instance's network confinement. See the type.
	Network Network

	// Endpoint, when non-nil, is the address callers should use to reach this
	// instance, OVERRIDING whatever the driver would compute.
	//
	// 🔴 THIS FIELD IS THE POINT OF THE WHOLE Endpoint DESIGN. The address in
	// the original project was a hardcoded format string with no override
	// anywhere — not an environment variable, not a config field, not a
	// per-instance one — so reaching an agent through anything other than that
	// project's own cluster DNS was not expressible. There are now three
	// layers: this, the driver's configurable template, and the driver's
	// default.
	Endpoint *Endpoint

	// Replicas is the desired instance count. Zero means one; use Scale to stop
	// something, which keeps "I did not think about replicas" and "I want this
	// stopped" from being the same value.
	Replicas int

	// Labels are attached to whatever objects the driver creates, for the
	// operator's own selection. Drivers add their own and MUST NOT let these
	// overwrite them.
	Labels map[string]string

	// Sidecar, when non-nil, is a second process run BESIDE the instance's own,
	// sharing its network and its workspace, and nothing else. See the type.
	//
	// ⚠ omitempty, so a spec without one serialises exactly as before the field
	// existed (agentspec's spec golden is a json.Marshal of a Spec).
	Sidecar *Sidecar `json:",omitempty"`

	// TerminationGraceSeconds is how long the instance is given to stop after
	// it is asked to (on Kubernetes the pod's terminationGracePeriodSeconds,
	// SIGTERM to SIGKILL). Zero is the driver's default (Kubernetes: 30 s).
	//
	// It exists for the hermes kind: Hermes drains for 30 s on SIGTERM before it
	// interrupts what is still running, so at the default the kubelet's SIGKILL
	// lands mid-drain and turns every in-flight run into an `unknown` one
	// (docs/design/hermes.md, "Restart and resume").
	//
	// ⚠ omitempty, like Sidecar, so every spec that declares none serialises as
	// before.
	TerminationGraceSeconds int `json:",omitempty"`
}

// Sidecar is a second long-running process in the same instance: on Kubernetes
// a second container in the same pod.
//
// 🔴 IT SHARES THE INSTANCE'S NETWORK NAMESPACE AND NOTHING ELSE, ON PURPOSE.
// Same network: the instance's own process reaches it on loopback, so a sidecar
// needs no port of its own and declares none — it is never exposed (no Service
// port, no NetworkPolicy ingress rule names it). NOT the workspace: the volume
// is the instance's, and whatever the sidecar writes there (a runtime's own
// credential store, say) would be readable by the instance's process; its state
// lives in its own container and is lost with the pod. Its environment,
// confidential environment and files are ITS OWN: the instance's Secrets are not
// exported into the sidecar, nor the sidecar's into the instance — each process
// holds only the credentials it uses. Security and Network are the instance's
// and apply to both processes.
type Sidecar struct {
	// Name names the container. A DNS label, and not the instance's own
	// container's name.
	Name string
	// Runtime is what the sidecar runs. Image is required: a sidecar is a
	// container.
	Runtime Runtime
	// Env, Secrets and Files are as Spec's, for the sidecar alone. File paths
	// must not repeat the instance's.
	Env     []EnvVar
	Secrets []EnvVar
	Files   []File
	// Resources is the sidecar's own budget.
	Resources Resources
}

// DesiredReplicas resolves Replicas' zero value per the rule stated on the
// field.
func (s Spec) DesiredReplicas() int {
	if s.Replicas <= 0 {
		return 1
	}
	return s.Replicas
}

// Validate checks the spec's internal consistency — the things that are wrong
// regardless of which driver receives it. Capability-dependent refusals are
// CheckSpec's job, and are separate on purpose: this one's answer does not
// change when you switch drivers.
// sidecarName is a DNS label (a container name).
var sidecarName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

func (s Spec) Validate() error {
	if err := s.Ref.Validate(); err != nil {
		return err
	}
	if s.TerminationGraceSeconds < 0 || s.TerminationGraceSeconds > 3600 {
		return fmt.Errorf("%w: termination grace %ds is outside 0..3600", ErrInvalidSpec, s.TerminationGraceSeconds)
	}
	if s.Runtime.Image == "" && len(s.Runtime.Command) == 0 {
		return fmt.Errorf("%w: runtime must set Image or Command (it is a union; neither is not a third option)", ErrInvalidSpec)
	}
	seen := map[string]bool{}
	for _, f := range s.Files {
		if !strings.HasPrefix(f.Path, "/") {
			return fmt.Errorf("%w: file path %q must be absolute", ErrInvalidSpec, f.Path)
		}
		if seen[f.Path] {
			return fmt.Errorf("%w: file path %q appears twice; which one wins is not a thing the caller should have to know", ErrInvalidSpec, f.Path)
		}
		seen[f.Path] = true
	}
	names := map[string]bool{}
	for _, p := range s.Ports {
		if p.Port < 1 || p.Port > 65535 {
			return fmt.Errorf("%w: port %q number %d out of range", ErrInvalidSpec, p.Name, p.Port)
		}
		if p.Name != "" && names[p.Name] {
			return fmt.Errorf("%w: port name %q appears twice", ErrInvalidSpec, p.Name)
		}
		names[p.Name] = true
	}
	// 🔴 BOTH-OR-NEITHER, AND THE PORT NAME IS MATCHED EXACTLY RATHER THAN
	// THROUGH PortNumber. PortNumber falls back to DefaultPortName and then to
	// "the single declared port", which is right for RESOLVING an address a
	// caller did not name — and wrong here: a Health naming a port that does not
	// exist would resolve to whatever the spec happens to declare, so a typo
	// would probe the wrong port and report an instance unhealthy for a reason no
	// log line carries. A path with no port, and a port with no path, are each
	// half a probe; a driver would have to invent the other half.
	if (s.Health.HTTPGetPath == "") != (s.Health.PortName == "") {
		return fmt.Errorf("%w: health declares %q/%q — a path and a port name are both required or both empty, "+
			"because a driver cannot invent the missing half",
			ErrInvalidSpec, s.Health.HTTPGetPath, s.Health.PortName)
	}
	if !s.Health.IsZero() {
		if !strings.HasPrefix(s.Health.HTTPGetPath, "/") {
			return fmt.Errorf("%w: health path %q must be absolute", ErrInvalidSpec, s.Health.HTTPGetPath)
		}
		if !names[s.Health.PortName] {
			return fmt.Errorf("%w: health names port %q, which this spec does not declare; a driver would "+
				"drop the probe and report the instance ready from its process state alone",
				ErrInvalidSpec, s.Health.PortName)
		}
	}
	if s.Security.RunAsUser < 0 || s.Security.RunAsGroup < 0 || s.Security.FSGroup < 0 {
		return fmt.Errorf("%w: security ids must not be negative (user %d, group %d, fsGroup %d)",
			ErrInvalidSpec, s.Security.RunAsUser, s.Security.RunAsGroup, s.Security.FSGroup)
	}
	if !s.Network.Isolate && len(s.Network.PublicEgressTCPPorts) > 0 {
		return fmt.Errorf("%w: network names public egress ports %v without Isolate; on an unconfined instance "+
			"that list restricts nothing and reads as if it did", ErrInvalidSpec, s.Network.PublicEgressTCPPorts)
	}
	egress := map[int]bool{}
	for _, p := range s.Network.PublicEgressTCPPorts {
		if p < 1 || p > 65535 {
			return fmt.Errorf("%w: network public egress port %d out of range", ErrInvalidSpec, p)
		}
		if egress[p] {
			return fmt.Errorf("%w: network public egress port %d appears twice", ErrInvalidSpec, p)
		}
		egress[p] = true
	}
	if !s.Network.Isolate && len(s.Network.ControllerEgressTCPPorts) > 0 {
		return fmt.Errorf("%w: network names controller egress ports %v without Isolate; on an unconfined "+
			"instance that list restricts nothing and reads as if it did", ErrInvalidSpec, s.Network.ControllerEgressTCPPorts)
	}
	ctrl := map[int]bool{}
	for _, p := range s.Network.ControllerEgressTCPPorts {
		if p < 1 || p > 65535 {
			return fmt.Errorf("%w: network controller egress port %d out of range", ErrInvalidSpec, p)
		}
		if ctrl[p] {
			return fmt.Errorf("%w: network controller egress port %d appears twice", ErrInvalidSpec, p)
		}
		ctrl[p] = true
	}
	if sc := s.Sidecar; sc != nil {
		if !sidecarName.MatchString(sc.Name) {
			return fmt.Errorf("%w: sidecar name %q is not a DNS label", ErrInvalidSpec, sc.Name)
		}
		if sc.Runtime.Image == "" {
			return fmt.Errorf("%w: sidecar %q has no image; a sidecar is a container", ErrInvalidSpec, sc.Name)
		}
		for _, f := range sc.Files {
			if !strings.HasPrefix(f.Path, "/") {
				return fmt.Errorf("%w: sidecar file path %q must be absolute", ErrInvalidSpec, f.Path)
			}
			if seen[f.Path] {
				return fmt.Errorf("%w: file path %q appears twice (the instance and its sidecar, or the sidecar twice)", ErrInvalidSpec, f.Path)
			}
			seen[f.Path] = true
		}
	}
	if s.Repo.URL != "" {
		u, err := url.Parse(s.Repo.URL)
		if err != nil {
			return fmt.Errorf("%w: repo url %q: %v", ErrInvalidSpec, s.Repo.URL, err)
		}
		// 🔴 A CREDENTIAL IN A CLONE URL IS A CREDENTIAL IN EVERY LOG LINE THAT
		// EVER PRINTS THE SPEC. Refuse it here, once, rather than hope each
		// driver redacts.
		if u.User != nil {
			return fmt.Errorf("%w: repo url must not carry credentials; supply the token via Secrets and let the instance's credential helper read it", ErrInvalidSpec)
		}
	}
	return nil
}

// Phase is an instance's coarse lifecycle state, spelled the same way by every
// driver so a caller can switch on it.
type Phase string

const (
	// PhasePending — accepted, not yet running.
	PhasePending Phase = "pending"
	// PhaseRunning — the process is up. Ready says whether it is serving.
	PhaseRunning Phase = "running"
	// PhaseSucceeded — the process exited zero and is not expected back.
	PhaseSucceeded Phase = "succeeded"
	// PhaseFailed — the process exited non-zero and is not expected back.
	PhaseFailed Phase = "failed"
	// PhaseStopped — deliberately scaled to zero. Distinct from Failed, because
	// conflating them is how a stop gets reported as an outage.
	PhaseStopped Phase = "stopped"
	// PhaseUnknown — the driver reached its backend but could not classify what
	// it found. NOT the same as being unable to reach it, which is an error.
	PhaseUnknown Phase = "unknown"
)

// Instance is one instance's live state, as the driver currently observes it.
type Instance struct {
	// Ref identifies it. Name is always set; ID is set only if the driver
	// recorded one at create time.
	Ref Ref
	// Driver is the name of the driver reporting this.
	Driver string
	// Group is the driver's own scoping unit — a namespace, a compose project,
	// a directory. Empty where the concept does not apply.
	Group string
	// InstanceID is the backend's own name for the running thing: a pod name, a
	// container id, a pid. It changes when the process is replaced, and callers
	// use that fact.
	InstanceID string
	// Phase is the coarse state.
	Phase Phase
	// Ready reports whether the instance is serving, as distinct from running.
	Ready bool
	// Replicas is how many are currently up.
	Replicas int
	// Restarts is the cumulative restart count.
	Restarts int32
	// Reason is why the instance is NOT running now, when it is not — a waiting
	// or pending reason.
	Reason string
	// RestartReason is why the instance LAST DIED, for one that has since come
	// back ("OOMKilled", "Error", …), or "" when nothing has terminated.
	//
	// 🔴 IT IS DELIBERATELY DISTINCT FROM Reason AND MUST NOT BE FOLDED INTO
	// IT. An out-of-memory kill is invisible in every other field once the
	// supervisor restarts the process: the instance reads running and ready,
	// with a bumped restart count, and nothing anywhere says WHY. This field is
	// the only signal that says an agent is simply too small for the repository
	// it was given, which is the difference between a useful message and a
	// mysteriously slow agent.
	RestartReason string
}

// Endpoint is where an instance can be reached.
type Endpoint struct {
	// Scheme is "http", "https", "tcp", …. Empty means "http".
	Scheme string
	// Host is a hostname or address, WITHOUT a port.
	Host string
	// Port is the port number.
	Port int
	// Path is a base path, if the instance is not served at the root.
	Path string
}

// Addr renders host:port, correctly bracketing an IPv6 literal.
func (e Endpoint) Addr() string {
	return net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
}

// URL renders the endpoint as a URL, defaulting the scheme to http.
func (e Endpoint) URL() string {
	scheme := e.Scheme
	if scheme == "" {
		scheme = "http"
	}
	p := e.Path
	if p != "" && !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return scheme + "://" + e.Addr() + p
}

// IsZero reports whether the endpoint names nothing.
func (e Endpoint) IsZero() bool { return e.Host == "" && e.Port == 0 }

// PortNumber resolves the port named `name` from the spec, falling back to
// DefaultPortName and then to the single declared port when there is exactly
// one. It returns 0 when nothing matches, which callers treat as "this instance
// declares no reachable port" rather than as an error.
func (s Spec) PortNumber(name string) int {
	if name != "" {
		for _, p := range s.Ports {
			if p.Name == name {
				return p.Port
			}
		}
	}
	for _, p := range s.Ports {
		if p.Name == DefaultPortName {
			return p.Port
		}
	}
	if len(s.Ports) == 1 {
		return s.Ports[0].Port
	}
	return 0
}

// sortedEnvPairs is a stable rendering of an env list as `name=value` pairs.
//
// 🔴 IT CONTAINS THE VALUES, SO ITS ONLY LEGITIMATE CONSUMER IS SOMETHING THAT
// HASHES IT. Fingerprint is that consumer, and today it is the only one. This
// function was called sortedEnvNames and its comment offered "driver-side
// annotations" as a second consumer — a name that hid what it returns, next to
// a sentence sanctioning the one use that would publish it: an annotation is
// readable by anything that can read the object, and it is called on
// Spec.Secrets. Nothing leaked, because no driver ever took that invitation.
//
// Sorting is what makes two specs that differ only in the ORDER of their
// environment compare equal — which they should, because nothing downstream
// depends on that order.
func sortedEnvPairs(env []EnvVar) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		out = append(out, e.Name+"="+e.Value)
	}
	sort.Strings(out)
	return out
}
