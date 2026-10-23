import { execFileSync } from 'node:child_process';
import type { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { dsn, repoRoot } from '../lib/env';

// A LONG TITLE MUST NOT BREAK THE AGENT PAGE (operator report: "A dispatched
// cluster agent … had a very long title and it completely breaks the UI").
// A dispatched agent is named after its task, and a task title has no length
// limit. The header drew the name whole (break-all), so a long one became a
// wall of lines over the chat. Now the header shows at most 60 characters on
// one line and a "…" button that opens the whole title in a popover.
//
// Synthetic titles only: one 600-character token with no break opportunity,
// and one made of long words and markdown on a task-linked agent (whose title
// is the task-detail button, so the "…" must sit beside it, not inside it).
// The agent rows are written straight into the database, as a dispatch writes
// them, and removed afterwards: the shared server's other specs count agents.

const stamp = Date.now().toString(36);
const token = (n: string) => 'e2e-long-title-token-not-a-real-one-' + n + '-' + stamp;
const unbroken = 'x'.repeat(600);
const wordy =
  '#803 · **Refactor** the `importerRetryBackoffControllerConfiguration` so ' +
  'supercalifragilisticexpialidocious '.repeat(12) + '_drains_ on restart';
// What the page draws for `wordy` once textplain strips the markdown.
const wordyPlain =
  '#803 · Refactor the importerRetryBackoffControllerConfiguration so ' +
  'supercalifragilisticexpialidocious '.repeat(12) + 'drains on restart';

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

// Inside the 60-character budget, but wider than a 390px header leaves the
// title (~10 characters), and narrower than a 1280px one.
const medium = 'Fix the importer retry backoff';
const short = 'Short';

const agents: { name: string; id: string; title: string; full: string; linked: boolean }[] = [
  { name: 'e2e-long-title-a-' + stamp, id: '', title: unbroken, full: unbroken, linked: false },
  { name: 'e2e-long-title-b-' + stamp, id: '', title: wordy, full: wordyPlain, linked: true },
  { name: 'e2e-long-title-c-' + stamp, id: '', title: medium, full: medium, linked: false },
  { name: 'e2e-long-title-d-' + stamp, id: '', title: short, full: short, linked: false },
];

test.beforeAll(async ({ api }) => {
  for (const a of agents) {
    a.id = fix('agent', '1', a.name, token(a.name));
    fix('label', a.id, a.title);
    if (a.linked) {
      const t = await api.createTask('Long-title spec task ' + stamp, 'A synthetic task for the long-title spec.');
      fix('linknote', a.id, String(t.id));
    }
  }
});

test.afterAll(() => {
  for (const a of agents) if (a.id) fix('rmagent', '1', a.name);
});

// Every box the header and the composer draw is inside the viewport, and the
// document is not wider than it.
async function assertFits(page: Page, label: string) {
  const m = await page.evaluate(() => {
    const vw = window.innerWidth, vh = window.innerHeight;
    const boxes = (sel: string) =>
      Array.from(document.querySelectorAll<HTMLElement>(sel))
        .filter((el) => el.offsetParent !== null)
        .map((el) => {
          const r = el.getBoundingClientRect();
          return { what: el.id || el.getAttribute('aria-label') || el.tagName, left: r.left, right: r.right, top: r.top, bottom: r.bottom };
        });
    const h1 = document.querySelector('header h1')!.getBoundingClientRect();
    return {
      vw, vh,
      docScroll: document.documentElement.scrollWidth, docClient: document.documentElement.clientWidth,
      bodyScroll: document.body.scrollWidth, bodyClient: document.body.clientWidth,
      header: boxes('header button, header a, header select, header input'),
      composer: boxes('#chat-input, #chat-send'),
      h1Height: h1.height,
    };
  });
  expect(m.docScroll, `${label}: the document scrolls sideways`).toBeLessThanOrEqual(m.docClient);
  expect(m.bodyScroll, `${label}: the body scrolls sideways`).toBeLessThanOrEqual(m.bodyClient);
  expect(m.header.length, `${label}: positive control — the header has controls to measure`).toBeGreaterThan(2);
  expect(m.composer.length, `${label}: positive control — the composer is drawn`).toBe(2);
  for (const b of [...m.header, ...m.composer]) {
    expect(b.left, `${label}: ${b.what} starts off screen`).toBeGreaterThanOrEqual(-0.5);
    expect(b.right, `${label}: ${b.what} runs off the right edge`).toBeLessThanOrEqual(m.vw + 0.5);
    expect(b.bottom, `${label}: ${b.what} is below the screen`).toBeLessThanOrEqual(m.vh + 0.5);
  }
  // ONE line: a wrapped title is the wall of text the report was about.
  expect(m.h1Height, `${label}: the title wraps onto more than one line`).toBeLessThanOrEqual(48);
}

for (const vp of [
  { width: 390, height: 844, mobile: true },
  { width: 1280, height: 800, mobile: false },
]) {
  test.describe(`at ${vp.width}x${vp.height}`, () => {
    test.use({ viewport: { width: vp.width, height: vp.height }, hasTouch: vp.mobile, isMobile: vp.mobile });

    for (const which of [0, 1]) {
      test(`a long ${which === 0 ? 'unbroken' : 'task-linked markdown'} title fits one line and opens whole from its "…"`, async ({ page }) => {
        const a = agents[which];
        await page.goto('/agents/' + a.name);
        await expect(page.locator('#chat-input')).toBeVisible();
        await assertFits(page, a.name);

        // The header shows the first 60 characters, not the whole title.
        const shown = (await page.locator('header h1').innerText()).trim();
        expect(shown).toBe(a.full.slice(0, 60).trimEnd());
        await expect(page).toHaveTitle(a.full.slice(0, 60).trimEnd() + '… · muster');

        const more = page.getByRole('button', { name: 'Show the full title' });
        const pop = page.locator('#agent-title-full');
        await expect(more).toBeVisible();
        const mb = (await more.boundingBox())!;
        expect(mb.height).toBeGreaterThanOrEqual(44);
        expect(mb.width).toBeGreaterThanOrEqual(44);
        await expect(more).toHaveAttribute('aria-expanded', 'false');
        await expect(pop).toBeHidden();

        // Tap/click opens it, with the whole title, inside the viewport.
        await more.click();
        await expect(pop).toBeVisible();
        await expect(more).toHaveAttribute('aria-expanded', 'true');
        await expect(pop).toHaveText(a.full.trim());
        const pb = (await pop.boundingBox())!;
        expect(pb.x).toBeGreaterThanOrEqual(0);
        expect(pb.x + pb.width).toBeLessThanOrEqual(vp.width);
        expect(pb.y).toBeGreaterThanOrEqual(0);
        expect(pb.y + pb.height).toBeLessThanOrEqual(vp.height);
        // The open popover widens nothing either.
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBe(true);

        // Escape closes it.
        await page.keyboard.press('Escape');
        await expect(pop).toBeHidden();
        await expect(more).toHaveAttribute('aria-expanded', 'false');

        // The keyboard opens it, and the button again closes it.
        await more.focus();
        await page.keyboard.press('Enter');
        await expect(pop).toBeVisible();
        await more.click();
        await expect(pop).toBeHidden();

        // A tap outside closes it.
        await more.click();
        await expect(pop).toBeVisible();
        const out = { x: vp.width - 10, y: Math.round(vp.height * 0.6) };
        const ob = (await pop.boundingBox())!;
        expect(out.x < ob.x || out.x > ob.x + ob.width || out.y < ob.y || out.y > ob.y + ob.height, 'the outside tap lands on the popover').toBe(true);
        await page.mouse.click(out.x, out.y);
        await expect(pop).toBeHidden();

        if (a.linked) {
          // The title is still the task-detail button, and the "…" did not
          // become part of it: tapping the words opens the task modal.
          await page.getByRole('button', { name: 'View task details' }).click();
          await expect(page.locator('#task-modal')).not.toBeEmpty();
        }
      });
    }
  });
}

// A title INSIDE the rune budget can still be clipped by the header's width:
// at 390px the row leaves the title ~10 characters. The "…" follows what is
// SEEN — it appears whenever the text is clipped, and goes when it fits.
// The "…" is rendered for every title and HIDDEN while the title fits. Assert
// it EXISTS and is hidden: a bare toBeHidden() also passes when the element is
// missing altogether, which is a different (and broken) state.
async function expectHiddenMore(page: Page) {
  const b = page.locator('[data-title-more]');
  await expect(b).toHaveCount(1);
  await expect(b).toHaveAttribute('hidden', '');
  await expect(b).toBeHidden();
}

test.describe('a short-but-clipped title', () => {
  const more = (page: Page) => page.getByRole('button', { name: 'Show the full title' });
  // The header's controls other than the title box, as boxes.
  const controls = (page: Page) =>
    page.evaluate(() =>
      Array.from(document.querySelectorAll<HTMLElement>('header button, header a, header input'))
        .filter((el) => el.offsetParent !== null && !el.closest('[data-agent-title]'))
        .map((el) => { const r = el.getBoundingClientRect(); return [Math.round(r.x), Math.round(r.y), Math.round(r.width)]; }));

  test.describe('at 390x844', () => {
    test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

    test('a 30-character title is clipped, shows its "…", and opens whole', async ({ page }) => {
      // Positive control: a 5-character title fits, so it has no "…".
      await page.goto('/agents/' + agents[3].name);
      await expect(page.locator('#chat-input')).toBeVisible();
      await expectHiddenMore(page);
      const fitting = await controls(page);
      expect(fitting.length, 'positive control: the header has controls to compare').toBeGreaterThan(2);

      await page.goto('/agents/' + agents[2].name);
      await expect(page.locator('#chat-input')).toBeVisible();
      const clipped = await page.locator('header h1').evaluate((el) => el.scrollWidth > el.clientWidth);
      expect(clipped, 'fixture: the 30-character title must be clipped at 390').toBe(true);
      await expect(more(page)).toBeVisible();
      const mb = (await more(page).boundingBox())!;
      expect(mb.x + mb.width).toBeLessThanOrEqual(390);
      // Showing the "…" moved no other header control.
      expect(await controls(page)).toEqual(fitting);
      await assertFits(page, agents[2].name);

      await more(page).click();
      const pop = page.locator('#agent-title-full');
      await expect(pop).toBeVisible();
      await expect(pop).toHaveText(medium);
      await expect(more(page)).toHaveAttribute('aria-expanded', 'true');
      await page.keyboard.press('Escape');
      await expect(pop).toBeHidden();
    });
  });

  test.describe('at 1280x800', () => {
    test.use({ viewport: { width: 1280, height: 800 } });

    test('the same title fits with no "…", and a resize to 390 brings it', async ({ page }) => {
      await page.goto('/agents/' + agents[2].name);
      await expect(page.locator('#chat-input')).toBeVisible();
      await expect(page.locator('header h1')).toHaveText(medium);
      await expectHiddenMore(page);

      await page.setViewportSize({ width: 390, height: 844 });
      await expect(more(page)).toBeVisible();
      await more(page).click();
      await expect(page.locator('#agent-title-full')).toHaveText(medium);
      await expect(page.locator('#agent-title-full')).toBeVisible();
      // Left OPEN: the resize back to 1280 must close it along with the "…".

      await page.setViewportSize({ width: 1280, height: 800 });
      await expectHiddenMore(page);
      // …and the popover that was open closed with it.
      await expect(page.locator('#agent-title-full')).toBeHidden();
      await expect(page.locator('[data-title-more]')).toHaveAttribute('aria-expanded', 'false');
    });
  });
});

// On a landscape phone the 600-character popover is taller than the space on
// either side of its "…": it goes on the roomier side, capped and scrolling,
// and never covers the button that opened it.
test.describe('landscape phone 844x390', () => {
  test.use({ viewport: { width: 844, height: 390 }, hasTouch: true, isMobile: true });

  test('the full-title popover does not cover its own "…"', async ({ page }) => {
    await page.goto('/agents/' + agents[0].name);
    await expect(page.locator('#chat-input')).toBeVisible();
    const more = page.getByRole('button', { name: 'Show the full title' });
    await more.click();
    const pop = page.locator('#agent-title-full');
    await expect(pop).toBeVisible();
    const m = (await more.boundingBox())!, p = (await pop.boundingBox())!;
    const overlap = !(p.x + p.width <= m.x || m.x + m.width <= p.x || p.y + p.height <= m.y || m.y + m.height <= p.y);
    expect(overlap, `the popover ${JSON.stringify(p)} covers its "…" ${JSON.stringify(m)}`).toBe(false);
    expect(p.y + p.height).toBeLessThanOrEqual(390);
    // It scrolls inside rather than being cut off.
    expect(await pop.evaluate((el) => el.scrollHeight > el.clientHeight)).toBe(true);
    await page.keyboard.press('Escape');
    await expect(pop).toBeHidden();
  });
});
