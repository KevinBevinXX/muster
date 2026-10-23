import fs from 'node:fs';
import { Page } from '@playwright/test';
import { test, expect, takeNew } from '../lib/fixtures';
import { agentsSearchPort, baseURL, password } from '../lib/env';
import { MirrorRig, mkchat } from '../lib/mirrorhost';
import { clsInit, readCls, resetCls } from '../lib/cls';
import { writeFileAtomic } from '../lib/atomic';

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
  writeFileAtomic(
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

    // PINNED ENTRIES ARE CHIPS (internal/ui/pinned_chips.go), drawn in a row
    // above every card: the rank orders the chips among themselves and the
    // cards among themselves, and never moves a card above a chip — in the
    // boxes AND in the DOM (Tab meets the chips first, where the eye does).
    test('pinned chips rank among themselves, ahead of every card, in the boxes and the DOM', async ({ page }) => {
      await rig.login(page);
      await rig.waitListed(page, 'mqtail');
      await page.goto(rig.origin + '/agents');
      const entry = (s: string) => page.locator(`#agents-list > article[data-f-session="${s}"]`);
      const domOrder = () =>
        page.locator('#agents-list > article').evaluateAll((els, ns) =>
          els.map((e) => e.getAttribute('data-f-session') || '').filter((n) => (ns as string[]).includes(n)), names);
      const pin = async (s: string, on: boolean) => {
        const r = await page.request.post(rig.origin + (on ? '/agents/pins' : '/agents/pins/delete'), {
          form: { kind: 'mirror', host: rig.host, session: s, window: '@' + (names.indexOf(s) + 1) },
          headers: { Origin: rig.origin },
        });
        expect(r.status(), `${on ? 'pin' : 'unpin'} ${s}`).toBe(200);
      };
      try {
        // "a" is on every one of the three (the host's name is e2e-search-…), in
        // no title as a start: ONE rank, so the list's own order decides.
        await type(page, 'a');
        await expect.poll(() => shown(page), { message: 'a tie keeps the list order' }).toEqual(['mqtail', 'old-qdock', 'qbase']);
        // Pin the substring match and the oldest: they become the chips…
        await pin('mqtail', true);
        await pin('qbase', true);
        await page.reload();
        await type(page, '');
        await expect(entry('mqtail')).toHaveAttribute('data-pinned-chip', 'mirror');
        await expect(entry('qbase')).toHaveAttribute('data-pinned-chip', 'mirror');
        await expect.poll(() => shown(page), { message: 'the chips first' }).toEqual(['mqtail', 'qbase', 'old-qdock']);
        // …a tie keeps their pinned order…
        await type(page, 'a');
        await expect.poll(() => shown(page), { message: 'a tie keeps the pinned order' }).toEqual(['mqtail', 'qbase', 'old-qdock']);
        // …and "q" ranks the chips among themselves (qbase a prefix, mqtail a
        // substring) while the card, a better match than mqtail, stays a card
        // below them.
        await type(page, 'q');
        await expect.poll(() => shown(page), { message: 'chips ranked, then the cards' }).toEqual(['qbase', 'mqtail', 'old-qdock']);
        await expect.poll(domOrder, { message: 'the DOM follows the boxes' }).toEqual(['qbase', 'mqtail', 'old-qdock']);
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

// HELD, NOT MOVED (Zach's call; tap-only, even at the top, confirmed): a
// background refresh of the open list — an activity change, an arrival — moves no card. The new order waits behind an
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
    writeFileAtomic(windowsFile, [windowLine('qbase', '@1', 'vim', 0), windowLine('old-qdock', '@2', 'zsh', 2040), windowLine('mqtail', '@3', 'zsh', 1020)].join('\n') + '\n');
    await expect(pill, 'the change waits behind the pill').toHaveText('1 new ↑', { timeout: 30_000 });
    expect(await domOrder(), 'held: no card moved').toEqual(['mqtail', 'old-qdock', 'qbase']);
    await expect(page.locator('#agents-list > article[data-f-session="qbase"]'), 'its content updated in place').toHaveAttribute('data-f-kind', 'vim');

    // An arrival: kept out of view, counted by the pill — and NOT counted by
    // its host's tab while it is held: a tab counts what is drawn.
    const hostCount = page.locator(`#agents-hosts [role="tab"][data-host="${rig.host}"] [data-tab-count]`);
    await expect(hostCount, 'positive control: the host tab counts its drawn cards').toHaveText('3');
    writeFileAtomic(windowsFile, fs.readFileSync(windowsFile, 'utf8') + windowLine('qzest', '@4', 'qtool', 0) + '\n');
    await expect(pill).toHaveText('2 new ↑', { timeout: 30_000 });
    await expect(page.locator('#agents-list > article[data-f-session="qzest"]')).toBeHidden();
    await expect(hostCount, 'a held arrival is not counted by its host tab').toHaveText('3');
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
    await expect(hostCount, 'drawn, it is counted').toHaveText('4');
  });
});

