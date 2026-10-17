/*
 * muster service worker.
 *
 * Served from "/sw.js" (root scope) so it controls the whole origin, with the
 * running build stamped into BUILD below by the server (internal/api/pwa.go) —
 * so every deploy changes these bytes and is offered to open pages as an
 * update. It does four jobs:
 *   1. Provide a minimal offline app shell (cache the root document + CSS so
 *      the UI still opens with no network; live data still needs the network
 *      and degrades gracefully). 🔴 Served ONLY while navigator.onLine is
 *      false — see the navigate branch in the fetch handler for why that gate
 *      is load-bearing rather than defensive.
 *   2. Handle Web Push: render a notification, and close stale ones on a
 *      "resolved" control message.
 *   3. Handle notification clicks: focus/open the app at the relevant card.
 *   4. Wait to be told. A new worker does NOT take over at install; it waits
 *      until the page's "Reload" (pwaScript) posts SKIP_WAITING, so an open
 *      page is never switched under its own inline scripts.
 *
 * 🔴 THIS IS NOT THE UPSTREAM WORKER, AND ONE BRANCH IS DELIBERATELY ABSENT.
 * Upstream this file also decides permission requests, by POSTing to the
 * permission router's decision route. That route is on the `router` side of the
 * route partition manifest — muster does not register it and never will.
 * Carrying the branch across would give the worker a button that POSTs to a
 * 404: a tap that reports nothing, changes nothing, and reads as "the approval
 * did not go through" with no error anywhere. (A privilege-request decision
 * branch was carried here until the privilege subsystem was removed; with it
 * gone NO push this worker renders carries an action button.)
 *
 * ⚠ THE DECISION ROUTE'S PATH IS DELIBERATELY NOT SPELLED ANYWHERE IN THIS
 * FILE. A test asserts this file does not contain it, and a string check on
 * JavaScript cannot tell a comment from a fetch — so naming it here to explain
 * its absence would red the guard from inside the explanation.
 *
 * Fetches use credentials:"include" so the session cookie rides along.
 */

'use strict';

// Replaced by the server with the build version (a JSON string literal). The
// placeholder must appear exactly once; the server refuses to serve otherwise.
const BUILD = "__MUSTER_BUILD__";
// Per-build shell cache, so each deploy refreshes the offline shell instead of
// it living forever. activate carries forward what this build could not fetch
// and only then deletes the older shell caches.
const CACHE_PREFIX = 'muster-shell-';
const CACHE = CACHE_PREFIX + BUILD;
// The stylesheet's URL as the pages link it: /static/app.css?v=<content hash>,
// replaced by the server like BUILD (exactly once). The shell precaches THAT
// URL, so it holds this build's stylesheet under the key the cached pages ask
// for — the bare /static/app.css could come back from an edge cache that still
// holds the last release's copy.
const APP_CSS = "__MUSTER_APP_CSS__";
// The minimal shell: the document and the compiled CSS. Vendored JS is fetched
// fresh (it is small and benefits from normal HTTP caching) to avoid serving a
// stale htmx during development.
const SHELL = ['/', APP_CSS, '/manifest.webmanifest'];

// usable reports whether a response may be stored as (or kept as) a shell
// entry: a same-origin 200 that was not reached through a redirect. A signed-out
// worker fetch of "/" is refused (401 from muster, a redirect to a login portal
// from a forward-auth edge); neither may become the shell. The install fetches
// with redirect:'manual', so a redirect arrives as an opaqueredirect (status 0)
// and fails here; `redirected` also rejects a followed redirect already sitting
// in an older cache.
function usable(res) {
  return !!res && res.status === 200 && res.type === 'basic' && !res.redirected;
}

self.addEventListener('install', (event) => {
  // Each entry is fetched and judged on its own: one refused entry costs that
  // entry, not the others, and never fails the install (push keeps working).
  event.waitUntil(
    caches.open(CACHE).then((cache) =>
      Promise.all(
        SHELL.map((path) =>
          fetch(path, { redirect: 'manual' })
            .then((res) => (usable(res) ? cache.put(path, res) : undefined))
            .catch(() => undefined)
        )
      )
    )
  );
  // 🔴 NO SKIP-WAITING CALL HERE. It used to be unconditional, so a deploy swapped
  // the worker under every open page silently. The page offers the update and
  // the operator's Reload sends SKIP_WAITING (the message handler below).
});

