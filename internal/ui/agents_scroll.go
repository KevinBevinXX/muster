package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE AGENTS TAB KEEPS ITS SCROLL POSITIONS ACROSS HYDRATION: the page's own
// (the document scrolls the Agents list) and the "Recently viewed" strip's
// (it scrolls sideways inside itself).
//
// 🔴 THE LIST: ITS CARDS ARE NOT IN THE FIRST PAINT. #agents-list arrives empty
// and fills from its own `load` request (the shell reads no instances on its
// first byte: TestTheAgentsShellReadsNoInstances). So when the page is reloaded,
// or come back to, the document is one screen tall until that request lands:
// the browser's own scroll restoration clamps to 0, and appScript's
// navApplyScroll chases the stored offset for NAV_SCROLL_SETTLE_MS (2s) and
// then gives up. MEASURED on the base tree (e2e/tests/host-icons.spec.ts): with
// the list's request held 3s, a reload and a Back both landed at 0 from 700,
// and even a fast load showed 0 for the frames before the cards arrived, then
// jumped to 700.
//
// The fix RESERVES THE LIST'S BOX: its height when the page was left is kept
// (localStorage, beside appScript's offset, with the same 12h expiry), and an
// empty list is given that min-height on the first paint, until its first load
// settles (or fails). The stored offset is then reachable at once — the
// browser's restore and navApplyScroll both land on the first frame — and the
// cards fill in under a viewport that does not move.
//
// 🔴 THE STRIP: IT WAS REPLACED, NOT MORPHED. Every list render carries a
// fresh copy of #agents-recent out of band, and `hx-swap-oob="true"` is an
// outerHTML REPLACE: the sideways scroller was a new element each time, at
// scrollLeft 0 — on the first load and on every refresh after it. The copy is
// now morphed (recentStrip's `morph:outerHTML`), which keeps the scroller. Its
// offset is also kept for the browser tab (sessionStorage) and put back on a
// strip this document has not seen, so a reload or a Back keeps it too.

const (
	// agentsListHeightKey is where the list's height is kept (localStorage).
	agentsListHeightKey = "muster.agents.listh"
	// recentScrollKey is where the strip's sideways offset is kept
	// (sessionStorage).
	recentScrollKey = "muster.agents.recent.x"
)

func agentsScrollScript() g.Node {
	return Script(g.Raw(`
(function () {
  if (window.__musterAgentsScroll) return;
  window.__musterAgentsScroll = true;
  var LIST_KEY = '` + agentsListHeightKey + `', STRIP_KEY = '` + recentScrollKey + `';
  // appScript's NAV_SCROLL_TTL_MS: a height older than the offset it serves is
  // of no use.
  var TTL = 12 * 60 * 60 * 1000;
  function el(id) { return document.getElementById(id); }
  function lsGet(k) { try { return localStorage.getItem(k); } catch (e) { return null; } }
  function lsSet(k, v) { try { localStorage.setItem(k, v); } catch (e) {} }
  function ssGet(k) { try { return sessionStorage.getItem(k); } catch (e) { return null; } }
  function ssSet(k, v) { try { sessionStorage.setItem(k, v); } catch (e) {} }

  // --- the list's box -------------------------------------------------------
  // save: the list's height as it is drawn, when the page is left — the same
  // two moments appScript saves its offset at. Only a drawn list counts.
  function saveList() {
    var list = el('agents-list');
    if (!list || !list.querySelector(':scope > article')) return;
    var h = Math.round(list.getBoundingClientRect().height);
    if (h > 0) lsSet(LIST_KEY, h + ':' + Date.now());
  }
  window.addEventListener('pagehide', saveList);
  document.addEventListener('visibilitychange', function () { if (document.visibilityState === 'hidden') saveList(); });
  function storedHeight() {
    var p = String(lsGet(LIST_KEY) || '').split(':');
    var h = parseInt(p[0], 10), at = parseInt(p[1], 10), age = Date.now() - at;
    return p.length === 2 && h > 0 && at > 0 && age >= 0 && age <= TTL ? h : 0;
  }
  // reserve runs NOW, at parse time — this script is in the body, after the
  // panels, and before appScript's restore — so the box is there for the first
  // paint and for the first scroll attempt. Only a list that is still EMPTY is
  // reserved: one that came with cards has its real height.
  var reserved = null;
  function release() {
    if (!reserved) return;
    reserved.style.minHeight = '';
    reserved.removeAttribute('data-reserved');
    reserved = null;
  }
  (function reserve() {
    var list = el('agents-list'), h = storedHeight();
    if (!list || !h || list.querySelector(':scope > article')) return;
    list.style.minHeight = h + 'px';
    list.setAttribute('data-reserved', String(h));
    reserved = list;
  })();
  // Released when the list's first load has SETTLED (the cards are drawn: the
  // document now has its real height), or failed. Not on a timer: the
  // reservation exists for a load that is slow.
  document.addEventListener('htmx:afterSettle', function (e) { if (e.target && e.target.id === 'agents-list') release(); });
  ['htmx:responseError', 'htmx:sendError', 'htmx:timeout'].forEach(function (ev) {
    document.addEventListener(ev, function (e) { var t = e.detail && e.detail.target; if (t && t.id === 'agents-list') release(); });
  });
  // A boosted navigation or a Back restoring a cached body replaces the list.
  document.addEventListener('htmx:beforeSwap', function (e) { if (e.target === document.body) release(); });
  document.addEventListener('htmx:historyRestore', release);

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
