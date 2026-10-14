import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { binDir, dsn, password, repoRoot, versionTerm, windowsPort } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';

// EVERY WINDOW IS MIRRORED, in a real browser, against a REAL muster-server
// (MUSTER_MIRROR=1) and a REAL muster-mirror dialling it over the tunnel. Only
// tmux is played: the `-tags e2emirror` binary's per-window fake
// (cmd/muster-mirror/e2emirror.go, MUSTER_MIRROR_E2E_WINDOWS_FILE), whose
// terminal prints `FAKE-MIRROR session=<s> window=<id> …` and which records
// every paste WITH THE PANE IT REACHED (`%e2e-<session>-<window id>`).
//
// The host is the operator's own shape: session `scratch2` (codename Gold) has
// three windows — a shell at index 1, Claude Code at index 8 (its ACTIVE
// window), Claude Code at index 11 — and session `solo` has one, opencode. With
// one card per session that host showed two cards and reached two windows; the
// other two were unreachable.
//
// Each test runs at a phone's size (390×844, touch) and at a desktop's.

const origin = `http://localhost:${windowsPort}`;
const host = 'e2e-win-' + Date.now().toString(36);
const mirrorBin = path.join(binDir, 'muster-mirror-e2e');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'mirror-windows-'));
const windowsFile = path.join(dir, 'windows');
const pasteFile = path.join(dir, 'pasted');
const trEight = path.join(dir, 'aaaaaaaa-0008.jsonl');
const trEleven = path.join(dir, 'bbbbbbbb-0011.jsonl');
let server: Running | undefined;
let mirrorProc: ChildProcess | undefined;
let mirrorLog = '';

const prompt = (text: string) => JSON.stringify({ type: 'user', promptId: 'p-' + text.length, message: { role: 'user', content: text } }) + '\n';

interface Win {
  session: string;
  id: string;
  index: number;
  active: boolean;
  command: string;
  activity: number;
  claude?: string;
}
const now = Math.floor(Date.now() / 1000);
const shellWin: Win = { session: 'scratch2', id: '@4', index: 1, active: false, command: 'zsh', activity: now - 3600 };
const eightWin: Win = { session: 'scratch2', id: '@7', index: 8, active: true, command: 'claude', activity: now - 1800, claude: trEight };
// The most recently active window is NOT its session's active one.
const elevenWin: Win = { session: 'scratch2', id: '@9', index: 11, active: false, command: 'claude', activity: now - 120, claude: trEleven };
const soloWin: Win = { session: 'solo', id: '@2', index: 3, active: true, command: 'opencode', activity: now - 900 };

function writeWindows(ws: Win[]) {
  const tmp = windowsFile + '.tmp';
  fs.writeFileSync(tmp, ws.map((w) => [w.session, w.id, w.index, w.active ? 1 : 0, w.command, w.activity, w.claude ?? ''].join('|')).join('\n') + '\n');
  fs.renameSync(tmp, windowsFile); // atomic: the host never reads half a list
}

const q = (w: Win) => `host=${encodeURIComponent(host)}&session=${encodeURIComponent(w.session)}&window=${encodeURIComponent(w.id)}`;
const viewPath = (w: Win) => '/mirror/view?' + q(w);
const termPath = (w: Win) => '/mirror/terminal?' + q(w);
const cards = (page: Page) => page.locator(`#agents-list article[data-mirror-card]:has([data-mirror-badge]:has-text("Mirrored · ${host}"))`);
const cardOf = (page: Page, href: string) => page.locator(`#agents-list article[data-mirror-card]:has(a[href="${href}"])`);
const screen = (page: Page) => page.locator('#term .xterm-rows');
const read = (f: string) => (fs.existsSync(f) ? fs.readFileSync(f, 'utf8') : '');

