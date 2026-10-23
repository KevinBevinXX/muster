package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/metrics"
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
	amu      sync.Mutex
	nudges   map[int64]int
	typed    []string
	priors   []*askq.Progress
	typeErr  error
	noAnswer bool // the host does not advertise AskAnswer
	// cancels records each CancelAsk (host session window), answered with
	// cancelNote and cancelErr; noCancel: the host does not advertise
	// AskCancel.
	cancels    []string
	cancelNote string
	cancelErr  error
	noCancel   bool
	// hold, when set, keeps AnswerWindow typing until it is closed (or the
	// call's ctx ends, which is then its error); typingCtx is the last call's
	// ctx.
	hold      chan struct{}
	typingCtx context.Context
	feedCh    chan mirror.Msg
	watchSeen int
}

func newAskMirror(interactive bool) *askMirror {
	fm := &fakeMirror{online: true, interactive: interactive, v2: &fakeMirrorV2{},
		windows: []mirror.Window{{Session: "main", ID: "@1", Index: 0, Active: true, Claude: true, ClaudeSession: "sess-1"}}}
	return &askMirror{fakeMirror: fm, nudges: map[int64]int{}, feedCh: make(chan mirror.Msg, 16)}
}

func (a *askMirror) Hosts(ctx context.Context) []mirror.HostView {
	hs := a.fakeMirror.Hosts(ctx)
	a.amu.Lock()
	defer a.amu.Unlock()
	for i := range hs {
		hs[i].AskAnswer = hs[i].Online && hs[i].Interactive && !a.noAnswer
		hs[i].AskCancel = hs[i].Online && hs[i].Interactive && !a.noCancel
	}
	return hs
}

func (a *askMirror) CancelAsk(ctx context.Context, host, session, window string) (string, error) {
	a.amu.Lock()
	defer a.amu.Unlock()
	a.cancels = append(a.cancels, host+" "+session+" "+window)
	return a.cancelNote, a.cancelErr
}

func (a *askMirror) cancelsSoFar() []string {
	a.amu.Lock()
	defer a.amu.Unlock()
	return append([]string(nil), a.cancels...)
}

// cancelReq posts a card's dismissal, as answerReq posts its answers.
func cancelReq(t *testing.T, s *Server, ts *httptest.Server, tool, cookie, origin string) (*http.Response, string) {
	t.Helper()
	form := url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}, "tool": {tool}}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+ui.MirrorAskCancelPath, strings.NewReader(form.Encode()))
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

