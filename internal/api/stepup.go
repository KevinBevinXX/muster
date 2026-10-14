package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/ZacxDev/muster/internal/auth"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/gotrue"
	"github.com/ZacxDev/muster/internal/stepup"
)

// ---------------------------------------------------------------------------
// STEP-UP: "Confirm it's you" — a FRESH sign-in for an action worth more than
// an ordinary signed-in request (internal/stepup). Today those actions are
// typing into a mirrored host — attaching its terminal INTERACTIVELY (a tmux
// client, and so a shell, on that host) or sending a message into a session —
// and, on its own shorter window, adding a passkey.
//
// There are two ways to confirm, and both END THE SAME WAY: the session is
// replaced with a brand-new GoTrue session, for the SAME user, whose sign-in
// time is now.
//
//   - POST /auth/confirm takes the SIGNED-IN person's password again.
//   - POST /auth/confirm/passkey/options then …/verify take a passkey
//     assertion with user verification (handleStepUpPasskeyVerify).
//
// 🔴 FRESHNESS IS THE CONFIRMING SESSION'S ALONE. The new session's cookies are
// set on the response to the browser that confirmed, and the gate reads the
// sign-in time out of the session a request PRESENTS (requireGoTrueSession →
// stepup.WithAuthTime). Nothing is recorded per user, so the same person's
// other browsers and devices are exactly as stale as before
// (TestAConfirmOnOneSessionLeavesAnotherStale).
//
// ⚠ THE OLD SESSION IS LEFT TO EXPIRE, not signed out: its cookies were just
// overwritten in the one browser that held them, and signing it out would cost
// a GoTrue call per confirm to end something nobody can present any more.
//
// 🔴 WHO IS CONFIRMED IS THE SESSION'S USER, NEVER THE FORM'S. There is no email
// field: the email the password is checked against is read from the session's
// own users row, so another account's password cannot be presented here (it is
// simply the wrong password for THIS account), and the new session must name the
// same muster user or it is refused before any cookie is set.
//
// 🔴 THE SAME THROTTLE AS SIGN-IN. In gotrue mode the attempt is reserved in
// the sign-in limiter under the session's account, before GoTrue is asked
// (signinLimiter) — a confirm is a password guess like any other, and a stolen
// session must not be a way to guess the password faster than /login allows.
// In password mode it is the same loginRate as /login.
//
// 🔴 SAME-ORIGIN ONLY. A cross-site page could otherwise submit a guess with the
// victim's cookies attached; it would learn nothing from the opaque answer, but
// it would burn the victim's sign-in budget.
//
// THE GATE DOES NOT KNOW HOW THE PERSON CONFIRMED: it reads GoTrue's `amr`
// timestamp whatever its method (gotrue.Claims.AuthenticatedAt). A passkey
// sign-in at /login and a passkey confirm here both mint a GoTrue session with
// a fresh `amr`, so after either the Interactive switch attaches with no
// prompt.
// ---------------------------------------------------------------------------

// stepUpConfirmPath is the confirm route. Spelled literally in
// registerMirrorRoutes (see registerLoginRoutes on why literals).
const stepUpConfirmPath = "/auth/confirm"

// stepUpPasskeyPath is the passkey confirm's route PREFIX: the dialog posts to
// <prefix>/options and <prefix>/verify. Spelled literally in
// registerMirrorRoutes.
const stepUpPasskeyPath = "/auth/confirm/passkey"

// StepUpOtherAccountRefusal answers a passkey that verified — for somebody
// else. The dialog shows it.
const StepUpOtherAccountRefusal = "That passkey belongs to a different account. Use a passkey for the account you are signed in to, or your password."

// Step-up refusals the terminal page shows verbatim.
const (
	stepUpWrongPassword = "That password is not right."
	stepUpThrottled     = "Too many attempts. Wait a few minutes and try again."
	stepUpUnavailable   = "Sign-in is unavailable right now. Try again shortly."
)

// stepUpNow is the clock step-up freshness is judged on: GoTrue's (the one its
// tokens' times are compared with) in gotrue mode, the wall clock otherwise.
func (s *Server) stepUpNow() time.Time {
	if s.auth.goTrueMode() && s.gt != nil {
		return s.gt.now()
	}
	return time.Now()
}

// stepUpResult is the confirm route's success body: how many milliseconds the
// new sign-in stays fresh. A duration, not an instant, so the browser's clock
// does not matter (the page counts it down on performance.now()).
type stepUpResult struct {
	FreshFor int64 `json:"fresh_for"`
}

