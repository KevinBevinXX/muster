// Package ui renders the muster web UI with gomponents. It is dark-first
// (the document root carries the `dark` class), mobile-first, and driven by
// htmx + the htmx SSE extension so the pending-request list stays live without
// a custom client framework.
//
// All request-derived content (commands, context output, project/host names)
// is treated as untrusted-ish and HTML-escaped by gomponents' Text/Textf
// nodes. Only the static, author-controlled scaffolding uses Raw.
package ui

import (
	_ "embed"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/web"
)

// hx is a thin helper for htmx (and other non-standard) attributes, which the
// html package does not provide first-class constructors for.
func hx(name, value string) g.Node { return g.Attr(name, value) }

// wordmark is the product name, split across two text nodes so the first
// syllable can be coloured.
//
// 🔴 IT IS ONE FUNCTION BECAUSE THE SPLIT IS A RENAME HAZARD, AND THE HAZARD IS
// MEASURED RATHER THAN IMAGINED. Upstream spelled this inline at both call
// sites — the header and the sidebar — and because the name is never written
// whole in the source, every grep for the upstream product name walked straight
// past both. The extraction's scrub therefore left the old brand rendering in
// muster's header and sidebar, with nothing erroring; the page simply said the
// wrong name.
//
// Two copies also meant a HALF rename was possible, and that is the shape that
// survives a naive guard: a test asking "does the document contain the right
// name somewhere" passes while one of the two sites still shows the old one.
// With a single function there is no half to rename, and
// TestTheWordmarkSpellsMuster can assert the WHOLE reassembled string against
// this one node instead of hunting for a substring in a document.
func wordmark() g.Node {
	return g.Group{
		Span(Class("text-accent"), g.Text("mu")),
		g.Text("ster"),
	}
}

// Page renders muster's full document shell. dark is the default theme: <html>
// carries the `dark` class, and light is reachable only by removing it.
//
// 🔴 IT TAKES ONLY THE ACTIVE TAB. Upstream's shell also took the pending
// permission-request list plus two auto-approve snapshots, and rendered the
// request queue server-side into the first paint. None of those exist in
// muster: the permission-routing surface, the request store and the
// auto-approve window machinery all stay with the router. Every tabpanel here
// is lazy — it fetches its own partial on `load` — so there is nothing for the
// first paint to embed.
// tab is one entry in muster's navigation: the key (which is also the path
// segment and the tabpanel id suffix), the sidebar label, the sidebar glyph and
// the document <h1> text.
//
// 🔴 ONE REGISTRY, NOT FOUR PARALLEL LISTS. Upstream spelled the tab set FOUR
// times — a `tabKeys` slice, a `tabHeadings` map, a hand-written run of
// sidebarTab(...) calls, and a `TABS` array inside the browser script — and its
// own comment records what that cost: a tab added to the slice but not the map
// rendered a generic <h1> and left the PREVIOUS tab's heading in place on an SPA
// switch, silently, because a tab missing from BOTH lists leaves the two in
// perfect agreement and a comparison between them sees nothing.
//
// muster derives all four from this one slice, so the failure has nowhere to
// live: the sidebar is g.Map'd over it, the headings map is built from it, and
// appScript emits the JS arrays from it. TestNavigationIsDerivedFromOneRegistry
// pins that derivation — it is a relationship guard, not a spelling guard, so
// renaming a tab cannot satisfy it while leaving a consumer behind.
type tab struct {
	Key     string
	Label   string
	Icon    string
	Heading string
}

// musterTabs is the ordered set of SPA tabs. Each key is a real route.
//
// 🔴 THIS IS NOT THE UPSTREAM TAB SET, AND THE DIFFERENCE IS THE POINT OF THIS
// FILE EXISTING SEPARATELY. Upstream's eight tabs include five surfaces muster
// does not serve — the permission-request queue, attention, suggestions, tmux
// and layout all stay with the permission router. Carrying them across would
// give muster a navigation bar that is mostly dead links, which is precisely the
// "notional separate service" outcome the extraction exists to avoid.
//
// ⚠ `runbooks` AND `privileges` USED TO BE TABS HERE. Both subsystems were
// removed (privileges superseded by connectors, runbooks unused); deleting their
// entries from this slice removed the links, the panels, the headings and the
// shell routes together, because they are all derived from it.
var musterTabs = []tab{
	{Key: "tasks", Label: "Tasks", Icon: "📋", Heading: "Tasks"},
	{Key: "agents", Label: "Agents", Icon: "🧩", Heading: "Agents"},
	{Key: "repos", Label: "Repos", Icon: "📦", Heading: "Repos"},
	{Key: "connectors", Label: "Connectors", Icon: "🔌", Heading: "Connectors"},
}

// defaultTab is what an unknown or empty tab string clamps to, and what `/`
// serves. Upstream's default is its permission-request queue, which muster does
// not have.
const defaultTab = "tasks"

// tabKeys is the ordered set of tab keys, derived from musterTabs.
var tabKeys = func() []string {
	out := make([]string, 0, len(musterTabs))
	for _, t := range musterTabs {
		out = append(out, t.Key)
	}
	return out
}()

// TabKeys returns the ordered tab keys, for a caller that must register one
// route per tab.
//
// 🔴 IT RETURNS A COPY. tabKeys is package state that every nav render reads;
// handing a caller the backing array would let an append in another package
// reorder or truncate the navigation. The cost is one small allocation per
// call, and the only caller calls it once at registration.
//
// 🔴 THIS EXISTS SO THE ROUTE TABLE IS DERIVED FROM THE NAV RATHER THAN TYPED
// BESIDE IT. Upstream those were two hand-written lists and its own comment
// records what that cost: a "sixth tab registry" no structural test could see,
// where a tab in the nav and absent from the routes still rendered — from the
// fallback branch, with the wrong heading, silently. A linked tab whose route
// does not exist is a 404 in the navigation; deriving one from the other makes
// that unrepresentable.
func TabKeys() []string {
	out := make([]string, len(tabKeys))
	copy(out, tabKeys)
	return out
}

// normalizeTab clamps an arbitrary tab string to a known tab (default tasks).
func normalizeTab(t string) string {
	for _, k := range tabKeys {
		if t == k {
			return t
		}
	}
	return defaultTab
}

// tabHeadings is the shell's per-tab <h1> text. The shell serves every tab URL
// from ONE document, so the heading is derived from the active tab rather than
// being a single constant repeated across all of them. appScript's show()
// carries the SAME strings so an in-page tab switch keeps the heading truthful.
//
// 🔴 DERIVED FROM musterTabs, NOT MAINTAINED BESIDE IT. Upstream kept this as a
// hand-written map and its own comment records the resulting defect: a tab
// present in the key list but absent here rendered a generic <h1> and left the
// previous tab's heading in place on an SPA switch, and no comparison between
// the two lists could see it because a tab missing from both leaves them in
// agreement. Building the map from the registry removes the failure mode rather
// than testing for it.
var tabHeadings = func() map[string]string {
	out := make(map[string]string, len(musterTabs))
	for _, t := range musterTabs {
		out[t.Key] = t.Heading
	}
	return out
}()

// tabHeading is the <h1> text for a tab (already normalized by the caller).
func tabHeading(tab string) string {
	if h, ok := tabHeadings[normalizeTab(tab)]; ok {
		return h
	}
	return "muster"
}

// ⚠ SEAM — THIS DOCUMENT REFERENCES SIX FILES UNDER /static/ THAT muster DOES
// NOT YET CARRY, AND EVERY ONE OF THEM FAILS SILENTLY.
//
//	WHAT: the head below loads /static/vendor/htmx.min.js,
//	  /static/vendor/idiomorph-ext.min.js, /static/vendor/sse.js, the two Faro
//	  scripts (conditionally) and /static/icons/icon-192.png. web/static/ holds
//	  exactly one file today — app.css, built by `make css` — so the other five
//	  are 404s, and /static/ is not even registered as a route yet.
//	WHY IT IS NOT VISIBLE HERE: a missing <script src> does not error the page.
//	  It renders, styled, and then does nothing: no htmx means every panel stays
//	  a skeleton because no hx-get ever fires, no idiomorph means every
//	  `morph:` swap silently degrades to a destructive one, no sse.js means the
//	  page is write-only. Nothing in this package can observe that — these
//	  renderers emit markup, and the markup is correct.
//	WHY THEY ARE NOT CARRIED IN THIS CHUNK: they are third-party binaries whose
//	  home is web/static/vendor/, served by a /static/ route this chunk
//	  deliberately does not register, and embedded by a web package that would
//	  make `go build ./...` depend on their presence.
//	CLOSING CONDITION: the API carve lands the web package, vendors these five
//	  files, registers `GET /static/`, and a test asserts that every
//	  `/static/...` path this package emits resolves to a file the embed
//	  actually contains — derived by scanning the sources, not by listing the
//	  paths a second time.
//	WHO CHECKS IT: the reviewer of the API-carve pull request, against that
//	  derived test.
func Page(activeTab string, feat Features) g.Node {
	return PageWith(activeTab, feat, ShellData{})
}

// PageWith is Page with state drawn on the first paint (ShellData).
func PageWith(activeTab string, feat Features, data ShellData) g.Node {
	return Doctype(shell(activeTab, feat, g.Text(""), data))
}

// ShellData is state a shell draws on its FIRST PAINT, rather than after the
// panel's own load, because drawing it later would move what is below it.
//
// 🔴 IT EXISTS FOR LAYOUT SHIFT, AND ITS ONE FIELD IS THE MEASURED CASE. The
// Agents tab's "Recently viewed" strip is drawn empty and hidden by the shell
// and filled out of band by the list's first load — and when it appeared it
// pushed the filter bar and every card down: 0.011 at 1280×800 on its own,
// measured by e2e/tests/cls.spec.ts. Drawn here, the strip is already where it
// will be when the list lands, and the out-of-band copy replaces it in place.
// The zero value draws what the shell always drew (an empty, hidden strip).
//
// The Agents tab's own document also draws the LIST (WithList): its cards are
// the first paint, not a request's answer (agents_list_ssr.go).
type ShellData struct {
	recent []recentItem
	list   *shellList
}

// WithRecent resolves the strip's entries the way RenderAgentsList does —
// against the same cards, so "exists" and "pinned" agree with the list.
func (d ShellData) WithRecent(list []AgentCardView, mirrors []MirrorCardView, refs []RecentRef) ShellData {
	d.recent = recentItems(list, mirrors, refs)
	return d
}

// shell is the <html> element of every shell document, with one extra
// body-level node — ComposePage's open-on-load composer, or nothing. Page and
// ComposePage both go through here, so they cannot differ in anything but that
// node. (Each wraps it in its own Doctype so the document registry in
// documents_test.go sees two documents rather than one helper.)
func shell(activeTab string, feat Features, extra g.Node, data ShellData) g.Node {
	activeTab = normalizeVisibleTab(activeTab, feat)
	return g.Group{
		HTML(Class("dark"), Lang("en"),
			Head(
				Meta(Charset("utf-8")),
				Meta(Name("viewport"), Content("width=device-width, initial-scale=1, viewport-fit=cover")),
				Meta(Name("color-scheme"), Content("dark light")),
				ThemeHead(),
				// The Agents list's stored filter, applied before the first paint
				// (agents_filter_head.go) — the list is in the document now.
				agentsFilterHead(),
				Meta(Name("mobile-web-app-capable"), Content("yes")),
				Meta(Name("apple-mobile-web-app-capable"), Content("yes")),
				Meta(Name("apple-mobile-web-app-status-bar-style"), Content("black-translucent")),
				Meta(Name("apple-mobile-web-app-title"), Content("muster")),
				TitleEl(g.Text("muster")),
				manifestLink(),
				Link(Rel("icon"), g.Attr("type", "image/png"), g.Attr("sizes", "192x192"), Href(web.Asset("/static/icons/icon-192.png"))),
				Link(Rel("apple-touch-icon"), Href(web.Asset("/static/icons/icon-192.png"))),
				Link(Rel("stylesheet"), Href(web.Asset("/static/app.css"))),
				Script(Src(web.Asset("/static/vendor/htmx.min.js")), Defer()),
				// idiomorph: morph-style swaps so live list updates patch the DOM
				// in place (no teardown) — preserves scroll, focus, hover, open
				// <details>, and typed text instead of flashing/jumping.
				Script(Src(web.Asset("/static/vendor/idiomorph-ext.min.js")), Defer()),
				Script(Src(web.Asset("/static/vendor/sse.js")), Defer()),
				// Frontend RUM (Grafana Faro). Renders nothing when MUSTER_FARO_URL
				// is unset (local/e2e) — zero telemetry surface.
				faroHead(),
			),
			Body(
				Class("min-h-dvh bg-bg text-fg antialiased selection:bg-accent/30"),
				// SPA navigation: boost internal <a> links + forms into AJAX
				// body-swaps with pushState + working back/forward (no white flash).
				// Init scripts are made boost-safe (they run on htmx:load, not just
				// DOMContentLoaded) so arriving via a boosted nav still wires up.
				//
				// 🔴 The sidebar tabs are <a> elements (sidebarTab returns A(...)),
				// NOT <button>s, so this body-level boost DOES reach them and they
				// must opt OUT explicitly: on THIS document (the shell) every tab
				// renders hx-boost="false" — see sidebarTab's hasPanels comment for
				// why that is load-bearing rather than an optimisation. Do not
				// delete those attributes as redundant: htmx 2.0.4's boosted-anchor
				// listener does not consult event.defaultPrevented, so appScript's
				// preventDefault cannot stop it and BOTH would navigate, pushing two
				// history entries per click. What this boost legitimately owns here
				// is every OTHER internal <a>/<form> (logo→/, agent list→detail, …).
				hx("hx-boost", "true"),
				// SSE wiring lives on a wrapper: connect to /events and, on a
				// created/resolved nudge, refetch the list partial. Events are
				// treated as hints, not an ordered log.
				Div(
					hx("hx-ext", "sse, morph"),
					hx("sse-connect", "/events"),
					// The shell IS the page each tab links to (pathForTab(activeTab)
					// is this document's URL), so activeTab is also currentTab.
					sidebar(activeTab, activeTab, true, feat),
					// Content column: offset right of the persistent sidebar on
					// desktop (lg+); full width on mobile (slide-out sidebar).
					Div(
						Class("lg:pl-72"),
						header(feat.Push),
						pushStatusMessage(),
						Main(
							// Width comes from contentWidth() — the SAME helper the header
							// row uses, so the brand and the content edges cannot drift
							// apart. Only the vertical padding is local to the content
							// column.
							Class(contentWidth()+" pb-[calc(7.5rem+env(safe-area-inset-bottom))] pt-4"),
							// The document's ONE <h1> (axe page-has-heading-one —
							// the shell served all five tab URLs with no h1 at all).
							// It is visually hidden because the design deliberately
							// has no page title bar; the text is the ACTIVE tab's
							// subject, so each tab URL announces itself rather than
							// five URLs sharing one constant. appScript's show()
							// rewrites it when the SPA router switches tab, so it
							// cannot go stale.
							H1(ID("page-heading"), Class("sr-only"), g.Text(tabHeading(activeTab))),
							// 🔴 ONE PANEL PER musterTabs ENTRY, AND THE PANEL IDS ARE
							// DERIVED FROM THE SAME KEYS. A panel whose id does not match
							// `panel-<key>` is not a rendering bug you can see — appScript's
							// switchTab toggles `#panel-<tab>` by id, so the tab simply
							// shows nothing and the previous panel stays visible.
							//
							// NotesPanel takes no pending count here: upstream passed one so
							// the Tasks panel could render the permission-request segment
							// badge, and muster has no requests to count.
							//
							// 🔴 THE REPOS PANEL IS CONDITIONAL, AND ITS ABSENCE IS THE
							// POINT RATHER THAN A TIDY-UP. This panel carries
							// hx-trigger="load" and the shell renders on every tab, so on
							// a deployment with no GitHub store every navigation in the
							// app fetched a route that could only refuse — one failure
							// toast per page view, on pages that have nothing to do with
							// repositories. Mounting it only when there is a store behind
							// it is what makes the tab's absence honest instead of making
							// its presence noisy.
							NotesPanel(activeTab == "tasks"),
							agentsPanel(activeTab == "agents", data.recent, data.list),
							g.If(feat.GitHub, ReposPanel(activeTab == "repos")),
							g.If(feat.Connectors, ConnectorsPanel(activeTab == "connectors")),
						),
					),
					// Keep relative timestamps ('10s', '30m') live + format new cards.
					relTimeScript(),
					// Task modal: dirty-guarded dismiss, save highlight, focus return.
					taskModalScript(),
					// #tasks-list load-failure escape hatch (the skeleton must not
					// become a permanent fake-loading state).
					tasksListScript(),
					// Tag filter chips (persisted AND selection) + the editor's
					// type-Enter-to-chip tag input.
					tagScript(),
					// Legacy /tasks#task-<id> deeplinks: redirect to /tasks/<id>.
					// (It used to scroll + expand the named card once the htmx
					// list settled, and had to run after tagScript because its
					// filter-clear retry called window.cgTagFilterClear. Neither
					// the retry nor that ordering constraint survives — the
					// server-rendered page cannot miss a filtered-out task.)
					taskHashScript(),
					// Task merge (supersede) multi-select: the Select toggle, the live
					// selection count, and the two "keep this one" winner buttons.
					taskMergeScript(),
					// Route a metadata-chip click to the card's own link. The chips are
					// raised above the card-link's stretched overlay so their titles are
					// readable; without this the strip's dominant surface is a dead zone
					// for tap-to-open (measured on noteCard in notes.go — one place, one
					// fixture). Must load AFTER taskMergeScript only in the sense
					// that both are bind-once and order-independent — it reads the
					// `selecting` class merge mode sets, not any of its functions.
					cardMetaNavScript(),
					// Places the agent cards' ⋯ popover menus (floating_menu.go).
					floatingMenuScript(),
					// The Agents list's search box (agents_filter.go).
					agentsFilterScript(),
					// …and the "Recently viewed" strip's sideways offset across
					// hydration (agents_scroll.go).
					agentsScrollScript(),
					// Refetch the list on focus / visibility / SSE reconnect so a
					// closed-and-reopened app is never stale.
					resyncScript(),
					// Sidebar open/close + SPA tab routing (pushState) + FAB toggle.
					appScript(feat),
					// The mirrored views' snapshot store: on the Agents list it warms
					// the pinned and recently viewed windows (mirror_snap.go).
					SnapScript(feat),
					// PWA: the service worker, install, update toast and app badge
					// (pwa.go) — then the push-subscribe flow behind the "Enable"
					// control, which uses the registration pwaScript made.
					pwaChrome(),
					// Only when the server holds VAPID keys (Features.Push): with
					// none, every route this script calls answers 503.
					g.If(feat.Push, pushScript()),
					// 🔴 THE CHIEF PANEL'S OPEN/WIDTH MEMORY, IN THE SHELL — NOT IN THE
					// PANEL. #chief-panel-body is an `hx-swap: innerHTML` target, so a
					// persistence script rendered inside it would be destroyed by the
					// very swap it exists to survive. See chiefPanelScript's header for
					// the other two constraints it is written against.
					chiefPanelScript(),
				),
				// Body-level overlays: rendered outside the content column so their
				// `position: fixed` is always viewport-relative (no ancestor
				// containing-block surprises) and they're not touched by panel
				// swaps. Only the active tab's FAB is shown; the router toggles them.
				shellFAB("fab-tasks", "tasks", "New task", "/ui/tasks/new", "task-modal", "bottom-[calc(1.5rem+env(safe-area-inset-bottom))]", activeTab == "tasks"),
				// Agents tab: dispatch is the only FAB. (Upstream briefly stacked a
				// second one above it linking to a page that was later deleted; a FAB
				// whose whole job is to open a 404 is worse than no FAB, so dispatch
				// takes the primary position rather than floating above a gap.)
				shellFAB("fab-agents", "agents", "Dispatch agent", "/ui/agents/new", "agent-modal", "bottom-[calc(1.5rem+env(safe-area-inset-bottom))]", activeTab == "agents"),
				noteModalShell(),
				agentModalShell(),
				// Transient error toasts for failed background (optimistic) actions.
				actionToasts(),
				// Persistent notification FAB (bottom-LEFT) — the launcher for recent
				// agent replies. Always visible (not per-tab); only the count badge
				// hides at zero. Its panel lazy-loads /ui/notifications.
				notifFAB(),
				// The chief slide-out. Body level for the same reason as badgePopover
				// and the FABs — and for one stronger one: it holds a LIVE WebSocket
				// and whatever the operator has half-typed, so a panel inside <main>
				// would lose both every time any tab panel swapped. That is criterion
				// 3, and it is satisfied by WHERE this line is rather than by anything
				// the panel itself does.
				//
				// ⚠ RENDERED ON EVERY TAB, NOT ONLY ON tmux. The sidebar switches tabs
				// CLIENT-SIDE inside this one document (appScript's switchTab), so a
				// panel rendered only when activeTab == "tmux" would be absent for
				// anyone who reached the grid by clicking rather than by URL. Its body
				// is lazy, so the cost of always rendering it is the shut aside and
				// nothing else.
				ChiefPanel(),
				extra,
			),
		),
	}
}

