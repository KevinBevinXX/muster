package api

import (
	"context"
	"errors"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
)

// ---------------------------------------------------------------------------
// THE CHIEF AGENT
//
// 🔴 "CHIEF" NAMES TWO DIFFERENT THINGS IN THIS PACKAGE AND THEY ARE NOT
// RELATED. internal/api/chief.go owns the CHIEF TOKEN — an auth tier for the
// agent-identified terminal-write door. This file owns the CHIEF AGENT: a row in
// the agents table that happens to be the fleet's orchestrator, and the only
// inference path muster has. A session that conflates them will go looking for
// a summariser in an auth check.
//
// 🔴 THE CANONICAL NAME IS GENERATED AND IS NOT "chief". POST /agents mints its
// own slug — the live one is `zesty-stoat`, namespace `devpod-zesty-stoat` — and
// that slug is the NAMESPACE IDENTITY, immutable by design (see
// agents.Store.SetDisplayName: "the slug Name — the namespace key — is
// immutable"). What says "chief" is the DISPLAY name, which is mutable and is
// the operator's label. So the lookup is by DisplayName, and a literal
// GetByName("chief") would resolve nothing on the live fleet while looking
// perfectly correct in review.
//
// ⚠ AND IT IS NOT AN ID EITHER. The live chief is id 71 today; a re-provision
// mints a new row. Nothing here may pin that number.
// ---------------------------------------------------------------------------

// ChiefDisplayName is the display name that marks an agent as the fleet's chief.
const ChiefDisplayName = "chief"

// ErrNoChief reports that no agent carries the chief display name. It is a
// CONFIGURATION state, not a failure: a deployment with no chief renders recap
// cards that say so rather than a broken page.
var ErrNoChief = errors.New("no agent carries the chief display name")

// ErrChiefNotRunning reports that the chief agent exists but its pod is not up,
// so there is nothing to ask.
var ErrChiefNotRunning = errors.New("the chief agent is not running")

// chiefAgent resolves the chief by DISPLAY NAME.
//
// 🔴 IT READS THE WHOLE agents TABLE ON EVERY CALL, INCLUDING FROM THE PANEL'S
// SEARCH ROUTE, AND THAT IS KEPT WITH A MEASUREMENT RATHER THAN ASSUMED CHEAP.
// MEASURED against a local Postgres 16, 50 calls per point, on a box
// running many concurrent suites (so these are upper bounds, not best cases):
// PGStore.List costs 329µs at 71 SEEDED rows, 369µs at 200 and 1.36ms at 1000. The
// SearchSessions call it scopes cost 402–716µs at the same points, and the panel's
// search input debounces with htmx's `delay:300ms`, which RESETS on each keystroke:
// a continuously-typed query is ONE request, not one per character. So the chief
// lookup is about half of the read it exists to scope, against a 300ms floor between
// reads — and 1.36ms at a thousand rows is the number the decision rests on, whatever
// the live table's size is.
//
// ⚠ TWO CORRECTIONS TO HOW THAT USED TO BE STATED, NEITHER CHANGING THE DECISION.
// (a) 71 IS AN ID, NOT A ROW COUNT. This said "71 rows — the live table's size, since
// the live chief is id 71", which does not follow: an id is a lower bound on rows ever
// CREATED, and this very arc documents deleting rows from a sibling table. The live
// row count is UNMEASURED; 71 is simply the smallest point that was seeded.
// (b) THERE IS NO "1.4µs PER ROW". That figure was total/rows at the 1000-row point
// only, and the curve is dominated by a constant: the same arithmetic gives 4.6µs/row
// at 71 and 1.8µs/row at 200. Fitting the 71→1000 endpoints puts the marginal cost near
// 1.1µs/row over a fixed cost of roughly 250µs, so a per-row rate quoted on its own
// will mislead at any size that matters.
//
// 🔴 AND IT IS DELIBERATELY NOT CACHED. A TTL cache here would make every consumer
// resolve a chief that WAS the chief — which is the exact staleness that let the
// panel's new-thread control write to the previous chief (see
// handleAgentSessionCreate's panel branch). Reducing this to one row is the correct
// move if the table ever grows by an order of magnitude: a store-level
// `GetByDisplayName` doing `ORDER BY id LIMIT 1` keeps the tie-break below and is
// staleness-free. It is not worth an interface method, a SQL path and three
// implementations while the whole read costs 1.36ms at a THOUSAND rows against a
// 300ms floor — which is the bound that decides this, not a live row count nobody
// has measured.
//
// ⚠ TIES ARE RESOLVED BY THE LOWEST ID, DELIBERATELY. Two agents labelled
// `chief` is an operator mistake, not a supported topology, and picking
// whichever the store listed first would make the recap's source depend on query
// order — the same recap arriving from two different agents across two renders,
// with nothing on screen saying so. Lowest id is arbitrary but STABLE, which is
// the property that matters.
func (s *Server) chiefAgent(ctx context.Context) (agents.Agent, error) {
	if s.ext.Agents == nil {
		return agents.Agent{}, ErrNoChief
	}
	list, err := s.ext.Agents.List(ctx)
	if err != nil {
		return agents.Agent{}, err
	}
	found, ok := resolveChief(list)
	if !ok {
		return agents.Agent{}, ErrNoChief
	}
	return found, nil
}

