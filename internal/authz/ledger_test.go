package authz

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// There are exactly four ways to put a Principal on a context: authz.System,
// authz.WithPrincipal, authz.Operator, and internal/api's (*Server).admit, which
// wraps the second. (The key is unexported — see the package doc — so there is
// no fifth.) Which code may reach each is pinned here as a ledger over the
// module's non-test source, and authz's own exported surface is pinned too,
// so a new alias for one of them cannot appear unlisted.
//
// 🔴 REFERENCES ARE RESOLVED BY THE TYPE CHECKER, NOT BY SPELLING. An earlier
// version matched `<import name>.System` in a syntax tree, and three shapes
// walked straight past it with every guard green: a dot-import calling bare
// `System(ctx)`; `var admitFn = (*Server).admit` at PACKAGE level, called from
// a handler; and `var Everyone = System` inside authz itself, which was never
// scanned. Here every package in the module (authz included) is type-checked
// from source and every identifier whose types.Info.Uses object IS one of the
// three functions is attributed to its enclosing top-level declaration — a
// FuncDecl, or the var/const a package-level initializer belongs to — so all
// three shapes land as an unlisted site. TestTheLedgerSeesEveryBypassShape
// type-checks each shape and demands it be seen.
//
// 🔴 EACH LEDGER IS SET EQUALITY. A new site fails (a handler minting its own
// principal, a request path reading every user's rows through System), and so
// does a vanished one (a loop that stopped calling System reads an empty board
// under row-level security; a wrapper that stopped calling admit hands its handler nothing).
//
// ⚠ SCOPE: the default build plus `-tags e2eterm` (the only build tag on any
// non-test file in this module, measured when this was written). A file behind
// some OTHER tag is not compiled by either pass and therefore not scanned.

const (
	modulePath = "github.com/ZacxDev/muster"
	authzPath  = modulePath + "/internal/authz"
	apiPath    = modulePath + "/internal/api"
)

// Each ledger entry is "<dir relative to the module>.<decl>".
var (
	// systemLedger: the background loops, which act for every user.
	//
	// ⚠ NOT LISTED, BECAUSE THERE IS NO LOOP TO LIST: ccpool (selection runs
	// inside a dispatch request), a GitHub mirror (none exists;
	// cmd/muster-server's connectStaticGitHub is a one-shot boot write of the
	// OPERATOR'S connection, which runs under authz.Operator (operatorLedger), not
	// System), and a reconciler (agents/reconcile.go is a pure decision table
	// that agentstatus drives).
	//
	// Accounts PR 3a added the two non-loop sites, each reading ONE row with
	// no principal to take its scope from, and handing nothing read on to a
	// caller:
	//   - agentByToken: the per-agent doors' token lookup. It is how the
	//     request's owner is LEARNED, so it cannot run as that owner; under
	//     row-level security with no principal it would see no rows and every
	//     agent door would 401.
	//   - notifyAgentRunning: BroadcastAgentChanged's status hook, which is
	//     handed only an agent name (by request handlers and loops alike) and
	//     re-reads the row to decide whether to push.
	systemLedger = []string{
		"internal/agentkickoff.(*Deliverer).Run",
		"internal/agentstatus.(*Promoter).Run",
		"internal/api.(*Server).RunTaskReap",
		"internal/api.(*Server).RunTaskRetention",
		"internal/api.(*Server).agentByToken",
		"internal/api.(*Server).notifyAgentRunning",
	}
	// operatorLedger: work done as the OPERATOR (user 1) with no request —
	// the boot-time GitHub connect of the operator's own static token, and the
	// e2e fixture seed. Not System: each is one user's rows, not everyone's.
	//
	// internal/dbtest.AsOperator is the Postgres-backed TESTS' default
	// principal. It is listed because dbtest is ordinary (non-_test) source;
	// it is safe only because no non-test file imports dbtest
	// (TestOnlyTestFilesImportDBTest).
	operatorLedger = []string{
		"cmd/muster-server.connectStaticGitHub",
		"e2e/seed.run",
		"internal/dbtest.AsOperator",
	}
	// withPrincipalLedger: admit only.
	withPrincipalLedger = []string{
		"internal/api.(*Server).admit",
	}
	// admitLedger: every authentication wrapper.
	admitLedger = []string{
		"internal/api.(*Server).admitAttributedService", // requireServiceToken + the service branch of requireHookOrServiceToken
		"internal/api.(*Server).requireAgentToken",
		"internal/api.(*Server).requireArmedHookToken",
		"internal/api.(*Server).requireConnectorAgent",
		"internal/api.(*Server).requireHookOrServiceToken",
		"internal/api.(*Server).requireHookToken",
		"internal/api.(*Server).requireRelayAgent",
		"internal/api.(*Server).requireSession",
	}
	// authzExports is authz's whole exported package scope. A new exported
	// name — `var Everyone = System`, a helper wrapping WithPrincipal — is a
	// new door until someone has decided it is not.
	authzExports = []string{
		"From", "MustFrom", "Operator", "OperatorUserID", "Principal", "Role", "RoleAdmin", "RoleMember",
		"System", "Via", "ViaAgent", "ViaConnect", "ViaHook", "ViaOperator", "ViaRelay", "ViaService",
		"ViaSession", "ViaSystem", "WithPrincipal",
	}
)

