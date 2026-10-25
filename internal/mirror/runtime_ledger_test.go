package mirror_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// runtimeConst reports whether an identifier is one of internal/mirror's
// names for a runtime, or for a value that is one runtime's own: the runtime
// constants, Hermes's key prefix, its kind, and its paste-refusal kinds
// (PasteRefusalHermes*).
func runtimeConst(name string) bool {
	switch name {
	case "RuntimeClaude", "RuntimeHermes", "RuntimeOpenCode", "HermesKeyPrefix", "KindHermes":
		return true
	}
	return strings.HasPrefix(name, "PasteRefusalHermes")
}

// runtimeField reports whether a field is a Hermes-only one of the wire, the
// host view or the UI (Hermes, HermesSend, HermesAnswer).
func runtimeField(name string) bool {
	return name == "Hermes" || name == "HermesSend" || name == "HermesAnswer"
}

// ledgerScope is the mirror surface the ledger reads: the server and host
// halves, the API and UI that act on mirrored windows, focus keys, and the
// host binary. The agent KIND "hermes" (agentspec, agentprovision, the
// server's config) is another track, and not read.
var ledgerScope = []string{"internal/mirror/", "internal/mirrorhost/", "internal/api/", "internal/ui/", "internal/focus/", "cmd/muster-mirror/"}

