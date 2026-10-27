package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notes"
)

// attention_answer (attention_tools.go, migration 0034), over a real Postgres:
// the operator answers an ask through one of their own MIRRORED Claude Code
// sessions (POST /mirror/mcp) — the same answer path the Attention view's form
// takes, with its provenance recorded and shown to the asker — and nobody else
// can: not the session that raised the ask, not a dispatched agent, not the
// agent, not another owner's host.
//
// ⚠ THESE TESTS NAME ONLY IDENTIFIERS THAT EXIST BEFORE THE FEATURE (the tool
// name and its refusals are string literals; provenance is read with SQL), so
// the file also compiles against the pre-feature tree and each test is red
// there for a BEHAVIOURAL reason — not merely because it does not build.

// answerFixture is the delivery fixture (an agent of user 1 with a turn-recording
// gateway and no delivery grace) plus user 1's mirror host "ws" listing two
// Claude sessions (focusSID in @1, secondSID in @3), user 1's second host
// "laptop" listing laptopSID, and another owner's token for a host also named
// "ws".
type answerFixture struct {
	*deliveryFixture
	ws, laptop, members string
}

const laptopSID = "5a6b7c8d-9e0f-4a1b-8c2d-3e4f5a6b7c8d"

func newAnswerFixture(t *testing.T) *answerFixture {
	t.Helper()
	f := &answerFixture{deliveryFixture: newDeliveryFixture(t)}
	f.mirror.mu.Lock()
	f.mirror.windows = append(attnWindows(mirror.StatusWaiting),
		mirror.Window{Session: "side", ID: "@3", Index: 0, Active: true, Claude: true, ClaudeSession: secondSID, Status: mirror.StatusBusy, StatusSince: attnSince, Activity: 1_700_000_000})
	f.mirror.others = []mirror.HostView{{Host: "laptop", Online: true, Interactive: true, StatusReport: true,
		Windows: mirror.NormalizeWindows([]mirror.Window{{Session: "lap", ID: "@1", Index: 1, Active: true, Claude: true, ClaudeSession: laptopSID,
			Status: mirror.StatusIdle, StatusSince: attnSince, Activity: 1_700_000_000}}, 0)}}
	f.mirror.mu.Unlock()
	f.ws, f.laptop, f.members = mirrorTok('a'), mirrorTok('b'), mirrorTok('c')
	f.tokens[f.ws] = mirror.Token{ID: 1, OwnerID: 1, Host: "ws"}
	f.tokens[f.laptop] = mirror.Token{ID: 2, OwnerID: 1, Host: "laptop"}
	f.tokens[f.members] = mirror.Token{ID: 3, OwnerID: f.member, Host: "ws"}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM attention_asks WHERE owner_id = 1 AND mirror_host IS NOT NULL`)
	})
	return f
}

// answerAs calls attention_answer through /mirror/mcp as token's host and
// session sid: the tool's text and whether it is a tool error; fatal when the
// call was refused outright (HTTP or JSON-RPC) — use mirrorRPC for those.
func (f *answerFixture) answerAs(t *testing.T, token, sid string, id int64, answer string) (string, bool) {
	t.Helper()
	code, r, body := f.mirrorRPC(t, mirrorAs(token, sid), mcpToolCall("attention_answer", map[string]any{"id": id, "answer": answer}))
	if code != http.StatusOK || r == nil || r.Error != nil {
		t.Fatalf("attention_answer as %s: HTTP %d %s", sid, code, body)
	}
	return toolText(t, r)
}

// answerArgs calls attention_answer with args as given (id, task_id, both or
// neither): the tool's text and whether it is a tool error.
func (f *answerFixture) answerArgs(t *testing.T, token, sid string, args map[string]any) (string, bool) {
	t.Helper()
	code, r, body := f.mirrorRPC(t, mirrorAs(token, sid), mcpToolCall("attention_answer", args))
	if code != http.StatusOK || r == nil || r.Error != nil {
		t.Fatalf("attention_answer %v: HTTP %d %s", args, code, body)
	}
	return toolText(t, r)
}

// mirrorAsk files an ask as mirrored session sid of ws.
func (f *answerFixture) mirrorAsk(t *testing.T, sid, question string) int64 {
	t.Helper()
	_, r, body := f.mirrorRPC(t, mirrorAs(f.ws, sid), mcpToolCall(attentionAskTool, map[string]any{"question": question}))
	if r == nil {
		t.Fatal(body)
	}
	text, isErr := toolText(t, r)
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(text), &out); isErr || err != nil || out.ID == 0 {
		t.Fatalf("ask as %s: %s", sid, text)
	}
	return out.ID
}

// provenance is ask id's (answered_via, answered_via_host, answered_via_session)
// read straight from the row ("" for NULL).
func (f *answerFixture) provenance(t *testing.T, id int64) (string, string, string) {
	t.Helper()
	var via, host, sess string
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT coalesce(answered_via, ''), coalesce(answered_via_host, ''), coalesce(answered_via_session, '')
		FROM attention_asks WHERE id = $1`, id).Scan(&via, &host, &sess); err != nil {
		t.Fatalf("read ask %d's provenance: %v", id, err)
	}
	return via, host, sess
}

