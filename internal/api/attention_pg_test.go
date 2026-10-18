package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
)

// THE ATTENTION QUEUE end to end on a real Postgres: the tools on both
// transports, the view and its forms, the JSON API, the pushes, the unified
// read model's three sources, and the chief-only gate.

type attnFixture struct {
	*connFixture
	attn    *attention.PGStore
	plugins *plugins.PGStore
	notes   notes.Store
	pusher  *payloadPusher
	mirror  *fakeMirror
	tokens  fakeMirrorTokens
	// The operator's (user 1's) two non-chief agents: the asker, and another.
	op, op2       agents.Agent
	opTok, op2Tok string
	// A member with a chief and a worker: the chief-only gate's subjects.
	member              int64
	chief, worker       agents.Agent
	chiefTok, workerTok string
}

const attnSince = int64(1_760_000_000_000)

func attnWindows(status string) []mirror.Window {
	return []mirror.Window{
		{Session: "main", ID: "@1", Index: 1, Active: true, Claude: true, ClaudeSession: focusSID, Status: status, StatusSince: attnSince, Activity: 1_700_000_000},
		{Session: "main", ID: "@2", Index: 2, Claude: true, Status: mirror.StatusIdle, StatusSince: attnSince, Activity: 1_700_000_000},
	}
}

func newAttnFixture(t *testing.T, more ...func(*Extensions, *pgxpool.Pool)) *attnFixture {
	t.Helper()
	f := &attnFixture{pusher: &payloadPusher{}, tokens: fakeMirrorTokens{},
		mirror: &fakeMirror{online: true, interactive: true, statusReport: true, v2: &fakeMirrorV2{}, windows: attnWindows(mirror.StatusWaiting)}}
	store := &memPush{}
	f.connFixture = newConnFixtureWith(t, func(ext *Extensions, pool *pgxpool.Pool) {
		f.attn, f.plugins, f.notes = attention.NewPG(pool), plugins.NewPG(pool), notes.NewPG(pool)
		ext.Attention, ext.Plugins, ext.Notes, ext.Focus, ext.Mirror = f.attn, f.plugins, f.notes, focus.NewPG(pool), f.mirror
		ext.Push = push.NewService(store, f.pusher, push.VAPIDConfig{PublicKey: "k"}, nil)
		ext.MirrorTokens = f.tokens
		for _, m := range more {
			m(ext, pool)
		}
	})
	claim(t, store, 1, ep("attn-operator"))
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	f.opTok, f.op2Tok = "attn-op-token-"+suffix, "attn-op2-token-"+suffix
	f.op = f.agentFor(t, 1, "attnop-"+suffix, "", f.opTok)
	f.op2 = f.agentFor(t, 1, "attnop2-"+suffix, "", f.op2Tok)
	f.member = f.newMember(t)
	f.chiefTok, f.workerTok = "attn-chief-token-"+suffix, "attn-worker-token-"+suffix
	f.chief = f.agentFor(t, f.member, "attnchief-"+suffix, "chief", f.chiefTok)
	f.worker = f.agentFor(t, f.member, "attnworker-"+suffix, "", f.workerTok)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM attention_asks WHERE agent_id IN ($1, $2)`, f.op.ID, f.op2.ID)
	})
	return f
}

func (f *attnFixture) newMember(t *testing.T) int64 {
	t.Helper()
	var id int64
	if err := f.pool.QueryRow(authz.System(f.ctx), `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`,
		"attn-"+strconv.FormatInt(time.Now().UnixNano(), 36)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sys := authz.System(context.Background())
		for _, q := range []string{`DELETE FROM attention_asks WHERE owner_id = $1`, `DELETE FROM notes WHERE owner_id = $1`,
			`DELETE FROM agents WHERE owner_id = $1`, `DELETE FROM audit_log WHERE user_id = $1`, `DELETE FROM users WHERE id = $1`} {
			_, _ = f.pool.Exec(sys, q, id)
		}
	})
	return id
}

func (f *attnFixture) agentFor(t *testing.T, owner int64, name, display, token string) agents.Agent {
	t.Helper()
	a, err := agents.NewPG(f.pool).Create(asUser(owner), agents.Agent{Name: name, Namespace: "devpod-" + name,
		DisplayName: display, Status: agents.StatusRunning, HooksToken: token})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, a.ID)
	})
	return a
}

// tool calls name over POST /mcp as token: the tool's text, whether it was
// flagged an error, and the JSON-RPC error when the call was refused outright.
func (f *attnFixture) tool(t *testing.T, token, name string, args any) (string, bool, *mcpError) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	r, code := f.mcp(t, token, string(raw), nil)
	if code != http.StatusOK || r == nil {
		t.Fatalf("mcp %s: HTTP %d", name, code)
	}
	if r.Error != nil {
		return "", false, r.Error
	}
	text, isErr := toolText(t, r)
	return text, isErr, nil
}

func (f *attnFixture) listed(t *testing.T, token string) map[string]bool {
	t.Helper()
	r, _ := f.mcp(t, token, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	var res struct {
		Tools []struct{ Name string } `json:"tools"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, tl := range res.Tools {
		out[tl.Name] = true
	}
	return out
}

