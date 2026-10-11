package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/auth"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/ui"
)

// The secure connector store's HTTP surface. See internal/connectors for the
// model; this file is its doors.
//
// TWO AUDIENCES, TWO TIERS:
//
//   - AGENTS (requireConnectorAgent): the proxy, /connect/{connector}/{host}/...,
//     and the small JSON API an agent uses to list connectors, ask for one and
//     poll its grant — /agent/connectors[/{name}[/request]]. These are the routes
//     ConnectSurfacePrefixes names, which is what the dedicated connect listener
//     (MUSTER_CONNECT_PORT) serves and nothing else.
//   - THE OPERATOR (requireSession): the Connectors tab — approve, deny, enter a
//     credential, rotate, delete, revoke.
//
// 🔴 NO ROUTE HERE RETURNS A SECRET. The connector type has no secret field; the
// only read of one is connectors.Store.Credential, called by the proxy alone.

// ConnectorsUnconfigured is why the connector routes refuse on a server with no
// connector store.
const ConnectorsUnconfigured = "this server has no connector store: MUSTER_CONNECTOR_ENCRYPTION_KEY is not set"

// ConnectSurfacePrefixes are the path prefixes the agent-facing connect surface
// is made of. cmd/muster-server's connect listener serves these and refuses
// everything else, so the network allowance an isolated agent pod is given to
// reach muster reaches this surface and no other route.
var ConnectSurfacePrefixes = []string{"/connect/", "/agent/connectors"}

// OnlyConnectSurface wraps h so it serves only ConnectSurfacePrefixes (and
// /health, for a probe), answering 404 for every other path.
func OnlyConnectSurface(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/health" {
			h.ServeHTTP(w, r)
			return
		}
		for _, pre := range ConnectSurfacePrefixes {
			if p == pre || strings.HasPrefix(p, pre+"/") || (strings.HasSuffix(pre, "/") && strings.HasPrefix(p, pre)) {
				h.ServeHTTP(w, r)
				return
			}
		}
		http.NotFound(w, r)
	})
}

func (s *Server) registerConnectorRoutes(mux Mux) {
	// --- agents ---
	mux.HandleFunc("/connect/{connector}/{host}/{path...}", s.requireConnectorAgent(s.handleConnectProxy))
	mux.HandleFunc("GET /agent/connectors", s.requireConnectorAgent(s.handleAgentConnectors))
	mux.HandleFunc("GET /agent/connectors/{name}", s.requireConnectorAgent(s.handleAgentConnectorStatus))
	mux.HandleFunc("POST /agent/connectors/{name}/request", s.requireConnectorAgent(s.handleAgentConnectorRequest))

	// --- the operator ---
	mux.HandleFunc("GET /ui/connectors", s.requireSession(s.handleConnectorsContent))
	mux.HandleFunc("GET /ui/connector-requests", s.requireSession(s.handleConnectorRequests))
	mux.HandleFunc("GET /ui/connectors/new", s.requireSession(s.handleConnectorNewForm))
	mux.HandleFunc("GET /ui/connectors/{name}/rotate", s.requireSession(s.handleConnectorRotateForm))
	// 🔴 EVERY STATE-CHANGING OPERATOR ROUTE IS requireOperatorWrite: a session
	// AND a same-origin request (requireSameOrigin). Approving a grant or
	// entering a credential from a forged cross-origin form would hand an agent
	// a platform credential. TestConnectorOperatorWritesRefuseCrossOrigin holds
	// the ledger of these routes.
	mux.HandleFunc("POST /connectors", s.requireOperatorWrite(s.handleConnectorCreate))
	mux.HandleFunc("POST /connectors/{name}/rotate", s.requireOperatorWrite(s.handleConnectorRotate))
	mux.HandleFunc("DELETE /connectors/{name}", s.requireOperatorWrite(s.handleConnectorDelete))
	mux.HandleFunc("POST /ui/connector-requests/{id}/approve", s.requireOperatorWrite(s.handleConnectorApprove))
	mux.HandleFunc("POST /ui/connector-requests/{id}/deny", s.requireOperatorWrite(s.handleConnectorDeny))
	mux.HandleFunc("DELETE /connector-grants/{id}", s.requireOperatorWrite(s.handleConnectorRevoke))
}

