package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/flows"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
)

// REPLY DRAFTS, THE OWNER'S AND THE PLUGIN'S HALVES (internal/plugins/drafts.go
// holds the rules; reply_draft_tool.go is the chief's half).
//
//   - The OWNER decides on /plugins: each pending draft is a card showing the
//     message it answers (untrusted, escaped) and the EXACT text that will be
//     sent (flows.VisibleEscape, with its length). Approve and "Approve again"
//     need a FRESH sign-in (stepup.Fresh, as typing into a mirrored session
//     does): an approval sends text to an outside party in the owner's name.
//     Reject needs none — refusing to send is never the risky direction.
//   - The PLUGIN collects: POST /plugin/v1/drafts/claim long-polls for an
//     approved draft and leases it; POST /plugin/v1/drafts/{id}/result settles it
//     as sent or failed. muster never calls the plugin.
//
// 🔴 RETRY-ON-CLAIM IS THE ONLY RETRY. A message whose delivery to the chief was
// provably not made (the chief was down, busy, absent) stays queued; the START of
// every claim retries the owner's queued messages (retryQueuedPluginMessages).
// There is no background loop: a plugin that long-polls is the clock.

// pluginRetryBatch bounds how many queued messages one claim retries.
const pluginRetryBatch = 20

// pluginDraftsShown is how many drafts /plugins lists.
const pluginDraftsShown = 50

// claimPollInterval is how often a waiting claim looks again when nothing in
// this process signalled an approval (another replica may have approved one).
const claimPollInterval = time.Second

// DraftStepUpWhy is the confirm dialog's sentence on /plugins.
func DraftStepUpWhy() string {
	return "Approving a reply sends it to an outside party in your name, so it needs a sign-in from the last " +
		stepup.WindowText() + "."
}

// DraftStepUpRequired is the /plugins notice for an approval refused as stale
// (the page's script did not ask first).
const DraftStepUpRequired = "Approving a reply needs a recent sign-in. Confirm it's you, then approve again."

// draftSignal wakes waiting claims when this process approves a draft. The zero
// value is ready.
type draftSignal struct {
	mu sync.Mutex
	ch chan struct{}
}

// wait returns a channel closed at the next signal.
func (d *draftSignal) wait() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ch == nil {
		d.ch = make(chan struct{})
	}
	return d.ch
}

// signal wakes every current waiter.
func (d *draftSignal) signal() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ch != nil {
		close(d.ch)
		d.ch = nil
	}
}

// --- the plugin's half ---------------------------------------------------------

// handlePluginDraftClaim is POST /plugin/v1/drafts/claim {wait_seconds}: 200
// with one approved draft (now leased for plugins.LeaseDuration), or 204 when
// none arrived within wait_seconds (0–25; more is 25). It first starts a retry of
// the owner's queued messages (see the file header).
func (s *Server) handlePluginDraftClaim(w http.ResponseWriter, r *http.Request) {
	var in struct {
		WaitSeconds int `json:"wait_seconds"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err == nil && len(bytes.TrimSpace(body)) > 0 {
		err = json.Unmarshal(body, &in)
	}
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": `the body is {"wait_seconds": 0-25}`})
		return
	}
	wait := time.Duration(min(max(in.WaitSeconds, 0), int(plugins.MaxClaimWait/time.Second))) * time.Second
	p := pluginFromContext(r.Context())
	s.retryQueuedPluginMessages(r, p)

	ctx, cancel := s.streamContext(r.Context())
	defer cancel()
	deadline := time.Now().Add(wait)
	for {
		woken := s.draftWake.wait()
		c, ok, err := s.ext.Plugins.ClaimDraft(ctx, p.ID)
		if err != nil {
			if ctx.Err() != nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			s.logger.Printf("plugins: plugin %d claim: %v", p.ID, err)
			s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the claim could not be made"})
			return
		}
		if ok {
			s.writeJSON(w, http.StatusOK, c)
			return
		}
		left := time.Until(deadline)
		if left <= 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t := time.NewTimer(min(left, claimPollInterval))
		select {
		case <-ctx.Done():
			t.Stop()
			w.WriteHeader(http.StatusNoContent)
			return
		case <-woken:
		case <-t.C:
		}
		t.Stop()
	}
}

// handlePluginDraftResult is POST /plugin/v1/drafts/{id}/result {ok,
// external_ref, detail}: settle a draft this plugin claimed as sent (ok) or
// failed. 200 {draft_id, status}; the same result again is 200 and changes
// nothing; the other result is 409; a draft never claimed (or re-approved since)
// is 409; another plugin's, or none, is 404.
func (s *Server) handlePluginDraftResult(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such draft"})
		return
	}
	var in struct {
		OK          *bool  `json:"ok"`
		ExternalRef string `json:"external_ref"`
		Detail      string `json:"detail"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 3*plugins.MaxDetailBytes)).Decode(&in); err != nil || in.OK == nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": `the body is {"ok": true|false, "external_ref", "detail"}`})
		return
	}
	p := pluginFromContext(r.Context())
	d, err := s.ext.Plugins.FinishDraft(r.Context(), p.ID, id, plugins.DraftResult{OK: *in.OK, ExternalRef: in.ExternalRef, Detail: in.Detail})
	switch {
	case err == nil:
		s.writeJSON(w, http.StatusOK, map[string]any{"draft_id": d.ID, "status": string(d.Status)})
	case errors.Is(err, plugins.ErrInvalid):
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": strings.TrimPrefix(err.Error(), plugins.ErrInvalid.Error()+": ")})
	case errors.Is(err, plugins.ErrNotFound):
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such draft claimed by this plugin"})
	case errors.Is(err, plugins.ErrResultConflict):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "the draft already settled with the other result",
			"draft_id": d.ID, "status": string(d.Status)})
	case errors.Is(err, plugins.ErrDraftState):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "the draft is not claimed by this plugin (never claimed, or approved again since)",
			"draft_id": d.ID, "status": string(d.Status)})
	default:
		s.logger.Printf("plugins: plugin %d result for draft %d: %v", p.ID, id, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the result could not be recorded"})
	}
}

