package api

// --- passkeys: GoTrue v2.197.0's native WebAuthn, proxied by muster ----------
//
// The password stays mandatory; a passkey is a second way IN, added on top.
//
//   - Sign-in (usernameless): POST /auth/passkey/options → the browser's
//     navigator.credentials.get → POST /auth/passkey/verify. A verified
//     passkey yields a gotrue.Session, admitted exactly like a password
//     sign-in (admitFreshSession: a muster user, not disabled) and stored in
//     the same two cookies — so the session cutoff, sign-out-everywhere, the
//     disable sweep and refresh all apply to a passkey session unchanged.
//   - Account: GET /account/passkeys lists them; options/verify add one;
//     /{id}/delete removes one. Adding requires a sign-in within
//     passkeyAddWindow (freshAuth), else the password is asked again
//     (/account/passkeys/reauth).
//   - Admin: /admin/users/{id}/passkeys/delete removes all of a user's.
//   - A password reset removes all of the user's (login_invite.go).
//
// 🔴 THE BROWSER NEVER TALKS TO GoTrue, here as everywhere: every route is on
// muster, every GoTrue call is server-side and carries X-Muster-Client-IP.
// Every route is BROWSER-LISTENER ONLY and every POST must be same-origin.
//
// 🔴 USER VERIFICATION IS muster's CHECK (gotrue/webauthn.go): GoTrue accepts a
// credential without UV; muster refuses one before forwarding it.

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/gotrue"
	"github.com/ZacxDev/muster/internal/users"
)

const (
	passkeySignInOptionsPath = "/auth/passkey/options"
	passkeySignInVerifyPath  = "/auth/passkey/verify"
	accountPasskeysPath      = "/account/passkeys"

	// passkeyAddWindow: adding a passkey needs a sign-in (password or
	// passkey) this recent; otherwise the password is asked again.
	passkeyAddWindow = 5 * time.Minute

	// passkeyMetaKey is the GoTrue app_metadata key under which muster records
	// each passkey's backup flags at registration: GoTrue stores them but its
	// API does not return them, and app_metadata is admin-writable only.
	passkeyMetaKey = "muster_passkeys"

	// passkeyBodyLimit bounds a passkey request body. A WebAuthn credential
	// is a few KB; an attestation with a certificate chain stays well under.
	passkeyBodyLimit = 64 << 10

	// passkeySettingsTTL is how long GoTrue's passkeys_enabled is believed.
	passkeySettingsTTL = 30 * time.Second
)

// The refusals a passkey route answers with. Each is shown to the person.
const (
	PasskeysOffRefusal     = "Passkeys are not enabled on this server."
	PasskeyNoUVRefusal     = "That passkey did not verify you (no PIN, fingerprint, face or device unlock). Use a passkey that asks for one."
	PasskeyRejectedRefusal = "That passkey was not accepted."
	PasskeyUnreadable      = "That passkey response could not be read."
	PasskeyReauthRequired  = "reauth_required"
)

// passkeyGate caches GoTrue's passkeys_enabled: GoTrue is the one source of
// truth (GOTRUE_PASSKEY_ENABLED), so muster needs no setting of its own that
// could disagree with it.
type passkeyGate struct {
	mu sync.Mutex
	at time.Time
	on bool
}

// passkeysOn reports whether GoTrue has passkeys enabled, read through a
// passkeySettingsTTL cache. An unreadable /settings is "off" and not cached.
func (g *gotrueState) passkeysOn(ctx context.Context) bool {
	now := g.now()
	g.pk.mu.Lock()
	if !g.pk.at.IsZero() && now.Sub(g.pk.at) < passkeySettingsTTL {
		on := g.pk.on
		g.pk.mu.Unlock()
		return on
	}
	g.pk.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	st, err := g.Client.Settings(cctx, "muster-server")
	if err != nil {
		if g.logger != nil {
			g.logger.Printf("passkeys: could not read GoTrue /settings (treating passkeys as off): %v", err)
		}
		return false
	}
	g.pk.mu.Lock()
	g.pk.at, g.pk.on = now, st.PasskeysEnabled
	g.pk.mu.Unlock()
	return st.PasskeysEnabled
}

