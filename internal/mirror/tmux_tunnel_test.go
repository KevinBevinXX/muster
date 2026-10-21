//go:build tmuxit

package mirror_test

// The WHOLE chain against REAL tmux: a muster (Service + Hub behind an HTTP
// server), a REAL muster-mirror client dialling it, and the tmux backend on a
// private socket. Read-only shows the pane and types nothing; on an
// -interactive host, interactive types into the pane; on a host WITHOUT it, the
// interactive attach is refused and the pane is untouched. Never skips: no tmux
// is a failure.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// waitScreen is waitLong for want on a terminal's screen — failing AT ONCE, with
// the attach's result and what the screen got, when the attach has ended: an
// ended terminal never draws, and a 15 s timeout that names neither hid why
// (it was the host hanging up on an open that beat its welcome; see
// TestNothingIsSentAHostBeforeItsWelcome).
func waitScreen(t *testing.T, what string, screen *syncBuf, res <-chan error, want string) {
	t.Helper()
	waitLong(t, what, func() bool {
		select {
		case err := <-res:
			t.Fatalf("%s: the attach ended (%v) before the screen showed %q; it showed %q", what, err, want, screen.String())
		default:
		}
		return strings.Contains(screen.String(), want)
	})
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
		waitScreen(t, "the pane through the tunnel", screen, res, "TUNNEL-MARK-c41")
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
		waitScreen(t, "the pane on the interactive tunnel", screen2, res2, "TUNNEL-MARK-c41")
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

// tunnelClaude writes a stand-in Claude Code CLI under dir and returns the argv
// that runs it in a pane: its argv names the npm package's entry script (what
// the host's locator looks for), it records itself in <home>/sessions/<pid>.json
// as the CLI does, prints a marker and echoes what is typed.
func tunnelClaude(t *testing.T, dir, home, sessionID string) []string {
	t.Helper()
	script := filepath.Join(dir, "node_modules", "@anthropic-ai", "claude-code", "cli.js")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "mkdir -p \"$1/sessions\"\n" +
		"printf '{\"sessionId\":\"%s\",\"cwd\":\"%s\",\"status\":\"idle\"}' \"$2\" \"$PWD\" > \"$1/sessions/$$.json\"\n" +
		"printf 'FAKE-CLAUDE %s\\n' \"$2\"\ncat\n"
	if err := os.WriteFile(script, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return []string{"sh", script, home, sessionID}
}

// TestTmuxEveryWindowThroughTheTunnel is per-window registration over the WHOLE
// chain — a muster, a real muster-mirror, real tmux — with the operator's own
// client attached and watched throughout.
//
// One session, three windows: a shell (the operator's current window) and two
// Claude Code windows in the SAME directory. Through muster: all three are
// listed; each Claude window's rendered view streams ITS OWN transcript; a
// message sent to the second Claude window lands in that window; an
// interactive terminal on it types into it. After every step the operator's
// client is still on window 0, and when the views close no viewer session is
// left on the server.
func TestTmuxEveryWindowThroughTheTunnel(t *testing.T) {
	sock, run := privateTmux(t)
	home, cwd := t.TempDir(), t.TempDir()
	project := filepath.Join(home, "projects", mirrorhost.ProjectSlug(cwd))
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "sess-a.jsonl"), []byte(promptLine("the first window's prompt")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "sess-b.jsonl"), []byte(promptLine("the second window's prompt")), 0o644); err != nil {
		t.Fatal(err)
	}
	id := func(args ...string) string { return strings.TrimSpace(run(args...)) }
	run("new-session", "-d", "-s", "work", "-x", "120", "-y", "40", "-c", cwd, "sh", "-c", "printf 'SHELL-WINDOW\\n'; exec cat")
	w0 := id("display-message", "-p", "-t", "=work:", "#{window_id}")
	w1 := id(append([]string{"new-window", "-d", "-P", "-F", "#{window_id}", "-t", "=work:", "-c", cwd}, tunnelClaude(t, t.TempDir(), home, "sess-a")...)...)
	w2 := id(append([]string{"new-window", "-d", "-P", "-F", "#{window_id}", "-t", "=work:", "-c", cwd}, tunnelClaude(t, t.TempDir(), home, "sess-b")...)...)
	pane := func(w string) string { return run("capture-pane", "-p", "-t", w) }
	waitLong(t, "both fake CLIs", func() bool {
		return strings.Contains(pane(w1), "FAKE-CLAUDE sess-a") && strings.Contains(pane(w2), "FAKE-CLAUDE sess-b")
	})

	backend := mirrorhost.Tmux{Socket: sock, Claude: mirrorhost.ClaudeLocator{Home: home}}
	// The operator: an interactive client on the session, as a terminal on the
	// host is.
	op, err := backend.Attach(context.Background(), "work", false, 150, 40)
	if err != nil {
		t.Fatal(err)
	}
	defer op.Close()
	go func() { _, _ = io.Copy(io.Discard, op) }()
	operator := func() string {
		return id("list-clients", "-t", "=work", "-F", "#{client_session} #{window_id}") + " / " + id("display-message", "-p", "-t", "=work:", "#{window_index} #{window_id}")
	}
	waitLong(t, "the operator's client", func() bool { return strings.HasPrefix(operator(), "work "+w0) })
	want := "work " + w0 + " / 0 " + w0
	still := func(step string) {
		t.Helper()
		time.Sleep(150 * time.Millisecond)
		if got := operator(); got != want {
			t.Fatalf("after %s the operator shows %q; it was on %q", step, got, want)
		}
	}

	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "workstation")
	// Wrapped as the shipped binary wraps it.
	r.connect(t, "workstation", tok, mirrorhost.NoShrink(backend), true)
	var h mirror.HostView
	waitLong(t, "three windows, two of them Claude Code", func() bool {
		hs := r.svc.Hosts(asUser(1))
		if len(hs) != 1 {
			return false
		}
		h = hs[0]
		a, _ := h.Find("work", w1)
		b, _ := h.Find("work", w2)
		return len(h.Windows) == 3 && a.Claude && b.Claude
	})
	if e, _ := h.Find("work", ""); e.Window != w0 || e.Claude || e.Title() != "work:0" {
		t.Fatalf("the session's own address = %+v, want its active window %s (a shell)", e, w0)
	}
	still("the host registered")

	// Each Claude window's rendered view is its own transcript.
	view := func(window string) (gen func() string, text func() string, cancel func()) {
		var mu sync.Mutex
		var gens, texts []string
		ctx, c := context.WithCancel(asUser(1))
		go func() {
			_ = r.svc.WatchWindow(ctx, "workstation", "work", window, "", 0, func(m mirror.Msg) error {
				mu.Lock()
				defer mu.Unlock()
				gens = append(gens, m.Gen)
				for _, ev := range m.Events {
					texts = append(texts, ev.Text)
				}
				return nil
			})
		}()
		t.Cleanup(c)
		return func() string {
				mu.Lock()
				defer mu.Unlock()
				return strings.Join(slices.Compact(slices.Clone(gens)), ",")
			},
			func() string { mu.Lock(); defer mu.Unlock(); return strings.Join(texts, "|") }, c
	}
	genA, textA, cancelA := view(w1)
	genB, textB, cancelB := view(w2)
	waitLong(t, "both transcripts", func() bool {
		return strings.Contains(textA(), "first window") && strings.Contains(textB(), "second window")
	})
	if genA() != "sess-a" || genB() != "sess-b" || strings.Contains(textA(), "second window") || strings.Contains(textB(), "first window") {
		t.Fatalf("the two windows' views are not their own: %q %q / %q %q", genA(), textA(), genB(), textB())
	}
	still("both rendered views opened")

	// A message to the second Claude window.
	if err := r.svc.PasteWindow(asUser(1), "workstation", "work", w2, "a message for sess-b", mirror.PasteClaude); err != nil {
		t.Fatal(err)
	}
	waitLong(t, "the message in the second window", func() bool { return strings.Contains(pane(w2), "a message for sess-b") })
	if strings.Contains(pane(w1), "a message for sess-b") || strings.Contains(pane(w0), "a message for sess-b") {
		t.Fatal("a message for the second window reached another")
	}
	still("a message was sent to the second window")
	// The shell window is not Claude Code: a prompt for Claude is refused there.
	if err := r.svc.PasteWindow(asUser(1), "workstation", "work", w0, "nope", mirror.PasteClaude); !errors.Is(err, mirror.ErrNotClaude) {
		t.Fatalf("a Claude send into the shell window = %v, want ErrNotClaude", err)
	}

	// An interactive terminal on the second window, typing.
	stdin, screen, _, cancel, _ := attachWindow(t, asUser(1), r.svc, "workstation", "work", w2, false)
	// The operator is checked as soon as the host has made the viewer's session
	// — before the viewer's own screen is waited for (a host that moved the
	// OPERATOR instead would fail that wait, and hide what it did).
	waitLong(t, "the host to make a viewer session", func() bool {
		return strings.Contains(run("list-sessions", "-F", "#{session_name} #{session_attached}"), mirror.ViewSessionPrefix)
	})
	still("the host made a viewer session for the second window")
	waitLong(t, "the second window on the terminal", func() bool { return strings.Contains(screen.String(), "FAKE-CLAUDE sess-b") })
	if strings.Contains(screen.String(), "SHELL-WINDOW") {
		t.Fatal("the terminal showed the operator's window")
	}
	still("an interactive terminal opened the second window")
	_, _ = stdin.Write([]byte("typed-into-sess-b\r"))
	waitLong(t, "the keys in the second window", func() bool { return strings.Contains(pane(w2), "typed-into-sess-b") })
	if strings.Contains(pane(w0), "typed-into-sess-b") {
		t.Fatal("keys for the second window were typed into the operator's")
	}
	still("the terminal typed into the second window")
	// And the session's own address, the old URL: the active window, through a
	// viewer session too.
	_, screen0, _, cancel0, _ := attach(t, asUser(1), r.svc, "workstation", "work", true)
	waitLong(t, "the active window on the terminal", func() bool { return strings.Contains(screen0.String(), "SHELL-WINDOW") })
	still("a terminal opened the session by name")

	cancel()
	cancel0()
	cancelA()
	cancelB()
	waitLong(t, "every viewer session to be gone", func() bool {
		return !strings.Contains(run("list-sessions", "-F", "#{session_name}"), mirror.ViewSessionPrefix)
	})
	still("everything closed")
	if got := strings.Fields(run("list-sessions", "-F", "#{session_name}")); !slices.Equal(got, []string{"work"}) {
		t.Fatalf("sessions left on the server = %q, want only [work]", got)
	}
	if n := len(strings.Fields(run("list-windows", "-t", "=work", "-F", "#{window_id}"))); n != 3 {
		t.Fatalf("the operator's session has %d windows after the views closed, want 3", n)
	}
}

