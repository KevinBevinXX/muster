package api

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
)

// ---------------------------------------------------------------------------
// agent_destroy: THE OPERATOR TEARS DOWN ONE OF THEIR AGENTS FROM THEIR OWN
// MIRRORED CLAUDE CODE SESSION.
//
// Operator decision ("Build agent_destroy MCP": "Mirrored owner sessions only,
// same pattern; tears down the pod via the existing lifecycle path, keeps the
// task"), on attention_answer's pattern:
//
//   - LISTED ONLY on the mirror door (mirror_mcp.go's mirrorTools), never in the
//     agent registry, so POST /mcp (a dispatched agent, the chief included) does
//     not offer it and refuses it by name as unknown;
//   - 🔴 REFUSED IN ITS HANDLER to every caller that is not a mirrored session
//     (toolCaller.mirroredSession): dispatchCallerTool is reached by a gateway
//     agent naming ANY tool (dispatchAgentTool's default case), the chief
//     included.
//
// It takes the browser's own teardown (destroyAgent, which handleAgentDelete
// calls too): the chief is refused there, then the provisioner's Destroy runs
// in the background — the instance, then the row — and, after a successful
// teardown and on BOTH doors, the chief's start on the task is released when
// the chief's start made this agent (releaseChiefStart) and the owner's open
// tabs are told (agent.changed, attention_queue.changed). What the row's
// deletion takes with it is the browser path's: the agent's open attention asks
// are deleted (0025's ON DELETE CASCADE) and a turn in flight is not stopped
// first — the pod's teardown ends it.
//
// ⚠ NO AUDIT ROW. The browser's Delete writes none, and an MCP destroy writes
// none either (operator decision): its provenance — the host the mirror token
// proves and the session the caller named — is in muster's server log only.
//
// The one thing only this door does: the TASK IS KEPT and, once the teardown
// has succeeded, moved back from in_progress to open when no other live agent
// is on it (reopenTaskAfterDestroy) — the state task_dispatch needs
// (chiefDispatchTaskRefusal: open, ungated, no live agent).
// ---------------------------------------------------------------------------

const agentDestroyTool = "agent_destroy"

// agentDestroyMirrorOnly is agent_destroy's refusal of a caller that is not a
// mirrored session.
const agentDestroyMirrorOnly = "agent_destroy is available only to your owner's own mirrored Claude Code sessions"

// agentDestroyToolDef is the mirrored sessions' agent_destroy.
func agentDestroyToolDef() agents.ToolDef {
	return agents.ToolDef{Type: "function", Name: agentDestroyTool,
		Description: "Destroy one of your owner's agents ON YOUR OWNER'S BEHALF — use it only when your owner asked you to. It is the same " +
			"teardown as Delete on muster's Agents page: the agent's pod and record are removed in the background, its open attention asks " +
			"go with it, and a turn it is running is cut off. The task it worked is KEPT: once the agent is gone, a task still in_progress " +
			"with no other live agent goes back to open, and if your owner's chief started this agent, the chief may start one new agent " +
			"on that task again. Name the agent by EXACTLY ONE of: name (its slug, as `muster agent ls` shows it) or id. The chief cannot " +
			"be destroyed this way.",
		Parameters: objSchema(map[string]any{
			"name": strProp("The agent's name (slug), as `muster agent ls` shows it. Give this OR id."),
			"id":   intProp("The agent's id. Give this OR name.", 1, 1<<53),
		})}
}

// toolAgentDestroy is agent_destroy. Another owner's agent and a missing one
// read the same ("no such agent").
func (s *Server) toolAgentDestroy(ctx context.Context, c toolCaller, arguments string) string {
	if !c.mirroredSession() {
		return toolErr("%s", agentDestroyMirrorOnly)
	}
	var in struct {
		Name *string `json:"name"`
		ID   *int64  `json:"id"`
	}
	if err := json.Unmarshal([]byte(orEmptyJSON(arguments)), &in); err != nil {
		return toolErr("arguments are {name or id}")
	}
	if (in.Name == nil) == (in.ID == nil) {
		return toolErr("name the agent by exactly one of name (its slug) or id, not both and not neither")
	}
	// 🔴 THE PROVISIONER BARRIER FOR THE MCP DOORS: they are not lifecycle
	// routes (requireLifecycleProvisioner), so this body — which also reaches
	// the provisioner, through the destroy closure below — is the nil check
	// provisioner_seam_test.go's walk looks for.
	if s.ext.Provisioner == nil || s.ext.Agents == nil {
		return toolErr("this deployment has no agent provisioner wired, so it cannot destroy an agent")
	}
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return toolErr("the agent could not be read")
	}
	var (
		a     agents.Agent
		err   error
		label string
	)
	if in.ID != nil {
		label = strconv.FormatInt(*in.ID, 10)
		if *in.ID <= 0 {
			return toolErr("no such agent: %s", label)
		}
		a, err = s.ext.Agents.Get(ctx, *in.ID)
	} else {
		label = strings.TrimSpace(*in.Name)
		if label == "" {
			return toolErr("no such agent: %q", label)
		}
		a, err = s.ext.Agents.GetByName(ctx, label)
	}
	switch {
	// Row-level security already hides another owner's agent; the owner
	// comparison is the same rule stated again.
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && a.OwnerID != p.UserID):
		return toolErr("no such agent: %s", label)
	case err != nil:
		s.logger.Printf("agent_destroy: read %s: %v", label, err)
		return toolErr("the agent could not be read")
	}
	from := c.asker.MirrorHost + "/" + c.asker.MirrorSession
	var chief errDestroyChief
	destroy := func(ctx context.Context, id int64) error { return s.ext.Provisioner.Destroy(ctx, id) }
	_, err = s.destroyAgent(ctx, a.ID, destroy, destroyHooks{done: s.reopenTaskAfterDestroy})
	switch {
	case err == nil:
	case errors.As(err, &chief):
		return toolErr("%s; to replace the chief use `muster chief replace`", chief.Error())
	default:
		s.logger.Printf("agent_destroy: %d (%s) via MCP from %s: %v", a.ID, a.Name, from, err)
		return toolErr("nothing was destroyed: could not check whether this is the chief agent")
	}
	// The ONLY record of where an MCP destroy came from (no audit row).
	s.logger.Printf("agents: destroy of %d (%s) started by the operator via MCP from %s", a.ID, a.Name, from)
	out := map[string]any{"agent_id": a.ID, "agent": a.Name, "destroying": true}
	next := "The teardown runs in the background: the agent is gone from muster when its pod and record are. " +
		"If the teardown fails the agent stays, with status error."
	if a.NoteID != nil {
		out["task_id"] = *a.NoteID
		next += " Its task is kept; once the agent is gone, if the task is still in_progress and no other agent is live on it, it goes back to open."
	}
	out["next"] = next
	return toolJSON(out)
}

