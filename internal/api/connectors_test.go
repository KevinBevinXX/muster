package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/flows"
)

// The connector store's DOORS: who may reach the agent surface, who may reach
// the operator surface, and what the dedicated connect listener serves. The
// proxy's own security (allowlist, address policy, header handling, redaction,
// grant checks) is tested in internal/connectors against a fake upstream.

const (
	connTestHookToken    = "hook-token-for-connector-tests-0123456789"
	connTestServiceToken = "service-token-for-connector-tests-0123456789"
	connTestPassword     = "operator-password-for-tests"
	connTestSecret       = "ghp_FAKE-connector-api-test-0123456789"
)

type connFixture struct {
	ctx   context.Context
	pool  *pgxpool.Pool
	srv   *Server
	h     http.Handler
	store *connectors.PGStore
}

func newConnFixture(t *testing.T) *connFixture { t.Helper(); return newConnFixtureWith(t, nil) }

// newConnFixtureWith is newConnFixture with more extensions wired over the same
// pool (extra may add to ext; nil adds nothing).
func newConnFixtureWith(t *testing.T, extra func(ext *Extensions, pool *pgxpool.Pool)) *connFixture {
	t.Helper()
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	dbtest.OwnerExec(t, dsn, `TRUNCATE connectors, connector_grants, connector_audit RESTART IDENTITY`)
	store, err := connectors.NewPG(pool, bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, AuthConfig{HookToken: connTestHookToken, ServiceToken: connTestServiceToken, UIPassword: connTestPassword},
		log.New(io.Discard, "", 0))
	ext := Extensions{Agents: agents.NewPG(pool), Connectors: store, Flows: flows.NewPG(pool)}
	if extra != nil {
		extra(&ext, pool)
	}
	s.UseExtensions(ext)
	return &connFixture{ctx: ctx, pool: pool, srv: s, h: s.Handler(), store: store}
}

