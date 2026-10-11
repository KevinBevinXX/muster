package dbtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// The guards in this file pin RELATIONSHIPS that no single package can see on
// its own: "no two test packages share a database", and "exactly one place
// decides whether a missing database skips or fails". None of them talks to
// Postgres, so none can redden the gate on a slow node.
//
//	guard 1 (TestDBTestIsTheSoleReaderOfTheEnvVars) — each of the two
//	        environment variables has exactly ONE reader. A package that reads
//	        the DSN directly is back on the shared database; a package that
//	        reads the REQUIRE flag directly has its own opinion about whether to
//	        skip, and CI's requirement silently does not reach it.
//	guard 2 (TestEveryPGBackedPackageGetsADistinctDatabase) — the packages that
//	        DO go through dbtest map to pairwise-distinct database names.
//
// Either alone is walkable. With only guard 1, every package could route
// through dbtest and still collide on a name. With only guard 2, a package
// could bypass dbtest entirely and the name set would still be distinct.
//
// Both are LEDGERS: they fail when the set GROWS *or* SHRINKS. A guard that
// only checked "at most one reader" would pass against a tree where the walker
// was broken and found nothing, which is the failure mode that makes a green
// meaningless.
// ---------------------------------------------------------------------------

const modulePath = "github.com/ZacxDev/muster"

// pgBackedPackages is the ledger of every directory in this module whose tests
// take a database from dbtest.
//
// 🔴 IF A CHANGE MAKES THIS LIST WRONG, UPDATE IT DELIBERATELY. That is the
// point: a new Postgres-backed package should be a decision someone made, not a
// thing that appeared. Paths are slash-separated and relative to the module
// root.
//
// ⚠ internal/dbtest IS NOT IN THE LIST AND TAKES DATABASES ANYWAY. The scan
// looks for the QUALIFIED call `dbtest.DSN(...)`, which is what an external
// user writes, while this package's own tests call the unqualified `DSN(t)` and
// `ensure(...)`. A full run therefore creates one database per entry here,
// PLUS one for internal/dbtest itself (dsn_test.go) and one probe for
// degrade_test.go.
//
// 🔴 NO TOTAL IS QUOTED HERE ON PURPOSE. Nothing asserts a total (neither guard
// counts databases), so nothing would catch one going stale — and on the
// project this was ported from a quoted total was written twice and was wrong
// both times. Count the list and add the two, or ask the server.
//
// 🔴 guard 2 HAS AN EXPLICIT `sawAnyCall` BRANCH IN BOTH DIRECTIONS RATHER THAN
// AN UNCONDITIONAL FATAL, so the positive control stays meaningful while this
// list is short and arms itself as it grows. An unconditional "the scanner
// found at least one call" would have been a vacuous failure on the commit that
// created this file with no consumers at all.
var pgBackedPackages = []string{
	// Added by the CLI carve, and it is the ONE `package main` in the list, so
	// it is worth saying what it takes a database FOR. The CLI and the server
	// are each tested in isolation everywhere else, and "verified in isolation"
	// is exactly where this seam's defect lives: the server reads three
	// provenance headers, the client sends three, and nothing but a run through
	// both proves the two spellings agree. cmd/muster/seam_test.go drives the
	// real command tree at the real api.Handler over a real database and watches
	// one task's session thread move 0 -> 1. A fake on either side would only
	// restate what that fake's author believed the other side does.
	//
	// ⚠ ITS PRESENCE HERE IS ALSO WHAT MAKES cmd/muster's OWN dbtest.Identity()
	// GUARD MEANINGFUL: a `package main` test binary is the case where the
	// runtime's reported package identity is easiest to get wrong, and getting
	// it wrong makes several main packages share ONE database silently.
	"cmd/muster",
	// Added by the SERVER carve, and it is the SECOND `package main` here — see
	// the identity note above, which is the reason two main packages can share
	// this ledger without silently sharing one database.
	//
	// 🔴 IT TAKES A DATABASE FOR ONE REASON AND THE REASON IS A REFUSAL. The
	// readiness door refuses to serve a notes store that has no session-liveness
	// probe — because that combination renders "no transcript recorded" over
	// transcripts that may be alive, silently, on every surface — and a notes
	// store only EXISTS when a pool does. So the defect is unreachable without a
	// real database: a DSN-less server has a nil Notes store, no defect, and
	// reports ready. Testing the refusal against a fake would be testing that
	// the fake refuses.
	//
	// ⚠ THE REST of cmd/muster-server's suite needs no database on purpose. The
	// "it serves" guard binds a real port and drives /health, which is
	// deliberately a database-free probe — so the one test that must never skip
	// does not depend on infrastructure that can be absent.
	"cmd/muster-server",
	// Added by the kickoff deliverer. ONE test takes a database, and for the
	// reason the internal/ui entry gives: the defect it pins was in SQL — a
	// recorded send failure moved updated_at, which is the only bound on
	// agents.DecideReconcile's retry branch — so only the real store can show the
	// escalation firing at the dwell boundary.
	"internal/agentkickoff",
	// Added by the status promoter. Its guard IS the SQL: MarkRunning promotes
	// only a row still `provisioning` at the updated_at the tick read, and only a
	// real store can show a Stop or a Start landing mid-tick winning that race.
	"internal/agentstatus",
	"internal/agents",
	// Added by the connector store. Its doors (the shared-token refusal, the
	// operator's enter-credential flow) resolve agents by token and write grants
	// that FOREIGN-KEY to real agent rows, so the API tests run against the real
	// stores rather than a fake that restates what its author believed.
	"internal/api",
	"internal/ccpool",
	// Added by the connector store. "The stored row never contains the plaintext"
	// and "the wrong key fails closed" are claims about what reaches Postgres, and
	// the proxy's grant checks are SQL (the partial unique index) — only a real database can falsify either.
	"internal/connectors",
	"internal/db",
	"internal/notes",
	// Added by the UI carve, and the decision behind it is worth one line
	// because a RENDERING package taking a real database looks wrong at a
	// glance. It takes exactly one: the reaper writes a task's idle flag
	// WITHOUT moving updated_at, and the card's stale-swap guard drops an
	// incoming render whose revision is older than the one on screen. Whether
	// those two agree is a property of the SQL — a fake would only restate
	// what the fake's author believed — so that one test opens a database and
	// the rest of the package renders strings.
	"internal/ui",
}

