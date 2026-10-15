package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/ZacxDev/muster/internal/pins"
)

// registerPinRoutes wires the Agents list's pin toggles (internal/pins,
// migration 0017): POST ui.PinPath and ui.UnpinPath, form-encoded, naming one
// subject — kind=agent&agent_id=N, or kind=mirror&host=&session=&window= (the
// window "" for a per-session host's entry).
//
// 🔴 BROWSER-SESSION ROUTES, CSRF-CHECKED: requireOperatorWrite is
// requireSession (so on the machine listener they answer 403 like every other
// browser route — refuseOffBrowserListener) plus requireSameOrigin. Both are
// idempotent: pinning a pinned entry, or unpinning an unpinned one, succeeds.
//
// The answer is the re-rendered list for an htmx post (hx-target #agents-list)
// and a 303 back to /agents for a non-htmx form post. That second branch has no
// user-reachable purpose today: #agents-list and its cards are loaded only by
// htmx, so a page without JavaScript draws no toggle to post. It is a route
// behaviour (TestThePinRoutesPinAndUnpinIdempotently), not a no-JS path.
func (s *Server) registerPinRoutes(mux Mux) {
	// Spelled literally (ui.PinPath / ui.UnpinPath are the same strings): the
	// route walks in this package's tests read the pattern from the source.
	mux.HandleFunc("POST /agents/pins", s.requireOperatorWrite(s.handlePin))
	mux.HandleFunc("POST /agents/pins/delete", s.requireOperatorWrite(s.handleUnpin))
}

func (s *Server) handlePin(w http.ResponseWriter, r *http.Request)   { s.handlePinToggle(w, r, true) }
func (s *Server) handleUnpin(w http.ResponseWriter, r *http.Request) { s.handlePinToggle(w, r, false) }

func (s *Server) handlePinToggle(w http.ResponseWriter, r *http.Request, pin bool) {
	if s.ext.Pins == nil || s.ext.Agents == nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	var err error
	switch r.PostFormValue("kind") {
	case "agent":
		id, perr := strconv.ParseInt(r.PostFormValue("agent_id"), 10, 64)
		if perr != nil || id <= 0 {
			http.Error(w, "bad agent_id", http.StatusBadRequest)
			return
		}
		if pin {
			err = s.ext.Pins.PinAgent(ctx, id)
		} else {
			err = s.ext.Pins.UnpinAgent(ctx, id)
		}
	case "mirror":
		k := pins.MirrorKey{Host: r.PostFormValue("host"), Session: r.PostFormValue("session"), Window: r.PostFormValue("window")}
		if verr := k.Valid(); verr != nil {
			http.Error(w, verr.Error(), http.StatusBadRequest)
			return
		}
		if pin {
			err = s.ext.Pins.PinMirror(ctx, k)
		} else {
			err = s.ext.Pins.UnpinMirror(ctx, k)
		}
	default:
		http.Error(w, "kind must be agent or mirror", http.StatusBadRequest)
		return
	}
	switch {
	case errors.Is(err, pins.ErrNoSuchAgent):
		http.Error(w, "agent not found", http.StatusNotFound)
		return
	case errors.Is(err, pins.ErrTooMany):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		s.logger.Printf("pins: %v", err)
		http.Error(w, "could not save the pin", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("HX-Request") == "" {
		http.Redirect(w, r, "/agents", http.StatusSeeOther)
		return
	}
	s.handleAgentsContent(w, r)
}

// pinSet is the viewing principal's pins, read once per render. No store, or a
// failed read (logged), is "nothing pinned": the list still renders, in its
// unpinned order.
func (s *Server) pinSet(ctx context.Context) pins.Set {
	if s.ext.Pins == nil {
		return pins.Set{}
	}
	set, err := s.ext.Pins.List(ctx)
	if err != nil {
		s.logger.Printf("pins: list: %v", err)
		return pins.Set{}
	}
	return set
}
