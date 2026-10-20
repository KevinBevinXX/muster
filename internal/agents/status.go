package agents

import (
	"strings"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/provision"
)

// ComputeStatus reconciles the STORED agent status with the LIVE instance state
// to produce the status shown on the card. The stored status is authoritative
// for error and for the "provisioned but never started" (stopped) case; the live
// phase refines running/provisioning.
//
// 🔴 IT TAKES A provision.Instance, NOT A POD. Upstream this read a `PodInfo`
// built from the Kubernetes API, and switched on that API's own phase spellings
// ("Running", "Pending", "Failed") as bare strings. Both are now the
// provisioner contract's vocabulary, which is what lets this function answer for
// an agent running under any driver — and the [provision.Phase] constants are
// typed, so a misspelling is a compile error rather than a branch that silently
// falls through to the default.
//
// ⚠ inst.Reason IS STILL A DRIVER-DEFINED STRING and the three values matched
// below are the Kubernetes driver's spellings. That is deliberate rather than an
// oversight: the contract does not enumerate reasons, and a driver that reports
// none simply never takes this branch — it falls through to the phase, which
// every driver does set. See [provision.Instance].Reason.
func ComputeStatus(a Agent, inst *provision.Instance) string {
	if a.Status == StatusError {
		return StatusError
	}
	if inst == nil {
		// No live instance. If we provisioned it (it has a group), it is
		// stopped; otherwise it is still pending.
		if a.Status == StatusStopped || a.KickedOff || a.Namespace != "" {
			return StatusStopped
		}
		return a.Status
	}
	if inst.Reason == "CrashLoopBackOff" || inst.Reason == "ImagePullBackOff" || inst.Reason == "ErrImagePull" {
		return StatusError
	}
	switch inst.Phase {
	case provision.PhaseRunning:
		if inst.Ready {
			return StatusRunning
		}
		return StatusProvisioning
	case provision.PhasePending:
		return StatusProvisioning
	case provision.PhaseFailed:
		return StatusError
	case provision.PhaseStopped:
		// 🔴 THIS CASE HAS NO UPSTREAM COUNTERPART AND IS NOT COSMETIC. No
		// Kubernetes pod phase means "deliberately scaled to zero" — upstream a
		// stopped agent simply had no pod, so it reached the inst == nil branch
		// above and answered `stopped`. The provisioner contract reports a
		// scaled-to-zero instance as a PRESENT instance in PhaseStopped
		// ([provision.Instance] is synthesised from the deployment, not from a
		// pod), so without this branch a stopped agent would fall to the
		// default and read `provisioning` forever — a stop permanently
		// misreported as a slow start.
		return StatusStopped
	default:
		// PhaseSucceeded and PhaseUnknown. Upstream's `default` answered
		// provisioning for Kubernetes' "Succeeded" too; the behaviour is
		// preserved rather than improved here, because deciding what a
		// completed agent should read is a product question and this carve is
		// not the place to answer it.
		return StatusProvisioning
	}
}

