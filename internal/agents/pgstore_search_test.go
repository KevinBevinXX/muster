package agents

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// --- SearchSessions, against a REAL Postgres ----------------------------------
//
// 🔴 THIS FILE IS THE AUTHORITY ON THE kind='text' PREDICATE AND NOTHING ELSE CAN
// BE. The filter is SQL. internal/api's handler tests run against a fake store, so
// a fake that reproduced the predicate would make every assertion there green
// whatever the query said — which is deriving a test's expectation from an
// implementation, one layer removed. The query is what has to be watched to
// behave, so it is watched against the database it runs on.
//
// ⚠ IT SKIPS WITHOUT MUSTER_TEST_DATABASE_URL, like every other PG-gated test in
// this package. A skip is not a pass: the exclusion claim in the PR description is
// backed by a run with the variable SET, and the run is named there.

func searchStore(t *testing.T) (*PGStore, *pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewPG(pool), pool, ctx
}

// searchAgent creates a throwaway agent, cleaned up (cascading to its sessions and
// messages) when the test ends.
func searchAgent(t *testing.T, s *PGStore, pool *pgxpool.Pool, ctx context.Context, suffix string) Agent {
	t.Helper()
	name := "search-" + suffix + "-" + time.Now().Format("150405.000000000")
	a, err := s.Create(ctx, Agent{Name: name, Namespace: "agent-" + name, Status: StatusProvisioning})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() { cleanupAgent(dbtest.AsOperator(context.Background()), pool, a.ID) })
	return a
}

// searchThread opens a thread on an agent and writes the given parts verbatim.
//
// 🔴 IT WRITES THE ROWS DIRECTLY RATHER THAN THROUGH AddChatMessage, because
// AddChatMessage derives the session TITLE from the first user message — and a
// fixture whose body text also became its title could not tell a title match from a
// body match, which is the distinction half this file turns on.
func searchThread(t *testing.T, s *PGStore, pool *pgxpool.Pool, ctx context.Context, a Agent, title string, parts ...ChatMessage) ChatSession {
	t.Helper()
	sess, err := s.CreateSession(ctx, a.ID, a.Name)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if title != "" {
		if _, err := pool.Exec(ctx, `UPDATE chat_sessions SET title=$1 WHERE id=$2`, title, sess.ID); err != nil {
			t.Fatalf("set title: %v", err)
		}
	}
	for i, p := range parts {
		if _, err := pool.Exec(ctx, `
			INSERT INTO chat_messages (agent_id, session_id, role, content, kind, tool_id, tool_name, tool_ok, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8, now() + make_interval(secs => $9::float8))`,
			a.ID, sess.ID, p.Role, p.Content, p.Kind, p.ToolID, p.ToolName, p.ToolOK, float64(i)/1000); err != nil {
			t.Fatalf("insert message %d: %v", i, err)
		}
	}
	return sess
}

