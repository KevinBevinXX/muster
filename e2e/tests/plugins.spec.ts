import { execFileSync } from 'node:child_process';
import crypto from 'node:crypto';
import path from 'node:path';
import { APIRequestContext, BrowserContext, Page, request as pwRequest } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import {
  dsn, e2eRoot, gotrueSecret, hookToken, pluginsBrowserPort, pluginsConnectPort, pluginsGotruePort, pluginsMainPort,
  repoRoot, versionA,
} from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { authenticator } from '../lib/webauthn';

// THE PLUGIN INTERFACE, END TO END: a FAKE PLUGIN (a Playwright request context
// holding the plugin token, as the real plugin process would) against a
// gotrue-mode muster with MUSTER_PLUGINS=1, its own real GoTrue (passkeys on),
// and the connector store on (so the chief's /mcp door exists).
//
//   mint on /plugins → the hook token is refused on the plugin door (negative
//   control) → POST a message twice (202, then 200 with the same id) → the
//   chief drafts a reply over /mcp → the owner, on a STALE sign-in, taps Approve
//   and confirms with a PASSKEY → the plugin claims it once → reports sent → the
//   card says Sent → a second draft's lease lapses → unconfirmed, not
//   re-offered, "Approve again" offered → a no_reply message's draft is refused.
//
// ⚠ WHAT IS PLAYED, AND WHY. The e2e harness runs no agent runtime, so the
// delivery TURN is not run here: `go run ./e2e/pluginfix deliver` records what a
// completed delivery records. The turn itself — fenced, through the real agent
// gateway — is the Go seam tests' (internal/api/plugins_seam_pg_test.go,
// plugin_drafts_pg_test.go). The chief is a fixture row for the same reason; its
// /mcp calls, the approval, the claim and the result are all the real routes.
//
// 🔴 ONLY THIS SPEC'S SERVER HAS PLUGINS ON, and afterAll removes the chief and
// the plugins it made, so the specs after it on the shared database see neither.

const compose = path.join(e2eRoot, 'gotrue', 'compose.yml');
const project = `muster-e2e-gotrue-${pluginsGotruePort}`;
const composeEnv = {
  ...process.env,
  MUSTER_E2E_GOTRUE_PROJECT: project,
  MUSTER_E2E_GOTRUE_PORT: String(pluginsGotruePort),
  MUSTER_E2E_GOTRUE_SITE: `http://localhost:${pluginsBrowserPort}`,
  MUSTER_E2E_GOTRUE_PASSKEYS: 'true',
};
const browserURL = `http://localhost:${pluginsBrowserPort}`;
const machineURL = `http://localhost:${pluginsMainPort}`;
const connectURL = `http://127.0.0.1:${pluginsConnectPort}`;
const gotrueURL = `http://127.0.0.1:${pluginsGotruePort}`;
const adminEmail = `plugins-admin-${Date.now()}@example.com`;
const adminPW = 'e2e-plugins-password-not-a-real-one';
const chiefName = 'e2e-plugins-chief-' + Date.now().toString(36);
const chiefToken = 'e2e-plugins-chief-token-not-a-real-one-' + Date.now().toString(36);
// 32 bytes of 0x2b, base64: an obviously-synthetic connector key for a throwaway server.
const FAKE_KEY = Buffer.alloc(32, 0x2b).toString('base64');

let server: Running | undefined;
let plugin: APIRequestContext | undefined;

test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(300_000);
  execFileSync('docker', ['compose', '-f', compose, 'up', '-d', '--wait'], { env: composeEnv, stdio: 'inherit' });
  execFileSync('go', ['run', './e2e/seed', '--accounts-only'], { cwd: repoRoot, stdio: 'inherit', env: { ...process.env, DATABASE_URL: dsn } });
  server = await startServer(versionA, pluginsMainPort, {
    MUSTER_AUTH_MODE: 'gotrue',
    MUSTER_GOTRUE_URL: gotrueURL,
    MUSTER_GOTRUE_JWT_SECRET: gotrueSecret,
    MUSTER_BROWSER_PORT: String(pluginsBrowserPort),
    MUSTER_PUBLIC_URL: browserURL,
    MUSTER_PLUGINS: '1',
    MUSTER_CONNECTOR_ENCRYPTION_KEY: FAKE_KEY,
    MUSTER_CONNECT_PORT: String(pluginsConnectPort),
    MUSTER_AGENT_CONNECT_URL: connectURL,
  });
});

