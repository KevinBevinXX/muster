package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/auth"
	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// The terminal socket, end to end through the real mux and a real WebSocket
// client, over a fake terminal that records what reached it.
// ---------------------------------------------------------------------------

// termAgents serves two agents by name: a claude-code one and a gateway one.
type termAgents struct{ stubAgentsStore }

func (termAgents) GetByName(_ context.Context, name string) (agents.Agent, error) {
	switch name {
	case "keen-wren":
		return agents.Agent{ID: 7, Name: name, Kind: agents.KindClaudeCode, CCAccount: "alpha"}, nil
	case "plain-otter":
		return agents.Agent{ID: 8, Name: name, Kind: agents.KindGateway}, nil
	}
	return agents.Agent{}, errors.New("no rows")
}

// termCall is one AttachTerminal the fake received.
type termCall struct {
	agent    string
	readOnly bool
}

// fakeTerminal plays the pod: it records the call, every byte that reached its
// stdin and every size, and stays attached until its context ends — unless
// refuse or exitAfter says otherwise.
type fakeTerminal struct {
	stubProvisioner

	refuse    error // returned at once, before any output
	exitAfter bool  // write a banner, then return nil (the client exited)
	// release, when set, is waited for before returning on EVERY exit path
	// once attached — the adapter's release of the pod-side client happens in
	// that interval, and the adapter runs it whatever ended the attach
	// (agentprovision.Adapter.AttachTerminal). Each attach that reaches its
	// release announces it on releasing first.
	//
	// 🔴 "EVERY EXIT PATH" IS THE POINT. This fake used to wait for release only
	// in its main loop, so an attach whose context ended while its banner write
	// was still in flight returned AT ONCE, with no release — and the browser
	// is told "ready" BEFORE that write finishes (terminalWriter.Write), so a
	// test that ended terminals right after reading "ready" raced it. That was
	// the whole flake in both TestEndTerminals* tests: the fake skipping a
	// release the real adapter never skips.
	release   chan struct{}
	releasing chan struct{}

	mu     sync.Mutex
	calls  []termCall
	stdin  []byte
	sizes  []provision.TermSize
	active int
	ended  int
	event  chan struct{}
}

func newFakeTerminal() *fakeTerminal {
	return &fakeTerminal{event: make(chan struct{}, 64), releasing: make(chan struct{}, 64)}
}

func (f *fakeTerminal) signal() {
	select {
	case f.event <- struct{}{}:
	default:
	}
}

func (f *fakeTerminal) AttachTerminal(ctx context.Context, a agents.Agent, readOnly bool, stdin io.Reader, stdout io.Writer, resize <-chan provision.TermSize) error {
	f.mu.Lock()
	f.calls = append(f.calls, termCall{agent: a.Name, readOnly: readOnly})
	f.mu.Unlock()
	if f.refuse != nil {
		f.signal()
		return f.refuse
	}
	f.mu.Lock()
	f.active++
	f.mu.Unlock()
	defer func() {
		if f.release != nil {
			select {
			case f.releasing <- struct{}{}:
			default:
			}
			<-f.release
		}
		f.mu.Lock()
		f.active--
		f.ended++
		f.mu.Unlock()
		f.signal()
	}()
	if _, err := fmt.Fprintf(stdout, "attached ro=%t\r\n", readOnly); err != nil {
		return err
	}
	f.signal()
	if f.exitAfter {
		return nil
	}
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				f.mu.Lock()
				f.stdin = append(f.stdin, buf[:n]...)
				f.mu.Unlock()
				f.signal()
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case sz := <-resize:
			f.mu.Lock()
			f.sizes = append(f.sizes, sz)
			f.mu.Unlock()
			f.signal()
		}
	}
}

func (f *fakeTerminal) snapshot() (calls []termCall, stdin string, sizes []provision.TermSize, active, ended int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]termCall(nil), f.calls...), string(f.stdin), append([]provision.TermSize(nil), f.sizes...), f.active, f.ended
}

