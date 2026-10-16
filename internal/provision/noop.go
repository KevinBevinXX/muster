package provision

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// DefaultNoopEndpointTemplate is what a Noop reports addresses under. `.invalid`
// is reserved by RFC 2606 and is guaranteed never to resolve, which is the
// point: a fake driver must not hand out an address that might accidentally
// reach something real.
const DefaultNoopEndpointTemplate = "{{.Name}}.noop.invalid"

// DefaultNoopCapabilities is what a Noop declares by default.
//
// ⚠ IT CLAIMS Files, Secrets, Persistence, ResourceLimits, Scale AND
// NetworkIsolation BECAUSE IT RECORDS THEM FAITHFULLY, AND CLAIMS NOTHING ELSE.
// The honest reading of a capability for an in-memory driver is "a spec asking
// for this survives a round trip through me unchanged", and those six do — which
// for NetworkIsolation is a statement about the RECORD and not about any
// confinement: nothing runs here, so nothing is confined, and Isolation below
// already says so. Exec does not — there is no process to enter — so it is
// false and the refusal in Exec has a real subject to fire on.
//
// 🔴 Isolation IS IsolationNone AND THAT IS NOT A PLACEHOLDER. Nothing runs, so
// nothing is isolated. An interface that warns on IsolationNone should warn
// here too; a fake that quietly claimed a safe isolation level would train
// people to ignore the warning on the driver where it matters.
var DefaultNoopCapabilities = Capabilities{
	Isolation:        IsolationNone,
	Secrets:          true,
	Files:            true,
	Persistence:      true,
	ResourceLimits:   true,
	Scale:            true,
	NetworkIsolation: true,
}

// Noop is a provisioner that records instead of provisioning.
//
// # What it is for
//
// Developing and testing everything ABOVE the provisioner without a backend:
// the interface renders, the lifecycle transitions happen, the state is real
// and queryable. The project muster was extracted from had one of these and it
// proved the separation was genuine — a struct of methods returning nil
// satisfied that project's entire agent interface.
//
// # What is different here, and why
//
// That original had four defects this one exists to not have:
//
//  1. It took a DATABASE ROW ID and loaded the row itself, so a second driver
//     would have needed its schema. This one takes [Ref] and is exercised with
//     Ref.ID zero.
//  2. It carried the CONVERSATION with the agent — chat, and a tool-calling
//     variant returning a sentinel that named a specific image version. There
//     is no such method on [Provisioner] at all, so there is none here.
//  3. It declared NO CAPABILITIES, and its Destroy was a bare `return nil` —
//     indistinguishable from success, the "declared but inert" shape. This one
//     declares capabilities that its own CheckSpec calls enforce, and its
//     Destroy actually removes state, so [Provisioner.Get] afterwards returns
//     [ErrNotFound] and the nil is a claim about something.
//  4. Its real contract was NINE THINGS WIDER than the declared one — seven
//     setters wired by concrete type plus two type-asserted interfaces. The
//     optional surface here is the exec interfaces, routed through [Exec] and
//     [ExecTTY] so the capability branch cannot be skipped.
//
// # Blind mode
//
// [NoopBlind] makes List and Get return [ErrBlind]. That is not a fault
// injector bolted on for tests — it is the shape the original had, and it was
// right: its pod lister returned an error rather than an empty slice
// specifically so callers fell back to STORED state instead of recomputing it
// from nothing and flipping every instance to stopped. Blind mode is how that
// behaviour stays available to anyone developing against the fake, and how
// provisiontest.RunContract exercises the rule on a driver whose backend it can
// actually take away.
type Noop struct {
	mu        sync.Mutex
	caps      Capabilities
	blind     bool
	tmpl      EndpointTemplate
	now       func() time.Time
	instances map[string]*noopInstance
}

type noopInstance struct {
	spec        Spec
	fingerprint string
	replicas    int
	logs        []string
}

// NoopOption configures a Noop.
type NoopOption func(*Noop) error

// NoopCapabilities overrides what the driver declares it can do. Used to build
// a deliberately restricted fake, which is how the capability refusals in
// CheckSpec get exercised against a driver that really does refuse.
func NoopCapabilities(c Capabilities) NoopOption {
	return func(n *Noop) error { n.caps = c; return nil }
}