// retryQueuedPluginMessages starts one attempt at the owner's queued messages,
// oldest first, detached from the request like deliverPluginMessage (and waited
// for by WaitForPluginDeliveries). It stops at the first attempt that left its
// message queued: the chief is not taking turns, and the next claim will try
// again. One retry per owner at a time in this process.
func (s *Server) retryQueuedPluginMessages(r *http.Request, p plugins.Plugin) {
	if s.pluginDeliverer == nil || p.OwnerID <= 0 {
		return
	}
	if _, busy := s.pluginRetrying.LoadOrStore(p.OwnerID, true); busy {
		return
	}
	ctx, cancel := s.streamContext(context.WithoutCancel(r.Context()))
	s.pluginInFlight.Add(1)
	safeGo(s.logger, "plugin retry", func() {
		defer s.pluginInFlight.Done()
		defer cancel()
		defer s.pluginRetrying.Delete(p.OwnerID)
		msgs, err := s.ext.Plugins.QueuedForOwner(ctx, p.OwnerID, pluginRetryBatch)
		if err != nil {
			s.logger.Printf("plugins: owner %d: list queued messages: %v", p.OwnerID, err)
			return
		}
		if len(msgs) == 0 {
			return
		}
		chief, err := s.chiefAgent(ctx)
		if err != nil && !errors.Is(err, ErrNoChief) {
			s.logger.Printf("plugins: owner %d retry: resolve the chief: %v", p.OwnerID, err)
		}
		for _, m := range msgs {
			if s.pluginDeliverer.Deliver(ctx, m.PluginID, m.ID, chief).Outcome == plugins.OutcomePending {
				return
			}
		}
	})
}

// --- the owner's half (/plugins) ----------------------------------------------

func (s *Server) handleDraftApprove(w http.ResponseWriter, r *http.Request) {
	s.decideDraft(w, r, "approve")
}

func (s *Server) handleDraftReapprove(w http.ResponseWriter, r *http.Request) {
	s.decideDraft(w, r, "reapprove")
}

func (s *Server) handleDraftReject(w http.ResponseWriter, r *http.Request) {
	s.decideDraft(w, r, "reject")
}