// TestAMirroredSessionAnswersTheChiefsAskAndTheChiefSeesWhereFrom is the
// feature: the agent asks and ends its turn; the operator's mirrored session
// answers it over MCP; the ask is answered BY THE OPERATOR with its provenance
// (mcp, ws, that session) on the row; the answer is delivered into the agent's
// default session exactly as a browser answer is, its preface saying it came
// via MCP from ws/<session>; the Answered item on /attention says so too. The
// CONTROL is an ask answered on the view: same delivery, no provenance, and no
// "via MCP" in its turn — so the line is conditional on the door, not constant.
func TestAMirroredSessionAnswersTheChiefsAskAndTheChiefSeesWhereFrom(t *testing.T) {
	f := newAnswerFixture(t)
	id := f.ask(t, f.chiefTok, "Task #776 needs you: dispatch it?", "dispatch it", "leave it")
	text, isErr := f.answerAs(t, f.ws, focusSID, id, "dispatch it")
	if isErr || !strings.Contains(text, `"status":"answered"`) || !strings.Contains(text, `"answered_from":"ws/`+focusSID+`"`) {
		t.Fatalf("attention_answer: %s", text)
	}
	a := f.delivery(t, id)
	if a.Status != attention.Answered || a.Answer != "dispatch it" || a.AnsweredBy != attention.ByOperator {
		t.Fatalf("the ask is %+v; want answered by the operator", a)
	}
	if via, host, sess := f.provenance(t, id); via != "mcp" || host != "ws" || sess != focusSID {
		t.Fatalf("provenance (%q, %q, %q); want (mcp, ws, %s)", via, host, sess, focusSID)
	}
	if a.Delivery != attention.DeliveryDelivered || a.DeliveredSession != f.def.ID {
		t.Fatalf("delivery %q into %d (%q); want delivered into the agent's default session %d, as a browser answer is", a.Delivery, a.DeliveredSession, a.DeliveryDetail, f.def.ID)
	}
	turns := f.turnsInto(f.def)
	if len(turns) != 1 {
		t.Fatalf("turns into the default session: %q", turns)
	}
	for _, want := range []string{"your owner answered attention #" + strconv.FormatInt(id, 10) + ", which you asked — answered by the operator via MCP from ws/" + focusSID,
		"|   dispatch it"} {
		if !strings.Contains(turns[0], want) {
			t.Errorf("the agent's turn lacks %q:\n%s", want, turns[0])
		}
	}
	item := answeredItem(f.page(t, "/attention"), id)
	if !strings.Contains(item, "data-attention-answered-via") || !strings.Contains(item, "ws/"+focusSID) {
		t.Errorf("the Answered item does not say where the answer came from:\n%s", item)
	}

	// CONTROL: the same agent, answered on the view.
	viewed := f.ask(t, f.chiefTok, "And task 777?", "dispatch it")
	f.answerForm(t, viewed, "dispatch it")
	if b := f.delivery(t, viewed); b.Delivery != attention.DeliveryDelivered {
		t.Fatalf("control: the view's answer was not delivered: %q", b.Delivery)
	}
	if via, host, sess := f.provenance(t, viewed); via != "" || host != "" || sess != "" {
		t.Fatalf("control: a view answer carries provenance (%q, %q, %q)", via, host, sess)
	}
	all := f.turnsInto(f.def)
	if len(all) != 2 || strings.Contains(all[1], "via MCP") || !strings.Contains(all[1], "your owner answered attention #"+strconv.FormatInt(viewed, 10)+", which you asked.") {
		t.Fatalf("control: the view answer's turn: %q", all)
	}
	if item := answeredItem(f.page(t, "/attention"), viewed); item == "" || strings.Contains(item, "data-attention-answered-via") {
		t.Fatalf("control: the view answer's item:\n%s", item)
	}
}

