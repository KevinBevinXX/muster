import { FrameLocator, Page, request as pwRequest } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { hookToken, sidenavPort } from '../lib/env';
import { cc, MirrorRig } from '../lib/mirrorhost';

// THE SIDE NAV ON EVERY SIGNED-IN PAGE (internal/ui/nav_layout.go), opened by
// its own toggle in a real browser, at a phone's size and a desktop's:
//
//   - the server-authored pages (/attention, /account, /plugins, /flows) carry
//     it, mark the entry they belong to, and keep that mark after the page's
//     script has run (no fallback onto Tasks);
//   - the mirrored Rendered view and the terminal carry it as an OVERLAY at
//     both sizes — its hamburger opens it, and nothing on the page moves;
//   - after a Rendered ↔ Terminal switch the view on screen is the FRAME, laid
//     over the whole page (view_swap.go), so the nav reachable then is the
//     frame's own: it opens, it is on top of everything, a link in it takes
//     the TOP window, and a theme change made in it is followed by the page
//     under it — in both directions, for a mirrored window and an agent.
//
// Its own server: the e2eterm binary with the mirror plugin, the plugin
// interface (/plugins is served only then) and claude-code agents on, and one
// per-window mirrored host whose two windows run Claude Code.

test.use({ storageState: { cookies: [], origins: [] } });

const rig = new MirrorRig(sidenavPort, 'e2e-nav', {
  MUSTER_PLUGINS: '1',
  MUSTER_AGENT_KINDS: 'gateway,claude-code',
  MUSTER_AGENT_CC_IMAGE: 'ghcr.io/example-org/claude-code-agent:e2e',
  MUSTER_AGENT_CC_ACCOUNTS: 'alpha',
  MUSTER_AGENT_CC_TOKEN_ALPHA: 'sk-ant-oat01-FAKE-e2e-nav-alpha',
  MUSTER_AGENT_GATEWAY: 'hooks-sha256',
  MUSTER_AGENT_GATEWAY_MODEL: 'e2e-sentinel',
  MUSTER_AGENT_RUNTIME_CONFIG: '{}',
  MUSTER_AGENT_RUNTIME_INSTALL: 'exec true',
});
const origin = rig.origin;
const q = (win: string) => `host=${encodeURIComponent(rig.host)}&session=work&window=${encodeURIComponent(win)}`;
const viewOf = (win: string) => `/mirror/view?${q(win)}`;
const termOf = (win: string) => `/mirror/terminal?${q(win)}`;
const pathOf = (page: Page) => new URL(page.url()).pathname;

test.beforeAll(async () => {
  test.setTimeout(180_000);
  await rig.start();
  const one = rig.transcript(cc.prompt('the prompt of window ONE') + cc.reply('Window one answered.'), 'one');
  const two = rig.transcript(cc.prompt('the prompt of window TWO') + cc.reply('Window two answered.'), 'two');
  const now = Math.floor(Date.now() / 1000);
  const windows = rig.transcript([`work|@1|1|1|claude|${now - 60}|${one}`, `work|@2|2|0|claude|${now - 120}|${two}`].join('\n') + '\n', 'windows');
  await rig.startHost({ MUSTER_MIRROR_INTERACTIVE: '1', MUSTER_MIRROR_E2E_WINDOWS_FILE: windows, MUSTER_MIRROR_E2E_REFRESH: '300ms' });
});

test.afterAll(async () => {
  await rig.stop();
});

// A document's nav, in the page itself or in the view-swap frame.
type Doc = Page | FrameLocator;
const root = (d: Doc) => d.locator('#sidebar-root');
const panel = (d: Doc) => d.locator('#sidebar');
const toggle = (d: Doc) => d.locator('#sidebar-open');
const frameEl = (page: Page) => page.locator('iframe[data-muster-view]');
const framed = (page: Page) => page.frameLocator('iframe[data-muster-view]');

