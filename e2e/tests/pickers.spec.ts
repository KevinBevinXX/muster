import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import { Locator, Page, request as pwRequest } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { hookToken, password, pickersFakePort, pickersPort, repoRoot, versionTerm } from '../lib/env';
import { startServer, Running } from '../lib/server';

// EVERY SEARCH PICKER RANKS BY ONE RULE (internal/searchrank; window.musterRank
// in the browser): the label STARTS with the query, then a WORD in it does, then
// it is anywhere in it, then its letters are in it in order. Four pickers, each
// driven end to end against a real muster-server whose upstreams are fakes this
// spec runs (the e2eterm build's MUSTER_E2E_GITHUB_API / _OPENROUTER_MODELS, and
// a fake permission router behind MUSTER_ROUTER_URL):
//
//   the dispatch dialog's task, repo and model pickers, and the new-task
//   sheet's directory picker.
//
// Each gets four entries for "kw", one per tier, served in the REVERSE of the
// ranked order, so the order can only come out right by being ranked:
//
//   tier 0  starts with kw          tier 1  a word starts with kw
//   tier 2  kw inside a word        tier 3  k … w, in order only
//
// And the browser's copy of the rule is run over the Go copy's own table
// (internal/searchrank/testdata/cases.json): one table, two implementations.

const origin = `http://localhost:${pickersPort}`;
const fake = `http://127.0.0.1:${pickersFakePort}`;
const tag = Date.now().toString(36);

// The four fixtures per picker, in RANKED order (served reversed).
const REPOS = ['kwanza/app', 'acme/old-kwik', 'acme/makwa', 'acme/kiwi'];
const MODELS = ['kwen/kwen-3', 'acme/old-kwik', 'acme/makwa-2', 'acme/kiwi-1'];
const DIRS = ['kwork/base', '/srv/old-kwik', '/srv/makwa', '/srv/kiwi-tools'];
const TASKS = [`kwality pass ${tag}`, `old-kwik fix ${tag}`, `makwa cleanup ${tag}`, `k then w ${tag}`];

let server: Running | undefined;
let upstream: http.Server | undefined;
const taskIDs: number[] = [];

