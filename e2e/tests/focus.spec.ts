import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { binDir, dsn, focusPort, password, repoRoot, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';

// SESSION FOCUS ON THE CARDS, in a real browser, against a real muster-server
// (MUSTER_MIRROR=1, its own port) and a real muster-mirror over the tunnel (the
// `-tags e2emirror` per-window fake). A dispatched agent of this spec's own
// (e2e/mkchat) and a mirrored Claude Code window each carry a focus (written by
// mkchat's MUSTER_E2E_FOCUS — the mirrored one keyed by the window's CLAUDE
// SESSION ID, which the host reports from the transcript's name). On a phone
// and on a desktop, each card's footer chip opens a popover with the title,
// the description and "updated … ago", inside the viewport; and the filter
// box finds each card by a word only its focus carries.

const origin = `http://localhost:${focusPort}`;
const stamp = Date.now().toString(36);
const host = 'e2e-focus-' + stamp;
const agentName = 'e2e-focus-' + stamp;
const sid = '0b2c3d4e-5f60-4718-8a9b-' + stamp.padStart(12, '0').slice(-12);
const mirrorBin = path.join(binDir, 'muster-mirror-e2e');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'focus-'));
const windowsFile = path.join(dir, 'windows');
const transcript = path.join(dir, sid + '.jsonl');
const focusEnv = JSON.stringify([
  { subject: 'self', title: 'Reconciling the quarterly ledgers', description: 'Matching payouts to the bank export. Two left.' },
  { subject: 'mirror:' + sid, title: 'Bisecting the flaky tunnel test', description: 'The backoff resets too early.' },
]);
let server: Running | undefined;
let mirrorProc: ChildProcess | undefined;
let mirrorLog = '';
let agentCleanup = false;
let signedIn: Awaited<ReturnType<BrowserContext['cookies']>> | undefined;

function mkchat(extra: NodeJS.ProcessEnv, input = '') {
  execFileSync('go', ['run', './e2e/mkchat'], {
    cwd: repoRoot,
    input,
    env: { ...process.env, DATABASE_URL: dsn, MUSTER_E2E_AGENT: agentName, MUSTER_E2E_FOCUS: focusEnv, ...extra },
  });
}

