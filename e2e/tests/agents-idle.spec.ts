import fs from 'node:fs';
import path from 'node:path';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { agentsIdlePort } from '../lib/env';
import { MirrorRig } from '../lib/mirrorhost';
import { clsInit, readCls, resetCls, settle, ClsResult } from '../lib/cls';
import { writeFileAtomic } from '../lib/atomic';

// THE AGENTS LIST'S IDLE SECTION (internal/ui/idle_section.go), in a real
// browser at a phone and a desktop, against a real muster-server with the
// DEFAULT threshold (MUSTER_AGENTS_IDLE_AFTER unset: 14 days) and a real
// muster-mirror over the tunnel whose tmux is the e2e fake:
//
//   1. Windows with no activity for more than 14 days are drawn last, under a
//      COLLAPSED "Idle (N)" header — collapsed on the first paint, no shift.
//   2. A pinned window never collapses, however old.
//   3. The header opens and closes the section; the choice survives a reload
//      (localStorage, applied before the first paint) — no shift either way.
//   4. A search finds an idle window while the section is collapsed.
//   5. A window that becomes active again leaves the section on the next live
//      refresh — behind the "N new ↑" pill, since applying it moves cards in
//      view — and the count follows; nothing moves until the tap.
//
// 🔴 REGRESSION TESTS: on the tree before, there was no section — every window
// was drawn in the one list however long it had been idle.
//
// Activity times are pairwise distinct; the nearest to the boundary on either
// side is 13 days 20 hours (working) and 15 days 3 hours (idle). The boundary
// itself is the Go suite's (InIdleSection, the API test with an injected clock).

const BAR = 0.01;
const rig = new MirrorRig(agentsIdlePort, 'e2e-idle', { MUSTER_FOCUS_INTERVAL: '0' }, { decimalHost: true });
const day = 86400;
const now = Math.floor(Date.now() / 1000);
// session -> seconds idle. Names carry no letter the seeded agents' cards hold
// together, so a search for one finds only it.
const WORKING: Record<string, number> = { ia1: 180, ia2: 2 * 3600 + 17 * 60, ia3: 6 * day + 5 * 3600, ia4: 13 * day + 20 * 3600 };
const IDLE: Record<string, number> = { iz1: 15 * day + 3 * 3600, iz2: 21 * day + 7 * 3600, iz3: 40 * day + 11 * 3600 };
const PINNED = { izpin: 50 * day + 13 * 3600 };
const ALL = { ...WORKING, ...IDLE, ...PINNED };
const OPEN_KEY = 'muster.agents.idle.open';
let windowsFile = '';
let pinned = false;

function writeWindows(over: Record<string, number> = {}) {
  const lines = Object.entries({ ...ALL, ...over }).map(([s, ago], i) => `${s}|@${i + 1}|1|1|zsh|${now - ago}|`);
  writeFileAtomic(windowsFile, lines.join('\n') + '\n');
}

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(120_000);
  await rig.start();
  windowsFile = path.join(rig.dir, 'windows');
  writeWindows();
  await rig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: windowsFile, MUSTER_MIRROR_E2E_REFRESH: '300ms' });
});

test.afterAll(async () => {
  await rig.stop();
});

const card = (page: Page, s: string) => page.locator(`#agents-list > article[data-f-host="${rig.host}"][data-f-session="${s}"]`);
const head = (page: Page) => page.locator('#agents-idle-head');
const toggle = (page: Page) => page.locator('#agents-idle-toggle');

async function open(page: Page) {
  await rig.login(page);
  await rig.waitListed(page, 'data-f-session="iz3"');
  if (!pinned) {
    const i = Object.keys(ALL).indexOf('izpin') + 1;
    const r = await page.request.post(rig.origin + '/agents/pins', {
      form: { kind: 'mirror', host: rig.host, session: 'izpin', window: `@${i}` },
      headers: { Origin: rig.origin },
    });
    expect(r.status(), 'pinned izpin').toBe(200);
    pinned = true;
  }
  await page.addInitScript(clsInit);
}

// tapByScript clicks without input: a scripted click is not the reader's, so
// whatever it moves is counted by the layout-shift record.
const tapByScript = (page: Page, sel: string) => page.evaluate((s) => (document.querySelector(s) as HTMLElement).click(), sel);