// openByToggle opens the nav with its hamburger and checks it is on screen,
// then closes it with its own close control.
async function openByToggle(d: Doc): Promise<void> {
  await expect(root(d)).toHaveAttribute('data-open', 'false');
  await expect(panel(d), 'closed, the panel is off screen').not.toBeInViewport();
  await toggle(d).click();
  await expect(root(d)).toHaveAttribute('data-open', 'true');
  await expect(panel(d)).toBeInViewport({ ratio: 0.99 });
  await expect(toggle(d)).toHaveAttribute('aria-expanded', 'true');
  await d.locator('#sidebar-close').click();
  await expect(root(d)).toHaveAttribute('data-open', 'false');
  await expect(panel(d)).not.toBeInViewport();
}

// navOnTopInFrame: the framed view's nav is open (by its hamburger, or as a
// desktop rail) and is what the TOP document has on screen at the nav's middle
// — the frame, nothing of the page under it drawn over it — and, inside the
// frame, the nav itself is the topmost element there.
async function navOnTopInFrame(page: Page, rail: boolean): Promise<void> {
  const f = framed(page);
  await expect(frameEl(page)).toBeVisible();
  if (rail) {
    await expect(toggle(f), 'a desktop rail: no hamburger').toBeHidden();
  } else {
    await toggle(f).click();
    await expect(root(f)).toHaveAttribute('data-open', 'true');
  }
  await expect(panel(f)).toBeInViewport({ ratio: 0.99 });
  const b = (await panel(f).boundingBox())!;
  const x = b.x + b.width / 2, y = b.y + b.height / 2;
  const topHit = await page.evaluate(([x, y]) => (document.elementFromPoint(x, y) as Element | null)?.tagName ?? '', [x, y]);
  expect(topHit, 'at the nav, the top document shows the frame').toBe('IFRAME');
  const handle = await frameEl(page).elementHandle();
  const frame = (await handle!.contentFrame())!;
  const inNav = await frame.evaluate(([x, y]) => !!document.elementFromPoint(x, y)?.closest('#sidebar'), [x, y]);
  expect(inNav, 'inside the frame, the nav is the topmost thing at its middle').toBe(true);
}

// marks asserts which nav link the page marks as its own — the accent and
// aria-current — after the page's own script has run, and that no other link
// (Tasks in particular, the script's old fallback) is marked.
async function marks(d: Doc, sel: string | null): Promise<void> {
  // The script has run: it binds the hamburger.
  await expect(toggle(d)).toHaveAttribute('data-cg-bound', '1');
  const current = panel(d).locator('[aria-current="page"]');
  const accented = panel(d).locator('a.bg-accent\\/15');
  if (sel === null) {
    await expect(current).toHaveCount(0);
    await expect(accented).toHaveCount(0);
    return;
  }
  const own = panel(d).locator(sel);
  await expect(own).toHaveCount(1);
  await expect(current).toHaveCount(1);
  await expect(own).toHaveAttribute('aria-current', 'page');
  await expect(accented).toHaveCount(1);
  await expect(own).toHaveClass(/(^|\s)bg-accent\/15(\s|$)/);
}

// The pages internal/api writes, and the entry each belongs to (a selector
// inside the nav), or null for a page no entry names.
const authored: { path: string; entry: string | null; content: string }[] = [
  { path: '/attention', entry: '#sidebar-attention', content: '[data-testid="attention-page"]' },
  { path: '/account', entry: '#sidebar-account', content: 'main h1:has-text("Account")' },
  { path: '/plugins', entry: null, content: 'main h1' },
  { path: '/flows', entry: null, content: '[data-testid="flows-page"]' },
];

// box is an element's place on the page, for "nothing moved".
async function box(page: Page, sel: string) {
  const b = await page.locator(sel).boundingBox();
  expect(b, `${sel} is drawn`).not.toBeNull();
  return b!;
}

async function bothWindowsListed(page: Page): Promise<void> {
  await rig.login(page);
  await expect.poll(() => rig.log.includes('connected to '), { message: `the host connected (log:\n${rig.log})`, timeout: 20_000 }).toBe(true);
  await expect
    .poll(async () => ((await (await page.request.get(origin + '/ui/agents')).text()).match(/data-mirror-claude/g) || []).length, {
      message: `both windows listed as Claude Code (log:\n${rig.log})`,
      timeout: 20_000,
    })
    .toBe(2);
}

