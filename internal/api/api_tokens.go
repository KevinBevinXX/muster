package api

import (
	"context"
	"errors"
	"html"
	"net/http"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/apitokens"
	"github.com/ZacxDev/muster/internal/authz"
)

// PERSONAL API TOKENS (accounts PR 6, plan §5).
//
// A user mints `mst_…` tokens at /account/tokens, and ONLY there: minting needs
// a signed-in browser session. 🔴 A TOKEN CANNOT MINT A TOKEN — a leaked token
// that could would mint a non-expiring child that outlives revoking the parent
// (TestATokenCannotMintAToken). A token may list and revoke its owner's tokens
// (/api/tokens). A token authenticates
// the MACHINE tier — requireHookToken, requireArmedHookToken and
// requireHookOrServiceToken all accept one — and the request then acts as the
// token's OWNER, with the owner's role (roleOf), through authz.ViaToken. That
// is the difference from the shared hook secret, which acts as the machine user
// (user 1, the admin) whoever holds it. Tokens are accepted on EVERY listener
// that serves the machine routes — the LAN machine port AND the browser
// (edge) listener, which faces the internet once the edge gate is bypassed —
// so a token is an internet-usable credential, not a LAN-only one.
//
// 🔴 ONE DOOR, admitAPIToken, FOR EVERY WRAPPER THAT TAKES A TOKEN. The
// refusals (malformed, unknown, revoked, disabled owner) and the principal
// are decided in one function, so a wrapper cannot admit a
// token on a weaker rule than its siblings. It is an admit-ledger entry
// (internal/authz/ledger_test.go).
//
// 🔴 NOTHING IS CACHED POSITIVELY. Every request re-reads the token row and its
// owner's disabled_at (apitokens.PGStore.Authenticate), so revoking a token or
// disabling its owner takes effect on the very next request — tighter than the
// gotrue session tier's DisableSweepInterval, which bounds how long an already
// OPEN stream survives a disable. No route a token can open streams.

// apiTokensPath is the token-management API (list, revoke; requireAPIToken).
const apiTokensPath = "/api/tokens"

// accountTokensPath is the token page on the browser session tier.
const accountTokensPath = "/account/tokens"

func (s *Server) registerAPITokenRoutes(mux Mux) {
	// Who am I: any machine credential. A personal token answers with its owner
	// and the token's own id and name; the hook/service tiers with the machine
	// user.
	mux.HandleFunc("GET /api/whoami", s.requireHookOrServiceToken(s.handleWhoAmI))
	// Token management over the API: a PERSONAL token only, and list/revoke
	// only — NO create (see the file header).
	mux.HandleFunc("GET "+apiTokensPath, s.requireAPIToken(s.handleAPITokenList))
	mux.HandleFunc("DELETE "+apiTokensPath+"/{id}", s.requireAPIToken(s.handleAPITokenRevoke))
	// The browser page. requireSession serves it only on the browser listener
	// in gotrue mode; the writes are requireOperatorWrite (same-origin).
	mux.HandleFunc("GET "+accountTokensPath, s.requireSession(s.handleAccountTokens))
	mux.HandleFunc("POST "+accountTokensPath, s.requireOperatorWrite(s.handleAccountTokenCreate))
	mux.HandleFunc("POST "+accountTokensPath+"/{id}/revoke", s.requireOperatorWrite(s.handleAccountTokenRevoke))
}

// --- the door -------------------------------------------------------------------

// apiTokenCtxKey carries the personal token a request was admitted with.
type apiTokenCtxKey struct{}

// apiTokenFromContext returns the token this request was admitted with, if it
// was admitted with one.
func apiTokenFromContext(ctx context.Context) (apitokens.Token, bool) {
	t, ok := ctx.Value(apiTokenCtxKey{}).(apitokens.Token)
	return t, ok
}

// personalTokenFor returns the presented credential when it is a personal
// token, and false when it is anything else (so the wrapper carries on with
// its own credentials).
//
// ⚠ AN `mst_` VALUE IS A PERSONAL TOKEN OR NOTHING. The shared secrets are
// kept out of that namespace at BOOT (AuthConfig.SharedSecretPrefixRefusal):
// cmd/muster-server refuses an `mst_`-prefixed MUSTER_HOOK_TOKEN or
// MUSTER_SERVICE_TOKEN, which this door would otherwise answer 401.
func (s *Server) personalTokenFor(r *http.Request) (string, bool) {
	tok := hookTokenFromRequest(r)
	return tok, apitokens.LooksLike(tok)
}

// SharedSecretPrefixRefusal reports why the shared machine secrets collide
// with the personal-token namespace, or "".
func (c AuthConfig) SharedSecretPrefixRefusal() string {
	for _, v := range []struct{ name, val string }{{"MUSTER_HOOK_TOKEN", c.HookToken}, {"MUSTER_SERVICE_TOKEN", c.ServiceToken}} {
		if apitokens.LooksLike(v.val) {
			return v.name + " begins with " + apitokens.Prefix + ", the personal API token prefix: every request presenting it would be read as a personal token and refused. Generate a new random value."
		}
	}
	return ""
}

