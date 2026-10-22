package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// SHARED APPROVALS: STANDING GRANTS (migration 0043, docs/connectors.md).
//
// Approve once; the approval applies to all of the owner's agents.
//
// An approval — on the Connectors tab, from a mirrored session
// (connector_request_decide) or with a credential entered for a request — goes
// through decideConnectorGrant, which records a STANDING GRANT for the owner
// (shareApproval). Every request filed — by the request route, the
// connector_request tool or the proxy — is first offered to
// approveFromStanding, and the owner is prompted only when no standing grant
// covers it. A grant RE-OPENED because its connector's credential source
// changed is not offered, by any door, in the request that re-opened it: that
// change asks the owner again, as 0033 decided, and the approval is then
// shared. And recording a standing grant under one source RETIRES the
// connector's standing grants under the other (CreateStandingGrant), so an
// approval of the stored token cannot come back into force when the App that
// replaced it is removed.
//
// 🔴 THE KEY IS (connector, credential source, repository for the GitHub App).
// The App's token reaches only the agent's OWN repository, so an App approval
// for example-org/widget never approves an agent on another repository; a
// stored credential is the same credential whichever agent sends it, so its
// key is the connector alone (connectors.StandingRepo).
//
// 🔴 AN AUTOMATIC APPROVAL GIVES EXACTLY WHAT A MANUAL ONE WOULD: it is the
// same per-agent grant row, approved against the same credential source, so the
// proxy serves it exactly as it serves a grant the owner approved by hand. And
// it passes every refusal a manual approval passes — the connector must exist.
// A worker-pool member is never approved from a standing grant at all, for any
// connector (its requests wait for the owner as before), and its own approvals
// are never shared.
// ---------------------------------------------------------------------------

// standingKey is the standing-grant key a grant on connector would be approved
// under for agent a, for ctx's principal (the owner): the credential source the
// connector has for the owner and the repository part. ok is false when an
// approval for this agent is never shared (the App, an agent with no
// repository: that approval grants nothing usable).
//
// 🔴 NEVER ON A FALLBACK ROLE (connectors.ReconcileGrant's rule): a principal
// whose role could not be read acts as a member, for whom the App vanishes, so
// an admin's `github` would be keyed `stored` and could match a stored-token
// standing grant. No key, no match: the request waits for the owner.
func (s *Server) standingKey(ctx context.Context, connector string, a agents.Agent) (source, repo string, ok bool) {
	if p, has := authz.From(ctx); has && p.RoleUncertain {
		return "", "", false
	}
	source = connectors.SourceFor(ctx, s.ext.Connectors, connector)
	repo, ok = connectors.StandingRepo(source, a.Repo)
	return source, repo, ok
}

// approveFromStanding approves pending grant g when one of its owner's live
// standing grants covers it, auditing where the approval came from and nudging
// the owner's Connectors tab. It is THE rule every door that leaves a grant
// pending applies (the request route and tool through onGrantPending, the proxy
// through connectors.Proxy.AutoApprove). false leaves g pending, untouched.
func (s *Server) approveFromStanding(ctx context.Context, g connectors.Grant) (connectors.Grant, bool) {
	if s.ext.Connectors == nil || s.ext.Agents == nil || g.Status != connectors.StatusPending {
		return g, false
	}
	a, err := s.ext.Agents.Get(ctx, g.AgentID)
	if err != nil {
		return g, false
	}
	if _, err := s.ext.Connectors.GetConnector(ctx, g.Connector); err != nil {
		return g, false
	}
	// 🔴 A WORKER-POOL MEMBER IS NEVER APPROVED FROM A STANDING GRANT, FOR ANY
	// CONNECTOR: it runs machine jobs that review code which may not be the
	// operator's own (jobs_grants.go), possibly with permission bypass, so each
	// of its requests waits for the owner, as before shared approvals.
	if a.WorkerPool != "" {
		return g, false
	}
	source, repo, ok := s.standingKey(ctx, g.Connector, a)
	if !ok {
		return g, false
	}
	sg, err := s.ext.Connectors.MatchStandingGrant(ctx, g.AgentID, g.Connector, source, repo)
	if err != nil {
		if !errors.Is(err, connectors.ErrNotFound) {
			s.logger.Printf("connectors: standing grant for request %d: %v", g.ID, err)
		}
		return g, false
	}
	ng, err := s.ext.Connectors.ApproveFromStanding(ctx, g.ID, sg.ID)
	if err != nil {
		if !errors.Is(err, connectors.ErrNotPending) {
			s.logger.Printf("connectors: approve request %d from standing grant %d: %v", g.ID, sg.ID, err)
		}
		return g, false
	}
	s.auditGrant(ctx, ng, standingApprovalDetail(sg))
	s.logger.Printf("connectors: request %d (agent %q, connector %q) approved by standing grant %d", ng.ID, ng.AgentName, ng.Connector, sg.ID)
	s.broadcast(ctx, EventConnectorChanged, strconv.FormatInt(ng.ID, 10))
	return ng, true
}