test.use({ storageState: { cookies: [], origins: [] }, viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

test.beforeAll(async () => {
  test.setTimeout(120_000);
  upstream = http.createServer((req, res) => {
    res.setHeader('Content-Type', 'application/json');
    const u = new URL(req.url || '/', fake);
    if (u.pathname === '/user') return res.end(JSON.stringify({ login: 'e2e-picker' }));
    if (u.pathname === '/user/repos') return res.end(JSON.stringify(REPOS.slice().reverse().map((full_name) => ({ full_name, private: false }))));
    if (u.pathname === '/models') return res.end(JSON.stringify({ data: MODELS.slice().reverse().map((id) => ({ id })) }));
    // The permission router's directory read. A real one matches by its own
    // rule; this one answers everything, reversed, and the picker ranks it.
    if (u.pathname === '/api/directories') return res.end(JSON.stringify({ directories: DIRS.slice().reverse() }));
    res.end('{}');
  });
  await new Promise<void>((r) => upstream!.listen(pickersFakePort, '127.0.0.1', () => r()));
  server = await startServer(versionTerm, pickersPort, {
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
    MUSTER_GITHUB_ENCRYPTION_KEY: Buffer.alloc(32, 0x4d).toString('base64'),
    MUSTER_GITHUB_TOKEN: 'ghp_e2e_picker_not_a_real_token',
    MUSTER_E2E_GITHUB_API: fake,
    MUSTER_E2E_OPENROUTER_MODELS: fake + '/models',
    // A router instead of the standalone declaration (the two are exclusive).
    MUSTER_STANDALONE: '',
    MUSTER_ROUTER_URL: fake,
    MUSTER_ROUTER_TOKEN: 'e2e-router-token-not-a-real-one',
  });
  for (const title of TASKS.slice().reverse()) {
    const r = await fetch(origin + '/api/tasks', {
      method: 'POST',
      headers: { Authorization: `Bearer ${hookToken}`, 'Content-Type': 'application/json' },
      body: JSON.stringify({ title, body: 'picker fixture' }),
    });
    expect(r.status, await r.clone().text()).toBeLessThan(300);
    taskIDs.push(((await r.json()) as { id: number }).id);
  }
});

test.afterAll(async () => {
  for (const id of taskIDs) {
    await fetch(`${origin}/api/tasks/${id}`, { method: 'DELETE', headers: { Authorization: `Bearer ${hookToken}` } }).catch(() => undefined);
  }
  // MUSTER_GITHUB_TOKEN stored a GitHub connection for the operator in the
  // shared database: it goes, or every later server's Repos tab is "connected".
  const ctx = await pwRequest.newContext({ baseURL: origin });
  try {
    await ctx.post('/login', { form: { password }, headers: { Origin: origin } });
    const r = await ctx.post('/github/disconnect', { headers: { Origin: origin, 'HX-Request': 'true' } });
    expect(r.status(), 'the GitHub connection this spec made is removed').toBeLessThan(300);
  } finally {
    await ctx.dispose();
  }
  await server?.stop();
  await new Promise<void>((r) => (upstream ? upstream.close(() => r()) : r()));
});

async function signIn(page: Page) {
  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
}

// shown is a combobox's visible options, top to bottom, as labels, kept to the
// fixture set (seeded quick-picks and other rows are someone else's).
const shown = (box: Locator, mine: string[]) =>
  box.locator('[data-combobox-option]').evaluateAll(
    (els, m) => els.filter((e) => !e.classList.contains('hidden')).map((e) => e.getAttribute('data-label') || '').filter((l) => (m as string[]).includes(l)),
    mine,
  );

async function expectRanked(box: Locator, mine: string[], label: string) {
  await box.locator('[data-combobox-input]').fill('kw');
  await expect.poll(() => shown(box, mine), { message: `${label}: "kw" ranked prefix, word start, substring, letters in order` }).toEqual(mine);
  // Closed again, so the open list covers nothing the next step taps.
  await box.locator('[data-combobox-input]').press('Escape');
}

test("the browser's rank agrees with the Go rank on the shared table", async ({ page }) => {
  await signIn(page);
  const table = JSON.parse(fs.readFileSync(path.join(repoRoot, 'internal/searchrank/testdata/cases.json'), 'utf8')) as { cases: { label: string; q: string; tier: number; why: string }[] };
  expect(table.cases.length, 'the table has rows').toBeGreaterThan(10);
  const got = await page.evaluate((cases) => cases.map((c) => (window as any).musterRank(c.label, c.q)), table.cases);
  const bad = table.cases.filter((c, i) => got[i] !== c.tier).map((c, i) => `${c.label} / ${c.q}: ${got[i]} want ${c.tier} (${c.why})`);
  expect(bad).toEqual([]);
});

test('the dispatch dialog ranks its task, repo and model pickers', async ({ page }) => {
  await signIn(page);
  await page.goto(origin + '/agents');
  await page.click('#fab-agents');
  const sheet = page.locator('#agent-modal form');
  await expect(sheet).toBeVisible();
  const task = sheet.locator('[data-combobox]:has(input[name="note_id"])');
  await expect(task.locator('[data-combobox-option]', { hasText: TASKS[0] })).toHaveCount(1, { timeout: 15_000 });
  await expectRanked(task, TASKS, 'tasks');

  await sheet.locator('summary[aria-label="Advanced options"]').click(); // Advanced: model and repository
  const repo = sheet.locator('[data-combobox]:has(input[name="repo"])');
  await expect(repo.locator('[data-combobox-option]', { hasText: REPOS[0] })).toHaveCount(1, { timeout: 15_000 });
  await expectRanked(repo, REPOS, 'repos');

  const model = sheet.locator('[data-combobox][data-combobox-remote="/api/openrouter/models"]');
  await expectRanked(model, MODELS, 'models');
});

test('the new-task sheet ranks its directory picker', async ({ page }) => {
  await signIn(page);
  await page.goto(origin + '/tasks');
  await page.click('#fab-tasks');
  const box = page.locator('#task-modal [data-combobox][data-combobox-remote="/api/directories"]');
  await page.locator('#task-modal summary[aria-label="Advanced options"]').click(); // the directory is an advanced field
  await expect(box).toBeVisible();
  await expect(box).toHaveAttribute('data-directories', 'ok');
  await expectRanked(box, DIRS, 'directories');
});
