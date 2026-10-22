package mirrorhost

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/askdrive"
	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/mirror"
)

// Every fixture here is synthetic: invented paths, pids and profile names.

// kernelChild lists child under parent's main thread in the fixture's
// /proc/<parent>/task/<parent>/children, as the kernel does.
func (f *fakeProc) kernelChild(parent, child int) {
	f.t.Helper()
	dir := filepath.Join(f.root, fmt.Sprint(parent), "task", fmt.Sprint(parent))
	must(f.t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "children")
	b, _ := os.ReadFile(path)
	must(f.t, os.WriteFile(path, append(b, []byte(fmt.Sprintf("%d ", child))...), 0o644))
}

// unreadable is a process whose /proc directory exists but whose stat cannot
// be read (here: is missing), listed as parent's child by the kernel only.
func (f *fakeProc) unreadable(pid, parent int) {
	f.t.Helper()
	must(f.t, os.MkdirAll(filepath.Join(f.root, fmt.Sprint(pid)), 0o755))
	f.kernelChild(parent, pid)
}

// stat rewrites pid's stat with another state ("Z" for a zombie).
func (f *fakeProc) state(pid, ppid int, comm, state string) {
	f.t.Helper()
	stat := fmt.Sprintf("%d (%s) %s %d 1 1 0 -1 4194560 0 0 0 0 0 0 0 0 20 0 1 0 100 0 0", pid, comm, state, ppid)
	must(f.t, os.WriteFile(filepath.Join(f.root, fmt.Sprint(pid), "stat"), []byte(stat), 0o644))
}

// hermesCase is one pane: a root shell at root, and below it the processes
// `procs` adds. want is the pid the guard must name (0: none), wantRule its
// rule; wantErr: the guard cannot check, and so refuses.
type hermesCase struct {
	name     string
	procs    func(f *fakeProc, root int)
	want     int
	wantRule string
	wantErr  string
}

