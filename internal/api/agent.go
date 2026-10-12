package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
)

// agentCtxKey carries the authenticated agent through the request context.
type agentCtxKey struct{}

// requireAgentToken authenticates the agent self-service API by the caller's
// per-agent hooks token (Bearer or X-Muster-Token). The token is unique per
// agent, so a valid bearer both authenticates and identifies the calling agent;
// the resolved agent is attached to the request context for the handler.
func (s *Server) requireAgentToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := hookTokenFromRequest(r)
		if token == "" {
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing agent token"})
			return
		}
		a, err := s.ext.Agents.GetByHooksToken(r.Context(), token)
		if err != nil {
			// Unknown token (or no rows) → unauthorized. Don't leak which.
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid agent token"})
			return
		}
		ctx := context.WithValue(r.Context(), agentCtxKey{}, a)
		s.admit(w, r.WithContext(ctx), next, principalFor(agentOwnerID(a), authz.ViaAgent))
	}
}

// agentFromContext returns the agent attached by requireAgentToken.
func agentFromContext(ctx context.Context) (agents.Agent, bool) {
	a, ok := ctx.Value(agentCtxKey{}).(agents.Agent)
	return a, ok
}

// agentTaskID returns the calling agent's bound task id, or writes a 404 and
// returns ok=false if the agent has no task assigned.
func (s *Server) agentTaskID(w http.ResponseWriter, a agents.Agent) (int64, bool) {
	if a.NoteID == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no task assigned to this agent"})
		return 0, false
	}
	return *a.NoteID, true
}

// handleAgentTask serves GET /agent/task: the calling agent's bound task with
// its current status and comment thread.
func (s *Server) handleAgentTask(w http.ResponseWriter, r *http.Request) {
	a, _ := agentFromContext(r.Context())
	id, ok := s.agentTaskID(w, a)
	if !ok {
		return
	}
	note, err := s.ext.Notes.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "task not found"})
			return
		}
		s.logger.Printf("agent: get task %d: %v", id, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not load task"})
		return
	}
	s.writeJSON(w, http.StatusOK, note)
}

type agentCommentRequest struct {
	Body string `json:"body"`
}

// handleAgentTaskComment serves POST /agent/task/comment: append a comment to the
// agent's task, authored by the agent.
func (s *Server) handleAgentTaskComment(w http.ResponseWriter, r *http.Request) {
	a, _ := agentFromContext(r.Context())
	id, ok := s.agentTaskID(w, a)
	if !ok {
		return
	}
	var body agentCommentRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	if strings.TrimSpace(body.Body) == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "comment body required"})
		return
	}
	c, err := s.ext.Notes.AddComment(r.Context(), notes.Comment{NoteID: id, Author: a.Name, Body: body.Body})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "task not found"})
			return
		}
		s.logger.Printf("agent: comment task %d: %v", id, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not add comment"})
		return
	}
	s.broadcast(EventTaskChanged, strconv.FormatInt(id, 10))
	s.writeJSON(w, http.StatusCreated, c)
}

type agentStatusRequest struct {
	Status string `json:"status"`
}

// handleAgentTaskStatus serves PATCH /agent/task/status: move the agent's task
// through the working states. Agents may NOT set `complete` — only the
// operator/human can declare a task done.
func (s *Server) handleAgentTaskStatus(w http.ResponseWriter, r *http.Request) {
	a, _ := agentFromContext(r.Context())
	id, ok := s.agentTaskID(w, a)
	if !ok {
		return
	}
	var body agentStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	if !notes.ValidStatus(body.Status) {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid status"})
		return
	}
	if !notes.StatusAllowedForAgent(body.Status) {
		s.writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "agents may not set this status; only the operator/human can mark a task complete",
		})
		return
	}
	note, err := s.setNoteStatus(r.Context(), writerAgentRoute, id, body.Status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "task not found"})
			return
		}
		s.logger.Printf("agent: set status task %d: %v", id, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not update status"})
		return
	}
	s.broadcast(EventTaskChanged, strconv.FormatInt(id, 10))
	// Background push when the agent marks its task ready_for_review (deduped).
	s.notifyTaskDone(note)
	s.writeJSON(w, http.StatusOK, note)
}

