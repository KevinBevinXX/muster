package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE RENDERED VIEW of a mirrored session whose pane runs Claude Code (tunnel
// v2): the page, its transcript socket, and its composer — and the ONE send
// handler behind that composer and the Terminal view's (mirrorSend).
//
// 🔴 NOTHING HERE IS PERSISTED. The socket relays the host's batches, rendered
// to HTML with the agent chat's own components, to the one browser that opened
// it; there is no table and no cache. A reload asks the host again.
//
// 🔴 OWNER-SCOPED EXACTLY LIKE THE TERMINAL: every route resolves the host
// through mirrorEntry (the request principal's hosts only), so another user's
// host 404s, and the plugin keys its own lookup on the same principal.
//
// 🔴 SENDING IS A WRITE INTO THE OPERATOR'S SESSION, AND IT IS GATED THREE
// TIMES HERE BEFORE THE PLUGIN AND THE HOST GATE IT AGAIN: a same-origin
// request (requireOperatorWrite), a host that advertised -interactive, and a
// FRESH SIGN-IN — internal/stepup, the SAME gate on the same clock as an
// interactive terminal attach (mirror.go), not a second notion of "fresh".
// Both send routes run those three in mirrorSend; they differ only in WHICH
// pane the host may paste into (mirror.PasteTarget).
//
// A leading `/` or `!` is pasted as typed (an operator decision): the sender is
// the operator, on a host that opted into -interactive, which is already a
// shell there. Only control characters are refused (mirror.SanitizePaste).
// ---------------------------------------------------------------------------

func (s *Server) registerMirrorViewRoutes(mux Mux) {
	mux.HandleFunc("GET /mirror/view", s.requireSession(s.handleMirrorViewPage))
	mux.HandleFunc("GET /mirror/view/ws", s.requireSession(s.handleMirrorViewWS))
	// An opened subagent card's steps (mirror_subagents.go).
	mux.HandleFunc("GET "+ui.MirrorSubagentPath, s.requireSession(s.handleMirrorSubagent))
	mux.HandleFunc("POST "+ui.MirrorSendPath, s.requireOperatorWrite(s.handleMirrorViewSend))
	// A send that came back from a sign-in as a GET (handleMirrorSendAgain).
	mux.HandleFunc("GET "+ui.MirrorSendPath, s.requireSession(s.handleMirrorViewSendAgain))
	mux.HandleFunc("GET "+ui.MirrorTerminalSendPath, s.requireSession(s.handleMirrorTerminalSendAgain))
	// The plain confirm page's form: a stale send the BROWSER submitted.
	mux.HandleFunc("POST "+mirrorConfirmSendPath, s.requireOperatorWrite(s.handleMirrorConfirmSend))
}

// handleMirrorViewPage is GET /mirror/view?host=…&session=…[&window=…]. An
// entry with no rendered view (a v1 host, an offline one, a pane not running
// Claude Code) goes to its terminal instead, which says why it cannot attach if
// it cannot.
func (s *Server) handleMirrorViewPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	h, e, _, redirect, ok := s.mirrorEntry(r, q.Get("host"), q.Get("session"), q.Get("window"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !e.HasRenderedView() {
		http.Redirect(w, r, keepNotice(r, mirrorRef(e).TerminalPath()), http.StatusSeeOther)
		return
	}
	if redirect {
		http.Redirect(w, r, keepNotice(r, mirrorRef(e).ViewPath()), http.StatusSeeOther)
		return
	}
	// The viewer opened this window (best-effort).
	s.recordMirrorVisit(r.Context(), h.Host, e.Session, e.Window)
	v := ui.MirrorViewView{Host: h.Host, Session: e.Session, Window: e.Window, Title: mirrorTitle(e), Label: e.Label, Qualifier: e.Qualifier, Interactive: h.Interactive,
		StepUp: s.stepUpView(r, ""), Nonce: newSendNonce(), Dictate: s.dictatePath(), Upload: s.uploadPath(),
		FocusHistory: s.mirrorFocusHistoryPath(e.SessionKey()), DraftUser: draftUser(r.Context())}
	// 🔴 A HERMES WINDOW SENDS ONLY WHEN ITS HOST ADVERTISED hermesSend
	// (Phase 3): otherwise its composer is drawn disabled, with no queue, and
	// every send is refused again at the hub (the hermesSend switch) and on
	// the host (its own switch, and the paste guard).
	if e.HermesView() {
		v.Hermes, v.HermesSend = true, h.HermesSend && h.Interactive
	}
	if !v.Hermes || v.HermesSend {
		v.Queue = s.mirrorComposerQueue(r.Context(), h, e, sendQueueEditing(r))
	}
	v.Notice, v.NoticeOK = composerNotice(r)
	v.HostIcon = s.mirrorHostIcon(r.Context(), h.Host)
	// The window's sessions' open attention asks, answerable here
	// (attention_asker.go).
	v.Asks, v.AsksSrc = s.askerAsksBlock(r.Context(), askerRef{mirror: ui.MirrorRef{Host: h.Host, Session: e.Session, Window: e.Window}})
	allowSameOriginFrame(w)
	formDocument(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.MirrorViewPage(v).Render(w); err != nil {
		s.logger.Printf("mirror: render view page: %v", err)
	}
}

// mirrorViewBatch is one socket message: a host batch, rendered.
type mirrorViewBatch struct {
	Type   string        `json:"type"` // "batch"
	Reset  bool          `json:"reset,omitempty"`
	Gen    string        `json:"gen,omitempty"`
	Offset int64         `json:"offset"`
	Busy   bool          `json:"busy"`
	Ops    []ui.MirrorOp `json:"ops"`
	// Index is the transcript's prompt index (mirror.Msg.PromptIndex), bounded
	// again here; absent from a host that predates it.
	Index *ui.MirrorIndex `json:"index,omitempty"`
	// Context is the header's context-usage readout, present only when this
	// batch changed it (ui.MirrorTranscript.TakeContext).
	Context *ui.ContextView `json:"context,omitempty"`
}

// handleMirrorViewWS is the view's socket: ?gen=&offset= resume.
func (s *Server) handleMirrorViewWS(w http.ResponseWriter, r *http.Request) {
	if s.ext.Mirror == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	host, session, window := q.Get("host"), q.Get("session"), q.Get("window")
	if _, _, _, _, ok := s.mirrorEntry(r, host, session, window); !ok {
		http.NotFound(w, r)
		return
	}
	gen := q.Get("gen")
	offset, _ := strconv.ParseInt(q.Get("offset"), 10, 64)

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: s.wsAllowedOrigins(r)})
	if err != nil {
		s.logger.Printf("mirror: view ws accept: %v", err)
		return
	}
	defer c.CloseNow()
	ctx, done, ok := s.terminals.add(r.Context())
	if !ok {
		_ = c.Close(websocket.StatusGoingAway, "muster is shutting down")
		return
	}
	defer done()
	// The browser sends nothing; CloseRead ends ctx when it goes away.
	ctx = c.CloseRead(ctx)

	var drawn ui.MirrorTranscript
	// A reconnect that resumes (gen + offset) draws onto a page that already
	// holds the conversation so far; see ui.MirrorTranscript.Resumed.
	drawn.Resumed = gen != "" && offset > 0
	// The window's questions changed (mirror_ask.go): tell the page at once,
	// so a question shows here the moment its hook registers it. The page's
	// own poll is the fallback.
	if h, e, _, _, ok := s.mirrorEntry(r, host, session, window); ok && e.ClaudeSession != "" {
		changed, stop := s.askSubscribe(askWatchKey{requestOwner(r.Context()), h.Host, e.ClaudeSession})
		defer stop()
		safeGo(s.logger, "mirror: tell a rendered view its questions changed", func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-changed:
					_ = terminalWriteJSON(ctx, c, map[string]string{"type": "asks"})
				}
			}
		})
	}
	err = s.ext.Mirror.WatchWindow(ctx, host, session, window, gen, offset, func(m mirror.Msg) error {
		if m.Reset {
			drawn.Reset()
		}
		// A host that reports subagents says so before it draws an Agent call
		// (the Reset batch, a resume's marker): those calls are then cards.
		if m.SubagentList {
			drawn.Subagents = true
		}
		b := mirrorViewBatch{Type: "batch", Reset: m.Reset, Gen: m.Gen, Offset: m.Offset, Busy: m.Busy, Ops: drawn.Ops(m.Events)}
		if m.SubagentList && len(m.Subagents) > 0 {
			b.Ops = append(b.Ops, drawn.SubagentOps(m.Subagents)...)
		}
		// After Ops: Ops is what records the usage TakeContext reports.
		b.Context = drawn.TakeContext()
		if m.PromptIndex {
			refs, dropped := mirror.BoundPrompts(m.Prompts)
			b.Index = ui.NewMirrorIndex(refs, m.PromptsMore+dropped)
		}
		return terminalWriteJSON(ctx, c, b)
	})
	if ctx.Err() != nil {
		return
	}
	msg := "the rendered view ended"
	switch {
	case err != nil && mirror.IsAttachRefusal(err):
		msg = err.Error()
	case err != nil:
		msg = "the rendered view ended with an error: " + err.Error()
		s.logger.Printf("mirror: view %s/%s: %v", host, session, err)
	}
	_ = terminalWriteJSON(ctx, c, map[string]string{"type": "error", "message": msg})
	_ = c.Close(websocket.StatusNormalClosure, "view ended")
}