// resolveChief is chiefAgent's rule over a list already read: the lowest-id
// agent whose display name is ChiefDisplayName. It is the one place that rule
// is written; chiefAgent and isChiefAmong both call it.
func resolveChief(list []agents.Agent) (agents.Agent, bool) {
	var found agents.Agent
	var ok bool
	for _, a := range list {
		if !strings.EqualFold(strings.TrimSpace(a.DisplayName), ChiefDisplayName) {
			continue
		}
		if !ok || a.ID < found.ID {
			found, ok = a, true
		}
	}
	return found, ok
}

// ---------------------------------------------------------------------------
// DELETION PROTECTION
//
// 🔴 ONE PREDICATE, EVERY DESTROY PATH. The chief is the fleet's orchestrator and
// the only inference path muster has; deleting it by a stray tap in a ⋯ menu, or
// as a side effect of dismissing the task it was once linked to, removes the
// thing the operator talks to. Every caller of Provisioner.Destroy in this package
// asks agentIsChief first (TestEveryDestroyCallSiteIsChiefGuarded holds the
// ledger of those callers), and the card's menu reads the same answer to hide
// Delete.
//
// "The chief" is BOTH of the two agents that can carry the role, because they
// are different rows on a real fleet (see the header above):
//   - the RESERVED SLUG agents.ChiefName, which POST /chief/provision creates and
//     which nothing else can mint; and
//   - the agent chiefAgent resolves — by display name, lowest id — which is what
//     the panel, recaps and `muster chief ask` actually talk to.
// Protecting only the first leaves the live chief (a generated slug labelled
// "chief") deletable; protecting only the second leaves the provisioned slug
// deletable the moment someone relabels it.
// ---------------------------------------------------------------------------

// chiefRefusal is the text every refused delete of the chief carries.
//
// ⚠ THE TWO CHIEFS GET DIFFERENT ADVICE BECAUSE ONLY ONE OF THEM HAS A WAY OUT.
// The reserved slug is protected ABSOLUTELY (operator decision): its name is
// immutable, so there is nothing to relabel and the message must not suggest
// there is. An agent that is the chief only because of its display name stops
// being the chief the moment it is renamed, and saying so is the remedy.
func chiefRefusal(a agents.Agent) string {
	if a.Name == agents.ChiefName {
		return "the chief agent cannot be deleted"
	}
	return "the chief agent cannot be deleted; this agent is the chief because it is labelled \"" +
		ChiefDisplayName + "\" — rename it first if it is really meant to go"
}

// chiefPredicate is THE chief predicate, bound to one read of the agent list:
// the chief is resolved once, so a caller deciding many agents (the card list)
// pays one resolve, not one per card.
func chiefPredicate(list []agents.Agent) func(agents.Agent) bool {
	c, ok := resolveChief(list)
	return func(a agents.Agent) bool {
		return a.Name == agents.ChiefName || (ok && a.ID == c.ID)
	}
}

// isChiefAmong reports whether a is the chief, given the full agent list.
func isChiefAmong(a agents.Agent, list []agents.Agent) bool { return chiefPredicate(list)(a) }