// panelClass returns the tabpanel class string, hidden unless active — and, for
// a panel whose own contents are DESTRUCTIVELY REPLACED, makes it INELIGIBLE AS
// A SCROLL ANCHOR.
//
// 🔴 `[overflow-anchor:none]` IS THE FIX FOR "ON MOBILE IT PERIODICALLY SCROLLS
// THE PAGE BY ITSELF". On the app shell the SCROLL CONTAINER IS THE DOCUMENT
// (`<body class="min-h-dvh …">`), and htmx's innerHTML swap inserts the new
// children and then removes the old ones — so the node the browser had chosen as
// its scroll anchor is destroyed mid-swap, and the browser's scroll-anchoring
// machinery then MOVES the document's scroll offset, unasked, to keep its
// (degraded) anchor where it was. No script is involved: nothing calls
// `scrollTo`, nothing assigns `scrollTop`, nothing calls `scrollIntoView` or
// `focus()`, and the document height does not change. That is what identifies
// the mechanism — a script-driven jump would show up in a call stack, and a
// height change would explain it without anchoring.
//
// 🔴 THE MECHANISM IS INHERITED; THE PANEL TABLE IS NOT. This behaviour was
// measured upstream, in a mobile browser at 390x844, against a panel set muster
// does not have — the numbers were taken on that project's permission-request,
// tmux and layout panels, none of which exist here, and re-quoting them as if
// they described muster's panels would be a claim about a measurement nobody
// took on this tree. What carries across is the CAUSE (a destructive swap
// destroys the anchor) and the REMEDY (exclude that subtree); what does not
// carry across is any particular scroll offset.
//
// 🔴 THE SECOND PARAMETER IS SET FROM WHAT THE PANEL CONTAINS, NOT FROM HOW THE
// PANEL ITSELF IS SWAPPED. `destructiveSwap` is true when ANY swap wired inside
// the panel discards nodes that are inside it — the panel's own
// `hx-target=#itself` + `innerHTML`, OR a descendant's plain
// `innerHTML`/`outerHTML` onto something the panel contains. Upstream's version
// of this table answered `no` for two panels by reasoning about how their bodies
// ARRIVED ("morph, so the panel survives") rather than what those bodies
// CONTAINED, and both answers were wrong: a per-card status icon polling itself
// on a timer discards nodes inside its panel every ten seconds with nobody
// touching the page.
//
// Read off muster's rendered subtrees, ALL THREE PANELS ANSWER `true`:
//
//	#panel-tasks       morph at the panel level, BUT the delete confirm is
//	                   `outerHTML swap:200ms` onto `#task-<id>`, inside it
//	#panel-agents      morph at the panel level, BUT the card status icon
//	                   (self, innerHTML, on a timer), the recent slot, and the
//	                   rename/delete confirms (`#agent-<id>`, outerHTML) all
//	                   discard nodes inside it
//	#panel-repos       the panel is its own target, innerHTML
//
// ⚠ SO THE `false` BRANCH IS UNEXERCISED IN muster TODAY, AND THAT IS STATED
// RATHER THAN HIDDEN BY DELETING THE PARAMETER. Upstream had exactly one
// non-destructive panel and measured a real benefit from leaving it eligible —
// content that grows ABOVE the viewport is precisely the case anchoring exists
// for. muster has no such panel yet; the knob stays because the predicate is
// then stated in ONE place for whatever panel arrives next, rather than being
// re-derived at a new call site by someone reasoning about morph semantics the
// way upstream did when it got this wrong twice.
// TestEveryTabpanelDeclaresItsSwapHonestly derives the expected value from each
// rendered subtree, so a call site that answers wrong fails in either direction.
//
// ⚠ THE `hidden` BRANCH CARRIES THE OPT-OUT TOO, and that is load-bearing rather
// than symmetry: appScript's `show()` switches tabs CLIENT-SIDE
// (`p.classList.toggle('hidden', t !== tab)`), so a panel that was server-rendered
// hidden becomes the showing one with NO re-render. Whatever the hidden branch
// returned is the class list it swaps under.
//
// ⚠ WHAT IT COSTS, SAID OUT LOUD — AND IT IS THE WHOLE PAGE, NOT JUST THE PANEL.
// Once the operator has scrolled INTO a panel, nothing at or below the scroll
// position is an eligible anchor: the only other in-flow content (the sr-only
// `<h1>` and `#push-status`) sits ABOVE the panels, the header is `sticky`, and
// the sidebar / FABs / modals / chief panel are `fixed`. So anchoring is
// effectively off for the document on every muster tab. That is the intended
// trade rather than an accident — but it IS scroller-level behaviour, so do not
// read this as "only the panel is affected". The one reachable loss today is
// pwaScript un-`hidden`ing `#push-status`, a one-line bar in the content column
// above the panels and therefore uncompensated.
func panelClass(active, destructiveSwap bool) string {
	cls := ""
	if !active {
		cls = "hidden"
	}
	if !destructiveSwap {
		return cls
	}
	if cls == "" {
		return "[overflow-anchor:none]"
	}
	return cls + " [overflow-anchor:none]"
}

// 🔴 DELETION-TRACKING NOTE (task #633 phase 1). `operatorFAB` was here: the
// Agents-tab primary action, a boosted <a href="/operator"> opening the Operator
// chat (and, when no operator agent existed, its provision CTA). The page it
// opened is gone, so the control could only ever navigate to a 404.

// notifFAB renders the persistent notification launcher: a round bottom-LEFT FAB
// (so it never collides with the right-side dispatch/operator/task FABs) carrying
// an overlaid unread-count badge, plus a slide/scale+fade panel anchored above it
// listing recent agent replies. The FAB is ALWAYS visible (it's the launcher for
// recent chats); only the badge hides at zero. The list lazy-loads
// /ui/notifications on load and refreshes on the chat.reply SSE event + a 30s
// poll. The badge (#notif-badge) is updated by the backend via an hx-swap-oob
// span in the /ui/notifications response, so it stays live without extra wiring.
// Open/close is owned by appScript (boost-safe, idempotent) mirroring the sidebar
// setOpen pattern: a backdrop + Esc close it; rows are boosted SPA links so
// tapping one body-swaps to the chat (panel close is moot once we navigate).
func notifFAB() g.Node {
	return g.Group{
		// Backdrop: closes the panel on outside tap. Hidden until open.
		Div(
			ID("notif-backdrop"),
			g.Attr("aria-hidden", "true"),
			Class("hidden fixed inset-0 z-20"),
		),
		// Panel: anchored above the FAB (bottom-left). Starts hidden + translated
		// down/scaled/transparent; appScript toggles the open classes for a
		// slide/scale+fade up. pointer-events guarded so the hidden panel is inert.
		Div(
			ID("notif-panel"),
			g.Attr("role", "dialog"),
			g.Attr("aria-label", "Agent replies"),
			Class("hidden fixed bottom-[calc(6rem+env(safe-area-inset-bottom))] left-[calc(1.5rem+env(safe-area-inset-left))] z-30 w-[22rem] max-w-[calc(100vw-3rem)] origin-bottom-left translate-y-2 scale-95 opacity-0 overflow-hidden rounded-2xl border border-line bg-s1 shadow-2xl shadow-black/50 transition duration-200 ease-out"),
			Div(
				Class("flex items-center gap-2 border-b border-line px-4 py-2.5"),
				Span(Class("text-sm font-semibold text-fg"), g.Text("Agent replies")),
				Span(Class("flex-1")),
			),
			Div(
				ID("notif-list"),
				Class("max-h-[70vh] overflow-auto p-3"),
				hx("hx-get", "/ui/notifications"),
				hx("hx-trigger", "load, sse:chat.reply from:body, every 30s"),
				hx("hx-target", "#notif-list"),
				hx("hx-swap", "innerHTML"),
			),
		),
		// The FAB button: a round 💬 launcher with an overlaid count badge.
		Button(
			ID("notif-fab"),
			Type("button"),
			g.Attr("aria-label", "Agent replies"),
			g.Attr("title", "Agent replies"),
			Class("press fixed bottom-[calc(1.5rem+env(safe-area-inset-bottom))] left-[calc(1.5rem+env(safe-area-inset-left))] z-30 inline-flex h-14 w-14 items-center justify-center rounded-full bg-s2 text-2xl text-fg shadow-xl shadow-black/40 ring-1 ring-inset ring-edge transition hover:bg-s3 active:scale-95"),
			g.Text("💬"),
			// Unread badge overlay. Hidden at zero; the backend's hx-swap-oob span
			// (id=notif-badge) replaces it in place with the live count.
			Span(
				ID("notif-badge"),
				Class("hidden absolute -right-1 -top-1 inline-flex min-w-[1.25rem] items-center justify-center rounded-full bg-accent px-1.5 py-0.5 text-xs font-bold tabular-nums text-on-accent ring-2 ring-bg"),
				g.Text("0"),
			),
		),
	}
}

// modalOpenClick is the ONE "reveal a bottom sheet" click handler. Both FABs and
// both card buttons (Edit, Dispatch) open a sheet the same way — hx-get the body,
// then un-hide the shell — so the "clear the previous open's body first" rule
// lives in one place rather than being re-derived at four call sites. See
// window.cgModalOpen in taskModalScript for why the clear is load-bearing.
func modalOpenClick(modalID string) string {
	return "if(window.cgModalOpen){window.cgModalOpen('" + modalID + "')}else{document.getElementById('" + modalID + "').classList.remove('hidden')}"
}

// shellFAB is a floating "+" action button rendered at the body level (so its
// fixed positioning is viewport-relative). data-fab keys the tab it belongs to;
// the router shows only the active tab's FAB. Tapping it loads the modal body
// (hxGet → #<modalID>-body) and reveals the modal.
func shellFAB(id, tab, ariaLabel, hxGet, modalID, bottomCls string, active bool) g.Node {
	cls := "press fixed " + bottomCls + " right-[calc(1.5rem+env(safe-area-inset-right))] z-30 inline-flex h-14 w-14 items-center justify-center rounded-full bg-accent text-on-accent shadow-xl shadow-accent/30 transition hover:bg-accent/90 active:scale-95"
	if !active {
		cls += " hidden"
	}
	return Button(
		ID(id),
		g.Attr("data-fab", tab),
		Type("button"),
		g.Attr("aria-label", ariaLabel),
		Class(cls),
		hx("hx-get", hxGet),
		hx("hx-target", "#"+modalID+"-body"),
		hx("hx-swap", "innerHTML"),
		hx("hx-on:click", modalOpenClick(modalID)),
		g.Raw(`<svg class="h-7 w-7" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M10 4v12M4 10h12"/></svg>`),
	)
}

// firstLine returns s up to the first newline.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// truncate shortens s to at most n runes, appending an ellipsis when cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// contentWidth is the ONE horizontal measure of the app shell. BOTH the sticky
// header row and the tab content column are sized by it, so they can never
// drift apart — widening only the content column leaves the brand sitting at a
// different left edge from the cards under it, which reads as a broken layout.
// (test: TestHeaderAndContentShareOneWidth.)
//
// The ladder, and why it stops where it does:
//
//	base            ≤1023px   max-w-xl       (36rem / 576px) — phone + tablet
//	lg              ≥1024px   max-w-5xl      (64rem / 1024px) — old desktop measure
//	xl              ≥1280px   max-w-6xl      (72rem / 1152px)
//	2xl             ≥1536px   max-w-[110rem] (1760px)
//	min-[2560px]    ≥2560px   max-w-[150rem] (2400px) — the ultrawide step
//
// 🔴 THE OLD CAP WAS A DELIBERATE DECISION AND IT HAS BEEN DELIBERATELY REVERSED
// (operator, 2026-09-06). It read: "ultrawide is a deliberate cap, not an
// oversight … past 1536px the right answer is more COLUMNS, not longer lines."
// The reasoning was sound and the conclusion was wrong for this app, because the
// dominant content on a wide monitor is not prose — it is tmux pane text, which
// is monospace, pre-wrapped, and gets strictly more readable with width. 1920 and
// 3440 previously shared the last bucket, so an ultrawide monitor showed the same
// 1536px column as a laptop and the rest of the glass went to background.
//
// 🔴 WHAT KEEPS THE OLD ARGUMENT SATISFIED IS proseWidth(), NOT THE CAP. The
// container is now wide; PROSE inside it is capped near 70ch at its own element
// (see proseWidth). That is the split the old comment could not make from the
// shell alone: grids, tables and monospace take the full column, sentences do
// not. Removing proseWidth without restoring a shell cap re-creates exactly the
// 3400px line length the previous decision was protecting against.
//
// ⚠ 2560px is a real device boundary, not a round number: it is the width at
// which a 1440p/ultrawide panel stops being "a big desktop". Tailwind's default
// scale ends at 2xl, so the last step is an arbitrary `min-[2560px]:` variant.
//
// Note the arbitrary values: Tailwind emits them only because it scans
// ./internal/ui/**/*.go (tailwind.config.js `content`). If this string is ever
// built somewhere Tailwind does not scan, the classes silently will not exist.
func contentWidth() string {
	return "mx-auto w-full max-w-xl px-4 lg:max-w-5xl xl:max-w-6xl 2xl:max-w-[110rem] min-[2560px]:max-w-[150rem]"
}

// proseWidth caps a block of SENTENCES at a readable measure, independently of
// the container it sits in.
//
// 🔴 IT IS THE HALF THAT MAKES THE WIDE SHELL SAFE. contentWidth() deliberately
// lets the column reach 2400px so monospace and grids can use it; a paragraph at
// that width is ~300 characters per line, which is roughly three times the point
// where prose stops being readable. 70ch is the conventional measure and is
// expressed in `ch` on purpose — it tracks the font, where a px value would not.
//
// Apply it to the ELEMENT holding the sentences, never to the column: a column
// carrying it would re-cap the grids and tables this change exists to widen.
func proseWidth() string {
	return "max-w-[70ch]"
}

// header is the sticky top bar carrying the hamburger (sidebar toggle), brand,
// the auto-approve control, and the live pending count.
// header is the sticky top bar: hamburger, brand, and the push-notification
// affordances.
//
// 🔴 ITS ONE ARGUMENT IS WHETHER PUSH EXISTS, AND THAT IS A NARROWING RATHER
// THAN A TIDY-UP. Upstream's header carried a live pending-permission-request
// badge and the auto-approve dropdown control; both read state that belongs to
// the permission router and neither has a muster-side source. Keeping either as
// an empty element would render an affordance that can never change — worse
// than absent, because it reads as "zero" rather than "not applicable". The
// push affordances follow the same rule: with no VAPID keys (push false) the
// Enable button is not drawn, because tapping it would ask for the OS
// permission and then fail on a 503.
func header(push bool) g.Node {
	return Header(
		Class("sticky top-0 z-20 border-b border-line bg-bg/80 backdrop-blur supports-[backdrop-filter]:bg-bg/60"),
		Div(
			// Same contentWidth() as the tab content column below — see the helper.
			// The two MUST move together: widening only the content leaves the brand
			// and the content edges misaligned, which reads as a broken layout.
			Class(contentWidth()+" flex items-center gap-3 py-3"),
			// Hamburger: opens the slide-out sidebar (top-left). Hidden on desktop
			// where the sidebar is always visible. Shared with agentDetailHeader so
			// appScript's initPage binds the same #sidebar-open on every page.
			sidebarOpenButton(),
			// Brand wordmark links home. Boosted (Task 3) so tapping it does an
			// AJAX body-swap to / rather than a full reload.
			A(Href("/"),
				g.Attr("aria-label", "muster home"),
				Class("inline-flex min-h-[44px] items-center text-lg font-semibold tracking-tight transition-opacity hover:opacity-80"),
				wordmark(),
			),
			Span(Class("flex-1")),
			// "Enable notifications" affordance. Hidden by default; the PWA client
			// script reveals it only when the browser supports push and the user
			// has not yet granted/subscribed. Tapping it drives the
			// permission-request + pushManager.subscribe flow (needs a user
			// gesture).
			g.If(push, enableNotificationsButton()),
			// Subtle "notifications on" indicator, shown once subscribed.
			g.If(push, pushOnIndicator()),
		),
	)
}

// sidebarOpenButton is the hamburger that opens the slide-out sidebar. It is
// shared by the home shell header() and the agent-detail / operator headers so
// every page carries the same #sidebar-open that appScript's initPage binds.
// Hidden on desktop (lg) where the sidebar is always visible.
func sidebarOpenButton() g.Node { return navOpenButton(false) }

// overlayOpenButton is the hamburger of a page whose sidebar is an overlay at
// every width (sidebarIn's overlay): the same #sidebar-open, never hidden.
func overlayOpenButton() g.Node { return navOpenButton(true) }

func navOpenButton(overlay bool) g.Node {
	display := " lg:hidden"
	if overlay {
		display = ""
	}
	return Button(
		ID("sidebar-open"),
		Type("button"),
		g.Attr("aria-label", "Open menu"),
		g.Attr("aria-controls", "sidebar"),
		g.Attr("aria-expanded", "false"),
		// ≥44x44 tap target (measured at 36x36 by an automated accessibility
		// audit). Only
		// the hit area grows — the icon stays h-5 w-5 — so the compact top bar's
		// layout is unchanged apart from the button box.
		Class("press -ml-1 inline-flex h-11 w-11 min-h-[44px] min-w-[44px] shrink-0 items-center justify-center rounded-lg text-fg2 transition hover:bg-s2 hover:text-fg"+display),
		g.Raw(`<svg class="h-5 w-5" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M3 5h14M3 10h14M3 15h14"/></svg>`),
	)
}

// pushStatusMessage is the small, visible line the push flow writes failures /
// state into (e.g. "permission denied", "push not supported"). Hidden until the
// client sets text. It lives just under the header.
func pushStatusMessage() g.Node {
	return Div(
		ID("push-status"),
		g.Attr("role", "status"),
		g.Attr("aria-live", "polite"),
		Class("hidden border-b border-line bg-s1/60 px-4 py-2 text-center text-xs"),
	)
}

// pushOnIndicator is the subtle "notifications on" pill shown once a push
// subscription is active. Hidden by default; pwaScript reveals it.
func pushOnIndicator() g.Node {
	return Span(
		ID("push-on"),
		g.Attr("title", "Notifications on"),
		g.Attr("aria-label", "Notifications enabled"),
		Class("hidden items-center gap-1 rounded-full bg-accent/10 px-2.5 py-1 text-xs font-medium text-accent ring-1 ring-inset ring-accent/20"),
		Span(g.Text("🔔")),
		Span(Class("hidden sm:inline"), g.Text("on")),
	)
}

// actionToasts is the body-level host for transient error toasts surfaced when a
// background (optimistic) action POST fails. It is fixed to the bottom of the
// viewport and pointer-events-none so it never blocks taps; individual toasts
// re-enable pointer events. appScript appends/removes children.
func actionToasts() g.Node {
	return Div(
		ID("action-toasts"),
		g.Attr("aria-live", "assertive"),
		Class("pointer-events-none fixed inset-x-0 bottom-4 z-50 mx-auto flex w-full max-w-sm flex-col items-center gap-2 px-4"),
	)
}

// enableNotificationsButton is the small header affordance that triggers the
// push subscription flow from a user gesture. It starts hidden; pwaScript shows
// it when appropriate and hides it again once subscribed.
func enableNotificationsButton() g.Node {
	return Button(
		ID("enable-push"),
		Type("button"),
		// hidden until the client decides push is available + ungranted.
		Class("press min-h-[44px] hidden items-center gap-1.5 rounded-full bg-accent/15 px-3 py-1 text-sm font-medium text-accent ring-1 ring-inset ring-accent/30 transition hover:bg-accent/25"),
		Span(g.Text("🔔")),
		Span(g.Text("Enable")),
	)
}

// cardTime renders a live relative timestamp ('10s', '30m', '2h'). The initial
// text is server-rendered; relTimeScript keeps it fresh client-side (the list
// only refetches on SSE events, so without the ticker the value would freeze).
func cardTime(t time.Time) g.Node {
	if t.IsZero() {
		return g.Text("")
	}
	return g.El("time",
		g.Attr("data-ts", strconv.FormatInt(t.UnixMilli(), 10)),
		g.Attr("title", t.Format(time.RFC3339)),
		Class("shrink-0 self-center text-xs tabular-nums text-muted"),
		g.Text(relTimeString(t)),
	)
}

// relTimeString is the compact relative-age string mirrored by relTimeScript.
func relTimeString(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		s := int(d.Seconds())
		if s < 0 {
			s = 0
		}
		return strconv.Itoa(s) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	}
}

// chip is a small muted label (e.g. host name).
func chip(label, value string) g.Node {
	return Span(
		Class("inline-flex items-center gap-1 rounded-full bg-s2/80 px-2.5 py-1 text-xs font-medium text-fg2 ring-1 ring-inset ring-line"),
		Span(Class("text-muted"), g.Text(label)),
		g.Text(value),
	)
}

