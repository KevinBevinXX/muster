package api

import (
	"context"
	"errors"
	"html"
	"net/http"
	"net/mail"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/gotrue"
	"github.com/ZacxDev/muster/internal/users"
)

// --- accounts PR 5: redeeming an invite link, and the password reset ----------
//
// Beside the admin claim (login_gotrue.go), and built from the same parts: the
// token in the URL FRAGMENT (never a path, a query or a log line), one uniform
// "invalid or expired" page for every way a link can be bad, the redemption in
// ONE transaction with a compensating GoTrue delete only when the muster side
// is known not to have committed, and a real sign-in at the end.
//
// The page is two POSTs. GET /invite serves a page whose script moves the
// fragment into a form and submits it (step=open); the server answers with the
// account form — the email LOCKED when the invite is bound to one — or the
// uniform page. That form posts step=redeem.

// Redemption throttles.
//
// 🔴 TWO KEYS, BECAUSE A GUESSER AND A LINK-HOLDER ARE DIFFERENT ATTACKS. An
// unknown token has a different hash on every guess, so guessing is limited per
// client address only (inviteIPLimit per minute, every POST) — and a hash that
// names no live invite gets NO per-link record, so a spray of invented tokens
// creates nothing to hold. A REAL link that keeps failing — a bound email
// being guessed, GoTrue's "email exists" used to enumerate accounts — is held
// per INVITE ID, looked up from the hash: after inviteTokenFailures the link
// answers the uniform page for inviteTokenFailWindow, whatever is posted.
//
// 🔴 THE PER-INVITE HOLD IS NEVER EVICTED. It is keyed by invites that exist
// (bounded by what admins create), not by attacker-chosen strings, so it needs
// no capacity bound — and a bounded, evicting map is exactly what a flood of
// other keys could use to push a held link's record out (audit G1).
const (
	inviteIPLimit         = 10
	inviteIPWindow        = time.Minute
	inviteTokenFailures   = 5
	inviteTokenFailWindow = time.Hour
)

// inviteLimits is the redemption pages' throttle state (zero value ready).
type inviteLimits struct {
	ip   boundedWindow // #65's capped window: keys are attacker-chosen
	hold linkHold
}

// linkHold counts failures per invite ID, reserved before the attempt.
//
// 🔴 A SLOT IS TAKEN BEFORE THE ATTEMPT AND REFUNDED IF IT DID NOT FAIL — not
// counted after. A check-then-record counter lets N concurrent attempts all
// pass the check before any records its failure (accounts PR 4's audit found
// that shape in the sign-in limiter). Taking under the lock bounds in-flight
// attempts too (TestConcurrentFailuresCannotOutrunTheLinkHold).
type linkHold struct {
	mu sync.Mutex
	m  map[int64]*linkCount
}

type linkCount struct {
	start time.Time
	n     int
}

func (h *linkHold) take(id int64, now time.Time, window time.Duration, limit int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.m == nil {
		h.m = map[int64]*linkCount{}
	}
	c, ok := h.m[id]
	if !ok || now.Sub(c.start) >= window {
		// Expired records are dropped here, never live ones.
		for k, v := range h.m {
			if now.Sub(v.start) >= window {
				delete(h.m, k)
			}
		}
		h.m[id] = &linkCount{start: now, n: 1}
		return true
	}
	if c.n >= limit {
		return false
	}
	c.n++
	return true
}

func (h *linkHold) refund(id int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c, ok := h.m[id]; ok && c.n > 0 {
		c.n--
	}
}

// inviteRefusal reports why this request may not use the invite or reset flow:
// GoTrue and the account store must be wired, the server must sign people in
// with GoTrue (an invitee could not sign in under the shared password), and the
// request must have come through the browser listener.
func (s *Server) inviteRefusal(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case !s.adminPagesServed():
		http.NotFound(w, r)
	case !s.auth.goTrueMode():
		s.renderNotice(w, http.StatusServiceUnavailable, "Not open yet",
			"This muster does not use per-user sign-in yet. Ask the operator to try the link again later.")
	case !viaBrowserListener(r):
		s.renderNotice(w, http.StatusForbidden, "Open this link at the public address",
			"This port serves muster's machine API only. Open the link at muster's public address"+s.publicURLSuffix()+".")
	case r.Method == http.MethodPost && !s.isSameOrigin(r):
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": CrossOriginRefusal})
	default:
		return false
	}
	return true
}

