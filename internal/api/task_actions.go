package api

import (
	"context"

	"strconv"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notes"
)

// ---------------------------------------------------------------------------
// WHAT THE CHIEF CAN DO WITH A TASK, NOW.
//
// When the chief puts a task in front of its owner (task_route_attention), the
// ask offers the owner — as its buttons — exactly the actions the chief can take
// on that task at that moment, so the owner never answers with an action the
// chief cannot carry out (an owner once answered "dispatch it to an agent" to a
// chief that had no dispatch tool).
//
// 🔴 ONE PREDICATE PER ACTION, AND THE TOOL ITSELF CALLS IT. The offer is not a
// second reading of the gates: task_forward finds its target with forwardTarget
// and task_dispatch refuses through chiefDispatchDeploymentRefusal and
// chiefDispatchTaskRefusal — the same functions
// chiefTaskActions calls — so the offer and the gate cannot disagree.
//
//   - task_forward: a running agent other than the chief works the task
//     (forwardTarget; the tool refuses otherwise).
//   - task_dispatch: the tool would start an agent on the task — the opt-in and
//     the deployment (chiefDispatchDeploymentRefusal) and the task itself: not
//     gated, open, no live agent on it (chiefDispatchTaskRefusal). The tool's
//     argument checks (kind, repo, instructions) are the chief's to satisfy.
//   - route_to_session is NEVER offered for a task: it routes a plugin message
//     to a session for a REPLY, and a task event is no_reply, so the tool refuses
//     it (RefusedNoReply). Offering it would be exactly the action the chief
//     cannot take.
//
// ⚠ "NOW" IS WHEN THE ASK IS FILED. The buttons are the ask's options, stored
// with it; an agent that starts or stops working the task afterwards does not
// change them. The tool still re-checks at the call.
// ---------------------------------------------------------------------------

// chiefTaskAction is one action offered on a task's ask: the tool, and the
// button text the owner taps (which is also the answer the chief receives).
type chiefTaskAction struct {
	Tool  string
	Label string
}

// forwardTarget is the agent task_forward would pass task taskID's change to:
// the newest running agent on it that is not the chief itself, or nil.
func (s *Server) forwardTarget(ctx context.Context, chief agents.Agent, taskID int64) (*agents.Agent, error) {
	on, err := s.agentsOnTask(ctx, agentOwnerID(chief), taskID)
	if err != nil {
		return nil, err
	}
	for i := range on { // newest first
		if on[i].ID != chief.ID && agentCanBeSent(on[i].Status) {
			return &on[i], nil
		}
	}
	return nil, nil
}

// chiefMayDispatch reports whether owner's chief may start an agent with
// task_dispatch: the tool is on and owner opted in.
func chiefMayDispatch(ext Extensions, owner int64) bool {
	return chiefDispatchOn(ext) && owner > 0 && ext.TaskRouting.DispatchOwners[owner]
}

// chiefDispatchDeploymentRefusal is task_dispatch's refusal for owner that no
// task or argument changes, "" when there is none: the opt-in
// (chiefMayDispatch), then the deployment's ability to start an agent at all.
// toolTaskDispatch answers with it first; chiefTaskActions offers dispatch only
// when it is "".
func (s *Server) chiefDispatchDeploymentRefusal(owner int64) string {
	if !chiefMayDispatch(s.ext, owner) {
		return chiefDispatchOptOut
	}
	if s.ext.Provisioner == nil {
		return "this deployment cannot start agents"
	}
	if reason := s.ext.Provisioner.KickoffUndeliverableReason(); reason != "" {
		return "this deployment cannot start an agent now: " + reason
	}
	return ""
}

// chiefDispatchTaskRefusal is task_dispatch's refusal for task n itself, ""
// when there is none: gated, not open, or a live agent (pending, provisioning,
// running) already on it. toolTaskDispatch runs it under its reservation lock;
// chiefTaskActions offers dispatch only when it is "". The tool's ARGUMENT
// checks (kind, repo, instructions) come on top and are not part of the offer:
// the chief picks those.
func (s *Server) chiefDispatchTaskRefusal(ctx context.Context, owner int64, n notes.Note) (string, error) {
	id := strconv.FormatInt(n.ID, 10)
	if reason, gated := notes.GateReason(n.Tags); gated {
		return "task " + id + " (" + reason + ") " + chiefDispatchGatedText, nil
	}
	if n.Status != notes.StatusOpen {
		return "task " + id + " " + chiefDispatchNotOpen + " (" + n.Status + ")", nil
	}
	on, err := s.agentsOnTask(ctx, owner, n.ID)
	if err != nil {
		return "", err
	}
	for _, x := range on {
		if agentLive(x.Status) {
			return "task " + id + " " + chiefDispatchHasAgent, nil
		}
	}
	return "", nil
}

// chiefTaskActions is the actions chief can take on task n now, in a fixed
// order (forward, then dispatch).
func (s *Server) chiefTaskActions(ctx context.Context, chief agents.Agent, n notes.Note) ([]chiefTaskAction, error) {
	var out []chiefTaskAction
	t, err := s.forwardTarget(ctx, chief, n.ID)
	if err != nil {
		return nil, err
	}
	if t != nil {
		out = append(out, chiefTaskAction{Tool: taskForwardTool,
			Label: "task_forward: pass it to " + mirror.OneLine(firstNonBlank(t.DisplayName, t.Name), 80) + ", already working it"})
	}
	if s.chiefDispatchDeploymentRefusal(agentOwnerID(chief)) == "" {
		why, err := s.chiefDispatchTaskRefusal(ctx, agentOwnerID(chief), n)
		if err != nil {
			return nil, err
		}
		if why == "" {
			out = append(out, chiefTaskAction{Tool: taskDispatchTool, Label: "task_dispatch: start a new agent on it"})
		}
	}
	return out, nil
}

// chiefTaskActionsLine is the ask's line saying what the chief can do — muster's
// words, after the chief's note.
func chiefTaskActionsLine(acts []chiefTaskAction) string {
	if len(acts) == 0 {
		return "Your chief cannot forward or dispatch this task now (no agent is working it, and starting agents is not enabled for you): it is yours to handle."
	}
	return "Your chief can act on your answer: pick one of the actions below, or write your own answer."
}