// requireOperatorWrite is requireSession plus requireSameOrigin.
func (s *Server) requireOperatorWrite(next http.HandlerFunc) http.HandlerFunc {
	return s.requireSession(s.requireSameOrigin(next))
}

// --- agent authentication -----------------------------------------------------------

// connectorAgentToken reads the calling agent's own token: X-Muster-Token first,
// then a Bearer Authorization.
//
// 🔴 X-Muster-Token WINS, AND THAT IS FOR THE PROXY. A client talking to a
// platform through /connect/ may carry an Authorization header of its own (git
// with a credential helper, an SDK with a default token); the proxy strips it,
// but it must not be mistaken for the agent's identity first. git is configured
// with `http.extraHeader = X-Muster-Token: …` for exactly this reason.
func connectorAgentToken(r *http.Request) string {
	if t := strings.TrimSpace(r.Header.Get("X-Muster-Token")); t != "" {
		return t
	}
	if h := r.Header.Get("Authorization"); h != "" {
		if t, ok := cutBearer(h); ok {
			return t
		}
	}
	return ""
}

// SharedTokenRefusal is the error a connector route answers when the caller
// presents a SHARED credential (the hook or service token) instead of an
// agent's own.
const SharedTokenRefusal = "a shared machine token is not an agent credential: connectors are granted per agent, " +
	"so present the calling agent's own token (MUSTER_HOOK_TOKEN inside its pod)"

// requireConnectorAgent authenticates the CALLING AGENT by its own per-agent
// token and puts the row on the context (agentFromContext).
//
// 🔴 THE SHARED HOOK TOKEN IS REFUSED BY NAME, BEFORE THE ROW LOOKUP. Grants are
// per agent; a credential every hook script holds names no agent, and admitting
// it "as" some agent would let any holder draw on that agent's approvals. The
// service token is refused for the same reason. Both checks are constant-time
// and come first, so even a database row whose hooks_token happened to equal
// one of them could not be reached through this door
// (TestConnectorRoutesRefuseTheSharedHookToken).
//
// ⚠ FAIL-CLOSED ON CONFIGURATION: with no connector store this answers 503
// whatever the credential, like requireArmedHookToken.
func (s *Server) requireConnectorAgent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.ext.Connectors == nil || s.connectProxy == nil || s.ext.Agents == nil {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": ConnectorsUnconfigured, HookUnarmedField: true,
			})
			return
		}
		token := connectorAgentToken(r)
		if token == "" {
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing agent token (X-Muster-Token)"})
			return
		}
		if auth.ConstantTimeEqual(token, s.auth.HookToken) || auth.ConstantTimeEqual(token, s.auth.ServiceToken) {
			s.logger.Printf("connectors: refused a shared machine token at %s %s", r.Method, r.URL.Path)
			s.writeJSON(w, http.StatusForbidden, map[string]any{"error": SharedTokenRefusal})
			return
		}
		a, err := s.ext.Agents.GetByHooksToken(r.Context(), token)
		if err != nil {
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid agent token"})
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), agentCtxKey{}, a)))
	}
}

func callerOf(a agents.Agent) connectors.Caller {
	return connectors.Caller{AgentID: a.ID, AgentName: a.Name}
}

// --- the proxy ------------------------------------------------------------------------

// handleConnectProxy serves /connect/{connector}/{host}/{path...}.
//
// The path is split from the ESCAPED form, so an encoded character in the
// upstream path reaches the upstream exactly as the client wrote it.
func (s *Server) handleConnectProxy(w http.ResponseWriter, r *http.Request) {
	a, _ := agentFromContext(r.Context())
	ep := strings.TrimPrefix(r.URL.EscapedPath(), "/connect/")
	parts := strings.SplitN(ep, "/", 3)
	if len(parts) < 3 {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "use /connect/{connector}/{host}/{path}"})
		return
	}
	connector, err1 := url.PathUnescape(parts[0])
	host, err2 := url.PathUnescape(parts[1])
	if err1 != nil || err2 != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "malformed proxy path"})
		return
	}
	s.connectProxy.Serve(w, r, callerOf(a), connector, host, parts[2])
}

