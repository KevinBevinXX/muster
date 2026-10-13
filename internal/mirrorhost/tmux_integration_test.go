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

	"github.com/ZacxDev/muster/internal/termsize"
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

// TestTmuxActiveWindowsIsEachSessionsCURRENTWindow: list-sessions'
// #{window_index} is the session's ACTIVE window — measured at three points (a
// middle window, the last after select-window, the first) and on a second
// session left on its newest window, so a format that answered "the first
// window" or "the last window" fails one of them.
func TestTmuxActiveWindowsIsEachSessionsCURRENTWindow(t *testing.T) {
	r := newTmuxRig(t)
	r.session("alpha", "A")
	r.run("new-window", "-d", "-t", "=alpha:")
	r.run("new-window", "-d", "-t", "=alpha:")
	r.run("select-window", "-t", "=alpha:1")
	r.session("beta", "B")
	r.run("new-window", "-t", "=beta:")
	b := Tmux{Socket: r.sock}
	got, err := b.ActiveWindows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got["alpha"] != "1" || got["beta"] != "1" || len(got) != 2 {
		t.Fatalf("ActiveWindows = %v, want alpha:1 beta:1", got)
	}
	r.run("select-window", "-t", "=alpha:2")
	if got, err = b.ActiveWindows(context.Background()); err != nil || got["alpha"] != "2" {
		t.Fatalf("after select-window alpha:2, ActiveWindows = %v (%v)", got, err)
	}
	r.run("select-window", "-t", "=alpha:0")
	if got, _ = b.ActiveWindows(context.Background()); got["alpha"] != "0" {
		t.Fatalf("after select-window alpha:0, ActiveWindows = %v", got)
	}
}

func TestMain(m *testing.M) {
	// The suite's own tmux servers inherit nothing from the developer's.
	_ = os.Unsetenv("TMUX_PANE")
	os.Exit(m.Run())
}

// TestTmuxNoShrinkLeavesTheWindowAloneAndADesktopStillGrowsIt is the hazard and
// its fix on REAL tmux, with the phone as the ONLY client (the case
// TestTmuxAReadOnlyViewerDoesNotResizeTheOperatorsWindow does not cover, and
// where tmux's ignore-size does not apply): a bare 47x20 attach — read-only —
// shrinks a 120x40 window (the control: what NoShrink exists for); through
// NoShrink the same request leaves it 120x40 and the client is told 120x41
// (the window plus the status line). A 150x50 desktop request still grows it.
func TestTmuxNoShrinkLeavesTheWindowAloneAndADesktopStillGrowsIt(t *testing.T) {
	r := newTmuxRig(t)
	size := func(s string) string {
		return strings.TrimSpace(r.run("display-message", "-p", "-t", "="+s+":", "#{window_width}x#{window_height}"))
	}
	attach := func(b Backend, s string, cols, rows uint16) (Terminal, *screen) {
		t.Helper()
		term, err := b.Attach(context.Background(), s, true, cols, rows)
		if err != nil {
			t.Fatal(err)
		}
		var sc screen
		go sc.read(term)
		waitFor(t, "the client draws", func() bool { return strings.Contains(sc.String(), "MARK-"+s) })
		return term, &sc
	}

	// Control: the bare backend shrinks the window.
	r.session("bare", "MARK-bare")
	if got := size("bare"); got != "120x40" {
		t.Fatalf("fixture: the detached window is %s", got)
	}
	term, _ := attach(Tmux{Socket: r.sock}, "bare", 47, 20)
	waitFor(t, "the bare phone shrinks the window", func() bool { return size("bare") == "47x19" })
	_ = term.Close()

	// The fix.
	r.session("main", "MARK-main")
	b := NoShrink(Tmux{Socket: r.sock})
	term, sc := attach(b, "main", 47, 20)
	time.Sleep(300 * time.Millisecond)
	if got := size("main"); got != "120x40" {
		t.Fatalf("a phone through NoShrink resized the window to %s (want 120x40)", got)
	}
	if want := string(termsize.Report(termsize.Size{Cols: 120, Rows: 41})); !strings.HasPrefix(sc.String(), want) {
		t.Fatalf("the client was not told its size first: %q", sc.String()[:min(len(sc.String()), 40)])
	}
	if err := term.Resize(40, 12); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := size("main"); got != "120x40" {
		t.Fatalf("a shrinking resize through NoShrink changed the window to %s", got)
	}
	_ = term.Close()

	term, _ = attach(b, "main", 150, 50)
	waitFor(t, "a desktop grows the window", func() bool { return size("main") == "150x49" })
	_ = term.Close()
}

