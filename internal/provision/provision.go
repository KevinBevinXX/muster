package provision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// The sentinel errors every driver reports through. They are sentinels rather
// than strings because callers BRANCH on them: the difference between "I cannot
// see the backend" and "the backend says there is nothing there" decides
// whether stored state is preserved or overwritten, and a caller cannot tell
// those apart from prose.
var (
	// ErrNotFound — the backend was reached and does not have this instance.
	ErrNotFound = errors.New("provision: instance not found")

	// ErrBlind — the driver could not reach its backend, so it knows NOTHING.
	//
	// 🔴 THIS IS THE ERROR THAT MUST NOT BECOME AN EMPTY SLICE. See the contract
	// rules in the package doc: a caller reads a List error as "keep the stored
	// status" and an empty List as "nothing is running", so a driver that
	// swallows a connection failure and returns `nil, nil` rewrites every
	// instance's state to stopped and looks, from the outside, like a cluster
	// that emptied itself.
	ErrBlind = errors.New("provision: backend unreachable")

	// ErrNotManaged — the backend was REACHED, and it has an object under the
	// name this instance asks for that the driver did not create.
	//
	// 🔴 IT IS A PERMANENT DECISION AND IT IS NOT ErrBlind. Five call sites in
	// the kubernetes driver's apply() wrapped this refusal in the ErrBlind
	// constructor, which formats with %v, so a caller saw only "backend
	// unreachable": a refusal that will never resolve on its own presented as a
	// transient fault, so a caller that retries never converges and a caller
	// that alerts pages for an outage that is not happening. TWO MORE were
	// found a round later, in that driver's Grant — the path that hands out
	// cluster RBAC — because the fix consolidated apply's sites and nobody
	// asked which OTHER paths wrote objects. A predicate corrected at some of
	// its sites is wrong at the rest of them, in the same direction.
	//
	// 🔴 A BY-NAME READ ALSO REPORTS ErrNotFound; A WRITE OR A TEARDOWN DOES
	// NOT. Those are different questions with different right answers, and the
	// asymmetry is deliberate:
	//
	//   - Get, Scale, Endpoint and the log paths were asked "what is instance
	//     X". A stranger's co-named object is not instance X, so the answer is
	//     "muster has no instance by that name" — ErrNotFound AND this.
	//     Reporting it as an instance is how a status read describes somebody
	//     else's Deployment as an agent.
	//   - Update, Destroy and Grant were asked to ACT on X. "There is no such
	//     instance" is not what happened — something else holds the name and
	//     muster refused — so these report this sentinel ALONE. Reporting
	//     ErrNotFound there is worse than imprecise: `if err != nil &&
	//     !errors.Is(err, ErrNotFound)` is the idiom Destroy's own contract
	//     invites, and it SILENTLY DISCARDS the refusal.
	//
	// 🔴 Create IS AN EXCEPTION AND THIS DOC USED TO NAME IT AS A PRODUCER.
	// Measured: Create over a foreign co-named Deployment gives
	// errors.Is(err, ErrDivergentSpec) true and errors.Is(err, ErrNotManaged)
	// FALSE. That is the DECISION, not a defect — "refused, nothing written" is
	// the branch Create's caller already has, and a single error claiming to be
	// both a divergence and an ownership refusal makes the caller's switch
	// order decide which one it is. The refusal's PROSE still names the label
	// that is wrong, because relabelling is the only escape. The kubernetes
	// driver's ownership tests now assert the absence of this sentinel there,
	// which nothing did while this paragraph was wrong.
	//
	// It is not pinned by provisiontest.RunContract: a driver has to have a
	// SHARED backend for a foreign co-named object to be expressible at all,
	// and Noop's backend is its own map, so a contract case would be
	// unfalsifiable for half the drivers in this repository. It is pinned in
	// the kubernetes driver's own ownership tests instead.
	ErrNotManaged = errors.New("provision: an object under this name is not managed by this driver")

	// ErrDivergentSpec — Create was called for an instance that already exists
	// with a materially different spec. The caller that meant to change it has
	// Update.
	ErrDivergentSpec = errors.New("provision: instance exists with a different spec")

	// ErrUnsupported — the driver cannot do what was asked and is REFUSING
	// rather than doing part of it. Returned by CheckSpec, CheckScale and Grant.
	ErrUnsupported = errors.New("provision: unsupported by this driver")

	// ErrInvalidSpec — the spec is wrong regardless of driver.
	ErrInvalidSpec = errors.New("provision: invalid spec")
)

