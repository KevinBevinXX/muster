package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// ASKUSERQUESTION THROUGH THE REAL MUX (mirror_ask.go): the hook's door on its
// mirror token, the live region, and the answer's gates — and the whole round
// trip with the REAL hook client (askq.Hook) and the PreToolUse payload
// captured from Claude Code 2.1.289.
// ---------------------------------------------------------------------------

// askMirror is fakeMirror plus the ask hook's half of the plugin. Tokens:
// mmt_ws is user 1's host "ws", mmt_other user 1's host "other", mmt_u2 user
// 2's host "ws". Host "ws" (user 1) lists window @1 of session main running
// Claude session sess-1.
type askMirror struct {
	*fakeMirror
	amu     sync.Mutex
	waiting func(owner int64, host, session string) bool
	nudges  map[int64]int
}

func newAskMirror(interactive bool) *askMirror {
	fm := &fakeMirror{online: true, interactive: interactive, v2: &fakeMirrorV2{},
		windows: []mirror.Window{{Session: "main", ID: "@1", Index: 0, Active: true, Claude: true, ClaudeSession: "sess-1"}}}
	return &askMirror{fakeMirror: fm, nudges: map[int64]int{}}
}

func (a *askMirror) AuthenticateHost(w http.ResponseWriter, r *http.Request) (int64, string, bool) {
	switch strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") {
	case "mmt_ws":
		return 1, "ws", true
	case "mmt_other":
		return 1, "other", true
	case "mmt_u2":
		return 2, "ws", true
	}
	http.Error(w, "bad token", http.StatusUnauthorized)
	return 0, "", false
}

func (a *askMirror) HostClaudeSession(owner int64, host, session string) (bool, bool) {
	if owner != 1 || host != "ws" {
		return false, false
	}
	a.fakeMirror.mu.Lock()
	defer a.fakeMirror.mu.Unlock()
	for _, w := range a.fakeMirror.windows {
		if w.ClaudeSession == session && session != "" {
			return true, a.fakeMirror.interactive
		}
	}
	return false, a.fakeMirror.interactive
}

func (a *askMirror) SetWaiting(fn func(int64, string, string) bool) {
	a.amu.Lock()
	a.waiting = fn
	a.amu.Unlock()
}

func (a *askMirror) Nudge(owner int64) {
	a.amu.Lock()
	a.nudges[owner]++
	a.amu.Unlock()
}

func (a *askMirror) isWaiting(owner int64, host, session string) bool {
	a.amu.Lock()
	fn := a.waiting
	a.amu.Unlock()
	return fn != nil && fn(owner, host, session)
}

func askServer(t *testing.T, interactive bool) (*askMirror, *Server, *httptest.Server) {
	t.Helper()
	am := newAskMirror(interactive)
	s, ts := mirrorServer(t, am, newFakeTerminal())
	s.askListWaitOverride = 50 * time.Millisecond
	return am, s, ts
}

// gateInput is the captured call's tool_input, and gatePayload its whole
// PreToolUse payload with the session id this fake host lists.
func gateInput(t *testing.T) json.RawMessage {
	t.Helper()
	var p askq.HookPayload
	if err := json.Unmarshal(gatePayload(t), &p); err != nil {
		t.Fatal(err)
	}
	return p.ToolInput
}

func gatePayload(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../askq/testdata/pretooluse-askuserquestion.json")
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Replace(b, []byte("8c1d1892-a9c0-4d90-bfdf-3c56848ac6fa"), []byte("sess-1"), 1)
}

