package plugins

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/db"
)

// fakeGateway resolves every agent and records each Turn, answering with err
// (or reply).
type fakeGateway struct {
	mu       sync.Mutex
	err      error
	reply    string
	sent     []string
	sessions []string
	// events are streamed to the turn's emit before it answers (a tool-running turn).
	events []agents.StreamEvent
	// during, when set, runs inside the turn (what a tool the agent calls sees).
	during func()
}

func (f *fakeGateway) Resolve(_ context.Context, a agents.Agent) (agentgateway.Target, error) {
	return agentgateway.Target{}, nil
}

func (f *fakeGateway) Turn(_ context.Context, _ agents.Agent, key, message string, emit agents.StreamEmit) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.during != nil {
		f.during()
	}
	for _, ev := range f.events {
		emit(ev)
	}
	f.sent = append(f.sent, message)
	f.sessions = append(f.sessions, key)
	if f.err != nil {
		return "", f.err
	}
	return f.reply, nil
}

func deliverFixture(t *testing.T) (*PGStore, agents.Store, *fakeGateway, *Deliverer, Plugin, agents.Agent) {
	t.Helper()
	s, _, dsn := pgStore(t)
	pool, err := db.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ag := agents.NewPG(pool)
	gw := &fakeGateway{reply: "noted"}
	d, err := NewDeliverer(DelivererConfig{Store: s, Agents: ag, Gateway: gw, Turn: gw.Turn})
	if err != nil {
		t.Fatal(err)
	}
	_, p := mustCreate(t, s, 2, "example-inbox")
	target := mustAgent(t, dsn, 2, "plg-target")
	return s, ag, gw, d, p, target
}

func TestADeliveredMessageIsAFencedTurnInThePluginsOwnSession(t *testing.T) {
	s, ag, gw, d, p, target := deliverFixture(t)
	m1, _, _ := s.Submit(plugin(2), p.ID, msg("m-1"))
	m2, _, _ := s.Submit(plugin(2), p.ID, msg("m-2"))
	for _, m := range []Message{m1, m2} {
		if r := d.Deliver(plugin(2), p.ID, m.ID, target); r.Outcome != OutcomeDelivered {
			t.Fatalf("message %d: %+v; want delivered", m.ID, r)
		}
	}
	if len(gw.sent) != 2 || gw.sessions[0] != gw.sessions[1] {
		t.Fatalf("sends %d in sessions %v; want two turns in ONE session", len(gw.sent), gw.sessions)
	}
	turn := gw.sent[0]
	if !strings.HasPrefix(turn, "Plugin Example inbox: message_id ") ||
		!strings.Contains(turn, "goes to scope example-inbox only.") ||
		!strings.Contains(turn, "<<<plugin-message untrusted nonce=") ||
		!strings.Contains(turn, "|   body: body of m-1") {
		t.Fatalf("the delivered turn is not the fenced block:\n%s", turn)
	}
	got, _ := s.Message(plugin(2), m1.ID)
	if got.Status != StatusDelivered || got.AgentID == nil || *got.AgentID != target.ID {
		t.Fatalf("message after delivery = %+v", got)
	}
	// The session is the target agent's, recorded on the plugin, titled by the label,
	// and the transcript carries the turn and the reply.
	pp, _ := s.Plugin(plugin(2), p.ID)
	if pp.SessionAgentID == nil || *pp.SessionAgentID != target.ID || pp.SessionID == nil {
		t.Fatalf("plugin's delivery session not recorded: %+v", pp)
	}
	sess, err := ag.GetSession(plugin(2), *pp.SessionID)
	if err != nil || !strings.HasPrefix(sess.Title, "Plugin Example inbox") {
		t.Fatalf("session %+v (%v); want titled by the plugin's label", sess, err)
	}
	msgs, _ := ag.ListChatMessages(plugin(2), sess.ID)
	if len(msgs) != 4 || msgs[0].Content != turn || msgs[1].Content != "noted" {
		t.Fatalf("transcript has %d messages; want turn, reply, turn, reply", len(msgs))
	}
	// A settled message is never sent again.
	if r := d.Deliver(plugin(2), p.ID, m1.ID, target); r.Outcome != OutcomeSettled || len(gw.sent) != 2 {
		t.Fatalf("redelivering a delivered message: %+v, %d sends; want settled and no send", r, len(gw.sent))
	}
}

