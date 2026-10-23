package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/mirror"
)

// AN ASKER'S OPEN ASKS ON ITS OWN SESSION PAGE (attention_asker.go), on a real
// Postgres: the agent page and the mirrored window's view show their asker's
// open asks — no one else's — with the queue's own answer form, and an answer
// given there returns there.

// pageFollow GETs path as the operator, following one redirect (the agent
// page puts its active session in the URL).
func (f *attnFixture) pageFollow(t *testing.T, path string) string {
	t.Helper()
	w := f.call(t, http.MethodGet, path, nil, f.operator())
	if w.Code == http.StatusSeeOther || w.Code == http.StatusFound {
		path = w.Header().Get("Location")
		w = f.call(t, http.MethodGet, path, nil, f.operator())
	}
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
	}
	return w.Body.String()
}

// askBlock is the page's asks block (its <section id="asker-asks">), "" when
// the page draws none.
func askBlock(page string) string {
	i := strings.Index(page, `id="asker-asks"`)
	if i < 0 {
		return ""
	}
	rest := page[i:]
	return rest[:strings.Index(rest, "</section>")]
}

// TestAnAgentPageShowsItsOwnOpenAsksWithTheQueuesForm: the agent page lists
// that agent's open ask — its question, its options, the free-text answer and
// Dismiss, the forms posting to the queue's routes and returning to this page —
// and not another agent's ask, nor its own ask once answered.
func TestAnAgentPageShowsItsOwnOpenAsksWithTheQueuesForm(t *testing.T) {
	f := newAttnFixture(t)
	mine := f.ask(t, f.opTok, "Ship the migration tonight?", "ship it", "wait for Monday")
	theirs := f.ask(t, f.op2Tok, "Rotate the other key?")
	answered := f.ask(t, f.opTok, "Already answered?")
	if _, err := f.srv.answerAsk(asUser(1), answered, "yes"); err != nil {
		t.Fatal(err)
	}
	page := f.pageFollow(t, "/agents/"+f.op.Name)
	block := askBlock(page)
	if block == "" {
		t.Fatal("the agent page draws no asks block")
	}
	// Live: re-read on the queue's own SSE event (the event the server sends).
	for _, want := range []string{`hx-get="/attention/asker?agent=` + url.QueryEscape(f.op.Name) + `&amp;session=`, `hx-trigger="sse:` + EventAttentionChanged + ` from:body`} {
		if !strings.Contains(block, want) {
			t.Errorf("the block lacks %q", want)
		}
	}
	id := strconv.FormatInt(mine, 10)
	back := `<input type="hidden" name="return" value="/agents/` + f.op.Name + `?session=` // the page's own chat session
	for _, want := range []string{`id="` + attentionAskKey(mine) + `"`, "Ship the migration tonight?", `value="ship it"`, `value="wait for Monday"`,
		`action="/attention/asks/` + id + `/answer"`, `action="/attention/asks/` + id + `/cancel"`, back, `name="answer"`, `hx-boost="false"`} {
		if !strings.Contains(block, want) {
			t.Errorf("the block lacks %q:\n%s", want, block)
		}
	}
	if strings.Count(block, back) != 2 {
		t.Errorf("both forms (answer, dismiss) return here: %d", strings.Count(block, back))
	}
	for _, not := range []string{attentionAskKey(theirs), "Rotate the other key?", attentionAskKey(answered)} {
		if strings.Contains(block, not) {
			t.Errorf("the block shows %q, which is not this agent's open ask", not)
		}
	}
	// The other agent's page shows ITS ask, not this one's.
	other := askBlock(f.pageFollow(t, "/agents/"+f.op2.Name))
	if !strings.Contains(other, attentionAskKey(theirs)) || strings.Contains(other, attentionAskKey(mine)) {
		t.Errorf("the other agent's block:\n%s", other)
	}
	// The live refresh answers the same content.
	frag := f.page(t, "/attention/asker?agent="+url.QueryEscape(f.op.Name))
	if !strings.Contains(frag, attentionAskKey(mine)) || strings.Contains(frag, attentionAskKey(theirs)) {
		t.Errorf("the fragment:\n%s", frag)
	}
}

