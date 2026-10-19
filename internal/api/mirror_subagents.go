package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// AN OPENED SUBAGENT CARD'S STEPS (ui.MirrorSubagentPath): the recent steps of
// one subagent of a mirrored window's Claude Code session, as the HTML
// fragment the card puts in its tail (internal/mirror's subagents.go).
//
// 🔴 OWNER-SCOPED LIKE EVERY MIRROR ROUTE: the window is resolved through
// mirrorEntry — the request principal's hosts only — so another user's host is
// a 404, and the plugin keys its own lookup on the same principal again. The
// host reads only the subagents of the transcript that window's pane runs NOW.
//
// It always answers 200 with a fragment for a window the principal has: a host
// that cannot say (older, offline, timed out) is a note in the card, not an
// error page inside it.
// ---------------------------------------------------------------------------

// mirrorSubagents is the plugin's tail request, when it has one.
type mirrorSubagents interface {
	SubagentTail(ctx context.Context, host, session, window, agent string) (mirror.SubagentTail, error)
}

// The notes a card shows instead of steps.
const (
	subagentNoteOld     = "This host's muster-mirror does not report subagents' steps — upgrade it."
	subagentNoteOffline = "The host is offline."
	subagentNoteFailed  = "Couldn't read its steps from the host."
)

func (s *Server) handleMirrorSubagent(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	h, e, listed, _, ok := s.mirrorEntry(r, q.Get("host"), q.Get("session"), q.Get("window"))
	if !ok || !listed {
		http.NotFound(w, r)
		return
	}
	agent := q.Get("agent")
	if !mirror.ValidSubagentID(agent) {
		http.Error(w, "not a subagent id", http.StatusBadRequest)
		return
	}
	var v ui.SubagentTailView
	sub, can := s.ext.Mirror.(mirrorSubagents)
	switch {
	case !h.Online:
		v.Note = subagentNoteOffline
	case !can:
		v.Note = subagentNoteOld
	default:
		t, err := sub.SubagentTail(r.Context(), h.Host, e.Session, e.Window, agent)
		switch {
		case err == nil:
			v.Tail = t
		case errors.Is(err, mirror.ErrSubagentsUnsupported):
			v.Note = subagentNoteOld
		case errors.Is(err, mirror.ErrHostOffline):
			v.Note = subagentNoteOffline
		default:
			s.logger.Printf("mirror: subagent %s of %s/%s%s: %v", agent, h.Host, e.Session, e.Window, err)
			v.Note = subagentNoteFailed
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The owner's own transcript text: never kept by a shared cache.
	w.Header().Set("Cache-Control", "private, no-store")
	if err := ui.SubagentTailFragment(v).Render(w); err != nil {
		s.logger.Printf("mirror: render subagent steps: %v", err)
	}
}
