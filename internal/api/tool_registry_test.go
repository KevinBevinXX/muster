package api

import (
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

// mcpOnlyLedger and gatewayOnlyLedger are the two halves of the registry with the
// connector store configured.
//
// 🔴 BOTH ARE PINNED AS LITERALS AND THE TEST FAILS WHEN EITHER SET GROWS OR
// SHRINKS. A new tool is a decision about which pods may call it: setting the
// MCP flag puts it on the connect listener, the one muster port a claude-code
// pod can reach. Adding a tool therefore means editing one of these lines, which
// is where that decision gets reviewed (agentTool's comment says why the
// existing tools are gateway-only).
var (
	mcpLedger         = []string{"connectors_list", "connector_request", "connector_status"}
	gatewayOnlyLedger = []string{"agent_get_task", "agent_comment_task", "agent_set_task_status",
		"agent_list_pull_requests"}
)

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

// TestTheMCPToolsAreTheRegistrysMCPSubset: what POST /mcp lists is, tool for
// tool and byte for byte (name, description, input schema), the registry's
// MCP-flagged entries — the same definitions the gateway kind is given — and the
// split between the two is the ledgered one.
func TestTheMCPToolsAreTheRegistrysMCPSubset(t *testing.T) {
	f := newConnFixture(t)
	f.seedAgent(t, "registry-agent", "registry-agent-token-0123456789")

	r, _ := f.mcp(t, "registry-agent-token-0123456789", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	if r == nil || r.Error != nil {
		t.Fatalf("tools/list: %+v", r)
	}
	var listed struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(r.Result, &listed); err != nil {
		t.Fatal(err)
	}
	// Positive control: the comparison below must have something to compare.
	if len(listed.Tools) == 0 {
		t.Fatal("positive control FAILED: tools/list returned no tools")
	}

	gateway := map[string]string{}
	var gatewayNames []string
	for _, d := range f.srv.AgentToolDefs() {
		gateway[d.Name] = normJSON(t, map[string]any{"name": d.Name, "description": d.Description, "inputSchema": d.Parameters})
		gatewayNames = append(gatewayNames, d.Name)
	}
	var mcpNames []string
	for _, tl := range listed.Tools {
		name, _ := tl["name"].(string)
		mcpNames = append(mcpNames, name)
		want, ok := gateway[name]
		if !ok {
			t.Errorf("/mcp lists %q, which the gateway tool defs do not have: a tool was added to one transport only", name)
			continue
		}
		if got := normJSON(t, tl); got != want {
			t.Errorf("/mcp's %q differs from the gateway's definition:\n  mcp:     %s\n  gateway: %s", name, got, want)
		}
	}
	if !reflect.DeepEqual(mcpNames, mcpLedger) {
		t.Errorf("/mcp tools = %v, want exactly %v", mcpNames, mcpLedger)
	}
	inMCP := map[string]bool{}
	for _, n := range mcpNames {
		inMCP[n] = true
	}
	var gatewayOnly []string
	for _, n := range gatewayNames {
		if !inMCP[n] {
			gatewayOnly = append(gatewayOnly, n)
		}
	}
	if !reflect.DeepEqual(gatewayOnly, gatewayOnlyLedger) {
		t.Errorf("gateway-only tools = %v, want exactly %v", gatewayOnly, gatewayOnlyLedger)
	}

	// A gateway-only tool is not callable over /mcp: the name is as unknown there
	// as a made-up one, even though dispatchAgentTool knows it.
	for _, n := range gatewayOnlyLedger {
		r, _ := f.mcp(t, "registry-agent-token-0123456789",
			`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"`+n+`","arguments":{}}}`, nil)
		if r == nil || r.Error == nil || r.Error.Code != -32602 {
			t.Errorf("tools/call %s over /mcp: %+v, want -32602 unknown tool", n, r)
		}
	}
}

// TestEveryRegisteredToolIsDispatched: a tool declared in the registry with no
// case in dispatchAgentTool would be listed and then answer "unknown tool".
func TestEveryRegisteredToolIsDispatched(t *testing.T) {
	f := newConnFixture(t)
	id := f.seedAgent(t, "dispatch-agent", "dispatch-agent-token-0123456789")
	a := agents.Agent{ID: id, Name: "dispatch-agent"}
	defs := f.srv.AgentToolDefs()
	if len(defs) != len(mcpLedger)+len(gatewayOnlyLedger) {
		t.Fatalf("positive control: %d tool defs, want %d", len(defs), len(mcpLedger)+len(gatewayOnlyLedger))
	}
	for _, d := range defs {
		if out := f.srv.dispatchAgentTool(f.ctx, a, d.Name, "{}"); out == toolErr("unknown tool %q", d.Name) {
			t.Errorf("%s is registered but not dispatched", d.Name)
		}
	}
	// Control: the comparison above can see an unknown tool.
	if out := f.srv.dispatchAgentTool(f.ctx, a, "no_such_tool", "{}"); out != toolErr("unknown tool %q", "no_such_tool") {
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
	for _, d := range bare.AgentToolDefs() {
		names = append(names, d.Name)
	}
	sort.Strings(names)
	want := append([]string(nil), gatewayOnlyLedger...)
	sort.Strings(want)
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("without a store the gateway is offered %v, want only %v", names, want)
	}
	if len(bare.MCPToolDefs()) != 0 {
		t.Fatalf("without a store /mcp would list %v", bare.MCPToolDefs())
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