// standingApprovalDetail is an automatic approval's audit line. It names the
// standing grant and the request whose approval created it: no human decided
// THIS request, and the line never says one did.
func standingApprovalDetail(sg connectors.StandingGrant) string {
	d := "approved automatically by standing grant " + strconv.FormatInt(sg.ID, 10)
	if sg.OriginGrantID != 0 {
		d += " (shared from your approval of request " + strconv.FormatInt(sg.OriginGrantID, 10) + ")"
	}
	return d
}

// onGrantPending is what the agent routes do with a request they just filed:
// approve it from a standing grant, or else prompt the owner
// (onConnectorRequested). It returns the grant as it now is.
func (s *Server) onGrantPending(ctx context.Context, g connectors.Grant) connectors.Grant {
	if ng, ok := s.approveFromStanding(ctx, g); ok {
		return ng
	}
	s.onConnectorRequested(ctx, g)
	return g
}

// sharedApproval is what shareApproval did with one approval.
type sharedApproval struct {
	// Standing is the standing grant the approval now stands for (zero when the
	// approval is not shared: the App for an agent with no repository).
	Standing connectors.StandingGrant
	// Created is false when a live standing grant already held the key.
	Created bool
	// Approved are the other pending requests it approved in the same step.
	Approved []connectors.Grant
}

// shareApproval makes approved grant g the owner's standing grant for its key
// and approves every request already WAITING that the standing grant covers.
// via is where the approval was made (recorded on the audit line).
//
// Errors are logged, not returned: the approval itself has already landed, and
// a standing grant that could not be recorded only means the next agent asks.
func (s *Server) shareApproval(ctx context.Context, g connectors.Grant, via connectors.DecisionSource) sharedApproval {
	var out sharedApproval
	if s.ext.Agents == nil || !g.Usable() {
		return out
	}
	// Belt and braces for decideConnectorGrant's refusal: never share (and so
	// never retire another source's standing grants) on a fallback role.
	if p, ok := authz.From(ctx); ok && p.RoleUncertain {
		return out
	}
	a, err := s.ext.Agents.Get(ctx, g.AgentID)
	if err != nil {
		s.logger.Printf("connectors: share approval %d: agent: %v", g.ID, err)
		return out
	}
	// A pool worker's approval is its own: it is never shared (and never
	// matched: approveFromStanding).
	if a.WorkerPool != "" {
		return out
	}
	repo, ok := connectors.StandingRepo(g.Source, a.Repo)
	if !ok {
		return out
	}
	sg, created, err := s.ext.Connectors.CreateStandingGrant(ctx, g.Connector, g.Source, repo, g.ID)
	if err != nil {
		s.logger.Printf("connectors: share approval %d: %v", g.ID, err)
		return out
	}
	out.Standing, out.Created = sg, created
	if created {
		d := "approval shared as standing grant " + strconv.FormatInt(sg.ID, 10) + ": later requests for " + g.Connector
		if sg.Repo != "" {
			d += " on " + sg.Repo
		}
		d += " from any of your agents are approved automatically"
		if via.ViaSession() {
			d += " (approved via MCP from " + via.String() + ")"
		}
		s.auditGrant(ctx, g, d)
	}
	pending, err := s.ext.Connectors.ListPendingGrants(ctx)
	if err != nil {
		s.logger.Printf("connectors: share approval %d: pending: %v", g.ID, err)
		return out
	}
	for _, p := range pending {
		if p.Connector != g.Connector || p.ID == g.ID {
			continue
		}
		if ng, ok := s.approveFromStanding(ctx, p); ok {
			out.Approved = append(out.Approved, ng)
		}
	}
	return out
}

