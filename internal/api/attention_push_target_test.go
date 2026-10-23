package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/push"
)

// pushTargetAgents is an agent store holding exactly byID; Get is the one
// method the push target reads (any other call panics on the nil Store).
type pushTargetAgents struct {
	agents.Store
	byID map[int64]agents.Agent
	err  error
}

func (p pushTargetAgents) Get(_ context.Context, id int64) (agents.Agent, error) {
	if p.err != nil {
		return agents.Agent{}, p.err
	}
	a, ok := p.byID[id]
	if !ok {
		return agents.Agent{}, pgx.ErrNoRows
	}
	return a, nil
}

// The push-target fixture's names: pairwise distinct, so a target built from
// the wrong field cannot pass.
const (
	ptHermesSession = "local-0123456789abcdef"
	ptOtherSID      = "9f8e7d6c-5b4a-4392-8170-6f5e4d3c2b1a"
)

// pushTargetServer is a server whose owner (user 1) has one online host "ws"
// listing: @1 a Claude window waiting at a prompt (session focusSID); @2 a
// Claude window, idle, whose session holds an open AskUserQuestion; @3 a
// waiting Hermes window that names its conversation; @4 a waiting Hermes window
// that names none. Agents: 7 ("ptworker-7", the owner's) and 8 (another
// owner's).
func pushTargetServer(t *testing.T, store push.Store, pusher push.Pusher) *Server {
	t.Helper()
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	fm := &fakeMirror{online: true, interactive: true, statusReport: true, v2: &fakeMirrorV2{}, windows: []mirror.Window{
		{Session: "main", ID: "@1", Index: 1, Active: true, Claude: true, ClaudeSession: focusSID, Status: mirror.StatusWaiting, StatusSince: attnSince},
		{Session: "main", ID: "@2", Index: 2, Claude: true, ClaudeSession: ptOtherSID, Status: mirror.StatusIdle, StatusSince: attnSince},
		{Session: "ops", ID: "@3", Index: 1, Active: true, Runtime: mirror.RuntimeHermes, AgentSession: ptHermesSession, Status: mirror.StatusWaiting, StatusSince: attnSince},
		{Session: "ops", ID: "@4", Index: 2, Runtime: mirror.RuntimeHermes, Status: mirror.StatusWaiting, StatusSince: attnSince},
	}}
	ext := Extensions{Users: stubUsers{}, Mirror: fm, Agents: pushTargetAgents{byID: map[int64]agents.Agent{
		7: {ID: 7, OwnerID: 1, Name: "ptworker-7"},
		8: {ID: 8, OwnerID: 2, Name: "someone-elses-8"},
	}}}
	if store != nil {
		ext.Push = push.NewService(store, pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))
	}
	s.UseExtensions(ext)
	if _, err := s.Questions().Open(askq.Key{Owner: 1, Source: askSourceMirror, Scope: "ws", Session: ptOtherSID, ToolUseID: "toolu_pt1"},
		[]askq.Question{{Question: "Which branch?", Options: []askq.Option{{Label: "main"}, {Label: "dev"}}}}, ""); err != nil {
		t.Fatal(err)
	}
	return s
}

// waitingItem is the owner's waiting-session item for window (built by the
// real read model, so its card is the one production sets).
func waitingItem(t *testing.T, s *Server, window string) attentionItem {
	t.Helper()
	for _, it := range s.waitingSessions(s.attentionPrincipal(context.Background(), 1)) {
		if it.Key == sessionKey("ws", windowSession(window), window) {
			return it
		}
	}
	t.Fatalf("window %s is not in the queue", window)
	return attentionItem{}
}

func windowSession(window string) string {
	if window == "@3" || window == "@4" {
		return "ops"
	}
	return "main"
}

