import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import crypto from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { binDir, composerMenuPort, dsn, password, repoRoot, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { clsInit, readCls, settle } from '../lib/cls';
import { writeFileAtomic } from '../lib/atomic';

// THE RENDERED VIEW COMPOSER'S SPLIT SEND/QUEUE BUTTON AND ITS `/` AND `:` MENU,
// in a real browser, against a real muster-server (MUSTER_MIRROR=1, password
// mode, its own port) and two real muster-mirror processes over the tunnel
// (the `-tags e2emirror` per-window fake: only tmux is played). The host's
// CATALOG is read by the real scanner from fixture directories this spec
// writes — CLAUDE_CONFIG_DIR (the user's skills and commands, an enabled
// plugin), the window's PROJECT (MUSTER_MIRROR_E2E_CWD, a repository with its
// own .claude) and MUSTER_MIRROR_SNIPPETS_FILE. The second host plays an older
// muster-mirror that reports no catalog (MUSTER_MIRROR_E2E_NO_CATALOG=1).
//
// The window's Claude Code reports BUSY the whole time, so a queued message is
// held (never sent) and a Send is the only thing that reaches the pane: what
// the pane received (<transcript>.pasted) tells a Send from a Queue.
//
// STEP-UP without waiting out the window: the password-mode session cookie is
// re-signed with the issue time 13 hours back — byte for byte what the server
// mints for a sign-in that old (internal/auth.SignSession over the key the
// server derives from MUSTER_UI_PASSWORD) — so the server's REAL gate refuses,
// and the page's real confirm dialog takes the password again.

const origin = `http://localhost:${composerMenuPort}`;
const stamp = Date.now().toString(36);
const newHost = 'e2e-cm-' + stamp;
const oldHost = 'e2e-cmold-' + stamp;
const mirrorBin = path.join(binDir, 'muster-mirror-e2e');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'composer-menu-'));
const tr = path.join(dir, 'eeeeeeee-0001.jsonl');
const trOld = path.join(dir, 'ffffffff-0002.jsonl');
const pane = '%e2e-work-@1';
const cfg = path.join(dir, 'claude-config');
const proj = path.join(dir, 'repo');
const snippetsFile = path.join(dir, 'snippets.json');
const EVIL = `<img src=x onerror="window.__pwned=1">`;
let server: Running | undefined;
const procs: ChildProcess[] = [];
let mirrorLog = '';

const w = (p: string, body: string) => {
  fs.mkdirSync(path.dirname(p), { recursive: true });
  fs.writeFileSync(p, body);
};
const skillMd = (name: string, desc: string) => `---\nname: ${name}\ndescription: ${JSON.stringify(desc)}\n---\n\nbody\n`;
const prompt = (text: string) => JSON.stringify({ type: 'user', promptId: 'p-' + text.length, message: { role: 'user', content: text } }) + '\n';
const read = (f: string) => (fs.existsSync(f) ? fs.readFileSync(f, 'utf8') : '');
const pasted = () => read(tr + '.pasted').split('\n').filter((l) => l !== '');
const viewPath = (host: string) => `/mirror/view?host=${encodeURIComponent(host)}&session=work&window=%401`;
const setBusy = (f: string) => writeFileAtomic(f + '.status', JSON.stringify({ status: 'busy', statusUpdatedAt: Date.now() }));

