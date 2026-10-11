package api

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/connectors"
)

// connectorOperatorWrites is the LEDGER of state-changing operator routes on the
// connector surface. TestConnectorOperatorWritesRefuseCrossOrigin fails if the
// registered set (testdata/routes.golden) grows or shrinks against it, so a new
// write route cannot be added without being checked here.
var connectorOperatorWrites = []string{
	"DELETE /connector-grants/{id}",
	"DELETE /connectors/{name}",
	"POST /connectors",
	"POST /connectors/{name}/rotate",
	"POST /ui/connector-requests/{id}/approve",
	"POST /ui/connector-requests/{id}/deny",
}

func registeredConnectorWrites(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("testdata/routes.golden")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		method, path, ok := strings.Cut(strings.TrimSpace(l), " ")
		if !ok || method == "GET" || method == "HEAD" || !strings.Contains(path, "connector") ||
			strings.HasPrefix(path, "/agent/") || strings.HasPrefix(path, "/connect/") {
			continue
		}
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

func concrete(route string) (string, string) {
	method, path, _ := strings.Cut(route, " ")
	path = strings.NewReplacer("{id}", "1", "{name}", "svc").Replace(path)
	return method, path
}

// TestConnectorOperatorWritesRefuseCrossOrigin: a request carrying a valid
// operator session but sent from another origin — including a SIBLING
// SUBDOMAIN, which is same-site, so SameSite=Lax lets the cookie through — is
// refused on every connector write route, and changes nothing. Muster's own
// origin (Origin, or Referer when Origin is absent) is admitted.
func TestConnectorOperatorWritesRefuseCrossOrigin(t *testing.T) {
	if got := registeredConnectorWrites(t); strings.Join(got, "\n") != strings.Join(connectorOperatorWrites, "\n") {
		t.Fatalf("the connector write routes changed; update the ledger AND check each is requireOperatorWrite.\n"+
			"registered:\n%s\nledger:\n%s", strings.Join(got, "\n"), strings.Join(connectorOperatorWrites, "\n"))
	}
	f := newConnFixture(t)
	id := f.seedAgent(t, "csrf-agent", "csrf-agent-token-0123456789")
	if _, err := f.store.CreateConnector(f.ctx, connectors.NewConnector{Name: "svc", Hosts: []string{"api.example.com"},
		Scheme: connectors.SchemeBearer, Secret: connTestSecret}); err != nil {
		t.Fatal(err)
	}
	g, _, err := f.store.RequestGrant(f.ctx, id, "svc", "")
	if err != nil || g.ID != 1 {
		t.Fatalf("grant %+v %v", g, err)
	}
	session := sessionCookieFor(f.srv)
	for _, route := range connectorOperatorWrites {
		method, path := concrete(route)
		for name, hdr := range map[string]map[string]string{
			"sibling subdomain": {"Origin": "https://evil.example.com"},
			"other port":        {"Origin": "http://example.com:8080"},
			"null origin":       {"Origin": "null"},
			"no origin":         {},
			"cross referer":     {"Referer": "https://evil.example.com/page"},
			"origin wins":       {"Origin": "https://evil.example.com", "Referer": "http://example.com/connectors"},
		} {
			hdr = merge(hdr, map[string]string{"Cookie": session, "HX-Request": "true",
				"Content-Type": "application/x-www-form-urlencoded"})
			w := f.call(t, method, path, strings.NewReader("secret=ghp_FAKE-csrf-0123456789&name=forged&hosts=evil.example.org&scheme=bearer"), hdr)
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "cross-origin") {
				t.Errorf("%s (%s): status %d %.120s, want 403 cross-origin", route, name, w.Code, w.Body)
			}
		}
	}
	// Nothing changed.
	if gg, _ := f.store.GetGrant(f.ctx, g.ID); gg.Status != connectors.StatusPending {
		t.Fatalf("a refused request changed the grant to %s", gg.Status)
	}
	if list, _ := f.store.ListConnectors(f.ctx); len(list) != 1 {
		t.Fatalf("a refused request changed the connector list: %+v", list)
	}
	// Muster's own origin is admitted: Origin, or a same-origin Referer alone.
	for _, hdr := range []map[string]string{{"Origin": "http://example.com"}, {"Referer": "http://example.com/connectors"}} {
		w := f.call(t, "GET", "/ui/connectors", nil, merge(hdr, map[string]string{"Cookie": session}))
		if w.Code != 200 {
			t.Fatalf("control GET: %d", w.Code)
		}
		w = f.call(t, "POST", "/ui/connector-requests/999/deny", nil, merge(hdr, map[string]string{"Cookie": session, "HX-Request": "true"}))
		if w.Code == http.StatusForbidden || strings.Contains(w.Body.String(), "cross-origin") {
			t.Fatalf("same-origin %v was refused: %d %s", hdr, w.Code, w.Body)
		}
	}
	w := f.call(t, "POST", "/ui/connector-requests/1/approve", nil,
		map[string]string{"Cookie": session, "HX-Request": "true", "Origin": "http://example.com"})
	if gg, _ := f.store.GetGrant(f.ctx, g.ID); w.Code != 200 || gg.Status != connectors.StatusApproved {
		t.Fatalf("same-origin approve: %d, grant %s", w.Code, gg.Status)
	}
}

