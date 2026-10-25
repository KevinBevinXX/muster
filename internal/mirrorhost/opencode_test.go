package mirrorhost

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/opencodehost"
	"github.com/ZacxDev/muster/internal/opencodehost/opencodetest"
)

const ocFixturePassword = "fixture-opencode-password-0a1b2c"

// TestOpenCodeTUIIsTheTUIOnTheSharedServer: the TUI is opencode with no
// subcommand (a directory and its flags are still the TUI); a subcommand is
// not it; --standalone and --server are the TUI on ANOTHER server, whose
// sessions the host's server does not have.
func TestOpenCodeTUIIsTheTUIOnTheSharedServer(t *testing.T) {
	cases := []struct {
		comm        string
		argv        []string
		tui, shared bool
	}{
		{".opencode-wrapp", []string{"opencode"}, true, true},
		{"opencode", []string{"/nix/store/x/bin/.opencode-wrapped"}, true, true},
		{".opencode-wrapp", []string{"opencode", "/srv/fixture"}, true, true},
		{".opencode-wrapp", []string{"opencode", "-s", "ses_fixture1", "--prompt", "run"}, true, true},
		{".opencode-wrapp", []string{"opencode", "--session=ses_fixture1", "-c"}, true, true},
		{".opencode-wrapp", []string{"opencode", "--standalone"}, true, false},
		{".opencode-wrapp", []string{"opencode", "--server", "http://127.0.0.1:4096"}, true, false},
		{".opencode-wrapp", []string{"opencode", "--server=http://127.0.0.1:4096", "/srv"}, true, false},
		{".opencode-wrapp", []string{"opencode", "serve", "--service"}, false, false},
		{".opencode-wrapp", []string{"opencode", "run", "hello"}, false, false},
		{".opencode-wrapp", []string{"opencode", "--print-logs", "pair"}, false, false},
		{"zsh", []string{"-zsh"}, false, false},
		{"node", []string{"node", "opencode"}, false, false},
	}
	for _, c := range cases {
		tui, shared := openCodeTUI(procInfo{comm: c.comm, argv: c.argv})
		if tui != c.tui || shared != c.shared {
			t.Errorf("%s %q: tui %v shared %v, want %v %v", c.comm, c.argv, tui, shared, c.tui, c.shared)
		}
	}
}

// ocServer is a fake opencode server with sessions, and an OpenCodeSource
// holding a token it issued.
func ocServer(t *testing.T, titles map[string]string) (*opencodetest.Server, *OpenCodeSource) {
	t.Helper()
	srv := opencodetest.New(ocFixturePassword)
	t.Cleanup(srv.Close)
	for id, title := range titles {
		srv.PutSession(id, title, "")
	}
	creds, err := opencodehost.EnvCredentials(srv.URL, srv.Issue())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return srv, NewOpenCodeSource(ctx, creds, "", log.New(io.Discard, "", 0))
}

// ocPaneTable is a pane (%1, pid 10, /dev/pts/1) whose shell runs an
// opencode TUI with argv on the pane's terminal.
func ocPaneTable(t *testing.T, argv ...string) (procs map[int]procInfo, children map[int][]int, root string) {
	t.Helper()
	proc := newFakeProc(t)
	proc.add(10, 1, "zsh", "/w", "-zsh")
	proc.stdin(10, "/dev/pts/1")
	proc.add(11, 10, ".opencode-wrapp", "/w", argv...)
	proc.stdin(11, "/dev/pts/1")
	procs, children = (ClaudeLocator{Proc: proc.root}).table()
	return procs, children, proc.root
}

func titled(title string) func(string) (string, bool) {
	return func(string) (string, bool) { return title, true }
}

var ocPane = []pane{{Session: "s", ID: "%1", PID: 10, TTY: "/dev/pts/1"}}