func matchIDs(ms []SessionMatch) []int64 {
	out := make([]int64, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

func containsID(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestSearchSessionsMatchesTitlesAndTextBodies — the two sources, and the control
// that a non-matching thread stays out.
func TestSearchSessionsMatchesTitlesAndTextBodies(t *testing.T) {
	s, pool, ctx := searchStore(t)
	a := searchAgent(t, s, pool, ctx, "sources")

	const term = "ZEBRAQUARTZ"
	byTitle := searchThread(t, s, pool, ctx, a, "a thread about "+term,
		ChatMessage{Role: "user", Kind: "text", Content: "nothing in this body"})
	byBody := searchThread(t, s, pool, ctx, a, "an unrelated title",
		ChatMessage{Role: "assistant", Kind: "text", Content: "the body mentions " + term + " here"})
	neither := searchThread(t, s, pool, ctx, a, "also unrelated",
		ChatMessage{Role: "user", Kind: "text", Content: "and so is this body"})

	got, err := s.SearchSessions(ctx, a.ID, strings.ToLower(term), 10)
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}
	ids := matchIDs(got)
	if !containsID(ids, byTitle.ID) {
		t.Errorf("a TITLE match is missing (got %v)", ids)
	}
	if !containsID(ids, byBody.ID) {
		t.Errorf("a kind='text' BODY match is missing (got %v)", ids)
	}
	if containsID(ids, neither.ID) {
		t.Errorf("a thread matching neither title nor body is in the results (got %v); the query "+
			"filters nothing", ids)
	}

	// A body match carries a snippet; a TITLE-only match must not — the title is
	// already on screen and a snippet repeating it would claim a body match.
	for _, m := range got {
		switch m.ID {
		case byBody.ID:
			if m.Snippet == "" {
				t.Error("the body match carries no snippet, so the operator cannot see WHY it matched")
			} else if !strings.Contains(m.Snippet, term) {
				t.Errorf("the snippet %q does not contain the matched term", m.Snippet)
			}
		case byTitle.ID:
			if m.Snippet != "" {
				t.Errorf("a TITLE-only match carries snippet %q; it claims a body match that does "+
					"not exist", m.Snippet)
			}
		}
	}
}

// TestSearchSessionsNeverMatchesAToolResultBody IS THE EXCLUSION, AND IT IS THE
// MOST IMPORTANT TEST IN THIS FILE.
//
// 🔴 WHY tool_result IS EXCLUDED AND WHAT INCLUDING IT WOULD DO. Those rows are
// FILE CONTENTS AND COMMAND OUTPUT — an agent's `cat` of a config file, a build log,
// a diff. Two harms, and the second is the one that matters: every common word would
// match them, so a search would return noise; and the snippet is rendered on screen,
// so a query would paste raw file bytes into the operator's panel. tool_call args
// carry the same material (paths, command lines) and are excluded for the same
// reason.
//
// 🔴 HOW TO WATCH IT FAIL. Delete `AND cm.kind = 'text'` from the LATERAL in
// PGStore.SearchSessions and run this test: the tool_result thread appears in the
// results and the sub-test below reports the leaked snippet verbatim. Do that before
// trusting this test.
func TestSearchSessionsNeverMatchesAToolResultBody(t *testing.T) {
	s, pool, ctx := searchStore(t)
	a := searchAgent(t, s, pool, ctx, "toolresult")

	const term = "QUARTZHORIZON"
	// 🔴 THE FIXTURE IS REALISTIC, NOT A TOKEN. A tool_result's real shape is a file
	// or a command's output, which is what makes the leak bad enough to exclude — and
	// a one-word fixture would let a reader mistake this for a nit.
	toolOnly := searchThread(t, s, pool, ctx, a, "a thread whose only hit is tool output",
		ChatMessage{Role: "user", Kind: "text", Content: "read the deploy config for me"},
		ChatMessage{Role: "assistant", Kind: "tool_call", ToolID: "c1", ToolName: "Read",
			Content: `{"path":"/etc/` + term + `/values.yaml"}`},
		ChatMessage{Role: "assistant", Kind: "tool_result", ToolID: "c1", ToolName: "Read", ToolOK: true,
			Content: "image:\n  repository: registry.example/" + term + "\n  tag: 1.2.3\nreplicas: 2\n"},
	)
	// A CONTROL THREAD in the same agent whose kind='text' body DOES carry the term,
	// so a zero result set cannot be mistaken for "the query is broken".
	textHit := searchThread(t, s, pool, ctx, a, "a thread that legitimately matches",
		ChatMessage{Role: "user", Kind: "text", Content: "what happened with " + term + " yesterday?"})

	got, err := s.SearchSessions(ctx, a.ID, term, 10)
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}
	ids := matchIDs(got)

	// POSITIVE CONTROL FIRST. Without it a query that matched nothing at all would
	// pass the exclusion assertion below, and a permanently-empty search is
	// indistinguishable from a correctly-excluding one.
	if !containsID(ids, textHit.ID) {
		t.Fatalf("the control thread — whose kind='text' body contains %q — did not match (got %v). "+
			"The exclusion assertion below would then be about a query that finds NOTHING.", term, ids)
	}

	if containsID(ids, toolOnly.ID) {
		var leaked string
		for _, m := range got {
			if m.ID == toolOnly.ID {
				leaked = m.Snippet
			}
		}
		t.Errorf("a thread whose ONLY hit is a tool_call/tool_result row matched the search.\n"+
			"  snippet returned: %q\n"+
			"🔴 Those rows are file contents and command output. Including them makes every common "+
			"word match noise AND pastes raw file bytes onto the operator's screen as a snippet — "+
			"on the one surface whose content-sensitivity question is still open.", leaked)
	}
	for _, m := range got {
		if strings.Contains(m.Snippet, "repository:") || strings.Contains(m.Snippet, "values.yaml") {
			t.Errorf("a snippet carries tool output verbatim: %q", m.Snippet)
		}
	}
}

// TestSearchSessionsIsScopedToOneAgent — a caller scoped to one agent may see
// that agent's threads and nothing else.
func TestSearchSessionsIsScopedToOneAgent(t *testing.T) {
	s, pool, ctx := searchStore(t)
	mine := searchAgent(t, s, pool, ctx, "scope-a")
	theirs := searchAgent(t, s, pool, ctx, "scope-b")

	const term = "HORIZONBASALT"
	ours := searchThread(t, s, pool, ctx, mine, "ours",
		ChatMessage{Role: "user", Kind: "text", Content: "about " + term})
	other := searchThread(t, s, pool, ctx, theirs, "theirs",
		ChatMessage{Role: "user", Kind: "text", Content: "also about " + term})

	got, err := s.SearchSessions(ctx, mine.ID, term, 10)
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}
	ids := matchIDs(got)
	if !containsID(ids, ours.ID) {
		t.Fatalf("our own matching thread is absent (got %v), so the scoping claim below is vacuous", ids)
	}
	if containsID(ids, other.ID) {
		t.Errorf("ANOTHER AGENT's thread matched a search scoped to agent %d (got %v). An unscoped "+
			"search returns every agent's conversation with the operator.", mine.ID, ids)
	}
}