func hookReq(t *testing.T, ts *httptest.Server, method, path, token string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, ts.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func openAsk(t *testing.T, ts *httptest.Server, token, session, tool string, input json.RawMessage) (int, string) {
	t.Helper()
	code, b := hookReq(t, ts, http.MethodPost, "/mirror/ask", token, map[string]any{"sessionId": session, "toolUseId": tool, "toolInput": input, "holdSeconds": 60})
	var st struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(b, &st)
	return code, st.ID
}

var asksPath = ui.MirrorAsksSrc(ui.MirrorRef{Host: "ws", Session: "main", Window: "@1"})

// answerReq posts an answer form as the operator (fresh unless cookie says
// otherwise), same-origin unless origin says otherwise.
func answerReq(t *testing.T, s *Server, ts *httptest.Server, form url.Values, cookie, origin string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+ui.MirrorAskPath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie == "" {
		cookie = sessionCookieFor(s)
	}
	req.Header.Set("Cookie", cookie)
	if origin == "" {
		origin = ts.URL
	}
	req.Header.Set("Origin", origin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func answerForm(tool string, kv ...string) url.Values {
	f := url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}, "tool": {tool}}
	for i := 0; i < len(kv); i += 2 {
		f.Add(kv[i], kv[i+1])
	}
	return f
}

// TestAQuestionRoundTripsFromTheHookToTheCard: the REAL hook client, given the
// captured PreToolUse payload, registers with muster and holds; the window's
// live region shows the form and the window reads WAITING; the operator's
// answers (a label, two multi-select labels) reach the hook, which prints the
// EXACT proven output; the card says it was answered here.
func TestAQuestionRoundTripsFromTheHookToTheCard(t *testing.T) {
	am, s, ts := askServer(t, true)
	h := askq.Hook{Base: ts.URL, Path: "/mirror/ask", Auth: func(r *http.Request) { r.Header.Set("Authorization", "Bearer mmt_ws") },
		Hold: 20 * time.Second, Wait: time.Second, Client: &http.Client{Timeout: 10 * time.Second}}
	out := make(chan []byte, 1)
	go func() { out <- h.Run(context.Background(), gatePayload(t)) }()

	var card string
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(card, "data-ask-form") {
		if time.Now().After(deadline) {
			t.Fatalf("the live region never showed the form: %s", card)
		}
		time.Sleep(20 * time.Millisecond)
		_, card = mirrorGet(t, s, ts, asksPath)
	}
	if !am.isWaiting(1, "ws", "sess-1") {
		t.Fatal("a held question does not mark its window waiting")
	}
	if am.isWaiting(1, "ws", "sess-2") || am.isWaiting(2, "ws", "sess-1") {
		t.Fatal("waiting leaked to another session or owner")
	}
	resp, body := answerReq(t, s, ts, answerForm("toolu_01RY27FfKTitvDGupDC74z2s", "a0", "Red", "a1", "Dog", "a1", "Cat"), "", "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Ask-State") != ui.AskStateAnswered || !strings.Contains(body, "Answered here") {
		t.Fatalf("answer: %d %s %s", resp.StatusCode, resp.Header.Get("X-Ask-State"), body)
	}
	var got []byte
	select {
	case got = <-out:
	case <-time.After(10 * time.Second):
		t.Fatal("the hook did not return after the answer")
	}
	want, _ := os.ReadFile("../askq/testdata/pretooluse-output-proven.json")
	var g, w any
	_ = json.Unmarshal(got, &g)
	_ = json.Unmarshal(want, &w)
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("the hook printed\n %s\nwant (proven)\n %s", got, want)
	}
	if am.isWaiting(1, "ws", "sess-1") {
		t.Fatal("an answered question still marks its window waiting")
	}
	if am.nudges[1] == 0 {
		t.Fatal("no nudge reached the owner's views")
	}
	_, card = mirrorGet(t, s, ts, asksPath)
	if !strings.Contains(card, `data-ask-state="answered"`) || strings.Contains(card, "data-ask-form") {
		t.Fatalf("after the answer the region shows: %s", card)
	}
}

// TestTheHookDoorRefuses: every refusal happens before anything is held — and
// each is a status the hook reads as "leave it to the terminal".
func TestTheHookDoorRefuses(t *testing.T) {
	_, _, ts := askServer(t, true)
	in := gateInput(t)
	cases := []struct {
		name, token, session, tool string
		input                      json.RawMessage
		status                     int
	}{
		{"no token", "", "sess-1", "toolu_1", in, http.StatusUnauthorized},
		{"a bad token", "mmt_nope", "sess-1", "toolu_1", in, http.StatusUnauthorized},
		{"a session this host does not list", "mmt_ws", "sess-9", "toolu_1", in, http.StatusNotFound},
		{"another host's session", "mmt_other", "sess-1", "toolu_1", in, http.StatusNotFound},
		{"another owner's host", "mmt_u2", "sess-1", "toolu_1", in, http.StatusNotFound},
		{"a malformed session id", "mmt_ws", "../x", "toolu_1", in, http.StatusBadRequest},
		{"a malformed tool id", "mmt_ws", "sess-1", "toolu 1", in, http.StatusBadRequest},
		{"questions muster will not show", "mmt_ws", "sess-1", "toolu_1", json.RawMessage(`{"questions":[]}`), http.StatusBadRequest},
		{"the control", "mmt_ws", "sess-1", "toolu_1", in, http.StatusCreated},
	}
	for _, c := range cases {
		if code, _ := openAsk(t, ts, c.token, c.session, c.tool, c.input); code != c.status {
			t.Errorf("%s: %d, want %d", c.name, code, c.status)
		}
	}
	// A read-only host: muster cannot answer into it, so it holds nothing.
	_, _, ro := askServer(t, false)
	if code, _ := openAsk(t, ro, "mmt_ws", "sess-1", "toolu_1", in); code != http.StatusConflict {
		t.Errorf("a read-only host: %d, want 409", code)
	}
}

