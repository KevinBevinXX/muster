import { test, expect } from '../lib/fixtures';

// The real install prompt cannot be produced in automation (it needs user
// engagement the harness cannot fake), so the browser's event is synthesised
// with a stubbed prompt() — what is under test is OUR handling of it.
const fire = `(() => {
  const e = new Event('beforeinstallprompt', { cancelable: true });
  window.__prompted = 0;
  e.prompt = () => { window.__prompted++; return Promise.resolve(); };
  e.userChoice = Promise.resolve({ outcome: 'accepted', platform: 'web' });
  window.dispatchEvent(e);
  return e.defaultPrevented;
})()`;

test('the install button appears on beforeinstallprompt and prompts once', async ({ page }) => {
  await page.goto('/tasks');
  await page.click('#sidebar-open');
  const btn = page.locator('#sidebar [data-install-app]');
  await expect(btn).toBeHidden();
  expect(await page.evaluate(fire), 'the default mini-infobar is suppressed').toBe(true);
  await expect(btn).toBeVisible();
  await btn.click();
  expect(await page.evaluate(() => (window as unknown as { __prompted: number }).__prompted)).toBe(1);
  await expect(btn).toBeHidden();
});

test('appinstalled hides it', async ({ page }) => {
  await page.goto('/tasks');
  await page.click('#sidebar-open');
  await page.evaluate(fire);
  const btn = page.locator('#sidebar [data-install-app]');
  await expect(btn).toBeVisible();
  await page.evaluate(() => window.dispatchEvent(new Event('appinstalled')));
  await expect(btn).toBeHidden();
});

test('never offered inside the installed app', async ({ page }) => {
  await page.addInitScript(() => {
    const orig = window.matchMedia.bind(window);
    window.matchMedia = (q: string) =>
      q.includes('display-mode: standalone') ? ({ matches: true, media: q, addEventListener() {}, removeEventListener() {} } as unknown as MediaQueryList) : orig(q);
  });
  await page.goto('/tasks');
  await page.click('#sidebar-open');
  await page.evaluate(fire);
  await page.waitForTimeout(300);
  await expect(page.locator('#sidebar [data-install-app]')).toBeHidden();
});

// 🔴 EVERY INSTALLED DISPLAY MODE, NOT ONLY standalone. The operator saw the
// button inside the installed app; the old check asked ONLY
// `(display-mode: standalone)`, so an installed window in fullscreen,
// minimal-ui or window-controls-overlay (a browser may fall back along the
// manifest's display chain, and a desktop app window can switch to
// window-controls-overlay at runtime) still offered "Install app" — measured
// red on the old script for those three, green for standalone.
//
// Playwright's emulateMedia has no display-mode, so matchMedia is overridden in
// an init script: a query matches when it names THIS mode, exactly as a real
// browser answers a single-feature query. `browser` is the positive control —
// it is the not-installed mode, and the button MUST appear under it, or this
// override is hiding the button for every mode and every case below is vacuous.
for (const mode of ['standalone', 'fullscreen', 'minimal-ui', 'window-controls-overlay', 'browser']) {
  test(`display-mode ${mode}: ${mode === 'browser' ? 'offered (control)' : 'never offered'}`, async ({ page }) => {
    await page.addInitScript((m: string) => {
      const orig = window.matchMedia.bind(window);
      window.matchMedia = (q: string) =>
        /display-mode/.test(q)
          ? ({ matches: q.includes(`display-mode: ${m})`), media: q, addEventListener() {}, removeEventListener() {} } as unknown as MediaQueryList)
          : orig(q);
    }, mode);
    await page.goto('/tasks');
    await page.click('#sidebar-open');
    await page.evaluate(fire);
    const btn = page.locator('#sidebar [data-install-app]');
    if (mode === 'browser') {
      await expect(btn).toBeVisible();
      return;
    }
    await page.waitForTimeout(300);
    await expect(btn).toBeHidden();
    // A boosted navigation re-runs the htmx:load path, which re-decides
    // visibility; it must reach the same answer.
    await page.evaluate(() => document.dispatchEvent(new Event('htmx:load')));
    await expect(btn).toBeHidden();
  });
}

// The mode can change AFTER the offer arrives (installing moves a desktop tab
// into an app window). The re-check on every show is what hides it then; the
// beforeinstallprompt check alone cannot, because the offer was made in a
// browser tab.
test('a window that becomes installed after the offer stops offering it', async ({ page }) => {
  await page.addInitScript(() => {
    const w = window as unknown as { __mode: string };
    w.__mode = 'browser';
    const orig = window.matchMedia.bind(window);
    window.matchMedia = (q: string) =>
      /display-mode/.test(q)
        ? ({ matches: q.includes(`display-mode: ${w.__mode})`), media: q, addEventListener() {}, removeEventListener() {} } as unknown as MediaQueryList)
        : orig(q);
  });
  await page.goto('/tasks');
  await page.click('#sidebar-open');
  await page.evaluate(fire);
  const btn = page.locator('#sidebar [data-install-app]');
  await expect(btn).toBeVisible();
  await page.evaluate(() => {
    (window as unknown as { __mode: string }).__mode = 'minimal-ui';
    document.dispatchEvent(new Event('htmx:load'));
  });
  await expect(btn).toBeHidden();
});