// TestCancelQuestionIsOneConfirmedEscape: the card offers "Cancel question"
// with a dialog naming the window; the dismissal goes to the host for exactly
// this window — with NO step-up (an operator decision), a stale sign-in
// dismisses — the card says Esc was sent (or the host's note: still showing)
// and offers no second Cancel, and the CLI's record of the call ending
// unanswered resolves it as any dismissal. The gates, each sent nothing:
// another origin, another owner, a read-only host, a host that does not
// advertise it (no control either), a question being typed into, one already
// resolved; a host's refusal is said as what happened.
func TestCancelQuestionIsOneConfirmedEscape(t *testing.T) {
	am, s, ts := askServer(t, true)
	register(t, ts)
	card := region(t, s, ts, `data-ask-state="pending"`)
	for _, want := range []string{"Cancel question", "data-ask-cancel-dialog", "Send Esc to main:0? The question is dismissed.", `action="` + ui.MirrorAskCancelPath + `"`} {
		if !strings.Contains(card, want) {
			t.Fatalf("the pending card lacks %q:\n%s", want, card)
		}
	}
	for _, c := range []struct {
		name, origin string
		status       int
	}{{"another origin", "https://evil.example", http.StatusForbidden}} {
		if resp, body := cancelReq(t, s, ts, gateTool, "", c.origin); resp.StatusCode != c.status {
			t.Errorf("%s: %d %q", c.name, resp.StatusCode, body)
		}
	}
	if resp, _ := cancelReq(t, s, ts, "toolu_9", "", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown call: %d", resp.StatusCode)
	}
	// Another owner, same-origin: not found.
	form := url.Values{"host": {"ws"}, "session": {"main"}, "window": {"@1"}, "tool": {gateTool}}
	req := httptest.NewRequest(http.MethodPost, ui.MirrorAskCancelPath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleMirrorAskCancel(rec, req.WithContext(authz.WithPrincipal(context.Background(), authz.Principal{UserID: 2, Role: authz.RoleMember, Via: authz.ViaSession})))
	if rec.Code != http.StatusNotFound {
		t.Errorf("user 2's dismissal: %d", rec.Code)
	}
	// A host that does not advertise it: no control, and refused.
	am.amu.Lock()
	am.noCancel = true
	am.amu.Unlock()
	if _, c := mirrorGet(t, s, ts, asksPath); strings.Contains(c, "Cancel question") {
		t.Fatalf("a host that cannot dismiss is offered Cancel:\n%s", c)
	}
	if resp, body := cancelReq(t, s, ts, gateTool, "", ""); resp.StatusCode != http.StatusConflict || !strings.Contains(body, "cannot dismiss a question") {
		t.Errorf("a host that cannot dismiss: %d %q", resp.StatusCode, body)
	}
	am.amu.Lock()
	am.noCancel = false
	am.amu.Unlock()
	// A host's refusal: said as what happened.
	am.amu.Lock()
	am.cancelErr = fmt.Errorf("%w: x", mirror.ErrAskBusy)
	am.amu.Unlock()
	if resp, body := cancelReq(t, s, ts, gateTool, "", ""); resp.StatusCode != http.StatusConflict || !strings.Contains(body, "An answer is being typed into this terminal — nothing was sent.") {
		t.Errorf("a host's refusal: %d %q", resp.StatusCode, body)
	}
	am.amu.Lock()
	am.cancelErr = nil
	am.amu.Unlock()
	if got := am.cancelsSoFar(); len(got) != 1 {
		t.Fatalf("dismissals sent: %q, want only the refused one", got)
	}
	// The dismissal, on a STALE sign-in: no step-up.
	resp, body := cancelReq(t, s, ts, gateTool, staleCookieFor(s), "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Ask-State") != ui.AskStatePending || !strings.Contains(body, "Esc sent — the question left the terminal&#39;s screen.") ||
		!strings.Contains(body, `data-ask-sent="gone"`) ||
		strings.Contains(body, "Cancel question") || strings.Contains(body, "data-ask-form") {
		t.Fatalf("the dismissal: %d %s\n%s", resp.StatusCode, resp.Header.Get("X-Ask-State"), body)
	}
	if got := am.cancelsSoFar(); len(got) != 2 || got[1] != "ws main @1" {
		t.Fatalf("dismissals sent: %q", got)
	}
	am.feed(gateTool, gateQs(t), nil)
	card = region(t, s, ts, `data-ask-state="declined"`)
	if strings.Contains(card, "Cancel question") {
		t.Fatalf("the dismissed card:\n%s", card)
	}
	// Resolved: refused with the card, nothing sent.
	if resp, _ := cancelReq(t, s, ts, gateTool, "", ""); resp.StatusCode != http.StatusConflict || resp.Header.Get("X-Ask-State") != ui.AskStateDeclined {
		t.Fatalf("a resolved question: %d %s", resp.StatusCode, resp.Header.Get("X-Ask-State"))
	}
	// The picker still showing after the Escape: a success, with the host's
	// note (no second Escape is the host's: askdrive.Cancel).
	amn, sn, tsn := askServer(t, true)
	register(t, tsn)
	region(t, sn, tsn, `data-ask-state="pending"`)
	amn.amu.Lock()
	amn.cancelNote = "Esc sent; the terminal still shows the question."
	amn.amu.Unlock()
	// The question is still the person's: the form and Cancel stay under
	// the note, and the card is marked so the next poll replaces it.
	if resp, body := cancelReq(t, sn, tsn, gateTool, "", ""); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Esc sent; the terminal still shows the question.") ||
		!strings.Contains(body, "data-ask-form") || !strings.Contains(body, "Cancel question") || !strings.Contains(body, `data-ask-sent="showing"`) {
		t.Errorf("still showing: %d %q", resp.StatusCode, body)
	}
	// While an answer is being typed: refused, nothing sent.
	am2, s2, ts2 := askServer(t, true)
	register(t, ts2)
	region(t, s2, ts2, `data-ask-state="pending"`)
	am2.amu.Lock()
	am2.hold = make(chan struct{})
	am2.amu.Unlock()
	go func() { _, _ = answerReq(t, s2, ts2, answerForm(gateTool, "a0", "Red", "a1", "Cat"), "", "") }()
	region(t, s2, ts2, `data-ask-state="typing"`)
	if resp, _ := cancelReq(t, s2, ts2, gateTool, "", ""); resp.StatusCode != http.StatusConflict || resp.Header.Get("X-Ask-State") != ui.AskStateTyping {
		t.Errorf("while typing: %d %s", resp.StatusCode, resp.Header.Get("X-Ask-State"))
	}
	close(am2.hold)
	if got := am2.cancelsSoFar(); len(got) != 0 {
		t.Fatalf("dismissed while typing: %q", got)
	}
	// A read-only host: refused, nothing sent, no control.
	am3, s3, ts3 := askServer(t, false)
	register(t, ts3)
	if c := region(t, s3, ts3, `data-ask-state="pending"`); strings.Contains(c, "Cancel question") {
		t.Fatalf("a read-only host is offered Cancel:\n%s", c)
	}
	if resp, _ := cancelReq(t, s3, ts3, gateTool, "", ""); resp.StatusCode != http.StatusConflict || len(am3.cancelsSoFar()) != 0 {
		t.Fatalf("a read-only host: %d", resp.StatusCode)
	}
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

func (a *askMirror) AnswerWindow(ctx context.Context, host, session, window string, qs []askq.Question, cs []askq.Choice, prior *askq.Progress) error {
	b, _ := json.Marshal(cs)
	a.amu.Lock()
	a.typed = append(a.typed, host+" "+session+" "+window+" "+string(b))
	a.priors = append(a.priors, prior)
	hold, err := a.hold, a.typeErr
	a.typingCtx = ctx
	a.amu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
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
		case m := <-a.feedCh:
			if err := out(m); err != nil {
				return err
			}
		}
	}
}

