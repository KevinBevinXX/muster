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
//
// 🔴 delay:50ms IS A DEBOUNCE, AND THE AGENTS LIST IS WHY IT IS NEEDED. htmx's
// SSE extension dispatches an event on EVERY element that listens for it, and
// each dispatch bubbles to body — so with the Agents list listening for this
// event too (its cards' attention badges), a `from:body` listener here heard
// ONE event twice and fetched the badge twice (measured: attention.spec's
// "badge requests while nothing changed" saw 2). Debounced, the dispatches of
// one event are one fetch, and the fetch still follows the last of them.
const attentionBadgeTrigger = "load, sse:attention_queue.changed from:body delay:50ms, every 60s"

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
		Span(Class("text-base"), g.Text("🔔")), Span(g.Text("Asks")), attentionBadgeSlot())
}

// AskerAsksID is the element id of a session page's asks block.
const AskerAsksID = "asker-asks"

// askerAsksTrigger refreshes the block on the queue's SSE event, debounced as
// the badge is (attentionBadgeTrigger), and once a minute.
const askerAsksTrigger = "sse:attention_queue.changed from:body delay:50ms, every 60s"

// askBusyFunc is THE "the owner is typing an answer" rule, in JavaScript:
// focus in a field of the list, or any non-blank answer box in it. Every live
// refresh of an answer form asks it first, so a refresh never eats a
// half-typed answer (AttentionLiveScript; the agent page's htmx refresh,
// askerAsksBusy).
const askBusyFunc = `function (list) {
    var a = document.activeElement;
    if (a && list.contains(a) && (a.tagName === 'TEXTAREA' || a.tagName === 'INPUT')) return true;
    var fields = list.querySelectorAll('textarea');
    for (var i = 0; i < fields.length; i++) if (fields[i].value.trim() !== '') return true;
    return false;
  }`

// askerAsksBusy cancels the agent page block's refresh while the owner types.
const askerAsksBusy = "if ((" + askBusyFunc + ")(this)) event.preventDefault()"

// AttentionLiveScript keeps the element listID live: on the queue's SSE event
// (event: internal/api's EventAttentionChanged) it re-fetches src into it —
// unless the owner is typing an answer (askBusyFunc), in which case it waits
// until they leave the field. The /attention page's list and a mirrored
// window's asks block use it; each is its own document (no htmx), so it opens
// its own stream.
func AttentionLiveScript(listID, src, event string) string {
	return `<script>
(function () {
  var busy = ` + askBusyFunc + `;
  var list = document.getElementById(` + strconv.Quote(listID) + `);
  if (!list || !window.EventSource) return;
  var src = ` + strconv.Quote(src) + `;
  var pending = false;
  function refresh() {
    // The Asks page's other parts (its tabs' counts) follow the same event.
    try { document.dispatchEvent(new Event('asks:changed')); } catch (e) {}
    if (busy(list)) { pending = true; return; }
    pending = false;
    fetch(src, { credentials: 'same-origin' }).then(function (r) {
      return r.ok ? r.text() : null;
    }).then(function (html) {
      if (html !== null && !busy(list)) list.innerHTML = html; else if (html !== null) pending = true;
    }).catch(function () {});
  }
  list.addEventListener('focusout', function () { if (pending) setTimeout(refresh, 0); });
  var es;
  try { es = new EventSource('/events'); } catch (e) { return; }
  es.addEventListener(` + strconv.Quote(event) + `, refresh);
  setInterval(refresh, 60000);
})();
</script>`
}

// askerAsksEvent is the SSE event the mirrored view's block refreshes on: the
// one askerAsksTrigger names (internal/api's EventAttentionChanged; its
// TestTheBadgeListensForTheEventTheServerSends pins the spelling).
const askerAsksEvent = "attention_queue.changed"

// AskerAsksBlock is a session page's block of its asker's OPEN attention asks:
// inner, the content internal/api renders (the queue's own items and answer
// forms; "" for none, and then the block takes no room), refreshed from src.
// viaHTMX: the page is in the shell, whose htmx SSE refreshes it (the agent
// page); otherwise (the mirrored view, its own document) the page carries
// internal/api's attentionLiveScript, which refreshes it by its id.
//
// Bounded and scrolling on its own (max-h, overflow-y-auto, flex-none), so on
// a phone it never pushes the conversation or the pinned composer off screen.
func AskerAsksBlock(inner, src string, viaHTMX bool) g.Node {
	return Section(
		ID(AskerAsksID),
		g.Attr("aria-label", "Questions waiting on your answer"),
		g.Attr("aria-live", "polite"),
		g.Attr("data-src", src),
		g.If(viaHTMX, g.Group([]g.Node{hx("hx-get", src), hx("hx-trigger", askerAsksTrigger), hx("hx-swap", "innerHTML"),
			hx("hx-target", "this"), g.Attr("hx-on::before-request", askerAsksBusy)})),
		Class("flex max-h-[45vh] min-h-0 flex-none flex-col gap-2 overflow-y-auto border-t border-line px-3 py-2 empty:hidden"),
		g.Raw(inner),
	)
}

// AskerAsksLive is what keeps a block drawn with viaHTMX false live: the
// queue page's own script, on the block (nothing without a block).
func AskerAsksLive(src string) g.Node {
	if src == "" {
		return nil
	}
	return g.Raw(AttentionLiveScript(AskerAsksID, src, askerAsksEvent))
}
