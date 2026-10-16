import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { binDir, dsn, hookToken, mirrorPort, password, repoRoot, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { touchDrag } from '../lib/touch';

// MIRRORED sessions at a phone's width (412×915), against a REAL muster-server
// with MUSTER_MIRROR=1 and a REAL muster-mirror binary dialling it over the
// tunnel. Only tmux is played: the muster-mirror under test is built with
// `-tags e2emirror` (cmd/muster-mirror/e2emirror.go), whose fake terminal prints
// `FAKE-MIRROR session=<s> mode=ro|rw size=CxR` and `in=<HEX>` per input chunk.
// The token is minted by the server binary's own `mirror token create` (as the
// operator), the path an operator uses. The host name is unique per run.
//
// A mirrored session is NOT an agent: it is never in /api/agents; it is a card
// of its own on the Agents view, rendered from the hub's memory.
test.use({ viewport: { width: 412, height: 915 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

const origin = `http://localhost:${mirrorPort}`;
const host = 'e2e-' + Date.now().toString(36);
const mirrorBin = path.join(binDir, 'muster-mirror-e2e');
let server: Running | undefined;
let mirrorProc: ChildProcess | undefined;
let mirrorLog = '';
let token = '';

// startMirror runs the host binary; read-only unless interactive. The e2e
// server is plain http on localhost, so the TEST-ONLY -allow-http is set here.
// claude, when given, makes session main a Claude Code pane writing that
// transcript (the rendered view; cmd/muster-mirror/e2emirror.go).
function startMirror(interactive: boolean, claude?: string, extra?: NodeJS.ProcessEnv): ChildProcess {
  const env: NodeJS.ProcessEnv = {
    ...process.env,
    MUSTER_MIRROR_URL: origin,
    MUSTER_MIRROR_ALLOW_HTTP: '1',
    MUSTER_MIRROR_TOKEN: token,
    MUSTER_MIRROR_HOST: host,
    MUSTER_MIRROR_E2E_SESSIONS: 'main,scratch',
  };
  if (interactive) env.MUSTER_MIRROR_INTERACTIVE = '1';
  if (claude) env.MUSTER_MIRROR_E2E_CLAUDE = 'main=' + claude;
  Object.assign(env, extra);
  const p = spawn(mirrorBin, [], { env, stdio: ['ignore', 'pipe', 'pipe'] });
  p.stdout?.on('data', (d) => (mirrorLog += d));
  p.stderr?.on('data', (d) => (mirrorLog += d));
  return p;
}

async function stopMirror() {
  if (!mirrorProc || mirrorProc.exitCode !== null) return;
  const done = new Promise((r) => mirrorProc!.once('exit', r));
  mirrorProc.kill('SIGTERM');
  await done;
}

test.beforeAll(async () => {
  execFileSync('go', ['build', '-tags', 'e2emirror', '-o', mirrorBin, './cmd/muster-mirror'], { cwd: repoRoot, stdio: 'inherit' });
  server = await startServer(versionTerm, mirrorPort, {
    MUSTER_MIRROR: '1',
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
  });
  token = execFileSync(serverBinary(versionTerm), ['mirror', 'token', 'create', host], {
    env: { ...process.env, DATABASE_URL: dsn },
  })
    .toString()
    .trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
});

test.afterAll(async () => {
  await stopMirror();
  await server?.stop();
});

// login signs the page's (fresh) context in. The FORM is driven once per run of
// this file and the session cookie it earns is handed to every later test:
// password-mode sign-in is throttled to 10 attempts a minute in ONE shared
// bucket (internal/api's loginRate), and this file has more tests than that —
// a later one's form was refused (429) and it timed out on /login. None of
// these tests is about signing in.
let signedIn: Awaited<ReturnType<BrowserContext['cookies']>> | undefined;
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

async function noHorizontalScroll(page: Page) {
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw, 'document scrollWidth').toBeLessThanOrEqual(cw);
}

const screen = (page: Page) => page.locator('#term .xterm-rows');
const termPath = (session: string) => `/mirror/terminal?host=${encodeURIComponent(host)}&session=${encodeURIComponent(session)}`;
const card = (page: Page, session: string) => page.locator(`#agents-list article[data-mirror-card]:has(a[href="${termPath(session)}"])`);

// waitForCards polls the session-scoped list partial until this host's cards
// are (or are not) there, with the given online state.
async function waitForCards(page: Page, online: boolean) {
  await expect
    .poll(
      async () => {
        const html = await (await page.request.get(origin + '/ui/agents')).text();
        const has = html.includes(`Mirrored · ${host}`);
        return has && html.includes('data-mirror-offline') === !online;
      },
      { message: `this host's cards, online=${online} (muster-mirror log:\n${mirrorLog})`, timeout: 15_000 },
    )
    .toBe(true);
}

test('a read-only host’s sessions appear as Mirrored cards and open the shared terminal with no Interactive switch', async ({ page }) => {
  mirrorProc = startMirror(false);
  await login(page);
  await waitForCards(page, true);

  // Not an agent: the machine roster never lists it.
  const roster = await (await page.request.get(origin + '/api/agents', { headers: { Authorization: `Bearer ${hookToken}` } })).text();
  expect(roster).not.toContain(host);

  await page.goto(origin + '/agents');
  const main = card(page, 'main');
  await expect(main).toBeVisible();
  await expect(card(page, 'scratch')).toBeVisible();
  await expect(main.locator('[data-mirror-badge]')).toHaveAttribute('aria-label', `Mirrored · ${host}`);
  await expect(main.locator('[data-mirror-host]')).toHaveText(host);
  await expect(main.locator('[data-mirror-offline]')).toHaveCount(0);
  await expect(main.locator('button[aria-label="Agent actions"]')).toHaveCount(0);
  await noHorizontalScroll(page);
  const cb = (await main.boundingBox())!;
  expect(cb.x + cb.width, 'the card fits 412px').toBeLessThanOrEqual(412);

  await main.locator('a').first().click();
  await page.waitForURL((u) => u.pathname === '/mirror/terminal');
  await expect(screen(page)).toContainText('FAKE-MIRROR session=main mode=ro');
  await expect(page.locator('#term-status')).toHaveText('Connected');
  await expect(page.locator('[data-term-badge]')).toContainText(`Mirrored · ${host}`);
  await expect(page.locator('#term-back')).toHaveAttribute('href', '/agents');
  // The host did not advertise -interactive: no switch is offered.
  await expect(page.locator('#term-mode')).toBeHidden();
  await noHorizontalScroll(page);

  await page.locator('#term').click();
  await page.keyboard.type('zz');
  await page.waitForTimeout(300);
  await expect(screen(page)).not.toContainText('in=');

  // Asking for rw anyway is refused by muster with the plugin's own reason.
  await page.goto(origin + termPath('main'));
  await page.evaluate(() => (document.getElementById('term-mode') as HTMLButtonElement).click());
  await expect(page.locator('#term-error-text')).toContainText('this host is read-only');
  await stopMirror();
});

// PRETTY NAMES: a host with a labels file (muster-mirror -labels-file) shows a
// session it has a codename for as `<codename>:<active window>` — and the card
// FOLLOWS the active window with the session set unchanged, live, with no
// reload. The label is display-only: the card still opens the session by its
// tmux name, which stays visible beside the label. A session with no codename
// keeps its raw name.
test('a labelled session shows its pretty name, follows the active window, and still opens by its tmux name', async ({ page }) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'mirror-labels-'));
  const labelsFile = path.join(dir, 'labels.json');
  const activeFile = path.join(dir, 'active');
  fs.writeFileSync(labelsFile, JSON.stringify({ main: 'Mango' }));
  fs.writeFileSync(activeFile, 'main=1\nscratch=4\n');
  try {
    mirrorProc = startMirror(false, undefined, { MUSTER_MIRROR_LABELS_FILE: labelsFile, MUSTER_MIRROR_E2E_ACTIVE_FILE: activeFile });
    await login(page);
    await waitForCards(page, true);
    await page.goto(origin + '/agents');
    const main = card(page, 'main');
    await expect(main.locator('[data-mirror-label]')).toHaveText('Mango:1');
    await expect(main.locator('[data-mirror-session]')).toHaveText('main');
    await expect(main.locator('[data-mirror-name]')).toHaveAttribute('title', 'tmux session main');
    // No codename: the raw name, and nothing else.
    const scratch = card(page, 'scratch');
    await expect(scratch.locator('[data-mirror-name]')).toHaveText('scratch');
    await expect(scratch.locator('[data-mirror-label]')).toHaveCount(0);

    // The operator moves main to window 3; the session SET is unchanged. The
    // open page follows without a reload.
    fs.writeFileSync(activeFile, 'main=3\nscratch=4\n');
    await expect(main.locator('[data-mirror-label]'), `the card follows the window (muster-mirror log:\n${mirrorLog})`).toHaveText('Mango:3', { timeout: 20_000 });
    await noHorizontalScroll(page);

    await main.locator('a').first().click();
    await page.waitForURL((u) => u.pathname === '/mirror/terminal');
    expect(new URL(page.url()).searchParams.get('session'), 'opened by the tmux name, not the label').toBe('main');
    await expect(screen(page)).toContainText('FAKE-MIRROR session=main mode=ro');
    await expect(page.locator('header h1')).toContainText('Mango:3');
    await expect(page.locator('header h1 [data-mirror-session]')).toHaveText('main');
    await expect(page).toHaveTitle(/Mango:3/);
    await noHorizontalScroll(page);
  } finally {
    await stopMirror();
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

// MOST RECENTLY ACTIVE FIRST: a host reports when each session was last active
// (real tmux: the newest of its windows' activity; here the fake's
// MUSTER_MIRROR_E2E_ACTIVITY_FILE), and the Agents list shows the session active
// last on top — then FOLLOWS, live and with no reload, when another session
// becomes the most recent. The session set never changes.
//
// The times are whole MINUTES apart on purpose: activity is carried floored to
// a minute (mirror.ActivityBucket), and two sessions inside one minute tie and
// keep their name order — which would make "main first" pass for the wrong
// reason. `scratch` leading `main` at the start is the reverse of name order,
// so it is the ordering and nothing else.
test('mirrored sessions are listed most recently active first, and the open list follows', async ({ page }) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'mirror-activity-'));
  const activityFile = path.join(dir, 'activity');
  const t0 = Math.floor(Date.now() / 1000);
  fs.writeFileSync(activityFile, `scratch=${t0 - 120}\nmain=${t0 - 3600}\n`);
  try {
    await stopMirror();
    mirrorProc = startMirror(false, undefined, { MUSTER_MIRROR_E2E_ACTIVITY_FILE: activityFile });
    await login(page);
    await waitForCards(page, true);
    await page.goto(origin + '/agents');
    const order = () =>
      page.locator('#agents-list article[data-mirror-card]').filter({ has: page.locator(`[data-mirror-badge][aria-label="Mirrored · ${host}"]`) }).locator('[data-mirror-name]').allTextContents();
    await expect.poll(order, { message: `scratch was active last (muster-mirror log:\n${mirrorLog})`, timeout: 20_000 }).toEqual(['scratch', 'main']);

    // main's pane produces output now; scratch has not moved.
    fs.writeFileSync(activityFile, `scratch=${t0 - 120}\nmain=${Math.floor(Date.now() / 1000) - 5}\n`);
    await expect.poll(order, { message: `the open list follows main becoming the most recent (muster-mirror log:\n${mirrorLog})`, timeout: 20_000 }).toEqual(['main', 'scratch']);
    await noHorizontalScroll(page);
  } finally {
    await stopMirror();
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

// The operator report "agent cards and mirrored cards look like two different
// apps": both are laid out from the same slots, at the same insets, with no
// chat preview on either; the mirrored card's footer prints the window's last
// activity with the agent card's own component; and "Mirrored · <host>" is an
// icon whose tooltip a TAP opens (a phone has no hover) without opening the
// window. The host is -interactive and still draws no mode badge.
test('an agent card and a mirrored card share one layout: no preview, the last activity in the footer, a mirrored mark a tap explains', async ({ page }) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'mirror-cards-'));
  const activityFile = path.join(dir, 'activity');
  const t0 = Math.floor(Date.now() / 1000);
  const mainAt = t0 - 300;
  fs.writeFileSync(activityFile, `main=${mainAt}\nscratch=${t0 - 7200}\n`);
  const previews: string[] = [];
  page.on('request', (r) => {
    if (/^\/ui\/agents\/\d+\/recent$/.test(new URL(r.url()).pathname)) previews.push(r.url());
  });
  try {
    await stopMirror();
    mirrorProc = startMirror(true, undefined, { MUSTER_MIRROR_E2E_ACTIVITY_FILE: activityFile });
    await login(page);
    await waitForCards(page, true);
    await page.goto(origin + '/agents');
    const m = card(page, 'main');
    const a = page.locator('#agents-list article[id^="agent-"]').first();
    await expect(m).toBeVisible();
    await expect(a).toBeVisible();

    // Same slots, same nesting.
    const slots = (loc: typeof m) =>
      loc.evaluate((el) =>
        [...el.querySelectorAll<HTMLElement>('[data-card-slot]')].map((s) => {
          const p: string[] = [];
          for (let n: HTMLElement | null = s; n && n !== el; n = n.parentElement) if (n.dataset.cardSlot) p.unshift(n.dataset.cardSlot);
          return p.join('>');
        }),
      );
    expect(await slots(a)).toEqual(['controls', 'body', 'body>header', 'footer']);
    expect(await slots(m)).toEqual(await slots(a));
    // Same insets: the header row starts, the controls sit and the footer ends
    // at the same place on both cards.
    const geo = (loc: typeof m) =>
      loc.evaluate((el) => {
        const r = el.getBoundingClientRect();
        const g = (n: string) => el.querySelector(`[data-card-slot="${n}"]`)!.getBoundingClientRect();
        return {
          headerLeft: Math.round(g('header').left - r.left),
          headerTop: Math.round(g('header').top - r.top),
          controlsRight: Math.round(r.right - g('controls').right),
          controlsTop: Math.round(g('controls').top - r.top),
          footerBottom: Math.round(r.bottom - g('footer').bottom),
          width: Math.round(r.width),
        };
      });
    expect(await geo(m)).toEqual(await geo(a));

    // No preview on any card, and nothing fetches one.
    await expect(page.locator('#agents-list article pre')).toHaveCount(0);
    await page.waitForTimeout(500);
    expect(previews, 'card preview requests').toEqual([]);

    // The footer: the host, visible, and the window's own last activity.
    await expect(m.locator('[data-mirror-host]')).toHaveText(host);
    await expect(m.locator('[data-card-active] time')).toHaveAttribute('data-ts', String((mainAt - (mainAt % 60)) * 1000));
    await expect(m.locator('[data-card-active]')).toBeVisible();

    // The host announced -interactive; the card draws no mode badge for it.
    await expect(m).toHaveAttribute('data-f-mode', 'interactive');
    await expect(m.getByText('interactive', { exact: true })).toHaveCount(0);
    await expect(m.locator('[data-card-slot="body"]')).not.toContainText('Mirrored');

    // The mark: an icon; a TAP shows its tooltip and does not open the window.
    const mark = m.locator('[data-mirror-badge]');
    await expect(mark).toHaveAttribute('aria-label', `Mirrored · ${host}`);
    await expect(mark).toHaveAttribute('title', `Mirrored · ${host}`);
    const tip = page.locator('#' + (await mark.getAttribute('popovertarget')));
    await expect(tip).toBeHidden();
    await mark.tap();
    await expect(tip).toBeVisible();
    await expect(tip).toHaveText(`Mirrored · ${host}`);
    expect(new URL(page.url()).pathname, 'the tap did not open the window').toBe('/agents');
    const mb = (await mark.boundingBox())!;
    const tb = (await tip.boundingBox())!;
    expect(tb.y, 'the tooltip sits below its mark').toBeGreaterThanOrEqual(mb.y + mb.height - 1);
    expect(tb.x + tb.width, 'the tooltip fits the phone').toBeLessThanOrEqual(412);
    // A tap elsewhere dismisses it.
    await page.locator('#panel-agents h2').first().tap();
    await expect(tip).toBeHidden();
    await noHorizontalScroll(page);
  } finally {
    await stopMirror();
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

// The control cluster (two 44 px buttons and a gap, 12 px in) is 104 px wide,
// and the card's nav anchor reserved only 80 px for it: a long title ran under
// the mirrored mark, where a tap opens the mark's tooltip instead of the card.
// At an iPhone's 390 px, on BOTH card kinds, a title long enough to wrap must
// end left of the first control.
test('at 390 px a long title wraps before the card controls, on both card kinds', async ({ page }) => {
  const long = 'a-long-session-name-that-runs-well-past-the-width-of-a-phone-card';
  try {
    await stopMirror();
    mirrorProc = startMirror(false, undefined, { MUSTER_MIRROR_E2E_SESSIONS: `main,${long}` });
    await page.setViewportSize({ width: 390, height: 844 });
    await login(page);
    await waitForCards(page, true);
    await page.goto(origin + '/agents');
    const m = card(page, long);
    const a = page.locator('#agents-list article[id^="agent-"]').first();
    await expect(m).toBeVisible();
    await expect(a).toBeVisible();
    // Title and first control, measured in ONE evaluate. The agent's title is
    // a short seeded name, so it is lengthened in place first — what is under
    // test is the card's layout, not the data — and measured in the same task,
    // before any list re-render can replace it.
    const measure = (loc: typeof m, title: string, text?: string) =>
      loc.evaluate(
        (el, [title, text]) => {
          const t = el.querySelector<HTMLElement>(title)!;
          if (text) t.textContent = text;
          const tr = t.getBoundingClientRect();
          const c = el.querySelector<HTMLElement>('[data-card-slot="controls"] button')!.getBoundingClientRect();
          const lh = parseFloat(getComputedStyle(t).lineHeight);
          return { titleRight: tr.right, titleLines: Math.round(tr.height / lh), controlLeft: c.left, vw: window.innerWidth };
        },
        [title, text] as const,
      );
    for (const [name, got] of [
      ['mirrored', await measure(m, '[data-mirror-name]')],
      ['agent', await measure(a, '[data-card-slot="header"] > span.mr-auto', 'a-long-agent-display-name-that-runs-well-past-the-width-of-a-phone-card')],
    ] as const) {
      expect(got.vw, 'the viewport is 390 px').toBe(390);
      // Positive control: the title wrapped, so it is long enough to have
      // reached the controls had the anchor not stopped it.
      expect(got.titleLines, `${name} card: the long title wraps`).toBeGreaterThanOrEqual(2);
      expect(got.titleRight, `${name} card: the title ends left of the first control`).toBeLessThanOrEqual(got.controlLeft);
    }
    await noHorizontalScroll(page);
  } finally {
    await stopMirror();
  }
});

test('an -interactive host offers the switch and types into the session', async ({ page }) => {
  mirrorProc = startMirror(true);
  await login(page);
  await waitForCards(page, true);
  await page.goto(origin + termPath('main'));
  await expect(screen(page)).toContainText('FAKE-MIRROR session=main mode=ro');
  await expect(page.locator('#term-mode')).toBeVisible();
  await page.locator('#term-mode').click();
  await expect(screen(page)).toContainText('FAKE-MIRROR session=main mode=rw');
  await expect(page.locator('#term-status')).toHaveText('Connected · live keys');
  await page.locator('#term-keys [data-key="esc"]').click();
  await expect(screen(page)).toContainText('in=1B');
  await page.locator('#term').click();
  await page.keyboard.type('ok');
  await expect(screen(page)).toContainText(/in=6F6B|in=6F/);
  await noHorizontalScroll(page);
});

// readOnlyShown is every "read-only" a person can READ on the page: its visible
// text, and the tooltips / labels / placeholders of its elements — minus the
// text of the elements `except` names.
async function readOnlyShown(page: Page, except: string[] = []): Promise<string[]> {
  return page.evaluate((except) => {
    const re = /read[- ]only/i;
    const out: string[] = [];
    const body = document.body.cloneNode(true) as HTMLElement;
    for (const sel of except) body.querySelectorAll(sel).forEach((el) => el.remove());
    body.querySelectorAll('script, style').forEach((el) => el.remove());
    // innerText of a detached clone is its textContent; scripts are gone.
    const text = (body.textContent || '').replace(/\s+/g, ' ');
    const m = text.match(new RegExp('.{0,30}' + re.source + '.{0,30}', 'gi'));
    if (m) out.push(...m.map((s) => 'text: ' + s));
    body.querySelectorAll('[title], [aria-label], [placeholder]').forEach((el) => {
      for (const a of ['title', 'aria-label', 'placeholder']) {
        const v = el.getAttribute(a);
        if (v && re.test(v)) out.push(`${a} of #${el.id || el.tagName}: ${v}`);
      }
    });
    return out;
  }, except);
}

// 🔴 A REGRESSION TEST. The switch read "Read-only" and the status "Connected ·
// read-only" with live keys off, while the composer under the terminal sends
// without the switch: the page told a person they could not send when they
// could. Red at origin/main 5252925, green at HEAD.
test('the terminal never says read-only where its composer can send; a read-only host explains once, in its composer’s note', async ({ page }) => {
  await stopMirror();
  mirrorProc = startMirror(true);
  await login(page);
  await waitForCards(page, true);
  for (const session of ['main', 'scratch']) {
    await page.goto(origin + termPath(session));
    await expect(screen(page)).toContainText(`FAKE-MIRROR session=${session} mode=ro`);
    await expect(page.locator('#mv-input'), `${session}: the composer can send`).toBeEnabled();
    await expect(page.locator('#term-status')).toHaveText('Connected');
    await expect(page.locator('#term-mode')).toBeVisible();
    await expect(page.locator('#term-mode-label')).toHaveText('Live keys off');
    expect(await readOnlyShown(page), `${session}: nothing on a page that can send says read-only`).toEqual([]);
  }
  // Live keys on and off again: still never "read-only".
  await page.locator('#term-mode').click();
  await expect(page.locator('#term-status')).toHaveText('Connected · live keys');
  await expect(page.locator('#term-mode-label')).toHaveText('Live keys on');
  await page.locator('#term-mode').click();
  await expect(page.locator('#term-status')).toHaveText('Connected');
  await expect(page.locator('#term-mode-label')).toHaveText('Live keys off');
  expect(await readOnlyShown(page), 'after a round trip of the switch').toEqual([]);

  // A host that genuinely cannot be sent to: no switch, and the composer's own
  // disabled note is the ONE place the page says so.
  await stopMirror();
  mirrorProc = startMirror(false);
  await waitForCards(page, true);
  await page.goto(origin + termPath('scratch'));
  await expect(screen(page)).toContainText('FAKE-MIRROR session=scratch mode=ro');
  await expect(page.locator('#term-status')).toHaveText('Connected');
  await expect(page.locator('#term-mode')).toBeHidden();
  await expect(page.locator('#mv-input')).toBeDisabled();
  await expect(page.locator('#mv-readonly')).toContainText('This host is read-only');
  expect((await readOnlyShown(page)).length, 'control: the note itself is seen').toBeGreaterThan(0);
  // The composer's note, and the composer's own fields (its tooltip and placeholder carry the same words).
  expect(await readOnlyShown(page, ['#mv-readonly', '#mv-compose']), 'read-only wording outside the composer').toEqual([]);
});

test('a host that disconnects reads offline and its terminal says why', async ({ page }) => {
  if (!mirrorProc || mirrorProc.exitCode !== null) mirrorProc = startMirror(false);
  await login(page);
  await waitForCards(page, true);
  await stopMirror();
  await waitForCards(page, false);

  await page.goto(origin + '/agents');
  await expect(card(page, 'main').locator('[data-mirror-offline]')).toContainText('offline');
  await noHorizontalScroll(page);

  await page.goto(origin + termPath('main'));
  await expect(page.locator('#term-error')).toBeVisible();
  await expect(page.locator('#term-error-text')).toContainText('the host is offline');
  await noHorizontalScroll(page);

  mirrorProc = startMirror(false);
  await waitForCards(page, true);
});

// ---------------------------------------------------------------------------
// THE RENDERED VIEW (tunnel v2). The host binary is the real muster-mirror with
// main played as a Claude Code pane whose transcript is a real file THIS test
// writes, record by record, as the CLI would: the tailer, the parser, the v2
// tunnel, the hub, the socket and the page are all real.
// ---------------------------------------------------------------------------

const viewPath = (session: string) => `/mirror/view?host=${encodeURIComponent(host)}&session=${encodeURIComponent(session)}`;

function rec(o: object): string {
  return JSON.stringify(o) + '\n';
}
const prompt = (t: string) => rec({ type: 'user', promptId: 'p', message: { role: 'user', content: t } });
const reply = (t: string) => rec({ type: 'assistant', message: { content: [{ type: 'text', text: t }] } });
const thinking = (t: string) => rec({ type: 'assistant', message: { content: [{ type: 'thinking', thinking: t }] } });
const toolUse = (id: string, name: string, input: object) =>
  rec({ type: 'assistant', message: { content: [{ type: 'tool_use', id, name, input }] } });
const toolResult = (id: string, out: string, isError: boolean) =>
  rec({ type: 'user', promptId: 'p', message: { content: [{ type: 'tool_result', tool_use_id: id, content: out, is_error: isError }] } });

function newTranscript(): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'muster-e2e-cc-'));
  const p = path.join(dir, 'e2e-session-0001.jsonl');
  fs.writeFileSync(
    p,
    prompt('list the files') + thinking('PRIVATE REASONING never rendered') + reply('Listing them.') +
      toolUse('tu_ls', 'Bash', { command: 'ls -la /srv', description: 'List' }) + toolResult('tu_ls', 'alpha\nbeta', false),
  );
  return p;
}

