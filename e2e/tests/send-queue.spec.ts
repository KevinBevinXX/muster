import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { Browser, BrowserContext, Locator, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { binDir, dsn, password, repoRoot, sendQueuePort, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { writeFileAtomic } from '../lib/atomic';

// THE SERVER-SIDE SEND QUEUE, in a real browser, against a real muster-server
// (MUSTER_MIRROR=1, MUSTER_SEND_QUEUE_IDLE=1s, its own port) and two real
// muster-mirror processes over the tunnel (the `-tags e2emirror` per-window
// fake): one whose Claude window reports Claude Code's status and the time it
// was written — from <transcript>.status, which this spec writes the way the
// CLI writes its sessions file ({status, statusUpdatedAt}) — and one that plays
// an older muster-mirror, which reports none (MUSTER_MIRROR_E2E_NO_STATUS=1).
//
// The operator decision: a queue of messages, on a mirrored window's Rendered
// view, that send when Claude Code goes idle, ONE AT A TIME — the next only once
// Claude Code reports idle since AFTER the previous one was sent — that
// survives the page closing and is visible and cancellable from any device. Each of those is driven here: the page that queued is CLOSED
// before anything is sent, and the queue is then read (and a message
// cancelled) from a second, separate browser context.
//
// ⚠ TIMING: the fake host re-reads its windows every 250 ms
// (MUSTER_MIRROR_E2E_REFRESH, an e2emirror-only knob; a shipped muster-mirror
// re-reads every 5 s), so a status flip reaches muster within a fraction of a
// second; the server sends after 1 s idle (MUSTER_SEND_QUEUE_IDLE, its minimum)
// and looks every 2 s (sendqueue.DefaultInterval). Every wait below is a poll
// on what the page or the host shows, with a bound — except two kinds of
// duration, each named and sized from those numbers: seen() (nothing
// observable says the server has a status the spec just wrote until something
// is queued) and holdWindow() (a message asserted NOT sent: once the page
// shows the server holding it, the wait is as long as a wrong send would take
// to happen — the idle time plus two dispatcher passes — and no longer).

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

const refreshMs = 250;
const idleMs = 1_000;
const dispatchEveryMs = 2_000; // sendqueue.DefaultInterval
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
// seen waits out a few host refreshes, so the server holds the status the spec
// just wrote.
const seen = () => sleep(4 * refreshMs + 500);
// holdWindow is how long a WRONG send would take to land once the server holds
// the state that should hold it: a host refresh, the idle time, and two
// dispatcher passes, plus a margin. Call it only AFTER the page showed the
// server's reason for holding (the same Decide the dispatcher runs), so the
// window starts from a state the server is known to have.
const holdWindow = () => sleep(refreshMs + idleMs + 2 * dispatchEveryMs + 500);

const prompt = (text: string) => JSON.stringify({ type: 'user', promptId: 'p-' + text.length, message: { role: 'user', content: text } }) + '\n';
const read = (f: string) => (fs.existsSync(f) ? fs.readFileSync(f, 'utf8') : '');
// setStatus is Claude Code writing its status: the status and NOW as its time.
const setStatus = (status: string) => writeFileAtomic(tr + '.status', JSON.stringify({ status, statusUpdatedAt: Date.now() }));
const busy = (on: boolean) => setStatus(on ? 'busy' : 'idle');
const viewPath = (host: string) => `/mirror/view?host=${encodeURIComponent(host)}&session=work&window=%401`;
// busyAndSeen makes Claude busy and waits until the server has seen it, before
// anything is queued. Nothing observable says "muster saw busy" until a message
// is queued (the queue's own line does), so this is a duration: seen().
async function busyAndSeen() {
  busy(true);
  await seen();
}
// What the host was pasted, in order (the fake appends `<pane> <text>`).
const pasted = () => read(tr + '.pasted').split('\n').filter((l) => l !== '');

function startMirror(host: string, transcript: string, extra: Record<string, string>) {
  const token = execFileSync(serverBinary(versionTerm), ['mirror', 'token', 'create', host], { env: { ...process.env, DATABASE_URL: dsn } })
    .toString()
    .trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
  const windows = path.join(dir, host + '.windows');
  writeFileAtomic(windows, `work|@1|0|1|claude|${Math.floor(Date.now() / 1000) - 60}|${transcript}\n`);
  const p = spawn(mirrorBin, [], {
    env: {
      ...process.env,
      MUSTER_MIRROR_URL: origin,
      MUSTER_MIRROR_ALLOW_HTTP: '1',
      MUSTER_MIRROR_TOKEN: token,
      MUSTER_MIRROR_HOST: host,
      MUSTER_MIRROR_INTERACTIVE: '1',
      MUSTER_MIRROR_E2E_WINDOWS_FILE: windows,
      MUSTER_MIRROR_E2E_REFRESH: `${refreshMs}ms`,
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
    MUSTER_SEND_QUEUE_IDLE: `${idleMs / 1000}s`,
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

// queue queues text through Send's caret: the caret opens its menu, and the
// menu's one item is Queue (composer_menu.go; Send's face never queues).
async function queue(page: Page, text: string, touch: boolean) {
  await page.locator('#mv-input').fill(text);
  const more = page.locator('#mv-send-more');
  const btn = page.locator('#mv-queue-send');
  if (touch) await more.tap();
  else await more.click();
  await expect(btn).toBeVisible();
  if (touch) await btn.tap();
  else await btn.click();
  await expect(page.locator('#mv-input'), 'the box clears once queued').toHaveValue('');
  await expect(page.locator('#mv-note')).toHaveText('Queued.');
}

// sqfix is the spec's database fixture (e2e/sqfix): ages a queued message, or
// strands it mid-send, so that the server's OWN dispatcher fails it.
const sqfix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/sqfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } });

// A queued row's ⋯ menu and its items (ui.sendQueueRowMenu): Send now, Copy
// message and Cancel live behind it.
const moreOf = (r: Locator) => r.getByRole('button', { name: 'More actions for this queued message' });
const itemOf = (r: Locator, name: string) => r.getByRole('menuitem', { name });
async function openMenu(r: Locator, touch = false) {
  const more = moreOf(r);
  if (touch) await more.tap();
  else await more.click();
  await expect(more).toHaveAttribute('aria-expanded', 'true');
}
// cancelRow is Cancel, through the row's menu.
async function cancelRow(r: Locator, touch = false) {
  await openMenu(r, touch);
  const c = itemOf(r, 'Cancel this queued message');
  if (touch) await c.tap();
  else await c.click();
}

const rows = (page: Page) => page.locator('#mv-queue [data-queue-id]');
const row = (page: Page, text: string) => page.locator('#mv-queue [data-queue-id]', { hasText: text });
const rowIn = (page: Page, text: string, state: string) => page.locator(`#mv-queue [data-queue-state="${state}"]`, { hasText: text });

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

    // Another device sees the queue: the first sent, the second waiting.
    const other = await otherDevice(browser);
    await openView(other.page, newHost);
    await expect(row(other.page, 'first queued')).toHaveAttribute('data-queue-state', 'sent');
    await expect(row(other.page, 'second queued')).toHaveAttribute('data-queue-state', 'queued');
    // Still idle — since BEFORE the first was sent: its turn has not ended,
    // so the second waits. The server says so, then holds it for as long as a
    // wrong send would take.
    await expect(other.page.locator('#mv-queue-waiting')).toContainText("previous message's turn");
    await holdWindow();
    expect(pasted(), 'one at a time: the second waits for the turn the first started').toEqual([`${pane} first queued`]);

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
    await cancelRow(r, true);
    await expect(r).toHaveAttribute('data-queue-state', 'cancelled');
    // The first device's list follows (it polls while anything is open).
    await expect(row(page, 'cancel me please'), 'the first device sees the cancel').toHaveAttribute('data-queue-state', 'cancelled', { timeout: 20_000 });

    // Idle: had the cancel not held, the cancelled message would be the head and
    // go first. A message queued after it is the observable — it is sent, and
    // it is the only thing sent.
    busy(false);
    await queue(page, 'queued after the cancel', true);
    await expect.poll(pasted, { message: `a cancelled message is never sent (log:\n${mirrorLog})`, timeout: 30_000 }).toEqual([
      `${pane} queued after the cancel`,
    ]);
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
        body: new URLSearchParams({ host, session: 'work', window: '@1', text: 'x' }),
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

  test('Queue is behind Send’s caret and does not send now; Send still sends now', async ({ page }) => {
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
    await cancelRow(row(page, 'desktop queued'));
    await expect(row(page, 'desktop queued')).toHaveAttribute('data-queue-state', 'cancelled');
    busy(false);
  });

  // 🔴 D3, THE CLI'S OWN TIME: a turn that starts and ends between two host
  // refreshes is never SEEN busy — the status reads idle both times — and the
  // time Claude Code wrote it is newer. That alone releases the next message;
  // without it the next is held.
  test('a turn too short for the host to see busy still releases the next message, and only after it', async ({ page }) => {
    await busyAndSeen();
    fs.rmSync(tr + '.pasted', { force: true });
    await openView(page, newHost);
    await queue(page, 'short one', false);
    await queue(page, 'short two', false);
    busy(false);
    await expect.poll(pasted, { message: `the first is sent once idle (log:\n${mirrorLog})`, timeout: 30_000 }).toEqual([`${pane} short one`]);
    await expect(page.locator('#mv-queue-waiting'), 'the server holds the second').toContainText("previous message's turn", { timeout: 20_000 });
    await holdWindow();
    expect(pasted(), 'the second waits: idle since before the first was sent').toEqual([`${pane} short one`]);
    // The first message's turn, too short to be seen: idle again, a newer time.
    setStatus('idle');
    await expect.poll(pasted, { message: 'the second is sent after the unseen turn', timeout: 30_000 }).toEqual([
      `${pane} short one`,
      `${pane} short two`,
    ]);
  });

  // 🔴 Y1, EVERY PASTE HOLDS THE QUEUE: a person's own Send starts a turn too,
  // so a message queued right after it waits for a status NEWER than the one
  // the Send was made in, not just for idle.
  test('a message queued after a Send waits for the turn that Send started', async ({ page }) => {
    setStatus('idle');
    await seen(); // the server has the idle
    fs.rmSync(tr + '.pasted', { force: true });
    await openView(page, newHost);
    await page.locator('#mv-input').fill('sent by hand');
    await page.locator('#mv-send').click();
    await expect.poll(pasted, { message: 'Send pastes at once' }).toEqual([`${pane} sent by hand`]);
    await queue(page, 'queued after the hand send', false);
    // Idle all along, on the status the Send was made in: its turn has not
    // ended. The server says so, then holds it for as long as a wrong send
    // would take.
    await expect(page.locator('#mv-queue-waiting')).toContainText("previous message's turn");
    await holdWindow();
    expect(pasted(), "the queued message waits for the Send's turn").toEqual([`${pane} sent by hand`]);
    // The Send's turn ends: a newer status time releases it.
    setStatus('idle');
    await expect.poll(pasted, { message: "sent after the Send's turn", timeout: 30_000 }).toEqual([
      `${pane} sent by hand`,
      `${pane} queued after the hand send`,
    ]);
  });

  // 🔴 FAIL CLOSED ON A STATUS WITHOUT ITS TIME: a host that says idle but not
  // since when is held, saying why — never released by a time guess. The
  // positive control is the same message, sent once the time is reported.
  test('a status without its time holds the queue and says why', async ({ page }) => {
    await busyAndSeen();
    fs.rmSync(tr + '.pasted', { force: true });
    await openView(page, newHost);
    await queue(page, 'untimed', false);
    // No .status file and no marker: the fake reports idle with no time.
    fs.rmSync(tr + '.status', { force: true });
    await expect(page.locator('#mv-queue-waiting'), `the reason is shown (log:\n${mirrorLog})`).toContainText('does not report since when', {
      timeout: 20_000,
    });
    await holdWindow();
    expect(pasted(), 'nothing is sent to an idle with no time').toEqual([]);
    setStatus('idle');
    await expect.poll(pasted, { message: 'CONTROL: sent once the time is reported', timeout: 30_000 }).toEqual([`${pane} untimed`]);
  });

  // D1: only the Rendered view queues. The Terminal view's composer pastes into
  // whatever pane is active, which is not the pane whose idle a queue waits on.
  test('the Terminal view of the same window has Send and no Queue', async ({ page }) => {
    await login(page);
    await page.goto(origin + `/mirror/terminal?host=${encodeURIComponent(newHost)}&session=work&window=%401`);
    await expect(page.locator('#mv-input'), 'the Terminal view draws its composer').toBeVisible({ timeout: 20_000 });
    await expect(page.locator('#mv-queue-send')).toHaveCount(0);
    await expect(page.locator('#mv-queue')).toHaveCount(0);
    // CONTROL: the Rendered view of the same window does, behind Send's caret.
    await page.goto(origin + viewPath(newHost));
    await page.locator('#mv-send-more').click({ timeout: 20_000 });
    await expect(page.locator('#mv-queue-send')).toBeVisible();
  });

  // 🔴 A MESSAGE STAYS AUTHORIZED FOR 7 DAYS FROM WHEN IT WAS QUEUED, then fails
  // as expired with a Retry; Retry queues the same text again under this
  // session's own (fresh) sign-in, and the dispatcher sends it. On origin/main
  // there is no Retry, so this fails at the Retry button.
  test('a message that expired shows Retry, and Retry queues it again and it is sent', async ({ page }) => {
    await busyAndSeen();
    fs.rmSync(tr + '.pasted', { force: true });
    await openView(page, newHost);
    const text = 'expired then retried ' + stamp;
    await queue(page, text, false);
    await expect(rowIn(page, text, 'queued')).toHaveCount(1);
    sqfix('age', newHost, text, String(7 * 24 * 3600 + 60));
    busy(false);
    const failed = rowIn(page, text, 'failed');
    await expect(failed, `the dispatcher fails it as expired (log:\n${mirrorLog})`).toContainText('queued more than 7 days ago', { timeout: 30_000 });
    expect(pasted(), 'an expired message is not pasted').toEqual([]);
    // Held busy again, so the Retry's row is seen queued before it is sent.
    await busyAndSeen();
    await failed.getByRole('button', { name: 'Queue this failed message again' }).click();
    await expect(rowIn(page, text, 'queued'), 'Retry queues it again').toHaveCount(1, { timeout: 20_000 });
    await expect(failed.getByRole('button', { name: /Queue this/ }), 'a retried message offers no second Retry').toHaveCount(0);
    busy(false);
    await expect.poll(pasted, { message: `the retried message is sent (log:\n${mirrorLog})`, timeout: 30_000 }).toEqual([`${pane} ${text}`]);
    await expect(rowIn(page, text, 'sent')).toHaveCount(1, { timeout: 20_000 });
  });

  // 🔴 A MESSAGE PAST ITS 7 DAYS EXPIRES EVEN WHILE ITS WINDOW NEVER GOES IDLE:
  // the dispatcher's own pass fails it as expired, so the list stops saying
  // "queued" and its slot frees; it offers Retry. Held busy throughout, so
  // the delivery (which also refuses it) never gets to run. On origin/main
  // the row stays queued, and this fails at the "failed" row.
  test('a message past 7 days expires while its window stays busy, and offers Retry', async ({ page }) => {
    await busyAndSeen();
    fs.rmSync(tr + '.pasted', { force: true });
    await openView(page, newHost);
    const text = 'expired while busy ' + stamp;
    await queue(page, text, false);
    await expect(rowIn(page, text, 'queued')).toHaveCount(1);
    sqfix('age', newHost, text, String(7 * 24 * 3600 + 60));
    const failed = rowIn(page, text, 'failed');
    await expect(failed, `the dispatcher expires it while busy (log:\n${mirrorLog})`).toContainText('queued more than 7 days ago', { timeout: 30_000 });
    await expect(rowIn(page, text, 'queued'), 'no longer shown queued').toHaveCount(0);
    await expect(failed.getByRole('button', { name: 'Queue this failed message again' }), 'it offers Retry').toHaveCount(1);
    expect(pasted(), 'an expired message is not pasted').toEqual([]);
  });

  // 🔴 A ROUTED MESSAGE THAT FAILED OFFERS NO RETRY: Retry would queue it again
  // as the person's own, with no route and no session binding, past the gate
  // its route is. A failed route goes back to the chief instead. The test
  // above is the control: a person's failed row draws the same button.
  test('a routed message that failed offers no Retry', async ({ page }) => {
    await busyAndSeen();
    fs.rmSync(tr + '.pasted', { force: true });
    await openView(page, newHost);
    const text = 'routed then failed ' + stamp;
    await queue(page, text, false);
    await expect(rowIn(page, text, 'queued')).toHaveCount(1);
    sqfix('route', newHost, text);
    busy(false);
    const failed = rowIn(page, text, 'failed');
    await expect(failed, `the route gate fails it (log:\n${mirrorLog})`).toContainText('its route could not be read', { timeout: 30_000 });
    expect(pasted(), 'a routed message whose route is gone is not pasted').toEqual([]);
    await expect(failed.getByRole('button', { name: /Queue this/ }), 'a failed routed row offers no Retry').toHaveCount(0);
    await busyAndSeen();
  });

  // 🔴 A FINISHED MESSAGE CAN BE DELETED — the operator's case: a failed
  // message the chief routed has no Retry, and used to sit in the list until it
  // was pruned. Its Delete removes the row without a page reload, and another
  // device's list no longer has it. A queued message has no Delete (that is
  // Cancel). On origin/main there is no Delete, so this fails at the button.
  test('a failed message has Delete, and Delete removes it without a reload, on every device', async ({ page, browser }) => {
    await busyAndSeen();
    fs.rmSync(tr + '.pasted', { force: true });
    await openView(page, newHost);
    const text = 'failed then deleted ' + stamp;
    await queue(page, text, false);
    const queuedRow = rowIn(page, text, 'queued');
    await expect(queuedRow).toHaveCount(1);
    await expect(queuedRow.getByRole('button', { name: 'Delete this message' }), 'a queued message has no Delete').toHaveCount(0);
    sqfix('route', newHost, text);
    busy(false);
    const failed = rowIn(page, text, 'failed');
    await expect(failed, `the route gate fails it (log:\n${mirrorLog})`).toContainText('its route could not be read', { timeout: 30_000 });
    await expect(failed.getByRole('button', { name: /Queue this/ }), 'no Retry: the operator case').toHaveCount(0);

    const other = await otherDevice(browser);
    await openView(other.page, newHost);
    await expect(rowIn(other.page, text, 'failed'), 'the other device shows it').toHaveCount(1);

    // A marker on the page's window: a reload (or a navigation) would drop it.
    await page.evaluate(() => ((window as unknown as { __sqNoReload?: number }).__sqNoReload = 1));
    const del = failed.getByRole('button', { name: 'Delete this message' });
    await expect(del).toBeVisible();
    await del.click();
    await expect(row(page, text), 'the row is gone').toHaveCount(0, { timeout: 10_000 });
    expect(await page.evaluate(() => (window as unknown as { __sqNoReload?: number }).__sqNoReload), 'no reload').toBe(1);
    await expect(page).toHaveURL(origin + viewPath(newHost));
    // Another device: its list follows (it re-reads every 15 s with nothing
    // open, at once when the page is shown again).
    await other.page.reload();
    await expect(row(other.page, text), 'gone on the other device too').toHaveCount(0, { timeout: 20_000 });
    expect(pasted(), 'nothing was pasted').toEqual([]);
    await other.ctx.close();
    await busyAndSeen();
  });

  // 🔴 A LIST READ IN FLIGHT WHEN A DELETE LANDS DOES NOT PUT THE ROW BACK
  // (#168). The page's periodic re-read can start before the Delete's post and
  // answer after the page removed the row: drawn, that answer redraws the
  // deleted row (until the next re-read, up to 15 s later). The re-read is held
  // here until the delete has answered, then released with the list it read —
  // the row still in it. On origin/main the row comes back.
  test('a list read in flight during a Delete does not redraw the deleted row', async ({ page }) => {
    await busyAndSeen();
    fs.rmSync(tr + '.pasted', { force: true });
    await openView(page, newHost);
    const text = 'deleted mid-read ' + stamp;
    await queue(page, text, false);
    await expect(rowIn(page, text, 'queued')).toHaveCount(1);
    sqfix('route', newHost, text);
    busy(false);
    const failed = rowIn(page, text, 'failed');
    await expect(failed, `the route gate fails it (log:\n${mirrorLog})`).toContainText('its route could not be read', { timeout: 30_000 });

    // Hold the NEXT list read: read the real list now (the row in it), answer
    // only once released.
    let release!: () => void;
    const released = new Promise<void>((r) => (release = r));
    let held = 0;
    let after = 0;
    await page.route(
      (u) => u.pathname === '/send-queue' && u.search.length > 0,
      async (route) => {
        if (route.request().method() !== 'GET') return route.fallback();
        if (held > 0) {
          after++;
          return route.fallback();
        }
        held++;
        const stale = await route.fetch();
        const body = await stale.text();
        expect(body, 'instrument: the held read has the row').toContain(text);
        await released;
        await route.fulfill({ response: stale, body });
      },
    );
    await page.evaluate(() => document.dispatchEvent(new Event('mv-queued')));
    await expect.poll(() => held, { message: 'instrument: a list read is held' }).toBe(1);

    const posted = page.waitForResponse((r) => r.request().method() === 'POST' && r.url().includes('/delete'));
    await failed.getByRole('button', { name: 'Delete this message' }).click();
    expect((await posted).status(), 'the delete succeeded').toBeLessThan(300);
    await expect(row(page, text), 'the row is removed at once').toHaveCount(0);
    release();
    // The held answer arrives now. Checked BEFORE any later re-read could
    // repair it (the next scheduled one is 3 s away at the soonest): the page
    // must not draw it.
    await page.waitForTimeout(700);
    await expect(row(page, text), 'the stale read did not redraw the deleted row').toHaveCount(0);
    // It read the list again instead.
    await expect.poll(() => after, { message: 'the stale read is followed by a fresh one', timeout: 10_000 }).toBeGreaterThan(0);
    await page.unrouteAll({ behavior: 'ignoreErrors' });
    await busyAndSeen();
  });

  // 🔴 A MESSAGE THAT MAY ALREADY HAVE BEEN SENT (muster stopped mid-send) says so,
  // and its Retry asks once: dismissed, nothing is queued; accepted, it is.
  test('Retry of a message that may already have been sent asks first', async ({ page }) => {
    await busyAndSeen();
    fs.rmSync(tr + '.pasted', { force: true });
    await openView(page, newHost);
    const text = 'stranded mid-send ' + stamp;
    await queue(page, text, false);
    await expect(rowIn(page, text, 'queued')).toHaveCount(1);
    sqfix('strand', newHost, text);
    const failed = rowIn(page, text, 'failed');
    await expect(failed, `recovered as may-have-been-sent (log:\n${mirrorLog})`).toContainText('May already have been sent.', { timeout: 30_000 });
    const btn = failed.getByRole('button', { name: 'Queue this message again (it may already have been sent)' });
    const asked: string[] = [];
    // Every Retry post the page makes: the observable for "dismissed sends
    // nothing", with the accepted Retry below as its positive control.
    const posts: string[] = [];
    page.on('request', (r) => {
      if (r.method() === 'POST' && r.url().includes('/retry')) posts.push(r.postData() ?? '');
    });
    page.once('dialog', (d) => {
      asked.push(d.message());
      void d.dismiss();
    });
    await btn.click();
    await expect.poll(() => asked.length, { message: 'Retry asks first' }).toBe(1);
    expect(asked[0]).toContain('May already have been sent.');
    await expect(btn, 'dismissed: the Retry is still there').toBeEnabled();
    expect(posts, 'a dismissed Retry posts nothing').toEqual([]);
    page.once('dialog', (d) => void d.accept());
    await btn.click();
    await expect(rowIn(page, text, 'queued'), 'an accepted Retry queues it again').toHaveCount(1, { timeout: 20_000 });
    expect(posts, 'CONTROL: the accepted Retry posted once, saying it was confirmed').toEqual(['confirm=1']);
    // Tidy: cancel the copy, so no later run sees it sent.
    await cancelRow(rowIn(page, text, 'queued'));
    await expect(rowIn(page, text, 'cancelled')).toHaveCount(1);
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
    await cancelRow(row(page, 'queued without a submitter'));
    await expect(row(page, 'queued without a submitter')).toHaveAttribute('data-queue-state', 'cancelled');
    busy(false);
  });
});

// 🔴 CLAUDE CODE'S "shell" IS IDLE (Claude Code 2.1.289 writes it instead of
// "idle" while a run_in_background Bash task is still alive and no turn is):
// the prompt is free, so a queued message is sent. On origin/main the server
// read "shell" as unknown and held the message for as long as the background
// shell ran (the window read as one whose status the host cannot read), so
// this fails at the paste.
test.describe('statuses', () => {
  test.use({ viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false, storageState: { cookies: [], origins: [] } });

  test('"shell" (idle with a background shell running) sends the queued message', async ({ page }) => {
    await busyAndSeen();
    fs.rmSync(tr + '.pasted', { force: true });
    await openView(page, newHost);
    const text = 'sent while a background shell runs ' + stamp;
    await queue(page, text, false);
    await expect(page.locator('#mv-queue-waiting'), `busy is seen (log:\n${mirrorLog})`).toContainText('working', { timeout: 20_000 });
    // The turn ends with a background shell still running: "shell", a newer time.
    setStatus('shell');
    await expect.poll(pasted, { message: `sent once Claude Code reports "shell" (log:\n${mirrorLog})`, timeout: 30_000 }).toEqual([`${pane} ${text}`]);
    await expect(rowIn(page, text, 'sent')).toHaveCount(1, { timeout: 20_000 });
    await busyAndSeen();
  });
});

// THE QUEUED ROW'S ⋯ MENU (ui.sendQueueRowMenu): Send now, Copy message,
// Cancel. On origin/main a queued row has a bare Cancel and no menu, so each
// of these fails at the ⋯ button.
test.describe('queued row menu, phone 390×844 touch', () => {
  test.use({ ...phone, storageState: { cookies: [], origins: [] } });

  // 🔴 SEND NOW SENDS WHILE BUSY, ONCE; THE OTHER WAITS FOR IDLE AS BEFORE.
  test('Send now pastes a queued message at once while Claude Code is busy, and the other still waits', async ({ page }) => {
    await busyAndSeen();
    fs.rmSync(tr + '.pasted', { force: true });
    await openView(page, newHost);
    const first = 'send-now first ' + stamp;
    const second = 'send-now second ' + stamp;
    await queue(page, first, true);
    await queue(page, second, true);
    const r = rowIn(page, second, 'queued');
    await openMenu(r, true);
    await itemOf(r, 'Send now').tap();
    await expect.poll(pasted, { message: `Send now pastes at once, busy or not (log:\n${mirrorLog})`, timeout: 15_000 }).toEqual([
      `${pane} ${second}`,
    ]);
    await expect(page.locator('#mv-note')).toHaveText('Sent.');
    await expect(rowIn(page, second, 'sent'), 'the row shows it sent').toHaveCount(1, { timeout: 20_000 });
    await expect(rowIn(page, first, 'queued'), 'the other stays queued').toHaveCount(1);
    await holdWindow();
    expect(pasted(), 'the other is not sent while busy').toEqual([`${pane} ${second}`]);
    // (That the next waits for the turn a Send now started, whatever the
    // timing, is pinned in Go: TestSendNowSendsAQueuedMessageAtOnceAndTheNextStillWaitsForItsTurn.)
    busy(false);
    await expect.poll(pasted, { message: 'the other is sent once idle', timeout: 30_000 }).toEqual([`${pane} ${second}`, `${pane} ${first}`]);
    await busyAndSeen();
  });

  test('Copy message copies the whole message, not the preview, and says so', async ({ page, context }) => {
    await context.grantPermissions(['clipboard-read', 'clipboard-write'], { origin });
    await busyAndSeen();
    await openView(page, newHost);
    // Longer than the 200-character preview, so a copy of the preview fails.
    const text = 'copy me ' + stamp + ' ' + 'lorem ipsum dolor sit amet '.repeat(12) + 'THE END';
    await queue(page, text, true);
    const r = rowIn(page, 'copy me ' + stamp, 'queued');
    await openMenu(r, true);
    await itemOf(r, 'Copy message').tap();
    await expect(page.locator('#mv-note')).toHaveText('Copied.');
    expect(await page.evaluate(() => navigator.clipboard.readText()), 'the clipboard holds the whole message').toBe(text);
    await expect(moreOf(r), 'the menu closed').toHaveAttribute('aria-expanded', 'false');
    await cancelRow(r, true);
    await expect(rowIn(page, 'copy me ' + stamp, 'cancelled')).toHaveCount(1);
    expect(pasted().some((l) => l.includes('copy me ' + stamp)), 'a copy sends nothing').toBe(false);
  });

  test('the menu fits the phone, moves nothing, and closes on a tap outside', async ({ page }) => {
    await busyAndSeen();
    await openView(page, newHost);
    const text = 'menu layout ' + stamp;
    await queue(page, text, true);
    const r = rowIn(page, text, 'queued');
    // 🔴 THE ROW IS DRAWN AND PAINTED BEFORE THE SHIFTS ARE COUNTED. queue()
    // returns on the "Queued." note, while the list's own re-fetch may still be
    // drawing the new row; the row grows the composer, which moves it (and the
    // "working" line above it) up. That shift is reported in the first frame
    // AFTER the row is inserted — so an observer armed in between counted
    // QUEUING as the menu's shift (CI, run 39502271983: 0.0314 on mv-compose;
    // reproduced with the row inserted at 237.7 ms, the observer armed at
    // 249.2 ms, the shift reported at 254.9 ms). The row visible, two frames
    // later that frame has been rendered and its shift is behind us.
    await expect(r).toBeVisible();
    await page.evaluate(() => new Promise<void>((res) => requestAnimationFrame(() => requestAnimationFrame(() => res()))));
    // Every layout shift whose moved nodes are the composer's (the queue is in
    // it) or the menu's —
    // input or not: CLS leaves out shifts right after input, and a menu opened
    // by a tap is exactly that. (Other parts of the view move on their own —
    // the "Claude is working…" line — and are not this menu's.)
    await page.evaluate(() => {
      const w = window as unknown as { __shift: number; __shifts: string[] };
      w.__shift = 0;
      w.__shifts = [];
      new PerformanceObserver((l) => {
        for (const e of l.getEntries() as unknown as { value: number; sources: { node?: Node | null }[] }[]) {
          const nodes = (e.sources || []).map((x) => x.node).filter((n): n is Node => !!n);
          const ours = nodes.some((n) => {
            const el = n instanceof Element ? n : n.parentElement;
            return !!el && !!el.closest('#mv-compose, [data-queue-menu]');
          });
          if (ours) w.__shift += e.value;
          w.__shifts.push(`${e.value.toFixed(4)} ours=${ours} ` + nodes.map((n) => (n instanceof Element ? n.id || n.tagName : n.nodeName)).join(','));
        }
      }).observe({ type: 'layout-shift', buffered: false });
    });
    const before = await r.boundingBox();
    await openMenu(r, true);
    const menu = r.getByRole('menu');
    await expect(menu).toBeVisible();
    const box = await menu.boundingBox();
    const vw = page.viewportSize()!.width;
    expect(box && box.x >= 0 && box.x + box.width <= vw, `the menu is on screen: ${JSON.stringify(box)}`).toBe(true);
    for (const name of ['Send now', 'Edit', 'Copy message', 'Cancel this queued message']) {
      const b = await itemOf(r, name).boundingBox();
      expect(b && b.height >= 44, `${name} is a 44px target: ${JSON.stringify(b)}`).toBe(true);
    }
    const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
    expect(sw, 'no horizontal scroll').toBeLessThanOrEqual(cw);
    // A tap outside the menu (the conversation) closes it.
    await page.locator('#mv-log').tap({ position: { x: 20, y: 20 } });
    await expect(menu).toBeHidden();
    await expect(moreOf(r)).toHaveAttribute('aria-expanded', 'false');
    expect(await r.boundingBox(), 'the row did not move').toEqual(before);
    await page.waitForTimeout(300);
    expect(await page.evaluate(() => (window as unknown as { __shift: number }).__shift), 'no layout shift opening or closing it: ' +
      JSON.stringify(await page.evaluate(() => (window as unknown as { __shifts: string[] }).__shifts))).toBeLessThan(0.001);
    await cancelRow(r, true);
    await expect(rowIn(page, text, 'cancelled')).toHaveCount(1);
  });
});

test.describe('queued row menu, desktop 1280×800 keyboard', () => {
  test.use({ viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false, storageState: { cookies: [], origins: [] } });

  test('the menu is a keyboard menu button: Enter opens it on its first item, arrows move, Escape closes it back to the button', async ({ page }) => {
    await busyAndSeen();
    await openView(page, newHost);
    const text = 'keyboard menu ' + stamp;
    await queue(page, text, false);
    const r = rowIn(page, text, 'queued');
    const more = moreOf(r);
    await expect(more).toHaveAttribute('aria-haspopup', 'menu');
    await expect(more).toHaveAttribute('aria-expanded', 'false');
    await more.focus();
    await page.keyboard.press('Enter');
    await expect(more).toHaveAttribute('aria-expanded', 'true');
    await expect(itemOf(r, 'Send now')).toBeFocused();
    await page.keyboard.press('ArrowDown');
    await expect(itemOf(r, 'Edit')).toBeFocused();
    await page.keyboard.press('ArrowDown');
    await expect(itemOf(r, 'Copy message')).toBeFocused();
    await page.keyboard.press('ArrowDown');
    await expect(itemOf(r, 'Cancel this queued message')).toBeFocused();
    await page.keyboard.press('ArrowDown');
    await expect(itemOf(r, 'Send now'), 'it wraps').toBeFocused();
    await page.keyboard.press('Escape');
    await expect(r.getByRole('menu')).toBeHidden();
    await expect(more).toHaveAttribute('aria-expanded', 'false');
    await expect(more, 'focus is back on the button').toBeFocused();
    // ArrowDown on the button opens it too; Cancel by keyboard cancels.
    await page.keyboard.press('ArrowDown');
    await expect(itemOf(r, 'Send now')).toBeFocused();
    await page.keyboard.press('End');
    await expect(itemOf(r, 'Cancel this queued message')).toBeFocused();
    await page.keyboard.press('Enter');
    await expect(rowIn(page, text, 'cancelled')).toHaveCount(1, { timeout: 20_000 });
    expect(pasted().some((l) => l.includes(text)), 'a cancelled message is not sent').toBe(false);
  });

  // 🔴 NO SCRIPT: the menu is the browser's own popover, and Send now and
  // Cancel are real forms; Copy message, which needs script, is not drawn.
  test('without JavaScript the menu opens, Send now and Cancel work as forms, and Copy is not offered', async ({ browser }) => {
    await busyAndSeen();
    fs.rmSync(tr + '.pasted', { force: true });
    // Queued with script, in a page of its own.
    const setup = await browser.newContext({ viewport: { width: 1280, height: 800 } });
    const sp = await setup.newPage();
    await openView(sp, newHost);
    const sendText = 'no-js send now ' + stamp;
    const cancelText = 'no-js cancel ' + stamp;
    await queue(sp, sendText, false);
    await queue(sp, cancelText, false);
    await setup.close();

    const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, javaScriptEnabled: false });
    const page = await ctx.newPage();
    await login(page);
    await page.goto(origin + viewPath(newHost));
    const r = rowIn(page, sendText, 'queued');
    await moreOf(r).click();
    await expect(r.getByRole('menu'), 'the popover opens with no script').toBeVisible();
    await expect(itemOf(r, 'Copy message'), 'no Copy without script').toHaveCount(0);
    await itemOf(r, 'Send now').click();
    await expect(page.locator('#mv-note')).toHaveText('Sent.');
    await expect.poll(pasted, { message: 'sent by the form', timeout: 15_000 }).toEqual([`${pane} ${sendText}`]);
    const c = rowIn(page, cancelText, 'queued');
    await moreOf(c).click();
    await itemOf(c, 'Cancel this queued message').click();
    await expect(page.locator('#mv-note')).toContainText('Cancelled');
    await expect(rowIn(page, cancelText, 'cancelled')).toHaveCount(1);
    await ctx.close();
  });
});

