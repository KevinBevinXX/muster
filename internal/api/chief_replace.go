package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// --- replacing the chief ----------------------------------------------------
//
// POST /api/chief/replace deletes the caller's reserved-slug chief and creates a
// new one under the same slug, on the kind (and Claude account) the request
// names. It exists because an agent's kind is immutable (migration 0003) and the
// chief is delete-protected everywhere else (agentIsChief), so there was no
// supported way to move the chief from one runtime to another.
//
// 🔴 IT IS THE ONE PATH THAT DESTROYS A CHIEF, AND ONLY WITH confirm. Without
// `confirm` equal to the reserved slug it changes nothing and answers 428 with
// the plan — what it would delete and what it would create — so the caller sees
// exactly what the confirmed call does. TestEveryDestroyCallSiteIsChiefGuarded
// ledgers replaceChief as the single deliberate bypass of the chief guard.
//
// 🔴 IT REFUSES WHILE A TURN IS IN FLIGHT TO THE OLD CHIEF, IN TWO STEPS. First it
// takes the old chief's SESSION CLAIM (agents.Store.ClaimKickoff — the claim a
// plugin delivery, a kickoff and a memo turn each hold for their whole turn): a
// claim it cannot take means such a turn is running, and it refuses. Holding the
// claim, no NEW delivery can begin to that chief, so the second check cannot race:
// any plugin message still `delivering` to it refuses the swap, because deleting
// the row would set that message's agent_id NULL and nothing could ever settle it.
//
// 🔴 IT IS RETRY-SAFE BY READING STATE, NOT BY REMEMBERING IT. Every call
// re-derives the plan from the agent table: the reserved slug present on another
// kind or account → delete it, then create; present on the requested kind and
// account → nothing to delete, re-apply it (Start re-builds the spec); absent →
// create. So a run that deleted the old chief and then failed to create the new
// one is finished by running the same command again.
//
// ⚠ IT IS SYNCHRONOUS UP TO THE CREATE. Destroy runs inside the request so its
// failure is the answer (the row is kept on a failed destroy — see
// agentprovision.Adapter.Destroy — and a re-run retries it). The new agent's
// provisioning then runs in the background exactly as POST /chief/provision's
// does; its progress is the new row's status.

// chiefReplaceRequest is POST /api/chief/replace's JSON body.
type chiefReplaceRequest struct {
	// Kind is the new chief's kind ("" = gateway).
	Kind string `json:"kind"`
	// CCAccount pins a claude-code chief's Claude account; "" lets the pool choose.
	CCAccount string `json:"cc_account"`
	// Model is a gateway chief's model; "" = the deployment default.
	Model string `json:"model"`
	// Confirm must equal the reserved slug being replaced, or nothing happens.
	Confirm string `json:"confirm"`
}

// chiefReplaceClaimTTL bounds how long a swap holds the old chief's session
// claim. The claim dies with the row on success; the TTL is the backstop for a
// process that dies mid-swap.
const chiefReplaceClaimTTL = 10 * time.Minute

// chiefIntro is the new chief's first turn: what it is, and where its
// instructions already are.
func chiefIntro(kind string) string {
	if agents.ResolveKind(kind) == agents.KindClaudeCode {
		return "You are chief, muster's fleet agent. You have no task and no repo, and that is expected. " +
			"Your standing instructions are already loaded (the managed CLAUDE.md); report the current state of " +
			"the fleet using your muster tools (mcp__muster__…)."
	}
	return "You are chief, muster's fleet agent. You have no task and no repo, and that is expected. " +
		"Read AGENTS.md, then report the current state of the fleet."
}

// chiefReplaceBrief is the shape this route reports an agent in.
func chiefReplaceBrief(a agents.Agent) map[string]any {
	return map[string]any{
		"id": a.ID, "name": a.Name, "displayName": a.DisplayName,
		"kind": agents.ResolveKind(a.Kind), "ccAccount": a.CCAccount, "status": a.Status,
	}
}

func (s *Server) handleChiefReplace(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.ext.Agents == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "this server has no agent store"})
		return
	}
	owner := requestOwner(ctx)
	if owner <= 0 {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": "no user to replace a chief for"})
		return
	}
	var req chiefReplaceRequest
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
	if err == nil && len(strings.TrimSpace(string(body))) > 0 {
		err = json.Unmarshal(body, &req)
	}
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the body must be a JSON object: " + err.Error()})
		return
	}
	code, out := s.replaceChief(ctx, owner, req)
	s.writeJSON(w, code, out)
}

