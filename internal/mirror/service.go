package mirror

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/stepup"
)

const (
	// TokenRecheck is how often a live connection's token is re-read, so a
	// revoked token ends a connection (and its open terminals) that is already up.
	TokenRecheck = 30 * time.Second
	// RecheckFailClosedAfter is how many CONSECUTIVE failed rechecks (a database
	// error, not a revocation) end the connection. One is tolerated — a blip —
	// but a token that cannot be re-verified is not kept live indefinitely.
	RecheckFailClosedAfter = 2
	// authFailLimit is how many failed token checks one client address may make
	// per authFailWindow before /mirror/connect answers 429 until the window
	// ends.
	authFailLimit  = 10
	authFailWindow = time.Minute
)

// ErrNoPrincipal: a view or attach was asked for with no user on the context.
var ErrNoPrincipal = errors.New("mirror: no user principal on this request")

// stepUpRequired is ErrStepUpRequired's type: stepup.ErrStale under this
// package's prefix. A type rather than fmt.Errorf("%w") because ErrStale's text
// names the CONFIGURED window and Errorf would freeze it at package load.
type stepUpRequired struct{}

func (stepUpRequired) Error() string { return "mirror: " + stepup.ErrStale.Error() }
func (stepUpRequired) Unwrap() error { return stepup.ErrStale }

// ErrStepUpRequired: an interactive attach, or a paste, on a session that did
// not sign in within stepup.Window.
var ErrStepUpRequired error = stepUpRequired{}

// Service is the mirror plugin as internal/api consumes it: the connect
// endpoint, the viewing principal's hosts, and terminal attach.
type Service struct {
	hub    *Hub
	store  Store
	logger *log.Logger

	// recheck is TokenRecheck; a test shortens it.
	recheck time.Duration

	limiter *failLimiter
}

// NewService builds the plugin over store.
func NewService(store Store, logger *log.Logger) *Service {
	if logger == nil {
		logger = log.Default()
	}
	return &Service{store: store, logger: logger, recheck: TokenRecheck, hub: NewHub(logger),
		limiter: newFailLimiter(authFailLimit, authFailWindow)}
}

// SetNotify installs the Agents-view refresh nudge (internal/api's
// agents-changed broadcast), called with the OWNER of the host that changed:
// the event reaches only that owner's open views (accounts PR 3b). It carries
// no data: it only tells them to re-fetch, and each re-fetch is scoped to its
// own principal.
func (s *Service) SetNotify(fn func(owner int64)) { s.hub.SetNotify(fn) }

// SetNotifyDebounce overrides the nudge coalescing window (tests).
func (s *Service) SetNotifyDebounce(d time.Duration) {
	s.hub.mu.Lock()
	s.hub.debounce = d
	s.hub.mu.Unlock()
}

// SetActivityNudgeEvery overrides activityNudgeEvery, the per-owner throttle on
// activity-only nudges (tests).
func (s *Service) SetActivityNudgeEvery(d time.Duration) {
	s.hub.mu.Lock()
	s.hub.activityEvery = d
	s.hub.mu.Unlock()
}

// Hub is the service's hub (tests).
func (s *Service) Hub() *Hub { return s.hub }

// SetTokenRecheck overrides TokenRecheck (tests).
func (s *Service) SetTokenRecheck(d time.Duration) { s.recheck = d }

