import { test, expect } from '../lib/fixtures';
import { Page } from '@playwright/test';

// Phone-width layout at the narrowest common Android width (360px).
test.use({ viewport: { width: 360, height: 740 }, hasTouch: true, isMobile: true });

const pages = ['/tasks', '/agents', '/tasks/1', '/tasks/999999', '/agents/docs-sweeper'];

async function settle(page: Page) {
  await page.waitForLoadState('networkidle').catch(() => undefined);
  await page.waitForTimeout(300);
}

test('no main page scrolls sideways at 360px', async ({ page }) => {
  for (const p of pages) {
    await page.goto(p);
    await settle(page);
    const [scroll, client] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
    expect(scroll, `${p}: scrollWidth ${scroll} > clientWidth ${client}`).toBeLessThanOrEqual(client);
  }
});

// Every VISIBLE control a thumb is meant to hit is at least 44×44 CSS px.
// The board card's stretched link is excluded: its hit area is the whole card
// (a ::after overlay), not the anchor's own box.
const controlsJS = `(() => {
  const vis = (e) => { const s = getComputedStyle(e); const r = e.getBoundingClientRect();
    return s.visibility !== 'hidden' && s.display !== 'none' && r.width > 0 && r.height > 0 &&
      r.right > 0 && r.left < innerWidth && !e.closest('[aria-hidden="true"]') && !e.closest('.sr-only'); };
  const out = [];
  for (const e of document.querySelectorAll('a[href], button, select, input:not([type=hidden]):not([type=checkbox]):not([type=radio]), textarea, summary')) {
    if (!vis(e) || e.classList.contains('card-link')) continue;
    const r = e.getBoundingClientRect();
    if (r.width < 44 || r.height < 44) out.push(e.tagName.toLowerCase() + (e.id ? '#' + e.id : '') + ' ' + Math.round(r.width) + 'x' + Math.round(r.height) + ' ' + JSON.stringify((e.getAttribute('aria-label') || e.textContent || '').trim().slice(0, 30)));
  }
  return out;
})()`;

const fieldsJS = `(() => [...document.querySelectorAll('input:not([type=hidden]):not([type=checkbox]):not([type=radio]), select, textarea')]
  .filter((e) => { const r = e.getBoundingClientRect(); return r.width > 0 && r.height > 0 && getComputedStyle(e).visibility !== 'hidden'; })
  .map((e) => ({ name: e.name || e.id || e.tagName, px: parseFloat(getComputedStyle(e).fontSize) }))
  .filter((f) => f.px < 16))()`;

test('every visible control is a 44px target and every field is 16px text', async ({ page }) => {
  // A control audit that matched nothing would report zero violations for any
  // page; require it to have measured a real number of controls.
  let measured = 0;
  const check = async (label: string) => {
    await settle(page);
    measured += await page.evaluate(() => document.querySelectorAll('a[href], button, select, textarea, input').length);
    expect(await page.evaluate(controlsJS), `${label}: controls under 44px`).toEqual([]);
    expect(await page.evaluate(fieldsJS), `${label}: fields under 16px (mobile browsers zoom on focus)`).toEqual([]);
  };
  for (const p of pages) {
    await page.goto(p);
    await check(p);
  }
  await page.goto('/tasks');
  await settle(page);
  await page.click('#fab-tasks');
  await expect(page.locator('#task-modal textarea[name="body"]')).toBeVisible();
  await check('new-task sheet');
  await page.goto('/agents');
  await settle(page);
  await page.click('#fab-agents');
  await expect(page.locator('#agent-modal form')).toBeVisible();
  await check('dispatch sheet');
  expect(measured, 'the audit measured real controls').toBeGreaterThan(100);
});

test('a sheet keeps its primary action on screen with Advanced open', async ({ page }) => {
  await page.setViewportSize({ width: 360, height: 640 });
  for (const [path, fab, label] of [['/tasks', '#fab-tasks', 'Save task'], ['/agents', '#fab-agents', 'Dispatch']] as const) {
    await page.goto(path);
    await settle(page);
    await page.click(fab);
    const sheet = page.locator('.fixed:not(.hidden) form').first();
    await expect(sheet).toBeVisible();
    await sheet.locator('summary').first().click();
    await page.waitForTimeout(300);
    const box = await page.getByRole('button', { name: label, exact: true }).boundingBox();
    expect(box, `${label} has a box`).toBeTruthy();
    expect(box!.y + box!.height, `${label} bottom edge is inside the 640px viewport`).toBeLessThanOrEqual(640);
    expect(box!.y).toBeGreaterThanOrEqual(0);
  }
});
