import { execFileSync } from 'node:child_process';
import path from 'node:path';
import { request as pwRequest } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import {
  dsn, e2eRoot, gotrueSecret, hookToken, invitesBrowserPort, invitesGotruePort, invitesMainPort, repoRoot, versionA,
} from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';

// Accounts PR 5, end to end against the REAL GoTrue image (e2e/gotrue/compose.yml,
// the deployment's pin): the admin claims the account, creates an invite link
// on /admin/invites, a SECOND browser context redeems it and lands on an EMPTY
// board, cannot open the admin's task (404) or the admin pages (403), and once
// the admin disables it, its next request is refused.
//
// 412 px, as the gotrue-login spec. Its own GoTrue and server ports
// (env.ts invites*), and it resets the accounts tables first (seed
// --accounts-only): in a whole-suite run it can follow gotrue-login on the same
// database, which leaves user 1 claimed against a GoTrue that is gone.

const compose = path.join(e2eRoot, 'gotrue', 'compose.yml');
const project = `muster-e2e-gotrue-${invitesGotruePort}`;
const composeEnv = {
  ...process.env,
  MUSTER_E2E_GOTRUE_PROJECT: project,
  MUSTER_E2E_GOTRUE_PORT: String(invitesGotruePort),
  MUSTER_E2E_GOTRUE_SITE: `http://localhost:${invitesBrowserPort}`,
};
const browserURL = `http://localhost:${invitesBrowserPort}`;
const machineURL = `http://localhost:${invitesMainPort}`;
const gotrueURL = `http://127.0.0.1:${invitesGotruePort}`;
const viewport = { width: 412, height: 915 };

let server: Running | undefined;

test.use({ viewport, storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(240_000);
  execFileSync('docker', ['compose', '-f', compose, 'up', '-d', '--wait'], { env: composeEnv, stdio: 'inherit' });
  const health = await fetch(gotrueURL + '/health');
  expect(health.status, 'the real GoTrue is up').toBe(200);
  expect(((await health.json()) as { version?: string }).version).toBe('v2.189.0');
  execFileSync('go', ['run', './e2e/seed', '--accounts-only'], {
    cwd: repoRoot, stdio: 'inherit', env: { ...process.env, DATABASE_URL: dsn },
  });
  server = await startServer(versionA, invitesMainPort, {
    MUSTER_AUTH_MODE: 'gotrue',
    MUSTER_GOTRUE_URL: gotrueURL,
    MUSTER_GOTRUE_JWT_SECRET: gotrueSecret,
    MUSTER_BROWSER_PORT: String(invitesBrowserPort),
    MUSTER_PUBLIC_URL: browserURL,
  });
});

test.afterAll(async () => {
  await server?.stop();
  execFileSync('docker', ['compose', '-f', compose, 'down', '-v'], { env: composeEnv, stdio: 'inherit' });
});

function claimLink(): string {
  return execFileSync(serverBinary(versionA), ['invite', '--claim-admin'], {
    env: { ...process.env, DATABASE_URL: dsn, MUSTER_PUBLIC_URL: browserURL, MUSTER_GOTRUE_URL: gotrueURL, MUSTER_GOTRUE_JWT_SECRET: gotrueSecret },
  }).toString().trim();
}

const taskCards = '[data-tasks-flat] > [id^="task-"]';