// TestSearchSessionsEscapesLIKEMetacharacters — a query of "%" must not match
// everything.
//
// 🔴 THIS IS A WRONG-ANSWER BUG, NOT A CRASH, WHICH IS WHY IT NEEDS A TEST. `%`
// and `_` are LIKE metacharacters: pasted straight into a pattern, "%" returns the
// whole table and "a_b" returns rows containing "axb" — search that looks like it
// works. Watch it fail by deleting likeEscape's two ReplaceAll calls.
func TestSearchSessionsEscapesLIKEMetacharacters(t *testing.T) {
	s, pool, ctx := searchStore(t)
	a := searchAgent(t, s, pool, ctx, "escapes")

	present := searchThread(t, s, pool, ctx, a, "one hundred % done",
		ChatMessage{Role: "user", Kind: "text", Content: "a body with no metacharacter"})
	absent := searchThread(t, s, pool, ctx, a, "no percent sign anywhere",
		ChatMessage{Role: "user", Kind: "text", Content: "also none here"})

	got, err := s.SearchSessions(ctx, a.ID, "%", 10)
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}
	ids := matchIDs(got)
	if !containsID(ids, present.ID) {
		t.Errorf("a LITERAL %% in a title did not match a query of %q (got %v)", "%", ids)
	}
	if containsID(ids, absent.ID) {
		t.Errorf("a query of %q matched a thread containing no %% at all (got %v) — the "+
			"metacharacter reached the pattern unescaped, so every query containing one returns "+
			"the whole table", "%", ids)
	}

	// The same for `_`, which is the SINGLE-character wildcard and therefore the
	// quieter of the two: it returns near-misses rather than everything.
	under := searchThread(t, s, pool, ctx, a, "snake_case matters",
		ChatMessage{Role: "user", Kind: "text", Content: "body"})
	near := searchThread(t, s, pool, ctx, a, "snakeXcase does not",
		ChatMessage{Role: "user", Kind: "text", Content: "body"})
	got, err = s.SearchSessions(ctx, a.ID, "snake_case", 10)
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}
	ids = matchIDs(got)
	if !containsID(ids, under.ID) {
		t.Errorf("a literal underscore query did not match (got %v)", ids)
	}
	if containsID(ids, near.ID) {
		t.Errorf("`snake_case` matched `snakeXcase` (got %v); the underscore is acting as a "+
			"wildcard", ids)
	}
}

