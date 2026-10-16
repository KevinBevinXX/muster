package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"time"

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
	// Qualifier is what tells this entry apart from another listed entry with
	// the same title — its host, and/or its tmux address — "" when the title is
	// already unique. Decided in ONE place over the whole list
	// (mirror.SessionView.Qualifier); shown as part of the title.
	Qualifier string
	// Kind is what runs in the pane, as the list names it ("Claude Code",
	// "opencode", "shell", or a command); "" = unknown. Shown as a chip and
	// searched; Claude decides where the card opens.
	Kind string
	// Online: the host is connected and still lists the session.
	Online bool
	// Interactive: the host ANNOUNCED -interactive, so its terminal page offers
	// the Interactive switch (still behind a fresh sign-in: internal/stepup).
	// The card does not badge it; it is a filter field (mode:interactive).
	Interactive bool
	// ActiveAt is when the host last saw the entry active
	// (mirror.SessionView.Activity: Unix seconds, floored to a minute); zero =
	// the host did not say. The footer's "active … ago" prints it — the same
	// number the list is ordered by (mirror.Ordered).
	ActiveAt time.Time
	// Claude: the host reports Claude Code in the session's pane, so the card
	// opens the RENDERED view by default (MirrorViewPath); otherwise it opens the
	// terminal.
	Claude bool
	// Pinned: the viewing user pinned this entry's address (internal/pins). The
	// list draws pinned entries first (AgentsCardsWithMirrors); the card shows a
	// pin glyph and its pin toggle pressed.
	Pinned bool
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

// ScrollForm is the entry's address as the body of a scroll request
// (MirrorTerminalScrollPath), before the page adds `lines`.
func (r MirrorRef) ScrollForm() string { return r.query() }

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