// bearer is the request's `Authorization: Bearer` value.
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if v, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// clientAddr is the failure counter's key: the TCP peer.
//
// ⚠ NOT X-Forwarded-For, which any client can write. Behind the public path
// (Traefik → nginx relay → gateway → muster) every client therefore shares ONE
// address and one failure count — which is safe ONLY because the count never
// refuses a VALID token (see ServeConnect): a flood of bad tokens from anyone
// earns those bad tokens 429s, and a legitimate host's reconnect is unaffected.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ServeConnect is GET /mirror/connect: authenticate the host's token, upgrade,
// and run the connection.
//
// 🔴 THE TOKEN IS THE ONLY DOOR, AND IT IS A MIRROR TOKEN OR NOTHING. The route
// is not behind the browser session or the hook token: a host is neither. It is
// looked up by hash in mirror_tokens and nowhere else, so the hook, service and
// agent tokens are refused here, and a mirror token opens no other route (no
// other tier consults this table).
//
// 🔴 THE LOOKUP RUNS UNDER authz.System: mirror_tokens is under row-level
// security and nobody is known until the token is found. What the token yields
// — its OWNER and HOST — is everything the connection is then allowed: its
// sessions are registered under that owner only.
//
// 🔴 THE TOKEN IS LOOKED UP FIRST, AND A VALID TOKEN IS NEVER REFUSED BY THE
// FAILURE LIMIT. The limit used to be checked before the lookup, keyed on the
// TCP peer — which behind the public path is one address for everyone — so ~10
// bad tokens a minute from anyone kept every legitimate host's reconnect
// refused. Now:
//   - a string that is not even SHAPED like a mirror token (TokenShapeOK) is
//     refused without touching the database — that, not the limiter, is what
//     keeps junk off the database;
//   - a well-shaped token is always looked up (one indexed read by unique hash);
//     a valid one proceeds whatever the failure count says;
//   - only FAILURES count, and an address past the limit gets 429 instead of
//     401 for its failures — a signal to a misconfigured host that it is
//     hammering, not a lock on anyone. The secret's strength (256 bits) is the
//     guessing defence.
func (s *Service) ServeConnect(w http.ResponseWriter, r *http.Request) {
	addr := clientAddr(r)
	refuse := func() {
		s.limiter.fail(addr)
		if s.limiter.blocked(addr) {
			http.Error(w, "mirror: too many failed token checks from this address; wait a minute", http.StatusTooManyRequests)
			return
		}
		http.Error(w, ErrBadToken.Error(), http.StatusUnauthorized)
	}
	presented := bearer(r)
	if !TokenShapeOK(presented) {
		refuse()
		return
	}
	tok, err := s.store.Authenticate(authz.System(r.Context()), presented)
	if err != nil {
		if errors.Is(err, ErrBadToken) {
			refuse()
			return
		}
		s.logger.Printf("mirror: authenticate: %v", err)
		http.Error(w, "mirror: could not check the token", http.StatusServiceUnavailable)
		return
	}
	// No OriginPatterns: a host sends no Origin, and a browser page cannot set
	// the Authorization header on a WebSocket, so a cross-site page never gets
	// this far. Accept still refuses a cross-origin Origin if one is sent.
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.logger.Printf("mirror: accept %s: %v", tok.Host, err)
		return
	}
	defer ws.CloseNow()

	// A hijacked connection's request context does not end with the TCP
	// connection, and nothing else ends this one on revocation, so its life is
	// its own context: cancelled by the token check below or by the read loop.
	ctx, cancel := context.WithCancelCause(context.WithoutCancel(r.Context()))
	defer cancel(nil)
	go s.watchToken(ctx, cancel, tok)
	err = s.hub.ServeConn(ctx, tok.OwnerID, tok.Host, ws)
	s.logger.Printf("mirror: host %s connection ended: %v", tok.Host, err)
}

// watchToken ends the connection once its token is revoked — or once it cannot
// be re-verified RecheckFailClosedAfter times in a row (fail closed). Ending the
// connection ends every terminal open on it (Hub.drop).
func (s *Service) watchToken(ctx context.Context, cancel context.CancelCauseFunc, tok Token) {
	sys := authz.System(ctx)
	t := time.NewTicker(s.recheck)
	defer t.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			live, err := s.store.TokenLive(sys, tok.ID)
			if err != nil {
				failures++
				s.logger.Printf("mirror: recheck token %d (%s) failed (%d/%d): %v", tok.ID, tok.Host, failures, RecheckFailClosedAfter, err)
				if failures >= RecheckFailClosedAfter {
					cancel(fmt.Errorf("mirror: token %d for host %s could not be re-verified %d times in a row; closing (fail closed)", tok.ID, tok.Host, failures))
					return
				}
				continue
			}
			failures = 0
			if !live {
				cancel(fmt.Errorf("%w (token %d, host %s)", ErrTokenRevoked, tok.ID, tok.Host))
				return
			}
		}
	}
}

// owner is the viewing principal's user, or ErrNoPrincipal.
//
// 🔴 authz.System IS NOT A VIEWER. Its UserID is 0, and a host's sessions belong
// to a user; "everyone's" is not a view this plugin offers.
func owner(ctx context.Context) (int64, error) {
	p, ok := authz.From(ctx)
	if !ok || p.All() || p.UserID <= 0 {
		return 0, ErrNoPrincipal
	}
	return p.UserID, nil
}

// Hosts is the viewing principal's hosts: connected, or recently disconnected.
// Another user's hosts are never in it.
func (s *Service) Hosts(ctx context.Context) []HostView {
	o, err := owner(ctx)
	if err != nil {
		return nil
	}
	return s.hub.Hosts(o)
}

