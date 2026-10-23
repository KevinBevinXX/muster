import { Locator, Page } from '@playwright/test';
import { test, expect, takeNew } from '../lib/fixtures';
import { hostNamesPort } from '../lib/env';
import { MirrorRig } from '../lib/mirrorhost';
import { writeFileAtomic } from '../lib/atomic';

// THE OPERATOR'S OWN HOSTS, in a real browser against a real muster-server
// (mirror plugin on) configured the way the operator's deployment is — two
// hosts named first and given icons by name:
//
//	MUSTER_HOST_ORDER=tower,laptop   MUSTER_HOST_ICONS=laptop=laptop,tower=server
//
// — and four REAL muster-mirror processes over the tunnel (the `-tags
// e2emirror` fake plays tmux only), plus the seeded dispatched agents on the
// host "cluster". (`tower` stands in for the operator's own server.)
//
// 1. TAB ORDER (internal/ui/agents_filter.go hostTabOrder): All, tower,
//    laptop, every other host A–Z, then cluster LAST. The hosts are registered
//    (their first mirror token minted) in ANOTHER order — zeta, tower, mint,
//    laptop — so an order by registration rank (zeta first) or by name
//    (cluster first, laptop before tower) is red.
// 2. NAMED ICONS (internal/ui/host_icons.go, HostPreferences.Icons): `laptop`
//    draws the laptop glyph though it was registered AFTER tower (by rank
//    alone it drew another), `tower` the server glyph, and no other host draws
//    either. The host tabs lead with the same glyph as the cards.
// 3. THE HOST IS IN A MIRRORED CARD'S BODY (hostChip), no longer in the
//    footer; a dispatched agent's card has no host chip (its cluster icon and
//    the tab say it).
//
// 🔴 REGRESSION TESTS: on the commit before, the tabs read All, cluster,
// e2e-names-mint-…, e2e-names-zeta-…, laptop, tower (the variables did not
// exist); the laptop host drew the icon of its registration rank; and a
// mirrored card's footer said "host <name>" with no host chip in its body.

const rig = new MirrorRig(hostNamesPort, 'e2e-names-zeta', { MUSTER_HOST_ORDER: 'tower,laptop', MUSTER_HOST_ICONS: 'laptop=laptop,tower=server' });
const zeta = rig.host;
const mint = rig.host.replace('e2e-names-zeta-', 'e2e-names-mint-');
const HOSTS = ['tower', 'laptop', mint, zeta];
const t0 = Math.floor(Date.now() / 1000);
// session|id|index|active|command|activity|claude-transcript
const line = (session: string, ago: number) => `${session}|@1|1|1|zsh|${t0 - ago}|`;

test.beforeAll(async () => {
  test.setTimeout(180_000);
  // Registration order: zeta (the rig's own), tower, mint, laptop.
  await rig.start();
  const files: Record<string, string> = {};
  for (const [h, session, ago] of [
    [zeta, 'zsession', 100],
    ['tower', 'wsession', 200],
    [mint, 'msession', 300],
    ['laptop', 'lsession', 400],
  ] as const) {
    files[h] = rig.transcript('', 'windows-' + session);
    writeFileAtomic(files[h], line(session, ago) + '\n');
  }
  await rig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: files[zeta] });
  for (const h of ['tower', mint, 'laptop']) rig.extraHost(h, { MUSTER_MIRROR_E2E_WINDOWS_FILE: files[h] });
});

test.afterAll(async () => {
  await rig.stop();
});

test.use({ storageState: { cookies: [], origins: [] } });

const cardsOn = (page: Page, host: string) => page.locator(`#agents-list > article[data-f-host="${host}"]`);
const tab = (page: Page, host: string) => page.locator(`#agents-hosts [role="tab"][data-host="${host}"]`);
const labels = (page: Page) => page.locator('#agents-hosts [role="tab"] [data-tab-label]').allTextContents();
const cardIcons = (page: Page, host: string) =>
  cardsOn(page, host).locator('[data-card-slot="header"] [data-host-icon]').evaluateAll((els) => els.map((e) => e.getAttribute('data-host-icon')));

