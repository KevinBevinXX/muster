import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { APIRequestContext, request as pwRequest } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { binDir, dsn, hookToken, password, repoRoot, routingConnectPort, routingPort, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';

// ONE ROUTED-MAIL ROUND TRIP, end to end, every hop a real route:
//
//   a plugin POSTs a message from a VERIFIED sender ALLOWLISTED for it → the chief (a fixture
//   row; the e2e harness runs no agent runtime — see plugins.spec.ts) is being
//   handed it → the chief calls route_to_session over the real /mcp door → the
//   SEND QUEUE pastes the fenced block into the mirrored Claude Code window (a
//   real muster-mirror over the tunnel, -tags e2emirror) once Claude Code is
//   idle → the session answers with `muster route reply` (the real CLI, on the
//   host's hook token) → the answer is an AUTO-APPROVED draft → the plugin's
//   ordinary claim takes it, once, and reports it sent.
//
// A password-mode server: the operator (user 1) owns everything, and the hook
// token acts as user 1, which is what a mirrored session's host holds.

const origin = `http://localhost:${routingPort}`;
const connectURL = `http://127.0.0.1:${routingConnectPort}`;
const stamp = Date.now().toString(36);
const host = 'e2e-route-' + stamp;
const sid = 'eeeeeeee-' + stamp;
const chiefName = 'e2e-route-chief-' + stamp;
const chiefToken = 'e2e-route-chief-token-not-a-real-one-' + stamp;
const mirrorBin = path.join(binDir, 'muster-mirror-e2e');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'routing-'));
const tr = path.join(dir, sid + '.jsonl');
const FAKE_KEY = Buffer.alloc(32, 0x3c).toString('base64');
let server: Running | undefined;
let mirrorProc: ChildProcess | undefined;
let mirrorLog = '';
let plugin: APIRequestContext | undefined;
let chiefMade = false;

const read = (f: string) => (fs.existsSync(f) ? fs.readFileSync(f, 'utf8') : '');
const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(240_000);
  execFileSync('go', ['build', '-tags', 'e2emirror', '-o', mirrorBin, './cmd/muster-mirror'], { cwd: repoRoot, stdio: 'inherit' });
  server = await startServer(versionTerm, routingPort, {
    MUSTER_MIRROR: '1',
    MUSTER_PLUGINS: '1',
    MUSTER_SESSION_ROUTING: '1',
    MUSTER_ROUTE_RELAY_ALLOW: '1:route-inbox:ada@example.invalid',
    MUSTER_SEND_QUEUE_IDLE: '1s',
    MUSTER_FOCUS_INTERVAL: '0',
    MUSTER_CONNECTOR_ENCRYPTION_KEY: FAKE_KEY,
    MUSTER_CONNECT_PORT: String(routingConnectPort),
    MUSTER_AGENT_CONNECT_URL: connectURL,
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
  });
  const token = execFileSync(serverBinary(versionTerm), ['mirror', 'token', 'create', host], { env: { ...process.env, DATABASE_URL: dsn } })
    .toString()
    .trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
  fs.writeFileSync(tr, JSON.stringify({ type: 'user', message: { role: 'user', content: 'working on the calendar' } }) + '\n');
  // Claude Code idle, as its sessions file says it — written a minute ago.
  fs.writeFileSync(tr + '.status', JSON.stringify({ status: 'idle', statusUpdatedAt: Date.now() - 60_000 }));
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
  await plugin?.dispose();
  try {
    if (chiefMade) fix('cleanup', '1', chiefName);
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
    headers: { 'X-Muster-Token': chiefToken, 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream' },
    body: JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'tools/call', params: { name, arguments: args } }),
  });
  expect(r.status, 'the /mcp door answered').toBe(200);
  const j = (await r.json()) as { result?: { content: { text: string }[]; isError?: boolean }; error?: unknown };
  expect(j.error, JSON.stringify(j.error)).toBeUndefined();
  return { text: j.result!.content[0].text, isError: !!j.result!.isError };
}

