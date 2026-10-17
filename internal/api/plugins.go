package api

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
)

// THE PLUGIN INTERFACE (internal/plugins): out-of-process connectors that bring
// inbound messages to their owner's chief agent. OFF unless MUSTER_PLUGINS=1
// (Extensions.Plugins nil): then every route below answers 404 — before any
// credential is looked at — and /plugins is not drawn.
//
// TWO DOORS, AND WHICH CREDENTIAL OPENS WHICH IS THE DESIGN:
//
//   - /plugins (the page and its forms): a browser SESSION, and the forms
//     requireOperatorWrite. Minting a plugin token is a person's act; the token
//     is shown once and only its sha256 is stored.
//   - /plugin/v1/*: a PLUGIN token (mpl_…), through requirePluginToken, and
//     nothing else. It acts as its owner (authz.ViaPlugin), so row-level security
//     confines it to that owner's plugin rows. 🔴 THE SHARED HOOK AND SERVICE
//     TOKENS, A PERSONAL TOKEN, A RUNNER TOKEN AND AN AGENT'S TOKEN ARE ALL
//     REFUSED BY SHAPE before any lookup: a plugin is a process on someone
//     else's terms, and nothing but its own narrow credential may speak for it.
//
// 🔴 AND THE CONVERSE: AN mpl_ TOKEN IS REFUSED ON EVERY OTHER ROUTE. admit — the
// one function every authentication wrapper hands a request on through —
// refuses a request carrying a plugin-shaped credential unless the wrapper that
// admitted it is the plugin door (TestAPluginTokenIsRefusedOnEveryOtherRoute).

const (
	pluginsPath      = "/plugins"
	pluginAPIPrefix  = "/plugin/v1"
	pluginsNotServed = "this server does not serve plugins (MUSTER_PLUGINS is off)"
)

// PluginTokenRefusal is the 401 body for every refused credential on the plugin
// door — another kind of token, a malformed one, an unknown or revoked one — so
// a refusal says nothing about which.
const PluginTokenRefusal = "this route takes a plugin token (" + plugins.TokenPrefix + "…), minted at " + pluginsPath

// PluginTokenElsewhere is the 401 body when a plugin token is presented on any
// route that is not the plugin door.
const PluginTokenElsewhere = "a plugin token (" + plugins.TokenPrefix + "…) opens " + pluginAPIPrefix + "/* only"

func (s *Server) registerPluginRoutes(mux Mux) {
	mux.HandleFunc("GET "+pluginsPath, s.requirePluginsOn(s.requireSession(s.handlePluginsPage)))
	mux.HandleFunc("POST "+pluginsPath, s.requirePluginsOn(s.requireOperatorWrite(s.handlePluginCreate)))
	mux.HandleFunc("POST "+pluginsPath+"/{id}/revoke", s.requirePluginsOn(s.requireOperatorWrite(s.handlePluginRevoke)))

	// Reply drafts (plugin_drafts.go): the owner decides; Approve also needs a
	// fresh sign-in, checked in the handler. Reject (and withdrawing an approval,
	// which is a reject) does not.
	mux.HandleFunc("POST "+pluginsPath+"/drafts/{id}/approve", s.requirePluginsOn(s.requireOperatorWrite(s.handleDraftApprove)))
	mux.HandleFunc("POST "+pluginsPath+"/drafts/{id}/reject", s.requirePluginsOn(s.requireOperatorWrite(s.handleDraftReject)))

	mux.HandleFunc("POST "+pluginAPIPrefix+"/messages", s.requirePluginToken(s.handlePluginMessage))
	mux.HandleFunc("POST "+pluginAPIPrefix+"/drafts/claim", s.requirePluginToken(s.handlePluginDraftClaim))
	mux.HandleFunc("POST "+pluginAPIPrefix+"/drafts/{id}/result", s.requirePluginToken(s.handlePluginDraftResult))
	// Deterministic upstream notices → one task per external key (plugin_tasks.go).
	mux.HandleFunc("POST "+pluginTasksPath, s.requirePluginToken(s.handlePluginTask))
	// Session routing (session_routes.go): a MIRRORED session's reply_to_chief,
	// with its host's own muster credential acting as the owner (a personal
	// token, or the armed hook token). 404 with routing off. Fail-closed on an
	// unarmed hook tier: it steers what an outside party is sent.
	mux.HandleFunc("POST /api/routes/{id}/reply", s.requireArmedHookToken(s.handleRouteReply))
}