// NoopBlind makes List and Get report ErrBlind. See the type's note.
func NoopBlind() NoopOption {
	return func(n *Noop) error { n.blind = true; return nil }
}

// NoopEndpointTemplate overrides the host template.
func NoopEndpointTemplate(raw string) NoopOption {
	return func(n *Noop) error {
		t, err := ParseEndpointTemplate(raw)
		if err != nil {
			return err
		}
		n.tmpl = t
		return nil
	}
}

// NoopClock injects the clock used for log timestamps, so a test can assert on
// a log line's whole content rather than on a prefix of it.
func NoopClock(now func() time.Time) NoopOption {
	return func(n *Noop) error { n.now = now; return nil }
}

// NewNoop builds a recording provisioner.
func NewNoop(opts ...NoopOption) (*Noop, error) {
	tmpl, err := ParseEndpointTemplate(DefaultNoopEndpointTemplate)
	if err != nil {
		return nil, err
	}
	n := &Noop{
		caps:      DefaultNoopCapabilities,
		tmpl:      tmpl,
		now:       time.Now,
		instances: map[string]*noopInstance{},
	}
	for _, o := range opts {
		if err := o(n); err != nil {
			return nil, err
		}
	}
	return n, nil
}

// MustNewNoop is NewNoop for callers with static options, where a failure is a
// programming error rather than a condition.
func MustNewNoop(opts ...NoopOption) *Noop {
	n, err := NewNoop(opts...)
	if err != nil {
		panic("provision: MustNewNoop: " + err.Error())
	}
	return n
}

var _ Provisioner = (*Noop)(nil)

// Driver implements Provisioner.
func (n *Noop) Driver() string { return "noop" }

// Capabilities implements Provisioner.
func (n *Noop) Capabilities() Capabilities {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.caps
}

func (n *Noop) logf(inst *noopInstance, format string, args ...any) {
	inst.logs = append(inst.logs, n.now().UTC().Format(time.RFC3339)+" "+fmt.Sprintf(format, args...))
}

// Create implements Provisioner: idempotent, and divergent means error.
func (n *Noop) Create(ctx context.Context, spec Spec) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := CheckSpec(n.caps, spec); err != nil {
		return err
	}
	fp := Fingerprint(spec)
	if cur, ok := n.instances[spec.Ref.Name]; ok {
		if cur.fingerprint != fp {
			return fmt.Errorf("%w: %q exists with fingerprint %s, spec is %s",
				ErrDivergentSpec, spec.Ref.Name, cur.fingerprint[:12], fp[:12])
		}
		// Identical spec: succeed without touching anything. This is what
		// "idempotent" has to mean — not "recreate it", which would restart a
		// running instance for a caller who asked for nothing to change.
		return nil
	}
	inst := &noopInstance{spec: spec, fingerprint: fp, replicas: spec.DesiredReplicas()}
	n.logf(inst, "created %s replicas=%d image=%q", spec.Ref, inst.replicas, spec.Runtime.Image)
	n.instances[spec.Ref.Name] = inst
	return nil
}

// Update implements Provisioner: overwrite, explicitly.
func (n *Noop) Update(ctx context.Context, spec Spec) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := CheckSpec(n.caps, spec); err != nil {
		return err
	}
	fp := Fingerprint(spec)
	cur, ok := n.instances[spec.Ref.Name]
	if !ok {
		inst := &noopInstance{spec: spec, fingerprint: fp, replicas: spec.DesiredReplicas()}
		n.logf(inst, "created %s by update", spec.Ref)
		n.instances[spec.Ref.Name] = inst
		return nil
	}
	// The replica count survives an update: Scale owns it, and an update that
	// silently restored a stopped instance to one replica would make "change
	// the image" and "start it again" the same operation.
	cur.spec = spec
	cur.fingerprint = fp
	n.logf(cur, "updated %s fingerprint=%s", spec.Ref, fp[:12])
	return nil
}

// Scale implements Provisioner.
func (n *Noop) Scale(ctx context.Context, ref Ref, replicas int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := CheckScale(n.caps, replicas); err != nil {
		return err
	}
	inst, ok := n.instances[ref.Name]
	if !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, ref.Name)
	}
	inst.replicas = replicas
	n.logf(inst, "scaled %s to %d", ref, replicas)
	return nil
}

