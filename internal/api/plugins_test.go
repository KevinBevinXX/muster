package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
)

// countingPlugins is a plugins.Store that records which methods ran and as
// whom. It resolves one plugin token (pgnTok) to user 1.
type countingPlugins struct {
	plugins.Store
	mu    sync.Mutex
	calls []string
}

// Short aliases: the leak gate reads `Bearer "+<a long identifier>` as a
// credential-shaped string.
const (
	pgnTok    = "mpl_CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
	pgnSvc    = "plugins-test-service-token-0123456789abcd"
	pgnRunner = "mfr_DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"
	pgnAgent  = "plugins-test-agent-own-token-0123456789"
	pgnMsg    = `{"external_id":"ext-1","sender":"a@example.invalid","subject":"s","body":"hello"}`
)

func (c *countingPlugins) note(ctx context.Context, m string) {
	p, _ := authz.From(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, m+" as "+string(p.Via))
}

func (c *countingPlugins) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

func (c *countingPlugins) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = nil
}

func (c *countingPlugins) Authenticate(ctx context.Context, tok string) (plugins.Plugin, error) {
	c.note(ctx, "Authenticate")
	if tok == pgnTok {
		return plugins.Plugin{ID: 3, OwnerID: authz.OperatorUserID, Name: "example-inbox", Label: "Example inbox"}, nil
	}
	return plugins.Plugin{}, plugins.ErrBadToken
}

func (c *countingPlugins) Submit(ctx context.Context, _ int64, in plugins.MessageIn) (plugins.Message, bool, error) {
	c.note(ctx, "Submit")
	return plugins.Message{ID: 11, ExternalID: in.ExternalID, Status: plugins.StatusQueued}, true, nil
}

func (c *countingPlugins) List(ctx context.Context) ([]plugins.Plugin, error) {
	c.note(ctx, "List")
	return nil, nil
}

func (c *countingPlugins) QueuedCounts(ctx context.Context) (map[int64]int, error) {
	c.note(ctx, "QueuedCounts")
	return map[int64]int{}, nil
}

func (c *countingPlugins) DraftCards(ctx context.Context, _ int) ([]plugins.DraftCard, error) {
	c.note(ctx, "DraftCards")
	return nil, nil
}

func (c *countingPlugins) ClaimDraft(ctx context.Context, _ int64) (plugins.Claim, bool, error) {
	c.note(ctx, "ClaimDraft")
	return plugins.Claim{}, false, nil
}

func (c *countingPlugins) FinishDraft(ctx context.Context, _, _ int64, _ plugins.DraftResult) (plugins.Draft, error) {
	c.note(ctx, "FinishDraft")
	return plugins.Draft{}, plugins.ErrNotFound
}

// pluginsTestAgents resolves one agent by its own token (pgnAgent).
type pluginsTestAgents struct{ agents.Store }

func (pluginsTestAgents) GetByHooksToken(_ context.Context, tok string) (agents.Agent, error) {
	if tok == pgnAgent {
		return agents.Agent{ID: 7, Name: "plugins-test-agent", OwnerID: authz.OperatorUserID}, nil
	}
	return agents.Agent{}, http.ErrNoCookie
}

func newPluginsDoorServer(t *testing.T, store plugins.Store) (*Server, http.Handler, string) {
	t.Helper()
	tf := newFakeAPITokens()
	s := New(nil, AuthConfig{HookToken: tokTestHook, ServiceToken: pgnSvc, UIPassword: "plugins-test-operator-password"},
		log.New(io.Discard, "", 0))
	ext := Extensions{APITokens: tf, Users: stubUsers{}, Agents: pluginsTestAgents{}, Flows: principalFlows{}}
	if store != nil {
		ext.Plugins = store
	}
	s.UseExtensions(ext)
	mux := http.NewServeMux()
	s.registerAll(mux)
	return s, mux, tf.add(t, authz.OperatorUserID)
}

type pluginCred func(r *http.Request)

