// Package scrubexec is the ONE way agentprovision reaches a provisioning driver:
// a wrapper whose every exec into an agent's container runs with the container's
// turn credentials removed from its environment.
//
// 🔴 WHY A WRAPPER IN ITS OWN PACKAGE. An exec does not inherit ccd's environment
// (which ccd scrubs); it inherits the CONTAINER's, which holds
// MUSTER_GATEWAY_BEARER and, for a chief, CCD_GATEWAY_UPSTREAM_BEARER
// (agentspec.TurnCredentialEnv). A job's git run in the session-writable
// workspace, or the terminal's long-lived `tmux attach` client (readable at
// /proc/<pid>/environ by any same-uid process), would otherwise hold them. A
// helper every call site had to remember was walkable by spelling (an import
// alias, a local interface assertion, a wrapper elsewhere). Here the raw driver
// is an UNEXPORTED field of a type in another package: agentprovision cannot
// read it, so every exec it can make goes through Exec/ExecTTY below. The
// structural ledger (agentprovision's TestTheRawDriverIsReachableOnlyThroughTheScrub)
// pins that the adapter stores nothing else.
//
// ⚠ IT NEEDS `env` IN THE IMAGE (coreutils or busybox; every exec already needs
// `sh`). It does not stop an exec'd command from RUNNING as the agent's user —
// which the session already is — only from seeing the credentials.
package scrubexec

import (
	"context"
	"fmt"
	"io"

	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/provision"
)

// Command is cmd run with every agentspec.TurnCredentialEnv variable removed
// from its environment: `env -u NAME … cmd…`.
func Command(cmd []string) []string {
	out := make([]string, 0, 1+2*len(agentspec.TurnCredentialEnv)+len(cmd))
	out = append(out, "env")
	for _, name := range agentspec.TurnCredentialEnv {
		out = append(out, "-u", name)
	}
	return append(out, cmd...)
}

// Driver is a provision.Provisioner whose Exec and ExecTTY strip the turn
// credentials. It deliberately exposes NO way back to the driver it wraps.
type Driver struct {
	raw provision.Provisioner
}

// Wrap is the only constructor. agentprovision.New calls it once, on
// Config.Driver, and keeps nothing else.
func Wrap(raw provision.Provisioner) Driver { return Driver{raw: raw} }

// Exec runs Command(cmd) through the wrapped driver.
func (d Driver) Exec(ctx context.Context, ref provision.Ref, cmd []string, stdin io.Reader, stdout, stderr io.Writer) error {
	e, ok := d.raw.(provision.Execer)
	if !ok {
		return fmt.Errorf("%w: driver %q cannot exec", provision.ErrUnsupported, d.raw.Driver())
	}
	return e.Exec(ctx, ref, Command(cmd), stdin, stdout, stderr)
}

// ExecTTY runs Command(cmd) on a terminal through the wrapped driver.
func (d Driver) ExecTTY(ctx context.Context, ref provision.Ref, cmd []string, stdin io.Reader, stdout io.Writer, resize <-chan provision.TermSize) error {
	e, ok := d.raw.(provision.TTYExecer)
	if !ok {
		return fmt.Errorf("%w: driver %q cannot attach a terminal", provision.ErrUnsupported, d.raw.Driver())
	}
	return e.ExecTTY(ctx, ref, Command(cmd), stdin, stdout, resize)
}

// The rest of provision.Provisioner, forwarded. Spelled out rather than
// embedded: an embedded field would be a way back to the raw driver
// (d.Provisioner), and a method the interface gains later is then a compile
// error here rather than a silent pass-through.

func (d Driver) Driver() string                       { return d.raw.Driver() }
func (d Driver) Capabilities() provision.Capabilities { return d.raw.Capabilities() }
func (d Driver) Create(ctx context.Context, spec provision.Spec) error {
	return d.raw.Create(ctx, spec)
}
func (d Driver) Update(ctx context.Context, spec provision.Spec) error {
	return d.raw.Update(ctx, spec)
}
func (d Driver) Scale(ctx context.Context, ref provision.Ref, replicas int) error {
	return d.raw.Scale(ctx, ref, replicas)
}
func (d Driver) Destroy(ctx context.Context, ref provision.Ref) error   { return d.raw.Destroy(ctx, ref) }
func (d Driver) List(ctx context.Context) ([]provision.Instance, error) { return d.raw.List(ctx) }
func (d Driver) Get(ctx context.Context, ref provision.Ref) (provision.Instance, error) {
	return d.raw.Get(ctx, ref)
}
func (d Driver) Endpoint(ctx context.Context, ref provision.Ref) (provision.Endpoint, error) {
	return d.raw.Endpoint(ctx, ref)
}
func (d Driver) StreamLogs(ctx context.Context, ref provision.Ref, emit func(line string)) error {
	return d.raw.StreamLogs(ctx, ref, emit)
}

var (
	_ provision.Provisioner = Driver{}
	_ provision.Execer      = Driver{}
	_ provision.TTYExecer   = Driver{}
)