// TestASessionOnAnotherHostAnswersAMirroredAsk: ws's session asked; the
// operator's session on ANOTHER host (laptop) answers it; the asker's
// attention_wait returns the answer with its provenance (a mirrored asker's
// answer is pulled, never pushed).
func TestASessionOnAnotherHostAnswersAMirroredAsk(t *testing.T) {
	f := newAnswerFixture(t)
	id := f.mirrorAsk(t, secondSID, "Keep the old schema?")
	if text, isErr := f.answerAs(t, f.laptop, laptopSID, id, "keep it"); isErr {
		t.Fatalf("attention_answer from laptop: %s", text)
	}
	if a := f.askStatus(t, id); a.Status != attention.Answered || a.Delivery != attention.DeliveryPull {
		t.Fatalf("%+v; want answered, left for the asker's wait", a)
	}
	if via, host, sess := f.provenance(t, id); via != "mcp" || host != "laptop" || sess != laptopSID {
		t.Fatalf("provenance (%q, %q, %q)", via, host, sess)
	}
	_, r, _ := f.mirrorRPC(t, mirrorAs(f.ws, secondSID), mcpToolCall(attentionWaitTool, map[string]any{"id": id, "wait_seconds": 0}))
	if text, _ := toolText(t, r); !strings.Contains(text, `"answer":"keep it"`) || !strings.Contains(text, `"answered_from":"laptop/`+laptopSID+`"`) {
		t.Fatalf("the asker's wait: %s", text)
	}
}

// TestNoSessionAnswersAnAskRaisedOnItsOwnHost: the HOST is the principal. An
// open ask raised by a session on ws is refused to the session that raised it
// AND to its sibling on ws (a session can name a sibling's pane), with the
// same-host refusal, and stays open. Once it is closed, a retry from ws says
// "no longer open" — the true reason — not the same-host one. CONTROLS: laptop
// answers it; and ws's session answers an AGENT's ask (no mirror host).
func TestNoSessionAnswersAnAskRaisedOnItsOwnHost(t *testing.T) {
	f := newAnswerFixture(t)
	id := f.mirrorAsk(t, focusSID, "Shall I force-push?")
	for name, sid := range map[string]string{"the asking session": focusSID, "its sibling on the same host": secondSID} {
		text, isErr := f.answerAs(t, f.ws, sid, id, "yes")
		if !isErr || text != `{"error":"ask `+strconv.FormatInt(id, 10)+` was raised by a mirrored session on this host (ws): no session answers an ask raised on its own host; your owner answers it on the Attention view or from another host"}` {
			t.Fatalf("%s: %s", name, text)
		}
	}
	if a := f.askStatus(t, id); a.Status != attention.Open {
		t.Fatalf("after the refused same-host answers the ask is %s", a.Status)
	}
	if text, isErr := f.answerAs(t, f.laptop, laptopSID, id, "no"); isErr {
		t.Fatalf("control: laptop's answer: %s", text)
	}
	for _, sid := range []string{focusSID, secondSID} {
		if text, _ := f.answerAs(t, f.ws, sid, id, "yes"); !strings.Contains(text, "is no longer open (it is answered)") {
			t.Fatalf("a same-host retry after the ask closed: %s", text)
		}
	}
	cancelled := f.mirrorAsk(t, focusSID, "Withdrawn?")
	if _, r, _ := f.mirrorRPC(t, mirrorAs(f.ws, focusSID), mcpToolCall(attentionCancelTool, map[string]any{"id": cancelled})); r == nil {
		t.Fatal("cancel")
	}
	if text, _ := f.answerAs(t, f.ws, focusSID, cancelled, "x"); !strings.Contains(text, "is no longer open (it is cancelled)") {
		t.Fatalf("a same-host answer to a cancelled ask: %s", text)
	}
	agentAsk := f.ask(t, f.opTok, "An agent's question")
	if text, isErr := f.answerAs(t, f.ws, focusSID, agentAsk, "fine"); isErr {
		t.Fatalf("control: ws answering an agent's ask: %s", text)
	}
}

