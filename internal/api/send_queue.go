package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"github.com/ZacxDev/muster/internal/plugins"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE SEND QUEUE's server half (internal/sendqueue, migration 0019): the
// Rendered view's Queue, the queue's list, Cancel, Retry and Delete, and the dispatcher's two
// hooks — the target's state (sendQueueProbe) and the delivery
// (sendQueueSend).
//
// A queue target is a MIRRORED Claude Code window, and only its Rendered view
// has Queue: a queued message is pasted into the window's Claude Code pane
// (mirror.PasteClaude) — the pane whose idle the queue waited on — never into
// whatever else is active in the window.
//
// 🔴 A QUEUED MESSAGE IS DELIVERED THROUGH THE SAME PATH AS A SEND, WITH THE
// SAME GATES, AS ITS OWNER. The delivery runs mirrorPaste — the function a
// person's Send runs — behind the same checks in the same order (the host is
// the owner's, -interactive, a fresh sign-in), under the owner's own principal:
// the dispatcher hands its hooks ctx through queuedPrincipal (authz.ViaQueue),
// so row-level security and the plugin's own gates see the owner and nobody
// else.
//
// 🔴 THE STEP-UP IS DECIDED WHEN THE MESSAGE IS QUEUED, AND THAT DECISION
// HOLDS FOR AT MOST sendqueue.AuthorizedFor (7 days) FROM THE QUEUEING — the
// operator's decision. Queueing needs the same fresh sign-in a Send needs (403
// + X-Muster-Step-Up, or the plain confirm page), and the queue stores the
// sign-in time of the session that queued it (stepup_at). The delivery checks
// that the recorded sign-in was inside the step-up window (MUSTER_STEPUP_WINDOW)
// when the row was created and that the row is no older than AuthorizedFor
// (sendQueueAuthorization); only then does it present the delivery moment to
// the paste's own freshness gate, as a routed message's delivery does. A
// message past either FAILS with a reason, pasting nothing, and its row offers
// Retry, which queues the same text again under the retrying request's own
// fresh sign-in (handleSendQueueRetry). It used to present the queueing time
// itself to the 12 h gate, so a message that waited out a long turn overnight
// failed though it was authorized when it was queued.
//
// 🔴 AND THE ACCOUNT IS RE-CHECKED WHEN IT IS SENT, FROM THE DATABASE. The
// delivery refuses — fails, saying why, pasting nothing — when the owner is
// disabled, or the recorded sign-in is at or before the account's session
// cutoff (users.sessions_not_before: every disable and password reset moves
// it) or its latest sign-out everywhere (users.signed_out_at, which the
// logout handler stamps: signedOutEverywhere). That check is the guarantee: it
// holds on every pod and across a restart, for a message queued by a request
// that committed after the sign-out, and when cancelling failed. A disable and
// a sign-out everywhere also cancel the owner's queued messages
// (cancelQueuedFor), so the list says so at once. A message already being sent
// when the account changed is past stopping.
//
// 🔴 UNKNOWN IS NOT IDLE (fail closed). For a mirror host whose muster-mirror
// does not report Claude Code's status, or a window that is not Claude Code,
// Queue is drawn disabled saying why and a queue post is refused saying why.
// Whatever is already queued for a target the server cannot read — including a
// host that reports a status but not since when — is held, the list saying
// why; never sent on a guess.
// ---------------------------------------------------------------------------

func (s *Server) registerSendQueueRoutes(mux Mux) {
	// Spelled literally (ui.SendQueuePath is the same string): the route walks
	// in this package's tests read the pattern from the source.
	mux.HandleFunc("POST /send-queue", s.requireOperatorWrite(s.handleSendQueueAdd))
	mux.HandleFunc("GET /send-queue", s.requireSession(s.handleSendQueueList))
	mux.HandleFunc("POST /send-queue/{id}/cancel", s.requireOperatorWrite(s.handleSendQueueCancel))
	mux.HandleFunc("POST /send-queue/{id}/delete", s.requireOperatorWrite(s.handleSendQueueDelete))
	// The plain confirm page's form: a stale queue the BROWSER submitted.
	mux.HandleFunc("POST "+sendQueueConfirmPath, s.requireOperatorWrite(s.handleSendQueueConfirm))
	mux.HandleFunc("POST /send-queue/{id}/retry", s.requireOperatorWrite(s.handleSendQueueRetry))
	// The plain confirm page's form for a stale Retry the BROWSER submitted.
	mux.HandleFunc("POST /send-queue/{id}/retry/confirm", s.requireOperatorWrite(s.handleSendQueueRetryConfirm))
	mux.HandleFunc("POST /send-queue/{id}/send-now", s.requireOperatorWrite(s.handleSendQueueSendNow))
	// The plain confirm page's form for a stale Send now the BROWSER submitted.
	mux.HandleFunc("POST /send-queue/{id}/send-now/confirm", s.requireOperatorWrite(s.handleSendQueueSendNowConfirm))
	s.registerSendQueueEditRoutes(mux)
}

// sendQueueConfirmPath is the plain confirm page's route for a queue post.
const sendQueueConfirmPath = "/send-queue/confirm"

// SendQueueNotClaude is why a mirrored window whose pane is not Claude Code
// cannot be queued for.
const SendQueueNotClaude = "messages can be queued only for a Claude Code session: muster can tell when one is idle, and not when a shell is"

// sendQueueTarget is a queue form's or query's target, resolved for the
// request's principal: the target, the Rendered view it was queued from, and
// the host and entry. ok is false when it answered already: 404 for anything
// that is not the principal's (never a 403 that would say "this exists").
type sendQueueTarget struct {
	t    sendqueue.Target
	back string
	h    mirror.HostView
	e    mirror.SessionView
	// listed: an entry the host lists now.
	listed bool
}

func (s *Server) resolveSendQueueTarget(w http.ResponseWriter, r *http.Request, v url.Values) (sendQueueTarget, bool) {
	var q sendQueueTarget
	if s.ext.SendQueue == nil {
		http.NotFound(w, r)
		return q, false
	}
	h, e, listed, _, ok := s.mirrorEntry(r, v.Get("host"), v.Get("session"), v.Get("window"))
	if !ok {
		http.NotFound(w, r)
		return q, false
	}
	q.h, q.e, q.listed = h, e, listed
	q.t = sendqueue.Target{Host: h.Host, Session: e.Session, Window: e.Window}
	q.back = mirrorRef(e).ViewPath()
	return q, true
}