async function waitForClaude(page: Page) {
  await expect
    .poll(async () => (await (await page.request.get(origin + '/ui/agents')).text()).includes('data-mirror-claude'), {
      message: `main reported as a Claude pane (muster-mirror log:\n${mirrorLog})`,
      timeout: 20_000,
    })
    .toBe(true);
}

const viewCard = (page: Page, session: string) => page.locator(`#agents-list article[data-mirror-card]:has(a[href="${viewPath(session)}"])`);

test('a mirrored Claude pane opens in the rendered view: collapsed tool rows, live updates, a Terminal toggle, no send on a read-only host', async ({ page }) => {
  await stopMirror();
  const tr = newTranscript();
  mirrorProc = startMirror(false, tr);
  await login(page);
  await waitForClaude(page);

  // The card for the Claude pane opens the rendered view; the other the terminal.
  await page.goto(origin + '/agents');
  await expect(viewCard(page, 'main')).toBeVisible();
  await expect(viewCard(page, 'main').locator('[data-mirror-claude]')).toContainText('Claude Code');
  await expect(card(page, 'scratch')).toBeVisible();
  await viewCard(page, 'main').locator('a').first().click();
  await page.waitForURL((u) => u.pathname === '/mirror/view');

  const log = page.locator('#mv-log');
  await expect(log.locator('[data-bubble="user"]').first()).toContainText('list the files');
  await expect(log.locator('[data-bubble="assistant"]').first()).toContainText('Listing them.');
  await expect(log).not.toContainText('PRIVATE REASONING');

  // One collapsed row per call: tool, target, status — expanding to the output.
  const ls = log.locator('[data-tool-row]').first();
  await expect(ls).toHaveAttribute('data-tool-status', 'ok');
  await expect(ls.locator('summary')).toContainText('Bash');
  await expect(ls.locator('[data-tool-target]')).toHaveText('ls -la /srv');
  await expect(ls).not.toHaveAttribute('open', '');
  await expect(ls.locator('pre').last()).toBeHidden();
  await ls.locator('summary').click();
  await expect(ls).toHaveAttribute('open', '');
  await expect(ls.locator('pre').last()).toBeVisible();
  await expect(ls.locator('pre').last()).toContainText('alpha');
  const row = (await ls.boundingBox())!;
  expect(row.x + row.width, 'the tool row fits 412px').toBeLessThanOrEqual(412);
  await noHorizontalScroll(page);

  // A turn runs: Claude Code reports busy, a call is written and runs, then fails.
  fs.writeFileSync(tr + '.busy', '');
  fs.appendFileSync(tr, prompt('now run the tests') + reply('Running the suite.') +
    toolUse('tu_test', 'Bash', { command: 'go test ./...' }));
  const testRow = log.locator('[data-tool-row]').nth(1);
  await expect(testRow.locator('[data-tool-target]')).toHaveText('go test ./...');
  await expect(testRow).toHaveAttribute('data-tool-status', 'running');
  await expect(page.locator('#mv-working')).toBeVisible({ timeout: 15_000 });
  fs.appendFileSync(tr, toolResult('tu_test', 'FAIL: TestX', true) + thinking('PRIVATE again'));
  await expect(testRow).toHaveAttribute('data-tool-status', 'error');
  await expect(log.locator('[data-tool-row]')).toHaveCount(2); // redrawn in place, not appended
  fs.rmSync(tr + '.busy');
  fs.appendFileSync(tr, reply('One test failed.'));
  await expect(log.locator('[data-bubble="assistant"]').last()).toContainText('One test failed.');
  await expect(page.locator('#mv-working')).toBeHidden({ timeout: 15_000 });
  await expect(log).not.toContainText('PRIVATE');

  // Read-only host: the composer is there and disabled, and says why.
  await expect(page.locator('#mv-input')).toBeDisabled();
  await expect(page.locator('#mv-send')).toBeDisabled();
  await expect(page.locator('#mv-readonly')).toContainText('read-only');
  await noHorizontalScroll(page);

  // The Terminal is one tap away, and back.
  await expect(page.locator('#view-rendered')).toHaveAttribute('aria-current', 'page');
  await page.locator('#view-terminal').click();
  await page.waitForURL((u) => u.pathname === '/mirror/terminal');
  await expect(screen(page)).toContainText('FAKE-MIRROR session=main mode=ro');
  await expect(page.locator('#view-terminal')).toHaveAttribute('aria-current', 'page');
  await page.locator('#view-rendered').click();
  await page.waitForURL((u) => u.pathname === '/mirror/view');
  await expect(log.locator('[data-tool-row]')).toHaveCount(2);
  await noHorizontalScroll(page);

  // A non-Claude session's view falls back to its terminal.
  await page.goto(origin + viewPath('scratch'));
  await page.waitForURL((u) => u.pathname === '/mirror/terminal');
  await stopMirror();
});

