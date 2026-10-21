import fs from 'node:fs';
import path from 'node:path';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { workCardsPort } from '../lib/env';
import { MirrorRig, cc } from '../lib/mirrorhost';
import { clsInit, readCls, resetCls, settle } from '../lib/cls';

// LIVE WORK COUNTS AND THE TMUX IDENTITY COLOUR ON MIRRORED CARDS
// (docs/design/agent-cards.md (a) and (b)), every hop real: a real
// muster-mirror (-tags e2emirror) whose fake windows carry what the real host
// reads — a Claude Code window's RAW session status, its process-tree shells
// and a real transcript the host's ledger reads for running Agent calls, and
// each window's @muster-color value — over the real tunnel to a real server.
//
//   - THE CHIP: a busy window with two running Agent calls and a shell reads
//     "2 subagents · 1+ shell" (a turn may be running, so the shell count is a
//     lower bound); a window whose CLI says "shell" with three reads "3 shells";
//     it goes away LIVE when the CLI goes idle. It is display only (no filter field).
//   - THE TINT: the card's border is the window's colour and its title the
//     title variant, the server's own per-theme values, in BOTH themes; a card
//     with no colour keeps the line border; a shell window is tinted too.
//   - AN OLDER HOST (MUSTER_MIRROR_E2E_NO_WORK: it sends neither message) whose
//     windows carry the same status and colour renders exactly as before: no
//     chip, no tint.
//   - Phone (390×844) and desktop (1280×800), with the layout shift of the list
//     carrying chips and tints under the cls spec's bar.

const BAR = 0.01;
const viewports = [
  { name: 'phone 390×844', viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true },
  { name: 'desktop 1280×800', viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false },
];

const rig = new MirrorRig(workCardsPort, 'e2e-work');
const oldHost = 'e2e-work-old-' + Date.now().toString(36);
let stopOld: (() => Promise<void>) | undefined;
let trBusy = '';
let trShell = '';
let trOld = '';

// status writes a fake window's <transcript>.status: its RAW Claude Code
// status and what the real host reads from /proc.
function status(tr: string, s: string, shells: number) {
  fs.writeFileSync(tr + '.status', JSON.stringify({ status: s, statusUpdatedAt: Date.now() - 5_000, startedAt: Date.now() - 3_600_000, shells }));
}

// An Agent call at a time the ledger counts (after the CLI started).
const agentCall = (id: string) =>
  JSON.stringify({
    type: 'assistant',
    timestamp: new Date(Date.now() - 60_000).toISOString(),
    message: { content: [{ type: 'tool_use', id, name: 'Agent', input: { description: 'fixture job', prompt: 'p', subagent_type: 'general-purpose' } }] },
  }) + '\n';

test.describe.configure({ mode: 'serial' });

test.beforeAll(async () => {
  test.setTimeout(240_000);
  await rig.start();
  trBusy = rig.transcript(cc.prompt('fan out') + agentCall('toolu_e2e_w1') + agentCall('toolu_e2e_w2'), 'busy');
  trShell = rig.transcript(cc.prompt('run the watcher') + cc.reply('Started.'), 'shell');
  trOld = rig.transcript(cc.prompt('older host') + agentCall('toolu_e2e_old'), 'old');
  status(trBusy, 'busy', 1);
  status(trShell, 'shell', 3);
  status(trOld, 'busy', 2);
  const now = Math.floor(Date.now() / 1000);
  const windows = path.join(rig.dir, 'windows');
  fs.writeFileSync(
    windows,
    [
      `fleet|@5|5|1|claude|${now - 60}|${trBusy}|||#d79921`,
      `fleet|@6|6|0|claude|${now - 90}|${trShell}|||#458588`,
      `notes|@7|0|1|zsh|${now - 120}||||#b16286`,
      `plain|@8|0|1|claude|${now - 150}|${trShell}`,
    ].join('\n') + '\n',
  );
  await rig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: windows, MUSTER_MIRROR_E2E_REFRESH: '300ms' });
  const oldWindows = path.join(rig.dir, 'windows-old');
  fs.writeFileSync(oldWindows, `relic|@9|0|1|claude|${now - 30}|${trOld}|||#d79921\n`);
  stopOld = rig.extraHost(oldHost, { MUSTER_MIRROR_E2E_WINDOWS_FILE: oldWindows, MUSTER_MIRROR_E2E_REFRESH: '300ms', MUSTER_MIRROR_E2E_NO_WORK: '1' });
});