// feed is the CLI writing the call's result: answers (nil = dismissed).
func (a *askMirror) feed(tool string, qs []askq.Question, answers map[string]string) {
	a.feedFrom("sess-1", tool, qs, answers)
}

// feedFrom is feed from the transcript of Claude session gen.
func (a *askMirror) feedFrom(gen, tool string, qs []askq.Question, answers map[string]string) {
	ev := cctranscript.Event{Kind: cctranscript.KindToolResult, ToolID: tool, OK: answers != nil}
	if answers != nil {
		ev.Ask = &cctranscript.Ask{Questions: qs, Answers: answers}
	}
	a.feedCh <- mirror.Msg{Type: mirror.TypeEvents, Gen: gen, Events: []cctranscript.Event{
		{Kind: cctranscript.KindToolCall, ToolID: tool, Tool: askq.ToolName, Ask: &cctranscript.Ask{Questions: qs}},
		ev,
	}}
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
	// A double submit while it is being typed: the typing card, nothing typed
	// twice.
	if resp, _ := answerReq(t, s, ts, answerForm(gateTool, "a0", "Red", "a1", "Fish", "a1", "Dog"), "", ""); resp.StatusCode != http.StatusOK ||
		resp.Header.Get("X-Ask-State") != ui.AskStateTyping {
		t.Fatalf("a double submit while typing: %d %s", resp.StatusCode, resp.Header.Get("X-Ask-State"))
	}
	if got := am.typedSoFar(); len(got) != 1 {
		t.Fatalf("a double submit while typing typed again: %q", got)
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
		!strings.Contains(body, "no longer on the terminal") || strings.Contains(body, "data-ask-form") {
		t.Fatalf("a typing with no picker: %d %s\n%s", resp.StatusCode, resp.Header.Get("X-Ask-State"), body)
	}
	am2.feed(gateTool, gateQs(t), map[string]string{"Which color do you prefer?": "Blue", "Which pets do you have?": "Cat"})
	region(t, s2, ts2, `data-ask-state="answered-terminal"`)
}

// TestAnotherConversationsResultIsNotTheAnswer: the window now runs another
// Claude session (its transcript, its generation); a result there with the
// same call id resolves nothing. The control: the question's own session's
// result does.
func TestAnotherConversationsResultIsNotTheAnswer(t *testing.T) {
	am, s, ts := askServer(t, true)
	register(t, ts)
	region(t, s, ts, `data-ask-state="pending"`)
	am.feedFrom("sess-other", gateTool, gateQs(t), map[string]string{"Which color do you prefer?": "Blue", "Which pets do you have?": "Cat"})
	time.Sleep(300 * time.Millisecond)
	if got := s.Questions().Pending(1); len(got) != 1 {
		t.Fatalf("another conversation's result resolved the question: %+v", got)
	}
	am.feed(gateTool, gateQs(t), map[string]string{"Which color do you prefer?": "Blue", "Which pets do you have?": "Cat"})
	region(t, s, ts, `data-ask-state="answered-terminal"`)
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
	if resp, body := answerReq(t, s, ts, good, "", ""); resp.StatusCode != http.StatusConflict || !strings.Contains(body, mirror.ErrInteractiveRefused.Error()) {
		t.Fatalf("a read-only host: %d %q, want 409 saying it is read-only", resp.StatusCode, body)
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

// TestTheRenderedViewIsToldAtOnce: the window's rendered-view socket carries a
// {"type":"asks"} frame when a question registers, so the page reads its
// region then rather than on its next poll. The control: nothing is sent
// before the question exists.
func TestTheRenderedViewIsToldAtOnce(t *testing.T) {
	_, s, ts := askServer(t, true)
	c, _, err := dialTerm(t, s, ts, ui.MirrorRef{Host: "ws", Session: "main", Window: "@1"}.ViewWSPath())
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	frames := make(chan string, 16)
	go func() {
		for {
			_, data, err := c.Read(context.Background())
			if err != nil {
				close(frames)
				return
			}
			frames <- string(data)
		}
	}()
	asks := func(within time.Duration) bool {
		deadline := time.After(within)
		for {
			select {
			case f, ok := <-frames:
				if !ok {
					t.Fatal("the view's socket closed")
				}
				if strings.Contains(f, `"type":"asks"`) {
					return true
				}
			case <-deadline:
				return false
			}
		}
	}
	// The socket is subscribed once its handler has run; wait for that.
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.askWatch.mu.Lock()
		n := len(s.askWatch.subs)
		s.askWatch.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the view's socket never subscribed to its window's questions")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if asks(200 * time.Millisecond) {
		t.Fatal("an asks frame before any question")
	}
	register(t, ts)
	if !asks(2 * time.Second) {
		t.Fatal("the view was not told a question registered")
	}
}

// TestADroppedPageDoesNotEndTheTyping: the page that sent an answer goes away
// while the host is typing it. The wait goes on (its ctx is not the
// request's): the question stays "typing", not failed — so no second answer
// can start a drive into the same picker — and the host's outcome, when it
// comes, is what counts.
func TestADroppedPageDoesNotEndTheTyping(t *testing.T) {
	am, s, ts := askServer(t, true)
	register(t, ts)
	region(t, s, ts, `data-ask-state="pending"`)
	hold := make(chan struct{})
	am.amu.Lock()
	am.hold = hold
	am.amu.Unlock()
	form := answerForm(gateTool, "a0", "Red", "a1", "Cat")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+ui.MirrorAskPath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", sessionCookieFor(s))
	req.Header.Set("Origin", ts.URL)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		t.Fatalf("the held answer returned %d", resp.StatusCode)
	}
	time.Sleep(300 * time.Millisecond) // the server sees the page gone
	am.amu.Lock()
	tctx := am.typingCtx
	am.amu.Unlock()
	if tctx == nil || tctx.Err() != nil {
		t.Fatalf("the typing's wait ended with the page: %v", tctx)
	}
	if e, _ := s.Questions().Find(askq.Key{Owner: 1, Source: askSourceMirror, Scope: "ws", Session: "sess-1", ToolUseID: gateTool}); e.State != askq.Typing {
		t.Fatalf("with the page gone the question is %s, want typing", e.State)
	}
	// Another answer meanwhile: refused, nothing more typed.
	if resp, _ := answerReq(t, s, ts, answerForm(gateTool, "a0", "Blue", "a1", "Cat"), "", ""); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a second answer while the first is typed: %d", resp.StatusCode)
	}
	if got := am.typedSoFar(); len(got) != 1 {
		t.Fatalf("typed %q, want one drive", got)
	}
	close(hold)
	am.feed(gateTool, gateQs(t), map[string]string{"Which color do you prefer?": "Red", "Which pets do you have?": "Cat"})
	region(t, s, ts, `data-ask-state="answered"`)
}

