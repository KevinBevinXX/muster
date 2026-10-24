import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { test, expect } from '../lib/fixtures';
import { attentionConnectPort, attentionMirrorMCPPort, attentionPort, binDir, dsn, password, repoRoot, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { writeFileAtomic } from '../lib/atomic';

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
//   The waiting window shows WHAT it waits on — its pending tool call and the
//   last thing Claude said, read by the real muster-mirror from the session's
//   transcript (mirror.Pending) — and the queue is newest first. A message the
//   operator sends into a session that asked answers its ask in the session:
//   the item leaves the open queue live and the session's wait returns it.
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
// A second Claude Code session in the SAME window's other pane (a split).
const splitSid = 'bbbbbbbb-' + stamp;
const trSplit = path.join(dir, splitSid + '.jsonl');
// The fake host's tmux server (cmd/muster-mirror/e2emirror.go's E2ETmuxSocket),
// as $TMUX names it inside one of its panes.
const fakeTMUX = '/tmp/muster-e2e-fake-tmux/default,1,0';
const mcpSecretFile = path.join(dir, 'mcp-secret');
const localMCP = `http://127.0.0.1:${attentionMirrorMCPPort}/mcp`;
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
  // The session's transcript: a prompt, Claude's words, and a tool call it is
  // waiting on the person to allow (no result yet) — what the host reads as
  // the window's pending action.
  fs.writeFileSync(
    tr,
    [
      { type: 'user', message: { role: 'user', content: 'deploying the importer' } },
      { type: 'assistant', message: { role: 'assistant', content: [{ type: 'text', text: 'Tests pass.\n\nPublishing   the importer now.' }] } },
      {
        type: 'assistant',
        message: { role: 'assistant', content: [{ type: 'tool_use', id: 'toolu_e2e_pub', name: 'Bash', input: { command: 'npm publish --tag next\nnpm dist-tag ls' } }] },
      },
    ]
      .map((r) => JSON.stringify(r))
      .join('\n') + '\n',
  );
  // Claude Code waiting on the person, as its sessions file says it.
  writeFileAtomic(tr + '.status', JSON.stringify({ status: 'waiting', statusUpdatedAt: Date.now() - 30_000 }));
  const windows = path.join(dir, 'windows');
  fs.writeFileSync(trSplit, JSON.stringify({ type: 'user', message: { role: 'user', content: 'in the split' } }) + '\n');
  writeFileAtomic(windows, `work|@1|0|1|claude|${Math.floor(Date.now() / 1000) - 60}|${tr}|${trSplit}\n`);
  mirrorProc = spawn(mirrorBin, [], {
    env: {
      ...process.env,
      MUSTER_MIRROR_URL: origin,
      MUSTER_MIRROR_ALLOW_HTTP: '1',
      MUSTER_MIRROR_TOKEN: token,
      MUSTER_MIRROR_HOST: host,
      MUSTER_MIRROR_INTERACTIVE: '1',
      MUSTER_MIRROR_E2E_WINDOWS_FILE: windows,
      // The host's loopback MCP endpoint for its own Claude Code sessions.
      MUSTER_MIRROR_MCP_LISTEN: `127.0.0.1:${attentionMirrorMCPPort}`,
      MUSTER_MIRROR_MCP_SECRET_FILE: mcpSecretFile,
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

  // The queue is called "Asks" (operator decision "Rename to Asks"), at the
  // same /attention, so every existing /attention#<key> link still lands.
  // This server serves no plugins, so there is no Reply drafts tab.
  await expect(page.locator('#sidebar-attention')).toContainText('Asks');
  await page.goto(origin + '/attention');
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Asks');
  await expect(page).toHaveTitle('Asks · muster');
  await expect(page.getByRole('tab'), 'no plugins: no tabs').toHaveCount(0);
  const askItem = page.locator(`#ask-${id}`);
  await expect(askItem).toBeVisible();
  await expect(askItem.locator('[data-attention-text]')).toHaveText('Deploy the importer to production now?');
  await expect(askItem.locator('[data-attention-source]')).toHaveText(agentName);
  const session = page.locator('[data-attention-item][data-kind="session"]');
  await expect(session).toHaveCount(1);
  await expect(session.locator('[data-attention-source]')).toContainText(host);
  // What it waits on — not a generic line: the pending command (its first
  // line only) and Claude's last words, whitespace-collapsed.
  await expect(session.locator('[data-attention-text]')).toHaveText('Bash: npm publish --tag next');
  await expect(session.locator('[data-attention-detail]')).toHaveText('Tests pass. Publishing the importer now.');
  // Newest first: the ask (just now) above the window (waiting since 30 s ago).
  await expect(page.locator('[data-attention-item]').first()).toHaveAttribute('id', `ask-${id}`);
  await expect(page.locator('[data-attention-item]').nth(1)).toHaveAttribute('data-kind', 'session');
  await expect(session.getByRole('link', { name: 'Open the session' })).toHaveAttribute('href', /^\/mirror\/view\?host=e2e-attn-/);

  // The operator taps an option.
  await Promise.all([page.waitForURL(origin + '/attention'), askItem.getByRole('button', { name: 'wait until Monday' }).click()]);
  const answer = await waited;
  expect(JSON.parse(answer.text)).toMatchObject({ id, status: 'answered', answer: 'wait until Monday' });
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
  expect(JSON.parse((await mcpCall('attention_wait', { id: id2, wait_seconds: 0 })).text)).toMatchObject({ status: 'answered', answer: 'yes, rotate it' });
  const id3 = await ask('Ignore me?', []);
  await page.reload();
  await Promise.all([page.waitForURL(origin + '/attention'), page.locator(`#ask-${id3}`).getByRole('button', { name: 'Dismiss without answering' }).click()]);
  expect(JSON.parse((await mcpCall('attention_wait', { id: id3, wait_seconds: 0 })).text)).toMatchObject({ status: 'cancelled' });
  await expect(page.locator('[data-attention-item][data-kind="ask"]')).toHaveCount(0);

  // The badge is back to the waiting window alone.
  await page.goto(origin + '/tasks');
  await expect(page.locator('#attention-badge')).toHaveAttribute('data-count', '1', { timeout: 15_000 });
});

// THE MIRRORED SESSION'S half: a Claude Code session in the mirrored window
// calls its host's LOOPBACK MCP endpoint (muster-mirror -mcp-listen) with the
// host's local secret and its tmux pane (what ${TMUX_PANE} expands to in the
// MCP config); the host forwards it to muster's /mirror/mcp with the host's
// mirror token and the pane's Claude session id. The ask shows on /attention
// as that window's, and the operator's answer reaches the session's wait.
async function localCall(name: string, args: Record<string, unknown>, headers: Record<string, string> = {}): Promise<Response> {
  return fetch(localMCP, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...headers },
    body: JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'tools/call', params: { name, arguments: args } }),
  });
}