// waitTerm polls until ok holds, failing with what the fake saw.
func (f *fakeTerminal) wait(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !ok() {
		select {
		case <-f.event:
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			calls, stdin, sizes, active, ended := f.snapshot()
			t.Fatalf("timed out waiting for %s; calls=%v stdin=%q sizes=%v active=%d ended=%d",
				what, calls, stdin, sizes, active, ended)
		}
	}
}

func terminalServer(t *testing.T, prov Provisioner) (*Server, *httptest.Server) {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{
		Notes:           stubNotesStore{},
		Agents:          termAgents{},
		SessionLiveness: stubLiveness{},
		Provisioner:     prov,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func sessionCookieFor(s *Server) string {
	return sessionCookieName + "=" + auth.SignSession(s.auth.uiSessionSecret(), time.Now(), sessionTTL)
}

// dialTerm opens the terminal socket as a logged-in browser on the server's
// own origin.
func dialTerm(t *testing.T, s *Server, ts *httptest.Server, path string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	h := http.Header{}
	h.Set("Cookie", sessionCookieFor(s))
	h.Set("Origin", ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+path, &websocket.DialOptions{HTTPHeader: h})
}

// readUntil reads frames until a text control message of type want arrives,
// returning it and the terminal bytes seen before it.
func readUntil(t *testing.T, c *websocket.Conn, want string) (map[string]string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var screen strings.Builder
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for %q: %v (screen so far %q)", want, err, screen.String())
		}
		if typ == websocket.MessageBinary {
			screen.Write(data)
			continue
		}
		var m map[string]string
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("bad control frame %q: %v", data, err)
		}
		if m["type"] == want {
			return m, screen.String()
		}
		if m["type"] == "error" || m["type"] == "exit" {
			t.Fatalf("got %v while waiting for %q", m, want)
		}
	}
}

// TestTheTerminalSocketRefusesTheHookToken: the machine tier's bearer token,
// presented both ways it is accepted elsewhere, opens neither the page nor the
// socket — and nothing is attached. The session cookie is the control: the
// same request with it upgrades.
func TestTheTerminalSocketRefusesTheHookToken(t *testing.T) {
	ft := newFakeTerminal()
	s, ts := terminalServer(t, ft)
	for _, path := range []string{"/agents/keen-wren/terminal/ws", "/agents/keen-wren/terminal"} {
		for _, hdr := range [][2]string{{"Authorization", "Bearer " + testHookToken}, {"X-Muster-Token", testHookToken}} {
			req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
			req.Header.Set(hdr[0], hdr[1])
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
			req.Header.Set("Sec-WebSocket-Version", "13")
			req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s with %s: status %d, want 401", path, hdr[0], resp.StatusCode)
			}
		}
	}
	if calls, _, _, _, _ := ft.snapshot(); len(calls) != 0 {
		t.Fatalf("the hook token reached the terminal: %v", calls)
	}

	c, _, err := dialTerm(t, s, ts, "/agents/keen-wren/terminal/ws")
	if err != nil {
		t.Fatalf("control: a session cookie must open the socket: %v", err)
	}
	defer c.CloseNow()
	readUntil(t, c, "ready")
}

// TestTheTerminalSocketRefusesACrossSiteOrigin: a logged-in browser on another
// site cannot open the socket (cross-site WebSocket hijacking); the same
// cookie from the server's own origin can.
func TestTheTerminalSocketRefusesACrossSiteOrigin(t *testing.T) {
	ft := newFakeTerminal()
	s, ts := terminalServer(t, ft)
	h := http.Header{}
	h.Set("Cookie", sessionCookieFor(s))
	h.Set("Origin", "https://evil.example")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/agents/keen-wren/terminal/ws", &websocket.DialOptions{HTTPHeader: h})
	if err == nil {
		t.Fatal("a cross-site origin opened the terminal socket")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site origin: want 403, got %v (%v)", resp, err)
	}
	if calls, _, _, _, _ := ft.snapshot(); len(calls) != 0 {
		t.Fatalf("a cross-site socket reached the terminal: %v", calls)
	}
	c, _, err := dialTerm(t, s, ts, "/agents/keen-wren/terminal/ws")
	if err != nil {
		t.Fatalf("control: the server's own origin must open the socket: %v", err)
	}
	c.CloseNow()
}