func (f *connFixture) seedAgent(t *testing.T, name, token string) int64 {
	t.Helper()
	var id int64
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO agents (name, namespace, hooks_token) VALUES ($1, $2, $3) RETURNING id`,
		name, "ns-"+name, token).Scan(&id); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(dbtest.AsOperator(context.Background()), `DELETE FROM agents WHERE id=$1`, id)
	})
	return id
}

func (f *connFixture) call(t *testing.T, method, path string, body io.Reader, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, body)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

func (f *connFixture) operator() map[string]string {
	// Origin is what a browser sends from muster's own page (httptest's Host is
	// example.com); requireSameOrigin refuses the write without it.
	return map[string]string{"Cookie": sessionCookieFor(f.srv), "HX-Request": "true", "Origin": "http://example.com"}
}

func form(v url.Values) (io.Reader, map[string]string) {
	return strings.NewReader(v.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
}

func merge(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// agentRoutes are every agent-facing connector route, as a caller would hit them.
var agentRoutes = []struct{ method, path string }{
	{"GET", "/connect/github/api.github.com/user"},
	{"GET", "/agent/connectors"},
	{"GET", "/agent/connectors/github"},
	{"POST", "/agent/connectors/github/request"},
	{"POST", "/mcp"},
}

// TestConnectorRoutesRefuseTheSharedHookToken: the operator's shared hook token
// (and the service token) cannot draw on connectors as if it were an agent —
// even in the worst case, where an agent row's own token happens to EQUAL the
// hook token, so the row lookup alone would have admitted it.
func TestConnectorRoutesRefuseTheSharedHookToken(t *testing.T) {
	f := newConnFixture(t)
	f.seedAgent(t, "collides-with-hook", connTestHookToken)
	f.seedAgent(t, "honest-agent", "honest-agent-token-0123456789")
	for _, rt := range agentRoutes {
		for name, tok := range map[string]string{"hook": connTestHookToken, "service": connTestServiceToken} {
			for _, hdr := range []map[string]string{{"X-Muster-Token": tok}, {"Authorization": "Bearer " + tok}} {
				w := f.call(t, rt.method, rt.path, nil, hdr)
				if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "not an agent credential") {
					t.Errorf("%s %s with the %s token (%v): %d %s, want 403 refusing a shared token",
						rt.method, rt.path, name, hdr, w.Code, w.Body)
				}
			}
		}
		// Control: the agent's OWN token passes the door (whatever the handler says next).
		w := f.call(t, rt.method, rt.path, nil, map[string]string{"X-Muster-Token": "honest-agent-token-0123456789"})
		if w.Code == http.StatusUnauthorized || strings.Contains(w.Body.String(), "not an agent credential") {
			t.Errorf("control: %s %s with an agent's own token: %d %s", rt.method, rt.path, w.Code, w.Body)
		}
	}
	pending, _ := f.store.ListPendingGrants(f.ctx)
	for _, g := range pending {
		if g.AgentName == "collides-with-hook" {
			t.Fatal("a request was filed in the name of the agent whose token equals the hook token")
		}
	}
}

func TestConnectorRoutesRefuseAMissingOrUnknownToken(t *testing.T) {
	f := newConnFixture(t)
	for _, rt := range agentRoutes {
		for _, hdr := range []map[string]string{nil, {"X-Muster-Token": "nobody-has-this-token-0123"}} {
			if w := f.call(t, rt.method, rt.path, nil, hdr); w.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with %v: %d, want 401", rt.method, rt.path, hdr, w.Code)
			}
		}
	}
}

func TestConnectorRoutesAre503WithoutAStore(t *testing.T) {
	s := New(nil, AuthConfig{HookToken: connTestHookToken, UIPassword: connTestPassword}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Agents: stubAgentsStore{}})
	h := s.Handler()
	for _, rt := range agentRoutes {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(rt.method, rt.path, nil)
		r.Header.Set("X-Muster-Token", "any-agent-token-0123456789")
		h.ServeHTTP(w, r)
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "MUSTER_CONNECTOR_ENCRYPTION_KEY") {
			t.Errorf("%s %s: %d %s, want 503 naming the key", rt.method, rt.path, w.Code, w.Body)
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/ui/connectors", nil)
	r.Header.Set("Cookie", sessionCookieFor(s))
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "connectors-unconfigured") {
		t.Fatalf("the panel without a store: %d %s", w.Code, w.Body)
	}
}

// TestTheOperatorSurfaceRefusesAgentAndHookTokens: approving, creating, rotating,
// deleting and revoking are the HUMAN tier's; no machine credential opens them.
func TestTheOperatorSurfaceRefusesAgentAndHookTokens(t *testing.T) {
	f := newConnFixture(t)
	f.seedAgent(t, "pushy-agent", "pushy-agent-token-0123456789")
	for _, rt := range []struct{ method, path string }{
		{"GET", "/ui/connectors"}, {"POST", "/connectors"}, {"POST", "/connectors/github/rotate"},
		{"DELETE", "/connectors/github"}, {"POST", "/ui/connector-requests/1/approve"},
		{"POST", "/ui/connector-requests/1/deny"}, {"DELETE", "/connector-grants/1"},
	} {
		for _, tok := range []string{"pushy-agent-token-0123456789", connTestHookToken} {
			w := f.call(t, rt.method, rt.path, nil, map[string]string{"X-Muster-Token": tok, "Authorization": "Bearer " + tok})
			if w.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with a machine token: %d, want 401", rt.method, rt.path, w.Code)
			}
		}
	}
}

// TestTheAgentRequestsAndTheOperatorEntersTheCredential walks the whole flow
// through the real handlers: the agent asks for a connector nobody configured,
// the operator's panel offers the enter-credential form, the operator submits it,
// and the agent's grant is approved — while no response anywhere echoes the
// secret.
func TestTheAgentRequestsAndTheOperatorEntersTheCredential(t *testing.T) {
	f := newConnFixture(t)
	f.seedAgent(t, "flow-agent", "flow-agent-token-0123456789")
	agent := map[string]string{"X-Muster-Token": "flow-agent-token-0123456789", "Content-Type": "application/json"}

	w := f.call(t, "POST", "/agent/connectors/github/request", strings.NewReader(`{"reason":"open a PR"}`), agent)
	var v AgentConnectorView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if w.Code != http.StatusAccepted || v.Grant != "pending" || v.Exists {
		t.Fatalf("request: %d %s", w.Code, w.Body)
	}
	panel := f.call(t, "GET", "/ui/connectors", nil, f.operator())
	if !strings.Contains(panel.Body.String(), `data-enter-credential="`+strconv.FormatInt(v.GrantID, 10)+`"`) {
		t.Fatalf("the panel does not offer the enter-credential form:\n%s", panel.Body)
	}
	formPage := f.call(t, "GET", "/ui/connectors/new?request="+strconv.FormatInt(v.GrantID, 10), nil, f.operator())
	if !strings.Contains(formPage.Body.String(), `value="github"`) || !strings.Contains(formPage.Body.String(), "flow-agent") {
		t.Fatalf("the form does not carry the request:\n%s", formPage.Body)
	}

	body, hdr := form(url.Values{"request_id": {strconv.FormatInt(v.GrantID, 10)}, "preset": {"github"},
		"name": {"github"}, "secret": {connTestSecret}})
	w = f.call(t, "POST", "/connectors", body, merge(f.operator(), hdr))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "flow-agent approved") {
		t.Fatalf("create+approve: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), connTestSecret) {
		t.Fatal("the create response echoes the secret")
	}
	panel = f.call(t, "GET", "/ui/connectors", nil, f.operator())
	if strings.Contains(panel.Body.String(), connTestSecret) || strings.Contains(panel.Body.String(), "••••") {
		t.Fatalf("the panel leaks the secret or a fragment of it:\n%s", panel.Body)
	}

	w = f.call(t, "GET", "/agent/connectors/github", nil, agent)
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if v.Grant != "approved" || !v.Exists || !strings.Contains(v.Usage, "/connect/github/") {
		t.Fatalf("status after approval: %s", w.Body)
	}
	list := f.call(t, "GET", "/agent/connectors", nil, agent)
	if strings.Contains(list.Body.String(), connTestSecret) || strings.Contains(list.Body.String(), "secretHint") {
		t.Fatalf("the agent listing carries the secret or a fragment of it: %s", list.Body)
	}
	audit, _ := f.store.ListAudit(f.ctx, 10)
	var decisions []string
	for _, e := range audit {
		if e.Kind == "grant" {
			decisions = append(decisions, e.Detail)
		}
	}
	if strings.Join(decisions, "|") != "approved|connector created by the operator|requested" {
		t.Fatalf("grant decisions recorded = %v", decisions)
	}
}

func TestTheOperatorDeniesAndRevokes(t *testing.T) {
	f := newConnFixture(t)
	a := f.seedAgent(t, "deny-agent", "deny-agent-token-0123456789")
	if _, err := f.store.CreateConnector(f.ctx, connectors.NewConnector{Name: "svc", Hosts: []string{"api.example.com"},
		Scheme: connectors.SchemeBearer, Secret: connTestSecret}); err != nil {
		t.Fatal(err)
	}
	g, _, _ := f.store.RequestGrant(f.ctx, a, "svc", "")
	w := f.call(t, "POST", "/ui/connector-requests/"+strconv.FormatInt(g.ID, 10)+"/deny", nil, f.operator())
	if got, _ := f.store.GetGrant(f.ctx, g.ID); w.Code != 200 || got.Status != "denied" {
		t.Fatalf("deny: %d %+v", w.Code, got)
	}
	g2, _, _ := f.store.RequestGrant(f.ctx, a, "svc", "")
	w = f.call(t, "POST", "/ui/connector-requests/"+strconv.FormatInt(g2.ID, 10)+"/approve", nil, f.operator())
	got, _ := f.store.GetGrant(f.ctx, g2.ID)
	if w.Code != 200 || got.Status != "approved" {
		t.Fatalf("approve: %d %+v", w.Code, got)
	}
	w = f.call(t, "DELETE", "/connector-grants/"+strconv.FormatInt(g2.ID, 10), nil, f.operator())
	if got, _ := f.store.GetGrant(f.ctx, g2.ID); w.Code != 200 || got.Status != "revoked" {
		t.Fatalf("revoke: %d %+v", w.Code, got)
	}
}

// TestTheConnectListenerServesOnlyTheConnectorSurface: the handler the second
// listener runs answers the agent surface and 404s everything else — so the
// network allowance an isolated agent gets reaches nothing but this.
func TestTheConnectListenerServesOnlyTheConnectorSurface(t *testing.T) {
	f := newConnFixture(t)
	f.seedAgent(t, "surface-agent", "surface-agent-token-0123456789")
	h := OnlyConnectSurface(f.h)
	tok := map[string]string{"X-Muster-Token": "surface-agent-token-0123456789", "Cookie": sessionCookieFor(f.srv)}
	for _, p := range []string{"/agent/connectors", "/agent/connectors/github", "/connect/github/api.github.com/x", "/health", "/mcp"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", p, nil)
		for k, v := range tok {
			r.Header.Set(k, v)
		}
		h.ServeHTTP(w, r)
		if w.Code == http.StatusNotFound && !strings.Contains(w.Body.String(), "grant") {
			t.Errorf("%s: 404 through the connect listener, want it served", p)
		}
	}
	for _, p := range []string{"/ui/connectors", "/connectors", "/api/tasks", "/agent/task", "/agents", "/login",
		"/agent/connectorsx", "/connect", "/metrics", "/", "/events"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", p, nil)
		for k, v := range tok {
			r.Header.Set(k, v)
		}
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: %d through the connect listener, want 404", p, w.Code)
		}
	}
}
