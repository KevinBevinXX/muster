import { execFileSync } from 'node:child_process';
import path from 'node:path';
import type { BrowserContext, Page } from '@playwright/test';
import { test, expect, expectShellShows } from '../lib/fixtures';
import {
  dsn, e2eRoot, gotrueSecret, passkeysBrowserPort, passkeysGotruePort, passkeysMainPort, repoRoot, versionA, versionTerm,
} from '../lib/env';
import { gotrueSkewEnv, outliveAccessToken } from '../lib/gotrue';
import { serverBinary, startServer, Running } from '../lib/server';
import { authenticator, credentialCount } from '../lib/webauthn';

// Passkeys, end to end: Chromium's VIRTUAL AUTHENTICATOR (CDP WebAuthn domain)
// against the REAL GoTrue image with GOTRUE_PASSKEY_ENABLED, through muster's
// proxy. Unlike the Go tests' fake, every signature here is real and GoTrue
// verifies it — this is where the cryptography is exercised.
//
// The authenticator is an internal (platform) CTAP2 one with resident keys and
// user verification, auto-confirming presence — a phone's or laptop's built-in
// passkey store, minus the human. Each browser context gets its own, so the
// admin's and the member's passkeys never meet.
//
// ⚠ One step outlives an access token (JWT_EXP 5 s + the e2e build's 1 s
// verifier skew, as gotrue-login.spec.ts; e2e/lib/gotrue.ts) to prove a PASSKEY
// session refreshes like a password one. muster's refresh code is the same for
// both (resolveGoTrueSession does not ask how a session began); what only this
// spec covers is the refresh token's provenance: the one GoTrue's passkey grant
// issued, which passkeys.go wrote into muster_rt.

const compose = path.join(e2eRoot, 'gotrue', 'compose.yml');
const project = `muster-e2e-gotrue-${passkeysGotruePort}`;
const browserURL = `http://localhost:${passkeysBrowserPort}`;
const gotrueURL = `http://127.0.0.1:${passkeysGotruePort}`;
const composeEnv = {
  ...process.env,
  MUSTER_E2E_GOTRUE_PROJECT: project,
  MUSTER_E2E_GOTRUE_PORT: String(passkeysGotruePort),
  MUSTER_E2E_GOTRUE_SITE: browserURL,
  MUSTER_E2E_GOTRUE_PASSKEYS: 'true',
};
const viewport = { width: 412, height: 915 };
const adminEmail = `pk-admin-${Date.now()}@example.com`;
const adminPW = 'e2e-admin-password-not-a-real-one';
const noUV = 'did not verify you';

let server: Running | undefined;

test.use({ viewport, storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(240_000);
  execFileSync('docker', ['compose', '-f', compose, 'up', '-d', '--wait'], { env: composeEnv, stdio: 'inherit' });
  const health = await fetch(gotrueURL + '/health');
  expect(health.status, 'the real GoTrue is up').toBe(200);
  expect(((await health.json()) as { version?: string }).version).toBe('v2.197.0');
  const settings = (await (await fetch(gotrueURL + '/settings')).json()) as { passkeys_enabled?: boolean };
  expect(settings.passkeys_enabled, 'GoTrue has passkeys on (GOTRUE_PASSKEY_ENABLED)').toBe(true);
  execFileSync('go', ['run', './e2e/seed', '--accounts-only'], {
    cwd: repoRoot, stdio: 'inherit', env: { ...process.env, DATABASE_URL: dsn },
  });
  server = await startServer(versionTerm, passkeysMainPort, {
    ...gotrueSkewEnv,
    MUSTER_AUTH_MODE: 'gotrue',
    MUSTER_GOTRUE_URL: gotrueURL,
    MUSTER_GOTRUE_JWT_SECRET: gotrueSecret,
    MUSTER_BROWSER_PORT: String(passkeysBrowserPort),
    MUSTER_PUBLIC_URL: browserURL,
  });
});

test.afterAll(async () => {
  await server?.stop();
  execFileSync('docker', ['compose', '-f', compose, 'down', '-v'], { env: composeEnv, stdio: 'inherit' });
});

async function session(ctx: BrowserContext): Promise<{ at?: string; rt?: string }> {
  const cs = await ctx.cookies(browserURL);
  return { at: cs.find((c) => c.name === 'muster_at')?.value, rt: cs.find((c) => c.name === 'muster_rt')?.value };
}

