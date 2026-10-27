package api

import (
	"context"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// TestTheTurnBudgetOutlastsAKindsOwnBudget: a claude-code turn gets
// agents.ClaudeCodeTurnTimeout plus the delivery slack (37m — past ccd's 30m
// AND past the client's own 35m, so ccd's typed 504 always lands first); every
// other kind keeps the 30m it had. Literal durations, not the constants.
func TestTheTurnBudgetOutlastsAKindsOwnBudget(t *testing.T) {
	for kind, want := range map[string]time.Duration{
		agents.KindClaudeCode: 37 * time.Minute,
		agents.KindGateway:    30 * time.Minute,
		"":                    30 * time.Minute,
	} {
		if got := agentTurnBudget(kind); got != want {
			t.Errorf("agentTurnBudget(%q) = %v, want %v", kind, got, want)
		}
	}
	if got := routeHandbackClaimTTL(37 * time.Minute); got <= 37*time.Minute {
		t.Errorf("the hand-back claim (%v) does not outlast its turn", got)
	}
}

// deadlineGateway records the time left on each turn's context.
type deadlineGateway struct {
	turnGateway
	mu   sync.Mutex
	left map[string]time.Duration
}

func (g *deadlineGateway) ChatWithTools(ctx context.Context, a agents.Agent, key, instr, text string, tools []agents.ToolDef, d agents.ToolDispatch, emit agents.StreamEmit) (string, error) {
	if dl, ok := ctx.Deadline(); ok {
		g.mu.Lock()
		g.left[a.Name] = time.Until(dl)
		g.mu.Unlock()
	}
	return g.turnGateway.ChatWithTools(ctx, a, key, instr, text, tools, d, emit)
}

func (g *deadlineGateway) leftFor(name string) (time.Duration, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	d, ok := g.left[name]
	return d, ok
}

func near(got, want time.Duration) bool { return got > want-time.Minute && got <= want }

// TestAPostedTurnIsBudgetedByTheAgentsKind: the operator's POST send runs a
// claude-code agent's turn on the 37m budget and a gateway agent's on 30m —
// measured on the context the gateway is actually handed.
func TestAPostedTurnIsBudgetedByTheAgentsKind(t *testing.T) {
	store, gw := newSendAgents(), &deadlineGateway{left: map[string]time.Duration{}}
	s, ts := sendServer(t, store, gw)
	postAgent(t, s, ts, "keen-wren", url.Values{"text": {"cc turn"}, "session": {"41"}}, false)
	postAgent(t, s, ts, "plain-otter", url.Values{"text": {"gw turn"}, "session": {"52"}}, false)
	eventuallyAPI(t, "both turns", func() bool {
		_, a := gw.leftFor("keen-wren")
		_, b := gw.leftFor("plain-otter")
		return a && b
	})
	if d, _ := gw.leftFor("keen-wren"); !near(d, 37*time.Minute) {
		t.Errorf("a claude-code agent's posted turn has %v left, want ~37m", d)
	}
	if d, _ := gw.leftFor("plain-otter"); !near(d, 30*time.Minute) {
		t.Errorf("a gateway agent's posted turn has %v left, want ~30m", d)
	}
}

// claimTTLAgents is sendAgents that grants every session claim and records its TTL.
type claimTTLAgents struct {
	*sendAgents
	mu  sync.Mutex
	ttl time.Duration
}

func (c *claimTTLAgents) ClaimKickoff(_ context.Context, _ int64, _ string, ttl time.Duration) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ttl = ttl
	return true, nil
}
func (c *claimTTLAgents) ReleaseKickoffClaim(context.Context, int64, string) error { return nil }

// TestARouteHandBackIsBudgetedByTheAgentsKindAndItsClaimOutlastsIt: a hand-back
// to a claude-code agent runs on 37m, and the session claim it holds is longer
// than that turn, so the claim cannot lapse mid-turn.
func TestARouteHandBackIsBudgetedByTheAgentsKindAndItsClaimOutlastsIt(t *testing.T) {
	store := &claimTTLAgents{sendAgents: newSendAgents()}
	gw := &deadlineGateway{left: map[string]time.Duration{}}
	s, _ := sendServer(t, store.sendAgents, gw)
	s.ext.Agents = store
	agent := agents.Agent{ID: 7, Name: "keen-wren", Kind: agents.KindClaudeCode, CCAccount: "alpha", Status: agents.StatusRunning}
	if !s.startClaimedAgentTurn(asUser(1), agent, agents.ChatSession{ID: 41, AgentID: 7, SessionKey: "key-41"}, "rt_x", "hand back") {
		t.Fatal("the hand-back did not start")
	}
	eventuallyAPI(t, "the hand-back turn", func() bool { _, ok := gw.leftFor("keen-wren"); return ok })
	left, _ := gw.leftFor("keen-wren")
	if !near(left, 37*time.Minute) {
		t.Errorf("the hand-back turn has %v left, want ~37m", left)
	}
	store.mu.Lock()
	ttl := store.ttl
	store.mu.Unlock()
	if ttl <= 37*time.Minute {
		t.Errorf("the hand-back's claim is %v, not longer than its 37m turn", ttl)
	}
}
