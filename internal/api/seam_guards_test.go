package api

import (
	"context"
	"errors"
	"github.com/ZacxDev/muster/internal/authz"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/notes"
)

// ---------------------------------------------------------------------------
// 1. FORGETTING TO COMPOSE SESSION LIVENESS MUST NOT BE SERVEABLE.
// ---------------------------------------------------------------------------

// TestUncomposedNotesStoreIsNotReady is the BEHAVIOURAL half of that guarantee.
//
// 🔴 IT DRIVES /readyz RATHER THAN CALLING defects(), because a defect list
// nothing reads is not a guard. The failure it defends against is silent by
// construction — every page renders, nothing errors — so the only observable
// that can distinguish the two states is the readiness answer itself.
func TestUncomposedNotesStoreIsNotReady(t *testing.T) {
	cases := []struct {
		name      string
		ext       Extensions
		wantReady bool
	}{
		{
			name:      "notes wired, liveness missing",
			ext:       Extensions{Notes: stubNotesStore{}},
			wantReady: false,
		},
		{
			name:      "notes wired, liveness wired",
			ext:       Extensions{Notes: stubNotesStore{}, SessionLiveness: stubLiveness{}},
			wantReady: true,
		},
		{
			// 🔴 THE CONTROL THAT STOPS THIS BECOMING "ANY MISSING DEPENDENCY IS A
			// DEFECT". No notes store means no task surface and nothing that could
			// render a false transcript row, so it is a configuration rather than a
			// lie. A guard that reddened here would make a standalone deployment
			// permanently not-ready.
			name:      "neither wired",
			ext:       Extensions{},
			wantReady: true,
		},
		{
			// A liveness probe with no notes store composes nothing and lies about
			// nothing.
			name:      "liveness wired, notes missing",
			ext:       Extensions{SessionLiveness: stubLiveness{}},
			wantReady: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(nil, AuthConfig{}, log.New(os.Stderr, "", 0))
			s.UseExtensions(tc.ext)
			rec := httptest.NewRecorder()
			s.handleReady(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			gotReady := rec.Code == http.StatusOK
			if gotReady != tc.wantReady {
				t.Fatalf("/readyz answered %d (ready=%v), want ready=%v.\nbody: %s",
					rec.Code, gotReady, tc.wantReady, rec.Body.String())
			}
			if !tc.wantReady && !strings.Contains(rec.Body.String(), "SessionLiveness") {
				t.Errorf("the refusal does not NAME the missing field, so an operator reading it "+
					"cannot act on it.\nbody: %s", rec.Body.String())
			}
		})
	}
}

