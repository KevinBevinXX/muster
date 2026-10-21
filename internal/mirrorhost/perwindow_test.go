package mirrorhost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestParseWindowsIsOneEntryPerWindowFromItsActivePane: the listing a poll
// reads. Each (session, window) is its ACTIVE pane's line; the id is carried
// beside the index (they differ, and the id is what addresses); the session's
// current window is marked; a viewer session's windows are not listed; and the
// result is ordered by session, then index. Every value is pairwise distinct so
// a parser that swapped two fields cannot pass.
func TestParseWindowsIsOneEntryPerWindowFromItsActivePane(t *testing.T) {
	out := "" +
		// session "scratch2": window @7 at index 3 (active), @4 at index 1, and a
		// two-pane window @9 at index 8 whose SECOND pane is the active one.
		"1\t@7\t3\t1\t%21\t4101\t1791903700\t1791903900\tclaude\t/dev/pts/121\tscratch2\n" +
		"1\t@4\t1\t0\t%14\t4102\t1791903800\t1791903900\tzsh\t/dev/pts/114\tscratch2\n" +
		"0\t@9\t8\t0\t%30\t4103\t1791903650\t1791903900\tzsh\t/dev/pts/130\tscratch2\n" +
		"1\t@9\t8\t0\t%31\t4104\t1791903650\t1791903900\topencode\t/dev/pts/131\tscratch2\n" +
		// another session, one window, the session's activity OLDER than the window's.
		"1\t@2\t0\t1\t%5\t4105\t1791903999\t1791903100\tvim\t/dev/pts/105\ta b: c\n" +
		// a viewer session's rows (it links scratch2's windows): never listed.
		"1\t@7\t3\t0\t%21\t4101\t1791903700\t1791903000\tclaude\t/dev/pts/121\tmuster-view-0123456789ab\n" +
		"1\t@4\t1\t1\t%14\t4102\t1791903800\t1791903000\tzsh\t/dev/pts/114\tmuster-view-0123456789ab\n" +
		// junk: too few fields, a non-id window, a non-pane, a bad index, a bad pid,
		// and a name tmux would never print (a control character).
		"1\t@1\t0\t1\t%1\t1\t1\t1\tzsh\t/dev/pts/101\n" +
		"1\t7\t0\t1\t%1\t10\t1\t1\tzsh\t/dev/pts/101\tbadid\n" +
		"1\t@1\t0\t1\t1\t10\t1\t1\tzsh\t/dev/pts/999\tbadpane\n" +
		"1\t@1\tx\t1\t%1\t10\t1\t1\tzsh\t/dev/pts/101\tbadindex\n" +
		"1\t@1\t0\t1\t%1\t0\t1\t1\tzsh\t/dev/pts/101\tbadpid\n" +
		"1\t@1\t0\t1\t%1\t10\t1\t1\tzsh\t/dev/pts/101\tbad\x07name\r\n"
	got := parseWindows(out)
	want := []WindowInfo{
		{Session: "a b: c", ID: "@2", Index: 0, Active: true, Pane: "%5", PanePID: 4105, TTY: "/dev/pts/105", Activity: 1791903999, Command: "vim"},
		{Session: "scratch2", ID: "@4", Index: 1, Pane: "%14", PanePID: 4102, TTY: "/dev/pts/114", Activity: 1791903800, Command: "zsh"},
		// The ACTIVE window takes the session's activity when that is newer (a
		// client switched to it); a background window never does.
		{Session: "scratch2", ID: "@7", Index: 3, Active: true, Pane: "%21", PanePID: 4101, TTY: "/dev/pts/121", Activity: 1791903900, Command: "claude"},
		{Session: "scratch2", ID: "@9", Index: 8, Pane: "%31", PanePID: 4104, TTY: "/dev/pts/131", Activity: 1791903650, Command: "opencode"},
	}
	// The non-active panes are checked by TestParseWindowsKeepsEveryPaneOfASplit.
	strip := func(w WindowInfo) WindowInfo { w.panes = nil; return w }
	if !slices.EqualFunc(got, want, func(a, b WindowInfo) bool { return reflect.DeepEqual(strip(a), strip(b)) }) {
		t.Fatalf("parseWindows =\n  %+v\nwant\n  %+v", got, want)
	}
}

