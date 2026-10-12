package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/gotrue"
	"github.com/ZacxDev/muster/internal/users"
)

// --- accounts PR 4: GoTrue sessions ------------------------------------------
//
// MUSTER_AUTH_MODE picks the human tier:
//
//   - password (the default): today's shared MUSTER_UI_PASSWORD, one login,
//     user 1. Unchanged by this file.
//   - gotrue: muster's own GoTrue. The browser holds two HttpOnly cookies —
//     muster_at (the access JWT) and muster_rt (the refresh token) — and
//     requireSession verifies the first on every request, refreshing it
//     server-side from the second when it has expired.
//
// 🔴 IN gotrue MODE A BROWSER SESSION IS SERVED ONLY ON THE BROWSER LISTENER.
// muster can be reached two ways: the public edge (TLS, the ingress chain) and
// the plain-HTTP LAN NodePort. The operator decided the LAN port is for machine
// tokens only, so a session cookie never has a reason to cross it. muster
// cannot tell the two apart from a request — Host and X-Forwarded-* are written
// by whoever sends the request — so the distinction is STRUCTURAL: the
// deployment serves the edge from its own port (MUSTER_BROWSER_PORT), cmd/
// muster-server wraps that listener's handler in BrowserListener, and only a
// request carrying that mark may use, mint or end a session
// (TestGoTrueSessionsAreRefusedOffTheBrowserListener). Machine tokens work on
// both listeners.
//
// ⚠ WHAT THE LISTENER SPLIT DOES NOT DO: it does not make the browser port
// unreachable from the LAN. That is the network's job (a NodePort with
// externalTrafficPolicy: Local plus a NetworkPolicy admitting only the edge
// proxy). What it guarantees on its own is that the machine-token port never
// serves, mints or refreshes a browser session — and that the client-IP headers
// (X-Forwarded-For, CF-Connecting-IP) are believed only on the edge port.

// AuthMode is MUSTER_AUTH_MODE.
type AuthMode string

const (
	// AuthModePassword is the shared operator password (the default).
	AuthModePassword AuthMode = "password"
	// AuthModeGoTrue is per-user login through muster's GoTrue.
	AuthModeGoTrue AuthMode = "gotrue"
)

// Cookie names for gotrue mode.
const (
	accessCookieName  = "muster_at"
	refreshCookieName = "muster_rt"
)

// gotrueCookieMaxAge is how long the browser keeps both cookies. The server,
// not the browser, decides whether they are still good: the access token is
// verified on every request and the refresh token is GoTrue's to accept.
const gotrueCookieMaxAge = 30 * 24 * time.Hour

// userCacheTTL bounds how stale a cached sub → user mapping (role, disabled)
// may be. A disable through this process invalidates its entry at once; one
// made elsewhere (another replica, raw SQL) takes effect within this window.
const userCacheTTL = 30 * time.Second

// refreshShareWindow is how long one refresh's result is handed to every
// other request presenting the SAME (now spent) refresh token.
//
// 🔴 WITHOUT IT, A PAGE LOAD WALKS ITS OWN SESSION INTO REVOCATION. A shell
// load fires several requests at once, all carrying the same expired access
// token and the same refresh token. GoTrue rotates on every refresh, accepts a
// replay of the direct PARENT of the live token, and REVOKES THE SESSION on any
// older replay (measured, v2.189.0, rotation on, no reuse interval). Two
// concurrent refreshes of one token, each setting a different new token, leave
// the tab holding a grandparent the next time — and the session dies. So every
// request presenting a token this process has just refreshed gets that one
// result (TestConcurrentRequestsShareOneRefresh).
const refreshShareWindow = 30 * time.Second

// GoTrueAuth is gotrue mode's wiring. cmd/muster-server builds it whenever
// MUSTER_GOTRUE_URL is set — in password mode too, so the admin claim can be
// redeemed before the mode is flipped.
type GoTrueAuth struct {
	Client   *gotrue.Client
	Verifier gotrue.Verifier
	Users    UserStore
}