// TestSearchSessionsBoundsWhatItReturns — the snippet is cut in SQL and the row
// count is capped, so neither can be the size of the transcript.
//
// 🔴 THE SNIPPET BOUND IS MEASURED AGAINST REALITY, NOT GUESSED. The longest
// kind='text' row on the deployment this was extracted from was 174,163 bytes. A
// reader that returned whole bodies and let CSS clamp them would put that on the
// wire for one search result.
func TestSearchSessionsBoundsWhatItReturns(t *testing.T) {
	s, pool, ctx := searchStore(t)
	a := searchAgent(t, s, pool, ctx, "bounds")

	const term = "BASALTNEEDLE"
	// A body far longer than the snippet, with the match in the MIDDLE so the cut
	// has to be positioned rather than simply truncated from the start.
	long := strings.Repeat("x", 4000) + " " + term + " " + strings.Repeat("y", 4000)
	searchThread(t, s, pool, ctx, a, "a long one",
		ChatMessage{Role: "user", Kind: "text", Content: long})

	got, err := s.SearchSessions(ctx, a.ID, term, 10)
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 match, got %d", len(got))
	}
	sn := got[0].Snippet
	if len(sn) > SearchSnippetLen {
		t.Errorf("the snippet is %d bytes; the bound is %d. A body of 174 KB — the real maximum on "+
			"the live database — would otherwise go over the wire for one search result.",
			len(sn), SearchSnippetLen)
	}
	if !strings.Contains(sn, term) {
		t.Errorf("the snippet %q does not contain the match, so the excerpt is cut in the wrong "+
			"place — the operator sees context for something else", sn)
	}

	// The row cap. A limit above SearchSessionsMaxLimit is clamped, and a limit of 0
	// is an empty page rather than "everything".
	for i := 0; i < 4; i++ {
		searchThread(t, s, pool, ctx, a, "more "+term)
	}
	two, err := s.SearchSessions(ctx, a.ID, term, 2)
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}
	if len(two) != 2 {
		t.Errorf("limit 2 returned %d rows", len(two))
	}
	huge, err := s.SearchSessions(ctx, a.ID, term, SearchSessionsMaxLimit*10)
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}
	if len(huge) > SearchSessionsMaxLimit {
		t.Errorf("a limit of %d returned %d rows; the clamp is %d",
			SearchSessionsMaxLimit*10, len(huge), SearchSessionsMaxLimit)
	}
	none, err := s.SearchSessions(ctx, a.ID, term, 0)
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("limit 0 returned %d rows; a non-positive limit is the CALLER's bug and yields an "+
			"empty page, not the whole table — the same direction ListChatMessagesPage chose, and "+
			"for the same reason: this reader is reachable from a query string", len(none))
	}
	blank, err := s.SearchSessions(ctx, a.ID, "   ", 10)
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}
	if len(blank) != 0 {
		t.Errorf("a whitespace-only query returned %d rows; it must return none so the caller can "+
			"render the plain recent list instead", len(blank))
	}
}

// TestSearchSessionsOrdersMostRecentlyActiveFirst — the list the operator reads is
// ordered the same way every other session list in this package is.
func TestSearchSessionsOrdersMostRecentlyActiveFirst(t *testing.T) {
	s, pool, ctx := searchStore(t)
	a := searchAgent(t, s, pool, ctx, "order")

	const term = "NEEDLEZEBRA"
	older := searchThread(t, s, pool, ctx, a, "older "+term)
	newer := searchThread(t, s, pool, ctx, a, "newer "+term)
	// Make the ordering unambiguous rather than relying on insertion order.
	if _, err := pool.Exec(ctx, `UPDATE chat_sessions SET updated_at = now() - interval '2 days' WHERE id=$1`, older.ID); err != nil {
		t.Fatalf("age the older thread: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE chat_sessions SET updated_at = now() WHERE id=$1`, newer.ID); err != nil {
		t.Fatalf("touch the newer thread: %v", err)
	}

	got, err := s.SearchSessions(ctx, a.ID, term, 10)
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 matches, got %d (%v)", len(got), matchIDs(got))
	}
	if got[0].ID != newer.ID {
		t.Errorf("results are ordered %v; the most-recently-active thread (%d) must come first",
			matchIDs(got), newer.ID)
	}
}
