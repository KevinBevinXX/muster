package connectors_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// upstream is a FAKE platform: it records every request it receives and answers
// with what the test scripts. Nothing here talks to a real platform or carries a
// real credential.
type upstream struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []*http.Request
	hits atomic.Int32
	// handle, when set, answers instead of the default echo.
	handle func(w http.ResponseWriter, r *http.Request)
}

func newUpstream(t *testing.T) *upstream {
	u := &upstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		u.mu.Lock()
		u.reqs = append(u.reqs, r.Clone(context.Background()))
		u.mu.Unlock()
		if u.handle != nil {
			u.handle(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"path": r.URL.Path, "query": r.URL.RawQuery})
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstream) port() string {
	_, p, _ := net.SplitHostPort(u.srv.Listener.Addr().String())
	return p
}

func (u *upstream) last(t *testing.T) *http.Request {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.reqs) == 0 {
		t.Fatal("the upstream received no request")
	}
	return u.reqs[len(u.reqs)-1]
}

// testPolicy maps `*.fake.test` to loopback so the fake upstream is reachable,
// exactly as the e2e build does; every other check is production's.
func testPolicy() connectors.Policy {
	return connectors.Policy{
		AllowLoopback: true,
		InsecureHTTP:  true,
		Resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
			if strings.HasSuffix(host, ".fake.test") {
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
			return nil, fmt.Errorf("no such host %s", host)
		},
	}
}

type fixture struct {
	ctx     context.Context
	pool    *pgxpool.Pool
	store   *connectors.PGStore
	proxy   *connectors.Proxy
	up      *upstream
	host    string // the allowlisted host:port of the fake upstream
	agentA  connectors.Caller
	agentB  connectors.Caller
	filed   atomic.Int32
	handler http.Handler
}

// newFixture: a fake upstream, a connector `svc` allowlisting it with basic auth,
// two agents, and a proxy behind a tiny router that stands in for the API's
// /connect/{connector}/{host}/{path...} route (the caller is chosen by a test
// header, because AUTHENTICATION is internal/api's and is tested there).
func newFixture(t *testing.T, scheme connectors.Scheme) *fixture {
	t.Helper()
	return newFixtureWithSecret(t, scheme, fakeSecret)
}

func newFixtureWithSecret(t *testing.T, scheme connectors.Scheme, secret string) *fixture {
	t.Helper()
	ctx, pool := testPool(t)
	f := &fixture{ctx: ctx, pool: pool, store: newStore(t, pool, key(7)), up: newUpstream(t)}
	f.host = "api.fake.test:" + f.up.port()
	nc := connectors.NewConnector{Name: "svc", Hosts: []string{f.host}, Scheme: scheme, Secret: secret}
	switch scheme {
	case connectors.SchemeBasic:
		nc.Username = "x-access-token"
	case connectors.SchemeHeader:
		nc.HeaderName = "X-Api-Key"
	}
	if _, err := f.store.CreateConnector(ctx, nc); err != nil {
		t.Fatal(err)
	}
	f.agentA = connectors.Caller{AgentID: seedAgent(t, ctx, pool, "proxy-agent-a", "tok-pa-0123456789"), AgentName: "proxy-agent-a"}
	f.agentB = connectors.Caller{AgentID: seedAgent(t, ctx, pool, "proxy-agent-b", "tok-pb-0123456789"), AgentName: "proxy-agent-b"}
	f.proxy = &connectors.Proxy{Store: f.store, Policy: testPolicy(), OnGrantRequested: func(context.Context, connectors.Grant) { f.filed.Add(1) }}
	f.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller := f.agentA
		if r.Header.Get("X-Test-Agent") == "b" {
			caller = f.agentB
		}
		parts := strings.SplitN(strings.TrimPrefix(r.URL.EscapedPath(), "/connect/"), "/", 3)
		// The API's door (requireConnectorAgent) attaches the calling agent's
		// owner as the request principal; row-level security scopes every store
		// call the proxy makes to it. Both fixture agents are the operator's.
		r = r.WithContext(dbtest.AsOperator(r.Context()))
		f.proxy.Serve(w, r, caller, parts[0], parts[1], parts[2])
	})
	return f
}

func (f *fixture) approve(t *testing.T, c connectors.Caller) connectors.Grant {
	t.Helper()
	g, _, err := f.store.RequestGrant(f.ctx, c.AgentID, "svc", "")
	if err != nil {
		t.Fatal(err)
	}
	g, err = f.store.DecideGrant(f.ctx, g.ID, true, "operator")
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func (f *fixture) do(t *testing.T, method, path string, hdr http.Header) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	for k, vs := range hdr {
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func basicValue() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+fakeSecret))
}