// envVarReaders is the ledger of files that may name either environment
// variable in CODE, keyed by the variable. Comments are not counted — the
// parser looks at string literals only, so a file that merely documents a
// variable is invisible to this guard.
var envVarReaders = map[string][]string{
	EnvVar:        {"internal/dbtest/dbtest.go"},
	RequireEnvVar: {"internal/dbtest/dbtest.go"},
}

// moduleRoot locates the module this package lives in, and FAILS rather than
// returning an empty tree. A walker that silently found nothing would make
// every ledger below vacuous.
//
// 🔴 IT WALKS UP FROM THE WORKING DIRECTORY, NOT FROM runtime.Caller, AND THAT
// IS NOT A STYLE CHOICE. `go test` sets the working directory to the package's
// own source directory, which is a REAL path in every environment. The file
// path runtime.Caller reports is not: a `-trimpath` build — which is what
// `buildGoModule` does, so it is what `nix build` does — reports
// "github.com/ZacxDev/muster/internal/dbtest/dbtest.go", a MODULE-relative
// string that stats as nothing. Deriving the root from it made all four
// source-walking guards fail inside the nix sandbox with a message about a
// missing go.mod, which reads as "the tree is broken" rather than "the path is
// synthetic". Measured on this repository's own `nix build .#muster-migrate`.
//
// 🔴 IT ALSO CHECKS THE MODULE PATH. `modulePath` is a constant this file uses
// to MODEL callerIdentity; if the module is ever renamed and that constant is
// not, every identity this file computes is wrong and every guard built on them
// is reasoning about a module that does not exist — silently, because the
// models and the expectations would move together.
func moduleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := wd
	for {
		gomod := filepath.Join(dir, "go.mod")
		if b, err := os.ReadFile(gomod); err == nil {
			want := "module " + modulePath
			if !strings.HasPrefix(strings.TrimSpace(string(b)), want) {
				first, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
				t.Fatalf("%s declares %q but this file's modulePath constant is %q.\n\n"+
					"identityOf() models callerIdentity using that constant, so every database "+
					"name these guards compute would be for a module that does not exist — and "+
					"the model and the expectation would move together, so nothing else would "+
					"notice.", gomod, first, modulePath)
			}
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("walked up from %q to the filesystem root without finding a go.mod; "+
				"the guards below would be vacuous", wd)
		}
		dir = parent
	}
}