// TestEachItemOpensItsSessionOrItsItem is the rule, per item kind: where a
// tap on its notification lands (pushTargetFor, run when tapped). Something
// with a session opens that session's page; anything else its item on the
// queue page.
func TestEachItemOpensItsSessionOrItsItem(t *testing.T) {
	s := pushTargetServer(t, nil, nil)
	ctx := s.attentionPrincipal(context.Background(), 1)
	now := time.Now()
	ask := func(id int64, asker attention.Asker) attentionItem {
		return askItem(attention.Ask{ID: id, Owner: 1, Asker: asker, Question: "q", CreatedAt: now})
	}
	session, question, hermes, hermesNoKey := waitingItem(t, s, "@1"), waitingItem(t, s, "@2"), waitingItem(t, s, "@3"), waitingItem(t, s, "@4")
	if question.Kind != attentionKindQuestion || session.Kind != attentionKindSession || hermes.hermes == nil || hermesNoKey.hermes == nil {
		t.Fatalf("fixture: kinds %s %s, hermes %v %v", session.Kind, question.Kind, hermes.hermes, hermesNoKey.hermes)
	}
	for _, c := range []struct {
		name string
		it   attentionItem
		want string
	}{
		{"a waiting mirrored session", session, "/mirror/view?host=ws&session=main&window=%401"},
		{"a held question", question, "/mirror/view?host=ws&session=main&window=%402"},
		{"a waiting Hermes window with a session key", hermes, "/mirror/view?host=ws&session=ops&window=%403"},
		{"a waiting Hermes window without a session key", hermesNoKey, "/attention#" + sessionKey("ws", "ops", "@4")},
		{"a mirrored session's attention_ask", ask(51, attention.Asker{MirrorHost: "ws", MirrorSession: ptOtherSID, Label: "s"}),
			"/mirror/view?host=ws&session=main&window=%402"},
		{"a mirrored session's attention_ask no window runs", ask(52, attention.Asker{MirrorHost: "ws", MirrorSession: "1a2b3c4d-0000-4000-8000-000000000052", Label: "s"}),
			"/attention#ask-52"},
		{"a mirrored session's attention_ask from a host the owner does not have", ask(53, attention.Asker{MirrorHost: "elsewhere", MirrorSession: focusSID, Label: "s"}),
			"/attention#ask-53"},
		{"an agent's attention_ask", ask(54, attention.Asker{AgentID: 7, Label: "Worker Seven"}), "/agents/ptworker-7"},
		{"an ask by another owner's agent", ask(55, attention.Asker{AgentID: 8, Label: "x"}), "/attention#ask-55"},
		{"an ask by an agent that is gone", ask(56, attention.Asker{AgentID: 9, Label: "x"}), "/attention#ask-56"},
		{"a task ask muster raised itself", ask(57, attention.Asker{System: true, Label: attention.SystemAskerLabel}), "/attention#ask-57"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := s.pushTargetFor(ctx, 1, c.it); got != c.want {
				t.Fatalf("opens %q, want %q", got, c.want)
			}
		})
	}
}

// TestAnAgentReadFailureOpensTheQueue: an asking agent that cannot be read (a
// database blip, not a missing row) opens its item, and the reason is said.
func TestAnAgentReadFailureOpensTheQueue(t *testing.T) {
	s := pushTargetServer(t, nil, nil)
	var logs strings.Builder
	s.logger = log.New(&syncWriter{w: &logs}, "", 0)
	s.ext.Agents = pushTargetAgents{err: errors.New("connection reset")}
	it := askItem(attention.Ask{ID: 61, Owner: 1, Asker: attention.Asker{AgentID: 7}, Question: "q"})
	if got := s.pushTargetFor(s.attentionPrincipal(context.Background(), 1), 1, it); got != "/attention#ask-61" {
		t.Fatalf("opens %q", got)
	}
	if !strings.Contains(logs.String(), "the tap target of agent 7's ask: connection reset") {
		t.Fatalf("log %q", logs.String())
	}
}

// androidMusterPath is muster-android's tap-target rule (core/model
// PayloadV2.kt, MusterPath): an absolute path on the muster origin of these
// characters only. A target it refuses opens the app's home page instead.
var androidMusterPath = regexp.MustCompile(`^/[A-Za-z0-9\-._~!$&'()*+,;=:@%/?#]*$`)

// countingAgents counts every read of the agent store (any method but Get
// panics on the nil Store, which also counts as a read).
type countingAgents struct {
	agents.Store
	mu    sync.Mutex
	reads int
}

func (c *countingAgents) Get(context.Context, int64) (agents.Agent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	return agents.Agent{ID: 7, OwnerID: 1, Name: "ptworker-7"}, nil
}