async function addPasskey(page: Page): Promise<void> {
  await expect(page.locator('#passkey-add')).toBeVisible();
  const before = await page.locator('#passkey-list [data-passkey-id]').count();
  await page.locator('#passkey-add').click();
  await expect(page.locator('#passkey-list [data-passkey-id]'), 'the new passkey is listed').toHaveCount(before + 1);
}

async function signOut(page: Page): Promise<void> {
  await page.goto(browserURL + '/account');
  await Promise.all([page.waitForURL(/\/login$/), page.getByRole('button', { name: 'Sign out', exact: true }).click()]);
}

test('add a passkey, sign in with it, outlive the access token, sign out everywhere (real GoTrue, virtual authenticator)', async ({ page, browser }) => {
  test.setTimeout(180_000);
  const auth = await authenticator(page);

  // The admin claims the account (password: it stays mandatory).
  const claim = execFileSync(serverBinary(versionA), ['invite', '--claim-admin'], {
    env: { ...process.env, DATABASE_URL: dsn, MUSTER_PUBLIC_URL: browserURL, MUSTER_GOTRUE_URL: gotrueURL, MUSTER_GOTRUE_JWT_SECRET: gotrueSecret },
  }).toString().trim();
  await page.goto(claim);
  await page.fill('#claim-email', adminEmail);
  await page.fill('#claim-password', adminPW);
  await page.fill('#claim-password2', adminPW);
  await Promise.all([page.waitForURL(browserURL + '/'), page.click('button[type=submit]')]);

  // Account → Passkeys → add one on this device (the sign-in was just now:
  // no password re-ask).
  await page.goto(browserURL + '/account');
  await Promise.all([page.waitForURL(browserURL + '/account/passkeys'), page.locator('#account-passkeys').click()]);
  await expect(page.locator('#passkey-none')).toBeVisible();
  await addPasskey(page);
  await expect(page.locator('#passkey-reauth')).toBeHidden();
  expect(await credentialCount(auth), 'the authenticator holds one (resident) credential').toBe(1);
  // The virtual authenticator's credential is not backed up: device-only.
  await expect(page.locator('#passkey-list [data-passkey-synced]')).toHaveText('this device only');

  // Sign out, then sign in with the passkey: no email, no password.
  await signOut(page);
  await expect(page.locator('#passkey-signin')).toBeVisible();
  await Promise.all([page.waitForURL(browserURL + '/'), page.locator('#passkey-signin').click()]);
  await expectShellShows(page, 'tasks', 'Tasks', 'landing on / after a passkey sign-in');
  const cs = await page.context().cookies(browserURL);
  for (const name of ['muster_at', 'muster_rt']) {
    const c = cs.find((x) => x.name === name);
    expect(c && c.httpOnly && c.secure && c.sameSite === 'Lax', `${name} is set HttpOnly + Secure + Lax`).toBeTruthy();
  }

  // Outlive the access token: the PASSKEY session refreshes server-side.
  await page.goto(browserURL + '/tasks');
  const before = (await session(page.context())).at;
  await outliveAccessToken(before!);
  await page.reload();
  await expect(page).toHaveURL(browserURL + '/tasks');
  await expect(page.locator('#fab-tasks')).toBeVisible();
  expect((await session(page.context())).at, 'served on a REFRESHED access token').not.toBe(before);

  // Sign out everywhere ends the passkey session: its cookies, replayed, open nothing.
  const held = await page.context().cookies(browserURL);
  await page.goto(browserURL + '/account');
  await Promise.all([page.waitForURL(/\/login$/), page.getByRole('button', { name: 'Sign out everywhere' }).click()]);
  const replay = await browser.newContext({ viewport });
  await replay.addCookies(held);
  const rp = await replay.newPage();
  await rp.goto(browserURL + '/tasks');
  await expect(rp, 'the passkey session after sign-out-everywhere').toHaveURL(/\/login/);
  await replay.close();
});