// requirePluginsOn answers 404 for a plugin route on a server with plugins off,
// before any credential is examined: "off" must look like "no such route".
func (s *Server) requirePluginsOn(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.ext.Plugins == nil {
			http.NotFound(w, r)
			return
		}
		next(w, r)
	}
}

// --- the plugin door --------------------------------------------------------

type pluginCtxKey struct{}

// requirePluginToken admits a PLUGIN token and nothing else, acting as its owner
// through authz.ViaPlugin. See the file header.
func (s *Server) requirePluginToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.ext.Plugins == nil {
			http.NotFound(w, r)
			return
		}
		tok := hookTokenFromRequest(r)
		if !plugins.TokenShapeOK(tok) {
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": PluginTokenRefusal})
			return
		}
		p, err := s.pluginFor(r.Context(), tok)
		switch {
		case err == nil:
		case errors.Is(err, plugins.ErrBadToken):
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": PluginTokenRefusal})
			return
		default:
			s.logger.Printf("plugins: token lookup failed: %v", err)
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "could not check the plugin token"})
			return
		}
		ctx := context.WithValue(r.Context(), pluginCtxKey{}, p)
		s.admit(w, r.WithContext(ctx), next, s.principalFor(r.Context(), p.OwnerID, authz.ViaPlugin))
	}
}

// pluginFor resolves a plugin token to its record.
//
// 🔴 IT RUNS UNDER authz.System FOR THE REASON runnerTokenFor DOES: it is how the
// request's owner is LEARNED, and plugins is under FORCE row-level security. The
// System context is used for this one store call and never handed on; the
// handler gets the request's context with a principal built from the row's
// owner_id. System-ledger entry.
func (s *Server) pluginFor(ctx context.Context, tok string) (plugins.Plugin, error) {
	return s.ext.Plugins.Authenticate(authz.System(ctx), tok)
}

func pluginFromContext(ctx context.Context) plugins.Plugin {
	p, _ := ctx.Value(pluginCtxKey{}).(plugins.Plugin)
	return p
}

// refusePluginTokenElsewhere is admit's half of "mpl_ opens /plugin/v1/* only":
// true (and the request answered) when a request admitted by any door but the
// plugin door carries a plugin-shaped credential.
func (s *Server) refusePluginTokenElsewhere(w http.ResponseWriter, r *http.Request, via authz.Via) bool {
	if via == authz.ViaPlugin || !plugins.LooksLikeToken(hookTokenFromRequest(r)) {
		return false
	}
	s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": PluginTokenElsewhere})
	return true
}

// handlePluginMessage is POST /plugin/v1/messages: store one inbound message
// (202 {id, status:"queued"}), or answer a repeat of its external_id with the
// stored row (200 {id, status}). A NEW message is then handed to the owner's
// chief in the background; the response does not wait for the turn.
func (s *Server) handlePluginMessage(w http.ResponseWriter, r *http.Request) {
	var in plugins.MessageIn
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, plugins.MaxRequestBytes))
	if err := dec.Decode(&in); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "the body is {\"external_id\",\"sender\",\"subject\",\"body\",\"received_at\" (RFC 3339),\"no_reply\"}"})
		return
	}
	p := pluginFromContext(r.Context())
	m, created, err := s.ext.Plugins.Submit(r.Context(), p.ID, in)
	switch {
	case err == nil:
	case errors.Is(err, plugins.ErrInvalid):
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": strings.TrimPrefix(err.Error(), plugins.ErrInvalid.Error()+": ")})
		return
	case errors.Is(err, plugins.ErrRateLimited), errors.Is(err, plugins.ErrQueueFull):
		w.Header().Set("Retry-After", "60")
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": strings.TrimPrefix(err.Error(), "plugins: ")})
		return
	case errors.Is(err, plugins.ErrNotFound):
		// Revoked between the token check and the insert.
		s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": PluginTokenRefusal})
		return
	default:
		s.logger.Printf("plugins: plugin %d submit: %v", p.ID, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the message could not be stored"})
		return
	}
	if !created {
		s.writeJSON(w, http.StatusOK, map[string]any{"id": m.ID, "status": string(m.Status)})
		return
	}
	s.deliverPluginMessage(r, p, m)
	s.writeJSON(w, http.StatusAccepted, map[string]any{"id": m.ID, "status": string(m.Status)})
}

