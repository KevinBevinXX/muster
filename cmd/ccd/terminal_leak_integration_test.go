//go:build tmuxit

package main

// THE TERMINAL-CLIENT LEAK, AGAINST REAL tmux, THROUGH THE REAL ADAPTER.
//
// Measured on a live cluster (muster 0.6.0, 2026-10-10): a browser
// that opened an agent's terminal read-only, switched to interactive and closed
// the tab left BOTH tmux clients attached inside the pod, alive more than 60 s
// after muster logged "detached: context canceled". Cancelling the client-go
// exec stream does not end the process exec'd in the container: the container
// runtime keeps the exec process and its pty, and sends it no signal.
//
// The muster unit tests could not see that, because their exec server is
// in-process and the command they run ends with the stream. So the pod here is
// played by [containerdExec], which models what the runtime does instead:
//
//   - ExecTTY gives the command a REAL pty (session leader, controlling
//     terminal), as runc does for a TTY exec;
//   - when the stream ends (ctx cancelled) ExecTTY returns, and the command is
//     LEFT RUNNING: no signal is sent, and the pty master stays OPEN and keeps
//     being drained — the shim holds the master and copies from it — so the
//     client sees neither a hangup nor EOF nor a blocked write;
//   - Exec runs a second, ordinary (non-TTY) command in the same environment,
//     as a second `kubectl exec` would.
//
// ⚠ FAITHFULNESS, STATED: this reproduces the OBSERVABLE measured on the
// cluster (the client process and its tmux client entry outlive the stream).
// It does not reproduce the runtime's mechanism — whether the shim keeps
// draining the master after containerd stops reading its FIFO, or what happens
// to stdin's FIFO, are the runtime's, and "no hangup reaches the client" is
// the measured result this model assumes rather than derives. The live check
// after a deploy is what confirms the fix against containerd itself.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/ZacxDev/muster/internal/agentprovision"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// ioctl runs one ioctl on f WITHOUT f.Fd(), which would switch the file to
// blocking mode and race with a concurrent Close.
func ioctl(f *os.File, req uintptr, arg unsafe.Pointer) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var errno syscall.Errno
	if cerr := rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg))
	}); cerr != nil {
		return cerr
	}
	if errno != 0 {
		return errno
	}
	return nil
}

// openPty opens a pseudo-terminal pair with the given size.
func openPty(t *testing.T, cols, rows uint16) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open /dev/ptmx: %v", err)
	}
	var unlock int32
	if err := ioctl(m, syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		t.Fatalf("unlockpt: %v", err)
	}
	var n uint32
	if err := ioctl(m, syscall.TIOCGPTN, unsafe.Pointer(&n)); err != nil {
		t.Fatalf("ptsname: %v", err)
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open pts: %v", err)
	}
	setPtySize(m, cols, rows)
	return m, s
}

func setPtySize(f *os.File, cols, rows uint16) {
	ws := struct{ Row, Col, X, Y uint16 }{Row: rows, Col: cols}
	_ = ioctl(f, syscall.TIOCSWINSZ, unsafe.Pointer(&ws))
}

// containerdExec plays the pod's container runtime for exec. See the file doc.
type containerdExec struct {
	*provision.Noop
	t   *testing.T
	env []string
}

func newContainerdExec(t *testing.T, env []string) *containerdExec {
	return &containerdExec{Noop: provision.MustNewNoop(), t: t, env: env}
}

func (d *containerdExec) Capabilities() provision.Capabilities {
	c := d.Noop.Capabilities()
	c.Exec = true
	return c
}

// lockedWriter can be redirected to io.Discard once the stream has ended, so
// the shim's drain never writes to a caller that has already returned.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (l *lockedWriter) detach() {
	l.mu.Lock()
	l.w = io.Discard
	l.mu.Unlock()
}

func (d *containerdExec) ExecTTY(ctx context.Context, _ provision.Ref, cmd []string, stdin io.Reader, stdout io.Writer, resize <-chan provision.TermSize) error {
	master, slave := openPty(d.t, 80, 24)
	c := exec.Command(cmd[0], cmd[1:]...)
	c.Env = d.env
	c.Stdin, c.Stdout, c.Stderr = slave, slave, slave
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := c.Start(); err != nil {
		master.Close()
		slave.Close()
		return err
	}
	slave.Close()
	exited := make(chan error, 1)
	gone := make(chan struct{})
	go func() { err := c.Wait(); close(gone); exited <- err }()
	// The runtime's half outlives the stream: the master is closed, and a
	// still-running command killed, only when the test ends. Only PIDs this
	// test started are ever signalled.
	d.t.Cleanup(func() {
		select {
		case <-gone:
		default:
			_ = c.Process.Kill()
			<-gone
		}
		master.Close()
	})

	out := &lockedWriter{w: stdout}
	go func() { _, _ = io.Copy(out, master) }() // the shim's drain; ends when master closes
	go func() { _, _ = io.Copy(master, stdin) }()
	for {
		select {
		case <-ctx.Done():
			// 🔴 THE MODELLED DEFECT: the stream is gone; the process, its pty and
			// the drain are not, and nothing is signalled.
			out.detach()
			return ctx.Err()
		case err := <-exited:
			out.detach()
			return err
		case sz := <-resize:
			setPtySize(master, sz.Cols, sz.Rows)
		}
	}
}

