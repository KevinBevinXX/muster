import { execFileSync } from 'node:child_process';
import { test, expect } from '../lib/fixtures';
import type { Page } from '@playwright/test';
import { password, pushPort, versionA } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';

// muster-native Web Push, the browser half: the Enable flow subscribes with the
// SERVER's VAPID key and the device lands on /account; a subscription made
// with an old key is replaced (key rotation); an iPhone Safari tab is told to
// install to the Home Screen rather than "not supported".
//
// pushManager and Notification are STUBBED (in the style of badge.spec.ts): a
// real subscribe would reach a vendor push service, and headless Chromium has
// none. What is real is everything muster does — the key route, the subscribe
// route, the store, the account page — on a server with a real keypair.

const origin = `http://localhost:${pushPort}`;
let server: Running | undefined;

test.use({ viewport: { width: 412, height: 915 }, storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  const out = execFileSync(serverBinary(versionA), ['vapid', 'generate'], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] });
  const env: Record<string, string> = { MUSTER_VAPID_SUBJECT: 'mailto:e2e@example.com' };
  for (const line of out.trim().split('\n')) {
    const i = line.indexOf('=');
    env[line.slice(0, i)] = line.slice(i + 1);
  }
  server = await startServer(versionA, pushPort, env);
});
test.afterAll(async () => {
  await server?.stop();
});

interface StubState {
  perm: NotificationPermission;
  current: { endpoint: string } | null;
  subscribeKeys: number[][];
  unsubscribed: number;
}

// stubPush installs a pushManager and Notification permission the page script
// drives. `existingKey`, when set, starts the page with a subscription made
// with THAT key (the pre-rotation state).
async function stubPush(page: Page, opts: { existingKey?: number[]; perm?: NotificationPermission } = {}) {
  await page.addInitScript((o) => {
    const state = { perm: o.perm ?? 'default', current: null as unknown, subscribeKeys: [] as number[][], unsubscribed: 0 };
    (window as unknown as { __push: unknown }).__push = state;
    class FakeSub {
      endpoint: string;
      options: { applicationServerKey: ArrayBuffer };
      constructor(endpoint: string, key: Uint8Array) {
        this.endpoint = endpoint;
        this.options = { applicationServerKey: key.buffer.slice(0) as ArrayBuffer };
      }
      toJSON() {
        return { endpoint: this.endpoint, keys: { p256dh: 'BStubClientKeyForE2E', auth: 'stubauthsecret' } };
      }
      async unsubscribe() {
        state.unsubscribed++;
        state.current = null;
        return true;
      }
    }
    if (o.existingKey) {
      state.current = new FakeSub('https://fcm.googleapis.com/fcm/send/e2e-stale-' + Math.random().toString(36).slice(2), new Uint8Array(o.existingKey));
    }
    const pm = {
      async getSubscription() {
        return state.current;
      },
      async subscribe(sopts: { applicationServerKey: Uint8Array }) {
        const k = new Uint8Array(sopts.applicationServerKey);
        state.subscribeKeys.push(Array.from(k));
        state.current = new FakeSub('https://fcm.googleapis.com/fcm/send/e2e-' + Math.random().toString(36).slice(2), k);
        return state.current;
      },
    };
    Object.defineProperty(ServiceWorkerRegistration.prototype, 'pushManager', { configurable: true, get: () => pm });
    Object.defineProperty(Notification, 'permission', { configurable: true, get: () => state.perm });
    Notification.requestPermission = async () => (state.perm = 'granted');
  }, opts);
}

async function signIn(page: Page) {
  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
}

async function serverKey(page: Page): Promise<number[]> {
  const r = await page.request.get(origin + '/api/push/vapid-public-key');
  expect(r.status()).toBe(200);
  const key = ((await r.json()) as { key: string }).key;
  const b64 = key.replace(/-/g, '+').replace(/_/g, '/') + '='.repeat((4 - (key.length % 4)) % 4);
  return Array.from(Buffer.from(b64, 'base64'));
}

const pushState = (page: Page) => page.evaluate(() => (window as unknown as { __push: StubState }).__push);

test('Enable subscribes with the server key and the device appears on the account page', async ({ page }) => {
  await stubPush(page);
  await signIn(page);
  const key = await serverKey(page);
  expect(key.length, 'an uncompressed P-256 public key').toBe(65);

  await page.goto(origin + '/tasks');
  const enable = page.locator('#enable-push');
  await expect(enable).toBeVisible();
  const saved = page.waitForResponse((r) => r.url().endsWith('/api/push/subscribe') && r.request().method() === 'POST');
  await enable.click();
  expect((await saved).status()).toBe(200);
  await expect(page.locator('#push-on')).toBeVisible();
  await expect(enable).toBeHidden();
  const st = await pushState(page);
  expect(st.subscribeKeys).toEqual([key]);

  await page.goto(origin + '/account');
  await expect(page.locator('[data-testid="push-device"]')).toHaveCount(1);
  await expect(page.locator('[data-testid="push-preferences"] input[type=checkbox]:checked')).toHaveCount(8); // every push.Events toggle, on by default (plugin: PR C; attention: the attention queue)
});