test('a routed message reaches the mirrored session, its answer is auto-approved and the plugin sends it once', async ({ page }) => {
  test.setTimeout(180_000);
  // The owner signs in and mints the plugin's token on /plugins.
  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
  await page.goto(origin + '/plugins');
  await page.fill('#plugin-name', 'route-inbox');
  await page.fill('#plugin-label', 'Route inbox');
  await page.fill('#plugin-scope', 'route-inbox');
  await page.getByRole('button', { name: 'Create plugin token' }).click();
  const token = await page.locator('#minted-plugin-token').inputValue();
  expect(token).toMatch(/^mpl_/);
  plugin = await pwRequest.newContext({ baseURL: origin, extraHTTPHeaders: { Authorization: `Bearer ${token}` } });

  // 1. The plugin posts a message from the allowlisted sender, which it verified.
  const posted = await plugin.post('/plugin/v1/messages', {
    data: { external_id: 'route-msg-1', sender: 'Ada <ada@example.invalid>', subject: 'standup', body: 'Can we move standup to 10?',
      sender_verified: true },
  });
  expect(posted.status(), await posted.text()).toBe(202);
  const messageID = ((await posted.json()) as { id: number }).id;

  // 2. The chief is being handed it (fixture; see the header).
  const chiefID = fix('chief', '1', chiefName, chiefToken);
  chiefMade = true;
  fix('delivering', String(messageID), chiefID);

  // 3. The chief routes it over /mcp to the mirrored window's Claude session.
  await expect.poll(async () => (await mcpCall('route_to_session', { message_id: messageID, subject: 'mirror:nope' })).text,
    { message: `the host is connected (log:\n${mirrorLog})`, timeout: 20_000 }).toContain('window_gone');
  let routed = { text: '', isError: true };
  await expect.poll(async () => {
    routed = await mcpCall('route_to_session', { message_id: messageID, subject: 'mirror:' + sid });
    return routed.isError ? routed.text : 'ok';
  }, { message: `the window reports its Claude session (log:\n${mirrorLog})`, timeout: 20_000 }).toBe('ok');
  const route = JSON.parse(routed.text) as { route_id: string; state: string };
  expect(route.route_id).toMatch(/^rt_[0-9a-f]{24}$/);
  expect(route.state).toBe('routed');

  // 4. The send queue pastes the fenced block into the window once Claude is idle.
  await expect.poll(() => read(tr + '.pasted'), { message: `the routed block is pasted (log:\n${mirrorLog})`, timeout: 60_000 })
    .toContain(route.route_id);
  const pasted = read(tr + '.pasted');
  expect(pasted).toContain('UNTRUSTED external message');
  expect(pasted).toContain('muster route reply ' + route.route_id);
  expect(pasted).toContain('Can we move standup to 10?');

  // 5. The session answers with the real CLI, on its host's hook token.
  const out = execFileSync('go', ['run', './cmd/muster', 'route', 'reply', route.route_id, '--body-file', '-'], {
    cwd: repoRoot,
    input: 'Yes, 10 works for everyone.',
    env: { ...process.env, MUSTER_API_URL: origin, MUSTER_TOKEN: hookToken },
  }).toString();
  const answered = JSON.parse(out) as { state: string; draft_status: string; draft_id: number };
  expect(answered.state).toBe('relayed');
  expect(answered.draft_status).toBe('approved');

  // 6. The plugin's ordinary claim takes it — once — and reports it sent.
  const claim = await plugin.post('/plugin/v1/drafts/claim', { data: { wait_seconds: 5 } });
  expect(claim.status(), await claim.text()).toBe(200);
  const c = (await claim.json()) as { draft_id: number; message_external_id: string; body: string };
  expect(c).toMatchObject({ draft_id: answered.draft_id, message_external_id: 'route-msg-1', body: 'Yes, 10 works for everyone.' });
  const again = await plugin.post('/plugin/v1/drafts/claim', { data: { wait_seconds: 0 } });
  expect(again.status(), 'a relayed draft is claimed once').toBe(204);
  const result = await plugin.post(`/plugin/v1/drafts/${c.draft_id}/result`, { data: { ok: true, external_ref: 'upstream-1' } });
  expect(result.status(), await result.text()).toBe(200);

  // 7. The answer again is the stored result; the route is done.
  const repeat = execFileSync('go', ['run', './cmd/muster', 'route', 'reply', route.route_id, '--body', 'Yes, 10 works for everyone.'], {
    cwd: repoRoot,
    env: { ...process.env, MUSTER_API_URL: origin, MUSTER_TOKEN: hookToken },
  }).toString();
  expect((JSON.parse(repeat) as { draft_id: number }).draft_id).toBe(answered.draft_id);
});
