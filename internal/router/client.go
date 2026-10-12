// Package router is muster's client for the permission router it was extracted
// from — the ONE place every outbound call to that service is spelled.
//
// 🔴 WHY A PACKAGE AND NOT A FEW http.Get CALLS AT THE SITES THAT NEED THEM.
// Before the extraction each of these was an in-process function call on a
// store this binary owned. After it they are network calls to a service with
// its own auth tier, its own failure modes and its own wire spellings, and the
// thing that goes wrong is not any single call — it is that the fifth one
// invents a second way to pass the credential, or forgets the actor header, or
// degrades a transport failure to a zero value. Consolidating them means the
// credential, the actor assertion, the timeout and the "a failed read is an
// ERROR, not a false" rule are decided once. The extraction plan's own §2.2
// records the opposite pattern — one predicate open-coded at N sites — as the
// thing it keeps finding wrong at N-1 of them.
//
// 🔴 EVERY METHOD HERE IS A SEAM THE CARVE CREATED, AND EACH ONE REPLACES AN
// IN-PROCESS CALL THAT STILL EXISTS UPSTREAM. That is the test for whether
// something belongs in this file: if muster could answer it from its own
// database, it does not go here.
//
// ⚠ TWO OF THESE CALL ROUTES muster's CREDENTIAL CANNOT CURRENTLY REACH, AND
// THAT IS RECORDED RATHER THAN PAPERED OVER. See Notify and Directories; both
// carry the tier they need, the tier the route has today, and the closing
// condition. They are built because the consumer exists and the shape is
// settled; they will 401 until the upstream route is re-tiered.
package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ActorHeader is the header this client asserts its caller's name in.
//
// 🔴 IT IS THE OTHER SERVICE'S SPELLING, DELIBERATELY, AND RENAMING IT BREAKS
// EVERY CALL IN THIS FILE. A client does not get to choose the server's
// protocol: the receiving door reads this exact header name, validates the
// value as an actor label and refuses the request when it is absent. This is
// the same category as the agent runtime's session-key header, which this
// project also carries verbatim for the same reason — an external wire
// protocol is a fact about somebody else's software, not a naming choice.
//
// ⚠ IT IS SPELLED EXACTLY ONCE, HERE. A second literal is how the two drift.
const ActorHeader = "X-Clawgate-Actor"

// TokenHeader is the alternative to `Authorization: Bearer <token>` the
// receiving doors accept. Both are sent; the server compares whichever it
// reads first, constant-time.
const TokenHeader = "X-Clawgate-Token"

// DefaultTimeout bounds a single call. It is NOT the checkpoint wait — that is
// a poll loop in the caller, and each poll is one request bounded by this.
const DefaultTimeout = 15 * time.Second

// ErrNotConfigured is returned by every method on a nil or unconfigured
// Client.
//
// 🔴 IT IS AN ERROR, NOT A ZERO VALUE, AND THAT IS THE WHOLE POINT OF THE
// TYPE. The failure this repository keeps finding is a read that could not
// happen being reported as a read that found nothing: "no transcript recorded"
// over a live transcript, an empty directory picker that queried successfully
// and returned nothing, a checkpoint nobody could file reading as consent. Every
// consumer of this package must be able to tell "the router said no" from "there
// was no router to ask", so the unconfigured case is loud at the type level and
// the caller decides what it means.
var ErrNotConfigured = errors.New("router: no permission router is configured")

// ErrUnauthorized is returned when the router refuses this client's credential
// (401/403). It is distinguished from a generic failure because two of the
// routes this client calls are on tiers muster's service credential does not
// currently reach, and a caller debugging that deserves to see which.
var ErrUnauthorized = errors.New("router: the permission router refused this credential")

// Client calls the permission router.
//
// The zero value is NOT usable; use New. A nil *Client is explicitly valid and
// every method returns ErrNotConfigured on it, so a deployment without a router
// does not need nil checks at each of the call sites.
type Client struct {
	baseURL string
	token   string
	actor   string
	http    *http.Client
}

// Config is what a Client needs to exist.
type Config struct {
	// BaseURL is the router's origin, e.g. "https://router.example". No trailing
	// slash is required; one is tolerated.
	BaseURL string
	// Token is the SERVICE-tier credential. The router's service door is
	// fail-closed: it answers 503 while its own copy is unset, so an empty token
	// here is not a quiet degradation, it is a client that cannot be built.
	Token string
	// Actor is the name this service asserts on every request. The router
	// validates it as an actor label and refuses an unattributable request.
	Actor string
	// HTTP is optional; a bounded default is used when nil.
	HTTP *http.Client
}