test.afterAll(async () => {
  await plugin?.dispose();
  try {
    fix('cleanup', '1', chiefName);
  } finally {
    await server?.stop();
    execFileSync('docker', ['compose', '-f', compose, 'down', '-v'], { env: composeEnv, stdio: 'inherit' });
  }
});

// fix runs the spec's fixture command (e2e/pluginfix) and returns its output.
function fix(...args: string[]): string {
  return execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } })
    .toString().trim();
}

const serverEnv = () => ({ ...process.env, DATABASE_URL: dsn, MUSTER_PUBLIC_URL: browserURL, MUSTER_GOTRUE_URL: gotrueURL, MUSTER_GOTRUE_JWT_SECRET: gotrueSecret });

async function claimAndSignIn(page: Page) {
  const link = execFileSync(serverBinary(versionA), ['invite', '--claim-admin'], { env: serverEnv() }).toString().trim();
  await page.goto(link);
  await page.fill('#claim-email', adminEmail);
  await page.fill('#claim-password', adminPW);
  await page.fill('#claim-password2', adminPW);
  await Promise.all([page.waitForURL(browserURL + '/'), page.click('button[type=submit]')]);
}

// makeStale re-signs the access cookie as the same session signed in 13 hours
// ago (mirror-stepup.spec.ts explains why this IS the token GoTrue would mint).
async function makeStale(ctx: BrowserContext) {
  const c = (await ctx.cookies(browserURL)).find((k) => k.name === 'muster_at');
  expect(c, 'a session cookie').toBeTruthy();
  const claims = JSON.parse(Buffer.from(c!.value.split('.')[1], 'base64url').toString('utf8'));
  const now = Math.floor(Date.now() / 1000);
  claims.amr = [{ method: 'password', timestamp: now - 13 * 3600 }];
  claims.exp = now + 600;
  const h = Buffer.from(JSON.stringify({ alg: 'HS256', typ: 'JWT' })).toString('base64url');
  const p = Buffer.from(JSON.stringify(claims)).toString('base64url');
  const s = crypto.createHmac('sha256', gotrueSecret).update(h + '.' + p).digest('base64url');
  await ctx.addCookies([{ name: 'muster_at', value: `${h}.${p}.${s}`, url: browserURL, httpOnly: true, secure: true, sameSite: 'Lax' }]);
}

// mcpCall is the chief calling one tool over the real /mcp door.
async function mcpCall(name: string, args: Record<string, unknown>): Promise<{ text: string; isError: boolean; rpcError?: unknown }> {
  const r = await fetch(connectURL + '/mcp', {
    method: 'POST',
    headers: { 'X-Muster-Token': chiefToken, 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream' },
    body: JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'tools/call', params: { name, arguments: args } }),
  });
  expect(r.status, 'the /mcp door answered').toBe(200);
  const j = (await r.json()) as { result?: { content: { text: string }[]; isError?: boolean }; error?: unknown };
  if (j.error) return { text: '', isError: true, rpcError: j.error };
  return { text: j.result!.content[0].text, isError: !!j.result!.isError };
}

const card = (page: Page, id: number) => page.locator(`#draft-${id}`);

