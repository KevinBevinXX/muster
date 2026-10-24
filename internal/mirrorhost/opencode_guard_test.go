package mirrorhost

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/mirror"
)

// Every fixture here is synthetic: invented store paths, pids and directories.

// openCodeTUIScreen is the opencode TUI idle at its composer, as a pane
// shows it (synthetic text): a transcript line, the turn footer, the composer
// box drawn with ┃ and closed by ╹▀▀▀, its agent · model line, and the
// status line. It holds no Claude Code input box.
const openCodeTUIScreen = "  The guard now checks the terminal.\n\n" +
	"  build · model-x · 12s\n\n" +
	"┃\n┃\n┃  build · model-x\n" +
	"╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀\n" +
	"  /work:main                                  12% · $0.04\n"

// ptsOf is pseudo-terminal n's path, and ttyNrOf its device number as
// /proc/<pid>/stat field 7 spells it (major 136, minor n).
func ptsOf(n int) string { return fmt.Sprintf("/dev/pts/%d", n) }
func ttyNrOf(n int) int  { return 136<<8 | n }

// term adds a process with its terminal state: stdin ("" for none, an
// unreadable fd 0) and controlling terminal pts ctty (-1 for none: setsid).
func (f *fakeProc) term(pid, ppid int, comm string, ctty int, stdin string, argv ...string) {
	f.t.Helper()
	f.add(pid, ppid, comm, "/work", argv...)
	tty := 0
	if ctty >= 0 {
		tty = ttyNrOf(ctty)
	}
	stat := fmt.Sprintf("%d (%s) S %d %d %d %d -1 4194560 0 0 0 0 0 0 0 0 20 0 1 0 100 0 0", pid, comm, ppid, pid, pid, tty)
	must(f.t, os.WriteFile(filepath.Join(f.root, fmt.Sprint(pid), "stat"), []byte(stat), 0o644))
	if stdin != "" {
		f.stdin(pid, stdin)
	}
}

// TestAnOpenCodeOnlyRefusesAPaneWhoseTerminalItIsOn: the guard refuses an
// opencode the pane's typing could reach — its stdin is the pane's terminal,
// or its controlling terminal is — and admits one PROVEN off it (stdin
// /dev/null, no controlling terminal: what Claude Code's Bash tool starts
// for `opencode run`). Each refusing case isolates ONE reason, and every
// doubt refuses: an unreadable stdin, a pane whose terminal cannot be read.
func TestAnOpenCodeOnlyRefusesAPaneWhoseTerminalItIsOn(t *testing.T) {
	for _, c := range []struct {
		name  string
		procs func(f *fakeProc)
		want  int // 0: admitted
	}{
		{"admitted: setsid, stdin /dev/null, under Claude Code", func(f *fakeProc) {
			f.term(11, 10, "claude", 7, ptsOf(7), "claude")
			f.term(12, 11, ".opencode-wrapp", -1, "/dev/null", "opencode", "run", "x")
		}, 0},
		{"admitted: stdin a pipe, on another terminal", func(f *fakeProc) {
			f.term(21, 10, "opencode", 9, "pipe:[4242]", "/opt/oc/bin/opencode", "run")
		}, 0},
		{"refused: stdin is the pane's terminal (no controlling tty)", func(f *fakeProc) {
			f.term(31, 10, ".opencode-wrapp", -1, ptsOf(7), "opencode")
		}, 31},
		{"refused: controlling terminal is the pane's (stdin /dev/null)", func(f *fakeProc) {
			f.term(41, 10, "claude", 7, ptsOf(7), "claude")
			f.term(42, 41, ".opencode-wrapp", 7, "/dev/null", "opencode", "run", "x")
		}, 42},
		{"refused: its stdin cannot be read", func(f *fakeProc) {
			f.term(51, 10, ".opencode-wrapp", -1, "", "opencode")
		}, 51},
		{"refused: a controlling terminal and a pane with none to compare", func(f *fakeProc) {
			f.term(61, 10, ".opencode-wrapp", 9, "/dev/null", "opencode")
		}, 61},
	} {
		t.Run(c.name, func(t *testing.T) {
			proc := newFakeProc(t)
			rootTTY := 7
			if c.want == 61 {
				rootTTY = -1
			}
			proc.term(10, 1, "zsh", rootTTY, ptsOf(7), "-zsh")
			c.procs(proc)
			hp, found, err := HermesIdentity{Proc: proc.root}.InTree(10)
			if err != nil {
				t.Fatalf("InTree: %v", err)
			}
			switch {
			case c.want == 0 && found:
				t.Fatalf("refused pid %d (%s); want it admitted", hp.PID, hp.Rule)
			case c.want != 0 && (!found || hp.PID != c.want || hp.Rule != hermesRuleOpenCode):
				t.Fatalf("found %v pid %d rule %q; want pid %d refused as opencode", found, hp.PID, hp.Rule, c.want)
			}
		})
	}
	// A pane whose own terminal cannot be read refuses an opencode on any.
	proc := newFakeProc(t)
	proc.term(10, 1, "zsh", -1, "", "-zsh")
	proc.term(12, 10, ".opencode-wrapp", -1, "/dev/null", "opencode", "run")
	if hp, found, err := (HermesIdentity{Proc: proc.root}).InTree(10); err != nil || !found || hp.PID != 12 {
		t.Fatalf("a pane with an unreadable terminal: found %v pid %d err %v; want 12 refused", found, hp.PID, err)
	}
}