// deliverPluginMessage starts the delivery of a just-stored message to the
// owner's chief, detached from the request (the turn can take minutes) but
// keeping its principal, and ended by EndStreams on shutdown so a cut-off turn
// is recorded (unconfirmed) rather than lost. WaitForPluginDeliveries waits for it.
//
// ⚠ NO DELIVERER (no agent gateway wired) LEAVES THE MESSAGE QUEUED. That is the
// honest state — nothing was handed to anyone — and the retry seam (a later
// caller passing a queued id to plugins.Deliverer.Deliver) picks it up once a
// gateway exists.
func (s *Server) deliverPluginMessage(r *http.Request, p plugins.Plugin, m plugins.Message) {
	if s.pluginDeliverer == nil {
		return
	}
	ctx, cancel := s.streamContext(context.WithoutCancel(r.Context()))
	s.pluginInFlight.Add(1)
	safeGo(s.logger, "plugin delivery", func() {
		defer s.pluginInFlight.Done()
		defer cancel()
		chief, err := s.chiefAgent(ctx)
		if err != nil && !errors.Is(err, ErrNoChief) {
			s.logger.Printf("plugins: plugin %d message %d: resolve the chief: %v", p.ID, m.ID, err)
		}
		s.pluginDeliverer.Deliver(ctx, p.ID, m.ID, chief)
	})
}

// WaitForPluginDeliveries blocks until every in-flight plugin delivery has
// recorded its outcome. Call during graceful shutdown, after EndStreams.
func (s *Server) WaitForPluginDeliveries() { s.pluginInFlight.Wait() }

// --- the page (session) -------------------------------------------------------

func (s *Server) handlePluginsPage(w http.ResponseWriter, r *http.Request) {
	s.renderPluginsPage(w, r, http.StatusOK, "", "")
}

func (s *Server) handlePluginCreate(w http.ResponseWriter, r *http.Request) {
	tok, _, err := s.ext.Plugins.Create(r.Context(), strings.TrimSpace(r.PostFormValue("name")),
		r.PostFormValue("label"), strings.TrimSpace(r.PostFormValue("data_scope")))
	switch {
	case errors.Is(err, plugins.ErrInvalid):
		s.renderPluginsPage(w, r, http.StatusBadRequest, "", strings.TrimPrefix(err.Error(), plugins.ErrInvalid.Error()+": "))
	case err != nil:
		s.logger.Printf("plugins: create: %v", err)
		s.renderPluginsPage(w, r, http.StatusInternalServerError, "", "The plugin could not be created.")
	default:
		// 200, NOT a redirect: the token is shown in THIS response only.
		s.renderPluginsPage(w, r, http.StatusOK, tok, "")
	}
}

func (s *Server) handlePluginRevoke(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	found := false
	if err == nil && id > 0 {
		found, err = s.ext.Plugins.Revoke(r.Context(), id)
		if err != nil {
			s.logger.Printf("plugins: revoke %d: %v", id, err)
			s.renderPluginsPage(w, r, http.StatusInternalServerError, "", "The plugin could not be revoked.")
			return
		}
	}
	if !found {
		s.renderPluginsPage(w, r, http.StatusNotFound, "", "No such plugin.")
		return
	}
	http.Redirect(w, r, pluginsPath, http.StatusSeeOther)
}