// jsonString returns a JSON-encoded (quoted, escaped) string literal for s, for
// safe embedding in an hx-vals attribute value.
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// sidebar renders the slide-out left navigation: a dimmed backdrop and the
// panel itself with three tabs. Both start in the closed state (translated off
// to the left / backdrop transparent + pointer-events-none); sidebarScript
// toggles the `open` data attribute on #sidebar-root which CSS keys off. The
// tabs switch the visible main panel and close the sidebar.
//
// hasPanels says whether THIS document contains the #panel-* elements (i.e. it
// is the home shell). It decides who owns a tab click — see sidebarTab.
//
// 🔴 activeTab and currentTab are DIFFERENT claims and must not be conflated:
//
//   - activeTab drives the VISUAL accent highlight. It is a best-effort
//     "which section does this document belong to", and is allowed to be a
//     fallback — /operator paints Agents because that is the section it hangs
//     off. (appScript's show() then moves the highlight to Requests via its own
//     fallback once it reconciles; that is inherited behaviour this split
//     deliberately leaves alone — styling promises nothing, which is exactly
//     why the ARIA claim had to stop riding on it.)
//   - currentTab drives aria-current="page", which asserts *this link points at
//     the document you are reading*. It must be EMPTY ("") whenever no sidebar
//     link points at this document, because a false aria-current is a stronger
//     lie than no styling: a screen reader announces the wrong link as the
//     current page.
//
// The two coincide on the shell (each tab has a real URL) and on agent-detail
// (the Agents link is the section index, which is near-universal practice), and
// they DIVERGE on /operator: nothing in the sidebar routes to /operator, so it
// passes currentTab="" and renders no aria-current at all. Before this was
// explicit, /operator painted aria-current on Agents server-side and appScript
// then MOVED it onto "Claude Code requests" client-side (tabFromPath('/operator')
// falls back to 'requests') — two different links, neither of them this page.
// Pinned by TestSidebarMarksExactlyOneCurrentPage and
// TestOffShellPagesClaimNoCurrentPage.
func sidebar(activeTab, currentTab string, hasPanels bool, feat Features) g.Node {
	return sidebarIn(activeTab, currentTab, hasPanels, feat, false)
}

// sidebarIn is sidebar with its desktop behaviour chosen.
//
// overlay=false is the ordinary page: at lg+ the panel is a persistent rail
// beside a content column offset by lg:pl-72 (input.css). overlay=true keeps it
// a slide-out at EVERY width (data-overlay, which input.css's lg rule excludes)
// with a hamburger that is never hidden (overlayOpenButton). It is for the
// full-height views — the mirrored Rendered view and the terminal — whose
// layout is sized to the window: a rail there would take 18rem from a terminal
// whose width is the far side's geometry, and from a composer pinned to the
// visible viewport's edges. As an overlay nothing on those pages moves.
//
// activeTab / currentTab may also name a non-tab entry (NavAttention,
// NavAccount, NavAdmin): the tab links then match nothing and that entry is
// the one marked.
func sidebarIn(activeTab, currentTab string, hasPanels bool, feat Features, overlay bool) g.Node {
	return Div(
		ID("sidebar-root"),
		g.Attr("data-open", "false"),
		g.If(overlay, g.Attr("data-overlay", "")),
		// Backdrop: fades in when open, click closes.
		Div(
			ID("sidebar-backdrop"),
			g.Attr("aria-hidden", "true"),
			Class("sidebar-backdrop fixed inset-0 z-30 bg-black/60 opacity-0 transition-opacity duration-300"),
		),
		// Panel: slides in from the left.
		Nav(
			ID("sidebar"),
			g.Attr("role", "navigation"),
			g.Attr("aria-label", "Main menu"),
			Class("sidebar-panel fixed inset-y-0 left-0 z-40 flex w-72 max-w-[85vw] -translate-x-full flex-col bg-s1 shadow-2xl shadow-black/50 ring-1 ring-line transition-transform duration-300 ease-out"),
			// Sidebar header: brand + close.
			Div(
				Class("flex items-center gap-3 border-b border-line px-4 py-3"),
				Span(Class("text-lg font-semibold tracking-tight"), wordmark()),
				Span(Class("flex-1")),
				Button(
					ID("sidebar-close"),
					Type("button"),
					g.Attr("aria-label", "Close menu"),
					// ≥44x44 tap target (measured at 32x32 by an automated
					// accessibility audit). The icon stays h-5 w-5 — only the hit area grows,
					// so the compact header layout is unchanged.
					Class("press inline-flex h-11 w-11 min-h-[44px] min-w-[44px] items-center justify-center rounded-lg text-muted transition hover:bg-s2 hover:text-fg"),
					g.Raw(`<svg class="h-5 w-5" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M5 5l10 10M15 5L5 15"/></svg>`),
				),
			),
			// Section links: five plain <a href> links, deliberately carrying NO
			// ARIA role at all.
			//
			// An earlier revision closed axe's aria-required-parent (CRITICAL, 22
			// nodes across all 11 crawled URLs, per an automated audit) by adding
			// role="tablist" here, because each sidebarTab rendered role="tab".
			// That satisfied the rule with the WRONG SHAPE: the "tabs" are plain
			// links with no roving tabindex and no arrow-key handler anywhere in
			// appScript. Declaring a tablist puts a screen reader into a mode where
			// arrow keys are expected to move between tabs, and they do nothing —
			// markup promising a keyboard contract the code does not implement.
			// Off-shell (agent-detail / operator) they are ordinary cross-document
			// links, where role="tab" is simply wrong.
			//
			// So the requirement is REMOVED rather than satisfied: dropping
			// role="tablist"/role="tab"/aria-selected leaves aria-required-parent
			// nothing to fire on and no keyboard contract to honour, with
			// aria-current="page" marking the active link. aria-orientation went
			// with the tablist — it is meaningless without the widget.
			//
			// This container needs no role of its own: the links are ALREADY inside
			// the enclosing #sidebar navigation landmark ("Main menu"), so making
			// this a second <nav> would only add a redundant nested landmark a
			// screen-reader user has to step through. A plain <div> is the honest
			// markup, and it carries no aria-label — a non-landmark labels nothing.
			//
			// This is a CORRECTNESS fix, not a usability win: the sidebar is no
			// easier to navigate than it was, it just no longer lies.
			//
			// Pinned by TestSidebarTabsCarryNoTabRoles and
			// TestSidebarMarksExactlyOneCurrentPage.
			// 🔴 g.Map OVER THE TAB REGISTRY, NOT A HAND-WRITTEN RUN OF CALLS.
			// Upstream spelled each tab here by hand, which is the third of the four
			// places its tab set was duplicated. A tab present in the key list but
			// missing from this block is unreachable from the nav while still being a
			// valid URL, and nothing in the package could observe the disagreement.
			//
			// 🔴 IT IS visibleTabs, NOT musterTabs, AND THE SET MUST MATCH THE PANELS
			// Page DREW. A link whose panel this document did not render shows a
			// blank page when clicked; deriving both from one function is what makes
			// the two unable to disagree.
			Div(
				Class("flex flex-col gap-1 p-2"),
				g.Map(visibleTabs(feat), func(t tab) g.Node {
					return sidebarTab(t.Key, t.Label, t.Icon, activeTab == t.Key, currentTab == t.Key, hasPanels)
				}),
				// The attention queue (attention.go): a standalone page, not a tab.
				attentionLink(feat, activeTab == NavAttention, currentTab == NavAttention),
			),
			// Device preferences, pinned to the bottom of the panel and clear of
			// the home indicator. They are per-device (localStorage), so they live
			// with the navigation rather than in any server-side settings.
			Div(
				ID("sidebar-prefs"),
				Class("mt-auto flex flex-col gap-1 border-t border-line p-2 pb-[calc(0.5rem+env(safe-area-inset-bottom))]"),
				installButton(),
				themeToggle(),
				accountLinks(feat, activeTab, currentTab),
			),
		),
	)
}

// accountLinks is the sidebar's way to the account page (sign out, sign out
// everywhere) and, for an admin, to the admin pages (accounts PR 5). Both are
// standalone documents outside the shell, so the links opt out of boost: a
// full navigation, never a body swap of a document that has no panels.
//
// activeTab / currentTab are sidebar's: the Admin link is the one marked on an
// admin page (NavAdmin), Account on the account pages (NavAccount).
func accountLinks(feat Features, activeTab, currentTab string) g.Node {
	link := func(id, href, label, icon, entry string) g.Node {
		return A(ID(id), Href(href), hx("hx-boost", "false"),
			g.If(currentTab == entry, g.Attr("aria-current", "page")),
			Class("press flex min-h-[44px] w-full items-center gap-3 rounded-lg px-3 text-left text-sm font-medium transition "+navEntryTone(activeTab == entry)),
			Span(Class("text-base"), g.Text(icon)), Span(g.Text(label)))
	}
	return g.Group([]g.Node{
		g.If(feat.Admin, link("sidebar-admin", "/admin/users", "Admin", "🛡️", NavAdmin)),
		link("sidebar-account", "/account", "Account", "👤", NavAccount),
	})
}

// navEntryTone is a nav link's colour: the accent highlight for the entry the
// page belongs to, the quiet tone for every other. sidebarTab and the
// non-tab entries (attentionLink, accountLinks) all take it from here, so the
// one highlighted entry looks the same whichever kind it is — and appScript's
// show() toggles exactly these classes on the tab links.
func navEntryTone(active bool) string {
	if active {
		return "bg-accent/15 text-fg ring-1 ring-inset ring-accent/30"
	}
	return "text-fg2 hover:bg-s2 hover:text-fg"
}

// pathForTab maps a tab key to its SPA route, "/<tab>". ("/" also SERVES the
// default tab — see defaultVisibleTab — but no link points at it.)
// Mirrors appScript's JS pathForTab so the boosted sidebar <a> hrefs match the
// routes the in-shell router pushes (deep-links + back/forward stay consistent).
func pathForTab(key string) string { return "/" + normalizeTab(key) }

// sidebarTab is one navigation entry. It is a BOOSTED <a href=pathForTab(tab)>
// carrying data-tab so it works on BOTH the shell and the standalone
// detail/operator pages: on the shell (where the target panel exists), appScript
// intercepts the click and does the instant in-place panel toggle (pushState +
// show); on a page WITHOUT panels (agent-detail/operator), the click falls
// through and the boosted <a> navigates to the shell with that tab. The sidebar
// <a> is NOT inside #agents-list, so boosting is safe (top-level body swap).
// active marks the default-selected tab.
//
// 🔴 hasPanels (the home shell) renders hx-boost="false", and that is
// load-bearing, not an optimisation. appScript's handler calls
// e.preventDefault() to keep the click in-page, but htmx 2.0.4's boosted-anchor
// listener does NOT consult event.defaultPrevented — it is a listener on the
// SAME anchor, so preventDefault cannot reach it. On the shell every tab click
// therefore produced TWO history entries: ours (switchTab's pushState,
// synchronous) and htmx's (replaceState + pushState once the boosted GET
// returns, ASYNCHRONOUS). Browser Back then spent a press on the duplicate and
// appeared to do nothing. Measured directly by instrumenting history:
// load '/' → click Tasks → click Repos left the stack as
// ['/', '/tasks', '/tasks', '/repos', '/repos'] (history.length 2 → 6).
// Because htmx's second push lands off a network response, whether Back moved
// depended on timing — which is what made routing.spec's back/forward test flaky
// (5 failed / 5 passed at --repeat-each=10 --retries=0). Opting the shell's tabs
// OUT of boost gives the JS router sole ownership: exactly one entry per tab
// click, and no redundant full-page GET per click either. Off-shell
// (agent-detail / operator) the tabs stay boosted — there is no panel to toggle,
// so the boosted navigation IS the behaviour. Pinned by
// TestSidebarTabBoostIsOwnedByThePanelOwner.
//
// Accepted consequence: because the shell's tab navigations no longer go
// through htmx, their URLs are not in htmx's history cache, so a Back that
// lands on one after a boosted nav REFETCHES instead of restoring a snapshot.
// Verified correct (one extra request, arguably fresher content).
func sidebarTab(tab, label, icon string, active, current, hasPanels bool) g.Node {
	base := "sidebar-tab press flex items-center gap-3 rounded-lg px-3 py-2.5 text-left text-sm font-medium transition"
	tone := navEntryTone(active)
	return A(
		Href(pathForTab(tab)),
		g.Attr("data-tab", tab),
		// aria-current="page" marks the link that POINTS AT THIS DOCUMENT, and
		// is emitted ONLY when `current` — which is NOT the same flag as
		// `active` (the accent highlight). See sidebar()'s activeTab vs
		// currentTab comment: /operator highlights Agents but no sidebar link
		// routes to /operator, so it renders no aria-current at all rather than
		// naming an arbitrary link as the page you are on.
		//
		// aria-current="false" is a real, valid value meaning "not current", but
		// emitting it on four of five links is noise, and the JS half (show())
		// removes the attribute rather than setting it false, so the two halves
		// must agree. No role="tab"/aria-selected here: see the landmark comment
		// on the container in sidebar().
		g.If(current, g.Attr("aria-current", "page")),
		// Only the shell opts out; elsewhere the inherited body-level
		// hx-boost="true" still owns the click.
		g.If(hasPanels, hx("hx-boost", "false")),
		Class(base+" "+tone),
		Span(Class("text-base"), g.Text(icon)),
		Span(g.Text(label)),
	)
}

// appScript wires the slide-out sidebar (open/close + backdrop), SPA-style tab
// routing (each tab is a real path via history.pushState; back/forward + deep
// links work), the per-tab floating action buttons, and the header badge's
// quick-manage popover. No framework — minimal vanilla JS.
// navRegistryJS emits the JavaScript values the SPA router needs — the ordered
// tab list, the tab→heading map and the default tab — from musterTabs.
//
// 🔴 THIS IS THE FOURTH CONSUMER OF THE REGISTRY, AND UPSTREAM'S HAND-WRITTEN
// VERSION OF IT IS THE ONE THAT ROTS SILENTLY. A Go-side tab missing from the
// JS list is not a compile error and not a render error: the server paints the
// deep-linked tab correctly and only an in-page switch onto it misbehaves —
// `TABS.indexOf(tab)` returns -1 so switchTab refuses it, and
// `HEADINGS[tab]` is undefined so the <h1> keeps announcing the PREVIOUS tab's
// subject. Deriving both from musterTabs makes the disagreement impossible
// rather than detectable.
//
// ⚠ json.Marshal, NOT string concatenation. A label containing an apostrophe
// ("Today's runs") pasted into a single-quoted JS literal ends the string and
// produces a syntax error that kills the WHOLE script — sidebar, tab routing,
// toasts and FABs — with nothing on screen to say why.
// 🔴 IT EMITS THE VISIBLE TABS, NOT THE WHOLE REGISTRY, AND THE VALUES MUST
// AGREE WITH WHAT WAS DRAWN. A tab in TABS whose panel this document did not
// render is a tab switchTab can restore from localStorage, landing the operator
// on a document with every panel hidden. Deriving them from visibleTabs is what
// keeps the browser's model of the shell equal to the shell.
//
// ⚠ THERE WAS A PANEL_SELECTOR TOO. Its only consumer was the tab-switch swipe,
// which is gone (see the touch gesture block in appScript), so it went with it.
//
// 🔴 DEFAULT_TAB IS THE LAST, AND IT IS THE ONE THAT WAS MISSING. It is the
// tab `/` shows, and it comes from defaultVisibleTab — the same function the
// shell clamps with — so it is always a member of TABS. See that function for
// what a script-side default of its own cost.
func navRegistryJS(feat Features) string {
	visible := visibleTabs(feat)
	keys := make([]string, 0, len(visible))
	headingMap := make(map[string]string, len(visible))
	for _, t := range visible {
		keys = append(keys, t.Key)
		headingMap[t.Key] = t.Heading
	}
	keysJSON, _ := json.Marshal(keys)
	headings, _ := json.Marshal(headingMap)
	def, _ := json.Marshal(defaultVisibleTab(feat))
	return "  var TABS = " + string(keysJSON) + ";\n" +
		"  var HEADINGS = " + string(headings) + ";\n" +
		"  var DEFAULT_TAB = " + string(def) + ";\n"
}

// panelID is the DOM id of a tab's tabpanel. It is a FUNCTION rather than a
// convention spelled at each call site because both halves of the app depend on
// the two agreeing: the Go renderers stamp it, and the browser script toggles
// `hidden` by looking it up. A panel whose id does not match what switchTab
// asks for does not error — the tab just shows nothing while the previous
// panel stays on screen.
func panelID(key string) string { return "panel-" + key }

