import { test, expect } from '../lib/fixtures';
import { Page } from '@playwright/test';

// 🔴 THE ⋯ MENU MUST BE FULLY ON SCREEN AND ON TOP — ASSERTED BY HIT-TESTING,
// NOT BY VISIBILITY. The operator report: "Agent card three dots context menu is
// cut off by card bounds". The menu used to be an absolute dropdown inside the
// card, and the card is overflow-hidden, so the part below the card's edge was
// clipped. Playwright's toBeVisible() is TRUE for a clipped element (it has a
// box), so it cannot see this bug; what can is asking the page which element
// is at each item's centre. A clipped item answers with whatever is under it —
// the next card, the page — instead of itself.
//
// Each case is run where the clip is worst: the right-most card of the last
// grid row (the menu right-aligns against the viewport edge) and with that card
// pushed down to the viewport's bottom (the menu has no room below), at a 412px
// phone and at a desktop width. The Delete step is opened too, because it is
// what GROWS the menu.

async function cards(page: Page) {
  await page.goto('/agents');
  const list = page.locator('#agents-list article[id^="agent-"]');
  await expect(list.first()).toBeVisible();
  // The seed's three agents; a list that rendered fewer would make "the last
  // card in a row" a different card than the one this was measured on.
  await expect(list).toHaveCount(3);
  return list;
}

// lastInRow is the right-most card of the bottom-most row.
async function lastInRow(page: Page): Promise<string> {
  return page.evaluate(() => {
    const cs = [...document.querySelectorAll('#agents-list article[id^="agent-"]')] as HTMLElement[];
    const rs = cs.map((c) => ({ id: c.id, r: c.getBoundingClientRect() }));
    const bottomRowTop = Math.max(...rs.map((x) => Math.round(x.r.top)));
    const row = rs.filter((x) => Math.round(x.r.top) === bottomRowTop);
    row.sort((a, b) => b.r.right - a.r.right);
    return row[0].id;
  });
}

// pushToBottom moves the card down so its ⋯ trigger sits `room` px above the
// viewport bottom. It adds space ABOVE the list rather than scrolling, because
// three seeded cards do not make a page tall enough to scroll at 915px.
async function pushToBottom(page: Page, cardID: string, room: number) {
  await page.evaluate(
    ([id, room]) => {
      const list = document.getElementById('agents-list')!;
      const t = document.querySelector(`#${id} [aria-label="Agent actions"]`)!;
      const r = t.getBoundingClientRect();
      const add = Math.max(0, window.innerHeight - room - r.bottom);
      list.style.marginTop = add + 'px';
      window.scrollTo(0, 0);
    },
    [cardID, room] as [string, number],
  );
}

// fullyHittable reports, for every visible menu control of the card, whether
// the element at its centre is the control itself (or inside it), and whether
// its box is inside the viewport.
async function hitReport(page: Page, cardID: string, labels: string[]) {
  return page.evaluate(
    ([id, labels]) => {
      const card = document.getElementById(id)!;
      const out: string[] = [];
      let found = 0;
      for (const el of card.querySelectorAll('button, summary') as NodeListOf<HTMLElement>) {
        const label = (el.textContent || '').trim();
        if (!labels.includes(label)) continue;
        const r = el.getBoundingClientRect();
        if (r.width === 0 || r.height === 0) continue;
        found++;
        const inView = r.left >= 0 && r.top >= 0 && r.right <= document.documentElement.clientWidth && r.bottom <= window.innerHeight;
        const x = r.left + r.width / 2, y = r.top + r.height / 2;
        const hit = document.elementFromPoint(x, y);
        const ok = !!hit && (hit === el || el.contains(hit));
        if (!inView || !ok) {
          out.push(`${label}: inView=${inView} box=${Math.round(r.left)},${Math.round(r.top)},${Math.round(r.right)},${Math.round(r.bottom)} hit=${hit ? hit.tagName.toLowerCase() + (hit.id ? '#' + hit.id : '') + '.' + String(hit.className).slice(0, 40) : 'null'}`);
        }
      }
      return { found, problems: out };
    },
    [cardID, labels] as [string, string[]],
  );
}

