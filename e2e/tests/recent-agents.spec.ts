import { Browser, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { baseURL, password } from '../lib/env';

// RECENTLY VIEWED, in a real browser on the shared server: a strip above the
// Agents list of the entries the operator last opened, newest first, one tap
// each. It is stored on the SERVER per user (internal/recents, migration
// 0018), so a second browser context — its own sign-in, its own
// sessionStorage — sees the same strip. Pinned entries are left out (they
// already lead the list).
//
// The seeded agents are shared by every spec (one database, one operator), so
// this asserts about the entries it opened itself, in the order it opened
// them, and unpins what it pinned.
//
// Phone-sized with touch: the strip is a row of tap targets.
test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

const strip = (page: Page) => page.locator('#agents-recent');
const entries = (page: Page) => strip(page).locator('a[data-recent-entry]');
const hrefs = (page: Page) => entries(page).evaluateAll((els) => els.map((e) => e.getAttribute('href')));

async function openAgent(page: Page, name: string) {
  await page.goto('/agents/' + name);
  await page.waitForURL((u) => u.pathname === '/agents/' + name && u.searchParams.has('session'));
}

async function openList(page: Page) {
  await page.goto('/agents');
  await expect(page.locator('#agents-list > article').first()).toBeVisible();
}

async function pin(page: Page, agentID: string, on: boolean) {
  const r = await page.request.post(on ? '/agents/pins' : '/agents/pins/delete', {
    form: { kind: 'agent', agent_id: agentID },
    headers: { Origin: baseURL, 'HX-Request': 'true' },
  });
  expect(r.status(), await r.text()).toBe(200);
}

async function freshContext(browser: Browser) {
  const ctx = await browser.newContext({ baseURL, viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });
  const page = await ctx.newPage();
  await page.goto('/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
  return { ctx, page };
}

let pinnedID = '';
test.afterEach(async ({ page }) => {
  if (pinnedID) await pin(page, pinnedID, false).catch(() => undefined);
  pinnedID = '';
});

// 🔴 A REGRESSION TEST: on the commit before, there is no #agents-recent.
test('a visit shows in the strip, newest first, one tap to open; it persists to another browser; pinned entries are left out', async ({ page, browser }) => {
  await openAgent(page, 'docs-sweeper');
  await openAgent(page, 'release-helper');
  await openList(page);

  // Newest first: the two just opened lead the strip.
  await expect(strip(page)).toBeVisible();
  await expect.poll(() => hrefs(page)).toEqual(expect.arrayContaining(['/agents/release-helper', '/agents/docs-sweeper']));
  const h = await hrefs(page);
  expect(h.slice(0, 2), 'the last opened first').toEqual(['/agents/release-helper', '/agents/docs-sweeper']);
  expect(h.length, 'at most six').toBeLessThanOrEqual(6);

  // Above the list, and not one of its cards: the filter's count is of cards only.
  await expect(page.locator('#agents-list #agents-recent')).toHaveCount(0);
  const sb = (await strip(page).boundingBox())!;
  const lb = (await page.locator('#agents-list').boundingBox())!;
  expect(sb.y + sb.height, 'the strip is above the list').toBeLessThanOrEqual(lb.y);
  const cards = await page.locator('#agents-list > article').count();
  await expect(page.locator('#agents-count')).toHaveText(`${cards} of ${cards}`);
  // Condensed and thumb-sized, and it never widens the page.
  const first = entries(page).first();
  expect((await first.boundingBox())!.height, 'a 44px tap target').toBeGreaterThanOrEqual(44);
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw, 'no sideways page scroll').toBeLessThanOrEqual(cw);

  // Revisiting moves an entry to the front rather than adding it twice.
  await openAgent(page, 'docs-sweeper');
  await openList(page);
  await expect.poll(async () => (await hrefs(page)).slice(0, 2)).toEqual(['/agents/docs-sweeper', '/agents/release-helper']);
  expect((await hrefs(page)).filter((x) => x === '/agents/docs-sweeper'), 'one entry per agent').toHaveLength(1);

  // One tap opens it.
  await Promise.all([page.waitForURL((u) => u.pathname === '/agents/docs-sweeper'), first.tap()]);

  // Another browser — its own sign-in and its own sessionStorage — sees the same strip.
  const other = await freshContext(browser);
  try {
    await openList(other.page);
    await expect.poll(async () => (await hrefs(other.page)).slice(0, 2)).toEqual(['/agents/docs-sweeper', '/agents/release-helper']);
  } finally {
    await other.ctx.close();
  }

  // Pinned entries are left out — and come back when unpinned.
  await openList(page);
  pinnedID = (await page.locator('#agents-list > article:has(a[href="/agents/docs-sweeper"])').getAttribute('id'))!.replace('agent-', '');
  await pin(page, pinnedID, true);
  await openList(page);
  await expect(page.locator('#agents-list > article').first(), 'pinned first in the list').toHaveId('agent-' + pinnedID);
  await expect.poll(() => hrefs(page)).not.toContain('/agents/docs-sweeper');
  expect((await hrefs(page))[0], 'the next newest moves up').toBe('/agents/release-helper');
  await pin(page, pinnedID, false);
  pinnedID = '';
  await openList(page);
  await expect.poll(async () => (await hrefs(page))[0]).toBe('/agents/docs-sweeper');
});
