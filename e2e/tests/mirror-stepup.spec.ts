import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import crypto from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import {
  binDir, dsn, e2eRoot, gotrueSecret, repoRoot, stepupBrowserPort, stepupGotruePort, stepupMainPort, versionA,
} from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { Authenticator, authenticator, credentialIds, removeCredential, setUserVerified } from '../lib/webauthn';

// STEP-UP for typing into a mirrored session, end to end against the REAL
// GoTrue image (e2e/gotrue/compose.yml, the deployment's pin), a gotrue-mode
// muster with the mirror plugin on, and a REAL muster-mirror (`-tags
// e2emirror`: only tmux is played) started -interactive.
//
// The Interactive switch attaches rw at once on a FRESH sign-in (within the
// step-up window — 12 hours, the server's default, which this spec runs with —
// read from the access token's `amr`), and on a STALE one asks "Confirm it's
// you" first: a PASSKEY (a WebAuthn assertion with user verification, verified
// by GoTrue and bound by muster to the signed-in user) or the password again.
// The server refuses a stale rw attach or send whatever the page believes.
//
// This GoTrue runs with passkeys ON (MUSTER_E2E_GOTRUE_PASSKEYS), so the
// dialog's passkey path is the real one; the first three tests never register a
// passkey, and so also pin that an account WITHOUT one is offered the password
// alone.
//
// 🔴 HOW A STALE SESSION IS MADE WITHOUT WAITING 12 HOURS: the access cookie
// is re-signed with the e2e JWT secret (which muster and this GoTrue share)
// with its `amr` timestamp moved 13 hours back and everything else — sub,
// session_id, iss, aud — the real token's. That is byte-for-byte the token
// GoTrue mints for a session signed in 13 hours ago (measured: a refresh
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
  MUSTER_E2E_GOTRUE_PASSKEYS: 'true',
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
// `main` is played as a Claude Code pane (cmd/muster-mirror/e2emirror.go) whose
// transcript is this file, so the session also has a RENDERED view; a message
// sent there is appended to <transcript>.pasted by the fake host.
const transcriptDir = fs.mkdtempSync(path.join(os.tmpdir(), 'muster-e2e-stepup-cc-'));
const transcript = path.join(transcriptDir, 'e2e-stepup-0001.jsonl');
fs.writeFileSync(
  transcript,
  JSON.stringify({ type: 'user', promptId: 'p', message: { role: 'user', content: 'hello there' } }) + '\n' +
    JSON.stringify({ type: 'assistant', message: { content: [{ type: 'text', text: 'Hi.' }] } }) + '\n',
);

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
  fs.rmSync(transcriptDir, { recursive: true, force: true });
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
      MUSTER_MIRROR_E2E_CLAUDE: 'main=' + transcript,
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

// How long ago makeStale's session "signed in": an hour past the 12 hour
// window. (10 minutes — stale under the old five-minute window — is FRESH now,
// which the window test below pins.)
const STALE_SECONDS = 13 * 3600;