// TestLiveNotesOverridesEveryLinkBearingRead is the STRUCTURAL half.
//
// 🔴 IT DERIVES THE SET BY REFLECTION OVER notes.Store, so a method added to
// that interface which can return a Note fails HERE rather than being promoted
// straight off the embedded interface — uncomposed, and silent.
//
// ⚠ WHAT IT CANNOT SEE, said so nobody reads it as wider than it is: it checks
// that liveNotes DECLARES an override for every link-bearing method, not that
// each override actually composes. TestLiveNotesComposesWhatItOverrides is the
// behavioural companion, and it is the one that would catch an override whose
// body forgot to call resolve.
func TestLiveNotesOverridesEveryLinkBearingRead(t *testing.T) {
	storeT := reflect.TypeOf((*notes.Store)(nil)).Elem()
	noteT := reflect.TypeOf(notes.Note{})
	linkT := reflect.TypeOf(notes.SessionLink{})
	pageT := reflect.TypeOf(notes.Page{})

	var carriesLink func(reflect.Type) bool
	carriesLink = func(rt reflect.Type) bool {
		switch rt {
		case noteT, pageT, linkT:
			return true
		}
		if rt.Kind() == reflect.Slice {
			return carriesLink(rt.Elem())
		}
		return false
	}

	// 🔴 THE OVERRIDE SET IS READ FROM THE SOURCE, NOT FROM reflect, AND THE
	// FIRST DRAFT OF THIS TEST USED reflect AND WAS WALKABLE. reflect cannot
	// distinguish a method liveNotes DECLARES from one PROMOTED off the embedded
	// notes.Store: both report the same receiver type and the same signature.
	// So deleting an override — the exact defect this guard exists to catch —
	// left the reflected method set unchanged and the test green. Measured, by
	// renaming ListPage's override and watching the guard survive.
	declared := declaredMethodsOn(t, "session_liveness.go", "liveNotes")
	if len(declared) < 5 {
		t.Fatalf("the source scan found only %d method(s) declared on liveNotes; it is "+
			"reading the wrong file and a clean result below would be meaningless", len(declared))
	}

	var linkBearing, missing []string
	for i := 0; i < storeT.NumMethod(); i++ {
		m := storeT.Method(i)
		found := false
		for j := 0; j < m.Type.NumOut(); j++ {
			if carriesLink(m.Type.Out(j)) {
				found = true
				break
			}
		}
		if !found {
			continue
		}
		linkBearing = append(linkBearing, m.Name)
		if !declared[m.Name] {
			missing = append(missing, m.Name)
		}
	}
	sort.Strings(linkBearing)
	sort.Strings(missing)

	// 🔴 POSITIVE CONTROL. A reflection walk that found no link-bearing method —
	// because the shape check stopped matching, or the interface moved — reports
	// zero missing, which is what a correct implementation also reports.
	if len(linkBearing) < 5 {
		t.Fatalf("the reflection walk found only %d link-bearing method(s) on notes.Store (%v); "+
			"it is measuring nothing and a zero result below would be meaningless",
			len(linkBearing), linkBearing)
	}
	if len(missing) > 0 {
		t.Errorf("liveNotes does NOT override %d link-bearing notes.Store method(s): %s\n\n"+
			"Each is promoted straight off the embedded interface, so every SessionLink it "+
			"returns has DetailAvailable=false — which renders as \"no transcript recorded\" "+
			"over transcripts that exist. Add an override in session_liveness.go.\n"+
			"(link-bearing set, derived: %v)", len(missing), strings.Join(missing, ", "), linkBearing)
	}
}

// declaredMethodsOn returns the set of method names the named file DECLARES on
// the named receiver type. Promoted methods are, correctly, absent.
func declaredMethodsOn(t *testing.T, file, recv string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	out := map[string]bool{}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		rt := fn.Recv.List[0].Type
		if star, ok := rt.(*ast.StarExpr); ok {
			rt = star.X
		}
		if id, ok := rt.(*ast.Ident); ok && id.Name == recv {
			out[fn.Name.Name] = true
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 2. ONE RULE, ONE PLACE — asserted rather than described.
// ---------------------------------------------------------------------------

// callSitesOf counts calls to a named method/function across this package's
// non-test sources, returning file:line for each.
func callSitesOf(t *testing.T, name string) []string {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var sites []string
	parsed := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		parsed++
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var got string
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				got = fn.Sel.Name
			case *ast.Ident:
				got = fn.Name
			}
			if got == name {
				sites = append(sites, fset.Position(call.Pos()).String())
			}
			return true
		})
	}
	if parsed < 10 {
		t.Fatalf("the scanner parsed only %d source file(s); it is wired to nothing", parsed)
	}
	return sites
}

// TestNotifyFanOutHasOneCallSite pins the choke point goNotify's own header
// claims.
//
// 🔴 THE INVARIANT IS ABOUT router.Notify, NOT ABOUT goNotify, AND THE FIRST
// DRAFT OF THIS TEST GOT THAT WRONG — IT ASSERTED goNotify HAD EXACTLY ONE
// CALLER AND WENT RED ON CORRECT CODE. There are legitimately two KINDS of
// notification (a task event and a privilege request) and both are supposed to
// call the choke point; the thing that must be singular is what the choke point
// WRAPS. A second direct call to s.router.Notify is what bypasses the
// pushInFlight registration and the timeout — so a graceful shutdown would
// return with that notification still in flight and drop it.
//
// Recorded rather than quietly corrected, because "the guard was walkable /
// wrong and the code was right" is the outcome this ladder keeps producing, and
// a guard that was fixed reads exactly like one that was always right.
func TestNotifyFanOutHasOneCallSite(t *testing.T) {
	sites := callSitesOf(t, "Notify")
	if len(sites) != 1 {
		t.Errorf("s.router.Notify has %d call site(s), want exactly 1 (inside goNotify):\n  %s\n\n"+
			"Every notification must route through the one choke point, or the "+
			"pushInFlight registration and the timeout are spelled per-caller and drift.",
			len(sites), strings.Join(sites, "\n  "))
		return
	}
	if !strings.Contains(sites[0], "server.go") {
		t.Errorf("s.router.Notify is called from %s; goNotify (server.go) is the only "+
			"place it may be called from", sites[0])
	}
	// The choke point must actually have callers, or "everything routes through
	// it" is true because nothing notifies at all.
	if n := len(callSitesOf(t, "goNotify")); n == 0 {
		t.Error("goNotify has NO callers, so nothing in this service ever notifies and " +
			"the assertion above is vacuous")
	}
}