// The rest of the pill's contract, on a keyboard-shaped desktop. Each test
// writes the host's windows back to a known order first. Runs after the test
// above (it rewrites the windows too).
const sessionsOrder = (page: Page) =>
  page.locator('#agents-list > article:not([data-held])').evaluateAll((els, ns) =>
    els.map((e) => e.getAttribute('data-f-session') || '').filter((n) => (ns as string[]).includes(n)), names);

async function freshList(page: Page, lines: string[], want: string[]) {
  writeFileAtomic(windowsFile, lines.join('\n') + '\n');
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

for (const leg of [
  { name: 'moveBefore', strip: false },
  { name: 'no moveBefore', strip: true },
]) {
  test.describe(`held behind the pill, desktop (${leg.name})`, () => {
    test.use({ viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false });

    // 🔴 A REGRESSION TEST (round-2 audit): reorder() re-appended EVERY card, so
    // a card holding focus was detached and focus fell to <body>. A refresh that
    // lands while a card is focused — with a filter typed — and the pill's tap
    // that moves that very card must both leave focus where it was.
    test('a focused card keeps focus through a held refresh and through the pill tap that moves it', async ({ page }) => {
      test.setTimeout(120_000);
      // Without Element.moveBefore (Firefox and Safari today) the hand-back must
      // not depend on a browser that keeps focus through a move by itself.
      if (leg.strip) await page.addInitScript(() => { delete (Element.prototype as unknown as { moveBefore?: unknown }).moveBefore; });
      await freshList(page, [windowLine('qbase', '@1', 'zsh', 3000), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000)], ['mqtail', 'old-qdock', 'qbase']);
      expect(await page.evaluate(() => typeof (Element.prototype as unknown as { moveBefore?: unknown }).moveBefore), 'the leg runs the mover it names').toBe(leg.strip ? 'undefined' : 'function');
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
      writeFileAtomic(windowsFile, [windowLine('qbase', '@1', 'vim', 0), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000)].join('\n') + '\n');
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
      writeFileAtomic(windowsFile, [windowLine('qbase', '@1', 'vim', 600), windowLine('old-qdock', '@2', 'zsh', 0), windowLine('mqtail', '@3', 'zsh', 1000)].join('\n') + '\n');
      await expect(pill, 'the re-order waits').toBeVisible({ timeout: 30_000 });
      expect(await stillFocused(), 'focus survived the second refresh').toMatchObject({ same: true, connected: true });
      await pill.click();
      await expect.poll(() => sessionsOrder(page), { message: 'the tap moved the focused card' }).toEqual(['old-qdock', 'qbase', 'mqtail']);
      const after = await stillFocused();
      expect(after, 'focus is on the moved card').toMatchObject({ same: true, connected: true });
      // moveBefore moves the card without detaching it: no blur. Without it the
      // move detaches the card, so it blurs and is refocused — a transient blur
      // the hand-back repairs. Counted, not hidden.
      if (leg.strip) expect(after.blurs, 'the moved card was detached and refocused').toBeGreaterThanOrEqual(1);
      else expect(after.blurs, 'moveBefore moved it without a blur').toBe(0);
    });
  });
}

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
    writeFileAtomic(windowsFile, [windowLine('qbase', '@1', 'vim', 0), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000)].join('\n') + '\n');
    await expect(page.locator('#agents-list > article[data-f-session="qbase"]'), 'the refresh landed').toHaveAttribute('data-f-kind', 'vim', { timeout: 30_000 });
    await expect.poll(() => sessionsOrder(page), { message: 'applied at once' }).toEqual(['qbase', 'mqtail', 'old-qdock']);
    await expect(page.locator('#agents-new-pill')).toBeHidden();
    const c = await readCls(page);
    expect(c.total, `nothing in view moved: ${JSON.stringify(c.shifts)}`).toBeLessThan(0.01);
  });
});