// TestOnlyTheAskingHostReadsOrWithdrawsItsQuestion: another host's token —
// the same owner's, or another owner's — cannot poll or withdraw it.
func TestOnlyTheAskingHostReadsOrWithdrawsItsQuestion(t *testing.T) {
	_, s, ts := askServer(t, true)
	code, id := openAsk(t, ts, "mmt_ws", "sess-1", "toolu_1", gateInput(t))
	if code != http.StatusCreated || id == "" {
		t.Fatalf("open: %d %q", code, id)
	}
	for _, tok := range []string{"mmt_other", "mmt_u2", ""} {
		want := http.StatusNotFound
		if tok == "" {
			want = http.StatusUnauthorized
		}
		if c, _ := hookReq(t, ts, http.MethodGet, "/mirror/ask/"+id, tok, nil); c != want {
			t.Errorf("poll with %q: %d, want %d", tok, c, want)
		}
		if c, _ := hookReq(t, ts, http.MethodDelete, "/mirror/ask/"+id, tok, nil); c != want {
			t.Errorf("withdraw with %q: %d, want %d", tok, c, want)
		}
	}
	if c, b := hookReq(t, ts, http.MethodGet, "/mirror/ask/"+id, "mmt_ws", nil); c != http.StatusOK || !strings.Contains(string(b), `"state":"pending"`) {
		t.Fatalf("its own poll: %d %s", c, b)
	}
	if c, _ := hookReq(t, ts, http.MethodDelete, "/mirror/ask/"+id, "mmt_ws", nil); c != http.StatusNoContent {
		t.Fatalf("its own withdraw: %d", c)
	}
	// Withdrawn: the card says the terminal has it, and an answer is refused.
	_, card := mirrorGet(t, s, ts, asksPath)
	if !strings.Contains(card, `data-ask-state="withdrawn"`) {
		t.Fatalf("after a withdraw the region shows: %s", card)
	}
	resp, body := answerReq(t, s, ts, answerForm("toolu_1", "a0", "Red", "a1", "Cat"), "", "")
	if resp.StatusCode != http.StatusConflict || resp.Header.Get("X-Ask-State") != ui.AskStateWithdrawn {
		t.Fatalf("answering a withdrawn question: %d %s", resp.StatusCode, body)
	}
}