test('a member: invite → passkey offer; no-UV and disabled refused; a reset deletes the passkey (real GoTrue)', async ({ page, browser }) => {
  test.setTimeout(180_000);
  const memberEmail = `pk-member-${Date.now()}@example.com`;
  const memberPW = 'e2e-member-password-not-a-real-one';

  // The admin (claimed by the test above) signs in with the password.
  await page.goto(browserURL + '/login');
  await page.fill('#muster-email', adminEmail);
  await page.fill('#muster-password', adminPW);
  await Promise.all([page.waitForURL(browserURL + '/'), page.click('button[type=submit]')]);
  await page.goto(browserURL + '/admin/invites');
  await page.getByRole('button', { name: 'Create link' }).click();
  const link = await page.locator('#once-link').inputValue();

  // The member redeems it in its own browser, and lands on the passkey offer.
  const mctx = await browser.newContext({ viewport });
  const mp = await mctx.newPage();
  const mauth = await authenticator(mp);
  await mp.goto(link);
  await mp.fill('#invite-email', memberEmail);
  await mp.fill('#invite-password', memberPW);
  await mp.fill('#invite-password2', memberPW);
  await Promise.all([mp.waitForURL(browserURL + '/account/passkeys?welcome=1'), mp.getByRole('button', { name: 'Create account' }).click()]);
  await expect(mp.locator('#passkey-welcome')).toBeVisible();
  await expect(mp.locator('#passkey-skip')).toHaveAttribute('href', '/');
  await addPasskey(mp);
  await signOut(mp);

  // 🔴 muster's UV GUARD, with real crypto. The options are rewritten in
  // flight to userVerification "discouraged" and the authenticator stops
  // verifying, so the browser makes a correctly SIGNED assertion with UV=0 —
  // which GoTrue (UV "preferred") would accept. muster refuses it.
  await mauth.cdp.send('WebAuthn.setUserVerified', { authenticatorId: mauth.id, isUserVerified: false });
  await mp.route('**/auth/passkey/options', async (route) => {
    const resp = await route.fetch();
    const body = await resp.json();
    body.options.userVerification = 'discouraged';
    await route.fulfill({ response: resp, json: body });
  });
  await mp.reload(); // the page prefetches its options: fetch them through the route
  await mp.locator('#passkey-signin').click();
  await expect(mp.locator('#passkey-signin-status')).toContainText(noUV);
  await expect(mp).toHaveURL(/\/login/);
  expect((await session(mctx)).at, 'no session for a no-UV assertion').toBeUndefined();
  // The page re-prefetches after a failure: let that in-flight handler finish.
  await mp.unrouteAll({ behavior: 'wait' });

  // With muster's own options (UV required) a non-verifying authenticator
  // cannot even produce an assertion: refused in the browser.
  await mp.reload();
  await mp.locator('#passkey-signin').click();
  await expect(mp.locator('#passkey-signin-status')).not.toHaveText('');
  await expect(mp).toHaveURL(/\/login/);
  expect((await session(mctx)).at).toBeUndefined();

  // CONTROL: verifying again, the same passkey signs in.
  await mauth.cdp.send('WebAuthn.setUserVerified', { authenticatorId: mauth.id, isUserVerified: true });
  await mp.reload();
  await Promise.all([mp.waitForURL(browserURL + '/'), mp.locator('#passkey-signin').click()]);
  await signOut(mp);

  // The admin disables the member: the passkey is refused.
  await page.goto(browserURL + '/admin/users');
  const row = page.locator(`[data-user-email="${memberEmail}"]`);
  await row.getByRole('button', { name: 'Disable' }).click();
  await expect(page.locator(`[data-user-email="${memberEmail}"] [data-user-state]`)).toHaveText('disabled');
  await mp.reload();
  await mp.locator('#passkey-signin').click();
  await expect(mp.locator('#passkey-signin-status')).toContainText('not accepted');
  await expect(mp).toHaveURL(/\/login/);

  // Enabled again, then reset: the reset deletes the member's passkeys, so the
  // credential still on the device is refused by GoTrue.
  await page.locator(`[data-user-email="${memberEmail}"]`).getByRole('button', { name: 'Enable' }).click();
  await expect(page.locator(`[data-user-email="${memberEmail}"] [data-user-state]`)).toHaveText('active');
  await page.locator(`[data-user-email="${memberEmail}"]`).getByRole('button', { name: 'Reset link' }).click();
  const resetLink = await page.locator('#once-link').inputValue();
  await mp.goto(resetLink);
  await mp.fill('#reset-password', 'e2e-member-new-password-not-real');
  await mp.fill('#reset-password2', 'e2e-member-new-password-not-real');
  await Promise.all([mp.waitForURL(browserURL + '/account?reset=1'), mp.getByRole('button', { name: 'Set password' }).click()]);
  await mp.goto(browserURL + '/account/passkeys');
  await expect(mp.locator('#passkey-none'), 'the reset removed the passkey').toBeVisible();
  await signOut(mp);
  expect(await credentialCount(mauth), 'CONTROL: the device still holds the credential').toBe(1);
  await mp.locator('#passkey-signin').click();
  await expect(mp.locator('#passkey-signin-status')).toContainText('not accepted');
  await expect(mp).toHaveURL(/\/login/);
  await mctx.close();
});