function fixtures() {
  w(path.join(cfg, 'skills', 'handoff', 'SKILL.md'), skillMd('handoff', 'Write a canonical session-handoff doc'));
  w(path.join(cfg, 'skills', 'evil-skill', 'SKILL.md'), skillMd('evil-skill', EVIL));
  w(path.join(cfg, 'commands', 'deploy-it.md'), '---\ndescription: Ship the build\n---\n');
  const plug = path.join(cfg, 'plugins', 'cache', 'mkt', 'stripe', '1.0.0');
  w(path.join(plug, 'skills', 'test-cards', 'SKILL.md'), skillMd('test-cards', 'Display Stripe test card numbers'));
  w(path.join(cfg, 'plugins', 'installed_plugins.json'), JSON.stringify({ version: 2, plugins: { 'stripe@mkt': [{ scope: 'user', installPath: plug }] } }));
  w(path.join(cfg, 'settings.json'), JSON.stringify({ enabledPlugins: { 'stripe@mkt': true } }));
  w(path.join(proj, '.git', 'HEAD'), 'ref: refs/heads/main\n');
  w(path.join(proj, '.claude', 'commands', 'proj-build.md'), '---\ndescription: Build this project\n---\n');
  w(snippetsFile, JSON.stringify([
    { trigger: ':eos', label: 'End of session ritual', body: 'reflect on work done, then write the handoff FIRST' },
    { trigger: ':kickoff', label: 'Kickoff message for next session', body: 'give the kickoff message for next session' },
    { trigger: ':mt', label: 'Meantime: tee up independent parallel work', body: 'tee up what we can do in the meantime' },
    // Two the measured misfires reached through the old rules: ":D" (letters in
    // order / label) and ":P" (":par").
    { trigger: ':dacq', label: 'Process feedback: dispatch subagent + elicit scope', body: 'do light recon and ask clarifying questions' },
    { trigger: ':par', label: 'Proceed as recommended', body: 'proceed as recommended' },
  ]));
}

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
      MUSTER_MIRROR_E2E_REFRESH: '250ms',
      MUSTER_MIRROR_E2E_CWD: proj,
      CLAUDE_CONFIG_DIR: cfg,
      MUSTER_MIRROR_SNIPPETS_FILE: snippetsFile,
      ...extra,
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  p.stdout?.on('data', (d) => (mirrorLog += d));
  p.stderr?.on('data', (d) => (mirrorLog += d));
  procs.push(p);
}

// Every context starts signed OUT: this server's own sign-in is what makes a
// session fresh (and what makeStale replaces).
test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  fixtures();
  execFileSync('go', ['build', '-tags', 'e2emirror', '-o', mirrorBin, './cmd/muster-mirror'], { cwd: repoRoot, stdio: 'inherit' });
  server = await startServer(versionTerm, composerMenuPort, {
    MUSTER_MIRROR: '1',
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
  });
  fs.writeFileSync(tr, prompt('the conversation so far'));
  fs.writeFileSync(trOld, prompt('an older host'));
  setBusy(tr);
  setBusy(trOld);
  startMirror(newHost, tr, {});
  startMirror(oldHost, trOld, { MUSTER_MIRROR_E2E_NO_CATALOG: '1' });
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
// throttled); a password sign-in is a FRESH one.
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

// makeStale replaces the session cookie with one the server signed 13 hours
// ago (internal/auth.SignSession: "<issued>|<expiry>|hex(hmac)", keyed by
// sha256("muster-ui-session-v1\0" + password)).
async function makeStale(context: BrowserContext) {
  const now = Math.floor(Date.now() / 1000);
  const payload = `${now - 13 * 3600}|${now + 3600}`;
  const key = crypto.createHash('sha256').update('muster-ui-session-v1\0' + password).digest();
  const sig = crypto.createHmac('sha256', key).update(payload).digest('hex');
  await context.addCookies([{ name: 'muster_session', value: `${payload}|${sig}`, url: origin }]);
}

async function openView(page: Page, host = newHost) {
  await login(page);
  await page.goto(origin + viewPath(host));
  await expect(page.locator('#mv-input'), `the composer is enabled (muster-mirror log:\n${mirrorLog})`).toBeEnabled({ timeout: 20_000 });
}

const input = (page: Page) => page.locator('#mv-input');
const menu = (page: Page) => page.locator('#mv-complete-box');
const options = (page: Page) => page.locator('#mv-complete [role=option]');
const optionNames = (page: Page) => options(page).evaluateAll((els) => els.map((e) => e.getAttribute('data-name') || e.getAttribute('data-trigger')));
const queued = (page: Page) => page.locator('#mv-queue [data-queue-state="queued"]');

