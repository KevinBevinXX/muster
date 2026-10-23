// Package connectors is muster's secure connector store: named credentials for
// external platforms that agents USE without ever HOLDING.
//
// 🔴 THE SECRET NEVER ENTERS AN AGENT POD. An agent reaches a platform through
// muster's proxy — /connect/{connector}/{host}/{path} — and muster injects the
// credential (a bearer token, a custom header, or HTTP basic) on the way out
// (proxy.go). The secret is stored only as AES-256-GCM ciphertext (seal.go) and
// no API, page or log line ever returns it after it was entered: the [Connector]
// type has no field that could carry it.
//
// ⚠ A RESPONSE IS NOT A GUARANTEED-CLEAN CHANNEL. An upstream that reflects the
// request (an echo endpoint, an error quoting a header) could hand the
// credential back; the proxy redacts the forms it knows and refuses the
// response shapes that would bypass that (proxy.go, secretForms). That is
// defence in depth, not a proof — the guarantee is that the secret is never in
// the pod, and only an approved agent reaches only the connector's own hosts.
//
// 🔴 ACCESS IS RECORDED PER AGENT, BUT AN APPROVAL IS SHARED BY THE OWNER'S
// AGENTS. Any agent may ASK for any connector by name; the first ask files a
// pending [Grant]. When the owner approves one, a [StandingGrant] is recorded
// for the owner, keyed on the connector, its credential source and — for the
// GitHub App's one-repository token — the agent's repository: a later request
// from ANY of the owner's agents with the same key is approved at request time
// (decided_via 'standing', naming the standing grant), and requests already
// waiting with that key are approved in the same step. Only when no standing
// grant matches is the owner prompted. An approved agent keeps access until it
// is destroyed (the grant row cascades with the agent), the grant or its
// standing grant is revoked, or the connector is deleted (its grants and
// standing grants go with it). There is no time-based expiry. A denial is final
// for that request, and is never shared.
//
// ⚠ HTTP ONLY. HTTPS APIs and git's smart-HTTP protocol go through the proxy.
// Non-HTTP platforms (a database, SSH) are out of scope: a future "handout"
// scheme would hand the secret to the pod on approval and would need its own
// opt-in, its own warning and its own audit shape — [Scheme] is the seam where
// it would be added, and nothing here accepts it today.
package connectors

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Scheme is how a connector's secret is presented to its upstream.
type Scheme string

const (
	// SchemeBearer sends `Authorization: Bearer <secret>`.
	SchemeBearer Scheme = "bearer"
	// SchemeHeader sends `<HeaderName>: <secret>` (e.g. an API-key header).
	SchemeHeader Scheme = "header"
	// SchemeBasic sends `Authorization: Basic base64(<Username>:<secret>)`.
	SchemeBasic Scheme = "basic"
)

// Grant statuses (mirrors the connector_grants.status CHECK constraint).
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusDenied   = "denied"
	StatusRevoked  = "revoked"
)

// ErrNotFound is returned when a connector or grant does not exist.
var ErrNotFound = errors.New("connectors: not found")

// ErrInvalid wraps every validation refusal, so a handler can answer 400.
var ErrInvalid = errors.New("connectors: invalid")

// ErrNotPending is returned when a decision targets a grant that was already
// decided (or revoked).
var ErrNotPending = errors.New("connectors: the request is no longer pending")

// MaxPendingGrantsPerAgent is how many requests one agent may have waiting on
// the operator at once.
//
// 🔴 EVERY NEW REQUEST PUSHES A NOTIFICATION. Without a cap, an agent looping
// over invented connector names (or a client retrying across names) files a
// row and buzzes the operator's phone per name. Past the cap a request is
// refused with ErrTooManyPending: nothing is filed and nothing is pushed. A
// request for a connector the agent ALREADY has a live grant for is answered
// with that grant and does not count.
const MaxPendingGrantsPerAgent = 5

// ErrTooManyPending is the refusal past MaxPendingGrantsPerAgent.
var ErrTooManyPending = fmt.Errorf("connectors: this agent already has %d requests waiting for the operator; "+
	"wait for them to be decided (or ask the operator) before requesting another connector", MaxPendingGrantsPerAgent)

