package api

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	g "maragu.dev/gomponents"

	"github.com/ZacxDev/muster/internal/auth"
	"github.com/ZacxDev/muster/internal/github"
	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// A nil Extensions.GitHub MUST RENDER, NOT DEREFERENCE.
//
// 🔴 THE FAILURE THIS FILE IS WRITTEN AGAINST, READ OFF A RUNNING DEPLOYMENT.
// The GitHub connection store is built only when an encryption key is set, so
// the shipping default has Extensions.GitHub == nil — and handleReposContent's
// first statement was a method call on it. The recovered panic answered 500 with
// a ZERO-LENGTH body, so the Repos tab rendered an empty rectangle: no error, no
// empty state, nothing to read.
//
// 🔴 AND THE BLAST RADIUS WAS THE WHOLE APPLICATION, WHICH IS THE PART A READER
// WOULD NOT GUESS FROM THE ROUTE NAME. The shell mounts the Repos panel on every
// page with hx-trigger="load", so EVERY navigation fetched this route and every
// navigation raised a request-failed toast. It had gone unobserved because
// nothing had ever opened the tab.
//
// ⚠ IT IS NOT A REGRESSION AND THERE IS NO CHANGE TO BISECT TO. The route was
// registered unconditionally from the day it was written, which is a deliberate
// property of this package (see routes.go), and the handler has never had the
// branch that property requires. What makes registration-without-branching sound
// is EVERY handler answering its own unwired case, and three of the four here
// did not.
// ---------------------------------------------------------------------------

// unwiredGitHubServer is a server with a real operator credential and NO GitHub
// extension at all — the shape cmd/muster-server builds without an encryption
// key, which is its default.
//
// 🔴 THE FIXTURE MUST BE A nil STORE, NOT A CONFIGURED ONE. A fixture that wires
// any store — even one whose every method returns an error — exercises a
// different branch and cannot see the dereference this file exists for.
// Extensions is left at its zero value so nothing can be wired by accident.
func unwiredGitHubServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword}, log.New(os.Stderr, "", 0))
	s.UseExtensions(Extensions{})
	if s.ext.GitHub != nil {
		t.Fatal("the fixture wired a GitHub store, so every assertion below would " +
			"measure the configured path instead of the unwired one")
	}
	return s, s.Handler()
}

// operatorRequest is a request carrying the human session this fixture's
// password issues, so what a route answers is its OWN behaviour and not the
// session gate's 401.
func operatorRequest(s *Server, method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.AddCookie(&http.Cookie{
		Name:  sessionCookieName,
		Value: auth.SignSession(s.auth.uiSessionSecret(), time.Now(), sessionTTL),
	})
	return r
}

// renderUINode renders a ui node to a string, so an assertion can compare what a
// route wrote against the WHOLE of a named view rather than against a word out
// of its copy.
func renderUINode(t *testing.T, n g.Node) string {
	t.Helper()
	var b bytes.Buffer
	if err := n.Render(&b); err != nil {
		t.Fatalf("rendering the expected view failed: %v", err)
	}
	return b.String()
}

// TestReposPanelRendersTheUnavailableStateWhenTheStoreIsUnwired is the
// regression test for the panic.
//
// 🔴 IT ASSERTS THREE THINGS THAT FAIL FOR DIFFERENT REASONS, AND THE STATUS
// CODE IS THE WEAKEST OF THEM. A handler that wrote nothing and returned 200
// would satisfy a status-only check while leaving the operator the same blank
// rectangle the panic produced, so the body is compared against the WHOLE
// rendered view — the state the page is in, not a word another empty state could
// also spell. The panic counter is read because a 500 is what a recovered panic
// and an ordinary error look like alike.
func TestReposPanelRendersTheUnavailableStateWhenTheStoreIsUnwired(t *testing.T) {
	s, h := unwiredGitHubServer(t)

	before := testutil.ToFloat64(metrics.Panics.WithLabelValues("http"))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, operatorRequest(s, http.MethodGet, "/ui/repos"))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/repos with a nil GitHub store answered %d, want 200.\n"+
			"    The handler must answer its own unwired case. Reaching the store is a "+
			"nil-interface dereference, which the middleware recovers into a 500 with an "+
			"empty body — and the shell fetches this route on EVERY page, so that 500 is "+
			"a failure toast on every navigation in the app.\n    body: %s",
			rec.Code, truncate(rec.Body.String(), 300))
	}
	if rec.Body.Len() == 0 {
		t.Fatal("GET /ui/repos answered with a ZERO-LENGTH body. htmx swaps that in as " +
			"nothing at all, which is the blank panel this test exists to end.")
	}
	want := renderUINode(t, ui.ReposUnavailable())
	if got := rec.Body.String(); got != want {
		t.Errorf("GET /ui/repos did not render the unavailable state.\n"+
			"    This compares the WHOLE view, not a phrase: the assertion is about WHICH "+
			"of the Repos states was chosen, and the four of them are distinguished by "+
			"their prose. If the copy was reworded on purpose, this line moves with it.\n"+
			"    got:  %s\n    want: %s", truncate(got, 300), truncate(want, 300))
	}
	if after := testutil.ToFloat64(metrics.Panics.WithLabelValues("http")); after != before {
		t.Errorf("muster_panics_total{source=\"http\"} moved %v -> %v while serving "+
			"/ui/repos. A recovered panic and a deliberate error both answer 5xx, so the "+
			"counter is what separates them.", before, after)
	}
}