// renderInvalidInvite is the ONE page for every bad link, so it cannot be used
// to learn which tokens exist, which are bound, or why one failed.
func (s *Server) renderInvalidInvite(w http.ResponseWriter) {
	s.renderNotice(w, http.StatusNotFound, "This link is invalid or expired", "Ask the person who invited you for a new link.")
}

// fragmentForm is the bootstrap page: its script moves the URL fragment into
// the hidden token field, strips it from the address bar, and (auto) submits.
func fragmentForm(action, heading, body, button string, autoSubmit bool, extra, token, problem string) string {
	return `<form id="frag-form" method="POST" action="` + action + `" class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line">
<h1 class="text-balance text-base font-semibold text-fg">` + html.EscapeString(heading) + `</h1>
<p class="text-sm text-fg2">` + html.EscapeString(body) + `</p>
` + problemNode(problem) + extra + `
` + linkTokenField("frag-token", token) + `
<button type="submit" class="press min-h-[44px] rounded-md bg-accent px-3 py-2 text-base font-semibold text-on-accent transition hover:bg-accent/90">` + html.EscapeString(button) + `</button>
</form>
` + linkTokenScript("frag-form", "frag-token", autoSubmit)
}

// --- the link token when the fragment did not arrive -------------------------
//
// 🔴 A FRAGMENT DOES NOT SURVIVE A SIGN-IN REDIRECT. Measured live: with
// Authelia in front of the host, opening <public>/auth/claim#<token> bounced
// through the sign-in portal, whose rd= return URL carries no fragment, so the
// page loaded with an empty token and the POST was refused as "invalid or
// expired". So every link page (claim, invite, reset) renders a VISIBLE "Paste
// your link" field; the page's script fills it from the fragment and hides it
// when the fragment did arrive. The pasted value may be the whole link or the
// bare token: the script keeps only what follows '#' before the form posts,
// and postedLinkToken does the same server-side, so a token is only ever in a
// POST body — never a URL (TestALinkPageWithNoFragmentAsksForThePastedLink).

// pasteLinkLabel is the visible field's label.
const pasteLinkLabel = "Paste your link"

// linkTokenField renders the token input: hidden when the server already has
// the token (a re-render after a form problem), otherwise the paste field.
func linkTokenField(id, token string) string {
	if token != "" {
		return `<input type="hidden" id="` + id + `" name="token" value="` + html.EscapeString(token) + `">`
	}
	return `<div id="` + id + `-paste" class="flex flex-col gap-2">
<label for="` + id + `" class="text-sm font-medium text-fg">` + pasteLinkLabel + `</label>
<p class="text-sm text-fg2">If this page opened without your link (for example after a sign-in page), paste the whole link you were sent.</p>
<input id="` + id + `" name="token" type="text" autocomplete="off" autocapitalize="off" spellcheck="false" required value=""
 class="min-h-[44px] rounded-md bg-bg px-3 py-2 text-base text-fg ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus">
</div>`
}

// linkTokenScript fills the token from the URL fragment (hiding the paste
// field), strips the fragment from the address bar, reduces a pasted full
// link to its fragment on submit, and — for autoSubmit — posts at once when
// the fragment supplied the token.
func linkTokenScript(formID, id string, autoSubmit bool) string {
	auto := ""
	if autoSubmit {
		auto = `if (t.value) { f.submit(); }`
	}
	return `<script>
(function () {
  var f = document.getElementById('` + formID + `'), t = document.getElementById('` + id + `'),
      wrap = document.getElementById('` + id + `-paste');
  if (!t.value && location.hash.length > 1) {
    t.value = location.hash.slice(1);
    if (wrap) { wrap.style.display = 'none'; }
  }
  if (location.hash) { history.replaceState(null, '', location.pathname); }
  function bare() { var v = t.value.trim(), i = v.lastIndexOf('#'); t.value = i >= 0 ? v.slice(i + 1) : v; }
  f.addEventListener('submit', bare);
  ` + auto + `
})();
</script>`
}

