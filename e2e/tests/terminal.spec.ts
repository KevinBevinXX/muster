import { request as pwRequest, Page, WebSocket as PWWebSocket } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { hookToken, password, termPort, versionTerm } from '../lib/env';
import { startServer, Running } from '../lib/server';

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

  await open.click();
  await page.waitForURL(origin + '/agents/' + name + '/terminal');
  await expect(screen(page)).toContainText('FAKE-TUI mode=ro');
  await expect(screen(page)).toContainText('agent=' + name);
  await expect(page.locator('#term-status')).toHaveText('Connected · read-only');
  await expect(page.locator('#term-mode')).toHaveAttribute('aria-checked', 'false');
  await expect(page.locator('#term-mode-label')).toHaveText('Read-only');

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
  for (const k of ['esc', 'up', '3']) {
    const b = (await page.locator(`#term-keys [data-key="${k}"]`).boundingBox())!;
    expect(b.height, `${k} key height`).toBeGreaterThanOrEqual(44);
  }
  // The bar is two rows of six, and the terminal gets the rest of the screen —
  // measured failing when the stylesheet lacked the grid: the bar stacked and
  // squeezed the terminal to 18 rows.
  const bar = (await page.locator('#term-keys').boundingBox())!;
  expect(bar.height, 'quick-key bar height (two rows)').toBeLessThanOrEqual(130);
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
  await expect(page.locator('#term-mode-label')).toHaveText('Interactive');
  await expect(screen(page)).toContainText('FAKE-TUI mode=rw');
  await expect(page.locator('#term-status')).toHaveText('Connected · interactive');

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
  for (const [key, hex] of want) {
    const before = sent.length;
    await page.locator(`#term-keys [data-key="${key}"]`).click();
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

test('a standard agent has no terminal', async ({ page }) => {
  await login(page);
  const name = await saveAgent(page, 'gateway', 'e2e terminal: standard');
  await page.goto(origin + '/agents/' + name);
  await expect(page.locator('h1').first()).toBeVisible();
  await expect(page.locator('#view-terminal')).toHaveCount(0);
  const r = await page.request.get(origin + '/agents/' + name + '/terminal');
  expect(r.status()).toBe(404);
});
