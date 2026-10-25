package mirrorhost

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE RUNTIME SEAM's host half (runtime.go) and the strict Hermes mirroring
// rule (hermes.go's hermesCLIShape and HermesIdentity.CLI). All fixtures are
// synthetic: a fake proc tree, no Hermes, no tmux server.

// stdin gives fake process pid a stdin (fd 0) that is tty.
func (f *fakeProc) stdin(pid int, tty string) {
	f.t.Helper()
	dir := filepath.Join(f.root, strconv.Itoa(pid), "fd")
	must(f.t, os.MkdirAll(dir, 0o755))
	must(f.t, os.Symlink(tty, filepath.Join(dir, "0")))
}

// The strict rule's argv cases. comm is "hermes" unless the case says.
var strictCases = []struct {
	name string
	comm string
	argv []string
	want bool
}{
	{"the Nix wrapper's exec, `hermes chat`", "hermes", []string{"/nix/store/x-python3-3.14/bin/python3.14", "/nix/store/y-hermes/bin/hermes", "chat"}, true},
	{"a venv console script, `hermes chat`", "hermes", []string{"/srv/venv/bin/python", "/srv/venv/bin/hermes", "chat", "--resume"}, true},
	{"the bare form, `hermes`", "hermes", []string{"/usr/bin/python3", "/usr/local/bin/hermes"}, true},
	{"a profile-launched CLI (argv[2] is -p)", "hermes", []string{"/usr/bin/python3", "/usr/local/bin/hermes", "-p", "coder", "chat"}, false},
	{"the gateway, `hermes gateway run`", "hermes", []string{"/usr/bin/python3", "/usr/local/bin/hermes", "gateway", "run"}, false},
	{"`chat` with gateway later in argv", "hermes", []string{"/usr/bin/python3", "/usr/local/bin/hermes", "chat", "gateway"}, false},
	{"another subcommand, `hermes mcp serve`", "hermes", []string{"/usr/bin/python3", "/usr/local/bin/hermes", "mcp", "serve"}, false},
	{"comm is python, `python -m hermes_cli.main chat`", "python3", []string{"/usr/bin/python3", "-m", "hermes_cli.main", "chat"}, false},
	{"argv[0] is not a Python", "hermes", []string{"/usr/local/bin/hermes", "/usr/local/bin/hermes", "chat"}, false},
	{"argv[1] is not the launcher", "hermes", []string{"/usr/bin/python3", "/srv/other.py", "chat"}, false},
	{"no argv[1]", "hermes", []string{"/usr/bin/python3"}, false},
}

// TestHermesCLIShapeIsTheStrictRule: the CLI's own identity (S10) — comm
// hermes, a Python running the hermes launcher, argv[2] chat or absent, no
// gateway — and nothing looser. Every field of a near miss differs from the
// match in exactly the clause it names.
func TestHermesCLIShapeIsTheStrictRule(t *testing.T) {
	matched := 0
	for _, c := range strictCases {
		got := hermesCLIShape(procInfo{comm: c.comm, argv: c.argv})
		if got != c.want {
			t.Errorf("%s: hermesCLIShape = %t, want %t", c.name, got, c.want)
		}
		if got {
			matched++
		}
	}
	if matched == 0 {
		t.Fatal("instrument check: no case matched, so the rule was never seen to admit anything")
	}
}

// TestEveryStrictHermesCLIIsRefusedByTheBroadGuard: the strict rule is never
// looser than the paste guard — every process it would claim as a Hermes
// window, the guard refuses to paste into — and the strict rule's near misses
// that are still Hermes (the gateway, a profile CLI, `-m hermes_cli`) are
// refused by the guard too. An INVARIANT GUARD between the two rules.
func TestEveryStrictHermesCLIIsRefusedByTheBroadGuard(t *testing.T) {
	for _, c := range strictCases {
		p := procInfo{comm: c.comm, argv: c.argv}
		_, broad := hermesShape(p, func() string { return "" })
		if hermesCLIShape(p) && !broad {
			t.Errorf("%s: claimed as a Hermes window by the strict rule, yet the paste guard admits it", c.name)
		}
	}
	for _, name := range []string{"a profile-launched CLI (argv[2] is -p)", "the gateway, `hermes gateway run`", "comm is python, `python -m hermes_cli.main chat`"} {
		for _, c := range strictCases {
			if c.name != name {
				continue
			}
			if _, broad := hermesShape(procInfo{comm: c.comm, argv: c.argv}, func() string { return "" }); !broad {
				t.Errorf("%s: not a Hermes window, and the paste guard admits it too", name)
			}
		}
	}
}

