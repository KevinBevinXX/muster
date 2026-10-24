package ui

import (
	"regexp"
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// PINNED ENTRIES ARE CHIPS, NOT CARDS (operator ask: "Pinned agents should show
// only the 'pretty Tmux' label (for example mango, vapor, wheat) not the full
// agent card").
//
// A pinned entry — a mirrored window or a dispatched agent (internal/pins) — is
// drawn at the top of the Agents list as a compact chip whose ONLY content is
// its name (pinnedChipLabels): the window's codename ("mango", from the host's
// "mango:1") or the agent's name. No focus title, no host chip, no kind or repo
// chip, no footer. Tapping it opens what the card's nav anchor opens
// (MirrorCardHref, "/agents/<name>"). The pinned entry is NOT also drawn as a
// card: the chip replaces it.
//
// 🔴 EACH CHIP IS STILL A DIRECT <article> CHILD OF #agents-list, carrying the
// card's own id and the card's own filter attributes (mirrorListAttrs /
// agentListAttrs — one function each, used by both). So everything the list
// does to its cards it does to a chip, with no second rule to keep in step: the
// search box and the host tabs hide it (agents_filter.go's `#agents-list >
// article` rules), the "N of M" count and the host tabs' counts include it, the
// filter chips (waiting / idle) select it, and the hold (keepDrawnOrder) keys it
// by the same id — so pinning a card morphs that card into its chip in place.
// THE CHOICE: chips are filtered by the host tab and the search exactly as
// cards are.
//
// HOW THEY WRAP: #agents-list lays its children out as a wrapping flex row
// (web/css/input.css): a card takes the whole row (half of it from lg up), a
// chip only its own width, so the chips sit side by side in rows above the
// cards. `order` draws them first whatever the DOM order, and the filter's
// rank (reorder) also keeps them ahead of every card in the DOM, so Tab and a
// screen reader meet them where the eye does.
//
// UNPINNING. The chip shows no control of its own. The row starts with ONE
// "Edit pins" toggle (pinsEditToggle): pressing it reveals a × on every chip
// (the pin routes' Unpin form, pinForm) — and, on an AGENT's chip, the card's
// own rename button and ⋯ menu (Start/Stop, Delete), so pinning an agent
// never takes its lifecycle controls away — and pressing it again hides them. It was
// chosen over a long-press or a context menu because it is a plain 44px button
// — discoverable, keyboard-operable, and the same on a phone and a desktop —
// and costs the row one icon-sized slot rather than a control per chip. The
// state is the viewer's (data-agents-pins-editing on <html>), so a list
// refresh does not drop it, and it is NOT remembered across loads: the server
// draws every × hidden, so nothing moves on the first paint.
//
// WAITING: a chip whose entry waits on you (data-wait, the same token the card
// carries) shows a small accent dot before its label (pinnedChipWaitDot); an
// agent that crashed or was stopped, a dot of its status's colour
// (pinnedChipStatusDot).

// pinnedChipsHeadID is the "Edit pins" toggle row's id.
const pinnedChipsHeadID = "agents-pins-head"

// pinsEditToggleID is the "Edit pins" toggle's id; the list's script toggles
// data-agents-pins-editing on <html> when it is pressed.
const pinsEditToggleID = "agents-pins-edit"

// windowIndexSuffix is the ":<index>" a host appends to a window's codename
// ("mango:1" — mirrorhost.WireWindows).
var windowIndexSuffix = regexp.MustCompile(`:[0-9]+$`)

// pinnedChipName is an entry's two names: short, what its chip shows when no
// other pinned chip shows the same; full, what it shows when one does.
//
//   - a window with a codename (Label, unless it only repeats the session): full
//     is the label as shown ("mango:1"), short is it without its ":<index>"
//     ("mango");
//   - a window with none: its "<session>:<window index>" title (or the session
//     name, for a per-session host) — both;
//   - an agent: its display name (or name), clamped to cardTitleMax runes with a
//     "…" as its card's is — both.
func pinnedChipName(e listEntry) (short, full string) {
	if e.agent != nil {
		name, clamped := clampTitle(plainOr(e.agent.DisplayName, e.agent.Name))
		if clamped {
			name += "…"
		}
		return name, name
	}
	m := e.mirror
	if m.Label != "" && m.Label != m.Session {
		full = plainOr(m.Label, m.Session)
		short = windowIndexSuffix.ReplaceAllString(full, "")
		if short == "" {
			short = full
		}
		return short, full
	}
	full = plainOr(m.Title, m.Session)
	return full, full
}

// pinnedChipLabels is what each pinned entry's chip says, in the entries'
// order: its short name, unless another chip's would read the same — then its
// full name ("mango:2"), and when even that is shared (one label on two hosts)
// the full name with the window's qualifier ("mango:2 · laptop"), the minimal
// difference the list already decided (mirror.SessionView.Qualifier).
func pinnedChipLabels(es []listEntry) []string {
	out := make([]string, len(es))
	fulls := make([]string, len(es))
	n := map[string]int{}
	for i, e := range es {
		out[i], fulls[i] = pinnedChipName(e)
		n[out[i]]++
	}
	for i := range es {
		if n[out[i]] > 1 {
			out[i] = fulls[i]
		}
	}
	n = map[string]int{}
	for _, l := range out {
		n[l]++
	}
	for i, e := range es {
		if n[out[i]] > 1 && e.mirror != nil && e.mirror.Qualifier != "" {
			out[i] = mirrorQualified(out[i], e.mirror.Qualifier)
		}
	}
	return out
}

// pinnedChip is one pinned entry's chip, labelled label (pinnedChipLabels).
func pinnedChip(e listEntry, label string) g.Node {
	var id, href, kind string
	var attrs, icon, unpin, status, edit g.Node
	wait := ""
	if e.agent != nil {
		a := e.agent
		ids := strconv.FormatInt(a.ID, 10)
		id, href, kind = "agent-"+ids, a.PageHref(), "agent"
		attrs = agentListAttrs(*a)
		icon = clusterMark()
		wait = waitTokens(a.Attention, a.AwaitingApproval)
		status = pinnedChipStatusDot(a.Status)
		unpin = pinForm(true, [][2]string{{"kind", "agent"}, {"agent_id", ids}}, chipUnpinButton(label))
		// A pinned agent keeps its card's controls — rename, and the ⋯ menu's
		// Start/Stop and Delete — behind "Edit pins": the SAME functions the
		// card draws them with, not copies.
		edit = g.Group{agentRenameButton(ids), agentMenu(*a)}
	} else {
		m := e.mirror
		id, href, kind = mirrorCardID(*m), MirrorCardHref(*m), "mirror"
		attrs = mirrorListAttrs(*m)
		icon = hostMark(m.Host, m.HostIcon)
		wait = waitTokens(m.Attention, false)
		unpin = pinForm(true, [][2]string{{"kind", "mirror"}, {"host", m.Host}, {"session", m.Session}, {"window", m.Window}}, chipUnpinButton(label))
	}
	return Article(
		ID(id),
		g.Attr("data-pinned", ""),
		g.Attr("data-pinned-chip", kind),
		attrs,
		Class("flex min-w-0 max-w-full items-center rounded-full border border-line bg-s1/70 ring-1 ring-line"),
		// The entry's host icon, NOT drawn: the host tabs take a host's icon
		// from its first drawn entry (agentsFilterScript syncHosts), and a host
		// whose every entry is pinned would otherwise get a tab without one.
		Span(Class("hidden"), g.Attr("aria-hidden", "true"), icon),
		A(
			Href(href),
			// A real navigation, as the card's nav anchor (agentCard): htmx attrs
			// on #agents-list are inherited, and a boosted tap would swap the
			// page into the list.
			hx("hx-boost", "false"),
			g.Attr("data-pinned-chip-link", ""),
			g.Attr("title", label),
			Class("press inline-flex min-h-[44px] min-w-[44px] max-w-full items-center justify-center gap-1.5 rounded-full px-4 text-sm font-semibold text-fg transition hover:bg-s2"),
			g.If(wait != "", pinnedChipWaitDot()),
			status,
			Span(g.Attr("data-pinned-chip-label", ""), Class("truncate"), g.Text(label)),
		),
		// Shown only while the viewer edits pins (web/css/input.css).
		Span(g.Attr("data-pinned-chip-edit", ""), Class("-ml-2 flex shrink-0 items-center"), edit, unpin),
	)
}

// pinnedChips1 is ONE pinned entry's chip, labelled as the list labels it
// alone — what RenderAgentCard swaps in for a pinned agent. An agent's label
// is its name whatever else is pinned (only a window's codename is
// disambiguated), so it is the label the list draws.
func pinnedChips1(e listEntry) g.Node {
	return pinnedChip(e, pinnedChipLabels([]listEntry{e})[0])
}

// pinnedChipStatusDot is an agent chip's status mark, nil while the agent is
// healthy: a dot like the waiting one (pinnedChipWaitDot), in its status's
// colour and named for a screen reader, so a pinned agent that crashed or was
// stopped is not silent behind a bare label.
func pinnedChipStatusDot(status string) g.Node {
	var label, colour string
	switch status {
	case "error":
		label, colour = "Agent error", "bg-st-error-fg"
	case "stopped":
		label, colour = "Agent stopped", "bg-muted"
	default:
		return nil
	}
	return Span(
		g.Attr("data-pinned-chip-status", status),
		g.Attr("role", "img"),
		g.Attr("aria-label", label),
		g.Attr("title", label),
		Class("h-2 w-2 shrink-0 rounded-full "+colour),
	)
}

// pinnedChipWaitDot is the chip's "waiting on you" mark: a small accent dot,
// named for a screen reader.
func pinnedChipWaitDot() g.Node {
	return Span(
		g.Attr("data-pinned-chip-wait", ""),
		g.Attr("role", "img"),
		g.Attr("aria-label", "Waiting on you"),
		g.Attr("title", "Waiting on you"),
		Class("h-2 w-2 shrink-0 rounded-full bg-accent"),
	)
}

// chipUnpinButton is a chip's ×, inside its edit controls (hidden unless the
// viewer is editing pins).
func chipUnpinButton(label string) g.Node {
	return Button(
		Type("submit"),
		g.Attr("data-pinned-chip-unpin", ""),
		g.Attr("aria-label", "Unpin "+label),
		g.Attr("title", "Unpin "+label),
		Class("press inline-flex h-11 w-11 shrink-0 items-center justify-center rounded-full text-muted transition hover:bg-s2 hover:text-fg"),
		hx("hx-disabled-elt", "this"),
		g.Raw(`<svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18"/></svg>`),
	)
}

// pinsEditToggle is the row's "Edit pins" button (see UNPINNING above). It is
// not an <article>, so no filter rule hides it; the list's script hides it
// while the filter shows no chip.
func pinsEditToggle() g.Node {
	return Div(
		ID(pinnedChipsHeadID),
		g.Attr("data-pins-head", ""),
		Class("flex items-center"),
		Button(
			Type("button"),
			ID(pinsEditToggleID),
			g.Attr("aria-pressed", "false"),
			g.Attr("aria-label", "Edit pins"),
			g.Attr("title", "Edit pins"),
			Class("press inline-flex h-11 w-11 items-center justify-center rounded-full text-accent transition hover:bg-s2 aria-pressed:bg-accent aria-pressed:text-on-accent"),
			g.Raw(pinIcon),
		),
	)
}

// pinnedChips is the list's pinned row: the "Edit pins" toggle and one chip per
// pinned entry, in the list's order; nothing when none is pinned.
func pinnedChips(es []listEntry) g.Node {
	if len(es) == 0 {
		return nil
	}
	labels := pinnedChipLabels(es)
	out := g.Group{pinsEditToggle()}
	for i, e := range es {
		out = append(out, pinnedChip(e, labels[i]))
	}
	return out
}