// Connector is one stored credential's PUBLIC description.
//
// 🔴 THERE IS NO SECRET FIELD, AND THAT ABSENCE IS THE GUARANTEE that no list,
// page or JSON projection can leak one: the secret exists in plaintext only on
// the stack of the proxy call that injects it ([Store.Credential]).
type Connector struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Platform   string     `json:"platform"`
	Hosts      []string   `json:"hosts"`
	Scheme     Scheme     `json:"scheme"`
	HeaderName string     `json:"headerName,omitempty"`
	Username   string     `json:"username,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
	RotatedAt  *time.Time `json:"rotatedAt,omitempty"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
}

// AllowsHost reports whether host (as it appears in a proxy path) is one of the
// connector's declared hosts. Comparison is case-insensitive and exact: no
// suffix, wildcard or subdomain matching.
func (c Connector) AllowsHost(host string) bool {
	h := strings.ToLower(host)
	for _, a := range c.Hosts {
		if strings.ToLower(a) == h {
			return true
		}
	}
	return false
}

// Grant is one agent's request for, and the operator's decision on, one
// connector.
type Grant struct {
	ID          int64      `json:"id"`
	AgentID     int64      `json:"agentId"`
	AgentName   string     `json:"agentName"`
	Connector   string     `json:"connector"`
	Status      string     `json:"status"`
	Reason      string     `json:"reason,omitempty"`
	RequestedAt time.Time  `json:"requestedAt"`
	DecidedAt   *time.Time `json:"decidedAt,omitempty"`
	DecidedBy   string     `json:"decidedBy,omitempty"`
	LastUsedAt  *time.Time `json:"lastUsedAt,omitempty"`
	// Source is the credential the approval was about: SourceStored or
	// SourceApp (migration 0033). See ReconcileGrant.
	Source string `json:"source,omitempty"`
	// DecidedVia is where the operator decided it when that was NOT the
	// Connectors tab (migration 0035): the zero value for the tab, for a grant
	// not yet decided, a revoked one, and every decision recorded before 0035.
	// DecidedViaStanding for an automatic approval (migration 0043).
	DecidedVia DecisionSource `json:"-"`
	// StandingGrantID is the standing grant that approved this grant
	// automatically (migration 0043); 0 for every other grant.
	StandingGrantID int64 `json:"standingGrantId,omitempty"`
}

// DecidedViaStanding is decided_via for a grant approved automatically by a
// [StandingGrant] (migration 0043): no human decided THIS request — the owner
// approved an earlier one with the same key. decided_by is DecidedByStanding.
const DecidedViaStanding = "standing"

// DecidedByStanding is decided_by on a grant approved by a standing grant. It is
// deliberately not "operator": the owner did not decide this request.
const DecidedByStanding = "standing-grant"

