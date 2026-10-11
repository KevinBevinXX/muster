package api

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ui"
)

// The approval relay (approvals.go): a claude-code pod's permission prompt
// reaches the operator's approval queue THROUGH muster, on the agent's own
// token, and the agent can read back only what it filed.

const (
	relayHookToken    = "hook-token-for-relay-tests-0123456789"
	relayServiceToken = "service-token-for-relay-tests-0123456789"
	relayPassword     = "operator-password-for-tests"
	relayTokenA       = "agent-a-own-token-0123456789"
	relayTokenB       = "agent-b-own-token-0123456789"
)

// relayAgents resolves agents by their own token and by id.
type relayAgents struct {
	agents.Store
	byToken map[string]agents.Agent
}

func (r relayAgents) GetByHooksToken(_ context.Context, tok string) (agents.Agent, error) {
	a, ok := r.byToken[tok]
	if !ok {
		return agents.Agent{}, errors.New("no such agent")
	}
	return a, nil
}

func (r relayAgents) Get(_ context.Context, id int64) (agents.Agent, error) {
	for _, a := range r.byToken {
		if a.ID == id {
			return a, nil
		}
	}
	return agents.Agent{}, errors.New("no such agent")
}

// fakeGate is the approval queue: it records what was filed, read and cleared.
type fakeGate struct {
	mu      sync.Mutex
	minted  []GateSpec
	reads   []string
	cleared []string
	state   GateDecision
	mintErr error
	readErr error
	nextID  int
	// states, when non-empty, is answered in order by ReadGate (then state).
	states []GateDecision
	// block, when non-nil, holds every MintGate until it is closed; inflight
	// counts the mints waiting on it.
	block    chan struct{}
	inflight int
}

func (g *fakeGate) MintGate(_ context.Context, spec GateSpec) (string, error) {
	g.mu.Lock()
	if b := g.block; b != nil {
		g.inflight++
		g.mu.Unlock()
		<-b
		g.mu.Lock()
	}
	defer g.mu.Unlock()
	if g.mintErr != nil {
		return "", g.mintErr
	}
	g.nextID++
	g.minted = append(g.minted, spec)
	return "req-" + strconv.Itoa(g.nextID), nil
}

func (g *fakeGate) ReadGate(_ context.Context, id string) (GateDecision, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reads = append(g.reads, id)
	if g.readErr != nil {
		return GateDecision{}, g.readErr
	}
	if len(g.states) > 0 {
		d := g.states[0]
		g.states = g.states[1:]
		return d, nil
	}
	return g.state, nil
}

func (g *fakeGate) ClearGate(_ context.Context, id string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cleared = append(g.cleared, id)
	return nil
}

type relayFixture struct {
	srv  *Server
	h    http.Handler
	gate *fakeGate
	now  time.Time
}

var (
	relayAgentA = agents.Agent{ID: 11, Name: "bold-hawk", Status: agents.StatusRunning}
	relayAgentB = agents.Agent{ID: 12, Name: "brave-heron", Status: agents.StatusRunning}
)

func newRelayFixture(t *testing.T, withGate bool) *relayFixture {
	t.Helper()
	s := New(nil, AuthConfig{HookToken: relayHookToken, ServiceToken: relayServiceToken, UIPassword: relayPassword},
		log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Agents: relayAgents{byToken: map[string]agents.Agent{
		relayTokenA: relayAgentA, relayTokenB: relayAgentB,
		// The worst case: an agent row whose token EQUALS the shared hook token.
		relayHookToken: {ID: 13, Name: "collides-with-hook"},
	}}})
	f := &relayFixture{srv: s, now: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}
	s.now = func() time.Time { return f.now }
	if withGate {
		f.gate = &fakeGate{state: GateDecision{State: GateStatePending}}
		s.UseApprovalGate(f.gate)
	}
	f.h = s.Handler()
	return f
}

