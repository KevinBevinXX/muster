package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"html"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/auth"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/gotrue"
	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/internal/users"
)

// The gotrue-mode halves of the login tier, the admin claim, and the account
// page. login.go dispatches here when AuthConfig.Mode is gotrue; password mode
// never reaches this file except for the claim, which works in both modes so
// the admin can be bound BEFORE the mode is flipped (accounts plan §8 step 3).

// Sign-in throttles for gotrue mode.
//
// 🔴 THE KEY IS THE ACCOUNT, NOT THE ADDRESS, AND THERE IS NO GLOBAL CEILING.
// The client address comes from CF-Connecting-IP / X-Forwarded-For on the
// browser listener, and anything that reaches the edge without passing the CDN
// can write those — so an address-only limit is walked by rotating it, and a
// process-wide ceiling (an earlier draft had one) is a lever any anonymous
// caller pulls to lock EVERY user out. Failures are therefore counted per
// ACCOUNT (the normalised email): after accountFreeFailures, each further
// failure doubles the wait before that account may try again, from
// accountBackoffBase up to accountBackoffMax. The per-address count
// (loginFailureLimit per loginFailureWindow) is kept as a secondary limit on
// one source spraying many accounts. GoTrue applies its own limits too.
//
// ⚠ THE ACCOUNT BACKOFF IS ITSELF A TARGETED LOCKOUT: someone who knows your
// email can keep you waiting up to accountBackoffMax by failing on purpose. That
// is the accepted trade (one account, bounded wait) against the alternative of
// unbounded guessing of that account's password.
const (
	accountFreeFailures = 3
	accountBackoffBase  = 2 * time.Second
	accountBackoffMax   = 5 * time.Minute
	loginFailureLimit   = 10
	loginFailureWindow  = 10 * time.Minute
)