// TestEveryGitHubRouteAnswersWhenTheStoreIsUnwired is the RELATIONSHIP half, and
// it is the one registerGitHubRoutes' comment claims.
//
// 🔴 IT IS DERIVED FROM THE REGISTERED ROUTE SET, NOT FROM A LIST TYPED HERE. A
// fifth GitHub route added later is swept by this test the moment it is
// registered — a hand-written list would leave it unchecked while this file went
// on claiming it covered "every GitHub route". The recorder is the same one the
// route golden uses, so the set is production's.
//
// 🔴 WHAT IT ASSERTS IS "NOT A RECOVERED PANIC", NOT A PARTICULAR STATUS. These
// four routes legitimately answer differently — a panel render, a refusal, a
// redirect — and demanding one code would either be wrong or would freeze
// behaviour this test has no opinion about. The counter is the discriminator.
func TestEveryGitHubRouteAnswersWhenTheStoreIsUnwired(t *testing.T) {
	s, h := unwiredGitHubServer(t)

	routes := githubRoutePatterns(t)
	// 🔴 POSITIVE CONTROL ON THE DERIVATION. An empty set would make this test
	// green while driving nothing, which is exactly how a scanner wired to
	// nothing reports a clean run.
	if len(routes) < 4 {
		t.Fatalf("the derivation found %d GitHub route(s); this package registers four, "+
			"so the walk below is asserting nothing", len(routes))
	}

	for _, route := range routes {
		t.Run(route, func(t *testing.T) {
			method, pattern, ok := strings.Cut(route, " ")
			if !ok {
				t.Fatalf("malformed route pattern %q", route)
			}
			before := testutil.ToFloat64(metrics.Panics.WithLabelValues("http"))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, operatorRequest(s, method, pattern))

			if after := testutil.ToFloat64(metrics.Panics.WithLabelValues("http")); after != before {
				t.Fatalf("%s panicked with a nil GitHub store (muster_panics_total moved "+
					"%v -> %v).\n    Registration in this package is unconditional, so every "+
					"handler runs on a deployment that built no store and every handler owes "+
					"an answer for it.\n    body: %s", route, before, after,
					truncate(rec.Body.String(), 300))
			}
			if rec.Code == http.StatusInternalServerError {
				t.Errorf("%s answered 500 with a nil GitHub store. An unwired dependency is "+
					"a property of the BUILD, not a server fault.\n    body: %s",
					route, truncate(rec.Body.String(), 300))
			}
		})
	}
}

