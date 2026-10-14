package modulegate

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// THE ENVIRONMENT THE CODE READS == THE TABLE docs/configuration.md PRINTS.
//
// A relationship with two directions, and each direction is a separate failure:
//
//	READ BUT UNDOCUMENTED — a variable some binary reads that the table does not
//	  list. An operator cannot set what nobody told them exists.
//	DOCUMENTED BUT UNREAD — a row naming a variable nothing reads. This is the
//	  worse one: the operator sets it and nothing happens, silently. It is the
//	  exact state config.go's const block was written to end (internal/api and
//	  internal/ui once documented a dozen MUSTER_* names that nothing read).
//
// HOW THE CODE SIDE IS COLLECTED — by parsing, not by grepping. Every non-test
// .go file in the module is parsed with go/parser (build tags are NOT applied, so
// an `-tags e2emirror` fixture's reads are collected too), and a READ is a call
// to one of:
//
//	os.Getenv / os.LookupEnv
//	an identifier named getenv / lookupEnv — the binaries take os.Getenv as a
//	  value (`loadConfig(getenv func(string) string)`, `run(..., os.Getenv, ...)`)
//	  so their configuration is testable without a process environment
//	a HELPER: any function or closure that forwards one of its own string
//	  parameters into a read (`envOr(name, def)`, `envBool := func(k string)`),
//	  found to a fixpoint, so helpers of helpers are reads too
//
// and its name argument is RESOLVED to strings: a literal, a constant (in the
// file, the package, or another package of this module), a range variable over
// a literal slice or over a package-level slice variable, a field of a range
// variable over a literal slice of structs, or `"PREFIX_" + <dynamic>` — which is
// recorded as a PREFIX and must be documented as `PREFIX_<SOMETHING>`.
//
// 🔴 AN ARGUMENT THE RESOLVER CANNOT RESOLVE IS A FAILURE, NOT A SKIP. A reader
// that silently drops what it does not understand reports a smaller set, and a
// smaller set is exactly what "the docs cover everything" looks like.
//
// ⚠ WHAT THIS DOES NOT SEE, STATED RATHER THAN DISCOVERED LATER:
//   - a read through a function value under any OTHER name than getenv /
//     lookupEnv (a param called `env` or `lookup`). The positive control below
//     pins one variable per read channel, so a rename of an existing channel
//     fails; a NEW channel spelled differently is invisible.
//   - _test.go files. Test-only variables (the liveenv controls, etc.) are not
//     configuration of anything that ships, and are documented where their
//     targets are (the Makefile, docs/testing.md). internal/dbtest's two
//     variables ARE in scope: they are read by non-test code.
//   - anything outside Go: the Playwright harness's MUSTER_E2E_* variables
//     (e2e/lib/env.ts), the Dockerfile, the Makefile.
//   - os.Environ(), which reads no name. Each call site is ledgered below with
//     what it does instead, and the ledger fails if a site appears or vanishes.
// ---------------------------------------------------------------------------

// configDoc is the document whose tables are the contract.
const configDoc = "docs/configuration.md"

// environCallSites is every non-test call of os.Environ(), with what it does.
// A ledger: a new site or a vanished one fails.
var environCallSites = map[string]string{
	"internal/mirrorhost/tmux.go": "copies the whole environment to the tmux child minus TMUX, TMUX_PANE " +
		"and TERM (a nested-client guard); it reads no variable by name",
}

