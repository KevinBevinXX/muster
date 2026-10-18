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
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// ASKUSERQUESTION IN BOTH PLACES, THROUGH THE REAL MUX (mirror_ask.go): the
// hook's door on its mirror token (with the REAL hook client and the payload
// captured from Claude Code 2.1.289), the live region, an answer's gates and
// its typing, and the watcher that reads the CLI's record of the result —
// fed here, as a host would feed it, through the window's transcript stream.
// ---------------------------------------------------------------------------

// askMirror is fakeMirror plus the ask half of the plugin. Tokens: mmt_ws is
// user 1's host "ws", mmt_other user 1's host "other", mmt_u2 user 2's host
// "ws". Host "ws" (user 1) lists window @1 of session main running Claude
// session sess-1. Its transcript stream is fed live (feed); AnswerWindow is
// recorded and answers typeErr.
type askMirror struct {
	*fakeMirror
	amu       sync.Mutex
	nudges    map[int64]int
	typed     []string
	typeErr   error
	noAnswer  bool // the host does not advertise AskAnswer
	feedCh    chan []cctranscript.Event
	watchSeen int
}

func newAskMirror(interactive bool) *askMirror {
	fm := &fakeMirror{online: true, interactive: interactive, v2: &fakeMirrorV2{},
		windows: []mirror.Window{{Session: "main", ID: "@1", Index: 0, Active: true, Claude: true, ClaudeSession: "sess-1"}}}
	return &askMirror{fakeMirror: fm, nudges: map[int64]int{}, feedCh: make(chan []cctranscript.Event, 16)}
}

func (a *askMirror) Hosts(ctx context.Context) []mirror.HostView {
	hs := a.fakeMirror.Hosts(ctx)
	a.amu.Lock()
	defer a.amu.Unlock()
	for i := range hs {
		hs[i].AskAnswer = hs[i].Online && hs[i].Interactive && !a.noAnswer
	}
	return hs
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

func (a *askMirror) AnswerWindow(ctx context.Context, host, session, window string, qs []askq.Question, cs []askq.Choice) error {
	b, _ := json.Marshal(cs)
	a.amu.Lock()
	defer a.amu.Unlock()
	a.typed = append(a.typed, host+" "+session+" "+window+" "+string(b))
	return a.typeErr
}

func (a *askMirror) Nudge(owner int64) {
	a.amu.Lock()
	a.nudges[owner]++
	a.amu.Unlock()
}

// WatchWindow is the window's transcript stream: whatever feed sends, until
// the watch ends. Only user 1's window main@1 has one.
func (a *askMirror) WatchWindow(ctx context.Context, host, session, window, gen string, offset int64, out func(mirror.Msg) error) error {
	if p, ok := authz.From(ctx); !ok || p.UserID != 1 || host != "ws" || session != "main" || window != "@1" {
		return mirror.ErrHostOffline
	}
	a.amu.Lock()
	a.watchSeen++
	a.amu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return nil
		case evs := <-a.feedCh:
			if err := out(mirror.Msg{Type: mirror.TypeEvents, Gen: "sess-1", Events: evs}); err != nil {
				return err
			}
		}
	}
}

// feed is the CLI writing the call's result: answers (nil = dismissed).
func (a *askMirror) feed(tool string, qs []askq.Question, answers map[string]string) {
	ev := cctranscript.Event{Kind: cctranscript.KindToolResult, ToolID: tool, OK: answers != nil}
	if answers != nil {
		ev.Ask = &cctranscript.Ask{Questions: qs, Answers: answers}
	}
	a.feedCh <- []cctranscript.Event{
		{Kind: cctranscript.KindToolCall, ToolID: tool, Tool: askq.ToolName, Ask: &cctranscript.Ask{Questions: qs}},
		ev,
	}
}

func (a *askMirror) typedSoFar() []string {
	a.amu.Lock()
	defer a.amu.Unlock()
	return append([]string(nil), a.typed...)
}

