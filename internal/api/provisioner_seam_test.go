package api

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/auth"
	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/push"
)

// ---------------------------------------------------------------------------
// A NIL Provisioner MUST REFUSE, NOT ANSWER 200 AND DO NOTHING.
//
// 🔴 THE MEASUREMENT THAT PUT THIS FILE HERE. cmd/muster-server leaves
// Extensions.Provisioner nil on purpose (doc_seams.go entry 1) and its boot
// banner asserted, on every start, that "every agent-control route is
// REGISTERED … and each one refuses at request time". Driven against the image
// this tree builds, on the same boot whose banner printed that line, NINE routes
// answered 200 and did nothing:
//
//	POST /agents                      200 + a rendered card; the row sat `provisioning` for ever
//	POST /agents/{id}/start           200 + 3,590 bytes of card HTML
//	POST /agents/{id}/stop            200
//	DELETE /agents/{id}               200; htmx removed the card, the row was still there
//	GET  /agents/{name}/logs/stream   200 + `: connected`, then EOF — reads as "no logs"
//	GET  /agents/{name}/ws            the same shape, through chatTurn
//	POST /chief/provision             200, then SIGSEGV — its goroutine was BARE
//	POST /runbooks/{id}/dispatch      200
//	POST /api/agents/{name}/messages  200
//
// muster_panics_total{source="goroutine"} read 4. The panic is recovered by
// safeGo AFTER the handler has written its 200, so recoverMiddleware's 500 never
// reaches the wire and the operator is told the thing worked.
//
// 🔴 THIS IS THE IDENTICAL OBSERVABLE doc_seams.go ENTRY 1 REFUSES TO WIRE A
// FAKE FOR: "dispatch would report success, no pod would exist, and the agent
// would sit in `provisioning` for ever. That is the 'lies silently' side of the
// line." Wiring nil produced it exactly. The nil is only the honest option this
// entry claims once something refuses, which is api.requireLifecycleProvisioner
// and api.requireGatewayProvisioner.
//
// The two halves below fail for different reasons and neither substitutes for
// the other: the first is a RELATIONSHIP over the source (a tenth call site
// cannot arrive unnoticed), the second is BEHAVIOUR through the real mux (a
// wrapper that is registered but broken cannot pass).
// ---------------------------------------------------------------------------

// Each row's `routes` is every route registered behind that row's wrapper.
//
// 🔴 THE TEST FAILS WHEN THIS SET GROWS *OR* SHRINKS, AND THAT SENTENCE WAS
// FALSE FOR A WHOLE REVISION — THIS COMMENT IS WHAT MADE IT UNCHECKABLE.
// Nothing derived the wrapped-route set from the source, so "shrinks" meant
// only "somebody edited this literal": a route could lose its wrapper and its
// ledger line together — two lines, the exact shape of a developer clearing a
// red subtest — and every test in the module stayed green. Measured: unwrap
// `POST /agents` at agents.go and delete its entry here, and the full suite
// passes with the shipped nil-provisioner deployment back to answering 200 over
// a row stuck in `provisioning` for ever. The only signal was the verdict count
// falling by two, and nothing read that.
//
// 🔴 SO THE SET IS DERIVED NOW, BY TestEveryProvisionerRouteIsDerivedNotDeclared
// BELOW, AND THIS LITERAL IS THE CLAIM BEING CHECKED — NOT THE SOURCE OF TRUTH.
// That guard asserts two different things, and the two-line mutant above is
// only caught by the SECOND:
//
//	DERIVED == DECLARED — the routes whose registration expression calls
//	  its dependency's wrapper are exactly the ones below. This catches a one-sided
//	  edit: an unwrap that leaves the ledger alone, or a delist that leaves the
//	  wrapper alone.
//	AND REACHABILITY DECIDES WHICH ROUTES BELONG — every registered route whose
//	  handler can reach s.ext.Provisioner unguarded through this package's call
//	  graph MUST be wrapped, and every wrapped one must be able to. The ledger
//	  plays no part in this half, which is why a matching two-line edit cannot
//	  satisfy it: `POST /agents` still resolves to handleAgentCreate, which
//	  still reaches Dispatch through createAndDispatchAgent.
//
// A plain "every wrapped route refuses" check would be satisfied by wrapping
// ZERO routes, which is the state this file was written to end.
// 🔴 THERE ARE TWO DEPENDENCIES NOW, AND THIS FILE IS A TABLE OVER THEM RATHER
// THAN TWO COPIES OF ITSELF. api.Provisioner used to carry agent LIFECYCLE and
// agent CHAT behind one nil and one wrapper; they are now api.Provisioner and
// api.Gateway, each with its own wrapper. Every derived check below runs once per
// row, so a third dependency is a row rather than a fork — and, more to the point,
// neither row can be weakened without the other noticing, because they share the
// derivation code.
//
// ⚠ THE COST OF THE SPLIT, STATED WHERE IT IS PAID: each ledger is now smaller, so
// each is individually easier to satisfy by accident. That is why the behavioural
// test drives THREE fixtures rather than one — both nil, lifecycle-only, and
// gateway-only — see TestWiringOneDependencyDoesNotSilenceTheOther. Without that
// third shape the split would be exactly the "wire it and the other routes stop
// refusing" defect it was made to prevent.
type provisionerDep struct {
	// name is what failure messages call this dependency.
	name string
	// field is the api.Extensions field, and the selector the AST walk matches:
	// `s.ext.<field>`.
	field string
	// wrapper is the Server method that refuses when the field is nil.
	wrapper string
	// routes is every route registered behind wrapper.
	routes []string
	// callers is every function in this package that calls a method on
	// s.ext.<field> WITHOUT first checking it for nil in its own body.
	callers []string
}

var provisionerDeps = []provisionerDep{
	{
		name:    "lifecycle provisioner",
		field:   "Provisioner",
		wrapper: "requireLifecycleProvisioner",
		routes: []string{
			"DELETE /agents/{id}",
			"GET /agents/{name}/logs/stream",
			"GET /agents/{name}/terminal/ws",
			"POST /agents",
			"POST /agents/{id}/start",
			"POST /agents/{id}/stop",
			"POST /api/chief/replace",
			"POST /chief/provision",
		},
		callers: []string{
			"(*Server).createAndDispatchAgent", // Dispatch; reached from handleAgentCreate, handleChiefProvision
			"(*Server).handleAgentCreate",      // KickoffUndeliverableReason, the synchronous pre-flight before the row
			"(*Server).handleAgentDelete",      // Destroy
			"(*Server).handleAgentLogsStream",  // StreamLogs
			"(*Server).handleAgentStart",       // Start
			"(*Server).handleAgentStop",        // Stop
			"(*Server).handleAgentTerminalWS",  // AttachTerminal
			"(*Server).handleChiefProvision",   // Start
			"(*Server).replaceChief",           // Destroy + Start; reached from handleChiefReplace
		},
	},
	{
		name:    "agent gateway",
		field:   "Gateway",
		wrapper: "requireGatewayProvisioner",
		routes: []string{
			"GET /agents/{name}/ws",
			"POST /agents/{name}/send",
			"POST /api/agents/{name}/messages",
			"POST /api/jobs",
		},
		callers: []string{
			"(*Server).askTurn",    // Chat, or ChatWithTools + Chat for the gateway sidecar; reached from handleAPIAgentSendMessage
			"(*Server).chatTurn",   // ChatWithTools + Chat; reached from runUserTurn (handleAgentWS, handleAgentSend)
			"(*Server).runJobTurn", // Chat; reached from runJob, started only by handleJobCreate (POST /api/jobs)
		},
	},
}