// TestTheGuardRefusesEveryOpenCodeShapeAndNothingElse is the opencode half of
// the host paste guard's broad match, over a fixture /proc. Each pane is a
// shell with the process under test as its CHILD (or deeper), and each case's
// pid is distinct from the root's and every other case's, so a walk that
// answered "the root" or "some process" cannot pass. The negatives are
// processes that merely NAME opencode (an editor on its config, a grep for
// it, a script of that name run by a shell), which must not be refused on
// opencode's account.
func TestTheGuardRefusesEveryOpenCodeShapeAndNothingElse(t *testing.T) {
	const nixOC = "/nix/store/0000000000000000000000000000000c-opencode-0.0.0/bin/.opencode-wrapped"
	cases := []struct {
		name  string
		procs func(f *fakeProc, root int)
		want  int // 0: not refused
	}{
		{"the Nix-wrapped TUI (comm truncated, argv[0] opencode)", func(f *fakeProc, root int) {
			f.add(7011, root, ".opencode-wrapp", "/work/a", "opencode")
		}, 7011},
		{"the Nix-wrapped binary by its full path (a private serve)", func(f *fakeProc, root int) {
			f.add(7021, root, ".opencode-wrapp", "/work/b", nixOC, "serve", "--port", "4096")
		}, 7021},
		{"an upstream binary install", func(f *fakeProc, root int) {
			f.add(7031, root, "opencode", "/work/c", "/opt/opencode/bin/opencode", "run", "hello")
		}, 7031},
		{"argv[0] alone (a renamed comm)", func(f *fakeProc, root int) {
			f.add(7041, root, "MainThread", "/work/d", "/usr/local/bin/opencode")
		}, 7041},
		{"comm alone (argv[0] set to something else, exec -a)", func(f *fakeProc, root int) {
			f.add(7045, root, ".opencode-wrapp", "/work/d2", "oc")
		}, 7045},
		// An npm or bun shim is a JavaScript runtime that spawns the native
		// binary: the runtime itself is not matched, its opencode CHILD is
		// (the walk goes on into a process that does not match).
		{"an npm shim under node, refused by its native child", func(f *fakeProc, root int) {
			f.add(7051, root, "node", "/work/e", "/usr/bin/node", "/usr/lib/node_modules/opencode-ai/bin/opencode")
			f.add(7052, 7051, "opencode", "/work/e", "/usr/lib/node_modules/opencode-ai/node_modules/opencode-linux-x64/bin/opencode")
		}, 7052},
		{"a bun shim, refused by its native child two levels down", func(f *fakeProc, root int) {
			f.add(7061, root, "bun", "/work/f", "bun", "/srv/oc/bin/opencode", "--continue")
			f.add(7062, 7061, "sh", "/work/f", "sh", "-c", "exec oc-native")
			f.add(7063, 7062, ".opencode-wrapp", "/work/f", "opencode", "--continue")
		}, 7063},
		{"two levels down (nix develop, then bash)", func(f *fakeProc, root int) {
			f.add(7071, root, "nix", "/work/g", "nix", "develop")
			f.add(7072, 7071, "bash", "/work/g", "bash")
			f.add(7073, 7072, ".opencode-wrapp", "/work/g", "opencode")
		}, 7073},
		{"a Claude Code pane with opencode in the foreground (claude &, then opencode)", func(f *fakeProc, root int) {
			f.add(7081, root, "claude", "/work/h", "claude")
			f.add(7083, root, ".opencode-wrapp", "/work/h", "opencode")
		}, 7083},

		{"not: an editor on opencode's config", func(f *fakeProc, root int) {
			f.add(7111, root, "nvim", "/work/i", "nvim", "opencode.json")
		}, 0},
		{"not: grep for opencode", func(f *fakeProc, root int) {
			f.add(7121, root, "grep", "/work/j", "grep", "-r", "opencode", ".")
		}, 0},
		{"not: a shell running a script that is named opencode", func(f *fakeProc, root int) {
			f.add(7131, root, "bash", "/work/k", "bash", "/work/k/opencode")
		}, 0},
		{"not: node running something else", func(f *fakeProc, root int) {
			f.add(7141, root, "node", "/work/l", "node", "server.js", "--name", "opencode-docs")
		}, 0},
		{"not: Claude Code alone", func(f *fakeProc, root int) {
			f.add(7151, root, "claude", "/work/m", "claude")
		}, 0},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			proc := newFakeProc(t)
			root := 9000 + i
			proc.add(root, 1, "zsh", "/work", "-zsh")
			c.procs(proc, root)
			hp, found, err := HermesIdentity{Proc: proc.root}.InTree(root)
			if err != nil {
				t.Fatalf("InTree: %v", err)
			}
			switch {
			case c.want == 0 && found:
				t.Fatalf("refused pid %d (%s); want no refusal", hp.PID, hp.Rule)
			case c.want != 0 && (!found || hp.PID != c.want || hp.Rule != hermesRuleOpenCode):
				t.Fatalf("found %v pid %d rule %q; want pid %d rule %q", found, hp.PID, hp.Rule, c.want, hermesRuleOpenCode)
			}
		})
	}
}