func askServer(t *testing.T, interactive bool) (*askMirror, *Server, *httptest.Server) {
	t.Helper()
	am := newAskMirror(interactive)
	s, ts := mirrorServer(t, am, newFakeTerminal())
	s.askWatch.mu.Lock()
	s.askWatch.retry = 20 * time.Millisecond
	s.askWatch.mu.Unlock()
	t.Cleanup(s.EndStreams)
	return am, s, ts
}

// gatePayload is the captured PreToolUse payload with the session id this
// fake host lists; gateQs its questions.
func gatePayload(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../askq/testdata/pretooluse-askuserquestion.json")
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Replace(b, []byte("8c1d1892-a9c0-4d90-bfdf-3c56848ac6fa"), []byte("sess-1"), 1)
}

func gateInput(t *testing.T) json.RawMessage {
	t.Helper()
	var p askq.HookPayload
	if err := json.Unmarshal(gatePayload(t), &p); err != nil {
		t.Fatal(err)
	}
	return p.ToolInput
}

func gateQs(t *testing.T) []askq.Question {
	qs, err := askq.ParseInput(gateInput(t))
	if err != nil {
		t.Fatal(err)
	}
	return qs
}

const gateTool = "toolu_01RY27FfKTitvDGupDC74z2s"

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

func openAsk(t *testing.T, ts *httptest.Server, token, session, tool string, input json.RawMessage) int {
	t.Helper()
	code, _ := hookReq(t, ts, http.MethodPost, "/mirror/ask", token, map[string]any{"id": askq.NewID(), "sessionId": session, "toolUseId": tool, "toolInput": input})
	return code
}

// register is the REAL hook client registering the captured call.
func register(t *testing.T, ts *httptest.Server) {
	t.Helper()
	h := askq.Hook{Base: ts.URL, Path: "/mirror/ask", Auth: func(r *http.Request) { r.Header.Set("Authorization", "Bearer mmt_ws") },
		Client: &http.Client{Timeout: 3 * time.Second}}
	start := time.Now()
	if !h.Run(context.Background(), gatePayload(t)) {
		t.Fatal("the hook's registration was refused")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the registration took %s: the terminal's picker waits on it", d)
	}
}

var asksPath = ui.MirrorAsksSrc(ui.MirrorRef{Host: "ws", Session: "main", Window: "@1"})

// region is the window's live region, waited for until it holds want.
func region(t *testing.T, s *Server, ts *httptest.Server, want string) string {
	t.Helper()
	var card string
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(card, want) {
		if time.Now().After(deadline) {
			t.Fatalf("the live region never showed %q:\n%s", want, card)
		}
		_, card = mirrorGet(t, s, ts, asksPath)
		time.Sleep(10 * time.Millisecond)
	}
	return card
}

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

// TestAQuestionShowsAtOnceAndTheTerminalsAnswerFlipsIt: the hook registers
// and returns at once; the question is in the live region (and the attention
// queue) at once; answered in the terminal, the card says so, with the
// answers, and it leaves the queue.
func TestAQuestionShowsAtOnceAndTheTerminalsAnswerFlipsIt(t *testing.T) {
	am, s, ts := askServer(t, true)
	register(t, ts)
	region(t, s, ts, `data-ask-state="pending"`)
	if got := s.Questions().Pending(1); len(got) != 1 {
		t.Fatalf("the attention queue's source has %d open questions", len(got))
	}
	am.feed(gateTool, gateQs(t), map[string]string{"Which color do you prefer?": "Green", "Which pets do you have?": "Dog"})
	card := region(t, s, ts, `data-ask-state="answered-terminal"`)
	if !strings.Contains(card, "Answered in the terminal") || !strings.Contains(card, "Answer: </span>Green") || strings.Contains(card, "<form") {
		t.Fatalf("the answered card:\n%s", card)
	}
	if got := s.Questions().Pending(1); len(got) != 0 {
		t.Fatalf("an answered question is still open: %+v", got)
	}
	if len(am.typedSoFar()) != 0 {
		t.Fatal("something was typed for a question answered in the terminal")
	}
}