// TestEveryEnvVarTheCodeReadsIsDocumentedAndViceVersa is the gate.
func TestEveryEnvVarTheCodeReadsIsDocumentedAndViceVersa(t *testing.T) {
	root := moduleRoot(t)
	got := collectEnvReads(t, root, modulePath)

	for _, u := range got.unresolved {
		t.Errorf("cannot resolve the variable NAME read at %s — teach collectEnvReads this shape, "+
			"or spell the name as a literal/constant", u)
	}

	// 🔴 POSITIVE CONTROL: one known variable per READ CHANNEL. A collector that
	// lost a channel would report a smaller set, which the doc comparison below
	// would then call "undocumented rows" — or, if the doc lost the same rows,
	// call clean. Each of these is read ONLY through the channel named.
	for name, channel := range map[string]string{
		"DATABASE_URL":               "os.Getenv with a literal (cmd/muster-migrate)",
		"MUSTER_PORT":                "a getenv parameter with a package constant (cmd/muster-server)",
		"MUSTER_MIRROR_PANE_MAP_DIR": "a getenv parameter (cmd/muster-mirror)",
		"MUSTER_MIRROR_INTERACTIVE":  "a closure helper (envBool, cmd/muster-mirror)",
		"CCD_LISTEN":                 "a function helper (envOr, cmd/ccd)",
		"CCD_TURN_TIMEOUT":           "a struct field of a range over a literal (cmd/ccd)",
		"CLAUDE_CODE_SESSION_ID":     "a range over a package slice variable (cmd/muster)",
		"MUSTER_E2E_TERM_WINDOW":     "a build-tagged file (cmd/muster-server, -tags e2eterm)",
	} {
		if !got.names[name] {
			t.Errorf("positive control FAILED: %s is read via %s and the collector did not find it — "+
				"the instrument is not measuring that channel", name, channel)
		}
	}
	// internal/dbtest reads its two variables through exported constants. Their
	// NAMES are deliberately not spelled here: internal/dbtest's ledger test
	// allows exactly one file to spell them in code, and this is not it. So the
	// control is that SOME read was collected from that file.
	dbtestReads := 0
	for _, sites := range got.where {
		for _, s := range sites {
			if strings.HasPrefix(s, "internal/dbtest/dbtest.go:") {
				dbtestReads++
			}
		}
	}
	if dbtestReads < 2 {
		t.Errorf("positive control FAILED: %d read(s) collected from internal/dbtest/dbtest.go, want its two "+
			"variables (read via a getenv parameter and exported constants)", dbtestReads)
	}
	if !got.prefixes["MUSTER_AGENT_CC_TOKEN_"] {
		t.Errorf("positive control FAILED: the prefix MUSTER_AGENT_CC_TOKEN_ (a constant + a dynamic suffix, " +
			"cmd/muster-server) was not collected")
	}
	if len(got.names) < 80 {
		t.Errorf("positive control FAILED: only %d variable name(s) collected; this module reads well over 80", len(got.names))
	}

	// The os.Environ ledger, both directions.
	for site := range got.environ {
		if _, ok := environCallSites[site]; !ok {
			t.Errorf("%s calls os.Environ() and is not in environCallSites — say what it does with the "+
				"environment (a read by name belongs in %s instead)", site, configDoc)
		}
	}
	for site := range environCallSites {
		if !got.environ[site] {
			t.Errorf("environCallSites lists %s, which no longer calls os.Environ() — remove the entry", site)
		}
	}

	docNames, docPrefixes := documentedEnv(t, filepath.Join(root, configDoc))

	for _, n := range sortedKeys(got.names) {
		if !docNames[n] && !coveredByPrefix(n, docPrefixes) {
			t.Errorf("%s is read by the code (%s) and is not in %s's tables", n, strings.Join(got.where[n], ", "), configDoc)
		}
	}
	for _, p := range sortedKeys(got.prefixes) {
		if !docPrefixes[p] {
			t.Errorf("the code reads every variable named %s<…> (%s); %s has no `%s<NAME>` row",
				p, strings.Join(got.where[p+"*"], ", "), configDoc, p)
		}
	}
	for _, n := range sortedKeys(docNames) {
		if !got.names[n] {
			t.Errorf("%s documents %s, and nothing in the module reads it — an operator who sets it gets nothing", configDoc, n)
		}
	}
	for _, p := range sortedKeys(docPrefixes) {
		if !got.prefixes[p] {
			t.Errorf("%s documents %s<…>, and nothing in the module reads that prefix", configDoc, p)
		}
	}
	t.Logf("code: %d variable name(s) + %d prefix(es); %s: %d row(s) + %d prefix row(s); os.Environ sites: %d",
		len(got.names), len(got.prefixes), configDoc, len(docNames), len(docPrefixes), len(got.environ))
}