// shareNotice is the sentence the Connectors tab adds to an approval's notice.
func shareNotice(sh sharedApproval) string {
	if sh.Standing.ID == 0 {
		return ""
	}
	n := " Shared with all your agents"
	if sh.Standing.Repo != "" {
		n += " on " + sh.Standing.Repo
	}
	n += "."
	if k := len(sh.Approved); k > 0 {
		n += " " + strconv.Itoa(k) + " waiting request"
		if k > 1 {
			n += "s"
		}
		n += " approved too."
	}
	return n
}

// shareSentence is what a request card says approving it will share (empty when
// it will not be shared). source and repo are the request's standing key.
func shareSentence(connector, source, repo string) string {
	if source == connectors.SourceApp {
		return "Approving also approves every request for " + connector + " on " + repo +
			" from any of your agents — now and later — until you revoke the shared approval."
	}
	return "Approving also approves every request for " + connector +
		" from any of your agents — now and later — until you revoke the shared approval."
}

// requestShares is what approving pending request g would share with the
// owner's other agents (shareSentence), or "" when it shares nothing: no such
// connector yet (the answer is a credential, and creating it shares the same
// way — the form says so), an agent that cannot be read, ANY request of a
// worker-pool member (its approvals are never shared: shareApproval), or an App
// request from an agent with no repository.
func (s *Server) requestShares(ctx context.Context, g connectors.Grant) string {
	if s.ext.Agents == nil {
		return ""
	}
	if _, err := s.ext.Connectors.GetConnector(ctx, g.Connector); err != nil {
		return ""
	}
	a, err := s.ext.Agents.Get(ctx, g.AgentID)
	if err != nil || a.WorkerPool != "" {
		return ""
	}
	source, repo, ok := s.standingKey(ctx, g.Connector, a)
	if !ok {
		return ""
	}
	return shareSentence(g.Connector, source, repo)
}

// handleStandingGrantRevoke serves DELETE /connector-standing-grants/{id}: the
// shared approval ends, and so does every grant it gave.
func (s *Server) handleStandingGrantRevoke(w http.ResponseWriter, r *http.Request) {
	if s.ext.Connectors == nil {
		http.Error(w, ConnectorsUnconfigured, http.StatusServiceUnavailable)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	sg, revoked, err := s.ext.Connectors.RevokeStandingGrant(ctx, id, "operator")
	switch {
	case errors.Is(err, connectors.ErrNotPending), errors.Is(err, connectors.ErrNotFound):
		s.renderConnectorsContent(w, r, ui.ConnectorsContent{Error: "That shared approval was already ended."})
		return
	case err != nil:
		s.logger.Printf("connectors: revoke standing grant %d: %v", id, err)
		http.Error(w, "could not revoke", http.StatusInternalServerError)
		return
	}
	scope := sg.Connector
	if sg.Repo != "" {
		scope += " on " + sg.Repo
	}
	s.auditGrant(ctx, connectors.Grant{Connector: sg.Connector}, "standing grant "+strconv.FormatInt(sg.ID, 10)+
		" ("+scope+") revoked by the operator; "+strconv.Itoa(len(revoked))+" grant(s) it gave revoked with it")
	for _, g := range revoked {
		s.auditGrant(ctx, g, "revoked with standing grant "+strconv.FormatInt(sg.ID, 10))
	}
	s.broadcast(ctx, EventConnectorChanged, sg.Connector)
	s.renderConnectorsContent(w, r, ui.ConnectorsContent{Notice: "The shared approval of " + scope +
		" was revoked, with " + strconv.Itoa(len(revoked)) + " agent grant(s) it gave. Agents asking again will need your approval."})
}

// standingScope is " on <repo>" for an App standing grant, "" otherwise.
func standingScope(sg connectors.StandingGrant) string {
	if sg.Repo == "" {
		return ""
	}
	return " on " + sg.Repo
}