func TestSystemIsCalledOnlyByTheListedBackgroundLoops(t *testing.T) {
	assertLedger(t, "authz.System", scanModule(t).sites[targetSystem], systemLedger)
}

func TestOperatorIsCalledOnlyByTheListedOutOfRequestWrites(t *testing.T) {
	assertLedger(t, "authz.Operator", scanModule(t).sites[targetOperator], operatorLedger)
}

// TestOnlyTestFilesImportDBTest pins the premise of dbtest.AsOperator's place
// in the Operator ledger: a helper that acts as the operator is harmless in a
// test and a door anywhere else. It reads the import lists of every module
// package's NON-test files (the same `go list` the scan uses) and refuses any
// that names internal/dbtest, other than dbtest itself.
func TestOnlyTestFilesImportDBTest(t *testing.T) {
	const dbtestPath = modulePath + "/internal/dbtest"
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, tags := range []string{"", "e2eterm"} {
		cmd := exec.Command("go", "list", "-tags="+tags, "-f", "{{.ImportPath}}|{{join .Imports \",\"}}", "./...")
		cmd.Dir = root
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list: %v", err)
		}
		seen := 0
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			pkg, imps, _ := strings.Cut(line, "|")
			seen++
			if pkg == dbtestPath {
				continue
			}
			for _, imp := range strings.Split(imps, ",") {
				if imp == dbtestPath {
					t.Errorf("%s (tags %q) imports internal/dbtest from a NON-test file. dbtest.AsOperator "+
						"acts as the operator with no request; outside a test that is an unledgered door.", pkg, tags)
				}
			}
		}
		// Positive control: the listing saw the module, not nothing.
		if seen < 20 {
			t.Fatalf("go list (tags %q) returned %d packages; the walk is not seeing the module", tags, seen)
		}
	}
}

func TestWithPrincipalIsCalledOnlyByAdmit(t *testing.T) {
	assertLedger(t, "authz.WithPrincipal", scanModule(t).sites[targetWithPrincipal], withPrincipalLedger)
}

func TestOnlyTheAuthWrappersCallAdmit(t *testing.T) {
	assertLedger(t, "api.(*Server).admit", scanModule(t).sites[targetAdmit], admitLedger)
}

func TestAuthzExportsExactlyTheListedNames(t *testing.T) {
	assertLedger(t, "authz's exported scope", setOf(scanModule(t).authzExports), authzExports)
}

// TestTheLedgerSeesEveryBypassShape is the scanner's positive control, run
// through the SAME type-check as the real scan: each shape below is a way to
// reach a door that a syntax match missed, and each must be attributed.
func TestTheLedgerSeesEveryBypassShape(t *testing.T) {
	m := scanModule(t)
	cases := []struct {
		name, pkgPath, pkgName, src string
		target                      target
		wantSite                    string
		wantExport                  string
	}{{
		name: "B1 dot-import, bare System(ctx)", pkgPath: modulePath + "/internal/bypassone", pkgName: "bypassone",
		src: `package bypassone
import (
	"context"
	. "` + authzPath + `"
)
func Handler(ctx context.Context) context.Context { return System(ctx) }
`,
		target: targetSystem, wantSite: "internal/bypassone.Handler",
	}, {
		name: "B2 package-level method expression of admit", pkgPath: apiPath, pkgName: "api",
		src: `package api
import "net/http"
var admitFn = (*Server).admit
func (s *Server) handleBypass(w http.ResponseWriter, r *http.Request) { _ = admitFn }
`,
		target: targetAdmit, wantSite: "internal/api.admitFn",
	}, {
		name: "B3 alias inside authz", pkgPath: authzPath, pkgName: "authz",
		src:    "package authz\nvar Everyone = System\n",
		target: targetSystem, wantSite: "internal/authz.Everyone", wantExport: "Everyone",
	}, {
		name: "B4 aliased import, function value in a closure", pkgPath: modulePath + "/internal/bypassfour", pkgName: "bypassfour",
		src: `package bypassfour
import az "` + authzPath + `"
func F() { g := func() { h := az.WithPrincipal; _ = h }; g() }
`,
		target: targetWithPrincipal, wantSite: "internal/bypassfour.F",
	}, {
		name: "B5 Operator as a package-level function value", pkgPath: modulePath + "/internal/bypassfive", pkgName: "bypassfive",
		src: `package bypassfive
import "` + authzPath + `"
var asOperator = authz.Operator
`,
		target: targetOperator, wantSite: "internal/bypassfive.asOperator",
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sites, exports := m.checkWithExtra(t, c.pkgPath, c.pkgName, c.src)
			if !sites[c.target][c.wantSite] {
				t.Fatalf("the scan did not attribute %s to %s; it saw %v", c.target, c.wantSite, keys(sites[c.target]))
			}
			if c.wantExport != "" && !setOf(exports)[c.wantExport] {
				t.Fatalf("the export scan did not see %s; it saw %v", c.wantExport, exports)
			}
		})
	}
}