func (f *relayFixture) call(method, path, body, token string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	if token != "" {
		r.Header.Set("X-Muster-Token", token)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

const relayBody = `{"tool":"Bash","command":"rm -rf build","cwd":"/data/workspace","session":"s1",` +
	`"context":["clean the build dir"],"host":"laptop","project":"some-other-project"}`

var relayRoutes = []struct{ method, path, body string }{
	{"POST", "/agent/approvals", relayBody},
	{"GET", "/agent/approvals/req-1", ""},
	{"DELETE", "/agent/approvals/req-1", ""},
	{"POST", "/agent/blocked", `{"blocked":true}`},
}

// 🔴 THE SHARED TOKENS ARE REFUSED BY NAME — even though an agent row's token
// equals the hook token here, so a lookup-first door would admit it.
func TestTheRelayRefusesSharedTokensAndStrangers(t *testing.T) {
	f := newRelayFixture(t, true)
	for _, rt := range relayRoutes {
		for name, tok := range map[string]string{"hook": relayHookToken, "service": relayServiceToken} {
			if w := f.call(rt.method, rt.path, rt.body, tok); w.Code != http.StatusForbidden {
				t.Errorf("%s %s with the %s token: %d %s, want 403", rt.method, rt.path, name, w.Code, w.Body)
			}
		}
		for _, tok := range []string{"", "nobody-has-this-token-0123"} {
			if w := f.call(rt.method, rt.path, rt.body, tok); w.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with token %q: %d, want 401", rt.method, rt.path, tok, w.Code)
			}
		}
	}
	if len(f.gate.minted) != 0 || len(f.gate.reads) != 0 || len(f.gate.cleared) != 0 {
		t.Fatalf("a refused caller reached the queue: %+v", f.gate)
	}
}

// The card names the agent that asked; the caller cannot choose Host or
// Project, and the type is the auto-approval-exempt gate type.
func TestAFiledCardNamesTheAgentNotTheCallersClaim(t *testing.T) {
	f := newRelayFixture(t, true)
	w := f.call("POST", "/agent/approvals", relayBody, relayTokenA)
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"requestId":"req-1"`) {
		t.Fatalf("file: %d %s", w.Code, w.Body)
	}
	got := f.gate.minted[0]
	want := GateSpec{Type: "checkpoint", Tool: "Bash", Command: "rm -rf build", Host: "muster-agent/bold-hawk",
		Project: "bold-hawk", Cwd: "/data/workspace", Session: "s1", Context: []string{"clean the build dir"}}
	if got.Type != want.Type || got.Tool != want.Tool || got.Command != want.Command || got.Host != want.Host ||
		got.Project != want.Project || got.Cwd != want.Cwd || got.Session != want.Session ||
		strings.Join(got.Context, "|") != strings.Join(want.Context, "|") {
		t.Fatalf("minted %+v\nwant   %+v", got, want)
	}
}

// 🔴 AN AGENT READS AND CLEARS ONLY WHAT IT FILED. Another agent's id answers
// the same 404 as a nonexistent one, and the queue is never touched for it.
func TestAnAgentCannotReadOrClearAnotherAgentsRequest(t *testing.T) {
	f := newRelayFixture(t, true)
	f.gate.state = GateDecision{State: GateStateDecided, Response: "approve"}
	if w := f.call("POST", "/agent/approvals", relayBody, relayTokenA); w.Code != http.StatusCreated {
		t.Fatalf("file: %d", w.Code)
	}
	for _, m := range []string{"GET", "DELETE"} {
		w := f.call(m, "/agent/approvals/req-1", "", relayTokenB)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s by another agent: %d %s, want 404", m, w.Code, w.Body)
		}
	}
	if w := f.call("GET", "/agent/approvals/req-404", "", relayTokenA); w.Code != http.StatusNotFound {
		t.Fatalf("GET of a nonexistent id: %d", w.Code)
	}
	if len(f.gate.reads) != 0 || len(f.gate.cleared) != 0 {
		t.Fatalf("the queue was reached for a request the caller did not file: reads %v cleared %v", f.gate.reads, f.gate.cleared)
	}
	// Control: the filer reads the decision.
	w := f.call("GET", "/agent/approvals/req-1", "", relayTokenA)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"response":"approve"`) {
		t.Fatalf("filer read: %d %s", w.Code, w.Body)
	}
	// A decided request is forgotten: no second read.
	if w := f.call("GET", "/agent/approvals/req-1", "", relayTokenA); w.Code != http.StatusNotFound {
		t.Fatalf("second read of a decided request: %d, want 404", w.Code)
	}
}