func appScript(feat Features) g.Node {
	return Script(g.Raw(`
(function () {
` + navRegistryJS(feat) + restoreJS(feat) + `

  // --- Navigation state persistence ----------------------------------------
  // Persist the active tab + its scroll offset so the PWA reopens where the
  // operator left off after a cold start. Pure client-side; zero server changes.
  //
  // localStorage is reached through these three wrappers because it THROWS
  // rather than returning null when storage is denied (Safari private mode,
  // a third-party-cookie block on an embedded document): an unguarded
  // getItem would take the whole appScript IIFE down with it, and with it
  // every tab click on the page.
  var NAV_TAB_KEY = 'muster.nav.tab';
  function navScrollKey(t) { return 'muster.nav.scroll.' + t; }
  function navStore() { try { return window.localStorage; } catch (e) { return null; } }
  function navGet(k) { var s = navStore(); if (!s) return null; try { return s.getItem(k); } catch (e) { return null; } }
  function navSet(k, v) { var s = navStore(); if (!s) return; try { s.setItem(k, v); } catch (e) {} }

  // 🔴 A STORED OFFSET EXPIRES, AND THAT IS NOT TIDINESS. Requests and Tasks are
  // SSE-updated lists: an offset is a PIXEL COUNT, so it names a position in
  // yesterday's content, not yesterday's content itself. Restoring a three-week-old
  // one lands the operator at an arbitrary point in a list that has since changed
  // length — or, if the list shrank, at the clamp. Twelve hours keeps the case the
  // feature exists for (reopen this evening, reopen tomorrow morning) and drops the
  // rest. Raised by the round-0 audit on #859 as a questioned requirement; the
  // operator chose the TTL over both keeping it unbounded and dropping scroll.
  var NAV_SCROLL_TTL_MS = 12 * 60 * 60 * 1000;
  // How long navApplyScroll may keep re-trying while the panels fill in.
  var NAV_SCROLL_SETTLE_MS = 2000;
  // 🔴 WHILE A RESTORE IS OUTSTANDING, THE SAVE MUST NOT RUN. This holds the
  // offset a restore is trying to reach and has NOT reached; 0 means none.
  //
  // Cold start with a stored 900: at t=100ms the panels have not filled, so the
  // scroll is clamped far below it. Background the app there and an unguarded
  // navSaveScroll writes that clamp; navReadScroll rejects 0 outright, and a
  // partial clamp is worse still — it persists 120 instead of 900, permanently,
  // and looks like a working restore.
  //
  // 🔴 IT IS KEYED ON THE TARGET, NOT ON A DEADLINE, AND THAT DISTINCTION IS A
  // FIX. A time-based version narrowed the window to 2s instead of closing it:
  // a restore ABANDONED short of its target leaves the viewport on the clamp,
  // and the NEXT teardown — after the deadline has passed — persists that clamp.
  // The corrupting state is "the restore never got there", which outlives any
  // deadline.
  //
  // 🔴 navApplyScroll HAS FOUR OUTCOMES AND ONLY TWO OF THEM RELEASE THE TARGET.
  // Two earlier versions of this heading miscounted — "exactly two things", which
  // omitted the most ORDINARY outcome, and then "THREE EXITS AND ONLY ONE
  // RELEASES", which contradicted its own list three lines later. The list is the
  // authority; count it:
  //   LANDED            -> release. The screen now shows the stored position.
  //   OPERATOR INPUT    -> release. The position on screen is theirs.
  //   BUDGET EXPIRED    -> KEEP. The restore never got there; the clamp on
  //                        screen is nobody's choice, so it must not be saved.
  //   (going hidden also KEEPS, for the same reason as budget expiry.)
  // Only the first two are releases. A maintainer "fixing the asymmetry" by
  // clearing the target on the budget branch would reopen the corruption above
  // for its commonest trigger — a board that shrank overnight, so the stored
  // offset is simply unreachable this session.
  //
  // The consequence is deliberate: after a restore that did not land, saves are
  // suppressed until the operator actually scrolls. That is correct — they have
  // not chosen a position, so there is nothing to record, the stored one is
  // still the best answer, and it ages out of the 12h TTL on its own if the
  // content really has shrunk for good.
  var navRestorePending = 0;

  // navIsRootPath: is this the bare root, the one path a PWA cold start lands on?
  // Only there is the stored tab consulted; any URL naming a tab is an explicit
  // instruction and wins.
  //
  // 🔴 IT IS SPELLED OUT HERE BECAUSE tabFromPath CANNOT ANSWER IT. tabFromPath('/')
  // returns DEFAULT_TAB, by its own documented fallback — so a restore gated on
  // !tabFromPath(location.pathname) never fires on '/'. (Measured: the first cut of
  // this feature was gated exactly that way and the tab half of it was entirely
  // dead; the one path that DID satisfy the gate was /operator, which carries no
  // #panel-* and so was rejected by the next guard anyway.)
  //
  // ⚠ An earlier version of this asked "does the URL name a tab?" and answered it
  // with "the first path segment is non-empty" — which is true of /operator and of
  // /agents/<id>, neither of which names a tab. It reached the right OUTCOME by a
  // claim that was false, flagged by the round-0 audit on #859. The predicate is
  // the root test; say so.
  function navIsRootPath() { return (location.pathname || '/') === '/'; }

  // 🔴 THESE FOUR LIVE AT IIFE TOP LEVEL, NOT INSIDE initGlobal/initPage, AND
  // THAT IS A BUG FIX RATHER THAN A STYLE CHOICE. navSaveScroll and
  // navReadScroll were first declared inside initGlobal() while restoreNav sat
  // inside initPage() — two different function scopes — so restoreNav threw
  // "navReadScroll is not defined" on every cold start. It threw AFTER the tab
  // had been switched, so the tab restore looked like it worked and only the
  // scroll silently vanished; nothing surfaced but an uncaught error in the
  // console.
  //
  // The stored form is 'offset:savedAtMillis'. navReadScroll is the only reader;
  // it returns 0 — a no-op for every caller — for a missing value, a malformed
  // one, and one older than NAV_SCROLL_TTL_MS. A value with NO timestamp reads
  // as EXPIRED rather than as trusted: that is the fail-closed direction, and it
  // is also what an offset written by a build before the TTL existed should get.
  //
  // 🔴 BOTH GUARD ON A REAL TAB *AND* A PRESENT PANEL, and the pair must stay
  // symmetric. An earlier cut guarded the SAVE both ways and the RESTORE only on
  // the tab, while a comment claimed they mirrored each other. tabFromPath
  // ('/tasks/42') is 'tasks', so a task-detail document — which renders this
  // script and has no #panel-* — had the Tasks BOARD's offset applied to it.
  // Measured at /tasks/1: scrollY 900 on a page that never scrolled.
  function navSaveScroll() {
    // A restore is outstanding (see navRestorePending): what is on screen is not
    // the operator's position, so keep the stored value rather than overwrite it
    // with a half-applied one.
    if (navRestorePending > 0) return;
    var t = tabFromPath(location.pathname);
    if (!t || !document.getElementById('panel-' + t)) return;
    navSet(navScrollKey(t), String(Math.round(window.scrollY)) + ':' + String(Date.now()));
  }
  function navReadScroll(t) {
    if (!t || !document.getElementById('panel-' + t)) return 0;
    var raw = navGet(navScrollKey(t));
    if (!raw) return 0;
    var parts = String(raw).split(':');
    if (parts.length !== 2) return 0;
    var n = parseInt(parts[0], 10);
    var at = parseInt(parts[1], 10);
    // NaN fails every one of these, so a corrupt value is a no-op, not a throw.
    if (!(n > 0) || !(at > 0)) return 0;
    var age = Date.now() - at;
    // A clock that moved backwards (NTP step, timezone-less device) yields a
    // negative age. Treat it as expired rather than as freshly saved.
    if (age < 0 || age > NAV_SCROLL_TTL_MS) return 0;
    return n;
  }

  // navApplyScroll: land on the restored offset even though the document is
  // still filling in.
  //
  // 🔴 ONE requestAnimationFrame IS NOT ENOUGH ON THE REAL SHELL. The panels
  // lazy-load over htmx, so at the first frame the document is often SHORTER
  // than the stored offset and the browser clamps the scroll — the operator
  // lands short of where they left. (The e2e fixture cannot see this: it forces
  // a 4000px document on every load, so there is nothing to wait for.) So retry
  // across frames until the offset actually takes.
  //
  // 🔴 BOUNDED AND CANCELLABLE, because an unbounded version of exactly this is
  // the defect that made the app unusable: a restore re-applied on every swap
  // pins the viewport and the operator cannot scroll at all. This one stops the
  // moment it lands, gives up after NAV_SCROLL_SETTLE_MS, and ABORTS on the
  // first sign of real input — it must never fight the operator for the
  // scrollbar.
  //
  // 🔴 THE GUARD THAT ACTUALLY CLOSES THE WALL-CLOCK HOLE IS THE visibilitychange
  // CANCEL, NOT THE DEADLINE. requestAnimationFrame does not run in a hidden
  // document, so a frame queued just before the operator locks the phone fires
  // whenever they come back: MEASURED at a scrollTo landing 4.00 HOURS after
  // navApplyScroll started, moving the page with no input — this same defect in
  // miniature, reintroduced by its own fix. Going hidden therefore ENDS the
  // restore rather than deferring it. Pinned by 'a restore abandoned when the
  // page is backgrounded never fires later'.
  //
  // 🔴 TESTING THE DEADLINE BEFORE THE SCROLL IS THE ONLY WALL-CLOCK TERMINATOR
  // ON iOS, AND AN EARLIER COMMENT HERE CALLED IT UNREACHABLE. That was FALSE,
  // and falsified by this file's own text 300 lines down: iOS Safari — which is
  // what the PWA actually runs in — does not reliably fire visibilitychange when
  // the app is backgrounded or killed; pagehide is the event that survives the
  // bfcache path. So on the target platform the cancel does NOT win first:
  // 'done' stays false, the parked frame resumes hours later, and only
  // 'Date.now() > deadline' stops it from scrolling.
  //
  // 🔴 SO DO NOT DELETE THE DEADLINE TERM. The earlier wording invited exactly
  // that, which would leave the loop with no unconditional terminator at all on
  // a short document with no input and no visibility change — the pinning defect
  // this whole feature exists to avoid.
  //
  // It is pinned by 'a restore parked across a background period does not fire
  // when frames resume', which suspends requestAnimationFrame to build the state
  // a real backgrounded tab produces.
  // ⚠ NOT RE-ENTRANT, AND THE PROTECTION IS IN THE CALLER RATHER THAN HERE.
  // navRestorePending is module-level: a second concurrent navApplyScroll would
  // overwrite it, and the first call's exit would then release a target the
  // second is still chasing. Unreachable today — restoreNav is the only caller
  // and runs once per document behind window.__cgNavRestored — so this is
  // latent, not a bug. Said here because nothing at the call site says it.
  function navApplyScroll(n) {
    var deadline = Date.now() + NAV_SCROLL_SETTLE_MS;
    navRestorePending = n;
    var done = false;
    // The operator took over: the position on screen is THEIRS from now on, so
    // the pending target is released and saves resume.
    function stopForOperator() { done = true; navRestorePending = 0; }
    // The page went away mid-restore: stop, but do NOT release the target — the
    // restore never reached it, so the stored offset is still the better answer
    // and must not be overwritten by the clamp left on screen.
    function stopForHidden() { done = true; }
    ['wheel', 'touchstart', 'keydown', 'pointerdown'].forEach(function (ev) {
      window.addEventListener(ev, stopForOperator, { once: true, passive: true });
    });
    // Any visibility transition ends it. Cancelling on becoming VISIBLE too is
    // deliberate and conservative: a restore parked across a background period is
    // exactly the one that must not fire.
    document.addEventListener('visibilitychange', stopForHidden, { once: true });
    (function attempt() {
      // Out of budget, or already stopped: return WITHOUT scrolling.
      if (done || Date.now() > deadline) return;
      window.scrollTo(0, n);
      // Landed, within a pixel of rounding: the target is reached, release it.
      if (Math.abs(window.scrollY - n) <= 1) { done = true; navRestorePending = 0; return; }
      requestAnimationFrame(attempt);
    })();
  }

  // restoreNav: a ONCE-PER-DOCUMENT action. Restore the tab the operator left
  // (only at a bare '/'), then that tab's scroll offset.
  //
  // 🔴 IT IS CALLED FROM init(), ONCE, AND NOT FROM initPage(). initPage re-runs
  // on EVERY htmx:load — which is every SSE swap, every OOB badge swap and every
  // 10/15/60-second poll — and this function is not idempotent against that.
  // MEASURED on an idle no-DB shell: 6 htmx:load events, ALL of them within 9ms
  // of first paint, from one response's out-of-band badge subtrees. So the
  // re-entry is a burst on every page load rather than a slow drip; what a real
  // shell with agent cards adds on top is asserted, not measured.
  //
  // ⚠ THE SEVERITY WORDING BELOW OUTRUNS THAT NUMBER. "every few seconds" and
  // "dragged back and HELD" were written against an earlier reading of the
  // figure; six events inside 9ms is a one-shot re-application per load. The
  // once-guard is right either way — the defect was reproduced against the base
  // tree — but do not cite the count as evidence for the adjective. Living
  // there it:
  //   (1) re-applied the stored offset every few seconds, so an operator who
  //       scrolled away was dragged back within a second and HELD there with no
  //       input of their own. On a permission router that puts Approve/Deny out
  //       of reach: an outage, not a cosmetic jump.
  //   (2) re-asserted the stored tab at '/', undoing a Back press, hijacking the
  //       Requests sidebar link from a detail page, and forcing the open sidebar
  //       shut via switchTab's setOpen(false).
  // Both were measured against the base tree. The once-guard is the fix.
  function restoreNav() {
    var t = tabFromPath(location.pathname);
    if (navIsRootPath()) {
      var saved = navGet(NAV_TAB_KEY);
      if (saved && TABS.indexOf(saved) >= 0 && document.getElementById('panel-' + saved)) {
        // replace=true: restoring is not a navigation the operator performed.
        // A pushed entry would sit between '/' and wherever they came from, so
        // the first Back press would spend itself returning to a '/' that
        // immediately restores again — a Back button that looks broken.
        switchTab(saved, true);
        t = saved;
      }
    }
    var n = navReadScroll(t);
    if (n > 0) navApplyScroll(n);
  }

  // cgTrack: telemetry helper. faroInitScript (rendered only when Faro is
  // enabled) overrides this with a real implementation; this no-op default means
  // every call site can fire cgTrack(...) unconditionally with zero telemetry
  // surface when Faro is off (local/e2e).
  if (typeof window.cgTrack !== 'function') { window.cgTrack = function () {}; }

  // --- Authelia session expiry -------------------------------------------
  //
  // A deployment behind an SSO forward-auth proxy (e.g. Authelia) with session.inactivity
  // at 5m, so this path is exercised constantly, not rarely.
  //
  // 🔴 htmx CANNOT FIX THIS FROM THE RESPONSE SIDE. htmx's docs are explicit
  // that response headers "are not provided to htmx for processing with 3xx
  // Redirect response codes" — the browser follows the 302 inside the XHR
  // before htmx ever sees it. Every request this app makes is an htmx request
  // (the whole shell is hx-boost + panel swaps), so an expired session swaps
  // the Authelia LOGIN PAGE into hx-target and the app appears to have turned
  // into the portal.
  //
  // Measured against the live edge (Authelia 4.39.20, 2026-09-02):
  //     GET /ui/requests                              -> 302
  //     GET /ui/requests  X-Requested-With: XHR       -> 401
  // So Authelia WILL answer with a clean 401, but only on a header htmx does
  // not send by default. Hence (a).
  //
  // ⚠ THAT IS MEASURED, NOT DOCUMENTED — an Authelia upgrade could change it.
  // (b) is therefore kept REGARDLESS: it catches a 401 arriving by any route,
  // including one that has nothing to do with X-Requested-With. Do not delete
  // (b) on the grounds that (a) makes it redundant — it does not.
  //
  // Bound on <body>, which SURVIVES an hx-boost swap (only its innerHTML is
  // replaced) and is why this IIFE can re-run; the guard keeps it to one bind.
  if (!window.__cgAuthGate) {
    window.__cgAuthGate = true;
    // (a) Ask Authelia for a 401 instead of a 302 on every htmx request.
    document.body.addEventListener('htmx:configRequest', function (e) {
      e.detail.headers['X-Requested-With'] = 'XMLHttpRequest';
    });
    // (b) Turn a 401 into a REAL navigation, which — unlike an XHR — hands the
    //     302 to the browser's own navigation machinery and lands the user on
    //     the portal. shouldSwap=false stops the login page landing in the
    //     target; isError=false stops htmx logging it (and resyncScript's
    //     htmx:responseError listener toasting it) as an application failure,
    //     because an expired session is an expected lifecycle event.
    document.body.addEventListener('htmx:beforeSwap', function (e) {
      if (!e.detail || !e.detail.xhr || e.detail.xhr.status !== 401) return;
      e.detail.shouldSwap = false;
      e.detail.isError = false;
      // 🔴 IN THE VIEW SWAP'S FRAME, THE TOP WINDOW RELOADS. The frame is the
      // other view laid over the page; reloading only the frame would put the
      // sign-in (or a refusal) inside it, and the page under it — whose
      // session expired too — would still be there. The top window's address
      // is the view on screen (view_swap.go keeps it so), so its reload is
      // that view, signed out: the sign-in, at the top, with ?next= back.
      try {
        if (window.top !== window && window.top.location.origin === window.location.origin) {
          window.top.location.reload();
          return;
        }
      } catch (x) {}
      window.location.reload();
    });
  }

  // cgCopy copies a button's data-copy-text to the clipboard, with a brief
  // "Copied" affordance. Boost-safe (looked up at call time). Falls back to a
  // hidden-textarea execCommand when the async Clipboard API is unavailable
  // (insecure context / older browser).
  window.cgCopy = function (btn) {
    if (!btn) return;
    var text = btn.getAttribute('data-copy-text') || '';
    var done = function () {
      var prev = btn.textContent;
      btn.textContent = 'Copied';
      setTimeout(function () { btn.textContent = prev; }, 1200);
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(done).catch(function () { fallback(); });
    } else { fallback(); }
    function fallback() {
      try {
        var ta = document.createElement('textarea');
        ta.value = text; ta.style.position = 'fixed'; ta.style.opacity = '0';
        document.body.appendChild(ta); ta.select();
        document.execCommand('copy'); document.body.removeChild(ta); done();
      } catch (e) {}
    }
  };

  // setOpen looks elements up live (not via captured refs) so it stays correct
  // after an hx-boost body swap re-creates the shell.
  function setOpen(open) {
    var root = document.getElementById('sidebar-root');
    if (!root) return;
    root.setAttribute('data-open', open ? 'true' : 'false');
    var openBtn = document.getElementById('sidebar-open');
    if (openBtn) openBtn.setAttribute('aria-expanded', open ? 'true' : 'false');
    // Only lock scroll for the mobile overlay (desktop sidebar is persistent).
    document.body.style.overflow = (open && window.innerWidth < 1024) ? 'hidden' : '';
  }

  // initPage (re)wires the per-page elements (sidebar buttons + tabs). It is
  // idempotent: each element is marked data-cg-bound once so firing on both
  // DOMContentLoaded and htmx:load (boost swaps) never double-binds.
  function initPage() {
    var openBtn = document.getElementById('sidebar-open');
    var closeBtn = document.getElementById('sidebar-close');
    var backdrop = document.getElementById('sidebar-backdrop');
    if (openBtn && !openBtn.hasAttribute('data-cg-bound')) { openBtn.setAttribute('data-cg-bound','1'); openBtn.addEventListener('click', function () { setOpen(true); }); }
    if (closeBtn && !closeBtn.hasAttribute('data-cg-bound')) { closeBtn.setAttribute('data-cg-bound','1'); closeBtn.addEventListener('click', function () { setOpen(false); }); }
    if (backdrop && !backdrop.hasAttribute('data-cg-bound')) { backdrop.setAttribute('data-cg-bound','1'); backdrop.addEventListener('click', function () { setOpen(false); }); }

    // Tab clicks. The tabs are boosted <a href=pathForTab(tab)> now (so they work
    // on pages without panels). If THIS document has the target panel (the home
    // shell), intercept: preventDefault + pushState + instant in-place show()
    // (byte-for-byte the old behavior). If it does NOT (agent-detail / operator
    // pages have no #panel-*), let the boosted <a> navigate to the shell with that
    // tab. Bound once per anchor.
    document.querySelectorAll('.sidebar-tab').forEach(function (tab) {
      if (tab.hasAttribute('data-cg-bound')) return;
      tab.setAttribute('data-cg-bound','1');
      tab.addEventListener('click', function (e) {
        var sel = tab.getAttribute('data-tab');
        window.cgTrack('tab.view', { tab: sel });
        if (!document.getElementById('panel-' + sel)) return; // no panel here → boosted <a> navigates
        e.preventDefault();
        switchTab(sel);
      });
    });
    // Segmented [Requests | Tasks] control: same shared router as the sidebar. Bound once per button (data-cg-bound). These always target a present
    // panel (they only render on the home shell), so they always intercept.
    document.querySelectorAll('[data-seg-tab]').forEach(function (seg) {
      if (seg.hasAttribute('data-cg-bound')) return;
      seg.setAttribute('data-cg-bound','1');
      seg.addEventListener('click', function () {
        var sel = seg.getAttribute('data-seg-tab');
        window.cgTrack('tab.view', { tab: sel });
        switchTab(sel);
      });
    });
    // Server set the initial active tab; reconcile JS state with the URL.
    showForPath();
  }

  // tabFromPath maps a pathname to its tab, or to '' when this document is NOT
  // one of the tab pages (/operator, or any future non-tab route). The empty
  // string is load-bearing: it is what lets showForPath tell "highlight the
  // default tab" apart from "the default tab's link IS this page". Callers that
  // only need the highlight substitute their own fallback.
  //
  // 🔴 '/' IS DEFAULT_TAB, WHICH THE SERVER EMITS (navRegistryJS) FROM THE TABS IT
  // DREW. This line used to say 'requests' — the default of the product this was
  // extracted from, and not a muster tab — so '/' mapped to '' and showForPath
  // hid every panel. Do not spell a tab key here.
  function tabFromPath(p) {
    var seg = (p || '/').split('/')[1] || DEFAULT_TAB;
    return TABS.indexOf(seg) >= 0 ? seg : '';
  }

  // showForPath reconciles the visible state with location.pathname. On a path
  // that is not a tab page it still paints a tab (the DEFAULT_TAB fallback, so
  // the chrome looks the same as it always has) but passes isPage=false, so
  // show() marks NO link aria-current="page" — matching what Go renders for
  // that document (sidebar(activeTab, "", ...)). Before this split, /operator
  // was painted aria-current on Agents by Go and then had it MOVED onto
  // "Claude Code requests" by this reconcile, naming a link that is neither the
  // page nor the highlight.
  //
  // 🔴 A DOCUMENT WITH NO PANELS ON A PATH NO TAB NAMES IS LEFT AS THE SERVER
  // PAINTED IT. /attention, /account, /plugins, the mirrored views: each marks
  // its own entry (or none) on first paint, and there is no panel here to
  // reconcile — so the DEFAULT_TAB fallback above would only move the accent
  // onto Tasks, beside the entry the page really belongs to.
  function showForPath() {
    var t = tabFromPath(location.pathname);
    if (!t && !anyPanel()) return;
    show(t || DEFAULT_TAB, t !== '');
  }
  function anyPanel() {
    for (var i = 0; i < TABS.length; i++) {
      if (document.getElementById('panel-' + TABS[i])) return true;
    }
    return false;
  }
  // Every tab's path is /<key>, as Go's pathForTab. '/' also SHOWS the default
  // tab, but no tab's link points at it, so nothing here returns it.
  function pathForTab(t) { return '/' + t; }

  // switchTab is the shared tab router used by the sidebar tabs and the
  // segmented [Requests | Tasks] control — so both drive the exact same
  // pushState + panel show/hide state (no forked routing). It is a no-op on a
  // page without the target panel (agent-detail/operator).
  // The 'replace' flag is set ONLY by restoreNav, which is re-painting state the operator
  // already had rather than navigating: it must not manufacture a history entry.
  // Every human-driven caller (sidebar, segmented control) leaves it unset
  // and keeps the pushState that Back depends on.
  function switchTab(sel, replace) {
    if (TABS.indexOf(sel) < 0) return;
    if (!document.getElementById('panel-' + sel)) return;
    if (pathForTab(sel) !== location.pathname) {
      if (replace) history.replaceState({tab: sel}, '', pathForTab(sel));
      else history.pushState({tab: sel}, '', pathForTab(sel));
    }
    // isPage=true: the pushState above (or the early-return equality check) means
    // location.pathname IS pathForTab(sel), so this tab's link is the current page.
    show(sel, true);
    // A panel that keeps state in the URL (the Agents filter's ?q=,
    // agents_filter.go) re-writes it: the push above is a bare /<tab>.
    document.dispatchEvent(new CustomEvent('muster:tabshown', { detail: { tab: sel } }));
    setOpen(false);
    navSet(NAV_TAB_KEY, sel);
  }

  // TABS and HEADINGS are emitted at the top of this script by navRegistryJS,
  // derived from Go's musterTabs. They used to be hand-written here and drift
  // out of step with the Go side silently — see navRegistryJS's header.
  // The shell renders ONE <h1 id="page-heading"> (axe page-has-heading-one)
  // whose text is the active tab's subject; the SPA router swaps tabs without a
  // server render, so it must rewrite the heading or a deep-linked-then-switched
  // page announces the wrong subject.

  // show reflects the given tab in the visible state. isPage is a SEPARATE claim: true
  // only when a sidebar link actually points at the document being displayed.
  // The two are decoupled deliberately — the accent highlight is decoration,
  // aria-current="page" is an assertion about the URL. See showForPath.
  //
  // 🔴 IT CANNOT LEAVE EVERY PANEL HIDDEN, WHATEVER IT IS ASKED FOR. The toggle
  // below hides each panel that is not the requested tab, so a tab with no panel in a
  // document that HAS panels blanks the page — which is what an unknown tab
  // clamping to a key outside TABS did on '/'. shownTab is the guard: the
  // server's DEFAULT_TAB first, and if even that has no panel here, the first
  // tab that does. On a document with no panels at all (agent detail) there is
  // nothing to blank and the tab is only a sidebar highlight, so it is kept.
  function shownTab(tab) {
    if (TABS.indexOf(tab) < 0) tab = DEFAULT_TAB;
    if (document.getElementById('panel-' + tab)) return tab;
    for (var i = 0; i < TABS.length; i++) {
      if (document.getElementById('panel-' + TABS[i])) return TABS[i];
    }
    return tab;
  }
  function show(tab, isPage) {
    tab = shownTab(tab);
    var heading = document.getElementById('page-heading');
    if (heading && HEADINGS[tab]) heading.textContent = HEADINGS[tab];
    TABS.forEach(function (t) {
      var p = document.getElementById('panel-' + t);
      if (p) p.classList.toggle('hidden', t !== tab);
    });
    // Per-tab floating action button.
    document.querySelectorAll('[data-fab]').forEach(function (f) {
      f.classList.toggle('hidden', f.getAttribute('data-fab') !== tab);
    });
    // Segmented [Requests | Tasks] control: reflect the active tab (aria-selected
    // drives the pill styling). All seg buttons across both panels stay in sync.
    document.querySelectorAll('[data-seg-tab]').forEach(function (s) {
      s.setAttribute('aria-selected', s.getAttribute('data-seg-tab') === tab ? 'true' : 'false');
    });
    // Sidebar active styling + aria. The sidebar is a NAV landmark of links,
    // not a tablist, so the current link is marked with aria-current="page" and
    // the others have the attribute REMOVED (never set to "false") — the same
    // rule Go's sidebarTab renders on first paint. Setting it on every link, or
    // leaving a stale one behind on an SPA tab switch, would announce two
    // current pages.
    //
    // The "&& isPage" conjunct is what keeps this half honest on a document no
    // sidebar link points at (/operator): the highlight below still moves, but
    // EVERY link has aria-current removed rather than one being picked
    // arbitrarily. Drop it and /operator claims "Claude Code requests" is the
    // page you are on.
    document.querySelectorAll('.sidebar-tab').forEach(function (b) {
      var on = b.getAttribute('data-tab') === tab;
      if (on && isPage) { b.setAttribute('aria-current', 'page'); } else { b.removeAttribute('aria-current'); }
      b.classList.toggle('bg-accent/15', on);
      b.classList.toggle('text-fg', on);
      b.classList.toggle('ring-1', on);
      b.classList.toggle('ring-inset', on);
      b.classList.toggle('ring-accent/30', on);
      b.classList.toggle('text-fg2', !on);
      b.classList.toggle('hover:bg-s2', !on);
      b.classList.toggle('hover:text-fg', !on);
    });
  }

  // initGlobal binds the document/window-level delegated listeners. These all
  // look elements up lazily (so they survive boost swaps), so they MUST bind
  // exactly once for the life of the document — guarded by window.__cgInit.
  function initGlobal() {
  // popstate handles both SPA tab back/forward AND hx-boost history nav: when
  // the boosted page is the home shell, reconcile the tab to the URL. (htmx
  // restores boosted bodies from its history cache and re-fires htmx:load, so
  // initPage runs then too.)
  window.addEventListener('popstate', function () { showForPath(); });

  // Persist the active tab's scroll offset when the page is backgrounded or torn
  // down (tab close, PWA sent to the background, OS app switch).
  //
  // navSaveScroll itself is at IIFE top level (with navReadScroll, its mirror) —
  // only the BINDING belongs here, because initGlobal is the once-per-document
  // block. Why the save reads window.scrollY and NOT panel.scrollTop is recorded
  // in e2e/tests/nav-state.spec.ts, on 'the stored offset is the one the restore
  // can act on' — the save and the restore must name the same scroller, and the
  // document is the scroll container for every panel on this shell.
  document.addEventListener('visibilitychange', function () {
    if (document.visibilityState === 'hidden') navSaveScroll();
  });
  // iOS Safari — which is what the PWA runs in on the phone — does not reliably
  // fire visibilitychange when the app is killed or a tab is closed. pagehide is
  // the event that survives the bfcache path, so both are wired.
  window.addEventListener('pagehide', navSaveScroll);

  // --- Persistent notification FAB + panel (works from any tab/page shell) ---
  // Look elements up lazily inside the handlers: #notif-panel / #notif-fab are
  // body-level siblings rendered AFTER this script's wrapper, and the panel's
  // content is swapped by htmx, so querying on demand stays robust. Open toggles
  // the slide/scale+fade classes (mirrors the sidebar setOpen pattern) and a
  // full-screen backdrop; rows are boosted SPA links so tapping one navigates away.
  function notifEls() {
    return {
      panel: document.getElementById('notif-panel'),
      backdrop: document.getElementById('notif-backdrop'),
    };
  }
  function notifOpen(open) {
    var e = notifEls();
    if (!e.panel) return;
    if (open) {
      e.panel.classList.remove('hidden');
      if (e.backdrop) e.backdrop.classList.remove('hidden');
      // Force reflow so the transition runs from the hidden start state.
      void e.panel.offsetHeight;
      e.panel.classList.remove('translate-y-2', 'scale-95', 'opacity-0');
    } else {
      e.panel.classList.add('translate-y-2', 'scale-95', 'opacity-0');
      if (e.backdrop) e.backdrop.classList.add('hidden');
      // Defer the display:none until the fade-out finishes.
      setTimeout(function () {
        var p = document.getElementById('notif-panel');
        if (p && p.classList.contains('opacity-0')) p.classList.add('hidden');
      }, 200);
    }
  }
  function notifIsOpen() {
    var p = document.getElementById('notif-panel');
    return p && !p.classList.contains('hidden');
  }
  document.addEventListener('click', function (e) {
    var fab = e.target.closest && e.target.closest('#notif-fab');
    if (fab) { e.preventDefault(); notifOpen(!notifIsOpen()); return; }
    // Backdrop / outside-tap closes (rows live inside #notif-panel).
    if (notifIsOpen() && !(e.target.closest && e.target.closest('#notif-panel'))) notifOpen(false);
  });

  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape') return;
    // 🔴 ESCAPE PRECEDENCE — innermost overlay first:
    //     open combobox dropdown  >  dialog / sidebar / popover / notif panel
    //
    // Precisely: an open combobox dropdown consumes the keypress ENTIRELY (early
    // return). Otherwise the keypress is broadcast to every other overlay —
    // setOpen/notifOpen plus BOTH cgModalDismiss calls all run. That is
    // deliberate and safe, not "exactly one dismissal per keypress" (an earlier
    // version of this comment claimed that, and the code below contradicts it):
    // each of those is a no-op for an overlay that is already closed, and at most
    // one of them can be open at a time in practice. What matters is only the
    // combobox precedence — cbOpen(box,false) always sets .hidden, so an Escape
    // can never be swallowed by a dropdown that then fails to close.
    // A combobox dropdown is a CHILD of a dialog (the edit sheet has 2, the
    // dispatch sheet 3). "Dismiss this dropdown" must never mean "destroy this
    // dialog" — measured: ArrowDown to open the repo list, Escape, and the whole
    // sheet vanished (taking the typed dispatch prompt with it).
    //
    // The check lives HERE, not as a stopPropagation() in the combobox's own
    // keydown at the bottom of this IIFE, because BOTH listeners are on
    // document in the BUBBLE phase and this one is registered FIRST — listener
    // order on a single node is registration order, and stopPropagation() cannot
    // reach a listener that has already run. Closing the dropdown here (rather
    // than merely returning) also means an Escape while focus sits outside the
    // input still dismisses it, instead of being silently swallowed.
    var openLists = document.querySelectorAll('[data-combobox] [data-combobox-list]:not(.hidden)');
    if (openLists.length) {
      // Reuse cbOpen (declared later in this same IIFE, hoisted) so the
      // open/closed rule — the hidden class AND aria-expanded AND the active
      // option — stays in ONE place.
      for (var li = 0; li < openLists.length; li++) {
        var cbBox = openLists[li].closest('[data-combobox]');
        if (cbBox) cbOpen(cbBox, false);
      }
      return;
    }
    // 🔴 popOpen() AND aaPopOpen() USED TO BE CALLED HERE AND NEITHER FUNCTION
    // EXISTS. They were the permission-router's popover and auto-approve popover,
    // deleted with that surface; the CALLS came across with the carve. A call to
    // an undefined identifier is a ReferenceError, and it landed BEFORE the
    // cgModalDismiss line below — so Escape threw on every keypress and neither
    // sheet ever closed. Measured in jsdom on the rendered /tasks document:
    // "ReferenceError: popOpen is not defined", with the modal still displayed
    // afterwards. From the outside it is indistinguishable from having no
    // handler at all, which is exactly how it was reported.
    setOpen(false); notifOpen(false);
    // Both dialogs were omitted from this handler, so Escape did nothing for
    // them. Both now route through the SAME dirty-guarded dismiss: an
    // in-progress edit — or a typed dispatch prompt, the longest free text in
    // the app — must not vanish on a stray keypress. cgModalDismiss is a no-op
    // for a modal that is already hidden.
    if (window.cgModalDismiss) { window.cgModalDismiss('task-modal'); window.cgModalDismiss('agent-modal'); }
  });

  // --- Error toasts ---
  // 🔴 WHAT USED TO SURROUND THIS WAS AN OPTIMISTIC PERMISSION-DECISION QUEUE,
  // AND IT IS GONE RATHER THAN DISABLED. Upstream's approve/deny/dismiss buttons
  // removed their card on click and fired the POST in the background, with a
  // counter beside the header badge tracking the in-flight writes. Every one of
  // those surfaces belongs to the permission router: muster renders nothing
  // carrying data-action-url, data-action-card or data-aa-project, so the
  // listener could never fire and the counter could never leave zero.
  //
  // What survives is window.toast, which is NOT part of that machinery even
  // though it lived inside it. resyncScript's htmx:responseError and
  // htmx:sendError listeners call it by name from another IIFE, and the task
  // modal calls it too — so deleting it with its former neighbours would make
  // every failed mutation in muster fail SILENTLY, which is the exact defect
  // resyncScript exists to prevent.
  // Canonical, window-global error-toast helper. Defined on window so callers in
  // OTHER IIFEs (resyncScript's htmx:responseError / htmx:sendError listeners)
  // and any hx-on handler can reach it — mirrors the window.cgTrack pattern.
  // Internal callers (sendAction's .catch) use the local toast alias.
  // TOAST_CAP is the hard ceiling on visible toasts — no burst of errors, however
  // varied, can ever exceed it (oldest is evicted). Coalescing by a normalized key
  // means that in practice a network blip shows just ONE toast with a ×N counter.
  var TOAST_CAP = 3;
  // The error glyph, rendered by the same Go helper every other status glyph
  // uses (statusGlyph), so the toast cannot draw a different shape. Static
  // markup with no request-derived content, so innerHTML is safe here; the
  // MESSAGE goes in through textContent.
  var TOAST_GLYPH = ` + toastGlyphJS() + `;
  // toastKey normalizes a message so volatile detail (status codes, ids, the
  // server's per-request error body) collapses to ONE coalesced toast. Without
  // this, htmx:responseError messages like "Request failed (500)" / "(502)" or
  // distinct server bodies each made their own toast and only the cap saved us.
  // We strip digits and long tails to a stable shape, so the whole family of
  // "Request failed (NNN)" / "boom: <varying>" collapses to a single ×N toast.
  function toastKey(msg) {
    return String(msg)
      .replace(/\d+/g, '#')      // 500/502/... -> #
      .replace(/\s+/g, ' ')       // collapse whitespace
      .trim()
      .slice(0, 40)               // ignore long varying tails
      .toLowerCase();
  }
  window.toast = function (msg) {
    var host = document.getElementById('action-toasts');
    if (!host) { try { alert(msg); } catch (e) {} return; }
    // Dedupe/coalesce: a network blip fails every in-flight request at once, each
    // firing an (often near-identical) error toast. Collapse same-FAMILY messages
    // (by normalized key) into ONE toast with a ×N counter so a burst can never
    // flood the screen.
    var key = toastKey(msg);
    var nodes = host.querySelectorAll('[data-toast-key]');
    for (var i = 0; i < nodes.length; i++) {
      if (nodes[i].getAttribute('data-toast-key') === key) {
        var el = nodes[i];
        var n = (parseInt(el.getAttribute('data-toast-count'), 10) || 1) + 1;
        el.setAttribute('data-toast-count', String(n));
        // Keep the FIRST message text (most representative) + a count.
        var txt = el.querySelector('[data-toast-text]') || el;
        txt.textContent = el.getAttribute('data-toast-msg') + '  (×' + n + ')';
        if (el._t) clearTimeout(el._t);
        el._t = setTimeout(function () { el.remove(); }, 5000);
        return;
      }
    }
    // Hard ceiling: evict oldest until strictly under the cap, then add one.
    while (host.children.length >= TOAST_CAP && host.firstChild) {
      host.removeChild(host.firstChild);
    }
    var t = document.createElement('div');
    // danger / on-danger: the pair the palette tests at 4.5:1. The old
    // translucent rose over the page measured 4.02:1, under AA for 14px text.
    // The glyph is the error SHAPE, so the toast does not rely on colour alone.
    t.className = 'pointer-events-auto flex items-start gap-2 rounded-lg bg-danger px-3 py-2 text-sm font-medium text-on-danger shadow-lg shadow-black/30';
    t.style.setProperty('--glyph-knock', 'rgb(var(--mu-danger))');
    t.setAttribute('role', 'alert');
    t.setAttribute('data-toast-msg', msg);
    t.setAttribute('data-toast-key', key);
    t.setAttribute('data-toast-count', '1');
    t.innerHTML = TOAST_GLYPH;
    var tx = document.createElement('span');
    tx.setAttribute('data-toast-text', '');
    tx.textContent = msg;
    t.appendChild(tx);
    host.appendChild(t);
    t._t = setTimeout(function () { t.remove(); }, 5000);
  };
  var toast = window.toast;

  // --- Custom type-to-filter comboboxes (dispatch modal repo/task pickers) ---
  // Wired via event delegation on document so it works whenever the modal body
  // is injected by htmx (inline scripts in swapped content are unreliable). Each
  // combobox is a [data-combobox] wrapper holding a hidden input
  // ([data-combobox-hidden], the submitted value), a visible text input
  // ([data-combobox-input], the filter/label), and a listbox
  // ([data-combobox-list]) of [data-combobox-option] items carrying data-value
  // + data-label.
  function cbInput(box) { return box.querySelector('[data-combobox-input]'); }
  function cbHidden(box) { return box.querySelector('[data-combobox-hidden]'); }
  function cbList(box) { return box.querySelector('[data-combobox-list]'); }
  function cbOptions(box) { return Array.prototype.slice.call(box.querySelectorAll('[data-combobox-option]')); }
` + rankJS + `  function cbMatches(label, q) { return window.musterRank(label, q) >= 0; }
  // cbModelLabel: trim the leading "openrouter/" provider prefix for DISPLAY only
  // (the full slug stays the option value). No-op for values without the prefix.
  function cbModelLabel(val) { return val.indexOf('openrouter/') === 0 ? val.slice('openrouter/'.length) : val; }

  // --- remote (model search) ---
  // A [data-combobox-remote="URL"] box fetches its options from URL?q=<input>
  // (debounced + cached). The visible input IS the submitted value (no hidden).
  var cbOptClass = 'cursor-pointer truncate px-3 py-2 text-sm text-fg hover:bg-accent/15 hover:text-fg data-[active=true]:bg-accent/15 data-[active=true]:text-fg';
  var cbCache = {};
  var cbTimers = {};
  function cbRemote(box) { return box.getAttribute('data-combobox-remote'); }
  // Render remote results while PRESERVING seeded quick-pick options
  // ([data-combobox-seed]) at the top: drop the previous remote results, filter
  // the seeds by the current query, then append remote slugs not already seeded.
  function cbRenderRemote(box, list) {
    var ul = cbList(box); if (!ul) return;
    var input = cbInput(box);
    var q = (input && input.value || '').trim().toLowerCase();
    var seeds = {};
    // Remove prior remote (non-seed) results; show/hide seeds by the query.
    Array.prototype.slice.call(ul.querySelectorAll('[data-combobox-option]')).forEach(function (o) {
      if (o.hasAttribute('data-combobox-seed')) {
        var label = (o.getAttribute('data-label') || '').toLowerCase();
        o.classList.toggle('hidden', !cbMatches(label, q));
        seeds[o.getAttribute('data-value') || ''] = true;
      } else {
        o.parentNode && o.parentNode.removeChild(o);
      }
    });
    (list || []).forEach(function (val) {
      if (seeds[val]) return; // already shown as a seeded quick-pick
      var disp = cbModelLabel(val); // trim openrouter/ for display; value stays full
      var li = document.createElement('li');
      li.setAttribute('role', 'option');
      li.setAttribute('data-combobox-option', '');
      li.setAttribute('data-value', val);
      li.setAttribute('data-label', disp);
      li.className = cbOptClass;
      li.textContent = disp;
      ul.appendChild(li);
    });
    cbOpen(box, true);
  }
  function cbFetchRemote(box) {
    var url = cbRemote(box); if (!url) return;
    var input = cbInput(box);
    var q = (input && input.value || '').trim();
    var key = url + '?q=' + q;
    if (cbCache[key]) { cbRenderRemote(box, cbCache[key]); return; }
    clearTimeout(cbTimers[url]);
    cbTimers[url] = setTimeout(function () {
      fetch(url + '?q=' + encodeURIComponent(q), { credentials: 'include' })
        .then(function (r) { return r.ok ? r.json() : []; })
        .then(function (list) { cbCache[key] = list || []; cbRenderRemote(box, cbCache[key]); })
        .catch(function () {});
    }, 200);
  }

  // --- recently-used (localStorage) ---
  // A [data-combobox-recents="KEY"] box floats recently-picked values to the top.
  function cbRecentsKey(box) { var k = box.getAttribute('data-combobox-recents'); return k ? 'muster.recent.' + k : null; }
  function cbGetRecents(box) {
    var k = cbRecentsKey(box); if (!k) return [];
    try { return JSON.parse(localStorage.getItem(k) || '[]'); } catch (e) { return []; }
  }
  function cbRemember(box, val) {
    var k = cbRecentsKey(box); if (!k || !val) return;
    var arr = cbGetRecents(box).filter(function (v) { return v !== val; });
    arr.unshift(val); arr = arr.slice(0, 8);
    try { localStorage.setItem(k, JSON.stringify(arr)); } catch (e) {}
  }
  // cbArrange puts the list in order. With a query: by rank (musterRank), and
  // within a rank recently-picked first, then the order the options arrived in
  // (seeds before a search's results). Without one: recently-picked first, then
  // arrival order — what the list showed before it was ranked. A remote
  // result the rule does not match at all (the other side matched it by its own
  // rule) goes after every ranked one rather than vanishing.
  function cbArrange(box) {
    var ul = cbList(box); if (!ul) return;
    var input = cbInput(box);
    var q = (input && input.value || '').trim();
    var recents = cbGetRecents(box);
    var remote = !!cbRemote(box);
    var opts = cbOptions(box);
    opts.forEach(function (o) {
      if (!o.hasAttribute('data-cb-seq')) { box.__cbSeq = (box.__cbSeq || 0) + 1; o.setAttribute('data-cb-seq', String(box.__cbSeq)); }
    });
    function key(o) {
      var t = q ? window.musterRank(o.getAttribute('data-label') || '', q) : 0;
      if (t < 0) t = remote && !o.hasAttribute('data-combobox-seed') ? 4 : 5;
      var r = recents.indexOf(o.getAttribute('data-value') || '');
      return [t, r < 0 ? 1e6 : r, +o.getAttribute('data-cb-seq')];
    }
    var keyed = opts.map(function (o) { return { o: o, k: key(o) }; });
    keyed.sort(function (a, b) { return a.k[0] - b.k[0] || a.k[1] - b.k[1] || a.k[2] - b.k[2]; });
    keyed.forEach(function (x) { ul.appendChild(x.o); });
  }

  function cbOpen(box, open) {
    var list = cbList(box), input = cbInput(box);
    if (!list) return;
    if (open) cbArrange(box);
    list.classList.toggle('hidden', !open);
    if (input) input.setAttribute('aria-expanded', open ? 'true' : 'false');
    if (!open) cbSetActive(box, null);
  }
  function cbSetActive(box, opt) {
    cbOptions(box).forEach(function (o) {
      o.setAttribute('data-active', o === opt ? 'true' : 'false');
    });
    if (opt && opt.scrollIntoView) opt.scrollIntoView({ block: 'nearest' });
  }
  function cbActive(box) { return box.querySelector('[data-combobox-option][data-active="true"]'); }
  function cbVisible(box) {
    return cbOptions(box).filter(function (o) { return !o.classList.contains('hidden'); });
  }
  function cbFilter(box) {
    var input = cbInput(box);
    var q = (input && input.value || '').trim().toLowerCase();
    var any = false;
    cbOptions(box).forEach(function (o) {
      var label = (o.getAttribute('data-label') || '').toLowerCase();
      var match = cbMatches(label, q);
      o.classList.toggle('hidden', !match);
      if (match) any = true;
    });
    return any;
  }
  function cbPick(box, opt) {
    var input = cbInput(box), hidden = cbHidden(box);
    var val = opt.getAttribute('data-value') || '';
    var label = opt.getAttribute('data-label') || '';
    if (hidden) {
      if (input) input.value = label;
      hidden.value = val;
      // Notify listeners (the agent-detail model form autosaves on this).
      hidden.dispatchEvent(new Event('change', { bubbles: true }));
    } else if (input) {
      // remote / value-mode (model): the visible input IS the submitted value.
      input.value = val;
    }
    cbRemember(box, val);
    cbOpen(box, false);
  }

  // Typing filters + opens the list, and clears the hidden value (a cleared text
  // input means "none"; a re-typed label is re-confirmed by selecting an option).
  document.addEventListener('input', function (e) {
    var input = e.target.closest && e.target.closest('[data-combobox-input]');
    if (!input) return;
    var box = input.closest('[data-combobox]');
    if (!box) return;
    if (cbRemote(box)) { cbFetchRemote(box); cbOpen(box, true); cbSetActive(box, null); return; }
    var hidden = cbHidden(box);
    if (hidden) hidden.value = '';
    cbFilter(box);
    cbOpen(box, true);
    cbSetActive(box, null);
  });

  // Focus/click on the input opens the list; clicking an option selects it.
  document.addEventListener('click', function (e) {
    var opt = e.target.closest && e.target.closest('[data-combobox-option]');
    if (opt) {
      var box = opt.closest('[data-combobox]');
      if (box) { cbPick(box, opt); return; }
    }
    var input = e.target.closest && e.target.closest('[data-combobox-input]');
    if (input) {
      var ibox = input.closest('[data-combobox]');
      if (ibox) {
        if (cbRemote(ibox)) cbFetchRemote(ibox); else cbFilter(ibox);
        cbOpen(ibox, true);
      }
      return;
    }
    // Outside-click closes any open comboboxes.
    document.querySelectorAll('[data-combobox]').forEach(function (box) {
      if (!box.contains(e.target)) cbOpen(box, false);
    });
  });

  // Keyboard: ArrowUp/Down move the highlight, Enter selects, Escape closes.
  document.addEventListener('keydown', function (e) {
    var input = e.target.closest && e.target.closest('[data-combobox-input]');
    if (!input) return;
    var box = input.closest('[data-combobox]');
    if (!box) return;
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      cbFilter(box); cbOpen(box, true);
      var vis = cbVisible(box);
      if (!vis.length) return;
      var cur = cbActive(box);
      var idx = vis.indexOf(cur);
      idx = e.key === 'ArrowDown' ? (idx + 1) % vis.length : (idx - 1 + vis.length) % vis.length;
      cbSetActive(box, vis[idx]);
    } else if (e.key === 'Enter') {
      var act = cbActive(box);
      if (act) { e.preventDefault(); cbPick(box, act); }
    } else if (e.key === 'Escape') {
      // 🔴 This branch is the FOCUSED-input path only, and it is NOT what
      // protects the enclosing dialog. Both this listener and the overlay
      // keydown above are on document in the bubble phase, and the overlay one
      // is registered FIRST — so it always runs first and no stopPropagation()
      // here could ever stop it. The precedence rule (open dropdown wins over
      // the dialog) is enforced there; see the ESCAPE PRECEDENCE comment.
      cbOpen(box, false);
    }
  });

  // --- Touch gesture: the mobile sidebar's edge-swipe ---
  // Below lg the sidebar is a slide-out overlay: an edge-swipe from the left
  // OPENS it; a left-swipe on the open sidebar CLOSES it. setOpen drives the
  // same data-open attribute the hamburger/backdrop use; prefers-reduced-motion
  // collapses the slide transition via CSS, so no JS motion branch is needed.
  // Mostly-vertical gestures are ignored so it never hijacks scrolling. Bound
  // once (initGlobal).
  //
  // 🔴 THERE IS NO TAB-SWITCH SWIPE ANY MORE, AND THAT IS DELIBERATE. A
  // horizontal swipe anywhere over a tabpanel used to walk the tab order. It was
  // unused, and it fired on gestures meant for something else: swiping the
  // recent-agents strip sideways switched the PARENT tab. Tabs change by a tap
  // on the sidebar or a segment, and nothing else. TestNoTabSwitchSwipe pins
  // its absence; recent-agents.spec.ts pins the strip.
  var sbX = 0, sbY = 0, sbMode = null; // 'open' | 'close' | null for this gesture
  var SB_EDGE = 28;     // px from the left edge that begins an open-swipe
  var SB_MIN_X = 45;    // px of horizontal travel to open/close the sidebar
  function sbMobile() { return window.innerWidth < 1024; }
  // ownsSideways: the touch began on something that takes a sideways swipe for
  // itself — an element marked [data-own-swipe] (the recent-agents strip, which
  // starts 12px from the edge at a phone's width) or any ancestor that scrolls
  // sideways — so a swipe there is that thing's, never the sidebar's.
  function ownsSideways(t) {
    for (var el = t; el && el.nodeType === 1 && el !== document.body; el = el.parentElement) {
      if (el.hasAttribute('data-own-swipe')) return true;
      if (el.scrollWidth > el.clientWidth + 1) {
        var ox = getComputedStyle(el).overflowX;
        if (ox === 'auto' || ox === 'scroll') return true;
      }
    }
    return false;
  }
  function sbIsOpen() { var r = document.getElementById('sidebar-root'); return !!(r && r.getAttribute('data-open') === 'true'); }
  document.addEventListener('touchstart', function (e) {
    sbMode = null;
    if (!e.touches || e.touches.length !== 1) return;
    var t = e.target;
    var x = e.touches[0].clientX, y = e.touches[0].clientY;
    // Close: the gesture starts inside the open sidebar panel. Open: it starts
    // within SB_EDGE px of the left edge while the sidebar is closed.
    if (sbMobile()) {
      if (sbIsOpen()) {
        if (t && t.closest && t.closest('#sidebar')) sbMode = 'close';
      } else if (x <= SB_EDGE && !ownsSideways(t)) {
        sbMode = 'open';
      }
    }
    sbX = x; sbY = y;
  }, { passive: true });
  document.addEventListener('touchend', function (e) {
    var ct = (e.changedTouches && e.changedTouches[0]);
    var mode = sbMode; sbMode = null;
    if (!mode || !ct) return;
    var sdx = ct.clientX - sbX, sdy = ct.clientY - sbY;
    if (Math.abs(sdx) < SB_MIN_X || Math.abs(sdx) <= Math.abs(sdy) * 1.2) return;
    if (mode === 'open' && sdx > 0) setOpen(true);
    else if (mode === 'close' && sdx < 0) setOpen(false);
  }, { passive: true });

  // Keyboard activation for label-buttons.
  //
  // A <label for> is activated by POINTER only — it has no native keyboard
  // activation at all. So a <label role="button" tabindex="0"> is focusable but
  // dead to Enter/Space unless something implements the widget contract it
  // claims. This delegated handler is that something: it is what lets
  // chatHistoryButton (and the session drawer's close control) keep role/tabindex
  // honestly. Delegated on document so it survives hx-boost body swaps and needs
  // no per-element rebinding.
  //
  // Space is preventDefault'ed because on a focusable non-control it scrolls the
  // page. The click() drives the <label>'s own for= association, so the CSS
  // checkbox-hack toggle stays the single source of truth for open/closed.
  //
  // The selector and the key set are NAMED so a11y_test.go can parse them and
  // then assert they are USED inside the handler body below. A substring check
  // for the bare literals 'Enter' / ' ' is satisfied by unrelated code elsewhere
  // in this script (the combobox, the tag input, a whitespace-collapsing
  // replace), which is exactly how the previous guard became vacuous. Do not
  // inline these back into the handler.
  var LABEL_BUTTON_SELECTOR = 'label[role="button"]';
  var LABEL_BUTTON_KEYS = ['Enter', ' '];
  document.addEventListener('keydown', function (e) {
    if (LABEL_BUTTON_KEYS.indexOf(e.key) === -1) return;
    var lb = e.target && e.target.closest && e.target.closest(LABEL_BUTTON_SELECTOR);
    if (!lb) return;
    e.preventDefault();
    lb.click();
  });

  // 🔴 renderQueued() USED TO BE CALLED HERE AND THE FUNCTION DOES NOT EXIST.
  // Same carve residue as the two popovers in the Escape handler above: it
  // painted the permission router's optimistic-decision queue indicator, which
  // muster does not render. The ReferenceError was the LAST statement of
  // initGlobal, so it propagated out of init() — and init() calls initPage()
  // AFTER initGlobal(). On a cold load, initPage therefore never ran at all:
  // the sidebar open/close buttons and the SPA tab wiring were unbound until
  // some later htmx:load re-entered init() with __cgInit already set.
  } // end initGlobal

  // init: per-document. Binds the global delegated listeners once (guarded), and
  // (re)wires the per-page elements every time — so it is safe to run on both
  // DOMContentLoaded and htmx:load (hx-boost swaps).
  function init() {
    if (!window.__cgInit) { window.__cgInit = true; initGlobal(); }
    initPage();
    // 🔴 ONCE PER DOCUMENT. init() re-runs on every htmx:load; restoreNav is not
    // idempotent against that (see its header), so it gets its own guard rather
    // than riding initPage's.
    //
    // ⚠ IT RUNS AFTER initPage, AND NOTHING KNOWN REQUIRES THAT ORDER. An earlier
    // comment here claimed restoring first "would simply be overwritten" by
    // showForPath(); that is false in every case. switchTab replaceState's the
    // path BEFORE it calls show(), so a later showForPath() reads the restored
    // tab back out of the URL and repaints the same one.
    // The order is kept because it is the one every test above was measured
    // against — not because a mechanism demands it. If you need to move it,
    // that is a real change: re-run the suite rather than trusting this note.
    if (!window.__cgNavRestored) { window.__cgNavRestored = true; restoreNav(); }
  }

  // Run after the full document is parsed so body-level siblings (the popover,
  // FABs, modal shells, the queued indicator) exist when we wire handlers.
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
  // Also (re)init after an hx-boost body-swap into the home shell: htmx fires
  // htmx:load on the new content. init() is idempotent (global listeners are
  // __cgInit-guarded; per-page elements carry data-cg-bound) so this is safe.
  // <body> survives boost swaps (only its innerHTML is replaced) so this IIFE
  // can re-run and re-add the listener; guard it so it binds exactly once.
  if (!window.__cgAppLoad) {
    window.__cgAppLoad = true;
    document.body.addEventListener('htmx:load', init);
  }
})();
`))
}