test.afterAll(async () => {
  await stopOld?.();
  await rig.stop();
});

const card = (page: Page, title: string) => page.locator('#agents-list > article[data-mirror-card]').filter({ hasText: title });
// chipText is what the chip SHOWS (its screen-reader label is a separate span).
const chipText = (page: Page, title: string) => card(page, title).locator('[data-mirror-work] > [aria-hidden="true"]');

// triplet is a style attribute's custom property ("R G B") as a computed rgb().
async function variant(page: Page, title: string, prop: string): Promise<string> {
  const style = (await card(page, title).getAttribute('style')) || '';
  const m = new RegExp(`${prop}:(\\d+) (\\d+) (\\d+)`).exec(style);
  expect(m, `${title}'s style ${style} names ${prop}`).toBeTruthy();
  return `rgb(${m![1]}, ${m![2]}, ${m![3]})`;
}

const borderOf = (page: Page, title: string) => card(page, title).evaluate((el) => getComputedStyle(el).borderTopColor);
const titleColourOf = (page: Page, title: string) => card(page, title).locator('[data-mirror-name]').first().evaluate((el) => getComputedStyle(el).color);

async function openList(page: Page) {
  await rig.login(page);
  await page.goto(rig.origin + '/agents');
  await expect(card(page, 'fleet:5'), rig.log).toHaveCount(1, { timeout: 30_000 });
  await expect(card(page, 'relic:0'), rig.log).toHaveCount(1, { timeout: 30_000 });
}