// goFiles lists every .go file in the module, excluding vendor and testdata.
func goFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "testdata", "node_modules", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %q: %v", root, err)
	}
	if len(out) == 0 {
		t.Fatalf("the walk of %q found no .go files at all; the guards below would be vacuous", root)
	}
	return out
}

func rel(t *testing.T, root, path string) string {
	t.Helper()
	r, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatalf("rel(%q, %q): %v", root, path, err)
	}
	return filepath.ToSlash(r)
}

// TestDBTestIsTheSoleReaderOfTheEnvVars — guard 1, over BOTH variables.
//
// 🔴 THE REQUIRE FLAG IS IN THE LEDGER FOR A DIFFERENT REASON FROM THE DSN, and
// the two are not interchangeable. A second reader of the DSN puts a package
// back on the shared database. A second reader of the REQUIRE flag is worse in
// a quieter way: it means some test decides for itself whether to skip, so CI
// can set MUSTER_TEST_REQUIRE_DB=1, believe the whole suite is required, and
// still have that test skip silently — the exact invisible-skip the flag exists
// to remove, reintroduced one file over.
func TestDBTestIsTheSoleReaderOfTheEnvVars(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	found := map[string][]string{}
	// Positive control: the scan must be able to SEE a literal. Without it a
	// broken parser loop reports "0 readers", which reads as a clean result and
	// is indistinguishable from a scanner wired to nothing.
	sawAnyStringLiteral := false
	for _, path := range goFiles(t, root) {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		hit := map[string]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			sawAnyStringLiteral = true
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if v == EnvVar || v == RequireEnvVar {
				hit[v] = true
			}
			return true
		})
		for v := range hit {
			found[v] = append(found[v], rel(t, root, path))
		}
	}
	if !sawAnyStringLiteral {
		t.Fatal("the AST scan saw no string literals anywhere in the module; it is not observing the code")
	}

	for _, v := range []string{EnvVar, RequireEnvVar} {
		got := append([]string(nil), found[v]...)
		sort.Strings(got)
		want := append([]string(nil), envVarReaders[v]...)
		sort.Strings(want)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("the set of files naming %s in CODE changed.\n got: %v\nwant: %v\n\n"+
				"Every Postgres-backed test must take its database from dbtest.DSN(t), and the "+
				"skip-or-fail decision must be made in exactly one place (dbtest.Base). A second "+
				"reader of either variable is how a test ends up on the shared database, or "+
				"silently skipping in a run that was told to require one.",
				v, got, want)
		}
	}
}

