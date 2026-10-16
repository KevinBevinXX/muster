import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { Browser, BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { binDir, dsn, password, repoRoot, sendQueuePort, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';

// THE SERVER-SIDE SEND QUEUE, in a real browser, against a real muster-server
// (MUSTER_MIRROR=1, MUSTER_SEND_QUEUE_IDLE=2s, its own port) and two real
// muster-mirror processes over the tunnel (the `-tags e2emirror` per-window
// fake): one whose Claude window reports Claude Code's status — busy while
// <transcript>.busy exists — and one that plays an older muster-mirror, which
// reports none (MUSTER_MIRROR_E2E_NO_STATUS=1).
//
// The operator decision: a queue of messages that send when the agent goes
// idle, ONE AT A TIME — the next only after the agent has been busy and idle
// again — that survives the page closing and is visible and cancellable from
// any device. Each of those is driven here: the page that queued is CLOSED
// before anything is sent, and the queue is then read (and a message
// cancelled) from a second, separate browser context.
//
// ⚠ TIMING: the fake host re-reads its windows every 5 s (mirrorhost's
// default Refresh), so a busy flip reaches muster within ~5 s; the server sends
// after 2 s idle and looks every 2 s. Every wait below is a poll with a bound,
// never a sleep standing in for an event — except the one that asserts a
// message is NOT sent, which must wait long enough for a wrong send to happen.

const origin = `http://localhost:${sendQueuePort}`;
const stamp = Date.now().toString(36);
const newHost = 'e2e-sq-' + stamp;
const oldHost = 'e2e-sqold-' + stamp;
const mirrorBin = path.join(binDir, 'muster-mirror-e2e');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'send-queue-'));
const tr = path.join(dir, 'cccccccc-0001.jsonl');
const trOld = path.join(dir, 'dddddddd-0002.jsonl');
const pane = '%e2e-work-@1';
let server: Running | undefined;
const procs: ChildProcess[] = [];
let mirrorLog = '';

const prompt = (text: string) => JSON.stringify({ type: 'user', promptId: 'p-' + text.length, message: { role: 'user', content: text } }) + '\n';
const read = (f: string) => (fs.existsSync(f) ? fs.readFileSync(f, 'utf8') : '');
const busy = (on: boolean) => (on ? fs.writeFileSync(tr + '.busy', '') : fs.rmSync(tr + '.busy', { force: true }));
const viewPath = (host: string) => `/mirror/view?host=${encodeURIComponent(host)}&session=work&window=%401`;
// busyAndSeen makes Claude busy and waits out one host refresh, so the server
// has seen it before anything is queued. Nothing observable says "muster saw
// busy" until a message is queued (the queue's own line does), so this is the
// one wait that is a duration: the fake's Refresh (5 s) and a margin.
async function busyAndSeen() {
  busy(true);
  await new Promise((r) => setTimeout(r, 6_500));
}
// What the host was pasted, in order (the fake appends `<pane> <text>`).
const pasted = () => read(tr + '.pasted').split('\n').filter((l) => l !== '');