// TestAttentionAnswerByTaskID: {task_id, answer} answers that task's ONE open
// ask; a task with none (including another owner's task, which reads the
// same), with only closed asks, or with two open ones is refused; and exactly
// one of id / task_id must be given.
func TestAttentionAnswerByTaskID(t *testing.T) {
	f := newAnswerFixture(t)
	mk := func(uid int64, title string) notes.Note {
		n, err := f.notes.Create(asUser(uid), notes.Note{Title: title, Body: "b", Status: notes.StatusOpen})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM notes WHERE id = $1`, n.ID)
		})
		return n
	}
	askAbout := func(uid, agent int64, task notes.Note) int64 {
		a, err := f.attn.Create(asUser(uid), attention.New{Asker: attention.Asker{AgentID: agent, Label: "agent"}, Question: "Task #N needs you", TaskID: task.ID})
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	one, none, two, closed, theirs := mk(1, "one ask"), mk(1, "no ask"), mk(1, "two asks"), mk(1, "closed ask"), mk(f.member, "theirs")
	id := askAbout(1, f.agent.ID, one)
	askAbout(1, f.agent.ID, two)
	askAbout(1, f.op.ID, two)
	c := askAbout(1, f.agent.ID, closed)
	if _, err := f.attn.Cancel(asUser(1), c); err != nil {
		t.Fatal(err)
	}
	askAbout(f.member, f.worker.ID, theirs)

	by := func(args map[string]any) (string, bool) {
		args["answer"] = "dispatch it"
		return f.answerArgs(t, f.ws, focusSID, args)
	}
	if text, isErr := by(map[string]any{"task_id": one.ID}); isErr || !strings.Contains(text, `"id":`+strconv.FormatInt(id, 10)) {
		t.Fatalf("task_id with one open ask: %s", text)
	}
	if a := f.askStatus(t, id); a.Status != attention.Answered {
		t.Fatalf("the task's ask is %s", a.Status)
	}
	for name, task := range map[string]notes.Note{"a task with no ask": none, "a task whose ask is closed": closed, "another owner's task": theirs, "the answered task, again": one} {
		if text, _ := by(map[string]any{"task_id": task.ID}); text != `{"error":"task `+strconv.FormatInt(task.ID, 10)+` has no open ask"}` {
			t.Errorf("%s: %s", name, text)
		}
	}
	if text, _ := by(map[string]any{"task_id": two.ID}); !strings.Contains(text, "has 2 open asks") || !strings.Contains(text, "answer one by id") {
		t.Errorf("a task with two open asks: %s", text)
	}
	const exactlyOne = `{"error":"name the ask by exactly one of id (the ask's id) or task_id (the task it is about), not both and not neither"}`
	if text, _ := by(map[string]any{"task_id": two.ID, "id": id}); text != exactlyOne {
		t.Errorf("both: %s", text)
	}
	if text, _ := by(map[string]any{}); text != exactlyOne {
		t.Errorf("neither: %s", text)
	}
	var st string
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT string_agg(status, ',' ORDER BY id) FROM attention_asks WHERE task_id = $1`, two.ID).Scan(&st); err != nil || st != "open,open" {
		t.Fatalf("the two-ask task's asks are %q (%v); want both still open", st, err)
	}
}