// makeStale replaces the access cookie with the same session signed in `ago`
// seconds ago — by default 13 hours, an hour past the window (see the header).
async function makeStale(ctx: BrowserContext, ago = STALE_SECONDS) {
  const real = await accessToken(ctx);
  const claims = b64urlJSON(real.split('.')[1]);
  const now = Math.floor(Date.now() / 1000);
  claims.amr = [{ method: 'password', timestamp: now - ago }];
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
  expect(Date.now() / 1000 - amrOf(staleToken), 'still the stale session').toBeGreaterThan(STALE_SECONDS - 100);

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

// ---------------------------------------------------------------------------
// SENDING FROM THE RENDERED VIEW, on a phone (390×844, touch), by the real tap
// path: the Agents list → the Claude session's card → the rendered view → type
// → Send. With real GoTrue, so the stale refusal is the SERVER's own (in
// mirrored.spec.ts it is played, because a password-mode session cannot be
// aged in a browser).
//
// ⚠ AN INVARIANT GUARD, NOT A REGRESSION TEST: it was written while
// investigating a report that messages could not be sent from the rendered
// view, and it PASSES on the code that report was made against (4208deb). It
// pins that the composer is there by default — no Interactive switch, which
// lives only on the Terminal page — and that a stale sign-in is asked to
// confirm inline and the SAME text is then sent.
// ---------------------------------------------------------------------------
test.describe('the rendered view on a phone (390×844)', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

  test('a message is typed and sent from the rendered view: at once when the sign-in is fresh, after an inline confirm when it is stale (real GoTrue)', async ({ page, context }) => {
    test.setTimeout(120_000);
    // Signed in through /login: like the desktop test above, this relies on the
    // 412px test having claimed the account (the file's tests run in order).
    await page.goto(browserURL + '/login');
    await page.fill('#muster-email', adminEmail);
    await page.fill('#muster-password', adminPW);
    await Promise.all([page.waitForURL(browserURL + '/'), page.click('button[type=submit]')]);
    if (!mirrorProc || mirrorProc.exitCode !== null) mirrorProc = startMirror();
    const pasted = () => (fs.existsSync(transcript + '.pasted') ? fs.readFileSync(transcript + '.pasted', 'utf8') : '');
    const sends: number[] = [];
    page.on('response', (r) => {
      if (new URL(r.url()).pathname === '/mirror/view/send') sends.push(r.status());
    });

    // The card of a Claude session opens the rendered view.
    await page.goto(browserURL + '/agents');
    const card = page.locator('#agents-list article[data-mirror-card]').filter({ hasText: `Mirrored · ${host}` });
    await expect(card.locator('[data-mirror-claude]'), `main is a Claude pane (muster-mirror log:\n${mirrorLog})`).toBeVisible({ timeout: 20_000 });
    await Promise.all([page.waitForURL((u) => u.pathname === '/mirror/view'), card.locator('a').first().tap()]);
    await expect(page.locator('#mv-log [data-bubble="assistant"]').first()).toContainText('Hi.');

    // The composer is THERE, enabled and on screen, with nothing to switch on.
    const input = page.locator('#mv-input');
    const send = page.locator('#mv-send');
    await expect(input).toBeEnabled();
    await expect(send).toBeEnabled();
    await expect(page.locator('#term-mode')).toHaveCount(0);
    for (const el of [input, send]) {
      const b = (await el.boundingBox())!;
      expect(b.y + b.height, 'the composer is above the bottom of a 390×844 screen').toBeLessThanOrEqual(844);
      expect(b.x + b.width, 'the composer fits 390px').toBeLessThanOrEqual(390);
      expect(b.height, 'a 44px touch target').toBeGreaterThanOrEqual(44);
    }

    // 1. FRESH: typed, tapped, pasted into the Claude pane; the composer clears
    // and the prompt comes back through the transcript.
    const before = pasted();
    await input.tap();
    await page.keyboard.type('first from the phone');
    await send.tap();
    await expect.poll(pasted).toBe(before + '%9 first from the phone\n');
    await expect(input).toHaveValue('');
    await expect(page.locator('#mv-log [data-bubble="user"]').last()).toContainText('first from the phone');
    await expect(dialog(page)).toBeHidden();
    expect(sends, 'one send, accepted').toEqual([204]);

    // 2. STALE: the server refuses (403 + X-Muster-Step-Up), the page asks to
    // confirm, and NOTHING is pasted and nothing typed is lost meanwhile.
    await makeStale(context);
    await input.tap();
    await page.keyboard.type('second, after the confirm');
    await send.tap();
    await expect(dialog(page)).toBeVisible();
    expect(sends, 'the stale send was refused by the server').toEqual([204, 403]);
    await expect(input).toHaveValue('second, after the confirm');
    expect(pasted()).toBe(before + '%9 first from the phone\n');
    const box = (await dialog(page).boundingBox())!;
    expect(box.x + box.width, 'the dialog fits 390px').toBeLessThanOrEqual(390);

    await page.fill('#term-confirm-password', 'not the password');
    await page.locator('#term-confirm-submit').tap();
    await expect(page.locator('#term-confirm-error')).toHaveText('That password is not right.');
    expect(pasted(), 'a wrong password sends nothing').toBe(before + '%9 first from the phone\n');
    await expect(input).toHaveValue('second, after the confirm');

    await page.fill('#term-confirm-password', adminPW);
    await page.locator('#term-confirm-submit').tap();
    await expect(dialog(page)).toBeHidden();
    await expect.poll(pasted).toBe(before + '%9 first from the phone\n%9 second, after the confirm\n');
    await expect(input).toHaveValue('');
    expect(sends, 'the SAME text was sent again after the confirm, once').toEqual([204, 403, 204]);
    expect(Math.abs(Date.now() / 1000 - amrOf(await accessToken(context))), 'the confirm signed in just now').toBeLessThan(60);
    await expect(page.locator('#mv-error')).toBeHidden();
    const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
    expect(sw, 'no horizontal scroll').toBeLessThanOrEqual(cw);
  });
});

