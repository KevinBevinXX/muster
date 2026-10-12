package api

import (
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// ---------------------------------------------------------------------------
// THE REFUSAL chief_threads.go HAS CLAIMED SINCE IT WAS WRITTEN.
//
// 🔴 THIS TEST DID NOT EXIST, AND TWO COMMENTS SAID IT DID. chief_threads.go and
// server.go both told a reader that "the two non-operator credentials that reach
// other routes in this package must get a 401 here, and
// TestChiefThreadSearchIsOperatorOnly asserts both of them". Nothing in the
// suite referenced this route, requireSession, or either credential. The
// invariant was held by two lines of mux.HandleFunc and by nothing else — and
// the comments actively discouraged anyone from checking, which is worse than
// silence. GET /ui/chief/threads/search returns MESSAGE CONTENT: an excerpt cut
// out of a chat message body.
//
// 🔴 THE CREDENTIALS PRESENTED HERE ARE GENUINELY VALID ONES, NOT NONSENSE
// STRINGS. That is the whole discrimination. A test that presents "not-a-token"
// and gets 401 passes identically whether the route is behind requireSession,
// requireHookToken or requireAgentToken — it measures nothing about the tier.
// The hook token below IS this server's configured hook token, and the agent
// token below DOES resolve to an agent row through the store. Both would be
// ADMITTED by the middleware on neighbouring routes in this package; they are
// refused here.
// ---------------------------------------------------------------------------

// tokenAgentsStore resolves exactly one hooks token to a real agent, so the
// agent credential presented below is one requireAgentToken would accept.
type tokenAgentsStore struct {
	agents.Store
	token string
	agent agents.Agent
}

func (s tokenAgentsStore) GetByHooksToken(_ context.Context, token string) (agents.Agent, error) {
	if token == s.token {
		a := s.agent
		if a.OwnerID == 0 { // a Postgres row always has an owner; see relayAgents
			a.OwnerID = 1
		}
		return a, nil
	}
	return agents.Agent{}, errNoSuchAgentInTest
}

var errNoSuchAgentInTest = &testError{"no agent with that hooks token"}

type testError struct{ s string }

func (e *testError) Error() string { return e.s }

const (
	// A configured hook token: the shared machine credential every hook script
	// on both hosts holds.
	testHookToken = "hook-token-for-the-shared-machine-tier-not-a-real-secret"
	// A per-agent hooks token that the store below resolves.
	testAgentToken = "agent-hooks-token-that-resolves-to-a-row-not-a-real-secret"
	// Long enough to clear minUIPasswordLen so the browser tier is CONFIGURED
	// rather than fail-closed. See the control test below for why that matters.
	testUIPassword = "an-operator-password-long-enough-to-be-accepted-not-a-real-one"
)

// operatorOnlyServer is a Server on which both machine credentials are real.
func operatorOnlyServer(t *testing.T) http.Handler {
	t.Helper()
	s := New(nil, AuthConfig{
		HookToken:  testHookToken,
		UIPassword: testUIPassword,
	}, log.New(os.Stderr, "", 0))
	s.UseExtensions(Extensions{
		Agents: tokenAgentsStore{
			token: testAgentToken,
			agent: agents.Agent{ID: 1, Name: "chief"},
		},
	})
	return s.Handler()
}

// chiefThreadSearchPath is the route under test. It is spelled once.
const chiefThreadSearchPath = "/ui/chief/threads/search"

// TestChiefThreadSearchIsOperatorOnly asserts the refusal the route's comment
// promises: neither machine credential gets in.
func TestChiefThreadSearchIsOperatorOnly(t *testing.T) {
	h := operatorOnlyServer(t)

	// 🔴 POSITIVE CONTROL: THE CREDENTIALS MUST ACTUALLY OPEN SOMETHING. Without
	// this, a 401 on the route under test is indistinguishable from a server on
	// which every credential is broken — the test would be green with the whole
	// auth layer inert. GET /agent/task is on requireAgentToken and is in this
	// same package; a valid agent token reaches its HANDLER, which then answers
	// 404 ("no task assigned to this agent") because the agent row has no task.
	// 404 is the handler speaking, which is exactly the proof wanted: the
	// middleware admitted the caller.
	control := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agent/task", nil)
	req.Header.Set("Authorization", "Bearer "+testAgentToken)
	h.ServeHTTP(control, req)
	if control.Code == http.StatusUnauthorized {
		t.Fatalf("positive control FAILED: the agent token was refused 401 on /agent/task, "+
			"a route it is supposed to open. Every assertion below would pass for the "+
			"wrong reason. body: %s", control.Body.String())
	}
	t.Logf("positive control: the agent token reaches the handler on /agent/task (%d)", control.Code)

	cases := []struct {
		name   string
		header string
		value  string
	}{
		// The shared machine credential, in both spellings the hook tier accepts.
		{"the shared hook token, as a bearer", "Authorization", "Bearer " + testHookToken},
		{"the shared hook token, as X-Muster-Token", "X-Muster-Token", testHookToken},
		// The per-agent credential, which resolves to a real agent row.
		{"a valid per-agent hooks token, as a bearer", "Authorization", "Bearer " + testAgentToken},
		{"a valid per-agent hooks token, as X-Muster-Token", "X-Muster-Token", testAgentToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, chiefThreadSearchPath+"?q=secret", nil)
			r.Header.Set(tc.header, tc.value)
			h.ServeHTTP(rec, r)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("GET %s with %s = %d, want 401.\n"+
					"  This route returns MESSAGE CONTENT — an excerpt from a chat message\n"+
					"  body. No machine credential may reach it, however many neighbouring\n"+
					"  routes those credentials open.\n  body: %s",
					chiefThreadSearchPath, tc.name, rec.Code, rec.Body.String())
			}
			// 🔴 THE BODY IS CHECKED FOR ABSENCE OF CONTENT, NOT JUST THE CODE. A
			// handler that wrote an excerpt and THEN set 401 would satisfy a
			// status-only assertion while having already leaked the thing this
			// route is gated for.
			if body := rec.Body.String(); !strings.Contains(body, "not signed in") {
				t.Errorf("the refusal body is %q, which is not the no-session refusal. "+
					"Check that nothing was rendered before the gate ran.", body)
			}
		})
	}
}

