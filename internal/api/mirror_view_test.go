package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE RENDERED VIEW through the real mux: a Claude pane's card opens it, the
// page and socket are the owner's only, batches arrive rendered with the chat's
// own components, and a send is gated on an interactive host AND a fresh
// step-up before the plugin is ever asked.
// ---------------------------------------------------------------------------

type watchCall struct {
	user          int64
	host, session string
	gen           string
	offset        int64
}

type pasteCall struct {
	user                int64
	host, session, text string
}

// fakeMirrorV2 is the plugin's rendered-view half, played: Watch serves
// batches, then blocks until the socket ends.
type fakeMirrorV2 struct {
	mu       sync.Mutex
	batches  []mirror.Msg
	watchErr error
	pasteErr error
	watches  []watchCall
	pastes   []pasteCall
}

func (f *fakeMirror) Watch(ctx context.Context, host, session, gen string, offset int64, out func(mirror.Msg) error) error {
	if f.v2 == nil {
		return mirror.ErrNoRenderedView
	}
	f.v2.mu.Lock()
	f.v2.watches = append(f.v2.watches, watchCall{f.user(ctx), host, session, gen, offset})
	batches, werr := append([]mirror.Msg(nil), f.v2.batches...), f.v2.watchErr
	f.v2.mu.Unlock()
	if f.user(ctx) != 1 {
		return mirror.ErrHostOffline
	}
	for _, b := range batches {
		if err := out(b); err != nil {
			return err
		}
	}
	if werr != nil {
		return werr
	}
	<-ctx.Done()
	return nil
}

func (f *fakeMirror) Paste(ctx context.Context, host, session, text string) error {
	if f.v2 == nil {
		return mirror.ErrNoRenderedView
	}
	f.v2.mu.Lock()
	defer f.v2.mu.Unlock()
	f.v2.pastes = append(f.v2.pastes, pasteCall{f.user(ctx), host, session, text})
	if f.user(ctx) != 1 {
		return mirror.ErrHostOffline
	}
	return f.v2.pasteErr
}

func (f *fakeMirrorV2) snapshot() ([]watchCall, []pasteCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]watchCall(nil), f.watches...), append([]pasteCall(nil), f.pastes...)
}

// stepUp is the shared gate, played.
type stepUp bool

func (s stepUp) StepUpFresh(*http.Request) bool { return bool(s) }