// ---------------------------------------------------------------------------
// THE 12 HOUR WINDOW and CONFIRMING WITH A PASSKEY, with real GoTrue and
// Chromium's virtual authenticator (real signatures, verified by GoTrue).
//
// 🔴 REGRESSION TESTS: on the commit before the feature the window is five
// minutes (so a sign-in from ten minutes ago is refused, and the first test
// fails at its first send) and the dialog has no passkey button (so the others
// fail at `#term-confirm-passkey`).
// ---------------------------------------------------------------------------

const viewPath = `/mirror/view?host=${encodeURIComponent(host)}&session=main`;
const pasted = () => (fs.existsSync(transcript + '.pasted') ? fs.readFileSync(transcript + '.pasted', 'utf8') : '');
const passkeyBtn = (page: Page) => page.locator('#term-confirm-passkey');
const subOf = (token: string) => String(b64urlJSON(token.split('.')[1]).sub);

async function signIn(page: Page, email = adminEmail, pw = adminPW) {
  await page.goto(browserURL + '/login');
  await page.fill('#muster-email', email);
  await page.fill('#muster-password', pw);
  await page.click('button[type=submit]');
  // A sign-in that does not land says WHY (the page's own words), rather than
  // timing out on a URL.
  await page.waitForURL(browserURL + '/', { timeout: 20_000 }).catch(async () => {
    throw new Error(`sign-in as ${email} did not land on /: ${page.url()} says: ${(await page.locator('body').innerText()).slice(0, 400)}`);
  });
  if (!mirrorProc || mirrorProc.exitCode !== null) mirrorProc = startMirror();
}

async function signOut(page: Page) {
  await page.goto(browserURL + '/account');
  await Promise.all([page.waitForURL(/\/login$/), page.getByRole('button', { name: 'Sign out', exact: true }).click()]);
}

// addPasskey registers a passkey for the signed-in account on this page's
// authenticator. It needs a sign-in from the last FIVE minutes — that window
// did not lengthen — so every caller has just signed in.
async function addPasskey(page: Page) {
  await page.goto(browserURL + '/account/passkeys');
  await expect(page.locator('#passkey-add')).toBeVisible();
  const before = await page.locator('#passkey-list [data-passkey-id]').count();
  await page.locator('#passkey-add').click();
  await expect(page.locator('#passkey-list [data-passkey-id]'), 'the new passkey is listed').toHaveCount(before + 1);
}

// sendFrom opens the rendered view and returns its composer plus a live list of
// the send route's answers.
async function openView(page: Page) {
  const sends: number[] = [];
  page.on('response', (r) => {
    if (new URL(r.url()).pathname === '/mirror/view/send') sends.push(r.status());
  });
  await page.goto(browserURL + viewPath);
  await expect(page.locator('#mv-log [data-bubble="assistant"]').first()).toContainText('Hi.', { timeout: 20_000 });
  await expect(page.locator('#mv-input')).toBeEnabled();
  return { input: page.locator('#mv-input'), send: page.locator('#mv-send'), sends };
}

