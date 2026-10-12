import { execFileSync } from 'node:child_process';
import path from 'node:path';
import { test, expect } from '../lib/fixtures';
import { dsn, e2eRoot, gotrueBrowserPort, gotrueMainPort, gotruePort, gotrueSecret, hookToken, versionA } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';

// Accounts PR 4, end to end against the REAL GoTrue image (e2e/gotrue/compose.yml,
// same pin as the deployment): claim the admin account from a link the server
// binary prints, land on the board, sign out, sign in through /login, survive an
// access-token expiry by a server-side refresh, and sign out for good.
//
// 🔴 412 px, NOT THE SUITE'S 390. The brief asked for this width; it is set here
// rather than in the config so every other spec keeps measuring what it always
// measured.
//
// ⚠ IT TAKES ~40 s ON PURPOSE. GoTrue's access tokens live 5 s here (the
// compose file's only real deviation) and muster's verifier allows 30 s of
// clock skew, so the earliest a token muster will refuse as expired exists is
// 35 s after it was minted. The wait below is that, plus margin — shortening
// it would test a token muster still accepts, i.e. not the refresh path at all.

const compose = path.join(e2eRoot, 'gotrue', 'compose.yml');
const project = `muster-e2e-gotrue-${gotruePort}`;
const composeEnv = {
  ...process.env,
  MUSTER_E2E_GOTRUE_PROJECT: project,
  MUSTER_E2E_GOTRUE_PORT: String(gotruePort),
  MUSTER_E2E_GOTRUE_SITE: `http://localhost:${gotrueBrowserPort}`,
};
const browserURL = `http://localhost:${gotrueBrowserPort}`;
const gotrueURL = `http://127.0.0.1:${gotruePort}`;

let server: Running | undefined;

test.use({ viewport: { width: 412, height: 915 }, storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(240_000);
  execFileSync('docker', ['compose', '-f', compose, 'up', '-d', '--wait'], { env: composeEnv, stdio: 'inherit' });
  const health = await fetch(gotrueURL + '/health');
  expect(health.status, 'the real GoTrue is up').toBe(200);
  expect(((await health.json()) as { version?: string }).version).toBe('v2.189.0');
  server = await startServer(versionA, gotrueMainPort, {
    MUSTER_AUTH_MODE: 'gotrue',
    MUSTER_GOTRUE_URL: gotrueURL,
    MUSTER_GOTRUE_JWT_SECRET: gotrueSecret,
    MUSTER_BROWSER_PORT: String(gotrueBrowserPort),
    MUSTER_PUBLIC_URL: browserURL,
  });
});

test.afterAll(async () => {
  await server?.stop();
  execFileSync('docker', ['compose', '-f', compose, 'down', '-v'], { env: composeEnv, stdio: 'inherit' });
});

function claimLink(): string {
  const out = execFileSync(serverBinary(versionA), ['invite', '--claim-admin'], {
    env: {
      ...process.env,
      DATABASE_URL: dsn,
      MUSTER_PUBLIC_URL: browserURL,
      MUSTER_GOTRUE_URL: gotrueURL,
      MUSTER_GOTRUE_JWT_SECRET: gotrueSecret,
    },
  }).toString().trim();
  expect(out.startsWith(browserURL + '/auth/claim/'), `claim link: ${out}`).toBe(true);
  return out;
}

test('claim the admin, sign in, survive an access-token expiry, sign out (real GoTrue, 412px)', async ({ page, browser }) => {
  test.setTimeout(180_000);
  const email = `operator-${Date.now()}@example.com`;
  const password = 'e2e-admin-password-not-a-real-one';

  // 1. Claim: the link the server binary printed, redeemed in the browser.
  await page.goto(claimLink());
  await expect(page.getByRole('heading', { name: 'Claim the admin account' })).toBeVisible();
  await page.fill('#claim-email', email);
  await page.fill('#claim-password', password);
  await page.fill('#claim-password2', password);
  await Promise.all([page.waitForURL(browserURL + '/'), page.click('button[type=submit]')]);

  // 2. The board, as the claimed admin.
  await page.goto(browserURL + '/tasks');
  await expect(page.locator('#fab-tasks')).toBeVisible();
  const cookies = await page.context().cookies(browserURL);
  const at = cookies.find((c) => c.name === 'muster_at');
  const rt = cookies.find((c) => c.name === 'muster_rt');
  expect(at && rt, 'both session cookies are set').toBeTruthy();
  for (const c of [at!, rt!]) {
    expect(c.httpOnly && c.secure && c.sameSite === 'Lax', `${c.name} is HttpOnly + Secure + Lax`).toBe(true);
  }

  // 3. Sign out, then sign in through /login.
  await page.goto(browserURL + '/account');
  await expect(page.getByText(email)).toBeVisible();
  await Promise.all([page.waitForURL(/\/login$/), page.getByRole('button', { name: 'Sign out', exact: true }).click()]);
  await page.fill('#muster-email', email);
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL(browserURL + '/'), page.click('button[type=submit]')]);
  await page.goto(browserURL + '/tasks');
  await expect(page.locator('#fab-tasks')).toBeVisible();
  const before = (await page.context().cookies(browserURL)).find((c) => c.name === 'muster_at')!.value;

  // 4. Outlive the access token (5 s + muster's 30 s skew), then reload: the
  // server refreshes from muster_rt and the board still renders.
  await page.waitForTimeout(37_000);
  await page.reload();
  await expect(page).toHaveURL(browserURL + '/tasks');
  await expect(page.locator('#fab-tasks')).toBeVisible();
  const after = (await page.context().cookies(browserURL)).find((c) => c.name === 'muster_at')!.value;
  expect(after, 'the reload after expiry was served on a REFRESHED access token').not.toBe(before);
  const signedIn = await page.context().cookies(browserURL);

  // 5. Sign out: back to /login, and the board is closed.
  await page.goto(browserURL + '/account');
  await Promise.all([page.waitForURL(/\/login$/), page.getByRole('button', { name: 'Sign out', exact: true }).click()]);
  await page.goto(browserURL + '/tasks');
  await expect(page).toHaveURL(/\/login/);

  // 6. The cookies from before the sign-out, replayed in a fresh browser
  // context, open nothing: the session is revoked, not merely forgotten.
  const replay = await browser.newContext({ viewport: { width: 412, height: 915 } });
  await replay.addCookies(signedIn);
  const rp = await replay.newPage();
  await rp.goto(browserURL + '/tasks');
  await expect(rp).toHaveURL(/\/login/);
  await replay.close();
});

test('the machine-token port refuses browser sign-in but serves the hook token', async ({ page }) => {
  const machine = `http://localhost:${gotrueMainPort}`;
  const r = await page.goto(machine + '/login');
  expect(r?.status()).toBe(403);
  await expect(page.locator('input[name="email"]')).toHaveCount(0);
  const api = await page.request.get(machine + '/api/tasks', { headers: { Authorization: `Bearer ${hookToken}` } });
  expect(api.status(), await api.text()).toBe(200);
});