// StandingGrant is one owner-wide approval (migration 0043): while it is live
// (RevokedAt nil), a request from any of the owner's agents for Connector whose
// credential source is Source — and, for the GitHub App (SourceApp), whose
// agent's repository is Repo — is approved automatically.
type StandingGrant struct {
	ID        int64  `json:"id"`
	Connector string `json:"connector"`
	Source    string `json:"source"`
	Repo      string `json:"repo,omitempty"`
	// OriginGrantID is the per-agent grant whose approval created it.
	OriginGrantID int64      `json:"originGrantId,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	RevokedAt     *time.Time `json:"revokedAt,omitempty"`
}

// StandingRepo is the repository part of a standing grant's key for an agent
// dispatched with repo, under credential source: the repository trimmed and
// lowercased (GitHub names are case-insensitive, and the App compares them so:
// githubapp.App.Credential) for SourceApp, and "" for a stored credential,
// which is keyed on the connector alone. ok is false when there is no key: a
// SourceApp approval for an agent with no repository grants nothing usable, so
// it is never shared.
func StandingRepo(source, repo string) (string, bool) {
	if source != SourceApp {
		return "", true
	}
	r := strings.ToLower(strings.TrimSpace(repo))
	if r == "" || len(r) > 256 {
		return "", false
	}
	return r, true
}

// DecidedViaMCP is decided_via for a decision made through
// connector_request_decide, from one of the operator's own mirrored Claude
// Code sessions (migration 0035).
const DecidedViaMCP = "mcp"

// DecisionSource is where the operator decided a request when it was NOT the
// Connectors tab: Via (DecidedViaMCP) and the mirrored session it came from
// (Host, the session's mirror host — proven by the host's token; Session, the
// Claude session id the caller named — a claim). The zero value is the tab.
type DecisionSource struct {
	Via     string
	Host    string
	Session string
}

// ViaSession reports whether src names a mirrored session (an MCP decision).
func (src DecisionSource) ViaSession() bool { return src.Via == DecidedViaMCP }

// String is "<host>/<session>", "" for the tab.
func (src DecisionSource) String() string {
	if !src.ViaSession() {
		return ""
	}
	return src.Host + "/" + src.Session
}

// Grant sources: which credential an approval was about.
const (
	SourceStored = "stored"
	SourceApp    = "app"
)

// Usable reports whether the grant currently authorises use: it is approved.
func (g Grant) Usable() bool { return g.Status == StatusApproved }

// AuditEntry is one line of the audit trail. It carries no body and no header.
type AuditEntry struct {
	ID        int64     `json:"id"`
	At        time.Time `json:"at"`
	Kind      string    `json:"kind"` // "proxy" | "grant"
	AgentID   int64     `json:"agentId,omitempty"`
	AgentName string    `json:"agentName"`
	Connector string    `json:"connector"`
	Method    string    `json:"method,omitempty"`
	Host      string    `json:"host,omitempty"`
	Path      string    `json:"path,omitempty"`
	Status    int       `json:"status,omitempty"`
	Detail    string    `json:"detail,omitempty"`
}

// NewConnector is what an operator submits to create a connector.
type NewConnector struct {
	Name       string
	Platform   string
	Hosts      []string
	Scheme     Scheme
	HeaderName string
	Username   string
	Secret     string
}

// Store persists connectors, grants and the audit trail.
type Store interface {
	ListConnectors(ctx context.Context) ([]Connector, error)
	GetConnector(ctx context.Context, name string) (Connector, error)
	CreateConnector(ctx context.Context, nc NewConnector) (Connector, error)
	RotateSecret(ctx context.Context, name, secret string) error
	DeleteConnector(ctx context.Context, name string) error
	// Credential returns the connector and its DECRYPTED secret. The proxy is
	// its only production caller.
	Credential(ctx context.Context, name string) (Connector, string, error)
	TouchConnector(ctx context.Context, name string) error

	// RequestGrant returns the live (pending or approved) grant for the pair,
	// creating a pending one when there is none. created reports which.
	RequestGrant(ctx context.Context, agentID int64, connector, reason string) (g Grant, created bool, err error)
	// LatestGrant returns the most recent grant row for the pair, live or not.
	LatestGrant(ctx context.Context, agentID int64, connector string) (Grant, error)
	GetGrant(ctx context.Context, id int64) (Grant, error)
	// DecideGrant approves or denies a PENDING grant; ErrNotPending otherwise.
	DecideGrant(ctx context.Context, id int64, approve bool, by, source string) (Grant, error)
	// DecideGrantFrom is DecideGrant recording where the decision was made
	// (via; the zero value is the Connectors tab, exactly DecideGrant).
	DecideGrantFrom(ctx context.Context, id int64, approve bool, by, source string, via DecisionSource) (Grant, error)
	// ReopenGrant puts an approved grant back to pending; reopened is false when
	// it was no longer approved.
	ReopenGrant(ctx context.Context, id int64) (g Grant, reopened bool, err error)
	RevokeGrant(ctx context.Context, id int64, by string) (Grant, error)
	ListPendingGrants(ctx context.Context) ([]Grant, error)
	// ListGrants returns every pending and approved grant (the live set).
	ListGrants(ctx context.Context) ([]Grant, error)
	TouchGrant(ctx context.Context, id int64) error

	// CreateStandingGrant records the owner's standing grant for the key
	// (connector, source, repo — see StandingRepo), from the approval of grant
	// origin; when a live one already exists it is returned and created is false.
	// It retires the connector's live standing grants under the OTHER source.
	CreateStandingGrant(ctx context.Context, connector, source, repo string, origin int64) (sg StandingGrant, created bool, err error)
	// MatchStandingGrant returns the owner's live standing grant for the key that
	// may approve agentID's request, or ErrNotFound. A standing grant does not
	// apply to an agent whose grant on the connector the owner revoked or denied
	// after the standing grant was created: that agent's own decision stands.
	MatchStandingGrant(ctx context.Context, agentID int64, connector, source, repo string) (StandingGrant, error)
	// ApproveFromStanding approves PENDING grant id from live standing grant sg
	// (recording the standing grant's credential source); ErrNotPending when the
	// grant is no longer pending or the standing grant is no longer live.
	ApproveFromStanding(ctx context.Context, id, sg int64) (Grant, error)
	// RevokeStandingGrant revokes a live standing grant AND every approved grant
	// it gave (the ones it approved automatically, and the approval it was
	// created from), returning the grants it revoked. ErrNotPending when it was
	// already revoked.
	RevokeStandingGrant(ctx context.Context, id int64, by string) (StandingGrant, []Grant, error)
	// ListStandingGrants returns the owner's live standing grants.
	ListStandingGrants(ctx context.Context) ([]StandingGrant, error)

	Audit(ctx context.Context, e AuditEntry) error
	ListAudit(ctx context.Context, limit int) ([]AuditEntry, error)
	PruneAudit(ctx context.Context, before time.Time) (int64, error)
}

// --- validation ---------------------------------------------------------------

// nameRE is the connector-name rule, the same one the table's CHECK enforces.
var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidateName refuses a connector name that is not lowercase [a-z0-9-],
// starting and ending alphanumeric, at most 63 characters.
func ValidateName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("%w: connector name %q must be lowercase letters, digits and '-', start and end "+
			"with a letter or digit, at most 63 characters", ErrInvalid, name)
	}
	return nil
}

// hostLabelRE is one DNS label.
var hostLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// NormalizeHost validates one allowlist entry and returns it lowercased.
//
// An entry is a DNS hostname with at least one dot, optionally followed by
// ":port". IP literals are refused: an allowlist that names an address cannot be
// checked against the private-range rule by name, and every real platform has a
// hostname. Wildcards are refused: the allowlist is exact by design.
func NormalizeHost(h string) (string, error) {
	h = strings.ToLower(strings.TrimSpace(h))
	if h == "" {
		return "", fmt.Errorf("%w: empty host", ErrInvalid)
	}
	host, port := h, ""
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		host, port = h[:i], h[i+1:]
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return "", fmt.Errorf("%w: host %q has an invalid port", ErrInvalid, h)
		}
	}
	if net.ParseIP(strings.Trim(host, "[]")) != nil {
		return "", fmt.Errorf("%w: host %q is an IP address; name the platform's hostname", ErrInvalid, h)
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 || len(host) > 253 {
		return "", fmt.Errorf("%w: host %q is not a fully-qualified hostname", ErrInvalid, h)
	}
	for _, l := range labels {
		if !hostLabelRE.MatchString(l) {
			return "", fmt.Errorf("%w: host %q is not a valid hostname (wildcards and URLs are not accepted)", ErrInvalid, h)
		}
	}
	return h, nil
}

// headerNameRE is an HTTP token, the characters a header name may hold.
var headerNameRE = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+.^_`|~-]+$")

