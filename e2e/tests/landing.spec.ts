import { test, expect, expectShellShows, navTabKey } from '../lib/fixtures';

// `/` is where every sign-in, claim and invite redirect lands, and it is the
// board. The shell's script used to resolve `/` to a tab key muster does not
// have (the product it was extracted from defaulted to a `requests` tab), so
// on `/` it hid every panel and every FAB: a blank main area under a correct
// URL. A browser that had already saved a tab never showed it — restoreNav
// moved it off `/` first — which is how it survived until a fresh account.
//
// 412 px, the width it was measured at.
test.use({ viewport: { width: 412, height: 915 } });

const taskCards = '[data-tasks-flat] > [id^="task-"]';

test('/ shows the board and its New-task button in a browser with no saved tab', async ({ page }) => {
  await page.addInitScript((k) => localStorage.removeItem(k), navTabKey);
  await page.goto('/');
  await expectShellShows(page, 'tasks', 'Tasks', '/ with no saved tab');
  await expect(page.locator(taskCards).first(), 'the seeded board is visible on /').toBeVisible();
  await expect(page.locator('#fab-tasks')).toBeVisible();
  expect(new URL(page.url()).pathname, '/ is served as itself, not redirected').toBe('/');
});

test('/ shows the board when the saved tab names a tab this shell does not have', async ({ page }) => {
  await page.addInitScript((k) => localStorage.setItem(k, 'requests'), navTabKey);
  await page.goto('/');
  await expectShellShows(page, 'tasks', 'Tasks', '/ with a saved tab that is not a tab');
  await expect(page.locator('#fab-tasks')).toBeVisible();
});

// INVARIANT GUARD, not regression coverage: this held before the fix too. It is
// here so the fix cannot be "ignore the saved tab".
test('/ still restores a saved tab the shell does have', async ({ page }) => {
  await page.addInitScript((k) => localStorage.setItem(k, 'agents'), navTabKey);
  await page.goto('/');
  await expectShellShows(page, 'agents', 'Agents', '/ with agents saved');
  expect(new URL(page.url()).pathname).toBe('/agents');
});

test('from /, the sidebar switches tab and comes back to the board', async ({ page }) => {
  await page.addInitScript((k) => localStorage.removeItem(k), navTabKey);
  await page.goto('/');
  await expectShellShows(page, 'tasks', 'Tasks', '/ before switching');
  await page.click('#sidebar-open');
  await page.click('.sidebar-tab[data-tab="agents"]');
  await expectShellShows(page, 'agents', 'Agents', 'after choosing Agents from /');
  expect(new URL(page.url()).pathname).toBe('/agents');
  await page.goBack();
  await expectShellShows(page, 'tasks', 'Tasks', 'Back, to /');
  expect(new URL(page.url()).pathname).toBe('/');
});