// cancelQueued cancels every queued message on the page, so the next test
// starts from an empty queue.
async function cancelQueued(page: Page) {
  for (let i = 0; i < 10 && (await queued(page).count()) > 0; i++) {
    // Cancel is in the row's ⋯ menu.
    await queued(page).first().getByRole('button', { name: 'More actions for this queued message' }).click();
    await queued(page).first().getByRole('menuitem', { name: 'Cancel this queued message' }).click();
    await page.waitForTimeout(300);
  }
  await expect(queued(page)).toHaveCount(0, { timeout: 10_000 });
}

test.describe('desktop 1280×800', () => {
  test.use({ viewport: { width: 1280, height: 800 } });

  test('Send is the face: a click, Enter and Ctrl+Enter each send now, and nothing is queued', async ({ page }) => {
    await openView(page);
    const before = pasted().length;
    await expect(page.locator('#mv-send')).toHaveText('Send');
    await input(page).fill('one by click');
    await page.locator('#mv-send').click();
    await expect.poll(pasted).toEqual(expect.arrayContaining([`${pane} one by click`]));
    await input(page).fill('two by enter');
    await input(page).press('Enter');
    await expect.poll(() => pasted().length).toBe(before + 2);
    await input(page).fill('three by ctrl+enter');
    await input(page).press('Control+Enter');
    await expect.poll(() => pasted().slice(before)).toEqual([`${pane} one by click`, `${pane} two by enter`, `${pane} three by ctrl+enter`]);
    await expect(queued(page)).toHaveCount(0);
    await expect(page.locator('#mv-send-menu')).toBeHidden();
  });

  test('Queue is behind the caret, by pointer and by keyboard; it queues and never sends', async ({ page }) => {
    await openView(page);
    const before = pasted();
    const more = page.locator('#mv-send-more');
    const item = page.getByRole('menuitem', { name: 'Queue: send when Claude Code is idle' });
    await expect(more).toHaveAttribute('aria-expanded', 'false');
    await expect(item).toBeHidden();

    // Pointer: the caret opens the menu, the item queues.
    await input(page).fill('queued by pointer');
    await more.click();
    await expect(item).toBeVisible();
    await expect(item).toHaveText('Queue for when idle');
    await expect(more).toHaveAttribute('aria-expanded', 'true');
    await expect(item, 'opening the menu puts focus in it').toBeFocused();
    // The menu sits above the caret, inside the viewport.
    const mb = (await page.locator('#mv-send-menu').boundingBox())!;
    const cb = (await more.boundingBox())!;
    expect(mb.y + mb.height, 'the menu ends above the caret').toBeLessThanOrEqual(cb.y + 1);
    expect(mb.x + mb.width).toBeLessThanOrEqual(1280);
    await item.click();
    await expect(page.locator('#mv-note')).toHaveText('Queued.');
    await expect(queued(page)).toHaveCount(1);
    await expect(page.locator('#mv-send-menu')).toBeHidden();
    await expect(more).toHaveAttribute('aria-expanded', 'false');

    // Keyboard: ArrowDown opens with the item focused; Escape closes it and
    // gives focus back to the caret; Enter opens it again and Enter on the
    // item queues.
    await input(page).fill('queued by keyboard');
    await more.focus();
    await page.keyboard.press('ArrowDown');
    await expect(item).toBeFocused();
    await page.keyboard.press('Escape');
    await expect(item).toBeHidden();
    await expect(more).toBeFocused();
    await expect(more).toHaveAttribute('aria-expanded', 'false');
    await page.keyboard.press('Enter');
    await expect(item).toBeFocused();
    await page.keyboard.press('Enter');
    await expect(queued(page)).toHaveCount(2);

    expect(pasted(), 'a queue sends nothing while Claude Code is busy').toEqual(before);
    await cancelQueued(page);
  });

  test('/ lists the window’s commands and skills — user, project, plugin and built-in — ranked, and inserts one; the slash command then sends', async ({ page }) => {
    await openView(page);
    const sends: number[] = [];
    page.on('response', (r) => {
      if (new URL(r.url()).pathname === '/mirror/view/send') sends.push(r.status());
    });
    await input(page).click();
    await page.keyboard.type('/');
    await expect(menu(page)).toBeVisible();
    await expect(page.locator('#mv-complete')).toHaveAttribute('role', 'listbox');
    const all = await optionNames(page);
    for (const n of ['handoff', 'deploy-it', 'proj-build', 'stripe:test-cards', 'compact', 'clear']) {
      expect(all, `/${n} is offered`).toContain(n);
    }
    await expect(page.locator('#mv-complete [data-name="proj-build"]')).toContainText('project');
    await expect(page.locator('#mv-complete [data-name="proj-build"]')).toContainText('Build this project');
    // The first option is active and the input says so.
    await expect(input(page)).toHaveAttribute('aria-activedescendant', 'mv-opt-0');

    // Ranked by the shared rule: prefix first; a letters-in-order match is
    // still found.
    await page.keyboard.type('comp');
    await expect.poll(() => optionNames(page)).toEqual(['compact']);
    await page.keyboard.press('Backspace');
    await page.keyboard.press('Backspace');
    await page.keyboard.press('Backspace');
    await page.keyboard.press('Backspace');
    await page.keyboard.type('hdf');
    await expect.poll(() => optionNames(page)).toEqual(['handoff']);
    // Moved to with an arrow, an item is taken by Enter even though "hdf" is
    // not a prefix of it (only typing that is not a choice is sent).
    await page.keyboard.press('ArrowDown');
    await page.keyboard.press('Enter');
    await expect(input(page)).toHaveValue('/handoff ');

    // Arrows move the active option; Enter inserts "/name " and sends nothing.
    await input(page).fill('');
    await page.keyboard.type('/co');
    const names = await optionNames(page);
    expect(names.length).toBeGreaterThan(1);
    await page.keyboard.press('ArrowDown');
    await expect(input(page)).toHaveAttribute('aria-activedescendant', 'mv-opt-1');
    await expect(page.locator('#mv-opt-1')).toHaveAttribute('aria-selected', 'true');
    await page.keyboard.press('ArrowUp');
    await page.keyboard.press('ArrowUp');
    await expect(input(page), 'ArrowUp from the first wraps to the last').toHaveAttribute('aria-activedescendant', `mv-opt-${names.length - 1}`);
    await page.keyboard.press('ArrowDown');
    await page.keyboard.press('Enter');
    await expect(input(page)).toHaveValue(`/${names[0]} `);
    await expect(menu(page)).toBeHidden();
    expect(sends, 'picking a command sends nothing').toEqual([]);

    // Tab inserts too.
    await input(page).fill('');
    await page.keyboard.type('/hand');
    await page.keyboard.press('Tab');
    await expect(input(page)).toHaveValue('/handoff ');
    await expect(input(page)).toBeFocused();

    // …and the slash command, completed, is SENT as typed into the pane.
    await page.keyboard.type('now please');
    await page.keyboard.press('Enter');
    await expect.poll(pasted).toEqual(expect.arrayContaining([`${pane} /handoff now please`]));
    await expect.poll(() => sends).toEqual([204]);
  });

  test(': lists the snippets and inserts the text, editable before it is sent; a click inserts too', async ({ page }) => {
    await openView(page);
    await input(page).click();
    await page.keyboard.type('please :');
    await expect(menu(page)).toBeVisible();
    await expect.poll(() => optionNames(page)).toEqual([':eos', ':kickoff', ':mt', ':dacq', ':par']);
    // By the trigger's prefix alone — a label word ("meantime") is not a match.
    await page.keyboard.type('meant');
    await expect(menu(page), 'a label word opens nothing').toBeHidden();
    for (let i = 0; i < 5; i++) await page.keyboard.press('Backspace');
    await page.keyboard.type('mt');
    await expect.poll(() => optionNames(page)).toEqual([':mt']);
    await page.keyboard.press('Enter');
    await expect(input(page)).toHaveValue('please tee up what we can do in the meantime');
    await expect(menu(page)).toBeHidden();
    // Editable: the text is the box's, typed onto like any other.
    await page.keyboard.type(', then report');
    await expect(input(page)).toHaveValue('please tee up what we can do in the meantime, then report');

    await input(page).fill('');
    await page.keyboard.type(':k');
    await page.locator('#mv-complete [data-trigger=":kickoff"]').click();
    await expect(input(page)).toHaveValue('give the kickoff message for next session');
    await expect(input(page)).toBeFocused();
  });

  test('Enter sends ordinary text the menu could have rewritten — "thanks :D", "ok :P", "ok :p", "see /etc" — and completes only a typed prefix', async ({ page }) => {
    await openView(page);
    const sends: number[] = [];
    page.on('response', (r) => {
      if (new URL(r.url()).pathname === '/mirror/view/send') sends.push(r.status());
    });
    const before = pasted();
    const sent: string[] = [];
    for (const text of ['thanks :D', 'ok :P', 'ok :p', 'see /etc', 'see /tmp']) {
      await input(page).fill('');
      await input(page).click();
      await page.keyboard.type(text);
      // Nothing is offered for a face or a path; ":p" may offer ":par", and
      // Enter must still send it.
      if (text !== 'ok :p') await expect(menu(page), `"${text}" offers nothing`).toBeHidden();
      await page.keyboard.press('Enter');
      sent.push(`${pane} ${text}`);
      await expect.poll(pasted, `"${text}" + Enter is sent as typed`).toEqual([...before, ...sent]);
      await expect(input(page)).toHaveValue('');
    }
    // The positive control: a typed PREFIX of the highlighted item is completed.
    await page.keyboard.type('/hand');
    await page.keyboard.press('Enter');
    await expect(input(page)).toHaveValue('/handoff ');
    await input(page).fill('');
    await page.keyboard.type('note :ki');
    await expect.poll(() => optionNames(page)).toEqual([':kickoff']);
    await page.keyboard.press('Enter');
    await expect(input(page)).toHaveValue('note give the kickoff message for next session');
    expect(pasted(), 'no completion was sent').toEqual([...before, ...sent]);
    // "/etc" offers no letters-in-order match at all.
    await input(page).fill('');
    await page.keyboard.type('see /etc');
    await expect(menu(page)).toBeHidden();
  });

  test('an inserted command or snippet is undone by Ctrl+Z', async ({ page }) => {
    await openView(page);
    await input(page).click();
    await page.keyboard.type('/hand');
    await expect(menu(page)).toBeVisible();
    await page.keyboard.press('Tab');
    await expect(input(page)).toHaveValue('/handoff ');
    await page.keyboard.press('Control+z');
    await expect(input(page)).toHaveValue('/hand');
    await input(page).fill('');
    await page.keyboard.type('do :eo');
    await page.keyboard.press('Enter');
    await expect(input(page)).toHaveValue('do reflect on work done, then write the handoff FIRST');
    await page.keyboard.press('Control+z');
    await expect(input(page)).toHaveValue('do :eo');
  });

  test('/ in the middle of a word or a path does not open the menu; Escape closes it and it stays closed for that word', async ({ page }) => {
    await openView(page);
    await input(page).click();
    for (const text of ['hello/co', 'see https://x.com/co', '/usr/', 'time:5']) {
      await input(page).fill('');
      await page.keyboard.type(text);
      await expect(menu(page), `"${text}" opens nothing`).toBeHidden();
    }
    // A `/` typed INTO a word.
    await input(page).fill('');
    await page.keyboard.type('ab');
    await page.keyboard.press('ArrowLeft');
    await page.keyboard.type('/');
    await expect(input(page)).toHaveValue('a/b');
    await expect(menu(page)).toBeHidden();
    // A `/` typed at the START of an existing word: the caret is not at the
    // end of the word it begins, so it is not a command being typed.
    await input(page).fill('');
    await page.keyboard.type('compact');
    await page.keyboard.press('Home');
    await page.keyboard.type('/');
    await expect(input(page)).toHaveValue('/compact');
    await expect(menu(page)).toBeHidden();
    // After white space it opens (Claude Code's own rule).
    await input(page).fill('');
    await page.keyboard.type('hello /co');
    await expect(menu(page)).toBeVisible();
    // Escape closes it, leaves the text, and typing on does not reopen it.
    await page.keyboard.press('Escape');
    await expect(menu(page)).toBeHidden();
    await expect(input(page)).toHaveValue('hello /co');
    await expect(input(page)).not.toHaveAttribute('aria-activedescendant', /.*/);
    await page.keyboard.type('m');
    await expect(menu(page)).toBeHidden();
    // A new trigger word opens it again; Enter with the menu closed still sends.
    await page.keyboard.type(' /cl');
    await expect(menu(page)).toBeVisible();
  });

  test('a host’s description is drawn as text, and this document ranks by internal/searchrank’s table', async ({ page }) => {
    await openView(page);
    await input(page).click();
    await page.keyboard.type('/evil');
    const opt = page.locator('#mv-complete [data-name="evil-skill"]');
    await expect(opt).toContainText(EVIL);
    await expect(opt.locator('img')).toHaveCount(0);
    expect(await page.evaluate(() => (window as any).__pwned)).toBeUndefined();

    const table = JSON.parse(fs.readFileSync(path.join(repoRoot, 'internal/searchrank/testdata/cases.json'), 'utf8')) as { cases: { label: string; q: string; tier: number; why: string }[] };
    expect(table.cases.length, 'the table has rows').toBeGreaterThan(10);
    const got = await page.evaluate((cases) => cases.map((c) => (window as any).musterRank(c.label, c.q)), table.cases);
    const bad = table.cases.map((c, i) => (got[i] === c.tier ? '' : `${c.label} / ${c.q}: ${got[i]} want ${c.tier} (${c.why})`)).filter((s) => s);
    expect(bad).toEqual([]);
  });

  test('an older muster-mirror: the menu offers the built-in commands alone, says why, and has no snippets', async ({ page }) => {
    await openView(page, oldHost);
    await input(page).click();
    await page.keyboard.type('/');
    await expect(menu(page)).toBeVisible();
    const names = await optionNames(page);
    expect(names).toContain('compact');
    expect(names).not.toContain('handoff');
    expect(names).not.toContain('proj-build');
    await expect(page.locator('#mv-complete-note')).toHaveText(/^Built-in commands only: this host's muster-mirror does not report/);
    await input(page).fill('');
    await page.keyboard.type(':');
    await expect(menu(page), 'no snippets: `:` opens nothing').toBeHidden();
  });

  test('a stale sign-in: Send and Queue each ask to confirm, then do what was asked — Send sends, Queue queues', async ({ page, context }) => {
    await openView(page);
    const before = pasted();
    await makeStale(context);
    await input(page).fill('a stale send');
    await page.locator('#mv-send').click();
    await expect(page.locator('#term-confirm')).toBeVisible();
    expect(pasted(), 'nothing is sent before the confirm').toEqual(before);
    await page.fill('#term-confirm-password', password);
    await page.locator('#term-confirm-submit').click();
    await expect(page.locator('#term-confirm')).toBeHidden();
    await expect.poll(pasted).toEqual([...before, `${pane} a stale send`]);
    await expect(queued(page)).toHaveCount(0);

    await makeStale(context);
    await input(page).fill('a stale queue');
    await page.locator('#mv-send-more').click();
    await page.getByRole('menuitem', { name: 'Queue: send when Claude Code is idle' }).click();
    await expect(page.locator('#term-confirm')).toBeVisible();
    await expect(queued(page)).toHaveCount(0);
    await page.fill('#term-confirm-password', password);
    await page.locator('#term-confirm-submit').click();
    await expect(page.locator('#term-confirm')).toBeHidden();
    await expect(queued(page)).toHaveCount(1);
    await expect(queued(page)).toContainText('a stale queue');
    expect(pasted(), 'the confirmed Queue sent nothing').toEqual([...before, `${pane} a stale send`]);
    await cancelQueued(page);
  });
});

test.describe('phone 390×844 touch', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

  test('the menu never covers the input — also on a viewport the keyboard shrank — and a tap inserts; the split button is two 44px targets', async ({ page }) => {
    await openView(page);
    for (const id of ['#mv-send', '#mv-send-more']) {
      const b = (await page.locator(id).boundingBox())!;
      expect(b.height, `${id} is a 44px target`).toBeGreaterThanOrEqual(44);
      expect(b.width, `${id} is a 44px target`).toBeGreaterThanOrEqual(44);
      expect(b.x + b.width).toBeLessThanOrEqual(390);
    }
    const covers = async () => {
      const [m, i, h] = await Promise.all([menu(page).boundingBox(), input(page).boundingBox(), page.locator('body > header').boundingBox()]);
      return { menuBottom: m!.y + m!.height, inputTop: i!.y, menuTop: m!.y, headerBottom: h!.y + h!.height, vh: page.viewportSize()!.height };
    };
    await input(page).tap();
    await page.keyboard.type('/');
    await expect(menu(page)).toBeVisible();
    for (const height of [844, 420]) {
      await page.setViewportSize({ width: 390, height });
      await page.waitForTimeout(250);
      await input(page).fill('');
      await page.keyboard.type('/');
      await expect(menu(page)).toBeVisible();
      const c = await covers();
      expect(c.menuBottom, `at ${height}px the menu ends above the input`).toBeLessThanOrEqual(c.inputTop);
      expect(c.menuTop, `at ${height}px the menu starts below the header`).toBeGreaterThanOrEqual(c.headerBottom - 1);
      expect(c.menuBottom).toBeLessThanOrEqual(c.vh);
    }
    await page.setViewportSize({ width: 390, height: 844 });
    await input(page).fill('');
    await page.keyboard.type('/depl');
    await page.locator('#mv-complete [data-name="deploy-it"]').tap();
    await expect(input(page)).toHaveValue('/deploy-it ');
    await expect(input(page)).toBeFocused();
    const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
    expect(sw, 'no horizontal scroll').toBeLessThanOrEqual(cw);
  });

  test('a tap on Send sends; the caret then Queue queues', async ({ page }) => {
    await openView(page);
    const before = pasted();
    await input(page).fill('tapped send');
    await page.locator('#mv-send').tap();
    await expect.poll(pasted).toEqual([...before, `${pane} tapped send`]);
    await input(page).fill('tapped queue');
    await page.locator('#mv-send-more').tap();
    await page.getByRole('menuitem', { name: 'Queue: send when Claude Code is idle' }).tap();
    await expect(queued(page)).toHaveCount(1);
    expect(pasted()).toEqual([...before, `${pane} tapped send`]);
    await cancelQueued(page);
  });
});

