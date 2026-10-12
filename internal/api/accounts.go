package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
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
	// Accounts serves link invites and the admin pages (accounts PR 5; see
	// admin.go). Nil: /admin/* answers 503 and /invite is not served.
	Accounts AccountStore
}

// UserStore is what the session tier needs from internal/users.
type UserStore interface {
	BySub(ctx context.Context, sub string) (users.User, error)
	ByID(ctx context.Context, id int64) (users.User, error)
	SetDisabled(ctx context.Context, id int64, disabled bool) (users.User, error)
	// IDByEmail resolves a sign-in email to a muster user id (case-folded).
	IDByEmail(ctx context.Context, email string) (int64, bool, error)
	// DisabledAmong reports, for each of ids that exists, whether it is
	// disabled. One query: the disable sweep calls it every interval.
	DisabledAmong(ctx context.Context, ids []int64) (map[int64]bool, error)
	RedeemClaim(ctx context.Context, tokenHash []byte, email string,
		create func(context.Context) (string, error), undo func(sub string)) (users.User, error)
}

// UseGoTrue attaches the GoTrue wiring. Call before Handler.
func (s *Server) UseGoTrue(g GoTrueAuth) {
	s.gt = &gotrueState{GoTrueAuth: g, now: time.Now, sweepInterval: DisableSweepInterval,
		lifetime: s.streams.context(), logger: s.logger}
	if g.Verifier.Now != nil {
		s.gt.now = g.Verifier.Now
	}
}

// DisableSweepInterval is how often the disable sweep (sweepDisabled) re-reads
// disabled_at for every user with an open request.
const DisableSweepInterval = 10 * time.Second

// gotrueState is the GoTrue wiring plus this process's caches.
type gotrueState struct {
	GoTrueAuth
	now func() time.Time

	mu        sync.Mutex
	userCache map[string]cachedUser
	// cacheGen counts user-cache invalidations (forgetUserLocked); see user.
	cacheGen uint64
	flights  map[string]*refreshFlight
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

	// live is every in-flight session request — SSE streams and terminal /
	// chat sockets included — by user, so a disable or a sign-out can END
	// what is already open (see track). disabled is the set of users disabled
	// through this process, checked under the same lock so a request that
	// resolved just before a disable cannot register just after its sweep.
	live     map[int64]map[*liveRequest]struct{}
	disabled map[int64]bool

	// The disable sweep: lifetime ends it (EndStreams at shutdown);
	// sweepInterval is DisableSweepInterval outside tests; sweepOnce starts it
	// with the first tracked request.
	sweepInterval time.Duration
	lifetime      context.Context
	sweepOnce     sync.Once
	logger        *log.Logger

	// beforeAdmit, TEST-ONLY (nil in production), runs in a refresh leader
	// between registering its flight and applying the throttles, so a test
	// can hold that window open (TestJoinersOfARefreshAreNeverThrottled).
	beforeAdmit func()

	// signin is the sign-in throttle (signin_limiter.go): its own mutex, not
	// mu, which every authenticated request takes.
	signin signinLimiter
	// Refresh throttles (resolveGoTrueSession), each bounded, O(1), own mutex:
	// refreshPerToken counts calls per refresh-token hash; failedRefreshes
	// counts FAILED refreshes per subject; issued is the set of refresh tokens
	// this process handed out (their hashes), which pass the subject cap.
	refreshPerToken boundedWindow
	failedRefreshes refreshGuard
	issued          boundedWindow
}

// liveRequest is one in-flight session request's cancel.
type liveRequest struct {
	sid    string
	cancel context.CancelCauseFunc
}

// errSessionEnded is the cause an open stream is cancelled with when its
// user is disabled or its session signed out.
var errSessionEnded = errors.New("session ended: signed out or disabled")