// postedLinkToken is the link token a claim, invite or reset form posted:
// the bare token, or — if a whole link was pasted and the page's script did
// not run — the part after its '#'.
func postedLinkToken(r *http.Request) string {
	v := strings.TrimSpace(r.FormValue("token"))
	if i := strings.LastIndex(v, "#"); i >= 0 {
		v = v[i+1:]
	}
	return v
}

func (s *Server) handleInvitePage(w http.ResponseWriter, r *http.Request) {
	if s.inviteRefusal(w, r) {
		return
	}
	writeAuthDocument(w, http.StatusOK, "You are invited", fragmentForm(InvitePath, "You are invited to muster",
		"Continue to set up your account.", "Continue", true, `<input type="hidden" name="step" value="open">`, "", ""))
}

func (s *Server) renderInviteForm(w http.ResponseWriter, code int, token string, inv users.Invite, email, name, problem string) {
	field := func(id, fname, label, typ, auto, value string, required, locked bool) string {
		attrs := ""
		if required {
			attrs += " required"
		}
		if locked {
			attrs += ` readonly aria-readonly="true"`
		}
		return `<label for="` + id + `" class="text-sm font-medium text-fg">` + label + `</label>
<input id="` + id + `" name="` + fname + `" type="` + typ + `" autocomplete="` + auto + `"` + attrs + ` value="` + html.EscapeString(value) + `"
 class="min-h-[44px] rounded-md bg-bg px-3 py-2 text-base text-fg ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus">`
	}
	locked := inv.Email != ""
	if locked {
		email = inv.Email
	}
	writeAuthDocument(w, code, "Create your account", `<form method="POST" action="`+InvitePath+`" class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line">
<h1 class="text-balance text-base font-semibold text-fg">Create your muster account</h1>
<p class="text-sm text-fg2">You are joining as a member. This link works once.</p>
`+problemNode(problem)+`
`+field("invite-email", "email", "Email", "email", "username", email, true, locked)+`
`+field("invite-name", "display_name", "Your name", "text", "name", name, false, false)+`
`+field("invite-password", "password", "Password (at least 12 characters)", "password", "new-password", "", true, false)+`
`+field("invite-password2", "password2", "Password again", "password", "new-password", "", true, false)+`
<input type="hidden" name="token" value="`+html.EscapeString(token)+`">
<input type="hidden" name="step" value="redeem">
<button type="submit" class="press min-h-[44px] rounded-md bg-accent px-3 py-2 text-base font-semibold text-on-accent transition hover:bg-accent/90">Create account</button>
</form>`)
}