// TestLiveStatusIsTheOnlyStatusComposer pins the consolidation in
// live_status.go.
//
// 🔴 THE HAZARD IS A KEY, NOT A DUPLICATE. agents.InstanceIndex is keyed on the
// agent NAME; the idiom this replaced was keyed on the namespace. A second call
// site written from memory of the old code indexes by namespace, gets nil back,
// and renders every agent "stopped" while it runs perfectly — silently.
func TestLiveStatusIsTheOnlyStatusComposer(t *testing.T) {
	idx := callSitesOf(t, "InstanceIndex")
	compute := callSitesOf(t, "ComputeStatus")
	for _, set := range []struct {
		name  string
		sites []string
	}{{"agents.InstanceIndex", idx}, {"agents.ComputeStatus", compute}} {
		if len(set.sites) != 1 {
			t.Errorf("%s is called from %d site(s), want exactly 1 (live_status.go):\n  %s\n\n"+
				"Status composition is consolidated so the INDEX KEY is spelled once. "+
				"A second site is a second chance to key it on the namespace, which "+
				"returns nil and renders every agent stopped.",
				set.name, len(set.sites), strings.Join(set.sites, "\n  "))
		}
		for _, s := range set.sites {
			if !strings.Contains(s, "live_status.go") {
				t.Errorf("%s is called from %s, not from live_status.go", set.name, s)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 3. THE ACTOR LABEL IS A WIRE RULE, AND ITS COPY MUST NOT WIDEN.
// ---------------------------------------------------------------------------

// TestActorLabelMatchesTheWireRule pins the pattern this service copied from
// the one it calls.
//
// 🔴 IT PINS THE WHOLE NORMALISED PATTERN STRING, NOT A SAMPLE OF VALUES. A
// value-based test is walkable: widening the class to accept upper case still
// accepts every lower-case fixture, so every sample passes and the drift ships.
// The pattern is a machine-readable claim, so it is compared as one.
//
// ⚠ IT WILL FAIL ON A DELIBERATE CHANGE, and that is the price. If the wire
// rule genuinely changes, update BOTH the regexp and this literal in one
// commit — the diff is then the record that someone decided.
func TestActorLabelMatchesTheWireRule(t *testing.T) {
	const wire = `^[a-z0-9][a-z0-9._-]{0,63}$`
	if got := actorLabel.String(); got != wire {
		t.Fatalf("the actor label pattern has drifted from the receiving service's rule.\n"+
			" here: %s\n wire: %s\n\n"+
			"A value this accepts and the other service rejects is a 403 that reads as a "+
			"credential problem. See actor.go.", got, wire)
	}
	// Behavioural companion: the properties the pattern exists to enforce.
	refuse := []string{"", "Upper", "-leading", ".leading", "has space", "has/slash",
		actorPre0035Unattributed, strings.Repeat("a", 65)}
	accept := []string{"a", "agent", "zesty-stoat", "svc.muster", "a_b", strings.Repeat("a", 64)}
	for _, s := range refuse {
		if ValidActor(s) {
			t.Errorf("ValidActor(%q) = true, want false", s)
		}
	}
	for _, s := range accept {
		if !ValidActor(s) {
			t.Errorf("ValidActor(%q) = false, want true", s)
		}
	}
}

// ---------------------------------------------------------------------------
// 4. THE DIRECTORY PICKER CLAMPS ON THE WAY OUT.
// ---------------------------------------------------------------------------

// TestDirectoryLimitIsClampedOnTheWayOut pins the half a rebuilt-across-a-seam
// feature loses by default.
//
// 🔴 THE FIXTURE VALUES OVERSHOOT THE BOUNDS AND ARE PAIRWISE DISTINCT FROM
// THEM. A fixture asking for exactly MaxDirectoryResults cannot distinguish a
// working clamp from a deleted one, because both return that number.
func TestDirectoryLimitIsClampedOnTheWayOut(t *testing.T) {
	if notes.DefaultDirectoryLimit >= notes.MaxDirectoryResults {
		t.Fatalf("the two bounds are not distinct (%d, %d), so a clamp to the wrong one "+
			"would be invisible below", notes.DefaultDirectoryLimit, notes.MaxDirectoryResults)
	}
	cases := []struct {
		query string
		asked int
		want  int
	}{
		{"", 10000, notes.DefaultDirectoryLimit}, // a blank query is the modal's payload read
		{"", 0, notes.DefaultDirectoryLimit},     // unspecified
		{"", -5, notes.DefaultDirectoryLimit},    // nonsense
		{"x", 10000, notes.MaxDirectoryResults},  // a search may go wider, but not unbounded
		{"x", 0, notes.MaxDirectoryResults},      //
		{"x", 3, 3},                              // a caller asking for LESS is honoured
		{"", 3, 3},                               //
	}
	for _, tc := range cases {
		if got := clampDirectoryLimit(tc.query, tc.asked); got != tc.want {
			t.Errorf("clampDirectoryLimit(%q, %d) = %d, want %d", tc.query, tc.asked, got, tc.want)
		}
	}
}

// recordingRouter captures the limit the handler actually asked the router for.
type recordingRouter struct {
	stubRouter
	gotLimit int
	err      error
}

func (r *recordingRouter) Directories(_ context.Context, _ string, limit int) ([]string, error) {
	r.gotLimit = limit
	return []string{"/work/a"}, r.err
}

// asAdminSession is the operator's request context: the router's directories
// are the operator's, and only an admin principal is served them (accounts PR
// 3b; TestAMemberIsNeverServedTheOperatorsDirectories).
func asAdminSession() context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: authz.RoleAdmin, Via: authz.ViaSession})
}

// TestAMemberIsNeverServedTheOperatorsDirectories: a member's session gets the
// no-router answer (empty, not failed) and the router is never asked.
func TestAMemberIsNeverServedTheOperatorsDirectories(t *testing.T) {
	rr := &recordingRouter{gotLimit: -1}
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	s.UseRouter(rr)
	member := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 2, Role: authz.RoleMember, Via: authz.ViaSession})
	if dirs, failed := s.directories(member, "", 5); failed || len(dirs) != 0 || rr.gotLimit != -1 {
		t.Fatalf("a member: dirs=%v failed=%v (router asked: %v); want none, not failed, router untouched", dirs, failed, rr.gotLimit != -1)
	}
	if dirs, _ := s.directories(asAdminSession(), "", 5); len(dirs) != 1 {
		t.Fatalf("control: the admin got %v; want the router's list", dirs)
	}
}