self.addEventListener('message', (event) => {
  const msg = event.data || {};
  if (msg.type === 'SKIP_WAITING') {
    self.skipWaiting();
    return;
  }
  // Lets a page (and the e2e suite) ask which build is in control.
  if (msg.type === 'GET_VERSION' && event.ports && event.ports[0]) {
    event.ports[0].postMessage({ build: BUILD });
  }
});

// 🔴 CARRY FORWARD, THEN DELETE. A deploy installed while signed out cannot
// fetch "/", so this build's cache has no shell document. Deleting the older
// caches at that point used to leave the device with NO offline shell at all.
// Each SHELL entry this build lacks is copied from an older shell cache that
// holds a usable one (in reverse caches.keys() order) — and so is the
// stylesheet the shell document it ends up with LINKS (shellStylesheet): a
// carried "/" from an older build links that build's /static/app.css?v=<old
// hash> (or the bare path, from before the hash), which is not this build's
// APP_CSS. Only then are the older shell caches deleted.
//
// ⚠ WHY CARRY THE OLD STYLESHEET, NOT MATCH ANY (ignoreSearch). Offline, the
// fallback below matches the EXACT URL the document asks for, so an older
// document gets the stylesheet it was built against. An ignoreSearch match
// would hand it whichever app.css the cache holds — this build's, newer than
// the document — and a class the older markup uses but the newer build no
// longer emits would silently go unstyled.
// shellStylesheet is the /static/app.css URL (with its ?v=, if any) that the
// cached shell document in cache links, or null.
async function shellStylesheet(cache) {
  const doc = await cache.match('/');
  if (!usable(doc)) return null;
  const m = (await doc.clone().text()).match(/href="(\/static\/app\.css[^"]*)"/);
  return m ? m[1].replace(/&amp;/g, '&') : null;
}

self.addEventListener('activate', (event) => {
  event.waitUntil(
    (async () => {
      const cache = await caches.open(CACHE);
      const older = (await caches.keys()).filter((k) => k !== CACHE && k.startsWith(CACHE_PREFIX)).reverse();
      for (const path of SHELL) {
        if (usable(await cache.match(path))) continue;
        for (const k of older) {
          const res = await (await caches.open(k)).match(path);
          if (usable(res)) {
            await cache.put(path, res);
            break;
          }
        }
      }
      const css = await shellStylesheet(cache);
      if (css && !usable(await cache.match(css))) {
        for (const k of older) {
          const res = await (await caches.open(k)).match(css);
          if (usable(res)) {
            await cache.put(css, res);
            break;
          }
        }
      }
      await Promise.all(older.map((k) => caches.delete(k)));
      await self.clients.claim();
    })()
  );
});

// Navigations are handled by the BROWSER unless the device is known to be
// offline (see below). app.css is network-first (with a cache fallback + cache
// refresh) — it must never be served stale, because each UI change ships a
// freshly-compiled Tailwind app.css with new classes; a cache-first strategy
// here silently breaks every new feature's styling for returning users. Other
// requests pass through to the network.
self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET') return;

  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return;

  // Never intercept the SSE stream or API calls — they must hit the network.
  if (url.pathname === '/events' || url.pathname.startsWith('/api/')) return;

  if (req.mode === 'navigate') {
    // 🔴 THE OFFLINE SHELL IS ONLY EVER SERVED WHEN THE DEVICE IS OFFLINE, AND
    // THAT GATE IS THE WHOLE POINT OF THIS BRANCH.
    //
    // This used to be `fetch(req).catch(() => caches.match('/'))` — a bare
    // catch, with "the fetch rejected" standing in for "we are offline". Those
    // are NOT the same thing, and the difference is a dead app.
    //
    // When the app sits behind a forward-auth edge, a top-level navigation with
    // an expired session gets a CROSS-ORIGIN 302 to the login portal. Upstream
    // measured a stuck tab reporting deliveryType "cache-storage" and
    // transferSize 0 for the document while its SUBRESOURCES reached the portal
    // in the same second — i.e. the network was up and only the document came
    // from this cache. The browser therefore never followed the 302; the app
    // rendered as if logged in, every subsequent script load was answered with
    // a cross-origin redirect, and a `script-src 'self'` CSP blocked it. A dead
    // page that looks authorised.
    //
    // ⚠ WHY the fetch took the catch branch was NOT established — do not write a
    // mechanism into this comment. What is established is the OBSERVABLE, and
    // this gate makes the observable impossible regardless of the mechanism:
    // while the device is online the worker does not touch the navigation at
    // all, so the browser performs it natively and follows any redirect —
    // including the one to the auth portal.
    //
    // navigator.onLine is used only in the direction it is trustworthy: `false`
    // means the browser has no network route at all. A wrongly-true reading
    // costs the offline shell (degraded); a wrongly-served shell costs the whole
    // app (broken). Fail in the cheap direction.
    if (navigator.onLine !== false) return;
    event.respondWith(caches.match('/').then((r) => r || fetch(req)));
    return;
  }

  if (url.pathname === '/static/app.css') {
    event.respondWith(
      fetch(req)
        .then((res) => {
          // Refresh the cached copy for offline use, then serve the fresh one.
          const copy = res.clone();
          caches.open(CACHE).then((c) => c.put(req, copy)).catch(() => undefined);
          return res;
        })
        .catch(() => caches.match(req))
    );
  }
});