// replaceChief is the swap. It returns the HTTP status and the JSON body.
func (s *Server) replaceChief(ctx context.Context, owner int64, req chiefReplaceRequest) (int, map[string]any) {
	refuse := func(code int, format string, a ...any) (int, map[string]any) {
		return code, map[string]any{"error": fmt.Sprintf(format, a...)}
	}
	req.Model = strings.TrimSpace(req.Model)
	req.CCAccount = strings.TrimSpace(req.CCAccount)

	// PRE-FLIGHT: every refusal the create would raise is raised BEFORE anything
	// is deleted, so a bad kind, account or model cannot leave the owner with no
	// chief at all.
	kind, account, err := s.resolveKindAccount(ctx, dispatchParams{Kind: req.Kind, CCAccount: req.CCAccount, Model: req.Model})
	if err != nil {
		return refuse(http.StatusConflict, "%v", err)
	}
	if err := agents.ValidateModelCredential(req.Model); err != nil {
		return refuse(http.StatusBadRequest, "%v", err)
	}
	if kind == agents.KindClaudeCode && s.ext.Connectors == nil {
		// A claude-code chief reaches every chief tool over POST /mcp, which is
		// served only with the connector store.
		return refuse(http.StatusConflict, "a %s chief reaches its tools only over muster's MCP server, which this "+
			"deployment does not serve (the connector store is not configured)", agents.KindClaudeCode)
	}

	chiefName := agents.ChiefNameFor(owner)
	list, err := s.ext.Agents.List(ctx)
	if err != nil {
		return refuse(http.StatusInternalServerError, "could not read the agent table; nothing was changed: %v", err)
	}
	var old agents.Agent
	hasOld := false
	for _, a := range list {
		if a.Name == chiefName {
			old, hasOld = a, true
		}
	}
	// 🔴 ANOTHER AGENT LABELLED chief WOULD SHADOW THE NEW ONE. Everything that
	// talks to the chief resolves it by display name, lowest id first
	// (resolveChief), and the new row's id is the highest there is — so the swap
	// would "succeed" while the panel, recaps and plugin delivery kept talking to
	// the older labelled agent.
	for _, a := range list {
		if a.Name != chiefName && a.OwnerID == owner && strings.EqualFold(strings.TrimSpace(a.DisplayName), ChiefDisplayName) {
			return refuse(http.StatusConflict, "agent %q (id %d) is also labelled %q and would keep answering as the chief "+
				"after the swap; rename it first", a.Name, a.ID, ChiefDisplayName)
		}
	}

	keep := hasOld && agents.ResolveKind(old.Kind) == kind && (req.CCAccount == "" || old.CCAccount == req.CCAccount)
	plan := map[string]any{"name": chiefName, "kind": kind}
	if account != "" {
		plan["ccAccount"] = account
	}
	var deletes any
	if hasOld && !keep {
		deletes = chiefReplaceBrief(old)
	}
	if strings.TrimSpace(req.Confirm) != chiefName {
		// The plan is in the error SENTENCE as well as in the fields: a client
		// that renders only `error` (cmd/muster's classify does) still shows the
		// caller exactly what the confirmed call would do.
		action, sentence := "create", ""
		switch {
		case keep:
			action = "keep and re-apply"
			sentence = fmt.Sprintf("%s (id %d) is already a %s chief; the confirmed call deletes nothing and re-applies it",
				old.Name, old.ID, kind)
		case hasOld:
			sentence = fmt.Sprintf("the confirmed call DELETES agent %s (id %d, kind %s%s, status %s) with its chat history, "+
				"and creates %s on kind %s%s", old.Name, old.ID, agents.ResolveKind(old.Kind), accountSuffix(old.CCAccount),
				old.Status, chiefName, kind, accountSuffix(account))
		default:
			sentence = fmt.Sprintf("there is no %s; the confirmed call deletes nothing and creates it on kind %s%s",
				chiefName, kind, accountSuffix(account))
		}
		return http.StatusPreconditionRequired, map[string]any{
			"error":   fmt.Sprintf("nothing was changed: %s. Repeat with confirm %q to apply it.", sentence, chiefName),
			"delete":  deletes,
			"action":  action,
			"chief":   plan,
			"confirm": chiefName,
		}
	}

	if keep {
		// Already the requested chief: a half-finished earlier run, or a re-run.
		// Start re-builds and re-applies its spec (and creates an absent instance).
		opCtx := context.WithoutCancel(ctx)
		safeGo(s.logger, "chief replace: start", func() {
			if err := s.ext.Provisioner.Start(opCtx, old.ID); err != nil {
				s.logger.Printf("chief replace: start %s: %v", old.Name, err)
			}
		})
		return http.StatusOK, map[string]any{"deleted": nil, "created": false, "started": true, "agent": chiefReplaceBrief(old)}
	}

	var deleted map[string]any
	// report is deleted as the body carries it: JSON null — and an untyped nil
	// in the map — when nothing was deleted, never a typed nil map.
	report := func() any {
		if deleted == nil {
			return nil
		}
		return deleted
	}
	if hasOld {
		claimOwner := "chief-replace/" + randomHex(6)
		won, err := s.ext.Agents.ClaimKickoff(ctx, old.ID, claimOwner, chiefReplaceClaimTTL)
		if err != nil {
			return refuse(http.StatusInternalServerError, "could not take the chief's session claim; nothing was changed: %v", err)
		}
		if !won {
			return refuse(http.StatusConflict, "a turn is being delivered to the chief right now (a plugin message, its "+
				"kickoff or a memo); nothing was changed — retry when it finishes")
		}
		release := func() {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			_ = s.ext.Agents.ReleaseKickoffClaim(rctx, old.ID, claimOwner)
		}
		if s.ext.Plugins != nil {
			ids, err := s.ext.Plugins.DeliveringTo(ctx, old.ID)
			if err != nil {
				release()
				return refuse(http.StatusInternalServerError, "could not read the chief's plugin deliveries; nothing was changed: %v", err)
			}
			if len(ids) > 0 {
				release()
				return http.StatusConflict, map[string]any{
					"error": fmt.Sprintf("%d plugin message(s) are still `delivering` to the chief; deleting it now would strand "+
						"them for good. Nothing was changed.", len(ids)),
					"delivering": ids,
				}
			}
		}
		deleted = chiefReplaceBrief(old)
		if sessions, err := s.ext.Agents.ListSessions(ctx, old.ID); err == nil {
			deleted["chatSessions"] = len(sessions)
		}
		// 🔴 THE CHIEF GUARD IS BYPASSED HERE AND NOWHERE ELSE, behind confirm.
		if err := s.ext.Provisioner.Destroy(ctx, old.ID); err != nil {
			release()
			s.logger.Printf("chief replace: destroy %s (id %d): %v", old.Name, old.ID, err)
			return http.StatusBadGateway, map[string]any{
				"error":   fmt.Sprintf("the old chief could not be removed, so no new one was created: %v", err),
				"deleted": nil,
				"created": false,
			}
		}
		s.logger.Printf("chief replace: deleted %s (id %d, kind %s)", old.Name, old.ID, agents.ResolveKind(old.Kind))
	}

	rec, err := s.createAndDispatchAgent(ctx, dispatchParams{
		Name: chiefName, DisplayName: ChiefDisplayName, Kind: kind, CCAccount: account, Model: req.Model,
		NoteText: chiefIntro(kind), Kickoff: true,
	})
	if err != nil {
		s.logger.Printf("chief replace: create %s: %v", chiefName, err)
		code := http.StatusBadGateway
		if errors.Is(err, ErrDispatchRefused) || errors.Is(err, agents.ErrModelNotCredentialed) {
			code = http.StatusConflict
		}
		return code, map[string]any{
			"error":   fmt.Sprintf("the new chief was not created: %v — re-run the same command to create it", err),
			"deleted": report(),
			"created": false,
		}
	}
	return http.StatusOK, map[string]any{"deleted": report(), "created": true, "agent": chiefReplaceBrief(rec)}
}

func accountSuffix(account string) string {
	if account == "" {
		return ""
	}
	return ", Claude account " + account
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