// track registers a session request so endUser / revoke can cancel it, and
// returns the context the handler must run under. ok is false when the user
// was disabled, or the session revoked, after this request resolved — the
// check is under the same lock as the sweeps, so nothing slips between them.
//
// 🔴 ONE CHOKE POINT FOR EVERY KIND OF OPEN STREAM. Every SSE handler takes
// its loop context from r.Context() (streamContext), and both terminal
// sockets and the chat socket bridge on a context derived from it; cancelling
// this one context ends all of them (TestEndingASessionEndsItsOpenStreams).
func (g *gotrueState) track(parent context.Context, uid int64, c gotrue.Claims) (context.Context, func(), bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.disabled[uid] || g.isRevokedLocked(c) {
		return nil, nil, false
	}
	ctx, cancel := context.WithCancelCause(parent)
	lr := &liveRequest{sid: c.SessionID, cancel: cancel}
	if g.live == nil {
		g.live = map[int64]map[*liveRequest]struct{}{}
	}
	if g.live[uid] == nil {
		g.live[uid] = map[*liveRequest]struct{}{}
	}
	g.live[uid][lr] = struct{}{}
	g.sweepOnce.Do(func() { safeGo(g.logger, "disable sweep", g.runDisableSweep) })
	return ctx, func() {
		g.mu.Lock()
		delete(g.live[uid], lr)
		if len(g.live[uid]) == 0 {
			delete(g.live, uid)
		}
		g.mu.Unlock()
		cancel(nil)
	}, true
}

// recordReset records a password reset of uid (GoTrue subject sub, session
// cutoff cut) the way revoke records a sign-out-everywhere: under g.mu,
// notBefore[sub] moves to cut — so track, which checks it under the same lock,
// refuses any request that passed the cutoff check before the reset and
// registers after it (TestARequestInFlightAcrossAResetIsRefusedAtAdmission) —
// and then every live request of the user is cancelled. The session the reset
// signs in is issued after cut (signInAfterCutoff), so it is not refused.
func (g *gotrueState) recordReset(uid int64, sub string, cut time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.revokedSessions == nil {
		g.revokedSessions = map[string]time.Time{}
		g.notBefore = map[string]time.Time{}
		g.afterSignOut = map[string]bool{}
	}
	if nb, ok := g.notBefore[sub]; !ok || cut.After(nb) {
		g.notBefore[sub] = cut
	}
	g.endLocked(uid, "")
}

// endLocked cancels uid's live requests — all of them, or only session sid's.
func (g *gotrueState) endLocked(uid int64, sid string) {
	for lr := range g.live[uid] {
		if sid == "" || lr.sid == sid {
			lr.cancel(errSessionEnded)
		}
	}
}

// runDisableSweep calls sweepDisabled every sweepInterval until lifetime ends.
//
// 🔴 IT EXISTS BECAUSE THE REAL DISABLE IS NOT IN THIS PROCESS. DisableUser
// ends a user's open streams at once — but its only caller today is the
// (future) admin page; the disable an operator actually runs is the CLI
// (`muster-server user disable`), which writes disabled_at from ANOTHER
// process. Without this, that user's open SSE stream, agent terminal or
// mirror shell lived on until the browser hung up. Now every open request's
// user is re-read here, in ONE batched query, and a disabled (or deleted)
// user's requests are cancelled within one interval
// (TestADisableFromAnotherProcessEndsOpenStreams).
func (g *gotrueState) runDisableSweep() {
	ctx := g.lifetime
	if ctx == nil {
		ctx = context.Background()
	}
	t := time.NewTicker(g.sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.sweepDisabled(ctx)
		}
	}
}