// TestAWindowViewShowsItsSessionsOpenAsksOnly: the rendered view of ws's @1
// (Claude session focusSID) lists that session's open ask, returning to the
// view — and not an agent's ask, nor a session's that this window does not
// run.
func TestAWindowViewShowsItsSessionsOpenAsksOnly(t *testing.T) {
	f := newAttnFixture(t)
	// Windows that share one part of @1's address each: @2 of the same tmux
	// session (secondSID); window @1 of ANOTHER tmux session on ws, "side"
	// (sideSID); and window @1 of session "main" on ANOTHER host, "laptop"
	// (laptopSID) — so each part of the address is what tells them apart.
	const sideSID = "2d4e6f80-1a2b-4c3d-8e4f-5a6b7c8d9e0f"
	f.mirror.mu.Lock()
	f.mirror.windows[1].ClaudeSession = secondSID
	f.mirror.windows = append(f.mirror.windows, mirror.Window{Session: "side", ID: "@1", Index: 1, Active: true, Claude: true,
		ClaudeSession: sideSID, Status: mirror.StatusIdle, StatusSince: attnSince, Activity: 1_700_000_000})
	f.mirror.others = []mirror.HostView{{Host: "laptop", Online: true, Interactive: true, StatusReport: true, Rendered: true,
		Windows: mirror.NormalizeWindows([]mirror.Window{{Session: "main", ID: "@1", Index: 1, Active: true, Claude: true, ClaudeSession: laptopSID,
			Status: mirror.StatusIdle, StatusSince: attnSince, Activity: 1_700_000_000}}, 0)}}
	f.mirror.mu.Unlock()
	mine := f.mirrorAsk(t, "Merge the schema change?")
	sibling := f.askAs(t, attention.Asker{MirrorHost: "ws", MirrorSession: secondSID, Label: "main:2 @ ws"}, "From the other window?")
	otherSession := f.askAs(t, attention.Asker{MirrorHost: "ws", MirrorSession: sideSID, Label: "side:1 @ ws"}, "From another session's @1?")
	otherHost := f.askAs(t, attention.Asker{MirrorHost: "laptop", MirrorSession: laptopSID, Label: "main:1 @ laptop"}, "From another host's main @1?")
	elsewhere := f.askAs(t, attention.Asker{MirrorHost: "ws", MirrorSession: "5f5f5f5f-0000-4000-8000-00000000abcd", Label: "gone @ ws"}, "From a session no window runs?")
	agentAsk := f.ask(t, f.opTok, "An agent's question?")
	view := "/mirror/view?host=ws&session=main&window=%401"
	block := askBlock(f.pageFollow(t, view))
	if block == "" {
		t.Fatal("the view draws no asks block")
	}
	for _, want := range []string{`id="` + attentionAskKey(mine.ID) + `"`, "Merge the schema change?",
		`<input type="hidden" name="return" value="/mirror/view?host=ws&amp;session=main&amp;window=%401">`} {
		if !strings.Contains(block, want) {
			t.Errorf("the block lacks %q:\n%s", want, block)
		}
	}
	for _, not := range []string{attentionAskKey(elsewhere.ID), attentionAskKey(agentAsk), attentionAskKey(sibling.ID),
		attentionAskKey(otherSession.ID), attentionAskKey(otherHost.ID)} {
		if strings.Contains(block, not) {
			t.Errorf("the block shows %s, which is not this window's", not)
		}
	}
	// The other window's view shows ITS session's ask, not this one's.
	other := askBlock(f.pageFollow(t, "/mirror/view?host=ws&session=main&window=%402"))
	if !strings.Contains(other, attentionAskKey(sibling.ID)) || strings.Contains(other, attentionAskKey(mine.ID)) {
		t.Errorf("@2's block:\n%s", other)
	}
	// Each of those windows' own views shows its own ask (the positive side).
	for view, k := range map[string]string{
		"/mirror/view?host=ws&session=side&window=%401":     attentionAskKey(otherSession.ID),
		"/mirror/view?host=laptop&session=main&window=%401": attentionAskKey(otherHost.ID),
	} {
		if b := askBlock(f.pageFollow(t, view)); !strings.Contains(b, k) || strings.Contains(b, attentionAskKey(mine.ID)) {
			t.Errorf("%s's block:\n%s", view, b)
		}
	}
	// The view keeps it live with the queue page's own script.
	if !strings.Contains(f.pageFollow(t, view), `document.getElementById("asker-asks")`) {
		t.Error("the view does not refresh its asks block")
	}
}

