//go:build tmuxit

package mirrorhost

// muster-mirror's tmux backend against REAL tmux, on a PRIVATE socket (-L) with
// $TMUX scrubbed, so nothing here can touch the operator's own server. Like
// cmd/ccd's real-tmux suite it never skips: no tmux is a failure. CI runs it in
// the `ccd-tmux` job (`go test -tags tmuxit -run '^TestTmux'`).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// tmuxRig is a private tmux server.
type tmuxRig struct {
	sock string
	t    *testing.T
}

func newTmuxRig(t *testing.T) *tmuxRig {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Fatalf("tmux is not installed; this suite fails rather than skips: %v", err)
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	r := &tmuxRig{sock: "muster-mirror-it-" + hex.EncodeToString(b[:]), t: t}
	// A stray $TMUX (the suite run from inside a pane) must not leak into the
	// rig. It does not prove tmuxEnv's scrub — with -L the socket wins over
	// $TMUX, and a mutant keeping it survives this suite; the scrub is pinned by
	// the untagged TestTmuxEnvDropsTMUX.
	t.Setenv("TMUX", "/tmp/not-the-operators-server,1,0")
	t.Cleanup(func() { _ = r.cmd("kill-server").Run() })
	return r
}

func (r *tmuxRig) cmd(args ...string) *exec.Cmd {
	// -f /dev/null: the server this rig starts reads no developer config.
	c := exec.Command("tmux", append([]string{"-f", "/dev/null", "-L", r.sock}, args...)...)
	c.Env = tmuxEnv()
	return c
}

func (r *tmuxRig) run(args ...string) string {
	r.t.Helper()
	out, err := r.cmd(args...).CombinedOutput()
	if err != nil {
		r.t.Fatalf("tmux %v: %v: %s", args, err, out)
	}
	return string(out)
}

// session starts a detached session whose pane prints marker and then echoes
// what is typed (cat on the pane's tty).
func (r *tmuxRig) session(name, marker string) {
	r.t.Helper()
	r.run("new-session", "-d", "-s", name, "-x", "120", "-y", "40", "sh", "-c", "printf '%s\\n' "+marker+"; exec cat")
}

func (r *tmuxRig) pane(name string) string {
	r.t.Helper()
	return r.run("capture-pane", "-p", "-t", "="+name+":")
}

// screen collects a Terminal's output.
type screen struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *screen) read(term io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := term.Read(buf)
		s.mu.Lock()
		s.b.Write(buf[:n])
		s.mu.Unlock()
		if err != nil {
			return
		}
	}
}

func (s *screen) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestTmuxListsTheServersSessions: exactly the server's sessions, sorted; a
// socket with no server is an empty list, not an error.
func TestTmuxListsTheServersSessions(t *testing.T) {
	r := newTmuxRig(t)
	b := Tmux{Socket: r.sock}
	if got, err := b.List(context.Background()); err != nil || len(got) != 0 {
		t.Fatalf("List with no server = %v, %v; want empty, nil", got, err)
	}
	r.session("zulu", "Z")
	r.session("alpha two", "A")
	got, err := b.List(context.Background())
	if err != nil || strings.Join(got, "|") != "alpha two|zulu" {
		t.Fatalf("List = %q, %v; want [alpha two zulu]", got, err)
	}
}

// TestTmuxReadOnlyAttachShowsThePaneAndIgnoresKeys: the pane's content arrives
// on a read-only client; keys typed into it never reach the pane. The
// interactive control types into the SAME session and its keys do.
func TestTmuxReadOnlyAttachShowsThePaneAndIgnoresKeys(t *testing.T) {
	r := newTmuxRig(t)
	r.session("main", "MIRROR-MARK-7f3")
	b := Tmux{Socket: r.sock}

	ro, err := b.Attach(context.Background(), "main", true, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	var s screen
	go s.read(ro)
	waitFor(t, "the pane on the read-only client", func() bool { return strings.Contains(s.String(), "MIRROR-MARK-7f3") })
	if _, err := ro.Write([]byte("readonly-typed-91\r")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if p := r.pane("main"); strings.Contains(p, "readonly-typed-91") {
		t.Fatalf("a READ-ONLY client typed into the pane:\n%s", p)
	}
	_ = ro.Close()

	rw, err := b.Attach(context.Background(), "main", false, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	var s2 screen
	go s2.read(rw)
	waitFor(t, "the pane on the interactive client", func() bool { return strings.Contains(s2.String(), "MIRROR-MARK-7f3") })
	if _, err := rw.Write([]byte("interactive-typed-42\r")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "interactive keys in the pane", func() bool { return strings.Contains(r.pane("main"), "interactive-typed-42") })
	_ = rw.Close()

	if got := strings.TrimSpace(r.run("list-clients", "-t", "=main")); got != "" {
		t.Fatalf("closed terminals left clients attached: %q", got)
	}
	if !strings.Contains(r.run("list-sessions"), "main") {
		t.Fatal("closing a terminal ended the SESSION; only the client may end")
	}
}

// TestTmuxAttachIsAnExactMatch: `main` must not attach `main2` once `main` has
// ended.
func TestTmuxAttachIsAnExactMatch(t *testing.T) {
	r := newTmuxRig(t)
	r.session("main2", "WRONG-SESSION-55")
	b := Tmux{Socket: r.sock}
	term, err := b.Attach(context.Background(), "main", true, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	var s screen
	done := make(chan struct{})
	go func() { s.read(term); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("attaching a missing session did not end")
	}
	if strings.Contains(s.String(), "WRONG-SESSION-55") {
		t.Fatalf("a prefix match attached main2:\n%s", s.String())
	}
	_ = term.Close()
}

// TestTmuxAReadOnlyViewerDoesNotResizeTheOperatorsWindow measures the hazard a
// phone viewer would otherwise be: with the operator's own (interactive) client
// at 160x45, a read-only client at 40x12 must leave the window at 160x45.
func TestTmuxAReadOnlyViewerDoesNotResizeTheOperatorsWindow(t *testing.T) {
	r := newTmuxRig(t)
	r.session("main", "SIZE-MARK")
	b := Tmux{Socket: r.sock}
	op, err := b.Attach(context.Background(), "main", false, 160, 45)
	if err != nil {
		t.Fatal(err)
	}
	defer op.Close()
	go (&screen{}).read(op)
	size := func() string {
		return strings.TrimSpace(r.run("display-message", "-p", "-t", "=main:", "#{window_width}x#{window_height}"))
	}
	// 160 wide, 44 tall: the status line takes the client's 45th row.
	waitFor(t, "the operator's client sizes the window", func() bool { return size() == "160x44" })

	phone, err := b.Attach(context.Background(), "main", true, 40, 12)
	if err != nil {
		t.Fatal(err)
	}
	defer phone.Close()
	go (&screen{}).read(phone)
	waitFor(t, "two clients attached", func() bool {
		return len(strings.Fields(strings.TrimSpace(r.run("list-clients", "-t", "=main", "-F", "#{client_name}")))) == 2
	})
	time.Sleep(300 * time.Millisecond)
	if got := size(); got != "160x44" {
		t.Fatalf("a READ-ONLY 40x12 viewer resized the operator's window to %s (want 160x44)", got)
	}
}

func TestMain(m *testing.M) {
	// The suite's own tmux servers inherit nothing from the developer's.
	_ = os.Unsetenv("TMUX_PANE")
	os.Exit(m.Run())
}
