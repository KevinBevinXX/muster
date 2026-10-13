package agents

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// modulePath is this module's import path. The scans below use it to recognise
// an INTERNAL import; spelled once so a module rename is a one-line change
// rather than a guard that silently stops matching anything.
const modulePath = "github.com/ZacxDev/muster"

// ---------------------------------------------------------------------------
// THE SEPARATION GUARDS FOR THIS PACKAGE.
//
// 🔴 THE SISTER GUARDS IN internal/notes DO NOT COVER THIS PACKAGE, AND THAT IS
// WHY THESE EXIST RATHER THAN BEING A COPY FOR TIDINESS. Both scans there read
// `os.ReadDir(".")` — the package they live in — so every assertion they make
// is scoped to internal/notes. This package was carved from the same service,
// writes its own SQL, and was the one that reached furthest into it: an
// unguarded second store is the obvious place the coupling comes back.
//
// The half left behind still owns tables with familiar names. A SQL reference
// to one of them fails at RUNTIME with an error naming a relation, long after
// review; an import of a package that talks to it does not fail at all, it just
// couples. Neither is a compile error, so neither is caught by anything else in
// this suite.
// ---------------------------------------------------------------------------

// nonTestSources returns this package's non-test .go files, keyed by base name.
//
// 🔴 IT FAILS LOUDLY ON AN EMPTY SCAN. A file walk that matches nothing returns
// a clean zero from every check below, which is indistinguishable from a clean
// package — the exact shape that makes a scanner's verdict worthless.
func nonTestSources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		out[name] = string(raw)
	}
	if len(out) == 0 {
		cwd, _ := filepath.Abs(".")
		t.Fatalf("the file scan found NO non-test sources in %s — it is looking at the wrong "+
			"place, and every assertion below would be a confident pass over nothing", cwd)
	}
	return out
}

// TestAgentsNeverNamesAnotherServicesTable refuses a reference — in SQL or in a
// comment — to a table this module's schema does not contain.
func TestAgentsNeverNamesAnotherServicesTable(t *testing.T) {
	srcs := nonTestSources(t)

	// POSITIVE CONTROL. A scanner that reports zero is indistinguishable from a
	// scanner wired to nothing, so make it find something it MUST find first:
	// this package's own table is named throughout pgstore.go.
	control := 0
	for _, body := range srcs {
		control += strings.Count(body, "chat_messages")
	}
	if control == 0 {
		t.Fatalf("positive control failed: the scan found 0 mentions of `chat_messages` across "+
			"%d files, which cannot be true. The scan is not reading what it thinks it is, so "+
			"its verdict below means nothing", len(srcs))
	}

	// Tables the OTHER service owns. Each was genuinely reachable from this
	// package's upstream form, which is why each is named rather than guessed:
	//
	//   cc_sessions      the transcript/session record the suggestion surface
	//                    owns. It is THE table this whole extraction exists to
	//                    stop depending on.
	//   requests         the permission-request WORKING QUEUE.
	//   request_history  the permission-decision ARCHIVE.
	denied := []string{"cc_sessions", "requests", "request_history"}

	var hits []string
	for name, body := range srcs {
		for i, line := range strings.Split(body, "\n") {
			for _, tbl := range denied {
				if strings.Contains(line, tbl) {
					hits = append(hits, name+":"+strconv.Itoa(i+1)+": ["+tbl+"] "+strings.TrimSpace(line))
					break
				}
			}
		}
	}
	if len(hits) != 0 {
		t.Fatalf("internal/agents names a table owned by the other service in %d place(s):\n  %s\n\n"+
			"None of %v is in this module's schema. A SQL reference fails at RUNTIME with an "+
			"error naming a relation; a comment compiles and lies. Re-word it, or compose the "+
			"data at the API layer where the other service is reachable over HTTP.\n"+
			"(positive control: %d mentions of `chat_messages` found, so the scan works)",
			len(hits), strings.Join(hits, "\n  "), denied, control)
	}
}

// TestAgentsImportsOnlyItsLedgeredDependencies pins the package's INTERNAL import
// ledger.
//
// 🔴 THE FAILURE IT GUARDS AGAINST IS NOT A COMPILE ERROR. An import added here
// builds, vets and passes every behavioural test in the module; it becomes
// wrong only in what it COUPLES. The concrete shape to watch for: the obvious
// fix for a missing piece — a live instance list, an agent's address, a
// permission grant — is to have this package go and fetch it, and that is
// exactly what must not happen. This package holds what is true about an agent
// regardless of what RUNS it; reaching a running one is internal/provision's
// contract, and composing anything this package does not own belongs at the API
// layer.
//
// It fails when the set GROWS or SHRINKS. A shrink is not automatically wrong,
// but it means a dependency was removed and the ledger should record that it
// was.
func TestAgentsImportsOnlyItsLedgeredDependencies(t *testing.T) {
	// 🔴 THREE ENTRIES, AND WHICH THREE IS THE POINT.
	//
	// `provision` is the provisioner CONTRACT — types and sentinels, standard
	// library only. It is not a driver, and importing one (internal/provision/k8s
	// in particular) would drag a cluster client's whole dependency tree into
	// the domain layer and make this package unusable with any other backend.
	//
	// `db` is here for exactly one symbol, LikeEscape, a pure string function.
	// The pool and the migration runner are the TESTS' dependency, injected into
	// the store as a *pgxpool.Pool by whoever constructs it — a store that opened
	// its own connection would be a store the API layer could not hand a
	// transaction to.
	//
	// `cctranscript` is here for exactly one symbol, NativeToolItem: the
	// Responses output-item type ccd streams a tool call the CLI already RAN
	// under, which this package must DRAW and never dispatch. It is a
	// standard-library-only leaf, and the constant lives there so ccd and this
	// parser cannot spell it differently.
	want := map[string]bool{
		modulePath + "/internal/cctranscript": true,
		modulePath + "/internal/db":           true,
		modulePath + "/internal/provision":    true,
	}

	fset := token.NewFileSet()
	got := map[string]bool{}
	scanned := 0
	for name := range nonTestSources(t) {
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		for _, spec := range f.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: unquote import %s: %v", name, spec.Path.Value, err)
			}
			if strings.HasPrefix(path, modulePath+"/") {
				got[path] = true
			}
		}
		// Keep ast referenced so the import set of this file is honest about
		// what it parses with.
		_ = ast.Print
	}
	if scanned == 0 {
		t.Fatal("parsed 0 files — the ledger below would be vacuously satisfied")
	}

	for p := range got {
		if !want[p] {
			t.Errorf("internal/agents imports %q, which is NOT in its ledger. If the coupling "+
				"is intended, add it here WITH the reason; if it is the reflex fix for missing "+
				"composed data, it is the thing this guard exists to stop", p)
		}
	}
	for p := range want {
		if !got[p] {
			t.Errorf("the ledger names %q but no non-test source imports it. A shrink is not "+
				"automatically wrong — remove the entry deliberately, so the ledger keeps "+
				"meaning what it says", p)
		}
	}
}
