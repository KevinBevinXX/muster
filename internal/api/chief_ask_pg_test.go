package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/plugins"
)

// chief_ask (chief_ask_tool.go, migration 0049) over a real Postgres: a
// mirrored session of user 1 (host ws) asks user 1's chief; the chief gets the
// question as a turn naming the session (and the routed message it is about);
// the chief answers it, or escalates it to the owner, whose answer comes back to
// the session; unanswered, it times out. The answer fixture supplies the chief
// (a turn-recording gateway, its default session), host ws (focusSID,
// secondSID), host laptop and another owner's token for a host named ws.

type chiefAskFixture struct {
	*answerFixture
}

func newChiefAskFixture(t *testing.T) *chiefAskFixture {
	t.Helper()
	f := &chiefAskFixture{answerFixture: newAnswerFixture(t)}
	t.Cleanup(func() {
		sys := authz.System(context.Background())
		_, _ = f.pool.Exec(sys, `UPDATE attention_asks SET escalates = NULL WHERE owner_id = 1`)
		_, _ = f.pool.Exec(sys, `DELETE FROM attention_asks WHERE owner_id = 1 AND (to_chief OR agent_id = $1)`, f.chief.ID)
	})
	return f
}

// mirrorTool calls name on /mirror/mcp as token's host and session sid.
func (f *chiefAskFixture) mirrorTool(t *testing.T, token, sid, name string, args any) (string, bool) {
	t.Helper()
	code, r, body := f.mirrorRPC(t, mirrorAs(token, sid), mcpToolCall(name, args))
	if code != http.StatusOK || r == nil {
		t.Fatalf("%s as %s: HTTP %d %s", name, sid, code, body)
	}
	if r.Error != nil {
		return "rpc-error " + strconv.Itoa(r.Error.Code) + " " + r.Error.Message, true
	}
	return toolText(t, r)
}

// chiefAsk files a question as session sid of ws and returns its id and result.
func (f *chiefAskFixture) chiefAsk(t *testing.T, sid string, args map[string]any) (int64, map[string]any) {
	t.Helper()
	text, isErr := f.mirrorTool(t, f.ws, sid, chiefAskTool, args)
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); isErr || err != nil {
		t.Fatalf("chief_ask as %s: %s", sid, text)
	}
	id, _ := out["id"].(float64)
	if id <= 0 || out["status"] != "pending" {
		t.Fatalf("chief_ask answered %s", text)
	}
	return int64(id), out
}

// wait is attention_wait{id, wait_seconds: 0} as session sid of ws.
func (f *chiefAskFixture) wait(t *testing.T, token, sid string, id int64) map[string]any {
	t.Helper()
	text, isErr := f.mirrorTool(t, token, sid, attentionWaitTool, map[string]any{"id": id, "wait_seconds": 0})
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("attention_wait: %s", text)
	}
	if isErr {
		out["error_text"] = text
	}
	return out
}

// chiefTool calls one of the chief's tools over POST /mcp as token.
func (f *chiefAskFixture) chiefTool(t *testing.T, token, name string, args any) (string, bool) {
	t.Helper()
	text, isErr, rpc := f.tool(t, token, name, args)
	if rpc != nil {
		return "rpc-error " + rpc.Message, true
	}
	return text, isErr
}

// turnFor waits for the chief's turn in sess carrying want and returns it.
func (f *chiefAskFixture) turnFor(t *testing.T, sess agents.ChatSession, want string) string {
	t.Helper()
	var got string
	eventuallyAPI(t, "the chief's turn carrying "+want, func() bool {
		for _, tn := range f.turnsInto(sess) {
			if strings.Contains(tn, want) {
				got = tn
				return true
			}
		}
		return false
	})
	return got
}

// audits is ask id's audit rows as "action detail-json", oldest first.
func (f *chiefAskFixture) audits(t *testing.T, id int64) []string {
	t.Helper()
	rows, err := f.pool.Query(authz.System(context.Background()), `SELECT action, detail::text FROM audit_log WHERE object = $1 ORDER BY id`, attention.AskObject(id))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a, d string
		if err := rows.Scan(&a, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, a+" "+d)
	}
	return out
}