// TestTheEnvCollectorSeesEveryShapeItClaims runs the collector over a synthetic
// module holding one read of every shape the header lists, plus one shape it
// must refuse — so a collector that reads nothing, or reads too little, is
// caught without depending on the real tree's contents.
func TestTheEnvCollectorSeesEveryShapeItClaims(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, src string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.test/m\n")
	write("lib/lib.go", "package lib\n\nconst EnvShared = \"S_SHARED\"\n")
	write("a/a.go", `package a

import (
	"os"
	"strings"

	"example.test/m/lib"
)

const envConst = "S_CONST"
const envPrefix = "S_PFX_"

var names = []string{"S_RANGE_1", "S_RANGE_2"}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func wrap(n string) string { return envOr(n, "") }

func run(getenv func(string) string, x string) {
	_ = os.Getenv("S_LITERAL")
	_, _ = os.LookupEnv(envConst)
	_ = getenv("S_PARAM")
	_ = getenv(lib.EnvShared)
	_ = envOr("S_HELPER", "d")
	_ = wrap("S_HELPER2")
	b := func(k string) bool { return getenv(k) == "1" }
	_ = b("S_CLOSURE")
	for _, n := range names {
		_ = getenv(n)
	}
	for _, d := range []struct{ name string }{{name: "S_FIELD"}} {
		_ = getenv(d.name)
	}
	_ = getenv(envPrefix + strings.ToUpper(x))
	_ = getenv(x) // UNRESOLVABLE: x is a parameter of a function nobody calls as a helper
	_ = os.Environ()
}
`)
	write("a/a_test.go", "package a\n\nimport \"os\"\n\nvar _ = os.Getenv(\"S_TEST_ONLY\")\n")

	got := collectEnvReads(t, dir, "example.test/m")
	want := []string{"S_CLOSURE", "S_CONST", "S_FIELD", "S_HELPER", "S_HELPER2", "S_LITERAL", "S_PARAM", "S_RANGE_1", "S_RANGE_2", "S_SHARED"}
	if g := sortedKeys(got.names); strings.Join(g, ",") != strings.Join(want, ",") {
		t.Errorf("names: got %v\n want %v", g, want)
	}
	if g := sortedKeys(got.prefixes); strings.Join(g, ",") != "S_PFX_" {
		t.Errorf("prefixes: got %v, want [S_PFX_]", g)
	}
	if len(got.unresolved) != 1 || !strings.Contains(got.unresolved[0], "a/a.go:") {
		t.Errorf("unresolved: got %v, want exactly the getenv(x) site in a/a.go", got.unresolved)
	}
	if !got.environ["a/a.go"] || len(got.environ) != 1 {
		t.Errorf("os.Environ sites: got %v, want a/a.go", got.environ)
	}
}

// TestTheConfigDocParserReadsRowsAndPrefixes pins documentedEnv on a synthetic
// table: a backticked first cell is a row, `X_<NAME>` is a prefix, and prose,
// headers and other columns are not rows.
func TestTheConfigDocParserReadsRowsAndPrefixes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.md")
	doc := "# c\n\nProse naming `NOT_A_ROW` in a sentence.\n\n" +
		"| variable | default |\n|---|---|\n" +
		"| `A_ONE` | `B_NOT_FIRST_CELL` |\n" +
		"| `A_PFX_<NAME>` | none |\n" +
		"| `a_lower` | ignored |\n"
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	names, prefixes := documentedEnv(t, p)
	if g := sortedKeys(names); strings.Join(g, ",") != "A_ONE" {
		t.Errorf("rows: got %v, want [A_ONE]", g)
	}
	if g := sortedKeys(prefixes); strings.Join(g, ",") != "A_PFX_" {
		t.Errorf("prefixes: got %v, want [A_PFX_]", g)
	}
}

// --- the collector -----------------------------------------------------------

type envReads struct {
	names      map[string]bool
	prefixes   map[string]bool
	where      map[string][]string // name (or prefix+"*") -> "file:line" sites
	unresolved []string
	environ    map[string]bool // module-relative file -> calls os.Environ()
}

type parsedFile struct {
	rel     string
	pkg     string // module-relative package directory
	f       *ast.File
	imports map[string]string // local name -> module-relative package dir (module imports only)
}