// TestAnOpenCodePaneShowsTheOneSessionItsTitleNames: the TUI's pane is
// claimed as an opencode window; with a source, the session its title names
// is its view, marked as found by title, with the status the event stream
// reports. An ambiguous title, a title naming no session, a TUI on another
// server and a host with no token are each an opencode window with NO view
// and an unknown status, saying why.
func TestAnOpenCodePaneShowsTheOneSessionItsTitleNames(t *testing.T) {
	srv, src := ocServer(t, map[string]string{"ses_fixtureA": "Fix the flaky fixture", "ses_fixtureB": "New session", "ses_fixtureC": "New session"})
	procs, children, root := ocPaneTable(t, "opencode")
	rt := openCodeRuntime{proc: root, src: src, title: titled("OC | Fix the flaky fixture")}
	// The stream opens on first use; wait for it so the status is believed.
	_ = src.state()
	waitUntil(t, "the event stream", func() bool { s, _ := src.status("x"); return s != "" })
	srv.SetStatus("ses_fixtureA", "busy")
	waitUntil(t, "the busy status", func() bool { s, _ := src.status("ses_fixtureA"); return s == mirror.StatusBusy })

	got := rt.LocatePanes(ocPane, procs, children)["%1"]
	if got.Runtime != mirror.RuntimeOpenCode || got.PID != 11 || got.Gen != "ses_fixtureA" || got.Source != SourceTitle ||
		got.Status != mirror.StatusBusy || !got.Busy || got.StatusSince == 0 || got.OpenCode == nil || got.OpenCode.NoView != "" {
		t.Fatalf("the matched pane: %+v (%+v)", got, got.OpenCode)
	}
	if got.OpenCode.Match.Title != "Fix the flaky fixture" {
		t.Fatalf("match %+v", got.OpenCode.Match)
	}

	noView := func(name string, r openCodeRuntime, procs map[int]procInfo, children map[int][]int, why string) {
		t.Helper()
		ap, ok := r.LocatePanes(ocPane, procs, children)["%1"]
		if !ok || ap.Runtime != mirror.RuntimeOpenCode {
			t.Fatalf("%s: the pane is not an opencode window: %+v", name, ap)
		}
		if ap.Gen != "" || ap.Status != "" || ap.Source != SourceProcess || ap.OpenCode == nil || !strings.Contains(ap.OpenCode.NoView, why) {
			t.Fatalf("%s: %+v (%+v), want no view saying %q", name, ap, ap.OpenCode, why)
		}
		// The card says the same (mirror.Window.AgentNote).
		if ap.Note != ap.OpenCode.NoView {
			t.Fatalf("%s: the note %q is not the reason %q", name, ap.Note, ap.OpenCode.NoView)
		}
	}
	noView("ambiguous", openCodeRuntime{proc: root, src: src, title: titled("OC | New session")}, procs, children, "ambiguous, so none is shown")
	noView("no such title", openCodeRuntime{proc: root, src: src, title: titled("OC | Something else")}, procs, children, "no opencode session is titled")
	sp, sc, sroot := ocPaneTable(t, "opencode", "--standalone")
	noView("standalone", openCodeRuntime{proc: sroot, src: src, title: titled("OC | Fix the flaky fixture")}, sp, sc, "its own server")
	none := NewOpenCodeSource(context.Background(), nil, "no opencode token at the fixture path", log.New(io.Discard, "", 0))
	noView("no token", openCodeRuntime{proc: root, src: none, title: titled("OC | Fix the flaky fixture")}, procs, children, "opencode is not paired on this host: run muster-mirror opencode-pair")

	// No source at all: still an opencode window, nothing resolved.
	if ap := (openCodeRuntime{proc: root}).LocatePanes(ocPane, procs, children)["%1"]; ap.Runtime != mirror.RuntimeOpenCode || ap.Gen != "" || ap.OpenCode != nil {
		t.Fatalf("no source: %+v", ap)
	}
}

