import fs from 'node:fs';
import { request as pwRequest, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { hookToken, password, viewSwapPort } from '../lib/env';
import { cc, MirrorRig, mkchat } from '../lib/mirrorhost';

// THE RENDERED ↔ TERMINAL SWITCH KEEPS BOTH VIEWS' CONNECTIONS
// (internal/ui/view_swap.go) — a mirrored window's and a claude-code agent's —
// against a REAL muster-server (the e2eterm build: its noop driver plays an
// agent pod's terminal, `FAKE-TUI mode=ro|rw`) and a REAL muster-mirror built
// with `-tags e2emirror`: a per-window host whose two windows, work:1 (@1) and
// work:2 (@2), each run Claude Code over a transcript this spec writes.
//
// The first switch opens the other view in a frame over the page; every later
// one only shows or hides it.
//
// 🔴 THE COUNTS ARE THE OTHER SIDE'S OWN LOG, never the page's bookkeeping:
//   - a mirrored window's transcript stream is a host `watching "<s>" window
//     <id> (from the start | resuming at <n>, …)` line and its end `unwatched`;
//     a tmux attach is `attached … (read-only | interactive, …)`, its end
//     `detached`;
//   - an agent's chat socket is a server `agents: chat <name> connected` line
//     and its end `disconnected`; its terminal `agents: terminal <name>
//     attached (ro|rw)` and `detached`.
// Each spec takes a POSITIVE CONTROL from the same counter: a reload, or leaving
// the window, does open or close one — so the counter can see one.
test.use({ viewport: { width: 412, height: 915 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

const rig = new MirrorRig(viewSwapPort, 'e2e-swap', {
  MUSTER_AGENT_KINDS: 'gateway,claude-code',
  MUSTER_AGENT_CC_IMAGE: 'ghcr.io/example-org/claude-code-agent:e2e',
  MUSTER_AGENT_CC_ACCOUNTS: 'alpha',
  MUSTER_AGENT_CC_TOKEN_ALPHA: 'sk-ant-oat01-FAKE-e2e-swap-alpha',
  // The agent gateway, so an agent's chat page opens its chat socket (no turn
  // is sent, so no runtime answers it).
  MUSTER_AGENT_GATEWAY: 'hooks-sha256',
  MUSTER_AGENT_GATEWAY_MODEL: 'e2e-sentinel',
  MUSTER_AGENT_RUNTIME_CONFIG: '{}',
  MUSTER_AGENT_RUNTIME_INSTALL: 'exec true',
});
const origin = rig.origin;
let trOne = '';
let trTwo = '';

const q = (win: string) => `host=${encodeURIComponent(rig.host)}&session=work&window=${encodeURIComponent(win)}`;
const viewOf = (win: string) => `/mirror/view?${q(win)}`;
const termOf = (win: string) => `/mirror/terminal?${q(win)}`;
// The frame the switch opened, and the view in it.
const frameEl = (page: Page) => page.locator('iframe[data-muster-view]');
const framed = (page: Page) => page.frameLocator('iframe[data-muster-view]');
const pathOf = (page: Page) => new URL(page.url()).pathname;

function lines(log: string, re: RegExp): number {
  return log.split('\n').filter((l) => re.test(l)).length;
}
const hostCount = (re: RegExp) => lines(rig.log, re);
const serverCount = (re: RegExp) => lines(rig.server!.log(), re);
const watchedFromStart = (win: string) => hostCount(new RegExp(`watching "work" window ${win} \\(from the start,`));
const watchedResuming = (win: string) => hostCount(new RegExp(`watching "work" window ${win} \\(resuming at \\d+,`));
const unwatched = (win: string) => hostCount(new RegExp(`unwatched "work" window ${win} \\(`));
const attached = (win: string, mode = 'read-only') => hostCount(new RegExp(`attached "work" window ${win} \\(${mode},`));
const detached = (win: string) => hostCount(new RegExp(`detached "work" window ${win} \\(`));
const chatOpened = (a: string) => serverCount(new RegExp(`agents: chat ${a} connected$`));
const chatClosed = (a: string) => serverCount(new RegExp(`agents: chat ${a} disconnected$`));
const termAttached = (a: string, mode = 'ro') => serverCount(new RegExp(`agents: terminal ${a} attached \\(${mode}\\)`));
const termDetached = (a: string) => serverCount(new RegExp(`agents: terminal ${a} detached`));

// settled waits until both logs have stopped growing: every open or close the
// page asked for has been logged. A count read before that could miss one.
async function settled(): Promise<void> {
  let last = '';
  for (let i = 0; i < 40; i++) {
    const now = `${rig.log.length}/${rig.server!.log().length}`;
    if (now === last) return;
    last = now;
    await new Promise((r) => setTimeout(r, 400));
  }
}

test.beforeAll(async () => {
  test.setTimeout(180_000);
  await rig.start();
});

test.afterAll(async () => {
  await rig.stop();
});

// ---------------------------------------------------------------------------
// A MIRRORED WINDOW
// ---------------------------------------------------------------------------
test.describe('a mirrored window', () => {
  test.beforeEach(async () => {
    trOne = rig.transcript(cc.prompt('the prompt of window ONE') + cc.reply('Window one answered.') + cc.toolUse('tu_1', 'Bash', { command: 'ls' }) + cc.toolResult('tu_1', 'a\nb'), 'one');
    trTwo = rig.transcript(cc.prompt('the prompt of window TWO') + cc.reply('Window two answered.'), 'two');
    const now = Math.floor(Date.now() / 1000);
    const windows = rig.transcript([`work|@1|1|1|claude|${now - 60}|${trOne}`, `work|@2|2|0|claude|${now - 120}|${trTwo}`].join('\n') + '\n', 'windows');
    await rig.startHost({ MUSTER_MIRROR_INTERACTIVE: '1', MUSTER_MIRROR_E2E_WINDOWS_FILE: windows, MUSTER_MIRROR_E2E_REFRESH: '300ms' });
  });

  test.afterEach(async () => {
    await rig.stopHost();
  });

  // ready signs in and waits until the host lists BOTH windows as Claude Code
  // panes: a window not yet reported as one has no rendered view, and its view
  // URL redirects to the terminal.
  async function ready(page: Page): Promise<void> {
    await rig.login(page);
    // THIS host process is connected (rig.log is its output only): the list
    // can still show the previous process's windows, offline, for a moment.
    await expect.poll(() => rig.log.includes('connected to '), { message: `the host connected (log:\n${rig.log})`, timeout: 20_000 }).toBe(true);
    await expect
      .poll(async () => ((await (await page.request.get(origin + '/ui/agents')).text()).match(/data-mirror-claude/g) || []).length, {
        message: `both windows listed as Claude Code (muster-mirror log:\n${rig.log})`,
        timeout: 20_000,
      })
      .toBe(2);
  }

  async function openRendered(page: Page, win = '@1'): Promise<void> {
    await ready(page);
    await page.goto(origin + '/agents');
    await page.goto(origin + viewOf(win));
    await expect(page.locator('#mv-log [data-bubble="user"]').first()).toContainText(win === '@1' ? 'window ONE' : 'window TWO');
    await expect(page.locator('#mv-status')).toHaveText('Live');
  }

  test('rendered → terminal → rendered → terminal: ONE transcript stream and ONE tmux attach, the content intact, the address and title following the view', async ({ page }) => {
    await openRendered(page);
    await settled();
    expect(watchedFromStart('@1'), `one transcript stream (log:\n${rig.log})`).toBe(1);
    expect(attached('@1'), 'no attach yet').toBe(0);

    // To the terminal: it opens in a frame over the page; the address and the
    // title follow it; the host attaches once.
    await page.locator('#view-terminal').click();
    await expect(frameEl(page)).toBeVisible();
    await expect(framed(page).locator('#term .xterm-rows')).toContainText('FAKE-MIRROR session=work window=@1 mode=ro');
    await expect(framed(page).locator('#term-status')).toHaveText('Connected');
    expect(pathOf(page)).toBe('/mirror/terminal');
    expect(new URL(page.url()).searchParams.get('window')).toBe('@1');
    await expect(page).toHaveTitle(/^Terminal · /);
    // The page under the frame cannot be reached: nothing in it takes focus.
    const focusable = () => page.evaluate(() => { const i = document.getElementById('mv-input') as HTMLElement; i.focus(); const ok = document.activeElement === i; i.blur(); return ok; });
    expect(await focusable(), 'the page under the frame takes no focus').toBe(false);
    // While the terminal is on screen, the transcript moves on.
    fs.appendFileSync(trOne, cc.reply('Written while the terminal was on screen.'));
    await settled();
    expect(attached('@1'), `one tmux attach (log:\n${rig.log})`).toBe(1);

    // Back: the page under the frame, never reloaded — what was written
    // meanwhile is there.
    await framed(page).locator('#view-rendered').click();
    await expect(frameEl(page)).toBeHidden();
    expect(pathOf(page)).toBe('/mirror/view');
    await expect(page).not.toHaveTitle(/^Terminal · /);
    await expect(page.locator('#mv-log [data-tool-row]')).toHaveCount(1);
    await expect(page.locator('#mv-log [data-bubble="user"]')).toHaveCount(1);
    await expect(page.locator('#mv-log [data-bubble="assistant"]').last()).toContainText('Written while the terminal was on screen.');
    await expect(page.locator('#mv-status')).toHaveText('Live');
    expect(await focusable(), 'and back on screen it does again').toBe(true);

    // And again: the same frame, the same attach, its screen still drawn.
    await page.locator('#view-terminal').click();
    await expect(frameEl(page)).toBeVisible();
    expect(pathOf(page)).toBe('/mirror/terminal');
    await expect(framed(page).locator('#term .xterm-rows')).toContainText('FAKE-MIRROR session=work window=@1 mode=ro');
    await framed(page).locator('#view-rendered').click();
    await expect(frameEl(page)).toBeHidden();
    await page.locator('#view-terminal').click();
    await expect(frameEl(page)).toBeVisible();
    await expect(page.locator('iframe[data-muster-view]'), 'one frame, re-used').toHaveCount(1);
    await settled();
    expect(watchedFromStart('@1'), `still ONE transcript stream after three round trips (log:\n${rig.log})`).toBe(1);
    expect(watchedResuming('@1'), 'and no resumed one either').toBe(0);
    expect(unwatched('@1'), 'none ended').toBe(0);
    expect(attached('@1'), `still ONE tmux attach (log:\n${rig.log})`).toBe(1);
    expect(detached('@1'), 'none ended').toBe(0);

    // A reload opens the view ON SCREEN (the address followed it) as the page
    // itself — and it is a real load: a NEW attach, the positive control that
    // the counter sees one. The old document's two streams both end.
    await page.reload();
    await expect(page.locator('#term .xterm-rows')).toContainText('FAKE-MIRROR session=work window=@1 mode=ro');
    await expect(frameEl(page), 'the reloaded page is the terminal itself').toHaveCount(0);
    await settled();
    expect(attached('@1'), `a reload attaches again (log:\n${rig.log})`).toBe(2);
    expect(unwatched('@1'), 'the reloaded document closed its transcript stream').toBe(1);
    expect(detached('@1'), 'and its attach').toBe(1);

    // Back leaves the window (no switch added a history entry), and leaving
    // closes every stream the document held.
    await page.locator('#view-rendered').click();
    await expect(framed(page).locator('#mv-log [data-tool-row]')).toHaveCount(1);
    await page.goBack();
    await expect.poll(() => pathOf(page)).toBe('/agents');
    // The Agents tab draws its list in the document (#225): Back onto it has
    // the cards, and no view frame came back with the page.
    await expect(page.locator(`#agents-list > article[data-f-host="${rig.host}"]`).first()).toBeVisible();
    await expect(frameEl(page)).toHaveCount(0);
    await settled();
    expect(unwatched('@1'), `leaving closed the transcript stream (log:\n${rig.log})`).toBe(2);
    expect(detached('@1'), 'and the attach').toBe(2);
    expect(watchedFromStart('@1') - unwatched('@1'), 'no stream left open').toBe(0);
    expect(attached('@1') - detached('@1'), 'no attach left open').toBe(0);
  });

  test('starting on the terminal: the rendered view opens in the frame, and both streams are kept across round trips', async ({ page }) => {
    await ready(page);
    await page.goto(origin + termOf('@1'));
    await expect(page.locator('#term .xterm-rows')).toContainText('FAKE-MIRROR session=work window=@1 mode=ro');
    for (let i = 0; i < 3; i++) {
      await page.locator('#view-rendered').click();
      await expect(frameEl(page)).toBeVisible();
      await expect(framed(page).locator('#mv-log [data-tool-row]')).toHaveCount(1);
      expect(pathOf(page)).toBe('/mirror/view');
      await framed(page).locator('#view-terminal').click();
      await expect(frameEl(page)).toBeHidden();
      expect(pathOf(page)).toBe('/mirror/terminal');
    }
    await settled();
    expect(attached('@1'), `ONE attach (log:\n${rig.log})`).toBe(1);
    expect(detached('@1')).toBe(0);
    expect(watchedFromStart('@1'), 'ONE transcript stream').toBe(1);
    expect(unwatched('@1')).toBe(0);
  });

  test('switching to a different window closes the old window’s streams', async ({ page }) => {
    await openRendered(page, '@1');
    await page.locator('#view-terminal').click();
    await expect(framed(page).locator('#term .xterm-rows')).toContainText('FAKE-MIRROR session=work window=@1 mode=ro');
    await settled();
    expect(watchedFromStart('@1')).toBe(1);
    expect(attached('@1')).toBe(1);

    await page.goto(origin + viewOf('@2'));
    await expect(page.locator('#mv-log [data-bubble="user"]').first()).toContainText('window TWO');
    await settled();
    expect(unwatched('@1'), `@1's transcript stream ended (log:\n${rig.log})`).toBe(1);
    expect(detached('@1'), "@1's attach ended — the frame went with its page").toBe(1);
    expect(watchedFromStart('@2'), "@2's own stream").toBe(1);
    expect(hostCount(/watching "work"/) - hostCount(/unwatched "work"/), 'exactly one transcript stream open on the host').toBe(1);
    expect(hostCount(/ attached "work"/) - hostCount(/detached "work"/), 'no attach open on the host').toBe(0);
  });

  test('a link in the framed view that leaves the window navigates the TOP window, and closes both streams', async ({ page }) => {
    await openRendered(page);
    await page.locator('#view-terminal').click();
    await expect(framed(page).locator('#term-status')).toHaveText('Connected');
    await framed(page).locator('#term-back').click();
    await expect.poll(() => pathOf(page)).toBe('/agents');
    await expect(frameEl(page)).toHaveCount(0);
    await settled();
    expect(unwatched('@1'), `the page's stream ended (log:\n${rig.log})`).toBe(1);
    expect(detached('@1'), "the frame's attach ended").toBe(1);
  });

  test('live keys re-attach (a mode change needs one); off screen they drop to read-only and come back on screen; a read-only terminal is kept', async ({ page }) => {
    await openRendered(page);
    await page.locator('#view-terminal').click();
    const t = framed(page);
    await expect(t.locator('#term .xterm-rows')).toContainText('FAKE-MIRROR session=work window=@1 mode=ro');
    await settled();
    expect(attached('@1', 'read-only')).toBe(1);

    // Signed in just now, so the sign-in is fresh: live keys attach at once.
    await t.locator('#term-mode').click();
    await expect(t.locator('#term-mode')).toHaveAttribute('aria-checked', 'true');
    await expect(t.locator('#term .xterm-rows')).toContainText('FAKE-MIRROR session=work window=@1 mode=rw');
    await settled();
    expect(attached('@1', 'interactive'), `live keys re-attached (log:\n${rig.log})`).toBe(1);
    expect(detached('@1'), 'the read-only attach ended').toBe(1);

    // 🟢 Off screen it drops live keys (one read-only re-attach), and takes
    // them back when it is shown again — the sign-in is still fresh.
    await t.locator('#view-rendered').click();
    await expect(frameEl(page)).toBeHidden();
    await expect.poll(() => attached('@1', 'read-only'), { message: `hidden: back to read-only (log:\n${rig.log})` }).toBe(2);
    await expect(t.locator('#term-mode')).toHaveAttribute('aria-checked', 'false');
    await page.locator('#view-terminal').click();
    await expect(t.locator('#term-mode')).toHaveAttribute('aria-checked', 'true');
    await expect(t.locator('#term-status')).toHaveText('Connected · live keys');
    await settled();
    expect(attached('@1', 'interactive'), `live keys again on screen (log:\n${rig.log})`).toBe(2);
    expect(attached('@1', 'read-only')).toBe(2);
    expect(detached('@1'), 'each mode change ended the attach before it').toBe(3);
    // A switch while read-only is still no attach at all.
    await t.locator('#term-mode').click();
    await expect(t.locator('#term-mode')).toHaveAttribute('aria-checked', 'false');
    await settled();
    const ro = attached('@1', 'read-only');
    await t.locator('#view-rendered').click();
    await expect(frameEl(page)).toBeHidden();
    await page.locator('#view-terminal').click();
    await expect(frameEl(page)).toBeVisible();
    await settled();
    expect(attached('@1', 'read-only'), 'a read-only terminal is kept as it is').toBe(ro);
    expect(attached('@1', 'interactive')).toBe(2);
  });

  // The framed view is a document of its own: its composer, its step-up dialog
  // and its focus are its own, and the page's are the page's.
  test('each view sends from its own composer, and a stale send in the framed view asks in the frame’s own dialog', async ({ page }) => {
    await ready(page);
    const pasted = () => (fs.existsSync(trOne + '.pasted') ? fs.readFileSync(trOne + '.pasted', 'utf8') : '');
    await page.goto(origin + termOf('@1'));
    await expect(page.locator('#term-status')).toHaveText('Connected');
    await page.locator('#mv-input').fill('a terminal draft');

    await page.locator('#view-rendered').click();
    const v = framed(page);
    await expect(v.locator('#mv-log [data-tool-row]')).toHaveCount(1);
    await expect(v.locator('#mv-input'), 'the rendered view’s own box').toHaveValue('');
    // A STALE sign-in, PLAYED (mirrored.spec.ts says why): one intercepted 403.
    let refused = 0;
    await page.route('**/mirror/view/send?*', async (route) => {
      if (refused++ === 0) {
        await route.fulfill({ status: 403, headers: { 'X-Muster-Step-Up': 'required', 'Content-Type': 'text/plain' }, body: "confirm it's you" });
        return;
      }
      await route.continue();
    });
    await v.locator('#mv-input').fill('from the rendered view');
    await v.locator('#mv-send').click();
    await expect(v.locator('#term-confirm'), 'asked in the frame on screen').toBeVisible();
    await v.locator('#term-confirm-password').fill(password);
    await v.locator('#term-confirm-submit').click();
    await expect(v.locator('#term-confirm')).toBeHidden();
    await expect.poll(pasted).toContain('from the rendered view\n');
    await expect(v.locator('#mv-input')).toHaveValue('');
    expect(refused, 'the send was retried once after the confirm').toBe(2);
    await page.unroute('**/mirror/view/send?*');

    await v.locator('#view-terminal').click();
    await expect(frameEl(page)).toBeHidden();
    await expect(page.locator('#mv-input'), 'the terminal’s box kept its text').toHaveValue('a terminal draft');
    await page.locator('#mv-send').click();
    await expect(page.locator('#mv-input')).toHaveValue('');
    await expect.poll(pasted).toContain('a terminal draft\n');
    expect(pasted().split('\n').filter(Boolean).map((l) => l.replace(/^\S+ /, '')), 'each sent once, in order').toEqual(['from the rendered view', 'a terminal draft']);
  });

  // 🟡 A FORM THE BROWSER POSTS FROM THE FRAMED VIEW (the composer's own way
  // when its fetch gets no answer) is posted by the TOP window: its answer may
  // be a page that refuses to be framed (a sign-in, the plain confirm page).
  // Delivered once; the view it answers with is the top window's page.
  test('a send the framed view hands to the browser is posted by the TOP window, and delivered once', async ({ page }) => {
    await ready(page);
    const pasted = () => (fs.existsSync(trOne + '.pasted') ? fs.readFileSync(trOne + '.pasted', 'utf8') : '');
    await page.goto(origin + termOf('@1'));
    await expect(page.locator('#term-status')).toHaveText('Connected');
    await page.locator('#view-rendered').click();
    const v = framed(page);
    await expect(v.locator('#mv-input')).toBeEnabled();
    let aborted = 0;
    await page.route('**/mirror/view/send?*', async (route) => {
      if (route.request().headers()['x-requested-with'] === 'XMLHttpRequest') {
        aborted++;
        await route.abort('failed');
        return;
      }
      await route.continue();
    });
    await v.locator('#mv-input').fill('by the browser, from the frame');
    await Promise.all([page.waitForURL((u) => u.pathname === '/mirror/view' && u.searchParams.get('notice') === 'sent'), v.locator('#mv-send').click()]);
    expect(aborted, 'the script tried its own way first').toBe(1);
    await expect(frameEl(page), 'the answer is the top window’s page').toHaveCount(0);
    await expect(page.locator('#mv-note')).toHaveText('Sent.');
    await expect.poll(pasted).toBe('%e2e-work-@1 by the browser, from the frame\n');
    await page.unroute('**/mirror/view/send?*');
  });

  // The framed view keeps its place while hidden, and a reader at the end
  // follows what arrived meanwhile. ⚠ AN INVARIANT GUARD, not a regression
  // test: a frame keeps its document's scroll offsets whether it is hidden
  // with visibility (as it is) or display:none — that mutant was measured to
  // pass. It pins the outcome, whatever hides the frame.
  test('the reader’s place in the framed view is kept: a scrolled-back log stays put, one at the end follows what arrived meanwhile', async ({ page }) => {
    let long = '';
    for (let i = 0; i < 40; i++) long += cc.reply(`Filler reply number ${i} with enough words to take a line or two on a phone.`);
    fs.appendFileSync(trOne, long);
    await ready(page);
    await page.goto(origin + termOf('@1'));
    await expect(page.locator('#term-status')).toHaveText('Connected');
    await page.locator('#view-rendered').click();
    const log = framed(page).locator('#mv-log');
    await expect(log.locator('[data-bubble="assistant"]').last()).toContainText('Filler reply number 39');
    const geometry = () => log.evaluate((el) => ({ top: el.scrollTop, max: el.scrollHeight - el.clientHeight }));
    await expect.poll(async () => (await geometry()).max, 'the log scrolls').toBeGreaterThan(600);

    await log.evaluate((el) => { el.scrollTop = 300; });
    await expect.poll(async () => (await geometry()).top).toBe(300);
    await framed(page).locator('#view-terminal').click();
    await expect(frameEl(page)).toBeHidden();
    await page.locator('#view-rendered').click();
    await expect(frameEl(page)).toBeVisible();
    await expect.poll(async () => (await geometry()).top, 'the place the reader left').toBe(300);

    await log.evaluate((el) => { el.scrollTop = el.scrollHeight; });
    await page.waitForTimeout(100);
    await framed(page).locator('#view-terminal').click();
    await expect(frameEl(page)).toBeHidden();
    fs.appendFileSync(trOne, cc.reply('Arrived while the terminal was on screen.') + long);
    await expect(log.locator('[data-bubble="assistant"]')).toHaveCount(82, { timeout: 15_000 });
    await page.locator('#view-rendered').click();
    await expect(frameEl(page)).toBeVisible();
    await expect.poll(async () => { const g = await geometry(); return g.max - g.top; }, 'at the end again').toBeLessThan(80);
  });

  // 🔴 THE KEYBOARD IS THE TOP WINDOW'S. On iOS the keyboard overlays the page:
  // the TOP window's visual viewport shrinks, never the frame's — so the framed
  // rendered view's pinned composer must measure the top window to ride above
  // it. Played as mirrored.spec.ts plays it (a visualViewport shorter than the
  // window), on the TOP window only.
  test('the framed rendered view’s composer rides above a keyboard that overlays the page', async ({ page }) => {
    const H = 915, KB = 300;
    await ready(page);
    await page.addInitScript((kb) => {
      if (window !== window.top) return;
      const vv = new EventTarget();
      Object.defineProperties(vv, {
        height: { get: () => window.innerHeight - kb },
        width: { get: () => window.innerWidth },
        offsetTop: { value: 0 },
        offsetLeft: { value: 0 },
        scale: { value: 1 },
      });
      Object.defineProperty(window, 'visualViewport', { configurable: true, get: () => vv });
    }, KB);
    await page.goto(origin + termOf('@1'));
    await expect(page.locator('#term-status')).toHaveText('Connected');
    await page.locator('#view-rendered').click();
    const v = framed(page);
    await expect(v.locator('#mv-input')).toBeEnabled();
    await expect.poll(async () => { const b = (await v.locator('#mv-send').boundingBox())!; return b.y + b.height; },
      { message: 'with an overlaid keyboard the framed composer sits above it' }).toBeLessThanOrEqual(H - KB + 0.5);
  });
});

// ---------------------------------------------------------------------------
// A CLAUDE-CODE AGENT: its chat page (hx-boost'ed, with its own chat socket)
// and its terminal (the noop driver's FAKE-TUI).
// ---------------------------------------------------------------------------
test.describe('a claude-code agent', () => {
  let agent = '';

  async function agentNames(): Promise<Set<string>> {
    const ctx = await pwRequest.newContext({ baseURL: origin, extraHTTPHeaders: { Authorization: `Bearer ${hookToken}` } });
    const r = await ctx.get('/api/agents');
    expect(r.status(), await r.text()).toBe(200);
    const rows = (await r.json()) as { name: string }[];
    await ctx.dispose();
    return new Set(rows.map((a) => a.name));
  }

  async function signedIn(page: Page): Promise<void> {
    await rig.login(page);
    if (agent) return;
    const before = await agentNames();
    await page.goto(origin + '/agents');
    await page.click('#fab-agents');
    const sheet = page.locator('#agent-modal form');
    await sheet.locator('[data-kind-option="claude-code"]').click();
    await sheet.locator('textarea[name="note_text"]').fill('e2e view swap');
    const resp = page.waitForResponse((r) => r.url().endsWith('/agents') && r.request().method() === 'POST');
    await sheet.locator('button[value="save"]').click();
    expect((await resp).status()).toBe(200);
    const created = [...(await agentNames())].filter((n) => !before.has(n));
    expect(created.length, 'exactly one agent created').toBe(1);
    agent = created[0];
  }

  async function openChat(page: Page): Promise<void> {
    await signedIn(page);
    await page.goto(origin + '/agents');
    await page.goto(origin + '/agents/' + agent);
    await expect(page.locator('#view-rendered')).toHaveAttribute('aria-current', 'page');
    await expect.poll(() => chatOpened(agent) - chatClosed(agent), { message: `the chat socket opened (server log:\n${rig.server!.log()})` }).toBe(1);
  }

  test('chat → terminal → chat → …: ONE chat socket and ONE terminal attach across round trips, the address following the view', async ({ page }) => {
    await openChat(page);
    await settled();
    const chat0 = chatOpened(agent);
    const term0 = termAttached(agent);
    for (let i = 0; i < 3; i++) {
      await page.locator('#view-terminal').click();
      await expect(frameEl(page)).toBeVisible();
      await expect(framed(page).locator('#term .xterm-rows')).toContainText('FAKE-TUI mode=ro');
      expect(pathOf(page)).toBe(`/agents/${agent}/terminal`);
      await framed(page).locator('#view-rendered').click();
      await expect(frameEl(page)).toBeHidden();
      expect(pathOf(page)).toBe(`/agents/${agent}`);
      await expect(page.locator('#chat-form')).toBeVisible();
    }
    await settled();
    expect(chatOpened(agent) - chat0, `no new chat socket (server log:\n${rig.server!.log()})`).toBe(0);
    expect(chatClosed(agent), 'none closed').toBe(chatOpened(agent) - 1);
    expect(termAttached(agent) - term0, 'ONE terminal attach').toBe(1);
    expect(termDetached(agent), 'none ended').toBe(term0);

    // A reload on the terminal opens it as the page — the positive control:
    // a NEW attach; and the old page's chat socket and attach both end.
    await page.locator('#view-terminal').click();
    await expect(frameEl(page)).toBeVisible();
    await page.reload();
    await expect(page.locator('#term .xterm-rows')).toContainText('FAKE-TUI mode=ro');
    await expect(frameEl(page)).toHaveCount(0);
    await settled();
    expect(termAttached(agent) - term0, `a reload attaches again (server log:\n${rig.server!.log()})`).toBe(2);
    expect(termDetached(agent) - term0, "the old page's attach ended").toBe(1);
    expect(chatOpened(agent) - chatClosed(agent), "and its chat socket").toBe(0);
  });

  test('starting on the terminal: the chat opens in the frame and is kept; a boosted link in it navigates the TOP window, ending both connections', async ({ page }) => {
    await signedIn(page);
    await page.goto(origin + `/agents/${agent}/terminal`);
    await expect(page.locator('#term .xterm-rows')).toContainText('FAKE-TUI mode=ro');
    await settled();
    const term0 = termAttached(agent);
    const chat0 = chatOpened(agent);

    await page.locator('#view-rendered').click();
    await expect(frameEl(page)).toBeVisible();
    await expect(framed(page).locator('#view-rendered')).toHaveAttribute('aria-current', 'page');
    await expect.poll(() => chatOpened(agent) - chat0, { message: `the framed chat connected (server log:\n${rig.server!.log()})` }).toBe(1);
    expect(pathOf(page)).toBe(`/agents/${agent}`);
    for (let i = 0; i < 2; i++) {
      await framed(page).locator('#view-terminal').click();
      await expect(frameEl(page)).toBeHidden();
      await page.locator('#view-rendered').click();
      await expect(frameEl(page)).toBeVisible();
    }
    await settled();
    expect(chatOpened(agent) - chat0, 'ONE chat socket').toBe(1);
    expect(termAttached(agent) - term0, 'no new terminal attach').toBe(0);

    // A boosted link in the framed chat page (its header's way back to the
    // list) navigates the TOP window, and both connections end.
    await framed(page).locator('#sidebar-open').click();
    await framed(page).locator('#sidebar a[data-tab="agents"]').click();
    await expect.poll(() => pathOf(page)).toBe('/agents');
    await expect(frameEl(page)).toHaveCount(0);
    await expect(page.locator('#agents-list > article').first(), 'the list is drawn on arrival (#225)').toBeVisible();
    await settled();
    expect(chatOpened(agent) - chatClosed(agent), `the chat socket ended (server log:\n${rig.server!.log()})`).toBe(0);
    expect(termAttached(agent) - termDetached(agent), 'and the attach').toBe(0);
  });

  test('the framed terminal’s back link to the chat shows the chat — no reload, no new socket', async ({ page }) => {
    await openChat(page);
    await settled();
    const chat0 = chatOpened(agent);
    await page.locator('#view-terminal').click();
    await expect(framed(page).locator('#term-status')).toHaveText('Connected');
    await framed(page).locator('#term-back').click();
    await expect(frameEl(page)).toBeHidden();
    expect(pathOf(page)).toBe(`/agents/${agent}`);
    await settled();
    expect(chatOpened(agent) - chat0, `no new chat socket (server log:\n${rig.server!.log()})`).toBe(0);
  });

  test('live keys re-attach the agent terminal; off screen it drops to read-only, and takes them back on screen', async ({ page }) => {
    await openChat(page);
    await settled();
    const rw0 = termAttached(agent, 'rw');
    const ro0 = termAttached(agent, 'ro');
    await page.locator('#view-terminal').click();
    const t = framed(page);
    await expect(t.locator('#term .xterm-rows')).toContainText('FAKE-TUI mode=ro');
    await t.locator('#term-mode').click();
    await expect(t.locator('#term .xterm-rows')).toContainText('FAKE-TUI mode=rw');
    await settled();
    expect(termAttached(agent, 'rw') - rw0, 'live keys re-attached').toBe(1);
    // 🟢 Off screen it drops live keys; on screen again it takes them back.
    await t.locator('#view-rendered').click();
    await expect(frameEl(page)).toBeHidden();
    await expect.poll(() => termAttached(agent, 'ro') - ro0, { message: 'hidden: back to read-only' }).toBe(2);
    await page.locator('#view-terminal').click();
    await expect(t.locator('#term-mode')).toHaveAttribute('aria-checked', 'true');
    await expect(t.locator('#term .xterm-rows')).toContainText('FAKE-TUI mode=rw');
    await settled();
    expect(termAttached(agent, 'rw') - rw0, 'live keys again on screen').toBe(2);
    expect(termAttached(agent, 'ro') - ro0).toBe(2);
  });

  // 🟡 THE FRAMED VIEW NAVIGATING ITSELF. Every load of the frame is the top
  // window's to judge: a page that is not this view goes to the TOP window —
  // never into the frame, where a page that refuses to be framed leaves a white
  // screen and a stale address.
  // A dedicated agent per test (e2e/mkchat): claude-code, with a task ready for
  // review (so its chat has Mark complete) and a message (so New chat opens a
  // NEW session).
  function taskAgent(tag: string): string {
    const name = `swap-${tag}-${Date.now().toString(36)}`;
    mkchat(name, [{ role: 'user', content: 'Fix the tunnel.' }], { MUSTER_E2E_KIND: 'claude-code', MUSTER_E2E_CC_ACCOUNT: 'alpha', MUSTER_E2E_TASK: `Task of ${name}` });
    return name;
  }
  async function chatInFrame(page: Page, name: string): Promise<void> {
    await signedIn(page);
    await page.goto(origin + `/agents/${name}/terminal`);
    await expect(page.locator('#term-status')).toHaveText('Connected');
    await page.locator('#view-rendered').click();
    await expect(frameEl(page)).toBeVisible();
    await expect(framed(page).locator('#chat-form')).toBeVisible();
  }
  const noWhiteFrame = async (page: Page) => {
    await expect(frameEl(page), 'no frame left behind').toHaveCount(0);
  };

  test('Mark complete in the framed chat takes the TOP window to the Agents list — no white frame', async ({ page }) => {
    const name = taskAgent('done');
    try {
      await chatInFrame(page, name);
      await framed(page).getByRole('button', { name: 'Mark complete' }).click();
      await expect.poll(() => pathOf(page), { message: 'the top window went to the list' }).toBe('/agents');
      await noWhiteFrame(page);
      await expect(page.locator('#agents-list')).toBeVisible();
    } finally {
      mkchat(name, null);
    }
  });

  test('a 401 in the framed chat reloads the TOP window, which lands on sign-in', async ({ page }) => {
    const name = taskAgent('auth');
    try {
      await chatInFrame(page, name);
      // The session is gone, and the next htmx request of the framed page is
      // answered 401: the auth gate reloads — the TOP window must be what goes
      // to sign-in.
      await page.context().clearCookies();
      await page.route('**/ui/agents?e2e=401', (route) => route.fulfill({ status: 401, body: 'unauthorized' }));
      const f = page.frames().find((x) => x !== page.mainFrame() && x.url().includes(`/agents/${name}`))!;
      await f.evaluate(() => (window as unknown as { htmx: { ajax: (m: string, u: string, o: object) => void } }).htmx.ajax('GET', '/ui/agents?e2e=401', { target: '#task-modal', swap: 'none' }));
      await expect.poll(() => pathOf(page), { message: 'the top window went to sign-in' }).toBe('/login');
      await noWhiteFrame(page);
      await expect(page.locator('#muster-password')).toBeVisible();
    } finally {
      mkchat(name, null);
    }
  });

  test('New chat in the framed chat updates the TOP address to the new session', async ({ page }) => {
    const name = taskAgent('new');
    try {
      await chatInFrame(page, name);
      const before = new URL(page.url()).searchParams.get('session');
      expect(before, 'the chat page names its session').toBeTruthy();
      await framed(page).getByRole('button', { name: 'New chat session' }).click();
      await expect.poll(() => new URL(page.url()).searchParams.get('session'), { message: 'the top address follows the new session' }).not.toBe(before);
      expect(pathOf(page)).toBe(`/agents/${name}`);
      await expect(frameEl(page)).toBeVisible();
      const inFrame = await frameEl(page).evaluate((f) => (f as HTMLIFrameElement).contentWindow!.location.href);
      expect(page.url(), 'the address is the frame’s').toBe(inFrame);
    } finally {
      mkchat(name, null);
    }
  });

  // 🟡 htmx's HISTORY CACHE: a boosted navigation away snapshots the body, and
  // Back restores it — without the frame, or the frame loads its view again,
  // unseen (a new attach).
  test('Back to a chat page from htmx’s history restores no frame, and attaches nothing', async ({ page }) => {
    await openChat(page);
    await settled();
    const a0 = termAttached(agent), d0 = termDetached(agent);
    await page.locator('#view-terminal').click();
    await expect(framed(page).locator('#term-status')).toHaveText('Connected');
    await framed(page).locator('#view-rendered').click();
    await expect(frameEl(page)).toBeHidden();
    await page.locator('#sidebar-open').click();
    await page.locator('#sidebar a[data-tab="agents"]').click();
    await expect.poll(() => pathOf(page)).toBe('/agents');
    await settled();
    const attaches = termAttached(agent);
    expect(attaches - a0, 'one attach while on the page').toBe(1);
    expect(termDetached(agent) - d0, 'leaving ended it').toBe(1);
    await page.goBack();
    await expect.poll(() => pathOf(page)).toBe(`/agents/${agent}`);
    await expect(page.locator('#chat-form')).toBeVisible();
    await page.waitForTimeout(1500);
    await settled();
    await expect(frameEl(page), 'the restored page has no frame').toHaveCount(0);
    expect(termAttached(agent), `no attach on Back (server log:\n${rig.server!.log()})`).toBe(attaches);
    // And the switch works on the restored page. (Clicked by script: the
    // snapshot was taken with the sidebar open, and its backdrop is restored
    // over the header.)
    await page.locator('#view-terminal').evaluate((a) => (a as HTMLElement).click());
    await expect(framed(page).locator('#term-status')).toHaveText('Connected');
  });
});