test('a mirrored Claude Code session asks through its host and gets the operator\'s answer', async ({ page }) => {
  test.setTimeout(120_000);
  const secret = fs.readFileSync(mcpSecretFile, 'utf8').trim();
  expect((fs.statSync(mcpSecretFile).mode & 0o777).toString(8), 'the secret file is private').toBe('600');
  const pane = '%e2e-work-@1';
  const auth = { Authorization: 'Bearer ' + secret, 'X-Muster-Pane': pane, 'X-Muster-Tmux': fakeTMUX };
  // Without the secret the endpoint refuses, and nothing is asked; nor from a
  // pane of another tmux server.
  expect((await localCall('attention_ask', { question: 'no secret?' }, { 'X-Muster-Pane': pane, 'X-Muster-Tmux': fakeTMUX })).status).toBe(401);
  expect((await localCall('attention_ask', { question: 'other server?' }, { ...auth, 'X-Muster-Tmux': '/tmp/tmux-1/other,9,0' })).status).toBe(400);
  // A chief tool is not on this surface.
  const chief = (await (await localCall('attention_list', {}, auth)).json()) as { error?: { code: number } };
  expect(chief.error?.code).toBe(-32602);
  // A session that has no Claude session to show yet — Claude Code connecting
  // as it starts — still connects: initialize and tools/list reach muster. Its
  // CALL is a tool error naming the pane, not a failed connection.
  const fresh = { ...auth, 'X-Muster-Pane': '%e2e-not-claude-yet' };
  const init = await fetch(localMCP, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', ...fresh },
    body: JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { protocolVersion: '2025-06-18' } }),
  });
  expect(init.status).toBe(200);
  expect(((await init.json()) as { result: { instructions: string } }).result.instructions).toContain('attention_ask');
  const unbound = (await (await localCall('attention_ask', { question: 'too early?' }, fresh)).json()) as {
    result: { isError: boolean; content: { text: string }[] };
  };
  expect(unbound.result.isError).toBe(true);
  expect(unbound.result.content[0].text).toContain('no Claude Code session runs in pane');

  const asked = (await (await localCall('attention_ask', { question: 'Merge the schema change?', options: ['merge', 'hold'] }, auth)).json()) as {
    result: { content: { text: string }[] };
  };
  const id = (JSON.parse(asked.result.content[0].text) as { id: number }).id;
  expect(id).toBeGreaterThan(0);

  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
  await page.goto(origin + '/attention');
  const item = page.locator(`#ask-${id}`);
  await expect(item.locator('[data-attention-text]')).toHaveText('Merge the schema change?');
  await expect(item.locator('[data-attention-source]')).toContainText('@ ' + host);
  const waited = localCall('attention_wait', { id, wait_seconds: 60 }, auth).then((r) => r.json());
  await Promise.all([page.waitForURL(origin + '/attention'), item.getByRole('button', { name: 'hold' }).click()]);
  const got = (await waited) as { result: { content: { text: string }[] } };
  expect(JSON.parse(got.result.content[0].text)).toMatchObject({ id, status: 'answered', answer: 'hold' });

  // The session in the window's OTHER pane asks too: it is this host's own.
  const split = (await (await localCall('attention_ask', { question: 'From the split pane?' }, { ...auth, 'X-Muster-Pane': pane + '-split' })).json()) as {
    result: { content: { text: string }[] };
  };
  const splitId = (JSON.parse(split.result.content[0].text) as { id: number }).id;
  expect(splitId).toBeGreaterThan(0);
  await page.reload();
  await expect(page.locator(`#ask-${splitId} [data-attention-source]`)).toContainText('@ ' + host);
});