func viewServer(t *testing.T, fm *fakeMirror, gate StepUpGate) (*Server, *httptest.Server) {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	ext := Extensions{Notes: stubNotesStore{}, Agents: mirrorAgents{}, SessionLiveness: stubLiveness{},
		Provisioner: newFakeTerminal(), MirrorStepUp: gate}
	if fm != nil {
		ext.Mirror = fm
	}
	s.UseExtensions(ext)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func amp(s string) string { return strings.ReplaceAll(s, "&", "&amp;") }

func TestAClaudePanesCardOpensTheRenderedViewAndOthersTheTerminal(t *testing.T) {
	for _, v2 := range []bool{true, false} {
		fm := &fakeMirror{online: true}
		if v2 {
			fm.v2 = &fakeMirrorV2{}
		}
		s, ts := viewServer(t, fm, nil)
		_, list := mirrorGet(t, s, ts, "/ui/agents")
		wantMain := ui.MirrorTerminalPath("ws", "main")
		if v2 {
			wantMain = ui.MirrorViewPath("ws", "main")
		}
		if !strings.Contains(list, `href="`+amp(wantMain)+`"`) {
			t.Errorf("v2=%v: main's card does not open %s:\n%s", v2, wantMain, list)
		}
		if !strings.Contains(list, `href="`+amp(ui.MirrorTerminalPath("ws", "chief"))+`"`) {
			t.Errorf("v2=%v: a non-Claude session's card does not open its terminal", v2)
		}
		if got := strings.Count(list, "data-mirror-claude"); got != map[bool]int{true: 1, false: 0}[v2] {
			t.Errorf("v2=%v: %d Claude Code badges", v2, got)
		}
	}
}

func TestTheRenderedViewPageIsTheOwnersAndFallsBackToTheTerminal(t *testing.T) {
	viewMain := ui.MirrorViewPath("ws", "main")
	for _, c := range []struct {
		name     string
		fm       *fakeMirror
		path     string
		status   int
		location string
	}{
		{"a Claude pane", &fakeMirror{online: true, v2: &fakeMirrorV2{}}, viewMain, 200, ""},
		{"a non-Claude session", &fakeMirror{online: true, v2: &fakeMirrorV2{}}, ui.MirrorViewPath("ws", "chief"), 303, ui.MirrorTerminalPath("ws", "chief")},
		{"a v1 host", &fakeMirror{online: true}, viewMain, 303, ui.MirrorTerminalPath("ws", "main")},
		{"an offline host", &fakeMirror{online: false, v2: &fakeMirrorV2{}}, viewMain, 303, ui.MirrorTerminalPath("ws", "main")},
		{"an unknown host", &fakeMirror{online: true, v2: &fakeMirrorV2{}}, ui.MirrorViewPath("nope", "main"), 404, ""},
		{"the plugin off", nil, viewMain, 404, ""},
	} {
		s, ts := viewServer(t, c.fm, nil)
		resp, body := mirrorGet(t, s, ts, c.path)
		if resp.StatusCode != c.status || resp.Header.Get("Location") != c.location {
			t.Errorf("%s: %d → %q, want %d → %q", c.name, resp.StatusCode, resp.Header.Get("Location"), c.status, c.location)
		}
		if c.status == 200 {
			for _, want := range []string{`id="mv-log"`, `data-ws="` + amp(ui.MirrorViewWSPath("ws", "main")) + `"`,
				`id="view-terminal" href="` + amp(ui.MirrorTerminalPath("ws", "main")) + `"`, `id="view-rendered" aria-current="page"`} {
				if !strings.Contains(body, want) {
					t.Errorf("%s: the page lacks %s", c.name, want)
				}
			}
		}
	}
	// Another user: 404, page and socket, and the plugin is never asked.
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}}
	s, _ := viewServer(t, fm, stepUp(true))
	for _, user := range []int64{2, 1} {
		for _, path := range []string{viewMain, ui.MirrorViewWSPath("ws", "main")} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req = req.WithContext(authz.WithPrincipal(req.Context(), authz.Principal{UserID: user, Role: authz.RoleMember, Via: authz.ViaSession}))
			rec := httptest.NewRecorder()
			if strings.Contains(path, "/ws") {
				s.handleMirrorViewWS(rec, req)
			} else {
				s.handleMirrorViewPage(rec, req)
			}
			if (user == 2) != (rec.Code == http.StatusNotFound) {
				t.Errorf("user %d %s = %d", user, path, rec.Code)
			}
		}
	}
	if w, _ := fm.v2.snapshot(); len(w) != 0 {
		t.Fatalf("the plugin was asked to watch for a non-owner: %+v", w)
	}
}

var disabledAttr = regexp.MustCompile(`\sdisabled(=""|\s|$)`)

func TestTheComposerIsDisabledOnAReadOnlyHost(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		s, ts := viewServer(t, &fakeMirror{online: true, interactive: interactive, v2: &fakeMirrorV2{}}, nil)
		_, page := mirrorGet(t, s, ts, ui.MirrorViewPath("ws", "main"))
		i := strings.Index(page, `id="mv-input"`)
		tag := page[strings.LastIndex(page[:i], "<"):]
		tag = tag[:strings.Index(tag, ">")]
		// The ATTRIBUTE, not the `disabled:` variant in the class list.
		if disabled := disabledAttr.MatchString(tag); disabled == interactive {
			t.Errorf("interactive=%v: composer disabled = %v (%s)", interactive, disabled, tag)
		}
	}
}

