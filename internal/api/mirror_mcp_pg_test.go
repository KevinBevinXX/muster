package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notes"
)

// THE MIRRORED SESSIONS' MCP DOOR (POST /mirror/mcp) over a real Postgres:
// a host's token acts as its own sessions only, with the all-agents tools only.

// fakeMirrorTokens resolves invented mirror tokens to (owner, host).
type fakeMirrorTokens map[string]mirror.Token

func (f fakeMirrorTokens) Authenticate(_ context.Context, tok string) (mirror.Token, error) {
	if t, ok := f[tok]; ok {
		return t, nil
	}
	return mirror.Token{}, mirror.ErrBadToken
}

// mirrorTok is a token shaped as mirror.NewToken mints one, ending in c.
func mirrorTok(c byte) string { return "mmt_" + strings.Repeat("A", 42) + string(c) }

const (
	secondSID = "1c3d4e5f-6071-4829-9bac-1d2e3f4a5b6c"
	// splitSID runs in the SECOND pane of ws's window @3 (a split).
	splitSID = "2d4e5f60-7182-4930-8cbd-2e3f4a5b6c7d"
)

// mirrorFixture: user 1's host "ws" (the fake hub's) lists two Claude windows,
// @1 (focusSID, waiting) and @3 (secondSID, with splitSID in its other pane). Tokens: ws's, user 1's OTHER
// host's ("laptop", which lists nothing), and the member's token for a host
// also named "ws".
func newMirrorMCPFixture(t *testing.T) (*attnFixture, string, string, string) {
	t.Helper()
	f := newAttnFixture(t)
	f.mirror.mu.Lock()
	f.mirror.windows = append(attnWindows(mirror.StatusWaiting),
		mirror.Window{Session: "side", ID: "@3", Index: 0, Active: true, Claude: true, ClaudeSession: secondSID, Status: mirror.StatusBusy, StatusSince: attnSince, Activity: 1_700_000_000,
			OtherClaude: []string{splitSID}})
	f.mirror.mu.Unlock()
	ws, laptop, members := mirrorTok('a'), mirrorTok('b'), mirrorTok('c')
	f.tokens[ws] = mirror.Token{ID: 1, OwnerID: 1, Host: "ws"}
	f.tokens[laptop] = mirror.Token{ID: 2, OwnerID: 1, Host: "laptop"}
	f.tokens[members] = mirror.Token{ID: 3, OwnerID: f.member, Host: "ws"}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM attention_asks WHERE owner_id = 1 AND mirror_host IS NOT NULL`)
	})
	return f, ws, laptop, members
}

// mirrorRPC posts one JSON-RPC body to /mirror/mcp with hdr.
func (f *attnFixture) mirrorRPC(t *testing.T, hdr map[string]string, body string) (int, *mcpReply, string) {
	t.Helper()
	h := map[string]string{"Content-Type": "application/json"}
	for k, v := range hdr {
		h[k] = v
	}
	w := f.call(t, http.MethodPost, "/mirror/mcp", strings.NewReader(body), h)
	if w.Code != http.StatusOK {
		return w.Code, nil, w.Body.String()
	}
	var r mcpReply
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("not JSON-RPC: %s", w.Body)
	}
	return w.Code, &r, w.Body.String()
}

func mirrorAs(token, sid string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token, mirrorSessionHeader: sid}
}

func mcpToolCall(name string, args any) string {
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	return string(raw)
}

// TestAMirroredSessionGetsTheAllAgentsToolsAndNothingElse: tools/list is
// exactly the attention and task tools and the operator's own attention_answer,
// connector decision and reply-draft decision tools; a chief tool or an agent tool called by name is
// refused as unknown.
func TestAMirroredSessionGetsTheAllAgentsToolsAndNothingElse(t *testing.T) {
	f, ws, _, _ := newMirrorMCPFixture(t)
	code, r, body := f.mirrorRPC(t, mirrorAs(ws, focusSID), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if code != 200 || r.Error != nil {
		t.Fatalf("%d %s", code, body)
	}
	var res struct {
		Tools []struct{ Name string } `json:"tools"`
	}
	_ = json.Unmarshal(r.Result, &res)
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	want := "attention_ask attention_wait attention_cancel tasks_list task_get task_comment task_set_status attention_answer agent_destroy connector_requests_list connector_request_decide plugin_drafts_list plugin_draft_decide"
	if strings.Join(names, " ") != want {
		t.Fatalf("tools = %v, want %s", names, want)
	}
	for _, n := range []string{attentionListTool, "attention_get", sessionTranscript, "fleet_list", "connector_request", "task_create_gated"} {
		_, r, _ := f.mirrorRPC(t, mirrorAs(ws, focusSID), mcpToolCall(n, map[string]any{"id": 1, "answer": "x", "subject": "agent:1"}))
		if r == nil || r.Error == nil || r.Error.Code != -32602 {
			t.Fatalf("%s by a mirrored session: %+v", n, r)
		}
	}
	_, r, _ = f.mirrorRPC(t, mirrorAs(ws, focusSID), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if !strings.Contains(string(r.Result), "call attention_ask") {
		t.Fatalf("initialize: %s", r.Result)
	}
}

// TestAMirroredSessionAsksAndItsWaitReturnsTheAnswer: the ask is the
// session's (its host and Claude session id, its window's title), it is on the
// view, and the operator's answer reaches the session's wait.
func TestAMirroredSessionAsksAndItsWaitReturnsTheAnswer(t *testing.T) {
	f, ws, _, _ := newMirrorMCPFixture(t)
	_, r, body := f.mirrorRPC(t, mirrorAs(ws, focusSID), mcpToolCall(attentionAskTool, map[string]any{"question": "Keep the old schema?", "options": []string{"keep", "drop"}}))
	if r == nil {
		t.Fatal(body)
	}
	text, _ := toolText(t, r)
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.ID == 0 {
		t.Fatalf("ask: %s", text)
	}
	a, err := f.attn.Get(asUser(1), out.ID)
	if err != nil || a.Asker.MirrorHost != "ws" || a.Asker.MirrorSession != focusSID || a.Asker.AgentID != 0 || !strings.HasSuffix(a.Asker.Label, " @ ws") {
		t.Fatalf("%v %+v", err, a)
	}
	if !strings.Contains(f.page(t, "/attention"), "Keep the old schema?") {
		t.Fatal("the session's ask is not on the view")
	}
	rd, hdr := form(url.Values{"option": {"drop"}})
	if w := f.call(t, http.MethodPost, "/attention/asks/"+strconv.FormatInt(out.ID, 10)+"/answer", rd, merge(f.operator(), hdr)); w.Code != http.StatusSeeOther {
		t.Fatalf("answer: %d", w.Code)
	}
	_, r, _ = f.mirrorRPC(t, mirrorAs(ws, focusSID), mcpToolCall(attentionWaitTool, map[string]any{"id": out.ID, "wait_seconds": 5}))
	if text, _ := toolText(t, r); !strings.Contains(text, `"answer":"drop"`) {
		t.Fatalf("wait: %s", text)
	}
	// Another session on the SAME host is another asker: no such ask.
	_, r, _ = f.mirrorRPC(t, mirrorAs(ws, secondSID), mcpToolCall(attentionWaitTool, map[string]any{"id": out.ID, "wait_seconds": 0}))
	if text, isErr := toolText(t, r); !isErr || !strings.Contains(text, "no such ask") {
		t.Fatalf("another session's get: %s", text)
	}
}

// TestAHostActsOnlyAsItsOwnSessions: a CALL naming a session the token's host
// does not list — another host's, a made-up one, or none — is refused as a
// tool error before the tool runs, and so is another owner's token naming the
// same host name; nothing is asked. initialize and tools/list need no session
// (Claude Code connects before its session can be listed). Credentials that
// are not a mirror token are 401 for everything.
func TestAHostActsOnlyAsItsOwnSessions(t *testing.T) {
	f, ws, laptop, members := newMirrorMCPFixture(t)
	prev := mirrorBindWait
	mirrorBindWait = 100 * time.Millisecond
	t.Cleanup(func() { mirrorBindWait = prev })
	ask := mcpToolCall(attentionAskTool, map[string]any{"question": "smuggled?"})
	for name, hdr := range map[string]map[string]string{
		"another host's token, ws's session": mirrorAs(laptop, focusSID),
		"a session no window shows":          mirrorAs(ws, "0f0f0f0f-0000-4000-8000-000000000000"),
		"no session header":                  mirrorAs(ws, ""),
		"a malformed session":                mirrorAs(ws, "../etc"),
		"another owner's token, same name":   mirrorAs(members, focusSID),
	} {
		code, r, body := f.mirrorRPC(t, hdr, ask)
		if code != http.StatusOK || r == nil {
			t.Errorf("%s: %d %s", name, code, body)
			continue
		}
		if text, isErr := toolText(t, r); !isErr || !strings.Contains(text, "lists no window running Claude session") {
			t.Errorf("%s: %s", name, text)
		}
		for _, m := range []string{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`} {
			if code, r, body := f.mirrorRPC(t, hdr, m); code != http.StatusOK || r == nil || r.Error != nil {
				t.Errorf("%s: %s: %d %s", name, m, code, body)
			}
		}
	}
	for name, hdr := range map[string]map[string]string{
		"no credential":           {mirrorSessionHeader: focusSID},
		"an unknown mirror token": mirrorAs(mirrorTok('z'), focusSID),
		"the hook token":          mirrorAs(connTestHookToken, focusSID),
		"an agent's token":        mirrorAs(f.opTok, focusSID),
	} {
		for _, m := range []string{ask, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`} {
			if code, _, body := f.mirrorRPC(t, hdr, m); code != http.StatusUnauthorized {
				t.Errorf("%s: %d %s, want 401", name, code, body)
			}
		}
	}
	var n int
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM attention_asks WHERE question = 'smuggled?'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d smuggled asks (%v)", n, err)
	}
	// CONTROL: the same ask with ws's own token and its own session is filed.
	code, r, body := f.mirrorRPC(t, mirrorAs(ws, secondSID), ask)
	if code != 200 || r == nil || r.Error != nil {
		t.Fatalf("control: %d %s", code, body)
	}
	if text, isErr := toolText(t, r); isErr || !strings.Contains(text, `"status":"pending"`) {
		t.Fatalf("control: %s", text)
	}
	// A browser page cannot drive it.
	h := mirrorAs(ws, focusSID)
	h["Origin"] = "http://evil.example"
	if code, _, _ := f.mirrorRPC(t, h, ask); code != http.StatusForbidden {
		t.Fatalf("cross-origin: %d", code)
	}
}

// TestASessionListedJustAfterItsCallArrivesIsBound is the startup race on
// muster's side: the host pushes its listing right before it forwards a call,
// so the call can arrive first. A call naming a session the host does not list
// YET waits (mirrorBindWait) and is bound once the listing shows it.
func TestASessionListedJustAfterItsCallArrivesIsBound(t *testing.T) {
	f, ws, _, _ := newMirrorMCPFixture(t)
	const fresh = "4f607182-93a4-4b52-8edf-4a5b6c7d8e9f"
	if code, r, body := f.mirrorRPC(t, mirrorAs(ws, fresh), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`); code != 200 || r == nil || r.Error != nil {
		t.Fatalf("initialize from a session no window shows yet: %d %s", code, body)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		f.mirror.mu.Lock()
		f.mirror.windows = append(f.mirror.windows, mirror.Window{Session: "fresh", ID: "@9", Index: 0, Active: true, Claude: true, ClaudeSession: fresh,
			Status: mirror.StatusIdle, StatusSince: attnSince, Activity: 1_700_000_000})
		f.mirror.mu.Unlock()
	}()
	code, r, body := f.mirrorRPC(t, mirrorAs(ws, fresh), mcpToolCall(attentionAskTool, map[string]any{"question": "Listed late?"}))
	if code != 200 || r == nil {
		t.Fatalf("%d %s", code, body)
	}
	text, isErr := toolText(t, r)
	if isErr || !strings.Contains(text, `"status":"pending"`) {
		t.Fatalf("the call was not bound once the listing showed the session: %s", text)
	}
}

