package api

import (
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
// an ordinary signed-in request (internal/stepup). Today that action is one:
// attaching a mirrored host's terminal INTERACTIVELY, which is a tmux client —
// and so a shell — on that host.
//
// POST /auth/confirm takes the SIGNED-IN person's password again and, when it is
// right, replaces the session with a brand-new one whose sign-in time is now.
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
// A PASSKEY SIGN-IN COUNTS TOO, without anything here knowing about passkeys:
// the gate reads GoTrue's `amr` timestamp whatever its method
// (gotrue.Claims.AuthenticatedAt). A passkey route that mints a GoTrue session
// leaves a fresh `amr` behind, so after it the Interactive switch attaches with
// no prompt.
// ---------------------------------------------------------------------------

// stepUpConfirmPath is the confirm route. Spelled literally in
// registerMirrorRoutes (see registerLoginRoutes on why literals).
const stepUpConfirmPath = "/auth/confirm"

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
	password := r.PostForm.Get("password")
	if s.auth.goTrueMode() {
		s.confirmGoTrue(w, r, password)
		return
	}
	s.confirmOperatorPassword(w, password)
}

// confirmGoTrue re-checks the session user's password with a GoTrue password
// grant and, on success, swaps in the new session.
func (s *Server) confirmGoTrue(w http.ResponseWriter, r *http.Request, password string) {
	if s.gt == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": stepUpUnavailable})
		return
	}
	ctx := r.Context()
	p := authz.MustFrom(ctx)
	me, err := s.gt.Users.ByID(ctx, p.UserID)
	if err != nil {
		s.logger.Printf("stepup: could not read user %d: %v", p.UserID, err)
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": stepUpUnavailable})
		return
	}
	ip := clientIP(r)
	ticket, ok := s.gt.signin.reserve(signinAccount{known: true, uid: me.ID}, ip, s.gt.now())
	if !ok {
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": stepUpThrottled})
		return
	}
	outcome := outcomeUnjudged
	defer func() { s.gt.signin.settle(ticket, outcome) }()
	if password == "" {
		outcome = outcomeFailed
		s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": stepUpWrongPassword})
		return
	}
	sess, err := s.gt.Client.PasswordGrant(ctx, ip, me.Email, password)
	if err != nil {
		var ae *gotrue.APIError
		switch {
		case errors.As(err, &ae) && ae.Status == http.StatusTooManyRequests:
			s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": stepUpThrottled})
		case gotrue.IsClientError(err):
			outcome = outcomeFailed
			s.logger.Printf("stepup: user %d failed to confirm (from %s)", me.ID, ip)
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": stepUpWrongPassword})
		default:
			s.logger.Printf("stepup: GoTrue is unavailable: %v", err)
			s.writeJSON(w, http.StatusBadGateway, map[string]any{"error": stepUpUnavailable})
		}
		return
	}
	u, problem, code := s.admitFreshSession(ctx, sess)
	if problem != "" {
		s.writeJSON(w, code, map[string]any{"error": problem})
		return
	}
	if u.ID != me.ID {
		// The email resolved to a different muster user than the session's —
		// never swap one person's session for another's.
		outcome = outcomeFailed
		s.logger.Printf("stepup: refusing a confirm whose new session is user %d, not the session's user %d", u.ID, me.ID)
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": stepUpWrongPassword})
		return
	}
	c, err := s.gt.Verifier.Verify(sess.AccessToken)
	if err != nil {
		// admitFreshSession verified it a moment ago; this cannot differ.
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": stepUpUnavailable})
		return
	}
	at, ok := c.AuthenticatedAt()
	if !ok {
		// Loud, not silent: without `amr` no session can ever be fresh, so the
		// Interactive switch could never attach. Say why.
		s.logger.Printf("stepup: GoTrue issued a session with no `amr` claim — the step-up gate cannot be satisfied")
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"error": "GoTrue's session has no sign-in time (amr), so it cannot be confirmed."})
		return
	}
	outcome = outcomeSucceeded
	s.setGoTrueCookies(w, sess)
	s.logger.Printf("stepup: user %d confirmed (fresh sign-in)", me.ID)
	s.writeJSON(w, http.StatusOK, stepUpResult{FreshFor: max(0, at.Add(stepup.MaxAge).Sub(s.stepUpNow()).Milliseconds())})
}

// confirmOperatorPassword is the password-mode confirm: the operator password
// again, a new session cookie issued now.
func (s *Server) confirmOperatorPassword(w http.ResponseWriter, password string) {
	now := time.Now()
	if !s.loginRate.allow(now, loginRateBurst, loginRateWindow) {
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": stepUpThrottled})
		return
	}
	if password == "" || !auth.ConstantTimeEqual(password, s.auth.UIPassword) {
		s.logger.Printf("stepup: rejected an operator-password confirm")
		s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": stepUpWrongPassword})
		return
	}
	s.setPasswordSessionCookie(w)
	// SignSession stamps whole seconds; report what the cookie will read as.
	issued := time.Unix(now.Unix(), 0)
	s.writeJSON(w, http.StatusOK, stepUpResult{FreshFor: issued.Add(stepup.MaxAge).Sub(now).Milliseconds()})
}
