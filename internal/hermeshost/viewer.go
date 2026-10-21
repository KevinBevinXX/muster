package hermeshost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE GATEWAY VIEWER (docs/design/hermes.md, "The gateway viewer", "Questions
// and approvals"; Phase 4): a second client of the gateway, attached to ONE
// session, to answer an approval the person answers in muster.
//
//	identify (pin) → session-ticket → WebSocket <api_origin>/api/ws
//	(subprotocols hermes-gateway-v1, hermes-gateway-ticket.<ticket>)
//	→ session.resume → its pending approvals ("prompts")
//	→ approval.respond {session_id, execution_generation, prompt_id, choice}
//
// Measured against the pin (a scratch home, the stub model): a viewer's
// approval.respond is authoritative — the command did not run, the turn
// finished — while the CLI's modal STAYS DRAWN until a key is pressed there
// (S1, and again in Phase 4).
//
// 🔴 HYGIENE IS STRUCTURAL: A FRAME IS DECODED ONLY INTO THE FIELDS BELOW.
// The gateway streams the whole session to a viewer: session.resume carries
// every message, pending[].text (the prompts a person typed) and session.info
// (system_prompt, credential_hint); events carry every delta and tool result;
// and it sends `sudo`, `secret` and `clarify` server requests. None of that is
// decoded, kept, logged, quoted in an error or forwarded: an approval's
// prompt id, generation, command, description, choices and timeout are the
// only values that leave this file, and a server request is dropped on
// receipt, unanswered (TestTheViewerKeepsOnlyTheApprovalFields holds the
// shape; TestTheViewerDropsEverySecretBearingFrame feeds every such frame
// carrying a canary).
//
// 🔴 IT ANSWERS NOTHING BY ITSELF. Respond is called only for an answer a
// person gave in muster, with "once" or "deny" (mirror.HermesApprovalChoices); a server
// request (sudo, secret, clarify) is never answered.

// The bounds of an approval as the viewer keeps it.
const (
	// MaxApprovalCommand bounds a command, in runes, before the wire's own
	// caps.
	MaxApprovalCommand = 2000
	// MaxApprovalDescription bounds a description, in runes.
	MaxApprovalDescription = 500
)

// ApprovalPrompt is one pending approval, as the gateway names it.
type ApprovalPrompt struct {
	// PromptID is the gateway's id for it (32 hex at the pin).
	PromptID string
	// Generation is the session's execution generation it was asked in.
	Generation int64
	// Command is the command waiting to run; Description why Hermes asks.
	Command     string
	Description string
	// Choices are what the gateway offers (once, deny, session, always), in
	// its order.
	Choices []string
	// Unshown: the approval carries content muster does not render (an
	// `edit` diff — gateway/session_local_editor.py — or any field beyond
	// these): never offered "once" from muster.
	Unshown bool
}

// Offers reports whether p offers choice.
func (p ApprovalPrompt) Offers(choice string) bool {
	for _, c := range p.Choices {
		if c == choice {
			return true
		}
	}
	return false
}

// The kinds of ViewerEvent.
const (
	// EventApprovalRequest: a new approval (Prompt).
	EventApprovalRequest = "approval.request"
	// EventApprovalSettled: the approval PromptID was answered.
	EventApprovalSettled = "approval.settled"
)

// ViewerEvent is a change to what the session waits on.
type ViewerEvent struct {
	Kind     string
	Prompt   ApprovalPrompt
	PromptID string
}

// promptWire is an approval as the gateway sends it: the only fields read.
type promptWire struct {
	Kind        string   `json:"kind"`
	PromptID    string   `json:"prompt_id"`
	Generation  *int64   `json:"execution_generation"`
	Command     string   `json:"command"`
	Description string   `json:"description"`
	Choices     []string `json:"choices"`
	// extra: the prompt carries a field beyond these and timeout_s (an
	// `edit` diff): muster cannot show it whole.
	extra bool
}

// shownPromptKeys are the approval fields muster renders (or needs to
// answer); timeout_s is read by nobody. Any other key — `edit` at the pin —
// is content the card would not show, so the prompt is Unshown.
var shownPromptKeys = map[string]bool{"kind": true, "prompt_id": true, "execution_generation": true, "command": true,
	"description": true, "choices": true, "timeout_s": true}

// UnmarshalJSON decodes an approval's own fields and notes whether it
// carries any other (its VALUE is never kept).
func (w *promptWire) UnmarshalJSON(b []byte) error {
	type plain promptWire
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		return err
	}
	for k := range keys {
		if !shownPromptKeys[k] {
			p.extra = true
		}
	}
	*w = promptWire(p)
	return nil
}

