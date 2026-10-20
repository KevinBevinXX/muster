import { test, expect } from '../lib/fixtures';
import { baseURL, password } from '../lib/env';

// Account → Devices (the Android app's paired phones, Phase 1), end to end on
// the shared password-mode server: inside the app's WebView — played here by
// the bridge object the app injects (window.musterApp) — "Pair this phone"
// mints a one-time code and hands it to the app; the app redeems it for its
// device credential; the phone is listed; Remove signs it out on its next
// request. Outside the shell the pairing buttons are not drawn.
//
// A FRESH sign-in in the test, not the suite's stored session: pairing needs a
// sign-in from the last five minutes.

test.use({ viewport: { width: 412, height: 915 }, storageState: { cookies: [], origins: [] } });

async function signIn(page: import('@playwright/test').Page) {
  await page.goto(baseURL + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
}

test('pair a phone from the app shell, see it on Account → Devices, remove it', async ({ page, request }) => {
  await page.addInitScript(() => {
    const w = window as unknown as { __appMsgs: string[]; musterApp: { postMessage(m: string): void } };
    w.__appMsgs = [];
    w.musterApp = { postMessage: (m: string) => w.__appMsgs.push(m) };
  });
  await signIn(page);
  await page.goto(baseURL + '/account');
  const section = page.getByTestId('paired-devices');
  await expect(section.getByRole('heading', { name: 'Devices' })).toBeVisible();

  await section.getByRole('button', { name: 'Pair this phone' }).click();
  await expect(page.locator('#pair-status')).toHaveText('The app is finishing the pairing.');
  const msgs = await page.evaluate(() => (window as unknown as { __appMsgs: string[] }).__appMsgs);
  expect(msgs).toHaveLength(1);
  const msg = JSON.parse(msgs[0]);
  expect(msg.type).toBe('pair');
  expect(msg.code).toMatch(/^mdp_[A-Za-z0-9_-]{43}$/);

  // The app's side: redeem the code, then use the credential.
  const name = `e2e phone ${Date.now()}`;
  const paired = await request.post(baseURL + '/api/device/pair', { data: { code: msg.code, name, app_version: '0.0.1' } });
  expect(paired.status(), await paired.text()).toBe(201);
  const token = (await paired.json()).token as string;
  const auth = { Authorization: 'Bearer ' + token };
  expect((await request.get(baseURL + '/api/device/config', { headers: auth })).status()).toBe(200);
  // The same code a second time pairs nothing.
  const again = await request.post(baseURL + '/api/device/pair', { data: { code: msg.code, name } });
  expect(again.status()).toBe(401);
  // The credential opens the device tier only.
  expect((await request.get(baseURL + '/api/tasks', { headers: auth })).status()).toBe(401);

  await page.reload();
  const row = section.getByTestId('paired-device').filter({ hasText: name });
  await expect(row).toBeVisible();
  await expect(row).toContainText('app 0.0.1');
  await expect(row).toContainText('push not registered');
  await row.getByRole('button', { name: 'Remove' }).click();
  await expect(page.getByText('Device removed. It is signed out.')).toBeVisible();
  await expect(section.getByTestId('paired-device').filter({ hasText: name })).toHaveCount(0);
  expect((await request.get(baseURL + '/api/device/config', { headers: auth })).status()).toBe(401);
});

test('outside the app shell the pairing buttons are not drawn', async ({ page }) => {
  await signIn(page);
  await page.goto(baseURL + '/account');
  const section = page.getByTestId('paired-devices');
  await expect(section.getByRole('heading', { name: 'Devices' })).toBeVisible();
  await expect(section.getByRole('button', { name: 'Pair this phone' })).toBeHidden();
});