// allProvisionerRoutes is every route on any row, for the fixtures that drive the
// whole set at once.
func allProvisionerRoutes() []string {
	var out []string
	for _, d := range provisionerDeps {
		out = append(out, d.routes...)
	}
	sort.Strings(out)
	return out
}

// 🔴 THE CALLER LEDGERS ABOVE CARRY A KNOWN LIMIT, FOUND BY A MUTANT THAT SURVIVED
// THIS GUARD RATHER THAN BY REVIEW: THE DETECTION IS FUNCTION-GRANULAR, NOT
// CALL-SITE-GRANULAR. A function counts as guarded if it compares the field
// against nil ANYWHERE in its body — so a function that nil-checks on one path and
// calls unguarded on another is classified guarded and never reaches a ledger. The
// sweep mutant that proved it bolted a call onto instanceIndex, which already had
// a nil check, and the guard stayed green. Closing it properly needs dominance
// analysis (does the check dominate the call?), which is a real piece of work and
// out of scope here.
//
// ⚠ WHAT THAT DOES AND DOES NOT COST. The case it cannot see is a MODIFIED
// already-guarded function; the case these ledgers exist for — a NEW handler
// reaching a dependency, which is how a lying route would arrive — is caught, and
// the sweep confirms it with a mutant that adds exactly such a function. Read a
// clean verdict as "no new unguarded caller", never as "every call site is
// dominated by a check".
//
// ⚠ A TYPE ASSERTION IS NOT A CALL AND IS DELIBERATELY ABSENT. `s.ext.Provisioner
// .(ProfileReapplier)` on a nil interface yields ok=false; it is the one form of
// optional dependency that is safe unguarded, and doc_seams.go entry 3 is about
// exactly that. handleAgentModel and the privilege grant path use it and are not
// on any list.

// unwiredProvisionerServer builds the deployment cmd/muster-server actually
// ships: every dependency EXCEPT Provisioner.
//
// 🔴 THE HOOK TOKEN HAS TO BE ARMED, OR ONE ROUTE WOULD PASS FOR THE WRONG
// REASON. POST /api/agents/{name}/messages sits behind requireArmedHookToken,
// which answers 503 of its OWN when the token is unset. A fixture that left it
// unset would see 503 on that route whether or not its wrapper was ever
// registered there — green from a DIFFERENT guard's refusal, and still green
// with the wrapper deleted. Arming the tier and presenting the credential makes
// the only remaining 503 the one under test.
//
// ⚠ testHookToken/testUIPassword are this package's existing fixture
// credentials (chief_threads_auth_test.go). Reused rather than re-minted: a
// second spelling of a shared secret is a second thing to keep in step.
//
// 🔴 IT RETURNS s.Handler(), NOT A BARE MUX, AND THAT IS WHAT MAKES THE
// MUTATION KILL ATTRIBUTABLE. Handler() is the production chain —
// recoverMiddleware included — so a handler that reaches a nil Provisioner
// answers 500 instead of taking the test binary down with it. Against a bare
// mux, deleting a wrapper would kill this test with a raw panic: red,
// but red for a reason that is indistinguishable from the fixture being wrong.
// With the real chain the failure is THIS test's own "answered 500, want 503".
func unwiredProvisionerServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	return provisionerServer(t, nil, nil)
}

// provisionerServer builds the same fixture with either dependency independently
// wired, which is what makes the three-shape matrix below expressible.
//
// 🔴 EVERY OTHER DEPENDENCY IS WIRED, AND THAT IS THE POINT OF THE FIXTURE. The
// only nil under test is the one the caller left nil, so a 503 carrying
// ProvisionerUnwiredField can have come from nowhere else.
func provisionerServer(t *testing.T, prov Provisioner, gw Gateway) (*Server, http.Handler) {
	t.Helper()
	s := New(nil, AuthConfig{
		UIPassword:   testUIPassword,
		HookToken:    testHookToken,
		ServiceToken: testServiceToken,
	}, log.New(os.Stderr, "", 0))
	s.UseExtensions(Extensions{
		Notes:           stubNotesStore{},
		Agents:          stubAgentsStore{},
		SessionLiveness: stubLiveness{},
		Provisioner:     prov,
		Gateway:         gw,
	})
	return s, s.Handler()
}

// TestWiringOneDependencyDoesNotSilenceTheOther is the test the SPLIT exists for,
// and without it the split would be strictly worse than the single nil it replaced.
//
// 🔴 THE DEFECT IT GUARDS IS THE WHOLE REASON THE OLD SHAPE WAS A PROBLEM. One nil
// gated nine routes, so wiring a lifecycle adapter would have stopped the two CHAT
// routes refusing as well — they would have answered 200 over a gateway that does
// not exist, which is byte-for-byte the "dispatch reports success, no pod exists"
// observable doc_seams.go entry 1 refuses to ship. Splitting the gate is only an
// improvement if the halves are actually independent, and "actually" means measured.
//
// ⚠ IT ASSERTS ON THE REFUSAL MARKER, NOT ON 503 ALONE. Several of these routes can
// answer 503 for reasons of their own (an unarmed hook tier, a stub store panicking
// into recoverMiddleware), so a status-code-only check would pass for the wrong
// reason and keep passing with a wrapper deleted. ProvisionerUnwiredField is
// specific to these two wrappers.
func TestWiringOneDependencyDoesNotSilenceTheOther(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prov    Provisioner
		gw      Gateway
		refuses string // the dep.field whose routes MUST still refuse
	}{
		{"lifecycle wired, gateway nil", stubProvisioner{}, nil, "Gateway"},
		{"gateway wired, lifecycle nil", nil, stubGateway{}, "Provisioner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, h := provisionerServer(t, tc.prov, tc.gw)

			var refusing, silent int
			for _, dep := range provisionerDeps {
				mustRefuse := dep.field == tc.refuses
				for _, route := range dep.routes {
					method, pattern, _ := strings.Cut(route, " ")
					path := strings.NewReplacer("{id}", "1", "{name}", "chief").Replace(pattern)
					req := httptest.NewRequest(method, path, strings.NewReader("{}"))
					admit(s, req)
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, req)
					got := strings.Contains(rec.Body.String(), ProvisionerUnwiredField)

					if mustRefuse && !got {
						t.Errorf("%s did NOT refuse with %s wired and %q nil (%d).\n"+
							"Wiring one dependency has silenced the other's refusal, which is the "+
							"exact defect splitting the wrapper was meant to prevent: this route "+
							"now answers over a dependency that does not exist.\n    body: %s",
							route, tc.name, tc.refuses, rec.Code, truncate(rec.Body.String(), 300))
					}
					if !mustRefuse && got {
						t.Errorf("%s refused with %s, but its dependency IS wired (%d).\n"+
							"The wrappers are crossed: this route is gated on the wrong field.\n"+
							"    body: %s", route, ProvisionerUnwiredField, rec.Code,
							truncate(rec.Body.String(), 300))
					}
					if mustRefuse {
						refusing++
					} else {
						silent++
					}
				}
			}

			// 🔴 POSITIVE CONTROLS ON THE MATRIX ITSELF. A loop that drove zero routes
			// on either side would report no failures and mean nothing.
			if refusing == 0 || silent == 0 {
				t.Fatalf("the matrix drove %d must-refuse and %d must-not-refuse route(s); "+
					"both sides must be non-empty or this test asserts nothing about "+
					"independence", refusing, silent)
			}
		})
	}
}

