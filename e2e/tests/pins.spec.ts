import fs from 'node:fs';
import path from 'node:path';
import { BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { password, pinsPort } from '../lib/env';
import { MirrorRig, mkchat } from '../lib/mirrorhost';
import { writeFileAtomic } from '../lib/atomic';

// PINNED ENTRIES COME FIRST — AS LABEL-ONLY CHIPS — in a real browser, against
// a real muster-server (MUSTER_MIRROR=1, its own port) and a real muster-mirror
// over the tunnel (the `-tags e2emirror` per-window fake, as
// mirror-windows.spec.ts), at a phone's size and a desktop's.
//
// The operator asks: "Add 'pinned' agents so they appear first in list", then
// "Pinned agents should show only the 'pretty Tmux' label (for example mango,
// vapor, wheat) not the full agent card" (internal/ui/pinned_chips.go). The
// host gives two sessions SYNTHETIC codenames (quince, sable) through its
// labels file, so their windows are "quince:1" and "sable:1" and their chips
// read "quince" and "sable". The entries pinned are the LEAST recently active
// (the two older windows, the last seeded agent), so "it moved to the top"
// cannot be the activity order doing it. Pins are server-stored, so they must
// survive a list refresh AND a page reload.
//
// THE CHOICE THIS PINS: a chip is filtered exactly as its card would be — the
// search box and the host tabs hide it — and is unpinned from the pinned row's
// "Edit pins" toggle, which reveals a × on each chip.
//
// 🔴 THE AGENT PIN IS ON A SEEDED AGENT, WHICH EVERY SERVER IN THE SUITE LISTS
// (one database, one operator). The test unpins it again, and afterAll unpins
// it once more through the route, so no later spec sees it pinned.

// The server, its mirror token and the fake host are e2e/lib/mirrorhost.ts's.
const rig = new MirrorRig(pinsPort, 'e2e-pin');
const origin = rig.origin;
const host = rig.host;
const windowsFile = path.join(rig.dir, 'windows');
const labelsFile = path.join(rig.dir, 'labels.json');
let pinnedAgentID = '';

const now = Math.floor(Date.now() / 1000);
// session|id|index|active|command|activity|claude-transcript
// pinold has had no activity for 30 days: it is drawn in the collapsed Idle
// section (MUSTER_AGENTS_IDLE_AFTER defaults to 14 days).
const windows = [
  `pinx|@1|1|1|zsh|${now - 60}|`,
  `pins|@2|1|1|zsh|${now - 3600}|`,
  `pinq|@3|1|1|zsh|${now - 7200}|`,
  `pinold|@4|1|1|zsh|${now - 30 * 86400}|`,
];
const href = (session: string, id: number) => `/mirror/terminal?host=${host}&session=${session}&window=%40${id}`;
const unpinnedHref = href('pinx', 1);
const sableHref = href('pins', 2);
const quinceHref = href('pinq', 3);
const oldHref = href('pinold', 4);

test.beforeAll(async () => {
  await rig.start();
  writeFileAtomic(windowsFile, windows.join('\n') + '\n');
  fs.writeFileSync(labelsFile, JSON.stringify({ pinq: 'quince', pins: 'sable' }));
  await rig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: windowsFile, MUSTER_MIRROR_LABELS_FILE: labelsFile });
});

let signedIn: Awaited<ReturnType<BrowserContext['cookies']>> | undefined;

async function unpinRoute(body: string) {
  if (!signedIn) return;
  const cookie = signedIn.map((c) => `${c.name}=${c.value}`).join('; ');
  await fetch(origin + '/agents/pins/delete', {
    method: 'POST',
    headers: { Cookie: cookie, Origin: origin, 'Content-Type': 'application/x-www-form-urlencoded', 'HX-Request': 'true' },
    body,
  }).catch(() => undefined);
}

test.afterAll(async () => {
  // Belt and braces: the seeded agent must not stay pinned for later specs.
  if (pinnedAgentID) await unpinRoute(`kind=agent&agent_id=${pinnedAgentID}`);
  await rig.stop();
});

async function login(page: Page) {
  if (signedIn) {
    await page.context().addCookies(signedIn);
    return;
  }
  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
  signedIn = await page.context().cookies(origin);
}