// refuseStale answers a send whose sign-in is not fresh: 403, the gate's own
// text, and a header the page reads to ask "Confirm it's you" and retry.
func refuseStale(w http.ResponseWriter) {
	w.Header().Set("X-Muster-Step-Up", "required")
	http.Error(w, stepup.ErrStale.Error(), http.StatusForbidden)
}

// mirrorSendTimeout bounds one send end to end (the host confirms the paste).
// It is above the hub's wait for a Hermes window's gated send (three Escapes
// at 1.5 s, the paste and its checks, up to 8 s for the confirming row).
const mirrorSendTimeout = 50 * time.Second

// handleMirrorViewSend is POST /mirror/view/send (host, session, window,
// text): paste the text into the window's CLAUDE pane, the way ccd hands an
// agent a prompt — and only into a pane that runs Claude Code.
func (s *Server) handleMirrorViewSend(w http.ResponseWriter, r *http.Request) {
	s.mirrorSend(w, r, mirror.PasteClaude)
}

// handleMirrorTerminalSend is POST /mirror/terminal/send (host, session,
// window, text): paste the text into the window's ACTIVE pane, whatever runs in
// it — the Terminal view's composer, for a session with no rendered view (a
// shell, another TUI) as much as for one with.
func (s *Server) handleMirrorTerminalSend(w http.ResponseWriter, r *http.Request) {
	s.mirrorSend(w, r, mirror.PasteActivePane)
}