// CLS on the mirrored view: the page load, then opening both menus, at both
// sizes — under 0.01. The instrument is shown able to see a shift first (a
// block inserted above the conversation, with no input near it), so a zero is
// a measurement and not an observer that never ran.
for (const vp of [{ width: 390, height: 844 }, { width: 1280, height: 800 }]) {
  test.describe(`layout shift at ${vp.width}×${vp.height}`, () => {
    test.use({ viewport: vp });
    test(`the mirrored view with its split button and menus shifts less than 0.01`, async ({ page }) => {
      await login(page);
      await page.addInitScript(clsInit);

      await page.goto(origin + viewPath(newHost));
      await expect(input(page)).toBeEnabled({ timeout: 20_000 });
      await settle(page);
      await input(page).click();
      await page.keyboard.type('/');
      await expect(menu(page)).toBeVisible();
      await page.keyboard.press('Escape');
      await page.locator('#mv-send-more').click();
      await expect(page.locator('#mv-send-menu')).toBeVisible();
      await page.keyboard.press('Escape');
      await settle(page);
      const r = await readCls(page);
      expect(r.unsupported, 'layout-shift is observable here').toBeUndefined();
      expect(r.total, `shifts: ${JSON.stringify(r.shifts)}`).toBeLessThan(0.01);

      // The positive control: a shift the page did not ask for IS counted — a
      // block pushed in above the header (the page's column is not a scroller,
      // so no scroll anchoring can absorb it), with no input within 500 ms.
      await page.waitForTimeout(600);
      await page.evaluate(() => {
        const d = document.createElement('div');
        d.style.cssText = 'height:150px;flex:none';
        document.body.prepend(d);
      });
      await page.waitForTimeout(300);
      expect((await readCls(page)).total, 'the instrument sees a shift').toBeGreaterThan(0.01);
    });
  });
}
