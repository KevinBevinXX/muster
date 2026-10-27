package api

import (
	"context"

	"strconv"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
)

// ---------------------------------------------------------------------------
// WHAT THE AGENT CAN DO WITH A TASK, NOW.
//
// When the agent puts a task in front of its owner (task_route_attention), the
// ask offers the owner — as its buttons — exactly the actions the agent can take
// on that task at that moment, so the owner never answers with an action the
// agent cannot carry out (an owner once answered "dispatch it to an agent" to a
// agent that had no dispatch tool).
//
// 🔴 ONE PREDICATE PER ACTION, AND THE TOOL ITSELF CALLS IT. The offer is not a
// second reading of the gates: task_forward finds its target with forwardTarget
// and task_dispatch refuses through taskDispatchDeploymentRefusal and
// taskDispatchTaskRefusal — the same functions
// taskActions calls — so the offer and the gate cannot disagree.
//
//   - task_forward: a running agent other than the agent works the task
//     (forwardTarget; the tool refuses otherwise).
//   - task_dispatch: the tool would start an agent on the task — the opt-in and
//     the deployment (taskDispatchDeploymentRefusal) and the task itself: not
//     gated, open, no live agent on it (taskDispatchTaskRefusal) — and the
//     agent's budget not Full (plugins.DispatchBudget, the predicates the
//     reservation refuses on). The tool's argument checks (kind, repo,
//     instructions) are the agent's to satisfy.
//   - route_to_session is NEVER offered for a task: it routes a plugin message
//     to a session for a REPLY, and a task event is no_reply, so the tool refuses
//     it (RefusedNoReply). Offering it would be exactly the action the agent
//     cannot take.
//
// ⚠ "NOW" IS WHEN THE ASK IS FILED. The buttons are the ask's options, stored
// with it; an agent that starts or stops working the task afterwards does not
// change them. The tool still re-checks at the call.
// ---------------------------------------------------------------------------

// taskAction is one action offered on a task's ask: the tool, and the
// button text the owner taps (which is also the answer the agent receives).
type taskAction struct {
	Tool  string
	Label string
}

// forwardTarget is the agent task_forward would pass task taskID's change to:
// the newest running agent on it that is not the agent itself, or nil.
func (s *Server) forwardTarget(ctx context.Context, agent agents.Agent, taskID int64) (*agents.Agent, error) {
	on, err := s.agentsOnTask(ctx, agentOwnerID(agent), taskID)
	if err != nil {
		return nil, err
	}
	for i := range on { // newest first
		if on[i].ID != agent.ID && agentCanBeSent(on[i].Status) {
			return &on[i], nil
		}
	}
	return nil, nil
}

// taskMayDispatch reports whether an agent may start an agent with
// task_dispatch: the tool is on and owner opted in.
func taskMayDispatch(ext Extensions, owner int64) bool {
	return taskDispatchOn(ext) && owner > 0 && ext.TaskRouting.DispatchOwners[owner]
}

// taskDispatchDeploymentRefusal is task_dispatch's refusal for owner that no
// task or argument changes, "" when there is none: the opt-in
// (chiefMayDispatch), then the deployment's ability to start an agent at all.
// toolTaskDispatch answers with it first; taskActions offers dispatch only
// when it is "".
func (s *Server) taskDispatchDeploymentRefusal(owner int64) string {
	if !taskMayDispatch(s.ext, owner) {
		return taskDispatchOptOut
	}
	if s.ext.Provisioner == nil {
		return "this deployment cannot start agents"
	}
	if reason := s.ext.Provisioner.KickoffUndeliverableReason(); reason != "" {
		return "this deployment cannot start an agent now: " + reason
	}
	return ""
}

// taskDispatchTaskRefusal is task_dispatch's refusal for task n itself, ""
// when there is none: gated, not open, or a live agent (pending, provisioning,
// running) already on it. toolTaskDispatch runs it under its reservation lock;
// taskActions offers dispatch only when it is "". The tool's ARGUMENT
// checks (kind, repo, instructions) come on top and are not part of the offer:
// the agent picks those.
func (s *Server) taskDispatchTaskRefusal(ctx context.Context, owner int64, n notes.Note) (string, error) {
	r, err := s.taskDispatchTaskBlock(ctx, owner, n)
	return r.refusal, err
}

// taskBlock is why task_dispatch refuses a task itself: the agent's refusal
// sentence, and the same reason in the OWNER's words for the ask
// (taskActionsLine). Both zero when the task allows a start.
type taskBlock struct {
	refusal string
	owner   string
}