func (s *Server) handleInviteSubmit(w http.ResponseWriter, r *http.Request) {
	if s.inviteRefusal(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderInvalidInvite(w)
		return
	}
	ip := clientIP(r)
	now := s.gt.now()
	lim := &s.inviteLimits
	if !lim.ip.take(ip, now, inviteIPWindow, inviteIPLimit) {
		s.renderNotice(w, http.StatusTooManyRequests, "Too many attempts", "Wait a minute and try again.")
		return
	}
	token := postedLinkToken(r)
	if token == "" {
		s.renderInvalidInvite(w)
		return
	}
	hash := claimHash(token)
	inv, err := s.gt.Accounts.OpenInvite(r.Context(), hash)
	if errors.Is(err, users.ErrInvalidInvite) {
		// No live invite: no per-link record (only the per-address count).
		s.renderInvalidInvite(w)
		return
	}
	if err != nil {
		s.logger.Printf("invite: open: %v", err)
		s.renderNotice(w, http.StatusInternalServerError, "Something went wrong", "Try the link again in a moment.")
		return
	}
	if !lim.hold.take(inv.ID, now, inviteTokenFailWindow, inviteTokenFailures) {
		s.renderInvalidInvite(w)
		return
	}
	// The reserved slot is kept only by a failure (bad); every other way out
	// of this handler hands it back.
	failed := false
	defer func() {
		if !failed {
			lim.hold.refund(inv.ID)
		}
	}()
	bad := func() {
		failed = true
		s.renderInvalidInvite(w)
	}
	if r.FormValue("step") != "redeem" {
		s.renderInviteForm(w, http.StatusOK, token, inv, "", "", "")
		return
	}
	email := strings.TrimSpace(r.FormValue("email"))
	name := strings.TrimSpace(r.FormValue("display_name"))
	pw, pw2 := r.FormValue("password"), r.FormValue("password2")
	if inv.Email == "" {
		if a, err := mail.ParseAddress(email); err != nil || a.Address != email {
			s.renderInviteForm(w, http.StatusBadRequest, token, inv, email, name, "Enter a valid email address.")
			return
		}
	}
	if utf8.RuneCountInString(name) > 80 {
		s.renderInviteForm(w, http.StatusBadRequest, token, inv, email, name, "Keep your name under 80 characters.")
		return
	}
	// 🔴 THE 12-CHARACTER FLOOR IS ENFORCED HERE AND IN gotrue.AdminCreateUser:
	// GoTrue's admin path does not check GOTRUE_PASSWORD_MIN_LENGTH (measured,
	// #986). This one answers with the form instead of a GoTrue round trip.
	if len(pw) < gotrue.MinPasswordLength {
		s.renderInviteForm(w, http.StatusBadRequest, token, inv, email, name, "The password must be at least 12 characters.")
		return
	}
	if pw != pw2 {
		s.renderInviteForm(w, http.StatusBadRequest, token, inv, email, name, "The two passwords are different.")
		return
	}
	// A bound invite's email is compared by the store against what was POSTed,
	// so a page whose locked field was edited is refused there
	// (ErrEmailMismatch), never trusted here.
	create, undo, createErr := s.gotrueCreate(r, ip, email, pw, "invite")
	u, err := s.gt.Accounts.RedeemInvite(r.Context(), hash, email, name, create, undo)
	var ae *gotrue.APIError
	switch {
	case errors.Is(err, users.ErrInvalidInvite), errors.Is(err, users.ErrEmailMismatch), errors.Is(err, users.ErrEmailTaken),
		*createErr != nil && errors.As(*createErr, &ae) && ae.Status == http.StatusUnprocessableEntity:
		// Every refusal about the LINK or the IDENTITY is the uniform page, and
		// counts against the link.
		s.logger.Printf("invite: refused a redemption from %s: %v", ip, err)
		bad()
		return
	case err != nil && *createErr != nil:
		s.logger.Printf("invite: GoTrue create failed: %v", *createErr)
		s.renderInviteForm(w, http.StatusBadGateway, token, inv, email, name, "Sign-up is unavailable right now. Try again shortly.")
		return
	case err != nil:
		s.logger.Printf("invite: redeem failed: %v", err)
		s.renderInviteForm(w, http.StatusInternalServerError, token, inv, email, name, "The account could not be created. Try again.")
		return
	}
	s.gt.forgetUser(u.ID)
	s.logger.Printf("invite: user %d (%s) created from invite %d", u.ID, inv.Role, inv.ID)
	// With passkeys on, the new member lands on the offer to add one on this
	// device (skippable); otherwise on the board.
	dest := "/"
	if s.passkeysServed(r.Context()) {
		dest = accountPasskeysPath + "?welcome=1"
	}
	s.signInNewLogin(w, r, ip, email, pw, dest)
}