func (f *attnFixture) ask(t *testing.T, token, question string, options ...string) int64 {
	t.Helper()
	text, isErr, rpc := f.tool(t, token, attentionAskTool, map[string]any{"question": question, "options": options})
	if rpc != nil || isErr {
		t.Fatalf("ask: %v %s", rpc, text)
	}
	var out struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.ID <= 0 || out.Status != "pending" {
		t.Fatalf("ask answered %s", text)
	}
	return out.ID
}

func (f *attnFixture) page(t *testing.T, path string) string {
	t.Helper()
	w := f.call(t, http.MethodGet, path, nil, f.operator())
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
	}
	return w.Body.String()
}

// pushes is what reached the operator's device, as "type tag" lines.
func (f *attnFixture) pushes() []string {
	f.srv.WaitForPushes()
	var out []string
	for _, p := range f.pusher.to(ep("attn-operator")) {
		out = append(out, p.Type+" "+p.Tag)
	}
	return out
}

// TestAnAskRaisedOverMCPIsAnsweredFromTheViewAndTheWaitReturnsIt is the whole
// loop: an agent asks over MCP and blocks in attention_wait; the question is on
// /attention (with its options) and on the operator's device; the operator taps
// an option; the wait returns the answer, the notification is cleared, and the
// badge drops by one.
func TestAnAskRaisedOverMCPIsAnsweredFromTheViewAndTheWaitReturnsIt(t *testing.T) {
	f := newAttnFixture(t)
	id := f.ask(t, f.opTok, "Ship the migration tonight?", "ship it", "wait for Monday")
	key := attentionAskKey(id)
	if got := f.pushes(); len(got) != 1 || got[0] != "attention attention-"+key {
		t.Fatalf("pushes after the ask: %v", got)
	}
	if p := f.pusher.to(ep("attn-operator"))[0]; p.Data["url"] != "/attention#"+key || !strings.Contains(p.Body, "Ship the migration tonight?") {
		t.Fatalf("payload %+v", p)
	}
	body := f.page(t, "/attention")
	for _, want := range []string{`id="` + key + `"`, "Ship the migration tonight?", `value="ship it"`, `value="wait for Monday"`,
		`action="/attention/asks/` + strconv.FormatInt(id, 10) + `/answer"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("the view lacks %q", want)
		}
	}
	if b := f.page(t, "/attention/badge"); !strings.Contains(b, `data-count="2"`) { // the ask + the waiting window
		t.Fatalf("badge before: %s", b)
	}

	type waited struct {
		text string
		took time.Duration
	}
	done := make(chan waited, 1)
	go func() {
		start := time.Now()
		text, _, _ := f.tool(t, f.opTok, attentionWaitTool, map[string]any{"id": id, "wait_seconds": 30})
		done <- waited{text, time.Since(start)}
	}()
	time.Sleep(200 * time.Millisecond) // the waiter is blocked before the answer lands
	rd, hdr := form(url.Values{"option": {"ship it"}, "answer": {""}})
	w := f.call(t, http.MethodPost, "/attention/asks/"+strconv.FormatInt(id, 10)+"/answer", rd, merge(f.operator(), hdr))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("answer: %d %s", w.Code, w.Body)
	}
	select {
	case got := <-done:
		if got.text != `{"answer":"ship it","id":`+strconv.FormatInt(id, 10)+`,"question":"Ship the migration tonight?","status":"answered"}` {
			t.Fatalf("wait returned %s", got.text)
		}
		// Woken by the answer, not by the waiter's own 2s re-read (attentionPoll):
		// the answer lands ~200ms in.
		if got.took > 1500*time.Millisecond {
			t.Fatalf("the wait took %s: it did not wake on the answer", got.took)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("attention_wait did not return after the answer")
	}
	if got := f.pushes(); len(got) != 2 || got[1] != "resolved attention-"+key {
		t.Fatalf("pushes after the answer: %v", got)
	}
	if b := f.page(t, "/attention/badge"); !strings.Contains(b, `data-count="1"`) {
		t.Fatalf("badge after: %s", b)
	}
	if strings.Contains(f.page(t, "/attention/items"), key) {
		t.Fatal("the answered ask is still listed")
	}
	// A second answer is refused with the row as it was.
	rd, hdr = form(url.Values{"answer": {"no"}})
	if w := f.call(t, http.MethodPost, "/attention/asks/"+strconv.FormatInt(id, 10)+"/answer", rd, merge(f.operator(), hdr)); w.Code != http.StatusConflict {
		t.Fatalf("second answer: %d", w.Code)
	}
}

// TestAttentionWaitReturnsPendingWhenItsWaitRunsOut: a wait with no answer
// returns "pending" after its wait — and not much later.
func TestAttentionWaitReturnsPendingWhenItsWaitRunsOut(t *testing.T) {
	f := newAttnFixture(t)
	id := f.ask(t, f.opTok, "Anyone there?")
	start := time.Now()
	text, isErr, _ := f.tool(t, f.opTok, attentionWaitTool, map[string]any{"id": id, "wait_seconds": 1})
	took := time.Since(start)
	if isErr || !strings.Contains(text, `"status":"pending"`) {
		t.Fatalf("%s", text)
	}
	if took < time.Second || took > 4*time.Second {
		t.Fatalf("a 1s wait took %s", took)
	}
	if text, _, _ := f.tool(t, f.opTok, attentionWaitTool, map[string]any{"id": id, "wait_seconds": 0}); !strings.Contains(text, `"status":"pending"`) {
		t.Fatalf("get: %s", text)
	}
}

// TestALongWaitKeepsTheConnectionAliveAndStillParses: once a call outlives
// the heartbeat, whitespace precedes the answer, which still decodes.
func TestALongWaitKeepsTheConnectionAliveAndStillParses(t *testing.T) {
	f := newAttnFixture(t)
	old := mcpHeartbeatEvery
	mcpHeartbeatEvery = 100 * time.Millisecond
	t.Cleanup(func() { mcpHeartbeatEvery = old })
	id := f.ask(t, f.opTok, "Heartbeat?")
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 9, "method": "tools/call",
		"params": map[string]any{"name": attentionWaitTool, "arguments": map[string]any{"id": id, "wait_seconds": 1}}})
	w := f.call(t, http.MethodPost, "/mcp", strings.NewReader(string(raw)), map[string]string{"X-Muster-Token": f.opTok, "Content-Type": "application/json"})
	b := w.Body.String()
	if w.Code != http.StatusOK || !strings.HasPrefix(b, "\n") || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%d %q %q", w.Code, w.Header().Get("Content-Type"), b)
	}
	var r mcpReply
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil || string(r.ID) != "9" || !strings.Contains(string(r.Result), `pending`) {
		t.Fatalf("%v %s", err, b)
	}
}

// TestAnAskIsOnlyItsAskersToWaitOnOrCancel: another agent of the same owner
// reads "no such ask" on get, wait and cancel — and the ask stays open.
func TestAnAskIsOnlyItsAskersToWaitOnOrCancel(t *testing.T) {
	f := newAttnFixture(t)
	id := f.ask(t, f.opTok, "Mine only?")
	for _, tool := range []string{attentionWaitTool, attentionCancelTool} {
		text, isErr, rpc := f.tool(t, f.op2Tok, tool, map[string]any{"id": id, "wait_seconds": 0})
		if rpc != nil || !isErr || !strings.Contains(text, "no such ask") {
			t.Fatalf("%s by another agent: %v %s", tool, rpc, text)
		}
	}
	if a, err := f.attn.Get(asUser(1), id); err != nil || a.Status != attention.Open {
		t.Fatalf("%v %+v", err, a)
	}
	text, _, _ := f.tool(t, f.opTok, attentionCancelTool, map[string]any{"id": id})
	if !strings.Contains(text, `"status":"cancelled"`) {
		t.Fatalf("own cancel: %s", text)
	}
	if got := f.pushes(); len(got) != 2 || got[1] != "resolved attention-"+attentionAskKey(id) {
		t.Fatalf("pushes: %v", got)
	}
}

// TestTheChiefOnlyToolsAreRefusedToEveryoneElse: a worker is not offered
// attention_list, attention_answer or session_transcript, and calling one by
// name is refused on BOTH transports; the chief is offered and served them.
func TestTheChiefOnlyToolsAreRefusedToEveryoneElse(t *testing.T) {
	f := newAttnFixture(t)
	chiefOnly := []string{attentionListTool, sessionTranscript}
	allAgents := []string{attentionAskTool, attentionWaitTool, attentionCancelTool, tasksListTool, taskGetTool, taskCommentTool, taskSetStatusTool}
	wl, cl := f.listed(t, f.workerTok), f.listed(t, f.chiefTok)
	for _, n := range chiefOnly {
		if wl[n] || !cl[n] {
			t.Fatalf("%s: worker listed %v, chief listed %v", n, wl[n], cl[n])
		}
	}
	for _, n := range allAgents {
		if !wl[n] || !cl[n] {
			t.Fatalf("%s: worker listed %v, chief listed %v", n, wl[n], cl[n])
		}
	}
	id := f.ask(t, f.workerTok, "Which region?", "us", "eu")
	args := map[string]any{"id": id, "answer": "eu", "subject": "agent:" + strconv.FormatInt(f.worker.ID, 10)}
	for _, n := range chiefOnly {
		_, _, rpc := f.tool(t, f.workerTok, n, args)
		if rpc == nil || rpc.Code != -32602 || !strings.Contains(rpc.Message, "unknown tool") {
			t.Fatalf("MCP %s by a worker: %+v", n, rpc)
		}
		raw, _ := json.Marshal(args)
		if got := f.srv.AgentToolDispatch(asUser(f.member), f.worker)(n, string(raw)); !strings.Contains(got, "chief agent only") {
			t.Fatalf("gateway %s by a worker: %s", n, got)
		}
	}
	// No agent answers an ask — the chief included: there is no such tool.
	for _, n := range []string{"attention_answer", "attention_get"} {
		if _, _, rpc := f.tool(t, f.chiefTok, n, args); rpc == nil || rpc.Code != -32602 {
			t.Fatalf("MCP %s by the chief: %+v", n, rpc)
		}
		raw, _ := json.Marshal(args)
		if got := f.srv.AgentToolDispatch(asUser(f.member), f.chief)(n, string(raw)); !strings.Contains(got, "unknown tool") {
			t.Fatalf("gateway %s by the chief: %s", n, got)
		}
	}
	if a, _ := f.attn.Get(asUser(f.member), id); a.Status != attention.Open {
		t.Fatalf("an agent moved the ask: %+v", a)
	}
	text, _, _ := f.tool(t, f.chiefTok, attentionListTool, map[string]any{})
	if !strings.Contains(text, `"ask_id":`+strconv.FormatInt(id, 10)) || !strings.Contains(text, "Which region?") {
		t.Fatalf("chief list: %s", text)
	}
	// session_transcript: the worker's own conversation, size-capped.
	sess, err := agents.NewPG(f.pool).CreateSession(asUser(f.member), f.worker.ID, f.worker.Name)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []agents.ChatMessage{{Role: "user", Content: "port the importer"}, {Role: "assistant", Content: strings.Repeat("x", transcriptEntryMax+50)}} {
		m.AgentID, m.SessionID = f.worker.ID, sess.ID
		if _, err := agents.NewPG(f.pool).AddChatMessage(asUser(f.member), m); err != nil {
			t.Fatal(err)
		}
	}
	text, _, _ = f.tool(t, f.chiefTok, sessionTranscript, map[string]any{"subject": "agent:" + strconv.FormatInt(f.worker.ID, 10)})
	var tr struct {
		Entries []struct{ Role, Text string } `json:"entries"`
		Trunc   bool                          `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(text), &tr); err != nil || len(tr.Entries) != 2 || tr.Entries[0].Text != "port the importer" ||
		len([]rune(tr.Entries[1].Text)) != transcriptEntryMax+1 || !tr.Trunc {
		t.Fatalf("transcript: %v %.300s", err, text)
	}
	// Another owner's agent is not a session of the chief's owner.
	if text, isErr, _ := f.tool(t, f.chiefTok, sessionTranscript, map[string]any{"subject": "agent:" + strconv.FormatInt(f.op.ID, 10)}); !isErr || !strings.Contains(text, "not a session") {
		t.Fatalf("another owner's transcript: %s", text)
	}
}