// TestCLIClaimsOnlyTheHermesOnThePanesTerminal: in one pane's tree, a Hermes
// CLI whose stdin is ANOTHER terminal (a backgrounded one, `</dev/null`) is
// not the pane's; the one on the pane's tty is, under a shell and a wrapper.
// The gateway on the pane's tty is never claimed. A tree with no Hermes and an
// empty tty claim nothing.
func TestCLIClaimsOnlyTheHermesOnThePanesTerminal(t *testing.T) {
	proc := newFakeProc(t)
	const tty = "/dev/pts/7"
	py, launcher := "/usr/bin/python3", "/usr/local/bin/hermes"
	proc.add(100, 1, "zsh", "/w", "-zsh")
	proc.stdin(100, tty)
	proc.add(110, 100, "hermes", "/w", py, launcher, "chat") // backgrounded: stdin elsewhere
	proc.stdin(110, "/dev/null")
	proc.add(120, 100, "hermes", "/w", py, launcher, "gateway", "run")
	proc.stdin(120, tty)
	proc.add(130, 100, "sh", "/w", "sh", "-c", "hermes chat")
	proc.stdin(130, tty)
	proc.add(140, 130, "hermes", "/w", py, launcher, "chat")
	proc.stdin(140, tty)
	id := HermesIdentity{Proc: proc.root}

	if pid, ok := id.CLI(100, tty); !ok || pid != 140 {
		t.Fatalf("CLI(pane root, its tty) = %d, %t; want the chat on the pane's terminal, 140", pid, ok)
	}
	if pid, ok := id.CLI(100, "/dev/pts/8"); ok {
		t.Fatalf("CLI with another terminal claimed %d", pid)
	}
	if pid, ok := id.CLI(100, ""); ok {
		t.Fatalf("CLI with no terminal claimed %d", pid)
	}
	proc.add(200, 1, "zsh", "/w", "-zsh")
	proc.stdin(200, "/dev/pts/9")
	if pid, ok := id.CLI(200, "/dev/pts/9"); ok {
		t.Fatalf("a shell-only pane claimed %d", pid)
	}
}

// TestAHermesWindowIsListedWithNoRenderedViewAndUnknownStatus is the stub: a
// window whose pane runs a Hermes CLI goes on the wire as Runtime "hermes",
// NOT Claude (no rendered view), with no status and no session id; a Claude
// window beside it is the same bytes it always was (no runtime field); a
// shell window is neither. Claude Code claims a pane first.
func TestAHermesWindowIsListedWithNoRenderedViewAndUnknownStatus(t *testing.T) {
	proc := newFakeProc(t)
	py, launcher := "/usr/bin/python3", "/usr/local/bin/hermes"
	proc.add(10, 1, "zsh", "/w", "-zsh")
	proc.stdin(10, "/dev/pts/1")
	proc.add(11, 10, "hermes", "/w", py, launcher, "chat")
	proc.stdin(11, "/dev/pts/1")
	proc.add(20, 1, "zsh", "/w", "-zsh")
	proc.stdin(20, "/dev/pts/2")
	t3 := Tmux{Claude: ClaudeLocator{Proc: proc.root, Home: t.TempDir()}}
	windows := []WindowInfo{
		{Session: "work", ID: "@1", Index: 0, Active: true, Pane: "%1", PanePID: 10, Command: "python3.14"},
		{Session: "work", ID: "@2", Index: 1, Pane: "%2", PanePID: 20, Command: "zsh"},
		// Claude Code's pane, claimed by its runtime before Hermes is asked.
		{Session: "work", ID: "@3", Index: 2, Pane: "%3", PanePID: 30, Claude: &ClaudePane{Pane: "%3", Gen: "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b", Status: mirror.StatusIdle, StatusSince: 1792000001111}},
	}
	procs, children := t3.Claude.table()
	attachAgents(windows, procs, children, t3.runtimes()[1:])
	if a := windows[0].Agent; a == nil || a.Runtime != mirror.RuntimeHermes || a.Pane != "%1" || a.PID != 11 || a.Source != SourceProcess {
		t.Fatalf("the Hermes window's Agent = %+v", a)
	}
	if windows[1].Agent != nil || windows[2].Agent != nil {
		t.Fatalf("a shell or a Claude window got an Agent: %+v / %+v", windows[1].Agent, windows[2].Agent)
	}
	wire := WireWindows(windows, nil, true, time.Unix(1792000002, 0))
	if len(wire) != 3 {
		t.Fatalf("wire = %+v", wire)
	}
	h, sh, cl := wire[0], wire[1], wire[2]
	if h.Runtime != mirror.RuntimeHermes || h.Claude || h.Status != "" || h.StatusSince != 0 || h.AgentSession != "" || h.AgentRuntime() != mirror.RuntimeHermes {
		t.Fatalf("the Hermes window on the wire = %+v", h)
	}
	if sh.Runtime != "" || sh.AgentRuntime() != "" {
		t.Fatalf("the shell window on the wire = %+v", sh)
	}
	b, _ := json.Marshal(cl)
	if strings.Contains(string(b), "runtime") || strings.Contains(string(b), "agentSession") || !cl.Claude {
		t.Fatalf("the Claude window's bytes changed: %s", b)
	}
	// A v1 tunnel (claude=false) carries no claim of either.
	if w := WireWindows(windows, nil, false, time.Unix(1792000002, 0)); w[0].Runtime != "" || w[2].Claude {
		t.Fatalf("a v1 list claims a runtime: %+v", w)
	}
}

