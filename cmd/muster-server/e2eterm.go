//go:build e2eterm

package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
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

func (d fakeTerminalDriver) Capabilities() provision.Capabilities {
	c := d.Provisioner.Capabilities()
	c.Exec = true
	return c
}

// Exec answers only the terminal release the adapter sends after every attach
// (agents.TerminalReleaseCommand): the played terminal holds no process, so
// there is nothing to release.
func (fakeTerminalDriver) Exec(_ context.Context, _ provision.Ref, cmd []string, _ io.Reader, stdout, _ io.Writer) error {
	if slices.Equal(cmd, agents.TerminalReleaseCommand()) {
		_, err := io.WriteString(stdout, "released 0 (e2e fake terminal)\n")
		return err
	}
	return fmt.Errorf("%w: the e2e fake driver only plays a terminal", provision.ErrUnsupported)
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
			if _, err := fmt.Fprintf(stdout, "size=%dx%d\r\n", sz.Cols, sz.Rows); err != nil {
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
