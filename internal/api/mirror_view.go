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
// through mirrorHost (the request principal's hosts only), so another user's
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
	mux.HandleFunc("POST "+ui.MirrorSendPath, s.requireOperatorWrite(s.handleMirrorViewSend))
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
		http.Redirect(w, r, mirrorRef(e).TerminalPath(), http.StatusSeeOther)
		return
	}
	if redirect {
		http.Redirect(w, r, mirrorRef(e).ViewPath(), http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.MirrorViewPage(ui.MirrorViewView{Host: h.Host, Session: e.Session, Window: e.Window, Title: mirrorTitle(e), Label: e.Label, Interactive: h.Interactive,
		StepUp: s.stepUpView(r, "")}).Render(w); err != nil {
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
	err = s.ext.Mirror.WatchWindow(ctx, host, session, window, gen, offset, func(m mirror.Msg) error {
		if m.Reset {
			drawn.Reset()
		}
		return terminalWriteJSON(ctx, c, mirrorViewBatch{Type: "batch", Reset: m.Reset, Gen: m.Gen,
			Offset: m.Offset, Busy: m.Busy, Ops: drawn.Ops(m.Events)})
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
const mirrorSendTimeout = 20 * time.Second

// handleMirrorViewSend is POST /mirror/view/send (host, session, text): paste
// the text into the session's CLAUDE pane, the way ccd hands an agent a prompt
// — and only into a pane that runs Claude Code.
func (s *Server) handleMirrorViewSend(w http.ResponseWriter, r *http.Request) {
	s.mirrorSend(w, r, mirror.PasteClaude)
}

// handleMirrorTerminalSend is POST /mirror/terminal/send (host, session, text):
// paste the text into the session's ACTIVE pane, whatever runs in it — the
// Terminal view's composer, for a session with no rendered view (a shell,
// another TUI) as much as for one with.
func (s *Server) handleMirrorTerminalSend(w http.ResponseWriter, r *http.Request) {
	s.mirrorSend(w, r, mirror.PasteActivePane)
}

// MirrorSendEmpty answers a send with no text: 400, said rather than dropped.
const MirrorSendEmpty = "nothing to send: type a message first"

// mirrorSend is BOTH send routes: paste the text into a pane of the session and
// press Enter. 204 on success; otherwise a status and the reason as plain text,
// which the composer shows inline.
//
// 🔴 THE GATES ARE HERE ONCE, FOR BOTH ROUTES, IN THIS ORDER: the plugin on;
// the host is the request principal's (else 404 — never a 403 that would say
// "this exists"); the host advertised -interactive (409); the sign-in is fresh
// (403 + X-Muster-Step-Up, which the page answers with "Confirm it's you" and a
// retry of the same text). Only then is the text looked at. The plugin checks
// the last two again on its own.
func (s *Server) mirrorSend(w http.ResponseWriter, r *http.Request, target mirror.PasteTarget) {
	if s.ext.Mirror == nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, mirror.MaxPaste*4)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "the message is too large", http.StatusRequestEntityTooLarge)
		return
	}
	host, session, window, text := r.PostFormValue("host"), r.PostFormValue("session"), r.PostFormValue("window"), r.PostFormValue("text")
	h, _, _, _, ok := s.mirrorEntry(r, host, session, window)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !h.Interactive {
		http.Error(w, mirror.ErrInteractiveRefused.Error(), http.StatusConflict)
		return
	}
	if !stepup.Fresh(r.Context(), s.stepUpNow()) {
		refuseStale(w)
		return
	}
	if strings.TrimSpace(text) == "" {
		http.Error(w, MirrorSendEmpty, http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), mirrorSendTimeout)
	defer cancel()
	// window == "" is the session's active window (mirror.Hub.PasteWindow).
	if err := s.ext.Mirror.PasteWindow(ctx, host, session, window, text, target); err != nil {
		if errors.Is(err, mirror.ErrStepUpRequired) {
			// The plugin's own check of the same gate (it does not rely on the
			// one above): the same answer.
			refuseStale(w)
			return
		}
		status := http.StatusBadGateway
		switch {
		case errors.Is(err, mirror.ErrBadPaste):
			status = http.StatusBadRequest
		case errors.Is(err, mirror.ErrInteractiveRefused), errors.Is(err, mirror.ErrNotClaude),
			errors.Is(err, mirror.ErrNoRenderedView), errors.Is(err, mirror.ErrSessionGone),
			errors.Is(err, mirror.ErrWindowGone), errors.Is(err, mirror.ErrPanePasteUnsupported):
			status = http.StatusConflict
		case errors.Is(err, mirror.ErrHostOffline):
			status = http.StatusServiceUnavailable
		}
		s.logger.Printf("mirror: send to %s/%s%s refused: %v", host, session, window, err)
		http.Error(w, err.Error(), status)
		return
	}
	s.logger.Printf("mirror: sent a message to %s/%s%s", host, session, window)
	w.WriteHeader(http.StatusNoContent)
}