// Provisioner is the lifecycle of an instance: create it, observe it, reach it,
// read its output, destroy it.
//
// 🔴 READ THE FOUR CONTRACT RULES IN THE PACKAGE DOC BEFORE IMPLEMENTING THIS.
// They are behavioural, they are not inferable from the signatures, and
// provisiontest.RunContract will fail a driver that breaks any of them.
//
// Note what is absent: nothing here talks TO the instance. No chat, no tool
// loop, no protocol. That absence is the reason a second driver is writable at
// all.
type Provisioner interface {
	// Driver returns the driver's short name ("noop", "kubernetes"). It appears
	// in Instance.Driver and in error messages, and it is how an operator knows
	// which set of capability losses applies to what they are looking at.
	Driver() string

	// Capabilities declares what this driver can do. Callers MUST branch on
	// it, and the branches live in this package — Capabilities' own doc
	// comment is the single authority on which capability is refused where.
	// This line used to enumerate them as "CheckSpec and Grant", which omitted
	// CheckScale and Exec; the enumeration exists once, there, because a stale
	// copy of it reads exactly like a complete one.
	Capabilities() Capabilities

	// Create brings the instance into existence.
	//
	// It is IDEMPOTENT for an identical spec: calling it twice succeeds twice
	// and leaves one instance. Called for an existing instance with a
	// materially different spec it returns ErrDivergentSpec and changes
	// nothing — the caller that wanted the change has Update and can say so.
	Create(ctx context.Context, spec Spec) error

	// Update reconciles an existing instance to spec. Unlike Create it is
	// explicit about overwriting. An instance that does not exist yet is
	// created, because "make it look like this" has an obvious answer when
	// there is nothing there.
	Update(ctx context.Context, spec Spec) error

	// Scale sets the instance's replica count. Zero is a stop and is expected
	// to be reversible: the instance's declaration survives, its processes do
	// not.
	Scale(ctx context.Context, ref Ref, replicas int) error

	// Destroy removes the instance and everything the driver created for it.
	//
	// 🔴 IT RETURNS nil ONLY IF THE INSTANCE WAS REMOVED OR WAS ALREADY ABSENT.
	// Anything else — a partial teardown, an unreachable backend, an object
	// that survived the delete — is an error. A `return nil` that means "I did
	// not try" is the defect this sentence exists to forbid.
	//
	// 🔴 A FOREIGN OBJECT UNDER THE INSTANCE'S NAME IS NOT "ALREADY ABSENT",
	// AND IT IS NOT nil. On a shared backend a name is not an identity:
	// something the driver did not create can already hold the name, and a
	// teardown that proceeded by name alone would delete a stranger's objects
	// and report success. A driver on such a backend MUST return
	// [ErrNotManaged] — and must NOT make that error satisfy
	// errors.Is(err, ErrNotFound), because `if err != nil && !errors.Is(err,
	// ErrNotFound)` is the idiom this contract invites and it would discard the
	// refusal. Which objects count as the instance's identity is the driver's
	// call; the kubernetes driver's Destroy doc states its answer.
	//
	// 🔴 AN OWNERSHIP REFUSAL FROM Destroy MEANS NOTHING WAS REMOVED, AND IT IS
	// TERMINAL. A driver must check every object it treats as the instance's
	// identity BEFORE it deletes anything, so the two outcomes are the only two
	// there are: the instance is gone (nil), or the backend is exactly as it
	// was (ErrNotManaged). A driver that removed the instance and THEN reported
	// a permanent refusal leaves a caller unable to tell which happened — and
	// "retry Destroy until it returns nil" never terminates, because the
	// refusal is about state only an operator can change. The kubernetes driver
	// had that defect where an instance's own objects were muster's and its
	// per-instance NAMESPACE was a stranger's: three consecutive Destroys
	// returned the same ErrNotManaged with the instance already torn down. It
	// now refuses first and removes nothing, so the pod keeps running until the
	// namespace is relabelled or removed. Callers branch on the SENTINEL: retry
	// [ErrBlind], surface [ErrNotManaged] to a human, treat nil as done.
	Destroy(ctx context.Context, ref Ref) error

	// List returns every instance this driver manages.
	//
	// 🔴 ERROR, NEVER AN EMPTY SLICE, WHEN THE BACKEND CANNOT BE SEEN. Wrap
	// ErrBlind. An empty slice is a positive claim that there is nothing there.
	List(ctx context.Context) ([]Instance, error)

	// Get returns one instance. ErrNotFound when the backend was reached and
	// does not have it; ErrBlind when the backend could not be reached. Those
	// are different answers and must not be collapsed.
	Get(ctx context.Context, ref Ref) (Instance, error)

	// Endpoint returns where the instance can be reached.
	//
	// 🔴 THIS METHOD IS THE POINT OF THE INTERFACE'S EXISTENCE AS MUCH AS
	// ANYTHING ELSE ON IT. In the project muster was extracted from, this was a
	// hardcoded format string in one file, with no override of any kind, which
	// meant running an agent anywhere other than that project's own cluster was
	// not expressible. Drivers resolve Spec.Endpoint first, then their own
	// configured template, then a default — and every layer of that is settable
	// from outside the driver.
	Endpoint(ctx context.Context, ref Ref) (Endpoint, error)

	// TailLogs returns the last `lines` lines of the instance's output.
	TailLogs(ctx context.Context, ref Ref, lines int64) (string, error)

	// StreamLogs follows the instance's output, calling emit once per line,
	// until ctx is cancelled or the stream ends. emit is called from the
	// calling goroutine's stream and must not block indefinitely.
	StreamLogs(ctx context.Context, ref Ref, emit func(line string)) error
}