test('an interactive host offers the composer: a fresh sign-in sends into the pane, and a stale one is asked to confirm and then sent', async ({ page }) => {
  await stopMirror();
  const tr = newTranscript();
  mirrorProc = startMirror(true, tr);
  await login(page);
  await waitForClaude(page);
  await page.goto(origin + viewPath('main'));
  await expect(page.locator('#mv-log [data-tool-row]')).toHaveCount(1);
  await expect(page.locator('#mv-input')).toBeEnabled();
  const pasted = () => (fs.existsSync(tr + '.pasted') ? fs.readFileSync(tr + '.pasted', 'utf8') : '');

  // Signed in just now, so the sign-in is fresh: the send is pasted into the
  // Claude pane (its pane id, which the fake host records), the composer
  // clears, and the prompt comes back through the transcript. A leading `/` is
  // pasted as typed.
  await page.locator('#mv-input').fill('/status please continue');
  await page.locator('#mv-send').click();
  await expect(page.locator('#mv-input')).toHaveValue('');
  await expect.poll(pasted).toBe('%9 /status please continue\n');
  await expect(page.locator('#mv-log [data-bubble="user"]').last()).toContainText('/status please continue');
  await expect(page.locator('#mv-error')).toBeHidden();

  // A STALE sign-in. ⚠ The refusal itself is PLAYED here (one intercepted
  // response, shaped as the server's own: 403 + X-Muster-Step-Up) because a
  // password-mode session cannot be aged in a browser test; the server's real
  // stale refusal is pinned in Go (TestASendIsGatedOnInteractiveAndAFreshSignIn)
  // and with real GoTrue in mirror-stepup.spec.ts. Everything after it is real:
  // the dialog, the confirm route, the new session, the retried send.
  let refused = 0;
  await page.route('**/mirror/view/send?*', async (route) => {
    if (refused++ === 0) {
      await route.fulfill({ status: 403, headers: { 'X-Muster-Step-Up': 'required', 'Content-Type': 'text/plain' }, body: 'confirm it\'s you' });
      return;
    }
    await route.continue();
  });
  await page.locator('#mv-input').fill('second message');
  await page.locator('#mv-send').click();
  const dialog = page.locator('#term-confirm');
  await expect(dialog).toBeVisible();
  expect(pasted(), 'nothing is pasted while the confirm is open').toBe('%9 /status please continue\n');
  await page.fill('#term-confirm-password', 'not the password');
  await page.click('#term-confirm-submit');
  await expect(page.locator('#term-confirm-error')).toHaveText('That password is not right.');
  expect(pasted()).toBe('%9 /status please continue\n');
  await page.fill('#term-confirm-password', password);
  await page.click('#term-confirm-submit');
  await expect(dialog).toBeHidden();
  await expect.poll(pasted).toBe('%9 /status please continue\n%9 second message\n');
  await expect(page.locator('#mv-input')).toHaveValue('');
  expect(refused, 'the send was retried once after the confirm').toBe(2);
  await page.unroute('**/mirror/view/send?*');
  await noHorizontalScroll(page);
  await stopMirror();
  mirrorProc = startMirror(false);
});