// stubGateway is the chat half of the old stubProvisioner. It exists because the
// interface split made "wire chat but not lifecycle" a state a test has to be able
// to build.
type stubGateway struct{}

func (stubGateway) Chat(context.Context, agents.Agent, string, string, func(string)) (string, error) {
	return "", nil
}

func (stubGateway) ChatWithTools(context.Context, agents.Agent, string, string, string, []agents.ToolDef, agents.ToolDispatch, agents.StreamEmit) (string, error) {
	return "", nil
}

// admit presents the credentials this fixture holds, so a route is measured at
// its provisioner door regardless of which auth door it sits behind: the session
// cookie always, and the ONE machine credential the route's tier takes. The hook
// and service tiers read the same header (hookTokenFromRequest), so they cannot
// both be presented; the job API (jobs.go) is the service tier's, every other
// machine route here the hook tier's.
func admit(s *Server, r *http.Request) {
	r.AddCookie(&http.Cookie{
		Name:  sessionCookieName,
		Value: auth.SignSession(s.auth.uiSessionSecret(), time.Now(), sessionTTL),
	})
	if strings.HasPrefix(r.URL.Path, "/api/jobs") && s.auth.ServiceToken != "" {
		r.Header.Set("Authorization", "Bearer "+testServiceToken)
		r.Header.Set(ActorHeader, "provisioner-seam-test")
		return
	}
	r.Header.Set("X-Muster-Token", testHookToken)
}

// TestEveryProvisionerRouteRefusesWhenItIsUnwired is the BEHAVIOURAL half.
//
// 🔴 IT DRIVES THE REAL MUX, NOT THE WRAPPER. A unit test of a wrapper
// would pass while every route was registered without it — which was the state
// that shipped. The only observable that separates "wrapped" from "not wrapped"
// is what the registered route answers, so that is what is read.
func TestEveryProvisionerRouteRefusesWhenItIsUnwired(t *testing.T) {
	s, h := unwiredProvisionerServer(t)

	for _, route := range allProvisionerRoutes() {
		t.Run(route, func(t *testing.T) {
			method, pattern, ok := strings.Cut(route, " ")
			if !ok {
				t.Fatalf("malformed ledger entry %q", route)
			}
			// Substitute concrete values for the wildcards so the request
			// actually matches the registered pattern.
			path := strings.NewReplacer("{id}", "1", "{name}", "chief").Replace(pattern)

			req := httptest.NewRequest(method, path, nil)
			// Past whichever auth door this route sits behind, so what is
			// measured is the PROVISIONER refusal and not the auth one — the two
			// are both 503 on the machine tier, and conflating them would let a
			// route pass this test while never reaching its wrapper.
			admit(s, req)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("%s answered %d, want 503.\n"+
					"    A nil Provisioner means this route cannot do its work. Answering "+
					"anything else is the measured failure: 200 with a rendered card over a "+
					"row that never changes, which an operator reads as success.\n"+
					"    body: %s", route, rec.Code, truncate(rec.Body.String(), 300))
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("%s: 503 body is not JSON (%v), so a client cannot branch on it.\n"+
					"    body: %s", route, err, truncate(rec.Body.String(), 300))
			}
			// 🔴 THE FIELD, NOT THE SENTENCE. A refusal a caller can only detect by
			// matching prose is one a reword silently breaks — and these routes
			// meet ordinary transient 503s too, which this one must be
			// distinguishable from because it can never resolve by retrying.
			if v, ok := body[ProvisionerUnwiredField].(bool); !ok || !v {
				t.Errorf("%s: 503 body carries no %s:true, so a client cannot tell this "+
					"permanent refusal from a transient outage and will retry for ever.\n"+
					"    body: %s", route, ProvisionerUnwiredField, truncate(rec.Body.String(), 300))
			}
		})
	}
}

// TestTheProvisionerRefusalIsBehindItsOwnAuthDoor pins the WRAPPER ORDER.
//
// 🔴 THE ORDER IS A DISCLOSURE BOUNDARY, NOT A STYLE CHOICE. Each wrapper
// tells the caller which dependencies this build has wired. Registered OUTSIDE
// the auth wrapper it would answer that to anyone who can reach the port, which
// on this service is anyone on the LAN NodePort — turning nine routes into an
// unauthenticated probe of the server's configuration. So an ANONYMOUS request
// must meet the auth refusal, never the provisioner one.
func TestTheProvisionerRefusalIsBehindItsOwnAuthDoor(t *testing.T) {
	_, h := unwiredProvisionerServer(t)

	for _, route := range allProvisionerRoutes() {
		t.Run(route, func(t *testing.T) {
			method, pattern, _ := strings.Cut(route, " ")
			path := strings.NewReplacer("{id}", "1", "{name}", "chief").Replace(pattern)
			rec := httptest.NewRecorder()
			// No session cookie, no hook token: an anonymous caller.
			h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))

			if strings.Contains(rec.Body.String(), ProvisionerUnwiredField) {
				t.Fatalf("%s told an ANONYMOUS caller which dependencies this build has "+
					"wired (%d, body contains %s). The wrapper must be registered "+
					"INSIDE the route's auth wrapper, not outside it.\n    body: %s",
					route, rec.Code, ProvisionerUnwiredField, truncate(rec.Body.String(), 300))
			}
			if rec.Code == http.StatusOK {
				t.Fatalf("%s answered 200 to an anonymous caller", route)
			}
		})
	}
}

// TestEveryUnguardedProvisionerCallerIsLedgered is the STRUCTURAL half.
//
// 🔴 IT IS WHAT MAKES A TENTH ROUTE IMPOSSIBLE TO ADD QUIETLY. The behavioural
// test above can only check routes somebody remembered to put in the ledger; a
// new handler calling s.ext.Provisioner.Foo() with no wrapper would be invisible
// to it and would answer 200-and-do-nothing exactly as the nine did. This walks
// the package's own AST instead, so the set is DERIVED and the ledger is the
// claim being checked against it.
func TestEveryUnguardedProvisionerCallerIsLedgered(t *testing.T) {
	for _, dep := range provisionerDeps {
		t.Run(dep.field, func(t *testing.T) { checkUnguardedCallers(t, dep) })
	}
}

