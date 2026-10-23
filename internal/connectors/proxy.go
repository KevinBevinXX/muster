package connectors

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
)

// Policy is the proxy's network policy for upstream connections.
//
// 🔴 THE ZERO VALUE IS PRODUCTION, AND IT IS THE STRICT ONE: HTTPS only, the
// system resolver, and no non-public destination. Each field below LOOSENS it and
// exists for tests (a fake upstream on loopback); cmd/muster-server never sets
// one outside a binary built with the e2e tag.
type Policy struct {
	// AllowLoopback admits loopback destinations (127.0.0.0/8, ::1). Every other
	// non-public range stays refused.
	AllowLoopback bool
	// InsecureHTTP speaks plain HTTP to the upstream instead of HTTPS.
	InsecureHTTP bool
	// Resolve replaces the system resolver. nil means net.DefaultResolver.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
}

// ErrRoleUncertain refuses access decided on a fallback role (the caller's
// owner's role could not be read). Retryable.
var ErrRoleUncertain = errors.New("connectors: the agent's owner's role could not be read; retry shortly")

// ErrForbiddenAddress is the dial refusal for a non-public destination.
var ErrForbiddenAddress = errors.New("connectors: refusing to connect to a non-public address")

// nonPublic are the ranges no connector may reach, beyond what netip's own
// predicates (private, loopback, link-local, multicast, unspecified) cover.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),  // RFC 6598 shared address space (CGNAT, mesh overlays)
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved, and broadcast
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64: maps onto IPv4, private ranges included
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64
	netip.MustParsePrefix("2002::/16"),      // 6to4: embeds an IPv4 address
	netip.MustParsePrefix("100::/64"),       // discard-only
	netip.MustParsePrefix("2001:db8::/32"),  // documentation
	netip.MustParsePrefix("fec0::/10"),      // deprecated site-local
	netip.MustParsePrefix("::ffff:0:0/96"),  // IPv4-mapped (unmapped before the check; belt and braces)
}

// AllowedAddr reports whether the policy permits connecting to ip.
func (p Policy) AllowedAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return false
	}
	if ip.IsLoopback() {
		return p.AllowLoopback
	}
	if ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, pfx := range nonPublic {
		if pfx.Contains(ip) {
			return false
		}
	}
	return true
}

func (p Policy) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if p.Resolve != nil {
		return p.Resolve(ctx, host)
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// dialContext resolves the host itself, refuses the connection if ANY resolved
// address is non-public, and dials a checked ADDRESS rather than the name.
//
// 🔴 DIALLING THE CHECKED ADDRESS IS WHAT DEFEATS DNS REBINDING. Checking a
// name's resolution and then handing the NAME to the dialer resolves twice; an
// attacker-controlled zone can answer public the first time and private the
// second. The Dialer's Control hook re-checks the socket's actual destination as
// a backstop for any path that reaches the dialer without coming through here.
func (p Policy) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var ips []netip.Addr
	if ip, perr := netip.ParseAddr(host); perr == nil {
		ips = []netip.Addr{ip}
	} else {
		ips, err = p.resolve(ctx, host)
		if err != nil {
			return nil, err
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("connectors: %s resolved to no address", host)
	}
	for _, ip := range ips {
		if !p.AllowedAddr(ip) {
			return nil, fmt.Errorf("%w (%s resolves to one)", ErrForbiddenAddress, host)
		}
	}
	d := &net.Dialer{
		Timeout: 15 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error { return p.checkSocketAddr(address) },
	}
	var lastErr error
	for _, ip := range ips {
		c, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return c, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// checkSocketAddr is the dialer's last-moment check on the address a socket is
// about to connect to ("ip:port").
func (p Policy) checkSocketAddr(address string) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil || !p.AllowedAddr(ap.Addr()) {
		return fmt.Errorf("%w (socket destination)", ErrForbiddenAddress)
	}
	return nil
}

// Transport builds the upstream transport for this policy.
//
// 🔴 Proxy IS nil, DELIBERATELY: an HTTPS_PROXY in muster's environment would
// otherwise carry every injected credential through a third party, and the
// address checks above would be checking the proxy rather than the platform.
func (p Policy) Transport() *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           p.dialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
		ExpectContinueTimeout: time.Second,
	}
}

// Caller is the authenticated agent a proxied request is made for.
type Caller struct {
	AgentID   int64
	AgentName string
	// Repo is the agent's own repository (owner/name), "" when it was dispatched
	// without one. A virtual connector that scopes its credential to the agent's
	// repository (the GitHub App) reads it; a stored connector ignores it.
	Repo string
}

