package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"time"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/mirror"
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
	// AgentView: the host reports a non-Claude runtime with a view (Hermes)
	// in the pane AND names the conversation it shows
	// (mirror.SessionView.AgentView), so the card opens
	// its read-only rendered view by default, as a Claude Code card does.
	AgentView bool
	// Pinned: the viewing user pinned this entry's address (internal/pins). The
	// list draws a pinned entry first, as a label-only CHIP instead of its card
	// (pinned_chips.go); a card is only ever drawn for an unpinned entry.
	Pinned bool
	// Focus is what the Claude Code session in the window is working on
	// (internal/focus, keyed by its Claude session id), nil for none.
	Focus *FocusView
	// Repo is the short name of the repository the window works in
	// (mirror.SessionView.Repo: its git top level's name, else its cwd's),
	// "" when the host did not say (an older muster-mirror). A chip, and found
	// by the filter (`repo:` and free text). The card's NAME is unchanged.
	Repo string
	// Attention is the card's attention badge (cards_attention.go), nil for
	// none.
	Attention *CardAttention
	// Work is what the window's Claude Code session has running now
	// (mirror.SessionView.Work): its chip (work_chip.go), display only. The zero value — nothing running, or a host that did not
	// say — draws no chip.
	Work mirror.WindowWork
	// Color is the window's tmux identity colour (mirror.SessionView.Color,
	// lowercase #rrggbb), "" for none: the card's border and title are tinted
	// with it (identity_color.go). None — or anything that is not a wire
	// colour — draws the card exactly as before.
	Color string
	// HostIcon is the key of Host's icon (HostIconFor, composed in
	// internal/api from the host's registration rank); "" draws the hash
	// fallback.
	HostIcon string
	// Idle: drawn in the list's Idle section (idle_section.go), composed by
	// MarkIdle. Its card carries data-idle-section.
	Idle bool
}