// forbiddenAuthHeaders are header names a SchemeHeader connector may not use:
// they are framing or hop-by-hop headers whose value the transport owns.
var forbiddenAuthHeaders = map[string]bool{
	"host": true, "content-length": true, "transfer-encoding": true, "connection": true,
	"te": true, "trailer": true, "upgrade": true, "keep-alive": true, "cookie": true,
	"proxy-authorization": true, "x-muster-token": true,
}

// Validate refuses a NewConnector that could not be safely used, and returns it
// normalised (hosts lowercased, deduplicated and sorted).
func (nc NewConnector) Validate() (NewConnector, error) {
	nc.Name = strings.TrimSpace(nc.Name)
	if err := ValidateName(nc.Name); err != nil {
		return nc, err
	}
	nc.Platform = strings.TrimSpace(nc.Platform)
	if nc.Platform == "" {
		nc.Platform = "custom"
	}
	seen := map[string]bool{}
	var hosts []string
	for _, h := range nc.Hosts {
		if strings.TrimSpace(h) == "" {
			continue
		}
		n, err := NormalizeHost(h)
		if err != nil {
			return nc, err
		}
		if !seen[n] {
			seen[n] = true
			hosts = append(hosts, n)
		}
	}
	if len(hosts) == 0 {
		return nc, fmt.Errorf("%w: a connector needs at least one host", ErrInvalid)
	}
	sort.Strings(hosts)
	nc.Hosts = hosts
	switch nc.Scheme {
	case SchemeBearer:
		nc.HeaderName, nc.Username = "", ""
	case SchemeHeader:
		nc.Username = ""
		nc.HeaderName = strings.TrimSpace(nc.HeaderName)
		if !headerNameRE.MatchString(nc.HeaderName) || forbiddenAuthHeaders[strings.ToLower(nc.HeaderName)] {
			return nc, fmt.Errorf("%w: header name %q is not usable for a credential", ErrInvalid, nc.HeaderName)
		}
	case SchemeBasic:
		nc.HeaderName = ""
		nc.Username = strings.TrimSpace(nc.Username)
		if strings.ContainsAny(nc.Username, ":\r\n") {
			return nc, fmt.Errorf("%w: a basic-auth username may not contain ':' or a line break", ErrInvalid)
		}
	default:
		return nc, fmt.Errorf("%w: auth scheme %q is not one of bearer, header, basic", ErrInvalid, nc.Scheme)
	}
	if err := ValidateSecret(nc.Secret); err != nil {
		return nc, err
	}
	return nc, nil
}

