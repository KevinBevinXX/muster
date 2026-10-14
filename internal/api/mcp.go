package api

import (
	"encoding/json"
	"io"
	"net/http"
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

// mcpGitNote tells a claude-code agent how git follows a grant.
const mcpGitNote = "For git, plain `git clone/fetch/push https://<host>/...` goes through the proxy on its own: " +
	"once a connector is approved, git is routed through muster for its hosts within " +
	"about 15 seconds (run `ccd connect-git <connector>` to apply it at once), and the routing is removed " +
	"the same way when access is revoked."

// isToolErr reports whether a dispatched tool's text is toolErr's encoding —
// the registry's one spelling of a failed tool call — so MCP can flag it isError.
func isToolErr(text string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &m) != nil || len(m) != 1 {
		return false
	}
	_, ok := m["error"]
	return ok
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

// handleMCP serves POST /mcp.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	// A browser page must not be able to drive an agent's tools (DNS-rebinding
	// defence the transport asks servers to apply). Agent clients send no Origin.
	if r.Header.Get("Origin") != "" {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin MCP requests are refused"})
		return
	}
	a, _ := agentFromContext(r.Context())
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
			"instructions":    mcpInstructions(),
		}, nil)
	case "ping":
		s.writeMCP(w, req.ID, map[string]any{}, nil)
	case "tools/list":
		defs := s.MCPToolDefsFor(r.Context(), a)
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
		// Only a tool this transport LISTS may be called through it: a gateway-only
		// tool's name is as unknown here as a made-up one.
		listed := false
		for _, d := range s.MCPToolDefsFor(r.Context(), a) {
			listed = listed || d.Name == p.Name
		}
		if !listed {
			s.writeMCP(w, req.ID, nil, &mcpError{Code: -32602, Message: "unknown tool " + p.Name})
			return
		}
		text := s.dispatchAgentTool(r.Context(), a, p.Name, string(p.Arguments))
		result := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
		if isToolErr(text) {
			result["isError"] = true
		}
		s.writeMCP(w, req.ID, result, nil)
	default:
		s.writeMCP(w, req.ID, nil, &mcpError{Code: -32601, Message: "method not found: " + req.Method})
	}
}