test.beforeAll(async () => {
  execFileSync('go', ['build', '-tags', 'e2emirror', '-o', mirrorBin, './cmd/muster-mirror'], { cwd: repoRoot, stdio: 'inherit' });
  server = await startServer(versionTerm, windowsPort, {
    MUSTER_MIRROR: '1',
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
  });
  const token = execFileSync(serverBinary(versionTerm), ['mirror', 'token', 'create', host], { env: { ...process.env, DATABASE_URL: dsn } })
    .toString()
    .trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
  fs.writeFileSync(trEight, prompt('the prompt of window EIGHT'));
  fs.writeFileSync(trEleven, prompt('the prompt of window ELEVEN'));
  fs.writeFileSync(path.join(dir, 'labels.json'), JSON.stringify({ scratch2: 'Gold' }));
  writeWindows([shellWin, eightWin, elevenWin, soloWin]);
  mirrorProc = spawn(mirrorBin, [], {
    env: {
      ...process.env,
      MUSTER_MIRROR_URL: origin,
      MUSTER_MIRROR_ALLOW_HTTP: '1',
      MUSTER_MIRROR_TOKEN: token,
      MUSTER_MIRROR_HOST: host,
      MUSTER_MIRROR_INTERACTIVE: '1',
      MUSTER_MIRROR_LABELS_FILE: path.join(dir, 'labels.json'),
      MUSTER_MIRROR_E2E_WINDOWS_FILE: windowsFile,
      MUSTER_MIRROR_E2E_PASTE_FILE: pasteFile,
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
  fs.rmSync(dir, { recursive: true, force: true });
});

// The form is driven once; the cookie it earns is handed to every later test
// (password-mode sign-in is throttled to 10 a minute in one shared bucket).
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

async function noHorizontalScroll(page: Page) {
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw, 'document scrollWidth').toBeLessThanOrEqual(cw);
}

async function openList(page: Page, want: number) {
  await login(page);
  await page.goto(origin + '/agents');
  await expect(cards(page), `this host's cards (muster-mirror log:\n${mirrorLog})`).toHaveCount(want, { timeout: 20_000 });
}

for (const vp of [
  { name: 'phone 390×844 touch', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.name, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });
    test.beforeEach(() => {
      writeWindows([shellWin, eightWin, elevenWin, soloWin]);
      fs.rmSync(pasteFile, { force: true });
      for (const t of [trEight, trEleven]) fs.rmSync(t + '.pasted', { force: true });
    });

    // 🔴 A REGRESSION TEST for "I only see a handful": on the commit before
    // per-window registration this host draws TWO cards (one per session), so
    // the count of four fails there — and the two windows that are not their
    // session's active one have no card to open at all.
    test('every window is a card of its own, named <codename>:<index>, most recently active first', async ({ page }) => {
      await openList(page, 4);
      const eleven = cardOf(page, viewPath(elevenWin));
      const eight = cardOf(page, viewPath(eightWin));
      const shell = cardOf(page, termPath(shellWin));
      const solo = cardOf(page, termPath(soloWin));

      // Named by codename and the window's OWN index, with the tmux session
      // name beside it; a session with no codename is <session>:<index>.
      await expect(eleven.locator('[data-mirror-label]')).toHaveText('Gold:11');
      await expect(eight.locator('[data-mirror-label]')).toHaveText('Gold:8');
      await expect(shell.locator('[data-mirror-label]')).toHaveText('Gold:1');
      for (const c of [eleven, eight, shell]) await expect(c.locator('[data-mirror-session]')).toHaveText('scratch2');
      await expect(solo.locator('[data-mirror-name]')).toHaveText('solo:3');
      await expect(solo.locator('[data-mirror-label]')).toHaveCount(0);

      // What runs in each.
      await expect(eleven.locator('[data-mirror-claude]')).toContainText('Claude Code');
      await expect(eight.locator('[data-mirror-claude]')).toContainText('Claude Code');
      await expect(shell.locator('[data-mirror-kind]')).toContainText('shell');
      await expect(solo.locator('[data-mirror-kind]')).toContainText('opencode');
      for (const c of [eleven, eight, shell, solo]) await expect(c.locator('[data-mirror-badge]')).toContainText(`Mirrored · ${host}`);

      // One flat list, most recently active first — two windows of ONE session
      // sit either side of another session's.
      const order = await cards(page).evaluateAll((els) =>
        els.map((e) => ((e.querySelector('[data-mirror-label]') ?? e.querySelector('[data-mirror-name]')) as HTMLElement).textContent),
      );
      expect(order).toEqual(['Gold:11', 'solo:3', 'Gold:8', 'Gold:1']);

      await noHorizontalScroll(page);
      for (const c of [eleven, solo]) {
        const b = (await c.boundingBox())!;
        expect(b.x + b.width, 'the card fits the viewport').toBeLessThanOrEqual(vp.use.viewport.width);
      }

      // Live, with no reload: a window is renumbered (its card follows, same
      // element), and another closes (its card goes).
      const id = await eleven.getAttribute('id');
      writeWindows([shellWin, eightWin, { ...elevenWin, index: 2 }, soloWin]);
      await expect(eleven.locator('[data-mirror-label]'), `the card follows the renumbering (log:\n${mirrorLog})`).toHaveText('Gold:2', { timeout: 20_000 });
      expect(await eleven.getAttribute('id'), 'a renumbered window is the SAME card: its address is the window id, not the index').toBe(id);
      writeWindows([eightWin, { ...elevenWin, index: 2 }, soloWin]);
      await expect(cards(page)).toHaveCount(3, { timeout: 20_000 });
      await expect(shell).toHaveCount(0);
    });

    // The window that is NOT its session's active one: its rendered view is
    // ITS transcript, and a message sent from it reaches ITS pane.
    test('a non-active Claude Code window opens its own rendered view, and Send reaches that window', async ({ page }) => {
      await openList(page, 4);
      await cardOf(page, viewPath(elevenWin)).locator('a').first().click();
      await page.waitForURL((u) => u.pathname === '/mirror/view');
      expect(new URL(page.url()).searchParams.get('window'), 'opened by the window id').toBe('@9');
      await expect(page.locator('header h1')).toContainText('Gold:11');
      await expect(page.locator('header h1 [data-mirror-session]')).toHaveText('scratch2');

      const log = page.locator('#mv-log');
      await expect(log.locator('[data-bubble="user"]').first()).toContainText('the prompt of window ELEVEN');
      await expect(log).not.toContainText('window EIGHT');

      await page.locator('#mv-input').fill('a message for window eleven');
      await page.locator('#mv-send').click();
      await expect(page.locator('#mv-input')).toHaveValue('');
      await expect.poll(() => read(trEleven + '.pasted'), { message: `pasted into window @9's pane (log:\n${mirrorLog})` }).toBe('%e2e-scratch2-@9 a message for window eleven\n');
      await expect(log.locator('[data-bubble="user"]').last()).toContainText('a message for window eleven');
      expect(read(trEight + '.pasted'), 'nothing reached the session’s ACTIVE window').toBe('');
      expect(read(pasteFile), 'nothing reached a non-Claude pane').toBe('');
      await expect(page.locator('#mv-note')).toBeHidden();

      // Its Terminal is this window's too.
      await page.locator('#view-terminal').click();
      await page.waitForURL((u) => u.pathname === '/mirror/terminal');
      expect(new URL(page.url()).searchParams.get('window')).toBe('@9');
      await expect(screen(page)).toContainText('FAKE-MIRROR session=scratch2 window=@9 mode=ro');
      await noHorizontalScroll(page);
    });

    test('a non-active window that is not Claude Code opens its own terminal, and Send reaches that window', async ({ page }) => {
      await openList(page, 4);
      await cardOf(page, termPath(shellWin)).locator('a').first().click();
      await page.waitForURL((u) => u.pathname === '/mirror/terminal');
      await expect(screen(page)).toContainText('FAKE-MIRROR session=scratch2 window=@4 mode=ro');
      await expect(page.locator('#term-status')).toHaveText('Connected · read-only');
      await expect(page.locator('header h1')).toContainText('Gold:1');
      await expect(page.locator('#view-rendered'), 'a shell has no rendered view').toHaveCount(0);

      await page.locator('#mv-input').fill('ls -la');
      await page.locator('#mv-send').click();
      await expect(page.locator('#mv-input')).toHaveValue('');
      await expect.poll(() => read(pasteFile), { message: `pasted into window @4's pane (log:\n${mirrorLog})` }).toBe('%e2e-scratch2-@4 ls -la\n');
      for (const t of [trEight, trEleven]) expect(read(t + '.pasted'), 'nothing reached a Claude window').toBe('');
      await noHorizontalScroll(page);
    });

    // The URLs that existed before windows were addressable.
    test('an old link naming only the session lands on its active window', async ({ page }) => {
      await login(page);
      const old = `host=${encodeURIComponent(host)}&session=scratch2`;
      await expect
        .poll(async () => (await page.request.get(origin + '/ui/agents')).text(), { timeout: 20_000 })
        .toContain(`Mirrored · ${host}`);
      await page.goto(origin + '/mirror/view?' + old);
      await page.waitForURL((u) => u.pathname === '/mirror/view' && u.searchParams.get('window') === '@7');
      await expect(page.locator('header h1')).toContainText('Gold:8');
      await expect(page.locator('#mv-log [data-bubble="user"]').first()).toContainText('the prompt of window EIGHT');
      await page.goto(origin + '/mirror/terminal?' + old);
      await page.waitForURL((u) => u.pathname === '/mirror/terminal' && u.searchParams.get('window') === '@7');
      await expect(screen(page)).toContainText('FAKE-MIRROR session=scratch2 window=@7 mode=ro');
    });
  });
}
