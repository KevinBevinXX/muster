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

// THE FOCUS IS THE CARD'S TITLE (operator ask): a session with a focus is
// headed by what it is working on (cardFocusTitle), and what the card was
// called before — the agent's name, the mirrored window's title with its muted
// session — moves under it as a subtitle (cardSubtitleClass). With no focus,
// or only a recorded failure, the card keeps its old title and has no
// subtitle, and the failure is the footer chip (cardFocus) it always was.
//
// 🔴 THE TITLE IS A BUTTON OUTSIDE THE NAV ANCHOR, IN ITS OWN SLOT ("focus").
// A button inside an <a> is invalid markup and its tap would bubble into the
// navigation, so the focus row sits above the anchor, beside the control
// cluster (it reserves the same pr-28): a tap on the title opens the popover
// and never the card; the rest of the card is the anchor, as before.
//
// 🔴 THE FILTER'S RANKING IS UNTOUCHED BY THIS. agents_filter.go ranks on
// data-rank-title (cardFields.Titles: the names), never on the visible
// heading, so a typed name still ranks its card first while the card is
// headed by its focus; the focus text stays in data-filter, as before.

// cardHasFocusTitle reports whether a card is headed by its focus.
func cardHasFocusTitle(f *FocusView) bool { return f != nil && f.Title != "" }

// cardTitleClass is a card's heading as it was (no focus); cardSubtitleClass
// is that same text demoted under a focus title.
const (
	cardTitleClass    = "mr-auto break-all text-base font-semibold leading-tight text-fg group-hover:text-accent"
	cardSubtitleClass = "mr-auto break-all text-sm font-medium leading-tight text-fg2 group-hover:text-accent"
)

// cardNameClass is the class of a card's own name: its title, or its subtitle
// under a focus title.
func cardNameClass(f *FocusView) string {
	if cardHasFocusTitle(f) {
		return cardSubtitleClass
	}
	return cardTitleClass
}

// cardBodyClass is the nav anchor's box: its top padding moves to the focus
// row when there is one, so the two read as one block.
//
// reserve is cardReserve's: what it keeps clear of the control cluster.
func cardBodyClass(f *FocusView, reserve string) string {
	if cardHasFocusTitle(f) {
		return "flex flex-col gap-3 px-4 pb-4 pt-1 " + reserve
	}
	return "flex flex-col gap-3 p-4 " + reserve
}

// focusChevron is the title's "this opens something" affordance: a down
// chevron, turned when the popover is open (aria-expanded).
const focusChevron = `<svg class="h-4 w-4 shrink-0 text-muted transition group-hover/focus:text-fg2 group-aria-expanded/focus:rotate-180" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m6 9 6 6 6-6"/></svg>`

// cardFocusTitle is a card's focus row: the focus title as the card's heading,
// a button that opens the focus popover, or nothing with no focus title.
//
// It must LOOK pressable, because it is the one heading on the page that does
// not navigate: a chevron, a hover and focus-visible surface, the press
// feedback, a 44px tap target, and aria-haspopup / aria-expanded (kept in step
// with the popover by floatingMenuScript's toggle listener).
//
// A focus title past cardTitleMax is CLAMPED like a long name, through the same
// row (cardTitleRow): the heading shows the clamped words and is followed by
// the same "…" button, which opens the same focus popover — where the whole
// title is (data-focus-title). The aria-label always carries the whole title.
func cardFocusTitle(cardID string, f *FocusView, reserve string) g.Node {
	if !cardHasFocusTitle(f) {
		return g.Text("")
	}
	pop := cardID + "-focus"
	return cardTitleRow("focus", pop, f.Title, "pt-3", reserve,
		func(short string) g.Node {
			return Button(
				Type("button"),
				g.Attr("data-card-focus", ""),
				g.If(f.Problem != "", g.Attr("data-focus-stale", "")),
				g.Attr("popovertarget", pop),
				g.Attr("aria-haspopup", "dialog"),
				g.Attr("aria-controls", pop),
				g.Attr("aria-expanded", "false"),
				g.Attr("aria-label", "Focus: "+f.Title+" — show details"),
				Class("press group/focus -ml-2 inline-flex min-h-[44px] min-w-0 max-w-full items-center gap-1.5 rounded-lg px-2 py-1 text-left text-base font-semibold leading-tight text-fg transition hover:bg-s2 hover:text-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus aria-expanded:bg-s2"),
				Span(Class("min-w-0 break-words"), g.Text(short)),
				g.If(f.Problem != "", Span(g.Attr("data-focus-stale-mark", ""),
					Class("shrink-0 rounded-full bg-s2 px-1.5 py-0.5 text-[11px] font-medium text-muted ring-1 ring-inset ring-line"),
					g.Text("stale"))),
				g.Raw(focusChevron),
			)
		},
		focusPopover(pop, f))
}

// cardFocus is the card footer's focus chip for a session with NO focus but a
// recorded failure: "focus unavailable", and — on a tap — a popover saying why.
// A session with a focus is headed by it instead (cardFocusTitle), and a
// session with neither shows nothing.
//
// 🔴 THE SAME NATIVE-POPOVER PATTERN AS THE ⋯ MENU AND THE MIRRORED MARK
// (agentMenu, mirroredMark): a button with popovertarget, a popover="auto"
// panel in the TOP LAYER (no card overflow can clip it; an outside tap or
// Escape closes it), placed by floatingMenuScript ([data-floating-menu]) and
// `invisible` until placed. It sits in the FOOTER, outside the card's nav
// anchor, so a tap opens the popover and never the card; and it is a 44px tap
// target, because a phone has no hover to show a tooltip with.
func cardFocus(cardID string, f *FocusView) g.Node {
	if f == nil || f.Title != "" || f.Problem == "" {
		return g.Text("")
	}
	pop := cardID + "-focus"
	return g.Group{
		Button(
			Type("button"),
			g.Attr("data-card-focus", ""),
			g.Attr("data-focus-stale", ""),
			g.Attr("popovertarget", pop),
			g.Attr("aria-haspopup", "dialog"),
			g.Attr("aria-controls", pop),
			g.Attr("aria-expanded", "false"),
			g.Attr("aria-label", "Focus unavailable: "+f.Problem),
			Class("press inline-flex min-h-[44px] min-w-0 max-w-full items-center gap-1 rounded-full px-2 text-left text-xs font-medium text-fg2 transition hover:bg-s2"),
			Span(Class("shrink-0 text-muted"), g.Text("focus")),
			Span(Class("truncate"), g.Text("unavailable")),
		),
		focusPopover(pop, f),
	}
}

// focusPopover is the focus's detail panel: the title, the description,
// "updated … ago", and — when the latest refresh failed — why ("No focus: …"
// with no title, "Last refresh failed: …" over a stale one).
func focusPopover(pop string, f *FocusView) g.Node {
	problem := "Last refresh failed: "
	if f.Title == "" {
		problem = "No focus: "
	}
	return Div(
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
	)
}
