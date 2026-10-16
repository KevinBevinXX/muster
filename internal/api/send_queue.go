package api

import (
	"context"
	"errors"
	"fmt"
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
// Rendered view's Queue, the queue's list and Cancel, and the dispatcher's two
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
// 🔴 THE STEP-UP IS DECIDED WHEN THE MESSAGE IS QUEUED, AND RE-CHECKED WHEN IT
// IS SENT. Queueing needs the same fresh sign-in a Send needs (403 +
// X-Muster-Step-Up, or the plain confirm page), and the queue stores the
// sign-in time of the session that queued it (stepup_at). The delivery presents
// EXACTLY that time to the same gate — the plugin's own Fresh check included —
// so a queued message is sent only while the sign-in that queued it is still
// inside the step-up window (MUSTER_STEPUP_WINDOW, 12 h by default). Past it
// the message FAILS with a reason and is not sent; nothing about the queue
// widens the window or mints a fresher time.
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
	// The plain confirm page's form: a stale queue the BROWSER submitted.
	mux.HandleFunc("POST "+sendQueueConfirmPath, s.requireOperatorWrite(s.handleSendQueueConfirm))
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
// the host must report Claude Code's status, and the window must run it.
func mirrorQueueOff(h mirror.HostView, e mirror.SessionView) string {
	switch {
	case !h.Online:
		return unprefixed(mirror.ErrHostOffline)
	case !h.StatusReport || h.Windows == nil:
		return unprefixed(mirror.ErrNoStatusReport)
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
	// THE SAME GATES AS A SEND, IN mirrorSend's ORDER — then the queue's own.
	if !q.h.Interactive {
		refuse(http.StatusConflict, mirror.ErrInteractiveRefused.Error(), noticeReadOnly)
		return
	}
	if !stepup.Fresh(ctx, s.stepUpNow()) {
		if nav {
			s.sendQueueStepUpPage(w, r, q, text, "")
			return
		}
		refuseStale(w)
		return
	}
	if !q.listed {
		refuse(http.StatusConflict, mirror.ErrWindowGone.Error(), noticeWindowGone)
		return
	}
	if off := mirrorQueueOff(q.h, q.e); off != "" {
		refuse(http.StatusConflict, off, noticeQueueOff)
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
	s.kickSendQueue()
	if nav {
		http.Redirect(w, r, withNotice(q.back, noticeQueued), http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sendQueueStepUpPage answers a browser-submitted queue whose sign-in is
// stale: the plain confirm page, carrying the message in hidden fields, which
// posts to sendQueueConfirmPath.
func (s *Server) sendQueueStepUpPage(w http.ResponseWriter, r *http.Request, q sendQueueTarget, text, problem string) {
	fields := []ui.ComposerField{{Name: "host", Value: q.h.Host}, {Name: "session", Value: q.e.Session}}
	if q.e.Window != "" {
		fields = append(fields, ui.ComposerField{Name: "window", Value: q.e.Window})
	}
	fields = append(fields, ui.ComposerField{Name: "text", Value: text})
	formDocument(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.StepUpFormPage(ui.StepUpFormView{Action: sendQueueConfirmPath, Fields: fields,
		BackHref: q.back, Window: stepup.WindowText(), Error: problem}).Render(w); err != nil {
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
		s.sendQueueStepUpPage(w, r, q, r.PostFormValue("text"), o.Error)
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
	if err := ui.SendQueueList(s.sendQueueListView(r.Context(), q.t)).Render(w); err != nil {
		s.logger.Printf("sendqueue: render list: %v", err)
	}
}

// sendQueuePreview bounds how much of a message the list shows.
const sendQueuePreview = 200

// sendQueueListView is t's queue as the composer draws it, with what the next
// message is waiting for — all read as ctx's principal: the REQUEST's own,
// never one minted here (only the dispatcher acts as a message's owner).
func (s *Server) sendQueueListView(ctx context.Context, t sendqueue.Target) ui.SendQueueListView {
	items, err := s.ext.SendQueue.List(ctx, t)
	if err != nil {
		s.logger.Printf("sendqueue: list for %s: %v", t, err)
		return ui.SendQueueListView{Error: "The queue could not be read."}
	}
	var v ui.SendQueueListView
	for _, it := range items {
		preview := it.Body
		if len(preview) > sendQueuePreview {
			// A cut through a character leaves invalid bytes; they are dropped.
			preview = strings.ToValidUTF8(preview[:sendQueuePreview], "") + "…"
		}
		v.Items = append(v.Items, ui.SendQueueItem{ID: it.ID, State: string(it.State), Preview: preview,
			Detail: it.Detail, Cancellable: it.State == sendqueue.Queued})
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
	if nav {
		http.Redirect(w, r, withNotice(back, noticeCancelled), http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// mirrorComposerQueue is the Queue of a mirrored entry's Rendered view, nil
// when there is no queue to draw.
func (s *Server) mirrorComposerQueue(ctx context.Context, h mirror.HostView, e mirror.SessionView) *ui.ComposerQueue {
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
		List: s.sendQueueListView(ctx, t), Disabled: mirrorQueueOff(h, e)}
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
	case st.Unknown != nil:
		return sendqueue.AgentState{Why: unprefixed(st.Unknown)}
	case st.Status == mirror.StatusIdle:
		return sendqueue.AgentState{Idle: true, Since: st.Since, Observed: st.Observed, Pasted: st.Pasted}
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
// owner; the sign-in time presented is the one the message was QUEUED with.
//
// 🔴 THE PASTE CARRIES THE TURN IT WAS DECIDED ON (st's Since and Pasted;
// mirror.ExpectTurn). A person's Send that lands between the dispatcher's
// decision and this paste moves the window's Pasted (or is still in flight),
// and the hub then refuses this paste under the same lock that records the
// Send — nothing reaches the host — so the message goes back to queued behind
// the Send's turn (Outcome.Superseded) instead of being pasted into it.
func (s *Server) sendQueueSend(ctx context.Context, it sendqueue.Item, st sendqueue.AgentState) sendqueue.Outcome {
	ctx = stepup.WithAuthTime(ctx, it.StepUpAt)
	if s.ext.Mirror == nil {
		return sendqueue.Outcome{Detail: "Not sent: mirrored sessions are off on this server."}
	}
	if why := s.sendQueueOwnerRefusal(ctx, it); why != "" {
		return sendqueue.Outcome{Detail: why}
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
		mirror.ExpectTurn(mirror.Turn{Since: st.Since, Pasted: st.Pasted}))
	var sup *mirror.SupersededError
	switch {
	case repeat:
		return sendqueue.Outcome{Sent: true, Detail: "Sent (already delivered once; not pasted again)."}
	case errors.Is(err, mirror.ErrStepUpRequired):
		return sendQueueStale()
	case errors.As(err, &sup):
		return sendqueue.Outcome{Superseded: true, AfterSince: sup.Pasted, Detail: unprefixed(err)}
	case err != nil:
		return sendqueue.Outcome{MayHaveDelivered: errors.Is(err, mirror.ErrPasteUnconfirmed),
			Detail: "Not sent: " + unprefixed(err)}
	}
	return sendqueue.Outcome{Sent: true, Detail: "Sent."}
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
	case atOrBefore(it.StepUpAt, u.SessionsNotBefore), atOrBefore(it.StepUpAt, u.SignedOutAt):
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
func (s *Server) signedOutEverywhere(ctx context.Context, uid int64) {
	if s.gt != nil && s.gt.Users != nil {
		if _, err := s.gt.Users.RecordSignOutEverywhere(ctx, uid); err != nil {
			s.logger.Printf("logout: record user %d's sign-out everywhere: %v (only the cancel below now stops "+
				"their queued messages; one queued by a request still in flight is not stopped)", uid, err)
		}
	}
	s.cancelQueuedFor(ctx, uid, SendQueueCancelSignOut)
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

// sendQueueStale is the outcome of a message whose queueing sign-in has left
// the step-up window.
func sendQueueStale() sendqueue.Outcome {
	return sendqueue.Outcome{Detail: "Not sent: the sign-in that queued it is older than " + stepup.WindowText() +
		". Confirm it's you and queue it again."}
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