func TestThereIsNoRouteThatDecides(t *testing.T) {
	f := newRelayFixture(t, true)
	f.call("POST", "/agent/approvals", relayBody, relayTokenA)
	for _, rt := range []struct{ m, p string }{
		{"POST", "/agent/approvals/req-1"}, {"PUT", "/agent/approvals/req-1"}, {"PATCH", "/agent/approvals/req-1"},
		{"POST", "/agent/approvals/req-1/decision"}, {"POST", "/agent/approvals/req-1/approve"},
	} {
		w := f.call(rt.m, rt.p, `{"response":"approve"}`, relayTokenA)
		if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: %d, want 404/405", rt.m, rt.p, w.Code)
		}
	}
}

func TestTheRelayFailsClosedWithoutAQueue(t *testing.T) {
	f := newRelayFixture(t, false)
	if w := f.call("POST", "/agent/approvals", relayBody, relayTokenA); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("file with no queue: %d, want 503", w.Code)
	}
	g := newRelayFixture(t, true)
	g.gate.mintErr = errors.New("router down")
	if w := g.call("POST", "/agent/approvals", relayBody, relayTokenA); w.Code != http.StatusBadGateway {
		t.Fatalf("file with the queue unreachable: %d, want 502", w.Code)
	}
	if g.srv.AwaitingApproval(relayAgentA.ID) {
		t.Fatal("a request that was never filed counts as awaiting")
	}
	g.gate.mintErr = nil
	g.call("POST", "/agent/approvals", relayBody, relayTokenA)
	g.gate.readErr = errors.New("router down")
	if w := g.call("GET", "/agent/approvals/req-1", "", relayTokenA); w.Code != http.StatusBadGateway {
		t.Fatalf("read with the queue unreachable: %d, want 502 (never a state)", w.Code)
	}
}

// The outstanding cap, sequentially. The rate limit is lifted so it cannot be
// what refuses (its burst is below the cap); TestAFileAndClearLoopIsRateLimited
// covers the rate.
func TestOneAgentCannotHoldMoreThanTheCapOpen(t *testing.T) {
	f := newRelayFixture(t, true)
	f.srv.approvals.burst = 100
	for i := 0; i < approvalMaxOutstanding; i++ {
		if w := f.call("POST", "/agent/approvals", relayBody, relayTokenA); w.Code != http.StatusCreated {
			t.Fatalf("file %d: %d", i, w.Code)
		}
	}
	if w := f.call("POST", "/agent/approvals", relayBody, relayTokenA); w.Code != http.StatusTooManyRequests {
		t.Fatalf("file past the cap: %d, want 429", w.Code)
	}
	// Another agent is unaffected.
	if w := f.call("POST", "/agent/approvals", relayBody, relayTokenB); w.Code != http.StatusCreated {
		t.Fatalf("another agent's file: %d", w.Code)
	}
}

func TestAwaitingApprovalFollowsTheRelayAndTheTerminalReport(t *testing.T) {
	f := newRelayFixture(t, true)
	a := relayAgentA.ID
	if f.srv.AwaitingApproval(a) {
		t.Fatal("awaiting before anything happened")
	}
	// 1. A filed request.
	f.call("POST", "/agent/approvals", relayBody, relayTokenA)
	if !f.srv.AwaitingApproval(a) || f.srv.AwaitingApproval(relayAgentB.ID) {
		t.Fatal("a filed request did not mark exactly its agent awaiting")
	}
	f.call("DELETE", "/agent/approvals/req-1", "", relayTokenA)
	if f.srv.AwaitingApproval(a) {
		t.Fatal("a cleared request still counts")
	}
	// 2. The terminal's report, and its expiry without a heartbeat.
	if w := f.call("POST", "/agent/blocked", `{"blocked":true}`, relayTokenA); w.Code != http.StatusNoContent {
		t.Fatalf("blocked report: %d", w.Code)
	}
	if !f.srv.AwaitingApproval(a) {
		t.Fatal("a blocked report did not mark the agent awaiting")
	}
	f.call("POST", "/agent/blocked", `{"blocked":false}`, relayTokenA)
	if f.srv.AwaitingApproval(a) {
		t.Fatal("a cleared report still counts")
	}
	f.call("POST", "/agent/blocked", `{"blocked":true}`, relayTokenA)
	f.now = f.now.Add(blockedReportTTL + time.Second)
	if f.srv.AwaitingApproval(a) {
		t.Fatal("a report never refreshed still counts after its TTL")
	}
	// 3. A filed request expires too (a pod that died mid-wait).
	f.call("POST", "/agent/approvals", relayBody, relayTokenA)
	f.now = f.now.Add(approvalOwnerTTL + time.Second)
	if f.srv.AwaitingApproval(a) {
		t.Fatal("a filed request outlived its TTL")
	}
}