// UserStore is what the session tier needs from internal/users.
type UserStore interface {
	BySub(ctx context.Context, sub string) (users.User, error)
	ByID(ctx context.Context, id int64) (users.User, error)
	SetDisabled(ctx context.Context, id int64, disabled bool) (users.User, error)
	ClaimInviteLive(ctx context.Context, tokenHash []byte) (bool, error)
	RedeemClaim(ctx context.Context, tokenHash []byte, email string,
		create func(context.Context) (string, error), undo func(sub string)) (users.User, error)
}

// UseGoTrue attaches the GoTrue wiring. Call before Handler.
func (s *Server) UseGoTrue(g GoTrueAuth) {
	s.gt = &gotrueState{GoTrueAuth: g, now: time.Now}
	if g.Verifier.Now != nil {
		s.gt.now = g.Verifier.Now
	}
}

// gotrueState is the GoTrue wiring plus this process's caches.
type gotrueState struct {
	GoTrueAuth
	now func() time.Time

	mu        sync.Mutex
	userCache map[string]cachedUser
	flights   map[string]*refreshFlight
	// revokedSessions: GoTrue session ids signed out through this process,
	// until their access tokens could no longer verify anyway.
	revokedSessions map[string]time.Time
	// notBefore: per subject, the instant of a sign-out-everywhere; any access
	// token issued at or before it is refused — unless its session was signed
	// in through this process AFTER the sign-out (afterSignOut). JWT `iat` is
	// whole seconds, so without that exception a sign-in in the same second as
	// the sign-out would be refused.
	notBefore    map[string]time.Time
	afterSignOut map[string]bool

	failures keyedWindow
	attempts rateLimiter
}

type cachedUser struct {
	u  users.User
	at time.Time
}

type refreshFlight struct {
	done chan struct{}
	sess gotrue.Session
	err  error
	at   time.Time
}

// errRevoked: the access token's session was signed out through muster.
var errRevoked = errors.New("session signed out")

// user resolves a subject to its muster row, through the 30 s cache.
func (g *gotrueState) user(ctx context.Context, sub string) (users.User, error) {
	now := g.now()
	g.mu.Lock()
	if c, ok := g.userCache[sub]; ok && now.Sub(c.at) < userCacheTTL {
		g.mu.Unlock()
		return c.u, nil
	}
	g.mu.Unlock()
	u, err := g.Users.BySub(ctx, sub)
	if err != nil {
		return users.User{}, err
	}
	g.mu.Lock()
	if g.userCache == nil {
		g.userCache = map[string]cachedUser{}
	}
	g.userCache[sub] = cachedUser{u: u, at: now}
	g.mu.Unlock()
	return u, nil
}

// forgetUser drops every cache entry for user id.
func (g *gotrueState) forgetUser(id int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for sub, c := range g.userCache {
		if c.u.ID == id {
			delete(g.userCache, sub)
		}
	}
}

// refresh exchanges rt, sharing one GoTrue call among every request that
// presents the same token within refreshShareWindow.
func (g *gotrueState) refresh(ctx context.Context, clientIP, rt string) (gotrue.Session, error) {
	sum := sha256.Sum256([]byte(rt))
	key := hex.EncodeToString(sum[:])
	now := g.now()
	g.mu.Lock()
	if g.flights == nil {
		g.flights = map[string]*refreshFlight{}
	}
	for k, f := range g.flights {
		select {
		case <-f.done:
			if now.Sub(f.at) > refreshShareWindow {
				delete(g.flights, k)
			}
		default:
		}
	}
	if f, ok := g.flights[key]; ok {
		g.mu.Unlock()
		select {
		case <-f.done:
		case <-ctx.Done():
			return gotrue.Session{}, ctx.Err()
		}
		return f.sess, f.err
	}
	f := &refreshFlight{done: make(chan struct{}), at: now}
	g.flights[key] = f
	g.mu.Unlock()
	// Detached from the request: a client that hangs up mid-refresh must not
	// leave the other waiters with a cancelled result for a token that GoTrue
	// may already have rotated.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	f.sess, f.err = g.Client.Refresh(rctx, clientIP, rt)
	cancel()
	close(f.done)
	return f.sess, f.err
}

