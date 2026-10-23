import { execFileSync } from 'node:child_process';
import { test, expect } from '../lib/fixtures';
import { dsn, repoRoot } from '../lib/env';

// TITLES SHOW AS THE TEXT A READER SEES, NOT AS MARKDOWN (operator report:
// "Titles sometimes include markdown, strip it out"). Every title sink goes
// through internal/textplain.Title at render time; this is the click path:
//
//   - an agent dispatched from a task is named after it ("#<id> · <title>"),
//     and a task title routinely carries markdown. Its card on /agents and its
//     detail page's <title> must read as plain text — with the "#<id>" and the
//     snake_case identifier intact (the previous stripper removed every # and _);
//   - a task created with a markdown title: its /tasks/{id} <title> and <h1>.
//
// The stored values stay as written (the title is rendered plain, not
// rewritten), so the agent row's display_name is set straight in the database,
// as a dispatch writes it, and removed afterwards: the shared server's other
// specs count its agents.

const stamp = Date.now().toString(36);
const agentName = 'e2e-plain-title-' + stamp;
const token = 'e2e-plain-title-token-not-a-real-one-' + stamp;
const markdownName = '#41 · **Fix** [the login](https://example.com/pr/41) for `snake_case_names` ~~today~~';
const plainName = '#41 · Fix the login for snake_case_names today';
let agentID = '';

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

test.beforeAll(() => {
  agentID = fix('agent', '1', agentName, token);
  fix('label', agentID, markdownName);
});

test.afterAll(() => {
  if (agentID) fix('rmagent', '1', agentName);
});

test('a task-derived agent name with markdown reads plain on its card and in its page title', async ({ page }) => {
  await page.goto('/agents');
  const card = page.locator(`#agents-list article#agent-${agentID}`);
  await expect(card).toBeVisible();
  // The card shows the plain title, whole, and none of the markers. (Asserted
  // on the card's text rather than one heading element: a name past the card's
  // clamp moves to a title row of its own, and the raw markdown name is longer
  // than the plain one, so an element check would fail on layout, not text.)
  await expect(card).toContainText(plainName);
  const shown = await card.innerText();
  for (const marker of ['**', '](', '`', '~~']) expect(shown, `the card shows ${marker}`).not.toContain(marker);

  await page.goto('/agents/' + agentName);
  await expect(page).toHaveTitle(plainName + ' · muster');
});

test('a task with a markdown title reads plain in its detail page title and heading', async ({ page, api }) => {
  const t = await api.createTask('**Ship** `v2` for [C#](https://example.com/c) and a*b ' + stamp, 'A synthetic task for the plain-title spec.');
  await page.goto('/tasks/' + t.id);
  const plain = 'Ship v2 for C# and a*b ' + stamp;
  await expect(page).toHaveTitle(plain + ' · muster');
  await expect(page.locator('h1[data-task-title]')).toHaveText(plain);
});
