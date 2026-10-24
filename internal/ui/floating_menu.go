package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// floatingMenuScript places every [data-floating-menu] popover next to the
// button that opens it (its [popovertarget]). The popover itself — top layer,
// outside-tap and Escape dismissal — is the browser's; this only decides WHERE.
//
// The rule, in order:
//   - right-aligned to the trigger, just below it;
//   - flipped ABOVE the trigger when below would cross the viewport bottom;
//   - when NEITHER side holds it whole, put on the ROOMIER side and capped to
//     that side's height (it scrolls inside), so it never covers its own
//     trigger — the agent header's full-title popover on a landscape phone
//     (844×390) is taller than the space on either side of its "…". Only when
//     both sides are under minSide (a trigger scrolled to an edge) is it
//     pinned to the bottom edge as before;
//   - clamped horizontally inside an 8px edge. EXERCISED by the agent header's
//     "…" (agentTitle): at 390px it sits mid-row, and right-aligning the 288px
//     panel to it would start left of the screen
//     (e2e/tests/agent-long-title.spec.ts asserts the panel's box is inside
//     the viewport).
//
// The height cap is THIS function's (maxHeight, inline); a popover carries no
// max-height class of its own, which this inline style would silently beat.
//
// 🔴 IT RE-PLACES WHENEVER THE MENU CAN HAVE MOVED, GROWN, OR LOST ITS POSITION,
// NOT ONLY ON OPEN. Three separate causes, each with its own observer because
// no one of them sees the others:
//
//   - IT GREW. Opening the nested "Delete" <details> makes the menu taller. A
//     ResizeObserver re-places it — NOT the <details>' toggle event, which is
//     dispatched a task after the synchronous growth; a probe sampling between
//     the two saw the grown menu hanging over its trigger. ResizeObserver
//     callbacks run in the rendering steps before paint.
//   - THE TRIGGER MOVED. Scroll and resize.
//   - A MORPH STRIPPED THE POSITION. #agents-list morphs in place on every
//     lifecycle and chat event, and the server orders cards by last activity, so
//     a reply RE-ORDERS them. (While the "N new ↑" pill holds the order — the
//     usual case for a background refresh — the morph is handed the cards in
//     the DRAWN order (agentsFilterScript's keepDrawnOrder) and is not
//     re-ranked, and held arrivals sort last, so the held refresh itself moves
//     no drawn card. (It can still HIDE one whose new text stops matching a
//     typed filter — an open gap.) A
//     re-order that is applied still moves cards: by moveBefore where the
//     browser has it, which keeps the popover open; by appendChild where it
//     does not, which CLOSES it.) An open popover stays open through a
//     moveBefore move (its state is not an attribute), but when idiomorph
//     moves its card it rewrites the
//     element's attributes to the server's — which carry no inline style — and the
//     menu fell to the popover default corner (measured: 0,0) with Start/Delete
//     still live, nowhere near its trigger. Its SIZE does not change, so the
//     ResizeObserver cannot see it. A MutationObserver on the open menu's `style`
//     attribute does, and runs as a microtask — before paint — so it is re-placed
//     beside the trigger's NEW position. It is re-placed rather than closed:
//     these morphs are fired by background events, and a menu that shuts under
//     the operator's finger because some other agent replied is worse than one
//     that follows its card.
//     ⚠ What the morph still does: it closes the nested Delete step (the
//     server's markup has no `open`), so an armed "Confirm delete" drops back to
//     step 1. That is the safe direction and is left alone.
//
// 🔴 AND IT IS INVISIBLE UNTIL PLACED. A popover opens synchronously on the
// click, but its `toggle` event — where it is first placed — is a later task,
// and a rendering update runs in between: measured, a requestAnimationFrame
// callback saw the menu open and drawn at 0,0 before it jumped to its trigger,
// on every open. `beforetoggle` cannot place it (the menu has no box to measure
// until it is shown), so the markup carries `invisible` and place() is the only
// thing that makes it visible; closing drops the inline style again.
//
// A trigger that DECLARES aria-expanded (the focus title, cardFocusTitle) is
// kept in step with its popover on every toggle, open and close, whatever
// closed it (a tap outside, Escape, a second tap) — AND after every htmx swap:
// #agents-list's morph rewrites the trigger's attributes to the server's
// aria-expanded="false" while its popover stays open, and no toggle fires, so
// the chevron and highlight (both keyed on aria-expanded) would drop under an
// open popover. A trigger without the attribute is left without it.
//
// A request started from inside a menu closes it as the request STARTS, so a
// request that fails (and morphs nothing) does not leave it open. Closing — by
// any route — also closes the nested Delete step, so reopening starts at step 1.
//
// A page without the Popover API can still use the placement: it marks its
// open menu data-menu-open, its trigger data-menu-trigger="<menu id>", and
// calls window.__musterPlaceMenu(menu) (quickKeyBar's fallback does).
//
// Bound once per document (__musterFloatingMenu), like every other script here:
// htmx re-executes in-body <script> tags on each boosted swap.
func floatingMenuScript() g.Node {
	return Script(g.Raw(`
(function () {
  if (window.__musterFloatingMenu) return;
  window.__musterFloatingMenu = true;
  var GAP = 4, EDGE = 8, MIN_SIDE = 120;
  // data-menu-open: a menu a page opened WITHOUT the Popover API (its own
  // fallback, quickKeyBar's on iOS < 17), placed by the same rule.
  function isOpen(m) {
    if (m.hasAttribute('data-menu-open')) return true;
    try { return m.matches(':popover-open'); } catch (e) { return false; }
  }
  function place(m) {
    if (!m || !m.id || !isOpen(m)) return;
    var t = document.querySelector('[popovertarget="' + m.id + '"], [data-menu-trigger="' + m.id + '"]');
    if (!t) return;
    var r = t.getBoundingClientRect();
    var vw = document.documentElement.clientWidth, vh = window.innerHeight;
    m.style.position = 'fixed';
    m.style.inset = 'auto';
    m.style.margin = '0';
    m.style.maxHeight = (vh - 2 * EDGE) + 'px';
    m.style.overflowY = 'auto';
    var w = m.offsetWidth, h = m.offsetHeight;
    var left = Math.max(EDGE, Math.min(r.right - w, vw - w - EDGE));
    var below = vh - EDGE - (r.bottom + GAP), above = r.top - GAP - EDGE;
    var top;
    if (h <= below) {
      top = r.bottom + GAP;
    } else if (h <= above) {
      top = r.top - GAP - h;
    } else if (Math.max(below, above) >= MIN_SIDE) {
      // Neither side holds it: the roomier side, capped, so it does not cover
      // its trigger.
      m.style.maxHeight = Math.max(below, above) + 'px';
      h = m.offsetHeight;
      top = below >= above ? r.bottom + GAP : r.top - GAP - h;
    } else {
      top = Math.max(EDGE, vh - h - EDGE);
    }
    m.style.left = left + 'px';
    m.style.top = top + 'px';
    // Shown only now. The menu is rendered with the invisible class (agentMenu), so every
    // frame before this line — and every frame after a morph strips this style —
    // draws nothing rather than a menu in the wrong place.
    m.style.visibility = 'visible';
  }
  window.__musterPlaceMenu = place;
  function placeAll() {
    var ms = document.querySelectorAll('[data-floating-menu]');
    for (var i = 0; i < ms.length; i++) place(ms[i]);
  }
  var grew = new ResizeObserver(function (es) {
    for (var i = 0; i < es.length; i++) place(es[i].target);
  });
  // Re-place ONLY when the position is gone. place() itself writes style, which
  // re-enters this observer; a menu that still has its top is this script's own
  // write and is left alone, which is what ends the loop.
  var stripped = new MutationObserver(function (rs) {
    for (var i = 0; i < rs.length; i++) {
      var m = rs[i].target;
      if (isOpen(m) && !m.style.top) place(m);
    }
  });
  // toggle does not bubble; a CAPTURING listener on document sees the popover's
  // own. Open: place it and start observing. Close: stop observing its SIZE,
  // drop the position (so the next open starts invisible again instead of
  // flashing at this one's stale coordinates) and reset the Delete step. The
  // style observer is NOT stopped — MutationObserver has no per-node unobserve —
  // and does nothing for a closed menu (its callback checks isOpen).
  function expanded(m, open) {
    if (!m.id) return;
    var ts = document.querySelectorAll('[popovertarget="' + m.id + '"][aria-expanded]');
    for (var i = 0; i < ts.length; i++) ts[i].setAttribute('aria-expanded', open ? 'true' : 'false');
  }
  document.addEventListener('toggle', function (e) {
    var m = e.target;
    if (!m || !m.matches || !m.matches('[data-floating-menu]')) return;
    expanded(m, isOpen(m));
    if (isOpen(m)) {
      grew.observe(m);
      stripped.observe(m, { attributes: true, attributeFilter: ['style'] });
      place(m);
      return;
    }
    grew.unobserve(m);
    m.removeAttribute('style');
    var ds = m.querySelectorAll('details[open]');
    for (var i = 0; i < ds.length; i++) ds[i].open = false;
  }, true);
  document.addEventListener('htmx:afterSettle', function () {
    var ms = document.querySelectorAll('[data-floating-menu]');
    for (var i = 0; i < ms.length; i++) expanded(ms[i], isOpen(ms[i]));
  });
  window.addEventListener('resize', placeAll);
  document.addEventListener('scroll', placeAll, { capture: true, passive: true });
  document.addEventListener('htmx:beforeRequest', function (e) {
    var el = e.target;
    var m = el && el.closest && el.closest('[data-floating-menu]');
    if (m && isOpen(m)) { try { m.hidePopover(); } catch (err) {} }
  });
})();
`))
}
