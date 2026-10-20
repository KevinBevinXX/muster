import fs from 'node:fs';
import { Page } from '@playwright/test';
import { test, expect, takeNew } from '../lib/fixtures';
import { hostIconsPort } from '../lib/env';
import { MirrorRig } from '../lib/mirrorhost';

// THE AGENTS TAB, ON A REAL SERVER WITH TWO MIRRORED HOSTS (the e2eterm binary,
// mirror plugin on, two real muster-mirror processes; only tmux is faked), plus
// the seeded dispatched agents, whose host is "cluster".
//
// 1. HOST ICONS (internal/ui/host_icons.go). Every listed agent draws its
//    host's icon beside its name — the host in its tooltip and accessible name —
//    and two hosts draw two DIFFERENT icons, the cluster a third. The same host
//    draws the same icon on its cards, its "Recently viewed" entries, its host
//    tab and its window's own page.
//
// 2. SCROLL POSITIONS SURVIVE HYDRATION (internal/ui/agents_scroll.go). The
//    page's offset (the document scrolls the list) and the strip's sideways one
//    are kept across the list's background refreshes, a reload and a Back —
//    including while the list's own load is SLOW, held here for 3 s by a route.
//
// 🔴 REGRESSION TESTS. On the commit before: no card carries [data-host-icon]
// (the icon test fails at its first count); every list render REPLACED the
// strip out of band, so its scrollLeft read 0 after any refresh; and with the
// list's load held 3 s a reload and a Back landed at scrollY 0 from 700 (the
// restore gave up after its 2 s budget against a one-screen document).

const rig = new MirrorRig(hostIconsPort, 'e2e-icons-a');
const hostA = rig.host;
const hostB = rig.host.replace('e2e-icons-a-', 'e2e-icons-b-');
const t0 = Math.floor(Date.now() / 1000);
// session|id|index|active|command|activity|claude-transcript — 14 windows a
// host, so the list runs several screens on a phone.
const line = (session: string, id: string, ago: number) => `${session}|${id}|1|1|zsh|${t0 - ago}|`;
const windowsA = Array.from({ length: 14 }, (_, i) => line(`alpha${i}`, `@${i + 1}`, 100 + i));
const windowsB = Array.from({ length: 14 }, (_, i) => line(`beta${i}`, `@${i + 1}`, 150 + i));

test.beforeAll(async () => {
  test.setTimeout(180_000);
  await rig.start();
  const fa = rig.transcript('', 'windows-a');
  const fb = rig.transcript('', 'windows-b');
  fs.writeFileSync(fa, windowsA.join('\n') + '\n');
  fs.writeFileSync(fb, windowsB.join('\n') + '\n');
  await rig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: fa });
  rig.extraHost(hostB, { MUSTER_MIRROR_E2E_WINDOWS_FILE: fb });
});

test.afterAll(async () => {
  await rig.stop();
});