// TestHermesIdentityFindsEveryHermesShapeAndNothingElse is the guard's broad
// match (docs/design/hermes.md, the paste guard; S10), over a fixture /proc.
// Each pane is a shell with the process under test as its CHILD (or deeper),
// so the walk below #{pane_pid} is exercised, not just the root. The pid each
// case must name is distinct from the root's and from every other case's, so
// a walk that answered "the root" or "some process" cannot pass.
func TestHermesIdentityFindsEveryHermesShapeAndNothingElse(t *testing.T) {
	install := t.TempDir() // a synthetic Hermes source checkout: cli.py beside hermes_cli/
	must(t, os.Mkdir(filepath.Join(install, "hermes_cli"), 0o755))
	must(t, os.WriteFile(filepath.Join(install, "cli.py"), nil, 0o644))
	notInstall := t.TempDir() // some other project's cli.py
	must(t, os.WriteFile(filepath.Join(notInstall, "cli.py"), nil, 0o644))
	// A cli.py whose hermes_cli cannot be stat'ed for a reason other than "not
	// there" (a symlink loop: ELOOP, whoever runs the test, root included).
	looped := t.TempDir()
	must(t, os.Symlink("hermes_cli", filepath.Join(looped, "hermes_cli")))

	const py = "/nix/store/0000000000000000000000000000000a-python3-3.14.0/bin/python3.14"
	const nixHermes = "/nix/store/0000000000000000000000000000000b-hermes-agent/bin/hermes"
	cases := []hermesCase{
		{name: "Nix-wrapped hermes chat", want: 1011, wantRule: hermesRuleLauncher, procs: func(f *fakeProc, root int) {
			f.add(1011, root, "hermes", "/work/a", py, nixHermes, "chat")
		}},
		{name: "a profile-launched CLI: hermes -p coder chat", want: 1021, wantRule: hermesRuleLauncher, procs: func(f *fakeProc, root int) {
			f.add(1021, root, "hermes", "/work/b", "/srv/venv/bin/python3", "/srv/venv/bin/hermes", "-p", "coder", "chat")
		}},
		{name: "the bare launcher, no subcommand", want: 1031, wantRule: hermesRuleLauncher, procs: func(f *fakeProc, root int) {
			f.add(1031, root, "hermes", "/work/c", py, nixHermes)
		}},
		{name: "python -m hermes_cli.main", want: 1041, wantRule: hermesRulePython, procs: func(f *fakeProc, root int) {
			f.add(1041, root, "python3", "/work/d", "python3", "-m", "hermes_cli.main", "chat")
		}},
		{name: "python -m hermes_cli, options first and the module glued on", want: 1051, wantRule: hermesRulePython, procs: func(f *fakeProc, root int) {
			f.add(1051, root, "python3.12", "/work/e", "/usr/bin/python3.12", "-u", "-Wonce", "-mhermes_cli")
		}},
		{name: "a script path inside hermes_cli/", want: 1061, wantRule: hermesRulePython, procs: func(f *fakeProc, root int) {
			f.add(1061, root, "python", "/work/f", "/opt/venv/bin/python", "-X", "dev", "/opt/hermes-agent/hermes_cli/main.py", "chat")
		}},
		{name: "the launcher run by an explicit interpreter (comm python)", want: 1071, wantRule: hermesRulePython, procs: func(f *fakeProc, root int) {
			f.add(1071, root, "python3", "/work/g", "python3", "/usr/local/bin/hermes", "chat")
		}},
		{name: "cli.py of a Hermes checkout, by a relative path", want: 1081, wantRule: hermesRuleCheckout, procs: func(f *fakeProc, root int) {
			f.add(1081, root, "python3", install, "python3", "cli.py")
		}},
		{name: "cli.py of a Hermes checkout, by an absolute path", want: 1091, wantRule: hermesRuleCheckout, procs: func(f *fakeProc, root int) {
			f.add(1091, root, "python3", "/", "python3", filepath.Join(install, "cli.py"))
		}},
		{name: "the gateway, as the Nix launcher runs it", want: 1101, wantRule: hermesRuleLauncher, procs: func(f *fakeProc, root int) {
			f.add(1101, root, "hermes", "/work/h", py, nixHermes, "gateway", "run", "--quiet", "--idle-exit")
		}},
		{name: "the gateway, as a module", want: 1111, wantRule: hermesRulePython, procs: func(f *fakeProc, root int) {
			f.add(1111, root, "python3.14", "/work/i", py, "-m", "hermes_cli.main", "--profile", "coder", "gateway", "run")
		}},
		{name: "python -c naming hermes_cli", want: 1121, wantRule: hermesRulePython, procs: func(f *fakeProc, root int) {
			f.add(1121, root, "python3", "/work/j", "python3", "-c", "from hermes_cli.main import main; main()")
		}},
		{name: "hermes started by hand, two levels down (nix develop, then bash)", want: 1133, wantRule: hermesRuleLauncher, procs: func(f *fakeProc, root int) {
			f.add(1131, root, "nix", "/work/k", "nix", "develop")
			f.add(1132, 1131, "bash", "/work/k", "bash")
			f.add(1133, 1132, "hermes", "/work/k", py, nixHermes, "chat")
		}},
		{name: "a Claude Code pane with Hermes in the foreground (claude &, then hermes)", want: 1143, wantRule: hermesRuleLauncher, procs: func(f *fakeProc, root int) {
			f.add(1141, root, "claude", "/work/l", "claude")
			f.add(1143, root, "hermes", "/work/l", py, nixHermes, "chat")
		}},
		{name: "a cli.py whose hermes_cli is a symlink loop (fail closed)", want: 1151, wantRule: hermesRuleCheckout, procs: func(f *fakeProc, root int) {
			f.add(1151, root, "python3", looped, "python3", filepath.Join(looped, "cli.py"))
		}},
		{name: "a relative cli.py whose cwd cannot be read (fail closed)", want: 1161, wantRule: hermesRuleCheckout, procs: func(f *fakeProc, root int) {
			f.add(1161, root, "python3", "" /* no cwd link */, "python3", "cli.py")
		}},

		// 🔴 A TERMINAL MULTIPLEXER CLIENT: it can show a Hermes this walk
		// cannot see (`TMUX= tmux -L other attach`). Refused, accepted as safe.
		{name: "a nested tmux client (comm as tmux sets it)", want: 1171, wantRule: hermesRuleMultiplexer, procs: func(f *fakeProc, root int) {
			f.add(1171, root, "tmux: client", "/work/m", "tmux", "-L", "other", "attach")
		}},
		{name: "GNU screen", want: 1181, wantRule: hermesRuleMultiplexer, procs: func(f *fakeProc, root int) {
			f.add(1181, root, "screen", "/work/n", "screen", "-r")
		}},
		{name: "dtach", want: 1191, wantRule: hermesRuleMultiplexer, procs: func(f *fakeProc, root int) {
			f.add(1191, root, "dtach", "/work/o", "/usr/bin/dtach", "-a", "/tmp/s")
		}},
		{name: "abduco", want: 1201, wantRule: hermesRuleMultiplexer, procs: func(f *fakeProc, root int) {
			f.add(1201, root, "abduco", "/work/p", "abduco", "-a", "s")
		}},
		{name: "zellij, below a shell", want: 1212, wantRule: hermesRuleMultiplexer, procs: func(f *fakeProc, root int) {
			f.add(1211, root, "bash", "/work/q", "bash")
			f.add(1212, 1211, "zellij", "/work/q", "/opt/zellij/bin/zellij", "attach")
		}},

		// 🔴 COULD NOT CHECK: refused, never presumed safe.
		{name: "a child whose stat cannot be read", wantErr: "could not be read", procs: func(f *fakeProc, root int) {
			f.unreadable(1221, root)
		}},
		{name: "a child whose cmdline cannot be read", wantErr: "command line", procs: func(f *fakeProc, root int) {
			f.add(1231, root, "sleep", "/work/r", "sleep", "60")
			must(t, os.Remove(filepath.Join(f.root, "1231", "cmdline")))
		}},

		// 🔴 MUST NOT BE REFUSED.
		{name: "a plain shell", want: 0, procs: func(f *fakeProc, root int) {}},
		{name: "a shell running vim", want: 0, procs: func(f *fakeProc, root int) {
			f.add(1301, root, "vim", "/work/s", "vim", "notes.md")
		}},
		{name: "a Claude Code pane", want: 0, procs: func(f *fakeProc, root int) {
			f.add(1311, root, "claude", "/work/t", "claude", "--continue")
			f.add(1312, 1311, "node", "/work/t", "node", "/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js")
		}},
		{name: "a NixOS-wrapped Claude Code", want: 0, procs: func(f *fakeProc, root int) {
			f.add(1321, root, ".claude-wrapped", "/work/u", "/nix/store/0000000000000000000000000000000c-claude-code/bin/.claude-wrapped")
		}},
		{name: "other Python programs", want: 0, procs: func(f *fakeProc, root int) {
			f.add(1331, root, "python3", "/work/v", "python3", "-m", "http.server", "8000")
			f.add(1332, root, "python3", "/work/v", "python3", "manage.py", "migrate")
			f.add(1333, root, "python3", notInstall, "python3", "cli.py")
			f.add(1334, root, "python3", "/work/v", "python3", "-c", "print('hi')")
			f.add(1335, root, "python3", "/work/v", "python3", "-W", "ignore", "tool.py")
		}},
		{name: "another program that happens to be named hermes (the doc's rule needs argv[1] too)", want: 0, procs: func(f *fakeProc, root int) {
			f.add(1341, root, "hermes", "/work/w", "/opt/hermes-engine/bin/hermes", "repl.js")
		}},
		{name: "a zombie child (no command line, reads no input)", want: 0, procs: func(f *fakeProc, root int) {
			f.add(1351, root, "sleep", "/work/x")
			f.state(1351, root, "sleep", "Z")
		}},
		{name: "a child the kernel listed that has exited since", want: 0, procs: func(f *fakeProc, root int) {
			f.kernelChild(root, 1361)
		}},
	}

	proc := newFakeProc(t)
	for i, c := range cases {
		root := 100 + i // each pane's shell
		proc.add(root, 1, "zsh", "/work", "-zsh")
		c.procs(proc, root)
	}
	id := HermesIdentity{Proc: proc.root}
	for i, c := range cases {
		root := 100 + i
		got, found, err := id.InTree(root)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%s: InTree = %+v, found %v, err %v; want an error naming %q (fail closed)", c.name, got, found, err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: InTree: %v", c.name, err)
		}
		if c.want == 0 {
			if found {
				t.Errorf("%s: REFUSED (%+v); it is neither Hermes nor a multiplexer", c.name, got)
			}
			continue
		}
		if !found || got.PID != c.want || got.Rule != c.wantRule {
			t.Errorf("%s: InTree = %+v, found %v; want pid %d by %q", c.name, got, found, c.want, c.wantRule)
		}
	}
}