// MirrorCardHref is where a mirrored card opens: the rendered view for a
// Claude Code pane or a Hermes one with a known session, the terminal for
// anything else.
func MirrorCardHref(m MirrorCardView) string {
	ref := MirrorRef{Host: m.Host, Session: m.Session, Window: m.Window}
	if m.Claude || m.AgentView {
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
// serving it, the whole body a real-load link to its terminal naming its host
// (hostChip), and a footer saying when the window was last active.
//
// 🔴 ITS SHAPE IS agentCard's, SLOT FOR SLOT (cardSlot), and so are its
// classes, so it adds nothing to the committed stylesheet.
// TestAgentAndMirrorCardsRenderTheSameSlots pins the shape.
func mirrorCard(m MirrorCardView) g.Node {
	status := mirrorFields(m).Status
	id := mirrorCardID(m)
	// A name past cardTitleMax is drawn in a title row of its own, clamped,
	// with a "…" button that opens it whole (cardLongTitle). The qualifier
	// stays beside it, as in mirrorCardTitle; the raw session name is not
	// drawn on a card.
	// The name as a reader sees it (textplain): a Hermes or tmux window title is
	// often LLM-written and carries markdown.
	name := plainOr(displayOr(m.Label, m.Title), m.Session)
	_, long := clampTitle(name)
	reserve := cardReserve(m.Attention)
	bodyClass := cardBodyClass(m.Focus, reserve)
	var titleRow g.Node
	if long {
		bodyClass = titledBodyClass(reserve)
		titleRow = cardLongTitle(id, MirrorCardHref(m), name, cardHasFocusTitle(m.Focus), reserve,
			g.If(m.Qualifier != "", Span(g.Attr("data-mirror-qualifier", ""), g.Text(mirrorQualifierSep+m.Qualifier))))
	}
	return Article(
		ID(id),
		g.Attr("data-mirror-card", ""),
		g.If(m.Idle, g.Attr("data-idle-section", "")),
		// The window's tmux identity colour: its border and title (input.css
		// maps the variants to the theme in force).
		identityAttrs(m.Color),
		mirrorListAttrs(m),
		Class("group relative overflow-hidden rounded-2xl border border-line bg-s1/70 shadow-lg shadow-black/20 ring-1 ring-line"),
		// The control cluster: the mirrored mark and the pin toggle, OUTSIDE the
		// nav anchor (agentCard's pattern) so tapping either never opens the
		// window.
		cardControls(
			cardAttentionBadge(m.Attention),
			mirroredMark(id, m.Host),
			mirrorPinToggle(m),
		),
		// With a focus, it heads the card (cardFocusTitle): a popover trigger
		// OUTSIDE the nav anchor, and the window's title below (mirrorCardTitle:
		// no raw session name) becomes its subtitle.
		cardFocusTitle(id, m.Focus, reserve),
		titleRow,
		A(
			Href(MirrorCardHref(m)),
			hx("hx-boost", "false"),
			cardSlot("body"),
			// A long name is in the title row above, so the anchor is named here.
			g.If(long, g.Attr("aria-label", mirrorQualified(name, m.Qualifier))),
			Class(bodyClass),
			Div(
				cardSlot("header"),
				Class("flex flex-wrap items-center gap-2"),
				statusIcon(status),
				// Its host's icon, beside its name (host_icons.go).
				hostMark(m.Host, m.HostIcon),
				g.If(!long && !cardHasFocusTitle(m.Focus), mirrorCardTitle(m.Label, m.Title, m.Session, m.Qualifier, cardTitleClass)),
				g.If(!long && cardHasFocusTitle(m.Focus), Span(g.Attr("data-card-subtitle", ""), Class("contents"),
					mirrorCardTitle(m.Label, m.Title, m.Session, m.Qualifier, cardSubtitleClass))),
				// Its host, spelled (hostChip).
				hostChip(m.Host),
				g.If(m.Claude, Span(g.Attr("data-mirror-claude", ""), chip("kind", "Claude Code"))),
				// What else runs there (a shell, opencode, another TUI): the
				// host's reading of the pane's command.
				g.If(!m.Claude && m.Kind != "", Span(g.Attr("data-mirror-kind", ""), chip("kind", m.Kind))),
				// The repository it works in (its host's repos message).
				g.If(m.Repo != "", Span(g.Attr("data-mirror-repo", ""), chip("repo", m.Repo))),
				// The subagents and background shells it has running now.
				workChip(m.Work),
				g.If(!m.Online, Span(
					g.Attr("data-mirror-offline", ""),
					g.Attr("title", "The host's muster-mirror is not connected, or the host no longer lists this tmux session"),
					// "state", the filter field (state:offline) — not "host", which
					// beside the host chip read as a second host called "offline".
					chip("state", "offline"),
				)),
			),
		),
		// The host is in the body (hostChip), not here.
		cardFooter(
			cardActivity(m.ActiveAt),
			cardFocus(id, m.Focus),
		),
	)
}

// mirrorListAttrs is what the list's filter, host tabs and filter chips read
// off a mirrored entry — on its card AND on its pinned chip (pinned_chips.go),
// from this ONE function, so a chip is found, counted and hidden exactly as
// its card would be: its recency (list_signals.go), whether it waits on you,
// and its filter fields and haystack (agents_filter.go) — its title ("Gold:8"),
// the title as shown, with its qualifier ("Gold:8 · laptop"), its raw tmux
// session name, its host, and what runs in it.
func mirrorListAttrs(m MirrorCardView) g.Node {
	f := mirrorFields(m)
	return g.Group{
		cardActivityAttrs(time.Now(), m.ActiveAt),
		g.If(waitTokens(m.Attention, false) != "", g.Attr("data-wait", waitTokens(m.Attention, false))),
		cardFilterAttrs(f, append([]string{
			m.Label, m.Title, mirrorQualified(displayOr(m.Label, displayOr(m.Title, m.Session)), m.Qualifier),
			m.Session, m.Host, f.Kind, m.Repo, "mirrored", f.Mode, f.State}, focusHaystack(m.Focus)...)...),
	}
}

// mirrorFields is what a mirrored card is filtered by: its data-f-* (and
// data-l-*) attributes, which the filter's options are read from.
func mirrorFields(m MirrorCardView) cardFields {
	f := cardFields{Host: m.Host, Type: "Mirrored", Name: displayOr(m.Label, displayOr(m.Title, m.Session)),
		Session: m.Session, Status: "running", Kind: m.Kind, Repo: m.Repo, Titles: []string{m.Label, m.Title, m.Session}}
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

// AgentsCardsWithMirrors is the Agents list: ONE flat list of agent cards and
// mirrored entries — the viewer's PINNED entries first, as compact chips
// (pinned_chips.go), then the rest as cards, each group most recently active
// first (orderList, which states the whole rule).
// The empty state shows only when there are neither.
//
// 🔴 THE PER-KIND ORDERS ARE INPUTS, NOT RE-DECIDED. agents.SortByLastActivity
// (what GET /api/agents publishes) and the hub's mirror.Ordered (whose order is
// also its change detection) stay per-viewer-free and keep deciding ties within
// their own kind; orderList only interleaves them by activity and lifts pins.
// Every card stays a direct <article> child of #agents-list — no wrapper, no
// divider article — so the filter's `#agents-list > article` rule and its count
// still see each one (agents_filter.go): a pinned chip that does not match is
// hidden like any card. (The pinned row's "Edit pins" toggle is the one
// non-article child besides the Idle header; the filter's rules never see it.)
//
// Entries marked Idle (MarkIdle) are drawn after the rest, under the Idle
// section's header (idle_section.go); this form, with no threshold to name,
// draws the header without its "no activity in over …" hint.
func AgentsCardsWithMirrors(list []AgentCardView, mirrors []MirrorCardView) g.Node {
	return agentsListNodes(list, mirrors, 0)
}

// agentsListNodes is AgentsCardsWithMirrors with the Idle section's threshold
// (after) for its header's hint.
func agentsListNodes(list []AgentCardView, mirrors []MirrorCardView, after time.Duration) g.Node {
	if len(list) == 0 && len(mirrors) == 0 {
		return AgentsCards(nil)
	}
	var pinned, working, idle []listEntry
	for _, e := range orderList(list, mirrors) {
		switch {
		case e.pinned():
			// A chip, not a card (pinned_chips.go). A pinned entry is never
			// idle (InIdleSection), so it is never in the Idle section either.
			pinned = append(pinned, e)
		case e.idle():
			idle = append(idle, e)
		default:
			working = append(working, e)
		}
	}
	out := g.Group{pinnedChips(pinned)}
	out = append(out, g.Map(working, listCard)...)
	if len(idle) > 0 {
		out = append(out, idleHead(len(idle), after))
		out = append(out, g.Map(idle, listCard)...)
	}
	return out
}

// mirrorSessionTitle is a mirrored session's name as a page title: the
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
// two "mango:1" cards read as two different titles. The rendered view and the
// terminal page draw their title here; the card through mirrorCardTitle, the
// same title without the raw session name.
func mirrorEntryTitle(label, title, session, qualifier, class string) g.Node {
	return entryTitle(label, title, session, qualifier, class, true)
}

// mirrorCardTitle is mirrorEntryTitle as the Agents list's CARD draws it: the
// label and its qualifier, WITHOUT the raw tmux session name beside it
// (an operator ask: the resolved title only, not the raw session name).
// Nothing that tells two cards apart is lost with it: two entries
// sharing a label on one host carry their tmux address in the qualifier
// (mirror.qualify), and an entry with no label is titled "<session>:<index>".
// The session is still the name's tooltip and a search term (cardFilterAttrs);
// the window's own pages, opened from the card, still show it.
func mirrorCardTitle(label, title, session, qualifier, class string) g.Node {
	return entryTitle(label, title, session, qualifier, class, false)
}

// entryTitle is mirrorEntryTitle, drawing the muted session name beside a
// label only when withSession is set.
func entryTitle(label, title, session, qualifier, class string, withSession bool) g.Node {
	q := g.If(qualifier != "", Span(g.Attr("data-mirror-qualifier", ""), g.Text(mirrorQualifierSep+qualifier)))
	if label == "" || label == session {
		return Span(Class(class), g.Attr("data-mirror-name", ""), g.Text(plainOr(title, session)), q)
	}
	return Span(
		Class(class),
		g.Attr("data-mirror-name", ""),
		g.Attr("title", "tmux session "+session),
		Span(g.Attr("data-mirror-label", ""), g.Text(plainOr(label, session))),
		q,
		g.If(withSession, Span(g.Attr("data-mirror-session", ""), Class("ml-1.5 text-xs text-muted"), g.Text(session))),
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
// leads with the HOST'S icon (host_icons.go: the one its cards draw), hostIcon
// being its key and host what it names.
func mirrorBadge(text, host, hostIcon string) g.Node {
	d := hostIconDefOf(hostIcon, host)
	return Span(
		g.Attr("data-host-icon", d.Key),
		Class("inline-flex items-center gap-1 rounded-full bg-st-progress-bg px-2.5 py-1 text-xs font-semibold text-st-progress-fg ring-1 ring-inset ring-st-progress-fg/40"),
		g.Raw(hostIconSVG(d, "h-3.5 w-3.5")),
		g.Text(text),
	)
}