// --- the agent JSON API ------------------------------------------------------------------

// AgentConnectorView is what an agent sees of one connector. No secret.
type AgentConnectorView struct {
	Name     string   `json:"name"`
	Exists   bool     `json:"exists"`
	Platform string   `json:"platform,omitempty"`
	Hosts    []string `json:"hosts,omitempty"`
	// Grant is this agent's status: none | pending | approved | denied | revoked.
	Grant   string `json:"grant"`
	GrantID int64  `json:"grantId,omitempty"`
	// Usage is how to call it once approved.
	Usage string `json:"usage,omitempty"`
}

// ProxyUsage is the one-line instruction an agent is given for a connector.
//
// 🔴 THE RECIPE SENDS THE AGENT'S TOKEN AS `Authorization: Bearer`, NOT AS
// X-Muster-Token. HTTP clients resend a custom header on a redirect to another
// host (curl -L does, for every -H), but drop Authorization when the host
// changes. The proxy never hands out an off-allowlist redirect in the first
// place (rewriteLocation); this is the second layer. X-Muster-Token stays
// accepted (connectorAgentToken) for clients whose Authorization header is
// already spoken for; such a client must be told not to follow redirects.
func ProxyUsage(name string, hosts []string) string {
	h := "<host>"
	if len(hosts) > 0 {
		h = hosts[0]
	}
	return "Send requests to $MUSTER_CONNECT_URL/connect/" + name + "/" + h + "/<path> with header " +
		"`Authorization: Bearer $MUSTER_HOOK_TOKEN`; muster replaces it with the credential. Allowed hosts: " +
		strings.Join(hosts, ", ")
}

func (s *Server) agentConnectorView(ctx context.Context, a agents.Agent, name string) (AgentConnectorView, error) {
	v := AgentConnectorView{Name: name, Grant: "none"}
	c, err := s.ext.Connectors.GetConnector(ctx, name)
	switch {
	case err == nil:
		v.Exists, v.Platform, v.Hosts = true, c.Platform, c.Hosts
	case !errors.Is(err, connectors.ErrNotFound):
		return v, err
	}
	g, err := s.ext.Connectors.LatestGrant(ctx, a.ID, name)
	switch {
	case err == nil:
		v.Grant, v.GrantID = g.Status, g.ID
	case !errors.Is(err, connectors.ErrNotFound):
		return v, err
	}
	if v.Exists && v.Grant == connectors.StatusApproved {
		v.Usage = ProxyUsage(name, v.Hosts)
	}
	return v, nil
}