// SendAgainHeader is ui.SendAgainHeader: one spelling, which the composer's
// script reads.
const SendAgainHeader = ui.SendAgainHeader

// MirrorSendUnconfirmedRepeat answers a send whose nonce was already handed
// to the host and never confirmed: it may be in the conversation already.
const MirrorSendUnconfirmedRepeat = "Not sent again: the first try may already have been submitted, and muster never repeats it on its own. Check the conversation; to send it anyway, use Send again."

// MirrorSendUnconfirmed answers a send the host never confirmed (a Claude Code
// window's; a Hermes window's says it in the host's own words).
const MirrorSendUnconfirmed = "May have been sent: muster handed it to the host, which did not confirm it. Check the conversation before sending again; to send it anyway, use Send again."

// errUnconfirmedRepeat: the same message, by its nonce, after a delivery the
// host never confirmed (MirrorSendUnconfirmedRepeat). Nothing was sent.
var errUnconfirmedRepeat = errors.New(MirrorSendUnconfirmedRepeat)

// MirrorSendEmpty answers a send with no text: 400, said rather than dropped.
const MirrorSendEmpty = "nothing to send: type a message first"

// The plain confirm page's route, and the values of its `kind` field: which of
// the two sends is waiting.
const (
	mirrorConfirmSendPath = "/mirror/confirm-send"
	mirrorSendKindView    = "view"
	mirrorSendKindTerm    = "terminal"
)

