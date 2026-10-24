package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/users"
)

// routeRuntime is the chief pod's ONE endpoint (ccd's port): it records, for
// every turn, the session key and the four route headers, and answers like the
// runtime does.
type routeRuntime struct {
	mu    sync.Mutex
	turns []routedTurn
	// instructions is each turn's request `instructions`, beside turns.
	instructions []string
}

type routedTurn struct {
	key, runtime, ccSession, ccModel, gwModel string
}

func (r routedTurn) String() string {
	return fmt.Sprintf("key=%s runtime=%q cc=%q ccmodel=%q gwmodel=%q", r.key, r.runtime, r.ccSession, r.ccModel, r.gwModel)
}

func (f *routeRuntime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var req struct {
		Instructions string `json:"instructions"`
	}
	_ = json.Unmarshal(raw, &req)
	f.mu.Lock()
	f.instructions = append(f.instructions, req.Instructions)
	f.turns = append(f.turns, routedTurn{key: r.Header.Get("X-Openclaw-Session-Key"),
		runtime: r.Header.Get("X-Muster-Runtime"), ccSession: r.Header.Get("X-Muster-CC-Session"),
		ccModel: r.Header.Get("X-Muster-CC-Model"), gwModel: r.Header.Get("X-Openclaw-Model")})
	f.mu.Unlock()
	const reply = "noted"
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\","+
		"\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":%q}]}]}}\n\n", reply)
}

func (f *routeRuntime) last(t *testing.T) routedTurn {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.turns) == 0 {
		t.Fatal("the runtime saw no turn")
	}
	return f.turns[len(f.turns)-1]
}

func (f *routeRuntime) lastInstructions(t *testing.T) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.instructions) == 0 {
		t.Fatal("the runtime saw no turn")
	}
	return f.instructions[len(f.instructions)-1]
}

func (f *routeRuntime) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.turns)
}

