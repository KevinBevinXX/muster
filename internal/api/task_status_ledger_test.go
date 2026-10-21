package api

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// THE STATUS-WRITER LEDGER.
//
// 🔴 THIS FILE AND THE TEST IN IT DID NOT EXIST, AND THREE COMMENTS SAID THEY
// DID. task_status_writer.go names both — "Pair this with
// task_status_ledger_test.go, which pins that internal/api contains EXACTLY ONE
// call to Notes.SetStatus … and pins the set of setNoteStatus callers with the
// label each passes" — and then tells a maintainer that
// TestEveryStatusWriterIsLabelled, "not the compiler", is what catches an empty
// label. internal/notes/statuslog.go repeats the claim and explicitly CORRECTS
// an older, opposite comment "to be consistent with this". The correction was
// anchored to a test nobody had written.
//
// 🔴 THE HAZARD IS REAL AND WAS MEASURED, NOT IMAGINED. `statusWriter` is a
// defined string type, so `s.setNoteStatus(ctx, "", id, status)` COMPILES CLEAN
// — an untyped constant is assignable to it. The write then logs
// `writer=UNLABELLED`, which is the loud sentinel, but only if anybody is
// reading; nothing fails, nothing 404s, and the task's status transition is
// attributed to nobody for ever.
//
// 🔴 WHAT THIS GUARD WOULD STILL ACCEPT, STATED SO NOBODY OVER-READS IT: it
// checks that every writer argument is a CONSTANT IDENTIFIER from the declared
// set, and that the set of call sites is the ledgered one. It does NOT check
// that each site passes the RIGHT label — `writerHumanUI` on the machine route
// would sail through, and that is a confidently-wrong attribution rather than a
// missing one. Only reading the call site catches that. The ledger below is
// therefore spelled with the label beside the function, so the review that
// catches it has somewhere to happen.
// ---------------------------------------------------------------------------

// expectedStatusWriterLedger is every function in this package that reaches
// notes.Store.SetStatus, and the writer label each one attributes its write to.
//
// 🔴 IT FAILS WHEN THE SET GROWS *OR* SHRINKS. An allowlist would silently
// absorb the next writer — which is the whole failure mode: a status write from
// a path that never announced itself. A vanished entry is a failure too, because
// the label constant it names is then dead and the next person to add a writer
// may reuse the string, making an old log row and a new one indistinguishable
// (task_status_writer.go records exactly that hazard for two deleted labels).
//
// ⚠ `applyTaskStatus` IS A FORWARDER, NOT A WRITER. It takes a statusWriter
// parameter because it is shared by two routes that must not be attributed to
// each other, so its OWN callers carry the labels and are listed here instead.
// The resolver below follows one level of forwarding; a second level would need
// this comment and that code to change together.
var expectedStatusWriterLedger = map[string][]string{
	// PATCH /tasks/{id}/status — the human card control, via applyTaskStatus.
	"handleNoteStatus": {"writerHumanUI"},
	// POST /tasks/{id}/complete — the agent page's Mark-complete without
	// JavaScript, via applyTaskStatus: the same human, so the same label.
	"handleNoteComplete": {"writerHumanUI"},
	// PATCH /api/tasks/{id}/status — the hook-token machine route, via applyTaskStatus.
	"handleAPITaskStatus": {"writerMachineAPI"},
	// The open→in_progress advance that follows a dispatch.
	"advanceTaskToInProgress": {"writerDispatchAdvance"},
	// The merge retiring the LOSER of a task merge to complete.
	"handleTaskMerge": {"writerTaskMerge"},
	// PATCH /agent/task/status — the in-devpod agent's HTTP route.
	"handleAgentTaskStatus": {"writerAgentRoute"},
	// The native function tool `agent_set_task_status`, which has NO route and
	// therefore produces no request log line — the path the original incident
	// could not distinguish, and the reason this seam exists at all.
	"dispatchAgentTool": {"writerAgentTool"},
	// The all-agents tool `task_set_status` (any of the owner's tasks, by id).
	"toolTaskSetStatus": {"writerTaskTool"},
	// The in_progress→open step after a mirrored session's agent_destroy.
	"reopenTaskAfterDestroy": {"writerDestroyReopen"},
}

