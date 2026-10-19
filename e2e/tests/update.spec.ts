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
  // 🔴 The new worker WAITS: the page is still controlled by build A. (A worker
  // that called skipWaiting() at install would already be in control here.)
  await page.waitForTimeout(500);
  expect(await page.evaluate(controllerBuild)).toBe(versionA);

  const loadsBefore = Number(await page.evaluate(() => sessionStorage.getItem('loads')));
  await page.locator('[data-sw-reload]').click();
  // The reload this click causes destroys the page's context mid-evaluate;
  // that is the expected event, so a destroyed context reads as "not yet".
  await expect
    .poll(() => page.evaluate(controllerBuild).catch(() => null), { timeout: 15_000 })
    .toBe(versionB);
  await page.waitForLoadState('load');
  await page.waitForTimeout(1500);
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
test('a resume checks for an update at most once a minute', async ({ page }) => {
  await page.clock.install();
  server = await startServer(versionA, updatePort);
  const origin = `http://localhost:${updatePort}`;
  await page.goto(origin + '/tasks');
  await waitControlled(page);
  const toast = page.locator('#sw-update-toast');
  const resume = () => page.evaluate(() => document.dispatchEvent(new Event('visibilitychange')));

  await resume(); // a check runs (against A: nothing new)
  await page.waitForTimeout(1000);
  await deployB();
  await resume(); // under a minute since that check: throttled
  await page.waitForTimeout(3000);
  await expect(toast).toBeHidden();
  await page.clock.fastForward(40_000);
  await resume(); // still under a minute: throttled, and must NOT reset the clock
  await page.waitForTimeout(1000);
  await expect(toast).toBeHidden();
  await page.clock.fastForward(25_000);
  await resume(); // over a minute since the last check that RAN
  await expect(toast).toBeVisible();
});