// resyncScript keeps the task list fresh after the app is backgrounded and
// reopened. The htmx SSE extension auto-reconnects, but a dropped connection can
// miss events while hidden; so we also refetch on focus, on visibility regain,
// and on SSE (re)connect. It fires a body-level "muster:resync" event, and is
// fully feature-detected/guarded.
//
// 🔴 WHO LISTENS IS THE HALF THAT MATTERS, AND THIS DOC NAMED A PANEL THAT DOES
// NOT EXIST HERE. It said "the requests panel listens for (see Page's
// hx-trigger)" — the permission router's queue, which muster does not serve. The
// subscribers are #tasks-list on the shell (notes.go) and the live card on the
// task detail document (task_detail.go); a document carrying this script and
// neither of those receives an event nobody is waiting for, which is what the
// shell did.
func resyncScript() g.Node {
	return Script(g.Raw(`
(function () {
  function resync() {
    try {
      // Prefer the htmx-driven trigger so the existing wiring handles the swap.
      document.body.dispatchEvent(new CustomEvent('muster:resync'));
    } catch (e) {
      // Fallback: direct htmx ajax if CustomEvent/dispatch is unavailable.
      //
      // 🔴 IT NAMES THE SAME ROUTE, TARGET AND SWAP THE PRIMARY PATH CAUSES, AND
      // THAT IS THE WHOLE REQUIREMENT ON IT. It used to name a route the
      // permission router serves and this service does not — GET /ui/requests,
      // answered here by the mux's own 404 — into a target id no document in this
      // app renders. Both halves were wrong, and neither could be observed:
      // nothing reaches this branch in a browser that has CustomEvent, so the
      // fallback had never once run.
      //
      // ⚠ THE ELEMENT IS LOOKED UP RATHER THAN ASSUMED. resyncScript is rendered
      // on the shell AND on the task detail document, and only the shell has a
      // task list; on detail the muster:resync listener above is the card's own,
      // so there is nothing for this branch to do there.
      var list = document.getElementById('tasks-list');
      if (window.htmx && list) {
        try {
          window.htmx.ajax('GET', '/ui/tasks', { target: '#tasks-list', swap: 'morph:innerHTML' });
        } catch (e2) {}
      }
    }
  }
  document.addEventListener('visibilitychange', function () {
    if (document.visibilityState === 'visible') resync();
  });
  window.addEventListener('focus', resync);
  // SSE (re)connect: the htmx sse extension dispatches htmx:sseOpen on (re)open.
  document.body.addEventListener('htmx:sseOpen', resync);

  // Surface htmx failures. htmx does NOT swap non-2xx responses, so without this
  // every failed form/mutation (dispatch, note, status, comment)
  // would fail SILENTLY — modal frozen, no feedback. Show the server's error text
  // (or a generic message) as a toast so the user always knows.
  document.body.addEventListener('htmx:responseError', function (e) {
    var xhr = e.detail && e.detail.xhr;
    var msg = (xhr && xhr.responseText ? xhr.responseText : '').trim();
    if (!msg || msg.charAt(0) === '<') msg = 'Request failed (' + ((xhr && xhr.status) || '?') + ')';
    if (msg.length > 200) msg = msg.slice(0, 200) + '…';
    // toast lives on window (defined in appScript); guard so this listener never
    // throws even if appScript somehow didn't run.
    if (window.toast) window.toast(msg);
  });
  document.body.addEventListener('htmx:sendError', function () {
    if (window.toast) window.toast('Network error — could not reach the server');
  });
  // 🔴 The THIRD htmx failure mode. #tasks-list carries hx-request {"timeout":
  // 15000} and htmx resolves hx-request by walking parentElement (no
  // hx-disinherit), so that cap applies to every request-issuing descendant too
  // — the status PATCH, the comment POST, delete, the dispatch/edit GETs. Those
  // aborts dispatch htmx:timeout, NOT responseError/sendError, so before this
  // listener existed a timed-out mutation failed with no feedback whatsoever.
  document.body.addEventListener('htmx:timeout', function () {
    if (window.toast) window.toast('Request timed out — it may not have been recorded.');
  });

  // 🔴 THE STALE-CARD GUARD. #task-{id} has TWO unsynchronised writers — the
  // response to a mutation (status PATCH, comment POST/DELETE, tag edit, and
  // POST /agents when dispatching from /tasks/{id}) and the SSE-triggered
  // re-fetch in taskDetailLive — and both morph:outerHTML into the same node.
  // Neither waits for the other, so before this listener the one that ARRIVED
  // last won, whichever had read the store last.
  //
  // Measured: a dispatch issues POST /agents at +195ms; the agent.changed SSE
  // it causes makes the page GET /ui/tasks/{id}/card at +233ms — before the
  // POST returns — and that read sees the task still "open", because
  // advanceTaskToInProgress has not committed yet. The POST's own answer
  // ("in_progress") came back at +278ms. 38ms, in the harmless order. Reverse
  // them and the page shows "open" for a task the store has as "in_progress",
  // with nothing to correct it until the next SSE event or a resync.
  //
  // The fix is a VERSION, not a delay: every card carries data-card-rev =
  // notes.updated_at in ns, which the STORE bumps inside the same statement as
  // each change. A swap whose incoming rev is STRICTLY OLDER than what is on
  // screen carries strictly less recent data and is dropped. Ties always apply,
  // so a change that legitimately does not move updated_at is never lost.
  //
  // The listener is NAMED, and that is load-bearing rather than style: several
  // scripts in this app attach an htmx:beforeSwap handler, so "the page carries
  // a beforeSwap listener" cannot tell you THIS guard is present.
  // TestEveryDocumentThatRendersATaskCardCarriesTheStaleGuard matches on this
  // identifier; renaming it silently drops that document-level seam check.
  function musterRefuseStaleCardSwap(e) {
    try {
      var d = e.detail;
      if (!d || !d.shouldSwap) return;
      var target = d.target;
      if (!target || !target.id || target.id.indexOf('task-') !== 0) return;
      var cur = target.getAttribute('data-card-rev');
      if (!cur) return;
      var text = d.serverResponse;
      if (typeof text !== 'string' || !text) return;
      // Parse rather than regex the response: a task BODY is user markdown and
      // could contain the attribute's own text.
      var doc = new DOMParser().parseFromString(text, 'text/html');
      var incoming = doc.body && doc.body.querySelector('[data-card-rev]');
      if (!incoming) return;
      var a = Number(incoming.getAttribute('data-card-rev'));
      var b = Number(cur);
      // 🔴 TWO UNSTATED PRECONDITIONS, both now stated because the comparison is
      // only correct under them.
      //
      // PRECISION. These are nanosecond magnitudes (~1.79e18) held in a double,
      // where one ULP is 256ns — so revs closer together than that COMPARE EQUAL.
      // Safe only because Postgres timestamptz is MICROSECOND resolution: two
      // distinct updated_at values are >=1000ns apart, ~4x the ULP. If a store
      // ever produced true nanosecond precision this silently starts treating
      // distinct revisions as ties.
      //
      // SIGN. isFinite() accepts large NEGATIVE values, and time.Time{}.UnixNano()
      // is -6795364578871345152. An incoming rev of that shape is "older" than
      // everything, so EVERY response for that card would be refused and only a
      // full reload could recover. noteCard omits the attribute entirely for a
      // zero timestamp; this is the second half of that fix, so a response from
      // any other source cannot wedge the card either. Non-positive => treat the
      // version as unknown and apply the swap, which is the pre-guard behaviour.
      if (!isFinite(a) || !isFinite(b)) return;
      if (a <= 0 || b <= 0) return;
      // 🔴 STRICTLY less-than. TIES MUST APPLY: a linked agent going "running"
      // does not move notes.updated_at, so the sse:agent.changed re-fetch that
      // updates the status chip arrives as a TIE. Under <= that swap is refused
      // and the chip silently stops updating live; a comment retraction's
      // response is dropped the same way. Changing this to <= is caught by the
      // tie case in e2e/tests/card-revision-tie.spec.ts — a Go string-match
      // cannot see it.
      //
      // (No backticks in this comment: it lives inside a Go raw string literal,
      // where one would terminate the string mid-function.)
      if (a < b) {
        d.shouldSwap = false;
      }
    } catch (err) {}
  }
  document.body.addEventListener('htmx:beforeSwap', musterRefuseStaleCardSwap);
})();
`))
}

