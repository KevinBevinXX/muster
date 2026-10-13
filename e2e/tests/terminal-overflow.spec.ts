import { request as pwRequest, CDPSession, Page, WebSocket as PWWebSocket } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { hookToken, overflowPort, password, versionTerm } from '../lib/env';
import { startServer, Running } from '../lib/server';

// A terminal whose far side is WIDER than the page (internal/termsize): the
// session keeps its size, the page renders it in a scrolling, pinch-zoomable
// view with "Fit width", and a phone never sends a size that would shrink it.
//
// The far side is the e2eterm fake pod with a 120x30 session window
// (MUSTER_E2E_TERM_WINDOW): it answers muster's window-size query as tmux would,
// prints `size=CxR` for every size that reaches the exec, and draws a ruler that
// ends exactly at the client's last column with RIGHT-EDGE. Everything between
// — agentprovision's no-shrink rule, the in-band report, the socket bridge, the
// page — is the shipped code.
//
// 🔴 120 COLUMNS ON PURPOSE: wider than 412px (≈52 columns) and narrower than
// 1280px (≈160), so the same server answers both the phone case (overflow) and
// the desktop case (fit, unchanged).

const origin = `http://localhost:${overflowPort}`;
const WIN = { cols: 120, rows: 30 };
let server: Running | undefined;

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  server = await startServer(versionTerm, overflowPort, {
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
    MUSTER_AGENT_KINDS: 'gateway,claude-code',
    MUSTER_AGENT_CC_IMAGE: 'ghcr.io/example-org/claude-code-agent:e2e',
    MUSTER_AGENT_CC_ACCOUNTS: 'alpha',
    MUSTER_AGENT_CC_TOKEN_ALPHA: 'sk-ant-oat01-FAKE-e2e-overflow-alpha',
    MUSTER_E2E_TERM_WINDOW: `${WIN.cols}x${WIN.rows}`,
  });
});
test.afterAll(async () => {
  await server?.stop();
});

async function login(page: Page) {
  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
}

async function agentNames(): Promise<Set<string>> {
  const ctx = await pwRequest.newContext({ baseURL: origin, extraHTTPHeaders: { Authorization: `Bearer ${hookToken}` } });
  const r = await ctx.get('/api/agents');
  expect(r.status(), await r.text()).toBe(200);
  const rows = (await r.json()) as { name: string }[];
  await ctx.dispose();
  return new Set(rows.map((a) => a.name));
}

// A claude-code agent created through the API's own door (the hook token), so
// the dispatch sheet's layout at each width is not part of this spec.
async function newAgent(page: Page, note: string): Promise<string> {
  const before = await agentNames();
  await page.goto(origin + '/agents');
  await page.click('#fab-agents');
  const sheet = page.locator('#agent-modal form');
  await sheet.locator('[data-kind-option="claude-code"]').click();
  await sheet.locator('textarea[name="note_text"]').fill(note);
  const resp = page.waitForResponse((r) => r.url().endsWith('/agents') && r.request().method() === 'POST');
  await sheet.locator('button[value="save"]').click();
  expect((await resp).status()).toBe(200);
  const created = [...(await agentNames())].filter((n) => !before.has(n));
  expect(created.length, 'exactly one agent created').toBe(1);
  return created[0];
}

// Every {"type":"resize"} the page SENT on the terminal socket.
function recordResizes(page: Page): string[] {
  const sent: string[] = [];
  page.on('websocket', (ws: PWWebSocket) => {
    if (!ws.url().includes('/terminal/ws')) return;
    ws.on('framesent', (f) => {
      if (typeof f.payload === 'string' && f.payload.includes('"resize"')) sent.push(f.payload);
    });
  });
  return sent;
}

const screen = (page: Page) => page.locator('#term .xterm-rows');
const sizeLines = async (page: Page) => [...(await screen(page).innerText()).matchAll(/size=(\d+)x(\d+)/g)].map((m) => `${m[1]}x${m[2]}`);

// The on-screen rectangle of the RIGHT-EDGE marker (the session's last
// columns), measured on the text itself.
async function markerRect(page: Page) {
  return page.evaluate(() => {
    const rows = document.querySelector('#term .xterm-rows')!;
    const walker = document.createTreeWalker(rows, NodeFilter.SHOW_TEXT);
    for (let n = walker.nextNode(); n; n = walker.nextNode()) {
      const i = (n.textContent || '').indexOf('RIGHT-EDGE');
      if (i < 0) continue;
      const r = document.createRange();
      r.setStart(n, i);
      r.setEnd(n, i + 'RIGHT-EDGE'.length);
      const b = r.getBoundingClientRect();
      return { left: b.left, right: b.right };
    }
    return null;
  });
}