// TestTheTerminalIsReadOnlyUnlessAskedAndDropsInput: no mode, and any mode
// that is not exactly "rw", attaches read-only AND forwards nothing typed;
// "rw" attaches interactive and forwards it. The resize sent after the keys is
// the ordering barrier: the read loop handles frames in order, so once the
// size has arrived the keys before it have been handled.
func TestTheTerminalIsReadOnlyUnlessAskedAndDropsInput(t *testing.T) {
	for _, tc := range []struct {
		query    string
		readOnly bool
	}{
		{"", true},
		{"?mode=ro", true},
		{"?mode=RW", true},
		{"?mode=rw%20", true},
		{"?mode=interactive", true},
		{"?mode=rw", false},
	} {
		t.Run("mode"+tc.query, func(t *testing.T) {
			ft := newFakeTerminal()
			s, ts := terminalServer(t, ft)
			c, _, err := dialTerm(t, s, ts, "/agents/keen-wren/terminal/ws"+tc.query)
			if err != nil {
				t.Fatal(err)
			}
			defer c.CloseNow()
			ready, screen := readUntil(t, c, "ready")
			wantMode := "rw"
			if tc.readOnly {
				wantMode = "ro"
			}
			if ready["mode"] != wantMode {
				t.Errorf("ready mode %q, want %q", ready["mode"], wantMode)
			}
			_ = screen
			ctx := context.Background()
			if err := c.Write(ctx, websocket.MessageBinary, []byte("typed\x03")); err != nil {
				t.Fatal(err)
			}
			if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":33,"rows":11}`)); err != nil {
				t.Fatal(err)
			}
			ft.wait(t, "the barrier resize", func() bool {
				_, _, sizes, _, _ := ft.snapshot()
				return len(sizes) > 0 && sizes[len(sizes)-1] == provision.TermSize{Cols: 33, Rows: 11}
			})
			if !tc.readOnly {
				ft.wait(t, "the typed bytes", func() bool { _, in, _, _, _ := ft.snapshot(); return in == "typed\x03" })
			}
			calls, stdin, _, _, _ := ft.snapshot()
			if len(calls) != 1 || calls[0].readOnly != tc.readOnly || calls[0].agent != "keen-wren" {
				t.Fatalf("calls %v, want one read-only=%t attach of keen-wren", calls, tc.readOnly)
			}
			if tc.readOnly && stdin != "" {
				t.Fatalf("a read-only terminal forwarded input: %q", stdin)
			}
		})
	}
}

// TestTheTerminalTakesItsSizeFromTheBrowser: the size in the URL is the first
// size the terminal gets (so tmux draws at the right width immediately),
// later resizes follow in order, and nonsense is clamped or ignored.
func TestTheTerminalTakesItsSizeFromTheBrowser(t *testing.T) {
	ft := newFakeTerminal()
	s, ts := terminalServer(t, ft)
	c, _, err := dialTerm(t, s, ts, "/agents/keen-wren/terminal/ws?cols=45&rows=29")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	readUntil(t, c, "ready")
	ctx := context.Background()
	for _, m := range []string{
		`{"type":"resize","cols":0,"rows":10}`,
		`{"type":"resize","cols":-4,"rows":10}`,
		`not json`,
		`{"type":"other","cols":9,"rows":9}`,
		`{"type":"resize","cols":5000,"rows":7}`,
		`{"type":"resize","cols":80,"rows":24}`,
	} {
		if err := c.Write(ctx, websocket.MessageText, []byte(m)); err != nil {
			t.Fatal(err)
		}
	}
	want := []provision.TermSize{{Cols: 45, Rows: 29}, {Cols: terminalMaxDim, Rows: 7}, {Cols: 80, Rows: 24}}
	ft.wait(t, "three sizes", func() bool { _, _, sizes, _, _ := ft.snapshot(); return len(sizes) >= len(want) })
	_, _, sizes, _, _ := ft.snapshot()
	if fmt.Sprint(sizes) != fmt.Sprint(want) {
		t.Fatalf("sizes %v, want %v", sizes, want)
	}
}