// TestEveryHostPasteIntoAnOpenCodePaneIsRefusedAndTypesNothing is the
// opencode refusal end to end through the host's three entry points that type
// into a pane, as muster's Send, Queue and Send now reach them:
//   - the Terminal view's Send (AnyPane), by window and by session (the
//     session's active pane);
//   - the rendered view's paste into Claude Code (PasteClaude: the Send of
//     the rendered view, and every queued message and Send now), into a
//     Claude Code pane with opencode in the foreground;
//   - an answer typed into a picker, into the same pane.
//
// Each is refused with the opencode words and kind, and NOTHING is typed —
// decided on the HOST, from the pane's processes, whatever muster asked: the
// messages below carry no runtime and no kind. The same host types into a
// shell and a Claude Code window by every path (the controls, which also
// prove the instrument sees what is typed).
func TestEveryHostPasteIntoAnOpenCodePaneIsRefusedAndTypesNothing(t *testing.T) {
	// Every process has its real terminal state: a pane's processes read
	// the pane's pts (stdin and controlling terminal) unless said otherwise.
	proc := newFakeProc(t)
	proc.term(1400, 1, "zsh", 14, ptsOf(14), "-zsh") // %14: a plain shell
	proc.term(2100, 1, "zsh", 21, ptsOf(21), "-zsh") // %21: Claude Code
	proc.term(2110, 2100, "claude", 21, ptsOf(21), "claude")
	proc.term(2200, 1, "zsh", 22, ptsOf(22), "-zsh") // %22: Claude Code backgrounded, opencode in the foreground
	proc.term(2210, 2200, "claude", 22, ptsOf(22), "claude")
	proc.term(2220, 2200, ".opencode-wrapp", 22, ptsOf(22), "opencode")
	proc.term(2300, 1, "zsh", 23, ptsOf(23), "-zsh") // %23: Claude Code whose Bash tool runs `opencode run` off the terminal
	proc.term(2310, 2300, "claude", 23, ptsOf(23), "claude")
	proc.term(2320, 2310, ".opencode-wrapp", -1, "/dev/null", "opencode", "run", "summarise the diff") // setsid: no controlling tty
	proc.term(3000, 1, "zsh", 30, ptsOf(30), "-zsh")                                                   // %30: the opencode TUI, the session's active pane
	proc.term(3010, 3000, ".opencode-wrapp", 30, ptsOf(30), "opencode")
	proc.term(3100, 1, "zsh", 31, ptsOf(31), "-zsh") // %31: opencode from an upstream install
	proc.term(3110, 3100, "opencode", 31, ptsOf(31), "/opt/oc/bin/opencode")
	proc.term(5000, 1, "zsh", 50, ptsOf(50), "-zsh") // %legacy: the session's active pane, as the fake reports it
	proc.term(5010, 5000, ".opencode-wrapp", 50, ptsOf(50), "opencode")
	windows := []WindowInfo{
		{Session: "scratch2", ID: "@4", Index: 1, Pane: "%14", PanePID: 1400, Command: "zsh"},
		{Session: "scratch2", ID: "@7", Index: 8, Active: true, Pane: "%21", PanePID: 2100, Command: "claude", Claude: &ClaudePane{Pane: "%21", Gen: "g7"}},
		{Session: "scratch2", ID: "@8", Index: 9, Pane: "%22", PanePID: 2200, Command: "opencode", Claude: &ClaudePane{Pane: "%22", Gen: "g8"}},
		{Session: "scratch2", ID: "@12", Index: 10, Pane: "%23", PanePID: 2300, Command: "claude", Claude: &ClaudePane{Pane: "%23", Gen: "g12"}},
		{Session: "solo", ID: "@9", Index: 0, Active: true, Pane: "%30", PanePID: 3000, Command: "opencode"},
		// The label is NOT what the guard reads: this window's command reads
		// "zsh", and its pane is refused all the same.
		{Session: "solo", ID: "@10", Index: 1, Pane: "%31", PanePID: 3100, Command: "zsh"},
	}
	// %22's screen is opencode's TUI, which runs in its foreground — what a
	// Claude pane shows once opencode was started in front of Claude Code.
	b := &windowsBackend{windows: windows, screens: map[string]string{"%22": openCodeTUIScreen}}
	var typed []string
	g := guardedBackend{windowsBackend: b, proc: proc.root, mu: &sync.Mutex{}, typed: &typed,
		roots: map[string]int{"%14": 1400, "%21": 2100, "%22": 2200, "%23": 2300, "%30": 3000, "%31": 3100, "%legacy": 5000}}
	d := newDrivenMuster(t)
	d.run(t, wrapped{g}, nil, true)
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
	typeOf := func(m mirror.Msg) string {
		if m.Type == mirror.TypeAnswer {
			return mirror.TypeAnswered
		}
		return mirror.TypePasted
	}
	ask := &mirror.AskTyping{Questions: hostAskQs, Choices: []askq.Choice{{Picks: []int{1}}}}

	for i, m := range []mirror.Msg{
		{Type: mirror.TypePaste, Session: "scratch2", Window: "@4", Text: "ls -la", AnyPane: true},
		{Type: mirror.TypePaste, Session: "scratch2", Window: "@7", Text: "to claude, terminal", AnyPane: true},
		{Type: mirror.TypePaste, Session: "scratch2", Window: "@7", Text: "to claude, rendered"},
		{Type: mirror.TypeAnswer, Session: "scratch2", Window: "@7", Ask: ask},
		// 🔴 A Claude pane whose Bash tool runs opencode OFF the terminal
		// (stdin /dev/null, no controlling tty) is Claude's, and pasted into.
		{Type: mirror.TypePaste, Session: "scratch2", Window: "@12", Text: "to claude, beside opencode run", AnyPane: true},
		{Type: mirror.TypePaste, Session: "scratch2", Window: "@12", Text: "rendered, beside opencode run"},
	} {
		m.ID = uint32(1 + i)
		d.send(t, m)
		if a := reply(typeOf(m), m.ID); a.Error != "" || a.Refusal != "" {
			t.Fatalf("control %d (%s into %s): refused %q (%q)", m.ID, m.Type, m.Window, a.Error, a.Refusal)
		}
	}
	wantPasted := []string{"%14 ls -la", "%21 to claude, terminal", "%21 to claude, rendered",
		"%23 to claude, beside opencode run", "%23 rendered, beside opencode run"}
	if got := pasted(); !slices.Equal(got, wantPasted) {
		t.Fatalf("instrument check: pasted %q, want %q", got, wantPasted)
	}
	if got := answers(); !slices.Equal(got, []string{"%21"}) {
		t.Fatalf("instrument check: answers typed into %q, want [%%21]", got)
	}

	for i, m := range []mirror.Msg{
		{Type: mirror.TypePaste, Session: "solo", Window: "@9", Text: "y", AnyPane: true},
		{Type: mirror.TypePaste, Session: "solo", Text: "allow it", AnyPane: true},
		{Type: mirror.TypePaste, Session: "solo", Window: "@10", Text: "unlabelled", AnyPane: true},
		{Type: mirror.TypePaste, Session: "scratch2", Window: "@8", Text: "queued for claude"},
		{Type: mirror.TypePaste, Session: "scratch2", Window: "@8", Text: "for claude, terminal", AnyPane: true},
		{Type: mirror.TypeAnswer, Session: "scratch2", Window: "@8", Ask: ask},
	} {
		m.ID = uint32(10 + i)
		d.send(t, m)
		a := reply(typeOf(m), m.ID)
		// The refusal is the guard's own words EXACTLY, and its kind.
		if a.Error != mirror.PasteRefusedOpenCodeText || (a.Type == mirror.TypePasted && a.Refusal != mirror.PasteRefusalOpenCode) {
			t.Fatalf("%s into %s/%s answered %q (kind %q); want %q (kind %q)", m.Type, m.Session, m.Window, a.Error, a.Refusal,
				mirror.PasteRefusedOpenCodeText, mirror.PasteRefusalOpenCode)
		}
	}
	if got := pasted(); !slices.Equal(got, wantPasted) {
		t.Fatalf("a refused paste was typed: pasted %q, want only %q", got, wantPasted)
	}
	if got := answers(); !slices.Equal(got, []string{"%21"}) {
		t.Fatalf("a refused answer was typed: into %q, want only [%%21]", got)
	}
}
