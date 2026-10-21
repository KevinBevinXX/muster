package agentprovision

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agentspec"
)

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

// execSites lists, per module-relative non-test file, every place that can run a
// command in an instance: a selector naming provision.Exec / provision.ExecTTY,
// and a type assertion to provision.Execer / provision.TTYExecer.
func execSites(t *testing.T, root string, src map[string][]byte) map[string]int {
	t.Helper()
	sites := map[string]int{}
	for rel, b := range src {
		f, err := parser.ParseFile(token.NewFileSet(), rel, b, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "provision" {
				switch sel.Sel.Name {
				case "Exec", "ExecTTY", "Execer", "TTYExecer":
					sites[rel]++
				}
			}
			return true
		})
	}
	return sites
}

// 🔴 TestEveryExecIntoAnAgentGoesThroughTheScrub: the ledger of the module's exec
// entry points outside the provision package itself. The ONLY place allowed to
// name provision.Exec / ExecTTY (or assert its interfaces) is agentexec.go,
// whose a.exec / a.execTTY strip the turn credentials; provisiontest's contract
// suite is the one other entry (it execs into a driver under test, never an
// agent's container). A new site anywhere else fails here, naming the file.
// The set is asserted EXACTLY, so a site that disappears from agentexec.go
// fails too.
func TestEveryExecIntoAnAgentGoesThroughTheScrub(t *testing.T) {
	root := moduleRoot(t)
	src := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "node_modules" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "internal/provision/") && !strings.HasPrefix(rel, "internal/provision/provisiontest/") {
			return nil // the definitions and the drivers themselves
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		src[rel] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(src) < 100 {
		t.Fatalf("instrument check: only %d Go files read; the walk is not seeing the module", len(src))
	}
	got := execSites(t, root, src)
	want := map[string]int{
		"internal/agentprovision/agentexec.go":         2, // a.exec, a.execTTY
		"internal/provision/provisiontest/contract.go": got["internal/provision/provisiontest/contract.go"],
	}
	if want["internal/provision/provisiontest/contract.go"] == 0 {
		t.Error("instrument check: provisiontest's contract suite names no exec entry; the scan cannot see one")
	}
	var bad []string
	for f, n := range got {
		if want[f] != n {
			bad = append(bad, f)
		}
	}
	for f, n := range want {
		if got[f] != n {
			bad = append(bad, f)
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("exec entry points outside agentexec.go's credential scrub (or missing from it): %v\n"+
			"every exec into an agent's container must go through a.exec / a.execTTY; found %v", bad, got)
	}
}

// TestTheScanSeesEveryExecSpelling is the ledger's own control: each way of
// reaching an exec entry is a site, and the word in a comment or string is not.
func TestTheScanSeesEveryExecSpelling(t *testing.T) {
	src := map[string][]byte{"x.go": []byte(`package x

import "github.com/ZacxDev/muster/internal/provision"

func a(p provision.Provisioner) { _ = provision.Exec(nil, p, provision.Ref{}, nil, nil, nil, nil) }
func b(p provision.Provisioner) { _ = provision.ExecTTY(nil, p, provision.Ref{}, nil, nil, nil, nil) }
func c(p provision.Provisioner) { e := p.(provision.Execer); _ = e }
func d(p provision.Provisioner) { f := provision.Exec; _ = f }
// provision.Exec in a comment
var s = "provision.ExecTTY"
`)}
	if n := execSites(t, "", src)["x.go"]; n != 4 {
		t.Fatalf("the scan found %d sites, want 4", n)
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