// TestTmuxActivityFollowsPaneOutputAndTypedKeysNotJustTheSession is the
// measurement behind Tmux.Activity, on real tmux with NO client attached: a
// session's time must move when its pane prints, when a BACKGROUND window of it
// prints, and when keys are sent to it — and must NOT move for a session where
// nothing happened. #{session_activity} alone fails the first two on tmux 3.7c
// (it stays at the session's creation time), which is why every window's
// #{window_activity} is read too.
//
// Four sessions, read before and after: `quiet` (nothing happens — the
// control that a time does not simply follow the clock), `noisy` (its pane
// prints after a delay), `bgwin` (a window that is NOT the session's current
// one prints), and `typed` (keys sent, nothing else).
func TestTmuxActivityFollowsPaneOutputAndTypedKeysNotJustTheSession(t *testing.T) {
	r := newTmuxRig(t)
	const later = "sleep 2; printf 'late output\\n'; exec cat"
	r.session("quiet", "Q")
	r.run("new-session", "-d", "-s", "noisy", "-x", "120", "-y", "40", "sh", "-c", later)
	r.session("bgwin", "B")
	r.run("new-window", "-d", "-t", "=bgwin:", "sh", "-c", later)
	r.session("typed", "T")
	b := Tmux{Socket: r.sock}
	read := func() map[string]int64 {
		t.Helper()
		got, err := b.Activity(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	before := read()
	started := time.Now().Unix()
	for _, s := range []string{"quiet", "noisy", "bgwin", "typed"} {
		if at := before[s]; at < started-30 || at > started+1 {
			t.Fatalf("%s's activity at start = %d, want about now (%d): %v", s, at, started, before)
		}
	}
	if len(before) != 4 {
		t.Fatalf("Activity = %v, want exactly the four sessions", before)
	}

	// tmux stamps whole seconds, so "moved" needs the output to land in a later
	// second than the session started in: the panes print 2 s in.
	waitFor(t, "noisy's pane output to move its activity", func() bool { return read()["noisy"] > before["noisy"] })
	waitFor(t, "bgwin's BACKGROUND window's output to move its activity", func() bool { return read()["bgwin"] > before["bgwin"] })
	r.run("send-keys", "-t", "=typed:", "x")
	waitFor(t, "keys sent to typed to move its activity", func() bool { return read()["typed"] > before["typed"] })

	after := read()
	if after["quiet"] != before["quiet"] {
		t.Fatalf("quiet's activity moved %d → %d with nothing happening in it", before["quiet"], after["quiet"])
	}
	for _, s := range []string{"noisy", "bgwin", "typed"} {
		if after[s] <= after["quiet"] {
			t.Fatalf("%s (%d) does not rank above quiet (%d)", s, after[s], after["quiet"])
		}
	}
}

// TestTmuxPasteTypesIntoTheNamedPaneAndSubmits is the rendered view's SEND
// against real tmux — the one link of that path every other test plays with a
// fake: the text lands in the pane it was addressed to (by pane id, even when
// another pane of the window is the active one), whole, and is submitted
// (cat echoes a line only once it has been entered).
//
// ⚠ It proves tmux's half. What Claude Code's own TUI does with a bracketed
// paste followed by Enter is not something a test here can see.
func TestTmuxPasteTypesIntoTheNamedPaneAndSubmits(t *testing.T) {
	r := newTmuxRig(t)
	r.session("claude", "READY")
	first := strings.TrimSpace(r.run("display-message", "-p", "-t", "=claude:", "#{pane_id}"))
	// A second pane, now the ACTIVE one: a paste aimed at the session would land here.
	r.run("split-window", "-t", "=claude:", "sh", "-c", "printf 'OTHER\\n'; exec cat")
	second := strings.TrimSpace(r.run("display-message", "-p", "-t", "=claude:", "#{pane_id}"))
	if first == second || !strings.HasPrefix(first, "%") {
		t.Fatalf("instrument check FAILED: the split did not move the active pane (%q, %q)", first, second)
	}
	b := Tmux{Socket: r.sock}
	const msg = "/status please — and `code` & $HOME 'quoted'"
	if err := b.Paste(context.Background(), ClaudePane{Pane: first}, msg); err != nil {
		t.Fatalf("Paste: %v", err)
	}
	capture := func(pane string) string { return r.run("capture-pane", "-p", "-J", "-t", pane) }
	// Typed once by the tty's echo, and once more by cat when Enter submits it.
	waitFor(t, "the message to be pasted AND submitted in the addressed pane", func() bool {
		return strings.Count(capture(first), msg) == 2
	})
	if got := capture(second); strings.Contains(got, "status please") {
		t.Fatalf("the paste reached the ACTIVE pane instead of the addressed one:\n%s", got)
	}
	if out := r.run("list-buffers"); strings.Contains(out, "muster-mirror-") {
		t.Fatalf("the paste left its buffer behind: %s", out)
	}
	if err := b.Paste(context.Background(), ClaudePane{Pane: "claude"}, msg); err == nil {
		t.Fatal("a session name was accepted as a paste target; only a pane id may be")
	}
}

// shellSession starts a detached session running an INTERACTIVE shell with no
// rc files, in dir, and waits for its prompt.
func (r *tmuxRig) shellSession(name, dir string, argv ...string) {
	r.t.Helper()
	args := append([]string{"new-session", "-d", "-s", name, "-x", "120", "-y", "40", "-c", dir,
		"env", "HOME=" + dir, "ZDOTDIR=" + dir, "HISTFILE=/dev/null"}, argv...)
	r.run(args...)
	waitFor(r.t, name+"'s shell prompt", func() bool { return strings.TrimSpace(r.pane(name)) != "" })
	// The line editor turns bracketed paste on as it starts reading the line,
	// just after the prompt is drawn.
	time.Sleep(300 * time.Millisecond)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// The Terminal view of a mirrored session IS a read-only tmux client on that
// session (Attach with readOnly). With it attached — and no other client —
// tmux refuses `send-keys` with "client is read-only", so a message sent from
// the Terminal view's own message box was pasted and never submitted (seen
// live on 0.32.0: POST /mirror/terminal/send 502). The submit must not depend
// on which clients are attached.
func TestTmuxPastePaneSubmitsWhileAReadOnlyClientIsAttached(t *testing.T) {
	r := newTmuxRig(t)
	b := Tmux{Socket: r.sock}
	ctx := context.Background()
	dir := t.TempDir()
	r.shellSession("viewed", dir, "bash", "--norc", "--noprofile", "-i")
	term, err := b.Attach(ctx, "viewed", true, 120, 40)
	if err != nil {
		t.Fatalf("Attach (read-only): %v", err)
	}
	defer term.Close()
	go func() { _, _ = io.Copy(io.Discard, term) }()
	waitFor(t, "the read-only client to attach", func() bool {
		return strings.TrimSpace(r.run("list-clients", "-t", "=viewed", "-F", "#{client_readonly}")) == "1"
	})
	pane, ok := b.ActivePane(ctx, "viewed")
	if !ok {
		t.Fatal("no active pane")
	}
	if err := b.PastePane(ctx, pane, "echo viewed-$((50+7)) >> ro.txt"); err != nil {
		t.Fatalf("PastePane with a read-only client attached: %v", err)
	}
	waitFor(t, "the line to run", func() bool { return readFile(t, dir+"/ro.txt") != "" })
	time.Sleep(300 * time.Millisecond) // a second submission would land by now
	if got := readFile(t, dir+"/ro.txt"); got != "viewed-57\n" {
		t.Fatalf("the line ran as %q, want it run exactly once", got)
	}
}

// TestTmuxPastePaneIntoAShellSubmitsOnce is the Terminal view's composer
// against REAL tmux and REAL shells (never skipped: bash and zsh are both
// required, like tmux): what PastePane + Enter does in a session whose active
// pane is NOT Claude Code.
//
// MEASURED (tmux 3.7c, bash 5.3, zsh 5.9 — both ask for bracketed paste by
// default, with no rc file):
//
//   - one line: typed at the prompt and run EXACTLY ONCE (the file gains one
//     line, not two — a paste that submitted and an Enter that submitted again
//     would write two);
//   - several lines: ONE bracketed paste. The shell holds all of them in its
//     edit buffer — the second line is on screen with NO prompt in front of it
//     — and the single Enter runs them, in order. Shell metacharacters reach
//     the shell as typed; tmux interprets none of them.
//   - a program that did NOT ask for bracketed paste (cat): the text arrives as
//     typed, each newline entering its line.
//
// And ActivePane is the pane the operator is looking at: it follows a split.
func TestTmuxPastePaneIntoAShellSubmitsOnce(t *testing.T) {
	r := newTmuxRig(t)
	b := Tmux{Socket: r.sock}
	ctx := context.Background()
	for name, argv := range map[string][]string{
		"bash": {"bash", "--norc", "--noprofile", "-i"},
		"zsh":  {"zsh", "-f", "-i"},
	} {
		if _, err := exec.LookPath(argv[0]); err != nil {
			t.Fatalf("%s is REQUIRED by this test and is not on PATH; it fails rather than skips: %v", argv[0], err)
		}
		dir := t.TempDir()
		r.shellSession(name, dir, argv...)
		pane, ok := b.ActivePane(ctx, name)
		if want := strings.TrimSpace(r.run("display-message", "-p", "-t", "="+name+":", "#{pane_id}")); !ok || pane != want {
			t.Fatalf("%s: ActivePane = %q, %v; want %q", name, pane, ok, want)
		}

		if err := b.PastePane(ctx, pane, "echo one-$((40+2)) 'q u o t e d' >> one.txt"); err != nil {
			t.Fatalf("%s: PastePane: %v", name, err)
		}
		waitFor(t, name+": the single line to run", func() bool { return readFile(t, dir+"/one.txt") != "" })
		time.Sleep(300 * time.Millisecond) // a second submission would land by now
		if got := readFile(t, dir+"/one.txt"); got != "one-42 q u o t e d\n" {
			t.Fatalf("%s: the single line ran as %q, want it run exactly once", name, got)
		}

		const second = "echo second >> two.txt"
		if err := b.PastePane(ctx, pane, "echo first >> two.txt\n"+second); err != nil {
			t.Fatalf("%s: PastePane (two lines): %v", name, err)
		}
		waitFor(t, name+": both lines to run", func() bool { return readFile(t, dir+"/two.txt") == "first\nsecond\n" })
		// Bracketed: the second line sat in the edit buffer under the first,
		// with no prompt IN FRONT of it. Typed raw, it would follow a prompt.
		// (HasPrefix, not equality: bash 5.3 draws its next prompt straight
		// after the pasted text, on the same line.)
		bare := false
		for _, line := range strings.Split(r.run("capture-pane", "-p", "-t", pane), "\n") {
			if strings.HasPrefix(line, second) {
				bare = true
			}
		}
		if !bare {
			t.Fatalf("%s: the two lines did not arrive as ONE bracketed paste (no bare %q line):\n%s", name, second, r.run("capture-pane", "-p", "-t", pane))
		}
	}

	// A program that never asked for bracketed paste gets the text as typed.
	dir := t.TempDir()
	r.run("new-session", "-d", "-s", "plain", "-x", "120", "-y", "40", "-c", dir, "sh", "-c", "printf 'READY\\n'; exec cat > out.txt")
	waitFor(t, "cat to start", func() bool { return strings.Contains(r.pane("plain"), "READY") })
	pane, _ := b.ActivePane(ctx, "plain")
	if err := b.PastePane(ctx, pane, "line one\nline two"); err != nil {
		t.Fatalf("PastePane into cat: %v", err)
	}
	waitFor(t, "cat to receive both lines", func() bool { return readFile(t, dir+"/out.txt") == "line one\nline two\n" })

	// ActivePane follows the operator: after a split, it is the new pane, and a
	// paste aimed at it does not reach the first.
	first := pane
	r.run("split-window", "-t", "=plain:", "-c", dir, "sh", "-c", "printf 'SECOND\\n'; exec cat > second.txt")
	now, ok := b.ActivePane(ctx, "plain")
	if !ok || now == first {
		t.Fatalf("after a split ActivePane = %q, %v; want the new pane, not %q", now, ok, first)
	}
	if err := b.PastePane(ctx, now, "to the active pane"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the active pane to receive the paste", func() bool { return readFile(t, dir+"/second.txt") == "to the active pane\n" })
	if got := readFile(t, dir+"/out.txt"); got != "line one\nline two\n" {
		t.Fatalf("the paste also reached the pane that is no longer active: %q", got)
	}

	if p, ok := b.ActivePane(ctx, "no-such-session"); ok {
		t.Fatalf("ActivePane of a session that does not exist = %q, true", p)
	}
	// A session whose name merely STARTS like another is not matched.
	if p, ok := b.ActivePane(ctx, "pla"); ok {
		t.Fatalf("ActivePane matched a prefix of a session name: %q", p)
	}
	if out := r.run("list-buffers"); strings.Contains(out, "muster-mirror-") {
		t.Fatalf("a paste left its buffer behind: %s", out)
	}
}

// TestTmuxFormatQueriesSurviveANonUTF8Locale: 🔴 every `-F` output this package
// parses is TAB-separated, and tmux turns a TAB into `_` when its client's
// locale is not UTF-8 — so under `LC_ALL=C` (a systemd unit with no LANG, a
// container) each of them silently parsed NOTHING: no active pane to send to,
// no label, no activity order. Tmux.query passes `-u`; this runs the three
// queries with the locale forced to C.
//
// The control is the instrument: the same format WITHOUT `-u`, in this same
// environment, must come back with no TAB in it — or the test proves nothing
// about a host where the locale is already UTF-8.
func TestTmuxFormatQueriesSurviveANonUTF8Locale(t *testing.T) {
	for _, v := range []string{"LANG", "LC_CTYPE"} {
		t.Setenv(v, "C")
	}
	t.Setenv("LC_ALL", "C")
	r := newTmuxRig(t)
	r.session("alpha", "A")
	r.run("new-window", "-t", "=alpha:")
	if raw := r.run("list-panes", "-a", "-F", "#{pane_id}\t#{session_name}"); strings.Contains(raw, "\t") {
		t.Fatalf("instrument check FAILED: under LC_ALL=C tmux still printed a TAB (%q); this host cannot show the hazard", raw)
	}
	b := Tmux{Socket: r.sock}
	ctx := context.Background()
	want := strings.TrimSpace(r.run("display-message", "-p", "-t", "=alpha:", "#{pane_id}"))
	if pane, ok := b.ActivePane(ctx, "alpha"); !ok || pane != want {
		t.Errorf("ActivePane under LC_ALL=C = %q, %v; want %q", pane, ok, want)
	}
	if got, err := b.ActiveWindows(ctx); err != nil || got["alpha"] != "1" {
		t.Errorf("ActiveWindows under LC_ALL=C = %v, %v; want alpha:1", got, err)
	}
	if got, err := b.Activity(ctx); err != nil || got["alpha"] <= 0 {
		t.Errorf("Activity under LC_ALL=C = %v, %v; want a time for alpha", got, err)
	}
}