// 🔴 A REGRESSION TEST (round-1 audit 🔴): the pill was built once and kept in
// a variable. A boosted navigation swaps the body and Back restores a snapshot
// of it, so on the SECOND boosted entry to the Agents tab the variable named a
// detached node: an arrival was held hidden with no pill in the document to tap.
test.describe('held behind the pill: across navigations', () => {
  test.use({ viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false });

  test('after detail → Agents → Back → Agents, a held arrival still has its pill, and the tap applies it', async ({ page }) => {
    test.setTimeout(120_000);
    await freshList(page, [windowLine('qbase', '@1', 'zsh', 3000), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000)], ['mqtail', 'old-qdock', 'qbase']);
    const agentsLink = page.locator('.sidebar-tab[data-tab="agents"]');
    // A detail page, by a full load: no Agents panel, so its Agents link is a
    // boosted navigation that swaps the body.
    await page.goto(rig.origin + '/tasks/1');
    await expect(agentsLink).toBeVisible();
    const pill = page.locator('#agents-new-pill');
    // First entry: a held change, so the pill is MADE on this body, and tapped.
    await agentsLink.click();
    await expect(page, 'first entry').toHaveURL(/\/agents$/);
    await expect.poll(() => sessionsOrder(page), { message: 'first entry' }).toEqual(['mqtail', 'old-qdock', 'qbase']);
    writeFileAtomic(windowsFile, [windowLine('qbase', '@1', 'vim', 0), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000)].join('\n') + '\n');
    await expect(pill, 'the first entry made its pill').toHaveText('1 new ↑', { timeout: 30_000 });
    await pill.click();
    await expect.poll(() => sessionsOrder(page)).toEqual(['qbase', 'mqtail', 'old-qdock']);
    // Back to the detail page (a history restore), and Agents again (boosted).
    await page.goBack();
    await expect(page).toHaveURL(/\/tasks\/1$/);
    await expect(agentsLink).toBeVisible();
    await agentsLink.click();
    await expect(page, 'second entry').toHaveURL(/\/agents$/);
    await expect.poll(() => sessionsOrder(page), { message: 'second entry' }).toEqual(['qbase', 'mqtail', 'old-qdock']);
    // An arrival and a re-order, from the host: held, with ONE pill to tap.
    writeFileAtomic(windowsFile, [windowLine('qbase', '@1', 'vim', 600), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 0), windowLine('qzest', '@4', 'zsh', 5)].join('\n') + '\n');
    const zest = page.locator('#agents-list > article[data-f-session="qzest"]');
    await expect(zest, 'the arrival reached the list').toHaveCount(1, { timeout: 30_000 });
    await expect(zest, 'held').toBeHidden();
    await expect(pill, 'one pill in the document').toHaveCount(1);
    await expect(pill, 'the held arrival has its pill').toHaveText('2 new ↑');
    await pill.click();
    await expect(zest, 'the tap drew it').toBeVisible();
    await expect(pill).toBeHidden();
  });
});

// 🔴 A REGRESSION TEST (round-1 audit 🟡): the pill is fixed on <body>, and
// nothing hid it when the Agents tab was: after switching to Tasks it floated
// over the board until the next refresh. It must go with the tab, and come
// back with its count when the tab does — by the sidebar and by Back.
test.describe('held behind the pill: other tabs', () => {
  test.use({ viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false });

  test('the pill hides on another tab and comes back, count intact, with the Agents tab', async ({ page }) => {
    test.setTimeout(120_000);
    await freshList(page, [windowLine('qbase', '@1', 'zsh', 3000), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000)], ['mqtail', 'old-qdock', 'qbase']);
    const pill = page.locator('#agents-new-pill');
    writeFileAtomic(windowsFile, [windowLine('qbase', '@1', 'zsh', 3000), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000), windowLine('qzest', '@4', 'zsh', 5)].join('\n') + '\n');
    await expect(pill, 'the arrival waits').toHaveText('1 new ↑', { timeout: 30_000 });
    await expect(pill).toBeVisible();

    await page.locator('.sidebar-tab[data-tab="tasks"]').click();
    await expect(page).toHaveURL(/\/tasks$/);
    await expect(page.locator('#panel-tasks')).toBeVisible();
    await expect(pill, 'not over the Tasks board').toBeHidden();

    await page.locator('.sidebar-tab[data-tab="agents"]').click();
    await expect(page).toHaveURL(/\/agents$/);
    await expect(pill, 'back with the tab').toBeVisible();
    await expect(pill, 'its count intact').toHaveText('1 new ↑');

    // Back (to Tasks) and Forward (to Agents) do the same.
    await page.goBack();
    await expect(page).toHaveURL(/\/tasks$/);
    await expect(pill, 'not over the Tasks board after Back').toBeHidden();
    await page.goForward();
    await expect(page).toHaveURL(/\/agents$/);
    await expect(pill, 'back after Forward').toHaveText('1 new ↑');
    await expect(pill).toBeVisible();
  });
});