// ---------------------------------------------------------------------------
// THE RENDERED VIEW'S COMPOSER STAYS ON SCREEN on a phone (operator report: on
// a real phone it was missing until Terminal → tap → Rendered).
//
// ⚠ NOT A REPRODUCTION. The suspected cause is a collapsing browser toolbar
// leaving the bottom of the page's h-dvh shell under the chrome, and Chromium
// emulation has no such toolbar (100dvh == innerHeight here). What this pins
// is the PROPERTY the fix gives — the composer is placed against the viewport,
// not at the bottom of the body — by making the body TALLER than the screen
// (the shape of that mismatch) and checking the composer is still on it. 🔴 A
// REGRESSION GUARD: red at the commit before the fix, where the composer is an
// in-flow last child (position: static) and lands below the screen.
//
// Also the Terminal composer's phone checks (below), for this view: 44px
// targets, a 16px input, nothing wider than the screen, and on screen with the
// keyboard up — both the kind that resizes the page and the kind that only
// overlays it (iOS; played with a visualViewport shorter than the window).
// ---------------------------------------------------------------------------
test('the Rendered composer stays on screen at a phone size: pinned to the viewport, above the transcript\'s end, and above the keyboard', async ({ page }) => {
  await stopMirror();
  const tr = newTranscript();
  // A conversation longer than the screen, so the transcript scrolls.
  let more = '';
  for (let i = 1; i <= 30; i++) more += prompt(`question ${i}`) + reply(`answer number ${i}`);
  fs.appendFileSync(tr, more);
  mirrorProc = startMirror(true, tr);
  const W = 412;
  const H = 915;
  const lastAnswer = () => page.locator('#mv-log [data-bubble="assistant"]').last();
  // onScreen: the composer's input and Send are inside [0, bottom] and the width.
  async function onScreen(bottom: number, why: string, soft = false) {
    const e = soft ? expect.soft : expect;
    for (const el of [page.locator('#mv-input'), page.locator('#mv-send')]) {
      const b = (await el.boundingBox())!;
      e(b.y + b.height, `${why}: the composer is above the bottom of the screen (${bottom}px)`).toBeLessThanOrEqual(bottom + 0.5);
      e(b.y, `${why}: and not pushed off the top`).toBeGreaterThanOrEqual(0);
      e(b.x + b.width, `${why}: the composer fits the width`).toBeLessThanOrEqual(W);
    }
  }
  // endAboveComposer: the transcript's last message ends above the composer
  // (nothing of it hidden behind it) and is on screen.
  async function endAboveComposer(why: string) {
    await expect(lastAnswer()).toContainText('answer number 30');
    await expect
      .poll(async () => {
        const a = (await lastAnswer().boundingBox())!;
        const c = (await page.locator('#mv-compose').boundingBox())!;
        return a.y + a.height <= c.y + 0.5 && a.y + a.height > 0;
      }, { message: `${why}: the last message ends above the composer, on screen` })
      .toBe(true);
  }

  await login(page);
  await waitForClaude(page);
  // The real path: the Agents list, scrolled (as a phone collapses its toolbar
  // on), then the card.
  await page.goto(origin + '/agents');
  await expect(viewCard(page, 'main')).toBeVisible();
  await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
  await Promise.all([page.waitForURL((u) => u.pathname === '/mirror/view'), viewCard(page, 'main').locator('a').first().tap()]);
  await expect(page.locator('#mv-input')).toBeEnabled();

  // As loaded: on screen, phone-shaped, and the transcript ends above it.
  await onScreen(H, 'as loaded');
  for (const el of [page.locator('#mv-input'), page.locator('#mv-send')]) {
    expect((await el.boundingBox())!.height, 'a 44px touch target').toBeGreaterThanOrEqual(44);
  }
  expect(await page.locator('#mv-input').evaluate((el) => parseFloat(getComputedStyle(el).fontSize)), 'a 16px input: iOS zooms the page on anything smaller').toBeGreaterThanOrEqual(16);
  await endAboveComposer('as loaded');
  await noHorizontalScroll(page);

  // 🔴 THE GUARD. A body taller than the screen — what a toolbar that sized
  // 100dvh against its other state leaves: the composer is still on screen,
  // because it is placed against the viewport. ⚠ The document is held at
  // scrollY 0, as a person finds it: overflow:hidden stops a FINGER scrolling
  // the page, not a script, and a scripted scroll here brings an in-flow
  // composer back into view (measured: that version of this check was green
  // at the pre-fix commit).
  await page.addStyleTag({ content: 'html body { height: calc(100dvh + 160px) !important; }' });
  await page.evaluate(() => window.scrollTo(0, 0));
  expect(await page.evaluate(() => window.scrollY), 'the document is not scrolled').toBe(0);
  await onScreen(H, 'with the page taller than the screen', true);
  expect
    .soft(await page.locator('#mv-compose').evaluate((el) => getComputedStyle(el).position), 'the Rendered composer is pinned to the viewport (position: fixed), not in flow at the bottom of the body')
    .toBe('fixed');
  expect(test.info().errors, 'the Rendered composer guard').toEqual([]);
  await page.evaluate(() => document.querySelectorAll('style').forEach((s) => s.textContent?.includes('160px') && s.remove()));

  // The keyboard RESIZES the page (interactive-widget=resizes-content): on
  // screen, and the transcript's end still above it.
  await page.locator('#mv-input').tap();
  await page.setViewportSize({ width: W, height: 460 });
  await onScreen(460, 'in a keyboard-height viewport');
  await endAboveComposer('in a keyboard-height viewport');
  await page.setViewportSize({ width: W, height: H });
  await onScreen(H, 'after the keyboard closed');

  // The keyboard OVERLAYS the page (iOS Safari: the window keeps its height,
  // the visual viewport shrinks): the composer rides on top of the keyboard.
  const KB = 300;
  await page.addInitScript((kb) => {
    const vv = new EventTarget();
    Object.defineProperties(vv, {
      height: { get: () => window.innerHeight - kb },
      width: { get: () => window.innerWidth },
      offsetTop: { value: 0 },
      offsetLeft: { value: 0 },
      scale: { value: 1 },
    });
    Object.defineProperty(window, 'visualViewport', { configurable: true, get: () => vv });
  }, KB);
  await page.reload();
  await expect(page.locator('#mv-input')).toBeEnabled();
  await expect.poll(async () => { const b = (await page.locator('#mv-send').boundingBox())!; return b.y + b.height; },
    { message: 'with an overlaid keyboard the composer sits above it' }).toBeLessThanOrEqual(H - KB + 0.5);
  await onScreen(H - KB, 'above an overlaid keyboard');
  await endAboveComposer('above an overlaid keyboard');
  await noHorizontalScroll(page);

  // A composer TALLER than the shell's no-script fallback reserve (4.5rem =
  // 72px): a read-only host's composer carries its note. Only the MEASURED
  // reserve (--mv-compose-h) keeps the transcript's end out from behind it —
  // the 65px interactive composer above fits inside the fallback either way.
  // A fresh page: the stubbed visualViewport above is that page's alone.
  await stopMirror();
  mirrorProc = startMirror(false, tr);
  const ro = await page.context().newPage();
  await ro.goto(origin + viewPath('main'));
  await expect(ro.locator('#mv-readonly')).toBeVisible();
  const roLast = ro.locator('#mv-log [data-bubble="assistant"]').last();
  await expect(roLast).toContainText('answer number 30');
  const roH = (await ro.locator('#mv-compose').boundingBox())!.height;
  expect(roH, 'the read-only composer is taller than the 72px fallback reserve (else this check cannot see the measurement)').toBeGreaterThan(72);
  await expect
    .poll(async () => {
      const a = (await roLast.boundingBox())!;
      const c = (await ro.locator('#mv-compose').boundingBox())!;
      return Math.round(a.y + a.height - c.y);
    }, { message: `a ${Math.round(roH)}px read-only composer: the last message ends above it (overlap px, must be <= 0)` })
    .toBeLessThanOrEqual(0);
  await ro.close();
  await stopMirror();
  mirrorProc = startMirror(false);
});