// The claude-code agent the agent case switches on, made once.
let agent = '';
async function ccAgent(page: Page): Promise<string> {
  if (agent) return agent;
  const names = async () => {
    const ctx = await pwRequest.newContext({ baseURL: origin, extraHTTPHeaders: { Authorization: `Bearer ${hookToken}` } });
    const r = await ctx.get('/api/agents');
    expect(r.status(), await r.text()).toBe(200);
    const rows = (await r.json()) as { name: string }[];
    await ctx.dispose();
    return new Set(rows.map((a) => a.name));
  };
  const before = await names();
  await page.goto(origin + '/agents');
  await page.click('#fab-agents');
  const sheet = page.locator('#agent-modal form');
  await sheet.locator('[data-kind-option="claude-code"]').click();
  await sheet.locator('textarea[name="note_text"]').fill('e2e side nav');
  const resp = page.waitForResponse((r) => r.url().endsWith('/agents') && r.request().method() === 'POST');
  await sheet.locator('button[value="save"]').click();
  expect((await resp).status()).toBe(200);
  const created = [...(await names())].filter((n) => !before.has(n));
  expect(created.length, 'exactly one agent created').toBe(1);
  agent = created[0];
  return agent;
}

for (const size of [
  { name: 'phone', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop', use: { viewport: { width: 1280, height: 800 } } },
]) {
  test.describe(size.name, () => {
    test.use(size.use);
    const desktop = size.name === 'desktop';

    test('the server-authored pages carry the nav, opened by its toggle, marking their own entry', async ({ page }) => {
      await rig.login(page);
      for (const p of authored) {
        await page.goto(origin + p.path);
        await expect(page.locator(p.content).first(), `${p.path} drew its own content`).toBeVisible();
        await marks(page, p.entry);
        if (desktop) {
          // A desktop's rail: in view without a toggle, which is hidden.
          await expect(panel(page), `${p.path}: the rail is in view`).toBeInViewport({ ratio: 0.99 });
          await expect(toggle(page)).toBeHidden();
          // The content column clears the rail.
          const rail = await box(page, '#sidebar');
          const main = await box(page, 'main');
          expect(main.x, `${p.path}: the content starts right of the rail`).toBeGreaterThanOrEqual(rail.x + rail.width);
        } else {
          await openByToggle(page);
        }
      }
      // A link in it leaves the page: to the board.
      if (!desktop) await toggle(page).click();
      await panel(page).locator('a[data-tab="tasks"]').click();
      await expect.poll(() => pathOf(page)).toBe('/tasks');
      await expect(page.locator('#panel-tasks')).toBeVisible();
    });

    test('a mirrored window: the nav on both views and in the frame after a switch, either way round', async ({ page }) => {
      await bothWindowsListed(page);

      // RENDERED, on its own: the overlay, and nothing on the page moves.
      await page.goto(origin + viewOf('@1'));
      await expect(page.locator('#mv-log [data-bubble="user"]').first()).toContainText('window ONE');
      await expect(page.locator('#mv-status')).toHaveText('Live');
      await expect(toggle(page), 'the hamburger is shown at this size too').toBeVisible();
      // A mirrored window hangs off the Agents list, as an agent's page does.
      await marks(page, 'a[data-tab="agents"]');
      const before = { log: await box(page, '#mv-log'), compose: await box(page, '#mv-compose') };
      await openByToggle(page);
      expect(await box(page, '#mv-log')).toEqual(before.log);
      expect(await box(page, '#mv-compose')).toEqual(before.compose);
      expect(before.compose.x, 'the pinned composer spans the window: no rail beside it').toBe(0);

      // RENDERED → TERMINAL: the terminal is the frame over everything; ITS nav
      // opens and is on top.
      await page.locator('#view-terminal').click();
      await expect(framed(page).locator('#term-status')).toHaveText('Connected');
      await navOnTopInFrame(page, false);
      // A theme change made in the frame's nav: the page under it follows.
      expect(await page.evaluate(() => document.documentElement.classList.contains('dark')), 'the page starts dark').toBe(true);
      await framed(page).locator('#sidebar [data-theme-toggle]').click();
      await expect.poll(() => page.evaluate(() => document.documentElement.classList.contains('dark')), { message: 'the page under the frame followed the theme the frame set' }).toBe(false);
      await framed(page).locator('#sidebar [data-theme-toggle]').click();
      await expect.poll(() => page.evaluate(() => document.documentElement.classList.contains('dark'))).toBe(true);
      await framed(page).locator('#sidebar-close').click();
      // Back: the page's own nav is the one on screen again, and still works.
      await framed(page).locator('#view-rendered').click();
      await expect(frameEl(page)).toBeHidden();
      await openByToggle(page);

      // TERMINAL on its own, then → RENDERED in the frame.
      await page.goto(origin + termOf('@1'));
      await expect(page.locator('#term-status')).toHaveText('Connected');
      await openByToggle(page);
      await page.locator('#view-rendered').click();
      await expect(framed(page).locator('#mv-status')).toHaveText('Live');
      await navOnTopInFrame(page, false);
      // A REAL nav link inside the frame takes the TOP window, and the frame
      // goes with the page it was over.
      await framed(page).locator('#sidebar a[data-tab="tasks"]').click();
      await expect.poll(() => pathOf(page)).toBe('/tasks');
      await expect(frameEl(page)).toHaveCount(0);
      await expect(page.locator('#panel-tasks')).toBeVisible();
    });

    test("a claude-code agent: from its terminal, the chat in the frame has a reachable nav", async ({ page }) => {
      await rig.login(page);
      const a = await ccAgent(page);
      await page.goto(origin + `/agents/${a}/terminal`);
      await expect(page.locator('#term .xterm-rows')).toContainText('FAKE-TUI mode=ro');
      await openByToggle(page);
      await page.locator('#view-rendered').click();
      await expect(framed(page).locator('#view-rendered')).toHaveAttribute('aria-current', 'page');
      // The agent page keeps the shell's rail on a desktop (it is not a
      // full-height view); on a phone its hamburger opens it.
      await navOnTopInFrame(page, desktop);
      await framed(page).locator('#sidebar a[data-tab="agents"]').click();
      await expect.poll(() => pathOf(page)).toBe('/agents');
      await expect(frameEl(page)).toHaveCount(0);
    });
  });
}

// 🔴 THE HAMBURGER COSTS THE PHONE NOTHING. At 390×844 the mirrored view's and
// the terminal's headers are the height they were before the nav (57px: one
// row; 105px: the terminal header already wraps to two there), the
// conversation keeps its 722px and the terminal its 37 rows. Measured on main
// (60e21bd1) with the same selectors. A number BELOW one of these is fine (the
// bound is a ceiling or a floor); one past it is the header wrapping.
test.describe('phone header', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });
  test('adding the hamburger wraps no header and takes no height from the log or the terminal', async ({ page }) => {
    await bothWindowsListed(page);
    await page.goto(origin + viewOf('@1'));
    await expect(page.locator('#mv-status')).toHaveText('Live');
    expect((await box(page, 'body > header')).height, 'the Rendered view header is one row').toBeLessThanOrEqual(57);
    expect((await box(page, '#mv-log')).height, 'the conversation keeps its height').toBeGreaterThanOrEqual(722);
    await page.goto(origin + termOf('@1'));
    await expect(page.locator('#term-status')).toHaveText('Connected');
    expect((await box(page, 'body > header')).height, 'the terminal header wraps no further').toBeLessThanOrEqual(105);
    await expect.poll(() => page.locator('#term .xterm-rows > div').count(), { message: 'the terminal keeps its rows' }).toBeGreaterThanOrEqual(37);
  });

  // An expired session on a mirrored view is now noticed by the nav's badge
  // poll too (appScript's auth gate turns its 401 into a reload, which lands on
  // sign-in). A half-typed message must survive that: it is kept as typed.
  test('a 401 on the badge poll reloads to sign-in, and the composer draft survives it', async ({ page }) => {
    await bothWindowsListed(page);
    await page.goto(origin + viewOf('@2'));
    await expect(page.locator('#mv-status')).toHaveText('Live');
    const draft = 'half-typed before the session expired ' + Date.now().toString(36);
    await page.locator('#mv-input').fill(draft);
    await expect
      .poll(() => page.evaluate((d) => Object.keys(localStorage).some((k) => (localStorage.getItem(k) || '').includes(d)), draft), { message: 'the draft is stored as typed' })
      .toBe(true);
    const cookies = await page.context().cookies(origin);
    await page.context().clearCookies();
    // The badge's refresh, now rather than at its minute: the same route into
    // the same slot through htmx, so the same auth gate sees its answer — 401.
    const badge = page.waitForResponse((r) => r.url().includes('/attention/badge'));
    await page.evaluate(() =>
      (window as unknown as { htmx: { ajax(m: string, u: string, o: object): void } }).htmx.ajax('GET', '/attention/badge', { target: '#attention-badge-slot', swap: 'innerHTML' }),
    );
    expect((await badge).status()).toBe(401);
    await expect.poll(() => pathOf(page), { message: 'the 401 reloaded the page onto sign-in' }).toBe('/login');
    await page.context().addCookies(cookies);
    await page.goto(origin + viewOf('@2'));
    await expect(page.locator('#mv-input')).toHaveValue(draft);
  });

  // An expired session while the other view is FRAMED: the frame's badge poll
  // gets the 401, and the auth gate reloads the TOP window — whose address is
  // the view on screen — onto sign-in; the frame never loads the refusal. (The
  // server half, for a framed load that does happen — a browser without the
  // top-reload, a reload the frame makes itself: a framed load is a document
  // navigation, TestAFramedLoadWithoutASessionIsSentToSignIn.)
  test('an expired session while a view is framed: the TOP window reloads onto sign-in; the frame never loads the refusal', async ({ page }) => {
    await bothWindowsListed(page);
    await page.goto(origin + viewOf('@1'));
    await expect(page.locator('#mv-status')).toHaveText('Live');
    await page.locator('#view-terminal').click();
    await expect(framed(page).locator('#term-status')).toHaveText('Connected');
    expect(pathOf(page), 'the top address follows the framed view').toBe('/mirror/terminal');
    const cookies = await page.context().cookies(origin);
    const topNavs: string[] = [];
    const frameNavs: string[] = [];
    page.on('request', (r) => {
      if (!r.isNavigationRequest()) return;
      (r.frame() === page.mainFrame() ? topNavs : frameNavs).push(new URL(r.url()).pathname);
    });
    await page.context().clearCookies();
    const handle = await frameEl(page).elementHandle();
    const frame = (await handle!.contentFrame())!;
    const badge = page.waitForResponse((r) => r.url().includes('/attention/badge'));
    await frame.evaluate(() =>
      (window as unknown as { htmx: { ajax(m: string, u: string, o: object): void } }).htmx.ajax('GET', '/attention/badge', { target: '#attention-badge-slot', swap: 'innerHTML' }),
    );
    expect((await badge).status()).toBe(401);
    await expect.poll(() => pathOf(page), { message: 'the top window is on sign-in' }).toBe('/login');
    await expect(page.locator('#muster-password')).toBeVisible();
    await expect(frameEl(page), 'no frame is left over the sign-in').toHaveCount(0);
    await expect(page.locator('body')).not.toContainText('not signed in');
    expect(topNavs[0], 'the TOP window reloaded the view on screen').toBe('/mirror/terminal');
    // 🔴 THE FRAME NEVER LOADED THE REFUSAL. Measured before the fix: the frame
    // reloaded itself, got the 401 (whose frame-ancestors 'none' Chromium turns
    // into an error page), and only view_swap's load check — seeing a frame it
    // could not read — then sent the top window on. The refusal was on screen
    // in the frame in between.
    expect(frameNavs, 'the frame made no load of its own after the 401').toEqual([]);
    await page.context().addCookies(cookies);
  });
});