// knownChoices are the choices the pin offers; any other is dropped.
var knownChoices = map[string]bool{"once": true, "deny": true, "session": true, "always": true}

// prompt is w checked and bounded; ok false when it is not an approval this
// viewer can name (no id, no generation, no command).
func (w promptWire) prompt(fallbackGen int64) (ApprovalPrompt, bool) {
	if (w.Kind != "" && w.Kind != "approval") || !validPromptID(w.PromptID) || w.Command == "" {
		return ApprovalPrompt{}, false
	}
	gen := fallbackGen
	if w.Generation != nil {
		gen = *w.Generation
	}
	if gen <= 0 {
		return ApprovalPrompt{}, false
	}
	p := ApprovalPrompt{PromptID: w.PromptID, Generation: gen, Command: capRunes(w.Command, MaxApprovalCommand),
		Description: capRunes(w.Description, MaxApprovalDescription), Unshown: w.extra}
	for _, c := range w.Choices {
		if knownChoices[c] && !p.Offers(c) {
			p.Choices = append(p.Choices, c)
		}
	}
	return p, true
}

// capRunes is s cut to at most n runes (invalid UTF-8 dropped).
func capRunes(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// The viewer's bounds.
const (
	// viewerReadLimit bounds one frame: session.resume carries the whole
	// conversation. Hermes's own client allows 8 MiB.
	viewerReadLimit = 8 << 20
	// viewerRPCTimeout bounds one request's answer.
	viewerRPCTimeout = 30 * time.Second
	// viewerEvents is the event buffer; a full one drops the viewer (the
	// caller re-attaches), never an event silently.
	viewerEvents = 64
)

// gatewayWSProtocol is the gateway's WebSocket subprotocol at the pin
// (P:gateway/session_contract.py, CANONICAL_GATEWAY_PROTOCOL); the ticket
// rides as a second one.
const (
	gatewayWSProtocol     = "hermes-gateway-v1"
	gatewayTicketProtocol = "hermes-gateway-ticket."
)

// Viewer is one attachment to a gateway, for one session.
type Viewer struct {
	ws *websocket.Conn
	// Session is the session the gateway resumed (its logical id), which
	// every answer names.
	Session string

	mu      sync.Mutex
	next    int64
	waiting map[int64]chan rpcAnswer
	closed  bool

	events chan ViewerEvent
	done   chan struct{}
	cancel context.CancelFunc
}

type rpcAnswer struct {
	result json.RawMessage
	reason string // a refusal's bounded reason; "" with result
	err    error
}

// frameWire is a frame's envelope: the only fields read off a frame.
type frameWire struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
		Data    struct {
			Reason string `json:"reason"`
		} `json:"data"`
	} `json:"error"`
	Params json.RawMessage `json:"params"`
}

// eventWire is an event's params: its kind, session and (raw) payload.
type eventWire struct {
	Type       string          `json:"type"`
	SessionID  string          `json:"session_id"`
	Generation *int64          `json:"execution_generation"`
	Payload    json.RawMessage `json:"payload"`
}

// resumeWire is session.resume's answer: the only fields read.
type resumeWire struct {
	SessionID  string       `json:"session_id"`
	Generation int64        `json:"execution_generation"`
	Prompts    []promptWire `json:"prompts"`
}