// TestASessionInANonActivePaneIsItsHosts: a Claude session running in a
// window's OTHER pane (a split) is recognised as the host's own — its ask is
// filed under that window — and one no pane runs is still refused.
func TestASessionInANonActivePaneIsItsHosts(t *testing.T) {
	f, ws, _, _ := newMirrorMCPFixture(t)
	code, r, body := f.mirrorRPC(t, mirrorAs(ws, splitSID), mcpToolCall(attentionAskTool, map[string]any{"question": "From the split?"}))
	if code != 200 || r == nil || r.Error != nil {
		t.Fatalf("%d %s", code, body)
	}
	text, _ := toolText(t, r)
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.ID == 0 {
		t.Fatalf("ask: %s", text)
	}
	a, err := f.attn.Get(asUser(1), out.ID)
	if err != nil || a.Asker.MirrorSession != splitSID || !strings.HasPrefix(a.Asker.Label, "side:") {
		t.Fatalf("%v %+v", err, a)
	}
}

// TestAMirroredSessionWorksOnlyTheTasksItCreated: the task tools act on the
// tasks THIS session created (its task_sessions link of role "created") and
// nothing else — the owner's other task, a task another session created and
// another owner's task each answer exactly what a missing task answers; no
// move into or out of complete.
func TestAMirroredSessionWorksOnlyTheTasksItCreated(t *testing.T) {
	f, ws, _, _ := newMirrorMCPFixture(t)
	mk := func(uid int64, title string) notes.Note {
		n, err := f.notes.Create(asUser(uid), notes.Note{Title: title, Body: title + " body", Status: notes.StatusOpen})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM notes WHERE id = $1`, n.ID)
		})
		return n
	}
	link := func(uid int64, n notes.Note, sid, role string) {
		if _, _, err := f.notes.LinkSession(asUser(uid), n.ID, notes.SessionLink{SessionID: sid, Role: role}, false); err != nil {
			t.Fatal(err)
		}
	}
	created := mk(1, "mirror-mcp created")
	link(1, created, focusSID, notes.RoleCreated)
	worked := mk(1, "mirror-mcp worked") // the session only touched it
	link(1, worked, focusSID, notes.RoleWorked)
	other := mk(1, "mirror-mcp other session's")
	link(1, other, secondSID, notes.RoleCreated)
	theirs := mk(f.member, "mirror-mcp theirs")
	call := func(name string, args map[string]any) string {
		_, r, body := f.mirrorRPC(t, mirrorAs(ws, focusSID), mcpToolCall(name, args))
		if r == nil {
			t.Fatalf("%s: %s", name, body)
		}
		text, _ := toolText(t, r)
		return text
	}
	var list struct{ Tasks []struct{ ID int64 } }
	if err := json.Unmarshal([]byte(call(tasksListTool, map[string]any{})), &list); err != nil || len(list.Tasks) != 1 || list.Tasks[0].ID != created.ID {
		t.Fatalf("list: %+v %v", list, err)
	}
	if text := call(taskCommentTool, map[string]any{"id": created.ID, "body": "from the session"}); !strings.Contains(text, `"body":"from the session"`) || !strings.Contains(text, " @ ws") {
		t.Fatalf("comment: %s", text)
	}
	for _, n := range []int64{worked.ID, other.ID, theirs.ID, 9_000_000_321} {
		for _, name := range []string{taskGetTool, taskCommentTool, taskSetStatusTool} {
			if text := call(name, map[string]any{"id": n, "body": "x", "status": "in_progress"}); text != `{"error":"no such task: `+strconv.FormatInt(n, 10)+`"}` {
				t.Fatalf("%s on %d: %s", name, n, text)
			}
		}
	}
	if text := call(taskSetStatusTool, map[string]any{"id": created.ID, "status": "complete"}); !strings.Contains(text, "may not set") {
		t.Fatalf("complete: %s", text)
	}
	if _, err := f.notes.SetStatus(asUser(1), created.ID, notes.StatusComplete); err != nil {
		t.Fatal(err)
	}
	if text := call(taskSetStatusTool, map[string]any{"id": created.ID, "status": "in_progress"}); !strings.Contains(text, "may not change a completed task") {
		t.Fatalf("out of complete: %s", text)
	}
}

// TestFailedMirrorTokenChecksAreLimitedAndARevokedTokenIsRefused: past the
// failure limit a bad token from an address is answered 429 — another token
// from the same address (a different bad one, or a valid one) is not — the
// bad token is checked again once the window has passed, and a token revoked
// between two calls is refused at the next one.
func TestFailedMirrorTokenChecksAreLimitedAndARevokedTokenIsRefused(t *testing.T) {
	f, ws, _, _ := newMirrorMCPFixture(t)
	clock := time.Now()
	f.srv.now = func() time.Time { return clock }
	list := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	for i := 0; i < mirrorMCPFailLimit; i++ {
		if code, _, _ := f.mirrorRPC(t, mirrorAs(mirrorTok('z'), focusSID), list); code != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d", i+1, code)
		}
	}
	if code, _, _ := f.mirrorRPC(t, mirrorAs(mirrorTok('z'), focusSID), list); code != http.StatusTooManyRequests {
		t.Fatalf("the bad token past the limit: %d", code)
	}
	if code, _, _ := f.mirrorRPC(t, mirrorAs("junk", focusSID), list); code != http.StatusUnauthorized {
		t.Fatalf("another bad token from the same address: %d, want its own 401", code)
	}
	if code, _, body := f.mirrorRPC(t, mirrorAs(ws, focusSID), list); code != http.StatusOK {
		t.Fatalf("a valid token from the same address: %d %s", code, body)
	}
	clock = clock.Add(mirrorMCPFailWindow + time.Second)
	if code, _, _ := f.mirrorRPC(t, mirrorAs(mirrorTok('z'), focusSID), list); code != http.StatusUnauthorized {
		t.Fatalf("the bad token once the window passed: %d, want 401", code)
	}
	delete(f.tokens, ws) // revoked
	if code, _, _ := f.mirrorRPC(t, mirrorAs(ws, focusSID), list); code == http.StatusOK {
		t.Fatal("a revoked token was still served")
	}
}

// countingTokens counts token lookups.
type countingTokens struct {
	MirrorTokenAuth
	n *atomic.Int64
}

func (c countingTokens) Authenticate(ctx context.Context, tok string) (mirror.Token, error) {
	c.n.Add(1)
	return c.MirrorTokenAuth.Authenticate(ctx, tok)
}

// TestAnAddressPastTheFailureLimitIsRefusedBeforeAnyLookup: two hosts share
// one client address (one NAT, one Cloudflare edge). One host's stale token
// fails past the limit: it is answered 429 WITHOUT a token lookup, while the
// other host's valid token from that same address is looked up and served.
// The failure counter's key holds a hash of the token, never the token.
func TestAnAddressPastTheFailureLimitIsRefusedBeforeAnyLookup(t *testing.T) {
	f, ws, _, _ := newMirrorMCPFixture(t)
	var n atomic.Int64
	f.srv.ext.MirrorTokens = countingTokens{MirrorTokenAuth: f.tokens, n: &n}
	clock := time.Now()
	f.srv.now = func() time.Time { return clock }
	list := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	stale := mirrorTok('z')
	for i := 0; i < mirrorMCPFailLimit; i++ {
		if code, _, _ := f.mirrorRPC(t, mirrorAs(stale, focusSID), list); code != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d", i+1, code)
		}
	}
	before := n.Load()
	if code, _, _ := f.mirrorRPC(t, mirrorAs(stale, focusSID), list); code != http.StatusTooManyRequests {
		t.Fatalf("the stale token past the limit: %d", code)
	}
	if n.Load() != before {
		t.Fatalf("a request past the limit looked its token up (%d lookups)", n.Load()-before)
	}
	if code, _, body := f.mirrorRPC(t, mirrorAs(ws, focusSID), list); code != http.StatusOK {
		t.Fatalf("the other host's valid token on the shared address: %d %s", code, body)
	}
	if n.Load() != before+1 {
		t.Fatalf("the valid token was not looked up (%d lookups)", n.Load()-before)
	}
	if k := mirrorMCPFailKey("192.0.2.1", stale); strings.Contains(k, stale) || strings.Contains(k, stale[4:20]) {
		t.Fatalf("the failure key holds the token: %q", k)
	}
	if mirrorMCPFailKey("192.0.2.1", stale) == mirrorMCPFailKey("192.0.2.1", ws) || mirrorMCPFailKey("192.0.2.1", stale) == mirrorMCPFailKey("192.0.2.2", stale) {
		t.Fatal("the failure key does not separate tokens and addresses")
	}
}