// mirrorQueueOff is why a mirrored entry cannot be queued for, "" when it can:
// the host must report the agent's status, and the window must run Claude
// Code — or Hermes, on a host that advertised hermesSend (Hermes Phase 3;
// the delivery then goes through the hub's idle gate and the host's gated
// send).
func mirrorQueueOff(h mirror.HostView, e mirror.SessionView) string {
	switch {
	case !h.Online:
		return unprefixed(mirror.ErrHostOffline)
	case !h.StatusReport || h.Windows == nil:
		return unprefixed(mirror.ErrNoStatusReport)
	case e.HermesView() && h.HermesSend:
		return ""
	case e.HermesView():
		return unprefixed(mirror.ErrHermesSendOff)
	case !e.Claude:
		return SendQueueNotClaude
	}
	return ""
}

// handleSendQueueAdd is POST /send-queue (the window's address, text): queue a
// message. To the composer's script 204 or a status and the reason; to a form
// the browser submitted, a redirect back with a notice.
func (s *Server) handleSendQueueAdd(w http.ResponseWriter, r *http.Request) {
	s.sendQueueAdd(w, r, r.Context(), false)
}

// sendQueueAdd queues the form's message. ctx carries the sign-in time the
// step-up gate reads (the request's, or one the confirm page just minted);
// confirmed says it is the confirm page's post, which is always a browser
// navigation.
func (s *Server) sendQueueAdd(w http.ResponseWriter, r *http.Request, ctx context.Context, confirmed bool) {
	if s.ext.SendQueue == nil {
		http.NotFound(w, r)
		return
	}
	if !confirmed {
		r.Body = http.MaxBytesReader(w, r.Body, mirror.MaxPaste*4)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "the message is too large", http.StatusRequestEntityTooLarge)
			return
		}
	}
	q, ok := s.resolveSendQueueTarget(w, r, r.PostForm)
	if !ok {
		return
	}
	text := r.PostFormValue("text")
	nav := confirmed || isDocumentNavigation(r)
	refuse := func(status int, msg, notice string) {
		if nav {
			http.Redirect(w, r, withNotice(q.back, notice), http.StatusSeeOther)
			return
		}
		http.Error(w, msg, status)
	}
	if strings.TrimSpace(text) == "" {
		refuse(http.StatusBadRequest, MirrorSendEmpty, noticeEmpty)
		return
	}
	if g := s.sendQueueGate(ctx, q); g.status != 0 {
		switch {
		case !g.stale:
			refuse(g.status, g.msg, g.notice)
		case nav:
			s.sendQueueStepUpPage(w, sendQueueConfirmPath, sendQueueAddFields(q, text), q.back, "")
		default:
			refuseStale(w)
		}
		return
	}
	if _, err := mirror.SanitizePaste(text); err != nil {
		refuse(http.StatusBadRequest, err.Error(), noticeBadText)
		return
	}
	it := sendqueue.Item{Target: q.t, Body: text}
	it.StepUpAt, _ = stepup.AuthTime(ctx)
	// A paste the hub remembers (a Send just now) holds this message too, and
	// is stored with it so that a restart does not forget it.
	it.AfterSince = s.ext.Mirror.AgentState(ctx, q.t.Host, q.t.Session, q.t.Window).Pasted
	stored, err := s.ext.SendQueue.Enqueue(ctx, it)
	switch {
	case errors.Is(err, sendqueue.ErrFull):
		refuse(http.StatusConflict, strings.TrimPrefix(err.Error(), "sendqueue: "), noticeQueueFull)
		return
	case err != nil:
		s.logger.Printf("sendqueue: queue for %s: %v", q.t, err)
		refuse(http.StatusInternalServerError, "could not queue the message", noticeFailed)
		return
	}
	s.logger.Printf("sendqueue: queued message %d (%d bytes) for %s", stored.ID, len(stored.Body), q.t)
	// The operator wrote to the session — when they QUEUED it, not when it is
	// pasted: a session blocked in attention_wait is busy, and a queued message
	// waits for idle, so it would never be pasted while the ask held it.
	s.mirrorAddressed(ctx, q.h.Host, q.e, mirror.PasteClaude, text, stored.ID)
	s.kickSendQueue()
	if nav {
		http.Redirect(w, r, withNotice(q.back, noticeQueued), http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sendQueueGateResult is why a queue (or a Retry) may not be queued for its
// target now: a status, the reason and the notice a redirect carries — or,
// stale, that the sign-in must be confirmed first. status 0: it may.
type sendQueueGateResult struct {
	status      int
	msg, notice string
	stale       bool
}

// sendQueueGate is the one gate a queue post and a Retry both pass: THE SAME
// GATES AS A SEND, IN mirrorSend's ORDER — an -interactive host, a FRESH
// sign-in on ctx — then the queue's own: the window is listed, and the host
// can tell when Claude Code in it is idle.
func (s *Server) sendQueueGate(ctx context.Context, q sendQueueTarget) sendQueueGateResult {
	switch {
	case !q.h.Interactive:
		return sendQueueGateResult{status: http.StatusConflict, msg: mirror.ErrInteractiveRefused.Error(), notice: noticeReadOnly}
	case !stepup.Fresh(ctx, s.stepUpNow()):
		return sendQueueGateResult{status: http.StatusForbidden, stale: true}
	case !q.listed:
		return sendQueueGateResult{status: http.StatusConflict, msg: mirror.ErrWindowGone.Error(), notice: noticeWindowGone}
	}
	if off := mirrorQueueOff(q.h, q.e); off != "" {
		return sendQueueGateResult{status: http.StatusConflict, msg: off, notice: noticeQueueOff}
	}
	return sendQueueGateResult{}
}

// sendQueueAddFields is a queue post's message, as the confirm page carries it.
func sendQueueAddFields(q sendQueueTarget, text string) []ui.ComposerField {
	fields := []ui.ComposerField{{Name: "host", Value: q.h.Host}, {Name: "session", Value: q.e.Session}}
	if q.e.Window != "" {
		fields = append(fields, ui.ComposerField{Name: "window", Value: q.e.Window})
	}
	return append(fields, ui.ComposerField{Name: "text", Value: text})
}

// sendQueueStepUpPage answers a browser-submitted queue (or Retry) whose
// sign-in is stale: the plain confirm page, carrying what was waiting in
// hidden fields, which posts to action.
func (s *Server) sendQueueStepUpPage(w http.ResponseWriter, action string, fields []ui.ComposerField, back, problem string) {
	formDocument(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.StepUpFormPage(ui.StepUpFormView{Action: action, Fields: fields,
		BackHref: back, Window: stepup.WindowText(), Error: problem}).Render(w); err != nil {
		s.logger.Printf("sendqueue: render confirm page: %v", err)
	}
}

// handleSendQueueConfirm is POST /send-queue/confirm: the plain confirm page's
// form — the password again, and the message that was waiting to be queued. A
// right password replaces the session with a freshly signed-in one (the SAME
// check and throttle as the dialog's: stepUpWithPassword) and the message is
// queued with THAT sign-in's time; a wrong one shows the page again.
func (s *Server) handleSendQueueConfirm(w http.ResponseWriter, r *http.Request) {
	if s.ext.SendQueue == nil || s.ext.Mirror == nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, mirror.MaxPaste*4)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "the message is too large", http.StatusRequestEntityTooLarge)
		return
	}
	q, ok := s.resolveSendQueueTarget(w, r, r.PostForm)
	if !ok {
		return
	}
	if !q.h.Interactive {
		http.Redirect(w, r, withNotice(q.back, noticeReadOnly), http.StatusSeeOther)
		return
	}
	// PostForm, never Form: a password in the URL would be in the access log.
	o := s.stepUpWithPassword(w, r, r.PostForm.Get("password"))
	if o.Error != "" {
		s.sendQueueStepUpPage(w, sendQueueConfirmPath, sendQueueAddFields(q, r.PostFormValue("text")), q.back, o.Error)
		return
	}
	s.sendQueueAdd(w, r, stepup.WithAuthTime(r.Context(), o.At), true)
}