test('a subscription made with an old VAPID key is replaced with one on the current key', async ({ page }) => {
  const stale = Array.from({ length: 65 }, (_, i) => (i === 0 ? 4 : (i * 7) % 256));
  await stubPush(page, { existingKey: stale, perm: 'granted' });
  await signIn(page);
  const key = await serverKey(page);
  const resubscribed = page.waitForResponse((r) => r.url().endsWith('/api/push/subscribe') && r.request().method() === 'POST');
  await page.goto(origin + '/tasks');
  expect((await resubscribed).status()).toBe(200);
  await expect.poll(async () => (await pushState(page)).unsubscribed).toBe(1);
  const st = await pushState(page);
  expect(st.subscribeKeys, 'resubscribed exactly once, with the server key').toEqual([key]);
  await expect(page.locator('#push-on')).toBeVisible();
});

test.describe('on an iPhone Safari tab', () => {
  test.use({
    userAgent:
      'Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Mobile/15E148 Safari/604.1',
  });
  test('the page says to install to the Home Screen, not that push is unsupported', async ({ page }) => {
    // A Safari tab has no PushManager: only an installed web app does.
    await page.addInitScript(() => {
      delete (window as unknown as { PushManager?: unknown }).PushManager;
    });
    await signIn(page);
    await page.goto(origin + '/tasks');
    const status = page.locator('#push-status');
    await expect(status).toContainText('Add to Home Screen');
    await expect(status).not.toContainText('not supported');
    await expect(page.locator('#enable-push')).toBeHidden();
  });
});

// The ATTENTION notification, the service worker's half: a new item is shown
// with its per-item tag and a tap target on /attention, and the "resolved"
// message carrying the same tag closes it. The payloads are EXACTLY what the
// server sends — internal/api's TestTheAttentionPayloadsAreWhatTheWorkerReads
// pins the same two JSON documents — delivered to the REAL worker's push
// handler through the browser's own push path (CDP
// ServiceWorker.deliverPushMessage), since headless Chromium has no vendor push
// service.
//
// ⚠ THE NOTIFICATION SINK IS STUBBED, in the worker (as pushManager is stubbed
// in the page above): headless Chromium refuses showNotification outright
// ("No notification permission has been granted", whatever permission the
// context is given — measured). So registration.showNotification and
// getNotifications are replaced by a recorder; what is real is the push event,
// the worker's handler and its branch on the payload.
test('an attention notification is shown with its tag and link, and its resolved message closes it', async ({ page, context }) => {
  await signIn(page);
  const registered = context.serviceWorkers().length ? Promise.resolve(context.serviceWorkers()[0]) : context.waitForEvent('serviceworker');
  await page.goto(origin + '/tasks');
  const worker = await registered;
  await page.evaluate(async () => {
    await navigator.serviceWorker.ready;
  });
  await worker.evaluate(() => {
    type Note = { title: string; body: string; tag: string; url: string; open: boolean; close: () => void };
    const sink: Note[] = [];
    (self as unknown as { __notes: Note[] }).__notes = sink;
    const reg = (self as unknown as { registration: ServiceWorkerRegistration }).registration;
    reg.showNotification = async (title: string, o?: NotificationOptions) => {
      const n: Note = { title, body: o?.body ?? '', tag: o?.tag ?? '', url: (o?.data || {}).url, open: true, close: () => (n.open = false) };
      sink.push(n);
    };
    reg.getNotifications = (async (f?: { tag?: string }) =>
      sink.filter((n) => n.open && (!f?.tag || n.tag === f.tag))) as unknown as typeof reg.getNotifications;
  });
  const cdp = await context.newCDPSession(page);
  const regs: { registrationId: string; scopeURL: string; isDeleted?: boolean }[] = [];
  cdp.on('ServiceWorker.workerRegistrationUpdated', (e) => regs.push(...e.registrations));
  await cdp.send('ServiceWorker.enable');
  await expect.poll(() => regs.find((r) => r.scopeURL === origin + '/' && !r.isDeleted)?.registrationId ?? '').not.toBe('');
  const registrationId = regs.find((r) => r.scopeURL === origin + '/' && !r.isDeleted)!.registrationId;
  const shown = () =>
    worker.evaluate(() =>
      (self as unknown as { __notes: { title: string; body: string; tag: string; url: string; open: boolean }[] }).__notes.map((n) => ({
        title: n.title, body: n.body, tag: n.tag, url: n.url, open: n.open,
      })),
    );
  const deliver = (data: string) => cdp.send('ServiceWorker.deliverPushMessage', { origin, registrationId, data });

  await deliver(
    '{"type":"attention","id":"ask-41","title":"An agent needs your input","body":"worker-7: Deploy the importer now?","tag":"attention-ask-41","data":{"url":"/attention#ask-41"}}',
  );
  const note = { title: 'An agent needs your input', body: 'worker-7: Deploy the importer now?', tag: 'attention-ask-41', url: '/attention#ask-41' };
  await expect.poll(shown).toEqual([{ ...note, open: true }]);
  await deliver('{"type":"resolved","id":"ask-41","tag":"attention-ask-41"}');
  await expect.poll(shown).toEqual([{ ...note, open: false }]);
});
