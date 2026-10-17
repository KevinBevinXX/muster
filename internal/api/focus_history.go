package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// FOCUS HISTORY's route (internal/focus, migration 0022): one page of a
// session's past focus, newest first, as list rows (ui.RenderFocusHistoryPage)
// for the rendered view's "Focus history" tab and an agent page's dialog.
//
// 🔴 OWNER-SCOPED TWICE, AND THE SECOND IS WHAT MAKES IT A 404. The read runs
// under the caller's principal, so agent_focus_history's row-level security
// returns no other owner's row — but "no rows" is also what a session with no
// history yet reads as. So the subject is checked first: an agent the caller
// has (agents' own row-level security), or a mirrored Claude session the caller
// has a focus row or history for, or an online window of theirs shows. Anything else —
// another owner's agent or session, or one that does not exist — is a 404,
// the same answer for both, so the route does not say which ids exist.
// ---------------------------------------------------------------------------

func (s *Server) registerFocusHistoryRoutes(mux Mux) {
	mux.HandleFunc("GET "+ui.FocusHistoryRoute, s.requireSession(s.handleFocusHistory))
}

// focusHistoryPath is sub's newest history page, or "" with no focus store
// (the pages then draw no history control).
func (s *Server) focusHistoryPath(sub focus.Subject) string {
	if s.ext.Focus == nil || sub.Valid() != nil {
		return ""
	}
	return ui.FocusHistoryPath(sub.String(), "")
}

// mirrorFocusHistoryPath is the history of the mirrored window whose Claude
// session is sid; "" when the host reports none.
func (s *Server) mirrorFocusHistoryPath(sid string) string {
	if sid == "" {
		return ""
	}
	return s.focusHistoryPath(focus.MirrorSubject(sid))
}

// handleFocusHistory is GET /ui/focus/history?subject=<kind:key>[&cursor=…].
func (s *Server) handleFocusHistory(w http.ResponseWriter, r *http.Request) {
	if s.ext.Focus == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	sub, err := focus.ParseSubject(q.Get("subject"))
	if err != nil {
		http.Error(w, "bad subject", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	ok, err := s.focusHistoryVisible(ctx, sub)
	if err != nil {
		s.logger.Printf("focus: history %s: %v", sub, err)
		http.Error(w, "could not load the focus history", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	cursor := q.Get("cursor")
	page, err := s.ext.Focus.History(ctx, sub, cursor, focus.DefaultHistoryPage)
	if errors.Is(err, focus.ErrBadCursor) {
		http.Error(w, "bad cursor", http.StatusBadRequest)
		return
	}
	if err != nil {
		s.logger.Printf("focus: history %s: %v", sub, err)
		http.Error(w, "could not load the focus history", http.StatusInternalServerError)
		return
	}
	v := ui.FocusHistoryPageView{First: cursor == ""}
	for _, e := range page.Entries {
		v.Entries = append(v.Entries, ui.FocusHistoryEntry{Title: e.Title, Description: e.Description, At: e.CreatedAt})
	}
	if page.Next != "" {
		v.Next = ui.FocusHistoryPath(sub.String(), page.Next)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := ui.RenderFocusHistoryPage(w, v); err != nil {
		s.logger.Printf("focus: render history %s: %v", sub, err)
	}
}

// focusHistoryVisible reports whether the caller may read sub's history: an
// agent of theirs, or a mirrored Claude session they hold a focus or history
// for, or an online window of theirs shows. A lookup that FAILED (not "no such row") is
// an error, never a "no".
func (s *Server) focusHistoryVisible(ctx context.Context, sub focus.Subject) (bool, error) {
	switch sub.Kind {
	case focus.KindAgent:
		if s.ext.Agents == nil {
			return false, nil
		}
		if _, err := s.ext.Agents.Get(ctx, sub.AgentID()); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	case focus.KindMirror:
		if _, has, err := s.ext.Focus.Get(ctx, sub); err != nil || has {
			return has, err
		}
		// History outlives the focus row (0022): a session whose focus was
		// pruned still has its history to read.
		if p, err := s.ext.Focus.History(ctx, sub, "", 1); err != nil || len(p.Entries) > 0 {
			return len(p.Entries) > 0, err
		}
		_, live := s.mirrorEntryForSession(ctx, sub.Key)
		return live, nil
	}
	return false, nil
}