// countAsks is how many of user 1's asks are addressed to the chief.
func (f *chiefAskFixture) countAsks(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM attention_asks WHERE owner_id = 1 AND to_chief`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// withRoute turns routing on and routes one plugin message, delivered to the
// chief in its own thread (a session that is NOT the chief's default), to
// focusSID. It returns the route, the message and that thread.
func (f *chiefAskFixture) withRoute(t *testing.T) (plugins.Route, plugins.Message, agents.ChatSession) {
	t.Helper()
	f.srv.ext.Routes, f.srv.ext.Routing = f.plugins, &Routing{}
	ctx := asUser(1)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	_, p, err := f.plugins.Create(ctx, "chiefask-"+suffix, "Example inbox", "example-inbox-data")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sys := authz.System(context.Background())
		_, _ = f.pool.Exec(sys, `DELETE FROM session_routes WHERE owner_id = 1 AND plugin_message_id IN (SELECT id FROM plugin_messages WHERE plugin_id = $1)`, p.ID)
		_, _ = f.pool.Exec(sys, `DELETE FROM plugin_messages WHERE plugin_id = $1`, p.ID)
		_, _ = f.pool.Exec(sys, `DELETE FROM plugins WHERE id = $1`, p.ID)
	})
	m, _, err := f.plugins.Submit(ctx, p.ID, plugins.MessageIn{ExternalID: "chiefask-" + suffix, Sender: "pat@example.test",
		Subject: "Lunch on Friday?", Body: "Can your team do noon at the usual place?"})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.plugins.BeginDelivery(ctx, m.ID, f.chief.ID); err != nil || !ok {
		t.Fatalf("begin delivery: %v %v", ok, err)
	}
	if ok, err := f.plugins.RecordDelivery(ctx, m.ID, f.chief.ID, plugins.StatusDelivered); err != nil || !ok {
		t.Fatalf("record delivery: %v %v", ok, err)
	}
	thread, err := f.agentsStore.CreateSession(ctx, f.chief.ID, f.chief.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.plugins.SetChiefSession(ctx, p.ID, f.chief.ID, thread.ID); err != nil {
		t.Fatal(err)
	}
	r, err := f.plugins.CreateRoute(ctx, f.chief.ID, 1, m.ID, plugins.RouteTarget{Kind: string(focus.KindMirror), Key: focusSID, Label: "main @ ws"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return r, m, thread
}

// TestChiefAskPutsTheQuestionToTheChiefWithItsSessionAndRoute: the session asks
// about the message the chief routed to it; the chief gets ONE turn, in the
// thread that message was delivered in (not its default session), naming the
// question's id, the session's host and Claude session id, and quoting the
// question, its options and the routed message's sender, subject and body,
// fenced. The question is NOT on the owner's Attention view and pushes nothing
// to the owner's phone; its audit row records the session and the route.
func TestChiefAskPutsTheQuestionToTheChiefWithItsSessionAndRoute(t *testing.T) {
	f := newChiefAskFixture(t)
	if plugins.RouteTargetMirror != string(focus.KindMirror) {
		t.Fatalf("plugins.RouteTargetMirror %q != focus.KindMirror %q", plugins.RouteTargetMirror, focus.KindMirror)
	}
	r, m, thread := f.withRoute(t)
	before := len(f.pushes())
	id, out := f.chiefAsk(t, focusSID, map[string]any{"question": "Should I accept noon, or propose 1pm?", "options": []string{"accept noon", "propose 1pm"},
		"route_id": r.RouteID})
	if out["delivered_as"] != chiefQuestionTurn || !strings.Contains(out["next"].(string), "attention_wait{id: "+strconv.FormatInt(id, 10)+"}") {
		t.Fatalf("chief_ask result: %v", out)
	}
	turn := f.turnFor(t, thread, "session question #"+strconv.FormatInt(id, 10))
	for _, want := range []string{"host ws", "Claude session " + focusSID, "about route " + r.RouteID, "<<<session-question untrusted nonce=",
		"|   Should I accept noon, or propose 1pm?", "|   - propose 1pm", "@ ws",
		"[message_id " + strconv.FormatInt(m.ID, 10) + "] from pat@example.test", "|   subject: Lunch on Friday?",
		"|   body: Can your team do noon at the usual place?", "answer_session_question{id: " + strconv.FormatInt(id, 10)} {
		if !strings.Contains(turn, want) {
			t.Errorf("the chief's turn lacks %q:\n%s", want, turn)
		}
	}
	if other := f.turnsInto(f.def); len(other) != 0 {
		t.Errorf("the question also went to the chief's default session: %q", other)
	}
	if page := f.page(t, "/attention"); strings.Contains(page, `id="`+attentionAskKey(id)+`"`) {
		t.Errorf("the question to the chief is listed on the owner's Attention view")
	}
	if got := f.pushes(); len(got) != before {
		t.Errorf("the question to the chief pushed to the owner: %v", got[before:])
	}
	if a := f.audits(t, id); len(a) != 1 || !strings.HasPrefix(a[0], "chief_question.asked ") || !strings.Contains(a[0], `"route_id": "`+r.RouteID+`"`) ||
		!strings.Contains(a[0], `"session": "`+focusSID+`"`) {
		t.Errorf("audit rows: %q", a)
	}
	// Pending until someone answers.
	if w := f.wait(t, f.ws, focusSID, id); w["status"] != "pending" {
		t.Errorf("the wait before any answer: %v", w)
	}
}

// TestTheChiefAnswersASessionQuestionAndTheWaitReturnsIt: no route — the turn
// goes to the chief's default session; the chief answers; the session's wait
// returns the answer as the chief's; a second answer is refused; the audit says
// who answered.
func TestTheChiefAnswersASessionQuestionAndTheWaitReturnsIt(t *testing.T) {
	f := newChiefAskFixture(t)
	id, _ := f.chiefAsk(t, focusSID, map[string]any{"question": "Which branch holds the release fix?"})
	f.turnFor(t, f.def, "session question #"+strconv.FormatInt(id, 10))

	text, isErr := f.chiefTool(t, f.chiefTok, answerSessionQuestionTool, map[string]any{"id": id, "answer": "release/1.4"})
	if isErr || !strings.Contains(text, `"answered_by":"chief"`) {
		t.Fatalf("the chief's answer: %s", text)
	}
	w := f.wait(t, f.ws, focusSID, id)
	if w["status"] != "answered" || w["answer"] != "release/1.4" || w["answered_by"] != attention.ByChief {
		t.Fatalf("the session's wait: %v", w)
	}
	text, isErr = f.chiefTool(t, f.chiefTok, answerSessionQuestionTool, map[string]any{"id": id, "answer": "main"})
	if !isErr || !strings.Contains(text, "is no longer open (it is answered)") {
		t.Fatalf("a second answer: %s", text)
	}
	if a := f.askStatus(t, id); a.Answer != "release/1.4" {
		t.Fatalf("the second answer overwrote the first: %q", a.Answer)
	}
	if a := f.audits(t, id); len(a) != 2 || !strings.HasPrefix(a[1], "chief_question.answered ") || !strings.Contains(a[1], `"by": "chief"`) {
		t.Errorf("audit rows: %q", a)
	}
}

// TestAnEscalatedQuestionIsAnsweredByTheOwner: the chief escalates; its ask is
// on the owner's Attention view and pushed, linked to the question; the owner
// answers it on the view; the session's wait returns that answer as the
// OPERATOR's; the chief is not sent it (its delivery is settled as read). An MCP
// answer to the escalation from the QUESTION's host is refused; the audit has
// the ask, the escalation and the answer.
func TestAnEscalatedQuestionIsAnsweredByTheOwner(t *testing.T) {
	f := newChiefAskFixture(t)
	id, _ := f.chiefAsk(t, focusSID, map[string]any{"question": "May I force-push the shared branch?"})
	f.turnFor(t, f.def, "session question #"+strconv.FormatInt(id, 10))
	before := len(f.pushes())
	text, isErr := f.chiefTool(t, f.chiefTok, escalateSessionQuestionTool, map[string]any{"id": id,
		"question": "A session on ws wants to force-push the shared branch. Allow it?", "options": []string{"allow", "refuse"}})
	var esc struct {
		ID         int64 `json:"id"`
		QuestionID int64 `json:"question_id"`
	}
	if err := json.Unmarshal([]byte(text), &esc); isErr || err != nil || esc.ID == 0 || esc.QuestionID != id {
		t.Fatalf("escalate: %s", text)
	}
	if e := f.askStatus(t, esc.ID); e.Escalates != id || e.Asker.AgentID != f.chief.ID {
		t.Fatalf("the escalation is not the chief's ask linked to question %d: %+v", id, e)
	}
	if page := f.page(t, "/attention"); !strings.Contains(page, `id="`+attentionAskKey(esc.ID)+`"`) {
		t.Fatalf("the escalation is not on the owner's Attention view")
	}
	if got := f.pushes(); len(got) != before+1 {
		t.Errorf("the escalation pushed %v, want one push", got[before:])
	}
	// The question's own host cannot answer it for the owner, one hop removed.
	text, isErr = f.answerAs(t, f.ws, secondSID, esc.ID, "allow")
	if !isErr || !strings.Contains(text, "raised by a mirrored session on this host") {
		t.Fatalf("an MCP answer from the question's host: %s", text)
	}
	if w := f.wait(t, f.ws, focusSID, id); w["status"] != "pending" {
		t.Fatalf("the refused answer reached the session: %v", w)
	}

	f.answerForm(t, esc.ID, "refuse")
	w := f.wait(t, f.ws, focusSID, id)
	if w["status"] != "answered" || w["answer"] != "refuse" || w["answered_by"] != attention.ByOperator {
		t.Fatalf("the session's wait after the owner answered: %v", w)
	}
	if e := f.delivery(t, esc.ID); e.Delivery != attention.DeliveryWaited {
		t.Errorf("the escalation's delivery to the chief is %q, want waited (the answer went to the session)", e.Delivery)
	}
	for _, tn := range f.gw.seen() {
		if strings.Contains(tn, "your owner answered attention #"+strconv.FormatInt(esc.ID, 10)) {
			t.Errorf("the chief was sent the relayed answer: %s", tn)
		}
	}
	a := f.audits(t, id)
	if len(a) != 3 || !strings.HasPrefix(a[1], "chief_question.escalated ") || !strings.Contains(a[1], `"escalation_id": `+strconv.FormatInt(esc.ID, 10)) ||
		!strings.HasPrefix(a[2], "chief_question.answered ") || !strings.Contains(a[2], `"by": "operator"`) {
		t.Errorf("audit rows: %q", a)
	}
}

// TestAnEscalationAnsweredFromAnotherHostCarriesItsProvenance: the owner
// answers the escalation through a session on laptop; the question is answered
// as the operator via MCP from laptop.
func TestAnEscalationAnsweredFromAnotherHostCarriesItsProvenance(t *testing.T) {
	f := newChiefAskFixture(t)
	id, _ := f.chiefAsk(t, focusSID, map[string]any{"question": "Rotate the key now?"})
	text, isErr := f.chiefTool(t, f.chiefTok, escalateSessionQuestionTool, map[string]any{"id": id, "question": "Rotate the key now?"})
	var esc struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(text), &esc); isErr || err != nil {
		t.Fatalf("escalate: %s", text)
	}
	if text, isErr := f.answerAs(t, f.laptop, laptopSID, esc.ID, "yes, now"); isErr {
		t.Fatalf("the answer from laptop: %s", text)
	}
	if w := f.wait(t, f.ws, focusSID, id); w["status"] != "answered" || w["answer"] != "yes, now" {
		t.Fatalf("the session's wait: %v", w)
	}
	if via, host, sess := f.provenance(t, id); via != "mcp" || host != "laptop" || sess != laptopSID {
		t.Fatalf("the question's provenance is (%q, %q, %q), want mcp from laptop", via, host, sess)
	}
}

// TestAQuestionTheChiefAnsweredKeepsItsAnswerWhenTheOwnerAnswersLate: the chief
// escalates and then answers the question itself; the owner's later answer to
// the escalation does not overwrite it — and, having nowhere to go, it is
// delivered to the chief.
func TestAQuestionTheChiefAnsweredKeepsItsAnswerWhenTheOwnerAnswersLate(t *testing.T) {
	f := newChiefAskFixture(t)
	id, _ := f.chiefAsk(t, focusSID, map[string]any{"question": "Ship today?"})
	f.turnFor(t, f.def, "session question #"+strconv.FormatInt(id, 10))
	text, _ := f.chiefTool(t, f.chiefTok, escalateSessionQuestionTool, map[string]any{"id": id, "question": "Ship today?"})
	var esc struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal([]byte(text), &esc)
	if text, isErr := f.chiefTool(t, f.chiefTok, answerSessionQuestionTool, map[string]any{"id": id, "answer": "yes"}); isErr {
		t.Fatalf("the chief's answer: %s", text)
	}
	f.answerForm(t, esc.ID, "no")
	if q := f.askStatus(t, id); q.Answer != "yes" || q.AnsweredBy != attention.ByChief {
		t.Fatalf("the late owner answer overwrote the chief's: %+v", q)
	}
	if e := f.delivery(t, esc.ID); e.Delivery != attention.DeliveryDelivered {
		t.Fatalf("the late answer's delivery to the chief is %q, want delivered", e.Delivery)
	}
}

// TestAnUnansweredSessionQuestionTimesOut: the question's deadline is
// MUSTER_CHIEF_ASK_TIMEOUT (ChiefAskTimeout) from asking; past it the wait says
// timed_out, and the chief can no longer answer it.
func TestAnUnansweredSessionQuestionTimesOut(t *testing.T) {
	f := newChiefAskFixture(t)
	f.srv.ext.ChiefAskTimeout = 7 * time.Minute
	id, out := f.chiefAsk(t, focusSID, map[string]any{"question": "Is the staging DB safe to reset?"})
	exp, err := time.Parse(time.RFC3339, out["expires_at"].(string))
	if err != nil || exp.Sub(time.Now()) < 6*time.Minute || exp.Sub(time.Now()) > 8*time.Minute {
		t.Fatalf("expires_at %v (%v); want ~7m from now", out["expires_at"], err)
	}
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE attention_asks SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if w := f.wait(t, f.ws, focusSID, id); w["status"] != "timed_out" {
		t.Fatalf("the wait past the deadline: %v", w)
	}
	text, isErr := f.chiefTool(t, f.chiefTok, answerSessionQuestionTool, map[string]any{"id": id, "answer": "yes"})
	if !isErr || !strings.Contains(text, "no longer open (it is timed_out)") {
		t.Fatalf("the chief's answer past the deadline: %s", text)
	}
	// An ordinary ask that expires still reads expired (the control).
	ask := f.mirrorAsk(t, focusSID, "control")
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE attention_asks SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 second' WHERE id = $1`, ask); err != nil {
		t.Fatal(err)
	}
	if w := f.wait(t, f.ws, focusSID, ask); w["status"] != "expired" {
		t.Fatalf("an ordinary expired ask: %v", w)
	}
}

