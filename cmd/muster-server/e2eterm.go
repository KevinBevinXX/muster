//go:build e2eterm

package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
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

func (d fakeTerminalDriver) Capabilities() provision.Capabilities {
	c := d.Provisioner.Capabilities()
	c.Exec = true
	return c
}

func (fakeTerminalDriver) Exec(context.Context, provision.Ref, []string, io.Reader, io.Writer, io.Writer) error {
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