func TestTheTerminalOffersTheRenderedViewForAClaudePane(t *testing.T) {
	s, ts := viewServer(t, &fakeMirror{online: true, v2: &fakeMirrorV2{}}, nil)
	_, main := mirrorGet(t, s, ts, ui.MirrorTerminalPath("ws", "main"))
	if !strings.Contains(main, `id="view-rendered" href="`+amp(ui.MirrorViewPath("ws", "main"))+`"`) || !strings.Contains(main, `id="view-terminal" aria-current="page"`) {
		t.Errorf("main's terminal has no Rendered switch")
	}
	_, chief := mirrorGet(t, s, ts, ui.MirrorTerminalPath("ws", "chief"))
	if strings.Contains(chief, `id="view-rendered"`) {
		t.Errorf("a non-Claude session's terminal offers a rendered view")
	}
}

func readBatch(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestTheViewSocketRendersBatchesWithTheChatsComponents(t *testing.T) {
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{batches: []mirror.Msg{
		{Type: mirror.TypeEvents, Reset: true, Gen: "g1", Offset: 100, Events: []cctranscript.Event{
			{Kind: cctranscript.KindPrompt, Text: "run the **tests** <script>alert(1)</script>"},
			{Kind: cctranscript.KindText, Text: "Running them."},
			{Kind: cctranscript.KindToolCall, ToolID: "tu1", Tool: "Bash", Target: "go test ./...", Input: `{"command":"go test ./..."}`},
		}},
		{Type: mirror.TypeEvents, Gen: "g1", Offset: 200, Busy: true, Events: []cctranscript.Event{
			{Kind: cctranscript.KindToolResult, ToolID: "tu1", OK: false, Output: "FAIL"},
		}},
	}}}
	s, ts := viewServer(t, fm, nil)
	c, _, err := dialTerm(t, s, ts, ui.MirrorViewWSPath("ws", "main")+"&gen=g0&offset=42")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()

	first := readBatch(t, c)
	if first["type"] != "batch" || first["reset"] != true || first["gen"] != "g1" || first["offset"] != float64(100) {
		t.Fatalf("first batch %v", first)
	}
	ops := first["ops"].([]any)
	if len(ops) != 3 {
		t.Fatalf("%d ops", len(ops))
	}
	html := func(o any) string { return o.(map[string]any)["html"].(string) }
	if h := html(ops[0]); !strings.Contains(h, `data-bubble="user"`) || !strings.Contains(h, "<strong>tests</strong>") || strings.Contains(h, "<script>") {
		t.Errorf("the prompt bubble is not escaped markdown: %s", h)
	}
	row := html(ops[2])
	for _, want := range []string{`data-tool-row`, `data-tool-status="running"`, `data-tool-target`, "go test ./...", ">Bash<", "<details"} {
		if !strings.Contains(row, want) {
			t.Errorf("the running tool row lacks %s: %s", want, row)
		}
	}
	id := ops[2].(map[string]any)["id"].(string)
	second := readBatch(t, c)
	op := second["ops"].([]any)[0].(map[string]any)
	if op["op"] != "replace" || op["id"] != id || !strings.Contains(op["html"].(string), `data-tool-status="error"`) ||
		!strings.Contains(op["html"].(string), "go test ./...") || second["busy"] != true {
		t.Fatalf("the result did not redraw its call's row: %v", second)
	}
	w, _ := fm.v2.snapshot()
	if len(w) != 1 || w[0] != (watchCall{1, "ws", "main", "g0", 42}) {
		t.Fatalf("watch calls %+v, want the resume point passed through", w)
	}
}