// TestChiefAskRefusesARouteNotSentToTheCaller: another of the owner's sessions
// on the same host naming focusSID's route is refused, and so is an unknown
// route; nothing is filed. The target session itself is accepted (the control).
func TestChiefAskRefusesARouteNotSentToTheCaller(t *testing.T) {
	f := newChiefAskFixture(t)
	r, _, _ := f.withRoute(t)
	for sid, rt := range map[string]string{secondSID: r.RouteID, focusSID: "rt_" + strings.Repeat("0", 24)} {
		text, isErr := f.mirrorTool(t, f.ws, sid, chiefAskTool, map[string]any{"question": "about that message", "route_id": rt})
		want := "was not routed to this session"
		if sid == focusSID {
			want = "no such route"
		}
		if !isErr || !strings.Contains(text, want) {
			t.Errorf("chief_ask as %s naming %s: %s (want %q)", sid, rt, text, want)
		}
	}
	if n := f.countAsks(t); n != 0 {
		t.Fatalf("%d questions filed by refused calls", n)
	}
	f.chiefAsk(t, focusSID, map[string]any{"question": "about that message", "route_id": r.RouteID})
}

// TestChiefAskIsTheMirroredSessionsAndTheAnswerToolsTheChiefs: no dispatched
// agent — the chief included — is offered chief_ask or reaches it by name; a
// worker is refused the chief's answer and escalate tools; a mirrored session
// does not get those either.
func TestChiefAskIsTheMirroredSessionsAndTheAnswerToolsTheChiefs(t *testing.T) {
	f := newChiefAskFixture(t)
	id, _ := f.chiefAsk(t, focusSID, map[string]any{"question": "q"})
	for who, tok := range map[string]string{"the chief": f.chiefTok, "a worker": f.opTok} {
		if f.listed(t, tok)[chiefAskTool] {
			t.Errorf("%s is offered chief_ask", who)
		}
		text, isErr := f.chiefTool(t, tok, chiefAskTool, map[string]any{"question": "by name"})
		if !isErr || !strings.Contains(text, "unknown tool") {
			t.Errorf("%s calling chief_ask by name over /mcp: %s", who, text)
		}
	}
	for who, a := range map[string]agents.Agent{"the chief": f.chief, "a worker": f.op} {
		if text := f.srv.dispatchAgentTool(asUser(1), a, chiefAskTool, `{"question":"by name"}`); !strings.Contains(text, "unknown tool") {
			t.Errorf("%s calling chief_ask through the gateway dispatch: %s", who, text)
		}
	}
	if !f.listed(t, f.chiefTok)[answerSessionQuestionTool] || !f.listed(t, f.chiefTok)[escalateSessionQuestionTool] {
		t.Errorf("the chief is not offered its question tools")
	}
	for _, n := range []string{answerSessionQuestionTool, escalateSessionQuestionTool} {
		if f.listed(t, f.opTok)[n] {
			t.Errorf("a worker is offered %s", n)
		}
		// Over /mcp it is not listed, so refused as unknown; through the
		// dispatch closure a gateway worker reaches with any name, refused as
		// chief-only.
		if text, isErr := f.chiefTool(t, f.opTok, n, map[string]any{"id": id, "answer": "x", "question": "x"}); !isErr || !strings.Contains(text, "unknown tool") {
			t.Errorf("a worker calling %s over /mcp: %s", n, text)
		}
		if text := f.srv.dispatchAgentTool(asUser(1), f.op, n, `{"id":`+strconv.FormatInt(id, 10)+`,"answer":"x","question":"x"}`); !strings.Contains(text, "available to your owner's chief agent only") {
			t.Errorf("a gateway worker calling %s: %s", n, text)
		}
		if text, isErr := f.mirrorTool(t, f.ws, secondSID, n, map[string]any{"id": id, "answer": "x", "question": "x"}); !isErr || !strings.Contains(text, "rpc-error") {
			t.Errorf("a mirrored session calling %s: %s", n, text)
		}
	}
	if a := f.askStatus(t, id); a.Status != attention.Open {
		t.Fatalf("a refused caller moved the question: %s", a.Status)
	}
}

