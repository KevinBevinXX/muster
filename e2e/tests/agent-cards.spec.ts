import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { test, expect } from '../lib/fixtures';
import { binDir, cardsConnectPort, cardsPort, dsn, password, repoRoot, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { writeFileAtomic } from '../lib/atomic';

// THE AGENTS LIST'S CARDS, the operator's click path, every hop real:
//
//   - ATTENTION BADGE: an agent asks over the real /mcp door, and a mirrored
//     Claude Code window (a real muster-mirror over the tunnel, -tags
//     e2emirror) waits at a prompt. Each card shows a badge with its count and
//     the most urgent kind's icon; a second ask raises the agent's count LIVE
//     (no reload); tapping the badge opens that item on /attention — never the
//     card.
//   - REPO CHIP: the window's Claude Code works in a SUBDIRECTORY of a git
//     repository; its host names the repository (the git top level's name) and
//     the card shows it — and typing the repo's name in the filter finds it.
//   - EVERY WINDOW HAS ONE: a plain shell window (no Claude Code at all) in
//     another repository shows that repository's chip too, and is found by it.
//
// A password-mode server: the operator (user 1) owns the agent and the host.

const origin = `http://localhost:${cardsPort}`;
const connectURL = `http://127.0.0.1:${cardsConnectPort}`;
const stamp = Date.now().toString(36);
const host = 'e2e-cards-' + stamp;
const sid = 'cccccccc-' + stamp;
const agentName = 'e2e-cards-agent-' + stamp;
const agentToken = 'e2e-cards-agent-token-not-a-real-one-' + stamp;
const mirrorBin = path.join(binDir, 'muster-mirror-e2e');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'cards-'));
const tr = path.join(dir, sid + '.jsonl');
// A repository whose name appears nowhere else on the page: not in the host,
// the session, the window or the agent.
const repoName = 'quokkafixture';
const repoDir = path.join(dir, 'src', repoName);
// The shell window's repository, at its root.
const shellRepo = 'wombatfixture';
const shellRepoDir = path.join(dir, 'src', shellRepo);
let server: Running | undefined;
let mirrorProc: ChildProcess | undefined;
let mirrorLog = '';
let agentMade = false;
let agentID = '';

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(240_000);
  execFileSync('go', ['build', '-tags', 'e2emirror', '-o', mirrorBin, './cmd/muster-mirror'], { cwd: repoRoot, stdio: 'inherit' });
  server = await startServer(versionTerm, cardsPort, {
    MUSTER_MIRROR: '1',
    MUSTER_FOCUS_INTERVAL: '0',
    MUSTER_CONNECTOR_ENCRYPTION_KEY: Buffer.alloc(32, 0x4e).toString('base64'),
    MUSTER_CONNECT_PORT: String(cardsConnectPort),
    MUSTER_AGENT_CONNECT_URL: connectURL,
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
  });
  agentID = fix('agent', '1', agentName, agentToken);
  agentMade = true;
  const token = execFileSync(serverBinary(versionTerm), ['mirror', 'token', 'create', host], { env: { ...process.env, DATABASE_URL: dsn } })
    .toString()
    .trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
  // The repository, and the subdirectory the window's Claude Code runs in.
  fs.mkdirSync(path.join(repoDir, '.git'), { recursive: true });
  fs.mkdirSync(path.join(repoDir, 'deploy', 'scripts'), { recursive: true });
  fs.mkdirSync(path.join(shellRepoDir, '.git'), { recursive: true });
  fs.writeFileSync(tr, JSON.stringify({ type: 'user', message: { role: 'user', content: 'rolling the fleet' } }) + '\n');
  // Claude Code idle at first; the first test puts it waiting while the list
  // is open.
  writeFileAtomic(tr + '.status', JSON.stringify({ status: 'idle', statusUpdatedAt: Date.now() - 60_000 }));
  const now = Math.floor(Date.now() / 1000);
  const windows = path.join(dir, 'windows');
  writeFileAtomic(
    windows,
    [
      `fleet|@5|5|1|claude|${now - 60}|${tr}||${path.join(repoDir, 'deploy', 'scripts')}`,
      `fleet|@6|6|0|zsh|${now - 120}|||${shellRepoDir}`,
      `notes|@7|0|1|vim|${now - 180}|`,
    ].join('\n') + '\n',
  );
  mirrorProc = spawn(mirrorBin, [], {
    env: {
      ...process.env,
      MUSTER_MIRROR_URL: origin,
      MUSTER_MIRROR_ALLOW_HTTP: '1',
      MUSTER_MIRROR_TOKEN: token,
      MUSTER_MIRROR_HOST: host,
      MUSTER_MIRROR_E2E_WINDOWS_FILE: windows,
      MUSTER_MIRROR_E2E_REFRESH: '300ms',
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  mirrorProc.stdout?.on('data', (d) => (mirrorLog += d));
  mirrorProc.stderr?.on('data', (d) => (mirrorLog += d));
});

test.afterAll(async () => {
  try {
    if (agentMade) fix('cleanup', '1', agentName);
  } finally {
    if (mirrorProc && mirrorProc.exitCode === null) {
      const done = new Promise((r) => mirrorProc!.once('exit', r));
      mirrorProc.kill('SIGTERM');
      await done;
    }
    await server?.stop();
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

async function ask(question: string): Promise<number> {
  const r = await fetch(connectURL + '/mcp', {
    method: 'POST',
    headers: { 'X-Muster-Token': agentToken, 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream' },
    body: JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'tools/call', params: { name: 'attention_ask', arguments: { question } } }),
  });
  expect(r.status, 'the /mcp door answered').toBe(200);
  const j = (await r.json()) as { result: { content: { text: string }[]; isError?: boolean } };
  expect(j.result.isError, j.result.content[0].text).toBeFalsy();
  return (JSON.parse(j.result.content[0].text) as { id: number }).id;
}

async function signIn(page: import('@playwright/test').Page) {
  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
}

// reportWork is what ccd's work reporter sends (cmd/ccd/work.go): POST
// /agent/work on the connect listener, with the agent's own token.
async function reportWork(body: object, token = agentToken): Promise<number> {
  const r = await fetch(connectURL + '/agent/work', {
    method: 'POST',
    headers: { 'X-Muster-Token': token, 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  return r.status;
}

const mirrorCard = (page: import('@playwright/test').Page) => page.locator('#agents-list > article[data-mirror-card]').filter({ hasText: 'fleet:5' });

test('each card shows its attention badge, live, and tapping it opens the item in the queue', async ({ page }) => {
  test.setTimeout(120_000);
  await signIn(page);
  const first = await ask('Ship the importer today?');
  await page.goto(origin + '/agents');
  const agentCard = page.locator('#agents-list > article').filter({ hasText: agentName });
  const agentBadge = agentCard.locator('[data-attention-badge]');
  await expect(agentBadge).toHaveAttribute('data-count', '1', { timeout: 20_000 });
  await expect(agentBadge).toHaveAttribute('data-kind', 'ask');
  await expect(agentBadge).toContainText('❓');
  await expect(mirrorCard(page), mirrorLog).toBeVisible({ timeout: 30_000 });
  const winBadge = mirrorCard(page).locator('[data-attention-badge]');
  await expect(winBadge, 'control: an idle window has no badge').toHaveCount(0);
  let navigations = 0;
  page.on('framenavigated', (f) => {
    if (f === page.mainFrame()) navigations++;
  });

  // The window goes waiting at a prompt while the list is open: its card's
  // badge appears LIVE. Nothing but the queue's own event says so — a status
  // change draws no card, so no list nudge carries it.
  writeFileAtomic(tr + '.status', JSON.stringify({ status: 'waiting', statusUpdatedAt: Date.now() - 1_000 }));
  await expect(winBadge, mirrorLog).toHaveAttribute('data-kind', 'session', { timeout: 30_000 });
  await expect(winBadge).toHaveAttribute('data-count', '1');
  await expect(winBadge).toContainText('🔒');

  // A second ask while the list is open raises the agent's count too.
  const second = await ask('Rotate the staging key too?');
  await expect(agentBadge).toHaveAttribute('data-count', '2', { timeout: 15_000 });
  expect(navigations, 'the badge moved without a page load').toBe(0);
  await expect(agentBadge).toHaveAttribute('href', `/attention#ask-${second}`);

  // Tapping the badge opens THAT item on the queue — not the agent's page.
  await Promise.all([page.waitForURL((u) => u.pathname === '/attention'), agentBadge.tap()]);
  expect(new URL(page.url()).hash).toBe(`#ask-${second}`);
  await expect(page.locator(`#ask-${second}`)).toBeVisible();
  await expect(page.locator(`#ask-${first}`)).toBeVisible();
});

test('typing the repo name in the filter finds the mirrored card, which shows it as a chip', async ({ page }) => {
  test.setTimeout(60_000);
  await signIn(page);
  await page.goto(origin + '/agents');
  const card = mirrorCard(page);
  await expect(card.locator('[data-mirror-repo]'), mirrorLog).toHaveText(new RegExp(`repo\\s*${repoName}`), { timeout: 30_000 });
  // The card's name is unchanged: its window, not its repo.
  await expect(card.locator('[data-mirror-name]')).toHaveText('fleet:5');
  const search = page.locator('#agents-search');
  await search.fill(repoName.slice(0, 6));
  await expect(card).toBeVisible();
  await expect(page.locator('#agents-list > article').filter({ hasText: agentName })).toBeHidden();
  await expect(page.locator('#agents-count')).toHaveText(/^1 of \d+$/);
  // The field token finds it too.
  await search.fill(`repo:${repoName}`);
  await expect(card).toBeVisible();
  await expect(page.locator('#agents-count')).toHaveText(/^1 of \d+$/);
});

test('a plain shell window shows its repo as a chip too, and the filter finds it by it', async ({ page }) => {
  test.setTimeout(60_000);
  await signIn(page);
  await page.goto(origin + '/agents');
  const shell = page.locator('#agents-list > article[data-mirror-card]').filter({ hasText: 'fleet:6' });
  // Every window keeps its card, a shell's included.
  await expect(shell, mirrorLog).toHaveCount(1, { timeout: 30_000 });
  await expect(shell.locator('[data-mirror-kind]')).toContainText('shell');
  await expect(shell.locator('[data-mirror-repo]')).toHaveText(new RegExp(`repo\\s*${shellRepo}`), { timeout: 30_000 });
  // The editor's pane has no working directory the host could read: no chip.
  await expect(page.locator('#agents-list > article[data-mirror-card]').filter({ hasText: 'notes:0' }).locator('[data-mirror-repo]')).toHaveCount(0);
  await page.locator('#agents-search').fill(shellRepo.slice(0, 6));
  await expect(shell).toBeVisible();
  await expect(mirrorCard(page)).toBeHidden();
  await expect(page.locator('#agents-count')).toHaveText(/^1 of \d+$/);
});

// LIVE WORK COUNTS ON A DISPATCHED AGENT'S CARD (docs/design/agent-cards.md
// (a)): what a RUNNING agent's pod reports draws the mirrored card's own chip
// on the agent's card, LIVE while the list is open (no reload); a report of
// nothing takes it off; a stranger's token is refused; and stopping the agent
// takes it off too, though its pod never posts a clear. (A report that simply
// stops expires after its TTL: the Go tests pin that, at agentWorkTTL, which is
// longer than an e2e run should wait.) The agent is started first — the noop
// provisioner then reports it live — because only a running agent wears a chip.
test('a dispatched agent card shows the work its pod reports, live, and loses it', async ({ page }) => {
  test.setTimeout(90_000);
  await signIn(page);
  expect((await page.request.post(`${origin}/agents/${agentID}/start`, { headers: { Origin: origin } })).status()).toBe(200);
  await page.goto(origin + '/agents');
  const agentCard = page.locator('#agents-list > article').filter({ hasText: agentName });
  const chip = agentCard.locator('[data-mirror-work] > [aria-hidden="true"]');
  await expect(agentCard.locator('[aria-label="Status: running"]')).toHaveCount(1, { timeout: 20_000 });
  await expect(agentCard).toBeVisible({ timeout: 20_000 });
  await expect(chip, 'control: an agent that reported nothing has no chip').toHaveCount(0);
  let navigations = 0;
  page.on('framenavigated', (f) => {
    if (f === page.mainFrame()) navigations++;
  });
  expect(await reportWork({ subagents: 1 }, 'nobody-has-this-token-0123')).toBe(401);
  expect(await reportWork({ subagents: 3, shells: 2, shellsAtLeast: true })).toBe(204);
  await expect(chip).toHaveText('3 subagents · 2+ shells', { timeout: 15_000 });
  await expect(agentCard.locator('[data-mirror-work] .sr-only')).toHaveText('3 subagents and at least 2 background shells running');
  expect(await reportWork({ shells: 1 })).toBe(204);
  await expect(chip).toHaveText('1 shell', { timeout: 15_000 });
  expect(await reportWork({})).toBe(204);
  await expect(chip).toHaveCount(0, { timeout: 15_000 });
  expect(navigations, 'the chip came and went without a page load').toBe(0);
  // Stopped with a report up: the pod is gone and posts no clear, and the
  // card loses the chip anyway.
  expect(await reportWork({ subagents: 2 })).toBe(204);
  await expect(chip).toHaveText('2 subagents', { timeout: 15_000 });
  expect((await page.request.post(`${origin}/agents/${agentID}/stop`, { headers: { Origin: origin } })).status()).toBe(200);
  await page.reload();
  await expect(agentCard).toBeVisible({ timeout: 20_000 });
  await expect(chip, 'a stopped agent wears no chip').toHaveCount(0);
});