type envCollector struct {
	fset    *token.FileSet
	files   []*parsedFile
	byFile  map[*ast.File]*parsedFile
	decls   map[string]map[string]declRef // pkg dir -> package-level name -> its value
	helpers map[string]map[string]int     // pkg dir -> helper name -> parameter index forwarded
}

type declRef struct {
	expr ast.Expr
	file *parsedFile
}

// paramRef is what resolving a function PARAMETER yields: the read is the
// enclosing function's caller's to resolve, so the function is a helper.
type paramRef struct{ name string }

func collectEnvReads(t *testing.T, root, modPath string) envReads {
	t.Helper()
	c := &envCollector{
		fset:    token.NewFileSet(),
		byFile:  map[*ast.File]*parsedFile{},
		decls:   map[string]map[string]declRef{},
		helpers: map[string]map[string]int{},
	}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata", "vendor":
				return filepath.SkipDir
			}
			if strings.HasPrefix(d.Name(), ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(c.fset, path, nil, 0)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		pf := &parsedFile{rel: filepath.ToSlash(rel), pkg: filepath.ToSlash(filepath.Dir(rel)), f: f, imports: map[string]string{}}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if !strings.HasPrefix(p, modPath+"/") {
				continue
			}
			dir := strings.TrimPrefix(p, modPath+"/")
			name := dir[strings.LastIndex(dir, "/")+1:]
			if imp.Name != nil {
				name = imp.Name.Name
			}
			pf.imports[name] = dir
		}
		c.files = append(c.files, pf)
		c.byFile[f] = pf
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(c.files) == 0 {
		t.Fatalf("instrument check FAILED: no .go files under %s", root)
	}

	// Package-level constants and variables, by package.
	for _, pf := range c.files {
		if c.decls[pf.pkg] == nil {
			c.decls[pf.pkg] = map[string]declRef{}
		}
		for _, d := range pf.f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
				continue
			}
			for _, s := range gd.Specs {
				vs := s.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if i < len(vs.Values) {
						c.decls[pf.pkg][n.Name] = declRef{vs.Values[i], pf}
					}
				}
			}
		}
	}

	// Helpers to a fixpoint: a function that forwards parameter i into a read.
	for changed := true; changed; {
		changed = false
		c.eachRead(func(pf *parsedFile, call *ast.CallExpr, arg ast.Expr, encl *funcScope) {
			if encl == nil || encl.name == "" {
				return
			}
			_, refs, _ := c.resolve(pf, arg)
			for _, r := range refs {
				if i, ok := encl.params[r.name]; ok {
					if c.helpers[pf.pkg] == nil {
						c.helpers[pf.pkg] = map[string]int{}
					}
					if _, seen := c.helpers[pf.pkg][encl.name]; !seen {
						c.helpers[pf.pkg][encl.name] = i
						changed = true
					}
				}
			}
		})
	}

	out := envReads{names: map[string]bool{}, prefixes: map[string]bool{}, where: map[string][]string{}, environ: map[string]bool{}}
	// 🔴 A HELPER NOBODY CALLS (by a name this collector can see) WOULD HIDE ITS
	// READS: the forwarding site is skipped because "its callers are the reads",
	// and there are none to count. So every helper must have a call site, or its
	// forwarding site is reported as unresolved.
	helperCalls := map[string]int{}
	forwardSite := map[string]string{}
	c.eachRead(func(pf *parsedFile, call *ast.CallExpr, arg ast.Expr, encl *funcScope) {
		pos := c.fset.Position(call.Pos())
		site := pf.rel + ":" + strconv.Itoa(pos.Line)
		if id, ok := call.Fun.(*ast.Ident); ok {
			if _, isHelper := c.helpers[pf.pkg][id.Name]; isHelper {
				helperCalls[pf.pkg+"."+id.Name]++
			}
		}

		vals, refs, ok := c.resolve(pf, arg)
		if len(refs) > 0 {
			// Forwarded from a parameter: legitimate only inside a recognised
			// helper, whose CALLERS are resolved as reads in their own right.
			if encl != nil && encl.name != "" {
				if _, isHelper := c.helpers[pf.pkg][encl.name]; isHelper {
					forwardSite[pf.pkg+"."+encl.name] = site
					return
				}
			}
			out.unresolved = append(out.unresolved, site)
			return
		}
		if !ok {
			out.unresolved = append(out.unresolved, site)
			return
		}
		for _, v := range vals {
			if v.prefix {
				out.prefixes[v.s] = true
				out.where[v.s+"*"] = append(out.where[v.s+"*"], site)
			} else {
				out.names[v.s] = true
				out.where[v.s] = append(out.where[v.s], site)
			}
		}
	})
	// A helper passed as a VALUE (`loadConfig(osGetenv)`) is consumed through a
	// parameter named getenv, whose calls are reads in their own right — so a
	// non-call reference counts as a use.
	for _, pf := range c.files {
		ast.Inspect(pf.f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			for _, a := range call.Args {
				if id, ok := a.(*ast.Ident); ok {
					if _, isHelper := c.helpers[pf.pkg][id.Name]; isHelper {
						helperCalls[pf.pkg+"."+id.Name]++
					}
				}
			}
			return true
		})
	}
	for h, site := range forwardSite {
		if helperCalls[h] == 0 {
			out.unresolved = append(out.unresolved, site+" (forwards a parameter, and nothing in its package calls "+h+")")
		}
	}
	for _, pf := range c.files {
		ast.Inspect(pf.f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && isSel(call.Fun, "os", "Environ") {
				out.environ[pf.rel] = true
			}
			return true
		})
	}
	sort.Strings(out.unresolved)
	return out
}

