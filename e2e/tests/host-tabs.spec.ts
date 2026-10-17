import fs from 'node:fs';
import { Locator, Page } from '@playwright/test';
import { test, expect, takeNew } from '../lib/fixtures';
import { hostTabsPort } from '../lib/env';
import { MirrorRig, mkchat } from '../lib/mirrorhost';

// THE AGENTS LIST'S HOST TABS (internal/ui/agents_filter.go agentsHostTabs), in
// a real browser, against a real muster-server with the mirror plugin on and
// REAL muster-mirror processes over the tunnel (the `-tags e2emirror` fake
// plays tmux only): host A (this rig's own), host B (connected beside it) and,
// later, host C — plus a dispatched agent of this spec's own (e2e/mkchat),
// which is on the host "cluster" with the seeded pod agents.
//
// The four facet dropdowns are gone; above the box is "All" and one tab per
// host, each with a count. A tab filters the ONE list (exactly: host A's tab
// never shows a host whose name merely contains A's), ANDs with the box, is
// remembered across a reload (localStorage, not ?q=), falls back to All when its
// host is gone, and a host that connects gets a tab.
//
// And THE FOCUS TITLE IS SHOWN WHOLE: this spec's agent carries a 120-character
// focus title, which the card, its popover and the agent page's focus history
// show untruncated and wrapped — never an ellipsis, never sideways overflow.
//
// 🔴 REGRESSION TESTS. On the commit before: there is no #agents-hosts (every
// tab test fails at its first locator), and the focus title is stored cut to
// 80 characters ending in "…" (internal/focus Clean), so the title tests fail
// on the exact text.

const rig = new MirrorRig(hostTabsPort, 'e2e-tabs-a', { MUSTER_FOCUS_INTERVAL: '0' });
const hostA = rig.host;
const hostB = rig.host.replace('e2e-tabs-a-', 'e2e-tabs-b-');
const hostC = rig.host.replace('e2e-tabs-a-', 'e2e-tabs-c-');
const hostD = rig.host.replace('e2e-tabs-a-', 'e2e-tabs-d-');
const agentName = 'e2e-tabs-' + Date.now().toString(36);
// 120 characters: past the old 80-character clamp, under the 300 storage guard.
const LONG = 'Untangling the reconnect backoff so a flapping tunnel stops resetting its retry window, then proving it with a soak test';

const t0 = Math.floor(Date.now() / 1000);
// session|id|index|active|command|activity|claude-transcript
const line = (session: string, id: string, ago: number) => `${session}|${id}|1|1|zsh|${t0 - ago}|`;
let filesA = '';
let filesB = '';
let filesC = '';
let filesD = '';
const writeTo = (f: string, lines: string[]) => {
  fs.writeFileSync(f + '.tmp', lines.length ? lines.join('\n') + '\n' : '');
  fs.renameSync(f + '.tmp', f); // atomic: the host never reads half a list
};
// Each of A and B has a window called "zebrafish", which the search finds on both.
const windowsA = () => [line('alpha', '@1', 100), line('zebrafish', '@2', 200)];
const windowsB = () => [line('beta', '@1', 150), line('zebrafish', '@2', 250)];

test.beforeAll(async () => {
  test.setTimeout(180_000);
  expect(LONG.length, 'the fixture title is 120 characters').toBe(120);
  mkchat(agentName, [{ role: 'user', content: 'Fix the tunnel backoff.' }], {
    MUSTER_E2E_FOCUS: JSON.stringify([{ subject: 'self', title: LONG, description: 'The retry window resets too early. One soak left.' }]),
  });
  await rig.start();
  filesA = rig.transcript('', 'windows-a');
  filesB = rig.transcript('', 'windows-b');
  filesC = rig.transcript('', 'windows-c');
  writeTo(filesA, windowsA());
  writeTo(filesB, windowsB());
  // Host C's one window is the OLDEST on the list, so its arrival lands at the
  // end and is drawn at once rather than held behind the pill.
  writeTo(filesC, [line('citrus', '@1', 90 * 86400)]);
  // Host D's one window is the NEWEST, so its arrival lands on top, among the
  // cards in view, and is held behind the "N new ↑" pill.
  filesD = rig.transcript('', 'windows-d');
  writeTo(filesD, [line('dateplum', '@1', 0)]);
  await rig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: filesA });
  rig.extraHost(hostB, { MUSTER_MIRROR_E2E_WINDOWS_FILE: filesB });
});

