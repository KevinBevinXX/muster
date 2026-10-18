package plugins

import (
	"context"
	"reflect"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/db"
)

// mustCCAgent is mustAgent on the claude-code kind (its Claude account set, as
// the 0003 constraint requires of that kind).
func mustCCAgent(t *testing.T, dsn string, uid int64, name string) agents.Agent {
	t.Helper()
	pool, err := db.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	a, err := agents.NewPG(pool).Create(session(uid), agents.Agent{Name: name, Namespace: "devpod-" + name, DisplayName: "chief",
		Status: agents.StatusRunning, HooksToken: "hooks-" + name, Kind: agents.KindClaudeCode, CCAccount: "acct-x"})
	if err != nil {
		t.Fatalf("agent %s: %v", name, err)
	}
	return a
}

// TestDeliveringToListsOnlyThisAgentsInFlightMessages: the chief replacement's
// guard reads this, so it must see exactly the rows deleting the agent would
// strand — `delivering` to THIS agent — and not a queued one, a settled one, or
// one delivering to another agent. Ids are compared as a set in order.
func TestDeliveringToListsOnlyThisAgentsInFlightMessages(t *testing.T) {
	s, _, dsn := pgStore(t)
	_, p := mustCreate(t, s, 2, "example-inbox")
	a := mustAgent(t, dsn, 2, "plg-dt-chief")
	other := mustAgent(t, dsn, 2, "plg-dt-other")
	m1, _, _ := s.Submit(plugin(2), p.ID, msg("dt-1"))
	m2, _, _ := s.Submit(plugin(2), p.ID, msg("dt-2"))
	settled, _, _ := s.Submit(plugin(2), p.ID, msg("dt-3"))
	toOther, _, _ := s.Submit(plugin(2), p.ID, msg("dt-4"))
	_, _, _ = s.Submit(plugin(2), p.ID, msg("dt-5")) // stays queued

	if got, err := s.DeliveringTo(plugin(2), a.ID); err != nil || len(got) != 0 {
		t.Fatalf("before any delivery: %v %v, want none", got, err)
	}
	for _, c := range []struct{ msg, agent int64 }{{m1.ID, a.ID}, {m2.ID, a.ID}, {settled.ID, a.ID}, {toOther.ID, other.ID}} {
		if ok, err := s.BeginDelivery(plugin(2), c.msg, c.agent); err != nil || !ok {
			t.Fatalf("begin %d: %v %v", c.msg, ok, err)
		}
	}
	if ok, err := s.RecordDelivery(plugin(2), settled.ID, a.ID, StatusDelivered); err != nil || !ok {
		t.Fatalf("settle: %v %v", ok, err)
	}
	got, err := s.DeliveringTo(plugin(2), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{m1.ID, m2.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DeliveringTo = %v, want %v", got, want)
	}
	// Another owner's view of the same agent id sees nothing (row-level security).
	if got, err := s.DeliveringTo(plugin(3), a.ID); err != nil || len(got) != 0 {
		t.Fatalf("another owner sees %v (%v)", got, err)
	}
}

// TestAPluginMessageIsDeliveredToAClaudeCodeChief: delivery is kind-agnostic —
// a claude-code chief takes the fenced turn, the message is settled delivered to
// it, and the plugin's chief session is recorded on it.
//
// ⚠ AN INVARIANT GUARD, NOT A REGRESSION TEST: nothing in Deliver branched on the
// kind before this change either (beyond the turn budget), so this was green
// before it. It pins that a kind check is never added here.
func TestAPluginMessageIsDeliveredToAClaudeCodeChief(t *testing.T) {
	s, _, dsn := pgStore(t)
	pool, err := db.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	gw := &fakeGateway{reply: "noted"}
	d, err := NewDeliverer(DelivererConfig{Store: s, Agents: agents.NewPG(pool), Gateway: gw, Turn: gw.Turn})
	if err != nil {
		t.Fatal(err)
	}
	_, p := mustCreate(t, s, 2, "example-inbox")
	chief := mustCCAgent(t, dsn, 2, "plg-cc-chief")
	m, _, _ := s.Submit(plugin(2), p.ID, msg("cc-1"))
	if r := d.Deliver(plugin(2), p.ID, m.ID, chief); r.Outcome != OutcomeDelivered || len(gw.sent) != 1 {
		t.Fatalf("delivery to a claude-code chief: %+v, %d sends", r, len(gw.sent))
	}
	got, _ := s.Message(plugin(2), m.ID)
	if got.Status != StatusDelivered || got.AgentID == nil || *got.AgentID != chief.ID {
		t.Fatalf("message after delivery = %+v", got)
	}
	if pp, _ := s.Plugin(plugin(2), p.ID); pp.ChiefAgentID == nil || *pp.ChiefAgentID != chief.ID {
		t.Fatalf("the plugin's chief session is not on the claude-code chief: %+v", pp)
	}
}
