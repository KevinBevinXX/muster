import { execFileSync } from 'node:child_process';
import path from 'node:path';
import { request as pwRequest } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { dsn, e2eRoot, gotrueSecret, hookToken, port, repoRoot, versionA } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';

// Accounts PR 6, end to end against the REAL GoTrue image (e2e/gotrue/compose.yml,
// the deployment's pin): the admin signs in (by claiming the account), mints a
// personal API token on /account/tokens, reads it ONCE, uses it — from the page
// with fetch and from outside the browser on the MACHINE port — as itself, then
// revokes it on the page and watches the very next request get 401.
//
// 412 px, as the gotrue-login spec. Its OWN ports (port+16..18) and GoTrue, so it
// never shares a container with another spec; and it resets the ACCOUNTS
// tables (seed --accounts-only, accounts PR 5) before and after, because
// claiming binds user 1 to a GoTrue login and a spec that runs later
// (gotrue-login sorts after this file) must be able to claim it again. The
// board is left alone: step 4 reads a task global-setup seeded.

const mainPort = port + 16;
const browserPort = port + 17;
const gotruePort = Number(process.env.MUSTER_E2E_TOKENS_GOTRUE_PORT ?? port + 18);
const compose = path.join(e2eRoot, 'gotrue', 'compose.yml');
const composeEnv = {
  ...process.env,
  MUSTER_E2E_GOTRUE_PROJECT: `muster-e2e-gotrue-${gotruePort}`,
  MUSTER_E2E_GOTRUE_PORT: String(gotruePort),
  MUSTER_E2E_GOTRUE_SITE: `http://localhost:${browserPort}`,
};
const browserURL = `http://localhost:${browserPort}`;
const machineURL = `http://localhost:${mainPort}`;
const gotrueURL = `http://127.0.0.1:${gotruePort}`;

let server: Running | undefined;

test.use({ viewport: { width: 412, height: 915 }, storageState: { cookies: [], origins: [] } });

function resetAccounts() {
  execFileSync('go', ['run', './e2e/seed', '--accounts-only'], { cwd: repoRoot, stdio: 'inherit', env: { ...process.env, DATABASE_URL: dsn } });
}

test.beforeAll(async () => {
  test.setTimeout(240_000);
  execFileSync('docker', ['compose', '-f', compose, 'up', '-d', '--wait'], { env: composeEnv, stdio: 'inherit' });
  const health = await fetch(gotrueURL + '/health');
  expect(health.status, 'the real GoTrue is up').toBe(200);
  resetAccounts();
  server = await startServer(versionA, mainPort, {
    MUSTER_AUTH_MODE: 'gotrue',
    MUSTER_GOTRUE_URL: gotrueURL,
    MUSTER_GOTRUE_JWT_SECRET: gotrueSecret,
    MUSTER_BROWSER_PORT: String(browserPort),
    MUSTER_PUBLIC_URL: browserURL,
  });
});

test.afterAll(async () => {
  await server?.stop();
  execFileSync('docker', ['compose', '-f', compose, 'down', '-v'], { env: composeEnv, stdio: 'inherit' });
  resetAccounts();
});

function claimLink(): string {
  return execFileSync(serverBinary(versionA), ['invite', '--claim-admin'], {
    env: { ...process.env, DATABASE_URL: dsn, MUSTER_PUBLIC_URL: browserURL, MUSTER_GOTRUE_URL: gotrueURL, MUSTER_GOTRUE_JWT_SECRET: gotrueSecret },
  }).toString().trim();
}

// fetch from INSIDE the page, on its own origin, with the token and WITHOUT
// cookies — so the answer is about the token alone, not the signed-in session.
async function pageFetch(page: import('@playwright/test').Page, url: string, token: string) {
  return page.evaluate(async ([u, t]) => {
    const r = await fetch(u, { headers: { Authorization: `Bearer ${t}`, Accept: 'application/json' }, credentials: 'omit' });
    return { status: r.status, body: await r.text() };
  }, [url, token] as const);
}