for (const vp of [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.name, () => {
    test.use(vp.use);

    test('idle windows are drawn last, collapsed; open/close persists; search finds them; no shift', async ({ page }) => {
      test.setTimeout(120_000);
      await open(page);
      await page.goto(rig.origin + '/agents');
      await page.evaluate((k) => localStorage.removeItem(k), OPEN_KEY);
      const results: Record<string, ClsResult> = {};
      await page.goto(rig.origin + '/agents');
      await settle(page);
      results['load, collapsed'] = await readCls(page);

      // 1. Collapsed: the working windows and the pinned one shown, the idle hidden.
      for (const s of Object.keys(WORKING)) await expect(card(page, s), `${s} (working) is shown`).toBeVisible();
      await expect(card(page, 'izpin'), 'a pinned window never collapses').toBeVisible();
      await expect(card(page, 'izpin')).not.toHaveAttribute('data-idle-section', '');
      for (const s of Object.keys(IDLE)) {
        await expect(card(page, s), `${s} (idle) is in the section`).toHaveAttribute('data-idle-section', '');
        await expect(card(page, s), `${s} is collapsed`).toBeHidden();
      }
      await expect(head(page)).toBeVisible();
      await expect(head(page).locator('[data-idle-count]')).toHaveText('(3)');
      await expect(toggle(page)).toHaveAttribute('aria-expanded', 'false');
      await expect(head(page)).toContainText('no activity in over 14 days');
      // The header is drawn after every working card.
      const below = await page.evaluate(() => {
        const h = document.getElementById('agents-idle-head')!.getBoundingClientRect().top;
        return [...document.querySelectorAll('#agents-list > article:not([data-idle-section])')]
          .filter((a) => a.getClientRects().length)
          .every((a) => a.getBoundingClientRect().top < h);
      });
      expect(below, 'the Idle header is below every working card').toBe(true);

      // 3. Opened (a scripted click: not input, so any shift counts).
      await resetCls(page);
      await tapByScript(page, '#agents-idle-toggle');
      for (const s of Object.keys(IDLE)) await expect(card(page, s), `${s} shown once open`).toBeVisible();
      await expect(toggle(page)).toHaveAttribute('aria-expanded', 'true');
      await settle(page, 500);
      results['open'] = await readCls(page);
      // Each idle card is drawn below the header.
      const after = await page.evaluate(() => {
        const h = document.getElementById('agents-idle-head')!.getBoundingClientRect().bottom;
        return [...document.querySelectorAll('#agents-list > article[data-idle-section]')].every((a) => a.getBoundingClientRect().top >= h);
      });
      expect(after, 'the idle cards are drawn under the header').toBe(true);
      expect(await page.evaluate((k) => localStorage.getItem(k), OPEN_KEY)).toBe('1');

      // Reloaded: open from the first paint.
      await page.goto(rig.origin + '/agents');
      await settle(page);
      results['load, restored open'] = await readCls(page);
      for (const s of Object.keys(IDLE)) await expect(card(page, s)).toBeVisible();

      // Closed, and reloaded closed.
      await resetCls(page);
      await tapByScript(page, '#agents-idle-toggle');
      for (const s of Object.keys(IDLE)) await expect(card(page, s)).toBeHidden();
      await settle(page, 500);
      results['close'] = await readCls(page);
      expect(await page.evaluate((k) => localStorage.getItem(k), OPEN_KEY)).toBeNull();
      await page.goto(rig.origin + '/agents');
      await settle(page);
      await expect(card(page, 'iz2')).toBeHidden();

      // 4. A search finds an idle window while the section is collapsed.
      await page.locator('#agents-search').fill('iz2');
      await expect(card(page, 'iz2'), 'the search finds the idle window').toBeVisible();
      await expect(head(page).locator('[data-idle-count]'), 'the count is what the search shows').toHaveText('(1)');
      await expect(card(page, 'ia1')).toBeHidden();
      await page.locator('#agents-search').fill('');
      await expect(card(page, 'iz2'), 'cleared: collapsed again').toBeHidden();
      await expect(head(page).locator('[data-idle-count]')).toHaveText('(3)');
      expect(await page.evaluate((k) => localStorage.getItem(k), OPEN_KEY), 'the search left the choice as it was').toBeNull();

      console.log(`[idle ${vp.name}] ` + Object.entries(results).map(([k, r]) => `${k} ${r.total}`).join(' | '));
      const bad = Object.entries(results).filter(([, r]) => r.total >= BAR).map(([k, r]) => `${k}: ${r.total} ${JSON.stringify(r.shifts)}`);
      expect(bad).toEqual([]);
    });

    test('a window active again leaves the section on the next live refresh, behind the pill; no shift until the tap', async ({ page }) => {
      test.setTimeout(120_000);
      await open(page);
      await page.goto(rig.origin + '/agents');
      await page.evaluate((k) => localStorage.removeItem(k), OPEN_KEY);
      await page.goto(rig.origin + '/agents');
      await settle(page);
      await expect(card(page, 'iz2')).toHaveAttribute('data-idle-section', '');
      await expect(head(page).locator('[data-idle-count]')).toHaveText('(3)');
      try {
        await resetCls(page);
        // iz2 used a minute ago: the host reports it on its next listing.
        writeWindows({ iz2: 60 });
        await expect(page.locator('#agents-new-pill'), 'the move waits behind the pill').toHaveText('1 new ↑', { timeout: 15_000 });
        await settle(page);
        const held = await readCls(page);
        await expect(card(page, 'iz2'), 'nothing moved before the tap').toBeHidden();
        expect(held.total, `held: ${JSON.stringify(held.shifts)}`).toBeLessThan(BAR);
        await page.locator('#agents-new-pill').click();
        await expect(card(page, 'iz2'), 'the tap brings it out of the section').toBeVisible();
        await expect(card(page, 'iz2')).not.toHaveAttribute('data-idle-section', '');
        await expect(head(page).locator('[data-idle-count]'), 'and the count follows').toHaveText('(2)');
        // It is drawn above the header, among the working windows.
        const above = await page.evaluate((h) => {
          const c = document.querySelector(`#agents-list > article[data-f-host="${h}"][data-f-session="iz2"]`)!.getBoundingClientRect().top;
          return c < document.getElementById('agents-idle-head')!.getBoundingClientRect().top;
        }, rig.host);
        expect(above, 'the reactivated window is above the Idle header').toBe(true);
        // A reload agrees: it is a working window now.
        await page.goto(rig.origin + '/agents');
        await expect(card(page, 'iz2')).toBeVisible();
      } finally {
        writeWindows();
      }
      // Idle again (the host reports its old time): back in the section.
      await expect.poll(async () => (await (await page.request.get(rig.origin + '/ui/agents')).text()).match(/data-idle-count="" class="tabular-nums">\((\d+)\)/)?.[1], { timeout: 15_000 }).toBe('3');
    });
  });
}

