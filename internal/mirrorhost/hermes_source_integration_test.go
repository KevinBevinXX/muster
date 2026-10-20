//go:build tmuxit

package mirrorhost

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/hermeshost"
	"github.com/ZacxDev/muster/internal/hermeshost/hermestest"
	"github.com/ZacxDev/muster/internal/mirror"
)

// fakeHermesWithBreadcrumb is fakeHermesCLI that first writes its terminal's
// breadcrumb into home the way Hermes does (terminal_breadcrumbs.py: named by
// os.ttyname(stdin) sanitised, {session_id, cwd, ts}, ts after the process
// started).
func fakeHermesWithBreadcrumb(t *testing.T, home, session string) string {
	t.Helper()
	cli := fakeHermesCLI(t, filepath.Join(t.TempDir(), "got"))
	crumb := "import os, re, json, time\n" +
		"n = 'tty-' + re.sub(r'[^A-Za-z0-9._-]', '-', os.ttyname(0).strip().strip('/'))[:120]\n" +
		"d = os.path.join(" + quotePy(home) + ", 'terminal-sessions')\nos.makedirs(d, exist_ok=True)\n" +
		"open(os.path.join(d, n), 'w').write(json.dumps({'session_id': " + quotePy(session) + ", 'cwd': '/w', 'ts': time.time()}))\n"
	b, err := os.ReadFile(cli)
	must(t, err)
	shebang, rest, _ := strings.Cut(string(b), "\n")
	must(t, os.WriteFile(cli, []byte(shebang+"\n"+crumb+rest), 0o755))
	return cli
}

// TestTmuxAHermesWindowResolvesItsSessionThroughItsTerminal is Phase 2's
// locator against REAL tmux on a private socket: the listing reads
// #{pane_tty} from tmux, finds the breadcrumb the fake CLI wrote for that
// terminal, resolves its session in a real state.db (the pinned schema) and
// believes the hook's idle; the hello carries the session id and the status.
// A second pane whose breadcrumb names a session ended by /resume is a Hermes
// window with no session and an unknown status.
func TestTmuxAHermesWindowResolvesItsSessionThroughItsTerminal(t *testing.T) {
	r := newTmuxRig(t)
	home := hermestest.New(t)
	live, switched := hermestest.LocalID(), hermestest.LocalID()
	home.Session(live, hermestest.SessionOpts{})
	home.Session(switched, hermestest.SessionOpts{})
	home.End(switched, "resumed_other")
	status := hermeshost.StatusDir(t.TempDir())
	since := time.Now().UnixMilli()
	for _, id := range []string{live, switched} {
		must(t, status.Write(hermeshost.StatusFile{SessionID: id, Status: mirror.StatusIdle, Since: since, WriterPID: 1, WriterStart: "1"}))
	}
	for name, session := range map[string]string{"live": live, "switched": switched} {
		cli := fakeHermesWithBreadcrumb(t, home.Dir, session)
		r.run("new-session", "-d", "-s", name, "-x", "120", "-y", "40", "sh", "-c", "'"+cli+"' chat; exit")
		waitFor(t, "the fake Hermes "+name+" to start", func() bool { return strings.Contains(r.pane(name), "HERMES-READY") })
	}

	src := NewHermesSource(home.Dir, string(status))
	t.Cleanup(func() { _ = src.State().Close() })
	b := Tmux{Socket: r.sock, Hermes: src}
	ws, err := b.Windows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]WindowInfo{}
	for _, w := range ws {
		byName[w.Session] = w
	}
	lw, sw := byName["live"], byName["switched"]
	if !strings.HasPrefix(lw.TTY, "/dev/") {
		t.Fatalf("instrument check: the listing read no pane tty: %+v", lw)
	}
	if a := lw.Agent; a == nil || a.Gen != live || a.Status != mirror.StatusIdle || a.StatusSince != since || a.Source != SourceBreadcrumb {
		t.Fatalf("the live Hermes window: %+v", a)
	}
	if a := sw.Agent; a == nil || a.Runtime != mirror.RuntimeHermes || a.Gen != "" || a.Status != "" || a.Hermes == nil || a.Hermes.NoView != hermeshost.NoViewSwitched {
		t.Fatalf("the switched Hermes window: %+v", a)
	}

	d := newDrivenMuster(t)
	d.run(t, b, nil, true)
	seen := 0
	for _, w := range d.of(mirror.TypeHello)[0].Windows {
		switch w.Session {
		case "live":
			seen++
			if w.Runtime != mirror.RuntimeHermes || w.AgentSession != live || w.Status != mirror.StatusIdle || w.StatusSince != since {
				t.Fatalf("the hello's live window = %+v", w)
			}
		case "switched":
			seen++
			if w.Runtime != mirror.RuntimeHermes || w.AgentSession != "" || w.Status != "" {
				t.Fatalf("the hello's switched window = %+v", w)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("instrument check: the hello listed %d of the two Hermes windows", seen)
	}
}
