package ui

import (
	"time"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// FocusView is a session's FOCUS (internal/focus) as a card shows it: what the
// session is working on, and when that was last written. A card with none
// shows nothing.
type FocusView struct {
	Title       string
	Description string
	UpdatedAt   time.Time
	// Problem is why the focus is missing (Title "") or stale (the latest
	// refresh failed after it was written): a short, non-secret reason
	// (focus.Reason), at ProblemAt. "" when the focus is current.
	Problem   string
	ProblemAt time.Time
}

// focusHaystack is what the list's filter finds a card by through its focus:
// the title and the description (agents_filter.go's data-filter).
func focusHaystack(f *FocusView) []string {
	if f == nil {
		return nil
	}
	return []string{f.Title, f.Description}
}

// cardFocus is the card footer's focus chip: the title, one line, and — on a
// tap — a popover with the title, the description and "updated … ago".
//
// 🔴 THE SAME NATIVE-POPOVER PATTERN AS THE ⋯ MENU AND THE MIRRORED MARK
// (agentMenu, mirroredMark): a button with popovertarget, a popover="auto"
// panel in the TOP LAYER (no card overflow can clip it; an outside tap or
// Escape closes it), placed by floatingMenuScript ([data-floating-menu]) and
// `invisible` until placed. It sits in the FOOTER, outside the card's nav
// anchor, so a tap opens the popover and never the card; and it is a 44px tap
// target, because a phone has no hover to show a tooltip with.
//
// With no focus but a recorded failure the chip reads "focus unavailable" and
// the popover says why; with a focus whose latest refresh failed, the popover
// adds that failure — so a missing or stale focus is never silent.
func cardFocus(cardID string, f *FocusView) g.Node {
	if f == nil || (f.Title == "" && f.Problem == "") {
		return g.Text("")
	}
	pop := cardID + "-focus"
	label, chipText := "Focus: "+f.Title, f.Title
	if f.Title == "" {
		label, chipText = "Focus unavailable: "+f.Problem, "unavailable"
	}
	problem := "Last refresh failed: "
	if f.Title == "" {
		problem = "No focus: "
	}
	return g.Group{
		Button(
			Type("button"),
			g.Attr("data-card-focus", ""),
			g.If(f.Problem != "", g.Attr("data-focus-stale", "")),
			g.Attr("popovertarget", pop),
			g.Attr("aria-label", label),
			Class("press inline-flex min-h-[44px] min-w-0 max-w-full items-center gap-1 rounded-full px-2 text-left text-xs font-medium text-fg2 transition hover:bg-s2"),
			Span(Class("shrink-0 text-muted"), g.Text("focus")),
			Span(Class("truncate"), g.Text(chipText)),
		),
		Div(
			ID(pop),
			g.Attr("popover", "auto"),
			g.Attr("role", "dialog"),
			g.Attr("aria-label", "Focus"),
			g.Attr("data-floating-menu", ""),
			g.Attr("data-focus-popover", ""),
			Class("invisible hidden [&:popover-open]:flex m-0 w-72 max-w-[calc(100vw-16px)] flex-col gap-1.5 rounded-xl border border-line bg-s1 p-3 text-fg shadow-2xl ring-1 ring-black/20"),
			g.If(f.Title != "", Span(g.Attr("data-focus-title", ""), Class("break-words text-sm font-semibold"), g.Text(f.Title))),
			g.If(f.Description != "", P(g.Attr("data-focus-description", ""), Class("break-words text-sm text-fg2"), g.Text(f.Description))),
			g.If(!f.UpdatedAt.IsZero(), Span(
				g.Attr("data-focus-updated", ""),
				Class("inline-flex items-center gap-1 text-xs text-muted"),
				g.Text("updated"), cardTime(f.UpdatedAt), g.Text("ago"),
			)),
			g.If(f.Problem != "", P(
				g.Attr("data-focus-problem", ""),
				Class("break-words text-xs text-muted"),
				g.Text(problem+f.Problem),
				g.If(!f.ProblemAt.IsZero(), Span(Class("inline-flex items-center gap-1"), g.Text(" ·"), cardTime(f.ProblemAt), g.Text("ago"))),
			)),
		),
	}
}