// TestChiefSessionRuntimesThroughTheRealPlane is the seam test for migration
// 0030's routing: real Postgres stores, real routes, the real deliverer and the
// real agentgateway.Gateway; only the chief's pod endpoint is a fake that
// records what each turn carried.
//
//   - A claude-code chief holding the live shape — its legacy session and a
//     NEWER plugin session — has the legacy one as its default.
//   - Two sessions opened on claude-code (opus, haiku), over
//     POST /agents/{id}/sessions, read back with their runtime and model; a bad
//     model, an unknown runtime, the retired gateway runtime, and any runtime
//     for a worker are refused and write nothing.
//   - Each session's turn carries its route: none for a legacy session (ccd's
//     original CLI), the session key and model for claude-code — and never the
//     retired gateway runtime's headers.
//   - `chief ask` and a plugin delivery go to the DEFAULT session — not the
//     latest, and not a session of the plugin's own — and carry its route; after
//     POST …/default moves the default, both follow it.
func TestChiefSessionRuntimesThroughTheRealPlane(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	sys := authz.System(ctx)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	var owner int64
	if err := pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, "rt"+suffix).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	asOwner := authz.WithPrincipal(ctx, authz.Principal{UserID: owner, Role: authz.RoleMember, Via: authz.ViaSession})
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM plugins WHERE owner_id = $1`, `DELETE FROM agents WHERE owner_id = $1`,
			`DELETE FROM audit_log WHERE user_id = $1`, `DELETE FROM users WHERE id = $1`} {
			_, _ = pool.Exec(authz.System(context.Background()), q, owner)
		}
	})
	as := agents.NewPG(pool)
	chief, err := as.Create(asOwner, agents.Agent{Name: "rtchief-" + suffix, Namespace: "devpod-rtchief-" + suffix,
		DisplayName: "chief", Status: agents.StatusRunning, HooksToken: "rt-hooks-" + suffix,
		Kind: agents.KindClaudeCode, CCAccount: "acct-one"})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := as.Create(asOwner, agents.Agent{Name: "rtworker-" + suffix, Namespace: "devpod-rtworker-" + suffix,
		Status: agents.StatusRunning, HooksToken: "rtw-hooks-" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := as.LatestOrCreateSession(asOwner, chief.ID, chief.Name)
	if err != nil {
		t.Fatal(err)
	}
	pluginSess, err := as.CreateSession(asOwner, chief.ID, chief.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := as.AddChatMessage(asOwner, agents.ChatMessage{AgentID: chief.ID, SessionID: pluginSess.ID, Role: "user", Content: "newer"}); err != nil {
		t.Fatal(err)
	}

	rt := &routeRuntime{}
	up := httptest.NewServer(rt)
	t.Cleanup(up.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(up.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	gw, err := agentgateway.New(agentgateway.Config{Driver: fixedEndpoint{provision.Endpoint{Host: host, Port: port}},
		Runtime: agentgateway.HooksSHA256(), Model: "runtime-sentinel", Client: up.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ps := plugins.NewPG(pool)
	srv := New(nil, AuthConfig{HookToken: tokTestHook, UIPassword: "rt-seam-operator-pw"}, log.New(io.Discard, "", 0))
	toks := newFakeAPITokens()
	ownerTok := toks.add(t, owner)
	srv.UseExtensions(Extensions{Agents: as, Users: users.NewPG(pool), Plugins: ps, PluginGateway: gw, Gateway: gw,
		APITokens: toks, SessionRuntimes: []string{agents.SessionRuntimeClaudeCode}})
	const userHeader = "X-Rt-Test-User"
	srv.sessionUserOverride = func(r *http.Request) int64 {
		v, _ := strconv.ParseInt(r.Header.Get(userHeader), 10, 64)
		return v
	}
	h := srv.Handler()
	do := func(method, path, body string, bearer string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Host = "muster.test"
		if strings.HasPrefix(body, "{") {
			r.Header.Set("Content-Type", "application/json")
		} else if body != "" {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		} else {
			r.Header.Set("Cookie", sessionCookieFor(srv))
			r.Header.Set("Origin", "http://muster.test")
			r.Header.Set(userHeader, strconv.FormatInt(owner, 10))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	sessionsOf := func(a agents.Agent) int {
		l, err := as.ListSessions(asOwner, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		return len(l)
	}

	// The default of the live shape is the legacy session, not the newer one.
	if d, err := srv.defaultSession(asOwner, chief); err != nil || d.ID != legacy.ID {
		t.Fatalf("the chief's default: %+v, %v; want its legacy session %d (the plugin session %d is newer)", d, err, legacy.ID, pluginSess.ID)
	}

	open := func(a agents.Agent, runtime, model string) (agents.ChatSession, *httptest.ResponseRecorder) {
		rec := do("POST", "/agents/"+strconv.FormatInt(a.ID, 10)+"/sessions",
			url.Values{"runtime": {runtime}, "model": {model}}.Encode(), "")
		loc := rec.Header().Get("HX-Redirect")
		i := strings.Index(loc, "?session=")
		if rec.Code != http.StatusOK || i < 0 {
			return agents.ChatSession{}, rec
		}
		id, _ := strconv.ParseInt(loc[i+len("?session="):], 10, 64)
		s, err := as.GetSession(asOwner, id)
		if err != nil {
			t.Fatal(err)
		}
		return s, rec
	}
	cc, rec := open(chief, "claude-code", "opus")
	if cc.Runtime != "claude-code" || cc.Model != "opus" || cc.AgentID != chief.ID {
		t.Fatalf("opening a claude-code/opus session: %d %q → %+v", rec.Code, rec.Body.String(), cc)
	}
	or, rec := open(chief, "claude-code", "haiku")
	if or.Runtime != "claude-code" || or.Model != "haiku" {
		t.Fatalf("opening a claude-code/haiku session: %d %q → %+v", rec.Code, rec.Body.String(), or)
	}
	before := sessionsOf(chief)
	for _, bad := range []struct {
		a              agents.Agent
		runtime, model string
	}{
		{chief, "gateway", "openrouter/vendor-x/model-y"}, {chief, "claude-code", "opus --dangerously"}, {chief, "webchat", "opus"},
		{worker, "claude-code", "opus"},
	} {
		if s, rec := open(bad.a, bad.runtime, bad.model); rec.Code != http.StatusBadRequest || s.ID != 0 {
			t.Errorf("opening %s/%q on %s: %d %+v; want 400", bad.runtime, bad.model, bad.a.Name, rec.Code, s)
		}
	}
	if after := sessionsOf(chief); after != before || sessionsOf(worker) != 0 {
		t.Errorf("refused opens wrote sessions: chief %d → %d, worker %d", before, after, sessionsOf(worker))
	}

	// A cross-site form (the operator's cookie, another site's Origin) can
	// neither open a session nor move the default: both are 403 and write nothing.
	crossSite := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Host = "muster.test"
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Cookie", sessionCookieFor(srv))
		r.Header.Set("Origin", "https://elsewhere.example")
		r.Header.Set(userHeader, strconv.FormatInt(owner, 10))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	if rec := crossSite("/agents/"+strconv.FormatInt(chief.ID, 10)+"/sessions",
		url.Values{"runtime": {"claude-code"}, "model": {"opus"}}.Encode()); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), CrossOriginRefusal) {
		t.Errorf("a cross-site session open: %d %q; want 403 %q", rec.Code, rec.Body.String(), CrossOriginRefusal)
	}
	if after := sessionsOf(chief); after != before {
		t.Errorf("a cross-site open wrote a session: %d → %d", before, after)
	}
	if rec := crossSite("/agents/"+strconv.FormatInt(chief.ID, 10)+"/sessions/"+strconv.FormatInt(cc.ID, 10)+"/default", ""); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), CrossOriginRefusal) {
		t.Errorf("a cross-site default move: %d %q; want 403 %q", rec.Code, rec.Body.String(), CrossOriginRefusal)
	}
	if d, _ := srv.defaultSession(asOwner, chief); d.ID != legacy.ID {
		t.Errorf("a cross-site POST moved the default to %d; want it still %d", d.ID, legacy.ID)
	}

	// Each session's turn carries its route, and only its route.
	want := map[int64]routedTurn{
		legacy.ID: {key: legacy.SessionKey},
		cc.ID:     {key: cc.SessionKey, ccSession: cc.SessionKey, ccModel: "opus"},
		or.ID:     {key: or.SessionKey, ccSession: or.SessionKey, ccModel: "haiku"},
	}
	for _, s := range []agents.ChatSession{legacy, cc, or} {
		if _, err := srv.runUserTurn(asOwner, chief, s, "hello "+s.SessionKey, nil); err != nil {
			t.Fatalf("a turn in session %d: %v", s.ID, err)
		}
		if got := rt.last(t); got != want[s.ID] {
			t.Errorf("session %d (%s/%s) turn carried\n  %v\nwant\n  %v", s.ID, s.Runtime, s.Model, got, want[s.ID])
		}
	}

	ask := func() (routedTurn, int64) {
		t.Helper()
		n := rt.count()
		rec := do("POST", "/api/agents/"+chief.Name+"/messages", `{"message":"status?"}`, ownerTok)
		if rec.Code != http.StatusOK || rt.count() != n+1 {
			t.Fatalf("chief ask: %d %s (%d turns)", rec.Code, rec.Body.String(), rt.count()-n)
		}
		var body struct {
			SessionID int64 `json:"sessionId"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return rt.last(t), body.SessionID
	}
	mint := func() string {
		rec := do("POST", "/plugins", url.Values{"name": {"example-inbox"}, "label": {"Example inbox"},
			"data_scope": {"example-inbox"}}.Encode(), "")
		m := mintedPluginRE.FindStringSubmatch(rec.Body.String())
		if rec.Code != http.StatusOK || m == nil {
			t.Fatalf("mint: %d", rec.Code)
		}
		return m[1]
	}
	plugTok := mint()
	n := 0
	deliver := func() (routedTurn, int64) {
		t.Helper()
		n++
		before := rt.count()
		rec := do("POST", "/plugin/v1/messages", fmt.Sprintf(`{"external_id":"rt-%s-%d","sender":"Ada <ada@example.invalid>",`+
			`"subject":"hi","body":"are you there?","received_at":"2000-01-02T03:04:05Z"}`, suffix, n), plugTok)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("plugin post: %d %s", rec.Code, rec.Body.String())
		}
		srv.WaitForPluginDeliveries()
		if rt.count() != before+1 {
			t.Fatalf("the plugin message made %d turns, want 1", rt.count()-before)
		}
		ps, _ := ps.List(asOwner)
		if len(ps) != 1 || ps[0].ChiefSessionID == nil {
			t.Fatalf("the plugin %+v has no chief session", ps)
		}
		return rt.last(t), *ps[0].ChiefSessionID
	}

	// Default = legacy: no route on either door, and the turn is in the legacy session.
	if got, sid := ask(); got != want[legacy.ID] || sid != legacy.ID {
		t.Errorf("chief ask with the legacy default: %v in session %d; want %v in %d", got, sid, want[legacy.ID], legacy.ID)
	}
	// ccd's own CLI reads the chief's CLAUDE.md: the ask carries no instructions.
	if got := rt.lastInstructions(t); got != "" {
		t.Errorf("chief ask with the legacy default carried %d bytes of instructions; want none", len(got))
	}
	if got, sid := deliver(); got != want[legacy.ID] || sid != legacy.ID {
		t.Errorf("a plugin delivery with the legacy default: %v in session %d; want %v in %d", got, sid, want[legacy.ID], legacy.ID)
	}
	chiefSessions := sessionsOf(chief)

	// Move the default to the haiku session: both doors follow it.
	if rec := do("POST", "/agents/"+strconv.FormatInt(chief.ID, 10)+"/sessions/"+strconv.FormatInt(or.ID, 10)+"/default", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("POST …/default: %d %s", rec.Code, rec.Body.String())
	}
	if got, sid := ask(); got != want[or.ID] || sid != or.ID {
		t.Errorf("chief ask with the haiku default: %v in session %d; want %v in %d", got, sid, want[or.ID], or.ID)
	}
	// A Claude Code CLI reads the chief's CLAUDE.md: this ask carries no
	// instructions either.
	if got := rt.lastInstructions(t); got != "" {
		t.Errorf("chief ask with the haiku default carried %d bytes of instructions; want none", len(got))
	}
	if got, sid := deliver(); got != want[or.ID] || sid != or.ID {
		t.Errorf("a plugin delivery with the haiku default: %v in session %d; want %v in %d", got, sid, want[or.ID], or.ID)
	}
	if got := sessionsOf(chief); got != chiefSessions {
		t.Errorf("deliveries opened sessions: %d → %d", chiefSessions, got)
	}
	// Another agent's session cannot be made this agent's default.
	other, err := as.LatestOrCreateSession(asOwner, worker.ID, worker.Name)
	if err != nil {
		t.Fatal(err)
	}
	if rec := do("POST", "/agents/"+strconv.FormatInt(chief.ID, 10)+"/sessions/"+strconv.FormatInt(other.ID, 10)+"/default", "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("making the worker's session the chief's default: %d; want 404", rec.Code)
	}
	if d, _ := srv.defaultSession(asOwner, chief); d.ID != or.ID {
		t.Errorf("a refused move changed the default to %d", d.ID)
	}
}