// TestARefusedTokenLeavesNoViewAndSaysSo: once the server refuses the token
// (its password rotated, which revokes every token), the window has no view
// and an unknown status, and says why — never a stale match.
func TestARefusedTokenLeavesNoViewAndSaysSo(t *testing.T) {
	srv, src := ocServer(t, map[string]string{"ses_fixtureA": "Fix the flaky fixture"})
	procs, children, root := ocPaneTable(t, "opencode")
	rt := openCodeRuntime{proc: root, src: src, title: titled("OC | Fix the flaky fixture")}
	_ = src.state()
	waitUntil(t, "the event stream", func() bool { s, _ := src.status("x"); return s != "" })
	if ap := rt.LocatePanes(ocPane, procs, children)["%1"]; ap.Gen != "ses_fixtureA" || ap.Status != mirror.StatusIdle {
		t.Fatalf("before: %+v", ap)
	}
	srv.Rotate("rotated-fixture-password")
	waitUntil(t, "the refusal", func() bool { return strings.Contains(src.state(), "refused") })
	ap := rt.LocatePanes(ocPane, procs, children)["%1"]
	if ap.Gen != "" || ap.Status != "" || ap.OpenCode == nil || !strings.Contains(ap.OpenCode.NoView, "opencode-pair") || ap.Note != ap.OpenCode.NoView {
		t.Fatalf("after: %+v (%+v)", ap, ap.OpenCode)
	}
}

// TestTheViewAndTheCardSayWhatTheTokenNeeds: each token state the person
// must act on reaches both places they look. A token within 3 days of expiry,
// and one whose renewal is failing (with why), leave the view in place: the
// card's note and the view's FIRST row say it. An expired token is refused:
// no view, and the card says it expired and how to pair again. A fresh token
// says nothing.
func TestTheViewAndTheCardSayWhatTheTokenNeeds(t *testing.T) {
	const sid = "ses_fixtureA"
	procs, children, root := ocPaneTable(t, "opencode")
	newServer := func(t *testing.T) *opencodetest.Server {
		srv := opencodetest.New(ocFixturePassword)
		t.Cleanup(srv.Close)
		srv.PutSession(sid, "Fix the flaky fixture", "")
		srv.PutMessage(sid, ocUser("msg_01", 1700000000001, "a prompt"))
		return srv
	}
	source := func(t *testing.T, creds *opencodehost.Credentials) *OpenCodeSource {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		return NewOpenCodeSource(ctx, creds, "", log.New(io.Discard, "", 0))
	}
	firstRow := func(t *testing.T, src *OpenCodeSource) cctranscript.Event {
		t.Helper()
		wb := &windowsBackend{windows: []WindowInfo{ocWindowInfo(src, "OC | Fix the flaky fixture")}}
		d := newDrivenMuster(t)
		d.run(t, NoShrink(ocBackend{wb, src}), nil, true)
		d.send(t, mirror.Msg{Type: mirror.TypeWatch, ID: 9, Session: "o", Window: "@7"})
		waitUntil(t, "the backfill", func() bool { evs, _, _ := watchEvents(d, 9); return len(evs) >= 2 })
		evs, _, _ := watchEvents(d, 9)
		return evs[0]
	}
	locate := func(src *OpenCodeSource) AgentPane {
		return openCodeRuntime{proc: root, src: src, title: titled("OC | Fix the flaky fixture")}.LocatePanes(ocPane, procs, children)["%1"]
	}

	t.Run("fresh: nothing", func(t *testing.T) {
		srv := newServer(t)
		creds, _ := opencodehost.EnvCredentials(srv.URL, srv.Issue())
		src := source(t, creds)
		if ap := locate(src); ap.Gen != sid || ap.Note != "" {
			t.Fatalf("%+v", ap)
		}
		if e := firstRow(t, src); !strings.HasPrefix(e.Text, "matched by title") {
			t.Fatalf("the first row: %+v", e)
		}
	})
	t.Run("expires within 3 days", func(t *testing.T) {
		srv := newServer(t)
		creds, _ := opencodehost.EnvCredentials(srv.URL, srv.IssueAt(time.Now().Add(2*24*time.Hour+time.Hour)))
		src := source(t, creds)
		want := "opencode token expires in 2 days and has not been renewed"
		if ap := locate(src); ap.Gen != sid || ap.Note != want {
			t.Fatalf("%+v", ap)
		}
		if e := firstRow(t, src); e.Kind != cctranscript.KindNotice || e.Text != want {
			t.Fatalf("the first row: %+v", e)
		}
	})
	t.Run("renewal failing", func(t *testing.T) {
		srv := newServer(t)
		file := t.TempDir() + "/opencode-token"
		if err := opencodehost.WriteTokenFile(file, opencodehost.TokenFile{URL: srv.URL, Token: srv.IssueAt(time.Now().Add(6 * 24 * time.Hour))}); err != nil {
			t.Fatal(err)
		}
		creds, err := opencodehost.LoadCredentials(file)
		if err != nil {
			t.Fatal(err)
		}
		srv.RefuseRedeem(true) // the token still reads; minting its successor fails
		if _, err := creds.Renew(context.Background(), time.Now()); err == nil {
			t.Fatal("instrument check: the renewal did not fail")
		}
		src := source(t, creds)
		want := "opencode token expires in 5 days — renewal failing: renew the opencode token: opencode refused to redeem the pairing code it had just issued"
		if ap := locate(src); ap.Gen != sid || ap.Note != want {
			t.Fatalf("note %q, want %q (%+v)", ap.Note, want, ap)
		}
		if e := firstRow(t, src); e.Text != want {
			t.Fatalf("the first row: %+v", e)
		}
	})
	t.Run("expired", func(t *testing.T) {
		srv := newServer(t)
		exp := time.Now().Add(-time.Hour).Truncate(time.Second)
		creds, _ := opencodehost.EnvCredentials(srv.URL, srv.IssueAt(exp))
		src := source(t, creds)
		ap := locate(src)
		want := "the opencode token expired " + exp.UTC().Format(time.RFC3339) + ": pair this host again with `muster-mirror opencode-pair`"
		if ap.Gen != "" || ap.Note != want {
			t.Fatalf("note %q, want %q (%+v)", ap.Note, want, ap)
		}
	})
}