// funcScope is the function a read sits in: its name (empty for an anonymous
// one) and its string parameters by position.
type funcScope struct {
	name   string
	params map[string]int
}

func scopeOf(name string, ft *ast.FuncType) *funcScope {
	s := &funcScope{name: name, params: map[string]int{}}
	i := 0
	for _, fld := range ft.Params.List {
		if len(fld.Names) == 0 {
			i++
			continue
		}
		for _, n := range fld.Names {
			s.params[n.Name] = i
			i++
		}
	}
	return s
}

// eachRead calls fn for every read in the module: its call, its name argument,
// and the innermost function around it.
func (c *envCollector) eachRead(fn func(*parsedFile, *ast.CallExpr, ast.Expr, *funcScope)) {
	for _, pf := range c.files {
		var stack []*funcScope
		var walk func(n ast.Node) bool
		walk = func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncDecl:
				if x.Body == nil {
					return false
				}
				name := x.Name.Name
				if x.Recv != nil {
					name = "" // methods are not treated as helpers
				}
				stack = append(stack, scopeOf(name, x.Type))
				ast.Inspect(x.Body, walk)
				stack = stack[:len(stack)-1]
				return false
			case *ast.AssignStmt:
				// `name := func(k string) …` names a closure helper.
				if len(x.Lhs) == 1 && len(x.Rhs) == 1 {
					if id, ok := x.Lhs[0].(*ast.Ident); ok {
						if fl, ok := x.Rhs[0].(*ast.FuncLit); ok {
							stack = append(stack, scopeOf(id.Name, fl.Type))
							ast.Inspect(fl.Body, walk)
							stack = stack[:len(stack)-1]
							return false
						}
					}
				}
			case *ast.FuncLit:
				stack = append(stack, scopeOf("", x.Type))
				ast.Inspect(x.Body, walk)
				stack = stack[:len(stack)-1]
				return false
			case *ast.CallExpr:
				if idx, ok := c.readArg(pf, x); ok && idx < len(x.Args) {
					var encl *funcScope
					if len(stack) > 0 {
						encl = stack[len(stack)-1]
					}
					fn(pf, x, x.Args[idx], encl)
				}
			}
			return true
		}
		ast.Inspect(pf.f, walk)
	}
}