// order is every entry of the list, top to bottom as DRAWN (chips and cards):
// "agent-<id>" for an agent, "win@<n>" for one of THIS host's windows; other
// hosts' entries are skipped. Read by position, so it is what the eye sees
// whatever the DOM order.
async function order(page: Page): Promise<string[]> {
  return page.locator('#agents-list > article').evaluateAll((els, host) => {
    const out: { k: string; top: number; left: number }[] = [];
    for (const e of els) {
      const r = e.getBoundingClientRect();
      if (!r.width && !r.height) continue;
      if (e.id.startsWith('agent-')) {
        out.push({ k: e.id, top: Math.round(r.top), left: r.left });
        continue;
      }
      const a = e.querySelector('a[href*="window="]') as HTMLAnchorElement | null;
      if (!a || !a.href.includes('host=' + host)) continue;
      out.push({ k: 'win@' + new URL(a.href).searchParams.get('window')!.slice(1), top: Math.round(r.top), left: r.left });
    }
    out.sort((a, b) => a.top - b.top || a.left - b.left);
    return out.map((x) => x.k);
  }, host);
}

const chip = (page: Page, h: string) => page.locator(`#agents-list > article[data-pinned-chip]:has(a[href="${h}"])`);
const card = (page: Page, h: string) => page.locator(`#agents-list > article[data-mirror-card]:has(a[href="${h}"])`);

// pinVia pins (or unpins) through the route, as ANOTHER tab would: this page
// hears nothing of it until its list refreshes.
async function pinVia(page: Page, on: boolean, form: Record<string, string>) {
  const r = await page.request.post(origin + (on ? '/agents/pins' : '/agents/pins/delete'), { form, headers: { Origin: origin } });
  expect(r.status(), `${on ? 'pin' : 'unpin'} ${JSON.stringify(form)}`).toBe(200);
}
const winForm = (session: string, id: number, h = host) => ({ kind: 'mirror', host: h, session, window: '@' + id });

async function openList(page: Page) {
  await login(page);
  await page.goto(origin + '/agents');
  await expect(card(page, quinceHref), `the host's windows (log:\n${rig.log})`).toHaveCount(1, { timeout: 20_000 });
  await expect(card(page, sableHref)).toHaveCount(1);
  await expect(card(page, unpinnedHref)).toHaveCount(1);
}

// noOverflow: the page never scrolls sideways.
async function noOverflow(page: Page, what: string) {
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw, `${what}: document scrollWidth`).toBeLessThanOrEqual(cw);
}