// TestDirectoryReadDistinguishesFailureFromEmptiness is the behavioural half,
// and it pins the property the whole feature exists to preserve.
func TestDirectoryReadDistinguishesFailureFromEmptiness(t *testing.T) {
	logger := log.New(os.Stderr, "", 0)

	t.Run("no router configured is NOT a failure", func(t *testing.T) {
		s := New(nil, AuthConfig{}, logger)
		dirs, failed := s.directories(context.Background(), "", 5)
		if failed {
			t.Error("a standalone deployment reports the picker as FAILED; it should report " +
				"an empty picker, because there is genuinely no archive to read")
		}
		if len(dirs) != 0 {
			t.Errorf("got %d directories from a server with no router", len(dirs))
		}
	})

	t.Run("a configured router that errors IS a failure", func(t *testing.T) {
		s := New(nil, AuthConfig{}, logger)
		s.UseRouter(&recordingRouter{err: errors.New("boom")})
		if _, failed := s.directories(asAdminSession(), "", 5); !failed {
			t.Error("a router read that errored reported success, so the modal would render an " +
				"empty dropdown as if the archive were empty — the exact conflation this " +
				"feature's bool exists to remove")
		}
	})

	t.Run("the clamp reaches the wire", func(t *testing.T) {
		rr := &recordingRouter{}
		s := New(nil, AuthConfig{}, logger)
		s.UseRouter(rr)
		s.directories(asAdminSession(), "q", 9999) //nolint:errcheck // the recorded limit is the assertion
		if rr.gotLimit != notes.MaxDirectoryResults {
			t.Errorf("the handler asked the router for %d directories; the clamp is %d.\n"+
				"clampDirectoryLimit is not on the path to the call.", rr.gotLimit, notes.MaxDirectoryResults)
		}
	})
}