// TestEveryStatusWriterIsLabelled pins every caller of setNoteStatus to the
// constant it passes, and reports a non-constant as
// `mutantEmptyLabel→<not-a-constant-identifier>`.
func TestEveryStatusWriterIsLabelled(t *testing.T) {
	fset := token.NewFileSet()
	files := parsePackageSources(t, fset)

	// 🔴 POSITIVE CONTROL ON THE PARSE. An empty file set produces an empty
	// ledger, and an empty ledger would fail the comparison below — but with a
	// message about missing writers rather than about a broken instrument, which
	// is the wrong diagnosis to hand someone at 2am.
	if len(files) == 0 {
		t.Fatal("positive control FAILED: no non-test sources parsed from this package")
	}
	t.Logf("positive control: parsed %d non-test source file(s)", len(files))

	consts := declaredStatusWriters(files)
	if len(consts) == 0 {
		t.Fatal("positive control FAILED: no statusWriter constants found; the resolver " +
			"below would classify every call site as non-constant")
	}

	// Pass 1: every setNoteStatus call site, with the writer argument it passes.
	type site struct{ fn, arg string }
	var sites []site
	var forwarders []string // functions that pass a statusWriter PARAMETER through
	for _, f := range files {
		forEachCallTo(f, "setNoteStatus", func(enclosing string, call *ast.CallExpr) {
			if len(call.Args) < 2 {
				t.Errorf("%s calls setNoteStatus with %d argument(s); the writer is the second",
					enclosing, len(call.Args))
				return
			}
			ident, ok := call.Args[1].(*ast.Ident)
			if !ok {
				// The exact spelling task_status_writer.go promises.
				t.Errorf("%s: mutantEmptyLabel→<not-a-constant-identifier> — the writer "+
					"argument is %s, not one of the declared statusWriter constants.\n"+
					"  `statusWriter` is a defined string type, so a bare \"\" compiles and\n"+
					"  the write is attributed to nobody. Pass a constant from\n"+
					"  task_status_writer.go, or declare a new one there.",
					enclosing, exprText(call.Args[1]))
				return
			}
			if consts[ident.Name] {
				sites = append(sites, site{enclosing, ident.Name})
				return
			}
			if isStatusWriterParam(files, enclosing, ident.Name) {
				forwarders = append(forwarders, enclosing)
				return
			}
			t.Errorf("%s: mutantEmptyLabel→<not-a-constant-identifier> — the writer "+
				"argument %q is neither a declared statusWriter constant nor a "+
				"statusWriter parameter of the enclosing function.", enclosing, ident.Name)
		})
	}

	// Pass 2: resolve one level of forwarding. A forwarder's own callers supply
	// the label, so they are the writers the ledger names.
	for _, fwd := range forwarders {
		found := 0
		for _, f := range files {
			forEachCallTo(f, fwd, func(enclosing string, call *ast.CallExpr) {
				found++
				if len(call.Args) < 2 {
					t.Errorf("%s calls %s with %d argument(s)", enclosing, fwd, len(call.Args))
					return
				}
				ident, ok := call.Args[1].(*ast.Ident)
				if !ok || !consts[ident.Name] {
					t.Errorf("%s: mutantEmptyLabel→<not-a-constant-identifier> — calls the "+
						"forwarder %s with %s, which is not a declared statusWriter constant.",
						enclosing, fwd, exprText(call.Args[1]))
					return
				}
				sites = append(sites, site{enclosing, ident.Name})
			})
		}
		if found == 0 {
			t.Errorf("%s forwards a statusWriter to setNoteStatus but nothing in this "+
				"package calls it. Either it is dead, or it is called from outside — "+
				"in which case the label it receives is unledgered.", fwd)
		}
	}

	got := map[string][]string{}
	for _, s := range sites {
		got[s.fn] = append(got[s.fn], s.arg)
	}
	for k := range got {
		sort.Strings(got[k])
	}

	// 🔴 THE COMPARISON IS BOTH DIRECTIONS. A subset check would pass for a
	// package that had lost every writer but one.
	for fn, want := range expectedStatusWriterLedger {
		g, ok := got[fn]
		if !ok {
			t.Errorf("%s is on the ledger with %v but no longer reaches setNoteStatus.\n"+
				"  If the writer was retired, remove the entry AND read the note in\n"+
				"  task_status_writer.go about not reusing a retired label string.",
				fn, want)
			continue
		}
		if strings.Join(g, ",") != strings.Join(want, ",") {
			t.Errorf("%s writes status as %v, ledgered as %v.\n"+
				"  A label that changed is a change in who the log attributes the\n"+
				"  write to. Update the ledger deliberately.", fn, g, want)
		}
	}
	for fn, g := range got {
		if _, ok := expectedStatusWriterLedger[fn]; !ok {
			t.Errorf("%s writes a task status as %v and is NOT on the ledger.\n"+
				"  A new status writer must be added to expectedStatusWriterLedger in\n"+
				"  this file, with the label it attributes its writes to.", fn, g)
		}
	}

	t.Logf("status-writer ledger: %d writer(s), %d forwarder(s)", len(got), len(forwarders))
}

