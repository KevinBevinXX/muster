package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE MESSAGE INDEX: every message the reader sent in this conversation, in a
// sheet opened from the header — its first line and its time — and a tap on
// one scrolls to that bubble, opens it if it is clamped (bubbleClampScript),
// and highlights it briefly.
//
// 🔴 IT IS READ FROM THE PAGE, AT THE MOMENT IT IS OPENED, AND RE-READ WHILE
// OPEN: the list is the [data-bubble="user"] elements in the conversation's log,
// so whatever streamed in — a mirrored transcript's live ops, the agent chat's
// own send, the chat-log refresh — is in it, with no second copy of the
// transcript to keep in step. A MutationObserver on the log rebuilds it while
// the sheet is open.
//
// Ids: a persisted agent-chat message has its row id (chatMsgID). Anything
// else — a mirrored prompt, the agent chat's live user bubble — is given a
// page-unique one the first time the index sees it, never reused.
//
// It is on the mirrored Rendered view and the agent chat page. ⚠ NOT the chief
// panel: its title bar has its own controls and no room for a third; the panel
// is the same conversation as the agent's own page, one tap away.

// chatIndexButton is the header control that opens the index for the log with
// id logID.
func chatIndexButton(logID string) g.Node {
	return Button(
		Type("button"),
		ID("chat-index-open"),
		g.Attr("data-chat-index-log", logID),
		g.Attr("aria-label", "Your messages"),
		g.Attr("title", "Your messages"),
		g.Attr("aria-haspopup", "dialog"),
		g.Attr("aria-controls", "chat-index"),
		g.Attr("aria-expanded", "false"),
		Class("press inline-flex h-11 w-11 shrink-0 items-center justify-center rounded-lg text-fg2 transition hover:bg-s2"),
		g.Raw(`<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M8 6h13M8 12h13M8 18h13M3 6h.01M3 12h.01M3 18h.01"/></svg>`),
	)
}

// chatIndexSheet is the index's sheet, closed: a bottom sheet over a backdrop.
// Its rows are built by chatIndexScript.
func chatIndexSheet() g.Node {
	return Div(
		ID("chat-index"),
		g.Attr("hidden", ""),
		g.Attr("role", "dialog"),
		g.Attr("aria-modal", "true"),
		g.Attr("aria-labelledby", "chat-index-title"),
		Class("fixed inset-0 z-50 flex items-end justify-center sm:items-center"),
		Div(g.Attr("data-chat-index-close", ""), Class("absolute inset-0 bg-black/60")),
		Div(
			Class("relative z-10 flex max-h-[70dvh] w-full max-w-lg flex-col overflow-hidden rounded-t-2xl bg-s1 shadow-2xl ring-1 ring-line sm:rounded-2xl"),
			Div(
				Class("flex items-center gap-2 border-b border-line px-4 py-2"),
				H2(ID("chat-index-title"), Class("flex-1 text-sm font-semibold text-fg"), g.Text("Your messages")),
				Button(
					Type("button"),
					g.Attr("data-chat-index-close", ""),
					g.Attr("aria-label", "Close"),
					Class("press inline-flex h-11 w-11 items-center justify-center rounded-lg text-muted transition hover:bg-s2 hover:text-fg"),
					g.Raw(`<svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M18 6 6 18M6 6l12 12"/></svg>`),
				),
			),
			Ol(ID("chat-index-list"), Class("flex-1 overflow-y-auto overscroll-contain py-1")),
			P(ID("chat-index-empty"), g.Attr("hidden", ""), Class("px-4 py-6 text-center text-sm text-muted"), g.Text("You have not sent anything yet.")),
		),
	)
}

