package agentgateway

import (
	"errors"
	"net"
	"net/http"

	"github.com/ZacxDev/muster/internal/agents"
)

// The reasons [Unsent] names. Each is a send failure that PROVES nothing was
// pasted into the agent's session.
const (
	UnsentBusy           = "busy"
	UnsentNotReady       = "not_ready"
	UnsentUnauthorized   = "unauthorized" // the runtime refused muster's own bearer
	UnsentRejected       = "rejected"     // the runtime refused the request's shape (a typed 400)
	UnsentNeverConnected = "never_connected"
)

// Unsent names a [Gateway.Send] failure that PROVES nothing was pasted into the
// agent's session, or "" for success and for every failure that does not prove it.
// Moved here from internal/memorelay so every deliverer of an UNSOLICITED turn
// (the memo relay, plugin messages) reads one rule.
//
//   - the runtime's own TYPED refusals — 409 `busy`, 503 `not_ready`, 401 `unauthorized`, and a
//     400 (`bad_request`, `invalid_input`) — are each answered before it pastes anything. Only
//     its typed body sets Type, so an untyped 409 or 503 from anything else is NOT read as
//     "nothing was sent";
//   - a dial error is a connection that was never opened, so no byte of the request left.
//
// ⚠ ONE CASE THIS CANNOT PROVE, AND IT ERRS TOWARDS "MAY HAVE BEEN SENT": a send cancelled
// while its connection was still being opened surfaces as a bare context error, with no dial
// error inside it. A caller records that `unconfirmed` and does not send it again; the
// alternative reading — a cancelled send was never sent — would repeat every turn a redeploy
// cut off mid-flight.
func Unsent(err error) string {
	if err == nil {
		return ""
	}
	var rt *agents.RuntimeError
	if errors.As(err, &rt) {
		switch {
		case rt.Status == http.StatusConflict && rt.Type == "busy":
			return UnsentBusy
		case rt.Status == http.StatusServiceUnavailable && rt.Type == "not_ready":
			return UnsentNotReady
		case rt.Status == http.StatusUnauthorized && rt.Type == "unauthorized":
			return UnsentUnauthorized
		case rt.Status == http.StatusBadRequest && (rt.Type == "bad_request" || rt.Type == "invalid_input"):
			return UnsentRejected
		}
		return ""
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return UnsentNeverConnected
	}
	return ""
}
