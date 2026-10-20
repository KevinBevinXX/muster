package mirrorhost

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/hermeshost"
	"github.com/ZacxDev/muster/internal/hermeshost/hermestest"
	"github.com/ZacxDev/muster/internal/mirror"
)

// hermesPane is a fake proc tree with one pane (%1, root 10 on /dev/pts/4)
// running a Hermes CLI (pid 11) that started 1 s after a boot at bootAt, and
// a Hermes home and status directory for a Tmux to resolve it from.
type hermesPane struct {
	proc   *fakeProc
	home   *hermestest.Home
	status hermeshost.StatusDir
	tmux   Tmux
	bootAt time.Time
}

const hermesTTY = "/dev/pts/4"

func newHermesPane(t *testing.T) *hermesPane {
	t.Helper()
	proc := newFakeProc(t)
	// The fixture stat's start time is 100 ticks: the CLI started 1 s after
	// boot, which was a minute ago.
	boot := time.Now().Add(-time.Minute).Truncate(time.Second)
	must(t, os.WriteFile(filepath.Join(proc.root, "stat"), []byte("cpu 1 2 3\nbtime "+itoa(int(boot.Unix()))+"\n"), 0o644))
	proc.add(10, 1, "zsh", "/w", "-zsh")
	proc.stdin(10, hermesTTY)
	proc.add(11, 10, "hermes", "/w", "/usr/bin/python3", "/usr/local/bin/hermes", "chat")
	proc.stdin(11, hermesTTY)
	// The gateway, which runs the hook: the status files' writer.
	proc.add(50, 1, "hermes", "/w", "/usr/bin/python3", "/usr/local/bin/hermes", "gateway", "run")
	h := hermestest.New(t)
	status := hermeshost.StatusDir(t.TempDir())
	src := NewHermesSource(h.Dir, string(status))
	t.Cleanup(func() { _ = src.State().Close() })
	return &hermesPane{proc: proc, home: h, status: status, bootAt: boot,
		tmux: Tmux{Claude: ClaudeLocator{Proc: proc.root, Home: t.TempDir()}, Hermes: src}}
}

func (p *hermesPane) windows(t *testing.T) []WindowInfo {
	t.Helper()
	ws := []WindowInfo{{Session: "work", ID: "@1", Active: true, Pane: "%1", PanePID: 10, TTY: hermesTTY, Command: "python3.14"}}
	procs, children := p.tmux.Claude.table()
	attachAgents(ws, procs, children, p.tmux.runtimes()[1:])
	return ws
}

// gatewayStart is the fixture gateway's /proc start time (field 22).
const gatewayStart = "100"

func TestAHermesWindowCarriesItsSessionAndStatus(t *testing.T) {
	p := newHermesPane(t)
	root := hermestest.LocalID()
	p.home.Session(root, hermestest.SessionOpts{})
	p.home.Breadcrumb(hermesTTY, root, hermestest.Now())
	since := time.Now().UnixMilli()
	must(t, p.status.Write(hermeshost.StatusFile{SessionID: root, Status: mirror.StatusIdle, Since: since, WriterPID: 50, WriterStart: gatewayStart}))
	ws := p.windows(t)
	a := ws[0].Agent
	if a == nil || a.Gen != root || a.Status != mirror.StatusIdle || a.StatusSince != since || a.Source != SourceBreadcrumb {
		t.Fatalf("Agent = %+v (hermes %+v)", a, a.Hermes)
	}
	w := WireWindows(ws, nil, true, time.Now())[0]
	if w.Runtime != mirror.RuntimeHermes || w.AgentSession != root || w.Status != mirror.StatusIdle || w.StatusSince != since || w.Claude {
		t.Fatalf("on the wire: %+v", w)
	}
	if w.SessionKey() != "hermes:"+root {
		t.Fatalf("SessionKey = %q", w.SessionKey())
	}

	// A busy file is believed while the gateway lives and the root's lease is.
	p.home.Lease(root, hermestest.Now()+300)
	must(t, p.status.Write(hermeshost.StatusFile{SessionID: root, Status: mirror.StatusBusy, Since: since + 1, WriterPID: 50, WriterStart: gatewayStart}))
	if a := p.windows(t)[0].Agent; a.Status != mirror.StatusBusy || !a.Busy {
		t.Fatalf("busy: %+v", a)
	}
	// …and is unknown once the gateway that wrote it is gone.
	must(t, os.RemoveAll(filepath.Join(p.proc.root, "50")))
	if a := p.windows(t)[0].Agent; a.Status != "" || a.StatusSince != 0 || a.Gen != root {
		t.Fatalf("stale busy: %+v (%s)", a, a.Hermes.Status.Reason)
	}
}

// 🔴 /resume or /branch at the terminal leaves the breadcrumb on the
// abandoned session: the window keeps its Hermes runtime but loses its
// session id (no rendered view) AND its status, though the abandoned
// session's own file says idle.
func TestASwitchedHermesWindowHasNoSessionAndUnknownStatus(t *testing.T) {
	p := newHermesPane(t)
	root := hermestest.LocalID()
	p.home.Session(root, hermestest.SessionOpts{})
	p.home.Breadcrumb(hermesTTY, root, hermestest.Now())
	must(t, p.status.Write(hermeshost.StatusFile{SessionID: root, Status: mirror.StatusIdle, Since: time.Now().UnixMilli(), WriterPID: 50, WriterStart: gatewayStart}))
	p.home.End(root, "resumed_other")
	ws := p.windows(t)
	a := ws[0].Agent
	if a == nil || a.Runtime != mirror.RuntimeHermes || a.Gen != "" || a.Status != "" || a.StatusSince != 0 || a.Source != SourceProcess {
		t.Fatalf("Agent = %+v", a)
	}
	if a.Hermes == nil || a.Hermes.NoView != hermeshost.NoViewSwitched {
		t.Fatalf("reason: %+v", a.Hermes)
	}
	w := WireWindows(ws, nil, true, time.Now())[0]
	if w.AgentSession != "" || w.Status != "" || w.Runtime != mirror.RuntimeHermes {
		t.Fatalf("on the wire: %+v", w)
	}
}

// The breadcrumb of a PREVIOUS CLI on the same terminal (written before this
// one started) is not this CLI's.
func TestABreadcrumbFromBeforeTheCLIStartedIsNotItsSession(t *testing.T) {
	p := newHermesPane(t)
	root := hermestest.LocalID()
	p.home.Session(root, hermestest.SessionOpts{})
	// Written before boot+1s, when the fixture CLI started.
	p.home.Breadcrumb(hermesTTY, root, float64(p.bootAt.Unix())-5)
	a := p.windows(t)[0].Agent
	if a.Gen != "" || a.Hermes.NoView != hermeshost.NoViewOldBreadcrumb {
		t.Fatalf("Agent = %+v (%+v)", a, a.Hermes)
	}
}

func TestProcStartTimeIsBootPlusTicks(t *testing.T) {
	root := t.TempDir()
	must(t, os.WriteFile(filepath.Join(root, "stat"), []byte("cpu 0\nintr 1\nbtime 1700000000\nprocesses 9\n"), 0o644))
	if got := procStartTime(root, "250"); !got.Equal(time.Unix(1700000002, 500_000_000)) {
		t.Fatalf("procStartTime = %v", got)
	}
	if !procStartTime(root, "").IsZero() || !procStartTime(t.TempDir(), "250").IsZero() {
		t.Fatal("an unreadable start is not zero")
	}
}