// Proxy forwards an agent's request to a connector's upstream with the
// credential injected.
type Proxy struct {
	Store  Store
	Policy Policy
	Logger *log.Logger
	// OnGrantRequested is called (synchronously; keep it cheap) when a proxied
	// request files a NEW pending grant, so the operator can be prompted.
	OnGrantRequested func(context.Context, Grant)
	// AutoApprove, when set, is offered every request a check FILES, BEFORE
	// OnGrantRequested: it returns the grant approved and true when an owner's
	// standing grant covers it (the api package's approveFromStanding, the one
	// rule every door shares), and the owner is then not prompted. false leaves
	// the grant as it was. A grant RE-OPENED because its connector's credential
	// source changed is not offered: that change asks the owner again (0033),
	// and their approval is then shared as any other.
	AutoApprove func(context.Context, Grant) (Grant, bool)
	// StreamsEnd is the server's shutdown signal for streams: once it is done, a
	// proxied GET EVENT STREAM in flight ends, and one that starts later ends at
	// once (endsAtShutdown says which exchanges, and why only GET).
	// Every other proxied request is left alone. nil means never (a Proxy built
	// directly, as tests do). The api package hands it the same signal
	// api.Server.EndStreams cancels, so ending its own streams and the ones relayed
	// here is ONE event, fired by the one EndStreams call at the top of
	// cmd/muster-server's shutdown, before either listener drains. See
	// endOnShutdown.
	StreamsEnd context.Context

	transportOnce sync.Once
	built         http.RoundTripper // Policy.Transport(), built once
	transport     http.RoundTripper // SetTransport's override
}

// SetTransport overrides the upstream transport (tests). Call it before the
// first Serve: the transport is fixed from then on.
func (p *Proxy) SetTransport(rt http.RoundTripper) { p.transport = rt }

func (p *Proxy) logf(format string, args ...any) {
	if p.Logger != nil {
		p.Logger.Printf(format, args...)
	}
}

// RefusalBody is the JSON every refusal from the proxy carries.
type RefusalBody struct {
	Error     string `json:"error"`
	Connector string `json:"connector,omitempty"`
	// Grant is the caller's grant status for the connector, when the refusal is
	// about access: "pending", "denied", "revoked".
	Grant   string `json:"grant,omitempty"`
	GrantID int64  `json:"grantId,omitempty"`
}

func writeRefusal(w http.ResponseWriter, code int, body RefusalBody) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// Access is the outcome of checking a caller's grant.
type Access struct {
	Grant  Grant
	Usable bool
	// Filed is true when THIS check filed a new pending request.
	Filed bool
}

// CheckAccess resolves whether the caller may use the connector now, filing a
// pending request when the caller has never asked.
//
// 🔴 A DENIAL OR A REVOCATION IS NOT RE-FILED HERE. A retrying client would
// otherwise turn one "no" into a prompt per retry. The agent must ask again
// explicitly (the request route / the connector_request tool), which is a
// deliberate act the operator then sees as a new request.
func (p *Proxy) CheckAccess(ctx context.Context, caller Caller, connector, reason string) (Access, error) {
	// 🔴 BACKSTOP: NEVER DECIDE ACCESS ON A FALLBACK ROLE. The connect door
	// already answers 503 for an uncertain principal (api.requireConnectorAgent);
	// this holds for any future door that forgets to, so no credential is ever
	// chosen — stored or App — for a role that could not be read.
	if pr, ok := authz.From(ctx); ok && pr.RoleUncertain {
		return Access{}, ErrRoleUncertain
	}
	g, err := p.Store.LatestGrant(ctx, caller.AgentID, connector)
	switch {
	case err == nil:
		// An approval about another credential source than the connector now
		// has (the GitHub App configured, or removed) is pending again.
		g, reopened, rerr := ReconcileGrant(ctx, p.Store, g)
		if rerr != nil {
			return Access{}, rerr
		}
		if reopened {
			p.recordGrantEvent(ctx, g, "re-opened: the connector's credential source changed")
			if p.OnGrantRequested != nil {
				p.OnGrantRequested(ctx, g)
			}
		}
		// approved -> usable; pending, denied, revoked -> refused as they stand.
		return Access{Grant: g, Usable: g.Usable()}, nil
	case err != nil && !errors.Is(err, ErrNotFound):
		return Access{}, err
	}
	// No grant at all: file (or find) a pending request.
	ng, created, err := p.Store.RequestGrant(ctx, caller.AgentID, connector, reason)
	if err != nil {
		return Access{}, err
	}
	if created {
		p.recordGrantEvent(ctx, ng, "requested")
		ng = p.pending(ctx, ng)
	}
	return Access{Grant: ng, Usable: ng.Usable(), Filed: created}, nil
}

// pending handles a request this check filed: approved by a standing grant
// (AutoApprove), or else the owner prompted (OnGrantRequested).
func (p *Proxy) pending(ctx context.Context, g Grant) Grant {
	if p.AutoApprove != nil {
		if ag, ok := p.AutoApprove(ctx, g); ok {
			return ag
		}
	}
	if p.OnGrantRequested != nil {
		p.OnGrantRequested(ctx, g)
	}
	return g
}

func (p *Proxy) recordGrantEvent(ctx context.Context, g Grant, detail string) {
	if err := p.Store.Audit(ctx, AuditEntry{Kind: "grant", AgentID: g.AgentID, AgentName: g.AgentName,
		Connector: g.Connector, Detail: detail}); err != nil {
		p.logf("connectors: audit grant %d %s: %v", g.ID, detail, err)
	}
}