// TestTheLargestWindowListWithHermesWindowsFitsOneFrame is the Hermes (and
// opencode: its runtime name is two bytes longer) twin of
// TestTheLargestWindowListFitsOneFrame: a Hermes window carries Runtime and an
// AgentSession of up to 128 bytes in place of a Claude window's flag and
// 64-byte id, so a list of them is the new worst case. Every window that does
// not carry what it waits on (Pending is Claude Code's, MaxPendingWindows of
// them) is a Hermes window here, every field at its bound. An INVARIANT GUARD
// over the constants, with the same control: twice the count must not fit.
func TestTheLargestWindowListWithHermesWindowsFitsOneFrame(t *testing.T) {
	for _, rt := range []string{mirror.RuntimeHermes, mirror.RuntimeOpenCode} {
		t.Run(rt, func(t *testing.T) { largestAgentList(t, rt) })
	}
}

// largestAgentList is the frame-budget check with every non-pending window
// one of runtime rt's.
func largestAgentList(t *testing.T, rt string) {
	worst := func(n int) []WindowInfo {
		out := make([]WindowInfo, 0, n)
		for i := 0; i < n; i++ {
			name := strings.Repeat("<", 128-len(itoa(i))) + itoa(i)
			w := WindowInfo{Session: name, ID: "@" + strings.Repeat("9", 10), Index: 1 << 20, Active: true,
				Pane: "%1", PanePID: 1, Activity: 1791903960, Command: strings.Repeat(">", mirror.MaxCommand),
				Others: otherClaudeAtBound(i)}
			if i < mirror.MaxPendingWindows {
				w.Claude = &ClaudePane{Status: mirror.StatusWaiting, StatusSince: math.MaxInt64, Gen: strings.Repeat("f", 64),
					Pending: &mirror.Pending{Summary: strings.Repeat("<", mirror.MaxPendingSummary), Tool: strings.Repeat("&", mirror.MaxPendingTool),
						Last: strings.Repeat(">", mirror.MaxPendingLast)}}
			} else {
				id := itoa(i) + "h"
				w.Agent = &AgentPane{Runtime: rt, Status: mirror.StatusWaiting, StatusSince: math.MaxInt64,
					Gen: id + strings.Repeat("a", 128-len(id)), Note: strings.Repeat("&", mirror.MaxAgentNote)}
			}
			out = append(out, w)
		}
		return out
	}
	codename := strings.Repeat("&", mirror.MaxLabel-len(":1048576"))
	encode := func(n int) int {
		raw := worst(n)
		codenames := map[string]string{}
		for _, w := range raw {
			codenames[w.Session] = codename
		}
		h := &hostSide{c: &Client{cfg: Config{Backend: &windowsBackend{windows: raw}, Codenames: codenames}, log: log.New(io.Discard, "", 0)},
			ctx: context.Background(), version: mirror.ProtocolVersion}
		l, ok := h.read()
		if !ok {
			t.Fatal("the read failed")
		}
		hermes, notes := 0, 0
		for _, w := range l.windows {
			if w.AgentNote != "" {
				notes++
			}
			if w.Runtime == rt {
				if len(w.AgentSession) != 128 || w.Status != mirror.StatusWaiting || w.StatusSince != math.MaxInt64 || w.Label == "" {
					t.Fatalf("instrument check: a worst-case %s field was dropped by normalisation: %+v", rt, w)
				}
				hermes++
			}
		}
		// At the bound every window but the pending ones is Hermes. Past it the
		// list is cut in session-name order, which keeps the longest indexes —
		// all Hermes windows — so it is a list of nothing else.
		want := mirror.MaxWindows - mirror.MaxPendingWindows
		if n > mirror.MaxWindows {
			want = mirror.MaxWindows
		}
		if notes != mirror.MaxAgentNoteWindows {
			t.Fatalf("instrument check: %d windows carry a note, want the bound %d", notes, mirror.MaxAgentNoteWindows)
		}
		if hermes != want || len(l.windows) != min(n, mirror.MaxWindows) {
			t.Fatalf("instrument check: %d %s windows of %d on the wire, want %d", hermes, rt, len(l.windows), want)
		}
		m := l.msg(mirror.TypeHello)
		m.Version, m.Host, m.Interactive, m.PanePaste, m.StatusReport, m.HermesSend = mirror.ProtocolVersion, strings.Repeat("h", 63), true, true, true, true
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return len(b)
	}
	got := encode(mirror.MaxWindows)
	if got > mirror.MaxFrame {
		t.Fatalf("the largest window list with %s windows encodes to %d bytes, over MaxFrame (%d)", rt, got, mirror.MaxFrame)
	}
	if over := encode(2 * mirror.MaxWindows); over > mirror.MaxFrame {
		t.Fatalf("a host with twice MaxWindows encodes to %d bytes, over MaxFrame (%d): the bound did not hold", over, mirror.MaxFrame)
	}
	if perWindow := got / mirror.MaxWindows; 2*mirror.MaxWindows*perWindow <= mirror.MaxFrame {
		t.Fatalf("instrument check: at %d bytes a window, even twice MaxWindows fits a frame — this test could not see an overrun", perWindow)
	}
	t.Logf("largest list with %s windows: %d windows in %d bytes (%.1f%% of MaxFrame)", rt, mirror.MaxWindows, got, 100*float64(got)/float64(mirror.MaxFrame))
}

