import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { binDir, dsn, focusPort, password, repoRoot, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { clsInit, readCls, resetCls, settle } from '../lib/cls';
import { writeFileAtomic } from '../lib/atomic';

// SESSION FOCUS ON THE CARDS, in a real browser, against a real muster-server
// (MUSTER_MIRROR=1, its own port) and a real muster-mirror over the tunnel (the
// `-tags e2emirror` per-window fake). A dispatched agent of this spec's own
// (e2e/mkchat) and a mirrored Claude Code window each carry a focus (written by
// mkchat's MUSTER_E2E_FOCUS — the mirrored one keyed by the window's CLAUDE
// SESSION ID, which the host reports from the transcript's name). On a phone
// and on a desktop, each card's footer chip opens a popover with the title,
// the description and "updated … ago", inside the viewport; and the filter
// box finds each card by a word only its focus carries.
//
// FOCUS AS THE CARD'S TITLE, AND FOCUS HISTORY (migration 0022): the focus
// title heads each card — a popover trigger that does not navigate, reached by
// Tab, opened by Enter, closed by Escape — over the card's own name; the
// rendered view's "Your messages" sheet is tabbed with "Focus history" (arrow
// keys, the choice kept across a reload) and scrolls back through 46 history
// rows a page at a time; the agent's page has the same history in a dialog;
// and none of it shifts the layout.

const origin = `http://localhost:${focusPort}`;
const stamp = Date.now().toString(36);
const host = 'e2e-focus-' + stamp;
const agentName = 'e2e-focus-' + stamp;
const sid = '0b2c3d4e-5f60-4718-8a9b-' + stamp.padStart(12, '0').slice(-12);
const mirrorBin = path.join(binDir, 'muster-mirror-e2e');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'focus-'));
const windowsFile = path.join(dir, 'windows');
const transcript = path.join(dir, sid + '.jsonl');
// A second Claude window whose only refresh FAILED: its card says why.
const sid2 = '0b2c3d4e-5f60-4718-8a9c-' + stamp.padStart(12, '0').slice(-12);
const transcript2 = path.join(dir, sid2 + '.jsonl');
// `history`: that many earlier focuses are written first (e2e/mkchat), so the
// mirrored session's history is 46 rows — three pages of 20 — and the agent's 4.
const MIRROR_HISTORY = 45;
const AGENT_HISTORY = 3;
const focusEnv = JSON.stringify([
  { subject: 'self', title: 'Reconciling the quarterly ledgers', description: 'Matching payouts to the bank export. Two left.', history: AGENT_HISTORY },
  { subject: 'mirror:' + sid, title: 'Bisecting the flaky tunnel test', description: 'The backoff resets too early.', history: MIRROR_HISTORY },
  { subject: 'mirror:' + sid2, error: 'no OpenRouter key' },
]);
let server: Running | undefined;
let mirrorProc: ChildProcess | undefined;
let mirrorLog = '';
let agentCleanup = false;
let signedIn: Awaited<ReturnType<BrowserContext['cookies']>> | undefined;

function mkchat(extra: NodeJS.ProcessEnv, input = '') {
  execFileSync('go', ['run', './e2e/mkchat'], {
    cwd: repoRoot,
    input,
    env: { ...process.env, DATABASE_URL: dsn, MUSTER_E2E_AGENT: agentName, MUSTER_E2E_FOCUS: focusEnv, ...extra },
  });
}