// hopHeaders are the request headers never forwarded upstream, beyond the
// hop-by-hop set httputil.ReverseProxy already removes.
//
// 🔴 Authorization AND THE MUSTER TOKEN ARE THE LOAD-BEARING ENTRIES: the
// caller's own credential to muster must never reach a third party, and an
// agent-supplied Authorization must never override (or accompany) the injected
// one. Cookie is dropped because a cookie the agent holds for the platform is a
// credential muster did not issue.
//
// 🔴 Range AND If-Range ARE DROPPED BECAUSE THEY DEFEAT THE REDACTOR. An
// upstream that reflects the credential into its body would otherwise serve it
// in caller-chosen slices (bytes=0-9, bytes=10-19, ...), each shorter than any
// form the redactor looks for, and the caller rejoins them. Te is dropped so the
// upstream is never invited to send trailers (they are discarded anyway, see
// trailerlessWriter). Accept-Encoding is replaced, not just dropped (Serve).
var droppedRequestHeaders = []string{
	"Authorization", "Proxy-Authorization", "Cookie", "X-Muster-Token", "X-Muster-Actor",
	"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip",
	"Accept-Encoding", "Range", "If-Range", "Te",
}

// allowedMethods are the request methods the proxy forwards. Everything else is
// refused with 405 before an access check, so an unjustifiable method neither
// reaches an upstream nor files a request.
//
// 🔴 TRACE IS THE ONE THAT MATTERS: its whole purpose is to echo the request,
// injected credential included, back in the response. CONNECT has no meaning
// for a path-routed proxy, and a WebDAV-style or invented method is not
// something a REST API or git's smart-HTTP protocol needs.
var allowedMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true, http.MethodOptions: true,
}

// Errors ModifyResponse returns to refuse an upstream response; ErrorHandler
// maps each to a 502 that names the reason.
var (
	errEncodedResponse   = errors.New("connectors: the upstream sent an encoded (compressed) body it was asked not to")
	errOffHostRedirect   = errors.New("connectors: the upstream redirected outside the connector's allowlist")
	errSwitchedProtocols = errors.New("connectors: the upstream tried to switch protocols")
)

// Serve proxies one request. connector and host are the path's first two
// segments after /connect/, and rest is everything after the host segment,
// still escaped (no leading slash).
func (p *Proxy) Serve(w http.ResponseWriter, r *http.Request, caller Caller, connector, host, rest string) {
	ctx := r.Context()
	auditPath := "/" + rest
	status := 0
	detail := ""
	defer func() {
		if err := p.Store.Audit(context.WithoutCancel(ctx), AuditEntry{
			Kind: "proxy", AgentID: caller.AgentID, AgentName: caller.AgentName, Connector: connector,
			Method: r.Method, Host: strings.ToLower(host), Path: auditPath, Status: status, Detail: detail,
		}); err != nil {
			p.logf("connectors: audit %s %s/%s: %v", r.Method, connector, host, err)
		}
	}()
	refuse := func(code int, body RefusalBody, why string) {
		status, detail = code, why
		body.Connector = connector
		writeRefusal(w, code, body)
	}

	if err := ValidateName(connector); err != nil {
		refuse(http.StatusBadRequest, RefusalBody{Error: err.Error()}, "bad connector name")
		return
	}
	if strings.ContainsAny(rest, "\r\n\x00") || strings.ContainsAny(host, "/\\@?#\r\n\x00") {
		refuse(http.StatusBadRequest, RefusalBody{Error: "malformed proxy path"}, "malformed path")
		return
	}
	if !allowedMethods[r.Method] {
		refuse(http.StatusMethodNotAllowed, RefusalBody{Error: "method " + r.Method +
			" is not forwarded by the connector proxy"}, "method not allowed")
		return
	}
	// A protocol upgrade (WebSocket, h2c) turns the response into a raw two-way
	// byte stream the redactor never sees. Refused outright.
	if r.Header.Get("Upgrade") != "" {
		refuse(http.StatusBadRequest, RefusalBody{Error: "protocol upgrades are not forwarded by the connector proxy"},
			"upgrade refused")
		return
	}

	acc, err := p.CheckAccess(ctx, caller, connector, "")
	if errors.Is(err, ErrRoleUncertain) {
		w.Header().Set("Retry-After", "5")
		refuse(http.StatusServiceUnavailable, RefusalBody{Error: err.Error()}, "role uncertain")
		return
	}
	if errors.Is(err, ErrTooManyPending) {
		refuse(http.StatusTooManyRequests, RefusalBody{Error: err.Error()}, "too many pending requests")
		return
	}
	if err != nil {
		p.logf("connectors: access check agent=%d connector=%q: %v", caller.AgentID, connector, err)
		refuse(http.StatusInternalServerError, RefusalBody{Error: "could not check access"}, "access check failed")
		return
	}
	if !acc.Usable {
		st := acc.Grant.Status
		msg := "this agent has no approved grant for connector " + connector + ": "
		switch st {
		case StatusPending:
			msg += "a request is pending the operator's approval; retry once it is approved " +
				"(poll GET /agent/connectors/" + connector + ")"
		case StatusDenied:
			msg += "the operator denied the request; file a new one with POST /agent/connectors/" + connector + "/request if it is still needed"
		default:
			msg += "the grant was " + st + "; file a new request with POST /agent/connectors/" + connector + "/request"
		}
		refuse(http.StatusForbidden, RefusalBody{Error: msg, Grant: st, GrantID: acc.Grant.ID}, "no grant: "+st)
		return
	}

	// A virtual connector (the GitHub App) mints its credential for THIS caller
	// and THIS target. The host allowlist is checked FIRST, so a request for a
	// host the connector does not serve mints nothing.
	if v, ok := VirtualOf(ctx, p.Store, connector); ok {
		c := v.Describe()
		if !c.AllowsHost(host) {
			refuse(http.StatusForbidden, RefusalBody{Error: "host " + host + " is not in connector " + connector +
				"'s allowlist (" + strings.Join(c.Hosts, ", ") + ")"}, "host not allowed")
			return
		}
		secret, anonymous, err := v.Credential(ctx, caller, strings.ToLower(host), rest)
		switch {
		case errors.Is(err, ErrTargetRefused):
			refuse(http.StatusForbidden, RefusalBody{Error: err.Error()}, "target refused")
			return
		case err != nil:
			p.logf("connectors: virtual connector %q for agent %q: %v", connector, caller.AgentName, err)
			refuse(http.StatusBadGateway, RefusalBody{Error: "connector " + connector + " could not obtain a credential: " +
				err.Error()}, "credential unavailable")
			return
		}
		// anonymous: a target outside the agent's own repository goes to the
		// platform with NO credential (a public dependency clones as for anyone).
		var onStatus func(int)
		if !anonymous {
			onStatus = func(code int) {
				if code == http.StatusUnauthorized {
					v.Rejected(caller)
				}
			}
		}
		p.forward(w, r, caller, acc, c, secret, anonymous, onStatus, connector, host, rest, &status, &detail, refuse)
		return
	}

	c, secret, err := p.Store.Credential(ctx, connector)
	switch {
	case errors.Is(err, ErrNotFound):
		refuse(http.StatusNotFound, RefusalBody{Error: "connector " + connector + " does not exist"}, "no connector")
		return
	case errors.Is(err, ErrDecrypt):
		p.logf("connectors: connector %q: %v", connector, err)
		refuse(http.StatusBadGateway, RefusalBody{Error: "the connector's credential could not be decrypted " +
			"(the key changed, or its hosts or auth settings were altered outside muster); the operator must " +
			"delete and re-create it"}, "decrypt failed")
		return
	case err != nil:
		p.logf("connectors: load connector %q: %v", connector, err)
		refuse(http.StatusInternalServerError, RefusalBody{Error: "could not load the connector"}, "load failed")
		return
	}
	if !c.AllowsHost(host) {
		refuse(http.StatusForbidden, RefusalBody{Error: "host " + host + " is not in connector " + connector +
			"'s allowlist (" + strings.Join(c.Hosts, ", ") + ")"}, "host not allowed")
		return
	}
	p.forward(w, r, caller, acc, c, secret, false, nil, connector, host, rest, &status, &detail, refuse)
}

