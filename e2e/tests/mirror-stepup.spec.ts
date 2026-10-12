import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import crypto from 'node:crypto';
import path from 'node:path';
import { BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import {
  binDir, dsn, e2eRoot, gotrueSecret, repoRoot, stepupBrowserPort, stepupGotruePort, stepupMainPort, versionA,
} from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';

// STEP-UP for typing into a mirrored session, end to end against the REAL
// GoTrue image (e2e/gotrue/compose.yml, the deployment's pin), a gotrue-mode
// muster with the mirror plugin on, and a REAL muster-mirror (`-tags
// e2emirror`: only tmux is played) started -interactive.
//
// The Interactive switch attaches rw at once on a FRESH sign-in (within 5
// minutes, read from the access token's `amr`), and on a STALE one asks
// "Confirm it's you" — the password again, checked by GoTrue — first. The
// server refuses a stale rw attach whatever the page believes.
//
// 🔴 HOW A STALE SESSION IS MADE WITHOUT WAITING 5 MINUTES: the access cookie
// is re-signed with the e2e JWT secret (which muster and this GoTrue share)
// with its `amr` timestamp moved 10 minutes back and everything else — sub,
// session_id, iss, aud — the real token's. That is byte-for-byte the token
// GoTrue mints for a session signed in 10 minutes ago (measured: a refresh
// carries amr forward unchanged), so muster's verifier takes it as such. Its
// exp is pushed out so muster does not refresh it back into a fresh one
// mid-test (GOTRUE_JWT_EXP is 5 s here).

const compose = path.join(e2eRoot, 'gotrue', 'compose.yml');
const project = `muster-e2e-gotrue-${stepupGotruePort}`;
const composeEnv = {
  ...process.env,
  MUSTER_E2E_GOTRUE_PROJECT: project,
  MUSTER_E2E_GOTRUE_PORT: String(stepupGotruePort),
  MUSTER_E2E_GOTRUE_SITE: `http://localhost:${stepupBrowserPort}`,
};
const browserURL = `http://localhost:${stepupBrowserPort}`;
const machineURL = `http://localhost:${stepupMainPort}`;
const gotrueURL = `http://127.0.0.1:${stepupGotruePort}`;
const host = 'stepup-' + Date.now().toString(36);
const mirrorBin = path.join(binDir, 'muster-mirror-e2e-stepup');
const WIN = '120x30';
const adminEmail = `stepup-admin-${Date.now()}@example.com`;
const adminPW = 'e2e-stepup-password-not-a-real-one';

let server: Running | undefined;
let mirrorProc: ChildProcess | undefined;
let mirrorLog = '';

test.use({ viewport: { width: 412, height: 915 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(300_000);
  execFileSync('docker', ['compose', '-f', compose, 'up', '-d', '--wait'], { env: composeEnv, stdio: 'inherit' });
  const health = await fetch(gotrueURL + '/health');
  expect(((await health.json()) as { version?: string }).version).toBe('v2.197.0');
  execFileSync('go', ['run', './e2e/seed', '--accounts-only'], { cwd: repoRoot, stdio: 'inherit', env: { ...process.env, DATABASE_URL: dsn } });
  execFileSync('go', ['build', '-tags', 'e2emirror', '-o', mirrorBin, './cmd/muster-mirror'], { cwd: repoRoot, stdio: 'inherit' });
  server = await startServer(versionA, stepupMainPort, {
    MUSTER_AUTH_MODE: 'gotrue',
    MUSTER_GOTRUE_URL: gotrueURL,
    MUSTER_GOTRUE_JWT_SECRET: gotrueSecret,
    MUSTER_BROWSER_PORT: String(stepupBrowserPort),
    MUSTER_PUBLIC_URL: browserURL,
    MUSTER_MIRROR: '1',
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
  });
});

test.afterAll(async () => {
  if (mirrorProc && mirrorProc.exitCode === null) {
    const done = new Promise((r) => mirrorProc!.once('exit', r));
    mirrorProc.kill('SIGTERM');
    await done;
  }
  await server?.stop();
  execFileSync('docker', ['compose', '-f', compose, 'down', '-v'], { env: composeEnv, stdio: 'inherit' });
});

const serverEnv = () => ({ ...process.env, DATABASE_URL: dsn, MUSTER_PUBLIC_URL: browserURL, MUSTER_GOTRUE_URL: gotrueURL, MUSTER_GOTRUE_JWT_SECRET: gotrueSecret });

// claimAndSignIn claims the admin (user 1, the owner of every mirror token the
// server binary mints) and leaves the page signed in.
async function claimAndSignIn(page: Page) {
  const link = execFileSync(serverBinary(versionA), ['invite', '--claim-admin'], { env: serverEnv() }).toString().trim();
  await page.goto(link);
  await page.fill('#claim-email', adminEmail);
  await page.fill('#claim-password', adminPW);
  await page.fill('#claim-password2', adminPW);
  await Promise.all([page.waitForURL(browserURL + '/'), page.click('button[type=submit]')]);
}

function startMirror(): ChildProcess {
  const token = execFileSync(serverBinary(versionA), ['mirror', 'token', 'create', host], { env: serverEnv() }).toString().trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
  const p = spawn(mirrorBin, [], {
    env: {
      ...process.env,
      MUSTER_MIRROR_URL: machineURL,
      MUSTER_MIRROR_ALLOW_HTTP: '1',
      MUSTER_MIRROR_TOKEN: token,
      MUSTER_MIRROR_HOST: host,
      MUSTER_MIRROR_E2E_SESSIONS: 'main',
      MUSTER_MIRROR_E2E_WINDOW: WIN,
      MUSTER_MIRROR_INTERACTIVE: '1',
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  p.stdout?.on('data', (d) => (mirrorLog += d));
  p.stderr?.on('data', (d) => (mirrorLog += d));
  return p;
}

const termPath = `/mirror/terminal?host=${encodeURIComponent(host)}&session=main`;
const screen = (page: Page) => page.locator('#term .xterm-rows');
const dialog = (page: Page) => page.locator('#term-confirm');

function b64urlJSON(part: string): Record<string, unknown> {
  return JSON.parse(Buffer.from(part, 'base64url').toString('utf8'));
}

function sign(payload: Record<string, unknown>): string {
  const h = Buffer.from(JSON.stringify({ alg: 'HS256', typ: 'JWT' })).toString('base64url');
  const p = Buffer.from(JSON.stringify(payload)).toString('base64url');
  const s = crypto.createHmac('sha256', gotrueSecret).update(h + '.' + p).digest('base64url');
  return `${h}.${p}.${s}`;
}

async function accessToken(ctx: BrowserContext): Promise<string> {
  const c = (await ctx.cookies(browserURL)).find((k) => k.name === 'muster_at');
  expect(c, 'a session cookie').toBeTruthy();
  return c!.value;
}

function amrOf(token: string): number {
  const amr = b64urlJSON(token.split('.')[1]).amr as { timestamp: number }[];
  return Math.max(...amr.map((e) => e.timestamp));
}

// makeStale replaces the access cookie with the same session signed in 10
// minutes ago (see the header).
async function makeStale(ctx: BrowserContext) {
  const real = await accessToken(ctx);
  const claims = b64urlJSON(real.split('.')[1]);
  const now = Math.floor(Date.now() / 1000);
  claims.amr = [{ method: 'password', timestamp: now - 600 }];
  claims.exp = now + 600;
  await ctx.addCookies([{ name: 'muster_at', value: sign(claims), url: browserURL, httpOnly: true, secure: true, sameSite: 'Lax' }]);
}

test('the Interactive switch on a mirrored session needs a fresh sign-in, and asks for the password when it is stale (real GoTrue, 412px)', async ({ page, context }) => {
  test.setTimeout(180_000);
  await claimAndSignIn(page);
  mirrorProc = startMirror();
  // Every terminal socket the page OPENS, by mode — so "the page asked before
  // trying" is distinguishable from "it tried, the server refused, then it
  // asked".
  const rwOpened: string[] = [];
  page.on('websocket', (ws) => {
    if (ws.url().includes('/mirror/terminal/ws') && ws.url().includes('mode=rw')) rwOpened.push(ws.url());
  });

  // The card announces the host is interactive.
  await expect.poll(async () => (await (await page.request.get(browserURL + '/ui/agents')).text()).includes(`Mirrored · ${host}`), {
    message: `the host's card (muster-mirror log:\n${mirrorLog})`, timeout: 20_000,
  }).toBe(true);
  await page.goto(browserURL + '/agents');
  const card = page.locator('#agents-list article[data-mirror-card]').filter({ hasText: `Mirrored · ${host}` });
  await expect(card.locator('[data-mirror-interactive]')).toContainText('interactive');

  // 1. FRESH (just signed in): the switch attaches rw with no prompt, at the
  // session's own size (a phone does not shrink it).
  await page.goto(browserURL + termPath);
  await expect(screen(page)).toContainText('FAKE-MIRROR session=main mode=ro size=' + WIN);
  await expect(page.locator('#term')).toHaveAttribute('data-remote', WIN);
  expect(Number(await page.locator('#term').getAttribute('data-fresh-for'))).toBeGreaterThan(200_000);
  await page.locator('#term-mode').click();
  await expect(screen(page)).toContainText('FAKE-MIRROR session=main mode=rw size=' + WIN);
  await expect(page.locator('#term-status')).toHaveText('Connected · interactive');
  await expect(dialog(page)).toBeHidden();

  // Remembered for this host, within the window: a reload comes back rw.
  await page.reload();
  await expect(screen(page)).toContainText('mode=rw');
  await expect(page.locator('#term-mode')).toHaveAttribute('aria-checked', 'true');
  await expect(dialog(page)).toBeHidden();

  // 2. STALE: the page loads read-only even though rw is remembered, and
  // flipping the switch asks first.
  await makeStale(context);
  const rwBeforeStale = rwOpened.length;
  await page.reload();
  await expect(screen(page)).toContainText('mode=ro');
  await expect(page.locator('#term-mode')).toHaveAttribute('aria-checked', 'false');
  expect(await page.locator('#term').getAttribute('data-fresh-for')).toBe('0');
  await page.locator('#term-mode').click();
  await expect(dialog(page)).toBeVisible();
  expect(rwOpened.length, 'a stale page asks BEFORE trying rw (no rw socket opened)').toBe(rwBeforeStale);
  await expect(page.getByRole('heading', { name: "Confirm it's you" })).toBeVisible();
  const box = (await dialog(page).boundingBox())!;
  expect(box.x + box.width, 'the dialog fits 412px').toBeLessThanOrEqual(412);

  //    A wrong password is refused, and nothing attached.
  await page.fill('#term-confirm-password', 'not the password');
  await page.click('#term-confirm-submit');
  await expect(page.locator('#term-confirm-error')).toHaveText('That password is not right.');
  await expect(page.locator('#term-mode')).toHaveAttribute('aria-checked', 'false');
  const staleToken = await accessToken(context);
  expect(Date.now() / 1000 - amrOf(staleToken), 'still the stale session').toBeGreaterThan(500);

  //    The right one: a NEW sign-in (amr is now), then rw.
  await page.fill('#term-confirm-password', adminPW);
  await page.click('#term-confirm-submit');
  await expect(dialog(page)).toBeHidden();
  await expect(screen(page)).toContainText('mode=rw size=' + WIN);
  await expect(page.locator('#term-status')).toHaveText('Connected · interactive');
  const freshToken = await accessToken(context);
  expect(freshToken).not.toBe(staleToken);
  expect(Math.abs(Date.now() / 1000 - amrOf(freshToken)), 'the confirm signed in just now').toBeLessThan(60);

  //    Within the window: off and on again does not ask.
  await page.locator('#term-mode').click();
  await expect(page.locator('#term-mode')).toHaveAttribute('aria-checked', 'false');
  await page.locator('#term-mode').click();
  await expect(page.locator('#term-mode')).toHaveAttribute('aria-checked', 'true');
  await expect(page.locator('#term-status')).toHaveText('Connected · interactive');
  await expect(dialog(page)).toBeHidden();

  // 3. THE SERVER DECIDES, NOT THE PAGE: the page was rendered fresh, the
  // cookie goes stale under it, and the switch's rw attach is refused (403)
  // — the page falls back to read-only and asks.
  await page.locator('#term-mode').click(); // back to ro
  await expect(page.locator('#term-mode')).toHaveAttribute('aria-checked', 'false');
  await page.reload();
  await expect(screen(page)).toContainText('mode=ro');
  expect(Number(await page.locator('#term').getAttribute('data-fresh-for'))).toBeGreaterThan(0);
  await makeStale(context);
  const rwBeforeRefusal = rwOpened.length;
  await page.locator('#term-mode').click();
  await expect(dialog(page), 'the server refused the stale rw attach and the page asked').toBeVisible();
  expect(rwOpened.length, 'the page (believing it fresh) DID try rw once').toBe(rwBeforeRefusal + 1);
  await expect(page.locator('#term-mode')).toHaveAttribute('aria-checked', 'false');
  const status = await page.evaluate(async (u) => {
    const r = await fetch(u + '&mode=rw');
    return r.status;
  }, browserURL + `/mirror/terminal/ws?host=${encodeURIComponent(host)}&session=main`);
  expect(status, 'a stale rw attach is 403 at the server').toBe(403);
});

test.describe('at a desktop width (1280px)', () => {
  test.use({ viewport: { width: 1280, height: 900 }, hasTouch: false, isMobile: false });

  test('a stale desktop session is asked to confirm before the switch attaches rw (real GoTrue)', async ({ page, context }) => {
    test.setTimeout(120_000);
    // Signed in through /login this time (the 412px test claimed the account).
    await page.goto(browserURL + '/login');
    await page.fill('#muster-email', adminEmail);
    await page.fill('#muster-password', adminPW);
    await Promise.all([page.waitForURL(browserURL + '/'), page.click('button[type=submit]')]);
    if (!mirrorProc || mirrorProc.exitCode !== null) mirrorProc = startMirror();

    await makeStale(context);
    await page.goto(browserURL + termPath);
    await expect(screen(page)).toContainText('FAKE-MIRROR session=main mode=ro');
    // 1280px covers the 120x30 session: the desktop fits as before.
    await expect(page.locator('#term')).toHaveAttribute('data-overflow', '0');
    await page.locator('#term-mode').click();
    await expect(dialog(page)).toBeVisible();
    await page.fill('#term-confirm-password', adminPW);
    await page.click('#term-confirm-submit');
    await expect(dialog(page)).toBeHidden();
    await expect(screen(page)).toContainText('mode=rw');
    await expect(page.locator('#term-status')).toHaveText('Connected · interactive');
    // Typing reaches the host.
    await page.locator('#term').click();
    await page.keyboard.type('ok');
    await expect(screen(page)).toContainText(/in=6F6B|in=6F/);
  });
});
