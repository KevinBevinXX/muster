import http from 'node:http';
import { Browser, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { bfcacheControlPort } from '../lib/env';
import { launchWithBfcache } from '../lib/bfcache';

// THE BACK/FORWARD CACHE CONTROL. lib/bfcache.ts launches a browser in which a
// back navigation can be a restore, as it is in a reader's browser (the suite's
// default one cannot: see there). A later spec that asserts "back to page X is
// restored" (or is not) needs proof that a restore can happen in that browser at
// all, or its green is vacuous.
//
// The pages are a static site this spec serves itself, so the verdict depends
// on the browser and the harness only, never on a muster header:
//
//   - /a: a plain page, nothing that keeps it out of the cache;
//   - /b: the page each of them links to, which sets a cookie when it loads;
//   - /nostore: /a with Cache-Control: no-store — the negative control, so the
//     probe is shown able to say "no".
//
// ⚠ no-store ALONE IS NOT A BLOCKER ANY MORE. Measured (Chromium 153): a
// no-store page IS restored when nothing else changed; it is refused once a
// cookie changed while it was away (CDP reason CacheControlNoStoreCookieModified),
// which is why /b sets one. /a is restored either way.
//
// Each page stamps its document with a random id on load and records every
// pageshow. A restore is the SAME document (same id) shown with
// persisted=true; a reload is a new document.
const origin = `http://127.0.0.1:${bfcacheControlPort}`;

function page(title: string, next: string): string {
  return `<!doctype html><html><head><meta charset="utf-8"><title>${title}</title></head><body>
<h1>${title}</h1><a id="next" href="${next}">next</a>
<script>
  window.__doc = Math.random().toString(36).slice(2);
  window.__shows = [];
  addEventListener('pageshow', function (e) { window.__shows.push(e.persisted); });
</script></body></html>`;
}

let server: http.Server;
test.beforeAll(async () => {
  server = http.createServer((req, res) => {
    const path = (req.url || '/').split('?')[0];
    if (path === '/a') return res.writeHead(200, { 'Content-Type': 'text/html' }).end(page('a', '/b'));
    if (path === '/b') return res.writeHead(200, { 'Content-Type': 'text/html', 'Set-Cookie': `bfcache-control=${Date.now()}; Path=/` }).end(page('b', '/a'));
    if (path === '/nostore') return res.writeHead(200, { 'Content-Type': 'text/html', 'Cache-Control': 'no-store' }).end(page('nostore', '/b'));
    res.writeHead(404).end();
  });
  await new Promise<void>((r) => server.listen(bfcacheControlPort, '127.0.0.1', r));
});
test.afterAll(async () => {
  await new Promise<void>((r) => server.close(() => r()));
});

// awayAndBack opens path, follows its link to the other page, goes Back, and
// reports whether the document Back landed on is the one first opened, and the
// pageshow record of the document it landed on.
async function awayAndBack(p: Page, path: string): Promise<{ same: boolean; shows: boolean[] }> {
  await p.goto(origin + path);
  const first = await p.evaluate(() => (window as any).__doc as string);
  await Promise.all([p.waitForURL(origin + '/b'), p.click('#next')]);
  // 'commit': a restore fires no load event.
  await p.goBack({ waitUntil: 'commit' });
  await expect(p).toHaveURL(origin + path);
  // pageshow on a restore is dispatched after the history navigation commits.
  await expect.poll(() => p.evaluate(() => ((window as any).__shows as boolean[]).length)).toBeGreaterThan(0);
  const back = await p.evaluate(() => ({ doc: (window as any).__doc as string, shows: (window as any).__shows as boolean[] }));
  return { same: back.doc === first, shows: back.shows };
}

async function inBrowser<T>(launch: () => Promise<Browser>, fn: (p: Page) => Promise<T>): Promise<T> {
  const browser = await launch();
  try {
    return await fn(await browser.newPage());
  } finally {
    await browser.close();
  }
}

test('a static page is restored from the back/forward cache in the bfcache browser', async ({ playwright }) => {
  const r = await inBrowser(() => launchWithBfcache(playwright.chromium), (p) => awayAndBack(p, '/a'));
  expect(r, 'Back restored the same document, shown persisted').toEqual({ same: true, shows: [false, true] });
});

test('negative control: a no-store page is reloaded there once a cookie changed', async ({ playwright }) => {
  const r = await inBrowser(() => launchWithBfcache(playwright.chromium), (p) => awayAndBack(p, '/nostore'));
  expect(r, 'Back loaded a new document, not persisted').toEqual({ same: false, shows: [false] });
});