// relTimeScript keeps the cards' relative timestamps fresh (the list only
// refetches on SSE events, so a server-rendered string would otherwise freeze).
// It reformats every <time data-ts> on load, every 15s, and after each htmx
// swap (new/replaced cards). The format mirrors relTimeString.
func relTimeScript() g.Node {
	return Script(g.Raw(`
(function () {
  function fmt(ms) {
    var s = Math.max(0, Math.floor((Date.now() - ms) / 1000));
    if (s < 60) return s + 's';
    var m = Math.floor(s / 60); if (m < 60) return m + 'm';
    var h = Math.floor(m / 60); if (h < 24) return h + 'h';
    return Math.floor(h / 24) + 'd';
  }
  function tick() {
    document.querySelectorAll('time[data-ts]').forEach(function (el) {
      var ms = parseInt(el.getAttribute('data-ts'), 10);
      if (ms) el.textContent = fmt(ms);
    });
  }
  tick();
  setInterval(tick, 15000);
  document.body.addEventListener('htmx:afterSettle', tick);
})();
`))
}

// taskModalScript owns BOTH bottom sheets' DISMISS semantics (cgModalDismiss /
// cgModalClose / cgModalDirty, keyed by modal id) and the task sheet's save
// feedback. The dispatch sheet shares the same discard-confirm pattern rather
// than getting a second one — its dirty check is narrower (free text only, see
// agentDirty) because its config fields are pre-filled from localStorage.
// Three separate gaps it closes, all on the edit path:
//
//  1. A stray outside click (or Escape) silently discarded an in-progress edit —
//     verified: typed text did not survive close+reopen. cgTaskModalDismiss
//     dirty-checks the EDIT form first and reveals the in-app discard confirm
//     instead of closing. It is DOM-only, never window.confirm(), which is
//     suppressed in the installed standalone PWA.
//  2. A successful save gave NO feedback at all — the modal just vanished.
//     cgTaskModalSaved re-pulses the app's EXISTING .card-enter highlight (the
//     accent ring already defined in web/css/input.css and already zeroed by the
//     prefers-reduced-motion block) on the morphed card. No new CSS.
//  3. Focus was dropped on the document body when the modal closed, so a keyboard
//     user restarted their traversal from the top of the page. A MutationObserver
//     on the modal's class attribute restores focus to the Edit button that
//     opened it, whichever of the four close paths ran.
//
// 🔴 Boost-safety: the document-level listener binds ONCE (__cgTaskModalInit,
// same discipline as tagScript's __cgTagInit) because htmx re-executes in-body
// <script> tags on every boosted swap. The remembered trigger lives on `window`,
// NOT in the closure — a later re-execution gets a FRESH closure while the
// once-bound listener still writes into the FIRST one, so a closure variable
// would be written by one run and read by another.
func taskModalScript() g.Node {
	return Script(g.Raw(`
(function () {
  var firstRun = !window.__cgTaskModalInit;
  window.__cgTaskModalInit = true;

  function modal() { return document.getElementById('task-modal'); }
  function body() { return document.getElementById('task-modal-body'); }
  function bar() { return document.getElementById('task-modal-discard'); }

  // --- Dirty checks, one per sheet -------------------------------------------
  //
  // Dirty only for the EDIT form (data-task-edit-form). The CREATE form is
  // excluded on purpose: advancedDefaults pre-fills directory/model from
  // localStorage recents, so a returning user's untouched create form would read
  // as dirty the instant it opened and the backdrop would stop dismissing it.
  function taskDirty() {
    var b = body();
    var form = b && b.querySelector('[data-task-edit-form]');
    if (!form) return false;
    // <select> is in the query as a FORWARD-LOOKING branch, not a bug fix.
    // 🔴 The claim this comment used to make — "a changed status/model select was
    // invisible to the guard" — was FALSE and is retracted: the rendered edit form
    // contains ZERO <select> elements. directoryCombobox renders the app's
    // combobox (input + <ul>) — it was an <input list=...> until the directory
    // picker moved to a seed-plus-server-search shape — the model field is the
    // same combobox component, and
    // the status <select> lives on the CARD, outside the form. So this branch is
    // currently UNREACHABLE. It is kept because the sweep below is generic and a
    // future <select> should not silently fall through it — but it is an
    // invariant guard, and TestTaskDirtyGuardSeesTags is labelled as
    // one, not counted as regression coverage.
    var els = form.querySelectorAll('input, textarea, select');
    for (var i = 0; i < els.length; i++) {
      var el = els[i];
      if (el.type === 'checkbox' || el.type === 'radio') {
        if (el.checked !== el.defaultChecked) return true;
      } else if (el.type === 'file') {
        if (el.files && el.files.length) return true;
      } else if (el.tagName === 'SELECT') {
        // A <select>'s "default" is the option carrying the selected attribute —
        // and when NO option carries one the browser's default is the FIRST
        // option, not ''. 🔴 Falling back to '' made the branch read PERMANENTLY
        // dirty for any such select, so the first <select> ever added to this form
        // would have nagged "Discard unsaved changes?" on every single dismiss.
        var opts = el.options, def = opts.length ? opts[0].value : '';
        for (var s = 0; s < opts.length; s++) { if (opts[s].defaultSelected) { def = opts[s].value; break; } }
        if (el.value !== def) return true;
      } else if (el.value !== el.defaultValue) {
        return true;
      }
    }
    // 🔴 TAGS are not comparable field-by-field. Removing a chip DELETES its
    // hidden <input name="tag"> outright (tagScript's remove handler), so there
    // is nothing left whose .value could differ from its .defaultValue — the
    // loop above saw a removal as "clean" and the backdrop silently discarded
    // it. (Adding a chip only registered by ACCIDENT: chipFor sets .value as a
    // property, leaving defaultValue "". Do not rely on that.) Compare the
    // SUBMITTED SET against the server-rendered baseline instead — that catches
    // add, remove, and add-then-remove-something-else alike.
    var chipBox = form.querySelector('[data-tag-chips]');
    if (chipBox) {
      var base = chipBox.getAttribute('data-tag-baseline') || '';
      var now = Array.prototype.map.call(
        form.querySelectorAll('[data-tag-chip]'),
        function (c) { return c.getAttribute('data-tag-chip') || ''; }
      ).sort().join(',');
      if (now !== base) return true;
    }
    return false;
  }

  // The dispatch sheet's guard is deliberately narrower: only its FREE TEXT.
  // Its config fields (model / repo / directory) are pre-filled from
  // localStorage recents as PROPERTIES, so a value-vs-defaultValue sweep would
  // read an untouched dispatch form as dirty the instant it opened — the exact
  // trap that excludes the task CREATE form. The prompt textarea is the long,
  // unrecoverable thing (measured: Escape hid the sheet, and reopening refetched
  // /ui/agents/new so the prompt came back empty).
  function agentDirty() {
    var b = document.getElementById('agent-modal-body');
    if (!b) return false;
    var els = b.querySelectorAll('textarea');
    for (var i = 0; i < els.length; i++) {
      if (els[i].value !== els[i].defaultValue) return true;
    }
    return false;
  }

  // --- Generic dismiss, shared by BOTH bottom sheets --------------------------
  // One pattern, not two: each sheet <id> pairs with an <id>-discard bar, and an
  // AMBIGUOUS dismiss (backdrop tap / Escape) reveals that bar instead of
  // destroying unsaved text. DOM-only, never window.confirm() — native dialogs
  // are suppressed in the installed standalone PWA and would never resolve.
  var DIRTY = { 'task-modal': taskDirty, 'agent-modal': agentDirty };
  function discardBar(id) { return document.getElementById(id + '-discard'); }

  window.cgModalDirty = function (id) {
    var f = DIRTY[id];
    return !!(f && f());
  };
  // 🔴 OPEN = CLEAR, then reveal. Every open path (both FABs, the card Edit and
  // the card Dispatch button) reveals the sheet from its own hx-on:click and
  // lets htmx fill <id>-body asynchronously. During that in-flight window the
  // body still held the PREVIOUS open's form, and the discard bar is a SIBLING
  // of the body so it survives the swap — so the dirty guard read stale text and
  // the user faced "Discard unsaved changes?" over a form that was not theirs.
  // Measured with /ui/agents/new delayed 1200ms: bar=true, promptValue="the
  // previous prompt", then after the swap bar=true, promptValue="" — and Escape
  // would not close the sheet. That also made tasks-mobile's "Escape does not
  // destroy a typed dispatch prompt" flaky (red first attempt, green on retry).
  //
  // Emptying the body makes the guard STRUCTURALLY unable to consult a previous
  // open: there are no fields to read until the swap lands. The element itself
  // survives (textContent, not replaceWith), so htmx's pending swap still finds
  // its target.
  //
  // The discard-bar hide states the open-state invariant from the other side. Be
  // honest about it: it is currently UNREACHABLE — every path that hides a sheet
  // goes through cgModalClose, which hides the bar too, so no open can inherit a
  // visible bar. Measured (mutant M7): neutering this clause kills no test in
  // either suite. It is kept because cgModalOpen is the one place that defines a
  // known-good open state, not because it fixes anything.
  window.cgModalOpen = function (id) {
    var b = document.getElementById(id + '-body');
    if (b) b.textContent = '';
    window.cgModalShow(id);
  };
  // cgModalShow is the open WITHOUT the emptying: for a body that was loaded on
  // purpose before the sheet was shown (composeOnLoad, which opens the sheet only
  // once its body has landed, so it does not grow on screen — a layout shift).
  window.cgModalShow = function (id) {
    var d = discardBar(id); if (d) d.classList.add('hidden');
    var m = document.getElementById(id); if (m) m.classList.remove('hidden');
  };
  // Hard close: hide the sheet and its discard bar. Used by ✕ (an explicit,
  // deliberate discard) and by a successful save/dispatch.
  window.cgModalClose = function (id) {
    var d = discardBar(id); if (d) d.classList.add('hidden');
    var m = document.getElementById(id); if (m) m.classList.add('hidden');
  };
  window.cgModalDismiss = function (id) {
    var m = document.getElementById(id);
    if (!m || m.classList.contains('hidden')) return;
    if (window.cgModalDirty(id)) {
      var d = discardBar(id);
      // Only refuse to close if there IS something to ask with; otherwise a
      // missing bar would trap the user in a dialog with no exit.
      if (d) { d.classList.remove('hidden'); return; }
    }
    window.cgModalClose(id);
  };

  // Task-sheet aliases: the existing call sites (backdrop, ✕, save, discard bar)
  // keep their names, and there is still exactly one implementation.
  window.cgTaskModalDirty = function () { return window.cgModalDirty('task-modal'); };
  window.cgTaskModalClose = function () { window.cgModalClose('task-modal'); };
  window.cgTaskModalDismiss = function () { window.cgModalDismiss('task-modal'); };

  // Success feedback on the card the edit morphed.
  //
  // 🔴 It must paint AFTER the settle, not now: htmx:afterRequest can fire before
  // idiomorph finishes, and the morph syncs the class attribute from the server's
  // (highlight-free) card — so a class added here would be silently stripped.
  // Measured: painting inline left the card with no highlight at all. remove ->
  // reflow -> add so a SECOND save restarts the animation (re-adding a class that
  // is already present does not).
  window.cgTaskModalSaved = function (cardId) {
    var done = false;
    function paint() {
      if (done) return;
      var c = document.getElementById(cardId);
      if (!c) return;
      done = true;
      c.classList.remove('card-enter');
      void c.offsetWidth;
      c.classList.add('card-enter');
    }
    document.body.addEventListener('htmx:afterSettle', function once() {
      document.body.removeEventListener('htmx:afterSettle', once);
      paint();
    });
    // Fallback: if no settle event arrives (nothing swapped), still acknowledge.
    setTimeout(paint, 400);
  };

  if (firstRun) {
    // Capture phase: remember which Edit button opened the modal even though the
    // button's own hx-on:click also fires.
    //
    // 🔴 Remember a SELECTOR, not the element. A successful save morphs the card,
    // and the Edit button carries no id, so idiomorph can replace the node — a
    // held reference is then detached and focusing it is a no-op (measured: focus
    // landed on <body>). The card's id is stable, so re-resolve through it.
    document.addEventListener('click', function (e) {
      var t = e.target && e.target.closest && e.target.closest('[data-edit-trigger]');
      if (!t) return;
      var card = t.closest('[id^="task-"]');
      window.__cgEditTriggerSel = card ? '#' + card.id + ' [data-edit-trigger]' : null;
    }, true);
  }

  // restoreFocus re-resolves the remembered Edit trigger and focuses it. It runs
  // on close AND again on the next settle, because the close can land before the
  // morph that recreates the button.
  function restoreFocus() {
    var sel = window.__cgEditTriggerSel;
    if (!sel) return;
    var t = document.querySelector(sel);
    if (t) { try { t.focus(); } catch (e) {} }
  }

  // Restore focus to the invoking Edit button on EVERY close path (✕, backdrop,
  // Escape, save). Bound per-element and marked, so a boosted body swap that
  // replaces #task-modal gets a fresh observer while the detached old one is
  // inert.
  function bindFocusObserver() {
    var m = modal();
    if (!m || m.__cgFocusObs) return;
    m.__cgFocusObs = true;
    new MutationObserver(function () {
      var el = modal();
      if (!el || !el.classList.contains('hidden')) return;
      var d = bar(); if (d) d.classList.add('hidden');
      restoreFocus();
      document.body.addEventListener('htmx:afterSettle', function once() {
        document.body.removeEventListener('htmx:afterSettle', once);
        restoreFocus();
        window.__cgEditTriggerSel = null;
      });
      setTimeout(function () { restoreFocus(); window.__cgEditTriggerSel = null; }, 400);
    }).observe(m, { attributes: true, attributeFilter: ['class'] });
  }
  // #task-modal is a body-level sibling rendered AFTER this script, so it does
  // not exist yet at parse time — bind once the document is complete, and again
  // on every htmx:load so a boosted swap's fresh modal element is re-observed.
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', bindFocusObserver);
  } else {
    bindFocusObserver();
  }
  if (firstRun) document.body.addEventListener('htmx:load', bindFocusObserver);
})();
`))
}