// pinch is a real two-finger gesture (CDP touch events) centred on the
// terminal, the fingers moving from `from` to `to` px either side of centre.
// (Input.synthesizePinchGesture was measured NOT to zoom in headless Chromium;
// raw touch points do, through the browser's own gesture recogniser.)
async function pinch(cdp: CDPSession, from: number, to: number) {
  const cx = 206, cy = 420;
  const pts = (d: number) => [{ x: cx - d, y: cy, id: 0 }, { x: cx + d, y: cy, id: 1 }];
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: pts(from) });
  const step = from < to ? 10 : -10;
  for (let d = from; from < to ? d <= to : d >= to; d += step) {
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: pts(d) });
  }
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
}

async function noDocumentScroll(page: Page) {
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw, 'the DOCUMENT never scrolls sideways — only the terminal view does').toBeLessThanOrEqual(cw);
}

test.describe('at a phone’s width (412px)', () => {
  test.use({ viewport: { width: 412, height: 915 }, hasTouch: true, isMobile: true });

  test('a wider session renders at its own size, scrolls to its right edge, pinch-zooms and fits width — and is never resized', async ({ page }) => {
    await login(page);
    const name = await newAgent(page, 'e2e overflow: phone');
    const resizes = recordResizes(page);
    await page.goto(origin + '/agents/' + name + '/terminal');
    await expect(screen(page)).toContainText('FAKE-TUI mode=ro');

    // The far side kept its window and told the page; the exec got 120x30,
    // not the phone's ~52 columns.
    await expect(page.locator('#term')).toHaveAttribute('data-remote', `${WIN.cols}x${WIN.rows}`);
    await expect.poll(() => sizeLines(page)).toEqual([`${WIN.cols}x${WIN.rows}`]);
    await expect(page.locator('#term')).toHaveAttribute('data-overflow', '1');
    await expect(page.locator('#term-fitw')).toBeVisible();
    await noDocumentScroll(page);

    // The marker starts OFF the right of the screen…
    await expect.poll(async () => (await markerRect(page))?.left ?? -1).toBeGreaterThan(412);
    // …and the view scrolls all the way to it.
    const scrolled = await page.locator('#term-scroll').evaluate((el) => {
      el.scrollLeft = el.scrollWidth;
      return { left: el.scrollLeft, max: el.scrollWidth - el.clientWidth };
    });
    expect(scrolled.max, 'the view is scrollable sideways').toBeGreaterThan(300);
    expect(scrolled.left, 'scrolled to the far end').toBe(scrolled.max);
    const atEdge = (await markerRect(page))!;
    expect(atEdge.right, 'RIGHT-EDGE is on screen once scrolled').toBeLessThanOrEqual(412);
    expect(atEdge.left).toBeGreaterThanOrEqual(0);
    await page.locator('#term-scroll').evaluate((el) => (el.scrollLeft = 0));

    // PINCH. Positive control first: the page really is zoomed afterwards,
    // and visualViewport really fired — or "no resize" would prove nothing.
    await page.evaluate(() => {
      (window as unknown as { __vv: number }).__vv = 0;
      window.visualViewport!.addEventListener('resize', () => ((window as unknown as { __vv: number }).__vv += 1));
    });
    const layoutsBefore = await page.locator('#term').getAttribute('data-layouts');
    const cdp = await page.context().newCDPSession(page);
    await pinch(cdp, 30, 140); // fingers apart: zoom in, on the terminal itself
    await expect.poll(() => page.evaluate(() => window.visualViewport!.scale), { message: 'the pinch zoomed the page' }).toBeGreaterThan(1.5);
    expect(await page.evaluate(() => (window as unknown as { __vv: number }).__vv), 'visualViewport fired resize during the pinch').toBeGreaterThan(0);
    await page.waitForTimeout(500);
    expect(resizes, 'resize messages sent by a pinch').toEqual([]);
    expect(await page.locator('#term').getAttribute('data-layouts'), 'the pinch did not re-lay out (no refit on zoom)').toBe(layoutsBefore);
    expect(await sizeLines(page)).toEqual([`${WIN.cols}x${WIN.rows}`]);
    await pinch(cdp, 140, 10); // fingers together: back out
    await expect.poll(() => page.evaluate(() => window.visualViewport!.scale)).toBeLessThan(1.1);
    expect(resizes, 'resize messages sent by pinching back out').toEqual([]);

    // FIT WIDTH: the whole width on screen, nothing scrolled, nothing sent.
    await page.locator('#term-fitw').click();
    await expect(page.locator('#term-fitw')).toHaveAttribute('aria-pressed', 'true');
    await expect.poll(async () => (await markerRect(page))?.right ?? 9999, { message: 'RIGHT-EDGE inside 412px under Fit width' }).toBeLessThanOrEqual(412);
    expect(await page.locator('#term-scroll').evaluate((el) => el.scrollWidth - el.clientWidth), 'nothing left to scroll sideways').toBeLessThanOrEqual(1);
    await page.locator('#term-fitw').click();
    await expect(page.locator('#term-fitw')).toHaveAttribute('aria-pressed', 'false');
    await expect.poll(async () => (await markerRect(page))?.left ?? -1).toBeGreaterThan(412);
    expect(resizes, 'resize messages sent by Fit width').toEqual([]);

    // A rotation-sized change is a REAL layout change, but the page still does
    // not cover the session, so it still sends nothing.
    await page.setViewportSize({ width: 600, height: 915 });
    await page.waitForTimeout(400);
    expect(resizes, 'a still-narrower-than-the-session viewport sends nothing').toEqual([]);
    await noDocumentScroll(page);
  });

  test('interactive on a phone does not shrink the session either', async ({ page }) => {
    await login(page);
    const name = await newAgent(page, 'e2e overflow: phone rw');
    const resizes = recordResizes(page);
    await page.goto(origin + '/agents/' + name + '/terminal');
    await expect(screen(page)).toContainText('FAKE-TUI mode=ro');
    await page.locator('#term-mode').click();
    await expect(screen(page)).toContainText('FAKE-TUI mode=rw');
    await expect(page.locator('#term')).toHaveAttribute('data-remote', `${WIN.cols}x${WIN.rows}`);
    await expect.poll(() => sizeLines(page)).toEqual([`${WIN.cols}x${WIN.rows}`]);
    await page.waitForTimeout(300);
    expect(resizes).toEqual([]);
  });
});

