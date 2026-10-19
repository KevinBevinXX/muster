package agents

import (
	"fmt"
	"time"

	"github.com/ZacxDev/muster/internal/provision"
)

// ProvisioningStuckTimeout bounds how long an agent may dwell in provisioning
// before the reconciler gives up and errors it. It is safely greater than any
// reasonable readiness timeout so a slow-but-healthy instance has room to
// recover before the backstop fires.
const ProvisioningStuckTimeout = 15 * time.Minute

// MaxKickoffAttempts bounds how many times the kickoff message may be DELIVERED
// for one agent — the initial dispatch plus each recovery re-send after the
// receiving instance died.
//
// It exists because the recovery below is otherwise unbounded against an agent
// that dies DETERMINISTICALLY: the out-of-memory kill that motivated it (a
// real-repository clone plus install against a small memory limit) reproduces on
// every retry, so an uncapped re-send would re-run an expensive model kickoff
// every tick, forever, silently. Spending the budget is what converts "silently
// stuck" into "visibly failed".
const MaxKickoffAttempts = 3

// ReconcileAction is the decision the reconciler makes for a single agent given
// its live instance state.
type ReconcileAction int

const (
	ActionNone              ReconcileAction = iota // no change needed
	ActionPersistStatus                            // persist the computed status (it changed)
	ActionRetryKickoff                             // instance ready but kickoff never sent → retry
	ActionResendKickoff                            // kickoff was sent, but its recipient died → re-send
	ActionError                                    // stuck too long → mark error
	ActionErrorKickoffLost                         // re-send budget spent against a dying instance → mark error
	ActionWaitCheckout                             // first turn held: the repository checkout is coming (DecideFirstTurn)
	ActionErrorCheckoutWait                        // the checkout wait outlived CheckoutWaitTimeout → mark error
)

// String names the action.
//
// ⚠ IT EXISTS FOR TEST FAILURE MESSAGES, and that is a real reason rather than
// polish. Upstream every assertion on this type printed it with %d, so a
// regression read `action = 3, want 2` and told the reader nothing about which
// two behaviours had been swapped.
func (a ReconcileAction) String() string {
	switch a {
	case ActionNone:
		return "none"
	case ActionPersistStatus:
		return "persist-status"
	case ActionRetryKickoff:
		return "retry-kickoff"
	case ActionResendKickoff:
		return "resend-kickoff"
	case ActionError:
		return "error"
	case ActionErrorKickoffLost:
		return "error-kickoff-lost"
	case ActionWaitCheckout:
		return "wait-checkout"
	case ActionErrorCheckoutWait:
		return "error-checkout-wait"
	default:
		return fmt.Sprintf("ReconcileAction(%d)", int(a))
	}
}

// kickoffLost reports whether the instance that RECEIVED this agent's kickoff is
// gone, so the message it was carrying is gone with it.
//
// 🔴 THIS IS THE DISCRIMINATOR A SILENT-NON-START DEFECT TURNED ON. `KickedOff`
// alone cannot separate the two failure shapes it has to serve:
//
//   - a LIVE instance whose first turn errored (model timeout, long analysis) —
//     the agent still holds the message; re-sending burns tokens and duplicates
//     work. That bleed is what the "do not re-send a kicked-off agent" guard was
//     added to stop, and it must stay stopped.
//   - a DEAD recipient (out-of-memory kill, eviction, drain) — the replacement
//     comes up healthy with an EMPTY session and no memory of the task. Nothing
//     re-sends, nothing errors, and the task sits in progress behind a healthy,
//     ready instance.
//
// Two independent movements are checked because each is blind to the other's
// shape: an in-place restart keeps the instance ID and bumps the restart COUNT;
// a replacement changes the ID and resets the COUNT to zero. Both are exact
// integers/strings from the same read — deliberately not a timestamp comparison,
// which would put this service's clock and the backend's on the two sides of a
// `>` and turn a few seconds of skew into a phantom restart on every fast
// dispatch.
//
// An empty KickoffPod means the recipient was never recorded (a row predating
// the provenance columns, or a delivery whose instance lookup failed). Those are
// exempt: detection is disabled and behaviour falls back to exactly the
// pre-existing behaviour. That is the fail-safe direction — the alternative,
// treating "unknown" as "lost", re-kicks every legacy agent in the database on
// the first tick after deploy.
//
// ⚠ ONE CONSEQUENCE TO KNOW ABOUT, and it is intended: any DELIBERATE roll of a
// kicked-off agent — a model change, an image bump, a node drain
// — also changes the instance ID and therefore re-sends the kickoff. That roll
// has already killed whatever turn was running, so the choice is between
// re-delivering the task and silently doing nothing. This picks the defined one,
// and MaxKickoffAttempts caps it. If a future caller needs a roll that does NOT
// re-kick, the place to fix it is that caller: have it re-stamp KickoffPod to
// the new instance after the roll.
func kickoffLost(a Agent, inst *provision.Instance) bool {
	if inst == nil || !a.KickedOff || a.KickoffPod == "" {
		return false
	}
	return inst.InstanceID != a.KickoffPod || inst.Restarts > a.KickoffRestarts
}

