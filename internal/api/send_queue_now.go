package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// SEND NOW (POST /send-queue/{id}/send-now): a person sends one of their
// QUEUED messages at once, without waiting for idle — the queue row's ⋯ menu
// (ui.SendQueueList). It is pasted through the same path a Send uses, into the
// window's Claude Code pane, which takes typed input while busy.
//
// 🔴 NEVER TWICE: the row is CLAIMED (queued → sending) before anything is
// pasted, by the dispatcher's own Claim (sendqueue.Dispatcher.SendNow), so the
// dispatcher can never also send it — whichever claims first sends, and the
// other finds it no longer queued. It ends sent, or failed with the reason; it
// is never put back to queued.
//
// 🔴 THE NEXT MESSAGE STILL WAITS FOR THIS ONE'S TURN: the paste is recorded
// like a Send's (mirrorPaste notes it, in the hub and on the open rows), and the
// claim carries the window's status time, which becomes the row's after_since.
//
// 🔴 THE SAME GATES AS A SEND, THEN THE DISPATCHER'S: the host online and
// -interactive, a FRESH sign-in on the request (403 + X-Muster-Step-Up to the
// script, the plain confirm page to a browser form), the window listed; then
// the claim, which the target's one-sending index refuses while another of its
// messages is being sent; then the delivery's own checks on the
// row, the ones the dispatcher's send makes (sendQueueDeliver: the owner's
// account, the sign-in that queued it, a routed row's route).
// ---------------------------------------------------------------------------

// Why a Send now was refused.
const (
	SendQueueNotQueuedNow   = "Not sent: that message is no longer queued (it is being sent, or has finished)."
	SendQueueSendingAnother = "Not sent: another message for this window is being sent right now. Try again in a moment."
)

// handleSendQueueSendNow is POST /send-queue/{id}/send-now: send the
// principal's QUEUED message now. To the list's script 204 when it was sent,
// 403 + X-Muster-Step-Up for a stale sign-in, 409 with the reason for a
// refusal that sent nothing, 502 with the reason for a delivery that failed
// (the row is then failed, saying so); to a form the browser submitted, a
// redirect back with a notice, or the plain confirm page when stale. 404 for a
// message the principal cannot see.
func (s *Server) handleSendQueueSendNow(w http.ResponseWriter, r *http.Request) {
	s.sendQueueSendNow(w, r, r.Context(), false)
}

// handleSendQueueSendNowConfirm is POST /send-queue/{id}/send-now/confirm: the
// plain confirm page's form for a stale Send now — the password again. A right
// one replaces the session with a freshly signed-in one (stepUpWithPassword)
// and the message is sent under THAT sign-in.
func (s *Server) handleSendQueueSendNowConfirm(w http.ResponseWriter, r *http.Request) {
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
		s.sendQueueStepUpPage(w, sendQueueSendNowConfirmPath(id), nil, q.back, o.Error)
		return
	}
	s.sendQueueSendNow(w, r, stepup.WithAuthTime(r.Context(), o.At), true)
}

func sendQueueSendNowConfirmPath(id int64) string { return ui.SendQueueSendNowPath(id) + "/confirm" }

// sendQueueSendNow sends the route's message now. ctx carries the sign-in time
// the gate reads (the request's, or one the confirm page just minted);
// confirmed says it is the confirm page's post, which is always a browser
// navigation.
func (s *Server) sendQueueSendNow(w http.ResponseWriter, r *http.Request, ctx context.Context, confirmed bool) {
	if s.ext.SendQueue == nil || s.ext.Mirror == nil {
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
	// A fast refusal before the gates (no step-up asked for a message already
	// sent). The claim below is what guarantees it: it refuses a row that is
	// no longer queued whatever this read saw.
	if it.State != sendqueue.Queued {
		refuse(http.StatusConflict, SendQueueNotQueuedNow, noticeNotQueuedNow)
		return
	}
	// A Send's gates, in mirrorSend's order.
	switch {
	case !q.h.Online:
		refuse(http.StatusConflict, unprefixed(mirror.ErrHostOffline), noticeOffline)
		return
	case !q.h.Interactive:
		refuse(http.StatusConflict, mirror.ErrInteractiveRefused.Error(), noticeReadOnly)
		return
	case !stepup.Fresh(ctx, s.stepUpNow()):
		if nav {
			s.sendQueueStepUpPage(w, sendQueueSendNowConfirmPath(id), nil, q.back, "")
		} else {
			refuseStale(w)
		}
		return
	case !q.listed:
		refuse(http.StatusConflict, mirror.ErrWindowGone.Error(), noticeWindowGone)
		return
	}
	// The window's status time now: the claim's, so the next queued message
	// waits for the turn this paste starts (0 when the host cannot say; the
	// hub's own record of the paste still holds it).
	since := s.ext.Mirror.AgentState(ctx, q.t.Host, q.t.Session, q.t.Window).Since
	sent, o, err := s.sendQueueDispatcher().SendNow(ctx, ctx, id, since, func(octx context.Context, it sendqueue.Item, _ sendqueue.AgentState) sendqueue.Outcome {
		// No turn expected: a person's Send now is pasted like a Send.
		return s.sendQueueDeliver(octx, it)
	})
	switch {
	case errors.Is(err, sendqueue.ErrNotClaimed):
		// Which of the two: the row moved on, or another row is being sent.
		if now, gerr := s.ext.SendQueue.Get(ctx, id); gerr == nil && now.State == sendqueue.Queued {
			refuse(http.StatusConflict, SendQueueSendingAnother, noticeSendingAnother)
		} else {
			refuse(http.StatusConflict, SendQueueNotQueuedNow, noticeNotQueuedNow)
		}
		return
	case err != nil:
		s.logger.Printf("sendqueue: send now %d for %s: %v", id, q.t, err)
		refuse(http.StatusInternalServerError, "could not send the message", noticeFailed)
		return
	}
	if !o.Sent {
		s.logger.Printf("sendqueue: send now: message %d (%d bytes) to %s failed (may have been delivered: %t): %s",
			sent.ID, len(sent.Body), sent.Target, o.MayHaveDelivered, o.Detail)
		refuse(http.StatusBadGateway, o.Detail, noticeSendNowFailed)
		return
	}
	s.logger.Printf("sendqueue: sent message %d (%d bytes) to %s now, at its owner's request", sent.ID, len(sent.Body), sent.Target)
	if nav {
		http.Redirect(w, r, withNotice(q.back, noticeSent), http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sendQueueDispatcher is the dispatcher a Send now claims through: the running
// one (RunSendQueue's), or — none running — one made for the call, with this
// server's hooks. Either way the claim is the store's one conditional UPDATE,
// which is what keeps it from being sent twice.
func (s *Server) sendQueueDispatcher() *sendqueue.Dispatcher {
	s.sendQueueMu.Lock()
	d := s.sendQueue
	s.sendQueueMu.Unlock()
	if d != nil {
		return d
	}
	return s.newSendQueueDispatcher()
}
