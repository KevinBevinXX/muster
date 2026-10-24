package api

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// A MESSAGE TO A DISPATCHED AGENT AS A FORM POST (agent_send.go), through the
// real mux: the composer on the agent's Terminal page, and the chat form when
// its socket cannot carry the message.
// ---------------------------------------------------------------------------

// sendAgents serves two agents and keeps their chat in memory: keen-wren
// (claude-code, the kind with a terminal) and plain-otter (gateway).
type sendAgents struct {
	stubAgentsStore
	mu     sync.Mutex
	status map[string]string
	msgs   []agents.ChatMessage
}

func newSendAgents() *sendAgents {
	return &sendAgents{status: map[string]string{"keen-wren": agents.StatusRunning, "plain-otter": agents.StatusRunning}}
}

func (s *sendAgents) GetByName(_ context.Context, name string) (agents.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch name {
	case "keen-wren":
		return agents.Agent{ID: 7, Name: name, Kind: agents.KindClaudeCode, CCAccount: "alpha", Status: s.status[name]}, nil
	case "plain-otter":
		return agents.Agent{ID: 8, Name: name, Kind: agents.KindGateway, Status: s.status[name]}, nil
	}
	return agents.Agent{}, pgx.ErrNoRows
}

// List and LastMessageByAgentIDs are what the session switcher reads to draw
// the pages' bar (switcher.go): the two agents above, no transcript times.
func (s *sendAgents) List(ctx context.Context) ([]agents.Agent, error) {
	var out []agents.Agent
	for _, n := range []string{"keen-wren", "plain-otter"} {
		a, _ := s.GetByName(ctx, n)
		out = append(out, a)
	}
	return out, nil
}

func (s *sendAgents) LastMessageByAgentIDs(context.Context, []int64) (map[int64]time.Time, error) {
	return map[int64]time.Time{}, nil
}

// Each agent has one session: 41 is keen-wren's, 52 plain-otter's.
func (s *sendAgents) LatestOrCreateSession(_ context.Context, agentID int64, _ string) (agents.ChatSession, error) {
	if agentID == 7 {
		return agents.ChatSession{ID: 41, AgentID: 7, SessionKey: "key-41"}, nil
	}
	return agents.ChatSession{ID: 52, AgentID: 8, SessionKey: "key-52"}, nil
}

func (s *sendAgents) GetSession(_ context.Context, id int64) (agents.ChatSession, error) {
	switch id {
	case 41:
		return agents.ChatSession{ID: 41, AgentID: 7, SessionKey: "key-41"}, nil
	case 52:
		return agents.ChatSession{ID: 52, AgentID: 8, SessionKey: "key-52"}, nil
	}
	return agents.ChatSession{}, pgx.ErrNoRows
}

func (s *sendAgents) ListSessions(_ context.Context, agentID int64) ([]agents.ChatSession, error) {
	sess, _ := s.LatestOrCreateSession(context.Background(), agentID, "")
	return []agents.ChatSession{sess}, nil
}

func (s *sendAgents) MarkSessionRead(context.Context, int64) error { return nil }

func (s *sendAgents) AddChatMessage(_ context.Context, m agents.ChatMessage) (agents.ChatMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m.ID = int64(len(s.msgs) + 1)
	s.msgs = append(s.msgs, m)
	return m, nil
}

func (s *sendAgents) ListRecentChatMessages(_ context.Context, sessionID int64, _ int) ([]agents.ChatMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []agents.ChatMessage
	for _, m := range s.msgs {
		if m.SessionID == sessionID {
			out = append(out, m)
		}
	}
	return out, nil
}

// transcript is session's stored messages as "role: content".
func (s *sendAgents) transcript(session int64) []string {
	msgs, _ := s.ListRecentChatMessages(context.Background(), session, 0)
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Role + ": " + m.Content
	}
	return out
}

// turnGateway plays an agent's gateway: each turn is recorded (agent, session
// key, text), streams its reply as two text deltas, and answers — or fails.
type turnGateway struct {
	mu    sync.Mutex
	turns []string // "agent key text"
	err   error
}

