package notes

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The note_comments SEAM ledger.
//
// 🔴 THE DEFECT SHAPE THIS EXISTS FOR. A retraction rule applied at N−1 of N query
// sites is the classic failure here, and it is INVISIBLE to a behavioural test
// scoped to one surface: the per-note read (ListComments, behind Get and so behind
// GET /api/tasks/{id} and the re-rendered card) and the batched read
// (listCommentsForNotes, behind List/ListByTags and so behind GET /api/tasks and
// the whole board) return the same rows through different SQL. Fix one and the
// task view looks perfect while the board still renders the retracted BODY.
//
// So this asserts the LEDGER — the exact set of places that touch the table, and
// which constant each is required to go through. It fails if the set GROWS (a new
// query nobody redacted) or SHRINKS (a query removed, so a behavioural test may
// have gone quietly vacuous). It is a STRUCTURAL check and is deliberately paired
// with behavioural cases: internal/api's TestRetractedCommentBodyAbsentEverywhere
// walks the actual HTTP surfaces, and (PG-gated)
// TestPGStoreCommentSoftDeleteTombstonesBothReads measures the real SQL.
//
// 🔴 TWO DIFFERENT CONSTANTS, AND THAT IS THE POINT. The READS must go through
// `commentCols` — the projection that blanks a retracted body in Postgres — and
// must NOT carry `liveOnly`, because filtering the row out is exactly the
// silently-shortening-thread behaviour this design rejects. The WRITES must carry
// `liveOnly` (AddComment guards on a live parent; SoftDeleteComment guards on a
// live comment AND a live parent). A read that swapped to liveOnly, or a write
// that dropped it, is a different bug and each is caught below.
//
// 🔴 IT READS STRING LITERALS, NOT FILE TEXT. A first version grepped the raw
// source and was wrong twice over: every doc COMMENT that mentions the table
// counted as a query site, and no site counted as filtered because the predicate
// is concatenated in as a CONSTANT, so the literal "deleted_at IS NULL" appears in
// none of them. Both errors point the same direction — a text grep over code is a
// claim about the text.

// commentSite is the required constant usage for one function's SQL.
type commentSite struct {
	// liveOnly is whether the function's body must reference the liveOnly
	// predicate constant.
	liveOnly bool
	// commentCols is whether it must reference the redacting projection constant.
	commentCols bool
}

