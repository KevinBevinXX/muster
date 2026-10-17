package api

import (
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/auth"
	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/web"
)

// --- The human login tier ----------------------------------------------------
//
// 🔴 THIS PAGE IS DELIBERATELY NOT AN internal/ui DOCUMENT, and the reason is
// operational rather than stylistic. It is the one page that must render while
// the rest of the app is refusing, so it carries no shared shell, no htmx, no
// sidebar and no dependency on the ui package's component graph — a refactor
// there cannot lock the operator out. It also keeps it out of that package's
// standaloneDocuments registry, which would otherwise subject a bare auth form
// to ~20 shell/tab/panel sweeps that have nothing to say about it.
//
// ⚠ THAT DODGES THE a11y SWEEPS, SO THE COVERAGE IS REPLACED, NOT DROPPED:
// TestLoginPageIsAccessible pins the properties those sweeps would have checked
// (exactly one h1, a label bound to the input, no low-contrast idiom, a typed
// password field with the right autocomplete) against this markup specifically.

// loginPath is the login route, spelled once so the handler, the redirect in
// refuseUnauthenticated and the tests cannot drift.
const loginPath = "/login"

// loginRateBurst / loginRateWindow bound password guesses.
//
// 🔴 THE BUCKET IS PROCESS-WIDE, NOT PER-IP, AND THAT IS THE CONSERVATIVE
// CHOICE HERE. A per-IP bucket is keyed on something the caller picks — on a LAN
// an attacker has the whole subnet to rotate through, and behind the ingress
// every request shares one source. A shared bucket means a determined guesser
// can lock the operator out for the window, which is the trade taken on purpose:
// this secret protects a remote shell, and a brief self-inflicted lockout is
// cheaper than an unbounded guessing rate. The window is short enough that the
// operator's own retry succeeds within a minute.
const (
	loginRateBurst  = 10
	loginRateWindow = time.Minute
)

// loginPathFor returns where an unauthenticated caller should be sent, carrying
// the path it was trying to reach so login can return it there.
//
// Only the path+query of the CURRENT request is preserved, and it is re-emitted
// as a relative URL — never a caller-supplied absolute one. That is what stops
// this becoming an open redirect: see safeReturnTo.
func loginPathFor(r *http.Request) string {
	want := r.URL.RequestURI()
	if want == "" || want == "/" || strings.HasPrefix(want, loginPath) {
		return loginPath
	}
	return loginPath + "?next=" + url.QueryEscape(want)
}

// safeReturnTo sanitises the ?next= parameter.
//
// 🔴 AN OPEN REDIRECT IS THE STANDARD BUG IN EXACTLY THIS PARAMETER, so the rule
// is allow-list shaped rather than deny-list shaped: the value must begin with a
// single "/" and must not begin with "//" (which a browser reads as a
// scheme-relative URL to ANOTHER HOST — the case a naive `strings.HasPrefix(v,
// "/")` check lets straight through). Anything else falls back to "/".
func safeReturnTo(v string) string {
	if !localPath(v) {
		return "/"
	}
	u, err := url.Parse(v)
	// The DECODED path must pass the same test: "/%2F%2Fevil.example" and
	// "/%5Cevil" decode to a second slash or a backslash.
	if err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" || u.User != nil || !localPath(u.Path) {
		return "/"
	}
	return v
}

// localPath reports whether p is a path on THIS origin: one leading "/", no
// "//" or "/\" start, and no byte a browser strips or rewrites before
// resolving — every control byte and space (≤ 0x20), DEL (0x7f) and a
// backslash anywhere.
//
// 🔴 THE CONTROL BYTES ARE THE CASE THE FIRST VERSION MISSED. It rejected
// CR/LF and "//" but not a TAB: "/\t/evil.example/x" passed every check,
// and browsers DELETE tab and newline from a URL before parsing it, leaving
// "//evil.example/x" — another host (TestSafeReturnToRefusesEveryOffOriginShape).
func localPath(p string) bool {
	if p == "" || p[0] != '/' {
		return false
	}
	for i := 0; i < len(p); i++ {
		if c := p[i]; c <= 0x20 || c == 0x7f || c == '\\' {
			return false
		}
	}
	return !strings.HasPrefix(p, "//")
}

