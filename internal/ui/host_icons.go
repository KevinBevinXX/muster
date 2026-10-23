package ui

import (
	"hash/fnv"
	"strings"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// HOST ICONS: every listed agent carries an icon that says which host it runs
// on — a mirrored tmux window its host's, a dispatched pod agent the cluster's —
// next to its name, with the host in the icon's tooltip and accessible name.
// The same host draws the same icon everywhere it appears: the Agents list's
// cards, the "Recently viewed" strip, the host tabs (cloned off a card by
// agentsFilterScript) and the badge on a mirrored window's own pages.
//
// 🔴 A HOST THE OPERATOR NAMED AN ICON FOR DRAWS IT (HostPreferences.Icons,
// MUSTER_HOST_ICONS: "laptop" a laptop, whatever its rank). EVERY OTHER HOST'S
// ICON IS PICKED BY ITS REGISTRATION RANK, NOT BY A HASH OF ITS NAME. The rank is the host's place
// among its owner's hosts in the order their first mirror token was minted
// (mirror.RanksByRegistration). A hash of
// the name can give two of an operator's first three hosts the same icon; a
// rank cannot until there are more hosts than icons. And a rank is STABLE: a
// host minted later takes the next rank, so adding one never changes an
// existing host's icon (tokens are revoked, never deleted, so no rank is ever
// vacated and reused). A host with no known rank — no token row the viewer can
// read — falls back to the hash: an icon of the set, never a blank box.
//
// 🔴 THE CLUSTER'S ICON IS NOT IN THE SET, so no mirrored host can ever draw
// it: a dispatched agent and a host's window never look alike.

// hostIconDef is one icon: its key (what data-host-icon says, and what a test
// compares) and its SVG body in the repo's icon style (24×24, 2px round
// strokes in currentColor — mirroredIcon's).
type hostIconDef struct {
	Key  string
	Body string
}

// hostIconSet is the curated set a mirrored host's icon is picked from: by
// name (HostPreferences.Icons), else in rank order over rankedIcons. Every key and every
// body is distinct (TestHostIconSetIsDistinct).
var hostIconSet = []hostIconDef{
	{"laptop", `<rect x="4" y="5" width="16" height="11" rx="1"/><path d="M2 19h20"/>`},
	{"server", `<rect x="3" y="4" width="18" height="7" rx="1"/><rect x="3" y="13" width="18" height="7" rx="1"/><path d="M7 7.5h.01M7 16.5h.01"/>`},
	{"terminal", `<rect x="3" y="4" width="18" height="16" rx="2"/><path d="M7 9l3 3-3 3M13 15h4"/>`},
	{"chip", `<rect x="6" y="6" width="12" height="12" rx="1"/><path d="M9 2v4M15 2v4M9 18v4M15 18v4M2 9h4M2 15h4M18 9h4M18 15h4"/>`},
	{"phone", `<rect x="6" y="2" width="12" height="20" rx="2"/><path d="M12 18h.01"/>`},
	{"home", `<path d="M3 10l9-7 9 7v10a1 1 0 0 1-1 1h-5v-6h-6v6H4a1 1 0 0 1-1-1z"/>`},
	{"globe", `<circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3a14 14 0 0 1 0 18M12 3a14 14 0 0 0 0 18"/>`},
	{"box", `<path d="M21 8l-9-5-9 5 9 5 9-5z"/><path d="M3 8v8l9 5 9-5V8M12 13v8"/>`},
	{"bolt", `<path d="M13 2L3 14h9l-1 8 10-12h-9l1-8z"/>`},
	{"star", `<path d="M12 2l3.1 6.3 6.9 1-5 4.9 1.2 6.8-6.2-3.2-6.2 3.2L7 14.2 2 9.3l6.9-1z"/>`},
	{"moon", `<path d="M21 12.8A9 9 0 1 1 11.2 3 7 7 0 0 0 21 12.8z"/>`},
	{"drive", `<path d="M22 12H2M5.5 5.1L2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.5-6.9A2 2 0 0 0 16.8 4H7.2a2 2 0 0 0-1.7 1.1z"/><path d="M6 16h.01M10 16h.01"/>`},
}

// clusterIcon is a dispatched pod agent's: the host "cluster" (podHost). It is
// NOT a member of hostIconSet.
var clusterIcon = hostIconDef{"cluster", `<path d="M12 2l8.7 5v10L12 22l-8.7-5V7z"/><circle cx="12" cy="12" r="3"/>`}

// rankedIcons is hostIconSet without the glyphs the operator gave named hosts
// (HostPreferences.Icons): what a host with no named icon is picked from, in
// rank order.
func rankedIcons() []hostIconDef {
	reserved := map[string]bool{}
	for _, k := range hostPrefs.Icons {
		reserved[k] = true
	}
	var out []hostIconDef
	for _, d := range hostIconSet {
		if !reserved[d.Key] {
			out = append(out, d)
		}
	}
	return out
}

// namedIcon is host's icon by name (HostPreferences.Icons), if it has one.
func namedIcon(host string) (string, bool) {
	k, ok := hostPrefs.Icons[strings.ToLower(host)]
	return k, ok
}

// HostIconFor is the icon key of a mirrored host: its named icon
// (HostPreferences.Icons) when it has one; else by its registration rank in
// ranks (mirror.RanksByRegistration) — rank r draws rankedIcons()[r mod N] — or,
// for a host ranks does not hold, by a hash of its name. A glyph named for a
// host is never drawn for another. This is the one rule; every surface that
// draws a host's icon is handed its result.
func HostIconFor(ranks map[string]int, host string) string {
	if k, ok := namedIcon(host); ok {
		return k
	}
	set := rankedIcons()
	if len(set) == 0 { // every glyph named: nothing is left to keep apart
		set = hostIconSet
	}
	if r, ok := ranks[host]; ok && r >= 0 {
		return set[r%len(set)].Key
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(host))
	return set[h.Sum32()%uint32(len(set))].Key
}

// hostIconDefOf is the icon a key names: the cluster's, one of the set, or —
// for a key that is neither (a view built without one) — the hash fallback of
// HostIconFor for host, so a card is never drawn without its icon.
func hostIconDefOf(key, host string) hostIconDef {
	if key == clusterIcon.Key {
		return clusterIcon
	}
	for _, d := range hostIconSet {
		if d.Key == key {
			return d
		}
	}
	return hostIconDefOf(HostIconFor(nil, host), host)
}

// hostIconSVG is an icon's <svg> at size (a Tailwind h-/w- pair).
func hostIconSVG(d hostIconDef, size string) string {
	return `<svg class="` + size + ` shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">` + d.Body + `</svg>`
}

// hostMark is a host's icon as drawn beside a listed agent's name: a fixed
// 16px box (h-4 w-4 shrink-0 — reserved in the server's markup, so its arrival
// moves nothing), the host in its tooltip and its accessible name. key is the
// icon (HostIconFor, or the cluster's); host is what the tooltip names.
//
// It is NOT a button: it sits inside a card's nav anchor, where a tap opens the
// card. A phone has no hover, so the card's body also spells the host
// (hostChip).
func hostMark(host, key string) g.Node {
	d := hostIconDefOf(key, host)
	label := "Host: " + host
	return Span(
		g.Attr("data-host-icon", d.Key),
		g.Attr("role", "img"),
		g.Attr("aria-label", label),
		g.Attr("title", label),
		Class("inline-flex h-4 w-4 shrink-0 items-center justify-center text-fg2"),
		g.Raw(hostIconSVG(d, "h-4 w-4")),
	)
}

// hostChip is a MIRRORED card's host, SPELLED, in the card's body (header row,
// beside its name), as a chip like the card's kind and repo: "host laptop". A
// phone has no hover for hostMark's tooltip, and the host is what tells two
// same-named windows apart and a filter field (host:). It is not in the footer
// (an operator ask). A dispatched agent's card draws none: its cluster icon and
// the "cluster" tab already say it. The value carries data-mirror-host, which
// tests locate the host by.
func hostChip(host string) g.Node {
	return Span(
		g.Attr("data-card-host", host),
		Class("inline-flex items-center gap-1 rounded-full bg-s2/80 px-2.5 py-1 text-xs font-medium text-fg2 ring-1 ring-inset ring-line"),
		Span(Class("text-muted"), g.Text("host")),
		Span(g.Attr("data-mirror-host", ""), g.Text(host)),
	)
}

// clusterMark is a dispatched pod agent's host mark.
func clusterMark() g.Node { return hostMark(podHost, clusterIcon.Key) }