// forward sends the request upstream with the credential injected and relays the
// answer. Serve has already decided access, the connector and the secret; status
// and detail are Serve's audit fields, written here as the exchange resolves.
//
// anonymous forwards with NO credential: nothing is injected (the caller's own
// Authorization is still dropped), and there is nothing to redact. onStatus, when
// set, is told the upstream's status.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, caller Caller, acc Access, c Connector, secret string,
	anonymous bool, onStatus func(int), connector, host, rest string, status *int, detail *string,
	refuse func(int, RefusalBody, string)) {
	ctx := r.Context()
	host = strings.ToLower(host)

	scheme := "https"
	if p.Policy.InsecureHTTP {
		scheme = "http"
	}
	target := &url.URL{Scheme: scheme, Host: host}
	if unescaped, err := url.PathUnescape("/" + rest); err == nil {
		target.Path = unescaped
		target.RawPath = "/" + rest
	} else {
		refuse(http.StatusBadRequest, RefusalBody{Error: "malformed proxy path"}, "bad escape")
		return
	}
	// The URL is BUILT from the allowlisted host, never parsed out of the path,
	// so nothing in the path can change where the request goes.
	target.RawQuery = r.URL.RawQuery

	authName, authValue := authHeader(c, secret)
	var red *redactor
	if anonymous {
		red = newRedactor(nil)
	} else {
		red = newRedactor(secretForms(c, secret))
	}
	prefix := "/connect/" + connector + "/"

	// The upstream request runs on outCtx, which only an event stream's shutdown
	// end cancels (ModifyResponse below); r.Context() still cancels it as before.
	outCtx, cancelOut := context.WithCancel(ctx)
	defer cancelOut()
	var endedAtShutdown atomic.Bool
	stopEnd := func() bool { return false }
	// 🔴 DEFERRED, AND THROUGH A CLOSURE. A client going away (the normal end of
	// an EventSource feed) or an upstream reset makes ReverseProxy panic with
	// http.ErrAbortHandler, which unwinds past every plain statement after
	// rp.ServeHTTP — a plain call left the registration on StreamsEnd, which
	// lives as long as the process, one more per reconnect. The closure reads
	// stopEnd at return time, after ModifyResponse has replaced it. Pinned by
	// TestAnAbortedEventStreamLeavesNothingRegisteredOnStreamsEnd.
	defer func() { stopEnd() }()

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = target
			pr.Out.Host = host
			for _, h := range droppedRequestHeaders {
				pr.Out.Header.Del(h)
			}
			// Set REPLACES every value the caller sent under this name.
			if !anonymous {
				pr.Out.Header.Set(authName, authValue)
			}
			// identity, STATED: with no Accept-Encoding at all Go's transport asks
			// for gzip itself; a stated one turns that off, so the body the
			// redactor scans is the bytes the upstream sent, uncompressed.
			pr.Out.Header.Set("Accept-Encoding", "identity")
		},
		Transport: p.upstreamTransport(),
		ModifyResponse: func(resp *http.Response) error {
			*status = resp.StatusCode
			if onStatus != nil {
				onStatus(resp.StatusCode)
			}
			if resp.StatusCode == http.StatusSwitchingProtocols {
				return errSwitchedProtocols
			}
			// The redactor scans bytes. A body compressed despite
			// `Accept-Encoding: identity` would be scanned compressed, so it is
			// refused rather than forwarded unread.
			if ce := strings.TrimSpace(resp.Header.Get("Content-Encoding")); ce != "" && !strings.EqualFold(ce, "identity") {
				return errEncodedResponse
			}
			resp.Header.Del("Set-Cookie")
			if loc := resp.Header.Get("Location"); loc != "" {
				rewritten, ok := rewriteLocation(loc, target, c, prefix)
				if !ok {
					return errOffHostRedirect
				}
				resp.Header.Set("Location", rewritten)
			}
			// No trailers: nothing announces them, and trailerlessWriter drops
			// any the upstream sends anyway.
			resp.Header.Del("Trailer")
			resp.Trailer = nil
			// Decided on the upstream's own Content-Type, before the redactor
			// touches any header; INSIDE the redactor's wrap, so the redactor sees
			// a clean EOF and releases what it was holding back.
			if p.StreamsEnd != nil && endsAtShutdown(r.Method, resp.Header) {
				resp.Body = &endOnShutdown{ReadCloser: resp.Body, ended: &endedAtShutdown}
				stopEnd = context.AfterFunc(p.StreamsEnd, func() {
					endedAtShutdown.Store(true) // before the cancel, so the failed Read sees it
					cancelOut()
				})
			}
			red.headers(resp.Header)
			resp.Body = red.reader(resp.Body)
			resp.Header.Del("Content-Length")
			resp.ContentLength = -1
			return nil
		},
		ErrorHandler: func(_ http.ResponseWriter, _ *http.Request, err error) {
			why := "upstream unreachable"
			switch {
			case errors.Is(err, ErrForbiddenAddress):
				why = "upstream resolves to a non-public address"
			case errors.Is(err, errEncodedResponse):
				why = "the upstream answered with a compressed body although identity was requested; " +
					"muster does not forward a body it cannot inspect"
			case errors.Is(err, errOffHostRedirect):
				why = "the upstream redirected to a host outside connector " + connector + "'s allowlist (" +
					strings.Join(c.Hosts, ", ") + "); muster neither follows nor forwards such a redirect"
			case errors.Is(err, errSwitchedProtocols):
				why = "the upstream tried to switch protocols; refused"
			}
			p.logf("connectors: proxy agent=%q connector=%q host=%q: %s", caller.AgentName, connector, host, why)
			refuse(http.StatusBadGateway, RefusalBody{Error: why}, why)
		},
		FlushInterval: -1,
	}
	// 🔴 FULL DUPLEX, OR A PROXIED REQUEST WITH A BODY CAN BE TORN AND DELIVERED
	// AS A CLEAN, SHORT 200. net/http's HTTP/1 server, when it writes a response
	// head, consumes what is left of the request body and CLOSES it unless the
	// handler enabled full duplex. This handler writes the head as soon as the
	// upstream's arrives, while the upstream transport may still be reading that
	// same body to forward it (at least its trailing EOF read). That read then
	// fails ("invalid Read on closed Body"), the transport's write loop closes the
	// upstream connection, ReverseProxy aborts, and api's recoverMiddleware lets
	// the response finish: the client reads a truncated body with no error, and
	// the upstream received a truncated request. MEASURED without this call:
	// cmd/muster-server's TestAProxiedPOSTIsNotTornWhenTheUpstreamAnswersBeforeTheBodyEnds,
	// which forces the order, failed 60/60 (30 with -race, 30 without), and the
	// shutdown POST test failed 4/240, 10/240 and 4/240 in three stress batches.
	// With it: 0/60, and 0/240 in each of three batches.
	//
	// The error is handled, not dropped: HTTP/2 always runs full duplex and
	// answers nil, and so does the server's own HTTP/1 writer reached through
	// api's statusRecorder.Unwrap. Anything else means a ResponseWriter in the
	// chain hides the server's (a wrapper with no Unwrap, a test recorder), and
	// then the race above is OPEN for this request — said in the log rather than
	// silently, because nothing else would show it.
	if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
		p.logf("connectors: proxy agent=%q connector=%q host=%q: full duplex unavailable (%v); "+
			"a request body still being forwarded when the upstream answers may be cut off",
			caller.AgentName, connector, host, err)
	}
	rp.ServeHTTP(&trailerlessWriter{w: w}, r.WithContext(outCtx))
	if endedAtShutdown.Load() {
		*detail = "event stream ended at server shutdown"
	}

	bg := context.WithoutCancel(ctx)
	if err := p.Store.TouchGrant(bg, acc.Grant.ID); err != nil {
		p.logf("connectors: touch grant %d: %v", acc.Grant.ID, err)
	}
	if err := p.Store.TouchConnector(bg, connector); err != nil {
		p.logf("connectors: touch connector %q: %v", connector, err)
	}
}

