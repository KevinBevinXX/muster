import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import crypto from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { binDir, docsBrowserPort, docsGotruePort, docsMainPort, dsn, e2eRoot, gotrueSecret, repoRoot, versionA } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { authenticator } from '../lib/webauthn';

// THE DOCS SCREENSHOTS (docs/img/*.png), generated rather than hand-made, so
// they can be re-taken after any UI change and can never show real data.
// `make screenshots` runs this file alone (MUSTER_E2E_DOCSHOTS=1); it is never
// part of the suite or of CI.
//
// 🔴 FIXTURE DATA ONLY. The board is e2e/seed's (global-setup refuses any
// database not named *_e2e, and so does the check below). The mirrored host is
// `muster-mirror -tags e2emirror` — real tunnel, FAKE tmux — whose windows and
// Claude Code transcript are written by this file. The account is a throwaway
// GoTrue user in a throwaway container. Every name below is invented.
//
// One gotrue-mode server (so the step-up dialog can offer a PASSKEY, from
// Chromium's virtual authenticator against the real GoTrue image), with the
// mirror plugin and dictation on. The STT URL points at nothing: it only makes
// the microphone drawn, and no shot records.

const outDir = path.join(repoRoot, 'docs', 'img');
const compose = path.join(e2eRoot, 'gotrue', 'compose.yml');
const project = `muster-e2e-gotrue-${docsGotruePort}`;
const browserURL = `http://localhost:${docsBrowserPort}`;
const machineURL = `http://localhost:${docsMainPort}`;
const gotrueURL = `http://127.0.0.1:${docsGotruePort}`;
const composeEnv = {
  ...process.env,
  MUSTER_E2E_GOTRUE_PROJECT: project,
  MUSTER_E2E_GOTRUE_PORT: String(docsGotruePort),
  MUSTER_E2E_GOTRUE_SITE: browserURL,
  MUSTER_E2E_GOTRUE_PASSKEYS: 'true',
};
const host = 'workstation';
const adminEmail = `docs-admin-${Date.now()}@example.com`;
const adminPW = 'e2e-docs-password-not-a-real-one';
const mirrorBin = path.join(binDir, 'muster-mirror-e2e-docs');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'muster-docshots-'));
const windowsFile = path.join(dir, 'windows');
const pasteFile = path.join(dir, 'pasted');
const transcript = path.join(dir, 'aaaaaaaa-docs-0001.jsonl');
const transcript2 = path.join(dir, 'bbbbbbbb-docs-0002.jsonl');

let server: Running | undefined;
let mirrorProc: ChildProcess | undefined;
let mirrorLog = '';

const rec = (o: unknown) => JSON.stringify(o) + '\n';
function writeTranscripts() {
  fs.writeFileSync(
    transcript,
    rec({ type: 'user', promptId: 'p1', message: { role: 'user', content: 'The task-list endpoint is slow with 5k tasks. Find out why and fix it.' } }) +
      rec({ type: 'assistant', message: { content: [{ type: 'text', text: "I'll profile the list query first." }] } }) +
      rec({ type: 'assistant', message: { content: [{ type: 'tool_use', id: 't1', name: 'Bash', input: { command: 'go test -run TestListTasks -bench . ./internal/notes' } }] } }) +
      rec({ type: 'user', promptId: 'p1', message: { content: [{ type: 'tool_result', tool_use_id: 't1', content: 'BenchmarkListTasks  412 ms/op' }] } }) +
      rec({ type: 'assistant', message: { content: [{ type: 'tool_use', id: 't2', name: 'Read', input: { file_path: 'internal/notes/list.go' } }] } }) +
      rec({ type: 'user', promptId: 'p1', message: { content: [{ type: 'tool_result', tool_use_id: 't2', content: '…' }] } }) +
      rec({ type: 'assistant', message: { content: [{ type: 'tool_use', id: 't3', name: 'Edit', input: { file_path: 'internal/notes/list.go' } }] } }) +
      rec({ type: 'user', promptId: 'p1', message: { content: [{ type: 'tool_result', tool_use_id: 't3', content: 'ok' }] } }) +
      rec({
        type: 'assistant',
        message: { content: [{ type: 'text', text: 'Comments were loaded one query per task. They now load in one batched query: 412 ms → 9 ms. Want me to add an index on the comments table as well?' }] },
      }),
  );
  fs.writeFileSync(
    transcript2,
    rec({ type: 'user', promptId: 'q1', message: { role: 'user', content: 'Write the release notes.' } }) +
      rec({ type: 'assistant', message: { content: [{ type: 'text', text: 'Drafted in CHANGELOG.md.' }] } }),
  );
}