// KickoffOwed reports whether this agent still OWES its first turn: a kickoff
// message is stored on the row ([Agent.PendingNote]) and nothing has delivered it
// ([Agent.KickedOff] is false).
//
// 🔴 IT IS A SEPARATE SIGNAL AND NOT A SIXTH STATUS, DELIBERATELY. The five
// values above are the agents.status CHECK constraint, they key
// [DecideReconcile]'s decision table, and every consumer switches on them; a new
// member would reach all of that untested. This answers an ORTHOGONAL question —
// "was this agent ever told what to do" — which is why it is true ALONGSIDE every
// one of the five rather than instead of one. [ComputeStatus] does not read it and
// must not, and TWO guards divide that claim rather than one:
// TestTheStatusEnumDidNotGrowToCarryAnOwedKickoff pins the declared values and
// ComputeStatus' OUTPUT SET, while TestAnOwedKickoffSurvivesEveryStatusIncludingRunning
// pins that an owed agent on a live ready instance still computes `running`. The
// second is the one that catches a ComputeStatus which READS these columns and
// answers one of the five anyway — measured: a mutant returning StatusError for an
// owed agent leaves the output set unchanged, so the enum guard alone is blind to
// it.
//
// 🔴 WHY IT IS WORTH RENDERING AT ALL, WHICH IS A MEASURED DEFECT AND NOT A
// POLISH ITEM. An agent brought up by agentprovision.Adapter.Start with its note
// never delivered reaches a live, ready instance, and ComputeStatus refines that to
// [StatusRunning] — a branch that reads neither PendingNote nor KickoffError.
// BEFORE this function had a reader the card therefore read HEALTHY over an agent
// that was never told what to do, and the only evidence was machine-tier columns:
// agents.pending_note, which had no reader at all, and agents.kickoff_error, which
// reaches one hook-token-gated JSON route. `running` + owed is the exact
// combination this exists to make visible, and it is the combination the guard
// asserts — a fixture that cannot produce it cannot see the bug.
//
// ⚠ THE PendingNote CONJUNCT IS NARROWER THAN internal/agentprovision'S OWN
// `!ag.KickedOff` TEST, AND THAT IS THE POINT RATHER THAN A DISAGREEMENT. Start
// records a non-delivery whenever the agent was never kicked off; this reports a
// first turn that is owed, and with no stored message there is no turn to deliver —
// an agent created with no note text has nothing outstanding, so flagging it would
// be a badge on every agent that was simply never dispatched with one. Both
// conjuncts are load-bearing and both are mutation-covered by
// TestKickoffOwedTruthTable.
//
// ⚠ IT SAYS NOTHING ABOUT WHY. A refused dispatch (status `error`, nothing
// provisioned) and a running pod whose note was never handed over both answer
// true, because both owe the same turn. The reason lives in
// agents.error_message / agents.kickoff_error.
func KickoffOwed(a Agent) bool { return a.PendingNote != "" && !a.KickedOff }

// KickoffFailed reports a first turn that was HANDED to a gateway and did not
// complete: the row is stamped [Agent.KickedOff] and a failure is recorded in
// [Agent.KickoffError]. In SQL, `kicked_off AND kickoff_error <> ”`.
//
// 🔴 IT EXISTS BECAUSE THE STAMP CLEARS [KickoffOwed]. internal/agentkickoff stamps
// kicked_off BEFORE the paid turn so a turn is never run twice, and does not retry a
// turn that fails after the stamp (an operator decision: at-most-once). So
// a failed, empty, timed-out or shutdown-cancelled first turn turns the "kickoff
// owed" badge OFF while nothing was delivered — and before this predicate the
// failure lived only in kickoff_error, which no page rendered. This is that state,
// as its own signal.
//
// It is the same SHAPE as [KickoffOwed] and for the same reasons: a separate
// boolean beside the status, never a sixth status value. The two are MUTUALLY
// EXCLUSIVE by construction (one requires kicked_off, the other its negation), so a
// card never carries both badges. TestKickoffFailedTruthTable covers every cell.
//
// ⚠ IT DOES NOT READ THE NOTE. An agent kicked off by an upstream writer with no
// stored note but a recorded send failure still failed its kickoff, and says so.
//
// ⚠ WHAT KEEPS IT FROM FIRING ON A SUCCESS: every production write of kickoff_error
// on a kicked-off row is a post-stamp FAILURE (agentkickoff.recordError after the
// stamp); agentprovision writes it only on a never-kicked-off row; and a successful
// turn's agents.Store.RecordKickoffDelivery clears it — or, when that write failed,
// the deliverer clears it itself.
func KickoffFailed(a Agent) bool { return a.KickedOff && a.KickoffError != "" }

