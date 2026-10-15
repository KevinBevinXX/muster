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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// MIRRORED SESSIONS through the real mux: the plugin off is invisible, a
// mirrored session is a card of its own (never an agent), its terminal attaches
// through the mirror — never the pod — and only for its owner.
// ---------------------------------------------------------------------------

const plainName = "plain-otter"

// mirrorAgents serves one gateway agent (and no chief).
type mirrorAgents struct{ stubAgentsStore }

func (mirrorAgents) rows() []agents.Agent {
	return []agents.Agent{{ID: 8, Name: plainName, Kind: agents.KindGateway, Namespace: "devpod-" + plainName, Status: agents.StatusStopped}}
}
func (m mirrorAgents) List(context.Context) ([]agents.Agent, error) { return m.rows(), nil }
func (m mirrorAgents) Get(_ context.Context, id int64) (agents.Agent, error) {
	for _, a := range m.rows() {
		if a.ID == id {
			return a, nil
		}
	}
	return agents.Agent{}, pgx.ErrNoRows
}
func (m mirrorAgents) GetByName(_ context.Context, name string) (agents.Agent, error) {
	for _, a := range m.rows() {
		if a.Name == name {
			return a, nil
		}
	}
	return agents.Agent{}, pgx.ErrNoRows
}
func (mirrorAgents) LastMessageByAgentIDs(context.Context, []int64) (map[int64]time.Time, error) {
	return map[int64]time.Time{}, nil
}

// mirrorCall is one Attach the fake received.
type mirrorCall struct {
	user     int64
	host     string
	session  string
	readOnly bool
}

// fakeMirror is the plugin, played. Its host "ws" (sessions main and chief)
// belongs to user 1 only: Hosts and Attach act for ctx's principal, as
// mirror.Service does.
type fakeMirror struct {
	mu          sync.Mutex
	online      bool
	interactive bool
	refuse      error
	calls       []mirrorCall
	stdin       []byte
	// v2 is the rendered view's half (mirror_view_test.go); nil = a v1 host.
	v2 *fakeMirrorV2
	// oldMirror: the host's muster-mirror predates Msg.PanePaste — it pastes
	// only into a Claude Code pane. The zero value is a current one.
	oldMirror bool
	// labels is the host's session labels (HostView.Labels); nil = none.
	labels map[string]string
	// activity is the host's per-session last-active times (HostView.Activity);
	// nil = none (an older muster-mirror).
	activity map[string]int64
	// windows, when set, makes "ws" a PER-WINDOW host listing exactly these
	// (mirror_perwindow_test.go); nil = a per-session host, as every test written
	// before windows were addressable expects.
	windows []mirror.Window
	// asked is the window id each AttachWindow / WatchWindow / PasteWindow was
	// given, in order ("" = the request named only the session).
	asked []string
	// others are further hosts listed AFTER "ws", as given (mirror_labels_test.go:
	// two hosts listing the same label); nil = "ws" alone.
	others []mirror.HostView
}

func (f *fakeMirror) ServeConnect(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusTeapot) // a status nothing else in the stack answers
}

func (f *fakeMirror) user(ctx context.Context) int64 {
	p, ok := authz.From(ctx)
	if !ok || p.All() {
		return 0
	}
	return p.UserID
}

