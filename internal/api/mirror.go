package api

import (
	"context"
	"io"
	"net/http"
	"strings"
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
// 🔴 EVERY WINDOW IS AN ENTRY, ADDRESSED BY (host, session, window id). A host
// that registers per window (mirror.Msg.Windows) has one card per tmux WINDOW,
// and its pages, sockets and sends carry `window=@<n>` — tmux's own id for the
// window, never its index, which tmux renumbers. A URL with no `window` (every
// link from before windows were addressable, and every link to a per-session
// host) means the session's ACTIVE window: mirrorEntry resolves it, and the
// two pages answer it with a redirect to the explicit address so that what is
// on screen, its socket and its composer all name one window.
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
	// The Terminal view's composer: one gated paste into the session's active
	// pane (mirror_view.go's mirrorSend — the rendered view's send, with the
	// other target).
	mux.HandleFunc("POST /mirror/terminal/send", s.requireOperatorWrite(s.handleMirrorTerminalSend))
	// "Confirm it's you" (stepup.go): the fresh sign-in an INTERACTIVE attach or
	// a send needs — the password again, or a passkey. Registered whether or
	// not the plugin is on — they are auth routes, and their consumers today
	// are the pages above and the account's passkey page.
	mux.HandleFunc("POST /auth/confirm", s.requireSession(s.handleStepUpConfirm))
	mux.HandleFunc("POST /auth/confirm/passkey/options", s.requireSession(s.handleStepUpPasskeyOptions))
	mux.HandleFunc("POST /auth/confirm/passkey/verify", s.requireSession(s.handleStepUpPasskeyVerify))
	s.registerMirrorViewRoutes(mux)
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
// view — one list across its hosts, most recently active first (mirror.Ordered
// is the order's one definition); nil with the plugin off.
func (s *Server) mirrorCards(ctx context.Context) []ui.MirrorCardView {
	if s.ext.Mirror == nil {
		return nil
	}
	var out []ui.MirrorCardView
	for _, m := range mirror.Ordered(s.ext.Mirror.Hosts(ctx)) {
		out = append(out, ui.MirrorCardView{Host: m.Host, Session: m.Session, Window: m.Window, Title: mirrorTitle(m), Label: m.Label,
			Kind: m.Kind(), Online: m.Online, Interactive: m.Interactive, Claude: m.Claude})
	}
	return out
}

// mirrorTitle is what an entry with no label is called, for the ui views'
// Title: "<session>:<window index>" for a window, "" (the session name) for a
// per-session entry. mirror.SessionView.Title is the rule.
func mirrorTitle(e mirror.SessionView) string {
	if e.Window == "" || e.Label != "" {
		return ""
	}
	return e.Title()
}

// mirrorEntry resolves a request's (host, session, window) parameters against
// the viewing principal's hosts.
//
//   - ok is false — the caller answers 404 — when the host is not the
//     principal's (or the plugin is off), the session name is not one the
//     tunnel carries, or `window` is set and is not a tmux window id.
//   - listed says the host lists the entry now (or, offline, listed it last).
//     An entry that is not listed is still returned, carrying the request's own
//     address: its page renders and its socket then says why it cannot attach.
//   - redirect says the request named only the session of a PER-WINDOW host,
//     and e is the session's active window: the pages send the browser to the
//     explicit address rather than serve a page whose window could change
//     between its socket connects.
func (s *Server) mirrorEntry(r *http.Request, host, session, window string) (h mirror.HostView, e mirror.SessionView, listed, redirect, ok bool) {
	h, found := s.mirrorHost(r.Context(), host)
	if !found || mirror.ValidSession(session) != nil || (window != "" && mirror.ValidWindowID(window) != nil) {
		return h, e, false, false, false
	}
	e, listed = h.Find(session, window)
	if !listed {
		e = mirror.SessionView{Host: h.Host, Session: session, Window: window, Online: h.Online, Interactive: h.Interactive,
			Rendered: h.Rendered, PanePaste: h.PanePaste}
	}
	return h, e, listed, listed && window == "" && e.Window != "", true
}

// mirrorRef is e's address for the ui package's URLs.
func mirrorRef(e mirror.SessionView) ui.MirrorRef {
	return ui.MirrorRef{Host: e.Host, Session: e.Session, Window: e.Window}
}

// handleMirrorTerminalPage is GET /mirror/terminal?host=…&session=…: the shared
// terminal page, pointed at the mirror socket.
func (s *Server) handleMirrorTerminalPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	h, e, _, redirect, ok := s.mirrorEntry(r, q.Get("host"), q.Get("session"), q.Get("window"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if redirect {
		http.Redirect(w, r, keepNotice(r, mirrorRef(e).TerminalPath()), http.StatusSeeOther)
		return
	}
	session := e.Session
	rawName := ""
	if e.Label != "" && e.Label != session {
		rawName = session
	}
	v := ui.AgentTerminalView{
		Name: session,
		// The host's display label ("mango:1") — or, for a window with none,
		// "<session>:<index>" — titles the page; the session NAME and the
		// window ID stay the address (WSPath).
		DisplayName: e.Title(),
		RawName:     rawName,
		BackHref:    "/agents",
		Badge:       "Mirrored · " + h.Host,
		WSPath:      mirrorRef(e).TerminalWSPath(),
		// 🔴 THE SWITCH IS OFFERED ONLY WHEN THE HOST ADVERTISED -interactive.
		// The hub refuses a rw attach to a host that did not, regardless.
		ReadOnlyOnly: !h.Interactive,
		// 🔴 SAY WHAT INTERACTIVE REALLY IS: a tmux client on the host. From it
		// a user can switch to ANY session there, or open a new window — a
		// shell on the host. The "only listed sessions" rule guards opening an
		// attach, not what an interactive client does once attached.
		InteractiveNote: "Interactive types into a tmux client on " + h.Host +
			": from it you can switch to any tmux session there or open a shell on the host.",
		// 🔴 STEP-UP: Interactive, and every send from the composer, needs a
		// sign-in from the last stepup.Window. The page asks to confirm (passkey
		// or password) when this has passed; the socket and the send route below
		// refuse a stale write whatever the page does.
		StepUp: s.stepUpView(r, "mirror:"+h.Host),
	}
	// 🔴 THE COMPOSER TAKES TEXT ONLY ON A HOST THAT ADVERTISED -interactive,
	// and it does not need the switch above: it is one gated paste per Send into
	// the pane on screen, not a live keyboard. Where it cannot send it is drawn
	// all the same, DISABLED AND SAYING WHY — a read-only host (it used to draw
	// none there, which reads as "this view has no message box"), and an
	// -interactive host whose muster-mirror cannot paste into this pane (an
	// older one, and no Claude Code here).
	if h.Online {
		c := ui.ComposerView{SendPath: ui.MirrorTerminalSendPath, Host: h.Host, Session: session, Window: e.Window,
			Placeholder: "Message…", Aria: "Send a message into this session", DisabledPlaceholder: "Update muster-mirror to send",
			Nonce: newSendNonce()}
		// The outcome of a send the browser submitted, which was answered with a
		// redirect back here (composer.go).
		c.Notice, c.NoticeOK = composerNotice(r)
		switch {
		case !h.Interactive:
			c.Disabled, c.DisabledPlaceholder = ui.MirrorReadOnlyNote, "Read-only host"
		case !e.CanPaste():
			c.Disabled = strings.TrimPrefix(mirror.ErrPanePasteUnsupported.Error(), "mirror: ")
		}
		v.Composer = &c
	}
	if e.HasRenderedView() {
		v.RenderedHref = mirrorRef(e).ViewPath()
	}
	formDocument(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := ui.AgentTerminalPage(v).Render(w); err != nil {
		s.logger.Printf("mirror: render terminal page: %v", err)
	}
}

// stepUpView is the "Confirm it's you" wiring a page renders: the confirm
// route, the passkey confirm's prefix when this server serves passkeys at all
// (the dialog asks whether THIS account has one when it opens — never here,
// which would cost a GoTrue call per page view), how long the request's sign-in
// stays fresh, and the window in words.
func (s *Server) stepUpView(r *http.Request, rememberKey string) *ui.StepUpView {
	v := &ui.StepUpView{ConfirmPath: stepUpConfirmPath, FreshFor: freshForMillis(r.Context(), s.stepUpNow()),
		RememberKey: rememberKey, Window: stepup.WindowText()}
	if s.auth.goTrueMode() && s.gt != nil {
		v.PasskeyPath = stepUpPasskeyPath
	}
	return v
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
	host, session, window := q.Get("host"), q.Get("session"), q.Get("window")
	if _, _, _, _, ok := s.mirrorEntry(r, host, session, window); !ok {
		http.NotFound(w, r)
		return
	}
	readOnly := q.Get("mode") != terminalModeInteractive
	// 🔴 STEP-UP, BEFORE THE UPGRADE: an interactive attach needs a sign-in
	// from the last stepup.Window, whatever the page decided. 403 rather than an
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
	s.logger.Printf("mirror: terminal %s/%s%s attached (%s)", host, session, window, mode)
	err = bridgeTerminal(ctx, s.logger, c, readOnly, initial, hasInitial,
		func(ctx context.Context, stdin io.Reader, stdout io.Writer, resize <-chan provision.TermSize) error {
			// window == "" is the session's active window, resolved by the plugin
			// to an explicit window id on a per-window host (mirror.Hub.AttachWindow).
			return s.ext.Mirror.AttachWindow(ctx, host, session, window, readOnly, stdin, stdout, resize)
		})
	s.logger.Printf("mirror: terminal %s/%s%s detached: %v", host, session, window, errOrNone(err))
}