// KickoffNeverConnectedReason opens agents.kickoff_error when a stamped first turn
// failed because the connection to the agent runtime could not be OPENED (a dial
// error: DNS, connection refused, unreachable). internal/agentkickoff writes it;
// [KickoffResendSafe] reads it. One constant, so the writer and the reader cannot
// drift apart.
//
// 🔴 IT IS ONE OF THE TWO POST-STAMP CAUSES THAT PROVE NOTHING WAS SENT (the
// other is [KickoffNotAcceptedReason]). Go's HTTP client
// reports a dial error only from opening a NEW connection, before any byte of the
// request is written on it, and it does not retry a POST whose bytes were written.
// So no turn ran and none is running. (agentgateway.Gateway.Send can reach a dial
// only after a /v1/responses request that the runtime answered 404, meaning it has
// no such endpoint; that request ran no turn either.)
const KickoffNeverConnectedReason = "kickoff turn NEVER REACHED the agent runtime: the " +
	"connection could not be opened, so the turn was not sent and nothing was paid. The row " +
	"was already marked delivered, so it is not retried automatically"

// KickoffEmptyReplyReason is written to agents.kickoff_error when the first turn
// came back with no text.
const KickoffEmptyReplyReason = "kickoff turn returned an EMPTY reply, so it is recorded as NOT " +
	"delivered: the gateway answered without error and with no text. The known cause is a " +
	"reasoning model behind the agent runtime's /v1/responses, which discards its own " +
	"successful retry for that model class, so the agent may have done the work anyway. Pin a " +
	"non-reasoning model for this agent, and check its logs or transcript before re-sending."

// KickoffResendSafe reports whether a failed kickoff's recorded cause PROVES no
// turn is running for it, so re-sending the task cannot pay for it twice. Only
// [KickoffNeverConnectedReason], [KickoffNotAcceptedReason] and
// [KickoffBusyReason] prove that.
//
// 🔴 EVERY OTHER CAUSE ANSWERS false, AN EMPTY REPLY INCLUDED. A shutdown or a
// timeout abandons a request the runtime already received and may still be running.
// A transport error after the request was written says nothing about the runtime's
// side. And an empty reply is not a finished turn either: [KickoffEmptyReplyReason]
// itself names the cause as a runtime that DISCARDS ITS OWN SUCCESSFUL RETRY, so the
// work may have been done, or may still be under way, behind the empty answer.
//
// ⚠ IT READS kickoff_error BY ITS PREFIX, which muster writes from the constant
// above. An unrecognised text answers false, the side that tells the operator to
// check before re-sending.
func KickoffResendSafe(a Agent) bool {
	return KickoffFailed(a) && (strings.HasPrefix(a.KickoffError, KickoffNeverConnectedReason) ||
		strings.HasPrefix(a.KickoffError, KickoffNotAcceptedReason) ||
		strings.HasPrefix(a.KickoffError, KickoffBusyReason))
}

// KickoffBusyReason is [KickoffNotAcceptedReason]'s sibling for a typed `409
// busy`: ccd's answer, before anything is pasted, when another turn holds the
// session or (code awaiting_approval) its pane shows a dialog a paste would
// answer. It proves nothing was sent, so a re-send is safe.
const KickoffBusyReason = "kickoff turn NOT ACCEPTED by the agent runtime: it answered " +
	"busy (another turn held the session, or a dialog was on its screen) for the whole delivery, so " +
	"nothing was sent. The row was already marked delivered, so it is not retried automatically"

// KickoffNotAcceptedReason opens agents.kickoff_error when the runtime was
// reached but kept refusing the first turn with a typed `not_ready` — ccd's answer
// before the Claude Code CLI is at its prompt, given before anything is pasted —
// until the delivery gave up (its bound, or this process shutting down). Like
// [KickoffNeverConnectedReason], it proves nothing was sent, so a re-send is safe.
const KickoffNotAcceptedReason = "kickoff turn NOT ACCEPTED by the agent runtime: it answered " +
	"not_ready (its session was not at the prompt) for the whole delivery, so nothing was sent. The " +
	"row was already marked delivered, so it is not retried automatically"

