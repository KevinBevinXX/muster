package agents

import "time"

// The agent KINDS: which runtime an agent row is provisioned as.
//
// 🔴 KindGateway IS EVERY AGENT THAT EXISTED BEFORE KINDS DID, AND IT IS THE
// DEFAULT. It is the operator-configured runtime image reached over the
// `hooks-sha256` gateway wire (MUSTER_AGENT_IMAGE_REPO and friends). Migration
// 0003 backfills every existing row to it, and [ResolveKind] maps an unset kind
// to it, so a deployment that never names a kind builds exactly the specs it
// built before. The image's own identifier is a denied token in this public
// repository, which is why the kind is spelled after the wire it speaks.
//
// KindClaudeCode is the Claude Code pod: the claude-code-agent image (cmd/ccd +
// the pinned CLI) authenticated by one `claude setup-token` drawn from muster's
// per-account pool (internal/ccpool). Its spec profile is in
// internal/agentspec/claudecode.go.
const (
	KindGateway    = "gateway"
	KindClaudeCode = "claude-code"
)

// Kinds is every kind this build knows, in the order a picker shows them. It is
// the same set the latest agents_kind_check admits (migration 0047); a test
// pins the two.
var Kinds = []string{KindGateway, KindClaudeCode}

// RunsPodSupervisor reports whether kind's image runs muster's OWN in-pod
// supervisor, ccd (claude-code), rather than the operator's gateway runtime.
// Such a kind gets a persistent /data volume, the repository checked out by
// the supervisor before the agent starts (and, so, the first turn held for
// it), a connected repository only, and the spec re-applied on every Start.
func RunsPodSupervisor(kind string) bool {
	return ResolveKind(kind) == KindClaudeCode
}

// ResolveKind maps the unset kind to [KindGateway] — the one place that default
// is spelled. A row read from the database always carries a kind; an Agent
// built in Go (every fake store, every test fixture) may not.
func ResolveKind(kind string) string {
	if kind == "" {
		return KindGateway
	}
	return kind
}

// ValidKind reports whether kind (after [ResolveKind]) is one this build knows.
func ValidKind(kind string) bool {
	k := ResolveKind(kind)
	for _, known := range Kinds {
		if k == known {
			return true
		}
	}
	return false
}

// KindLabel is the human name a picker and a card show.
func KindLabel(kind string) string {
	switch ResolveKind(kind) {
	case KindClaudeCode:
		return "Claude Code"
	case KindGateway:
		return "Standard"
	}
	return kind
}

// ClaudeCodeTurnTimeout bounds ONE request to a claude-code agent's ccd.
//
// 🔴 IT IS LONGER THAN ccd's OWN TURN BUDGET ON PURPOSE, AND THE ORDER IS THE
// DECISION. ccd ends a turn that has seen no Stop/StopFailure after
// CCD_TURN_TIMEOUT (default 30m, cmd/ccd/main.go) with a TYPED
// `504 turn_timeout` whose message says the turn is still running in the
// session. If muster's client gave up first — the gateway kind's 10m
// (agentgateway.DefaultTurnTimeout) — a long Claude Code turn would surface as a
// bare client timeout with no body, ccd would stay busy until the CLI's Stop,
// and the next message would get a 409 `busy` for a turn nobody is waiting for.
// Outlasting ccd by five minutes means the typed answer always arrives first.
//
// ⚠ A TURN THAT GENUINELY RUNS LONGER THAN ccd's BUDGET STILL ENDS AS A 504 TO
// THE CALLER; the work keeps going in the TUI and is visible in the transcript.
const ClaudeCodeTurnTimeout = 35 * time.Minute

// KindTurnTimeout is the per-request budget a kind's turns need, or ZERO for "use
// the gateway's configured client unchanged" — which is what the gateway kind
// gets, so its behaviour is byte-identical to before kinds existed.
func KindTurnTimeout(kind string) time.Duration {
	switch ResolveKind(kind) {
	case KindClaudeCode:
		return ClaudeCodeTurnTimeout
	}
	return 0
}