// TestATaskToolTouchesOnlyTheCallersOwnTasks: a worker reads, comments on and
// sets the status of the task it was dispatched for — on BOTH transports —
// and nothing else: its owner's other task and another owner's task each
// answer exactly what a task that does not exist answers, and are untouched.
// tasks_list lists its own task alone; the chief's lists the whole board. No
// agent moves a task into or out of complete.
func TestATaskToolTouchesOnlyTheCallersOwnTasks(t *testing.T) {
	f := newAttnFixture(t)
	mine, err := f.notes.Create(asUser(f.member), notes.Note{Title: "attn mine", Body: "the worker's own task", Status: notes.StatusOpen})
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := f.notes.Create(asUser(f.member), notes.Note{Title: "attn sibling", Body: "the same owner's other task", Status: notes.StatusOpen})
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := f.notes.Create(asUser(1), notes.Note{Title: "attn theirs", Body: "the operator's task", Status: notes.StatusOpen})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM notes WHERE id = $1`, theirs.ID)
	})
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	tok := "attn-scoped-token-" + suffix
	worker, err := agents.NewPG(f.pool).Create(asUser(f.member), agents.Agent{Name: "attnscoped-" + suffix, Namespace: "devpod-attnscoped-" + suffix,
		Status: agents.StatusRunning, HooksToken: tok, NoteID: &mine.ID})
	if err != nil {
		t.Fatal(err)
	}
	const ghost = int64(9_000_000_123)
	gateway := f.srv.AgentToolDispatch(asUser(f.member), worker)
	both := func(name string, args map[string]any) map[string]string {
		raw, _ := json.Marshal(args)
		text, _, rpc := f.tool(t, tok, name, args)
		if rpc != nil {
			text = "rpc " + rpc.Message
		}
		return map[string]string{"mcp": text, "gateway": gateway(name, string(raw))}
	}
	for surface, text := range both(tasksListTool, map[string]any{}) {
		var res struct{ Tasks []struct{ ID int64 } }
		if err := json.Unmarshal([]byte(text), &res); err != nil || len(res.Tasks) != 1 || res.Tasks[0].ID != mine.ID {
			t.Fatalf("%s list: %s", surface, text)
		}
	}
	for surface, text := range both(taskGetTool, map[string]any{"id": mine.ID}) {
		if !strings.Contains(text, "the worker's own task") {
			t.Fatalf("%s get own: %s", surface, text)
		}
	}
	for surface, text := range both(taskCommentTool, map[string]any{"id": mine.ID, "body": "on it"}) {
		if !strings.Contains(text, `"body":"on it"`) {
			t.Fatalf("%s comment own: %s", surface, text)
		}
	}
	for surface, text := range both(taskSetStatusTool, map[string]any{"id": mine.ID, "status": "in_progress"}) {
		if !strings.Contains(text, `"status":"in_progress"`) {
			t.Fatalf("%s status own: %s", surface, text)
		}
	}
	for _, name := range []string{taskGetTool, taskCommentTool, taskSetStatusTool} {
		for _, id := range []int64{sibling.ID, theirs.ID, ghost} {
			for surface, text := range both(name, map[string]any{"id": id, "body": "x", "status": "in_progress"}) {
				if text != `{"error":"no such task: `+strconv.FormatInt(id, 10)+`"}` {
					t.Fatalf("%s %s on %d: %s", surface, name, id, text)
				}
			}
		}
	}
	for _, id := range []int64{sibling.ID} {
		if n, _ := f.notes.Get(asUser(f.member), id); n.Status != notes.StatusOpen || len(n.Comments) != 0 {
			t.Fatalf("an out-of-scope task moved: %+v", n)
		}
	}
	if n, _ := f.notes.Get(asUser(1), theirs.ID); n.Status != notes.StatusOpen || len(n.Comments) != 0 {
		t.Fatalf("another owner's task moved: %+v", n)
	}
	for surface, text := range both(taskSetStatusTool, map[string]any{"id": mine.ID, "status": "complete"}) {
		if !strings.Contains(text, "may not set") {
			t.Fatalf("%s into complete: %s", surface, text)
		}
	}
	// The operator completes it; no agent reopens it.
	if _, err := f.notes.SetStatus(asUser(f.member), mine.ID, notes.StatusComplete); err != nil {
		t.Fatal(err)
	}
	for surface, text := range both(taskSetStatusTool, map[string]any{"id": mine.ID, "status": "in_progress"}) {
		if !strings.Contains(text, "may not change a completed task") {
			t.Fatalf("%s out of complete: %s", surface, text)
		}
	}
	// The bound task's own tool and HTTP route obey the same rule.
	if got := gateway("agent_set_task_status", `{"status":"in_progress"}`); !strings.Contains(got, "may not change a completed task") {
		t.Fatalf("agent_set_task_status out of complete: %s", got)
	}
	w := f.call(t, "PATCH", "/agent/task/status", strings.NewReader(`{"status":"in_progress"}`),
		map[string]string{"X-Muster-Token": tok, "Content-Type": "application/json"})
	if w.Code != 403 || !strings.Contains(w.Body.String(), "may not change a completed task") {
		t.Fatalf("PATCH /agent/task/status out of complete: %d %s", w.Code, w.Body)
	}
	if n, _ := f.notes.Get(asUser(f.member), mine.ID); n.Status != notes.StatusComplete {
		t.Fatalf("a completed task was reopened: %s", n.Status)
	}
	// The chief: the whole board of its owner, and never another owner's.
	text, _, _ := f.tool(t, f.chiefTok, tasksListTool, map[string]any{})
	if !strings.Contains(text, `"id":`+strconv.FormatInt(sibling.ID, 10)) || !strings.Contains(text, `"id":`+strconv.FormatInt(mine.ID, 10)) ||
		strings.Contains(text, `"id":`+strconv.FormatInt(theirs.ID, 10)+",") {
		t.Fatalf("chief list: %s", text)
	}
	if text, _, _ := f.tool(t, f.chiefTok, taskCommentTool, map[string]any{"id": sibling.ID, "body": "chief note"}); !strings.Contains(text, `"body":"chief note"`) {
		t.Fatalf("chief comment: %s", text)
	}
	if text, _, _ := f.tool(t, f.chiefTok, taskGetTool, map[string]any{"id": theirs.ID}); text != `{"error":"no such task: `+strconv.FormatInt(theirs.ID, 10)+`"}` {
		t.Fatalf("chief on another owner's task: %s", text)
	}
}

// TestTheReadModelHasEachSource: an ask, a waiting window and a pending reply
// draft each appear as one item of their kind; an idle window, a window of an
// offline host and a decided draft do not.
func TestTheReadModelHasEachSource(t *testing.T) {
	f := newAttnFixture(t)
	ctx := asUser(1)
	id := f.ask(t, f.opTok, "Which bucket?")
	// A pending reply draft of the operator's, filed by an agent the message was delivered to.
	ps, err := f.plugins.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var p plugins.Plugin
	if len(ps) > 0 {
		p = ps[0]
	} else if _, p, err = f.plugins.Create(ctx, "attn-inbox", "Attention inbox", "attn-inbox"); err != nil {
		t.Fatal(err)
	}
	m, _, err := f.plugins.Submit(ctx, p.ID, plugins.MessageIn{ExternalID: "attn-" + strconv.FormatInt(time.Now().UnixNano(), 36), Sender: "s@example.test", Body: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.plugins.BeginDelivery(ctx, m.ID, f.op.ID); err != nil || !ok {
		t.Fatalf("begin delivery: %v %v", ok, err)
	}
	if ok, err := f.plugins.RecordDelivery(ctx, m.ID, f.op.ID, plugins.StatusDelivered); err != nil || !ok {
		t.Fatalf("record delivery: %v %v", ok, err)
	}
	d, _, err := f.plugins.FileDraft(ctx, f.op.ID, 1, f.op.Name, m.ID, "thanks, will do")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM plugin_messages WHERE id = $1`, m.ID)
	})

	kinds := func() map[string][]string {
		items, err := f.srv.attentionItems(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string][]string{}
		for _, it := range items {
			out[it.Kind] = append(out[it.Kind], it.Key)
		}
		return out
	}
	got := kinds()
	if strings.Join(got["ask"], ",") != attentionAskKey(id) || len(got["session"]) != 1 || got["session"][0] != sessionKey("ws", "main", "@1") ||
		!containsStr(got["plugin"], draftKey(d.ID)) {
		t.Fatalf("items %v", got)
	}
	items, _ := f.srv.attentionItems(ctx)
	for _, it := range items {
		if it.Kind == "session" && (it.Link != "/mirror/view?host=ws&session=main&window=%401" || !it.Since.Equal(time.UnixMilli(attnSince))) {
			t.Fatalf("session item %+v", it)
		}
	}
	// The window goes idle; the host goes offline; the draft is rejected.
	f.mirror.mu.Lock()
	f.mirror.windows = attnWindows(mirror.StatusIdle)
	f.mirror.mu.Unlock()
	if got := kinds(); len(got["session"]) != 0 {
		t.Fatalf("an idle window is listed: %v", got)
	}
	f.mirror.mu.Lock()
	f.mirror.windows, f.mirror.online = attnWindows(mirror.StatusWaiting), false
	f.mirror.mu.Unlock()
	if got := kinds(); len(got["session"]) != 0 {
		t.Fatalf("an offline host's window is listed: %v", got)
	}
	// A host that is offline yet still says it reports status is not believed
	// either (the hub keeps a disconnected host's last list for a while).
	f.mirror.mu.Lock()
	f.mirror.online = true
	f.mirror.windows = attnWindows(mirror.StatusIdle)
	f.mirror.others = []mirror.HostView{{Host: "gone", Online: false, StatusReport: true, Windows: mirror.NormalizeWindows(attnWindows(mirror.StatusWaiting), 0)}}
	f.mirror.mu.Unlock()
	if got := kinds(); len(got["session"]) != 0 {
		t.Fatalf("an offline host's window is listed: %v", got)
	}
	if _, err := f.plugins.DecideDraft(ctx, d.ID, false); err != nil {
		t.Fatal(err)
	}
	if got := kinds(); containsStr(got["plugin"], draftKey(d.ID)) {
		t.Fatalf("a rejected draft is listed: %v", got)
	}
	// Another owner reads none of the operator's items.
	if items, _ := f.srv.attentionItems(asUser(f.member)); len(items) != 0 {
		t.Fatalf("the member sees %+v", items)
	}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// setWindows sets the fake host's @1 window to status.
