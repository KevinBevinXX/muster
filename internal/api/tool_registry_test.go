package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// The ONE agent tool registry (agentTools, agent.go) and its two transports.

// mcpLedger and gatewayOnlyLedger are the two halves of the registry with the
// connector, flow and plugin stores configured, as the CHIEF sees it;
// chiefOnlyLedger is the part of it no other agent is offered.
//
// 🔴 BOTH ARE PINNED AS LITERALS AND THE TEST FAILS WHEN EITHER SET GROWS OR
// SHRINKS. A new tool is a decision about which pods may call it: setting the
// MCP flag puts it on the connect listener, the one muster port a claude-code
// pod can reach. Adding a tool therefore means editing one of these lines, which
// is where that decision gets reviewed (agentTool's comment says why the
// existing tools are gateway-only).
var (
	mcpLedger = []string{"connectors_list", "connector_request", "connector_status",
		"browser_flow_request", "browser_flow_status", "task_create_gated", "reply_draft",
		"agents_list", "focus_search", "focus_refresh"}
	gatewayOnlyLedger = []string{"agent_get_task", "agent_comment_task", "agent_set_task_status",
		"agent_list_pull_requests"}
	// 🔴 chiefOnlyLedger IS PINNED THE SAME WAY: a tool joining it is offered to
	// one agent per owner and refused to every other, and a tool LEAVING it is
	// offered to every worker in the fleet. Either is a decision to review here.
	chiefOnlyLedger = []string{"task_create_gated", "reply_draft", "agents_list", "focus_search", "focus_refresh"}
)

// without returns list minus the names in drop, in order.
func without(list, drop []string) []string {
	skip := map[string]bool{}
	for _, d := range drop {
		skip[d] = true
	}
	var out []string
	for _, n := range list {
		if !skip[n] {
			out = append(out, n)
		}
	}
	return out
}

// mcpListNames is what POST /mcp tools/list answers for token, by name.
func mcpListNames(t *testing.T, f *connFixture, token string) ([]string, []map[string]any) {
	t.Helper()
	r, _ := f.mcp(t, token, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	if r == nil || r.Error != nil {
		t.Fatalf("tools/list: %+v", r)
	}
	var listed struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(r.Result, &listed); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range listed.Tools {
		name, _ := tl["name"].(string)
		names = append(names, name)
	}
	return names, listed.Tools
}

func normJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(m)
	return string(out)
}

// TestTheMCPToolsAreTheRegistrysMCPSubset: what POST /mcp lists for an agent is,
// tool for tool and byte for byte (name, description, input schema), the
// MCP-flagged entries of what the registry offers THAT agent — the same
// definitions its gateway turns are given — and the split is the ledgered one,
// for the chief and (minus the chief-only tools) for a worker.
func TestTheMCPToolsAreTheRegistrysMCPSubset(t *testing.T) {
	g := newGatedFixture(t)

	for _, c := range []struct {
		name    string
		agent   agents.Agent
		token   string
		wantMCP []string
	}{
		{"chief", g.chief, g.chiefTok, mcpLedger},
		{"worker", g.worker, g.workerTok, without(mcpLedger, chiefOnlyLedger)},
	} {
		t.Run(c.name, func(t *testing.T) {
			mcpNames, tools := mcpListNames(t, g.connFixture, c.token)
			// Positive control: the comparison below must have something to compare.
			if len(tools) == 0 {
				t.Fatal("positive control FAILED: tools/list returned no tools")
			}
			gateway := map[string]string{}
			var gatewayNames []string
			for _, d := range g.srv.AgentToolDefsFor(g.asOwner, c.agent) {
				gateway[d.Name] = normJSON(t, map[string]any{"name": d.Name, "description": d.Description, "inputSchema": d.Parameters})
				gatewayNames = append(gatewayNames, d.Name)
			}
			for _, tl := range tools {
				name, _ := tl["name"].(string)
				want, ok := gateway[name]
				if !ok {
					t.Errorf("/mcp lists %q, which the gateway tool defs do not have: a tool was added to one transport only", name)
					continue
				}
				if got := normJSON(t, tl); got != want {
					t.Errorf("/mcp's %q differs from the gateway's definition:\n  mcp:     %s\n  gateway: %s", name, got, want)
				}
			}
			if !reflect.DeepEqual(mcpNames, c.wantMCP) {
				t.Errorf("/mcp tools = %v, want exactly %v", mcpNames, c.wantMCP)
			}
			if got := without(gatewayNames, mcpNames); !reflect.DeepEqual(got, gatewayOnlyLedger) {
				t.Errorf("gateway-only tools = %v, want exactly %v", got, gatewayOnlyLedger)
			}

			// A gateway-only tool is not callable over /mcp: the name is as unknown
			// there as a made-up one, even though dispatchAgentTool knows it.
			for _, n := range gatewayOnlyLedger {
				r, _ := g.mcp(t, c.token,
					`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"`+n+`","arguments":{}}}`, nil)
				if r == nil || r.Error == nil || r.Error.Code != -32602 {
					t.Errorf("tools/call %s over /mcp: %+v, want -32602 unknown tool", n, r)
				}
			}
		})
	}
}