func TestAProvablyUnsentAttemptStaysQueuedAndAnUnprovenOneIsNeverResent(t *testing.T) {
	s, _, gw, d, p, target := deliverFixture(t)
	m, _, _ := s.Submit(plugin(2), p.ID, msg("u-1"))

	gw.err = &agents.RuntimeError{Status: 409, Type: "busy"}
	if r := d.Deliver(plugin(2), p.ID, m.ID, target); r.Outcome != OutcomePending || r.Reason != "busy" {
		t.Fatalf("a typed busy: %+v; want pending/busy", r)
	}
	gw.err = &net.OpError{Op: "dial", Err: errors.New("refused")}
	if r := d.Deliver(plugin(2), p.ID, m.ID, target); r.Outcome != OutcomePending || r.Reason != "never_connected" {
		t.Fatalf("a dial error: %+v; want pending/never_connected", r)
	}
	got, _ := s.Message(plugin(2), m.ID)
	if got.Status != StatusQueued || got.Attempts != 2 || got.AgentID != nil {
		t.Fatalf("after two provably-unsent attempts: %+v; want queued, attempts 2, no target named", got)
	}
	gw.err = context.DeadlineExceeded // may have been pasted
	if r := d.Deliver(plugin(2), p.ID, m.ID, target); r.Outcome != OutcomeUnconfirmed {
		t.Fatalf("an unproven failure: %+v; want unconfirmed", r)
	}
	gw.err = nil
	before := len(gw.sent)
	if r := d.Deliver(plugin(2), p.ID, m.ID, target); r.Outcome != OutcomeSettled || len(gw.sent) != before {
		t.Fatalf("an unconfirmed message offered again: %+v; want settled and no send", r)
	}
}

func TestNothingIsSentToATargetThatCannotTakeItOrIsNotTheOwners(t *testing.T) {
	s, ag, gw, d, p, target := deliverFixture(t)
	m, _, _ := s.Submit(plugin(2), p.ID, msg("n-1"))
	// Another owner's agent, whatever the caller resolved.
	foreign := target
	foreign.OwnerID = 3
	if r := d.Deliver(plugin(2), p.ID, m.ID, foreign); r.Outcome != OutcomePending || r.Reason != "not_owners_agent" {
		t.Fatalf("another owner's agent: %+v", r)
	}
	// No target at all.
	if r := d.Deliver(plugin(2), p.ID, m.ID, agents.Agent{}); r.Outcome != OutcomePending || r.Reason != "no_target" {
		t.Fatalf("no target: %+v", r)
	}
	// The session claim held by a first turn.
	if won, err := ag.ClaimKickoff(plugin(2), target.ID, "someone-else", time.Minute); err != nil || !won {
		t.Fatalf("claim: %v %v", won, err)
	}
	if r := d.Deliver(plugin(2), p.ID, m.ID, target); r.Outcome != OutcomePending || r.Reason != "session_claimed" {
		t.Fatalf("claim held: %+v", r)
	}
	if err := ag.ReleaseKickoffClaim(plugin(2), target.ID, "someone-else"); err != nil {
		t.Fatal(err)
	}
	// A target that is not running.
	if err := ag.UpdateStatus(plugin(2), target.ID, agents.StatusStopped, "", ""); err != nil {
		t.Fatal(err)
	}
	if r := d.Deliver(plugin(2), p.ID, m.ID, target); r.Outcome != OutcomePending || r.Reason != "target_not_running" {
		t.Fatalf("stopped target: %+v", r)
	}
	if len(gw.sent) != 0 {
		t.Fatalf("%d sends; want none", len(gw.sent))
	}
	got, _ := s.Message(plugin(2), m.ID)
	if got.Status != StatusQueued || got.Attempts != 4 {
		t.Fatalf("message = %+v; want queued with 4 counted attempts", got)
	}
}