// The badge is rendered BY the polled status endpoint, so the poll that shows it
// is also the poll that clears it.
func TestTheStatusPollRendersTheAwaitingBadgeOnlyWhileAwaiting(t *testing.T) {
	f := newRelayFixture(t, true)
	poll := func() string {
		r := httptest.NewRequest("GET", "/ui/agents/"+strconv.FormatInt(relayAgentA.ID, 10)+"/status", nil)
		r.Header.Set("Cookie", sessionCookieFor(f.srv))
		w := httptest.NewRecorder()
		f.h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status poll: %d %s", w.Code, w.Body)
		}
		return w.Body.String()
	}
	badge := `data-badge="` + ui.AwaitingApprovalBadgeID + `"`
	if body := poll(); strings.Contains(body, badge) || !strings.Contains(body, `aria-label="Status: running"`) {
		t.Fatalf("idle poll: %s", body)
	}
	f.call("POST", "/agent/blocked", `{"blocked":true}`, relayTokenA)
	if body := poll(); strings.Count(body, badge) != 1 || !strings.Contains(body, `aria-label="Status: running"`) {
		t.Fatalf("blocked poll, want the status dot AND one badge: %s", body)
	}
	f.call("POST", "/agent/blocked", `{"blocked":false}`, relayTokenA)
	if body := poll(); strings.Contains(body, badge) {
		t.Fatalf("poll after the clear still shows the badge: %s", body)
	}
}

// The dedicated connect listener — the only muster port an isolated pod may
// reach — serves the relay.
func TestTheConnectListenerServesTheRelay(t *testing.T) {
	f := newRelayFixture(t, true)
	h := OnlyConnectSurface(f.h)
	for _, rt := range relayRoutes {
		r := httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))
		r.Header.Set("X-Muster-Token", relayTokenA)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusNotFound && !strings.Contains(w.Body.String(), "approval request") {
			t.Errorf("%s %s: 404 through the connect listener, want it served", rt.method, rt.path)
		}
	}
	for _, p := range []string{"/agent/approvalsx", "/agent/blockedx", "/agent/task"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: %d through the connect listener, want 404", p, w.Code)
		}
	}
}

// --- round 1 of the #48 audit ---------------------------------------------

var (
	gone    = GateDecision{State: GateStateGone}
	pending = GateDecision{State: GateStatePending}
	approve = GateDecision{State: GateStateDecided, Response: "approve"}
)

func (f *relayFixture) poll(t *testing.T) (int, string) {
	t.Helper()
	w := f.call("GET", "/agent/approvals/req-1", "", relayTokenA)
	return w.Code, w.Body.String()
}

// 🔴 ONE "gone" FROM THE ROUTER IS NOT EVIDENCE: its store reports a card absent
// on any query error. A single gone read must leave the card owned and the agent
// waiting, so the operator's later approval still reaches it.
func TestASingleGoneReadDoesNotOrphanTheCard(t *testing.T) {
	f := newRelayFixture(t, true)
	f.call("POST", "/agent/approvals", relayBody, relayTokenA)
	f.gate.states = []GateDecision{gone, approve}
	if code, body := f.poll(t); code != http.StatusOK || !strings.Contains(body, `"state":"pending"`) {
		t.Fatalf("first (transient) gone: %d %s, want 200 pending", code, body)
	}
	if code, body := f.poll(t); code != http.StatusOK || !strings.Contains(body, `"response":"approve"`) {
		t.Fatalf("after the transient gone the approval was lost: %d %s", code, body)
	}
	if len(f.gate.cleared) != 0 {
		t.Fatalf("a live card was cleared: %v", f.gate.cleared)
	}
}

