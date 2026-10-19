package api

import (
	"context"
	"errors"
	"html"
	"net/http"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/plugins"
)

// RETRYING A PLUGIN MESSAGE THE CHIEF NEVER CONFIRMED (internal/plugins/retry.go).
//
// Two doors onto ONE handler body (retryPluginMessage):
//
//   - POST /plugins/messages/{id}/retry — the /plugins page's Retry button, behind
//     requireOperatorWrite exactly like Revoke and a draft's Reject (a session and
//     same-origin). No fresh sign-in: like Reject, nothing leaves muster here — the
//     message goes back to the owner's own chief, and anything the chief then
//     drafts still waits for an Approve, which does take one.
//   - POST /api/plugins/messages/{id}/retry — `muster plugins retry <id>`, behind
//     requireArmedHookToken (a personal token acts as its owner; the shared hook
//     token as the operator, and only on an armed server): it steers what the
//     chief is handed, like the session-routing reply on the same tier.
//
// The message is requeued synchronously — so a refusal (and a double click, which
// finds it queued) is the answer — and then handed to the CURRENT chief through
// the same Deliverer every new message takes: the session claim, the chat turn
// with the chief's tools, the attempt counted. With no agent gateway wired the
// message simply stays queued, as a new one would.

const (
	pluginRetryPath    = pluginsPath + "/messages/{id}/retry"
	pluginRetryAPIPath = "/api/plugins/messages/{id}/retry"
	// pluginRetryShown bounds the page's list of retryable messages.
	pluginRetryShown = 20
)

func (s *Server) registerPluginRetryRoutes(mux Mux) {
	mux.HandleFunc("POST "+pluginRetryPath, s.requirePluginsOn(s.requireOperatorWrite(s.handlePluginRetryForm)))
	mux.HandleFunc("POST "+pluginRetryAPIPath, s.requirePluginsOn(s.requireArmedHookToken(s.handlePluginRetryAPI)))
}

// retryPluginMessage is the retry: the status code, and the refusal's sentence
// ("" on success).
func (s *Server) retryPluginMessage(ctx context.Context, id int64) (int, plugins.Message, string) {
	if _, err := s.chiefAgent(ctx); err != nil {
		if errors.Is(err, ErrNoChief) {
			return http.StatusConflict, plugins.Message{}, "You have no chief agent to deliver it to."
		}
		s.logger.Printf("plugins: retry %d: resolve the chief: %v", id, err)
		return http.StatusInternalServerError, plugins.Message{}, "Your chief could not be looked up."
	}
	m, err := s.ext.Plugins.Requeue(ctx, id)
	switch {
	case err == nil:
	case errors.Is(err, plugins.ErrNotFound):
		return http.StatusNotFound, plugins.Message{}, "No such message."
	case errors.Is(err, plugins.ErrNotRetryable):
		return http.StatusConflict, plugins.Message{}, "That message cannot be retried: " +
			strings.TrimPrefix(err.Error(), plugins.ErrNotRetryable.Error()+": ") + "."
	default:
		s.logger.Printf("plugins: retry %d: %v", id, err)
		return http.StatusInternalServerError, plugins.Message{}, "The retry could not be recorded."
	}
	s.logger.Printf("plugins: message %d requeued by its owner for another delivery", m.ID)
	s.startPluginDelivery(ctx, m.PluginID, m.ID)
	return http.StatusAccepted, m, ""
}

// startPluginDelivery hands a queued message to the owner's CURRENT chief in the
// background, exactly as deliverPluginMessage does for a new one (detached from
// the request but keeping its principal, ended by EndStreams, waited for by
// WaitForPluginDeliveries).
func (s *Server) startPluginDelivery(ctx context.Context, pluginID, messageID int64) {
	if s.pluginDeliverer == nil {
		return
	}
	dctx, cancel := s.streamContext(context.WithoutCancel(ctx))
	s.pluginInFlight.Add(1)
	safeGo(s.logger, "plugin retry delivery", func() {
		defer s.pluginInFlight.Done()
		defer cancel()
		chief, err := s.chiefAgent(dctx)
		if err != nil && !errors.Is(err, ErrNoChief) {
			s.logger.Printf("plugins: message %d: resolve the chief: %v", messageID, err)
		}
		s.pluginDeliverer.Deliver(dctx, pluginID, messageID, chief)
	})
}

func (s *Server) handlePluginRetryForm(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderPluginsPage(w, r, http.StatusNotFound, "", "No such message.")
		return
	}
	code, _, problem := s.retryPluginMessage(r.Context(), id)
	if problem != "" {
		s.renderPluginsPage(w, r, code, "", problem)
		return
	}
	http.Redirect(w, r, pluginsPath+"#plugin-retryable", http.StatusSeeOther)
}

func (s *Server) handlePluginRetryAPI(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such message"})
		return
	}
	code, m, problem := s.retryPluginMessage(r.Context(), id)
	if problem != "" {
		s.writeJSON(w, code, map[string]any{"error": problem})
		return
	}
	s.writeJSON(w, code, map[string]any{"id": m.ID, "status": string(m.Status), "retrying": true})
}

// retryableSectionHTML is the /plugins list of messages the chief never
// confirmed, each with its Retry button. The sender and subject are an outside
// party's text: escaped, never interpreted.
func retryableSectionHTML(msgs []plugins.Message) string {
	if len(msgs) == 0 {
		return ""
	}
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<section id="plugin-retryable" class="flex flex-col gap-2" data-testid="plugin-retryable">
<h2 class="text-sm font-semibold text-fg">Not confirmed by your chief</h2>
<p class="text-xs text-fg2">Your chief's turn on each of these was cut off, so it may or may not have acted on it. Retry hands it to your chief again; anything it drafts still waits for your approval.</p>
<ul class="flex flex-col gap-2">
`)
	for _, m := range msgs {
		id := strconv.FormatInt(m.ID, 10)
		when := m.CreatedAt.UTC().Format("2006-01-02 15:04") + " UTC"
		b.WriteString(`<li class="` + flowsCardClass + `" data-retry-message-id="` + id + `">
<span class="break-all text-sm text-fg">` + esc(m.Sender) + ` <span class="text-fg2">` + esc(m.Subject) + `</span></span>
<span class="text-xs text-fg2">message ` + id + ` · ` + when + ` · ` + strconv.Itoa(m.Attempts) + ` attempt(s)</span>
<form method="POST" action="` + pluginsPath + `/messages/` + id + `/retry">
<button type="submit" class="` + flowsButtonClass + `" data-retry-button>Retry</button></form>
</li>
`)
	}
	b.WriteString("</ul>\n</section>\n")
	return b.String()
}