// TestATurnThatCallsToolsIsStoredAsTheChatTurnStoresIt: the delivered turn runs
// with tools, so what the target did on it — a tool call, its result, the text
// around them — is stored in order in the plugin's session, as an ordinary chat
// turn's parts are, and not collapsed to the final text.
func TestATurnThatCallsToolsIsStoredAsTheChatTurnStoresIt(t *testing.T) {
	s, ag, gw, d, p, target := deliverFixture(t)
	gw.events = []agents.StreamEvent{
		{Kind: "text", Text: "filing it"},
		{Kind: "tool_call", ToolID: "c1", ToolName: "agent_get_task", ToolArgs: `{}`},
		{Kind: "tool_result", ToolID: "c1", ToolName: "agent_get_task", ToolOK: true, ToolOutput: `{"ok":true}`},
		{Kind: "text", Text: "done"},
	}
	m, _, _ := s.Submit(plugin(2), p.ID, msg("tools-1"))
	if r := d.Deliver(plugin(2), p.ID, m.ID, target); r.Outcome != OutcomeDelivered {
		t.Fatalf("%+v; want delivered", r)
	}
	pp, _ := s.Plugin(plugin(2), p.ID)
	msgs, _ := ag.ListChatMessages(plugin(2), *pp.SessionID)
	var got []string
	for _, c := range msgs {
		got = append(got, c.Role+"/"+c.Kind+"/"+c.ToolName+"/"+c.Content)
	}
	want := []string{"user/text//" + gw.sent[0], "assistant/text//filing it",
		"assistant/tool_call/agent_get_task/{}", `assistant/tool_result/agent_get_task/{"ok":true}`, "assistant/text//done"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("stored turn:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestTheChiefCanCiteTheMessageOnTheTurnThatDeliversIt pins finding 2 of round 1
// at the deliverer: DURING the turn — when the target's tools run — the message is
// already `delivering` to THAT target, so a tool that accepts only a message
// delivered to the calling target (task_create_gated, reply_draft) accepts it on
// the turn that tells the target about it. Before, it was `queued` with no agent
// until after the turn, and the core flow could not cite it at all.
func TestTheChiefCanCiteTheMessageOnTheTurnThatDeliversIt(t *testing.T) {
	s, _, gw, d, p, target := deliverFixture(t)
	m, _, _ := s.Submit(plugin(2), p.ID, msg("cite-1"))
	var during Message
	gw.during = func() { during, _ = s.Message(plugin(2), m.ID) }
	if r := d.Deliver(plugin(2), p.ID, m.ID, target); r.Outcome != OutcomeDelivered {
		t.Fatalf("%+v; want delivered", r)
	}
	if during.Status != StatusDelivering || during.AgentID == nil || *during.AgentID != target.ID {
		t.Fatalf("the message as the target's tools see it mid-turn: status %q agent %v; want delivering to %d",
			during.Status, during.AgentID, target.ID)
	}
}

// TestADeliveryLeftMidTurnByADeadProcessIsSettledUnconfirmedAndNeverResent: a
// process that died mid-turn leaves its message `delivering` (and its session
// claim lapses). The next delivery to the same target — which now holds the claim,
// so no live delivery to it can be in flight — settles that row unconfirmed;
// it is never offered to the runtime again.
func TestADeliveryLeftMidTurnByADeadProcessIsSettledUnconfirmedAndNeverResent(t *testing.T) {
	s, _, gw, d, p, target := deliverFixture(t)
	dead, _, _ := s.Submit(plugin(2), p.ID, msg("dead-1"))
	if ok, err := s.BeginDelivery(plugin(2), dead.ID, target.ID); err != nil || !ok {
		t.Fatalf("the dead process's begin: %v %v", ok, err)
	}
	// Retrying the stuck message itself sends nothing.
	if r := d.Deliver(plugin(2), p.ID, dead.ID, target); r.Outcome != OutcomeSettled || len(gw.sent) != 0 {
		t.Fatalf("redelivering a message left delivering: %+v, %d sends; want settled, none", r, len(gw.sent))
	}
	if got, _ := s.Message(plugin(2), dead.ID); got.Status != StatusUnconfirmed {
		t.Fatalf("the dead process's message after the next delivery: %+v; want unconfirmed", got)
	}
	// A new message to the target is delivered normally.
	m, _, _ := s.Submit(plugin(2), p.ID, msg("next-1"))
	if r := d.Deliver(plugin(2), p.ID, m.ID, target); r.Outcome != OutcomeDelivered || len(gw.sent) != 1 {
		t.Fatalf("the next message: %+v, %d sends; want delivered once", r, len(gw.sent))
	}
}
