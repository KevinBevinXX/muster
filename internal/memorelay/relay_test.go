package memorelay

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
)

// ---- fakes ------------------------------------------------------------------------------------

// fakeAgents is an in-memory agents.Store. It embeds the interface (nil) so a method the relay
// was not expected to call PANICS rather than returning a quiet zero value.
type fakeAgents struct {
	agents.Store
	mu       sync.Mutex
	rows     []agents.Agent
	messages []agents.ChatMessage
	// principals records who each List was made as.
	principals []authz.Principal
	// claims is each agent's session claim (agents.Store.ClaimKickoff): owner by agent id.
	claims map[int64]string
	// claimTTLs records the ttl of every claim that was WON; claimErr fails every claim.
	claimTTLs []time.Duration
	claimErr  error
	// onGet, when set, runs inside Get — "something happened between the list and the claim".
	onGet func(id int64)
	// getOverride, when set for an id, is what Get returns instead of the listed row.
	getOverride map[int64]agents.Agent
}

func (f *fakeAgents) Get(_ context.Context, id int64) (agents.Agent, error) {
	f.mu.Lock()
	hook := f.onGet
	f.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.getOverride[id]; ok {
		return a, nil
	}
	for _, a := range f.rows {
		if a.ID == id {
			return a, nil
		}
	}
	return agents.Agent{}, errors.New("no such agent")
}

// ClaimKickoff wins iff the claim is unheld, exactly as the real conditional UPDATE does.
//
// ⚠ It does not model EXPIRY: the real claim is also winnable once its ttl has passed, so a ttl
// too short to cover the turn excludes nobody. The ttl is recorded and asserted on instead.
func (f *fakeAgents) ClaimKickoff(_ context.Context, id int64, owner string, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return true, f.claimErr // the worst a store could answer: "won", and an error
	}
	if f.claims == nil {
		f.claims = map[int64]string{}
	}
	if f.claims[id] != "" {
		return false, nil
	}
	f.claims[id] = owner
	f.claimTTLs = append(f.claimTTLs, ttl)
	return true, nil
}

// ReleaseKickoffClaim clears the claim only for its owner, and — like pgx — refuses a done ctx.
func (f *fakeAgents) ReleaseKickoffClaim(ctx context.Context, id int64, owner string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claims[id] == owner {
		delete(f.claims, id)
	}
	return nil
}

func (f *fakeAgents) claimOf(id int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims[id]
}

func (f *fakeAgents) setClaim(id int64, owner string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claims == nil {
		f.claims = map[int64]string{}
	}
	if owner == "" {
		delete(f.claims, id)
	} else {
		f.claims[id] = owner
	}
}

func (f *fakeAgents) List(ctx context.Context) ([]agents.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := authz.From(ctx); ok {
		f.principals = append(f.principals, p)
	}
	return append([]agents.Agent(nil), f.rows...), nil
}

func (f *fakeAgents) LatestOrCreateSession(_ context.Context, agentID int64, _ string) (agents.ChatSession, error) {
	return agents.ChatSession{ID: agentID * 10, AgentID: agentID, SessionKey: fmt.Sprintf("sess-%d", agentID)}, nil
}

func (f *fakeAgents) AddChatMessage(ctx context.Context, m agents.ChatMessage) (agents.ChatMessage, error) {
	if ctx.Err() != nil {
		return agents.ChatMessage{}, ctx.Err() // as pgx refuses a write on a done context
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, m)
	return m, nil
}

// fakeSource answers Live from a map and records every scope asked for.
type fakeSource struct {
	mu    sync.Mutex
	live  map[string][]Memo
	errs  map[string]error
	asked []string
}

func (s *fakeSource) Live(_ context.Context, scope string) ([]Memo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, scope)
	if err := s.errs[scope]; err != nil {
		return nil, err
	}
	return append([]Memo(nil), s.live[scope]...), nil
}

func (s *fakeSource) set(scope string, memos ...Memo) {
	s.mu.Lock()
	s.live[scope] = memos
	s.mu.Unlock()
}

func (s *fakeSource) askedFor() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.asked...)
}

// sentTurn is one turn the fake gateway was asked to run.
type sentTurn struct {
	agent   string
	session string
	message string
}

// fakeGateway records every Send and fails it with whatever `fail` holds for the agent.
type fakeGateway struct {
	mu         sync.Mutex
	sends      []sentTurn
	fail       map[string]error
	names      map[string]string
	resolveErr error
	// block, when set, is waited on inside Send (after the turn is recorded) until the Send's
	// own context ends: a turn in flight when the process is told to stop.
	block bool
	// duringSend, when set, runs inside every Send that pastes.
	duringSend func(agent string)
}

// Resolve notes which agent each session belongs to. agentgateway.Target's fields are unexported
// by design, so a fake cannot carry a name in one; the session key (fakeAgents hands out
// "sess-<agent id>") is what Send is given back.
func (g *fakeGateway) Resolve(_ context.Context, a agents.Agent) (agentgateway.Target, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.resolveErr != nil {
		return agentgateway.Target{}, g.resolveErr
	}
	g.names[fmt.Sprintf("sess-%d", a.ID)] = a.Name
	return agentgateway.Target{}, nil
}

func (g *fakeGateway) Send(ctx context.Context, _ agentgateway.Target, sessionKey, message string, _ func(string)) (string, error) {
	g.mu.Lock()
	name := g.names[sessionKey]
	err := g.fail[name]
	block := g.block
	hook := g.duringSend
	pasted := unsent(err) == ""
	if pasted {
		// The runtime pasted it (or may have): this is what "the agent saw" means below.
		g.sends = append(g.sends, sentTurn{agent: name, session: sessionKey, message: message})
	}
	g.mu.Unlock()
	if pasted && hook != nil {
		hook(name)
	}
	if block {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if err != nil {
		return "", err
	}
	return "noted", nil
}

func (g *fakeGateway) sent() []sentTurn {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]sentTurn(nil), g.sends...)
}

func (g *fakeGateway) setFail(agent string, err error) {
	g.mu.Lock()
	if err == nil {
		delete(g.fail, agent)
	} else {
		g.fail[agent] = err
	}
	g.mu.Unlock()
}

// memLedger is an in-memory Ledger that outlives a Relay, as the database outlives a process.
type memLedger struct {
	mu        sync.Mutex
	rows      []ledgerRow
	readErr   error
	recordErr error
}

type ledgerRow struct {
	agent   int64
	scope   string
	ids     []int64
	outcome string
}

func (l *memLedger) Delivered(_ context.Context, agentID int64) (map[Key]bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.readErr != nil {
		return nil, l.readErr
	}
	out := map[Key]bool{}
	for _, r := range l.rows {
		if r.agent == agentID {
			for _, id := range r.ids {
				out[Key{Scope: r.scope, ID: id}] = true
			}
		}
	}
	return out, nil
}

