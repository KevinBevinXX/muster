import { Page } from '@playwright/test';
import { test, expect, useSuiteSession } from '../lib/fixtures';
import { composerFitPort } from '../lib/env';
import { cc, MirrorRig, mkchat } from '../lib/mirrorhost';

// THE COMPOSERS GROW WITH THEIR TEXT (internal/ui/composer_fit.go): both message
// boxes — the agent chat's #chat-input and the mirrored views' #mv-input (the
// Rendered view's and the Terminal's) — are as tall as what is typed, up to 40%
// of the visible viewport, scroll inside themselves past that, and shrink back
// when a send empties them. At a phone (390×844, touch) and a desktop
// (1280×800), each in two modes:
//
//   native   — `field-sizing: content` does the sizing (Chromium has it);
//   fallback — the page is told field-sizing is unsupported, so the script's
//              own sizing is what is measured (Safari before 26, Firefox).
//
// 🔴 THE CAP IS MEASURED, NOT ASSUMED: 40% of visualViewport.height, read in the
// page, compared to the box's rendered height.
//
// ⚠ WHAT THIS CANNOT SHOW: a real phone keyboard. Emulation has none, so the cap
// following the keyboard (iOS overlays it; visualViewport shrinks) is shown by
// resizing the viewport instead — the same resize event, not the same device.

const rig = new MirrorRig(composerFitPort, 'e2e-fit');
const agent = 'e2e-fit-' + Date.now().toString(36);
let transcript = '';
let pasteFile = '';

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(120_000);
  await rig.start();
  transcript = rig.transcript(cc.prompt('hello') + cc.reply('Hi.'));
  pasteFile = transcript + '.panes';
  await rig.startHost({
    MUSTER_MIRROR_INTERACTIVE: '1',
    MUSTER_MIRROR_E2E_SESSIONS: 'main,scratch',
    MUSTER_MIRROR_E2E_CLAUDE: 'main=' + transcript,
    MUSTER_MIRROR_E2E_PASTE_FILE: pasteFile,
  });
  mkchat(agent, null);
  mkchat(agent, [{ role: 'user', content: 'hello' }, { role: 'assistant', content: 'Hi.' }]);
});

test.afterAll(async () => {
  mkchat(agent, null);
  await rig.stop();
});

// The fallback: field-sizing reported unsupported, before any page script runs.
const noFieldSizing = `(() => {
  const real = CSS.supports.bind(CSS);
  CSS.supports = function (a, b) {
    if (String(a).indexOf('field-sizing') >= 0) return false;
    return arguments.length < 2 ? real(a) : real(a, b);
  };
})()`;

interface Box {
  h: number;
  bottom: number;
  sh: number;
  ch: number;
  vvh: number;
  inline: string;
  oy: string;
  scrollY: number;
}
const box = (page: Page, sel: string): Promise<Box> =>
  page.locator(sel).evaluate((el: HTMLTextAreaElement) => {
    const r = el.getBoundingClientRect();
    return {
      h: r.height,
      bottom: r.bottom,
      sh: el.scrollHeight,
      ch: el.clientHeight,
      vvh: window.visualViewport ? window.visualViewport.height : innerHeight,
      inline: el.style.height,
      oy: getComputedStyle(el).overflowY,
      scrollY: window.scrollY,
    };
  });

const lines = (n: number) => Array.from({ length: n }, (_, i) => `line ${i + 1} of a long message`).join('\n');

// grow checks the growth half on one composer: a few lines grow it, many cap
// it at 40% of the visible viewport and scroll inside it, and the box grows
// UPWARD — its bottom edge and the page's scroll do not move.
async function grow(page: Page, sel: string, mode: string): Promise<Box> {
  const input = page.locator(sel);
  await expect(input).toBeEnabled();
  const empty = await box(page, sel);
  expect(empty.h, 'an empty box is one line (44px)').toBeLessThan(60);

  await input.fill(lines(3));
  const three = await box(page, sel);
  expect(three.h, `three lines grow the box (${empty.h} → ${three.h})`).toBeGreaterThan(empty.h + 25);
  expect(Math.abs(three.bottom - empty.bottom), 'it grows upward: its bottom edge stays put').toBeLessThanOrEqual(1);
  expect(three.scrollY, 'the page itself did not scroll').toBe(empty.scrollY);
  if (mode === 'fallback') expect(three.inline, "the script's fallback sized it").not.toBe('');
  else expect(three.inline, 'field-sizing sized it, not the script').toBe('');

  await input.fill(lines(60));
  const many = await box(page, sel);
  const cap = Math.round(many.vvh * 0.4);
  expect(Math.abs(many.h - cap), `capped at 40% of the visible viewport (${many.vvh}px → ${cap}px), got ${many.h}px`).toBeLessThanOrEqual(1);
  expect(many.oy, 'past the cap it scrolls').toBe('auto');
  expect(many.sh, 'its content is taller than the box').toBeGreaterThan(many.ch + 200);
  const scrolled = await input.evaluate((el) => {
    el.scrollTop = 99999;
    return el.scrollTop;
  });
  expect(scrolled, 'and it scrolls inside itself').toBeGreaterThan(100);
  expect(Math.abs(many.bottom - empty.bottom), 'still anchored at its bottom edge').toBeLessThanOrEqual(1);
  return empty;
}