// TestTheChiefOnlyToolsAreLedgered: the registry's chief-only set is exactly
// chiefOnlyLedger with the plugin store wired, and EMPTY without it (a chief-only
// tool with plugins off would be offered to the chief and answer "unconfigured").
func TestTheChiefOnlyToolsAreLedgered(t *testing.T) {
	g := newGatedFixture(t)
	chiefOnly := func(s *Server) []string {
		var out []string
		for _, tl := range s.agentTools() {
			if tl.chiefOnly {
				out = append(out, tl.def.Name)
			}
		}
		return out
	}
	if got := chiefOnly(g.srv); !reflect.DeepEqual(got, chiefOnlyLedger) {
		t.Errorf("chief-only tools = %v, want exactly %v", got, chiefOnlyLedger)
	}
	if got := chiefOnly(newConnFixture(t).srv); len(got) != 0 {
		t.Errorf("with no plugin store the registry still declares chief-only tools %v", got)
	}
	// Behavioural half: each ledgered tool is offered to the chief and to nobody else.
	names := func(a agents.Agent) map[string]bool {
		m := map[string]bool{}
		for _, d := range g.srv.AgentToolDefsFor(g.asOwner, a) {
			m[d.Name] = true
		}
		return m
	}
	chief, worker := names(g.chief), names(g.worker)
	for _, n := range chiefOnlyLedger {
		if !chief[n] {
			t.Errorf("the chief is not offered %s", n)
		}
		if worker[n] {
			t.Errorf("a worker is offered the chief-only %s", n)
		}
	}
}

// TestEveryRegisteredToolIsDispatched: a tool declared in the registry with no
// case in dispatchAgentTool would be listed and then answer "unknown tool".
func TestEveryRegisteredToolIsDispatched(t *testing.T) {
	g := newGatedFixture(t)
	defs := g.srv.AgentToolDefsFor(g.asOwner, g.chief)
	if len(defs) != len(mcpLedger)+len(gatewayOnlyLedger) {
		t.Fatalf("positive control: %d tool defs, want %d", len(defs), len(mcpLedger)+len(gatewayOnlyLedger))
	}
	for _, d := range defs {
		if out := g.srv.dispatchAgentTool(g.asOwner, g.chief, d.Name, "{}"); out == toolErr("unknown tool %q", d.Name) {
			t.Errorf("%s is registered but not dispatched", d.Name)
		}
	}
	// Control: the comparison above can see an unknown tool.
	if out := g.srv.dispatchAgentTool(g.asOwner, g.chief, "no_such_tool", "{}"); out != toolErr("unknown tool %q", "no_such_tool") {
		t.Fatalf("control: an unregistered name answered %s", out)
	}
}