// checkUnguardedCallers is the per-dependency body. It is a function rather than
// an inline closure so the positive controls below are attributed to the row that
// failed them: "the walk found no unguarded Gateway caller" is a different finding
// from the same sentence about Provisioner, and a shared failure message would
// have made them indistinguishable.
func checkUnguardedCallers(t *testing.T, dep provisionerDep) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var guarded, unguarded []string
	scanned := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		scanned++
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			calls, nilChecked := inspectProvisionerUse(fn.Body, dep.field)
			if !calls {
				return true
			}
			name := funcIdent(fn)
			if nilChecked {
				guarded = append(guarded, name)
			} else {
				unguarded = append(unguarded, name)
			}
			return true
		})
	}

	// 🔴 POSITIVE CONTROLS, BOTH DIRECTIONS, REPORTED AS A PAIR. "0 unledgered"
	// from a walker that parsed nothing is indistinguishable from a clean tree.
	if scanned < 10 {
		t.Fatalf("positive control FAILED: only %d non-test file(s) were parsed in this "+
			"package, which declares far more. The walk is not measuring.", scanned)
	}
	if len(guarded) == 0 {
		t.Logf("no nil-guarded %s caller found; the guarded/unguarded discrimination is "+
			"unexercised for this row (it IS exercised for Provisioner, where "+
			"instanceIndex and dismiss-destroy are written that way)", dep.field)
	}
	if len(unguarded) == 0 {
		t.Fatalf("positive control FAILED: the walk found no UNGUARDED %s caller at all. "+
			"Either every call site grew a nil check (in which case delete this row's "+
			"caller ledger deliberately) or the detection is matching nothing for this "+
			"field — which would make a clean verdict here meaningless.", dep.field)
	}

	sort.Strings(unguarded)
	want := append([]string(nil), dep.callers...)
	sort.Strings(want)

	if strings.Join(unguarded, "\n") != strings.Join(want, "\n") {
		t.Errorf("the set of functions that call s.ext.%s with NO nil check has "+
			"changed.\n  derived from source: %v\n  ledger:              %v\n"+
			"\n  GREW? A new function reaches the provisioner unguarded. Every route that "+
			"can reach it must be registered behind that dependency's wrapper and added to "+
			"that dependency's routes ledger — otherwise it answers 200 and does nothing on the "+
			"deployment cmd/muster-server actually ships, which is the failure this file "+
			"records.\n  SHRANK? A call site grew its own nil check or moved. Confirm the "+
			"route still refuses before removing it from the ledger.\n"+
			"\n  Guarded (informational, not checked): %v",
			dep.field, unguarded, want, guarded)
	}
	t.Logf("%d file(s) parsed; %d unguarded %s caller(s), %d guarded",
		scanned, len(unguarded), dep.field, len(guarded))
}

// inspectProvisionerUse reports whether body CALLS a method on s.ext.<field>, and
// whether body also compares s.ext.<field> against nil.
//
// ⚠ A TYPE ASSERTION IS NOT A CALL. `s.ext.Provisioner.(ProfileReapplier)` is an
// ast.TypeAssertExpr, is nil-safe, and must not put its function on the
// unguarded list — doing so would demand a wrapper for a branch that simply is
// not taken.
func inspectProvisionerUse(body *ast.BlockStmt, field string) (calls, nilChecked bool) {
	ast.Inspect(body, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.CallExpr:
			sel, ok := e.Fun.(*ast.SelectorExpr)
			if ok && isProvisionerExpr(sel.X, field) {
				calls = true
			}
		case *ast.BinaryExpr:
			if e.Op != token.EQL && e.Op != token.NEQ {
				return true
			}
			if isProvisionerExpr(e.X, field) && isNilIdent(e.Y) {
				nilChecked = true
			}
			if isProvisionerExpr(e.Y, field) && isNilIdent(e.X) {
				nilChecked = true
			}
		}
		return true
	})
	return calls, nilChecked
}

// isProvisionerExpr reports whether e is the expression `s.ext.<field>`.
func isProvisionerExpr(e ast.Expr, field string) bool {
	outer, ok := e.(*ast.SelectorExpr)
	if !ok || outer.Sel.Name != field {
		return false
	}
	mid, ok := outer.X.(*ast.SelectorExpr)
	if !ok || mid.Sel.Name != "ext" {
		return false
	}
	recv, ok := mid.X.(*ast.Ident)
	return ok && recv.Name == "s"
}

func isNilIdent(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}

// funcIdent spells a FuncDecl the way the ledger does: `(*Server).method`.
func funcIdent(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	switch t := fn.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return "(*" + id.Name + ")." + fn.Name.Name
		}
	case *ast.Ident:
		return t.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// routeRegistration is one route-registration site in this package's non-test
// sources: `mux.HandleFunc(pattern, handler)` or `mux.Handle(pattern, handler)`.
//
// 🔴 BOTH, BECAUSE THE Mux INTERFACE HAS BOTH. Matching only HandleFunc would
// make this walk narrower than the sentence it is written under — a route
// registered as `mux.Handle("POST /x", http.HandlerFunc(s.handleX))` would be
// invisible to the derived set AND to the reachability check, which is worse
// than being caught by neither on purpose. Nothing in this package registers a
// provisioner route that way today; the point is that doing so later cannot be
// silent.
type routeRegistration struct {
	route    string // the literal pattern, e.g. "POST /agents"
	handler  string // the innermost function the wrappers close over
	wrapped  bool   // the registration expression calls this row's wrapper
	computed bool   // the pattern is not a string literal
	file     string
	line     int
}

// unnameableRouteAllowlist is the EXHAUSTIVE set of routes whose handler
// expression resolves to no named function, and which are therefore outside the
// reachability verdict below. The value is the reason, and it is required: an
// entry with no reason is not an exemption, it is a skip with a comment.
//
// 🔴 IT REPLACED A BARE `continue`, WHICH IS WHY IT EXISTS. The reachability
// half used to skip every unnameable handler silently, so the sentence "every
// registered route whose handler can reach s.ext.Provisioner unguarded MUST be
// wrapped" was quietly false for however many routes the walk could not name —
// and a THIRD such route would have joined them without a word. Now the two
// below are named and anything else fails.
//
// ⚠ THE BAR FOR ADDING ONE IS THAT THE HANDLER PROVABLY HOLDS NO REFERENCE TO
// s. Both of these are built by a zero-argument constructor over package-level
// state, so neither can reach s.ext.Provisioner at all — they are not
// "unchecked", they are outside the seam. A handler that closes over s does not
// belong here; give it a name on *Server instead.
var unnameableRouteAllowlist = map[string]string{
	"GET /metrics": "promhttp.Handler() — built by the Prometheus client over the default registry; takes no Server",
	"GET /static/": "staticHandler() — a zero-argument constructor over the embedded web FS; takes no Server",
}