test.beforeAll(async () => {
  execFileSync('go', ['build', '-tags', 'e2emirror', '-o', mirrorBin, './cmd/muster-mirror'], { cwd: repoRoot, stdio: 'inherit' });
  agentCleanup = true;
  mkchat({}, JSON.stringify([{ role: 'user', content: 'Reconcile the ledgers.' }]));
  server = await startServer(versionTerm, focusPort, {
    MUSTER_MIRROR: '1',
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
    MUSTER_FOCUS_INTERVAL: '0',
  });
  const token = execFileSync(serverBinary(versionTerm), ['mirror', 'token', 'create', host], { env: { ...process.env, DATABASE_URL: dsn } })
    .toString()
    .trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
  fs.writeFileSync(transcript, '');
  const now = Math.floor(Date.now() / 1000);
  // session|id|index|active|command|activity|claude-transcript
  fs.writeFileSync(windowsFile, `focuswin|@1|1|1|claude|${now - 60}|${transcript}\nfocuswin|@2|2|0|zsh|${now - 120}|\n`);
  mirrorProc = spawn(mirrorBin, [], {
    env: {
      ...process.env,
      MUSTER_MIRROR_URL: origin,
      MUSTER_MIRROR_ALLOW_HTTP: '1',
      MUSTER_MIRROR_TOKEN: token,
      MUSTER_MIRROR_HOST: host,
      MUSTER_MIRROR_E2E_WINDOWS_FILE: windowsFile,
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  mirrorProc.stdout?.on('data', (d) => (mirrorLog += d));
  mirrorProc.stderr?.on('data', (d) => (mirrorLog += d));
});

test.afterAll(async () => {
  if (mirrorProc && mirrorProc.exitCode === null) {
    const done = new Promise((r) => mirrorProc!.once('exit', r));
    mirrorProc.kill('SIGTERM');
    await done;
  }
  await server?.stop();
  if (agentCleanup) mkchat({ MUSTER_E2E_DELETE: '1' });
  fs.rmSync(dir, { recursive: true, force: true });
});

async function login(page: Page) {
  if (signedIn) {
    await page.context().addCookies(signedIn);
    return;
  }
  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
  signedIn = await page.context().cookies(origin);
}

const claudeCard = (page: Page) =>
  page.locator(`#agents-list article[data-mirror-card]:has(a[href*="host=${host}"][href*="window=%401"])`);
const shellCard = (page: Page) =>
  page.locator(`#agents-list article[data-mirror-card]:has(a[href*="host=${host}"][href*="window=%402"])`);
const agentCard = (page: Page) => page.locator(`#agents-list article[id^="agent-"]:has(a[href="/agents/${agentName}"])`);

async function openList(page: Page) {
  await login(page);
  await page.goto(origin + '/agents');
  await expect(claudeCard(page), `the host's Claude window (log:\n${mirrorLog})`).toHaveCount(1, { timeout: 20_000 });
  await expect(agentCard(page)).toHaveCount(1);
}

for (const vp of [
  { name: 'phone 390×844 touch', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.name, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });

    for (const c of [
      { kind: 'agent card', card: agentCard, title: 'Reconciling the quarterly ledgers', desc: 'Matching payouts to the bank export. Two left.' },
      { kind: 'mirror card', card: claudeCard, title: 'Bisecting the flaky tunnel test', desc: 'The backoff resets too early.' },
    ]) {
      test(`the ${c.kind}'s focus chip opens a popover with the focus`, async ({ page }) => {
        await openList(page);
        const card = c.card(page);
        const chip = card.locator('[data-card-focus]');
        await expect(chip).toContainText(c.title);
        const box = (await chip.boundingBox())!;
        expect(box.height, 'the chip is a 44px tap target').toBeGreaterThanOrEqual(44);
        const pop = card.locator('[data-focus-popover]');
        await expect(pop).toBeHidden();
        if (vp.use.hasTouch) await chip.tap();
        else await chip.click();
        await expect(pop).toBeVisible();
        await expect(pop.locator('[data-focus-title]')).toHaveText(c.title);
        await expect(pop.locator('[data-focus-description]')).toHaveText(c.desc);
        await expect(pop.locator('[data-focus-updated]')).toContainText('updated');
        await expect(pop.locator('[data-focus-updated]')).toContainText('ago');
        // Placed (no longer invisible) and inside the viewport.
        await expect.poll(() => pop.evaluate((e) => getComputedStyle(e).visibility)).toBe('visible');
        const pb = (await pop.boundingBox())!;
        expect(pb.x, 'the popover starts inside the viewport').toBeGreaterThanOrEqual(0);
        expect(pb.x + pb.width, 'the popover ends inside the viewport').toBeLessThanOrEqual(vp.use.viewport.width);
        expect(pb.y + pb.height).toBeLessThanOrEqual(vp.use.viewport.height);
        // The chip is outside the card's nav anchor: opening it did not navigate.
        expect(new URL(page.url()).pathname).toBe('/agents');
        await page.keyboard.press('Escape');
        await expect(pop).toBeHidden();
      });
    }

    test('a window whose host reports no Claude session has no chip, and the filter finds cards by their focus', async ({ page }) => {
      await openList(page);
      await expect(shellCard(page)).toHaveCount(1);
      await expect(shellCard(page).locator('[data-card-focus]')).toHaveCount(0);
      const search = page.locator('#agents-search');
      await search.fill('quarterly ledgers');
      await expect(agentCard(page)).toBeVisible();
      await expect(claudeCard(page)).toBeHidden();
      await search.fill('flaky tunnel');
      await expect(claudeCard(page)).toBeVisible();
      await expect(agentCard(page)).toBeHidden();
      await expect(shellCard(page)).toBeHidden();
    });
  });
}