// 🔴 A REGRESSION TEST (round-1 audit 🟡): while the pill was up, EVERY
// background refresh moved every card — idiomorph put them in the live order
// and the hold put them back — so a refresh that changed only a card's
// content still closed an open popover and blurred (then refocused) the
// focused element. Measured by the audit: 6 blurs in 3 refreshes, and a badge
// popover that survives at the base closed at the head. A content-only refresh
// while held must move nothing: the popover stays open, the focused element
// gets no blur — with Element.moveBefore and without it (Firefox/Safari today).
for (const leg of [
  { name: 'moveBefore', strip: false },
  { name: 'no moveBefore', strip: true },
]) {
  test.describe(`held behind the pill: a content-only refresh moves nothing (${leg.name})`, () => {
    test.use({ viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false });

    test('an open popover stays open', async ({ page }) => {
      test.setTimeout(120_000);
      if (leg.strip) await page.addInitScript(() => { delete (Element.prototype as unknown as { moveBefore?: unknown }).moveBefore; });
      await freshList(page, [windowLine('qbase', '@1', 'zsh', 3000), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000)], ['mqtail', 'old-qdock', 'qbase']);
      const pill = page.locator('#agents-new-pill');
      const card = (s: string) => page.locator(`#agents-list > article[data-f-session="${s}"]`);
      const write = (qbaseCmd: string, mqtailCmd: string) =>
        writeFileAtomic(windowsFile, [windowLine('qbase', '@1', qbaseCmd, 0), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', mqtailCmd, 1000)].join('\n') + '\n');

      // Held: qbase becomes the newest, and waits behind the pill.
      write('zsh', 'zsh');
      await expect(pill).toHaveText('1 new ↑', { timeout: 30_000 });
      expect(await sessionsOrder(page)).toEqual(['mqtail', 'old-qdock', 'qbase']);

      // A popover open on a held card (mqtail's "Mirrored" mark), and a
      //    refresh that changes only a card's content.
      await card('mqtail').locator('[popovertarget]').first().click();
      const open = () => card('mqtail').locator(':popover-open').count();
      expect(await open(), 'control: the popover opened').toBe(1);
      write('zsh', 'vim');
      await expect(card('mqtail'), 'the content-only refresh landed').toHaveAttribute('data-f-kind', 'vim', { timeout: 30_000 });
      expect(await open(), 'the popover survived the refresh').toBe(1);
    });

    test('the focused element is never blurred', async ({ page }) => {
      test.setTimeout(120_000);
      if (leg.strip) await page.addInitScript(() => { delete (Element.prototype as unknown as { moveBefore?: unknown }).moveBefore; });
      await freshList(page, [windowLine('qbase', '@1', 'zsh', 3000), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000)], ['mqtail', 'old-qdock', 'qbase']);
      const pill = page.locator('#agents-new-pill');
      const card = (s: string) => page.locator(`#agents-list > article[data-f-session="${s}"]`);
      const write = (qbaseCmd: string, mqtailCmd: string) =>
        writeFileAtomic(windowsFile, [windowLine('qbase', '@1', qbaseCmd, 0), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', mqtailCmd, 1000)].join('\n') + '\n');
      write('zsh', 'vim');
      await expect(pill).toHaveText('1 new ↑', { timeout: 30_000 });
      await expect(card('mqtail')).toHaveAttribute('data-f-kind', 'vim', { timeout: 30_000 });

      // Focus on an element of a held card, and a content-only refresh.
      await card('old-qdock').locator('a[href]').first().focus();
      await page.evaluate(() => {
        const w = window as unknown as { __f?: Element; __blurs: number };
        w.__f = document.activeElement ?? undefined;
        w.__blurs = 0;
        w.__f?.addEventListener('blur', () => w.__blurs++);
      });
      write('vim', 'zsh');
      await expect(card('mqtail'), 'the second refresh landed').toHaveAttribute('data-f-kind', 'shell', { timeout: 30_000 });
      await expect(card('qbase')).toHaveAttribute('data-f-kind', 'vim');
      const f = await page.evaluate(() => {
        const w = window as unknown as { __f?: Element; __blurs: number };
        return { same: document.activeElement === w.__f, blurs: w.__blurs };
      });
      expect(f, 'the focused element was never blurred').toEqual({ same: true, blurs: 0 });
      expect(await sessionsOrder(page), 'still held').toEqual(['mqtail', 'old-qdock', 'qbase']);
      await expect(pill).toHaveText('1 new ↑');
    });

    // 🔴 A REGRESSION TEST (round-2 audit 🟡): with a FREE-TEXT filter typed,
    // the rank sorted a held (hidden) arrival that matches the filter above
    // worse-ranked drawn cards, so arrange re-appended every card — without
    // moveBefore that closed an open popover (audit: 7 nodes removed, filter
    // "q"). A held arrival is sorted last, so nothing drawn moves.
    test('with a filter typed, a held matching arrival moves no card and an open popover stays open', async ({ page }) => {
      test.setTimeout(120_000);
      if (leg.strip) await page.addInitScript(() => { delete (Element.prototype as unknown as { moveBefore?: unknown }).moveBefore; });
      await freshList(page, [windowLine('qbase', '@1', 'zsh', 3000), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000)], ['mqtail', 'old-qdock', 'qbase']);
      const pill = page.locator('#agents-new-pill');
      const card = (s: string) => page.locator(`#agents-list > article[data-f-session="${s}"]`);
      // A held state first (qbase becomes the newest), then the filter.
      writeFileAtomic(windowsFile, [windowLine('qbase', '@1', 'zsh', 10), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000)].join('\n') + '\n');
      await expect(pill).toHaveText('1 new ↑', { timeout: 30_000 });
      await page.locator('#agents-search').fill('q');
      await expect.poll(() => sessionsOrder(page)).toEqual(['qbase', 'old-qdock', 'mqtail']);
      // A popover open on the worst-ranked card, and every card removal counted.
      await card('mqtail').locator('[popovertarget]').first().click();
      const open = () => card('mqtail').locator(':popover-open').count();
      expect(await open(), 'control: the popover opened').toBe(1);
      await page.evaluate(() => {
        const w = window as unknown as { __removed: number };
        w.__removed = 0;
        new MutationObserver((ms) => ms.forEach((m) => m.removedNodes.forEach((n) => { if ((n as Element).tagName === 'ARTICLE') w.__removed++; })))
          .observe(document.getElementById('agents-list')!, { childList: true });
      });
      // The arrival: "qzest" STARTS with q, the best rank there is — held.
      writeFileAtomic(windowsFile, [windowLine('qbase', '@1', 'zsh', 10), windowLine('old-qdock', '@2', 'zsh', 2000), windowLine('mqtail', '@3', 'zsh', 1000), windowLine('qzest', '@4', 'zsh', 0)].join('\n') + '\n');
      await expect(card('qzest'), 'the arrival reached the list').toHaveCount(1, { timeout: 30_000 });
      await expect(card('qzest'), 'held').toBeHidden();
      await expect(pill).toHaveText('2 new ↑');
      const removed = await page.evaluate(() => (window as unknown as { __removed: number }).__removed);
      expect(await open(), 'the popover survived the held arrival').toBe(1);
      expect(removed, 'no card was removed and re-added').toBe(0);
      expect(await sessionsOrder(page), 'the drawn cards did not move').toEqual(['qbase', 'old-qdock', 'mqtail']);
    });
  });
}

