package ui

import (
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/web"
)

// THE MIRRORED VIEW'S SNAPSHOT, the server half: the script tag that carries
// the snapshot store (web/static/mirror-snap.js, which says what it stores and
// why), the view's "Updating…" chip, and the purge every auth page runs.
//
// 🔴 THE USER AND THE BUILD ARE SERVER-RENDERED ON THE TAG, AND THE STORE
// CHECKS A RECORD AGAINST THEM, NOT ONLY ITS KEY: a record whose own user or
// build differs from the page's is deleted unread. Nothing the browser chose
// decides whose snapshot is painted.

// SnapStoreDB is the IndexedDB database the store keeps snapshots in. The
// purge deletes it by this name; mirror-snap.js spells it once too, and
// TestTheSnapshotPurgeNamesTheStoresDatabase pins the two together.
const SnapStoreDB = "muster-snap"

// SnapScript is the snapshot store's script, for a page that reads or warms
// snapshots: the rendered view, the Agents list, an agent's page. Deferred, so
// it runs before DOMContentLoaded and never blocks the parser. Nothing at all
// without a signed-in user.
func SnapScript(f Features) g.Node {
	if f.SnapUser == 0 || f.SnapBuild == "" {
		return nil
	}
	return Script(Src(web.Asset("/static/mirror-snap.js")), Defer(),
		g.Attr("data-snap-user", strconv.FormatInt(f.SnapUser, 10)),
		g.Attr("data-snap-build", f.SnapBuild))
}

// mirrorUpdatingChip is the rendered view's "Updating…": shown only while a
// page painted from a snapshot has waited over a second for its socket.
//
// 🔴 IT TAKES NO LAYOUT AT ALL, AND ITS PLACE IS ALWAYS LAID OUT. It is
// absolutely positioned at the right of the status row (which is `relative`),
// so it adds no width to a header that must stay ONE row on a 390px phone
// (e2e sidenav.spec "the Rendered view header is one row" — an in-flow chip
// wrapped it). It is `invisible` (visibility: hidden), never `hidden`
// (display: none), so showing it moves nothing and its box is where it shows.
func mirrorUpdatingChip() g.Node {
	return Span(ID("mv-updating"), g.Attr("aria-hidden", "true"),
		Class("invisible absolute right-0 top-1/2 -translate-y-1/2 whitespace-nowrap rounded-full bg-s2 px-2 py-0.5 text-[11px] text-fg2 ring-1 ring-inset ring-line"),
		g.Text("Updating…"))
}

// SnapPurgeHTML is the inline script every server-written auth page carries
// (internal/api writeAuthDocument, /login among them): it deletes every
// snapshot in this browser. A sign-out always ends there, and so does any sign
// in, so no user's snapshots outlive their session — even when the sign-out's
// own page never ran its script. Self-contained: no shared shell, no other
// script, so the login page stays independent of the rest of the UI.
func SnapPurgeHTML() string {
	return `<script>try{indexedDB.deleteDatabase('` + SnapStoreDB + `')}catch(e){}</script>`
}
