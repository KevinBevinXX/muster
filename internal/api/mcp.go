package api

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// muster's MCP server: the connector store's tools for agents, over MCP's
// streamable-HTTP transport (one JSON-RPC message per POST, one JSON response).
//
// It is served at POST /mcp behind requireConnectorAgent — the same door as the
// proxy — so a tool call acts as the CALLING AGENT and the shared hook token is
// refused exactly as it is there. It speaks the subset of MCP a tool server
// needs: initialize, notifications/initialized, ping, tools/list, tools/call.
// There is no server-initiated stream (GET /mcp answers 405, which the
// transport defines as "this server offers none") and no session id: every
// call carries the agent's own token, so there is nothing to resume.
//
// 🔴 THIS FILE IS A TRANSPORT, NOT A TOOL SET. It declares no tool and executes
// none: tools/list is the agent tool registry's MCP subset ([Server.MCPToolDefsFor],
// filtered to what the calling agent may see)
// and tools/call runs the registry's one dispatcher (dispatchAgentTool). Which
// tools are on /mcp is a per-tool flag in agentTools (agent.go), whose comment
// says which tools carry it and why.

// mcpProtocolVersions are the protocol revisions this server answers. A client
// asking for one gets it back; any other gets the newest.
var mcpProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// mcpInstructions is the initialize result's instructions. The proxy recipe in
// it is ProxyRecipe's — the same sentence a connector's usage line carries — so
// the two cannot teach different URL shapes.
//
// The git sentence is true because ccd keeps git's routing in step with the
// grants by itself (cmd/ccd/connect.go, syncGitRouting): the MCP server is
// served only to claude-code agents, and ccd runs in every one of them.
func mcpInstructions() string {
	return "muster's connector store. To reach an external platform (e.g. GitHub), call connector_request " +
		"for it, wait for the operator's approval with connector_status, then call it through muster's proxy: " +
		ProxyRecipe("<connector>", "<host>") + " You never see the credential. " + mcpGitNote
}

// mcpInstructionsFor is the initialize instructions this server gives: the
// connector store's, and the attention queue's paragraph when its tools are
// offered (attention_tools.go's attentionInstruction).
func (s *Server) mcpInstructionsFor() string {
	if s.attentionToolsOn() {
		return mcpInstructions() + " " + attentionInstruction
	}
	return mcpInstructions()
}

// mcpGitNote tells a claude-code agent how git follows a grant.
const mcpGitNote = "For git, plain `git clone/fetch/push https://<host>/...` goes through the proxy on its own: " +
	"once a connector is approved, git is routed through muster for its hosts within " +
	"about 15 seconds (run `ccd connect-git <connector>` to apply it at once), and the routing is removed " +
	"the same way when access is revoked."

// isToolErr reports whether a dispatched tool's text is toolErr's encoding —
// the registry's one spelling of a failed tool call — so MCP can flag it isError.
func isToolErr(text string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &m) != nil {
		return false
	}
	if _, ok := m["error"]; !ok {
		return false
	}
	// toolErr's {error}, plus only the keys a structured refusal adds:
	// `refused` (session_routes.go's routeRefusal), `budget`
	// (task_dispatch's dispatchRefusal) and `candidates`
	// (plugin_message_route's subject refusals) — nothing else.
	for k := range m {
		switch k {
		case "error", "refused", "budget", "candidates":
		default:
			return false
		}
	}
	return true
}

func (s *Server) writeMCP(w http.ResponseWriter, id json.RawMessage, result any, rpcErr *mcpError) {
	body := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		body["error"] = rpcErr
	} else {
		body["result"] = result
	}
	if len(id) == 0 {
		body["id"] = nil
	}
	s.writeJSON(w, http.StatusOK, body)
}

// handleMCP serves POST /mcp: the calling agent's tools.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	a, _ := agentFromContext(r.Context())
	s.serveMCP(w, r, mcpSurface{
		tools:        func() []agents.ToolDef { return s.MCPToolDefsFor(r.Context(), a) },
		call:         func(name, args string) string { return s.dispatchAgentTool(r.Context(), a, name, args) },
		instructions: s.mcpInstructionsFor(),
	})
}

// mcpSurface is one MCP door's tool set: what tools/list answers, how a
// tools/call runs, and the initialize instructions. POST /mcp (an agent's
// token) and POST /mirror/mcp (a mirrored session, through its host's token)
// are two surfaces over the ONE protocol implementation below.
type mcpSurface struct {
	tools        func() []agents.ToolDef
	call         func(name, args string) string
	instructions string
}

