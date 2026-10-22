package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE "RECENTLY VIEWED" STRIP: the entries the viewer last opened (an agent's
// page, a mirrored window's view or terminal), as one tap each, above the
// Agents list. Stored per user on the server (internal/recents, migration
// 0018), so it is the same on every device.

// RecentRef is one recently opened entry, as the store keys it: an agent by id
// (Mirror zero), or a mirrored entry by its address (AgentID 0).
type RecentRef struct {
	AgentID int64
	Mirror  MirrorRef
}

// RecentMax is how many entries the strip shows.
const RecentMax = 6

// recentItem is one entry the strip draws, resolved against the current list.
type recentItem struct {
	Href, Title, Status string
	Mirror              bool
	// Host and HostIcon are the entry's host and its icon key — the card's
	// (host_icons.go): the cluster for an agent, the mirrored host's otherwise.
	Host, HostIcon string
}

// recentItems resolves refs (newest first) against the cards the list holds
// now, and keeps the first RecentMax that are:
//
//   - STILL THERE — an agent in list, a window in mirrors. A deleted agent, or a
//     window its host no longer lists, is skipped, not drawn dead;
//   - NOT PINNED — a pinned entry already leads the list, so the strip does not
//     spend a slot repeating it.
//
// Resolving here, against the same views the cards are drawn from, is what
// makes "exists" and "pinned" mean exactly what the list beside it shows.
func recentItems(list []AgentCardView, mirrors []MirrorCardView, refs []RecentRef) []recentItem {
	agentsByID := make(map[int64]AgentCardView, len(list))
	for _, a := range list {
		agentsByID[a.ID] = a
	}
	mirrorsByRef := make(map[MirrorRef]MirrorCardView, len(mirrors))
	for _, m := range mirrors {
		mirrorsByRef[MirrorRef{Host: m.Host, Session: m.Session, Window: m.Window}] = m
	}
	var out []recentItem
	for _, r := range refs {
		if len(out) == RecentMax {
			break
		}
		if r.AgentID != 0 {
			a, ok := agentsByID[r.AgentID]
			if !ok || a.Pinned {
				continue
			}
			out = append(out, recentItem{Href: "/agents/" + a.Name, Title: plainOr(a.DisplayName, a.Name), Status: a.Status,
				Host: podHost, HostIcon: clusterIcon.Key})
			continue
		}
		m, ok := mirrorsByRef[r.Mirror]
		if !ok || m.Pinned {
			continue
		}
		out = append(out, recentItem{Href: MirrorCardHref(m), Mirror: true, Status: mirrorFields(m).Status,
			Title: mirrorQualified(plainOr(displayOr(m.Label, m.Title), m.Session), m.Qualifier), Host: m.Host, HostIcon: m.HostIcon})
	}
	return out
}

// recentStripID is the strip's element: drawn by the shell, and MORPHED out of
// band by every list render (RenderAgentsList).
const recentStripID = "agents-recent"

// recentStrip is the condensed horizontal row of recently viewed entries. It
// scrolls sideways inside itself, so six long names never widen the page. With
// nothing to show it is present but hidden (the swap target stays).
func recentStrip(items []recentItem, oob bool) g.Node {
	return Nav(
		ID(recentStripID),
		g.Attr("aria-label", "Recently viewed"),
		g.If(len(items) == 0, g.Attr("hidden", "")),
		// 🔴 MORPHED, NOT REPLACED: "true" is an outerHTML replace, which made
		// the sideways scroller below a new element — at scrollLeft 0 — on every
		// list render (agents_scroll.go).
		//
		// 🔴 AND IT IS "morph", NEVER "morph:outerHTML". htmx 2 reads an OOB
		// value "X:Y" as swap style X into the element matching the CSS
		// SELECTOR Y: "morph:outerHTML" looked for an <outerHTML> element,
		// matched nothing, and the copy was DROPPED — the strip never changed
		// after its first paint (e2e/tests/host-icons.spec.ts, "a list refresh
		// with no navigation changes what the strip shows").
		g.If(oob, hx("hx-swap-oob", "morph")),
		Class("mb-3 min-w-0"),
		// A sideways swipe here is the strip's, not the sidebar edge-swipe's
		// (appScript's ownsSideways): the strip starts inside the 28px edge zone.
		g.Attr("data-own-swipe", ""),
		g.If(len(items) > 0, g.Group{
			H3(Class("mb-1 text-[11px] font-medium uppercase tracking-wide text-muted"), g.Text("Recent")),
			Div(
				Class("-mx-1 flex gap-2 overflow-x-auto px-1 pb-1"),
				g.Group(g.Map(items, recentEntry)),
			),
		}),
	)
}

// recentEntry is one tap target: a real-load link (hx-boost="false", like a
// card's) to the entry's page — an agent's detail, a window's rendered view or
// terminal, exactly where its card opens.
func recentEntry(it recentItem) g.Node {
	kind := "agent"
	if it.Mirror {
		kind = "mirror"
	}
	return A(
		Href(it.Href),
		hx("hx-boost", "false"),
		g.Attr("data-recent-entry", kind),
		g.Attr("title", it.Title),
		Class("press inline-flex min-h-[44px] max-w-[14rem] shrink-0 items-center gap-2 rounded-xl bg-s1/70 px-3 text-sm font-medium text-fg ring-1 ring-inset ring-line transition hover:bg-s2"),
		statusIcon(it.Status),
		// Its host's icon — the one its card draws (host_icons.go).
		hostMark(it.Host, it.HostIcon),
		Span(Class("truncate"), g.Text(it.Title)),
	)
}
