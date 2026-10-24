package mirrorhost

import (
	"slices"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// hostRuntimesBySpec is the host's runtimes by name, failing the test unless
// every runtime in mirror.Runtimes has a host implementation, and the
// reverse.
func hostRuntimesBySpec(t *testing.T) map[string]Runtime {
	t.Helper()
	out := map[string]Runtime{}
	for _, r := range knownRuntimes {
		if _, ok := mirror.LookupRuntime(r.Name()); !ok {
			t.Fatalf("the host's runtime %q is not in the server's table", r.Name())
		}
		out[r.Name()] = r
	}
	for _, s := range mirror.Runtimes() {
		if _, ok := out[s.Name]; !ok {
			t.Fatalf("the server's runtime %q has no host implementation", s.Name)
		}
	}
	return out
}

// TestAHostViewSourceExistsExactlyWhereTheServerSaysAView: a runtime the
// server gives a rendered view (mirror.RuntimeSpec.Caps.View) has a host view
// source, and one it does not has none — so muster never asks a host for a
// view it cannot stream, nor leaves one it can unused.
func TestAHostViewSourceExistsExactlyWhereTheServerSaysAView(t *testing.T) {
	for name, r := range hostRuntimesBySpec(t) {
		spec, _ := mirror.LookupRuntime(name)
		if _, ok := r.(viewSource); ok != spec.Caps.View {
			t.Errorf("%s: a host view source %v, the server's View %v", name, ok, spec.Caps.View)
		}
	}
}

// TestAHostGatedSenderExistsExactlyWhereTheServerGatesTheSend: a runtime the
// server sends to only through the host's gated send (GatedSend) has a host
// gatedSender — the paste routed by Msg.Runtime — and no other runtime does.
func TestAHostGatedSenderExistsExactlyWhereTheServerGatesTheSend(t *testing.T) {
	for name, r := range hostRuntimesBySpec(t) {
		spec, _ := mirror.LookupRuntime(name)
		if _, ok := r.(gatedSender); ok != spec.GatedSend {
			t.Errorf("%s: a host gated sender %v, the server's GatedSend %v", name, ok, spec.GatedSend)
		}
	}
	// The listing's runtimes are the ones that locate panes: Claude Code
	// first, then Hermes; the opencode stub locates nothing.
	var loc []string
	for _, r := range (Tmux{}).runtimes() {
		loc = append(loc, r.Name())
	}
	if want := []string{mirror.RuntimeClaude, mirror.RuntimeHermes}; !slices.Equal(loc, want) {
		t.Fatalf("locating runtimes %q, want %q", loc, want)
	}
}

// TestTheOpenCodeStubReportsNoViewAndNoSend: opencode is a runtime of the
// interface that can do nothing yet — "view: no, send: no", no prompts, in
// the server's table — claims no pane even when one runs opencode, has no
// view source, no gated send and no answerer, and its one behaviour is the
// paste guard's refusal.
func TestTheOpenCodeStubReportsNoViewAndNoSend(t *testing.T) {
	r, ok := runtimeNamed(mirror.RuntimeOpenCode)
	if !ok {
		t.Fatal("no opencode runtime")
	}
	spec, ok := mirror.LookupRuntime(mirror.RuntimeOpenCode)
	if !ok || spec.Caps.View || spec.Caps.Send || spec.Caps.Prompts || spec.OnWire || spec.KeyPrefix != "" || spec.GatedSend {
		t.Fatalf("the stub's spec does more than report itself: %+v", spec)
	}
	if _, ok := r.(viewSource); ok {
		t.Error("the stub has a view source")
	}
	if _, ok := r.(gatedSender); ok {
		t.Error("the stub has a send")
	}
	if _, ok := r.(answerer); ok {
		t.Error("the stub answers")
	}
	proc := newFakeProc(t)
	proc.add(10, 1, "zsh", "/w", "-zsh")
	proc.stdin(10, "/dev/pts/1")
	proc.add(11, 10, ".opencode-wrapp", "/w", "opencode")
	procs, children := (ClaudeLocator{Proc: proc.root}).table()
	if got := r.LocatePanes([]pane{{Session: "s", ID: "%1", PID: 10, TTY: "/dev/pts/1"}}, procs, children); len(got) != 0 {
		t.Fatalf("the stub claimed %+v", got)
	}
	g, ok := r.(pasteGuard)
	if !ok {
		t.Fatal("the stub is not a paste guard")
	}
	if rule, ok := g.shape(procInfo{comm: ".opencode-wrapp", argv: []string{"opencode"}}, func() string { return "" }); !ok || !g.owns(rule) ||
		g.refusal() != ErrOpenCodePane || g.refusalKind() != mirror.PasteRefusalOpenCode {
		t.Fatalf("the stub's guard: rule %q ok %v", rule, ok)
	}
}