// revoke records a sign-out so this process refuses the access tokens it
// covers for the rest of their life. GoTrue revokes the refresh side; an
// access token is a signed statement GoTrue cannot recall, so without this a
// signed-out cookie would keep working until it expired (up to JWT_EXP).
//
// ⚠ IN-MEMORY. A restart forgets it, leaving at most one access-token lifetime
// in which a copied token from before the sign-out still verifies; the refresh
// side stays revoked in GoTrue regardless.
func (g *gotrueState) revoke(c gotrue.Claims, scope gotrue.LogoutScope) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if g.revokedSessions == nil {
		g.revokedSessions = map[string]time.Time{}
		g.notBefore = map[string]time.Time{}
		g.afterSignOut = map[string]bool{}
	}
	// Prune what can no longer matter: anything older than an hour is past
	// every access token's expiry (JWT_EXP is minutes) plus skew.
	for k, t := range g.revokedSessions {
		if now.Sub(t) > time.Hour {
			delete(g.revokedSessions, k)
		}
	}
	for k, t := range g.notBefore {
		if now.Sub(t) > time.Hour {
			delete(g.notBefore, k)
		}
	}
	if c.SessionID != "" {
		g.revokedSessions[c.SessionID] = now
	}
	if scope == gotrue.ScopeGlobal || c.SessionID == "" {
		g.notBefore[c.Subject] = now
		if len(g.afterSignOut) > 10000 {
			g.afterSignOut = map[string]bool{}
		}
	}
}

// signedIn records a session this process just minted (sign-in, claim), so a
// sign-out-everywhere in the same second does not refuse it.
func (g *gotrueState) signedIn(c gotrue.Claims) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if nb, ok := g.notBefore[c.Subject]; ok && c.IssuedAt <= nb.Unix() && c.SessionID != "" {
		g.afterSignOut[c.SessionID] = true
	}
}

func (g *gotrueState) isRevoked(c gotrue.Claims) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.revokedSessions[c.SessionID]; ok && c.SessionID != "" {
		return true
	}
	if nb, ok := g.notBefore[c.Subject]; ok && c.IssuedAt <= nb.Unix() && !g.afterSignOut[c.SessionID] {
		return true
	}
	return false
}

// gotrueSession is a resolved browser session.
type gotrueSession struct {
	user   users.User
	claims gotrue.Claims
	access string
	// refreshed is the new pair when this request had to refresh; the caller
	// must set it as cookies.
	refreshed *gotrue.Session
}

// resolveGoTrueSession reads the two cookies and returns the session they
// carry, refreshing an expired access token from the refresh token. It never
// writes to w; the caller sets refreshed cookies (or clears them).
//
// 🔴 ONLY AN EXPIRED (OR ABSENT) ACCESS TOKEN IS REFRESHED. A token that fails
// for any other reason — another algorithm, a bad signature, the wrong issuer
// or audience, a revoked session — ends the session instead: those are not
// what time does to a good cookie, and "fix it with the refresh token" would
// turn a tampered cookie into a working one.
func (s *Server) resolveGoTrueSession(r *http.Request) (gotrueSession, error) {
	g := s.gt
	at := cookieValue(r, accessCookieName)
	rt := cookieValue(r, refreshCookieName)
	var sess gotrueSession
	var c gotrue.Claims
	err := gotrue.ErrExpired
	if at != "" {
		c, err = g.Verifier.Verify(at)
	}
	if errors.Is(err, gotrue.ErrExpired) && rt != "" {
		fresh, rerr := g.refresh(r.Context(), clientIP(r), rt)
		if rerr != nil {
			return sess, rerr
		}
		at = fresh.AccessToken
		if c, err = g.Verifier.Verify(at); err != nil {
			s.logger.Printf("gotrue: a token GoTrue just issued does not verify (%v) — check "+
				"MUSTER_GOTRUE_JWT_SECRET and the issuer/audience against GoTrue's", err)
			return sess, err
		}
		sess.refreshed = &fresh
	}
	if err != nil {
		return sess, err
	}
	if g.isRevoked(c) {
		return sess, errRevoked
	}
	u, err := g.user(r.Context(), c.Subject)
	if err != nil {
		return sess, err
	}
	if u.Disabled() {
		return sess, errDisabled
	}
	sess.user, sess.claims, sess.access = u, c, at
	return sess, nil
}

