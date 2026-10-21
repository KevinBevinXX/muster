package hermeshost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"time"
)

// THE GATEWAY'S CONTROL SOCKET (docs/design/hermes.md, "The gateway viewer";
// Phase 4). At the pin, every Hermes CLI is a thin client of one gateway per
// home (`hermes gateway run --idle-exit`), which runs the turns. The gateway
// answers versioned JSON verbs on a local socket, one request line in and one
// response line out per connection (P:gateway/control_socket.py):
//
//   - `identify` names the gateway: its kind, the commit it booted from
//     (code_sha), the instance, its state, the loopback API origin its
//     WebSocket listens on, the profiles it serves and its capabilities;
//   - `session-ticket` mints a 30-second ticket to open that WebSocket as a
//     viewer (viewer.go).
//
// muster uses it for ONE thing: answering an approval the person answers in
// muster. It is never a send path (S1 refuted it) and never a status source.
//
// 🔴 THE PIN IS CHECKED ON EVERY ATTACH. A gateway that is not the pinned
// commit is refused before anything else is asked of it: muster supports one
// Hermes commit (Versions), and a gateway that outlived a `hermes update`
// keeps serving the code it booted from.
//
// 🔴 THE SOCKET IS AUTHENTICATED, NOT JUST ITS PATH: the path is checked
// (home.go, gatewaySocketPath), and then the process that answers the
// connection must run as this uid (SO_PEERCRED), as Hermes's own client
// requires — connect() follows a symlink swapped in after the checks.

// Pin is the Hermes commit muster supports (docs/design/hermes.md, Versions).
const Pin = "5fc490e2b34123212d73ef316086f939567a725a"

var (
	// errNoGateway: no gateway is running for the home (no socket, no
	// pointer).
	errNoGateway = errors.New("no Hermes gateway is running for this home")
	// errGatewayUnsafe: a control path failed its ownership or mode check.
	errGatewayUnsafe = errors.New("the Hermes gateway's control socket failed its safety check")
	// ErrNoGateway is errNoGateway, for a caller to tell "none running" from
	// a refusal.
	ErrNoGateway = errNoGateway
	// ErrGatewayUnsafe is errGatewayUnsafe.
	ErrGatewayUnsafe = errGatewayUnsafe
	// ErrGatewayNotPin: the gateway booted from another commit than Pin.
	ErrGatewayNotPin = errors.New("the Hermes gateway is not the pinned commit")
	// ErrGatewayNotReady: the gateway answered, but not as a ready owner of
	// this home muster can attach to (starting, draining, another profile,
	// no session authority).
	ErrGatewayNotReady = errors.New("the Hermes gateway is not ready for a viewer")
	// ErrGatewayProtocol: the gateway's answer is not what the pin answers.
	ErrGatewayProtocol = errors.New("the Hermes gateway answered something muster cannot read")
)

// controlTimeout bounds one control request, end to end (Hermes's own client
// uses 2 s for identify and 5 s for a ticket).
const controlTimeout = 5 * time.Second

// maxControlResponse bounds a control answer, as Hermes's own reader does.
const maxControlResponse = 512 << 10

// Gateway is what a gateway's identify says, checked.
type Gateway struct {
	// CodeSHA is the commit it booted from.
	CodeSHA string
	// InstanceID names this run of it; a ticket is bound to it.
	InstanceID string
	// ProfileID is the id it serves this home under.
	ProfileID string
	// APIOrigin is its loopback API, "http://127.0.0.1:<port>".
	APIOrigin string
	// PID is the gateway's process id, for a diagnostic.
	PID int
}

// identifyWire is the identify answer's fields muster reads. Nothing else is
// decoded.
type identifyWire struct {
	Kind            string          `json:"kind"`
	PID             int             `json:"pid"`
	CodeSHA         string          `json:"code_sha"`
	InstanceID      string          `json:"instance_id"`
	RuntimeProtocol json.RawMessage `json:"runtime_protocol"`
	AuthorityEpoch  json.RawMessage `json:"authority_epoch"`
	State           string          `json:"state"`
	APIOrigin       string          `json:"api_origin"`
	Capabilities    []string        `json:"capabilities"`
	ServedProfiles  []struct {
		ProfileID string `json:"profile_id"`
		Home      string `json:"home"`
	} `json:"served_profiles"`
}