// ---------------------------------------------------------------------------
// 6. THE SERVICE WORKER MUST NOT OFFER A BUTTON THAT POSTS TO A 404.
// ---------------------------------------------------------------------------

// TestServiceWorkerOffersNoDecisionButtons reads the shipped worker.
//
// 🔴 THE TEST IS ON THE SHIPPED FILE, NOT ON A COPY. web/static/sw.js is what
// the browser runs; a test against a fixture would assert a claim about a file
// nobody serves.
//
// ⚠ THIS IS A TEXT CHECK ON JAVASCRIPT AND IT SAYS SO. It cannot prove the
// worker behaves. It replaces a check that the Approve/Deny buttons were gated on
// the privilege push type; with the privilege subsystem removed this service has
// NO decision route at all, so the worker must render no action buttons and POST
// to neither the privilege route nor the permission router's decision route.
func TestServiceWorkerOffersNoDecisionButtons(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "web", "static", "sw.js"))
	if err != nil {
		t.Fatalf("read the shipped service worker: %v", err)
	}
	src := string(b)
	// Positive control: this is the worker (it handles push and clicks), so the
	// absences below are not a file this test failed to read.
	for _, want := range []string{"addEventListener('push'", "addEventListener('notificationclick'", "actions: [],"} {
		if !strings.Contains(src, want) {
			t.Fatalf("the shipped worker does not contain %q; this test is reading the wrong file "+
				"or the push handler changed shape", want)
		}
	}
	for _, gone := range []string{"/ui/privilege-requests/", "/ui/decision/", "action: 'approve'", "action: 'deny'"} {
		if strings.Contains(src, gone) {
			t.Errorf("the service worker still contains %q — a decision button or a POST to a "+
				"route this service does not register (a tap that 404s and reads as a failed "+
				"approval)", gone)
		}
	}
}

// ---------------------------------------------------------------------------
// 7. THE AGENT-ROW CREDENTIAL IS GONE FROM THE SERVICE TIER.
// ---------------------------------------------------------------------------

// TestNoAgentRowCredentialIsAdmittedAtTheServiceTier pins a DELETION.
//
// 🔴 A DELETION NEEDS A STRUCTURAL PIN OR IT COMES BACK AS A ONE-LINE DIFF
// NOBODY QUESTIONS. The branch that was removed resolved a presented bearer to
// an agent row and admitted it. Re-adding it would hand a credential that lives
// inside an LLM-driven pod, beside a repository checkout, the ability to assert
// an identity on this service's task routes.
func TestNoAgentRowCredentialIsAdmittedAtTheServiceTier(t *testing.T) {
	b, err := os.ReadFile("auth.go")
	if err != nil {
		t.Fatalf("read auth.go: %v", err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "auth.go", b, 0)
	if err != nil {
		t.Fatalf("parse auth.go: %v", err)
	}
	var body string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "requireHookOrServiceToken" || fn.Body == nil {
			continue
		}
		body = string(b[fn.Body.Pos()-1 : fn.Body.End()])
	}
	if body == "" {
		t.Fatal("requireHookOrServiceToken was not found, so this test asserts nothing about it")
	}
	for _, forbidden := range []string{"GetByHooksToken", "ChiefToken"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("requireHookOrServiceToken's BODY mentions %q, so a row-resolved agent "+
				"credential may reach this tier again. That branch was deleted deliberately "+
				"when these routes moved here; see the comment in its place.", forbidden)
		}
	}
}

// ---------------------------------------------------------------------------
// 8. A COMPILED regexp IS THE THING TESTED, not a second copy of it.
// ---------------------------------------------------------------------------

var _ = regexp.MustCompile // keep the import honest if the file is trimmed