// Execer runs a command inside a live instance. OPTIONAL: callers type-assert
// for it, and Capabilities.Exec declares whether the assertion will succeed
// meaningfully.
//
// ⚠ A DRIVER MAY IMPLEMENT THIS AND STILL REPORT Exec FALSE. Implementing it is
// a compile-time fact; being able to do it can depend on runtime configuration
// the driver was given. Callers must consult Capabilities, not the assertion —
// which is what Exec (the helper below) does.
type Execer interface {
	Exec(ctx context.Context, ref Ref, cmd []string, stdin io.Reader, stdout, stderr io.Writer) error
}

// TermSize is a terminal's size in character cells.
type TermSize struct {
	Cols, Rows uint16
}

// TTYExecer runs a command inside a live instance ON A TERMINAL: the command
// gets a pseudo-terminal, its stdout and stderr arrive merged on stdout (that is
// what a terminal is), and every size sent on resize is applied to it.
// OPTIONAL and type-asserted, exactly like [Execer], and gated by the SAME
// capability — Capabilities.Exec — through [ExecTTY]. A driver that declares
// Exec must implement both; provisiontest.RunContract fails one that does not.
//
// ExecTTY returns when the command exits, when stdin reaches EOF and the
// command exits because of it, or when ctx is cancelled — whichever is first.
// 🔴 ctx CANCELLATION MUST END IT. A caller bridging a browser socket cancels
// ctx when the socket closes, and a driver that kept the stream open after
// that would leave a goroutine in muster for every closed tab.
//
// 🔴 ENDING THE STREAM DOES NOT END THE COMMAND. Measured on containerd (the
// kubernetes driver, 2026-10-10): after the exec stream was cancelled the
// command — a tmux client — was still running in the container, with its pty,
// more than 60 s later; the runtime signals it nothing. A caller whose
// command must not outlive the stream has to end it itself, with a second
// exec (agentprovision's AttachTerminal does).
//
// resize may be nil (the terminal keeps the driver's default size). The driver
// stops reading it when ExecTTY returns; it never closes it.
type TTYExecer interface {
	ExecTTY(ctx context.Context, ref Ref, cmd []string, stdin io.Reader, stdout io.Writer, resize <-chan TermSize) error
}

