package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/web"
)

// TaskNotFoundPage is the styled 404 for GET /tasks/{id} when the task is
// dismissed or was never there.
//
// 🔴 IT EXISTS BECAUSE OF THE BACK BUTTON, not because a plain 404 is ugly.
// taskHashScript upgrades every legacy `/tasks#task-N` deeplink — and those are
// out in ClickUp comments, in ClickHouse activity.events and in several docs —
// using location.replace(), which CONSUMES the history entry it arrived on. Go's
// http.NotFound answers with a bare text/plain "404 page not found": no chrome,
// no sidebar, no link anywhere, and a Back press that leaves muster entirely
// because the entry the user came from was replaced. The board's old behaviour
// for the same link was to load and toast "Task #N is not on this board." — this
// page is that, on a URL that legitimately 404s.
//
// It is an OFF-SHELL document like TaskDetailPage, and deliberately a MINIMAL
// one: the sidebar (so every other tab is one tap away), a real <h1> (axe
// page-has-heading-one applies to a 404 too), the explanation, and an unboosted
// link to the board. No htmx surface at all — there is nothing here to mutate,
// so there is nothing to keep live and no failure to toast.
func TaskNotFoundPage(id string, feat Features) g.Node {
	label := "This task"
	if id != "" {
		label = "Task #" + id
	}
	return Doctype(
		HTML(Class("dark"), Lang("en"),
			Head(
				Meta(Charset("utf-8")),
				Meta(Name("viewport"), Content("width=device-width, initial-scale=1, viewport-fit=cover")),
				Meta(Name("color-scheme"), Content("dark light")),
				ThemeHead(),
				manifestLink(),
				TitleEl(g.Text(label+" not found · muster")),
				Link(Rel("stylesheet"), Href(web.Asset("/static/app.css"))),
				Script(Src(web.Asset("/static/vendor/htmx.min.js")), Defer()),
			),
			Body(
				Class("min-h-dvh bg-bg text-fg antialiased selection:bg-accent/30"),
				// The sidebar tabs are ordinary boosted links off this page, exactly as
				// on the detail page (hasPanels=false — there is no panel to toggle).
				hx("hx-boost", "true"),
				sidebar("tasks", "tasks", false, feat),
				Div(
					Class("lg:pl-72"),
					Header(
						Class("sticky top-0 z-20 border-b border-line bg-bg/80 backdrop-blur"),
						Div(
							// Same contentWidth() as the <main> below and as the detail
							// page this is the 404 for — see TaskDetailPage.
							Class(contentWidth()+" flex items-center gap-2 py-2"),
							sidebarOpenButton(),
						),
					),
					Main(
						// contentWidth(): the shell's ONE measure. The sidebar offset is on
						// the wrapper above (lg:pl-72), not on this column, so widening it
						// is safe here — see TestRouteContentWidthRelationship.
						Class(contentWidth()+" pb-[calc(4rem+env(safe-area-inset-bottom))] pt-10"),
						Div(
							g.Attr("data-task-missing", id),
							Class("flex flex-col items-start gap-3 rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line"),
							H1(
								Class("text-base font-semibold text-fg"),
								g.Text(label+" is not on the board"),
							),
							P(
								Class("text-sm leading-relaxed text-muted"),
								g.Text("It was dismissed, or it never existed. Dismissing keeps the row and its "+
									"comment thread, so a task that was dismissed can still be restored — it just "+
									"has no page while it is off the board."),
							),
							// 🔴 hx-boost="false": this is the recovery link on a page a
							// history-replacing redirect can land you on, so it must be a real
							// navigation to the real shell, not a body swap into this document.
							A(
								Href("/tasks"),
								hx("hx-boost", "false"),
								g.Attr("data-task-back", ""),
								Class("press mt-1 inline-flex h-11 min-h-[44px] items-center gap-1.5 rounded-lg bg-s2 px-4 text-sm font-medium text-fg ring-1 ring-inset ring-edge transition hover:bg-s3"),
								g.Raw(`<svg class="h-4 w-4" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 15l-5-5 5-5"/></svg>`),
								g.Text("Back to tasks"),
							),
						),
					),
				),
				// Sidebar open/close.
				appScript(feat),
				// The worker, update toast and app badge — on every document, not
				// just the shell, so an update is offered wherever the operator is.
				pwaChrome(),
			),
		),
	)
}
