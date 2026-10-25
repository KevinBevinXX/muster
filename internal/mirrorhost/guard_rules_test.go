package mirrorhost

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// guardRuleValues is every hermesRule* constant by its name.
var guardRuleValues = map[string]string{
	"hermesRuleLauncher":    hermesRuleLauncher,
	"hermesRulePython":      hermesRulePython,
	"hermesRuleCheckout":    hermesRuleCheckout,
	"hermesRuleMultiplexer": hermesRuleMultiplexer,
	"hermesRuleChrome":      hermesRuleChrome,
	"hermesRuleOpenCode":    hermesRuleOpenCode,
}

// TestEveryRuleTheGuardReturnsIsOwned: every rule the paste guard can name —
// read from the SOURCE, as every hermesRule* constant a return statement of
// guardShape, hermesShape, a runtime's shape method or Tmux.PaneHermes (the
// chrome check) names — is the multiplexer rule or is OWNED by one runtime's
// pasteGuard, so guardPane refuses it in that runtime's words, never by its
// fall-back. The table above must also hold every hermesRule* constant the
// package declares, so a new rule cannot be missed by a stale list.
func TestEveryRuleTheGuardReturnsIsOwned(t *testing.T) {
	fset := token.NewFileSet()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	declared, returned := map[string]bool{}, map[string]bool{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for _, n := range vs.Names {
							if strings.HasPrefix(n.Name, "hermesRule") {
								declared[n.Name] = true
							}
						}
					}
				}
			case *ast.FuncDecl:
				name := d.Name.Name
				if name != "guardShape" && name != "hermesShape" && name != "shape" && name != "PaneHermes" {
					continue
				}
				ast.Inspect(d.Body, func(n ast.Node) bool {
					ret, ok := n.(*ast.ReturnStmt)
					if !ok {
						return true
					}
					ast.Inspect(ret, func(m ast.Node) bool {
						if id, ok := m.(*ast.Ident); ok && strings.HasPrefix(id.Name, "hermesRule") {
							returned[id.Name] = true
						}
						return true
					})
					return true
				})
			}
		}
	}
	var dk, tk []string
	for k := range declared {
		dk = append(dk, k)
	}
	for k := range guardRuleValues {
		tk = append(tk, k)
	}
	sort.Strings(dk)
	sort.Strings(tk)
	if !slices.Equal(dk, tk) {
		t.Fatalf("the package declares rules %q; this test's table holds %q", dk, tk)
	}
	if len(returned) < 5 {
		t.Fatalf("instrument check: the guard's functions return only %d rules (%v)", len(returned), returned)
	}
	for name := range returned {
		rule := guardRuleValues[name]
		if rule == hermesRuleMultiplexer {
			continue
		}
		owners := 0
		for _, r := range knownRuntimes {
			if g, ok := r.(pasteGuard); ok && g.owns(rule) {
				owners++
			}
		}
		if owners != 1 {
			t.Errorf("the guard can return %s (%q), which %d runtimes own; want exactly one", name, rule, owners)
		}
	}
}