// passkeysServed reports whether this server offers passkeys at all: gotrue
// mode, GoTrue wired, and GoTrue saying passkeys are on.
func (s *Server) passkeysServed(ctx context.Context) bool {
	return s.auth.goTrueMode() && s.gt != nil && s.gt.passkeysOn(ctx)
}

func (s *Server) registerPasskeyRoutes(mux Mux) {
	mux.HandleFunc("POST /auth/passkey/options", s.handlePasskeySignInOptions)
	mux.HandleFunc("POST /auth/passkey/verify", s.handlePasskeySignInVerify)
	mux.HandleFunc("GET /account/passkeys", s.requireSession(s.handleAccountPasskeys))
	mux.HandleFunc("POST /account/passkeys/options", s.requireSession(s.handlePasskeyAddOptions))
	mux.HandleFunc("POST /account/passkeys/verify", s.requireSession(s.handlePasskeyAddVerify))
	mux.HandleFunc("POST /account/passkeys/reauth", s.requireSession(s.handlePasskeyReauth))
	mux.HandleFunc("POST /account/passkeys/{id}/delete", s.requireSession(s.handlePasskeyDelete))
}

// readPasskeyBody decodes a bounded JSON body into v.
func readPasskeyBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, passkeyBodyLimit)
	return json.NewDecoder(r.Body).Decode(v) == nil
}

// --- sign-in ------------------------------------------------------------------

// passkeySignInRefusal answers (and returns true) when this request may not use
// the passkey sign-in: the machine listener, a non-gotrue server, a
// cross-origin POST, or passkeys off in GoTrue.
func (s *Server) passkeySignInRefusal(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case !viaBrowserListener(r):
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": OffBrowserListenerRefusal})
	case !s.isSameOrigin(r):
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": CrossOriginRefusal})
	case !s.passkeysServed(r.Context()):
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": PasskeysOffRefusal})
	default:
		return false
	}
	return true
}

// writeGoTrueRefusal maps a GoTrue error on a passkey call to an answer.
func (s *Server) writeGoTrueRefusal(w http.ResponseWriter, what string, err error, clientMsg string) {
	var ae *gotrue.APIError
	switch {
	case errors.As(err, &ae) && ae.Status == http.StatusTooManyRequests:
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "Too many attempts. Wait a few minutes and try again."})
	case gotrue.IsClientError(err):
		s.logger.Printf("passkeys: GoTrue refused %s: %v", what, err)
		s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": clientMsg})
	default:
		s.logger.Printf("passkeys: GoTrue %s failed: %v", what, err)
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"error": "Sign-in is unavailable right now. Try again shortly."})
	}
}

func (s *Server) handlePasskeySignInOptions(w http.ResponseWriter, r *http.Request) {
	if s.passkeySignInRefusal(w, r) {
		return
	}
	o, err := s.gt.Client.PasskeySignInOptions(r.Context(), clientIP(r))
	if err != nil {
		s.writeGoTrueRefusal(w, "sign-in options", err, PasskeysOffRefusal)
		return
	}
	opts, err := gotrue.RequireUserVerification(o.Options, false)
	if err != nil {
		s.writeGoTrueRefusal(w, "sign-in options", err, PasskeysOffRefusal)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"challenge_id": o.ChallengeID, "options": json.RawMessage(opts)})
}

