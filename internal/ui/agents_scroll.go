package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE AGENTS TAB KEEPS ITS SCROLL POSITIONS ACROSS HYDRATION: the page's own
// (the document scrolls the Agents list) and the "Recently viewed" strip's
// (it scrolls sideways inside itself).
//
// ⚠ THE LIST'S HEIGHT USED TO BE RESERVED HERE, AND IS NOT ANY MORE. When the
// Agents tab's document drew #agents-list empty and filled it from its own
// `load`, a reload or a Back landed at scroll 0 (the document was one screen
// tall until the cards came), so the list's height was kept in localStorage
// and given to the empty list as a min-height until its first load settled.
// The document now carries the cards (agents_list_ssr.go), so the list has its
// real height on the first frame. What the reservation still did was a defect:
// with ZERO agents the empty state has no <article>, the stored height was
// reserved, and nothing released it (no load; measured: a 2400px empty box
// after settle, both sizes). The one path where the list still fills late — a
// client-side switch to the Agents tab before the hidden list's load lands —
// measured CLS 0 and scrollY 0 with a stored height and without, at both
// sizes, so it had nothing left to protect. Deleted.
//
// 🔴 THE STRIP: IT WAS REPLACED, NOT MORPHED. Every list render carries a
// fresh copy of #agents-recent out of band, and `hx-swap-oob="true"` is an
// outerHTML REPLACE: the sideways scroller was a new element each time, at
// scrollLeft 0 — on the first load and on every refresh after it. The copy is
// now morphed (recentStrip's `morph`), which keeps the scroller. Its
// offset is also kept for the browser tab (sessionStorage) and put back on a
// strip this document has not seen, so a reload or a Back keeps it too.

// recentScrollKey is where the strip's sideways offset is kept
// (sessionStorage).
const recentScrollKey = "muster.agents.recent.x"

func agentsScrollScript() g.Node {
	return Script(g.Raw(`
(function () {
  if (window.__musterAgentsScroll) return;
  window.__musterAgentsScroll = true;
  var STRIP_KEY = '` + recentScrollKey + `';
  function ssGet(k) { try { return sessionStorage.getItem(k); } catch (e) { return null; } }
  function ssSet(k, v) { try { sessionStorage.setItem(k, v); } catch (e) {} }

  // --- the strip's sideways offset -----------------------------------------
  function scroller() { return document.querySelector('#` + recentStripID + ` > div'); }
  // Kept on every scroll of the strip (scroll does not bubble: captured).
  document.addEventListener('scroll', function (e) {
    var s = scroller();
    if (s && e.target === s && s.__musterSeen) ssSet(STRIP_KEY, String(Math.round(s.scrollLeft)));
  }, true);
  // put: a strip this document has not seen yet — the first paint's, or one
  // drawn where there was none — is given the kept offset. A strip already
  // seen is the reader's: a refresh (morphed) never touches it.
  function put() {
    var s = scroller();
    if (!s || s.__musterSeen) return;
    s.__musterSeen = true;
    var x = parseInt(ssGet(STRIP_KEY) || '', 10);
    if (x > 0) s.scrollLeft = x;
  }
  put();
  document.addEventListener('htmx:load', put);
  document.addEventListener('htmx:afterSettle', put);
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', put);
})();
`))
}