// TestAnAnswerOnASessionPageReturnsThere: the answer and the dismissal sent
// from a session page land back on it; any other `return` — another origin,
// a page that draws no form — lands on the queue, as a form on /attention
// does.
func TestAnAnswerOnASessionPageReturnsThere(t *testing.T) {
	f := newAttnFixture(t)
	post := func(id int64, verb string, v url.Values) string {
		t.Helper()
		body, hdr := form(v)
		w := f.call(t, http.MethodPost, "/attention/asks/"+strconv.FormatInt(id, 10)+"/"+verb, body, merge(f.operator(), hdr))
		if w.Code != http.StatusSeeOther {
			t.Fatalf("%s %d: %d %s", verb, id, w.Code, w.Body)
		}
		return w.Header().Get("Location")
	}
	agentPage := "/agents/" + f.op.Name
	view := "/mirror/view?host=ws&session=main&window=%401"
	for _, c := range []struct {
		verb, ret, want string
	}{
		{"answer", agentPage, agentPage},
		{"cancel", agentPage, agentPage},
		{"answer", view, view},
		{"answer", "", "/attention"},
		{"answer", "https://evil.example.test/agents/x", "/attention"},
		{"answer", "//evil.example.test/agents/x", "/attention"},
		{"answer", "/tasks", "/attention"},
		{"answer", "/agents/\tx", "/attention"},
	} {
		id := f.ask(t, f.opTok, "Where do I land? "+c.verb+" "+c.ret)
		v := url.Values{"answer": {"here"}}
		if c.ret != "" {
			v.Set("return", c.ret)
		}
		if got := post(id, c.verb, v); got != c.want {
			t.Errorf("%s with return %q lands on %q, want %q", c.verb, c.ret, got, c.want)
		}
		if a := f.askStatus(t, id); a.Status == attention.Open {
			t.Errorf("%s with return %q left the ask open", c.verb, c.ret)
		}
	}
}

// TestTheChiefsTaskAskOffersItsActionsOnTheChiefsPage: the chief's
// task_route_attention ask, on the chief's page, offers the task actions as its
// buttons (task_dispatch here: the owner opted in and nobody works the task).
func TestTheChiefsTaskAskOffersItsActionsOnTheChiefsPage(t *testing.T) {
	f := newTaskRouteFixture(t, true, true)
	n := f.task(t, "who takes it")
	res := f.call(f.chief, taskRouteAttentionTool, map[string]any{"task_id": n.ID, "note": "Who should take it?"})
	var out struct {
		ID      int64    `json:"id"`
		Offered []string `json:"actions_offered"`
	}
	if err := json.Unmarshal([]byte(res), &out); err != nil || out.ID <= 0 || !containsStr(out.Offered, taskDispatchTool) {
		t.Fatalf("task_route_attention answered %s", res)
	}
	// The chief's page, as its owner (a member: the handler itself, under the
	// owner's principal, as requireSession would call it).
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil).WithContext(f.asOwner)
		r.SetPathValue("name", f.chief.Name)
		w := httptest.NewRecorder()
		f.srv.handleAgentDetail(w, r)
		return w
	}
	w := get("/agents/" + f.chief.Name)
	if w.Code == http.StatusSeeOther {
		w = get(w.Header().Get("Location"))
	}
	if w.Code != http.StatusOK {
		t.Fatalf("the chief's page: %d", w.Code)
	}
	block := askBlock(w.Body.String())
	for _, want := range []string{`id="` + attentionAskKey(out.ID) + `"`, `value="task_dispatch: start a new agent on it"`,
		`action="/attention/asks/` + strconv.FormatInt(out.ID, 10) + `/answer"`, `name="return" value="/agents/` + f.chief.Name + `?session=`} {
		if !strings.Contains(block, want) {
			t.Errorf("the chief's block lacks %q:\n%s", want, block)
		}
	}
}