test.beforeAll(async () => {
  execFileSync('go', ['build', '-tags', 'e2emirror', '-o', mirrorBin, './cmd/muster-mirror'], { cwd: repoRoot, stdio: 'inherit' });
  agentCleanup = true;
  mkchat({}, JSON.stringify([{ role: 'user', content: 'Reconcile the ledgers.' }]));
  server = await startServer(versionTerm, focusPort, {
    MUSTER_MIRROR: '1',
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
    MUSTER_FOCUS_INTERVAL: '0',
  });
  const token = execFileSync(serverBinary(versionTerm), ['mirror', 'token', 'create', host], { env: { ...process.env, DATABASE_URL: dsn } })
    .toString()
    .trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
  fs.writeFileSync(transcript, '');
  fs.writeFileSync(transcript2, '');
  const now = Math.floor(Date.now() / 1000);
  // session|id|index|active|command|activity|claude-transcript
  writeFileAtomic(windowsFile, `focuswin|@1|1|1|claude|${now - 60}|${transcript}\nfocuswin|@2|2|0|zsh|${now - 120}|\nfocuswin|@3|3|0|claude|${now - 180}|${transcript2}\nreconciling|@4|1|1|zsh|${now - 240}|\n`);
  mirrorProc = spawn(mirrorBin, [], {
    env: {
      ...process.env,
      MUSTER_MIRROR_URL: origin,
      MUSTER_MIRROR_ALLOW_HTTP: '1',
      MUSTER_MIRROR_TOKEN: token,
      MUSTER_MIRROR_HOST: host,
      MUSTER_MIRROR_E2E_WINDOWS_FILE: windowsFile,
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  mirrorProc.stdout?.on('data', (d) => (mirrorLog += d));
  mirrorProc.stderr?.on('data', (d) => (mirrorLog += d));
});

test.afterAll(async () => {
  if (mirrorProc && mirrorProc.exitCode === null) {
    const done = new Promise((r) => mirrorProc!.once('exit', r));
    mirrorProc.kill('SIGTERM');
    await done;
  }
  await server?.stop();
  if (agentCleanup) mkchat({ MUSTER_E2E_DELETE: '1' });
  fs.rmSync(dir, { recursive: true, force: true });
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

const claudeCard = (page: Page) =>
  page.locator(`#agents-list article[data-mirror-card]:has(a[href*="host=${host}"][href*="window=%401"])`);
const shellCard = (page: Page) =>
  page.locator(`#agents-list article[data-mirror-card]:has(a[href*="host=${host}"][href*="window=%402"])`);
const failedCard = (page: Page) =>
  page.locator(`#agents-list article[data-mirror-card]:has(a[href*="host=${host}"][href*="window=%403"])`);
const agentCard = (page: Page) => page.locator(`#agents-list article[id^="agent-"]:has(a[href="/agents/${agentName}"])`);
const viewURL = origin + '/mirror/view?' + new URLSearchParams({ host, session: 'focuswin', window: '@1' }).toString();

async function openList(page: Page) {
  await login(page);
  await page.goto(origin + '/agents');
  await expect(claudeCard(page), `the host's Claude window (log:\n${mirrorLog})`).toHaveCount(1, { timeout: 20_000 });
  await expect(agentCard(page)).toHaveCount(1);
}

for (const vp of [
  { name: 'phone 390×844 touch', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.name, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });

    for (const c of [
      { kind: 'agent card', card: agentCard, title: 'Reconciling the quarterly ledgers', desc: 'Matching payouts to the bank export. Two left.', name: agentName },
      { kind: 'mirror card', card: claudeCard, title: 'Bisecting the flaky tunnel test', desc: 'The backoff resets too early.', name: 'focuswin:1' },
    ]) {
      test(`the ${c.kind}'s focus title heads it and opens a popover with the focus, without navigating`, async ({ page }) => {
        await openList(page);
        const card = c.card(page);
        const chip = card.locator('[data-card-focus]');
        await expect(chip).toHaveCount(1);
        await expect(chip).toContainText(c.title);
        // It HEADS the card: its own slot, above the nav anchor, with the card's
        // name under it as the subtitle.
        await expect(card.locator('[data-card-slot="focus"] [data-card-focus]')).toHaveCount(1);
        const subtitle = card.locator('[data-card-subtitle]');
        await expect(subtitle).toContainText(c.name);
        const tb = (await chip.boundingBox())!;
        // The mirrored subtitle's wrapper is display:contents; its box is the name.
        const subBox = card.locator('[data-card-subtitle], [data-card-subtitle] > [data-mirror-name]').last();
        const sb = (await subBox.boundingBox())!;
        expect(tb.y + tb.height, 'the focus title sits above the subtitle').toBeLessThanOrEqual(sb.y + 1);
        const tfs = await chip.evaluate((e) => parseFloat(getComputedStyle(e).fontSize));
        const sfs = await subBox.evaluate((e) => parseFloat(getComputedStyle(e).fontSize));
        expect(tfs, 'the title is drawn larger than the subtitle').toBeGreaterThan(sfs);
        expect(tb.height, 'the title is a 44px tap target').toBeGreaterThanOrEqual(44);
        // It LOOKS pressable: a chevron, and a pointer.
        await expect(chip.locator('svg')).toBeVisible();
        const pop = card.locator('[data-focus-popover]');
        await expect(pop).toBeHidden();
        await expect(chip).toHaveAttribute('aria-expanded', 'false');
        if (vp.use.hasTouch) await chip.tap();
        else await chip.click();
        await expect(pop).toBeVisible();
        await expect(chip).toHaveAttribute('aria-expanded', 'true');
        await expect(pop.locator('[data-focus-title]')).toHaveText(c.title);
        await expect(pop.locator('[data-focus-description]')).toHaveText(c.desc);
        await expect(pop.locator('[data-focus-updated]')).toContainText('updated');
        await expect(pop.locator('[data-focus-updated]')).toContainText('ago');
        // Placed (no longer invisible) and inside the viewport.
        await expect.poll(() => pop.evaluate((e) => getComputedStyle(e).visibility)).toBe('visible');
        const pb = (await pop.boundingBox())!;
        expect(pb.x, 'the popover starts inside the viewport').toBeGreaterThanOrEqual(0);
        expect(pb.x + pb.width, 'the popover ends inside the viewport').toBeLessThanOrEqual(vp.use.viewport.width);
        expect(pb.y + pb.height).toBeLessThanOrEqual(vp.use.viewport.height);
        // The title is outside the card's nav anchor: opening it did not navigate.
        await page.waitForTimeout(300);
        expect(new URL(page.url()).pathname).toBe('/agents');
        await page.keyboard.press('Escape');
        await expect(pop).toBeHidden();
        await expect(chip).toHaveAttribute('aria-expanded', 'false');
        // The rest of the card still opens it: its subtitle is in the nav anchor.
        await Promise.all([page.waitForURL((u) => u.pathname !== '/agents'), subtitle.click()]);
      });

      test(`the ${c.kind}'s focus title works from the keyboard: Tab reaches it, Enter opens, Escape closes`, async ({ page }) => {
        await openList(page);
        const card = c.card(page);
        const chip = card.locator('[data-card-focus]');
        const pop = card.locator('[data-focus-popover]');
        // Tab from the last control of the card's control cluster lands on it.
        await card.locator('[data-card-slot="controls"] button:visible').last().focus();
        await page.keyboard.press('Tab');
        await expect(chip).toBeFocused();
        await page.keyboard.press('Enter');
        await expect(pop).toBeVisible();
        await expect(chip).toHaveAttribute('aria-expanded', 'true');
        expect(new URL(page.url()).pathname, 'Enter on the title does not navigate').toBe('/agents');
        await page.keyboard.press('Escape');
        await expect(pop).toBeHidden();
        await expect(chip).toHaveAttribute('aria-expanded', 'false');
        expect(new URL(page.url()).pathname).toBe('/agents');
      });
    }

    // A list refresh MORPHS the open card: the trigger's attributes are
    // rewritten to the server's aria-expanded="false" while the popover stays
    // open, and no toggle fires. The title must still say expanded — and keep
    // the chevron turned and the highlight, both keyed on aria-expanded.
    test('an open focus popover keeps its title expanded through a list refresh', async ({ page }) => {
      await openList(page);
      // Styles are read as end states: no transition caught half-way.
      await page.addStyleTag({ content: '*, *::before, *::after { transition: none !important; }' });
      const card = agentCard(page);
      const chip = card.locator('[data-card-focus]');
      const pop = card.locator('[data-focus-popover]');
      const chevron = chip.locator('svg');
      const closedBg = await chip.evaluate((e) => getComputedStyle(e).backgroundColor);
      const closedTurn = await chevron.evaluate((e) => getComputedStyle(e).transform);
      await chip.click();
      await expect(pop).toBeVisible();
      await expect(chip).toHaveAttribute('aria-expanded', 'true');
      await page.mouse.move(0, 0); // no hover, so the highlight is aria-expanded's alone
      const openBg = await chip.evaluate((e) => getComputedStyle(e).backgroundColor);
      const openTurn = await chevron.evaluate((e) => getComputedStyle(e).transform);
      expect(openBg, 'control: the open title is highlighted').not.toBe(closedBg);
      expect(openTurn, 'control: the open chevron is turned').not.toBe(closedTurn);
      // A refresh of the list, as an agent event would start it; wait for it to land.
      const settled = page.evaluate(() => new Promise<void>((resolve) => {
        document.body.addEventListener('htmx:afterSettle', (e) => {
          if ((e.target as HTMLElement).id === 'agents-list') resolve();
        });
      }));
      await page.evaluate(() => (window as any).htmx.trigger(document.body, 'agents:changed'));
      await settled;
      await expect(pop, 'the popover stays open through the morph').toBeVisible();
      await expect(chip).toHaveAttribute('aria-expanded', 'true');
      expect(await chip.evaluate((e) => getComputedStyle(e).backgroundColor), 'the highlight stays').toBe(openBg);
      expect(await chevron.evaluate((e) => getComputedStyle(e).transform), 'the chevron stays turned').toBe(openTurn);
      await page.keyboard.press('Escape');
      await expect(chip).toHaveAttribute('aria-expanded', 'false');
    });

    // 🔴 A DISCRIMINATING PAIR: "reconciling" is the START of the agent's FOCUS
    // TITLE and the start of a shell window's NAME ("reconciling:1"). Ranked by
    // names (data-rank-title), the window ranks first; were the visible heading
    // — the focus title — ranked, the agent would tie it at the top and keep
    // its list place, ahead. Both stay listed: the filter matches focus text.
    test("typing a card's NAME still ranks it first, though another card is headed by that word", async ({ page }) => {
      await openList(page);
      const named = page.locator(`#agents-list article[data-mirror-card]:has(a[href*="host=${host}"][href*="session=reconciling"])`);
      await expect(named).toHaveCount(1);
      const before = await page.locator('#agents-list > article').evaluateAll((as) => as.map((a) => a.id));
      expect(before.indexOf(await agentCard(page).getAttribute('id') as string), 'control: unfiltered, the agent is listed before the window')
        .toBeLessThan(before.indexOf(await named.getAttribute('id') as string));
      await page.locator('#agents-search').fill('reconciling');
      await expect(agentCard(page)).toBeVisible();
      await expect(named).toBeVisible();
      const ids = await page.locator('#agents-list > article:visible').evaluateAll((as) => as.map((a) => a.id));
      expect(ids[0], `ranked ${ids}`).toBe(await named.getAttribute('id'));
    });

    test('a session whose refresh failed says why it has no focus', async ({ page }) => {
      await openList(page);
      const card = failedCard(page);
      await expect(card).toHaveCount(1);
      const chip = card.locator('[data-card-focus]');
      await expect(chip).toContainText('unavailable');
      await expect(chip).toHaveAttribute('data-focus-stale', '');
      if (vp.use.hasTouch) await chip.tap();
      else await chip.click();
      const pop = card.locator('[data-focus-popover]');
      await expect(pop).toBeVisible();
      await expect(pop.locator('[data-focus-problem]')).toContainText('No focus: no OpenRouter key');
      await expect(pop.locator('[data-focus-title]')).toHaveCount(0);
      const pb = (await pop.boundingBox())!;
      expect(pb.x).toBeGreaterThanOrEqual(0);
      expect(pb.x + pb.width).toBeLessThanOrEqual(vp.use.viewport.width);
    });

    test('a window whose host reports no Claude session has no chip, and the filter finds cards by their focus', async ({ page }) => {
      await openList(page);
      await expect(shellCard(page)).toHaveCount(1);
      await expect(shellCard(page).locator('[data-card-focus]')).toHaveCount(0);
      const search = page.locator('#agents-search');
      await search.fill('quarterly ledgers');
      await expect(agentCard(page)).toBeVisible();
      await expect(claudeCard(page)).toBeHidden();
      await search.fill('flaky tunnel');
      await expect(claudeCard(page)).toBeVisible();
      await expect(agentCard(page)).toBeHidden();
      await expect(shellCard(page)).toBeHidden();
    });

    test('the rendered view\'s sheet tabs "Your messages" with "Focus history", by arrow keys, and keeps the choice', async ({ page }) => {
      await login(page);
      await page.goto(viewURL);
      // Start from no stored choice.
      await page.evaluate(() => { try { localStorage.clear(); } catch (e) {} });
      await page.reload();
      const opener = page.locator('#chat-index-open');
      const sheet = page.locator('#chat-index');
      const msgsTab = sheet.locator('[role="tab"][data-tab="messages"]');
      const histTab = sheet.locator('[role="tab"][data-tab="focus"]');
      const msgsPanel = sheet.locator('#chat-index-tabs-panel-messages');
      const histPanel = sheet.locator('#chat-index-tabs-panel-focus');
      await opener.click();
      await expect(sheet).toBeVisible();
      await expect(sheet.locator('[role="tablist"]')).toBeVisible();
      await expect(msgsTab).toHaveAttribute('aria-selected', 'true');
      await expect(histTab).toHaveAttribute('aria-selected', 'false');
      await expect(msgsTab).toHaveAttribute('tabindex', '0');
      await expect(histTab).toHaveAttribute('tabindex', '-1');
      await expect(msgsPanel).toBeVisible();
      await expect(histPanel).toBeHidden();
      // ArrowRight: the next tab is focused AND selected; its panel shows.
      await msgsTab.focus();
      await page.keyboard.press('ArrowRight');
      await expect(histTab).toBeFocused();
      await expect(histTab).toHaveAttribute('aria-selected', 'true');
      await expect(histTab).toHaveAttribute('tabindex', '0');
      await expect(msgsTab).toHaveAttribute('tabindex', '-1');
      await expect(histPanel).toBeVisible();
      await expect(msgsPanel).toBeHidden();
      await expect(histPanel.locator('[data-focus-history-item]').first()).toContainText('Bisecting the flaky tunnel test');
      // ArrowRight wraps; ArrowLeft goes back; End/Home jump.
      await page.keyboard.press('ArrowRight');
      await expect(msgsTab).toBeFocused();
      await expect(msgsTab).toHaveAttribute('aria-selected', 'true');
      await page.keyboard.press('ArrowLeft');
      await expect(histTab).toHaveAttribute('aria-selected', 'true');
      await page.keyboard.press('Home');
      await expect(msgsTab).toHaveAttribute('aria-selected', 'true');
      await page.keyboard.press('End');
      await expect(histTab).toHaveAttribute('aria-selected', 'true');
      // Kept across a reload, for this window.
      await page.reload();
      await page.locator('#chat-index-open').click();
      await expect(sheet.locator('[role="tab"][data-tab="focus"]')).toHaveAttribute('aria-selected', 'true');
      await expect(sheet.locator('#chat-index-tabs-panel-focus')).toBeVisible();
      await expect(sheet.locator('#chat-index-tabs-panel-messages')).toBeHidden();
      // A click selects too, and is kept.
      await sheet.locator('[role="tab"][data-tab="messages"]').click();
      await page.reload();
      await page.locator('#chat-index-open').click();
      await expect(sheet.locator('[role="tab"][data-tab="messages"]')).toHaveAttribute('aria-selected', 'true');
    });

    test('the focus history scrolls back through every page, newest first, to its start', async ({ page }) => {
      await login(page);
      await page.goto(viewURL);
      await page.locator('#chat-index-open').click();
      await page.locator('#chat-index [role="tab"][data-tab="focus"]').click();
      const panel = page.locator('#chat-index-tabs-panel-focus');
      const items = panel.locator('[data-focus-history-item]');
      const total = MIRROR_HISTORY + 1;
      await expect(items.first()).toContainText('Bisecting the flaky tunnel test');
      await expect(items.first().locator('[data-focus-history-description]')).toHaveText('The backoff resets too early.');
      await expect(items.first().locator('[data-focus-history-at]')).toContainText('ago');
      // One page at first: the sentinel below it has not been scrolled to.
      await expect(items).toHaveCount(20);
      await expect(panel.locator('[data-focus-history-end]')).toHaveCount(0);
      const seen: number[] = [];
      for (let i = 0; i < 6 && (await items.count()) < total; i++) {
        await panel.evaluate((p) => { p.scrollTop = p.scrollHeight; });
        await expect.poll(() => items.count()).toBeGreaterThan(seen.length ? seen[seen.length - 1] : 20);
        seen.push(await items.count());
      }
      await expect(items).toHaveCount(total);
      expect(seen, 'the history arrived a page at a time').toEqual([40, total]);
      await panel.evaluate((p) => { p.scrollTop = p.scrollHeight; });
      await expect(panel.locator('[data-focus-history-end]')).toBeVisible();
      await expect(items.last()).toContainText('Earlier focus 01');
      // Newest first, and nothing repeated or skipped across the page joins.
      const titles = await panel.locator('[data-focus-history-title]').allTextContents();
      const want = ['Bisecting the flaky tunnel test'];
      for (let i = MIRROR_HISTORY; i >= 1; i--) want.push(`Earlier focus ${String(i).padStart(2, '0')}`);
      expect(titles).toEqual(want);
    });

    test("the dispatched agent's page has its focus history in a dialog", async ({ page }) => {
      await login(page);
      await page.goto(origin + '/agents/' + agentName);
      const open = page.locator('#focus-history-open');
      const dialog = page.locator('dialog#focus-history');
      await expect(open).toBeVisible();
      await expect(dialog).toBeHidden();
      await expect(open).toHaveAttribute('aria-expanded', 'false');
      if (vp.use.hasTouch) await open.tap();
      else await open.click();
      await expect(dialog).toBeVisible();
      await expect(open).toHaveAttribute('aria-expanded', 'true');
      const items = dialog.locator('[data-focus-history-item]');
      await expect(items).toHaveCount(AGENT_HISTORY + 1);
      await expect(items.first()).toContainText('Reconciling the quarterly ledgers');
      await expect(items.last()).toContainText('Earlier focus 01');
      await expect(dialog.locator('[data-focus-history-end]')).toBeVisible();
      const db = (await dialog.boundingBox())!;
      expect(db.x).toBeGreaterThanOrEqual(0);
      expect(db.x + db.width).toBeLessThanOrEqual(vp.use.viewport.width);
      expect(db.y + db.height).toBeLessThanOrEqual(vp.use.viewport.height + 1);
      await page.keyboard.press('Escape');
      await expect(dialog).toBeHidden();
      await expect(open).toHaveAttribute('aria-expanded', 'false');
      // The close button closes it too.
      await open.click();
      await expect(dialog).toBeVisible();
      await dialog.locator('[data-focus-history-close]').click();
      await expect(dialog).toBeHidden();
    });

    test('a card with its focus as its title, the rendered view and the agent page shift the layout less than 0.01', async ({ page }, testInfo) => {
      await login(page);
      await page.addInitScript(clsInit);
      const bad: string[] = [];
      for (const [label, url, ready] of [
        ['/agents', origin + '/agents', '#agents-list [data-card-slot="focus"] [data-card-focus]'],
        ['rendered view', viewURL, '#mv-log'],
        ['agent page', origin + '/agents/' + agentName, '#focus-history-open'],
      ] as const) {
        await page.goto(url);
        await settle(page);
        const r = await readCls(page);
        expect(r.unsupported, 'the browser reports layout-shift entries').toBeUndefined();
        await expect(page.locator(ready).first(), `${label} drew its content`).toBeVisible();
        console.log(`[focus cls ${vp.name}] ${label} ${r.total}`);
        if (r.total >= 0.01) bad.push(`${label}: ${r.total} ${JSON.stringify(r.shifts)}`);
        if (label === '/agents') {
          await agentCard(page).screenshot({ path: testInfo.outputPath(`agent-card-focus-${vp.use.viewport.width}.png`) });
          await claudeCard(page).screenshot({ path: testInfo.outputPath(`mirror-card-focus-${vp.use.viewport.width}.png`) });
        }
      }
      // The history arriving (by script: no input, so every shift counts)
      // moves nothing: the sheet is a fixed height, the rows land inside it.
      await page.goto(viewURL);
      await settle(page);
      await resetCls(page);
      await page.evaluate(() => {
        (window as any).muIndex.open();
        const tab = document.querySelector('#chat-index [role="tab"][data-tab="focus"]') as HTMLElement;
        (window as any).muTabs.select(tab.closest('[data-tabs]'), tab, {});
      });
      await expect(page.locator('#chat-index-tabs-panel-focus [data-focus-history-item]')).toHaveCount(20);
      await settle(page, 500);
      const opened = await readCls(page);
      console.log(`[focus cls ${vp.name}] history tab loaded ${opened.total}`);
      if (opened.total >= 0.01) bad.push(`history tab: ${opened.total} ${JSON.stringify(opened.shifts)}`);
      expect(bad).toEqual([]);
    });
  });
}
