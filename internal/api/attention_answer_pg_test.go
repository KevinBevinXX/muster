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
// chief, not another owner's host.
//
// ⚠ THESE TESTS NAME ONLY IDENTIFIERS THAT EXIST BEFORE THE FEATURE (the tool
// name and its refusals are string literals; provenance is read with SQL), so
// the file also compiles against the pre-feature tree and each test is red
// there for a BEHAVIOURAL reason — not merely because it does not build.

// answerFixture is the delivery fixture (a chief of user 1 with a turn-recording
// gateway and no delivery grace) plus user 1's mirror host "ws" listing two
// Claude sessions (focusSID in @1, secondSID in @3), and another owner's token
// for a host also named "ws".
type answerFixture struct {
	*deliveryFixture
	ws, members string
}

func newAnswerFixture(t *testing.T) *answerFixture {
	t.Helper()
	f := &answerFixture{deliveryFixture: newDeliveryFixture(t)}
	f.mirror.mu.Lock()
	f.mirror.windows = append(attnWindows(mirror.StatusWaiting),
		mirror.Window{Session: "side", ID: "@3", Index: 0, Active: true, Claude: true, ClaudeSession: secondSID, Status: mirror.StatusBusy, StatusSince: attnSince, Activity: 1_700_000_000})
	f.mirror.mu.Unlock()
	f.ws, f.members = mirrorTok('a'), mirrorTok('c')
	f.tokens[f.ws] = mirror.Token{ID: 1, OwnerID: 1, Host: "ws"}
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
// feature: the chief asks and ends its turn; the operator's mirrored session
// answers it over MCP; the ask is answered BY THE OPERATOR with its provenance
// (mcp, ws, that session) on the row; the answer is delivered into the chief's
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
		t.Fatalf("delivery %q into %d (%q); want delivered into the chief's default session %d, as a browser answer is", a.Delivery, a.DeliveredSession, a.DeliveryDetail, f.def.ID)
	}
	turns := f.turnsInto(f.def)
	if len(turns) != 1 {
		t.Fatalf("turns into the default session: %q", turns)
	}
	for _, want := range []string{"your owner answered attention #" + strconv.FormatInt(id, 10) + ", which you asked — answered by the operator via MCP from ws/" + focusSID,
		"|   dispatch it"} {
		if !strings.Contains(turns[0], want) {
			t.Errorf("the chief's turn lacks %q:\n%s", want, turns[0])
		}
	}
	item := answeredItem(f.page(t, "/attention"), id)
	if !strings.Contains(item, "data-attention-answered-via") || !strings.Contains(item, "ws/"+focusSID) {
		t.Errorf("the Answered item does not say where the answer came from:\n%s", item)
	}

	// CONTROL: the same chief, answered on the view.
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

// TestAMirroredSessionAnswersAnotherSessionsAsk: ws's second session asked;
// the first answers it; the asker's attention_wait returns the answer with its
// provenance (a mirrored asker's answer is pulled, never pushed).
func TestAMirroredSessionAnswersAnotherSessionsAsk(t *testing.T) {
	f := newAnswerFixture(t)
	id := f.mirrorAsk(t, secondSID, "Keep the old schema?")
	if text, isErr := f.answerAs(t, f.ws, focusSID, id, "keep it"); isErr {
		t.Fatalf("attention_answer: %s", text)
	}
	if a := f.askStatus(t, id); a.Status != attention.Answered || a.Delivery != attention.DeliveryPull {
		t.Fatalf("%+v; want answered, left for the asker's wait", a)
	}
	_, r, _ := f.mirrorRPC(t, mirrorAs(f.ws, secondSID), mcpToolCall(attentionWaitTool, map[string]any{"id": id, "wait_seconds": 0}))
	if text, _ := toolText(t, r); !strings.Contains(text, `"answer":"keep it"`) || !strings.Contains(text, `"answered_from":"ws/`+focusSID+`"`) {
		t.Fatalf("the asker's wait: %s", text)
	}
}

// TestASessionCannotAnswerItsOwnAsk: the session that raised the ask is
// refused, with the self-answer refusal, and the ask stays open; the CONTROL is
// the other session of the same host answering it.
func TestASessionCannotAnswerItsOwnAsk(t *testing.T) {
	f := newAnswerFixture(t)
	id := f.mirrorAsk(t, focusSID, "Shall I force-push?")
	text, isErr := f.answerAs(t, f.ws, focusSID, id, "yes")
	if !isErr || !strings.Contains(text, "was raised by this session") {
		t.Fatalf("self-answer: %s", text)
	}
	if a := f.askStatus(t, id); a.Status != attention.Open {
		t.Fatalf("after a refused self-answer the ask is %s", a.Status)
	}
	if text, isErr := f.answerAs(t, f.ws, secondSID, id, "no"); isErr {
		t.Fatalf("control: another session's answer: %s", text)
	}
}

// TestOnlyAMirroredSessionCanAnswer: the chief and a worker, calling
// attention_answer BY NAME through the gateway dispatcher (the trust boundary:
// a gateway runtime names any tool it likes) are refused by the handler; over
// POST /mcp it is not even listed (unknown tool); a non-mirror credential on
// /mirror/mcp is 401. The ask stays open through all of it.
func TestOnlyAMirroredSessionCanAnswer(t *testing.T) {
	f := newAnswerFixture(t)
	id := f.ask(t, f.opTok, "Ship it?")
	args := `{"id":` + strconv.FormatInt(id, 10) + `,"answer":"ship it"}`
	for name, a := range map[string]struct{ tok string }{"the chief": {f.chiefTok}, "a worker": {f.opTok}, "another worker": {f.op2Tok}} {
		ag := f.chief
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
	for name, tok := range map[string]string{"the hook token": connTestHookToken, "the chief's token": f.chiefTok, "an agent's token": f.opTok} {
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

// TestAnMCPAnswerIsTheOwnersAnswerForTaskDispatch pins the boundary the PR
// states: an answer given through attention_answer IS the operator's answer to
// a task's ask, so task_dispatch's instructions provenance (AnswersForTask)
// accepts it, exactly as it accepts the view's.
func TestAnMCPAnswerIsTheOwnersAnswerForTaskDispatch(t *testing.T) {
	f := newAnswerFixture(t)
	n, err := f.notes.Create(asUser(1), notes.Note{Title: "mcp-answer task", Body: "b", Status: notes.StatusOpen})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM notes WHERE id = $1`, n.ID)
	})
	a, err := f.attn.Create(asUser(1), attention.New{Asker: attention.Asker{AgentID: f.chief.ID, Label: "chief"}, Question: "Task needs you", TaskID: n.ID})
	if err != nil {
		t.Fatal(err)
	}
	if text, isErr := f.answerAs(t, f.ws, focusSID, a.ID, "fix the flaky test"); isErr {
		t.Fatal(text)
	}
	got, why := f.srv.ownerAnswer(asUser(1), 1, n.ID, "fix the flaky test")
	if why != "" || got.ID != a.ID {
		t.Fatalf("ownerAnswer: %+v %q", got, why)
	}
}