test.afterAll(async () => {
  await rig.stop();
  mkchat(agentName, null);
});

test.use({ storageState: { cookies: [], origins: [] } });

const strip = (page: Page) => page.locator('#agents-hosts');
const tab = (page: Page, host: string) => page.locator(`#agents-hosts [role="tab"][data-host="${host}"]`);
const allTab = (page: Page) => tab(page, '');
const cardsOn = (page: Page, host: string) => page.locator(`#agents-list > article[data-f-host="${host}"]`);
const shownOn = (page: Page, host: string) => cardsOn(page, host).filter({ visible: true });
const agentCard = (page: Page) => page.locator(`#agents-list > article[id^="agent-"]:has(a[href="/agents/${agentName}"])`);
const labels = (page: Page) => page.locator('#agents-hosts [role="tab"] [data-tab-label]').allTextContents();
const countOf = (t: Locator) => t.locator('[data-tab-count]').textContent();
const stored = (page: Page) => page.evaluate(() => localStorage.getItem('muster:tabs:agents-host'));

// openList signs in, opens the Agents list, and waits until both hosts and the
// agent are drawn (applying anything a background refresh held back).
async function openList(page: Page) {
  await rig.login(page);
  await rig.waitListed(page, 'beta');
  await rig.waitListed(page, 'alpha');
  await page.goto(rig.origin + '/agents');
  await expect
    .poll(async () => {
      await takeNew(page);
      return [await cardsOn(page, hostA).count(), await cardsOn(page, hostB).count(), await agentCard(page).count()];
    }, { timeout: 30_000, message: `both hosts' windows and the agent are listed (log:\n${rig.log})` })
    .toEqual([2, 2, 1]);
}

async function noPageOverflow(page: Page) {
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw, 'no sideways page scroll').toBeLessThanOrEqual(cw);
}