// New builds a Client, or returns nil when the configuration is incomplete.
//
// 🔴 IT RETURNS nil RATHER THAN A HALF-BUILT CLIENT, AND THE CALLER MUST NOT
// "FIX" THAT WITH A NON-nil EMPTY ONE. A Client with no token produces a 401 on
// every call, which is indistinguishable at the call site from a router that is
// down — so the deployment that simply has no router configured would look like
// a broken one, forever, in the logs. nil says "not configured" once, at boot,
// and ErrNotConfigured says it at every call.
func New(cfg Config) *Client {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	token := strings.TrimSpace(cfg.Token)
	actor := strings.TrimSpace(cfg.Actor)
	if base == "" || token == "" || actor == "" {
		return nil
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: DefaultTimeout}
	}
	return &Client{baseURL: base, token: token, actor: actor, http: hc}
}

// Configured reports whether c can make calls. It is nil-safe.
func (c *Client) Configured() bool { return c != nil }

// do issues one request and decodes a JSON response into out (which may be
// nil). It is the only place the credential and the actor are attached.
func (c *Client) do(ctx context.Context, method, path string, body any, out any) (int, error) {
	if c == nil {
		return 0, ErrNotConfigured
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("router: encode %s %s: %w", method, path, err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return 0, fmt.Errorf("router: build %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set(TokenHeader, c.token)
	req.Header.Set(ActorHeader, c.actor)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("router: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// Drain a little so the message is in the log, then discard the rest.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return resp.StatusCode, fmt.Errorf("%w: %s %s: %s",
			ErrUnauthorized, method, path, strings.TrimSpace(string(snippet)))
	}
	if out == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)) //nolint:errcheck // drained for connection reuse only
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return resp.StatusCode, fmt.Errorf("router: decode %s %s (status %d): %w",
			method, path, resp.StatusCode, err)
	}
	return resp.StatusCode, nil
}

// --- session liveness -------------------------------------------------------

// sessionLookup is the GET /api/sessions/{id} response body. Only presence is
// used here; the other fields are decoded so a shape change is visible.
type sessionLookup struct {
	SessionID string `json:"sessionId"`
	Project   string `json:"project"`
	Cwd       string `json:"cwd"`
	Host      string `json:"host"`
}

// SessionsExisting reports which of sessionIDs the router still holds a
// transcript record for. Ids absent from the returned map have none.
//
// 🔴 THIS IS THE REPLACEMENT FOR A SQL JOIN, AND THE THREE-STATE RENDER IT
// FEEDS IS WHY IT MUST NOT DEGRADE TO false. The bit it produces drives
// SessionLink.DetailAvailable, which distinguishes "a live transcript" from
// "reaped" from "never recorded". A transport failure turned into false renders
// the permanent-looking sentence "no transcript recorded" over a session whose
// transcript is perfectly alive — silently, with nothing logged, on every row of
// the page. Upstream that exact conflation disguised a months-long outage as
// ordinary retention. So: an error propagates, and the read fails, exactly as
// the JOIN failed before it.
//
// 🔴 N REQUESTS FOR N IDS, AND THAT IS A MEASURED-COST DECISION RATHER THAN AN
// OVERSIGHT. The route is per-id; no batch form exists. See the package's
// BatchFormOwed note in doc_seams.go for what would close it.
func (c *Client) SessionsExisting(ctx context.Context, sessionIDs []string) (map[string]bool, error) {
	if c == nil {
		return nil, ErrNotConfigured
	}
	out := make(map[string]bool, len(sessionIDs))
	seen := make(map[string]bool, len(sessionIDs))
	for _, id := range sessionIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		// 🔴 ONE LOOKUP FUNCTION, TWO CALLERS. SessionMeta already decides what a
		// 404 means, what a non-200 means and how a decode failure is reported;
		// re-deciding any of that here would be a second spelling of the same rule
		// that can disagree with the first — and the direction it would disagree in
		// is "treat a failure as an absence", which is the conflation this whole
		// seam exists to prevent.
		_, ok, err := c.SessionMeta(ctx, id)
		if err != nil {
			return nil, err
		}
		if ok {
			out[id] = true
		}
	}
	return out, nil
}

// SessionMeta is what the router knows about one session.
type SessionMeta struct {
	SessionID string
	Project   string
	Cwd       string
	Host      string
}