// TestLocatePanesGivesEachWindowOfOneSessionItsOwnTranscript is the defect a
// per-session key could not avoid: TWO Claude Code windows of ONE session, in
// the SAME cwd. Keyed by pane, each resolves to its own process and so its own
// transcript — and the older transcript belongs to the pane listed second, so
// "the newest in the cwd" (what a cwd-keyed lookup would answer for both) is
// wrong for exactly one of them.
func TestLocatePanesGivesEachWindowOfOneSessionItsOwnTranscript(t *testing.T) {
	proc, home := newFakeProc(t), newFakeHome(t)
	const cwd = "/work/repo"
	proc.add(100, 1, "zsh", cwd, "-zsh")
	proc.add(110, 100, "claude", cwd, "claude")
	proc.add(200, 1, "zsh", cwd, "-zsh")
	proc.add(210, 200, "claude", cwd, "claude", "--continue")
	home.session(110, "sess-first", cwd, "idle")
	home.session(210, "sess-second", cwd, "busy")
	now := time.Now()
	first := home.transcript(cwd, "sess-first", now)                   // the NEWEST in the cwd
	second := home.transcript(cwd, "sess-second", now.Add(-time.Hour)) // older, and still pane %12's

	got := ClaudeLocator{Proc: proc.root, Home: home.root}.LocatePanes([]pane{
		{Session: "scratch2", ID: "%11", PID: 100},
		{Session: "scratch2", ID: "%12", PID: 200},
	})
	if a := got["%11"]; a.Transcript != first || a.Gen != "sess-first" || a.PID != 110 || a.Busy {
		t.Fatalf("pane %%11 = %+v, want pid 110 sess-first %s", a, first)
	}
	if b := got["%12"]; b.Transcript != second || b.Gen != "sess-second" || b.PID != 210 || !b.Busy {
		t.Fatalf("pane %%12 = %+v, want pid 210 sess-second %s (its own, NOT the newest in the cwd)", b, second)
	}
	// 🔴 The per-SESSION view of the same two panes can only hold one of them —
	// which is why a window is never resolved through it.
	if bySession := (ClaudeLocator{Proc: proc.root, Home: home.root}).Locate([]pane{
		{Session: "scratch2", ID: "%11", PID: 100}, {Session: "scratch2", ID: "%12", PID: 200},
	}); len(bySession) != 1 {
		t.Fatalf("instrument check: Locate keyed two panes of one session to %d entries", len(bySession))
	}
}

// TestLocatePanesDropsAGuessedTranscriptTwoPanesShare: with NO sessions files
// (a CLI older than them) each pane's transcript is GUESSED from its cwd — the
// newest file there — so two CLIs in one cwd guess the SAME file. Neither is
// shown it. The control: one such CLI alone still resolves, and a third pane in
// another cwd is unaffected.
func TestLocatePanesDropsAGuessedTranscriptTwoPanesShare(t *testing.T) {
	proc, home := newFakeProc(t), newFakeHome(t)
	const shared, own = "/work/shared", "/work/own"
	proc.add(10, 1, "claude", shared, "claude")
	proc.add(20, 1, "claude", shared, "claude")
	proc.add(30, 1, "claude", own, "claude")
	now := time.Now()
	guess := home.transcript(shared, "whoever-wrote-last", now)
	mine := home.transcript(own, "only-one-here", now)
	loc := ClaudeLocator{Proc: proc.root, Home: home.root}

	got := loc.LocatePanes([]pane{{ID: "%1", PID: 10}, {ID: "%2", PID: 20}, {ID: "%3", PID: 30}})
	if _, ok := got["%1"]; ok {
		t.Fatalf("pane %%1 was shown a transcript two panes guessed: %+v", got["%1"])
	}
	if _, ok := got["%2"]; ok {
		t.Fatalf("pane %%2 was shown a transcript two panes guessed: %+v", got["%2"])
	}
	if got["%3"].Transcript != mine {
		t.Fatalf("pane %%3 (alone in its cwd) = %+v, want %s", got["%3"], mine)
	}
	// Control: the same guess, by ONE pane, is kept — so the drop above is the
	// sharing, not the guessing.
	if alone := loc.LocatePanes([]pane{{ID: "%1", PID: 10}}); alone["%1"].Transcript != guess {
		t.Fatalf("a lone guessed transcript = %+v, want %s", alone["%1"], guess)
	}
	// A pane that NAMED its session keeps it even when another pane guesses the
	// same file: only the guess is dropped.
	home.session(10, "whoever-wrote-last", shared, "idle")
	got = loc.LocatePanes([]pane{{ID: "%1", PID: 10}, {ID: "%2", PID: 20}})
	if got["%1"].Transcript != guess {
		t.Fatalf("the pane that named its session lost it: %+v", got)
	}
	if _, ok := got["%2"]; ok {
		t.Fatalf("the guessing pane was shown the other pane's transcript: %+v", got["%2"])
	}
}