// TestSetNoteStatusIsTheOnlyPlaceThisPackageCallsTheStoresSetStatus is the other
// half of the pair task_status_writer.go describes.
//
// 🔴 WITHOUT IT THE LEDGER ABOVE IS WALKABLE BY NOT USING THE SEAM AT ALL. A
// handler that calls `s.ext.Notes.SetStatus` directly writes a status with no
// writer on the context, logs `writer=UNLABELLED`, and appears in no ledger —
// because the ledger is built from calls to setNoteStatus, which that handler
// never makes. The guard on the label and the guard on the chokepoint only mean
// anything together.
func TestSetNoteStatusIsTheOnlyPlaceThisPackageCallsTheStoresSetStatus(t *testing.T) {
	fset := token.NewFileSet()
	files := parsePackageSources(t, fset)

	// The receivers a SetStatus call is permitted on, and why.
	permitted := map[string]string{
		// The chokepoint itself. This is the one the ledger above is built around.
		"s.ext.Notes": "setNoteStatus, the chokepoint",
		// The liveness decorator delegating to the store it wraps. It adds no
		// status behaviour and cannot: it does not know who the writer is, and it
		// is reached THROUGH the chokepoint, which has already stamped the context.
		"l.Store": "liveNotes delegating to the wrapped store",
	}

	var offenders []string
	seen := map[string]int{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "SetStatus" {
				return true
			}
			recv := exprText(sel.X)
			seen[recv]++
			if _, ok := permitted[recv]; !ok {
				offenders = append(offenders, fmt.Sprintf("%s.SetStatus at %s",
					recv, fset.Position(call.Pos())))
			}
			return true
		})
	}

	// 🔴 POSITIVE CONTROL: THE SCAN MUST HAVE FOUND THE CHOKEPOINT. A scan that
	// found nothing reports zero offenders exactly the way a clean package does.
	if seen["s.ext.Notes"] == 0 {
		t.Fatalf("positive control FAILED: the scan found no s.ext.Notes.SetStatus call. "+
			"setNoteStatus contains one, so this instrument is not measuring. saw: %v", seen)
	}
	if n := seen["s.ext.Notes"]; n != 1 {
		t.Errorf("this package calls s.ext.Notes.SetStatus %d times, want exactly 1 "+
			"(inside setNoteStatus). A second call site writes a status with no writer "+
			"on the context and appears in no ledger.", n)
	}
	for _, o := range offenders {
		t.Errorf("%s — not a permitted receiver for a status write.\n"+
			"  Route it through setNoteStatus so the write carries a writer label.", o)
	}
	t.Logf("positive control: SetStatus call sites by receiver: %v", seen)
}

// --- AST helpers -------------------------------------------------------------

// parsePackageSources parses every non-test .go file of this package.
func parsePackageSources(t *testing.T, fset *token.FileSet) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	var out []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		out = append(out, f)
	}
	return out
}

// declaredStatusWriters returns the set of constants declared with the
// statusWriter type.
func declaredStatusWriters(files []*ast.File) map[string]bool {
	set := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			gen, ok := d.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "statusWriter" {
					continue
				}
				for _, n := range vs.Names {
					set[n.Name] = true
				}
			}
		}
	}
	return set
}

// forEachCallTo visits every call to a method named name, reporting the name of
// the function the call sits inside.
func forEachCallTo(f *ast.File, name string, fn func(enclosing string, call *ast.CallExpr)) {
	for _, d := range f.Decls {
		decl, ok := d.(*ast.FuncDecl)
		if !ok || decl.Body == nil {
			continue
		}
		// A function does not count as a caller of itself.
		if decl.Name.Name == name {
			continue
		}
		ast.Inspect(decl.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
				fn(decl.Name.Name, call)
			}
			return true
		})
	}
}

