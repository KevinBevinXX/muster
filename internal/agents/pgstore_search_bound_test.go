package agents

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/ZacxDev/muster/internal/db"
)

// messageLoops runs EXPLAIN (ANALYZE, FORMAT JSON) of query and sums the
// "Actual Loops" of every plan node that reads chat_messages: how many
// sessions' messages the query actually looked at.
func messageLoops(t *testing.T, query string, args ...any) int {
	t.Helper()
	s, pool, ctx := searchStore(t)
	_ = s
	var raw []byte
	if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+query, args...).Scan(&raw); err != nil {
		t.Fatalf("explain: %v", err)
	}
	var plans []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil {
		t.Fatal(err)
	}
	loops := 0
	var walk func(n map[string]any)
	walk = func(n map[string]any) {
		if n["Relation Name"] == "chat_messages" {
			loops += int(n["Actual Loops"].(float64))
		}
		if kids, ok := n["Plans"].([]any); ok {
			for _, k := range kids {
				walk(k.(map[string]any))
			}
		}
	}
	walk(plans[0].Plan)
	return loops
}

// TestSearchSessionsReadsABoundedNumberOfMessageSets pins what makes thread
// search cheap: neither read looks at more sessions' messages than the limit,
// however many sessions match.
//
// 🔴 A ROW-COUNT GUARD, NOT A CLOCK. Ranking the whole title-or-body match set
// in one query made Postgres read the messages of EVERY session before its
// LIMIT applied (measured: 3000 sessions, ~1.8 ms → ~200 ms warm). Here 1000
// sessions all match and the limit is 10: each read must touch ≤ 10 sessions'
// messages — the one-scan shape touches 1000.
func TestSearchSessionsReadsABoundedNumberOfMessageSets(t *testing.T) {
	s, pool, ctx := searchStore(t)
	const limit, n = 10, 1000
	titled := searchAgent(t, s, pool, ctx, "boundtitle")
	bodied := searchAgent(t, s, pool, ctx, "boundbody")
	// n sessions per agent, each with three text messages that match — written in
	// bulk, then ANALYZEd, so the planner sees the table a real chief has.
	for _, f := range []struct {
		a     Agent
		title string
	}{{titled, "kite run "}, {bodied, "zz run "}} {
		if _, err := pool.Exec(ctx, `
			WITH s AS (
				INSERT INTO chat_sessions (agent_id, session_key, title, updated_at)
				SELECT $1, $2 || g, $3 || g, now() - make_interval(mins => g) FROM generate_series(1, $4::int) g
				RETURNING id, agent_id, updated_at)
			INSERT INTO chat_messages (agent_id, session_id, role, content, kind, created_at)
			SELECT s.agent_id, s.id, 'user', 'the kite flew ' || m, 'text', s.updated_at - make_interval(secs => m)
			FROM s, generate_series(1, 3) m`, f.a.ID, f.a.Name+"-", f.title, n); err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `ANALYZE chat_sessions; ANALYZE chat_messages`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	// ⚠ limit+1, NOT limit, for the ranked read: measured, its nested loop
	// evaluated one candidate past the tenth before the Limit stopped it. The
	// shape this guards against reads all 1000.
	bound := limit + 1
	q := "kite"
	pattern, prefix, word := "%"+db.LikeEscape(q)+"%", db.LikeEscape(q)+"%", `(^|[^[:alnum:]])`+regexpLiteral(q)

	if got := messageLoops(t, searchRankedTitlesSQL, titled.ID, q, pattern, SearchSnippetRadius, SearchSnippetLen, limit, prefix, word); got > bound || got == 0 {
		t.Errorf("the ranked read looked at %d sessions' messages for %d title matches; want 1..%d", got, n, bound)
	}
	batch := searchRecentBatch(limit)
	if got := messageLoops(t, searchRecentSQL, bodied.ID, q, pattern, SearchSnippetRadius, SearchSnippetLen, nil, int64(0), batch); got > batch || got == 0 {
		t.Errorf("one recent read looked at %d sessions' messages of %d; want 1..%d (one batch)", got, n, batch)
	}
	// And the answer itself is the limit, best first.
	got, err := s.SearchSessions(ctx, titled.ID, q, limit)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != limit {
		t.Errorf("%d results, want %d", len(got), limit)
	}
}

// TestSearchSessionsIsTheTwoBoundedReads: the bound above is about two SQL
// texts, so SearchSessions must be what runs them — read from its source, so
// a third query added beside them (or one swapped out) fails here instead of
// leaving the guard pinning a constant nothing executes.
func TestSearchSessionsIsTheTwoBoundedReads(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "pgstore.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var body *ast.BlockStmt
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "SearchSessions" {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("no SearchSessions in pgstore.go")
	}
	used := map[string]bool{}
	var stray []string
	ast.Inspect(body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			used[id.Name] = true
		}
		// Every statement SearchSessions sends is one of the two bounded reads:
		// pool.Query's query argument, and searchRows' (which runs one).
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Query" || sel.Sel.Name == "QueryRow" || sel.Sel.Name == "searchRows") && len(call.Args) >= 2 {
				if id, ok := call.Args[1].(*ast.Ident); !ok || (id.Name != "searchRankedTitlesSQL" && id.Name != "searchRecentSQL") {
					stray = append(stray, sel.Sel.Name)
				}
			}
		}
		return true
	})
	for _, q := range []string{"searchRankedTitlesSQL", "searchRecentSQL"} {
		if !used[q] {
			t.Errorf("SearchSessions does not run %s, whose bound TestSearchSessionsReadsABoundedNumberOfMessageSets pins", q)
		}
	}
	if len(stray) != 0 {
		t.Errorf("SearchSessions sends a statement that is not one of the two bounded reads: %v", stray)
	}
}