// TestAnUnknownOutcomeHoldsTheQuestion: the host's outcome never came back
// (a timeout): the card says the picker may be partly filled in, and a new
// answer is refused — the host may still be typing. The control: a refusal
// the host reported (no picker) is retried.
func TestAnUnknownOutcomeHoldsTheQuestion(t *testing.T) {
	am, s, ts := askServer(t, true)
	register(t, ts)
	region(t, s, ts, `data-ask-state="pending"`)
	am.amu.Lock()
	am.typeErr = mirror.ErrAskTimeout
	am.amu.Unlock()
	resp, body := answerReq(t, s, ts, answerForm(gateTool, "a0", "Red", "a1", "Cat"), "", "")
	if resp.StatusCode != http.StatusConflict || resp.Header.Get("X-Ask-State") != ui.AskStateFailed || !strings.Contains(body, "may be partly filled in") {
		t.Fatalf("a typing with no outcome: %d %s\n%s", resp.StatusCode, resp.Header.Get("X-Ask-State"), body)
	}
	am.amu.Lock()
	am.typeErr = nil
	am.amu.Unlock()
	if resp, _ := answerReq(t, s, ts, answerForm(gateTool, "a0", "Blue", "a1", "Cat"), "", ""); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a new answer while the host may still type: %d, want 409", resp.StatusCode)
	}
	if got := am.typedSoFar(); len(got) != 1 {
		t.Fatalf("typed %q, want one drive", got)
	}

	am2, s2, ts2 := askServer(t, true)
	register(t, ts2)
	region(t, s2, ts2, `data-ask-state="pending"`)
	am2.amu.Lock()
	am2.typeErr = fmt.Errorf("%w: gone", mirror.ErrAskNoPicker)
	am2.amu.Unlock()
	_, _ = answerReq(t, s2, ts2, answerForm(gateTool, "a0", "Red", "a1", "Cat"), "", "")
	am2.amu.Lock()
	am2.typeErr = nil
	am2.amu.Unlock()
	if resp, _ := answerReq(t, s2, ts2, answerForm(gateTool, "a0", "Blue", "a1", "Cat"), "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("a retry after a reported refusal: %d, want 200", resp.StatusCode)
	}
}