// TestEveryProvisionerRouteIsDerivedNotDeclared is the half that makes
// each row's "GROWS *or* SHRINKS" sentence true.
//
// 🔴 IT EXISTS BECAUSE THAT SENTENCE WAS MEASURED FALSE. The ledger was
// hand-written and nothing derived the wrapped set, so shrinking it was a
// two-line edit — unwrap the route, delete its line — and the whole module
// stayed green while the shipped deployment went back to answering 200 and
// doing nothing. The route ledger sat four lines from a caller ledger that IS
// derived from the source; this closes the asymmetry.
//
// 🔴 THE REACHABILITY HALF IS THE ONE THAT KILLS THE TWO-LINE EDIT, AND IT IS
// WORTH SAYING WHY A derived==declared CHECK ALONE DOES NOT. Unwrap a route and
// delist it and the derived set still equals the declared one — both simply
// shrank by the same entry. What does not change is that the route's handler
// can still reach s.ext.Provisioner with no nil check, and that is the property
// the wrapper exists for. So the wrapper requirement is decided by the CALL
// GRAPH, with the ledger out of the loop entirely.
//
// ⚠ THE REACHABILITY IS FUNCTION-GRANULAR AND INHERITS THE CALLER LEDGERS'
// KNOWN LIMIT, deliberately: a function that compares s.ext.Provisioner against
// nil ANYWHERE in its body is treated as guarded and stops propagation. That
// under-approximates — a function that checks on one path and calls on another
// hides everything below it — and it is the same approximation the caller
// ledger declares, so the two halves cannot disagree about what "guarded" means.
// Closing it needs dominance analysis; read a clean verdict here as "no
// registered route reaches the provisioner through an unguarded chain", never
// as "every call site is dominated by a check".
//
// ⚠ AND READ IT OVER THE ROUTES THE VERDICT ACTUALLY COVERS, WHICH IS NOT ALL OF
// THEM. Two routes are exempt by name in unnameableRouteAllowlist because their
// handler expression resolves to no function this walk can follow. That
// exemption used to be a silent `continue` over every unnameable handler and the
// count was unread; it is an enumerated list and a printed number now, so the
// set can no longer grow without failing — but the two on it are still outside
// this verdict, not inside it and clean.
func TestEveryProvisionerRouteIsDerivedNotDeclared(t *testing.T) {
	for _, dep := range provisionerDeps {
		t.Run(dep.field, func(t *testing.T) { checkRoutesDerived(t, dep) })
	}
}

// checkRoutesDerived is the per-dependency body. Split out for the same reason as
// checkUnguardedCallers: a positive control that cannot name which row it failed
// for is a control nobody can act on.
func checkRoutesDerived(t *testing.T, dep provisionerDep) {
	regs, reaches, declared, scanned := scanRouteRegistrations(t, dep)

	// 🔴 POSITIVE CONTROLS, REPORTED AS NUMBERS. A walk that parsed nothing
	// derives an empty set, and an empty set compared against an empty ledger is
	// a green computed from silence. Each of these is a producer: the number has
	// to be able to move.
	if scanned < 10 {
		t.Fatalf("positive control FAILED: only %d non-test file(s) parsed in this "+
			"package, which declares far more. The walk is not measuring.", scanned)
	}
	if len(regs) < 50 {
		t.Fatalf("positive control FAILED: only %d route registration(s) were found, and "+
			"testdata/routes.golden records well over a hundred. The HandleFunc "+
			"detection has stopped matching, so every verdict below is over an empty "+
			"or truncated set.", len(regs))
	}
	// 🔴 POSITIVE CONTROL FOR THE `Handle` ARM SPECIFICALLY. The walk matches
	// Handle AND HandleFunc because the Mux interface has both, and the count
	// above cannot tell whether the Handle arm is matching — 95 with it broken
	// and 93 are both comfortably over the floor. These two are the package's
	// only mux.Handle registrations, so their absence means a route registered
	// that way has become invisible to BOTH checks below.
	for _, want := range []string{"GET /metrics", "GET /static/"} {
		found := false
		for _, r := range regs {
			if r.route == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("positive control FAILED: %q is registered with mux.Handle in this "+
				"package and the walk did not find it. The Handle arm has stopped "+
				"matching, so a provisioner route registered as "+
				"mux.Handle(pattern, http.HandlerFunc(h)) would be invisible to the "+
				"derived set AND to the reachability check.", want)
		}
	}

	reachable := 0
	for _, r := range regs {
		if r.handler != "" && reaches(r.handler) {
			reachable++
		}
	}
	if reachable == 0 {
		t.Fatalf("positive control FAILED: not ONE of the %d registered route(s) was "+
			"found to reach s.ext.Provisioner, and nine of them call it. The call-graph "+
			"walk is inert, so the \"reaches but is not wrapped\" verdict below is a "+
			"silence rather than a result.", len(regs))
	}

	// --- half 1: DERIVED == DECLARED ---------------------------------------
	var derived []string
	for _, r := range regs {
		if r.wrapped && !r.computed {
			derived = append(derived, r.route)
		}
	}
	sort.Strings(derived)
	want := append([]string(nil), dep.routes...)
	sort.Strings(want)
	if strings.Join(derived, "\n") != strings.Join(want, "\n") {
		t.Errorf("the set of routes REGISTERED behind %s is not the set "+
			"its ledger row declares.\n  derived from source: %v\n  ledger:          "+
			"    %v\n"+
			"\n  SHRANK? A route lost its wrapper. On the deployment cmd/muster-server "+
			"ships it is back to answering 200 and doing nothing — a card rendered over a "+
			"row that never changes. Restore the wrapper; do not delete the ledger line to "+
			"match.\n  GREW? A route was wrapped without being recorded. Add it here so "+
			"the behavioural test above actually drives it — a wrapped route missing from "+
			"this list is never requested by any test in this file.",
			dep.wrapper, derived, want)
	}

	// --- half 2: REACHABILITY DECIDES, NOT THE LEDGER -----------------------
	//
	// 🔴 A ROUTE THIS WALK CANNOT RESOLVE IS A FAILURE, NOT A SKIP. Both
	// counters below are producers and are printed at the end of this test:
	// `emptyHandler` is every route whose handler expression resolves to no name
	// at all — exempt ONLY if unnameableRouteAllowlist names it, an error
	// otherwise — and `unresolvedHandler` is every route whose candidate name
	// corroborated against no FuncDecl this walk parsed. Either number moving is
	// a route that left the reachability verdict's coverage, which is precisely
	// how that verdict quietly narrows.
	emptyHandler, unresolvedHandler := 0, 0
	for _, r := range regs {
		if r.handler == "" {
			emptyHandler++
			if r.computed || unnameableRouteAllowlist[r.route] == "" {
				name := r.route
				if r.computed {
					name = "<computed pattern>"
				}
				t.Errorf("%s (%s:%d) is registered with a handler expression this walk "+
					"cannot resolve to a named function, and it is not on "+
					"unnameableRouteAllowlist.\n"+
					"    It is therefore EXCLUDED from the reachability verdict below — "+
					"if its handler reaches s.ext.Provisioner unguarded, nothing here "+
					"says so, and on the deployment cmd/muster-server ships (Provisioner "+
					"nil by design) that route answers 200 and does nothing.\n"+
					"    Register it as a named method on *Server so the call graph can "+
					"see it, or — only if it provably holds no reference to s — add it to "+
					"unnameableRouteAllowlist with the reason.",
					name, r.file, r.line)
			}
			continue
		}
		if !declared[r.handler] {
			unresolvedHandler++
			name := r.route
			if r.computed {
				name = "<computed pattern>"
			}
			t.Errorf("%s (%s:%d) names handler %q, which is not a function declared in "+
				"this package.\n"+
				"    innermostRouteHandler produces a CANDIDATE name out of an expression "+
				"with no type information behind it, so this is what a LOCAL VARIABLE "+
				"looks like: `h := s.requireSession(s.handleX); mux.HandleFunc(p, h)`. "+
				"Left unchecked, %q is in no call graph, reaches() answers false, and the "+
				"route is scored SAFE whether or not its handler touches the "+
				"provisioner — which is the exact two-line edit this test exists to "+
				"kill.\n"+
				"    Register the handler expression inline — "+
				"s.requireSession(s.%s(s.handleX)) — rather than through "+
				"a local.",
				name, r.file, r.line, r.handler, r.handler, dep.wrapper)
			continue
		}
		can := reaches(r.handler)
		switch {
		case can && !r.wrapped:
			name := r.route
			if r.computed {
				name = "<computed pattern>"
			}
			t.Errorf("%s (%s:%d) is registered WITHOUT %s, and its handler "+
				"%s reaches s.ext.%s with no nil check.\n"+
				"    cmd/muster-server leaves it nil on purpose (doc_seams.go "+
				"entry 1), so on the deployment this tree ships that route either answers "+
				"200 and does nothing or panics in a goroutine. Wrap it — "+
				"s.requireSession(s.%s(h)), auth OUTSIDE — and add it to "+
				"that dependency's routes ledger.\n"+
				"    Deleting the ledger entry does NOT satisfy this check: the ledger is "+
				"not consulted here, the call graph is.",
				name, r.file, r.line, dep.wrapper, r.handler, dep.field, dep.wrapper)
		case !can && r.wrapped:
			t.Errorf("%s (%s:%d) is wrapped in %s, but its handler %s was "+
				"NOT found to reach s.ext.%s through this package's call graph.\n"+
				"    Either the wrapper is now unnecessary — in which case remove it AND "+
				"its routes-ledger line, having first confirmed nothing below it "+
				"calls the provisioner — or the call-graph walk has stopped resolving a "+
				"call it used to resolve, which would make the \"reaches but is not "+
				"wrapped\" half above silently blind. Every wrapped route on this row "+
				"reached when this was written; a new failure here is far more likely to "+
				"be the second.",
				r.route, r.file, r.line, dep.wrapper, r.handler, dep.field)
		}
	}
	t.Logf("%d file(s) parsed; %d function(s) declared; %d route registration(s), "+
		"%d wrapped, %d reaching the provisioner; %d with an unnameable handler "+
		"(%d allowlisted), %d unresolved",
		scanned, len(declared), len(regs), len(derived), reachable,
		emptyHandler, len(unnameableRouteAllowlist), unresolvedHandler)
}