// EDITING A QUEUED MESSAGE (the operator's choice: edit in place, HELD while
// editing). Edit, in the row's ⋯ menu, holds the message on the server and
// opens its text in the row; while it is open nothing of the window's queue is
// sent; Save keeps its place; Cancel leaves it as it was; an abandoned edit's
// hold runs out and the message goes back to the queue with its original text.
// On origin/main the menu has no Edit, so each of these fails at the menu item.
const editing = (page: Page) => page.locator('#mv-queue [data-queue-editing]');
const editBox = (page: Page) => editing(page).getByRole('textbox', { name: 'Edit this queued message' });
async function openEdit(page: Page, r: Locator, touch: boolean) {
  await openMenu(r, touch);
  const e = itemOf(r, 'Edit');
  if (touch) await e.tap();
  else await e.click();
  await expect(editBox(page), 'the row opens for editing').toBeVisible();
}
async function press(l: Locator, touch: boolean) {
  if (touch) await l.tap();
  else await l.click();
}

for (const vp of [
  { name: 'phone 390×844 touch', touch: true, use: phone },
  { name: 'desktop 1280×800', touch: false, use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(`edit a queued message, ${vp.name}`, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });

    // 🔴 THE WHOLE CYCLE: held while open (the window idle, and nothing sent
    // — neither it nor the one behind it), saved in place, and the NEW text is
    // what is sent, still first.
    test('an edited message is held while open, saved in place, and its new text is sent in its original order', async ({ page }) => {
      await busyAndSeen();
      fs.rmSync(tr + '.pasted', { force: true });
      await openView(page, newHost);
      const first = `edit me ${vp.touch ? 'p' : 'd'} ${stamp}`;
      const second = `behind it ${vp.touch ? 'p' : 'd'} ${stamp}`;
      const edited = `edited text ${vp.touch ? 'p' : 'd'} ${stamp}`;
      await queue(page, first, vp.touch);
      await queue(page, second, vp.touch);
      await openEdit(page, rowIn(page, first, 'queued'), vp.touch);
      await expect(editBox(page)).toHaveValue(first);
      await expect(editBox(page), 'the box is focused to type in').toBeFocused();
      const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
      expect(sw, 'no horizontal scroll with the edit open').toBeLessThanOrEqual(cw);
      for (const name of ['Save', 'Cancel editing; keep the message as it was']) {
        const b = await editing(page).getByRole('button', { name }).boundingBox();
        expect(b && b.height >= 44, `${name} is a 44px target: ${JSON.stringify(b)}`).toBe(true);
      }
      await editBox(page).fill(edited);
      // Idle: had the hold not held, the first message would be sent now.
      busy(false);
      await seen();
      await holdWindow();
      expect(pasted(), 'nothing is sent while the message is open for editing').toEqual([]);
      // Another device sees it held.
      await press(editing(page).getByRole('button', { name: 'Save' }), vp.touch);
      await expect(page.locator('#mv-note')).toHaveText('Saved.');
      await expect.poll(pasted, { message: `the EDITED text is sent first (log:\n${mirrorLog})`, timeout: 30_000 }).toEqual([`${pane} ${edited}`]);
      // The second waits for the edited one's turn, then follows it.
      setStatus('idle');
      await expect.poll(pasted, { message: 'then the one behind it', timeout: 30_000 }).toEqual([`${pane} ${edited}`, `${pane} ${second}`]);
      await expect(rowIn(page, edited, 'sent'), 'the list shows the edited text sent').toHaveCount(1, { timeout: 20_000 });
      await busyAndSeen();
    });

    test('Cancel leaves the message as it was, and that is what is sent', async ({ page, browser }) => {
      await busyAndSeen();
      fs.rmSync(tr + '.pasted', { force: true });
      await openView(page, newHost);
      const text = `keep as is ${vp.touch ? 'p' : 'd'} ${stamp}`;
      await queue(page, text, vp.touch);
      await openEdit(page, rowIn(page, text, 'queued'), vp.touch);
      await editBox(page).fill('typed and thrown away ' + stamp);
      // Another device sees the message held while it is open.
      const other = await otherDevice(browser);
      await openView(other.page, newHost);
      await expect(rowIn(other.page, text, 'queued').locator('[data-queue-held]'), 'held, on every device').toHaveCount(1);
      await other.ctx.close();
      await press(editing(page).getByRole('button', { name: 'Cancel editing; keep the message as it was' }), vp.touch);
      await expect(page.locator('#mv-note')).toHaveText('Edit cancelled: the message is unchanged.');
      await expect(editing(page)).toHaveCount(0);
      await expect(rowIn(page, text, 'queued'), 'the original text, queued').toHaveCount(1);
      busy(false);
      await expect.poll(pasted, { message: `the original is sent (log:\n${mirrorLog})`, timeout: 30_000 }).toEqual([`${pane} ${text}`]);
      expect(pasted().some((l) => l.includes('thrown away')), 'the cancelled edit is never sent').toBe(false);
      await busyAndSeen();
    });

    // 🔴 AN ABANDONED EDIT CANNOT FREEZE THE QUEUE: its hold runs out (here
    // made to, on the database's clock, by e2e/sqfix), the message is queued
    // again with its ORIGINAL text, the late Save is refused saying so, and
    // what was typed is not thrown away.
    test('an edit whose hold ran out reverts to the original text, and its Save is refused', async ({ page }) => {
      await busyAndSeen();
      fs.rmSync(tr + '.pasted', { force: true });
      await openView(page, newHost);
      const text = `abandoned ${vp.touch ? 'p' : 'd'} ${stamp}`;
      const late = `never saved ${vp.touch ? 'p' : 'd'} ${stamp}`;
      await queue(page, text, vp.touch);
      await openEdit(page, rowIn(page, text, 'queued'), vp.touch);
      await editBox(page).fill(late);
      sqfix('lapse', newHost, text);
      await press(editing(page).getByRole('button', { name: 'Save' }), vp.touch);
      await expect(page.locator('#mv-note')).toContainText('Not saved: the hold on that message ran out');
      await expect(page.locator('#mv-note'), 'what was typed is kept').toContainText('Your edited text is in the message box.');
      await expect(page.locator('#mv-input')).toHaveValue(late);
      const r = rowIn(page, text, 'queued');
      await expect(r, 'the original text, queued again').toHaveCount(1);
      await expect(r.locator('[data-queue-lapsed]'), 'the row says the edit ran out').toContainText('kept its original text');
      busy(false);
      await expect.poll(pasted, { message: `the original is sent (log:\n${mirrorLog})`, timeout: 30_000 }).toEqual([`${pane} ${text}`]);
      // Tidy: the kept text (and its draft) go.
      await page.locator('#mv-input').fill('');
      await busyAndSeen();
    });
  });
}

