package ui

import (
	"io"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/web"
)

// THE SIDE NAV ON EVERY SIGNED-IN PAGE.
//
// The slide-out navigation (sidebar) is drawn by ONE function, and every
// document a signed-in person reads carries it: the shell, the detail pages,
// the mirrored views, the terminal — and the server-authored pages internal/api
// writes as strings (Attention, Account, API tokens, Credentials, Passkeys,
// Plugins, Browser flows, the admin pages, a notice shown to someone signed
// in), which reach it through NavDocument below rather than through a second
// copy of the sidebar.
//
// What stays BARE is enumerated, each with its reason, by the two ledgers that
// pin this: TestEveryDocumentIsNavOrNamedBare (this package) and
// TestEveryAPIDocumentIsNavOrNamedBare (internal/api). A document added to
// either side without a decision fails there.
//
// 🔴 A VIEW IN THE VIEW-SWAP FRAME DRAWS ITS NAV TOO. view_swap.go lays the
// frame over the WHOLE page (position:fixed, inset 0, the top z-index), so the
// page underneath — and its nav — cannot be reached while the other view is on
// screen; the frame's own nav is the only one there is. A link in it leaves
// the window: the frame's viewFrameScript hands every link that leaves the
// view (a boosted one included) to the TOP window. The two documents run their
// own appScript. INPUT is not doubled — keys, clicks and touches reach only
// the document on screen (the frame while it is shown, the page under it
// otherwise). What DOES cross documents is same-origin storage and time: a
// `storage` event fires in the other document (the theme listens for it on
// purpose, so the page under the frame follows a theme set in it), and each
// document runs its own timers — the badge's minute refresh runs in both while
// a frame is open, one extra small request a minute, accepted.
//
// 🔴 THE NAV OPENS NO STREAM. The Attention badge refreshes on load and once a
// minute (attentionBadgeTrigger); it also listens for the queue's SSE event,
// which it hears only on a document that already holds an /events stream for
// its own reasons (the shell, a task). A document that had no stream before
// the nav does not get one from it — TestTheNavAddsNoStream.

// The nav entries that are not tabs: each names a sidebar link a document can
// mark as the one it belongs to (sidebar's activeTab / currentTab).
const (
	NavAttention = "attention"
	NavAccount   = "account"
	NavAdmin     = "admin"
)

// NavChrome is what a page outside the shell needs to draw the navigation:
// which optional entries exist.
type NavChrome struct {
	Feat Features
}

// The four pieces a full-height view (the mirrored Rendered view, the
// terminal) places itself — its layout is its own, so it cannot be wrapped.
// Split by WHERE each goes: htmx in the head (the badge's refresh), the
// sidebar (first in <body>), the hamburger (in the page's own header), and the
// open/close script (last in <body>).

func overlayNavHead() g.Node {
	return Script(Src(web.Asset("/static/vendor/htmx.min.js")), Defer())
}

func overlayNav(n NavChrome, entry string) g.Node {
	return sidebarIn(entry, entry, false, n.Feat, true)
}

func overlayNavButton() g.Node { return overlayOpenButton() }

func overlayNavScript(n NavChrome) g.Node { return appScript(n.Feat) }

// NavDocumentView is a server-authored page drawn inside the navigation.
type NavDocumentView struct {
	// Title is the page's name; the document's <title> is "<Title> · muster".
	Title string
	// Entry is the nav entry the page belongs to (a tab key, NavAttention,
	// NavAccount, NavAdmin), or "" for a page with none (Plugins, Browser
	// flows, a notice): no link is then highlighted or marked current.
	Entry string
	Feat  Features
	// Main is the page's content: TRUSTED markup, built by its caller with
	// every value escaped there. It is placed as it is.
	Main string
	// Secret: the page shows a one-time secret (a minted token, a reset or
	// invite link) or takes one (an OAuth token or API key typed in, a passkey
	// ceremony). It loads no telemetry script — these pages never did, and a
	// RUM script on the page a secret is rendered or typed in is a reader of it
	// nobody needs. Every other page keeps the shell's (faroHead).
	Secret bool
}

// NavDocument is the document every server-authored signed-in page is: the
// navigation, the shell's header, and the page's own content in the column the
// bare pages use (max-w-sm), so the content reads as it did before.
//
// 🔴 <body> CARRIES NO hx-boost, unlike the shell's. These pages are made of
// plain forms — sign out, mint a token, revoke a device — whose POST answers
// with a redirect or a whole document; boosted, each would become an AJAX body
// swap of a page that was never written for one. The sidebar's links are
// therefore ordinary navigations here, as their own hx-boost="false" links
// already were.
func NavDocument(v NavDocumentView) g.Node {
	return Doctype(
		HTML(Class("dark"), Lang("en"),
			Head(
				Meta(Charset("utf-8")),
				Meta(Name("viewport"), Content("width=device-width, initial-scale=1, viewport-fit=cover")),
				Meta(Name("color-scheme"), Content("dark light")),
				ThemeHead(),
				Meta(Name("robots"), Content("noindex")),
				manifestLink(),
				TitleEl(g.Text(v.Title+" · muster")),
				Link(Rel("stylesheet"), Href(web.Asset("/static/app.css"))),
				Script(Src(web.Asset("/static/vendor/htmx.min.js")), Defer()),
				g.If(!v.Secret, faroHead()),
			),
			Body(
				Class("min-h-dvh bg-bg text-fg antialiased selection:bg-accent/30"),
				sidebarIn(v.Entry, v.Entry, false, v.Feat, false),
				Div(
					Class("lg:pl-72"),
					// The shell's own header (hamburger + wordmark), without the push
					// affordances: their script is the shell's, not this page's.
					header(false),
					Main(
						g.Attr("data-nav-document", ""),
						Class("mx-auto w-full max-w-sm px-4 pb-[calc(4rem+env(safe-area-inset-bottom))] pt-6"),
						g.Raw(v.Main),
					),
				),
				// Sidebar open/close (and the router, which off the shell only
				// navigates).
				appScript(v.Feat),
				// The worker, update toast and app badge — on every document.
				pwaChrome(),
			),
		),
	)
}

// RenderNavDocument writes NavDocument.
func RenderNavDocument(w io.Writer, v NavDocumentView) error {
	return NavDocument(v).Render(w)
}