// gotrueCreate is the create/undo pair a redemption hands the store: create
// makes a CONFIRMED GoTrue user (email_confirm:true, the client's 12-character
// floor, this request's client IP); undo deletes it again, detached from the
// request, when the store reports the muster side did not commit. createErr is
// create's own error, so the caller can tell a GoTrue refusal from a database
// failure. Shared by the admin claim and invite redemption.
func (s *Server) gotrueCreate(r *http.Request, ip, email, pw, what string) (func(context.Context) (string, error), func(string), *error) {
	createErr := new(error)
	create := func(ctx context.Context) (string, error) {
		sub, err := s.gt.Client.AdminCreateUser(ctx, ip, email, pw)
		*createErr = err
		return sub, err
	}
	undo := func(sub string) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
		defer cancel()
		if err := s.gt.Client.AdminDeleteUser(ctx, ip, sub); err != nil {
			s.logger.Printf("%s: COMPENSATION FAILED — GoTrue user %s has no muster binding and could not be deleted: %v", what, sub, err)
		}
	}
	return create, undo, createErr
}

// signInNewLogin signs a just-created (or just-reset) login in: a real
// password grant — which proves the login works — the same admission checks
// /login applies, cookies, and a redirect to dest (a local path).
func (s *Server) signInNewLogin(w http.ResponseWriter, r *http.Request, ip, email, pw, dest string) {
	sess, err := s.gt.Client.PasswordGrant(r.Context(), ip, email, pw)
	if err != nil {
		s.logger.Printf("sign-in after account setup failed: %v", err)
		s.renderNotice(w, http.StatusBadGateway, "Your account is ready, but sign-in failed",
			"Sign in with your email and new password in a moment.")
		return
	}
	if _, problem, code := s.admitFreshSession(r.Context(), sess); problem != "" {
		s.renderNotice(w, code, "Your account is ready, but sign-in failed", problem)
		return
	}
	s.setGoTrueCookies(w, sess)
	http.Redirect(w, r, safeReturnTo(dest), http.StatusSeeOther)
}

// registerInviteRoutes wires the invitee's page and the password reset. Open
// by necessity (the visitor has no session yet); literal patterns, for the
// reason registerLoginRoutes gives.
func (s *Server) registerInviteRoutes(mux Mux) {
	mux.HandleFunc("GET /invite", s.handleInvitePage)
	mux.HandleFunc("POST /invite", s.handleInviteSubmit)
	mux.HandleFunc("GET /auth/reset", s.handleResetPage)
	mux.HandleFunc("POST /auth/reset", s.handleResetSubmit)
}

// --- the password reset ---------------------------------------------------------

func (s *Server) handleResetPage(w http.ResponseWriter, r *http.Request) {
	if s.inviteRefusal(w, r) {
		return
	}
	renderResetForm(w, http.StatusOK, "", "")
}

func renderResetForm(w http.ResponseWriter, code int, token, problem string) {
	pw := func(id, name, label string) string {
		return `<label for="` + id + `" class="text-sm font-medium text-fg">` + label + `</label>
<input id="` + id + `" name="` + name + `" type="password" autocomplete="new-password" required
 class="min-h-[44px] rounded-md bg-bg px-3 py-2 text-base text-fg ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus">`
	}
	writeAuthDocument(w, code, "Set a new password", fragmentForm(ResetPath, "Set a new password",
		"This link works once.", "Set password", false,
		pw("reset-password", "password", "New password (at least 12 characters)")+pw("reset-password2", "password2", "New password again"),
		token, problem))
}

