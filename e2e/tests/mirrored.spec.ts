import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
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
// The e2eterm build's step-up gate is FRESH while this file exists
// (cmd/muster-server/e2estepup.go).
const stepUpFile = path.join(os.tmpdir(), `muster-e2e-stepup-${process.pid}`);

// startMirror runs the host binary; read-only unless interactive. The e2e
// server is plain http on localhost, so the TEST-ONLY -allow-http is set here.
// claude, when given, makes session main a Claude Code pane writing that
// transcript (the rendered view; cmd/muster-mirror/e2emirror.go).
function startMirror(interactive: boolean, claude?: string): ChildProcess {
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
    MUSTER_E2E_STEPUP_FRESH_FILE: stepUpFile,
  });
  fs.rmSync(stepUpFile, { force: true });
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

test('an interactive host offers the composer; a send is refused until the step-up is fresh, then pasted into the pane', async ({ page }) => {
  await stopMirror();
  const tr = newTranscript();
  mirrorProc = startMirror(true, tr);
  await login(page);
  await waitForClaude(page);
  await page.goto(origin + viewPath('main'));
  await expect(page.locator('#mv-log [data-tool-row]')).toHaveCount(1);
  await expect(page.locator('#mv-input')).toBeEnabled();
  await page.locator('#mv-input').fill('please continue');
  await page.locator('#mv-send').click();
  // 🔴 The step-up is not fresh: muster refuses, and nothing reaches the host.
  await expect(page.locator('#mv-error-text')).toContainText('step-up required');
  await page.waitForTimeout(500);
  expect(fs.existsSync(tr + '.pasted'), 'the host was asked to paste').toBe(false);
  await expect(page.locator('#mv-input')).toHaveValue('please continue');

  // Fresh step-up: the same send is pasted into the Claude pane (its pane id,
  // the fake host records), the composer clears, and the prompt arrives back
  // through the transcript — the view shows what the CLI received.
  fs.writeFileSync(stepUpFile, '');
  try {
    await page.locator('#mv-send').click();
    await expect(page.locator('#mv-input')).toHaveValue('');
    await expect(page.locator('#mv-error')).toBeHidden();
    await expect.poll(() => (fs.existsSync(tr + '.pasted') ? fs.readFileSync(tr + '.pasted', 'utf8') : '')).toBe('%9 please continue\n');
    await expect(page.locator('#mv-log [data-bubble="user"]').last()).toContainText('please continue');
  } finally {
    fs.rmSync(stepUpFile, { force: true });
  }
  await noHorizontalScroll(page);
  await stopMirror();
  mirrorProc = startMirror(false);
});