// scanRouteRegistrations parses this package's non-test sources and returns
// every route registration (Handle and HandleFunc alike), a predicate answering
// whether a named function can reach s.ext.Provisioner unguarded through the
// package call graph, and `declared` — every function name this walk actually
// parsed a FuncDecl for.
//
// 🔴 `declared` IS NOT BOOKKEEPING; IT IS WHAT TURNS A HANDLER NAME FROM A GUESS
// INTO A FACT. innermostRouteHandler produces a CANDIDATE name out of an
// expression, with no type information behind it — a local variable and a
// method value are the same shape to it. A candidate that names nothing this
// walk declared is an unresolved handler, and the caller MUST fail on it rather
// than hand it to `reaches`, which would answer false and score the route safe.
func scanRouteRegistrations(t *testing.T, dep provisionerDep) (regs []routeRegistration, reaches func(string) bool, declared map[string]bool, scanned int) {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	callees := map[string][]string{}
	direct := map[string]bool{}
	guarded := map[string]bool{}
	declared = map[string]bool{}

	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		scanned++
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok {
				return true
			}
			name := funcIdent(fn)
			// Recorded BEFORE the body check: a bodiless declaration is still a
			// function a route may legitimately name, and leaving it out of
			// `declared` would turn a valid registration into a spurious failure.
			declared[name] = true
			if fn.Body == nil {
				return true
			}
			calls, nilChecked := inspectProvisionerUse(fn.Body, dep.field)
			if calls && nilChecked {
				guarded[name] = true
			}
			if calls && !nilChecked {
				direct[name] = true
			}
			ast.Inspect(fn.Body, func(m ast.Node) bool {
				c, ok := m.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fu := c.Fun.(type) {
				case *ast.SelectorExpr:
					// 🔴 Fun POSITION ONLY. `s.requireSession(s.handleAgentCreate)`
					// PASSES handleAgentCreate as a value; counting that as a call
					// would make registerAll reach every handler in the package and
					// the reachability verdict would be "everything", which is the
					// same as nothing.
					if id, ok := fu.X.(*ast.Ident); ok && id.Name == "s" {
						callees[name] = append(callees[name], "(*Server)."+fu.Sel.Name)
					}
					if (fu.Sel.Name == "HandleFunc" || fu.Sel.Name == "Handle") && len(c.Args) == 2 {
						pos := fset.Position(c.Pos())
						r := routeRegistration{
							handler: innermostRouteHandler(c.Args[1]),
							file:    f,
							line:    pos.Line,
						}
						if lit, ok := c.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							if s, err := strconv.Unquote(lit.Value); err == nil {
								r.route = s
							} else {
								r.computed = true
							}
						} else {
							// e.g. `"GET /"+tab` in the tab-route loop. Its pattern
							// cannot be compared against the ledger, but it is still
							// held to the reachability rule — a wrapped route must
							// not be able to hide behind a computed pattern.
							r.computed = true
						}
						ast.Inspect(c.Args[1], func(k ast.Node) bool {
							cc, ok := k.(*ast.CallExpr)
							if !ok {
								return true
							}
							if se, ok := cc.Fun.(*ast.SelectorExpr); ok && se.Sel.Name == dep.wrapper {
								r.wrapped = true
							}
							return true
						})
						regs = append(regs, r)
					}
				case *ast.Ident:
					callees[name] = append(callees[name], fu.Name)
				}
				return true
			})
			return true
		})
	}

	// Memoised DFS. 0 = in progress or proven unreachable, 1 = reaches; an
	// in-progress node reads as unreachable, which terminates recursion on a
	// cycle without claiming a path this walk has not established.
	memo := map[string]int{}
	var walk func(string) bool
	walk = func(n string) bool {
		if v, ok := memo[n]; ok {
			return v == 1
		}
		memo[n] = 0
		if guarded[n] {
			return false // the declared function-granular limit; see the caller
		}
		if direct[n] {
			memo[n] = 1
			return true
		}
		for _, c := range callees[n] {
			if walk(c) {
				memo[n] = 1
				return true
			}
		}
		return false
	}
	return regs, walk, declared, scanned
}