// TestALostAnswerIsContinuedFromTheCard: the operator's report — a
// multi-question answer "gets stuck mid answer with 'someone else started
// answering'". The host typed part of the answer and lost the picker; the card
// says so (not that someone else is answering), shows the answer it was
// typing, and offers "Try again" as a FIXED retry — that answer's fields,
// nothing editable; trying again posts exactly them and hands the host what
// was typed, so it continues. The controls: someone else answering in the
// terminal is said as that, with no form — and, after muster's keys reached
// the picker, that it may be partly filled in.
func TestALostAnswerIsContinuedFromTheCard(t *testing.T) {
	am, s, ts := askServer(t, true)
	register(t, ts)
	region(t, s, ts, `data-ask-state="pending"`)
	am.amu.Lock()
	am.typeErr = mirror.WithAskKeys(fmt.Errorf("%w: after key 5", mirror.ErrAskLost), 4)
	am.amu.Unlock()
	resp, body := answerReq(t, s, ts, answerForm(gateTool, "a0", "Blue", "a1", "Fish", "a1", "Dog"), "", "")
	if resp.StatusCode != http.StatusConflict || resp.Header.Get("X-Ask-State") != ui.AskStateFailed {
		t.Fatalf("a lost typing: %d %s\n%s", resp.StatusCode, resp.Header.Get("X-Ask-State"), body)
	}
	for _, want := range []string{"lost track of the terminal", "muster continues from where it stopped", "data-ask-retry", "Try again",
		"Answer: </span>Blue", "Answer: </span>Dog, Fish"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the lost card lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Someone") || strings.Contains(body, `type="radio"`) || strings.Contains(body, `type="checkbox"`) || strings.Contains(body, "<textarea") {
		t.Fatalf("the lost card offers an editable answer:\n%s", body)
	}
	// Try again posts the card's own fields.
	retry := url.Values{}
	for _, m := range regexp.MustCompile(`<input type="hidden" name="([^"]+)" value="([^"]*)">`).FindAllStringSubmatch(body, -1) {
		retry.Add(m[1], html.UnescapeString(m[2]))
	}
	if !reflect.DeepEqual(retry["a0"], []string{"Blue"}) || !reflect.DeepEqual(retry["a1"], []string{"Dog", "Fish"}) || retry.Get("tool") != gateTool {
		t.Fatalf("the fixed retry posts %v", retry)
	}
	am.amu.Lock()
	am.typeErr = nil
	am.amu.Unlock()
	resp, body = answerReq(t, s, ts, retry, "", "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Ask-State") != ui.AskStateTyping {
		t.Fatalf("trying again: %d %s\n%s", resp.StatusCode, resp.Header.Get("X-Ask-State"), body)
	}
	am.amu.Lock()
	priors := append([]*askq.Progress(nil), am.priors...)
	am.amu.Unlock()
	want := &askq.Progress{Choices: []askq.Choice{{Picks: []int{1}}, {Picks: []int{1, 2}}}, Keys: 4}
	if len(priors) != 2 || priors[0] != nil || !reflect.DeepEqual(priors[1], want) {
		t.Fatalf("the host was handed %+v then %+v, want nothing then %+v", priors[0], priors[len(priors)-1], want)
	}

	// A retry that types nothing (busy: another drive has the pane) keeps the
	// record: the next retry is still handed it, and continues.
	am4, s4, ts4 := askServer(t, true)
	register(t, ts4)
	region(t, s4, ts4, `data-ask-state="pending"`)
	form := answerForm(gateTool, "a0", "Blue", "a1", "Fish", "a1", "Dog")
	for _, e := range []error{mirror.WithAskKeys(fmt.Errorf("%w: x", mirror.ErrAskLost), 4), fmt.Errorf("%w: x", mirror.ErrAskBusy), nil} {
		am4.amu.Lock()
		am4.typeErr = e
		am4.amu.Unlock()
		_, _ = answerReq(t, s4, ts4, form, "", "")
	}
	am4.amu.Lock()
	p4 := append([]*askq.Progress(nil), am4.priors...)
	am4.amu.Unlock()
	if len(p4) != 3 || !reflect.DeepEqual(p4[1], want) || !reflect.DeepEqual(p4[2], want) {
		t.Fatalf("after a busy retry the host was handed %+v, want %+v again", p4, want)
	}

	// Someone else typed in the terminal: said as that, no form; after keys
	// of muster's reached the picker, that it may be partly filled in.
	for _, keys := range []int{0, 3} {
		am2, s2, ts2 := askServer(t, true)
		register(t, ts2)
		region(t, s2, ts2, `data-ask-state="pending"`)
		am2.amu.Lock()
		am2.typeErr = mirror.WithAskKeys(fmt.Errorf("%w: x", mirror.ErrAskTouched), keys)
		am2.amu.Unlock()
		_, body = answerReq(t, s2, ts2, answerForm(gateTool, "a0", "Blue", "a1", "Fish"), "", "")
		if !strings.Contains(body, "Someone else has started answering it in the terminal") || strings.Contains(body, "data-ask-form") ||
			strings.Contains(body, "lost track") || strings.Contains(body, "may be partly filled in") != (keys > 0) {
			t.Fatalf("the touched card after %d keys:\n%s", keys, body)
		}
	}
}

