import fs from 'node:fs';
import { Browser, BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { authState, baseURL, bfcacheBuildPort, bfcacheRestorePort, password, versionA, versionB } from '../lib/env';
import { launchWithBfcache } from '../lib/bfcache';
import { clsInit, readCls, settle } from '../lib/cls';
import { cc, mkchat, MirrorRig } from '../lib/mirrorhost';
import { startServer, Running } from '../lib/server';

// BACK AND FORWARD RESTORE THE VIEW PAGES (internal/ui restore.go, internal/api
// viewDocument). Operator ask: "cache things better so that I don't have to
// wait for full reloads every time I hit the back button … Be careful not to
// cause layout shift when rehydrating."
//
// 🔴 EVERY CASE RUNS IN lib/bfcache.ts's BROWSER. The suite's own browser
// cannot restore a page at all (see there), so a goBack in it is always the
// reload path. tests/bfcache.spec.ts is the control that this browser can.
//
// 🔴 A COOKIE IS CHANGED WHILE EACH PAGE IS AWAY, AND THAT IS WHAT MAKES THE
// REGRESSION VISIBLE. Measured (Chromium 153): a no-store page IS restored when
// nothing else changed, and refused once a cookie changed while it was away
// (CacheControlNoStoreCookieModified) — which a real session does all the time
// (a sign-in's refresh, any Set-Cookie). Without the change, the no-store page
// of main would be restored too, and "restored" would prove nothing.
//
// A restore is the SAME document shown again with pageshow persisted=true. The
// recorder below stamps each document with an id and writes every pageshow to
// sessionStorage, so the record survives a reload that follows a restore.

const recorder = `(() => {
  const doc = Math.random().toString(36).slice(2);
  window.__e2eDoc = doc;
  addEventListener('pageshow', (e) => {
    try {
      const a = JSON.parse(sessionStorage.getItem('__e2eShows') || '[]');
      a.push({ doc, path: location.pathname, persisted: e.persisted, at: Date.now() });
      sessionStorage.setItem('__e2eShows', JSON.stringify(a));
    } catch (x) {}
  });
})()`;

interface Show {
  doc: string;
  path: string;
  persisted: boolean;
  at: number;
}
const shows = (page: Page): Promise<Show[]> => page.evaluate(() => JSON.parse(sessionStorage.getItem('__e2eShows') || '[]'));
const docOf = (page: Page): Promise<string> => page.evaluate(() => (window as any).__e2eDoc);

let browser: Browser;
test.beforeAll(async ({ playwright }) => {
  browser = await launchWithBfcache(playwright.chromium);
});
test.afterAll(async () => {
  await browser?.close();
});

// notRestored is, per page, why the browser did NOT restore it (CDP
// Page.backForwardCacheNotUsed): the message of a "restored" assertion carries
// it, so a refusal names its reason.
const notRestored = new WeakMap<Page, string[]>();

// init: more init scripts, installed BEFORE the page exists (one added to a
// context whose page is already open was measured not to run in it).
async function newContext(opts: { origin?: string; signedIn?: boolean; width?: number; init?: string[] } = {}): Promise<{ ctx: BrowserContext; page: Page }> {
  const width = opts.width ?? 390;
  const ctx = await browser.newContext({
    baseURL: opts.origin ?? baseURL,
    storageState: opts.signedIn === false ? undefined : authState,
    viewport: width === 390 ? { width: 390, height: 844 } : { width: 1280, height: 800 },
  });
  await ctx.addInitScript(recorder);
  for (const i of opts.init || []) await ctx.addInitScript(i);
  const page = await ctx.newPage();
  const why: string[] = [];
  notRestored.set(page, why);
  const cdp = await ctx.newCDPSession(page);
  await cdp.send('Page.enable');
  cdp.on('Page.backForwardCacheNotUsed', (e: any) => {
    why.push(JSON.stringify((e.notRestoredExplanations || []).map((x: any) => x.reason)));
  });
  return { ctx, page };
}

// restoredAs asserts Back showed doc again, restored.
async function expectRestored(page: Page, back: Show, doc: string): Promise<void> {
  expect(back, `Back restored the same document (not restored because: ${(notRestored.get(page) || []).join(' ')})`).toMatchObject({ doc, persisted: true });
}

// settled waits until a restored page is quiet: no htmx request in flight and
// no DOM change or shift for a second (lib/cls.ts's settle, without its wait
// for a load event — a restore fires none).
async function settled(page: Page): Promise<void> {
  await expect
    .poll(() => page.evaluate(() => {
      const S = (window as any).__cls;
      return !!S && S.inflight === 0 && performance.now() - S.last >= 1000;
    }), { timeout: 15_000 })
    .toBe(true);
}

// away leaves the page for path (a real navigation, so the page goes into the
// back/forward cache), changes a cookie there, runs during(), and goes Back.
async function awayAndBack(page: Page, path: string, during: () => Promise<void> = async () => {}): Promise<void> {
  await page.goto(path);
  await page.evaluate(() => (document.cookie = 'e2e-away=' + Date.now() + '; path=/'));
  await during();
  // 'commit': a restore fires no load event.
  await page.goBack({ waitUntil: 'commit' });
}

// backShow is the pageshow Back landed on: the first one recorded after `since`
// for path.
async function backShow(page: Page, path: string, since: number): Promise<Show> {
  let got: Show | undefined;
  await expect
    .poll(async () => {
      got = (await shows(page).catch(() => [])).find((s) => s.path === path && s.at >= since);
      return !!got;
    }, { message: 'Back showed the page' })
    .toBe(true);
  return got!;
}

// The page's own record of its streams: every WebSocket and EventSource it
// opens, when (performance.now()), and how many messages each received.
// 🔴 IN THE PAGE: Playwright's websocket event does not see a socket a RESTORED
// document opens (measured: a restored terminal said Connected while
// page.on('websocket') reported nothing new), and an EventSource has no
// Playwright event at all, so the instrument lives in the document and
// survives the restore with it.
const streamLog = `(() => {
  window.__socks = [];
  const wrap = (C, kind) => {
    const N = function (url, p) {
      const o = p === undefined ? new C(url) : new C(url, p);
      const rec = { kind, url: String(url), at: performance.now(), msgs: 0, opened: false };
      window.__socks.push(rec);
      o.addEventListener('message', () => rec.msgs++);
      o.addEventListener('open', () => (rec.opened = true));
      return o;
    };
    N.prototype = C.prototype;
    for (const k of ['CONNECTING', 'OPEN', 'CLOSING', 'CLOSED']) N[k] = C[k];
    return N;
  };
  window.WebSocket = wrap(window.WebSocket, 'ws');
  window.EventSource = wrap(window.EventSource, 'sse');
  addEventListener('pageshow', (e) => { if (e.persisted) window.__restoredAt = performance.now(); });
})()`;

// opened is how many streams of kind whose URL contains part this document
// opened since its last restore, and saw open.
const openedSinceRestore = (page: Page, kind: string, part: string) =>
  page.evaluate(([kind, part]) => {
    const w = window as any;
    return w.__socks.filter((s: any) => s.kind === kind && s.url.includes(part) && s.at >= w.__restoredAt && s.opened).length;
  }, [kind, part]);

// --- an agent's chat page (the shared server) ---------------------------------

const agent = 'e2e-bfc-' + Date.now().toString(36);
const transcript = Array.from({ length: 40 }, (_, i) => ({
  role: i % 2 ? 'assistant' : 'user',
  content: `Synthetic message ${i}: the quick brown fox jumps over the lazy dog, then reports back.`,
}));
test.beforeAll(() => mkchat(agent, transcript));
test.afterAll(() => mkchat(agent, null));

async function agentURL(page: Page): Promise<string> {
  const r = await page.request.get('/agents/' + agent, { maxRedirects: 0 });
  expect(r.status(), 'the bare agent URL redirects to its session').toBe(303);
  return r.headers()['location'];
}

async function openAgent(page: Page): Promise<string> {
  const url = await agentURL(page);
  await page.goto(url);
  await expect(page.locator('#chat-log [data-msg-id]').first()).toBeVisible();
  // The page's stream is open (the restore must reopen it).
  await expect.poll(() => page.evaluate(() => !!document.querySelector('[sse-connect]'))).toBe(true);
  return url;
}

test('back to an agent page restores it, and it then shows a message stored while it was away', async () => {
  const { ctx, page } = await newContext({ init: [streamLog] });
  try {
    const url = await openAgent(page);
    const doc = await docOf(page);
    const marker = 'Stored while the page was in the back/forward cache ' + Date.now();
    const t0 = Date.now();
    await awayAndBack(page, '/tasks', async () => mkchat(agent, [{ role: 'assistant', content: marker }], { MUSTER_E2E_APPEND: '1' }));
    await expectRestored(page, await backShow(page, new URL(url, baseURL).pathname, t0), doc);
    // 🔴 THE MEASURED HAZARD: restored, its event stream is gone and nothing
    // refetches the transcript, so the message would never appear.
    await expect(page.locator('#chat-log')).toContainText(marker, { timeout: 5000 });
    expect(await docOf(page), 'and it was not reloaded to get it').toBe(doc);
    // Its event stream is open again, for what comes NEXT: the refetch above
    // caught up on what came while it was away, and the stream is how the
    // page hears of anything after that.
    await expect.poll(() => openedSinceRestore(page, 'sse', '/events'), { message: 'the /events stream was reopened after the restore' }).toBeGreaterThan(0);
    // The service worker stored no document: only its shell, stylesheet and
    // manifest (sw.js SHELL). Read once it is installed, so the listing can
    // see what it holds (the shell, below, is the positive control).
    const cached = await page.evaluate(async () => {
      await navigator.serviceWorker.ready;
      const out: string[] = [];
      for (const k of await caches.keys()) for (const r of await (await caches.open(k)).keys()) out.push(new URL(r.url).pathname);
      return out;
    });
    expect(cached, 'the instrument sees the worker\'s cache: its shell is there').toContain('/');
    expect(cached.filter((p) => p.startsWith('/agents') || p.startsWith('/mirror')), 'no view document in the service worker cache').toEqual([]);
  } finally {
    await ctx.close();
  }
});

// 🔴 #247's DIRECT LINKS: every switcher entry names the session it opens
// (ui.AgentSessionHref), drawn when the page was. A restored page re-requests
// itself, as a reload would have — which records the visit and marks the
// session read — and redraws the switcher's rows from the answer, so Recent
// and Pins name what is current now.
test('a restored agent page redraws its switcher: an agent opened meanwhile is in Recent', async () => {
  const other = 'e2e-bfc-other-' + Date.now().toString(36);
  mkchat(other, [{ role: 'user', content: 'The other agent.' }]);
  const { ctx, page } = await newContext();
  try {
    const url = await openAgent(page);
    const doc = await docOf(page);
    const recent = page.locator('[data-switcher-list="recent"]');
    await expect(recent).toBeAttached();
    expect(await recent.locator(`a[href^="/agents/${other}"]`).count(), 'the other agent is not in Recent yet').toBe(0);
    const t0 = Date.now();
    await awayAndBack(page, '/tasks', async () => {
      // Opened elsewhere (another tab, another device): the server records the visit.
      const r = await page.request.get('/agents/' + other);
      expect(r.status()).toBe(200);
    });
    await expectRestored(page, await backShow(page, new URL(url, baseURL).pathname, t0), doc);
    await expect(recent.locator(`a[href^="/agents/${other}"]`), 'Recent was redrawn with it').toHaveCount(1, { timeout: 5000 });
    expect(await docOf(page), 'without a reload').toBe(doc);
  } finally {
    await ctx.close();
    mkchat(other, null);
  }
});

// The Agents list (the shell) was already restorable; its restore reopens the
// list's event stream too, and the list catches up on the stream's (re)open.
test('a restored Agents list shows an agent created while it was away', async () => {
  const fresh = 'e2e-bfc-new-' + Date.now().toString(36);
  const { ctx, page } = await newContext();
  try {
    await page.goto('/agents');
    await expect(page.locator('#agents-list > article').first()).toBeVisible();
    const doc = await docOf(page);
    const t0 = Date.now();
    await awayAndBack(page, '/tasks', async () => mkchat(fresh, [{ role: 'user', content: 'Created while the list was away.' }]));
    await expectRestored(page, await backShow(page, '/agents', t0), doc);
    await expect(page.locator(`#agents-list a[href^="/agents/${fresh}"]`).first(), 'the list caught up').toBeAttached({ timeout: 10_000 });
  } finally {
    await ctx.close();
    mkchat(fresh, null);
  }
});

test('an agent page restored after its agent was deleted reloads', async () => {
  const gone = 'e2e-bfc-gone-' + Date.now().toString(36);
  mkchat(gone, [{ role: 'user', content: 'An agent deleted while its page was away.' }]);
  const { ctx, page } = await newContext();
  try {
    const r = await page.request.get('/agents/' + gone, { maxRedirects: 0 });
    const url = r.headers()['location'];
    await page.goto(url);
    await expect(page.locator('#chat-log [data-msg-id]').first()).toBeVisible();
    const doc = await docOf(page);
    const t0 = Date.now();
    await awayAndBack(page, '/tasks', async () => mkchat(gone, null));
    const path = new URL(url, baseURL).pathname;
    await expectRestored(page, await backShow(page, path, t0), doc);
    await expect.poll(async () => (await shows(page).catch(() => [])).filter((s) => s.at >= t0 && s.doc !== doc).length, {
      message: 'and then reloaded: the page says what is there now',
    }).toBeGreaterThan(0);
    await expect(page.locator('#chat-log [data-msg-id]')).toHaveCount(0);
  } finally {
    await ctx.close();
    mkchat(gone, null);
  }
});

test('a restore after more than ten minutes away reloads', async () => {
  const { ctx, page } = await newContext();
  try {
    const url = await openAgent(page);
    const doc = await docOf(page);
    const t0 = Date.now();
    // 🔴 THE CLOCK IS MOVED IN THE RECORD, NOT IN THE PAGE: a document frozen in
    // the back/forward cache runs no script, so no fake clock can be advanced
    // inside it while it is away. The page records the instant it was hidden
    // (sessionStorage, ui.RestoreHiddenKey); the test moves that instant eleven
    // minutes back, which is the same as having been away for eleven minutes.
    await awayAndBack(page, '/tasks', async () => {
      await page.evaluate(() => {
        const m = JSON.parse(sessionStorage.getItem('muster.restore.hidden') || '{}');
        for (const k of Object.keys(m)) m[k] -= 11 * 60 * 1000;
        sessionStorage.setItem('muster.restore.hidden', JSON.stringify(m));
      });
    });
    const path = new URL(url, baseURL).pathname;
    await expectRestored(page, await backShow(page, path, t0), doc);
    await expect.poll(async () => (await shows(page)).filter((s) => s.path === path && s.at >= t0 && s.doc !== doc).length, {
      message: 'and then reloaded it: a new document',
    }).toBe(1);
  } finally {
    await ctx.close();
  }
});

// The positive control of the case above, through the same instrument: nine
// minutes away is a restore that stays.
test('a restore after nine minutes away is kept', async () => {
  const { ctx, page } = await newContext();
  try {
    const url = await openAgent(page);
    const doc = await docOf(page);
    const t0 = Date.now();
    await awayAndBack(page, '/tasks', async () => {
      await page.evaluate(() => {
        const m = JSON.parse(sessionStorage.getItem('muster.restore.hidden') || '{}');
        for (const k of Object.keys(m)) m[k] -= 9 * 60 * 1000;
        sessionStorage.setItem('muster.restore.hidden', JSON.stringify(m));
      });
    });
    const path = new URL(url, baseURL).pathname;
    await expectRestored(page, await backShow(page, path, t0), doc);
    await page.waitForTimeout(1500);
    expect((await shows(page)).filter((s) => s.path === path && s.at >= t0).map((s) => s.doc), 'no reload').toEqual([doc]);
  } finally {
    await ctx.close();
  }
});

// --- the "Updating…" chip ------------------------------------------------------

// chipLog records, from the restore on, when the page's Updating… chip is shown
// and hidden (performance.now()).
const chipLog = `(() => {
  window.__chip = [];
  let was = null;
  const mark = () => {
    const c = document.querySelector('[data-updating-chip]');
    if (!c) return;
    const on = !c.classList.contains('invisible');
    if (on !== was) { was = on; window.__chip.push({ on, at: performance.now() }); }
  };
  addEventListener('pageshow', (e) => { if (e.persisted) window.__chip.push({ restore: true, at: performance.now() }); });
  // An init script can run before <html> exists.
  const watch = () => new MutationObserver(mark).observe(document.documentElement, { subtree: true, attributes: true, attributeFilter: ['class'] });
  if (document.documentElement) watch(); else document.addEventListener('DOMContentLoaded', watch);
})()`;

// slowNet holds every fetch and XHR the page makes for 1.8 s while
// sessionStorage.__e2eSlow is '1'. 🔴 IN THE PAGE, NOT page.route: Playwright's
// routing does not see the requests a RESTORED document makes (measured: a
// route holding /health and the page's own URL held nothing after a restore,
// and the catch-up landed at once), so the delay is installed where the
// requests start, and survives the restore with the document.
const slowNet = `(() => {
  const slow = () => { try { return sessionStorage.getItem('__e2eSlow') === '1'; } catch (e) { return false; } };
  const wait = () => new Promise((r) => setTimeout(r, 1800));
  const F = window.fetch;
  window.fetch = function () { const a = arguments; return slow() ? wait().then(() => F.apply(window, a)) : F.apply(window, a); };
  const S = XMLHttpRequest.prototype.send;
  XMLHttpRequest.prototype.send = function () { const x = this, a = arguments; if (slow()) setTimeout(() => S.apply(x, a), 1800); else S.apply(x, a); };
})()`;

test('the Updating… chip shows only once a restore has been catching up for a second', async () => {
  const { ctx, page } = await newContext({ init: [chipLog, slowNet] });
  try {
    await openAgent(page);
    const doc = await docOf(page);
    const path = new URL(page.url()).pathname;
    // Slow: every catch-up request is held 1.8 s. The chip must show after
    // the first second, and go once they land.
    let t0 = Date.now();
    await awayAndBack(page, '/tasks', async () => {
      await page.evaluate(() => sessionStorage.setItem('__e2eSlow', '1'));
    });
    await expectRestored(page, await backShow(page, path, t0), doc);
    await expect.poll(() => page.evaluate(() => {
      const L = (window as any).__chip as any[];
      const r = L.findIndex((x) => x.restore);
      return r >= 0 && L.slice(r).some((c) => c.on === true) && L.slice(r).some((c, i, a) => c.on === false && a.slice(0, i).some((p) => p.on === true));
    }), {
      message: 'the chip was shown and hidden again',
      timeout: 10_000,
    }).toBe(true).catch(async (e) => {
      throw new Error(e.message + '\nchip log: ' + JSON.stringify(await page.evaluate(() => (window as any).__chip)));
    });
    const log = (await page.evaluate(() => (window as any).__chip)) as { on?: boolean; restore?: boolean; at: number }[];
    const restore = log.find((c) => c.restore)!;
    const on = log.find((c) => c.on === true && c.at >= restore.at);
    const off = log.find((c) => c.on === false && on && c.at > on.at);
    expect(on, `the chip showed during a slow catch-up: ${JSON.stringify(log)}`).toBeTruthy();
    expect(on!.at - restore.at, 'not before a second had passed').toBeGreaterThanOrEqual(950);
    expect(off, 'and was hidden once it caught up').toBeTruthy();
    expect(off!.at - restore.at, 'once the held requests (1.8 s) had landed').toBeGreaterThanOrEqual(1700);

    // Fast: nothing held — the chip never shows. (After the slow restore's
    // own stream reopen has caught up too: its refetch, held like the rest,
    // lands up to 1.8 s after the chip went.)
    await page.waitForTimeout(2500);
    await page.evaluate(() => {
      sessionStorage.removeItem('__e2eSlow');
      (window as any).__chip = [];
    });
    t0 = Date.now();
    await awayAndBack(page, '/tasks');
    await expectRestored(page, await backShow(page, path, t0), doc);
    await page.waitForTimeout(2000);
    const fast = (await page.evaluate(() => (window as any).__chip)) as { on?: boolean }[];
    expect(fast.some((c: any) => c.restore), 'the instrument saw the second restore').toBe(true);
    expect(fast.filter((c) => c.on === true), `a fast catch-up shows no chip: ${JSON.stringify(fast)}`).toEqual([]);
  } finally {
    await ctx.close();
  }
});

// --- layout shift on restore -----------------------------------------------------

for (const width of [390, 1280]) {
  test(`an agent page restored at ${width}px, catching up on a new message, does not shift`, async () => {
    const { ctx, page } = await newContext({ width, init: [clsInit] });
    try {
      const url = await openAgent(page);
      await settle(page);
      const doc = await docOf(page);
      const t0 = Date.now();
      // Measured from the restore on: the record is zeroed as the page leaves.
      await page.evaluate(() => addEventListener('pagehide', () => {
        const S = (window as any).__cls;
        S.total = 0; S.shifts = []; S.excluded = 0; S.excludedShifts = [];
      }));
      await awayAndBack(page, '/tasks', async () => mkchat(agent, [{ role: 'assistant', content: `A reply for the ${width}px restore ${Date.now()}` }], { MUSTER_E2E_APPEND: '1' }));
      await expectRestored(page, await backShow(page, new URL(url, baseURL).pathname, t0), doc);
      await expect(page.locator('#chat-log')).toContainText(`the ${width}px restore`);
      await settled(page);
      const cls = await readCls(page);
      expect(cls.total, `layout shift on restore: ${JSON.stringify(cls.shifts)}`).toBeLessThan(0.01);
    } finally {
      await ctx.close();
    }
  });
}

// --- sign-out ----------------------------------------------------------------

// signIn signs a fresh context in through the form (its own session, so the
// sign-out ends no other spec's).
async function signIn(page: Page, origin = baseURL): Promise<void> {
  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
}

// ⚠ ON AN AGENT PAGE THIS HOLDS EVEN WITHOUT Clear-Site-Data: the sign-in
// page's snapshot purge evicts it first (see the terminal's case below, which
// is the one that tests the header). It is kept as the agent page's own check.
test('after a sign-out, Back never shows the agent page again', async () => {
  const { ctx, page } = await newContext({ signedIn: false });
  try {
    await signIn(page);
    const url = await openAgent(page);
    const doc = await docOf(page);
    await page.goto('/tasks');
    // The sign-out: its form's POST, as a page submits it (a navigation).
    const t0 = Date.now();
    await Promise.all([
      page.waitForURL((u) => u.pathname === '/login'),
      page.evaluate(() => {
        const f = document.createElement('form');
        f.method = 'POST';
        f.action = '/logout';
        document.body.appendChild(f);
        f.submit();
      }),
    ]);
    // Back, twice: past /tasks, to the agent page's entry.
    for (let i = 0; i < 2; i++) {
      await page.goBack({ waitUntil: 'commit' });
      await expect(page.locator('#muster-password'), `Back #${i + 1} shows the sign-in`).toBeVisible();
    }
    const after = (await shows(page)).filter((s) => s.at >= t0);
    expect(after.filter((s) => s.persisted), `nothing was restored after the sign-out: ${JSON.stringify(after)}`).toEqual([]);
    expect(after.some((s) => s.doc === doc), 'the agent page\'s document was never shown again').toBe(false);
    expect(page.url()).toContain('/login');
    void url;
  } finally {
    await ctx.close();
  }
});

test('a page restored after its session ended reloads to the sign-in', async () => {
  const { ctx, page } = await newContext({ signedIn: false });
  try {
    await signIn(page);
    const url = await openAgent(page);
    const doc = await docOf(page);
    const t0 = Date.now();
    // The session ends with no sign-out page seen (it expired, or ended on
    // another tab): no Clear-Site-Data reached this browser, so the page IS
    // restored — and must not stay.
    await awayAndBack(page, '/tasks', async () => {
      await ctx.clearCookies();
    });
    await expectRestored(page, await backShow(page, new URL(url, baseURL).pathname, t0), doc);
    await expect(page.locator('#muster-password'), 'the restored page went to the sign-in').toBeVisible({ timeout: 10_000 });
  } finally {
    await ctx.close();
  }
});

// --- a deploy while the page was away (build A, then build B) -----------------

test('a restore after a deploy reloads into the new build', async () => {
  const origin = `http://localhost:${bfcacheBuildPort}`;
  let server: Running | undefined = await startServer(versionA, bfcacheBuildPort);
  const { ctx, page } = await newContext({ origin, signedIn: false });
  try {
    await signIn(page, origin);
    const url = await openAgent(page);
    const doc = await docOf(page);
    const build = () => page.evaluate(() => document.querySelector('script[data-snap-build]')?.getAttribute('data-snap-build')).catch(() => 'navigating');
    expect(await build()).toBe(versionA);
    const t0 = Date.now();
    await awayAndBack(page, '/tasks', async () => {
      await server!.stop();
      server = await startServer(versionB, bfcacheBuildPort);
    });
    const path = new URL(url, origin).pathname;
    await expectRestored(page, await backShow(page, path, t0), doc);
    await expect.poll(build, { message: 'and then reloaded it from build B' }).toBe(versionB);
    expect((await shows(page)).filter((s) => s.path === path && s.at >= t0 && s.doc !== doc).length).toBe(1);
  } finally {
    // The server first: a context that cannot close must not leak it.
    await server?.stop();
    await ctx.close().catch(() => {});
  }
});

// 🔴 THE OTHER WAY BACK: NOT A RESTORE, BUT THE HTTP CACHE. A view document is
// no longer no-store, so the HTTP cache keeps it, and a Back the back/forward
// cache cannot serve (evicted, too old, a browser without one) is answered from
// there WITHOUT asking the server — measured: navigation type back_forward,
// transferSize 0. That is a fresh run of the OLD build's script. The suite's
// own browser has no back/forward cache (lib/bfcache.ts), so its Back is
// exactly that path.
test('a Back answered from the HTTP cache after a deploy reloads into the new build', async ({ browser: plain }) => {
  const origin = `http://localhost:${bfcacheBuildPort}`;
  let server: Running | undefined = await startServer(versionA, bfcacheBuildPort);
  // Signed in by the suite's saved state: its session cookie is for the host,
  // whatever the port, and every server here shares one database.
  const ctx = await plain.newContext({ baseURL: origin, storageState: authState, viewport: { width: 390, height: 844 } });
  await ctx.addInitScript(recorder);
  const page = await ctx.newPage();
  try {
    const url = await openAgent(page);
    const build = () => page.evaluate(() => document.querySelector('script[data-snap-build]')?.getAttribute('data-snap-build')).catch(() => 'navigating');
    expect(await build()).toBe(versionA);
    await page.goto('/tasks');
    await server.stop();
    server = await startServer(versionB, bfcacheBuildPort);
    const t0 = Date.now();
    await page.goBack({ waitUntil: 'commit' });
    const path = new URL(url, origin).pathname;
    // The instrument: Back ran the build-A document again, from the cache.
    await expect.poll(async () => (await shows(page).catch(() => [])).filter((s) => s.path === path && s.at >= t0).length).toBeGreaterThan(0);
    await expect.poll(build, { message: 'the build-A copy from the HTTP cache reloaded into build B' }).toBe(versionB);
    const nav = await page.evaluate(() => {
      const n = performance.getEntriesByType('navigation')[0] as PerformanceNavigationTiming;
      return { type: n.type, fromCache: n.transferSize === 0 };
    });
    expect(nav, 'the build-B document came from the network').toEqual({ type: 'reload', fromCache: false });
  } finally {
    // The server first: a context that cannot close must not leak it.
    await server?.stop();
    await ctx.close().catch(() => {});
  }
});

// --- the mirrored Rendered view and its terminal ---------------------------------

const rig = new MirrorRig(bfcacheRestorePort, 'e2e-bfc');
let trPath = '';
test.describe('mirrored', () => {
  test.beforeAll(async () => {
    test.setTimeout(180_000);
    await rig.start();
    trPath = rig.transcript(cc.prompt('the prompt of the restored window') + cc.reply('The first answer.'), 'restore');
    const now = Math.floor(Date.now() / 1000);
    const windows = rig.transcript(`work|@1|1|1|claude|${now - 60}|${trPath}\n`, 'windows');
    await rig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: windows, MUSTER_MIRROR_E2E_REFRESH: '300ms' });
  });
  test.afterAll(async () => {
    await rig.stop();
  });

  const view = () => `/mirror/view?host=${encodeURIComponent(rig.host)}&session=work&window=${encodeURIComponent('@1')}`;
  const term = () => `/mirror/terminal?host=${encodeURIComponent(rig.host)}&session=work&window=${encodeURIComponent('@1')}`;

  async function mirrorPage(width = 390, init: string[] = []): Promise<{ ctx: BrowserContext; page: Page }> {
    const r = await newContext({ origin: rig.origin, signedIn: false, width, init });
    await rig.login(r.page);
    await rig.waitListed(r.page, 'data-mirror-claude');
    return r;
  }

  test('back to the Rendered view restores it, resumes its socket and draws a reply that came meanwhile, once', async () => {
    const { ctx, page } = await mirrorPage();
    try {
      await page.goto(rig.origin + view());
      await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 15_000 });
      await expect(page.locator('#mv-log')).toContainText('The first answer.');
      // The open's own storage reads are done, and so is its first save.
      await page.waitForTimeout(1500);
      // 🔴 LEFT RIGHT AFTER A BATCH, while the page's snapshot save is still
      // pending (it waits 800 ms after a batch). That save used to run as the
      // page was hidden, and its completion, reaching the frozen page, evicted
      // it (CDP: IgnoreEventAndEvict): Back reloaded the view.
      const first = 'A reply drawn just before leaving ' + Date.now();
      fs.appendFileSync(trPath, cc.reply(first));
      await expect(page.locator('#mv-log')).toContainText(first, { timeout: 10_000 });
      const before = await page.locator('#mv-log [data-bubble]').count();
      const doc = await docOf(page);
      const t0 = Date.now();
      const marker = 'A reply written while the view was away ' + Date.now();
      await awayAndBack(page, rig.origin + '/tasks', async () => fs.appendFileSync(trPath, cc.reply(marker)));
      await expectRestored(page, await backShow(page, '/mirror/view', t0), doc);
      await expect(page.locator('#mv-log')).toContainText(marker, { timeout: 10_000 });
      await expect(page.locator('#mv-status')).toHaveText('Live');
      // Nothing painted twice: the earlier bubbles once each, plus the reply.
      expect(await page.locator('#mv-log [data-bubble]').count()).toBe(before + 1);
      expect(await page.locator('#mv-log').getByText(first).count()).toBe(1);
    } finally {
      await ctx.close();
    }
  });

  for (const width of [390, 1280]) {
    test(`the Rendered view restored at ${width}px does not shift`, async () => {
      const { ctx, page } = await mirrorPage(width, [clsInit]);
      try {
        await page.goto(rig.origin + view());
        await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 15_000 });
        await settle(page);
        const doc = await docOf(page);
        await page.evaluate(() => addEventListener('pagehide', () => {
          const S = (window as any).__cls;
          S.total = 0; S.shifts = []; S.excluded = 0; S.excludedShifts = [];
        }));
        const t0 = Date.now();
        const marker = `A reply for the ${width}px view restore ${Date.now()}`;
        await awayAndBack(page, rig.origin + '/tasks', async () => fs.appendFileSync(trPath, cc.reply(marker)));
        await expectRestored(page, await backShow(page, '/mirror/view', t0), doc);
        await expect(page.locator('#mv-log')).toContainText(marker, { timeout: 10_000 });
        await settled(page);
        const cls = await readCls(page);
        expect(cls.total, `layout shift on restore: ${JSON.stringify(cls.shifts)}`).toBeLessThan(0.01);
      } finally {
        await ctx.close();
      }
    });
  }

  // 🔴 A STORAGE WRITE IN FLIGHT AS A PAGE IS FROZEN EVICTS IT (CDP reason
  // IgnoreEventAndEvict): its completion event reaches a page in the cache. The
  // Agents list warms snapshots of the windows it links (web/static/
  // mirror-snap.js), and stored what a warm had read the moment it was hidden —
  // so leaving the list mid-warm made Back reload it.
  test('the Agents list left in the middle of warming a snapshot is still restored', async () => {
    const { ctx, page } = await mirrorPage();
    try {
      // The window is in this user's Recent (a visit is recorded by the view's
      // own GET), and this browser holds no snapshot of it: it is warmed.
      expect((await page.request.get(rig.origin + view())).status()).toBe(200);
      const warm = page.waitForEvent('websocket', { predicate: (ws) => ws.url().includes('/mirror/view/ws'), timeout: 15_000 });
      await page.goto(rig.origin + '/agents');
      await expect(page.locator('#agents-list')).toBeVisible();
      const doc = await docOf(page);
      await warm;
      const t0 = Date.now();
      await awayAndBack(page, rig.origin + '/tasks');
      await expectRestored(page, await backShow(page, '/agents', t0), doc);
    } finally {
      await ctx.close();
    }
  });


  // 🔴 ON THE TERMINAL, BECAUSE IT HOLDS NO STORAGE CONNECTION. Measured with
  // Clear-Site-Data removed: an agent page (or the list) is evicted at the
  // sign-out anyway — the sign-in page's snapshot purge deletes the database
  // its open connection holds (CDP: IgnoreEventAndEvict) — so a sign-out test
  // on those pages stays green without the header and proves nothing about it.
  // A terminal page has no such connection: only the header (and, behind it,
  // the restore's own refusal check) keeps it from being shown again.
  test('after a sign-out, Back never shows a terminal again', async () => {
    const { ctx, page } = await mirrorPage();
    try {
      await page.goto(rig.origin + term());
      await expect(page.locator('.xterm-rows')).toContainText('FAKE-MIRROR', { timeout: 15_000 });
      const doc = await docOf(page);
      await page.goto(rig.origin + '/tasks');
      const t0 = Date.now();
      await Promise.all([
        page.waitForURL((u) => u.pathname === '/login'),
        page.evaluate(() => {
          const f = document.createElement('form');
          f.method = 'POST';
          f.action = '/logout';
          document.body.appendChild(f);
          f.submit();
        }),
      ]);
      for (let i = 0; i < 2; i++) {
        await page.goBack({ waitUntil: 'commit' });
        await expect(page.locator('#muster-password'), `Back #${i + 1} shows the sign-in`).toBeVisible();
      }
      const after = (await shows(page)).filter((s) => s.at >= t0);
      expect(after.filter((s) => s.persisted), `nothing was restored after the sign-out: ${JSON.stringify(after)}`).toEqual([]);
      expect(after.some((s) => s.doc === doc), 'the terminal\'s document was never shown again').toBe(false);
    } finally {
      await ctx.close();
    }
  });

  test('a terminal restored after its session ended reloads to the sign-in', async () => {
    const { ctx, page } = await mirrorPage();
    try {
      await page.goto(rig.origin + term());
      await expect(page.locator('.xterm-rows')).toContainText('FAKE-MIRROR', { timeout: 15_000 });
      const doc = await docOf(page);
      const t0 = Date.now();
      await awayAndBack(page, rig.origin + '/tasks', async () => {
        await ctx.clearCookies();
      });
      await expectRestored(page, await backShow(page, '/mirror/terminal', t0), doc);
      await expect(page.locator('#muster-password'), 'the restored terminal went to the sign-in').toBeVisible({ timeout: 10_000 });
    } finally {
      await ctx.close();
    }
  });

  test('back to a terminal restores it and attaches again', async () => {
    const { ctx, page } = await mirrorPage(390, [streamLog]);
    try {
      await page.goto(rig.origin + term());
      await expect(page.locator('.xterm-rows')).toContainText('FAKE-MIRROR', { timeout: 15_000 });
      const doc = await docOf(page);
      const t0 = Date.now();
      await awayAndBack(page, rig.origin + '/tasks');
      await expectRestored(page, await backShow(page, '/mirror/terminal', t0), doc);
      await expect.poll(() => page.evaluate(() => {
        const w = window as any;
        return w.__socks.filter((s: any) => s.kind === 'ws' && s.url.includes('/mirror/terminal/ws') && s.at >= w.__restoredAt && s.msgs > 0).length;
      }), { message: 'after the restore a new terminal socket was opened and the far side answered on it', timeout: 10_000 }).toBeGreaterThan(0);
      await expect(page.locator('#term-status')).not.toHaveText('Disconnected');
    } finally {
      await ctx.close();
    }
  });
});