test.use({ storageState: { cookies: [], origins: [] }, viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

const cardsOn = (page: Page, host: string) => page.locator(`#agents-list > article[data-f-host="${host}"]`);
const iconKeys = (page: Page, host: string) =>
  cardsOn(page, host).locator('[data-card-slot="header"] [data-host-icon]').evaluateAll((els) => els.map((e) => e.getAttribute('data-host-icon')));
const strip = (page: Page) => page.locator('#agents-recent');
const stripScroller = (page: Page) => page.locator('#agents-recent > div');

// open signs in, visits six of host A's windows (so the strip has six entries,
// wider than a phone) and opens the list with every card drawn.
async function open(page: Page) {
  await rig.login(page);
  await rig.waitListed(page, 'alpha13');
  await rig.waitListed(page, 'beta13');
  for (let i = 0; i < 6; i++) await page.goto(rig.origin + rig.terminalPath(`alpha${i}`));
  await page.goto(rig.origin + '/agents');
  await drawn(page);
  await expect(strip(page).locator('a[data-recent-entry]')).toHaveCount(6);
}

// drawn waits until both hosts' 14 windows and the seeded agents are drawn.
async function drawn(page: Page) {
  await expect
    .poll(async () => {
      await takeNew(page);
      return [await cardsOn(page, hostA).count(), await cardsOn(page, hostB).count(), (await cardsOn(page, 'cluster').count()) > 0];
    }, { timeout: 30_000, message: `both hosts' windows and the cluster's agents are listed (log:\n${rig.log})` })
    .toEqual([14, 14, true]);
}

const pos = (page: Page) =>
  page.evaluate(() => ({
    y: Math.round(window.scrollY),
    x: Math.round((document.querySelector('#agents-recent > div') as HTMLElement | null)?.scrollLeft ?? -1),
  }));

// scrollBoth puts the page at 700 and the strip at 200 — and proves both CAN
// be (a document or a strip too short to scroll would read 0 for any code).
async function scrollBoth(page: Page) {
  await stripScroller(page).evaluate((e) => (e.scrollLeft = 200));
  await page.evaluate(() => window.scrollTo(0, 700));
  await expect.poll(() => pos(page), 'control: the page and the strip both scroll that far').toEqual({ y: 700, x: 200 });
}

test.describe('host icons', () => {
  test('two hosts draw two different icons, the cluster a third; each host the same one on every card, entry, tab and page', async ({ page }) => {
    await open(page);
    const a = await iconKeys(page, hostA);
    const b = await iconKeys(page, hostB);
    const c = await iconKeys(page, 'cluster');
    expect(a, 'every card of host A draws an icon').toHaveLength(14);
    expect(b, 'every card of host B draws an icon').toHaveLength(14);
    expect(new Set(a).size, "one icon for all of host A's cards").toBe(1);
    expect(new Set(b).size, "one icon for all of host B's cards").toBe(1);
    expect(new Set(c), "the dispatched agents draw the cluster's icon").toEqual(new Set(['cluster']));
    expect(a[0], 'two hosts, two icons').not.toBe(b[0]);
    expect([a[0], b[0]], "no host draws the cluster's icon").not.toContain('cluster');

    // The host is the tooltip and the accessible name, and the box is the
    // reserved 16px square beside the name.
    const mark = cardsOn(page, hostA).first().locator('[data-card-slot="header"] [data-host-icon]');
    await expect(mark).toHaveAttribute('title', `Host: ${hostA}`);
    await expect(mark).toHaveAttribute('aria-label', `Host: ${hostA}`);
    await expect(mark).toHaveAttribute('role', 'img');
    const box = (await mark.boundingBox())!;
    expect([Math.round(box.width), Math.round(box.height)], 'a 16px box').toEqual([16, 16]);
    await expect(page.locator(`#agents-list > article[data-f-host="cluster"] [data-host-icon="cluster"]`).first()).toHaveAttribute('title', 'Host: cluster');

    // The same icon everywhere host A appears: its strip entries, its host tab
    // and its window's own page.
    const entries = strip(page).locator(`a[data-recent-entry="mirror"] [data-host-icon]`);
    expect(new Set(await entries.evaluateAll((els) => els.map((e) => e.getAttribute('data-host-icon'))))).toEqual(new Set([a[0]]));
    await expect(page.locator(`#agents-hosts [role="tab"][data-host="${hostA}"] [data-host-icon]`)).toHaveAttribute('data-host-icon', a[0]!);
    await expect(page.locator(`#agents-hosts [role="tab"][data-host="${hostB}"] [data-host-icon]`)).toHaveAttribute('data-host-icon', b[0]!);
    const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
    expect(sw, 'no sideways page scroll at 390px').toBeLessThanOrEqual(cw);

    // (rig.terminalPath names host A; this is host B's window.)
    await page.goto(`${rig.origin}/mirror/terminal?host=${encodeURIComponent(hostB)}&session=beta2`);
    await expect(page.locator('[data-term-badge] [data-host-icon]')).toHaveAttribute('data-host-icon', b[0]!);
  });
});

test.describe('scroll positions', () => {
  test("a background refresh and the reader's own keep the page's and the strip's offsets", async ({ page }) => {
    await open(page);
    for (const ev of ['muster:resync', 'agents:changed']) {
      await scrollBoth(page);
      await page.evaluate(() => {
        (window as any).__settled = false;
        document.addEventListener('htmx:afterSettle', (e) => {
          if ((e.target as Element).id === 'agents-list') (window as any).__settled = true;
        });
      });
      // 🔴 THE KEPT OFFSET IS DROPPED FIRST, so this case sees the MORPH alone:
      // the strip's restore (agents_scroll.go put) would also put a REPLACED
      // scroller back at 200, and with it this test passed against an
      // outerHTML replace (measured: that mutant survived).
      // (Two frames first: the scroll event that writes it is dispatched on one.)
      await page.evaluate(
        () => new Promise<void>((r) => requestAnimationFrame(() => requestAnimationFrame(() => (sessionStorage.removeItem('muster.agents.recent.x'), r())))),
      );
      await page.evaluate((name) => document.body.dispatchEvent(new CustomEvent(name)), ev);
      await expect.poll(() => page.evaluate(() => (window as any).__settled), `control: ${ev} re-rendered the list`).toBe(true);
      await page.waitForTimeout(300);
      expect(await pos(page), `after ${ev}`).toEqual({ y: 700, x: 200 });
    }
  });

  for (const how of ['reload', 'back'] as const) {
    test(`a ${how} keeps both offsets, through a list load held 3 s`, async ({ page }) => {
      await open(page);
      let hold = 0;
      await page.route('**/ui/agents', async (r) => {
        if (hold) await new Promise((res) => setTimeout(res, hold));
        await r.continue().catch(() => undefined);
      });
      await scrollBoth(page);
      hold = 3000;
      if (how === 'reload') {
        await page.reload({ waitUntil: 'domcontentloaded' });
      } else {
        await page.goto(rig.origin + rig.terminalPath('alpha7'));
        await page.goBack({ waitUntil: 'domcontentloaded' });
      }
      // Hydrating: the list is still empty, and the offsets are already where
      // they were — no frame at 0 waiting for the cards.
      await page.waitForTimeout(1000);
      expect(await cardsOn(page, hostA).count(), 'control: the list has not loaded yet').toBe(0);
      // (soft, so a failure here still reports the hydrated state below.)
      expect.soft(await pos(page), `while the list hydrates after a ${how}`).toEqual({ y: 700, x: 200 });
      // Hydrated, past appScript's 2 s restore budget: still there.
      await drawn(page);
      await page.waitForTimeout(500);
      expect(await pos(page), `after the list hydrated (${how})`).toEqual({ y: 700, x: 200 });
      hold = 0;
    });
  }
});
