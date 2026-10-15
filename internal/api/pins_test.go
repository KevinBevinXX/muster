package api

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/pins"
)

// memPins is pins.Store in memory, per principal user — the PG store's
// row-level security is pinned by internal/pins' own Postgres tests.
type memPins struct {
	mu   sync.Mutex
	sets map[int64]*pins.Set
	// agents is what PinAgent accepts (nil = every id).
	agents map[int64]bool
}

func (m *memPins) set(ctx context.Context) *pins.Set {
	uid := int64(0)
	if p, ok := authz.From(ctx); ok {
		uid = p.UserID
	}
	if m.sets == nil {
		m.sets = map[int64]*pins.Set{}
	}
	s := m.sets[uid]
	if s == nil {
		s = &pins.Set{Agents: map[int64]bool{}, Mirrors: map[pins.MirrorKey]bool{}}
		m.sets[uid] = s
	}
	return s
}

func (m *memPins) List(ctx context.Context) (pins.Set, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.set(ctx)
	out := pins.Set{Agents: map[int64]bool{}, Mirrors: map[pins.MirrorKey]bool{}}
	for k, v := range s.Agents {
		out.Agents[k] = v
	}
	for k, v := range s.Mirrors {
		out.Mirrors[k] = v
	}
	return out, nil
}

func (m *memPins) PinAgent(ctx context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.agents != nil && !m.agents[id] {
		return pins.ErrNoSuchAgent
	}
	m.set(ctx).Agents[id] = true
	return nil
}

func (m *memPins) UnpinAgent(ctx context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.set(ctx).Agents, id)
	return nil
}

func (m *memPins) PinMirror(ctx context.Context, k pins.MirrorKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.set(ctx).Mirrors[k] = true
	return nil
}

func (m *memPins) UnpinMirror(ctx context.Context, k pins.MirrorKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.set(ctx).Mirrors, k)
	return nil
}

func (m *memPins) pinFor(uid int64) *pins.Set {
	return m.set(authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: authz.RoleAdmin, Via: authz.ViaSession}))
}

// pinWindows is a per-window host "ws": two windows, main:@1 the most recently
// active and chief:@2 the least. Activity times are pairwise distinct and
// distinct from the agents' (activityFixture).
func pinWindows() []mirror.Window {
	return []mirror.Window{
		{Session: "main", ID: "@1", Index: 1, Active: true, Activity: 1_800_009_000},
		{Session: "chief", ID: "@2", Index: 1, Active: true, Activity: 1_800_001_000},
	}
}

func pinServer(t *testing.T, fm *fakeMirror, mp *memPins) (*Server, http.Handler) {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	ext := Extensions{Agents: activityFixture(), SessionLiveness: stubLiveness{}}
	if fm != nil {
		ext.Mirror = fm
	}
	if mp != nil {
		ext.Pins = mp
	}
	s.UseExtensions(ext)
	return s, s.Handler()
}

var listArticle = regexp.MustCompile(`<article id="([^"]+)"`)

// listOrder reads the rendered list's card order: "agent-<id>" for an agent
// card, "<session><window>" ("chief@2") for a mirrored one.
func listOrder(t *testing.T, body string) []string {
	t.Helper()
	locs := listArticle.FindAllStringSubmatchIndex(body, -1)
	var out []string
	for i, l := range locs {
		end := len(body)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		id, chunk := body[l[2]:l[3]], body[l[0]:end]
		if strings.HasPrefix(id, "agent-") {
			out = append(out, id)
			continue
		}
		m := regexp.MustCompile(`session=([a-z]+)&amp;window=%40([0-9]+)`).FindStringSubmatch(chunk)
		if m == nil {
			t.Fatalf("instrument check FAILED: card %s names no window:\n%s", id, chunk)
		}
		out = append(out, m[1]+"@"+m[2])
	}
	return out
}

func pinnedList(t *testing.T, s *Server, h http.Handler) (string, []string) {
	t.Helper()
	body := getOwed(t, s, h, "/ui/agents").Body.String()
	return body, listOrder(t, body)
}