// TestAnotherOwnerNeitherSeesNorAnswersTheQuestion: another owner's session
// (its host also named ws) cannot wait on or answer user 1's question, and that
// owner's chief cannot answer or escalate it; user 1's chief cannot answer an
// ask that was not put to it with answer_session_question.
func TestAnotherOwnerNeitherSeesNorAnswersTheQuestion(t *testing.T) {
	f := newChiefAskFixture(t)
	id, _ := f.chiefAsk(t, focusSID, map[string]any{"question": "q"})
	// The door already refuses the other owner's host for a session it does not
	// list; the tools themselves, as that owner's mirrored session naming the
	// SAME host and Claude session id, find no such ask (row-level security).
	if _, _, text := f.mirrorRPC(t, mirrorAs(f.members, focusSID), mcpToolCall(attentionWaitTool, map[string]any{"id": id})); !strings.Contains(text, "lists no window") {
		t.Errorf("another owner's host was bound to user 1's session: %s", text)
	}
	other := toolCaller{asker: attention.Asker{MirrorHost: "ws", MirrorSession: focusSID, Label: "main @ ws"}}
	octx := asUser(f.member)
	if text := f.srv.toolAttentionWait(octx, other, `{"id":`+strconv.FormatInt(id, 10)+`,"wait_seconds":0}`); !strings.Contains(text, "no such ask") {
		t.Errorf("another owner's session waiting on it: %s", text)
	}
	if text := f.srv.toolAttentionAnswer(octx, other, `{"id":`+strconv.FormatInt(id, 10)+`,"answer":"x"}`); !strings.Contains(text, "no such ask") {
		t.Errorf("another owner's session answering it: %s", text)
	}
	if text := f.srv.toolAttentionCancel(octx, other, `{"id":`+strconv.FormatInt(id, 10)+`}`); !strings.Contains(text, "no such ask") {
		t.Errorf("another owner's session cancelling it: %s", text)
	}
	for _, n := range []string{answerSessionQuestionTool, escalateSessionQuestionTool} {
		text, isErr := f.chiefTool(t, f.attnFixture.chiefTok, n, map[string]any{"id": id, "answer": "x", "question": "x"})
		if !isErr || !strings.Contains(text, "no such session question") {
			t.Errorf("another owner's chief calling %s: %s", n, text)
		}
	}
	// An ordinary ask of the session's — to the owner — is not the chief's to answer.
	ask := f.mirrorAsk(t, focusSID, "for the owner")
	for _, n := range []string{answerSessionQuestionTool, escalateSessionQuestionTool} {
		text, isErr := f.chiefTool(t, f.chiefTok, n, map[string]any{"id": ask, "answer": "x", "question": "x"})
		if !isErr || !strings.Contains(text, "no such session question") {
			t.Errorf("the chief calling %s on an ask to the owner: %s", n, text)
		}
	}
	if a := f.askStatus(t, ask); a.Status != attention.Open {
		t.Fatalf("the chief moved an ask to the owner: %s", a.Status)
	}
	if a := f.askStatus(t, id); a.Status != attention.Open {
		t.Fatalf("another owner moved the question: %s", a.Status)
	}
}