// TestEachFailureSaysWhatTheTerminalHas: the card's note for every outcome —
// "partly filled in" exactly where keys may have been typed.
func TestEachFailureSaysWhatTheTerminalHas(t *testing.T) {
	for _, c := range []struct {
		err     error
		says    string
		partial bool
	}{
		{fmt.Errorf("%w: x", mirror.ErrAskNoPicker), "no longer on the terminal's screen", false},
		{fmt.Errorf("%w: x", mirror.ErrAskTouched), "Someone else has started answering it in the terminal", false},
		{fmt.Errorf("%w: x", mirror.ErrAskUnreadable), "Couldn't read this question off the terminal's screen as the one muster was sent — answer it in the terminal.", false},
		{fmt.Errorf("%w: %w: x", mirror.ErrAskUnreadable, mirror.ErrAskLost), "the question went off the terminal's screen while typing", true},
		{fmt.Errorf("%w: x", mirror.ErrAskBusy), "Another answer is being typed into this terminal", false},
		{fmt.Errorf("%w: x", mirror.ErrAskUnrecognised), "Couldn't recognise the terminal's picker (Claude Code may have changed) — answer it in the terminal.", false},
		{fmt.Errorf("%w: x", mirror.ErrAskLost), "lost track of the terminal's picker while typing", true},
		{fmt.Errorf("%w: %w", mirror.ErrAskNotSent, mirror.ErrHostOffline), "The host is offline", false},
		{mirror.ErrHostDisconnected, "went offline while typing", true},
		{mirror.ErrAskTimeout, "Couldn't type it in.", true},
	} {
		got := askTypeFailure(c.err, false, false)
		if !strings.Contains(got, c.says) || strings.Contains(got, "partly filled in") != c.partial {
			t.Errorf("%v: %q", c.err, got)
		}
	}
	// Lost with what was typed recorded: try again, it continues.
	if got := askTypeFailure(fmt.Errorf("%w: x", mirror.ErrAskLost), true, false); got != askNoteLost {
		t.Errorf("lost, resumable: %q", got)
	}
	// Not as muster left it, after keys of muster's reached the picker.
	if got := askTypeFailure(fmt.Errorf("%w: x", mirror.ErrAskTouched), false, true); got != askNoteTouched+askPartlyFilled {
		t.Errorf("touched after muster's keys: %q", got)
	}
}

