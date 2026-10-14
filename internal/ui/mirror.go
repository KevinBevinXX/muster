package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// MirrorCardView is one MIRRORED entry on the Agents list: a tmux WINDOW (or,
// from a muster-mirror that registers per session, a whole session) on one of
// the operator's own machines, reported live by that machine's muster-mirror
// (internal/mirror). It is NOT an agent — there is no agents row, no chat,
// nothing to start, stop or delete — so it is its own card, not an
// AgentCardView with fields switched off.
type MirrorCardView struct {
	Host    string
	Session string
	// Window is the tmux window id the card is ("@12"); "" = a per-session
	// host's session. With Session it is the card's ADDRESS: what it links,
	// keys and ids by. Never the window's index, which tmux renumbers.
	Window string
	// Title is what the card is called when the host sent no Label:
	// "<session>:<window index>" for a window. "" = the session name.
	Title string
	// Label is the host's display name for the entry ("mango:1"), "" for
	// none. DISPLAY ONLY: the card shows Session beside the label so the real
	// name stays findable.
	Label string
	// Kind is what runs in the pane, as the list names it ("Claude Code",
	// "opencode", "shell", or a command); "" = unknown. Shown as a chip and
	// searched; Claude decides where the card opens.
	Kind string
	// Online: the host is connected and still lists the session.
	Online bool
	// Interactive: the host ANNOUNCED -interactive, so its terminal page offers
	// the Interactive switch (still behind a fresh sign-in: internal/stepup).
	Interactive bool
	// Claude: the host reports Claude Code in the session's pane, so the card
	// opens the RENDERED view by default (MirrorViewPath); otherwise it opens the
	// terminal.
	Claude bool
}

// MirrorCardHref is where a mirrored card opens: the rendered view for a
// Claude Code pane, the terminal for anything else.
func MirrorCardHref(m MirrorCardView) string {
	ref := MirrorRef{Host: m.Host, Session: m.Session, Window: m.Window}
	if m.Claude {
		return ref.ViewPath()
	}
	return ref.TerminalPath()
}

// MirrorRef addresses one mirrored entry: a host, a tmux session on it, and —
// for a host that registers per window — the window's tmux id. Every URL of a
// mirrored entry is built from one, so the pages, their sockets and their
// composer cannot name different windows.
//
// Window == "" is "the session's ACTIVE window": the only address a
// per-session host has, and what every URL from before windows were
// addressable means.
type MirrorRef struct {
	Host    string
	Session string
	Window  string
}

// query is the ref as URL parameters. `window` is present only when set, so a
// per-session entry's URLs are byte-identical to what they always were.
func (r MirrorRef) query() string {
	v := url.Values{"host": {r.Host}, "session": {r.Session}}
	if r.Window != "" {
		v.Set("window", r.Window)
	}
	return v.Encode()
}

// TerminalPath is the entry's terminal page.
func (r MirrorRef) TerminalPath() string { return "/mirror/terminal?" + r.query() }

// TerminalWSPath is that page's socket, before the page adds its mode and size.
func (r MirrorRef) TerminalWSPath() string { return "/mirror/terminal/ws?" + r.query() }

// ViewPath is the entry's rendered view.
func (r MirrorRef) ViewPath() string { return "/mirror/view?" + r.query() }

// ViewWSPath is that page's transcript socket, before the page adds its resume
// point.
func (r MirrorRef) ViewWSPath() string { return "/mirror/view/ws?" + r.query() }

// MirrorTerminalPath is a mirrored session's terminal page (its active window).
func MirrorTerminalPath(host, session string) string {
	return MirrorRef{Host: host, Session: session}.TerminalPath()
}

// MirrorTerminalWSPath is that page's socket, before the page adds its mode and
// size.
func MirrorTerminalWSPath(host, session string) string {
	return MirrorRef{Host: host, Session: session}.TerminalWSPath()
}

// mirrorCardID is a stable element id for (host, session, window): session
// names are arbitrary text, so they are hashed rather than spelled into an id.
// A per-session card's id is what it always was.
func mirrorCardID(m MirrorCardView) string {
	key := m.Host + "\x00" + m.Session
	if m.Window != "" {
		key += "\x00" + m.Window
	}
	sum := sha256.Sum256([]byte(key))
	return "mirror-" + hex.EncodeToString(sum[:6])
}