// Attach attaches a terminal to one of the viewing principal's host sessions.
// See Hub.Attach.
//
// 🔴 AN INTERACTIVE ATTACH NEEDS A FRESH SIGN-IN (internal/stepup), checked
// HERE as well as by internal/api before its socket upgrade: the plugin does
// not rely on its caller to have asked. A context with no sign-in time is never
// fresh, so a caller that forgot to carry one gets read-only or nothing.
func (s *Service) Attach(ctx context.Context, host, session string, readOnly bool, stdin io.Reader, stdout io.Writer, resize <-chan provision.TermSize) error {
	return s.AttachWindow(ctx, host, session, "", readOnly, stdin, stdout, resize)
}

// AttachWindow is Attach on one window of the session (its tmux window id);
// window == "" is the session's active window. See Hub.AttachWindow.
func (s *Service) AttachWindow(ctx context.Context, host, session, window string, readOnly bool, stdin io.Reader, stdout io.Writer, resize <-chan provision.TermSize) error {
	o, err := owner(ctx)
	if err != nil {
		return err
	}
	if !readOnly && !stepup.Fresh(ctx, s.hub.now()) {
		return ErrStepUpRequired
	}
	return s.hub.AttachWindow(ctx, o, host, session, window, readOnly, stdin, stdout, resize)
}

// Watch streams one of the viewing principal's host sessions' Claude Code
// transcript. See Hub.Watch.
func (s *Service) Watch(ctx context.Context, host, session, gen string, offset int64, out func(Msg) error) error {
	return s.WatchWindow(ctx, host, session, "", gen, offset, out)
}

// WatchWindow is Watch on one window of the session; window == "" is the
// session's active window. See Hub.WatchWindow.
func (s *Service) WatchWindow(ctx context.Context, host, session, window, gen string, offset int64, out func(Msg) error) error {
	o, err := owner(ctx)
	if err != nil {
		return err
	}
	return s.hub.WatchWindow(ctx, o, host, session, window, gen, offset, out)
}

// Paste types a message into a pane of one of the viewing principal's host
// sessions: its Claude pane, or its active pane (target). See Hub.Paste.
func (s *Service) Paste(ctx context.Context, host, session, text string, target PasteTarget) error {
	return s.PasteWindow(ctx, host, session, "", text, target)
}

// PasteWindow is Paste into one window of the session; window == "" is the
// session's active window. See Hub.PasteWindow.
func (s *Service) PasteWindow(ctx context.Context, host, session, window, text string, target PasteTarget) error {
	o, err := owner(ctx)
	if err != nil {
		return err
	}
	return s.hub.PasteWindow(ctx, o, host, session, window, text, target)
}

// IsAttachRefusal reports whether err is one of the plugin's own refusals,
// whose text is written for the operator and is passed to the browser as is.
func IsAttachRefusal(err error) bool {
	for _, e := range []error{ErrHostOffline, ErrSessionGone, ErrWindowGone, ErrHostDisconnected, ErrOverrun,
		ErrInteractiveRefused, ErrTooManyStreams, ErrNoPrincipal, ErrStepUpRequired,
		ErrNoRenderedView, ErrNotClaude, ErrViewOverrun, ErrTooManyWatches, ErrPasteTimeout, ErrPasteRefused, ErrBadPaste,
		ErrPanePasteUnsupported} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// failLimiter counts failed token checks per client address in fixed windows.
type failLimiter struct {
	limit  int
	window time.Duration
	now    func() time.Time

	mu sync.Mutex
	by map[string]failWindow
}

type failWindow struct {
	start time.Time
	n     int
}

func newFailLimiter(limit int, window time.Duration) *failLimiter {
	return &failLimiter{limit: limit, window: window, now: time.Now, by: map[string]failWindow{}}
}

func (l *failLimiter) blocked(addr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.by[addr]
	if !ok {
		return false
	}
	if l.now().Sub(w.start) >= l.window {
		delete(l.by, addr)
		return false
	}
	return w.n >= l.limit
}

func (l *failLimiter) fail(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.by) > 4096 { // bound the map: drop expired windows
		for a, w := range l.by {
			if now.Sub(w.start) >= l.window {
				delete(l.by, a)
			}
		}
	}
	w, ok := l.by[addr]
	if !ok || now.Sub(w.start) >= l.window {
		w = failWindow{start: now}
	}
	w.n++
	l.by[addr] = w
}