// DecideReconcile decides what to do for one agent given its live instance (nil
// if none) and the current time. It is a pure function so the full decision
// matrix is testable without a live backend. When the action is
// ActionPersistStatus the returned newStatus is the status to persist;
// otherwise newStatus is "".
//
// Rules, in priority order:
//  1. An errored agent is sticky (stored-status-authoritative, matching
//     ComputeStatus).
//  2. A ready instance whose kickoff was never sent is recovered: retried while
//     young, errored once it has dwelt past ProvisioningStuckTimeout (the agent
//     runtime is likely dead).
//  3. A kickoff that WAS delivered, to an instance that no longer exists, is
//     re-sent while budget remains and errored once it is spent.
//  4. An agent that has dwelled in the stored `provisioning` status past
//     ProvisioningStuckTimeout without ever being kicked off is errored — keyed
//     on the stored status plus dwell, independent of whether the current
//     snapshot reads pending or absent (a dispatched-but-never-scheduled agent
//     collapses to "stopped" via ComputeStatus, so a computed-status guard
//     misses it). Otherwise the live status is recomputed; a changed status is
//     persisted, and an unchanged status is left alone — so no write happens and
//     the dwell clock keeps running.
func DecideReconcile(a Agent, inst *provision.Instance, now time.Time) (action ReconcileAction, newStatus string) {
	if a.Status == StatusError {
		return ActionNone, ""
	}

	if inst != nil && inst.Ready && !a.KickedOff {
		if now.Sub(a.UpdatedAt) > ProvisioningStuckTimeout {
			return ActionError, ""
		}
		return ActionRetryKickoff, ""
	}

	// The kickoff WAS delivered, but to an instance that no longer exists. Its
	// replacement is healthy and empty, so nothing below this point would ever
	// notice: ComputeStatus reads running, the status matches the stored one,
	// and the tick returns ActionNone — forever. Re-send while budget remains;
	// once it is spent, go red so a human sees it.
	//
	// inst.Ready is required: re-sending into a runtime that has not come back
	// up would burn an attempt on a probe that just times out. An instance stuck
	// not-ready is already covered — ComputeStatus drops it to provisioning and
	// the stuck/crash backstops take it from there.
	if inst != nil && inst.Ready && kickoffLost(a, inst) {
		if a.KickoffAttempts >= MaxKickoffAttempts {
			return ActionErrorKickoffLost, ""
		}
		return ActionResendKickoff, ""
	}

	// An agent that has dwelled in `provisioning` past the timeout without ever
	// reaching a ready, kicked-off state is stuck (never scheduled, never
	// pulled, or the runtime never came up) — error it regardless of whether the
	// current snapshot reads as pending or absent.
	if a.Status == StatusProvisioning && !a.KickedOff && now.Sub(a.UpdatedAt) > ProvisioningStuckTimeout {
		return ActionError, ""
	}
	cs := ComputeStatus(a, inst)
	if cs != a.Status {
		return ActionPersistStatus, cs
	}
	return ActionNone, ""
}

// PromoteToRunning reports whether an agent STORED as `provisioning` should now
// be stored as `running`: its live instance computes to running
// ([ComputeStatus] — present, phase running, ready, not crash-looping). It is the
// decision internal/agentstatus acts on; nothing else writes this transition.
//
// 🔴 IT DOES NOT WAIT FOR THE FIRST TURN. An agent whose kickoff is still owed
// is promoted too, because that is what every view already shows for it — a
// `running` dot plus the separate "kickoff owed" badge ([KickoffOwed]); a stored
// status that kept saying `provisioning` for the same agent was the defect. The
// owed turn's dwell bound is unaffected: DecideReconcile's ready-but-never-kicked
// branch keys on updated_at, which the promotion moves once, by one tick.
//
// Only `provisioning` is promoted. `pending` is a row that was never dispatched,
// `stopped` was put there by a Stop and `error` is sticky; none of them is this
// function's to overrule.
func PromoteToRunning(a Agent, inst *provision.Instance) bool {
	return a.Status == StatusProvisioning && inst != nil && ComputeStatus(a, inst) == StatusRunning
}