// TestAnUnrecognisedPickerIsCounted: the outcome counter moves for an
// unrecognised picker (the signal that Claude Code's layout changed).
func TestAnUnrecognisedPickerIsCounted(t *testing.T) {
	for err, want := range map[error]string{
		fmt.Errorf("%w: x", mirror.ErrAskBusy):                              "busy",
		fmt.Errorf("%w: x", mirror.ErrAskTouched):                           "touched",
		fmt.Errorf("%w: x", mirror.ErrAskUnreadable):                        "unreadable",
		fmt.Errorf("%w: %w: x", mirror.ErrAskUnreadable, mirror.ErrAskLost): "unreadable",
		nil: "typed",
	} {
		if got := askTypingResult(err); got != want {
			t.Errorf("%v: label %q, want %q", err, got, want)
		}
	}
	am, s, ts := askServer(t, true)
	register(t, ts)
	region(t, s, ts, `data-ask-state="pending"`)
	before := testutil.ToFloat64(metrics.MirrorAskTyping.WithLabelValues("unrecognised"))
	am.amu.Lock()
	am.typeErr = fmt.Errorf("%w: x", mirror.ErrAskUnrecognised)
	am.amu.Unlock()
	if _, body := answerReq(t, s, ts, answerForm(gateTool, "a0", "Red", "a1", "Cat"), "", ""); !strings.Contains(body, "Couldn&#39;t recognise the terminal&#39;s picker") {
		t.Fatalf("the card:\n%s", body)
	}
	if got := testutil.ToFloat64(metrics.MirrorAskTyping.WithLabelValues("unrecognised")) - before; got != 1 {
		t.Fatalf("the unrecognised count moved by %v", got)
	}
}