// TestEveryAttentionPushCarriesItsTapURLToBothDevices: every item's push —
// whatever its kind — carries GET /attention/open/<key> to the browser
// (data.url, what sw.js opens) and the paired phone (payload v2's url) alike,
// a path the phone's rule accepts; and the send reads NOTHING to build it (no
// agent row, no host list), so attention_ask and a task ask do no inline
// I/O for the notification.
func TestEveryAttentionPushCarriesItsTapURLToBothDevices(t *testing.T) {
	store := &memPush{}
	web, native := ep("pt-web"), ep("pt-native")
	claim(t, store, 1, web)
	store.mu.Lock()
	store.nextID++
	var sub push.Subscription
	sub.Endpoint, sub.Keys.P256dh, sub.Keys.Auth = native, "k", "a"
	store.devs = append(store.devs, push.Device{ID: store.nextID, OwnerID: 1, Sub: sub, Kind: push.KindAndroid})
	store.mu.Unlock()
	pusher := &rawPusher{}
	s := pushTargetServer(t, store, pusher)
	session := waitingItem(t, s, "@1")
	reads := &countingAgents{}
	s.ext.Agents = reads
	fm := s.ext.Mirror.(*fakeMirror)
	hostReads := 0
	s.ext.Mirror = &hostCounter{fakeMirror: fm, n: &hostReads}
	for _, c := range []struct {
		it   attentionItem
		want string
	}{
		{session, "/attention/open/" + session.Key},
		{askItem(attention.Ask{ID: 71, Owner: 1, Asker: attention.Asker{System: true, Label: attention.SystemAskerLabel}, Question: "q"}), "/attention/open/ask-71"},
		{askItem(attention.Ask{ID: 72, Owner: 1, Asker: attention.Asker{AgentID: 7, Label: "w"}, Question: "q"}), "/attention/open/ask-72"},
		{askItem(attention.Ask{ID: 73, Owner: 1, Asker: attention.Asker{MirrorHost: "ws", MirrorSession: focusSID, Label: "s"}, Question: "q"}), "/attention/open/ask-73"},
	} {
		pusher.mu.Lock()
		pusher.got = nil
		pusher.mu.Unlock()
		s.attentionNotify(1, c.it)
		s.WaitForPushes()
		var b push.Payload
		pusher.one(t, web, &b)
		var n push.PayloadV2
		pusher.one(t, native, &n)
		if b.Data["url"] != c.want || n.URL != c.want || n.V != 2 || n.Tag != attentionTag(c.it.Key) || b.Tag != attentionTag(c.it.Key) {
			t.Fatalf("browser %+v, native %+v; want url %q", b, n, c.want)
		}
		if !androidMusterPath.MatchString(n.URL) || !attentionOpenKey.MatchString(strings.TrimPrefix(n.URL, attentionOpenPath)) {
			t.Fatalf("%q is not a tap target the phone and the route both accept", n.URL)
		}
	}
	if reads.reads != 0 || hostReads != 0 {
		t.Fatalf("the sends read the agent store %d and the host list %d time(s); want none", reads.reads, hostReads)
	}
}

// hostCounter is a mirror whose host list is counted.
type hostCounter struct {
	*fakeMirror
	n *int
}

func (h *hostCounter) Hosts(ctx context.Context) []mirror.HostView {
	*h.n++
	return h.fakeMirror.Hosts(ctx)
}

// TestTheSessionPushPayloadIsWhatTheWorkerReads pins a SESSION item's
// notification byte for byte, as TestTheAttentionPayloadsAreWhatTheWorkerReads
// pins an ask's: e2e/tests/push.spec.ts delivers this SAME document to the real
// service worker and asserts a tap opens its URL in the open tab.
func TestTheSessionPushPayloadIsWhatTheWorkerReads(t *testing.T) {
	store, pusher := &memPush{}, &rawPusher{}
	claim(t, store, 1, ep("pt-pin"))
	s := pushTargetServer(t, store, pusher)
	s.attentionNotify(1, attentionItem{Key: "session-0011223344556677", Kind: attentionKindSession, Source: "build @ ws", host: "ws",
		card: attentionCard{ref: waitingItem(t, s, "@1").card.ref}})
	s.WaitForPushes()
	var raw json.RawMessage
	pusher.one(t, ep("pt-pin"), &raw)
	const want = `{"type":"attention","id":"session-0011223344556677","title":"A session is waiting on you","body":"Waiting: build @ ws","tag":"attention-session-0011223344556677","data":{"url":"/attention/open/session-0011223344556677"}}`
	if string(raw) != want {
		t.Fatalf("payload:\n%s\nwant:\n%s", raw, want)
	}
}

// syncWriter serialises writes to w (the push goroutine logs too).
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
