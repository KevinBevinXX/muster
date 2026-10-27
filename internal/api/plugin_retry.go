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

// RETRYING A PLUGIN MESSAGE THE AGENT NEVER CONFIRMED (internal/plugins/retry.go).
//
// Two doors onto ONE handler body (retryPluginMessage):
//
//   - POST /plugins/messages/{id}/retry — the /plugins page's Retry button, behind
//     requireOperatorWrite exactly like Revoke and a draft's Reject (a session and
//     same-origin). No fresh sign-in: like Reject, nothing leaves muster here — the
//     message goes back to the owner's own agent. What the agent then does is
//     what it does with any message: a reply it drafts waits for an Approve
//     (which does take one), EXCEPT an answer it routes through a session for a
//     verified sender the owner allowlisted for this plugin
//     (MUSTER_ROUTE_RELAY_ALLOW), which relayRoute approves automatically. The
//     page's copy (retryCopy) says so; it used to claim every draft waits.
//   - POST /api/plugins/messages/{id}/retry — `muster plugins retry <id>`, behind
//     requireArmedHookToken (a personal token acts as its owner; the shared hook
//     token as the operator, and only on an armed server): it steers what the
//     agent is handed, like the session-routing reply on the same tier.
//
// The message is requeued synchronously — so a refusal (and a double click, which
// finds it queued) is the answer — and then handed to the CURRENT agent through
// the same Deliverer every new message takes: the session claim, the chat turn
// with the agent's tools, the attempt counted. With no agent gateway wired the
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
	// 🔴 THE PRE-CHECK ASKS THIS MESSAGE'S OWN PLUGIN, so it must read the message
	// before requeueing it. The old check asked whether the OWNER had an agent,
	// which was one answer for every plugin; a target is per plugin now, and
	// requeueing a message nothing can deliver would leave it queued with the
	// refusal invisible.
	switch msg, err := s.ext.Plugins.Message(ctx, id); {
	case errors.Is(err, plugins.ErrNotFound):
		return http.StatusNotFound, plugins.Message{}, "No such message."
	case err != nil:
		s.logger.Printf("plugins: retry %d: read the message: %v", id, err)
		return http.StatusInternalServerError, plugins.Message{}, "The message could not be looked up."
	default:
		if _, terr := s.pluginTargetByID(ctx, msg.PluginID); terr != nil {
			if errors.Is(terr, ErrNoPluginTarget) {
				return http.StatusConflict, plugins.Message{},
					"That plugin names no target agent to deliver it to. Pick one on the plugins page."
			}
			s.logger.Printf("plugins: retry %d: resolve the target agent: %v", id, terr)
			return http.StatusInternalServerError, plugins.Message{}, "The plugin's target agent could not be looked up."
		}
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

// startPluginDelivery hands a queued message to its plugin's CURRENT target in
// the background, exactly as deliverPluginMessage does for a new one (detached from
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
		target, err := s.pluginTargetByID(dctx, pluginID)
		if err != nil && !errors.Is(err, ErrNoPluginTarget) {
			s.logger.Printf("plugins: message %d: resolve the target agent: %v", messageID, err)
		}
		s.pluginDeliverer.Deliver(dctx, pluginID, messageID, target)
	})
}