// runtimeSites is every occurrence, in the non-test Go source of
// ledgerScope, of a runtime constant (runtimeConst: mirror.X, or bare inside
// internal/mirror), of a Hermes-only field (runtimeField: a selector or a
// literal's key), or of a comparison with the literal "hermes" (==, !=, a
// case), counted per function: "<file> <func> ×<n>". It also returns how
// many files it parsed.
func runtimeSites(t *testing.T, root string) ([]string, int) {
	t.Helper()
	counts := map[string]int{}
	files := 0
	mirrorDir := filepath.Join("internal", "mirror") + string(filepath.Separator)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", "testdata", "e2e", "web":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		relPath, _ := filepath.Rel(root, path)
		if !slices.ContainsFunc(ledgerScope, func(d string) bool { return strings.HasPrefix(filepath.ToSlash(relPath), d) }) {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		files++
		rel, _ := filepath.Rel(root, path)
		inMirror := strings.HasPrefix(rel, mirrorDir)
		isHermesLit := func(e ast.Expr) bool {
			b, ok := e.(*ast.BasicLit)
			if !ok || b.Kind != token.STRING {
				return false
			}
			s, err := strconv.Unquote(b.Value)
			return err == nil && s == "hermes"
		}
		for _, decl := range f.Decls {
			where := "(declarations)"
			if fd, ok := decl.(*ast.FuncDecl); ok {
				where = fd.Name.Name
				if fd.Recv != nil && len(fd.Recv.List) == 1 {
					switch r := fd.Recv.List[0].Type.(type) {
					case *ast.Ident:
						where = r.Name + "." + where
					case *ast.StarExpr:
						if id, ok := r.X.(*ast.Ident); ok {
							where = id.Name + "." + where
						}
					}
				}
			}
			key := rel + " " + where
			// A selector's Sel and a key's Ident are counted once, there, not
			// again as bare identifiers.
			done := map[*ast.Ident]bool{}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr:
					done[x.Sel] = true
					pkg, _ := x.X.(*ast.Ident)
					if runtimeField(x.Sel.Name) || (pkg != nil && pkg.Name == "mirror" && runtimeConst(x.Sel.Name)) {
						counts[key]++
					}
				case *ast.KeyValueExpr:
					if id, ok := x.Key.(*ast.Ident); ok {
						done[id] = true
						if runtimeField(id.Name) {
							counts[key]++
						}
					}
				case *ast.Ident:
					if inMirror && !done[x] && runtimeConst(x.Name) {
						counts[key]++
					}
				case *ast.BinaryExpr:
					if (x.Op == token.EQL || x.Op == token.NEQ) && (isHermesLit(x.X) || isHermesLit(x.Y)) {
						counts[key]++
					}
				case *ast.CaseClause:
					for _, e := range x.List {
						if isHermesLit(e) {
							counts[key]++
						}
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var sites []string
	for k, n := range counts {
		sites = append(sites, fmt.Sprintf("%s ×%d", k, n))
	}
	sort.Strings(sites)
	return sites, files
}

// TestEveryPlaceThatNamesARuntimeIsListed is a SEAM LEDGER over the whole
// module's non-test source: every OCCURRENCE of a runtime's name or of a
// value that is one runtime's own (runtimeNamed: the runtime constants,
// HermesKeyPrefix, KindHermes, PasteRefusalHermes*, the Hermes-only Hermes,
// HermesSend and HermesAnswer fields), and every comparison with the literal
// "hermes", is counted per function below. The ledger fails when a
// function's count GROWS or SHRINKS, or a function appears or goes. Per-
// runtime decisions belong to the runtime interface (mirror.RuntimeSpec, the
// mirrorhost Runtime); what names a runtime outside it is one of:
//   - the interface's own tables (runtime.go, both halves);
//   - a runtime's implementation checking that a window is its own, or
//     speaking its own wire (Hermes's send, asks, stream, answer, listing,
//     its hello flags and refusal kinds; its e2e fake);
//   - Claude Code's wire, which is Window.Claude and per-session hosts, never
//     Window.Runtime;
//   - per-runtime WORDS (the queue's, Send now's and the composer's copy, the
//     Hermes composer of the rendered view), keyed by name.
//
// A new site that branches on a runtime's name fails here: route the
// decision through the interface, or add it on purpose.
func TestEveryPlaceThatNamesARuntimeIsListed(t *testing.T) {
	sites, files := runtimeSites(t, filepath.Join("..", ".."))
	if files < 60 {
		t.Fatalf("instrument check: parsed %d source files; is the test running in internal/mirror?", files)
	}
	want := []string{
		"cmd/muster-mirror/e2ehermes.go hermesWindowsE2E ×2",
		"cmd/muster-mirror/hermes_list.go printHermes ×2",
		"cmd/muster-mirror/main.go run ×2",
		"cmd/muster-mirror/main.go tmuxBackend ×1",
		"internal/api/attention_hermes.go attentionNotice ×1",
		"internal/api/composer.go (declarations) ×5",
		"internal/api/mirror_view.go Server.handleMirrorViewPage ×4",
		"internal/api/send_queue.go (declarations) ×2",
		"internal/api/send_queue.go queueWordsOf ×1",
		"internal/api/send_queue.go sendQueueFailure ×5",
		"internal/api/send_queue_now.go (declarations) ×1",
		"internal/focus/focus.go HermesSubject ×1",
		"internal/mirror/agentstate.go Hub.AgentState ×1",
		"internal/mirror/hermes_answer.go Hub.AnswerHermesApproval ×3",
		"internal/mirror/hub.go Hub.Hosts ×4",
		"internal/mirror/hub.go hostConn.target ×1",
		"internal/mirror/hub.go hostConn.targetRuntime ×1",
		"internal/mirror/order.go (declarations) ×1",
		"internal/mirror/order.go SessionView.HermesView ×1",
		"internal/mirror/runtime.go (declarations) ×9",
		"internal/mirror/runtime.go HostView.GatedSendOn ×4",
		"internal/mirror/transcript.go PasteGuardRefusal ×1",
		"internal/mirror/transcript.go hostConn.claudeWaiting ×1",
		"internal/mirror/transcript.go hostConn.pasted ×2",
		"internal/mirror/turnerror.go boundTurnError ×1",
		"internal/mirror/wire.go (declarations) ×9",
		"internal/mirror/wire.go Window.AgentRuntime ×1",
		"internal/mirrorhost/client.go Client.runOnce ×2",
		"internal/mirrorhost/hermes.go hermesCLIShape ×2",
		"internal/mirrorhost/hermes.go hermesShape ×3",
		"internal/mirrorhost/hermes_asks.go Tmux.AttachHermesViewer ×2",
		"internal/mirrorhost/hermes_asks.go hermesAsks.attachPending ×3",
		"internal/mirrorhost/hermes_asks.go hostSide.answerHermes ×3",
		"internal/mirrorhost/hermes_asks.go hostSide.hermesAttacher ×1",
		"internal/mirrorhost/hermes_send.go HermesSendError.Refusal ×4",
		"internal/mirrorhost/hermes_send.go Tmux.HermesViMode ×2",
		"internal/mirrorhost/hermes_send.go Tmux.SendHermes ×2",
		"internal/mirrorhost/hermes_send.go hermesStillIdle ×7",
		"internal/mirrorhost/hermes_send.go hostSide.hermesPaste ×2",
		"internal/mirrorhost/hermes_send.go hostSide.hermesSend ×1",
		"internal/mirrorhost/hermes_source.go HermesSource.resolve ×1",
		"internal/mirrorhost/hermes_stream.go Tmux.HermesSource ×1",
		"internal/mirrorhost/hermes_stream.go hostSide.hermesWindow ×1",
		"internal/mirrorhost/runtime.go Tmux.hermesRuntime ×1",
		"internal/mirrorhost/runtime.go claudeRuntime.Name ×1",
		"internal/mirrorhost/runtime.go hermesRuntime.LocatePanes ×1",
		"internal/mirrorhost/runtime.go hermesRuntime.Name ×1",
		"internal/mirrorhost/runtime.go hermesRuntime.refusalKind ×1",
		"internal/mirrorhost/runtime.go openCodeRuntime.Name ×1",
		"internal/mirrorhost/transcript.go hostSide.watch ×1",
		"internal/ui/mirror_view.go MirrorViewPage ×7",
	}
	if !slices.Equal(sites, want) {
		t.Fatalf("the functions that name a runtime are\n  %s\nwant exactly\n  %s\nA per-runtime decision goes through the runtime interface (mirror.RuntimeSpec / mirrorhost.Runtime); update this ledger only on purpose",
			strings.Join(sites, "\n  "), strings.Join(want, "\n  "))
	}
}
