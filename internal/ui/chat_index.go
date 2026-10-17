package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE MESSAGE INDEX: every message the reader sent in a mirrored
// conversation, in a sheet opened from the Rendered view's header. A tap on one
// whose bubble is on the page scrolls to it, opens it if it is clamped
// (bubbleClampScript) and highlights it briefly.
//
// 🔴 "EVERY" MEANS THE WHOLE TRANSCRIPT, NOT THE PAGE. The view loads only the
// file's last window (mirror.BackfillWindow / MaxBackfillEvents), so the list
// is the HOST's prompt index (mirror.Msg.PromptIndex: every prompt in the file,
// window.muMirrorIndex here) joined with the page's own [data-bubble="user"]
// elements for whatever streamed in after it. A prompt whose bubble is not on
// the page is listed with its text in full and an "earlier — not loaded" mark,
// never left out; when the host's bound dropped the oldest, a line says how
// many. A host that predates the index sends none: the list is then the page's
// bubbles, under a line saying that is all it can show.
//
// Ids: a bubble's id is its prompt's place in the transcript (MirrorPromptID),
// the same id the index names it by. A bubble with none (an older host) is
// given a page-unique one the first time the index sees it, never reused.
//
// It is on the mirrored Rendered view only.
//
// 🔴 THE SHEET IS TABBED (tabs.go): "Your messages" and "Focus history" — the
// session's past focus, newest first (focus_history.go). The tab chosen is
// kept per session in localStorage (the Tabs key, which the page derives from
// the window's address), and the sheet is a FIXED height, so neither switching
// tabs nor a history page arriving moves anything outside the list.

// chatIndexButton is the header control that opens the index for the log with
// id logID.
func chatIndexButton(logID string) g.Node {
	return Button(
		Type("button"),
		ID("chat-index-open"),
		g.Attr("data-chat-index-log", logID),
		g.Attr("aria-label", "Your messages and focus history"),
		g.Attr("title", "Your messages and focus history"),
		g.Attr("aria-haspopup", "dialog"),
		g.Attr("aria-controls", "chat-index"),
		g.Attr("aria-expanded", "false"),
		Class("press inline-flex h-11 w-11 shrink-0 items-center justify-center rounded-lg text-fg2 transition hover:bg-s2"),
		g.Raw(`<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M8 6h13M8 12h13M8 18h13M3 6h.01M3 12h.01M3 18h.01"/></svg>`),
	)
}

// chatIndexSheet is the index's sheet, closed: a bottom sheet over a backdrop,
// with two tabs — the reader's messages (built by chatIndexScript) and the
// session's focus history (historyPath, FocusHistoryPath of its newest page;
// "" = the host reports no Claude session, so there is none). tabsKey is what
// the tab choice is stored under.
func chatIndexSheet(historyPath, tabsKey string) g.Node {
	tabs := Tabs("chat-index-tabs", tabsKey, "Session index",
		Tab{ID: "messages", Label: "Your messages", Panel: chatIndexMessages()},
		Tab{ID: "focus", Label: "Focus history", Panel: focusHistoryList(historyPath)},
	)
	return g.Group{Div(
		ID("chat-index"),
		g.Attr("hidden", ""),
		g.Attr("role", "dialog"),
		g.Attr("aria-modal", "true"),
		g.Attr("aria-label", "Your messages and focus history"),
		Class("fixed inset-0 z-50 flex items-end justify-center sm:items-center"),
		Div(g.Attr("data-chat-index-close", ""), Class("absolute inset-0 bg-black/60")),
		Div(
			Class("relative z-10 flex h-[70dvh] w-full max-w-lg flex-col overflow-hidden rounded-t-2xl bg-s1 shadow-2xl ring-1 ring-line sm:rounded-2xl"),
			Div(
				Class("flex items-center gap-2 border-b border-line pl-2 pr-2"),
				tabs.List,
				Button(
					Type("button"),
					g.Attr("data-chat-index-close", ""),
					g.Attr("aria-label", "Close"),
					Class("press inline-flex h-11 w-11 items-center justify-center rounded-lg text-muted transition hover:bg-s2 hover:text-fg"),
					g.Raw(`<svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M18 6 6 18M6 6l12 12"/></svg>`),
				),
			),
			tabs.Panels,
		),
	), tabsScript(), focusHistoryScript()}
}

