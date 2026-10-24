package api

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// chiefMCPToolList is the claude-code chief's WHOLE tool list over /mcp with
// every gate open (everyExtensionOn: what a deployment with the plugin
// interface, session focus, session and task routing with dispatch, the
// connector and flow stores and the attention queue gives it), in registry
// order. A literal on purpose: the chief's model carries this list on every
// turn, so a tool joining it, or one it was relieved of coming back, is a
// decision to make here and not a side effect.
var chiefMCPToolList = []string{
	"connectors_list", "task_create_gated", "reply_draft", "fleet_list", "focus_search", "sessions_overview",
	"route_to_session", "task_route_attention", "task_forward", "task_dispatch",
	"attention_ask", "attention_wait", "attention_cancel", "tasks_list", "task_get", "task_comment", "task_set_status",
	"attention_list",
}

// notChiefToolList are the registry's notChief tools: offered to every agent
// but the chief.
var notChiefToolList = []string{"connector_request", "connector_status", "browser_flow_request", "browser_flow_status"}

func chiefAndWorkerServer(t *testing.T) (*Server, agents.Agent, agents.Agent) {
	t.Helper()
	s := everyExtensionOn(t)
	chief := agents.Agent{ID: 1, Name: "chief", OwnerID: 1}
	worker := agents.Agent{ID: 2, Name: "a-worker", OwnerID: 1}
	s.ext.Agents = listAgents{list: []agents.Agent{chief, worker}}
	return s, chief, worker
}

func toolNames(defs []agents.ToolDef) []string {
	var out []string
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}

// TestTheChiefsToolListIsPinned: the chief's /mcp tool list is exactly
// chiefMCPToolList; its gateway list is that plus the gateway-only tools; a
// worker is still offered every notChief tool; and the chief calling a notChief
// tool by name is refused (a worker's call is served: the pg tests, e.g.
// TestTheGatewayKindGetsTheConnectorToolsOnItsOwnPath).
func TestTheChiefsToolListIsPinned(t *testing.T) {
	s, chief, worker := chiefAndWorkerServer(t)
	ctx := context.Background()
	if got := toolNames(s.MCPToolDefsFor(ctx, chief)); !reflect.DeepEqual(got, chiefMCPToolList) {
		t.Errorf("the chief's /mcp tools (%d) = %v,\nwant exactly (%d) %v", len(got), got, len(chiefMCPToolList), chiefMCPToolList)
	}
	gw := toolNames(s.AgentToolDefsFor(ctx, chief))
	if want := append(append([]string(nil), gatewayOnlyLedger...), chiefMCPToolList...); len(gw) != len(want) {
		t.Errorf("the chief's gateway tools (%d) = %v, want the %d gateway-only plus the %d MCP ones", len(gw), gw,
			len(gatewayOnlyLedger), len(chiefMCPToolList))
	}
	offered := map[string]bool{}
	for _, n := range toolNames(s.MCPToolDefsFor(ctx, worker)) {
		offered[n] = true
	}
	for _, n := range notChiefToolList {
		// Positive control: the worker IS offered it, so the chief's lack of it is
		// the axis and not a closed gate.
		if !offered[n] {
			t.Errorf("a worker is not offered %s", n)
		}
		if got := s.dispatchAgentTool(ctx, chief, n, "{}"); got != toolErr("%s is not one of your owner's chief agent's tools", n) {
			t.Errorf("the chief calling %s by name: %s", n, got)
		}
	}
	for _, n := range []string{"focus_refresh", "session_transcript"} {
		if got := s.dispatchAgentTool(ctx, chief, n, "{}"); got != toolErr("unknown tool %q", n) {
			t.Errorf("the removed %s answered the chief %s", n, got)
		}
	}
}

// TestTheChiefsMCPInstructionsNameNoToolItIsNotOffered: the initialize
// instructions the chief gets name none of the tools it was relieved of, and
// still carry the proxy recipe; a worker's still teach connector_request.
func TestTheChiefsMCPInstructionsNameNoToolItIsNotOffered(t *testing.T) {
	s, chief, worker := chiefAndWorkerServer(t)
	ctx := context.Background()
	in := s.mcpInstructionsFor(ctx, chief)
	for _, n := range append([]string{"focus_refresh", "session_transcript"}, notChiefToolList...) {
		if strings.Contains(in, n) {
			t.Errorf("the chief's MCP instructions name %s:\n%s", n, in)
		}
	}
	if !strings.Contains(in, ProxyRecipe("<connector>", "<host>")) || !strings.Contains(in, "connectors_list") {
		t.Errorf("the chief's MCP instructions lost the proxy recipe or connectors_list:\n%s", in)
	}
	if w := s.mcpInstructionsFor(ctx, worker); !strings.Contains(w, "connector_request") {
		t.Errorf("a worker's MCP instructions no longer teach connector_request:\n%s", w)
	}
}