// assertNoSecret refuses any form of the credential in a response.
func assertNoSecret(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	all := w.Body.String()
	for k, vs := range w.Header() {
		all += "\n" + k + ": " + strings.Join(vs, ",")
	}
	for _, needle := range []string{fakeSecret, "not-a-real-token", strings.TrimPrefix(basicValue(), "Basic ")} {
		if strings.Contains(all, needle) {
			t.Fatalf("the response carries the credential (%q):\n%s", needle, all)
		}
	}
}

func TestTheProxyForwardsToTheAllowlistedHostWithTheCredential(t *testing.T) {
	f := newFixture(t, connectors.SchemeBasic)
	f.approve(t, f.agentA)
	w := f.do(t, "GET", "/connect/svc/"+f.host+"/repos/o/r?per_page=5", nil)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	got := f.up.last(t)
	if got.URL.Path != "/repos/o/r" || got.URL.RawQuery != "per_page=5" {
		t.Fatalf("upstream saw %s?%s", got.URL.Path, got.URL.RawQuery)
	}
	if got.Header.Get("Authorization") != basicValue() {
		t.Fatalf("upstream Authorization = %q, want the injected basic credential", got.Header.Get("Authorization"))
	}
	assertNoSecret(t, w)
}

func TestTheProxyRefusesAHostOutsideTheAllowlist(t *testing.T) {
	f := newFixture(t, connectors.SchemeBearer)
	f.approve(t, f.agentA)
	for _, h := range []string{"evil.fake.test:" + f.up.port(), "api.fake.test", "API.fake.test:1"} {
		w := f.do(t, "GET", "/connect/svc/"+h+"/x", nil)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "allowlist") {
			t.Fatalf("host %s: status %d %s, want 403 naming the allowlist", h, w.Code, w.Body)
		}
	}
	if n := f.up.hits.Load(); n != 0 {
		t.Fatalf("the upstream received %d request(s) for a non-allowlisted host", n)
	}
}

// TestTheProxyRefusesNonPublicDestinations: an allowlisted NAME that resolves to
// loopback (with loopback not allowed), a private address, or the metadata
// address is refused at dial time — the upstream never sees a byte.
func TestTheProxyRefusesNonPublicDestinations(t *testing.T) {
	f := newFixture(t, connectors.SchemeBearer)
	f.approve(t, f.agentA)
	for name, addr := range map[string]netip.Addr{
		"loopback":  netip.MustParseAddr("127.0.0.1"),
		"private":   netip.AddrFrom4([4]byte{10, 9, 8, 7}),
		"metadata":  netip.AddrFrom4([4]byte{169, 254, 169, 254}),
		"ipv6 ula":  netip.MustParseAddr("fd12::1"),
		"cgnat":     netip.AddrFrom4([4]byte{100, 64, 1, 1}),
		"v4-mapped": netip.AddrFrom16(netip.AddrFrom4([4]byte{192, 168, 4, 4}).As16()),
	} {
		f.proxy.Policy = connectors.Policy{InsecureHTTP: true, Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{addr}, nil
		}}
		f.proxy.SetTransport(f.proxy.Policy.Transport())
		w := f.do(t, "GET", "/connect/svc/"+f.host+"/x", nil)
		if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "non-public") {
			t.Fatalf("%s: status %d %s, want 502 naming a non-public address", name, w.Code, w.Body)
		}
		assertNoSecret(t, w)
	}
	if n := f.up.hits.Load(); n != 0 {
		t.Fatalf("the upstream received %d request(s)", n)
	}
}

func TestTheProxyNeverFollowsARedirectOffHost(t *testing.T) {
	f := newFixture(t, connectors.SchemeBasic)
	f.approve(t, f.agentA)
	evil := newUpstream(t)
	evilURL := "http://evil.fake.test:" + evil.port() + "/steal"
	f.up.handle = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/on-host" {
			http.Redirect(w, r, "/moved?x=1", http.StatusFound)
			return
		}
		http.Redirect(w, r, evilURL, http.StatusFound)
	}
	// 🔴 An off-allowlist redirect is not handed to the client at all: its
	// muster token travels in a header that curl and git resend cross-host.
	w := f.do(t, "GET", "/connect/svc/"+f.host+"/off-host", nil)
	if w.Code != http.StatusBadGateway || w.Header().Get("Location") != "" ||
		!strings.Contains(w.Body.String(), "allowlist") || strings.Contains(w.Body.String(), evilURL) {
		t.Fatalf("off-host: status %d Location %q body %s, want 502 naming the allowlist and no Location",
			w.Code, w.Header().Get("Location"), w.Body)
	}
	if n := evil.hits.Load(); n != 0 {
		t.Fatalf("the proxy followed an off-host redirect: the other host got %d request(s)", n)
	}
	w = f.do(t, "GET", "/connect/svc/"+f.host+"/on-host", nil)
	if want := "/connect/svc/" + f.host + "/moved?x=1"; w.Code != http.StatusFound || w.Header().Get("Location") != want {
		t.Fatalf("on-host: status %d Location %q, want %q", w.Code, w.Header().Get("Location"), want)
	}
	if n := f.up.hits.Load(); n != 2 {
		t.Fatalf("the upstream got %d requests, want exactly the two sent (no follow)", n)
	}
}