// requireAPIToken admits a PERSONAL token and nothing else. The token-
// management API is behind it: minting a credential is a person's act.
func (s *Server) requireAPIToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := s.personalTokenFor(r)
		if !ok {
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "this route takes a personal API token (" + apitokens.Prefix + "…), minted at " + accountTokensPath,
			})
			return
		}
		s.admitAPIToken(w, r, next, tok)
	}
}

// APITokenRefusal is the 401 body every refused personal token gets —
// malformed, unknown, revoked or a disabled owner alike, so a refusal says
// nothing about which.
const APITokenRefusal = "invalid or revoked API token"

// admitAPIToken authenticates a personal token and admits the request as its
// owner, or refuses it 401. See the file header.
//
// ⚠ NO FAILURE LIMITER, ON PURPOSE: a 256-bit token cannot be guessed, so a
// limiter would only turn 401 into 429. The one gate before the lookup is
// apitokens.ShapeOK — a stateless format check, so junk costs no DB read.
func (s *Server) admitAPIToken(w http.ResponseWriter, r *http.Request, next http.HandlerFunc, tok string) {
	if s.ext.APITokens == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "this server does not serve personal API tokens (no database)"})
		return
	}
	if !apitokens.ShapeOK(tok) {
		s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": APITokenRefusal})
		return
	}
	t, err := s.apiTokenUser(r.Context(), tok)
	switch {
	case err == nil:
	case errors.Is(err, apitokens.ErrBadToken), errors.Is(err, apitokens.ErrUserDisabled):
		s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": APITokenRefusal})
		return
	default:
		// The lookup FAILED; nothing was judged. 503 rather than 401 so a
		// caller does not throw away a good token.
		s.logger.Printf("api-token: lookup failed: %v", err)
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "could not check the API token"})
		return
	}
	ctx := context.WithValue(r.Context(), apiTokenCtxKey{}, t)
	s.admit(w, r.WithContext(ctx), next, s.principalFor(r.Context(), t.UserID, authz.ViaToken))
}

// apiTokenUser resolves a personal token to its record.
//
// 🔴 IT RUNS UNDER authz.System FOR THE REASON agentByToken DOES: it is how
// the request's user is LEARNED, so it cannot run as that user (api_tokens is
// under FORCE row-level security). The System context is used for this one
// store call and never handed on; the handler gets the request's own context
// with the principal built from the row's user_id. System-ledger entry.
func (s *Server) apiTokenUser(ctx context.Context, tok string) (apitokens.Token, error) {
	return s.ext.APITokens.Authenticate(authz.System(ctx), tok)
}

// --- the API ---------------------------------------------------------------------

func (s *Server) handleWhoAmI(w http.ResponseWriter, r *http.Request) {
	p := authz.MustFrom(r.Context())
	out := map[string]any{"userId": p.UserID, "role": p.Role, "via": p.Via}
	if t, ok := apiTokenFromContext(r.Context()); ok {
		out["token"] = map[string]any{"id": t.ID, "name": t.Name, "createdAt": t.CreatedAt}
	}
	if actor, ok := serviceActorFromContext(r.Context()); ok {
		out["actor"] = actor
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAPITokenList(w http.ResponseWriter, r *http.Request) {
	list, err := s.ext.APITokens.List(r.Context())
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleAPITokenRevoke(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "token not found"})
		return
	}
	found, wasLive, err := s.ext.APITokens.Revoke(r.Context(), id)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if !found {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "token not found"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"id": id, "revoked": wasLive})
}

// --- the browser page --------------------------------------------------------------

func (s *Server) handleAccountTokens(w http.ResponseWriter, r *http.Request) {
	s.renderAccountTokens(w, r, http.StatusOK, "", "")
}

func (s *Server) handleAccountTokenCreate(w http.ResponseWriter, r *http.Request) {
	if s.ext.APITokens == nil {
		s.renderAccountTokens(w, r, http.StatusServiceUnavailable, "", "")
		return
	}
	// The minting session's iat, for the store's cutoff check. 🔴 IN GOTRUE
	// MODE A MISSING ONE IS A WIRING BUG, NOT "NO CUTOFF": zero would skip the
	// check, so it is refused rather than passed on.
	iat, _ := r.Context().Value(sessionIssuedAtKey{}).(int64)
	if s.auth.goTrueMode() && iat <= 0 {
		s.logger.Printf("api-token: create: a gotrue session reached the mint with no issued-at")
		s.renderAccountTokens(w, r, http.StatusInternalServerError, "", "The token could not be created.")
		return
	}
	tok, _, err := s.ext.APITokens.Create(r.Context(), r.PostFormValue("name"), iat)
	if errors.Is(err, apitokens.ErrNameTooLong) {
		s.renderAccountTokens(w, r, http.StatusBadRequest, "", err.Error())
		return
	}
	if errors.Is(err, apitokens.ErrSessionEnded) {
		s.renderAccountTokens(w, r, http.StatusUnauthorized, "", "This sign-in has been ended (your password was reset, or the account was disabled and re-enabled). Sign in again to create API tokens.")
		return
	}
	if errors.Is(err, apitokens.ErrUserDisabled) {
		s.renderAccountTokens(w, r, http.StatusForbidden, "", "This account is disabled, so it cannot create API tokens.")
		return
	}
	if err != nil {
		s.logger.Printf("api-token: create: %v", err)
		s.renderAccountTokens(w, r, http.StatusInternalServerError, "", "The token could not be created.")
		return
	}
	// 200, NOT a redirect: the token is shown in THIS response and nowhere
	// else, and writeAuthDocument marks it no-store.
	s.renderAccountTokens(w, r, http.StatusOK, tok, "")
}