// TestARefusedAnswerOnASessionPageGoesBackThereSayingWhy: an answer or a
// dismissal that is not recorded — the ask was answered on another device
// meanwhile, or the answer is blank — sends the browser back to the page it
// was given on (its chat session kept) with a notice that page shows, never
// the queue page in its place (it may be a framed view); a form given on the
// queue still gets the queue page, as before. And a return address that
// climbs out of the two page shapes ("..", raw or percent-encoded) is not
// followed.
func TestARefusedAnswerOnASessionPageGoesBackThereSayingWhy(t *testing.T) {
	f := newAttnFixture(t)
	post := func(id int64, verb string, v url.Values) *httptest.ResponseRecorder {
		t.Helper()
		body, hdr := form(v)
		return f.call(t, http.MethodPost, "/attention/asks/"+strconv.FormatInt(id, 10)+"/"+verb, body, merge(f.operator(), hdr))
	}
	// The agent page's own address: its active chat session in the URL.
	first := f.call(t, http.MethodGet, "/agents/"+f.op.Name, nil, f.operator())
	agentPage := first.Header().Get("Location")
	if !strings.HasPrefix(agentPage, "/agents/"+f.op.Name+"?session=") {
		t.Fatalf("the agent page's address: %d %q", first.Code, agentPage)
	}
	view := "/mirror/view?host=ws&session=main&window=%401"
	closed := f.ask(t, f.opTok, "Answered on the phone already?")
	if _, err := f.srv.answerAsk(asUser(1), closed, "yes"); err != nil {
		t.Fatal(err)
	}
	open := f.ask(t, f.opTok, "Still open?")
	for _, c := range []struct {
		id         int64
		verb, ret  string
		answer     string
		wantLoc    string
		wantStatus int
	}{
		{closed, "answer", agentPage, "x", agentPage + "&notice=" + noticeAskClosed, http.StatusSeeOther},
		{closed, "cancel", view, "", view + "&notice=" + noticeAskClosed, http.StatusSeeOther},
		{open, "answer", agentPage, "   ", agentPage + "&notice=" + noticeAskInvalid, http.StatusSeeOther},
		// Given on the queue: the queue page, with its problem (unchanged).
		{closed, "answer", "", "x", "", http.StatusConflict},
	} {
		v := url.Values{"answer": {c.answer}}
		if c.ret != "" {
			v.Set("return", c.ret)
		}
		w := post(c.id, c.verb, v)
		if w.Code != c.wantStatus || w.Header().Get("Location") != c.wantLoc {
			t.Errorf("%s %d (return %q): %d Location %q; want %d %q", c.verb, c.id, c.ret, w.Code, w.Header().Get("Location"), c.wantStatus, c.wantLoc)
		}
	}
	// The page says it.
	if p := f.pageFollow(t, agentPage+"&notice="+noticeAskClosed); !strings.Contains(p, composerNotices[noticeAskClosed].text) {
		t.Error("the agent page does not show the refused answer's notice")
	}
	if p := f.pageFollow(t, view+"&notice="+noticeAskClosed); !strings.Contains(p, composerNotices[noticeAskClosed].text) {
		t.Error("the view does not show the refused answer's notice")
	}
	// Dot segments: not followed, raw or encoded.
	for _, ret := range []string{"/agents/../tasks", "/agents/%2e%2e/tasks", "/agents/%2E%2E/admin", "/mirror/view/../../tasks?x", "/agents/./x"} {
		id := f.ask(t, f.opTok, "Dots "+ret)
		if w := post(id, "answer", url.Values{"answer": {"ok"}, "return": {ret}}); w.Header().Get("Location") != "/attention" {
			t.Errorf("return %q lands on %q, want /attention", ret, w.Header().Get("Location"))
		}
	}
}