// TestTheOAuthFlowRefusesWhenItCannotCompleteAConnection covers the two routes
// the sweep above cannot distinguish.
//
// 🔴 THE SWEEP PASSES THESE TWO AT EITHER END, AND THAT IS EXACTLY WHY THEY NEED
// THIS. Neither reaches the store before returning on the request shape the
// sweep sends — connect redirects, callback rejects a missing state cookie — so
// "it did not panic" is true of them with or without a guard. What that leaves
// unmeasured is the behaviour the guard is FOR: an unwired build handing a
// caller a redirect to an authorize URL carrying an empty client id, which
// GitHub answers with its own error page. The caller is told to go and
// authorize something that cannot be authorized.
//
// 🔴 THE ASSERTION IS ON THE GUARD'S OWN REASON, NOT JUST ON 503. These routes
// sit beside others that answer 503 for a wholly different dependency, and a
// status-only check cannot tell one refusal from another — it would stay green
// with this guard deleted and a neighbouring one firing.
func TestTheOAuthFlowRefusesWhenItCannotCompleteAConnection(t *testing.T) {
	s, h := unwiredGitHubServer(t)

	for _, path := range []string{"/github/connect", "/github/callback"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, operatorRequest(s, http.MethodGet, path))

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("GET %s on a build with no GitHub store and no OAuth App answered "+
					"%d, want 503.\n    Anything else sends the operator into an OAuth flow "+
					"this build cannot finish.\n    body: %s",
					path, rec.Code, truncate(rec.Body.String(), 200))
			}
			if !strings.Contains(rec.Body.String(), githubUnwiredReason) {
				t.Errorf("GET %s refused with %q, which is not this guard's reason. Several "+
					"routes in this package answer 503 for unrelated missing dependencies, so "+
					"the code alone does not say which guard fired.",
					path, truncate(rec.Body.String(), 200))
			}
		})
	}
}

// TestTheOAuthFlowStartsWhenItCanCompleteAConnection is the discrimination
// control for the guard above.
//
// 🔴 WITHOUT IT, A GUARD THAT REFUSED UNCONDITIONALLY WOULD BE GREEN. That is
// not a hypothetical shape — `if true` and a mistyped conjunction produce it —
// and it would take the only way to connect an account off every deployment
// while every refusal test passed.
func TestTheOAuthFlowStartsWhenItCanCompleteAConnection(t *testing.T) {
	s := New(nil, AuthConfig{UIPassword: testUIPassword}, log.New(os.Stderr, "", 0))
	s.UseExtensions(Extensions{
		GitHub: disconnectedGitHubStore{},
		GitHubOAuth: GitHubOAuthConfig{
			// The state's MAC key (cmd/muster-server derives it from the store's
			// encryption key); a build that can connect always has one.
			StateKey:     []byte("unwired-test-state-key-0123456789abcd"),
			ClientID:     "an-oauth-app-client-id",
			ClientSecret: "an-oauth-app-client-secret-not-a-real-one",
			BaseURL:      "https://muster.example",
		},
	})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, operatorRequest(s, http.MethodGet, "/github/connect"))

	if rec.Code != http.StatusFound {
		t.Fatalf("GET /github/connect with a store AND an OAuth App answered %d, want a "+
			"302 to the authorize page. A guard that refuses a build that CAN connect "+
			"removes the only way to connect an account.\n    body: %s",
			rec.Code, truncate(rec.Body.String(), 200))
	}
}

// disconnectedGitHubStore is a wired store holding no connection. It exists so
// the guard below can drive the WIRED shape without a database.
//
// ⚠ ITS METHODS ARE NEVER REACHED BY THE GUARD THAT USES IT — only its presence
// is, through Extensions.GitHub != nil — but they are implemented rather than
// embedded-nil so a future assertion that DOES call one fails on its answer
// instead of on a dereference.
type disconnectedGitHubStore struct{}

func (disconnectedGitHubStore) Save(context.Context, string, string, string) error { return nil }
func (disconnectedGitHubStore) Get(context.Context) (github.Connection, bool, error) {
	return github.Connection{}, false, nil
}
func (disconnectedGitHubStore) Clear(context.Context) error { return nil }