// registerLoginRoutes wires the human login tier. These routes are OPEN by
// necessity — they are how a caller stops being anonymous — and they are the
// only browser-facing routes that are.
// 🔴 THE PATTERNS ARE WRITTEN AS LITERALS, NOT AS "GET "+loginPath, AND THAT IS
// LOAD-BEARING. Four ledgers in this package reason about routes by reading the
// SOURCE (terminal_write_ledger_test.go, and the human-tier ledger in
// browser_auth_test.go), and a computed pattern is invisible to every one of
// them — a `send-keys` route added that way would reach a real host with no
// ledger and nothing red. TestTheLoginPathConstantMatchesItsRoutes pins the
// literal against loginPath, so the duplication cannot drift.
func (s *Server) registerLoginRoutes(mux Mux) {
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.handleLoginSubmit)
	mux.HandleFunc("POST /logout", s.handleLogout)
	s.registerAccountRoutes(mux)
}

// handleLoginPage renders the form. An already-signed-in caller is sent on
// rather than shown a second login.
func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.auth.goTrueMode() {
		s.handleGoTrueLoginPage(w, r)
		return
	}
	if reason := s.auth.BrowserAuthRefusal(); reason != "" {
		// 🔴 THE REASON IS SHOWN, and it is safe to show: it names an env var the
		// operator controls and discloses no secret. Without it a fail-closed
		// deploy presents as a login form that rejects the correct password, and
		// the operator debugs the password instead of the deployment.
		s.renderLoginPage(w, http.StatusServiceUnavailable, "",
			"This server has no operator password configured ("+reason+"), so it cannot sign anyone in. "+
				"Set MUSTER_UI_PASSWORD in the muster secret and redeploy.")
		return
	}
	if s.hasValidSession(r) {
		http.Redirect(w, r, safeReturnTo(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	s.renderLoginPage(w, http.StatusOK, r.URL.Query().Get("next"), "")
}

// handleLoginSubmit checks the password and mints the session cookie.
func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if s.auth.goTrueMode() {
		s.handleGoTrueLoginSubmit(w, r)
		return
	}
	if reason := s.auth.BrowserAuthRefusal(); reason != "" {
		s.renderLoginPage(w, http.StatusServiceUnavailable, "",
			"This server has no operator password configured ("+reason+").")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderLoginPage(w, http.StatusBadRequest, "", "That form could not be read.")
		return
	}
	next := r.FormValue("next")
	if !s.loginRate.allow(time.Now(), loginRateBurst, loginRateWindow) {
		// 429, not 401: the credential was never judged, and saying otherwise
		// would teach a guesser that this password was wrong.
		s.renderLoginPage(w, http.StatusTooManyRequests, next,
			"Too many attempts. Wait a minute and try again.")
		return
	}
	if !auth.ConstantTimeEqual(r.FormValue("password"), s.auth.UIPassword) {
		s.logger.Printf("login: rejected a password attempt")
		s.renderLoginPage(w, http.StatusUnauthorized, next, "That password is not right.")
		return
	}

	s.setPasswordSessionCookie(w)
	s.logger.Printf("login: a browser session was issued")
	http.Redirect(w, r, safeReturnTo(next), http.StatusSeeOther)
}

// setPasswordSessionCookie mints a password-mode session issued NOW — the
// sign-in and the step-up confirm (stepup.go) are its two callers, and the
// issue time is what the step-up gate reads.
func (s *Server) setPasswordSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:  sessionCookieName,
		Value: auth.SignSession(s.auth.uiSessionSecret(), time.Now(), sessionTTL),
		Path:  "/",
		// HttpOnly: the cookie is never read by script, so keep it away from any
		// XSS that reaches this origin.
		HttpOnly: true,
		// Lax, not Strict: Strict withholds the cookie on a cross-site TOP-LEVEL
		// navigation, so following a link to muster from anywhere else would
		// land on the login page despite a valid session. Lax still withholds it
		// from cross-site POSTs, which is the CSRF case that matters here.
		SameSite: http.SameSiteLaxMode,
		Secure:   s.auth.SecureCookies,
		MaxAge:   int(sessionTTL / time.Second),
	})
}

