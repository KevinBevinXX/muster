package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// EDITING A QUEUED MESSAGE (internal/sendqueue's Hold, SaveEdit, ReleaseEdit;
// migration 0039): the queued row's ⋯ menu has Edit, which opens the message's
// text in the row with Save and Cancel.
//
//   - POST /send-queue/{id}/edit HOLDS the row (the dispatcher never claims a
//     held row, nor anything queued after it for the window) and only then is
//     the text drawn for editing: to the list's script 204, after which it
//     re-reads the list with ?edit=<id>; to a form the browser submitted, a
//     redirect to the view with &edit=<id>, which draws the same form.
//   - POST /send-queue/{id}/edit/save replaces the text under the hold and
//     releases it. The row keeps its place.
//   - POST /send-queue/{id}/edit/cancel releases the hold, text unchanged.
//
// 🔴 THE SAME FRESH SIGN-IN A QUEUE POST NEEDS, for Edit and for Save: an edit
// changes what will be pasted into the session. Save stores its own sign-in
// time on the row (edit_stepup_at) and the delivery checks it again, as it
// checks the queue post's (sendQueueAuthorization, sendQueueOwnerRefusal).
// Stale: 403 + X-Muster-Step-Up to the script, the plain confirm page to a
// browser form (…/edit/confirm, …/edit/save/confirm). Cancel releases a hold
// and changes nothing that will be pasted, so it needs no step-up.
//
// 🔴 A ROUTED MESSAGE IS NOT EDITABLE: its text is the chief's, and its
// delivery is gated on the route rather than on a sign-in.
// ---------------------------------------------------------------------------

// The send queue's edit routes, spelled literally for the route walks.
func (s *Server) registerSendQueueEditRoutes(mux Mux) {
	mux.HandleFunc("POST /send-queue/{id}/edit", s.requireOperatorWrite(s.handleSendQueueEdit))
	mux.HandleFunc("POST /send-queue/{id}/edit/confirm", s.requireOperatorWrite(s.handleSendQueueEditConfirm))
	mux.HandleFunc("POST /send-queue/{id}/edit/save", s.requireOperatorWrite(s.handleSendQueueEditSave))
	mux.HandleFunc("POST /send-queue/{id}/edit/save/confirm", s.requireOperatorWrite(s.handleSendQueueEditSaveConfirm))
	mux.HandleFunc("POST /send-queue/{id}/edit/cancel", s.requireOperatorWrite(s.handleSendQueueEditCancel))
}

// Why an edit was refused, and what it did.
var (
	SendQueueNotEditable       = "Not editable: that message is no longer queued (it was sent, or cancelled)."
	SendQueueEditSending       = "Not editable: that message is being sent right now."
	SendQueueRoutedNotEditable = "Not editable: your chief routed that message, so its text is the chief's."
	SendQueueEditLapsed        = "Not saved: the hold on that message ran out (an edit holds it for " + editHoldText +
		") or it was opened for editing again elsewhere, so it kept its original text. Edit it again to change it."
	SendQueueEditTooLateSent = "Not saved: the hold on that message ran out and it has been sent with its original text."
	SendQueueEditTooLate     = "Not saved: that message is no longer queued (it was cancelled, or has finished)."
	SendQueueHeldNow         = "Not sent: that message is being edited. Save or cancel the edit first."
)

// editHoldText is sendqueue.EditHold as a person reads it.
var editHoldText = strconv.Itoa(int(sendqueue.EditHold.Minutes())) + " minutes"

// sendQueueEditBack is where an edit route sends a browser back to: the view,
// drawing the row's edit form when editing is set.
func sendQueueEditBack(back string, id int64, editing bool) string {
	if !editing {
		return back
	}
	return back + "&" + ui.SendQueueEditParam + "=" + strconv.FormatInt(id, 10)
}

// handleSendQueueEdit is POST /send-queue/{id}/edit: hold the principal's
// queued message for an edit.
func (s *Server) handleSendQueueEdit(w http.ResponseWriter, r *http.Request) {
	s.sendQueueEdit(w, r, r.Context(), false)
}

// handleSendQueueEditConfirm is the plain confirm page's form for a stale
// Edit: the password again, then the Edit under that sign-in.
func (s *Server) handleSendQueueEditConfirm(w http.ResponseWriter, r *http.Request) {
	s.sendQueueEditStepUp(w, r, func(w http.ResponseWriter, r *http.Request, ctx context.Context) { s.sendQueueEdit(w, r, ctx, true) },
		func(id int64) string { return ui.SendQueueEditPath(id) + "/confirm" }, nil)
}