// TestWireWindowsLabelsByCodenameAndWindowIndex: a window of a session with a
// codename is "<codename>:<its own index>" — each window its own, not the
// session's active one — and a session without a codename sends no label
// (muster shows "<session>:<index>"). Claude is carried only when the tunnel
// carries a rendered view.
func TestWireWindowsLabelsByCodenameAndWindowIndex(t *testing.T) {
	raw := []WindowInfo{
		{Session: "scratch2", ID: "@4", Index: 1, Pane: "%14", PanePID: 1, Activity: 1791903800, Command: "zsh"},
		{Session: "scratch2", ID: "@7", Index: 8, Active: true, Pane: "%21", PanePID: 2, Activity: 1791903961, Command: "claude", Claude: &ClaudePane{Pane: "%21"}},
		{Session: "plain", ID: "@2", Index: 5, Active: true, Pane: "%5", PanePID: 3, Command: "vim"},
	}
	now := time.Unix(1791904000, 0)
	got := WireWindows(raw, map[string]string{"scratch2": "Gold"}, true, now)
	want := []mirror.Window{
		{Session: "plain", ID: "@2", Index: 5, Active: true, Command: "vim"},
		{Session: "scratch2", ID: "@4", Index: 1, Label: "Gold:1", Activity: 1791903780, Command: "zsh"},
		{Session: "scratch2", ID: "@7", Index: 8, Active: true, Label: "Gold:8", Activity: 1791903960, Claude: true, Command: "claude"},
	}
	if !slices.EqualFunc(got, want, mirror.Window.Equal) {
		t.Fatalf("WireWindows =\n  %+v\nwant\n  %+v", got, want)
	}
	for _, w := range WireWindows(raw, nil, false, now) {
		if w.Claude || w.Label != "" {
			t.Fatalf("a v1 tunnel / no codenames still sent %+v", w)
		}
	}
}

// TestKillViewSessionRefusesAnyNameThatIsNotAViewerSessions is the guard on the
// ONE place muster-mirror ends a tmux session: only a name viewSessionName
// makes. The binary is a recorder, so a kill that got through is seen; the
// control is a real viewer name, which does reach it.
func TestKillViewSessionRefusesAnyNameThatIsNotAViewerSessions(t *testing.T) {
	rec := recordingTmux(t)
	tm := Tmux{Bin: rec.bin}
	for _, name := range []string{
		"main", "scratch2", "", "muster-view-", "muster-view-short", "muster-view-0123456789AB",
		"muster-view-0123456789abc", "xmuster-view-0123456789ab", "muster-view-0123456789ab; kill-server",
		"muster-view-0123456789a ", "=muster-view-0123456789ab",
	} {
		if err := tm.killViewSession(context.Background(), name); err == nil {
			t.Errorf("killViewSession(%q) was not refused", name)
		}
	}
	if calls := rec.calls(t); len(calls) != 0 {
		t.Fatalf("a refused name still reached tmux: %q", calls)
	}
	if err := tm.killViewSession(context.Background(), "muster-view-0123456789ab"); err != nil {
		t.Fatal(err)
	}
	if calls := rec.calls(t); len(calls) != 1 || calls[0] != "kill-session -t =muster-view-0123456789ab" {
		t.Fatalf("the control kill = %q, want exactly one exact-match kill-session", calls)
	}
	if name := viewSessionName(); !viewSessionPattern.MatchString(name) || !mirror.IsViewSession(name) {
		t.Fatalf("viewSessionName made %q, which this process could neither end nor hide", name)
	}
}

// TestViewClientFlagsAlwaysIgnoreSize: every window viewer is ignore-size
// (the measurement is the tmuxit suite's); only the read-only one is read-only.
func TestViewClientFlagsAlwaysIgnoreSize(t *testing.T) {
	if got := ViewClientFlags(true); got != "read-only,ignore-size" {
		t.Fatalf("read-only flags = %q", got)
	}
	if got := ViewClientFlags(false); got != "ignore-size" {
		t.Fatalf("interactive flags = %q (it must not be read-only, and must be ignore-size)", got)
	}
}

// windowsBackend is a per-window host's tmux, played: a settable window list,
// with every read and every attach counted by kind.
type windowsBackend struct {
	mu      sync.Mutex
	windows []WindowInfo
	err     error
	reads   int
	// legacy counts the per-session reads a per-window host must NOT make.
	legacy  []string
	attach  []string // "session" or "session window"
	pasted  []string // "pane text"
	closed  []string
	noPaste bool
}

func (b *windowsBackend) set(w ...WindowInfo) {
	b.mu.Lock()
	b.windows = w
	b.mu.Unlock()
}

func (b *windowsBackend) fail(err error) {
	b.mu.Lock()
	b.err = err
	b.mu.Unlock()
}

func (b *windowsBackend) note(kind string) {
	b.mu.Lock()
	b.legacy = append(b.legacy, kind)
	b.mu.Unlock()
}

func (b *windowsBackend) Windows(context.Context) ([]WindowInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reads++
	if b.err != nil {
		return nil, b.err
	}
	return append([]WindowInfo(nil), b.windows...), nil
}