func (d *containerdExec) Exec(ctx context.Context, _ provision.Ref, cmd []string, stdin io.Reader, stdout, stderr io.Writer) error {
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	c.Env = d.env
	c.Stdin, c.Stdout, c.Stderr = stdin, stdout, stderr
	return c.Run()
}

// testLog sends a logger's lines to the test's log.
type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// unusedStore satisfies agents.Store for an adapter whose terminal path never
// touches its store; any call panics on the nil interface, loudly.
type unusedStore struct{ agents.Store }

// podRig is a tmux server playing ccd's (session "cc" on the default socket
// under a private TMUX_TMPDIR) and the environment the pod's exec runs in.
type podRig struct {
	tmux   string
	server []string
	env    []string
	ready  string
}

func startPodRig(t *testing.T) *podRig {
	t.Helper()
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal("tmux is REQUIRED by the tmuxit suite and is not on PATH; install it (this suite never skips)")
	}
	dir := t.TempDir()
	r := &podRig{
		tmux:   tmuxBin,
		server: []string{"-S", innerSocketIn(t, dir), "-f", "/dev/null"},
		// Only what the container would have: TMUX_TMPDIR keeps the default
		// socket private, TMPDIR keeps anything the command writes private.
		env:   []string{"PATH=" + os.Getenv("PATH"), "TMUX_TMPDIR=" + dir, "TMPDIR=" + dir},
		ready: fmt.Sprintf("POD-READY-%d", time.Now().UnixNano()),
	}
	r.run(t, "new-session", "-d", "-s", defaultTmuxTarget, "-x", "200", "-y", "50",
		"sh", "-c", "echo "+r.ready+"; exec cat")
	t.Cleanup(func() {
		k := exec.Command(tmuxBin, append(append([]string{}, r.server...), "kill-server")...)
		k.Env = scrubbedEnv()
		_ = k.Run()
	})
	return r
}

func (r *podRig) run(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(r.tmux, append(append([]string{}, r.server...), args...)...)
	cmd.Env = scrubbedEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("tmux %v: %v: %s", args, err, out)
	}
	return string(out)
}

// clients is the server's client list, one "tty flags" line per client.
func (r *podRig) clients(t *testing.T) []string {
	t.Helper()
	out := strings.TrimSpace(r.run(t, "list-clients", "-F", "#{client_tty} #{client_flags}"))
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func (r *podRig) waitClients(t *testing.T, want int, within time.Duration, why string) {
	t.Helper()
	var got []string
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if got = r.clients(t); len(got) == want {
			return
		}
	}
	t.Fatalf("%s: want %d tmux client(s) attached, have %d:\n%s", why, want, len(got), strings.Join(got, "\n"))
}

// attachThenClose opens one terminal through the adapter, waits until it is
// attached and showing the session, then ends the stream the way a closed
// browser tab does (the bridge cancels the attach's context) and waits for
// AttachTerminal to return.
func attachThenClose(t *testing.T, r *podRig, a *agentprovision.Adapter, readOnly bool, before int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	out := &syncBuffer{}
	done := make(chan error, 1)
	ag := agents.Agent{ID: 1, Name: "leak-probe", Kind: agents.KindClaudeCode}
	go func() { done <- a.AttachTerminal(ctx, ag, readOnly, stdinR, out, nil) }()

	r.waitClients(t, before+1, 10*time.Second, fmt.Sprintf("attaching (read-only=%v)", readOnly))
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(out.String(), r.ready); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the attached terminal never showed the session; it showed %q", out.String())
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("AttachTerminal after the stream was cancelled: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("AttachTerminal did not return after its context was cancelled")
	}
}

// TestTmuxTerminalClosedStreamLeavesNoClient is the measured leak's shape: a
// read-only attach, a switch to interactive (close + reattach), then the tab
// closed. After each close the session's client count is back where it was —
// the tmux client in the "pod" is gone, not merely muster's side of it — and
// the session itself is still there.
func TestTmuxTerminalClosedStreamLeavesNoClient(t *testing.T) {
	r := startPodRig(t)
	a, err := agentprovision.New(agentprovision.Config{Driver: newContainerdExec(t, r.env), Store: unusedStore{},
		Logger: log.New(testLog{t}, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.clients(t); len(got) != 0 {
		t.Fatalf("the rig starts with clients attached: %q", got)
	}
	for _, readOnly := range []bool{true, false} {
		attachThenClose(t, r, a, readOnly, 0)
		r.waitClients(t, 0, 5*time.Second,
			fmt.Sprintf("after the read-only=%v terminal's stream ended (THE LEAK: the client outlived it)", readOnly))
	}
	r.run(t, "has-session", "-t", "="+defaultTmuxTarget)
}