test.describe('the window and the passkey confirm on a phone (390×844)', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

  test('a sign-in from ten minutes ago still sends; one from over twelve hours ago is asked — by password alone when the account has no passkey (real GoTrue)', async ({ page, context }) => {
    test.setTimeout(120_000);
    await signIn(page);
    const { input, send, sends } = await openView(page);
    const before = pasted();

    // Ten minutes: stale under the old five-minute window, FRESH under twelve hours.
    await makeStale(context, 600);
    await input.tap();
    await page.keyboard.type('ten minutes in');
    await send.tap();
    await expect.poll(pasted).toBe(before + '%9 ten minutes in\n');
    await expect(input, 'cleared once the 204 arrived').toHaveValue('');
    expect(sends, 'sent at once, no step-up').toEqual([204]);
    await expect(dialog(page)).toBeHidden();

    // Eleven hours fifty: still inside the window.
    await makeStale(context, 12 * 3600 - 600);
    await input.tap();
    await page.keyboard.type('nearly twelve hours in');
    await send.tap();
    await expect.poll(pasted).toBe(before + '%9 ten minutes in\n%9 nearly twelve hours in\n');
    await expect(input).toHaveValue('');
    expect(sends).toEqual([204, 204]);
    await page.goto(browserURL + termPath);
    const freshFor = Number(await page.locator('#term').getAttribute('data-fresh-for'));
    expect(freshFor, 'about ten minutes of freshness left').toBeGreaterThan(8 * 60_000);
    expect(freshFor).toBeLessThanOrEqual(10 * 60_000);

    // Twelve hours and ten minutes: asked, and the dialog names the window.
    const v = await openView(page);
    await makeStale(context, 12 * 3600 + 600);
    await v.input.tap();
    await page.keyboard.type('past the window');
    await v.send.tap();
    await expect(dialog(page)).toBeVisible();
    expect(v.sends, 'refused by the server').toEqual([403]);
    await expect(dialog(page)).toContainText('needs a sign-in from the last 12 hours.');
    await expect(dialog(page)).not.toContainText('5 minutes');
    // No passkey is registered for this account: the dialog stays password-only
    // although this GoTrue has passkeys on and this browser supports WebAuthn.
    await page.waitForResponse((r) => r.url().endsWith('/auth/confirm/passkey/options')).catch(() => undefined);
    await expect(passkeyBtn(page)).toBeHidden();
    await expect(page.locator('#term-confirm-submit')).toHaveText('Confirm');
    await page.fill('#term-confirm-password', adminPW);
    await page.locator('#term-confirm-submit').tap();
    await expect(dialog(page)).toBeHidden();
    await expect.poll(pasted).toBe(before + '%9 ten minutes in\n%9 nearly twelve hours in\n%9 past the window\n');
    await expect(v.input).toHaveValue('');
    expect(v.sends).toEqual([403, 204]);
  });

  test('a stale send is confirmed with a PASSKEY and the same text is delivered; a UV=false assertion and another account’s passkey are refused; the password still works (real GoTrue, virtual authenticator)', async ({ page, context }) => {
    test.setTimeout(300_000);
    const auth: Authenticator = await authenticator(page);
    await signIn(page);
    await addPasskey(page);
    const adminCreds = await credentialIds(auth);
    expect(adminCreds.length, 'the authenticator holds the admin’s passkey').toBe(1);
    const adminSub = subOf(await accessToken(context));

    // 1. STALE → Send → the dialog offers the PASSKEY as the primary action →
    //    confirmed → the SAME text is delivered.
    let v = await openView(page);
    let before = pasted();
    await makeStale(context);
    await v.input.tap();
    await page.keyboard.type('sent after a passkey');
    await v.send.tap();
    await expect(dialog(page)).toBeVisible();
    expect(v.sends, 'the stale send was refused by the server').toEqual([403]);
    await expect(passkeyBtn(page)).toBeVisible();
    await expect(passkeyBtn(page), 'the passkey is the default action').toBeFocused();
    await expect(page.locator('#term-confirm-password'), 'the password stays as the fallback').toBeVisible();
    await expect(page.locator('#term-confirm-submit')).toHaveText('Use password');
    await expect(dialog(page)).toContainText('Confirm with your passkey, or use your password.');
    const box = (await dialog(page).boundingBox())!;
    expect(box.x + box.width, 'the dialog fits 390px').toBeLessThanOrEqual(390);
    expect(box.y + box.height, 'and is on screen').toBeLessThanOrEqual(844);
    expect((await passkeyBtn(page).boundingBox())!.height, 'a 44px touch target').toBeGreaterThanOrEqual(44);
    await expect(v.input, 'nothing typed is lost while confirming').toHaveValue('sent after a passkey');
    expect(pasted(), 'nothing is pasted while the confirm is open').toBe(before);
    const staleToken = await accessToken(context);

    await passkeyBtn(page).tap();
    await expect(dialog(page)).toBeHidden();
    await expect.poll(pasted).toBe(before + '%9 sent after a passkey\n');
    await expect(v.input).toHaveValue('');
    expect(v.sends, 'the SAME text was sent again after the confirm, once').toEqual([403, 204]);
    const freshToken = await accessToken(context);
    expect(freshToken).not.toBe(staleToken);
    expect(subOf(freshToken), 'the new session is the SAME user').toBe(adminSub);
    expect(Math.abs(Date.now() / 1000 - amrOf(freshToken)), 'the passkey confirm signed in just now').toBeLessThan(60);
    const methods = (b64urlJSON(freshToken.split('.')[1]).amr as { method: string }[]).map((e) => e.method);
    test.info().annotations.push({ type: 'amr-after-passkey-confirm', description: methods.join(',') });
    expect(methods, 'not a password sign-in').not.toContain('password');
    await expect(page.locator('#mv-note')).toBeHidden();

    // 2. 🔴 UV=false. The options are rewritten in flight to userVerification
    //    "discouraged" and the authenticator stops verifying, so the browser
    //    makes a correctly SIGNED assertion with UV=0 — which GoTrue (UV
    //    "preferred") would accept. muster refuses it; nothing is sent.
    before = pasted();
    await setUserVerified(auth, false);
    await page.route('**/auth/confirm/passkey/options', async (route) => {
      const resp = await route.fetch();
      const body = await resp.json();
      if (body.options) body.options.userVerification = 'discouraged';
      await route.fulfill({ response: resp, json: body });
    });
    v = await openView(page);
    await makeStale(context);
    await v.input.tap();
    await page.keyboard.type('must not be sent without verification');
    await v.send.tap();
    await expect(passkeyBtn(page)).toBeVisible();
    const verify = page.waitForResponse((r) => r.url().endsWith('/auth/confirm/passkey/verify'));
    await passkeyBtn(page).tap();
    expect((await verify).status(), 'muster refused the UV=0 assertion').toBe(401);
    await expect(page.locator('#term-confirm-error')).toContainText('did not verify you');
    await expect(dialog(page)).toBeVisible();
    expect(pasted(), 'a UV=0 assertion sends nothing').toBe(before);
    await expect(v.input).toHaveValue('must not be sent without verification');
    expect(Date.now() / 1000 - amrOf(await accessToken(context)), 'still the stale session').toBeGreaterThan(STALE_SECONDS - 100);
    await page.unrouteAll({ behavior: 'wait' });

    // 3. THE PASSWORD FALLBACK, in the same dialog, sends the same text.
    await setUserVerified(auth, true);
    await page.fill('#term-confirm-password', 'not the password');
    await page.locator('#term-confirm-submit').tap();
    await expect(page.locator('#term-confirm-error')).toHaveText('That password is not right.');
    expect(pasted(), 'a wrong password sends nothing').toBe(before);
    await page.fill('#term-confirm-password', adminPW);
    await page.locator('#term-confirm-submit').tap();
    await expect(dialog(page)).toBeHidden();
    await expect.poll(pasted).toBe(before + '%9 must not be sent without verification\n');
    await expect(v.input).toHaveValue('');

    // 4. 🔴 ANOTHER ACCOUNT'S PASSKEY. A member joins and registers a passkey on
    //    this same authenticator; the admin's own credential is then removed
    //    from it, so the only passkey this browser can present is the MEMBER's
    //    — perfectly valid, user-verified, and GoTrue (usernameless) accepts it.
    //    Presented on the ADMIN's session, muster refuses it: nothing is sent,
    //    and the browser is still the admin.
    await page.goto(browserURL + '/admin/invites');
    await page.getByRole('button', { name: 'Create link' }).click();
    const link = await page.locator('#once-link').inputValue();
    await signOut(page);
    const memberEmail = `stepup-member-${Date.now()}@example.com`;
    const memberPW = 'e2e-stepup-member-password-not-real';
    await page.goto(link);
    await page.fill('#invite-email', memberEmail);
    await page.fill('#invite-password', memberPW);
    await page.fill('#invite-password2', memberPW);
    await Promise.all([page.waitForURL(browserURL + '/account/passkeys?welcome=1'), page.getByRole('button', { name: 'Create account' }).click()]);
    await page.locator('#passkey-add').click();
    await expect(page.locator('#passkey-list [data-passkey-id]')).toHaveCount(1);
    await signOut(page);
    await removeCredential(auth, adminCreds[0]);
    expect((await credentialIds(auth)).length, 'the authenticator now holds ONLY the member’s passkey').toBe(1);

    await signIn(page);
    v = await openView(page);
    before = pasted();
    await makeStale(context);
    await v.input.tap();
    await page.keyboard.type('must not be sent as somebody else');
    await v.send.tap();
    await expect(passkeyBtn(page), 'the admin HAS a passkey, so one is offered').toBeVisible();
    const verify2 = page.waitForResponse((r) => r.url().endsWith('/auth/confirm/passkey/verify'));
    await passkeyBtn(page).tap();
    expect((await verify2).status(), 'muster refused another account’s passkey').toBe(403);
    await expect(page.locator('#term-confirm-error')).toContainText('belongs to a different account');
    await expect(dialog(page)).toBeVisible();
    expect(pasted(), 'another account’s passkey sends nothing').toBe(before);
    await expect(v.input).toHaveValue('must not be sent as somebody else');
    const after = await accessToken(context);
    expect(subOf(after), '🔴 the browser is STILL the admin, not the member').toBe(adminSub);
    expect(Date.now() / 1000 - amrOf(after), 'and still stale').toBeGreaterThan(STALE_SECONDS - 100);
    await page.locator('#term-confirm-cancel').tap();
    const account = await (await page.request.get(browserURL + '/account')).text();
    expect(account.includes(adminEmail) && !account.includes(memberEmail), 'the account page is the admin’s').toBe(true);
    const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
    expect(sw, 'no horizontal scroll').toBeLessThanOrEqual(cw);
  });
});

