package api

import (
	"context"
	"errors"
	"html"
	"net/http"
	"strings"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/stepup"
)

// A WAITING HERMES WINDOW IN THE ATTENTION QUEUE (Hermes Phase 4,
// docs/design/hermes.md, "Questions and approvals"). Its host's hook says
// what it waits on: an approval, a clarify question or a sudo password. Only
// an APPROVAL is ever answered here, and only when its host answers
// approvals (-interactive and -hermes-answer) and could read it through
// Hermes's gateway viewer: Allow once or Deny, never "session" or "always".
// A sudo prompt is never answered from muster; a clarify question is
// answered in the terminal.
//
// 🔴 THE GATES COPY THE CLAUDE ASK-ANSWER PATH (mirror_ask.go's
// handleMirrorAskAnswer): the session's owner (another owner's host is not
// found), same-origin (requireOperatorWrite), a fresh sign-in
// (internal/stepup) — and the hub and host check again that the window still
// waits on exactly this approval, offering this answer.

// attentionHermesPath answers a Hermes approval (the no-script form).
const attentionHermesPath = attentionPath + "/hermes/approve"

// The words an answered or refused Hermes approval shows.
const (
	// hermesAnsweredText: the CLI's modal stays drawn after a viewer's answer
	// until a key is pressed there (measured, S1 and Phase 4).
	hermesAnsweredText = "Answered here. The terminal will show a stale prompt until a key is pressed there."
	hermesGoneText     = "Hermes is no longer waiting on that approval (it was answered in the terminal, or timed out). Nothing was answered here."
	hermesUnreachText  = "muster could not reach Hermes's gateway on that host, so nothing was answered: answer it in the terminal."
	hermesUnknownText  = "muster sent the answer but did not hear back: it may have been answered. Check the terminal before answering again."
	hermesNoAnswerText = "That host does not answer Hermes approvals (its muster-mirror needs -interactive and -hermes-answer): answer it in the terminal."
)

// hermesApprover answers a Hermes approval; *mirror.Service implements it.
type hermesApprover interface {
	AnswerHermesApproval(ctx context.Context, host, session, window, promptID, choice string) error
}

// hermesChoiceLabel is a choice as its button says it.
var hermesChoiceLabel = map[string]string{"once": "Allow once", "deny": "Deny"}

// hermesAskHTML is a waiting Hermes item's answer area: Allow once / Deny for
// an approval muster can answer, else what to do instead. "" for an item that
// is not a Hermes window's.
func hermesAskHTML(it attentionItem) string {
	ask := it.hermes
	if ask == nil {
		return ""
	}
	esc := html.EscapeString
	note := func(s string) string {
		return `<p class="text-sm text-fg2" data-hermes-note>` + esc(s) + `</p>
`
	}
	switch {
	case ask.kind == mirror.PendingSudo:
		return note("muster never answers a password prompt: type it in the terminal.")
	case ask.kind == mirror.PendingApproval && ask.promptID != "" && len(ask.choices) > 0:
		ref := it.card.ref
		var b strings.Builder
		b.WriteString(`<form method="POST" action="` + attentionHermesPath + `" class="flex flex-col gap-2" data-hermes-approval>
<input type="hidden" name="host" value="` + esc(ref.Host) + `">
<input type="hidden" name="session" value="` + esc(ref.Session) + `">
<input type="hidden" name="window" value="` + esc(ref.Window) + `">
<input type="hidden" name="prompt" value="` + esc(ask.promptID) + `">
<div class="flex flex-wrap gap-2">`)
		for _, c := range ask.choices {
			label, ok := hermesChoiceLabel[c]
			if !ok {
				continue
			}
			class := flowsQuietButton
			if c == "deny" {
				class = flowsButtonClass
			}
			b.WriteString(`<button type="submit" name="choice" value="` + esc(c) + `" class="` + class + `" data-hermes-choice="` + esc(c) + `">` + esc(label) + `</button>`)
		}
		b.WriteString(`</div>
</form>
`)
		b.WriteString(note("If you answer here, the terminal keeps showing the prompt until a key is pressed there."))
		return b.String()
	}
	return note("Answer it in the terminal.")
}

// handleAttentionHermesApprove is POST /attention/hermes/approve (host,
// session, window, prompt, choice): answer that window's Hermes approval
// through its host, then back to the queue.
func (s *Server) handleAttentionHermesApprove(w http.ResponseWriter, r *http.Request) {
	if s.ext.Attention == nil || s.ext.Mirror == nil {
		http.NotFound(w, r)
		return
	}
	a, can := s.ext.Mirror.(hermesApprover)
	if !can {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		s.renderAttentionPage(w, r, http.StatusBadRequest, "That answer could not be read.")
		return
	}
	h, e, listed, _, found := s.mirrorEntry(r, r.PostFormValue("host"), r.PostFormValue("session"), r.PostFormValue("window"))
	if !found || !listed {
		s.renderAttentionPage(w, r, http.StatusNotFound, "That window is gone: "+hermesGoneText)
		return
	}
	if !stepup.Fresh(r.Context(), s.stepUpNow()) {
		w.Header().Set("X-Muster-Step-Up", "required")
		s.renderAttentionPage(w, r, http.StatusForbidden, stepup.ErrStale.Error())
		return
	}
	// 🔴 NOT THE REQUEST'S CONTEXT: a page that goes away must not cut the
	// wait while the host may be answering. The hub bounds it.
	err := a.AnswerHermesApproval(context.WithoutCancel(r.Context()), h.Host, e.Session, e.Window, r.PostFormValue("prompt"), r.PostFormValue("choice"))
	if err != nil {
		code, text := http.StatusConflict, hermesGoneText
		switch {
		case errors.Is(err, mirror.ErrStepUpRequired):
			code, text = http.StatusForbidden, stepup.ErrStale.Error()
		case errors.Is(err, mirror.ErrHermesAnswerUnsupported):
			text = hermesNoAnswerText
		case errors.Is(err, mirror.ErrHermesUnreachable):
			code, text = http.StatusBadGateway, hermesUnreachText
		case errors.Is(err, mirror.ErrHermesAnswerUnknown):
			code, text = http.StatusGatewayTimeout, hermesUnknownText
		case errors.Is(err, mirror.ErrHostOffline):
			code, text = http.StatusBadGateway, "That host is offline: nothing was answered. Answer it in the terminal."
		case errors.Is(err, mirror.ErrHermesApprovalGone):
		default:
			s.logger.Printf("attention: answer a Hermes approval on %s: %v", h.Host, err)
			code, text = http.StatusBadGateway, "The approval was not answered: "+mirror.OneLine(err.Error(), 200)
		}
		s.renderAttentionPage(w, r, code, text)
		return
	}
	s.logger.Printf("attention: answered a Hermes approval on %s (%s)", h.Host, r.PostFormValue("choice"))
	http.Redirect(w, r, attentionPath+"?answered=hermes", http.StatusSeeOther)
}

// attentionNotice is the page's notice for a query that names one (an
// answer that went through), "" for none.
func attentionNotice(r *http.Request) string {
	if r.Method != http.MethodGet || r.URL.Query().Get("answered") != "hermes" {
		return ""
	}
	return `<p role="status" class="rounded-lg bg-s2 px-3 py-2 text-sm text-fg ring-1 ring-inset ring-line" data-testid="attention-notice">` +
		html.EscapeString(hermesAnsweredText) + `</p>`
}