// ---------------------------------------------------------------------------
// THE TERMINAL VIEW'S COMPOSER: a message box for a session whose pane is NOT
// Claude Code (here `scratch` — a shell, opencode, anything). Such a session has
// no rendered view, so before this its card opened a Terminal with no way to
// send it a message short of flipping Interactive and typing key by key.
//
// By the real tap path at a phone's size and at a desktop's: the Agents list →
// the session's card → the Terminal → type → Send. The fake host records a
// paste into a session's active pane (`%e2e-<session>`) in
// MUSTER_MIRROR_E2E_PASTE_FILE; everything in muster and the tunnel is real.
//
// 🔴 A REGRESSION TEST: on the commit before the feature the Terminal page draws
// no composer, so the first `#mv-input` assertion fails there.
// ---------------------------------------------------------------------------
for (const size of [
  { name: 'a phone (390×844, touch)', viewport: { width: 390, height: 844 }, touch: true },
  { name: 'a desktop (1280×900)', viewport: { width: 1280, height: 900 }, touch: false },
]) {
  test.describe(`the Terminal composer on ${size.name}`, () => {
    test.use({ viewport: size.viewport, hasTouch: size.touch, isMobile: size.touch });

    test('a non-Claude session is sent a message from its Terminal view, without the Interactive switch', async ({ page }) => {
      await stopMirror();
      const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'muster-e2e-panes-'));
      const pasteFile = path.join(dir, 'pasted');
      const tr = newTranscript();
      const pasted = () => (fs.existsSync(pasteFile) ? fs.readFileSync(pasteFile, 'utf8') : '');
      mirrorProc = startMirror(true, tr, { MUSTER_MIRROR_E2E_PASTE_FILE: pasteFile });
      const rw: string[] = [];
      const sends: number[] = [];
      page.on('websocket', (ws) => {
        if (ws.url().includes('/mirror/terminal/ws') && ws.url().includes('mode=rw')) rw.push(ws.url());
      });
      page.on('response', (r) => {
        if (new URL(r.url()).pathname === '/mirror/terminal/send') sends.push(r.status());
      });
      const act = (l: ReturnType<Page['locator']>) => (size.touch ? l.tap() : l.click());
      try {
        await login(page);
        await waitForClaude(page);

        // The card of a session with no Claude pane opens its TERMINAL.
        await page.goto(origin + '/agents');
        const scratch = card(page, 'scratch');
        await expect(scratch).toBeVisible();
        await expect(scratch.locator('[data-mirror-claude]')).toHaveCount(0);
        await Promise.all([page.waitForURL((u) => u.pathname === '/mirror/terminal'), act(scratch.locator('a').first())]);
        await expect(screen(page)).toContainText('FAKE-MIRROR session=scratch mode=ro');
        await expect(page.locator('#view-rendered'), 'scratch has no rendered view').toHaveCount(0);

        // The composer is THERE, enabled, on screen, phone-shaped — and the
        // terminal is still read-only.
        const input = page.locator('#mv-input');
        const send = page.locator('#mv-send');
        await expect(input).toBeEnabled();
        await expect(send).toBeEnabled();
        await expect(input).toHaveAttribute('placeholder', 'Message…');
        await expect(page.locator('#term-mode')).toHaveAttribute('aria-checked', 'false');
        for (const el of [input, send]) {
          const b = (await el.boundingBox())!;
          expect(b.y + b.height, 'the composer is above the bottom of the screen').toBeLessThanOrEqual(size.viewport.height);
          expect(b.x + b.width, 'the composer fits the width').toBeLessThanOrEqual(size.viewport.width);
          expect(b.height, 'a 44px touch target').toBeGreaterThanOrEqual(44);
        }
        expect(await input.evaluate((el) => parseFloat(getComputedStyle(el).fontSize)), 'a 16px input: iOS zooms the page on anything smaller').toBeGreaterThanOrEqual(16);
        const keys = (await page.locator('#term-keys').boundingBox())!;
        expect((await input.boundingBox())!.y, 'the composer is the LAST thing on the page, under the quick keys').toBeGreaterThanOrEqual(keys.y + keys.height - 1);
        await noHorizontalScroll(page);

        // Typed, sent, pasted into THAT session's active pane; the box clears.
        await act(input);
        await page.keyboard.type('git status');
        await act(send);
        await expect.poll(pasted).toBe('%e2e-scratch git status\n');
        await expect(input).toHaveValue('');
        await expect(page.locator('#mv-note')).toBeHidden();
        expect(sends, 'one send, accepted').toEqual([204]);
        expect(rw, 'no interactive terminal was opened to do it').toEqual([]);
        await expect(page.locator('#term-mode')).toHaveAttribute('aria-checked', 'false');
        await expect(page.locator('#term-confirm')).toBeHidden();

        // Enter sends too; Shift+Enter is a newline, and several lines go as ONE
        // message (one paste).
        await act(input);
        await page.keyboard.type('line one');
        await page.keyboard.press('Shift+Enter');
        await page.keyboard.type('line two');
        await page.keyboard.press('Enter');
        await expect.poll(pasted).toBe('%e2e-scratch git status\n%e2e-scratch line one\nline two\n');
        await expect(input).toHaveValue('');

        // An EMPTY Send says so, inline, and sends nothing.
        await act(send);
        await expect(page.locator('#mv-note')).toHaveText('Type a message first.');
        await input.fill('   ');
        await act(send);
        await expect(page.locator('#mv-note')).toHaveText('Type a message first.');
        expect(sends, 'an empty Send never reached the server').toEqual([204, 204]);
        await input.fill('x');
        await expect(page.locator('#mv-note'), 'typing clears the note').toBeHidden();
        await input.fill('');

        // With the on-screen keyboard up the page is shorter (the viewport meta
        // is interactive-widget=resizes-content): the composer stays on screen.
        if (size.touch) {
          await page.setViewportSize({ width: size.viewport.width, height: 460 });
          for (const el of [input, send]) {
            const b = (await el.boundingBox())!;
            expect(b.y + b.height, 'the composer is still on screen in a keyboard-height viewport').toBeLessThanOrEqual(460);
            expect(b.y, 'and not pushed off the top').toBeGreaterThanOrEqual(0);
          }
          await page.setViewportSize(size.viewport);
        }

        // A FAILURE is shown inline with its reason, and the text is KEPT. (The
        // host goes away between the page load and the Send.)
        await stopMirror();
        await waitForCards(page, false);
        await input.fill('this one cannot be delivered');
        await act(send);
        await expect(page.locator('#mv-note')).toContainText('offline');
        await expect(input).toHaveValue('this one cannot be delivered');
        expect(sends[sends.length - 1], 'the refusal was the server\'s').toBe(503);
        expect(pasted(), 'nothing more was pasted').toBe('%e2e-scratch git status\n%e2e-scratch line one\nline two\n');
        await noHorizontalScroll(page);
      } finally {
        await stopMirror();
        fs.rmSync(dir, { recursive: true, force: true });
        mirrorProc = startMirror(false);
      }
    });
  });
}

