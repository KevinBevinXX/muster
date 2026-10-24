package ui

import (
	"strconv"
	"strings"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE SESSION SWITCHER: a bar BELOW the message input of a session's page — a
// mirrored window's Rendered view and a dispatched agent's chat page — that
// hot-swaps to another session in one tap (operator ask: "add another bar
// component below the message input bar where I can hot swap between active
// sessions").
//
// THREE VIEWS, picked by ONE button on the bar's left that cycles them:
//
//   - Recent: the sessions the viewer last OPENED — the SAME server-side,
//     cross-device list the Agents tab's "Recently viewed" strip reads
//     (internal/recents; recents.go), newest first. Unlike the strip it keeps
//     pinned entries: here the pins are a view of their own, not the head of a
//     list beside it. The page's own session is left out (it is always the
//     newest: the page recorded the visit before it was drawn).
//   - Pins: the viewer's pins (internal/pins), in the list's order.
//   - All: every entry the Agents list draws, in its order (orderList).
//
// In Pins and All the page's own session is drawn as "current" (aria-current,
// not a link), so the bar says where the viewer is.
//
// Every entry is labelled as its pinned chip is (pinnedChipLabels: the
// window's codename, an agent's name — disambiguated only against the other
// entries of the same view; plained through textplain.Title there, once), and opens what its card opens (MirrorCardHref,
// "/agents/<name>") with a real load.
//
// 🔴 SERVER-RENDERED, ALL THREE VIEWS, AT A FIXED HEIGHT — THAT IS THE
// NO-LAYOUT-SHIFT CONTRACT. The bar paints with the page (never empty, then
// filled), its row is h-11 whatever it holds (an entry, an empty-state line),
// and a view switch only toggles which row is shown. The viewer's choice of
// view is a per-device convenience kept in localStorage; the inline script
// right after the bar applies it before the bar is painted, so a page that
// loads in "All" never shows "Recent" first.
//
// 🔴 IT SCROLLS SIDEWAYS INSIDE ITSELF (each row is overflow-x-auto, min-w-0),
// so thirty long names never widen the page.

// SwitcherID is the bar's element id.
const SwitcherID = "session-switcher"

// switcherViewKey is where the viewer's chosen view is kept (localStorage).
const switcherViewKey = "muster.switcher.view"

// The switcher's views, as data-switcher-view and the storage value spell
// them, in the order the toggle cycles them.
const (
	SwitcherRecent = "recent"
	SwitcherPins   = "pins"
	SwitcherAll    = "all"
)

// switcherViews is the cycle: each view's key, its button label, and what its
// row says when it holds nothing.
var switcherViews = []struct{ key, label, empty string }{
	{SwitcherRecent, "Recent", "No other recent sessions"},
	{SwitcherPins, "Pins", "Nothing pinned"},
	{SwitcherAll, "All", "No other sessions"},
}

// switcherRecentMax is how many entries Recent shows.
const switcherRecentMax = 8

// SwitcherCurrent names the page's own session: an agent by id, or a mirrored
// window by its address (AgentID 0). A window addressed by its session alone
// (Window "") is the current one whichever of that session's windows a card
// names.
type SwitcherCurrent struct {
	AgentID int64
	Mirror  MirrorRef
}

// is reports whether e is the page's own session.
func (c SwitcherCurrent) is(e listEntry) bool {
	if e.agent != nil {
		return c.AgentID != 0 && e.agent.ID == c.AgentID
	}
	m := e.mirror
	return c.AgentID == 0 && c.Mirror.Host != "" && m.Host == c.Mirror.Host && m.Session == c.Mirror.Session &&
		(c.Mirror.Window == "" || m.Window == c.Mirror.Window)
}

// Switcher is the bar's content, resolved on the server: one list per view.
type Switcher struct {
	Recent, Pins, All []SwitcherItem
}

// SwitcherItem is one entry of a view.
type SwitcherItem struct {
	Href, Label string
	// Kind is "agent" or "mirror".
	Kind string
	// Current: the page's own session (drawn, not linked).
	Current bool
	// Waiting: the entry waits on the viewer (its card's data-wait).
	Waiting bool
	// Host and HostIcon are the entry's host and its icon key — the pinned
	// chip's (pinnedChip): the cluster for an agent, the window's host's
	// otherwise. HostNamed: the label already spells the host
	// (pinnedChipLabelNamesHost), so the icon is decorative.
	Host, HostIcon string
	HostNamed      bool
}

// NewSwitcher resolves the bar against the entries the Agents list draws
// (list, mirrors — internal/api builds them the way it builds the list) and
// the viewer's recently opened refs (newest first, as the strip's).
func NewSwitcher(list []AgentCardView, mirrors []MirrorCardView, refs []RecentRef, cur SwitcherCurrent) *Switcher {
	entries := orderList(list, mirrors)
	byAgent := map[int64]listEntry{}
	byMirror := map[MirrorRef]listEntry{}
	var pins []listEntry
	for _, e := range entries {
		if e.agent != nil {
			byAgent[e.agent.ID] = e
		} else {
			byMirror[MirrorRef{Host: e.mirror.Host, Session: e.mirror.Session, Window: e.mirror.Window}] = e
		}
		if e.pinned() {
			pins = append(pins, e)
		}
	}
	var recent []listEntry
	seen := map[listEntry]bool{}
	for _, r := range refs {
		if len(recent) == switcherRecentMax {
			break
		}
		var e listEntry
		var ok bool
		if r.AgentID != 0 {
			e, ok = byAgent[r.AgentID]
		} else {
			e, ok = byMirror[r.Mirror]
		}
		// Gone (deleted, no longer listed), the page itself, or a repeat.
		if !ok || cur.is(e) || seen[e] {
			continue
		}
		seen[e] = true
		recent = append(recent, e)
	}
	return &Switcher{Recent: switcherItems(recent, cur), Pins: switcherItems(pins, cur), All: switcherItems(entries, cur)}
}

// switcherItems is es as one view's items, labelled against each other.
func switcherItems(es []listEntry, cur SwitcherCurrent) []SwitcherItem {
	labels := pinnedChipLabels(es)
	out := make([]SwitcherItem, len(es))
	for i, e := range es {
		it := SwitcherItem{Label: labels[i], Current: cur.is(e)}
		if e.agent != nil {
			it.Href, it.Kind = e.agent.PageHref(), "agent"
			it.Waiting = waitTokens(e.agent.Attention, e.agent.AwaitingApproval) != ""
			it.Host, it.HostIcon = podHost, clusterIcon.Key
		} else {
			it.Href, it.Kind = MirrorCardHref(*e.mirror), "mirror"
			it.Waiting = waitTokens(e.mirror.Attention, false) != ""
			it.Host, it.HostIcon = e.mirror.Host, e.mirror.HostIcon
			it.HostNamed = pinnedChipLabelNamesHost(*e.mirror, labels[i])
		}
		out[i] = it
	}
	return out
}

// view is the items of view key.
func (s *Switcher) view(key string) []SwitcherItem {
	switch key {
	case SwitcherPins:
		return s.Pins
	case SwitcherAll:
		return s.All
	}
	return s.Recent
}

// switcherBar is the bar: the view toggle, then one row per view (Recent
// shown, the others hidden until the script picks them). pinned: the bar is
// the last row of a pinned composer that spans the screen (the Rendered
// view's) — it is inset like that composer's rows and padded by the safe-area
// inset, being the last thing above the screen's edge; otherwise (the agent
// page, inside its already-inset pane) it lines up with the form above it.
// Nothing when s is nil.
func switcherBar(s *Switcher, pinned bool) g.Node {
	if s == nil {
		return nil
	}
	rows := make([]g.Node, 0, len(switcherViews))
	for _, v := range switcherViews {
		rows = append(rows, switcherRow(v.key, v.empty, s.view(v.key), v.key != SwitcherRecent))
	}
	return g.Group{
		Nav(
			ID(SwitcherID),
			g.Attr("aria-label", "Switch session"),
			g.Attr("data-switcher-view", SwitcherRecent),
			// A sideways swipe here is the bar's, not the sidebar edge-swipe's
			// (appScript's ownsSideways), as on the Recent strip.
			g.Attr("data-own-swipe", ""),
			// 🔴 ABSOLUTE, ON ROOM ITS CONTAINER RESERVED (switcherRoom): the
			// bar's markup comes after the form, and a browser may paint the
			// page with the form parsed and the bar not yet — measured, the
			// Rendered view's composer then grew 61→106px under the reader
			// (layout shift 0.0047–0.012 at 1280×800). Reserved by the
			// container's padding from its first byte, the bar's arrival moves
			// nothing.
			Class("absolute inset-x-0 bottom-0 border-t border-line bg-bg"+classIf(pinned, " px-3 pb-[env(safe-area-inset-bottom)]")),
			Div(
				Class("flex h-11 min-w-0 items-center gap-2"),
				Button(
					ID("switcher-toggle"),
					Type("button"),
					g.Attr("aria-label", switcherToggleLabel(SwitcherRecent)),
					g.Attr("title", switcherToggleLabel(SwitcherRecent)),
					// A fixed width: the label changes on a tap, the row beside it
					// must not move. 44px tall (the row's height), its face 32px.
					Class("press group inline-flex h-11 w-[4.5rem] shrink-0 items-center"),
					Span(
						Class("inline-flex h-8 w-full items-center justify-center gap-1 rounded-lg bg-s2 text-xs font-semibold text-fg2 ring-1 ring-inset ring-line transition group-hover:bg-s3"),
						g.Raw(`<svg class="h-3.5 w-3.5 shrink-0" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true"><path fill-rule="evenodd" d="M2 5.75A.75.75 0 012.75 5h14.5a.75.75 0 010 1.5H2.75A.75.75 0 012 5.75zm0 4.25a.75.75 0 01.75-.75h14.5a.75.75 0 010 1.5H2.75A.75.75 0 012 10zm0 4.25a.75.75 0 01.75-.75h14.5a.75.75 0 010 1.5H2.75a.75.75 0 01-.75-.75z" clip-rule="evenodd"/></svg>`),
						Span(g.Attr("data-switcher-label", ""), g.Text(switcherViews[0].label)),
					),
				),
				g.Group(rows),
			),
		),
		switcherScript(),
	}
}

// switcherRoom is the bottom padding a container holding the bar reserves
// for it: its 44px row and 1px border (2.8125rem), plus the safe-area inset
// when pinned. The bar is absolutely placed in that padding (switcherBar).
func switcherRoom(pinned bool) string {
	if pinned {
		return "pb-[calc(2.8125rem+env(safe-area-inset-bottom))]"
	}
	return "pb-[2.8125rem]"
}

// classIf is s when ok, "" otherwise: an optional class suffix.
func classIf(ok bool, s string) string {
	if ok {
		return s
	}
	return ""
}

// switcherToggleLabel is the toggle's accessible name while key is shown.
func switcherToggleLabel(key string) string {
	for i, v := range switcherViews {
		if v.key == key {
			next := switcherViews[(i+1)%len(switcherViews)]
			return "Showing " + v.label + " sessions; show " + next.label
		}
	}
	return ""
}

// switcherRow is one view's row: its entries, scrolling sideways, or the
// line saying it holds none.
func switcherRow(key, empty string, items []SwitcherItem, hidden bool) g.Node {
	body := g.Group(g.Map(items, switcherEntry))
	if len(items) == 0 {
		body = g.Group{P(g.Attr("data-switcher-empty", ""), Class("truncate text-xs text-muted"), g.Text(empty))}
	}
	return Div(
		g.Attr("data-switcher-list", key),
		g.If(hidden, g.Attr("hidden", "")),
		Class("flex min-w-0 flex-1 items-center gap-1.5 overflow-x-auto"),
		body,
	)
}

// switcherEntry is one entry: a real-load link (hx-boost="false", like a
// card's), or — the page's own session — the same chip, not a link.
func switcherEntry(it SwitcherItem) g.Node {
	// 🔴 A 44px TAP TARGET AROUND A 32px PILL: the entry fills the bar's h-11
	// row (every control in the app is a 44px target; e2e/tests/layout.spec.ts
	// measures it), and only the pill inside it is drawn.
	const pill = "inline-flex h-8 min-w-0 max-w-full items-center gap-1.5 rounded-full px-3 text-xs font-semibold ring-1 ring-inset "
	const box = "inline-flex h-11 max-w-[10rem] shrink-0 items-center"
	// The host's icon, as the pinned chip draws it (#252): hostMark's fixed
	// 16px box, server-rendered, so it moves nothing; decorative when the
	// label already names the host.
	icon := hostMark(it.Host, it.HostIcon)
	if it.HostNamed {
		icon = hostMarkDecorative(it.Host, it.HostIcon)
	}
	inner := g.Group{
		g.If(it.Waiting, Span(g.Attr("data-switcher-wait", ""), g.Attr("role", "img"), g.Attr("aria-label", "Waiting on you"),
			Class("h-2 w-2 shrink-0 rounded-full bg-accent"))),
		g.If(it.Host != "", icon),
		Span(Class("truncate"), g.Text(it.Label)),
	}
	if it.Current {
		return Span(
			g.Attr("data-switcher-entry", it.Kind),
			g.Attr("data-switcher-current", ""),
			g.Attr("aria-current", "page"),
			g.Attr("title", it.Label+" (this session)"),
			Class(box),
			Span(Class(pill+"bg-accent/15 text-accent ring-accent/40"), inner),
		)
	}
	return A(
		Href(it.Href),
		hx("hx-boost", "false"),
		// A link to a window's Rendered view is a warming source for the view
		// snapshots (a[data-snap-warm]); inert until something reads it.
		g.If(strings.HasPrefix(it.Href, "/mirror/view?"), g.Attr("data-snap-warm", "")),
		g.Attr("data-switcher-entry", it.Kind),
		g.Attr("title", it.Label),
		Class("press group "+box),
		Span(Class(pill+"bg-s1/70 text-fg ring-line transition group-hover:bg-s2"), inner),
	)
}

// switcherScript applies the viewer's stored view at once (it runs as the bar
// is parsed, before it is painted) and cycles the view on the toggle. Storage
// that throws (a private window, blocked site data) leaves Recent, and the
// toggle still works for the page.
func switcherScript() g.Node {
	views, labels, names := "[", "{", "{"
	for i, v := range switcherViews {
		if i > 0 {
			views, labels, names = views+",", labels+",", names+","
		}
		views += strconv.Quote(v.key)
		labels += strconv.Quote(v.key) + ":" + strconv.Quote(v.label)
		names += strconv.Quote(v.key) + ":" + strconv.Quote(switcherToggleLabel(v.key))
	}
	return Script(g.Raw(`
(function () {
  var bar = document.getElementById('` + SwitcherID + `');
  // A property, not an attribute: htmx's history snapshot carries attributes,
  // and after Back an attribute guard left the restored toggle unbound.
  if (!bar || bar.__musterSwitcherBound) return;
  bar.__musterSwitcherBound = true;
  var VIEWS = ` + views + `], LABELS = ` + labels + `}, NAMES = ` + names + `};
  var KEY = '` + switcherViewKey + `';
  var btn = document.getElementById('switcher-toggle');
  function show(v) {
    if (VIEWS.indexOf(v) < 0) v = VIEWS[0];
    bar.setAttribute('data-switcher-view', v);
    var rows = bar.querySelectorAll('[data-switcher-list]');
    for (var i = 0; i < rows.length; i++) rows[i].hidden = rows[i].getAttribute('data-switcher-list') !== v;
    btn.querySelector('[data-switcher-label]').textContent = LABELS[v];
    btn.setAttribute('aria-label', NAMES[v]);
    btn.setAttribute('title', NAMES[v]);
  }
  var stored = null;
  try { stored = localStorage.getItem(KEY); } catch (e) {}
  if (stored) show(stored);
  btn.addEventListener('click', function () {
    var v = VIEWS[(VIEWS.indexOf(bar.getAttribute('data-switcher-view')) + 1) % VIEWS.length];
    show(v);
    try { localStorage.setItem(KEY, v); } catch (e) {}
  });
})();
`))
}