func (g *turnGateway) Chat(context.Context, agents.Agent, string, string, func(string)) (string, error) {
	return "", errors.New("turnGateway: the tool-less path is not expected here")
}

func (g *turnGateway) ChatWithTools(_ context.Context, a agents.Agent, key, _, text string, _ []agents.ToolDef, _ agents.ToolDispatch, emit agents.StreamEmit) (string, error) {
	g.mu.Lock()
	g.turns = append(g.turns, a.Name+" "+key+" "+text)
	err := g.err
	g.mu.Unlock()
	if err != nil {
		return "", err
	}
	reply := "answer to: " + text
	if emit != nil {
		emit(agents.StreamEvent{Kind: "text", Text: "answer to: "})
		emit(agents.StreamEvent{Kind: "text", Text: text})
	}
	return reply, nil
}

func (g *turnGateway) seen() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.turns...)
}

func sendServer(t *testing.T, store *sendAgents, gw Gateway) (*Server, *httptest.Server) {
	t.Helper()
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{
		Notes:           stubNotesStore{},
		Agents:          store,
		SessionLiveness: stubLiveness{},
		Provisioner:     newFakeTerminal(),
		Gateway:         gw,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

// postAgent posts form to an agent's send route as a signed-in browser on the
// server's own origin; nav makes it the browser's own form submission.
func postAgent(t *testing.T, s *Server, ts *httptest.Server, name string, form url.Values, nav bool) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+ui.AgentSendPath(name), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", sessionCookieFor(s))
	req.Header.Set("Origin", ts.URL)
	if nav {
		req.Header.Set("Sec-Fetch-Dest", "document")
	} else {
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func eventuallyAPI(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAMessagePostedToAnAgentRunsTheTurnTheSocketRuns: the form post is
// answered at once (204 to the script; to the browser a redirect back to the
// page it came from) and the turn runs behind it — against the agent's gateway,
// under the chat session named, with the user's message and the reply's parts
// stored exactly as the chat socket stores them.
//
// 🔴 A REGRESSION TEST: on the commit before this the route does not exist
// (404 from the mux), which is what "the terminal page has no message box"
// looks like to a request.
func TestAMessagePostedToAnAgentRunsTheTurnTheSocketRuns(t *testing.T) {
	store, gw := newSendAgents(), &turnGateway{}
	s, ts := sendServer(t, store, gw)

	resp := postAgent(t, s, ts, "keen-wren", url.Values{"text": {"build it"}, "session": {"41"}, "from": {"terminal"}}, false)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("the composer's post = %d, want 204", resp.StatusCode)
	}
	eventuallyAPI(t, "the turn to be stored", func() bool { return len(store.transcript(41)) == 2 })
	if got, want := gw.seen(), []string{"keen-wren key-41 build it"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("the gateway was asked %q, want %q", got, want)
	}
	if got := store.transcript(41); got[0] != "user: build it" || got[1] != "assistant: answer to: build it" {
		t.Fatalf("stored transcript = %q", got)
	}

	// The browser's own submission: a redirect back to where it came from.
	for from, back := range map[string]string{
		ui.AgentSendFromTerminal: "/agents/keen-wren/terminal?notice=sent",
		"":                       "/agents/keen-wren?session=41&notice=sent",
	} {
		resp := postAgent(t, s, ts, "keen-wren", url.Values{"text": {"again from " + from}, "session": {"41"}, "from": {from}}, true)
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != back {
			t.Fatalf("from=%q as a navigation = %d → %q; want 303 → %s", from, resp.StatusCode, resp.Header.Get("Location"), back)
		}
	}
	eventuallyAPI(t, "all three turns", func() bool { return len(gw.seen()) == 3 && len(store.transcript(41)) == 6 })

	// A session that is not this agent's is never written to: the agent's own is.
	postAgent(t, s, ts, "keen-wren", url.Values{"text": {"wrong session"}, "session": {"52"}}, false)
	eventuallyAPI(t, "the fourth turn", func() bool { return len(gw.seen()) == 4 })
	if last := gw.seen()[3]; last != "keen-wren key-41 wrong session" {
		t.Fatalf("a post naming another agent's session ran as %q", last)
	}
	if n := len(store.transcript(52)); n != 0 {
		t.Fatalf("another agent's session received %d message(s)", n)
	}
}

// TestAnAgentThatCannotBeSentAMessageIsRefusedAndNothingRuns: every way a send
// is not delivered says so and starts no turn — an agent that is not running
// (stopped, failed, still starting), an empty message, an agent that does not
// exist, a cross-origin post, no session. The control at the end is the same
// post to a running agent, which does start one.
func TestAnAgentThatCannotBeSentAMessageIsRefusedAndNothingRuns(t *testing.T) {
	store, gw := newSendAgents(), &turnGateway{}
	s, ts := sendServer(t, store, gw)
	text := func(resp *http.Response) string { b, _ := io.ReadAll(resp.Body); return strings.TrimSpace(string(b)) }

	for _, status := range []string{agents.StatusStopped, agents.StatusError, agents.StatusProvisioning, agents.StatusPending} {
		store.status["keen-wren"] = status
		resp := postAgent(t, s, ts, "keen-wren", url.Values{"text": {"are you there"}}, false)
		if resp.StatusCode != http.StatusConflict || text(resp) != AgentNotRunningNotice {
			t.Errorf("a %s agent: %d %q; want 409 and the reason", status, resp.StatusCode, text(resp))
		}
		nav := postAgent(t, s, ts, "keen-wren", url.Values{"text": {"are you there"}, "from": {"terminal"}}, true)
		if nav.StatusCode != http.StatusSeeOther || nav.Header.Get("Location") != "/agents/keen-wren/terminal?notice=not-running" {
			t.Errorf("a %s agent, as a navigation: %d → %q", status, nav.StatusCode, nav.Header.Get("Location"))
		}
	}
	store.status["keen-wren"] = agents.StatusRunning

	if resp := postAgent(t, s, ts, "keen-wren", url.Values{"text": {"  \n "}}, false); resp.StatusCode != http.StatusBadRequest || text(resp) != AgentSendEmpty {
		t.Errorf("an empty message: %d", resp.StatusCode)
	}
	if resp := postAgent(t, s, ts, "no-such-agent", url.Values{"text": {"hi"}}, false); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an agent that does not exist: %d, want 404", resp.StatusCode)
	}
	// Cross-origin, and no session at all.
	for name, mutate := range map[string]func(*http.Request){
		"a cross-origin post":     func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
		"a post with no session":  func(r *http.Request) { r.Header.Del("Cookie") },
		"a post with no Origin":   func(r *http.Request) { r.Header.Del("Origin") },
		"the hook token, no user": func(r *http.Request) { r.Header.Del("Cookie"); r.Header.Set("Authorization", "Bearer "+testHookToken) },
	} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+ui.AgentSendPath("keen-wren"), strings.NewReader(url.Values{"text": {"hi"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Cookie", sessionCookieFor(s))
		req.Header.Set("Origin", ts.URL)
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		mutate(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s was answered %d, want a 401/403 refusal", name, resp.StatusCode)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if got := gw.seen(); len(got) != 0 || len(store.transcript(41)) != 0 {
		t.Fatalf("a refused send ran a turn or stored a message: turns %q, transcript %q", got, store.transcript(41))
	}
	// CONTROL: the instrument can see a turn.
	postAgent(t, s, ts, "keen-wren", url.Values{"text": {"now"}}, false)
	eventuallyAPI(t, "CONTROL: an accepted send runs a turn", func() bool { return len(gw.seen()) == 1 })
}

// TestATurnStartedByAPostThatFailsSaysSoInTheConversation: the post has
// already answered "sent" when the turn fails, and there is no socket to say so
// on — so the failure is stored where the reply would have been, with the
// user's message kept.
func TestATurnStartedByAPostThatFailsSaysSoInTheConversation(t *testing.T) {
	store, gw := newSendAgents(), &turnGateway{err: errors.New("the gateway is restarting")}
	s, ts := sendServer(t, store, gw)
	if resp := postAgent(t, s, ts, "keen-wren", url.Values{"text": {"build it"}}, false); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("the post = %d", resp.StatusCode)
	}
	eventuallyAPI(t, "the failure to be stored", func() bool { return len(store.transcript(41)) == 2 })
	got := store.transcript(41)
	if got[0] != "user: build it" || got[1] != "assistant: "+AgentTurnFailedPrefix+"the gateway is restarting" {
		t.Fatalf("stored transcript = %q", got)
	}
}

// TestTheSameMessagePostedToAnAgentTwiceIsOneTurn: the nonce, on the agent's
// route — a double tap, or the chat page handing a message to the browser
// after its socket already carried it, must not run two turns.
func TestTheSameMessagePostedToAnAgentTwiceIsOneTurn(t *testing.T) {
	store, gw := newSendAgents(), &turnGateway{}
	s, ts := sendServer(t, store, gw)
	form := url.Values{"text": {"deploy it"}, "nonce": {"dddddddddddddddddddddddddddddddd"}}
	for i := 0; i < 3; i++ {
		if resp := postAgent(t, s, ts, "keen-wren", form, i == 1); resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("post #%d = %d", i+1, resp.StatusCode)
		}
	}
	eventuallyAPI(t, "the turn", func() bool { return len(gw.seen()) >= 1 })
	// Long enough for a second and a third turn to have started, had they been.
	time.Sleep(300 * time.Millisecond)
	if got := gw.seen(); len(got) != 1 {
		t.Fatalf("the same message posted 3 times ran %d turns: %q", len(got), got)
	}
	form.Set("text", "and then test it")
	postAgent(t, s, ts, "keen-wren", form, false)
	eventuallyAPI(t, "a different message under the same nonce to run", func() bool { return len(gw.seen()) == 2 })
}

// TestAnAgentsTerminalPageHasTheSwitchAndTheComposer: the agent's Terminal page
// carries the Rendered | Terminal switch and the shared composer, posting to
// the agent's send route "from the terminal" with a nonce — enabled for a
// running agent, DISABLED with the reason on screen for one that is not, and
// for a build with no gateway to deliver through. The chat page has the switch
// too, and both documents are cacheable VIEW documents (viewDocument): kept for
// Back and Forward, never by a shared cache.
func TestAnAgentsTerminalPageHasTheSwitchAndTheComposer(t *testing.T) {
	store := newSendAgents()
	s, ts := sendServer(t, store, &turnGateway{})
	get := func(path string) (*http.Response, string) { return mirrorGet(t, s, ts, path) }

	resp, term := get(ui.AgentTerminalPath("keen-wren"))
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != viewDocumentCacheControl {
		t.Fatalf("the terminal page = %d, Cache-Control %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	if got := resp.Header.Get("Referrer-Policy"); got != "same-origin" {
		t.Errorf("the terminal page's Referrer-Policy = %q, want same-origin (its composer is a form the browser may submit itself)", got)
	}
	if !strings.Contains(term, `<a id="view-rendered" href="/agents/keen-wren"`) || !strings.Contains(term, `id="view-terminal" aria-current="page"`) {
		t.Error("the terminal page has no Rendered | Terminal switch")
	}
	if form := tagOf(term, "mv-form"); !strings.Contains(form, `method="post" action="/agents/keen-wren/send?from=terminal"`) {
		t.Errorf("the terminal page's composer does not post to the agent's send route: %s", form)
	}
	f := composerFields(t, term)
	if f["from"] != "terminal" || len(f["nonce"]) != 32 || f["host"] != "" {
		t.Errorf("the composer's hidden fields = %v; want from=terminal and a nonce", f)
	}
	if in := tagOf(term, "mv-input"); in == "" || disabledAttr.MatchString(in) || !hasText16px.MatchString(in) {
		t.Errorf("a running agent's composer is not an enabled 16px input: %s", in)
	}
	if strings.Contains(term, `<dialog id="term-confirm"`) {
		t.Error("an agent's terminal carries a step-up dialog it has no use for")
	}
	// The notice of a browser-submitted send.
	if _, noted := get(ui.AgentTerminalPath("keen-wren") + "?notice=sent"); !strings.Contains(noted, ">Sent.</p>") {
		t.Error("the terminal page does not draw the notice it is redirected to with")
	}

	// Not running: disabled, saying why.
	store.status["keen-wren"] = agents.StatusStopped
	_, stopped := get(ui.AgentTerminalPath("keen-wren"))
	if in, send := tagOf(stopped, "mv-input"), tagOf(stopped, "mv-send"); !disabledAttr.MatchString(in) || !disabledAttr.MatchString(send) {
		t.Errorf("a stopped agent's composer is not disabled: %s / %s", in, send)
	}
	if !strings.Contains(stopped, `<p id="mv-readonly"`) || !strings.Contains(stopped, AgentNotRunningNotice) {
		t.Error("a stopped agent's composer does not say why")
	}
	store.status["keen-wren"] = agents.StatusRunning

	// The chat page: the switch, the form's fallback, a view document.
	resp, chat := get("/agents/keen-wren?session=41")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != viewDocumentCacheControl {
		t.Fatalf("the chat page = %d, Cache-Control %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	if got := resp.Header.Get("Referrer-Policy"); got != "same-origin" {
		t.Errorf("the chat page's Referrer-Policy = %q, want same-origin (its form is one the browser may submit itself)", got)
	}
	if !strings.Contains(chat, `id="view-rendered" aria-current="page"`) || !strings.Contains(chat, `<a id="view-terminal" href="/agents/keen-wren/terminal"`) {
		t.Error("the chat page has no Rendered | Terminal switch")
	}
	if form := tagOf(chat, "chat-form"); !strings.Contains(form, `method="post" action="/agents/keen-wren/send?session=41"`) {
		t.Errorf("the chat form cannot be sent without its socket: %s", form)
	}
	// A gateway agent has no terminal: one view, no switch.
	if _, plain := get("/agents/plain-otter?session=52"); strings.Contains(plain, `id="view-terminal"`) {
		t.Error("an agent of a kind with no terminal draws a view switch")
	}

	// No gateway wired: the composer is drawn disabled, and the route refuses.
	s2, ts2 := sendServer(t, store, nil)
	_, noGw := mirrorGet(t, s2, ts2, ui.AgentTerminalPath("keen-wren"))
	if !disabledAttr.MatchString(tagOf(noGw, "mv-input")) || !strings.Contains(noGw, "no agent gateway wired") {
		t.Error("with no gateway the composer is not disabled with its reason")
	}
}

// TestAnAgentSendThatComesBackAsAGETIsSentBackToItsPage: like a mirrored
// send's (TestASendThatComesBackAsAGETIsSentBackToItsView).
func TestAnAgentSendThatComesBackAsAGETIsSentBackToItsPage(t *testing.T) {
	store, gw := newSendAgents(), &turnGateway{}
	s, ts := sendServer(t, store, gw)
	for path, back := range map[string]string{
		"/agents/keen-wren/send?from=terminal": "/agents/keen-wren/terminal?notice=again",
		"/agents/keen-wren/send?session=41":    "/agents/keen-wren?session=41&notice=again",
	} {
		resp, _ := mirrorGet(t, s, ts, path)
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != back {
			t.Errorf("GET %s = %d → %q; want 303 → %s", path, resp.StatusCode, resp.Header.Get("Location"), back)
		}
	}
	if resp, _ := mirrorGet(t, s, ts, "/agents/no-such-agent/send"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET on an agent that does not exist = %d, want 404", resp.StatusCode)
	}
	if len(gw.seen()) != 0 {
		t.Fatal("a GET ran a turn")
	}
}
