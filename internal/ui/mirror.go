package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// MirrorCardView is one MIRRORED session on the Agents list: a tmux session on
// one of the operator's own machines, reported live by that machine's
// muster-mirror (internal/mirror). It is NOT an agent — there is no agents row,
// no chat, nothing to start, stop or delete — so it is its own card, not an
// AgentCardView with fields switched off.
type MirrorCardView struct {
	Host    string
	Session string
	// Online: the host is connected and still lists the session.
	Online bool
	// Interactive: the host ANNOUNCED -interactive, so its terminal page offers
	// the Interactive switch (still behind a fresh sign-in: internal/stepup).
	Interactive bool
}

// MirrorTerminalPath is a mirrored session's terminal page.
func MirrorTerminalPath(host, session string) string {
	return "/mirror/terminal?" + url.Values{"host": {host}, "session": {session}}.Encode()
}

// MirrorTerminalWSPath is that page's socket, before the page adds its mode and
// size.
func MirrorTerminalWSPath(host, session string) string {
	return "/mirror/terminal/ws?" + url.Values{"host": {host}, "session": {session}}.Encode()
}

// mirrorCardID is a stable element id for (host, session): session names are
// arbitrary text, so they are hashed rather than spelled into an id.
func mirrorCardID(m MirrorCardView) string {
	sum := sha256.Sum256([]byte(m.Host + "\x00" + m.Session))
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
			Href(MirrorTerminalPath(m.Host, m.Session)),
			hx("hx-boost", "false"),
			Class("flex flex-col gap-3 p-4"),
			Div(
				Class("flex flex-wrap items-center gap-2"),
				statusIcon(status),
				Span(
					Class("mr-auto break-all text-base font-semibold leading-tight text-fg group-hover:text-accent"),
					g.Text(m.Session),
				),
				Span(g.Attr("data-mirror-badge", ""), chip("kind", "Mirrored · "+m.Host)),
				// What the host announced, not what this viewer may do right
				// now: typing still needs a fresh sign-in on the terminal page.
				g.If(m.Interactive, Span(
					g.Attr("data-mirror-interactive", ""),
					g.Attr("title", "This host accepts typing (muster-mirror -interactive); the terminal asks you to confirm it's you first"),
					chip("mode", "interactive"),
				)),
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
