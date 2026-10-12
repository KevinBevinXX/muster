package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/flows"
)

// THE AGENT DOOR TO BROWSER FLOWS (PR 4): two entries in the ONE agent tool
// registry (agentTools), with the MCP flag set so claude-code pods reach them on
// the connect listener's existing /mcp — no new route, no new prefix.
//
//   - browser_flow_request files a REQUEST, never a job. The agent's OWNER
//     approves or denies it on /flows (a browser session, requireOperatorWrite,
//     the connector-request approve pattern). There is no agent-side decide path
//     and no auto-approve: not the approval relay, not a grant-on-first-use.
//   - browser_flow_status reads one of the agent's own requests and, once
//     approved, its job's status and output.
//
// Operator decision 2026-10-12: agents may request ANY catalogued flow, read or
// write, each run approved by the owner. Agent-composed free text bound for a
// third party is additionally screened (flows.AgentTextRefusal — a heuristic,
// stated as one).

// flowRequestMaxWait caps browser_flow_status's wait, like connector_status.
const flowRequestMaxWait = 50 * time.Second

func flowToolDefs() []agents.ToolDef {
	fn := func(name, desc string, params map[string]any) agents.ToolDef {
		return agents.ToolDef{Type: "function", Name: name, Description: desc, Parameters: params}
	}
	return []agents.ToolDef{
		fn("browser_flow_request", "Ask your owner to run a named browser flow on one of their hosts' logged-in "+
			"browsers (catalogue: "+strings.Join(flows.Names(), ", ")+"). muse.poll reads the latest Muse reply "+
			"(params: thread, task); muse.send sends a message to Meta's Muse (params: text, thread, task). "+
			"Nothing runs until the owner approves THIS request in muster; poll browser_flow_status. "+
			"Text may not contain host names, IP addresses, credentials or infrastructure names.",
			objSchema(map[string]any{
				"flow":     strProp("The catalogue flow, e.g. muse.poll."),
				"host":     strProp("The owner's runner host, e.g. laptop."),
				"instance": strProp("The browser instance on that host, e.g. personal."),
				"params": map[string]any{"type": "object", "description": "The flow's parameters (strings).",
					"additionalProperties": map[string]any{"type": "string"}},
				"reason": strProp("Why — shown to the owner beside the request."),
			}, "flow", "host", "instance")),
		fn("browser_flow_status", "One of your browser-flow requests: pending, approved (with the job's status "+
			"and output once it ran), denied, or expired. With wait_seconds (max 50) it waits for a change.",
			objSchema(map[string]any{
				"request_id": map[string]any{"type": "integer", "description": "The id browser_flow_request returned."},
				"wait_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": 50,
					"description": "Wait this long for the request (or its job) to move."},
			}, "request_id")),
	}
}

// flowRequestView is what an agent sees: the request, and its job once there is
// one. No owner-side detail beyond that.
type flowRequestView struct {
	Request flows.FlowRequest `json:"request"`
	Job     *flows.Job        `json:"job,omitempty"`
	Next    string            `json:"next"`
}

func (s *Server) toolFlowRequest(ctx context.Context, a agents.Agent, arguments string) string {
	if s.ext.Flows == nil {
		return toolErr("%s", FlowsUnconfigured)
	}
	var in struct {
		Flow     string            `json:"flow"`
		Host     string            `json:"host"`
		Instance string            `json:"instance"`
		Params   map[string]string `json:"params"`
		Reason   string            `json:"reason"`
	}
	if err := json.Unmarshal([]byte(orEmptyJSON(arguments)), &in); err != nil {
		return toolErr("arguments are {flow, host, instance, params{…}, reason}")
	}
	q, err := s.ext.Flows.FileRequest(ctx, a.ID, agentOwnerID(a), a.Name, flows.Request{Flow: strings.TrimSpace(in.Flow),
		Host: strings.TrimSpace(in.Host), Instance: strings.TrimSpace(in.Instance), Params: in.Params}, in.Reason)
	switch {
	case err == nil:
	case errors.Is(err, flows.ErrInvalid), errors.Is(err, flows.ErrTooManyPendingRequests):
		return toolErr("%s", err.Error())
	default:
		s.logger.Printf("flows: agent %d request: %v", a.ID, err)
		return toolErr("the request could not be filed")
	}
	return toolJSON(flowRequestView{Request: q, Next: "Your owner must approve this in muster (Browser flows). Poll browser_flow_status{request_id} until it is approved and its job finishes."})
}

func (s *Server) toolFlowStatus(ctx context.Context, a agents.Agent, arguments string) string {
	if s.ext.Flows == nil {
		return toolErr("%s", FlowsUnconfigured)
	}
	var in struct {
		RequestID   int64 `json:"request_id"`
		WaitSeconds int   `json:"wait_seconds"`
	}
	if err := json.Unmarshal([]byte(orEmptyJSON(arguments)), &in); err != nil || in.RequestID <= 0 {
		return toolErr("arguments are {request_id, wait_seconds}")
	}
	wait := time.Duration(in.WaitSeconds) * time.Second
	if wait < 0 {
		wait = 0
	}
	if wait > flowRequestMaxWait {
		wait = flowRequestMaxWait
	}
	deadline := time.Now().Add(wait)
	for {
		v, settled, err := s.flowRequestView(ctx, a, in.RequestID)
		if errors.Is(err, flows.ErrNotFound) {
			return toolErr("no such request of yours")
		}
		if err != nil {
			s.logger.Printf("flows: agent %d status %d: %v", a.ID, in.RequestID, err)
			return toolErr("the status could not be read")
		}
		if settled || !time.Now().Before(deadline) {
			return toolJSON(v)
		}
		select {
		case <-ctx.Done():
			return toolJSON(v)
		case <-time.After(time.Second):
		}
	}
}

// flowRequestView reads one of agent a's requests and, when approved, its job.
// settled is true when nothing more will change (a terminal request or job).
func (s *Server) flowRequestView(ctx context.Context, a agents.Agent, id int64) (flowRequestView, bool, error) {
	q, err := s.ext.Flows.RequestForAgent(ctx, a.ID, id)
	if err != nil {
		return flowRequestView{}, false, err
	}
	v := flowRequestView{Request: q}
	switch q.Status {
	case flows.RequestPending:
		v.Next = "waiting for your owner's decision"
		return v, false, nil
	case flows.RequestDenied:
		v.Next = "denied by your owner; do not re-ask for the same thing"
		return v, true, nil
	case flows.RequestExpired:
		v.Next = "expired undecided; nothing ran"
		return v, true, nil
	}
	if q.JobID == nil {
		return v, true, nil
	}
	j, err := s.ext.Flows.Get(ctx, *q.JobID)
	if err != nil {
		return flowRequestView{}, false, err
	}
	v.Job = &j
	if j.Status.Terminal() || j.Status == flows.StatusNeedsOperator {
		v.Next = "finished: " + string(j.Status)
		return v, true, nil
	}
	v.Next = "approved; the job is " + string(j.Status)
	return v, false, nil
}