// TestOnlyAMirroredSessionCanAnswer: the agent and a worker, calling
// attention_answer BY NAME through the gateway dispatcher (the trust boundary:
// a gateway runtime names any tool it likes) are refused by the handler; over
// POST /mcp it is not even listed (unknown tool); a non-mirror credential on
// /mirror/mcp is 401. The ask stays open through all of it.
func TestOnlyAMirroredSessionCanAnswer(t *testing.T) {
	f := newAnswerFixture(t)
	id := f.ask(t, f.opTok, "Ship it?")
	args := `{"id":` + strconv.FormatInt(id, 10) + `,"answer":"ship it"}`
	for name, a := range map[string]struct{ tok string }{"the agent": {f.chiefTok}, "a worker": {f.opTok}, "another worker": {f.op2Tok}} {
		ag := f.agent
		switch a.tok {
		case f.opTok:
			ag = f.op
		case f.op2Tok:
			ag = f.op2
		}
		got := f.srv.dispatchAgentTool(asUser(1), ag, "attention_answer", args)
		if got != `{"error":"attention_answer is available only to your owner's own mirrored Claude Code sessions; only your owner answers an ask"}` {
			t.Errorf("%s through the gateway dispatcher: %s", name, got)
		}
		if _, _, rpc := f.tool(t, a.tok, "attention_answer", map[string]any{"id": id, "answer": "ship it"}); rpc == nil || rpc.Code != -32602 {
			t.Errorf("%s over POST /mcp: %+v, want unknown tool", name, rpc)
		}
		if f.listed(t, a.tok)["attention_answer"] {
			t.Errorf("%s is offered attention_answer on /mcp", name)
		}
		for _, d := range f.srv.AgentToolDefsFor(asUser(1), ag) {
			if d.Name == "attention_answer" {
				t.Errorf("%s is offered attention_answer as a gateway tool", name)
			}
		}
	}
	for name, tok := range map[string]string{"the hook token": connTestHookToken, "the agent's token": f.chiefTok, "an agent's token": f.opTok} {
		if code, _, body := f.mirrorRPC(t, mirrorAs(tok, focusSID), mcpToolCall("attention_answer", map[string]any{"id": id, "answer": "x"})); code != http.StatusUnauthorized {
			t.Errorf("%s on /mirror/mcp: %d %s, want 401", name, code, body)
		}
	}
	if a := f.askStatus(t, id); a.Status != attention.Open {
		t.Fatalf("the ask is %s after the refused callers; want open", a.Status)
	}
	// CONTROL: the mirrored session answers it.
	if text, isErr := f.answerAs(t, f.ws, focusSID, id, "ship it"); isErr {
		t.Fatalf("control: %s", text)
	}
}

