//go:build tmuxit

package mirrorhost

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// fakeAgent writes an executable script named name in a fresh directory: it
// prints READY, then appends every line it reads to got. Run from a shebang,
// the kernel names the process after the SCRIPT (comm = name) and passes the
// script's path as argv[1] — exactly the shape the Nix-wrapped hermes has
// (S10), with nothing of Hermes in it.
func fakeAgent(t *testing.T, name, got string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	script := "#!/bin/sh\nprintf '%s\\n' " + strings.ToUpper(name) + "-READY\n" +
		"while IFS= read -r line; do printf '%s\\n' \"$line\" >> '" + got + "'; done\n"
	must(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

// TestTmuxATerminalPasteIntoAHermesPaneReachesNothing is the host paste guard
// against REAL tmux, through the host's real paste entry (a driven muster
// sending what the Terminal view's composer sends: AnyPane). Two sessions run
// the SAME fake agent under two names, each a child of `sh -c` so the guard
// must walk below #{pane_pid}: `notes` (the control) and `hermes`. The control's
// paste arrives — so the rig can see a paste — and the hermes one is refused
// with the guard's message, and neither its process, its screen (a pasted line
// is echoed by the tty) nor tmux's buffers ever hold the text.
func TestTmuxATerminalPasteIntoAHermesPaneReachesNothing(t *testing.T) {
	r := newTmuxRig(t)
	dir := t.TempDir()
	notesGot, hermesGot := filepath.Join(dir, "notes.got"), filepath.Join(dir, "hermes.got")
	for _, s := range []struct{ name, got string }{{"notes", notesGot}, {"hermes", hermesGot}} {
		agent := fakeAgent(t, s.name, s.got)
		// `; exit` keeps sh as the pane's root process, the agent its child.
		r.run("new-session", "-d", "-s", s.name, "-x", "120", "-y", "40", "sh", "-c", "'"+agent+"' chat; exit")
		waitFor(t, s.name+" to start", func() bool { return strings.Contains(r.pane(s.name), strings.ToUpper(s.name)+"-READY") })
		root := strings.TrimSpace(r.run("display-message", "-p", "-t", "="+s.name+":", "#{pane_pid}"))
		if comm, _ := os.ReadFile("/proc/" + root + "/comm"); strings.TrimSpace(string(comm)) != "sh" {
			t.Fatalf("instrument check: %s's pane root is %q, not sh — the walk below it would not be exercised", s.name, comm)
		}
	}
	b := Tmux{Socket: r.sock}
	d := newDrivenMuster(t)
	d.run(t, b, nil, true)
	answer := func(id uint32) mirror.Msg {
		var out mirror.Msg
		waitUntil(t, "the paste's answer", func() bool {
			for _, m := range d.of(mirror.TypePasted) {
				if m.ID == id {
					out = m
					return true
				}
			}
			return false
		})
		return out
	}

	const control = "a note for the control pane"
	d.send(t, mirror.Msg{Type: mirror.TypePaste, ID: 1, Session: "notes", Text: control, AnyPane: true})
	if a := answer(1); a.Error != "" {
		t.Fatalf("the control paste (an agent NOT named hermes) was refused: %s", a.Error)
	}
	waitFor(t, "the control paste to be submitted", func() bool { return readFile(t, notesGot) == control+"\n" })

	const msg = "4 deny-this-please"
	d.send(t, mirror.Msg{Type: mirror.TypePaste, ID: 2, Session: "hermes", Text: msg, AnyPane: true})
	a := answer(2)
	if !strings.Contains(a.Error, "Hermes runs in this pane") {
		t.Fatalf("a Terminal paste into the hermes pane answered %q; want the Hermes refusal", a.Error)
	}
	time.Sleep(400 * time.Millisecond) // a paste and its Enter (150 ms apart) would have landed by now
	if got := readFile(t, hermesGot); got != "" {
		t.Fatalf("the hermes process received %q", got)
	}
	if p := r.run("capture-pane", "-p", "-J", "-t", "=hermes:"); strings.Contains(p, "deny-this-please") {
		t.Fatalf("the text reached the hermes pane:\n%s", p)
	}
	if out := r.run("list-buffers"); strings.Contains(out, "muster-mirror-") {
		t.Fatalf("a refused paste loaded a tmux buffer: %s", out)
	}
}

// TestTmuxAPasteIntoANestedMultiplexerIsRefused is the bypass the guard's walk
// alone cannot see, against REAL tmux on TWO private sockets: the rig's pane
// runs a tmux CLIENT of another server (`TMUX= tmux -L <inner> new-session`),
// and the fake hermes runs in THAT server's pane — a child of the inner
// server, not of anything in the rig's pane. The paste is refused as a
// multiplexer, and nothing reaches the inner hermes.
func TestTmuxAPasteIntoANestedMultiplexerIsRefused(t *testing.T) {
	r := newTmuxRig(t)
	inner := r.sock + "-inner"
	t.Cleanup(func() {
		c := exec.Command("tmux", "-L", inner, "kill-server")
		c.Env = tmuxEnv()
		_ = c.Run()
	})
	got := filepath.Join(t.TempDir(), "hermes.got")
	agent := fakeAgent(t, "hermes", got)
	r.run("new-session", "-d", "-s", "outer", "-x", "120", "-y", "40",
		"env", "-u", "TMUX", "tmux", "-f", "/dev/null", "-L", inner, "new-session", "-s", "inner", "'"+agent+"' chat")
	innerPane := func() string {
		c := exec.Command("tmux", "-L", inner, "capture-pane", "-p", "-J", "-t", "=inner:")
		c.Env = tmuxEnv()
		out, _ := c.CombinedOutput()
		return string(out)
	}
	waitFor(t, "the inner hermes to start", func() bool { return strings.Contains(innerPane(), "HERMES-READY") })
	// Instrument check: hermes is NOT in the outer pane's tree, so only the
	// multiplexer rule can refuse this paste.
	root := strings.TrimSpace(r.run("display-message", "-p", "-t", "=outer:", "#{pane_pid}"))
	if comm, _ := os.ReadFile("/proc/" + root + "/comm"); !strings.HasPrefix(strings.TrimSpace(string(comm)), "tmux") {
		t.Fatalf("instrument check: the outer pane's root is %q, not a tmux client", comm)
	}

	d := newDrivenMuster(t)
	d.run(t, Tmux{Socket: r.sock}, nil, true)
	d.send(t, mirror.Msg{Type: mirror.TypePaste, ID: 1, Session: "outer", Text: "4 deny-this-please", AnyPane: true})
	var a mirror.Msg
	waitUntil(t, "the paste's answer", func() bool {
		for _, m := range d.of(mirror.TypePasted) {
			if m.ID == 1 {
				a = m
				return true
			}
		}
		return false
	})
	if a.Error != ErrMultiplexerPane.Error() || a.Refusal != mirror.PasteRefusalMultiplexer {
		t.Fatalf("a paste into a pane running another tmux's client answered %q (kind %q); want the multiplexer refusal", a.Error, a.Refusal)
	}
	time.Sleep(400 * time.Millisecond)
	if s := readFile(t, got); s != "" {
		t.Fatalf("the inner hermes received %q", s)
	}
	if p := innerPane(); strings.Contains(p, "deny-this-please") {
		t.Fatalf("the text reached the inner pane:\n%s", p)
	}
}
