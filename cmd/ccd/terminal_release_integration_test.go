//go:build tmuxit

package main

// The terminal release (agents.TerminalReleaseCommand) against REAL tmux, in
// the containerd model of terminal_leak_integration_test.go: the cases the
// adapter-level leak test does not reach — a release that runs before its
// attach has started, a release among several terminals, and what a client
// left behind by a muster that never released it does to a new attach.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// rawAttach is one attach started straight through the model driver — no
// adapter, so no release unless the test runs one.
type rawAttach struct {
	tag    agents.TerminalTag
	cancel context.CancelFunc
	done   chan error
}

func startRawAttach(t *testing.T, d *containerdExec, readOnly bool, size provision.TermSize) *rawAttach {
	t.Helper()
	tag, err := agents.NewTerminalTag()
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := agents.TerminalCommand(agents.KindClaudeCode, readOnly, tag)
	if err != nil {
		t.Fatal(err)
	}
	return startRawAttachCmd(t, d, tag, cmd, size)
}

func startRawAttachCmd(t *testing.T, d *containerdExec, tag agents.TerminalTag, cmd []string, size provision.TermSize) *rawAttach {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	resize := make(chan provision.TermSize, 1)
	resize <- size
	stdin, stdinW := io.Pipe()
	t.Cleanup(func() { stdinW.Close() })
	a := &rawAttach{tag: tag, cancel: cancel, done: make(chan error, 1)}
	go func() { a.done <- d.ExecTTY(ctx, provision.Ref{Name: "raw"}, cmd, stdin, &syncBuffer{}, resize) }()
	return a
}

// release runs the release command for tag the way the adapter does, and
// returns what it printed.
func release(t *testing.T, d *containerdExec, tag agents.TerminalTag) string {
	t.Helper()
	var out syncBuffer
	if err := d.Exec(context.Background(), provision.Ref{Name: "raw"}, agents.TerminalReleaseCommand(),
		strings.NewReader(agents.TerminalReleaseInput(tag)), &out, &out); err != nil {
		t.Fatalf("release: %v: %s", err, out.String())
	}
	return strings.TrimSpace(out.String())
}

func tombstone(r *podRig, tag agents.TerminalTag) string {
	dir := strings.TrimPrefix(r.env[2], "TMPDIR=")
	return filepath.Join(dir, "muster-terminal", strings.TrimPrefix(string(tag), "MUSTER_TERMINAL_ID="))
}