// --- worker self-service native tools (the /v1/responses mechanism) ---

// --- tool plumbing shared by every native-tool surface ---------------------
//
// 🔴 DELETION-TRACKING NOTE (upstream task #653, phase two). These five helpers
// LIVED IN internal/api/operator.go, which is deleted with the operator tier.
// They were never operator-specific — objSchema/strProp build a JSON-schema
// parameter block, toolJSON/toolErr encode a tool RESULT, and orEmptyJSON makes a
// no-argument tool call unmarshalable — and the worker tool set below has always
// used them. They moved rather than went because their only remaining consumer is
// this file.
//
// ⚠ THREE SIBLINGS DID NOT MOVE: intProp, strArray and policyRuleArraySchema had
// no consumer outside operatorToolDefs, so they went with it. Re-add one here if a
// worker tool ever needs an integer, a string array or an RBAC PolicyRule block —
// do not resurrect operator.go for it.

// objSchema builds a JSON-schema object node for a tool's parameters.
func objSchema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// strProp is a described string property.
func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// toolJSON encodes a tool result, degrading to an error object rather than
// returning an empty string the model would read as success.
func toolJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"error":"could not encode result"}`
	}
	return string(b)
}

// toolErr is the tool-result spelling of an error.
func toolErr(format string, args ...any) string {
	return toolJSON(map[string]string{"error": fmt.Sprintf(format, args...)})
}

// orEmptyJSON returns "{}" for empty/blank args so json.Unmarshal never fails on
// a no-argument tool call.
func orEmptyJSON(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}

// agentSystemPrompt is the worker's system prompt for the tool loop. The tools
// carry their own schemas; this frames when to use them.
const agentSystemPrompt = `You are a muster worker agent. You have native tools to read and update YOUR assigned task. Use them: call agent_get_task to read your task before starting; agent_set_task_status to set "in_progress" when you begin and "ready_for_review" when done (you CANNOT set "complete" — only the operator/human can); agent_comment_task to post progress notes. Then do the work.`

// agentTool is one entry of muster's ONE agent tool registry.
//
// 🔴 THERE IS ONE LIST AND ONE SWITCH. agentTools is the only place a tool is
// DECLARED; dispatchAgentTool is the only place one is EXECUTED. Both transports
// read them: the gateway kind gets [Server.AgentToolDefs] + [Server.AgentToolDispatch]
// on its /v1/responses turns, and POST /mcp (mcp.go) lists and calls the subset
// whose MCP flag is set. A tool added for one transport is therefore added for
// the other by setting a flag, never by copying a definition.
//
// 🔴 MCP IS AN EXPLICIT, PER-TOOL OPT-IN, AND ONLY THE CONNECTOR TOOLS HAVE IT.
// /mcp is served on the connect listener — the ONE muster port a claude-code
// pod's network policy lets it reach — and that listener's contract is "the
// connector surface and nothing else" (OnlyConnectSurface). The other tools stay
// gateway-only on purpose: the task/status/comment tools
// would hand a claude-code pod muster's task API through a door that was opened
// for connectors only, and
// agent_list_pull_requests spends muster's OWN server-side GitHub connection,
// which would let a pod read through a credential it was never granted under the
// per-agent connector model. TestTheMCPToolsAreTheRegistrysMCPSubset pins both
// sets.
type agentTool struct {
	def agents.ToolDef
	// mcp: also served over POST /mcp to claude-code agents. See above before
	// setting it.
	mcp bool
}

// agentTools is the registry. The connector tools are present only when the
// connector store is configured, so an agent is never offered a tool that can
// only answer "unconfigured".
func (s *Server) agentTools() []agentTool {
	fn := func(name, desc string, params map[string]any) agentTool {
		return agentTool{def: agents.ToolDef{Type: "function", Name: name, Description: desc, Parameters: params}}
	}
	tools := []agentTool{
		fn("agent_get_task", "Read your assigned task: its text, status, and comments.", objSchema(map[string]any{})),
		fn("agent_comment_task", "Post a progress comment on your task.", objSchema(map[string]any{
			"body": strProp("comment text"),
		}, "body")),
		fn("agent_set_task_status", "Set your task's status. Allowed: open, in_progress, ready_for_review (you cannot set complete).", objSchema(map[string]any{
			"status": map[string]any{"type": "string", "enum": []string{"open", "in_progress", "ready_for_review"}},
		}, "status")),
		fn("agent_list_pull_requests", "List the pull requests on YOUR repo (the one cloned into your workspace). muster calls the GitHub API with its stored token server-side — do NOT ask for a token. Returns each PR's number, title, state, author, head→base branches, draft flag, url and updated time.", objSchema(map[string]any{
			"state": map[string]any{"type": "string", "description": "which PRs to list", "enum": []string{"open", "closed", "all"}},
		})),
	}
	if s.ext.Connectors != nil {
		for _, d := range connectorToolDefs() {
			tools = append(tools, agentTool{def: d, mcp: true})
		}
	}
	return tools
}

// AgentToolDefs returns the native function tools given to gateway agents: every
// tool in the registry.
func (s *Server) AgentToolDefs() []agents.ToolDef {
	tools := s.agentTools()
	defs := make([]agents.ToolDef, 0, len(tools))
	for _, t := range tools {
		defs = append(defs, t.def)
	}
	return defs
}

// MCPToolDefs returns the registry's MCP subset, in registry order.
func (s *Server) MCPToolDefs() []agents.ToolDef {
	var defs []agents.ToolDef
	for _, t := range s.agentTools() {
		if t.mcp {
			defs = append(defs, t.def)
		}
	}
	return defs
}

// AgentInstructions returns the worker's system prompt (used by the kickoff loop
// AND every interactive chat turn). It grounds the agent in its ACTUAL
// environment — its repo and, when configured, the connector store — and tells it to describe only its real capabilities. The
// base image prompt otherwise advertises a generic assistant feature set (Signal/
// Telegram messaging, paired devices, image/video, calendar/email) this sandboxed
// worker does not have, so "what can you do" answers confabulate without this.
func (s *Server) AgentInstructions(a agents.Agent) string {
	b := agentSystemPrompt
	if a.Repo != "" {
		repo := a.Repo
		if i := strings.LastIndex(repo, "/"); i >= 0 && i+1 < len(repo) {
			b += "\n\nYour workspace is the GitHub repo " + a.Repo + ", cloned at /data/repos/" + repo[i+1:] + " — read/write/edit files there and run git + shell commands."
		}
		b += "\n\nTo see this repo's pull requests, call agent_list_pull_requests{state: open|closed|all} — do NOT ask for a GitHub token, it's handled server-side."
		// Close-out protocol: only when the agent is working a dispatched TASK
		// (NoteID) on a repo. Without this, agents complete the edit + tests and
		// STOP — the change is trapped in the ephemeral workspace and the task never
		// closes. This turns the loop into a reviewable artifact + a status report.
		//
		// 🔴 NO GITHUB CREDENTIAL IS IN THE POD. This text used to say "the git
		// credential helper + GITHUB_TOKEN are already configured"; agentspec's
		// buildSecrets emits no GITHUB_TOKEN (nor any GIT_CONFIG_* helper), so that
		// was false for every agent this binary provisions. Pushing goes through the
		// connector store's proxy when it is offered, and is otherwise reported as
		// impossible rather than retried against a token that is not there.
		if a.NoteID != nil {
			b += "\n\nCLOSE THE LOOP when the task's change is complete and verified — do NOT just stop after editing/testing:" +
				s.pushSteps(a.Repo) +
				"\n3. Call agent_comment_task with the PR URL, then agent_set_task_status{status:\"ready_for_review\"}." +
				"\nOnly finish once the change is pushed, a PR is open, and the task is marked ready_for_review. If you genuinely cannot (e.g. no change is needed), comment on the task explaining why and set it ready_for_review."
		}
	}
	if s.ext.Connectors != nil {
		b += "\n\nTo use an external platform's API (e.g. GitHub), ask for its connector with connector_request, wait with connector_status, then call it through muster's proxy: " +
			ProxyRecipe("<connector>", "<host>") + " You never see the credential."
	}
	b += "\n\nWhen asked what you can do, describe ONLY these real capabilities: your task tools (above), reading/writing files + git + shell in your workspace" +
		". Do NOT claim messaging (Signal/Telegram/WhatsApp/Discord), paired devices/cameras/location, image/video generation, web browsing, or calendar/email — you do not have those here."
	return b
}

// pushSteps is steps 1-2 of the close-out protocol: how to push and open a PR.
// With the connector store configured the agent goes through the `github`
// connector; without it muster provides no GitHub credential at all.
func (s *Server) pushSteps(repo string) string {
	if s.ext.Connectors == nil {
		return "\n1. Create a branch and commit your work, then `git push` it. muster provides no GitHub credential here — do NOT ask for a token; if the push is refused, name your branch in step 3 and explain." +
			"\n2. If the push succeeded, open a pull request against the repo's default branch if this environment lets you."
	}
	return "\n1. Create a branch and commit your work. You hold no GitHub token — do NOT ask for one. Get the `github` connector: connector_request{name:\"github\", reason:\"push <branch> to " + repo + "\"}, then connector_status{name:\"github\", wait_seconds:50} until it is approved. Push through muster's proxy: " +
		ProxyRecipe("github", "github.com") + " For git: `" + GitPushRecipe("github", "github.com/"+repo+".git") + " <branch>`." +
		"\n2. Open a pull request against the repo's default branch through the same connector: POST $MUSTER_CONNECT_URL/connect/github/api.github.com/repos/" + repo + "/pulls with the same header."
}

// GitPushRecipe is the git command line (up to the refspec) that pushes through
// a connector's proxy.
//
// 🔴 BOTH -c KEYS ARE SCOPED TO THE CONNECTOR'S PROXY URL, AND REDIRECTS ARE OFF.
// An unscoped `http.extraHeader` goes to every URL git talks to, and git's
// default redirect policy follows the first request's redirect carrying that
// header to whatever host it names — measured. Scoped to
// `$MUSTER_CONNECT_URL/connect/<connector>/` the token goes to the proxy only,
// and `followRedirects=false` keeps git from following even a redirect the
// proxy itself would never hand out (it answers 502 instead).
func GitPushRecipe(connector, path string) string {
	proxy := "$MUSTER_CONNECT_URL/connect/" + connector + "/"
	return `git -c "http.` + proxy + `.extraHeader=X-Muster-Token: $MUSTER_HOOK_TOKEN" -c "http.` + proxy +
		`.followRedirects=false" push ` + proxy + path
}

