import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { Browser, BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import {
  baseURL, clsConnectPort, clsFullPort, clsListPort, clsGotrueBrowserPort, clsGotrueMainPort, clsSignedBrowserPort, clsSignedConnectPort,
  clsSignedGotruePort, clsSignedMainPort, dsn, e2eRoot, gotrueSecret, password, repoRoot, versionA, versionTerm,
} from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { clsInit, readCls, resetCls, settle, ClsResult } from '../lib/cls';
import { cc, MirrorRig, mkchat } from '../lib/mirrorhost';
import { writeFileAtomic } from '../lib/atomic';

// LAYOUT SHIFT UNDER 0.01 ON EVERY PAGE, at a phone (390×844, touch) and a
// desktop (1280×800): each page is loaded, its async htmx loads are let land
// (lib/cls.ts settle), and every layout-shift entry not caused by the reader's
// own input is summed. The chief panel's open state is measured too.
//
// 🔴 THE PAGE LIST IS THE ROUTE TABLE'S, NOT A LIST SOMEONE REMEMBERED. Every
// GET route in internal/api/testdata/routes.golden — the golden the Go suite
// holds the real mux to — must be named below, either as a page with the URL it
// is measured at, or as not-a-page with the reason. A route added to the mux
// changes the golden, and this spec then fails until the route is classified:
// a new page cannot ship unmeasured by being forgotten.
//
type Where = 'shared' | 'login' | 'full' | 'public' | 'signed-in';
// `ready` is what the page is FOR, asserted visible after the measurement: a
// page that failed to draw its content shifts nothing, and must not pass as
// still. (The share sheet once did exactly that: opened empty, measured 0.)
const PAGES: Record<string, { where: Where; url: string; ready?: string }> = {
  'GET /{$}': { where: 'shared', url: '/', ready: '#tasks-list article' },
  'GET /tasks': { where: 'shared', url: '/tasks', ready: '#tasks-list article' },
  'GET /agents': { where: 'shared', url: '/agents', ready: '#agents-recent a[data-recent-entry]' },
  // The shared server builds neither store, so its /repos and /connectors fall
  // back to the board; the full server below builds both and draws the real tabs.
  'GET /repos': { where: 'full', url: '/repos', ready: '#panel-repos :is(p, li, button)' },
  'GET /connectors': { where: 'full', url: '/connectors', ready: '#panel-connectors button' },
  'GET /attention': { where: 'shared', url: '/attention', ready: '[data-testid="attention-items"] li' },
  'GET /flows': { where: 'shared', url: '/flows', ready: 'main form button[type="submit"], main form button:not([type])' },
  'GET /share': { where: 'shared', url: '/share?title=Layout%20probe&text=hello', ready: '#task-modal textarea[name="body"]' },
  'GET /account': { where: 'shared', url: '/account', ready: '#notifications' },
  'GET /account/credentials': { where: 'shared', url: '/account/credentials', ready: 'main :is(form, section)' },
  'GET /account/tokens': { where: 'shared', url: '/account/tokens', ready: '#token-name' },
  'GET /tasks/{id}': { where: 'shared', url: '/tasks/1', ready: '#task-1' },
  'GET /agents/{name}': { where: 'shared', url: '/agents/docs-sweeper', ready: '#chat-log [data-tool-group]' },
  'GET /login': { where: 'login', url: '/login', ready: '#muster-password' },
  'GET /mirror/view': { where: 'full', url: 'VIEW', ready: '#mv-log [data-tool-row]' },
  'GET /mirror/terminal': { where: 'full', url: 'TERMINAL', ready: '#mv-input' },
  'GET /agents/{name}/terminal': { where: 'full', url: 'AGENT_TERMINAL', ready: '#term' },
  'GET /invite': { where: 'public', url: '/invite', ready: '#frag-form' },
  'GET /auth/claim': { where: 'public', url: '/auth/claim', ready: '#claim-form' },
  'GET /auth/reset': { where: 'public', url: '/auth/reset', ready: '#frag-form' },
  // Signed in as a GoTrue admin, against a real GoTrue (passkeys on, plugins on).
  'GET /account/passkeys': { where: 'signed-in', url: '/account/passkeys', ready: '#passkey-add' },
  'GET /plugins': { where: 'signed-in', url: '/plugins', ready: 'h1' },
  'GET /admin/invites': { where: 'signed-in', url: '/admin/invites', ready: '#invite-ttl' },
  'GET /admin/users': { where: 'signed-in', url: '/admin/users', ready: '[data-user-email]' },
};
const NOT_A_PAGE: Record<string, string> = {
  'GET /agent/approvals/{id}': 'agent API (JSON)',
  'GET /agent/connectors': 'agent API (JSON)',
  'GET /agent/connectors/{name}': 'agent API (JSON)',
  'GET /agent/muster': 'agent API (JSON)',
  'GET /agent/task': 'agent API (JSON)',
  'GET /agents/{name}/logs/stream': 'event stream',
  'GET /agents/{name}/send': 'a redirect back to the agent page',
  'GET /agents/{name}/terminal/ws': 'websocket',
  'GET /agents/{name}/ws': 'websocket',
  'GET /apple-touch-icon-precomposed.png': 'image',
  'GET /apple-touch-icon.png': 'image',
  'GET /events': 'event stream',
  'GET /favicon.ico': 'image',
  'GET /github/callback': 'OAuth redirect',
  'GET /github/connect': 'OAuth redirect',
  'GET /health': 'JSON',
  'GET /readyz': 'JSON',
  'GET /manifest.webmanifest': 'JSON',
  'GET /metrics': 'Prometheus text',
  'GET /mirror/connect': 'the mirror host tunnel',
  'GET /mirror/terminal/send': 'a redirect back to the view',
  'GET /mirror/terminal/ws': 'websocket',
  'GET /mirror/view/asks': 'an HTML fragment (the Rendered view\'s question cards, polled into #mv-asks)',
  'GET /mirror/view/complete': 'JSON (the composer menu data)',
  'GET /mirror/view/suggest': 'JSON (the composer\'s suggested replies)',
  'GET /mirror/view/send': 'a redirect back to the view',
  'GET /mirror/view/subagent': 'an HTML fragment (an opened subagent card\'s steps, fetched into the card)',
  'GET /mirror/view/ws': 'websocket',
  'GET /send-queue': 'an htmx partial (the queue list)',
  'GET /attention/items': 'an html partial (the attention list)',
  'GET /attention/badge': 'an htmx partial (the attention count badge)',
  'GET /attention/drafts': 'JSON (the Asks page\'s Reply drafts lists, re-fetched live)',
  'GET /attention/asker': 'an HTML fragment (a session page\'s asks block, re-fetched live)',
  'GET /attention/open/{key}': 'a redirect (a notification\'s tap, resolved to its session or its Asks item)',
  'GET /static/': 'static files',
  'GET /sw.js': 'service worker script',
  'GET /tasks/{id}/attachments/{aid}': 'a file download',
  'GET /f/{id}': 'a file download (the signed link an agent fetches)',
  'GET /uploads/{id}/file': 'a file download (the owner\'s attachment thumbnail)',
};

const BAR = 0.01;
const viewports = [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
];

function goldenGets(): string[] {
  const golden = fs.readFileSync(path.join(repoRoot, 'internal/api/testdata/routes.golden'), 'utf8');
  return golden
    .split('\n')
    .map((l) => l.trim())
    .filter((l) => l.startsWith('GET /') && !l.startsWith('GET /api/') && !l.startsWith('GET /ui/') && !l.startsWith('GET /runner/') && !l.startsWith('GET /plugin/'));
}

test('every GET route is measured here, or named as not a page', () => {
  const gets = goldenGets();
  expect(gets.length, 'positive control: the golden lists page routes').toBeGreaterThan(30);
  const named = new Set([...Object.keys(PAGES), ...Object.keys(NOT_A_PAGE)]);
  expect(gets.filter((r) => !named.has(r)), 'routes with no layout-shift classification — measure them here').toEqual([]);
  expect([...named].filter((r) => !gets.includes(r)), 'classified routes the golden no longer has').toEqual([]);
});

function report(label: string, r: ClsResult): string {
  return `${label}: ${r.total} — ${JSON.stringify(r.shifts)}`;
}

async function measure(page: Page, url: string, ready?: string): Promise<ClsResult> {
  await page.goto(url);
  await settle(page);
  const r = await readCls(page);
  if (ready) await expect(page.locator(ready).first(), `${url} drew its content (${ready})`).toBeVisible();
  return r;
}

// measureWithDraft is measure() of url with a three-line draft stored for its
// message box (box: the textarea), and checks the draft WAS restored — the
// state measured — then removes it.
const draftText = 'a restored draft\nwith a second line\nand a third';
async function measureWithDraft(page: Page, url: string, box: string): Promise<ClsResult> {
  await page.goto(url);
  const key = await page.locator(box).getAttribute('data-draft-key');
  expect(key, `${url}: its box keeps a draft`).toBeTruthy();
  await page.evaluate(([k, t]) => localStorage.setItem(k, JSON.stringify({ t, at: Date.now() })), [key!, draftText]);
  const r = await measure(page, url, box);
  await expect(page.locator(box), `${url}: the draft was restored (the state measured)`).toHaveValue(draftText);
  await page.evaluate((k) => localStorage.removeItem(k), key!);
  return r;
}

// A chief agent with threads, so the panel's open state is a real one (intro,
// thread list), not the "no chief" notice. Its own agent, removed afterwards.
const chiefName = 'e2e-cls-chief-' + Date.now().toString(36);
function mkchief(del: boolean) {
  execFileSync('go', ['run', './e2e/mkchat'], {
    cwd: repoRoot,
    env: { ...process.env, DATABASE_URL: dsn, MUSTER_E2E_AGENT: chiefName, MUSTER_E2E_DISPLAY_NAME: 'chief', MUSTER_E2E_THREADS: '1', ...(del ? { MUSTER_E2E_DELETE: '1' } : {}) },
    input: del ? '' : JSON.stringify([
      { title: 'Release checklist', hoursAgo: 3, messages: [{ role: 'user', content: 'what is left for the release?' }, { role: 'assistant', content: 'Two items.' }] },
      { title: 'Flaky test triage', hoursAgo: 1, messages: [{ role: 'user', content: 'which tests flaked?' }] },
    ]),
  });
}

for (const vp of viewports) {
  test.describe(vp.name, () => {
    test.use(vp.use);
    test.beforeEach(async ({ page }) => {
      await page.addInitScript(clsInit);
    });

    // The positive control for the instrument: a shift the page did not ask for
    // (a block inserted above the content after load, no input) MUST read above
    // the bar, or a green run below means nothing.
    test('positive control: an unprovoked shift is measured', async ({ page }) => {
      await page.goto('/tasks');
      await settle(page);
      await page.evaluate(() => {
        const d = document.createElement('div');
        d.style.height = '160px';
        const list = document.getElementById('tasks-list')!;
        list.parentElement!.insertBefore(d, list);
      });
      await settle(page, 500);
      const r = await readCls(page);
      expect(r.unsupported, 'the browser reports layout-shift entries').toBeUndefined();
      expect(r.total, report('injected shift', r)).toBeGreaterThan(BAR);
    });

    test(`every page on the shared server shifts less than ${BAR}`, async ({ page }) => {
      test.setTimeout(240_000);
      const bad: string[] = [];
      const seen: string[] = [];
      // An agent opened first, so the Agents tab's "Recently viewed" strip has
      // an entry — the state a returning operator sees, and the one that shifted.
      await page.goto('/agents/docs-sweeper');
      await settle(page);
      for (const [route, p] of Object.entries(PAGES)) {
        if (p.where !== 'shared') continue;
        const r = await measure(page, p.url, p.ready);
        seen.push(`${route} ${r.total}`);
        if (r.total >= BAR) bad.push(report(`${route} (${p.url})`, r));
      }
      console.log(`[cls ${vp.name}] ` + seen.join(' | '));
      expect(bad).toEqual([]);
    });

    test(`an agent's chat with a restored draft shifts less than ${BAR}`, async ({ page }) => {
      const r = await measureWithDraft(page, '/agents/docs-sweeper', '#chat-input');
      console.log(`[cls ${vp.name}] GET /agents/{name} with a restored draft ${r.total}`);
      expect(r.total, report('/agents/docs-sweeper with a restored draft', r)).toBeLessThan(BAR);
    });

    test(`the sign-in page shifts less than ${BAR}`, async ({ browser }) => {
      const ctx = await browser.newContext({ ...vp.use, baseURL, storageState: { cookies: [], origins: [] } });
      try {
        const page = await ctx.newPage();
        await page.addInitScript(clsInit);
        const r = await measure(page, '/login', PAGES['GET /login'].ready);
        console.log(`[cls ${vp.name}] GET /login ${r.total}`);
        expect(r.total, report('/login', r)).toBeLessThan(BAR);
      } finally {
        await ctx.close();
      }
    });

    // A tool group's height animates open (tool_groups.go), moving what is
    // below it. Opened by a TAP that is the page answering the reader, which CLS
    // excludes — and this spec must exclude it the same way, while still
    // counting the very same movement when no input caused it.
    test('a tapped tool-group expand is not counted; the same expand without input is', async ({ page }) => {
      await measure(page, '/agents/docs-sweeper', '#chat-log [data-tool-group]');
      const group = page.locator('#chat-log [data-tool-group]').first();
      await resetCls(page);
      await group.locator(':scope > summary').click();
      await expect(group).toHaveAttribute('open', '');
      await settle(page, 700);
      const tapped = await readCls(page);
      expect(tapped.total, report('tapped expand', tapped)).toBeLessThan(BAR);
      expect(tapped.excluded, 'the expand did move content — and it was excluded as input').toBeGreaterThan(0);
      // Closed again, then opened by script: no input, so it counts.
      await group.locator(':scope > summary').click();
      await expect(group).not.toHaveAttribute('open', '');
      await page.waitForTimeout(700);
      await resetCls(page);
      await group.evaluate((d) => (d.querySelector(':scope > summary') as HTMLElement).click());
      await expect(group).toHaveAttribute('open', '');
      await settle(page, 700);
      const scripted = await readCls(page);
      expect(scripted.total, report('scripted expand (positive control)', scripted)).toBeGreaterThan(0);
    });

    // AFTER THE LOAD. A page does not stop moving when it has loaded: the Agents
    // list re-renders itself on every sse:agent.changed, a tab switch draws a
    // panel in place, and a boosted navigation swaps the body and then loads its
    // lists. Each is started by SCRIPT here (a scripted event or click is not
    // input), so every shift it causes is counted.
    test(`after the load: a list refresh, an arriving agent, a tab switch and a boosted navigation each shift less than ${BAR}`, async ({ page }) => {
      test.setTimeout(120_000);
      const results: Record<string, ClsResult> = {};
      const nudge = () => page.evaluate(() => (window as any).htmx.trigger(document.body, 'sse:agent.changed'));
      const refreshed = () => page.evaluate(() => new Promise<void>((r) => document.addEventListener('htmx:afterSettle', (e) => {
        if ((e.target as Element).id === 'agents-list') r();
      })));

      await page.goto('/agents/docs-sweeper'); // a recent entry, so the strip is drawn
      await measure(page, '/agents', '#agents-list > article');
      await resetCls(page);
      let done = refreshed();
      await nudge();
      await done;
      await settle(page);
      results['refresh, nothing changed'] = await readCls(page);

      const added = 'e2e-cls-added-' + Date.now().toString(36);
      const control = 'e2e-cls-control-' + Date.now().toString(36);
      mkchat(added, null);
      mkchat(added, [{ role: 'user', content: 'arrived while the list was open' }]);
      try {
        await resetCls(page);
        done = refreshed();
        await nudge();
        await done;
        await settle(page);
        // Held behind the pill (Zach's call; tap-only, confirmed): nothing in view moves.
        await expect(page.locator('#agents-new-pill'), 'the arrival waits behind the pill').toHaveText('1 new ↑');
        await expect(page.locator(`#agents-list > article[data-f-name="${added}"]`)).toBeHidden();
        results['refresh, an agent added'] = await readCls(page);
        // A tap applies it.
        await page.locator('#agents-new-pill').click();
        await expect(page.locator(`#agents-list > article[data-f-name="${added}"]`), 'the tap drew the new agent').toBeVisible();
        await settle(page);

        // 🔴 THE POSITIVE CONTROL: an arrival APPLIED at once — by a refresh
        // the reader caused (agents:changed, which the hold never holds),
        // started by script, so not input. It must read a shift: a zero here
        // would mean the instrument cannot see an arrival at all, and the held
        // zero above would prove nothing.
        // ⚠ A FRESH agent, not the held one: Chromium's layout-shift entries
        // leave out a node that was MOVED in the DOM (it is laid out anew), and
        // a held arrival is already in the list, so applying it moves nodes —
        // measured, that reads 0 while the cards visibly jump. A new card is
        // INSERTED, and the cards it pushes down are the shift being measured.
        mkchat(control, null);
        mkchat(control, [{ role: 'user', content: 'arrived, and applied at once' }]);
        await resetCls(page);
        done = refreshed();
        await page.evaluate(() => (window as any).htmx.trigger(document.body, 'agents:changed'));
        await done;
        await settle(page);
        await expect(page.locator(`#agents-list > article[data-f-name="${control}"]`), 'applied: the control agent is drawn').toBeVisible();
        await expect(page.locator('#agents-new-pill'), 'and nothing waits').toBeHidden();
        const c = await readCls(page);
        console.log(`[cls ${vp.name}] arrival applied (positive control): ${c.total}`);
        expect(c.total, report('an arrival applied without the hold (positive control)', c)).toBeGreaterThan(0);
      } finally {
        mkchat(added, null);
        mkchat(control, null);
      }

      // An in-page tab switch: /tasks → Agents, by the sidebar's own link.
      await measure(page, '/tasks', '#tasks-list article');
      await resetCls(page);
      await page.evaluate(() => (document.querySelector('.sidebar-tab[data-tab="agents"]') as HTMLElement).click());
      await expect(page).toHaveURL(/\/agents$/);
      await settle(page);
      await expect(page.locator('#agents-list > article').first()).toBeVisible();
      results['tab switch tasks → agents'] = await readCls(page);

      // A boosted navigation: from an agent's page (no panels) to the board.
      await measure(page, '/agents/docs-sweeper', '#chat-log');
      await resetCls(page);
      await page.evaluate(() => (document.querySelector('.sidebar-tab[data-tab="tasks"]') as HTMLElement).click());
      await expect(page).toHaveURL(/\/tasks$/);
      await settle(page);
      await expect(page.locator('#tasks-list article').first()).toBeVisible();
      results['boosted navigation agent → tasks'] = await readCls(page);

      console.log(`[cls ${vp.name}] after the load: ` + Object.entries(results).map(([k, r]) => `${k} ${r.total}`).join(' | '));
      // 🔴 THE ARRIVAL IS GATED TOO. Applied, it moves the open list (the
      // positive control above measures and logs by how much, every run); it is
      // held behind the "N new ↑" pill (agentsFilterScript's hold) and moves
      // nothing until the reader taps.
      const bad = Object.entries(results).filter(([, r]) => r.total >= BAR).map(([k, r]) => report(k, r));
      expect(bad).toEqual([]);
    });

    test(`the chief panel, opened and restored open, shifts less than ${BAR}`, async ({ page }) => {
      test.setTimeout(120_000);
      mkchief(true);
      mkchief(false);
      try {
        // Opened: not a tap (a tap's shifts are excluded as input) but a click
        // dispatched by script on a launcher — ChiefPanelToggle's markup, whose
        // handler is delegated on document — so whatever the panel's loading
        // moves is counted.
        await measure(page, '/tasks');
        await resetCls(page);
        await page.evaluate(() => {
          const b = document.createElement('button');
          b.setAttribute('data-chief-panel-toggle', '');
          b.style.cssText = 'position:fixed;left:-9999px;top:0';
          document.body.appendChild(b);
          b.click();
        });
        await expect(page.locator('#chief-panel')).toHaveAttribute('data-chief-panel-open', '1');
        await expect(page.locator('#chief-panel-body [data-chief-thread], #chief-panel-body #chat-form').first()).toBeAttached({ timeout: 15_000 });
        await settle(page);
        const opened = await readCls(page);
        // Restored open: the next page load comes up with the panel open and
        // loads its body itself.
        const restored = await measure(page, '/agents');
        await expect(page.locator('#chief-panel')).toHaveAttribute('data-chief-panel-open', '1');
        console.log(`[cls ${vp.name}] chief panel opened ${opened.total}, restored ${restored.total}`);
        expect(opened.total, report('chief panel opened', opened)).toBeLessThan(BAR);
        expect(restored.total, report('chief panel restored open on /agents', restored)).toBeLessThan(BAR);
      } finally {
        await page.evaluate(() => { try { localStorage.removeItem('muster.chief.v1.open.chief-panel'); } catch (e) {} });
        mkchief(true);
      }
    });
  });
}

// --- The FULL server: every optional store built — the e2eterm binary (its noop
// driver plays a terminal) with the mirror plugin on, claude-code enabled, and
// the connector and GitHub stores keyed — so the Repos and Connectors tabs, the
// mirrored views and a claude-code agent's terminal are the real pages. A fake
// mirrored host's `main` session is a Claude Code pane over a transcript this
// spec writes.
const rig = new MirrorRig(clsFullPort, 'e2e-cls', {
  MUSTER_AGENT_KINDS: 'gateway,claude-code',
  MUSTER_AGENT_CC_IMAGE: 'ghcr.io/example-org/claude-code-agent:e2e',
  MUSTER_AGENT_CC_ACCOUNTS: 'alpha',
  MUSTER_AGENT_CC_TOKEN_ALPHA: 'sk-ant-oat01-FAKE-e2e-cls-alpha',
  MUSTER_CONNECTOR_ENCRYPTION_KEY: Buffer.alloc(32, 0x5a).toString('base64'),
  // The store refuses to boot without the URL agents are told to use.
  MUSTER_CONNECT_PORT: String(clsConnectPort),
  MUSTER_AGENT_CONNECT_URL: `http://127.0.0.1:${clsConnectPort}`,
  MUSTER_GITHUB_ENCRYPTION_KEY: Buffer.alloc(32, 0x6b).toString('base64'),
});
const morigin = rig.origin;
const host = rig.host;
let ccAgent = '';

test.describe('the full server', () => {
  test.use({ storageState: { cookies: [], origins: [] } });
  test.beforeAll(async () => {
    test.setTimeout(180_000);
    await rig.start();
    const transcript = rig.transcript(
      cc.prompt('list the files') + cc.reply('Listing them.') + cc.toolUse('tu_1', 'Bash', { command: 'ls' }) +
        cc.toolResult('tu_1', 'a\nb') + cc.reply('Two files.'),
    );
    await rig.startHost({ MUSTER_MIRROR_INTERACTIVE: '1', MUSTER_MIRROR_E2E_SESSIONS: 'main,scratch', MUSTER_MIRROR_E2E_CLAUDE: 'main=' + transcript });
  });

  test.afterAll(async () => {
    await rig.stop();
    // The claude-code agent this spec dispatched, so no later spec lists it.
    if (ccAgent) mkchat(ccAgent, null);
  });

  async function signIn(browser: Browser, use: object): Promise<{ ctx: BrowserContext; page: Page }> {
    const ctx = await browser.newContext({ ...use, storageState: { cookies: [], origins: [] } });
    const page = await ctx.newPage();
    await page.goto(morigin + '/login');
    await page.fill('#muster-password', password);
    await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
    await page.addInitScript(clsInit);
    return { ctx, page };
  }

  // A claude-code agent of this spec's own, through the dispatch sheet (the
  // terminal spec's path), so its terminal page exists.
  async function dispatchCC(page: Page): Promise<string> {
    const names = async () => new Set(((await (await page.request.get(morigin + '/ui/agents')).text()).match(/href="\/agents\/([^"/?]+)"/g) || []).map((h) => h.slice(14, -1)));
    const before = await names();
    await page.goto(morigin + '/agents');
    await page.click('#fab-agents');
    const sheet = page.locator('#agent-modal form');
    await sheet.locator('[data-kind-option="claude-code"]').click();
    await sheet.locator('textarea[name="note_text"]').fill('e2e cls terminal');
    const resp = page.waitForResponse((r) => r.url().endsWith('/agents') && r.request().method() === 'POST');
    await sheet.locator('button[value="save"]').click();
    expect((await resp).status()).toBe(200);
    const created = [...(await names())].filter((n) => !before.has(n));
    expect(created.length, 'exactly one agent created').toBe(1);
    return created[0];
  }

  test(`repos, connectors, the mirrored views and an agent terminal shift less than ${BAR}, at both sizes`, async ({ browser }) => {
    test.setTimeout(180_000);
    const view = `/mirror/view?host=${host}&session=main`;
    const term = `/mirror/terminal?host=${host}&session=scratch`;
    const bad: string[] = [];
    for (const vp of viewports) {
      const { ctx, page } = await signIn(browser, vp.use);
      try {
        await expect
          .poll(async () => (await (await page.request.get(morigin + '/ui/agents')).text()).includes('data-mirror-claude'), {
            message: `main reported as a Claude pane (muster-mirror log:\n${rig.log})`,
            timeout: 20_000,
          })
          .toBe(true);
        if (!ccAgent) ccAgent = await dispatchCC(page);
        const urls: Record<string, string> = { VIEW: view, TERMINAL: term, AGENT_TERMINAL: `/agents/${ccAgent}/terminal` };
        for (const [route, p] of Object.entries(PAGES)) {
          if (p.where !== 'full') continue;
          const r = await measure(page, morigin + (urls[p.url] ?? p.url), p.ready);
          console.log(`[cls ${vp.name}] ${route} ${r.total}`);
          if (r.total >= BAR) bad.push(report(`${vp.name} ${route}`, r));
        }
        // The view really drew its transcript (a blank page shifts nothing).
        await page.goto(morigin + view);
        await expect(page.locator('#mv-log [data-tool-row]')).toHaveCount(1, { timeout: 15_000 });
        // 🔴 A RESTORED DRAFT IS NOT A SHIFT (composer_draft.go): the mirrored
        // views loaded with a three-line draft waiting for their box — it is put
        // in, and the box grown to it, before the box is painted.
        for (const [label, url] of [['VIEW', view], ['TERMINAL', term]] as const) {
          const r = await measureWithDraft(page, morigin + url, '#mv-input');
          console.log(`[cls ${vp.name}] ${label} with a restored draft ${r.total}`);
          if (r.total >= BAR) bad.push(report(`${vp.name} ${label} with a restored draft`, r));
        }
        // 🔴 THE RENDERED ↔ TERMINAL SWITCH (internal/ui/view_swap.go): the
        // first tap opens the other view in a frame over the page, later taps
        // show or hide it. Measured in BOTH documents — the page's and the
        // frame's (clsInit runs in every frame) — and each tap is started by
        // SCRIPT (a scripted click is not input), so every shift counts.
        await page.goto(morigin + view);
        await expect(page.locator('#mv-log [data-tool-row]')).toHaveCount(1, { timeout: 15_000 });
        await settle(page);
        const frame = () => page.frames().find((f) => f !== page.mainFrame() && f.url().includes('/mirror/terminal'));
        type Cls = { total: number; shifts: unknown[] };
        const frameCls = async (reset: boolean): Promise<Cls> => {
          const f = frame();
          if (!f) {
            // Before the first switch there is no frame to reset; after it, a
            // missing frame is a measurement that did not happen, never a 0.
            if (reset) return { total: 0, shifts: [] };
            throw new Error('no view frame to measure');
          }
          return f.evaluate((r) => {
            const S = (window as unknown as { __cls: { total: number; shifts: unknown[]; last: number } }).__cls;
            const out = { total: +S.total.toFixed(5), shifts: S.shifts };
            if (r) { S.total = 0; S.shifts = []; S.last = performance.now(); }
            return out;
          }, reset);
        };
        for (const [label, inFrame, link, ready] of [
          ['switch to the terminal (opens the frame)', false, '#view-terminal', '#term .xterm-rows'],
          ['switch back to the rendered view (kept)', true, '#view-rendered', '#mv-log [data-tool-row]'],
          ['switch to the terminal again (kept)', false, '#view-terminal', '#term .xterm-rows'],
        ] as const) {
          await resetCls(page);
          await frameCls(true);
          const where = inFrame ? page.frameLocator('iframe[data-muster-view]') : page;
          await where.locator(link).evaluate((a) => (a as HTMLElement).click());
          const shown = label.includes('terminal') ? page.frameLocator('iframe[data-muster-view]') : page;
          await expect(shown.locator(ready).first(), `${label} drew its content`).toBeVisible();
          await settle(page);
          await page.waitForTimeout(1000);
          const top = await readCls(page);
          const fr = await frameCls(false);
          console.log(`[cls ${vp.name}] ${label} page ${top.total} frame ${fr.total}`);
          if (top.total >= BAR) bad.push(report(`${vp.name} ${label} (the page)`, top));
          if (fr.total >= BAR) bad.push(`${vp.name} ${label} (the frame): ${fr.total} ${JSON.stringify(fr.shifts)}`);
        }
        await expect(page.locator('iframe[data-muster-view]'), 'one frame: the switch opened it once and kept it').toHaveCount(1);
      } finally {
        await ctx.close();
      }
    }
    expect(bad).toEqual([]);
  });
});

// --- A LONG AGENTS LIST, SERVER-RENDERED. The Agents tab's document draws its
// cards itself (internal/ui/agents_list_ssr.go) — the list is the first paint,
// not a request's answer — and the reader's stored filter (a quick-filter chip,
// a host tab) is applied before that paint (agents_filter_head.go).
//
// The fixture is the size of a real operator's list: sixty-four mirrored tmux
// windows on one fake host (pairwise-distinct activity, 2 minutes to ~7.5 hours
// ago), two of them pinned, and two dispatched agents with focus titles.
//
// 🔴 REGRESSION TESTS. On the tree before, #agents-list was drawn EMPTY and
// filled by its own `load` request: with that request unanswered the document
// held no card at all.
//
// ⚠ THE FIRST LIVE REFRESH (the resync the SSE stream's open fires) STILL
// RE-SENDS THE LIST, and morphs it over the identical server-rendered one. A
// version/204 protocol that answered it with no content was measured and
// removed: CLS 0 with it and 0 / 0.00006 without, at both sizes; what it saved
// was invisible DOM churn (data-list-i restamps, a class rewrite) and
// relative-time labels a second fresher ("8s"->"9s"). The morph's shift is
// measured below instead.
const lrig = new MirrorRig(clsListPort, 'e2e-clslist', { MUSTER_FOCUS_INTERVAL: '0' });
const lstamp = Date.now().toString(36);
const lagents = [`cls-list-focus-a-${lstamp}`, `cls-list-focus-b-${lstamp}`];
const NWIN = 64;
const lwin = (i: number) => 'cl' + String(i).padStart(2, '0');
// The pinned two: old windows, so their pins lift them from far down the list.
const lpinned = [40, 51];

// framesProbe records, for every animation frame from the document's first
// until a little after load, how many of #agents-list's cards are DRAWN
// (rendered boxes). A frame runs before its paint, so this is what each paint
// showed. The stored filter must never draw a card it hides: no frame may show
// more cards than the filtered list ends with.
const framesProbe = `(() => {
  window.__frames = [];
  let after = 0;
  const tick = () => {
    const l = document.getElementById('agents-list');
    if (l) {
      let v = 0;
      for (const a of l.children) if (a.tagName === 'ARTICLE' && a.getClientRects().length) v++;
      window.__frames.push(v);
      // Frames painted with cards while the document was still held before
      // its body scripts (the stall below): the positive control.
      if (v > 0 && !window.__stallEnded) window.__preStall = (window.__preStall || 0) + 1;
    }
    if (document.readyState !== 'complete' || after++ < 10) requestAnimationFrame(tick);
  };
  requestAnimationFrame(tick);
})()`;

test.describe('a long Agents list', () => {
  test.use({ storageState: { cookies: [], origins: [] } });
  let pinnedDone = false;
  test.beforeAll(async () => {
    test.setTimeout(180_000);
    await lrig.start();
    const now = Math.floor(Date.now() / 1000);
    const windows = path.join(lrig.dir, 'windows');
    const lines: string[] = [];
    for (let i = 0; i < NWIN; i++) lines.push(`${lwin(i)}|@${i + 1}|1|1|zsh|${now - (2 + 7 * i) * 60}|`);
    writeFileAtomic(windows, lines.join('\n') + '\n');
    await lrig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: windows, MUSTER_MIRROR_E2E_REFRESH: '300ms' });
    mkchat(lagents[0], [{ role: 'user', content: 'rebalance the shards' }], {
      MUSTER_E2E_ACTIVE_MINUTES_AGO: '33',
      MUSTER_E2E_FOCUS: JSON.stringify([{ subject: 'self', title: 'Rebalancing the shard map before the cutover', description: 'Four shards left.' }]),
    });
    mkchat(lagents[1], [{ role: 'user', content: 'trim the dashboards' }], {
      MUSTER_E2E_ACTIVE_MINUTES_AGO: '211',
      MUSTER_E2E_FOCUS: JSON.stringify([{ subject: 'self', title: 'Trimming the dashboards nobody opens', description: 'Twelve to go.' }]),
    });
  });
  test.afterAll(async () => {
    await lrig.stop();
    for (const a of lagents) mkchat(a, null);
  });

  for (const vp of viewports) {
    test(`${vp.name}: the first paint is the list, and load, the first refresh, a restored filter and navigation shift less than ${BAR}`, async ({ browser }) => {
      test.setTimeout(180_000);
      const ctx = await browser.newContext({ ...vp.use, storageState: { cookies: [], origins: [] } });
      try {
        const page = await ctx.newPage();
        await lrig.login(page);
        await lrig.waitListed(page, `data-f-session="${lwin(NWIN - 1)}"`);
        if (!pinnedDone) {
          for (const i of lpinned) {
            const r = await page.request.post(lrig.origin + '/agents/pins', {
              form: { kind: 'mirror', host: lrig.host, session: lwin(i), window: `@${i + 1}` },
              headers: { Origin: lrig.origin },
            });
            expect(r.status(), `pinned ${lwin(i)}`).toBe(200);
          }
          pinnedDone = true;
        }
        const total = NWIN + lagents.length;
        const listCards = page.locator('#agents-list > article');
        const mine = (i: number) => page.locator(`#agents-list > article[data-f-host="${lrig.host}"][data-f-session="${lwin(i)}"]`);

        // (1) THE FIRST PAINT HOLDS THE CARDS: with the list's own request left
        // unanswered, the document already has every one of them.
        const heldRoutes: import('@playwright/test').Route[] = [];
        await page.route('**/ui/agents', (r) => { heldRoutes.push(r); });
        await page.goto(lrig.origin + '/agents', { waitUntil: 'domcontentloaded' });
        const drawn = await listCards.count();
        await page.unroute('**/ui/agents');
        for (const r of heldRoutes) await r.abort().catch(() => {});
        expect(drawn, `the /agents document drew its cards with /ui/agents unanswered (${heldRoutes.length} held)`).toBeGreaterThanOrEqual(total);
        await expect(page.locator(`#agents-list > article:has-text("Rebalancing the shard map")`), 'a focus title is on the first paint').toHaveCount(1);
        await expect(listCards.first(), 'a pinned window leads the list').toHaveAttribute('data-f-session', new RegExp(`^cl(${lpinned.join('|')})$`));

        // (1b) AN ARRIVAL IN THE FIRST LIVE REFRESH IS HELD. The server-rendered
        // list is the drawn order (agentsFilterScript's adopt), so the very
        // first background refresh — the resync the SSE stream's open fires —
        // waits behind "N new ↑" when it carries a change, like any later one.
        // That refresh is held at the network here while an agent arrives, so
        // it is the one that carries it. (Without adopt it found no drawn order
        // and was applied at once, pushing every card down.)
        let release: () => void = () => {};
        const gate = new Promise<void>((r) => (release = r));
        let firstRefresh = 0;
        await page.route('**/ui/agents', async (r) => {
          if (firstRefresh++ === 0) await gate;
          await r.continue().catch(() => {});
        });
        await page.addInitScript(clsInit);
        await page.goto(lrig.origin + '/agents');
        await page.waitForLoadState('load');
        await expect.poll(() => firstRefresh, { message: 'the first live refresh was asked for' }).toBeGreaterThan(0);
        await resetCls(page);
        const arrival = `cls-list-arrival-${lstamp}-${vp.name.split(' ')[0]}`;
        mkchat(arrival, [{ role: 'user', content: 'arrived before the first refresh landed' }]);
        try {
          release();
          await expect(page.locator('#agents-new-pill'), 'the arrival waits behind the pill').toHaveText('1 new ↑');
          await expect(page.locator(`#agents-list > article[data-f-name="${arrival}"]`)).toBeHidden();
          await settle(page);
          const held = await readCls(page);
          expect(held.total, report('an arrival in the first live refresh', held)).toBeLessThan(BAR);
          await page.locator('#agents-new-pill').click();
          await expect(page.locator(`#agents-list > article[data-f-name="${arrival}"]`)).toBeVisible();
        } finally {
          await page.unroute('**/ui/agents');
          mkchat(arrival, null);
        }

        // (2) A RESTORED FILTER, THE LOAD AND THE FIRST LIVE REFRESH. The chip
        // "Idle >1h" and this host's tab, stored the way the page stores them.
        await page.evaluate((h) => {
          localStorage.setItem('muster:agents:chips', JSON.stringify(['idle-1h']));
          localStorage.setItem('muster:tabs:agents-host', h);
        }, lrig.host);
        const results: Record<string, ClsResult> = {};
        const statuses: number[] = [];
        page.on('response', (r) => { if (new URL(r.url()).pathname === '/ui/agents') statuses.push(r.status()); });
        await page.addInitScript(framesProbe);
        // 🔴 THE FIRST PAINT IS FORCED TO COME BEFORE THE BODY SCRIPTS. Left to
        // itself Chromium parses this document in one go and paints after
        // agentsFilterScript has run, so a missing head script read green four
        // times out of four at both sizes (review, round 1). Here the document
        // is held, between its panels and its scripts, on a parser-blocking
        // script that answers after 1.2 s: the browser paints the list while it
        // waits — with only what the HEAD has applied. (The frames counted
        // during the hold are the positive control: __preStall.)
        await page.route(lrig.origin + '/agents', async (r) => {
          const res = await r.fetch();
          const html = (await res.text()).replace('</main>', '</main><script src="/__e2e-stall.js"></script>');
          await r.fulfill({ response: res, body: html });
        });
        await page.route('**/__e2e-stall.js', async (r) => {
          await new Promise((res) => setTimeout(res, 1200));
          await r.fulfill({ contentType: 'text/javascript', body: 'window.__stallEnded = true;' });
        });
        await page.goto(lrig.origin + '/agents');
        await page.unroute(lrig.origin + '/agents');
        await settle(page);
        // The resync the SSE stream's open fires is the list's first live
        // refresh: it morphs the list over its identical server-rendered self.
        await expect.poll(() => statuses.length, { message: 'the first live refresh was made', timeout: 10_000 }).toBeGreaterThan(0);
        await settle(page);
        results['load, chip + host tab restored, first refresh'] = await readCls(page);
        const frames: number[] = await page.evaluate(() => (window as any).__frames);
        const shown = await page.evaluate(() => [...document.querySelectorAll('#agents-list > article')].filter((a) => a.getClientRects().length).length);
        // Positive control: the filter really hides cards (the 1h chip leaves
        // the windows active within the hour, and the other host's agents, out).
        expect(shown, 'the restored filter shows some of the list').toBeGreaterThan(0);
        expect(shown, 'and hides some of it').toBeLessThan(await listCards.count());
        await expect(mine(0), 'a window active 2 minutes ago is hidden by Idle >1h').toBeHidden();
        await expect(mine(20), 'a window idle 2h22m is shown').toBeVisible();
        const preStall = await page.evaluate(() => (window as any).__preStall || 0);
        expect(preStall, `control: frames were painted with cards before the body scripts ran (frames: ${frames.join(',')})`).toBeGreaterThan(0);
        expect(Math.max(...frames), `no frame drew a card the restored filter hides (frames: ${frames.join(',')})`).toBeLessThanOrEqual(shown);

        // A live nudge: the list refreshes and morphs; nothing moves.
        await resetCls(page);
        const before = statuses.length;
        await page.evaluate(() => (window as any).htmx.trigger(document.body, 'sse:agent.changed'));
        await expect.poll(() => statuses.length, { timeout: 10_000 }).toBeGreaterThan(before);
        await settle(page);
        results['a live refresh, nothing changed'] = await readCls(page);

        // Back and forward: away to the board, then Back.
        await resetCls(page);
        await page.goto(lrig.origin + '/tasks');
        await settle(page);
        await page.goBack();
        await expect(page).toHaveURL(/\/agents$/);
        await settle(page);
        await expect(mine(20)).toBeVisible();
        results['back to /agents'] = await readCls(page);
        await page.goForward();
        await expect(page).toHaveURL(/\/tasks$/);
        await settle(page);

        // A boosted navigation back to /agents, from a page with no panels.
        await page.goto(lrig.origin + '/agents/' + lagents[0]);
        await settle(page);
        await resetCls(page);
        await page.evaluate(() => (document.querySelector('.sidebar-tab[data-tab="agents"]') as HTMLElement).click());
        await expect(page).toHaveURL(/\/agents$/);
        await settle(page);
        await expect(mine(20)).toBeVisible();
        await expect(mine(0)).toBeHidden();
        results['boosted navigation to /agents'] = await readCls(page);

        console.log(`[cls ${vp.name}] long list: ` + Object.entries(results).map(([k, r]) => `${k} ${r.total}`).join(' | '));
        const bad = Object.entries(results).filter(([, r]) => r.total >= BAR).map(([k, r]) => report(k, r));
        expect(bad).toEqual([]);
        await page.evaluate(() => { localStorage.removeItem('muster:agents:chips'); localStorage.removeItem('muster:tabs:agents-host'); });
      } finally {
        await ctx.close();
      }
    });
  }
});