// sweepDisabled re-reads disabled_at for every user with an open request, and
// for every user this process holds as disabled: it cancels the first kind
// when the row says disabled (or the row is gone), and forgets the second when
// the row says enabled again (a CLI `user enable`).
func (g *gotrueState) sweepDisabled(ctx context.Context) {
	g.mu.Lock()
	seen := map[int64]bool{}
	var ids []int64
	for id := range g.live {
		seen[id] = true
		ids = append(ids, id)
	}
	for id := range g.disabled {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	g.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	state, err := g.Users.DisabledAmong(qctx, ids)
	if err != nil {
		if g.logger != nil {
			g.logger.Printf("session: disable sweep could not read users (open streams unchanged): %v", err)
		}
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.disabled == nil {
		g.disabled = map[int64]bool{}
	}
	for _, id := range ids {
		disabled, exists := state[id]
		if !exists || disabled {
			if !g.disabled[id] || len(g.live[id]) > 0 {
				g.disabled[id] = true
				g.endLocked(id, "")
				g.forgetUserLocked(id)
			}
			continue
		}
		if g.disabled[id] {
			delete(g.disabled, id)
			g.forgetUserLocked(id)
		}
	}
}

// setDisabled records a disable (ending the user's open streams) or clears it.
func (g *gotrueState) setDisabled(uid int64, disabled bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.disabled == nil {
		g.disabled = map[int64]bool{}
	}
	if !disabled {
		delete(g.disabled, uid)
		return
	}
	g.disabled[uid] = true
	g.endLocked(uid, "")
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
//
// 🔴 A READ THAT STARTED BEFORE AN INVALIDATION DOES NOT WRITE THE CACHE. The
// row is read with the lock released; if forgetUser runs meanwhile (a disable,
// an enable, a reset, a role change), the row this read returns may predate
// that change, and caching it would undo the invalidation for userCacheTTL
// (TestAStaleReadDoesNotRePoisonTheUserCache). cacheGen counts invalidations;
// a write whose read began under an older generation is dropped — the caller
// still gets its row, the next request reads again. One counter for every
// user: an invalidation of one user only costs another's in-flight read its
// cache write.
func (g *gotrueState) user(ctx context.Context, sub string) (users.User, error) {
	now := g.now()
	g.mu.Lock()
	if c, ok := g.userCache[sub]; ok && now.Sub(c.at) < userCacheTTL {
		g.mu.Unlock()
		return c.u, nil
	}
	gen := g.cacheGen
	g.mu.Unlock()
	u, err := g.Users.BySub(ctx, sub)
	if err != nil {
		return users.User{}, err
	}
	g.mu.Lock()
	if g.cacheGen == gen {
		if g.userCache == nil {
			g.userCache = map[string]cachedUser{}
		}
		g.userCache[sub] = cachedUser{u: u, at: now}
	}
	g.mu.Unlock()
	return u, nil
}

// forgetUser drops every cache entry for user id.
func (g *gotrueState) forgetUser(id int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.forgetUserLocked(id)
}

func (g *gotrueState) forgetUserLocked(id int64) {
	g.cacheGen++
	for sub, c := range g.userCache {
		if c.u.ID == id {
			delete(g.userCache, sub)
		}
	}
}

// refresh exchanges rt, sharing one GoTrue call among every request that
// presents the same token within refreshShareWindow.
//
// admit runs in the LEADER only — the one request that will actually call
// GoTrue — before the call: it applies the refresh throttles and returns a
// release for the result, or ok=false, which answers every waiter with
// errRefreshThrottled. Requests joining a flight are never throttled: they
// cost GoTrue nothing, and a page load's parallel requests all join one.
func (g *gotrueState) refresh(ctx context.Context, clientIP, rt string,
	admit func() (release func(error), ok bool)) (gotrue.Session, error) {
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
	hold := g.beforeAdmit
	g.mu.Unlock()
	if hold != nil {
		hold()
	}
	release, ok := admit()
	if !ok {
		f.err = errRefreshThrottled
	} else {
		// Detached from the request: a client that hangs up mid-refresh must
		// not leave the other waiters with a cancelled result for a token that
		// GoTrue may already have rotated.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		f.sess, f.err = g.Client.Refresh(rctx, clientIP, rt)
		cancel()
		release(f.err)
	}
	if f.err != nil {
		// A FAILED flight is shared only with the requests already waiting on
		// it, never cached: a GoTrue blip must not stay the answer for 30 s.
		g.mu.Lock()
		if g.flights[key] == f {
			delete(g.flights, key)
		}
		g.mu.Unlock()
	}
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
func (g *gotrueState) revoke(uid int64, c gotrue.Claims, scope gotrue.LogoutScope) {
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
		g.endLocked(uid, "")
		return
	}
	g.endLocked(uid, c.SessionID)
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
	return g.isRevokedLocked(c)
}

func (g *gotrueState) isRevokedLocked(c gotrue.Claims) bool {
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
	// 🔴 NO SIGNED ACCESS TOKEN, NO REFRESH. A refresh is attempted only for an
	// access token whose SIGNATURE verified and which has merely expired
	// (Verify returns ErrExpired only after the signature check): that is
	// unforgeable, so a request carrying only a muster_rt — real or invented,
	// with the access cookie absent ("" is malformed) or forged — cannot make
	// muster call GoTrue at all (TestARefreshNeedsASignedExpiredAccessToken).
	c, err := g.Verifier.Verify(at)
	if errors.Is(err, gotrue.ErrExpired) && rt != "" {
		// TWO refresh throttles, applied by the flight's LEADER only (the one
		// request that calls GoTrue; joiners of a flight cost GoTrue nothing),
		// and both RESERVED before the call — see refreshGuard for why a
		// check-call-record cap let a parallel flood through.
		//
		// 🔴 PER SUBJECT, ONLY FAILURES COUNT, AND A TOKEN THIS PROCESS ISSUED
		// IS EXEMPT. With a signed expired access token, a caller can attach a
		// fresh junk refresh token to every request; a per-token cap alone
		// bounds nothing (measured: every one of 500 requests a minute reached
		// GoTrue). So failed refreshes are counted against the expired token's
		// SUBJECT (one in flight per subject), and over failedRefreshLimit a refresh is answered 503
		// without calling GoTrue — unless the refresh token is one this process
		// issued (issued), which is what the subject's real browser holds. The
		// victim's own refresh therefore keeps working through a junk flood
		// aimed at them (TestAJunkRefreshFloodIsBoundedAndSparesTheVictim);
		// after a restart, when nothing is known as issued, it waits out the
		// window with its cookies kept.
		//
		// And per refresh token, a ceiling on calls (a replay of one token).
		expiredSub := c.Subject
		// 🔴 NO REFRESH FROM A TOKEN ISSUED BEFORE THE USER'S SESSION CUTOFF
		// (accounts PR 5). GoTrue accepts a pre-ban refresh token again once a
		// ban is lifted, so after disable → enable the old refresh token is
		// live in GoTrue; this check, on the expired access token it is paired
		// with, is what keeps it dead (TestEnablingDoesNotResurrectOldSessions).
		//
		// 🔴 FAIL CLOSED: a lookup error refuses the refresh (503 with cookies
		// kept, or the end of the session for an unknown subject — see
		// endsTheSession). Skipping the check on an error would mint a
		// post-cutoff token and walk past the cutoff once the lookup recovered
		// (TestARefreshFailsClosedWhenTheCutoffCannotBeRead).
		u, uerr := g.user(r.Context(), expiredSub)
		if uerr != nil {
			return sess, uerr
		}
		if issuedBeforeCutoff(u, c) {
			return sess, errRevoked
		}
		fresh, rerr := g.refresh(r.Context(), clientIP(r), rt, func() (func(error), bool) {
			rtSum := sha256.Sum256([]byte(rt))
			rtKey := hex.EncodeToString(rtSum[:])
			now := g.now()
			exempt := g.issued.count(rtKey, now, issuedWindow) > 0
			var guard refreshTicket
			if !exempt {
				var ok bool
				if guard, ok = g.failedRefreshes.reserve(expiredSub, now, time.Minute, failedRefreshLimit); !ok {
					return nil, false
				}
			}
			if !g.refreshPerToken.take(rtKey, now, time.Minute, refreshPerTokenLimit) {
				if !exempt {
					g.failedRefreshes.release(guard, false)
				}
				return nil, false
			}
			return func(rerr error) {
				if exempt {
					return
				}
				// The slot stands only when GoTrue judged the token bad;
				// success, a 429 or no answer refunds it.
				var ae *gotrue.APIError
				judgedBad := errors.As(rerr, &ae) && ae.Status >= 400 && ae.Status < 500 && ae.Status != http.StatusTooManyRequests
				g.failedRefreshes.release(guard, judgedBad)
			}, true
		})
		if rerr != nil {
			return sess, rerr
		}
		at = fresh.AccessToken
		if c, err = g.Verifier.Verify(at); err != nil {
			s.logger.Printf("gotrue: a token GoTrue just issued does not verify (%v) — check "+
				"MUSTER_GOTRUE_JWT_SECRET and the issuer/audience against GoTrue's", err)
			return sess, err
		}
		if c.Subject != expiredSub {
			// 🔴 THE REFRESH TOKEN BELONGS TO SOMEONE ELSE. The pair in the
			// cookies is not one session: a planted muster_rt beside the
			// victim's own expired access token would otherwise sign the
			// victim in as the PLANTER (TestARefreshForAnotherSubjectIsRefused).
			s.logger.Printf("session: refusing a refresh whose new subject differs from the expired token's")
			return sess, errRefreshMismatch
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
	if issuedBeforeCutoff(u, c) {
		return sess, errRevoked
	}
	sess.user, sess.claims, sess.access = u, c, at
	return sess, nil
}

// issuedBeforeCutoff reports whether c was issued at or before u's session
// cutoff (users.sessions_not_before, set by every disable; migration 0010).
// "At" as well as before: iat is whole seconds, so a token stamped with the
// cutoff's own second may predate it, and refusing it costs only a sign-in
// made in that same second.
func issuedBeforeCutoff(u users.User, c gotrue.Claims) bool {
	return u.SessionsNotBefore != nil && c.IssuedAt <= u.SessionsNotBefore.Unix()
}

var (
	errDisabled         = errors.New("user is disabled")
	errRefreshThrottled = errors.New("too many refreshes of this refresh token")
	errRefreshMismatch  = errors.New("the refresh token belongs to another subject")
)

// Refresh throttle limits (see resolveGoTrueSession).
const (
	// refreshPerTokenLimit bounds GoTrue refresh calls per refresh token per minute.
	refreshPerTokenLimit = 20
	// failedRefreshLimit bounds FAILED refreshes per subject per minute, for
	// refresh tokens this process did not issue.
	failedRefreshLimit = 10
	// issuedWindow is how long an issued refresh token is remembered.
	issuedWindow = 30 * 24 * time.Hour
)

// endsTheSession reports whether err means the cookies are no good — so they
// are cleared and the caller must sign in again — as opposed to muster or
// GoTrue being unable to answer right now.
//
// 🔴 ONLY AUTH-CLASS FAILURES CLEAR COOKIES. A GoTrue 5xx or 429, a timeout or
// a Postgres error says nothing about the session; clearing on those signed
// every open tab out for a blip (TestATransientFailureKeepsTheSession).
func endsTheSession(err error) bool {
	switch {
	case errors.Is(err, errRevoked), errors.Is(err, errDisabled), errors.Is(err, users.ErrNotFound),
		errors.Is(err, errRefreshMismatch):
		return true
	case errors.Is(err, gotrue.ErrMalformed), errors.Is(err, gotrue.ErrAlgorithm),
		errors.Is(err, gotrue.ErrSignature), errors.Is(err, gotrue.ErrIssuer),
		errors.Is(err, gotrue.ErrAudience), errors.Is(err, gotrue.ErrExpired),
		errors.Is(err, gotrue.ErrNotYet), errors.Is(err, gotrue.ErrNoSubject):
		return true
	}
	var ae *gotrue.APIError
	return errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500 && ae.Status != http.StatusTooManyRequests
}

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
	if err != nil && !endsTheSession(err) {
		s.logger.Printf("session: could not resolve (cookies kept): %v", err)
		if isDocumentNavigation(r) {
			s.renderNotice(w, http.StatusServiceUnavailable, "muster cannot check your sign-in right now",
				"Reload in a moment; you are still signed in.")
			return
		}
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "sign-in could not be checked right now; retry"})
		return
	}
	if err != nil {
		s.clearGoTrueCookies(w)
		s.refuseUnauthenticated(w, r)
		return
	}
	ctx, untrack, ok := s.gt.track(r.Context(), sess.user.ID, sess.claims)
	if !ok {
		s.clearGoTrueCookies(w)
		s.refuseUnauthenticated(w, r)
		return
	}
	defer untrack()
	if sess.refreshed != nil {
		s.setGoTrueCookies(w, *sess.refreshed)
	}
	// The role comes from roleOf — the ONE role read (users.role, failure →
	// member) every door uses — not from the session's cached user row.
	// The session's issued-at rides along for the one handler that needs it:
	// minting an API token re-checks it against the session cutoff under the
	// credentials lock (api_tokens.go, apitokens.Create).
	ctx = context.WithValue(ctx, sessionIssuedAtKey{}, sess.claims.IssuedAt)
	s.admit(w, r.WithContext(ctx), next, s.principalFor(ctx, sess.user.ID, authz.ViaSession))
}

// sessionIssuedAtKey carries a GoTrue session's iat (unix seconds).
type sessionIssuedAtKey struct{}

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
	if s.gt != nil && sess.RefreshToken != "" {
		sum := sha256.Sum256([]byte(sess.RefreshToken))
		s.gt.issued.add(hex.EncodeToString(sum[:]), s.gt.now(), issuedWindow)
	}
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
	return SetUserDisabled(ctx, s.gt.GoTrueAuth, id, disabled, func() {
		s.gt.forgetUser(id)
		s.gt.setDisabled(id, disabled)
	})
}

// DisableUserAs is DisableUser on behalf of by: the user.disable /
// user.enable audit row is written in the same transaction as the change
// (accounts PR 5; the admin page).
func (s *Server) DisableUserAs(ctx context.Context, by users.Actor, id int64, disabled bool) error {
	if s.gt == nil {
		return errors.New("GoTrue is not configured (MUSTER_GOTRUE_URL)")
	}
	return SetUserDisabledAs(ctx, s.gt.GoTrueAuth, &by, id, disabled, func() {
		s.gt.forgetUser(id)
		s.gt.setDisabled(id, disabled)
	})
}

// SetUserDisabled is DisableUser without a server. invalidate (may be nil)
// drops a cache entry once the row has changed.
func SetUserDisabled(ctx context.Context, g GoTrueAuth, id int64, disabled bool, invalidate func()) error {
	return SetUserDisabledAs(ctx, g, nil, id, disabled, invalidate)
}

// SetUserDisabledAs is SetUserDisabled that audits as by (the CLI passes
// Via "cli"). With by set and an account store wired, the audit row commits
// with the change; a GoTrue ban that fails AFTER it gets its own
// user.ban_failed row.
func SetUserDisabledAs(ctx context.Context, g GoTrueAuth, by *users.Actor, id int64, disabled bool, invalidate func()) error {
	var u users.User
	var err error
	if by != nil && g.Accounts != nil {
		u, err = g.Accounts.SetDisabledAs(ctx, by, id, disabled)
	} else {
		u, err = g.Users.SetDisabled(ctx, id, disabled)
	}
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
		if by != nil && g.Accounts != nil {
			_ = g.Accounts.Audit(ctx, *by, "user.ban_failed", users.UserObject(id), map[string]any{"disabled": disabled, "error": err.Error()})
		}
		return errors.New("users.disabled_at is updated, but the GoTrue ban did not apply: " + err.Error())
	}
	if disabled {
		// A ban does not end GoTrue sessions (an unban resurrects them), so
		// a disable also signs the user out everywhere in GoTrue. muster's
		// own cutoff (users.sessions_not_before) refuses those sessions
		// regardless; this keeps GoTrue from holding them live.
		if err := g.Client.AdminSignOutEverywhere(ctx, "muster-server", u.GotrueSub); err != nil {
			if by != nil && g.Accounts != nil {
				_ = g.Accounts.Audit(ctx, *by, "user.signout_failed", users.UserObject(id), map[string]any{"error": err.Error()})
			}
			return errors.New("users.disabled_at is updated and the login banned, but GoTrue's sign-out-everywhere did not apply " +
				"(muster still refuses the old sessions): " + err.Error())
		}
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
// CDN can choose its own address — which is why every per-address limit is a
// SECONDARY signal: sign-in is throttled per ACCOUNT (signin_limiter.go) and
// invite links per INVITE (login_invite.go), neither keyed on this value. There
// is no process-wide ceiling (an earlier draft had one; it was a lockout lever).
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