// AgentToolDispatch returns the dispatch closure executing a worker's tool calls
// in-process, scoped to that agent (its bound task). Used by both the kickoff
// (via the provisioner hook) and the interactive chat.
func (s *Server) AgentToolDispatch(ctx context.Context, a agents.Agent) agents.ToolDispatch {
	return func(name, arguments string) string {
		return s.dispatchAgentTool(ctx, a, name, arguments)
	}
}

func (s *Server) dispatchAgentTool(ctx context.Context, a agents.Agent, name, arguments string) string {
	arg := func(v any) error { return json.Unmarshal([]byte(orEmptyJSON(arguments)), v) }
	switch name {
	case "agent_get_task":
		if a.NoteID == nil {
			return toolErr("no task assigned to you")
		}
		n, err := s.ext.Notes.Get(ctx, *a.NoteID)
		if err != nil {
			return toolErr("could not load task: %v", err)
		}
		return toolJSON(n)
	case "agent_comment_task":
		if a.NoteID == nil {
			return toolErr("no task assigned to you")
		}
		var in struct{ Body string }
		_ = arg(&in)
		if strings.TrimSpace(in.Body) == "" {
			return toolErr("body required")
		}
		c, err := s.ext.Notes.AddComment(ctx, notes.Comment{NoteID: *a.NoteID, Author: a.Name, Body: in.Body})
		if err != nil {
			return toolErr("could not comment: %v", err)
		}
		s.broadcast(EventTaskChanged, strconv.FormatInt(*a.NoteID, 10))
		return toolJSON(c)
	case "agent_set_task_status":
		if a.NoteID == nil {
			return toolErr("no task assigned to you")
		}
		var in struct{ Status string }
		_ = arg(&in)
		if !notes.ValidStatus(in.Status) {
			return toolErr("invalid status %q", in.Status)
		}
		if !notes.StatusAllowedForAgent(in.Status) {
			return toolErr("agents may not set %q; only the operator/human can mark a task complete", in.Status)
		}
		n, err := s.setNoteStatus(ctx, writerAgentTool, *a.NoteID, in.Status)
		if err != nil {
			return toolErr("could not set status: %v", err)
		}
		s.broadcast(EventTaskChanged, strconv.FormatInt(*a.NoteID, 10))
		// Background push when the agent's own tool marks the task ready_for_review.
		s.notifyTaskDone(n)
		return toolJSON(n)
	case "agent_list_pull_requests":
		return s.listAgentPullRequests(ctx, a, arguments)
	case "connectors_list":
		return s.toolConnectorsList(ctx, a)
	case "connector_request":
		var in struct{ Name, Reason string }
		_ = arg(&in)
		return s.toolConnectorRequest(ctx, a, in.Name, in.Reason)
	case "connector_status":
		var in struct {
			Name        string `json:"name"`
			WaitSeconds int    `json:"wait_seconds"`
		}
		_ = arg(&in)
		return s.toolConnectorStatus(ctx, a, in.Name, in.WaitSeconds)
	default:
		return toolErr("unknown tool %q", name)
	}
}

