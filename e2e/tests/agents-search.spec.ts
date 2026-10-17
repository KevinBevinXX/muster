import fs from 'node:fs';
import { Page } from '@playwright/test';
import { test, expect, takeNew } from '../lib/fixtures';
import { agentsSearchPort, baseURL, password } from '../lib/env';
import { MirrorRig } from '../lib/mirrorhost';
import { clsInit, readCls, resetCls } from '../lib/cls';

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

const rig = new MirrorRig(agentsSearchPort, 'e2e-search', {}, { decimalHost: true });

test.use({ storageState: { cookies: [], origins: [] } });

let windowsFile = '';
const t0 = Math.floor(Date.now() / 1000);
const windowLine = (session: string, id: string, command: string, ago: number) => `${session}|${id}|1|1|${command}|${t0 - ago}|`;

test.beforeAll(async () => {
  test.setTimeout(120_000);
  await rig.start();
  const now = t0;
  windowsFile = rig.transcript('', 'windows');
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

    // 🔴 THE DOM ITSELF IS REORDERED, not only the boxes: Tab and a screen
    // reader walk the cards in ranked order. Typing never takes focus from the
    // box, and clearing it puts the cards back in the list's own order.
    test('typing reorders the cards in the DOM: Tab follows the ranking, focus stays in the box, clearing restores', async ({ page }) => {
      await rig.login(page);
      await rig.waitListed(page, 'mqtail');
      await page.goto(rig.origin + '/agents');
      const domOrder = () =>
        page.locator('#agents-list > article').evaluateAll((els, ns) =>
          els.map((e) => e.getAttribute('data-f-session') || '').filter((n) => (ns as string[]).includes(n)), names);
      await expect.poll(domOrder).toEqual(['mqtail', 'old-qdock', 'qbase']);
      await page.locator('#agents-search').focus();
      await page.keyboard.type('q');
      await expect.poll(domOrder, { message: 'the DOM order is the ranked order' }).toEqual(['qbase', 'old-qdock', 'mqtail']);
      expect(await page.evaluate(() => document.activeElement && document.activeElement.id), 'focus stayed in the box').toBe('agents-search');
      // Tab from the box into the list: the cards' controls come in ranked order.
      const reached: string[] = [];
      for (let i = 0; i < 80 && reached.length < 3; i++) {
        await page.keyboard.press('Tab');
        const s = await page.evaluate(() => {
          const a = document.activeElement && document.activeElement.closest('#agents-list > article');
          return a ? a.getAttribute('data-f-session') || '' : '';
        });
        if (s && names.includes(s) && !reached.includes(s)) reached.push(s);
      }
      expect(reached, 'Tab reaches the cards best match first').toEqual(['qbase', 'old-qdock', 'mqtail']);
      // A refresh morphs the list back into the server's order; it is ranked
      // again before anything is drawn.
      // The order is read at the FIRST frame after the swap, before htmx settles:
      // ranked there means no frame showed the server's order.
      const firstFrame = page.evaluate((ns) => new Promise<string[]>((r) => document.addEventListener('htmx:afterSwap', (e) => {
        if ((e.target as Element).id !== 'agents-list') return;
        const list = e.target as Element;
        requestAnimationFrame(() => r([...list.querySelectorAll(':scope > article')].map((c) => c.getAttribute('data-f-session') || '').filter((n) => ns.includes(n))));
      })), names);
      await page.evaluate(() => (window as any).htmx.trigger(document.body, 'agents:changed'));
      expect(await firstFrame, 'ranked in the first frame after a refresh').toEqual(['qbase', 'old-qdock', 'mqtail']);
      await expect.poll(domOrder, { message: 'still ranked after a refresh' }).toEqual(['qbase', 'old-qdock', 'mqtail']);
      // Cleared: the list's own order again.
      await page.locator('#agents-search').fill('');
      await expect.poll(domOrder, { message: 'clearing restores the order' }).toEqual(['mqtail', 'old-qdock', 'qbase']);
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

// HELD, NOT MOVED (Zach's decision): a background refresh of the open list — an
// activity change, an arrival — moves no card. The new order waits behind an
// "N new ↑" pill, a tap applies it, and a filter's ranking still orders what is
// shown. A card's own content still updates in place. Runs LAST: it rewrites
// the host's windows.
test.describe('held behind the pill', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

  test('an activity change and an arrival wait behind "N new ↑"; nothing moves; content updates in place; a tap applies', async ({ page }) => {
    test.setTimeout(120_000);
    await page.addInitScript(clsInit);
    await rig.login(page);
    await rig.waitListed(page, 'mqtail');
    await page.goto(rig.origin + '/agents');
    const domOrder = () =>
      page.locator('#agents-list > article:not([data-held])').evaluateAll((els, ns) =>
        els.map((e) => e.getAttribute('data-f-session') || '').filter((n) => (ns as string[]).includes(n)), [...names, 'qzest']);
    await expect.poll(domOrder).toEqual(['mqtail', 'old-qdock', 'qbase']);
    const pill = page.locator('#agents-new-pill');
    await expect(pill).toBeHidden();
    await resetCls(page);

    // qbase becomes the most recently active, and its command changes.
    fs.writeFileSync(windowsFile, [windowLine('qbase', '@1', 'vim', 0), windowLine('old-qdock', '@2', 'zsh', 2040), windowLine('mqtail', '@3', 'zsh', 1020)].join('\n') + '\n');
    await expect(pill, 'the change waits behind the pill').toHaveText('1 new ↑', { timeout: 30_000 });
    expect(await domOrder(), 'held: no card moved').toEqual(['mqtail', 'old-qdock', 'qbase']);
    await expect(page.locator('#agents-list > article[data-f-session="qbase"]'), 'its content updated in place').toHaveAttribute('data-f-kind', 'vim');

    // An arrival: kept out of view, counted.
    fs.appendFileSync(windowsFile, windowLine('qzest', '@4', 'zsh', 0) + '\n');
    await expect(pill).toHaveText('2 new ↑', { timeout: 30_000 });
    await expect(page.locator('#agents-list > article[data-f-session="qzest"]')).toBeHidden();
    expect(await domOrder()).toEqual(['mqtail', 'old-qdock', 'qbase']);
    const held = await readCls(page);
    expect(held.total, `nothing moved while held: ${JSON.stringify(held.shifts)}`).toBeLessThan(0.01);

    // With a filter: the ranking orders what is shown, and the pill still waits.
    await page.locator('#agents-search').fill('q');
    await expect.poll(domOrder).toEqual(['qbase', 'old-qdock', 'mqtail']);
    await expect(pill).toHaveText('2 new ↑');
    await page.locator('#agents-search').fill('');
    await expect.poll(domOrder).toEqual(['mqtail', 'old-qdock', 'qbase']);

    // A tap applies the live order, arrival included, and the pill goes.
    await pill.tap();
    await expect(pill).toBeHidden();
    await expect.poll(domOrder, { message: 'the live order, applied' }).toEqual(['qbase', 'qzest', 'mqtail', 'old-qdock']);
  });
});

// The rest of the pill's contract, on a keyboard-shaped desktop. Each test
// writes the host's windows back to a known order first. Runs after the test
// above (it rewrites the windows too).
const sessionsOrder = (page: Page) =>
  page.locator('#agents-list > article:not([data-held])').evaluateAll((els, ns) =>
    els.map((e) => e.getAttribute('data-f-session') || '').filter((n) => (ns as string[]).includes(n)), names);

async function freshList(page: Page, lines: string[], want: string[]) {
  fs.writeFileSync(windowsFile, lines.join('\n') + '\n');
  await rig.login(page);
  await page.goto(rig.origin + '/agents');
  await expect
    .poll(async () => {
      await takeNew(page);
      return sessionsOrder(page);
    }, { timeout: 30_000, message: 'the host reported the written windows' })
    .toEqual(want);
  await expect(page.locator('#agents-list > article[data-f-session="qzest"]')).toHaveCount(0);
}

test.describe('held behind the pill, desktop', () => {
  test.use({ viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false });

  // 🔴 A REGRESSION TEST (round-2 audit): reorder() re-appended EVERY card, so
  // a card holding focus was detached and focus fell to <body>. A refresh that
  // lands while a card is focused — with a filter typed — and the pill's tap
  // that moves that very card must both leave focus where it was.
  test('a focused card keeps focus through a held refresh and through the pill tap that moves it', async ({ page }) => {
    test.setTimeout(120_000);
    // No Element.moveBefore (Firefox and Safari today): the hand-back must
    // not depend on a browser that keeps focus through a move by itself.
    await page.addInitScript(() => { delete (Element.prototype as unknown as { moveBefore?: unknown }).moveBefore; });
    await freshList(page, [windowLine('qbase', '@1', 'zsh', 3000), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000)], ['mqtail', 'old-qdock', 'qbase']);
    expect(await page.evaluate(() => typeof (Element.prototype as unknown as { moveBefore?: unknown }).moveBefore), 'moveBefore is absent').toBe('undefined');
    const box = page.locator('#agents-search');
    const pill = page.locator('#agents-new-pill');

    // Tab from the box into a card; mark the element and count its blurs.
    const tabInto = async (session: string) => {
      await box.focus();
      for (let i = 0; i < 60; i++) {
        await page.keyboard.press('Tab');
        if (await page.evaluate((s) => !!document.activeElement?.closest(`#agents-list > article[data-f-session="${s}"]`), session)) break;
      }
      const ok = await page.evaluate(() => {
        const w = window as unknown as { __f?: Element; __blurs: number };
        w.__f = document.activeElement ?? undefined;
        w.__blurs = 0;
        w.__f?.addEventListener('blur', () => w.__blurs++);
        return !!w.__f && w.__f !== document.body;
      });
      expect(ok, `Tab reached the ${session} card`).toBe(true);
    };
    const stillFocused = () =>
      page.evaluate(() => {
        const w = window as unknown as { __f?: Element; __blurs: number };
        return { same: document.activeElement === w.__f, connected: !!w.__f?.isConnected, blurs: w.__blurs, now: document.activeElement?.outerHTML.slice(0, 80) };
      });

    // 1. A filter typed, focus in the last-ranked card, and a refresh that
    //    re-orders the list (qbase becomes the newest, its command changes).
    await box.fill('q');
    await expect.poll(() => sessionsOrder(page)).toEqual(['qbase', 'old-qdock', 'mqtail']);
    await tabInto('mqtail');
    fs.writeFileSync(windowsFile, [windowLine('qbase', '@1', 'vim', 0), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000)].join('\n') + '\n');
    await expect(page.locator('#agents-list > article[data-f-session="qbase"]'), 'the refresh landed').toHaveAttribute('data-f-kind', 'vim', { timeout: 30_000 });
    await expect(pill).toBeVisible();
    expect(await stillFocused(), 'focus survived the refresh').toMatchObject({ same: true, connected: true });
    // The tap applies the live order and does not take focus.
    await pill.click();
    await expect(pill).toBeHidden();
    expect(await stillFocused(), 'focus survived the pill tap').toMatchObject({ same: true, connected: true });

    // 2. No filter, focus in the LAST card, and a refresh that makes it the
    //    newest: the pill's tap moves the focused card itself to the top.
    await box.fill('');
    await expect.poll(() => sessionsOrder(page)).toEqual(['qbase', 'mqtail', 'old-qdock']);
    await tabInto('old-qdock');
    fs.writeFileSync(windowsFile, [windowLine('qbase', '@1', 'vim', 600), windowLine('old-qdock', '@2', 'zsh', 0), windowLine('mqtail', '@3', 'zsh', 1000)].join('\n') + '\n');
    await expect(pill, 'the re-order waits').toBeVisible({ timeout: 30_000 });
    expect(await stillFocused(), 'focus survived the second refresh').toMatchObject({ same: true, connected: true });
    await pill.click();
    await expect.poll(() => sessionsOrder(page), { message: 'the tap moved the focused card' }).toEqual(['old-qdock', 'qbase', 'mqtail']);
    const after = await stillFocused();
    expect(after, 'focus is on the moved card').toMatchObject({ same: true, connected: true });
    // ⚠ The move detaches the card, so it blurs and is refocused: a transient
    // blur the hand-back repairs. Counted, not hidden.
    expect(after.blurs, 'the moved card was detached and refocused').toBeGreaterThanOrEqual(1);
  });
});

// A change that moves nothing the reader can see — the whole list below the
// screen — is applied at once: no pill, no shift.
test.describe('held behind the pill: off screen', () => {
  test.use({ viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false });

  test('a re-order wholly below the screen is applied at once, with no pill and no shift', async ({ page }) => {
    test.setTimeout(120_000);
    await page.addInitScript(clsInit);
    await freshList(page, [windowLine('qbase', '@1', 'zsh', 3000), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000)], ['mqtail', 'old-qdock', 'qbase']);
    // Push the whole list below the fold (a test-only spacer above it).
    await page.evaluate(() => {
      const list = document.getElementById('agents-list')!;
      const d = document.createElement('div');
      d.id = 'test-spacer';
      d.style.height = '3000px';
      list.parentElement!.insertBefore(d, list);
      window.scrollTo(0, 0);
    });
    expect(await page.evaluate(() => document.getElementById('agents-list')!.getBoundingClientRect().top), 'the list is below the screen').toBeGreaterThan(800);
    await resetCls(page);
    fs.writeFileSync(windowsFile, [windowLine('qbase', '@1', 'vim', 0), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000)].join('\n') + '\n');
    await expect(page.locator('#agents-list > article[data-f-session="qbase"]'), 'the refresh landed').toHaveAttribute('data-f-kind', 'vim', { timeout: 30_000 });
    await expect.poll(() => sessionsOrder(page), { message: 'applied at once' }).toEqual(['qbase', 'mqtail', 'old-qdock']);
    await expect(page.locator('#agents-new-pill')).toBeHidden();
    const c = await readCls(page);
    expect(c.total, `nothing in view moved: ${JSON.stringify(c.shifts)}`).toBeLessThan(0.01);
  });
});
