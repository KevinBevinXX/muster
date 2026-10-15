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
      await expect(page.locator('#term-status')).toHaveText('Connected');
      await expect(page.locator('header h1')).toContainText('Gold:1');
      await expect(page.locator('#view-rendered'), 'a shell has no rendered view').toHaveCount(0);

      await page.locator('#mv-input').fill('ls -la');
      await page.locator('#mv-send').click();
      await expect(page.locator('#mv-input')).toHaveValue('');
      await expect.poll(() => read(pasteFile), { message: `pasted into window @4's pane (log:\n${mirrorLog})` }).toBe('%e2e-scratch2-@4 ls -la\n');
      for (const t of [trEight, trEleven]) expect(read(t + '.pasted'), 'nothing reached a Claude window').toBe('');
      await noHorizontalScroll(page);
    });

    // THE LIST FILTER: a search box above the cards.
    //
    // 🔴 A REGRESSION TEST: there is no #agents-search on the commit before it.
    test('the filter narrows the cards as you type, ANDs its words, counts, clears — and survives the list refreshing itself', async ({ page }) => {
      const sockets: string[] = [];
      page.on('websocket', (ws) => sockets.push(ws.url()));
      await openList(page, 4);
      const box = page.locator('#agents-search');
      const count = page.locator('#agents-count');
      const visible = () => cards(page).filter({ visible: true });
      const names = async () =>
        (await visible().evaluateAll((els) => els.map((e) => ((e.querySelector('[data-mirror-label]') ?? e.querySelector('[data-mirror-name]')) as HTMLElement).textContent))).sort();

      // Phone-shaped: 16px (iOS zooms the page on focusing anything smaller), a
      // 44px target, inside the viewport.
      expect(await box.evaluate((e) => parseFloat(getComputedStyle(e).fontSize)), 'a 16px input').toBeGreaterThanOrEqual(16);
      const bb = (await box.boundingBox())!;
      expect(bb.height).toBeGreaterThanOrEqual(44);
      expect(bb.x + bb.width, 'the filter fits the viewport').toBeLessThanOrEqual(vp.use.viewport.width);
      await expect(page.locator('#agents-search-clear')).toBeHidden();

      // With nothing typed, everything is shown and counted. (Other cards — the
      // seeded agents — are in the list too: the count is of all of it.)
      const all = await page.locator('#agents-list > article').count();
      expect(all).toBeGreaterThanOrEqual(4);
      await expect(count).toHaveText(`${all} of ${all}`);

      // One word: a label, case-insensitively.
      await box.pressSequentially('GOLD');
      expect(await names()).toEqual(['Gold:1', 'Gold:11', 'Gold:8']);
      await expect(count).toHaveText(`3 of ${all}`);
      await expect(page.locator('#agents-search-clear')).toBeVisible();
      // Several words must ALL match: a label and a kind…
      await box.pressSequentially('   shell');
      expect(await names()).toEqual(['Gold:1']);
      await expect(count).toHaveText(`1 of ${all}`);
      // …the raw session name and a kind…
      await box.fill('scratch2 claude');
      expect(await names()).toEqual(['Gold:11', 'Gold:8']);
      // …the host and a kind (a window with no codename)…
      await box.fill(`${host} opencode`);
      expect(await names()).toEqual(['solo:3']);
      // …and "mirrored" finds every mirrored card of this host and no agent.
      await box.fill(`mirrored ${host}`);
      expect(await names()).toEqual(['Gold:1', 'Gold:11', 'Gold:8', 'solo:3']);
      await expect(count).toHaveText(`4 of ${all}`);
      // Nothing matches: said, not a blank list.
      await box.fill('gold opencode');
      await expect(visible()).toHaveCount(0);
      await expect(count).toHaveText(`0 of ${all}`);
      await expect(page.locator('#agents-nomatch')).toBeVisible();
      // A quote or a backslash in the box is text, not a broken selector.
      await box.fill('gold" \\');
      await expect(count).toHaveText(`0 of ${all}`);

      // Clear (×): everything back, the box empty and focused.
      await page.locator('#agents-search-clear').click();
      await expect(box).toHaveValue('');
      await expect(box).toBeFocused();
      await expect(count).toHaveText(`${all} of ${all}`);
      await expect(page.locator('#agents-nomatch')).toBeHidden();
      await expect(page.locator('#agents-search-clear')).toBeHidden();

      // 🔴 THE LIST REFRESHES ITSELF UNDER THE FILTER. Type, put the caret in the
      // middle of the word, and have the host change: a window is renumbered (a
      // card's text changes) and a new one appears. The filter still holds, the
      // count follows, and the box keeps its text, its focus and its caret.
      await box.fill('gold');
      await box.evaluate((e: HTMLInputElement) => e.setSelectionRange(2, 2));
      await expect(count).toHaveText(`3 of ${all}`);
      const marked = await page.evaluate(() => ((window as unknown as { __noReload: number }).__noReload = 1));
      expect(marked).toBe(1);
      const twelve: Win = { session: 'scratch2', id: '@12', index: 12, active: false, command: 'zsh', activity: now - 30 };
      writeWindows([shellWin, eightWin, { ...elevenWin, index: 2 }, soloWin, twelve]);
      await expect(cardOf(page, viewPath(elevenWin)).locator('[data-mirror-label]'), `the list refreshed (log:\n${mirrorLog})`).toHaveText('Gold:2', { timeout: 20_000 });
      await expect(cardOf(page, termPath(twelve))).toBeVisible({ timeout: 20_000 });
      expect(await names()).toEqual(['Gold:1', 'Gold:12', 'Gold:2', 'Gold:8']);
      await expect(cardOf(page, termPath(soloWin)), 'a card the filter hides stays hidden through the refresh').toBeHidden();
      await expect(count).toHaveText(`4 of ${all + 1}`);
      await expect(box).toHaveValue('gold');
      await expect(box).toBeFocused();
      expect(await box.evaluate((e: HTMLInputElement) => [e.selectionStart, e.selectionEnd]), 'the caret did not move').toEqual([2, 2]);
      expect(await page.evaluate(() => (window as unknown as { __noReload?: number }).__noReload), 'no reload happened').toBe(1);
      // Typing on, after the refresh, still works on the refreshed list.
      await page.keyboard.press('End');
      await page.keyboard.type(':12');
      expect(await names()).toEqual(['Gold:12']);

      // A hundred cards must not mean a hundred connections: the list page
      // opened no socket at all for its mirrored cards. (Read here, before a
      // card is opened: its page has a socket of its own.)
      expect(sockets.filter((u) => u.includes('/mirror/')), 'no per-card socket').toEqual([]);

      // It is remembered across a visit to a card and back.
      await visible().first().locator('a').first().click();
      await page.waitForURL((u) => u.pathname === '/mirror/terminal');
      await page.goto(origin + '/agents');
      await expect(page.locator('#agents-search')).toHaveValue('gold:12');
      await expect(cards(page).filter({ visible: true })).toHaveCount(1);
      await page.locator('#agents-search-clear').click();

      await noHorizontalScroll(page);
    });

    // FIELD TOKENS: `host:cluster` asks about the host alone; a free word asks
    // about everything a card says. A session named `cluster-notes` is the trap:
    // the free word "cluster" finds it, the field token must not.
    //
    // 🔴 A REGRESSION TEST: on the commit before field tokens `host:cluster` is
    // one free word no card's text contains, so it hides every card — the pod
    // agents included — and `?q=` is never read.
    test('host:<x> matches the host field only, and survives the list refreshing itself', async ({ page }) => {
      const trap: Win = { session: 'cluster-notes', id: '@3', index: 1, active: true, command: 'zsh', activity: now - 60 };
      writeWindows([shellWin, eightWin, elevenWin, soloWin, trap]);
      await openList(page, 5);
      const box = page.locator('#agents-search');
      const count = page.locator('#agents-count');
      const trapCard = cardOf(page, termPath(trap));
      const pods = page.locator('#agents-list > article:not([data-mirror-card])');
      const names = async () =>
        (await cards(page).filter({ visible: true }).evaluateAll((els) => els.map((e) => ((e.querySelector('[data-mirror-label]') ?? e.querySelector('[data-mirror-name]')) as HTMLElement).textContent))).sort();
      const all = await page.locator('#agents-list > article').count();
      const podCount = await pods.count();
      expect(podCount, 'the seeded pod agents are listed').toBeGreaterThanOrEqual(3);
      const qOf = () => new URL(page.url()).searchParams.get('q');
      const historyBefore = await page.evaluate(() => history.length);

      // The control: the FREE word "cluster" does find the trap (it is in its title).
      await box.fill('cluster');
      await expect(trapCard, 'the free word "cluster" finds the cluster-notes window').toBeVisible();

      // The token: every pod agent, and no mirrored window — not even the one
      // whose title spells "cluster".
      await box.fill('host:cluster');
      await expect(trapCard, 'LEAK: the word "cluster" in a mirrored title let it into host:cluster').toBeHidden();
      await expect(page.locator('#agents-list > article[data-mirror-card]').filter({ visible: true }), 'no mirrored window is on host "cluster"').toHaveCount(0);
      await expect(pods.filter({ visible: true }), 'every pod agent is on host "cluster"').toHaveCount(podCount);
      await expect(count).toHaveText(`${podCount} of ${all}`);
      expect(qOf(), 'the filter is in the URL').toBe('host:cluster');

      // This host: its five windows, and no pod agent.
      await box.fill(`host:${host}`);
      expect(await names()).toEqual(['Gold:1', 'Gold:11', 'Gold:8', 'cluster-notes:1', 'solo:3']);
      await expect(pods.filter({ visible: true })).toHaveCount(0);
      // Fields AND with each other and with free words; a quoted value may hold a space.
      await box.fill(`host:${host} kind:opencode`);
      expect(await names()).toEqual(['solo:3']);
      await box.fill(`HOST:${host.toUpperCase()} kind:"claude code"`);
      expect(await names()).toEqual(['Gold:11', 'Gold:8']);
      await box.fill(`session:scratch2 shell`);
      expect(await names()).toEqual(['Gold:1']);
      // An unknown field is free text: "gold:8" is a title, not a field.
      await box.fill('gold:8');
      expect(await names()).toEqual(['Gold:8']);
      // A quote or a backslash in a field value is text, not a broken selector.
      await box.fill('host:"a\\"b');
      await expect(count).toHaveText(`0 of ${all}`);
      expect(await page.evaluate(() => history.length), 'typing replaced the URL, it did not push history').toBe(historyBefore);

      // 🔴 THE LIST REFRESHES ITSELF UNDER A FIELD TOKEN: the trap window is
      // renumbered (its card is re-rendered) and a new pod-less window appears.
      // The hidden stay hidden, the count follows, nothing reloads.
      await box.fill('host:cluster');
      await expect(count).toHaveText(`${podCount} of ${all}`);
      await page.evaluate(() => ((window as unknown as { __noReload: number }).__noReload = 1));
      const extra: Win = { session: 'cluster-two', id: '@5', index: 4, active: true, command: 'zsh', activity: now - 30 };
      writeWindows([shellWin, eightWin, elevenWin, soloWin, { ...trap, index: 2 }, extra]);
      await expect(trapCard.locator('[data-mirror-name]'), `the list refreshed (log:\n${mirrorLog})`).toHaveText('cluster-notes:2', { timeout: 20_000 });
      await expect(page.locator(`#agents-list article[data-mirror-card]:has(a[href="${termPath(extra)}"])`)).toHaveCount(1, { timeout: 20_000 });
      await expect(trapCard, 'a refreshed card the token hides stays hidden').toBeHidden();
      await expect(page.locator('#agents-list > article[data-mirror-card]').filter({ visible: true })).toHaveCount(0);
      await expect(pods.filter({ visible: true })).toHaveCount(podCount);
      await expect(count).toHaveText(`${podCount} of ${all + 1}`);
      expect(await page.evaluate(() => (window as unknown as { __noReload?: number }).__noReload), 'no reload happened').toBe(1);

      await noHorizontalScroll(page);
    });

    // 🔴 A REGRESSION TEST: on the commit before, the filter lived in
    // sessionStorage only — a shared link carried nothing, and the box came back
    // with whatever this tab last typed.
    test('a shared ?q= link reproduces the filter, wins over the remembered one, and clearing removes it', async ({ page }) => {
      await openList(page, 4);
      const box = page.locator('#agents-search');
      const qOf = () => new URL(page.url()).searchParams.get('q');
      const names = async () =>
        (await cards(page).filter({ visible: true }).evaluateAll((els) => els.map((e) => ((e.querySelector('[data-mirror-label]') ?? e.querySelector('[data-mirror-name]')) as HTMLElement).textContent))).sort();
      // This tab remembers a filter of its own…
      await box.fill('gold');
      await expect(cards(page).filter({ visible: true })).toHaveCount(3);
      // …and a link with another one wins over it, on first paint.
      await page.goto(origin + '/agents?q=' + encodeURIComponent(`host:${host} kind:opencode`));
      await expect(box).toHaveValue(`host:${host} kind:opencode`);
      await expect(cards(page).filter({ visible: true })).toHaveCount(1, { timeout: 20_000 });
      expect(await names()).toEqual(['solo:3']);
      // Clearing it takes it out of the URL.
      await page.locator('#agents-search-clear').click();
      expect(qOf(), 'an empty filter leaves no ?q=').toBeNull();
      // Typing writes it back, and a reload of that URL is the same list.
      await box.fill('solo');
      expect(qOf()).toBe('solo');
      await page.reload();
      await expect(box).toHaveValue('solo');
      await expect.poll(names, { message: 'the reloaded list is filtered', timeout: 20_000 }).toEqual(['solo:3']);

      // 🔴 LEAVING THE TAB AND COMING BACK KEEPS ?q=. The tab router pushes a
      // bare /<tab>; the URL must carry the filter the moment the Agents panel is
      // back — read right after the click, before any list poll could heal it —
      // and no other tab may carry it.
      const tab = async (t: string) => {
        const open = page.locator('#sidebar-open');
        if (await open.isVisible()) await open.click();
        await page.locator(`#sidebar a[data-tab="${t}"]`).click();
        await expect(page.locator(`#sidebar a[data-tab="${t}"][aria-current="page"]`)).toHaveCount(1);
      };
      const hist = await page.evaluate(() => history.length);
      await tab('tasks');
      expect(new URL(page.url()).pathname).toBe('/tasks');
      expect(qOf(), 'the Tasks tab carries no ?q=').toBeNull();
      await tab('agents');
      const back = new URL(page.url());
      expect(back.pathname + back.search, 'back on Agents, the URL carries the filter at once').toBe('/agents?q=solo');
      expect(await page.evaluate(() => history.length), 'two tab switches, two history entries').toBe(hist + 2);
      await expect(box).toHaveValue('solo');
      await noHorizontalScroll(page);
    });

    // THE COMPOSER WITHOUT ITS SCRIPT. A browser context with JavaScript OFF: the
    // page's composer script does not exist, so what is left is the form itself.
    //
    // 🔴 A REGRESSION TEST: on the commit before this the composer was a form with
    // no method and no action — a browser submitting it GETs the page it is on
    // with `?text=…`, and nothing is sent.
    test('with no script at all, a message is typed, submitted by the browser, delivered, and the page comes back saying so', async ({ page, browser }) => {
      await login(page);
      await expect.poll(async () => (await page.request.get(origin + '/ui/agents')).text(), { timeout: 20_000 }).toContain(`Mirrored · ${host}`);
      const ctx = await browser.newContext({ ...vp.use, javaScriptEnabled: false });
      await ctx.addCookies(signedIn!);
      const plain = await ctx.newPage();
      try {
        // The Terminal view of a shell window (not its session's active one).
        await plain.goto(origin + termPath(shellWin));
        // The control that this page ran no script: the page's own script is
        // what builds the terminal, and there is none. (page.evaluate is no
        // witness — it runs in a context with script disabled all the same.)
        await expect(plain.locator('#mv-input')).toBeVisible();
        await expect(plain.locator('#term .xterm-rows'), 'this page ran no script').toHaveCount(0);
        await plain.locator('#mv-input').fill('ls -la /srv');
        await plain.locator('#mv-send').click();
        await plain.waitForURL((u) => u.pathname === '/mirror/terminal' && u.searchParams.get('notice') === 'sent', { timeout: 20_000 });
        expect(new URL(plain.url()).searchParams.get('window'), 'back on the same window').toBe('@4');
        await expect(plain.locator('#mv-note')).toBeVisible();
        await expect(plain.locator('#mv-note')).toHaveText('Sent.');
        expect(read(pasteFile)).toBe('%e2e-scratch2-@4 ls -la /srv\n');

        // The Rendered view of a Claude Code window: the same form, the Claude pane.
        await plain.goto(origin + viewPath(elevenWin));
        await plain.locator('#mv-input').fill('continue, please');
        await plain.locator('#mv-send').click();
        await plain.waitForURL((u) => u.pathname === '/mirror/view' && u.searchParams.get('notice') === 'sent', { timeout: 20_000 });
        await expect(plain.locator('#mv-note')).toHaveText('Sent.');
        expect(read(trEleven + '.pasted')).toBe('%e2e-scratch2-@9 continue, please\n');
        expect(read(trEight + '.pasted'), 'nothing reached the active window').toBe('');

        // Nothing typed: said on the page it comes back to, and nothing is sent.
        await plain.locator('#mv-send').click();
        await plain.waitForURL((u) => u.searchParams.get('notice') === 'empty');
        await expect(plain.locator('#mv-note')).toHaveText('Type a message first.');
        expect(read(trEleven + '.pasted')).toBe('%e2e-scratch2-@9 continue, please\n');
        expect(read(pasteFile)).toBe('%e2e-scratch2-@4 ls -la /srv\n');
      } finally {
        await ctx.close();
      }
    });

    // THE COMPOSER WHEN ITS SCRIPT'S WAY FAILS. Send must never be a silent
    // no-op: when the fetch gets no answer the browser submits the form itself.
    test('a send whose request gets no answer is handed to the browser, and a message is never delivered twice', async ({ page }) => {
      await openList(page, 4);
      await page.goto(origin + termPath(shellWin));
      await expect(screen(page)).toContainText('FAKE-MIRROR session=scratch2 window=@4');
      const isFetch = (h: Record<string, string>) => h['x-requested-with'] === 'XMLHttpRequest';

      // 1. The request never reaches muster (a sign-in redirect a fetch cannot
      // follow, a proxy's error): the script's fetch is aborted, the browser's
      // own submission is let through. Delivered once, and the page says so.
      let aborted = 0;
      await page.route('**/mirror/terminal/send?*', async (route) => {
        if (isFetch(route.request().headers())) {
          aborted++;
          await route.abort('failed');
          return;
        }
        await route.continue();
      });
      await page.locator('#mv-input').fill('echo first');
      await page.locator('#mv-send').click();
      await page.waitForURL((u) => u.searchParams.get('notice') === 'sent');
      await expect(page.locator('#mv-note')).toHaveText('Sent.');
      expect(aborted, 'the script tried its own way first').toBe(1);
      expect(read(pasteFile)).toBe('%e2e-scratch2-@4 echo first\n');
      // The notice is taken out of the address once shown: a reload does not repeat it.
      await expect.poll(() => new URL(page.url()).searchParams.get('notice')).toBeNull();
      await page.unroute('**/mirror/terminal/send?*');

      // 2. The request REACHES muster and is delivered, and only the answer is
      // lost. The browser posts the form again — the same message — and it must
      // not be pasted a second time.
      let delivered = 0;
      await page.route('**/mirror/terminal/send?*', async (route) => {
        if (isFetch(route.request().headers())) {
          const res = await route.fetch();
          expect(res.status(), 'the first post was delivered').toBe(204);
          delivered++;
          await route.abort('failed'); // …and the page never hears
          return;
        }
        await route.continue();
      });
      await page.locator('#mv-input').fill('echo second');
      await page.locator('#mv-send').click();
      await page.waitForURL((u) => u.searchParams.get('notice') === 'sent');
      expect(delivered).toBe(1);
      expect(read(pasteFile), 'the message whose answer was lost was delivered ONCE').toBe('%e2e-scratch2-@4 echo first\n%e2e-scratch2-@4 echo second\n');
      await page.unroute('**/mirror/terminal/send?*');

      // 3. Control: with nothing in the way the script sends without leaving the
      // page, and the NEXT message is a new message (its nonce was renewed).
      const before = page.url();
      await page.locator('#mv-input').fill('echo third');
      await page.locator('#mv-send').click();
      await expect(page.locator('#mv-input')).toHaveValue('');
      await page.locator('#mv-input').fill('echo third');
      await page.locator('#mv-send').click();
      await expect.poll(() => read(pasteFile)).toBe(
        '%e2e-scratch2-@4 echo first\n%e2e-scratch2-@4 echo second\n%e2e-scratch2-@4 echo third\n%e2e-scratch2-@4 echo third\n',
      );
      // The send's answer empties the box; typing before it has would be wiped.
      await expect(page.locator('#mv-input')).toHaveValue('');
      expect(page.url(), 'the script sends without navigating').toBe(before);

      // 3b. Text typed while a send is still out is not thrown away when its
      // answer arrives.
      let release: () => void = () => {};
      const held = new Promise<void>((r) => (release = r));
      await page.route('**/mirror/terminal/send?*', async (route) => {
        await held;
        await route.continue();
      });
      await page.locator('#mv-input').fill('echo fourth');
      await page.locator('#mv-send').click();
      await expect(page.locator('#mv-send')).toBeDisabled();
      await page.locator('#mv-input').fill('typed meanwhile');
      release();
      await expect(page.locator('#mv-send')).toBeEnabled();
      expect(read(pasteFile)).toContain('%e2e-scratch2-@4 echo fourth\n');
      await expect(page.locator('#mv-input')).toHaveValue('typed meanwhile');
      await page.unroute('**/mirror/terminal/send?*');

      // 4. Offline is the one failure the script keeps: it says so and keeps the text.
      await page.context().setOffline(true);
      await page.locator('#mv-input').fill('while offline');
      await page.locator('#mv-send').click();
      await expect(page.locator('#mv-note')).toHaveText('Not sent: you are offline.');
      await expect(page.locator('#mv-input')).toHaveValue('while offline');
      expect(page.url()).toBe(before);
      await page.context().setOffline(false);
    });

    // THE UPDATE TOAST AND THE COMPOSER. "muster has been updated — Reload" is a
    // fixed overlay near the bottom of every page; it must never sit on the
    // message box or on Send.
    test('the update toast does not cover the composer', async ({ page }) => {
      await openList(page, 4);
      for (const p of [termPath(shellWin), viewPath(elevenWin)]) {
        await page.goto(origin + p);
        await expect(page.locator('#mv-send')).toBeVisible();
        const boxes = await page.evaluate(() => {
          const toast = document.getElementById('sw-update-toast')!;
          toast.hidden = false;
          const r = (e: Element) => e.getBoundingClientRect();
          const t = r(toast);
          const over = (e: Element) => {
            const b = r(e);
            return !(t.right <= b.left || t.left >= b.right || t.bottom <= b.top || t.top >= b.bottom);
          };
          return { height: t.height, input: over(document.getElementById('mv-input')!), send: over(document.getElementById('mv-send')!) };
        });
        expect(boxes.height, 'CONTROL: the toast was really laid out').toBeGreaterThan(20);
        expect(boxes, `on ${p}`).toMatchObject({ input: false, send: false });
      }
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
