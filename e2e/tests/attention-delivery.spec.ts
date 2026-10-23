import http from 'node:http';
import { execFileSync } from 'node:child_process';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { attentionDeliveryConnectPort, attentionDeliveryFakePort, attentionDeliveryPort, dsn, password, repoRoot, versionTerm } from '../lib/env';
import { startServer, Running } from '../lib/server';

// AN ANSWER IS DELIVERED TO THE CHIEF THAT ASKED (internal/api/attention_delivery.go),
// the operator's click path, every muster hop real:
//
//   the chief asks over the real /mcp door (attention_ask, with options) and
//   does NOT wait → /attention lists it → the operator taps an option → muster
//   pushes the answer into the chief's default session as a turn, through the
//   real agent gateway, to a FAKE RUNTIME this spec runs (the e2eterm build's
//   MUSTER_E2E_AGENT_ENDPOINT points every agent endpoint at it) → the item's
//   Answered entry says "Delivered to chief" → the chief panel shows the
//   delivered answer and the chief's reply.
//
// ⚠ WHAT IS PLAYED: the runtime (it answers every /v1/responses turn with one
// fixed sentence and records what it was sent) and the chief's pod (a fixture
// row, e2e/pluginfix chief). The ask, the answer form, the delivery, the gateway
// request, the stored turn and the panel are muster's own.

const origin = `http://localhost:${attentionDeliveryPort}`;
const connectURL = `http://127.0.0.1:${attentionDeliveryConnectPort}`;
const stamp = Date.now().toString(36);
const chiefName = 'e2e-deliv-chief-' + stamp;
const chiefToken = 'e2e-deliv-chief-token-not-a-real-one-' + stamp;
const REPLY = 'On it: starting a new agent on the task.';

let server: Running | undefined;
let runtime: http.Server | undefined;
let chiefMade = false;
const turns: string[] = [];

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

// sse is one Server-Sent Event of the /v1/responses stream.
const sse = (type: string, data: Record<string, unknown>) => `event: ${type}\ndata: ${JSON.stringify({ type, ...data })}\n\n`;

test.beforeAll(async () => {
  test.setTimeout(180_000);
  runtime = http.createServer((req, res) => {
    let body = '';
    req.on('data', (d) => (body += d));
    req.on('end', () => {
      if (req.method !== 'POST' || !(req.url || '').endsWith('/v1/responses')) {
        res.statusCode = 404;
        return res.end();
      }
      // The turn's user message, as the gateway sent it (/v1/responses `input`).
      const input = (JSON.parse(body) as { input?: { content?: string }[] }).input ?? [];
      turns.push(input.map((m) => m.content ?? '').join('\n'));
      res.setHeader('Content-Type', 'text/event-stream');
      res.write(sse('response.output_text.delta', { delta: REPLY, output_index: 0 }));
      res.end(
        sse('response.completed', {
          response: { status: 'completed', output: [{ type: 'message', role: 'assistant', content: [{ type: 'output_text', text: REPLY }] }] },
        }),
      );
    });
  });
  await new Promise<void>((r) => runtime!.listen(attentionDeliveryFakePort, '127.0.0.1', () => r()));
  server = await startServer(versionTerm, attentionDeliveryPort, {
    MUSTER_FOCUS_INTERVAL: '0',
    // Plugins on, so the page is the TABBED Asks page: the Answered section
    // must be on its Ask tab (asks_drafts.go). No plugin is ever created.
    MUSTER_PLUGINS: '1',
    MUSTER_CONNECTOR_ENCRYPTION_KEY: Buffer.alloc(32, 0x4e).toString('base64'),
    MUSTER_CONNECT_PORT: String(attentionDeliveryConnectPort),
    MUSTER_AGENT_CONNECT_URL: connectURL,
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
    MUSTER_AGENT_GATEWAY: 'hooks-sha256',
    MUSTER_AGENT_GATEWAY_MODEL: 'e2e-sentinel',
    // The runtime bundle the gateway needs at boot (nothing is provisioned).
    MUSTER_AGENT_RUNTIME_CONFIG: '{}',
    MUSTER_AGENT_RUNTIME_INSTALL: 'exec true',
    MUSTER_E2E_AGENT_ENDPOINT: `http://127.0.0.1:${attentionDeliveryFakePort}`,
  });
  fix('chief', '1', chiefName, chiefToken);
  chiefMade = true;
});

