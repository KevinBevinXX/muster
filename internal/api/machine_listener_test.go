package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
)

// The SHARED machine credentials (MUSTER_HOOK_TOKEN, MUSTER_SERVICE_TOKEN) act as
// user 1, the admin, and are held by every hook script on every host. The browser
// listener (MUSTER_BROWSER_PORT) is the one the public edge routes to, so on it they
// are refused outright; only a PERSONAL token (mst_) still opens the /api/ tier
// there. These tests drive every hook/service route from routeVias, so a new route
// on either tier is covered without editing this file.

// newMachineListenerTestServer is the principal-probe server in GOTRUE mode — the
// only mode the refusal applies in — with both shared secrets armed.
func newMachineListenerTestServer(t *testing.T) (*Server, http.Handler, *[]authz.Principal, *bytes.Buffer) {
	t.Helper()
	s, h, seen := newPrincipalTestServerWith(t, AuthConfig{
		Mode: AuthModeGoTrue, HookToken: pHookToken, ServiceToken: pServiceToken,
	})
	logs := &bytes.Buffer{}
	s.logger = log.New(logs, "", 0)
	return s, h, seen, logs
}

// sharedTokenRoutes is every (door, route) pair a shared secret opens, sorted.
func sharedTokenRoutes() [][2]string {
	var out [][2]string
	for _, via := range []authz.Via{authz.ViaHook, authz.ViaService} {
		for _, p := range routeVias[via] {
			out = append(out, [2]string{string(via), p})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0]+out[i][1] < out[j][0]+out[j][1] })
	return out
}

// assertSharedTokenRefusal checks one response is THE refusal: 403, the exported
// sentence, and nothing handed to a handler.
func assertSharedTokenRefusal(t *testing.T, rec *httptest.ResponseRecorder, aborted bool, seen []authz.Principal) {
	t.Helper()
	if aborted || len(seen) != 0 {
		t.Fatalf("the request reached a handler (principal %v); want it refused before admit", seen)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d; want 403 (body %q)", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %q", rec.Body.String())
	}
	if got := body["error"]; got != SharedTokenOffMachineListenerRefusal {
		t.Fatalf("error = %q; want %q", got, SharedTokenOffMachineListenerRefusal)
	}
}

// serveRec is serve() that also returns the recorder, for body assertions.
func serveRec(t *testing.T, h http.Handler, req *http.Request) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	rec := httptest.NewRecorder()
	aborted := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				aborted = r == errProbeAbort //nolint:errorlint // identity of the sentinel
				if !aborted {
					t.Logf("%s %s: the handler ran and panicked: %v", req.Method, req.URL.Path, r)
					rec.Code = http.StatusInternalServerError
				}
			}
		}()
		h.ServeHTTP(rec, req)
	}()
	return rec, aborted
}

func TestSharedMachineTokensAreRefusedOnTheBrowserListener(t *testing.T) {
	s, h, seen, logs := newMachineListenerTestServer(t)
	browser := BrowserListener(h)
	pairs := sharedTokenRoutes()
	for _, pr := range pairs {
		via, pattern := authz.Via(pr[0]), pr[1]
		method, path := concretePath(pattern)
		t.Run(string(via)+" "+pattern, func(t *testing.T) {
			*seen = nil
			req := httptest.NewRequest(method, path, strings.NewReader(""))
			credential(s, req, via)
			rec, aborted := serveRec(t, browser, req)
			assertSharedTokenRefusal(t, rec, aborted, *seen)
		})
	}
	// Positive control on the walk: both tiers were actually driven.
	if len(pairs) < len(routeVias[authz.ViaHook])+len(routeVias[authz.ViaService]) || len(routeVias[authz.ViaService]) == 0 {
		t.Fatalf("drove %d pairs; the table has %d hook and %d service routes",
			len(pairs), len(routeVias[authz.ViaHook]), len(routeVias[authz.ViaService]))
	}
	// The refusal is logged without the credential, so a straggling client is findable.
	if !strings.Contains(logs.String(), SharedTokenRefusalLogPrefix) {
		t.Fatalf("no %q line was logged", SharedTokenRefusalLogPrefix)
	}
	if strings.Contains(logs.String(), pHookToken) || strings.Contains(logs.String(), pServiceToken) {
		t.Fatalf("a shared secret was written to the log")
	}
}

// TestSharedMachineTokensStillOpenTheMachineListener is the positive control for
// the test above, on the same table with the same credentials: without it, that
// test passes on a server that refuses everything.
func TestSharedMachineTokensStillOpenTheMachineListener(t *testing.T) {
	s, h, seen, _ := newMachineListenerTestServer(t)
	for _, pr := range sharedTokenRoutes() {
		via, pattern := authz.Via(pr[0]), pr[1]
		method, path := concretePath(pattern)
		t.Run(string(via)+" "+pattern, func(t *testing.T) {
			*seen = nil
			req := httptest.NewRequest(method, path, strings.NewReader(""))
			credential(s, req, via)
			rec, aborted := serveRec(t, h, req)
			if !aborted || len(*seen) != 1 {
				t.Fatalf("not admitted on the machine listener (status %d, body %q, saw %v)", rec.Code, rec.Body.String(), *seen)
			}
			want := authz.Principal{UserID: authz.OperatorUserID, Role: authz.RoleAdmin, Via: via}
			if got := (*seen)[0]; got != want {
				t.Fatalf("principal = %v; want %v", got, want)
			}
		})
	}
}