func (s *Server) handleStepUpConfirm(w http.ResponseWriter, r *http.Request) {
	if !s.isSameOrigin(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": CrossOriginRefusal})
		return
	}
	if err := r.ParseForm(); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "That form could not be read."})
		return
	}
	// PostForm, never Form: a password in the URL would be in the access log.
	o := s.stepUpWithPassword(w, r, r.PostForm.Get("password"))
	if o.Error != "" {
		s.writeJSON(w, o.Status, map[string]any{"error": o.Error})
		return
	}
	s.writeJSON(w, http.StatusOK, stepUpResult{FreshFor: o.FreshFor})
}

// stepUpOutcome is how a password confirm ended.
type stepUpOutcome struct {
	// Error is the refusal the person is shown, "" when the confirm succeeded;
	// Status is its HTTP status.
	Error  string
	Status int
	// FreshFor is how many milliseconds the new sign-in stays fresh, and At is
	// that sign-in's time on the gate's own clock (success only).
	FreshFor int64
	At       time.Time
}

func stepUpRefused(status int, msg string) stepUpOutcome {
	return stepUpOutcome{Error: msg, Status: status}
}

// stepUpWithPassword is the password confirm, for BOTH of its callers: the
// dialog's route (handleStepUpConfirm, which answers JSON) and the plain confirm
// page of a send the browser submitted (mirror_view.go, which answers a page).
// On success the new session's cookies are ALREADY SET on w when it returns;
// on refusal nothing is.
//
// 🔴 ONE CHECK, ONE THROTTLE, ONE PLACE. The second caller exists so that a page
// whose script did not run can still confirm; a second copy of the check would
// be a second way to guess a password, with its own idea of the limit.
func (s *Server) stepUpWithPassword(w http.ResponseWriter, r *http.Request, password string) stepUpOutcome {
	if s.auth.goTrueMode() {
		return s.confirmGoTrue(w, r, password)
	}
	return s.confirmOperatorPassword(w, password)
}

// confirmGoTrue re-checks the session user's password with a GoTrue password
// grant and, on success, swaps in the new session.
func (s *Server) confirmGoTrue(w http.ResponseWriter, r *http.Request, password string) stepUpOutcome {
	if s.gt == nil {
		return stepUpRefused(http.StatusServiceUnavailable, stepUpUnavailable)
	}
	ctx := r.Context()
	p := authz.MustFrom(ctx)
	me, err := s.gt.Users.ByID(ctx, p.UserID)
	if err != nil {
		s.logger.Printf("stepup: could not read user %d: %v", p.UserID, err)
		return stepUpRefused(http.StatusServiceUnavailable, stepUpUnavailable)
	}
	ip := clientIP(r)
	ticket, ok := s.gt.signin.reserve(signinAccount{known: true, uid: me.ID}, ip, s.gt.now())
	if !ok {
		return stepUpRefused(http.StatusTooManyRequests, stepUpThrottled)
	}
	outcome := outcomeUnjudged
	defer func() { s.gt.signin.settle(ticket, outcome) }()
	if password == "" {
		outcome = outcomeFailed
		return stepUpRefused(http.StatusUnauthorized, stepUpWrongPassword)
	}
	sess, err := s.gt.Client.PasswordGrant(ctx, ip, me.Email, password)
	if err != nil {
		var ae *gotrue.APIError
		switch {
		case errors.As(err, &ae) && ae.Status == http.StatusTooManyRequests:
			return stepUpRefused(http.StatusTooManyRequests, stepUpThrottled)
		case gotrue.IsClientError(err):
			outcome = outcomeFailed
			s.logger.Printf("stepup: user %d failed to confirm (from %s)", me.ID, ip)
			return stepUpRefused(http.StatusUnauthorized, stepUpWrongPassword)
		default:
			s.logger.Printf("stepup: GoTrue is unavailable: %v", err)
			return stepUpRefused(http.StatusBadGateway, stepUpUnavailable)
		}
	}
	u, problem, code := s.admitFreshSession(ctx, sess)
	if problem != "" {
		return stepUpRefused(code, problem)
	}
	if u.ID != me.ID {
		// The email resolved to a different muster user than the session's —
		// never swap one person's session for another's.
		outcome = outcomeFailed
		s.logger.Printf("stepup: refusing a confirm whose new session is user %d, not the session's user %d", u.ID, me.ID)
		return stepUpRefused(http.StatusForbidden, stepUpWrongPassword)
	}
	c, err := s.gt.Verifier.Verify(sess.AccessToken)
	if err != nil {
		// admitFreshSession verified it a moment ago; this cannot differ.
		return stepUpRefused(http.StatusInternalServerError, stepUpUnavailable)
	}
	at, ok := c.AuthenticatedAt()
	if !ok {
		// Loud, not silent: without `amr` no session can ever be fresh, so the
		// Interactive switch could never attach. Say why.
		s.logger.Printf("stepup: GoTrue issued a session with no `amr` claim — the step-up gate cannot be satisfied")
		return stepUpRefused(http.StatusBadGateway, "GoTrue's session has no sign-in time (amr), so it cannot be confirmed.")
	}
	outcome = outcomeSucceeded
	s.setGoTrueCookies(w, sess)
	s.logger.Printf("stepup: user %d confirmed (fresh sign-in)", me.ID)
	return stepUpOutcome{Status: http.StatusOK, At: at, FreshFor: max(0, at.Add(stepup.Window()).Sub(s.stepUpNow()).Milliseconds())}
}