// upstreamTransport is built ONCE, on the first request, and shared by every
// request after it — concurrent Serves included.
//
// It never returns nil: a nil RoundTripper would make httputil.ReverseProxy fall
// back to http.DefaultTransport, which has none of Policy's address checks.
func (p *Proxy) upstreamTransport() http.RoundTripper {
	if p.transport != nil {
		return p.transport
	}
	p.transportOnce.Do(func() { p.built = p.Policy.Transport() })
	return p.built
}

// trailerlessWriter is the ResponseWriter the reverse proxy writes to. It lets
// exactly ONE final response through, and nothing around it:
//
//   - a 1xx informational response (103 Early Hints and the like) is
//     SWALLOWED. httputil.ReverseProxy relays those from a client trace straight
//     to the writer, never through ModifyResponse, so their headers would reach
//     the client unredacted — and a 103 may carry any header, Set-Cookie
//     included.
//   - once the status line is written, Header() returns a scratch map, so a
//     trailer (which ReverseProxy sets on the header map AFTER the body) goes
//     nowhere. Trailers are not redacted, so they are not forwarded.
type trailerlessWriter struct {
	w       http.ResponseWriter
	wrote   bool
	scratch http.Header
}

func (t *trailerlessWriter) Header() http.Header {
	if t.wrote {
		if t.scratch == nil {
			t.scratch = http.Header{}
		}
		return t.scratch
	}
	return t.w.Header()
}