// TestAnAnswerIsTypedAndTheCLIsRecordConfirmsIt: the person's answers go to the
// host as the picker's choices; the card says "typing" until the CLI records
// the same answers, then "answered here".
func TestAnAnswerIsTypedAndTheCLIsRecordConfirmsIt(t *testing.T) {
	am, s, ts := askServer(t, true)
	register(t, ts)
	region(t, s, ts, `data-ask-state="pending"`)
	resp, body := answerReq(t, s, ts, answerForm(gateTool, "a0", "Red", "a1", "Fish", "a1", "Dog"), "", "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Ask-State") != ui.AskStateTyping {
		t.Fatalf("answer: %d %s\n%s", resp.StatusCode, resp.Header.Get("X-Ask-State"), body)
	}
	if got := am.typedSoFar(); !reflect.DeepEqual(got, []string{`ws main @1 [{"picks":[0]},{"picks":[1,2]}]`}) {
		t.Fatalf("typed %q", got)
	}
	am.feed(gateTool, gateQs(t), map[string]string{"Which color do you prefer?": "Red", "Which pets do you have?": "Dog, Fish"})
	card := region(t, s, ts, `data-ask-state="answered"`)
	if !strings.Contains(card, "Answered here") {
		t.Fatalf("the answered card:\n%s", card)
	}
	// A double submit after Claude has it: the card that says so, and nothing
	// typed again.
	if resp, _ := answerReq(t, s, ts, answerForm(gateTool, "a0", "Red", "a1", "Fish", "a1", "Dog"), "", ""); resp.StatusCode != http.StatusConflict ||
		resp.Header.Get("X-Ask-State") != ui.AskStateAnswered {
		t.Fatalf("a double submit: %d %s", resp.StatusCode, resp.Header.Get("X-Ask-State"))
	}
	if got := am.typedSoFar(); len(got) != 1 {
		t.Fatalf("a double submit typed again: %q", got)
	}
}

// TestTheTerminalWinsTheRace: the terminal answered first (the CLI's record
// arrived); muster's late answer is refused with the card that says so, and
// nothing is typed. And a typing that the host could not do (its picker gone)
// leaves the question to the terminal, saying so — then the terminal's answer
// still resolves it.
func TestTheTerminalWinsTheRace(t *testing.T) {
	am, s, ts := askServer(t, true)
	register(t, ts)
	region(t, s, ts, `data-ask-state="pending"`)
	am.feed(gateTool, gateQs(t), map[string]string{"Which color do you prefer?": "Blue", "Which pets do you have?": "Cat"})
	region(t, s, ts, `data-ask-state="answered-terminal"`)
	resp, body := answerReq(t, s, ts, answerForm(gateTool, "a0", "Red", "a1", "Cat"), "", "")
	if resp.StatusCode != http.StatusConflict || resp.Header.Get("X-Ask-State") != ui.AskStateAnsweredTerminal {
		t.Fatalf("a late answer: %d %s\n%s", resp.StatusCode, resp.Header.Get("X-Ask-State"), body)
	}
	if len(am.typedSoFar()) != 0 {
		t.Fatal("a late answer was typed")
	}

	am2, s2, ts2 := askServer(t, true)
	register(t, ts2)
	region(t, s2, ts2, `data-ask-state="pending"`)
	am2.amu.Lock()
	am2.typeErr = mirror.ErrAskNoPicker
	am2.amu.Unlock()
	resp, body = answerReq(t, s2, ts2, answerForm(gateTool, "a0", "Red", "a1", "Cat"), "", "")
	if resp.StatusCode != http.StatusConflict || resp.Header.Get("X-Ask-State") != ui.AskStateFailed ||
		!strings.Contains(body, "no longer on the terminal") || strings.Contains(body, "<form") {
		t.Fatalf("a typing with no picker: %d %s\n%s", resp.StatusCode, resp.Header.Get("X-Ask-State"), body)
	}
	am2.feed(gateTool, gateQs(t), map[string]string{"Which color do you prefer?": "Blue", "Which pets do you have?": "Cat"})
	region(t, s2, ts2, `data-ask-state="answered-terminal"`)
}