test('an admin invites a member by link; the member sees only its own (empty) board and is refused once disabled (real GoTrue, 412px)', async ({ page, browser }) => {
  test.setTimeout(240_000);
  const stamp = Date.now();
  const adminEmail = `admin-${stamp}@example.com`;
  const memberEmail = `member-${stamp}@example.com`;
  const adminPW = 'e2e-admin-password-not-a-real-one';
  const memberPW = 'e2e-member-password-not-a-real-one';

  // 0. The admin's own task, through the machine port's hook token (user 1).
  const machine = await pwRequest.newContext({ baseURL: machineURL, extraHTTPHeaders: { Authorization: `Bearer ${hookToken}` } });
  const created = await machine.post('/api/tasks', { data: { title: `admin-only task ${stamp}`, body: 'the admin’s, not the member’s' } });
  expect(created.status(), await created.text()).toBeLessThan(300);
  const adminTask = ((await created.json()) as { id: number }).id;

  // 1. The admin claims the account and reaches the admin pages from the sidebar.
  // The claim page opened WITHOUT its fragment — what a sign-in redirect in
  // front of muster does (measured live behind Authelia: rd= carries no
  // fragment). The page asks for the link; the WHOLE link is pasted, and the
  // page's script keeps only the token before posting.
  const claim = claimLink();
  await page.goto(browserURL + '/auth/claim');
  await expect(page.getByLabel('Paste your link')).toBeVisible();
  await page.getByLabel('Paste your link').fill(claim);
  await page.fill('#claim-email', adminEmail);
  await page.fill('#claim-password', adminPW);
  await page.fill('#claim-password2', adminPW);
  await Promise.all([page.waitForURL(browserURL + '/'), page.click('button[type=submit]')]);
  await page.goto(browserURL + '/tasks');
  await expect(page.locator(`#task-${adminTask}`), 'CONTROL: the admin sees its own task').toBeVisible();
  await expect(page.locator('#sidebar-admin')).toHaveAttribute('href', '/admin/users');
  await expect(page.locator('#sidebar-account')).toHaveAttribute('href', '/account');

  // 2. Create an invite link; it is shown once, with the token in the FRAGMENT.
  await page.goto(browserURL + '/admin/invites');
  // An invite always makes a member: there is no role to choose (admins are
  // made only by promoting a member on /admin/users).
  await expect(page.locator('#invite-role, [name="role"]')).toHaveCount(0);
  await expect(page.locator('#invite-ttl')).toHaveValue('72');
  await page.getByRole('button', { name: 'Create link' }).click();
  const link = await page.locator('#once-link').inputValue();
  expect(link.startsWith(browserURL + '/invite#'), `invite link: ${link}`).toBe(true);
  const token = link.slice(link.indexOf('#') + 1);
  expect(Buffer.from(token, 'base64url').length, 'the token is 32 random bytes').toBe(32);
  await page.goto(browserURL + '/admin/invites');
  await expect(page.locator('[data-invite-state="pending"]')).toHaveCount(1);
  await expect(page.locator('body')).not.toContainText(token);

  // 3. A SECOND browser context (the invitee) redeems it.
  const invitee = await browser.newContext({ viewport });
  const mp = await invitee.newPage();
  await mp.goto(link);
  await expect(mp.getByRole('heading', { name: 'Create your muster account' })).toBeVisible();
  expect(mp.url(), 'the fragment left the address bar').toBe(browserURL + '/invite');
  await mp.fill('#invite-email', memberEmail);
  await mp.fill('#invite-name', 'E2E Member');
  await mp.fill('#invite-password', memberPW);
  await mp.fill('#invite-password2', memberPW);
  await Promise.all([mp.waitForURL(browserURL + '/'), mp.getByRole('button', { name: 'Create account' }).click()]);
  const mcookies = await invitee.cookies(browserURL);
  expect(mcookies.find((c) => c.name === 'muster_at' && c.httpOnly && c.secure), 'the member is signed in').toBeTruthy();

  // 3b. The same for an invite opened without its fragment: a third context
  // loads /invite bare, pastes the whole link, and redeems it. (With the
  // fragment, step 3 above, the paste field stays hidden.)
  await page.goto(browserURL + '/admin/invites');
  await page.getByRole('button', { name: 'Create link' }).click();
  const link2 = await page.locator('#once-link').inputValue();
  const paster = await browser.newContext({ viewport });
  const pp = await paster.newPage();
  const posted: string[] = [];
  pp.on('request', (r) => { if (r.method() === 'POST') posted.push(r.url() + ' ' + (r.postData() ?? '')); });
  await pp.goto(browserURL + '/invite');
  await expect(pp.getByLabel('Paste your link')).toBeVisible();
  await pp.getByLabel('Paste your link').fill(link2);
  await pp.getByRole('button', { name: 'Continue' }).click();
  await expect(pp.getByRole('heading', { name: 'Create your muster account' })).toBeVisible();
  await pp.fill('#invite-email', `paster-${stamp}@example.com`);
  await pp.fill('#invite-password', memberPW);
  await pp.fill('#invite-password2', memberPW);
  await Promise.all([pp.waitForURL(browserURL + '/'), pp.getByRole('button', { name: 'Create account' }).click()]);
  const token2 = link2.slice(link2.indexOf('#') + 1);
  // The token travelled only in POST bodies, as the bare token — never in a URL.
  expect(posted.some((p) => p.includes('token=' + token2)), `posts: ${posted.join(' | ')}`).toBe(true);
  expect(posted.every((p) => !p.split(' ')[0].includes(token2)), 'no POST URL carries the token').toBe(true);
  expect(posted.every((p) => !decodeURIComponent(p.split(' ')[1] ?? '').includes('/invite#')), 'the whole link was reduced to its token').toBe(true);
  await paster.close();
  await page.goto(browserURL + '/admin/invites');
  await expect(page.locator('[data-invite-state="pending"]'), 'both links are redeemed').toHaveCount(0);

  // 4. The member's board is EMPTY (the admin's, on the same page, is not).
  await mp.goto(browserURL + '/tasks');
  await expect(mp.locator('#fab-tasks')).toBeVisible();
  // Wait for the board to have LOADED (it renders after the shell): a count
  // read before then is 0 for any board, which proves nothing either way —
  // the first version read the admin's count too early and saw 0.
  await expect(mp.getByText('No tasks yet'), "the member's board loaded, and empty").toBeVisible();
  await expect(mp.locator(taskCards)).toHaveCount(0);
  await page.goto(browserURL + '/tasks');
  await expect(page.locator(`#task-${adminTask}`), 'CONTROL: the admin board loaded with its task').toBeVisible();
  expect(await page.locator(taskCards).count(), 'CONTROL: the admin board has tasks').toBeGreaterThan(0);

  // 5. The admin's task is a 404 to the member; the admin pages are a 403; no Admin link.
  const taskResp = await mp.goto(browserURL + `/tasks/${adminTask}`);
  expect(taskResp?.status(), "the member opening the admin's task").toBe(404);
  await expect(mp.locator('body')).not.toContainText(`admin-only task ${stamp}`);
  const adminResp = await mp.goto(browserURL + '/admin/users');
  expect(adminResp?.status(), 'the member opening /admin/users').toBe(403);
  await mp.goto(browserURL + '/tasks');
  await expect(mp.locator('#sidebar-admin')).toHaveCount(0);
  await expect(mp.locator('#sidebar-account')).toHaveCount(1);

  // 5b. The admin makes a password-reset link (GoTrue generate_link, exchanged
  // server-side at /verify) and the member sets a new password with it; the
  // reset signs the member in with the NEW password, and the link is single-use.
  await page.goto(browserURL + '/admin/users');
  await page.locator(`[data-user-email="${memberEmail}"]`).getByRole('button', { name: 'Reset link' }).click();
  const resetLink = await page.locator('#once-link').inputValue();
  expect(resetLink.startsWith(browserURL + '/auth/reset#'), `reset link: ${resetLink}`).toBe(true);
  const newPW = 'e2e-member-new-password-not-real';
  await mp.goto(resetLink);
  expect(mp.url(), 'the fragment left the address bar').toBe(browserURL + '/auth/reset');
  await mp.fill('#reset-password', newPW);
  await mp.fill('#reset-password2', newPW);
  // A reset lands on /account, which says it revoked the API tokens (accounts PR 6).
  await Promise.all([mp.waitForURL(browserURL + '/account?reset=1'), mp.getByRole('button', { name: 'Set password' }).click()]);
  await expect(mp.getByRole('status')).toContainText('Your API tokens were revoked');
  await mp.goto(browserURL + '/tasks');
  await expect(mp.locator('#fab-tasks')).toBeVisible();
  const again = await browser.newContext({ viewport });
  const ap = await again.newPage();
  await ap.goto(resetLink);
  await ap.fill('#reset-password', 'yet-another-password-x');
  await ap.fill('#reset-password2', 'yet-another-password-x');
  await ap.getByRole('button', { name: 'Set password' }).click();
  await expect(ap.getByRole('heading', { name: 'This link is invalid or expired' })).toBeVisible();
  await again.close();

  // 6. The admin disables the member.
  await page.goto(browserURL + '/admin/users');
  const row = page.locator(`[data-user-email="${memberEmail}"]`);
  await expect(row.locator('[data-user-state]')).toHaveText('active');
  await row.getByRole('button', { name: 'Disable' }).click();
  await expect(page).toHaveURL(browserURL + '/admin/users');
  await expect(page.locator(`[data-user-email="${memberEmail}"] [data-user-state]`)).toHaveText('disabled');

  // 7. The member's NEXT request is refused: a navigation lands on /login, and
  // the same cookies replayed on an API call get a 401 (the refusal is about
  // the account, not about cookies the first refusal cleared).
  const held = await invitee.cookies(browserURL);
  await mp.goto(browserURL + '/tasks');
  await expect(mp).toHaveURL(/\/login/);
  const replay = await browser.newContext({ viewport });
  await replay.addCookies(held);
  const api = await replay.request.get(browserURL + '/ui/tasks/review-count', { headers: { 'HX-Request': 'true' } });
  expect(api.status(), 'the disabled member’s held cookies on an API call').toBe(401);
  await replay.close();
  // And signing in again is refused too (GoTrue ban + disabled_at).
  await mp.fill('#muster-email', memberEmail);
  await mp.fill('#muster-password', newPW);
  await mp.click('button[type=submit]');
  await expect(mp).toHaveURL(/\/login/);

  // 8. Enabling does NOT resurrect the pre-disable session. A GoTrue ban alone
  // would (an unban accepts pre-ban refresh tokens again — measured), so a
  // disable also signs the user out everywhere in GoTrue, and muster's
  // per-user cutoff refuses the held cookies: first while their access token is
  // still alive (only muster can — GoTrue cannot recall a JWT), then after it
  // expires on the refresh path. A sign-in made after the enable works.
  await page.goto(browserURL + '/admin/users');
  await page.locator(`[data-user-email="${memberEmail}"]`).getByRole('button', { name: 'Enable' }).click();
  await expect(page.locator(`[data-user-email="${memberEmail}"] [data-user-state]`)).toHaveText('active');
  const live = await browser.newContext({ viewport });
  await live.addCookies(held);
  expect((await live.request.get(browserURL + '/ui/tasks/review-count', { headers: { 'HX-Request': 'true' } })).status(),
    'the pre-disable access token after enable').toBe(401);
  await live.close();
  // Outlive the held access token (JWT_EXP 5 s + muster's 30 s skew + margin):
  // muster would now have to REFRESH from the held refresh token.
  await page.waitForTimeout(37_000);
  const late = await browser.newContext({ viewport });
  await late.addCookies(held);
  const lp = await late.newPage();
  await lp.goto(browserURL + '/tasks');
  await expect(lp, 'the pre-disable refresh token after enable').toHaveURL(/\/login/);
  await late.close();
  // And GoTrue no longer holds it either: the disable signed the user out
  // everywhere there (a ban alone would not — an unban resurrects it).
  const heldRT = held.find((c) => c.name === 'muster_rt')!.value;
  const direct = await fetch(gotrueURL + '/token?grant_type=refresh_token', {
    method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Muster-Client-IP': '127.0.0.1' },
    body: JSON.stringify({ refresh_token: heldRT }),
  });
  expect(direct.status, 'GoTrue refuses the pre-disable refresh token after enable').toBe(400);
  // A sign-in after the enable works.
  await mp.goto(browserURL + '/login');
  await mp.fill('#muster-email', memberEmail);
  await mp.fill('#muster-password', newPW);
  await Promise.all([mp.waitForURL(browserURL + '/'), mp.click('button[type=submit]')]);
  await mp.goto(browserURL + '/tasks');
  await expect(mp.locator('#fab-tasks')).toBeVisible();

  await invitee.close();
  await machine.dispose();
});