for (const vp of [
  { name: 'phone 390×844 touch', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.name, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });

    test('pinned entries become label-only chips at the top, filter like cards, open their session, and unpin', async ({ page }) => {
      await openList(page);
      const before = await order(page);
      const agentsBefore = before.filter((x) => x.startsWith('agent-'));
      expect(agentsBefore.length, 'the seeded agents are listed').toBeGreaterThanOrEqual(2);
      expect(before.filter((x) => x.startsWith('win@')), 'the windows by activity, unpinned').toEqual(['win@1', 'win@2', 'win@3']);
      expect(await page.locator('#agents-list [data-pinned]').count(), 'nothing pinned at the start').toBe(0);
      await expect(page.locator('#agents-pins-edit'), 'no pinned row with nothing pinned').toHaveCount(0);
      await noOverflow(page, 'unpinned');

      // Pin through the real controls: the LEAST recently active agent's ⋯ menu…
      const last = agentsBefore[agentsBefore.length - 1];
      pinnedAgentID = last.replace('agent-', '');
      // The name its card shows: the first of its titles (display name, name).
      const agentName = ((await page.locator(`#${last}`).getAttribute('data-rank-title')) ?? '').split('\n').find((t) => t)!;
      await page.locator(`#${last} [aria-label="Agent actions"]`).click();
      await page.locator(`#agent-menu-${pinnedAgentID}`).getByRole('button', { name: 'Pin to top' }).click();
      await expect(page.locator(`#${last}[data-pinned-chip="agent"]`), 'the pinned agent is a chip').toHaveCount(1);
      // …and the two older windows' pin toggles (44px targets).
      for (const h of [quinceHref, sableHref]) {
        const toggle = card(page, h).locator('[data-pin-toggle]');
        expect((await toggle.boundingBox())!.width, 'the toggle is a 44px target').toBeGreaterThanOrEqual(44);
        await toggle.click();
        await expect(chip(page, h)).toHaveCount(1);
      }

      // THE CHIPS: label only — the codename without its ":1", the agent's name
      // — and no card for a pinned entry.
      await expect(chip(page, quinceHref)).toHaveText('quince');
      await expect(chip(page, sableHref)).toHaveText('sable');
      expect(await page.locator(`#${last}`).innerText(), 'the agent chip shows its name alone').toBe(agentName);
      for (const h of [quinceHref, sableHref]) await expect(card(page, h), 'no card beside the chip').toHaveCount(0);
      await expect(page.locator('#agents-list > article[data-pinned-chip] :is([data-card-slot], [data-card-host], [data-card-focus])')).toHaveCount(0);
      for (const ic of await page.locator('#agents-list > article[data-pinned-chip] [data-host-icon]').all()) await expect(ic, 'a chip draws no host icon').toBeHidden();

      // First, by activity among themselves (the agent, seeded this run, is
      // newer than both windows), then the rest in the ONE flat order they had.
      const want = [last, 'win@2', 'win@3', ...before.filter((x) => x !== last && x !== 'win@2' && x !== 'win@3')];
      await expect.poll(() => order(page), { message: 'the chips, then the rest in their order' }).toEqual(want);

      // Geometry: 44px tall, side by side in rows ABOVE every card, the page
      // never scrolling sideways.
      const chips = page.locator('#agents-list > article[data-pinned-chip]');
      const boxes = await chips.evaluateAll((els) => els.map((e) => e.getBoundingClientRect().toJSON()));
      for (const b of boxes) {
        expect(b.height, 'a chip is a 44px target').toBeGreaterThanOrEqual(44);
        expect(b.right, 'a chip stays inside the viewport').toBeLessThanOrEqual(vp.use.viewport.width);
      }
      // Side by side: the "Edit pins" toggle and at least the first chip share
      // a row (the chips wrap onto more rows only as they need to).
      const head = (await page.locator('#agents-pins-head').boundingBox())!;
      expect(Math.round(boxes[0].top), 'the chips sit beside the Edit toggle, not under it').toBe(Math.round(head.y));
      expect(Math.max(...boxes.map((b) => b.height)), 'a chip is not stretched to a card\'s height').toBeLessThan(60);
      const firstCard = await page.locator('#agents-list > article:not([data-pinned-chip])').first().boundingBox();
      expect(firstCard!.y, 'the cards start below the chips').toBeGreaterThanOrEqual(Math.max(...boxes.map((b) => b.bottom)));
      await noOverflow(page, 'pinned');

      // A list refresh (what an SSE event or a lifecycle action does) keeps it.
      await page.evaluate(() => (window as any).htmx.trigger(document.body, 'agents:changed'));
      await page.waitForTimeout(500);
      expect(await order(page), 'after a list refresh').toEqual(want);
      // And a reload: the pins are the server's, not the page's.
      await page.reload();
      await expect.poll(() => order(page), { message: 'after a reload', timeout: 20_000 }).toEqual(want);

      // The host's tab still leads with its icon, though the host's first drawn
      // entry is now a chip, which draws none (syncHosts takes the icon from
      // the first entry that has one).
      await expect(page.locator(`#agents-hosts [role="tab"][data-host="${host}"] [data-host-icon]`)).toHaveCount(1);

      // FILTERS: a chip is filtered as its card is. This host's name is on the
      // windows and not on the agent…
      await page.fill('#agents-search', host);
      await expect(page.locator(`#${last}`)).toBeHidden();
      await expect(chip(page, quinceHref)).toBeVisible();
      // …a word no entry carries hides every chip, and the Edit toggle with them…
      await page.fill('#agents-search', 'zzz-no-such-entry');
      await expect(chips.first()).toBeHidden();
      await expect(page.locator('#agents-pins-edit')).toBeHidden();
      await page.fill('#agents-search', '');
      await expect(page.locator('#agents-pins-edit')).toBeVisible();
      // …and the host tabs: this host's tab hides the agent's chip, the
      // cluster's hides the windows'.
      await page.locator(`#agents-hosts [role="tab"][data-host="${host}"]`).click();
      await expect(page.locator(`#${last}`)).toBeHidden();
      await expect(chip(page, sableHref)).toBeVisible();
      await page.locator('#agents-hosts [role="tab"][data-host="cluster"]').click();
      await expect(chip(page, sableHref)).toBeHidden();
      await expect(page.locator(`#${last}`)).toBeVisible();
      await page.locator('#agents-hosts [role="tab"][data-host=""]').click();
      await expect(chips).toHaveCount(3);
      for (const c of await chips.all()) await expect(c).toBeVisible();

      // A tap opens the session — the card's own href.
      await chip(page, quinceHref).locator('a').click();
      await page.waitForURL((u) => u.pathname + u.search === quinceHref);
      expect(new URL(page.url()).searchParams.get('session')).toBe('pinq');
      await page.goto(origin + '/agents');
      await expect(chip(page, quinceHref)).toHaveCount(1, { timeout: 20_000 });

      // UNPIN: the × is hidden until "Edit pins" is pressed.
      const x = (label: string) => page.getByRole('button', { name: `Unpin ${label}`, exact: true });
      await expect(x('quince')).toBeHidden();
      const edit = page.locator('#agents-pins-edit');
      expect((await edit.boundingBox())!.width, 'Edit pins is a 44px target').toBeGreaterThanOrEqual(44);
      await edit.click();
      await expect(edit).toHaveAttribute('aria-pressed', 'true');
      await expect(x('quince')).toBeVisible();
      await noOverflow(page, 'editing pins');
      await x('quince').click();
      await expect(card(page, quinceHref), 'unpinned: a card again').toHaveCount(1);
      await expect(chip(page, quinceHref)).toHaveCount(0);
      // The edit state survives the list's re-render.
      await expect(edit).toHaveAttribute('aria-pressed', 'true');
      await x('sable').click();
      await expect(card(page, sableHref)).toHaveCount(1);
      await x(agentName).click();
      await expect(page.locator(`#${last}[data-pinned-chip]`)).toHaveCount(0);
      await expect.poll(() => order(page), { message: 'after unpinning all three' }).toEqual(before);
      expect(await page.locator('#agents-list [data-pinned]').count()).toBe(0);
      await expect(page.locator('#agents-pins-edit')).toHaveCount(0);
    });

    // A pinned AGENT keeps its lifecycle controls (operator review): behind
    // "Edit pins" its chip has the card's own ⋯ menu, and Stop from it stops
    // the agent while the chip stays a chip — whose status dot then says so.
    test('a pinned agent chip: label only, and behind Edit pins its ⋯ menu stops it and it stays pinned', async ({ page }) => {
      const name = 'e2e-pinag-' + Date.now().toString(36);
      mkchat(name, [{ role: 'user', content: 'pinned agent' }]);
      try {
        await login(page);
        await page.goto(origin + '/agents');
        const entry = page.locator(`#agents-list > article:has(a[href="/agents/${name}"])`);
        await expect(entry).toHaveCount(1, { timeout: 20_000 });
        const id = (await entry.getAttribute('id'))!.replace('agent-', '');
        await pinVia(page, true, { kind: 'agent', agent_id: id });
        // Started (the noop provisioner then reports it live, as
        // agent-cards.spec.ts relies on), so there is something to Stop.
        expect((await page.request.post(`${origin}/agents/${id}/start`, { headers: { Origin: origin } })).status()).toBe(200);
        const chipEl = page.locator(`#agent-${id}[data-pinned-chip="agent"]`);
        await expect
          .poll(async () => { await page.reload(); return chipEl.getAttribute('data-f-status'); }, { message: 'the pinned agent runs', timeout: 20_000 })
          .toBe('running');
        // Normal mode: the label, and nothing else visible.
        expect(await chipEl.innerText(), 'label only').toBe(name);
        await expect(chipEl.locator('[data-pinned-chip-status]'), 'no status dot while it runs').toHaveCount(0);
        await expect(chipEl.getByRole('button', { name: 'Agent actions' })).toBeHidden();
        // Edit pins: the ⋯ menu is there, and Stop works.
        await page.locator('#agents-pins-edit').click();
        const more = chipEl.getByRole('button', { name: 'Agent actions' });
        await expect(more).toBeVisible();
        expect((await more.boundingBox())!.width, 'the ⋯ is a 44px target').toBeGreaterThanOrEqual(44);
        await noOverflow(page, 'agent chip in edit mode');
        const menu = page.locator(`#agent-menu-${id}`);
        await more.click();
        await expect(menu).toBeVisible();
        await menu.getByRole('button', { name: 'Stop', exact: true }).click();
        // The stop lands (the provisioner settles it; a reload reads it), and
        // the agent is still pinned: a chip, never a card.
        await expect
          .poll(async () => { await page.reload(); return chipEl.getAttribute('data-f-status'); }, { message: 'stopped from the chip', timeout: 20_000 })
          .toBe('stopped');
        await expect(chipEl).toHaveAttribute('data-pinned-chip', 'agent');
        await expect(page.locator('#agents-list > article:not([data-pinned-chip])').filter({ has: page.locator(`a[href="/agents/${name}"]`) })).toHaveCount(0);
        // Out of edit mode (a reload starts out of it): the label again, with
        // the stopped dot beside it.
        await expect(page.locator('#agents-pins-edit')).toHaveAttribute('aria-pressed', 'false');
        await expect(chipEl.getByRole('button', { name: 'Agent actions' })).toBeHidden();
        await expect(chipEl.getByRole('img', { name: 'Agent stopped' })).toBeVisible();
        expect(await chipEl.innerText(), 'label only again').toBe(name);
      } finally {
        mkchat(name, null);
      }
    });

    // 🔴 A HELD REFRESH MUST NOT HIDE A NEW CHIP IN THE IDLE SECTION. Another
    // tab pins a window this tab draws in the collapsed Idle section; the
    // background refresh that brings the chip is applied at once, the chip is
    // in the pinned row and visible, and the cards keep their order. Red at
    // 5643e772: the hold kept the drawn section on the chip, which the
    // collapsed section then hid.
    test('another tab pins a window drawn in Idle: its chip shows in the pinned row, not held, cards keep their order', async ({ page }) => {
      await openList(page);
      const old = page.locator(`#agents-list > article:has(a[href="${oldHref}"])`);
      await expect(old).toHaveAttribute('data-idle-section', '');
      await expect(old, 'the Idle section is collapsed').toBeHidden();
      const before = await order(page);
      try {
        await pinVia(page, true, winForm('pinold', 4));
        await page.evaluate(() => (window as any).htmx.trigger(document.body, 'muster:resync'));
        await expect(chip(page, oldHref), 'the chip is drawn').toBeVisible();
        await expect(chip(page, oldHref)).toHaveText('pinold:1');
        await expect(chip(page, oldHref)).not.toHaveAttribute('data-idle-section', '');
        // Applied, not held: nothing waits behind the "N new" pill.
        await expect(page.locator('#agents-new-pill'), 'the refresh was not held').toBeHidden();
        await expect(chip(page, oldHref)).not.toHaveAttribute('data-held', '');
        const c = (await chip(page, oldHref).boundingBox())!;
        const firstCard = (await page.locator('#agents-list > article:not([data-pinned-chip])').first().boundingBox())!;
        expect(c.y + c.height, 'the chip is in the pinned row, above the cards').toBeLessThanOrEqual(firstCard.y);
        const now_ = (await order(page)).filter((k) => k !== 'win@4');
        expect(now_, 'no other entry moved').toEqual(before);
        // The stylesheet's own guard, reached directly: even a chip that DID
        // carry the Idle section's mark stays drawn, in the pinned row.
        // (Set, measured and removed in ONE task, so no list refresh can morph
        // the mark away before it is read.)
        const marked = await chip(page, oldHref).evaluate((e) => {
          e.setAttribute('data-idle-section', '');
          const r = e.getBoundingClientRect(), d = getComputedStyle(e).display;
          e.removeAttribute('data-idle-section');
          return { display: d, bottom: r.bottom, height: r.height };
        });
        expect(marked.display, 'a marked chip is still drawn').not.toBe('none');
        expect(marked.height).toBeGreaterThan(0);
        expect(marked.bottom, 'and still in the pinned row').toBeLessThanOrEqual(firstCard.y);
      } finally {
        await pinVia(page, false, winForm('pinold', 4));
      }
    });

    // A host whose EVERY entry is pinned still has its icon on its tab: the
    // chip carries the icon undrawn. Red at 5643e772: no chip had one.
    test('a host whose every entry is pinned keeps its tab icon', async ({ page }) => {
      const other = 'e2e-pinb-' + Date.now().toString(36);
      const otherWindows = path.join(rig.dir, 'windows-' + other);
      writeFileAtomic(otherWindows, `solo|@1|1|1|zsh|${now - 120}|\n`);
      const stop = rig.extraHost(other, { MUSTER_MIRROR_E2E_WINDOWS_FILE: otherWindows });
      try {
        await login(page);
        await page.goto(origin + '/agents');
        const soloHref = `/mirror/terminal?host=${other}&session=solo&window=%401`;
        await expect(page.locator(`#agents-list > article:has(a[href="${soloHref}"])`)).toHaveCount(1, { timeout: 20_000 });
        await pinVia(page, true, winForm('solo', 1, other));
        await page.reload();
        await expect(chip(page, soloHref)).toBeVisible({ timeout: 20_000 });
        await expect(page.locator(`#agents-list > article[data-f-host="${other}"]:not([data-pinned-chip])`), 'every entry of the host is a chip').toHaveCount(0);
        await expect(page.locator(`#agents-hosts [role="tab"][data-host="${other}"] [data-host-icon]`)).toHaveCount(1);
        await expect(page.locator(`#agents-hosts [role="tab"][data-host="${other}"] [data-host-icon]`)).toBeVisible();
      } finally {
        await pinVia(page, false, winForm('solo', 1, other));
        await stop();
      }
    });

    // Unpinning by KEYBOARD keeps focus in the row: the next chip's ×, and when
    // the last chip goes, the search box — never <body>. Red at 5643e772.
    test('keyboard unpin moves focus to the next chip, then to the search box', async ({ page }) => {
      await login(page);
      await pinVia(page, true, winForm('pins', 2));
      try {
        await pinVia(page, true, winForm('pinq', 3));
        await page.goto(origin + '/agents');
        await expect(chip(page, sableHref)).toHaveCount(1, { timeout: 20_000 });
        await page.locator('#agents-pins-edit').focus();
        await page.keyboard.press('Enter');
        await expect(page.locator('#agents-pins-edit')).toHaveAttribute('aria-pressed', 'true');
        const first = page.locator('#agents-list > article[data-pinned-chip]').first();
        const firstLabel = (await first.locator('[data-pinned-chip-label]').textContent())!.trim();
        const secondLabel = firstLabel === 'sable' ? 'quince' : 'sable';
        await first.locator('[data-pinned-chip-unpin]').focus();
        await page.keyboard.press('Enter');
        await expect(chip(page, firstLabel === 'sable' ? sableHref : quinceHref)).toHaveCount(0);
        await expect.poll(() => page.evaluate(() => document.activeElement?.getAttribute('aria-label') ?? document.activeElement?.tagName),
          { message: 'focus is on the next chip\'s ×' }).toBe(`Unpin ${secondLabel}`);
        await page.keyboard.press('Enter');
        await expect(page.locator('#agents-list > article[data-pinned-chip]')).toHaveCount(0);
        await expect.poll(() => page.evaluate(() => document.activeElement?.id || document.activeElement?.tagName),
          { message: 'focus is on the search box' }).toBe('agents-search');
      } finally {
        await pinVia(page, false, winForm('pins', 2));
        await pinVia(page, false, winForm('pinq', 3));
      }
    });

    // A filter that hides every chip leaves no gap above the cards: the line
    // break after the chips is drawn only while a chip is shown. Red at
    // 5643e772: a 12px gap (the row gap around the zero-height break).
    test('a filter hiding every chip leaves no gap above the cards', async ({ page }) => {
      await login(page);
      await pinVia(page, true, winForm('pinq', 3));
      try {
        await page.goto(origin + '/agents');
        await expect(chip(page, quinceHref)).toBeVisible({ timeout: 20_000 });
        // Only the unpinned window: the chip is hidden by the search.
        await page.fill('#agents-search', 'pinx');
        await expect(chip(page, quinceHref)).toBeHidden();
        await expect(card(page, unpinnedHref)).toBeVisible();
        const list = (await page.locator('#agents-list').boundingBox())!;
        const c = (await card(page, unpinnedHref).boundingBox())!;
        expect(c.y - list.y, 'the first shown card starts at the top of the list').toBeLessThan(1);
        await page.fill('#agents-search', '');
        await expect(chip(page, quinceHref)).toBeVisible();
      } finally {
        await pinVia(page, false, winForm('pinq', 3));
      }
    });
  });
}