// TestAnAnswerPassesEveryGate: the gates a Send passes (same origin, an
// interactive host, a fresh sign-in, the owner's own host), then the answers
// are checked against the questions; a double submit is idempotent, a
// different second answer is refused.
func TestAnAnswerPassesEveryGate(t *testing.T) {
	am, s, ts := askServer(t, true)
	if code, _ := openAsk(t, ts, "mmt_ws", "sess-1", "toolu_1", gateInput(t)); code != http.StatusCreated {
		t.Fatalf("open: %d", code)
	}
	good := answerForm("toolu_1", "a0", "Red", "a1", "Cat")
	type tc struct {
		name           string
		form           url.Values
		cookie, origin string
		status         int
	}
	cases := []tc{
		{"another origin", good, "", "https://evil.example", http.StatusForbidden},
		{"a stale sign-in", good, staleCookieFor(s), "", http.StatusForbidden},
		{"an unknown call", answerForm("toolu_9", "a0", "Red", "a1", "Cat"), "", "", http.StatusNotFound},
		{"another window's session", url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@7"}, "tool": {"toolu_1"}, "a0": {"Red"}, "a1": {"Cat"}}, "", "", http.StatusNotFound},
		{"not an option", answerForm("toolu_1", "a0", "Purple", "a1", "Cat"), "", "", http.StatusBadRequest},
		{"an unknown question", answerForm("toolu_1", "a0", "Red", "a1", "Cat", "a7", "Red"), "", "", http.StatusBadRequest},
		{"a question unanswered", answerForm("toolu_1", "a0", "Red"), "", "", http.StatusBadRequest},
		{"two answers to a single-select", answerForm("toolu_1", "a0", "Red", "a0", "Blue", "a1", "Cat"), "", "", http.StatusBadRequest},
		{"Other with no text", answerForm("toolu_1", "a0", askq.OtherValue, "a1", "Cat"), "", "", http.StatusBadRequest},
		{"text past the bound", answerForm("toolu_1", "a0", askq.OtherValue, "o0", strings.Repeat("x", askq.MaxFreeText+1), "a1", "Cat"), "", "", http.StatusBadRequest},
	}
	for _, c := range cases {
		resp, body := answerReq(t, s, ts, c.form, c.cookie, c.origin)
		if resp.StatusCode != c.status {
			t.Errorf("%s: %d %q, want %d", c.name, resp.StatusCode, body, c.status)
		}
		if c.name == "a stale sign-in" && resp.Header.Get("X-Muster-Step-Up") != "required" {
			t.Errorf("a stale sign-in is not asked to confirm")
		}
	}
	if !am.isWaiting(1, "ws", "sess-1") {
		t.Fatal("a refused answer resolved the question")
	}
	// Another owner, fresh and same-origin: not found (their hosts have no ws).
	form := answerForm("toolu_1", "a0", "Red", "a1", "Cat")
	req := httptest.NewRequest(http.MethodPost, ui.MirrorAskPath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	other := stepup.WithAuthTime(authz.WithPrincipal(context.Background(), authz.Principal{UserID: 2, Role: authz.RoleMember, Via: authz.ViaSession}), time.Now())
	rec := httptest.NewRecorder()
	s.handleMirrorAskAnswer(rec, req.WithContext(other))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("user 2's answer: %d", rec.Code)
	}
	// The answer, free text passed through; then the same again (a double
	// submit) is fine; then a different one is refused.
	first := answerForm("toolu_1", "a0", askq.OtherValue, "o0", "teal, mostly", "a1", "Fish", "a1", "Cat")
	if resp, body := answerReq(t, s, ts, first, "", ""); resp.StatusCode != http.StatusOK || !strings.Contains(body, "teal, mostly") || !strings.Contains(body, "Cat, Fish") {
		t.Fatalf("the answer: %d %s", resp.StatusCode, body)
	}
	if resp, _ := answerReq(t, s, ts, first, "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("a double submit: %d, want 200", resp.StatusCode)
	}
	if resp, _ := answerReq(t, s, ts, answerForm("toolu_1", "a0", "Red", "a1", "Cat"), "", ""); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a different second answer: %d, want 409", resp.StatusCode)
	}
	// A read-only host cannot be answered into, even with a fresh sign-in.
	am.fakeMirror.mu.Lock()
	am.fakeMirror.interactive = false
	am.fakeMirror.mu.Unlock()
	if resp, _ := answerReq(t, s, ts, first, "", ""); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a read-only host: %d, want 409", resp.StatusCode)
	}
}

// TestTheLiveRegionIsTheOwnersWindowOnly: the region of a window the
// principal does not have is not found; a window with no Claude session has
// none.
func TestTheLiveRegionIsTheOwnersWindowOnly(t *testing.T) {
	_, s, ts := askServer(t, true)
	if resp, _ := mirrorGet(t, s, ts, asksPath); resp.StatusCode != http.StatusOK {
		t.Fatalf("the owner's region: %d", resp.StatusCode)
	}
	if resp, _ := mirrorGet(t, s, ts, ui.MirrorAsksSrc(ui.MirrorRef{Host: "nope", Session: "main", Window: "@1"})); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("an unknown host's region: %d", resp.StatusCode)
	}
	req := httptest.NewRequest(http.MethodGet, asksPath, nil)
	rec := httptest.NewRecorder()
	s.handleMirrorAsks(rec, req.WithContext(authz.WithPrincipal(context.Background(), authz.Principal{UserID: 2, Role: authz.RoleMember, Via: authz.ViaSession})))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("user 2 reading user 1's region: %d", rec.Code)
	}
}