// TestAnAgentPastThePendingCapIsTold429: the explicit request route refuses past
// the per-agent cap with a message that says why, and files nothing.
func TestAnAgentPastThePendingCapIsTold429(t *testing.T) {
	const capPerAgent = 5
	f := newConnFixture(t)
	const tok = "cap-agent-token-0123456789"
	f.seedAgent(t, "cap-agent", tok)
	agent := map[string]string{"Authorization": "Bearer " + tok}
	for i := 0; i < capPerAgent; i++ {
		if w := f.call(t, "POST", fmt.Sprintf("/agent/connectors/want-%d/request", i), nil, agent); w.Code != http.StatusAccepted {
			t.Fatalf("request %d: %d %s", i, w.Code, w.Body)
		}
	}
	w := f.call(t, "POST", "/agent/connectors/one-more/request", nil, agent)
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "waiting") {
		t.Fatalf("past the cap: %d %s, want 429", w.Code, w.Body)
	}
	pending, _ := f.store.ListPendingGrants(f.ctx)
	if len(pending) != capPerAgent {
		t.Fatalf("%d pending, want %d", len(pending), capPerAgent)
	}
}

// TestTheDocumentedRecipeSendsTheTokenAsBearer: the instruction an approved agent
// is given carries its muster token in Authorization (which curl drops on a
// cross-host redirect), not in a custom header (which it resends).
func TestTheDocumentedRecipeSendsTheTokenAsBearer(t *testing.T) {
	u := ProxyUsage("svc", []string{"api.example.com"})
	if !strings.Contains(u, "`Authorization: Bearer $MUSTER_HOOK_TOKEN`") || strings.Contains(u, "X-Muster-Token") {
		t.Fatalf("ProxyUsage = %q", u)
	}
}

// TestCurlFollowingTheRecipeNeverSendsItsTokenOffHost drives REAL curl -L the
// way an agent would, with the header from ProxyUsage and, separately, with
// X-Muster-Token, through the real handler. The upstream redirects to another
// host; that host must never see a request carrying the agent's token.
func TestCurlFollowingTheRecipeNeverSendsItsTokenOffHost(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Fatal("curl is required for this test (it is on every CI runner): ", err)
	}
	const token = "redirect-agent-token-0123456789"
	var mu sync.Mutex
	var offHost []string
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		offHost = append(offHost, fmt.Sprintf("%v", r.Header))
		mu.Unlock()
		_, _ = io.WriteString(w, "stolen")
	}))
	t.Cleanup(evil.Close)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+"/steal", http.StatusFound)
	}))
	t.Cleanup(up.Close)
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
	host := "api.fake.test:" + port

	f := newConnFixture(t)
	f.srv = New(nil, AuthConfig{HookToken: connTestHookToken, ServiceToken: connTestServiceToken, UIPassword: connTestPassword},
		log.New(io.Discard, "", 0))
	f.srv.UseExtensions(Extensions{Agents: agents.NewPG(f.pool), Connectors: f.store, ConnectorPolicy: connectors.Policy{
		AllowLoopback: true, InsecureHTTP: true,
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
	}})
	front := httptest.NewServer(f.srv.Handler())
	t.Cleanup(front.Close)
	id := f.seedAgent(t, "redirect-agent", token)
	if _, err := f.store.CreateConnector(f.ctx, connectors.NewConnector{Name: "svc", Hosts: []string{host},
		Scheme: connectors.SchemeBearer, Secret: connTestSecret}); err != nil {
		t.Fatal(err)
	}
	g, _, _ := f.store.RequestGrant(f.ctx, id, "svc", "")
	if _, err := f.store.DecideGrant(f.ctx, g.ID, true, "operator"); err != nil {
		t.Fatal(err)
	}

	m := regexp.MustCompile("`([A-Za-z-]+): ([^`]*)`").FindStringSubmatch(ProxyUsage("svc", []string{host}))
	if m == nil {
		t.Fatal("ProxyUsage names no header")
	}
	recipe := m[1] + ": " + strings.ReplaceAll(m[2], "$MUSTER_HOOK_TOKEN", token)
	for _, hdr := range []string{recipe, "X-Muster-Token: " + token} {
		out, err := exec.Command(curl, "-sS", "-L", "--max-redirs", "3", "-o", "/dev/null", "-w", "%{http_code}",
			"-H", hdr, front.URL+"/connect/svc/"+host+"/archive").CombinedOutput()
		if err != nil && !strings.Contains(string(out), "502") {
			t.Logf("curl (%s): %v %s", strings.SplitN(hdr, ":", 2)[0], err, out)
		}
		if code := strings.TrimSpace(string(out)); code != "502" {
			t.Errorf("curl with %s: final status %s, want 502 (no redirect handed out)", strings.SplitN(hdr, ":", 2)[0], code)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, h := range offHost {
		if strings.Contains(h, token) {
			t.Fatalf("the redirect target received the agent's muster token: %s", h)
		}
	}
	if len(offHost) != 0 {
		t.Fatalf("the redirect target was reached %d time(s): %v", len(offHost), offHost)
	}
}