async function openList(page: Page) {
  await rig.login(page);
  for (const s of ['zsession', 'wsession', 'msession', 'lsession']) await rig.waitListed(page, s);
  await page.goto(rig.origin + '/agents');
  await expect
    .poll(async () => {
      await takeNew(page);
      return [...(await Promise.all(HOSTS.map((h) => cardsOn(page, h).count()))), (await cardsOn(page, 'cluster').count()) > 0];
    }, { timeout: 30_000, message: `every host's window and the cluster's agents are listed (log:\n${rig.log})` })
    .toEqual([1, 1, 1, 1, true]);
}

for (const vp of [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.name, () => {
    test.use(vp.use);
    const press = (l: Locator) => (vp.use.hasTouch ? l.tap() : l.click());

    test('the host tabs read All, tower, laptop, the other hosts A–Z, then cluster; the choice survives a reload', async ({ page }) => {
      await openList(page);
      await expect.poll(() => labels(page)).toEqual(['All', 'tower', 'laptop', mint, zeta, 'cluster']);
      // Choosing a tab still filters and is remembered — and the order with it.
      await tab(page, 'laptop').scrollIntoViewIfNeeded();
      await press(tab(page, 'laptop'));
      await expect(cardsOn(page, 'laptop').filter({ visible: true })).toHaveCount(1);
      await expect(cardsOn(page, 'tower').filter({ visible: true })).toHaveCount(0);
      await page.reload();
      await expect(tab(page, 'laptop')).toHaveAttribute('aria-selected', 'true', { timeout: 20_000 });
      await expect.poll(() => labels(page)).toEqual(['All', 'tower', 'laptop', mint, zeta, 'cluster']);
      await expect(cardsOn(page, 'tower').filter({ visible: true })).toHaveCount(0);
      await press(tab(page, ''));
      const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
      expect(sw, 'no sideways page scroll').toBeLessThanOrEqual(cw);
    });

    test('laptop draws the laptop glyph and tower the server glyph, on cards and tabs; no other host draws either', async ({ page }) => {
      await openList(page);
      expect(await cardIcons(page, 'laptop'), 'laptop, registered after tower, draws a laptop').toEqual(['laptop']);
      expect(await cardIcons(page, 'tower')).toEqual(['server']);
      for (const h of [mint, zeta]) {
        const [k] = await cardIcons(page, h);
        expect(['laptop', 'server', 'cluster'], `${h} draws ${k}`).not.toContain(k);
      }
      expect(new Set(await cardIcons(page, 'cluster'))).toEqual(new Set(['cluster']));
      // The tab strip leads each host's tab with its card's glyph.
      for (const [h, k] of [['laptop', 'laptop'], ['tower', 'server'], ['cluster', 'cluster']] as const) {
        await expect(tab(page, h).locator('[data-host-icon]')).toHaveAttribute('data-host-icon', k);
      }
    });

    test("every mirrored card spells its host in its body, and its footer does not; a dispatched agent's card has no host chip", async ({ page }) => {
      await openList(page);
      // The cluster's cards: its icon, no chip (the icon and the tab say it).
      await expect(cardsOn(page, 'cluster').locator('[data-card-host]')).toHaveCount(0);
      await expect(cardsOn(page, 'cluster').first().locator('[data-host-icon="cluster"]')).toBeVisible();
      for (const h of HOSTS) {
        const c = cardsOn(page, h).first();
        const chip = c.locator('[data-card-slot="header"] [data-card-host]');
        await expect(chip, h).toHaveAttribute('data-card-host', h);
        await expect(chip, h).toBeVisible();
        await expect(chip, h).toContainText(h);
        const footer = c.locator('[data-card-slot="footer"]');
        if (await footer.count()) {
          // (The footer is matched by word: a host named "laptop" inside
          // another word would be a different claim.)
          expect(await footer.innerText(), `${h}'s footer`).not.toMatch(new RegExp(`\\bhost\\b|${h.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}`));
        }
        const cb = (await c.boundingBox())!;
        const kb = (await chip.boundingBox())!;
        expect(kb.x + kb.width, `${h}: the chip fits its card`).toBeLessThanOrEqual(cb.x + cb.width + 1);
      }
      // Positive control: the mirrored cards' footers exist and say something.
      await expect(cardsOn(page, 'laptop').locator('[data-card-slot="footer"]')).toContainText('active');
    });
  });
}
