import fs from 'node:fs';
import path from 'node:path';
import { BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { password, pinsPort } from '../lib/env';
import { MirrorRig } from '../lib/mirrorhost';

// PINNED ENTRIES COME FIRST, in a real browser, against a real muster-server
// (MUSTER_MIRROR=1, its own port) and a real muster-mirror over the tunnel
// (the `-tags e2emirror` per-window fake, as mirror-windows.spec.ts).
//
// The operator ask: "Add 'pinned' agents so they appear first in list." The
// entries pinned here are the LEAST recently active of each kind — the last
// seeded agent card and the older of two windows — so "it moved to the top"
// cannot be the activity order doing it. Pins are server-stored, so they must
// survive a list refresh AND a page reload.
//
// 🔴 THE AGENT PIN IS ON A SEEDED AGENT, WHICH EVERY SERVER IN THE SUITE LISTS
// (one database, one operator). The test unpins it again, and afterAll unpins
// it once more through the route, so no later spec sees it pinned.

// The server, its mirror token and the fake host are e2e/lib/mirrorhost.ts's.
const rig = new MirrorRig(pinsPort, 'e2e-pin');
const origin = rig.origin;
const host = rig.host;
const windowsFile = path.join(rig.dir, 'windows');
let pinnedAgentID = '';

const now = Math.floor(Date.now() / 1000);
// session|id|index|active|command|activity|claude-transcript
const windows = [`pinme|@1|1|1|zsh|${now - 60}|`, `pinme|@2|2|0|zsh|${now - 7200}|`];
const newerHref = `/mirror/terminal?host=${host}&session=pinme&window=%401`;
const olderHref = `/mirror/terminal?host=${host}&session=pinme&window=%402`;

test.beforeAll(async () => {
  await rig.start();
  fs.writeFileSync(windowsFile, windows.join('\n') + '\n');
  await rig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: windowsFile });
});

let signedIn: Awaited<ReturnType<BrowserContext['cookies']>> | undefined;

test.afterAll(async () => {
  // Belt and braces: the seeded agent must not stay pinned for later specs.
  if (pinnedAgentID && signedIn) {
    const cookie = signedIn.map((c) => `${c.name}=${c.value}`).join('; ');
    await fetch(origin + '/agents/pins/delete', {
      method: 'POST',
      headers: { Cookie: cookie, Origin: origin, 'Content-Type': 'application/x-www-form-urlencoded', 'HX-Request': 'true' },
      body: `kind=agent&agent_id=${pinnedAgentID}`,
    }).catch(() => undefined);
  }
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

// order is every card of the list, top to bottom: "agent-<id>" for an agent,
// "win@<n>" for one of THIS host's windows; other hosts' cards are skipped.
async function order(page: Page): Promise<string[]> {
  return page.locator('#agents-list > article').evaluateAll((els, host) =>
    els.flatMap((e) => {
      if (e.id.startsWith('agent-')) return [e.id];
      const a = e.querySelector('a[href*="window="]') as HTMLAnchorElement | null;
      if (!a || !a.href.includes('host=' + host)) return [];
      return ['win@' + new URL(a.href).searchParams.get('window')!.slice(1)];
    }), host);
}

async function openList(page: Page) {
  await login(page);
  await page.goto(origin + '/agents');
  await expect(page.locator(`#agents-list article[data-mirror-card] a[href="${olderHref}"]`), `the host's windows (log:\n${rig.log})`).toHaveCount(1, {
    timeout: 20_000,
  });
  await expect(page.locator(`#agents-list article[data-mirror-card] a[href="${newerHref}"]`)).toHaveCount(1);
}

for (const vp of [
  { name: 'phone 390×844 touch', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.name, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });

    test('a pinned agent and a pinned window move to the top and stay there across a refresh and a reload', async ({ page }) => {
      await openList(page);
      const before = await order(page);
      const agentsBefore = before.filter((x) => x.startsWith('agent-'));
      expect(agentsBefore.length, 'the seeded agents are listed').toBeGreaterThanOrEqual(2);
      expect(before.filter((x) => x.startsWith('win@')), 'the newer window first, unpinned').toEqual(['win@1', 'win@2']);
      expect(await page.locator('#agents-list [data-pinned]').count(), 'nothing pinned at the start').toBe(0);

      // The LEAST recently active agent: the last agent card.
      const last = agentsBefore[agentsBefore.length - 1];
      pinnedAgentID = last.replace('agent-', '');
      const card = page.locator(`#${last}`);
      await card.locator('[aria-label="Agent actions"]').click();
      await page.locator(`#agent-menu-${pinnedAgentID}`).getByRole('button', { name: 'Pin to top' }).click();
      await expect.poll(() => order(page).then((o) => o[0]), { message: 'the pinned agent is the first card' }).toBe(last);
      await expect(page.locator(`#${last} [data-pinned-glyph]`)).toBeVisible();

      // The OLDER window's pin toggle.
      const older = page.locator(`#agents-list article[data-mirror-card]:has(a[href="${olderHref}"])`);
      const toggle = older.locator('[data-pin-toggle]');
      await expect(toggle).toHaveAttribute('aria-pressed', 'false');
      const box = (await toggle.boundingBox())!;
      expect(box.width, 'the toggle is a 44px target').toBeGreaterThanOrEqual(44);
      await toggle.click();
      const want = [last, 'win@2', ...agentsBefore.filter((x) => x !== last), 'win@1'];
      await expect.poll(() => order(page), { message: 'pinned agent, pinned window, then the rest in their order' }).toEqual(want);
      await expect(older.locator('[data-pin-toggle]')).toHaveAttribute('aria-pressed', 'true');

      // A list refresh (what an SSE event or a lifecycle action does) keeps it.
      await page.evaluate(() => (window as any).htmx.trigger(document.body, 'agents:changed'));
      await page.waitForTimeout(500);
      expect(await order(page), 'after a list refresh').toEqual(want);
      // And a reload: the pins are the server's, not the page's.
      await page.reload();
      await expect.poll(() => order(page), { message: 'after a reload', timeout: 20_000 }).toEqual(want);

      // Pinned-but-filtered-out stays hidden: filter by this host's name, which
      // the windows' cards carry and the pinned agent's card does not.
      await page.fill('#agents-search', host);
      await expect(page.locator(`#${last}`)).toBeHidden();
      await expect(older).toBeVisible();
      await page.fill('#agents-search', '');
      await expect(page.locator(`#${last}`)).toBeVisible();

      // Unpin both; the list returns to its activity order.
      await page.locator(`#${last} [aria-label="Agent actions"]`).click();
      await page.locator(`#agent-menu-${pinnedAgentID}`).getByRole('button', { name: 'Unpin' }).click();
      await page.locator(`#agents-list article[data-mirror-card]:has(a[href="${olderHref}"]) [data-pin-toggle]`).click();
      await expect.poll(() => order(page), { message: 'after unpinning both' }).toEqual(before);
      expect(await page.locator('#agents-list [data-pinned]').count()).toBe(0);
    });
  });
}
