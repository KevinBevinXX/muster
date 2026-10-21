package ui

import (
	"io"
	"net/url"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// ---------------------------------------------------------------------------
// THE INSTALLED-APP SURFACE: manifest link, service worker, update toast,
// install button, app badge, and the composer a share or shortcut opens.
// ---------------------------------------------------------------------------

// manifestLink is the <link rel=manifest> every document carries.
//
// 🔴 crossorigin="use-credentials" IS LOAD-BEARING, AND THE REASON IS BLINK, NOT
// THE EDGE. Chromium fetches the manifest in CORS mode with credentials OMITTED
// unless this attribute says otherwise — even same-origin. Without it the
// manifest request carries no session cookie at all. (This comment used to
// blame a Traefik basic-auth edge that answered 401; the public edge is
// Authelia forward-auth now, and it is configured to let GET/HEAD on exactly
// /manifest.webmanifest, /static/icons/*.png and /sw.js through without a
// session — the WebAPK icon hasher never sends cookies, so an icon behind the
// edge degrades the install to a plain shortcut. muster's side of that bargain
// is that those three responses never depend on a session and never carry
// per-user data; internal/api's TestPWAMetadataIsSessionIndependent pins it.)
func manifestLink() g.Node {
	return Link(Rel("manifest"), Href("/manifest.webmanifest"), g.Attr("crossorigin", "use-credentials"))
}

// pwaChrome is the per-document PWA furniture: the update toast and the
// script that owns the worker. Every document renders it, so an update is
// offered and the badge stays true wherever the operator happens to be.
func pwaChrome() g.Node {
	return g.Group{updateToast(), pwaScript()}
}

// updateToast is the "new version" prompt. Hidden until pwaScript sees a
// waiting worker; its Reload button hands control to that worker.
//
// 🔴 THE NEW WORKER WAITS FOR THIS BUTTON. sw.js no longer calls skipWaiting()
// at install, so an open page is never silently switched to a worker whose
// inline scripts it does not have. Reload is the operator's decision.
//
// 🔴 IT SITS AT THE TOP, UNDER THE STICKY HEADER — NOT AT THE BOTTOM. It used
// to float 5.5rem above the bottom edge, which on a phone is exactly where the
// composer, Send, the FAB and the chat button live: the operator reported it
// "blocking the UI". The class below is the fallback (just under the safe-area
// inset); pwaScript's placeToast moves it under whatever header the page has
// before it is shown, so it never covers the header's controls either. It is
// `fixed`, so it takes no layout space and showing it shifts nothing.
func updateToast() g.Node {
	return Div(
		ID("sw-update-toast"),
		g.Attr("role", "status"),
		g.Attr("aria-live", "polite"),
		g.Attr("hidden", ""),
		Class("fixed inset-x-0 top-[calc(env(safe-area-inset-top)+0.5rem)] z-50 mx-auto flex w-[calc(100%-2rem)] max-w-sm items-center gap-2 rounded-xl bg-s3 py-1.5 pl-4 pr-1.5 text-sm text-fg shadow-xl shadow-black/40 ring-1 ring-inset ring-edge"),
		Span(Class("flex-1"), g.Text("muster has been updated.")),
		Button(
			Type("button"),
			g.Attr("data-sw-reload", ""),
			Class("press inline-flex min-h-[44px] items-center justify-center rounded-lg bg-accent px-4 text-sm font-semibold text-on-accent transition hover:bg-accent/90"),
			g.Text("Reload"),
		),
		Button(
			Type("button"),
			g.Attr("data-sw-dismiss", ""),
			g.Attr("aria-label", "Dismiss"),
			Class("press inline-flex min-h-[44px] min-w-[44px] items-center justify-center rounded-lg text-muted transition hover:bg-s2"),
			g.Raw(`<svg class="h-4 w-4" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" aria-hidden="true"><path d="M4 4l8 8M12 4l-8 8"/></svg>`),
		),
	)
}

// installButton is the sidebar's "Install app" control. Hidden until the
// browser fires beforeinstallprompt (pwaScript), and never shown once the app
// runs in ANY installed display mode (pwaScript's installed()) or has been
// installed. It is rendered `hidden`, so a page whose script never runs — or
// runs inside the app — never shows it.
func installButton() g.Node {
	return Button(
		Type("button"),
		g.Attr("data-install-app", ""),
		g.Attr("hidden", ""),
		Class("press flex min-h-[44px] w-full items-center gap-3 rounded-lg px-3 text-left text-sm font-medium text-accent transition hover:bg-s2"),
		g.Raw(`<svg class="h-4 w-4 shrink-0" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M8 2v8m-3.5-3.5L8 10l3.5-3.5M3 13h10"/></svg>`),
		Span(g.Text("Install app")),
	)
}

// reviewCountPath is the session-gated count the desktop app badge reads.
const reviewCountPath = "/ui/tasks/review-count"

// AppBridgeName is the JavaScript object the Android app's WebView injects
// (WebViewCompat.addWebMessageListener), on muster's own origin only. Its
// presence is how a page knows it is inside the app; the page posts the
// pairing code and "open the app's notification settings" to it, and never
// receives a credential from it. The app repository uses the same name.
const AppBridgeName = "musterApp"

// pwaScript is the ONE service-worker registration site, plus the three things
// that hang off the registration: the update toast, the install button and the
// app badge. It is idempotent per page load (a boosted navigation re-runs body
// scripts) and looks elements up at use time, because a boosted swap replaces
// the toast and the button underneath it.
//
// ⚠ ORDER: it publishes window.musterSWReady, which pushScript consumes, so it
// renders before pushScript on the shell.
func pwaScript() g.Node {
	return Script(g.Raw(`
(function () {
  if (window.__musterPWA) return;
  window.__musterPWA = true;
  if (!('serviceWorker' in navigator)) return;

  // --- registration ---------------------------------------------------------
  // updateViaCache:'none' is explicit; the top-level script already bypasses
  // the HTTP cache by default. Every deploy changes sw.js's bytes (the server
  // stamps the build into it), so every deploy is an update.
  var registered = navigator.serviceWorker.register('/sw.js', { scope: '/', updateViaCache: 'none' });
  window.musterSWReady = registered;

  // --- update toast -----------------------------------------------------------
  var wantReload = false;
  var dismissed = null; // the waiting worker the operator dismissed the toast for
  // placeToast puts the toast just under the page's sticky header (or the
  // safe-area inset, when there is none). It runs BEFORE the toast is unhidden,
  // so the toast appears in its final place and nothing moves.
  function placeToast(t) {
    var h = document.querySelector('header');
    var below = h ? Math.max(0, Math.round(h.getBoundingClientRect().bottom)) : 0;
    t.style.top = below > 0 ? 'max(calc(env(safe-area-inset-top) + 0.5rem), ' + (below + 8) + 'px)' : '';
  }
  function offerUpdate(w) {
    if (w && w === dismissed) return;
    var t = document.getElementById('sw-update-toast');
    if (!t) return;
    if (t.hidden) placeToast(t);
    t.hidden = false;
  }
  window.addEventListener('resize', function () {
    var t = document.getElementById('sw-update-toast');
    if (t && !t.hidden) placeToast(t);
  });
  registered.then(function (reg) {
    if (reg.waiting && navigator.serviceWorker.controller) offerUpdate(reg.waiting);
    // A boosted navigation swaps the body, and the toast with it: the new one
    // arrives from the server hidden while the update is still waiting. Offer
    // it again whenever a waiting worker exists.
    document.addEventListener('htmx:load', function () {
      if (reg.waiting && navigator.serviceWorker.controller) offerUpdate(reg.waiting);
    });
    reg.addEventListener('updatefound', function () {
      var w = reg.installing;
      if (!w) return;
      w.addEventListener('statechange', function () {
        // installed + an existing controller = an UPDATE waiting. With no
        // controller this is the first install, which needs no prompt.
        if (w.state === 'installed' && navigator.serviceWorker.controller) offerUpdate(w);
      });
    });
    // An installed app resumes from the background rather than navigating, so
    // the browser's own navigation-time check rarely runs. Ask on every resume,
    // at most once a minute; a throttled resume does not move the window.
    var last = 0;
    document.addEventListener('visibilitychange', function () {
      if (document.visibilityState !== 'visible' || Date.now() - last < 60 * 1000) return;
      last = Date.now();
      reg.update().catch(function () {});
    });
  }).catch(function () {});
  // ONE reload, and only one the operator asked for: the first install's
  // clients.claim() also fires controllerchange, and must not reload the page.
  function reloadOnce() {
    if (!wantReload) return;
    wantReload = false;
    location.reload();
  }
  navigator.serviceWorker.addEventListener('controllerchange', reloadOnce);
  // 🔴 RELOAD ASKS THE REGISTRATION WHAT IS WAITING *NOW*, AND ALWAYS RELOADS.
  // This used to post SKIP_WAITING to the worker captured when the toast was
  // shown, and then wait for a controllerchange. When ANOTHER client — a second
  // tab, the installed app beside a browser tab — took the update first, that
  // worker was already the active one: skipWaiting() did nothing, no
  // controllerchange came, and the tap disabled the button and reloaded
  // nothing (e2e/tests/update.spec.ts, "another client took the update"). So:
  // nothing waiting -> reload straight away (the page is an old document under
  // the new worker); something waiting -> SKIP_WAITING and reload on
  // controllerchange, with a timed fallback so a worker that never activates
  // cannot leave the tap dead.
  var RELOAD_FALLBACK_MS = 3000;
  document.addEventListener('click', function (e) {
    var b = e.target && e.target.closest && e.target.closest('[data-sw-reload]');
    if (!b || wantReload) return;
    wantReload = true;
    b.disabled = true;
    setTimeout(reloadOnce, RELOAD_FALLBACK_MS);
    registered.then(function (reg) {
      if (!reg.waiting) return reloadOnce();
      reg.waiting.postMessage({ type: 'SKIP_WAITING' });
    }, reloadOnce);
  });
  document.addEventListener('click', function (e) {
    var b = e.target && e.target.closest && e.target.closest('[data-sw-dismiss]');
    if (!b) return;
    var t = document.getElementById('sw-update-toast');
    if (t) t.hidden = true;
    registered.then(function (reg) { dismissed = reg.waiting; }, function () {});
  });

  // --- install button -------------------------------------------------------
  var deferred = null;
  // installed() is the ONE "are we already the app" predicate. Every display
  // mode but 'browser' is an installed window: the manifest asks for
  // standalone, a browser that cannot do that falls back along the spec's
  // chain (standalone -> minimal-ui -> browser), and a desktop app window can
  // switch to window-controls-overlay at runtime. Asking only for standalone —
  // what this used to do — offered "Install app" inside the other three.
  // navigator.standalone is iOS Safari's own flag for a home-screen launch.
  var INSTALLED_MODES = ['standalone', 'fullscreen', 'minimal-ui', 'window-controls-overlay'];
  function installed() {
    if (navigator.standalone === true) return true;
    // Inside the Android app's WebView the page IS the installed app: never
    // offer Install there (docs/design/android.md, server change 24).
    if (window.` + AppBridgeName + `) return true;
    if (!window.matchMedia) return false;
    for (var i = 0; i < INSTALLED_MODES.length; i++) {
      if (window.matchMedia('(display-mode: ' + INSTALLED_MODES[i] + ')').matches) return true;
    }
    return false;
  }
  // 🔴 THE PREDICATE IS APPLIED HERE, ON EVERY SHOW, NOT ONLY WHEN THE EVENT
  // ARRIVES. A window's display mode can change after beforeinstallprompt (the
  // install itself moves a desktop tab into an app window), and every caller
  // that turns the button on goes through this one function.
  function showInstall(on) {
    on = on && !installed();
    var bs = document.querySelectorAll('[data-install-app]');
    for (var i = 0; i < bs.length; i++) bs[i].hidden = !on;
  }
  window.addEventListener('beforeinstallprompt', function (e) {
    e.preventDefault();
    if (installed() || !window.isSecureContext) return;
    deferred = e;
    showInstall(true);
  });
  window.addEventListener('appinstalled', function () { deferred = null; showInstall(false); });
  document.addEventListener('click', function (e) {
    var b = e.target && e.target.closest && e.target.closest('[data-install-app]');
    if (!b || !deferred) return;
    var ev = deferred;
    deferred = null;
    showInstall(false);
    ev.prompt();
    if (ev.userChoice) ev.userChoice.catch(function () {});
  });
  document.addEventListener('htmx:load', function () { showInstall(!!deferred); });

  // --- app badge (desktop / ChromeOS) ---------------------------------------
  // Where setAppBadge exists, show the ready-for-review count. Android has no
  // badging API (its icon dot is the unread-notification count) and muster does
  // not clear that dot: that needs muster-native Web Push, a separate arc.
  if ('setAppBadge' in navigator) {
    var timer = null;
    var refresh = function () {
      fetch('` + reviewCountPath + `', { credentials: 'include', headers: { Accept: 'application/json' } })
        .then(function (r) { return r.ok ? r.json() : null; })
        .then(function (d) {
          if (!d || typeof d.count !== 'number') return;
          if (d.count > 0) navigator.setAppBadge(d.count).catch(function () {});
          else navigator.clearAppBadge().catch(function () {});
        })
        .catch(function () {});
    };
    var soon = function () { clearTimeout(timer); timer = setTimeout(refresh, 2000); };
    refresh();
    document.addEventListener('visibilitychange', function () { if (document.visibilityState === 'visible') soon(); });
    // task.changed carries only an id; the count is re-read rather than pushed
    // so no write path pays for a count it does not need.
    document.addEventListener('sse:task.changed', soon);
  }
})();
`))
}

// ---------------------------------------------------------------------------
// THE COMPOSER, OPENED BY A URL.
// ---------------------------------------------------------------------------

// composeMaxBytes bounds a pre-filled task body. A share can carry a whole
// page of text; the composer is for a sentence and a link.
const composeMaxBytes = 8 << 10

// ClampComposeBody cuts s to composeMaxBytes without splitting a UTF-8
// sequence.
func ClampComposeBody(s string) string {
	if len(s) <= composeMaxBytes {
		return s
	}
	cut := composeMaxBytes
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// ComposeURL is the new-task sheet's body URL with an optional pre-filled
// body. The value is query-escaped here and attribute-escaped by gomponents.
func ComposeURL(body string) string {
	if body == "" {
		return "/ui/tasks/new"
	}
	return "/ui/tasks/new?" + url.Values{"body": {ClampComposeBody(body)}}.Encode()
}

// ComposePage is the tasks shell with the new-task sheet opening itself on
// load, pre-filled with body. It is what GET /share and the "New task"
// shortcut (/tasks?new=1) serve.
//
// 🔴 IT CREATES NOTHING. Opening the page only fills the form; the task is
// written by the operator pressing Save, through the SAME POST /tasks every
// other create uses — one write path, one session check, one broadcast. A GET
// that wrote would be a create-by-link.
func ComposePage(feat Features, body string) g.Node {
	return Doctype(shell("tasks", feat, composeOnLoad(body), ShellData{}))
}

// RenderComposePage writes ComposePage.
func RenderComposePage(w io.Writer, feat Features, body string) error {
	return ComposePage(feat, body).Render(w)
}

// composeOnLoad opens the new-task sheet once the page's scripts are ready,
// fetches its body from ComposeURL, and rewrites the address bar to /tasks so a
// reload (or the back button) does not reopen it.
func composeOnLoad(body string) g.Node {
	return g.Group{
		Div(ID("compose-on-load"), g.Attr("hidden", ""), g.Attr("data-compose-url", ComposeURL(body))),
		Script(g.Raw(`
(function () {
  var tries = 0;
  function go() {
    var el = document.getElementById('compose-on-load');
    if (!el) return;
    if (!window.htmx || !window.cgModalShow) { if (++tries < 100) setTimeout(go, 50); return; }
    var u = el.getAttribute('data-compose-url');
    el.remove();
    // 🔴 THE SHEET OPENS WHEN ITS BODY HAS LANDED, NOT BEFORE. Opened first, an
    // empty sheet appeared and then grew to the form's height — and a centred
    // sheet growing moves its top: a layout shift of 0.045 at 1280×800, with no
    // tap to excuse it (e2e/tests/cls.spec.ts). Opened after, it appears whole.
    // A failed load still opens it, so the failure is seen rather than nothing.
    // cgModalShow, NOT cgModalOpen: Open empties the body, which here is the
    // composer that was just loaded into it.
    var open = function () { window.cgModalShow('task-modal'); };
    var req = window.htmx.ajax('GET', u, { target: '#task-modal-body', swap: 'innerHTML' });
    if (req && typeof req.then === 'function') req.then(open, open); else open();
    try { history.replaceState(history.state, '', '/tasks'); } catch (e) {}
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', go); else go();
})();
`)),
	}
}
