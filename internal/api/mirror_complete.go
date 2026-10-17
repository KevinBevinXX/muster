package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE COMPOSER MENU'S DATA (ui.MirrorCompletePath): what a mirrored window's
// message bar offers after `/` (Claude Code's built-in commands, and the
// commands and skills the window's host reports — user, plugin and the
// window's PROJECT) and after `:` (the host's snippets).
//
// 🔴 OWNER-SCOPED LIKE EVERY MIRROR ROUTE: the window is resolved through
// mirrorEntry — the request principal's hosts only — so another user's host,
// and a window its own host does not list, are a 404, and the plugin keys its
// lookup on the same principal again. Only the host's owner ever sees what that
// host reports.
//
// 🔴 IT ALWAYS ANSWERS WITH THE BUILT-INS. A host that cannot say — an older
// muster-mirror, an offline host, one that timed out — is not an error to the
// page: the built-in commands are still the menu, and Note says why nothing of
// the host's is in it.
//
// The page draws every string as TEXT. encoding/json escapes `<`, `>` and `&`
// as well, so even a page that mishandled it could not be handed markup.
// ---------------------------------------------------------------------------

// mirrorCatalog is the plugin's catalog request, when the plugin has one.
// *mirror.Service does; a Mirror without it (an older test double) is a
// plugin whose hosts report nothing, and the menu is the built-ins.
type mirrorCatalog interface {
	CatalogWindow(ctx context.Context, host, session, window string) (mirror.Catalog, error)
}

// composerMenu is the route's answer.
type composerMenu struct {
	Commands []mirror.CatalogEntry `json:"commands"`
	Snippets []mirror.Snippet      `json:"snippets"`
	// Host: the host's own catalog is in it (not the built-ins alone).
	Host bool `json:"host"`
	// Truncated: the host's catalog was cut to the wire's bounds.
	Truncated bool `json:"truncated,omitempty"`
	// Note says why the host's commands are missing, "" when they are not.
	Note string `json:"note,omitempty"`
}

// The notes the menu shows under a built-ins-only list.
const (
	composerMenuNoteOld     = "Built-in commands only: this host's muster-mirror does not report its skills, commands or snippets — upgrade it."
	composerMenuNoteOffline = "Built-in commands only: the host is offline."
	composerMenuNoteFailed  = "Built-in commands only: the host did not send its skills, commands or snippets."
)

func (s *Server) registerMirrorCompleteRoutes(mux Mux) {
	mux.HandleFunc("GET "+ui.MirrorCompletePath, s.requireSession(s.handleMirrorComplete))
}

// handleMirrorComplete is GET ui.MirrorCompletePath?host=…&session=…[&window=…].
func (s *Server) handleMirrorComplete(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	h, e, listed, _, ok := s.mirrorEntry(r, q.Get("host"), q.Get("session"), q.Get("window"))
	if !ok || !listed {
		http.NotFound(w, r)
		return
	}
	var host mirror.Catalog
	out := composerMenu{}
	cat, can := s.ext.Mirror.(mirrorCatalog)
	switch {
	case !h.Online:
		out.Note = composerMenuNoteOffline
	case !can:
		out.Note = composerMenuNoteOld
	default:
		c, err := cat.CatalogWindow(r.Context(), h.Host, e.Session, e.Window)
		switch {
		case err == nil:
			host, out.Host, out.Truncated = c, true, c.Truncated
		case errors.Is(err, mirror.ErrCatalogUnsupported):
			out.Note = composerMenuNoteOld
		case errors.Is(err, mirror.ErrHostOffline):
			out.Note = composerMenuNoteOffline
		default:
			s.logger.Printf("mirror: composer menu of %s/%s%s: %v", h.Host, e.Session, e.Window, err)
			out.Note = composerMenuNoteFailed
		}
	}
	merged := mirror.MergeBuiltins(host)
	out.Commands, out.Snippets = merged.Commands, merged.Snippets
	out.Truncated = out.Truncated || merged.Truncated
	if out.Commands == nil {
		out.Commands = []mirror.CatalogEntry{}
	}
	if out.Snippets == nil {
		out.Snippets = []mirror.Snippet{}
	}
	// The owner's own host content: never kept by a shared cache.
	w.Header().Set("Cache-Control", "private, no-store")
	s.writeJSON(w, http.StatusOK, out)
}