// TestAnOpenCodeStatusIsUnknownWhileTheEventStreamIsDown: the session's
// status comes from the event stream only, so while it cannot be opened the
// window still shows its session (the REST reads work) but its status is
// UNKNOWN — never a guessed idle, which would fail open.
func TestAnOpenCodeStatusIsUnknownWhileTheEventStreamIsDown(t *testing.T) {
	srv, src := ocServer(t, map[string]string{"ses_fixtureA": "Fix the flaky fixture"})
	srv.FailEvents(true)
	procs, children, root := ocPaneTable(t, "opencode")
	rt := openCodeRuntime{proc: root, src: src, title: titled("OC | Fix the flaky fixture")}
	_ = src.state()
	waitUntil(t, "a failed stream", func() bool { return strings.Contains(src.state(), "cannot be read") })
	ap := rt.LocatePanes(ocPane, procs, children)["%1"]
	if ap.Gen != "ses_fixtureA" || ap.Status != "" || ap.Busy || ap.StatusSince != 0 {
		t.Fatalf("with the stream down: %+v", ap)
	}
	// The control: the stream back, the status is believed.
	srv.FailEvents(false)
	waitUntil(t, "the event stream", func() bool { s, _ := src.status("x"); return s != "" })
	if ap := rt.LocatePanes(ocPane, procs, children)["%1"]; ap.Status != mirror.StatusIdle {
		t.Fatalf("with the stream up: %+v", ap)
	}
}