// shaPattern is a commit as an error may quote it.
var shaPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// Identify asks this home's gateway who it is, and checks the answer: the
// pinned commit (ErrGatewayNotPin), a ready owner of THIS home with session
// authority and a loopback API (ErrGatewayNotReady, ErrGatewayProtocol), as
// Hermes's own client checks it (P:hermes_cli/gateway_runtime.py, _endpoint).
// ErrNoGateway when none is running.
func (h Home) Identify(ctx context.Context) (Gateway, error) {
	sock, resolved, err := h.gatewaySocketPath()
	if err != nil {
		return Gateway{}, err
	}
	var w identifyWire
	if err := controlRequest(ctx, sock, "identify", nil, &w); err != nil {
		return Gateway{}, err
	}
	return checkIdentify(w, resolved)
}

// checkIdentify is Identify's checks on a decoded answer for the home
// resolved.
func checkIdentify(w identifyWire, resolved string) (Gateway, error) {
	if w.Kind != "hermes-gateway" {
		return Gateway{}, fmt.Errorf("%w: it is not a hermes-gateway", ErrGatewayProtocol)
	}
	if w.CodeSHA != Pin {
		sha := "an unreadable commit"
		if shaPattern.MatchString(w.CodeSHA) {
			sha = w.CodeSHA
		}
		return Gateway{}, fmt.Errorf("%w: it runs %s, muster supports %s only", ErrGatewayNotPin, sha, Pin[:12])
	}
	if !jsonIntIs(w.RuntimeProtocol, func(n int64) bool { return n == 1 }) {
		return Gateway{}, fmt.Errorf("%w: runtime protocol is not 1", ErrGatewayProtocol)
	}
	if w.State != "ready" {
		return Gateway{}, fmt.Errorf("%w: its state is %q", ErrGatewayNotReady, boundedWord(w.State))
	}
	authority := false
	for _, c := range w.Capabilities {
		authority = authority || c == "session-authority-v1"
	}
	if !authority {
		return Gateway{}, fmt.Errorf("%w: it has no session authority", ErrGatewayNotReady)
	}
	if !jsonIntIs(w.AuthorityEpoch, func(n int64) bool { return n > 0 }) || w.InstanceID == "" || len(w.InstanceID) > 128 {
		return Gateway{}, fmt.Errorf("%w: no runtime identity", ErrGatewayProtocol)
	}
	var profile string
	matches := 0
	for _, p := range w.ServedProfiles {
		if p.Home == "" {
			continue
		}
		if home, err := filepath.EvalSymlinks(p.Home); err == nil && filepath.Clean(home) == resolved {
			matches++
			profile = p.ProfileID
		}
	}
	if matches != 1 || profile == "" || len(profile) > 4096 {
		return Gateway{}, fmt.Errorf("%w: it does not serve this home", ErrGatewayNotReady)
	}
	if err := loopbackOrigin(w.APIOrigin); err != nil {
		return Gateway{}, err
	}
	return Gateway{CodeSHA: w.CodeSHA, InstanceID: w.InstanceID, ProfileID: profile, APIOrigin: w.APIOrigin, PID: w.PID}, nil
}

// loopbackOrigin requires origin to be http(s)://<loopback IP>:<port> and
// nothing more (no user, path, query or fragment), as Hermes's client does.
func loopbackOrigin(origin string) error {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("%w: its API origin is not a bare loopback URL", ErrGatewayProtocol)
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil || !ip.IsLoopback() || u.Port() == "" {
		return fmt.Errorf("%w: its API origin is not a loopback address and port", ErrGatewayProtocol)
	}
	if p, err := strconv.Atoi(u.Port()); err != nil || p <= 0 || p > 65535 {
		return fmt.Errorf("%w: its API origin's port is not a port", ErrGatewayProtocol)
	}
	return nil
}

// jsonIntIs reports whether raw is a JSON integer (not a bool, not a float
// with a fraction) for which ok holds.
func jsonIntIs(raw json.RawMessage, ok func(int64) bool) bool {
	n, err := strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 64)
	return err == nil && ok(n)
}