test.describe('at a desktop width (1280px)', () => {
  test.use({ viewport: { width: 1280, height: 900 } });

  test('a page wider than the session fits its container and grows it, as before; narrowing scrolls instead of shrinking', async ({ page }) => {
    await login(page);
    const name = await newAgent(page, 'e2e overflow: desktop');
    const resizes = recordResizes(page);
    await page.goto(origin + '/agents/' + name + '/terminal');
    await expect(screen(page)).toContainText('FAKE-TUI mode=ro');

    // The page covers 120x30, so it asks for its own fit and gets it.
    await expect.poll(async () => (await page.locator('#term').getAttribute('data-remote')) ?? '').not.toBe('');
    const fitted = (await page.locator('#term').getAttribute('data-remote'))!;
    const [fc, fr] = fitted.split('x').map(Number);
    expect(fc, 'the desktop fit is wider than the session').toBeGreaterThan(WIN.cols);
    expect(fr).toBeGreaterThanOrEqual(WIN.rows);
    await expect.poll(() => sizeLines(page)).toEqual([fitted]);
    await expect(page.locator('#term')).toHaveAttribute('data-overflow', '0');
    await expect(page.locator('#term-fitw')).toBeHidden();
    expect(await page.locator('#term-scroll').evaluate((el) => el.scrollWidth - el.clientWidth), 'no sideways scroll on a desktop that fits').toBeLessThanOrEqual(1);
    const edge = (await markerRect(page))!;
    expect(edge.right, 'the ruler (the client width) ends inside the page').toBeLessThanOrEqual(1280);

    // Wider: the page grows the session, as it always did.
    await page.setViewportSize({ width: 1600, height: 900 });
    await expect.poll(() => resizes.length, { message: 'a wider desktop sends its new size' }).toBeGreaterThan(0);
    await expect.poll(async () => (await sizeLines(page)).length).toBe(2);
    const grown = (await sizeLines(page))[1];
    expect(Number(grown.split('x')[0])).toBeGreaterThan(fc);

    // Narrower than what it grew: the session is not shrunk — the view scrolls.
    const sentBefore = resizes.length;
    await page.setViewportSize({ width: 1100, height: 900 });
    await expect(page.locator('#term')).toHaveAttribute('data-overflow', '1');
    await expect(page.locator('#term-fitw')).toBeVisible();
    await page.waitForTimeout(400);
    expect(resizes.length, 'narrowing below the session sends nothing').toBe(sentBefore);
    expect(await sizeLines(page)).toEqual([fitted, grown]);
    await noDocumentScroll(page);
    // Fit width on a desktop, too: the whole session width in view.
    await page.locator('#term-fitw').click();
    await expect.poll(async () => (await markerRect(page))?.right ?? 9999).toBeLessThanOrEqual(1100);
    expect(resizes.length, 'Fit width sends nothing').toBe(sentBefore);
  });
});