for (const vp of [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.name, () => {
    test.use(vp.use);
    const press = (l: Locator) => (vp.use.hasTouch ? l.tap() : l.click());

    test('one tab per host with its count; a tab filters the list exactly; the dropdowns are gone', async ({ page }) => {
      await openList(page);
      await expect(page.locator('#agents-filter select'), 'no dropdown filter is left').toHaveCount(0);
      // All, then each host by name — the pod agents under "cluster".
      await expect.poll(() => labels(page)).toEqual(['All', 'cluster', hostA, hostB]);
      await expect(allTab(page)).toHaveAttribute('aria-selected', 'true');
      // Each count is that host's cards; All's is every card.
      const total = await page.locator('#agents-list > article').count();
      const pods = await cardsOn(page, 'cluster').count();
      expect(pods, 'the agent (and any seeded pod agents) are on "cluster"').toBeGreaterThanOrEqual(1);
      expect(await countOf(allTab(page))).toBe(String(total));
      expect(await countOf(tab(page, 'cluster'))).toBe(String(pods));
      expect(await countOf(tab(page, hostA))).toBe('2');
      expect(await countOf(tab(page, hostB))).toBe('2');

      // Each tab a 44px target (measured before any is pressed: the press
      // feedback scales a tapped one for a moment).
      for (const t of await page.locator('#agents-hosts [role="tab"]').all()) {
        expect((await t.boundingBox())!.height, 'a tab is a 44px target').toBeGreaterThanOrEqual(44);
      }

      // Host A: its two windows, nothing else.
      await press(tab(page, hostA));
      await expect(tab(page, hostA)).toHaveAttribute('aria-selected', 'true');
      await expect(allTab(page)).toHaveAttribute('aria-selected', 'false');
      await expect(shownOn(page, hostA)).toHaveCount(2);
      await expect(shownOn(page, hostB)).toHaveCount(0);
      await expect(shownOn(page, 'cluster')).toHaveCount(0);
      await expect(page.locator('#agents-count')).toHaveText(`2 of ${total}`);
      expect(new URL(page.url()).searchParams.get('q'), 'the tab is not part of ?q=').toBeNull();
      // "cluster": the dispatched agent, and no mirrored window.
      await press(tab(page, 'cluster'));
      await expect(agentCard(page)).toBeVisible();
      await expect(page.locator('#agents-list > article[data-mirror-card]').filter({ visible: true })).toHaveCount(0);
      // Exactly one tab is selected, and it alone is in the tab order.
      await expect(page.locator('#agents-hosts [role="tab"][aria-selected="true"]')).toHaveCount(1);
      await expect(page.locator('#agents-hosts [role="tab"][tabindex="0"]')).toHaveCount(1);

      // Thumb-friendly, and at a phone's width the strip scrolls inside itself.
      const [sw, cw] = await strip(page).evaluate((e) => [e.scrollWidth, e.clientWidth]);
      if (vp.use.isMobile) expect(sw, 'positive control: at 390px the tabs overflow the strip').toBeGreaterThan(cw);
      expect(await strip(page).evaluate((e) => getComputedStyle(e).overflowX)).toBe('auto');
      await noPageOverflow(page);
      // The last tab is reachable by scrolling the strip, and works.
      await tab(page, hostB).scrollIntoViewIfNeeded();
      await press(tab(page, hostB));
      await expect(shownOn(page, hostB)).toHaveCount(2);
      await expect(shownOn(page, hostA)).toHaveCount(0);
      await noPageOverflow(page);
      await press(allTab(page));
      await expect(page.locator('#agents-list > article').filter({ visible: true })).toHaveCount(total);
    });

    test('the agent card shows its 120-character focus title whole, wrapped, in the card and its popover', async ({ page }) => {
      await openList(page);
      const card = agentCard(page);
      const btn = card.locator('[data-card-focus]');
      const title = btn.locator('span').first();
      await expect(title).toHaveText(LONG);
      expect(await title.textContent(), 'no ellipsis').not.toContain('…');
      // Wrapped, not clipped: on a phone it takes several lines, and nothing
      // overflows the button or the card sideways.
      const [tsw, tcw, th, lh] = await title.evaluate((e) => [e.scrollWidth, e.clientWidth, e.getBoundingClientRect().height, parseFloat(getComputedStyle(e).lineHeight)]);
      expect(tsw, 'the title does not overflow sideways').toBeLessThanOrEqual(tcw + 1);
      if (vp.use.isMobile) expect(th, 'on a phone the title wraps onto more than one line').toBeGreaterThan(lh * 1.5);
      const cb = (await card.boundingBox())!;
      const bb = (await btn.boundingBox())!;
      expect(bb.x + bb.width, 'the title fits its card').toBeLessThanOrEqual(cb.x + cb.width + 1);
      expect(cb.x + cb.width, 'the card fits the viewport').toBeLessThanOrEqual(vp.use.viewport.width);
      await noPageOverflow(page);

      await press(btn);
      const pop = card.locator('[data-focus-popover]');
      await expect(pop).toBeVisible();
      await expect(pop.locator('[data-focus-title]')).toHaveText(LONG);
      const pb = (await pop.boundingBox())!;
      expect(pb.x).toBeGreaterThanOrEqual(0);
      expect(pb.x + pb.width, 'the popover fits the viewport').toBeLessThanOrEqual(vp.use.viewport.width);
      expect(await pop.evaluate((e) => e.scrollWidth <= e.clientWidth + 1), 'the popover does not overflow sideways').toBe(true);
      await page.keyboard.press('Escape');
    });

    test("the agent page's focus history shows the 120-character title whole", async ({ page }) => {
      await rig.login(page);
      await page.goto(rig.origin + '/agents/' + agentName);
      await press(page.locator('#focus-history-open'));
      const dialog = page.locator('dialog#focus-history');
      await expect(dialog).toBeVisible();
      const first = dialog.locator('[data-focus-history-title]').first();
      await expect(first).toHaveText(LONG);
      expect(await first.evaluate((e) => e.scrollWidth <= e.clientWidth + 1), 'the history title does not overflow sideways').toBe(true);
      const db = (await dialog.boundingBox())!;
      expect(db.x + db.width).toBeLessThanOrEqual(vp.use.viewport.width);
      await page.keyboard.press('Escape');
    });
  });
}