// taskDispatchTaskBlock is THE task predicate behind taskDispatchTaskRefusal
// (the tool's refusal) and taskActions (the ask's reason): one check,
// two wordings.
func (s *Server) taskDispatchTaskBlock(ctx context.Context, owner int64, n notes.Note) (taskBlock, error) {
	id := strconv.FormatInt(n.ID, 10)
	if reason, gated := notes.GateReason(n.Tags); gated {
		return taskBlock{"task " + id + " (" + reason + ") " + taskDispatchGatedText,
			"this task is gated (" + reason + "), so only you can release it"}, nil
	}
	if n.Status != notes.StatusOpen {
		return taskBlock{"task " + id + " " + taskDispatchNotOpen + " (" + n.Status + ")",
			"this task is not open (" + n.Status + "), so it cannot start one on it"}, nil
	}
	on, err := s.agentsOnTask(ctx, owner, n.ID)
	if err != nil {
		return taskBlock{}, err
	}
	for _, x := range on {
		if agentLive(x.Status) {
			return taskBlock{"task " + id + " " + taskDispatchHasAgent,
				"an agent on this task is still starting, so it cannot start another"}, nil
		}
	}
	return taskBlock{}, nil
}

// taskActions is the actions agent can take on task n now, in a fixed
// order (forward, then dispatch), and — when the owner opted in to dispatch —
// the agent's dispatch budget (nil otherwise, or when it could not be read).
//
// task_dispatch is offered only when the budget is NOT Full: the reservation
// refuses on the same predicates (plugins.DispatchBudget.LiveFull / DailyFull),
// so a full budget is an action the agent cannot take. A budget that could not
// be read withholds the offer (the ask is still filed; the tool re-checks).
//
// withheld is why task_dispatch was NOT offered, in the owner's words, for
// the ask (taskActionsLine); "" exactly when it was offered. Each reason
// is named only when it is the one that applies, checked in the tool's own
// order: the opt-in (only then "not enabled for you"), the deployment, the
// task itself, then the budget (could not be read, or full).
func (s *Server) taskActions(ctx context.Context, agent agents.Agent, n notes.Note) (acts []taskAction, budget *plugins.DispatchBudget, withheld string, err error) {
	var out []taskAction
	t, err := s.forwardTarget(ctx, agent, n.ID)
	if err != nil {
		return nil, nil, "", err
	}
	if t != nil {
		out = append(out, taskAction{Tool: taskForwardTool,
			Label: "task_forward: pass it to " + mirror.OneLine(firstNonBlank(t.DisplayName, t.Name), 80) + ", already working it"})
	}
	owner := agentOwnerID(agent)
	if taskMayDispatch(s.ext, owner) {
		budget = s.dispatchBudgetNow(ctx)
	}
	switch {
	case !taskMayDispatch(s.ext, owner):
		withheld = withheldNotEnabled
	case s.taskDispatchDeploymentRefusal(owner) != "":
		withheld = withheldDeployment
	default:
		blk, err := s.taskDispatchTaskBlock(ctx, owner, n)
		if err != nil {
			return nil, nil, "", err
		}
		switch {
		case blk.refusal != "": // the task itself refuses: not the budget's doing
			withheld = blk.owner
		case budget == nil:
			withheld = withheldBudgetUnknown
		case budget.Full():
			withheld = withheldBudgetFull
		default:
			out = append(out, taskAction{Tool: taskDispatchTool, Label: "task_dispatch: start a new agent on it"})
		}
	}
	return out, budget, withheld, nil
}

// Why taskActions withheld task_dispatch, in the owner's words (the
// task's own reasons are taskDispatchTaskBlock's).
const (
	withheldNotEnabled    = "starting agents is not enabled for you"
	withheldDeployment    = "this deployment cannot start an agent now"
	withheldBudgetUnknown = "its allowance for starting agents could not be read right now"
	withheldBudgetFull    = "it has used up its allowance for starting agents for now"
)

// taskActionsLine is the ask's line saying what the agent can do — muster's
// words, after the agent's note. withheld is taskActions's reason
// task_dispatch was not offered.
func taskActionsLine(acts []taskAction, withheld string) string {
	if len(acts) == 0 {
		return "The target agent cannot forward or dispatch this task now (no agent is working it, and " + withheld + "): it is yours to handle."
	}
	return "The target agent can act on your answer: pick one of the actions below, or write your own answer."
}