// chatIndexMessages is the "Your messages" tab's panel: the rows
// chatIndexScript builds, and its notes.
func chatIndexMessages() g.Node {
	return g.Group{
		// ⚠ Shown when the host sent no prompt index (it predates it, or its
		// scan failed): the list is only what this page loaded.
		P(ID("chat-index-partial"), g.Attr("hidden", ""), Class("border-b border-line px-4 py-2 text-xs text-muted"),
			g.Text("Only loaded messages shown: this host's muster-mirror does not send the full list.")),
		// The host's bound dropped the oldest prompts: how many.
		P(ID("chat-index-more"), g.Attr("hidden", ""), Class("border-b border-line px-4 py-2 text-xs text-muted")),
		Ol(ID("chat-index-list"), Class("py-1")),
		P(ID("chat-index-empty"), g.Attr("hidden", ""), Class("px-4 py-6 text-center text-sm text-muted"), g.Text("You have not sent anything yet.")),
	}
}

// chatIndexScript builds the sheet's rows and jumps. Bound once per document
// (window.muIndex) by delegation, and every element is looked up when it is
// used.
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
  // A bubble's id: its prompt's (MirrorPromptID), else a page-unique one.
  function idOf(el) { if (!el.id) el.id = 'chat-umsg-' + (++seq); return el.id; }
  function firstLine(el) {
    var t = (el.innerText || el.textContent || '').split('\n');
    for (var i = 0; i < t.length; i++) { var s = t[i].trim(); if (s) return s; }
    return '';
  }

  // rows is every sent message, oldest first: the host's index (each with its
  // bubble when the page has it), then the page's bubbles it does not name —
  // the ones that streamed in after it, or all of them from a host with none.
  function rows() {
    var idx = window.muMirrorIndex, l = logEl(), out = [], named = {};
    if (idx && idx.items) {
      idx.items.forEach(function (it) {
        var el = document.getElementById(it.id);
        named[it.id] = true;
        out.push({ id: it.id, text: it.text, el: el && l && l.contains(el) ? el : null });
      });
    }
    bubbles().forEach(function (b) {
      if (b.id && named[b.id]) return;
      out.push({ id: idOf(b), text: firstLine(b), el: b });
    });
    return out;
  }

  function build() {
    var list = document.getElementById('chat-index-list');
    if (!list) return;
    var idx = window.muMirrorIndex, rs = rows(), more = idx && idx.more > 0 ? idx.more : 0;
    var partial = document.getElementById('chat-index-partial');
    if (partial) partial.hidden = !!idx;
    var moreEl = document.getElementById('chat-index-more');
    if (moreEl) {
      moreEl.hidden = more === 0;
      moreEl.textContent = more === 0 ? '' : more + ' more earlier, not listed';
    }
    list.textContent = '';
    rs.forEach(function (r, i) {
      var li = document.createElement('li');
      var n = document.createElement('span'); n.className = 'w-6 shrink-0 text-right text-xs tabular-nums text-muted'; n.textContent = String(more + i + 1);
      var t = document.createElement('span');
      if (r.el) {
        var btn = document.createElement('button');
        btn.type = 'button';
        btn.setAttribute('data-chat-index-item', r.id);
        btn.className = 'press flex min-h-[44px] w-full items-center gap-3 px-4 py-2 text-left text-sm text-fg transition hover:bg-s2';
        t.className = 'min-w-0 flex-1 truncate'; t.textContent = r.text || '(empty)';
        btn.appendChild(n); btn.appendChild(t);
        li.appendChild(btn);
      } else {
        // Not on the page: nothing to jump to, so its whole text is here.
        li.setAttribute('data-chat-index-unloaded', r.id);
        li.className = 'flex min-h-[44px] w-full items-start gap-3 px-4 py-2 text-sm text-fg2';
        var body = document.createElement('span'); body.className = 'flex min-w-0 flex-1 flex-col gap-0.5';
        var mark = document.createElement('span'); mark.className = 'text-[11px] font-medium uppercase tracking-wide text-muted';
        mark.setAttribute('data-chat-index-mark', ''); mark.textContent = 'earlier — not loaded';
        t.className = 'whitespace-pre-wrap [overflow-wrap:anywhere]'; t.textContent = r.text || '(empty)';
        body.appendChild(mark); body.appendChild(t);
        li.appendChild(n); li.appendChild(body);
      }
      list.appendChild(li);
    });
    var empty = document.getElementById('chat-index-empty');
    if (empty) empty.hidden = rs.length > 0 || more > 0;
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
  // The host's index arrived (or a Reset dropped it) while the sheet is open.
  document.addEventListener('muster:chat-index', function () { var s = sheet(); if (s && !s.hidden) build(); });

  window.muIndex = { open: open, close: close, jump: jump };
})();
`))
}