for (const vp of viewports) {
  test.describe(vp.name, () => {
    test.use({ viewport: vp.viewport, hasTouch: vp.hasTouch, isMobile: vp.isMobile, storageState: { cookies: [], origins: [] } });

    test('each card reads what its window has running, and an older host shows nothing', async ({ page }) => {
      test.setTimeout(90_000);
      await openList(page);
      await expect(chipText(page, 'fleet:5'), rig.log).toHaveText('2 subagents · 1+ shell', { timeout: 20_000 });
      await expect(card(page, 'fleet:5').locator('[data-mirror-work] .sr-only')).toHaveText('2 subagents and at least 1 background shell running');
      await expect(chipText(page, 'fleet:6')).toHaveText('3 shells');
      // A shell window, and a Claude window with no status read: no chip.
      await expect(card(page, 'notes:0').locator('[data-mirror-work]')).toHaveCount(0);
      // The older host: the same busy status, shells and transcript — no chip.
      await expect(card(page, 'relic:0').locator('[data-mirror-work]')).toHaveCount(0);
      // The chip sits in the header row, inside the card (no overflow).
      const box = (await card(page, 'fleet:5').boundingBox())!;
      const chip = (await card(page, 'fleet:5').locator('[data-mirror-work]').boundingBox())!;
      expect(chip.x >= box.x && chip.x + chip.width <= box.x + box.width + 0.5, `the chip ${JSON.stringify(chip)} is inside the card ${JSON.stringify(box)}`).toBe(true);
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
      // Display only: the chip is not a filter field.
      expect(await card(page, 'fleet:5').getAttribute('data-f-work')).toBeNull();
    });

    test('the border and title wear the window colour in both themes; untinted and older-host cards keep the line', async ({ page }) => {
      test.setTimeout(90_000);
      await openList(page);
      await expect(card(page, 'fleet:5')).toHaveAttribute('data-identity', '#d79921', { timeout: 20_000 });
      const line = await borderOf(page, 'plain:0');
      for (const theme of ['dark', 'light'] as const) {
        if (theme === 'light') {
          await page.evaluate(() => localStorage.setItem('muster-theme', 'light'));
          await page.reload();
          await expect(card(page, 'fleet:5')).toHaveCount(1, { timeout: 20_000 });
        }
        const sfx = theme === 'dark' ? 'd' : 'l';
        for (const title of ['fleet:5', 'fleet:6', 'notes:0']) {
          expect(await borderOf(page, title), `${title} border (${theme})`).toBe(await variant(page, title, `--id-bd-${sfx}`));
          expect(await titleColourOf(page, title), `${title} title (${theme})`).toBe(await variant(page, title, `--id-tt-${sfx}`));
        }
        // No colour, or an older host's: the line border, exactly as before.
        const plainLine = await borderOf(page, 'plain:0');
        expect(await borderOf(page, 'relic:0'), `the older host's card (${theme})`).toBe(plainLine);
        expect(await card(page, 'relic:0').getAttribute('data-identity')).toBeNull();
        expect(await card(page, 'plain:0').getAttribute('style')).toBeNull();
        if (theme === 'dark') expect(plainLine).toBe(line);
      }
      // The terminal colour itself is the dark border (it already meets 3:1
      // there); the light title had to darken (4.5:1 on white).
      await page.evaluate(() => localStorage.setItem('muster-theme', 'dark'));
      await page.reload();
      await expect(card(page, 'fleet:5')).toHaveCount(1, { timeout: 20_000 });
      expect(await borderOf(page, 'fleet:5')).toBe('rgb(215, 153, 33)');
      expect(await variant(page, 'fleet:5', '--id-tt-l')).not.toBe('rgb(215, 153, 33)');
      // The CARDS only: the rendered view's header title is not tinted.
      await page.goto(rig.origin + `/mirror/view?host=${encodeURIComponent(rig.host)}&session=fleet&window=${encodeURIComponent('@5')}`);
      await expect(page.locator('header [data-mirror-name]')).toHaveCount(1, { timeout: 15_000 });
      await expect(page.locator('[data-identity]')).toHaveCount(0);
    });

    test(`the list with chips and tints shifts less than ${BAR}`, async ({ page }) => {
      test.setTimeout(60_000);
      await rig.login(page);
      await page.addInitScript(clsInit);
      await page.goto(rig.origin + '/agents');
      await expect(card(page, 'fleet:5').locator('[data-mirror-work]'), rig.log).toHaveCount(1, { timeout: 20_000 });
      await settle(page);
      const r = await readCls(page);
      console.log(`[cls ${vp.name}] /agents with work chips and tints ${r.total}`);
      expect(r.total, JSON.stringify(r.shifts)).toBeLessThan(BAR);
      // POSITIVE CONTROL: the same instrument, on the same page, sees a shift
      // when one happens — a block pushed in above the cards.
      await resetCls(page);
      await page.evaluate(() => {
        const d = document.createElement('div');
        d.style.height = '160px';
        document.querySelector('#agents-list')!.prepend(d);
      });
      await settle(page);
      const c = await readCls(page);
      console.log(`[cls ${vp.name}] positive control (a block pushed in above the cards) ${c.total}`);
      expect(c.total, 'the instrument can observe a shift').toBeGreaterThan(BAR);
    });
  });
}

test('a chip goes away live when its CLI goes idle, and comes back when it runs a shell again', async ({ page }) => {
  test.setTimeout(90_000);
  await page.setViewportSize({ width: 390, height: 844 });
  await page.addInitScript(clsInit);
  await openList(page);
  const chip = card(page, 'fleet:6').locator('[data-mirror-work]');
  await expect(chipText(page, 'fleet:6'), rig.log).toHaveText('3 shells', { timeout: 20_000 });
  await settle(page);
  await resetCls(page);
  status(trShell, 'idle', 0);
  await expect(chip, 'the chip left without a reload').toHaveCount(0, { timeout: 20_000 });
  status(trShell, 'shell', 2);
  await expect(chipText(page, 'fleet:6'), 'the chip came back without a reload').toHaveText('2 shells', { timeout: 20_000 });
  await settle(page);
  // The live updates shift the list less than the bar too (measured 0.0003:
  // the chip is in a wrapping header row, so only its own row reflows).
  const r = await readCls(page);
  console.log(`[cls phone] the chip leaving and coming back live ${r.total} ${JSON.stringify(r.shifts)}`);
  expect(r.total, JSON.stringify(r.shifts)).toBeLessThan(BAR);
});