// listAgentPullRequests backs the agent_list_pull_requests worker tool: it
// resolves the connected GitHub token (server-side, never surfaced to the LLM),
// lists the agent's OWN repo's PRs, and returns a compact JSON array. The repo
// is implicit (a.Repo) — an agent only reads its own repo.
func (s *Server) listAgentPullRequests(ctx context.Context, a agents.Agent, arguments string) string {
	if strings.TrimSpace(a.Repo) == "" {
		return toolErr("agent has no repo")
	}
	if s.ext.GitHub == nil {
		return toolErr("no GitHub connection")
	}
	conn, ok, err := s.ext.GitHub.Get(ctx)
	if err != nil {
		return toolErr("could not load GitHub connection: %v", err)
	}
	if !ok || conn.Token == "" {
		return toolErr("no GitHub connection")
	}
	var in struct{ State string }
	_ = json.Unmarshal([]byte(orEmptyJSON(arguments)), &in)
	state := strings.TrimSpace(in.State)
	switch state {
	case "open", "closed", "all":
	default:
		state = "open"
	}
	prs, err := s.listPullRequests(ctx, conn.Token, a.Repo, state)
	if err != nil {
		return toolErr("could not list pull requests: %v", err)
	}
	if len(prs) > 50 {
		prs = prs[:50]
	}
	// A compact view: drop nested noise, keep what an agent needs to reason.
	type prView struct {
		Number    int    `json:"number"`
		Title     string `json:"title"`
		State     string `json:"state"`
		Author    string `json:"author"`
		Head      string `json:"head"`
		Base      string `json:"base"`
		Draft     bool   `json:"draft"`
		URL       string `json:"url"`
		UpdatedAt string `json:"updated"`
	}
	out := make([]prView, 0, len(prs))
	for _, p := range prs {
		out = append(out, prView{
			Number: p.Number, Title: p.Title, State: p.State, Author: p.User,
			Head: p.HeadRef, Base: p.BaseRef, Draft: p.Draft, URL: p.HTMLURL, UpdatedAt: p.UpdatedAt,
		})
	}
	return toolJSON(out)
}
