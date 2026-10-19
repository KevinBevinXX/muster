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
// beside ccd has no standing file of the chief's, so a turn routed to it from a
// claude-code chief carries the chief's function-tool prose as its instructions;
// every other turn — the same chief's legacy and Claude Code sessions, and a
// gateway-KIND agent's gateway session (its own process reads its own files) —
// carries what it always did.
func TestAClaudeCodeChiefsGatewaySessionGetsTheChiefsProse(t *testing.T) {
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Agents: stubAgentsStore{}, Plugins: struct{ plugins.Store }{}})
	cc := agents.Agent{ID: 3, Name: "chief", Kind: agents.KindClaudeCode, CCAccount: "acct"}
	gw := agents.Agent{ID: 4, Name: "chief-2", Kind: agents.KindGateway}
	route := func(rt string) context.Context {
		if rt == "" {
			return context.Background()
		}
		return agents.WithTurnRoute(context.Background(), agents.TurnRoute{Runtime: rt, Header: http.Header{}})
	}
	prose := agentspec.ChiefInstructionsFunctionTools(true)
	for _, c := range []struct {
		name    string
		a       agents.Agent
		runtime string
		want    string
	}{
		{"cc chief, gateway session", cc, agents.SessionRuntimeGateway, prose},
		{"cc chief, claude-code session", cc, agents.SessionRuntimeClaudeCode, s.AgentInstructions(cc)},
		{"cc chief, legacy session", cc, "", s.AgentInstructions(cc)},
		{"gateway-kind chief, gateway session", gw, agents.SessionRuntimeGateway, s.AgentInstructions(gw)},
	} {
		if got := s.turnInstructions(route(c.runtime), c.a); got != c.want {
			t.Errorf("%s: got %d bytes starting %.40q, want %.40q", c.name, len(got), got, c.want)
		}
	}
	if prose == s.AgentInstructions(cc) {
		t.Fatal("control: the chief prose and the agent instructions are the same text, so this test sees nothing")
	}
}