// TestEveryPGBackedPackageGetsADistinctDatabase — guard 2.
func TestEveryPGBackedPackageGetsADistinctDatabase(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()

	// dir -> the set of PACKAGE NAMES declared by files in it that call
	// dbtest.DSN. It is a set, not one name: a directory can hold BOTH an
	// internal (`package x`) and an external (`package x_test`) caller, the
	// runtime reports them under DIFFERENT import paths, and they therefore get
	// two databases. A model with one identity per directory is blind to that.
	users := map[string]map[string]bool{}
	sawAnyCall := false
	for _, path := range goFiles(t, root) {
		if !strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		uses := false
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "DSN" {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "dbtest" {
				uses = true
				sawAnyCall = true
			}
			return true
		})
		if !uses {
			continue
		}
		dir := rel(t, root, filepath.Dir(path))
		if users[dir] == nil {
			users[dir] = map[string]bool{}
		}
		users[dir][f.Name.Name] = true
	}

	// 🔴 THE POSITIVE CONTROL ARMS ITSELF. While the ledger is empty, finding no
	// calls is CORRECT and finding some is the bug; once the ledger has an entry,
	// finding none means the scanner is not observing the code. Both directions
	// are asserted so neither reads as a pass by default.
	if len(pgBackedPackages) == 0 && sawAnyCall {
		t.Fatal("the scan found a dbtest.DSN call, but pgBackedPackages is empty — add the " +
			"package to the ledger deliberately; each entry costs one CREATE DATABASE in the " +
			"serialised setup queue")
	}
	if len(pgBackedPackages) > 0 && !sawAnyCall {
		t.Fatal("pgBackedPackages names at least one package, but the AST scan found no " +
			"dbtest.DSN call anywhere; it is not observing the code")
	}

	dirs := make([]string, 0, len(users))
	for d := range users {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	want := append([]string(nil), pgBackedPackages...)
	sort.Strings(want)
	if strings.Join(dirs, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the set of Postgres-backed packages changed.\n got: %v\nwant: %v\n\n"+
			"Update pgBackedPackages deliberately.", dirs, want)
	}

	// The relationship: distinct packages, distinct databases.
	byName := map[string]string{}
	total := 0
	for _, dir := range dirs {
		pkgs := make([]string, 0, len(users[dir]))
		for p := range users[dir] {
			pkgs = append(pkgs, p)
		}
		sort.Strings(pkgs)
		for _, pkg := range pkgs {
			total++
			id := identityOf(pkg, dir, filepath.Join(root, dir))
			db := DatabaseName(id)
			label := dir + " (" + pkg + ")"
			if prev, clash := byName[db]; clash {
				t.Fatalf("packages %q and %q both resolve to database %q — they would SHARE a "+
					"database, which is the contamination this package exists to remove", prev, label, db)
			}
			byName[db] = label
		}
	}
	if len(byName) != total {
		t.Fatalf("got %d distinct database names for %d test packages", len(byName), total)
	}
}

// identityOf reproduces callerIdentity's key from static information.
//
// 🔴 IT TAKES THE ABSOLUTE DIRECTORY, NOT THE MODULE-RELATIVE ONE, and the
// difference is not cosmetic. callerIdentity reads runtime.Caller's FILE path,
// so for the module root it sees ".../muster" and produces ".../muster|muster";
// a model built on the relative path would produce "|." and would be describing
// a function that does not exist.
//
// ⚠ The package PATH is the module path plus the directory — including for
// `package main`. The package NAME matters only for the "_test" suffix an
// external test package carries.
func identityOf(pkgName, relDir, absDir string) string {
	pkgPath := modulePath
	if relDir != "." {
		pkgPath += "/" + relDir
	}
	if strings.HasSuffix(pkgName, "_test") {
		pkgPath += "_test"
	}
	return pkgPath + "|" + filepath.Base(absDir)
}

// TestTheMainPackagesSurviveABareMainPath pins the DEFENCE, and is honest that
// it is defence: on the toolchains measured so far the runtime reports the full
// import path for a `package main` test too, so this case does not arise today.
// It asserts what happens IF it ever did — the `package main` binaries must
// still get distinct databases from the directory half alone, because the
// alternative is several packages silently sharing one.
func TestTheMainPackagesSurviveABareMainPath(t *testing.T) {
	root := moduleRoot(t)
	dirs := []string{".", "cmd/muster-migrate"}
	seen := map[string]string{}
	for _, dir := range dirs {
		id := "main|" + filepath.Base(filepath.Join(root, dir))
		db := DatabaseName(id)
		if prev, ok := seen[db]; ok {
			t.Fatalf("%q and %q collide on database %q with a bare \"main\" package path", prev, dir, db)
		}
		seen[db] = dir
	}
	if len(seen) != len(dirs) {
		t.Fatalf("got %d distinct names, want %d: %v", len(seen), len(dirs), seen)
	}
}