// SessionMeta reads one session record, reporting whether the router holds one.
//
// 🔴 A 404 IS `false, nil` AND EVERYTHING ELSE IS AN ERROR. "The router has no
// record of this session" and "the router could not be asked" are different
// answers and the caller treats them differently: the first denormalises
// nothing onto the link row and moves on, the second is logged. Collapsing them
// to `false, nil` would make an outage look like a fleet of sessions that never
// existed.
func (c *Client) SessionMeta(ctx context.Context, sessionID string) (SessionMeta, bool, error) {
	if c == nil {
		return SessionMeta{}, false, ErrNotConfigured
	}
	var body sessionLookup
	status, err := c.do(ctx, http.MethodGet, "/api/sessions/"+url.PathEscape(sessionID), nil, &body)
	if status == http.StatusNotFound {
		return SessionMeta{}, false, nil
	}
	if err != nil {
		return SessionMeta{}, false, err
	}
	if status != http.StatusOK {
		return SessionMeta{}, false, fmt.Errorf("router: GET /api/sessions/%s: unexpected status %d", sessionID, status)
	}
	return SessionMeta{
		SessionID: body.SessionID,
		Project:   body.Project,
		Cwd:       body.Cwd,
		Host:      body.Host,
	}, true, nil
}

// --- the SSE bus ------------------------------------------------------------

// PublishEvent puts one event onto the router's SSE bus, so a browser holding a
// single EventSource against the router also sees muster's changes.
//
// ⚠ BEST-EFFORT BY CONTRACT AT THE CALL SITE, NOT HERE. This method reports its
// failure; whether a failed broadcast should fail the request that caused it is
// the caller's decision, and for every current caller the answer is no — a task
// was still created, and refusing the write because a notification did not land
// would be worse than a stale panel the next poll fixes.
func (c *Client) PublishEvent(ctx context.Context, name, data string) error {
	_, err := c.do(ctx, http.MethodPost, "/api/events/publish",
		map[string]string{"name": name, "data": data}, nil)
	return err
}

// --- the approval gate ------------------------------------------------------

// GateSpec is the POST /api/gate body. Field names and spellings are the
// router's, not muster's.
//
// ⚠ `sessionId`, NOT `session`. The receiving service has exactly one meaning
// for the bare word and refuses a second one on a new producer struct.
type GateSpec struct {
	Type      string   `json:"type"`
	Tool      string   `json:"tool"`
	Command   string   `json:"command"`
	Host      string   `json:"host"`
	Project   string   `json:"project"`
	Cwd       string   `json:"cwd,omitempty"`
	Context   []string `json:"context,omitempty"`
	SessionID string   `json:"sessionId,omitempty"`
}

// Gate states, as the router spells them on the wire.
const (
	// GateDecided means the card was answered — by a human, or by the router's
	// auto-approval for a type it lets that decide. Response/Comment are populated.
	GateDecided = "decided"
	// GatePending means the card is still on the operator's screen.
	GatePending = "pending"
	// GateGone means no decision will ever arrive — evicted, swept, deleted, or
	// an id this door does not speak for. A caller must stop polling on it.
	GateGone = "gone"
)

// GateDecision is the GET /api/gate/{id} response.
type GateDecision struct {
	State    string `json:"state"`
	Response string `json:"response,omitempty"`
	Comment  string `json:"comment,omitempty"`
}

// MintGate files an approval request and returns its id. Whether the router's
// auto-approval may decide it depends on spec.Type: the router's, not this
// client's, rule.
//
// 🔴 AN ERROR HERE IS A REFUSAL, NEVER AN APPROVAL. Every caller must treat a
// checkpoint that could not be FILED as not approved: nobody was asked, so
// nothing was consented to. The upstream chokepoint this replaces carries the
// same rule in the same words, and it is the one property of this whole seam
// that a wrong default silently inverts.
func (c *Client) MintGate(ctx context.Context, spec GateSpec) (string, error) {
	var out struct {
		RequestID string `json:"requestId"`
		Error     string `json:"error"`
	}
	status, err := c.do(ctx, http.MethodPost, "/api/gate", spec, &out)
	if err != nil {
		return "", err
	}
	if status != http.StatusCreated {
		if out.Error != "" {
			return "", fmt.Errorf("router: POST /api/gate: %d: %s", status, out.Error)
		}
		return "", fmt.Errorf("router: POST /api/gate: unexpected status %d", status)
	}
	if out.RequestID == "" {
		return "", errors.New("router: POST /api/gate returned no requestId")
	}
	return out.RequestID, nil
}

// ReadGate reads the three-state decision for a request this client minted.
func (c *Client) ReadGate(ctx context.Context, id string) (GateDecision, error) {
	var out GateDecision
	status, err := c.do(ctx, http.MethodGet, "/api/gate/"+url.PathEscape(id), nil, &out)
	if err != nil {
		return GateDecision{}, err
	}
	if status != http.StatusOK {
		return GateDecision{}, fmt.Errorf("router: GET /api/gate/%s: unexpected status %d", id, status)
	}
	switch out.State {
	case GateDecided, GatePending, GateGone:
		return out, nil
	default:
		// 🔴 AN UNKNOWN STATE IS AN ERROR, NOT A PENDING. Defaulting to pending
		// would make a caller poll forever against a router that has started
		// answering in a vocabulary this client does not know; defaulting to gone
		// would abandon a live card. Saying so is the only honest option.
		return GateDecision{}, fmt.Errorf("router: GET /api/gate/%s: unknown state %q", id, out.State)
	}
}