// mirrorCard is a mirrored session's card: the session name, a
// "Mirrored · <host>" badge, an "offline" badge while the host is not serving
// it, and the whole body a real-load link to its terminal.
//
// ⚠ ITS CLASSES ARE agentCard's, so it adds nothing to the committed stylesheet.
func mirrorCard(m MirrorCardView) g.Node {
	status := "running"
	if !m.Online {
		status = "stopped"
	}
	return Article(
		ID(mirrorCardID(m)),
		g.Attr("data-mirror-card", ""),
		Class("group relative overflow-hidden rounded-2xl border border-line bg-s1/70 shadow-lg shadow-black/20 ring-1 ring-line"),
		A(
			Href(MirrorCardHref(m)),
			hx("hx-boost", "false"),
			Class("flex flex-col gap-3 p-4"),
			Div(
				Class("flex flex-wrap items-center gap-2"),
				statusIcon(status),
				mirrorEntryTitle(m.Label, m.Title, m.Session,
					"mr-auto break-all text-base font-semibold leading-tight text-fg group-hover:text-accent"),
				Span(g.Attr("data-mirror-badge", ""), chip("kind", "Mirrored · "+m.Host)),
				// What the host announced, not what this viewer may do right
				// now: typing still needs a fresh sign-in on the terminal page.
				g.If(m.Interactive, Span(
					g.Attr("data-mirror-interactive", ""),
					g.Attr("title", "This host accepts typing (muster-mirror -interactive); the terminal asks you to confirm it's you first"),
					chip("mode", "interactive"),
				)),
				g.If(m.Claude, Span(g.Attr("data-mirror-claude", ""), chip("kind", "Claude Code"))),
				// What else runs there (a shell, opencode, another TUI): the
				// host's reading of the pane's command.
				g.If(!m.Claude && m.Kind != "", Span(g.Attr("data-mirror-kind", ""), chip("kind", m.Kind))),
				g.If(!m.Online, Span(
					g.Attr("data-mirror-offline", ""),
					g.Attr("title", "The host's muster-mirror is not connected, or the host no longer lists this tmux session"),
					chip("host", "offline"),
				)),
			),
		),
	)
}

// AgentsCardsWithMirrors is the Agents list: every agent card, then every
// mirrored session the viewing principal's hosts report. The empty state shows
// only when there are neither.
func AgentsCardsWithMirrors(list []AgentCardView, mirrors []MirrorCardView) g.Node {
	if len(mirrors) == 0 {
		return AgentsCards(list)
	}
	return g.Group{g.Group(g.Map(list, agentCard)), g.Group(g.Map(mirrors, mirrorCard))}
}

// mirrorSessionTitle is a mirrored session's name as a list or page title: the
// host's label when it sent one, with the raw tmux session name as a small
// secondary text AND the tooltip; the session name alone otherwise.
//
// 🔴 THE RAW NAME STAYS VISIBLE, NOT ONLY IN A TOOLTIP: it is what an operator
// types into `tmux attach`, and a phone has no hover.
func mirrorSessionTitle(label, session, class string) g.Node {
	return mirrorEntryTitle(label, "", session, class)
}

// mirrorEntryTitle is mirrorSessionTitle for an entry that may be a WINDOW:
// title is what an entry with no label is called ("<session>:<window index>";
// "" = the session name). A window with no label needs no secondary text — its
// title already spells the session.
func mirrorEntryTitle(label, title, session, class string) g.Node {
	if label == "" || label == session {
		return Span(Class(class), g.Attr("data-mirror-name", ""), g.Text(displayOr(title, session)))
	}
	return Span(
		Class(class),
		g.Attr("data-mirror-name", ""),
		g.Attr("title", "tmux session "+session),
		Span(g.Attr("data-mirror-label", ""), g.Text(label)),
		Span(g.Attr("data-mirror-session", ""), Class("ml-1.5 text-xs text-muted"), g.Text(session)),
	)
}