// serveMCP speaks MCP's streamable-HTTP transport for one surface.
func (s *Server) serveMCP(w http.ResponseWriter, r *http.Request, surface mcpSurface) {
	// A browser page must not be able to drive an agent's tools (DNS-rebinding
	// defence the transport asks servers to apply). Agent clients send no Origin.
	if r.Header.Get("Origin") != "" {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin MCP requests are refused"})
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		s.writeMCP(w, nil, nil, &mcpError{Code: -32700, Message: "could not read the request"})
		return
	}
	var req mcpRequest
	if err := json.Unmarshal(raw, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
		s.writeMCP(w, nil, nil, &mcpError{Code: -32600, Message: "not a JSON-RPC 2.0 request (batches are not supported)"})
		return
	}
	// A notification (no id) gets 202 and no body.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := mcpProtocolVersions[0]
		for _, v := range mcpProtocolVersions {
			if v == p.ProtocolVersion {
				version = v
			}
		}
		s.writeMCP(w, req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "muster", "version": BuildVersion},
			"instructions":    surface.instructions,
		}, nil)
	case "ping":
		s.writeMCP(w, req.ID, map[string]any{}, nil)
	case "tools/list":
		defs := surface.tools()
		tools := make([]map[string]any, 0, len(defs))
		for _, d := range defs {
			tools = append(tools, map[string]any{"name": d.Name, "description": d.Description, "inputSchema": d.Parameters})
		}
		s.writeMCP(w, req.ID, map[string]any{"tools": tools}, nil)
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			s.writeMCP(w, req.ID, nil, &mcpError{Code: -32602, Message: "invalid tools/call params"})
			return
		}
		// Only a tool this surface LISTS may be called through it: a gateway-only
		// tool's name is as unknown here as a made-up one.
		// A retired name is looked up — and called — as the tool it became
		// (tool_names.go); the retired name itself is never listed.
		p.Name = canonicalToolName(p.Name)
		listed := false
		for _, d := range surface.tools() {
			listed = listed || d.Name == p.Name
		}
		if !listed {
			s.writeMCP(w, req.ID, nil, &mcpError{Code: -32602, Message: "unknown tool " + p.Name})
			return
		}
		s.writeMCPToolResult(w, req.ID, func() string { return surface.call(p.Name, string(p.Arguments)) })
	default:
		s.writeMCP(w, req.ID, nil, &mcpError{Code: -32601, Message: "method not found: " + req.Method})
	}
}

// mcpHeartbeatEvery is how often a tool call still running writes a keep-alive
// byte. A var so a test can shorten it.
var mcpHeartbeatEvery = 15 * time.Second

// toolResult is a dispatched tool's text as an MCP tools/call result.
func toolResult(text string) map[string]any {
	result := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
	if isToolErr(text) {
		result["isError"] = true
	}
	return result
}

// writeMCPToolResult runs one tool call and writes its JSON-RPC answer.
//
// 🔴 A LONG CALL KEEPS THE CONNECTION ALIVE WITH WHITESPACE. attention_wait
// blocks for up to ten minutes, longer than the idle timeout of any proxy
// between an agent and muster (and connector_status for fifty seconds). So once
// a call has run for mcpHeartbeatEvery, the response's status and
// Content-Type are sent and a newline follows every mcpHeartbeatEvery until the
// answer, which is then written after them. Leading whitespace is legal JSON
// (RFC 8259 §2), so the body still parses as the one JSON-RPC response the
// transport promises; a call that finishes sooner is written exactly as before.
func (s *Server) writeMCPToolResult(w http.ResponseWriter, id json.RawMessage, run func() string) {
	done := make(chan string, 1)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Printf("PANIC in MCP tool call: %v", rec)
				done <- toolErr("the tool failed")
			}
		}()
		done <- run()
	}()
	t := time.NewTicker(mcpHeartbeatEvery)
	defer t.Stop()
	rc := http.NewResponseController(w)
	started := false
	for {
		select {
		case text := <-done:
			if !started {
				s.writeMCP(w, id, toolResult(text), nil)
				return
			}
			if err := json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": toolResult(text)}); err != nil {
				s.logger.Printf("mcp: write a long call's answer: %v", err)
			}
			return
		case <-t.C:
			if !started {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				started = true
			}
			_, _ = w.Write([]byte("\n"))
			_ = rc.Flush()
		}
	}
}