// TestTheOpenCodeLocatorClaimsNoOtherRuntimesPane: a Claude Code pane, a
// Hermes one and a plain shell are not opencode windows, and with no opencode
// process in the table no pane title is even read (the negative control: the
// same table with the TUI is read).
func TestTheOpenCodeLocatorClaimsNoOtherRuntimesPane(t *testing.T) {
	_, src := ocServer(t, map[string]string{"ses_fixtureA": "Fix the flaky fixture"})
	proc := newFakeProc(t)
	proc.add(10, 1, "zsh", "/w", "-zsh")
	proc.stdin(10, "/dev/pts/1")
	proc.add(11, 10, "claude", "/w", "claude")
	proc.stdin(11, "/dev/pts/1")
	proc.add(20, 1, "zsh", "/w", "-zsh")
	proc.stdin(20, "/dev/pts/2")
	proc.add(21, 20, "hermes", "/w", "/usr/bin/python3", "/usr/bin/hermes", "chat")
	proc.stdin(21, "/dev/pts/2")
	procs, children := (ClaudeLocator{Proc: proc.root}).table()
	reads := 0
	rt := openCodeRuntime{proc: proc.root, src: src, title: func(string) (string, bool) { reads++; return "OC | Fix the flaky fixture", true }}
	panes := []pane{{Session: "s", ID: "%1", PID: 10, TTY: "/dev/pts/1"}, {Session: "s", ID: "%2", PID: 20, TTY: "/dev/pts/2"}}
	if got := rt.LocatePanes(panes, procs, children); len(got) != 0 || reads != 0 {
		t.Fatalf("claimed %+v, read %d titles", got, reads)
	}
	// The control: an opencode TUI on the pane's terminal is claimed and read.
	proc.add(12, 10, ".opencode-wrapp", "/w", "opencode")
	proc.stdin(12, "/dev/pts/1")
	procs, children = (ClaudeLocator{Proc: proc.root}).table()
	if got := rt.LocatePanes(panes, procs, children); len(got) != 1 || got["%1"].PID != 12 || reads != 1 {
		t.Fatalf("control: claimed %+v, read %d titles", got, reads)
	}
	// An opencode NOT on the pane's terminal (run by a tool, stdin elsewhere)
	// is not this pane's TUI.
	proc2 := newFakeProc(t)
	proc2.add(10, 1, "zsh", "/w", "-zsh")
	proc2.stdin(10, "/dev/pts/1")
	proc2.add(11, 10, ".opencode-wrapp", "/w", "opencode")
	proc2.stdin(11, "/dev/null")
	p2, c2 := (ClaudeLocator{Proc: proc2.root}).table()
	if got := (openCodeRuntime{proc: proc2.root, src: src, title: rt.title}).LocatePanes(ocPane, p2, c2); len(got) != 0 {
		t.Fatalf("an opencode off the pane's terminal was claimed: %+v", got)
	}
}

// ocBackend is a per-window fake whose window runs opencode, with a source.
type ocBackend struct {
	*windowsBackend
	src *OpenCodeSource
}

func (b ocBackend) OpenCodeSource() *OpenCodeSource { return b.src }

func ocWindowInfo(src *OpenCodeSource, title string) WindowInfo {
	ap := ResolveOpenCodePane(src, "%7", 70, title)
	return WindowInfo{Session: "o", ID: "@7", Index: 0, Active: true, Pane: "%7", PanePID: 69, Command: "opencode", Agent: &ap}
}

func ocUser(id string, created int, text string) map[string]any {
	return map[string]any{"id": id, "type": "user", "text": text, "files": []any{}, "agents": []any{}, "time": map[string]any{"created": created}}
}

func ocAssistant(id string, completed int, content ...map[string]any) map[string]any {
	m := map[string]any{"id": id, "type": "assistant", "agent": "build", "time": map[string]any{"created": 1}, "content": content}
	if completed > 0 {
		m["time"] = map[string]any{"created": 1, "completed": completed}
		m["finish"] = "stop"
	}
	return m
}