// innermostRouteHandler produces a CANDIDATE name for the function a
// registration expression ultimately serves: it descends through each wrapper's
// first argument until it reaches something that is not a call.
//
// 🔴 A CANDIDATE, NOT A VERDICT — THE CALLER MUST CORROBORATE IT AGAINST
// `declared`, AND THIS COMMENT USED TO CLAIM MORE THAN THE CODE DID. It said
// only that the function "returns "" rather than guessing", which is true of the
// function-literal case and was FALSE of the identifier case one line below:
// `create := s.requireSession(s.handleAgentCreate); mux.HandleFunc("POST
// /agents", create)` yielded "create", a LOCAL VARIABLE. That name is in no call
// graph, so reaches() answered false and the route was scored safe — measured
// green across the whole module while the shipped nil-provisioner deployment
// answered 200 over a row stuck in `provisioning` for ever. There is no type
// information here, so a local variable, a struct field and a method value are
// all the same shape; the only honest thing this function can do is hand back a
// name to be checked.
//
// ⚠ IT STILL RETURNS "" FOR SHAPES THAT HAVE NO NAME AT ALL — a function
// literal, or a handler built by a zero-argument constructor. "" is not a pass:
// the caller holds those to unnameableRouteAllowlist.
//
// ⚠ RESIDUAL LIMIT, STATED BECAUSE THE CORROBORATION DOES NOT CLOSE IT: a local
// variable whose name happens to COLLIDE with a function declared in this
// package would corroborate, and the verdict would then be about that function
// rather than about the route. Closing it needs scope resolution. The mutant
// above is caught; a deliberately-shadowing one is not.
func innermostRouteHandler(e ast.Expr) string {
	for {
		switch v := e.(type) {
		case *ast.CallExpr:
			if len(v.Args) == 0 {
				return ""
			}
			e = v.Args[0]
		case *ast.SelectorExpr:
			if id, ok := v.X.(*ast.Ident); ok && id.Name == "s" {
				return "(*Server)." + v.Sel.Name
			}
			return ""
		case *ast.Ident:
			// A bare identifier. It may name a package-level handler
			// (handleNoContent) or a local variable holding an http.HandlerFunc —
			// this walk cannot tell the two apart. Returned as a candidate; the
			// caller's `declared` check is what decides, and rejects the second.
			return v.Name
		default:
			return ""
		}
	}
}

// TestNoBackgroundGoroutineInThisPackageIsWrittenBare pins the F1 class.
//
// 🔴 A PANIC IN A GOROUTINE WITH NO recover TERMINATES THE PROCESS, AND
// recoverMiddleware DOES NOT COVER ONE. handleChiefProvision was the single bare
// `go func` among eight background goroutines in this package; two POSTs to that
// route took the container to `exited (2)` with /health connection-refused, on an
// ordinary operator sequence — click once, nothing happens, click again. safeGo
// is the house pattern and this asserts it is the ONLY one.
//
// ⚠ THE ONE PERMITTED SITE IS safeGo's OWN BODY, which is where the recover
// lives. Wrapping it in itself is infinite recursion, so it is exempted by
// LOCATION (the function named safeGo) rather than by a comment a reword could
// remove.
func TestNoBackgroundGoroutineInThisPackageIsWrittenBare(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	type site struct {
		fn   string
		file string
		line int
	}
	var bare []site
	total := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			ast.Inspect(fn.Body, func(m ast.Node) bool {
				g, ok := m.(*ast.GoStmt)
				if !ok {
					return true
				}
				total++
				if fn.Name.Name == "safeGo" {
					return true // the recover machinery itself
				}
				if goStmtHasRecover(g) {
					return true
				}
				bare = append(bare, site{fn.Name.Name, f, fset.Position(g.Pos()).Line})
				return true
			})
			return true
		})
	}

	// Positive control: the walk must be able to SEE a go statement at all.
	if total == 0 {
		t.Fatal("positive control FAILED: no `go` statement was found anywhere in this " +
			"package, and safeGo alone contains one. The walk is not measuring, so the " +
			"clean verdict below would be a silence rather than a result.")
	}

	for _, s := range bare {
		t.Errorf("BARE goroutine at %s:%d, in %s.\n"+
			"    A panic here has no recover and terminates the PROCESS — "+
			"recoverMiddleware covers the request goroutine only, and by the time this "+
			"runs the response has usually already been written. Use "+
			"safeGo(s.logger, \"<what>\", func(){...}); its recover logs the panic and "+
			"increments muster_panics_total{source=\"goroutine\"}.",
			s.file, s.line, s.fn)
	}
	t.Logf("%d `go` statement(s) in non-test sources; %d bare", total, len(bare))
}

// goStmtHasRecover reports whether a go statement's own function literal begins
// with a deferred recover, which is what makes a bare one safe.
func goStmtHasRecover(g *ast.GoStmt) bool {
	lit, ok := g.Call.Fun.(*ast.FuncLit)
	if !ok || lit.Body == nil {
		return false
	}
	found := false
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		d, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		ast.Inspect(d, func(m ast.Node) bool {
			if id, ok := m.(*ast.Ident); ok && id.Name == "recover" {
				found = true
			}
			return true
		})
		return true
	})
	return found
}

// ---------------------------------------------------------------------------
// THE PROPERTY CONVERTING goNotify/mirrorToRouter TO safeGo COULD HAVE BROKEN.
// ---------------------------------------------------------------------------

// panickingRouter is a RouterPort whose fan-out methods panic, which is the case
// the conversion exists for. It is built from stubRouter so only the two methods
// under test are overridden.
type panickingRouter struct{ stubRouter }

func (panickingRouter) Notify(context.Context, RouterNotification) error {
	panic("router client exploded mid-notify")
}

func (panickingRouter) PublishEvent(context.Context, string, string) error {
	panic("router client exploded mid-publish")
}

// panicInGoroutineLog is the fragment safeGo's recover writes for every panic it
// catches. It mirrors that Printf's format string; a reword there makes the wait
// below time out and the positive control go RED, which is the direction that
// costs a confusing failure rather than a silent pass.
const panicInGoroutineLog = "PANIC in goroutine"

