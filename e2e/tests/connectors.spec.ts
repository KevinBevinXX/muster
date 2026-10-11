import http from 'node:http';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { agentToken, connectPort, connectorsPort, fakeUpstreamPort, password, versionTerm } from '../lib/env';
import { startServer, Running } from '../lib/server';

// The secure connector store, end to end, at a phone's width (412×915): an
// AGENT asks to use a connector nobody has configured, the OPERATOR is prompted
// and enters the credential, and the agent's proxied call then succeeds against a
// FAKE upstream — with the credential injected by muster, never seen by the
// agent, and never echoed back to it.
//
// The server is the e2eterm build (cmd/muster-server/e2econnect.go): its proxy
// may reach `*.e2e.test` on loopback over plain HTTP. Everything else — the
// agent's token check, the grant, the operator's form, the allowlist, the header
// handling and the redaction — is the shipped code. The upstream is a node
// server in this file; no real platform, no real credential.
test.use({ viewport: { width: 412, height: 915 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

const origin = `http://localhost:${connectorsPort}`;
const connectBase = `http://localhost:${connectPort}`;
const upstreamHost = `api.fake.e2e.test:${fakeUpstreamPort}`;
const FAKE_SECRET = 'e2e-FAKE-upstream-token-0123456789';
// 32 bytes of 0x2a, base64: an obviously-synthetic key for a throwaway server.
const FAKE_KEY = Buffer.alloc(32, 0x2a).toString('base64');

interface Seen {
  method: string;
  url: string;
  headers: http.IncomingHttpHeaders;
}
const seen: Seen[] = [];
let upstream: http.Server | undefined;
let server: Running | undefined;

test.beforeAll(async () => {
  upstream = http.createServer((req, res) => {
    seen.push({ method: req.method ?? '', url: req.url ?? '', headers: req.headers });
    res.setHeader('Content-Type', 'application/json');
    // It ECHOES the Authorization it received: the proxy must redact it.
    res.end(JSON.stringify({ ok: true, path: req.url, echoedAuth: req.headers.authorization ?? null }));
  });
  await new Promise<void>((r) => upstream!.listen(fakeUpstreamPort, '127.0.0.1', () => r()));
  server = await startServer(versionTerm, connectorsPort, {
    MUSTER_CONNECTOR_ENCRYPTION_KEY: FAKE_KEY,
    MUSTER_CONNECT_PORT: String(connectPort),
    // The store refuses to boot without the URL agents are told to use.
    MUSTER_AGENT_CONNECT_URL: `http://127.0.0.1:${connectPort}`,
  });
});
test.afterAll(async () => {
  await server?.stop();
  await new Promise<void>((r) => (upstream ? upstream.close(() => r()) : r()));
});

async function asAgent(path: string, init: RequestInit = {}): Promise<{ status: number; text: string }> {
  const r = await fetch(connectBase + path, {
    ...init,
    headers: { 'X-Muster-Token': agentToken, ...(init.headers ?? {}) },
  });
  return { status: r.status, text: await r.text() };
}

async function login(page: Page) {
  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
}

async function noHorizontalScroll(page: Page) {
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw, 'document scrollWidth').toBeLessThanOrEqual(cw);
}

test('an agent requests a connector, the operator enters the credential, the proxied call succeeds', async ({ page }) => {
  const proxied = `/connect/fakehub/${upstreamHost}/repos/example/site?per_page=3`;

  // 1. The agent asks before anything exists: refused, and a request is filed.
  const first = await asAgent(proxied);
  expect(first.status, first.text).toBe(403);
  expect(JSON.parse(first.text).grant).toBe('pending');
  expect(seen.length, 'the upstream is untouched without a grant').toBe(0);

  // The shared hook token is not an agent credential, even on the connect port.
  const shared = await fetch(connectBase + proxied, { headers: { 'X-Muster-Token': 'e2e-hook-token-not-a-real-one' } });
  expect(shared.status).toBe(403);
  // And the connect port serves nothing but the connector surface.
  expect((await fetch(connectBase + '/ui/connectors')).status).toBe(404);

  // 2. The operator is prompted on the Connectors tab.
  await login(page);
  await page.goto(origin + '/connectors');
  const card = page.locator('[data-connector-request="fakehub"]');
  await expect(card).toBeVisible();
  await expect(card).toContainText('docs-sweeper');
  await noHorizontalScroll(page);

  // 3. ... and enters the credential, creating the connector and approving the
  // agent in one step.
  await card.locator('[data-enter-credential]').click();
  const form = page.locator('#connector-form');
  await expect(form).toBeVisible();
  await expect(form.locator('#connector-name')).toHaveValue('fakehub');
  await form.locator('#connector-hosts').fill(upstreamHost);
  await form.locator('#connector-scheme').selectOption('bearer');
  await form.locator('#connector-secret').fill(FAKE_SECRET);
  await noHorizontalScroll(page);
  const save = form.locator('[data-save-connector]');
  const sb = (await save.boundingBox())!;
  expect(sb.height, 'Save is thumb-sized').toBeGreaterThanOrEqual(44);
  expect(sb.x + sb.width, 'Save fits the viewport').toBeLessThanOrEqual(412);
  await save.click();
  await expect(page.locator('#connectors-notice')).toContainText('docs-sweeper approved');
  await expect(page.locator('[data-connector="fakehub"]')).toBeVisible();
  // No fragment of the secret is shown back (the masked hint was removed 2026-10-10).
  await expect(page.locator('[data-connector="fakehub"] [data-rotated]')).toContainText('rotated never');
  await expect(page.locator('[data-secret-hint]')).toHaveCount(0);
  await expect(page.locator('[data-request-card], [data-connector-request="fakehub"]')).toHaveCount(0);
  expect(await page.content(), 'the page never shows the secret').not.toContain(FAKE_SECRET);
  await noHorizontalScroll(page);

  // 4. The agent's proxied call now succeeds against the fake upstream.
  const ok = await asAgent(proxied);
  expect(ok.status, ok.text).toBe(200);
  expect(seen.length).toBe(1);
  expect(seen[0].url).toBe('/repos/example/site?per_page=3');
  expect(seen[0].headers.authorization, 'muster injected the credential').toBe('Bearer ' + FAKE_SECRET);
  expect(seen[0].headers['x-muster-token'], "the agent's own token never reaches the platform").toBeUndefined();
  // The upstream echoed the credential back; the agent receives it redacted.
  expect(ok.text).not.toContain(FAKE_SECRET);
  expect(JSON.parse(ok.text).echoedAuth).toBe('Bearer [REDACTED-BY-MUSTER]');

  // 5. A host outside the allowlist is refused, and the upstream sees nothing.
  const off = await asAgent(`/connect/fakehub/other.fake.e2e.test:${fakeUpstreamPort}/x`);
  expect(off.status).toBe(403);
  expect(seen.length).toBe(1);

  // 6. The operator sees the use: last-used and the audit line.
  await page.goto(origin + '/connectors');
  await expect(page.locator('[data-connector="fakehub"] [data-last-used]')).not.toContainText('never');
  await expect(page.locator('#connector-audit')).toContainText('GET ' + upstreamHost + '/repos/example/site → 200');
  await expect(page.locator('[data-grant="docs-sweeper"] [data-grant-status]')).toHaveText('approved');
  await noHorizontalScroll(page);

  // 7. Revoking stops it.
  page.once('dialog', (d) => d.accept());
  await page.locator('[data-revoke="docs-sweeper"]').click();
  await expect(page.locator('#connectors-notice')).toContainText('revoked');
  const after = await asAgent(proxied);
  expect(after.status).toBe(403);
  expect(JSON.parse(after.text).grant).toBe('revoked');
  expect(seen.length).toBe(1);
});
