import fs from 'node:fs';
import path from 'node:path';
import { Browser, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { openCodeViewPort } from '../lib/env';
import { MirrorRig } from '../lib/mirrorhost';
import { writeFileAtomic } from '../lib/atomic';
import { clsInit, readCls, settle, ClsResult } from '../lib/cls';

// AN OPENCODE WINDOW, ON A REAL SERVER (the e2eterm binary, mirror plugin on)
// AND A REAL muster-mirror whose fakes are tmux and the opencode server
// (docs/design/opencode.md). The fake host runs a FAKE opencode v2 server in
// its own process (internal/opencodehost/opencodetest) and holds a token it
// issued; the REAL client, title match and tail read it over HTTP. The spec
// writes the server's sessions and messages (ops.jsonl, applied on every
// listing: cmd/muster-mirror/e2eopencode.go) and the panes' titles (the
// opencode windows file). Every value is invented fixture data.
//
// 🔴 REGRESSION TEST. On origin/main an opencode window has no rendered view:
// its card opens the Terminal, and /mirror/view redirects there.

const rig = new MirrorRig(openCodeViewPort, 'e2e-opencode');
const sid = 'ses_0e2efixture1a2b3c4d';
const t0 = Math.floor(Date.now() / 1000);
let ops = '';
let hostEnv: NodeJS.ProcessEnv = {};
let state: object = {};

function op(o: Record<string, unknown>) {
  fs.appendFileSync(ops, JSON.stringify(o) + '\n');
}
const user = (id: string, created: number, text: string) => ({ id, type: 'user', text, files: [], agents: [], time: { created } });
const assistant = (id: string, completed: number, content: object[]) => ({
  id,
  type: 'assistant',
  agent: 'build',
  model: { id: 'fixture-model', providerID: 'fixture' },
  ...(completed ? { finish: 'stop', time: { created: 1, completed } } : { time: { created: 1 } }),
  content,
});
const tool = (id: string, name: string, status: string, input: object) => ({
  type: 'tool',
  id,
  name,
  executed: false,
  time: { created: 1 },
  state: { status, input, metadata: {}, ...(status === 'completed' ? { content: [{ type: 'text', text: 'TOOL-OUTPUT-FIXTURE' }] } : {}) },
});

test.beforeAll(async ({ browser }) => {
  test.setTimeout(180_000);
  await rig.start();
  const dir = fs.mkdtempSync(path.join(rig.dir, 'opencode-'));
  ops = path.join(dir, 'ops.jsonl');
  op({ op: 'session', id: sid, title: 'Fix the flaky fixture loader' });
  // Two sessions with the default title: a pane showing it is ambiguous.
  op({ op: 'session', id: 'ses_0e2edup1', title: 'New session' });
  op({ op: 'session', id: 'ses_0e2edup2', title: 'New session' });
  op({ op: 'message', session: sid, message: user('msg_0e2e01', 1700000000001, 'list the opencode fixtures') });
  op({
    op: 'message',
    session: sid,
    message: assistant('msg_0e2e02', 9, [
      { type: 'reasoning', text: 'REASONING-NEVER-SHOWN' },
      { type: 'text', text: 'Listing them now.' },
      tool('call_e2e_1', 'shell', 'completed', { command: 'ls fixtures/', description: 'list' }),
    ]),
  });
  const windows = path.join(rig.dir, 'windows');
  writeFileAtomic(windows, `plain|@1|0|1|zsh|${t0 - 50}|\n`);
  const ocWindows = path.join(rig.dir, 'opencode-windows');
  // sess|id|index|active|activity|pane title
  writeFileAtomic(ocWindows, `oc|@2|0|1|${t0 - 10}|OC | Fix the flaky fixture loader\noc|@3|1|0|${t0 - 20}|OC | New session\n`);
  hostEnv = {
    MUSTER_MIRROR_E2E_WINDOWS_FILE: windows,
    MUSTER_MIRROR_E2E_OPENCODE_WINDOWS_FILE: ocWindows,
    MUSTER_MIRROR_E2E_OPENCODE_DIR: dir,
    MUSTER_MIRROR_E2E_REFRESH: '200ms',
    // Interactive: the composer is disabled by the runtime, not by the host.
    MUSTER_MIRROR_INTERACTIVE: '1',
  };
  await rig.startHost(hostEnv);
  const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
  const page = await ctx.newPage();
  await rig.login(page);
  await rig.waitListed(page, 'session=oc');
  state = await ctx.storageState();
  await ctx.close();
});

test.afterAll(async () => {
  await rig.stop();
});

test.use({ storageState: { cookies: [], origins: [] } });

const viewOf = (window: string) => `${rig.viewPath('oc')}&window=${encodeURIComponent(window)}`;
const card = (page: Page, window: string) =>
  page
    .locator('#agents-list > article[data-mirror-card]')
    .filter({ has: page.locator(`a[href*="session=${window === '@1' ? 'plain' : 'oc'}"][href*="window=${encodeURIComponent(window)}"]`) });

test('an opencode window opens a read-only rendered view of the session its title names, live', async ({ page }) => {
  await rig.login(page);
  await page.goto(rig.origin + '/agents');
  // The matched card opens the rendered view; the ambiguous one, the Terminal.
  await expect(card(page, '@2').locator('a[href*="/mirror/view?"]'), `the matched card (mirror log:\n${rig.log})`).toHaveCount(1, { timeout: 20_000 });
  await expect(card(page, '@3').locator('a[href*="/mirror/terminal?"]')).toHaveCount(1);
  for (const w of ['@2', '@3']) await expect(card(page, w).locator('[data-mirror-kind]')).toContainText('opencode');
  // The ambiguous one says why it has no view; the matched one has no note.
  await expect(card(page, '@3').locator('[data-mirror-note]')).toHaveText('2 opencode sessions match the title "New session": ambiguous, so none is shown');
  await expect(card(page, '@2').locator('[data-mirror-note]')).toHaveCount(0);

  await page.goto(rig.origin + viewOf('@2'));
  await expect(page).toHaveURL(/\/mirror\/view\?/);
  const log = page.locator('#mv-log');
  // It says how it found the session.
  await expect(log.locator('[data-notice-row="opencode"]').first()).toContainText('matched by title: Fix the flaky fixture loader', { timeout: 20_000 });
  await expect(log.locator('[data-bubble="user"]').first()).toContainText('list the opencode fixtures');
  await expect(log).toContainText('Listing them now.');
  await expect(log.locator('[data-tool-row]').first()).toContainText('ls fixtures/');
  // 🔴 Name and target only: no reasoning, no tool output.
  await expect(log).not.toContainText('REASONING-NEVER-SHOWN');
  await expect(log).not.toContainText('TOOL-OUTPUT-FIXTURE');

  // 🔴 Read-only on an interactive host: disabled, saying why, no queue.
  await expect(page.locator('#mv-input')).toBeDisabled();
  await expect(page.locator('#mv-readonly')).toContainText("Sending to opencode isn't supported yet");
  await expect(page.locator('#mv-queue, #mv-queue-send')).toHaveCount(0);

  // A turn arrives without a reload: the prompt, busy while it streams, the
  // reply once final, idle after.
  op({ op: 'message', session: sid, message: user('msg_0e2e03', 1700000000003, 'and the second prompt, LIVE') });
  op({ op: 'status', session: sid, status: 'busy' });
  op({ op: 'message', session: sid, message: assistant('msg_0e2e04', 0, [{ type: 'text', text: 'Second repl' }]) });
  await expect(log.locator('[data-bubble="user"]').last()).toContainText('and the second prompt, LIVE', { timeout: 20_000 });
  await expect(page.locator('#mv-working')).toBeVisible({ timeout: 20_000 });
  op({ op: 'message', session: sid, message: assistant('msg_0e2e04', 12, [{ type: 'text', text: 'Second reply, also live.' }]) });
  op({ op: 'status', session: sid, status: 'idle' });
  await expect(log).toContainText('Second reply, also live.', { timeout: 20_000 });
  await expect(page.locator('#mv-working')).toBeHidden({ timeout: 20_000 });
  // The streaming text was never drawn half-written: one row says it, whole.
  await expect(log.getByText(/Second repl/)).toHaveCount(1);
  await expect(log.getByText(/Second repl/)).toHaveText('Second reply, also live.');

  // The ambiguous window has no view: its address goes to the Terminal.
  await page.goto(rig.origin + viewOf('@3'));
  await expect(page).toHaveURL(/\/mirror\/terminal\?/);
});

// --- Layout shift, opening the view: under 0.01 at a phone and a desktop.
const BAR = 0.01;
const viewports = [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
];
// Every shift measured, input-excluded or not: Chromium's mobile emulation
// reports early shifts with hadRecentInput though nothing was input
// (cls-long-transcript.spec.ts says how it was measured).
const moved = (r: ClsResult) => +(r.total + r.excluded).toFixed(5);
const report = (r: ClsResult) => `${moved(r)} — ${JSON.stringify(r.shifts)} excluded: ${JSON.stringify(r.excludedShifts)}`;

async function openView(browser: Browser, use: object) {
  const ctx = await browser.newContext({ ...use, baseURL: rig.origin, storageState: state as never });
  const page = await ctx.newPage();
  await page.addInitScript(clsInit);
  await page.goto(rig.origin + viewOf('@2'));
  await expect(page.locator('#mv-log [data-bubble="user"]').first()).toBeVisible({ timeout: 20_000 });
  await settle(page);
  return { ctx, page };
}

for (const vp of viewports) {
  test(`${vp.name}: opening an opencode view shifts less than ${BAR}`, async ({ browser }) => {
    const { ctx, page } = await openView(browser, vp.use);
    try {
      const r = await readCls(page);
      console.log(`[opencode view ${vp.name}] ${report(r)}`);
      expect(moved(r), report(r)).toBeLessThan(BAR);
    } finally {
      await ctx.close();
    }
  });

  // The positive control on THIS page: a block put above the log, unasked,
  // MUST be measured — so the number above is not an instrument that sees
  // nothing.
  test(`${vp.name}: positive control: an unprovoked shift on the opencode view is measured`, async ({ browser }) => {
    const { ctx, page } = await openView(browser, vp.use);
    try {
      await page.evaluate(() => {
        const l = document.getElementById('mv-log')!;
        const d = document.createElement('div');
        d.style.height = '160px';
        l.parentElement!.insertBefore(d, l);
      });
      await settle(page, 500);
      const r = await readCls(page);
      console.log(`[opencode view ${vp.name}] positive control: ${report(r)}`);
      expect(moved(r), report(r)).toBeGreaterThan(BAR);
    } finally {
      await ctx.close();
    }
  });
}

// LAST, because it breaks the fake server for good: the server's password is
// rotated, which revokes the host's token. The window keeps its card, as an
// opencode window with NO view: its address goes to the Terminal.
test('a refused token leaves the opencode window with no view', async ({ page }) => {
  await rig.login(page);
  op({ op: 'rotate' });
  await expect
    .poll(
      async () => {
        await page.goto(rig.origin + viewOf('@2'));
        return new URL(page.url()).pathname;
      },
      { message: `the view after the token is refused (mirror log:\n${rig.log})`, timeout: 30_000 },
    )
    .toBe('/mirror/terminal');
  // The card says why, and what fixes it.
  await page.goto(rig.origin + '/agents');
  await expect(card(page, '@2').locator('[data-mirror-note]')).toContainText("refused muster-mirror's token", { timeout: 20_000 });
  await expect(card(page, '@2').locator('[data-mirror-note]')).toContainText('muster-mirror opencode-pair');
  await expect.poll(() => rig.log, { message: 'the host logs the refusal' }).toContain("refused muster-mirror's token");
});
