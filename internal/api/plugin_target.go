package api

import (
	"context"
	"errors"
	"html"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/textplain"
)

// ---------------------------------------------------------------------------
// WHERE A PLUGIN'S MESSAGES GO
//
// 🔴 THERE IS NO PRIVILEGED AGENT, AND THIS FILE IS WHAT REPLACED ONE. muster
// used to resolve a plugin's recipient by LABEL: the owner's lowest-id agent
// whose DISPLAY NAME was "agent". That made a mutable label load-bearing —
// renaming an agent moved every plugin's inbox, two agents could carry the
// label, and the agent holding it was delete-protected so the operator could
// not remove it by the ordinary route. The recipient is now a CHOICE, stored
// per plugin in plugins.target_agent_id (migration 0050) and changed only by
// POST /plugins/{id}/target.
//
// 🔴 "AGENT" STILL NAMES SOMETHING ELSE IN THIS PACKAGE AND IT IS UNRELATED.
// internal/api/agent.go owns the AGENT TOKEN — an auth tier for the
// agent-identified terminal-write door (MUSTER_CHIEF_TOKEN). That is a
// credential, not an agent, and nothing here touches it. A session that
// conflates them will go looking for a delivery target in an auth check.
//
// ⚠ NO TARGET IS A CONFIGURATION STATE, NOT A FAILURE, which is why it has its
// own error. A plugin with no target accepts messages and leaves them QUEUED
// (the deliverer's `no_target`), exactly as a deployment with no deliverer
// wired does, and the retry seam sends them once a target is picked. Rendering
// it as a broken page, or dropping the message, would both be wrong.
// ---------------------------------------------------------------------------

// ErrNoPluginTarget reports that a plugin names no agent to deliver to. It is a
// CONFIGURATION state: the caller renders or logs it as "pick a target", never
// as an error, and the message stays queued.
var ErrNoPluginTarget = errors.New("the plugin names no target agent")

// pluginTargetAgent resolves the agent p's messages are delivered to.
//
// 🔴 IT READS ON THE CALLER'S OWN CONTEXT, so row-level security answers with
// the principal's agents only. That — not a rank check — is what stops one
// user's plugin pointing at another user's agent: a target_agent_id naming a
// row the principal cannot see reads as not found, and the deliverer refuses a
// cross-owner agent again anyway (OutcomePending "not_owners_agent").
//
// ⚠ IT DOES NOT CHECK WHETHER THE POD IS RUNNING, deliberately. Liveness is
// decided inside Deliverer.Deliver against a FRESH read, because minutes can
// pass between this resolve and the turn; answering it here as well would be a
// second, staler answer to the same question.
func (s *Server) pluginTargetAgent(ctx context.Context, p plugins.Plugin) (agents.Agent, error) {
	if s.ext.Agents == nil {
		return agents.Agent{}, ErrNoPluginTarget
	}
	if p.TargetAgentID == nil || *p.TargetAgentID <= 0 {
		return agents.Agent{}, ErrNoPluginTarget
	}
	a, err := s.ext.Agents.Get(ctx, *p.TargetAgentID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The chosen agent is gone (deleted, or not this principal's).
			// That is the same state as never having picked one.
			return agents.Agent{}, ErrNoPluginTarget
		}
		return agents.Agent{}, err
	}
	return a, nil
}

// pluginTargetByID resolves the target for a plugin id, reading the plugin
// first. Callers that already hold the plugin row use pluginTargetAgent.
func (s *Server) pluginTargetByID(ctx context.Context, pluginID int64) (agents.Agent, error) {
	if s.ext.Plugins == nil {
		return agents.Agent{}, ErrNoPluginTarget
	}
	p, err := s.ext.Plugins.Plugin(ctx, pluginID)
	if err != nil {
		return agents.Agent{}, err
	}
	return s.pluginTargetAgent(ctx, p)
}

// ---------------------------------------------------------------------------
// PICKING THE TARGET
// ---------------------------------------------------------------------------