test.describe('desktop 1280×800, behaviour', () => {
  test.use({ viewport: { width: 1280, height: 800 } });

  test('the arrow keys, Home and End move between tabs and choose them', async ({ page }) => {
    await openList(page);
    await expect.poll(() => labels(page)).toEqual(['All', 'cluster', hostA, hostB]);
    await allTab(page).focus();
    await page.keyboard.press('ArrowRight');
    await expect(tab(page, 'cluster')).toBeFocused();
    await expect(tab(page, 'cluster')).toHaveAttribute('aria-selected', 'true');
    await expect(shownOn(page, hostA)).toHaveCount(0);
    await page.keyboard.press('End');
    await expect(tab(page, hostB)).toBeFocused();
    await expect(shownOn(page, hostB)).toHaveCount(2);
    await page.keyboard.press('ArrowRight');
    await expect(allTab(page), 'Right wraps to the first tab').toBeFocused();
    await page.keyboard.press('ArrowLeft');
    await expect(tab(page, hostB), 'Left wraps to the last').toBeFocused();
    await page.keyboard.press('Home');
    await expect(allTab(page)).toHaveAttribute('aria-selected', 'true');
    await expect(shownOn(page, hostB)).toHaveCount(2);
    await expect(shownOn(page, hostA)).toHaveCount(2);
  });

  test('the search ANDs with the tab, and every tab counts what the search shows', async ({ page }) => {
    await openList(page);
    const box = page.locator('#agents-search');
    await box.fill('zebrafish');
    // All: the one zebrafish window on each host.
    await expect(shownOn(page, hostA)).toHaveCount(1);
    await expect(shownOn(page, hostB)).toHaveCount(1);
    await expect.poll(() => countOf(allTab(page))).toBe('2');
    expect(await countOf(tab(page, hostA))).toBe('1');
    expect(await countOf(tab(page, hostB))).toBe('1');
    expect(await countOf(tab(page, 'cluster'))).toBe('0');
    // The tabs stay, though one counts nothing.
    expect(await labels(page)).toEqual(['All', 'cluster', hostA, hostB]);

    await tab(page, hostB).click();
    await expect(shownOn(page, hostB)).toHaveCount(1);
    await expect(shownOn(page, hostA)).toHaveCount(0);
    const total = await page.locator('#agents-list > article').count();
    await expect(page.locator('#agents-count')).toHaveText(`1 of ${total}`);
    expect(new URL(page.url()).searchParams.get('q'), 'the box alone is ?q=').toBe('zebrafish');
    // Clearing the box keeps the tab.
    await page.locator('#agents-search-clear').click();
    await expect(shownOn(page, hostB)).toHaveCount(2);
    await expect(shownOn(page, hostA)).toHaveCount(0);
    await expect.poll(() => countOf(tab(page, hostA))).toBe('2');
    // A typed host token for the OTHER host, ANDed with the tab: nothing.
    await box.fill(`host:${hostA}`);
    await expect(page.locator('#agents-list > article').filter({ visible: true })).toHaveCount(0);
    await expect(page.locator('#agents-nomatch')).toBeVisible();
    await page.locator('#agents-search-clear').click();
    await allTab(page).click();
  });

  test('the chosen tab survives a reload and a new visit; a remembered host that is gone falls back to All', async ({ page }) => {
    await openList(page);
    await tab(page, hostB).click();
    expect(await stored(page)).toBe(hostB);
    await page.reload();
    await expect(tab(page, hostB)).toHaveAttribute('aria-selected', 'true', { timeout: 20_000 });
    await expect(shownOn(page, hostB)).toHaveCount(2);
    await expect(shownOn(page, hostA)).toHaveCount(0);
    await page.goto(rig.origin + '/tasks');
    await page.goto(rig.origin + '/agents');
    await expect(tab(page, hostB)).toHaveAttribute('aria-selected', 'true', { timeout: 20_000 });
    await expect(shownOn(page, hostA)).toHaveCount(0);

    // A remembered host no card is on: All, and the choice is forgotten.
    await page.evaluate(() => localStorage.setItem('muster:tabs:agents-host', 'e2e-tabs-gone-host'));
    await page.reload();
    await expect(cardsOn(page, hostA)).toHaveCount(2, { timeout: 20_000 });
    await expect(allTab(page)).toHaveAttribute('aria-selected', 'true');
    await expect(tab(page, 'e2e-tabs-gone-host'), 'no tab is made up for it').toHaveCount(0);
    await expect(shownOn(page, hostA)).toHaveCount(2);
    await expect(shownOn(page, hostB)).toHaveCount(2);
    await expect.poll(() => stored(page)).toBeNull();
  });

  test('the chosen host going away live falls back to All; coming back gives its tab back without choosing it', async ({ page }) => {
    await openList(page);
    await page.evaluate(() => ((window as unknown as { __noReload: number }).__noReload = 1));
    await tab(page, hostB).click();
    await expect(shownOn(page, hostA)).toHaveCount(0);
    writeTo(filesB, []);
    await expect(cardsOn(page, hostB), `host B's windows closed (log:\n${rig.log})`).toHaveCount(0, { timeout: 30_000 });
    await expect(tab(page, hostB), 'its tab is gone').toHaveCount(0);
    await expect(allTab(page)).toHaveAttribute('aria-selected', 'true');
    await expect(shownOn(page, hostA)).toHaveCount(2);
    await expect.poll(() => stored(page)).toBeNull();
    // Back: the tab returns, and All stays chosen.
    writeTo(filesB, windowsB());
    await expect
      .poll(async () => {
        await takeNew(page);
        return tab(page, hostB).count();
      }, { timeout: 30_000 })
      .toBe(1);
    await expect(allTab(page)).toHaveAttribute('aria-selected', 'true');
    await expect(shownOn(page, hostB)).toHaveCount(2);
    expect(await page.evaluate(() => (window as unknown as { __noReload?: number }).__noReload), 'no reload happened').toBe(1);
  });

  test('a host that connects gets a tab, live, in its place', async ({ page }) => {
    await openList(page);
    await page.evaluate(() => ((window as unknown as { __noReload: number }).__noReload = 1));
    await expect.poll(() => labels(page)).toEqual(['All', 'cluster', hostA, hostB]);
    rig.extraHost(hostC, { MUSTER_MIRROR_E2E_WINDOWS_FILE: filesC });
    await expect
      .poll(async () => {
        await takeNew(page);
        return labels(page);
      }, { timeout: 30_000, message: `host C connected (log:\n${rig.log})` })
      .toEqual(['All', 'cluster', hostA, hostB, hostC]);
    expect(await countOf(tab(page, hostC))).toBe('1');
    await tab(page, hostC).click();
    await expect(shownOn(page, hostC)).toHaveCount(1);
    await expect(shownOn(page, hostA)).toHaveCount(0);
    expect(await page.evaluate(() => (window as unknown as { __noReload?: number }).__noReload), 'no reload happened').toBe(1);
    await allTab(page).click();
  });
});