// Destroy implements Provisioner.
//
// 🔴 THE nil IS A CLAIM. It is returned when the instance was removed from the
// store, or when it was not in the store to begin with — and in both cases a
// subsequent Get returns ErrNotFound, which is the property the contract test
// asserts. The version this replaces returned nil having done nothing.
func (n *Noop) Destroy(ctx context.Context, ref Ref) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.blind {
		// A driver that cannot see its backend cannot claim it removed
		// anything. Saying nil here would be the exact lie the contract rule
		// forbids, just on a different method.
		return fmt.Errorf("%w: cannot destroy %q", ErrBlind, ref.Name)
	}
	delete(n.instances, ref.Name)
	return nil
}

// List implements Provisioner. Ordered by name so a caller's output is stable.
func (n *Noop) List(ctx context.Context) ([]Instance, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.blind {
		return nil, fmt.Errorf("%w: noop driver is blind", ErrBlind)
	}
	names := make([]string, 0, len(n.instances))
	for name := range n.instances {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Instance, 0, len(names))
	for _, name := range names {
		out = append(out, n.instanceLocked(n.instances[name]))
	}
	return out, nil
}

// Get implements Provisioner.
func (n *Noop) Get(ctx context.Context, ref Ref) (Instance, error) {
	if err := ctx.Err(); err != nil {
		return Instance{}, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.blind {
		return Instance{}, fmt.Errorf("%w: noop driver is blind", ErrBlind)
	}
	inst, ok := n.instances[ref.Name]
	if !ok {
		return Instance{}, fmt.Errorf("%w: %q", ErrNotFound, ref.Name)
	}
	return n.instanceLocked(inst), nil
}

func (n *Noop) instanceLocked(inst *noopInstance) Instance {
	out := Instance{
		Ref:      inst.spec.Ref,
		Driver:   "noop",
		Replicas: inst.replicas,
		Phase:    PhaseRunning,
		Ready:    true,
	}
	if inst.replicas == 0 {
		out.Phase = PhaseStopped
		out.Ready = false
		out.Reason = "scaled to zero"
		return out
	}
	// A deterministic instance id derived from the fingerprint, so a caller
	// that watches for a CHANGED id across an update sees one change exactly
	// when the spec changed. A random id would make every poll look like a
	// restart; a constant one would hide a real replacement.
	out.InstanceID = inst.spec.Ref.Name + "-" + inst.fingerprint[:8]
	return out
}

// Endpoint implements Provisioner, honouring the per-instance override first.
func (n *Noop) Endpoint(ctx context.Context, ref Ref) (Endpoint, error) {
	if err := ctx.Err(); err != nil {
		return Endpoint{}, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.blind {
		return Endpoint{}, fmt.Errorf("%w: noop driver is blind", ErrBlind)
	}
	inst, ok := n.instances[ref.Name]
	if !ok {
		return Endpoint{}, fmt.Errorf("%w: %q", ErrNotFound, ref.Name)
	}
	return ResolveEndpoint(
		inst.spec.Endpoint,
		n.tmpl,
		EndpointVars{Name: inst.spec.Ref.Name, ID: inst.spec.Ref.ID},
		inst.spec.PortNumber(DefaultPortName),
		"http",
	)
}

// StreamLogs implements Provisioner. The recorded log is finite, so the stream
// ends rather than following — and it ends by RETURNING, not by blocking until
// the context is cancelled, because a caller that cannot distinguish "the
// stream finished" from "nothing will ever arrive" has no way to show either.
func (n *Noop) StreamLogs(ctx context.Context, ref Ref, emit func(line string)) error {
	n.mu.Lock()
	if n.blind {
		n.mu.Unlock()
		return fmt.Errorf("%w: noop driver is blind", ErrBlind)
	}
	inst, ok := n.instances[ref.Name]
	if !ok {
		n.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrNotFound, ref.Name)
	}
	// Copy under the lock and emit outside it: emit is caller code and may do
	// anything, including calling back into this driver.
	logs := append([]string(nil), inst.logs...)
	n.mu.Unlock()
	for _, line := range logs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if emit != nil {
			emit(line)
		}
	}
	return nil
}