// retryHasNoTarget reports whether the page must disable its Retry buttons:
// there is something to retry and NOT ONE of the plugins involved names a target
// agent (the same lookup retryPluginMessage refuses on).
//
// 🔴 IT IS "NONE OF THEM", NOT "THE OWNER HAS NO AGENT". A target is per plugin
// now, so a list can mix deliverable and undeliverable messages; one button is
// drawn for the whole section, so it may only be disabled when NO message in it
// could be delivered. Where some could, the buttons stay on and the POST refuses
// the ones that cannot, naming the plugin.
//
// ⚠ A LOOKUP THAT FAILS LEAVES THE BUTTONS ON — the POST answers for itself —
// rather than calling an outage "no target".
func (s *Server) retryHasNoTarget(ctx context.Context, msgs []plugins.Message) bool {
	if len(msgs) == 0 {
		return false
	}
	seen := make(map[int64]bool, len(msgs))
	for _, m := range msgs {
		if seen[m.PluginID] {
			continue
		}
		seen[m.PluginID] = true
		_, err := s.pluginTargetByID(ctx, m.PluginID)
		if err == nil {
			return false
		}
		if !errors.Is(err, ErrNoPluginTarget) {
			s.logger.Printf("plugins: page: resolve plugin %d's target agent: %v", m.PluginID, err)
			return false
		}
	}
	return true
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

// retryCopy is the /plugins explanation above the retry list.
//
// 🔴 IT MUST NOT SAY EVERY REPLY WAITS FOR APPROVAL: an answer routed through a
// session for a verified sender the owner allowlisted for this plugin
// (MUSTER_ROUTE_RELAY_ALLOW) is approved automatically (relayRoute), on a
// retried delivery exactly as on a first one. Retry does not change that.
const retryCopy = "The target agent's turn on each of these was cut off, so it may or may not have acted on it. " +
	"Retry hands it to the plugin's target again, which handles it like any message: a reply it drafts waits for your " +
	"approval, except an answer it routes through one of your sessions to a verified sender you allowlisted " +
	"for this plugin, which is sent without it."

// retryNoTarget is what the retry section says, in place of working buttons,
// when not one of the plugins involved names a target agent: the reason every
// Retry would be refused (409).
//
// ⚠ IT NAMES THE PLUGIN, NOT A LABEL. The old copy told the operator that "an
// agent becomes the target agent when its display name is \"agent\"", which is no
// longer true of anything: delivery reads a target chosen per plugin.
const retryNoTarget = "Retry is off: these plugins name no target agent to deliver to. " +
	"Pick one for each plugin under Plugins above."

// retryableSectionHTML is the /plugins list of messages the agent never
// confirmed, each with its Retry button. The sender and subject are an outside
// party's text: escaped, never interpreted.
//
// noTarget: not one of these messages' plugins names a target agent, so every
// retry would be refused (409, retryPluginMessage). The buttons are then
// DISABLED and the section says why (retryNoTarget) — the messages are still
// listed, since they still need a decision once a target is picked. The POST
// still refuses on its own; this is the page not offering what the write would
// refuse.
func retryableSectionHTML(msgs []plugins.Message, noTarget bool) string {
	if len(msgs) == 0 {
		return ""
	}
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<section id="plugin-retryable" class="flex flex-col gap-2" data-testid="plugin-retryable">
<h2 class="text-sm font-semibold text-fg">Not confirmed by the target agent</h2>
<p class="text-xs text-fg2" data-retry-copy>` + retryCopy + `</p>
`)
	disabled := ""
	if noTarget {
		b.WriteString(`<p id="retry-no-target-reason" class="text-xs font-semibold text-fg" data-retry-no-target>` + esc(retryNoTarget) + `</p>
`)
		disabled = ` disabled aria-disabled="true" aria-describedby="retry-no-target-reason"`
	}
	b.WriteString(`<ul class="flex flex-col gap-2">
`)
	for _, m := range msgs {
		id := strconv.FormatInt(m.ID, 10)
		when := m.CreatedAt.UTC().Format("2006-01-02 15:04") + " UTC"
		b.WriteString(`<li class="` + flowsCardClass + `" data-retry-message-id="` + id + `">
<span class="break-all text-sm text-fg">` + esc(m.Sender) + ` <span class="text-fg2">` + esc(m.Subject) + `</span></span>
<span class="text-xs text-fg2">message ` + id + ` · ` + when + ` · ` + strconv.Itoa(m.Attempts) + ` attempt(s)</span>
<form method="POST" action="` + pluginsPath + `/messages/` + id + `/retry">
<button type="submit" class="` + flowsButtonClass + ` disabled:cursor-not-allowed disabled:opacity-50" data-retry-button` + disabled + `>Retry</button></form>
</li>
`)
	}
	b.WriteString("</ul>\n</section>\n")
	return b.String()
}