func ocText(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

func ocTool(id, name, status string, input map[string]any) map[string]any {
	st := map[string]any{"status": status, "input": input, "metadata": map[string]any{}}
	if status == "completed" {
		st["content"] = []any{map[string]any{"type": "text", "text": "TOOL-OUTPUT-NEVER-SHOWN"}}
	}
	return map[string]any{"type": "tool", "id": id, "name": name, "executed": false, "time": map[string]any{"created": 1}, "state": st}
}

// TestAnOpenCodeWindowStreamsItsSessionReadOnly is the rendered view's host
// half for opencode over the real wire, against a fake opencode server read
// through the real client: a Reset backfill that opens with "matched by
// title", the session's prompt, text and tool call (name and target, no
// input, no output), redacted, and the prompt index; an appended message and
// the streaming message's final items arrive WITHOUT a reset; busy follows
// the event stream; an event already sent that changes starts the view
// over; a refused token closes it saying so. Every request the host made was
// a GET.
func TestAnOpenCodeWindowStreamsItsSessionReadOnly(t *testing.T) {
	const sid = "ses_fixtureA"
	srv, src := ocServer(t, map[string]string{sid: "Fix the flaky fixture"})
	secret := "ghp_" + strings.Repeat("B", 36)
	srv.PutMessage(sid, ocUser("msg_01", 1700000000001, "deploy with "+secret))
	srv.PutMessage(sid, ocAssistant("msg_02", 5, ocText("On it."), ocTool("call_1", "shell", "completed", map[string]any{"command": "make deploy", "description": "deploy"})))
	_ = src.state()
	waitUntil(t, "the event stream", func() bool { s, _ := src.status("x"); return s != "" })
	wb := &windowsBackend{windows: []WindowInfo{ocWindowInfo(src, "OC | Fix the flaky fixture")}}
	if wb.windows[0].Agent.Gen != sid {
		t.Fatalf("instrument check: the window resolved %+v", wb.windows[0].Agent)
	}
	d := newDrivenMuster(t)
	d.run(t, NoShrink(ocBackend{wb, src}), nil, true)

	d.send(t, mirror.Msg{Type: mirror.TypeWatch, ID: 4, Session: "o", Window: "@7"})
	waitUntil(t, "the backfill", func() bool { evs, _, _ := watchEvents(d, 4); return len(evs) >= 5 })
	evs, resets, last := watchEvents(d, 4)
	got := evText(evs)
	if resets != 1 || last.Gen != sid {
		t.Fatalf("resets %d gen %q", resets, last.Gen)
	}
	if evs[0].Kind != cctranscript.KindNotice || !strings.HasPrefix(evs[0].Text, "matched by title: Fix the flaky fixture") {
		t.Fatalf("the view does not open with how it was matched: %+v", evs[0])
	}
	if strings.Contains(got, secret) || !strings.Contains(got, "prompt:deploy with") || !strings.Contains(got, "text:On it.") ||
		!strings.Contains(got, "tool_call:shell") || !strings.Contains(got, "tool_result:") {
		t.Fatalf("the backfill: %s", got)
	}
	for _, e := range evs {
		if e.Input != "" || strings.Contains(e.Output, "TOOL-OUTPUT") || (e.Kind == cctranscript.KindToolCall && e.Target != "make deploy") {
			t.Fatalf("a tool event carries more than its name and target: %+v", e)
		}
	}
	waitUntil(t, "the prompt index", func() bool {
		for _, m := range d.of(mirror.TypeEvents) {
			if m.ID == 4 && m.PromptIndex && len(m.Prompts) == 1 && m.Prompts[0].Line == 1700000000001 && !strings.Contains(m.Prompts[0].Text, secret) {
				return true
			}
		}
		return false
	})

	// A new turn streams in: the prompt, then the assistant message rewritten
	// in place — only its final items are drawn, and nothing resets.
	srv.PutMessage(sid, ocUser("msg_03", 1700000000003, "and now the docs"))
	srv.SetStatus(sid, "busy")
	srv.PutMessage(sid, ocAssistant("msg_04", 0, ocText("Read")))
	// The prompt is drawn by the read that also saw the half-written text, so
	// the rewrite below is a SECOND version the tail has to reconcile.
	waitUntil(t, "the live prompt", func() bool {
		evs, _, _ := watchEvents(d, 4)
		return strings.Contains(evText(evs), "prompt:and now the docs")
	})
	srv.PutMessage(sid, ocAssistant("msg_04", 0, ocText("Reading the docs."), ocTool("call_2", "read", "running", map[string]any{"filePath": "/srv/fixture/README.md"})))
	waitUntil(t, "the live turn", func() bool {
		evs, _, _ := watchEvents(d, 4)
		s := evText(evs)
		return strings.Contains(s, "prompt:and now the docs") && strings.Contains(s, "text:Reading the docs.") && strings.Contains(s, "tool_call:read")
	})
	waitUntil(t, "busy", func() bool { _, _, last := watchEvents(d, 4); return last.Busy })
	srv.PutMessage(sid, ocAssistant("msg_04", 9, ocText("Reading the docs."), ocTool("call_2", "read", "completed", map[string]any{"filePath": "/srv/fixture/README.md"}), ocText("Done.")))
	srv.SetStatus(sid, "idle")
	waitUntil(t, "the finished turn", func() bool { evs, _, _ := watchEvents(d, 4); return strings.Contains(evText(evs), "text:Done.") })
	waitUntil(t, "idle", func() bool {
		ms := d.of(mirror.TypeEvents)
		return len(ms) > 0 && !ms[len(ms)-1].Busy
	})
	evs, resets, _ = watchEvents(d, 4)
	if resets != 1 {
		t.Fatalf("the live turn reset the view (%d resets): %s", resets, evText(evs))
	}
	if n := strings.Count(evText(evs), "text:Reading the docs."); n != 1 {
		t.Fatalf("the streaming text was drawn %d times: %s", n, evText(evs))
	}

	// An event already sent changed (a message rewritten after it was final):
	// the view starts over, with the new text.
	srv.PutMessage(sid, ocAssistant("msg_02", 5, ocText("On it, rewritten."), ocTool("call_1", "shell", "completed", map[string]any{"command": "make deploy"})))
	waitUntil(t, "the reset", func() bool { _, r, _ := watchEvents(d, 4); return r == 2 })
	if evs, _, _ := watchEvents(d, 4); !strings.Contains(evText(evs), "On it, rewritten.") {
		t.Fatalf("the reset did not redraw: %s", evText(evs))
	}

	// The token is refused: the view closes, saying so.
	srv.Rotate("rotated-fixture-password")
	srv.PutMessage(sid, ocUser("msg_05", 1700000000005, "after the rotation"))
	waitUntil(t, "the view to close", func() bool {
		for _, m := range d.of(mirror.TypeClosed) {
			if m.ID == 4 && strings.Contains(m.Error, "refused muster-mirror's token") {
				return true
			}
		}
		return false
	})
	for _, r := range srv.Requests() {
		if r.Method != "GET" {
			t.Fatalf("the host sent %s %s", r.Method, r.Path)
		}
	}
}

// TestTheHostSendsAnOpenCodeWindowsNote: the pane's note travels with the
// window (mirror.Window.AgentNote), which is how the card shows it.
func TestTheHostSendsAnOpenCodeWindowsNote(t *testing.T) {
	ap := AgentPane{Runtime: mirror.RuntimeOpenCode, Pane: "%7", PID: 70, Source: SourceProcess, Note: notPairedWhy}
	got := WireWindows([]WindowInfo{{Session: "o", ID: "@7", Pane: "%7", PanePID: 69, Command: "opencode", Agent: &ap}}, nil, true, time.Now())
	if len(got) != 1 || got[0].AgentNote != "opencode is not paired on this host: run muster-mirror opencode-pair" || got[0].Runtime != mirror.RuntimeOpenCode {
		t.Fatalf("%+v", got)
	}
}
