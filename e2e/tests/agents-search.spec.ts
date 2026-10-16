import fs from 'node:fs';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { agentsSearchPort, baseURL, password } from '../lib/env';
import { MirrorRig } from '../lib/mirrorhost';

// THE AGENTS FILTER RANKS WHILE TYPING (internal/ui/agents_filter.go rankAttrs):
// a one-letter query already filters, and the cards it shows are REORDERED —
// a title starting with it, then a title with a word starting with it, then any
// other match — keeping the list's own order within a rank. Clearing the box
// restores the list's order. `field:value` tokens, several words (all must
// match) and pins keep working.
//
// The cards are three mirrored sessions of this spec's own host, named so each
// is in exactly one rank for "q" — and none of the seeded agents (or the host's
// name, which is decimal digits) contains a q:
//
//   qbase      a title that STARTS with q       rank 0
//   old-qdock  a WORD in the title starts with q  rank 1
//   mqtail     q inside a word                   rank 2
//
// They are listed in the OPPOSITE order (the host reports mqtail most recently
// active), so a rank can only come out right by being applied.

const rig = new MirrorRig(agentsSearchPort, 'e2e-search');

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(120_000);
  await rig.start();
  const now = Math.floor(Date.now() / 1000);
  const windowsFile = rig.transcript('', 'windows');
  // session|id|index|active|command|activity|claude-transcript — newest first by activity.
  fs.writeFileSync(
    windowsFile,
    [`qbase|@1|1|1|zsh|${now - 3000}|`, `old-qdock|@2|1|1|zsh|${now - 2000}|`, `mqtail|@3|1|1|zsh|${now - 1000}|`].join('\n') + '\n',
  );
  await rig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: windowsFile });
});

test.afterAll(async () => {
  await rig.stop();
});

const names = ['qbase', 'old-qdock', 'mqtail'];

// shown is the visible cards of this host, in the order they are DRAWN (by
// box: top to bottom, then left to right), as their session names.
async function shown(page: Page): Promise<string[]> {
  return page.locator('#agents-list > article').evaluateAll((els, ns) => {
    const vis = els
      .map((e) => ({ e, r: e.getBoundingClientRect() }))
      .filter(({ r }) => r.width > 0 && r.height > 0)
      .sort((a, b) => a.r.top - b.r.top || a.r.left - b.r.left);
    return vis.flatMap(({ e }) => {
      const t = (e.getAttribute('data-f-session') || '').trim();
      return (ns as string[]).includes(t) ? [t] : [];
    });
  }, names);
}

async function type(page: Page, q: string) {
  await page.locator('#agents-search').fill(q);
}

