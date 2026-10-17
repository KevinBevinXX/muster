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
// 🔴 THE BADGE IS ONE FRAGMENT WITH ONE RENDERER. The sidebar draws it and GET
// AttentionBadgePath answers it, both through AttentionBadgeHTML, and the
// fragment carries its own hx-get/hx-trigger — so the swap that replaces it
// (outerHTML) puts back an element that keeps listening. A second spelling of
// the attributes would let the first refresh strip them and leave a badge that
// never moves again.

// AttentionBadgePath is the badge fragment's route.
const AttentionBadgePath = "/attention/badge"

// attentionBadgeTrigger refreshes the badge on the queue's SSE event (the
// shell's /events stream; internal/api's EventAttentionChanged), on load, and
// once a minute — the backstop on a page whose stream is not connected.
const attentionBadgeTrigger = "load, sse:attention_queue.changed from:body, every 60s"

// AttentionBadgeHTML is the badge for n waiting items: hidden at zero.
func AttentionBadgeHTML(n int) string {
	hidden := ""
	if n <= 0 {
		hidden = " hidden"
	}
	label := strconv.Itoa(n) + " waiting"
	return `<span id="attention-badge" data-testid="attention-badge" data-count="` + strconv.Itoa(n) + `" hx-get="` + AttentionBadgePath +
		`" hx-trigger="` + attentionBadgeTrigger + `" hx-swap="outerHTML" aria-label="` + label + `"` +
		` class="ml-auto inline-flex min-w-[1.5rem] items-center justify-center rounded-full bg-accent px-1.5 text-xs font-semibold text-on-accent"` +
		hidden + `>` + strconv.Itoa(n) + `</span>`
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
		Span(Class("text-base"), g.Text("🔔")), Span(g.Text("Attention")), g.Raw(AttentionBadgeHTML(0)))
}