function startMirror(host: string, transcript: string, extra: Record<string, string>) {
  const token = execFileSync(serverBinary(versionTerm), ['mirror', 'token', 'create', host], { env: { ...process.env, DATABASE_URL: dsn } })
    .toString()
    .trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
  const windows = path.join(dir, host + '.windows');
  fs.writeFileSync(windows, `work|@1|0|1|claude|${Math.floor(Date.now() / 1000) - 60}|${transcript}\n`);
  const p = spawn(mirrorBin, [], {
    env: {
      ...process.env,
      MUSTER_MIRROR_URL: origin,
      MUSTER_MIRROR_ALLOW_HTTP: '1',
      MUSTER_MIRROR_TOKEN: token,
      MUSTER_MIRROR_HOST: host,
      MUSTER_MIRROR_INTERACTIVE: '1',
      MUSTER_MIRROR_E2E_WINDOWS_FILE: windows,
      ...extra,
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  p.stdout?.on('data', (d) => (mirrorLog += d));
  p.stderr?.on('data', (d) => (mirrorLog += d));
  procs.push(p);
}

test.beforeAll(async () => {
  execFileSync('go', ['build', '-tags', 'e2emirror', '-o', mirrorBin, './cmd/muster-mirror'], { cwd: repoRoot, stdio: 'inherit' });
  server = await startServer(versionTerm, sendQueuePort, {
    MUSTER_MIRROR: '1',
    MUSTER_SEND_QUEUE_IDLE: '2s',
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
  });
  fs.writeFileSync(tr, prompt('the conversation so far'));
  fs.writeFileSync(trOld, prompt('an older host'));
  busy(true);
  startMirror(newHost, tr, {});
  startMirror(oldHost, trOld, { MUSTER_MIRROR_E2E_NO_STATUS: '1' });
});

test.afterAll(async () => {
  for (const p of procs) {
    if (p.exitCode === null) {
      const done = new Promise((r) => p.once('exit', r));
      p.kill('SIGTERM');
      await done;
    }
  }
  await server?.stop();
  fs.rmSync(dir, { recursive: true, force: true });
});

// One password sign-in, its cookie handed to every context (sign-in is
// throttled); a password sign-in is also a FRESH one, which the step-up gate
// on queueing for a mirror needs.
let signedIn: Awaited<ReturnType<BrowserContext['cookies']>> | undefined;
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

const phone = { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true };

// A separate browser context: another device, with its own page and no state
// shared with the first but the account.
async function otherDevice(browser: Browser): Promise<{ ctx: BrowserContext; page: Page }> {
  const ctx = await browser.newContext({ ...phone, storageState: { cookies: [], origins: [] } });
  const page = await ctx.newPage();
  await login(page);
  return { ctx, page };
}

async function openView(page: Page, host: string) {
  await login(page);
  await page.goto(origin + viewPath(host));
  await expect(page.locator('#mv-input'), `the rendered view (log:\n${mirrorLog})`).toBeEnabled({ timeout: 20_000 });
}

async function queue(page: Page, text: string, touch: boolean) {
  await page.locator('#mv-input').fill(text);
  const btn = page.locator('#mv-queue-send');
  if (touch) await btn.tap();
  else await btn.click();
  await expect(page.locator('#mv-input'), 'the box clears once queued').toHaveValue('');
  await expect(page.locator('#mv-note')).toHaveText('Queued.');
}

const rows = (page: Page) => page.locator('#mv-queue [data-queue-id]');
const row = (page: Page, text: string) => page.locator('#mv-queue [data-queue-id]', { hasText: text });

test.describe('phone 390×844 touch', () => {
  test.use({ ...phone, storageState: { cookies: [], origins: [] } });

  // 🔴 THE WHOLE CYCLE, AND THE REGRESSION TEST: on origin/main there is no
  // Queue button (and no /send-queue route), so this fails at the first tap.
  test('queued messages send one at a time when Claude goes idle, after the page that queued them closed, and are seen from another device', async ({ page, browser }) => {
    busy(true);
    await openView(page, newHost);
    await queue(page, 'first queued', true);
    await queue(page, 'second queued', true);
    await expect(rows(page)).toHaveCount(2);
    await expect(row(page, 'first queued')).toHaveAttribute('data-queue-state', 'queued');
    await expect(page.locator('#mv-queue-waiting'), `busy is seen (log:\n${mirrorLog})`).toContainText('working', { timeout: 20_000 });
    const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
    expect(sw, 'no horizontal scroll with the queue drawn').toBeLessThanOrEqual(cw);
    expect(pasted(), 'nothing is sent while Claude is busy').toEqual([]);

    // The page that queued them goes away. The queue is the server's.
    await page.close();
    busy(false);
    await expect.poll(pasted, { message: `the first message is sent once idle (log:\n${mirrorLog})`, timeout: 30_000 }).toEqual([`${pane} first queued`]);

    // Still idle — from BEFORE the first was sent: its turn has not been seen,
    // so the second waits. Long enough for a wrong send to have happened
    // (refresh 5 s + idle 2 s + a tick, twice over).
    await new Promise((r) => setTimeout(r, 16_000));
    expect(pasted(), 'one at a time: the second waits for the turn the first started').toEqual([`${pane} first queued`]);

    // Another device sees the queue: the first sent, the second waiting.
    const other = await otherDevice(browser);
    await openView(other.page, newHost);
    await expect(row(other.page, 'first queued')).toHaveAttribute('data-queue-state', 'sent');
    await expect(row(other.page, 'second queued')).toHaveAttribute('data-queue-state', 'queued');
    await expect(other.page.locator('#mv-queue-waiting')).toContainText("previous message's turn");

    // The first message's turn runs, then ends: the second is sent — and the
    // other device's list moves with it, live.
    busy(true);
    await expect(other.page.locator('#mv-queue-waiting'), 'the turn is seen').toContainText('working', { timeout: 20_000 });
    busy(false);
    await expect.poll(pasted, { message: `the second message is sent after the turn (log:\n${mirrorLog})`, timeout: 30_000 }).toEqual([
      `${pane} first queued`,
      `${pane} second queued`,
    ]);
    await expect(row(other.page, 'second queued'), 'the list updates live').toHaveAttribute('data-queue-state', 'sent', { timeout: 20_000 });
    await other.ctx.close();
  });

  test('a message queued on one device is cancelled from another, and never sent', async ({ page, browser }) => {
    await busyAndSeen();
    fs.rmSync(tr + '.pasted', { force: true });
    await openView(page, newHost);
    await queue(page, 'cancel me please', true);

    const other = await otherDevice(browser);
    await openView(other.page, newHost);
    const r = row(other.page, 'cancel me please');
    await expect(r).toHaveAttribute('data-queue-state', 'queued');
    await r.getByRole('button', { name: 'Cancel this queued message' }).tap();
    await expect(r).toHaveAttribute('data-queue-state', 'cancelled');
    // The first device's list follows (it polls while anything is open).
    await expect(row(page, 'cancel me please'), 'the first device sees the cancel').toHaveAttribute('data-queue-state', 'cancelled', { timeout: 20_000 });

    busy(false);
    await new Promise((r) => setTimeout(r, 12_000));
    expect(pasted(), 'a cancelled message is never sent').toEqual([]);
    await other.ctx.close();
  });

  // 🔴 FAIL CLOSED: a host whose muster-mirror does not report Claude Code's
  // status cannot be queued for — Queue is drawn disabled, saying why, and the
  // route refuses a queue post saying the same — so nothing is ever sent to it
  // on a guess.
  test('an older muster-mirror: Queue is off and says why, and the server refuses it', async ({ page }) => {
    await openView(page, oldHost);
    await expect(page.locator('#mv-queue-send')).toBeDisabled();
    await expect(page.locator('#mv-queue-off')).toContainText('does not report whether Claude Code is busy');
    const res = await page.evaluate(async (host) => {
      const r = await fetch('/send-queue', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'X-Requested-With': 'XMLHttpRequest' },
        body: new URLSearchParams({ queue: 'view', host, session: 'work', window: '@1', text: 'x' }),
      });
      return { status: r.status, text: await r.text() };
    }, oldHost);
    expect(res.status).toBe(409);
    expect(res.text).toContain('does not report whether Claude Code is busy');
    // Send still works there: the queue being off is not the composer being
    // off — and a send does not switch Queue back on.
    await page.locator('#mv-input').fill('sent to the older host');
    await page.locator('#mv-send').tap();
    await expect(page.locator('#mv-input')).toHaveValue('');
    await expect.poll(() => read(trOld + '.pasted')).toBe(`${pane} sent to the older host\n`);
    await expect(page.locator('#mv-queue-send'), 'Queue stays off after a send').toBeDisabled();
  });
});

test.describe('desktop 1280×800', () => {
  test.use({ viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false, storageState: { cookies: [], origins: [] } });

  test('Queue is a button beside Send that does not send now; Send still sends now', async ({ page }) => {
    await busyAndSeen();
    fs.rmSync(tr + '.pasted', { force: true });
    await openView(page, newHost);
    await queue(page, 'desktop queued', false);
    await expect(row(page, 'desktop queued')).toHaveAttribute('data-queue-state', 'queued');
    await page.locator('#mv-input').fill('desktop sent now');
    await page.locator('#mv-send').click();
    await expect(page.locator('#mv-input')).toHaveValue('');
    await expect.poll(pasted, { message: 'Send pastes at once, busy or not' }).toEqual([`${pane} desktop sent now`]);
    // Tidy: the queued one is cancelled, so no later run sees it sent.
    await row(page, 'desktop queued').getByRole('button', { name: 'Cancel this queued message' }).click();
    await expect(row(page, 'desktop queued')).toHaveAttribute('data-queue-state', 'cancelled');
    busy(false);
  });

  // 🔴 WHERE THE BROWSER HAS NO SubmitEvent.submitter (older Safari), the
  // composer's script must still know Queue was pressed: guessing Send would
  // deliver the message NOW, the one thing the person asked not to happen.
  test('with no SubmitEvent.submitter, a Queue click still queues rather than sending', async ({ page }) => {
    await page.addInitScript(() => {
      delete (SubmitEvent.prototype as unknown as { submitter?: unknown }).submitter;
    });
    await busyAndSeen();
    fs.rmSync(tr + '.pasted', { force: true });
    await openView(page, newHost);
    expect(await page.evaluate(() => 'submitter' in SubmitEvent.prototype), 'instrument: the submitter is really gone').toBe(false);
    await queue(page, 'queued without a submitter', false);
    await expect(row(page, 'queued without a submitter')).toHaveAttribute('data-queue-state', 'queued');
    expect(pasted(), 'nothing was sent now').toEqual([]);
    await row(page, 'queued without a submitter').getByRole('button', { name: 'Cancel this queued message' }).click();
    await expect(row(page, 'queued without a submitter')).toHaveAttribute('data-queue-state', 'cancelled');
    busy(false);
  });
});