test.describe('desktop 1280×800, a held arrival', () => {
  test.use({ viewport: { width: 1280, height: 800 } });

  // A tab counts and offers only what is DRAWN: a host whose only card is an
  // arrival held behind the pill would be a tab that shows nothing.
  test("a host whose card is held behind the pill gets no tab until the tap draws it", async ({ page }) => {
    await openList(page);
    const pill = page.locator('#agents-new-pill');
    await expect(pill).toBeHidden();
    rig.extraHost(hostD, { MUSTER_MIRROR_E2E_WINDOWS_FILE: filesD });
    await expect(pill, `host D's arrival waits behind the pill (log:\n${rig.log})`).toHaveText(/^\d+ new ↑$/, { timeout: 30_000 });
    await expect(cardsOn(page, hostD), 'its card is in the list, held').toHaveCount(1);
    await expect(cardsOn(page, hostD)).toBeHidden();
    await expect(tab(page, hostD), 'a held arrival offers no tab').toHaveCount(0);
    const total = await page.locator('#agents-list > article:not([data-held])').count();
    expect(await countOf(allTab(page)), 'nor is it counted by All').toBe(String(total));
    await pill.click();
    await expect(tab(page, hostD), 'drawn, it has its tab').toHaveCount(1);
    expect(await countOf(tab(page, hostD))).toBe('1');
  });
});
