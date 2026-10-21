//go:build tmuxit

package mirrorhost

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestTmuxThePasteGuardRefusesAPaneShowingHermesChrome is the guard's chrome
// check (Phase 3; Tmux.PaneHermes), against REAL tmux on a private socket:
// two panes run nothing Hermes-shaped (a shell printing a capture, then
// `cat`), one showing Hermes's idle screen as the pinned CLI drew it — a
// Hermes the process walk cannot see, say over ssh — and one Claude Code's
// idle screen, whose ❯ between two rules looks alike. The Hermes screen is
// refused with the guard's Hermes refusal and nothing reaches its `cat`; the
// Claude Code one is pasted into (the control: the check keys on Hermes's
// ☤, not on the frame).
func TestTmuxThePasteGuardRefusesAPaneShowingHermesChrome(t *testing.T) {
	r := newTmuxRig(t)
	dir := t.TempDir()
	hermesScreen := filepath.Join("..", "hermeshost", "testdata", "screens", "idle.ansi")
	claudeScreen := filepath.Join("..", "askdrive", "testdata", "single-done.txt")
	for _, s := range []struct{ name, screen string }{{"lookshermes", hermesScreen}, {"looksclaude", claudeScreen}} {
		abs, err := filepath.Abs(s.screen)
		if err != nil {
			t.Fatal(err)
		}
		got := filepath.Join(dir, s.name+".got")
		// The capture is printed (escapes and all), then the pane waits in
		// `cat`, appending what it reads.
		r.run("new-session", "-d", "-s", s.name, "-x", "160", "-y", "45", "sh", "-c",
			"clear; cat '"+abs+"'; stty -echo; cat > '"+got+"'")
		waitFor(t, s.name+" to draw", func() bool { return strings.Contains(r.pane(s.name), "─────") })
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
	d.send(t, mirror.Msg{Type: mirror.TypePaste, ID: 1, Session: "looksclaude", Text: "to the claude-looking pane", AnyPane: true})
	if a := answer(1); a.Error != "" {
		t.Fatalf("the control (Claude Code's screen) was refused: %s", a.Error)
	}
	waitFor(t, "the control paste to arrive", func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, "looksclaude.got"))
		return strings.Contains(string(b), "to the claude-looking pane")
	})

	d.send(t, mirror.Msg{Type: mirror.TypePaste, ID: 2, Session: "lookshermes", Text: "4", AnyPane: true})
	a := answer(2)
	if a.Refusal != mirror.PasteRefusalHermes || !strings.Contains(a.Error, "Hermes runs in this pane") {
		t.Fatalf("a paste into a pane showing Hermes answered %q (%q); want the Hermes refusal", a.Error, a.Refusal)
	}
	time.Sleep(400 * time.Millisecond)
	if b, _ := os.ReadFile(filepath.Join(dir, "lookshermes.got")); len(b) != 0 {
		t.Fatalf("the pane showing Hermes received %q", b)
	}
}

// TestTmuxTheGatedSendRefusesAPaneWithANestedMultiplexer: SendHermes into a
// pane whose tree holds a tmux CLIENT (of another server, where a Hermes
// the status does not describe could run) refuses before anything — no
// status read, no key — and the control, the same send into a plain shell
// pane, gets past that check (it then stops at the status, here an error).
func TestTmuxTheGatedSendRefusesAPaneWithANestedMultiplexer(t *testing.T) {
	r := newTmuxRig(t)
	inner := r.sock + "-inner"
	t.Cleanup(func() {
		c := exec.Command("tmux", "-L", inner, "kill-server")
		c.Env = tmuxEnv()
		_ = c.Run()
	})
	r.run("new-session", "-d", "-s", "outer", "-x", "120", "-y", "40", "sh", "-c", "TMUX= tmux -L '"+inner+"' -f /dev/null new-session -s in sh; exit")
	r.run("new-session", "-d", "-s", "plain", "-x", "120", "-y", "40", "sh")
	waitFor(t, "the nested client", func() bool {
		out, err := exec.Command("tmux", "-L", inner, "ls").CombinedOutput()
		return err == nil && strings.Contains(string(out), "in:")
	})
	b := Tmux{Socket: r.sock, Hermes: NewHermesSource(t.TempDir(), t.TempDir())}
	statusRead := 0
	send := func(session string) error {
		pane := strings.TrimSpace(r.run("display-message", "-p", "-t", "="+session+":", "#{pane_id}"))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return b.SendHermes(ctx, HermesSendRequest{Pane: pane, Since: 1, Text: "hello",
			Status: func(context.Context) (AgentPane, error) { statusRead++; return AgentPane{}, os.ErrNotExist },
			Logf:   t.Logf})
	}
	err := send("outer")
	if kindOf(err) != mirror.PasteRefusalHermesNotSubmitted || !strings.Contains(err.Error(), "terminal multiplexer") || statusRead != 0 {
		t.Fatalf("into the nested client's pane: %v (status read %d times)", err, statusRead)
	}
	err = send("plain")
	if strings.Contains(err.Error(), "terminal multiplexer") || statusRead == 0 {
		t.Fatalf("control, a plain pane: %v (status read %d times)", err, statusRead)
	}
}

// TestTmuxTheGatedSendRechecksThePaneAndTakesTheDriveLock: SendHermes into a
// real pane refuses (1) while another drive holds the pane's lock — before
// any status read — and (2) when the window's pane, re-resolved, is no
// longer the one the send was for (superseded). Nothing is typed either way.
func TestTmuxTheGatedSendRechecksThePaneAndTakesTheDriveLock(t *testing.T) {
	r := newTmuxRig(t)
	got := filepath.Join(t.TempDir(), "got")
	r.run("new-session", "-d", "-s", "plain", "-x", "120", "-y", "40", "sh", "-c", "stty -echo; exec cat > '"+got+"'")
	pane := strings.TrimSpace(r.run("display-message", "-p", "-t", "=plain:", "#{pane_id}"))
	// Settled: until `stty` has exited, its half-gone process makes the
	// guard's walk refuse (fail closed) before the checks under test.
	waitFor(t, "the pane to settle in cat", func() bool {
		return strings.TrimSpace(r.run("display-message", "-p", "-t", pane, "#{pane_current_command}")) == "cat"
	})
	time.Sleep(200 * time.Millisecond)
	b := Tmux{Socket: r.sock, Hermes: NewHermesSource(t.TempDir(), t.TempDir())}
	reads := 0
	send := func(status AgentPane) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return b.SendHermes(ctx, HermesSendRequest{Pane: pane, Since: fakeSince, Text: "hello",
			Status: func(context.Context) (AgentPane, error) { reads++; return status, nil }, Logf: t.Logf})
	}
	moved := idleHermes()
	moved.Pane = "%99999"
	// (1) the drive lock.
	release, ok := lockPane(strings.Join(append(b.server(), pane), " "))
	if !ok {
		t.Fatal("instrument check: the lock was already held")
	}
	err := send(moved)
	release()
	if kindOf(err) != mirror.PasteRefusalHermesNotSubmitted || !strings.Contains(err.Error(), "already typing") || reads != 0 {
		t.Fatalf("with the lock held: %v (status read %d times)", err, reads)
	}
	// (2) the pane moved.
	err = send(moved)
	if kindOf(err) != mirror.PasteRefusalHermesSuperseded || reads == 0 {
		t.Fatalf("a moved pane: %v (status read %d times)", err, reads)
	}
	time.Sleep(300 * time.Millisecond)
	if b, _ := os.ReadFile(got); len(b) != 0 {
		t.Fatalf("the pane received %q", b)
	}
}