// TestABusyChiefGetsTheQuestionAsANote: while the chief is in the middle of a
// turn (its turn gate held, as the operator's own chat turn holds it), the
// question is not pushed over it: it is written into the chief's default
// session as a note, and the result says so. Once the chief is idle, the next
// question is a turn again (the control).
func TestABusyChiefGetsTheQuestionAsANote(t *testing.T) {
	f := newChiefAskFixture(t)
	if !f.srv.turns.enter(f.chief.ID) {
		t.Fatal("could not hold the chief's turn gate")
	}
	id, out := f.chiefAsk(t, focusSID, map[string]any{"question": "Asked while you were busy?"})
	f.srv.turns.leave(f.chief.ID)
	f.srv.WaitForAttentionDeliveries()
	if out["delivered_as"] != chiefQuestionNote {
		t.Fatalf("delivered_as %v, want note", out["delivered_as"])
	}
	if n := len(f.turnsInto(f.def)); n != 0 {
		t.Fatalf("%d turns pushed over a busy chief", n)
	}
	msgs, err := f.agentsStore.ListChatMessages(asUser(1), f.def.ID)
	if err != nil || len(msgs) != 1 || msgs[0].Role != "user" || !strings.Contains(msgs[0].Content, "session question #"+strconv.FormatInt(id, 10)) {
		t.Fatalf("the note in the chief's default session: %+v %v", msgs, err)
	}
	id2, out := f.chiefAsk(t, focusSID, map[string]any{"question": "And now?"})
	if out["delivered_as"] != chiefQuestionTurn {
		t.Fatalf("an idle chief: delivered_as %v, want turn", out["delivered_as"])
	}
	f.turnFor(t, f.def, "session question #"+strconv.FormatInt(id2, 10))
}

// TestAQuestionWaitsForTheChiefsSessionClaim: while another delivery holds the
// chief's session claim (a plugin message, a hand-back), the question is a
// note, not a second concurrent turn.
func TestAQuestionWaitsForTheChiefsSessionClaim(t *testing.T) {
	f := newChiefAskFixture(t)
	if won, err := f.agentsStore.ClaimKickoff(asUser(1), f.chief.ID, "test/another-delivery", time.Minute); err != nil || !won {
		t.Fatalf("could not hold the chief's claim: %v %v", won, err)
	}
	_, out := f.chiefAsk(t, focusSID, map[string]any{"question": "Asked while a delivery runs?"})
	if err := f.agentsStore.ReleaseKickoffClaim(asUser(1), f.chief.ID, "test/another-delivery"); err != nil {
		t.Fatal(err)
	}
	f.srv.WaitForAttentionDeliveries()
	if out["delivered_as"] != chiefQuestionNote || len(f.turnsInto(f.def)) != 0 {
		t.Fatalf("delivered_as %v with %d turns; want a note and no turn", out["delivered_as"], len(f.turnsInto(f.def)))
	}
}