// handleAgentConnectors lists every connector, plus any the agent has asked for
// that do not exist yet, each with the agent's grant status.
func (s *Server) handleAgentConnectors(w http.ResponseWriter, r *http.Request) {
	a, _ := agentFromContext(r.Context())
	views, err := s.AgentConnectorList(r.Context(), a)
	if err != nil {
		s.logger.Printf("connectors: list for agent %d: %v", a.ID, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not list connectors"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"connectors": views})
}

// AgentConnectorList is the agent's view of every connector (and of the
// connectors it has requested that do not exist yet).
func (s *Server) AgentConnectorList(ctx context.Context, a agents.Agent) ([]AgentConnectorView, error) {
	list, err := s.ext.Connectors.ListConnectors(ctx)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, c := range list {
		names[c.Name] = true
	}
	pending, err := s.ext.Connectors.ListGrants(ctx)
	if err != nil {
		return nil, err
	}
	for _, g := range pending {
		if g.AgentID == a.ID {
			names[g.Connector] = true
		}
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	out := make([]AgentConnectorView, 0, len(sorted))
	for _, n := range sorted {
		v, err := s.agentConnectorView(ctx, a, n)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// maxConnectorWait bounds a status long-poll, under common proxy idle timeouts.
const maxConnectorWait = 50 * time.Second

// AgentConnectorStatus returns the agent's view of one connector, waiting up to
// wait for a PENDING grant to be decided.
func (s *Server) AgentConnectorStatus(ctx context.Context, a agents.Agent, name string, wait time.Duration) (AgentConnectorView, error) {
	if wait > maxConnectorWait {
		wait = maxConnectorWait
	}
	deadline := time.Now().Add(wait)
	for {
		v, err := s.agentConnectorView(ctx, a, name)
		if err != nil || v.Grant != connectors.StatusPending || !time.Now().Before(deadline) {
			return v, err
		}
		select {
		case <-ctx.Done():
			return v, nil
		case <-time.After(time.Second):
		}
	}
}

// handleAgentConnectorStatus serves GET /agent/connectors/{name}[?wait=seconds].
func (s *Server) handleAgentConnectorStatus(w http.ResponseWriter, r *http.Request) {
	a, _ := agentFromContext(r.Context())
	name := r.PathValue("name")
	if err := connectors.ValidateName(name); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	var wait time.Duration
	if v := r.URL.Query().Get("wait"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "wait is a number of seconds"})
			return
		}
		wait = time.Duration(n) * time.Second
	}
	v, err := s.AgentConnectorStatus(r.Context(), a, name, wait)
	if err != nil {
		s.logger.Printf("connectors: status %q for agent %d: %v", name, a.ID, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not read the connector"})
		return
	}
	s.writeJSON(w, http.StatusOK, v)
}

// AgentRequestConnector files (or finds) the agent's request for a connector.
//
// Unlike the proxy's implicit request, this one IS filed after a denial or a
// revocation: asking explicitly is the deliberate act that re-prompts the
// operator.
func (s *Server) AgentRequestConnector(ctx context.Context, a agents.Agent, name, reason string) (AgentConnectorView, bool, error) {
	if err := connectors.ValidateName(name); err != nil {
		return AgentConnectorView{}, false, err
	}
	if g, err := s.ext.Connectors.LatestGrant(ctx, a.ID, name); err == nil && g.Usable() {
		v, err := s.agentConnectorView(ctx, a, name)
		return v, false, err
	}
	g, created, err := s.ext.Connectors.RequestGrant(ctx, a.ID, name, strings.TrimSpace(reason))
	if err != nil {
		return AgentConnectorView{}, false, err
	}
	if created {
		if err := s.ext.Connectors.Audit(ctx, connectors.AuditEntry{Kind: "grant", AgentID: a.ID, AgentName: a.Name,
			Connector: name, Detail: "requested"}); err != nil {
			s.logger.Printf("connectors: audit request %d: %v", g.ID, err)
		}
		s.onConnectorRequested(g)
	}
	v, err := s.agentConnectorView(ctx, a, name)
	return v, created, err
}

type agentConnectorRequestBody struct {
	Reason string `json:"reason"`
}

// handleAgentConnectorRequest serves POST /agent/connectors/{name}/request.
func (s *Server) handleAgentConnectorRequest(w http.ResponseWriter, r *http.Request) {
	a, _ := agentFromContext(r.Context())
	var body agentConnectorRequestBody
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
			return
		}
	}
	v, created, err := s.AgentRequestConnector(r.Context(), a, r.PathValue("name"), body.Reason)
	if errors.Is(err, connectors.ErrInvalid) {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if errors.Is(err, connectors.ErrTooManyPending) {
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": err.Error()})
		return
	}
	if err != nil {
		s.logger.Printf("connectors: request %q for agent %d: %v", r.PathValue("name"), a.ID, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not file the request"})
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusAccepted
	}
	s.writeJSON(w, code, v)
}

// onConnectorRequested prompts the operator for a new pending grant: an SSE
// nudge for an open Connectors tab and a push notification.
func (s *Server) onConnectorRequested(g connectors.Grant) {
	s.logger.Printf("connector request created id=%d agent=%q connector=%q", g.ID, g.AgentName, g.Connector)
	s.broadcast(EventConnectorChanged, strconv.FormatInt(g.ID, 10))
	if s.router == nil {
		return
	}
	body := g.Connector
	if g.Reason != "" {
		body += " — " + g.Reason
	}
	s.goNotify(RouterNotification{
		Type:  "connector",
		ID:    strconv.FormatInt(g.ID, 10),
		Title: "Connector request: " + g.AgentName,
		Body:  body,
		Tag:   "connector-" + strconv.FormatInt(g.ID, 10),
		Data:  map[string]string{"url": "/connectors"},
	}, func(n int) {
		s.logger.Printf("push: delivered connector request id=%d to %d device(s)", g.ID, n)
	})
}

// --- the operator's surface ---------------------------------------------------------------

func (s *Server) renderConnectorsContent(w http.ResponseWriter, r *http.Request, c ui.ConnectorsContent) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if s.ext.Connectors == nil {
		c = ui.ConnectorsContent{Unconfigured: ConnectorsUnconfigured + ". Generate a 32-byte key " +
			"(`muster gentoken` or `openssl rand -base64 32`), set it, and restart."}
	} else {
		if err := s.fillConnectorsContent(r.Context(), &c); err != nil {
			s.logger.Printf("connectors: render: %v", err)
			http.Error(w, "could not load connectors", http.StatusInternalServerError)
			return
		}
	}
	if err := ui.RenderConnectors(w, c); err != nil {
		s.logger.Printf("connectors: render: %v", err)
	}
}

func (s *Server) connectorRequestViews(ctx context.Context) ([]ui.ConnectorRequestView, error) {
	pending, err := s.ext.Connectors.ListPendingGrants(ctx)
	if err != nil {
		return nil, err
	}
	list, err := s.ext.Connectors.ListConnectors(ctx)
	if err != nil {
		return nil, err
	}
	exists := map[string]bool{}
	for _, c := range list {
		exists[c.Name] = true
	}
	out := make([]ui.ConnectorRequestView, 0, len(pending))
	for _, g := range pending {
		out = append(out, ui.ConnectorRequestView{ID: g.ID, AgentName: g.AgentName, Connector: g.Connector,
			Reason: g.Reason, RequestedAt: g.RequestedAt, Exists: exists[g.Connector]})
	}
	return out, nil
}

func (s *Server) fillConnectorsContent(ctx context.Context, c *ui.ConnectorsContent) error {
	reqs, err := s.connectorRequestViews(ctx)
	if err != nil {
		return err
	}
	c.Requests = reqs
	list, err := s.ext.Connectors.ListConnectors(ctx)
	if err != nil {
		return err
	}
	grants, err := s.ext.Connectors.ListGrants(ctx)
	if err != nil {
		return err
	}
	by := map[string][]ui.ConnectorGrantView{}
	for _, g := range grants {
		by[g.Connector] = append(by[g.Connector], ui.ConnectorGrantView{ID: g.ID, AgentName: g.AgentName,
			Status: g.Status, LastUsedAt: g.LastUsedAt, RequestedAt: g.RequestedAt})
	}
	c.Connectors = make([]ui.ConnectorView, 0, len(list))
	for _, k := range list {
		c.Connectors = append(c.Connectors, ui.ConnectorView{Name: k.Name, Platform: k.Platform, Hosts: k.Hosts,
			Scheme: string(k.Scheme), HeaderName: k.HeaderName, Username: k.Username,
			CreatedAt: k.CreatedAt, RotatedAt: k.RotatedAt, LastUsedAt: k.LastUsedAt, Grants: by[k.Name]})
	}
	audit, err := s.ext.Connectors.ListAudit(ctx, 25)
	if err != nil {
		return err
	}
	for _, e := range audit {
		c.Audit = append(c.Audit, ui.ConnectorAuditView{At: e.At, Kind: e.Kind, AgentName: e.AgentName,
			Connector: e.Connector, Method: e.Method, Host: e.Host, Path: e.Path, Status: e.Status, Detail: e.Detail})
	}
	return nil
}

func (s *Server) handleConnectorsContent(w http.ResponseWriter, r *http.Request) {
	s.renderConnectorsContent(w, r, ui.ConnectorsContent{})
}

func (s *Server) handleConnectorRequests(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if s.ext.Connectors == nil {
		return
	}
	reqs, err := s.connectorRequestViews(r.Context())
	if err != nil {
		s.logger.Printf("connectors: list requests: %v", err)
		http.Error(w, "could not load connector requests", http.StatusInternalServerError)
		return
	}
	if err := ui.RenderConnectorRequests(w, reqs); err != nil {
		s.logger.Printf("connectors: render requests: %v", err)
	}
}

func presetViews() []ui.ConnectorPresetView {
	var out []ui.ConnectorPresetView
	for _, p := range connectors.Presets() {
		out = append(out, ui.ConnectorPresetView{Key: p.Key, Label: p.Label, Help: p.Help, SecretLabel: p.SecretLabel, Hosts: p.Hosts})
	}
	return out
}

// connectorForm builds the form view from a request id (if any) and the
// submitted or queried fields.
func (s *Server) connectorForm(ctx context.Context, q url.Values) (ui.ConnectorFormView, error) {
	f := ui.ConnectorFormView{
		Presets:  presetViews(),
		Name:     strings.TrimSpace(q.Get("name")),
		Preset:   q.Get("preset"),
		Hosts:    q.Get("hosts"),
		Scheme:   q.Get("scheme"),
		Header:   q.Get("header_name"),
		Username: q.Get("username"),
	}
	if id, err := strconv.ParseInt(q.Get("request"), 10, 64); err == nil && id > 0 {
		f.RequestID = id
	} else if id, err := strconv.ParseInt(q.Get("request_id"), 10, 64); err == nil && id > 0 {
		f.RequestID = id
	}
	if f.RequestID != 0 {
		g, err := s.ext.Connectors.GetGrant(ctx, f.RequestID)
		if err != nil {
			return f, err
		}
		if g.Status != connectors.StatusPending {
			return f, connectors.ErrNotPending
		}
		f.Name, f.AgentName = g.Connector, g.AgentName
	}
	if f.Preset == "" {
		f.Preset = "custom"
		if _, ok := connectors.PresetFor(f.Name); ok {
			f.Preset = f.Name
		}
	}
	return f, nil
}

func (s *Server) handleConnectorNewForm(w http.ResponseWriter, r *http.Request) {
	if s.ext.Connectors == nil {
		http.Error(w, ConnectorsUnconfigured, http.StatusServiceUnavailable)
		return
	}
	f, err := s.connectorForm(r.Context(), r.URL.Query())
	if err != nil {
		s.renderConnectorsContent(w, r, ui.ConnectorsContent{Error: "That request is no longer pending."})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.RenderConnectorForm(w, f); err != nil {
		s.logger.Printf("connectors: render form: %v", err)
	}
}

func (s *Server) handleConnectorRotateForm(w http.ResponseWriter, r *http.Request) {
	if s.ext.Connectors == nil {
		http.Error(w, ConnectorsUnconfigured, http.StatusServiceUnavailable)
		return
	}
	name := r.PathValue("name")
	if _, err := s.ext.Connectors.GetConnector(r.Context(), name); err != nil {
		s.renderConnectorsContent(w, r, ui.ConnectorsContent{Error: "No connector named " + name + "."})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.RenderConnectorRotateForm(w, name, ""); err != nil {
		s.logger.Printf("connectors: render rotate form: %v", err)
	}
}

// handleConnectorCreate creates a connector from the form and, when the form
// answers a pending request, approves that request in the same step.
func (s *Server) handleConnectorCreate(w http.ResponseWriter, r *http.Request) {
	if s.ext.Connectors == nil {
		http.Error(w, ConnectorsUnconfigured, http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	f, err := s.connectorForm(ctx, r.PostForm)
	if err != nil {
		s.renderConnectorsContent(w, r, ui.ConnectorsContent{Error: "That request is no longer pending."})
		return
	}
	fail := func(msg string) {
		f.Error = msg
		s.renderConnectorsContent(w, r, ui.ConnectorsContent{EditorForm: &f})
	}
	nc := connectors.NewConnector{Name: f.Name, Secret: r.PostForm.Get("secret")}
	if p, ok := connectors.PresetFor(f.Preset); ok {
		nc.Platform, nc.Hosts, nc.Scheme, nc.HeaderName, nc.Username = p.Key, p.Hosts, p.Scheme, p.HeaderName, p.Username
	} else {
		nc.Platform = "custom"
		nc.Hosts = strings.FieldsFunc(f.Hosts, func(r rune) bool { return r == '\n' || r == ',' || r == ' ' || r == '\r' })
		nc.Scheme = connectors.Scheme(f.Scheme)
		nc.HeaderName, nc.Username = f.Header, f.Username
	}
	c, err := s.ext.Connectors.CreateConnector(ctx, nc)
	if err != nil {
		if errors.Is(err, connectors.ErrInvalid) {
			fail(strings.TrimPrefix(err.Error(), connectors.ErrInvalid.Error()+": "))
			return
		}
		s.logger.Printf("connectors: create %q: %v", nc.Name, err)
		fail("Could not save the connector.")
		return
	}
	s.auditGrant(ctx, connectors.Grant{Connector: c.Name}, "connector created by the operator")
	notice := "Connector " + c.Name + " saved."
	if f.RequestID != 0 {
		g, err := s.ext.Connectors.DecideGrant(ctx, f.RequestID, true, "operator")
		if err != nil {
			s.logger.Printf("connectors: approve %d after create: %v", f.RequestID, err)
			s.renderConnectorsContent(w, r, ui.ConnectorsContent{Notice: notice,
				Error: "The connector was saved but the request could not be approved; approve it below."})
			s.broadcast(EventConnectorChanged, c.Name)
			return
		}
		s.auditGrant(ctx, g, "approved")
		notice = "Connector " + c.Name + " saved and " + g.AgentName + " approved."
	}
	s.broadcast(EventConnectorChanged, c.Name)
	s.renderConnectorsContent(w, r, ui.ConnectorsContent{Notice: notice})
}

func (s *Server) auditGrant(ctx context.Context, g connectors.Grant, detail string) {
	if err := s.ext.Connectors.Audit(ctx, connectors.AuditEntry{Kind: "grant", AgentID: g.AgentID,
		AgentName: g.AgentName, Connector: g.Connector, Detail: detail}); err != nil {
		s.logger.Printf("connectors: audit %q %s: %v", g.Connector, detail, err)
	}
}

func (s *Server) handleConnectorRotate(w http.ResponseWriter, r *http.Request) {
	if s.ext.Connectors == nil {
		http.Error(w, ConnectorsUnconfigured, http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	name := r.PathValue("name")
	err := s.ext.Connectors.RotateSecret(r.Context(), name, r.PostForm.Get("secret"))
	switch {
	case errors.Is(err, connectors.ErrInvalid):
		s.renderConnectorsContent(w, r, ui.ConnectorsContent{Error: strings.TrimPrefix(err.Error(), connectors.ErrInvalid.Error()+": ")})
		return
	case errors.Is(err, connectors.ErrNotFound):
		s.renderConnectorsContent(w, r, ui.ConnectorsContent{Error: "No connector named " + name + "."})
		return
	case errors.Is(err, connectors.ErrDecrypt):
		s.logger.Printf("connectors: rotate %q: %v", name, err)
		s.renderConnectorsContent(w, r, ui.ConnectorsContent{Error: "The stored secret of " + name + " no longer " +
			"opens (the encryption key changed, or its hosts or auth settings were altered outside muster), so it " +
			"cannot be rotated in place. Delete the connector and create it again."})
		return
	case err != nil:
		s.logger.Printf("connectors: rotate %q: %v", name, err)
		http.Error(w, "could not rotate", http.StatusInternalServerError)
		return
	}
	s.auditGrant(r.Context(), connectors.Grant{Connector: name}, "secret rotated by the operator")
	s.broadcast(EventConnectorChanged, name)
	s.renderConnectorsContent(w, r, ui.ConnectorsContent{Notice: "The secret of " + name + " was replaced."})
}

func (s *Server) handleConnectorDelete(w http.ResponseWriter, r *http.Request) {
	if s.ext.Connectors == nil {
		http.Error(w, ConnectorsUnconfigured, http.StatusServiceUnavailable)
		return
	}
	name := r.PathValue("name")
	if err := s.ext.Connectors.DeleteConnector(r.Context(), name); err != nil && !errors.Is(err, connectors.ErrNotFound) {
		s.logger.Printf("connectors: delete %q: %v", name, err)
		http.Error(w, "could not delete", http.StatusInternalServerError)
		return
	}
	s.auditGrant(r.Context(), connectors.Grant{Connector: name}, "connector deleted by the operator; its grants were removed")
	s.broadcast(EventConnectorChanged, name)
	s.renderConnectorsContent(w, r, ui.ConnectorsContent{Notice: "Connector " + name + " deleted."})
}

func (s *Server) decideConnectorRequest(w http.ResponseWriter, r *http.Request, approve bool) {
	if s.ext.Connectors == nil {
		http.Error(w, ConnectorsUnconfigured, http.StatusServiceUnavailable)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	if approve {
		g, err := s.ext.Connectors.GetGrant(ctx, id)
		if err != nil {
			s.renderConnectorsContent(w, r, ui.ConnectorsContent{Error: "That request no longer exists."})
			return
		}
		if _, err := s.ext.Connectors.GetConnector(ctx, g.Connector); err != nil {
			s.renderConnectorsContent(w, r, ui.ConnectorsContent{Error: "Connector " + g.Connector +
				" does not exist; enter a credential for it instead."})
			return
		}
	}
	g, err := s.ext.Connectors.DecideGrant(ctx, id, approve, "operator")
	switch {
	case errors.Is(err, connectors.ErrNotPending), errors.Is(err, connectors.ErrNotFound):
		s.renderConnectorsContent(w, r, ui.ConnectorsContent{Error: "That request was already decided."})
		return
	case err != nil:
		s.logger.Printf("connectors: decide %d: %v", id, err)
		http.Error(w, "could not record the decision", http.StatusInternalServerError)
		return
	}
	verb := "denied"
	if approve {
		verb = "approved"
	}
	s.auditGrant(ctx, g, verb)
	s.broadcast(EventConnectorChanged, strconv.FormatInt(id, 10))
	s.renderConnectorsContent(w, r, ui.ConnectorsContent{Notice: g.AgentName + " " + verb + " for " + g.Connector + "."})
}

func (s *Server) handleConnectorApprove(w http.ResponseWriter, r *http.Request) {
	s.decideConnectorRequest(w, r, true)
}

func (s *Server) handleConnectorDeny(w http.ResponseWriter, r *http.Request) {
	s.decideConnectorRequest(w, r, false)
}

func (s *Server) handleConnectorRevoke(w http.ResponseWriter, r *http.Request) {
	if s.ext.Connectors == nil {
		http.Error(w, ConnectorsUnconfigured, http.StatusServiceUnavailable)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	g, err := s.ext.Connectors.RevokeGrant(r.Context(), id, "operator")
	switch {
	case errors.Is(err, connectors.ErrNotPending), errors.Is(err, connectors.ErrNotFound):
		s.renderConnectorsContent(w, r, ui.ConnectorsContent{Error: "That grant was already ended."})
		return
	case err != nil:
		s.logger.Printf("connectors: revoke %d: %v", id, err)
		http.Error(w, "could not revoke", http.StatusInternalServerError)
		return
	}
	s.auditGrant(r.Context(), g, "revoked")
	s.broadcast(EventConnectorChanged, strconv.FormatInt(id, 10))
	s.renderConnectorsContent(w, r, ui.ConnectorsContent{Notice: g.AgentName + "'s access to " + g.Connector + " was revoked."})
}