func (l *memLedger) Record(ctx context.Context, a agents.Agent, scope string, ids []int64, outcome string) error {
	if ctx.Err() != nil {
		return ctx.Err() // as pgx refuses a write on a done context
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.recordErr != nil {
		return l.recordErr
	}
	l.rows = append(l.rows, ledgerRow{agent: a.ID, scope: scope, ids: append([]int64(nil), ids...), outcome: outcome})
	return nil
}

func (l *memLedger) snapshot() []ledgerRow {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]ledgerRow(nil), l.rows...)
}

// ---- the world --------------------------------------------------------------------------------

const prefix = "fleet-"

// agent is an operator-owned, running, kicked-off claude-code agent working on repo.
//
// ⚠ The real claude-code kind refuses a row that names a repository (its scope comes from its
// task); the fixture names one directly so most tests need no task. The task path has its own
// test.
func agent(id int64, name, repo string) agents.Agent {
	return agents.Agent{ID: id, Name: name, Namespace: prefix + name, Repo: repo, OwnerID: authz.OperatorUserID,
		Status: agents.StatusRunning, HooksToken: "hooks-" + name, KickedOff: true, Kind: agents.KindClaudeCode}
}

type world struct {
	store    *fakeAgents
	src      *fakeSource
	gw       *fakeGateway
	ledger   *memLedger
	tasks    map[int64]string
	taskErr  error
	scopeMap ScopeMap
}

func newWorld(rows ...agents.Agent) *world {
	return &world{
		store:  &fakeAgents{rows: rows},
		src:    &fakeSource{live: map[string][]Memo{}, errs: map[string]error{}},
		gw:     &fakeGateway{fail: map[string]error{}, names: map[string]string{}},
		ledger: &memLedger{},
		tasks:  map[int64]string{},
	}
}

// relay builds a NEW Relay over the world — a new process over the same database.
func (w *world) relay(t *testing.T) *Relay {
	t.Helper()
	return w.relayWith(t, w.src)
}