func (f *fakeMirror) Hosts(ctx context.Context) []mirror.HostView {
	if f.user(ctx) != 1 {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	hv := mirror.HostView{Host: "ws", Sessions: []string{"chief", "main"}, Labels: f.labels, Activity: f.activity, Online: f.online, Interactive: f.interactive}
	if f.v2 != nil {
		hv.Rendered, hv.Claude = f.online, []string{"main"}
		hv.PanePaste = f.online && f.interactive && !f.oldMirror
	}
	if f.windows != nil {
		// A per-window host: its per-session view is derived from its windows,
		// exactly as the hub derives it.
		hv.Windows = mirror.NormalizeWindows(f.windows, 0)
		hv.Sessions, hv.Claude, hv.Labels, hv.Activity = mirror.SessionsOf(hv.Windows)
	}
	return append([]mirror.HostView{hv}, f.others...)
}

func (f *fakeMirror) AttachWindow(ctx context.Context, host, session, window string, readOnly bool, stdin io.Reader, stdout io.Writer, _ <-chan provision.TermSize) error {
	f.mu.Lock()
	f.asked = append(f.asked, window)
	f.calls = append(f.calls, mirrorCall{user: f.user(ctx), host: host, session: session, readOnly: readOnly})
	refuse := f.refuse
	f.mu.Unlock()
	if refuse != nil {
		return refuse
	}
	if _, err := fmt.Fprintf(stdout, "MIRROR ro=%t\r\n", readOnly); err != nil {
		return err
	}
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				f.mu.Lock()
				f.stdin = append(f.stdin, buf[:n]...)
				f.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	<-ctx.Done()
	return nil
}

func (f *fakeMirror) snapshot() ([]mirrorCall, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mirrorCall(nil), f.calls...), string(f.stdin)
}

