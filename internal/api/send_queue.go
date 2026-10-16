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

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE SEND QUEUE's server half (internal/sendqueue, migration 0019): the
// composers' Queue, the queue's list and Cancel, and the dispatcher's two
// hooks — the target's state (sendQueueProbe) and the delivery
// (sendQueueSend).
//
// 🔴 A QUEUED MESSAGE IS DELIVERED THROUGH THE SAME PATH AS A SEND, WITH THE
// SAME GATES, AS ITS OWNER. The delivery runs mirrorPaste / agentSendTurn —
// the functions a person's Send runs — behind the same checks in the same
// order (the host is the owner's, -interactive, a fresh sign-in; the agent is
// the owner's, a gateway, running), under the owner's own principal
// (queuedPrincipal: authz.ViaQueue), so row-level security and the plugin's
// own gates see the owner and nobody else.
//
// 🔴 THE STEP-UP IS DECIDED WHEN THE MESSAGE IS QUEUED, AND RE-CHECKED WHEN IT
// IS SENT. Queueing for a mirrored window needs the same fresh sign-in a Send
// needs (403 + X-Muster-Step-Up, or the plain confirm page), and the queue
// stores the sign-in time of the session that queued it (stepup_at). The
// delivery presents EXACTLY that time to the same gate — the plugin's own
// Fresh check included — so a queued message is sent only while the sign-in
// that queued it is still inside the step-up window (MUSTER_STEPUP_WINDOW,
// 12 h by default). Past it the message FAILS with a reason and is not sent;
// nothing about the queue widens the window or mints a fresher time. (An agent
// send has no step-up, queued or not.)
//
// ⚠ WHAT THAT DOES NOT COVER: the recorded time is the queueing SESSION's, and
// the delivery does not re-check that the session still exists — a message
// queued and then signed out of is still sent inside its window. Cancel it to
// stop it.
//
// 🔴 UNKNOWN IS NOT IDLE (fail closed). For a mirror host whose muster-mirror
// does not report Claude Code's status, or a window that is not Claude Code,
// Queue is drawn disabled saying why and a queue post is refused saying why.
// An agent that is not running may be queued for (its composer is disabled, so
// only a post can do it) and its messages wait. Whatever is already queued for
// a target the server cannot read is held, the list saying why — never sent on
// a guess.
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
// request's principal: the target, the pane a mirror message may land in, the
// page the composer is on, and — for a mirror — the host and entry. ok is
// false when it answered already: 404 for anything that is not the
// principal's (never a 403 that would say "this exists"), 400 for a malformed
// kind.
type sendQueueTarget struct {
	t    sendqueue.Target
	kind string
	pane string
	back string
	h    mirror.HostView
	e    mirror.SessionView
	// listed: a mirror entry the host lists now.
	listed bool
}