test.describe('the passkey confirm at a desktop width (1280px)', () => {
  test.use({ viewport: { width: 1280, height: 900 }, hasTouch: false, isMobile: false });

  test('the Interactive switch, and a send from the Terminal composer, are confirmed with a passkey (real GoTrue, virtual authenticator)', async ({ page, context }) => {
    test.setTimeout(180_000);
    await authenticator(page);
    await signIn(page);
    await addPasskey(page); // this browser's own passkey for the admin
    const adminSub = subOf(await accessToken(context));

    // The Interactive attach: stale → the switch asks → passkey → rw.
    await makeStale(context);
    await page.goto(browserURL + termPath);
    await expect(screen(page)).toContainText('FAKE-MIRROR session=main mode=ro');
    await page.locator('#term-mode').click();
    await expect(dialog(page)).toBeVisible();
    await expect(dialog(page)).toContainText('needs a sign-in from the last 12 hours.');
    await expect(passkeyBtn(page)).toBeVisible();
    await expect(passkeyBtn(page)).toBeFocused();
    await passkeyBtn(page).click();
    await expect(dialog(page)).toBeHidden();
    await expect(screen(page)).toContainText('mode=rw');
    await expect(page.locator('#term-status')).toHaveText('Connected · interactive');
    await expect(page.locator('#term-mode')).toHaveAttribute('aria-checked', 'true');
    let token = await accessToken(context);
    expect(subOf(token)).toBe(adminSub);
    expect(Math.abs(Date.now() / 1000 - amrOf(token)), 'the passkey confirm signed in just now').toBeLessThan(60);
    await page.locator('#term').click();
    await page.keyboard.type('ok');
    await expect(screen(page)).toContainText(/in=6F6B|in=6F/);

    // The Terminal view's composer (main's active pane is its Claude pane, %9):
    // stale → Send → passkey → the same text is delivered.
    await page.locator('#term-mode').click(); // back to read-only: the composer does not need the switch
    await expect(page.locator('#term-mode')).toHaveAttribute('aria-checked', 'false');
    const sends: number[] = [];
    page.on('response', (r) => {
      if (new URL(r.url()).pathname === '/mirror/terminal/send') sends.push(r.status());
    });
    const before = pasted();
    await makeStale(context);
    await page.locator('#mv-input').fill('from the desktop terminal');
    await page.locator('#mv-send').click();
    await expect(dialog(page)).toBeVisible();
    expect(sends).toEqual([403]);
    await expect(passkeyBtn(page)).toBeVisible();
    await passkeyBtn(page).click();
    await expect(dialog(page)).toBeHidden();
    await expect.poll(pasted).toBe(before + '%9 from the desktop terminal\n');
    await expect(page.locator('#mv-input')).toHaveValue('');
    expect(sends).toEqual([403, 204]);
    await expect(page.locator('#term-mode'), 'the send did not flip the switch').toHaveAttribute('aria-checked', 'false');
    token = await accessToken(context);
    expect(Math.abs(Date.now() / 1000 - amrOf(token))).toBeLessThan(60);
  });
});