// mirrorSendKind is target as the confirm page's `kind` field.
func mirrorSendKind(target mirror.PasteTarget) string {
	if target == mirror.PasteClaude {
		return mirrorSendKindView
	}
	return mirrorSendKindTerm
}

// mirrorSendPage is the view a send to e at target came from: where the
// browser is sent back to.
func mirrorSendPage(e mirror.SessionView, target mirror.PasteTarget) string {
	if target == mirror.PasteClaude {
		return mirrorRef(e).ViewPath()
	}
	return mirrorRef(e).TerminalPath()
}

// mirrorPending is one send as it arrived.
type mirrorPending struct {
	e      mirror.SessionView // the address the request named (mirrorEntry)
	window string             // as the request named it; "" = the session's active window
	text   string
	nonce  string
	target mirror.PasteTarget
	// nav: the BROWSER submitted the form (see composer.go).
	nav bool
}

// refuse answers a send that is not delivered: to the composer's script the
// status and the reason, to the browser a redirect back to the view with the
// notice code.
func (s *Server) mirrorRefuse(w http.ResponseWriter, r *http.Request, p mirrorPending, status int, msg, notice string) {
	if p.nav {
		http.Redirect(w, r, withNotice(mirrorSendPage(p.e, p.target), notice), http.StatusSeeOther)
		return
	}
	http.Error(w, msg, status)
}

// mirrorStepUpPage answers a browser-submitted send whose sign-in is stale: the
// plain confirm page, carrying the send in hidden fields.
func (s *Server) mirrorStepUpPage(w http.ResponseWriter, p mirrorPending, problem string) {
	fields := []ui.ComposerField{{Name: "kind", Value: mirrorSendKind(p.target)}, {Name: "host", Value: p.e.Host}, {Name: "session", Value: p.e.Session}}
	if p.window != "" {
		fields = append(fields, ui.ComposerField{Name: "window", Value: p.window})
	}
	fields = append(fields, ui.ComposerField{Name: "text", Value: p.text}, ui.ComposerField{Name: "nonce", Value: p.nonce})
	formDocument(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.StepUpFormPage(ui.StepUpFormView{Action: mirrorConfirmSendPath, Fields: fields,
		BackHref: mirrorSendPage(p.e, p.target), Window: stepup.WindowText(), Error: problem}).Render(w); err != nil {
		s.logger.Printf("mirror: render confirm page: %v", err)
	}
}

// mirrorSendForm reads a send's form: its address, resolved against the
// principal's hosts, and its text. ok is false when it answered already (the
// plugin off, a body too large, or an address that is not the principal's —
// 404, never a 403 that would say "this exists").
func (s *Server) mirrorSendForm(w http.ResponseWriter, r *http.Request, target mirror.PasteTarget) (h mirror.HostView, p mirrorPending, ok bool) {
	if s.ext.Mirror == nil {
		http.NotFound(w, r)
		return h, p, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, mirror.MaxPaste*4)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "the message is too large", http.StatusRequestEntityTooLarge)
		return h, p, false
	}
	window := r.PostFormValue("window")
	h, e, _, _, found := s.mirrorEntry(r, r.PostFormValue("host"), r.PostFormValue("session"), window)
	if !found {
		http.NotFound(w, r)
		return h, p, false
	}
	return h, mirrorPending{e: e, window: window, text: r.PostFormValue("text"), nonce: r.PostFormValue("nonce"),
		target: target, nav: isDocumentNavigation(r)}, true
}