func (t *trailerlessWriter) WriteHeader(code int) {
	if code >= 100 && code < 200 {
		return
	}
	if !t.wrote {
		t.wrote = true
		t.w.WriteHeader(code)
	}
}

func (t *trailerlessWriter) Write(b []byte) (int, error) {
	if !t.wrote {
		t.WriteHeader(http.StatusOK)
	}
	return t.w.Write(b)
}

// Flush keeps streaming (FlushInterval -1) working through the wrapper.
func (t *trailerlessWriter) Flush() {
	if f, ok := t.w.(http.Flusher); ok {
		f.Flush()
	}
}

// endsAtShutdown reports whether a proxied exchange is one shutdown ENDS rather
// than drains: a GET answered with text/event-stream — an EventSource-style
// feed. Every other proxied exchange is drained like any request.
//
// 🔴 THE MEDIA TYPE IS NECESSARY BUT NOT SUFFICIENT; THE METHOD IS WHAT MAKES
// ENDING IT HARMLESS. Ending a stream is safe only when it has no "finished"
// state and its client resumes by reconnecting — which is what a GET
// EventSource feed is (WHATWG EventSource reconnects, with Last-Event-ID), and
// the same reason api.Server.EndStreams may end /events. A POST answered with
// text/event-stream is a different thing: an LLM API call with stream:true, or
// an MCP Streamable-HTTP tool-call response, is ONE finite answer streamed in
// pieces, and it is not resumable. Ending it mid-body cuts a turn the upstream
// may already have acted on — and because endOnShutdown reports the end as
// io.EOF, the client would read a truncated answer as a COMPLETE one. So any
// method but GET is drained, whatever it streams.
//
// Why the media type rather than something else: the flush mode is
// FlushInterval -1 for every proxied response, and ModifyResponse forces
// ContentLength -1 on every one too (the redactor changes lengths), so neither
// marks a stream. An unknown-length body is not a stream either: a git fetch's
// pack arrives exactly that way, is finite, and truncating it breaks the
// clone. The media-type test is httputil.ReverseProxy's own for "this response
// is a stream" (mime.ParseMediaType, parameters and case ignored).
//
// ⚠ RESIDUALS, all drained and so all able to hold a shutdown to its deadline:
// a long-lived GET of any OTHER type (an ndjson watch, a chunked long-poll),
// and a long-lived non-GET stream. Which platforms answer that way was not
// surveyed; widening the set means naming an exchange whose truncation is as
// harmless as an EventSource feed's.
//
// ⚠ AND ONE ENDED CLASS LOSES DATA, BY DECISION: a GET event stream that
// carries FINITE answers. The original MCP HTTP+SSE transport delivers
// tool-call responses on its GET stream, and some result-streaming APIs do
// the same. Ending such a stream drops a response still in flight — bounded to
// what would have arrived inside the drain window, which is all a drain could
// have saved. The operator chose to end streams anyway; there is deliberately
// NO grace period. The current MCP transport (Streamable HTTP) answers on the
// POST, so it is drained, not ended.
func endsAtShutdown(method string, h http.Header) bool {
	if method != http.MethodGet {
		return false
	}
	mt, _, _ := mime.ParseMediaType(h.Get("Content-Type"))
	return mt == "text/event-stream"
}