test.afterAll(async () => {
  try {
    if (chiefMade) fix('cleanup', '1', chiefName);
  } finally {
    await server?.stop();
    await new Promise<void>((r) => (runtime ? runtime.close(() => r()) : r()));
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

async function openChiefPanel(page: Page) {
  await page.evaluate(() => {
    const b = document.createElement('button');
    b.setAttribute('data-chief-panel-toggle', '');
    b.style.cssText = 'position:fixed;left:-9999px;top:0';
    document.body.appendChild(b);
    b.click();
  });
  await expect(page.locator('#chief-panel')).toHaveAttribute('data-chief-panel-open', '1');
}

test('the operator answers the chief on /attention and the chief\'s session receives the answer as a turn, once', async ({ page }) => {
  test.setTimeout(120_000);
  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);

  // The chief asks and does not wait: its tool result says it need not.
  const asked = await mcpCall('attention_ask', { question: 'Who should take task 42?', options: ['forward it', 'dispatch a new agent'] });
  expect(asked.isError, asked.text).toBe(false);
  const { id, next } = JSON.parse(asked.text) as { id: number; next: string };
  expect(next).toContain('you do not need to wait');

  await page.goto(origin + '/attention');
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Asks');
  await expect(page.getByRole('tab', { name: 'Ask', exact: true })).toHaveAttribute('aria-selected', 'true');
  await expect(page.getByRole('tab', { name: 'Reply drafts', exact: true })).toHaveAttribute('aria-selected', 'false');
  const item = page.locator(`#ask-${id}`);
  await expect(item.locator('[data-attention-text]')).toHaveText('Who should take task 42?');
  await Promise.all([page.waitForURL(origin + '/attention'), item.getByRole('button', { name: 'dispatch a new agent' }).click()]);
  await expect(page.locator(`#ask-${id}`)).toHaveCount(0);

  // Where the answer went, live on the page (after the waiter's grace).
  const answered = page.locator(`#answered-${id}`);
  await expect(answered.locator('[data-attention-answer-text]')).toHaveText('Your answer: dispatch a new agent');
  await expect(answered).toHaveAttribute('data-delivery', 'delivered', { timeout: 30_000 });
  await expect(answered.locator('[data-attention-delivery]')).toHaveText('Delivered to chief as a new message in its session.');
  // The Answered section is the Ask tab's: visible there, and hidden with it.
  await expect(page.locator('#asks-tabs-panel-ask').locator(`#answered-${id}`)).toBeVisible();
  await page.getByRole('tab', { name: 'Reply drafts', exact: true }).click();
  await expect(answered).toBeHidden();
  await expect(page.locator('#asks-drafts-pending [data-drafts-empty]')).toBeVisible();
  await expect(page).toHaveURL(origin + '/attention?tab=drafts');

  // The runtime got exactly one turn carrying it, fenced.
  const carrying = turns.filter((b) => b.includes(`your owner answered attention #${id}`));
  expect(carrying, `turns the runtime received: ${turns.length}`).toHaveLength(1);
  expect(carrying[0]).toContain('<<<attention-answer untrusted nonce=');
  expect(carrying[0]).toContain('dispatch a new agent');

  // The chief panel shows the delivered answer and the chief's reply.
  await page.goto(origin + '/tasks');
  await openChiefPanel(page);
  const log = page.locator('#chief-panel #chat-log');
  await expect(log).toContainText(`your owner answered attention #${id}`, { timeout: 15_000 });
  await expect(log).toContainText(REPLY);
});
