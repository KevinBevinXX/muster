package ui

import (
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/web"
)

// 🔴 THIS DOCUMENT DELIBERATELY STAMPS NOTHING ON ITS REQUESTS.
//
// It used to carry `hx-headers='{"X-Muster-View":"detail"}'` on <body>, and
// internal/api's renderNoteCard branched on that header to pick the card shape.
// The seam leaked: htmx boosts with target=document.body and swap=innerHTML, and
// an innerHTML swap replaces a node's CHILDREN, never its ATTRIBUTES — so any
// boosted navigation off this page (the eight sidebar tabs, boosted by design)
// left the stamp on the live <body> of the BOARD, and every subsequent board
// mutation came back as a DETAIL card morphed into #tasks-list.
//
// The shape is now derived from htmx's own HX-Current-URL, per request, in
// internal/api/view.go — which carries the full account. Two tests hold the
// markup half here: TestNoDocumentsBodyCarriesAnythingABoostedNavCanStrand
// (the <body> attribute ledger) and
// TestDetailPageAttachesNoInheritedRequestStampAnywhere (the same hazard moved
// onto any other ancestor of the card).

// TaskDetailPage renders the standalone GET /tasks/{id} document: the task's
// full body, attachments, session thread, comments and comment form, wrapped in
// the app's shared chrome.
//
// It is an OFF-SHELL document (its own Doctype/HTML/Head/Body), like
// AgentDetailPage and sessionDetailPage — the shell's Page() is not reused,
// because the shell is the multi-panel SPA and this page has no panels.
//
// 🔴 hasPanels=false on the sidebar. With no #panel-* elements in this document
// the sidebar tabs must stay BOOSTED — there is nothing to toggle in place, so
// a boosted cross-document navigation IS the behaviour. TestSidebarTabBoostIs
// OwnedByThePanelOwner checks that against the panels actually rendered.
//
// 🔴 /tasks/{id} is deliberately NOT in tabFromPath. That switch is exact-match
// and only feeds handleIndex, which never serves this document — same as
// /agents/{name} and /suggestions/{id}, neither of which is in it either.
func TaskDetailPage(v TaskCardView, feat Features) g.Node {
	v.Detail = true
	heading := taskDetailHeading(v.Note)
	return Doctype(
		HTML(Class("dark"), Lang("en"),
			Head(
				Meta(Charset("utf-8")),
				Meta(Name("viewport"), Content("width=device-width, initial-scale=1, viewport-fit=cover")),
				Meta(Name("color-scheme"), Content("dark light")),
				ThemeHead(),
				manifestLink(),
				TitleEl(g.Text(heading+" · muster")),
				Link(Rel("stylesheet"), Href(web.Asset("/static/app.css"))),
				Script(Src(web.Asset("/static/vendor/htmx.min.js")), Defer()),
				// idiomorph: the card's own mutation swaps are `morph:outerHTML`, and
				// that is load-bearing rather than cosmetic — a plain outerHTML swap
				// re-creates the nested comment form and drops its htmx submit
				// binding, so the NEXT comment posts as a native form navigation.
				Script(Src(web.Asset("/static/vendor/idiomorph-ext.min.js")), Defer()),
				Script(Src(web.Asset("/static/vendor/sse.js")), Defer()),
				faroHead(),
			),
			Body(
				Class("min-h-dvh bg-bg text-fg antialiased selection:bg-accent/30"),
				// SPA navigation for the sidebar tabs and any other internal link.
				hx("hx-boost", "true"),
				// morph must be ACTIVE on an ancestor of the card, or every
				// `hx-swap="morph:outerHTML"` on it silently degrades.
				hx("hx-ext", "morph"),
				// 🔴 NOTHING ELSE GOES ON <body>. See the file comment: a boosted
				// navigation swaps body.innerHTML and leaves body's own attributes
				// behind on the arrived-at document. bodyAttrLedger pins this set.
				sidebar("tasks", "tasks", false, feat),
				Div(
					Class("lg:pl-72"),
					// Liveness, exactly as the shell wires it: connect to /events on a
					// WRAPPER element (never on <body>, which a boost would strand).
					// Without this the page was write-only — a comment posted by an
					// agent, or a status flipped from another device, never appeared
					// until a manual reload, and this is the page a "New comment on
					// task #N" push opens.
					hx("hx-ext", "sse"),
					hx("sse-connect", "/events"),
					taskDetailHeader(),
					Main(
						// Width comes from contentWidth() — the SAME helper the shell's
						// content column and header row use. Opening a task is the primary
						// navigation path off the board, and carrying the pre-#667 measure
						// here made the column visibly narrow on the way in (~1536px →
						// 1024px) and widen again on the way back. Only the vertical
						// padding is local to this page.
						//
						// Safe to widen because the sidebar offset is on the WRAPPER above
						// (lg:pl-72, a few lines up), exactly as in the shell — unlike
						// /operator and /agents/{name}, whose <main> carries the offset
						// itself and is deliberately excluded. Pinned by
						// TestRouteContentWidthRelationship.
						Class(contentWidth()+" pb-[calc(4rem+env(safe-area-inset-bottom))] pt-4"),
						// The card in DETAIL shape, inside its liveness/timeout envelope.
						// Its title is this document's ONE <h1>; the page adds no heading
						// of its own.
						taskDetailLive(v),
					),
				),
				// The Edit button (hx-get /ui/tasks/{id}/edit → #task-modal-body) and
				// the Dispatch button (→ #agent-modal-body) both target modal shells
				// that must exist in the document, or their swap lands nowhere.
				noteModalShell(),
				agentModalShell(),
				// Transient error toasts for failed background actions.
				actionToasts(),
				// Keep the card's relative timestamp ('10s', '30m') live.
				relTimeScript(),
				// Task modal: dirty-guarded dismiss, save highlight, focus return.
				taskModalScript(),
				// The edit form's type-Enter-to-chip tag input.
				tagScript(),
				// 🔴 THE FAILURE-FEEDBACK LISTENERS LIVE HERE, and they are the only
				// ones in the app: htmx does not swap a non-2xx response, so without
				// resyncScript every failed mutation on this page (status, comment,
				// comment retract, delete, tag edit, dispatch) failed SILENTLY — the
				// <select> kept reading "Complete" for a task that had been dismissed
				// in another tab. It also owns htmx:timeout (the envelope below sets
				// one) and dispatches the `muster:resync` the card listens for on
				// focus / visibility / SSE reconnect.
				resyncScript(),
				// Sidebar open/close + the combobox the edit/dispatch forms use.
				// It defines window.toast, which resyncScript's listeners call — so it
				// must be present, and it is fine for it to load after them.
				appScript(feat),
				// The worker, update toast and app badge — on every document, not
				// just the shell, so an update is offered wherever the operator is.
				pwaChrome(),
			),
		),
	)
}