// 🔴 A REGRESSION TEST (#124 round-2 audit 🟡): a held background refresh can
// still change a DRAWN card's rank — another client renames an agent while a
// filter is typed — and the filter's re-rank then moved every card (arrange
// re-appends). A held refresh does not re-rank: the drawn cards keep their
// place, and the tap applies the new rank with the rest.
test.describe('held behind the pill: a rename elsewhere', () => {
  test.use({ viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false });

  test('with a filter typed, a rename that changes a drawn card’s rank moves no card until the tap', async ({ page }) => {
    test.setTimeout(180_000);
    const stamp = Date.now().toString(36).replace(/w/g, 'x');
    // Each is in exactly one rank for "wren": a prefix, a word start, inside a word.
    const [first, second, third] = [`wren-one-${stamp}`, `big-wren-${stamp}`, `awrenx-${stamp}`];
    // The arrival MATCHES the filter: an arrival the filter hides moves nothing
    // in view, so it is applied at once and holds nothing (agents_filter.go
    // decide) — the hold this test needs is one the reader can see.
    const arrival = `zzwrenheld-${stamp}`;
    for (const n of [first, second, third]) mkchat(n, [{ role: 'user', content: 'a wren card' }]);
    try {
      await rig.login(page);
      await page.goto(rig.origin + '/agents');
      const card = (n: string) => page.locator(`#agents-list > article[data-f-name="${n}"]`);
      await expect(card(third)).toHaveCount(1, { timeout: 30_000 });
      await takeNew(page);
      // By card id: the rename changes the card's name, not its id.
      const ids = await Promise.all([first, second, third].map(async (n) => (await card(n).getAttribute('id'))!));
      const [idFirst, idSecond, idThird] = ids;
      const shown = () =>
        page.locator('#agents-list > article:not([data-held])').evaluateAll((els, want) =>
          els.filter((e) => (e as HTMLElement).offsetHeight > 0).map((e) => e.id).filter((i) => (want as string[]).includes(i)), ids);
      await page.locator('#agents-search').fill('wren');
      await expect.poll(shown).toEqual([idFirst, idSecond, idThird]);
      await page.evaluate(() => {
        const w = window as unknown as { __removed: number };
        w.__removed = 0;
        new MutationObserver((ms) => ms.forEach((m) => m.removedNodes.forEach((n) => { if ((n as Element).tagName === 'ARTICLE') w.__removed++; })))
          .observe(document.getElementById('agents-list')!, { childList: true });
      });

      // The list is HELD: an agent arrives (newest, so above every card).
      mkchat(arrival, [{ role: 'user', content: 'arrived' }]);
      await page.evaluate(() => (window as any).htmx.trigger(document.body, 'sse:agent.changed'));
      await expect(page.locator('#agents-new-pill'), 'held').toBeVisible({ timeout: 30_000 });
      await page.evaluate(() => { (window as unknown as { __removed: number }).__removed = 0; });

      // Another client renames the worst-ranked card to a PREFIX match (and so
      // perhaps the most recently active), then this tab's list refreshes in
      // the background.
      const id = idThird.replace('agent-', '');
      const r = await page.request.post(`${rig.origin}/agents/${id}/name`, { form: { name: 'wren renamed' }, headers: { Origin: rig.origin } });
      expect(r.status(), await r.text()).toBe(200);
      await page.evaluate(() => (window as any).htmx.trigger(document.body, 'sse:agent.changed'));
      await expect(page.locator(`#${idThird}`), 'the refresh brought the new name').toHaveAttribute('data-rank-title', /wren renamed/, { timeout: 30_000 });
      await expect(page.locator('#agents-new-pill'), 'the change waits').toBeVisible();
      const removed = await page.evaluate(() => (window as unknown as { __removed: number }).__removed);
      expect(removed, 'no card was removed and re-added').toBe(0);
      expect(await shown(), 'the drawn cards kept their place').toEqual([idFirst, idSecond, idThird]);

      // The tap applies it: the renamed card now ranks with the prefix match.
      await page.locator('#agents-new-pill').click();
      await expect.poll(async () => { const a = await shown(); return a.indexOf(idThird) < a.indexOf(idSecond); }, { message: 'the tap applied the new rank: the renamed card above the word-start match' }).toBe(true);
    } finally {
      for (const n of [first, second, third, arrival]) mkchat(n, null);
    }
  });
});