// TestHermesIdentityFailsClosed: a pane whose processes cannot be read is not
// presumed Hermes-free. The root missing from /proc, an unreadable proc root,
// and a tree larger than the walk's bound are errors, which the guard turns
// into a refusal.
func TestHermesIdentityFailsClosed(t *testing.T) {
	proc := newFakeProc(t)
	proc.add(10, 1, "zsh", "/work", "-zsh")
	for pid := 11; pid <= 15; pid++ {
		proc.add(pid, 10, "sleep", "/work", "sleep", "60")
	}
	id := HermesIdentity{Proc: proc.root}
	if _, _, err := id.InTree(10); err != nil {
		t.Fatalf("instrument check: a six-process tree under the default bound answered %v", err)
	}
	if _, found, err := id.InTree(99); err == nil || found {
		t.Fatalf("a root not in /proc: found %v, err %v; want an error", found, err)
	}
	if _, _, err := (HermesIdentity{Proc: filepath.Join(proc.root, "absent")}).InTree(10); err == nil {
		t.Fatal("an unreadable proc root answered no error")
	}
	// The bound: 6 processes, a bound of 4 (deliberately not a divisor of
	// anything here). Every one of them is harmless, so only the bound can
	// make it refuse.
	defer func(b int) { procWalkBound = b }(procWalkBound)
	procWalkBound = 4
	if _, found, err := id.InTree(10); err == nil || found || !strings.Contains(err.Error(), "larger than 4") {
		t.Fatalf("a tree past the walk's bound: found %v, err %v; want the bound's error", found, err)
	}
	procWalkBound = 6
	if _, _, err := id.InTree(10); err != nil {
		t.Fatalf("control: the same tree at a bound it fits answered %v", err)
	}
}