// Policy is an authorisation grant attached to an instance's identity.
//
// The WORKFLOW around it — request, approve, apply, audit, revoke — is
// provider-agnostic and belongs to muster. The RULES are not, which is why
// Rules is opaque bytes interpreted only by a driver that declares
// Capabilities.Policy.
//
// ⚠ A POLICY CARRIES RULES ONLY. It once also carried Env and Files — the
// credential file or cluster config a grant places in the instance. Applying
// either means rolling the instance's pod, which no driver here does from the
// grant path, so every driver refused them and the fields could only ever be
// set to be rejected. They come back with the driver that can roll a pod;
// muster is not declaring that policy files are out of scope forever.
type Policy struct {
	// Name identifies the policy, and identifies the objects a driver created
	// for it so Revoke can find them again.
	Name string
	// Rules is the driver-interpreted authorisation payload. Opaque here.
	//
	// 🔴 A DRIVER THAT CANNOT INTERPRET A NON-EMPTY Rules MUST REFUSE THE
	// GRANT. See Grant.
	Rules json.RawMessage
}

// HasRules reports whether the policy carries a driver-interpreted payload at
// all. A `null` or empty JSON object is not one.
func (p Policy) HasRules() bool {
	s := strings.TrimSpace(string(p.Rules))
	return s != "" && s != "null" && s != "{}" && s != "[]"
}

// PolicyGranter attaches and detaches policies. OPTIONAL, type-asserted.
//
// Grant must be idempotent: granting the same policy twice is one grant.
// Revoke must succeed when the policy was never granted, for the same reason
// Destroy does — an absent thing is the state the caller asked for.
type PolicyGranter interface {
	Grant(ctx context.Context, ref Ref, p Policy) error
	Revoke(ctx context.Context, ref Ref, policyName string) error
}

// Grant applies a policy through p, REFUSING rather than silently ignoring when
// the driver cannot honour it.
//
// 🔴 THIS FUNCTION IS A SECURITY CONTROL, NOT A CONVENIENCE WRAPPER, AND IT IS
// WHY CALLERS MUST NOT TYPE-ASSERT PolicyGranter THEMSELVES. An interface that
// shows "granted" for a policy nobody applied is worse than having no policy
// feature: it reads as coverage, so nobody looks. Every path that grants goes
// through here, and every refusal it returns carries a reason a user interface
// can display verbatim and a machine endpoint can return as a 409.
//
// The two refusals:
//
//   - the driver does not implement PolicyGranter at all;
//   - it implements it but reports Capabilities.Policy false, which is how a
//     driver says "configured without the access needed to apply policy".
//
// ⚠ THERE WERE FOUR. Two more guarded Policy.Files against Capabilities.Files
// and Capabilities.Secrets, and they went with those fields. They were worse
// than redundant: the kubernetes driver reports Files true, so the guard here
// PASSED and its own Grant then refused the identical policy — a second,
// contradictory refusal open-coded in a driver, which is exactly what the
// chokepoint above the function exists to prevent. Whatever replaces them when
// policy files return must be spelled HERE, once, and must agree with what the
// drivers actually do.
func Grant(ctx context.Context, p Provisioner, ref Ref, pol Policy) error {
	caps := p.Capabilities()
	granter, ok := p.(PolicyGranter)
	if !ok {
		return fmt.Errorf("%w: driver %q cannot apply authorisation policy, so policy %q was NOT granted",
			ErrUnsupported, p.Driver(), pol.Name)
	}
	if !caps.Policy {
		return fmt.Errorf("%w: driver %q reports no policy capability, so policy %q was NOT granted",
			ErrUnsupported, p.Driver(), pol.Name)
	}
	return granter.Grant(ctx, ref, pol)
}

// Revoke removes a policy through p. A driver that cannot grant cannot have
// granted, so a revoke against one is a no-op rather than an error — the
// asymmetry with Grant is deliberate: refusing to revoke leaves a caller with
// no way to clean up after a driver swap.
func Revoke(ctx context.Context, p Provisioner, ref Ref, policyName string) error {
	granter, ok := p.(PolicyGranter)
	if !ok || !p.Capabilities().Policy {
		return nil
	}
	return granter.Revoke(ctx, ref, policyName)
}

