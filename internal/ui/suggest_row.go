package ui

import (
	"strconv"
	"time"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// SUGGESTED REPLIES' CHIP ROW (docs/suggested-replies.md): up to three short
// replies a model wrote for a mirrored Claude Code window's turn end, in a row
// directly above the Rendered view's composer. The session switcher sits
// below the input; the two never compete.
//
// 🔴 A TAP FILLS, IT NEVER SENDS. A chip is a type="button" outside the form:
// it puts its text in the message box — the whole box when it is empty, or on
// a new line below a draft already there, never over it — as an EDIT
// (insertText, so Ctrl+Z gives the draft back), and focuses the box with the
// caret at the end. Send is still the operator's, with its own step-up gate.
//
// 🔴 LABELLED AI. The row is a group named SuggestRowLabel, it starts with an
// "AI" mark, and each chip's accessible name says it is an AI-suggested
// reply. Every string from the server is drawn with textContent.
//
// 🔴 NO LAYOUT SHIFT, AND ROOM ONLY WHILE IT IS IN USE. The row is out of flow
// (absolute, bottom-full of the pinned #mv-compose), so showing it moves
// nothing in the composer; its 44px are reserved — added to the pinned
// composer's measured room (composerPinScript), which shrinks the transcript
// from the bottom — only while a set is pending or shown. Hidden, it takes
// none. Without script it is never shown (it is drawn hidden).
//
// 🔴 WITHIN SuggestCutoff OF THE TURN END OR NOT AT ALL. The socket says how
// long ago the turn ended (pending/ready); a set that is not on screen by the
// cutoff is dropped, and the row with it. (A page newly opened on an idle
// turn is a turn end of its own, within the cutoff of the open; a reload
// resumes its socket and shows no row for the turn it reloaded into.)
//
// What it reports (SuggestEventPath): "shown" once per set, "filled" on a tap,
// "sent" when the box is sent after a tap — with the EDIT DISTANCE between
// what the tap left in the box and what was sent. Numbers and the set's
// fingerprint only: never any text.

// SuggestCutoff is how long after a turn ends its suggestions may still
// appear; past it they never do (and the server stops before spending).
const SuggestCutoff = 8 * time.Second

// SuggestPath is the window's current set (JSON); SuggestEventPath where the
// row reports what happened to it.
const (
	SuggestPath      = "/mirror/view/suggest"
	SuggestEventPath = "/mirror/view/suggest/event"
)

// SuggestRowLabel names the row (its group's accessible name).
const SuggestRowLabel = "AI-suggested replies"

// suggestRow is the chip row of a composer whose Suggest is set (its set's
// URL), drawn hidden; nil for none.
func suggestRow(src string) g.Node {
	if src == "" {
		return nil
	}
	return Div(
		ID("mv-suggest"),
		g.Attr("hidden", ""),
		g.Attr("role", "group"),
		g.Attr("aria-label", SuggestRowLabel),
		g.Attr("data-src", src),
		g.Attr("data-event", SuggestEventPath),
		g.Attr("data-cutoff-ms", strconv.FormatInt(SuggestCutoff.Milliseconds(), 10)),
		Class("absolute inset-x-0 bottom-full z-20 flex h-11 items-center gap-1 border-t border-line bg-bg pl-3"),
		Span(
			g.Attr("title", "Written by a model from this conversation"),
			Class("shrink-0 rounded-md bg-s2 px-1.5 py-0.5 text-[10px] font-semibold tracking-wide text-fg2 ring-1 ring-inset ring-line"),
			g.Text("AI"),
		),
		Div(ID("mv-suggest-chips"), Class("flex h-11 min-w-0 flex-1 items-center gap-1 overflow-x-auto overflow-y-hidden")),
		Button(
			ID("mv-suggest-x"),
			Type("button"),
			g.Attr("aria-label", "Hide suggestions for this turn"),
			g.Attr("title", "Hide suggestions for this turn"),
			Class("press inline-flex h-11 w-11 shrink-0 items-center justify-center rounded-lg text-fg2 transition hover:bg-s2"),
			g.Raw(`<svg class="h-4 w-4" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true"><path d="M6.28 5.22a.75.75 0 00-1.06 1.06L8.94 10l-3.72 3.72a.75.75 0 101.06 1.06L10 11.06l3.72 3.72a.75.75 0 101.06-1.06L11.06 10l3.72-3.72a.75.75 0 00-1.06-1.06L10 8.94 6.28 5.22z"/></svg>`),
		),
	)
}

// SuggestPausedText is the composer's line while the viewer's suggested
// replies are auto-paused.
const SuggestPausedText = "Suggested replies are paused: you rarely tapped them."

// suggestPausedNote is that line, with the way back on.
func suggestPausedNote() g.Node {
	return P(ID("mv-suggest-paused"), Class("flex flex-wrap items-center gap-x-1 px-3 text-[11px] text-muted"),
		g.Text(SuggestPausedText+" "),
		A(Href("/account#suggest"), Class("inline-flex min-h-[44px] items-center text-accent underline"), g.Text("Turn them back on")))
}

// suggestChipClass is a chip: a 44px tap target around a pill at most 32px
// tall, its text cut with an ellipsis (the whole text is its title and name).
const (
	suggestChipClass = "press inline-flex min-h-[44px] max-w-[16rem] shrink-0 items-center"
	suggestPillClass = "block max-h-8 truncate rounded-full bg-s2 px-3 py-1.5 text-sm leading-5 text-fg ring-1 ring-inset ring-line hover:bg-s3"
)

// suggestRowScript drives the row: the socket's news (window.musterSuggest,
// called by mirrorViewScript), the fetch of the set, the taps, the × and the
// reports. See the file's comment for the rules.
func suggestRowScript() g.Node {
	return Script(g.Raw(`
(function () {
  var row = document.getElementById('mv-suggest');
  var input = document.getElementById('mv-input');
  if (!row || !input) return;
  var chips = document.getElementById('mv-suggest-chips');
  var x = document.getElementById('mv-suggest-x');
  var src = row.getAttribute('data-src'), evURL = row.getAttribute('data-event');
  var cutoff = +row.getAttribute('data-cutoff-ms') || 8000;
  // gen invalidates whatever an earlier state started (a fetch, a timer);
  // fp is the set on screen; fill what the last tap left in the box.
  var gen = 0, deadline = 0, timer = 0, fp = '', fill = null;
  // countable: the set answers a turn that ENDED while this view was open (an
  // open on an idle session is shown but not reported); seen: "shown" was
  // reported for the set on screen; inView: the row is in the viewport.
  var countable = false, seen = false, inView = false;
  var io = typeof IntersectionObserver !== 'undefined' ? new IntersectionObserver(function (es) {
    es.forEach(function (en) { inView = en.isIntersecting && en.intersectionRatio > 0; });
    seenNow();
  }, { threshold: [0, 0.5] }) : null;
  var DKEY = 'mv-suggest-dismissed';
  function dismissedSet(f) { try { return !!f && localStorage.getItem(DKEY) === f; } catch (e) { return false; } }
  function dismiss(f) { try { localStorage.setItem(DKEY, f); } catch (e) {} }
  function measure() { if (window.musterPinMeasure) window.musterPinMeasure(); }
  function state() { return row.getAttribute('data-state') || ''; }
  function hide() {
    gen++;
    clearTimeout(timer);
    deadline = 0; fp = '';
    if (io) io.unobserve(row);
    inView = false;
    if (row.hidden && !state()) return;
    row.hidden = true;
    row.removeAttribute('data-state');
    chips.textContent = '';
    measure();
  }
  // blocked: what the server excludes, seen on the page too — a question card
  // drawn, an attention ask (shown or hidden on this device: hidden is not
  // answered).
  function blocked() {
    var q = document.getElementById('mv-asks');
    if (q && !q.hidden && q.querySelector('form, button')) return true;
    var a = document.getElementById('asker-asks');
    if (a && a.querySelector('[id^="ask-"]')) return true;
    return false;
  }
  function post(ev, extra) {
    if (!fp && !(extra && extra.fp)) return;
    try {
      var b = new URLSearchParams(src.split('?')[1] || '');
      b.set('fingerprint', (extra && extra.fp) || fp);
      b.set('event', ev);
      if (extra && typeof extra.dist === 'number') b.set('dist', String(extra.dist));
      fetch(evURL, { method: 'POST', body: b, credentials: 'same-origin',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'X-Requested-With': 'XMLHttpRequest' } }).catch(function () {});
    } catch (e) {}
  }
  // reserve: a set is being made for a turn that ended ms ago. The row takes
  // its room now (empty, inert) and gives it back at the cutoff.
  function reserve(ms, open) {
    hide();
    countable = !open;
    if (blocked()) return;
    deadline = Date.now() + cutoff - (ms || 0);
    if (deadline <= Date.now()) { deadline = 0; return; }
    row.setAttribute('data-state', 'pending');
    row.hidden = false;
    measure();
    var g = gen;
    timer = setTimeout(function () { if (g === gen && state() === 'pending') hide(); }, deadline - Date.now());
  }
  function load() {
    if (state() !== 'pending') return;
    var g = gen;
    fetch(src, { credentials: 'same-origin', headers: { 'Accept': 'application/json' } })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (v) {
        if (g !== gen) return;
        var list = v && v.suggestions ? v.suggestions.slice(0, 3) : [];
        if (!list.length || Date.now() > deadline || blocked() || dismissedSet(v.fingerprint)) { hide(); return; }
        show(v.fingerprint, list);
      }, function () { if (g === gen) hide(); });
  }
  function show(f, list) {
    clearTimeout(timer);
    chips.textContent = '';
    fp = f;
    list.forEach(function (s, i) {
      var b = document.createElement('button');
      b.type = 'button';
      b.className = '` + suggestChipClass + `';
      b.setAttribute('data-suggest-chip', String(i));
      b.setAttribute('data-kind', s.kind || '');
      b.setAttribute('aria-label', 'AI-suggested reply: ' + s.text);
      b.title = s.text;
      var p = document.createElement('span');
      p.className = '` + suggestPillClass + `';
      p.textContent = s.text;
      b.appendChild(p);
      // detail 0: a keyboard activation (Enter, Space) — the focus stays on
      // the chip, so a held Enter cannot go on to send the box.
      b.addEventListener('click', function (e) { take(s.text, e.detail === 0 ? b : null); });
      // A held Enter or Space fills ONCE (a button repeats its activation).
      b.addEventListener('keydown', function (e) { if (e.repeat && (e.key === 'Enter' || e.key === ' ')) e.preventDefault(); });
      chips.appendChild(b);
    });
    row.setAttribute('data-state', 'shown');
    row.hidden = false;
    measure();
    seen = false;
    if (io) io.observe(row); else { inView = true; seenNow(); }
  }
  // seenNow reports "shown" once per set, and only when it was really seen:
  // the page visible AND the row in the viewport (a hidden tab, a row off
  // screen, are not an impression the auto-pause may count).
  function seenNow() {
    if (!countable || seen || !fp || state() !== 'shown' || !inView || document.visibilityState !== 'visible') return;
    seen = true;
    post('shown');
  }
  document.addEventListener('visibilitychange', seenNow);
  // take fills the box with text — below a draft, never over it — as an edit.
  // keyEl: the chip a keyboard activated, which gets the focus back.
  function take(text, keyEl) {
    var cur = input.value;
    var draft = cur.trim() !== '';
    var add = draft ? (/\n$/.test(cur) ? '' : '\n') + text : text;
    input.focus();
    input.setSelectionRange(draft ? cur.length : 0, cur.length);
    var done = false;
    try { done = !!(document.execCommand && document.execCommand('insertText', false, add)); } catch (e) {}
    if (!done || input.value === cur) {
      input.setRangeText(add, draft ? cur.length : 0, cur.length, 'end');
      input.dispatchEvent(new Event('input', { bubbles: true }));
    }
    var end = input.value.length;
    input.setSelectionRange(end, end);
    if (window.musterComposerFit) window.musterComposerFit(input);
    if (keyEl) keyEl.focus();
    fill = { fp: fp, after: input.value, countable: countable };
    if (countable) {
      // A tap is seen by definition.
      if (!seen) { seen = true; post('shown'); }
      post('filled');
    }
  }
  // dist is the edit distance between a and b (Levenshtein, over at most
  // 4000 characters of each): how much was changed after a tap. A number
  // is all that leaves the page.
  function dist(a, b) {
    a = a.slice(0, 4000); b = b.slice(0, 4000);
    if (a === b) return 0;
    var prev = new Array(b.length + 1), cur = new Array(b.length + 1), i, j;
    for (j = 0; j <= b.length; j++) prev[j] = j;
    for (i = 1; i <= a.length; i++) {
      cur[0] = i;
      for (j = 1; j <= b.length; j++) {
        cur[j] = Math.min(prev[j] + 1, cur[j - 1] + 1, prev[j - 1] + (a.charCodeAt(i - 1) === b.charCodeAt(j - 1) ? 0 : 1));
      }
      var t = prev; prev = cur; cur = t;
    }
    return prev[b.length];
  }
  x.addEventListener('click', function () {
    if (fp) dismiss(fp);
    hide();
    input.focus();
  });
  // A message went (Send or Queue, mirrorComposerScript): a new turn starts,
  // and a tap before it is reported with how much it was edited.
  document.addEventListener('mv-sent', function (e) {
    var sent = e.detail && typeof e.detail.text === 'string' ? e.detail.text : null;
    if (fill && fill.countable && sent !== null) post('sent', { fp: fill.fp, dist: dist(fill.after, sent) });
    fill = null;
    hide();
  });
  // A question card or an attention ask that arrives while chips are up
  // takes them away (the server's exclusions run only when a set is made or
  // read).
  if (typeof MutationObserver !== 'undefined') {
    var mo = new MutationObserver(function () { if (state() && blocked()) hide(); });
    ['mv-asks', 'asker-asks'].forEach(function (id) {
      var el = document.getElementById(id);
      if (el) mo.observe(el, { childList: true, subtree: true, attributes: true, attributeFilter: ['hidden'] });
    });
  }
  window.musterSuggest = function (m) {
    if (!m) return;
    if (m.state === 'pending') reserve(m.ms, m.open);
    else if (m.state === 'ready') load();
    else hide();
  };
  window.musterSuggestDist = dist;
})();
`))
}
