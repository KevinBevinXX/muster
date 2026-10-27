package api

import (
	"context"
	"io"
	"log"
	"net/http"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/plugins"
)

// TestAClaudeCodeChiefsGatewaySessionGetsTheChiefsProse: the gateway runtime
// beside ccd has no standing file of the agent's, so a turn routed to it from a
// claude-code agent carries the agent's function-tool prose as its instructions;
// every other turn — the same agent's legacy and Claude Code sessions, and a
// gateway-KIND agent's gateway session (its own process reads its own files) —
// carries what it always did.
func TestAClaudeCodeChiefsGatewaySessionGetsTheChiefsProse(t *testing.T) {
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Agents: stubAgentsStore{}, Plugins: struct{ plugins.Store }{}})
	cc := agents.Agent{ID: 3, Name: "agent", Kind: agents.KindClaudeCode, CCAccount: "acct"}
	gw := agents.Agent{ID: 4, Name: "agent-2", Kind: agents.KindGateway}
	route := func(rt string) context.Context {
		if rt == "" {
			return context.Background()
		}
		return agents.WithTurnRoute(context.Background(), agents.TurnRoute{Runtime: rt, Header: http.Header{}})
	}
	prose := agentspec.SupervisorInstructionsFunctionTools(true)
	for _, c := range []struct {
		name    string
		a       agents.Agent
		runtime string
		want    string
	}{
		{"cc agent, gateway session", cc, agents.SessionRuntimeGateway, prose},
		{"cc agent, claude-code session", cc, agents.SessionRuntimeClaudeCode, s.AgentInstructions(cc)},
		{"cc agent, legacy session", cc, "", s.AgentInstructions(cc)},
		{"gateway-kind agent, gateway session", gw, agents.SessionRuntimeGateway, s.AgentInstructions(gw)},
	} {
		if got := s.turnInstructions(route(c.runtime), c.a); got != c.want {
			t.Errorf("%s: got %d bytes starting %.40q, want %.40q", c.name, len(got), got, c.want)
		}
	}
	if prose == s.AgentInstructions(cc) {
		t.Fatal("control: the agent prose and the agent instructions are the same text, so this test sees nothing")
	}
}
