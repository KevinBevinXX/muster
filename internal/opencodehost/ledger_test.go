package opencodehost

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// sourceFiles is this package's non-test Go files, parsed.
func sourceFiles(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	for _, n := range names {
		if strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		out[n] = f
	}
	if len(out) < 4 {
		t.Fatalf("instrument check: %d source files", len(out))
	}
	return fset, out
}

// enclosing is the name of the function each call expression sits in.
func callsIn(f *ast.File, match func(*ast.CallExpr) bool) map[string][]*ast.CallExpr {
	out := map[string][]*ast.CallExpr{}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok && match(c) {
				out[fd.Name.Name] = append(out[fd.Name.Name], c)
			}
			return true
		})
	}
	return out
}

func selectorName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.SelectorExpr:
		return x.Sel.Name
	case *ast.Ident:
		return x.Name
	}
	return ""
}

func exprText(fset *token.FileSet, src []byte, e ast.Expr) string {
	return string(src[fset.Position(e.Pos()).Offset:fset.Position(e.End()).Offset])
}

// TestTheClientMakesOnlyGETsExceptTheMint: the ONE place a request is built
// is Client.request; every call of it names http.MethodGet except exactly one,
// mint's POST to /api/pair; mint is called only by Pair and Renew; and no
// file spells another way to send (http.Post, a Client.Post, a raw method
// string). So muster-mirror cannot prompt, interrupt, reply to a permission or
// change anything on the opencode server — whatever the rest of the code does.
// An INVARIANT GUARD (the code never did otherwise); mutation-checked in the PR.
func TestTheClientMakesOnlyGETsExceptTheMint(t *testing.T) {
	fset, files := sourceFiles(t)
	var builders, requests, mints []string
	for name, f := range files {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for fn, calls := range callsIn(f, func(c *ast.CallExpr) bool {
			s := selectorName(c.Fun)
			return strings.HasPrefix(s, "NewRequest") || s == "Post" || s == "PostForm" || s == "Head" || s == "Patch" || s == "Put"
		}) {
			for range calls {
				builders = append(builders, fn)
			}
		}
		for fn, calls := range callsIn(f, func(c *ast.CallExpr) bool { return selectorName(c.Fun) == "request" }) {
			for _, c := range calls {
				if len(c.Args) < 2 {
					t.Fatalf("%s: a request call with %d args", fn, len(c.Args))
				}
				requests = append(requests, fn+" "+exprText(fset, src, c.Args[1])+" "+exprText(fset, src, c.Args[2]))
			}
		}
		for fn, calls := range callsIn(f, func(c *ast.CallExpr) bool { return selectorName(c.Fun) == "mint" }) {
			for range calls {
				mints = append(mints, fn)
			}
		}
		// No method spelled as a literal anywhere.
		for _, m := range []string{`"POST"`, `"PUT"`, `"PATCH"`, `"DELETE"`, "MethodPut", "MethodPatch", "MethodDelete"} {
			if strings.Contains(string(src), m) {
				t.Errorf("%s spells %s", name, m)
			}
		}
	}
	sort.Strings(requests)
	sort.Strings(mints)
	if len(builders) != 1 || builders[0] != "request" {
		t.Errorf("requests are built in %q, want only Client.request", builders)
	}
	want := []string{
		"Events http.MethodGet \"/api/event\"",
		"get http.MethodGet path",
		"mint http.MethodGet \"/auth/connect/\"+url.PathEscape(code.Code)",
		"mint http.MethodPost \"/api/pair\"",
	}
	if strings.Join(requests, "\n") != strings.Join(want, "\n") {
		t.Errorf("the requests this package sends:\n%s\nwant:\n%s", strings.Join(requests, "\n"), strings.Join(want, "\n"))
	}
	if strings.Join(mints, ",") != "Pair,Renew" {
		t.Errorf("mint is called from %q, want Pair and Renew only", mints)
	}
}