// TestTmuxAViewerDoesNotKeepAnEndedSessionsWindowAlive: a viewer's session
// LINKS the window it shows, so when the operator ends that window's session
// tmux keeps the window — on nobody's list, its program running on. The host
// notices on its next refresh that it no longer lists the window, ends the
// viewer, and the window goes with it: what the operator asked for happens.
//
// The control is in the same test: the moment after the kill, BEFORE the host
// has refreshed, the window is still on the server — so "it is gone" below is
// the host's doing, not tmux's.
func TestTmuxAViewerDoesNotKeepAnEndedSessionsWindowAlive(t *testing.T) {
	sock, run := privateTmux(t)
	run("new-session", "-d", "-s", "keep", "-x", "100", "-y", "30", "sh", "-c", "exec cat") // keeps the server up
	run("new-session", "-d", "-s", "work", "-x", "100", "-y", "30", "sh", "-c", "printf 'DOOMED-WINDOW\\n'; exec cat")
	w := strings.TrimSpace(run("display-message", "-p", "-t", "=work:", "#{window_id}"))
	windows := func() string { return run("list-windows", "-a", "-F", "#{window_id}") }

	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "workstation")
	// A SLOW refresh, so the moment between the kill and the host noticing is
	// long enough to look at.
	c, err := mirrorhost.New(mirrorhost.Config{URL: r.srv.URL, Token: tok, Host: "workstation", Backend: mirrorhost.NoShrink(mirrorhost.Tmux{Socket: sock}),
		AllowHTTP: true, Refresh: 1500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	go func() { _ = c.RunOnce(ctx) }()
	r.online(t, 1, "workstation", "work")

	_, screen, _, _, res := attachWindow(t, asUser(1), r.svc, "workstation", "work", w, true)
	waitLong(t, "the viewer to see the window", func() bool { return strings.Contains(screen.String(), "DOOMED-WINDOW") })

	run("kill-session", "-t", "=work")
	if !strings.Contains(windows(), w) {
		t.Fatalf("CONTROL: straight after kill-session the window %s is already gone (%q) — tmux removed it, so this test shows nothing about the host", w, windows())
	}
	waitLong(t, "the host to end the viewer, and the window with it", func() bool { return !strings.Contains(windows(), w) })
	_ = within(t, res, "the viewer's terminal to end")
	if got := strings.Fields(run("list-sessions", "-F", "#{session_name}")); !slices.Equal(got, []string{"keep"}) {
		t.Fatalf("sessions left = %q, want only [keep]", got)
	}
}

// TestTmuxAMessageSentWhileScrolledIsOnScreen is the composer's send into a
// pane a drag-to-scroll left in copy-mode, over the WHOLE chain with real tmux:
// the paste lands in the program either way, but while the pane is in
// copy-mode it goes on showing old history, so the message and its answer are
// out of view. The copy-mode THIS host entered is cancelled before the paste;
// copy-mode the operator entered is not.
func TestTmuxAMessageSentWhileScrolledIsOnScreen(t *testing.T) {
	sock, run := privateTmux(t)
	run("new-session", "-d", "-s", "work", "-x", "80", "-y", "20", "sh", "-c", "seq 1 300; exec cat")
	window := strings.TrimSpace(run("display-message", "-p", "-t", "=work:", "#{window_id}"))
	pane := strings.TrimSpace(run("display-message", "-p", "-t", "=work:", "#{pane_id}"))
	waitLong(t, "the pane's history", func() bool {
		h, _ := strconv.Atoi(strings.TrimSpace(run("display-message", "-p", "-t", pane, "#{history_size}")))
		return h >= 200
	})
	inMode := func() string { return strings.TrimSpace(run("display-message", "-p", "-t", pane, "#{pane_in_mode}")) }

	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "workstation")
	r.connect(t, "workstation", tok, mirrorhost.Tmux{Socket: sock}, true)
	r.online(t, 1, "workstation", "work")

	// 1. This host's copy-mode: a scroll back, then a message.
	if err := r.svc.ScrollWindow(asUser(1), "workstation", "work", window, 10); err != nil {
		t.Fatalf("scroll: %v", err)
	}
	if got := inMode(); got != "1" {
		t.Fatalf("fixture: after the scroll pane_in_mode=%s, want 1", got)
	}
	if err := r.svc.PasteWindow(asUser(1), "workstation", "work", window, "SENT-WHILE-SCROLLED-7c", mirror.PasteActivePane); err != nil {
		t.Fatalf("paste: %v", err)
	}
	if got := inMode(); got != "0" {
		t.Fatalf("after a message into the copy-mode this host entered, pane_in_mode=%s, want 0 (cancelled first)", got)
	}
	waitLong(t, "the message on screen", func() bool {
		return strings.Contains(run("capture-pane", "-p", "-t", pane), "SENT-WHILE-SCROLLED-7c")
	})

	// 2. The operator's own copy-mode (prefix-[): scrolled by muster, but a
	// message does not end it.
	run("copy-mode", "-t", pane)
	if err := r.svc.ScrollWindow(asUser(1), "workstation", "work", window, 4); err != nil {
		t.Fatalf("scroll the operator's copy-mode: %v", err)
	}
	if err := r.svc.PasteWindow(asUser(1), "workstation", "work", window, "SENT-INTO-OPERATORS-9d", mirror.PasteActivePane); err != nil {
		t.Fatalf("paste: %v", err)
	}
	if got := inMode(); got != "1" {
		t.Fatalf("a message ended the OPERATOR's copy-mode: pane_in_mode=%s, want 1", got)
	}
}

