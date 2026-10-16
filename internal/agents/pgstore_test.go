package agents

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestPGStoreModelRoundTrip verifies an agent's Model column persists and reads
// back via Create/Get/GetByName, and that an empty Model round-trips as "".
//
// It needs a real Postgres (the package's Store is Postgres-backed). It is
// skipped unless MUSTER_TEST_DATABASE_URL points at a disposable database
// (the e2e suite spins one up; locally `go test ./...` stays green by skipping).
func TestPGStoreModelRoundTrip(t *testing.T) {
	dsn := dbtest.DSN(t)

	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	store := NewPG(pool)
	name := "test-model-" + time.Now().Format("150405.000000")

	created, err := store.Create(ctx, Agent{
		Name:      name,
		Namespace: "devpod-" + name,
		Model:     "openrouter/deepseek/deepseek-v4-flash",
		Status:    StatusProvisioning,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer cleanupAgent(ctx, pool, created.ID)

	if created.Model != "openrouter/deepseek/deepseek-v4-flash" {
		t.Fatalf("Create returned Model %q, want the model we set", created.Model)
	}

	got, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Model != "openrouter/deepseek/deepseek-v4-flash" {
		t.Fatalf("Get Model = %q, want round-tripped model", got.Model)
	}

	byName, err := store.GetByName(ctx, name)
	if err != nil {
		t.Fatalf("get by name: %v", err)
	}
	if byName.Model != "openrouter/deepseek/deepseek-v4-flash" {
		t.Fatalf("GetByName Model = %q, want round-tripped model", byName.Model)
	}

	// Empty model defaults to "" (NOT NULL DEFAULT '').
	name2 := name + "-empty"
	created2, err := store.Create(ctx, Agent{
		Name:      name2,
		Namespace: "devpod-" + name2,
		Status:    StatusProvisioning,
	})
	if err != nil {
		t.Fatalf("create empty-model agent: %v", err)
	}
	defer cleanupAgent(ctx, pool, created2.ID)
	if created2.Model != "" {
		t.Fatalf("empty Model should round-trip as \"\", got %q", created2.Model)
	}
}

// TestPGStoreSetDisplayName verifies SetDisplayName updates the display name,
// leaves the immutable slug Name untouched, and falls back to the slug when the
// new name is blank. PG-gated (skipped without MUSTER_TEST_DATABASE_URL).
func TestPGStoreSetDisplayName(t *testing.T) {
	dsn := dbtest.DSN(t)

	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	store := NewPG(pool)
	name := "test-name-" + time.Now().Format("150405.000000")
	created, err := store.Create(ctx, Agent{
		Name: name, Namespace: "devpod-" + name, DisplayName: name, Status: StatusProvisioning,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer cleanupAgent(ctx, pool, created.ID)

	// Rename: display name changes, slug Name stays the same.
	got, err := store.SetDisplayName(ctx, created.ID, "Renamed Agent")
	if err != nil {
		t.Fatalf("set display name: %v", err)
	}
	if got.DisplayName != "Renamed Agent" {
		t.Fatalf("DisplayName = %q, want \"Renamed Agent\"", got.DisplayName)
	}
	if got.Name != name {
		t.Fatalf("slug Name changed to %q, want immutable %q", got.Name, name)
	}

	// Blank name falls back to the slug.
	got, err = store.SetDisplayName(ctx, created.ID, "")
	if err != nil {
		t.Fatalf("set blank display name: %v", err)
	}
	if got.DisplayName != name {
		t.Fatalf("blank name should fall back to slug %q, got %q", name, got.DisplayName)
	}
}

// cleanupAgent deletes an agent row; its sessions and chat messages follow via
// ON DELETE CASCADE.
//
// 🔴 It deliberately IGNORES the caller's context and uses a fresh one. Several
// callers invoke it from t.Cleanup, which fires AFTER the test function returns —
// i.e. after that function's own `defer cancel()` has already cancelled the test
// context. The DELETE then failed with "context canceled", the error is discarded
// here, and the rows survived into the next run against the same database. That is
// exactly why `go test ./internal/agents/` passed against a fresh Postgres and
// FAILED on a second run: TestPGStoreUnreadByAgent leaked 7 chat_messages rows and
// TestPGStoreRecentChatMessages' `delete future cutoff` assertion then counted 12
// instead of 5. Verified pre-existing at a39ed0c5. The pool itself is closed by a
// t.Cleanup registered before these, so it is still open when this runs.
func cleanupAgent(_ context.Context, pool querier, id int64) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 10*time.Second)
	defer cancel()
	_, _ = pool.Exec(ctx, `DELETE FROM agents WHERE id=$1`, id)
}

// createNote inserts a minimal note row and returns its generated id. agents.note_id
// carries a FK to notes(id) (ON DELETE SET NULL), so a note must exist before an
// agent can reference it — a made-up id violates agents_note_id_fkey. notes.id is
// GENERATED ALWAYS AS IDENTITY, so the id can't be chosen, only read back.
func createNote(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO notes (directory, body) VALUES ('', '') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("create note: %v", err)
	}
	// Fresh context for the same reason as cleanupAgent: this runs after the
	// caller's `defer cancel()`.
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM notes WHERE id=$1`, id)
	})
	return id
}

// TestPGStoreAgentsByNoteIDs verifies AgentsByNoteIDs returns the LATEST agent per
// note id (most recent when several point at one note), keys only requested ids,
// omits notes with no linked agent, and returns an empty map for empty ids.
// PG-gated (skipped without MUSTER_TEST_DATABASE_URL).
func TestPGStoreAgentsByNoteIDs(t *testing.T) {
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := NewPG(pool)
	tag := time.Now().Format("150405.000000")

	// Real notes: agents.note_id has a FK to notes(id), so referenced notes must
	// exist. note3 is a real note that no agent links to (→ absent from results).
	note1 := createNote(ctx, t, pool)
	note2 := createNote(ctx, t, pool)
	note3 := createNote(ctx, t, pool) // no agent → absent

	// Two agents for note1 (the second created is the "latest"); one for note2.
	a1, err := store.Create(ctx, Agent{Name: "abn-1-" + tag, Namespace: "devpod-abn-1-" + tag, NoteID: &note1, Status: StatusStopped})
	if err != nil {
		t.Fatalf("create a1: %v", err)
	}
	defer cleanupAgent(ctx, pool, a1.ID)
	a1b, err := store.Create(ctx, Agent{Name: "abn-1b-" + tag, Namespace: "devpod-abn-1b-" + tag, NoteID: &note1, Status: StatusRunning})
	if err != nil {
		t.Fatalf("create a1b: %v", err)
	}
	defer cleanupAgent(ctx, pool, a1b.ID)
	a2, err := store.Create(ctx, Agent{Name: "abn-2-" + tag, Namespace: "devpod-abn-2-" + tag, NoteID: &note2, Status: StatusProvisioning})
	if err != nil {
		t.Fatalf("create a2: %v", err)
	}
	defer cleanupAgent(ctx, pool, a2.ID)

	got, err := store.AgentsByNoteIDs(ctx, []int64{note1, note2, note3})
	if err != nil {
		t.Fatalf("AgentsByNoteIDs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2 (note3 has no agent)", len(got))
	}
	if got[note1].ID != a1b.ID {
		t.Errorf("note1 latest = agent %d, want the most-recent %d", got[note1].ID, a1b.ID)
	}
	if got[note1].Status != StatusRunning {
		t.Errorf("note1 status = %q, want the stored running", got[note1].Status)
	}
	if got[note2].ID != a2.ID {
		t.Errorf("note2 = agent %d, want %d", got[note2].ID, a2.ID)
	}
	if _, ok := got[note3]; ok {
		t.Errorf("note3 must be absent (no linked agent)")
	}

	// Empty ids → empty map, no query.
	empty, err := store.AgentsByNoteIDs(ctx, nil)
	if err != nil {
		t.Fatalf("AgentsByNoteIDs(nil): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("empty ids should return an empty map, got %d", len(empty))
	}
}

// TestReverseChatChronological is a pure unit test (no DB) for the ordering flip
// behind the capped reader.
func TestReverseChatChronological(t *testing.T) {
	// Newest-first input (as the LIMIT query returns) → chronological output.
	msgs := []ChatMessage{{ID: 5}, {ID: 4}, {ID: 3}}
	reverseChatChronological(msgs)
	if msgs[0].ID != 3 || msgs[1].ID != 4 || msgs[2].ID != 5 {
		t.Fatalf("reverse wrong: %v", []int64{msgs[0].ID, msgs[1].ID, msgs[2].ID})
	}
	// Empty and single-element slices are no-ops (no panic).
	reverseChatChronological(nil)
	one := []ChatMessage{{ID: 1}}
	reverseChatChronological(one)
	if one[0].ID != 1 {
		t.Fatalf("single-element reverse changed slice: %v", one)
	}
}

// TestPGStoreRecentChatMessages verifies the capped reader returns the most
// recent `limit` messages in chronological order, and that
// DeleteChatMessagesOlderThan removes only rows older than the cutoff. PG-gated.
func TestPGStoreRecentChatMessages(t *testing.T) {
	dsn := dbtest.DSN(t)

	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	store := NewPG(pool)
	name := "test-chat-" + time.Now().Format("150405.000000")
	ag, err := store.Create(ctx, Agent{Name: name, Namespace: "devpod-" + name, Status: StatusProvisioning})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	defer cleanupAgent(ctx, pool, ag.ID)

	sess, err := store.CreateSession(ctx, ag.ID, ag.Name)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Insert five messages in order, scoped to the session.
	for i := 1; i <= 5; i++ {
		if _, err := store.AddChatMessage(ctx, ChatMessage{AgentID: ag.ID, SessionID: sess.ID, Role: "user", Content: "m" + string(rune('0'+i))}); err != nil {
			t.Fatalf("add message %d: %v", i, err)
		}
	}

	// Cap at 3 → the three newest (m3,m4,m5) in chronological order.
	recent, err := store.ListRecentChatMessages(ctx, sess.ID, 3)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(recent) != 3 || recent[0].Content != "m3" || recent[2].Content != "m5" {
		t.Fatalf("capped recent wrong: %+v", recent)
	}

	// Limit larger than the count returns everything, still chronological.
	all, err := store.ListRecentChatMessages(ctx, sess.ID, 100)
	if err != nil {
		t.Fatalf("recent all: %v", err)
	}
	if len(all) != 5 || all[0].Content != "m1" || all[4].Content != "m5" {
		t.Fatalf("full recent wrong: %+v", all)
	}

	// A non-positive limit returns the full transcript (delegates to List).
	if full, err := store.ListRecentChatMessages(ctx, sess.ID, 0); err != nil || len(full) != 5 {
		t.Fatalf("limit 0 = %d msgs, err %v; want 5", len(full), err)
	}

	// DeleteChatMessagesOlderThan: a past cutoff deletes none; a future one all.
	if n, err := store.DeleteChatMessagesOlderThan(ctx, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("delete past cutoff = %d, err %v; want 0", n, err)
	}
	if n, err := store.DeleteChatMessagesOlderThan(ctx, time.Now().Add(time.Hour)); err != nil || n != 5 {
		t.Fatalf("delete future cutoff = %d, err %v; want 5", n, err)
	}
}

// TestPGStoreChatMessagesPage verifies the PAGED reader against a real database:
// the newest page, a cursor that walks backwards without gaps or duplicates, a
// cursor older than everything, session scoping, and the limit<=0 contract. PG-gated.
//
// 🔴 IT HAS TO BE PG-GATED AND IT CANNOT BE COVERED BY THE API PACKAGE'S FAKE. The
// thing under test is one SQL predicate — `($2 <= 0 OR id < $2)` combined with
// `ORDER BY id DESC LIMIT $3` and then reversed in Go — and internal/api's fake
// re-implements that rule in Go rather than running it. A fake and a query can agree
// on every fixture and disagree about the boundary; only this exercises the
// statement the server will actually send.
//
// ⚠ THE BOUNDARY IS THE POINT, SO THE CURSOR IS TESTED AT A PAGE EDGE. A cursor that
// used `<=` instead of `<` drops exactly one message per page, and a fixture whose
// page size divides the conversation evenly cannot tell that from a clean walk — 7
// messages at limit 3 is deliberately not a multiple.
func TestPGStoreChatMessagesPage(t *testing.T) {
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := NewPG(pool)
	name := "test-page-" + time.Now().Format("150405.000000")
	ag, err := store.Create(ctx, Agent{Name: name, Namespace: "devpod-" + name, Status: StatusProvisioning})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	defer cleanupAgent(ctx, pool, ag.ID)

	sess, err := store.CreateSession(ctx, ag.ID, ag.Name)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	// A SECOND session on the same agent. Without it, "the page is scoped to a
	// session" is unfalsifiable: a query missing its WHERE session_id would return
	// the identical rows.
	other, err := store.CreateSession(ctx, ag.ID, ag.Name)
	if err != nil {
		t.Fatalf("create second session: %v", err)
	}
	if _, err := store.AddChatMessage(ctx, ChatMessage{
		AgentID: ag.ID, SessionID: other.ID, Role: "user", Content: "OTHER-SESSION",
	}); err != nil {
		t.Fatalf("seed other session: %v", err)
	}

	const seeded = 7 // not a multiple of the page size below
	for i := 1; i <= seeded; i++ {
		if _, err := store.AddChatMessage(ctx, ChatMessage{
			AgentID: ag.ID, SessionID: sess.ID, Role: "user", Content: fmt.Sprintf("m%d", i),
		}); err != nil {
			t.Fatalf("add message %d: %v", i, err)
		}
	}

	// The newest page, chronological.
	page, err := store.ListChatMessagesPage(ctx, sess.ID, 0, 3)
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if len(page) != 3 || page[0].Content != "m5" || page[2].Content != "m7" {
		t.Fatalf("newest page = %v, want m5,m6,m7 in that order", contentsOf(page))
	}
	// beforeID == 0 must mean exactly what ListRecentChatMessages means, since the
	// latter is now one call to the former.
	recent, err := store.ListRecentChatMessages(ctx, sess.ID, 3)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if fmt.Sprint(contentsOf(recent)) != fmt.Sprint(contentsOf(page)) {
		t.Fatalf("ListRecentChatMessages returned %v and ListChatMessagesPage(before=0) returned "+
			"%v; the first delegates to the second, so a disagreement means the delegation is "+
			"not what it claims", contentsOf(recent), contentsOf(page))
	}

	// Walk backwards. 7 messages at 3 per page is 3+3+1, so the last page is short
	// and the boundary falls inside the conversation twice.
	var walked []string
	cursor := page[0].ID
	walked = append(contentsOf(page), walked...)
	for i := 0; i < seeded; i++ {
		p, err := store.ListChatMessagesPage(ctx, sess.ID, cursor, 3)
		if err != nil {
			t.Fatalf("page before %d: %v", cursor, err)
		}
		if len(p) == 0 {
			break
		}
		for _, m := range p {
			if m.ID >= cursor {
				t.Fatalf("a page before id %d contained id %d; the cursor is inclusive, so every "+
					"page boundary re-serves a message", cursor, m.ID)
			}
		}
		walked = append(contentsOf(p), walked...)
		cursor = p[0].ID
	}
	want := []string{"m1", "m2", "m3", "m4", "m5", "m6", "m7"}
	if fmt.Sprint(walked) != fmt.Sprint(want) {
		t.Fatalf("the backwards walk reconstructed %v, want %v. A `<=` cursor drops one message "+
			"per page and 7-at-3 is deliberately not an even split, so an even fixture would have "+
			"hidden it.", walked, want)
	}

	// A cursor older than every message is an empty page, not a wrap-around.
	if p, err := store.ListChatMessagesPage(ctx, sess.ID, 1, 3); err != nil || len(p) != 0 {
		t.Fatalf("page before the lowest possible id = %d rows, err %v; want 0", len(p), err)
	}
	// Session scoping: the other session's message must never appear.
	for _, c := range walked {
		if c == "OTHER-SESSION" {
			t.Fatal("a page of one session returned another session's message")
		}
	}
	if p, err := store.ListChatMessagesPage(ctx, other.ID, 0, 10); err != nil ||
		len(p) != 1 || p[0].Content != "OTHER-SESSION" {
		t.Fatalf("the other session's own page = %v, err %v; want exactly its one message — "+
			"without this, 'scoped correctly' could mean 'returns nothing at all'", contentsOf(p), err)
	}

	// 🔴 limit<=0 IS AN EMPTY PAGE HERE, NOT THE WHOLE TRANSCRIPT. Its sibling
	// ListRecentChatMessages deliberately means the opposite, because that one is
	// called by the page render with a constant while this one is reachable from a
	// query string — `?limit=0` must never be an unbounded table dump.
	for _, bad := range []int{0, -1} {
		if p, err := store.ListChatMessagesPage(ctx, sess.ID, 0, bad); err != nil || len(p) != 0 {
			t.Fatalf("limit=%d returned %d rows, err %v; want 0. The other reader treats this as "+
				"\"everything\", and inheriting that would make the bound optional.", bad, len(p), err)
		}
	}
}

// contentsOf projects message bodies, so a failure message names what came back
// rather than printing struct dumps.
func contentsOf(msgs []ChatMessage) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Content)
	}
	return out
}

// TestPGStoreChatMessageParts verifies the structured-transcript columns (migration
// 0015) round-trip: kind/tool_id/tool_name/tool_ok persist and read back, a legacy
// row (no Kind set) defaults to 'text', and List preserves order. PG-gated.
func TestPGStoreChatMessageParts(t *testing.T) {
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := NewPG(pool)
	name := "test-parts-" + time.Now().Format("150405.000000")
	ag, err := store.Create(ctx, Agent{Name: name, Namespace: "devpod-" + name, Status: StatusProvisioning})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	defer cleanupAgent(ctx, pool, ag.ID)
	sess, _ := store.CreateSession(ctx, ag.ID, ag.Name)

	// A structured assistant turn: text, tool_call, tool_result — plus a legacy
	// user message with no Kind (defaults to 'text').
	_, _ = store.AddChatMessage(ctx, ChatMessage{AgentID: ag.ID, SessionID: sess.ID, Role: "user", Content: "go"})
	_, _ = store.AddChatMessage(ctx, ChatMessage{AgentID: ag.ID, SessionID: sess.ID, Role: "assistant", Kind: "text", Content: "checking"})
	_, _ = store.AddChatMessage(ctx, ChatMessage{AgentID: ag.ID, SessionID: sess.ID, Role: "assistant", Kind: "tool_call", ToolID: "c1", ToolName: "read_file", Content: `{"p":"x"}`})
	_, _ = store.AddChatMessage(ctx, ChatMessage{AgentID: ag.ID, SessionID: sess.ID, Role: "assistant", Kind: "tool_result", ToolID: "c1", ToolName: "read_file", ToolOK: true, Content: "contents"})

	msgs, err := store.ListRecentChatMessages(ctx, sess.ID, 0)
	if err != nil || len(msgs) != 4 {
		t.Fatalf("list = %d msgs, err %v; want 4", len(msgs), err)
	}
	if msgs[0].Kind != "text" {
		t.Errorf("legacy user message should default kind='text', got %q", msgs[0].Kind)
	}
	tc := msgs[2]
	if tc.Kind != "tool_call" || tc.ToolID != "c1" || tc.ToolName != "read_file" || tc.Content != `{"p":"x"}` {
		t.Errorf("tool_call did not round-trip: %+v", tc)
	}
	tr := msgs[3]
	if tr.Kind != "tool_result" || !tr.ToolOK || tr.ToolName != "read_file" || tr.Content != "contents" {
		t.Errorf("tool_result did not round-trip: %+v", tr)
	}
}

// TestPGStoreUnreadByAgent verifies UnreadByAgent counts only assistant messages
// created after a session's read_at (user messages + read sessions excluded),
// sums per agent, picks the most-recently-active session as the link target, and
// that MarkSessionRead clears the unread. PG-gated.
func TestPGStoreUnreadByAgent(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	store := requirePGStore(t, ctx)

	stamp := time.Now().Format("150405.000000")
	mk := func(slug string) Agent {
		name := slug + "-" + stamp
		ag, err := store.Create(ctx, Agent{Name: name, Namespace: "devpod-" + name, DisplayName: slug, Status: StatusProvisioning})
		if err != nil {
			t.Fatalf("create agent %s: %v", slug, err)
		}
		t.Cleanup(func() { cleanupAgent(ctx, store.pool, ag.ID) })
		return ag
	}

	// Agent A: one fresh session with an unread assistant reply (the latest), plus
	// an older session that's been read — only the unread one counts, and the
	// latest session is the link target.
	a := mk("alpha")
	aOld, _ := store.LatestOrCreateSession(ctx, a.ID, a.Name) // legacy/oldest
	aNew, _ := store.CreateSession(ctx, a.ID, a.Name)         // becomes latest on append
	// Old session: an assistant reply, then marked read → not unread.
	if _, err := store.AddChatMessage(ctx, ChatMessage{AgentID: a.ID, SessionID: aOld.ID, Role: "assistant", Content: "old reply"}); err != nil {
		t.Fatalf("add old reply: %v", err)
	}
	if err := store.MarkSessionRead(ctx, aOld.ID); err != nil {
		t.Fatalf("mark old read: %v", err)
	}
	// New session: a user turn (never counted) + two assistant replies (unread).
	store.AddChatMessage(ctx, ChatMessage{AgentID: a.ID, SessionID: aNew.ID, Role: "user", Content: "go"})
	store.AddChatMessage(ctx, ChatMessage{AgentID: a.ID, SessionID: aNew.ID, Role: "assistant", Content: "reply 1"})
	store.AddChatMessage(ctx, ChatMessage{AgentID: a.ID, SessionID: aNew.ID, Role: "assistant", Content: "reply 2"})

	// Agent B: a single unread assistant reply.
	b := mk("bravo")
	bSess, _ := store.LatestOrCreateSession(ctx, b.ID, b.Name)
	store.AddChatMessage(ctx, ChatMessage{AgentID: b.ID, SessionID: bSess.ID, Role: "assistant", Content: "hi"})

	// Agent C: only a user message + a fully-read assistant reply → no unread.
	c := mk("charlie")
	cSess, _ := store.LatestOrCreateSession(ctx, c.ID, c.Name)
	store.AddChatMessage(ctx, ChatMessage{AgentID: c.ID, SessionID: cSess.ID, Role: "user", Content: "hello"})
	store.AddChatMessage(ctx, ChatMessage{AgentID: c.ID, SessionID: cSess.ID, Role: "assistant", Content: "answered"})
	if err := store.MarkSessionRead(ctx, cSess.ID); err != nil {
		t.Fatalf("mark c read: %v", err)
	}

	unread, err := store.UnreadByAgent(ctx)
	if err != nil {
		t.Fatalf("unread by agent: %v", err)
	}
	got := map[int64]AgentUnread{}
	for _, u := range unread {
		got[u.AgentID] = u
	}
	if _, ok := got[c.ID]; ok {
		t.Fatalf("agent C has only read/user messages; should not appear: %+v", got[c.ID])
	}
	ua, ok := got[a.ID]
	if !ok {
		t.Fatalf("agent A should have unread replies")
	}
	if ua.Count != 2 {
		t.Fatalf("agent A unread count = %d, want 2 (assistant-after-read only)", ua.Count)
	}
	if ua.SessionID != aNew.ID {
		t.Fatalf("agent A link target = %d, want the most-recently-active session %d", ua.SessionID, aNew.ID)
	}
	if ua.DisplayName != "alpha" {
		t.Fatalf("agent A display name = %q, want alpha", ua.DisplayName)
	}
	if ub, ok := got[b.ID]; !ok || ub.Count != 1 || ub.SessionID != bSess.ID {
		t.Fatalf("agent B unread = %+v, want count 1 on session %d", ub, bSess.ID)
	}

	// MarkSessionRead clears A's unread (both replies are in the new session).
	if err := store.MarkSessionRead(ctx, aNew.ID); err != nil {
		t.Fatalf("mark A new read: %v", err)
	}
	after, err := store.UnreadByAgent(ctx)
	if err != nil {
		t.Fatalf("unread after read: %v", err)
	}
	for _, u := range after {
		if u.AgentID == a.ID {
			t.Fatalf("agent A should have no unread after MarkSessionRead, got %+v", u)
		}
	}
}

// requirePGStore connects, migrates, and returns a fresh PGStore — or skips the
// test when MUSTER_TEST_DATABASE_URL isn't set (so plain `go test` stays green).
func requirePGStore(t *testing.T, ctx context.Context) *PGStore {
	t.Helper()
	dsn := dbtest.DSN(t)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewPG(pool)
}

// TestPGStoreSessions verifies CreateSession / ListSessions order (most-recently-
// active first), session-scoped message isolation, title-from-first-user-message,
// the updated_at bump on append, and LatestOrCreateSession's legacy-key behaviour.
// PG-gated (skipped without MUSTER_TEST_DATABASE_URL).
func TestPGStoreSessions(t *testing.T) {
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	store := requirePGStore(t, ctx)

	name := "test-sess-" + time.Now().Format("150405.000000")
	ag, err := store.Create(ctx, Agent{Name: name, Namespace: "devpod-" + name, Status: StatusProvisioning})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	defer cleanupAgent(ctx, store.pool, ag.ID)

	// First LatestOrCreateSession on a sessionless agent reuses the LEGACY key so
	// its pre-sessions gateway context carries over.
	legacy, err := store.LatestOrCreateSession(ctx, ag.ID, ag.Name)
	if err != nil {
		t.Fatalf("latest-or-create (first): %v", err)
	}
	if legacy.SessionKey != "agent:"+name+":webchat" {
		t.Fatalf("first session should use the legacy key, got %q", legacy.SessionKey)
	}
	// A second call now takes the latest branch (no new row, no collision).
	again, err := store.LatestOrCreateSession(ctx, ag.ID, ag.Name)
	if err != nil || again.ID != legacy.ID {
		t.Fatalf("second latest-or-create should return the same session: id=%d err=%v", again.ID, err)
	}

	// A brand-new session gets a unique (non-legacy) key.
	s2, err := store.CreateSession(ctx, ag.ID, ag.Name)
	if err != nil {
		t.Fatalf("create session 2: %v", err)
	}
	if s2.SessionKey == legacy.SessionKey {
		t.Fatalf("new session should get a fresh key distinct from the legacy one")
	}
	if s2.Title != "" {
		t.Fatalf("new session title should start empty, got %q", s2.Title)
	}

	// Messages are session-scoped: each session sees only its own.
	if _, err := store.AddChatMessage(ctx, ChatMessage{AgentID: ag.ID, SessionID: legacy.ID, Role: "user", Content: "in legacy session"}); err != nil {
		t.Fatalf("add msg legacy: %v", err)
	}
	longFirst := "Investigate the failing deploy and report back with the root cause please thanks"
	if _, err := store.AddChatMessage(ctx, ChatMessage{AgentID: ag.ID, SessionID: s2.ID, Role: "user", Content: longFirst + "\nsecond line"}); err != nil {
		t.Fatalf("add msg s2: %v", err)
	}
	if msgs, _ := store.ListChatMessages(ctx, s2.ID); len(msgs) != 1 || msgs[0].Content != longFirst+"\nsecond line" {
		t.Fatalf("session 2 should see only its own message, got %+v", msgs)
	}

	// Title is derived from the first user message: first line, capped to ~48 runes.
	got, err := store.GetSession(ctx, s2.ID)
	if err != nil {
		t.Fatalf("get session 2: %v", err)
	}
	wantTitle := deriveSessionTitle(longFirst + "\nsecond line")
	if got.Title != wantTitle {
		t.Fatalf("session 2 title = %q, want derived %q", got.Title, wantTitle)
	}
	if []rune(got.Title)[len([]rune(got.Title))-1] != '…' {
		t.Fatalf("long first line should be truncated with an ellipsis, got %q", got.Title)
	}

	// A second user message does NOT overwrite an already-set title.
	if _, err := store.AddChatMessage(ctx, ChatMessage{AgentID: ag.ID, SessionID: s2.ID, Role: "user", Content: "a later message"}); err != nil {
		t.Fatalf("add msg s2 again: %v", err)
	}
	if again, _ := store.GetSession(ctx, s2.ID); again.Title != wantTitle {
		t.Fatalf("title should be sticky after first user message, got %q", again.Title)
	}

	// ListSessions orders most-recently-active first: s2 was appended to last, so
	// it sorts ahead of the legacy session.
	list, err := store.ListSessions(ctx, ag.ID)
	if err != nil || len(list) != 2 {
		t.Fatalf("list sessions = %d, err %v; want 2", len(list), err)
	}
	if list[0].ID != s2.ID {
		t.Fatalf("most-recently-active session should sort first; got %d want %d", list[0].ID, s2.ID)
	}

	// Bump the legacy session by appending to it; it should now sort first.
	if _, err := store.AddChatMessage(ctx, ChatMessage{AgentID: ag.ID, SessionID: legacy.ID, Role: "assistant", Content: "reply"}); err != nil {
		t.Fatalf("bump legacy: %v", err)
	}
	if list, _ := store.ListSessions(ctx, ag.ID); list[0].ID != legacy.ID {
		t.Fatalf("appending to legacy session should bump it to the front, got first id %d", list[0].ID)
	}
}

// TestPGStoreLastMessageByAgentIDs exercises the SQL behind the machine API's
// liveness field: the newest chat_messages.created_at per requested agent id.
//
// It pins the three properties the API contract rests on:
//   - the MAX is returned, not the first or last row scanned (the fixture inserts
//     an out-of-order backdated message after the newest one);
//   - an agent with NO messages is ABSENT from the map, not mapped to the zero
//     time (the API turns absence into `lastMessageAt: null` and falls back to
//     updated_at, which it could not do if absence and epoch were the same);
//   - only requested ids are keyed, and an empty ids slice is an empty map.
//
// PG-gated (skipped without MUSTER_TEST_DATABASE_URL).
func TestPGStoreLastMessageByAgentIDs(t *testing.T) {
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 30*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := NewPG(pool)
	tag := time.Now().Format("150405.000000")

	// 🔴 Cleanup is DEFERRED, not t.Cleanup'd. t.Cleanup callbacks run AFTER the
	// test function returns — i.e. after `defer pool.Close()` above — so they would
	// execute against a closed pool and silently do nothing (cleanupAgent ignores
	// its error). This test inserts a deliberately BACKDATED message, so leaking it
	// poisons TestPGStoreRecentChatMessages' retention assertion on the NEXT run
	// against the same database. Deferring here means the deletes run before the
	// pool closes, and the ON DELETE CASCADE on chat_messages.agent_id takes the
	// transcript rows with the agent.
	var created []int64
	defer func() {
		for _, id := range created {
			cleanupAgent(ctx, pool, id)
		}
	}()
	mk := func(suffix string) Agent {
		a, err := store.Create(ctx, Agent{
			Name: "lmb-" + suffix + "-" + tag, Namespace: "devpod-lmb-" + suffix + "-" + tag,
			Status: StatusRunning,
		})
		if err != nil {
			t.Fatalf("create agent %s: %v", suffix, err)
		}
		created = append(created, a.ID)
		return a
	}
	chatty := mk("chatty")   // several messages
	silent := mk("silent")   // provisioned, never produced a turn
	other := mk("other")     // has a message but is NOT requested
	unasked := mk("unasked") // exists but is not in the ids slice

	sess := func(a Agent) int64 {
		s, err := store.CreateSession(ctx, a.ID, a.Name)
		if err != nil {
			t.Fatalf("create session for %s: %v", a.Name, err)
		}
		return s.ID
	}
	chattySess, otherSess := sess(chatty), sess(other)

	for i := 1; i <= 3; i++ {
		if _, err := store.AddChatMessage(ctx, ChatMessage{
			AgentID: chatty.ID, SessionID: chattySess, Role: "assistant", Content: "m",
		}); err != nil {
			t.Fatalf("add message %d: %v", i, err)
		}
	}
	if _, err := store.AddChatMessage(ctx, ChatMessage{
		AgentID: other.ID, SessionID: otherSess, Role: "assistant", Content: "elsewhere",
	}); err != nil {
		t.Fatalf("add other message: %v", err)
	}

	// The newest row so far, read back directly, is the expected answer.
	var newest time.Time
	if err := pool.QueryRow(ctx,
		`SELECT max(created_at) FROM chat_messages WHERE agent_id=$1`, chatty.ID).Scan(&newest); err != nil {
		t.Fatalf("read newest: %v", err)
	}
	// Now insert a BACKDATED row. A query that took the last row scanned, or the
	// highest id, would return this one instead of the true max.
	if _, err := pool.Exec(ctx,
		`INSERT INTO chat_messages (agent_id, session_id, role, content, created_at)
		 VALUES ($1, $2, 'assistant', 'backdated', $3)`,
		chatty.ID, chattySess, newest.Add(-2*time.Hour)); err != nil {
		t.Fatalf("insert backdated: %v", err)
	}

	got, err := store.LastMessageByAgentIDs(ctx, []int64{chatty.ID, silent.ID, other.ID})
	if err != nil {
		t.Fatalf("LastMessageByAgentIDs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries (%v), want 2 (chatty + other; silent has no messages)", len(got), got)
	}
	if !got[chatty.ID].Equal(newest) {
		t.Errorf("chatty = %s, want the MAX %s (a backdated row must not win)",
			got[chatty.ID].UTC(), newest.UTC())
	}
	if _, present := got[silent.ID]; present {
		t.Errorf("an agent with no messages must be ABSENT from the map, got %v", got[silent.ID])
	}
	if _, present := got[unasked.ID]; present {
		t.Errorf("an unrequested agent id leaked into the result")
	}
	if _, present := got[other.ID]; !present {
		t.Errorf("requested agent %d with a message is missing", other.ID)
	}

	empty, err := store.LastMessageByAgentIDs(ctx, nil)
	if err != nil {
		t.Fatalf("LastMessageByAgentIDs(nil): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("empty ids returned %d entries, want 0", len(empty))
	}
}