// --- The public pages of a GoTrue-mode server. They render without a GoTrue to
// talk to (nothing on them is fetched until a form is submitted), so the server
// is started without one.
const gport = clsGotrueMainPort;
const gbrowser = clsGotrueBrowserPort;
test.describe('public pages in gotrue mode', () => {
  let gserver: Running | undefined;
  test.beforeAll(async () => {
    gserver = await startServer(versionA, gport, {
      MUSTER_AUTH_MODE: 'gotrue',
      MUSTER_GOTRUE_URL: 'http://127.0.0.1:9',
      MUSTER_GOTRUE_JWT_SECRET: gotrueSecret,
      MUSTER_BROWSER_PORT: String(gbrowser),
      MUSTER_PUBLIC_URL: `http://localhost:${gbrowser}`,
    });
  });
  test.afterAll(async () => {
    await gserver?.stop();
  });

  test(`invite, claim and reset shift less than ${BAR}, at both sizes`, async ({ browser }) => {
    const bad: string[] = [];
    for (const vp of viewports) {
      const ctx = await browser.newContext({ ...vp.use, storageState: { cookies: [], origins: [] } });
      try {
        const page = await ctx.newPage();
        await page.addInitScript(clsInit);
        for (const [route, p] of Object.entries(PAGES)) {
          if (p.where !== 'public') continue;
          const r = await measure(page, `http://localhost:${gbrowser}${p.url}`, p.ready);
          console.log(`[cls ${vp.name}] ${route} ${r.total}`);
          if (r.total >= BAR) bad.push(report(`${vp.name} ${route}`, r));
        }
      } finally {
        await ctx.close();
      }
    }
    expect(bad).toEqual([]);
  });
});