var errDisabled = errors.New("user is disabled")

// requireGoTrueSession is requireSession in gotrue mode.
func (s *Server) requireGoTrueSession(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	if s.gt == nil {
		// Fail closed: gotrue mode with no GoTrue wiring serves nothing.
		// cmd/muster-server refuses to boot this way; this is the second wall.
		if isDocumentNavigation(r) {
			http.Redirect(w, r, loginPath, http.StatusSeeOther)
			return
		}
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the web UI is not configured for login (GoTrue)"})
		return
	}
	if !viaBrowserListener(r) {
		s.refuseOffBrowserListener(w, r)
		return
	}
	sess, err := s.resolveGoTrueSession(r)
	if err != nil {
		s.clearGoTrueCookies(w)
		s.refuseUnauthenticated(w, r)
		return
	}
	if sess.refreshed != nil {
		s.setGoTrueCookies(w, *sess.refreshed)
	}
	s.admit(w, r, next, authz.Principal{UserID: sess.user.ID, Role: sess.user.Role, Via: authz.ViaSession})
}

// OffBrowserListenerRefusal is the error a browser-session route answers on
// the machine-token listener in gotrue mode.
const OffBrowserListenerRefusal = "browser sign-in is not served on this port; it serves machine tokens only"

// refuseOffBrowserListener answers a browser-session request that arrived on
// the machine-token listener. 403, not 401: no credential would be accepted
// here, so "sign in" is not the remedy; going to the public address is.
func (s *Server) refuseOffBrowserListener(w http.ResponseWriter, r *http.Request) {
	if isDocumentNavigation(r) {
		s.renderNotice(w, http.StatusForbidden, "Sign in at the public address",
			"This port serves muster's machine API only. Open muster at its public address to sign in"+
				s.publicURLSuffix()+".")
		return
	}
	s.writeJSON(w, http.StatusForbidden, map[string]any{"error": OffBrowserListenerRefusal})
}

func (s *Server) publicURLSuffix() string {
	if s.auth.PublicURL == "" {
		return ""
	}
	return " (" + s.auth.PublicURL + ")"
}

// setGoTrueCookies writes both cookies.
//
// 🔴 Secure IS UNCONDITIONAL IN gotrue MODE — it does not read
// MUSTER_SECURE_COOKIES. Browser sign-in is served only on the edge, which is
// TLS; a Secure cookie is the browser's half of "never over plain HTTP".
func (s *Server) setGoTrueCookies(w http.ResponseWriter, sess gotrue.Session) {
	for _, c := range []struct{ name, value string }{
		{accessCookieName, sess.AccessToken},
		{refreshCookieName, sess.RefreshToken},
	} {
		http.SetCookie(w, &http.Cookie{
			Name: c.name, Value: c.value, Path: "/",
			HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: true,
			MaxAge: int(gotrueCookieMaxAge / time.Second),
		})
	}
}

func (s *Server) clearGoTrueCookies(w http.ResponseWriter) {
	for _, name := range []string{accessCookieName, refreshCookieName} {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: "/",
			HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: true, MaxAge: -1,
		})
	}
}

func cookieValue(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}