func TestADismissedPickerIsDeclined(t *testing.T) {
	am, s, ts := askServer(t, true)
	register(t, ts)
	region(t, s, ts, `data-ask-state="pending"`)
	am.feed(gateTool, gateQs(t), nil)
	if card := region(t, s, ts, `data-ask-state="declined"`); !strings.Contains(card, "Dismissed in the terminal") {
		t.Fatalf("the declined card:\n%s", card)
	}
}

// TestTheHookDoorRefuses: every refusal happens on the hook's door; none needs
// the session listed (the question shows only where it is).
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
		{"a malformed session id", "mmt_ws", "../x", "toolu_1", in, http.StatusBadRequest},
		{"a malformed tool id", "mmt_ws", "sess-1", "toolu 1", in, http.StatusBadRequest},
		{"questions muster will not show", "mmt_ws", "sess-1", "toolu_1", json.RawMessage(`{"questions":[]}`), http.StatusBadRequest},
		{"the control", "mmt_ws", "sess-1", "toolu_1", in, http.StatusCreated},
		{"a session the host does not list yet", "mmt_ws", "sess-9", "toolu_2", in, http.StatusCreated},
	}
	for _, c := range cases {
		if code := openAsk(t, ts, c.token, c.session, c.tool, c.input); code != c.status {
			t.Errorf("%s: %d, want %d", c.name, code, c.status)
		}
	}
	// The hold-era poll and withdraw routes are gone.
	if code, _ := hookReq(t, ts, http.MethodGet, "/mirror/ask/"+askq.NewID(), "mmt_ws", nil); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
		t.Errorf("GET /mirror/ask/{id}: %d", code)
	}
}

// TestAQuestionIsShownOnlyToItsOwnerInItsHostsWindow: a question another
// owner's (or another host's) token registered for the same session id is not
// in this window's region.
func TestAQuestionIsShownOnlyToItsOwnerInItsHostsWindow(t *testing.T) {
	_, s, ts := askServer(t, true)
	for _, tok := range []string{"mmt_u2", "mmt_other"} {
		if code := openAsk(t, ts, tok, "sess-1", "toolu_"+tok[4:], gateInput(t)); code != http.StatusCreated {
			t.Fatalf("%s: %d", tok, code)
		}
	}
	if _, card := mirrorGet(t, s, ts, asksPath); strings.Contains(card, "data-ask-live") {
		t.Fatalf("another owner's or host's question is shown here:\n%s", card)
	}
	if code := openAsk(t, ts, "mmt_ws", "sess-1", "toolu_mine", gateInput(t)); code != http.StatusCreated {
		t.Fatal(code)
	}
	region(t, s, ts, `data-ask-tool="toolu_mine"`)
}