// taskDetailLive wraps the detail card in the envelope that makes this page's
// mutation surface behave like the board's #tasks-list, which is where all three
// of these were inherited from before /tasks/{id} existed as its own document:
//
//   - hx-request {"timeout":15000} — INHERITED by every request-issuing
//     descendant (htmx resolves hx-request by walking up parentElement and does
//     not honour hx-disinherit), so the status PATCH, the comment POST, the
//     delete and the dispatch/edit GETs are all BOUNDED here exactly as they are
//     on the board. resyncScript's htmx:timeout listener is what makes that
//     bound visible instead of silent.
//   - a re-fetch on sse:task.changed / sse:agent.changed, so a comment posted by
//     an agent, a status flipped from another device or a linked agent going
//     `running` lands on this page without a reload.
//   - a re-fetch on muster:resync, which resyncScript dispatches on focus,
//     visibility regain and SSE reconnect — the case SSE alone misses, because a
//     backgrounded connection can drop and miss events while hidden.
//
// It is a WRAPPER, not attributes on the card: the card is replaced wholesale by
// every mutation's morph:outerHTML swap, and the replacement markup comes from
// noteCard, which (correctly) carries none of this.
//
// hx-target/hx-swap here are inherited too, and harmlessly: every control inside
// the card sets its own, and the inherited pair is the same #task-{id} /
// morph:outerHTML the controls use.
func taskDetailLive(v TaskCardView) g.Node {
	ids := strconv.FormatInt(v.Note.ID, 10)
	return Div(
		g.Attr("data-task-live", ids),
		hx("hx-get", "/ui/tasks/"+ids+"/card"),
		hx("hx-trigger", "sse:task.changed from:body, sse:agent.changed from:body, muster:resync from:body"),
		hx("hx-target", "#task-"+ids),
		hx("hx-swap", "morph:outerHTML"),
		hx("hx-request", `{"timeout":15000}`),
		noteCard(v),
	)
}

// taskDetailHeader is the page's top bar: the hamburger (the sidebar is how you
// leave a detail page) plus an explicit back link to the board.
func taskDetailHeader() g.Node {
	return Header(
		// No offset class here: the sidebar offset is on the wrapper this header
		// is rendered INTO (TaskDetailPage's lg:pl-72 Div), which is where the
		// shell puts it too. (This comment used to claim lg:pl-72 was on this
		// element — it never was.)
		Class("sticky top-0 z-20 border-b border-line bg-bg/80 backdrop-blur"),
		Div(
			// Same contentWidth() as the <main> below, so the back link and the card
			// under it cannot sit at different left edges.
			Class(contentWidth()+" flex items-center gap-2 py-2"),
			sidebarOpenButton(),
			// 🔴 hx-boost="false", and the reason written here used to be BACKWARDS.
			// It read: a boosted trip "would replace this document's <body>
			// wholesale … leaving the arrived-at shell WITHOUT" this page's
			// body-level attributes. The opposite is true, and believing the wrong
			// direction is what shipped the X-Muster-View leak: htmx boosts with
			// swap=innerHTML on document.body, so body's own ATTRIBUTES are exactly
			// what SURVIVES onto the shell.
			//
			// What survives today is benign (hx-boost, hx-ext="morph" — the shell
			// wants both, it just declares morph on an inner div), and the eight
			// sidebar tabs boost across this same seam by design. This link stays
			// unboosted anyway because it is the one link whose whole job is "leave
			// this document": a plain navigation re-parses the real shell, with no
			// inheritance question to answer. bodyAttrLedger keeps that question
			// answerable if someone ever adds a third <body> attribute.
			A(
				Href("/tasks"),
				hx("hx-boost", "false"),
				g.Attr("data-task-back", ""),
				g.Attr("aria-label", "Back to tasks"),
				Class("press inline-flex h-11 min-h-[44px] items-center gap-1.5 rounded-lg px-2 text-sm font-medium text-fg2 transition hover:bg-s2 hover:text-fg"),
				g.Raw(`<svg class="h-4 w-4" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 15l-5-5 5-5"/></svg>`),
				g.Text("Tasks"),
			),
		),
	)
}