// guardedBackend is windowsBackend whose panes have processes: each pane id is
// a root pid in a fixture /proc, and PaneHermes is the real identity over it.
// It also types answers into pickers, recording them.
type guardedBackend struct {
	*windowsBackend
	proc  string
	roots map[string]int
	mu    *sync.Mutex
	typed *[]string
}

func (g guardedBackend) PaneHermes(_ context.Context, pane string) (HermesProc, bool, error) {
	return HermesIdentity{Proc: g.proc}.InTree(g.roots[pane])
}

func (g guardedBackend) AnswerAsk(_ context.Context, pane string, _ []askq.Question, _ []askdrive.Choice, _ *askq.Progress) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	*g.typed = append(*g.typed, pane)
	return 0, nil
}

// TestEveryHostPasteIntoAHermesPaneIsRefusedAndTypesNothing is the host paste
// guard, end to end through the host's three entry points that type into a
// pane — the Terminal view's paste (AnyPane), the rendered view's paste into
// Claude Code, and an answer typed into a picker — each refused with the
// guard's message and kind, and NOTHING typed: into a Hermes pane by window
// and by session (the active-pane path, a gateway), into a nested multiplexer,
// into a pane that cannot be read, and into a Claude Code pane with Hermes in
// the foreground (`claude &`, then `hermes chat`). The same host types into a
// shell window and a Claude Code window as before by every path (the
// controls, which also prove the instrument sees what is typed).
func TestEveryHostPasteIntoAHermesPaneIsRefusedAndTypesNothing(t *testing.T) {
	proc := newFakeProc(t)
	proc.add(1400, 1, "zsh", "/work", "-zsh") // %14: a plain shell
	proc.add(2100, 1, "zsh", "/work", "-zsh") // %21: Claude Code
	proc.add(2110, 2100, "claude", "/work", "claude")
	proc.add(2200, 1, "zsh", "/work", "-zsh") // %22: Claude Code backgrounded, hermes in the foreground
	proc.add(2210, 2200, "claude", "/work", "claude")
	proc.add(2220, 2200, "hermes", "/work", "python3.14", "/x/bin/hermes", "chat")
	proc.add(3000, 1, "zsh", "/work", "-zsh") // %30: hermes chat, by hand in a shell
	proc.add(3010, 3000, "hermes", "/work", "python3.14", "/x/bin/hermes", "chat")
	proc.add(3100, 1, "zsh", "/work", "-zsh") // %31: a client of ANOTHER tmux server
	proc.add(3110, 3100, "tmux: client", "/work", "tmux", "-L", "other", "attach")
	proc.add(5000, 1, "zsh", "/work", "-zsh") // %legacy: the session's active pane, a gateway
	proc.add(5010, 5000, "python3", "/work", "python3", "-m", "hermes_cli.main", "gateway", "run")
	windows := []WindowInfo{
		{Session: "scratch2", ID: "@4", Index: 1, Pane: "%14", PanePID: 1400, Command: "zsh"},
		{Session: "scratch2", ID: "@7", Index: 8, Active: true, Pane: "%21", PanePID: 2100, Command: "claude", Claude: &ClaudePane{Pane: "%21", Gen: "g7"}},
		{Session: "scratch2", ID: "@8", Index: 9, Pane: "%22", PanePID: 2200, Command: "python3.14", Claude: &ClaudePane{Pane: "%22", Gen: "g8"}},
		{Session: "solo", ID: "@9", Index: 0, Active: true, Pane: "%30", PanePID: 3000, Command: "python3.14"},
		{Session: "solo", ID: "@10", Index: 1, Pane: "%31", PanePID: 3100, Command: "tmux"},
		{Session: "gone", ID: "@11", Index: 0, Active: true, Pane: "%40", PanePID: 4000, Command: "zsh"},
	}
	b := &windowsBackend{windows: windows}
	var typed []string
	g := guardedBackend{windowsBackend: b, proc: proc.root, mu: &sync.Mutex{}, typed: &typed,
		roots: map[string]int{"%14": 1400, "%21": 2100, "%22": 2200, "%30": 3000, "%31": 3100, "%legacy": 5000, "%40": 4000 /* not in /proc */}}
	d := newDrivenMuster(t)
	d.run(t, wrapped{g}, nil, true) // through a wrapper, as NoShrink wraps the real backend
	pasted := func() []string { _, _, _, p, _ := b.snapshot(); return p }
	answers := func() []string { g.mu.Lock(); defer g.mu.Unlock(); return slices.Clone(typed) }
	reply := func(typ string, id uint32) mirror.Msg {
		var out mirror.Msg
		waitUntil(t, "the host's answer", func() bool {
			for _, m := range d.of(typ) {
				if m.ID == id {
					out = m
					return true
				}
			}
			return false
		})
		return out
	}
	ask := &mirror.AskTyping{Questions: hostAskQs, Choices: []askq.Choice{{Picks: []int{1}}}}

	// Controls first: a shell, and a Claude Code window by all three paths.
	for i, m := range []mirror.Msg{
		{Type: mirror.TypePaste, Session: "scratch2", Window: "@4", Text: "ls -la", AnyPane: true},
		{Type: mirror.TypePaste, Session: "scratch2", Window: "@7", Text: "to claude, terminal", AnyPane: true},
		{Type: mirror.TypePaste, Session: "scratch2", Window: "@7", Text: "to claude, rendered"},
		{Type: mirror.TypeAnswer, Session: "scratch2", Window: "@7", Ask: ask},
	} {
		m.ID = uint32(1 + i)
		d.send(t, m)
		typ := mirror.TypePasted
		if m.Type == mirror.TypeAnswer {
			typ = mirror.TypeAnswered
		}
		if a := reply(typ, m.ID); a.Error != "" || a.Refusal != "" {
			t.Fatalf("control %d (%s into %s): refused %q (%q)", m.ID, m.Type, m.Window, a.Error, a.Refusal)
		}
	}
	wantPasted := []string{"%14 ls -la", "%21 to claude, terminal", "%21 to claude, rendered"}
	if got := pasted(); !slices.Equal(got, wantPasted) {
		t.Fatalf("instrument check: pasted %q, want %q", got, wantPasted)
	}
	if got := answers(); !slices.Equal(got, []string{"%21"}) {
		t.Fatalf("instrument check: answers typed into %q, want [%%21]", got)
	}

	for i, c := range []struct {
		m       mirror.Msg
		why     error
		refusal string
	}{
		{mirror.Msg{Type: mirror.TypePaste, Session: "solo", Window: "@9", Text: "4", AnyPane: true}, ErrHermesPane, mirror.PasteRefusalHermes},
		{mirror.Msg{Type: mirror.TypePaste, Session: "solo", Text: "approve nothing", AnyPane: true}, ErrHermesPane, mirror.PasteRefusalHermes},
		{mirror.Msg{Type: mirror.TypePaste, Session: "solo", Window: "@10", Text: "nested", AnyPane: true}, ErrMultiplexerPane, mirror.PasteRefusalMultiplexer},
		{mirror.Msg{Type: mirror.TypePaste, Session: "gone", Window: "@11", Text: "unchecked", AnyPane: true}, ErrPaneUnchecked, mirror.PasteRefusalUnchecked},
		// 🔴 Claude Code's own paths, into its pane with Hermes in the foreground.
		{mirror.Msg{Type: mirror.TypePaste, Session: "scratch2", Window: "@8", Text: "for claude, rendered"}, ErrHermesPane, mirror.PasteRefusalHermes},
		{mirror.Msg{Type: mirror.TypePaste, Session: "scratch2", Window: "@8", Text: "for claude, terminal", AnyPane: true}, ErrHermesPane, mirror.PasteRefusalHermes},
		{mirror.Msg{Type: mirror.TypeAnswer, Session: "scratch2", Window: "@8", Ask: ask}, ErrHermesPane, ""},
	} {
		c.m.ID = uint32(10 + i)
		d.send(t, c.m)
		typ := mirror.TypePasted
		if c.m.Type == mirror.TypeAnswer {
			typ = mirror.TypeAnswered
		}
		a := reply(typ, c.m.ID)
		// The refusal is the guard's own words EXACTLY: nothing internal (a
		// pid, /proc, tmux's stderr) appended.
		if a.Error != c.why.Error() || (typ == mirror.TypePasted && a.Refusal != c.refusal) {
			t.Fatalf("%s into %s/%s answered %q (kind %q); want %q (kind %q)", c.m.Type, c.m.Session, c.m.Window, a.Error, a.Refusal, c.why.Error(), c.refusal)
		}
	}
	if got := pasted(); !slices.Equal(got, wantPasted) {
		t.Fatalf("a refused paste was typed: pasted %q, want only %q", got, wantPasted)
	}
	if got := answers(); !slices.Equal(got, []string{"%21"}) {
		t.Fatalf("a refused answer was typed: into %q, want only [%%21]", got)
	}
}
