package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/auth"
	"github.com/ZacxDev/muster/internal/github"
	"github.com/ZacxDev/muster/internal/ui"
)

const oauthStateCookie = "muster_gh_state"

// githubUnwiredReason is what the two OAuth-flow routes answer when this build
// cannot complete a connection. It is stated ONCE so the two cannot drift into
// describing different deployments.
//
// ⚠ IT COVERS BOTH HALVES OF "CANNOT CONNECT" ON PURPOSE — no store to save the
// token into, and no OAuth App to obtain one with. Splitting it would mean
// telling a caller which of the two is missing at a route they reached by
// following a link the UI only draws when both are present, which is a
// distinction for a log line rather than for an error page.
const githubUnwiredReason = "this build cannot connect a GitHub account: it has no " +
	"GitHub connection store, no OAuth App, or neither"

// registerGitHubRoutes wires the Repos tab + GitHub OAuth flow when a GitHub
// connection store is set.
// 🔴 NO EARLY RETURN ON A nil DEPENDENCY — see the package doc in server.go.
// Every handler below answers its own "nothing to serve from" case, so the
// recorded route set is a function of the CODE and never of the fixture.
//
// 🔴 THAT SENTENCE WAS A DESCRIPTION OF AN INTENTION, NOT OF THE CODE, FOR THE
// WHOLE LIFE OF THIS FILE — AND IT IS THE REASON NOBODY LOOKED. Three of the
// four handlers reached straight through a nil Extensions.GitHub, so on the only
// deployment shape this module ships by default (no encryption key, therefore no
// store) GET /ui/repos answered 500 with an empty body on every single page load,
// not just on the Repos tab. Each handler answers its own case NOW.
// TestEveryGitHubRouteAnswersWhenTheStoreIsUnwired drives all four through the
// real mux, so the claim is checked rather than asserted.
func (s *Server) registerGitHubRoutes(mux Mux) {
	mux.HandleFunc("GET /ui/repos", s.requireSession(s.handleReposContent))
	mux.HandleFunc("GET /github/connect", s.requireSession(s.handleGitHubConnect))
	mux.HandleFunc("GET /github/callback", s.requireSession(s.handleGitHubCallback))
	mux.HandleFunc("POST /github/disconnect", s.requireSession(s.handleGitHubDisconnect))
}

// handleReposContent serves the /ui/repos partial: not-configured / connect /
// connected-with-repo-list depending on state.
func (s *Server) handleReposContent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 🔴 THE nil STORE IS ANSWERED HERE, AND THE REGISTRATION COMMENT ABOVE IS
	// WHY IT HAS TO BE. Routes in this package are registered unconditionally, so
	// this handler runs on every deployment — including the ones that build no
	// GitHub store at all, which is every deployment without an encryption key.
	// Without this branch the next line dereferences a nil interface: the
	// recovered panic answers 500 with a ZERO-LENGTH body, and because the shell
	// mounts the Repos panel on every page, every navigation in the app raises a
	// request-failed toast. The panel is the loudest symptom and it is not the
	// only one.
	//
	// ⚠ THIS IS THE CLAIM cmd/muster-server/doc_seams.go ENTRY 5 ALREADY MAKES —
	// "unset means the store is not built, which the Repos tab says on screen" —
	// and until this branch existed the tab said nothing on screen, because the
	// handler never reached a renderer. A doc that describes a guard is not a
	// guard.
	if s.ext.GitHub == nil {
		_ = ui.ReposUnavailable().Render(w)
		return
	}
	// Check for a stored connection FIRST — it may come from the static-token
	// path (MUSTER_GITHUB_TOKEN auto-connect) even when no OAuth App is set.
	conn, ok, err := s.ext.GitHub.Get(r.Context())
	if err != nil {
		s.logger.Printf("github: get connection: %v", err)
		http.Error(w, "could not load GitHub connection", http.StatusInternalServerError)
		return
	}
	if !ok {
		// Not connected: offer the OAuth flow if it's configured, otherwise
		// explain how to connect.
		if s.ext.GitHubOAuth.Configured() {
			_ = ui.ReposConnect().Render(w)
		} else {
			_ = ui.ReposNotConfigured().Render(w)
		}
		return
	}
	repos, err := github.ListRepos(r.Context(), conn.Token)
	if err != nil {
		s.logger.Printf("github: list repos: %v", err)
		_ = ui.ReposConnected(conn.Login, nil).Render(w)
		return
	}
	views := make([]ui.RepoView, 0, len(repos))
	for _, rp := range repos {
		views = append(views, ui.RepoView{FullName: rp.FullName, Private: rp.Private, HTMLURL: rp.HTMLURL})
	}
	_ = ui.ReposConnected(conn.Login, views).Render(w)
}

// handleGitHubConnect starts the OAuth web flow: set a state cookie and redirect
// to GitHub's authorize page.
func (s *Server) handleGitHubConnect(w http.ResponseWriter, r *http.Request) {
	if s.ext.GitHub == nil || !s.ext.GitHubOAuth.Configured() {
		http.Error(w, githubUnwiredReason, http.StatusServiceUnavailable)
		return
	}
	nonce, err := auth.RandomToken(16)
	if err != nil {
		http.Error(w, "could not start oauth", http.StatusInternalServerError)
		return
	}
	state, ok := s.githubOAuthState(requestOwner(r.Context()), nonce)
	if !ok {
		http.Error(w, githubUnwiredReason, http.StatusServiceUnavailable)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     oauthStateCookie,
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.auth.SecureCookies,
		SameSite: http.SameSiteLaxMode,
		Expires:  s.now().Add(10 * time.Minute),
	})
	http.Redirect(w, r, github.AuthorizeURL(s.ext.GitHubOAuth.ClientID, s.callbackURL(r), state), http.StatusFound)
}