// Attach attaches a viewer to this home's gateway for session and returns it
// with the session's pending approvals. Every refusal names why: no gateway
// (ErrNoGateway), not the pin (ErrGatewayNotPin), a check that failed
// (ErrGatewayUnsafe), an answer muster cannot read (ErrGatewayProtocol).
func (h Home) Attach(ctx context.Context, session string) (*Viewer, []ApprovalPrompt, error) {
	if !mirror.ValidAgentSession(session) {
		return nil, nil, fmt.Errorf("%w: not a Hermes session id", ErrGatewayProtocol)
	}
	g, err := h.Identify(ctx)
	if err != nil {
		return nil, nil, err
	}
	ticket, err := h.ticket(ctx, g)
	if err != nil {
		return nil, nil, err
	}
	u := strings.Replace(strings.Replace(g.APIOrigin, "https:", "wss:", 1), "http:", "ws:", 1) + "/api/ws"
	dctx, dcancel := context.WithTimeout(ctx, 10*time.Second)
	defer dcancel()
	// 🔴 NO PROXY: the gateway is on loopback; a proxy from the environment
	// would see the ticket (and cannot reach 127.0.0.1 anyway).
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	ws, _, err := websocket.Dial(dctx, u, &websocket.DialOptions{HTTPClient: client,
		Subprotocols: []string{gatewayWSProtocol, gatewayTicketProtocol + ticket}})
	if err != nil {
		return nil, nil, fmt.Errorf("%w: its WebSocket refused the viewer (%s)", ErrGatewayProtocol, errClass(err))
	}
	if ws.Subprotocol() != gatewayWSProtocol {
		ws.Close(websocket.StatusPolicyViolation, "")
		return nil, nil, fmt.Errorf("%w: its WebSocket speaks another protocol", ErrGatewayProtocol)
	}
	ws.SetReadLimit(viewerReadLimit)
	rctx, cancel := context.WithCancel(context.Background())
	v := &Viewer{ws: ws, waiting: map[int64]chan rpcAnswer{}, events: make(chan ViewerEvent, viewerEvents), done: make(chan struct{}), cancel: cancel}
	go v.read(rctx)
	raw, err := v.rpc(ctx, "session.resume", map[string]any{"session_id": session})
	if err != nil {
		v.Close()
		return nil, nil, err
	}
	var r resumeWire
	if err := json.Unmarshal(raw, &r); err != nil || !mirror.ValidAgentSession(r.SessionID) {
		v.Close()
		return nil, nil, fmt.Errorf("%w: its session.resume answer has the wrong shape", ErrGatewayProtocol)
	}
	v.mu.Lock()
	v.Session = r.SessionID
	v.mu.Unlock()
	var prompts []ApprovalPrompt
	for _, w := range r.Prompts {
		if p, ok := w.prompt(r.Generation); ok {
			prompts = append(prompts, p)
		}
	}
	return v, prompts, nil
}

// Events are the session's approval changes while attached; closed when the
// viewer is done.
func (v *Viewer) Events() <-chan ViewerEvent { return v.events }

// Done is closed when the viewer has ended (Close, or the gateway hung up).
func (v *Viewer) Done() <-chan struct{} { return v.done }

// Close ends the viewer. Closing a viewer never stops its session (the
// gateway's own rule: "closing a viewer never stops its authority").
func (v *Viewer) Close() {
	v.cancel()
	v.ws.Close(websocket.StatusNormalClosure, "")
	<-v.done
}

var (
	// ErrApprovalRefused: the gateway did not take the answer (already
	// settled, timed out, another generation). Nothing was answered.
	ErrApprovalRefused = errors.New("Hermes did not take the answer")
	// ErrViewerNotSent: the request never reached the gateway (the viewer had
	// ended, or the write failed). Nothing was answered.
	ErrViewerNotSent = errors.New("the request did not reach Hermes's gateway")
	// ErrViewerNoAnswer: the request was sent and no answer came (a timeout,
	// or the gateway hung up): its outcome is unknown.
	ErrViewerNoAnswer = errors.New("Hermes's gateway did not answer")
)

