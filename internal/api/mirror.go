package api

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// MIRRORED SESSIONS — the optional plugin's HTTP surface (internal/mirror).
//
// 🔴 OFF IS THE DEFAULT, AND OFF MEANS INVISIBLE. With Extensions.Mirror nil
// (MUSTER_MIRROR unset) every route below answers 404 — they are still
// REGISTERED, because registration in this package is unconditional (routes.go)
// — and the Agents view draws no mirrored card.
//
// 🔴 A MIRRORED SESSION IS NOT AN AGENT. It lives in the hub's memory, keyed by
// its host's token OWNER, and is addressed by (host, session) — never by an
// agents row, so no agent route, lookup or predicate (the chief's included) can
// ever see one.
//
// 🔴 IT REUSES THE CLAUDE-CODE TERMINAL, IT DOES NOT FORK IT: the same page
// (ui.AgentTerminalPage) and the same bridge (bridgeTerminal, s.terminals for
// the shutdown drain); only the far side differs — mirror's Attach instead of
// the provisioner's pod exec.
//
// 🔴 OWNER ISOLATION IS THE PLUGIN'S, ON THE REQUEST'S PRINCIPAL. Hosts and
// Attach read authz.From(ctx); a host minted for another user is not in this
// principal's view at all, so its page 404s like a host that does not exist.
// ---------------------------------------------------------------------------

func (s *Server) registerMirrorRoutes(mux Mux) {
	// 🔴 NO SESSION, NO HOOK TOKEN: the plugin authenticates its own mirror
	// tokens and nothing else opens this route (mirror.Service.ServeConnect).
	mux.HandleFunc("GET "+mirror.ConnectPath, s.handleMirrorConnect)
	// The operator's view of a mirrored session: the human tier and ONLY it,
	// exactly like the claude-code terminal (the hook token must not open a
	// terminal). No provisioner is involved, so no provisioner wrapper.
	mux.HandleFunc("GET /mirror/terminal", s.requireSession(s.handleMirrorTerminalPage))
	mux.HandleFunc("GET /mirror/terminal/ws", s.requireSession(s.handleMirrorTerminalWS))
	// "Confirm it's you" (stepup.go): the fresh sign-in an INTERACTIVE attach
	// needs. Registered whether or not the plugin is on — it is an auth route,
	// and its only consumer today is the terminal page above.
	mux.HandleFunc("POST /auth/confirm", s.requireSession(s.handleStepUpConfirm))
}

func (s *Server) handleMirrorConnect(w http.ResponseWriter, r *http.Request) {
	if s.ext.Mirror == nil {
		http.NotFound(w, r)
		return
	}
	s.ext.Mirror.ServeConnect(w, r)
}

// mirrorHost is the viewing principal's host named host, if it has one.
func (s *Server) mirrorHost(ctx context.Context, host string) (mirror.HostView, bool) {
	if s.ext.Mirror == nil {
		return mirror.HostView{}, false
	}
	for _, h := range s.ext.Mirror.Hosts(ctx) {
		if h.Host == host {
			return h, true
		}
	}
	return mirror.HostView{}, false
}

// mirrorCards is the viewing principal's mirrored sessions, for the Agents
// view; nil with the plugin off.
func (s *Server) mirrorCards(ctx context.Context) []ui.MirrorCardView {
	if s.ext.Mirror == nil {
		return nil
	}
	var out []ui.MirrorCardView
	for _, h := range s.ext.Mirror.Hosts(ctx) {
		for _, sess := range h.Sessions {
			out = append(out, ui.MirrorCardView{Host: h.Host, Session: sess, Online: h.Online, Interactive: h.Interactive})
		}
	}
	return out
}

