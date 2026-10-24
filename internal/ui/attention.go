package ui

import (
	"html"
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
// never a body swap of the shell. active/current mark it as the entry the
// page belongs to (the /attention page itself): see sidebar.
func attentionLink(feat Features, active, current bool) g.Node {
	if !feat.Attention {
		return nil
	}
	return A(ID("sidebar-attention"), Href("/attention"), hx("hx-boost", "false"), g.If(current, g.Attr("aria-current", "page")),
		Class("press flex min-h-[44px] w-full items-center gap-3 rounded-lg px-3 text-left text-sm font-medium transition "+navEntryTone(active)),
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
//
// It carries "Hide until next ask" (AskerAsksHeading) and, after it, the chip
// that reopens it (askerAsksChip) — see HIDE UNTIL NEXT ASK below.
func AskerAsksBlock(inner, src string, viaHTMX bool) g.Node {
	return g.Group{
		Section(
			ID(AskerAsksID),
			g.Attr("aria-label", "Questions waiting on your answer"),
			g.Attr("aria-live", "polite"),
			g.Attr("data-src", src),
			g.If(viaHTMX, g.Group([]g.Node{hx("hx-get", src), hx("hx-trigger", askerAsksTrigger), hx("hx-swap", "innerHTML"),
				hx("hx-target", "this"), g.Attr("hx-on::before-request", askerAsksBusy)})),
			Class("flex max-h-[45vh] min-h-0 flex-none flex-col gap-2 overflow-y-auto border-t border-line px-3 py-2 empty:hidden"),
			g.Raw(inner),
		),
		askerAsksChip(),
		askerAsksHideScript(),
	}
}

// HIDE UNTIL NEXT ASK (operator decision): the block's heading carries a
// button that hides the block IN THIS VIEW and nothing else. The asks stay
// open in the Asks queue (/attention); nothing is posted — not to the queue's
// answer or dismiss route, not to the session — and no attention item is
// cancelled. A small "N waiting" chip takes the block's place and reopens it.
//
// 🔴 IT IS PER ASK ID, AND A NEW ASK BRINGS THE BLOCK BACK. The hide records the
// ids of the asks it hid (their items' element ids, "ask-<id>") in this
// device's localStorage; the block stays hidden only while EVERY ask it holds
// is one of those. A refresh that adds an ask with an id not hidden shows the
// block again on its own; one that only drops answered asks keeps it hidden.
// Reopening by the chip forgets the ids it shows, so the next load shows it too.
// Storage that throws keeps the hide for this page only. Every write re-reads
// the stored list first (another tab's hides are merged, never overwritten),
// and a write in another tab applies here at once (the storage event).
//
// 🔴 APPLIED BEFORE THE BLOCK IS PAINTED. The script runs as the block is
// parsed (it follows it) and again on every refresh of its content (a
// MutationObserver, whose callback runs before the next paint), so a page
// that loads with its asks hidden never shows them first.

// askerAsksHideLabel is the hide button's text.
const askerAsksHideLabel = "Hide until next ask"

// askerAsksHiddenKey is where the hidden ask ids are kept (localStorage).
const askerAsksHiddenKey = "muster.asks.hidden.v1"

// AskerAsksHeading is the block's heading row: the count (n, "1 question") and
// the hide button. internal/api's askerAsksHTML draws it above the items.
func AskerAsksHeading(n string) string {
	return `<div class="flex min-w-0 items-center gap-2"><h2 class="min-w-0 flex-1 break-words text-xs font-semibold uppercase tracking-wide text-fg2" data-asker-asks-heading>Waiting on your answer: ` +
		html.EscapeString(n) + `</h2><button type="button" data-asker-asks-hide class="press inline-flex min-h-[44px] shrink-0 items-center rounded-lg px-3 text-xs font-medium text-fg2 ring-1 ring-inset ring-line transition hover:bg-s2 hover:text-fg">` +
		askerAsksHideLabel + `</button></div>`
}

// askerAsksChip reopens a hidden block: drawn hidden, shown by
// askerAsksHideScript while the block is hidden, saying how many wait.
func askerAsksChip() g.Node {
	return Button(
		ID(AskerAsksID+"-chip"),
		Type("button"),
		g.Attr("hidden", ""),
		g.Attr("aria-controls", AskerAsksID),
		g.Attr("aria-label", "Show the questions waiting on your answer"),
		// A 44px tap target (layout.spec.ts), drawn as the small pill inside it.
		Class("press group mx-3 inline-flex h-11 flex-none items-center self-start"),
		Span(
			Class("inline-flex h-7 items-center gap-1.5 rounded-full bg-accent/15 px-3 text-xs font-semibold text-accent ring-1 ring-inset ring-accent/40 transition group-hover:bg-accent/25"),
			Span(Class("h-2 w-2 shrink-0 rounded-full bg-accent"), g.Attr("aria-hidden", "true")),
			Span(g.Attr("data-asker-asks-count", "")),
		),
	)
}

// askerAsksHideScript is the hide and the chip (see HIDE UNTIL NEXT ASK).
func askerAsksHideScript() g.Node {
	return Script(g.Raw(`
(function () {
  var block = document.getElementById('` + AskerAsksID + `');
  var chip = document.getElementById('` + AskerAsksID + `-chip');
  // 🔴 THE GUARD IS A PROPERTY, NOT AN ATTRIBUTE: htmx's history snapshot
  // carries attributes, so after Back an attribute guard made this script
  // bail on a block it had never bound — the Hide, the chip and the refresh
  // all dead. A restored block is a new element, without the property.
  if (!block || !chip || block.__musterHideBound) return;
  block.__musterHideBound = true;
  var KEY = '` + askerAsksHiddenKey + `';
  // mem is only the fallback for storage that throws (this page's own hides).
  var mem = [];
  // 🔴 READ AT EVERY USE, NEVER CACHED: another tab may have written since,
  // and a write built on a stale copy dropped its hides.
  function load() {
    try {
      var a = JSON.parse(localStorage.getItem(KEY) || '[]');
      if (Array.isArray(a)) { mem = a; return a.slice(); }
    } catch (e) {}
    return mem.slice();
  }
  // The newest 200 are kept: an id is never reused, so older ones only weigh.
  function save(a) { mem = a.slice(-200); try { localStorage.setItem(KEY, JSON.stringify(mem)); } catch (e) {} }
  function keys() {
    var out = [], items = block.querySelectorAll('[data-attention-item][id]');
    for (var i = 0; i < items.length; i++) out.push(items[i].id);
    return out;
  }
  function apply() {
    var k = keys(), d = load(), hide = k.length > 0;
    for (var i = 0; i < k.length && hide; i++) if (d.indexOf(k[i]) < 0) hide = false;
    block.hidden = hide;
    chip.hidden = !hide;
    if (hide) chip.querySelector('[data-asker-asks-count]').textContent = k.length + ' waiting';
  }
  block.addEventListener('click', function (e) {
    var b = e.target && e.target.closest ? e.target.closest('[data-asker-asks-hide]') : null;
    if (!b) return;
    e.preventDefault();
    var d = load(), k = keys();
    for (var i = 0; i < k.length; i++) if (d.indexOf(k[i]) < 0) d.push(k[i]);
    save(d);
    apply();
    try { chip.focus(); } catch (err) {}
  });
  chip.addEventListener('click', function () {
    var k = keys();
    save(load().filter(function (x) { return k.indexOf(x) < 0; }));
    apply();
    var b = block.querySelector('[data-asker-asks-hide]');
    if (b) try { b.focus(); } catch (err) {}
  });
  if (typeof MutationObserver !== 'undefined') new MutationObserver(apply).observe(block, { childList: true });
  // A hide (or a reopen) in another tab applies here at once.
  window.addEventListener('storage', function (e) { if (e.key === KEY && block.isConnected) apply(); });
  apply();
})();
`))
}

// AskerAsksLive is what keeps a block drawn with viaHTMX false live: the
// queue page's own script, on the block (nothing without a block).
func AskerAsksLive(src string) g.Node {
	if src == "" {
		return nil
	}
	return g.Raw(AttentionLiveScript(AskerAsksID, src, askerAsksEvent))
}