// TestAnAnswerPassesEveryGate: the gates a Send passes (same origin, an
// interactive host, a fresh sign-in, the owner's own host), then the answers
// are checked against the questions and what the picker can take.
func TestAnAnswerPassesEveryGate(t *testing.T) {
	am, s, ts := askServer(t, true)
	if code := openAsk(t, ts, "mmt_ws", "sess-1", "toolu_1", gateInput(t)); code != http.StatusCreated {
		t.Fatalf("open: %d", code)
	}
	good := answerForm("toolu_1", "a0", "Red", "a1", "Cat")
	cases := []struct {
		name           string
		form           url.Values
		cookie, origin string
		status         int
	}{
		{"another origin", good, "", "https://evil.example", http.StatusForbidden},
		{"a stale sign-in", good, staleCookieFor(s), "", http.StatusForbidden},
		{"an unknown call", answerForm("toolu_9", "a0", "Red", "a1", "Cat"), "", "", http.StatusNotFound},
		{"no call named", answerForm("", "a0", "Red", "a1", "Cat"), "", "", http.StatusBadRequest},
		{"another window", url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@7"}, "tool": {"toolu_1"}, "a0": {"Red"}, "a1": {"Cat"}}, "", "", http.StatusNotFound},
		{"not an option", answerForm("toolu_1", "a0", "Purple", "a1", "Cat"), "", "", http.StatusBadRequest},
		{"an unknown question", answerForm("toolu_1", "a0", "Red", "a1", "Cat", "a7", "Red"), "", "", http.StatusBadRequest},
		{"a question unanswered", answerForm("toolu_1", "a0", "Red"), "", "", http.StatusBadRequest},
		{"two answers to a single-select", answerForm("toolu_1", "a0", "Red", "a0", "Blue", "a1", "Cat"), "", "", http.StatusBadRequest},
		{"own words on two lines", answerForm("toolu_1", "a0", askq.OtherValue, "o0", "two\nlines", "a1", "Cat"), "", "", http.StatusBadRequest},
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
	if got := am.typedSoFar(); len(got) != 0 {
		t.Fatalf("a refused answer was typed: %q", got)
	}
	// Another owner, fresh and same-origin: not found.
	req := httptest.NewRequest(http.MethodPost, ui.MirrorAskPath, strings.NewReader(good.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	other := stepup.WithAuthTime(authz.WithPrincipal(context.Background(), authz.Principal{UserID: 2, Role: authz.RoleMember, Via: authz.ViaSession}), time.Now())
	rec := httptest.NewRecorder()
	s.handleMirrorAskAnswer(rec, req.WithContext(other))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("user 2's answer: %d", rec.Code)
	}
	// A host that cannot type answers: refused, nothing typed.
	am.amu.Lock()
	am.noAnswer = true
	am.amu.Unlock()
	if resp, _ := answerReq(t, s, ts, good, "", ""); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a host that cannot type: %d", resp.StatusCode)
	}
	am.amu.Lock()
	am.noAnswer = false
	am.amu.Unlock()
	// The answer; then a different one while it is being typed is refused.
	if resp, _ := answerReq(t, s, ts, good, "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("the answer: %d", resp.StatusCode)
	}
	if resp, _ := answerReq(t, s, ts, answerForm("toolu_1", "a0", "Blue", "a1", "Cat"), "", ""); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a different answer while typing: %d, want 409", resp.StatusCode)
	}
	if got := am.typedSoFar(); len(got) != 1 {
		t.Fatalf("typed %q, want exactly one", got)
	}
	// A read-only host cannot be answered into, even with a fresh sign-in.
	am.fakeMirror.mu.Lock()
	am.fakeMirror.interactive = false
	am.fakeMirror.mu.Unlock()
	if resp, _ := answerReq(t, s, ts, good, "", ""); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a read-only host: %d, want 409", resp.StatusCode)
	}
}

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

// TestTheWatcherEndsWhenNothingIsOpen: once its questions are resolved the
// session's watch ends, and a new question starts another.
func TestTheWatcherEndsWhenNothingIsOpen(t *testing.T) {
	am, s, ts := askServer(t, true)
	register(t, ts)
	region(t, s, ts, `data-ask-state="pending"`)
	am.feed(gateTool, gateQs(t), nil)
	region(t, s, ts, `data-ask-state="declined"`)
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.askWatch.mu.Lock()
		n := len(s.askWatch.running)
		s.askWatch.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the watch did not end with nothing open")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code := openAsk(t, ts, "mmt_ws", "sess-1", "toolu_next", gateInput(t)); code != http.StatusCreated {
		t.Fatal(code)
	}
	am.feed("toolu_next", gateQs(t), map[string]string{"Which color do you prefer?": "Red", "Which pets do you have?": "Cat"})
	region(t, s, ts, `data-ask-state="answered-terminal"`)
}
