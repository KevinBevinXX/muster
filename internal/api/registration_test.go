package api

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/github"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE ROUTE SET MUST NOT DEPEND ON WHAT IS WIRED IN.
//
// 🔴 THIS IS A RELATIONSHIP GUARD, NOT A COUNT. It records the routes twice —
// once from a bare Server, once from one with every dependency supplied — and
// demands the two sets be EQUAL. A count would pass whether or not they agree,
// which is the shape of guard this arc keeps finding: a check whose description
// claims a relationship while its body inspects one side.
//
// What it defends: RegisterRoutes registers against a zero-value Server, and
// the route golden is recorded through RegisterRoutes. If any registrar ever
// grows an `if s.ext.X == nil { return }`, the golden silently becomes a record
// of the EMPTY configuration while production registers more — and every
// comparison against it passes. Upstream that exact hole cost seven routes with
// two guards green over it.
// ---------------------------------------------------------------------------

// fullyWiredServer returns a Server with every optional dependency non-nil.
//
// 🔴 EVERY FIELD OF Extensions MUST BE SET HERE, AND TestFullyWiredServerSetsEveryExtensionField
// FAILS WHEN THE STRUCT GROWS. A fixture that is merely "quite full" makes the
// comparison below vacuous for exactly the field someone just added — which is
// the field most likely to arrive with a new conditional branch.
func fullyWiredServer(t *testing.T) *Server {
	t.Helper()
	s := New(nil, AuthConfig{}, log.New(os.Stderr, "", 0))
	s.UseExtensions(Extensions{
		Notes:           stubNotesStore{},
		Agents:          stubAgentsStore{},
		GitHub:          stubGitHubStore{},
		Users:           stubUsers{},
		APITokens:       stubAPITokens{},
		SessionLiveness: stubLiveness{},
		TagAutoDispatch: true,
		Provisioner:     stubProvisioner{},
		// 🔴 BOTH, because this fixture's whole claim is "every dependency wired".
		// api.Provisioner and api.Gateway are separate now, so setting only the first
		// would quietly make this an "almost everything" fixture — and a registration
		// test that does not wire a dependency cannot notice a route gated on it.
		Gateway: stubGateway{},
		Kinds:   stubKinds{},
		GitHubOAuth: GitHubOAuthConfig{ClientID: "id", ClientSecret: "secret", BaseURL: "http://example.test",
			StateKey: []byte("registration-test-state-key-01234567")},
		// A NON-DEFAULT prefix on purpose. agents.NamespacePrefix here would be
		// indistinguishable from the zero value's resolved behaviour, so this
		// fixture would claim "fully wired" while exercising the default — which is
		// the exact blindness that let the namespace defect ship. See
		// Extensions.AgentNamespacePrefix.
		AgentNamespacePrefix: "muster-agent-",
		Connectors:           stubConnectorsStore{},
		// NON-ZERO on purpose, for AgentNamespacePrefix's reason: the zero policy
		// is production's, so a fixture holding it would not be "wired".
		ConnectorPolicy: connectors.Policy{AllowLoopback: true},
		Mirror:          stubMirror{},
	})
	s.UseRouter(stubRouter{})
	return s
}

// stubMirror satisfies Mirror for registration only; any method call panics.
type stubMirror struct{ Mirror }

// stubConnectorsStore satisfies connectors.Store for registration only; any
// method call panics, which is the point — registering must call none.
type stubConnectorsStore struct{ connectors.Store }

func recordFrom(register func(Mux)) []string {
	rec := &muxRecorder{}
	register(rec)
	out := append([]string(nil), rec.patterns...)
	sort.Strings(out)
	return out
}

func TestRegistrationIsIndependentOfDependencies(t *testing.T) {
	bare := recordFrom(RegisterRoutes)
	wired := recordFrom(fullyWiredServer(t).registerAll)

	if d := diffRoutes(bare, wired); d != "" {
		t.Fatalf("the recorded route set DEPENDS ON WHAT IS WIRED IN, so testdata/routes.golden "+
			"describes a configuration rather than the code.\n"+
			"bare server registered %d routes, fully wired registered %d.\n"+
			"(- bare, + wired):\n%s\n\n"+
			"Fix the REGISTRAR, not this test: move the nil check out of the registration "+
			"path and into the handler, which answers its own missing-dependency case. "+
			"See the package doc in server.go.", len(bare), len(wired), d)
	}
	if len(bare) == 0 {
		t.Fatal("both recordings are EMPTY, so the comparison above proved nothing. " +
			"RegisterRoutes registered no routes at all.")
	}
}

