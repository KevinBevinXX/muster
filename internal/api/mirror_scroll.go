package api

import (
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
)

// DRAG-TO-SCROLL on a mirrored session's Terminal view: POST
// /mirror/terminal/scroll (host, session, window, lines). The host scrolls the
// window's active pane through its tmux history in copy-mode
// (internal/mirror's scroll.go).
//
// 🔴 NO STEP-UP, BY THE OPERATOR'S DECISION, AND THIS IS WHY IT IS SAFE: a
// scroll puts the pane in tmux COPY-MODE and moves it, and while a pane is in
// copy-mode tmux sends its input to copy-mode, not to the program. The host
// runs only `copy-mode -e` and copy-mode's own scroll-up / scroll-down /
// cancel, so nothing reaches the program — unlike a paste or a live key, which is why
// those need a fresh sign-in and this does not. What it does need: a signed-in
// session of the page's own tier, a same-origin request (requireOperatorWrite),
// a host the principal owns that advertised scrolling and is -interactive, a
// line count mirror.ClampScroll accepts, and this route's rate limit.

// scrollRateBurst and scrollRateWindow bound one user's scroll requests. The
// limit must sit ABOVE the page's own fastest cadence, because a 429 costs the
// page a retry (internal/ui's terminal script puts the lines back and sends
// them again). A page sends at most one request per animation frame and only
// one at a time (it waits for each answer), so on a 120 Hz screen with an
// instant round trip it sends 120 a second. The window is fixed, not sliding,
// so the limit is twice that: a page at full speed is never refused, and a
// script sending without waiting for answers is.
const (
	scrollRateBurst  = 240
	scrollRateWindow = time.Second
)

// keyedRate is a rateLimiter per key (a user id).
type keyedRate struct {
	mu sync.Mutex
	by map[int64]*rateLimiter
}

func (k *keyedRate) allow(key int64, now time.Time, burst int, window time.Duration) bool {
	k.mu.Lock()
	if k.by == nil {
		k.by = map[int64]*rateLimiter{}
	}
	l := k.by[key]
	if l == nil {
		// Bounded: a key whose window has long passed holds nothing worth
		// keeping, so a map grown past a few hundred users is swept.
		if len(k.by) >= 512 {
			for id, old := range k.by {
				old.mu.Lock()
				stale := now.Sub(old.windowStart) >= window
				old.mu.Unlock()
				if stale {
					delete(k.by, id)
				}
			}
		}
		l = &rateLimiter{}
		k.by[key] = l
	}
	k.mu.Unlock()
	return l.allow(now, burst, window)
}

// MirrorScrollUnsupported is the 409's text when the host cannot scroll.
var MirrorScrollUnsupported = unprefixed(mirror.ErrScrollUnsupported)

func (s *Server) handleMirrorTerminalScroll(w http.ResponseWriter, r *http.Request) {
	if s.ext.Mirror == nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "the request is too large", http.StatusRequestEntityTooLarge)
		return
	}
	n, err := strconv.Atoi(r.PostFormValue("lines"))
	lines, ok := mirror.ClampScroll(n)
	if err != nil || !ok {
		http.Error(w, "lines must be a non-zero whole number", http.StatusBadRequest)
		return
	}
	var user int64
	if p, ok := authz.From(r.Context()); ok {
		user = p.UserID
	}
	if !s.scrollRate.allow(user, time.Now(), scrollRateBurst, scrollRateWindow) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "scrolling too fast", http.StatusTooManyRequests)
		return
	}
	window := r.PostFormValue("window")
	_, e, listed, _, found := s.mirrorEntry(r, r.PostFormValue("host"), r.PostFormValue("session"), window)
	if !found || !listed {
		http.NotFound(w, r)
		return
	}
	if !e.CanScroll() {
		http.Error(w, MirrorScrollUnsupported, http.StatusConflict)
		return
	}
	err = s.ext.Mirror.ScrollWindow(r.Context(), e.Host, e.Session, window, lines)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, mirror.ErrHostOffline), errors.Is(err, mirror.ErrSessionGone), errors.Is(err, mirror.ErrWindowGone),
		errors.Is(err, mirror.ErrHostDisconnected):
		http.NotFound(w, r)
	case errors.Is(err, mirror.ErrScrollUnsupported), errors.Is(err, mirror.ErrScrollRefused), errors.Is(err, mirror.ErrBadScroll):
		http.Error(w, unprefixed(err), http.StatusConflict)
	case errors.Is(err, mirror.ErrScrollTimeout):
		http.Error(w, unprefixed(err), http.StatusGatewayTimeout)
	default:
		s.logger.Printf("mirror: scroll %s/%s%s (%d lines): %v", e.Host, e.Session, window, lines, err)
		http.Error(w, "the scroll did not reach the host", http.StatusBadGateway)
	}
}