func (w *world) relayWith(t *testing.T, src Source) *Relay {
	t.Helper()
	r, err := New(Config{
		Agents: w.store, Gateway: w.gw, Source: src, Ledger: w.ledger, NamespacePrefix: prefix,
		TaskRepo: func(_ context.Context, id int64) (string, error) { return w.tasks[id], w.taskErr },
		ScopeMap: w.scopeMap,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func tick(t *testing.T, r *Relay) {
	t.Helper()
	if err := r.Tick(authz.Operator(context.Background())); err != nil {
		t.Fatal(err)
	}
	r.Wait()
}

func m(t testing.TB, id int64, scope, created string) Memo {
	return memo(t, id, scope, "Ada via laptop (user)", created, "2000-01-09", fmt.Sprintf("subject %d", id), fmt.Sprintf("body %d", id))
}

// memoIDs is the ids a delivered turn carries, read off its stanza headers.
func memoIDs(message string) []int64 {
	var out []int64
	for _, line := range strings.Split(message, "\n") {
		var id int64
		if _, err := fmt.Sscanf(line, "| [m-%d] from", &id); err == nil {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func busyErr() error {
	return &agents.RuntimeError{Status: 409, Type: "busy"}
}

// ---- once -------------------------------------------------------------------------------------

// TestEachAgentIsSentEachMemoOnce: three ticks in one process, then a NEW process over the same
// ledger (a restart). The memo is sent to each agent on the scope exactly once across all four.
func TestEachAgentIsSentEachMemoOnce(t *testing.T) {
	w := newWorld(agent(1, "ada", "example/widgets"), agent(2, "bob", "example/widgets"), agent(3, "cy", "example/gadgets"))
	w.src.set("widgets", m(t, 7, "widgets", "2000-01-02T03:04:05Z"))

	r := w.relay(t)
	for i := 0; i < 3; i++ {
		tick(t, r)
	}
	tick(t, w.relay(t)) // the restart

	got := map[string]int{}
	for _, s := range w.gw.sent() {
		if !reflect.DeepEqual(memoIDs(s.message), []int64{7}) {
			t.Errorf("agent %s was sent memos %v, want [7]", s.agent, memoIDs(s.message))
		}
		got[s.agent]++
	}
	if want := map[string]int{"ada": 1, "bob": 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("turns sent per agent = %v, want %v (cy works on another scope)", got, want)
	}
	// A memo that arrives later is new to everyone, and still sent once.
	w.src.set("widgets", m(t, 7, "widgets", "2000-01-02T03:04:05Z"), m(t, 8, "widgets", "2000-01-02T04:00:00Z"))
	tick(t, r)
	tick(t, r)
	var later []string
	for _, s := range w.gw.sent()[2:] {
		later = append(later, fmt.Sprintf("%s%v", s.agent, memoIDs(s.message)))
	}
	sort.Strings(later)
	if want := []string{"ada[8]", "bob[8]"}; !reflect.DeepEqual(later, want) {
		t.Fatalf("after memo 8 arrived the further turns were %v, want %v", later, want)
	}
}

// TestADeliveredTurnIsTheFencedBlockAndLandsInTheTranscript: what the gateway is handed is the
// pinned block, byte for byte beyond its nonce, on the agent's own chat session; and the
// transcript holds it as the user message, with the agent's reply after it.
func TestADeliveredTurnIsTheFencedBlockAndLandsInTheTranscript(t *testing.T) {
	w := newWorld(agent(1, "ada", "example/widgets"))
	w.src.set("widgets", twoMemos(t)...)
	tick(t, w.relay(t))

	sent := w.gw.sent()
	if len(sent) != 1 {
		t.Fatalf("%d turn(s) sent, want 1", len(sent))
	}
	if got := normalise(sent[0].message); got != wantTwoMemos {
		t.Fatalf("the turn handed to the gateway is not the pinned block.\n--- got ---\n%s", got)
	}
	if sent[0].session != "sess-1" {
		t.Errorf("sent on session %q, want the agent's own (sess-1)", sent[0].session)
	}
	if len(w.store.messages) != 2 || w.store.messages[0].Role != "user" || w.store.messages[0].Content != sent[0].message ||
		w.store.messages[1].Role != "assistant" || w.store.messages[1].Content != "noted" {
		t.Fatalf("transcript = %+v, want the delivered block as the user message then the reply", w.store.messages)
	}
	if rows := w.ledger.snapshot(); len(rows) != 1 || rows[0].outcome != OutcomeDelivered ||
		!reflect.DeepEqual(rows[0].ids, []int64{7, 9}) || rows[0].scope != "widgets" || rows[0].agent != 1 {
		t.Fatalf("ledger = %+v, want one delivered row for agent 1 naming memos 7 and 9", rows)
	}
}

// ---- retraction and expiry --------------------------------------------------------------------

// TestARetractedOrExpiredMemoStopsBeingDelivered: the listener lists only live memos, so a memo
// that is gone from the list reaches no agent that had not already been sent it. bob is mid-turn
// while it is live, so he is the agent who must never see it.
func TestARetractedOrExpiredMemoStopsBeingDelivered(t *testing.T) {
	w := newWorld(agent(1, "ada", "example/widgets"), agent(2, "bob", "example/widgets"))
	w.src.set("widgets", m(t, 7, "widgets", "2000-01-02T03:04:05Z"))
	w.gw.setFail("bob", busyErr())
	r := w.relay(t)
	tick(t, r)
	if got := len(w.gw.sent()); got != 1 || w.gw.sent()[0].agent != "ada" {
		t.Fatalf("while bob is busy: %d turn(s) %+v, want exactly ada's", got, w.gw.sent())
	}

	// Positive control for the retry: were the memo still live, bob WOULD get it now.
	w.gw.setFail("bob", nil)
	w.src.set("widgets") // retracted (or expired): no longer listed
	tick(t, r)
	tick(t, r)
	if got := len(w.gw.sent()); got != 1 {
		t.Fatalf("%d turn(s) after the retraction, want still 1: a memo that is no longer live was delivered", got)
	}

	w.src.set("widgets", m(t, 7, "widgets", "2000-01-02T03:04:05Z"))
	tick(t, r)
	if got := w.gw.sent(); len(got) != 2 || got[1].agent != "bob" {
		t.Fatalf("control: with the memo live again bob must be sent it; turns = %+v", got)
	}
}

// ---- the lockout guard, through the relay -----------------------------------------------------

// TestARefusedCredentialEndsTheTickAndEveryLaterOne drives the REAL Listener against a listener
// that refuses: agents on three scopes, five ticks, ONE request in all. Then the backoff passes
// and exactly one more is sent.
func TestARefusedCredentialEndsTheTickAndEveryLaterOne(t *testing.T) {
	s := newStub(t, func(string) (int, string) { return 401, `{}` })
	c := newClock()
	l := started(t, s, c)
	w := newWorld(agent(1, "ada", "example/widgets"), agent(2, "bob", "example/gadgets"), agent(3, "cy", "example/sprockets"))
	r := w.relayWith(t, l)

	for i := 0; i < 5; i++ {
		tick(t, r)
		c.advance(DefaultInterval)
	}
	if n := s.requests.Load(); n != 1 {
		t.Fatalf("%d request(s) reached a refusing listener over five ticks and three scopes, want 1", n)
	}
	counts := r.Counts()
	if counts["upstream/refused"] != 1 || counts["upstream/backed_off"] != 4 {
		t.Fatalf("counts = %v, want upstream/refused 1 and upstream/backed_off 4", counts)
	}
	if len(w.gw.sent()) != 0 {
		t.Fatal("a turn was sent with no memos read")
	}

	c.advance(RefusedBackoff)
	tick(t, r)
	if n := s.requests.Load(); n != 2 {
		t.Fatalf("after the backoff: %d request(s) in all, want 2 (one more, then refused again)", n)
	}
}

// TestARefusalTellsTheOperatorToRestartOnce pins the WHOLE line a 401 logs. It is the instruction
// an operator acts on with a shared lockout bucket at stake, so a reword must be a decision.
func TestARefusalTellsTheOperatorToRestartOnce(t *testing.T) {
	var buf strings.Builder
	w := newWorld(agent(1, "ada", "example/widgets"))
	w.src.errs["widgets"] = ErrRefused
	r, err := New(Config{Agents: w.store, Gateway: w.gw, Source: w.src, Ledger: w.ledger, NamespacePrefix: prefix,
		TaskRepo: func(context.Context, int64) (string, error) { return "", nil }, Logger: log.New(&buf, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	tick(t, r)
	want := "memorelay: the listener REFUSED the relay's credential; nothing is asked of it for 15m0s. " +
		"The token is read once, at start: fix the Secret behind MUSTER_MEMO_TOKEN, then restart muster ONCE. " +
		"Until then this process re-sends the same token every 15m0s. Never restart in a loop: each refused " +
		"request counts toward the listener's lockout of this node.\n"
	if got := buf.String(); got != want {
		t.Fatalf("the refusal logged:\n%q\nwant:\n%q", got, want)
	}
}

// TestAnUnreachableListenerEndsTheTickButAMissedScopeDoesNot: an unreachable listener is not
// asked about the remaining scopes; a scope it merely does not have says nothing about the next.
func TestAnUnreachableListenerEndsTheTickButAMissedScopeDoesNot(t *testing.T) {
	w := newWorld(agent(1, "ada", "example/aaa"), agent(2, "bob", "example/bbb"), agent(3, "cy", "example/ccc"))
	w.src.set("ccc", m(t, 7, "ccc", "2000-01-02T03:04:05Z"))

	for _, answer := range []error{
		ErrScopeUnreadable,
		ErrScopeBackedOff,
		fmt.Errorf("%w: a later release's shape", ErrMalformed),
		fmt.Errorf("%w: HTTP 500", ErrUnexpectedStatus),
	} {
		w.src.errs["aaa"], w.src.errs["bbb"] = answer, answer
		w.src.asked = nil
		w.gw.sends = nil
		w.ledger.rows = nil
		r := w.relay(t)
		tick(t, r)
		if got := w.src.askedFor(); !reflect.DeepEqual(got, []string{"aaa", "bbb", "ccc"}) {
			t.Fatalf("%v: asked for %v, want all three scopes in order", answer, got)
		}
		if got := w.gw.sent(); len(got) != 1 || got[0].agent != "cy" {
			t.Fatalf("%v: turns = %+v, want cy's alone", answer, got)
		}
	}
	r := w.relay(t)

	w.src.errs["aaa"] = fmt.Errorf("%w: connection refused", ErrUnreachable)
	w.src.asked = nil
	tick(t, r)
	if got := w.src.askedFor(); !reflect.DeepEqual(got, []string{"aaa"}) {
		t.Fatalf("with the listener unreachable, asked for %v, want the first scope only", got)
	}
}

// ---- which agents, and which scope ------------------------------------------------------------

// TestAnAgentWithNoResolvableScopeIsSentNothingAndCounted: no repository on the agent and none on
// its task is not a scope, and the listener is never asked on its behalf. The control is ada, who
// IS resolvable and is sent the memo in the same tick.
func TestAnAgentWithNoResolvableScopeIsSentNothingAndCounted(t *testing.T) {
	task := int64(40)
	bare := agent(2, "bare", "")
	fromTask := agent(3, "tasked", "")
	fromTask.NoteID = &task
	repoless := int64(41)
	taskNoRepo := agent(4, "repoless", "")
	taskNoRepo.NoteID = &repoless

	w := newWorld(agent(1, "ada", "example/widgets"), bare, fromTask, taskNoRepo)
	w.tasks[40] = "example/gadgets"
	w.tasks[41] = ""
	w.src.set("widgets", m(t, 7, "widgets", "2000-01-02T03:04:05Z"))
	w.src.set("gadgets", m(t, 8, "gadgets", "2000-01-02T03:04:05Z"))
	// A memo under the empty scope and under the agents' own names: were a scope GUESSED from
	// anything else on the row, one of these would be delivered.
	for _, guess := range []string{"", "bare", "repoless", prefix + "bare", "data", "workspace"} {
		w.src.set(guess, m(t, 99, guess, "2000-01-02T03:04:05Z"))
	}

	r := w.relay(t)
	tick(t, r)

	var got []string
	for _, s := range w.gw.sent() {
		got = append(got, fmt.Sprintf("%s%v", s.agent, memoIDs(s.message)))
	}
	sort.Strings(got)
	if want := []string{"ada[7]", "tasked[8]"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("turns = %v, want %v: the agent's own repository, else its task's, and nothing else", got, want)
	}
	if asked := w.src.askedFor(); !reflect.DeepEqual(asked, []string{"gadgets", "widgets"}) {
		t.Fatalf("the listener was asked for %v, want only the two derived scopes", asked)
	}
	counts := r.Counts()
	if counts["agent/no_scope"] != 2 || counts["agent/eligible"] != 2 {
		t.Fatalf("counts = %v, want agent/no_scope 2 and agent/eligible 2", counts)
	}

	// A task that could not be READ is not "a task with no repository": it is counted apart.
	w.taskErr = errors.New("database unavailable")
	tick(t, r)
	if counts := r.Counts(); counts["agent/scope_lookup_failed"] != 2 {
		t.Fatalf("counts = %v, want agent/scope_lookup_failed 2 (both task-linked agents)", counts)
	}
}

// TestOnlyTheOperatorsRunningKickedOffAgentsAreSentMemos: one agent per refusal, each on a scope
// with a live memo, and one control that differs from them in nothing else.
func TestOnlyTheOperatorsRunningKickedOffAgentsAreSentMemos(t *testing.T) {
	other := agent(2, "theirs", "example/widgets")
	other.OwnerID = authz.OperatorUserID + 1
	unowned := agent(3, "unowned", "example/widgets")
	unowned.OwnerID = 0
	foreign := agent(4, "foreign", "example/widgets")
	foreign.Namespace = "elsewhere-foreign"
	stopped := agent(5, "stopped", "example/widgets")
	stopped.Status = agents.StatusStopped
	errored := agent(6, "errored", "example/widgets")
	errored.Status = agents.StatusError
	owed := agent(7, "owed", "example/widgets")
	owed.KickedOff, owed.PendingNote = false, "the task"
	tokenless := agent(8, "tokenless", "example/widgets")
	tokenless.HooksToken = ""
	// Never kicked off and nothing pending: a blank agent a person chats with. It IS eligible.
	blank := agent(9, "blank", "example/widgets")
	blank.KickedOff = false
	// Any kind but claude-code: nothing says its runtime refuses a turn while another runs.
	gatewayKind := agent(10, "gatewaykind", "example/widgets")
	gatewayKind.Kind = agents.KindGateway
	defaultKind := agent(11, "defaultkind", "example/widgets")
	defaultKind.Kind = "" // reads as the gateway kind

	w := newWorld(agent(1, "ada", "example/widgets"), other, unowned, foreign, stopped, errored, owed, tokenless, blank,
		gatewayKind, defaultKind)
	w.src.set("widgets", m(t, 7, "widgets", "2000-01-02T03:04:05Z"))
	r := w.relay(t)
	tick(t, r)

	var got []string
	for _, s := range w.gw.sent() {
		got = append(got, s.agent)
	}
	sort.Strings(got)
	if want := []string{"ada", "blank"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("agents sent the memo = %v, want %v", got, want)
	}
	want := map[string]int{"agent/eligible": 2, "agent/not_operator": 2, "agent/foreign": 1, "agent/not_running": 2,
		"agent/kickoff_owed": 1, "agent/no_token": 1, "agent/kind_unsupported": 2}
	counts := r.Counts()
	for k, n := range want {
		if counts[k] != n {
			t.Errorf("counts[%s] = %d, want %d (all counts: %v)", k, counts[k], n, counts)
		}
	}
}

// TestRunActsAsTheOperatorAndNeverAsTheSystem: the loop's own context is what row-level security
// scopes its reads to. As the system it would be handed every user's agents.
func TestRunActsAsTheOperatorAndNeverAsTheSystem(t *testing.T) {
	w := newWorld(agent(1, "ada", "example/widgets"))
	r := w.relay(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx, time.Hour); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		w.store.mu.Lock()
		n := len(w.store.principals)
		w.store.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	select {
	case <-r.Done():
	default:
		t.Fatal("Done is not closed after Run returned")
	}
	if len(w.store.principals) == 0 {
		t.Fatal("Run never listed agents, or listed them with no principal at all")
	}
	for _, p := range w.store.principals {
		if p.UserID != authz.OperatorUserID || p.All() {
			t.Fatalf("the relay listed agents as %s, want the operator (user %d) and never the system", p, authz.OperatorUserID)
		}
	}
}

// ---- what a failed send means -----------------------------------------------------------------

// TestASendThatPastedNothingIsOfferedAgainAndOneThatMayHaveIsNot walks every send failure:
// the three that prove nothing was pasted leave no record and are retried; every other failure
// is recorded `unconfirmed` and never retried.
func TestASendThatPastedNothingIsOfferedAgainAndOneThatMayHaveIsNot(t *testing.T) {
	dial := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	for name, c := range map[string]struct {
		err       error
		retried   bool
		countedAs string
	}{
		"typed busy":           {busyErr(), true, ResultBusy},
		"typed not_ready":      {&agents.RuntimeError{Status: 503, Type: "not_ready"}, true, ResultNotReady},
		"a dial that failed":   {fmt.Errorf("post: %w", dial), true, ResultNeverConnected},
		"typed unauthorized":   {&agents.RuntimeError{Status: 401, Type: "unauthorized"}, true, ResultUnauthorized},
		"typed invalid_input":  {&agents.RuntimeError{Status: 400, Type: "invalid_input"}, true, ResultRejected},
		"typed bad_request":    {&agents.RuntimeError{Status: 400, Type: "bad_request"}, true, ResultRejected},
		"an UNTYPED 401":       {&agents.RuntimeError{Status: 401}, false, OutcomeUnconfirmed},
		"an UNTYPED 400":       {&agents.RuntimeError{Status: 400}, false, OutcomeUnconfirmed},
		"a cancelled send":     {context.Canceled, false, OutcomeUnconfirmed},
		"an UNTYPED 409":       {&agents.RuntimeError{Status: 409}, false, OutcomeUnconfirmed},
		"an UNTYPED 503":       {&agents.RuntimeError{Status: 503}, false, OutcomeUnconfirmed},
		"busy on another code": {&agents.RuntimeError{Status: 500, Type: "busy"}, false, OutcomeUnconfirmed},
		"typed turn_timeout":   {&agents.RuntimeError{Status: 504, Type: "turn_timeout"}, false, OutcomeUnconfirmed},
		"a read that failed":   {&net.OpError{Op: "read", Net: "tcp", Err: errors.New("reset")}, false, OutcomeUnconfirmed},
		"an unexplained error": {errors.New("unexpected EOF"), false, OutcomeUnconfirmed},
		"the budget ran out":   {context.DeadlineExceeded, false, OutcomeUnconfirmed},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(agent(1, "ada", "example/widgets"))
			w.src.set("widgets", m(t, 7, "widgets", "2000-01-02T03:04:05Z"))
			w.gw.setFail("ada", c.err)
			r := w.relay(t)
			tick(t, r)
			if n := r.Counts()["delivery/"+c.countedAs]; n != 1 {
				t.Fatalf("counted as %v, want delivery/%s 1", r.Counts(), c.countedAs)
			}
			rows := w.ledger.snapshot()
			if c.retried && len(rows) != 0 {
				t.Fatalf("a send that pasted nothing was recorded: %+v", rows)
			}
			if !c.retried && (len(rows) != 1 || rows[0].outcome != OutcomeUnconfirmed) {
				t.Fatalf("ledger = %+v, want one unconfirmed row", rows)
			}

			w.gw.setFail("ada", nil)
			before := len(w.gw.sent())
			tick(t, r)
			tick(t, w.relay(t)) // and across a restart
			after := len(w.gw.sent())
			if c.retried && after-before != 1 {
				t.Fatalf("%d further turn(s) once the agent could take one, want 1", after-before)
			}
			if !c.retried && after-before != 0 {
				t.Fatalf("%d further turn(s) after a send that may have been pasted, want 0", after-before)
			}
		})
	}
}

// TestAnUnresolvableAgentIsOfferedAgain: finding the agent's address fails before anything is
// sent, so nothing is recorded and the next tick delivers.
func TestAnUnresolvableAgentIsOfferedAgain(t *testing.T) {
	w := newWorld(agent(1, "ada", "example/widgets"))
	w.src.set("widgets", m(t, 7, "widgets", "2000-01-02T03:04:05Z"))
	w.gw.resolveErr = errors.New("the cluster API is unavailable")
	r := w.relay(t)
	tick(t, r)
	if len(w.gw.sent()) != 0 || len(w.ledger.snapshot()) != 0 || r.Counts()["delivery/unresolved"] != 1 {
		t.Fatalf("sent=%d ledger=%d counts=%v, want nothing sent, nothing recorded, unresolved 1",
			len(w.gw.sent()), len(w.ledger.snapshot()), r.Counts())
	}
	w.gw.resolveErr = nil
	tick(t, r)
	if len(w.gw.sent()) != 1 {
		t.Fatalf("%d turn(s) once the agent resolves, want 1", len(w.gw.sent()))
	}
}

// TestAnUnreadableLedgerSendsNothing: "could not read what was delivered" is not "nothing was
// delivered". The memo was already sent; a ledger that then fails must not send it again.
func TestAnUnreadableLedgerSendsNothing(t *testing.T) {
	w := newWorld(agent(1, "ada", "example/widgets"))
	w.src.set("widgets", m(t, 7, "widgets", "2000-01-02T03:04:05Z"))
	tick(t, w.relay(t))
	if len(w.gw.sent()) != 1 {
		t.Fatalf("setup: %d turn(s), want 1", len(w.gw.sent()))
	}
	w.ledger.readErr = errors.New("database unavailable")
	r := w.relay(t) // a new process: nothing in memory, so only the ledger knows
	tick(t, r)
	if len(w.gw.sent()) != 1 {
		t.Fatalf("%d turn(s) with the ledger unreadable, want still 1", len(w.gw.sent()))
	}
	if r.Counts()["delivery/ledger_unreadable"] != 1 {
		t.Fatalf("counts = %v, want delivery/ledger_unreadable 1", r.Counts())
	}
}

// TestAMemoWhoseRecordCouldNotBeWrittenIsNotSentAgainByTheSameProcess: the send happened, the
// row did not. The process that sent it remembers; it is counted and logged as what it is.
func TestAMemoWhoseRecordCouldNotBeWrittenIsNotSentAgainByTheSameProcess(t *testing.T) {
	w := newWorld(agent(1, "ada", "example/widgets"))
	w.src.set("widgets", m(t, 7, "widgets", "2000-01-02T03:04:05Z"))
	w.ledger.recordErr = errors.New("database unavailable")
	r := w.relay(t)
	for i := 0; i < 3; i++ {
		tick(t, r)
	}
	if len(w.gw.sent()) != 1 {
		t.Fatalf("%d turn(s) over three ticks, want 1", len(w.gw.sent()))
	}
	if r.Counts()["delivery/record_failed"] != 1 {
		t.Fatalf("counts = %v, want delivery/record_failed 1", r.Counts())
	}
}

// TestATurnCutOffByShutdownIsStillRecorded: the process is told to stop while a turn is in
// flight. The turn's context is cancelled — and so would be any write made on it (both fakes
// refuse a done context, as pgx does) — yet the delivery is recorded, so the next process does
// not send it again.
func TestATurnCutOffByShutdownIsStillRecorded(t *testing.T) {
	w := newWorld(agent(1, "ada", "example/widgets"))
	w.src.set("widgets", m(t, 7, "widgets", "2000-01-02T03:04:05Z"))
	w.gw.block = true
	r := w.relay(t)

	ctx, cancel := context.WithCancel(authz.Operator(context.Background()))
	if err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(w.gw.sent()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(w.gw.sent()) != 1 {
		t.Fatal("setup: the turn never reached the gateway")
	}
	cancel() // SIGTERM
	r.Wait()

	rows := w.ledger.snapshot()
	if len(rows) != 1 || rows[0].outcome != OutcomeUnconfirmed || !reflect.DeepEqual(rows[0].ids, []int64{7}) {
		t.Fatalf("ledger = %+v, want one unconfirmed row for memo 7 written after the cancel", rows)
	}
	w.gw.block = false
	tick(t, w.relay(t))
	if len(w.gw.sent()) != 1 {
		t.Fatalf("%d turn(s) after the restart, want still 1", len(w.gw.sent()))
	}
}

// TestAnAgentMidDeliveryIsNotStartedOnAgain: a turn can run for many ticks. While one is in
// flight the agent is neither sent a second turn nor asked about again, and the memo that
// arrived meanwhile goes out once the first turn has ended.
func TestAnAgentMidDeliveryIsNotStartedOnAgain(t *testing.T) {
	w := newWorld(agent(1, "ada", "example/widgets"))
	w.src.set("widgets", m(t, 7, "widgets", "2000-01-02T03:04:05Z"))
	w.gw.block = true
	r := w.relay(t)

	ctx, cancel := context.WithCancel(authz.Operator(context.Background()))
	defer cancel()
	if err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(w.gw.sent()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	w.src.set("widgets", m(t, 7, "widgets", "2000-01-02T03:04:05Z"), m(t, 8, "widgets", "2000-01-02T04:00:00Z"))
	for i := 0; i < 3; i++ {
		if err := r.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Give a wrongly started second delivery time to reach the gateway.
	time.Sleep(50 * time.Millisecond)
	if got := len(w.gw.sent()); got != 1 {
		t.Fatalf("%d turn(s) reached the gateway while the first was still running, want 1", got)
	}

	cancel() // the first turn ends (cut off): recorded, and the agent is free again
	r.Wait()
	w.gw.mu.Lock()
	w.gw.block = false
	w.gw.mu.Unlock()
	tick(t, r)
	sent := w.gw.sent()
	if len(sent) != 2 || !reflect.DeepEqual(memoIDs(sent[1].message), []int64{8}) {
		t.Fatalf("after the first turn ended: turns = %d, the second carrying %v; want 2 and [8]",
			len(sent), memoIDs(sent[len(sent)-1].message))
	}
}

// TestADeliveryCarriesAtMostFiveMemosOldestFirst: seven live memos are two turns, and the first
// carries the five OLDEST, so nothing is starved by newer arrivals.
func TestADeliveryCarriesAtMostFiveMemosOldestFirst(t *testing.T) {
	w := newWorld(agent(1, "ada", "example/widgets"))
	var live []Memo
	// Listed newest first, to show the order is the relay's and not the listener's.
	for id := int64(7); id >= 1; id-- {
		live = append(live, m(t, id, "widgets", fmt.Sprintf("2000-01-02T03:04:%02dZ", id)))
	}
	w.src.set("widgets", live...)
	r := w.relay(t)
	tick(t, r)
	tick(t, r)
	tick(t, r)
	sent := w.gw.sent()
	if len(sent) != 2 {
		t.Fatalf("%d turn(s), want 2", len(sent))
	}
	if got := memoIDs(sent[0].message); !reflect.DeepEqual(got, []int64{1, 2, 3, 4, 5}) {
		t.Errorf("first turn carried %v, want the five oldest", got)
	}
	if got := memoIDs(sent[1].message); !reflect.DeepEqual(got, []int64{6, 7}) {
		t.Errorf("second turn carried %v, want the remaining two", got)
	}
	if MaxPerDelivery != 5 {
		t.Fatalf("MaxPerDelivery is %d; this test's fixture is sized for 5", MaxPerDelivery)
	}
}

// TestTwoScopesNeverSuppressEachOther: the ledger is keyed on (scope, id), so an agent sent memo 7
// of one scope is still owed memo 7 of another after its task moves it there.
func TestTwoScopesNeverSuppressEachOther(t *testing.T) {
	w := newWorld(agent(1, "ada", "example/widgets"))
	w.src.set("widgets", m(t, 7, "widgets", "2000-01-02T03:04:05Z"))
	w.src.set("gadgets", m(t, 7, "gadgets", "2000-01-02T03:04:05Z"))
	r := w.relay(t)
	tick(t, r)
	w.store.rows[0].Repo = "example/gadgets"
	tick(t, w.relay(t))
	if got := len(w.gw.sent()); got != 2 {
		t.Fatalf("%d turn(s), want 2: one per scope", got)
	}
}

// ---- the session claim ------------------------------------------------------------------------

// TestAMemoNeverTakesTheSessionFromAFirstTurnStillBeingHandedOver: internal/agentkickoff stamps a
// row kicked-off BEFORE it sends the first turn, then re-sends it for minutes while the CLI
// starts. Through all of that the row reads eligible — running, stamped, with a token. A memo
// sent then would leave the first turn answered `busy`, recorded as failed and never retried.
// The deliverer holds the agent's claim for its whole delivery; the relay must lose to it.
func TestAMemoNeverTakesTheSessionFromAFirstTurnStillBeingHandedOver(t *testing.T) {
	midKickoff := agent(1, "ada", "example/widgets")
	midKickoff.PendingNote = "the task" // stamped, first turn in flight
	w := newWorld(midKickoff)
	w.src.set("widgets", m(t, 7, "widgets", "2000-01-02T03:04:05Z"))
	w.store.setClaim(1, "kickoff-deliverer")
	r := w.relay(t)

	for i := 0; i < 3; i++ {
		tick(t, r)
	}
	if got := len(w.gw.sent()); got != 0 {
		t.Fatalf("%d memo turn(s) reached the agent while its first turn held the session claim, want 0", got)
	}
	if rows := w.ledger.snapshot(); len(rows) != 0 {
		t.Fatalf("a memo that was not sent was recorded: %+v", rows)
	}
	if n := r.Counts()["delivery/session_claimed"]; n != 3 {
		t.Fatalf("counts = %v, want delivery/session_claimed 3", r.Counts())
	}
	if got := w.store.claimOf(1); got != "kickoff-deliverer" {
		t.Fatalf("the first turn's claim is now %q: the relay released or overwrote a claim it never held", got)
	}

	w.store.setClaim(1, "") // the first turn has finished
	tick(t, r)
	if got := len(w.gw.sent()); got != 1 {
		t.Fatalf("control: once the claim is free the memo must be delivered; turns = %d", got)
	}
}

// TestADeliveryHoldsTheSessionClaimForTheTurnAndReleasesIt: held while the turn runs (so a first
// turn owed meanwhile waits), released afterwards whatever the turn came to — including a turn
// cut off by shutdown, whose release must not ride the cancelled context.
func TestADeliveryHoldsTheSessionClaimForTheTurnAndReleasesIt(t *testing.T) {
	for name, fail := range map[string]error{"delivered": nil, "busy": busyErr(), "unconfirmed": errors.New("unexpected EOF")} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(agent(1, "ada", "example/widgets"))
			w.src.set("widgets", m(t, 7, "widgets", "2000-01-02T03:04:05Z"))
			w.gw.setFail("ada", fail)
			var during string
			w.gw.duringSend = func(string) { during = w.store.claimOf(1) }
			tick(t, w.relay(t))
			if fail == nil || unsent(fail) == "" {
				if !strings.Contains(during, "/memorelay/") {
					t.Fatalf("during the turn the agent's claim was %q, want this relay's", during)
				}
			}
			if got := w.store.claimOf(1); got != "" {
				t.Fatalf("after the delivery the claim is still held by %q", got)
			}
		})
	}

	w := newWorld(agent(1, "ada", "example/widgets"))
	w.src.set("widgets", m(t, 7, "widgets", "2000-01-02T03:04:05Z"))
	w.gw.block = true
	r := w.relay(t)
	ctx, cancel := context.WithCancel(authz.Operator(context.Background()))
	if err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(w.gw.sent()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if w.store.claimOf(1) == "" {
		t.Fatal("setup: the claim is not held while the turn is in flight")
	}
	cancel()
	r.Wait()
	if got := w.store.claimOf(1); got != "" {
		t.Fatalf("after a shutdown-cancelled turn the claim is still held by %q", got)
	}
}

// TestTheSessionClaimOutlivesTheTurnAndAnUnverifiedClaimSendsNothing: the real claim can be taken
// by anyone once its ttl has passed, so a ttl shorter than the turn it covers would let a first
// turn in while the memo's is still running — the collision the claim exists to prevent. And a
// claim the store could not verify is not a claim, whatever it answered.
func TestTheSessionClaimOutlivesTheTurnAndAnUnverifiedClaimSendsNothing(t *testing.T) {
	w := newWorld(agent(1, "ada", "example/widgets"))
	w.src.set("widgets", m(t, 7, "widgets", "2000-01-02T03:04:05Z"))
	tick(t, w.relay(t))
	if len(w.store.claimTTLs) != 1 {
		t.Fatalf("%d claim(s) won, want 1", len(w.store.claimTTLs))
	}
	// A claude-code turn may run for the kind's whole budget.
	if got, floor := w.store.claimTTLs[0], agents.KindTurnTimeout(agents.KindClaudeCode); got <= floor {
		t.Fatalf("the claim lives %s, which does not outlast a %s turn", got, floor)
	}

	w = newWorld(agent(1, "ada", "example/widgets"))
	w.src.set("widgets", m(t, 7, "widgets", "2000-01-02T03:04:05Z"))
	w.store.claimErr = errors.New("database unavailable")
	r := w.relay(t)
	tick(t, r)
	if len(w.gw.sent()) != 0 || r.Counts()["delivery/claim_failed"] != 1 {
		t.Fatalf("sent=%d counts=%v; want nothing sent on an unverified claim and delivery/claim_failed 1", len(w.gw.sent()), r.Counts())
	}
}

// TestTheAgentAndTheRecordAreReadAgainUnderTheClaim: both were read before the claim was taken.
// Between the two, a first turn can have become owed, and another process can have sent the batch.
func TestTheAgentAndTheRecordAreReadAgainUnderTheClaim(t *testing.T) {
	t.Run("the agent came to owe a first turn", func(t *testing.T) {
		w := newWorld(agent(1, "ada", "example/widgets"))
		w.src.set("widgets", m(t, 7, "widgets", "2000-01-02T03:04:05Z"))
		owed := agent(1, "ada", "example/widgets")
		owed.KickedOff, owed.PendingNote = false, "the task"
		w.store.getOverride = map[int64]agents.Agent{1: owed}
		r := w.relay(t)
		tick(t, r)
		if len(w.gw.sent()) != 0 || r.Counts()["delivery/moved"] != 1 {
			t.Fatalf("sent=%d counts=%v; want nothing sent and delivery/moved 1", len(w.gw.sent()), r.Counts())
		}
	})
	t.Run("the agent moved to another scope", func(t *testing.T) {
		w := newWorld(agent(1, "ada", "example/widgets"))
		w.src.set("widgets", m(t, 7, "widgets", "2000-01-02T03:04:05Z"))
		w.store.getOverride = map[int64]agents.Agent{1: agent(1, "ada", "example/gadgets")}
		r := w.relay(t)
		tick(t, r)
		if len(w.gw.sent()) != 0 || r.Counts()["delivery/moved"] != 1 {
			t.Fatalf("sent=%d counts=%v; want nothing sent and delivery/moved 1", len(w.gw.sent()), r.Counts())
		}
	})
	t.Run("another process sent the batch", func(t *testing.T) {
		w := newWorld(agent(1, "ada", "example/widgets"))
		w.src.set("widgets", m(t, 7, "widgets", "2000-01-02T03:04:05Z"), m(t, 8, "widgets", "2000-01-02T04:00:00Z"))
		once := sync.Once{}
		w.store.onGet = func(id int64) {
			once.Do(func() {
				_ = w.ledger.Record(context.Background(), agents.Agent{ID: id}, "widgets", []int64{7}, OutcomeDelivered)
			})
		}
		tick(t, w.relay(t))
		sent := w.gw.sent()
		if len(sent) != 1 || !reflect.DeepEqual(memoIDs(sent[0].message), []int64{8}) {
			t.Fatalf("turns = %d; want one turn carrying memo 8 alone (7 was sent elsewhere meanwhile)", len(sent))
		}
	})
	// Control: with nothing changing in between, the same world delivers both.
	w := newWorld(agent(1, "ada", "example/widgets"))
	w.src.set("widgets", m(t, 7, "widgets", "2000-01-02T03:04:05Z"), m(t, 8, "widgets", "2000-01-02T04:00:00Z"))
	tick(t, w.relay(t))
	if sent := w.gw.sent(); len(sent) != 1 || !reflect.DeepEqual(memoIDs(sent[0].message), []int64{7, 8}) {
		t.Fatalf("control: turns = %+v, want one carrying 7 and 8", sent)
	}
}

// ---- one repository, several scopes ------------------------------------------------------------

// sentKeys is every (scope, id) the gateway was handed, across all turns, with a count.
func sentKeys(t *testing.T, sent []sentTurn) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, s := range sent {
		for _, line := range strings.Split(s.message, "\n") {
			var id int64
			var scope string
			// "| [m-<id>] from <sender> · <scope> · <created> · expires <date>"
			if _, err := fmt.Sscanf(line, "| [m-%d] from", &id); err != nil {
				continue
			}
			parts := strings.Split(line, " · ")
			if len(parts) < 2 {
				t.Fatalf("unparseable stanza header %q", line)
			}
			scope = parts[1]
			out[fmt.Sprintf("%s/%s:%d", s.agent, scope, id)]++
		}
	}
	return out
}

// TestARepositoryMappedToTwoScopesReceivesBothAndEachMemoOnce: the deployment's map names two
// scopes for one repository (memos filed under the repository's name AND under the directory its
// clone lives in). The agent working on it is sent the memos of BOTH, and each exactly once —
// over several ticks and across a restart — including two memos that share an id in different
// scopes, which are distinct memos because a memo's identity is (scope, id).
func TestARepositoryMappedToTwoScopesReceivesBothAndEachMemoOnce(t *testing.T) {
	task := int64(40)
	viaTask := agent(2, "tasked", "")
	viaTask.NoteID = &task
	w := newWorld(agent(1, "ada", "example/widgets"), viaTask, agent(3, "cy", "example/gadgets"))
	w.tasks[40] = "example/widgets"
	w.scopeMap = ScopeMap{"widgets": {"widgets", "widgets-clone"}}
	w.src.set("widgets", m(t, 7, "widgets", "2000-01-02T03:04:05Z"))
	w.src.set("widgets-clone", m(t, 7, "widgets-clone", "2000-01-02T03:05:00Z"), m(t, 8, "widgets-clone", "2000-01-02T03:06:00Z"))
	w.src.set("gadgets", m(t, 9, "gadgets", "2000-01-02T03:04:05Z"))

	r := w.relay(t)
	for i := 0; i < 4; i++ {
		tick(t, r)
	}
	tick(t, w.relay(t)) // a restart

	want := map[string]int{
		"ada/widgets:7": 1, "ada/widgets-clone:7": 1, "ada/widgets-clone:8": 1,
		"tasked/widgets:7": 1, "tasked/widgets-clone:7": 1, "tasked/widgets-clone:8": 1,
		"cy/gadgets:9": 1,
	}
	if got := sentKeys(t, w.gw.sent()); !reflect.DeepEqual(got, want) {
		t.Fatalf("memos delivered (agent/scope:id → times) = %v\nwant %v", got, want)
	}
	if asked := w.src.askedFor(); !slices.Contains(asked, "widgets-clone") || slices.Contains(asked, "gadgets-clone") {
		t.Fatalf("the listener was asked for %v: want the mapped scope, and no scope for an unmapped repository's guess", asked)
	}
}

// TestAMappedRepositoryReadsOnlyItsSet: an entry REPLACES the default, so a repository mapped to
// one other scope does not also read its own name — and an unmapped repository is untouched.
func TestAMappedRepositoryReadsOnlyItsSet(t *testing.T) {
	w := newWorld(agent(1, "ada", "example/widgets"))
	w.scopeMap = ScopeMap{"widgets": {"widgets-clone"}}
	w.src.set("widgets", m(t, 7, "widgets", "2000-01-02T03:04:05Z"))
	w.src.set("widgets-clone", m(t, 8, "widgets-clone", "2000-01-02T03:04:05Z"))
	tick(t, w.relay(t))
	if got := sentKeys(t, w.gw.sent()); !reflect.DeepEqual(got, map[string]int{"ada/widgets-clone:8": 1}) {
		t.Fatalf("delivered %v, want the mapped scope's memo alone", got)
	}
}

// TestAMemoOfASecondMappedScopeIsStillDeliveredUnderTheClaim: the re-read under the claim must
// accept ANY scope of the agent's set, not only the first. Otherwise the second scope's memos
// are counted "moved" and never sent.
func TestAMemoOfASecondMappedScopeIsStillDeliveredUnderTheClaim(t *testing.T) {
	w := newWorld(agent(1, "ada", "example/widgets"))
	w.scopeMap = ScopeMap{"widgets": {"aaa-first", "widgets"}}
	w.src.set("widgets", m(t, 7, "widgets", "2000-01-02T03:04:05Z"))
	r := w.relay(t)
	tick(t, r)
	if got := sentKeys(t, w.gw.sent()); !reflect.DeepEqual(got, map[string]int{"ada/widgets:7": 1}) {
		t.Fatalf("delivered %v (counts %v), want memo 7 of the second scope", got, r.Counts())
	}
}

func TestParseScopeMap(t *testing.T) {
	got, err := ParseScopeMap(" widgets = widgets-clone , widgets ,widgets ; gadgets=gadgets ; ")
	if err != nil {
		t.Fatal(err)
	}
	want := ScopeMap{"widgets": {"widgets", "widgets-clone"}, "gadgets": {"gadgets"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed %v, want %v", got, want)
	}
	if s := got.String(); s != "gadgets=gadgets;widgets=widgets,widgets-clone" {
		t.Fatalf("String() = %q", s)
	}
	if m, err := ParseScopeMap(""); err != nil || len(m) != 0 {
		t.Fatalf("empty spec: %v, %v; want an empty map", m, err)
	}
	for _, bad := range []string{
		"widgets",               // no =
		"widgets=",              // no scope
		"widgets=a,,b",          // an empty scope
		"Widgets=widgets",       // not normalised
		"widgets=Widgets-Clone", // not normalised
		"widgets=a;widgets=b",   // a repository twice
		"wid gets=widgets",      // not normalised
		"=widgets",              // no repository
		"widgets=-x",            // not normalised
	} {
		if _, err := ParseScopeMap(bad); err == nil {
			t.Errorf("ParseScopeMap(%q) was accepted", bad)
		}
	}
}

func TestNewRefusesAnIncompleteConfiguration(t *testing.T) {
	w := newWorld()
	full := Config{Agents: w.store, Gateway: w.gw, Source: w.src, Ledger: w.ledger,
		TaskRepo: func(context.Context, int64) (string, error) { return "", nil }}
	if _, err := New(full); err != nil {
		t.Fatalf("the complete configuration was refused: %v", err)
	}
	for name, strip := range map[string]func(*Config){
		"Agents":   func(c *Config) { c.Agents = nil },
		"TaskRepo": func(c *Config) { c.TaskRepo = nil },
		"Gateway":  func(c *Config) { c.Gateway = nil },
		"Source":   func(c *Config) { c.Source = nil },
		"Ledger":   func(c *Config) { c.Ledger = nil },
	} {
		c := full
		strip(&c)
		if _, err := New(c); err == nil || !strings.Contains(err.Error(), "Config."+name) {
			t.Errorf("without %s: err = %v, want a refusal naming Config.%s", name, err, name)
		}
	}
}
