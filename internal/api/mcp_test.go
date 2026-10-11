package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

type mcpReply struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *mcpError       `json:"error"`
}

func (f *connFixture) mcp(t *testing.T, token, body string, extra map[string]string) (*mcpReply, int) {
	t.Helper()
	hdr := map[string]string{"X-Muster-Token": token, "Content-Type": "application/json", "Accept": "application/json, text/event-stream"}
	for k, v := range extra {
		hdr[k] = v
	}
	w := f.call(t, "POST", "/mcp", strings.NewReader(body), hdr)
	if w.Code != http.StatusOK {
		return nil, w.Code
	}
	var r mcpReply
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("not JSON-RPC: %s", w.Body)
	}
	return &r, w.Code
}

// toolText unwraps a tools/call result's single text content.
func toolText(t *testing.T, r *mcpReply) (string, bool) {
	t.Helper()
	var res struct {
		Content []struct {
			Type, Text string
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil || len(res.Content) != 1 || res.Content[0].Type != "text" {
		t.Fatalf("tool result shape: %s", r.Result)
	}
	return res.Content[0].Text, res.IsError
}

// TestTheMCPServerSpeaksTheToolProtocolAsTheCallingAgent drives a client's real
// sequence — initialize, the initialized notification, tools/list, tools/call —
// and checks the tools act as the agent whose token made the call.
func TestTheMCPServerSpeaksTheToolProtocolAsTheCallingAgent(t *testing.T) {
	f := newConnFixture(t)
	f.seedAgent(t, "mcp-agent", "mcp-agent-token-0123456789")
	tok := "mcp-agent-token-0123456789"

	r, code := f.mcp(t, tok, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`, nil)
	if code != 200 || r.Error != nil || !strings.Contains(string(r.Result), `"protocolVersion":"2025-03-26"`) ||
		!strings.Contains(string(r.Result), `"tools":{}`) || string(r.ID) != "1" {
		t.Fatalf("initialize: %d %+v %s", code, r, r.Result)
	}
	w := f.call(t, "POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`),
		map[string]string{"X-Muster-Token": tok})
	if w.Code != http.StatusAccepted || w.Body.Len() != 0 {
		t.Fatalf("notification: %d %q, want 202 with no body", w.Code, w.Body)
	}

	r, _ = f.mcp(t, tok, `{"jsonrpc":"2.0","id":"x","method":"tools/list"}`, nil)
	var list struct {
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(r.Result, &list)
	var names []string
	for _, tl := range list.Tools {
		names = append(names, tl.Name)
		if tl.InputSchema["type"] != "object" {
			t.Errorf("%s has no object input schema", tl.Name)
		}
	}
	if strings.Join(names, ",") != "connectors_list,connector_request,connector_status" {
		t.Fatalf("tools = %v", names)
	}

	r, _ = f.mcp(t, tok, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"connector_request","arguments":{"name":"github","reason":"open a PR"}}}`, nil)
	text, isErr := toolText(t, r)
	if isErr || !strings.Contains(text, `"grant":"pending"`) {
		t.Fatalf("connector_request: %s", text)
	}
	pending, _ := f.store.ListPendingGrants(f.ctx)
	if len(pending) != 1 || pending[0].AgentName != "mcp-agent" || pending[0].Reason != "open a PR" {
		t.Fatalf("the request was not filed as the calling agent: %+v", pending)
	}

	r, _ = f.mcp(t, tok, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"connector_status","arguments":{"name":"github"}}}`, nil)
	if text, _ := toolText(t, r); !strings.Contains(text, `"grant":"pending"`) {
		t.Fatalf("connector_status: %s", text)
	}
	r, _ = f.mcp(t, tok, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"connectors_list"}}`, nil)
	if text, _ := toolText(t, r); !strings.Contains(text, `"name":"github"`) {
		t.Fatalf("connectors_list: %s", text)
	}

	r, _ = f.mcp(t, tok, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"connector_status","arguments":{"name":"Bad Name"}}}`, nil)
	if text, isErr := toolText(t, r); !isErr || !strings.Contains(text, "connector name") {
		t.Fatalf("a bad name is not a tool error: %s", text)
	}
	r, _ = f.mcp(t, tok, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"nope"}}`, nil)
	if r.Error == nil || r.Error.Code != -32602 {
		t.Fatalf("unknown tool: %+v", r)
	}
	r, _ = f.mcp(t, tok, `{"jsonrpc":"2.0","id":7,"method":"resources/list"}`, nil)
	if r.Error == nil || r.Error.Code != -32601 {
		t.Fatalf("unknown method: %+v", r)
	}
}

func TestTheMCPServerRefusesABrowserOrigin(t *testing.T) {
	f := newConnFixture(t)
	f.seedAgent(t, "mcp-origin-agent", "mcp-origin-token-0123456789")
	if _, code := f.mcp(t, "mcp-origin-token-0123456789", `{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		map[string]string{"Origin": "http://evil.example.com"}); code != http.StatusForbidden {
		t.Fatalf("a request with an Origin: %d, want 403", code)
	}
	if r, code := f.mcp(t, "mcp-origin-token-0123456789", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, nil); code != 200 || r.Error != nil {
		t.Fatalf("control: ping without an Origin: %d %+v", code, r)
	}
}