// DescribeKickoffLoss renders WHY the recipient is judged gone, in the terms an
// operator can act on. The backend's last-termination reason is the load-bearing
// part: "OOMKilled" turns "the agent never started" into "the agent's memory
// limit is too small for this repository", which is a capacity finding rather
// than a mystery. Without it a restart is just a number.
func DescribeKickoffLoss(a Agent, inst *provision.Instance) string {
	if inst == nil {
		return "instance gone"
	}
	what := fmt.Sprintf("instance restarted %d→%d", a.KickoffRestarts, inst.Restarts)
	if inst.InstanceID != a.KickoffPod {
		what = fmt.Sprintf("instance replaced (%s → %s)", a.KickoffPod, inst.InstanceID)
	}
	if inst.RestartReason != "" {
		what += ", last termination " + inst.RestartReason
	}
	return what
}

// KickoffErrorSuffix appends the recorded send failure to the terminal error
// when one exists, so the red card carries the primary evidence ("unexpected
// EOF") instead of making someone go find the server log before it rolls.
func KickoffErrorSuffix(a Agent) string {
	if a.KickoffError == "" {
		return ""
	}
	return "; last send error: " + a.KickoffError
}

// ---------------------------------------------------------------------------
// 🔴 THE RECONCILE LOOP ITSELF WAS NOT CARVED WITH THIS DECISION TABLE, AND
// THIS IS THE RECORD OF IT.
//
// Upstream, everything above was private and had exactly one caller: a
// `reconcileTick` method hanging off a 1,201-line `Provisioner` that owned a
// Helm client, a Kubernetes clientset, an embedded chart, an agent-gateway HTTP
// client and the kickoff goroutines. Carrying that across would have meant
// vendoring a second provisioning implementation beside internal/provision —
// which already renders its own objects, owns its own objects by label, and
// declares its capabilities — so the loop's BODY stayed behind and its
// DECISION TABLE came over, exported.
//
// ✅ THE FIRST-TURN HALF OF THIS TABLE HAS A DRIVER: internal/agentkickoff. Each
// tick it reads the stored agents (Store.List), lists and indexes the live
// instances (InstanceIndex), calls DecideReconcile for every row this deployment
// owns that holds a pending note and was NEVER kicked off, and acts:
// ActionRetryKickoff delivers the note through the gateway, ActionError fails the
// agent with KickoffErrorSuffix appended.
//
// 🔴 SINGLE-FLIGHT IS THE PER-AGENT CLAIM (Store.ClaimKickoff), NOT A LEADER LEASE.
// This footer used to say "This repository has db.LeaderGate for exactly that".
// It does not, and never did: the only LeaderGate is the CONSUMER-side interface
// in internal/api/server.go, nothing implements it, and internal/db has no lease.
// None is needed here: the claim is a conditional UPDATE on the agent's own row,
// and the winner re-reads the row under it, so two replicas ticking against one
// database cannot both pay one turn.
//
// ✅ ActionWaitCheckout AND ActionErrorCheckoutWait ARE DecideFirstTurn's (a
// first turn waiting on a claude-code agent's repository checkout), and
// internal/agentkickoff drives both, under the per-agent claim.
//
// ✅ ActionPersistStatus'S ONE MISSING TRANSITION NOW HAS A DRIVER:
// internal/agentstatus promotes provisioning → running (PromoteToRunning). It was
// listed below as driverless, and the cost was measured: stored statuses stuck at
// `provisioning` for hours behind ready instances, on every machine read. The
// other recomputed transitions are still not persisted (views refine them live).
//
// ⚠ STILL WITHOUT A DRIVER, AND NAMED SO IT IS NOT AN OBJECT NOBODY CAN CLOSE:
// everything this table decides for a KICKED-OFF row — ActionResendKickoff,
// ActionErrorKickoffLost (and so DescribeKickoffLoss).
// The deliverer excludes kicked-off rows by design: re-sending a task that was
// already handed over is a costlier decision than paying a first turn nobody has
// paid. So agents whose recipient dies after their first turn do NOT self-heal
// here. CLOSING CONDITION: a pull request that either drives those actions (behind
// the same per-agent claim) or records the decision to drop recovery. WHO CHECKS
// IT: the reviewer of that pull request, against this comment and against a grep
// for ActionResendKickoff returning only this file and tests.
// ---------------------------------------------------------------------------