// mirrorSend is BOTH send routes: paste the text into a pane of the window and
// press Enter. To the composer's script: 204 on success, otherwise a status
// and the reason as plain text, which it shows inline. To a form the browser
// submitted: a redirect back to the view (composer.go).
//
// 🔴 THE GATES ARE HERE ONCE, FOR BOTH ROUTES, IN THIS ORDER: the plugin on;
// the host is the request principal's (else 404 — never a 403 that would say
// "this exists"); the host advertised -interactive (409); the sign-in is fresh
// (403 + X-Muster-Step-Up, which the page answers with "Confirm it's you" and a
// retry of the same text — or, for a browser-submitted form, the plain confirm
// page). Only then is the text looked at. The plugin checks the last two again
// on its own.
func (s *Server) mirrorSend(w http.ResponseWriter, r *http.Request, target mirror.PasteTarget) {
	h, p, ok := s.mirrorSendForm(w, r, target)
	if !ok {
		return
	}
	if !h.Interactive {
		s.mirrorRefuse(w, r, p, http.StatusConflict, mirror.ErrInteractiveRefused.Error(), noticeReadOnly)
		return
	}
	if !stepup.Fresh(r.Context(), s.stepUpNow()) {
		if p.nav {
			s.mirrorStepUpPage(w, p, "")
			return
		}
		refuseStale(w)
		return
	}
	s.mirrorDeliver(r.Context(), w, r, p)
}

// handleMirrorConfirmSend is POST /mirror/confirm-send: the plain confirm
// page's form — the password again, and the send that was waiting. A right
// password replaces the session with a freshly signed-in one (the SAME check
// and throttle as the dialog's route: stepUpWithPassword) and the send is then
// delivered, in this request; a wrong one shows the page again, saying so.
//
// 🔴 THE SEND IS FRESH BECAUSE THIS REQUEST JUST CONFIRMED, AND ONLY THEN. The
// request arrived with a stale session, so the gate is given the sign-in time
// of the session the confirm minted — whose cookies are on this very response —
// and nothing else: a wrong password delivers nothing, and every other gate
// (the host is the principal's, -interactive, the plugin's own) runs as for any
// send.
func (s *Server) handleMirrorConfirmSend(w http.ResponseWriter, r *http.Request) {
	if s.ext.Mirror == nil {
		http.NotFound(w, r)
		return
	}
	// The kind is read before the form is otherwise looked at: it decides the
	// target mirrorSendForm resolves for.
	r.Body = http.MaxBytesReader(w, r.Body, mirror.MaxPaste*4)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "the message is too large", http.StatusRequestEntityTooLarge)
		return
	}
	var target mirror.PasteTarget
	switch r.PostFormValue("kind") {
	case mirrorSendKindView:
		target = mirror.PasteClaude
	case mirrorSendKindTerm:
		target = mirror.PasteActivePane
	default:
		http.Error(w, "unknown kind of send", http.StatusBadRequest)
		return
	}
	h, p, ok := s.mirrorSendForm(w, r, target)
	if !ok {
		return
	}
	// The page is only ever the browser's: its result is a page or a redirect.
	p.nav = true
	if !h.Interactive {
		s.mirrorRefuse(w, r, p, http.StatusConflict, mirror.ErrInteractiveRefused.Error(), noticeReadOnly)
		return
	}
	// PostForm, never Form: a password in the URL would be in the access log.
	o := s.stepUpWithPassword(w, r, r.PostForm.Get("password"))
	if o.Error != "" {
		s.mirrorStepUpPage(w, p, o.Error)
		return
	}
	s.mirrorDeliver(stepup.WithAuthTime(r.Context(), o.At), w, r, p)
}

// handleMirrorViewSendAgain and handleMirrorTerminalSendAgain are GET on the
// two send routes. Nothing in this app links there: it is where a POST lands
// that was sent through a sign-in at a forward-auth edge and came back as a GET
// with no body (ui.ComposerView's action carries the address in its query for
// exactly this). The browser is sent back to the view, told the message was not
// sent.
func (s *Server) handleMirrorViewSendAgain(w http.ResponseWriter, r *http.Request) {
	s.mirrorSendAgain(w, r, mirror.PasteClaude)
}

