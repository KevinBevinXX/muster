package agents

import (
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/provision"
)

// The repository checkout a claude-code agent's ccd reports on /healthz under
// `repo.state` (cmd/ccd/checkout.go). ccd clones the agent's repository BEFORE it
// starts the Claude Code CLI, and a private one cannot be cloned until the owner
// approves the agent's `github` connector grant — which ccd asks for itself.
//
// 🔴 THE FIRST TURN WAITS FOR IT, UNSTAMPED. Before this, the deliverer stamped the
// row kicked_off, sent the turn into ccd's `503 not_ready`, re-sent for three
// minutes and recorded a failed kickoff — so approving the grant afterwards left an
// agent that had never been told its task, and the owner had to re-send it by hand.
// Now internal/agentkickoff reads this state under its claim, BEFORE the stamp, and
// while it says the checkout is coming (cloning, or waiting for the grant) it sends
// nothing and stamps nothing; a later tick tries again. The stamp-before-send rule
// (a paid turn never runs twice) is untouched: the read is one more pre-stamp check.
const (
	// CheckoutUnknown: not read (no repository, the read failed, or a ccd that does
	// not report one).
	CheckoutUnknown = ""
	// CheckoutCloning: ccd has not finished its first clone attempt.
	CheckoutCloning = "cloning"
	// CheckoutAwaitingGrant: a clone failed and this agent's `github` connector
	// request is pending (ccd filed it, or it already was).
	CheckoutAwaitingGrant = "awaiting_grant"
	// CheckoutFailed: a clone failed for any other reason (the grant is approved
	// but git has not caught up, it was denied, the repository is missing …).
	// The first turn is sent as before: ccd answers not_ready with the reason, and
	// the deliverer's bounded re-send records it if it does not clear.
	CheckoutFailed = "failed"
	// CheckoutReady: the repository is checked out and the CLI starts.
	CheckoutReady = "ready"
)

// CheckoutStatus is ccd's `repo` object as muster reads it.
type CheckoutStatus struct {
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// CheckoutWaitTimeout bounds how long a first turn waits for a checkout that is
// coming (cloning, or waiting for the github grant). An owner may not see the
// request for hours; a day is long enough to come back to it and short enough
// that an agent nobody approves goes red rather than sitting owed for ever.
const CheckoutWaitTimeout = 24 * time.Hour

// KickoffAwaitingGrantReason / KickoffAwaitingCloneReason open agents.kickoff_error
// while a first turn is held for its checkout. They are written by
// internal/agentkickoff on a row that is NOT kicked off, so [KickoffFailed] (which
// requires kicked_off) never reads them as a failure; the deliverer clears them
// before it stamps. [KickoffAwaitingGrant] and [WaitedForCheckout] read them by
// prefix — one constant per state, so writer and readers cannot drift.
const (
	KickoffAwaitingGrantReason = "kickoff not sent yet: waiting for the owner to approve this agent's " +
		"github connector request, without which its repository cannot be checked out"
	KickoffAwaitingCloneReason = "kickoff not sent yet: the agent's repository is still being cloned"
)

// NeedsCheckout reports whether an agent's first turn depends on a repository
// checkout: a claude-code agent dispatched with a repository. A gateway agent, or
// a claude-code agent without one, never waits.
func NeedsCheckout(a Agent) bool {
	return ResolveKind(a.Kind) == KindClaudeCode && a.Repo != ""
}

// CheckoutGates reports whether the checkout decides this tick's first turn: the
// agent needs one, has not been kicked off, is not errored, and its instance is
// ready (the only state in which a first turn would otherwise be sent or the
// ready-but-never-kicked dwell bound applied). It is the ONE predicate the
// deliverer routes on and [DecideFirstTurn] gates on.
func CheckoutGates(a Agent, inst *provision.Instance) bool {
	return NeedsCheckout(a) && a.Status != StatusError && !a.KickedOff && inst != nil && inst.Ready
}

// KickoffAwaitingGrant reports a first turn held because the agent's github
// connector request is not approved yet: the card's "waiting for your github
// approval" line. Like [KickoffOwed] it is a separate boolean, never a status.
// An errored row answers false: the wait ended (see [DecideFirstTurn]).
func KickoffAwaitingGrant(a Agent) bool {
	return KickoffOwed(a) && a.Status != StatusError && strings.HasPrefix(a.KickoffError, KickoffAwaitingGrantReason)
}

// WaitedForCheckout reports that the deliverer recorded a checkout wait on this
// row and has not stamped it. The deliverer clears the marker before the stamp,
// or the stamped row would read as a failed kickoff ([KickoffFailed]) until the
// turn ends.
func WaitedForCheckout(a Agent) bool {
	return !a.KickedOff && (strings.HasPrefix(a.KickoffError, KickoffAwaitingGrantReason) ||
		strings.HasPrefix(a.KickoffError, KickoffAwaitingCloneReason))
}

// DecideFirstTurn is [DecideReconcile] for a first turn whose agent needs a
// checkout ([CheckoutGates]), given the checkout state ccd reported. For any other
// row it IS DecideReconcile's action, so agents without a repository and gateway
// agents are decided exactly as before.
//
// Rules, for a gated row (dwell = now − updated_at, the same clock as
// ProvisioningStuckTimeout):
//  1. The checkout is coming — cloning or awaiting_grant, or unreadable on a row
//     that already recorded a wait — so nothing is sent: [ActionWaitCheckout]
//     while dwell ≤ [CheckoutWaitTimeout], then [ActionErrorCheckoutWait].
//  2. The checkout is ready or failed on a row that waited: the dwell bound is
//     [CheckoutWaitTimeout], not ProvisioningStuckTimeout — a grant approved after
//     an hour must not find the row already past its 15 minutes and fail it in
//     the tick that could finally deliver it.
//  3. Otherwise (never waited; ready, failed or unreadable): DecideReconcile, as
//     before.
//
// 🔴 RULE 1's "UNREADABLE ON A ROW THAT WAITED" KEEPS A HELD TURN HELD. One failed
// /healthz read after hours of waiting must neither error the row (it is past 15
// minutes) nor send into a checkout that, last anyone saw, did not exist.
func DecideFirstTurn(a Agent, inst *provision.Instance, checkout string, now time.Time) ReconcileAction {
	action, _ := DecideReconcile(a, inst, now)
	if !CheckoutGates(a, inst) {
		return action
	}
	dwell := now.Sub(a.UpdatedAt)
	waited := WaitedForCheckout(a)
	coming := checkout == CheckoutCloning || checkout == CheckoutAwaitingGrant || (checkout == CheckoutUnknown && waited)
	switch {
	case coming && dwell > CheckoutWaitTimeout:
		return ActionErrorCheckoutWait
	case coming:
		return ActionWaitCheckout
	case waited && dwell <= CheckoutWaitTimeout:
		return ActionRetryKickoff
	}
	return action
}