// TestTheHermesLocatorWalksNothingWithoutAHermesProcess: the listing reads the
// proc table once for every runtime, and the Hermes locator walks no pane
// when that table holds no process named "hermes" — the strict rule needs
// that comm. Measured by a walk that would CLAIM: the same tree, with the
// one process renamed, is claimed when it is "hermes" and not otherwise; and
// a Hermes pane is still found through the shared table (the positive
// control, by the same path Tmux.Windows takes).
func TestTheHermesLocatorWalksNothingWithoutAHermesProcess(t *testing.T) {
	build := func(comm string) (Tmux, []pane) {
		proc := newFakeProc(t)
		proc.add(10, 1, "zsh", "/w", "-zsh")
		proc.stdin(10, "/dev/pts/1")
		proc.add(11, 10, comm, "/w", "/usr/bin/python3", "/usr/local/bin/hermes", "chat")
		proc.stdin(11, "/dev/pts/1")
		return Tmux{Claude: ClaudeLocator{Proc: proc.root, Home: t.TempDir()}}, []pane{{Session: "s", ID: "%1", PID: 10}}
	}
	tm, panes := build("hermes")
	procs, children := tm.Claude.table()
	if !anyComm(procs, "hermes") {
		t.Fatal("instrument check: the fixture's hermes process is not in the table")
	}
	if got := tm.hermesRuntime().LocatePanes(panes, procs, children); got["%1"].PID != 11 {
		t.Fatalf("a Hermes pane through the shared table: %+v, want pid 11 claimed", got)
	}
	tm, panes = build("python3.14")
	procs, children = tm.Claude.table()
	if anyComm(procs, "hermes") {
		t.Fatal("instrument check: a process named hermes in the renamed fixture")
	}
	if got := tm.hermesRuntime().LocatePanes(panes, procs, children); len(got) != 0 {
		t.Fatalf("no process named hermes, yet claimed %+v", got)
	}
}