// handleGitHubCallback completes the flow: verify state, exchange the code,
// fetch the login, and persist the (encrypted) token.
func (s *Server) handleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	if s.ext.GitHub == nil || !s.ext.GitHubOAuth.Configured() {
		http.Error(w, githubUnwiredReason, http.StatusServiceUnavailable)
		return
	}
	wantState, err := r.Cookie(oauthStateCookie)
	if err != nil || wantState.Value == "" || r.URL.Query().Get("state") != wantState.Value ||
		!s.githubOAuthStateNames(wantState.Value, requestOwner(r.Context())) {
		http.Error(w, "invalid oauth state", http.StatusBadRequest)
		return
	}
	// Clear the state cookie.
	http.SetCookie(w, &http.Cookie{Name: oauthStateCookie, Value: "", Path: "/", MaxAge: -1})

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}
	token, scopes, err := github.ExchangeCode(r.Context(), s.ext.GitHubOAuth.ClientID, s.ext.GitHubOAuth.ClientSecret, code, s.callbackURL(r))
	if err != nil {
		s.logger.Printf("github: exchange code: %v", err)
		http.Error(w, "github authorization failed", http.StatusBadGateway)
		return
	}
	login, err := github.FetchLogin(r.Context(), token)
	if err != nil {
		s.logger.Printf("github: fetch login: %v", err)
		http.Error(w, "could not read github account", http.StatusBadGateway)
		return
	}
	if err := s.ext.GitHub.Save(r.Context(), login, scopes, token); err != nil {
		s.logger.Printf("github: save connection: %v", err)
		http.Error(w, "could not save github connection", http.StatusInternalServerError)
		return
	}
	s.logger.Printf("github: connected account %q (scopes=%q)", login, scopes)
	// Back to the app, Repos tab.
	http.Redirect(w, r, "/repos", http.StatusFound)
}

// handleGitHubDisconnect clears the stored connection and re-renders the panel.
func (s *Server) handleGitHubDisconnect(w http.ResponseWriter, r *http.Request) {
	// This one answers INTO the panel (htmx swaps the response into
	// #panel-repos), so the refusal is the panel's own unavailable state rather
	// than an error body — a 503 here would swap nothing and leave the old
	// markup claiming a connection that cannot be cleared.
	if s.ext.GitHub == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = ui.ReposUnavailable().Render(w)
		return
	}
	if err := s.ext.GitHub.Clear(r.Context()); err != nil {
		s.logger.Printf("github: clear connection: %v", err)
		http.Error(w, "could not disconnect", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = ui.ReposConnect().Render(w)
}

// githubOAuthState is the OAuth `state` for user uid: "<uid>.<nonce>.<mac>",
// the mac an HMAC-SHA256 over "<uid>.<nonce>" under a key derived from the
// server's session secret.
//
// 🔴 IT BINDS THE FLOW TO THE USER WHO STARTED IT (accounts PR 3b). The
// callback saves the token as the CALLBACK's session user, so a state that did
// not name that user would let one user's browser complete a flow another
// started — attaching that user's GitHub account to the wrong muster user.
// The cookie equality check alone does not stop that: it proves the same
// browser, not the same user. The mac makes the uid unforgeable; the callback
// demands it equal its own principal (githubOAuthStateNames).
//
// 🔴 THE KEY IS GitHubOAuthConfig.StateKey AND AN EMPTY ONE MINTS NOTHING
// (false): a MAC under no key is a public function of the state.
func (s *Server) githubOAuthState(uid int64, nonce string) (string, bool) {
	mac, ok := s.githubStateMAC(strconv.FormatInt(uid, 10) + "." + nonce)
	if !ok || uid <= 0 {
		return "", false
	}
	return strconv.FormatInt(uid, 10) + "." + nonce + "." + mac, true
}

// githubOAuthStateNames reports whether state is a well-formed, authentic
// state naming uid (and uid is a user).
func (s *Server) githubOAuthStateNames(state string, uid int64) bool {
	if uid <= 0 {
		return false
	}
	i := strings.LastIndexByte(state, '.')
	if i < 0 {
		return false
	}
	body, mac := state[:i], state[i+1:]
	want, ok := s.githubStateMAC(body)
	if !ok || !hmac.Equal([]byte(mac), []byte(want)) {
		return false
	}
	owner, _, ok := strings.Cut(body, ".")
	return ok && owner == strconv.FormatInt(uid, 10)
}

func (s *Server) githubStateMAC(body string) (string, bool) {
	key := s.ext.GitHubOAuth.StateKey
	if len(key) == 0 {
		return "", false
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil)), true
}

// callbackURL builds the OAuth redirect URI. It prefers the configured public
// base URL; otherwise it derives the origin from the inbound request.
func (s *Server) callbackURL(r *http.Request) string {
	base := strings.TrimRight(s.ext.GitHubOAuth.BaseURL, "/")
	if base == "" {
		scheme := "https"
		if r.TLS == nil && !s.auth.SecureCookies {
			scheme = "http"
		}
		base = scheme + "://" + r.Host
	}
	return base + "/github/callback"
}