const now = Math.floor(Date.now() / 1000);
// <session>|<window id>|<index>|<active 0/1>|<command>|<activity>|<transcript>
const windows = [
  ['api', '@1', 1, 1, 'claude', now - 30, transcript],
  ['api', '@2', 2, 0, 'zsh', now - 400, ''],
  ['web', '@3', 1, 1, 'claude', now - 90, transcript2],
  ['web', '@4', 2, 0, 'npm', now - 1200, ''],
  ['dotfiles', '@5', 1, 1, 'nvim', now - 5000, ''],
];

test.describe.configure({ mode: 'serial' });
test.use({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 1, isMobile: true, hasTouch: true, storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(300_000);
  expect(new URL(dsn).pathname, 'screenshots run only against the seeded *_e2e database').toMatch(/_e2e$/);
  fs.mkdirSync(outDir, { recursive: true });
  execFileSync('docker', ['compose', '-f', compose, 'up', '-d', '--wait'], { env: composeEnv, stdio: 'inherit' });
  execFileSync('go', ['run', './e2e/seed', '--accounts-only'], { cwd: repoRoot, stdio: 'inherit', env: { ...process.env, DATABASE_URL: dsn } });
  execFileSync('go', ['build', '-tags', 'e2emirror', '-o', mirrorBin, './cmd/muster-mirror'], { cwd: repoRoot, stdio: 'inherit' });
  server = await startServer(versionA, docsMainPort, {
    MUSTER_AUTH_MODE: 'gotrue',
    MUSTER_GOTRUE_URL: gotrueURL,
    MUSTER_GOTRUE_JWT_SECRET: gotrueSecret,
    MUSTER_BROWSER_PORT: String(docsBrowserPort),
    MUSTER_PUBLIC_URL: browserURL,
    MUSTER_MIRROR: '1',
    MUSTER_STT_URL: 'http://127.0.0.1:9',
    MUSTER_STT_TOKEN: 'e2e-docs-stt-token-not-a-real-one',
  });
  writeTranscripts();
  fs.writeFileSync(windowsFile, windows.map((w) => w.join('|')).join('\n') + '\n');
  const token = execFileSync(serverBinary(versionA), ['mirror', 'token', 'create', host], { env: serverEnv() }).toString().trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
  mirrorProc = spawn(mirrorBin, [], {
    env: {
      ...process.env,
      MUSTER_MIRROR_URL: machineURL,
      MUSTER_MIRROR_ALLOW_HTTP: '1',
      MUSTER_MIRROR_TOKEN: token,
      MUSTER_MIRROR_HOST: host,
      MUSTER_MIRROR_INTERACTIVE: '1',
      MUSTER_MIRROR_E2E_WINDOWS_FILE: windowsFile,
      MUSTER_MIRROR_E2E_PASTE_FILE: pasteFile,
      MUSTER_MIRROR_E2E_WINDOW: '120x40',
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  mirrorProc.stdout?.on('data', (d) => (mirrorLog += d));
  mirrorProc.stderr?.on('data', (d) => (mirrorLog += d));
});

test.afterAll(async () => {
  try {
    if (mirrorProc && mirrorProc.exitCode === null) {
      const done = new Promise((r) => mirrorProc!.once('exit', r));
      mirrorProc.kill('SIGTERM');
      await done;
    }
    await server?.stop();
  } finally {
    execFileSync('docker', ['compose', '-f', compose, 'down', '-v'], { env: composeEnv, stdio: 'inherit' });
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

const serverEnv = () => ({ ...process.env, DATABASE_URL: dsn, MUSTER_PUBLIC_URL: browserURL, MUSTER_GOTRUE_URL: gotrueURL, MUSTER_GOTRUE_JWT_SECRET: gotrueSecret });
const q = (session: string, id: string) => `host=${host}&session=${session}&window=${encodeURIComponent(id)}`;

// The push-permission line reads "Notifications are blocked…" in a headless
// browser: true of the harness, not of the app. Hidden, as e2e/capture does.
async function shoot(page: Page, file: string) {
  await page.addStyleTag({ content: '#push-status{display:none!important} *{caret-color:transparent!important}' });
  await page.waitForTimeout(600);
  await page.screenshot({ path: path.join(outDir, file) });
}

// One sign-in for the whole file: the admin claims user 1 (the owner of the
// seeded board and of the mirror token), and the cookies are reused.
let cookies: Awaited<ReturnType<BrowserContext['cookies']>> | undefined;
async function signIn(page: Page) {
  if (cookies) {
    await page.context().addCookies(cookies);
    return;
  }
  const link = execFileSync(serverBinary(versionA), ['invite', '--claim-admin'], { env: serverEnv() }).toString().trim();
  await page.goto(link);
  await page.fill('#claim-email', adminEmail);
  await page.fill('#claim-password', adminPW);
  await page.fill('#claim-password2', adminPW);
  await Promise.all([page.waitForURL(browserURL + '/'), page.click('button[type=submit]')]);
  cookies = await page.context().cookies(browserURL);
}

test('tasks.png — the task board', async ({ page }) => {
  await signIn(page);
  await page.goto(browserURL + '/tasks');
  await expect(page.locator('#tasks-list article').first()).toBeVisible();
  await shoot(page, 'tasks.png');
});

test('agents-filter.png — mirrored windows and a dispatched agent, filtered', async ({ page }) => {
  await signIn(page);
  await page.goto(browserURL + '/agents');
  await expect(page.locator('#agents-list article[data-mirror-card]').first()).toBeVisible({ timeout: 20_000 });
  await page.locator('#agents-search').fill('api');
  await expect(page.locator('#agents-count')).toContainText('of');
  await shoot(page, 'agents-filter.png');
});

test('mirror-rendered.png — a Claude Code window as a chat, with the message box and mic', async ({ page }) => {
  await signIn(page);
  await page.goto(browserURL + '/mirror/view?' + q('api', '@1'));
  await expect(page.locator('#mv-log [data-bubble="assistant"]').last()).toContainText('batched query', { timeout: 20_000 });
  await expect(page.locator('#mv-dictate')).toBeVisible();
  await page.locator('#mv-input').fill('Yes, add the index and rerun the benchmark');
  await shoot(page, 'mirror-rendered.png');
});

test('mirror-terminal.png — the terminal view of a shell window', async ({ page }) => {
  await signIn(page);
  await page.goto(browserURL + '/mirror/terminal?' + q('api', '@2'));
  await expect(page.locator('#term .xterm-rows')).toContainText('FAKE-MIRROR', { timeout: 20_000 });
  await expect(page.locator('#mv-input')).toBeEnabled();
  await page.locator('#mv-input').fill('make test');
  await shoot(page, 'mirror-terminal.png');
});

// A stale sign-in is made by re-signing the access cookie with its `amr` moved
// 13 hours back — the token GoTrue would mint for a sign-in that long ago (see
// e2e/tests/mirror-stepup.spec.ts, which this mirrors).
function b64urlJSON(part: string): Record<string, unknown> {
  return JSON.parse(Buffer.from(part, 'base64url').toString('utf8'));
}
function sign(payload: Record<string, unknown>): string {
  const h = Buffer.from(JSON.stringify({ alg: 'HS256', typ: 'JWT' })).toString('base64url');
  const p = Buffer.from(JSON.stringify(payload)).toString('base64url');
  return `${h}.${p}.${crypto.createHmac('sha256', gotrueSecret).update(h + '.' + p).digest('base64url')}`;
}

test('stepup-passkey.png — confirm it is you, with a passkey', async ({ page, context }) => {
  test.setTimeout(120_000);
  await authenticator(page);
  await signIn(page);
  await page.goto(browserURL + '/account/passkeys');
  await page.locator('#passkey-add').click();
  await expect(page.locator('#passkey-list [data-passkey-id]')).toHaveCount(1);

  await page.goto(browserURL + '/mirror/view?' + q('api', '@1'));
  await expect(page.locator('#mv-input')).toBeEnabled({ timeout: 20_000 });
  const real = (await context.cookies(browserURL)).find((c) => c.name === 'muster_at')!.value;
  const claims = b64urlJSON(real.split('.')[1]);
  const t = Math.floor(Date.now() / 1000);
  claims.amr = [{ method: 'password', timestamp: t - 13 * 3600 }];
  claims.exp = t + 600;
  await context.addCookies([{ name: 'muster_at', value: sign(claims), url: browserURL, httpOnly: true, secure: true, sameSite: 'Lax' }]);

  await page.locator('#mv-input').fill('Ship it');
  await page.locator('#mv-send').tap();
  await expect(page.locator('#term-confirm')).toBeVisible();
  await expect(page.locator('#term-confirm-passkey')).toBeVisible();
  await shoot(page, 'stepup-passkey.png');
  expect(fs.existsSync(transcript + '.pasted'), 'nothing was pasted while the sign-in is stale').toBe(false);
  expect(mirrorLog).not.toContain('panic');
});
