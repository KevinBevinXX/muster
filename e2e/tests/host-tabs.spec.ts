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
// And THE FOCUS TITLE IS STORED AND SHOWN WHOLE WHERE IT IS READ: this spec's
// agent carries a 120-character focus title, which the card's popover and the
// agent page's focus history show untruncated and wrapped. The CARD's heading
// clamps it to 60 characters and ends in the "…" button that opens that
// popover (operator ask: long card titles are truncated, the ellipsis tapped to
// open the full title — agents-list-ux.spec.ts owns the clamp); never sideways
// overflow anywhere.
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
// Sorts between host A and host B ('-' < 'b').
const hostAB = rig.host.replace('e2e-tabs-a-', 'e2e-tabs-ab-');
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
let filesAB = '';
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
  writeTo(filesD, [line('zebrafishd', '@1', 0)]);
  filesAB = rig.transcript('', 'windows-ab');
  writeTo(filesAB, [line('abacus', '@1', 0)]);
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

    test('the agent card clamps its 120-character focus title; the "…" opens it whole, wrapped, in the popover', async ({ page }) => {
      await openList(page);
      const card = agentCard(page);
      const btn = card.locator('[data-card-focus]');
      const title = btn.locator('span').first();
      await expect(title).toHaveText(LONG.slice(0, 60).trimEnd());
      await expect(btn).toHaveAttribute('aria-label', `Focus: ${LONG} — show details`);
      const more = card.locator('[data-card-slot="focus"] [data-title-more]');
      await expect(more).toBeVisible();
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

      const before = page.url();
      await press(more);
      expect(page.url(), 'the "…" does not navigate').toBe(before);
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

test.describe('phone 390×844, the strip', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });
  const tapOn = (l: Locator) => l.tap();

test('one tab per host with its count; a tab filters; the dropdowns are gone; the strip scrolls, not the page', async ({ page }) => {
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
    await tapOn(tab(page, hostA));
    await expect(tab(page, hostA)).toHaveAttribute('aria-selected', 'true');
    await expect(allTab(page)).toHaveAttribute('aria-selected', 'false');
    await expect(shownOn(page, hostA)).toHaveCount(2);
    await expect(shownOn(page, hostB)).toHaveCount(0);
    await expect(shownOn(page, 'cluster')).toHaveCount(0);
    await expect(page.locator('#agents-count')).toHaveText(`2 of ${total}`);
    expect(new URL(page.url()).searchParams.get('q'), 'the tab is not part of ?q=').toBeNull();
    // "cluster": the dispatched agent, and no mirrored window.
    await tapOn(tab(page, 'cluster'));
    await expect(agentCard(page)).toBeVisible();
    await expect(page.locator('#agents-list > article[data-mirror-card]').filter({ visible: true })).toHaveCount(0);
    // Exactly one tab is selected, and it alone is in the tab order.
    await expect(page.locator('#agents-hosts [role="tab"][aria-selected="true"]')).toHaveCount(1);
    await expect(page.locator('#agents-hosts [role="tab"][tabindex="0"]')).toHaveCount(1);

    // Thumb-friendly, and at a phone's width the strip scrolls inside itself.
    const [sw, cw] = await strip(page).evaluate((e) => [e.scrollWidth, e.clientWidth]);
    expect(sw, 'positive control: at 390px the tabs overflow the strip').toBeGreaterThan(cw);
    expect(await strip(page).evaluate((e) => getComputedStyle(e).overflowX)).toBe('auto');
    await noPageOverflow(page);
    // The last tab is reachable by scrolling the strip, and works.
    await tab(page, hostB).scrollIntoViewIfNeeded();
    await tapOn(tab(page, hostB));
    await expect(shownOn(page, hostB)).toHaveCount(2);
    await expect(shownOn(page, hostA)).toHaveCount(0);
    await noPageOverflow(page);
    await tapOn(allTab(page));
    await expect(page.locator('#agents-list > article').filter({ visible: true })).toHaveCount(total);
  });

  // The selected tab is scrolled into the strip — the strip only, not the
  // page — when it is chosen and when a load restores it.
  test('the selected tab is brought into the strip on load, without scrolling the page', async ({ page }) => {
    await openList(page);
    await tab(page, hostB).scrollIntoViewIfNeeded();
    await tab(page, hostB).tap();
    await page.reload();
    await expect(tab(page, hostB)).toHaveAttribute('aria-selected', 'true', { timeout: 20_000 });
    const inside = () =>
      page.evaluate((h) => {
        const s = document.getElementById('agents-hosts')!;
        const t = s.querySelector(`[role="tab"][data-host="${h}"]`)!;
        const sr = s.getBoundingClientRect(), tr = t.getBoundingClientRect();
        return [s.scrollLeft > 0, tr.left >= sr.left - 1 && tr.right <= sr.right + 1, window.scrollY];
      }, hostB);
    expect(await inside(), 'scrolled into the strip, the page not scrolled').toEqual([true, true, 0]);
    // Picking a tab off the strip's left edge brings it in too.
    await strip(page).evaluate((e) => (e.scrollLeft = e.scrollWidth));
    await allTab(page).evaluate((e) => (e as HTMLElement).click());
    expect(await strip(page).evaluate((e) => e.scrollLeft), 'All, the first tab, is scrolled back into view').toBe(0);
  });
});

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

  test('the chosen tab survives a reload and a new visit; a remembered host that is absent shows All and is kept', async ({ page }) => {
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

    // A remembered host no card is on: the list SHOWS All, and the choice is
    // kept for when that host is back.
    await page.evaluate(() => localStorage.setItem('muster:tabs:agents-host', 'e2e-tabs-gone-host'));
    await page.reload();
    await expect(cardsOn(page, hostA)).toHaveCount(2, { timeout: 20_000 });
    await expect(allTab(page)).toHaveAttribute('aria-selected', 'true');
    await expect(tab(page, 'e2e-tabs-gone-host'), 'no tab is made up for it').toHaveCount(0);
    await expect(shownOn(page, hostA)).toHaveCount(2);
    await expect(shownOn(page, hostB)).toHaveCount(2);
    expect(await stored(page), 'the choice is kept').toBe('e2e-tabs-gone-host');
    // Only the reader's pick changes it.
    await allTab(page).click();
    expect(await stored(page)).toBeNull();
  });

  // A LINK WITH ITS OWN ?q= starts on All, so it shows what it was shared to
  // show; the stored tab is left as it is. A reload of this tab's own filter is
  // not a link.
  test('a shared ?q= link starts on All and keeps the stored tab; a reload of your own filter keeps the tab', async ({ page }) => {
    await openList(page);
    await tab(page, hostB).click();
    await page.goto(rig.origin + '/agents?q=zebrafish');
    await expect(cardsOn(page, hostA)).toHaveCount(2, { timeout: 20_000 });
    await expect(allTab(page)).toHaveAttribute('aria-selected', 'true');
    await expect(shownOn(page, hostA)).toHaveCount(1);
    await expect(shownOn(page, hostB)).toHaveCount(1);
    expect(await stored(page), 'the stored tab is untouched').toBe(hostB);
    // The same URL again is this tab's own filter now: the stored tab applies.
    await page.reload();
    await expect(tab(page, hostB)).toHaveAttribute('aria-selected', 'true', { timeout: 20_000 });
    await expect(shownOn(page, hostB)).toHaveCount(1);
    await expect(shownOn(page, hostA)).toHaveCount(0);
    await page.locator('#agents-search-clear').click();
    await allTab(page).click();
  });

  // THE DISCONNECT-THEN-RECONNECT CASE (a restart, a laptop asleep): the
  // chosen host's windows go away, the list shows All with the choice KEPT,
  // and when the host is back the list is filtered to it again.
  test('the chosen host going away shows All and keeps the choice; coming back restores its tab', async ({ page }) => {
    await openList(page);
    await page.evaluate(() => ((window as unknown as { __noReload: number }).__noReload = 1));
    await tab(page, hostB).click();
    await expect(shownOn(page, hostA)).toHaveCount(0);
    writeTo(filesB, []);
    await expect(cardsOn(page, hostB), `host B's windows closed (log:\n${rig.log})`).toHaveCount(0, { timeout: 30_000 });
    await expect(tab(page, hostB), 'its tab is gone').toHaveCount(0);
    await expect(allTab(page)).toHaveAttribute('aria-selected', 'true');
    await expect(shownOn(page, hostA)).toHaveCount(2);
    expect(await stored(page), 'the choice is kept').toBe(hostB);
    // Back — while the reader is looking at host A's alpha window through a
    // search that hides B's. Switching the view to B now would take alpha away
    // with no input, so the refresh is HELD: B's cards wait (not drawn), the
    // tab stays All, alpha stays, and the pill offers the change.
    const box = page.locator('#agents-search');
    const pill = page.locator('#agents-new-pill');
    await box.fill('alpha');
    await expect(page.locator('#agents-list > article').filter({ visible: true })).toHaveCount(1);
    writeTo(filesB, windowsB());
    await expect(cardsOn(page, hostB), `host B's windows are back (log:\n${rig.log})`).toHaveCount(2, { timeout: 30_000 });
    await expect(pill, 'the switch waits behind the pill').toHaveText('1 new ↑');
    await expect(allTab(page), 'no switch until the tap').toHaveAttribute('aria-selected', 'true');
    await expect(shownOn(page, hostA), 'what the reader was looking at stays').toHaveCount(1);
    await expect(cardsOn(page, hostB).first()).toHaveAttribute('data-held', '');
    expect(await stored(page)).toBe(hostB);
    // The tap applies it: B's tab is the view again.
    await pill.click();
    await expect(tab(page, hostB)).toHaveAttribute('aria-selected', 'true');
    await expect(pill).toBeHidden();
    await page.locator('#agents-search-clear').click();
    await expect(shownOn(page, hostB)).toHaveCount(2);
    await expect(shownOn(page, hostA)).toHaveCount(0);
    expect(await page.evaluate(() => (window as unknown as { __noReload?: number }).__noReload), 'no reload happened').toBe(1);
    await allTab(page).click();
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

  // 🔴 THE PILL AND THE HOLD COUNT ONLY WHAT THE FILTER SHOWS. On host B's tab,
  // an arrival on host A and a whole new host move nothing the reader can see:
  // no pill, the refresh is applied at once, and the hidden tabs and counts
  // follow it live. The new tab lands mid-strip, and the focused tab keeps
  // its focus through the move.
  test("on one host's tab, changes on other hosts apply at once: no pill, their counts and a new host's tab update live", async ({ page }) => {
    await openList(page);
    const pill = page.locator('#agents-new-pill');
    await tab(page, hostB).click();
    await expect(tab(page, hostB)).toBeFocused();
    await expect(shownOn(page, hostA)).toHaveCount(0);
    // An arrival on host A, newest on the list (it would be drawn on top).
    writeTo(filesA, [...windowsA(), line('apricot', '@3', 0)]);
    await expect(tab(page, hostA).locator('[data-tab-count]'), `host A's count follows (log:\n${rig.log})`).toHaveText('3', { timeout: 30_000 });
    await expect(pill, 'nothing the filter shows changed: no pill').toBeHidden();
    await expect(page.locator('#agents-list > article[data-held]')).toHaveCount(0);
    // A new host, newest too, whose name sorts between A and B.
    rig.extraHost(hostAB, { MUSTER_MIRROR_E2E_WINDOWS_FILE: filesAB });
    await expect(tab(page, hostAB), `host AB's tab, at once (log:\n${rig.log})`).toHaveCount(1, { timeout: 30_000 });
    await expect(pill).toBeHidden();
    const ls = await labels(page);
    expect(ls.indexOf(hostAB), `host AB's tab sits between A's and B's: ${ls}`).toBe(ls.indexOf(hostA) + 1);
    expect(ls.indexOf(hostB)).toBe(ls.indexOf(hostAB) + 1);
    await expect(tab(page, hostB), 'the focused tab kept focus through the move').toBeFocused();
    await expect(shownOn(page, hostB)).toHaveCount(2);
    await expect(shownOn(page, hostAB)).toHaveCount(0);
    writeTo(filesA, windowsA());
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
    // Narrow the list to the zebrafish windows, so the cards in view are ones
    // the newest arrival (host D's zebrafishd, which the search shows too)
    // lands ABOVE — whatever else (pod agents from other specs) the shared
    // database has put on the list — and it is held.
    await page.locator('#agents-search').fill('zebrafish');
    await expect(page.locator('#agents-list > article').filter({ visible: true })).toHaveCount(2);
    rig.extraHost(hostD, { MUSTER_MIRROR_E2E_WINDOWS_FILE: filesD });
    await expect(cardsOn(page, hostD), `host D's card is in the list (log:\n${rig.log})`).toHaveCount(1, { timeout: 30_000 });
    await expect(pill, "host D's arrival waits behind the pill").toHaveText(/^\d+ new ↑$/);
    await expect(cardsOn(page, hostD)).toHaveAttribute('data-held', '');
    await expect(tab(page, hostD), 'a held arrival offers no tab').toHaveCount(0);
    expect(await countOf(allTab(page)), 'nor is it counted by All').toBe('2');
    await page.locator('#agents-search-clear').click();
    await expect(tab(page, hostD), 'still none with the box cleared').toHaveCount(0);
    await pill.click();
    await expect(tab(page, hostD), 'drawn, it has its tab').toHaveCount(1);
    expect(await countOf(tab(page, hostD))).toBe('1');
  });

  // The pill counts what WAITS for the reader — of the cards the filter shows.
  // One refresh both lifts a shown card (host B's zebrafish becomes the newest)
  // and brings a card the search hides (kiwiberry): one waits, not two.
  test('with a search typed, the pill counts only what the search shows', async ({ page }) => {
    await openList(page);
    const pill = page.locator('#agents-new-pill');
    await page.locator('#agents-search').fill('session=zebrafish');
    await expect(page.locator('#agents-list > article').filter({ visible: true })).toHaveCount(2);
    const before = await page.locator('#agents-list > article').filter({ visible: true }).evaluateAll((els) => els.map((e) => e.getAttribute('data-f-host')));
    expect(before, 'host A\'s zebrafish is drawn first').toEqual([hostA, hostB]);
    writeTo(filesB, [line('beta', '@1', 150), line('zebrafish', '@2', 0), line('kiwiberry', '@3', 0)]);
    await expect(cardsOn(page, hostB), `host B's new window is listed (log:\n${rig.log})`).toHaveCount(3, { timeout: 30_000 });
    await expect(pill).toHaveText('1 new ↑');
    // The count follows the filter DURING the hold: with the search cleared
    // the arrival it hid waits too; typed back, it does not.
    await page.locator('#agents-search').fill('');
    await expect(pill, 'recounted for the cleared search').toHaveText('2 new ↑');
    await page.locator('#agents-search').fill('session=zebrafish');
    await expect(pill, 'recounted for the search typed back').toHaveText('1 new ↑');
    await pill.click();
    await expect(pill).toBeHidden();
    const after = await page.locator('#agents-list > article').filter({ visible: true }).evaluateAll((els) => els.map((e) => e.getAttribute('data-f-host')));
    expect(after, 'the tap applied the lift').toEqual([hostB, hostA]);
    writeTo(filesB, windowsB());
    await page.locator('#agents-search-clear').click();
  });
});