// mirrorCard is a mirrored session's card: the session name, a mirrored icon
// (with its host in a tooltip), an "offline" badge while the host is not
// serving it, the whole body a real-load link to its terminal, and a footer
// naming the host and when the window was last active.
//
// 🔴 ITS SHAPE IS agentCard's, SLOT FOR SLOT (cardSlot), and so are its
// classes, so it adds nothing to the committed stylesheet.
// TestAgentAndMirrorCardsRenderTheSameSlots pins the shape.
func mirrorCard(m MirrorCardView) g.Node {
	f := mirrorFields(m)
	status, kind, mode, state := f.Status, f.Kind, f.Mode, f.State
	id := mirrorCardID(m)
	return Article(
		ID(id),
		g.Attr("data-mirror-card", ""),
		g.If(m.Pinned, g.Attr("data-pinned", "")),
		// What the list's filter finds this card by (agents_filter.go): its title
		// ("Gold:8"), the title as shown — with its qualifier ("Gold:8 · laptop")
		// — its raw tmux session name, its host, and what runs in it.
		cardFilterAttrs(f,
			m.Label, m.Title, mirrorQualified(displayOr(m.Label, displayOr(m.Title, m.Session)), m.Qualifier),
			m.Session, m.Host, kind, "mirrored", mode, state),
		Class("group relative overflow-hidden rounded-2xl border border-line bg-s1/70 shadow-lg shadow-black/20 ring-1 ring-line"),
		// The control cluster: the mirrored mark and the pin toggle, OUTSIDE the
		// nav anchor (agentCard's pattern) so tapping either never opens the
		// window.
		cardControls(
			mirroredMark(id, m.Host),
			mirrorPinToggle(m),
		),
		A(
			Href(MirrorCardHref(m)),
			hx("hx-boost", "false"),
			cardSlot("body"),
			Class("flex flex-col gap-3 p-4 pr-28"),
			Div(
				cardSlot("header"),
				Class("flex flex-wrap items-center gap-2"),
				statusIcon(status),
				g.If(m.Pinned, pinnedGlyph()),
				mirrorEntryTitle(m.Label, m.Title, m.Session, m.Qualifier,
					"mr-auto break-all text-base font-semibold leading-tight text-fg group-hover:text-accent"),
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
		// The host stays VISIBLE, not only in the mark's tooltip: it is what tells
		// two same-named windows apart and a filter field (host:), and a phone has
		// no hover.
		cardFooter(
			cardMeta("host", Span(g.Attr("data-mirror-host", ""), Class("font-medium text-fg2"), g.Text(m.Host))),
			cardActivity(m.ActiveAt),
		),
	)
}

// mirrorFields is what a mirrored card is filtered by: its data-f-* (and
// data-l-*) attributes, which the filter's options are read from.
func mirrorFields(m MirrorCardView) cardFields {
	f := cardFields{Host: m.Host, Type: "Mirrored", Name: displayOr(m.Label, displayOr(m.Title, m.Session)),
		Session: m.Session, Status: "running", Kind: m.Kind, Titles: []string{m.Label, m.Title, m.Session}}
	if !m.Online {
		f.Status, f.State = "stopped", "offline"
	}
	if m.Claude {
		f.Kind = "Claude Code"
	}
	if m.Interactive {
		f.Mode = "interactive"
	}
	return f
}

// mirroredMark is the icon that says "this card is a mirrored tmux window, not
// an agent". Its host is in the tooltip: `title` for a pointer, the accessible
// name for a screen reader, and — because a phone has no hover — a native
// popover the same button opens on a tap (placed by floatingMenuScript; an
// outside tap or Escape closes it, the browser's own popover behaviour).
//
// It is a BUTTON in the control cluster rather than an icon in the title row
// because the title row is inside the card's nav anchor, where a tap navigates
// and a button is not allowed.
func mirroredMark(cardID, host string) g.Node {
	label := "Mirrored · " + host
	tip := cardID + "-mark"
	return g.Group{
		Button(
			Type("button"),
			g.Attr("data-mirror-badge", ""),
			g.Attr("aria-label", label),
			g.Attr("title", label),
			g.Attr("popovertarget", tip),
			Class("press inline-flex h-11 w-11 items-center justify-center rounded-xl text-st-progress-fg transition hover:bg-s2"),
			g.Raw(mirroredIcon),
		),
		Div(
			ID(tip),
			g.Attr("popover", "auto"),
			g.Attr("role", "tooltip"),
			g.Attr("data-floating-menu", ""),
			Class("invisible hidden [&:popover-open]:flex m-0 w-44 flex-col gap-1 rounded-xl border border-line bg-s1 p-1.5 text-fg shadow-2xl ring-1 ring-black/20"),
			Span(Class("break-words px-3 py-2 text-sm font-medium"), g.Text(label)),
		),
	}
}

// mirroredIcon is the mirrored mark's screen glyph: mirrorBadge's, one size up.
const mirroredIcon = `<svg class="h-4 w-4 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="4" width="18" height="12" rx="2"/><path d="M8 20h8M12 16v4"/></svg>`

// AgentsCardsWithMirrors is the Agents list: the viewer's PINNED entries
// first, then the rest — and within each group the order the list already had:
// agent cards (most recently active first, agents.SortByLastActivity), then
// mirrored entries (mirror.Ordered). The empty state shows only when there
// are neither.
//
// 🔴 A STABLE PARTITION, NOT A SORT, AND THIS IS THE ONE PLACE IT HAPPENS. It
// moves pinned cards ahead without re-deciding anyone's order, so the
// activity order GET /api/agents publishes and the hub's mirror.Ordered (whose
// order is also its change detection) stay per-viewer-free. Every card stays a
// direct <article> child of #agents-list — no wrapper, no divider article — so
// the filter's `#agents-list > article` rule and its count still see each one
// (agents_filter.go): a pinned card that does not match is hidden like any
// other.
func AgentsCardsWithMirrors(list []AgentCardView, mirrors []MirrorCardView) g.Node {
	if len(list) == 0 && len(mirrors) == 0 {
		return AgentsCards(nil)
	}
	var pinnedAgents, rest []AgentCardView
	for _, a := range list {
		if a.Pinned {
			pinnedAgents = append(pinnedAgents, a)
		} else {
			rest = append(rest, a)
		}
	}
	var pinnedMirrors, others []MirrorCardView
	for _, m := range mirrors {
		if m.Pinned {
			pinnedMirrors = append(pinnedMirrors, m)
		} else {
			others = append(others, m)
		}
	}
	return g.Group{
		g.Group(g.Map(pinnedAgents, agentCard)), g.Group(g.Map(pinnedMirrors, mirrorCard)),
		g.Group(g.Map(rest, agentCard)), g.Group(g.Map(others, mirrorCard)),
	}
}

// mirrorSessionTitle is a mirrored session's name as a list or page title: the
// host's label when it sent one, with the raw tmux session name as a small
// secondary text AND the tooltip; the session name alone otherwise.
//
// 🔴 THE RAW NAME STAYS VISIBLE, NOT ONLY IN A TOOLTIP: it is what an operator
// types into `tmux attach`, and a phone has no hover.
func mirrorSessionTitle(label, session, class string) g.Node {
	return mirrorEntryTitle(label, "", session, "", class)
}

// mirrorEntryTitle is mirrorSessionTitle for an entry that may be a WINDOW:
// title is what an entry with no label is called ("<session>:<window index>";
// "" = the session name). A window with no label needs no secondary text — its
// title already spells the session.
//
// qualifier ("" = none) is what tells the entry apart from another listed
// entry of the same name (mirror.SessionView.Qualifier). It is drawn AS PART
// OF THE TITLE, right after the name — "mango:1 · laptop" — not in a chip, so
// two "mango:1" cards read as two different titles. The card, the rendered
// view and the terminal page all draw their title here.
func mirrorEntryTitle(label, title, session, qualifier, class string) g.Node {
	q := g.If(qualifier != "", Span(g.Attr("data-mirror-qualifier", ""), g.Text(mirrorQualifierSep+qualifier)))
	if label == "" || label == session {
		return Span(Class(class), g.Attr("data-mirror-name", ""), g.Text(displayOr(title, session)), q)
	}
	return Span(
		Class(class),
		g.Attr("data-mirror-name", ""),
		g.Attr("title", "tmux session "+session),
		Span(g.Attr("data-mirror-label", ""), g.Text(label)),
		q,
		Span(g.Attr("data-mirror-session", ""), Class("ml-1.5 text-xs text-muted"), g.Text(session)),
	)
}

// mirrorQualifierSep joins a mirrored entry's name and its qualifier — the
// same separator as mirror.QualifierSep (this package does not import the
// plugin), so the titles here spell mirror.SessionView.DisplayTitle exactly.
const mirrorQualifierSep = " · "

// mirrorQualified is a mirrored entry's name as plain text (a <title>, the
// filter's haystack): the name, then its qualifier when it has one.
func mirrorQualified(name, qualifier string) string {
	if qualifier == "" {
		return name
	}
	return name + mirrorQualifierSep + qualifier
}

// mirrorBadge is the "Mirrored · <host>" badge beside a mirrored entry's
// title on both its pages (the card carries mirroredMark instead). It is NOT the generic grey chip:
// a muted chip among the kind/mode chips was easy to miss on a phone, and the
// host is what tells two same-named windows apart — so it is tinted, bold, and
// leads with a screen icon.
func mirrorBadge(text string) g.Node {
	return Span(
		Class("inline-flex items-center gap-1 rounded-full bg-st-progress-bg px-2.5 py-1 text-xs font-semibold text-st-progress-fg ring-1 ring-inset ring-st-progress-fg/40"),
		g.Raw(`<svg class="h-3.5 w-3.5 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="4" width="18" height="12" rx="2"/><path d="M8 20h8M12 16v4"/></svg>`),
		g.Text(text),
	)
}
