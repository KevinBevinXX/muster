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

// fakeHermesCLI writes an executable script named hermes whose shebang is a
// REAL Python interpreter, in a fresh directory: it names its own process
// "hermes" (its comm, as the Hermes CLI's is, S10), prints HERMES-READY, then
// appends every line it reads to got. It runs as [<python>, <path>/hermes,
// <args>] — exactly the Hermes CLI's shape, with nothing of Hermes in it.
//
// ⚠ THE COMM IS SET BY THE SCRIPT, NOT BY THE SHEBANG: an interpreter that is
// itself a wrapper (Nix's python3 env re-execs the real binary) leaves comm
// as the binary's name, measured "python3.14".
func fakeHermesCLI(t *testing.T, got string) string {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is not installed; this case fails rather than skips: %v", err)
	}
	if py, err = filepath.Abs(py); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "hermes")
	script := "#!" + py + "\nimport sys\nopen('/proc/self/comm', 'w').write('hermes')\nprint('HERMES-READY', flush=True)\n" +
		"for line in sys.stdin:\n    open(" + quotePy(got) + ", 'a').write(line)\n"
	must(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

func quotePy(s string) string { return "'" + strings.ReplaceAll(s, "'", "\\'") + "'" }

// TestTmuxAHermesWindowIsListedAndNeverPastedInto is the runtime seam against
// REAL tmux on a private socket. A window whose pane runs a Hermes-shaped CLI
// (under `sh -c`, so the claim walks below #{pane_pid}) is listed as a Hermes
// window — Runtime "hermes", NOT Claude, no status, no session — and the hello
// advertises no hermesSend; a plain shell window beside it is no agent at all.
// A Terminal paste into the Hermes window is refused and reaches nothing.
func TestTmuxAHermesWindowIsListedAndNeverPastedInto(t *testing.T) {
	r := newTmuxRig(t)
	got := filepath.Join(t.TempDir(), "hermes.got")
	cli := fakeHermesCLI(t, got)
	r.run("new-session", "-d", "-s", "agent", "-x", "120", "-y", "40", "sh", "-c", "'"+cli+"' chat; exit")
	waitFor(t, "the fake Hermes to start", func() bool { return strings.Contains(r.pane("agent"), "HERMES-READY") })
	r.session("plain", "PLAIN-READY")
	waitFor(t, "the plain pane to start", func() bool { return strings.Contains(r.pane("plain"), "PLAIN-READY") })

	b := Tmux{Socket: r.sock}
	ws, err := b.Windows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]WindowInfo{}
	for _, w := range ws {
		byName[w.Session] = w
	}
	if a := byName["agent"].Agent; a == nil || a.Runtime != mirror.RuntimeHermes || byName["agent"].Claude != nil {
		t.Fatalf("the Hermes window: Agent %+v, Claude %+v; want a Hermes agent and no Claude", a, byName["agent"].Claude)
	} else if comm, _ := os.ReadFile("/proc/" + itoa(a.PID) + "/comm"); strings.TrimSpace(string(comm)) != "hermes" {
		t.Fatalf("instrument check: the claimed process is %q, not the fake hermes", comm)
	}
	if w := byName["plain"]; w.Agent != nil || w.Claude != nil {
		t.Fatalf("the plain window claims an agent: %+v / %+v", w.Agent, w.Claude)
	}

	d := newDrivenMuster(t)
	d.run(t, b, nil, true)
	hello := d.of(mirror.TypeHello)[0]
	if hello.HermesSend {
		t.Fatal("a host without -hermes-send advertised hermesSend")
	}
	var hw *mirror.Window
	for i, w := range hello.Windows {
		if w.Session == "agent" {
			hw = &hello.Windows[i]
		}
	}
	if hw == nil || hw.Runtime != mirror.RuntimeHermes || hw.Claude || hw.Status != "" || hw.AgentSession != "" {
		t.Fatalf("the hello's Hermes window = %+v; want Runtime hermes, not Claude, no status or session", hw)
	}

	d.send(t, mirror.Msg{Type: mirror.TypePaste, ID: 1, Session: "agent", Window: hw.ID, Text: "4 deny-this-please", AnyPane: true})
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
	if a.Refusal != mirror.PasteRefusalHermes {
		t.Fatalf("a Terminal paste into the Hermes window answered %q (kind %q); want the Hermes refusal", a.Error, a.Refusal)
	}
	time.Sleep(400 * time.Millisecond)
	if s := readFile(t, got); s != "" {
		t.Fatalf("the fake Hermes received %q", s)
	}
}