// handleSendQueueList is GET /send-queue (the window's address): the window's
// queue as an HTML fragment (ui.SendQueueList), which the composer's script
// swaps into #mv-queue.
func (s *Server) handleSendQueueList(w http.ResponseWriter, r *http.Request) {
	q, ok := s.resolveSendQueueTarget(w, r, r.URL.Query())
	if !ok {
		return
	}
	noStore(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.SendQueueList(s.sendQueueListView(r.Context(), q.t, sendQueueEditing(r))).Render(w); err != nil {
		s.logger.Printf("sendqueue: render list: %v", err)
	}
}

// sendQueuePreview bounds how much of a message the list shows.
const sendQueuePreview = 200

// sendQueueEditRef is the message a page asks to draw open for editing
// (?edit=<id>, ui.SendQueueEditParam) and the hold ITS Edit was given
// (&hold=<token>, ui.SendQueueHoldParam); id 0 for none.
type sendQueueEditRef struct {
	id   int64
	hold string
}

// sendQueueEditing is r's edit reference.
//
// 🔴 THE FORM IS DRAWN ONLY UNDER THE HOLD THIS DEVICE'S EDIT WAS GIVEN, never
// under whatever hold the row has now (sendQueueListView): a reload on a device
// whose edit another device took over would otherwise carry the OTHER device's
// hold, and its stale Save would win.
func sendQueueEditing(r *http.Request) sendQueueEditRef {
	q := r.URL.Query()
	id, err := strconv.ParseInt(q.Get(ui.SendQueueEditParam), 10, 64)
	if err != nil || id <= 0 {
		return sendQueueEditRef{}
	}
	return sendQueueEditRef{id: id, hold: q.Get(ui.SendQueueHoldParam)}
}

// sendQueueListView is t's queue as the composer draws it, with what the next
// message is waiting for — all read as ctx's principal: the REQUEST's own,
// never one minted here (only the dispatcher acts as a message's owner).
// editing is the row to draw as its edit form (id 0: none): only a row whose
// LIVE hold is the one editing names is drawn so; a held row named under any
// other hold is drawn as being edited on another device.
func (s *Server) sendQueueListView(ctx context.Context, t sendqueue.Target, editing sendQueueEditRef) ui.SendQueueListView {
	items, err := s.ext.SendQueue.List(ctx, t)
	if err != nil {
		s.logger.Printf("sendqueue: list for %s: %v", t, err)
		return ui.SendQueueListView{Error: "The queue could not be read."}
	}
	var v ui.SendQueueListView
	answered := s.queueAnswered(ctx, items)
	for _, it := range items {
		preview := it.Body
		if len(preview) > sendQueuePreview {
			// A cut through a character leaves invalid bytes; they are dropped.
			preview = strings.ToValidUTF8(preview[:sendQueuePreview], "") + "…"
		}
		body := ""
		if it.State == sendqueue.Queued {
			body = it.Body // Copy message copies all of it, not the preview.
		}
		row := ui.SendQueueItem{ID: it.ID, State: string(it.State), Preview: preview, Body: body,
			Detail: it.Detail, Cancellable: it.State == sendqueue.Queued, Answered: answered[it.ID],
			Retryable: sendQueueRetryable(it), Deletable: it.State.Finished(), MayHaveDelivered: it.MayHaveDelivered,
			Editable: it.State == sendqueue.Queued && it.RouteID == "", Held: it.State == sendqueue.Queued && it.Held,
			HoldLapsed: it.State == sendqueue.Queued && it.HoldLapsed, Edited: !it.EditedAt.IsZero()}
		if row.Held && it.ID == editing.id {
			if editing.hold != "" && subtle.ConstantTimeCompare([]byte(editing.hold), []byte(it.HoldToken)) == 1 {
				row.Editing, row.HoldToken = true, it.HoldToken
				row.HoldLeftMS = it.HeldUntil.Sub(s.sendQueueNow()).Milliseconds()
			} else {
				row.HeldElsewhere = true
			}
		}
		v.Items = append(v.Items, row)
	}
	if h, ok, err := s.ext.SendQueue.HeadOf(ctx, t); err == nil && ok {
		if d := s.sendQueueDecide(h, s.sendQueueProbe(ctx, h)); !d.Send {
			v.Waiting = d.Why
		} else {
			v.Waiting = "sending shortly"
		}
	}
	return v
}

// queueAnswered is, for each of items that answered a question in its session
// when it was queued (attention.go's mirrorAddressed), the asker's label.
func (s *Server) queueAnswered(ctx context.Context, items []sendqueue.Item) map[int64]string {
	if s.ext.Attention == nil || len(items) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	out, err := s.ext.Attention.AnsweredByQueue(ctx, ids)
	if err != nil {
		s.logger.Printf("sendqueue: which messages answered a question: %v", err)
		return nil
	}
	return out
}

// handleSendQueueCancel is POST /send-queue/{id}/cancel: cancel one of the
// principal's queued messages. 404 for one it cannot see; 409 for one already
// being sent or finished.
func (s *Server) handleSendQueueCancel(w http.ResponseWriter, r *http.Request) {
	if s.ext.SendQueue == nil {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	it, err := s.ext.SendQueue.Get(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	back := ui.MirrorRef{Host: it.Target.Host, Session: it.Target.Session, Window: it.Target.Window}.ViewPath()
	nav := isDocumentNavigation(r)
	if _, err := s.ext.SendQueue.Cancel(ctx, id, s.sendQueueNow()); err != nil {
		if errors.Is(err, sendqueue.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if !errors.Is(err, sendqueue.ErrNotCancellable) {
			s.logger.Printf("sendqueue: cancel %d: %v", id, err)
		}
		if nav {
			http.Redirect(w, r, withNotice(back, noticeNotCancelled), http.StatusSeeOther)
			return
		}
		http.Error(w, composerNotices[noticeNotCancelled].text, http.StatusConflict)
		return
	}
	s.logger.Printf("sendqueue: cancelled message %d for %s", id, it.Target)
	// 🔴 A CANCEL DOES NOT UN-SEND AN ANSWER: a queued message answers its
	// session's asks when it is queued, and that answer is already delivered.
	if asker, ok := s.queueAnswered(ctx, []sendqueue.Item{it})[id]; ok {
		if nav {
			http.Redirect(w, r, withNotice(back, noticeCancelledAnswered), http.StatusSeeOther)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(SendQueueCancelledAnsweredText(asker)))
		return
	}
	if nav {
		http.Redirect(w, r, withNotice(back, noticeCancelled), http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SendQueueNotDeleted is why a Delete was refused: the message is not
// finished.
const SendQueueNotDeleted = "Not deleted: that message is still queued (cancel it first) or is being sent."

// handleSendQueueDelete is POST /send-queue/{id}/delete: delete one of the
// principal's FINISHED messages (sent, failed — expired included — or
// cancelled; sendqueue.State.Finished), so it leaves the list on every device.
// 404 for one it cannot see; 409 for one still queued (Cancel it) or being
// sent, which is never deleted from under its claim. To the list's script 204;
// to a form the browser submitted, a redirect back with a notice.
func (s *Server) handleSendQueueDelete(w http.ResponseWriter, r *http.Request) {
	if s.ext.SendQueue == nil {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	it, err := s.ext.SendQueue.Get(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	back := ui.MirrorRef{Host: it.Target.Host, Session: it.Target.Session, Window: it.Target.Window}.ViewPath()
	nav := isDocumentNavigation(r)
	switch err := s.ext.SendQueue.Delete(ctx, id); {
	case errors.Is(err, sendqueue.ErrNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		if !errors.Is(err, sendqueue.ErrNotDeletable) {
			s.logger.Printf("sendqueue: delete %d: %v", id, err)
		}
		if nav {
			http.Redirect(w, r, withNotice(back, noticeNotDeleted), http.StatusSeeOther)
			return
		}
		http.Error(w, SendQueueNotDeleted, http.StatusConflict)
		return
	}
	s.logger.Printf("sendqueue: deleted finished message %d (%s) for %s", id, it.State, it.Target)
	if nav {
		http.Redirect(w, r, withNotice(back, noticeDeleted), http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sendQueueRetryable is whether it's row offers Retry: it failed, has not
// been retried yet, and is a person's own — never a ROUTED row, whose failed
// route goes back to the chief (sendqueue.PGStore.Retry refuses it otherwise).
func sendQueueRetryable(it sendqueue.Item) bool {
	return it.State == sendqueue.Failed && it.RetriedAs == 0 && it.RouteID == ""
}

// SendQueueRetryConfirmField is the form field that says the person confirmed
// retrying a message that may already have been sent.
const SendQueueRetryConfirmField = "confirm"

// SendQueueMayHaveSent is what Retry on a message that may already have been
// sent asks before it queues it again.
const SendQueueMayHaveSent = "This message may already have been sent: muster stopped while sending it. Queue it again anyway?"

// handleSendQueueRetry is POST /send-queue/{id}/retry: queue the principal's
// FAILED message again — the same text, the same window — under the
// request's OWN sign-in, which must be fresh like a queue post's
// (sendQueueGate: 403 + X-Muster-Step-Up to the script, the plain confirm page
// to a browser form). A message that may already have been sent (muster died
// mid-send) needs one confirm first (SendQueueRetryConfirmField): 409 saying so
// to the script, a confirm page to a browser form. 404 for a message the
// principal cannot see; 409 for one that did not fail or was already retried.
func (s *Server) handleSendQueueRetry(w http.ResponseWriter, r *http.Request) {
	s.sendQueueRetry(w, r, r.Context(), false)
}

// handleSendQueueRetryConfirm is POST /send-queue/{id}/retry/confirm: the
// plain confirm page's form for a stale Retry — the password again. A right
// one replaces the session with a freshly signed-in one (stepUpWithPassword,
// as the queue's own confirm page) and the Retry is made under THAT sign-in.
func (s *Server) handleSendQueueRetryConfirm(w http.ResponseWriter, r *http.Request) {
	if s.ext.SendQueue == nil || s.ext.Mirror == nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "the form is too large", http.StatusRequestEntityTooLarge)
		return
	}
	id, _, q, ok := s.sendQueueRowTarget(w, r)
	if !ok {
		return
	}
	// PostForm, never Form: a password in the URL would be in the access log.
	o := s.stepUpWithPassword(w, r, r.PostForm.Get("password"))
	if o.Error != "" {
		s.sendQueueStepUpPage(w, sendQueueRetryConfirmPath(id), sendQueueRetryFields(r), q.back, o.Error)
		return
	}
	s.sendQueueRetry(w, r, stepup.WithAuthTime(r.Context(), o.At), true)
}

func sendQueueRetryConfirmPath(id int64) string { return ui.SendQueueRetryPath(id) + "/confirm" }

// sendQueueRetryFields is what a Retry's confirm page carries: the
// may-have-been-sent confirm, when it was given.
func sendQueueRetryFields(r *http.Request) []ui.ComposerField {
	if r.PostFormValue(SendQueueRetryConfirmField) == "1" {
		return []ui.ComposerField{{Name: SendQueueRetryConfirmField, Value: "1"}}
	}
	return nil
}

// sendQueueRowTarget is the {id} of a Retry or Send now route: the
// principal's message and its window, resolved for the principal like a queue
// post's. ok is false
// when it answered already (404: not the principal's, or no such window).
func (s *Server) sendQueueRowTarget(w http.ResponseWriter, r *http.Request) (int64, sendqueue.Item, sendQueueTarget, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return 0, sendqueue.Item{}, sendQueueTarget{}, false
	}
	it, err := s.ext.SendQueue.Get(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return 0, sendqueue.Item{}, sendQueueTarget{}, false
	}
	v := url.Values{"host": {it.Target.Host}, "session": {it.Target.Session}}
	if it.Target.Window != "" {
		v.Set("window", it.Target.Window)
	}
	q, ok := s.resolveSendQueueTarget(w, r, v)
	return id, it, q, ok
}

// sendQueueRetry retries the route's message. ctx carries the sign-in time the
// gate reads (the request's, or one the confirm page just minted); confirmed
// says it is the confirm page's post, which is always a browser navigation.
func (s *Server) sendQueueRetry(w http.ResponseWriter, r *http.Request, ctx context.Context, confirmed bool) {
	if s.ext.SendQueue == nil {
		http.NotFound(w, r)
		return
	}
	if !confirmed {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "the form is too large", http.StatusRequestEntityTooLarge)
			return
		}
	}
	id, it, q, ok := s.sendQueueRowTarget(w, r)
	if !ok {
		return
	}
	nav := confirmed || isDocumentNavigation(r)
	refuse := func(status int, msg, notice string) {
		if nav {
			http.Redirect(w, r, withNotice(q.back, notice), http.StatusSeeOther)
			return
		}
		http.Error(w, msg, status)
	}
	if !sendQueueRetryable(it) {
		n := noticeNotRetried
		if it.RouteID != "" {
			n = noticeRoutedNotRetried
		}
		refuse(http.StatusConflict, composerNotices[n].text, n)
		return
	}
	// 🔴 ONE CONFIRM FOR A MESSAGE THAT MAY ALREADY HAVE BEEN SENT: a second
	// paste of it is the failure the queue otherwise never risks.
	if it.MayHaveDelivered && r.PostFormValue(SendQueueRetryConfirmField) != "1" {
		if nav {
			formDocument(w)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if err := ui.SendQueueRetryConfirmPage(ui.SendQueueRetryPath(id), SendQueueRetryConfirmField,
				SendQueueMayHaveSent, q.back).Render(w); err != nil {
				s.logger.Printf("sendqueue: render retry confirm page: %v", err)
			}
			return
		}
		w.Header().Set("X-Muster-Confirm", "may-have-sent")
		http.Error(w, SendQueueMayHaveSent, http.StatusConflict)
		return
	}
	if g := s.sendQueueGate(ctx, q); g.status != 0 {
		switch {
		case !g.stale:
			refuse(g.status, g.msg, g.notice)
		case nav:
			s.sendQueueStepUpPage(w, sendQueueRetryConfirmPath(id), sendQueueRetryFields(r), q.back, "")
		default:
			refuseStale(w)
		}
		return
	}
	at, _ := stepup.AuthTime(ctx)
	stored, err := s.ext.SendQueue.Retry(ctx, id, at, s.ext.Mirror.AgentState(ctx, q.t.Host, q.t.Session, q.t.Window).Pasted)
	switch {
	case errors.Is(err, sendqueue.ErrNotFound):
		http.NotFound(w, r)
		return
	case errors.Is(err, sendqueue.ErrNotRetryable):
		refuse(http.StatusConflict, composerNotices[noticeNotRetried].text, noticeNotRetried)
		return
	case errors.Is(err, sendqueue.ErrFull):
		refuse(http.StatusConflict, strings.TrimPrefix(err.Error(), "sendqueue: "), noticeQueueFull)
		return
	case err != nil:
		s.logger.Printf("sendqueue: retry %d for %s: %v", id, q.t, err)
		refuse(http.StatusInternalServerError, "could not queue the message again", noticeFailed)
		return
	}
	s.logger.Printf("sendqueue: retried message %d as %d (%d bytes) for %s (may have been delivered: %t)",
		id, stored.ID, len(stored.Body), q.t, it.MayHaveDelivered)
	s.mirrorAddressed(ctx, q.h.Host, q.e, mirror.PasteClaude, stored.Body, stored.ID)
	s.kickSendQueue()
	if nav {
		http.Redirect(w, r, withNotice(q.back, noticeQueued), http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// mirrorComposerQueue is the Queue of a mirrored entry's Rendered view, nil
// when there is no queue to draw.
//
// editing is the row a no-script Edit came back to draw open (?edit=<id>).
func (s *Server) mirrorComposerQueue(ctx context.Context, h mirror.HostView, e mirror.SessionView, editing sendQueueEditRef) *ui.ComposerQueue {
	if s.ext.SendQueue == nil {
		return nil
	}
	ref := mirrorRef(e)
	q := url.Values{"host": {ref.Host}, "session": {ref.Session}}
	if ref.Window != "" {
		q.Set("window", ref.Window)
	}
	t := sendqueue.Target{Host: h.Host, Session: e.Session, Window: e.Window}
	return &ui.ComposerQueue{Src: ui.SendQueuePath + "?" + q.Encode(),
		List: s.sendQueueListView(ctx, t, editing), Disabled: mirrorQueueOff(h, e)}
}

// ---------------------------------------------------------------------------
// The dispatcher's hooks.
// ---------------------------------------------------------------------------

// queuedPrincipal is ctx acting as owner, through authz.ViaQueue: how a queued
// message is probed and delivered — as the user who queued it, never as the
// system. Its one caller is the dispatcher (RunSendQueue hands it over as the
// Dispatcher's AsOwner), which read the row, and so its owner, under the system
// scope; nothing else it read is handed on. internal/authz's
// withPrincipalLedger lists it.
func (s *Server) queuedPrincipal(ctx context.Context, owner int64) context.Context {
	return authz.WithPrincipal(ctx, s.principalFor(ctx, owner, authz.ViaQueue))
}

// sendQueueNow is the queue's clock (a test's fake one, else the step-up's).
func (s *Server) sendQueueNow() time.Time {
	if s.sendQueueClock != nil {
		return s.sendQueueClock()
	}
	return time.Now()
}

// sendQueueIdle is how long a window must have been idle (MUSTER_SEND_QUEUE_IDLE).
func (s *Server) sendQueueIdle() time.Duration {
	if s.ext.SendQueueIdle > 0 {
		return s.ext.SendQueueIdle
	}
	return sendqueue.DefaultIdleFor
}

// sendQueueDecide is sendqueue.Decide with this server's idle time and clock.
func (s *Server) sendQueueDecide(h sendqueue.Head, st sendqueue.AgentState) sendqueue.Decision {
	return sendqueue.Decide(h, st, s.sendQueueNow(), s.sendQueueIdle())
}

// sendQueueProbe is what ctx's principal can see of h's window now: idle, and
// since when (Claude Code's own statusUpdatedAt, as the host reports it), or
// why not.
func (s *Server) sendQueueProbe(ctx context.Context, h sendqueue.Head) sendqueue.AgentState {
	if s.ext.Mirror == nil {
		return sendqueue.AgentState{Why: "mirrored sessions are off on this server"}
	}
	st := s.ext.Mirror.AgentState(ctx, h.Target.Host, h.Target.Session, h.Target.Window)
	switch {
	case st.GuardHeld != "":
		// The host's Claude prompt guard refused the last paste on what it
		// read at the moment of typing; held, with its words, and backing off.
		return sendqueue.AgentState{Why: st.GuardHeld}
	case st.Unknown != nil:
		return sendqueue.AgentState{Why: unprefixed(st.Unknown)}
	case st.Status == mirror.StatusIdle:
		return sendqueue.AgentState{Idle: true, Since: st.Since, Observed: st.Observed, Pasted: st.Pasted}
	case st.Runtime == mirror.RuntimeHermes && st.Status == mirror.StatusWaiting:
		return sendqueue.AgentState{Why: "Hermes is waiting for you (an approval or a question)."}
	case st.Runtime == mirror.RuntimeHermes:
		return sendqueue.AgentState{Why: "Hermes is working."}
	case st.Status == mirror.StatusWaiting:
		return sendqueue.AgentState{Why: "Claude Code is waiting for you (a prompt or a question)."}
	}
	return sendqueue.AgentState{Why: "Claude Code is working."}
}

// sendQueueNonce is the nonce a queued message is delivered with: one per
// row, so a delivery the send path has already seen is never made twice. It
// must match sendNoncePattern (16+ characters), or the send path would treat
// it as no nonce at all and claim nothing.
func sendQueueNonce(id int64) string { return fmt.Sprintf("send-queue-%020d", id) }

// sendQueueSend delivers one claimed message: mirrorSend's gates, then
// mirrorPaste into the window's Claude Code pane. ctx acts as the message's
// owner.
//
// 🔴 THE SIGN-IN TIME THE PASTE'S FRESHNESS CHECK SEES IS THE DELIVERY MOMENT,
// NOT THE ONE THE MESSAGE WAS QUEUED WITH — and only once the row is
// authorized by what actually gates it. The queued sign-in (StepUpAt) is read
// twice before that: against the owner's account cutoffs
// (sendQueueOwnerRefusal), and, for a person's message, against the row's own
// age (sendQueueAuthorization: fresh when queued, at most
// sendqueue.AuthorizedFor ago). A routed message is gated by its route instead
// (sendQueueRoutedGate). Only after the applicable gate passes is ctx's auth
// time moved to now; a row that fails either is refused before it is.
//
// 🔴 THE PASTE CARRIES THE TURN IT WAS DECIDED ON (st's Since and Pasted;
// mirror.ExpectTurn). A person's Send that lands between the dispatcher's
// decision and this paste moves the window's Pasted (or is still in flight),
// and the hub then refuses this paste under the same lock that records the
// Send — nothing reaches the host — so the message goes back to queued behind
// the Send's turn (Outcome.Superseded) instead of being pasted into it.
func (s *Server) sendQueueSend(ctx context.Context, it sendqueue.Item, st sendqueue.AgentState) sendqueue.Outcome {
	return s.sendQueueDeliver(ctx, it, mirror.ExpectTurn(mirror.Turn{Since: st.Since, Pasted: st.Pasted}))
}

// sendQueueDeliver is a queued message's delivery, as sendQueueSend describes:
// the owner's account, the row's own authorization (or its route), the host's
// gates, then mirrorPaste into the window's Claude Code pane with opts. The
// dispatcher's send passes the turn it decided on (mirror.ExpectTurn); a
// person's Send now passes none, so it is pasted like a Send — busy or not.
func (s *Server) sendQueueDeliver(ctx context.Context, it sendqueue.Item, opts ...mirror.PasteOption) sendqueue.Outcome {
	ctx = stepup.WithAuthTime(ctx, it.StepUpAt)
	if s.ext.Mirror == nil {
		return sendqueue.Outcome{Detail: "Not sent: mirrored sessions are off on this server."}
	}
	if why := s.sendQueueOwnerRefusal(ctx, it); why != "" {
		return sendqueue.Outcome{Detail: why}
	}
	if it.RouteID == "" {
		if why := sendQueueAuthorization(it, s.sendQueueNow()); why != "" {
			return sendqueue.Outcome{Detail: why}
		}
		// Authorized when it was queued, and still within AuthorizedFor of
		// that: the delivery moment is the auth time the paste's own
		// freshness check sees, as a routed message's is.
		ctx = stepup.WithAuthTime(ctx, s.stepUpNow())
	} else {
		why := s.sendQueueRoutedGate(ctx, it)
		if why != "" {
			return sendqueue.Outcome{Detail: why}
		}
		// The route gates it, not a sign-in (see session_routes.go): the
		// delivery moment is the auth time the paste's freshness check sees.
		ctx = stepup.WithAuthTime(ctx, s.stepUpNow())
	}
	var h mirror.HostView
	found := false
	for _, x := range s.ext.Mirror.Hosts(ctx) {
		if x.Host == it.Target.Host {
			h, found = x, true
			break
		}
	}
	if !found {
		return sendqueue.Outcome{Detail: "Not sent: " + unprefixed(mirror.ErrHostOffline)}
	}
	if !h.Interactive {
		return sendqueue.Outcome{Detail: "Not sent: " + unprefixed(mirror.ErrInteractiveRefused)}
	}
	if !stepup.Fresh(ctx, s.stepUpNow()) {
		return sendQueueStale()
	}
	repeat, err := s.mirrorPaste(ctx, it.Target.Host, it.Target.Session, it.Target.Window, it.Body, sendQueueNonce(it.ID), mirror.PasteClaude,
		opts...)
	var sup *mirror.SupersededError
	switch {
	case repeat:
		return sendqueue.Outcome{Sent: true, Detail: "Sent (already delivered once; not pasted again)."}
	case errors.Is(err, mirror.ErrStepUpRequired):
		return sendQueueStale()
	case errors.As(err, &sup):
		return sendqueue.Outcome{Superseded: true, AfterSince: sup.Pasted, Detail: unprefixed(err)}
	case err != nil:
		return sendQueueFailure(err)
	}
	return sendqueue.Outcome{Sent: true, Detail: "Sent."}
}

// sendQueueFailure is a failed delivery's outcome, in the composer's words.
// 🔴 A delivery that MAY have landed (the host never confirmed it; Hermes's
// gated send pressed Enter and saw no row) or that sits pasted in Hermes's
// box is NEVER "Not sent", which would invite a resend and a second turn.
func sendQueueFailure(err error) sendqueue.Outcome {
	may := errors.Is(err, mirror.ErrPasteUnconfirmed)
	var pr *mirror.PasteRefusedError
	if errors.As(err, &pr) {
		switch pr.Refusal {
		case mirror.PasteRefusalHermesUnconfirmed, mirror.PasteRefusalHermesPasted,
			mirror.PasteRefusalHermesNotSubmitted, mirror.PasteRefusalHermesSuperseded:
			// The host's gated send says it in full ("May have been
			// submitted: …", "Pasted, not submitted: …", "Not sent: …").
			return sendqueue.Outcome{MayHaveDelivered: may, Pasted: pr.Refusal == mirror.PasteRefusalHermesPasted, Detail: pr.Why}
		case mirror.PasteRefusalClaudeWaiting, mirror.PasteRefusalClaudeBox:
			// The Claude prompt guard, nothing typed, in its own words. A
			// queued message never gets here (the hub supersedes it, and it is
			// held); a Send now does, and fails.
			return sendqueue.Outcome{Detail: pr.Why}
		case mirror.PasteRefusalClaudePasted:
			// 🔴 Its text is in Claude Code's input box: it MAY be delivered
			// (with whatever is typed next), so it is never resent blindly —
			// its Retry asks first.
			return sendqueue.Outcome{Pasted: true, MayHaveDelivered: true, Detail: pr.Why}
		}
	}
	if may {
		return sendqueue.Outcome{MayHaveDelivered: true,
			Detail: "May have been sent: muster handed it to the host, which did not confirm it (" + unprefixed(err) + "). Check the conversation before sending again."}
	}
	return sendqueue.Outcome{Detail: "Not sent: " + unprefixed(err)}
}

// Why a routed queued message is not sent (sendQueueRoutedGate).
const (
	SendQueueRouteClosed  = "Not sent: its route is no longer waiting for an answer"
	SendQueueRouteMoved   = "Not sent: the window no longer holds the conversation the message was routed to; it went back to the chief."
	SendQueueRouteUnknown = "Not sent: its route could not be read."
)

// sendQueueRoutedGate is why routed row it may not be pasted now, "" when it
// may: its route (send_queue.route_id) must still be `routed` — so the row
// never outlives its route, whether or not its cancel happened — and the
// window must still hold the Claude session the route PINNED (the route's
// target key). A window that moved on fails the route and hands it back to
// the chief. ctx is the row's owner.
func (s *Server) sendQueueRoutedGate(ctx context.Context, it sendqueue.Item) string {
	if s.ext.Routes == nil {
		return SendQueueRouteUnknown
	}
	r, err := s.ext.Routes.Route(ctx, it.RouteID)
	switch {
	case err != nil:
		s.logger.Printf("sendqueue: message %d: read route %s: %v", it.ID, it.RouteID, err)
		return SendQueueRouteUnknown
	case r.State != plugins.RouteRouted:
		return SendQueueRouteClosed + " (" + string(r.State) + ")."
	}
	holds := ""
	for _, e := range s.mirrorClaudeEntries(ctx) {
		if e.Host == it.Target.Host && e.Session == it.Target.Session && e.Window == it.Target.Window {
			holds = e.ClaudeSession
		}
	}
	if holds == r.TargetKey {
		return ""
	}
	why := "the window no longer holds the Claude session it was routed to"
	if _, err := s.ext.Routes.FailRoute(context.WithoutCancel(ctx), r.RouteID, why, why); err != nil {
		s.logger.Printf("routes: %s: fail on a moved window: %v", r.RouteID, err)
		return SendQueueRouteMoved
	}
	// The failure owes the chief a hand-back, which the route sweep delivers.
	s.logger.Printf("routes: %s: %s (it holds %q)", r.RouteID, why, holds)
	s.kickRouteSweep()
	return SendQueueRouteMoved
}

// Why a queued message is not sent because of its owner's account
// (sendQueueOwnerRefusal), and why it was cancelled (cancelQueuedFor).
const (
	SendQueueOwnerDisabled  = "Not sent: the account that queued it is disabled."
	SendQueueOwnerCutoff    = "Not sent: the account that queued it was signed out of every session (a disable, a password reset or a sign-out everywhere) after it was queued. Queue it again."
	SendQueueOwnerUnchecked = "Not sent: the account that queued it could not be checked. Queue it again."
	SendQueueCancelDisabled = "Cancelled: the account was disabled."
	SendQueueCancelSignOut  = "Cancelled: signed out everywhere."
)

// sendQueueOwnerRefusal is why it's owner can no longer send it, "" when they
// can: the owner is disabled, or the sign-in that queued it is at or before
// the account's session cutoff or its latest sign-out everywhere — the same
// "at or before, in whole seconds" rule the session tier applies to a token
// (issuedBeforeCutoff). It fails closed: an account that cannot be read sends
// nothing. Without accounts (the operator password) there is nobody to
// disable.
func (s *Server) sendQueueOwnerRefusal(ctx context.Context, it sendqueue.Item) string {
	if s.gt == nil || s.gt.Users == nil {
		return ""
	}
	u, err := s.gt.Users.ByID(ctx, it.Owner)
	switch {
	case err != nil:
		s.logger.Printf("sendqueue: message %d: read owner %d: %v", it.ID, it.Owner, err)
		return SendQueueOwnerUnchecked
	case u.Disabled():
		return SendQueueOwnerDisabled
	// StepUpAt is GoTrue's clock (the signed amr time) and both cutoffs are
	// Postgres's: a GoTrue clock AHEAD lets a sign-in from just before the
	// cutoff through by the skew, one BEHIND refuses one just after it.
	// Accepted: the session tier compares GoTrue's iat with the same cutoff
	// the same way, and amr is GoTrue-signed, so no client can move it.
	case atOrBefore(it.StepUpAt, u.SessionsNotBefore), atOrBefore(it.StepUpAt, u.SignedOutAt):
		return SendQueueOwnerCutoff
	// An edited message: the sign-in that SAVED its text is held to the same
	// cutoffs (it can be another device's, older than the one that queued it).
	case !it.EditStepUpAt.IsZero() && (atOrBefore(it.EditStepUpAt, u.SessionsNotBefore) || atOrBefore(it.EditStepUpAt, u.SignedOutAt)):
		return SendQueueOwnerCutoff
	}
	return ""
}

// atOrBefore is whether at is at or before cut, in whole seconds; false for
// no cut.
func atOrBefore(at time.Time, cut *time.Time) bool {
	return cut != nil && at.Unix() <= cut.Unix()
}

// signedOutEverywhere is what a sign-out of every session of user uid does to
// the queue: it records the sign-out durably (users.signed_out_at), which the
// delivery checks, and cancels the queued messages so the list says so. The
// record is the guarantee and the cancel the courtesy: a failed cancel leaves
// rows queued that the delivery refuses (both are logged).
//
// It returns the record's error, which the logout shows the person: the record
// is also what signs out their paired phones (users.RecordSignOutEverywhere).
func (s *Server) signedOutEverywhere(ctx context.Context, uid int64) error {
	var recErr error
	if s.gt != nil && s.gt.Users != nil {
		if _, err := s.gt.Users.RecordSignOutEverywhere(ctx, uid); err != nil {
			s.logger.Printf("logout: record user %d's sign-out everywhere: %v (only the cancel below now stops "+
				"their queued messages; one queued by a request still in flight is not stopped)", uid, err)
			recErr = err
		}
	}
	s.cancelQueuedFor(ctx, uid, SendQueueCancelSignOut)
	return recErr
}

// cancelQueuedFor cancels every message user uid still has queued, saying why:
// the account was disabled, or signed out everywhere.
func (s *Server) cancelQueuedFor(ctx context.Context, uid int64, detail string) {
	if s.ext.SendQueue == nil {
		return
	}
	n, err := s.ext.SendQueue.CancelOwner(ctx, uid, s.sendQueueNow(), detail)
	if err != nil {
		// The cancel is not what stops them: the delivery re-checks the
		// account (sendQueueOwnerRefusal) against the recorded disable or
		// sign-out.
		s.logger.Printf("sendqueue: cancel user %d's queued messages: %v (left queued; the delivery's account check refuses them)", uid, err)
		return
	}
	if n > 0 {
		s.logger.Printf("sendqueue: cancelled %d queued message(s) of user %d (%s)", n, uid, detail)
	}
}

// sendQueueNotePaste records on (host, session, window)'s open queue rows the
// status time the hub held at the paste just made into it (mirrorPaste), as
// ctx's principal, so a queued message waits for that paste's turn even after
// a restart. window is the id the paste RESOLVED to (a Send to the session's
// active window included). A window the hub has no status time for records
// nothing.
func (s *Server) sendQueueNotePaste(ctx context.Context, host, session, window string) {
	if s.ext.SendQueue == nil || s.ext.Mirror == nil || window == "" {
		return
	}
	pasted := s.ext.Mirror.AgentState(ctx, host, session, window).Pasted
	if pasted <= 0 {
		return
	}
	t := sendqueue.Target{Host: host, Session: session, Window: window}
	if err := s.ext.SendQueue.NotePaste(ctx, t, pasted); err != nil {
		s.logger.Printf("sendqueue: note a paste into %s: %v", t, err)
	}
}

// sendQueueStale is the outcome of a delivery the paste's own freshness gate
// refused (it presents the delivery moment, so this is not expected).
func sendQueueStale() sendqueue.Outcome {
	return sendqueue.Outcome{Detail: "Not sent: the sign-in that queued it is older than " + stepup.WindowText() +
		". Retry it to queue it again."}
}

// Why a person's queued message is not sent on the sign-in that queued it
// (sendQueueAuthorization).
var (
	// SendQueueExpired is the same words the dispatcher's expiry sweep
	// records (sendqueue.PGStore.Expire).
	SendQueueExpired            = sendqueue.ExpiredDetail
	SendQueueNotFreshWhenQueued = "Not sent: the sign-in that queued it was not a recent one when it was queued. Retry it to queue it again."
	SendQueueNotFreshWhenEdited = "Not sent: the sign-in that edited it was not a recent one when the edit was saved. Retry it to queue it again."
)

// sendQueueAuthorization is why the sign-in that queued person's message it no
// longer authorizes sending it at now, "" when it does: it was inside the
// step-up window when the row was created (the queue route's gate, checked
// again from the row), and the row is at most sendqueue.AuthorizedFor old.
// CreatedAt is Postgres's clock, StepUpAt GoTrue's and now muster's; a skew of
// seconds between them is immaterial at either bound.
func sendQueueAuthorization(it sendqueue.Item, now time.Time) string {
	switch {
	case it.CreatedAt.Sub(it.StepUpAt) > stepup.Window():
		return SendQueueNotFreshWhenQueued
	// An EDITED message's text was written by its Save, under the Save's own
	// sign-in (edit_stepup_at): that one must have been fresh when it saved.
	case !it.EditedAt.IsZero() && it.EditedAt.Sub(it.EditStepUpAt) > stepup.Window():
		return SendQueueNotFreshWhenEdited
	case now.Sub(it.CreatedAt) > sendqueue.AuthorizedFor:
		return SendQueueExpired
	}
	return ""
}

// newSendQueueDispatcher is the dispatcher RunSendQueue runs: this server's
// hooks, no Instance (each one makes its own: sendqueue.Dispatcher.InstanceID).
func (s *Server) newSendQueueDispatcher() *sendqueue.Dispatcher {
	return &sendqueue.Dispatcher{Store: s.ext.SendQueue, Probe: s.sendQueueProbe, Send: s.sendQueueSend,
		AsOwner: s.queuedPrincipal, IdleFor: s.ext.SendQueueIdle, Now: s.sendQueueNow, Logger: s.logger}
}

// RunSendQueue runs the send queue's dispatcher until ctx ends. It does
// nothing without a queue store. Started by cmd/muster-server beside the
// other background loops.
func (s *Server) RunSendQueue(ctx context.Context, interval time.Duration) {
	if s.ext.SendQueue == nil {
		return
	}
	d := s.newSendQueueDispatcher()
	d.Init()
	s.logger.Printf("sendqueue: dispatcher started: a queued message is sent once its Claude Code session has been idle for %s, "+
		"one at a time per window (MUSTER_SEND_QUEUE_IDLE)", s.sendQueueIdle())
	s.sendQueueMu.Lock()
	s.sendQueue = d
	s.sendQueueMu.Unlock()
	d.Run(ctx, interval)
}

// kickSendQueue asks the dispatcher for a pass now (a message was queued).
func (s *Server) kickSendQueue() {
	s.sendQueueMu.Lock()
	d := s.sendQueue
	s.sendQueueMu.Unlock()
	if d != nil {
		d.Kick()
	}
}
