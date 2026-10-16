package api

import (
	"context"
	"net/http"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
)

// This file is where every authentication wrapper turns "the credential
// checked out" into "this request acts as THIS user" (accounts PR 2/6).
//
// 🔴 admit IS THE ONLY REQUEST-PATH CALL SITE OF authz.WithPrincipal IN THE
// MODULE (the other is the send queue's dispatcher, queuedPrincipal, which
// runs with no request), AND EVERY require* WRAPPER HANDS ITS ADMITTED REQUEST TO
// next THROUGH IT. One
// function is one place to get the attach right; a wrapper that called next
// directly would reach its handler with no principal, which under row-level
// security is an empty board and a failing insert.
// TestEveryAuthenticatedRouteAttachesAPrincipal drives every route in
// routes.golden through its real wrapper and demands the principal (with the
// right Via) arrive at admit's hand-off; internal/authz's
// TestOnlyTheAuthWrappersCallAdmit pins the call-site ledger (type-resolved, so
// a method value or package-level alias of admit counts too).
//
// 🔴 THE PRINCIPAL IS ENFORCED (accounts PR 3a): internal/db sets Postgres's
// muster.uid from it on every acquire, and row-level security shows the request
// only that user's rows. Its ROLE is read from users.role (3b; roleOf). With
// one user every principal is still user 1, admin, and behaviour is unchanged.

// machineUserID is who the SHARED machine tokens (hook, service) act as: the
// admin, user 1 — no deployment needs otherwise; a person uses a personal token.
func machineUserID() int64 { return authz.OperatorUserID }

// sessionUserID is who a valid browser session on r acts as. The password mode
// has exactly one login, the operator's, so it is user 1 by construction; PR
// 4's GoTrue mode resolves it from the token's subject instead.
//
// sessionUserOverride is the seam that resolution will replace. TEST-ONLY
// today: nothing in production sets it, and it exists so the isolation walk
// (isolation_test.go) can hold a SECOND user's session — the one door that
// cannot otherwise act as anyone but the operator until PR 4.
func (s *Server) sessionUserID(r *http.Request) int64 {
	if s.sessionUserOverride != nil {
		return s.sessionUserOverride(r)
	}
	return authz.OperatorUserID
}

// agentOwnerID is who a request authenticated by an AGENT'S OWN token acts as:
// the agent's owner (agents.owner_id). Every per-agent door (requireAgentToken,
// requireConnectorAgent, requireRelayAgent) derives the user here.
//
// 🔴 ZERO IS "NOBODY", AND THE DOORS REFUSE IT (agentPrincipal) rather than
// guess. A row read from Postgres always carries its owner (NOT NULL); a zero
// can only come from a store that never read the column, and acting as user 1
// for it would hand the operator's rows to whatever that agent is.
func agentOwnerID(a agents.Agent) int64 { return a.OwnerID }

// agentByToken resolves a per-agent token to its agent row, for the three
// per-agent doors (requireAgentToken directly; requireConnectorAgent and
// requireRelayAgent through authenticateCallingAgent).
//
// 🔴 IT RUNS UNDER authz.System, AND THAT IS NOT A SHORTCUT: IT IS HOW THE
// OWNER IS LEARNED. No principal exists yet — the token IS the credential, and
// the row it names says who the request acts as. Under row-level security a
// lookup with no principal sees no rows, so without System every /agent/*,
// /connect/*, /mcp and relay request would 401 (accounts PR 2's top follow-up;
// TestEachAgentDoorWorksUnderRowLevelSecurity is the regression test). The
// System context is used for this ONE store call and is never handed on: the
// handler gets the request's own context, with the principal built from the
// row's owner. It is one entry in the System ledger
// (internal/authz/ledger_test.go).
func (s *Server) agentByToken(ctx context.Context, token string) (agents.Agent, error) {
	return s.ext.Agents.GetByHooksToken(authz.System(ctx), token)
}

// agentPrincipal is the principal a request authenticated by agent a acts as,
// or false when the row names no owner (see agentOwnerID).
func (s *Server) agentPrincipal(ctx context.Context, a agents.Agent, via authz.Via) (authz.Principal, bool) {
	uid := agentOwnerID(a)
	if uid <= 0 {
		return authz.Principal{}, false
	}
	return s.principalFor(ctx, uid, via), true
}

// roleOf is the role user uid acts with, read from users.role (accounts PR
// 3b; until then every principal was admin by construction).
//
// 🔴 IT READS UNDER authz.System, FOR THE SAME REASON agentByToken DOES: it
// runs inside the wrappers, BEFORE the principal it completes exists. It reads
// one column of one row and hands nothing else on. System ledger entry.
//
// 🔴 EVERY FAILURE IS MEMBER, NEVER ADMIN. A read error, a missing row or an
// unknown role value acts with the LEAST privilege: the one branch on the role
// today is the approval relay, and a wrong "admin" files a stranger's prompt on
// the operator's phone, while a wrong "member" only sends a prompt back to the
// agent's own terminal.
//
// ⚠ WITH NO users STORE WIRED (unit tests over fakes) the operator — user 1 —
// is admin and everyone else is a member: the seeded row, without a database.
// cmd/muster-server always wires the store beside the database.
func (s *Server) roleOf(ctx context.Context, uid int64) authz.Role {
	if s.ext.Users == nil {
		if uid == authz.OperatorUserID {
			return authz.RoleAdmin
		}
		return authz.RoleMember
	}
	r, err := s.ext.Users.Role(authz.System(ctx), uid)
	if err != nil {
		s.logger.Printf("principal: reading user %d's role: %v (acting as member)", uid, err)
		return authz.RoleMember
	}
	return r
}

// principalFor builds the request principal for a user admitted through via.
func (s *Server) principalFor(ctx context.Context, uid int64, via authz.Via) authz.Principal {
	return authz.Principal{UserID: uid, Role: s.roleOf(ctx, uid), Via: via}
}

// admit attaches p to the request and hands it to next. See the file header:
// it is the only way a wrapper may call next.
func (s *Server) admit(w http.ResponseWriter, r *http.Request, next http.HandlerFunc, p authz.Principal) {
	// A plugin token opens the plugin door and nothing else (plugins.go).
	if s.refusePluginTokenElsewhere(w, r, p.Via) {
		return
	}
	r = r.WithContext(authz.WithPrincipal(r.Context(), p))
	if s.principalProbe != nil {
		s.principalProbe(r)
	}
	next(w, r)
}
