import type { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { hookToken, updatePort, versionA, versionB } from '../lib/env';
import { startServer, Running } from '../lib/server';

// A deploy, end to end: build A serves the page, build B replaces it on the
// same origin, and the open page offers the update without taking it.
//
// 🔴 TWO REAL BUILDS, NOT A ROUTED SCRIPT. Playwright cannot route() a service
// worker's update fetch, and the property under test is that DIFFERENT BUILDS
// serve different worker bytes — so the spec runs both binaries global-setup
// built, on a port of its own (a fresh origin, so no other spec's worker is
// involved).

let server: Running | undefined;
test.afterEach(async () => {
  await server?.stop();
  server = undefined;
});

// Asks the CONTROLLING worker which build it is. It times out to null: a
// message posted to a controller that is being replaced can go unanswered (it
// turns redundant mid-question), and a poll that hangs on one call never asks
// again.
const controllerBuild = `(async () => {
  const c = navigator.serviceWorker.controller;
  if (!c) return null;
  return await new Promise((resolve) => {
    const ch = new MessageChannel();
    const t = setTimeout(() => resolve(null), 1000);
    ch.port1.onmessage = (e) => { clearTimeout(t); resolve(e.data.build); };
    c.postMessage({ type: 'GET_VERSION' }, [ch.port2]);
  });
})()`;

// Every cache on the origin, and what its "/" entry is. `shell` reads the BODY:
// the authenticated document renders the update toast, the login page does
// not, so a login page stored under "/" reads as shell:false even at 200.
const shellCaches = `(async () => {
  const out = {};
  for (const k of await caches.keys()) {
    const r = await (await caches.open(k)).match('/');
    out[k] = r ? { status: r.status, type: r.type, redirected: r.redirected, shell: (await r.text()).includes('id="sw-update-toast"') } : null;
  }
  return out;
})()`;
const goodShell = { status: 200, type: 'basic', redirected: false, shell: true };

async function waitControlled(page: Page): Promise<void> {
  await page.evaluate(async () => {
    await navigator.serviceWorker.ready;
    if (!navigator.serviceWorker.controller) {
      await new Promise((r) => navigator.serviceWorker.addEventListener('controllerchange', r, { once: true }));
    }
  });
}

// A deploy: the same origin starts answering with build B.
async function deployB(): Promise<void> {
  await server!.stop();
  server = await startServer(versionB, updatePort);
}

// The waiting worker's state, or null when nothing waits.
const waitingState = `(async () => {
  const reg = await navigator.serviceWorker.getRegistration();
  return reg && reg.waiting ? reg.waiting.state : null;
})()`;

const checkForUpdate = async () => {
  const reg = await navigator.serviceWorker.getRegistration();
  await reg!.update();
};

test('a new build is offered, waits for Reload, and Reload takes it exactly once', async ({ page }) => {
  server = await startServer(versionA, updatePort);
  const origin = `http://localhost:${updatePort}`;
  await page.addInitScript(() => {
    sessionStorage.setItem('loads', String(Number(sessionStorage.getItem('loads') || '0') + 1));
  });
  await page.goto(origin + '/tasks');
  await page.evaluate(async () => {
    await navigator.serviceWorker.ready;
    if (!navigator.serviceWorker.controller) {
      await new Promise((r) => navigator.serviceWorker.addEventListener('controllerchange', r, { once: true }));
    }
  });
  expect(await page.evaluate(controllerBuild)).toBe(versionA);
  // A first install offers nothing.
  await expect(page.locator('#sw-update-toast')).toBeHidden();

  // Deploy: same origin, new build.
  await server.stop();
  server = await startServer(versionB, updatePort);
  await page.evaluate(async () => {
    const reg = await navigator.serviceWorker.getRegistration();
    await reg!.update();
  });

  await expect(page.locator('#sw-update-toast')).toBeVisible();
  // 🔴 The new worker WAITS: the page is still controlled by build A, and the
  // registration still holds B as an installed, waiting worker. (A worker that
  // called skipWaiting() at install would be activating, not waiting.)
  expect(await page.evaluate(waitingState)).toBe('installed');
  expect(await page.evaluate(controllerBuild)).toBe(versionA);

  const loadsBefore = Number(await page.evaluate(() => sessionStorage.getItem('loads')));
  await page.locator('[data-sw-reload]').click();
  // The reload this click causes destroys the page's context mid-evaluate;
  // that is the expected event, so a destroyed context reads as "not yet".
  await expect
    .poll(() => page.evaluate(controllerBuild).catch(() => null), { timeout: 15_000 })
    .toBe(versionB);
  await page.waitForLoadState('load');
  // No sleep to "let a second reload happen": once the page is controlled by B
  // with NOTHING waiting, no reload source is left (the fallback timer died
  // with the old document, and no further controllerchange can come).
  expect(await page.evaluate(waitingState)).toBeNull();
  expect(Number(await page.evaluate(() => sessionStorage.getItem('loads'))), 'exactly one reload').toBe(loadsBefore + 1);
  await expect(page.locator('#sw-update-toast')).toBeHidden();
  // Per-build cache naming: the new build's shell cache replaced the old one.
  await expect.poll(() => page.evaluate(shellCaches)).toEqual({ ['muster-shell-' + versionB]: goodShell });
});

// F1: an htmx in-app navigation swaps the body — the toast included, which
// comes back from the server hidden. The update is still waiting, so the toast
// must still be offered after the swap.
test('the update toast survives an in-app navigation while the update waits', async ({ page }) => {
  server = await startServer(versionA, updatePort);
  const origin = `http://localhost:${updatePort}`;
  const r = await page.request.get(origin + '/api/tasks', { headers: { Authorization: `Bearer ${hookToken}` } });
  expect(r.status()).toBe(200);
  const tasks = (await r.json()) as { id: number }[];
  expect(tasks.length).toBeGreaterThan(0);
  await page.addInitScript(() => {
    sessionStorage.setItem('loads', String(Number(sessionStorage.getItem('loads') || '0') + 1));
  });
  await page.goto(`${origin}/tasks/${tasks[0].id}`);
  await waitControlled(page);
  await deployB();
  await page.evaluate(checkForUpdate);
  const toast = page.locator('#sw-update-toast');
  await expect(toast).toBeVisible();

  const loadsBefore = Number(await page.evaluate(() => sessionStorage.getItem('loads')));
  await page.click('#sidebar-open');
  await page.locator('#sidebar a[data-tab="agents"]').click();
  await expect(page).toHaveURL(origin + '/agents');
  await expect(page.locator('#sidebar a[data-tab="agents"][aria-current="page"]')).toHaveCount(1);
  // 🔴 The navigation was a swap, not a document load: a full load would
  // re-offer the update from the registration-time check and prove nothing.
  expect(Number(await page.evaluate(() => sessionStorage.getItem('loads'))), 'no full reload').toBe(loadsBefore);
  await expect(toast).toBeVisible();
});

// F2: a deploy installed while the page has NO session. The new worker's
// precache of "/" is refused (401 to a worker fetch), and that must not cost
// the offline shell the previous build cached.
test('an update installed while signed out keeps the offline shell', async ({ page, context }) => {
  server = await startServer(versionA, updatePort);
  const origin = `http://localhost:${updatePort}`;
  await page.goto(origin + '/tasks');
  await waitControlled(page);
  // Per-build naming, and a real shell, from a signed-in install.
  await expect.poll(() => page.evaluate(shellCaches)).toEqual({ ['muster-shell-' + versionA]: goodShell });

  await context.clearCookies();
  await deployB();
  // Positive control on the premise: signed out, "/" really is refused.
  expect((await page.request.get(origin + '/', { maxRedirects: 0 })).status()).not.toBe(200);
  await page.evaluate(checkForUpdate);
  await expect(page.locator('#sw-update-toast')).toBeVisible();
  await page.locator('[data-sw-reload]').click();
  await expect
    .poll(() => page.evaluate(controllerBuild).catch(() => null), { timeout: 15_000 })
    .toBe(versionB);
  // The new build's cache holds a usable shell, carried forward from A.
  await expect.poll(() => page.evaluate(shellCaches).catch(() => null)).toEqual({ ['muster-shell-' + versionB]: goodShell });
});

// 🔴 A REGRESSION TEST (#126 audit F1): the carried shell keeps its stylesheet.
// A deploy installed while signed out carries the older build's "/" forward —
// and that document links the OLDER build's /static/app.css?v=<old hash>, a key
// only the older cache held. Activate deleted that cache, the offline fallback
// matches the exact URL, and the offline shell rendered unstyled.
//
// Both e2e builds compile the same app.css, so the older build's hash is made
// here: build A's worker is handed a "/" that links ?v=0ld0ld0ld0ld (its own
// precache fetch, routed), and the page then asks for that stylesheet through
// the worker, which caches it under that URL — the cache an older release
// leaves behind.
test('an update installed while signed out keeps the offline shell styled', async ({ page, context }) => {
  server = await startServer(versionA, updatePort);
  const origin = `http://localhost:${updatePort}`;
  const oldCSS = '/static/app.css?v=0ld0ld0ld0ld';
  let routed = 0;
  await context.route(origin + '/', async (route) => {
    if (!route.request().serviceWorker()) return route.continue();
    const res = await route.fetch();
    const html = await res.text();
    routed++;
    return route.fulfill({ response: res, body: html.replace(/\/static\/app\.css\?v=[0-9a-f]+/, oldCSS) });
  });
  await page.goto(origin + '/tasks');
  await waitControlled(page);
  await expect.poll(() => page.evaluate(shellCaches)).toEqual({ ['muster-shell-' + versionA]: goodShell });
  expect(routed, "control: the worker's precache of / was the routed one").toBeGreaterThan(0);
  await context.unroute(origin + '/');
  // A's shell links the old stylesheet, and A's cache holds it under that URL.
  const links = await page.evaluate(async ([build, old]) => {
    await fetch(old);
    const doc = await (await caches.open('muster-shell-' + build)).match('/');
    return (await doc!.text()).includes('href="' + old + '"');
  }, [versionA, oldCSS]);
  expect(links, "A's shell links the old stylesheet").toBe(true);
  // ⚠ POLLED, NOT READ ONCE: the worker answers the fetch first and writes
  // the cache after (sw.js's app.css handler does not wait for its put), so a
  // read right after the fetch can land before the write. That was this
  // spec's flake: {cached: false} in 547 ms on CI, green on re-run.
  await expect
    .poll(
      () => page.evaluate(async ([build, old]) => !!(await (await caches.open('muster-shell-' + build)).match(old)), [versionA, oldCSS]),
      { message: "A's cache holds the old stylesheet under its URL" },
    )
    .toBe(true);

  await context.clearCookies();
  await deployB();
  await page.evaluate(checkForUpdate);
  await expect(page.locator('#sw-update-toast')).toBeVisible();
  await page.locator('[data-sw-reload]').click();
  await expect
    .poll(() => page.evaluate(controllerBuild).catch(() => null), { timeout: 15_000 })
    .toBe(versionB);
  await expect.poll(() => page.evaluate(shellCaches).catch(() => null)).toEqual({ ['muster-shell-' + versionB]: goodShell });

  // Offline: the carried "/" (linking the OLD stylesheet) is the page, and its
  // stylesheet loads — from the carried entry, not the network. The browser's
  // HTTP cache is cleared first: it also holds that stylesheet (the fetch
  // above).
  // And the SERVER is stopped: the worker's own fetch is not reliably cut by
  // the page's offline emulation (measured: with it alone, the no-carry mutant
  // still read styled), so the network answer is taken away for real.
  const cdp = await context.newCDPSession(page);
  await cdp.send('Network.clearBrowserCache');
  await server!.stop();
  server = undefined;
  await context.setOffline(true);
  await page.goto(origin + '/agents');
  const sheet = await page.evaluate(() => {
    const l = document.querySelector('link[rel="stylesheet"][href*="/static/app.css"]') as HTMLLinkElement | null;
    let rules = -1;
    try { rules = l?.sheet ? l.sheet.cssRules.length : 0; } catch { rules = -2; }
    return { href: l?.getAttribute('href') ?? null, rules };
  });
  expect(sheet.href, 'the carried shell links the older stylesheet').toBe(oldCSS);
  expect(sheet.rules, 'the offline shell is styled').toBeGreaterThan(0);
  await context.setOffline(false);
});

// F2, the edge's shape: behind a forward-auth edge an expired session's "/" is
// a REDIRECT to a login page that answers 200. A followed redirect must not be
// cached as the shell. The route answers only the WORKER's fetch of "/", so the
// page itself stays signed in.
//
// ⚠ THE STAND-IN LOGIN PAGE IS /manifest.webmanifest: a real same-origin 200
// that is not the app document. Measured: the request a worker makes when it
// follows a route-fulfilled redirect is NOT routed again: a routed stand-in
// page counted 0 hits and "/" was simply not cached, even with the redirect
// handling mutated away — a test blind to the thing it names. Redirecting to a
// path the real server answers 200 is what made that mutant go red.
// muster's own /login would not do either: signed in, it redirects to the app.
test('a redirected "/" is never cached as the shell', async ({ page, context }) => {
  server = await startServer(versionA, updatePort);
  const origin = `http://localhost:${updatePort}`;
  await page.goto(origin + '/tasks');
  await waitControlled(page);
  await expect.poll(() => page.evaluate(shellCaches)).toEqual({ ['muster-shell-' + versionA]: goodShell });

  let redirected = 0;
  await context.route(origin + '/', (route) => {
    if (!route.request().serviceWorker()) return route.continue();
    redirected++;
    return route.fulfill({ status: 302, headers: { location: origin + '/manifest.webmanifest' } });
  });
  await deployB();
  await page.evaluate(checkForUpdate);
  await expect(page.locator('#sw-update-toast')).toBeVisible();
  // Positive control: the route really answered the new worker's precache.
  expect(redirected).toBeGreaterThan(0);
  await page.locator('[data-sw-reload]').click();
  await expect
    .poll(() => page.evaluate(controllerBuild).catch(() => null), { timeout: 15_000 })
    .toBe(versionB);
  await expect.poll(() => page.evaluate(shellCaches).catch(() => null)).toEqual({ ['muster-shell-' + versionB]: goodShell });
});

// N1: an installed app resumes rather than navigates, so the resume is the
// update check. The first resume after a page load runs one — loading the page
// does not start the throttle window.
test('the first resume after a load checks for an update', async ({ page }) => {
  await page.clock.install();
  server = await startServer(versionA, updatePort);
  const origin = `http://localhost:${updatePort}`;
  await page.goto(origin + '/tasks');
  await waitControlled(page);
  await deployB();
  await page.evaluate(() => document.dispatchEvent(new Event('visibilitychange')));
  await expect(page.locator('#sw-update-toast')).toBeVisible();
});

// N1, the throttle: after a check that ran, resumes are ignored for a minute,
// and an ignored resume does not push the next check back.
//
// ⚠ COUNTED, NOT SLEPT ON. This used to prove "throttled" by waiting 1–3 s and
// seeing no toast — a sleep standing in for an absence. The resume handler
// calls reg.update() synchronously, so wrapping update() and reading the count
// right after the dispatch answers "did this resume check?" exactly.
test('a resume checks for an update at most once a minute', async ({ page }) => {
  await page.clock.install();
  await page.addInitScript(() => {
    const w = window as unknown as { __updates: number; __lastUpdate: Promise<unknown> };
    w.__updates = 0;
    w.__lastUpdate = Promise.resolve();
    const orig = ServiceWorkerRegistration.prototype.update;
    ServiceWorkerRegistration.prototype.update = function (this: ServiceWorkerRegistration) {
      w.__updates++;
      const p = orig.call(this);
      w.__lastUpdate = p.catch(() => undefined);
      return p;
    };
  });
  server = await startServer(versionA, updatePort);
  const origin = `http://localhost:${updatePort}`;
  await page.goto(origin + '/tasks');
  await waitControlled(page);
  const toast = page.locator('#sw-update-toast');
  // Dispatches a resume and returns how many checks have run, once the last
  // one has settled.
  const resume = () =>
    page.evaluate(async () => {
      const w = window as unknown as { __updates: number; __lastUpdate: Promise<unknown> };
      document.dispatchEvent(new Event('visibilitychange'));
      await w.__lastUpdate;
      return w.__updates;
    });

  expect(await resume(), 'the first resume checks (against A: nothing new)').toBe(1);
  await deployB();
  expect(await resume(), 'under a minute since that check: throttled').toBe(1);
  await page.clock.fastForward(40_000);
  expect(await resume(), 'still under a minute: throttled, and must NOT reset the clock').toBe(1);
  await expect(toast).toBeHidden();
  await page.clock.fastForward(25_000);
  expect(await resume(), 'over a minute since the last check that RAN').toBe(2);
  await expect(toast).toBeVisible();
});

// 🔴 A REGRESSION TEST (operator report: "the reload button is showing but
// doesn't reload on click"). Two clients of one origin — a second tab, or the
// installed app beside a browser tab — both offer the update. The first one's
// Reload activates B for EVERY client. The second page is then an old document
// under the new worker, with its toast still up; its Reload used to post
// SKIP_WAITING to the worker it captured (now the ACTIVE one, so a no-op), wait
// for a controllerchange that never came, and disable itself: a dead tap.
// It must load the new build instead. Run on the real input of each viewport:
// a touch tap on the phone, a mouse click on the desktop.
const viewports = [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true }, tap: true },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false }, tap: false },
];
for (const vp of viewports) {
  test.describe(vp.name, () => {
    test.use(vp.use);

    test('another client took the update: Reload here still lands on the new build', async ({ context }) => {
      server = await startServer(versionA, updatePort);
      const origin = `http://localhost:${updatePort}`;
      await context.addInitScript(() => {
        sessionStorage.setItem('loads', String(Number(sessionStorage.getItem('loads') || '0') + 1));
      });
      const first = await context.newPage();
      const second = await context.newPage();
      await first.goto(origin + '/tasks');
      await waitControlled(first);
      await second.goto(origin + '/agents');
      await waitControlled(second);
      await deployB();
      await first.evaluate(checkForUpdate);
      await expect(first.locator('#sw-update-toast')).toBeVisible();
      await expect(second.locator('#sw-update-toast')).toBeVisible();

      // The first client takes the update.
      await first.locator('[data-sw-reload]').click();
      await expect.poll(() => first.evaluate(controllerBuild).catch(() => null), { timeout: 15_000 }).toBe(versionB);
      // Premise: the second page is now controlled by B, nothing waits, and its
      // toast is still offering a reload (it IS still build A's document).
      await expect.poll(() => second.evaluate(controllerBuild)).toBe(versionB);
      expect(await second.evaluate(waitingState)).toBeNull();
      await expect(second.locator('#sw-update-toast')).toBeVisible();

      const before = Number(await second.evaluate(() => sessionStorage.getItem('loads')));
      // 🔴 THE PAGE CLOCK IS PAUSED, so Reload's timed fallback cannot fire:
      // the reload below must come from Reload seeing that nothing waits. (The
      // fallback has a test of its own, next.)
      await second.clock.install();
      await second.clock.pauseAt(Date.now() + 1000);
      const reloaded = second.waitForEvent('load', { timeout: 10_000 });
      const button = second.locator('[data-sw-reload]');
      if (vp.tap) await button.tap();
      else await button.click();
      await reloaded;
      expect(Number(await second.evaluate(() => sessionStorage.getItem('loads'))), 'the tap reloaded the page').toBe(before + 1);
      expect(await second.evaluate(controllerBuild)).toBe(versionB);
      await expect(second.locator('#sw-update-toast')).toBeHidden();
    });
  });
}

