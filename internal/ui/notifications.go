package ui

import (
	"io"
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/textplain"
)

// NotificationRow is one agent's unread-replies entry in the notifications
// panel: the link target session, the agent's name/display name, and the unread
// count.
type NotificationRow struct {
	Name        string // the agent slug (the route key)
	DisplayName string // human-facing label ("" → falls back to Name)
	SessionID   int64  // the session to deep-link to
	Count       int    // unread assistant messages for this agent
}

// ActiveRow is one agent currently streaming (thinking/responding) or active
// within the recency window. Distinct from NotificationRow: it carries a live
// phase + a "since" label instead of an unread count.
type ActiveRow struct {
	Name        string // the agent slug (the route key)
	DisplayName string // human-facing label ("" → falls back to Name)
	SessionID   int64  // the session to deep-link to (0 → link to bare /agents/{name})
	Phase       string // "thinking" | "responding" — the live phase
	Since       string // relative label for recently-active rows (e.g. "2m ago"); "" while in-flight
	InFlight    bool   // true → show the live phase pill; false → show the Since label
}

// RenderNotifications writes the notifications panel partial: an out-of-band FAB
// badge update (the total UNREAD count) followed by the panel body — an ACTIVE
// section (agents thinking/responding now or recently) above the existing UNREAD
// section. This is the GET /ui/notifications response — the FAB hx-gets it into
// #notif-list and the OOB #notif-badge updates the button badge in place.
func RenderNotifications(w io.Writer, active []ActiveRow, unread []NotificationRow) error {
	return notifications(active, unread).Render(w)
}

func notifications(active []ActiveRow, unread []NotificationRow) g.Node {
	return g.Group([]g.Node{
		notifBadge(unreadTotal(unread)),
		notifList(active, unread),
	})
}

// unreadTotal sums the unread counts (the badge meaning).
func unreadTotal(unread []NotificationRow) int {
	total := 0
	for _, it := range unread {
		total += it.Count
	}
	return total
}

// notifBadge is the out-of-band FAB badge. It targets #notif-badge and is hidden
// when there are no unread replies (total == 0).
//
// The badge counts UNREAD replies only — deliberately NOT active agents. Active
// state changes on every stream delta, so folding it into the badge would make
// the count flap during streaming; the badge stays a stable "you have N replies
// to read" signal while the ACTIVE section carries the live indicator.
func notifBadge(total int) g.Node {
	cls := "absolute -right-1 -top-1 inline-flex min-w-[1.25rem] items-center justify-center rounded-full bg-danger px-1.5 py-0.5 text-xs font-semibold tabular-nums text-fg ring-2 ring-bg"
	if total == 0 {
		cls += " hidden"
	}
	return Span(
		ID("notif-badge"),
		hx("hx-swap-oob", "true"),
		Class(cls),
		g.Textf("%d", total),
	)
}

// notifList is the panel body: an ACTIVE section (agents streaming now or
// recently) followed by the UNREAD section (agents with unread replies). Each
// section is labelled and only shown when non-empty. When BOTH are empty a single
// muted "Nothing active." state is shown. Rows are full-width real-load links
// (≥44px tall) so the panel is mobile-friendly.
func notifList(active []ActiveRow, unread []NotificationRow) g.Node {
	if len(active) == 0 && len(unread) == 0 {
		return Div(
			Class("px-4 py-6 text-center text-sm text-muted"),
			g.Text("Nothing active."),
		)
	}
	sections := make([]g.Node, 0, 2)
	if len(active) > 0 {
		rows := make([]g.Node, 0, len(active))
		for _, it := range active {
			rows = append(rows, activeRow(it))
		}
		sections = append(sections,
			notifSectionLabel("Active"),
			Div(Class("divide-y divide-line"), g.Group(rows)),
		)
	}
	if len(unread) > 0 {
		rows := make([]g.Node, 0, len(unread))
		for _, it := range unread {
			rows = append(rows, notifRow(it))
		}
		sections = append(sections,
			notifSectionLabel("Unread"),
			Div(Class("divide-y divide-line"), g.Group(rows)),
		)
	}
	return g.Group(sections)
}

// notifSectionLabel is the small muted heading above a non-empty group.
func notifSectionLabel(text string) g.Node {
	return Div(
		Class("px-4 pt-3 pb-1 text-xs font-semibold uppercase tracking-wide text-muted"),
		g.Text(text),
	)
}

// agentHref builds the detail deep-link for an agent + optional session. A zero
// session id links to the bare detail page, which the server redirects to the
// agent's active session.
//
// ⚠ IT USED TO CARRY AN OPERATOR SPECIAL CASE (/operator, not /agents/operator).
// Task #633 deleted that page, so the panel links every agent the same way.
func agentHref(name string, sessionID int64) string {
	base := "/agents/" + name
	if sessionID > 0 {
		return base + "?session=" + strconv.FormatInt(sessionID, 10)
	}
	return base
}

func activeRow(it ActiveRow) g.Node {
	label := textplain.Title(it.DisplayName)
	if label == "" {
		label = it.Name
	}
	return A(
		Href(agentHref(it.Name, it.SessionID)),
		Class("flex min-h-[44px] items-center justify-between gap-3 px-4 py-2.5 text-sm text-fg transition-colors hover:bg-s2/60"),
		Span(Class("truncate"), g.Text(label)),
		activePill(it),
	)
}

// activePill is the right-hand indicator on an active row: a live phase pill
// ("● thinking…" / "● responding") while in-flight, else a muted relative time.
func activePill(it ActiveRow) g.Node {
	if it.InFlight {
		text := "responding"
		if it.Phase == "thinking" {
			text = "thinking…"
		}
		return Span(
			Class("inline-flex items-center gap-1 rounded-full bg-accent/15 px-2 py-0.5 text-xs font-medium text-accent ring-1 ring-inset ring-accent/30"),
			Span(Class("text-accent"), g.Text("●")),
			g.Text(text),
		)
	}
	since := it.Since
	if since == "" {
		since = "just now"
	}
	return Span(
		Class("inline-flex items-center text-xs text-muted tabular-nums"),
		g.Text(since),
	)
}

func notifRow(it NotificationRow) g.Node {
	label := textplain.Title(it.DisplayName)
	if label == "" {
		label = it.Name
	}
	// The operator lives at /operator (not /agents/operator); everything else at
	// /agents/{name}. Boosted SPA nav: the detail/operator page is now a full shell
	// document and this row is NOT inside #agents-list, so a boosted body-swap
	// re-renders the transcript + agentChatScript's boost-safe init reconnects the
	// WS for the picked session.
	return A(
		Href(agentHref(it.Name, it.SessionID)),
		Class("flex min-h-[44px] items-center justify-between gap-3 px-4 py-2.5 text-sm text-fg transition-colors hover:bg-s2/60"),
		Span(Class("truncate"), g.Text(label)),
		Span(
			Class("inline-flex min-w-[1.5rem] items-center justify-center rounded-full bg-st-error-bg px-2 py-0.5 text-xs font-semibold tabular-nums text-st-error-fg ring-1 ring-inset ring-st-error-fg/40"),
			g.Textf("%d", it.Count),
		),
	)
}