// normaliseAccount is the key an unknown email is throttled under.
func normaliseAccount(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// signinAccountFor resolves email for the limiter: a muster user (by id, in
// the known table) or an unknown email. The lookup runs as System inside the
// store (users.sys); a database error is returned, never guessed.
func (g *gotrueState) signinAccountFor(ctx context.Context, email string) (signinAccount, error) {
	key := normaliseAccount(email)
	if key == "" {
		return signinAccount{}, nil
	}
	uid, ok, err := g.Users.IDByEmail(ctx, key)
	if err != nil {
		return signinAccount{}, err
	}
	if ok {
		return signinAccount{known: true, uid: uid}, nil
	}
	return signinAccount{email: key}, nil
}

// ClaimPath is the admin claim page. The token is NOT in the path: it rides
// in the URL FRAGMENT (`/auth/claim#<token>`), which a browser never sends to
// a server, and the page's form posts it as a field.
//
// 🔴 A PATH TOKEN WAS WRITTEN TO THE ACCESS LOG. accessLog records
// r.URL.Path for every request, so `GET /auth/claim/<token>` put a live,
// single-use admin credential into muster's logs (and any proxy's). A fragment
// reaches neither (TestTheClaimTokenNeverReachesTheLog).
const ClaimPath = "/auth/claim"

// ClaimTTL is how long a claim link stays redeemable.
const ClaimTTL = 24 * time.Hour

// accountPath is the signed-in account page (sign out, sign out everywhere).
const accountPath = "/account"

// SignOutEverywhereTokenNote sits beside "Sign out everywhere": that button
// ends GoTrue sessions only, and a person must not believe it ended their API
// tokens (accounts PR 6).
const SignOutEverywhereTokenNote = "Paired phones are signed out too. API tokens are not affected — revoke them under API tokens below."

// ResetDoneNote is what /account?reset=1 — where a successful password reset
// lands — says: the reset revoked every personal API token (users.ResetCredentials).
// ⚠ DISPLAY-ONLY: the query flag is not a fact the server checks, so a
// hand-typed /account?reset=1 shows it too. It reaches nothing else.
const ResetDoneNote = "Your password was reset. Your API tokens were revoked; create new ones under API tokens below."

// accountResetDonePath is where a successful reset redirects.
const accountResetDonePath = "/account?reset=1"

// registerAccountRoutes wires the claim and account routes. Literal patterns,
// for the reason registerLoginRoutes gives.
func (s *Server) registerAccountRoutes(mux Mux) {
	mux.HandleFunc("GET /auth/claim", s.handleClaimPage)
	mux.HandleFunc("POST /auth/claim", s.handleClaimSubmit)
	mux.HandleFunc("GET /account", s.requireSession(s.handleAccountPage))
	s.registerPasskeyRoutes(mux)
}

// renderNotice writes a one-message standalone page.
func (s *Server) renderNotice(w http.ResponseWriter, code int, heading, body string) {
	writeAuthDocument(w, code, heading, `<section class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line">
<h1 class="text-balance text-base font-semibold text-fg">`+html.EscapeString(heading)+`</h1>
<p class="text-sm text-fg2">`+html.EscapeString(body)+`</p>
</section>`)
}

// --- sign-in ------------------------------------------------------------------

func (s *Server) handleGoTrueLoginPage(w http.ResponseWriter, r *http.Request) {
	if !viaBrowserListener(r) {
		s.refuseOffBrowserListener(w, r)
		return
	}
	if s.gt == nil {
		s.renderNotice(w, http.StatusServiceUnavailable, "Sign-in is not configured",
			"This server is in gotrue mode but has no GoTrue configured, so it cannot sign anyone in.")
		return
	}
	if sess, err := s.resolveGoTrueSession(r); err == nil {
		if sess.refreshed != nil {
			s.setGoTrueCookies(w, *sess.refreshed)
		}
		http.Redirect(w, r, safeReturnTo(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	s.renderGoTrueLogin(w, http.StatusOK, r.URL.Query().Get("next"), "", "")
}

func (s *Server) renderGoTrueLogin(w http.ResponseWriter, code int, next, email, problem string) {
	passkey := ""
	if s.gt != nil && s.gt.passkeysOn(context.Background()) {
		// Outside the form: the passkey button posts nothing, so the form's
		// required email/password never block it. The script prefetches the
		// sign-in options and runs the ceremony (passkeySignInJS).
		passkey = `<section class="mt-4 flex flex-col gap-2">
<button type="button" id="passkey-signin" class="press min-h-[44px] w-full rounded-md bg-bg px-3 py-2 text-base font-semibold text-fg ring-1 ring-inset ring-edge">Sign in with a passkey</button>
<p class="text-sm text-fg2" id="passkey-signin-status" role="status" aria-live="polite"></p>
</section>
<script>` + ui.PasskeyJS + passkeySignInJS + `</script>`
	}
	inner := `<form method="POST" action="` + loginPath + `" class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line">
<h1 class="text-balance text-base font-semibold text-fg">Sign in to muster</h1>
` + problemNode(problem) + `
<label for="muster-email" class="text-sm font-medium text-fg">Email</label>
<input id="muster-email" name="email" type="email" autocomplete="username" autofocus required value="` + html.EscapeString(email) + `"
 class="min-h-[44px] rounded-md bg-bg px-3 py-2 text-base text-fg ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus">
<label for="muster-password" class="text-sm font-medium text-fg">Password</label>
<input id="muster-password" name="password" type="password" autocomplete="current-password" required
 class="min-h-[44px] rounded-md bg-bg px-3 py-2 text-base text-fg ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus">
<input type="hidden" name="next" value="` + html.EscapeString(safeReturnTo(next)) + `">
<button type="submit" class="press min-h-[44px] rounded-md bg-accent px-3 py-2 text-base font-semibold text-on-accent transition hover:bg-accent/90">Sign in</button>
</form>` + passkey
	writeAuthDocument(w, code, "Sign in", inner)
}

func (s *Server) handleGoTrueLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if !viaBrowserListener(r) {
		s.refuseOffBrowserListener(w, r)
		return
	}
	if s.gt == nil {
		s.renderNotice(w, http.StatusServiceUnavailable, "Sign-in is not configured",
			"This server is in gotrue mode but has no GoTrue configured.")
		return
	}
	if !s.isSameOrigin(r) {
		// Login CSRF: a cross-site form could otherwise sign the victim's
		// browser into the ATTACKER'S account. Every browser sends Origin on
		// a form POST, and muster's own form is same-origin.
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": CrossOriginRefusal})
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderGoTrueLogin(w, http.StatusBadRequest, "", "", "That form could not be read.")
		return
	}
	next, email, password := r.FormValue("next"), strings.TrimSpace(r.FormValue("email")), r.FormValue("password")
	ip := clientIP(r)
	// The account is resolved FIRST: a muster user is throttled in the known
	// table (never evicted, never full), anything else in the evictable one
	// (see signinLimiter).
	acct, err := s.gt.signinAccountFor(r.Context(), email)
	if err != nil {
		s.logger.Printf("login: could not resolve the account: %v", err)
		s.renderGoTrueLogin(w, http.StatusServiceUnavailable, next, email, "Sign-in is unavailable right now. Try again shortly.")
		return
	}
	// Reserved BEFORE GoTrue is asked (see signinLimiter): the attempt counts
	// as a failure until settle says otherwise, and a second attempt at the
	// same account while this one is in flight is refused here.
	ticket, ok := s.gt.signin.reserve(acct, ip, s.gt.now())
	if !ok {
		// 429, not 401: the credential was never judged.
		s.renderGoTrueLogin(w, http.StatusTooManyRequests, next, email, "Too many attempts. Wait a few minutes and try again.")
		return
	}
	outcome := outcomeUnjudged
	defer func() { s.gt.signin.settle(ticket, outcome) }()
	sess, err := s.gt.Client.PasswordGrant(r.Context(), ip, email, password)
	if err != nil {
		var ae *gotrue.APIError
		switch {
		case errors.As(err, &ae) && ae.Status == http.StatusTooManyRequests:
			s.renderGoTrueLogin(w, http.StatusTooManyRequests, next, email, "Too many attempts. Wait a few minutes and try again.")
		case gotrue.IsClientError(err):
			outcome = outcomeFailed
			s.logger.Printf("login: rejected a sign-in attempt from %s", ip)
			s.renderGoTrueLogin(w, http.StatusUnauthorized, next, email, "That email and password do not match.")
		default:
			s.logger.Printf("login: GoTrue is unavailable: %v", err)
			s.renderGoTrueLogin(w, http.StatusBadGateway, next, email, "Sign-in is unavailable right now. Try again shortly.")
		}
		return
	}
	u, problem, code := s.admitFreshSession(r.Context(), sess)
	if problem != "" {
		if code != http.StatusInternalServerError {
			outcome = outcomeFailed // no muster account / disabled: still throttled
		}
		s.renderGoTrueLogin(w, code, next, email, problem)
		return
	}
	outcome = outcomeSucceeded
	s.setGoTrueCookies(w, sess)
	s.logger.Printf("login: user %d signed in", u.ID)
	http.Redirect(w, r, safeReturnTo(next), http.StatusSeeOther)
}

// admitFreshSession checks a pair GoTrue just issued: the access token must
// verify with muster's own settings, and its subject must be a muster user
// who is not disabled. A refusal returns the message and status to show.
func (s *Server) admitFreshSession(ctx context.Context, sess gotrue.Session) (users.User, string, int) {
	c, err := s.gt.Verifier.Verify(sess.AccessToken)
	if err != nil {
		s.logger.Printf("login: a token GoTrue just issued does not verify (%v) — check "+
			"MUSTER_GOTRUE_JWT_SECRET and the issuer/audience against GoTrue's", err)
		return users.User{}, "Sign-in is misconfigured on this server.", http.StatusInternalServerError
	}
	u, err := s.gt.user(ctx, c.Subject)
	if errors.Is(err, users.ErrNotFound) {
		return users.User{}, "This login has no muster account.", http.StatusForbidden
	}
	if err != nil {
		s.logger.Printf("login: user lookup failed: %v", err)
		return users.User{}, "Sign-in is unavailable right now. Try again shortly.", http.StatusInternalServerError
	}
	if u.Disabled() {
		return users.User{}, "This account is disabled.", http.StatusForbidden
	}
	s.gt.signedIn(c)
	return u, "", 0
}

// handleGoTrueLogout ends the session: GoTrue /logout (scope global when the
// form asks for "everywhere"), this process's revocation of the access token,
// and the cookies.
func (s *Server) handleGoTrueLogout(w http.ResponseWriter, r *http.Request) {
	if !s.isSameOrigin(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": CrossOriginRefusal})
		return
	}
	scope := gotrue.ScopeLocal
	if r.FormValue("scope") == "global" {
		scope = gotrue.ScopeGlobal
	}
	if s.gt != nil && viaBrowserListener(r) {
		if sess, err := s.resolveGoTrueSession(r); err == nil {
			if err := s.gt.Client.Logout(r.Context(), clientIP(r), sess.access, scope); err != nil {
				// The local revocation below still applies; the refresh side
				// may survive in GoTrue, so it is logged rather than swallowed.
				s.logger.Printf("logout: GoTrue /logout failed for user %d: %v", sess.user.ID, err)
			}
			s.gt.revoke(sess.user.ID, sess.claims, scope)
			if signsOutEverywhere(sess.claims, scope) {
				// Signed out of every session: nothing queued from one is sent.
				s.signedOutEverywhere(r.Context(), sess.user.ID)
			}
			// This browser's push subscription goes with its session.
			s.forgetPushDevice(w, r, sess.user.ID)
			s.logger.Printf("logout: user %d signed out (%s)", sess.user.ID, scope)
		}
	}
	s.clearGoTrueCookies(w)
	http.Redirect(w, r, loginPath, http.StatusSeeOther)
}

// --- the account page ---------------------------------------------------------

func (s *Server) handleAccountPage(w http.ResponseWriter, r *http.Request) {
	who := "the operator password"
	if s.auth.goTrueMode() && s.gt != nil {
		// The principal requireSession attached names the user; read the row
		// rather than resolving the cookies a second time.
		if u, err := s.gt.Users.ByID(r.Context(), authz.MustFrom(r.Context()).UserID); err == nil {
			who = u.Email
		}
	}
	everywhere := ""
	if s.auth.goTrueMode() {
		everywhere = `<form method="POST" action="/logout"><input type="hidden" name="scope" value="global">
<button type="submit" class="press min-h-[44px] w-full rounded-md bg-bg px-3 py-2 text-base font-semibold text-fg ring-1 ring-inset ring-edge">Sign out everywhere</button></form>
<p class="text-sm text-fg2">` + SignOutEverywhereTokenNote + `</p>`
	}
	passkeysLink := ""
	if s.passkeysServed(r.Context()) {
		passkeysLink = `<a href="` + accountPasskeysPath + `" id="account-passkeys" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Passkeys</a>`
	}
	resetNote := ""
	if r.URL.Query().Get("reset") == "1" {
		resetNote = `<p class="text-sm font-semibold text-fg" role="status">` + ResetDoneNote + `</p>`
	}
	writeAuthDocument(w, http.StatusOK, "Account", `<div class="flex flex-col gap-4"><section class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line">
<h1 class="text-balance text-base font-semibold text-fg">Account</h1>
`+resetNote+`
<p class="text-sm text-fg2">Signed in as <span class="font-semibold text-fg">`+html.EscapeString(who)+`</span>.</p>
<form method="POST" action="/logout">
<button type="submit" class="press min-h-[44px] w-full rounded-md bg-accent px-3 py-2 text-base font-semibold text-on-accent transition hover:bg-accent/90">Sign out</button></form>
`+everywhere+`
`+passkeysLink+`
<a href="/account/credentials" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Credentials (Claude, OpenRouter)</a>
<a href="`+accountTokensPath+`" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">API tokens</a>
<a href="/" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Back to muster</a>
</section>
`+s.accountDevicesSection(r)+`
`+s.accountNotificationsSection(r)+`</div>`)
}

// --- the admin claim ------------------------------------------------------------
//
// `muster-server invite --claim-admin` mints a single-use link; redeeming it
// creates a GoTrue user and binds it to users.id = 1 (the operator, whose
// gotrue_sub is NULL until then). It is the ONLY invite flow in this PR; general
// invites are accounts PR 5.

// ClaimMinter is what minting a claim link needs from the store.
type ClaimMinter interface {
	CreateClaimInvite(ctx context.Context, tokenHash []byte, claimUserID, createdBy int64, expiresAt time.Time) error
}

// NewAdminClaimLink mints a claim invite for user 1 and returns the link. The
// token is shown once here and stored only as its sha256.
func NewAdminClaimLink(ctx context.Context, store ClaimMinter, publicURL string, now time.Time) (string, error) {
	if strings.TrimSpace(publicURL) == "" {
		return "", errors.New("MUSTER_PUBLIC_URL is not set, so there is no address to put in the link")
	}
	token, err := auth.RandomToken(32)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(token))
	if err := store.CreateClaimInvite(ctx, sum[:], adminUserID, adminUserID, now.Add(ClaimTTL)); err != nil {
		return "", err
	}
	return strings.TrimRight(publicURL, "/") + ClaimPath + "#" + token, nil
}

// adminUserID is the seeded operator row the claim binds.
const adminUserID int64 = 1

func claimHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// claimRefusal reports why this request may not use the claim flow, or "".
func (s *Server) claimRefusal(r *http.Request) (int, string) {
	if s.gt == nil {
		return http.StatusNotFound, ""
	}
	if !viaBrowserListener(r) {
		return http.StatusForbidden, "This port serves muster's machine API only. Open the link at muster's public address" + s.publicURLSuffix() + "."
	}
	return 0, ""
}

func (s *Server) renderInvalidClaim(w http.ResponseWriter) {
	// ONE page for every way a link can be bad, so it cannot be used to learn
	// which tokens exist.
	s.renderNotice(w, http.StatusNotFound, "This link is invalid or expired",
		"Ask the operator for a new link.")
}

func (s *Server) handleClaimPage(w http.ResponseWriter, r *http.Request) {
	if code, msg := s.claimRefusal(r); code != 0 {
		if msg == "" {
			http.NotFound(w, r)
			return
		}
		s.renderNotice(w, code, "Open this link at the public address", msg)
		return
	}
	// The token is in the fragment, so the server cannot check it here; the
	// page reads it into the form and the POST is where it is judged.
	s.renderClaimForm(w, http.StatusOK, "", "", "")
}

func (s *Server) renderClaimForm(w http.ResponseWriter, code int, token, email, problem string) {
	field := func(id, name, label, typ, auto, value string) string {
		return `<label for="` + id + `" class="text-sm font-medium text-fg">` + label + `</label>
<input id="` + id + `" name="` + name + `" type="` + typ + `" autocomplete="` + auto + `" required value="` + html.EscapeString(value) + `"
 class="min-h-[44px] rounded-md bg-bg px-3 py-2 text-base text-fg ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus">`
	}
	writeAuthDocument(w, code, "Claim the admin account", `<form id="claim-form" method="POST" action="`+ClaimPath+`" class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line">
<h1 class="text-balance text-base font-semibold text-fg">Claim the admin account</h1>
<p class="text-sm text-fg2">Choose the email and password you will sign in to muster with. This link works once.</p>
`+problemNode(problem)+`
`+field("claim-email", "email", "Email", "email", "username", email)+`
`+field("claim-password", "password", "Password (at least 12 characters)", "password", "new-password", "")+`
`+field("claim-password2", "password2", "Password again", "password", "new-password", "")+`
`+linkTokenField("claim-token", token)+`
<button type="submit" class="press min-h-[44px] rounded-md bg-accent px-3 py-2 text-base font-semibold text-on-accent transition hover:bg-accent/90">Claim</button>
</form>
`+linkTokenScript("claim-form", "claim-token", false))
}

func (s *Server) handleClaimSubmit(w http.ResponseWriter, r *http.Request) {
	if code, msg := s.claimRefusal(r); code != 0 {
		if msg == "" {
			http.NotFound(w, r)
			return
		}
		s.renderNotice(w, code, "Open this link at the public address", msg)
		return
	}
	if !s.isSameOrigin(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": CrossOriginRefusal})
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderClaimForm(w, http.StatusBadRequest, "", "", "That form could not be read.")
		return
	}
	ip := clientIP(r)
	ticket, ok := s.gt.signin.reserve(signinAccount{}, ip, s.gt.now())
	if !ok {
		s.renderNotice(w, http.StatusTooManyRequests, "Too many attempts", "Wait a few minutes and try again.")
		return
	}
	outcome := outcomeUnjudged
	defer func() { s.gt.signin.settle(ticket, outcome) }()
	token := postedLinkToken(r)
	email := strings.TrimSpace(r.FormValue("email"))
	pw, pw2 := r.FormValue("password"), r.FormValue("password2")
	if token == "" {
		outcome = outcomeFailed
		s.renderInvalidClaim(w)
		return
	}
	if a, err := mail.ParseAddress(email); err != nil || a.Address != email {
		s.renderClaimForm(w, http.StatusBadRequest, token, email, "Enter a valid email address.")
		return
	}
	if len(pw) < gotrue.MinPasswordLength {
		s.renderClaimForm(w, http.StatusBadRequest, token, email, "The password must be at least 12 characters.")
		return
	}
	if pw != pw2 {
		s.renderClaimForm(w, http.StatusBadRequest, token, email, "The two passwords are different.")
		return
	}
	// Compensation (undo): the GoTrue user exists but its muster binding did
	// not commit. Delete it, so the email is not stranded in GoTrue.
	create, undo, createErrp := s.gotrueCreate(r, ip, email, pw, "claim")
	u, err := s.gt.Users.RedeemClaim(r.Context(), claimHash(token), email, create, undo)
	createErr := *createErrp
	switch {
	case errors.Is(err, users.ErrInvalidInvite):
		outcome = outcomeFailed
		s.renderInvalidClaim(w)
		return
	case err != nil && createErr != nil:
		var ae *gotrue.APIError
		if errors.As(createErr, &ae) && ae.Status == http.StatusUnprocessableEntity {
			s.renderClaimForm(w, http.StatusUnprocessableEntity, token, email, "GoTrue refused that email or password ("+ae.Code+").")
			return
		}
		s.logger.Printf("claim: GoTrue create failed: %v", createErr)
		s.renderClaimForm(w, http.StatusBadGateway, token, email, "Sign-in is unavailable right now. Try again shortly.")
		return
	case err != nil:
		s.logger.Printf("claim: redeem failed: %v", err)
		s.renderClaimForm(w, http.StatusInternalServerError, token, email, "The account could not be claimed. Try again.")
		return
	}
	outcome = outcomeSucceeded
	s.gt.forgetUser(u.ID)
	s.logger.Printf("claim: user %d is now bound to a GoTrue login", u.ID)
	// Prove the new login works before telling anyone it does.
	sess, err := s.gt.Client.PasswordGrant(r.Context(), ip, email, pw)
	if err != nil {
		s.logger.Printf("claim: the new login could not sign in: %v", err)
		s.renderNotice(w, http.StatusBadGateway, "Claimed, but sign-in failed",
			"The account is bound, but signing in with it failed. Try signing in again in a moment.")
		return
	}
	if !s.auth.goTrueMode() {
		s.renderNotice(w, http.StatusOK, "Admin account claimed",
			"Your sign-in works. This server still uses the operator password; once it is switched to "+
				"GoTrue sign-in (MUSTER_AUTH_MODE=gotrue) you will sign in with this email and password.")
		return
	}
	if _, problem, code := s.admitFreshSession(r.Context(), sess); problem != "" {
		s.renderNotice(w, code, "Claimed, but sign-in failed", problem)
		return
	}
	s.setGoTrueCookies(w, sess)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
