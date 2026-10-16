// Package authz carries WHO a request (or a background loop) is acting as.
//
// A Principal is attached to a context by exactly three kinds of code:
//
//   - the require* authentication wrappers in internal/api, through
//     WithPrincipal, once the credential has been checked;
//   - the background loops, through System, which acts for every user; and
//   - the operator's own out-of-request writes (the boot-time GitHub connect,
//     the e2e seed), through Operator, which acts as user 1 and nobody else.
//
// 🔴 THE CONTEXT KEY IS UNEXPORTED, AND THAT IS THE COMPILE-LEVEL HALF OF THE
// GUARD. No package can plant a value that From will return by calling
// context.WithValue itself: the key type cannot be named outside this file, so
// the ONLY ways to put a Principal on a context are WithPrincipal, System and
// Operator.
// Which code may call those two is the test-level half, and it is a
// relationship over the source rather than a convention: see
// internal/api/principal_test.go (every authenticated route observes a
// Principal with the right Via) and ledger_test.go in this package (the
// type-checked set of code that reaches System, WithPrincipal, Operator and
// api's admit, and this package's exported names, are each a ledger).
//
// 🔴 THE DATABASE ENFORCES ON IT (accounts PR 3a). internal/db reads the
// principal on EVERY pool acquire and sets Postgres's `muster.uid` /
// `muster.scope` from it; row-level security (migration 0007) then shows a
// connection only its user's rows. A context with NO principal sees no rows
// and cannot insert one. Today every request principal names user 1 (the
// operator, admin) — see OperatorUserID — so single-user behaviour is
// unchanged.
package authz

import (
	"context"
	"fmt"
)

// Role is a user's role. The set is closed.
type Role string