test.describe('edit a queued message without JavaScript, desktop 1280×800', () => {
  test.use({ viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false, storageState: { cookies: [], origins: [] } });

  // 🔴 NO SCRIPT: Edit is a form (the server holds the message and redirects
  // back with it open), Save is a form, and the saved text is what is sent.
  test('Edit and Save work as forms, and the saved text is sent', async ({ browser }) => {
    await busyAndSeen();
    fs.rmSync(tr + '.pasted', { force: true });
    const setup = await browser.newContext({ viewport: { width: 1280, height: 800 } });
    const sp = await setup.newPage();
    await openView(sp, newHost);
    const text = 'no-js edit ' + stamp;
    const edited = 'no-js edited ' + stamp;
    await queue(sp, text, false);
    await setup.close();

    const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, javaScriptEnabled: false });
    const page = await ctx.newPage();
    await login(page);
    await page.goto(origin + viewPath(newHost));
    const r = rowIn(page, text, 'queued');
    await moreOf(r).click();
    await itemOf(r, 'Edit').click();
    await expect(page.locator('#mv-note')).toContainText('Editing: the message is held');
    await expect(editBox(page)).toHaveValue(text);
    await editBox(page).fill(edited);
    await editing(page).getByRole('button', { name: 'Save' }).click();
    await expect(page.locator('#mv-note')).toHaveText('Saved: the message keeps its place in the queue.');
    await expect(rowIn(page, edited, 'queued')).toHaveCount(1);
    busy(false);
    await expect.poll(pasted, { message: `the saved text is sent (log:\n${mirrorLog})`, timeout: 30_000 }).toEqual([`${pane} ${edited}`]);
    await ctx.close();
    await busyAndSeen();
  });
});

