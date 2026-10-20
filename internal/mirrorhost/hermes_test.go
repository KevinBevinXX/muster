package mirrorhost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// Every fixture here is synthetic: invented paths, pids and profile names.

// hermesCase is one pane: a root shell at root, and below it the processes
// `procs` adds. want is the pid the guard must name (0: none), wantRule its rule.
type hermesCase struct {
	name     string
	procs    func(f *fakeProc, root int)
	want     int
	wantRule string
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
		{name: "python -m hermes_cli.main", want: 1041, wantRule: hermesRuleModule, procs: func(f *fakeProc, root int) {
			f.add(1041, root, "python3", "/work/d", "python3", "-m", "hermes_cli.main", "chat")
		}},
		{name: "python -m hermes_cli, options first and the module glued on", want: 1051, wantRule: hermesRuleModule, procs: func(f *fakeProc, root int) {
			f.add(1051, root, "python3.12", "/work/e", "/usr/bin/python3.12", "-u", "-Wonce", "-mhermes_cli")
		}},
		{name: "a script path inside hermes_cli/", want: 1061, wantRule: hermesRuleScript, procs: func(f *fakeProc, root int) {
			f.add(1061, root, "python", "/work/f", "/opt/venv/bin/python", "-X", "dev", "/opt/hermes-agent/hermes_cli/main.py", "chat")
		}},
		{name: "the launcher run by an explicit interpreter (comm python)", want: 1071, wantRule: hermesRuleScript, procs: func(f *fakeProc, root int) {
			f.add(1071, root, "python3", "/work/g", "python3", "/usr/local/bin/hermes", "chat")
		}},
		{name: "cli.py of a Hermes checkout, by a relative path", want: 1081, wantRule: hermesRuleScript, procs: func(f *fakeProc, root int) {
			f.add(1081, root, "python3", install, "python3", "cli.py")
		}},
		{name: "cli.py of a Hermes checkout, by an absolute path", want: 1091, wantRule: hermesRuleScript, procs: func(f *fakeProc, root int) {
			f.add(1091, root, "python3", "/", "python3", filepath.Join(install, "cli.py"))
		}},
		{name: "the gateway, as the Nix launcher runs it", want: 1101, wantRule: hermesRuleLauncher, procs: func(f *fakeProc, root int) {
			f.add(1101, root, "hermes", "/work/h", py, nixHermes, "gateway", "run", "--quiet", "--idle-exit")
		}},
		{name: "the gateway, as a module", want: 1111, wantRule: hermesRuleModule, procs: func(f *fakeProc, root int) {
			f.add(1111, root, "python3.14", "/work/i", py, "-m", "hermes_cli.main", "--profile", "coder", "gateway", "run")
		}},
		{name: "python -c naming hermes_cli", want: 1121, wantRule: hermesRuleInline, procs: func(f *fakeProc, root int) {
			f.add(1121, root, "python3", "/work/j", "python3", "-c", "from hermes_cli.main import main; main()")
		}},
		{name: "hermes started by hand, two levels down (nix develop, then bash)", want: 1133, wantRule: hermesRuleLauncher, procs: func(f *fakeProc, root int) {
			f.add(1131, root, "nix", "/work/k", "nix", "develop")
			f.add(1132, 1131, "bash", "/work/k", "bash")
			f.add(1133, 1132, "hermes", "/work/k", py, nixHermes, "chat")
		}},

		// 🔴 MUST NOT BE REFUSED.
		{name: "a plain shell", want: 0, procs: func(f *fakeProc, root int) {}},
		{name: "a shell running vim", want: 0, procs: func(f *fakeProc, root int) {
			f.add(1201, root, "vim", "/work/l", "vim", "notes.md")
		}},
		{name: "a Claude Code pane", want: 0, procs: func(f *fakeProc, root int) {
			f.add(1211, root, "claude", "/work/m", "claude", "--continue")
			f.add(1212, 1211, "node", "/work/m", "node", "/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js")
		}},
		{name: "a NixOS-wrapped Claude Code", want: 0, procs: func(f *fakeProc, root int) {
			f.add(1221, root, ".claude-wrapped", "/work/n", "/nix/store/0000000000000000000000000000000c-claude-code/bin/.claude-wrapped")
		}},
		{name: "other Python programs", want: 0, procs: func(f *fakeProc, root int) {
			f.add(1231, root, "python3", "/work/o", "python3", "-m", "http.server", "8000")
			f.add(1232, root, "python3", "/work/o", "python3", "manage.py", "hermes_cli")
			f.add(1233, root, "python3", notInstall, "python3", "cli.py")
			f.add(1234, root, "python3", "/work/o", "python3", "-c", "print('hi')")
			f.add(1235, root, "python3", "/work/o", "python3", "-W", "ignore", "tool.py")
		}},
		{name: "another program that happens to be named hermes (the doc's rule needs argv[1] too)", want: 0, procs: func(f *fakeProc, root int) {
			f.add(1241, root, "hermes", "/work/p", "/opt/hermes-engine/bin/hermes", "repl.js")
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
		if err != nil {
			t.Fatalf("%s: InTree: %v", c.name, err)
		}
		if c.want == 0 {
			if found {
				t.Errorf("%s: REFUSED as Hermes (%+v); it is not Hermes", c.name, got)
			}
			continue
		}
		if !found || got.PID != c.want || got.Rule != c.wantRule {
			t.Errorf("%s: InTree = %+v, found %v; want pid %d by %q", c.name, got, found, c.want, c.wantRule)
		}
	}
}

// TestHermesIdentityFailsClosed: a pane whose processes cannot be read is not
// presumed Hermes-free. The root missing from /proc, and an unreadable proc
// root, are errors, which the guard turns into a refusal.
func TestHermesIdentityFailsClosed(t *testing.T) {
	proc := newFakeProc(t)
	proc.add(10, 1, "zsh", "/work", "-zsh")
	if _, _, err := (HermesIdentity{Proc: proc.root}).InTree(10); err != nil {
		t.Fatalf("instrument check: a present root answered %v", err)
	}
	if _, found, err := (HermesIdentity{Proc: proc.root}).InTree(99); err == nil || found {
		t.Fatalf("a root not in /proc: found %v, err %v; want an error", found, err)
	}
	if _, _, err := (HermesIdentity{Proc: filepath.Join(proc.root, "absent")}).InTree(10); err == nil {
		t.Fatal("an unreadable proc root answered no error")
	}
}

// TestPythonTarget pins how a Python command line is read: what the broad
// match keys on. `-Wonce` holds a 'c' that is the warning filter's value, not
// a -c.
func TestPythonTarget(t *testing.T) {
	for _, c := range []struct {
		args                 []string
		module, script, code string
	}{
		{[]string{"-m", "hermes_cli.main", "chat"}, "hermes_cli.main", "", ""},
		{[]string{"-um", "hermes_cli"}, "hermes_cli", "", ""},
		{[]string{"-mhermes_cli.main"}, "hermes_cli.main", "", ""},
		{[]string{"-Wonce", "-m", "x"}, "x", "", ""},
		{[]string{"-W", "ignore", "s.py"}, "", "s.py", ""},
		{[]string{"-X", "dev", "-B", "s.py", "-m", "y"}, "", "s.py", ""},
		{[]string{"--", "-weird.py"}, "", "-weird.py", ""},
		{[]string{"-c", "code", "-m", "z"}, "", "", "code"},
		{[]string{"-"}, "", "", ""},
		{[]string{"-m"}, "", "", ""},
		{nil, "", "", ""},
	} {
		m, s, k := pythonTarget(c.args)
		if m != c.module || s != c.script || k != c.code {
			t.Errorf("pythonTarget(%q) = (%q, %q, %q), want (%q, %q, %q)", c.args, m, s, k, c.module, c.script, c.code)
		}
	}
}

// guardedBackend is windowsBackend whose panes have processes: each pane id is
// a root pid in a fixture /proc, and PaneHermes is the real identity over it.
type guardedBackend struct {
	*windowsBackend
	proc  string
	roots map[string]int
}

func (g guardedBackend) PaneHermes(_ context.Context, pane string) (HermesProc, bool, error) {
	return HermesIdentity{Proc: g.proc}.InTree(g.roots[pane])
}

// TestATerminalPasteIntoAHermesPaneIsRefusedAndTypesNothing is the host paste
// guard, end to end through the host's paste entry: a Terminal-view paste
// (AnyPane) into a window whose pane runs Hermes is refused with the guard's
// message and NOTHING is pasted — by window and by session (the active-pane
// path), and also when Hermes is the gateway. The same host pastes into a shell
// window and into a Claude Code window as before (the controls, which also
// prove the instrument sees a paste), and a pane whose processes cannot be read
// is refused (fail closed).
func TestATerminalPasteIntoAHermesPaneIsRefusedAndTypesNothing(t *testing.T) {
	proc := newFakeProc(t)
	proc.add(1400, 1, "zsh", "/work", "-zsh") // %14: a plain shell
	proc.add(2100, 1, "zsh", "/work", "-zsh") // %21: Claude Code
	proc.add(2110, 2100, "claude", "/work", "claude")
	proc.add(3000, 1, "zsh", "/work", "-zsh") // %30: hermes chat, by hand in a shell
	proc.add(3010, 3000, "hermes", "/work", "python3.14", "/x/bin/hermes", "chat")
	proc.add(5000, 1, "zsh", "/work", "-zsh") // %legacy: the session's active pane, a gateway
	proc.add(5010, 5000, "python3", "/work", "python3", "-m", "hermes_cli.main", "gateway", "run")
	windows := []WindowInfo{
		{Session: "scratch2", ID: "@4", Index: 1, Pane: "%14", PanePID: 1400, Command: "zsh"},
		{Session: "scratch2", ID: "@7", Index: 8, Active: true, Pane: "%21", PanePID: 2100, Command: "claude", Claude: &ClaudePane{Pane: "%21", Gen: "g7"}},
		{Session: "solo", ID: "@9", Index: 0, Active: true, Pane: "%30", PanePID: 3000, Command: "python3.14"},
		{Session: "gone", ID: "@11", Index: 0, Active: true, Pane: "%40", PanePID: 4000, Command: "zsh"},
	}
	b := &windowsBackend{windows: windows}
	g := guardedBackend{windowsBackend: b, proc: proc.root,
		roots: map[string]int{"%14": 1400, "%21": 2100, "%30": 3000, "%legacy": 5000, "%40": 4000 /* not in /proc */}}
	d := newDrivenMuster(t)
	d.run(t, wrapped{g}, nil, true) // through a wrapper, as NoShrink wraps the real backend
	pasted := func() []string { _, _, _, p, _ := b.snapshot(); return p }
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

	// Controls first: a shell, and a Claude Code window by both send paths.
	d.send(t, mirror.Msg{Type: mirror.TypePaste, ID: 1, Session: "scratch2", Window: "@4", Text: "ls -la", AnyPane: true})
	if a := answer(1); a.Error != "" {
		t.Fatalf("a Terminal paste into a SHELL was refused: %s", a.Error)
	}
	d.send(t, mirror.Msg{Type: mirror.TypePaste, ID: 2, Session: "scratch2", Window: "@7", Text: "to claude, terminal", AnyPane: true})
	if a := answer(2); a.Error != "" {
		t.Fatalf("a Terminal paste into a CLAUDE CODE window was refused: %s", a.Error)
	}
	d.send(t, mirror.Msg{Type: mirror.TypePaste, ID: 3, Session: "scratch2", Window: "@7", Text: "to claude, rendered"})
	if a := answer(3); a.Error != "" {
		t.Fatalf("a rendered-view paste into a CLAUDE CODE window was refused: %s", a.Error)
	}
	want := []string{"%14 ls -la", "%21 to claude, terminal", "%21 to claude, rendered"}
	if got := pasted(); !slices.Equal(got, want) {
		t.Fatalf("instrument check: pasted %q, want %q", got, want)
	}

	// 🔴 Hermes: by window, and by session (its active pane runs the gateway).
	for i, m := range []mirror.Msg{
		{Session: "solo", Window: "@9", Text: "4"},
		{Session: "solo", Text: "approve nothing"},
	} {
		m.Type, m.ID, m.AnyPane = mirror.TypePaste, uint32(10+i), true
		d.send(t, m)
		a := answer(m.ID)
		if a.Error != ErrHermesPane.Error() {
			t.Fatalf("a Terminal paste into a Hermes pane (session %q, window %q) answered %q; want the guard's refusal %q",
				m.Session, m.Window, a.Error, ErrHermesPane.Error())
		}
	}
	// The pane's processes cannot be read: refused, not presumed safe.
	d.send(t, mirror.Msg{Type: mirror.TypePaste, ID: 20, Session: "gone", Window: "@11", Text: "unchecked", AnyPane: true})
	if a := answer(20); !strings.HasPrefix(a.Error, errHermesUnchecked.Error()) {
		t.Fatalf("a paste into a pane whose processes cannot be read answered %q; want %q…", a.Error, errHermesUnchecked.Error())
	}
	if got := pasted(); !slices.Equal(got, want) {
		t.Fatalf("a refused paste was typed: pasted %q, want only %q", got, want)
	}
	if !errors.Is(&hermesRefusal{}, ErrHermesPane) {
		t.Fatal("hermesRefusal does not unwrap to ErrHermesPane")
	}
}