const (
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

// Via names the door a principal came through: which credential admitted the
// request, or that no request is involved at all.
//
// It is an audit fact, not an authorization input. Two requests from the same
// user through different doors carry the same UserID and different Vias.
type Via string

const (
	// ViaSession is the human browser tier: requireSession (and
	// requireOperatorWrite, which wraps it).
	ViaSession Via = "session"
	// ViaHook is the shared machine hook token: requireHookToken,
	// requireArmedHookToken, and the hook branch of requireHookOrServiceToken.
	ViaHook Via = "hook"
	// ViaService is MUSTER_SERVICE_TOKEN plus an asserted X-Muster-Actor:
	// requireServiceToken and the service branch of requireHookOrServiceToken.
	ViaService Via = "service"
	// ViaAgent is an agent's own per-agent token on the agent self-service API
	// (requireAgentToken: /agent/task, /agent/muster).
	ViaAgent Via = "agent"
	// ViaConnect is an agent's own per-agent token on the connector surface
	// (requireConnectorAgent: /connect/, /agent/connectors, /mcp) — the routes
	// the dedicated connect listener serves.
	ViaConnect Via = "connect"
	// ViaRelay is an agent's own per-agent token on the claude-code approval
	// relay (requireRelayAgent: /agent/approvals, /agent/blocked).
	ViaRelay Via = "relay"
	// ViaToken is a user's own PERSONAL API token (`mst_…`, accounts PR 6),
	// presented on the machine tier (requireHookToken and its siblings, and
	// requireAPIToken). It acts as the token's OWNER, never as the machine
	// user. The spelling is the audit_log `via` value migration 0006 names.
	ViaToken Via = "api-token"
	// ViaRunner is a host's BROWSER-FLOW RUNNER token (`mfr_…`, internal/flows):
	// requireRunnerToken, on the /runner/flows/* routes only. It acts as the
	// token's OWNER, so row-level security confines the runner to that owner's
	// jobs; the token's host confines it further, to jobs addressed to it.
	ViaRunner Via = "flow-runner"
	// ViaPlugin is a PLUGIN token (`mpl_…`, internal/plugins): requirePluginToken,
	// on the /plugin/v1/* routes only. It acts as the token's OWNER, so row-level
	// security confines the plugin to that owner's plugin rows.
	ViaPlugin Via = "plugin"
	// ViaQueue is the SERVER-SIDE SEND QUEUE's dispatcher probing the window
	// and delivering a message its owner queued from a signed-in session
	// earlier (internal/sendqueue): the deferred end of that owner's own
	// request, acting as the OWNER only. One site mints it — internal/api's
	// queuedPrincipal, called only by the dispatcher — and it is a request door
	// in the sense that matters here: one user, never everybody.
	ViaQueue Via = "send-queue"
	// ViaSystem is a background loop acting for every user. Only System sets
	// it.
	ViaSystem Via = "system"
	// ViaOperator is the operator acting OUTSIDE any request: a one-shot write
	// the process makes on the operator's behalf (cmd/muster-server's boot-time
	// GitHub connect) or a fixture (e2e/seed). Only Operator sets it.
	ViaOperator Via = "operator"
)

// requestVias is the closed set WithPrincipal accepts. ViaSystem is
// deliberately absent: a request is never the system.
var requestVias = map[Via]bool{
	ViaSession: true, ViaHook: true, ViaService: true,
	ViaAgent: true, ViaConnect: true, ViaRelay: true, ViaToken: true, ViaRunner: true, ViaPlugin: true,
	ViaQueue: true,
}

// OperatorUserID is the user every principal names today: the operator, the
// one user muster has. It is the seed admin row the accounts schema creates
// (users.id = 1), and the owner every existing row is backfilled to.
const OperatorUserID int64 = 1

// Principal is who a context acts as.
type Principal struct {
	// UserID is the acting user. Zero only for the System principal.
	UserID int64
	Role   Role
	Via    Via
}

// All reports whether this principal acts across every user's rows — true
// only for System. internal/db maps it to the `muster.scope = 'all'` setting.
func (p Principal) All() bool { return p.Via == ViaSystem }

// String renders the principal for a log line.
func (p Principal) String() string {
	if p.All() {
		return "system"
	}
	return fmt.Sprintf("user:%d(%s) via %s", p.UserID, p.Role, p.Via)
}

// principalKey is the unexported context key. See the package doc.
type principalKey struct{}

// WithPrincipal returns ctx carrying p. It is for the require* wrappers in
// internal/api ONLY; the call sites are a ledger in that package's tests.
//
// 🔴 IT PANICS ON A MALFORMED PRINCIPAL rather than attaching it. A request
// principal with no user, an unknown role or a door outside the closed set is
// a programming error in a wrapper, and attaching it would hand PR 3's
// row-level security a value that silently selects nobody (or, for a forged
// ViaSystem, everybody). internal/api's recover middleware turns the panic into
// a 500 for that one request.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	if p.UserID <= 0 {
		panic(fmt.Sprintf("authz: request principal with no user (UserID %d)", p.UserID))
	}
	if p.Role != RoleAdmin && p.Role != RoleMember {
		panic(fmt.Sprintf("authz: request principal with unknown role %q", p.Role))
	}
	if !requestVias[p.Via] {
		panic(fmt.Sprintf("authz: request principal with door %q, which is not a request door", p.Via))
	}
	return context.WithValue(ctx, principalKey{}, p)
}

// System returns ctx carrying the system principal: a background loop acting
// across every user's rows. Only the background loops listed in this
// package's allowlist test may call it.
func System(ctx context.Context) context.Context {
	return context.WithValue(ctx, principalKey{}, Principal{Role: RoleAdmin, Via: ViaSystem})
}

// Operator returns ctx carrying the OPERATOR (user 1, admin) for work the
// process does on the operator's behalf with no request to take a principal
// from — today the boot-time GitHub connect and the e2e seed. It is NOT a
// scope-all principal: it sees user 1's rows only, exactly like the operator's
// browser session. Its call sites are a ledger (ledger_test.go).
func Operator(ctx context.Context) context.Context {
	return context.WithValue(ctx, principalKey{}, Principal{UserID: OperatorUserID, Role: RoleAdmin, Via: ViaOperator})
}

// From returns the principal ctx carries, and whether it carries one.
func From(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// MustFrom returns the principal ctx carries, panicking if there is none.
// Use it where reaching the code without one is a wiring bug.
func MustFrom(ctx context.Context) Principal {
	p, ok := From(ctx)
	if !ok {
		panic("authz: no principal on this context (a route reached a handler without passing a require* wrapper, or a loop did not call authz.System)")
	}
	return p
}