// endOnShutdown wraps an ending exchange's upstream body (endsAtShutdown). Once ended is set (by
// Proxy.StreamsEnd firing, which also cancels the upstream request), the read
// error that cancellation causes is reported as io.EOF.
//
// 🔴 THE EOF IS WHAT MAKES THE END CLEAN RATHER THAN AN ABORT. A non-EOF error
// from the body (context.Canceled, MEASURED) makes httputil.ReverseProxy panic
// with http.ErrAbortHandler under a real server. In muster that panic does not
// even reach net/http: api's recoverMiddleware catches it, logs a PANIC with a
// stack and counts muster's panic metric — on every shutdown with a stream
// open — and Serve's grant/connector touches after ServeHTTP never run. The
// client cannot tell (the middleware lets the response finish, so it reads EOF
// either way), which is why cmd/muster-server's test reads the panic counter.
// As EOF the redactor releases what it held back and Serve returns normally.
type endOnShutdown struct {
	io.ReadCloser
	ended *atomic.Bool
}

func (b *endOnShutdown) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && err != io.EOF && b.ended.Load() {
		err = io.EOF
	}
	return n, err
}

// authHeader is the header name and value that carries the credential.
func authHeader(c Connector, secret string) (string, string) {
	switch c.Scheme {
	case SchemeHeader:
		return c.HeaderName, secret
	case SchemeBasic:
		return "Authorization", "Basic " + base64.StdEncoding.EncodeToString([]byte(c.Username+":"+secret))
	default:
		return "Authorization", "Bearer " + secret
	}
}

// secretForms are the byte strings that must never appear in a response: the
// credential and the re-encodings of it a reflecting upstream is likely to
// produce.
//
// 🔴 WHAT REDACTION IS, AND IS NOT. It is DEFENCE IN DEPTH against an upstream
// that reflects the request back (an echo endpoint, an error page quoting a
// header, a debug field). It is NOT a proof that the credential cannot leave
// through a response. It covers, for the secret and (basic auth) the
// `user:secret` pair:
//
//   - the plain form, matched ASCII-case-insensitively;
//   - URL encoding (query and path escaping, and every byte %XX-encoded), hex,
//     and JSON string escaping (with and without HTML escaping, and with `\/`),
//     each matched case-insensitively;
//   - base64, standard and URL alphabets, at ALL THREE byte alignments, so the
//     credential is caught inside a larger base64 blob (e.g. a whole reflected
//     header encoded), not only when it starts one. An unaligned match leaves
//     at most the one or two boundary characters it shares with its neighbours.
//
// Together with what Serve refuses outright — Range/If-Range (slicing), TRACE,
// protocol upgrades, 1xx responses, trailers, and compressed bodies — that
// closes the bypasses measured against it. It does NOT cover a transformation
// it does not know: double encodings, compression inside the body (a gzip
// FILE is still bytes), encryption, hashing, reordering, an upstream that
// splits the value across responses on its own, or one that answers a request
// the agent shaped to extract it piecewise (e.g. "character N of header X").
// The real guarantees are elsewhere: the secret is never in an agent's pod, and
// only an operator-approved agent reaches only the connector's own hosts.
func secretForms(c Connector, secret string) []needle {
	primaries := []string{secret}
	if c.Scheme == SchemeBasic {
		primaries = append(primaries, c.Username+":"+secret)
	}
	var out []needle
	for _, p := range primaries {
		out = append(out, needle{s: p, fold: true})
		for _, f := range textEncodings(p) {
			out = append(out, needle{s: f, fold: true})
		}
		for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
			for off := 0; off < 3; off++ {
				out = append(out, needle{s: base64Core(enc, p, off)})
			}
		}
	}
	return out
}

// textEncodings are the case-insensitive re-encodings of s.
func textEncodings(s string) []string {
	out := []string{url.QueryEscape(s), url.PathEscape(s), hex.EncodeToString([]byte(s))}
	var all strings.Builder
	for _, b := range []byte(s) {
		fmt.Fprintf(&all, "%%%02X", b)
	}
	out = append(out, all.String())
	for _, html := range []bool{true, false} {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(html)
		_ = enc.Encode(s)
		j := strings.TrimSuffix(strings.TrimSpace(buf.String()), `"`)
		j = strings.TrimPrefix(j, `"`)
		out = append(out, j, strings.ReplaceAll(j, "/", `\/`))
	}
	return out
}

// base64Core is the part of s's base64 encoding that is the SAME wherever s
// sits in a larger encoded blob, given that s starts off bytes (0-2) after a
// 3-byte boundary: the characters made only of s's own bits.
func base64Core(enc *base64.Encoding, s string, off int) string {
	buf := append(make([]byte, off), s...)
	full := enc.EncodeToString(buf)
	start := (8*off + 5) / 6  // first character with no prefix bits
	end := (8 * len(buf)) / 6 // last character with no unknown suffix bits, exclusive
	if end > len(full) {
		end = len(full)
	}
	if start >= end {
		return ""
	}
	return full[start:end]
}