// Exec runs a command inside an instance, refusing when the driver declares it
// cannot. Callers use this rather than asserting, so the Capabilities check
// cannot be forgotten at one of several call sites.
func Exec(ctx context.Context, p Provisioner, ref Ref, cmd []string, stdin io.Reader, stdout, stderr io.Writer) error {
	execer, ok := p.(Execer)
	if !ok {
		return fmt.Errorf("%w: driver %q cannot exec", ErrUnsupported, p.Driver())
	}
	if !p.Capabilities().Exec {
		return fmt.Errorf("%w: driver %q reports no exec capability", ErrUnsupported, p.Driver())
	}
	return execer.Exec(ctx, ref, cmd, stdin, stdout, stderr)
}

// ExecTTY runs a command inside an instance on a terminal, refusing when the
// driver declares it cannot exec. It is [Exec]'s rule reached by a second entry
// point — one capability, Capabilities.Exec, refused in this file and nowhere
// else — so a caller cannot reach a terminal by asserting [TTYExecer] itself.
func ExecTTY(ctx context.Context, p Provisioner, ref Ref, cmd []string, stdin io.Reader, stdout io.Writer, resize <-chan TermSize) error {
	execer, ok := p.(TTYExecer)
	if !ok {
		return fmt.Errorf("%w: driver %q cannot attach a terminal", ErrUnsupported, p.Driver())
	}
	if !p.Capabilities().Exec {
		return fmt.Errorf("%w: driver %q reports no exec capability, so it cannot attach a terminal", ErrUnsupported, p.Driver())
	}
	if len(cmd) == 0 {
		return fmt.Errorf("%w: a terminal needs a command", ErrInvalidSpec)
	}
	return execer.ExecTTY(ctx, ref, cmd, stdin, stdout, resize)
}