// TestAttentionAnswerRefusesClosedForeignAndBadAsks: an ask already answered,
// a cancelled one, another owner's (row-level security: the same words as a
// missing id), a missing one, and an empty answer.
func TestAttentionAnswerRefusesClosedForeignAndBadAsks(t *testing.T) {
	f := newAnswerFixture(t)
	answered := f.ask(t, f.opTok, "first?")
	if text, isErr := f.answerAs(t, f.ws, focusSID, answered, "a"); isErr {
		t.Fatal(text)
	}
	if text, isErr := f.answerAs(t, f.ws, secondSID, answered, "b"); !isErr || !strings.Contains(text, "is no longer open (it is answered)") {
		t.Fatalf("a second answer: %s", text)
	}
	cancelled := f.ask(t, f.opTok, "second?")
	if text, isErr, _ := f.tool(t, f.opTok, attentionCancelTool, map[string]any{"id": cancelled}); isErr {
		t.Fatal(text)
	}
	if text, isErr := f.answerAs(t, f.ws, focusSID, cancelled, "x"); !isErr || !strings.Contains(text, "is no longer open (it is cancelled)") {
		t.Fatalf("a cancelled ask: %s", text)
	}
	theirs := f.ask(t, f.workerTok, "the member's question") // f.member's worker
	if text, _ := f.answerAs(t, f.ws, focusSID, theirs, "x"); text != `{"error":"no such ask: `+strconv.FormatInt(theirs, 10)+`"}` {
		t.Fatalf("another owner's ask: %s", text)
	}
	var st string
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT status FROM attention_asks WHERE id = $1`, theirs).Scan(&st); err != nil || st != "open" {
		t.Fatalf("another owner's ask is %q (%v); want open", st, err)
	}
	if text, _ := f.answerAs(t, f.ws, focusSID, 9_000_000_123, "x"); text != `{"error":"no such ask: 9000000123"}` {
		t.Fatalf("a missing ask: %s", text)
	}
	open := f.ask(t, f.opTok, "third?")
	if text, isErr := f.answerAs(t, f.ws, focusSID, open, "   "); !isErr || !strings.Contains(text, "the answer is empty") {
		t.Fatalf("an empty answer: %s", text)
	}
	if text, isErr := f.answerAs(t, f.ws, focusSID, open, strings.Repeat("x", attention.MaxAnswerRunes+1)); !isErr || !strings.Contains(text, "longer than") {
		t.Fatalf("an over-long answer: %s", text)
	}
	if a := f.askStatus(t, open); a.Status != attention.Open {
		t.Fatalf("after two invalid answers the ask is %s", a.Status)
	}
}

// TestAnMCPAnswerIsTaskDispatchInstructions (operator decision "Just remove
// the filter"): an answer given through attention_answer is delivered like any
// answer AND ownerAnswer — task_dispatch's instructions provenance — accepts it,
// carrying where it came from (ws/<session>). CONTROL: a view answer on another
// of the task's asks is accepted too, with no provenance — so the provenance is
// the MCP answer's, not a constant.
func TestAnMCPAnswerIsTaskDispatchInstructions(t *testing.T) {
	f := newAnswerFixture(t)
	n, err := f.notes.Create(asUser(1), notes.Note{Title: "mcp-answer task", Body: "b", Status: notes.StatusOpen})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM notes WHERE id = $1`, n.ID)
	})
	a, err := f.attn.Create(asUser(1), attention.New{Asker: attention.Asker{AgentID: f.agent.ID, Label: "agent"}, Question: "Task needs you", TaskID: n.ID})
	if err != nil {
		t.Fatal(err)
	}
	if text, isErr := f.answerAs(t, f.ws, focusSID, a.ID, "fix the flaky test"); isErr {
		t.Fatal(text)
	}
	if got := f.delivery(t, a.ID); got.Delivery != attention.DeliveryDelivered {
		t.Fatalf("the MCP answer was not delivered to the agent: %q", got.Delivery)
	}
	got, why := f.srv.ownerAnswer(asUser(1), 1, n.ID, "fix the flaky test")
	if why != "" || got.ID != a.ID {
		t.Fatalf("the MCP answer was not accepted as instructions: %+v %q", got, why)
	}
	if got.AnsweredVia.Via != "mcp" || got.AnsweredVia.Host != "ws" || got.AnsweredVia.Session != focusSID {
		t.Fatalf("the accepted answer lost its provenance: %+v", got.AnsweredVia)
	}
	b, err := f.attn.Create(asUser(1), attention.New{Asker: attention.Asker{AgentID: f.agent.ID, Label: "agent"}, Question: "Task needs you (again)", TaskID: n.ID})
	if err != nil {
		t.Fatal(err)
	}
	f.answerForm(t, b.ID, "use the view's words")
	if got, why := f.srv.ownerAnswer(asUser(1), 1, n.ID, "use the view's words"); why != "" || got.ID != b.ID || got.AnsweredVia.Via != "" {
		t.Fatalf("control: the view's answer: %+v %q", got, why)
	}
}