// TestAGoneSessionsQuestionExpires: the Claude session leaves its host's
// windows (Claude exited); after the grace its question expires, saying so,
// and leaves the attention queue. The control: while the window is listed, it
// stays open past the same grace.
func TestAGoneSessionsQuestionExpires(t *testing.T) {
	am, s, ts := askServer(t, true)
	s.askWatch.mu.Lock()
	s.askWatch.grace = 400 * time.Millisecond
	s.askWatch.mu.Unlock()
	register(t, ts)
	region(t, s, ts, `data-ask-state="pending"`)
	am.fakeMirror.mu.Lock()
	ws := am.fakeMirror.windows
	am.fakeMirror.mu.Unlock()
	setWindows := func(w []mirror.Window) {
		am.fakeMirror.mu.Lock()
		am.fakeMirror.windows = w
		am.fakeMirror.mu.Unlock()
	}
	gone := []mirror.Window{{Session: "main", ID: "@1", Index: 0, Active: true}} // Claude is not in it
	// A blip shorter than the grace (a host reconnecting): still open.
	setWindows(gone)
	time.Sleep(100 * time.Millisecond)
	setWindows(ws)
	time.Sleep(800 * time.Millisecond)
	if got := s.Questions().Pending(1); len(got) != 1 {
		t.Fatalf("a listed session's question did not stay open: %+v", got)
	}
	setWindows(gone)
	defer func() {
		am.fakeMirror.mu.Lock()
		am.fakeMirror.windows = ws
		am.fakeMirror.mu.Unlock()
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(s.Questions().Pending(1)) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("a gone session's question is still open")
		}
		time.Sleep(20 * time.Millisecond)
	}
	e, _ := s.Questions().Find(askq.Key{Owner: 1, Source: askSourceMirror, Scope: "ws", Session: "sess-1", ToolUseID: gateTool})
	if e.State != askq.Expired || e.Note != askGoneNote {
		t.Fatalf("the gone session's question: %+v", e)
	}
	// The session comes back and its transcript shows the terminal's answer:
	// the expired card still flips to it.
	setWindows(ws)
	am.feed(gateTool, gateQs(t), map[string]string{"Which color do you prefer?": "Green", "Which pets do you have?": "Dog"})
	region(t, s, ts, `data-ask-state="answered-terminal"`)
}

// TestAHostAwayIsNotASessionGone: the host goes offline for far longer than
// the grace (its reconnect backoff reaches a minute; scaled here: 4× the
// grace) and comes back still listing the session: the question is still
// live. Only time the host is ONLINE without the session counts.
func TestAHostAwayIsNotASessionGone(t *testing.T) {
	am, s, ts := askServer(t, true)
	s.askWatch.mu.Lock()
	s.askWatch.grace = 150 * time.Millisecond
	s.askWatch.mu.Unlock()
	register(t, ts)
	region(t, s, ts, `data-ask-state="pending"`)
	setOnline := func(on bool) {
		am.fakeMirror.mu.Lock()
		am.fakeMirror.online = on
		am.fakeMirror.mu.Unlock()
	}
	setOnline(false)
	time.Sleep(600 * time.Millisecond)
	setOnline(true)
	time.Sleep(300 * time.Millisecond)
	if got := s.Questions().Pending(1); len(got) != 1 {
		t.Fatalf("a question expired while its host was away: %+v", got)
	}
	region(t, s, ts, `data-ask-state="pending"`)
}

// TestANewerQuestionReplacesTheOldCard: Claude asks again in the same
// session; the older card says it was replaced, and an answer to it is
// refused with nothing typed.
func TestANewerQuestionReplacesTheOldCard(t *testing.T) {
	am, s, ts := askServer(t, true)
	register(t, ts)
	region(t, s, ts, `data-ask-state="pending"`)
	if code := openAsk(t, ts, "mmt_ws", "sess-1", "toolu_again", gateInput(t)); code != http.StatusCreated {
		t.Fatal(code)
	}
	card := region(t, s, ts, `data-ask-state="superseded"`)
	if !strings.Contains(card, "newer question") {
		t.Fatalf("the replaced card:\n%s", card)
	}
	resp, _ := answerReq(t, s, ts, answerForm(gateTool, "a0", "Red", "a1", "Cat"), "", "")
	if resp.StatusCode != http.StatusConflict || resp.Header.Get("X-Ask-State") != ui.AskStateSuperseded {
		t.Fatalf("an answer to the replaced card: %d %s", resp.StatusCode, resp.Header.Get("X-Ask-State"))
	}
	if got := am.typedSoFar(); len(got) != 0 {
		t.Fatalf("typed into a replaced question: %q", got)
	}
}