// --- the passkey confirm ------------------------------------------------------
//
// The same WebAuthn ceremony as a passkey sign-in (passkeys.go), with three
// differences that make it a CONFIRM rather than a sign-in:
//
//   - it needs a session (requireSession), and the session it mints must be
//     the SAME GoTrue user's or it is refused and ended;
//   - it is throttled under the session's ACCOUNT, like the password confirm,
//     not under the client address like the anonymous sign-in;
//   - the options call first asks GoTrue whether this account HAS a passkey,
//     and mints no challenge when it has none.

// stepUpPasskeyRefusal answers (and returns true) when this request may not
// use the passkey confirm: a cross-origin POST, a server not in gotrue mode or
// with passkeys off in GoTrue, or a request with no GoTrue session on it. The
// session itself was required by the route's wrapper.
func (s *Server) stepUpPasskeyRefusal(w http.ResponseWriter, r *http.Request) (gotrueCreds, bool) {
	if !s.isSameOrigin(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": CrossOriginRefusal})
		return gotrueCreds{}, true
	}
	creds, ok := gotrueCredsFrom(r.Context())
	if !ok || !s.passkeysServed(r.Context()) {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": PasskeysOffRefusal})
		return gotrueCreds{}, true
	}
	return creds, false
}

// handleStepUpPasskeyOptions is POST /auth/confirm/passkey/options: whether
// the SESSION's account has a passkey and, when it does, a WebAuthn challenge
// for it.
//
// 🔴 NO PASSKEY, NO CHALLENGE. An account without one answers
// {"available":false} after a single list call and mints nothing — the dialog
// then offers the password only. A challenge costs GoTrue's passkey rate
// budget (bucketed on the client address), and a confirm must not spend it for
// someone who could not use it.
func (s *Server) handleStepUpPasskeyOptions(w http.ResponseWriter, r *http.Request) {
	creds, refused := s.stepUpPasskeyRefusal(w, r)
	if refused {
		return
	}
	ip := clientIP(r)
	list, err := s.gt.Client.Passkeys(r.Context(), ip, creds.access)
	if err != nil {
		s.writeGoTrueRefusal(w, "the passkey list (confirm)", err, PasskeysOffRefusal)
		return
	}
	if len(list) == 0 {
		s.writeJSON(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	o, err := s.gt.Client.PasskeySignInOptions(r.Context(), ip)
	if err != nil {
		s.writeGoTrueRefusal(w, "confirm options", err, PasskeysOffRefusal)
		return
	}
	// The browser-side half of the UV requirement; the flag check in verify is
	// what enforces it.
	opts, err := gotrue.RequireUserVerification(o.Options, false)
	if err != nil {
		s.writeGoTrueRefusal(w, "confirm options", err, PasskeysOffRefusal)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"available": true, "challenge_id": o.ChallengeID, "options": json.RawMessage(opts)})
}

// handleStepUpPasskeyVerify is POST /auth/confirm/passkey/verify: a passkey
// assertion that, when it is this session's user's and carries user
// verification, replaces the session with a freshly signed-in one.
//
// HOW THE ASSERTION IS VERIFIED, AND BY WHOM:
//
//  1. muster reads the authenticator-data flags of the credential and refuses
//     one without USER VERIFICATION before GoTrue sees it — the same guard as
//     the passkey sign-in, for the same reason (GoTrue's own setting is
//     "preferred"; gotrue/webauthn.go). The credential forwarded is the
//     canonical re-encoding of the one checked.
//  2. GoTrue verifies the signature against the challenge it issued (single
//     use, origin and RP id bound) and the stored public key, and issues a
//     session for the user the credential belongs to.
//  3. 🔴 muster BINDS THAT SESSION TO THE ONE CONFIRMING: the new access
//     token's `sub` must be the `sub` of the session this request presented.
//     GoTrue's ceremony is usernameless — it answers "whose passkey is this",
//     not "is it THIS person's" — so without this check anyone holding any
//     account's passkey could make another person's session fresh, and worse,
//     the cookies set below would sign this browser in as them. A mismatch is
//     refused, sets no cookie, and the session GoTrue just minted is ended.
//
// The same sign-in limiter as the password confirm, under the session's
// account: a confirm is an authentication attempt, and a held session must not
// be a way to try credentials faster than /login allows.
func (s *Server) handleStepUpPasskeyVerify(w http.ResponseWriter, r *http.Request) {
	creds, refused := s.stepUpPasskeyRefusal(w, r)
	if refused {
		return
	}
	var body struct {
		ChallengeID string          `json:"challenge_id"`
		Credential  json.RawMessage `json:"credential"`
	}
	if !readPasskeyBody(w, r, &body) || body.ChallengeID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": PasskeyUnreadable})
		return
	}
	ctx := r.Context()
	me := authz.MustFrom(ctx).UserID
	ip := clientIP(r)
	ticket, ok := s.gt.signin.reserve(signinAccount{known: true, uid: me}, ip, s.gt.now())
	if !ok {
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": stepUpThrottled})
		return
	}
	outcome := outcomeUnjudged
	defer func() { s.gt.signin.settle(ticket, outcome) }()
	flags, canonical, err := gotrue.CanonicalAssertion(body.Credential)
	if err != nil {
		outcome = outcomeFailed
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": PasskeyUnreadable})
		return
	}
	// 🔴 THE UV GUARD. A passkey that only proved PRESENCE is "whoever is
	// holding the unlocked device", which a step-up exists to rule out.
	if !flags.UserPresent || !flags.UserVerified {
		outcome = outcomeFailed
		s.logger.Printf("stepup: refused a passkey confirm without user verification for user %d (from %s)", me, ip)
		s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": PasskeyNoUVRefusal})
		return
	}
	sess, err := s.gt.Client.PasskeySignInVerify(ctx, ip, body.ChallengeID, canonical)
	if err != nil {
		if gotrue.IsClientError(err) && !isStatus(err, http.StatusTooManyRequests) {
			outcome = outcomeFailed
		}
		s.writeGoTrueRefusal(w, "a passkey confirm", err, PasskeyRejectedRefusal)
		return
	}
	// end signs the just-minted session out of GoTrue: it is not going to be
	// handed to anyone.
	end := func() {
		ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_ = s.gt.Client.Logout(ectx, ip, sess.AccessToken, gotrue.ScopeLocal)
		cancel()
	}
	c, err := s.gt.Verifier.Verify(sess.AccessToken)
	if err != nil {
		end()
		s.logger.Printf("stepup: a token GoTrue just issued for a passkey confirm does not verify: %v", err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": stepUpUnavailable})
		return
	}
	// 🔴 THE USER BINDING (step 3 above), BEFORE anything admits the session:
	// admitFreshSession exempts the session it is given from this user's
	// sign-out-everywhere cutoff (signedIn), which another person's session
	// must never earn here.
	if c.Subject != creds.claims.Subject {
		outcome = outcomeFailed
		end()
		s.logger.Printf("stepup: refusing a passkey confirm for user %d: the passkey is another account's (from %s)", me, ip)
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": StepUpOtherAccountRefusal})
		return
	}
	// The same sub is the same muster user (users.gotrue_sub is unique), so
	// what is left to check is what a sign-in checks: still a muster account,
	// not disabled.
	if _, problem, code := s.admitFreshSession(ctx, sess); problem != "" {
		if code != http.StatusInternalServerError {
			outcome = outcomeFailed
			end()
		}
		s.writeJSON(w, code, map[string]any{"error": problem})
		return
	}
	at, ok := c.AuthenticatedAt()
	if !ok {
		end()
		s.logger.Printf("stepup: GoTrue issued a passkey session with no `amr` claim — the step-up gate cannot be satisfied")
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"error": "GoTrue's session has no sign-in time (amr), so it cannot be confirmed."})
		return
	}
	outcome = outcomeSucceeded
	s.setGoTrueCookies(w, sess)
	s.logger.Printf("stepup: user %d confirmed with a passkey (fresh sign-in)", me)
	s.writeJSON(w, http.StatusOK, stepUpResult{FreshFor: max(0, at.Add(stepup.Window()).Sub(s.stepUpNow()).Milliseconds())})
}

// confirmOperatorPassword is the password-mode confirm: the operator password
// again, a new session cookie issued now.
func (s *Server) confirmOperatorPassword(w http.ResponseWriter, password string) stepUpOutcome {
	now := time.Now()
	if !s.loginRate.allow(now, loginRateBurst, loginRateWindow) {
		return stepUpRefused(http.StatusTooManyRequests, stepUpThrottled)
	}
	if password == "" || !auth.ConstantTimeEqual(password, s.auth.UIPassword) {
		s.logger.Printf("stepup: rejected an operator-password confirm")
		return stepUpRefused(http.StatusUnauthorized, stepUpWrongPassword)
	}
	s.setPasswordSessionCookie(w)
	// SignSession stamps whole seconds; report what the cookie will read as.
	issued := time.Unix(now.Unix(), 0)
	return stepUpOutcome{Status: http.StatusOK, At: issued, FreshFor: issued.Add(stepup.Window()).Sub(now).Milliseconds()}
}