// recoveredPanicLog is a log sink that reports each recovered-goroutine-panic
// line safeGo writes, so a test can block until the recovers have actually run.
//
// 🔴 THIS IS THE ORDERING THE POSITIVE CONTROL BELOW NEEDS, AND ISSUE #20 IS WHAT
// HAPPENS WITHOUT IT. safeGo registers its recover in the OUTER goroutine, so it
// unwinds AFTER fn's own `defer s.pushInFlight.Done()` — that is the property the
// test exists to protect, and it is also why WaitForPushes can return BEFORE
// either recover has incremented muster_panics_total{source="goroutine"}. Reading
// the counter once, the instant the wait returns, is therefore a race — one CI
// hit, and one that reproduces locally at a few runs in a hundred on unmodified
// main, every failure reading a delta of 1. The commit carries the rates.
//
// WaitForPushes is not the thing to fix. Its contract is that the fan-out WORK
// has finished (cmd/muster-server's shutdown waits on it so a notification is not
// dropped on exit); nothing in the service reads this counter, least of all at
// shutdown, when /metrics has already been drained. The test was reading an
// unordered value, so the test is what gains an ordering.
//
// The edge is the recover's own body: it increments the counter and THEN logs.
// Receiving the line therefore orders the reader after the increment. It also
// ATTRIBUTES the panics in a way the process-global counter cannot — this logger
// belongs to this test's Server, so a line arriving here came from these two
// fan-outs and not from another test's leftover goroutine.
type recoveredPanicLog struct {
	mu     sync.Mutex
	lines  []string
	landed chan struct{}
}

func newRecoveredPanicLog() *recoveredPanicLog {
	// Buffered so Write never blocks on a test that has stopped reading; 64 is far
	// more than any caller waits for.
	return &recoveredPanicLog{landed: make(chan struct{}, 64)}
}

// Write records the line and, if it is one of safeGo's recovered-panic lines,
// signals it. log.Logger emits exactly one Write per Printf, so one recovered
// panic is one signal.
func (l *recoveredPanicLog) Write(p []byte) (int, error) {
	line := string(p)
	l.mu.Lock()
	l.lines = append(l.lines, line)
	l.mu.Unlock()
	if strings.Contains(line, panicInGoroutineLog) {
		select {
		case l.landed <- struct{}{}:
		default:
		}
	}
	return len(p), nil
}

// waitFor blocks until n recovered-goroutine-panic lines have been logged, or
// returns an error naming how many it saw and what the log held instead.
func (l *recoveredPanicLog) waitFor(n int, within time.Duration) error {
	deadline := time.After(within)
	for i := 0; i < n; i++ {
		select {
		case <-l.landed:
		case <-deadline:
			l.mu.Lock()
			held := strings.Join(l.lines, "")
			l.mu.Unlock()
			if held == "" {
				held = "    (nothing at all was logged)\n"
			}
			return fmt.Errorf("saw %d of %d %q log line(s) within %s; the server logged:\n%s",
				i, n, panicInGoroutineLog, within, held)
		}
	}
	return nil
}

// TestAPanickingFanOutStillReleasesTheShutdownWait is the guard for the ONE
// invariant the safeGo conversion could have silently broken.
//
// 🔴 pushInFlight's CONTRACT IS Add-ON-THE-CALLER, Done-IN-THE-GOROUTINE, AND A
// recover PLACED WRONG WOULD EAT THE Done. safeGo registers its recover OUTSIDE
// fn, so fn's own `defer s.pushInFlight.Done()` unwinds FIRST and the counter is
// released before the panic is caught. Had it been written the other way — the
// recover inside fn, above the defer — a panicking fan-out would leave the
// WaitGroup at 1 and every shutdown would hang to its full pushDrainTimeout,
// with nothing in the log naming why. That is a deadlock, which is strictly
// worse than the crash this conversion removed, so it gets its own assertion
// rather than riding on "the tests still pass".
//
// ⚠ THE PRE-CONVERSION CODE COULD NOT HAVE PASSED THIS TEST AT ALL — it would
// have taken the test binary down with it. That is the measurement: red by
// process death before, green after.
//
// ⚠ THE POSITIVE CONTROL IS ORDERED AGAINST THE RECOVERS, NOT AGAINST
// WaitForPushes (issue #20). The two are different events and the second happens
// first — recoveredPanicLog's header carries the mechanism and the failure rate.
func TestAPanickingFanOutStillReleasesTheShutdownWait(t *testing.T) {
	// The server logs through a sink the test can watch, which is what lets the
	// positive control below read the counter only after both recovers have run.
	logs := newRecoveredPanicLog()
	s := New(nil, AuthConfig{}, log.New(logs, "", 0))
	s.UseRouter(panickingRouter{})

	// A DELTA, not an absolute read: metrics.Panics is a process-global counter
	// that other tests in this package also move, so an absolute threshold would
	// pass on their increments rather than on this test's.
	before := goroutinePanicCount()

	s.goNotify(1, push.EventReview, RouterNotification{Tag: "a-tag"}, nil) // user 1: admin with no users store
	s.mirrorToRouter(1, "an-event", "some-data")                           // user 1: admin with no users store

	done := make(chan struct{})
	go func() {
		s.WaitForPushes()
		close(done)
	}()

	select {
	case <-done:
		// Both fan-outs panicked, both were recovered, and both released their
		// slot. The process is still here to assert that.
	case <-time.After(5 * time.Second):
		t.Fatal("WaitForPushes did not return within 5s after two PANICKING fan-outs.\n" +
			"    pushInFlight was left holding a slot, so every shutdown from now on " +
			"blocks for the full pushDrainTimeout and then exits anyway, logging a " +
			"drain timeout that names nothing. Check that Done() is fn's own defer " +
			"and that safeGo's recover is registered OUTSIDE fn — if the recover is " +
			"inside fn above the defer, it swallows the unwind that would have run it.")
	}

	// 🔴 POSITIVE CONTROL, PART 1: WAIT FOR THE RECOVERS. WaitForPushes returning
	// says the fan-out work finished; it does NOT say either recover has run, so
	// the counter read below is only meaningful after this. With the panics deleted
	// from panickingRouter no line ever arrives and this is where the control goes
	// red — which is the whole point of it.
	if err := logs.waitFor(2, 5*time.Second); err != nil {
		t.Fatalf("positive control FAILED: safeGo did not report two recovered panics: %v\n"+
			"    The fan-outs did not panic (or were not recovered), so the wait "+
			"returning proves nothing about recovery — this test would read exactly "+
			"the same against a router that works.", err)
	}

	// 🔴 POSITIVE CONTROL, PART 2: THE PANICS MUST ACTUALLY HAVE BEEN COUNTED.
	// Without this the test passes just as happily against a router that never
	// panicked — and would keep passing if someone deleted the panic from the stub,
	// which is the "green for the wrong reason" shape. The recovered-panic counter
	// is the signal a Prometheus scrape would see, and the log wait above is what
	// makes reading it here sound rather than a race.
	if got := goroutinePanicCount() - before; got < 2 {
		t.Fatalf("positive control FAILED: muster_panics_total{source=\"goroutine\"} rose "+
			"by %v across these two fan-outs, want at least 2. The fan-outs did not "+
			"panic, so the wait returning proves nothing about recovery — this test "+
			"would read exactly the same against a router that works.", got)
	}
}

// goroutinePanicCount reads the recovered-goroutine-panic counter safeGo writes.
func goroutinePanicCount() float64 {
	return testutil.ToFloat64(metrics.Panics.WithLabelValues("goroutine"))
}