// Respond answers the approval p with choice — "once" or "deny" only
// (mirror.HermesApprovalChoices), and only one p offers. nil means the gateway resolved
// it with this answer.
func (v *Viewer) Respond(ctx context.Context, p ApprovalPrompt, choice string) error {
	allowed := false
	for _, c := range mirror.HermesApprovalChoices {
		allowed = allowed || c == choice
	}
	if !allowed || !p.Offers(choice) {
		return fmt.Errorf("muster answers a Hermes approval with once or deny only, and only with one it offers (not %q)", boundedWord(choice))
	}
	if !validPromptID(p.PromptID) || p.Generation <= 0 {
		return fmt.Errorf("%w: not an approval", ErrApprovalRefused)
	}
	v.mu.Lock()
	session := v.Session
	v.mu.Unlock()
	raw, err := v.rpc(ctx, "approval.respond", map[string]any{"session_id": session, "execution_generation": p.Generation,
		"prompt_id": p.PromptID, "choice": choice})
	if err != nil {
		return err
	}
	var r struct {
		Status   string `json:"status"`
		PromptID string `json:"prompt_id"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Status != "resolved" || r.PromptID != p.PromptID {
		return fmt.Errorf("%w: it answered %q", ErrApprovalRefused, boundedWord(r.Status))
	}
	return nil
}

// rpc sends one request and waits for its answer: the result, or a refusal
// (ErrApprovalRefused with the gateway's bounded reason code).
func (v *Viewer) rpc(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return nil, fmt.Errorf("%w: %w: the viewer has ended", ErrGatewayProtocol, ErrViewerNotSent)
	}
	v.next++
	id := v.next
	ch := make(chan rpcAnswer, 1)
	v.waiting[id] = ch
	v.mu.Unlock()
	defer func() {
		v.mu.Lock()
		delete(v.waiting, id)
		v.mu.Unlock()
	}()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	ctx, cancel := context.WithTimeout(ctx, viewerRPCTimeout)
	defer cancel()
	if err := v.ws.Write(ctx, websocket.MessageText, b); err != nil {
		return nil, fmt.Errorf("%w: %w: %s (%s)", ErrGatewayProtocol, ErrViewerNotSent, method, errClass(err))
	}
	select {
	case a := <-ch:
		if a.err != nil {
			return nil, a.err
		}
		if a.result == nil {
			return nil, fmt.Errorf("%w: %s refused (%s)", ErrApprovalRefused, method, a.reason)
		}
		return a.result, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %w: to %s", ErrGatewayProtocol, ErrViewerNoAnswer, method)
	case <-v.done:
		return nil, fmt.Errorf("%w: %w: it hung up", ErrGatewayProtocol, ErrViewerNoAnswer)
	}
}

// read is the viewer's one reader: answers to its own requests, and the
// approval events of its session. Everything else is dropped unread.
func (v *Viewer) read(ctx context.Context) {
	defer func() {
		v.mu.Lock()
		v.closed = true
		v.mu.Unlock()
		close(v.events)
		close(v.done)
	}()
	for {
		typ, data, err := v.ws.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var f frameWire
		if json.Unmarshal(data, &f) != nil {
			continue
		}
		switch {
		case f.Method == "" && len(f.ID) > 0:
			v.answer(f)
		case f.Method == "event":
			if !v.event(f.Params) {
				return
			}
		default:
			// A server request (sudo, secret, clarify, …): never answered,
			// never kept, never logged.
		}
	}
}

// answer hands a response frame to the request waiting on its id.
func (v *Viewer) answer(f frameWire) {
	var id int64
	if json.Unmarshal(f.ID, &id) != nil {
		return
	}
	v.mu.Lock()
	ch := v.waiting[id]
	v.mu.Unlock()
	if ch == nil {
		return
	}
	var a rpcAnswer
	switch {
	case f.Error != nil:
		reason := boundedWord(f.Error.Message)
		if reason == "unreadable" {
			reason = boundedWord(f.Error.Data.Reason)
		}
		a = rpcAnswer{reason: reason}
	case len(f.Result) > 0 && f.Result[0] == '{':
		a = rpcAnswer{result: f.Result}
	default:
		a = rpcAnswer{err: fmt.Errorf("%w: an answer with no result", ErrGatewayProtocol)}
	}
	// 🔴 NEVER BLOCK THE READER: the first answer to an id is the one kept;
	// a gateway that answers it again is dropped, so the reader always reads
	// on and Close always returns.
	select {
	case ch <- a:
	default:
	}
}

// event turns one of the session's approval events into a ViewerEvent;
// false when the buffer is full (the viewer ends, and the caller re-attaches
// rather than miss a settle).
func (v *Viewer) event(params json.RawMessage) bool {
	var e eventWire
	if json.Unmarshal(params, &e) != nil {
		return true
	}
	v.mu.Lock()
	session := v.Session
	v.mu.Unlock()
	if session == "" || e.SessionID != session {
		return true
	}
	var out ViewerEvent
	switch e.Type {
	case EventApprovalRequest:
		var w promptWire
		if json.Unmarshal(e.Payload, &w) != nil {
			return true
		}
		gen := int64(0)
		if e.Generation != nil {
			gen = *e.Generation
		}
		p, ok := w.prompt(gen)
		if !ok {
			return true
		}
		out = ViewerEvent{Kind: EventApprovalRequest, Prompt: p, PromptID: p.PromptID}
	case EventApprovalSettled:
		var w struct {
			PromptID string `json:"prompt_id"`
		}
		if json.Unmarshal(e.Payload, &w) != nil || !validPromptID(w.PromptID) {
			return true
		}
		out = ViewerEvent{Kind: EventApprovalSettled, PromptID: w.PromptID}
	default:
		return true
	}
	select {
	case v.events <- out:
		return true
	default:
		return false
	}
}

// validPromptID: an approval's id as the gateway mints it (uuid4().hex at
// the pin): mirror's one rule for a Hermes request id.
func validPromptID(id string) bool { return mirror.ValidPromptID(id) }
