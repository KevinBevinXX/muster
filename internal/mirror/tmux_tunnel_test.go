//go:build tmuxit

package mirror_test

// The WHOLE chain against REAL tmux: a muster (Service + Hub behind an HTTP
// server), a REAL muster-mirror client dialling it, and the tmux backend on a
// private socket. Read-only shows the pane and types nothing; on an
// -interactive host, interactive types into the pane; on a host WITHOUT it, the
// interactive attach is refused and the pane is untouched. Never skips: no tmux
// is a failure.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/mirrorhost"
)

func privateTmux(t *testing.T) (sock string, run func(args ...string) string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Fatalf("tmux is not installed; this suite fails rather than skips: %v", err)
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	sock = "muster-mirror-tunnel-" + hex.EncodeToString(b[:])
	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TMUX=") && !strings.HasPrefix(kv, "TMUX_PANE=") {
			env = append(env, kv)
		}
	}
	run = func(args ...string) string {
		t.Helper()
		c := exec.Command("tmux", append([]string{"-f", "/dev/null", "-L", sock}, args...)...)
		c.Env = env
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	t.Cleanup(func() {
		c := exec.Command("tmux", "-L", sock, "kill-server")
		c.Env = env
		_ = c.Run()
	})
	return sock, run
}

func waitLong(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTmuxThroughTheTunnel(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		sock, run := privateTmux(t)
		run("new-session", "-d", "-s", "work", "-x", "120", "-y", "40", "sh", "-c", "printf '%s\\n' TUNNEL-MARK-c41; exec cat")
		pane := func() string { return run("capture-pane", "-p", "-t", "=work:") }

		r := newRig(t)
		tok, _ := r.store.mint(t, 1, "workstation")
		r.connect(t, "workstation", tok, mirrorhost.Tmux{Socket: sock}, interactive)
		r.online(t, 1, "workstation", "work")

		stdin, screen, _, cancel, res := attach(t, asUser(1), r.svc, "workstation", "work", true)
		waitLong(t, "the pane through the tunnel", func() bool { return strings.Contains(screen.String(), "TUNNEL-MARK-c41") })
		_, _ = stdin.Write([]byte("ro-typed-via-tunnel\r"))
		time.Sleep(400 * time.Millisecond)
		if p := pane(); strings.Contains(p, "ro-typed-via-tunnel") {
			t.Fatalf("a READ-ONLY tunnel typed into the pane:\n%s", p)
		}
		cancel()
		if err := within(t, res, "read-only attach to end"); err != nil {
			t.Fatalf("read-only attach ended with %v", err)
		}

		stdin2, screen2, _, cancel2, res2 := attach(t, asUser(1), r.svc, "workstation", "work", false)
		if !interactive {
			if err := within(t, res2, "the refusal"); !errors.Is(err, mirror.ErrInteractiveRefused) {
				t.Fatalf("rw attach to a host without -interactive = %v, want ErrInteractiveRefused", err)
			}
			time.Sleep(300 * time.Millisecond)
			if c := strings.TrimSpace(run("list-clients", "-t", "=work")); c != "" {
				t.Fatalf("a refused interactive attach left a tmux client attached: %q", c)
			}
			continue
		}
		waitLong(t, "the pane on the interactive tunnel", func() bool { return strings.Contains(screen2.String(), "TUNNEL-MARK-c41") })
		_, _ = stdin2.Write([]byte("rw-typed-via-tunnel\r"))
		waitLong(t, "interactive keys in the pane", func() bool { return strings.Contains(pane(), "rw-typed-via-tunnel") })
		cancel2()
		waitLong(t, "the tunnel's clients detach when the browser leaves", func() bool {
			return strings.TrimSpace(run("list-clients", "-t", "=work")) == ""
		})
		if !strings.Contains(run("list-sessions"), "work") {
			t.Fatal("ending the terminal ended the operator's SESSION")
		}
	}
}
