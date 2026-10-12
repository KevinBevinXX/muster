package api

import (
	"context"
	"errors"
	"html"
	"net/http"
	"strings"

	"github.com/ZacxDev/muster/internal/credentials"
	"github.com/ZacxDev/muster/internal/ui"
)

// Account → Credentials: where a member brings their own Claude setup-tokens
// and OpenRouter API key (accounts plan, operator decisions). The store
// is internal/credentials; whose credentials an agent runs on is decided by
// credentials.Plane, which this page only asks "does this user run on the
// environment's?" (an admin does, and has nothing to add here).
//
// 🔴 NO SECRET IS EVER WRITTEN BACK. The list shows a name and a 48-bit
// fingerprint; the forms' secret inputs are never prefilled, not even after a
// refused submit; nothing here logs a form value; and nothing here writes to
// audit_log.

// CredentialStore is what the page needs (internal/credentials.PGStore, with
// the plane's role decision beside it).
type CredentialStore interface {
	List(ctx context.Context) ([]credentials.Summary, error)
	PutClaude(ctx context.Context, name, token string) error
	PutOpenRouter(ctx context.Context, key string) error
	Delete(ctx context.Context, kind, name string) (bool, error)
	// UsesEnvironment reports whether owner's agents run on the environment's
	// credentials (an admin): such a user has nothing to add here.
	UsesEnvironment(ctx context.Context, owner int64) bool
}

// credentialsFormLimit bounds a credentials POST. A setup-token is ~110 bytes.
const credentialsFormLimit = 16 << 10

func (s *Server) registerCredentialRoutes(mux Mux) {
	mux.HandleFunc("GET "+ui.CredentialsPath, s.requireSession(s.handleCredentialsPage))
	mux.HandleFunc("POST "+ui.CredentialsPath+"/claude", s.requireOperatorWrite(s.handleCredentialClaudePut))
	mux.HandleFunc("POST "+ui.CredentialsPath+"/claude/{name}/delete", s.requireOperatorWrite(s.handleCredentialClaudeDelete))
	mux.HandleFunc("POST "+ui.CredentialsPath+"/openrouter", s.requireOperatorWrite(s.handleCredentialOpenRouterPut))
	mux.HandleFunc("POST "+ui.CredentialsPath+"/openrouter/delete", s.requireOperatorWrite(s.handleCredentialOpenRouterDelete))
}

// credentialsGate answers the request itself (and returns false) when there is
// no store, or when the caller runs on the environment's credentials.
func (s *Server) credentialsGate(w http.ResponseWriter, r *http.Request) bool {
	if s.ext.Credentials == nil {
		s.renderCredentials(w, r, http.StatusServiceUnavailable, "", "")
		return false
	}
	if s.ext.Credentials.UsesEnvironment(r.Context(), callerID(r.Context())) {
		s.renderCredentials(w, r, http.StatusConflict, "", "")
		return false
	}
	return true
}

func (s *Server) handleCredentialsPage(w http.ResponseWriter, r *http.Request) {
	notice := ""
	switch r.URL.Query().Get("done") {
	case "claude":
		notice = "Claude account saved."
	case "openrouter":
		notice = "OpenRouter API key saved."
	case "deleted":
		notice = "Deleted."
	}
	s.renderCredentials(w, r, http.StatusOK, notice, "")
}

func (s *Server) readCredentialForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, credentialsFormLimit)
	if err := r.ParseForm(); err != nil {
		s.renderCredentials(w, r, http.StatusBadRequest, "", "That form could not be read.")
		return false
	}
	return true
}

// credentialProblem is the sentence a refused write shows. 🔴 It is never the
// raw error for anything but a shape refusal or a missing-credential sentence:
// those are written to be shown and carry no value.
func (s *Server) credentialProblem(err error) (int, string) {
	if errors.Is(err, credentials.ErrInvalid) {
		return http.StatusBadRequest, err.Error()
	}
	s.logger.Printf("credentials: user write failed: %v", err)
	return http.StatusInternalServerError, "That could not be saved; try again."
}