for (const vp of [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.name, () => {
    test.use(vp.use);

    test('one letter filters and ranks prefix, word start, substring; clearing restores the order', async ({ page }) => {
      await rig.login(page);
      await rig.waitListed(page, 'mqtail');
      await page.goto(rig.origin + '/agents');
      await expect.poll(() => shown(page), { message: 'the three sessions, most recently active first' }).toEqual(['mqtail', 'old-qdock', 'qbase']);

      await type(page, 'q');
      await expect.poll(() => shown(page), { message: 'q: prefix, then word start, then substring' }).toEqual(['qbase', 'old-qdock', 'mqtail']);
      // Only the q cards are shown — the seeded agents carry no q.
      const cards = await page.locator('#agents-list > article').count();
      await expect(page.locator('#agents-count')).toHaveText(`3 of ${cards}`);

      // Upper case is the same query; a longer one narrows, still ranked.
      await type(page, 'Q');
      await expect.poll(() => shown(page)).toEqual(['qbase', 'old-qdock', 'mqtail']);
      await type(page, 'qd');
      await expect.poll(() => shown(page)).toEqual(['old-qdock']);

      // Several words: ALL must match; the FIRST ranks.
      await type(page, 'q tail');
      await expect.poll(() => shown(page)).toEqual(['mqtail']);
      // A field token filters and leaves the rank to the free word.
      await type(page, `host:${rig.host} q`);
      await expect.poll(() => shown(page)).toEqual(['qbase', 'old-qdock', 'mqtail']);
      await type(page, `session=mqtail q`);
      await expect.poll(() => shown(page)).toEqual(['mqtail']);

      // Cleared: the list's own order, everything shown.
      await page.locator('#agents-search-clear').click();
      await expect.poll(() => shown(page)).toEqual(['mqtail', 'old-qdock', 'qbase']);
      await expect(page.locator('#agents-count')).toHaveText(`${cards} of ${cards}`);
    });

    test('pins keep their place within a rank, and never lift a card above a better match', async ({ page }) => {
      await rig.login(page);
      await rig.waitListed(page, 'mqtail');
      await page.goto(rig.origin + '/agents');
      const card = (s: string) => page.locator(`#agents-list > article[data-f-session="${s}"]`);
      const pin = async (s: string, on: boolean) => {
        const t = card(s).locator('[data-pin-toggle]');
        if ((await t.getAttribute('aria-pressed')) !== String(on)) await t.click();
        await expect(card(s).locator('[data-pin-toggle]')).toHaveAttribute('aria-pressed', String(on));
      };
      try {
        // "a" is on every one of the three (the host's name is e2e-search-…), in
        // no title as a start: ONE rank, so the list's own order decides.
        await type(page, 'a');
        await expect.poll(() => shown(page), { message: 'a tie keeps the list order' }).toEqual(['mqtail', 'old-qdock', 'qbase']);
        // Pin the substring match and the oldest: they lead the list…
        await pin('mqtail', true);
        await pin('qbase', true);
        await page.reload();
        await type(page, '');
        await expect.poll(() => shown(page), { message: 'pinned first' }).toEqual(['mqtail', 'qbase', 'old-qdock']);
        // …and lead a tie, in their pinned order…
        await type(page, 'a');
        await expect.poll(() => shown(page), { message: 'a tie keeps the pinned order' }).toEqual(['mqtail', 'qbase', 'old-qdock']);
        // …but a pin never beats a better match.
        await type(page, 'q');
        await expect.poll(() => shown(page), { message: 'the pinned substring match stays last' }).toEqual(['qbase', 'old-qdock', 'mqtail']);
      } finally {
        await type(page, '');
        await pin('mqtail', false);
        await pin('qbase', false);
      }
    });
  });
}

// The seeded agents on the shared server: one letter is a filter too, and the
// rank works on agent cards (display name / name), not only mirrored ones.
test('agent cards: one letter ranks a name prefix above a word start', async ({ page }) => {
  await page.goto(baseURL + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
  await page.goto(baseURL + '/agents');
  const order = () =>
    page.locator('#agents-list > article').evaluateAll((els) =>
      els
        .map((e) => ({ n: e.getAttribute('data-f-name') || '', r: e.getBoundingClientRect() }))
        .filter(({ r }) => r.height > 0)
        .sort((a, b) => a.r.top - b.r.top || a.r.left - b.r.left)
        .map(({ n }) => n)
        .filter((n) => ['docs-sweeper', 'release-helper', 'notes-tidy'].includes(n)),
    );
  await expect.poll(order).toHaveLength(3);
  // "s": no name starts with s; "sweeper" is a word start (rank 1); "notes-tidy"
  // and "release-helper" carry an s inside a word (rank 2), in list order.
  const before = await order();
  await page.locator('#agents-search').fill('s');
  await expect.poll(order).toEqual(['docs-sweeper', ...before.filter((n) => n !== 'docs-sweeper')]);
  // "r": release-helper STARTS with r (rank 0); docs-sweeper only contains one.
  await page.locator('#agents-search').fill('r');
  await expect.poll(async () => (await order())[0]).toBe('release-helper');
});