// TestTheTerminalRefusesAKindWithoutOne: a gateway agent has no session; the
// socket is refused before the upgrade with a status that says so, and the
// page is not served for it.
func TestTheTerminalRefusesAKindWithoutOne(t *testing.T) {
	ft := newFakeTerminal()
	s, ts := terminalServer(t, ft)
	_, resp, err := dialTerm(t, s, ts, "/agents/plain-otter/terminal/ws")
	if err == nil || resp == nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("gateway agent socket: want a 409 refusal, got %v (%v)", resp, err)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/agents/plain-otter/terminal", nil)
	req.Header.Set("Cookie", sessionCookieFor(s))
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("gateway agent page: %d, want 404", r.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/agents/keen-wren/terminal", nil)
	req.Header.Set("Cookie", sessionCookieFor(s))
	r, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != http.StatusOK || !strings.Contains(string(body), `data-agent="keen-wren"`) {
		t.Fatalf("control: the claude-code agent's page must render (%d)", r.StatusCode)
	}
	if calls, _, _, _, _ := ft.snapshot(); len(calls) != 0 {
		t.Fatalf("a refused kind reached the terminal: %v", calls)
	}
}

// TestATerminalRefusalReachesTheBrowserAsItsReason: a driver refusal (here the
// RBAC 403 the k8s driver produces) arrives as an error message carrying the
// remedy and the socket closes — no "ready" first, no hang.
func TestATerminalRefusalReachesTheBrowserAsItsReason(t *testing.T) {
	ft := newFakeTerminal()
	ft.refuse = fmt.Errorf("%w: the cluster refused to exec into keen-wren: forbidden — this is almost always RBAC: "+
		`grant muster's ClusterRole apiGroups [""] resources ["pods/exec"] verbs ["create"]`, provision.ErrUnsupported)
	s, ts := terminalServer(t, ft)
	c, _, err := dialTerm(t, s, ts, "/agents/keen-wren/terminal/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("no refusal arrived: %v", err)
	}
	var m map[string]string
	if typ != websocket.MessageText || json.Unmarshal(data, &m) != nil {
		t.Fatalf("first frame %q is not a control message", data)
	}
	if m["type"] != "error" || !strings.Contains(m["message"], `resources ["pods/exec"]`) {
		t.Fatalf("first message %v, want the error naming the pods/exec rule", m)
	}
	_, _, err = c.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusInternalError {
		t.Fatalf("after a refusal the socket must close with 1011, got %v", err)
	}
}

// TestATerminalWhoseClientExitsSaysSo: the attach returning on its own ends
// the socket with "exit" and a normal close.
func TestATerminalWhoseClientExitsSaysSo(t *testing.T) {
	ft := newFakeTerminal()
	ft.exitAfter = true
	s, ts := terminalServer(t, ft)
	c, _, err := dialTerm(t, s, ts, "/agents/keen-wren/terminal/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	_, screen := readUntil(t, c, "ready")
	_ = screen
	readUntil(t, c, "exit")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("want a normal close after exit, got %v", err)
	}
}

// TestClosingTheTerminalSocketEndsTheExecAndLeaksNothing: a browser that goes
// away — abruptly, as a killed tab or a lost phone does, and politely — ends
// the attach (its context is cancelled, which closes the exec stream; ending
// tmux's client in the pod is the adapter's release, tested in
// internal/agentprovision and against real tmux in cmd/ccd), and after
// repeated attach/close cycles the process holds no more goroutines than it
// did after one.
func TestClosingTheTerminalSocketEndsTheExecAndLeaksNothing(t *testing.T) {
	ft := newFakeTerminal()
	s, ts := terminalServer(t, ft)

	cycle := func(polite bool) {
		t.Helper()
		_, _, _, _, endedBefore := ft.snapshot()
		c, _, err := dialTerm(t, s, ts, "/agents/keen-wren/terminal/ws?mode=rw")
		if err != nil {
			t.Fatal(err)
		}
		readUntil(t, c, "ready")
		if polite {
			_ = c.Close(websocket.StatusNormalClosure, "tab closed")
		} else {
			_ = c.CloseNow()
		}
		ft.wait(t, "the attach to end after the socket closed", func() bool {
			_, _, _, active, ended := ft.snapshot()
			return active == 0 && ended == endedBefore+1
		})
	}

	cycle(false) // warm-up: the httptest server's own goroutines settle
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	baseline := runtime.NumGoroutine()
	for i := 0; i < 6; i++ {
		cycle(i%2 == 0)
	}
	var n int
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		runtime.GC()
		if n = runtime.NumGoroutine(); n <= baseline {
			return
		}
	}
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	fmt.Fprintf(os.Stderr, "%s\n", buf)
	t.Fatalf("goroutines grew from %d to %d over 6 attach/close cycles", baseline, n)
}