// tagNormalizeJS is the chip editor's tag normalizer, embedded from its own file
// rather than written inline, because the SAME bytes are read by
// extension/tests/project-vectors.test.js and driven from the shared golden
// vector table (testdata/project-normalization-vectors.json). Inline, it was a
// third hand-rolled spelling of notes.NormalizeTag and it had already drifted.
//
//go:embed js/tag-normalize.js
var tagNormalizeJS string

// filterToggleJS is the filter-chip selection rule (multi-select AND for ordinary
// tags, single-select within a `data-tag-filter-exclusive` group). Embedded from
// its own file so extension/tests/filter-toggle.test.js can EXECUTE it — a
// substring assertion over inline handler source proves nothing about behaviour,
// and the last round already had one that a `…-DISABLED` mutant satisfied.
//
//go:embed js/filter-toggle.js
var filterToggleJS string

// tagScript drives the two client-side halves of Task tags:
//
//  1. The FILTER chip row. Selection is multi-select AND, persisted in
//     localStorage under 'muster.tasks.tagfilter' (matching the existing
//     'muster.recent.*' precedent). Rather than hold the filter in JS state, it
//     writes the selection into #tasks-list's hx-get URL — which, TOGETHER WITH
//     the htmx:configRequest listener below, is what makes EVERY refresh path
//     (the load trigger, sse:task.changed, sse:agent.changed, tasks:changed) keep
//     the filter instead of silently resetting to "all tasks".
//
//     🔴 THE LISTENER IS NOT AN OPTIONAL EXTRA. This comment used to claim the
//     hx-get write alone was enough, and it was false from the day the filter
//     shipped: htmx captures the verb path at process time, so the attribute and
//     the request had drifted apart and every trigger-driven refetch asked for
//     the unfiltered board. See the listener for the measurement.
//
//  2. The EDITOR's tag input: type a tag + Enter (or comma) → a chip carrying a
//     hidden `tag` input; × removes it. The visible input is deliberately
//     unnamed, so a half-typed value can never be submitted as a tag.
//
// Telemetry is COUNTS + namespaces only — never tag values, which can carry
// repo/project names (see metrics.RoutingTags for the same rule server-side).
func tagScript() g.Node {
	return Script(g.Raw(`
(function () {
  var KEY = 'muster.tasks.tagfilter';
` + filterToggleJS + `

  // 🔴 Bind the delegated document listeners EXACTLY ONCE. htmx re-executes
  // in-body <script> tags on every hx-boost body swap (a sidebar tab nav), and
  // document-level listeners survive the swap — so without this guard the chip
  // click handler stacks up and a single click TOGGLES TWICE (net zero: the
  // filter silently never applies). Same discipline as appScript's __cgInit.
  var firstRun = !window.__cgTagInit;
  window.__cgTagInit = true;

  function get() {
    try { var a = JSON.parse(localStorage.getItem(KEY) || '[]'); return Array.isArray(a) ? a : []; }
    catch (e) { return []; }
  }
  function set(arr) {
    try { localStorage.setItem(KEY, JSON.stringify(arr)); } catch (e) {}
  }
  // The board query URL. It carries the tag filter AND the status —
  // see internal/ui/tags.go boardURL, which builds the same string server-side
  // for the show-more control.
  //
  // 🔴 IT DELIBERATELY DROPS ANY limit=. Changing a filter changes WHICH tasks
  // are in the list, so carrying a "show more" expansion across that change
  // would page through a set the user never asked to page through; resetting to
  // the first page is the honest behaviour. The limit is preserved across
  // everything that does NOT change the filter — every SSE refetch, the load
  // trigger and the error Retry — because the [data-tasks-more] branch of the
  // click handler below writes the server-computed limit into #tasks-list's
  // hx-get AND the htmx:configRequest listener below turns that attribute into
  // the request each of those three paths actually issues. Retry is the one that
  // never needed the listener (it passes the URL to htmx.ajax itself).
  function url(tags, status) {
    var q = [];
    if (status) q.push('status=' + encodeURIComponent(status));
    tags.forEach(function (t) { q.push('tag=' + encodeURIComponent(t)); });
    return q.length ? '/ui/tasks?' + q.join('&') : '/ui/tasks';
  }
  // The ACTIVE status, read back off #tasks-list's own hx-get.
  //
  // 🔴 The URL is the single home of the board's query state; there is no second
  // copy in JS to fall out of step with it. Tags are the one exception (they are
  // persisted in localStorage, which predates this), and even they are written
  // THROUGH the URL — so a tag toggle that did not re-read the status here would
  // silently clear the status chip, which is the exact composition bug the
  // status filter exists to avoid.
  function statusOf() {
    var el = document.getElementById('tasks-list');
    var u = (el && el.getAttribute('hx-get')) || '';
    var m = /[?&]status=([^&]*)/.exec(u);
    return m ? decodeURIComponent(m[1]) : '';
  }
  // Keep #tasks-list's hx-get in sync with the persisted filter so every trigger
  // (load / sse / tasks:changed) requests the FILTERED list. The attribute is
  // only half of that: the htmx:configRequest listener below is what carries it
  // into the request htmx issues from a trigger.
  function syncTarget() {
    var el = document.getElementById('tasks-list');
    if (el) el.setAttribute('hx-get', url(get(), statusOf()));
    return el;
  }
  // 🔴 'source: el' is load-bearing, not decoration. htmx defaults an ajax()
  // call's source to document.body (issueAjaxRequest: 'if (elt == null) elt =
  // body'), and #tasks-list's failure handlers are ELEMENT listeners guarded by
  // 'event.target===this' — an event fired on their own ANCESTOR can never reach
  // them. Without a source a failed filter change left the skeleton (or the old
  // list) standing with no error and no Retry, and ALSO missed the 15s
  // hx-request timeout #tasks-list inherits down, so a hung request never
  // aborted. Identical defect to the one #296 fixed in cgTasksListRetry; that
  // fix did not reach this call site. Pinned as a ledger over BOTH call sites by
  // TestEveryTasksListAjaxIsSourcedAtTheContainer.
  //
  // 🔴 ONE AJAX CALL SITE, and it is the reason refreshTo takes a URL rather than
  // deriving one. The board now has three ways to change its query — a tag chip,
  // a status chip and Show more — and each one deriving its own htmx.ajax call
  // would be three chances to omit 'source: el' (that omission is #296, and the
  // second copy of it survived the first fix). They all funnel here, and
  // TestEveryTasksListAjaxIsSourcedAtTheContainer enumerates what is left.
  function refreshTo(u) {
    var el = document.getElementById('tasks-list');
    if (el) el.setAttribute('hx-get', u);
    if (!el || !window.htmx) return;
    try { window.htmx.ajax('GET', u, { source: el, target: '#tasks-list', swap: 'morph:innerHTML' }); } catch (e) {}
  }
  function refresh() { refreshTo(url(get(), statusOf())); }
  function apply(tags, status) {
    set(tags);
    refreshTo(url(tags, status));
    try { window.cgTrack('tag.filter', { count: tags.length, status: status || 'all' }); } catch (e) {}
  }

  // (window.cgTagFilterClear used to be exported here so taskHashScript could
  // clear the filter through THIS call site when a deeplinked task was filtered
  // out of the board. taskHashScript now redirects to the server-rendered
  // /tasks/<id>, which cannot miss a filtered-out task, so its last consumer is
  // gone and the export went with it. The [data-tag-filter-clear] chip below
  // calls apply([]) directly. Do not reintroduce a second clear path: it would
  // have to re-derive the storage key, the URL builder AND htmx.ajax's
  // 'source: el', and getting that last one wrong is silent — it reintroduces
  // the exact #296 defect, a failed refresh whose error can never reach
  // #tasks-list's element listeners. TestEveryTasksListAjaxIsSourcedAtThe
  // Container pins the call sites; one rule, one place.)

  if (firstRun) {
  // --- the hx-get attribute -> the actual request -----------------------------
  //
  // 🔴 WITHOUT THIS, WRITING #tasks-list's hx-get DOES NOTHING TO WHAT ITS
  // TRIGGERS FETCH. htmx 2.0.4 (web/static/vendor/htmx.min.js, processVerbs)
  // reads a node's verb path ONCE, at process time, and CLOSES OVER it:
  //
  //     const o = te(t,'hx-'+r); i=true; n.path=o; n.verb=r;
  //     e.forEach(function(e){ St(t,e,n,function(e,t){ … de(r,o,n,t) }) })
  //
  // every trigger fires de(r, o, …) with the CAPTURED o. #tasks-list is swapped
  // morph:innerHTML — the container itself is never replaced, so htmx never
  // re-processes it — and o therefore stays the page's original '/ui/tasks' for
  // the lifetime of the tab. refreshTo's setAttribute moved the attribute and
  // nothing else.
  //
  // MEASURED at PR #727's head (57 tasks, page 50): after Show more the
  // attribute read '/ui/tasks?limit=100' while the next sse:task.changed refetch
  // requested a bare '/ui/tasks' and the board collapsed to 50 cards. The tag
  // filter was on the same broken channel and had been since it shipped —
  // hx-get '/ui/tasks?tag=api', refetch '/ui/tasks' — which is why several
  // comments in this file and in internal/ui/notes.go asserted a survival
  // mechanism that was never wired. They are corrected, not merely annotated.
  // (window.cgTasksListRetry in internal/ui/notes.go was NEVER affected: it
  // re-reads hx-get and passes it to htmx.ajax explicitly, which takes the URL
  // as an argument rather than from the captured path.)
  //
  // THE FIX. htmx re-reads the request path from the event AFTER
  // htmx:configRequest ('n=C.path' in the same bundle), so that is the one point
  // at which the attribute can still become the request. e2e/tests/
  // task-board-paging.spec.ts pins it off the WIRE (page.on('request')), because
  // an attribute assertion was already green while the bug was live.
  //
  // 🔴 SCOPED BY IDENTITY, deliberately NOT by .closest('#tasks-list'). Every
  // card INSIDE the list issues its own requests (the status PATCH, the comment
  // POST, the dispatch/edit GETs); a subtree-wide rewrite would point all of
  // them at the board URL.
  document.body.addEventListener('htmx:configRequest', function (e) {
    var el = e.target;
    if (!el || el.id !== 'tasks-list' || !e.detail) return;
    var want = el.getAttribute('hx-get');
    if (want) e.detail.path = want;
  });

  // --- filter chips (delegated: the row is re-rendered on every list swap) ---
  document.addEventListener('click', function (e) {
    var t = e.target;
    if (!t || !t.closest) return;
    // Clear drops EVERY filter, tags and status alike. It is the control the
    // filtered-empty state offers as the way out, and a "Clear filter" that left
    // a status chip engaged would leave the user staring at the same empty board
    // it was supposed to rescue them from.
    var clear = t.closest('[data-tag-filter-clear]');
    if (clear) { e.preventDefault(); apply([], ''); return; }
    // Show more: apply the URL the SERVER computed, verbatim. No arithmetic here,
    // so the browser cannot disagree with the server about the page size — and
    // the limit lands in #tasks-list's hx-get, which the htmx:configRequest
    // listener above then applies to the next sse:task.changed refetch so the
    // expansion survives it instead of collapsing. e2e/tests/
    // task-board-paging.spec.ts is the regression test for exactly that; before
    // it existed, deleting this branch outright survived the whole Go suite.
    var more = t.closest('[data-tasks-more]');
    if (more) {
      e.preventDefault();
      more.disabled = true;
      refreshTo(more.getAttribute('data-tasks-more'));
      return;
    }
    // Status: composed with whatever tags are selected, never replacing
    // them. The All chip carries an empty value, which is the same "no status
    // predicate" the server reads.
    var st = t.closest('[data-status-filter]');
    if (st) { e.preventDefault(); apply(get(), st.getAttribute('data-status-filter')); return; }
    var chip = t.closest('[data-tag-filter]');
    if (!chip) return;
    e.preventDefault();
    var tag = chip.getAttribute('data-tag-filter');
    apply(toggleFilter(get(), tag, chip.getAttribute('data-tag-filter-exclusive')), statusOf());
  });

  // --- editor: type + Enter/comma → chip -------------------------------------
  function chipFor(tag) {
    var span = document.createElement('span');
    span.setAttribute('data-tag-chip', tag);
    span.setAttribute('data-tag-kind', tag.indexOf(':') > 0 ? 'routing' : 'descriptive');
    span.className = 'inline-flex shrink-0 items-center gap-1.5 rounded-md bg-s2 px-1.5 py-1 text-xs text-muted ring-1 ring-inset ring-line';
    span.appendChild(document.createTextNode(tag));
    var hid = document.createElement('input');
    hid.type = 'hidden'; hid.name = 'tag'; hid.value = tag;
    span.appendChild(hid);
    var rm = document.createElement('button');
    rm.type = 'button';
    rm.setAttribute('data-tag-remove', '');
    rm.setAttribute('aria-label', 'Remove tag ' + tag);
    rm.className = 'press -mr-0.5 inline-flex h-4 w-4 items-center justify-center rounded-full text-current opacity-60 transition hover:opacity-100';
    rm.textContent = '✕';
    span.appendChild(rm);
    return span;
  }
` + tagNormalizeJS + `
  function addTag(editor, raw) {
    var tag = normalize(raw);
    if (!tag) return false;
    var box = editor.querySelector('[data-tag-chips]');
    if (!box) return false;
    if (box.querySelector('[data-tag-chip="' + tag.replace(/"/g, '\\"') + '"]')) return true;
    box.appendChild(chipFor(tag));
    try { window.cgTrack('tag.added', { routing: tag.indexOf(':') > 0 ? '1' : '0' }); } catch (e) {}
    return true;
  }
  document.addEventListener('keydown', function (e) {
    var inp = e.target;
    if (!inp || !inp.matches || !inp.matches('[data-tag-input]')) return;
    if (e.key !== 'Enter' && e.key !== ',') return;
    // Enter inside a form would SUBMIT it; a tag input must only add a chip.
    e.preventDefault();
    var editor = inp.closest('[data-tag-editor]');
    if (editor && addTag(editor, inp.value)) inp.value = '';
  });
  // Blur commits a typed-but-unconfirmed tag, so a user who types and taps Save
  // doesn't silently lose it.
  document.addEventListener('blur', function (e) {
    var inp = e.target;
    if (!inp || !inp.matches || !inp.matches('[data-tag-input]')) return;
    var editor = inp.closest('[data-tag-editor]');
    if (editor && inp.value && addTag(editor, inp.value)) inp.value = '';
  }, true);
  document.addEventListener('click', function (e) {
    var t = e.target;
    if (!t || !t.closest) return;
    var rm = t.closest('[data-tag-remove]');
    if (!rm) return;
    e.preventDefault();
    var chip = rm.closest('[data-tag-chip]');
    if (chip && chip.parentNode) chip.parentNode.removeChild(chip);
  });
  } // end firstRun

  // On (re)load, point #tasks-list at the persisted filter and, if one is set,
  // re-request so a reloaded page opens filtered rather than showing everything.
  function boot() {
    var el = syncTarget();
    if (el && get().length) refresh();
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot);
  else boot();
})();
`))
}