// TestTheGatewayKindGetsTheConnectorToolsOnItsOwnPath: the gateway's native tool
// path (AgentToolDispatch) files a request as the calling agent, exactly as the
// MCP call does — and without a store the tools are not offered at all.
func TestTheGatewayKindGetsTheConnectorToolsOnItsOwnPath(t *testing.T) {
	f := newConnFixture(t)
	id := f.seedAgent(t, "gw-agent", "gw-agent-token-0123456789")
	out := f.srv.AgentToolDispatch(f.ctx, agents.Agent{ID: id, Name: "gw-agent"})("connector_request",
		`{"name":"github","reason":"push a branch"}`)
	if !strings.Contains(out, `"grant":"pending"`) {
		t.Fatalf("connector_request on the gateway path: %s", out)
	}
	pending, _ := f.store.ListPendingGrants(f.ctx)
	if len(pending) != 1 || pending[0].AgentName != "gw-agent" || pending[0].Reason != "push a branch" {
		t.Fatalf("the gateway call did not file the agent's request: %+v", pending)
	}

	bare := New(nil, AuthConfig{HookToken: connTestHookToken, UIPassword: connTestPassword}, log.New(io.Discard, "", 0))
	bare.UseExtensions(Extensions{Agents: stubAgentsStore{}})
	var names []string
	for _, d := range bare.AgentToolDefsFor(context.Background(), agents.Agent{ID: 1, Name: "bare"}) {
		names = append(names, d.Name)
	}
	sort.Strings(names)
	want := append([]string(nil), gatewayOnlyLedger...)
	sort.Strings(want)
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("without a store the gateway is offered %v, want only %v", names, want)
	}
	if got := bare.MCPToolDefsFor(context.Background(), agents.Agent{ID: 1, Name: "bare"}); len(got) != 0 {
		t.Fatalf("without a store /mcp would list %v", got)
	}
}

// TestTheProxyRecipeHasOneSource: the MCP initialize instructions and the
// gateway agent's prompt both carry ProxyRecipe's sentence verbatim, so a change
// to the recipe reaches every place that teaches it.
func TestTheProxyRecipeHasOneSource(t *testing.T) {
	f := newConnFixture(t)
	f.seedAgent(t, "recipe-agent", "recipe-agent-token-0123456789")
	r, _ := f.mcp(t, "recipe-agent-token-0123456789",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`, nil)
	var res struct {
		Instructions string `json:"instructions"`
	}
	if r == nil || json.Unmarshal(r.Result, &res) != nil {
		t.Fatalf("initialize: %+v", r)
	}
	if !strings.Contains(res.Instructions, ProxyRecipe("<connector>", "<host>")) {
		t.Fatalf("the MCP instructions do not carry ProxyRecipe's sentence:\n%s", res.Instructions)
	}
	if !strings.Contains(ProxyUsage("github", []string{"api.github.com"}), ProxyRecipe("github", "api.github.com")) {
		t.Fatal("ProxyUsage does not carry ProxyRecipe's sentence")
	}
}

// TestTheCloseOutProtocolClaimsNoGitHubToken: agentspec gives a provisioned pod
// no GITHUB_TOKEN and no credential helper, so the prompt must not say one is
// configured — with the store it points at the github connector instead.
func TestTheCloseOutProtocolClaimsNoGitHubToken(t *testing.T) {
	note := int64(7)
	a := agents.Agent{ID: 1, Name: "closer", Repo: "example-org/example-repo", NoteID: &note}

	f := newConnFixture(t)
	with := f.srv.AgentInstructions(a)
	bare := New(nil, AuthConfig{HookToken: connTestHookToken, UIPassword: connTestPassword}, log.New(io.Discard, "", 0))
	bare.UseExtensions(Extensions{Agents: stubAgentsStore{}})
	without := bare.AgentInstructions(a)

	for name, text := range map[string]string{"with the store": with, "without the store": without} {
		// Positive control: this IS the close-out text being checked.
		if !strings.Contains(text, "CLOSE THE LOOP") {
			t.Fatalf("%s: positive control FAILED, no close-out protocol in the prompt", name)
		}
		for _, claim := range []string{"GITHUB_TOKEN", "credential helper", "already configured"} {
			if strings.Contains(text, claim) {
				t.Errorf("%s: the prompt still says %q", name, claim)
			}
		}
	}
	for _, want := range []string{`connector_request{name:"github"`, ProxyRecipe("github", "github.com"),
		"/connect/github/github.com/example-org/example-repo.git"} {
		if !strings.Contains(with, want) {
			t.Errorf("with the store, the close-out does not contain %q", want)
		}
	}
	if strings.Contains(without, "connector_request") {
		t.Error("without the store, the prompt points at a tool the agent is not given")
	}
}