// The fallback: a waiting worker that is told SKIP_WAITING but never takes
// control (here the message is dropped, so no controllerchange can come) must
// not leave Reload dead. The page clock is paused, so it is the fallback timer
// — and nothing else — that reloads, and only once the clock passes it.
test('Reload still reloads when the waiting worker never takes control', async ({ page }) => {
  server = await startServer(versionA, updatePort);
  const origin = `http://localhost:${updatePort}`;
  await page.addInitScript(() => {
    sessionStorage.setItem('loads', String(Number(sessionStorage.getItem('loads') || '0') + 1));
    const orig = ServiceWorker.prototype.postMessage;
    ServiceWorker.prototype.postMessage = function (this: ServiceWorker, msg: unknown, ...rest: unknown[]) {
      if (msg && (msg as { type?: string }).type === 'SKIP_WAITING') {
        (window as unknown as { __skipDropped: number }).__skipDropped = ((window as unknown as { __skipDropped: number }).__skipDropped || 0) + 1;
        return;
      }
      return (orig as (...a: unknown[]) => void).call(this, msg, ...rest);
    };
  });
  await page.goto(origin + '/tasks');
  await waitControlled(page);
  await deployB();
  await page.evaluate(checkForUpdate);
  await expect(page.locator('#sw-update-toast')).toBeVisible();
  expect(await page.evaluate(waitingState)).toBe('installed');

  await page.clock.install();
  await page.clock.pauseAt(Date.now() + 1000);
  const before = Number(await page.evaluate(() => sessionStorage.getItem('loads')));
  await page.locator('[data-sw-reload]').click();
  // CONTROL: Reload really asked the waiting worker (and the ask was dropped).
  await expect.poll(() => page.evaluate(() => (window as unknown as { __skipDropped?: number }).__skipDropped ?? 0)).toBe(1);
  expect(Number(await page.evaluate(() => sessionStorage.getItem('loads'))), 'nothing reloads while the clock stands still').toBe(before);
  const reloaded = page.waitForEvent('load', { timeout: 10_000 });
  await page.clock.runFor(3_000);
  await reloaded;
  expect(Number(await page.evaluate(() => sessionStorage.getItem('loads'))), 'the fallback reloaded once').toBe(before + 1);
});