// releaseChiefStart is the shared after-step of an owner's destroy of agent a
// (both doors, destroyAgent): when the chief's latest start on a's task made
// a, that start is released, so the chief may start ONE new agent on the task
// (plugins.ReleaseChiefDispatch; operator decision "Owner's destroy resets the
// chief's allowance"). An agent with no task, a deployment with no
// chief-dispatch ledger, or an agent the chief did not start release nothing.
// Best-effort: a failure is logged and the task keeps its one start (the
// fail-closed direction).
func (s *Server) releaseChiefStart(ctx context.Context, a agents.Agent) {
	if a.ID == 0 || a.NoteID == nil || s.ext.ChiefDispatch == nil {
		return
	}
	released, err := s.ext.ChiefDispatch.ReleaseChiefDispatch(ctx, *a.NoteID, a.ID)
	if err != nil {
		s.logger.Printf("agents: release the chief's start on task %d after destroying %d: %v", *a.NoteID, a.ID, err)
		return
	}
	if released {
		s.logger.Printf("agents: destroying %d released the chief's start on task %d", a.ID, *a.NoteID)
	}
}

// reopenTaskAfterDestroy moves destroyed agent a's task back from in_progress
// to open when no other live agent is on it, so the chief's task_dispatch can
// start a new one (chiefDispatchTaskRefusal wants it open). A task in any other
// state (open already, ready_for_review, complete) is left as it is. Best-effort:
// a failure is logged and the task stays in_progress, as the browser path leaves
// it.
//
// 🔴 THE WRITE IS CONDITIONAL (notes.OnlyFrom): the UPDATE moves the row only
// while it is still in_progress, so a status change landing between the read
// below and the write is never overwritten — the write answers
// notes.ErrStatusMoved and the task keeps its new status.
func (s *Server) reopenTaskAfterDestroy(ctx context.Context, a agents.Agent) {
	if a.NoteID == nil || s.ext.Notes == nil {
		return
	}
	n, err := s.ext.Notes.Get(ctx, *a.NoteID)
	if err != nil || n.Status != notes.StatusInProgress {
		return
	}
	on, err := s.agentsOnTask(ctx, a.OwnerID, n.ID)
	if err != nil {
		s.logger.Printf("agent_destroy: agents on task %d: %v; leaving it in_progress", n.ID, err)
		return
	}
	for _, x := range on {
		if x.ID != a.ID && agentLive(x.Status) {
			return
		}
	}
	hooks := reopenTestHooks.Load()
	if hooks != nil && hooks.beforeWrite != nil {
		hooks.beforeWrite(n.ID)
	}
	_, err = s.setNoteStatus(notes.OnlyFrom(ctx, notes.StatusInProgress), writerDestroyReopen, n.ID, notes.StatusOpen)
	if hooks != nil && hooks.afterWrite != nil {
		hooks.afterWrite(n.ID, err)
	}
	if err != nil {
		s.logger.Printf("agent_destroy: reopen task %d: %v", n.ID, err)
		return
	}
	s.broadcast(ctx, EventTaskChanged, strconv.FormatInt(n.ID, 10))
}

// reopenHooks are test-only seams around reopenTaskAfterDestroy's write:
// beforeWrite runs between its read and its write (the window the conditional
// write closes), afterWrite with the write's result.
type reopenHooks struct {
	beforeWrite func(taskID int64)
	afterWrite  func(taskID int64, err error)
}

// reopenTestHooks holds the hooks (nil in production). 🔴 ATOMIC, because it
// is read by destroyAgent's background goroutine — possibly one a previous
// test started — while a test stores or clears it.
var reopenTestHooks atomic.Pointer[reopenHooks]