// mirrorServer builds a server whose plugin is m (nil = off) and whose pod
// terminal is pod.
func mirrorServer(t *testing.T, m Mirror, pod *fakeTerminal) (*Server, *httptest.Server) {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{
		Notes:           stubNotesStore{},
		Agents:          mirrorAgents{},
		SessionLiveness: stubLiveness{},
		Provisioner:     pod,
		Mirror:          m,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func mirrorGet(t *testing.T, s *Server, ts *httptest.Server, path string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	req.Header.Set("Cookie", sessionCookieFor(s))
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

var mainPage = ui.MirrorTerminalPath("ws", "main")

// TestTheMirrorPluginOffIsInvisible: with Extensions.Mirror nil every mirror
// route 404s and the Agents view draws no mirrored card — and the SAME requests
// with the plugin on reach it (the control that makes each 404 the off-state).
func TestTheMirrorPluginOffIsInvisible(t *testing.T) {
	for _, on := range []bool{false, true} {
		var m Mirror
		if on {
			m = &fakeMirror{online: true}
		}
		s, ts := mirrorServer(t, m, newFakeTerminal())
		if resp, _ := mirrorGet(t, s, ts, mirror.ConnectPath); resp.StatusCode != map[bool]int{false: 404, true: 418}[on] {
			t.Errorf("on=%v: GET %s = %d", on, mirror.ConnectPath, resp.StatusCode)
		}
		if resp, _ := mirrorGet(t, s, ts, mainPage); resp.StatusCode != map[bool]int{false: 404, true: 200}[on] {
			t.Errorf("on=%v: the mirror terminal page = %d", on, resp.StatusCode)
		}
		_, list := mirrorGet(t, s, ts, "/ui/agents")
		if got := strings.Contains(list, "data-mirror-card"); got != on {
			t.Errorf("on=%v: a mirrored card is drawn = %v", on, got)
		}
		if !strings.Contains(list, plainName) {
			t.Errorf("on=%v: the gateway agent vanished from /ui/agents", on)
		}
	}
	s, ts := mirrorServer(t, nil, newFakeTerminal())
	if _, resp, err := dialTerm(t, s, ts, ui.MirrorTerminalWSPath("ws", "main")); err == nil || resp == nil || resp.StatusCode != 404 {
		t.Fatalf("plugin off: the mirror socket did not 404 (%v)", err)
	}
}

// mirrorCardOf is the mirrored card for session in the list markup.
func mirrorCardOf(t *testing.T, markup, session string) string {
	t.Helper()
	marker := `href="` + strings.ReplaceAll(ui.MirrorTerminalPath("ws", session), "&", "&amp;") + `"`
	i := strings.Index(markup, marker)
	if i < 0 {
		t.Fatalf("no mirrored card for %s (%s) in:\n%s", session, marker, markup)
	}
	start := strings.LastIndex(markup[:i], "<article")
	rest := markup[start:]
	if j := strings.Index(rest, "</article>"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// TestAMirroredSessionIsACardOfItsOwnNotAnAgent: badged "Mirrored · ws",
// "offline" exactly while the host is not serving it, a status dot from the
// host, a link to its terminal — and NO agent menu (nothing to start, stop or
// delete). The gateway card beside it is unchanged.
func TestAMirroredSessionIsACardOfItsOwnNotAnAgent(t *testing.T) {
	for _, online := range []bool{false, true} {
		s, ts := mirrorServer(t, &fakeMirror{online: online}, newFakeTerminal())
		_, list := mirrorGet(t, s, ts, "/ui/agents")
		card := mirrorCardOf(t, list, "main")
		if !strings.Contains(card, "data-mirror-badge") || !strings.Contains(card, "Mirrored · ws") {
			t.Errorf("online=%v: no Mirrored · ws badge:\n%s", online, card)
		}
		if got := strings.Contains(card, "data-mirror-offline"); got == online {
			t.Errorf("online=%v: offline badge drawn = %v", online, got)
		}
		want := map[bool]string{false: "stopped", true: "running"}[online]
		if !strings.Contains(card, `aria-label="Status: `+want+`"`) {
			t.Errorf("online=%v: status is not %s:\n%s", online, want, card)
		}
		for _, agentOnly := range []string{"Agent actions", "/start", "/stop", "hx-delete", "Rename agent"} {
			if strings.Contains(card, agentOnly) {
				t.Errorf("online=%v: a mirrored card carries the agent control %q", online, agentOnly)
			}
		}
		if !strings.Contains(list, `id="agent-8"`) || !strings.Contains(list, "/agents/8/start") {
			t.Errorf("control: the gateway card changed")
		}
	}
}

// TestTheTerminalPageOffersInteractiveOnlyWhenTheHostAdvertisedIt.
func TestTheTerminalPageOffersInteractiveOnlyWhenTheHostAdvertisedIt(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		s, ts := mirrorServer(t, &fakeMirror{online: true, interactive: interactive}, newFakeTerminal())
		_, page := mirrorGet(t, s, ts, mainPage)
		i := strings.Index(page, `id="term-mode"`)
		if i < 0 {
			t.Fatalf("no switch element:\n%s", page)
		}
		tag := page[strings.LastIndex(page[:i], "<"):]
		tag = tag[:strings.Index(tag, ">")]
		// The CLASS decides (an author display utility beats the [hidden]
		// attribute), so the class is what is asserted.
		offered := !strings.Contains(tag, `class="hidden `)
		if offered != interactive {
			t.Errorf("host interactive=%v: the Interactive switch is offered = %v (%s)", interactive, offered, tag)
		}
		if !strings.Contains(tag, `title="Live keys type into a tmux client on ws: from it you can switch to any tmux session there or open a shell on the host."`) {
			t.Errorf("the switch does not say that live keys are a shell on the host: %s", tag)
		}
		wantWS := strings.ReplaceAll(ui.MirrorTerminalWSPath("ws", "main"), "&", "&amp;")
		if !strings.Contains(page, `data-ws="`+wantWS+`"`) || !strings.Contains(page, `href="/agents"`) {
			t.Errorf("the page does not point at the mirror socket / back to the list")
		}
	}
}

// TestAMirroredTerminalAttachesThroughTheMirrorNeverThePod: the same socket
// bridge, read-only by default; mode=rw types through; the principal reaches
// the plugin; the pod's exec is never called.
func TestAMirroredTerminalAttachesThroughTheMirrorNeverThePod(t *testing.T) {
	fm := &fakeMirror{online: true, interactive: true}
	pod := newFakeTerminal()
	s, ts := mirrorServer(t, fm, pod)
	ws := ui.MirrorTerminalWSPath("ws", "main")

	c, _, err := dialTerm(t, s, ts, ws)
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := readUntil(t, c, "ready"); m["mode"] != "ro" {
		t.Fatalf("default mode = %q, want ro", m["mode"])
	}
	_ = c.Write(context.Background(), websocket.MessageBinary, []byte("nope"))
	c.Close(websocket.StatusNormalClosure, "")

	c2, _, err := dialTerm(t, s, ts, ws+"&mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := readUntil(t, c2, "ready"); m["mode"] != "rw" {
		t.Fatalf("mode=rw answered %q", m["mode"])
	}
	_ = c2.Write(context.Background(), websocket.MessageBinary, []byte("ls\r"))
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, typed := fm.snapshot(); typed == "ls\r" {
			break
		}
		if time.Now().After(deadline) {
			_, typed := fm.snapshot()
			t.Fatalf("interactive input did not reach the mirror; it has %q", typed)
		}
		time.Sleep(10 * time.Millisecond)
	}
	c2.Close(websocket.StatusNormalClosure, "")

	calls, _ := fm.snapshot()
	if len(calls) != 2 || calls[0] != (mirrorCall{1, "ws", "main", true}) || calls[1] != (mirrorCall{1, "ws", "main", false}) {
		t.Fatalf("mirror attach calls = %+v, want [ro, rw] for user 1 ws/main", calls)
	}
	if podCalls, _, _, _, _ := pod.snapshot(); len(podCalls) != 0 {
		t.Fatalf("a MIRRORED terminal reached the pod's exec: %+v", podCalls)
	}
}

// TestAnOfflineHostAnswersItsOwnReason: the plugin's refusal reaches the browser
// verbatim, not wrapped as a generic terminal error.
func TestAnOfflineHostAnswersItsOwnReason(t *testing.T) {
	fm := &fakeMirror{online: false, refuse: fmt.Errorf("%w (host %q)", mirror.ErrHostOffline, "ws")}
	s, ts := mirrorServer(t, fm, newFakeTerminal())
	c, _, err := dialTerm(t, s, ts, ui.MirrorTerminalWSPath("ws", "main"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("no error frame: %v", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var m map[string]string
		_ = json.Unmarshal(data, &m)
		if m["type"] != "error" {
			continue
		}
		if m["message"] != fm.refuse.Error() {
			t.Fatalf("the browser was told %q, want the plugin's own %q", m["message"], fm.refuse.Error())
		}
		return
	}
}

// TestAnotherUsersHostIsNotFound: the same requests as a principal that does
// not own the host get 404 — page and socket — and the plugin is never asked to
// attach. User 1 is the control.
func TestAnotherUsersHostIsNotFound(t *testing.T) {
	fm := &fakeMirror{online: true, interactive: true}
	s, _ := mirrorServer(t, fm, newFakeTerminal())
	for _, path := range []string{mainPage, ui.MirrorTerminalWSPath("ws", "main")} {
		for _, user := range []int64{2, 1} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req = req.WithContext(authz.WithPrincipal(req.Context(), authz.Principal{UserID: user, Role: authz.RoleMember, Via: authz.ViaSession}))
			rec := httptest.NewRecorder()
			if strings.Contains(path, "/ws") {
				s.handleMirrorTerminalWS(rec, req)
			} else {
				s.handleMirrorTerminalPage(rec, req)
			}
			if user == 2 && rec.Code != http.StatusNotFound {
				t.Errorf("user 2 %s = %d, want 404", path, rec.Code)
			}
			if user == 1 && rec.Code == http.StatusNotFound {
				t.Errorf("control: user 1 %s = 404", path)
			}
		}
	}
	if calls, _ := fm.snapshot(); len(calls) != 0 {
		t.Fatalf("the plugin was asked to attach for a non-owner: %+v", calls)
	}
}

// TestAMirroredSessionNamedChiefIsNotTheChief: a host listing a tmux session
// called "chief" changes nothing about the chief — mirrored sessions are not
// agents, so no agent lookup can see one.
func TestAMirroredSessionNamedChiefIsNotTheChief(t *testing.T) {
	fm := &fakeMirror{online: true}
	s, ts := mirrorServer(t, fm, newFakeTerminal())
	ctx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession})
	if hs := fm.Hosts(ctx); len(hs) != 1 || hs[0].Sessions[0] != "chief" {
		t.Fatalf("fixture: the host does not list a session named chief: %+v", hs)
	}
	if a, err := s.chiefAgent(ctx); !errors.Is(err, ErrNoChief) {
		t.Fatalf("chiefAgent found %+v (%v); a mirrored tmux session named chief must not be the chief", a, err)
	}
	if resp, _ := mirrorGet(t, s, ts, "/agents/chief"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/agents/chief = %d; a mirrored session must not resolve as an agent", resp.StatusCode)
	}
}