func TestDatabaseNameIsAFunctionOfTheWholeIdentity(t *testing.T) {
	// Two identities that share a directory basename but differ in package path
	// must not collide — the slug is truncated to 20 characters, so a guard that
	// only looked at the readable half would miss this.
	a := DatabaseName(modulePath + "/internal/aaaaaaaaaaaaaaaaaaaaaaaaaaaa|store")
	b := DatabaseName(modulePath + "/internal/bbbbbbbbbbbbbbbbbbbbbbbbbbbb|store")
	if a == b {
		t.Fatalf("two different identities produced the same database name %q", a)
	}
	if len(a) > 63 {
		t.Fatalf("database name %q is %d bytes; Postgres identifiers truncate at 63", a, len(a))
	}
}

func TestPackageOfFunc(t *testing.T) {
	cases := []struct{ in, want string }{
		{modulePath + "/internal/notes.TestX", modulePath + "/internal/notes"},
		{modulePath + "/internal/notes.(*PGStore).List.func1", modulePath + "/internal/notes"},
		{"main.TestX", "main"},
		{"main.(*T).M", "main"},
	}
	for _, c := range cases {
		if got := packageOfFunc(c.in); got != c.want {
			t.Errorf("packageOfFunc(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestCallerIdentityNamesThisPackage exercises the runtime half — the part the
// static cases above cannot reach — and BINDS it to identityOf.
//
// 🔴 THE BINDING IS THE POINT, not the literal. Guard 2 reasons about database
// names through identityOf, which is a MODEL of callerIdentity written in the
// test file. Without an assertion that the two agree, callerIdentity could stop
// including the directory (reintroducing the `package main` collision in
// production) while every static guard stayed green against the model.
func TestCallerIdentityNamesThisPackage(t *testing.T) {
	got := callerIdentity(0)
	want := modulePath + "/internal/dbtest|dbtest"
	if got != want {
		t.Fatalf("callerIdentity = %q, want %q", got, want)
	}
	if model := identityOf("dbtest", "internal/dbtest", filepath.Join(moduleRoot(t), "internal/dbtest")); model != got {
		t.Fatalf("identityOf models callerIdentity as %q but it returns %q — guard 2 is reasoning "+
			"about a function that no longer behaves the way it assumes", model, got)
	}
}

func TestParseBaseRejectsNonURLForms(t *testing.T) {
	for _, bad := range []string{
		"host=localhost dbname=muster_test",
		"mysql://u:p@localhost/db",
		"postgres://u:p@localhost/",
		"postgres://u:p@localhost",
	} {
		if _, err := parseBase(bad); err == nil {
			t.Errorf("parseBase(%q) = nil error; a DSN this package cannot read must fail loudly, "+
				"not silently resolve to an empty template name", bad)
		}
	}
	u, err := parseBase("postgres://muster:muster@127.0.0.1:55432/muster_test?sslmode=disable")
	if err != nil {
		t.Fatalf("parseBase(valid) = %v", err)
	}
	if got := strings.TrimPrefix(u.Path, "/"); got != "muster_test" {
		t.Fatalf("template = %q, want muster_test", got)
	}
}

func TestWithDatabaseKeepsCredentialsAndOptions(t *testing.T) {
	got, err := withDatabase("postgres://muster:muster@127.0.0.1:55432/muster_test?sslmode=disable", "ms_notes_0123456789")
	if err != nil {
		t.Fatalf("withDatabase: %v", err)
	}
	want := "postgres://muster:muster@127.0.0.1:55432/ms_notes_0123456789?sslmode=disable"
	if got != want {
		t.Fatalf("withDatabase =\n %q\nwant\n %q", got, want)
	}
}

func TestBudgetIsClampedByTheTestDeadline(t *testing.T) {
	// budget() is what keeps a slow node from turning into a whole-binary `go
	// test` panic, so it is asserted directly rather than trusted.
	if got := budget(t, time.Millisecond); got != time.Millisecond {
		t.Fatalf("a budget well inside the deadline must pass through, got %v", got)
	}
	// `go test -timeout 0` removes the deadline entirely; the clamp has nothing
	// to clamp against then, and asserting it would fail for a legitimate way of
	// running the suite.
	if _, ok := t.Deadline(); !ok {
		t.Skip("no test deadline (-timeout 0); the clamp has nothing to bound against")
	}
	if got := budget(t, 100*time.Hour); got >= 100*time.Hour {
		t.Fatalf("a budget past the deadline must be clamped, got %v", got)
	}
}