// agentIsChief reads the agent table and answers isChiefAmong for id, returning
// the agent's row with it (the refusal text depends on which chief it is). An
// agent that is not in the table is not the chief. A read error is returned, and
// every destroy path treats it as "might be the chief" — it FAILS CLOSED, since
// a refused delete can be retried and a destroyed chief cannot be undone.
func (s *Server) agentIsChief(ctx context.Context, id int64) (agents.Agent, bool, error) {
	if s.ext.Agents == nil {
		return agents.Agent{}, false, nil
	}
	list, err := s.ext.Agents.List(ctx)
	if err != nil {
		return agents.Agent{}, false, err
	}
	for _, a := range list {
		if a.ID == id {
			return a, isChiefAmong(a, list), nil
		}
	}
	return agents.Agent{}, false, nil
}

// 🔴 THERE IS NO chat_sessions ROW FOR RECAPS, AND DELETING IT IS THE FIX.
//
// This used to be a `chiefRecapSession` type holding a mutex, a key and an id,
// with the row created on first use. Its stated purpose — from its own comment —
// was that "up to 120 windows summarised on a loop would bury the operator's own
// conversation with chief under machine traffic, on the very page the slide-out
// adds for talking to them."
//
// 🔴 THAT HARM IS STRUCTURALLY IMPOSSIBLE AND ALWAYS WAS. A recap turn is NEVER
// persisted: chiefRecapGenerator.Generate calls Provisioner.Chat and returns the
// string. Enumerated with a positive control — AddChatMessage appears on the
// kickoff path (provision.go), the operator path (operator.go) and the web-chat
// path (agents.go), and ZERO times anywhere on the recap path. So no recap has
// ever been able to appear in any transcript, whatever session key it used, and
// the dedicated row was permanently empty by construction.
//
// 🔴 AND THE ROW WAS ACTIVELY HARMFUL — IT CAUSED THE BUG IT LOOKED LIKE IT WAS
// PREVENTING. chat_sessions.updated_at DEFAULTS TO now() (migration 0009), and
// both LatestOrCreateSession and ListSessions order `updated_at DESC, id DESC`.
// So the instant a recap row was inserted it became the agent's most-recently-
// updated session — which is where `muster chief ask` posts (operator.go)
// AND what GET /api/agents/{name}/messages resolves to by default
// (machine_agents.go). Measured on the upstream database: one agent
// carried four sessions, two of them empty recap rows created 29s and 24s after
// two pod rollouts, and reading chief's chat answered `count: 0` while the
// operator's actual conversation sat in the session one row back.
//
// ⚠ A DETERMINISTIC KEY DOES NOT FIX THAT, which is worth recording because it
// was the first fix attempted here. Inserting the row ONCE with a stable key
// still makes it newest — once, permanently — so the default read still resolves
// to an empty session. Only having no row at all removes the failure.
//
// ⚠ WHAT THE SEPARATION ACTUALLY BUYS is one GATEWAY CONTEXT, so recaps do not
// pollute the operator's conversational memory with the agent. That is bought by
// the session KEY STRING alone: Provisioner.Chat takes a sessionKey and hands it
// straight to chatStream, consulting no chat_sessions row. So the key below is
// all that was ever load-bearing.

// recapSessionName is the recap conversation's gateway key.
//
// 🔴 IT IS A PURE FUNCTION OF THE AGENT NAME — no clock, no counter, no store.
// That is what makes it survive a restart: the same agent yields the same key
// forever, so a rebooted pod rejoins the gateway context it was using instead of
// starting a new one. The store's own newSessionKey stamps
// time.Now().UnixNano(), which is precisely why it cannot be used here.
//
// ⚠ THE SUFFIX MUST NOT COLLIDE WITH legacySessionKey (`agent:<name>:webchat`),
// which is the key LatestOrCreateSession gives an agent's FIRST auto-created
// session — i.e. the operator's own thread on a fresh agent. Sharing it would put
// recaps back in the operator's gateway context, which is the one thing the
// separation is for.
func recapSessionName(agentName string) string {
	return "agent:" + agentName + ":webchat:recap"
}

// recapSessionKey returns the chief's recap gateway key.
//
// ⚠ IT TOUCHES NO STORE AND CANNOT FAIL. The (ctx, error) shape is kept because
// every caller already handles an error from it and the signature is the seam a
// future store-backed variant would need; returning a nil error from a pure
// function is cheaper than churning four call sites for no behaviour change.
func (s *Server) recapSessionKey(_ context.Context, a agents.Agent) (string, error) {
	return recapSessionName(a.Name), nil
}
