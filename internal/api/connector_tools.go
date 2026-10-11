package api

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/connectors"
)

// The connector store's agent tools. They are entries in the ONE agent tool
// registry (agentTools, agent.go) with the MCP flag set, so the gateway kind gets
// them on its native tool path and claude-code agents get the same definitions
// over POST /mcp (mcp.go). They execute in dispatchAgentTool like every other
// tool.
//
// 🔴 THE TOOLS NEVER RETURN A SECRET. They are thin wrappers over the agent JSON
// API (AgentConnectorList / AgentRequestConnector / AgentConnectorStatus), whose
// view type has no field for one; using a connector is the proxy's job, and the
// tool output says how.

// connectorToolDefs declares the three connector tools, in registry order.
func connectorToolDefs() []agents.ToolDef {
	fn := func(name, desc string, params map[string]any) agents.ToolDef {
		return agents.ToolDef{Type: "function", Name: name, Description: desc, Parameters: params}
	}
	return []agents.ToolDef{
		fn("connectors_list", "List the platform connectors muster holds (e.g. github) and this agent's access to each "+
			"(none, pending, approved, denied, revoked). Credentials are never shown: an approved connector is used "+
			"through muster's proxy, as each entry's `usage` explains.", objSchema(map[string]any{})),
		fn("connector_request", "Ask the operator for access to a connector by name (lowercase letters, digits, '-'). "+
			"If it does not exist yet, the operator is prompted to enter the credential. Returns the request's status; "+
			"poll connector_status until it is approved or denied. A denial is final for that request.",
			objSchema(map[string]any{
				"name":   strProp("The connector name, e.g. github."),
				"reason": strProp("Why access is needed — shown to the operator."),
			}, "name")),
		fn("connector_status", "This agent's access to one connector, and how to use it once approved. With "+
			"wait_seconds (max 50) it waits for a pending request to be decided.",
			objSchema(map[string]any{
				"name": strProp("The connector name."),
				"wait_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": 50,
					"description": "Wait this long for a pending request to be decided."},
			}, "name")),
	}
}

// connectorToolResult encodes a connector tool's outcome. A validation refusal
// or the per-agent pending cap is shown to the agent verbatim (it says what to
// do next); anything else is logged and summarised, so a
// database error never reaches a model.
func (s *Server) connectorToolResult(tool string, a agents.Agent, out any, err error) string {
	if err == nil {
		return toolJSON(out)
	}
	if errors.Is(err, connectors.ErrInvalid) || errors.Is(err, connectors.ErrTooManyPending) {
		return toolErr("%s", err.Error())
	}
	s.logger.Printf("connectors: tool %s for agent %d: %v", tool, a.ID, err)
	return toolErr("the tool failed")
}

func (s *Server) toolConnectorsList(ctx context.Context, a agents.Agent) string {
	if s.ext.Connectors == nil {
		return toolErr("%s", ConnectorsUnconfigured)
	}
	out, err := s.AgentConnectorList(ctx, a)
	return s.connectorToolResult("connectors_list", a, out, err)
}

func (s *Server) toolConnectorRequest(ctx context.Context, a agents.Agent, name, reason string) string {
	if s.ext.Connectors == nil {
		return toolErr("%s", ConnectorsUnconfigured)
	}
	out, _, err := s.AgentRequestConnector(ctx, a, strings.TrimSpace(name), reason)
	return s.connectorToolResult("connector_request", a, out, err)
}

func (s *Server) toolConnectorStatus(ctx context.Context, a agents.Agent, name string, waitSeconds int) string {
	if s.ext.Connectors == nil {
		return toolErr("%s", ConnectorsUnconfigured)
	}
	name = strings.TrimSpace(name)
	if err := connectors.ValidateName(name); err != nil {
		return s.connectorToolResult("connector_status", a, nil, err)
	}
	if waitSeconds < 0 {
		waitSeconds = 0
	}
	out, err := s.AgentConnectorStatus(ctx, a, name, time.Duration(waitSeconds)*time.Second)
	return s.connectorToolResult("connector_status", a, out, err)
}