async function shrunk(page: Page, sel: string, empty: Box) {
  await expect(page.locator(sel)).toHaveValue('');
  await expect.poll(async () => Math.round((await box(page, sel)).h), { message: 'after the send the box is one line again' }).toBe(Math.round(empty.h));
}

for (const vp of [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  for (const mode of ['native', 'fallback']) {
    test.describe(`${vp.name}, ${mode}`, () => {
      test.use(vp.use);
      test.beforeEach(async ({ page }) => {
        if (mode === 'fallback') await page.addInitScript(noFieldSizing);
      });

      test('the agent chat box grows, caps at 40%, scrolls, and shrinks after a send', async ({ page, baseURL }) => {
        // Signed in on the shared server, where the agent is.
        await useSuiteSession(page);
        // A socket that accepts the message and finishes the turn: the send path
        // that empties the box is the WebSocket one.
        const sent: string[] = [];
        await page.routeWebSocket(/\/agents\/[^/]+\/ws/, (ws) => {
          ws.onMessage((m) => {
            sent.push(String(m));
            ws.send(JSON.stringify({ type: 'done' }));
          });
        });
        await page.goto(`${baseURL}/agents/${agent}`);
        const empty = await grow(page, '#chat-input', mode);
        await page.locator('#chat-input').fill('three\nline\nmessage');
        await page.locator('#chat-input').press('Enter');
        await expect.poll(() => sent.length, { message: 'the message went over the socket' }).toBe(1);
        await shrunk(page, '#chat-input', empty);
      });

      test('the mirrored Rendered view and Terminal boxes grow, cap at 40%, scroll, and shrink after a send', async ({ page }) => {
        await rig.login(page);
        await rig.waitListed(page, 'data-mirror-claude');
        for (const [path, label] of [
          [rig.viewPath('main'), 'Rendered'],
          [rig.terminalPath('scratch'), 'Terminal'],
        ]) {
          await page.goto(rig.origin + path);
          const empty = await grow(page, '#mv-input', mode);
          await page.locator('#mv-input').fill(`a ${label} send\nover two lines`);
          await page.locator('#mv-input').press('Enter');
          await shrunk(page, '#mv-input', empty);
        }
      });

      // 🔴 THE VISUAL VIEWPORT ALONE. A phone's keyboard shrinks
      // window.visualViewport and leaves the layout viewport (and 40dvh, the CSS
      // fallback) where it was — so a resize of the whole viewport cannot tell
      // the script's cap from the fallback. Here only visualViewport reports a
      // shorter height (400px) and fires its resize, as a keyboard opening does:
      // the cap must follow it (160px) while innerHeight stays put.
      test('the cap follows the visual viewport alone (an on-screen keyboard)', async ({ page }) => {
        await rig.login(page);
        await rig.waitListed(page, 'data-mirror-claude');
        await page.goto(rig.origin + rig.viewPath('main'));
        await page.locator('#mv-input').fill(lines(60));
        const before = await box(page, '#mv-input');
        expect(Math.abs(before.h - Math.round(before.vvh * 0.4)), 'capped at 40% before').toBeLessThanOrEqual(1);
        const inner = await page.evaluate(() => {
          const vv = window.visualViewport!;
          Object.defineProperty(vv, 'height', { configurable: true, get: () => 400 });
          vv.dispatchEvent(new Event('resize'));
          return window.innerHeight;
        });
        expect(inner, 'the layout viewport did not change').toBe(vp.use.viewport.height);
        await expect.poll(async () => Math.round((await box(page, '#mv-input')).h), { message: 'the cap is 40% of the VISUAL viewport' }).toBe(160);
      });

      test('the cap follows the visible viewport when it changes', async ({ page }) => {
        await rig.login(page);
        await rig.waitListed(page, 'data-mirror-claude');
        await page.goto(rig.origin + rig.viewPath('main'));
        await page.locator('#mv-input').fill(lines(60));
        const tall = await box(page, '#mv-input');
        // A shorter viewport — what an on-screen keyboard does to visualViewport.
        await page.setViewportSize({ width: vp.use.viewport.width, height: 500 });
        await expect.poll(async () => Math.round((await box(page, '#mv-input')).h), { message: 'the cap is 40% of the new height' }).toBe(200);
        expect(tall.h).toBeGreaterThan(300);
      });
    });
  }
}
