package api

import (
	"context"
	"errors"
)

// ---------------------------------------------------------------------------
// THE APPROVAL GATE, AS SEEN FROM THIS SIDE OF THE CARVE.
//
// ⚠ ITS ONE CALLER NOW IS THE CLAUDE-CODE APPROVAL RELAY (approvals.go), through
// its own gate (UseApprovalGate). The agent_checkpoint tool this file was written
// for, and the router-wired gate it polled through, were removed: never invoked.
// The paragraphs below describe the gate's contract, which the relay relies on.
//
// An agent that needs approval has to ask a human. muster has no approval
// queue — the permission router owns it, and owning it is what that service IS
// — so asking is a network call now, where it used to be a store write in the
// same process.
//
// 🔴 EVERY EXIT FROM THIS FILE THAT IS NOT AN EXPLICIT APPROVAL IS A REFUSAL,
// AND THE DEFAULT SURVIVES EVERY NEW FAILURE MODE THE HOP INTRODUCES. That
// sentence was already true in-process, where the only ways to fail were "no
// store" and "timed out". Across a network it is also true for: no router
// configured, a DNS failure, a refused connection, a 401 because the credential
// is wrong, a 503 because the router has no queue, an unparseable body, and a
// state name this client does not recognise. All of them land on
// `approved:false` with a status the agent can read, because "I could not ask"
// and "they said yes" must never be the same value. A gate that fails open is
// not a gate; it is a delay.
//
// ⚠ WHAT THE HOP GENUINELY CHANGES, STATED RATHER THAN GLOSSED: in-process, a
// checkpoint could only be lost by the operator not answering. Now a network
// partition refuses checkpoints that a human would have approved, and an agent
// sees `status:"unavailable"` rather than waiting. That is the correct
// direction to fail, and it is a real behaviour change — a partition now stops
// agent work rather than delaying it.
// ---------------------------------------------------------------------------

// GateSpec describes the approval card an agent is asking a human to judge.
type GateSpec struct {
	Type    string
	Tool    string
	Command string
	Host    string
	Project string
	Cwd     string
	Session string
	Context []string
}

// GateDecision is the answer to one gate read.
type GateDecision struct {
	// State is one of GateStateDecided, GateStatePending, GateStateGone.
	State string
	// Response is the answer when State is decided ("approve" / "reject"): a
	// human's, or the router's auto-approval for a type it lets that decide.
	Response string
	// Comment is the note left with the decision, when there is one.
	Comment string
}

// The three states a gate read can report. They are the router's wire
// vocabulary, spelled here so a handler does not have to import the client.
const (
	// GateStateDecided: the card was answered, by a human or by the router's
	// auto-approval.
	GateStateDecided = "decided"
	// GateStatePending: the card is on the operator's screen, undecided.
	GateStatePending = "pending"
	// GateStateGone: no decision will ever arrive. A caller MUST stop polling.
	GateStateGone = "gone"
)

// GatePort is the approval queue muster asks through.
//
// 🔴 A nil GatePort MEANS "NOBODY CAN BE ASKED", WHICH MEANS "NOT APPROVED".
// It does not mean "proceed"; see this file's header.
type GatePort interface {
	// MintGate files an approval request and returns its id.
	MintGate(ctx context.Context, spec GateSpec) (string, error)
	// ReadGate reports the current state of a request this caller minted.
	ReadGate(ctx context.Context, id string) (GateDecision, error)
	// ClearGate removes a pending card. Best-effort.
	ClearGate(ctx context.Context, id string) error
}

// gateTypeAgentPermission is the request type a claude-code pod's permission
// prompt is filed under by the approval relay (approvals.go). The router lets
// its auto-approval decide this type: auto-approve-ALL approves it, and a
// per-project window approves it when the window's project is the card's own.
// (The relay used to file the human-only type "checkpoint", which the router
// exempts from every auto-approval path.)
//
// 🔴 IT IS A LITERAL HERE AND AN ALLOWLIST ENTRY ON THE ROUTER, AND THE
// DUPLICATION IS DELIBERATE. The router's gate mints only allowlisted types and
// refuses anything else; this side cannot import that allowlist without
// importing the service. A typo here — or a router older than the type — is
// therefore a 400 at the door naming the type, loud, rather than a silently
// mis-filed card.
const gateTypeAgentPermission = "agent-permission"

// errNoGate is the refusal when no approval queue is reachable.
var errNoGate = errors.New("this server has no approval queue, so no approval could be sought")
