//go:build tmuxit

package main

// muster's terminal attach command (internal/agents.TerminalCommand) against
// REAL tmux. The browser's side of the terminal is played by an OUTER tmux
// server whose pane runs the attach command — that gives the command a real
// pty, which is what the pod exec gives it, and lets the test type into it and
// read what it shows.
//
// Like the rest of this suite it never skips: no tmux is a failure.
//
// 🔴 EVERY SERVER HERE IS ADDRESSED BY AN EXPLICIT PRIVATE SOCKET, AND TMUX IS
// SCRUBBED FROM EVERY ENVIRONMENT. A tmux client with $TMUX set and no -L/-S
// talks to the server $TMUX names — on a developer machine, the operator's own
// — so neither the setup nor the cleanup here may rely on defaults.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// attachRig is an inner tmux server playing the pod's (ccd's session "cc",
// whose pane runs `cat` after printing a marker) and an outer server whose
// pane runs muster's attach command.
type attachRig struct {
	tmux       string
	inner      []string // tmux args addressing the inner server
	outer      []string // tmux args addressing the outer server
	innerReady string
}

func scrubbedEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") || strings.HasPrefix(kv, "TMUX_TMPDIR=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, extra...)
}

func (r *attachRig) run(t *testing.T, server []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(r.tmux, append(append([]string{}, server...), args...)...)
	cmd.Env = scrubbedEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("tmux %v: %v: %s", args, err, out)
	}
	return string(out)
}

// startAttachRig starts the inner server at innerSock (an explicit -S path)
// with one session named session, and the outer server running the attach
// command with env in its pane.
func startAttachRig(t *testing.T, innerSock, session string, readOnly bool, env []string) *attachRig {
	t.Helper()
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal("tmux is REQUIRED by the tmuxit suite and is not on PATH; install it (this suite never skips)")
	}
	outerSock := filepath.Join(t.TempDir(), "outer.sock")
	r := &attachRig{
		tmux:       tmuxBin,
		inner:      []string{"-S", innerSock, "-f", "/dev/null"},
		outer:      []string{"-S", outerSock, "-f", "/dev/null"},
		innerReady: fmt.Sprintf("INNER-READY-%d", time.Now().UnixNano()),
	}
	r.run(t, r.inner, "new-session", "-d", "-s", session, "-x", "200", "-y", "50",
		"sh", "-c", "echo "+r.innerReady+"; exec cat")
	t.Cleanup(func() {
		k := exec.Command(tmuxBin, "-S", innerSock, "kill-server")
		k.Env = scrubbedEnv()
		_ = k.Run()
	})

	tag, err := agents.NewTerminalTag()
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := agents.TerminalCommand(agents.KindClaudeCode, readOnly, tag)
	if err != nil {
		t.Fatal(err)
	}
	// The pane gets ONLY the variables the pod's container would have; tmux
	// sets TMUX in every pane it starts, so it is removed again here, as it is
	// absent from a pod exec.
	pane := append([]string{"env", "-u", "TMUX"}, env...)
	pane = append(pane, cmd...)
	quoted := make([]string, len(pane))
	for i, a := range pane {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	// `; sleep 30` keeps the outer pane alive after a failed attach so its
	// error can be read.
	r.run(t, r.outer, "new-session", "-d", "-s", "outer", "-x", "60", "-y", "20",
		strings.Join(quoted, " ")+"; sleep 30")
	t.Cleanup(func() {
		k := exec.Command(tmuxBin, "-S", outerSock, "kill-server")
		k.Env = scrubbedEnv()
		_ = k.Run()
	})
	return r
}

func (r *attachRig) capture(t *testing.T, server []string, target string) string {
	return r.run(t, server, "capture-pane", "-p", "-t", target)
}

func (r *attachRig) waitOuter(t *testing.T, want string) {
	t.Helper()
	var got string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if got = r.capture(t, r.outer, "outer"); strings.Contains(got, want) {
			return
		}
	}
	t.Fatalf("the attached terminal never showed %q; it shows:\n%s", want, got)
}

