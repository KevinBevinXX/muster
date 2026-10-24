import { request as pwRequest, Page, WebSocket as PWWebSocket } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { hookToken, password, termPort, versionTerm } from '../lib/env';
import { startServer, Running } from '../lib/server';
import { touchDrag } from '../lib/touch';
import { clsInit, readCls, settle } from '../lib/cls';

// A claude-code agent's live terminal, at a phone's width (412×915), against a
// REAL muster-server built with `-tags e2eterm`: the page, the socket, the
// session door, the kind check and the capability gate are the shipped code;
// only the pod is played, by a fake terminal that prints what reached it
// (cmd/muster-server/e2eterm.go): `FAKE-TUI mode=ro|rw`, `size=CxR` per
// resize and `in=<HEX>` per input chunk. The tokens below are obvious fakes and
// nothing is provisioned ("Save for later").
test.use({ viewport: { width: 412, height: 915 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

const origin = `http://localhost:${termPort}`;
let server: Running | undefined;

test.beforeAll(async () => {
  server = await startServer(versionTerm, termPort, {
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
    MUSTER_AGENT_KINDS: 'gateway,claude-code',
    MUSTER_AGENT_CC_IMAGE: 'ghcr.io/example-org/claude-code-agent:e2e',
    MUSTER_AGENT_CC_ACCOUNTS: 'alpha',
    MUSTER_AGENT_CC_TOKEN_ALPHA: 'sk-ant-oat01-FAKE-e2e-term-alpha',
  });
});
test.afterAll(async () => {
  await server?.stop();
});

interface AgentRow {
  name: string;
  kind: string;
}

async function agentNames(): Promise<Set<string>> {
  const ctx = await pwRequest.newContext({ baseURL: origin, extraHTTPHeaders: { Authorization: `Bearer ${hookToken}` } });
  const r = await ctx.get('/api/agents');
  expect(r.status(), await r.text()).toBe(200);
  const rows = (await r.json()) as AgentRow[];
  await ctx.dispose();
  return new Set(rows.map((a) => a.name));
}

async function login(page: Page) {
  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
}

// saveAgent creates one agent of the kind through the dispatch sheet's "Save
// for later" and returns its name (diffed, so an earlier run's rows cannot
// answer).
async function saveAgent(page: Page, kind: 'gateway' | 'claude-code', note: string): Promise<string> {
  const before = await agentNames();
  await page.goto(origin + '/agents');
  await page.click('#fab-agents');
  const sheet = page.locator('#agent-modal form');
  await expect(sheet.locator('[data-kind-picker]')).toBeVisible();
  await sheet.locator(`[data-kind-option="${kind}"]`).click();
  await sheet.locator('textarea[name="note_text"]').fill(note);
  const resp = page.waitForResponse((r) => r.url().endsWith('/agents') && r.request().method() === 'POST');
  await sheet.locator('button[value="save"]').click();
  expect((await resp).status()).toBe(200);
  const created = [...(await agentNames())].filter((n) => !before.has(n));
  expect(created.length, 'exactly one agent created').toBe(1);
  return created[0];
}

async function noHorizontalScroll(page: Page) {
  const [sw, cw, bw] = await page.evaluate(() => [
    document.documentElement.scrollWidth,
    document.documentElement.clientWidth,
    document.body.scrollWidth,
  ]);
  expect(sw, 'document scrollWidth').toBeLessThanOrEqual(cw);
  expect(bw, 'body scrollWidth').toBeLessThanOrEqual(cw);
}

// The bytes the page sent as BINARY frames on the terminal socket, hex.
function recordSent(page: Page): string[] {
  const sent: string[] = [];
  page.on('websocket', (ws: PWWebSocket) => {
    if (!ws.url().includes('/terminal/ws')) return;
    ws.on('framesent', (f) => {
      if (typeof f.payload !== 'string') sent.push(Buffer.from(f.payload).toString('hex').toUpperCase());
    });
  });
  return sent;
}

const screen = (page: Page) => page.locator('#term .xterm-rows');

test('a claude-code agent opens its live terminal read-only at phone width', async ({ page }) => {
  await login(page);
  const name = await saveAgent(page, 'claude-code', 'e2e terminal: open');
  const sent = recordSent(page);

  await page.goto(origin + '/agents/' + name);
  // The chat is the agent's rendered view and its default; the Terminal is the
  // other half of the Rendered | Terminal switch.
  await expect(page.locator('#view-rendered')).toHaveAttribute('aria-current', 'page');
  const open = page.locator('#view-terminal');
  await expect(open).toBeVisible();
  await expect(open).toHaveText(/Terminal/);
  const ob = (await open.boundingBox())!;
  expect(ob.height, 'Terminal button is thumb-sized').toBeGreaterThanOrEqual(44);
  expect(ob.x + ob.width, 'Terminal button fits the viewport').toBeLessThanOrEqual(412);
  await noHorizontalScroll(page);

  // The switch opens the terminal in a frame over the chat (view_swap.go),
  // the address following it; its own page, opened at that address, is what
  // the checks below measure at phone width.
  await open.click();
  await page.waitForURL(origin + '/agents/' + name + '/terminal');
  await expect(page.frameLocator('iframe[data-muster-view]').locator('#term .xterm-rows')).toContainText('FAKE-TUI mode=ro');
  await page.goto(origin + '/agents/' + name + '/terminal');
  await expect(screen(page)).toContainText('FAKE-TUI mode=ro');
  await expect(screen(page)).toContainText('agent=' + name);
  await expect(page.locator('#term-status')).toHaveText('Connected');
  await expect(page.locator('#term-mode')).toHaveAttribute('aria-checked', 'false');
  await expect(page.locator('#term-mode-label')).toHaveText('Live keys off');

  // The bar has every key a phone keyboard lacks, all disabled while read-only.
  const keys = page.locator('#term-keys [data-key]');
  await expect(keys).toHaveCount(12);
  for (const k of ['esc', 'tab', 'stab', 'ctrlc', 'enter', 'up', 'down', 'left', 'right', '1', '2', '3']) {
    await expect(page.locator(`#term-keys [data-key="${k}"]`)).toBeDisabled();
  }

  // Fits: the terminal, the header and the bar are inside 412px, nothing scrolls
  // sideways, the keys are thumb-sized, and the terminal got a size from the
  // browser that matches its box.
  await noHorizontalScroll(page);
  for (const sel of ['#term', '#term .xterm-screen', '#term-keys', 'header']) {
    const b = (await page.locator(sel).first().boundingBox())!;
    expect(b.x, `${sel} left edge`).toBeGreaterThanOrEqual(0);
    expect(b.x + b.width, `${sel} right edge`).toBeLessThanOrEqual(412);
  }
  for (const sel of ['[data-key="up"]', '[data-key="down"]', '[data-key="enter"]', '#term-keys-more']) {
    const b = (await page.locator(`#term-keys ${sel}`).boundingBox())!;
    expect(b.height, `${sel} key height`).toBeGreaterThanOrEqual(44);
  }
  await expect(page.locator('#term-keys-more'), 'the ⋯ is disabled while read-only too').toBeDisabled();
  // The bar is one row (↑ ↓ ⏎ ⋯), and the terminal gets the rest of the screen —
  // measured failing when the stylesheet lacked the layout: the bar stacked and
  // squeezed the terminal to 18 rows.
  const bar = (await page.locator('#term-keys').boundingBox())!;
  expect(bar.height, 'quick-key bar height').toBeLessThanOrEqual(130);
  const termBox = (await page.locator('#term').boundingBox())!;
  expect(termBox.height, 'terminal height at 915px').toBeGreaterThanOrEqual(915 * 0.6);
  const size = await screen(page).innerText();
  const m = /size=(\d+)x(\d+)/.exec(size);
  expect(m, `a size line on screen: ${size}`).not.toBeNull();
  const cols = Number(m![1]);
  const termWidth = (await page.locator('#term').boundingBox())!.width;
  expect(cols, 'columns at 412px').toBeGreaterThanOrEqual(30);
  expect(Number(m![2]), 'rows at 915px').toBeGreaterThanOrEqual(30);
  const cellWidth = await page.evaluate(() => {
    const el = document.querySelector('#term .xterm-screen') as HTMLElement;
    return el.getBoundingClientRect().width;
  });
  expect(cellWidth, 'the rendered grid fits its box').toBeLessThanOrEqual(termWidth);

  // Typing into a read-only terminal sends nothing.
  await page.locator('#term').click();
  await page.keyboard.type('zz');
  await page.keyboard.press('Enter');
  await page.waitForTimeout(300);
  expect(sent, 'binary frames sent while read-only').toEqual([]);
  await expect(screen(page)).not.toContainText('in=');
});

test('the switch makes it interactive and the quick-key bar types the right bytes', async ({ page }) => {
  await login(page);
  const name = await saveAgent(page, 'claude-code', 'e2e terminal: keys');
  const sent = recordSent(page);
  await page.goto(origin + '/agents/' + name + '/terminal');
  await expect(screen(page)).toContainText('FAKE-TUI mode=ro');

  await page.locator('#term-mode').click();
  await expect(page.locator('#term-mode')).toHaveAttribute('aria-checked', 'true');
  await expect(page.locator('#term-mode-label')).toHaveText('Live keys on');
  await expect(screen(page)).toContainText('FAKE-TUI mode=rw');
  await expect(page.locator('#term-status')).toHaveText('Connected · live keys');

  // Literal expected bytes, NOT read from the page's data attributes.
  const want: [string, string][] = [
    ['esc', '1B'],
    ['tab', '09'],
    ['stab', '1B5B5A'],
    ['ctrlc', '03'],
    ['enter', '0D'],
    ['up', '1B5B41'],
    ['down', '1B5B42'],
    ['left', '1B5B44'],
    ['right', '1B5B43'],
    ['1', '31'],
    ['2', '32'],
    ['3', '33'],
  ];
  const menu = page.locator('#term-keys-menu');
  for (const [key, hex] of want) {
    const before = sent.length;
    // ↑ ↓ ⏎ are on the bar; every other key is in the ⋯ menu.
    const large = ['up', 'down', 'enter'].includes(key);
    if (!large) {
      await page.locator('#term-keys-more').click();
      await expect(menu).toBeVisible();
    }
    await page.locator(`#term-keys [data-key="${key}"]`).click();
    if (!large) await expect(menu, `the menu closes after ${key}`).toBeHidden();
    // The page sent exactly these bytes, and the far side received them.
    await expect.poll(() => sent.slice(before), { message: `${key} frame` }).toEqual([hex]);
    await expect(screen(page)).toContainText('in=' + hex);
  }

  // Typed text goes through too once interactive.
  await page.locator('#term').click();
  await page.keyboard.type('ok');
  await expect(screen(page)).toContainText(/in=6F6B|in=6F/);

  // And back: read-only again, keys disabled, a fresh read-only attach.
  await page.locator('#term-mode').click();
  await expect(page.locator('#term-mode')).toHaveAttribute('aria-checked', 'false');
  await expect(screen(page)).toContainText('FAKE-TUI mode=ro');
  await expect(page.locator('#term-keys [data-key="esc"]')).toBeDisabled();
  await noHorizontalScroll(page);
});

test('the terminal follows the viewport: keyboard-height and rotation', async ({ page }) => {
  await login(page);
  const name = await saveAgent(page, 'claude-code', 'e2e terminal: resize');
  await page.goto(origin + '/agents/' + name + '/terminal');
  await expect(screen(page)).toContainText('FAKE-TUI mode=ro');
  const sizes = async () =>
    [...(await screen(page).innerText()).matchAll(/size=(\d+)x(\d+)/g)].map((m) => [Number(m[1]), Number(m[2])]);
  await expect.poll(async () => (await sizes()).length).toBeGreaterThan(0);
  const [c0, r0] = (await sizes()).at(-1)!;

  // A soft keyboard takes ~40% of the height: fewer rows, same columns.
  await page.setViewportSize({ width: 412, height: 560 });
  const last = async (i: 0 | 1) => (await sizes()).at(-1)?.[i] ?? -1;
  await expect.poll(async () => { const r = await last(1); return r > 0 && r < r0; }, { message: 'fewer rows after the keyboard opens' }).toBe(true);
  expect(await last(0)).toBe(c0);
  await noHorizontalScroll(page);

  // Landscape: more columns.
  await page.setViewportSize({ width: 915, height: 412 });
  await expect.poll(() => last(0), { message: 'cols in landscape' }).toBeGreaterThan(c0);
  await noHorizontalScroll(page);
});

// RECORDED EXCLUSION (the operator's decision): drag-to-scroll is for mirrored
// windows only. An agent pod's Terminal page wires no scroll gesture — the
// page's own instrument (data-scroll-on, set when the handlers are wired; see
// mirrored.spec.ts, where it is) stays unset — and a real vertical touch drag
// and the mouse wheel post nothing to the scroll route.
test('an agent pod’s terminal wires no drag-to-scroll', async ({ page }) => {
  await login(page);
  const name = await saveAgent(page, 'claude-code', 'e2e terminal: no scroll');
  const posts: string[] = [];
  page.on('request', (r) => {
    if (new URL(r.url()).pathname.endsWith('/scroll')) posts.push(r.url());
  });
  await page.goto(origin + '/agents/' + name + '/terminal');
  await expect(screen(page)).toContainText('FAKE-TUI mode=ro');
  const term = page.locator('#term');
  await expect(term).not.toHaveAttribute('data-scroll', /.*/);
  await expect(term).not.toHaveAttribute('data-scroll-on', /.*/);
  await expect(page.locator('#term-scroll-note')).toHaveCount(0);
  const box = (await page.locator('#term-scroll').boundingBox())!;
  const mid = { x: box.x + box.width / 2, y: box.y + box.height / 2 };
  await touchDrag(page, { x: mid.x, y: mid.y - 120 }, { x: mid.x, y: mid.y + 120 });
  await touchDrag(page, { x: mid.x, y: mid.y + 120 }, { x: mid.x, y: mid.y - 120 });
  await page.waitForTimeout(300);
  expect(posts, 'an agent pod’s page posted no scroll').toEqual([]);
  await expect(screen(page)).not.toContainText('in=');
});

test('a standard agent has no terminal', async ({ page }) => {
  await login(page);
  const name = await saveAgent(page, 'gateway', 'e2e terminal: standard');
  await page.goto(origin + '/agents/' + name);
  await expect(page.locator('h1').first()).toBeVisible();
  await expect(page.locator('#view-terminal')).toHaveCount(0);
  const r = await page.request.get(origin + '/agents/' + name + '/terminal');
  expect(r.status()).toBe(404);
});

// THE LARGE KEYS AND THE ⋯ MENU (operator ask: "In the agent raw view show the
// up and down arrow keys and the return button. As large key buttons, and then
// collapse the rest into a three dots button context menu"), at a phone's
// width and a desktop's. Literal expected bytes, NOT read from the page.
const largeWant: [string, string][] = [['up', '1B5B41'], ['down', '1B5B42'], ['enter', '0D']];
const menuWant: [string, string, string][] = [
  ['esc', 'Escape', '1B'],
  ['tab', 'Tab', '09'],
  ['stab', 'Shift Tab', '1B5B5A'],
  ['ctrlc', 'Control C', '03'],
  ['1', '1', '31'],
  ['left', 'Left arrow', '1B5B44'],
  ['right', 'Right arrow', '1B5B43'],
  ['2', '2', '32'],
  ['3', '3', '33'],
];
for (const vp of [
  { name: 'phone 390×844 touch', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.name, () => {
    test.use(vp.use);
    test('↑ ↓ ⏎ are large keys and every other key is in the ⋯ menu, each sending its bytes', async ({ page }) => {
      await login(page);
      const name = await saveAgent(page, 'claude-code', 'e2e terminal: large keys ' + vp.name);
      const sent = recordSent(page);
      await page.addInitScript(clsInit);
      await page.goto(origin + '/agents/' + name + '/terminal');
      await expect(screen(page)).toContainText('FAKE-TUI mode=ro');
      await page.locator('#term-mode').click();
      await expect(page.locator('#term-status')).toHaveText('Connected · live keys');
      await expect(screen(page)).toContainText('FAKE-TUI mode=rw');

      // The bar: exactly ↑ ↓ ⏎ as its own keys, then the ⋯ — in that order.
      const barKeys = await page.locator('#term-keys > [data-key]').evaluateAll((els) => els.map((e) => e.getAttribute('data-key')));
      expect(barKeys, 'the bar\'s own keys').toEqual(['up', 'down', 'enter']);
      const menuBtn = page.locator('#term-keys-more');
      const mb = (await menuBtn.boundingBox())!;
      expect(Math.min(mb.width, mb.height), 'the ⋯ is a 44px target').toBeGreaterThanOrEqual(44);
      for (const [key] of largeWant) {
        const b = (await page.locator(`#term-keys [data-key="${key}"]`).boundingBox())!;
        expect(Math.min(b.width, b.height), `${key} is at least 44px both ways`).toBeGreaterThanOrEqual(44);
        expect(b.height, `${key} is a LARGE key (taller than the 44px minimum)`).toBeGreaterThanOrEqual(52);
        expect(b.x + b.width, `${key} inside the viewport`).toBeLessThanOrEqual(vp.use.viewport.width);
      }
      expect(mb.x + mb.width, 'the ⋯ inside the viewport').toBeLessThanOrEqual(vp.use.viewport.width);
      await noHorizontalScroll(page);

      // Each large key sends exactly its bytes.
      for (const [key, hex] of largeWant) {
        const before = sent.length;
        await page.locator(`#term-keys [data-key="${key}"]`).click();
        await expect.poll(() => sent.slice(before), { message: `${key} frame` }).toEqual([hex]);
        await expect(screen(page)).toContainText('in=' + hex);
      }

      // The ⋯ menu lists every other key, in order, by name, and fits the screen.
      const menu = page.locator('#term-keys-menu');
      await expect(menu).toBeHidden();
      await menuBtn.click();
      await expect(menu).toBeVisible();
      await expect(menuBtn).toHaveAttribute('aria-expanded', 'true');
      const items = menu.getByRole('menuitem');
      await expect(items).toHaveCount(menuWant.length);
      expect(await items.evaluateAll((els) => els.map((e) => e.getAttribute('data-key'))), 'the menu\'s keys, in order').toEqual(menuWant.map((w) => w[0]));
      const box = (await menu.boundingBox())!;
      expect(box.x, 'menu left edge').toBeGreaterThanOrEqual(0);
      expect(box.x + box.width, 'menu right edge').toBeLessThanOrEqual(vp.use.viewport.width);
      expect(box.y, 'menu top edge').toBeGreaterThanOrEqual(0);
      expect(box.y + box.height, 'the menu does not cover its ⋯').toBeLessThanOrEqual(mb.y + 0.5);
      for (const it of await items.all()) expect((await it.boundingBox())!.height, 'a menu key is a 44px target').toBeGreaterThanOrEqual(44);
      await noHorizontalScroll(page);
      await page.keyboard.press('Escape');
      await expect(menu).toBeHidden();

      // Each menu key, picked from a freshly opened menu, sends exactly its
      // bytes and closes the menu.
      for (const [key, label, hex] of menuWant) {
        const before = sent.length;
        await menuBtn.click();
        await expect(menu).toBeVisible();
        await menu.getByRole('menuitem', { name: label, exact: true }).click();
        await expect.poll(() => sent.slice(before), { message: `${key} frame` }).toEqual([hex]);
        await expect(screen(page)).toContainText('in=' + hex);
        await expect(menu, `the menu closes after ${key}`).toBeHidden();
      }
      await noHorizontalScroll(page);

      // Live keys off: the ⋯ and every key are disabled again.
      await page.locator('#term-mode').click();
      await expect(page.locator('#term-mode')).toHaveAttribute('aria-checked', 'false');
      await expect(menuBtn).toBeDisabled();
      await expect(page.locator('#term-keys [data-key="up"]')).toBeDisabled();

      await settle(page);
      const r = await readCls(page);
      expect(r.unsupported, 'layout-shift is observable here').toBeUndefined();
      expect(r.total, `shifts: ${JSON.stringify(r.shifts)}`).toBeLessThan(0.01);
      // The positive control: a shift the page did not ask for IS counted.
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

// 🔴 NO POPOVER API (iOS Safari < 17): the ⋯ must still open its menu, or the
// nine menu keys — ^C among them — are unreachable. The API is stubbed away
// before the page's scripts run; the ⋯ then toggles the menu itself, placed by
// the floating-menu rule, closed by a pick, a tap outside, or Escape. Red
// before the fallback: the ⋯ was a bare popovertarget, which (once the page
// stops relying on the native attribute) opens nothing.
test.describe('without the Popover API (390×844 touch)', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });
  test('the ⋯ still opens the menu and a menu key sends its bytes', async ({ page }) => {
    await login(page);
    const name = await saveAgent(page, 'claude-code', 'e2e terminal: no popover');
    const sent = recordSent(page);
    await page.addInitScript(() => {
      const p = HTMLElement.prototype as any;
      delete p.togglePopover;
      delete p.showPopover;
      delete p.hidePopover;
    });
    await page.goto(origin + '/agents/' + name + '/terminal');
    expect(await page.evaluate(() => typeof (HTMLElement.prototype as any).togglePopover), 'the API is stubbed away').toBe('undefined');
    await expect(screen(page)).toContainText('FAKE-TUI mode=ro');
    await page.locator('#term-mode').click();
    await expect(page.locator('#term-status')).toHaveText('Connected · live keys');

    const more = page.locator('#term-keys-more');
    const menu = page.locator('#term-keys-menu');
    // The fallback is engaged: no native popover wiring left to do the work.
    await expect(more).not.toHaveAttribute('popovertarget', /.*/);
    await expect(menu).toBeHidden();
    await more.click();
    await expect(menu).toBeVisible();
    await expect(more).toHaveAttribute('aria-expanded', 'true');
    await expect(menu.getByRole('menuitem')).toHaveCount(9);
    const mb = (await more.boundingBox())!;
    const box = (await menu.boundingBox())!;
    expect(box.x, 'menu left edge').toBeGreaterThanOrEqual(0);
    expect(box.x + box.width, 'menu right edge').toBeLessThanOrEqual(390);
    expect(box.y + box.height, 'placed above its ⋯, not over it').toBeLessThanOrEqual(mb.y + 0.5);
    await noHorizontalScroll(page);

    // ^C, picked from the menu: exactly 0x03, and the menu closes.
    let before = sent.length;
    await menu.getByRole('menuitem', { name: 'Control C', exact: true }).click();
    await expect.poll(() => sent.slice(before), { message: '^C frame' }).toEqual(['03']);
    await expect(screen(page)).toContainText('in=03');
    await expect(menu).toBeHidden();
    await expect(more).toHaveAttribute('aria-expanded', 'false');

    // A tap outside closes it; so does Escape; so does the ⋯ again.
    await more.click();
    await expect(menu).toBeVisible();
    await page.locator('#term-status').click();
    await expect(menu, 'a tap outside closes it').toBeHidden();
    await more.click();
    await expect(menu).toBeVisible();
    await page.keyboard.press('Escape');
    await expect(menu, 'Escape closes it').toBeHidden();
    await more.click();
    await expect(menu).toBeVisible();
    await more.click();
    await expect(menu, 'the ⋯ again closes it').toBeHidden();

    // A large key still sends its bytes in this mode.
    before = sent.length;
    await page.locator('#term-keys [data-key="up"]').click();
    await expect.poll(() => sent.slice(before), { message: 'up frame' }).toEqual(['1B5B41']);
  });
});