test('a read-only host’s Terminal view draws its composer disabled and says why, and the rendered view’s empty Send says so', async ({ page }) => {
  await stopMirror();
  const tr = newTranscript();
  // Read-only: the Terminal page of either kind of session has the message
  // box, disabled, with the reason under it (it used to have none at all).
  mirrorProc = startMirror(false, tr);
  await login(page);
  await waitForClaude(page);
  for (const session of ['scratch', 'main']) {
    await page.goto(origin + termPath(session));
    await expect(screen(page)).toContainText(`FAKE-MIRROR session=${session} mode=ro`);
    await expect(page.locator('#mv-input'), `${session}: a read-only host’s composer is disabled`).toBeDisabled();
    await expect(page.locator('#mv-send')).toBeDisabled();
    await expect(page.locator('#mv-readonly')).toContainText('This host is read-only');
    await expect(page.locator('#term-keys'), 'control: the rest of the page is there').toBeVisible();
  }

  // Interactive: the SAME composer on the rendered view answers an empty Send
  // (it used to do nothing at all, silently).
  await stopMirror();
  mirrorProc = startMirror(true, tr);
  await waitForClaude(page);
  await page.goto(origin + viewPath('main'));
  await expect(page.locator('#mv-input')).toBeEnabled();
  const sends: number[] = [];
  page.on('response', (r) => {
    if (new URL(r.url()).pathname === '/mirror/view/send') sends.push(r.status());
  });
  await page.locator('#mv-send').click();
  await expect(page.locator('#mv-note')).toHaveText('Type a message first.');
  expect(sends, 'nothing was sent').toEqual([]);
  expect(await page.locator('#mv-input').evaluate((el) => parseFloat(getComputedStyle(el).fontSize)), 'the rendered view’s input is 16px too').toBeGreaterThanOrEqual(16);
  await noHorizontalScroll(page);
  await stopMirror();
  mirrorProc = startMirror(false);
});

// ---------------------------------------------------------------------------
// DRAG-TO-SCROLL: a vertical drag on the Terminal scrolls the session's tmux
// HISTORY. The viewer's tmux client paints only the visible screen, so the
// browser holds none; the host scrolls the pane in tmux copy-mode and the view
// arrives on the socket. The fake host plays copy-mode and records every scroll
// in MUSTER_MIRROR_E2E_SCROLL_FILE (`<pane> <lines> pos=<lines back>`); unset,
// it is an OLDER muster-mirror that cannot scroll.
//
// 🔴 REAL TOUCH: the drags are Chrome touch events (../lib/touch.ts), which go
// through the browser's own gesture handling — touch-action, native panning of
// #term-scroll — exactly as a finger does.
// ---------------------------------------------------------------------------

// scrollLog is the fake host's record: each scroll's lines and the position
// after it, and the cancels.
function scrollLog(file: string) {
  const lines = fs.existsSync(file) ? fs.readFileSync(file, 'utf8').trim().split('\n').filter(Boolean) : [];
  const scrolls = lines.filter((l) => !l.endsWith(' cancel')).map((l) => {
    const [pane, n, pos] = l.split(' ');
    return { pane, lines: Number(n), pos: Number(pos.slice('pos='.length)) };
  });
  return { scrolls, cancels: lines.filter((l) => l.endsWith(' cancel')) };
}

// reportScrollTiming prints (and attaches to the test) the scroll requests'
// round trips and the most requests the page sent in any one second.
function reportScrollTiming(rtts: number[], sentAt: number[]) {
  const s = [...rtts].sort((a, b) => a - b);
  const q = (p: number) => (s.length ? s[Math.min(s.length - 1, Math.floor(p * s.length))].toFixed(1) : 'n/a');
  let peak = 0;
  for (let i = 0, j = 0; i < sentAt.length; i++) {
    while (sentAt[i] - sentAt[j] >= 1000) j++;
    peak = Math.max(peak, i - j + 1);
  }
  const line = `scroll round trip over ${s.length} requests: min ${q(0)} ms, median ${q(0.5)} ms, p95 ${q(0.95)} ms, max ${q(1)} ms; peak ${peak} requests in one second`;
  console.log(line);
  test.info().annotations.push({ type: 'scroll-timing', description: line });
}