func (s *Server) handleMirrorTerminalSendAgain(w http.ResponseWriter, r *http.Request) {
	s.mirrorSendAgain(w, r, mirror.PasteActivePane)
}

func (s *Server) mirrorSendAgain(w http.ResponseWriter, r *http.Request, target mirror.PasteTarget) {
	q := r.URL.Query()
	_, e, _, _, ok := s.mirrorEntry(r, q.Get("host"), q.Get("session"), q.Get("window"))
	if !ok {
		// Not the principal's host: not found, like its pages.
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, withNotice(mirrorSendPage(e, target), noticeAgain), http.StatusSeeOther)
}

// mirrorDeliver pastes p's text and answers. ctx carries the sign-in time the
// plugin's own freshness gate reads: the request's, or a just-confirmed one.
func (s *Server) mirrorDeliver(ctx context.Context, w http.ResponseWriter, r *http.Request, p mirrorPending) {
	host, session := p.e.Host, p.e.Session
	if strings.TrimSpace(p.text) == "" {
		s.mirrorRefuse(w, r, p, http.StatusBadRequest, MirrorSendEmpty, noticeEmpty)
		return
	}
	sent := func() {
		if p.nav {
			http.Redirect(w, r, withNotice(mirrorSendPage(p.e, p.target), noticeSent), http.StatusSeeOther)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
	repeat, err := s.mirrorPaste(ctx, host, session, p.window, p.text, p.nonce, p.target)
	if repeat {
		s.logger.Printf("mirror: a repeat of a message to %s/%s%s was not sent again", host, session, p.window)
		sent()
		return
	}
	if err != nil {
		if errors.Is(err, mirror.ErrStepUpRequired) {
			// The plugin's own check of the same gate (it does not rely on the
			// one above): the same answer.
			if p.nav {
				s.mirrorStepUpPage(w, p, "")
				return
			}
			refuseStale(w)
			return
		}
		status, notice, msg := http.StatusBadGateway, noticeFailed, err.Error()
		var notIdle *mirror.HermesNotIdleError
		var pr *mirror.PasteRefusedError
		// 🔴 A send that MAY have landed offers "Send again" (a new nonce): the
		// same nonce is never delivered again (claim's nonceUnconfirmed).
		if errors.Is(err, errUnconfirmedRepeat) || errors.Is(err, mirror.ErrPasteUnconfirmed) {
			w.Header().Set(SendAgainHeader, "1")
		}
		switch {
		case errors.Is(err, errUnconfirmedRepeat):
			status, notice, msg = http.StatusConflict, noticeUnconfirmedRepeat, MirrorSendUnconfirmedRepeat
		case errors.As(err, &notIdle):
			// The hub's idle gate for a Hermes window: nothing was sent.
			status, notice, msg = http.StatusConflict, hermesNotIdleNotices[notIdle.Status], notIdle.Error()
		case errors.As(err, &pr) && hermesSendNotices[pr.Refusal] != "":
			// The host's gated send: its own words, and a notice that says the
			// same when the browser posted the form. Pasted-not-submitted and
			// may-have-been-submitted are said as such, never as "not sent".
			status, notice, msg = http.StatusConflict, hermesSendNotices[pr.Refusal], pr.Why
		case errors.Is(err, mirror.ErrPasteUnconfirmed):
			status, notice, msg = http.StatusBadGateway, noticeUnconfirmed, MirrorSendUnconfirmed
		case mirror.PasteGuardRefusal(err) != "":
			// The host's Hermes paste guard: its own words, without this
			// side's "the host refused the message:" in front, and a notice
			// code that says the same when the browser posted the form.
			var pr *mirror.PasteRefusedError
			errors.As(err, &pr)
			status, notice, msg = http.StatusConflict, pasteGuardNotices[pr.Refusal], pr.Why
		case errors.Is(err, mirror.ErrBadPaste):
			status, notice = http.StatusBadRequest, noticeBadText
		case errors.Is(err, mirror.ErrInteractiveRefused):
			status, notice = http.StatusConflict, noticeReadOnly
		case errors.Is(err, mirror.ErrNotClaude), errors.Is(err, mirror.ErrNoRenderedView):
			status, notice = http.StatusConflict, noticeNotClaude
		case errors.Is(err, mirror.ErrSessionGone):
			status, notice = http.StatusConflict, noticeGone
		case errors.Is(err, mirror.ErrWindowGone):
			status, notice = http.StatusConflict, noticeWindowGone
		case errors.Is(err, mirror.ErrPanePasteUnsupported):
			status, notice = http.StatusConflict, noticeUpgrade
		case errors.Is(err, mirror.ErrHostOffline):
			status, notice = http.StatusServiceUnavailable, noticeOffline
		}
		s.logger.Printf("mirror: send to %s/%s%s refused: %v", host, session, p.window, err)
		s.mirrorRefuse(w, r, p, status, msg, notice)
		return
	}
	s.logger.Printf("mirror: sent a message to %s/%s%s", host, session, p.window)
	// The operator wrote to the session: its open asks are answered here.
	s.mirrorAddressed(ctx, host, p.e, p.target, p.text, 0)
	sent()
}

// mirrorPaste is the DELIVERY itself — what a person's Send (mirrorDeliver) and
// a queued message (sendQueueToMirror) both run once their gates have passed:
// the nonce claim, the bounded paste, and the claim's settlement. repeat is a
// nonce already delivered, which is not pasted again. ctx carries the principal
// whose host it is and the sign-in time the plugin's own freshness gate reads.
//
// 🔴 THE SAME MESSAGE TWICE IS DELIVERED ONCE (sendNonces): the composer's
// script hands a send whose answer was lost to the browser's own submit, and
// the first may have been pasted.
//
// 🔴 EVERY PASTE THAT MAY HAVE LANDED HOLDS THE SEND QUEUE FOR ITS TURN. The
// hub records the window's status time at the paste (mirror's turns.go), and
// this records it on the window's open queue rows too (sendQueueNotePaste), so
// a queued message waits for the turn a person's own Send started — across a
// restart as well.
//
// opts go to the plugin's PasteWindow (a queued message's mirror.ExpectTurn).
// The window id the plugin resolved is what the queue rows are stamped for, so
// a Send to the session's ACTIVE window (window "") holds that window's queue
// too.
func (s *Server) mirrorPaste(ctx context.Context, host, session, window, text, nonce string, target mirror.PasteTarget, opts ...mirror.PasteOption) (repeat bool, err error) {
	resolved := window
	opts = append(opts, mirror.ReportWindow(&resolved))
	repeat, err = s.mirrorPasteOnce(ctx, host, session, window, text, nonce, target, opts...)
	if !repeat && (err == nil || errors.Is(err, mirror.ErrPasteUnconfirmed)) {
		s.sendQueueNotePaste(ctx, host, session, resolved)
	}
	return repeat, err
}

func (s *Server) mirrorPasteOnce(ctx context.Context, host, session, window, text, nonce string, target mirror.PasteTarget, opts ...mirror.PasteOption) (repeat bool, err error) {
	st, finish := s.sends.claim(requestOwner(ctx), nonce, text)
	switch st {
	case nonceRepeat:
		return true, nil
	case nonceUnconfirmed:
		return false, errUnconfirmedRepeat
	}
	pctx, cancel := context.WithTimeout(ctx, mirrorSendTimeout)
	defer cancel()
	// window == "" is the session's active window (mirror.Hub.PasteWindow).
	err = s.ext.Mirror.PasteWindow(pctx, host, session, window, text, target, opts...)
	switch {
	case err == nil:
		finish(nonceDelivered)
	case errors.Is(err, mirror.ErrPasteUnconfirmed):
		// 🔴 KEPT, NOT RELEASED: it may be in the conversation already, and
		// the composer keeps this nonce until the person asks to Send again.
		finish(nonceMaybe)
	default:
		finish(nonceFailed)
	}
	return false, err
}