// MESSAGE DRAFTS IN THE RENDERED VIEW, PER DEVICE (composer_draft.go): what is
// typed is kept in this browser as it is typed, restored on coming back,
// cleared once sent or queued — and never offered in another window, another
// host, or the other view of the same window. On origin/main nothing is kept,
// so each fails at the first restore.
const tr2 = path.join(dir, 'eeeeeeee-0003.jsonl');
const windowsFile = path.join(dir, newHost + '.windows');
const viewOf = (host: string, w: string) => `/mirror/view?host=${encodeURIComponent(host)}&session=work&window=${encodeURIComponent(w)}`;

for (const vp of [
  { name: 'phone 390×844 touch', touch: true, use: phone },
  { name: 'desktop 1280×800', touch: false, use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(`drafts, ${vp.name}`, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });

    test('a draft survives navigating away and back, is cleared once sent, and is never restored into another window, host or view', async ({ page }) => {
      await busyAndSeen();
      fs.rmSync(tr + '.pasted', { force: true });
      // A second Claude window in the same session of the same host.
      const before = fs.readFileSync(windowsFile, 'utf8');
      fs.writeFileSync(tr2, prompt('the second window'));
      writeFileAtomic(windowsFile, before + `work|@2|1|0|claude|${Math.floor(Date.now() / 1000) - 60}|${tr2}\n`);
      try {
        await openView(page, newHost);
        const draft = `a draft, two lines ${vp.touch ? 'p' : 'd'} ${stamp}\nand its second line`;
        await page.locator('#mv-input').fill(draft);
        // Away (straight away: no wait for the debounce) and back.
        await page.goto(origin + '/');
        await page.goto(origin + viewPath(newHost));
        await expect(page.locator('#mv-input'), 'the draft is back').toHaveValue(draft);
        // Never into another window of the same session…
        await page.goto(origin + viewOf(newHost, '@2'));
        await expect(page.locator('#mv-input'), `the second window's view (log:\n${mirrorLog})`).toBeEnabled({ timeout: 20_000 });
        await expect(page.locator('#mv-input'), 'another window starts empty').toHaveValue('');
        // …another host with the same session and window names…
        await page.goto(origin + viewPath(oldHost));
        await expect(page.locator('#mv-input')).toBeEnabled({ timeout: 20_000 });
        await expect(page.locator('#mv-input'), 'another host starts empty').toHaveValue('');
        // …or the Terminal view of the same window, whose Send reaches another pane.
        await page.goto(origin + `/mirror/terminal?host=${encodeURIComponent(newHost)}&session=work&window=%401`);
        await expect(page.locator('#mv-input')).toBeVisible({ timeout: 20_000 });
        await expect(page.locator('#mv-input'), 'the Terminal view starts empty').toHaveValue('');
        // CONTROL: the window it was typed for still has it.
        await page.goto(origin + viewPath(newHost));
        await expect(page.locator('#mv-input')).toHaveValue(draft);

        // Sent: cleared, and it stays cleared across a navigation.
        await press(page.locator('#mv-send'), vp.touch);
        await expect(page.locator('#mv-input')).toHaveValue('');
        // (The fake records a paste as `<pane> <text>` per line it was given.)
        await expect.poll(pasted, { message: 'Send pastes it, both lines' }).toEqual([`${pane} ${draft.split('\n')[0]}`, draft.split('\n')[1]]);
        await page.goto(origin + '/');
        await page.goto(origin + viewPath(newHost));
        await expect(page.locator('#mv-input'), 'a sent draft is not restored').toHaveValue('');

        // Queued: cleared too.
        const queued = `a queued draft ${vp.touch ? 'p' : 'd'} ${stamp}`;
        await queue(page, queued, vp.touch);
        await page.goto(origin + '/');
        await page.goto(origin + viewPath(newHost));
        await expect(page.locator('#mv-input'), 'a queued draft is not restored').toHaveValue('');
        await cancelRow(rowIn(page, queued, 'queued'), vp.touch);
        await expect(rowIn(page, queued, 'cancelled')).toHaveCount(1);
      } finally {
        writeFileAtomic(windowsFile, before);
      }
    });

    // 🔴 ANOTHER ACCOUNT'S DRAFT IS NEVER OFFERED (the user is the first part of
    // every key): a draft stored for this window under ANOTHER account, and one
    // left by the old key format (no account in it — whoever typed it), are not
    // restored, and the old one is dropped. On origin/main the old-format draft
    // is restored to whoever opens the window, so this fails there.
    test("another account's draft is not restored on the same window, and an old-format draft is dropped", async ({ page }) => {
      await openView(page, newHost);
      const key = (await page.locator('#mv-input').getAttribute('data-draft-key')) ?? '';
      const legacy = 'muster.draft.v1:' + JSON.stringify(['mirror', 'view', newHost, 'work', '@1']);
      const theirs = 'muster.draft.v2:' + JSON.stringify([99, 'mirror', 'view', newHost, 'work', '@1']);
      expect(theirs, 'instrument: the other account’s key differs from this one’s').not.toBe(key);
      await page.evaluate(([l, o]) => {
        const at = Date.now();
        localStorage.setItem(l, JSON.stringify({ t: 'typed under the old key format', at }));
        localStorage.setItem(o, JSON.stringify({ t: "another account's draft", at }));
      }, [legacy, theirs]);
      await page.goto(origin + '/');
      await page.goto(origin + viewPath(newHost));
      await expect(page.locator('#mv-input')).toBeEnabled({ timeout: 20_000 });
      await expect(page.locator('#mv-input'), 'neither draft is restored for this account').toHaveValue('');
      expect(await page.evaluate((l) => localStorage.getItem(l), legacy), 'the old-format draft is dropped').toBeNull();
      // CONTROL: this account's own draft, under its own key, is restored.
      await page.evaluate((k) => localStorage.setItem(k, JSON.stringify({ t: 'my own draft', at: Date.now() })), key);
      await page.goto(origin + viewPath(newHost));
      await expect(page.locator('#mv-input')).toHaveValue('my own draft');
      await page.locator('#mv-input').fill('');
      await page.evaluate((o) => localStorage.removeItem(o), theirs);
    });

    // 🔴 THE SEND THE BROWSER MAKES ITSELF (the script hands the form over when
    // its own request gets no answer): the draft is cleared by the page it
    // comes back to, which says it was sent — not before, and not otherwise.
    test('a send handed to the browser clears the draft only once the page it lands on says it was sent', async ({ page }) => {
      await busyAndSeen();
      fs.rmSync(tr + '.pasted', { force: true });
      await openView(page, newHost);
      const text = `handed over ${vp.touch ? 'p' : 'd'} ${stamp}`;
      // The script's own request gets no answer: it hands the form to the browser.
      await page.route(/\/mirror\/view\/send(\?|$)/, async (route) => {
        if (route.request().headers()['x-requested-with']) await route.abort('connectionreset');
        else await route.continue();
      });
      await page.locator('#mv-input').fill(text);
      await Promise.all([
        page.waitForURL((u) => u.pathname === '/mirror/view' && u.searchParams.get('notice') === 'sent'),
        press(page.locator('#mv-send'), vp.touch),
      ]);
      await expect(page.locator('#mv-note')).toHaveText('Sent.');
      await expect(page.locator('#mv-input'), 'the draft is not restored after the send').toHaveValue('');
      await expect.poll(pasted).toEqual([`${pane} ${text}`]);
      await page.unroute(/\/mirror\/view\/send(\?|$)/);
      await page.goto(origin + '/');
      await page.goto(origin + viewPath(newHost));
      await expect(page.locator('#mv-input'), 'and it stays gone').toHaveValue('');
    });
  });
}