// Three gone reads IN A ROW: muster believes it, forgets the id and clears the
// card so nothing stays approvable that nobody waits on.
func TestConsecutiveGoneReadsForgetAndClearTheCard(t *testing.T) {
	f := newRelayFixture(t, true)
	f.call("POST", "/agent/approvals", relayBody, relayTokenA)
	f.gate.state = gone
	for i := 1; i < approvalGoneReads; i++ {
		if _, body := f.poll(t); !strings.Contains(body, `"state":"pending"`) {
			t.Fatalf("gone read %d: %s, want pending until %d in a row", i, body, approvalGoneReads)
		}
	}
	if code, body := f.poll(t); code != http.StatusOK || !strings.Contains(body, `"state":"gone"`) {
		t.Fatalf("gone read %d: %d %s, want gone", approvalGoneReads, code, body)
	}
	if strings.Join(f.gate.cleared, ",") != "req-1" {
		t.Fatalf("the gone card was not cleared on the router: %v", f.gate.cleared)
	}
	if code, _ := f.poll(t); code != http.StatusNotFound {
		t.Fatalf("after giving up, the id still answers %d", code)
	}
}

// The streak is CONSECUTIVE: any other state in between restarts it.
func TestAPendingReadResetsTheGoneStreak(t *testing.T) {
	f := newRelayFixture(t, true)
	f.call("POST", "/agent/approvals", relayBody, relayTokenA)
	f.gate.states = []GateDecision{gone, gone, pending, gone, gone}
	for i := 0; i < 5; i++ {
		if _, body := f.poll(t); !strings.Contains(body, `"state":"pending"`) {
			t.Fatalf("read %d: %s, want pending (no %d gone reads in a row)", i, body, approvalGoneReads)
		}
	}
}

// 🔴 THE CAP DOES NOT BOUND A FILE-AND-CLEAR LOOP; THE MINT RATE DOES. Each
// iteration has at most one card open, and each mint is a Web Push.
func TestAFileAndClearLoopIsRateLimited(t *testing.T) {
	f := newRelayFixture(t, true)
	limited := 0
	for i := 0; i < 10; i++ {
		w := f.call("POST", "/agent/approvals", relayBody, relayTokenA)
		if w.Code == http.StatusTooManyRequests {
			limited++
			continue
		}
		if w.Code != http.StatusCreated {
			t.Fatalf("file %d: %d", i, w.Code)
		}
		f.call("DELETE", "/agent/approvals/req-"+strconv.Itoa(f.gate.nextID), "", relayTokenA)
	}
	if n := len(f.gate.minted); n != approvalMintBurst || limited != 10-approvalMintBurst {
		t.Fatalf("10 file-and-clear rounds minted %d cards (%d refused), want %d minted", n, limited, approvalMintBurst)
	}
	// One token refills per approvalMintEvery.
	f.now = f.now.Add(approvalMintEvery)
	if w := f.call("POST", "/agent/approvals", relayBody, relayTokenA); w.Code != http.StatusCreated {
		t.Fatalf("after %s: %d, want one refilled mint", approvalMintEvery, w.Code)
	}
	if w := f.call("POST", "/agent/approvals", relayBody, relayTokenA); w.Code != http.StatusTooManyRequests {
		t.Fatalf("a second mint in the same window: %d, want 429", w.Code)
	}
	// Another agent has its own bucket.
	if w := f.call("POST", "/agent/approvals", relayBody, relayTokenB); w.Code != http.StatusCreated {
		t.Fatalf("another agent: %d", w.Code)
	}
}

// 🔴 PARALLEL FILES CANNOT EXCEED THE CAP: the slot is reserved under the lock
// before the network call. The rate limit is lifted here so it cannot be what
// stops them (its burst is below the cap).
func TestParallelFilesCannotExceedTheOutstandingCap(t *testing.T) {
	f := newRelayFixture(t, true)
	f.srv.approvals.burst = 100
	f.gate.block = make(chan struct{})
	const n = 10
	codes := make(chan int, n)
	for i := 0; i < n; i++ {
		go func() { codes <- f.call("POST", "/agent/approvals", relayBody, relayTokenA).Code }()
	}
	refused := 0
	deadline := time.After(10 * time.Second)
	for refused < n-approvalMaxOutstanding {
		select {
		case c := <-codes:
			if c != http.StatusTooManyRequests {
				t.Fatalf("a POST finished with %d while mints were held", c)
			}
			refused++
		case <-deadline:
			f.gate.mu.Lock()
			in := f.gate.inflight
			f.gate.mu.Unlock()
			close(f.gate.block)
			t.Fatalf("only %d of %d parallel POSTs were refused; %d mints in flight (cap %d)",
				refused, n, in, approvalMaxOutstanding)
		}
	}
	close(f.gate.block)
	for i := 0; i < approvalMaxOutstanding; i++ {
		if c := <-codes; c != http.StatusCreated {
			t.Fatalf("a held mint finished with %d", c)
		}
	}
	if len(f.gate.minted) != approvalMaxOutstanding {
		t.Fatalf("minted %d, want %d", len(f.gate.minted), approvalMaxOutstanding)
	}
}