// TestPinnedCardsRenderFirstInTheirOriginalOrder: the operator ask "pinned
// agents appear first in the list". The pinned entries are the LEAST recently
// active of each kind (agent 7, window chief@2), so pinned-first cannot pass by
// accident of the activity order; the rest keep that order. Red before pins
// existed: the list was 5,6,7 then main@1, chief@2 whatever was pinned.
func TestPinnedCardsRenderFirstInTheirOriginalOrder(t *testing.T) {
	fm := &fakeMirror{online: true, windows: pinWindows()}
	mp := &memPins{}
	s, h := pinServer(t, fm, mp)

	// Control: nothing pinned — the order the list always had.
	_, got := pinnedList(t, s, h)
	if want := "agent-5,agent-6,agent-7,main@1,chief@2"; strings.Join(got, ",") != want {
		t.Fatalf("unpinned order = %s, want %s (the instrument or the fixture moved)", strings.Join(got, ","), want)
	}

	set := mp.pinFor(authz.OperatorUserID)
	set.Agents[7] = true
	set.Mirrors[pins.MirrorKey{Host: "ws", Session: "chief", Window: "@2"}] = true
	body, got := pinnedList(t, s, h)
	if want := "agent-7,chief@2,agent-5,agent-6,main@1"; strings.Join(got, ",") != want {
		t.Fatalf("pinned order = %s, want %s: the pinned agent and the pinned window first (each "+
			"kind in its existing order), then the unpinned ones in theirs", strings.Join(got, ","), want)
	}
	// Two pinned agents keep THEIR activity order between them (a stable
	// partition, not a re-sort): 6 before 7.
	set.Agents[6] = true
	_, got = pinnedList(t, s, h)
	if want := "agent-6,agent-7,chief@2,agent-5,main@1"; strings.Join(got, ",") != want {
		t.Fatalf("two pinned agents = %s, want %s", strings.Join(got, ","), want)
	}

	// The pinned cards say so, and offer Unpin; the others offer Pin.
	if n := strings.Count(body, `data-pinned-glyph`); n != 2 {
		t.Errorf("%d pinned glyphs, want 2 (one per pinned card)", n)
	}
	card7 := body[strings.Index(body, `<article id="agent-7"`):strings.Index(body, `<article id="mirror-`)]
	if !strings.Contains(card7, `action="/agents/pins/delete"`) || !strings.Contains(card7, ">Unpin<") {
		t.Errorf("the pinned agent's menu does not offer Unpin:\n%s", card7)
	}
	card5 := body[strings.Index(body, `<article id="agent-5"`):strings.Index(body, `<article id="agent-6"`)]
	if !strings.Contains(card5, `action="/agents/pins"`) || !strings.Contains(card5, ">Pin to top<") {
		t.Errorf("an unpinned agent's menu does not offer Pin to top:\n%s", card5)
	}
	if !strings.Contains(body, `aria-pressed="true"`) || !strings.Contains(body, `aria-pressed="false"`) {
		t.Errorf("the mirrored cards' pin toggles are not pressed/unpressed by pin state")
	}
}

// TestADormantMirrorPinReappearsWithItsWindow: a pin whose window is not
// listed draws nothing and changes nothing; when the window is listed again,
// it is pinned again — first.
func TestADormantMirrorPinReappearsWithItsWindow(t *testing.T) {
	fm := &fakeMirror{online: true, windows: pinWindows()}
	mp := &memPins{}
	s, h := pinServer(t, fm, mp)
	mp.pinFor(authz.OperatorUserID).Mirrors[pins.MirrorKey{Host: "ws", Session: "chief", Window: "@9"}] = true

	body, got := pinnedList(t, s, h)
	if want := "agent-5,agent-6,agent-7,main@1,chief@2"; strings.Join(got, ",") != want {
		t.Fatalf("with a dormant pin the list = %s, want the unpinned %s", strings.Join(got, ","), want)
	}
	if strings.Contains(body, "data-pinned") {
		t.Fatalf("a dormant pin drew something pinned")
	}

	fm.mu.Lock()
	fm.windows = append(pinWindows(), mirror.Window{Session: "chief", ID: "@9", Index: 2, Activity: 1_800_000_100})
	fm.mu.Unlock()
	_, got = pinnedList(t, s, h)
	if want := "chief@9,agent-5,agent-6,agent-7,main@1,chief@2"; strings.Join(got, ",") != want {
		t.Fatalf("after its window came back the list = %s, want %s", strings.Join(got, ","), want)
	}
}