// chatIndexScript builds the sheet's rows and jumps. Bound once per document
// (window.muIndex) by delegation, and every element is looked up when it is
// used, so an hx-boost body swap brings in a fresh header and sheet that just
// work.
//
// Every row's text is set with textContent: no transcript text is parsed as
// markup here.
func chatIndexScript() g.Node {
	return Script(g.Raw(`
(function () {
  if (window.muIndex) return;
  var seq = 0, watch = null;
  var reduce = window.matchMedia && matchMedia('(prefers-reduced-motion: reduce)');

  function sheet() { return document.getElementById('chat-index'); }
  function opener() { return document.getElementById('chat-index-open'); }
  function logEl() { var b = opener(); return b && document.getElementById(b.getAttribute('data-chat-index-log')); }
  function bubbles() { var l = logEl(); return l ? Array.prototype.slice.call(l.querySelectorAll('[data-bubble="user"]')) : []; }
  // A bubble's id: its own (a persisted message's), else a page-unique one.
  function idOf(el) { if (!el.id) el.id = 'chat-umsg-' + (++seq); return el.id; }
  function firstLine(el) {
    var t = (el.innerText || el.textContent || '').split('\n');
    for (var i = 0; i < t.length; i++) { var s = t[i].trim(); if (s) return s; }
    return '';
  }
  function when(el) {
    var at = el.getAttribute('data-at');
    if (!at) return '';
    var d = new Date(at);
    if (isNaN(d.getTime())) return '';
    var today = new Date().toDateString() === d.toDateString();
    return today ? d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
      : d.toLocaleDateString([], { month: 'short', day: 'numeric' }) + ' ' + d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  }

  function build() {
    var list = document.getElementById('chat-index-list');
    if (!list) return;
    var bs = bubbles();
    list.textContent = '';
    bs.forEach(function (b, i) {
      var li = document.createElement('li');
      var btn = document.createElement('button');
      btn.type = 'button';
      btn.setAttribute('data-chat-index-item', idOf(b));
      btn.className = 'press flex min-h-[44px] w-full items-center gap-3 px-4 py-2 text-left text-sm text-fg transition hover:bg-s2';
      var n = document.createElement('span'); n.className = 'w-6 shrink-0 text-right text-xs tabular-nums text-muted'; n.textContent = String(i + 1);
      var t = document.createElement('span'); t.className = 'min-w-0 flex-1 truncate'; t.textContent = firstLine(b) || '(empty)';
      var w = document.createElement('span'); w.className = 'shrink-0 text-xs tabular-nums text-muted'; w.textContent = when(b);
      btn.appendChild(n); btn.appendChild(t); btn.appendChild(w);
      li.appendChild(btn);
      list.appendChild(li);
    });
    var empty = document.getElementById('chat-index-empty');
    if (empty) empty.hidden = bs.length > 0;
  }

  function open() {
    var s = sheet(), b = opener();
    if (!s) return;
    build();
    s.hidden = false;
    if (b) b.setAttribute('aria-expanded', 'true');
    // Keep the list current while it is open: new messages stream in.
    var l = logEl();
    if (l && typeof MutationObserver !== 'undefined') {
      watch = new MutationObserver(build);
      watch.observe(l, { childList: true, subtree: true });
    }
    var list = document.getElementById('chat-index-list');
    var last = list && list.lastElementChild;
    if (last) last.scrollIntoView({ block: 'nearest' }); // the newest is nearest the reader
    var c = s.querySelector('button[data-chat-index-close]'); if (c) c.focus({ preventScroll: true });
  }
  function close() {
    var s = sheet(), b = opener();
    if (watch) { watch.disconnect(); watch = null; }
    if (!s || s.hidden) return;
    s.hidden = true;
    if (b) { b.setAttribute('aria-expanded', 'false'); b.focus({ preventScroll: true }); }
  }

  function jump(id) {
    var el = document.getElementById(id);
    close();
    if (!el) return;
    // Open it first WITHOUT the clamp's stick-to-bottom (the reader is going
    // to IT, not to the end), then bring its top into view: it grows below its
    // top, so the top stays where it is put.
    if (window.muClamp && el.getAttribute('data-clamp') === 'closed') window.muClamp.expand(el, { follow: false });
    el.scrollIntoView({ block: 'start', behavior: reduce && reduce.matches ? 'auto' : 'smooth' });
    el.removeAttribute('data-flash');
    void el.offsetWidth; // restart the highlight if it is still running
    el.setAttribute('data-flash', '');
    setTimeout(function () { el.removeAttribute('data-flash'); }, 1600);
  }

  document.addEventListener('click', function (e) {
    var t = e.target;
    if (!t || !t.closest) return;
    if (t.closest('#chat-index-open')) { open(); return; }
    var item = t.closest('[data-chat-index-item]');
    if (item) { jump(item.getAttribute('data-chat-index-item')); return; }
    if (t.closest('[data-chat-index-close]')) close();
  });
  document.addEventListener('keydown', function (e) { if (e.key === 'Escape') close(); });
  // An hx-boost navigation takes the page (and its sheet) away: stop watching.
  document.addEventListener('htmx:beforeSwap', function (e) { if (e.detail && e.detail.target === document.body) close(); });

  window.muIndex = { open: open, close: close, jump: jump };
})();
`))
}