// 🔴 A CLAUDE WINDOW IS IDLE BY ITS LAST MESSAGE, NOT BY TMUX. tmux's window
// activity is reset for every window by a tmux restart (a tmux-resurrect
// restore), so on a real host every window read "active a minute ago". A
// second host here lists one Claude window whose tmux activity is a minute old
// and whose transcript's last message is 30 days old, followed by what a
// RESUME appends (an attachment and a system row with today's time): the real
// muster-mirror reads the last message (internal/mirrorhost/lastmsg.go), sends
// it, and the card is in the Idle section saying "active 30d ago".
// On the tree before, the card was drawn by tmux's minute: working, "1m".
test('a Claude window with fresh tmux activity but a 30-day-old last message is idle, and says so', async ({ page }) => {
  test.setTimeout(120_000);
  const hostL = rig.host.replace('e2e-idle-', 'e2e-idlel-');
  const tr = path.join(rig.dir, 'claude-old.jsonl');
  const old = new Date(Date.now() - 30 * day * 1000 - 5 * 3600 * 1000).toISOString();
  const fresh = new Date().toISOString();
  writeFileAtomic(
    tr,
    [
      { type: 'user', timestamp: old, message: { role: 'user', content: 'tidy the notes' } },
      { type: 'assistant', timestamp: old, message: { role: 'assistant', content: [{ type: 'text', text: 'Done.' }] } },
      { type: 'cost-state' },
      { type: 'attachment', timestamp: fresh, attachment: { type: 'deferred_tools_delta' } },
      { type: 'system', subtype: 'informational', timestamp: fresh, content: 'resumed' },
    ].map((r) => JSON.stringify(r)).join('\n') + '\n',
  );
  // The same 30-day-old conversation in a second window whose session is
  // WORKING (a tool running: Claude Code writes no row meanwhile). Busy counts
  // as active: that card reads active now, and is not idle.
  const trBusy = path.join(rig.dir, 'claude-busy.jsonl');
  fs.copyFileSync(tr, trBusy);
  writeFileAtomic(trBusy + '.status', JSON.stringify({ status: 'busy', statusUpdatedAt: Date.now() - 40 * 60_000 }));
  const lwin = path.join(rig.dir, 'windows-l');
  writeFileAtomic(lwin, `iclaude|@1|1|1|claude|${Math.floor(Date.now() / 1000) - 60}|${tr}\nicbusy|@2|1|1|claude|${Math.floor(Date.now() / 1000) - 60}|${trBusy}\n`);
  const stop = rig.extraHost(hostL, { MUSTER_MIRROR_E2E_WINDOWS_FILE: lwin, MUSTER_MIRROR_E2E_REFRESH: '300ms' });
  try {
    await rig.login(page);
    await rig.waitListed(page, 'data-f-session="iclaude"');
    await page.goto(rig.origin + '/agents');
    await page.evaluate((k) => localStorage.removeItem(k), OPEN_KEY);
    await page.goto(rig.origin + '/agents');
    const c = page.locator(`#agents-list > article[data-f-host="${hostL}"][data-f-session="iclaude"]`);
    await expect(c, 'the card is in the Idle section').toHaveAttribute('data-idle-section', '');
    await expect(c, 'collapsed with it').toBeHidden();
    await tapByScript(page, '#agents-idle-toggle');
    await expect(c).toBeVisible();
    await expect(c.locator('[data-card-active]'), 'its card prints the last message').toHaveText(/active\s*30d\s*ago/);
    const b = page.locator(`#agents-list > article[data-f-host="${hostL}"][data-f-session="icbusy"]`);
    await expect(b, 'the working window is not idle').not.toHaveAttribute('data-idle-section', '');
    await expect(b).toBeVisible();
    await expect(b.locator('[data-card-active]'), 'and reads active now, not 30 days ago').toHaveText(/active\s*(\d+s|[01]m)\s*ago/);
    await tapByScript(page, '#agents-idle-toggle');
  } finally {
    await stop();
  }
});