// Fingerprint is a stable digest of the parts of a spec that, if changed, mean
// the instance must be rebuilt rather than left alone.
//
// It is how a driver answers Create's idempotence question without storing the
// whole spec: record the fingerprint at create time, compare on the next
// Create, return ErrDivergentSpec when they differ.
//
// 🔴 WHAT IT COVERS IS A DECISION, AND THE OMISSIONS ARE DELIBERATE. Ref.ID is
// excluded because it is a correlation hint whose change means nothing about
// the running instance. Replicas is excluded because Scale changes it
// legitimately and a scaled instance is not a divergent one. Labels are
// excluded because they are the operator's own annotation space.
//
// ⚠ SECRET VALUES ARE HASHED, NOT RECORDED. The digest goes into a label or an
// annotation on a real backend, and an annotation is world-readable to anything
// that can read the object.
func Fingerprint(s Spec) string {
	h := sha256.New()
	w := func(parts ...string) {
		for _, p := range parts {
			h.Write([]byte(p))
			h.Write([]byte{0})
		}
	}
	w("name", s.Ref.Name)
	w("image", s.Runtime.Image)
	w("command", strings.Join(s.Runtime.Command, "\x1f"))
	w("args", strings.Join(s.Runtime.Args, "\x1f"))
	w("workdir", s.Runtime.WorkingDir)
	w("env", strings.Join(sortedEnvPairs(s.Env), "\x1f"))
	w("secrets", strings.Join(sortedEnvPairs(s.Secrets), "\x1f"))

	files := make([]string, 0, len(s.Files))
	for _, f := range s.Files {
		sum := sha256.Sum256(f.Content)
		files = append(files, fmt.Sprintf("%s|%o|%t|%s", f.Path, f.EffectiveMode(), f.Secret, hex.EncodeToString(sum[:8])))
	}
	sort.Strings(files)
	w("files", strings.Join(files, "\x1f"))

	w("init", strings.Join(s.Init, "\x1f"))
	w("resources", s.Resources.CPURequest, s.Resources.CPULimit, s.Resources.MemoryRequest, s.Resources.MemoryLimit)
	w("workspace", s.Workspace.Path, s.Workspace.Size, strconv.FormatBool(s.Workspace.Persist))
	w("repo", s.Repo.URL, s.Repo.Branch, s.Repo.Path)

	ports := make([]string, 0, len(s.Ports))
	for _, p := range s.Ports {
		ports = append(ports, fmt.Sprintf("%s|%d|%s", p.Name, p.Port, p.Protocol))
	}
	sort.Strings(ports)
	w("ports", strings.Join(ports, "\x1f"))

	// 🔴 HEALTH COUNTS, AND LEAVING IT OUT WOULD HAVE BEEN INVISIBLE RATHER THAN
	// HARMLESS. The k8s driver renders Spec.Health into the POD TEMPLATE, so two
	// specs differing only in it describe two different pods — and this digest is
	// what Create compares to decide a spec has diverged and what Update writes
	// to decide whether to roll. A Health change absent from the digest is a
	// probe an existing instance never gets, under a fingerprint that says it
	// matches.
	//
	// ⚠ ADDING IT MOVES EVERY SPEC'S FINGERPRINT, INCLUDING SPECS THAT DECLARE NO
	// HEALTH, because the keyed writes are positional. That is a one-time roll of
	// every live instance on its next Update, and it is correct: the pod template
	// genuinely changed in the same release.
	w("health", s.Health.HTTPGetPath, s.Health.PortName)

	if s.Endpoint != nil {
		w("endpoint", s.Endpoint.Scheme, s.Endpoint.Host, strconv.Itoa(s.Endpoint.Port), s.Endpoint.Path)
	} else {
		w("endpoint", "")
	}

	// Config is opaque, but a CHANGE to it still means the instance's runtime
	// was told something different, so it counts. json.Marshal sorts map keys,
	// which is what makes this stable across runs.
	if len(s.Config) > 0 {
		b, err := json.Marshal(s.Config)
		if err != nil {
			// A Config that cannot be marshalled is a caller bug, but this
			// function has no error return and inventing one would push the
			// handling into every driver. Fold the error text in: two
			// unmarshallable Configs then compare equal only if they fail the
			// same way, which is strictly better than treating both as empty.
			b = []byte("unmarshalable:" + err.Error())
		}
		w("config", string(b))
	} else {
		w("config", "")
	}

	// 🔴 SECURITY COUNTS — it is rendered into the pod template, so two specs that
	// differ only in it are two different pods — BUT ONLY WHEN DECLARED, which is
	// the opposite of the "health" write above and deliberately so. Health was
	// added in a release whose pod templates genuinely all changed; Security was
	// added with every existing spec's value zero, and an unconditional write
	// would have moved every live instance's fingerprint and rolled every agent on
	// its next Update for a pod template that had not changed. Written last, and
	// only when non-zero, every pre-existing fingerprint is byte-identical.
	if sec := s.Security; !sec.IsZero() {
		w("security", strconv.FormatInt(sec.RunAsUser, 10), strconv.FormatInt(sec.RunAsGroup, 10),
			strconv.FormatInt(sec.FSGroup, 10), strconv.FormatBool(sec.RunAsNonRoot),
			strconv.FormatBool(sec.Restricted), strconv.FormatBool(sec.NoServiceAccountToken))
	}

	// 🔴 NETWORK COUNTS, AND ONLY WHEN DECLARED — the rule and the reason are
	// Security's, immediately above. A spec declaring none keeps the fingerprint
	// it had before the type existed. A spec that GAINS a declaration moves, which
	// is what makes an instance created before its kind was confined reconcile on
	// its next Update instead of comparing equal to a spec that now asks for more.
	//
	// The ports are sorted: their order changes nothing about what is allowed.
	if n := s.Network; !n.IsZero() {
		ports := make([]string, 0, len(n.PublicEgressTCPPorts))
		for _, p := range n.PublicEgressTCPPorts {
			ports = append(ports, strconv.Itoa(p))
		}
		sort.Strings(ports)
		w("network", strconv.FormatBool(n.Isolate), strings.Join(ports, "\x1f"))
		// Its own line, written only when declared, so a spec that declares no
		// controller egress keeps the fingerprint it had before the field existed.
		if len(n.ControllerEgressTCPPorts) > 0 {
			cports := make([]string, 0, len(n.ControllerEgressTCPPorts))
			for _, p := range n.ControllerEgressTCPPorts {
				cports = append(cports, strconv.Itoa(p))
			}
			sort.Strings(cports)
			w("network-controller", strings.Join(cports, "\x1f"))
		}
	}

	return hex.EncodeToString(h.Sum(nil))
}