// handleResetSubmit exchanges the recovery token SERVER-SIDE (GoTrue /verify),
// sets the new password through the admin path, and signs the user in. The
// token never reaches GoTrue from the browser, and never a log.
func (s *Server) handleResetSubmit(w http.ResponseWriter, r *http.Request) {
	if s.inviteRefusal(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderInvalidInvite(w)
		return
	}
	ip := clientIP(r)
	now := s.gt.now()
	if !s.inviteLimits.ip.take(ip, now, inviteIPWindow, inviteIPLimit) {
		s.renderNotice(w, http.StatusTooManyRequests, "Too many attempts", "Wait a minute and try again.")
		return
	}
	token, pw, pw2 := postedLinkToken(r), r.FormValue("password"), r.FormValue("password2")
	if token == "" {
		s.renderInvalidInvite(w)
		return
	}
	if len(pw) < gotrue.MinPasswordLength || pw != pw2 {
		// Refused before GoTrue sees the token, so it is still unspent: the
		// form comes back carrying it.
		msg := "The password must be at least 12 characters."
		if len(pw) >= gotrue.MinPasswordLength {
			msg = "The two passwords are different."
		}
		renderResetForm(w, http.StatusBadRequest, token, msg)
		return
	}
	sess, err := s.gt.Client.VerifyRecovery(r.Context(), ip, token)
	if gotrue.IsClientError(err) {
		s.renderInvalidInvite(w)
		return
	}
	if err != nil {
		s.logger.Printf("reset: GoTrue /verify failed: %v", err)
		s.renderNotice(w, http.StatusBadGateway, "Password reset is unavailable right now", "Try the link again shortly.")
		return
	}
	c, err := s.gt.Verifier.Verify(sess.AccessToken)
	if err != nil {
		s.logger.Printf("reset: a token GoTrue just issued does not verify: %v", err)
		s.renderNotice(w, http.StatusInternalServerError, "Password reset is misconfigured on this server", "Tell the operator.")
		return
	}
	u, err := s.gt.Users.BySub(r.Context(), c.Subject)
	if err != nil || u.Disabled() {
		// A recovery token for a login with no (enabled) muster account sets
		// nothing.
		s.renderInvalidInvite(w)
		return
	}
	// 🔴 THE SESSION CUTOFF MOVES FIRST, BEFORE THE PASSWORD AND BEFORE THE NEW
	// SESSION. GoTrue's admin password change revokes the user's REFRESH tokens,
	// but an access token is a signed statement GoTrue cannot recall: without
	// the cutoff another device's access token kept working until its exp (up
	// to JWT_EXP, 900 s). With it, that token is refused on its next request
	// (TestAResetEndsOtherDevicesAccessTokensAtOnce). Moving it first means a
	// failure below leaves the old sessions ended and the password unchanged —
	// the safe half — never the reverse.
	// ResetCredentials also REVOKES every personal API token the user holds, in
	// the cutoff's transaction (accounts PR 6): a reset is what someone does
	// after a credential leaked, and a token is a credential.
	cut, err := s.gt.Accounts.ResetCredentials(r.Context(), u.ID)
	if err != nil {
		s.logger.Printf("reset: moving the session cutoff for user %d failed: %v", u.ID, err)
		// GoTrue has already SPENT the single-use recovery token (/verify
		// above), so the link cannot be tried again.
		s.renderNotice(w, http.StatusInternalServerError, "The password could not be set", "Ask for a new reset link.")
		return
	}
	s.gt.forgetUser(u.ID)
	// 🔴 AND END WHAT IS ALREADY OPEN. The cutoff refuses the other devices'
	// NEXT requests, but an SSE stream or a terminal/chat socket was admitted
	// before it and is checked once, at connect — so a disable and a
	// sign-out-everywhere end them, and so must a reset. The session the reset
	// is about to sign in is not open yet, so ending every one of the user's
	// streams spares nothing it should (TestAResetEndsTheOtherDevicesOpenStreams).
	// Recorded like a sign-out-everywhere, so a request that resolved before
	// this point is refused when it registers after it (recordReset).
	s.gt.recordReset(u.ID, c.Subject, cut)
	// 🔴 AND EVERY PASSKEY GOES, BEFORE THE PASSWORD IS SET. A reset is what
	// someone does after a credential leaked or a device was lost; a passkey on
	// that device is a credential too, and the cutoff above only ends SESSIONS —
	// a passkey would sign the thief straight back in. Deleted through GoTrue's
	// admin API (list, then each). FAIL CLOSED: if any may remain, the password
	// is not set and the person is told to ask for a new link — the sessions are
	// already ended, which is the safe half (TestAResetDeletesEveryPasskey).
	if n, err := s.gt.Client.AdminDeleteAllPasskeys(r.Context(), ip, c.Subject); err != nil {
		s.logger.Printf("reset: removing the passkeys of user %d failed after %d: %v", u.ID, n, err)
		s.renderNotice(w, http.StatusBadGateway, "The password could not be set", "Ask for a new reset link.")
		return
	} else if n > 0 {
		s.logger.Printf("reset: removed %d passkeys of user %d", n, u.ID)
		s.setPasskeyFlag(r.Context(), c.Subject, "", nil)
	}
	// The audit row (actor: the user; the client address in the detail)
	// commits only if GoTrue set the password.
	//
	// ⚠ NO /logout OF THE RECOVERY SESSION AFTERWARDS. It always answered 403
	// against the real GoTrue: the admin password change already revokes the
	// refresh side of every session the user has (the recovery one included),
	// and the cutoff above refuses their access tokens
	// (TestAResetRevokesTheUsersOtherSessions).
	if err := s.gt.Accounts.Audited(r.Context(), users.Actor{UserID: u.ID, Via: authz.Via("reset")}, "user.password_reset",
		users.UserObject(u.ID), map[string]any{"ip": ip}, func() error {
			return s.gt.Client.AdminSetPassword(r.Context(), ip, c.Subject, pw)
		}); err != nil {
		s.logger.Printf("reset: setting the password for user %d failed: %v", u.ID, err)
		s.renderNotice(w, http.StatusBadGateway, "The password could not be set", "Ask for a new reset link.")
		return
	}
	s.logger.Printf("reset: user %d set a new password", u.ID)
	s.signInAfterCutoff(w, r, ip, u.Email, pw, cut)
}