// ANSWERED IN THE SESSION: the mirrored session asks and waits; the operator,
// with the queue open, writes to that session from its rendered view instead
// of answering in the queue. The ask leaves the open queue without a reload,
// and the session's wait returns answered_in_session with the message.
test('a message sent to the asking session answers its ask, live', async ({ page }) => {
  test.setTimeout(120_000);
  const secret = fs.readFileSync(mcpSecretFile, 'utf8').trim();
  const auth = { Authorization: 'Bearer ' + secret, 'X-Muster-Pane': '%e2e-work-@1', 'X-Muster-Tmux': fakeTMUX };
  const asked = (await (await localCall('attention_ask', { question: 'Tag the release latest too?' }, auth)).json()) as {
    result: { content: { text: string }[] };
  };
  const id = (JSON.parse(asked.result.content[0].text) as { id: number }).id;
  expect(id).toBeGreaterThan(0);
  const waited = localCall('attention_wait', { id, wait_seconds: 90 }, auth).then((r) => r.json());

  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
  await page.goto(origin + '/attention');
  const item = page.locator(`#ask-${id}`);
  await expect(item.locator('[data-attention-text]')).toHaveText('Tag the release latest too?');
  // Newest first: this ask heads the list.
  await expect(page.locator('[data-attention-item]').first()).toHaveAttribute('id', `ask-${id}`);

  // The operator answers in the session: its rendered view, in another tab.
  // 🔴 The session is BUSY while it asks — a turn is running attention_wait —
  // as a real one is. A window WAITING on the person (a permission prompt, a
  // question) takes no Send at all: Enter there would answer the prompt (the
  // Claude prompt guard), so the fixture's waiting status is lifted first and
  // put back for the session check below.
  writeFileAtomic(tr + '.status', JSON.stringify({ status: 'busy', statusUpdatedAt: Date.now() }));
  await expect(page.locator('[data-attention-item][data-kind="session"]'), 'muster saw it busy').toHaveCount(0, { timeout: 15_000 });
  const view = await page.context().newPage();
  await view.goto(origin + `/mirror/view?host=${encodeURIComponent(host)}&session=work&window=%401`);
  await expect(view.locator('#mv-input'), mirrorLog).toBeEnabled({ timeout: 20_000 });
  await view.locator('#mv-input').fill('only next for now');
  await view.locator('#mv-send').click();
  await expect(view.locator('#mv-input')).toHaveValue('');

  // The queue tab, untouched: the ask leaves it live.
  await expect(item).toHaveCount(0, { timeout: 15_000 });
  const got = (await waited) as { result: { content: { text: string }[] } };
  expect(JSON.parse(got.result.content[0].text)).toMatchObject({ id, status: 'answered_in_session', answer: 'only next for now' });
  await view.close();

  // A waiting SESSION clears itself the same way, through its status: when
  // Claude Code stops waiting, the host's next report (its refresh, ~5 s)
  // takes the item out of the open queue — measured, not assumed.
  const session = page.locator('[data-attention-item][data-kind="session"]');
  writeFileAtomic(tr + '.status', JSON.stringify({ status: 'waiting', statusUpdatedAt: Date.now() - 30_000 }));
  await expect(session).toHaveCount(1, { timeout: 15_000 });
  writeFileAtomic(tr + '.status', JSON.stringify({ status: 'busy', statusUpdatedAt: Date.now() }));
  const flipped = Date.now();
  await expect(session).toHaveCount(0, { timeout: 15_000 });
  const took = Date.now() - flipped;
  console.log(`attention: a session that stopped waiting left the open queue in ${took} ms`);
  expect(took, 'the waiting window left within one host refresh and the queue nudge').toBeLessThan(12_000);
});