test.describe('drag-to-scroll on a phone (412×915, touch)', () => {
  test('a vertical drag scrolls tmux history through the host’s copy-mode, a sideways one still pans, and an older host has no gesture', async ({ page }) => {
    await stopMirror();
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'muster-e2e-scroll-'));
    const scrollFile = path.join(dir, 'scrolls');
    // 200 columns: wider than the phone, so #term-scroll has something to pan
    // sideways. 20 rows fit its height, so a vertical drag is never the area's.
    mirrorProc = startMirror(true, undefined, { MUSTER_MIRROR_E2E_SCROLL_FILE: scrollFile, MUSTER_MIRROR_E2E_WINDOW: '200x20' });
    const posts: number[] = [];
    const sentAt: number[] = [];
    page.on('request', (r) => {
      if (new URL(r.url()).pathname === '/mirror/terminal/scroll') sentAt.push(Date.now());
    });
    page.on('response', (r) => {
      if (new URL(r.url()).pathname !== '/mirror/terminal/scroll') return;
      posts.push(r.status());
    });
    try {
      await login(page);
      await waitForCards(page, true);
      await page.goto(origin + termPath('scratch'));
      await expect(screen(page)).toContainText('FAKE-MIRROR session=scratch mode=ro');
      const term = page.locator('#term');
      await expect(term, 'the gesture is wired for a host that can scroll').toHaveAttribute('data-scroll-on', '1');
      await expect(term, 'fixture: the far side is wider than the phone').toHaveAttribute('data-overflow', '1');
      const box = (await page.locator('#term-scroll').boundingBox())!;
      const mid = { x: box.x + box.width / 2, y: box.y + box.height / 2 };
      const left0 = await page.locator('#term-scroll').evaluate((el) => el.scrollLeft);

      // Finger DOWN: back into history (tmux scroll-up), in whole lines.
      await touchDrag(page, { x: mid.x, y: mid.y - 120 }, { x: mid.x, y: mid.y + 120 });
      await expect.poll(() => scrollLog(scrollFile).scrolls.reduce((a, s) => a + s.lines, 0), { message: `a drag down scrolled back (muster-mirror log:\n${mirrorLog})` }).toBeGreaterThan(5);
      const down = scrollLog(scrollFile).scrolls;
      expect(down.every((s) => s.pane === '%e2e-scratch' && s.lines > 0), `every scroll was back, into scratch's pane: ${JSON.stringify(down)}`).toBe(true);
      const back = down[down.length - 1].pos;
      // The page's own count agrees with the host's: nothing dropped.
      await expect(term).toHaveAttribute('data-scroll-sent', String(back));
      expect(posts.every((s) => s === 204), `every scroll answered 204: ${posts}`).toBe(true);
      expect(await page.locator('#term-scroll').evaluate((el) => el.scrollLeft), 'a vertical drag did not pan sideways').toBe(left0);
      // Live keys stayed off and nothing was typed into the session.
      await expect(page.locator('#term-mode')).toHaveAttribute('aria-checked', 'false');
      await expect(screen(page)).not.toContainText('in=');

      // Finger UP past where it started: forward again, out of copy-mode at
      // the bottom (the fake's -e).
      await touchDrag(page, { x: mid.x, y: mid.y + 150 }, { x: mid.x, y: mid.y - 150 });
      await expect.poll(() => { const s = scrollLog(scrollFile).scrolls; return s[s.length - 1].pos; }, { message: 'a drag up scrolled back to the bottom' }).toBe(0);
      expect(scrollLog(scrollFile).scrolls.slice(down.length).every((s) => s.lines < 0), 'the drag up only scrolled forward').toBe(true);

      // SIDEWAYS: the scroll area pans as it always did, and no scroll is sent.
      // Every scroll the host recorded has had its answer (a late one would
      // otherwise land in the sideways count below).
      // Both sides are re-read on every poll: a scroll still landing moves both.
      await expect.poll(() => posts.length - scrollLog(scrollFile).scrolls.length, { message: 'every scroll answered' }).toBe(0);
      // THE MEASURED ROUND TRIP and the page's cadence through this real stack
      // (browser → muster-server → tunnel → muster-mirror → the fake's
      // copy-mode), for the rate limit's headroom. Reported, not asserted: a
      // number about this host, not a contract.
      // The browser's own Resource Timing (fetch start to response end), not
      // Playwright's event times. It must have seen every scroll posted.
      const rtts = await page.evaluate(() => performance.getEntriesByType('resource')
        .filter((e) => new URL(e.name).pathname === '/mirror/terminal/scroll').map((e) => e.duration));
      expect(rtts.length, 'the round-trip instrument saw every scroll').toBe(posts.length);
      reportScrollTiming(rtts, sentAt);
      await expect(page.locator('#term-scroll-note'), 'nothing failed, so no note').toBeHidden();
      const before = scrollLog(scrollFile).scrolls.length;
      const sent = posts.length;
      await touchDrag(page, { x: mid.x + 120, y: mid.y }, { x: mid.x - 120, y: mid.y + 40 });
      await expect.poll(() => page.locator('#term-scroll').evaluate((el) => el.scrollLeft), { message: 'a sideways drag pans #term-scroll' }).toBeGreaterThan(left0 + 40);
      await page.waitForTimeout(300);
      expect(scrollLog(scrollFile).scrolls.length, 'a sideways drag scrolled no history').toBe(before);
      expect(posts.length, 'a sideways drag posted nothing').toBe(sent);

      // An OLDER muster-mirror: no gesture, no note, and the area still pans.
      await stopMirror();
      mirrorProc = startMirror(true, undefined, { MUSTER_MIRROR_E2E_WINDOW: '200x20' });
      await waitForCards(page, true);
      await page.goto(origin + termPath('scratch'));
      await expect(screen(page)).toContainText('FAKE-MIRROR session=scratch mode=ro');
      await expect(term).not.toHaveAttribute('data-scroll', /.*/);
      await expect(term).not.toHaveAttribute('data-scroll-on', /.*/);
      const postsBefore = posts.length;
      const l1 = await page.locator('#term-scroll').evaluate((el) => el.scrollLeft);
      await touchDrag(page, { x: mid.x, y: mid.y - 120 }, { x: mid.x, y: mid.y + 120 });
      await touchDrag(page, { x: mid.x + 120, y: mid.y }, { x: mid.x - 120, y: mid.y + 40 });
      await expect.poll(() => page.locator('#term-scroll').evaluate((el) => el.scrollLeft), { message: 'an older host’s page still pans sideways' }).toBeGreaterThan(l1 + 40);
      await page.waitForTimeout(300);
      expect(posts.length, 'an older host’s page posted no scroll').toBe(postsBefore);
      expect(await readOnlyShown(page), 'and no note about it').toEqual([]);
      await expect(page.locator('#term-scroll-note'), 'an older host’s page has no scroll note').toHaveCount(0);
      await noHorizontalScroll(page);
    } finally {
      await stopMirror();
      fs.rmSync(dir, { recursive: true, force: true });
      mirrorProc = startMirror(false);
    }
  });
});

// A SCROLL THAT DID NOT LAND IS PUT BACK. page.route plays the failures (a 429,
// a network failure, a 504, then a host that keeps refusing); everything else
// is the real stack. `asked` is the page's own count of the lines its gestures
// asked for, `sent` of those that landed, and the fake host's log is what
// arrived.
test.describe('drag-to-scroll when scrolls fail (412×915, touch)', () => {
  test('a scroll that did not land is sent again, and one that keeps failing gives up with a note', async ({ page }) => {
    await stopMirror();
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'muster-e2e-scrollfail-'));
    const scrollFile = path.join(dir, 'scrolls');
    mirrorProc = startMirror(true, undefined, { MUSTER_MIRROR_E2E_SCROLL_FILE: scrollFile, MUSTER_MIRROR_E2E_WINDOW: '200x20' });
    // Each scroll request takes the next fault in `plan`, or `always` once the
    // plan is spent; '' passes it through to the real server.
    let plan: string[] = [];
    let always = '';
    let attempts = 0;
    let faulted = 0;
    const hostLines = () => scrollLog(scrollFile).scrolls.reduce((a, s) => a + s.lines, 0);
    const attr = async (name: string) => Number((await page.locator('#term').getAttribute(name)) ?? '0');
    try {
      await login(page);
      await waitForCards(page, true);
      await page.route('**/mirror/terminal/scroll', async (route) => {
        attempts++;
        const f = plan.length ? plan.shift()! : always;
        if (f) faulted++;
        if (f === 'abort') return route.abort('failed');
        if (f === '429') return route.fulfill({ status: 429, headers: { 'Retry-After': '1' }, body: 'scrolling too fast' });
        if (f) return route.fulfill({ status: Number(f), body: 'fault' });
        return route.continue();
      });
      await page.goto(origin + termPath('scratch'));
      await expect(screen(page)).toContainText('FAKE-MIRROR session=scratch mode=ro');
      const term = page.locator('#term');
      await expect(term).toHaveAttribute('data-scroll-on', '1');
      const note = page.locator('#term-scroll-note');
      await expect(note).toBeHidden();
      const box = (await page.locator('#term-scroll').boundingBox())!;
      const mid = { x: box.x + box.width / 2, y: box.y + box.height / 2 };

      // 1. Three failures, then the real host: nothing the drag asked for is lost.
      plan = ['429', 'abort', '504'];
      await touchDrag(page, { x: mid.x, y: mid.y - 120 }, { x: mid.x, y: mid.y + 120 });
      // Both sides re-read on every poll.
      await expect.poll(async () => {
        const asked = await attr('data-scroll-asked');
        return `host-asked=${hostLines() - asked} sent-asked=${(await attr('data-scroll-sent')) - asked}`;
      }, { message: `every line asked for arrived (muster-mirror log:\n${mirrorLog})` }).toBe('host-asked=0 sent-asked=0');
      expect(faulted, 'fixture: the three faults were served').toBe(3);
      const asked1 = await attr('data-scroll-asked');
      expect(asked1, 'fixture: the drag asked for some history').toBeGreaterThan(5);
      expect(hostLines(), 'after the retries the host has every line').toBe(asked1);
      await expect(note, 'retries that succeeded leave no note').toBeHidden();

      // 2. A host that keeps failing: a bounded number of tries, then the note.
      always = '503';
      const tries0 = attempts;
      const host0 = hostLines();
      await touchDrag(page, { x: mid.x, y: mid.y - 60 }, { x: mid.x, y: mid.y + 60 });
      await expect(note, 'the page gave up and says so').toBeVisible();
      await expect(note).toHaveText('Scrolling is not reaching the host. Drag again to retry.');
      await page.waitForTimeout(1500);
      expect(attempts - tries0, 'tries before giving up').toBe(5);
      expect(hostLines(), 'nothing reached the host').toBe(host0);

      // 3. The host recovers: the next drag lands and the note goes.
      always = '';
      await touchDrag(page, { x: mid.x, y: mid.y + 120 }, { x: mid.x, y: mid.y - 120 });
      await expect(note, 'a scroll that lands clears the note').toBeHidden();
      await expect.poll(async () => hostLines() - (await attr('data-scroll-sent')), { message: 'the page and the host agree again' }).toBe(0);
      expect(hostLines(), 'the recovered drag scrolled forward').toBeLessThan(host0);
    } finally {
      await page.unroute('**/mirror/terminal/scroll');
      await stopMirror();
      fs.rmSync(dir, { recursive: true, force: true });
      mirrorProc = startMirror(false);
    }
  });
});

