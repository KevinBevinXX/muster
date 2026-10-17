package ui

import (
	"net/url"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// TWO CONTROLS OF THE RENDERED VIEW'S COMPOSER (a mirrored Claude Code window):
//
//  1. SEND, WITH QUEUE BEHIND A CARET (sendSplit). The operator's decision is
//     "always Send": the button's face is Send and nothing else, so a stray tap
//     can only send — never queue. A caret beside it opens a one-item menu,
//     "Queue for when idle", which is the queue's submit (send_queue.go). The
//     menu is a POPOVER (the `popover` attribute, opened by the caret's
//     `popovertarget`), so it opens, light-dismisses and closes on Escape with
//     no script at all; sendMenuScript adds the menu-button keyboard pattern,
//     places it above the caret, and keeps aria-expanded true to what is shown.
//     Without a database (Queue nil) or on a disabled composer there is no
//     caret: Send is the whole control, as it always was.
//
//  2. THE `/` AND `:` MENU (composerMenuBox, composerMenuScript). Typing `/` at
//     the start of the message, or after white space, lists the window's
//     commands and skills — Claude Code's built-ins and what its host reports
//     (user, plugin and the window's PROJECT .claude) — and `:` the operator's
//     snippets, filtered by window.musterRank (internal/searchrank's browser
//     copy, rankJS). Arrow keys move, Enter or Tab insert, Escape closes, a tap
//     inserts. A command inserts "/name " for the rest to be typed; a snippet
//     inserts its text, which stays editable before it is sent.
//
//     🔴 THE RULE FOR WHEN IT OPENS IS CLAUDE CODE'S OWN, MEASURED, NOT GUESSED.
//     In Claude Code 2.1.289 (a real pane, 2026-10-17): "/co" at the start of
//     the input opens the command menu; "hello /co" offers the completion too
//     (after white space); "hello/co" and "see https://x.com/co" offer nothing.
//     So: the trigger character must be the first character of the message or
//     follow white space, and the caret must be at the end of that word (a `/`
//     typed into the middle of a word is never taken). A second `/` in the word
//     ("/usr/bin") closes it: that is a path, not a command.
//
//     🔴 IT NEVER COVERS THE INPUT. It is placed above the whole composer
//     (bottom-full of #mv-compose, which is pinned to the visible viewport's
//     bottom and rides on top of a phone's keyboard), and its height is capped
//     to the room between the composer's top and the header's bottom. It is out
//     of flow, so opening it moves nothing (no layout shift).
//
//     The data (MirrorCompletePath) is fetched ONCE per page, on the first
//     trigger, and is owner-scoped on the server. Every string from it is
//     drawn with textContent: a host's skill description is host file content.

// composerSendSplitClass is Send's look as the left half of the split button.
const composerSendSplitClass = "press min-h-[44px] shrink-0 rounded-l-xl rounded-r-none bg-accent px-4 text-sm font-semibold text-on-accent transition disabled:opacity-40"

// sendMoreClass is the caret: the right half, 44px square.
const sendMoreClass = "press inline-flex min-h-[44px] min-w-[44px] shrink-0 items-center justify-center rounded-l-none rounded-r-xl border-l border-on-accent/30 bg-accent text-on-accent transition disabled:opacity-40"

// sendSplit is the composer's Send control: Send alone, or — with a queue —
// Send with the caret and its menu.
func sendSplit(q *ComposerQueue, off bool) g.Node {
	send := func(cls string) g.Node {
		return Button(
			ID("mv-send"),
			Type("submit"),
			g.If(off, g.Attr("disabled", "")),
			g.Attr("aria-label", "Send"),
			Class(cls),
			g.Text("Send"),
		)
	}
	if q == nil || off {
		return send(composerSendClass)
	}
	return Div(
		ID("mv-send-group"),
		g.Attr("role", "group"),
		g.Attr("aria-label", "Send"),
		Class("flex shrink-0"),
		send(composerSendSplitClass),
		Button(
			ID("mv-send-more"),
			Type("button"),
			g.Attr("popovertarget", "mv-send-menu"),
			g.Attr("aria-haspopup", "menu"),
			g.Attr("aria-expanded", "false"),
			g.Attr("aria-controls", "mv-send-menu"),
			g.Attr("aria-label", "More ways to send"),
			g.Attr("title", "More ways to send"),
			Class(sendMoreClass),
			g.Raw(`<svg class="h-4 w-4" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true"><path fill-rule="evenodd" d="M14.77 12.79a.75.75 0 01-1.06-.02L10 8.83l-3.71 3.94a.75.75 0 11-1.08-1.04l4.25-4.5a.75.75 0 011.08 0l4.25 4.5a.75.75 0 01-.02 1.06z" clip-rule="evenodd"/></svg>`),
		),
		Div(
			ID("mv-send-menu"),
			g.Attr("popover", ""),
			g.Attr("role", "menu"),
			g.Attr("aria-label", "More ways to send"),
			Class("m-0 min-w-[14rem] rounded-xl border-0 bg-s1 p-1 text-fg shadow-lg ring-1 ring-line"),
			sendQueueButton(q, off),
		),
	)
}

// sendMenuScript is the caret's menu-button behaviour on top of the popover's
// own: ArrowDown / ArrowUp / Enter / Space on the caret open the menu with its
// item focused; in the menu, the arrow keys, Home and End move, Escape closes
// it and puts focus back on the caret, Tab closes it. aria-expanded follows the
// popover's actual state (its toggle event), and the menu is placed above the
// caret, right-aligned — the popover's default place is the middle of the
// screen, which is where it stays with no script.
func sendMenuScript() g.Node {
	return Script(g.Raw(`
(function () {
  var more = document.getElementById('mv-send-more');
  var menu = document.getElementById('mv-send-menu');
  if (!more || !menu || typeof menu.showPopover !== 'function') return;
  function items() { return Array.prototype.slice.call(menu.querySelectorAll('[role=menuitem]')); }
  function focusItem(i) {
    var it = items(); if (!it.length) return;
    it[(i + it.length) % it.length].focus();
  }
  function place() {
    var r = more.getBoundingClientRect();
    var vv = window.visualViewport;
    var bottom = vv ? vv.height + vv.offsetTop : window.innerHeight;
    menu.style.position = 'fixed';
    menu.style.inset = 'auto';
    menu.style.margin = '0';
    menu.style.right = Math.max(8, Math.round(document.documentElement.clientWidth - r.right)) + 'px';
    menu.style.bottom = Math.max(8, Math.round(window.innerHeight - r.top + 6 - (window.innerHeight - bottom))) + 'px';
  }
  var focusFirst = false;
  // Placed BEFORE it is shown (beforetoggle): placed on toggle, it is painted
  // once at the popover's default place first and then jumps — a layout shift.
  menu.addEventListener('beforetoggle', function (e) { if (e.newState === 'open') place(); });
  menu.addEventListener('toggle', function (e) {
    var open = e.newState ? e.newState === 'open' : menu.matches(':popover-open');
    more.setAttribute('aria-expanded', open ? 'true' : 'false');
    if (open) { if (!e.newState) place(); if (focusFirst) focusItem(0); focusFirst = false; }
  });
  // A pointer open focuses the item too: the menu is one item, and a keyboard
  // user who clicked the caret should land in it.
  more.addEventListener('click', function () { focusFirst = true; });
  more.addEventListener('keydown', function (e) {
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      focusFirst = true;
      if (!menu.matches(':popover-open')) menu.showPopover(); else focusItem(e.key === 'ArrowUp' ? -1 : 0);
    }
  });
  menu.addEventListener('keydown', function (e) {
    var it = items(), i = it.indexOf(document.activeElement);
    if (e.key === 'ArrowDown') { e.preventDefault(); focusItem(i + 1); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); focusItem(i - 1); }
    else if (e.key === 'Home') { e.preventDefault(); focusItem(0); }
    else if (e.key === 'End') { e.preventDefault(); focusItem(-1); }
    else if (e.key === 'Escape') { e.preventDefault(); menu.hidePopover(); more.focus(); }
    else if (e.key === 'Tab') { menu.hidePopover(); }
  });
  // Choosing the item submits the form; the menu has done its job.
  menu.addEventListener('click', function (e) {
    var t = e.target && e.target.closest ? e.target.closest('[role=menuitem]') : null;
    if (t && !t.disabled) { try { menu.hidePopover(); } catch (err) {} }
  });
  window.addEventListener('resize', function () { if (menu.matches(':popover-open')) place(); });
})();
`))
}

// composerMenuSrc is the menu data's URL for a composer that has the menu —
// the Rendered view's (MirrorSendPath), enabled — else "".
func composerMenuSrc(v ComposerView) string {
	if v.SendPath != MirrorSendPath || v.Disabled != "" {
		return ""
	}
	q := url.Values{}
	for _, f := range []ComposerField{{"host", v.Host}, {"session", v.Session}, {"window", v.Window}} {
		if f.Value != "" {
			q.Set(f.Name, f.Value)
		}
	}
	return MirrorCompletePath + "?" + q.Encode()
}

// composerMenuBox is the `/` and `:` menu's place: a listbox over the
// composer, hidden until a trigger is typed, and its note line.
func composerMenuBox(src string) g.Node {
	if src == "" {
		return nil
	}
	return Div(
		ID("mv-complete-box"),
		g.Attr("data-src", src),
		g.Attr("hidden", ""),
		Class("absolute inset-x-2 bottom-full z-30 mb-1 flex flex-col overflow-hidden rounded-xl bg-s1 shadow-lg ring-1 ring-line"),
		Ul(
			ID("mv-complete"),
			g.Attr("role", "listbox"),
			g.Attr("aria-label", "Commands, skills and snippets"),
			Class("min-h-0 flex-1 overflow-y-auto overscroll-contain py-1"),
		),
		P(ID("mv-complete-note"), g.Attr("hidden", ""), Class("border-t border-line px-3 py-1.5 text-[11px] text-muted")),
	)
}

// composerMenuScript wires the `/` and `:` menu to #mv-input (see the file's
// comment for the rule and the layout). Its keys are taken in the CAPTURE phase
// on the form, so an Enter or Tab that picks an item never reaches the
// composer's own Enter-sends handler; Ctrl/Cmd+Enter is never taken — it sends,
// menu or not.
func composerMenuScript(src string) g.Node {
	if src == "" {
		return nil
	}
	return Script(g.Raw(`
(function () {
` + rankJS + `
  var input = document.getElementById('mv-input');
  var form = document.getElementById('mv-form');
  var box = document.getElementById('mv-complete-box');
  var list = document.getElementById('mv-complete');
  var note = document.getElementById('mv-complete-note');
  var compose = document.getElementById('mv-compose');
  if (!input || !form || !box || !list || typeof fetch !== 'function') return;
  var src = box.getAttribute('data-src');
  var data = null, loading = false;
  var token = null, shown = [], active = -1, dismissed = -1;
  var SOURCE = { project: 0, user: 1, plugin: 2, builtin: 3 };
  var MAX = 50;
  input.setAttribute('aria-autocomplete', 'list');
  input.setAttribute('aria-controls', 'mv-complete');

  function load() {
    if (data || loading) return;
    loading = true;
    fetch(src, { credentials: 'same-origin', headers: { 'Accept': 'application/json', 'X-Requested-With': 'XMLHttpRequest' } })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (d) {
        loading = false;
        data = d && d.commands ? d : { commands: [], snippets: [], note: 'The menu could not be loaded.' };
        refresh();
      }, function () { loading = false; data = { commands: [], snippets: [], note: 'The menu could not be loaded.' }; refresh(); });
  }

  // tokenAt is the trigger word the caret ends, or null: '/' or ':' as the
  // first character of the message or after white space, the caret at the end
  // of the word, and (for '/') no second '/' in it.
  function tokenAt() {
    if (input.selectionStart !== input.selectionEnd) return null;
    var v = input.value, c = input.selectionStart;
    if (c < v.length && !/\s/.test(v.charAt(c))) return null;
    var i = c;
    while (i > 0 && !/\s/.test(v.charAt(i - 1))) i--;
    var k = v.charAt(i);
    if (c - i < 1 || (k !== '/' && k !== ':')) return null;
    var q = v.slice(i + 1, c);
    if (k === '/' && q.indexOf('/') >= 0) return null;
    return { kind: k, start: i, end: c, q: q };
  }

  function matches(t) {
    var out = [];
    if (t.kind === '/') {
      (data.commands || []).forEach(function (e, n) {
        var r = window.musterRank(e.name, t.q);
        if (r >= 0) out.push({ e: e, r: r, n: n });
      });
      out.sort(function (a, b) {
        return a.r - b.r || (t.q ? a.e.name.length - b.e.name.length : 0) ||
          (SOURCE[a.e.source] - SOURCE[b.e.source]) || (a.e.name < b.e.name ? -1 : a.e.name > b.e.name ? 1 : 0);
      });
    } else {
      (data.snippets || []).forEach(function (s, n) {
        // The trigger by the full rule; the label only by a prefix, word or
        // substring — its letters-in-order (tier 3) would match most labels.
        var a = window.musterRank(s.trigger.slice(1), t.q), b = window.musterRank(s.label || '', t.q);
        if (b > 2) b = -1;
        var r = a < 0 ? b : (b < 0 ? a : Math.min(a, b));
        if (r >= 0) out.push({ s: s, r: r, n: n });
      });
      out.sort(function (a, b) { return a.r - b.r || a.n - b.n; });
    }
    return out.slice(0, MAX);
  }

  function close() {
    token = null; shown = []; active = -1;
    if (!box.hidden) box.hidden = true;
    input.removeAttribute('aria-activedescendant');
    list.textContent = '';
  }

  // fit caps the menu to the room above the composer, below the header.
  function fit() {
    var top = compose.getBoundingClientRect().top;
    var head = document.querySelector('body > header');
    var floor = head ? Math.max(0, head.getBoundingClientRect().bottom) : 0;
    var vv = window.visualViewport;
    if (vv) floor = Math.max(floor, vv.offsetTop);
    box.style.maxHeight = Math.max(88, Math.min(320, Math.floor(top - floor - 12))) + 'px';
  }

  function mark() {
    var opts = list.children;
    for (var i = 0; i < opts.length; i++) {
      var on = i === active;
      opts[i].setAttribute('aria-selected', on ? 'true' : 'false');
      opts[i].classList.toggle('bg-s3', on);
    }
    if (active >= 0 && opts[active]) {
      input.setAttribute('aria-activedescendant', opts[active].id);
      var o = opts[active];
      if (o.offsetTop < list.scrollTop) list.scrollTop = o.offsetTop;
      else if (o.offsetTop + o.offsetHeight > list.scrollTop + list.clientHeight) list.scrollTop = o.offsetTop + o.offsetHeight - list.clientHeight;
    } else input.removeAttribute('aria-activedescendant');
  }

  function el(tag, cls, text) { var x = document.createElement(tag); if (cls) x.className = cls; if (text != null) x.textContent = text; return x; }

  function render(t, found) {
    list.textContent = '';
    found.forEach(function (m, i) {
      var li = el('li', 'flex min-h-[44px] cursor-pointer flex-col justify-center gap-0.5 px-3 py-1.5');
      li.id = 'mv-opt-' + i;
      li.setAttribute('role', 'option');
      li.setAttribute('data-index', String(i));
      var row = el('div', 'flex min-w-0 items-baseline gap-2');
      if (m.e) {
        li.setAttribute('data-name', m.e.name);
        row.appendChild(el('span', 'truncate font-mono text-sm text-fg', '/' + m.e.name));
        if (m.e.source !== 'builtin') row.appendChild(el('span', 'shrink-0 rounded bg-s2 px-1.5 text-[10px] text-muted', m.e.source));
        li.appendChild(row);
        if (m.e.description) li.appendChild(el('span', 'truncate text-xs text-muted', m.e.description));
      } else {
        li.setAttribute('data-trigger', m.s.trigger);
        row.appendChild(el('span', 'shrink-0 font-mono text-sm text-fg', m.s.trigger));
        if (m.s.label) row.appendChild(el('span', 'truncate text-xs text-fg2', m.s.label));
        li.appendChild(row);
        li.appendChild(el('span', 'truncate text-xs text-muted', m.s.body));
      }
      list.appendChild(li);
    });
  }

  function refresh() {
    var t = tokenAt();
    if (!t) { dismissed = -1; close(); return; }
    if (t.start === dismissed) { close(); return; }
    if (!data) { load(); close(); return; }
    var found = matches(t);
    if (!found.length) { close(); return; }
    var same = token && token.kind === t.kind && token.start === t.start && token.q === t.q;
    token = t;
    if (!same) {
      shown = found; active = 0;
      render(t, found);
    }
    var msg = t.kind === '/' ? (data.note || '') : '';
    note.textContent = msg; note.hidden = !msg;
    box.hidden = false;
    fit();
    mark();
  }

  function insert(i) {
    var m = shown[i], t = token;
    if (!m || !t) return;
    var text = m.e ? '/' + m.e.name + ' ' : m.s.body;
    var v = input.value;
    input.value = v.slice(0, t.start) + text + v.slice(t.end);
    var at = t.start + text.length;
    input.setSelectionRange(at, at);
    // What was inserted is not typed: it must not reopen the menu (a snippet
    // whose text ends in a ':word').
    var nt = tokenAt();
    dismissed = nt ? nt.start : -1;
    close();
    if (window.musterComposerFit) window.musterComposerFit(input);
    input.focus();
  }

  input.addEventListener('input', refresh);
  input.addEventListener('click', refresh);
  input.addEventListener('focus', function () { load(); });
  input.addEventListener('blur', function () { setTimeout(function () { if (document.activeElement !== input) close(); }, 150); });
  document.addEventListener('selectionchange', function () { if (document.activeElement === input && token) refresh(); });
  form.addEventListener('keydown', function (e) {
    if (e.target !== input || box.hidden || !token || e.isComposing) return;
    if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) { close(); return; }
    var n = shown.length;
    if (e.key === 'ArrowDown') { e.preventDefault(); e.stopPropagation(); active = (active + 1) % n; mark(); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); e.stopPropagation(); active = (active - 1 + n) % n; mark(); }
    else if ((e.key === 'Enter' && !e.shiftKey) || e.key === 'Tab') { e.preventDefault(); e.stopPropagation(); insert(active); }
    else if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); dismissed = token.start; close(); }
  }, true);
  // A tap or click picks the option; the mousedown is kept from moving focus,
  // so a phone's keyboard stays up and the caret stays where it was.
  list.addEventListener('mousedown', function (e) { e.preventDefault(); });
  list.addEventListener('click', function (e) {
    var li = e.target && e.target.closest ? e.target.closest('[role=option]') : null;
    if (li) insert(Number(li.getAttribute('data-index')));
  });
  window.addEventListener('resize', function () { if (!box.hidden) fit(); });
  if (window.visualViewport) window.visualViewport.addEventListener('resize', function () { if (!box.hidden) fit(); });
})();
`))
}