func pBearer(tok string) pluginCred {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

func pServe(h http.Handler, method, path, body string, c pluginCred) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "muster.test"
	if strings.HasPrefix(body, "{") {
		r.Header.Set("Content-Type", "application/json")
	} else if body != "" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	c(r)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// TestThePluginDoorRefusesEveryOtherCredential: POST /plugin/v1/messages takes a
// PLUGIN token and nothing else. The shared hook and service tokens, a personal
// token, a runner token, an agent's own token and a browser session are each
// refused 401 WITHOUT reaching the plugin store at all — not even its token
// lookup: the shape check refuses them first. The positive control is the
// plugin's own token reaching Submit, as the plugin door, and answering 202.
func TestThePluginDoorRefusesEveryOtherCredential(t *testing.T) {
	f := &countingPlugins{}
	s, h, personal := newPluginsDoorServer(t, f)
	foreign := map[string]pluginCred{
		"hook token (Bearer)":         pBearer(tokTestHook),
		"hook token (X-Muster-Token)": func(r *http.Request) { r.Header.Set("X-Muster-Token", tokTestHook) },
		"service token + actor": func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+pgnSvc)
			r.Header.Set(ActorHeader, "some-service")
		},
		"personal token":    pBearer(personal),
		"runner token":      pBearer(pgnRunner),
		"agent's own token": func(r *http.Request) { r.Header.Set("X-Muster-Token", pgnAgent) },
		"browser session": func(r *http.Request) {
			r.Header.Set("Cookie", sessionCookieFor(s))
			r.Header.Set("Origin", "http://"+r.Host)
		},
		"no credential":          func(*http.Request) {},
		"malformed plugin token": pBearer(pgnTok[:len(pgnTok)-1]),
	}
	for _, door := range pluginDoorRoutes {
		for name, c := range foreign {
			f.reset()
			rec := pServe(h, "POST", door.path, door.body, c)
			if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "this route takes a plugin token") {
				t.Errorf("%s %s: %d %q; want 401 with the plugin-token refusal", door.path, name, rec.Code, rec.Body.String())
			}
			if got := f.snapshot(); len(got) != 0 {
				t.Errorf("%s %s reached the plugin store: %v", door.path, name, got)
			}
		}
	}
	f.reset()
	rec := pServe(h, "POST", "/plugin/v1/messages", pgnMsg, pBearer(pgnTok))
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"status":"queued"`) {
		t.Fatalf("CONTROL: the plugin's own token: %d %q; want 202 queued", rec.Code, rec.Body.String())
	}
	got := f.snapshot()
	if len(got) != 2 || got[0] != "Authenticate as system" || got[1] != "Submit as plugin" {
		t.Fatalf("CONTROL: the plugin's own token made store calls %v; want [Authenticate as system, Submit as plugin]", got)
	}
	// CONTROL on the two draft routes: the plugin's own token reaches the store.
	for _, door := range pluginDoorRoutes[1:] {
		f.reset()
		rec := pServe(h, "POST", door.path, door.body, pBearer(pgnTok))
		if got := f.snapshot(); rec.Code != door.ownCode || len(got) < 2 || got[0] != "Authenticate as system" || got[len(got)-1] != door.reaches+" as plugin" {
			t.Errorf("CONTROL %s: %d, store calls %v; want %d reaching %s as the plugin", door.path, rec.Code, got, door.ownCode, door.reaches)
		}
	}
}

// pluginDoorRoutes is every /plugin/v1/* route with a body, the store method the
// plugin's own token reaches, and what the countingPlugins fake makes it answer.
var pluginDoorRoutes = []struct {
	path, body, reaches string
	ownCode             int
}{
	{"/plugin/v1/messages", pgnMsg, "Submit", http.StatusAccepted},
	{"/plugin/v1/drafts/claim", `{"wait_seconds":0}`, "ClaimDraft", http.StatusNoContent},
	{"/plugin/v1/drafts/5/result", `{"ok":true}`, "FinishDraft", http.StatusNotFound},
}

// TestAPluginTokenIsRefusedOnEveryOtherRoute: a plugin token opens nothing but
// the plugin door. (1) Every classified route, driven with ONLY a valid plugin
// token, hands no principal to its handler. (2) admit's own refusal — the one
// reachable when ANOTHER credential would admit the request — is driven on every
// session route: a real session cookie plus a plugin token is refused 401 with
// PluginTokenElsewhere, and no principal reaches the handler.
func TestAPluginTokenIsRefusedOnEveryOtherRoute(t *testing.T) {
	s, h, seen := newPrincipalTestServer(t)
	for via, routes := range routeVias {
		if via == authz.ViaPlugin {
			continue
		}
		for _, pattern := range routes {
			method, path := concretePath(pattern)
			*seen = nil
			req := httptest.NewRequest(method, path, nil)
			req.Header.Set("Authorization", "Bearer "+pPluginToken)
			if aborted, code := serve(t, h, req); aborted || len(*seen) != 0 {
				t.Errorf("%s with only a plugin token reached its handler as %v (status %d)", pattern, *seen, code)
			}
		}
	}
	reached := 0
	for _, pattern := range routeVias[authz.ViaSession] {
		method, path := concretePath(pattern)
		*seen = nil
		req := httptest.NewRequest(method, path, nil)
		credential(s, req, authz.ViaSession)
		req.Header.Set("Authorization", "Bearer "+pPluginToken)
		rec := httptest.NewRecorder()
		aborted := false
		func() {
			defer func() { aborted = recover() == errProbeAbort }() //nolint:errorlint
			h.ServeHTTP(rec, req)
		}()
		if aborted || len(*seen) != 0 {
			t.Errorf("%s: a session carrying a plugin token reached its handler as %v", pattern, *seen)
			continue
		}
		if rec.Code == http.StatusUnauthorized && strings.Contains(rec.Body.String(), "opens /plugin/v1/* only") {
			reached++
		}
	}
	// Every session route admits the cookie, so admit's refusal must be what
	// answered on each one; the plugin page routes are session routes too.
	if want := len(routeVias[authz.ViaSession]); reached != want {
		t.Fatalf("admit's plugin-token refusal answered on %d of %d session routes", reached, want)
	}
}

// TestPluginsOffIsNotFound: with no plugin store (MUSTER_PLUGINS unset) every
// plugin route answers 404 — whatever the credential, before any is examined —
// and the page is not drawn.
func TestPluginsOffIsNotFound(t *testing.T) {
	s, h, _ := newPluginsDoorServer(t, nil)
	session := func(r *http.Request) {
		r.Header.Set("Cookie", sessionCookieFor(s))
		r.Header.Set("Origin", "http://"+r.Host)
	}
	for _, rt := range []struct{ method, path, body string }{
		{"GET", "/plugins", ""}, {"POST", "/plugins", "name=example-inbox&label=x&data_scope=x"},
		{"POST", "/plugins/1/revoke", ""}, {"POST", "/plugin/v1/messages", pgnMsg},
		{"POST", "/plugins/drafts/1/approve", ""},
		{"POST", "/plugins/drafts/1/reject", ""},
		{"POST", "/plugin/v1/drafts/claim", `{"wait_seconds":0}`}, {"POST", "/plugin/v1/drafts/1/result", `{"ok":true}`},
	} {
		for name, c := range map[string]pluginCred{"plugin token": pBearer(pgnTok), "session": session,
			"hook token": pBearer(tokTestHook), "none": func(*http.Request) {}} {
			if rec := pServe(h, rt.method, rt.path, rt.body, c); rec.Code != http.StatusNotFound ||
				strings.Contains(rec.Body.String(), "plugins-page") {
				t.Errorf("plugins OFF: %s %s with %s: %d; want 404", rt.method, rt.path, name, rec.Code)
			}
		}
	}
	// CONTROL: the same server with a store wired answers the page.
	onS, on, _ := newPluginsDoorServer(t, &countingPlugins{})
	if rec := pServe(on, "GET", "/plugins", "", func(r *http.Request) { r.Header.Set("Cookie", sessionCookieFor(onS)) }); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `data-testid="plugins-page"`) {
		t.Fatalf("CONTROL: plugins ON, GET /plugins with a session: %d; want the page", rec.Code)
	}
}

// validatingPlugins is countingPlugins whose Submit applies the real per-field
// caps (MessageIn.Validate) and keeps the decoded body.
type validatingPlugins struct {
	countingPlugins
	body string
}

func (v *validatingPlugins) Submit(ctx context.Context, id int64, in plugins.MessageIn) (plugins.Message, bool, error) {
	if err := in.Validate(); err != nil {
		return plugins.Message{}, false, err
	}
	v.body = in.Body
	return v.countingPlugins.Submit(ctx, id, in)
}

// TestAValidMessageIsReadWhateverItsJSONEscaping (audit round 1): the route's
// READ cap must admit every message the per-field caps admit, however the plugin
// spelled it. JSON escapes a control character as six bytes, and encoding/json
// escapes `<` the same way, so a valid 16 KiB body arrives as up to 96 KiB; a
// 4-byte rune may arrive as a 12-byte surrogate pair. The decoded caps still
// refuse a body one byte over 16 KiB.
func TestAValidMessageIsReadWhateverItsJSONEscaping(t *testing.T) {
	store := &validatingPlugins{}
	_, h, _ := newPluginsDoorServer(t, store)
	marshal := func(in map[string]string) string {
		b, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	// Every field at its cap, every character at its widest spelling.
	// Escapes are built at run time so no tool between here and the file can decode them.
	esc := func(hex string) string { return string(rune(92)) + "u" + hex }
	surrogates := strings.Repeat(esc("d83d")+esc("de00"), plugins.MaxSenderRunes) // U+1F600 as a surrogate pair, 200 runes
	worst := `{"external_id":"` + strings.Repeat(esc("0041"), plugins.MaxExternalIDLen) +
		`","sender":"` + surrogates + `","subject":"` + surrogates +
		`","body":"` + strings.Repeat(esc("0001"), plugins.MaxBodyBytes) + `","received_at":"2000-01-02T03:04:05Z"}`
	for _, c := range []struct {
		name, body string
		want       int
		wantLen    int
	}{
		{"16 KiB of control characters", marshal(map[string]string{"external_id": "c-1", "body": strings.Repeat("\x01", plugins.MaxBodyBytes)}), http.StatusAccepted, plugins.MaxBodyBytes},
		{"16 KiB of <", marshal(map[string]string{"external_id": "c-2", "body": strings.Repeat("<", plugins.MaxBodyBytes)}), http.StatusAccepted, plugins.MaxBodyBytes},
		{"every field at its widest", worst, http.StatusAccepted, plugins.MaxBodyBytes},
		{"one byte over the body cap", marshal(map[string]string{"external_id": "c-3", "body": strings.Repeat("a", plugins.MaxBodyBytes+1)}), http.StatusBadRequest, 0},
		{"over the read cap", `{"external_id":"c-4","body":"` + strings.Repeat(" ", plugins.MaxRequestBytes) + `"}`, http.StatusBadRequest, 0},
	} {
		store.body = ""
		rec := pServe(h, "POST", "/plugin/v1/messages", c.body, pBearer(pgnTok))
		if rec.Code != c.want || len(store.body) != c.wantLen {
			t.Errorf("%s (%d bytes on the wire): %d %s, stored %d bytes; want %d, %d bytes",
				c.name, len(c.body), rec.Code, rec.Body.String(), len(store.body), c.want, c.wantLen)
		}
	}
	if len(worst) > plugins.MaxRequestBytes {
		t.Fatalf("the worst-case fixture is %d bytes, over the %d read cap it is meant to fit", len(worst), plugins.MaxRequestBytes)
	}
}