// TestTheShellDrawsTheReposSurfaceExactlyWhenTheStoreIsWired is the SEAM between
// the two halves of this fix, and neither half implies it.
//
// 🔴 EACH HALF IS CORRECT IN ISOLATION AND STILL LEAVES THE DEFECT. The handler
// answering its unwired case removes the 500 but leaves the shell fetching a
// route that can only refuse, on every page; the shell hiding the panel removes
// the fetch but leaves a stale tab and a bookmarked URL reaching a handler that
// must still answer. What nothing inside either package can see is whether the
// page and the handler read the SAME state — internal/ui does not know what a
// server wired, and a handler test never renders a shell. This drives the real
// mux at both ends of that state.
//
// 🔴 IT IS A RELATIONSHIP, ASSERTED IN BOTH DIRECTIONS. Checking only the
// unwired end passes on a build that never draws the tab at all; checking only
// the wired end passes on the shipped defect. The pair is the claim.
func TestTheShellDrawsTheReposSurfaceExactlyWhenTheStoreIsWired(t *testing.T) {
	cases := []struct {
		name  string
		store github.Store
		want  bool
	}{
		{"no GitHub store (the shipping default)", nil, false},
		{"a GitHub store, with nothing connected", disconnectedGitHubStore{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(nil, AuthConfig{UIPassword: testUIPassword}, log.New(os.Stderr, "", 0))
			s.UseExtensions(Extensions{GitHub: tc.store})
			h := s.Handler()

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, operatorRequest(s, http.MethodGet, "/"))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET / answered %d, want 200; nothing below is a claim about the shell", rec.Code)
			}
			body := rec.Body.String()

			// The two attributes the BROWSER keys on, not the label: the panel id
			// appScript toggles, and the data-tab the router reads off a nav link.
			// A copy change cannot move either, and no other feature can spell them.
			panel := strings.Contains(body, `id="panel-repos"`)
			link := strings.Contains(body, `data-tab="repos"`)

			if panel != tc.want || link != tc.want {
				t.Errorf("shell with %s: repos panel present = %v, repos nav link present = %v; "+
					"want both %v.\n"+
					"    The panel carries hx-trigger=\"load\" and the shell renders on EVERY page, so "+
					"mounting it without a store behind it is one failed fetch per navigation across the "+
					"whole application. Hiding it without a store is what makes the tab's absence honest.",
					tc.name, panel, link, tc.want)
			}
		})
	}
}

// TestADeepLinkToAHiddenTabStillRendersAShell closes the hole the guard above
// leaves: the tab routes stay registered whatever is wired, so /repos is served
// on a build with no store and must land somewhere.
//
// 🔴 A SHELL WITH NO PANEL SHOWN IS A BLANK PAGE UNDER A CORRECT-LOOKING URL,
// which is worse than the 500 this change removes — there is nothing at all to
// read. The clamp sends it to the default tab, which is what an unrouted path
// already does.
func TestADeepLinkToAHiddenTabStillRendersAShell(t *testing.T) {
	s, h := unwiredGitHubServer(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, operatorRequest(s, http.MethodGet, "/repos"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /repos with no GitHub store answered %d, want 200 — the route is "+
			"registered unconditionally, so it must serve something", rec.Code)
	}
	body := rec.Body.String()
	// The default tab's panel must be the VISIBLE one. `hidden` is the class
	// every inactive panel carries, so its absence on this panel is the state
	// "this is the tab being shown".
	i := strings.Index(body, `id="panel-`+defaultTabForTest+`"`)
	if i < 0 {
		t.Fatalf("GET /repos rendered no #panel-%s at all", defaultTabForTest)
	}
	// Read the class attribute of that element only.
	rest := body[i:]
	end := strings.Index(rest, ">")
	if end < 0 {
		t.Fatal("could not read the panel's opening tag")
	}
	if strings.Contains(rest[:end], "hidden") {
		t.Errorf("GET /repos with no GitHub store left #panel-%s hidden, so every panel in "+
			"the document is hidden and the page is blank. The active tab must clamp to one "+
			"this build draws.\n    tag: %s", defaultTabForTest, rest[:end])
	}
}

// defaultTabForTest is the tab an unknown or undrawable tab clamps to. It is
// internal/ui's `defaultTab`, which is unexported; spelled here because the two
// packages are separate and this is the only thing this file needs from it.
const defaultTabForTest = "tasks"

// githubRoutePatterns is every registered route this file's handlers serve,
// taken from the route recorder.
//
// ⚠ IT MATCHES ON THE PATH PREFIXES THE GitHub ROUTES USE, which is a property
// of the URL space rather than of the fixture — /ui/repos is the panel and
// /github/… is the OAuth flow, and nothing else in the module registers either.
func githubRoutePatterns(t *testing.T) []string {
	t.Helper()
	rec := &muxRecorder{}
	RegisterRoutes(rec)
	var out []string
	for _, p := range rec.patterns {
		_, path, ok := strings.Cut(p, " ")
		if !ok {
			continue
		}
		if path == "/ui/repos" || strings.HasPrefix(path, "/github/") {
			out = append(out, p)
		}
	}
	return out
}
