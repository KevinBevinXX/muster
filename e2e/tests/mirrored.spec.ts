import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import path from 'node:path';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { binDir, dsn, hookToken, mirrorPort, password, repoRoot, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';

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
function startMirror(interactive: boolean): ChildProcess {
  const env: NodeJS.ProcessEnv = {
    ...process.env,
    MUSTER_MIRROR_URL: origin,
    MUSTER_MIRROR_ALLOW_HTTP: '1',
    MUSTER_MIRROR_TOKEN: token,
    MUSTER_MIRROR_HOST: host,
    MUSTER_MIRROR_E2E_SESSIONS: 'main,scratch',
  };
  if (interactive) env.MUSTER_MIRROR_INTERACTIVE = '1';
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

async function login(page: Page) {
  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
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
  await expect(main.locator('[data-mirror-badge]')).toContainText(`Mirrored · ${host}`);
  await expect(main.locator('[data-mirror-offline]')).toHaveCount(0);
  await expect(main.locator('button[aria-label="Agent actions"]')).toHaveCount(0);
  await noHorizontalScroll(page);
  const cb = (await main.boundingBox())!;
  expect(cb.x + cb.width, 'the card fits 412px').toBeLessThanOrEqual(412);

  await main.locator('a').first().click();
  await page.waitForURL((u) => u.pathname === '/mirror/terminal');
  await expect(screen(page)).toContainText('FAKE-MIRROR session=main mode=ro');
  await expect(page.locator('#term-status')).toHaveText('Connected · read-only');
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

test('an -interactive host offers the switch and types into the session', async ({ page }) => {
  mirrorProc = startMirror(true);
  await login(page);
  await waitForCards(page, true);
  await page.goto(origin + termPath('main'));
  await expect(screen(page)).toContainText('FAKE-MIRROR session=main mode=ro');
  await expect(page.locator('#term-mode')).toBeVisible();
  await page.locator('#term-mode').click();
  await expect(screen(page)).toContainText('FAKE-MIRROR session=main mode=rw');
  await expect(page.locator('#term-status')).toHaveText('Connected · interactive');
  await page.locator('#term-keys [data-key="esc"]').click();
  await expect(screen(page)).toContainText('in=1B');
  await page.locator('#term').click();
  await page.keyboard.type('ok');
  await expect(screen(page)).toContainText(/in=6F6B|in=6F/);
  await noHorizontalScroll(page);
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