// innerSocketIn is the default-socket path tmux derives for TMUX_TMPDIR=dir:
// <dir>/tmux-<uid>/default.
func innerSocketIn(t *testing.T, dir string) string {
	t.Helper()
	sub := filepath.Join(dir, fmt.Sprintf("tmux-%d", os.Getuid()))
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(sub, "default")
}

// TestTmuxTerminalAttachReadOnlyIgnoresKeys: the read-only attach shows the
// session and nothing typed into it reaches the pane.
func TestTmuxTerminalAttachReadOnlyIgnoresKeys(t *testing.T) {
	dir := t.TempDir()
	// CCD_TMUX_SOCKET is a -L NAME, resolved under TMUX_TMPDIR — the test's own
	// directory, so the path stays private.
	sock := filepath.Join(filepath.Dir(innerSocketIn(t, dir)), "ccd-named")
	r := startAttachRig(t, sock, "cc", true, []string{"TMUX_TMPDIR=" + dir, "CCD_TMUX_SOCKET=ccd-named"})
	r.waitOuter(t, r.innerReady)

	r.run(t, r.outer, "send-keys", "-t", "outer", "typed-while-read-only", "Enter")
	time.Sleep(500 * time.Millisecond)
	if got := r.capture(t, r.inner, "cc"); strings.Contains(got, "typed-while-read-only") {
		t.Fatalf("a read-only attach delivered keys to the session:\n%s", got)
	}
	if got := r.run(t, r.inner, "list-clients", "-F", "#{client_flags}"); !strings.Contains(got, "read-only") {
		t.Fatalf("the attached client is not read-only: %q", got)
	}
}

// TestTmuxTerminalAttachInteractiveDeliversKeys: the control for the case
// above — the same rig without -r does deliver keys, so the read-only case is
// not passing because nothing could ever arrive.
func TestTmuxTerminalAttachInteractiveDeliversKeys(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(filepath.Dir(innerSocketIn(t, dir)), "ccd-named")
	r := startAttachRig(t, sock, "cc", false, []string{"TMUX_TMPDIR=" + dir, "CCD_TMUX_SOCKET=ccd-named"})
	r.waitOuter(t, r.innerReady)

	r.run(t, r.outer, "send-keys", "-t", "outer", "typed-interactively", "Enter")
	var got string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if got = r.capture(t, r.inner, "cc"); strings.Contains(got, "typed-interactively") {
			return
		}
	}
	t.Fatalf("an interactive attach did not deliver keys; the session shows:\n%s", got)
}

// TestTmuxTerminalAttachDefaultsReachCcdsDefaultServer: with CCD_TMUX_SOCKET
// and CCD_TMUX_TARGET UNSET — the pod's configuration — the command reaches the
// server tmux starts with no -L (ccd's case) and the session named "cc".
func TestTmuxTerminalAttachDefaultsReachCcdsDefaultServer(t *testing.T) {
	dir := t.TempDir()
	r := startAttachRig(t, innerSocketIn(t, dir), defaultTmuxTarget, true, []string{"TMUX_TMPDIR=" + dir})
	r.waitOuter(t, r.innerReady)
}

// TestTmuxTerminalAttachMatchesTheSessionExactly: a server holding only
// "cc2" is NOT attached as "cc" — a bare -t would prefix-match it.
func TestTmuxTerminalAttachMatchesTheSessionExactly(t *testing.T) {
	dir := t.TempDir()
	r := startAttachRig(t, innerSocketIn(t, dir), defaultTmuxTarget+"2", true, []string{"TMUX_TMPDIR=" + dir})
	r.waitOuter(t, "can't find session")
	if got := r.capture(t, r.outer, "outer"); strings.Contains(got, r.innerReady) {
		t.Fatalf("attached to a session whose name only starts with %q:\n%s", defaultTmuxTarget, got)
	}
}