func (s *Server) handlePasskeySignInVerify(w http.ResponseWriter, r *http.Request) {
	if s.passkeySignInRefusal(w, r) {
		return
	}
	var body struct {
		ChallengeID string          `json:"challenge_id"`
		Credential  json.RawMessage `json:"credential"`
		Next        string          `json:"next"`
	}
	if !readPasskeyBody(w, r, &body) || body.ChallengeID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": PasskeyUnreadable})
		return
	}
	ip := clientIP(r)
	// The ADDRESS side of the sign-in limiter: a passkey assertion names no
	// account muster can key on before GoTrue answers (the user is in the
	// signed userHandle), so it is throttled per client address, reserved
	// before GoTrue is asked like every sign-in (signinLimiter).
	ticket, ok := s.gt.signin.reserve(signinAccount{}, ip, s.gt.now())
	if !ok {
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "Too many attempts. Wait a few minutes and try again."})
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
	// 🔴 THE UV GUARD. Refused BEFORE GoTrue sees it (see gotrue/webauthn.go).
	if !flags.UserPresent || !flags.UserVerified {
		outcome = outcomeFailed
		s.logger.Printf("passkeys: refused a sign-in assertion without user verification from %s", ip)
		s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": PasskeyNoUVRefusal})
		return
	}
	sess, err := s.gt.Client.PasskeySignInVerify(r.Context(), ip, body.ChallengeID, canonical)
	if err != nil {
		if gotrue.IsClientError(err) && !isStatus(err, http.StatusTooManyRequests) {
			outcome = outcomeFailed
		}
		s.writeGoTrueRefusal(w, "a passkey sign-in", err, PasskeyRejectedRefusal)
		return
	}
	u, problem, code := s.admitFreshSession(r.Context(), sess)
	if problem != "" {
		if code != http.StatusInternalServerError {
			outcome = outcomeFailed
			// GoTrue issued a session muster will not honour (no muster
			// account): end it there too rather than leave it live.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
			_ = s.gt.Client.Logout(ctx, ip, sess.AccessToken, gotrue.ScopeLocal)
			cancel()
		}
		s.writeJSON(w, code, map[string]any{"error": problem})
		return
	}
	outcome = outcomeSucceeded
	s.setGoTrueCookies(w, sess)
	s.logger.Printf("login: user %d signed in with a passkey", u.ID)
	s.writeJSON(w, http.StatusOK, map[string]any{"redirect": safeReturnTo(body.Next)})
}

func isStatus(err error, code int) bool {
	var ae *gotrue.APIError
	return errors.As(err, &ae) && ae.Status == code
}

// --- the account side -----------------------------------------------------------

// passkeyAccountCreds returns the request's GoTrue session for an account
// passkey route, or answers and returns false: passkeys off, no GoTrue session
// on the request, or a cross-origin write.
func (s *Server) passkeyAccountCreds(w http.ResponseWriter, r *http.Request) (gotrueCreds, bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !s.isSameOrigin(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": CrossOriginRefusal})
		return gotrueCreds{}, false
	}
	creds, ok := gotrueCredsFrom(r.Context())
	if !ok || !s.passkeysServed(r.Context()) {
		if isDocumentNavigation(r) {
			s.renderNotice(w, http.StatusNotFound, "Passkeys are not available", PasskeysOffRefusal)
		} else {
			s.writeJSON(w, http.StatusNotFound, map[string]any{"error": PasskeysOffRefusal})
		}
		return gotrueCreds{}, false
	}
	return creds, true
}

// passkeyFlags reads the backup flags muster recorded for sub's passkeys.
// Best effort: an unreadable record shows no flag, it does not fail the page.
func (s *Server) passkeyFlags(ctx context.Context, sub string) map[string]map[string]any {
	md, err := s.gt.Client.AdminAppMetadata(ctx, "muster-server", sub)
	if err != nil {
		s.logger.Printf("passkeys: could not read the recorded passkey flags: %v", err)
		return nil
	}
	raw, _ := md[passkeyMetaKey].(map[string]any)
	out := map[string]map[string]any{}
	for id, v := range raw {
		if m, ok := v.(map[string]any); ok {
			out[id] = m
		}
	}
	return out
}

