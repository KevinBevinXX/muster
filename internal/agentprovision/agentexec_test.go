package agentprovision

import (
	"bytes"
	"encoding/json"
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
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/agentspec"
)

const (
	modulePath    = "github.com/ZacxDev/muster"
	provisionPath = modulePath + "/internal/provision"
	scrubPath     = modulePath + "/internal/agentprovision/scrubexec"
	thisPath      = modulePath + "/internal/agentprovision"
)

// listedPkg is one `go list -json` record.
type listedPkg struct {
	ImportPath, Dir, Export string
	GoFiles                 []string
	Imports                 []string
}

var (
	listOnce sync.Once
	listed   map[string]listedPkg
	listErr  error
)

// moduleListing is `go list -export -deps -json ./...` from the module root:
// every package with its compiled export data, which go/types imports from.
func moduleListing(t *testing.T) map[string]listedPkg {
	t.Helper()
	listOnce.Do(func() {
		cmd := exec.Command("go", "list", "-export", "-deps", "-json", "./...")
		cmd.Dir = moduleRoot(t)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if listErr = cmd.Run(); listErr != nil {
			listErr = &listError{err: listErr, stderr: errb.String()}
			return
		}
		listed = map[string]listedPkg{}
		dec := json.NewDecoder(&out)
		for {
			var p listedPkg
			if err := dec.Decode(&p); err == io.EOF {
				break
			} else if err != nil {
				listErr = err
				return
			}
			listed[p.ImportPath] = p
		}
	})
	if listErr != nil {
		t.Fatalf("go list: %v", listErr)
	}
	return listed
}

type listError struct {
	err    error
	stderr string
}

func (e *listError) Error() string { return e.err.Error() + ": " + e.stderr }

