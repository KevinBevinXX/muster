package agents

import (
	"context"
	"net/http"
)

// A TURN'S ROUTE INSIDE THE POD: which runtime, and which model, one turn goes
// to when an agent's pod runs more than one (the chief: ccd and the gateway
// runtime beside it — session_runtime.go).
//
// 🔴 THE ADDRESS DOES NOT CHANGE, ONLY THESE HEADERS DO. Every turn still goes to
// the agent's one endpoint, ccd's port: ccd is the only thing the pod's network
// policy lets muster reach, and it hands a gateway-runtime turn to the runtime
// beside it on loopback. So a session's route is a property of the REQUEST, set
// by the one place that knows the session (internal/api's sessionRoute) and
// read by the two request builders here, rather than a parameter threaded
// through every turn function between them.
//
// The header names are a wire contract with cmd/ccd, which spells them itself;
// cmd/ccd's tests pin the two (TestTheLaneHeadersAreMustersHeaders,
// TestTheWorkdirHeaderIsMusters).
const (
	// HeaderRuntime names the runtime a turn is for; absent is ccd's own.
	// "gateway" is the only other value.
	HeaderRuntime = "X-Muster-Runtime"
	// HeaderCCSession is the session key of an EXPLICIT Claude Code session:
	// ccd runs it in a CLI of its own. Absent: ccd's original CLI, the one
	// every legacy session has always shared.
	HeaderCCSession = "X-Muster-CC-Session"
	// HeaderCCModel is the model that CLI is started with (`--model`).
	HeaderCCModel = "X-Muster-CC-Model"
	// HeaderCCWorkdir is the working directory that CLI is started in: a job's
	// own worktree ([JobWorktreeDir]). Absent: the agent's working directory,
	// as for every session before jobs had worktrees. ccd accepts only a
	// directory [IsJobWorktreeDir] admits, and only on a turn that also names
	// an explicit session (HeaderCCSession), whose CLI it then fixes in that
	// directory for the session's life.
	HeaderCCWorkdir = "X-Muster-CC-Workdir"
	// HeaderCCWorkdirUsed is ccd's ANSWER on an explicit session's turn: the
	// directory that session's CLI actually works in. A sha job's runner
	// compares it with the HeaderCCWorkdir it sent and refuses the turn's result
	// when it is missing or different — a ccd from before job worktrees ignores
	// HeaderCCWorkdir and would run the job in the agent's own checkout.
	HeaderCCWorkdirUsed = "X-Muster-CC-Workdir-Used"
	// HeaderGatewayModel is the gateway runtime's per-request model override.
	//
	// ⚠ ITS SPELLING IS THE RUNTIME'S, LIKE sessionKeyHeader's: a third party's
	// wire protocol, not this project's identity.
	HeaderGatewayModel = "X-Openclaw-Model"
)

// RuntimeGateway is HeaderRuntime's value for the gateway runtime.
const RuntimeGateway = "gateway"

// TurnRoute is what a turn carries about where in the pod it goes.
type TurnRoute struct {
	// Runtime is the session's runtime (SessionRuntime*); "" is the agent's own.
	Runtime string
	// Header is set on every request of the turn, after the transport's own.
	Header http.Header
	// OnResponse, when set, is handed the response headers of every request of
	// the turn that got an answer, whatever its status.
	OnResponse func(http.Header)
}

// observeTurnRoute hands resp's headers to the route's OnResponse, if any.
func observeTurnRoute(req *http.Request, resp *http.Response) {
	if r, ok := TurnRouteFrom(req.Context()); ok && r.OnResponse != nil && resp != nil {
		r.OnResponse(resp.Header.Clone())
	}
}

type turnRouteKey struct{}

// WithTurnRoute returns ctx carrying r for the turn run under it.
func WithTurnRoute(ctx context.Context, r TurnRoute) context.Context {
	return context.WithValue(ctx, turnRouteKey{}, r)
}

// TurnRouteFrom is the route ctx carries, if any.
func TurnRouteFrom(ctx context.Context) (TurnRoute, bool) {
	r, ok := ctx.Value(turnRouteKey{}).(TurnRoute)
	return r, ok
}

// applyTurnRoute sets the route's headers on req. Called by both request
// builders AFTER their own headers, and the route can set only its own names:
// a route is never a way to replace the bearer or the session key.
func applyTurnRoute(req *http.Request) {
	r, ok := TurnRouteFrom(req.Context())
	if !ok {
		return
	}
	for _, name := range []string{HeaderRuntime, HeaderCCSession, HeaderCCModel, HeaderCCWorkdir, HeaderGatewayModel} {
		if v := r.Header.Get(name); v != "" {
			req.Header.Set(name, v)
		}
	}
}