func (s *Server) handleCredentialClaudePut(w http.ResponseWriter, r *http.Request) {
	if !s.credentialsGate(w, r) || !s.readCredentialForm(w, r) {
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	token := strings.TrimSpace(r.PostFormValue("token"))
	if err := s.ext.Credentials.PutClaude(r.Context(), name, token); err != nil {
		code, msg := s.credentialProblem(err)
		s.renderCredentials(w, r, code, "", msg)
		return
	}
	http.Redirect(w, r, ui.CredentialsPath+"?done=claude", http.StatusSeeOther)
}

func (s *Server) handleCredentialOpenRouterPut(w http.ResponseWriter, r *http.Request) {
	if !s.credentialsGate(w, r) || !s.readCredentialForm(w, r) {
		return
	}
	if err := s.ext.Credentials.PutOpenRouter(r.Context(), strings.TrimSpace(r.PostFormValue("key"))); err != nil {
		code, msg := s.credentialProblem(err)
		s.renderCredentials(w, r, code, "", msg)
		return
	}
	http.Redirect(w, r, ui.CredentialsPath+"?done=openrouter", http.StatusSeeOther)
}

func (s *Server) handleCredentialClaudeDelete(w http.ResponseWriter, r *http.Request) {
	s.deleteCredential(w, r, credentials.KindClaude, r.PathValue("name"))
}

func (s *Server) handleCredentialOpenRouterDelete(w http.ResponseWriter, r *http.Request) {
	s.deleteCredential(w, r, credentials.KindOpenRouter, credentials.OpenRouterName)
}

// deleteCredential deletes the CALLER's credential (kind, name). Another
// user's credential of the same name is not theirs to name: the store deletes
// within the caller's own rows only, so the answer for it is the answer for a
// name that does not exist.
func (s *Server) deleteCredential(w http.ResponseWriter, r *http.Request, kind, name string) {
	if !s.credentialsGate(w, r) {
		return
	}
	ok, err := s.ext.Credentials.Delete(r.Context(), kind, name)
	if err != nil {
		code, msg := s.credentialProblem(err)
		s.renderCredentials(w, r, code, "", msg)
		return
	}
	if !ok {
		s.renderCredentials(w, r, http.StatusNotFound, "", "You have no such credential.")
		return
	}
	http.Redirect(w, r, ui.CredentialsPath+"?done=deleted", http.StatusSeeOther)
}

// --- the page -----------------------------------------------------------------

const (
	credInput  = `class="min-h-[44px] w-full rounded-md bg-bg px-3 py-2 text-base text-fg ring-1 ring-inset ring-edge focus:outline-none focus:ring-2 focus:ring-focus"`
	credButton = `class="press min-h-[44px] w-full rounded-md bg-accent px-3 py-2 text-base font-semibold text-on-accent transition hover:bg-accent/90"`
	credDelete = `class="press min-h-[44px] rounded-md bg-bg px-3 py-2 text-sm font-semibold text-fg ring-1 ring-inset ring-edge"`
	credCard   = `class="flex flex-col gap-3 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line"`
)

func (s *Server) renderCredentials(w http.ResponseWriter, r *http.Request, code int, notice, problem string) {
	var b strings.Builder
	b.WriteString(`<div class="flex flex-col gap-4" data-credentials-page>`)
	b.WriteString(`<section ` + credCard + `><h1 class="text-balance text-base font-semibold text-fg">Credentials</h1>`)
	if notice != "" {
		b.WriteString(`<p role="status" class="text-sm text-fg">` + html.EscapeString(notice) + `</p>`)
	}
	b.WriteString(problemNode(problem))
	switch {
	case s.ext.Credentials == nil:
		b.WriteString(`<p class="text-sm text-fg2">This server has no credential store configured, so credentials cannot be added here.</p></section>`)
	case s.ext.Credentials.UsesEnvironment(r.Context(), callerID(r.Context())):
		b.WriteString(`<p class="text-sm text-fg2" data-credentials-environment>Your agents use the credentials this server is configured with (its Claude accounts and OpenRouter key). There is nothing to add here.</p></section>`)
	default:
		b.WriteString(`<p class="text-sm text-fg2">Your agents run only on the credentials you add here. Each is stored encrypted and is never shown again.</p></section>`)
		s.writeCredentialSections(&b, r)
	}
	b.WriteString(`<a href="/account" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Back to account</a></div>`)
	writeAuthDocument(w, code, "Credentials", b.String())
}

func (s *Server) writeCredentialSections(b *strings.Builder, r *http.Request) {
	list, err := s.ext.Credentials.List(r.Context())
	if err != nil {
		s.logger.Printf("credentials: list: %v", err)
	}
	var claude []credentials.Summary
	var openrouter *credentials.Summary
	for i := range list {
		switch list[i].Kind {
		case credentials.KindClaude:
			claude = append(claude, list[i])
		case credentials.KindOpenRouter:
			openrouter = &list[i]
		}
	}

	b.WriteString(`<section ` + credCard + ` data-credentials-claude><h2 class="text-sm font-semibold text-fg">Claude Code accounts</h2>`)
	if len(claude) == 0 {
		b.WriteString(`<p class="text-sm text-fg2" data-credentials-empty>None yet. Without one, Claude Code agents are unavailable to you.</p>`)
	} else {
		b.WriteString(`<ul class="flex flex-col gap-2">`)
		for _, c := range claude {
			b.WriteString(`<li class="flex items-center justify-between gap-2" data-credential-account="` + html.EscapeString(c.Name) + `">` +
				`<span class="min-w-0 text-sm text-fg"><span class="font-semibold">` + html.EscapeString(c.Name) + `</span> ` +
				`<span class="font-mono text-xs text-muted">` + html.EscapeString(credentials.ClaudeTokenPrefix) + `… · ` + html.EscapeString(c.Fingerprint) + `</span></span>` +
				`<form method="POST" action="` + ui.CredentialsPath + `/claude/` + html.EscapeString(c.Name) + `/delete">` +
				`<button type="submit" ` + credDelete + ` aria-label="Delete ` + html.EscapeString(c.Name) + `">Delete</button></form></li>`)
		}
		b.WriteString(`</ul>`)
	}
	b.WriteString(`<form method="POST" action="` + ui.CredentialsPath + `/claude" class="flex flex-col gap-2">
<label for="cred-claude-name" class="text-sm font-medium text-fg">Account name</label>
<input id="cred-claude-name" name="name" required pattern="[a-z0-9]([a-z0-9\-]{0,30}[a-z0-9])?" maxlength="32" autocomplete="off" placeholder="personal" ` + credInput + `>
<label for="cred-claude-token" class="text-sm font-medium text-fg">Setup token</label>
<input id="cred-claude-token" name="token" type="password" required autocomplete="off" spellcheck="false" placeholder="` + credentials.ClaudeTokenPrefix + `…" ` + credInput + `>
<p class="text-xs text-muted">Run <code>claude setup-token</code> and paste what it prints. Saving an existing name replaces its token.</p>
<button type="submit" ` + credButton + `>Save Claude account</button></form></section>`)

	b.WriteString(`<section ` + credCard + ` data-credentials-openrouter><h2 class="text-sm font-semibold text-fg">OpenRouter</h2>`)
	if openrouter == nil {
		b.WriteString(`<p class="text-sm text-fg2" data-credentials-empty>No key yet. Without one, Standard agents are unavailable to you.</p>`)
	} else {
		b.WriteString(`<div class="flex items-center justify-between gap-2" data-credential-openrouter>` +
			`<span class="text-sm text-fg">API key <span class="font-mono text-xs text-muted">` + html.EscapeString(credentials.OpenRouterKeyPrefix) + `… · ` + html.EscapeString(openrouter.Fingerprint) + `</span></span>` +
			`<form method="POST" action="` + ui.CredentialsPath + `/openrouter/delete"><button type="submit" ` + credDelete + `>Delete</button></form></div>`)
	}
	b.WriteString(`<form method="POST" action="` + ui.CredentialsPath + `/openrouter" class="flex flex-col gap-2">
<label for="cred-openrouter-key" class="text-sm font-medium text-fg">OpenRouter API key</label>
<input id="cred-openrouter-key" name="key" type="password" required autocomplete="off" spellcheck="false" placeholder="` + credentials.OpenRouterKeyPrefix + `…" ` + credInput + `>
<p class="text-xs text-muted">An ordinary OpenRouter API key, billed to your OpenRouter account. Your agents use it as entered. Saving again replaces it; stop and start a running agent to give it the new one.</p>
<button type="submit" ` + credButton + `>Save OpenRouter API key</button></form></section>`)
}