// isStatusWriterParam reports whether ident is a statusWriter PARAMETER of the
// function named enclosing — i.e. the function is a forwarder, not a writer.
func isStatusWriterParam(files []*ast.File, enclosing, ident string) bool {
	for _, f := range files {
		for _, d := range f.Decls {
			decl, ok := d.(*ast.FuncDecl)
			if !ok || decl.Name.Name != enclosing || decl.Type.Params == nil {
				continue
			}
			for _, p := range decl.Type.Params.List {
				id, ok := p.Type.(*ast.Ident)
				if !ok || id.Name != "statusWriter" {
					continue
				}
				for _, n := range p.Names {
					if n.Name == ident {
						return true
					}
				}
			}
		}
	}
	return false
}

// exprText renders an expression for a message. It handles the shapes that
// appear as a receiver or an argument here and names anything else by its type,
// which is enough to find it.
func exprText(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.BasicLit:
		return v.Value
	case *ast.SelectorExpr:
		return exprText(v.X) + "." + v.Sel.Name
	case *ast.CallExpr:
		return exprText(v.Fun) + "(…)"
	default:
		return fmt.Sprintf("%T", e)
	}
}

// ---------------------------------------------------------------------------
// THE COMPLETE-WRITER LEDGER, AND THE STOP ON THE FUNNEL.
//
// A task becoming `complete` stops the agent the chief started for it
// (stopChiefAgentOnComplete). It is called from setNoteStatus, so every writer
// above reaches it by construction; these two tests pin the two halves of that
// claim:
//
//   - TestTheCompleteStopIsOnTheStatusFunnel: setNoteStatus calls it, and
//     NOTHING ELSE in the package does — a second call site would mean the stop
//     had started to move out to the writers, where the next one forgets it.
//   - TestEveryStatusWriterIsClassifiedForComplete: every writer on
//     expectedStatusWriterLedger is classified as able to set `complete` or not,
//     and the set FAILS WHEN IT GROWS OR SHRINKS — a new writer must be
//     classified, and the ones that can set it are each driven end to end by
//     TestEveryCompleteWriterStopsTheChiefsAgent (task_complete_stop_pg_test.go).
//
// ⚠ The false entries are claims read from the code, not checked here: the
// three agent paths refuse `complete` (agentStatusRefusal, notes.ForAgent), and
// the dispatch advance and destroy reopen write in_progress and open.
// ---------------------------------------------------------------------------

// expectedCompleteWriters: every status writer, and whether it can set complete.
var expectedCompleteWriters = map[string]bool{
	"handleNoteStatus":        true,  // the human card control: any status
	"handleNoteComplete":      true,  // Mark complete
	"handleAPITaskStatus":     true,  // the machine route: any status, unless an agent calls it
	"handleTaskMerge":         true,  // retires the merge's loser to complete
	"advanceTaskToInProgress": false, // open → in_progress
	"handleAgentTaskStatus":   false, // an agent: never complete
	"dispatchAgentTool":       false, // agent_set_task_status: never complete
	"toolTaskSetStatus":       false, // task_set_status (agents): never complete
	"reopenTaskAfterDestroy":  false, // in_progress → open
}

func TestEveryStatusWriterIsClassifiedForComplete(t *testing.T) {
	if len(expectedStatusWriterLedger) == 0 {
		t.Fatal("positive control FAILED: the status-writer ledger is empty")
	}
	for fn := range expectedStatusWriterLedger {
		if _, ok := expectedCompleteWriters[fn]; !ok {
			t.Errorf("status writer %s is not classified in expectedCompleteWriters: "+
				"say whether it can set `complete` (and if it can, drive it in "+
				"TestEveryCompleteWriterStopsTheChiefsAgent)", fn)
		}
	}
	for fn := range expectedCompleteWriters {
		if _, ok := expectedStatusWriterLedger[fn]; !ok {
			t.Errorf("expectedCompleteWriters names %s, which is not a status writer any more", fn)
		}
	}
}

func TestTheCompleteStopIsOnTheStatusFunnel(t *testing.T) {
	fset := token.NewFileSet()
	files := parsePackageSources(t, fset)
	var callers []string
	for _, f := range files {
		forEachCallTo(f, "stopChiefAgentOnComplete", func(enclosing string, _ *ast.CallExpr) {
			callers = append(callers, enclosing)
		})
	}
	sort.Strings(callers)
	if got := strings.Join(callers, ","); got != "setNoteStatus" {
		t.Errorf("stopChiefAgentOnComplete is called from [%s], want exactly [setNoteStatus]: "+
			"the funnel every status writer passes, so no writer of `complete` can skip it", got)
	}
}