func (b *windowsBackend) List(context.Context) ([]string, error) {
	b.note("List")
	return nil, nil
}
func (b *windowsBackend) ActiveWindows(context.Context) (map[string]string, error) {
	b.note("ActiveWindows")
	return nil, nil
}
func (b *windowsBackend) Activity(context.Context) (map[string]int64, error) {
	b.note("Activity")
	return nil, nil
}
func (b *windowsBackend) ClaudePanes(context.Context, []string) map[string]ClaudePane {
	b.note("ClaudePanes")
	return nil
}
func (b *windowsBackend) Paste(_ context.Context, p ClaudePane, text string) error {
	return b.PastePane(context.Background(), p.Pane, text)
}
func (b *windowsBackend) ActivePane(context.Context, string) (string, bool) {
	b.note("ActivePane")
	return "%legacy", true
}
func (b *windowsBackend) PastePane(_ context.Context, pane, text string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pasted = append(b.pasted, pane+" "+text)
	return nil
}

func (b *windowsBackend) Attach(_ context.Context, session string, _ bool, _, _ uint16) (Terminal, error) {
	return b.term(session), nil
}

func (b *windowsBackend) AttachWindow(_ context.Context, session, window string, _ bool, _, _ uint16) (Terminal, error) {
	return b.term(session + " " + window), nil
}

func (b *windowsBackend) term(what string) Terminal {
	b.mu.Lock()
	b.attach = append(b.attach, what)
	b.mu.Unlock()
	pr, pw := io.Pipe()
	return &closeTerm{r: pr, w: pw, onClose: func() {
		b.mu.Lock()
		b.closed = append(b.closed, what)
		b.mu.Unlock()
	}}
}

func (b *windowsBackend) snapshot() (reads int, legacy, attach, pasted, closed []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reads, slices.Clone(b.legacy), slices.Clone(b.attach), slices.Clone(b.pasted), slices.Clone(b.closed)
}

type closeTerm struct {
	r       *io.PipeReader
	w       *io.PipeWriter
	once    sync.Once
	onClose func()
}

func (c *closeTerm) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *closeTerm) Write(p []byte) (int, error) { return len(p), nil }
func (c *closeTerm) Resize(uint16, uint16) error { return nil }
func (c *closeTerm) Close() error {
	c.once.Do(func() { _ = c.w.Close(); c.onClose() })
	return nil
}

// drivenMuster is the server side of the tunnel, hand-played like fakeMuster,
// that can also SEND the host requests — and plays an OLDER muster when old is
// set: one that decodes only the fields it knew, so what it "sees" is what a
// deployed muster from before windows would.
type drivenMuster struct {
	srv  *httptest.Server
	mu   sync.Mutex
	ws   *websocket.Conn
	msgs []mirror.Msg
}

