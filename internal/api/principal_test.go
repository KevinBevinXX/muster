package api

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/apitokens"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/flows"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
)

// Accounts PR 2/6: every authenticated route reaches its handler carrying an
// authz.Principal, and the Principal names the door that admitted it.
//
// 🔴 THE CLASSIFICATION BELOW IS EXHAUSTIVE OVER routes.golden, BOTH WAYS. A
// route in the golden that is not classified fails (a new route must say
// which door it is behind, or that it is deliberately public); a route
// classified here that is not in the golden fails (a stale entry would keep
// vouching for a route that no longer exists). Public is an ALLOWLIST, not
// the default.

const (
	pHookToken    = "principal-test-hook-token-0123456789abcdef"
	pServiceToken = "principal-test-service-token-0123456789abcd"
	pPassword     = "principal-test-operator-password"
	// pAPIToken is shaped like a minted personal token (apitokens.ShapeOK) so
	// it reaches the store; principalAPITokens resolves it to user 1.
	pAPIToken   = "mst_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	pAgentToken = "principal-test-agent-own-token-0123456789"
	// pRunnerToken is shaped like a minted flow runner token
	// (flows.RunnerTokenShapeOK); principalFlows resolves it to user 1.
	pRunnerToken = "mfr_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	// pPluginToken is shaped like a minted plugin token (plugins.TokenShapeOK);
	// principalPlugins resolves it to user 1.
	pPluginToken = "mpl_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	// A mirror host token's shape (mirror.TokenShapeOK): mmt_ + 43 base64url.
	pMirrorToken = "mmt_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

// publicRoutes are served with no credential and therefore no Principal.
var publicRoutes = []string{
	// The admin claim (accounts PR 4): how an anonymous visitor holding a
	// single-use link stops being anonymous, like /login.
	"GET /auth/claim",
	"POST /auth/claim",
	// Invite redemption and the password reset (accounts PR 5): the same
	// shape — a single-use link held by a visitor with no session yet.
	"GET /invite",
	"POST /invite",
	"GET /auth/reset",
	"POST /auth/reset",
	// Passkey sign-in: like POST /login, how a visitor with no session stops
	// being anonymous (browser listener + same-origin only).
	"POST /auth/passkey/options",
	"POST /auth/passkey/verify",
	"GET /apple-touch-icon-precomposed.png",
	"GET /apple-touch-icon.png",
	"GET /favicon.ico",
	"GET /health",
	"GET /login",
	"GET /manifest.webmanifest",
	"GET /metrics",
	// ⚠ NOT UNAUTHENTICATED: the mirror plugin's host connection. No muster door
	// admits it and no request Principal is attached — internal/mirror
	// authenticates its own per-host mirror token (mirror_tokens, looked up under
	// authz.System), and the token's OWNER is what its sessions are registered
	// under. It 404s with the plugin off.
	"GET /mirror/connect",
	// ⚠ NOT UNAUTHENTICATED either: the AskUserQuestion hook's door
	// (mirror_ask.go). Like /mirror/connect, the plugin authenticates its own
	// mirror token (mirror.Service.AuthenticateHost); no request Principal.
	"POST /mirror/ask",
	"GET /readyz",
	// ⚠ NOT UNAUTHENTICATED EITHER: a file attachment's signed link (uploads.go).
	// No door admits it and no Principal is attached — the HMAC over the id and
	// expiry in its query IS the credential (internal/uploads.Signer.Verify),
	// and the one row it names is read under the system principal after that.
	// It 404s with uploads off.
	"GET /f/{id}",
	"GET /static/",
	"GET /sw.js",
	"POST /login",
	"POST /logout",
}

// routeVias is, for every non-public route, each door that admits it.
// A route behind requireHookOrServiceToken has two.
var routeVias = map[authz.Via][]string{
	authz.ViaSession: {
		// Plugins (0015): the page and its forms (404 while plugins are off).
		"GET /plugins",
		"POST /plugins",
		"POST /plugins/{id}/revoke",
		"POST /plugins/drafts/{id}/approve",
		"POST /plugins/drafts/{id}/reject",
		// The attention queue (0023): the page, its list, the badge and the forms.
		"GET /attention",
		"GET /attention/items",
		"GET /attention/badge",
		"POST /attention/asks/{id}/answer",
		"POST /attention/asks/{id}/cancel",
		"GET /account",
		// The step-up confirm (stepup.go): the SESSION's user, re-proven.
		"POST /auth/confirm",
		// …and its passkey form: the same session's user, re-proven by a
		// passkey that must be THAT user's (handleStepUpPasskeyVerify).
		"POST /auth/confirm/passkey/options",
		"POST /auth/confirm/passkey/verify",
		"GET /account/credentials",
		"POST /account/credentials/claude",
		"POST /account/credentials/claude/{name}/delete",
		"POST /account/credentials/openrouter",
		"POST /account/credentials/openrouter/delete",
		"GET /account/tokens",
		"POST /account/tokens",
		"POST /account/tokens/{id}/revoke",
		// Passkeys: the caller's own, acted on AS the caller at GoTrue.
		"GET /account/passkeys",
		"POST /account/passkeys/options",
		"POST /account/passkeys/verify",
		"POST /account/passkeys/{id}/delete",
		"POST /admin/users/{id}/passkeys/delete",
		// muster-native Web Push: the subscribe flow and the account page's
		// Notifications section — the session tier only.
		"GET /api/push/vapid-public-key",
		"POST /api/push/subscribe",
		"POST /api/push/unsubscribe",
		"POST /account/push/test",
		"POST /account/push/{id}/remove",
		"POST /account/push/preferences",
		// The admin pages (accounts PR 5): requireAdmin is requireSession plus
		// a role check AFTER the principal is attached.
		"GET /admin/invites",
		"GET /admin/users",
		"POST /admin/invites",
		"POST /admin/invites/{id}/revoke",
		"POST /admin/users/{id}/disable",
		"POST /admin/users/{id}/enable",
		"POST /admin/users/{id}/reset-link",
		"POST /admin/users/{id}/role",
		"DELETE /agents/{id}",
		"GET /mirror/terminal",
		"GET /mirror/terminal/ws",
		"GET /mirror/view",
		"GET /mirror/view/ws",
		// The composer menu's data (mirror_complete.go): the session tier.
		"GET /mirror/view/complete",
		"POST /mirror/view/send",
		"POST /mirror/terminal/send",
		"POST /mirror/terminal/scroll",
		"GET /mirror/view/send",
		"GET /mirror/terminal/send",
		"POST /mirror/confirm-send",
		// AskUserQuestion: the live region and the answers (mirror_ask.go).
		"GET /mirror/view/asks",
		"POST /mirror/view/ask",
		"GET /agents/{name}/send",
		"POST /agents/{name}/send",
		// Dictation: the session tier only (requireOperatorWrite), like a send.
		"POST /transcribe",
		// File attachments (uploads.go): the upload and the revoke are
		// requireOperatorWrite, the owner's view requireSession.
		"POST /uploads",
		"GET /uploads/{id}/file",
		"POST /uploads/{id}/revoke",
		"DELETE /connector-grants/{id}",
		"DELETE /connectors/{name}",
		"DELETE /tasks/{id}",
		"DELETE /tasks/{id}/comments/{cid}",
		"DELETE /tasks/{id}/tags/{tag}",
		"GET /agents",
		"GET /agents/{name}",
		"GET /agents/{name}/logs/stream",
		"GET /agents/{name}/terminal",
		"GET /agents/{name}/terminal/ws",
		"GET /agents/{name}/ws",
		"GET /api/directories",
		"GET /api/openrouter/models",
		"GET /connectors",
		"GET /events",
		"GET /github/callback",
		"GET /github/connect",
		"GET /repos",
		"GET /share",
		"GET /tasks",
		"GET /tasks/{id}",
		"GET /tasks/{id}/attachments/{aid}",
		"GET /ui/agents",
		"GET /ui/agents/new",
		"GET /ui/agents/notes",
		"GET /ui/agents/repos",
		"GET /ui/agents/{id}/card",
		"GET /ui/focus/history",
		"GET /ui/agents/{id}/rename",
		"GET /ui/agents/{id}/status",
		"GET /ui/agents/{name}/chat-log",
		"GET /ui/agents/{name}/task",
		"GET /ui/chief/panel",
		"GET /ui/chief/threads/search",
		"GET /ui/connector-requests",
		"GET /ui/connectors",
		"GET /ui/connectors/new",
		"GET /ui/connectors/{name}/rotate",
		"GET /ui/notifications",
		"GET /ui/repos",
		"GET /ui/tags",
		"GET /ui/tasks",
		"GET /ui/tasks/new",
		"GET /ui/tasks/review-count",
		"GET /ui/tasks/{id}/card",
		"GET /ui/tasks/{id}/edit",
		"GET /{$}",
		"PATCH /tasks/{id}/status",
		"POST /agents",
		"POST /agents/pins",
		"POST /agents/pins/delete",
		"POST /send-queue",
		"POST /send-queue/{id}/cancel",
		"POST /send-queue/confirm",
		"GET /send-queue",
		"POST /agents/sessions/{sid}/read",
		"POST /agents/{id}/model",
		"POST /agents/{id}/name",
		"POST /agents/{id}/sessions",
		"POST /agents/{id}/start",
		"POST /agents/{id}/stop",
		"POST /chief/provision",
		"POST /connectors",
		"POST /connectors/{name}/rotate",
		"POST /github/disconnect",
		"POST /tasks",
		"POST /tasks/merge",
		"POST /tasks/{id}/comments",
		"POST /tasks/{id}/edit",
		"POST /tasks/{id}/tags",
		"POST /ui/connector-requests/{id}/approve",
		"POST /ui/connector-requests/{id}/deny",
		// Browser flows (0012): the page and its forms.
		"GET /flows",
		"POST /flows/jobs",
		"POST /flows/jobs/{id}/cancel",
		"POST /flows/runner-tokens",
		"POST /flows/runner-tokens/{id}/revoke",
		"POST /flows/requests/{id}/approve",
		"POST /flows/requests/{id}/deny",
	},
	authz.ViaHook: {
		"DELETE /api/tasks/{id}",
		"DELETE /api/tasks/{id}/comments/{cid}",
		"GET /api/agents",
		"GET /api/agents/{name}/messages",
		"GET /api/projects",
		"GET /api/sessions/{id}/tasks",
		"GET /api/tags",
		"GET /api/tasks",
		"GET /api/tasks/{id}",
		"PATCH /api/tasks/{id}",
		"PATCH /api/tasks/{id}/status",
		"POST /api/agents/{name}/messages",
		"POST /api/tasks",
		"POST /api/tasks/{id}/comments",
		"GET /api/whoami",
		"POST /api/routes/{id}/reply",
	},
	authz.ViaService: {
		"GET /api/tasks",
		"GET /api/whoami",
		"PATCH /api/tasks/{id}/status",
	},
	// A PERSONAL token (accounts PR 6) opens every hook-tier route (each
	// hook-tier wrapper takes one first) plus the token API, acting as its
	// owner — user 1 here, because principalAPITokens says so.
	authz.ViaToken: {
		"DELETE /api/tasks/{id}",
		"DELETE /api/tasks/{id}/comments/{cid}",
		"DELETE /api/tokens/{id}",
		"GET /api/agents",
		"GET /api/agents/{name}/messages",
		"GET /api/projects",
		"GET /api/sessions/{id}/tasks",
		"GET /api/tags",
		"GET /api/tasks",
		"GET /api/tasks/{id}",
		"GET /api/tokens",
		"GET /api/whoami",
		"PATCH /api/tasks/{id}",
		"PATCH /api/tasks/{id}/status",
		"POST /api/agents/{name}/messages",
		"POST /api/tasks",
		"POST /api/tasks/{id}/comments",
		"POST /api/routes/{id}/reply",
		// Browser flows (0012): a personal token ONLY — the hook and service
		// tiers are absent above on purpose (requireAPIToken).
		"GET /api/flows/jobs",
		"GET /api/flows/jobs/{id}",
		"POST /api/flows/jobs",
	},
	// A browser-flow RUNNER token (0012) opens exactly the runner door.
	authz.ViaRunner: {
		"POST /runner/flows/claim",
		"POST /runner/flows/{id}/heartbeat",
		"POST /runner/flows/{id}/result",
	},
	// A MIRROR HOST token opens exactly the mirrored sessions' MCP door.
	authz.ViaMirrorHost: {
		"POST /mirror/mcp",
	},
	// A PLUGIN token (0015) opens exactly the plugin door.
	authz.ViaPlugin: {
		"POST /plugin/v1/messages",
		"POST /plugin/v1/drafts/claim",
		"POST /plugin/v1/drafts/{id}/result",
		"POST /plugin/v1/tasks",
	},
	authz.ViaAgent: {
		"GET /agent/muster",
		"GET /agent/task",
		"PATCH /agent/task/status",
		"POST /agent/task/comment",
	},
	authz.ViaConnect: {
		"/connect/{connector}/{host}/{path...}",
		"GET /agent/connectors",
		"GET /agent/connectors/{name}",
		"POST /agent/connectors/{name}/request",
		"POST /mcp",
	},
	authz.ViaRelay: {
		"DELETE /agent/approvals/{id}",
		"GET /agent/approvals/{id}",
		"POST /agent/approvals",
		"POST /agent/blocked",
	},
}

// principalAgents resolves the one test agent by its own token. Every other
// method is the nil embedded interface: the probe aborts the request before
// any handler could call one.
type principalAgents struct{ agents.Store }

func (principalAgents) GetByHooksToken(_ context.Context, tok string) (agents.Agent, error) {
	if tok == pAgentToken {
		return agents.Agent{ID: 7, Name: "principal-test-agent", OwnerID: authz.OperatorUserID}, nil
	}
	return agents.Agent{}, errors.New("no such agent")
}

type principalConnectors struct{ connectors.Store }

// principalFlows resolves the one test runner token to user 1.
type principalFlows struct{ flows.Store }

func (principalFlows) AuthenticateRunner(_ context.Context, tok string) (flows.RunnerToken, error) {
	if tok == pRunnerToken {
		return flows.RunnerToken{ID: 5, OwnerID: authz.OperatorUserID, Host: "principal-test"}, nil
	}
	return flows.RunnerToken{}, flows.ErrBadRunnerToken
}

// principalPlugins resolves the one test plugin token to user 1.
type principalPlugins struct{ plugins.Store }

func (principalPlugins) Authenticate(_ context.Context, tok string) (plugins.Plugin, error) {
	if tok == pPluginToken {
		return plugins.Plugin{ID: 9, OwnerID: authz.OperatorUserID, Name: "principal-test"}, nil
	}
	return plugins.Plugin{}, plugins.ErrBadToken
}

// principalMirrorTokens resolves the one test mirror token to user 1's host.
type principalMirrorTokens struct{}

func (principalMirrorTokens) Authenticate(_ context.Context, tok string) (mirror.Token, error) {
	if tok == pMirrorToken {
		return mirror.Token{ID: 4, OwnerID: authz.OperatorUserID, Host: "principal-test"}, nil
	}
	return mirror.Token{}, mirror.ErrBadToken
}

// principalAPITokens resolves the one test personal token to user 1.
type principalAPITokens struct{ apitokens.Store }

func (principalAPITokens) Authenticate(_ context.Context, tok string) (apitokens.Token, error) {
	if tok == pAPIToken {
		return apitokens.Token{ID: 3, UserID: authz.OperatorUserID, Name: "principal-test"}, nil
	}
	return apitokens.Token{}, apitokens.ErrBadToken
}

// errProbeAbort stops a request at admit's hand-off, so no handler body runs:
// the test observes the principal, not the handler's dependencies.
var errProbeAbort = errors.New("principal probe: abort before the handler")

// concretePath turns a mux pattern into a request path that matches it.
var wildcard = regexp.MustCompile(`\{[^}]*\}`)

func concretePath(pattern string) (method, path string) {
	method, path = http.MethodGet, pattern
	if i := strings.IndexByte(pattern, ' '); i > 0 {
		method, path = pattern[:i], pattern[i+1:]
	}
	path = strings.ReplaceAll(path, "{$}", "")
	path = wildcard.ReplaceAllString(path, "x1")
	if strings.HasSuffix(path, "/") && path != "/" {
		path += "x1"
	}
	return method, path
}

func newPrincipalTestServer(t *testing.T) (*Server, http.Handler, *[]authz.Principal) {
	t.Helper()
	return newPrincipalTestServerWith(t, AuthConfig{HookToken: pHookToken, ServiceToken: pServiceToken, UIPassword: pPassword})
}

func newPrincipalTestServerWith(t *testing.T, cfg AuthConfig) (*Server, http.Handler, *[]authz.Principal) {
	t.Helper()
	s := New(nil, cfg, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Agents: principalAgents{}, Connectors: principalConnectors{}, APITokens: principalAPITokens{},
		Flows: principalFlows{}, Plugins: principalPlugins{}, MirrorTokens: principalMirrorTokens{}})
	seen := &[]authz.Principal{}
	s.principalProbe = func(r *http.Request) {
		p, ok := authz.From(r.Context())
		if !ok {
			t.Errorf("%s %s: admit handed on a request with no principal", r.Method, r.URL.Path)
		}
		*seen = append(*seen, p)
		panic(errProbeAbort)
	}
	mux := http.NewServeMux()
	s.registerAll(mux)
	return s, mux, seen
}

