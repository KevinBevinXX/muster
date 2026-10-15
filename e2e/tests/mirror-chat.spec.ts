import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { binDir, dsn, mirrorChatPort, password, repoRoot, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';

// THE MIRRORED RENDERED VIEW'S CHAT, on a phone with touch (412×915), against a
// REAL muster-server and a REAL muster-mirror built with `-tags e2emirror`
// (cmd/muster-mirror/e2emirror.go): session main is a Claude Code pane whose
// transcript is a real JSONL file this spec writes record by record, read by
// the real tailer and parser. Every record shape is the CLI's own; the content
// is invented.
test.use({ viewport: { width: 412, height: 915 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

const origin = `http://localhost:${mirrorChatPort}`;
const host = 'e2e-chat-' + Date.now().toString(36);
const mirrorBin = path.join(binDir, 'muster-mirror-e2e');
let server: Running | undefined;
let mirrorProc: ChildProcess | undefined;
let mirrorLog = '';
let token = '';

function startMirror(transcript: string): ChildProcess {
  const env: NodeJS.ProcessEnv = {
    ...process.env,
    MUSTER_MIRROR_URL: origin,
    MUSTER_MIRROR_ALLOW_HTTP: '1',
    MUSTER_MIRROR_TOKEN: token,
    MUSTER_MIRROR_HOST: host,
    MUSTER_MIRROR_E2E_SESSIONS: 'main',
    MUSTER_MIRROR_E2E_CLAUDE: 'main=' + transcript,
  };
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
  server = await startServer(versionTerm, mirrorChatPort, {
    MUSTER_MIRROR: '1',
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
  });
  token = execFileSync(serverBinary(versionTerm), ['mirror', 'token', 'create', host], { env: { ...process.env, DATABASE_URL: dsn } })
    .toString()
    .trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
});

test.afterEach(async () => {
  await stopMirror();
});

test.afterAll(async () => {
  await server?.stop();
});

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

const rec = (o: object) => JSON.stringify(o) + '\n';
const prompt = (t: string) => rec({ type: 'user', promptId: 'p', origin: { kind: 'human' }, message: { role: 'user', content: t } });
const reply = (t: string) => rec({ type: 'assistant', message: { content: [{ type: 'text', text: t }] } });
const taskNotice = (status: string, summary: string, result: string) =>
  rec({
    type: 'user',
    promptId: 'p',
    origin: { kind: 'task-notification' },
    message: {
      role: 'user',
      content: `<task-notification>\n<task-id>t1</task-id>\n<status>${status}</status>\n<summary>${summary}</summary>\n<result>${result}</result>\n</task-notification>`,
    },
  });
const command = (name: string) =>
  rec({ type: 'user', promptId: 'p', message: { role: 'user', content: `<command-name>${name}</command-name>\n<command-message>${name.slice(1)}</command-message>\n<command-args></command-args>` } });

function newTranscript(body: string): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'muster-e2e-chat-'));
  const p = path.join(dir, 'e2e-chat-0001.jsonl');
  fs.writeFileSync(p, body);
  return p;
}

const viewPath = `/mirror/view?host=${encodeURIComponent(host)}&session=main`;

async function openView(page: Page, transcript: string) {
  mirrorProc = startMirror(transcript);
  await login(page);
  await expect
    .poll(async () => (await (await page.request.get(origin + '/ui/agents')).text()).includes('data-mirror-claude'), {
      message: `main reported as a Claude pane (muster-mirror log:\n${mirrorLog})`,
      timeout: 20_000,
    })
    .toBe(true);
  await page.goto(origin + viewPath);
  await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 15_000 });
}

// B1: a harness notice is its own compact row — status and summary, detail on
// expand — and never the human's bubble; a human who MENTIONS the tag stays one.
test('a task notification is a compact collapsible row, not a user bubble; a prompt that mentions the tag is still a bubble', async ({ page }) => {
  const tr = newTranscript(
    prompt('audit the widget in the background') +
      reply('Started an agent on it.') +
      taskNotice('completed', 'Agent "Audit the widget" finished', 'The widget is fine. Two nits.') +
      prompt('why did the <task-notification> say completed?'),
  );
  await openView(page, tr);
  const log = page.locator('#mv-log');

  const row = log.locator('[data-notice-row="task"]');
  await expect(row).toHaveCount(1);
  await expect(row).toHaveAttribute('data-notice-status', 'completed');
  await expect(row.locator('[data-notice-text]')).toHaveText('Agent "Audit the widget" finished');
  await expect(row.locator('summary [aria-label="status: completed"]')).toHaveText('✓');
  // Collapsed: the result is hidden until the row is tapped.
  await expect(row).not.toHaveAttribute('open', '');
  await expect(row.locator('pre')).toBeHidden();
  await row.locator('summary').tap();
  await expect(row).toHaveAttribute('open', '');
  await expect(row.locator('pre')).toContainText('The widget is fine.');

  // Exactly the human's two prompts are user bubbles — the notice is not one,
  // and the prompt that merely MENTIONS the tag is.
  const users = log.locator('[data-bubble="user"]');
  await expect(users).toHaveCount(2);
  await expect(users.nth(0)).toContainText('audit the widget in the background');
  await expect(users.nth(1)).toContainText('why did the <task-notification> say completed?');
  await expect(users.filter({ hasText: 'Audit the widget" finished' })).toHaveCount(0);
  // The row fits the phone, like a tool row.
  const box = (await row.boundingBox())!;
  expect(box.x + box.width, 'the notice row fits 412px').toBeLessThanOrEqual(412);

  // Live: a slash command arrives while the view is open, drawn the same way.
  fs.appendFileSync(tr, command('/model') + taskNotice('failed', 'Agent "Second" failed', 'boom'));
  await expect(log.locator('[data-notice-row="command"] [data-notice-text]')).toHaveText('/model');
  const failed = log.locator('[data-notice-row="task"][data-notice-status="failed"]');
  await expect(failed.locator('summary [aria-label="status: failed"]')).toHaveText('✗');
  await expect(users).toHaveCount(2);
});
