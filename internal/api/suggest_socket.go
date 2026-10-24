package api

import (
	"context"
	"net/http"
	"net/url"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/suggest"
	"github.com/ZacxDev/muster/internal/ui"
)

// suggestPath is the suggested replies' URL for window e of host h — what the
// Rendered view's chip row reads — or "" when the window can have none: the
// server has no engine, suggestions are off for the viewer (the kill switch,
// their toggle, the auto-pause), the host is read-only, or the window is not a
// Claude Code pane with a session (Hermes, a plain terminal).
func (s *Server) suggestPath(ctx context.Context, h mirror.HostView, e mirror.SessionView) string {
	if s.ext.Suggest == nil || !h.Interactive {
		return ""
	}
	if !e.Claude || e.ClaudeSession == "" || e.HermesView() || e.Runtime != "" || !e.HasRenderedView() {
		return ""
	}
	if s.ext.Suggest.Enabled(ctx, requestOwner(ctx)) != "" {
		return ""
	}
	q := url.Values{"host": {h.Host}, "session": {e.Session}}
	if e.Window != "" {
		q.Set("window", e.Window)
	}
	return ui.SuggestPath + "?" + q.Encode()
}

// suggestPaused reports whether window e of host h would have suggestions
// but for the viewer's auto-pause: the composer then says so in one line.
func (s *Server) suggestPaused(ctx context.Context, h mirror.HostView, e mirror.SessionView) bool {
	if s.ext.Suggest == nil || !h.Interactive || s.ext.Suggest.Off {
		return false
	}
	if !e.Claude || e.ClaudeSession == "" || e.HermesView() || e.Runtime != "" || !e.HasRenderedView() {
		return false
	}
	return s.ext.Suggest.Enabled(ctx, requestOwner(ctx)) == suggest.OutcomePaused
}

// suggestSocket is one rendered view socket's suggestions: its tracker, the
// one-at-a-time gate of its turn ends, and the socket it tells the page on.
// A nil *suggestSocket (the window can have none) does nothing.
type suggestSocket struct {
	s                     *Server
	ctx                   context.Context
	c                     *websocket.Conn
	host, session, window string
	t                     *suggestTracker
	gate                  chan struct{}
}

// suggestSocket is the view socket c's suggestions for (host, session,
// window), or nil when the window can have none (suggestPath). resumed: the
// socket resumed at an offset (suggestTracker.resumed).
func (s *Server) suggestSocket(ctx context.Context, r *http.Request, c *websocket.Conn, host, session, window string, resumed bool) *suggestSocket {
	if s.ext.Suggest == nil || s.ext.Mirror == nil {
		return nil
	}
	h, e, _, _, ok := s.mirrorEntry(r, host, session, window)
	if !ok || s.suggestPath(ctx, h, e) == "" {
		return nil
	}
	k := &suggestSocket{s: s, ctx: ctx, c: c, host: host, session: session, window: window, gate: make(chan struct{}, 1)}
	k.t = &suggestTracker{resumed: resumed}
	return k
}

// observe feeds m to the tracker: a turn end starts its generation; a status
// that moved on without ending a turn (busy, a prompt) tells the page that
// the chips it shows are no longer for this turn.
func (k *suggestSocket) observe(m mirror.Msg) {
	if k == nil {
		return
	}
	before := k.t.token()
	if k.t.observe(m) {
		k.s.suggestOnTurnEnd(k.ctx, k.host, k.session, k.window, k.t, k.t.lastEnd(), k.gate, k.tell)
		return
	}
	if after := k.t.token(); before != 0 && after != before {
		k.tell(suggestSocketMsg{Type: "suggest", State: "none"})
	}
}

func (k *suggestSocket) tell(m suggestSocketMsg) { _ = terminalWriteJSON(k.ctx, k.c, m) }