// typeCheck parses and type-checks the NON-TEST files of path, resolving its
// imports from compiled export data. Resolution, not spelling: an import alias,
// a dot import or a local interface all resolve to the same objects.
func typeCheck(t *testing.T, path string) (*types.Package, *types.Info, []*ast.File, *token.FileSet) {
	t.Helper()
	all := moduleListing(t)
	p, ok := all[path]
	if !ok {
		t.Fatalf("instrument check: %s is not in the module listing", path)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, f := range p.GoFiles {
		af, err := parser.ParseFile(fset, filepath.Join(p.Dir, f), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, af)
	}
	imp := importer.ForCompiler(fset, "gc", func(ip string) (io.ReadCloser, error) {
		return os.Open(all[ip].Export)
	})
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{},
		Types: map[ast.Expr]types.TypeAndValue{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	pkg, err := (&types.Config{Importer: imp}).Check(path, fset, files, info)
	if err != nil {
		t.Fatalf("type-check %s: %v", path, err)
	}
	return pkg, info, files, fset
}

// isNamed reports whether typ is the named type pkgPath.name.
func isNamed(typ types.Type, pkgPath, name string) bool {
	n, ok := types.Unalias(typ).(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == pkgPath && n.Obj().Name() == name
}

// enclosingFunc is the name of the function declaration containing pos in file
// ("Recv.Name" for a method), or "".
func enclosingFunc(file *ast.File, pos token.Pos) string {
	for _, d := range file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || pos < fd.Pos() || pos > fd.End() {
			continue
		}
		if fd.Recv != nil && len(fd.Recv.List) == 1 {
			rt := fd.Recv.List[0].Type
			if st, ok := rt.(*ast.StarExpr); ok {
				rt = st.X
			}
			if id, ok := rt.(*ast.Ident); ok {
				return id.Name + "." + fd.Name.Name
			}
		}
		return fd.Name.Name
	}
	return ""
}

// provisionIn is internal/provision as pkg's own type-check imported it (pkg
// itself when it IS provision).
func provisionIn(t *testing.T, pkg *types.Package) *types.Package {
	t.Helper()
	if pkg.Path() == provisionPath {
		return pkg
	}
	for _, imp := range pkg.Imports() {
		if imp.Path() == provisionPath {
			return imp
		}
	}
	t.Fatalf("instrument check: %s does not import %s", pkg.Path(), provisionPath)
	return nil
}

// execSignatures are provision.Execer.Exec's and TTYExecer.ExecTTY's
// signatures: any method with one of them IS an exec entry, whatever it is
// called through.
func execSignatures(t *testing.T, prov *types.Package) []*types.Signature {
	t.Helper()
	var out []*types.Signature
	for _, name := range []string{"Execer", "TTYExecer"} {
		obj := prov.Scope().Lookup(name)
		if obj == nil {
			t.Fatalf("instrument check: provision.%s not found", name)
		}
		out = append(out, obj.Type().Underlying().(*types.Interface).Method(0).Type().(*types.Signature))
	}
	return out
}

// 🔴 TestNoExecInTheModuleBypassesTheScrub resolves, with go/types, every
// reference in the module's non-test code to an exec entry point:
//   - provision.Exec / provision.ExecTTY (the package functions): allowed only
//     when called with a scrubexec.Driver as the provisioner;
//   - any METHOD with Execer.Exec's or TTYExecer.ExecTTY's signature — through an
//     interface, a local interface, a concrete driver or a method value: allowed
//     only inside scrubexec.Driver's own Exec/ExecTTY (the wrap's delegation) and
//     inside provision.Exec/ExecTTY (the capability gate those call through).
//
// provisiontest is the one exempt package: its contract suite execs into a driver
// under test, never into an agent's container. Spelling plays no part: an
// alias, a local interface or a new wrapper in internal/provision all resolve to
// the objects checked here.
func TestNoExecInTheModuleBypassesTheScrub(t *testing.T) {
	all := moduleListing(t)
	var scanned []string
	var bad []string
	for path, p := range all {
		if !strings.HasPrefix(path, modulePath+"/") || strings.HasSuffix(path, "/provisiontest") {
			continue
		}
		touches := path == provisionPath
		for _, imp := range p.Imports {
			touches = touches || imp == provisionPath
		}
		if !touches || len(p.GoFiles) == 0 {
			continue
		}
		scanned = append(scanned, path)
		pkg, info, files, fset := typeCheck(t, path)
		// provision's types AS THIS CHECK SEES THEM: types.Identical compares
		// named types by object, and every typeCheck is its own universe.
		provPkg := provisionIn(t, pkg)
		sigs := execSignatures(t, provPkg)
		isExecSig := func(s *types.Signature) bool {
			for _, want := range sigs {
				if types.Identical(s.Params(), want.Params()) && types.Identical(s.Results(), want.Results()) {
					return true
				}
			}
			return false
		}
		for _, f := range files {
			ast.Inspect(f, func(n ast.Node) bool {
				var id *ast.Ident
				var call *ast.CallExpr
				switch x := n.(type) {
				case *ast.CallExpr:
					call = x
					switch fn := x.Fun.(type) {
					case *ast.Ident:
						id = fn
					case *ast.SelectorExpr:
						id = fn.Sel
					}
				case *ast.SelectorExpr:
					id = x.Sel
				}
				if id == nil {
					return true
				}
				fn, ok := info.Uses[id].(*types.Func)
				if !ok {
					return true
				}
				sig := fn.Type().(*types.Signature)
				where := fset.Position(id.Pos()).String()
				encl := enclosingFunc(f, id.Pos())
				switch {
				case sig.Recv() == nil && fn.Pkg() != nil && fn.Pkg().Path() == provisionPath &&
					(fn.Name() == "Exec" || fn.Name() == "ExecTTY"):
					if call == nil {
						if _, isCall := n.(*ast.SelectorExpr); isCall {
							return true // the CallExpr case reports it
						}
						bad = append(bad, where+": provision."+fn.Name()+" used as a value")
						return true
					}
					if len(call.Args) < 2 || !isNamed(info.TypeOf(call.Args[1]), scrubPath, "Driver") {
						bad = append(bad, where+": provision."+fn.Name()+" on a provisioner that is not a scrubexec.Driver")
					}
				case sig.Recv() != nil && isExecSig(sig):
					if _, isSel := n.(*ast.SelectorExpr); !isSel && call != nil {
						return true // the SelectorExpr inside reports it once
					}
					allowed := (path == scrubPath && (encl == "Driver.Exec" || encl == "Driver.ExecTTY")) ||
						(path == provisionPath && (encl == "Exec" || encl == "ExecTTY"))
					if !allowed {
						bad = append(bad, where+": a raw exec method ("+fn.FullName()+") in "+encl)
					}
				}
				return true
			})
		}
	}
	sort.Strings(scanned)
	if len(scanned) < 5 {
		t.Fatalf("instrument check: only %d packages scanned (%v); the listing is not seeing the module", len(scanned), scanned)
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("exec entries that bypass the turn-credential scrub:\n  %s", strings.Join(bad, "\n  "))
	}
	t.Logf("scanned %d packages: %v", len(scanned), scanned)
}

// 🔴 TestTheRawDriverIsReachableOnlyThroughTheScrub pins the STRUCTURE that makes
// every exec this package makes a scrubbed one:
//  1. Adapter.driver is a scrubexec.Driver;
//  2. Config.Driver is read exactly once in this package, as the argument of
//     scrubexec.Wrap;
//  3. no struct field or package variable here holds a provision.Provisioner,
//     Execer or TTYExecer (where a raw driver could be kept);
//  4. scrubexec.Driver has no exported field and no method returning anything
//     that is a Provisioner, Execer or TTYExecer — no way back to the raw driver.
func TestTheRawDriverIsReachableOnlyThroughTheScrub(t *testing.T) {
	pkg, info, files, fset := typeCheck(t, thisPath)
	provPkg := provisionIn(t, pkg)
	ifaces := map[string]*types.Interface{}
	for _, n := range []string{"Provisioner", "Execer", "TTYExecer"} {
		ifaces[n] = provPkg.Scope().Lookup(n).Type().Underlying().(*types.Interface)
	}

	// 1.
	adapter := pkg.Scope().Lookup("Adapter").Type().Underlying().(*types.Struct)
	var driverField *types.Var
	for i := 0; i < adapter.NumFields(); i++ {
		if adapter.Field(i).Name() == "driver" {
			driverField = adapter.Field(i)
		}
	}
	if driverField == nil || !isNamed(driverField.Type(), scrubPath, "Driver") {
		t.Fatalf("Adapter.driver is %v, want scrubexec.Driver", driverField)
	}

	// 2.
	cfgStruct := pkg.Scope().Lookup("Config").Type().Underlying().(*types.Struct)
	var cfgDriver *types.Var
	for i := 0; i < cfgStruct.NumFields(); i++ {
		if cfgStruct.Field(i).Name() == "Driver" {
			cfgDriver = cfgStruct.Field(i)
		}
	}
	if cfgDriver == nil {
		t.Fatal("instrument check: Config.Driver not found")
	}
	var uses, wrapped, nilChecks int
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, isCall := n.(*ast.CallExpr)
			if isCall {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					if fn, ok := info.Uses[sel.Sel].(*types.Func); ok && fn.Pkg() != nil &&
						fn.Pkg().Path() == scrubPath && fn.Name() == "Wrap" && len(call.Args) == 1 {
						if s, ok := call.Args[0].(*ast.SelectorExpr); ok && info.Uses[s.Sel] == cfgDriver {
							wrapped++
						}
					}
				}
			}
			if be, ok := n.(*ast.BinaryExpr); ok && (be.Op == token.EQL || be.Op == token.NEQ) {
				for _, side := range []ast.Expr{be.X, be.Y} {
					if s, ok := side.(*ast.SelectorExpr); ok && info.Uses[s.Sel] == cfgDriver {
						nilChecks++
					}
				}
			}
			if s, ok := n.(*ast.SelectorExpr); ok && info.Uses[s.Sel] == cfgDriver {
				uses++
				_ = fset
			}
			return true
		})
	}
	// The nil check in New compares it; every other use must be the wrap.
	if wrapped != 1 || uses != wrapped+nilChecks {
		t.Errorf("Config.Driver is read %d time(s): %d as scrubexec.Wrap's argument, %d in a nil comparison; "+
			"every other read is a raw driver outside the scrub", uses, wrapped, nilChecks)
	}

	// 3.
	holdsRaw := func(typ types.Type) string {
		for n, it := range ifaces {
			if _, isIface := typ.Underlying().(*types.Interface); isIface && types.Identical(typ.Underlying(), it) {
				return n
			}
			if isNamed(typ, provisionPath, n) {
				return n
			}
		}
		return ""
	}
	for _, name := range pkg.Scope().Names() {
		obj := pkg.Scope().Lookup(name)
		if v, ok := obj.(*types.Var); ok {
			if n := holdsRaw(v.Type()); n != "" {
				t.Errorf("package variable %s holds a provision.%s", name, n)
			}
		}
		tn, ok := obj.(*types.TypeName)
		if !ok {
			continue
		}
		st, ok := tn.Type().Underlying().(*types.Struct)
		if !ok {
			continue
		}
		for i := 0; i < st.NumFields(); i++ {
			if n := holdsRaw(st.Field(i).Type()); n != "" && !(name == "Config" && st.Field(i).Name() == "Driver") {
				t.Errorf("%s.%s holds a provision.%s", name, st.Field(i).Name(), n)
			}
		}
	}

	// 4.
	scrub, _, _, _ := typeCheck(t, scrubPath)
	scrubProv := provisionIn(t, scrub)
	for _, n := range []string{"Provisioner", "Execer", "TTYExecer"} {
		ifaces[n] = scrubProv.Scope().Lookup(n).Type().Underlying().(*types.Interface)
	}
	drv := scrub.Scope().Lookup("Driver").Type()
	st := drv.Underlying().(*types.Struct)
	for i := 0; i < st.NumFields(); i++ {
		if st.Field(i).Exported() {
			t.Errorf("scrubexec.Driver exports field %s", st.Field(i).Name())
		}
	}
	ms := types.NewMethodSet(drv)
	for i := 0; i < ms.Len(); i++ {
		sig := ms.At(i).Obj().Type().(*types.Signature)
		for j := 0; j < sig.Results().Len(); j++ {
			rt := sig.Results().At(j).Type()
			for n, it := range ifaces {
				if types.Implements(rt, it) || types.Identical(rt.Underlying(), it) {
					t.Errorf("scrubexec.Driver.%s returns a %s (a way back to the raw driver)", ms.At(i).Obj().Name(), n)
				}
			}
		}
	}
}

// TestAgentExecCommandStripsEveryTurnCredential pins the prefix as a literal.
func TestAgentExecCommandStripsEveryTurnCredential(t *testing.T) {
	got := strings.Join(agentExecCommand([]string{"sh", "-c", "x"}), " ")
	if want := "env -u MUSTER_GATEWAY_BEARER -u CCD_GATEWAY_UPSTREAM_BEARER -u HOOKS_TOKEN sh -c x"; got != want {
		t.Fatalf("agentExecCommand = %q, want %q", got, want)
	}
	if len(agentspec.TurnCredentialEnv) != 3 {
		t.Fatalf("agentspec.TurnCredentialEnv has %d names; this pin and ccd's list name 3", len(agentspec.TurnCredentialEnv))
	}
}

// moduleRoot is the directory holding go.mod, found upward from this package.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the package")
		}
		dir = parent
	}
}