// TestChiefThreadSearchRefusesAnAnonymousCallerToo is the floor under the test
// above: no credential at all is also refused.
//
// ⚠ IT IS A SEPARATE TEST BECAUSE IT PROVES A DIFFERENT THING. The test above
// asserts that VALID machine credentials do not open this door; this one
// asserts the door is shut by default. A single test covering both would go
// green on a route that refused everything including an operator session, which
// is the jammed-door case.
func TestChiefThreadSearchRefusesAnAnonymousCallerToo(t *testing.T) {
	rec := httptest.NewRecorder()
	operatorOnlyServer(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, chiefThreadSearchPath, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET %s anonymously = %d, want 401", chiefThreadSearchPath, rec.Code)
	}
}

// TestChiefThreadSearchIsRegisteredAtThePathItsGuardNames closes the way the two
// tests above could both be green over a deleted route.
//
// 🔴 A ROUTE THAT DOES NOT EXIST ANSWERS 404, NOT 401 — so this is not merely
// belt and braces. But a route RENAMED would leave both tests above asserting a
// refusal on a path nothing serves, and the real path would be unguarded with a
// full green suite. The route golden is the authority on what is registered, so
// the path is checked against it rather than against another copy of the string.
func TestChiefThreadSearchIsRegisteredAtThePathItsGuardNames(t *testing.T) {
	rec := &muxRecorder{}
	RegisterRoutes(rec)
	want := "GET " + chiefThreadSearchPath
	for _, p := range rec.patterns {
		if p == want {
			return
		}
	}
	t.Fatalf("no route %q is registered. The refusal tests in this file are asserting "+
		"a 401 on a path this server does not serve — which it answers for every "+
		"unregistered path.\n  registered: %d route(s)", want, len(rec.patterns))
}
