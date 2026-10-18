package ui

import (
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE ATTENTION QUEUE's navigation entry: a link to /attention (a standalone
// document, rendered by internal/api) with a live count of what is waiting on
// the owner.
//
// 🔴 THE WIRING IS ON THE SLOT, THE ROUTE ANSWERS ONLY ITS CONTENTS. The slot
// (attentionBadgeSlot) carries hx-get/hx-trigger and swaps its INNER html with
// what GET AttentionBadgePath answers — AttentionBadgeHTML, which carries no
// trigger at all. The first version put the wiring on the badge itself and
// swapped it outerHTML: every swap inserted a fresh element whose `load`
// trigger fired again, a request every ~25ms for as long as the page was open
// (measured in the browser, which is how a disabled member's session got
// refused mid-test). TestTheBadgeFragmentCannotRetriggerItself pins it.

// AttentionBadgePath is the badge fragment's route.
const AttentionBadgePath = "/attention/badge"

// attentionBadgeTrigger refreshes the badge on the queue's SSE event (the
// shell's /events stream; internal/api's EventAttentionChanged), on load, and
// once a minute — the backstop on a page whose stream is not connected.
const attentionBadgeTrigger = "load, sse:attention_queue.changed from:body, every 60s"

// AttentionBadgeHTML is the badge for n waiting items: hidden at zero. It is
// the slot's CONTENT and carries no htmx wiring of its own.
func AttentionBadgeHTML(n int) string {
	hidden := ""
	if n <= 0 {
		hidden = " hidden"
	}
	label := strconv.Itoa(n) + " waiting"
	return `<span id="attention-badge" data-testid="attention-badge" data-count="` + strconv.Itoa(n) + `" aria-label="` + label + `"` +
		` class="inline-flex min-w-[1.5rem] items-center justify-center rounded-full bg-accent px-1.5 text-xs font-semibold text-on-accent"` +
		hidden + `>` + strconv.Itoa(n) + `</span>`
}

// attentionBadgeSlot is the element that refreshes the badge.
func attentionBadgeSlot() g.Node {
	return Span(ID("attention-badge-slot"), Class("ml-auto"), hx("hx-get", AttentionBadgePath), hx("hx-trigger", attentionBadgeTrigger),
		hx("hx-swap", "innerHTML"), g.Raw(AttentionBadgeHTML(0)))
}

// attentionLink is the sidebar's entry, drawn only when the queue is served.
// A full navigation (hx-boost="false"): /attention is a standalone document,
// never a body swap of the shell.
func attentionLink(feat Features) g.Node {
	if !feat.Attention {
		return nil
	}
	return A(ID("sidebar-attention"), Href("/attention"), hx("hx-boost", "false"),
		Class("press flex min-h-[44px] w-full items-center gap-3 rounded-lg px-3 text-left text-sm font-medium text-fg2 transition hover:bg-s2 hover:text-fg"),
		Span(Class("text-base"), g.Text("🔔")), Span(g.Text("Attention")), attentionBadgeSlot())
}