// terminalTestWatchdog bounds a wait for something that MUST happen. It is a
// hang detector, not a budget: nothing passes or fails by how long it took,
// only by whether it happened at all, so host load cannot turn it red.
const terminalTestWatchdog = 30 * time.Second

// terminalTestGrace is how long a test watches for something that must NOT
// happen yet. Load can only make the forbidden thing later, so this can fail
// only when the behaviour is really broken — never spuriously.
const terminalTestGrace = 50 * time.Millisecond

// awaitReleasing waits for one attach to reach its release: its context has
// ended and it is now held open until ft.release is closed.
func (f *fakeTerminal) awaitReleasing(t *testing.T) {
	t.Helper()
	select {
	case <-f.releasing:
	case <-time.After(terminalTestWatchdog):
		_, _, _, active, ended := f.snapshot()
		t.Fatalf("the terminal was never ended: its attach did not reach its release (active=%d ended=%d)", active, ended)
	}
}

// endTerminalsResult is what an EndTerminals call returned and what the fake
// looked like AT THAT MOMENT.
type endTerminalsResult struct {
	ok            bool
	active, ended int
}

// TestEndTerminalsEndsALiveTerminalAndWaitsForItsRelease: a shutdown cancels
// an open terminal — whose socket is hijacked, so http.Server.Shutdown would
// neither end nor wait for it — and returns only once its attach has returned,
// which is after the adapter's release. Then it refuses new terminals without
// attaching anything.
func TestEndTerminalsEndsALiveTerminalAndWaitsForItsRelease(t *testing.T) {
	ft := newFakeTerminal()
	ft.release = make(chan struct{})
	s, ts := terminalServer(t, ft)
	c, _, err := dialTerm(t, s, ts, "/agents/keen-wren/terminal/ws?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	readUntil(t, c, "ready")

	// An hour: this test is about waiting, so the budget must never be what
	// ends the call. The give-up path has its own test below.
	returned := make(chan endTerminalsResult, 1)
	go func() {
		ok := s.EndTerminals(time.Hour)
		_, _, _, active, ended := ft.snapshot()
		returned <- endTerminalsResult{ok: ok, active: active, ended: ended}
	}()
	// The attach sees its context end and then "releases" until ft.release is
	// closed. Reaching the release is the proof EndTerminals cancelled it.
	ft.awaitReleasing(t)
	// ...and EndTerminals must still be waiting.
	select {
	case <-returned:
		t.Fatal("EndTerminals returned while the terminal was still releasing")
	case <-time.After(terminalTestGrace):
	}
	close(ft.release)
	select {
	case got := <-returned:
		if !got.ok {
			t.Fatal("EndTerminals reported a timeout for a terminal that ended")
		}
		// Read the instant it returned, not after: an EndTerminals that
		// returns without waiting is caught here even if it was scheduled too
		// late for the grace window above.
		if got.active != 0 || got.ended != 1 {
			t.Fatalf("when EndTerminals returned: active=%d ended=%d, want 0 and 1 (it did not wait for the release)", got.active, got.ended)
		}
	case <-time.After(terminalTestWatchdog):
		t.Fatal("EndTerminals did not return after the terminal ended")
	}

	callsBefore, _, _, _, _ := ft.snapshot()
	c2, _, err := dialTerm(t, s, ts, "/agents/keen-wren/terminal/ws?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	defer c2.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), terminalTestWatchdog)
	defer cancel()
	if _, _, err := c2.Read(ctx); websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("a terminal opened during shutdown: want close %v, got %v", websocket.StatusGoingAway, err)
	}
	if calls, _, _, _, _ := ft.snapshot(); len(calls) != len(callsBefore) {
		t.Fatalf("a terminal opened during shutdown was attached: calls %v", calls)
	}
}

// TestEndTerminalsGivesUpAtItsTimeout: a terminal whose release never finishes
// does not hold the shutdown past its budget, and the result says so.
//
// The budget running out is driven by the test (terminalSet.endAll's giveUp),
// so "at its timeout" is asserted as an ORDER — still waiting before it, false
// right after it — rather than as a duration measured on a loaded host. The
// last step goes through EndTerminals itself, with a real timer, because the
// duration-to-timer wiring lives there and nowhere else: EndTerminals(1ms)
// must return false within 2s, which is what "not past its budget" means for
// the exported call (a wrapper that stretched the timeout would pass the
// order checks above).
func TestEndTerminalsGivesUpAtItsTimeout(t *testing.T) {
	ft := newFakeTerminal()
	ft.release = make(chan struct{})
	defer close(ft.release)
	s, ts := terminalServer(t, ft)
	c, _, err := dialTerm(t, s, ts, "/agents/keen-wren/terminal/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	readUntil(t, c, "ready")

	giveUp := make(chan time.Time)
	returned := make(chan bool, 1)
	go func() { returned <- s.terminals.endAll(s.logger, giveUp) }()
	ft.awaitReleasing(t)
	select {
	case ok := <-returned:
		t.Fatalf("EndTerminals returned %t before its timeout, for a terminal still releasing", ok)
	case <-time.After(terminalTestGrace):
	}
	select {
	case giveUp <- time.Now():
	case ok := <-returned:
		t.Fatalf("EndTerminals returned %t before its timeout, for a terminal still releasing", ok)
	case <-time.After(terminalTestWatchdog):
		t.Fatal("EndTerminals was not waiting on its timeout")
	}
	select {
	case ok := <-returned:
		if ok {
			t.Fatal("EndTerminals reported success for a terminal still releasing")
		}
	case <-time.After(terminalTestWatchdog):
		t.Fatal("EndTerminals did not give up at its timeout")
	}

	// The same terminal is still releasing: the exported call, on a real
	// timer, must also give up, say so, and do it PROMPTLY — the wrapper is
	// where the duration becomes a timer, so a budget it stretches is caught
	// only here. 2s against 1ms leaves three orders of magnitude for load.
	start := time.Now()
	go func() { returned <- s.EndTerminals(time.Millisecond) }()
	select {
	case ok := <-returned:
		if ok {
			t.Fatal("EndTerminals(1ms) reported success for a terminal still releasing")
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("EndTerminals(1ms) took %s: it held the shutdown past its budget", d)
		}
	case <-time.After(terminalTestWatchdog):
		t.Fatal("EndTerminals(1ms) did not give up: its timeout is not wired to the drain")
	}
}
