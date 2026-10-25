package ui

import (
	"encoding/json"
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// BACK AND FORWARD: what a page does when the browser shows it again from a
// cache instead of loading it (operator ask: "cache things better so that I
// don't have to wait for full reloads every time I hit the back button").
//
// The agent page, the mirrored Rendered view and both terminals are cacheable
// (internal/api viewDocument), so Back can RESTORE them from the back/forward
// cache: the same document, its script still running, shown in a frame. That
// is fast, and it is also a page that was asleep. Everything it holds is as old
// as the moment it was hidden, its sockets are gone, and the build it was
// rendered by may no longer be the one serving. restoreJS is the page's answer,
// and it runs on EVERY signed-in document (it is part of appScript):
//
//   - on pagehide, every stream the page holds is closed — the htmx event
//     streams here, the page's own sockets through its participants — so the
//     page is eligible for the cache and the server is not left holding them;
//   - on a restore (pageshow, persisted), a page hidden for longer than
//     RestoreMaxAwayMs reloads; otherwise it asks /health which build is
//     serving and reloads when that is not the page's own; it reopens every
//     stream, and each participant catches up (the chat log's conditional
//     refetch, a mirrored view's socket resuming from its offset, a terminal's
//     re-attach);
//   - a view page (one that draws an Updating… chip) re-requests ITSELF, as a
//     reload would have: the server records the visit and marks the session
//     read, the switcher's Pins and Recent are redrawn from the answer, and an
//     answer that is a refusal — 401, or a redirect to the sign-in — reloads
//     the page, so a page restored after its session ended shows the sign-in
//     (as does one whose agent or window is gone, or that another user's
//     sign-in now answers);
//   - "Updating…" shows in the page's reserved chip only if all of that takes
//     longer than RestoreChipDelayMs, so a fast catch-up shows nothing.
//
// 🔴 A HISTORY LOAD FROM THE HTTP CACHE GETS THE SAME CHECKS. A cacheable
// document is also kept in the HTTP cache, and Chromium takes it from there for
// a Back that is not a restore WITHOUT asking the server (measured: navigation
// type back_forward, transferSize 0, no request). That document's scripts run
// afresh, so its streams open by themselves; what it lacks is the build check
// and the refusal check, which it gets here.
//
// ⚠ ONLY THE TOP WINDOW CHECKS. The view swap draws the other view in a frame
// (view_swap.go); the frame is restored with the page and reconnects its own
// streams, but the build, the age and the refusal are decided once, by the
// page, whose reload takes the frame with it.

// RestoreMaxAwayMs is how long a page may sit in the back/forward cache and
// still be shown as it was: past it, a restore reloads. Chromium itself keeps
// an entry about this long; the bound is muster's, so it holds in any browser.
const RestoreMaxAwayMs = 10 * 60 * 1000

// RestoreChipDelayMs is how long a restore catches up before its Updating…
// chip shows.
const RestoreChipDelayMs = 1000

// RestoreHiddenKey is the sessionStorage entry a page records the moment it
// was hidden in, by document: {"<doc id>": <ms>}. The restored page reads its
// own and deletes it. A page that cannot reach storage keeps the time in
// memory, which is the same number.
const RestoreHiddenKey = "muster.restore.hidden"

// RestoreParticipantsVar is the window property a page's own script pushes its
// participant onto: {hide(event), show() → optional Promise (caught up),
// alive() → false once the page's script has torn itself down}. A list rather
// than a call, so a script that runs before appScript joins all the same.
const RestoreParticipantsVar = "__musterRestoreParts"

// RestoreRefreshAttr marks a region a restore redraws from the page's own
// fresh copy: the element of the fresh document with the same value replaces
// this one's children. Every such region keeps its size (the switcher's rows
// are a fixed h-11), so the redraw moves nothing.
const RestoreRefreshAttr = "data-restore-refresh"

// UpdatingChipAttr marks a page's Updating… chip (updatingChip). A page with
// one is a VIEW page, and a restore re-requests it.
const UpdatingChipAttr = "data-updating-chip"

// restoreJoinJS is the line a page's script uses to join: p is its participant.
func restoreJoinJS(p string) string {
	return "(window." + RestoreParticipantsVar + " = window." + RestoreParticipantsVar + " || []).push(" + p + ");"
}

// updatingChip is a view's "Updating…": shown only while a page that was shown
// from a cache (a restore) or painted from a snapshot (the Rendered view's)
// has been catching up for over a second.
//
// 🔴 IT TAKES NO LAYOUT AT ALL, AND ITS PLACE IS ALWAYS LAID OUT. It is
// absolutely positioned (pos: its placement inside a `relative` box), so it
// adds no width or height to the row it sits in, and it is `invisible`
// (visibility: hidden), never `hidden` (display: none), so showing it moves
// nothing and its box is where it shows.
func updatingChip(id, pos string) g.Node {
	return Span(ID(id), g.Attr(UpdatingChipAttr, ""), g.Attr("aria-hidden", "true"),
		Class("invisible absolute "+pos+" pointer-events-none z-10 whitespace-nowrap rounded-full bg-s2 px-2 py-0.5 text-[11px] text-fg2 ring-1 ring-inset ring-line"),
		g.Text("Updating…"))
}

// restoreJS is the restore handler (see the top of this file), as a part of
// appScript's IIFE. It binds once per window: appScript re-runs on a boosted
// swap, and the document it serves is still the same one.
func restoreJS(feat Features) string {
	build, _ := json.Marshal(feat.SnapBuild) // a JSON string is a valid JS string literal
	return `
  // --- Back/forward restore (restore.go) ------------------------------------
  if (!window.__musterRestore) {
    window.__musterRestore = (function () {
      var BUILD = ` + string(build) + `;
      var MAX_AWAY = ` + strconv.Itoa(RestoreMaxAwayMs) + `;
      var CHIP_DELAY = ` + strconv.Itoa(RestoreChipDelayMs) + `;
      var KEY = '` + RestoreHiddenKey + `';
      var DOC = Math.random().toString(36).slice(2) + Date.now().toString(36);
      var TOP = window.top === window;
      var hiddenAt = 0;
      // The htmx event streams this page opened, each with the element that
      // owns it: the sse extension names both on htmx:sseOpen.
      var streams = [];
      document.addEventListener('htmx:sseOpen', function (e) {
        var src = e.detail && e.detail.source;
        if (src && streams.every(function (s) { return s.src !== src; })) streams.push({ elt: e.target, src: src });
      });
      function parts() {
        return (window.` + RestoreParticipantsVar + ` || []).filter(function (p) {
          try { return !p.alive || p.alive(); } catch (e) { return false; }
        });
      }
      function store() { try { return window.sessionStorage; } catch (e) { return null; } }
      function readHidden() {
        var s = store(), m = {};
        if (!s) return m;
        try { m = JSON.parse(s.getItem(KEY) || '{}') || {}; } catch (e) { m = {}; }
        return m;
      }
      function writeHidden(m) {
        var s = store();
        if (!s) return;
        // Twenty documents at most: the oldest go first.
        var ids = Object.keys(m).sort(function (a, b) { return m[a] - m[b]; });
        while (ids.length > 20) delete m[ids.shift()];
        try { s.setItem(KEY, JSON.stringify(m)); } catch (e) {}
      }
      function chips(on) {
        document.querySelectorAll('[` + UpdatingChipAttr + `]').forEach(function (c) {
          c.classList.toggle('invisible', !on);
          c.setAttribute('aria-hidden', on ? 'false' : 'true');
        });
      }
      // reload: why is for a reader of this code; the page simply loads again.
      function reload(why) { void why; location.reload(); }
      // within: p, or nothing after ms — a catch-up that never answers
      // must not hold the chip up for ever.
      function within(p, ms) {
        return Promise.race([Promise.resolve(p).catch(function () {}), new Promise(function (r) { setTimeout(r, ms); })]);
      }
      // build: /health names the build serving; another than the page's own
      // is a deploy since the page was rendered.
      function build() {
        return fetch('/health', { cache: 'no-store', credentials: 'same-origin' })
          .then(function (r) { return r.ok ? r.json() : null; })
          .then(function (j) { if (j && j.version && j.version !== BUILD) reload('build'); })
          .catch(function () {});
      }
      // self: the page asks for itself again, as a reload would have. A
      // refusal reloads (to the sign-in); otherwise each refreshable region
      // is redrawn from the fresh copy.
      function self() {
        return fetch(location.href, { cache: 'no-store', credentials: 'same-origin', headers: { 'X-Requested-With': 'XMLHttpRequest' } })
          .then(function (r) {
            var path = '';
            try { path = new URL(r.url).pathname; } catch (e) {}
            if (r.status === 401 || r.status === 403 || path === '/login' || path.indexOf('/login/') === 0) { reload('refused'); return; }
            // Gone (an agent deleted, a window closed) or now somewhere else
            // (a redirect): what the page shows is not what is there. A
            // reload shows what is. Any other failure (a 5xx mid-deploy, the
            // network) keeps the page; its streams retry on their own.
            if (r.status === 404 || r.status === 410 || r.redirected) { reload('changed'); return; }
            if (!r.ok) return;
            return r.text().then(function (html) {
              var doc = new DOMParser().parseFromString(html, 'text/html');
              // Another person's page now (one device, a sign-in in between
              // that this browser did not purge for): never redraw theirs
              // into this one — load it.
              var mine = document.querySelector('script[data-snap-user]'), now = doc.querySelector('script[data-snap-user]');
              if (mine && now && mine.getAttribute('data-snap-user') !== now.getAttribute('data-snap-user')) { reload('user'); return; }
              document.querySelectorAll('[` + RestoreRefreshAttr + `]').forEach(function (el) {
                var k = el.getAttribute('` + RestoreRefreshAttr + `');
                var fresh = doc.querySelector('[` + RestoreRefreshAttr + `="' + (window.CSS && CSS.escape ? CSS.escape(k) : k) + '"]');
                if (fresh && fresh.innerHTML !== el.innerHTML) el.innerHTML = fresh.innerHTML;
              });
            });
          })
          .catch(function () {});
      }
      // reopen: every htmx event stream, by the extension's own reconnect
      // (its onerror on a closed source opens a new one and re-registers the
      // element's listeners), so the stream is the extension's as before.
      function reopen() {
        var was = streams; streams = [];
        was.forEach(function (s) {
          if (!document.body.contains(s.elt)) return;
          try { s.src.close(); } catch (e) {}
          try { if (s.src.onerror) s.src.onerror(new Event('error')); } catch (e) {}
        });
      }
      function catchUp(how) {
        var done = false;
        var t = setTimeout(function () { if (!done) chips(true); }, CHIP_DELAY);
        var waits = [];
        if (TOP) waits.push(build());
        if (TOP && document.querySelector('[` + UpdatingChipAttr + `]')) waits.push(self());
        if (how === 'restore') {
          reopen();
          // Whatever catches up on an event stream's (re)open catches up now
          // too, without waiting for the stream (the chat log's refetch).
          try { document.body.dispatchEvent(new CustomEvent('muster:restored')); } catch (e) {}
          parts().forEach(function (p) { try { if (p.show) waits.push(p.show()); } catch (e) {} });
        }
        return Promise.all(waits.map(function (w) { return within(w, 15000); })).then(function () {
          done = true;
          clearTimeout(t);
          chips(false);
        });
      }
      window.addEventListener('pagehide', function (e) {
        hiddenAt = Date.now();
        var m = readHidden(); m[DOC] = hiddenAt; writeHidden(m);
        streams.forEach(function (s) { try { s.src.close(); } catch (x) {} });
        parts().forEach(function (p) { try { if (p.hide) p.hide(e); } catch (x) {} });
      });
      window.addEventListener('pageshow', function (e) {
        if (!e.persisted) return;
        var m = readHidden(), at = m[DOC] || hiddenAt;
        delete m[DOC]; writeHidden(m);
        if (TOP && at && Date.now() - at > MAX_AWAY) { reload('away'); return; }
        catchUp('restore');
      });
      // A Back that was not a restore, answered by the HTTP cache: the
      // document is as old as its first load. Checked once it is parsed.
      try {
        var nav = performance.getEntriesByType('navigation')[0];
        if (TOP && nav && nav.type === 'back_forward' && nav.transferSize === 0 && nav.decodedBodySize > 0) {
          var go = function () { catchUp('http-cache'); };
          if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', go); else go();
        }
      } catch (e) {}
      return { doc: DOC };
    })();
  }
`
}