// TestNoRegistrarBranchesOnADependency is the STRUCTURAL half, and it is the
// half that catches the case the comparison above cannot.
//
// 🔴 THE TWO ARE NOT REDUNDANT. The comparison sees a branch only when the
// fixture leaves the relevant dependency nil — so a branch on a dependency the
// fixture happens to set is INVISIBLE to it, and every branch anyone adds will
// be on a dependency they also remembered to add to the fixture. This test
// reads the registrars' own source instead, so it does not care what the
// fixture holds.
func TestNoRegistrarBranchesOnADependency(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var offenders []string
	scanned := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if !strings.HasPrefix(fn.Name.Name, "register") || fn.Recv == nil {
				continue
			}
			scanned++
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				ifs, ok := n.(*ast.IfStmt)
				if !ok {
					return true
				}
				if sel := selectorText(ifs.Cond); strings.Contains(sel, "s.ext.") {
					offenders = append(offenders,
						fset.Position(ifs.Pos()).String()+" in "+fn.Name.Name+": "+sel)
				}
				return true
			})
		}
	}
	// 🔴 POSITIVE CONTROL. A glob that matched nothing, a parser that silently
	// skipped everything, or a prefix that stopped matching each produce ZERO
	// offenders — indistinguishable from a clean tree. Assert the scanner
	// actually reached the registrars.
	if scanned < 5 {
		t.Fatalf("the scanner found only %d registrar(s); it is wired to nothing and a zero "+
			"result above would mean nothing. Expected every register*Routes method plus registerAll.", scanned)
	}
	if len(offenders) > 0 {
		t.Errorf("%d registrar(s) BRANCH ON A DEPENDENCY, so the recorded route set is a "+
			"function of the fixture rather than of the code:\n  %s\n\n"+
			"Move the check into the handler. See the package doc in server.go.",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

// selectorText renders a condition expression as dotted text, so `s.ext.Notes
// == nil` is greppable without matching an unrelated `ext` local.
func selectorText(e ast.Expr) string {
	var b strings.Builder
	var walk func(ast.Expr)
	walk = func(e ast.Expr) {
		switch v := e.(type) {
		case *ast.SelectorExpr:
			walk(v.X)
			b.WriteString("." + v.Sel.Name)
		case *ast.Ident:
			b.WriteString(v.Name)
		case *ast.BinaryExpr:
			walk(v.X)
			b.WriteString(" " + v.Op.String() + " ")
			walk(v.Y)
		}
	}
	walk(e)
	return b.String()
}

// TestFullyWiredServerSetsEveryExtensionField keeps the fixture above honest.
//
// 🔴 IT DERIVES THE FIELD SET BY REFLECTION rather than listing it, so a field
// added to Extensions fails here instead of quietly leaving the comparison
// vacuous for that field.
func TestFullyWiredServerSetsEveryExtensionField(t *testing.T) {
	s := fullyWiredServer(t)
	var unset []string
	v := reflectValueOfExtensions(s.ext)
	for name, isZero := range v {
		if isZero {
			unset = append(unset, name)
		}
	}
	sort.Strings(unset)
	if len(unset) > 0 {
		t.Fatalf("fullyWiredServer leaves %d Extensions field(s) at their zero value: %s\n"+
			"A conditional branch on any of them would be INVISIBLE to "+
			"TestRegistrationIsIndependentOfDependencies. Set them in the fixture.",
			len(unset), strings.Join(unset, ", "))
	}
}

// --- stubs -----------------------------------------------------------------
//
// These exist only to be non-nil. Every method that must exist to satisfy an
// interface is present; none is expected to be CALLED by the tests in this
// file, which record routes rather than serve them.

type stubLiveness struct{}

func (stubLiveness) SessionsExisting(context.Context, []string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

type stubRouter struct{ stubLiveness }

func (stubRouter) PublishEvent(context.Context, string, string) error { return nil }
func (stubRouter) Notify(context.Context, RouterNotification) error   { return nil }
func (stubRouter) Directories(context.Context, string, int) ([]string, error) {
	return nil, nil
}
func (stubRouter) SessionMeta(context.Context, string) (SessionMeta, bool, error) {
	return SessionMeta{}, false, nil
}

type stubGate struct{}

func (stubGate) MintGate(context.Context, GateSpec) (string, error) { return "id", nil }
func (stubGate) ReadGate(context.Context, string) (GateDecision, error) {
	return GateDecision{State: GateStatePending}, nil
}
func (stubGate) ClearGate(context.Context, string) error { return nil }

type stubProvisioner struct{}

func (stubProvisioner) Dispatch(context.Context, int64, bool) error { return nil }
func (stubProvisioner) Start(context.Context, int64) error          { return nil }
func (stubProvisioner) Stop(context.Context, int64) error           { return nil }
func (stubProvisioner) Destroy(context.Context, int64) error        { return nil }
func (stubProvisioner) Instances(context.Context) ([]provision.Instance, error) {
	return nil, nil
}
func (stubProvisioner) TailLogs(context.Context, agents.Agent, int64) (string, error) {
	return "", nil
}
func (stubProvisioner) StreamLogs(context.Context, agents.Agent, func(string)) error { return nil }

// AttachTerminal REFUSES, as the interface requires of anything that attaches
// nothing; the terminal route's own tests use a fixture that streams.
func (stubProvisioner) AttachTerminal(context.Context, agents.Agent, bool, io.Reader, io.Writer, <-chan provision.TermSize) error {
	return provision.ErrUnsupported
}

// ⚠ THE STUB REPORTS A KICKOFF AS DELIVERABLE, so the create handler's pre-flight
// is a no-op for every fixture that does not override it. That is the right default
// HERE and only here: these fixtures exist to drive the paths PAST the pre-flight,
// and a stub that refused would silence them all with a 409. The refusal itself is
// driven by a fixture that overrides this — see
// TestADispatchIsRefusedWhileASaveAndAStartAreNot.
func (stubProvisioner) KickoffUndeliverableReason() string { return "" }

// ⚠ stubProvisioner HAS NO Chat METHODS ANY MORE, and their absence is load-bearing
// rather than tidying. While one type satisfied both halves of the old combined
// interface, any fixture that wired "the provisioner" also wired chat — so a route
// gated on the wrong one of the two fields would have been satisfied either way and
// no fixture could tell them apart. The chat half is stubGateway, in
// provisioner_seam_test.go.

// The five domain stores are stubbed by EMBEDDING their interface.
//
// 🔴 A nil EMBEDDED INTERFACE SATISFIES THE INTERFACE AND PANICS ON ANY CALL,
// AND BOTH HALVES OF THAT ARE WANTED HERE. Satisfying it is what makes the
// field non-nil, which is all a REGISTRATION fixture needs. Panicking on a call
// is the safety property: these stubs must never be used by a test that
// actually serves a request, and a panic says so immediately instead of a zero
// value flowing into an assertion. Between them the five interfaces carry about
// 150 methods; hand-writing them would be 150 places for a typo to become a
// silently wrong fixture.
type (
	stubNotesStore  struct{ notes.Store }
	stubAgentsStore struct{ agents.Store }
	stubGitHubStore struct{ github.Store }
)

// Compile-time proof each stub satisfies what Extensions asks for, and that the
// two ports are satisfiable at all.
var (
	_ notes.Store  = stubNotesStore{}
	_ agents.Store = stubAgentsStore{}
	_ github.Store = stubGitHubStore{}
	_ RouterPort   = stubRouter{}
	_ GatePort     = stubGate{}
)

// reflectValueOfExtensions reports, per field name, whether that field is at
// its zero value.
func reflectValueOfExtensions(e Extensions) map[string]bool {
	rv := reflect.ValueOf(e)
	rt := rv.Type()
	out := make(map[string]bool, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		out[rt.Field(i).Name] = rv.Field(i).IsZero()
	}
	return out
}

// TestEveryTabHasADocumentRoute closes the nav seam recorded in
// internal/ui/components.go.
//
// 🔴 IT PINS THE RELATIONSHIP, NOT THE ROUTES. It derives the tab set from
// ui.TabKeys() and the route set from the recorder, and demands every tab have
// a document route. Listing the five routes instead would pass unchanged when a
// sixth tab is added — which is precisely how the seam was created.
func TestEveryTabHasADocumentRoute(t *testing.T) {
	registered := make(map[string]bool)
	for _, p := range recordFrom(RegisterRoutes) {
		registered[p] = true
	}
	tabs := ui.TabKeys()
	if len(tabs) == 0 {
		t.Fatal("ui.TabKeys() is empty, so the loop below asserts nothing")
	}
	for _, tab := range tabs {
		want := "GET /" + tab
		if !registered[want] {
			t.Errorf("the nav links to /%s and no route serves it: %q is not registered.\n"+
				"A tab whose document route is missing renders a 404 from the navigation.", tab, want)
		}
	}
}

// stubUsers is the seeded users table without a database: user 1 is the
// admin, everyone else a member.
type stubUsers struct{}

func (stubUsers) Role(_ context.Context, id int64) (authz.Role, error) {
	if id == authz.OperatorUserID {
		return authz.RoleAdmin, nil
	}
	return authz.RoleMember, nil
}