// --- The signed-in GoTrue pages: an admin's account pages, the admin pages and
// the plugins page, as a GoTrue-mode server draws them for a signed-in admin —
// against the real GoTrue image (e2e/gotrue/compose.yml), passkeys on, plugins
// on. The admin is claimed the way invites.spec.ts claims one.
const signedURL = `http://localhost:${clsSignedBrowserPort}`;
const signedGotrue = `http://127.0.0.1:${clsSignedGotruePort}`;
const signedCompose = path.join(e2eRoot, 'gotrue', 'compose.yml');
const signedComposeEnv = {
  ...process.env,
  MUSTER_E2E_GOTRUE_PROJECT: `muster-e2e-gotrue-${clsSignedGotruePort}`,
  MUSTER_E2E_GOTRUE_PORT: String(clsSignedGotruePort),
  MUSTER_E2E_GOTRUE_SITE: signedURL,
  MUSTER_E2E_GOTRUE_PASSKEYS: 'true',
};
test.describe('signed-in pages in gotrue mode', () => {
  let sserver: Running | undefined;
  test.use({ storageState: { cookies: [], origins: [] } });
  test.beforeAll(async () => {
    test.setTimeout(300_000);
    execFileSync('docker', ['compose', '-f', signedCompose, 'up', '-d', '--wait'], { env: signedComposeEnv, stdio: 'inherit' });
    execFileSync('go', ['run', './e2e/seed', '--accounts-only'], { cwd: repoRoot, stdio: 'inherit', env: { ...process.env, DATABASE_URL: dsn } });
    sserver = await startServer(versionA, clsSignedMainPort, {
      MUSTER_AUTH_MODE: 'gotrue',
      MUSTER_GOTRUE_URL: signedGotrue,
      MUSTER_GOTRUE_JWT_SECRET: gotrueSecret,
      MUSTER_BROWSER_PORT: String(clsSignedBrowserPort),
      MUSTER_PUBLIC_URL: signedURL,
      MUSTER_PLUGINS: '1',
      MUSTER_CONNECTOR_ENCRYPTION_KEY: Buffer.alloc(32, 0x2e).toString('base64'),
      MUSTER_CONNECT_PORT: String(clsSignedConnectPort),
      MUSTER_AGENT_CONNECT_URL: `http://127.0.0.1:${clsSignedConnectPort}`,
    });
  });
  test.afterAll(async () => {
    await sserver?.stop();
    execFileSync('docker', ['compose', '-f', signedCompose, 'down', '-v'], { env: signedComposeEnv, stdio: 'inherit' });
    // The claim bound user 1 to this spec's GoTrue user: unbind it again, or the
    // next spec that claims the admin (gotrue-login does, without a reset of its
    // own) is refused.
    execFileSync('go', ['run', './e2e/seed', '--accounts-only'], { cwd: repoRoot, stdio: 'inherit', env: { ...process.env, DATABASE_URL: dsn } });
  });

  test(`passkeys, plugins and the admin pages shift less than ${BAR}, at both sizes`, async ({ browser }) => {
    test.setTimeout(180_000);
    // Claim the admin (the server binary prints the link), signed in by the claim.
    const claim = execFileSync(serverBinary(versionA), ['invite', '--claim-admin'], {
      env: { ...process.env, DATABASE_URL: dsn, MUSTER_PUBLIC_URL: signedURL, MUSTER_GOTRUE_URL: signedGotrue, MUSTER_GOTRUE_JWT_SECRET: gotrueSecret },
    }).toString().trim();
    const email = `cls-admin-${Date.now()}@example.com`;
    const pw = 'e2e-admin-password-not-a-real-one';
    const first = await browser.newContext({ storageState: { cookies: [], origins: [] } });
    const fp = await first.newPage();
    await fp.goto(claim);
    await fp.fill('#claim-email', email);
    await fp.fill('#claim-password', pw);
    await fp.fill('#claim-password2', pw);
    await Promise.all([fp.waitForURL(signedURL + '/'), fp.click('button[type=submit]')]);
    await first.close();

    const bad: string[] = [];
    for (const vp of viewports) {
      const ctx = await browser.newContext({ ...vp.use, storageState: { cookies: [], origins: [] } });
      try {
        // Each size signs in on its own: GoTrue's access tokens live 5 s here and a
        // refresh rotates the refresh token, so another context's cookies go stale.
        const page = await ctx.newPage();
        await page.goto(signedURL + '/login');
        await page.fill('#muster-email', email);
        await page.fill('#muster-password', pw);
        await Promise.all([page.waitForURL(signedURL + '/'), page.click('button[type=submit]')]);
        await page.addInitScript(clsInit);
        for (const [route, p] of Object.entries(PAGES)) {
          if (p.where !== 'signed-in') continue;
          const r = await measure(page, signedURL + p.url, p.ready);
          console.log(`[cls ${vp.name}] ${route} ${r.total}`);
          if (r.total >= BAR) bad.push(report(`${vp.name} ${route}`, r));
        }
      } finally {
        await ctx.close();
      }
    }
    expect(bad).toEqual([]);
  });
});