// decideDraft is the three decision routes. 🔴 THE STEP-UP IS CHECKED HERE, ON
// THE SERVER, FOR BOTH APPROVALS, whatever the page decided: the page's script
// only asks BEFORE submitting so the owner is not bounced. A stale approval is
// 403 — to a script with X-Muster-Step-Up (refuseStale), to a browser form the
// page again, saying so.
func (s *Server) decideDraft(w http.ResponseWriter, r *http.Request, how string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.renderPluginsPage(w, r, http.StatusNotFound, "", "No such draft.")
		return
	}
	if how != "reject" && !stepup.Fresh(r.Context(), s.stepUpNow()) {
		if isDocumentNavigation(r) {
			w.Header().Set("X-Muster-Step-Up", "required")
			s.renderPluginsPage(w, r, http.StatusForbidden, "", DraftStepUpRequired)
			return
		}
		refuseStale(w)
		return
	}
	var d plugins.Draft
	switch how {
	case "approve":
		d, err = s.ext.Plugins.DecideDraft(r.Context(), id, true)
	case "reapprove":
		d, err = s.ext.Plugins.ReapproveDraft(r.Context(), id)
	default:
		d, err = s.ext.Plugins.DecideDraft(r.Context(), id, false)
	}
	switch {
	case err == nil:
	case errors.Is(err, plugins.ErrNotFound):
		s.renderPluginsPage(w, r, http.StatusNotFound, "", "No such draft.")
		return
	case errors.Is(err, plugins.ErrDraftState):
		s.renderPluginsPage(w, r, http.StatusConflict, "", "That draft is no longer awaiting this decision (it is "+string(d.Status)+").")
		return
	default:
		s.logger.Printf("plugins: draft %d %s: %v", id, how, err)
		s.renderPluginsPage(w, r, http.StatusInternalServerError, "", "The decision could not be recorded.")
		return
	}
	if d.Status == plugins.DraftApproved {
		s.draftWake.signal()
	}
	http.Redirect(w, r, pluginsPath+"#"+draftCardAnchor(d.ID), http.StatusSeeOther)
}

// draftsSectionHTML is /plugins' drafts: a card per draft, those awaiting a
// decision first.
func draftsSectionHTML(cards []plugins.DraftCard) string {
	var b strings.Builder
	b.WriteString(`<h2 class="text-sm font-semibold text-fg">Reply drafts</h2>
<p class="text-xs text-fg2">Your chief drafts replies; nothing is sent until you approve one. The plugin then sends it once.</p>
<ul class="flex flex-col gap-2" data-testid="plugin-drafts">
`)
	if len(cards) == 0 {
		b.WriteString(`<li class="text-sm text-fg2">No drafts.</li>`)
	}
	for _, c := range cards {
		b.WriteString(draftCardHTML(c))
	}
	b.WriteString("</ul>\n")
	return b.String()
}

// draftStatusText is a draft's state in words.
func draftStatusText(d plugins.Draft) string {
	at := func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return " " + t.UTC().Format("2006-01-02 15:04") + " UTC"
	}
	switch d.Status {
	case plugins.DraftPending:
		return "Awaiting your decision · expires" + at(&d.ExpiresAt)
	case plugins.DraftApproved:
		if d.ClaimedAt != nil {
			return "Approved · the plugin collected it" + at(d.ClaimedAt) + " and is sending it"
		}
		return "Approved" + at(d.ApprovedAt) + " · waiting for the plugin to collect it"
	case plugins.DraftSent:
		return "Sent" + at(d.FinishedAt)
	case plugins.DraftFailed:
		return "The plugin could not send it" + at(d.FinishedAt)
	case plugins.DraftUnconfirmed:
		return "Unconfirmed: the plugin collected it" + at(d.ClaimedAt) + " and never said whether it was sent. " +
			"It may have been. muster will not offer it again unless you approve it again."
	case plugins.DraftRejected:
		return "Rejected" + at(d.DecidedAt)
	case plugins.DraftSuperseded:
		return "Replaced by a newer draft"
	case plugins.DraftExpired:
		return "Expired unsent"
	}
	return string(d.Status)
}