// renderPluginsPage writes /plugins. minted, when set, is a plugin token just
// created — the only time it is ever rendered.
func (s *Server) renderPluginsPage(w http.ResponseWriter, r *http.Request, code int, minted, problem string) {
	list, err := s.ext.Plugins.List(r.Context())
	if err != nil {
		s.logger.Printf("plugins: page list: %v", err)
		problem = "The plugin list could not be read."
	}
	queued, err := s.ext.Plugins.QueuedCounts(r.Context())
	if err != nil {
		s.logger.Printf("plugins: page queued counts: %v", err)
		problem = "The plugin list could not be read."
	}
	cards, err := s.ext.Plugins.DraftCards(r.Context(), pluginDraftsShown)
	if err != nil {
		s.logger.Printf("plugins: page drafts: %v", err)
		problem = "The reply drafts could not be read."
	}
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<section class="flex flex-col gap-4 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line" data-testid="plugins-page">
<h1 class="text-balance text-base font-semibold text-fg">Plugins</h1>
<p class="text-sm text-fg2">A plugin is a separate process that brings messages from another system to your chief agent. It holds a plugin token, which can post messages here and do nothing else. Each message reaches your chief once, marked as untrusted data.</p>
`)
	b.WriteString(problemNode(problem))
	if minted != "" {
		b.WriteString(`<div class="flex flex-col gap-2 rounded-md bg-bg px-3 py-3 ring-1 ring-inset ring-edge" data-testid="minted-plugin-token">
<p class="text-sm font-semibold text-fg">Copy it now — it will not be shown again. It can only post this plugin's messages.</p>
<input id="minted-plugin-token" readonly value="` + esc(minted) + `" aria-label="New plugin token"
 class="min-h-[44px] w-full rounded-md bg-s1 px-3 py-2 font-mono text-sm text-fg ring-1 ring-inset ring-edge" onclick="this.select()">
</div>
`)
	}
	b.WriteString(`<form method="POST" action="` + pluginsPath + `" class="flex flex-col gap-2" data-testid="plugin-create">
<label for="plugin-name" class="text-sm font-medium text-fg">Name <span class="text-xs text-fg2">lower-case letters, digits and hyphens</span></label>
<input id="plugin-name" name="name" maxlength="40" placeholder="example-inbox" class="` + flowsInputClass + `">
<label for="plugin-label" class="text-sm font-medium text-fg">Label <span class="text-xs text-fg2">what your chief's conversation for it is called</span></label>
<input id="plugin-label" name="label" maxlength="` + strconv.Itoa(plugins.MaxLabelRunes) + `" placeholder="Example inbox" class="` + flowsInputClass + `">
<label for="plugin-scope" class="text-sm font-medium text-fg">Data scope <span class="text-xs text-fg2">where your chief is told to record anything durable from these messages</span></label>
<input id="plugin-scope" name="data_scope" maxlength="64" placeholder="example-inbox" class="` + flowsInputClass + `">
<button type="submit" class="` + flowsButtonClass + `">Create plugin token</button>
</form>
<ul class="flex flex-col gap-2" data-testid="plugins">
`)
	if len(list) == 0 {
		b.WriteString(`<li class="text-sm text-fg2">No plugins yet.</li>`)
	}
	for _, p := range list {
		seen := "never seen"
		if p.LastSeenAt != nil {
			seen = "last seen " + p.LastSeenAt.UTC().Format("2006-01-02 15:04") + " UTC"
		}
		id := strconv.FormatInt(p.ID, 10)
		b.WriteString(`<li class="` + flowsCardClass + `" data-plugin-id="` + id + `">
<span class="break-all text-sm font-semibold text-fg">` + esc(p.Label) + ` <span class="font-mono text-xs text-fg2">` + esc(p.Name) + `</span></span>
<span class="text-xs text-fg2">scope ` + esc(p.DataScope) + ` · created ` + p.CreatedAt.UTC().Format("2006-01-02 15:04") + ` UTC</span>
<span class="text-xs text-fg2"><span data-plugin-seen>` + seen + `</span> · <span data-plugin-queued="` + strconv.Itoa(queued[p.ID]) + `">` +
			queuedText(queued[p.ID]) + `</span></span>
`)
		if p.RevokedAt != nil {
			b.WriteString(`<span class="text-xs font-semibold text-fg2">revoked</span>`)
		} else {
			b.WriteString(`<form method="POST" action="` + pluginsPath + `/` + id + `/revoke">
<button type="submit" class="` + flowsQuietButton + `">Revoke</button></form>`)
		}
		b.WriteString("</li>\n")
	}
	b.WriteString("</ul>\n")
	b.WriteString(draftsSectionHTML(cards))
	b.WriteString(`<a href="/" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Back to muster</a>
</section>`)
	b.WriteString(s.draftStepUpHTML(r))
	writeAuthDocument(w, code, "Plugins", b.String())
}

// queuedText is a plugin's undelivered-message count in words.
func queuedText(n int) string {
	switch n {
	case 0:
		return "nothing waiting for your chief"
	case 1:
		return "1 message waiting for your chief"
	}
	return strconv.Itoa(n) + " messages waiting for your chief"
}
