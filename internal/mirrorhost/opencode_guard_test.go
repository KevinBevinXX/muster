package mirrorhost

import (
	"slices"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/mirror"
)

// Every fixture here is synthetic: invented store paths, pids and directories.

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
		{"an npm shim under node", func(f *fakeProc, root int) {
			f.add(7051, root, "node", "/work/e", "/usr/bin/node", "/usr/lib/node_modules/opencode-ai/bin/opencode")
		}, 7051},
		{"under bun", func(f *fakeProc, root int) {
			f.add(7061, root, "bun", "/work/f", "bun", "/srv/oc/bin/opencode", "--continue")
		}, 7061},
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
	proc := newFakeProc(t)
	proc.add(1400, 1, "zsh", "/work", "-zsh") // %14: a plain shell
	proc.add(2100, 1, "zsh", "/work", "-zsh") // %21: Claude Code
	proc.add(2110, 2100, "claude", "/work", "claude")
	proc.add(2200, 1, "zsh", "/work", "-zsh") // %22: Claude Code backgrounded, opencode in the foreground
	proc.add(2210, 2200, "claude", "/work", "claude")
	proc.add(2220, 2200, ".opencode-wrapp", "/work", "opencode")
	proc.add(3000, 1, "zsh", "/work", "-zsh") // %30: the opencode TUI, the session's active pane
	proc.add(3010, 3000, ".opencode-wrapp", "/work", "opencode")
	proc.add(3100, 1, "zsh", "/work", "-zsh") // %31: opencode from an upstream install
	proc.add(3110, 3100, "opencode", "/work", "/opt/oc/bin/opencode")
	proc.add(5000, 1, "zsh", "/work", "-zsh") // %legacy: the session's active pane, as the fake reports it
	proc.add(5010, 5000, ".opencode-wrapp", "/work", "opencode")
	windows := []WindowInfo{
		{Session: "scratch2", ID: "@4", Index: 1, Pane: "%14", PanePID: 1400, Command: "zsh"},
		{Session: "scratch2", ID: "@7", Index: 8, Active: true, Pane: "%21", PanePID: 2100, Command: "claude", Claude: &ClaudePane{Pane: "%21", Gen: "g7"}},
		{Session: "scratch2", ID: "@8", Index: 9, Pane: "%22", PanePID: 2200, Command: "opencode", Claude: &ClaudePane{Pane: "%22", Gen: "g8"}},
		{Session: "solo", ID: "@9", Index: 0, Active: true, Pane: "%30", PanePID: 3000, Command: "opencode"},
		// The label is NOT what the guard reads: this window's command reads
		// "zsh", and its pane is refused all the same.
		{Session: "solo", ID: "@10", Index: 1, Pane: "%31", PanePID: 3100, Command: "zsh"},
	}
	b := &windowsBackend{windows: windows}
	var typed []string
	g := guardedBackend{windowsBackend: b, proc: proc.root, mu: &sync.Mutex{}, typed: &typed,
		roots: map[string]int{"%14": 1400, "%21": 2100, "%22": 2200, "%30": 3000, "%31": 3100, "%legacy": 5000}}
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
	} {
		m.ID = uint32(1 + i)
		d.send(t, m)
		if a := reply(typeOf(m), m.ID); a.Error != "" || a.Refusal != "" {
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