// MinSecretLen is the shortest secret accepted. It is a floor on what the proxy
// can REDACT: a response that reflects the credential has the forms the
// redactor knows replaced (proxy.go, secretForms), and replacing a
// two-character string would mangle bodies while protecting nothing.
const MinSecretLen = 8

// ValidateSecret refuses a too-short secret or one that cannot travel in a header.
func ValidateSecret(s string) error {
	if len(s) < MinSecretLen {
		return fmt.Errorf("%w: the secret is shorter than %d characters", ErrInvalid, MinSecretLen)
	}
	if strings.ContainsAny(s, "\r\n\x00") {
		return fmt.Errorf("%w: the secret contains a line break or NUL and cannot be sent in a header", ErrInvalid)
	}
	if len(s) > 8192 {
		return fmt.Errorf("%w: the secret is longer than 8192 bytes", ErrInvalid)
	}
	return nil
}

// --- presets --------------------------------------------------------------------

// Preset is a ready-made connector shape for a known platform.
type Preset struct {
	Key         string
	Label       string
	Hosts       []string
	Scheme      Scheme
	HeaderName  string
	Username    string
	SecretLabel string
	Help        string
}

// GitHubPreset covers the REST API and git's smart-HTTP protocol.
//
// 🔴 BASIC WITH USERNAME `x-access-token`, NOT BEARER, BECAUSE ONE CONNECTOR
// SERVES BOTH HOSTS. git over HTTPS to github.com authenticates with basic auth
// (token as the password); api.github.com accepts the same basic pair as well as a
// bearer header. Basic is the one shape both accept.
var GitHubPreset = Preset{
	Key:         "github",
	Label:       "GitHub",
	Hosts:       []string{"api.github.com", "github.com"},
	Scheme:      SchemeBasic,
	Username:    "x-access-token",
	SecretLabel: "Personal access token",
	Help: "A fine-grained personal access token scoped to the repositories the agents should reach " +
		"(Contents read/write, Pull requests read/write, Metadata read). It is used for the REST API " +
		"and for git over HTTPS.",
}

// Presets is every built-in preset, in display order.
func Presets() []Preset { return []Preset{GitHubPreset} }

// PresetFor returns the preset with key, or false.
func PresetFor(key string) (Preset, bool) {
	for _, p := range Presets() {
		if p.Key == key {
			return p, true
		}
	}
	return Preset{}, false
}