// wantCommentSites is the ledger. Update it DELIBERATELY, with the behavioural
// test to match — never merely to make this test green again.
var wantCommentSites = map[string]commentSite{
	// Read #1: the per-note thread (Get → the task view, GET /api/tasks/{id}).
	// Redacts, does NOT filter — a retracted comment stays in the thread as a
	// tombstone so the thread cannot silently shorten.
	"ListComments": {liveOnly: false, commentCols: true},
	// Read #2: the batched board fan-out (List/ListByTags → GET /api/tasks,
	// /ui/tasks). Same rule, and it has to be the same constant or the two drift.
	"listCommentsForNotes": {liveOnly: false, commentCols: true},
	// Write: the INSERT, guarded on a LIVE PARENT (a comment on a dismissed task
	// would otherwise be written into a thread nothing can see).
	"AddComment": {liveOnly: true, commentCols: false},
	// Write: the retraction itself — the only writer of note_comments.deleted_at.
	"SoftDeleteComment": {liveOnly: true, commentCols: false},
	// Write + a NON-BODY probe: the idle-task reaper's single statement. It INSERTs
	// the reap comment (hence liveOnly — a dismissed task must not gain one, same
	// rule as AddComment) and it also SCANS note_comments in a `NOT EXISTS` to
	// decide whether this task has already been reaped since its last activity.
	//
	// 🔴 THAT SCAN IS DELIBERATELY *NOT* THROUGH commentCols, and the reason is the
	// opposite of a leak: it selects no columns at all. It reads only `author` and
	// `created_at` inside an EXISTS, never `body`, so there is nothing for the
	// redacting projection to redact — adding commentCols here would be cargo cult.
	//
	// 🔴 It also deliberately does NOT filter `deleted_at IS NULL`. A RETRACTED reap
	// comment must still suppress the next reap: filtering it out would let an
	// operator who retracts the notice re-arm the reaper, and the next 30-minute
	// sweep would post a fresh one — a retract/repost loop. Behavioural case:
	// TestFlagIdleStaysSuppressedByARetractedReapComment.
	"FlagIdle": {liveOnly: true, commentCols: false},
	// Write + a NON-BODY-RETURNING probe: the plugin task route's idempotent
	// comment. It INSERTs (liveOnly, AddComment's rule) and its `NOT EXISTS`
	// compares author and body to decide whether the comment is already there.
	// It RETURNS no comment column, so nothing can leak; and it deliberately
	// does NOT filter `deleted_at IS NULL`, so a comment the owner retracted is
	// not written again by a retry — the same reasoning as FlagIdle's.
	// Behavioural case: TestAddCommentOnceWritesOnceAndRespectsRetraction.
	// It lives in addCommentOnceTx, shared by AddCommentOnce and
	// CreateOnceByTag (which records a create's comment in its own transaction).
	"addCommentOnceTx": {liveOnly: true, commentCols: false},
	// NOT a comment read: the board's ACTIVITY ORDERING. Its lateral selects
	// `max(note_comments.created_at)` and no comment column at all, so there is no
	// body for commentCols to redact — which is why commentCols is false here and
	// why that is not the drift this ledger warns about elsewhere. (It does
	// PREDICATE on `c.author`, to exclude ReapCommentAuthor — see FlagIdle above
	// and TestReapedTaskDoesNotFloatToTheTopOfTheBoard — but predicating on a
	// column is not selecting one, so that does not change either flag.)
	//
	// ⚠ READ THE liveOnly FLAG NARROWLY HERE, AND ONLY HERE. It is true because
	// ListPage references the constant for the NOTES predicate (`n.`+liveOnly);
	// the AST detector matches an identifier, so it cannot tell which table the
	// predicate is applied to. So this entry does NOT assert anything about
	// comment filtering, and it would stay green if someone added a comment-level
	// `deleted_at IS NULL` to that lateral. What covers THAT is behavioural, not
	// structural: TestListPageRetractedCommentStillFloatsTheTask (PG-gated) puts a
	// retracted comment on a task and requires it to keep its position — because
	// a retraction must not silently sink a task down the board, for the same
	// reason a retracted comment tombstones instead of vanishing.
	"ListPage": {liveOnly: true, commentCols: false},
}

// sqlSites maps each function in file that has a string literal mentioning
// `table` to which of the tracked constants that function references. It also
// returns how many such literals were found in total and how many were inside a
// function, so a query parked in a package-level constant cannot escape.
//
// 🔴 commentCols is itself a package-level constant containing the literal
// "note_comments"? No — it deliberately does not name the table, only columns, so
// it is not counted as a query site. If that ever changes, the total != inFuncs
// assertion below fires rather than the ledger silently gaining a phantom entry.
func sqlSites(t *testing.T, file, table string) (sites map[string]commentSite, total, inFuncs int) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0) // comments dropped: only code counts
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	sites = map[string]commentSite{}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if ok && lit.Kind == token.STRING && strings.Contains(lit.Value, table) {
			total++
		}
		return true
	})
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		var mentions int
		var got commentSite
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.BasicLit:
				if v.Kind == token.STRING && strings.Contains(v.Value, table) {
					mentions++
				}
			case *ast.Ident:
				switch v.Name {
				case "liveOnly":
					got.liveOnly = true
				case "commentCols":
					got.commentCols = true
				}
			}
			return true
		})
		if mentions > 0 {
			inFuncs += mentions
			sites[fn.Name.Name] = got
		}
	}
	return sites, total, inFuncs
}