// serve runs one request and reports whether it was stopped by the probe.
func serve(t *testing.T, h http.Handler, req *http.Request) (aborted bool, code int) {
	t.Helper()
	rec := httptest.NewRecorder()
	func() {
		defer func() {
			// Any OTHER panic means a handler body ran — against the fakes'
			// nil methods — which is itself the observation "no principal was
			// handed on". It is reported as not-aborted so the caller's own
			// assertion names the failure, rather than crashing the binary.
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
	return aborted, rec.Code
}

func credential(s *Server, req *http.Request, via authz.Via) {
	switch via {
	case authz.ViaSession:
		req.Header.Set("Cookie", sessionCookieFor(s))
		req.Header.Set("Origin", "http://"+req.Host) // requireOperatorWrite's same-origin half
	case authz.ViaHook:
		req.Header.Set("X-Muster-Token", pHookToken)
	case authz.ViaService:
		req.Header.Set("Authorization", "Bearer "+pServiceToken)
		req.Header.Set(ActorHeader, "principal-test-service")
	case authz.ViaAgent, authz.ViaConnect, authz.ViaRelay:
		req.Header.Set("X-Muster-Token", pAgentToken)
	case authz.ViaToken:
		req.Header.Set("Authorization", "Bearer "+pAPIToken)
	case authz.ViaRunner:
		req.Header.Set("Authorization", "Bearer "+pRunnerToken)
	case authz.ViaPlugin:
		req.Header.Set("Authorization", "Bearer "+pPluginToken)
	case authz.ViaMirrorHost:
		req.Header.Set("Authorization", "Bearer "+pMirrorToken)
	}
}

func TestEveryRouteIsClassifiedForItsPrincipal(t *testing.T) {
	golden := map[string]bool{}
	for _, r := range readGolden(t) {
		golden[r] = true
	}
	classified := map[string]bool{}
	for _, r := range publicRoutes {
		classified[r] = true
	}
	for _, rs := range routeVias {
		for _, r := range rs {
			classified[r] = true
		}
	}
	var unclassified, stale []string
	for r := range golden {
		if !classified[r] {
			unclassified = append(unclassified, r)
		}
	}
	for r := range classified {
		if !golden[r] {
			stale = append(stale, r)
		}
	}
	sort.Strings(unclassified)
	sort.Strings(stale)
	if len(unclassified) > 0 {
		t.Errorf("routes in %s with no principal classification (add each to routeVias under the door "+
			"that admits it, or to publicRoutes if it is deliberately unauthenticated):\n  %s",
			goldenPath, strings.Join(unclassified, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("classified routes that are not in %s:\n  %s", goldenPath, strings.Join(stale, "\n  "))
	}
	for _, r := range publicRoutes {
		for via, rs := range routeVias {
			for _, x := range rs {
				if x == r {
					t.Errorf("%s is listed public AND behind %s", r, via)
				}
			}
		}
	}
}

// TestEveryAuthenticatedRouteAttachesAPrincipal drives each classified route
// through its REAL registered wrapper chain with that door's credential and
// observes the principal at admit's hand-off to the handler.
//
// Per route and door it asserts: (1) with the credential, exactly one
// principal reaches the hand-off, and it is user 1 / admin / THAT door; (2)
// with NO credential, nothing reaches the hand-off — which is what makes (1)
// a fact about the credential rather than about a route that admits anyone.
func TestEveryAuthenticatedRouteAttachesAPrincipal(t *testing.T) {
	s, h, seen := newPrincipalTestServer(t)
	vias := make([]string, 0, len(routeVias))
	for v := range routeVias {
		vias = append(vias, string(v))
	}
	sort.Strings(vias)
	checked := 0
	for _, v := range vias {
		via := authz.Via(v)
		for _, pattern := range routeVias[via] {
			method, path := concretePath(pattern)
			t.Run(string(via)+" "+pattern, func(t *testing.T) {
				*seen = nil
				bare := httptest.NewRequest(method, path, nil)
				if aborted, _ := serve(t, h, bare); aborted || len(*seen) != 0 {
					t.Fatalf("with no credential the request reached a handler (principal %v)", *seen)
				}
				req := httptest.NewRequest(method, path, nil)
				credential(s, req, via)
				aborted, code := serve(t, h, req)
				if !aborted || len(*seen) != 1 {
					t.Fatalf("with the %s credential no principal reached the handler (status %d, saw %v)",
						via, code, *seen)
				}
				want := authz.Principal{UserID: authz.OperatorUserID, Role: authz.RoleAdmin, Via: via}
				if got := (*seen)[0]; got != want {
					t.Fatalf("principal = %v; want %v", got, want)
				}
			})
			checked++
		}
	}
	// Positive control on the walk, derived from the golden rather than from
	// the table it is checking: every non-public route must have been driven
	// at least once (the classification test pins that the table and the
	// golden agree; this pins that the loop actually ran them).
	if nonPublic := len(readGolden(t)) - len(publicRoutes); checked < nonPublic || nonPublic < 90 {
		t.Fatalf("drove %d route/door pairs; the golden has %d non-public routes", checked, nonPublic)
	}
}

// TestAnUnarmedHookTierStillActsAsTheMachineUser covers the branch the main
// walk cannot reach: with NO MUSTER_HOOK_TOKEN, requireHookToken and
// requireHookOrServiceToken admit an uncredentialed caller (back-compat; see
// requireHookToken). Such a request must still carry the machine principal —
// an admitted request with none is exactly what PR 3 turns into an empty
// board. requireArmedHookToken refuses instead (503), so its routes are the
// hook routes that must NOT reach a handler here.
func TestAnUnarmedHookTierStillActsAsTheMachineUser(t *testing.T) {
	_, h, seen := newPrincipalTestServerWith(t, AuthConfig{UIPassword: pPassword})
	// The fail-closed (requireArmedHookToken) routes: the agent-steering write,
	// and session routing's answer (it decides what an outside party is sent).
	armed := map[string]bool{"POST /api/agents/{name}/messages": true, "POST /api/routes/{id}/reply": true}
	for _, pattern := range routeVias[authz.ViaHook] {
		method, path := concretePath(pattern)
		t.Run(pattern, func(t *testing.T) {
			*seen = nil
			aborted, code := serve(t, h, httptest.NewRequest(method, path, nil))
			if armed[pattern] {
				if aborted || code != http.StatusServiceUnavailable {
					t.Fatalf("the armed route on an unarmed server: aborted=%v status %d; want a 503 refusal", aborted, code)
				}
				return
			}
			want := authz.Principal{UserID: authz.OperatorUserID, Role: authz.RoleAdmin, Via: authz.ViaHook}
			if !aborted || len(*seen) != 1 || (*seen)[0] != want {
				t.Fatalf("unarmed hook tier: aborted=%v status %d saw %v; want %v", aborted, code, *seen, want)
			}
		})
	}
}

// TestPublicRoutesAreServedWithoutAPrincipal drives every allowlisted public
// route once with EACH door's credential. None may reach admit: a route that
// does is behind a wrapper, so listing it public is a misclassification that
// would exempt it from the walk above. (Driving it with NO credential would
// prove nothing — a mislabelled route simply refuses and attaches nothing.)
func TestPublicRoutesAreServedWithoutAPrincipal(t *testing.T) {
	s, h, seen := newPrincipalTestServer(t)
	doors := []authz.Via{authz.ViaSession, authz.ViaHook, authz.ViaService, authz.ViaAgent, authz.ViaToken, authz.ViaRunner, authz.ViaPlugin, authz.ViaMirrorHost}
	for _, pattern := range publicRoutes {
		method, path := concretePath(pattern)
		for _, via := range doors {
			t.Run(pattern+" as "+string(via), func(t *testing.T) {
				*seen = nil
				req := httptest.NewRequest(method, path, strings.NewReader(""))
				credential(s, req, via)
				if aborted, _ := serve(t, h, req); aborted || len(*seen) != 0 {
					t.Fatalf("a route listed public attached principal %v — it is behind a wrapper", *seen)
				}
			})
		}
	}
}