// readArg reports whether call is a read, and which argument is the name.
func (c *envCollector) readArg(pf *parsedFile, call *ast.CallExpr) (int, bool) {
	if isSel(call.Fun, "os", "Getenv") || isSel(call.Fun, "os", "LookupEnv") {
		return 0, true
	}
	if id, ok := call.Fun.(*ast.Ident); ok {
		switch strings.ToLower(id.Name) {
		case "getenv", "lookupenv":
			return 0, true
		}
		if i, ok := c.helpers[pf.pkg][id.Name]; ok {
			return i, true
		}
	}
	return 0, false
}

func isSel(e ast.Expr, pkg, name string) bool {
	s, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := s.X.(*ast.Ident)
	return ok && x.Name == pkg && s.Sel.Name == name
}

type envVal struct {
	s      string
	prefix bool
}

// resolve turns a name expression into its possible values. refs are function
// parameters it depends on; ok is false for a shape it does not understand.
func (c *envCollector) resolve(pf *parsedFile, e ast.Expr) ([]envVal, []paramRef, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return nil, nil, false
		}
		s, err := strconv.Unquote(x.Value)
		return []envVal{{s: s}}, nil, err == nil
	case *ast.ParenExpr:
		return c.resolve(pf, x.X)
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return nil, nil, false
		}
		l, lr, lok := c.resolve(pf, x.X)
		if !lok || len(lr) > 0 {
			return nil, lr, false
		}
		r, rr, rok := c.resolve(pf, x.Y)
		if !rok || len(rr) > 0 {
			// A constant head and a dynamic tail: a PREFIX.
			var out []envVal
			for _, v := range l {
				out = append(out, envVal{s: v.s, prefix: true})
			}
			return out, nil, true
		}
		var out []envVal
		for _, a := range l {
			for _, b := range r {
				out = append(out, envVal{s: a.s + b.s, prefix: b.prefix})
			}
		}
		return out, nil, true
	case *ast.CompositeLit:
		var out []envVal
		var refs []paramRef
		for _, el := range x.Elts {
			v, r, ok := c.resolve(pf, el)
			if !ok {
				return nil, nil, false
			}
			out, refs = append(out, v...), append(refs, r...)
		}
		return out, refs, true
	case *ast.Ident:
		return c.resolveIdent(pf, x)
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			if dir, ok := pf.imports[id.Name]; ok && id.Obj == nil {
				if d, ok := c.decls[dir][x.Sel.Name]; ok {
					return c.resolve(d.file, d.expr)
				}
				return nil, nil, false
			}
			// A field of a range variable over a literal slice of structs.
			if elems, efile, ok := c.rangeElems(pf, id); ok {
				pos := c.fieldIndex(pf, id, x.Sel.Name)
				var out []envVal
				for _, el := range elems {
					cl, ok := el.(*ast.CompositeLit)
					if !ok {
						return nil, nil, false
					}
					found := false
					for fi, f := range cl.Elts {
						kv, ok := f.(*ast.KeyValueExpr)
						if !ok {
							// Positional: the field's index in the struct type.
							if fi == pos {
								v, _, ok := c.resolve(efile, f)
								if !ok {
									return nil, nil, false
								}
								out = append(out, v...)
								found = true
							}
							continue
						}
						if k, ok := kv.Key.(*ast.Ident); ok && k.Name == x.Sel.Name {
							v, _, ok := c.resolve(efile, kv.Value)
							if !ok {
								return nil, nil, false
							}
							out = append(out, v...)
							found = true
						}
					}
					if !found {
						return nil, nil, false
					}
				}
				return out, nil, true
			}
		}
		return nil, nil, false
	}
	return nil, nil, false
}

func (c *envCollector) resolveIdent(pf *parsedFile, id *ast.Ident) ([]envVal, []paramRef, bool) {
	if id.Obj != nil {
		switch d := id.Obj.Decl.(type) {
		case *ast.Field:
			return nil, []paramRef{{name: id.Name}}, true
		case *ast.ValueSpec:
			for i, n := range d.Names {
				if n.Name == id.Name && i < len(d.Values) {
					return c.resolve(pf, d.Values[i])
				}
			}
			return nil, nil, false
		case *ast.AssignStmt:
			if elems, efile, ok := c.rangeElems(pf, id); ok {
				var out []envVal
				for _, el := range elems {
					v, _, ok := c.resolve(efile, el)
					if !ok {
						return nil, nil, false
					}
					out = append(out, v...)
				}
				return out, nil, true
			}
			for i, l := range d.Lhs {
				if li, ok := l.(*ast.Ident); ok && li.Name == id.Name && i < len(d.Rhs) && len(d.Lhs) == len(d.Rhs) {
					return c.resolve(pf, d.Rhs[i])
				}
			}
			return nil, nil, false
		}
		return nil, nil, false
	}
	// Package scope, declared in another file of the same package.
	if d, ok := c.decls[pf.pkg][id.Name]; ok {
		return c.resolve(d.file, d.expr)
	}
	return nil, nil, false
}