func (f *attnFixture) setWindows(status string) {
	f.mirror.mu.Lock()
	f.mirror.windows = attnWindows(status)
	f.mirror.mu.Unlock()
}

// TestAWaitingSessionIsPushedOncePerEpisode: a host's first observation in
// this process seeds its waiting window SILENTLY (a restart re-notifies
// nothing), and the seed counts as that episode's push: stopping and waiting
// again within attentionRepushAfter is not pushed. A window that starts
// waiting after that is pushed once, however many nudges follow, and cleared
// when it stops; waiting again within attentionRepushAfter of its last push is
// the same episode (no push); after it, a new one (pushed).
func TestAWaitingSessionIsPushedOncePerEpisode(t *testing.T) {
	f := newAttnFixture(t)
	clock := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	f.srv.now = func() time.Time { return clock }
	tag := "attention-" + sessionKey("ws", "main", "@1")
	// Each nudge's pushes are delivered before the next, so their order is the
	// order the windows changed in.
	nudge := func() {
		f.srv.BroadcastAgentsChangedFor(1)
		f.srv.WaitForPushes()
	}
	cycle := func() { // busy, then waiting again (nudged twice)
		f.setWindows(mirror.StatusBusy)
		nudge()
		f.setWindows(mirror.StatusWaiting)
		nudge()
		nudge()
	}
	nudge() // the host's first observation: @1 is already waiting
	if got := f.pushes(); len(got) != 0 {
		t.Fatalf("the first observation pushed %v", got)
	}
	cycle()
	if got := f.pushes(); len(got) != 1 || got[0] != "resolved "+tag {
		t.Fatalf("seeded, stopped, waiting again within %s: %v", attentionRepushAfter, got)
	}
	// Past the seed's window: a new episode, pushed once.
	clock = clock.Add(attentionRepushAfter + time.Minute)
	cycle()
	if got := f.pushes(); len(got) != 3 || got[1] != "resolved "+tag || got[2] != "attention "+tag {
		t.Fatalf("a re-entry after the seed's window: %v", got)
	}
	// Busy and back within the re-push window: cleared, not pushed again.
	clock = clock.Add(time.Minute)
	cycle()
	if got := f.pushes(); len(got) != 4 || got[3] != "resolved "+tag {
		t.Fatalf("a re-entry within %s: %v", attentionRepushAfter, got)
	}
	// After it: a new episode.
	clock = clock.Add(attentionRepushAfter)
	cycle()
	if got := f.pushes(); len(got) != 6 || got[5] != "attention "+tag {
		t.Fatalf("a re-entry after %s: %v", attentionRepushAfter, got)
	}
	// A restart (this process's attention state gone) with @1 still waiting:
	// the reconnecting host is seeded, nothing is pushed.
	f.srv.attention = attentionState{}
	nudge()
	nudge()
	if got := f.pushes(); len(got) != 6 {
		t.Fatalf("after a restart: %v", got)
	}
}