// TestTmuxAClaudeMessageSentWhileScrolledIsOnScreen is the same over the OTHER
// send path: the rendered view's composer (mirror.PasteClaude), which the host
// resolves to the window's Claude Code pane (mirrorhost/transcript.go) rather
// than to the active pane. The copy-mode THIS host entered is cancelled before
// the paste and the message is on screen; copy-mode the operator entered is
// left alone.
func TestTmuxAClaudeMessageSentWhileScrolledIsOnScreen(t *testing.T) {
	sock, run := privateTmux(t)
	home, cwd := t.TempDir(), t.TempDir()
	project := filepath.Join(home, "projects", mirrorhost.ProjectSlug(cwd))
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "sess-s.jsonl"), []byte(promptLine("a scrolled conversation")), 0o644); err != nil {
		t.Fatal(err)
	}
	// History first, then the fake CLI in the SAME process (exec), so the pane
	// is a Claude Code pane with 300 lines to scroll back through.
	claude := strings.Join(tunnelClaude(t, t.TempDir(), home, "sess-s"), " ")
	run("new-session", "-d", "-s", "work", "-x", "80", "-y", "20", "-c", cwd, "sh", "-c", "seq 1 300; exec "+claude)
	window := strings.TrimSpace(run("display-message", "-p", "-t", "=work:", "#{window_id}"))
	pane := strings.TrimSpace(run("display-message", "-p", "-t", "=work:", "#{pane_id}"))
	screen := func() string { return run("capture-pane", "-p", "-t", pane) }
	waitLong(t, "the fake CLI after its history", func() bool {
		h, _ := strconv.Atoi(strings.TrimSpace(run("display-message", "-p", "-t", pane, "#{history_size}")))
		return h >= 200 && strings.Contains(screen(), "FAKE-CLAUDE sess-s")
	})
	inMode := func() string { return strings.TrimSpace(run("display-message", "-p", "-t", pane, "#{pane_in_mode}")) }

	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "workstation")
	r.connect(t, "workstation", tok, mirrorhost.Tmux{Socket: sock, Claude: mirrorhost.ClaudeLocator{Home: home}}, true)
	waitLong(t, "the window listed as Claude Code", func() bool {
		hs := r.svc.Hosts(asUser(1))
		if len(hs) != 1 {
			return false
		}
		e, ok := hs[0].Find("work", window)
		return ok && e.Claude
	})

	// 1. This host's copy-mode: a scroll back, then a message from the composer.
	if err := r.svc.ScrollWindow(asUser(1), "workstation", "work", window, 10); err != nil {
		t.Fatalf("scroll: %v", err)
	}
	if got := inMode(); got != "1" {
		t.Fatalf("fixture: after the scroll pane_in_mode=%s, want 1", got)
	}
	if err := r.svc.PasteWindow(asUser(1), "workstation", "work", window, "CLAUDE-SENT-WHILE-SCROLLED-3e", mirror.PasteClaude); err != nil {
		t.Fatalf("paste: %v", err)
	}
	if got := inMode(); got != "0" {
		t.Fatalf("after a Claude message into the copy-mode this host entered, pane_in_mode=%s, want 0 (cancelled first)", got)
	}
	waitLong(t, "the message on screen", func() bool { return strings.Contains(screen(), "CLAUDE-SENT-WHILE-SCROLLED-3e") })

	// 2. The operator's own copy-mode (prefix-[): scrolled by muster, but a
	// message does not end it.
	run("copy-mode", "-t", pane)
	if err := r.svc.ScrollWindow(asUser(1), "workstation", "work", window, 4); err != nil {
		t.Fatalf("scroll the operator's copy-mode: %v", err)
	}
	if err := r.svc.PasteWindow(asUser(1), "workstation", "work", window, "CLAUDE-SENT-INTO-OPERATORS-5b", mirror.PasteClaude); err != nil {
		t.Fatalf("paste: %v", err)
	}
	waitLong(t, "the second message reached the program", func() bool { return strings.Contains(screen(), "CLAUDE-SENT-INTO-OPERATORS-5b") })
	if got := inMode(); got != "1" {
		t.Fatalf("a Claude message ended the OPERATOR's copy-mode: pane_in_mode=%s, want 1", got)
	}
}