// A REFUSAL WITH A REASON KEEPS THE GESTURE. Only a host that cannot scroll at
// all ("unsupported") unwires it; a host that is reconnecting, a pane in a
// tmux menu and a window that is gone each show their note, keep the gesture
// wired, and clear once a scroll lands. Each refusal is played twice: as the
// route answers it (page.route, with the code header the page branches on)
// and — mode and offline — by the real stack (the fake host's `.mode` switch;
// the host actually stopped).
test.describe('drag-to-scroll when the host refuses (412×915, touch)', () => {
  test('a pane in another mode or a reconnecting host shows a note and keeps the gesture; only a host that cannot scroll unwires it, and Reconnect wires it again', async ({ page }) => {
    await stopMirror();
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'muster-e2e-scrollrefuse-'));
    const scrollFile = path.join(dir, 'scrolls');
    const env = { MUSTER_MIRROR_E2E_SCROLL_FILE: scrollFile, MUSTER_MIRROR_E2E_WINDOW: '200x20' };
    mirrorProc = startMirror(true, undefined, env);
    // Every scroll request is answered with `fault` while it is set ('' = the
    // real server).
    let fault: { status: number; code: string } | null = null;
    let posts = 0;
    const hostLines = () => scrollLog(scrollFile).scrolls.reduce((a, s) => a + s.lines, 0);
    try {
      await login(page);
      await waitForCards(page, true);
      await page.route('**/mirror/terminal/scroll', async (route) => {
        posts++;
        if (fault) return route.fulfill({ status: fault.status, headers: { 'X-Muster-Scroll-Refusal': fault.code }, body: 'refused' });
        return route.continue();
      });
      await page.goto(origin + termPath('scratch'));
      await expect(screen(page)).toContainText('FAKE-MIRROR session=scratch mode=ro');
      const term = page.locator('#term');
      const note = page.locator('#term-scroll-note');
      await expect(term).toHaveAttribute('data-scroll-on', '1');
      const box = (await page.locator('#term-scroll').boundingBox())!;
      const mid = { x: box.x + box.width / 2, y: box.y + box.height / 2 };
      const down = () => touchDrag(page, { x: mid.x, y: mid.y - 80 }, { x: mid.x, y: mid.y + 80 });
      // lands: a drag reaches the host, and any note goes.
      const lands = async (what: string) => {
        const before = hostLines();
        await down();
        await expect.poll(hostLines, { message: `${what}: a later drag scrolled (muster-mirror log:\n${mirrorLog})` }).toBeGreaterThan(before);
        await expect(note, `${what}: a scroll that lands clears the note`).toBeHidden();
        await expect(term, `${what}: the gesture is wired`).toHaveAttribute('data-scroll-on', '1');
      };
      // refusedWith: a drag while the route refuses shows `text`, keeps the
      // gesture, and reaches nothing.
      const refusedWith = async (what: string, text: string) => {
        const before = hostLines();
        const p0 = posts;
        await down();
        await expect(note, `${what}: the note`).toBeVisible();
        await expect(note).toHaveText(text);
        expect(posts, `${what}: fixture: the drag posted`).toBeGreaterThan(p0);
        await expect(term, `${what}: the gesture is still wired`).toHaveAttribute('data-scroll-on', '1');
        expect(hostLines(), `${what}: nothing reached the host`).toBe(before);
      };
      await lands('control');

      // 1. Each refusal as the route answers it.
      for (const c of [
        { status: 409, code: 'mode', text: 'The pane is in another mode (e.g. a tmux menu). Close it, then drag again.' },
        { status: 503, code: 'offline', text: 'Host reconnecting. Drag again once it is back.' },
        { status: 404, code: 'gone', text: 'This window is no longer on the host.' },
        { status: 409, code: 'refused', text: 'The host refused the scroll. Drag again to retry.' },
      ]) {
        fault = { status: c.status, code: c.code };
        await refusedWith(`${c.status} ${c.code}`, c.text);
        fault = null;
        await lands(`after ${c.status} ${c.code}`);
      }

      // 2. A pane in another mode, through the real stack: the host refuses it
      // as one, and muster says so.
      fs.writeFileSync(scrollFile + '.mode', '');
      await refusedWith('a real pane in another mode', 'The pane is in another mode (e.g. a tmux menu). Close it, then drag again.');
      fs.rmSync(scrollFile + '.mode');
      await lands('after the real mode closed');

      // 3. A host that is really gone for a while: "reconnecting", the gesture
      // kept; back again, Reconnect re-attaches and a drag lands.
      await stopMirror();
      await waitForCards(page, false);
      await refusedWith('a host that went offline', 'Host reconnecting. Drag again once it is back.');
      mirrorProc = startMirror(true, undefined, env);
      await waitForCards(page, true);
      await page.locator('#term-reconnect').click();
      await expect(screen(page)).toContainText('FAKE-MIRROR session=scratch mode=ro');
      await lands('after the host came back');

      // 4. ONLY "this host cannot scroll" unwires the gesture; a drag then posts
      // nothing. Reconnect (after a dropped connection) wires it again.
      fault = { status: 409, code: 'unsupported' };
      await down();
      await expect(term, 'a host that cannot scroll unwires the gesture').not.toHaveAttribute('data-scroll-on', /.*/);
      await expect(note, 'and shows no note').toBeHidden();
      const p1 = posts;
      await down();
      await page.waitForTimeout(300);
      expect(posts, 'an unwired gesture posts nothing').toBe(p1);
      fault = null;
      await stopMirror();
      await expect(page.locator('#term-reconnect')).toBeVisible();
      mirrorProc = startMirror(true, undefined, env);
      await waitForCards(page, true);
      await page.locator('#term-reconnect').click();
      await expect(term, 'Reconnect wires the gesture again').toHaveAttribute('data-scroll-on', '1');
      await lands('after Reconnect');
    } finally {
      await page.unroute('**/mirror/terminal/scroll');
      await stopMirror();
      fs.rmSync(dir, { recursive: true, force: true });
      mirrorProc = startMirror(false);
    }
  });
});

test.describe('drag-to-scroll on a desktop (1280×900, mouse)', () => {
  test.use({ viewport: { width: 1280, height: 900 }, hasTouch: false, isMobile: false });

  test('the mouse wheel scrolls tmux history the same way', async ({ page }) => {
    await stopMirror();
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'muster-e2e-wheel-'));
    const scrollFile = path.join(dir, 'scrolls');
    mirrorProc = startMirror(true, undefined, { MUSTER_MIRROR_E2E_SCROLL_FILE: scrollFile });
    try {
      await login(page);
      await waitForCards(page, true);
      await page.goto(origin + termPath('main'));
      await expect(screen(page)).toContainText('FAKE-MIRROR session=main mode=ro');
      await expect(page.locator('#term')).toHaveAttribute('data-scroll-on', '1');
      const box = (await page.locator('#term-scroll').boundingBox())!;
      await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
      // Wheel UP is back.
      for (let i = 0; i < 4; i++) await page.mouse.wheel(0, -120);
      await expect.poll(() => scrollLog(scrollFile).scrolls.reduce((a, s) => a + s.lines, 0), { message: 'the wheel scrolled back' }).toBeGreaterThan(3);
      expect(scrollLog(scrollFile).scrolls.every((s) => s.pane === '%e2e-main' && s.lines > 0)).toBe(true);
      // Wheel DOWN, further than it went up: back at the bottom.
      for (let i = 0; i < 8; i++) await page.mouse.wheel(0, 120);
      await expect.poll(() => { const s = scrollLog(scrollFile).scrolls; return s[s.length - 1].pos; }, { message: 'the wheel scrolled back to the bottom' }).toBe(0);
    } finally {
      await stopMirror();
      fs.rmSync(dir, { recursive: true, force: true });
      mirrorProc = startMirror(false);
    }
  });
});