// THE STEP-UP WITH NO SCRIPT. A browser context with JavaScript off has no
// confirm dialog: a stale send, submitted by the browser itself, is answered
// with a plain page — the password, and the message that was waiting — and
// confirming there delivers it.
//
// 🔴 A REGRESSION TEST: on the commit before this the composer had no method and
// no action (the browser would GET the page with `?text=…`), and a stale POST
// was a bare 403 text page with nothing to do on it.
test.describe('with no script at all (390×844)', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

  test('a stale send is confirmed on a plain page with the password, and the same text is then delivered once (real GoTrue)', async ({ page, context, browser }) => {
    test.setTimeout(120_000);
    await signIn(page);
    const viewPath = `/mirror/view?host=${encodeURIComponent(host)}&session=main`;
    await expect.poll(async () => (await (await page.request.get(browserURL + '/ui/agents')).text()).includes('data-mirror-claude'), {
      message: `main is a Claude pane (muster-mirror log:\n${mirrorLog})`, timeout: 20_000,
    }).toBe(true);
    const pasted = () => (fs.existsSync(transcript + '.pasted') ? fs.readFileSync(transcript + '.pasted', 'utf8') : '');
    const before = pasted();

    // The signed-in session, made stale, in a context that runs no script.
    await makeStale(context);
    const ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, javaScriptEnabled: false });
    await ctx.addCookies(await context.cookies(browserURL));
    const plain = await ctx.newPage();
    try {
      await plain.goto(browserURL + viewPath);
      await plain.locator('#mv-input').fill('typed with no script, before the confirm');
      await plain.locator('#mv-send').click();

      // Not delivered: the plain confirm page, in the send route's place.
      await expect(plain.locator('#stepup-form')).toBeVisible();
      await expect(plain.locator('#stepup-form')).toContainText('from the last 12 hours');
      expect(pasted(), 'nothing is delivered before the confirm').toBe(before);
      const box = (await plain.locator('#stepup-form').boundingBox())!;
      expect(box.x + box.width, 'the confirm page fits 390px').toBeLessThanOrEqual(390);
      for (const id of ['#stepup-password', '#stepup-submit', '#stepup-cancel']) {
        expect((await plain.locator(id).boundingBox())!.height, `${id} is a 44px target`).toBeGreaterThanOrEqual(44);
      }

      // A wrong password: the page again, saying so, and still nothing delivered.
      await plain.locator('#stepup-password').fill('not the password');
      await plain.locator('#stepup-submit').click();
      await expect(plain.locator('#stepup-error')).toHaveText('That password is not right.');
      expect(pasted()).toBe(before);

      // The right one: delivered, and back on the view, which says so.
      await plain.locator('#stepup-password').fill(adminPW);
      await plain.locator('#stepup-submit').click();
      await plain.waitForURL((u) => u.pathname === '/mirror/view' && u.searchParams.get('notice') === 'sent');
      await expect(plain.locator('#mv-note')).toHaveText('Sent.');
      expect(pasted(), 'the pending message was delivered once').toBe(before + '%9 typed with no script, before the confirm\n');

      // That confirm signed this browser in afresh: the next send needs none.
      await plain.locator('#mv-input').fill('and the next one');
      await plain.locator('#mv-send').click();
      await plain.waitForURL((u) => u.pathname === '/mirror/view' && u.searchParams.get('notice') === 'sent');
      expect(pasted()).toBe(before + '%9 typed with no script, before the confirm\n%9 and the next one\n');
    } finally {
      await ctx.close();
    }
  });
});
