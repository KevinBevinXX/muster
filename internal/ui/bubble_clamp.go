package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// BUBBLE CLAMP: a chat bubble taller than about twelve lines is drawn clamped —
// its top twelve lines, fading out, and a "Show more" control under it. A tap on
// the closed bubble or on "Show more" animates it open to its full height.
//
// 🔴 A TAP ON THE BUBBLE ONLY EVER OPENS IT. A tap on an OPEN bubble does
// nothing to the clamp — the reader is reading it, and a stray tap must not
// fold the text away under their thumb. The ONLY way to close it is the
// control under it, which reads "Show less" while the bubble is open.
//
// 🔴 ONE SCRIPT FOR BOTH CLAMPED SURFACES, BECAUSE THE BUBBLES ARE ONE
// COMPONENT. It acts on any [data-bubble] element in the document — the mirrored
// view's (MirrorTranscript.Ops), the agent chat page's server-rendered
// transcript (chatBubbleOf) and its own live user bubbles — wherever and
// whenever it appears: a MutationObserver on <body> (which survives the
// chat-log refresh) finds new ones, and a ResizeObserver per bubble re-measures
// it when its width or content changes, so a bubble that is markdown-converted
// after insertion is measured when it actually has a size.
//
// ⚠ THE CHIEF PANEL IS NOT CLAMPED, deliberately: chiefPanelBody does not
// include this script, and the shell it lives in includes it nowhere else.
//
// ⚠ A STREAMING ASSISTANT REPLY IS NOT CLAMPED, deliberately: the agent chat's
// live reply bubbles carry no data-bubble (agentChatScript's ensureText), so
// text arriving at the bottom is never hidden while it streams. The persisted
// copy the chat-log refresh swaps in afterwards is clamped like any other.
//
// 🔴 STICK-TO-BOTTOM: a bubble opened while its log is scrolled to the end keeps
// the log at the end on every animation frame and after it, so expanding the
// newest message never leaves the reader above the bottom. The page scripts'
// own "at the end" state follows, because a scroll to the bottom IS the end.
//
// 🔴 AN OPENED MESSAGE STAYS OPEN ACROSS A REDRAW. The agent chat page swaps
// #chat-log's whole content on sse:chat.reply, task:changed and agent.changed
// (chatLogTrigger), so every bubble is a NEW element after each swap. A bubble
// carrying data-msg-id (a persisted message's row id, chatBubble) is remembered
// by that id when it finishes opening or closing, and a new bubble with a
// remembered-open id is drawn open at once — no animation, no stick-to-bottom.
// A bubble with no id (a mirrored one, a live one) is not remembered.
//
// 🔴 A TAP ON A CONTROL INSIDE A BUBBLE IS THAT CONTROL'S, NOT THE BUBBLE'S. A
// tap whose target is inside a link, a <summary>/<details> (a fenced code
// block is a collapsed <details>), a button, a form field or anything marked
// [data-no-clamp] within the bubble never toggles the bubble.
//
// It is re-executable: the observers and the delegated handler are bound once
// per document, on window.muClamp, and a re-run rescans.
func bubbleClampScript() g.Node {
	return Script(g.Raw(`
(function () {
  if (window.muClamp) { window.muClamp.scan(document); return; }
  var LINES = 12;       // "about 12 lines"
  // What a tap inside a bubble belongs to instead of the bubble.
  var CONTROLS = 'a, summary, details, button, input, textarea, select, label, [data-no-clamp]';
  var reduce = window.matchMedia && matchMedia('(prefers-reduced-motion: reduce)');
  var seen = new WeakSet();
  // The data-msg-id of every bubble the reader left open, for this document.
  var opened = new Set();
  function idOf(el) { return el.getAttribute('data-msg-id') || ''; }
  // remember records el's settled state under its id (none = not remembered).
  function remember(el) {
    var id = idOf(el);
    if (!id) return;
    if (el.getAttribute('data-clamp') === 'open') opened.add(id); else opened.delete(id);
  }

  // The clamped height: LINES of the bubble's own line-height, plus its padding.
  function limit(el) {
    var s = getComputedStyle(el);
    var lh = parseFloat(s.lineHeight);
    if (!(lh > 0)) lh = parseFloat(s.fontSize) * 1.25;
    return Math.round(lh * LINES + parseFloat(s.paddingTop) + parseFloat(s.paddingBottom));
  }
  function logOf(el) { return el.closest('#mv-log, #chat-log'); }
  function atBottom(log) { return !!log && log.scrollHeight - log.scrollTop - log.clientHeight < 80; }
  function toggleOf(el) { var n = el.nextElementSibling; return n && n.hasAttribute('data-clamp-toggle') ? n : null; }

  function setToggle(el, open) {
    var b = toggleOf(el);
    if (!b) {
      b = document.createElement('button');
      b.type = 'button';
      b.setAttribute('data-clamp-toggle', '');
      // 44px: a thumb target, like every control (e2e/tests/layout.spec.ts).
      b.className = 'press -mt-2 inline-flex min-h-[44px] items-center px-2 text-xs font-medium text-accent ' +
        (el.getAttribute('data-bubble') === 'user' ? 'self-end' : 'self-start');
      el.after(b);
    }
    b.textContent = open ? 'Show less' : 'Show more';
    b.setAttribute('aria-expanded', open ? 'true' : 'false');
  }

  // measure decides whether el is clamped. An open (or moving) bubble is left
  // alone: the reader chose to see all of it.
  function measure(el) {
    var st = el.getAttribute('data-clamp');
    if (st === 'open' || st === 'moving') return;
    if (!el.isConnected || el.clientWidth === 0) return; // no box yet: measured when it gets one
    var max = limit(el);
    // A line of slack, so a bubble a few pixels over is not clamped to hide half a line.
    var need = el.scrollHeight > max + parseFloat(getComputedStyle(el).lineHeight || '20');
    if (need && opened.has(idOf(el))) {
      // A redrawn copy of a message the reader opened: open at once, unanimated.
      el.style.maxHeight = 'none';
      el.setAttribute('data-clamp', 'open');
      setToggle(el, true);
    } else if (need) {
      el.style.maxHeight = max + 'px';
      el.setAttribute('data-clamp', 'closed');
      setToggle(el, false);
    } else if (st === 'closed') {
      el.removeAttribute('data-clamp');
      el.style.maxHeight = '';
      var b = toggleOf(el); if (b) b.remove();
    }
  }

  var ro = typeof ResizeObserver !== 'undefined' ? new ResizeObserver(function (es) { es.forEach(function (e) { measure(e.target); }); }) : null;
  function track(el) {
    if (seen.has(el)) { measure(el); return; }
    seen.add(el);
    if (ro) ro.observe(el);
    measure(el);
  }
  function scan(root) {
    if (!root || !root.querySelectorAll) return;
    if (root.matches && root.matches('[data-bubble]')) track(root);
    root.querySelectorAll('[data-bubble]').forEach(track);
  }

  // animate moves el's max-height from its current height to target (px, or
  // null = its full height), keeping its log at the bottom throughout when it
  // was there, and calls done after.
  function animate(el, target, done, noFollow) {
    var log = logOf(el), stick = !noFollow && atBottom(log);
    var to = target == null ? el.scrollHeight : target;
    el.setAttribute('data-clamp', 'moving');
    el.style.maxHeight = el.getBoundingClientRect().height + 'px';
    void el.offsetHeight; // commit the start height before the transition
    var finished = false, frame = 0;
    function follow() { if (stick && log) log.scrollTop = log.scrollHeight; }
    function finish() {
      if (finished) return;
      finished = true;
      cancelAnimationFrame(frame);
      el.removeEventListener('transitionend', onEnd);
      done();
      remember(el);
      follow();
    }
    function onEnd(e) { if (e.target === el && e.propertyName === 'max-height') finish(); }
    function tick() { follow(); if (!finished) frame = requestAnimationFrame(tick); }
    if (reduce && reduce.matches) { el.style.maxHeight = to + 'px'; finish(); return; }
    el.addEventListener('transitionend', onEnd);
    el.style.maxHeight = to + 'px';
    frame = requestAnimationFrame(tick);
    setTimeout(finish, 600); // a transition that never ends (display:none, no change) still finishes
  }

  // expand opens el. opts.follow === false skips stick-to-bottom: the message
  // index opens a bubble it is jumping TO, not one at the end.
  function expand(el, opts) {
    if (el.getAttribute('data-clamp') !== 'closed') return;
    setToggle(el, true);
    animate(el, null, function () { el.style.maxHeight = 'none'; el.setAttribute('data-clamp', 'open'); }, opts && opts.follow === false);
  }
  function collapse(el) {
    if (el.getAttribute('data-clamp') !== 'open') return;
    setToggle(el, false);
    var max = limit(el);
    animate(el, max, function () { el.style.maxHeight = max + 'px'; el.setAttribute('data-clamp', 'closed'); });
  }
  // toggle is the CONTROL's action only: Show more opens, Show less closes.
  function toggle(el) {
    var st = el.getAttribute('data-clamp');
    if (st === 'closed') expand(el); else if (st === 'open') collapse(el);
  }

  document.addEventListener('click', function (e) {
    var t = e.target;
    if (!t || !t.closest) return;
    var b = t.closest('[data-clamp-toggle]');
    if (b) {
      var prev = b.previousElementSibling;
      if (prev && prev.hasAttribute('data-clamp')) toggle(prev);
      return;
    }
    // A tap on the bubble itself — but not on a control inside it (see the
    // comment above bubbleClampScript), and not the end of a text selection.
    // It OPENS a closed bubble and never collapses an open one: expand is a
    // no-op unless the bubble is closed.
    var el = t.closest('[data-clamp]');
    if (!el) return;
    var ctl = t.closest(CONTROLS);
    if (ctl && el.contains(ctl)) return;
    var sel = window.getSelection && window.getSelection();
    if (sel && !sel.isCollapsed && el.contains(sel.anchorNode)) return;
    expand(el);
  });

  new MutationObserver(function (ms) {
    ms.forEach(function (m) {
      m.addedNodes.forEach(function (n) { if (n.nodeType === 1) scan(n); });
      // Content replaced INSIDE a bubble (the agent chat converts markdown in place).
      var host = m.target && m.target.nodeType === 1 && m.target.closest && m.target.closest('[data-bubble]');
      if (host) measure(host);
    });
  }).observe(document.body, { childList: true, subtree: true });

  window.muClamp = { scan: scan, measure: measure, expand: expand, collapse: collapse };
  scan(document);
})();
`))
}