// TestNoteCommentsReadPathLedger enumerates every function in the notes package
// that issues SQL against note_comments and pins the set against wantCommentSites.
func TestNoteCommentsReadPathLedger(t *testing.T) {
	const file = "pgstore.go"
	got, total, inFuncs := sqlSites(t, file, "note_comments")

	// Positive control on the instrument: if it found no SQL literal at all, every
	// assertion below would pass vacuously.
	if total == 0 {
		t.Fatalf("positive control failed: no string literal in %s mentions note_comments", file)
	}
	// Negative control on the instrument: it must be able to tell a function that
	// carries liveOnly from one that does not. `notes` is queried by both — Delete
	// has no liveOnly, Get does.
	notesSites, _, _ := sqlSites(t, file, "FROM notes")
	if notesSites["Delete"].liveOnly {
		t.Errorf("instrument check failed: Delete reports as carrying liveOnly, but it deliberately does not")
	}
	if !notesSites["Get"].liveOnly {
		t.Errorf("instrument check failed: Get reports as NOT carrying liveOnly, but it does")
	}
	// The same control for the OTHER constant, which is what the reads now hinge
	// on: Get must NOT report commentCols (it is a note read), and a detector that
	// answered true for everything would fail here.
	if notesSites["Get"].commentCols {
		t.Errorf("instrument check failed: Get reports as carrying commentCols, but it is a note read")
	}

	if total != inFuncs {
		t.Errorf("%d of %d note_comments SQL literals in %s are outside any function body — "+
			"a query held in a package-level constant would escape this ledger", total-inFuncs, total, file)
	}

	for name, want := range wantCommentSites {
		g, ok := got[name]
		if !ok {
			t.Errorf("ledger SHRANK: %s no longer queries note_comments. If that is deliberate, "+
				"remove it from wantCommentSites AND check whether a behavioural test just went vacuous.", name)
			continue
		}
		if g.commentCols != want.commentCols {
			t.Errorf("%s: goes through the commentCols redacting projection = %v, want %v. "+
				"A comment READ that hand-rolls its column list will select a retracted body out of Postgres.",
				name, g.commentCols, want.commentCols)
		}
		if g.liveOnly != want.liveOnly {
			t.Errorf("%s: references the liveOnly predicate = %v, want %v. "+
				"A comment READ carrying liveOnly would DROP retracted comments instead of tombstoning them — "+
				"the thread would silently shorten, which is the behaviour 0021 deliberately rejects.",
				name, g.liveOnly, want.liveOnly)
		}
	}
	for name := range got {
		if _, ok := wantCommentSites[name]; !ok {
			t.Errorf("ledger GREW: %s queries note_comments and is not in wantCommentSites. "+
				"A new read that does not go through commentCols leaks retracted bodies; add it deliberately.", name)
		}
	}
}

// TestNoteCommentsQueriedOnlyByPGStore is the other half of the ledger: it proves
// no OTHER package reaches the table directly, so redacting the store's queries
// redacts ALL of them. A handler or a report running its own
// `SELECT … FROM note_comments` would bypass the predicate entirely, and this is
// what would catch it. It reads string literals, so the several doc comments that
// legitimately discuss the table (internal/api/notes.go, internal/notes/merge.go,
// internal/notes/notes.go) are not offenders.
func TestNoteCommentsQueriedOnlyByPGStore(t *testing.T) {
	root := filepath.Join("..", "..") // the module root
	var offenders []string
	var scanned, withLiteral int
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case "node_modules", ".git", "chart":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scanned++
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil // a file we cannot parse is not evidence of a query
		}
		var hit bool
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING &&
				strings.Contains(lit.Value, "note_comments") {
				hit = true
			}
			return true
		})
		if !hit {
			return nil
		}
		withLiteral++
		if filepath.Base(path) == "pgstore.go" && strings.Contains(path, filepath.Join("internal", "notes")) {
			return nil
		}
		offenders = append(offenders, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	// Positive controls: the walk really visited the tree, and it really found the
	// one file that DOES carry the literals. A clean zero from a walk wired to
	// nothing looks identical to a clean zero from a clean tree.
	if scanned < 20 {
		t.Fatalf("positive control failed: only %d non-test .go files scanned under %s — the walk is not reaching the tree", scanned, root)
	}
	if withLiteral == 0 {
		t.Fatalf("positive control failed: the walk found NO file containing a note_comments SQL literal, not even pgstore.go")
	}
	if len(offenders) > 0 {
		t.Errorf("note_comments is queried outside internal/notes/pgstore.go, bypassing the commentCols redaction: %v", offenders)
	}
}