// resultPlugins is countingPlugins whose FinishDraft applies the real result
// caps (DraftResult.Validate) and keeps the decoded detail.
type resultPlugins struct {
	countingPlugins
	detail string
}

func (v *resultPlugins) FinishDraft(_ context.Context, _, id int64, r plugins.DraftResult) (plugins.Draft, error) {
	if err := r.Validate(); err != nil {
		return plugins.Draft{}, err
	}
	v.detail = r.Detail
	return plugins.Draft{ID: id, Status: plugins.DraftSent}, nil
}

// TestAValidDraftResultIsReadWhateverItsJSONEscaping (audit round 1): the result
// route's READ cap must admit every result the decoded caps admit. A detail of
// 4,096 '<' is valid, and encoding/json sends it as 24,576 bytes; the old cap
// (3x the detail cap) refused it. external_ref is capped in runes, and a 4-byte
// rune may arrive as a 12-byte surrogate pair. The decoded caps still refuse a
// detail one byte over.
func TestAValidDraftResultIsReadWhateverItsJSONEscaping(t *testing.T) {
	store := &resultPlugins{}
	_, h, _ := newPluginsDoorServer(t, store)
	marshal := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	// Built at run time so no tool between here and the file can decode them.
	esc := func(hex string) string { return string(rune(92)) + "u" + hex }
	worst := `{"ok":true,"external_ref":"` + strings.Repeat(esc("d83d")+esc("de00"), plugins.MaxExternalRefRunes) +
		`","detail":"` + strings.Repeat(esc("0001"), plugins.MaxDetailBytes) + `"}`
	for _, c := range []struct {
		name, body string
		want, n    int
	}{
		{"4,096 '<' via encoding/json", marshal(map[string]any{"ok": true, "detail": strings.Repeat("<", plugins.MaxDetailBytes)}), http.StatusOK, plugins.MaxDetailBytes},
		{"every field at its widest", worst, http.StatusOK, plugins.MaxDetailBytes},
		{"one byte over the detail cap", marshal(map[string]any{"ok": true, "detail": strings.Repeat("a", plugins.MaxDetailBytes+1)}), http.StatusBadRequest, 0},
		{"over the read cap", `{"ok":true,"detail":"` + strings.Repeat(" ", plugins.MaxResultRequestBytes) + `"}`, http.StatusBadRequest, 0},
	} {
		store.detail = ""
		rec := pServe(h, "POST", "/plugin/v1/drafts/5/result", c.body, pBearer(pgnTok))
		if rec.Code != c.want || len(store.detail) != c.n {
			t.Errorf("%s (%d bytes on the wire): %d %s, kept %d bytes; want %d, %d bytes",
				c.name, len(c.body), rec.Code, rec.Body.String(), len(store.detail), c.want, c.n)
		}
	}
	if len(worst) > plugins.MaxResultRequestBytes {
		t.Fatalf("the worst-case fixture is %d bytes, over the %d read cap it is meant to fit", len(worst), plugins.MaxResultRequestBytes)
	}
}