func (s *Server) handleAccountTokenRevoke(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	found := false
	if err == nil && id > 0 && s.ext.APITokens != nil {
		found, _, err = s.ext.APITokens.Revoke(r.Context(), id)
		if err != nil {
			s.logger.Printf("api-token: revoke %d: %v", id, err)
			s.renderAccountTokens(w, r, http.StatusInternalServerError, "", "The token could not be revoked.")
			return
		}
	}
	if !found {
		s.renderAccountTokens(w, r, http.StatusNotFound, "", "No such token.")
		return
	}
	http.Redirect(w, r, accountTokensPath, http.StatusSeeOther)
}

// renderAccountTokens writes the token page. minted, when set, is the token
// just created — the only time it is ever rendered.
func (s *Server) renderAccountTokens(w http.ResponseWriter, r *http.Request, code int, minted, problem string) {
	var list []apitokens.Token
	if s.ext.APITokens == nil {
		problem = "This server has no database, so it cannot hold API tokens."
	} else if l, err := s.ext.APITokens.List(r.Context()); err != nil {
		s.logger.Printf("api-token: list: %v", err)
		problem = "The token list could not be read."
	} else {
		list = l
	}
	var b strings.Builder
	b.WriteString(`<section class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line">
<h1 class="text-balance text-base font-semibold text-fg">API tokens</h1>
<p class="text-sm text-fg2">A token acts as you on muster's machine API and CLI (` + "<code>MUSTER_TOKEN</code>" + `). It is shown once.</p>
`)
	b.WriteString(problemNode(problem))
	if minted != "" {
		b.WriteString(`<div class="flex flex-col gap-2 rounded-md bg-bg px-3 py-3 ring-1 ring-inset ring-edge" data-testid="minted-token">
<p class="text-sm font-semibold text-fg">Copy it now — it will not be shown again.</p>
<input id="minted-token" readonly value="` + html.EscapeString(minted) + `" aria-label="New API token"
 class="min-h-[44px] w-full rounded-md bg-s1 px-3 py-2 font-mono text-sm text-fg ring-1 ring-inset ring-edge" onclick="this.select()">
</div>
`)
	}
	b.WriteString(`<form method="POST" action="` + accountTokensPath + `" class="flex flex-col gap-2">
<label for="token-name" class="text-sm font-medium text-fg">Name</label>
<input id="token-name" name="name" maxlength="` + strconv.Itoa(apitokens.MaxNameLen) + `" placeholder="laptop CLI"
 class="min-h-[44px] rounded-md bg-bg px-3 py-2 text-base text-fg ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus">
<button type="submit" class="press min-h-[44px] rounded-md bg-accent px-3 py-2 text-base font-semibold text-on-accent transition hover:bg-accent/90">Create token</button>
</form>
<ul class="flex flex-col gap-2" data-testid="token-list">
`)
	if len(list) == 0 {
		b.WriteString(`<li class="text-sm text-fg2">No tokens yet.</li>`)
	}
	for _, t := range list {
		name := t.Name
		if name == "" {
			name = "(unnamed)"
		}
		used := "never used"
		if t.LastUsedAt != nil {
			used = "last used " + t.LastUsedAt.UTC().Format("2006-01-02 15:04") + " UTC"
		}
		b.WriteString(`<li class="flex flex-col gap-1 rounded-md bg-bg px-3 py-2 ring-1 ring-inset ring-edge" data-token-id="` + strconv.FormatInt(t.ID, 10) + `">
<span class="break-all text-sm font-semibold text-fg">` + html.EscapeString(name) + `</span>
<span class="text-xs text-fg2">created ` + t.CreatedAt.UTC().Format("2006-01-02 15:04") + ` UTC · ` + used + `</span>
`)
		if t.Revoked() {
			b.WriteString(`<span class="text-xs font-semibold text-fg2">revoked</span>`)
		} else {
			b.WriteString(`<form method="POST" action="` + accountTokensPath + `/` + strconv.FormatInt(t.ID, 10) + `/revoke">
<button type="submit" class="press min-h-[44px] w-full rounded-md bg-bg px-3 py-2 text-sm font-semibold text-fg ring-1 ring-inset ring-edge">Revoke</button></form>`)
		}
		b.WriteString("</li>\n")
	}
	b.WriteString(`</ul>
<a href="` + accountPath + `" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Back to account</a>
</section>`)
	writeAuthDocument(w, code, "API tokens", b.String())
}