// wordPattern is a value an error may quote: a short identifier.
var wordPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,40}$`)

// boundedWord is s when it is a short identifier, else "unreadable": no
// error quotes what a peer sent beyond that.
func boundedWord(s string) string {
	if wordPattern.MatchString(s) {
		return s
	}
	return "unreadable"
}

// ticketWire is a session-ticket answer.
type ticketWire struct {
	Ticket          string          `json:"ticket"`
	InstanceID      string          `json:"instance_id"`
	ProfileID       string          `json:"profile_id"`
	RuntimeProtocol json.RawMessage `json:"runtime_protocol"`
}

// ticket asks this home's gateway for a viewer ticket bound to g: the
// answer must name g's profile and instance, as Hermes's client requires
// (P:hermes_cli/gateway_client.py, _session_ticket).
//
// 🔴 THE TICKET IS A CREDENTIAL FOR 30 SECONDS: it opens the gateway's
// WebSocket. It is used once, at once, and never logged or put in an error.
func (h Home) ticket(ctx context.Context, g Gateway) (string, error) {
	sock, _, err := h.gatewaySocketPath()
	if err != nil {
		return "", err
	}
	var w ticketWire
	params := map[string]any{"profile_id": g.ProfileID, "instance_id": g.InstanceID, "purpose": "interactive"}
	if err := controlRequest(ctx, sock, "session-ticket", params, &w); err != nil {
		return "", err
	}
	if w.Ticket == "" || len(w.Ticket) > 4096 || w.InstanceID != g.InstanceID || w.ProfileID != g.ProfileID ||
		!jsonIntIs(w.RuntimeProtocol, func(n int64) bool { return n == 1 }) {
		return "", fmt.Errorf("%w: the ticket is not for this gateway", ErrGatewayProtocol)
	}
	for _, c := range w.Ticket {
		// It travels in a WebSocket subprotocol: a token, nothing a header
		// could not carry.
		if c <= ' ' || c > '~' || c == ',' {
			return "", fmt.Errorf("%w: the ticket is not a token", ErrGatewayProtocol)
		}
	}
	return w.Ticket, nil
}

// controlEnvelope is a control answer: {ok, protocol, id, result} or
// {ok:false, error}.
type controlEnvelope struct {
	OK       bool            `json:"ok"`
	Protocol json.RawMessage `json:"protocol"`
	ID       json.RawMessage `json:"id"`
	Result   json.RawMessage `json:"result"`
}

// controlRequest sends one verb to the control socket at sock and decodes
// the answer's result into out. The answering process must run as this uid.
func controlRequest(ctx context.Context, sock, verb string, params map[string]any, out any) error {
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w (its socket does not answer)", errNoGateway)
		}
		return fmt.Errorf("connect to the Hermes gateway: %w", err)
	}
	defer c.Close()
	if err := peerIsUs(c); err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	req := map[string]any{"protocol": 1, "verb": verb, "id": 1}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	if _, err := c.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("ask the Hermes gateway: %w", err)
	}
	var data []byte
	buf := make([]byte, 64<<10)
	for !bytes.Contains(data, []byte{'\n'}) {
		n, err := c.Read(buf)
		data = append(data, buf[:n]...)
		if len(data) > maxControlResponse {
			return fmt.Errorf("%w: its answer is over %d bytes", ErrGatewayProtocol, maxControlResponse)
		}
		if err != nil {
			if len(data) > 0 && bytes.Contains(data, []byte{'\n'}) {
				break
			}
			return fmt.Errorf("%w: no complete answer (%v)", ErrGatewayProtocol, errClass(err))
		}
	}
	line, _, _ := bytes.Cut(data, []byte{'\n'})
	var env controlEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return fmt.Errorf("%w: its answer is not JSON", ErrGatewayProtocol)
	}
	if !env.OK || !jsonIntIs(env.Protocol, func(n int64) bool { return n == 1 }) || !jsonIntIs(env.ID, func(n int64) bool { return n == 1 }) ||
		len(env.Result) == 0 || env.Result[0] != '{' {
		// The refusal's own text is not quoted: it can carry anything.
		return fmt.Errorf("%w: it refused %s", ErrGatewayProtocol, verb)
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("%w: its %s answer has the wrong shape", ErrGatewayProtocol, verb)
	}
	return nil
}

// errClass names an I/O error without quoting anything a peer sent.
func errClass(err error) string {
	var ne net.Error
	switch {
	case errors.As(err, &ne) && ne.Timeout():
		return "timed out"
	case errors.Is(err, os.ErrDeadlineExceeded):
		return "timed out"
	}
	return "connection closed"
}