// TestTmuxTerminalReleaseBeforeAttachStopsTheAttach: the browser left while
// the exec was still being set up, so the release ran FIRST. The late attach
// must exit on its own without ever attaching a client, and consume the mark
// the release left.
func TestTmuxTerminalReleaseBeforeAttachStopsTheAttach(t *testing.T) {
	r := startPodRig(t)
	d := newContainerdExec(t, r.env)
	tag, err := agents.NewTerminalTag()
	if err != nil {
		t.Fatal(err)
	}
	if got := release(t, d, tag); got != "released 0" {
		t.Fatalf("a release with nothing to end printed %q, want %q", got, "released 0")
	}
	if _, err := os.Stat(tombstone(r, tag)); err != nil {
		t.Fatalf("a release that ended nothing left no mark for a late attach: %v", err)
	}
	cmd, err := agents.TerminalCommand(agents.KindClaudeCode, false, tag)
	if err != nil {
		t.Fatal(err)
	}
	a := startRawAttachCmd(t, d, tag, cmd, provision.TermSize{Cols: 80, Rows: 24})
	select {
	case err := <-a.done:
		if err != nil {
			t.Fatalf("the late attach exited with %v, want a clean exit", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("an attach whose release already ran is still running; clients: %q", r.clients(t))
	}
	if got := r.clients(t); len(got) != 0 {
		t.Fatalf("an attach whose release already ran attached a client: %q", got)
	}
	if _, err := os.Stat(tombstone(r, tag)); !os.IsNotExist(err) {
		t.Fatalf("the late attach did not consume the release's mark: %v", err)
	}
}

// TestTmuxTerminalReleaseEndsOnlyItsOwnClient: with two terminals attached, a
// release ends exactly the one it was minted for, says so, removes its mark,
// and leaves the session and the other terminal alone.
func TestTmuxTerminalReleaseEndsOnlyItsOwnClient(t *testing.T) {
	r := startPodRig(t)
	d := newContainerdExec(t, r.env)
	ro := startRawAttach(t, d, true, provision.TermSize{Cols: 80, Rows: 24})
	r.waitClients(t, 1, 10*time.Second, "attaching the read-only terminal")
	rw := startRawAttach(t, d, false, provision.TermSize{Cols: 80, Rows: 24})
	r.waitClients(t, 2, 10*time.Second, "attaching the interactive terminal")

	// Both streams end, as for a closed tab: in this model, as on containerd,
	// that alone ends neither client.
	ro.cancel()
	rw.cancel()
	<-ro.done
	<-rw.done
	time.Sleep(300 * time.Millisecond)
	if got := r.clients(t); len(got) != 2 {
		t.Fatalf("control: ending the streams should leave both clients (the modelled runtime), have %q", got)
	}

	if got := release(t, d, ro.tag); got != "released 1" {
		t.Fatalf("release printed %q, want %q", got, "released 1")
	}
	r.waitClients(t, 1, 5*time.Second, "after releasing the read-only terminal")
	if got := r.clients(t); strings.Contains(got[0], "read-only") {
		t.Fatalf("the release ended the wrong client; the one left is %q", got[0])
	}
	if _, err := os.Stat(tombstone(r, ro.tag)); !os.IsNotExist(err) {
		t.Fatalf("a release that ended its attach left its mark behind: %v", err)
	}
	if got := release(t, d, rw.tag); got != "released 1" {
		t.Fatalf("release printed %q, want %q", got, "released 1")
	}
	r.waitClients(t, 0, 5*time.Second, "after releasing both")
	r.run(t, "has-session", "-t", "="+defaultTmuxTarget)
}

// TestTmuxTerminalStaleClientDoesNotSizeANewAttach: a client left behind by a
// muster that died before releasing it (no release ever runs) neither blocks a
// new attach nor holds the window at its own size — tmux's window-size
// "latest" follows the newest client, and the stale one is never active
// again. Measured both ways round (the stale client smaller, then larger) so
// the result is not "the window took the smaller size".
func TestTmuxTerminalStaleClientDoesNotSizeANewAttach(t *testing.T) {
	for _, c := range []struct{ stale, fresh provision.TermSize }{
		{provision.TermSize{Cols: 49, Rows: 49}, provision.TermSize{Cols: 120, Rows: 40}},
		{provision.TermSize{Cols: 150, Rows: 60}, provision.TermSize{Cols: 61, Rows: 23}},
	} {
		t.Run(fmt.Sprintf("%dx%d-then-%dx%d", c.stale.Cols, c.stale.Rows, c.fresh.Cols, c.fresh.Rows), func(t *testing.T) {
			r := startPodRig(t)
			d := newContainerdExec(t, r.env)
			stale := startRawAttach(t, d, false, c.stale)
			r.waitClients(t, 1, 10*time.Second, "attaching the soon-stale client")
			stale.cancel()
			<-stale.done // its stream is gone; its client is not, and nothing releases it

			startRawAttach(t, d, false, c.fresh)
			r.waitClients(t, 2, 10*time.Second, "attaching beside a stale client")
			// One row of the client is tmux's status line, not the window.
			want := fmt.Sprintf("%dx%d", c.fresh.Cols, c.fresh.Rows-1)
			var got string
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
				if got = strings.TrimSpace(r.run(t, "display-message", "-p", "-t", "="+defaultTmuxTarget+":",
					"#{window_width}x#{window_height}")); got == want {
					return
				}
			}
			t.Fatalf("with a stale client attached the window is %s, not the new attach's %s", got, want)
		})
	}
}

// TestTmuxTerminalReleaseRefusesAMalformedTag: a release told anything but a
// well-formed tag exits 2 and ends nothing — an empty pattern would match
// every process with an empty argv or environment entry. An attached client
// is the witness, and a well-formed release of it afterwards is the control
// that this rig's release CAN end it.
func TestTmuxTerminalReleaseRefusesAMalformedTag(t *testing.T) {
	r := startPodRig(t)
	d := newContainerdExec(t, r.env)
	a := startRawAttach(t, d, false, provision.TermSize{Cols: 80, Rows: 24})
	r.waitClients(t, 1, 10*time.Second, "attaching the witness")
	for _, in := range []string{
		"\n",
		"MUSTER_TERMINAL_ID=\n",
		"MUSTER_TERMINAL_ID=" + strings.ToUpper(strings.TrimPrefix(string(a.tag), "MUSTER_TERMINAL_ID=")) + "\n",
		strings.TrimPrefix(string(a.tag), "MUSTER_TERMINAL_ID=") + "\n",
	} {
		var out syncBuffer
		err := d.Exec(context.Background(), provision.Ref{Name: "raw"}, agents.TerminalReleaseCommand(),
			strings.NewReader(in), &out, &out)
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 {
			t.Errorf("release of %q: want exit 2, got %v (%s)", in, err, out.String())
		}
	}
	if got := r.clients(t); len(got) != 1 {
		t.Fatalf("a malformed release changed the attached clients: %q", got)
	}
	if got := release(t, d, a.tag); got != "released 1" {
		t.Fatalf("control: the well-formed release printed %q", got)
	}
	r.waitClients(t, 0, 5*time.Second, "control: after the well-formed release")
}

// TestTmuxTerminalReleaseEndsAnAttachBeforeItReachesTmux: between the exec
// starting and the script exec'ing tmux, the tag is ONLY in the process's
// argv — no client exists yet for tmux to list. The release must end it
// there too: that is the half of the release-before-attach argument the
// tombstone does not cover. A shell that stays in the foreground with the tag
// as its $1 stands in for the script at that instant.
func TestTmuxTerminalReleaseEndsAnAttachBeforeItReachesTmux(t *testing.T) {
	r := startPodRig(t)
	d := newContainerdExec(t, r.env)
	tag, err := agents.NewTerminalTag()
	if err != nil {
		t.Fatal(err)
	}
	// `; :` keeps sh from exec'ing sleep in its place, so the process holding
	// the tag in its argv stays this sh.
	early := exec.Command("sh", "-c", "sleep 30; :", "muster-terminal", string(tag))
	early.Env = r.env
	early.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := early.Start(); err != nil {
		t.Fatal(err)
	}
	// Its own process group, so the sleep it leaves behind is ended too.
	pgid := early.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	exited := make(chan error, 1)
	gone := make(chan struct{})
	go func() { err := early.Wait(); close(gone); exited <- err }()
	t.Cleanup(func() {
		select {
		case <-gone:
		default:
			_ = early.Process.Kill()
			<-gone
		}
	})
	// A bystander that merely MENTIONS the tag (inside a longer argument, as a
	// shell echoing it would) is not the attach and must survive.
	bystander := exec.Command("sh", "-c", "sleep 30; :", "bystander", "seen "+string(tag))
	bystander.Env = r.env
	bystander.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	bpgid := bystander.Process.Pid
	bystanderGone := make(chan struct{})
	var bystanderErr error
	go func() { bystanderErr = bystander.Wait(); close(bystanderGone) }()
	t.Cleanup(func() { _ = syscall.Kill(-bpgid, syscall.SIGKILL); <-bystanderGone })

	got := release(t, d, tag)
	select {
	case <-bystanderGone:
		t.Fatalf("the release ended a process that only mentions the tag (%v); it printed %q", bystanderErr, got)
	case <-time.After(300 * time.Millisecond):
	}
	if got != "released 1" {
		t.Fatalf("release printed %q, want %q", got, "released 1")
	}
	select {
	case err := <-exited:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != -1 {
			t.Fatalf("the pre-tmux attach ended with %v, want killed by a signal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the release did not end an attach whose tag is only in its argv")
	}
}