// TestTheProxyDropsTheCallersHeadersThatCarryIdentity: the agent's own token, an
// Authorization of its choosing, cookies, forwarding headers and a header named
// in Connection (hop-by-hop smuggling) never reach the upstream, and an
// agent-supplied value for the credential header cannot override or accompany
// the injected one.
func TestTheProxyDropsTheCallersHeadersThatCarryIdentity(t *testing.T) {
	for _, scheme := range []connectors.Scheme{connectors.SchemeBearer, connectors.SchemeHeader} {
		t.Run(string(scheme), func(t *testing.T) {
			f := newFixture(t, scheme)
			f.approve(t, f.agentA)
			w := f.do(t, "POST", "/connect/svc/"+f.host+"/x", http.Header{
				"X-Muster-Token":      {"tok-pa-0123456789"},
				"Authorization":       {"Bearer agent-chosen"},
				"X-Api-Key":           {"agent-chosen"},
				"Cookie":              {"session=agent"},
				"Proxy-Authorization": {"Basic agent"},
				"X-Forwarded-For":     {"203.0.113.9"},
				"Connection":          {"X-Smuggle"},
				"X-Smuggle":           {"1"},
				"X-Kept":              {"yes"},
			})
			if w.Code != 200 {
				t.Fatalf("status %d %s", w.Code, w.Body)
			}
			got := f.up.last(t)
			for _, h := range []string{"X-Muster-Token", "Cookie", "Proxy-Authorization", "X-Forwarded-For", "X-Smuggle"} {
				if v := got.Header.Get(h); v != "" {
					t.Errorf("upstream received %s: %q", h, v)
				}
			}
			if got.Header.Get("X-Kept") != "yes" {
				t.Error("an ordinary header was dropped (control)")
			}
			switch scheme {
			case connectors.SchemeBearer:
				if v := got.Header.Values("Authorization"); len(v) != 1 || v[0] != "Bearer "+fakeSecret {
					t.Errorf("upstream Authorization = %q", v)
				}
			case connectors.SchemeHeader:
				if v := got.Header.Values("X-Api-Key"); len(v) != 1 || v[0] != fakeSecret {
					t.Errorf("upstream X-Api-Key = %q", v)
				}
				if v := got.Header.Get("Authorization"); v != "" {
					t.Errorf("upstream received the caller's Authorization %q", v)
				}
			}
		})
	}
}

// TestAnEchoingUpstreamCannotReturnTheCredential: an upstream that reflects the
// request's headers into its body AND into a response header gets every form of
// the credential replaced before the agent sees it.
func TestAnEchoingUpstreamCannotReturnTheCredential(t *testing.T) {
	f := newFixture(t, connectors.SchemeBasic)
	f.approve(t, f.agentA)
	f.up.handle = func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		dec, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
		w.Header().Set("X-Echo", auth)
		_, _ = io.WriteString(w, "auth="+auth+" decoded="+string(dec))
	}
	w := f.do(t, "GET", "/connect/svc/"+f.host+"/echo", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), connectors.RedactedMarker) {
		t.Fatalf("status %d body %q: the echo did not come back redacted", w.Code, w.Body)
	}
	assertNoSecret(t, w)
}

func TestAnUnreachableUpstreamErrorCarriesNoSecret(t *testing.T) {
	f := newFixture(t, connectors.SchemeBasic)
	f.approve(t, f.agentA)
	f.up.srv.Close()
	w := f.do(t, "GET", "/connect/svc/"+f.host+"/x", nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status %d", w.Code)
	}
	assertNoSecret(t, w)
}

// TestAnAgentWithoutAGrantIsRefusedAndARequestIsFiled.
func TestAnAgentWithoutAGrantIsRefusedAndARequestIsFiled(t *testing.T) {
	f := newFixture(t, connectors.SchemeBasic)
	w := f.do(t, "GET", "/connect/svc/"+f.host+"/x", nil)
	var body connectors.RefusalBody
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusForbidden || body.Grant != connectors.StatusPending || body.GrantID == 0 {
		t.Fatalf("status %d body %+v, want 403 with a pending grant", w.Code, body)
	}
	if f.filed.Load() != 1 {
		t.Fatalf("OnGrantRequested called %d times, want 1", f.filed.Load())
	}
	pending, _ := f.store.ListPendingGrants(f.ctx)
	if len(pending) != 1 || pending[0].AgentID != f.agentA.AgentID || pending[0].Connector != "svc" {
		t.Fatalf("pending = %+v", pending)
	}
	// A retry does not prompt again.
	f.do(t, "GET", "/connect/svc/"+f.host+"/x", nil)
	if f.filed.Load() != 1 {
		t.Fatalf("a retry prompted the operator again (%d)", f.filed.Load())
	}
	if n := f.up.hits.Load(); n != 0 {
		t.Fatalf("the upstream was reached without a grant (%d)", n)
	}
	assertNoSecret(t, w)
}

