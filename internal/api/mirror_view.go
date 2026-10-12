package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE RENDERED VIEW of a mirrored session whose pane runs Claude Code (tunnel
// v2): the page, its transcript socket, and its composer.
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
// FRESH STEP-UP (Extensions.MirrorStepUp — nil refuses every send).
// ---------------------------------------------------------------------------

func (s *Server) registerMirrorViewRoutes(mux Mux) {
	mux.HandleFunc("GET /mirror/view", s.requireSession(s.handleMirrorViewPage))
	mux.HandleFunc("GET /mirror/view/ws", s.requireSession(s.handleMirrorViewWS))
	mux.HandleFunc("POST "+ui.MirrorSendPath, s.requireOperatorWrite(s.handleMirrorViewSend))
}

// hasRenderedView reports whether h serves session's rendered view: a v2 host,
// connected, reporting Claude Code in the session's pane.
func hasRenderedView(h mirror.HostView, session string) bool {
	return h.Online && h.Rendered && slices.Contains(h.Claude, session)
}

// handleMirrorViewPage is GET /mirror/view?host=…&session=…. A session with no
// rendered view (a v1 host, an offline one, a pane not running Claude Code)
// goes to its terminal instead, which says why it cannot attach if it cannot.
func (s *Server) handleMirrorViewPage(w http.ResponseWriter, r *http.Request) {
	host, session := r.URL.Query().Get("host"), r.URL.Query().Get("session")
	h, ok := s.mirrorHost(r.Context(), host)
	if !ok || mirror.ValidSession(session) != nil {
		http.NotFound(w, r)
		return
	}
	if !hasRenderedView(h, session) {
		http.Redirect(w, r, ui.MirrorTerminalPath(h.Host, session), http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.MirrorViewPage(ui.MirrorViewView{Host: h.Host, Session: session, Interactive: h.Interactive}).Render(w); err != nil {
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
	host, session := q.Get("host"), q.Get("session")
	if _, ok := s.mirrorHost(r.Context(), host); !ok || mirror.ValidSession(session) != nil {
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
	err = s.ext.Mirror.Watch(ctx, host, session, gen, offset, func(m mirror.Msg) error {
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

// errStepUpRequired is the send refusal while the step-up is not fresh.
var errStepUpRequired = errors.New("step-up required: re-authenticate to send into this session")

// stepUpFresh asks the shared gate; with none wired, the answer is no.
func (s *Server) stepUpFresh(r *http.Request) bool {
	return s.ext.MirrorStepUp != nil && s.ext.MirrorStepUp.StepUpFresh(r)
}

// mirrorSendTimeout bounds one send end to end (the host confirms the paste).
const mirrorSendTimeout = 20 * time.Second

// handleMirrorViewSend is POST /mirror/view/send (host, session, text): paste
// the text into the session's Claude pane, the way ccd hands an agent a prompt.
// 204 on success; otherwise a status and the reason as plain text, which the
// page shows.
func (s *Server) handleMirrorViewSend(w http.ResponseWriter, r *http.Request) {
	if s.ext.Mirror == nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, mirror.MaxPaste*4)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "the message is too large", http.StatusRequestEntityTooLarge)
		return
	}
	host, session, text := r.PostFormValue("host"), r.PostFormValue("session"), r.PostFormValue("text")
	h, ok := s.mirrorHost(r.Context(), host)
	if !ok || mirror.ValidSession(session) != nil {
		http.NotFound(w, r)
		return
	}
	if !h.Interactive {
		http.Error(w, mirror.ErrInteractiveRefused.Error(), http.StatusConflict)
		return
	}
	if !s.stepUpFresh(r) {
		w.Header().Set("X-Muster-Step-Up", "required")
		http.Error(w, errStepUpRequired.Error(), http.StatusForbidden)
		return
	}
	if strings.TrimSpace(text) == "" {
		http.Error(w, "nothing to send", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), mirrorSendTimeout)
	defer cancel()
	if err := s.ext.Mirror.Paste(ctx, host, session, text); err != nil {
		status := http.StatusBadGateway
		switch {
		case errors.Is(err, mirror.ErrBadPaste):
			status = http.StatusBadRequest
		case errors.Is(err, mirror.ErrInteractiveRefused), errors.Is(err, mirror.ErrNotClaude),
			errors.Is(err, mirror.ErrNoRenderedView), errors.Is(err, mirror.ErrSessionGone):
			status = http.StatusConflict
		case errors.Is(err, mirror.ErrHostOffline):
			status = http.StatusServiceUnavailable
		}
		s.logger.Printf("mirror: send to %s/%s refused: %v", host, session, err)
		http.Error(w, err.Error(), status)
		return
	}
	s.logger.Printf("mirror: sent a message to %s/%s", host, session)
	w.WriteHeader(http.StatusNoContent)
}
