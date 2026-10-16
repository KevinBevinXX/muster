import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { Browser, BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import {
  baseURL, clsConnectPort, clsFullPort, clsGotrueBrowserPort, clsGotrueMainPort, clsSignedBrowserPort, clsSignedConnectPort,
  clsSignedGotruePort, clsSignedMainPort, dsn, e2eRoot, gotrueSecret, password, repoRoot, versionA, versionTerm,
} from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { clsInit, readCls, resetCls, settle, ClsResult } from '../lib/cls';
import { cc, MirrorRig, mkchat } from '../lib/mirrorhost';

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
  'GET /flows': { where: 'shared', url: '/flows' },
  'GET /share': { where: 'shared', url: '/share?title=Layout%20probe&text=hello', ready: '#task-modal textarea[name="body"]' },
  'GET /account': { where: 'shared', url: '/account' },
  'GET /account/credentials': { where: 'shared', url: '/account/credentials' },
  'GET /account/tokens': { where: 'shared', url: '/account/tokens' },
  'GET /tasks/{id}': { where: 'shared', url: '/tasks/1', ready: '#task-1' },
  'GET /agents/{name}': { where: 'shared', url: '/agents/docs-sweeper', ready: '#chat-log [data-tool-group]' },
  'GET /login': { where: 'login', url: '/login' },
  'GET /mirror/view': { where: 'full', url: 'VIEW', ready: '#mv-log [data-tool-row]' },
  'GET /mirror/terminal': { where: 'full', url: 'TERMINAL', ready: '#mv-input' },
  'GET /agents/{name}/terminal': { where: 'full', url: 'AGENT_TERMINAL', ready: '#term' },
  'GET /invite': { where: 'public', url: '/invite' },
  'GET /auth/claim': { where: 'public', url: '/auth/claim' },
  'GET /auth/reset': { where: 'public', url: '/auth/reset' },
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
  'GET /mirror/view/send': 'a redirect back to the view',
  'GET /mirror/view/ws': 'websocket',
  'GET /send-queue': 'an htmx partial (the queue list)',
  'GET /static/': 'static files',
  'GET /sw.js': 'service worker script',
  'GET /tasks/{id}/attachments/{aid}': 'a file download',
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

    test(`the sign-in page shifts less than ${BAR}`, async ({ browser }) => {
      const ctx = await browser.newContext({ ...vp.use, baseURL, storageState: { cookies: [], origins: [] } });
      try {
        const page = await ctx.newPage();
        await page.addInitScript(clsInit);
        const r = await measure(page, '/login');
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
      } finally {
        await ctx.close();
      }
    }
    expect(bad).toEqual([]);
  });
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
          const r = await measure(page, `http://localhost:${gbrowser}${p.url}`);
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