test('a plugin posts, the chief drafts, the owner approves with a passkey on a stale sign-in, the plugin sends it once (real GoTrue, 390px)', async ({ page, context }) => {
  test.setTimeout(240_000);
  // Chromium's virtual authenticator: the passkey the confirm uses.
  await authenticator(page);
  await claimAndSignIn(page);

  // A passkey for the confirm (a sign-in from the last five minutes is needed to
  // add one, which the claim just was).
  await page.goto(browserURL + '/account/passkeys');
  await page.locator('#passkey-add').click();
  await expect(page.locator('#passkey-list [data-passkey-id]')).toHaveCount(1);

  // 1. Mint the plugin's token on /plugins.
  await page.goto(browserURL + '/plugins');
  await page.fill('#plugin-name', 'example-inbox');
  await page.fill('#plugin-label', 'Example inbox');
  await page.fill('#plugin-scope', 'example-inbox');
  await page.getByRole('button', { name: 'Create plugin token' }).click();
  const token = await page.locator('#minted-plugin-token').inputValue();
  expect(token).toMatch(/^mpl_[A-Za-z0-9_-]{43}$/);
  plugin = await pwRequest.newContext({ baseURL: machineURL, extraHTTPHeaders: { Authorization: `Bearer ${token}` } });

  // 2. NEGATIVE CONTROL: the shared hook token is not a plugin credential.
  const hook = await fetch(machineURL + '/plugin/v1/messages', {
    method: 'POST', headers: { Authorization: `Bearer ${hookToken}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ external_id: 'never', body: 'x' }),
  });
  expect(hook.status, 'the hook token on the plugin door').toBe(401);
  const hookClaim = await fetch(machineURL + '/plugin/v1/drafts/claim', { method: 'POST', headers: { Authorization: `Bearer ${hookToken}` }, body: '{}' });
  expect(hookClaim.status, 'the hook token on the claim route').toBe(401);

  // 3. The plugin posts a message, then posts it again: one row.
  const message = { external_id: 'e2e-msg-1', sender: 'Ada <ada@example.invalid>', subject: 'lunch?', body: 'Are you free at noon? <b>bold</b>', received_at: '2000-01-02T03:04:05Z' };
  const first = await plugin.post('/plugin/v1/messages', { data: message });
  expect(first.status(), await first.text()).toBe(202);
  const firstBody = (await first.json()) as { id: number; status: string };
  expect(firstBody.status).toBe('queued');
  const again = await plugin.post('/plugin/v1/messages', { data: { ...message, body: 'a different body' } });
  expect(again.status(), 'a repeated external_id is answered, not stored again').toBe(200);
  expect(((await again.json()) as { id: number }).id).toBe(firstBody.id);
  const nr = await plugin.post('/plugin/v1/messages', { data: { external_id: 'e2e-msg-noreply', sender: 'notices@example.invalid', body: 'Your statement is ready.', no_reply: true } });
  expect(nr.status()).toBe(202);
  const nrID = ((await nr.json()) as { id: number }).id;

  // The page counts what is waiting for the chief (nothing is delivered yet:
  // this server has no chief and no agent runtime).
  await page.goto(browserURL + '/plugins');
  await expect(page.locator('[data-plugin-queued]')).toHaveAttribute('data-plugin-queued', '2');
  await expect(page.locator('[data-plugin-seen]')).toContainText('last seen');

  // 4. The chief (a fixture row, see the header) is handed both messages.
  const chiefID = fix('chief', '1', chiefName, chiefToken);
  fix('deliver', String(firstBody.id), chiefID);
  fix('deliver', String(nrID), chiefID);

  // 5. The chief drafts over /mcp. A no_reply message is refused.
  const refused = await mcpCall('reply_draft', { message_id: nrID, body: 'Thanks!' });
  expect(refused.isError, refused.text).toBe(true);
  expect(refused.text).toContain('cannot be replied to');
  const drafted = await mcpCall('reply_draft', { message_id: firstBody.id, body: 'Yes, noon works.' });
  expect(drafted.isError, drafted.text).toBe(false);
  const draftID = (JSON.parse(drafted.text) as { draft_id: number }).draft_id;
  expect(draftID).toBeGreaterThan(0);

  // Nothing to claim before the owner decides.
  expect((await plugin.post('/plugin/v1/drafts/claim', { data: { wait_seconds: 0 } })).status()).toBe(204);

  // 6. The card shows the outside text escaped and the reply exactly.
  await page.goto(browserURL + '/plugins');
  const c = card(page, draftID);
  await expect(c).toHaveAttribute('data-draft-status', 'pending');
  await expect(c.locator('[data-draft-original-body]')).toHaveText('Are you free at noon? <b>bold</b>');
  await expect(c.locator('[data-draft-body]')).toHaveText('Yes, noon works.');
  await expect(c.locator('[data-draft-text]')).toHaveAttribute('data-runes', '16');
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw, 'no horizontal scroll at 390px').toBeLessThanOrEqual(cw);

  // 7. STALE: the server refuses an approval outright...
  await makeStale(context);
  const direct = await page.request.post(browserURL + `/plugins/drafts/${draftID}/approve`, { headers: { Origin: browserURL }, maxRedirects: 0 });
  expect(direct.status(), 'a stale approval is refused by the server').toBe(403);
  expect((await plugin.post('/plugin/v1/drafts/claim', { data: { wait_seconds: 0 } })).status()).toBe(204);
  //    ...and the page asks before it submits: the passkey confirms, then the
  //    approval goes through.
  await page.reload();
  const options = page.waitForResponse((r) => r.url().endsWith('/auth/confirm/passkey/options'), { timeout: 20_000 });
  await card(page, draftID).locator('[data-draft-approve]').tap();
  await expect(page.locator('#term-confirm')).toBeVisible();
  await expect(page.locator('#term-confirm')).toContainText('Approving a reply sends it to an outside party');
  await options;
  await expect(page.locator('#term-confirm-passkey')).toBeVisible();
  await Promise.all([
    page.waitForURL((u) => u.pathname === '/plugins' && u.hash === `#draft-${draftID}`),
    page.locator('#term-confirm-passkey').tap(),
  ]);
  await expect(card(page, draftID)).toHaveAttribute('data-draft-status', 'approved');

  // 8. The plugin claims it ONCE, sends it, reports.
  const claim = await plugin.post('/plugin/v1/drafts/claim', { data: { wait_seconds: 5 } });
  expect(claim.status(), await claim.text()).toBe(200);
  const claimed = (await claim.json()) as Record<string, unknown>;
  expect(Object.keys(claimed).sort()).toEqual(['approved_at', 'body', 'draft_id', 'message_external_id']);
  expect(claimed.draft_id).toBe(draftID);
  expect(claimed.body).toBe('Yes, noon works.');
  expect(claimed.message_external_id).toBe('e2e-msg-1');
  expect((await plugin.post('/plugin/v1/drafts/claim', { data: { wait_seconds: 0 } })).status(), 'handed out once').toBe(204);
  const result = await plugin.post(`/plugin/v1/drafts/${draftID}/result`, { data: { ok: true, external_ref: 'upstream-77' } });
  expect(result.status()).toBe(200);
  expect(await result.json()).toEqual({ draft_id: draftID, status: 'sent' });
  expect((await plugin.post(`/plugin/v1/drafts/${draftID}/result`, { data: { ok: true } })).status(), 'the same result again').toBe(200);
  expect((await plugin.post(`/plugin/v1/drafts/${draftID}/result`, { data: { ok: false } })).status(), 'the other result').toBe(409);
  await page.goto(browserURL + '/plugins');
  await expect(card(page, draftID)).toHaveAttribute('data-draft-status', 'sent');
  await expect(card(page, draftID).locator('[data-draft-ref]')).toContainText('upstream-77');

  // 9. A LAPSED LEASE: a second draft is approved (the sign-in is fresh now) and
  //    claimed; no result comes back within the lease; it is unconfirmed and the
  //    next claim does not get it.
  const second = await mcpCall('reply_draft', { message_id: firstBody.id, body: 'Also, see you there.' });
  expect(second.isError, second.text).toBe(false);
  const secondID = (JSON.parse(second.text) as { draft_id: number }).draft_id;
  await page.goto(browserURL + '/plugins');
  await Promise.all([
    page.waitForURL((u) => u.hash === `#draft-${secondID}`),
    card(page, secondID).locator('[data-draft-approve]').tap(),
  ]);
  await expect(page.locator('#term-confirm'), 'fresh: no confirm asked').toBeHidden();
  const claim2 = await plugin.post('/plugin/v1/drafts/claim', { data: { wait_seconds: 5 } });
  expect(claim2.status()).toBe(200);
  expect(((await claim2.json()) as { draft_id: number }).draft_id).toBe(secondID);
  fix('lapse', String(secondID));
  expect((await plugin.post('/plugin/v1/drafts/claim', { data: { wait_seconds: 0 } })).status(), 'a lapsed lease is never re-offered').toBe(204);
  await page.goto(browserURL + '/plugins');
  await expect(card(page, secondID)).toHaveAttribute('data-draft-status', 'unconfirmed');
  await expect(card(page, secondID).locator('[data-draft-reapprove]')).toBeVisible();
  await expect(card(page, secondID).locator('[data-draft-approve]')).toHaveCount(0);
});