// 🔴 A USAGE LIMIT HOLDS THE QUEUE (internal/sendqueue limit.go). The bug: a
// session hit its usage limit and the queue sent every message it held into
// that limit. Claude Code under a limit rejects each message at once with a
// record it writes itself ("You've hit your session limit · resets …", model
// "<synthetic>") and goes back to its prompt — idle, with a newer time, which is
// all the queue waited for. The fake CLI below plays exactly that against the
// real host's transcript reader: every message pasted gets the limit as its
// only reply and the status goes idle. On origin/main the queue then drains —
// all three pasted, one per idle stretch — so the first test fails at its
// "nothing more is sent into it" assertion.
test.describe('a usage limit, phone 390×844 touch', () => {
  test.use({ ...phone, storageState: { cookies: [], origins: [] } });

  const iso = () => new Date().toISOString();
  // The reset, two hours ahead, as the CLI names it: "resets 9:30pm (UTC)",
  // and its own quotaLimits.resetsAt (Unix seconds), the one that wins.
  const resetAt = () => {
    const d = new Date(Date.now() + 2 * 3600_000);
    d.setUTCSeconds(0, 0);
    return d;
  };
  const resetText = (d: Date) => {
    const h = d.getUTCHours();
    const hh = h % 12 === 0 ? 12 : h % 12;
    return `${hh}:${String(d.getUTCMinutes()).padStart(2, '0')}${h < 12 ? 'am' : 'pm'} (UTC)`;
  };
  const promptRow = (text: string) => JSON.stringify({ type: 'user', timestamp: iso(), message: { role: 'user', content: text } }) + '\n';
  // limitRow is the CLI's limit record; null names no reset at all.
  const limitRow = (d: Date | null) =>
    JSON.stringify({
      type: 'assistant',
      timestamp: iso(),
      error: 'rate_limit',
      isApiErrorMessage: true,
      apiErrorStatus: 429,
      ...(d ? { quotaLimits: { status: 'rejected', resetsAt: Math.floor(d.getTime() / 1000), rateLimitType: 'five_hour' } } : {}),
      message: {
        model: '<synthetic>',
        role: 'assistant',
        content: [{ type: 'text', text: "You've hit your session limit" + (d ? ' · resets ' + resetText(d) : '') }],
      },
    }) + '\n';
  const replyRow = (text: string) =>
    JSON.stringify({ type: 'assistant', timestamp: iso(), message: { model: 'claude-e2e', role: 'assistant', content: [{ type: 'text', text }] } }) + '\n';

  // limitedCLI is Claude Code under the limit: each new paste is written to the
  // transcript as its prompt, answered only by the limit, and the status goes
  // idle with a newer time. It runs until stopped.
  function limitedCLI(reset: Date) {
    let seen = pasted().length;
    const timer = setInterval(() => {
      const ps = pasted();
      for (; seen < ps.length; seen++) {
        fs.appendFileSync(tr, promptRow(ps[seen].slice(pane.length + 1)) + limitRow(reset));
        setStatus('idle');
      }
    }, 100);
    return () => clearInterval(timer);
  }
  // answered is a real answer to the latest prompt: the limit has lifted.
  function answered(text: string) {
    fs.appendFileSync(tr, replyRow(text));
    setStatus('idle');
  }

  test('a usage limit holds the queue instead of draining it, fails the bounced message for a Retry by hand, and a real answer resumes the rest', async ({ page }) => {
    await busyAndSeen();
    fs.rmSync(tr + '.pasted', { force: true });
    await openView(page, newHost);
    const [a, b, c] = ['limit one ' + stamp, 'limit two ' + stamp, 'limit three ' + stamp];
    for (const t of [a, b, c]) await queue(page, t, true);
    await expect(page.locator('#mv-queue-waiting'), `busy is seen (log:\n${mirrorLog})`).toContainText('working', { timeout: 20_000 });
    const reset = resetAt();
    const stop = limitedCLI(reset);
    try {
      busy(false);
      await expect.poll(pasted, { message: `the first message is sent once idle (log:\n${mirrorLog})`, timeout: 30_000 }).toEqual([`${pane} ${a}`]);
      // The limit rejected it: the queue says it is held, until the reset in
      // the reader's own time — and then holds for as long as a wrong send
      // would take to land.
      const waiting = page.locator('#mv-queue-waiting');
      // Whichever comes first: the server says it holds, or a second message
      // goes into the limit (origin/main: the drain, then the third).
      await expect
        .poll(async () => pasted().length > 1 || ((await waiting.textContent()) ?? '').includes('Held'), { timeout: 30_000 })
        .toBe(true);
      expect(pasted(), 'the limit rejected the first: nothing more is sent into it').toEqual([`${pane} ${a}`]);
      await expect(waiting, `held on the limit (log:\n${mirrorLog})`).toContainText('Held — usage limit, resumes', { timeout: 20_000 });
      await expect(waiting.locator('time[data-clock]')).toHaveAttribute('data-clock', String(reset.getTime()));
      const local = await page.evaluate((ms) => {
        const d = new Date(ms);
        return d.toDateString() === new Date().toDateString()
          ? d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' })
          : d.toLocaleString([], { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });
      }, reset.getTime());
      await expect(waiting.locator('time[data-clock]'), 'the reset in the reader\'s local time').toHaveText(local);
      await holdWindow();
      await holdWindow();
      expect(pasted(), 'nothing else is sent into the limit').toEqual([`${pane} ${a}`]);
      // The bounced message is failed, saying why, with a plain Retry (no
      // "may already have been sent" question: the limit refused it).
      const bounced = rowIn(page, a, 'failed');
      await expect(bounced, 'the bounced message is failed').toHaveCount(1, { timeout: 20_000 });
      await expect(bounced).toContainText('Rejected: usage limit');
      await expect(bounced.getByRole('button', { name: 'Queue this failed message again' })).toBeVisible();
      await expect(rowIn(page, b, 'queued')).toHaveCount(1);
      await expect(rowIn(page, c, 'queued')).toHaveCount(1);
    } finally {
      stop();
    }
    // A real answer (to a message typed by hand) shows the limit has lifted:
    // the queue resumes at once, one per turn — and the bounced message is
    // not among them: only its Retry sends it again.
    answered('the limit has lifted');
    await expect.poll(pasted, { message: `the next message is sent (log:\n${mirrorLog})`, timeout: 30_000 }).toEqual([`${pane} ${a}`, `${pane} ${b}`]);
    answered('answered ' + b);
    await expect.poll(pasted, { timeout: 30_000 }).toEqual([`${pane} ${a}`, `${pane} ${b}`, `${pane} ${c}`]);
    answered('answered ' + c);
    await holdWindow();
    expect(pasted(), 'the bounced message is not re-sent on its own').toEqual([`${pane} ${a}`, `${pane} ${b}`, `${pane} ${c}`]);
    await expect(rowIn(page, a, 'failed')).toHaveCount(1);
    await busyAndSeen();
  });

  // A WEEKLY LIMIT resets days away: the held line says the date, not only a
  // time (which would read as today's).
  test('a limit that resets on another day shows its date', async ({ page }) => {
    await busyAndSeen();
    fs.rmSync(tr + '.pasted', { force: true });
    await openView(page, newHost);
    const text = 'weekly ' + stamp;
    await queue(page, text, true);
    await expect(page.locator('#mv-queue-waiting')).toContainText('working', { timeout: 20_000 });
    const reset = new Date(Date.now() + 3 * 24 * 3600_000);
    reset.setUTCSeconds(0, 0);
    fs.appendFileSync(tr, promptRow('typed by hand ' + stamp) + limitRow(reset));
    setStatus('idle');
    const clock = page.locator('#mv-queue-waiting time[data-clock]');
    await expect(clock, `held (log:\n${mirrorLog})`).toHaveAttribute('data-clock', String(reset.getTime()), { timeout: 20_000 });
    const want = await page.evaluate((ms) => new Date(ms).toLocaleString([], { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' }), reset.getTime());
    const month = await page.evaluate((ms) => new Date(ms).toLocaleString([], { month: 'short' }), reset.getTime());
    await expect(clock).toHaveText(want);
    await expect(clock).toContainText(month);
    expect(pasted(), 'held: nothing is sent').toEqual([]);
    await cancelRow(rowIn(page, text, 'queued'), true);
    await expect(rowIn(page, text, 'cancelled')).toHaveCount(1);
    answered('the limit has lifted');
    await busyAndSeen();
  });

  // 🔴 THE HELD LINE TAKES RESERVED SPACE. The queue going from "working" to
  // held is not the reader's input, so anything it moved would count against
  // the page. Here the hold comes with no send at all (the latest turn — a
  // message typed by hand — already hit the limit), so the only thing that
  // changes is that line; and the limit names no reset time, so it says the
  // LONGEST thing the held line says (two lines at this width, where
  // "working" is one) — the case the reservation is for. The positive control
  // is the same observer seeing a shift a block inserted above the queue
  // makes.
  test('going from working to held on a usage limit moves nothing (no layout shift)', async ({ page }) => {
    await busyAndSeen();
    fs.rmSync(tr + '.pasted', { force: true });
    await openView(page, newHost);
    const text = 'held line ' + stamp;
    await queue(page, text, true);
    const waiting = page.locator('#mv-queue-waiting');
    await expect(waiting).toContainText('working', { timeout: 20_000 });
    await page.evaluate(() => new Promise<void>((res) => requestAnimationFrame(() => requestAnimationFrame(() => res()))));
    await page.evaluate(() => {
      const w = window as unknown as { __shift: number; __shifts: string[] };
      w.__shift = 0;
      w.__shifts = [];
      new PerformanceObserver((l) => {
        for (const e of l.getEntries() as unknown as { value: number; hadRecentInput: boolean; sources: { node?: Node | null }[] }[]) {
          const nodes = (e.sources || []).map((x) => x.node).filter((n): n is Node => !!n);
          const ours = nodes.some((n) => {
            const el = n instanceof Element ? n : n.parentElement;
            return !!el && !!el.closest('#mv-compose');
          });
          if (ours) w.__shift += e.value;
          w.__shifts.push(`${e.value.toFixed(4)} ours=${ours} ` + nodes.map((n) => (n instanceof Element ? n.id || n.tagName : n.nodeName)).join(','));
        }
      }).observe({ type: 'layout-shift', buffered: false });
    });
    const before = await rowIn(page, text, 'queued').boundingBox();
    // A prompt typed by hand, rejected by a limit that names no reset; the
    // turn ends.
    fs.appendFileSync(tr, promptRow('typed by hand ' + stamp) + limitRow(null));
    setStatus('idle');
    await expect(waiting, `held (log:\n${mirrorLog})`).toContainText('Held — usage limit; no reset time given', { timeout: 20_000 });
    const lines = await waiting.evaluate((el) => Math.round(el.scrollHeight / parseFloat(getComputedStyle(el).lineHeight)));
    expect(lines, 'instrument check: the held line wraps, so the reservation is what keeps it still').toBe(2);
    await page.evaluate(() => new Promise<void>((res) => requestAnimationFrame(() => requestAnimationFrame(() => res()))));
    await page.waitForTimeout(300);
    const shift = await page.evaluate(() => (window as unknown as { __shift: number }).__shift);
    const log = await page.evaluate(() => (window as unknown as { __shifts: string[] }).__shifts);
    test.info().annotations.push({ type: 'layout-shift', description: `held line: ${shift} ${JSON.stringify(log)}` });
    // Under 0.001, not the page budget's 0.01: one line of growth with no
    // reservation measured 0.005–0.006 here (the composer's top moving), which the
    // budget would let through.
    expect(shift, 'no layout shift going to held: ' + JSON.stringify(log)).toBeLessThan(0.001);
    expect(await rowIn(page, text, 'queued').boundingBox(), 'the queued row did not move').toEqual(before);
    expect(pasted(), 'held: nothing is sent').toEqual([]);
    // CONTROL: the observer sees a shift when something above the queue grows.
    await page.evaluate(() => {
      const q = document.getElementById('mv-queue');
      const d = document.createElement('div');
      d.style.height = '120px';
      q?.parentElement?.insertBefore(d, q);
    });
    await page.evaluate(() => new Promise<void>((res) => requestAnimationFrame(() => requestAnimationFrame(() => res()))));
    await page.waitForTimeout(300);
    const control = await page.evaluate(() => (window as unknown as { __shift: number }).__shift);
    test.info().annotations.push({ type: 'layout-shift', description: `control: ${control}` });
    expect(control, 'CONTROL: the same observer sees an inserted block shift the queue').toBeGreaterThan(shift);
    // At 320px the held line still takes at most its two reserved lines —
    // clamped, the whole text in its title.
    await page.setViewportSize({ width: 320, height: 640 });
    const at320 = await waiting.evaluate((el) => ({
      lines: Math.round(el.getBoundingClientRect().height / parseFloat(getComputedStyle(el).lineHeight)),
      title: el.getAttribute('title') ?? '',
    }));
    expect(at320.lines, 'two lines at 320px').toBeLessThanOrEqual(2);
    expect(at320.title).toContain('Held — usage limit; no reset time given');
    // A tap shows the whole line in a popover — the clamped end ("…or tap
    // Send now.") included — and moves nothing on the page.
    const before320 = await page.evaluate(() => (window as unknown as { __shift: number }).__shift);
    const rowBefore = await rowIn(page, text, 'queued').boundingBox();
    await waiting.tap();
    const full = page.locator('#mv-queue-waiting-full');
    await expect(full).toBeVisible();
    await expect(full).toContainText('Send now');
    await page.evaluate(() => new Promise<void>((res) => requestAnimationFrame(() => requestAnimationFrame(() => res()))));
    expect(await page.evaluate(() => (window as unknown as { __shift: number }).__shift), 'opening the whole line shifts nothing').toBe(before320);
    expect(await rowIn(page, text, 'queued').boundingBox(), 'the queued row did not move').toEqual(rowBefore);
    await page.keyboard.press('Escape');
    await expect(full).toBeHidden();
    await page.setViewportSize({ width: 390, height: 844 });
    // Tidy: cancel it, and leave the session answered for the specs after.
    await cancelRow(rowIn(page, text, 'queued'), true);
    await expect(rowIn(page, text, 'cancelled')).toHaveCount(1);
    answered('the limit has lifted');
    await busyAndSeen();
  });
});

// 🔴 THE CLAUDE PROMPT GUARD: a message from muster must never be the keystroke
// that answers a permission prompt. Each test runs its OWN host and transcript,
// so it neither reads nor moves the shared one's status.
test.describe('Claude Code waiting on a permission prompt, desktop 1280×800', () => {
  test.use({ viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false, storageState: { cookies: [], origins: [] } });

  // ownHost starts a host whose Claude window writes transcript t, its status
  // idle (since now) at its hello; extra overrides its environment.
  function ownHost(name: string, extra: Record<string, string>) {
    const host = `e2e-sqpg${name}-${stamp}`;
    const t = path.join(dir, `eeeeeeee-${name}.jsonl`);
    fs.writeFileSync(t, prompt('the conversation so far'));
    const status = (s: string) => writeFileAtomic(t + '.status', JSON.stringify({ status: s, statusUpdatedAt: Date.now() }));
    status('idle');
    startMirror(host, t, extra);
    const typed = () => read(t + '.pasted').split('\n').filter((l) => l !== '');
    return { host, t, status, typed };
  }
  // connected waits until host's muster-mirror says it is connected: a view
  // opened before that is a page with no composer, which does not recover.
  const connected = (host: string) =>
    expect.poll(() => mirrorLog.includes(`as host ${host} `), { message: `${host} connects`, timeout: 20_000 }).toBe(true);

  // The host reports the prompt (its usual refresh): muster itself refuses
  // Send, and holds a queued message until the prompt is answered.
  test('Send is refused and types nothing; a queued message is held until the prompt is answered, then sent', async ({ page }) => {
    const h = ownHost('seen', {});
    await connected(h.host);
    await openView(page, h.host);
    h.status('waiting');
    await seen();
    await page.locator('#mv-input').fill('1');
    await page.locator('#mv-send').click();
    await expect(page.locator('#mv-note')).toContainText('Claude is waiting on a permission prompt — answer it first');
    await expect(page.locator('#mv-input'), 'the text stays in the box').toHaveValue('1');
    await queue(page, 'after the prompt', false);
    await expect(page.locator('#mv-queue-waiting'), 'the server holds it').toContainText('waiting for you', { timeout: 20_000 });
    await holdWindow();
    expect(h.typed(), 'nothing was typed into the prompt').toEqual([]);
    h.status('idle');
    await expect.poll(h.typed, { message: 'sent once the prompt is answered', timeout: 20_000 }).toEqual([`${pane} after the prompt`]);
  });

  // The race only the host can close: a host that last reported IDLE (its
  // refresh is an hour, so muster never hears of the prompt) — the dispatcher
  // decides to send, and the host, reading the status at the moment of typing,
  // refuses. The message stays queued, is never typed into the prompt, and is
  // sent once the prompt is answered. A person's Send into the same window is
  // refused by the host in the same words.
  test('a prompt that opened after the host last reported is refused at the moment of typing; the queued message waits and is then sent', async ({ page }) => {
    const h = ownHost('race', { MUSTER_MIRROR_E2E_REFRESH: '1h' });
    await connected(h.host);
    await openView(page, h.host);
    h.status('waiting');
    const logFrom = mirrorLog.length;
    await page.locator('#mv-input').fill('2');
    await page.locator('#mv-send').click();
    await expect(page.locator('#mv-note')).toContainText('Claude is waiting on a permission prompt — answer it first');
    // The refusal is the HOST's, at the moment of typing (muster never heard
    // of the prompt): its own log says so.
    expect(mirrorLog.slice(logFrom), 'the host refused it').toContain(`Claude Code in pane ${pane} is waiting on the person`);
    const queuedFrom = mirrorLog.length;
    await queue(page, 'raced the prompt', false);
    // 🔴 HELD WITH THE HOST'S REASON, BACKING OFF — not re-sent at every
    // 2-second dispatcher pass with nothing said.
    await expect(page.locator('#mv-queue-waiting'), 'the hold says why').toContainText('Claude is waiting on a permission prompt', { timeout: 20_000 });
    await holdWindow();
    await holdWindow();
    expect(h.typed(), 'nothing was typed into the prompt').toEqual([]);
    await expect(row(page, 'raced the prompt'), 'still queued').toHaveAttribute('data-queue-state', 'queued');
    const tries = mirrorLog.slice(queuedFrom).split(`Claude Code in pane ${pane} is waiting on the person`).length - 1;
    expect(tries, 'the host was asked again with a backoff (4 s, 8 s, …), not every pass').toBeGreaterThanOrEqual(1);
    expect(tries, 'the host was asked again with a backoff (4 s, 8 s, …), not every pass').toBeLessThanOrEqual(3);
    h.status('idle');
    await expect.poll(h.typed, { message: 'sent once the prompt is answered', timeout: 40_000 }).toEqual([`${pane} raced the prompt`]);
  });

  // The narrowest window: a prompt that opens AS the message lands, between
  // the paste and its Enter. The host's checked paste reads the pane again
  // before the Enter, sees the prompt, and withholds the Enter: the text sits
  // in the input box, unsubmitted, and the reply says so.
  test('a prompt that opens as the message lands gets no Enter, and the reply says pasted, not submitted', async ({ page }) => {
    const h = ownHost('enter', {});
    await connected(h.host);
    await openView(page, h.host);
    fs.writeFileSync(h.t + '.prompt-on-paste', '');
    await page.locator('#mv-input').fill('3');
    await page.locator('#mv-send').click();
    await expect(page.locator('#mv-note')).toContainText('Pasted, not submitted');
    const inBox = () => read(h.t + '.typed').split('\n').filter((l) => l !== '');
    expect(inBox(), 'the text landed in the box').toEqual([`${pane} 3`]);
    expect(h.typed(), 'no Enter was pressed into the prompt').toEqual([]);

    // 🔴 THE LEFTOVER: the prompt is answered, but "3" is still in the input
    // box. Nothing more is pasted after it — a queued message or a Send would
    // be submitted WITH it as one turn — until the box is cleared.
    fs.rmSync(h.t + '.prompt-on-paste');
    fs.rmSync(h.t + '.screen');
    h.status('idle');
    await seen();
    await queue(page, 'after the leftover', false);
    await expect(page.locator('#mv-queue-waiting'), 'held, saying why').toContainText('input box already has text', { timeout: 20_000 });
    await holdWindow();
    expect(inBox(), 'nothing was pasted after the leftover').toEqual([`${pane} 3`]);
    expect(h.typed(), 'and nothing submitted').toEqual([]);
    await page.locator('#mv-input').fill('4');
    await page.locator('#mv-send').click();
    await expect(page.locator('#mv-note')).toContainText("Claude's input box already has text — clear or send it first");
    expect(inBox(), 'a Send into the full box typed nothing').toEqual([`${pane} 3`]);
    // The person clears the box: the queued message goes, on its own.
    fs.rmSync(h.t + '.box');
    await expect.poll(h.typed, { message: 'sent once the box is clear', timeout: 40_000 }).toEqual([`${pane} after the leftover`]);
  });
});