// TestAnExpiredAskIsClearedAndItsWaitReturns: the sweep expires a lapsed ask,
// clears its notification, and a wait on it returns "expired".
func TestAnExpiredAskIsClearedAndItsWaitReturns(t *testing.T) {
	f := newAttnFixture(t)
	id := f.ask(t, f.opTok, "Expiring?")
	if _, err := f.pool.Exec(authz.System(context.Background()),
		`UPDATE attention_asks SET created_at = now() - interval '2 days', expires_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	f.srv.expireAsks(context.Background())
	if got := f.pushes(); len(got) != 2 || got[1] != "resolved attention-"+attentionAskKey(id) {
		t.Fatalf("pushes: %v", got)
	}
	if text, _, _ := f.tool(t, f.opTok, attentionWaitTool, map[string]any{"id": id, "wait_seconds": 5}); !strings.Contains(text, `"status":"expired"`) {
		t.Fatalf("wait: %s", text)
	}
}

// TestAgentInstructionsTellEveryAgentToAskInsteadOfStopping: the gateway
// system prompt and the MCP initialize instructions both carry the paragraph.
func TestAgentInstructionsTellEveryAgentToAskInsteadOfStopping(t *testing.T) {
	f := newAttnFixture(t)
	if !strings.Contains(f.srv.AgentInstructions(f.worker), attentionInstruction) {
		t.Fatal("the gateway instructions lack the attention paragraph")
	}
	r, _ := f.mcp(t, f.workerTok, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`, nil)
	var res struct {
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil || !strings.Contains(res.Instructions, "call attention_ask") {
		t.Fatalf("MCP instructions: %v %q", err, res.Instructions)
	}
}