// DisableUser disables user id: users.disabled_at, a GoTrue ban (which also
// stops its refresh tokens), and this process's cache entry, so the user's
// next request is a 401 (TestADisabledUserIsRefusedOnTheNextRequest).
//
// ⚠ THE ORDER IS DATABASE FIRST. disabled_at is what requireSession reads; a
// GoTrue failure after it leaves the user locked out of muster with a live
// GoTrue account, which is the safe half to be left in, and the error says so.
func (s *Server) DisableUser(ctx context.Context, id int64, disabled bool) error {
	if s.gt == nil {
		return errors.New("GoTrue is not configured (MUSTER_GOTRUE_URL)")
	}
	return SetUserDisabled(ctx, s.gt.GoTrueAuth, id, disabled, func() { s.gt.forgetUser(id) })
}

// SetUserDisabled is DisableUser without a server, for the CLI. invalidate
// (may be nil) drops a cache entry once the row has changed.
func SetUserDisabled(ctx context.Context, g GoTrueAuth, id int64, disabled bool, invalidate func()) error {
	u, err := g.Users.SetDisabled(ctx, id, disabled)
	if err != nil {
		return err
	}
	if invalidate != nil {
		invalidate()
	}
	if u.GotrueSub == "" {
		return nil
	}
	if err := g.Client.AdminSetBanned(ctx, "muster-server", u.GotrueSub, disabled); err != nil {
		return errors.New("users.disabled_at is updated, but the GoTrue ban did not apply: " + err.Error())
	}
	return nil
}

// --- the browser listener and the client address -------------------------

type browserListenerKey struct{}

// BrowserListener marks every request h serves as having arrived on the
// browser (edge) listener. cmd/muster-server wraps MUSTER_BROWSER_PORT's
// handler in it and nothing else; the key is unexported, so no request can
// carry the mark any other way.
func BrowserListener(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), browserListenerKey{}, true)))
	})
}

func viaBrowserListener(r *http.Request) bool {
	v, _ := r.Context().Value(browserListenerKey{}).(bool)
	return v
}

// clientIP is the address muster rate-limits on and forwards to GoTrue as
// X-Muster-Client-IP.
//
// On the browser listener the request came through the edge proxy chain, so
// the TCP peer is the proxy and the client is in the headers:
// CF-Connecting-IP (set by the CDN at the front) first, then the left-most
// X-Forwarded-For entry. On any other listener the headers are whatever the
// caller typed, so only the TCP peer is believed.
//
// ⚠ THE HEADERS ARE TRUSTED BECAUSE OF WHERE THE REQUEST ARRIVED, NOT BECAUSE
// OF WHO SENT IT. Anything that can reach the browser port without passing the
// CDN can choose its own address — which is why the per-address limit sits
// under a process-wide ceiling (loginAttemptCeiling) rather than replacing it.
func clientIP(r *http.Request) string {
	if viaBrowserListener(r) {
		if ip := parseIP(r.Header.Get("CF-Connecting-IP")); ip != "" {
			return ip
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if ip := parseIP(first); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := parseIP(host); ip != "" {
		return ip
	}
	return "unknown"
}

func parseIP(v string) string {
	ip := net.ParseIP(strings.TrimSpace(v))
	if ip == nil {
		return ""
	}
	return ip.String()
}

// keyedWindow is a fixed-window counter per key (a client address).
type keyedWindow struct {
	mu   sync.Mutex
	hits map[string]*windowCount
}

type windowCount struct {
	start time.Time
	n     int
}

// count returns the key's count in the current window.
func (k *keyedWindow) count(key string, now time.Time, window time.Duration) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	c, ok := k.hits[key]
	if !ok || now.Sub(c.start) >= window {
		return 0
	}
	return c.n
}

// add records one hit for key.
func (k *keyedWindow) add(key string, now time.Time, window time.Duration) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.hits == nil {
		k.hits = map[string]*windowCount{}
	}
	if len(k.hits) > 10000 {
		for key, c := range k.hits {
			if now.Sub(c.start) >= window {
				delete(k.hits, key)
			}
		}
	}
	c, ok := k.hits[key]
	if !ok || now.Sub(c.start) >= window {
		k.hits[key] = &windowCount{start: now, n: 1}
		return
	}
	c.n++
}