// setPasskeyFlag records (flags non-nil) or forgets (nil) one passkey's
// backup flags; with id "" it forgets them all. Best effort, logged.
func (s *Server) setPasskeyFlag(ctx context.Context, sub, id string, flags map[string]any) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if id == "" {
		if err := s.gt.Client.AdminSetAppMetadata(ctx, "muster-server", sub, passkeyMetaKey, nil); err != nil {
			s.logger.Printf("passkeys: could not clear the recorded passkey flags: %v", err)
		}
		return
	}
	all := s.passkeyFlags(ctx, sub)
	if all == nil {
		all = map[string]map[string]any{}
	}
	if flags == nil {
		delete(all, id)
	} else {
		all[id] = flags
	}
	if err := s.gt.Client.AdminSetAppMetadata(ctx, "muster-server", sub, passkeyMetaKey, all); err != nil {
		s.logger.Printf("passkeys: could not record the passkey flags: %v", err)
	}
}

func (s *Server) handleAccountPasskeys(w http.ResponseWriter, r *http.Request) {
	creds, ok := s.passkeyAccountCreds(w, r)
	if !ok {
		return
	}
	s.renderAccountPasskeys(w, r, http.StatusOK, creds, "")
}

func (s *Server) renderAccountPasskeys(w http.ResponseWriter, r *http.Request, code int, creds gotrueCreds, problem string) {
	ctx := r.Context()
	list, err := s.gt.Client.Passkeys(ctx, clientIP(r), creds.access)
	if err != nil {
		s.logger.Printf("passkeys: list: %v", err)
		s.renderNotice(w, http.StatusBadGateway, "Passkeys could not be loaded", "Try again in a moment.")
		return
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	flags := s.passkeyFlags(ctx, creds.claims.Subject)
	var b strings.Builder
	if r.URL.Query().Get("welcome") == "1" {
		b.WriteString(`<p class="text-sm font-semibold text-fg" role="status" id="passkey-welcome">Your account is ready. Add a passkey on this device to sign in next time without typing your password.</p>`)
	}
	b.WriteString(problemNode(problem))
	b.WriteString(`<p class="text-sm text-fg2">A passkey signs you in with this device's fingerprint, face, PIN or screen lock. Your password keeps working.</p>`)
	b.WriteString(`<ul class="flex flex-col gap-3" id="passkey-list">`)
	if len(list) == 0 {
		b.WriteString(`<li class="text-sm text-fg2" id="passkey-none">No passkeys yet.</li>`)
	}
	for _, p := range list {
		name := p.FriendlyName
		if name == "" {
			name = "Passkey"
		}
		used := "never used"
		if p.LastUsedAt != nil {
			used = "last used " + fmtTime(*p.LastUsedAt)
		}
		syncNote := ""
		if f, ok := flags[p.ID]; ok {
			if bs, _ := f["synced"].(bool); bs {
				syncNote = `<span data-passkey-synced="true">synced</span> · `
			} else {
				syncNote = `<span data-passkey-synced="false">this device only</span> · `
			}
		}
		b.WriteString(`<li class="flex flex-col gap-2 rounded-lg border border-line px-3 py-3" data-passkey-id="` + html.EscapeString(p.ID) + `">
<span class="font-semibold text-fg" data-passkey-name>` + html.EscapeString(name) + `</span>
<span class="text-xs text-fg2">` + syncNote + `added ` + fmtTime(p.CreatedAt) + ` · ` + used + `</span>
<form method="POST" action="` + accountPasskeysPath + `/` + html.EscapeString(p.ID) + `/delete"><button type="submit" class="` + adminQuiet + `">Remove</button></form>
</li>`)
	}
	b.WriteString(`</ul>`)
	skip := ""
	if r.URL.Query().Get("welcome") == "1" {
		skip = `<a href="/" class="inline-flex min-h-[44px] items-center text-sm text-accent underline" id="passkey-skip">Skip for now</a>`
	}
	writeAuthDocument(w, code, "Passkeys", `<section class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line">
<h1 class="text-balance text-base font-semibold text-fg">Passkeys</h1>
`+b.String()+`
<p class="text-sm text-fg2" id="passkey-status" role="status" aria-live="polite"></p>
<button type="button" id="passkey-add" class="`+adminButton+`">Add a passkey on this device</button>
<form id="passkey-reauth" hidden class="flex flex-col gap-2">
<p class="text-sm text-fg2">To add a passkey, enter your password again.</p>
<label for="passkey-password" class="text-sm font-medium text-fg">Password</label>
<input id="passkey-password" type="password" autocomplete="current-password" class="`+adminInput+`">
<button type="submit" class="`+adminButton+`">Continue</button>
</form>
`+skip+`
<a href="/account" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Back to account</a>
</section>
<script>`+passkeyJS+passkeyAddJS+`</script>`)
}

// refuseStale answers 401 reauth_required when the session's last sign-in is
// older than passkeyAddWindow.
func (s *Server) refuseStale(w http.ResponseWriter, creds gotrueCreds) bool {
	if freshAuth(creds.claims, s.gt.now(), passkeyAddWindow) {
		return false
	}
	s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": PasskeyReauthRequired})
	return true
}

func (s *Server) handlePasskeyAddOptions(w http.ResponseWriter, r *http.Request) {
	creds, ok := s.passkeyAccountCreds(w, r)
	if !ok || s.refuseStale(w, creds) {
		return
	}
	o, err := s.gt.Client.PasskeyRegisterOptions(r.Context(), clientIP(r), creds.access)
	if err != nil {
		s.writeGoTrueRefusal(w, "registration options", err, "A passkey cannot be added right now.")
		return
	}
	opts, err := gotrue.RequireUserVerification(o.Options, true)
	if err != nil {
		s.writeGoTrueRefusal(w, "registration options", err, "A passkey cannot be added right now.")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"challenge_id": o.ChallengeID, "options": json.RawMessage(opts)})
}

