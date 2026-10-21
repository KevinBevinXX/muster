//go:build e2eterm

package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/termsize"
)

// A BROWSER-TEST FIXTURE, COMPILED ONLY WITH `-tags e2eterm` (e2e/lib/server.ts
// builds one such binary for e2e/tests/terminal.spec.ts). It gives the noop
// driver a terminal that reports, on screen, exactly what reached it: the mode
// its command asked for, every size, and every input byte in hex. The terminal
// page, the socket, the auth door, the kind check and the capability gate in
// front of it are all the real ones; only the pod is played.
func init() {
	e2eFakeTerminal = func(p provision.Provisioner) provision.Provisioner { return fakeTerminalDriver{p} }
}

type fakeTerminalDriver struct{ provision.Provisioner }

// MUSTER_E2E_HOLD_READY_DIR arms the second thing this fixture plays: an instance that
// is NOT YET READY. The noop driver reports every running instance ready the
// moment it exists, so no browser test could open an agent page while it was
// provisioning. With this set, every running instance reports phase pending /
// not ready UNTIL a file named after the instance exists in the directory —
// the test releases it by creating that file, deterministically, when it wants
// the instance to come up. Unset (the terminal spec), nothing is held.
// e2e/tests/provisioning-indicator.spec.ts is the consumer; the name is
// envE2EHoldReadyDir in config.go's const block. hold applies it to one
// instance; List and Get are the two reads it has to cover.
func (d fakeTerminalDriver) hold(inst provision.Instance) provision.Instance {
	dir := os.Getenv(envE2EHoldReadyDir)
	if dir == "" || !inst.Ready {
		return inst
	}
	if _, err := os.Stat(filepath.Join(dir, inst.Ref.Name)); err == nil {
		return inst // released
	}
	inst.Phase = provision.PhasePending
	inst.Ready = false
	return inst
}

func (d fakeTerminalDriver) List(ctx context.Context) ([]provision.Instance, error) {
	insts, err := d.Provisioner.List(ctx)
	for i := range insts {
		insts[i] = d.hold(insts[i])
	}
	return insts, err
}

func (d fakeTerminalDriver) Get(ctx context.Context, ref provision.Ref) (provision.Instance, error) {
	inst, err := d.Provisioner.Get(ctx, ref)
	if err != nil {
		return inst, err
	}
	return d.hold(inst), nil
}

// Endpoint is every agent's gateway endpoint when envE2EAgentEndpoint names a
// loopback base URL ("http://127.0.0.1:<port>"): the spec's fake runtime answers
// the agent's turns there. Unset, the noop driver's own answer (which knows only
// the instances it created). Anything but a loopback http URL is refused, so
// even this fixture binary cannot be pointed at a real host by the variable.
func (d fakeTerminalDriver) Endpoint(ctx context.Context, ref provision.Ref) (provision.Endpoint, error) {
	raw := os.Getenv(envE2EAgentEndpoint)
	if raw == "" {
		return d.Provisioner.Endpoint(ctx, ref)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" {
		return provision.Endpoint{}, fmt.Errorf("e2e fake driver: %s=%q is not http://127.0.0.1:<port>", envE2EAgentEndpoint, raw)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		return provision.Endpoint{}, err
	}
	return provision.Endpoint{Scheme: "http", Host: "127.0.0.1", Port: p}, nil
}

func (d fakeTerminalDriver) Capabilities() provision.Capabilities {
	c := d.Provisioner.Capabilities()
	c.Exec = true
	return c
}

// Exec answers only the terminal release the adapter sends after every attach
// (agents.TerminalReleaseCommand): the played terminal holds no process, so
// there is nothing to release.
func (fakeTerminalDriver) Exec(_ context.Context, _ provision.Ref, cmd []string, _ io.Reader, stdout, _ io.Writer) error {
	if endsWith(cmd, agents.TerminalReleaseCommand()) {
		_, err := io.WriteString(stdout, "released 0 (e2e fake terminal)\n")
		return err
	}
	// The window-size query (internal/termsize's no-shrink rule): a session
	// window of envE2ETermWindow ("CxR", the CLIENT size that shows it),
	// answered in tmux's own format with the status line off. Unset, the query
	// fails as it does on a pod with no session — the pre-termsize behaviour.
	if endsWith(cmd, agents.TerminalSizeCommand(termsize.TmuxWindowFormat)) {
		var c, r int
		if _, err := fmt.Sscanf(os.Getenv(envE2ETermWindow), "%dx%d", &c, &r); err != nil || c <= 0 || r <= 0 {
			return fmt.Errorf("e2e fake terminal: no session window (no window configured)")
		}
		_, err := fmt.Fprintf(stdout, "%d %d off\n", c, r)
		return err
	}
	return fmt.Errorf("%w: the e2e fake driver only plays a terminal", provision.ErrUnsupported)
}

// endsWith reports whether cmd ends with tail: muster prefixes every exec with
// the turn-credential scrub (internal/agentprovision/scrubexec.Command).
func endsWith(cmd, tail []string) bool {
	return len(cmd) >= len(tail) && slices.Equal(cmd[len(cmd)-len(tail):], tail)
}

func (fakeTerminalDriver) ExecTTY(ctx context.Context, ref provision.Ref, cmd []string, stdin io.Reader, stdout io.Writer, resize <-chan provision.TermSize) error {
	mode := "rw"
	if cmd[len(cmd)-1] == agents.TerminalReadOnlyFlag {
		mode = "ro"
	}
	if _, err := fmt.Fprintf(stdout, "\x1b[2J\x1b[HFAKE-TUI mode=%s\r\nagent=%s\r\n", mode, ref.Name); err != nil {
		return err
	}
	in := make(chan []byte)
	go func() {
		defer close(in)
		buf := make([]byte, 512)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				b := append([]byte(nil), buf[:n]...)
				select {
				case in <- b:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case sz := <-resize:
			// A ruler ending EXACTLY at the client's last column, so a browser
			// test can scroll to RIGHT-EDGE on a client wider than the page.
			ruler := ""
			if mark := "RIGHT-EDGE"; int(sz.Cols) > len(mark) {
				ruler = strings.Repeat("-", int(sz.Cols)-len(mark)) + mark + "\r\n"
			}
			if _, err := fmt.Fprintf(stdout, "size=%dx%d\r\n%s", sz.Cols, sz.Rows, ruler); err != nil {
				return err
			}
		case b, ok := <-in:
			if !ok {
				return nil
			}
			if _, err := fmt.Fprintf(stdout, "in=%s\r\n", strings.ToUpper(hex.EncodeToString(b))); err != nil {
				return err
			}
		}
	}
}
