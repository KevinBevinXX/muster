import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { mkchat } from '../lib/mirrorhost';

// THE CHIEF PANEL'S THREAD SEARCH, in a real browser: one letter searches, and
// the matches come back RANKED while typing (agents.PGStore.SearchSessions) — a
// title that starts with the query, then a title with a word that starts with
// it, then any other match (a substring, or the message body only), most
// recently active first within each. Clearing the box gives back the plain
// recent list.
//
// The chief is an agent of this spec's own (display name "chief", e2e/mkchat's
// threads mode), removed afterwards. Its threads are aged AGAINST the rank —
// the prefix threads oldest — so recency alone would print the reverse.

const chief = 'e2e-search-chief-' + Date.now().toString(36);

test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

test.beforeAll(() => {
  mkchat(chief, null);
  mkchat(
    chief,
    [
      { title: 'Kestrel notes', hoursAgo: 50, messages: [{ role: 'user', content: 'first' }] }, // rank 0, oldest
      { title: 'kestrel follow-up', hoursAgo: 40, messages: [{ role: 'user', content: 'second' }] }, // rank 0
      { title: 'about the kestrel', hoursAgo: 30, messages: [{ role: 'user', content: 'third' }] }, // rank 1
      { title: 'mykestrel', hoursAgo: 20, messages: [{ role: 'user', content: 'fourth' }] }, // rank 2
      { title: 'unrelated', hoursAgo: 10, messages: [{ role: 'user', content: 'the kestrel came back' }] }, // rank 2, body only
      { title: 'nothing here', hoursAgo: 1, messages: [{ role: 'user', content: 'nope' }] }, // no match
    ],
    { MUSTER_E2E_DISPLAY_NAME: 'chief', MUSTER_E2E_THREADS: '1' },
  );
});

test.afterAll(() => {
  mkchat(chief, null);
});

// The panel has no launcher on these pages; its toggle is delegated on the
// document, so a launcher's markup clicked by script opens it.
async function openThreads(page: Page) {
  await page.goto('/tasks');
  await page.evaluate(() => {
    const b = document.createElement('button');
    b.setAttribute('data-chief-panel-toggle', '');
    b.style.cssText = 'position:fixed;left:-9999px;top:0';
    document.body.appendChild(b);
    b.click();
  });
  await expect(page.locator('#chief-panel')).toHaveAttribute('data-chief-panel-open', '1');
  await expect(page.locator('#chief-thread-search')).toBeAttached({ timeout: 15_000 });
  await page.locator('label[for="chief-threads"]').first().click();
  await expect(page.locator('#chief-thread-search')).toBeVisible();
}

const titles = (page: Page) =>
  page.locator('#chief-thread-results [data-chief-thread] span.truncate').evaluateAll((els) => els.map((e) => (e.textContent || '').trim()));

test('one letter searches; the matches are ranked prefix, word start, then the rest, newest first in each; clearing restores the list', async ({ page }) => {
  await openThreads(page);
  const recent = ['nothing here', 'unrelated', 'mykestrel', 'about the kestrel', 'kestrel follow-up', 'Kestrel notes'];
  await expect.poll(async () => (await titles(page)).slice(0, 6), { message: 'the plain list, most recent first' }).toEqual(recent);

  const ranked = ['kestrel follow-up', 'Kestrel notes', 'about the kestrel', 'unrelated', 'mykestrel'];
  const search = page.locator('#chief-thread-search');
  await search.fill('k');
  await expect.poll(() => titles(page), { message: 'k: one letter, ranked' }).toEqual(ranked);
  await expect(page.locator('#chief-thread-results')).toHaveAttribute('data-chief-threads-state', 'rows');
  // Typing on re-ranks with the longer query; the body-only match keeps its snippet.
  await search.fill('kest');
  await expect.poll(() => titles(page), { message: 'kest' }).toEqual(ranked);
  await expect(page.locator('#chief-thread-results [data-chief-thread]', { hasText: 'unrelated' })).toContainText('the kestrel came back');
  // A query only one thread has.
  await search.fill('ab');
  await expect.poll(() => titles(page), { message: 'ab' }).toEqual(['about the kestrel']);

  // Cleared: the plain recent list again.
  await search.fill('');
  await expect.poll(async () => (await titles(page)).slice(0, 6), { message: 'cleared' }).toEqual(recent);
});