// --- Web Push ---

self.addEventListener('push', (event) => {
  let payload = {};
  if (event.data) {
    try {
      payload = event.data.json();
    } catch (e) {
      payload = { title: 'muster', body: event.data.text() };
    }
  }

  // A "resolved" control message: close any open notification for this id on
  // this device (it was decided elsewhere). Render nothing.
  if (payload.type === 'resolved') {
    event.waitUntil(closeByTag(payload.tag || payload.id));
    return;
  }

  // A "notify" message: a fire-and-forget, informational notification. It is
  // NOT a decision prompt, so render a plain notification with no action
  // buttons and let it auto-clear (requireInteraction: false). A body tap just
  // opens the app.
  if (payload.type === 'notify') {
    const data = Object.assign({ type: 'notify' }, payload.data || {});
    if (!data.url) data.url = '/';
    event.waitUntil(
      self.registration.showNotification(payload.title || 'muster', {
        body: payload.body || '',
        icon: '/static/icons/icon-192.png',
        badge: '/static/icons/badge-72.png',
        tag: payload.tag || undefined,
        data: data,
        requireInteraction: false,
      })
    );
    return;
  }

  const title = payload.title || 'muster';
  const tag = payload.tag || payload.id || 'muster';
  // Always carry type + id on the notification data. payload.data may
  // add/override fields — a task push sets url:'/tasks/<id>' in the common case (the server derives it from the task
  // id), falling back to '/tasks' when there is no id (the coalesced
  // N-tasks-created push) and to an explicit '/agents/<name>' for the
  // agent-ready push.
  const data = Object.assign({ type: payload.type, id: payload.id }, payload.data || {});
  if (!data.url) data.url = '/#' + (payload.id || '');
  // Every push here is informational (task created / agent ready / ready for
  // review / connector request): title + body, no action buttons, not pinned —
  // a body tap focuses/opens data.url. This service has no decision route a
  // button could POST to.
  const options = {
    body: payload.body || '',
    icon: '/static/icons/icon-192.png',
    badge: '/static/icons/badge-72.png',
    tag: tag,
    data: data,
    requireInteraction: false,
    actions: [],
  };
  event.waitUntil(self.registration.showNotification(title, options));
});

async function closeByTag(tag) {
  if (!tag) return;
  const notes = await self.registration.getNotifications({ tag });
  notes.forEach((n) => n.close());
}

// --- Notification clicks ---

self.addEventListener('notificationclick', (event) => {
  const note = event.notification;
  const data = note.data || {};
  const id = data.id || note.tag;
  note.close();

  // An action button (only a notification shown by an OLDER worker build can
  // still carry one) has no decision endpoint on this service. Open the app
  // rather than POSTing somewhere that would 404.
  if (event.action) {
    event.waitUntil(focusOrOpen(data.url || '/'));
    return;
  }

  // Body tap: bring the app forward (or open it) at this card.
  const target = (data.url || '/#' + (id || '')).toString();
  event.waitUntil(focusOrOpen(target));
});

async function focusOrOpen(target) {
  const all = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
  for (const client of all) {
    // Reuse any open tab.
    if ('focus' in client) {
      await client.focus();
      if ('navigate' in client && target) {
        try {
          await client.navigate(target);
        } catch (e) {
          /* cross-origin or not allowed; focus is enough */
        }
      }
      return;
    }
  }
  if (self.clients.openWindow) {
    await self.clients.openWindow(target || '/');
  }
}