func (s *Server) resolveSendQueueTarget(w http.ResponseWriter, r *http.Request, v url.Values) (sendQueueTarget, bool) {
	var q sendQueueTarget
	if s.ext.SendQueue == nil {
		http.NotFound(w, r)
		return q, false
	}
	q.kind = v.Get("queue")
	switch q.kind {
	case ui.QueueKindView, ui.QueueKindTerminal:
		h, e, listed, _, ok := s.mirrorEntry(r, v.Get("host"), v.Get("session"), v.Get("window"))
		if !ok {
			http.NotFound(w, r)
			return q, false
		}
		q.h, q.e, q.listed = h, e, listed
		q.t = sendqueue.Target{Kind: sendqueue.KindMirror, Host: h.Host, Session: e.Session, Window: e.Window}
		q.pane, q.back = sendqueue.PaneClaude, mirrorRef(e).ViewPath()
		if q.kind == ui.QueueKindTerminal {
			q.pane, q.back = sendqueue.PaneActive, mirrorRef(e).TerminalPath()
		}
	case ui.QueueKindAgent:
		if s.ext.Agents == nil {
			http.NotFound(w, r)
			return q, false
		}
		a, err := s.ext.Agents.GetByName(r.Context(), v.Get("agent"))
		if err != nil {
			http.NotFound(w, r)
			return q, false
		}
		q.t = sendqueue.Target{Kind: sendqueue.KindAgent, AgentID: a.ID}
		q.back = ui.AgentTerminalPath(a.Name)
	default:
		http.Error(w, "unknown queue", http.StatusBadRequest)
		return q, false
	}
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

// handleSendQueueAdd is POST /send-queue (queue, the target's address, text,
// session): queue a message. To the composer's script 204 or a status and the
// reason; to a form the browser submitted, a redirect back with a notice.
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
		r.Body = http.MaxBytesReader(w, r.Body, agentSendMax)
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
	it := sendqueue.Item{Target: q.t, Pane: q.pane, Body: text}
	switch q.t.Kind {
	case sendqueue.KindMirror:
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
		it.StepUpAt, _ = stepup.AuthTime(ctx)
	case sendqueue.KindAgent:
		if s.ext.Gateway == nil {
			refuse(http.StatusConflict, composerNotices[noticeNoGateway].text, noticeNoGateway)
			return
		}
		if id, err := strconv.ParseInt(r.PostFormValue("session"), 10, 64); err == nil && id > 0 {
			it.AgentSession = id
		}
	}
	stored, err := s.ext.SendQueue.Enqueue(ctx, it)
	switch {
	case errors.Is(err, sendqueue.ErrFull):
		refuse(http.StatusConflict, strings.TrimPrefix(err.Error(), "sendqueue: "), noticeQueueFull)
		return
	case errors.Is(err, sendqueue.ErrNoSuchAgent):
		http.NotFound(w, r)
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
	fields := []ui.ComposerField{{Name: "queue", Value: q.kind}, {Name: "host", Value: q.h.Host}, {Name: "session", Value: q.e.Session}}
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
	if k := r.PostFormValue("queue"); k != ui.QueueKindView && k != ui.QueueKindTerminal {
		http.Error(w, "unknown queue", http.StatusBadRequest)
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

// handleSendQueueList is GET /send-queue (queue + the target's address): the
// target's queue as an HTML fragment (ui.SendQueueList), which the composer's
// script swaps into #mv-queue.
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

// sendQueueListView is t's queue as the composer draws it, read as ctx's
// principal, with what the next message is waiting for.
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
	back := s.sendQueueBack(ctx, it)
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

// sendQueueBack is the composer page a message was queued from.
func (s *Server) sendQueueBack(ctx context.Context, it sendqueue.Item) string {
	if it.Target.Kind == sendqueue.KindMirror {
		ref := ui.MirrorRef{Host: it.Target.Host, Session: it.Target.Session, Window: it.Target.Window}
		if it.Pane == sendqueue.PaneActive {
			return ref.TerminalPath()
		}
		return ref.ViewPath()
	}
	if s.ext.Agents != nil {
		if a, err := s.ext.Agents.Get(ctx, it.Target.AgentID); err == nil {
			return ui.AgentTerminalPath(a.Name)
		}
	}
	return "/agents"
}

// mirrorComposerQueue is the Queue of a mirrored entry's composer (kind: its
// Rendered or Terminal view), nil when there is no queue to draw.
func (s *Server) mirrorComposerQueue(ctx context.Context, h mirror.HostView, e mirror.SessionView, kind string) *ui.ComposerQueue {
	if s.ext.SendQueue == nil {
		return nil
	}
	ref := mirrorRef(e)
	q := url.Values{"queue": {kind}, "host": {ref.Host}, "session": {ref.Session}}
	if ref.Window != "" {
		q.Set("window", ref.Window)
	}
	t := sendqueue.Target{Kind: sendqueue.KindMirror, Host: h.Host, Session: e.Session, Window: e.Window}
	return &ui.ComposerQueue{Kind: kind, Src: ui.SendQueuePath + "?" + q.Encode(),
		List: s.sendQueueListView(ctx, t), Disabled: mirrorQueueOff(h, e)}
}

// agentComposerQueue is the Queue of an agent's composer, nil when there is
// none to draw.
func (s *Server) agentComposerQueue(ctx context.Context, a agents.Agent) *ui.ComposerQueue {
	if s.ext.SendQueue == nil || s.ext.Gateway == nil {
		return nil
	}
	q := url.Values{"queue": {ui.QueueKindAgent}, "agent": {a.Name}}
	return &ui.ComposerQueue{Kind: ui.QueueKindAgent, Src: ui.SendQueuePath + "?" + q.Encode(),
		List: s.sendQueueListView(ctx, sendqueue.Target{Kind: sendqueue.KindAgent, AgentID: a.ID})}
}

// ---------------------------------------------------------------------------
// The dispatcher's hooks.
// ---------------------------------------------------------------------------

// queuedPrincipal is ctx acting as owner, through authz.ViaQueue: how a queued
// message is probed and delivered — as the user who queued it, never as the
// system. The dispatcher read the row (and so its owner) under the system
// scope, and nothing else it read is handed on; the queue list reads it as the
// requesting owner, so there it names the same user. internal/authz's
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

// sendQueueDecide is sendqueue.Decide with this server's idle time and clock.
func (s *Server) sendQueueDecide(h sendqueue.Head, st sendqueue.AgentState) sendqueue.Decision {
	idle := s.ext.SendQueueIdle
	if idle <= 0 {
		idle = sendqueue.DefaultIdleFor
	}
	return sendqueue.Decide(h, st, s.sendQueueNow(), idle, sendqueue.DefaultTurnWait)
}

// sendQueueProbe is what the server can see of h's target now.
func (s *Server) sendQueueProbe(ctx context.Context, h sendqueue.Head) sendqueue.AgentState {
	if h.Owner <= 0 {
		return sendqueue.AgentState{Why: "the message has no owner"}
	}
	octx := s.queuedPrincipal(ctx, h.Owner)
	switch h.Target.Kind {
	case sendqueue.KindMirror:
		if s.ext.Mirror == nil {
			return sendqueue.AgentState{Why: "mirrored sessions are off on this server"}
		}
		st := s.ext.Mirror.AgentState(octx, h.Target.Host, h.Target.Session, h.Target.Window)
		switch {
		case st.Unknown != nil:
			return sendqueue.AgentState{Why: unprefixed(st.Unknown)}
		case st.Status == mirror.StatusIdle:
			return sendqueue.AgentState{Idle: true, Since: st.Since}
		case st.Status == mirror.StatusWaiting:
			return sendqueue.AgentState{Why: "Claude Code is waiting for you (a prompt or a question)."}
		}
		return sendqueue.AgentState{Why: "Claude Code is working."}
	case sendqueue.KindAgent:
		if s.ext.Agents == nil || s.ext.Gateway == nil {
			return sendqueue.AgentState{Why: composerNotices[noticeNoGateway].text}
		}
		a, err := s.ext.Agents.Get(octx, h.Target.AgentID)
		if err != nil {
			return sendqueue.AgentState{Why: "the agent could not be read"}
		}
		if !agentCanBeSent(s.liveStatus(octx, a)) {
			return sendqueue.AgentState{Why: AgentNotRunningNotice}
		}
		inFlight, ended := s.agentTurnState(a.Name)
		if inFlight {
			return sendqueue.AgentState{Why: "the agent is answering."}
		}
		// Idle since its last turn ended — or, with none since this process
		// started, since it started (a turn in flight before a restart is gone).
		since := ended
		if since.Before(s.startedAt) {
			since = s.startedAt
		}
		return sendqueue.AgentState{Idle: true, Since: since}
	}
	return sendqueue.AgentState{Why: "unknown target"}
}

// sendQueueNonce is the nonce a queued message is delivered with: one per
// row, so a delivery the send path has already seen is never made twice. It
// must match sendNoncePattern (16+ characters), or the send path would treat
// it as no nonce at all and claim nothing.
func sendQueueNonce(id int64) string { return fmt.Sprintf("send-queue-%020d", id) }

// sendQueueSend delivers one claimed message as its owner.
func (s *Server) sendQueueSend(ctx context.Context, it sendqueue.Item) sendqueue.Outcome {
	octx := s.queuedPrincipal(ctx, it.Owner)
	switch it.Target.Kind {
	case sendqueue.KindMirror:
		return s.sendQueueToMirror(stepup.WithAuthTime(octx, it.StepUpAt), it)
	case sendqueue.KindAgent:
		return s.sendQueueToAgent(octx, it)
	}
	return sendqueue.Outcome{Detail: "unknown target"}
}

// sendQueueToMirror is mirrorSend's gates, then mirrorPaste. ctx carries the
// owner and the sign-in time the message was queued with.
func (s *Server) sendQueueToMirror(ctx context.Context, it sendqueue.Item) sendqueue.Outcome {
	if s.ext.Mirror == nil {
		return sendqueue.Outcome{Detail: "Not sent: mirrored sessions are off on this server."}
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
	target := mirror.PasteClaude
	if it.Pane == sendqueue.PaneActive {
		target = mirror.PasteActivePane
	}
	repeat, err := s.mirrorPaste(ctx, it.Target.Host, it.Target.Session, it.Target.Window, it.Body, sendQueueNonce(it.ID), target)
	switch {
	case repeat:
		return sendqueue.Outcome{Sent: true, Detail: "Sent (already delivered once; not pasted again)."}
	case errors.Is(err, mirror.ErrStepUpRequired):
		return sendQueueStale()
	case err != nil:
		return sendqueue.Outcome{MayHaveDelivered: errors.Is(err, mirror.ErrPasteUnconfirmed),
			Detail: "Not sent: " + unprefixed(err)}
	}
	return sendqueue.Outcome{Sent: true, Detail: "Sent."}
}

// sendQueueStale is the outcome of a message whose queueing sign-in has left
// the step-up window.
func sendQueueStale() sendqueue.Outcome {
	return sendqueue.Outcome{Detail: "Not sent: the sign-in that queued it is older than " + stepup.WindowText() +
		". Confirm it's you and queue it again."}
}

// sendQueueToAgent is handleAgentSend's gates, then agentSendTurn. ctx
// carries the owner.
func (s *Server) sendQueueToAgent(ctx context.Context, it sendqueue.Item) sendqueue.Outcome {
	if s.ext.Agents == nil || s.ext.Gateway == nil {
		return sendqueue.Outcome{Detail: composerNotices[noticeNoGateway].text}
	}
	a, err := s.ext.Agents.Get(ctx, it.Target.AgentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return sendqueue.Outcome{Detail: "Not sent: the agent no longer exists."}
	}
	if err != nil {
		return sendqueue.Outcome{Detail: "Not sent: the agent could not be read."}
	}
	if !agentCanBeSent(s.liveStatus(ctx, a)) {
		return sendqueue.Outcome{Detail: "Not sent: " + AgentNotRunningNotice}
	}
	session := ""
	if it.AgentSession > 0 {
		session = strconv.FormatInt(it.AgentSession, 10)
	}
	if _, _, err := s.agentSendTurn(ctx, a, session, it.Body, sendQueueNonce(it.ID)); err != nil {
		return sendqueue.Outcome{Detail: "Not sent: could not open a chat session for the agent."}
	}
	return sendqueue.Outcome{Sent: true, Detail: "Sent."}
}

// RunSendQueue runs the send queue's dispatcher until ctx ends. It does
// nothing without a queue store. Started by cmd/muster-server beside the
// other background loops.
func (s *Server) RunSendQueue(ctx context.Context, interval time.Duration) {
	if s.ext.SendQueue == nil {
		return
	}
	d := &sendqueue.Dispatcher{Store: s.ext.SendQueue, Probe: s.sendQueueProbe, Send: s.sendQueueSend,
		IdleFor: s.ext.SendQueueIdle, Now: s.sendQueueNow, Logger: s.logger}
	d.Init()
	idle := s.ext.SendQueueIdle
	if idle <= 0 {
		idle = sendqueue.DefaultIdleFor
	}
	s.logger.Printf("sendqueue: dispatcher started: a queued message is sent once its target has been idle for %s, "+
		"one at a time per target (MUSTER_SEND_QUEUE_IDLE)", idle)
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