// PLACEMENT (operator report: "it's blocking the UI, move it higher up"). On a
// phone the toast sits at the TOP, under the sticky header — never on the
// header, the composer, Send, or any other fixed/sticky control. Offered for
// real (a deploy), so it is placed by the code that places it in production.
test.describe('placement at phone width', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

  test('the update toast sits under the header and covers no control', async ({ page }) => {
    server = await startServer(versionA, updatePort);
    const origin = `http://localhost:${updatePort}`;
    await page.goto(origin + '/tasks');
    await waitControlled(page);
    await deployB();
    await page.evaluate(checkForUpdate);
    for (const path of ['/tasks', '/agents', '/agents/docs-sweeper']) {
      // A full load re-offers the waiting update from the registration check.
      await page.goto(origin + path);
      const toast = page.locator('#sw-update-toast');
      await expect(toast).toBeVisible();
      const r = await page.evaluate(() => {
        const t = document.getElementById('sw-update-toast')!;
        const tb = t.getBoundingClientRect();
        const hit = (b: DOMRect) => !(tb.right <= b.left || tb.left >= b.right || tb.bottom <= b.top || tb.top >= b.bottom);
        const header = document.querySelector('header')!.getBoundingClientRect();
        // Every visible fixed/sticky element that is not the toast: the header,
        // the composer bar, the FAB, the chat button.
        const covered: string[] = [];
        let checked = 0;
        for (const e of Array.from(document.querySelectorAll('body *'))) {
          if (e === t || t.contains(e) || e.contains(t)) continue;
          const cs = getComputedStyle(e);
          if (cs.position !== 'fixed' && cs.position !== 'sticky') continue;
          // A closed overlay (the sidebar's backdrop) spans the screen but is
          // neither seen nor tapped.
          if (cs.visibility === 'hidden' || cs.opacity === '0' || cs.pointerEvents === 'none') continue;
          const b = e.getBoundingClientRect();
          if (b.width === 0 || b.height === 0) continue;
          checked++;
          if (hit(b)) covered.push(e.tagName.toLowerCase() + (e.id ? '#' + e.id : ''));
        }
        for (const id of ['chat-input', 'chat-send']) {
          const e = document.getElementById(id);
          if (e && hit(e.getBoundingClientRect())) covered.push('#' + id);
        }
        return { top: tb.top, bottom: tb.bottom, height: tb.height, headerBottom: header.bottom, vh: innerHeight, checked, covered };
      });
      expect(r.height, `CONTROL on ${path}: the toast was really laid out`).toBeGreaterThan(20);
      expect(r.checked, `CONTROL on ${path}: fixed/sticky controls were found to compare against`).toBeGreaterThan(0);
      expect(r.top, `on ${path}: under the header`).toBeGreaterThanOrEqual(r.headerBottom);
      expect(r.bottom, `on ${path}: in the top half of the screen`).toBeLessThan(r.vh / 2);
      expect(r.covered, `on ${path}: covers nothing`).toEqual([]);
    }
    // The chat composer page really has its composer (the overlap check above is
    // only as good as this).
    await expect(page.locator('#chat-input')).toBeVisible();

    // Dismissible: the × hides it, and an in-app navigation does not re-offer
    // the update that was dismissed.
    await page.locator('[data-sw-dismiss]').tap();
    await expect(page.locator('#sw-update-toast')).toBeHidden();
    await page.evaluate(() => document.body.dispatchEvent(new Event('htmx:load', { bubbles: true })));
    await expect(page.locator('#sw-update-toast')).toBeHidden();
  });
});