// A QUEUED message answers the ask too — when it is queued (a session blocked
// on the answer is busy, and a queued message waits for idle). The queue row
// says so, and cancelling it warns that the answer cannot be un-sent.
test('a queued message answers the ask, and its Cancel says the answer stays', async ({ page }) => {
  test.setTimeout(120_000);
  const secret = fs.readFileSync(mcpSecretFile, 'utf8').trim();
  const auth = { Authorization: 'Bearer ' + secret, 'X-Muster-Pane': '%e2e-work-@1', 'X-Muster-Tmux': fakeTMUX };
  const asked = (await (await localCall('attention_ask', { question: 'Bump the minor version?' }, auth)).json()) as {
    result: { content: { text: string }[] };
  };
  const id = (JSON.parse(asked.result.content[0].text) as { id: number }).id;
  const waited = localCall('attention_wait', { id, wait_seconds: 90 }, auth).then((r) => r.json());

  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
  await page.goto(origin + `/mirror/view?host=${encodeURIComponent(host)}&session=work&window=%401`);
  await expect(page.locator('#mv-input'), mirrorLog).toBeEnabled({ timeout: 20_000 });
  await page.locator('#mv-input').fill('yes, a minor bump');
  await page.locator('#mv-send-more').click();
  await page.locator('#mv-queue-send').click();
  await expect(page.locator('#mv-note')).toHaveText('Queued.');

  const got = (await waited) as { result: { content: { text: string }[] } };
  expect(JSON.parse(got.result.content[0].text)).toMatchObject({ id, status: 'answered_in_session', answer: 'yes, a minor bump' });
  const row = page.locator('#mv-queue [data-queue-id]', { hasText: 'yes, a minor bump' });
  await expect(row.locator('[data-queue-answered]')).toHaveText(/^Already answered .+'s question\.$/, { timeout: 20_000 });
  // Cancel is in the row's ⋯ menu.
  await row.getByRole('button', { name: 'More actions for this queued message' }).click();
  await row.getByRole('menuitem', { name: 'Cancel this queued message' }).click();
  await expect(page.locator('#mv-note')).toHaveText(/^Cancelled — but this message had already answered .+'s question, and that answer was delivered: it can't be un-sent\.$/);
  await expect(row).toHaveAttribute('data-queue-state', 'cancelled');
});

// ANSWERED WHERE THE NOTIFICATION LANDS (attention_asker.go): the
// notification's URL, /attention/open/<key>, resolves a mirrored session's
// ask to that session's rendered view when tapped (attention_push_target.go). The view
// shows the ask with the queue's own form; a second ask arrives live; the
// operator answers there, lands back on the view, and the session's wait
// returns the answer. On a phone the block stays above the pinned composer.
for (const vp of [
  { name: 'phone', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(`on a ${vp.name}`, () => {
    test.use(vp.use);
    test('a mirrored session\'s ask is answered on its own rendered view', async ({ page }) => {
      test.setTimeout(120_000);
      // The host writes its local secret once it is up (a fresh worker may run
      // this test first).
      await expect.poll(() => fs.existsSync(mcpSecretFile), { timeout: 30_000 }).toBe(true);
      const secret = fs.readFileSync(mcpSecretFile, 'utf8').trim();
      const auth = { Authorization: 'Bearer ' + secret, 'X-Muster-Pane': '%e2e-work-@1', 'X-Muster-Tmux': fakeTMUX };
      const askLocal = async (question: string, options: string[] = []) => {
        const r = (await (await localCall('attention_ask', { question, options }, auth)).json()) as { result: { content: { text: string }[] } };
        return (JSON.parse(r.result.content[0].text) as { id: number }).id;
      };
      const id = await askLocal(`Merge on the ${vp.name}?`, ['merge', 'hold']);

      await page.goto(origin + '/login');
      await page.fill('#muster-password', password);
      await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
      // The notification's URL (the tap route) resolves to the window's view.
      const view = origin + `/mirror/view?host=${encodeURIComponent(host)}&session=work&window=%401`;
      await page.goto(origin + `/attention/open/ask-${id}`);
      await page.waitForURL(view);
      const block = page.locator('#asker-asks');
      const item = block.locator(`#ask-${id}`);
      await expect(item.locator('[data-attention-text]')).toHaveText(`Merge on the ${vp.name}?`);
      await expect(item.getByRole('button', { name: 'merge', exact: true })).toBeVisible();

      // A second ask, raised while the view is open, arrives without a reload.
      const id2 = await askLocal(`And the changelog on the ${vp.name}?`);
      await expect(block.locator(`#ask-${id2}`)).toBeVisible({ timeout: 15_000 });

      // The layout holds: the block sits above the pinned composer, which is
      // still inside the visible viewport, and the conversation keeps room.
      const size = page.viewportSize()!;
      const compose = (await page.locator('#mv-compose').boundingBox())!;
      const asks = (await block.boundingBox())!;
      const log = (await page.locator('#mv-log').boundingBox())!;
      expect(compose.y + compose.height, 'the composer is on screen').toBeLessThanOrEqual(size.height + 1);
      expect(asks.y + asks.height, 'the asks block ends above the composer').toBeLessThanOrEqual(compose.y + 1);
      expect(log.height, 'the conversation keeps room').toBeGreaterThan(80);

      // Answer by an option: back on this view, the session's wait gets it, the ask leaves.
      const waited = localCall('attention_wait', { id, wait_seconds: 60 }, auth).then((r) => r.json());
      await Promise.all([page.waitForURL(view), item.getByRole('button', { name: 'hold', exact: true }).click()]);
      const got = (await waited) as { result: { content: { text: string }[] } };
      expect(JSON.parse(got.result.content[0].text)).toMatchObject({ id, status: 'answered', answer: 'hold' });
      await expect(block.locator(`#ask-${id}`)).toHaveCount(0);

      // Free text on the second: it leaves too.
      const second = block.locator(`#ask-${id2}`);
      await second.locator('textarea').fill('yes, add it');
      await Promise.all([page.waitForURL(view), second.getByRole('button', { name: 'Answer', exact: true }).click()]);
      const w2 = (await (await localCall('attention_wait', { id: id2, wait_seconds: 0 }, auth)).json()) as { result: { content: { text: string }[] } };
      expect(JSON.parse(w2.result.content[0].text)).toMatchObject({ status: 'answered', answer: 'yes, add it' });
      await expect(block.locator(`#ask-${id2}`)).toHaveCount(0);
    });
  });
}

// HIDE UNTIL NEXT ASK, on the Rendered view (ui/attention.go): the operator's
// "close it without taking any action". The block hides here and NOTHING is
// sent — no request of any kind, so the session's wait still says pending and
// the ask is still in the queue; an "N waiting" chip takes its place and
// reopens it; a NEW ask (arriving live, by the queue's SSE event) brings the
// block back by itself. 🔴 A REGRESSION TEST: at the base the block has no hide.
test('the rendered view\'s asks hide until the next ask, without answering or sending anything', async ({ page }) => {
  test.setTimeout(120_000);
  await expect.poll(() => fs.existsSync(mcpSecretFile), { timeout: 30_000 }).toBe(true);
  const secret = fs.readFileSync(mcpSecretFile, 'utf8').trim();
  const auth = { Authorization: 'Bearer ' + secret, 'X-Muster-Pane': '%e2e-work-@1', 'X-Muster-Tmux': fakeTMUX };
  const askLocal = async (question: string, options: string[] = []) => {
    const r = (await (await localCall('attention_ask', { question, options }, auth)).json()) as { result: { content: { text: string }[] } };
    return (JSON.parse(r.result.content[0].text) as { id: number }).id;
  };
  const status = async (id: number) => {
    const r = (await (await localCall('attention_wait', { id, wait_seconds: 0 }, auth)).json()) as { result: { content: { text: string }[] } };
    return (JSON.parse(r.result.content[0].text) as { status: string }).status;
  };
  const id = await askLocal('Rotate the staging key?', ['rotate', 'later']);

  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
  const view = origin + `/mirror/view?host=${encodeURIComponent(host)}&session=work&window=%401`;
  await page.goto(view);
  const block = page.locator('#asker-asks');
  const chip = page.locator('#asker-asks-chip');
  await expect(block.locator(`#ask-${id}`)).toBeVisible();
  // The window may hold an earlier test's open ask too (the split pane's): the
  // chip counts what the block holds.
  const n = await block.locator('[data-attention-item]').count();
  expect(n).toBeGreaterThanOrEqual(1);

  const sent: string[] = [];
  page.on('request', (r) => {
    if (r.resourceType() !== 'eventsource' && r.resourceType() !== 'websocket') sent.push(`${r.method()} ${new URL(r.url()).pathname}`);
  });
  await block.getByRole('button', { name: 'Hide until next ask' }).click();
  await expect(block).toBeHidden();
  await expect(chip).toHaveText(`${n} waiting`);
  await page.waitForTimeout(500);
  expect(sent.filter((s) => !s.startsWith('GET /mirror/view/asks')), 'the hide sent nothing (the asks poll aside, a GET)').toEqual([]);
  expect(sent.filter((s) => !s.startsWith('GET ')), 'no POST of any kind').toEqual([]);
  expect(await status(id), 'the session still waits: nothing answered or cancelled it').toBe('pending');
  expect(await (await page.request.get(origin + '/attention')).text(), 'still in the Asks queue').toContain(`id="ask-${id}"`);

  // A new ask arrives live: the block comes back by itself, with both.
  const id2 = await askLocal('And the prod key?');
  await expect(block.locator(`#ask-${id2}`)).toBeAttached({ timeout: 15_000 });
  await expect(block).toBeVisible();
  await expect(chip).toBeHidden();
  // Hidden again, then reopened by the chip.
  await block.getByRole('button', { name: 'Hide until next ask' }).click();
  await expect(chip).toHaveText(`${n + 1} waiting`);
  await chip.click();
  await expect(block).toBeVisible();
  await expect(chip).toBeHidden();
  expect(sent.filter((s) => !s.startsWith('GET ')), 'still no POST').toEqual([]);

  // Cleanup by the block's own answer forms (the queue is shared by the spec).
  await Promise.all([page.waitForURL(view), block.locator(`#ask-${id}`).getByRole('button', { name: 'later', exact: true }).click()]);
  const second = block.locator(`#ask-${id2}`);
  await second.locator('textarea').fill('not now');
  await Promise.all([page.waitForURL(view), second.getByRole('button', { name: 'Answer', exact: true }).click()]);
  expect(await status(id2)).toBe('answered');
});