// ClearGate removes a pending card this client minted. Best-effort: a decision
// already clears it, so this covers the timeout and abort paths.
func (c *Client) ClearGate(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, "/api/gate/"+url.PathEscape(id), nil, nil)
	return err
}

// --- push fan-out -----------------------------------------------------------

// Notification is the POST /api/notify body: a fire-and-forget Web Push with a
// title and body. It creates no request, no card and nothing to poll.
type Notification struct {
	Type  string            `json:"type"`
	ID    string            `json:"id,omitempty"`
	Title string            `json:"title,omitempty"`
	Body  string            `json:"body,omitempty"`
	Tag   string            `json:"tag,omitempty"`
	Data  map[string]string `json:"data,omitempty"`
}

// Notify asks the router to fan a notification out to its push subscriptions.
//
// 🔴 SEAM — THIS ROUTE IS ON THE HOOK TIER UPSTREAM AND muster MUST NOT HOLD
// THAT CREDENTIAL, SO THIS CALL WILL 401 UNTIL THE ROUTE IS RE-TIERED.
//
//	WHAT: POST /api/notify is the generic push fan-out. It is gated by the
//	  router's shared hook token — one value handed to every hook script on
//	  every host, travelling in cleartext to a LAN port, naming no caller.
//	WHY muster CANNOT SIMPLY HOLD IT: the extraction plan concludes in two
//	  separate sections that this service must not be given that tier. The
//	  token also opens task delete, task status and the comment retraction
//	  routes; handing it over to reach a notification would grant all of them.
//	WHY THE CALL IS BUILT ANYWAY: the consumer is real and already carved (the
//	  agent-ready push in internal/api), the body shape is settled, and the
//	  alternative — dropping the notification at the carve — removes a feature
//	  silently rather than visibly. It fails LOUD (ErrUnauthorized, logged by
//	  the caller) instead of quietly doing nothing.
//	CLOSING CONDITION: the router registers POST /api/notify on its service
//	  tier (the same door /api/gate and /api/sessions/{id} already use), OR
//	  this method and its one caller are deleted together and the agent-ready
//	  push is recorded as dropped.
//	WHO CHECKS IT: the reviewer of the pull request that re-tiers it, against
//	  the router's own regenerated route golden.
func (c *Client) Notify(ctx context.Context, n Notification) error {
	_, err := c.do(ctx, http.MethodPost, "/api/notify", n, nil)
	return err
}

// --- the task-create directory picker ---------------------------------------

// Directories returns distinct working directories the router has observed,
// optionally filtered by a substring query, capped at limit.
//
// 🔴 SEAM — THE UPSTREAM ROUTE IS SESSION-GATED, SO THIS CALL WILL 401 UNTIL IT
// IS RE-TIERED. This is recorded in full in the upstream seam file too; the
// short version:
//
//	WHAT: GET /api/directories exists on the router, behind a signed operator
//	  session cookie, because a browser is its only caller there. A service
//	  credential is not a session, so muster cannot reach it.
//	WHY THE CALL IS BUILT: the picker is muster's feature — the task-create
//	  modal is on muster's side of the route partition — and the data it wants
//	  is on the router's side. That is precisely the shape a seam exists for,
//	  and the operator decided to REBUILD the picker here rather than drop it.
//	WHAT HAPPENS MEANWHILE: the handler degrades to a seeded-only picker and
//	  SAYS SO on screen. It does not render an empty dropdown as if the archive
//	  were empty — that exact conflation is why the upstream picker shipped
//	  broken and nobody noticed for the whole of its life.
//	CLOSING CONDITION: the router registers a service-tier read of the same
//	  data, and this method's path is repointed at it.
//	WHO CHECKS IT: the reviewer of that pull request, against the router's
//	  regenerated route golden.
func (c *Client) Directories(ctx context.Context, query string, limit int) ([]string, error) {
	if c == nil {
		return nil, ErrNotConfigured
	}
	q := url.Values{}
	if query != "" {
		q.Set("q", query)
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	path := "/api/directories"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var out struct {
		Directories []string `json:"directories"`
	}
	status, err := c.do(ctx, http.MethodGet, path, nil, &out)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("router: GET /api/directories: unexpected status %d", status)
	}
	return out.Directories, nil
}