// --- the scanner ---------------------------------------------------------------

type target string

const (
	targetSystem        target = "authz.System"
	targetOperator      target = "authz.Operator"
	targetWithPrincipal target = "authz.WithPrincipal"
	targetAdmit         target = "api.(*Server).admit"
)

// targetOf reports which door obj is, if any. Objects are matched by package
// path and name rather than pointer identity, because a package type-checked
// from source and the same package read from export data are distinct objects.
func targetOf(obj types.Object) (target, bool) {
	fn, ok := obj.(*types.Func)
	if !ok || fn.Pkg() == nil {
		return "", false
	}
	sig := fn.Type().(*types.Signature)
	switch {
	case fn.Pkg().Path() == authzPath && sig.Recv() == nil && fn.Name() == "System":
		return targetSystem, true
	case fn.Pkg().Path() == authzPath && sig.Recv() == nil && fn.Name() == "WithPrincipal":
		return targetWithPrincipal, true
	case fn.Pkg().Path() == authzPath && sig.Recv() == nil && fn.Name() == "Operator":
		return targetOperator, true
	case fn.Pkg().Path() == apiPath && sig.Recv() != nil && fn.Name() == "admit":
		return targetAdmit, true
	}
	return "", false
}

type listedPkg struct {
	ImportPath string
	Name       string
	Dir        string
	GoFiles    []string
	Export     string
	Module     *struct{ Path string }
}

type pass struct {
	fset    *token.FileSet
	imp     types.Importer
	ownPkgs []listedPkg
}

type module struct {
	root         string
	passes       []pass
	sites        map[target]map[string]bool
	authzExports []string
}

var (
	scanOnce sync.Once
	scanned  *module
	scanErr  error
)

func scanModule(t *testing.T) *module {
	t.Helper()
	scanOnce.Do(func() { scanned, scanErr = loadAndScan() })
	if scanErr != nil {
		t.Fatal(scanErr)
	}
	return scanned
}

func loadAndScan() (*module, error) {
	root, err := filepath.Abs("../..")
	if err != nil {
		return nil, err
	}
	if b, err := os.ReadFile(filepath.Join(root, "go.mod")); err != nil || !bytes.Contains(b, []byte("module "+modulePath+"\n")) {
		return nil, errors.New("module root " + root + " is not " + modulePath)
	}
	m := &module{root: root, sites: map[target]map[string]bool{}}
	files := 0
	for _, tags := range []string{"", "e2eterm"} {
		p, err := loadPass(root, tags)
		if err != nil {
			return nil, err
		}
		m.passes = append(m.passes, p)
		for _, lp := range p.ownPkgs {
			sites, exports, n, err := p.check(root, lp, "")
			if err != nil {
				return nil, err
			}
			files += n
			mergeSites(m.sites, sites)
			if lp.ImportPath == authzPath && tags == "" {
				m.authzExports = exports
			}
		}
	}
	// Positive control on the walk: a scan that type-checked a handful of
	// packages would report a short ledger exactly like a tidy one.
	if files < 200 {
		return nil, errors.New("type-checked only " + strconv.Itoa(files) + " source files across both passes; the load is broken")
	}
	return m, nil
}

func loadPass(root, tags string) (pass, error) {
	args := []string{"list", "-export", "-deps", "-json=ImportPath,Name,Dir,GoFiles,Export,Module"}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, "./...")
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return pass{}, errors.New("go list: " + err.Error() + ": " + stderr.String())
	}
	exports := map[string]string{}
	var own []listedPkg
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var lp listedPkg
		if err := dec.Decode(&lp); err == io.EOF {
			break
		} else if err != nil {
			return pass{}, err
		}
		exports[lp.ImportPath] = lp.Export
		if lp.Module != nil && lp.Module.Path == modulePath {
			own = append(own, lp)
		}
	}
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		f, ok := exports[path]
		if !ok || f == "" {
			return nil, errors.New("no export data for " + path)
		}
		return os.Open(f)
	})
	return pass{fset: fset, imp: imp, ownPkgs: own}, nil
}