func TestTheViewSocketPassesTheRefusalThrough(t *testing.T) {
	refusal := fmt.Errorf("%w (host %q, session %q)", mirror.ErrNotClaude, "ws", "main")
	s, ts := viewServer(t, &fakeMirror{online: true, v2: &fakeMirrorV2{watchErr: refusal}}, nil)
	c, _, err := dialTerm(t, s, ts, ui.MirrorViewWSPath("ws", "main"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if m := readBatch(t, c); m["type"] != "error" || m["message"] != refusal.Error() {
		t.Fatalf("got %v, want the plugin's own refusal", m)
	}
}

// 🔴 THE SEND GATE: same origin, an interactive host, a FRESH step-up — each
// refused before the plugin is asked; and only then is the text pasted.
func TestASendIsGatedOnInteractiveAndAFreshStepUp(t *testing.T) {
	type tc struct {
		name        string
		interactive bool
		gate        StepUpGate
		origin      string
		status      int
		pasted      bool
	}
	cases := []tc{
		{"a read-only host", false, stepUp(true), "", http.StatusConflict, false},
		{"no step-up gate wired (fail closed)", true, nil, "", http.StatusForbidden, false},
		{"a stale step-up", true, stepUp(false), "", http.StatusForbidden, false},
		{"another origin", true, stepUp(true), "https://evil.example", http.StatusForbidden, false},
		{"interactive + fresh step-up", true, stepUp(true), "", http.StatusNoContent, true},
	}
	for _, c := range cases {
		fm := &fakeMirror{online: true, interactive: c.interactive, v2: &fakeMirrorV2{}}
		s, ts := viewServer(t, fm, c.gate)
		form := url.Values{"host": {"ws"}, "session": {"main"}, "text": {"hello claude"}}
		req, _ := http.NewRequest(http.MethodPost, ts.URL+ui.MirrorSendPath, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Cookie", sessionCookieFor(s))
		origin := ts.URL
		if c.origin != "" {
			origin = c.origin
		}
		req.Header.Set("Origin", origin)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		_, pastes := fm.v2.snapshot()
		if resp.StatusCode != c.status || (len(pastes) == 1) != c.pasted {
			t.Errorf("%s: %d %q, pasted %+v; want %d pasted=%v", c.name, resp.StatusCode, body, pastes, c.status, c.pasted)
		}
		if c.pasted && pastes[0] != (pasteCall{1, "ws", "main", "hello claude"}) {
			t.Errorf("%s: pasted %+v", c.name, pastes[0])
		}
		if c.status == http.StatusForbidden && c.origin == "" && resp.Header.Get("X-Muster-Step-Up") != "required" {
			t.Errorf("%s: the step-up refusal does not say so", c.name)
		}
	}
	// Another user's host: 404, never pasted.
	fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}}
	s, _ := viewServer(t, fm, stepUp(true))
	form := url.Values{"host": {"ws"}, "session": {"main"}, "text": {"x"}}
	req := httptest.NewRequest(http.MethodPost, ui.MirrorSendPath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(authz.WithPrincipal(req.Context(), authz.Principal{UserID: 2, Role: authz.RoleMember, Via: authz.ViaSession}))
	rec := httptest.NewRecorder()
	s.handleMirrorViewSend(rec, req)
	if _, pastes := fm.v2.snapshot(); rec.Code != http.StatusNotFound || len(pastes) != 0 {
		t.Fatalf("user 2's send: %d, pasted %+v", rec.Code, pastes)
	}
}

// isoMirror (the isolation walk's plugin) has no rendered view: A's host is a
// v1 host there, and a view route as B must still 404 before the plugin.
func (m *isoMirror) Watch(ctx context.Context, host, _, _ string, _ int64, _ func(mirror.Msg) error) error {
	if !m.mine(ctx) || host != m.host {
		return mirror.ErrHostOffline
	}
	return mirror.ErrNoRenderedView
}

func (m *isoMirror) Paste(ctx context.Context, host, _, _ string) error {
	if !m.mine(ctx) || host != m.host {
		return mirror.ErrHostOffline
	}
	return mirror.ErrNoRenderedView
}