func newDrivenMuster(t *testing.T) *drivenMuster {
	t.Helper()
	d := &drivenMuster{}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ws.SetReadLimit(mirror.MaxFrame)
		d.mu.Lock()
		d.ws = ws
		d.mu.Unlock()
		for {
			typ, data, err := ws.Read(r.Context())
			if err != nil {
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			var m mirror.Msg
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			// 🔴 A HELLO IS RECORDED ONLY ONCE ITS WELCOME IS WRITTEN. Tests
			// wait for the hello (d.of) and then send the host a request; the
			// host reads its first frame as the welcome and hangs up on anything
			// else, so a hello recorded before the welcome let a request race it
			// onto the socket first (TestAnAnswerIsTypedIntoTheClaudePaneOnly
			// timed out "waiting for the answered" in CI). muster does the same
			// (Hub.ServeConn registers a host only after its welcome).
			if m.Type == mirror.TypeHello {
				b, _ := json.Marshal(mirror.Msg{Type: mirror.TypeWelcome, Version: m.Version, Host: m.Host})
				if ws.Write(r.Context(), websocket.MessageText, b) != nil {
					return
				}
			}
			d.mu.Lock()
			d.msgs = append(d.msgs, m)
			d.mu.Unlock()
		}
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *drivenMuster) send(t *testing.T, m mirror.Msg) {
	t.Helper()
	d.mu.Lock()
	ws := d.ws
	d.mu.Unlock()
	b, _ := json.Marshal(m)
	if err := ws.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func (d *drivenMuster) of(typ string) []mirror.Msg {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []mirror.Msg
	for _, m := range d.msgs {
		if m.Type == typ {
			out = append(out, m)
		}
	}
	return out
}

func (d *drivenMuster) run(t *testing.T, b Backend, codenames map[string]string, interactive bool) {
	t.Helper()
	c, err := New(Config{URL: d.srv.URL, Token: mirror.TokenPrefix + "t", Host: "laptop", Backend: b, Codenames: codenames,
		Interactive: interactive, AllowHTTP: true, Refresh: 10 * time.Millisecond, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.RunOnce(ctx) }()
	waitUntil(t, "the hello", func() bool { return len(d.of(mirror.TypeHello)) == 1 })
}

var threeWindows = []WindowInfo{
	{Session: "scratch2", ID: "@4", Index: 1, Pane: "%14", PanePID: 1, Command: "zsh"},
	{Session: "scratch2", ID: "@7", Index: 8, Active: true, Pane: "%21", PanePID: 2, Command: "claude", Claude: &ClaudePane{Pane: "%21", Gen: "g7"}},
	{Session: "solo", ID: "@9", Index: 0, Active: true, Pane: "%30", PanePID: 3, Command: "vim"},
}

// TestAPerWindowHostRegistersEveryWindowFromOneReadPerRefresh: the hello carries
// one entry per WINDOW (labelled per window), AND the per-session fields an
// older muster reads — each session described by its ACTIVE window, as before.
// And the refresh loop costs ONE backend read per tick however many windows
// there are: none of the per-session reads (List, ActiveWindows, Activity,
// ClaudePanes) is ever made.
func TestAPerWindowHostRegistersEveryWindowFromOneReadPerRefresh(t *testing.T) {
	d := newDrivenMuster(t)
	b := &windowsBackend{windows: threeWindows}
	d.run(t, NoShrink(wrapped{b}), map[string]string{"scratch2": "Gold"}, true)

	hello := d.of(mirror.TypeHello)[0]
	if got, want := hello.Windows, []mirror.Window{
		{Session: "scratch2", ID: "@4", Index: 1, Label: "Gold:1", Command: "zsh"},
		{Session: "scratch2", ID: "@7", Index: 8, Active: true, Label: "Gold:8", Claude: true, Command: "claude", ClaudeSession: "g7"},
		{Session: "solo", ID: "@9", Index: 0, Active: true, Command: "vim"},
	}; !slices.EqualFunc(got, want, mirror.Window.Equal) {
		t.Fatalf("hello.Windows =\n  %+v\nwant\n  %+v", got, want)
	}
	// What a muster from before windows is told: sessions, each by its active window.
	if !slices.Equal(hello.Sessions, []string{"scratch2", "solo"}) || !slices.Equal(hello.Claude, []string{"scratch2"}) ||
		len(hello.Labels) != 1 || hello.Labels["scratch2"] != "Gold:8" {
		t.Fatalf("the per-session fields = %v / claude %v / labels %v; want [scratch2 solo] / [scratch2] / scratch2=Gold:8",
			hello.Sessions, hello.Claude, hello.Labels)
	}

	reads := func() int { n, _, _, _, _ := b.snapshot(); return n }
	start := reads()
	waitUntil(t, "several refreshes", func() bool { return reads() >= start+5 })
	if n := len(d.of(mirror.TypeSessions)); n != 0 {
		t.Fatalf("an unchanged list was re-sent %d time(s)", n)
	}
	if _, legacy, _, _, _ := b.snapshot(); len(legacy) != 0 {
		t.Fatalf("a per-window host made per-session reads %v — the refresh must be ONE read", legacy)
	}

	// A window closes and another is renumbered: one message, with the new list.
	b.set(threeWindows[1], WindowInfo{Session: "solo", ID: "@9", Index: 4, Active: true, Pane: "%30", PanePID: 3, Command: "vim"})
	waitUntil(t, "the changed list", func() bool { return len(d.of(mirror.TypeSessions)) == 1 })
	got := d.of(mirror.TypeSessions)[0].Windows
	if len(got) != 2 || got[0].ID != "@7" || got[1].ID != "@9" || got[1].Index != 4 {
		t.Fatalf("the re-sent windows = %+v, want @7 and a renumbered @9 (index 4)", got)
	}

	// A failed read is NOT an empty host: nothing is re-sent, and the last list stands.
	b.fail(errors.New("tmux hiccup"))
	at := reads()
	waitUntil(t, "refreshes that fail", func() bool { return reads() >= at+5 })
	if n := len(d.of(mirror.TypeSessions)); n != 1 {
		t.Fatalf("a failed read sent a list (%d messages); it must keep the last one", n)
	}
}

// TestAWindowRequestIsServedByThatWindowAndOnlyAListedOne: an open that names a
// window attaches THAT window through the window attacher (never the session
// attach, which shows whatever the operator has current); one that names only
// the session — an older muster — attaches the session as it always did; and a
// window this host did not list, or a malformed id, is refused with nothing
// attached.
func TestAWindowRequestIsServedByThatWindowAndOnlyAListedOne(t *testing.T) {
	d := newDrivenMuster(t)
	b := &windowsBackend{windows: threeWindows}
	d.run(t, NoShrink(wrapped{b}), nil, true)
	attached := func() []string { _, _, a, _, _ := b.snapshot(); return a }

	d.send(t, mirror.Msg{Type: mirror.TypeOpen, ID: 1, Session: "scratch2", Window: "@4", ReadOnly: true})
	waitUntil(t, "the window attach", func() bool { return len(attached()) == 1 })
	if got := attached()[0]; got != "scratch2 @4" {
		t.Fatalf("an open for window @4 attached %q, want the window attacher on \"scratch2 @4\"", got)
	}
	// An older muster names only the session.
	d.send(t, mirror.Msg{Type: mirror.TypeOpen, ID: 2, Session: "scratch2", ReadOnly: true})
	waitUntil(t, "the session attach", func() bool { return len(attached()) == 2 })
	if got := attached()[1]; got != "scratch2" {
		t.Fatalf("an open naming only the session attached %q, want the session itself", got)
	}

	for i, m := range []mirror.Msg{
		{Session: "scratch2", Window: "@99"},              // not listed
		{Session: "solo", Window: "@4"},                   // listed, but under another session
		{Session: "scratch2", Window: "1"},                // an INDEX is not an address
		{Session: "scratch2", Window: "@4; kill-server"},  // not an id
		{Session: "elsewhere", Window: "@4"},              // an unlisted session
		{Session: "muster-view-0123456789ab", Window: ""}, // a viewer session is never listed
	} {
		m.Type, m.ID, m.ReadOnly = mirror.TypeOpen, uint32(10+i), true
		d.send(t, m)
		id := m.ID
		waitUntil(t, "the refusal", func() bool {
			return slices.ContainsFunc(d.of(mirror.TypeClosed), func(c mirror.Msg) bool { return c.ID == id && c.Error != "" })
		})
	}
	if got := attached(); len(got) != 2 {
		t.Fatalf("a refused open still attached: %q", got)
	}
}

// TestAPasteIntoAWindowTypesIntoThatWindowsPaneReadNow: a paste that names a
// window lands in THAT window's active pane, read at the moment of pasting —
// the pane moved after the list was sent, and the paste follows it. A Claude
// paste into a window whose pane is not Claude Code is refused; one into the
// Claude window is typed there. Naming only the session keeps the old path.
func TestAPasteIntoAWindowTypesIntoThatWindowsPaneReadNow(t *testing.T) {
	d := newDrivenMuster(t)
	b := &windowsBackend{windows: threeWindows}
	d.run(t, wrapped{b}, nil, true)
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

	// The window's pane changes AFTER the hello (a split); the list is unchanged
	// as muster sees it, so nothing is re-sent — the paste must still find it.
	moved := slices.Clone(threeWindows)
	moved[0].Pane = "%77"
	b.set(moved...)
	d.send(t, mirror.Msg{Type: mirror.TypePaste, ID: 1, Session: "scratch2", Window: "@4", Text: "to the shell", AnyPane: true})
	if a := answer(1); a.Error != "" {
		t.Fatalf("an active-pane paste into window @4 was refused: %s", a.Error)
	}
	if got := pasted(); len(got) != 1 || got[0] != "%77 to the shell" {
		t.Fatalf("pasted %q, want the window's pane as it is NOW (%%77)", got)
	}

	// A prompt for Claude never lands in a shell: window @4 is not Claude Code.
	d.send(t, mirror.Msg{Type: mirror.TypePaste, ID: 2, Session: "scratch2", Window: "@4", Text: "for claude"})
	if a := answer(2); a.Error == "" {
		t.Fatal("a Claude paste into a window whose pane is a shell was accepted")
	}
	d.send(t, mirror.Msg{Type: mirror.TypePaste, ID: 3, Session: "scratch2", Window: "@7", Text: "for claude"})
	if a := answer(3); a.Error != "" {
		t.Fatalf("a Claude paste into the Claude window was refused: %s", a.Error)
	}
	if got := pasted(); len(got) != 2 || got[1] != "%21 for claude" {
		t.Fatalf("pasted %q, want the Claude window's pane %%21 second", got)
	}

	// A window that is gone by the time of the paste is refused, not redirected.
	b.set(moved[1], moved[2])
	d.send(t, mirror.Msg{Type: mirror.TypePaste, ID: 4, Session: "scratch2", Window: "@4", Text: "too late", AnyPane: true})
	if a := answer(4); a.Error == "" {
		t.Fatal("a paste into a window that no longer exists was accepted")
	}
	if got := pasted(); len(got) != 2 {
		t.Fatalf("a refused paste was typed somewhere: %q", got)
	}
}

// TestAViewerOfAWindowThatIsGoneIsEnded: when the host stops listing a window
// (it closed, or its session ended), the terminal open on it is closed — a
// viewer session would otherwise keep that window alive on nobody's list. A
// terminal on a window still listed, and one attached to a session by name,
// are left alone.
func TestAViewerOfAWindowThatIsGoneIsEnded(t *testing.T) {
	d := newDrivenMuster(t)
	b := &windowsBackend{windows: threeWindows}
	d.run(t, wrapped{b}, nil, true)
	d.send(t, mirror.Msg{Type: mirror.TypeOpen, ID: 1, Session: "scratch2", Window: "@4", ReadOnly: true})
	d.send(t, mirror.Msg{Type: mirror.TypeOpen, ID: 2, Session: "scratch2", Window: "@7", ReadOnly: true})
	d.send(t, mirror.Msg{Type: mirror.TypeOpen, ID: 3, Session: "scratch2", ReadOnly: true})
	waitUntil(t, "three attaches", func() bool { _, _, a, _, _ := b.snapshot(); return len(a) == 3 })

	b.set(threeWindows[1], threeWindows[2]) // window @4 closes
	waitUntil(t, "the gone window's terminal to end", func() bool { _, _, _, _, c := b.snapshot(); return len(c) >= 1 })
	time.Sleep(100 * time.Millisecond)
	if _, _, _, _, closed := b.snapshot(); len(closed) != 1 || closed[0] != "scratch2 @4" {
		t.Fatalf("closed %q, want only the terminal on the window that is gone", closed)
	}
}

// TestTheLargestWindowListFitsOneFrame is the size behind mirror.MaxWindows. The
// whole list is ONE message, and one over mirror.MaxFrame ends the connection —
// for every window on the host, not just the ones that did not fit. So the
// WORST case is encoded here: MaxWindows entries with every field at its own
// limit and made of the bytes JSON writes six-fold, in MaxWindows different
// sessions, beside the per-session fields at THEIR limits, exactly as a real
// client builds the message (hostSide.read).
//
// It is an INVARIANT GUARD over the constants, not a regression test: it fails
// when someone raises MaxWindows, or a field's limit, past what a frame holds.
// The control is the same list at twice the count, which must NOT fit — so the
// measurement is of the list, and can see an overrun.
func TestTheLargestWindowListFitsOneFrame(t *testing.T) {
	worst := func(n int) []WindowInfo {
		out := make([]WindowInfo, 0, n)
		for i := 0; i < n; i++ {
			// 128 bytes of session name, unique per window, all escaped six-fold.
			name := strings.Repeat("<", 128-len(itoa(i))) + itoa(i)
			out = append(out, WindowInfo{Session: name, ID: "@" + strings.Repeat("9", 10), Index: 1 << 20, Active: true,
				Pane: "%1", PanePID: 1, Activity: 1791903960, Command: strings.Repeat(">", mirror.MaxCommand),
				// The longest status (the send queue's): "waiting", and the longest
				// time the wire lets through (no bound but int64's).
				// And the longest Claude session id the wire keeps (64 bytes).
				// And what it waits on at both fields' rune bounds, every rune one
				// JSON writes six-fold; the list-wide bound (MaxPendingWindows)
				// keeps the first windows' alone.
				Claude: &ClaudePane{Status: mirror.StatusWaiting, StatusSince: math.MaxInt64, Gen: strings.Repeat("f", 64),
					Pending: &mirror.Pending{Summary: strings.Repeat("<", mirror.MaxPendingSummary), Tool: strings.Repeat("&", mirror.MaxPendingTool),
						Last: strings.Repeat(">", mirror.MaxPendingLast)}},
				// Every window with its other panes' Claude sessions at their
				// per-window bound; the list-wide bound (MaxOtherClaude) keeps
				// the first windows' alone.
				Others: otherClaudeAtBound(i)})
		}
		return out
	}
	// The longest codename a label can carry beside ":<index>".
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
		if want := min(n, mirror.MaxWindows); len(l.windows) != want {
			t.Fatalf("a host with %d windows registers %d, want %d", n, len(l.windows), want)
		}
		others := 0
		for _, w := range l.windows {
			others += len(w.OtherClaude)
		}
		if others != mirror.MaxOtherClaude {
			t.Fatalf("instrument check: %d other-pane sessions on the wire, want the bound %d", others, mirror.MaxOtherClaude)
		}
		pending := 0
		for _, w := range l.windows {
			if p := w.Pending; p != nil {
				if len(p.Summary) != mirror.MaxPendingSummary || len(p.Tool) != mirror.MaxPendingTool || len(p.Last) != mirror.MaxPendingLast {
					t.Fatalf("instrument check: a pending field was cut, so this is not the worst case: %+v", p)
				}
				pending++
			}
		}
		if pending != mirror.MaxPendingWindows {
			t.Fatalf("instrument check: %d windows carry what they wait on, want the bound %d", pending, mirror.MaxPendingWindows)
		}
		for _, w := range l.windows {
			if w.Label == "" || w.Command == "" || !w.Claude || w.Status != mirror.StatusWaiting || w.StatusSince != math.MaxInt64 || len(w.ClaudeSession) != 64 {
				t.Fatalf("instrument check: a worst-case field was dropped by normalisation, so this is not the worst case: %+v", w)
			}
		}
		m := l.msg(mirror.TypeHello)
		m.Version, m.Host, m.Interactive, m.PanePaste, m.StatusReport = mirror.ProtocolVersion, strings.Repeat("h", 63), true, true, true
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return len(b)
	}
	got := encode(mirror.MaxWindows)
	if got > mirror.MaxFrame {
		t.Fatalf("the largest window list encodes to %d bytes, over MaxFrame (%d): a host that large could not connect at all", got, mirror.MaxFrame)
	}
	// More windows than the bound are truncated to it (encode checks the count),
	// so a host twice the size still fits.
	if over := encode(2 * mirror.MaxWindows); over > mirror.MaxFrame {
		t.Fatalf("a host with twice MaxWindows encodes to %d bytes, over MaxFrame (%d): the bound did not hold", over, mirror.MaxFrame)
	}
	// Control: the entries themselves are big enough to overrun — the same
	// encoding without the bound would not fit.
	if perWindow := got / mirror.MaxWindows; 2*mirror.MaxWindows*perWindow <= mirror.MaxFrame {
		t.Fatalf("instrument check: at %d bytes a window, even twice MaxWindows fits a frame — this test could not see an overrun", perWindow)
	}
	t.Logf("largest list: %d windows in %d bytes (%.0f%% of MaxFrame)", mirror.MaxWindows, got, 100*float64(got)/float64(mirror.MaxFrame))
}

// recorder is a stand-in tmux binary that records each invocation's arguments
// (one line each) and exits 0.
type recorder struct {
	bin, log string
}

func recordingTmux(t *testing.T) recorder {
	t.Helper()
	dir := t.TempDir()
	r := recorder{bin: dir + "/tmux", log: dir + "/calls"}
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + r.log + "'\n"
	if err := os.WriteFile(r.bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r recorder) calls(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(r.log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

// otherClaudeAtBound is MaxOtherClaudePerWindow distinct 64-byte Claude
// session ids for window i.
func otherClaudeAtBound(i int) []ClaudePane {
	var out []ClaudePane
	for j := 0; j < mirror.MaxOtherClaudePerWindow; j++ {
		id := itoa(i) + "x" + itoa(j)
		out = append(out, ClaudePane{Gen: id + strings.Repeat("e", 64-len(id))})
	}
	return out
}

// TestParseWindowsKeepsEveryPaneOfASplit: a window's NON-active panes are kept
// beside its active one (what Windows locates Claude sessions in, so one in a
// split resolves), each in its own window; a malformed one is dropped.
func TestParseWindowsKeepsEveryPaneOfASplit(t *testing.T) {
	out := "0\t@3\t1\t1\t%9\t5009\t1\t1\tclaude\t/dev/pts/109\twork\n" +
		"1\t@3\t1\t1\t%7\t5007\t1\t1\tzsh\t/dev/pts/107\twork\n" +
		"0\t@3\t1\t1\t%10\t5010\t1\t1\tvim\t/dev/pts/110\twork\n" +
		"0\t@4\t2\t0\t%11\t5011\t1\t1\tzsh\t/dev/pts/111\twork\n" +
		"1\t@4\t2\t0\t%12\t5012\t1\t1\tzsh\t/dev/pts/112\twork\n" +
		"0\t@3\t1\t1\tnotapane\t5013\t1\t1\tzsh\t/dev/pts/999\twork\n"
	got := parseWindows(out)
	if len(got) != 2 || got[0].Pane != "%7" || got[1].Pane != "%12" {
		t.Fatalf("%+v", got)
	}
	if !reflect.DeepEqual(got[0].panes, []pane{{Session: "work", ID: "%9", PID: 5009, TTY: "/dev/pts/109"}, {Session: "work", ID: "%10", PID: 5010, TTY: "/dev/pts/110"}}) ||
		!reflect.DeepEqual(got[1].panes, []pane{{Session: "work", ID: "%11", PID: 5011, TTY: "/dev/pts/111"}}) {
		t.Fatalf("other panes: %+v / %+v", got[0].panes, got[1].panes)
	}
}

// TestASplitPaneNeverTakesItsWindowsClaude: the active pane and a split pane
// of one window run Claude Code in ONE directory with no sessions files, so
// both GUESS the same transcript. The active pane keeps it — its window's
// Claude flag and rendered view, as before split panes were looked at — and
// the ambiguous split pane is left out.
func TestASplitPaneNeverTakesItsWindowsClaude(t *testing.T) {
	proc, home := newFakeProc(t), newFakeHome(t)
	const cwd = "/work/repo"
	proc.add(10, 1, "claude", cwd, "claude")
	proc.add(20, 1, "claude", cwd, "claude")
	guess := home.transcript(cwd, "the-newest-here", time.Now())
	loc := ClaudeLocator{Proc: proc.root, Home: home.root}
	ws := []WindowInfo{{Session: "work", ID: "@1", Pane: "%1", PanePID: 10, panes: []pane{{Session: "work", ID: "%2", PID: 20}}}}
	attachClaude(ws, loc.LocatePanes)
	if ws[0].Claude == nil || ws[0].Claude.Transcript != guess {
		t.Fatalf("the active pane lost its Claude: %+v", ws[0].Claude)
	}
	if len(ws[0].Others) != 0 {
		t.Fatalf("the ambiguous split pane was kept: %+v", ws[0].Others)
	}
}