// signInAfterCutoff is signInNewLogin for a session that must be issued AFTER
// cut. iat is whole seconds and the session tier refuses iat <= cut's second
// (issuedBeforeCutoff), so a sign-in in the cutoff's own second would mint a
// session muster refuses on its next request. It waits for the next second
// first, and checks what GoTrue issued: a token still inside the cutoff's
// second (GoTrue's clock behind Postgres's) is signed out again and the grant
// retried, at most three times.
// resetTokensRevoked is appended to every page a reset that SET the password
// ends on: the reset revoked the user's API tokens (users.ResetCredentials).
const resetTokensRevoked = "Your API tokens were revoked."

func (s *Server) signInAfterCutoff(w http.ResponseWriter, r *http.Request, ip, email, pw string, cut time.Time) {
	for attempt := 0; attempt < 3; attempt++ {
		// Attempt n waits until n+1 whole seconds past the cutoff's second.
		if wait := time.Until(cut.Truncate(time.Second).Add(time.Duration(attempt+1) * time.Second)); wait > 0 {
			select {
			case <-time.After(wait):
			case <-r.Context().Done():
				return
			}
		}
		sess, err := s.gt.Client.PasswordGrant(r.Context(), ip, email, pw)
		if err != nil {
			s.logger.Printf("sign-in after the reset failed: %v", err)
			break
		}
		c, err := s.gt.Verifier.Verify(sess.AccessToken)
		if err == nil && c.IssuedAt <= cut.Unix() {
			_ = s.gt.Client.Logout(r.Context(), ip, sess.AccessToken, gotrue.ScopeLocal)
			continue
		}
		if _, problem, code := s.admitFreshSession(r.Context(), sess); problem != "" {
			s.renderNotice(w, code, "Your password is set, but sign-in failed", problem+" "+resetTokensRevoked)
			return
		}
		s.setGoTrueCookies(w, sess)
		// To /account, which says the reset revoked the user's API tokens
		// (accounts PR 6), rather than straight to the board.
		http.Redirect(w, r, accountResetDonePath, http.StatusSeeOther)
		return
	}
	s.renderNotice(w, http.StatusBadGateway, "Your password is set, but sign-in failed",
		"Sign in with your email and new password in a moment. "+resetTokensRevoked)
}