// check type-checks one package from source (plus extra, an additional file,
// when non-empty) and returns the door references it contains and, for
// authz, its exported names.
func (p pass) check(root string, lp listedPkg, extra string) (map[target]map[string]bool, []string, int, error) {
	var files []*ast.File
	for _, name := range lp.GoFiles {
		f, err := parser.ParseFile(p.fset, filepath.Join(lp.Dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, nil, 0, err
		}
		files = append(files, f)
	}
	if extra != "" {
		f, err := parser.ParseFile(p.fset, filepath.Join(lp.Dir, "zz_extra.go"), extra, parser.SkipObjectResolution)
		if err != nil {
			return nil, nil, 0, err
		}
		files = append(files, f)
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
	pkg, err := (&types.Config{Importer: p.imp}).Check(lp.ImportPath, p.fset, files, info)
	if err != nil {
		return nil, nil, 0, errors.New("type-check " + lp.ImportPath + ": " + err.Error())
	}
	rel, _ := filepath.Rel(root, lp.Dir)
	dir := filepath.ToSlash(rel)
	sites := map[target]map[string]bool{}
	for _, f := range files {
		for _, decl := range f.Decls {
			site := dir + "." + declName(decl)
			ast.Inspect(decl, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				if tg, ok := targetOf(info.Uses[id]); ok {
					if sites[tg] == nil {
						sites[tg] = map[string]bool{}
					}
					sites[tg][site] = true
				}
				return true
			})
		}
	}
	var exports []string
	for _, n := range pkg.Scope().Names() {
		if token.IsExported(n) {
			exports = append(exports, n)
		}
	}
	return sites, exports, len(lp.GoFiles), nil
}

// checkWithExtra type-checks pkgPath (an existing module package, or a new one
// when it is not in the module) with one extra source file, using the default
// pass's importer.
func (m *module) checkWithExtra(t *testing.T, pkgPath, pkgName, src string) (map[target]map[string]bool, []string) {
	t.Helper()
	p := m.passes[0]
	lp := listedPkg{ImportPath: pkgPath, Name: pkgName, Dir: filepath.Join(m.root, strings.TrimPrefix(pkgPath, modulePath+"/"))}
	for _, own := range p.ownPkgs {
		if own.ImportPath == pkgPath {
			lp = own
		}
	}
	sites, exports, _, err := p.check(m.root, lp, src)
	if err != nil {
		t.Fatal(err)
	}
	return sites, exports
}

func declName(decl ast.Decl) string {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Recv != nil && len(d.Recv.List) == 1 {
			typ := d.Recv.List[0].Type
			star := ""
			if s, ok := typ.(*ast.StarExpr); ok {
				typ, star = s.X, "*"
			}
			if ix, ok := typ.(*ast.IndexExpr); ok { // generic receiver
				typ = ix.X
			}
			if id, ok := typ.(*ast.Ident); ok {
				if star != "" {
					return "(*" + id.Name + ")." + d.Name.Name
				}
				return id.Name + "." + d.Name.Name
			}
		}
		return d.Name.Name
	case *ast.GenDecl:
		var names []string
		for _, s := range d.Specs {
			switch sp := s.(type) {
			case *ast.ValueSpec:
				for _, n := range sp.Names {
					names = append(names, n.Name)
				}
			case *ast.TypeSpec:
				names = append(names, sp.Name.Name)
			}
		}
		if len(names) == 0 {
			return d.Tok.String()
		}
		return strings.Join(names, ",")
	}
	return "?"
}

func mergeSites(into, from map[target]map[string]bool) {
	for tg, s := range from {
		if into[tg] == nil {
			into[tg] = map[string]bool{}
		}
		for k := range s {
			into[tg][k] = true
		}
	}
}

func assertLedger(t *testing.T, what string, got map[string]bool, want []string) {
	t.Helper()
	g := keys(got)
	w := append([]string(nil), want...)
	sort.Strings(w)
	if strings.Join(g, "\n") != strings.Join(w, "\n") {
		t.Fatalf("%s is reached from:\n  %s\nthe ledger lists:\n  %s\n"+
			"A new site is a new way to put a principal on a context — justify it and add it here; "+
			"a missing one is a loop or wrapper that stopped attaching one.",
			what, strings.Join(g, "\n  "), strings.Join(w, "\n  "))
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func setOf(s []string) map[string]bool {
	out := map[string]bool{}
	for _, v := range s {
		out[v] = true
	}
	return out
}