// pinPost posts a pin form as the signed-in operator from muster's own origin.
func pinPost(t *testing.T, s *Server, h http.Handler, path string, form url.Values, htmx bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://"+req.Host)
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	admit(s, req)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestThePinRoutesPinAndUnpinIdempotently: through the real mux — an htmx post
// answers the re-rendered list with the pinned card first, a non-htmx form post
// a 303 back to /agents (a route behaviour only: no page without JavaScript
// draws a toggle), each twice with the same result.
func TestThePinRoutesPinAndUnpinIdempotently(t *testing.T) {
	fm := &fakeMirror{online: true, windows: pinWindows()}
	mp := &memPins{agents: map[int64]bool{5: true, 6: true, 7: true}}
	s, h := pinServer(t, fm, mp)
	agent7 := url.Values{"kind": {"agent"}, "agent_id": {"7"}}
	chief := url.Values{"kind": {"mirror"}, "host": {"ws"}, "session": {"chief"}, "window": {"@2"}}

	for i := 0; i < 2; i++ {
		rec := pinPost(t, s, h, "/agents/pins", agent7, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("htmx pin #%d = %d %s", i, rec.Code, rec.Body)
		}
		if got := listOrder(t, rec.Body.String()); len(got) == 0 || got[0] != "agent-7" {
			t.Fatalf("htmx pin #%d answered a list whose first card is %v, want agent-7", i, got)
		}
		rec = pinPost(t, s, h, "/agents/pins", chief, false)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/agents" {
			t.Fatalf("plain pin #%d = %d → %q, want 303 → /agents", i, rec.Code, rec.Header().Get("Location"))
		}
	}
	if _, got := pinnedList(t, s, h); strings.Join(got, ",") != "agent-7,chief@2,agent-5,agent-6,main@1" {
		t.Fatalf("after pinning = %v", got)
	}
	for i := 0; i < 2; i++ {
		for _, f := range []url.Values{agent7, chief} {
			if rec := pinPost(t, s, h, "/agents/pins/delete", f, false); rec.Code != http.StatusSeeOther {
				t.Fatalf("unpin #%d of %v = %d %s", i, f, rec.Code, rec.Body)
			}
		}
	}
	if _, got := pinnedList(t, s, h); strings.Join(got, ",") != "agent-5,agent-6,agent-7,main@1,chief@2" {
		t.Fatalf("after unpinning = %v", got)
	}

	// Refusals: a bad subject, an agent the store does not know, a
	// cross-origin post, and no session.
	for _, c := range []struct {
		name string
		form url.Values
		want int
	}{
		{"no kind", url.Values{"agent_id": {"7"}}, http.StatusBadRequest},
		{"bad id", url.Values{"kind": {"agent"}, "agent_id": {"x"}}, http.StatusBadRequest},
		{"a window index, not an id", url.Values{"kind": {"mirror"}, "host": {"ws"}, "session": {"chief"}, "window": {"2"}}, http.StatusBadRequest},
		{"an unknown agent", url.Values{"kind": {"agent"}, "agent_id": {"99"}}, http.StatusNotFound},
	} {
		if rec := pinPost(t, s, h, "/agents/pins", c.form, true); rec.Code != c.want {
			t.Errorf("%s: %d, want %d", c.name, rec.Code, c.want)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/agents/pins", strings.NewReader(agent7.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	admit(s, req)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), CrossOriginRefusal) {
		t.Errorf("cross-origin pin = %d %s, want 403 %q", rec.Code, rec.Body, CrossOriginRefusal)
	}
	req = httptest.NewRequest(http.MethodPost, "/agents/pins", strings.NewReader(agent7.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://"+req.Host)
	req.Header.Set("HX-Request", "true")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("signed-out pin = %d, want 401", rec.Code)
	}
	if set := mp.pinFor(authz.OperatorUserID); len(set.Agents)+len(set.Mirrors) != 0 {
		t.Errorf("a refused request left a pin: %+v", set)
	}

	// No store: 404, the list renders unpinned.
	s2, h2 := pinServer(t, fm, nil)
	if rec := pinPost(t, s2, h2, "/agents/pins", agent7, true); rec.Code != http.StatusNotFound {
		t.Errorf("pin with no store = %d, want 404", rec.Code)
	}
}

// TestThePinRoutesAreRefusedOnTheMachineListener: in gotrue mode the pin
// routes are browser-session routes, so the machine listener (:30306) refuses
// them with the off-browser 403 even for a valid session — and the browser
// listener, same session and origin, pins (the control that makes the 403 a
// fact about the listener).
func TestThePinRoutesAreRefusedOnTheMachineListener(t *testing.T) {
	f := newGoTrueFixtureWith(t, &Extensions{Agents: activityFixture(), SessionLiveness: stubLiveness{}, Pins: &memPins{}})
	sub := f.member("op@example.com", authz.OperatorUserID, authz.RoleAdmin)
	at, rt, _ := f.fake.Mint(sub)
	form := url.Values{"kind": {"agent"}, "agent_id": {"7"}}
	origin := func(r *http.Request) { r.Header.Set("Origin", "http://"+r.Host) }
	for _, path := range []string{"/agents/pins", "/agents/pins/delete"} {
		if w := do(f.machine, "POST", path, form, withCookies(at, rt), asXHR, origin); w.Code != http.StatusForbidden ||
			!strings.Contains(w.Body.String(), OffBrowserListenerRefusal) {
			t.Errorf("%s on the machine listener: %d %s, want 403 %q", path, w.Code, w.Body, OffBrowserListenerRefusal)
		}
		if w := do(f.machine, "POST", path, form, func(r *http.Request) { r.Header.Set("X-Muster-Token", gtHook) }, asXHR, origin); w.Code == http.StatusOK || w.Code == http.StatusSeeOther {
			t.Errorf("%s with the hook token on the machine listener: %d, want a refusal", path, w.Code)
		}
		if w := do(f.edge, "POST", path, form, withCookies(at, rt), origin); w.Code != http.StatusSeeOther {
			t.Errorf("%s on the browser listener (the control): %d %s, want 303", path, w.Code, w.Body)
		}
	}
}
