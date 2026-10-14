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

// fakeGateway records each Send and answers with err (or reply).
type fakeGateway struct {
	mu       sync.Mutex
	err      error
	reply    string
	sent     []string
	sessions []string
}

func (f *fakeGateway) Resolve(_ context.Context, a agents.Agent) (agentgateway.Target, error) {
	return agentgateway.Target{}, nil
}

func (f *fakeGateway) Send(_ context.Context, _ agentgateway.Target, key, message string, _ func(string)) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	d, err := NewDeliverer(DelivererConfig{Store: s, Agents: ag, Gateway: gw})
	if err != nil {
		t.Fatal(err)
	}
	_, p := mustCreate(t, s, 2, "example-inbox")
	chief := mustAgent(t, dsn, 2, "plg-chief")
	return s, ag, gw, d, p, chief
}

func TestADeliveredMessageIsAFencedTurnInThePluginsOwnSession(t *testing.T) {
	s, ag, gw, d, p, chief := deliverFixture(t)
	m1, _, _ := s.Submit(plugin(2), p.ID, msg("m-1"))
	m2, _, _ := s.Submit(plugin(2), p.ID, msg("m-2"))
	for _, m := range []Message{m1, m2} {
		if r := d.Deliver(plugin(2), p.ID, m.ID, chief); r.Outcome != OutcomeDelivered {
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
	if got.Status != StatusDelivered || got.AgentID == nil || *got.AgentID != chief.ID {
		t.Fatalf("message after delivery = %+v", got)
	}
	// The session is the chief's, recorded on the plugin, titled by the label,
	// and the transcript carries the turn and the reply.
	pp, _ := s.Plugin(plugin(2), p.ID)
	if pp.ChiefAgentID == nil || *pp.ChiefAgentID != chief.ID || pp.ChiefSessionID == nil {
		t.Fatalf("plugin's chief session not recorded: %+v", pp)
	}
	sess, err := ag.GetSession(plugin(2), *pp.ChiefSessionID)
	if err != nil || !strings.HasPrefix(sess.Title, "Plugin Example inbox") {
		t.Fatalf("session %+v (%v); want titled by the plugin's label", sess, err)
	}
	msgs, _ := ag.ListChatMessages(plugin(2), sess.ID)
	if len(msgs) != 4 || msgs[0].Content != turn || msgs[1].Content != "noted" {
		t.Fatalf("transcript has %d messages; want turn, reply, turn, reply", len(msgs))
	}
	// A settled message is never sent again.
	if r := d.Deliver(plugin(2), p.ID, m1.ID, chief); r.Outcome != OutcomeSettled || len(gw.sent) != 2 {
		t.Fatalf("redelivering a delivered message: %+v, %d sends; want settled and no send", r, len(gw.sent))
	}
}

func TestAProvablyUnsentAttemptStaysQueuedAndAnUnprovenOneIsNeverResent(t *testing.T) {
	s, _, gw, d, p, chief := deliverFixture(t)
	m, _, _ := s.Submit(plugin(2), p.ID, msg("u-1"))

	gw.err = &agents.RuntimeError{Status: 409, Type: "busy"}
	if r := d.Deliver(plugin(2), p.ID, m.ID, chief); r.Outcome != OutcomePending || r.Reason != "busy" {
		t.Fatalf("a typed busy: %+v; want pending/busy", r)
	}
	gw.err = &net.OpError{Op: "dial", Err: errors.New("refused")}
	if r := d.Deliver(plugin(2), p.ID, m.ID, chief); r.Outcome != OutcomePending || r.Reason != "never_connected" {
		t.Fatalf("a dial error: %+v; want pending/never_connected", r)
	}
	got, _ := s.Message(plugin(2), m.ID)
	if got.Status != StatusQueued || got.Attempts != 2 {
		t.Fatalf("after two provably-unsent attempts: %+v; want queued, attempts 2", got)
	}
	gw.err = context.DeadlineExceeded // may have been pasted
	if r := d.Deliver(plugin(2), p.ID, m.ID, chief); r.Outcome != OutcomeUnconfirmed {
		t.Fatalf("an unproven failure: %+v; want unconfirmed", r)
	}
	gw.err = nil
	before := len(gw.sent)
	if r := d.Deliver(plugin(2), p.ID, m.ID, chief); r.Outcome != OutcomeSettled || len(gw.sent) != before {
		t.Fatalf("an unconfirmed message offered again: %+v; want settled and no send", r)
	}
}

func TestNothingIsSentToAChiefThatCannotTakeItOrIsNotTheOwners(t *testing.T) {
	s, ag, gw, d, p, chief := deliverFixture(t)
	m, _, _ := s.Submit(plugin(2), p.ID, msg("n-1"))
	// Another owner's agent, whatever the caller resolved.
	foreign := chief
	foreign.OwnerID = 3
	if r := d.Deliver(plugin(2), p.ID, m.ID, foreign); r.Outcome != OutcomePending || r.Reason != "not_owners_chief" {
		t.Fatalf("another owner's chief: %+v", r)
	}
	// No chief at all.
	if r := d.Deliver(plugin(2), p.ID, m.ID, agents.Agent{}); r.Outcome != OutcomePending || r.Reason != "no_chief" {
		t.Fatalf("no chief: %+v", r)
	}
	// The session claim held by a first turn.
	if won, err := ag.ClaimKickoff(plugin(2), chief.ID, "someone-else", time.Minute); err != nil || !won {
		t.Fatalf("claim: %v %v", won, err)
	}
	if r := d.Deliver(plugin(2), p.ID, m.ID, chief); r.Outcome != OutcomePending || r.Reason != "session_claimed" {
		t.Fatalf("claim held: %+v", r)
	}
	if err := ag.ReleaseKickoffClaim(plugin(2), chief.ID, "someone-else"); err != nil {
		t.Fatal(err)
	}
	// A chief that is not running.
	if err := ag.UpdateStatus(plugin(2), chief.ID, agents.StatusStopped, "", ""); err != nil {
		t.Fatal(err)
	}
	if r := d.Deliver(plugin(2), p.ID, m.ID, chief); r.Outcome != OutcomePending || r.Reason != "chief_not_running" {
		t.Fatalf("stopped chief: %+v", r)
	}
	if len(gw.sent) != 0 {
		t.Fatalf("%d sends; want none", len(gw.sent))
	}
	got, _ := s.Message(plugin(2), m.ID)
	if got.Status != StatusQueued || got.Attempts != 4 {
		t.Fatalf("message = %+v; want queued with 4 counted attempts", got)
	}
}