// TestPersonalTokensStillOpenTheBrowserListener pins that the refusal sits AFTER
// the mst_ door: an off-LAN member's CLI keeps working through the public edge.
func TestPersonalTokensStillOpenTheBrowserListener(t *testing.T) {
	s, h, seen, _ := newMachineListenerTestServer(t)
	browser := BrowserListener(h)
	for _, pattern := range routeVias[authz.ViaToken] {
		method, path := concretePath(pattern)
		t.Run(pattern, func(t *testing.T) {
			*seen = nil
			req := httptest.NewRequest(method, path, strings.NewReader(""))
			credential(s, req, authz.ViaToken)
			rec, aborted := serveRec(t, browser, req)
			want := authz.Principal{UserID: authz.OperatorUserID, Role: authz.RoleAdmin, Via: authz.ViaToken}
			if !aborted || len(*seen) != 1 || (*seen)[0] != want {
				t.Fatalf("personal token on the browser listener: status %d body %q saw %v; want %v",
					rec.Code, rec.Body.String(), *seen, want)
			}
		})
	}
}

// TestTheBrowserListenerRefusalIsNotAnOracle: the refusal is decided before the
// presented secret is compared, so a WRONG shared token, and no token at all, get
// byte-for-byte the answer a RIGHT one does. A 401-vs-403 split would let anyone on
// the public port test guesses against the admin secret.
func TestTheBrowserListenerRefusalIsNotAnOracle(t *testing.T) {
	_, h, seen, _ := newMachineListenerTestServer(t)
	browser := BrowserListener(h)
	for _, pr := range sharedTokenRoutes() {
		pattern := pr[1]
		method, path := concretePath(pattern)
		var bodies []string
		for _, set := range []func(*http.Request){
			func(r *http.Request) { r.Header.Set("X-Muster-Token", pHookToken) },
			func(r *http.Request) { r.Header.Set("X-Muster-Token", "wrong-"+pHookToken) },
			func(r *http.Request) { r.Header.Set("Authorization", "bEaReR "+pServiceToken) },
			func(r *http.Request) { r.Header.Set("Authorization", "Bearer wrong-"+pServiceToken) },
			func(*http.Request) {},
		} {
			*seen = nil
			req := httptest.NewRequest(method, path, strings.NewReader(""))
			set(req)
			rec, aborted := serveRec(t, browser, req)
			t.Run(pr[0]+" "+pattern, func(t *testing.T) { assertSharedTokenRefusal(t, rec, aborted, *seen) })
			b, _ := io.ReadAll(rec.Body)
			bodies = append(bodies, rec.Result().Status+string(b))
		}
		for i := 1; i < len(bodies); i++ {
			if bodies[i] != bodies[0] {
				t.Errorf("%s: responses differ by credential: %q vs %q", pattern, bodies[0], bodies[i])
			}
		}
	}
}

// TestRequireServiceTokenRefusesOnTheBrowserListener drives requireServiceToken
// DIRECTLY. No route is registered behind it today (every service-tier route uses
// requireHookOrServiceToken), so the routeVias walks cannot reach its guard; this
// pins it for the first route that is. Machine-listener control included.
func TestRequireServiceTokenRefusesOnTheBrowserListener(t *testing.T) {
	s, _, seen, _ := newMachineListenerTestServer(t)
	h := s.requireServiceToken(func(http.ResponseWriter, *http.Request) {
		t.Error("the handler ran: admit should have handed off to the principal probe")
	})
	req := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
		credential(s, r, authz.ViaService)
		return r
	}
	*seen = nil
	rec, aborted := serveRec(t, BrowserListener(h), req())
	assertSharedTokenRefusal(t, rec, aborted, *seen)

	*seen = nil
	rec, aborted = serveRec(t, h, req())
	want := authz.Principal{UserID: authz.OperatorUserID, Role: authz.RoleAdmin, Via: authz.ViaService}
	if !aborted || len(*seen) != 1 || (*seen)[0] != want {
		t.Fatalf("machine listener: status %d body %q saw %v; want %v", rec.Code, rec.Body.String(), *seen, want)
	}
}

// TestPasswordModeBrowserListenerIsUnchanged pins the gotrue-only scope: a
// password-mode server serves the hook tier on a browser-marked request as before.
func TestPasswordModeBrowserListenerIsUnchanged(t *testing.T) {
	s, h, seen := newPrincipalTestServer(t)
	browser := BrowserListener(h)
	for _, pattern := range routeVias[authz.ViaHook] {
		method, path := concretePath(pattern)
		t.Run(pattern, func(t *testing.T) {
			*seen = nil
			req := httptest.NewRequest(method, path, strings.NewReader(""))
			credential(s, req, authz.ViaHook)
			rec, aborted := serveRec(t, browser, req)
			want := authz.Principal{UserID: authz.OperatorUserID, Role: authz.RoleAdmin, Via: authz.ViaHook}
			if !aborted || len(*seen) != 1 || (*seen)[0] != want {
				t.Fatalf("password mode, browser listener: status %d body %q saw %v; want %v",
					rec.Code, rec.Body.String(), *seen, want)
			}
		})
	}
}