// --- round 2 of the #48 audit ---------------------------------------------

// fileAndSettle files one card at the fixture's clock and lets the agent's poll
// see d (enough polls to settle a gone card too), returning the POST's status.
func (f *relayFixture) fileAndSettle(t *testing.T, d GateDecision) int {
	t.Helper()
	w := f.call("POST", "/agent/approvals", relayBody, relayTokenA)
	if w.Code != http.StatusCreated {
		return w.Code
	}
	id := "req-" + strconv.Itoa(f.gate.nextID)
	f.gate.state = d
	for i := 0; i < approvalGoneReads; i++ {
		if c := f.call("GET", "/agent/approvals/"+id, "", relayTokenA).Code; c != http.StatusOK {
			break
		}
	}
	return http.StatusCreated
}

// 🔴 A HUMAN ANSWERING PROMPTS BACK TO BACK IS NOT A FLOOD. Every approve or
// deny gives the card's mint token back, so prompts arriving every 3s (well
// under approvalMintEvery) are never pushed to the terminal.
func TestAnOperatorAnsweringBackToBackIsNeverThrottled(t *testing.T) {
	f := newRelayFixture(t, true)
	for i := 0; i < 30; i++ {
		d := approve
		if i%2 == 1 {
			d = GateDecision{State: GateStateDecided, Response: "reject"}
		}
		if code := f.fileAndSettle(t, d); code != http.StatusCreated {
			t.Fatalf("prompt #%d (3s apart, each answered by a human): %d, want 201", i+1, code)
		}
		f.now = f.now.Add(3 * time.Second)
	}
}

// No refund without a human decision: a clear, a confirmed gone and a dismissal
// leave the filing loop limited to the burst at a frozen clock.
func TestOnlyAHumanDecisionRefundsAMint(t *testing.T) {
	cases := map[string]func(f *relayFixture, t *testing.T) int{
		"clear": func(f *relayFixture, t *testing.T) int {
			w := f.call("POST", "/agent/approvals", relayBody, relayTokenA)
			if w.Code == http.StatusCreated {
				f.call("DELETE", "/agent/approvals/req-"+strconv.Itoa(f.gate.nextID), "", relayTokenA)
			}
			return w.Code
		},
		"gone": func(f *relayFixture, t *testing.T) int { return f.fileAndSettle(t, gone) },
		"dismiss": func(f *relayFixture, t *testing.T) int {
			return f.fileAndSettle(t, GateDecision{State: GateStateDecided, Response: "ignore"})
		},
	}
	for name, settle := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRelayFixture(t, true)
			created := 0
			for i := 0; i < 10; i++ {
				if settle(f, t) == http.StatusCreated {
					created++
				}
			}
			if created != approvalMintBurst {
				t.Fatalf("10 %s rounds at a frozen clock minted %d, want %d (no refund)", name, created, approvalMintBurst)
			}
		})
	}
}

// A FAILED mint spends its token: a router that keeps failing must not turn the
// relay into an unthrottled retry loop.
func TestAFailedMintSpendsAToken(t *testing.T) {
	f := newRelayFixture(t, true)
	f.gate.mintErr = errors.New("router down")
	for i := 0; i < approvalMintBurst; i++ {
		if w := f.call("POST", "/agent/approvals", relayBody, relayTokenA); w.Code != http.StatusBadGateway {
			t.Fatalf("failed mint %d: %d, want 502", i, w.Code)
		}
	}
	f.gate.mintErr = nil
	if w := f.call("POST", "/agent/approvals", relayBody, relayTokenA); w.Code != http.StatusTooManyRequests {
		t.Fatalf("after %d failed mints: %d, want 429 (failed mints spend tokens)", approvalMintBurst, w.Code)
	}
}
