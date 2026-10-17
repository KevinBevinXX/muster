package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/jackc/pgx/v5"
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

// TestSearchSessionsReadsABoundedNumberOfMessageSets: when every session
// matches, ONE ranked read looks at ≤ limit+1 sessions' messages and ONE recent
// read at ≤ its batch — counted from EXPLAIN ANALYZE's loops.
//
// ⚠ WHAT IT DOES NOT PROVE. (1) It reads the plan the test server chose for this
// fixture; on PG16 the recent read's plan is bounded with or without the LIMIT
// inside the subquery, so it does not pin that LIMIT as the cause, nor the bound
// under another plan. (2) It is PER READ: a search with fewer matches than the
// limit walks recent reads until the sessions run out, so its total work is the
// agent's whole history (see nextRecentBatch; TestTheRecentWalkTakesFewRoundTrips).
// The one-scan ranked shape this replaced touches all 1000 here.
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

// countingQuerier counts the recent reads SearchSessions sends.
type countingQuerier struct {
	querier
	recent int
}

func (c *countingQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if sql == searchRecentSQL {
		c.recent++
	}
	return c.querier.Query(ctx, sql, args...)
}

// 🔴 A REGRESSION TEST: a search with fewer matches than the limit walks the
// recent reads until the sessions run out, and a FIXED batch of 2×limit made
// that one round trip per 2×limit sessions (measured: 114–144 ms for no match
// at 3000 sessions, against main's 60–72 ms). Here 1000 sessions, limit 10, one
// match in the OLDEST session: the walk must still find it (the work is the
// whole history — that is not what is bounded) in 6 reads, where the fixed
// batch took 51.
func TestTheRecentWalkTakesFewRoundTrips(t *testing.T) {
	s, pool, ctx := searchStore(t)
	const limit, n = 10, 1000
	a := searchAgent(t, s, pool, ctx, "walk")
	if _, err := pool.Exec(ctx, `
		WITH s AS (
			INSERT INTO chat_sessions (agent_id, session_key, title, updated_at)
			SELECT $1, $2 || g, 'thread ' || g, now() - make_interval(mins => g) FROM generate_series(1, $3::int) g
			RETURNING id, agent_id, updated_at, title)
		INSERT INTO chat_messages (agent_id, session_id, role, content, kind, created_at)
		SELECT s.agent_id, s.id, 'user',
		       CASE WHEN s.title = 'thread ' || $3::int THEN 'the oldest says walrus' ELSE 'nothing to see' END,
		       'text', s.updated_at
		FROM s`, a.ID, a.Name+"-", n); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	c := &countingQuerier{querier: s.pool}
	got, err := (&PGStore{pool: c}).SearchSessions(ctx, a.ID, "walrus", limit)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != fmt.Sprintf("thread %d", n) {
		t.Fatalf("got %+v, want the oldest session's one match", got)
	}
	if c.recent != 6 {
		t.Errorf("the walk sent %d recent reads over %d sessions at limit %d; want 6 (20, 40, … doubling)", c.recent, n, limit)
	}
}