// TestChiefAskGoesToTheDefaultSessionWithTheRuntimesFlagUnset pins what the docs
// now say: MUSTER_CHIEF_SESSION_RUNTIMES draws the runtime picker and does NOT
// gate default-session routing. With it unset, `chief ask` still lands in the
// chief's default (its legacy-key session), not in its newer, latest one.
//
// ⚠ AN INVARIANT GUARD, NOT A REGRESSION TEST: the behaviour was already this
// at origin/main; what was wrong was the docs and comments claiming otherwise.
func TestChiefAskGoesToTheDefaultSessionWithTheRuntimesFlagUnset(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	sys := authz.System(ctx)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	var owner int64
	if err := pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, "rf"+suffix).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	asOwner := authz.WithPrincipal(ctx, authz.Principal{UserID: owner, Role: authz.RoleMember, Via: authz.ViaSession})
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM agents WHERE owner_id = $1`, `DELETE FROM audit_log WHERE user_id = $1`, `DELETE FROM users WHERE id = $1`} {
			_, _ = pool.Exec(authz.System(context.Background()), q, owner)
		}
	})
	as := agents.NewPG(pool)
	chief, err := as.Create(asOwner, agents.Agent{Name: "rfchief-" + suffix, Namespace: "devpod-rfchief-" + suffix,
		DisplayName: "chief", Status: agents.StatusRunning, HooksToken: "rf-hooks-" + suffix, Kind: agents.KindGateway})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := as.LatestOrCreateSession(asOwner, chief.ID, chief.Name)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := as.CreateSession(asOwner, chief.ID, chief.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := as.AddChatMessage(asOwner, agents.ChatMessage{AgentID: chief.ID, SessionID: newer.ID, Role: "user", Content: "newer"}); err != nil {
		t.Fatal(err)
	}
	// Control: the latest session is the newer one, so landing in legacy can
	// only be the default rule.
	if l, err := as.LatestOrCreateSession(asOwner, chief.ID, chief.Name); err != nil || l.ID != newer.ID {
		t.Fatalf("control: the latest session is %d (%v); want the newer %d", l.ID, err, newer.ID)
	}

	rt := &routeRuntime{}
	up := httptest.NewServer(rt)
	t.Cleanup(up.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(up.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	gw, err := agentgateway.New(agentgateway.Config{Driver: fixedEndpoint{provision.Endpoint{Host: host, Port: port}},
		Runtime: agentgateway.HooksSHA256(), Model: "runtime-sentinel", Client: up.Client()})
	if err != nil {
		t.Fatal(err)
	}
	srv := New(nil, AuthConfig{HookToken: tokTestHook, UIPassword: "rf-seam-operator-pw"}, log.New(io.Discard, "", 0))
	toks := newFakeAPITokens()
	ownerTok := toks.add(t, owner)
	srv.UseExtensions(Extensions{Agents: as, Users: users.NewPG(pool), Gateway: gw, APITokens: toks}) // no SessionRuntimes
	r := httptest.NewRequest("POST", "/api/agents/"+chief.Name+"/messages", strings.NewReader(`{"message":"status?"}`))
	r.Host = "muster.test"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+ownerTok)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	var body struct {
		SessionID int64 `json:"sessionId"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		t.Fatalf("chief ask: %d %s", rec.Code, rec.Body.String())
	}
	if body.SessionID != legacy.ID || rt.last(t).key != legacy.SessionKey {
		t.Errorf("with MUSTER_CHIEF_SESSION_RUNTIMES unset, chief ask went to session %d (key %s); want the default, %d",
			body.SessionID, rt.last(t).key, legacy.ID)
	}
}