// rewriteLocation maps a redirect to one of the connector's hosts back into the
// proxy, so the client's next request comes through muster again. ok is false
// for a redirect ANYWHERE ELSE — another host, another scheme, userinfo, or a
// Location that does not parse — and the caller answers 502 with no Location.
//
// 🔴 AN OFF-ALLOWLIST LOCATION IS NEVER HANDED TO THE CLIENT, NOT EVEN UNCHANGED.
// The client sent this request with its own muster token in a header, and HTTP
// clients carry custom headers across a cross-host redirect (curl -L resends
// every -H header; git resends http.extraHeader — both measured). Passing the
// Location through would have the client deliver its muster token to whatever
// host the upstream named. The proxy itself never follows a redirect.
func rewriteLocation(loc string, base *url.URL, c Connector, prefix string) (string, bool) {
	u, err := url.Parse(loc)
	if err != nil {
		return "", false
	}
	abs := base.ResolveReference(u)
	if abs.Scheme != base.Scheme || abs.User != nil || !c.AllowsHost(abs.Host) {
		return "", false
	}
	out := prefix + strings.ToLower(abs.Host) + abs.EscapedPath()
	if abs.RawQuery != "" {
		out += "?" + abs.RawQuery
	}
	return out, true
}

// RedactedMarker replaces every occurrence of the credential in a response.
const RedactedMarker = "[REDACTED-BY-MUSTER]"

// needle is one form the redactor removes. fold matches it ASCII
// case-insensitively.
type needle struct {
	s    string
	fold bool
}

// minNeedleLen is the shortest form the redactor will look for: anything
// shorter would mangle ordinary bodies while protecting nothing (a secret is at
// least MinSecretLen bytes, so every real form is longer).
const minNeedleLen = 6

// redactor removes the credential's forms from response headers and bodies.
type redactor struct {
	needles []needle // fold needles are stored lowercased
	maxLen  int
}

func newRedactor(forms []needle) *redactor {
	r := &redactor{}
	seen := map[needle]bool{}
	for _, f := range forms {
		if f.fold {
			f.s = string(asciiLower([]byte(f.s)))
		}
		if len(f.s) < minNeedleLen || seen[f] {
			continue
		}
		seen[f] = true
		r.needles = append(r.needles, f)
		if len(f.s) > r.maxLen {
			r.maxLen = len(f.s)
		}
	}
	return r
}

func asciiLower(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return out
}

// replace returns b with every byte covered by any needle's match replaced:
// overlapping and adjacent matches collapse into ONE marker, so no needle is
// left half-replaced by another's replacement.
func (r *redactor) replace(b []byte) []byte {
	type span struct{ s, e int }
	var spans []span
	var lower []byte
	for _, n := range r.needles {
		hay := b
		if n.fold {
			if lower == nil {
				lower = asciiLower(b)
			}
			hay = lower
		}
		nb := []byte(n.s)
		for off := 0; off <= len(hay)-len(nb); {
			i := bytes.Index(hay[off:], nb)
			if i < 0 {
				break
			}
			spans = append(spans, span{off + i, off + i + len(nb)})
			off += i + 1
		}
	}
	if len(spans) == 0 {
		return b
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].s < spans[j].s })
	out := make([]byte, 0, len(b))
	cur := spans[0]
	pos := 0
	flush := func(sp span) {
		out = append(out, b[pos:sp.s]...)
		out = append(out, RedactedMarker...)
		pos = sp.e
	}
	for _, sp := range spans[1:] {
		if sp.s <= cur.e {
			if sp.e > cur.e {
				cur.e = sp.e
			}
			continue
		}
		flush(cur)
		cur = sp
	}
	flush(cur)
	return append(out, b[pos:]...)
}

func (r *redactor) headers(h http.Header) {
	for k, vs := range h {
		for i, v := range vs {
			vs[i] = string(r.replace([]byte(v)))
		}
		h[k] = vs
	}
}

func (r *redactor) reader(rc io.ReadCloser) io.ReadCloser {
	return &redactingReader{src: rc, red: r}
}

// redactingReader streams src with every needle replaced, holding back just
// enough bytes (the longest needle, less one) that a needle split across two
// reads is still caught.
type redactingReader struct {
	src     io.ReadCloser
	red     *redactor
	pending []byte // read from src, not yet scanned to completion
	out     []byte // scanned, ready to return
	eof     bool
}

func (rr *redactingReader) Read(p []byte) (int, error) {
	for len(rr.out) == 0 {
		if rr.eof {
			if len(rr.pending) > 0 {
				rr.out = rr.red.replace(rr.pending)
				rr.pending = nil
				continue
			}
			return 0, io.EOF
		}
		buf := make([]byte, 32*1024)
		n, err := rr.src.Read(buf)
		rr.pending = append(rr.pending, buf[:n]...)
		if err == io.EOF {
			rr.eof = true
		} else if err != nil {
			return 0, err
		}
		rr.pending = rr.red.replace(rr.pending)
		keep := rr.red.maxLen - 1
		if keep < 0 {
			keep = 0
		}
		if !rr.eof && len(rr.pending) > keep {
			// A needle not yet complete in pending starts within the last keep
			// bytes, so everything before them is final.
			cut := len(rr.pending) - keep
			rr.out = append(rr.out, rr.pending[:cut]...)
			rr.pending = append([]byte(nil), rr.pending[cut:]...)
		}
	}
	n := copy(p, rr.out)
	rr.out = rr.out[n:]
	return n, nil
}

func (rr *redactingReader) Close() error { return rr.src.Close() }
