import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { test, expect } from '../lib/fixtures';
import { attentionConnectPort, attentionPort, binDir, dsn, password, repoRoot, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';

// THE ATTENTION QUEUE, the operator's click path, every hop real:
//
//   an agent asks over the real /mcp door (attention_ask, with options) and
//   blocks in attention_wait → the shell's Attention badge counts it beside a
//   mirrored Claude Code window that is waiting at a prompt (a real
//   muster-mirror over the tunnel, -tags e2emirror) → /attention lists both →
//   the operator taps an option → the agent's wait returns that answer, the
//   item leaves the list and the badge drops. A second ask raised while the
//   page is open appears without a reload (the queue's SSE event), and Dismiss
//   withdraws it.
//
// A password-mode server: the operator (user 1) owns the agent and the host.

const origin = `http://localhost:${attentionPort}`;
const connectURL = `http://127.0.0.1:${attentionConnectPort}`;
const stamp = Date.now().toString(36);
const host = 'e2e-attn-' + stamp;
const sid = 'aaaaaaaa-' + stamp;
const agentName = 'e2e-attn-agent-' + stamp;
const agentToken = 'e2e-attn-agent-token-not-a-real-one-' + stamp;
const mirrorBin = path.join(binDir, 'muster-mirror-e2e');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'attention-'));
const tr = path.join(dir, sid + '.jsonl');
let server: Running | undefined;
let mirrorProc: ChildProcess | undefined;
let mirrorLog = '';
let agentMade = false;

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(240_000);
  execFileSync('go', ['build', '-tags', 'e2emirror', '-o', mirrorBin, './cmd/muster-mirror'], { cwd: repoRoot, stdio: 'inherit' });
  server = await startServer(versionTerm, attentionPort, {
    MUSTER_MIRROR: '1',
    MUSTER_FOCUS_INTERVAL: '0',
    MUSTER_CONNECTOR_ENCRYPTION_KEY: Buffer.alloc(32, 0x3d).toString('base64'),
    MUSTER_CONNECT_PORT: String(attentionConnectPort),
    MUSTER_AGENT_CONNECT_URL: connectURL,
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
  });
  fix('agent', '1', agentName, agentToken);
  agentMade = true;
  const token = execFileSync(serverBinary(versionTerm), ['mirror', 'token', 'create', host], { env: { ...process.env, DATABASE_URL: dsn } })
    .toString()
    .trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
  fs.writeFileSync(tr, JSON.stringify({ type: 'user', message: { role: 'user', content: 'deploying the importer' } }) + '\n');
  // Claude Code waiting on the person, as its sessions file says it.
  fs.writeFileSync(tr + '.status', JSON.stringify({ status: 'waiting', statusUpdatedAt: Date.now() - 30_000 }));
  const windows = path.join(dir, 'windows');
  fs.writeFileSync(windows, `work|@1|0|1|claude|${Math.floor(Date.now() / 1000) - 60}|${tr}\n`);
  mirrorProc = spawn(mirrorBin, [], {
    env: {
      ...process.env,
      MUSTER_MIRROR_URL: origin,
      MUSTER_MIRROR_ALLOW_HTTP: '1',
      MUSTER_MIRROR_TOKEN: token,
      MUSTER_MIRROR_HOST: host,
      MUSTER_MIRROR_INTERACTIVE: '1',
      MUSTER_MIRROR_E2E_WINDOWS_FILE: windows,
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  mirrorProc.stdout?.on('data', (d) => (mirrorLog += d));
  mirrorProc.stderr?.on('data', (d) => (mirrorLog += d));
});

test.afterAll(async () => {
  try {
    if (agentMade) fix('cleanup', '1', agentName);
  } finally {
    if (mirrorProc && mirrorProc.exitCode === null) {
      const done = new Promise((r) => mirrorProc!.once('exit', r));
      mirrorProc.kill('SIGTERM');
      await done;
    }
    await server?.stop();
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

async function mcpCall(name: string, args: Record<string, unknown>): Promise<{ text: string; isError: boolean }> {
  const r = await fetch(connectURL + '/mcp', {
    method: 'POST',
    headers: { 'X-Muster-Token': agentToken, 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream' },
    body: JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'tools/call', params: { name, arguments: args } }),
  });
  expect(r.status, 'the /mcp door answered').toBe(200);
  const j = (await r.json()) as { result?: { content: { text: string }[]; isError?: boolean }; error?: unknown };
  expect(j.error, JSON.stringify(j.error)).toBeUndefined();
  return { text: j.result!.content[0].text, isError: !!j.result!.isError };
}

async function ask(question: string, options: string[]): Promise<number> {
  const { text, isError } = await mcpCall('attention_ask', { question, options });
  expect(isError, text).toBe(false);
  const id = (JSON.parse(text) as { id: number }).id;
  expect(id).toBeGreaterThan(0);
  return id;
}

test('an agent asks over MCP, the operator answers in the Attention view, and the waiting agent gets the answer', async ({ page }) => {
  test.setTimeout(180_000);
  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);

  const id = await ask('Deploy the importer to production now?', ['deploy now', 'wait until Monday']);
  // The agent blocks on the answer — a real long poll through the connect door.
  const waited = mcpCall('attention_wait', { id, wait_seconds: 120 });

  // The shell's badge counts the ask and the waiting window (the host's
  // status reaches muster on its own schedule, so poll).
  // The badge refreshes on its triggers only — never by re-triggering itself
  // (a fragment with its own `load` trigger once polled every ~25ms).
  let badgeFetches = 0;
  page.on('request', (r) => {
    if (new URL(r.url()).pathname === '/attention/badge') badgeFetches++;
  });
  await page.goto(origin + '/tasks');
  const badge = page.locator('#attention-badge');
  await expect(badge, mirrorLog).toHaveAttribute('data-count', '2', { timeout: 30_000 });
  const settled = badgeFetches;
  await page.waitForTimeout(1500); // a bounded observation window: a loop would add ~60 here
  expect(badgeFetches - settled, 'badge requests while nothing changed').toBeLessThanOrEqual(1);

  await page.goto(origin + '/attention');
  const askItem = page.locator(`#ask-${id}`);
  await expect(askItem).toBeVisible();
  await expect(askItem.locator('[data-attention-text]')).toHaveText('Deploy the importer to production now?');
  await expect(askItem.locator('[data-attention-source]')).toHaveText(agentName);
  const session = page.locator('[data-attention-item][data-kind="session"]');
  await expect(session).toHaveCount(1);
  await expect(session.locator('[data-attention-source]')).toContainText(host);
  await expect(session.getByRole('link', { name: 'Open the session' })).toHaveAttribute('href', /^\/mirror\/view\?host=e2e-attn-/);

  // The operator taps an option.
  await Promise.all([page.waitForURL(origin + '/attention'), askItem.getByRole('button', { name: 'wait until Monday' }).click()]);
  const answer = await waited;
  expect(JSON.parse(answer.text)).toMatchObject({ id, status: 'answered', answer: 'wait until Monday', answered_by: 'operator' });
  await expect(page.locator(`#ask-${id}`)).toHaveCount(0);
  await expect(page.locator('[data-attention-item][data-kind="session"]')).toHaveCount(1);

  // A second ask while the page is open arrives live, without a reload.
  const id2 = await ask('Rotate the staging key too?', []);
  const second = page.locator(`#ask-${id2}`);
  await expect(second).toBeVisible({ timeout: 15_000 });
  await expect(second.locator('[data-attention-option]')).toHaveCount(0);
  // Free text, then Dismiss on a third: both leave the list.
  await second.locator('textarea').fill('yes, rotate it');
  await Promise.all([page.waitForURL(origin + '/attention'), second.getByRole('button', { name: 'Answer', exact: true }).click()]);
  expect(JSON.parse((await mcpCall('attention_get', { id: id2 })).text)).toMatchObject({ status: 'answered', answer: 'yes, rotate it' });
  const id3 = await ask('Ignore me?', []);
  await page.reload();
  await Promise.all([page.waitForURL(origin + '/attention'), page.locator(`#ask-${id3}`).getByRole('button', { name: 'Dismiss without answering' }).click()]);
  expect(JSON.parse((await mcpCall('attention_get', { id: id3 })).text)).toMatchObject({ status: 'cancelled' });
  await expect(page.locator('[data-attention-item][data-kind="ask"]')).toHaveCount(0);

  // The badge is back to the waiting window alone.
  await page.goto(origin + '/tasks');
  await expect(page.locator('#attention-badge')).toHaveAttribute('data-count', '1', { timeout: 15_000 });
});