// KickoffFailureText is the error text a surface may show for a failed kickoff:
// [Agent.KickoffError] with the pending note scrubbed out ([ScrubNote]), or "" when
// [KickoffFailed] is false. It is the ONE reader both tiers use, so the scrub cannot
// be present on one and missing on the other.
func KickoffFailureText(a Agent) string {
	if !KickoffFailed(a) {
		return ""
	}
	return ScrubNote(a.KickoffError, a.PendingNote)
}

// NoteWithheld replaces any part of the pending note found inside text a surface
// emits. See [ScrubNote].
const NoteWithheld = "[kickoff note withheld]"

// noteEchoMinBytes is the shortest run of the note [ScrubNote] removes. A note
// shorter than this is removed whenever it appears whole.
const noteEchoMinBytes = 16

// ScrubNote returns s with every run of at least noteEchoMinBytes bytes (or the
// whole note, if it is shorter) that also occurs in note replaced by
// [NoteWithheld]. An empty note returns s unchanged.
//
// 🔴 WHY ERROR TEXT NEEDS THIS AT ALL: kickoff_error is muster-authored PREFIX plus,
// for a failed turn, the gateway's error — and agents' responses transport quotes up
// to 512 bytes of a non-200 runtime BODY into that error ("responses HTTP %d: %s").
// A runtime that echoes the request it rejected therefore puts the operator's note —
// instruction text, `json:"-"` everywhere — into a column this package now surfaces
// on a public-repo service. The guarantee is "no 16-byte run of the note leaves the
// process through kickoff text", which is what this enforces.
//
// 🔴 RUNS, NOT ONLY THE WHOLE NOTE, BECAUSE THE QUOTE IS TRUNCATED. A 512-byte snippet
// of a body echoing a 2 KB note contains a PREFIX of it, which strings.ReplaceAll on
// the whole note would never match. Pinned by TestScrubNoteRemovesATruncatedEcho.
//
// ⚠ ITS LIMITS, STATED RATHER THAN IMPLIED: a fragment shorter than 16 bytes
// survives (an echo re-escaped every few characters would leak in pieces that
// small), and a TRANSFORMED echo — case-folded, re-encoded — is not recognised.
// Matching works on whole runes, so the output stays valid UTF-8.
func ScrubNote(s, note string) string {
	if note == "" || s == "" {
		return s
	}
	floor := noteEchoMinBytes
	if len(note) < floor {
		floor = len(note)
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		// Longest run starting at i, extended a whole rune at a time, that the note
		// contains.
		n := 0
		for j := i; j < len(s); {
			_, size := utf8.DecodeRuneInString(s[j:])
			if !strings.Contains(note, s[i:j+size]) {
				break
			}
			j += size
			n = j - i
		}
		if n >= floor {
			b.WriteString(NoteWithheld)
			i += n
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		b.WriteString(s[i : i+size])
		i += size
	}
	return b.String()
}

// InstanceIndex maps an agent NAME to its live instance, for quick
// reconciliation against a stored agent list.
//
// 🔴 THE KEY IS Ref.Name, NOT THE GROUP. Upstream keyed this on the pod's
// namespace, because every agent had one and it matched Agent.Namespace. Group
// is the driver's own scoping unit and [provision.Instance] states it is EMPTY
// where the concept does not apply — so a group-keyed index collapses every
// instance of a non-namespaced driver onto the single key "", and the last one
// wins. Ref.Name is the contract's identity and is always set.
func InstanceIndex(insts []provision.Instance) map[string]*provision.Instance {
	idx := make(map[string]*provision.Instance, len(insts))
	for i := range insts {
		idx[insts[i].Ref.Name] = &insts[i]
	}
	return idx
}