// sendQueueEditStepUp is an edit route's plain confirm page: a right password
// replaces the session with a freshly signed-in one (stepUpWithPassword, the
// SAME check as the dialog's) and then runs next under it; a wrong one draws
// the page again, carrying fields.
func (s *Server) sendQueueEditStepUp(w http.ResponseWriter, r *http.Request, next func(http.ResponseWriter, *http.Request, context.Context),
	action func(int64) string, fields func(*http.Request) []ui.ComposerField) {
	if s.ext.SendQueue == nil || s.ext.Mirror == nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, mirror.MaxPaste*4)
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
		var fs []ui.ComposerField
		if fields != nil {
			fs = fields(r)
		}
		s.sendQueueStepUpPage(w, action(id), fs, q.back, o.Error)
		return
	}
	next(w, r, stepup.WithAuthTime(r.Context(), o.At))
}

// sendQueueEdit holds the route's message. ctx carries the sign-in the
// step-up gate reads; confirmed says it is the confirm page's post.
func (s *Server) sendQueueEdit(w http.ResponseWriter, r *http.Request, ctx context.Context, confirmed bool) {
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
	// Fast refusals before the step-up (no "Confirm it's you" for a message
	// that cannot be edited anyway). Hold is what guarantees them.
	if msg, notice := sendQueueEditRefusal(it); msg != "" {
		refuse(http.StatusConflict, msg, notice)
		return
	}
	if !stepup.Fresh(ctx, s.stepUpNow()) {
		if nav {
			s.sendQueueStepUpPage(w, ui.SendQueueEditPath(id)+"/confirm", nil, q.back, "")
		} else {
			refuseStale(w)
		}
		return
	}
	held, err := s.ext.SendQueue.Hold(ctx, id)
	if err != nil {
		if errors.Is(err, sendqueue.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if !errors.Is(err, sendqueue.ErrNotEditable) {
			s.logger.Printf("sendqueue: hold %d for an edit: %v", id, err)
			refuse(http.StatusInternalServerError, "could not open the message for editing", noticeFailed)
			return
		}
		msg, notice := sendQueueEditRefusal(held)
		if msg == "" {
			msg, notice = SendQueueNotEditable, noticeNotEditable
		}
		refuse(http.StatusConflict, msg, notice)
		return
	}
	s.logger.Printf("sendqueue: message %d for %s held for an edit until %s", id, q.t, held.HeldUntil.Format("15:04:05"))
	if nav {
		http.Redirect(w, r, withNotice(sendQueueEditBack(q.back, id, true), noticeEditing), http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sendQueueEditRefusal is why it cannot be held for an edit, and its notice;
// "" when it can as far as this read of it goes.
func sendQueueEditRefusal(it sendqueue.Item) (msg, notice string) {
	switch {
	case it.RouteID != "":
		return SendQueueRoutedNotEditable, noticeRoutedNotEditable
	case it.State == sendqueue.Sending:
		return SendQueueEditSending, noticeEditSending
	case it.State != sendqueue.Queued:
		return SendQueueNotEditable, noticeNotEditable
	}
	return "", ""
}

// handleSendQueueEditSave is POST /send-queue/{id}/edit/save (hold, text):
// replace the held message's text and release the hold.
func (s *Server) handleSendQueueEditSave(w http.ResponseWriter, r *http.Request) {
	s.sendQueueEditSave(w, r, r.Context(), false)
}

// handleSendQueueEditSaveConfirm is the plain confirm page's form for a
// stale Save: the password again, then the Save under that sign-in.
func (s *Server) handleSendQueueEditSaveConfirm(w http.ResponseWriter, r *http.Request) {
	s.sendQueueEditStepUp(w, r, func(w http.ResponseWriter, r *http.Request, ctx context.Context) {
		s.sendQueueEditSave(w, r, ctx, true)
	},
		func(id int64) string { return ui.SendQueueEditSavePath(id) + "/confirm" }, sendQueueEditFields)
}

// sendQueueEditFields is what a Save's confirm page carries: the hold and the
// edited text.
func sendQueueEditFields(r *http.Request) []ui.ComposerField {
	return []ui.ComposerField{{Name: "hold", Value: r.PostFormValue("hold")}, {Name: "text", Value: r.PostFormValue("text")}}
}

// sendQueueEditSave saves the route's edit. ctx carries the sign-in the gate
// reads; confirmed says it is the confirm page's post.
func (s *Server) sendQueueEditSave(w http.ResponseWriter, r *http.Request, ctx context.Context, confirmed bool) {
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
	id, it, q, ok := s.sendQueueRowTarget(w, r)
	if !ok {
		return
	}
	nav := confirmed || isDocumentNavigation(r)
	text, hold := r.PostFormValue("text"), r.PostFormValue("hold")
	refuse := func(status int, msg, notice string, editing bool) {
		if nav {
			http.Redirect(w, r, withNotice(sendQueueEditBack(q.back, id, editing), notice), http.StatusSeeOther)
			return
		}
		http.Error(w, msg, status)
	}
	if it.RouteID != "" {
		refuse(http.StatusConflict, SendQueueRoutedNotEditable, noticeRoutedNotEditable, false)
		return
	}
	// The form stays open (the hold is still live) for what the person can fix.
	if strings.TrimSpace(text) == "" {
		refuse(http.StatusBadRequest, MirrorSendEmpty, noticeEmpty, it.Held)
		return
	}
	if _, err := mirror.SanitizePaste(text); err != nil {
		refuse(http.StatusBadRequest, err.Error(), noticeBadText, it.Held)
		return
	}
	if !stepup.Fresh(ctx, s.stepUpNow()) {
		if nav {
			s.sendQueueStepUpPage(w, ui.SendQueueEditSavePath(id)+"/confirm", sendQueueEditFields(r), q.back, "")
		} else {
			refuseStale(w)
		}
		return
	}
	at, _ := stepup.AuthTime(ctx)
	saved, err := s.ext.SendQueue.SaveEdit(ctx, id, hold, text, at)
	switch {
	case errors.Is(err, sendqueue.ErrNotFound):
		http.NotFound(w, r)
		return
	case errors.Is(err, sendqueue.ErrHoldLapsed):
		refuse(http.StatusConflict, SendQueueEditLapsed, noticeEditLapsed, false)
		return
	case errors.Is(err, sendqueue.ErrEditTooLate):
		if saved.State == sendqueue.Sending || saved.State == sendqueue.Sent || (saved.State == sendqueue.Failed && saved.MayHaveDelivered) {
			refuse(http.StatusConflict, SendQueueEditTooLateSent, noticeEditTooLateSent, false)
		} else {
			refuse(http.StatusConflict, SendQueueEditTooLate, noticeEditTooLate, false)
		}
		return
	case err != nil:
		s.logger.Printf("sendqueue: save the edit of %d: %v", id, err)
		refuse(http.StatusInternalServerError, "could not save the edit", noticeFailed, false)
		return
	}
	s.logger.Printf("sendqueue: message %d for %s edited (%d bytes)", id, q.t, len(saved.Body))
	s.kickSendQueue()
	if nav {
		http.Redirect(w, r, withNotice(q.back, noticeEdited), http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSendQueueEditCancel is POST /send-queue/{id}/edit/cancel (hold):
// release the hold, the text unchanged. A hold that is no longer live (it ran
// out, or was taken over) has nothing to release; the answer is the same.
func (s *Server) handleSendQueueEditCancel(w http.ResponseWriter, r *http.Request) {
	if s.ext.SendQueue == nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, mirror.MaxPaste*4)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "the form is too large", http.StatusRequestEntityTooLarge)
		return
	}
	id, _, q, ok := s.sendQueueRowTarget(w, r)
	if !ok {
		return
	}
	_, released, err := s.ext.SendQueue.ReleaseEdit(r.Context(), id, r.PostFormValue("hold"))
	switch {
	case errors.Is(err, sendqueue.ErrNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		s.logger.Printf("sendqueue: release the edit of %d: %v", id, err)
		if isDocumentNavigation(r) {
			http.Redirect(w, r, withNotice(q.back, noticeFailed), http.StatusSeeOther)
			return
		}
		http.Error(w, "could not cancel the edit", http.StatusInternalServerError)
		return
	}
	if released {
		s.logger.Printf("sendqueue: the edit of message %d for %s was cancelled", id, q.t)
		s.kickSendQueue()
	}
	if isDocumentNavigation(r) {
		http.Redirect(w, r, withNotice(q.back, noticeEditCancelled), http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