func TestADeniedAgentIsRefusedWithoutAFreshPrompt(t *testing.T) {
	f := newFixture(t, connectors.SchemeBasic)
	g, _, _ := f.store.RequestGrant(f.ctx, f.agentA.AgentID, "svc", "")
	if _, err := f.store.DecideGrant(f.ctx, g.ID, false, "operator"); err != nil {
		t.Fatal(err)
	}
	w := f.do(t, "GET", "/connect/svc/"+f.host+"/x", nil)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"grant":"denied"`) {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	if f.filed.Load() != 0 {
		t.Fatal("a denial was re-filed as a new request by a retry")
	}
	if f.up.hits.Load() != 0 {
		t.Fatal("the upstream was reached after a denial")
	}
}

func TestARevokedGrantStopsWorking(t *testing.T) {
	f := newFixture(t, connectors.SchemeBasic)
	g := f.approve(t, f.agentA)
	if w := f.do(t, "GET", "/connect/svc/"+f.host+"/x", nil); w.Code != 200 {
		t.Fatalf("control: approved status %d", w.Code)
	}
	if _, err := f.store.RevokeGrant(f.ctx, g.ID, "operator"); err != nil {
		t.Fatal(err)
	}
	w := f.do(t, "GET", "/connect/svc/"+f.host+"/x", nil)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"grant":"revoked"`) {
		t.Fatalf("after revoke: status %d %s", w.Code, w.Body)
	}
	if n := f.up.hits.Load(); n != 1 {
		t.Fatalf("upstream hits = %d, want only the control's 1", n)
	}
	// Like a denial, a revocation is not re-filed by a retrying client.
	if n := f.filed.Load(); n != 0 {
		t.Fatalf("a revocation was re-filed as %d new request(s) by a retry", n)
	}
}

// TestAnotherAgentCannotUseAGrantItDoesNotHold: agent A is approved; agent B's
// identical request is refused (and files B's OWN request).
func TestAnotherAgentCannotUseAGrantItDoesNotHold(t *testing.T) {
	f := newFixture(t, connectors.SchemeBasic)
	f.approve(t, f.agentA)
	w := f.do(t, "GET", "/connect/svc/"+f.host+"/x", http.Header{"X-Test-Agent": {"b"}})
	if w.Code != http.StatusForbidden {
		t.Fatalf("agent B: status %d %s", w.Code, w.Body)
	}
	if f.up.hits.Load() != 0 {
		t.Fatal("agent B reached the upstream on agent A's grant")
	}
	g, err := f.store.LatestGrant(f.ctx, f.agentB.AgentID, "svc")
	if err != nil || g.Status != connectors.StatusPending {
		t.Fatalf("agent B's own request = %+v %v", g, err)
	}
	if w := f.do(t, "GET", "/connect/svc/"+f.host+"/x", nil); w.Code != 200 {
		t.Fatalf("control: agent A status %d", w.Code)
	}
}

func TestEveryProxiedRequestIsAudited(t *testing.T) {
	f := newFixture(t, connectors.SchemeBasic)
	f.approve(t, f.agentA)
	f.do(t, "DELETE", "/connect/svc/"+f.host+"/repos/o/r?token=should-not-be-logged", nil)
	list, err := f.store.ListAudit(f.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var proxied []connectors.AuditEntry
	for _, e := range list {
		if e.Kind == "proxy" {
			proxied = append(proxied, e)
		}
	}
	if len(proxied) != 1 {
		t.Fatalf("proxy audit entries = %+v", list)
	}
	e := proxied[0]
	if e.AgentName != "proxy-agent-a" || e.Connector != "svc" || e.Method != "DELETE" || e.Host != f.host ||
		e.Path != "/repos/o/r" || e.Status != 200 {
		t.Fatalf("audit entry = %+v", e)
	}
	if strings.Contains(e.Path+e.Detail, "should-not-be-logged") {
		t.Fatal("the audit trail recorded the query string")
	}
	c, _ := f.store.GetConnector(f.ctx, "svc")
	if c.LastUsedAt == nil {
		t.Fatal("last_used_at was not stamped")
	}
}