func (s *Server) handlePasskeyAddVerify(w http.ResponseWriter, r *http.Request) {
	creds, ok := s.passkeyAccountCreds(w, r)
	if !ok || s.refuseStale(w, creds) {
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
	flags, canonical, err := gotrue.CanonicalAttestation(body.Credential)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": PasskeyUnreadable})
		return
	}
	if !flags.UserPresent || !flags.UserVerified {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": PasskeyNoUVRefusal})
		return
	}
	p, err := s.gt.Client.PasskeyRegisterVerify(r.Context(), clientIP(r), creds.access, body.ChallengeID, canonical)
	if err != nil {
		s.writeGoTrueRefusal(w, "a passkey registration", err, "That passkey could not be added.")
		return
	}
	s.setPasskeyFlag(r.Context(), creds.claims.Subject, p.ID, map[string]any{"synced": flags.BackedUp, "can_sync": flags.BackupEligible})
	uid := authz.MustFrom(r.Context()).UserID
	if s.gt.Accounts != nil {
		_ = s.gt.Accounts.Audit(r.Context(), users.Actor{UserID: uid, Via: authz.ViaSession}, "user.passkey_added",
			users.UserObject(uid), map[string]any{"passkey_id": p.ID, "ip": clientIP(r)})
	}
	s.logger.Printf("passkeys: user %d added a passkey", uid)
	s.writeJSON(w, http.StatusOK, map[string]any{"id": p.ID})
}