// handleLogout clears the session cookie. It is a POST because a GET logout is
// triggerable by any third-party page embedding an image.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.auth.goTrueMode() {
		s.handleGoTrueLogout(w, r)
		return
	}
	// This browser's push subscription goes with its session (push_routes.go).
	if s.hasValidSession(r) {
		s.forgetPushDevice(w, r, s.sessionUserID(r))
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.auth.SecureCookies,
		MaxAge:   -1,
	})
	http.Redirect(w, r, loginPath, http.StatusSeeOther)
}

// renderLoginPage writes the whole document. Hand-written rather than composed,
// for the independence reason at the top of this file.
func (s *Server) renderLoginPage(w http.ResponseWriter, code int, next, problem string) {
	inner := `<form method="POST" action="` + loginPath + `" class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line">
<h1 class="text-balance text-base font-semibold text-fg">Sign in to muster</h1>
<p class="text-sm text-fg2">Enter the operator password to continue.</p>
` + problemNode(problem) + `
<label for="muster-password" class="text-sm font-medium text-fg">Operator password</label>
<input id="muster-password" name="password" type="password" autocomplete="current-password" autofocus required
 class="min-h-[44px] rounded-md bg-bg px-3 py-2 text-base text-fg ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus">
<input type="hidden" name="next" value="` + html.EscapeString(safeReturnTo(next)) + `">
<button type="submit" class="press min-h-[44px] rounded-md bg-accent px-3 py-2 text-base font-semibold text-on-accent transition hover:bg-accent/90">Sign in</button>
</form>`
	writeAuthDocument(w, code, "Sign in", inner)
}

// problemNode renders a server-authored failure message, or nothing.
func problemNode(problem string) string {
	if problem == "" {
		return ""
	}
	// role=alert so a screen reader announces the failure without the operator
	// hunting for it; the text is server-authored, and escaped regardless.
	return `<p role="alert" class="rounded-lg bg-st-error-bg px-3 py-2 text-sm text-st-error-fg ring-1 ring-inset ring-st-error-fg/40">` +
		html.EscapeString(problem) + `</p>`
}

// writeAuthDocument writes one of the standalone auth pages (sign-in, the
// gotrue sign-in, the admin claim, the account page, notices): the same bare
// document, so none of them depends on internal/ui's shell. title is escaped;
// inner is trusted markup built by this package.
func writeAuthDocument(w http.ResponseWriter, code int, title, inner string) {
	doc := `<!doctype html>
<html class="dark" lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<meta name="color-scheme" content="dark light">
` + ui.ThemeHeadHTML() + `
<meta name="robots" content="noindex">
<title>` + html.EscapeString(title) + ` · muster</title>
<link rel="stylesheet" href="` + html.EscapeString(web.Asset("/static/app.css")) + `">
</head>
<body class="flex min-h-dvh items-center justify-center bg-bg px-4 text-fg antialiased">
<main class="w-full max-w-sm">
` + inner + `
</main>
</body>
</html>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// A login page must never be cached: a shared or restored cache entry would
	// show a stale form (or a stale error) to the next person at this origin.
	w.Header().Set("Cache-Control", "no-store")
	// 🔴 same-origin, OVERRIDING securityHeaders' no-referrer, FOR THESE PAGES
	// ONLY. Under no-referrer a browser sends `Origin: null` on a form POST
	// navigation, so the gotrue sign-in, claim and sign-out forms — which
	// refuse a POST whose Origin is not muster's (login CSRF) — refused
	// muster's OWN forms. Measured in Chromium by the e2e spec: the claim POST
	// carried `Origin: null` and got 403. same-origin still sends nothing to
	// any other site, which is what no-referrer protects (a token in a URL).
	w.Header().Set("Referrer-Policy", "same-origin")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(doc))
}