// handleMirrorTerminalPage is GET /mirror/terminal?host=…&session=…: the shared
// terminal page, pointed at the mirror socket.
func (s *Server) handleMirrorTerminalPage(w http.ResponseWriter, r *http.Request) {
	host, session := r.URL.Query().Get("host"), r.URL.Query().Get("session")
	h, ok := s.mirrorHost(r.Context(), host)
	if !ok || mirror.ValidSession(session) != nil {
		http.NotFound(w, r)
		return
	}
	v := ui.AgentTerminalView{
		Name:     session,
		BackHref: "/agents",
		Badge:    "Mirrored · " + h.Host,
		WSPath:   ui.MirrorTerminalWSPath(h.Host, session),
		// 🔴 THE SWITCH IS OFFERED ONLY WHEN THE HOST ADVERTISED -interactive.
		// The hub refuses a rw attach to a host that did not, regardless.
		ReadOnlyOnly: !h.Interactive,
		// 🔴 SAY WHAT INTERACTIVE REALLY IS: a tmux client on the host. From it
		// a user can switch to ANY session there, or open a new window — a
		// shell on the host. The "only listed sessions" rule guards opening an
		// attach, not what an interactive client does once attached.
		InteractiveNote: "Interactive types into a tmux client on " + h.Host +
			": from it you can switch to any tmux session there or open a shell on the host.",
		// 🔴 STEP-UP: Interactive needs a sign-in from the last
		// stepup.MaxAge. The page asks for the password when this has passed;
		// the socket below refuses a stale rw attach whatever the page does.
		StepUp: &ui.StepUpView{ConfirmPath: stepUpConfirmPath, FreshFor: freshForMillis(r.Context(), s.stepUpNow()), RememberKey: "mirror:" + h.Host},
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.AgentTerminalPage(v).Render(w); err != nil {
		s.logger.Printf("mirror: render terminal page: %v", err)
	}
}

// freshForMillis is how long the request's sign-in stays fresh enough for a
// step-up, in milliseconds from now; 0 when it is not fresh (or unknown).
func freshForMillis(ctx context.Context, now time.Time) int64 {
	if !stepup.Fresh(ctx, now) {
		return 0
	}
	return stepup.FreshUntil(ctx).Sub(now).Milliseconds()
}

// handleMirrorTerminalWS is the mirror terminal's socket: the same bridge as the
// claude-code terminal, read-only unless ?mode=rw.
func (s *Server) handleMirrorTerminalWS(w http.ResponseWriter, r *http.Request) {
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
	readOnly := q.Get("mode") != terminalModeInteractive
	// 🔴 STEP-UP, BEFORE THE UPGRADE: an interactive attach needs a sign-in
	// from the last stepup.MaxAge, whatever the page decided. 403 rather than an
	// in-socket error, so nothing is attached and the refusal has a status a
	// test (or a proxy log) can read. internal/mirror's Attach checks the same
	// context again, independently.
	if !readOnly && !stepup.Fresh(r.Context(), s.stepUpNow()) {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": stepup.ErrStale.Error()})
		return
	}
	initial, hasInitial := terminalSize(q.Get("cols"), q.Get("rows"))

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: s.wsAllowedOrigins(r)})
	if err != nil {
		s.logger.Printf("mirror: terminal ws accept: %v", err)
		return
	}
	defer c.CloseNow()
	ctx, done, ok := s.terminals.add(r.Context())
	if !ok {
		_ = c.Close(websocket.StatusGoingAway, "muster is shutting down")
		return
	}
	defer done()

	mode := terminalModeInteractive
	if readOnly {
		mode = terminalModeReadOnly
	}
	s.logger.Printf("mirror: terminal %s/%s attached (%s)", host, session, mode)
	err = bridgeTerminal(ctx, s.logger, c, readOnly, initial, hasInitial,
		func(ctx context.Context, stdin io.Reader, stdout io.Writer, resize <-chan provision.TermSize) error {
			return s.ext.Mirror.Attach(ctx, host, session, readOnly, stdin, stdout, resize)
		})
	s.logger.Printf("mirror: terminal %s/%s detached: %v", host, session, errOrNone(err))
}