// fieldIndex is the position of field name in the element struct type of the
// literal slice id ranges over (`[]struct{ name string; def T }{…}`), or -1.
func (c *envCollector) fieldIndex(pf *parsedFile, id *ast.Ident, name string) int {
	as, ok := id.Obj.Decl.(*ast.AssignStmt)
	if !ok {
		return -1
	}
	u, ok := as.Rhs[0].(*ast.UnaryExpr)
	if !ok {
		return -1
	}
	cl, ok := u.X.(*ast.CompositeLit)
	if !ok {
		return -1
	}
	at, ok := cl.Type.(*ast.ArrayType)
	if !ok {
		return -1
	}
	st, ok := at.Elt.(*ast.StructType)
	if !ok {
		return -1
	}
	i := 0
	for _, f := range st.Fields.List {
		for _, n := range f.Names {
			if n.Name == name {
				return i
			}
			i++
		}
		if len(f.Names) == 0 {
			i++
		}
	}
	return -1
}

// rangeElems: id is the VALUE variable of `for _, id := range X`, where X is a
// literal slice or a package-level variable holding one. It returns X's elements
// and the file they are written in.
func (c *envCollector) rangeElems(pf *parsedFile, id *ast.Ident) ([]ast.Expr, *parsedFile, bool) {
	if id.Obj == nil {
		return nil, nil, false
	}
	as, ok := id.Obj.Decl.(*ast.AssignStmt)
	if !ok || len(as.Rhs) != 1 || len(as.Lhs) != 2 {
		return nil, nil, false
	}
	u, ok := as.Rhs[0].(*ast.UnaryExpr)
	if !ok || u.Op != token.RANGE {
		return nil, nil, false
	}
	if v, ok := as.Lhs[1].(*ast.Ident); !ok || v.Name != id.Name {
		return nil, nil, false
	}
	src, file := u.X, pf
	if xi, ok := src.(*ast.Ident); ok {
		var d declRef
		var found bool
		if xi.Obj != nil {
			if vs, ok := xi.Obj.Decl.(*ast.ValueSpec); ok {
				for i, n := range vs.Names {
					if n.Name == xi.Name && i < len(vs.Values) {
						d, found = declRef{vs.Values[i], pf}, true
					}
				}
			}
		} else {
			d, found = c.decls[pf.pkg][xi.Name]
		}
		if !found {
			return nil, nil, false
		}
		src, file = d.expr, d.file
	}
	cl, ok := src.(*ast.CompositeLit)
	if !ok {
		return nil, nil, false
	}
	return cl.Elts, file, true
}

// --- the document side --------------------------------------------------------

// docRow is a table row whose FIRST cell is a backticked variable name, or a
// `PREFIX_<NAME>` family.
var docRow = regexp.MustCompile("^\\|\\s*`([A-Z][A-Z0-9_]*)(<[A-Z_]+>)?`\\s*\\|")

func documentedEnv(t *testing.T, path string) (names, prefixes map[string]bool) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	names, prefixes = map[string]bool{}, map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		m := docRow.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		if m[2] != "" {
			prefixes[m[1]] = true
		} else {
			names[m[1]] = true
		}
	}
	if len(names)+len(prefixes) == 0 {
		t.Fatalf("positive control FAILED: no variable rows parsed from %s — the table shape changed "+
			"and every comparison would run against nothing", path)
	}
	return names, prefixes
}

func coveredByPrefix(name string, prefixes map[string]bool) bool {
	for p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