for (const vp of [
  { name: 'phone 412x915', width: 412, height: 915 },
  { name: 'desktop 1280x800', width: 1280, height: 800 },
]) {
  test.describe(vp.name, () => {
    test.use({ viewport: { width: vp.width, height: vp.height } });

    for (const where of ['in place', 'at the viewport bottom']) {
      test(`the last card's ⋯ menu is fully visible and on top (${where})`, async ({ page }) => {
        await cards(page);
        const id = await lastInRow(page);
        // 96px: the lowest a trigger can sit and still be TAPPED. The dispatch FAB
        // (56px, 24px off the bottom-right) covers anything lower at phone width,
        // and a click there is not a click on the trigger: Playwright scrolled the
        // page 150px to find an unoccluded one, the menu then fitted below, and
        // the flip assertion went red on a geometry the test never meant to build.
        const room = 96;
        if (where !== 'in place') await pushToBottom(page, id, room);
        const trigger = page.locator(`#${id} [aria-label="Agent actions"]`);
        await trigger.click();
        if (where !== 'in place') {
          // The precondition, asserted: the trigger is STILL at the bottom after
          // the click, so "no room below" is the state actually under test.
          // pushToBottom leaves the page at scrollY 0; a click that had to scroll
          // to reach the trigger leaves it elsewhere.
          expect(await page.evaluate(() => window.scrollY), 'the click scrolled the page to reach the trigger').toBe(0);
        }

        // Lifecycle item + Delete: both must be reachable where they are drawn.
        // Polled, not sampled once: placement runs in the browser's rendering
        // steps, and a probe in the same task as the click can read the frame
        // before it. A clipped menu never becomes hittable, so a poll cannot turn
        // the old code green.
        const settled = (labels: string[], n: number) =>
          expect.poll(async () => {
            const r = await hitReport(page, id, labels);
            return r.found === n ? r.problems : [`found ${r.found} items, want ${n}`];
          }, { timeout: 3000 });
        // Lifecycle item + Delete: both must be reachable where they are drawn.
        await settled(['Start', 'Stop', 'Delete'], 2).toEqual([]);

        // The two-step delete GROWS the menu; the confirm must be reachable too.
        await page.locator(`#${id} summary`, { hasText: 'Delete' }).click();
        await settled(['Start', 'Stop', 'Delete', 'Confirm delete'], 3).toEqual([]);

        // At the bottom there is no room below, so the menu must have FLIPPED
        // above its trigger — not merely fit by accident of a short menu.
        if (where !== 'in place') {
          await expect
            .poll(
              () =>
                page.evaluate((cid) => {
                  const c = [...document.querySelectorAll(`#${cid} button`)].find((b) => (b.textContent || '').trim() === 'Confirm delete')!;
                  const t = document.querySelector(`#${cid} [aria-label="Agent actions"]`)!;
                  return t.getBoundingClientRect().top - c.getBoundingClientRect().bottom;
                }, id),
              { message: 'the grown menu did not flip above its trigger (gap = trigger top - confirm bottom)', timeout: 3000 },
            )
            .toBeGreaterThanOrEqual(0);
        }
      });
    }

    // 🔴 THE LIST RE-RENDERS UNDER AN OPEN MENU THROUGH ITS OWN hx-trigger, AND
    // THE RE-RENDER CAN RE-ORDER THE CARDS. sse:chat.reply / agent.changed /
    // agents:changed all morph #agents-list in place, and the server sorts by last
    // activity, so a reply MOVES cards. Moving the open menu's card made idiomorph
    // strip the inline position the placer wrote, while the menu stayed open and
    // the same size — so nothing re-placed it and it sat at 0,0 with its actions
    // live, away from its trigger. An earlier version of this test drove
    // htmx.ajax(), which REPLACES the node instead of morphing it, so it could only
    // ever see the menu closed and passed for the wrong reason.
    //
    // Here the response is the server's own, with the cards REVERSED, so the open
    // menu's card is guaranteed to move; the swap is fired by the list's own
    // trigger event, which is the real path.
    //
    // EVERY card, because idiomorph — not this test — chooses which nodes of a
    // re-order it moves: measured, reversing three cards moved the open card at
    // 412px and a different one at 1280px, so a single card is red at one width
    // and vacuously green at the other.
    test('a re-ordering list morph keeps the open menu beside its trigger', async ({ page }) => {
      // Only the swap this test fires is reversed; every page load is the server's.
      let armed = false;
      await page.route('**/ui/agents', async (route) => {
        if (!armed) return route.continue();
        armed = false;
        const res = await route.fetch();
        const body = await res.text();
        const starts = [...body.matchAll(/<article id="agent-/g)].map((m) => m.index!);
        const end = body.lastIndexOf('</article>') + '</article>'.length;
        const parts = starts.map((st, k) => body.slice(st, k + 1 < starts.length ? starts[k + 1] : end));
        await route.fulfill({ response: res, body: body.slice(0, starts[0]) + parts.reverse().join('') + body.slice(end) });
      });
      for (let k = 0; k < 3; k++) {
      await cards(page);
      const before = await page.evaluate(() => [...document.querySelectorAll('#agents-list article')].map((a) => a.id).join(','));
      const id = before.split(',')[k];
      await page.locator(`#${id} [aria-label="Agent actions"]`).click();
      const settled = page.waitForResponse('**/ui/agents');
      armed = true;
      await page.evaluate(() => document.body.dispatchEvent(new CustomEvent('agents:changed', { bubbles: true })));
      await settled;
      // Positive control: the morph really did move the cards. Without it, an
      // unchanged order would make "still beside" trivially true.
      await expect
        .poll(() => page.evaluate(() => [...document.querySelectorAll('#agents-list article')].map((a) => a.id).join(',')))
        .toBe(before.split(',').reverse().join(','));
      await expect
        .poll(
          () =>
            page.evaluate((cid) => {
              const m = document.getElementById('agent-menu-' + cid.replace('agent-', ''));
              if (!m || !m.matches(':popover-open')) return 'closed';
              const t = document.querySelector(`#${cid} [aria-label="Agent actions"]`)!.getBoundingClientRect();
              const r = m.getBoundingClientRect();
              const beside = Math.abs(r.right - t.right) <= 1 && (Math.abs(r.top - t.bottom) <= 8 || Math.abs(t.top - r.bottom) <= 8);
              return beside ? 'beside' : `stranded: menu ${Math.round(r.left)},${Math.round(r.top)} trigger ${Math.round(t.left)},${Math.round(t.top)}`;
            }, id),
          { timeout: 3000, message: `card ${k} (${id})` },
        )
        .toBe('beside');
      }
    });

    // 🔴 NO FRAME WITH THE MENU SHOWN AWAY FROM ITS TRIGGER — NOT EVEN THE FIRST.
    // The popover opens synchronously on the click, but the `toggle` event that
    // places it is a later task, and a rendering update runs between the two:
    // measured, a requestAnimationFrame callback saw the menu open and painted at
    // 0,0 before it jumped to its trigger. So the first frame after the click is
    // sampled from INSIDE the page: by then the menu must be not yet open, not yet
    // visible, or already beside its trigger.
    test('the menu is never shown unplaced, even on its first frame', async ({ page }) => {
      await cards(page);
      const id = await lastInRow(page);
      await page.evaluate((cid) => {
        const w = window as unknown as { __firstFrame: string };
        w.__firstFrame = 'no frame sampled';
        const t = document.querySelector(`#${cid} [aria-label="Agent actions"]`)!;
        t.addEventListener('click', () => {
          requestAnimationFrame(() => {
            const m = document.getElementById('agent-menu-' + cid.replace('agent-', ''))!;
            if (!m.matches(':popover-open')) return void (w.__firstFrame = 'not open yet');
            if (getComputedStyle(m).visibility !== 'visible') return void (w.__firstFrame = 'open, not visible yet');
            const tr = t.getBoundingClientRect();
            const r = m.getBoundingClientRect();
            const beside = Math.abs(r.right - tr.right) <= 1 && (Math.abs(r.top - tr.bottom) <= 8 || Math.abs(tr.top - r.bottom) <= 8);
            w.__firstFrame = beside ? 'beside' : `SHOWN UNPLACED at ${Math.round(r.left)},${Math.round(r.top)}`;
          });
        });
      }, id);
      await page.locator(`#${id} [aria-label="Agent actions"]`).click();
      await expect(page.locator(`#${id} summary`, { hasText: 'Delete' })).toBeVisible();
      const read = () => page.evaluate(() => (window as unknown as { __firstFrame: string }).__firstFrame);
      const ok = ['not open yet', 'open, not visible yet', 'beside'];
      expect(ok, 'first open').toContain(await read());

      // And on a REOPEN after the card has moved: a menu that kept its last
      // position would be shown for a frame where the trigger USED to be.
      await page.keyboard.press('Escape');
      await page.evaluate(() => {
        document.getElementById('agents-list')!.style.marginTop = '120px';
        (window as unknown as { __firstFrame: string }).__firstFrame = 'no frame sampled';
      });
      await page.locator(`#${id} [aria-label="Agent actions"]`).click();
      await expect(page.locator(`#${id} summary`, { hasText: 'Delete' })).toBeVisible();
      expect(ok, 'reopen after the card moved').toContain(await read());
    });

    // Closing the menu resets it: reopening starts at step 1 (no armed "Confirm
    // delete" waiting from last time).
    test('reopening the menu starts at step 1 of the delete', async ({ page }) => {
      await cards(page);
      const id = await lastInRow(page);
      const trigger = page.locator(`#${id} [aria-label="Agent actions"]`);
      const confirm = page.locator(`#${id} button`, { hasText: 'Confirm delete' });
      await trigger.click();
      await page.locator(`#${id} summary`, { hasText: 'Delete' }).click();
      await expect(confirm).toBeVisible();
      await page.keyboard.press('Escape');
      await trigger.click();
      await expect(page.locator(`#${id} summary`, { hasText: 'Delete' })).toBeVisible();
      await expect(confirm).toBeHidden();
    });

    // Start/Stop post and morph the list; the menu must not linger over the
    // result. The request is answered here, so no seeded agent is started.
    test('a menu action closes the menu', async ({ page }) => {
      await page.route('**/agents/*/st*', (r) => r.fulfill({ status: 204, body: '' }));
      await cards(page);
      const id = await lastInRow(page);
      await page.locator(`#${id} [aria-label="Agent actions"]`).click();
      const action = page.locator(`#${id} button`, { hasText: /^(Start|Stop)$/ });
      await expect(action).toBeVisible();
      await action.click();
      await expect(action).toBeHidden();
    });

    test('Escape and an outside tap close the menu', async ({ page }) => {
      await cards(page);
      const id = await lastInRow(page);
      const trigger = page.locator(`#${id} [aria-label="Agent actions"]`);
      const item = page.locator(`#${id} summary`, { hasText: 'Delete' });
      await trigger.click();
      await expect(item).toBeVisible();
      await page.keyboard.press('Escape');
      await expect(item).toBeHidden();
      await trigger.click();
      await expect(item).toBeVisible();
      await page.mouse.click(5, vp.height - 5);
      await expect(item).toBeHidden();
    });
  });
}