// handlePluginTarget sets (or clears) which agent a plugin's messages go to.
//
// 🔴 THE AGENT IS VALIDATED BY READING IT AS THE CALLER, not by trusting the
// posted id. The read goes through the request's own context, so row-level
// security answers only with the principal's agents: another owner's agent id
// reads as not found and is refused here, rather than being written and then
// refused on every delivery.
//
// ⚠ AN EMPTY OR ZERO agent_id CLEARS THE TARGET, which is a supported state and
// not an error: the plugin keeps accepting messages and keeps them queued.
func (s *Server) handlePluginTarget(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	raw := strings.TrimSpace(r.PostFormValue("agent_id"))
	var agentID int64
	if raw != "" && raw != "0" {
		agentID, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || agentID < 0 {
			s.renderPluginsPage(w, r, http.StatusBadRequest, "", "That is not an agent.")
			return
		}
	}
	if agentID > 0 {
		if s.ext.Agents == nil {
			s.renderPluginsPage(w, r, http.StatusConflict, "", "This deployment provisions no agents.")
			return
		}
		switch _, gerr := s.ext.Agents.Get(r.Context(), agentID); {
		case errors.Is(gerr, pgx.ErrNoRows):
			s.renderPluginsPage(w, r, http.StatusNotFound, "", "No such agent.")
			return
		case gerr != nil:
			s.logger.Printf("plugins: target %d: read agent %d: %v", id, agentID, gerr)
			s.renderPluginsPage(w, r, http.StatusInternalServerError, "", "The agent could not be looked up.")
			return
		}
	}
	switch err := s.ext.Plugins.SetTargetAgent(r.Context(), id, agentID); {
	case err == nil:
	case errors.Is(err, plugins.ErrNotFound):
		s.renderPluginsPage(w, r, http.StatusNotFound, "", "No such plugin.")
		return
	default:
		s.logger.Printf("plugins: target %d: %v", id, err)
		s.renderPluginsPage(w, r, http.StatusInternalServerError, "", "The target could not be saved.")
		return
	}
	if agentID > 0 {
		s.logger.Printf("plugins: plugin %d will deliver to agent %d", id, agentID)
	} else {
		s.logger.Printf("plugins: plugin %d has no target agent; its messages stay queued", id)
	}
	// Queued messages become deliverable the moment a target exists, so the
	// owner's backlog is attempted now rather than on the next inbound message.
	if agentID > 0 {
		s.startQueuedPluginDeliveries(r.Context(), id)
	}
	s.renderPluginsPage(w, r, http.StatusOK, "", "")
}

// startQueuedPluginDeliveries attempts plugin pluginID's queued messages in the
// background, after a target is picked. Best-effort: anything still queued is
// picked up by the ordinary retry seam.
func (s *Server) startQueuedPluginDeliveries(ctx context.Context, pluginID int64) {
	if s.pluginDeliverer == nil {
		return
	}
	msgs, err := s.ext.Plugins.Queued(ctx, pluginID, pluginRetryBatch)
	if err != nil {
		s.logger.Printf("plugins: plugin %d: list queued messages after a target was picked: %v", pluginID, err)
		return
	}
	for _, m := range msgs {
		s.startPluginDelivery(ctx, pluginID, m.ID)
	}
}

// pluginTargetFormHTML is one plugin row's target picker: a select of the
// owner's agents plus Save. unreadable draws it disabled, so a failed agent
// read is not shown as "you have no agents".
func pluginTargetFormHTML(p plugins.Plugin, mine []agents.Agent, unreadable bool) string {
	esc := html.EscapeString
	id := strconv.FormatInt(p.ID, 10)
	var cur int64
	if p.TargetAgentID != nil {
		cur = *p.TargetAgentID
	}
	var b strings.Builder
	b.WriteString(`<form method="POST" action="` + pluginsPath + `/` + id + `/target" class="flex flex-wrap items-center gap-2" data-plugin-target="` + id + `">`)
	b.WriteString(`<label for="plugin-target-` + id + `" class="text-xs text-fg2">Delivers to</label>`)
	if unreadable {
		b.WriteString(`<span class="text-xs text-fg2" data-plugin-target-unreadable>your agents could not be read</span></form>`)
		return b.String()
	}
	b.WriteString(`<select id="plugin-target-` + id + `" name="agent_id" class="min-h-[44px] rounded-md bg-bg px-2 py-1 text-sm text-fg ring-1 ring-inset ring-edge">`)
	b.WriteString(`<option value="0"` + selectedAttr(cur == 0) + `>nobody — keep messages queued</option>`)
	for _, a := range mine {
		aid := strconv.FormatInt(a.ID, 10)
		// 🔴 A DISPLAY NAME IS THE OPERATOR'S OWN TEXT AND ROUTINELY CARRIES
		// MARKDOWN, so it is shown the way every other display of one is: through
		// textplain.Title, which renders what a reader would see rather than the
		// source. Escaping alone would print the backticks and asterisks.
		// (internal/textplain's TestTheTitleSinkLedger pins this.)
		label := textplain.Title(a.DisplayName)
		if strings.TrimSpace(label) == "" {
			label = a.Name
		}
		b.WriteString(`<option value="` + aid + `"` + selectedAttr(cur == a.ID) + `>` + esc(label) + ` (` + esc(a.Name) + `)</option>`)
	}
	b.WriteString(`</select><button type="submit" class="` + flowsQuietButton + `">Save</button>`)
	if cur == 0 {
		b.WriteString(`<span class="text-xs text-fg2" data-plugin-no-target>no target: messages stay queued</span>`)
	}
	b.WriteString(`</form>`)
	return b.String()
}

func selectedAttr(on bool) string {
	if on {
		return " selected"
	}
	return ""
}