// draftCardHTML draws one draft.
//
// 🔴 WHAT IS APPROVED IS WHAT IS SEEN (flowRequestCardHTML's rule). The draft is
// drawn through flows.VisibleEscape — printable ASCII as itself, everything else
// spelled out — with its length in characters, so nothing invisible or
// look-alike rides along unseen. The message it answers is the OUTSIDE party's
// text: drawn escaped, in its own box, labelled untrusted, and never mistaken for
// the draft.
func draftCardHTML(c plugins.DraftCard) string {
	esc := html.EscapeString
	d, m := c.Draft, c.Message
	id := strconv.FormatInt(d.ID, 10)
	runes := strconv.Itoa(utf8.RuneCountInString(d.Body))
	var b strings.Builder
	b.WriteString(`<li id="` + draftCardAnchor(d.ID) + `" class="` + flowsCardClass + `" data-draft-id="` + id +
		`" data-draft-status="` + esc(string(d.Status)) + `">
<span class="break-all text-sm font-semibold text-fg">` + esc(flows.VisibleEscape(d.AgentName)) + ` drafted a reply · ` +
		esc(c.PluginLabel) + `</span>
<span class="text-xs text-fg2" data-draft-state>` + esc(draftStatusText(d)) + `</span>
<div class="flex flex-col gap-1 rounded-md bg-s1 px-3 py-2 ring-1 ring-inset ring-edge" data-draft-original>
<span class="text-xs font-semibold text-fg2">The message it answers — from an outside sender, untrusted (message ` +
		strconv.FormatInt(m.ID, 10) + `)</span>
<span class="break-all text-xs text-fg2">from ` + esc(m.Sender) + ` · subject ` + esc(m.Subject) + `</span>
<pre class="whitespace-pre-wrap break-words font-sans text-xs text-fg2" data-draft-original-body>` + esc(m.Body) + `</pre>
</div>
<div class="flex flex-col gap-1 rounded-md bg-bg px-3 py-2 ring-1 ring-inset ring-edge" data-draft-text data-runes="` + runes + `">
<span class="text-xs font-semibold text-fg">The reply · ` + runes + ` characters` + draftTextLead(d.Status) + `</span>
<code class="whitespace-pre-wrap break-all font-mono text-xs text-fg" data-draft-body>` + esc(flows.VisibleEscape(d.Body)) + `</code>
</div>
`)
	if d.ExternalRef != "" {
		b.WriteString(`<span class="break-all text-xs text-fg2" data-draft-ref>reference ` + esc(flows.VisibleEscape(d.ExternalRef)) + `</span>
`)
	}
	if d.Detail != "" {
		b.WriteString(`<span class="break-all text-xs text-fg2" data-draft-detail>the plugin said: ` + esc(flows.VisibleEscape(d.Detail)) + `</span>
`)
	}
	switch d.Status {
	case plugins.DraftPending:
		b.WriteString(`<form method="POST" action="` + pluginsPath + `/drafts/` + id + `/approve" data-step-up-form><button type="submit" class="` +
			flowsButtonClass + ` w-full" data-draft-approve>Approve and send</button></form>
<form method="POST" action="` + pluginsPath + `/drafts/` + id + `/reject"><button type="submit" class="` + flowsQuietButton + `" data-draft-reject>Reject</button></form>
`)
	case plugins.DraftUnconfirmed:
		b.WriteString(`<form method="POST" action="` + pluginsPath + `/drafts/` + id + `/reapprove" data-step-up-form><button type="submit" class="` +
			flowsQuietButton + `" data-draft-reapprove>Approve again (it may be sent twice)</button></form>
`)
	}
	b.WriteString("</li>\n")
	return b.String()
}

// draftTextLead says what the reply text is FOR, by state.
func draftTextLead(st plugins.DraftStatus) string {
	switch st {
	case plugins.DraftPending:
		return " · approving sends exactly this, once"
	case plugins.DraftUnconfirmed:
		return " · approving again lets the plugin send exactly this once more"
	}
	return ""
}

// draftPageScript makes an Approve on a stale sign-in ask "Confirm it's you"
// BEFORE it submits, and submit once confirmed. It is a convenience: the server
// refuses a stale approval whatever this script did (decideDraft).
const draftPageScript = `<script>
(function () {
  document.querySelectorAll('form[data-step-up-form]').forEach(function (f) {
    f.addEventListener('submit', function (ev) {
      var su = window.musterStepUp;
      if (!su || su.fresh()) return;
      ev.preventDefault();
      su.ask('', function () { f.submit(); });
    });
  });
})();
</script>`

// draftStepUpHTML is the confirm dialog, its script and the page's own, as HTML.
func (s *Server) draftStepUpHTML(r *http.Request) string {
	v := s.stepUpView(r, "")
	v.Why = DraftStepUpWhy()
	var b strings.Builder
	if err := ui.StepUpDialog(v).Render(&b); err != nil {
		s.logger.Printf("plugins: render the confirm dialog: %v", err)
		return ""
	}
	return b.String() + draftPageScript
}