// taskHashScript UPGRADES a legacy `/tasks#task-<id>` fragment to the real page.
//
// 🔴 IT USED TO BE A SCROLLER, AND THE SCROLLER COULD NOT ALWAYS WIN. `GET
// /tasks` serves the document SHELL; the cards arrive in a LATER htmx fetch
// (#tasks-list's hx-trigger="load"), so the browser's own fragment resolution
// had already been abandoned by the time a card existed. This script used to
// retry that: wait for htmx:afterSettle, scroll the card into view, open every
// ancestor <details>, and — when the card was filtered out — clear the tag
// filter once and try again, finally giving up with "Task #N is not on this
// board." for anything archived or genuinely absent.
//
// `/tasks/<id>` is server-rendered and does not care about board state, so that
// entire failure mode is now UNREACHABLE rather than merely handled: redirect
// to the page instead of hunting for the card. The scroll/expand/filter-clear/
// toast machinery, and openAncestorDetails with it, are gone — the cards no
// longer have a disclosure to open at all.
//
// Old `#task-N` links are already out in ClickUp comments, ClickHouse
// activity.events and several docs, which is exactly why this handler survives
// as a redirect rather than being deleted.
//
// Design notes, each one load-bearing:
//
//   - location.replace(), NOT assign(). The board is not a place you meant to
//     stop at, so it must not become a Back-button trap: replace() swaps the
//     history entry instead of adding one, so Back returns to wherever the link
//     was clicked.
//   - THE MATCHER STAYS ANCHORED AND DIGITS-ONLY (/^#task-([0-9]+)$/).
//     `#task-1-notes` or a foreign `#task-list` anchor is somebody else's
//     fragment; claiming it would navigate away from a page the user is on.
//   - hashchange IS WIRED, not just first paint. A same-page fragment change
//     (an in-app link, a pasted #task-N) fires no navigation of its own.
//   - THE LISTENER BINDS ONCE (__cgTaskHashInit) even though the body of the
//     script re-runs: htmx re-executes in-body <script> tags on every hx-boost
//     swap, and a stacked hashchange listener would fire N redirects.
func taskHashScript() g.Node {
	return Script(g.Raw(`
(function () {
  var firstRun = !window.__cgTaskHashInit;
  window.__cgTaskHashInit = true;

  function go() {
    var m = /^#task-([0-9]+)$/.exec(location.hash || '');
    if (!m) return;              // not ours — leave the fragment alone
    location.replace('/tasks/' + m[1]);
  }

  if (firstRun) window.addEventListener('hashchange', go);

  go();
})();
`))
}

// cardMetaNavScript gives the board card's metadata chips back the card's click.
//
// 🔴 THIS IS THE OTHER HALF OF THE F6 FIX, AND WITHOUT IT F6 WAS A TRADE, NOT A
// FIX. The chips (`#id`, the dispatch/provenance/sessions chips, the tag chips
// and the `+N` overflow) sit in a row that noteCard raises above the card-link's
// stretched ::after so their `title` tooltips resolve — `relative z-10
// pointer-events-none [&>*]:pointer-events-auto`, see notes.go. The row is
// pointer-TRANSPARENT so its gaps still hit the overlay, but the chip rectangles
// themselves opt back in, and a chip that hit-tests as itself no longer hits the
// anchor — so the strip's dominant surface stopped opening the task. Tooltips
// are hover-only, so the whole cost landed on touch. (The measurement, its
// fixture and its denominator live in ONE place, on noteCard's `card-meta`
// Class() in notes.go; the figure that used to be restated here was quoted in
// four files and could not be reproduced.)
//
// The chips carry no click behaviour of their OWN — that was the reasoning that
// missed this — but they were part of the CARD's. So route their click to the
// card's link explicitly. Both halves are measured in
// e2e/tests/tasks.spec.ts ("the metadata chips are hoverable, AND a chip tap
// opens the task"); a class assertion could not see either.
//
// 🔴 IT MUST FORWARD THE GESTURE, NOT SYNTHESISE ONE, AND THE FIRST VERSION DID
// NOT. `link.click()` produces an untrusted, UNMODIFIED, button-0 click, and
// input.css already had the reason written down three files over: the stretched
// anchor exists so "keyboard focus, middle-click, ⌘-click and 'open in new tab'
// all work for free — none of which a delegated JS click handler would give".
// This IS that handler, so it owes exactly that debt. Measured on the compiled
// CSS, with the anchor's own gesture as the control:
//
//	chip   CTRL   -> anchor saw ctrlKey:false  (so it navigated THIS tab)
//	anchor CTRL   -> anchor saw ctrlKey:true   [control: instrument works]
//	chip   MIDDLE -> auxclick at the chip; the anchor's click never fired
//	middle-click on chip      -> pages 1 -> 1, url unchanged
//	middle-click on card body -> pages 1 -> 2  [control: a tab DID open]
//
// i.e. the standard triage gesture — middle- or ⌘-click a chip to queue the task
// in a background tab — did nothing on middle-click and DESTROYED THE BOARD on
// ⌘-click, 20px from a card body where both worked. So the click is forwarded
// with its modifiers and button intact, and 'auxclick' is bound as well because
// a middle-click produces no 'click' event at all. Both are asserted
// behaviourally in e2e/tests/tasks.spec.ts ("… opens the task in a new tab, as
// the card body does"), with the card link as the positive control — a listener
// existing is not the claim; the modifier ARRIVING and a second page OPENING is.
//
// Four things it deliberately does NOT do:
//
//   - it never fires when a text selection is open. A selection drag ends in a
//     mouseup that fires `click`, and nothing on the event distinguishes it from
//     a real one. Raising the row is what made the chip text selectable at all
//     (before it, the stretched ::after ate the drag), so this is a cost the
//     raise created: selecting a tag name or a truncated session id to copy it
//     used to leave the board.
//   - it never fires while merge-selection mode is on. `.selecting
//     .card-link::after { pointer-events: none }` makes a card-body click do
//     NOTHING mid-merge, on purpose (see merge.go's setMode); the chips must not
//     become the one surface that still navigates away from a half-made merge.
//   - it never hijacks a real control. Nothing interactive lives in the row
//     today, so the closest() bail is for the row as it GROWS — a chip that
//     becomes a filter link or a button keeps its own behaviour.
//   - it does nothing on the DETAIL card, which renders the same row but no
//     `a.card-link` (you are already on the task's page).
//
// Delegated on the document and bound once (__cgCardMetaNavInit), like every
// other script here: the cards are re-rendered by every list swap, and htmx
// re-executes in-body <script> tags on each hx-boost swap.
func cardMetaNavScript() g.Node {
	return Script(g.Raw(`
(function () {
  if (window.__cgCardMetaNavInit) return;
  window.__cgCardMetaNavInit = true;

  function route(e) {
    var t = e.target;
    if (!t || !t.closest) return;
    var meta = t.closest('.card-meta');
    if (!meta) return;
    if (t.closest('a,button,input,select,textarea,label,[data-no-card-nav]')) return;
    if (document.documentElement.classList.contains('selecting')) return;
    // A text-selection drag ends in a click, and nothing in the event
    // distinguishes it from a real one. Raising the row is what made the chip
    // text selectable in the first place, so selecting a tag name or a
    // truncated session id to copy it must not navigate away.
    var sel = window.getSelection && window.getSelection();
    if (sel && !sel.isCollapsed) return;
    var card = meta.closest('article');
    var link = card && card.querySelector('a.card-link');
    if (!link) return;
    // NOT link.click(): that synthesises an UNMODIFIED, button-0 click, so
    // ⌘/Ctrl/Shift-click navigated this tab (losing the board) and
    // middle-click did nothing at all. Forward the gesture instead — the
    // anchor's own activation behaviour then does what it does for a real
    // click on the card body. Measured: dispatching with ctrlKey, and
    // dispatching with button:1, each open a background tab and leave this
    // one on the board.
    link.dispatchEvent(new MouseEvent('click', {
      bubbles: true,
      cancelable: true,
      view: window,
      ctrlKey: e.ctrlKey,
      metaKey: e.metaKey,
      shiftKey: e.shiftKey,
      altKey: e.altKey,
      button: e.button,
      buttons: e.buttons
    }));
  }

  document.addEventListener('click', route);
  // A middle-click never produces a 'click' event — it is 'auxclick' — so a
  // click-only delegate is dead for the standard open-in-a-background-tab
  // gesture. Button 2 is deliberately NOT routed: the context menu is its own
  // affordance and must stay on the chip.
  document.addEventListener('auxclick', function (e) {
    if (e.button !== 1) return;
    route(e);
  });
})();
`))
}

// pushScript drives the Web Push subscription flow. It is fully
// feature-detected: on a browser without serviceWorker / PushManager /
// Notification it does nothing (and leaves the Enable button hidden).
// Subscribing is gated behind a user gesture on the header Enable button, as
// required for Notification.requestPermission() on modern browsers.
//
// 🔴 IT DOES NOT REGISTER THE SERVICE WORKER. pwaScript (pwa.go) is the one
// registration site and publishes the promise as window.musterSWReady; install,
// update and share all depend on the worker, and none of them should depend on
// the script whose job is notifications. pwaScript must therefore render BEFORE
// this one, which TestPWAScriptRegistersBeforeThePushScript pins.
func pushScript() g.Node {
	return Script(g.Raw(`
(function () {
  var enableBtn = document.getElementById('enable-push');
  var onPill = document.getElementById('push-on');
  var statusEl = document.getElementById('push-status');

  function status(msg, kind) {
    if (!statusEl) return;
    if (!msg) { statusEl.classList.add('hidden'); statusEl.textContent = ''; return; }
    statusEl.textContent = msg;
    statusEl.classList.remove('hidden', 'text-st-error-fg', 'text-st-warning-fg', 'text-accent');
    statusEl.classList.add(kind === 'error' ? 'text-st-error-fg' : (kind === 'ok' ? 'text-accent' : 'text-st-warning-fg'));
  }
  function showEnable() { if (enableBtn) { enableBtn.classList.remove('hidden'); enableBtn.classList.add('inline-flex'); } }
  function hideEnable() { if (enableBtn) { enableBtn.classList.add('hidden'); enableBtn.classList.remove('inline-flex'); } }
  function showOn() { if (onPill) { onPill.classList.remove('hidden'); onPill.classList.add('inline-flex'); } }
  function hideOn() { if (onPill) { onPill.classList.add('hidden'); onPill.classList.remove('inline-flex'); } }

  // applyState is the SINGLE source of truth for the header push affordances:
  // exactly one of the Enable button / "on" pill is ever visible (never both,
  // never neither when state is known). Pass 'on' (subscribed), 'enable'
  // (offer to enable), or 'off' (denied/unsupported — hide both).
  function applyState(s) {
    if (s === 'on') { hideEnable(); showOn(); }
    else if (s === 'enable') { hideOn(); showEnable(); }
    else { hideEnable(); hideOn(); } // 'off'
  }

  // Inside the Android app's WebView the app owns notifications (UnifiedPush,
  // its own settings); the page offers no Enable and says nothing about
  // browser support (docs/design/android.md, server change 24).
  if (window.` + AppBridgeName + `) { status(''); applyState('off'); return; }

  var supported = ('serviceWorker' in navigator) &&
                  ('PushManager' in window) &&
                  ('Notification' in window);
  if (!supported) {
    // 🔴 iOS/iPadOS: Web Push exists ONLY in a Home-Screen-installed app
    // (16.4+). A Safari TAB has no PushManager, so "not supported" would be
    // false and unhelpful there — say what makes it work instead. iPadOS
    // reports itself as a Mac, hence the touch-points check.
    var ios = /iPad|iPhone|iPod/.test(navigator.userAgent) ||
              (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1);
    var installed = (window.matchMedia && window.matchMedia('(display-mode: standalone)').matches) ||
                    navigator.standalone === true;
    if (ios && !installed) {
      status('To get notifications on iPhone or iPad, add muster to your Home Screen (Share → Add to Home Screen) and open it from there.', 'warn');
    } else {
      // Surface the unsupported case instead of silently doing nothing.
      status('Notifications are not supported in this browser.', 'warn');
    }
    applyState('off');
    return;
  }

  function urlBase64ToUint8Array(base64String) {
    var padding = '='.repeat((4 - (base64String.length % 4)) % 4);
    var base64 = (base64String + padding).replace(/-/g, '+').replace(/_/g, '/');
    var raw = atob(base64);
    var out = new Uint8Array(raw.length);
    for (var i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
    return out;
  }

  // Absolute, same-origin request URLs. (This used to justify itself with
  // basic-auth credentials in the page URL — an edge muster no longer sits
  // behind; it is harmless either way, since location.origin never carries
  // userinfo.)
  function apiURL(p) { return location.origin + p; }

  async function getVapidKey() {
    var res = await fetch(apiURL('/api/push/vapid-public-key'), { credentials: 'include' });
    if (res.status === 503) throw new Error('Push is not configured on the server.');
    if (!res.ok) throw new Error('Could not load the notification key (' + res.status + ').');
    var data = await res.json();
    if (!data.key) throw new Error('Server returned an empty notification key.');
    return data.key;
  }

  async function postSubscription(sub) {
    var res = await fetch(apiURL('/api/push/subscribe'), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(sub),
      credentials: 'include',
    });
    if (!res.ok) throw new Error('Saving the subscription failed (' + res.status + ').');
  }

  async function subscribe(reg) {
    var key = await getVapidKey();
    var sub = await reg.pushManager.subscribe({
      userVisibleOnly: true,
      applicationServerKey: urlBase64ToUint8Array(key),
    });
    await postSubscription(sub);
    return sub;
  }

  // Reflect a known-subscribed state in the UI.
  function markSubscribed() { applyState('on'); status('', null); }

  // refresh is the authoritative reconciler: it derives the header state from
  // the LIVE permission + getSubscription() result. It is awaited on load AND
  // re-awaited after a successful subscribe (so the just-enabled button is
  // deterministically hidden the instant the subscription exists, rather than
  // relying on the click handler's optimistic flip). Determinism rule: the
  // Enable button is shown ONLY when permission is granted-able AND there is no
  // active subscription; once a subscription exists, only the "on" pill shows.
  // sameKey reports whether a subscription was made with the server's CURRENT
  // VAPID key. A subscription is bound to the key it was made with, so after a
  // key rotation every device's old subscription is refused by the push
  // service — it must be replaced, not re-POSTed.
  function sameKey(sub, key) {
    var have = sub && sub.options && sub.options.applicationServerKey;
    if (!have) return true; // unknown: leave it (older browsers do not expose it)
    var a = new Uint8Array(have), b = urlBase64ToUint8Array(key);
    if (a.length !== b.length) return false;
    for (var i = 0; i < a.length; i++) if (a[i] !== b[i]) return false;
    return true;
  }

  async function refresh(reg) {
    var existing = await reg.pushManager.getSubscription();
    if (existing) {
      var key = null;
      try { key = await getVapidKey(); } catch (e) {}
      if (key && !sameKey(existing, key)) {
        // Key rotation: drop the stale subscription (here and on the server)
        // and make a new one with the current key. Permission is already
        // granted, so no user gesture is needed.
        var old = existing.endpoint;
        try { await existing.unsubscribe(); } catch (e) {}
        try {
          await fetch(apiURL('/api/push/unsubscribe'), {
            method: 'POST', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ endpoint: old }), credentials: 'include',
          });
        } catch (e) {}
        try { await subscribe(reg); markSubscribed(); return; }
        catch (e) { status(e.message || 'Could not re-enable notifications.', 'error'); applyState('enable'); return; }
      }
      // Re-POST so a restarted/persisted-empty server re-learns this device.
      try { await postSubscription(existing); } catch (e) {}
      markSubscribed();
      return;
    }
    // Granted but no active subscription (e.g. server lost it / SW reinstalled):
    // re-subscribe defensively without a fresh user gesture.
    if (Notification.permission === 'granted') {
      try { await subscribe(reg); markSubscribed(); return; }
      catch (e) { status(e.message || 'Could not re-enable notifications.', 'error'); }
    }
    if (Notification.permission === 'denied') {
      applyState('off');
      status('Notifications are blocked. Enable them in your browser/site settings.', 'warn');
      return;
    }
    applyState('enable');
  }

  var registered = window.musterSWReady;
  if (!registered) {
    status('Service worker failed to register; notifications are unavailable.', 'error');
    applyState('off');
    return;
  }
  registered.then(function (reg) {
    refresh(reg);

    if (enableBtn) {
      enableBtn.addEventListener('click', async function () {
        enableBtn.disabled = true;
        status('', null);
        try {
          var perm = await Notification.requestPermission();
          if (perm === 'denied') {
            applyState('off');
            status('Notifications were blocked. Enable them in your browser/site settings.', 'error');
            return;
          }
          if (perm !== 'granted') {
            // Dismissed without choosing: leave the button so they can retry.
            status('Notification permission was not granted — tap Enable to try again.', 'warn');
            return;
          }
          await subscribe(reg);
          // Re-run the authoritative reconciler now the subscription exists, so
          // the Enable button is deterministically hidden (not just optimistically
          // flipped) and the "on" pill is shown — never both.
          await refresh(reg);
        } catch (e) {
          status((e && e.message) ? e.message : 'Could not enable notifications.', 'error');
        } finally {
          enableBtn.disabled = false;
        }
      });
    }
  }).catch(function (e) {
    status('Service worker failed to register; notifications are unavailable.', 'error');
  });
})();
`))
}