test('mint a personal API token on the page, use it, revoke it → 401 (real GoTrue, 412px)', async ({ page }) => {
  test.setTimeout(180_000);
  const stamp = Date.now();
  const email = `operator-${stamp}@example.com`;
  const pw = 'e2e-admin-password-not-a-real-one';

  // 1. Sign in (the admin claim is the sign-in path that exists before invites).
  await page.goto(claimLink());
  await page.fill('#claim-email', email);
  await page.fill('#claim-password', pw);
  await page.fill('#claim-password2', pw);
  await Promise.all([page.waitForURL(browserURL + '/'), page.click('button[type=submit]')]);

  // 2. Account → API tokens.
  await page.goto(browserURL + '/account');
  await Promise.all([page.waitForURL(browserURL + '/account/tokens'), page.getByRole('link', { name: 'API tokens' }).click()]);
  await expect(page.getByRole('heading', { name: 'API tokens' })).toBeVisible();
  await expect(page.getByText('No tokens yet.')).toBeVisible();

  // 3. Mint. The token is on THIS page, once.
  const name = `e2e laptop ${stamp}`;
  await page.fill('#token-name', name);
  await page.getByRole('button', { name: 'Create token' }).click();
  const minted = page.locator('#minted-token');
  await expect(minted).toBeVisible();
  const token = await minted.inputValue();
  expect(token).toMatch(/^mst_[A-Za-z0-9_-]{43}$/);
  // Nothing on the page overflows the 412px viewport (the token is long).
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  expect(overflow, 'horizontal overflow at 412px').toBeLessThanOrEqual(0);

  // ...and never again: the list shows the NAME, not the token.
  await page.goto(browserURL + '/account/tokens');
  await expect(page.locator('[data-testid="token-list"]')).toContainText(name);
  expect(await page.content()).not.toContain(token);

  // 4. Use it: from the page with fetch (no cookies) on the browser listener,
  // and from outside the browser on the MACHINE port — acting as the admin.
  const who = await pageFetch(page, '/api/whoami', token);
  expect(who.status, who.body).toBe(200);
  expect(JSON.parse(who.body)).toMatchObject({ userId: 1, via: 'api-token', role: 'admin' });
  const tasks = await pageFetch(page, '/api/tasks', token);
  expect(tasks.status, tasks.body).toBe(200);
  expect(tasks.body).toContain('Rotate the staging webhook secret'); // a seeded task of user 1
  const machine = await pwRequest.newContext({ baseURL: machineURL, extraHTTPHeaders: { Authorization: `Bearer ${token}` } });
  const onMachinePort = await machine.get('/api/tasks');
  expect(onMachinePort.status(), await onMachinePort.text()).toBe(200);
  // Control: the same fetch with no token is refused — the 200 above was the token.
  // 403, not 401: on the browser listener the hook/service tier refuses every
  // credential but a personal token before comparing anything (auth.go,
  // refuseSharedTokenOnBrowserListener), so "missing" and "wrong" look alike.
  const bare = await page.evaluate(async () => (await fetch('/api/tasks', { credentials: 'omit' })).status);
  expect(bare).toBe(403);
  // The SHARED hook token (user 1, admin, held by every hook script) is refused on
  // the browser listener — the port the public edge routes to — and still works on
  // the machine port. The machine-port 200 is the control: without it the 403
  // could be a server refusing the hook token everywhere.
  const hookOnBrowser = await pageFetch(page, '/api/whoami', hookToken);
  expect(hookOnBrowser.status, hookOnBrowser.body).toBe(403);
  expect(JSON.parse(hookOnBrowser.body).error).toBe(
    'shared machine tokens are not accepted on this port; use a personal API token (mst_) or the machine port');
  const hookMachine = await pwRequest.newContext({ baseURL: machineURL, extraHTTPHeaders: { Authorization: `Bearer ${hookToken}` } });
  const hookOnMachine = await hookMachine.get('/api/whoami');
  expect(hookOnMachine.status(), await hookOnMachine.text()).toBe(200);
  expect(await hookOnMachine.json()).toMatchObject({ userId: 1, via: 'hook', role: 'admin' });
  await hookMachine.dispose();
  // The list now shows it was used.
  await page.reload();
  await expect(page.locator('[data-testid="token-list"]')).toContainText('last used');

  // 5. Revoke on the page; the very next request with it is 401, on both ports.
  await Promise.all([
    page.waitForURL(browserURL + '/account/tokens'),
    page.locator('[data-testid="token-list"] li', { hasText: name }).getByRole('button', { name: 'Revoke' }).click(),
  ]);
  await expect(page.locator('[data-testid="token-list"] li', { hasText: name })).toContainText('revoked');
  const after = await pageFetch(page, '/api/tasks', token);
  expect(after.status, after.body).toBe(401);
  expect((await machine.get('/api/tasks')).status()).toBe(401);
  await machine.dispose();
});