// handlePasskeyReauth re-asks the password of the signed-in user (never an
// email the client names) and, when it is right, replaces the session with the
// new one — whose amr is now — so adding a passkey passes freshAuth.
func (s *Server) handlePasskeyReauth(w http.ResponseWriter, r *http.Request) {
	creds, ok := s.passkeyAccountCreds(w, r)
	if !ok {
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if !readPasskeyBody(w, r, &body) || body.Password == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Enter your password."})
		return
	}
	uid := authz.MustFrom(r.Context()).UserID
	u, err := s.gt.Users.ByID(r.Context(), uid)
	if err != nil || u.Email == "" {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "Your account could not be read."})
		return
	}
	ip := clientIP(r)
	// The same per-ACCOUNT backoff a sign-in gets: this is a password guess.
	ticket, ok := s.gt.signin.reserve(signinAccount{known: true, uid: u.ID}, ip, s.gt.now())
	if !ok {
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "Too many attempts. Wait a few minutes and try again."})
		return
	}
	outcome := outcomeUnjudged
	defer func() { s.gt.signin.settle(ticket, outcome) }()
	sess, err := s.gt.Client.PasswordGrant(r.Context(), ip, u.Email, body.Password)
	if err != nil {
		if gotrue.IsClientError(err) && !isStatus(err, http.StatusTooManyRequests) {
			outcome = outcomeFailed
		}
		s.writeGoTrueRefusal(w, "a re-authentication", err, "That password is not right.")
		return
	}
	nu, problem, code := s.admitFreshSession(r.Context(), sess)
	if problem != "" || nu.ID != uid {
		if problem == "" {
			problem, code = "That password is not right.", http.StatusUnauthorized
		}
		s.writeJSON(w, code, map[string]any{"error": problem})
		return
	}
	outcome = outcomeSucceeded
	s.setGoTrueCookies(w, sess)
	// The session this request came in on is replaced; end its refresh side
	// in GoTrue so sessions do not pile up. Best effort.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if err := s.gt.Client.Logout(ctx, ip, creds.access, gotrue.ScopeLocal); err != nil {
		s.logger.Printf("passkeys: ending the replaced session failed (it expires on its own): %v", err)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handlePasskeyDelete(w http.ResponseWriter, r *http.Request) {
	creds, ok := s.passkeyAccountCreds(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if err := s.gt.Client.DeletePasskey(r.Context(), clientIP(r), creds.access, id); err != nil {
		if gotrue.IsClientError(err) {
			s.renderAccountPasskeys(w, r, http.StatusNotFound, creds, "That passkey is not one of yours.")
			return
		}
		s.logger.Printf("passkeys: delete: %v", err)
		s.renderAccountPasskeys(w, r, http.StatusBadGateway, creds, "That passkey could not be removed right now.")
		return
	}
	s.setPasskeyFlag(r.Context(), creds.claims.Subject, id, nil)
	uid := authz.MustFrom(r.Context()).UserID
	if s.gt.Accounts != nil {
		_ = s.gt.Accounts.Audit(r.Context(), users.Actor{UserID: uid, Via: authz.ViaSession}, "user.passkey_removed",
			users.UserObject(uid), map[string]any{"passkey_id": id})
	}
	http.Redirect(w, r, accountPasskeysPath, http.StatusSeeOther)
}

// --- admin ------------------------------------------------------------------------

// handleAdminUserPasskeysDelete removes every passkey a user has (their
// password, sessions and tokens are untouched — Disable or a reset link for
// those).
func (s *Server) handleAdminUserPasskeysDelete(w http.ResponseWriter, r *http.Request) {
	u, ok := s.adminUserTarget(w, r)
	if !ok {
		return
	}
	if u.GotrueSub == "" {
		s.renderAdminUsers(w, r, http.StatusConflict, "That user has no login.", "")
		return
	}
	n := 0
	err := s.gt.Accounts.Audited(r.Context(), actorOf(r), "user.passkeys_removed", users.UserObject(u.ID), nil, func() error {
		var err error
		n, err = s.gt.Client.AdminDeleteAllPasskeys(r.Context(), clientIP(r), u.GotrueSub)
		return err
	})
	if err != nil {
		s.logger.Printf("admin: remove passkeys of user %d (%d removed before the failure): %v", u.ID, n, err)
		s.renderAdminUsers(w, r, http.StatusBadGateway, "The passkeys could not all be removed. Try again.", "")
		return
	}
	s.setPasskeyFlag(r.Context(), u.GotrueSub, "", nil)
	s.logger.Printf("admin: removed %d passkeys of user %d", n, u.ID)
	who := u.Email
	if who == "" {
		who = u.DisplayName
	}
	s.renderAdminUsers(w, r, http.StatusOK, "", `<p class="text-sm font-semibold text-fg" role="status" id="admin-passkeys-removed">Removed `+
		plural(n, "passkey")+` of `+html.EscapeString(who)+`.</p>`)
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return strconv.Itoa(n) + " " + what + "s"
}
