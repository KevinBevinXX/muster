import http from 'node:http';
import { Locator, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { chiefRuntimesFakePort, chiefRuntimesPort, password, versionTerm } from '../lib/env';
import { mkchat } from '../lib/mirrorhost';
import { startServer, Running } from '../lib/server';

// THE CHIEF'S SESSIONS ON MORE THAN ONE RUNTIME (internal/api/session_runtime.go),
// in a real browser, against a real muster-server: the chief panel's thread list
// offers a "New session" picker — Claude Code with a Claude model, or an
// OpenRouter model from the model search — and a session started there is listed
// with its runtime and model; one thread is the DEFAULT (where plugin messages
// and `chief ask` go), and "Make default" moves it.
//
// The chief is a claude-code agent of this spec's own (e2e/mkchat), on a server
// of its own with both runtimes offered (MUSTER_CHIEF_SESSION_RUNTIMES) over the
// noop driver; the model search's upstream is a fake this spec runs (the e2eterm
// build's MUSTER_E2E_OPENROUTER_MODELS). Nothing is provisioned and no turn is
// sent: what is under test is the picker, the stored session and the list.

const origin = `http://localhost:${chiefRuntimesPort}`;
const fake = `http://127.0.0.1:${chiefRuntimesFakePort}`;
const chief = 'e2e-runtimes-chief-' + Date.now().toString(36);
const MODELS = ['vendor-x/model-y', 'acme/other-1'];

let server: Running | undefined;
let upstream: http.Server | undefined;

test.use({ storageState: { cookies: [], origins: [] }, viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

test.beforeAll(async () => {
  test.setTimeout(120_000);
  upstream = http.createServer((req, res) => {
    res.setHeader('Content-Type', 'application/json');
    if ((req.url || '').startsWith('/models')) return res.end(JSON.stringify({ data: MODELS.map((id) => ({ id })) }));
    res.end('{}');
  });
  await new Promise<void>((r) => upstream!.listen(chiefRuntimesFakePort, '127.0.0.1', () => r()));
  server = await startServer(versionTerm, chiefRuntimesPort, {
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
    MUSTER_AGENT_KINDS: 'gateway,claude-code',
    MUSTER_AGENT_CC_IMAGE: 'ghcr.io/example-org/claude-code-agent:e2e',
    MUSTER_AGENT_CC_ACCOUNTS: 'alpha',
    MUSTER_AGENT_CC_TOKEN_ALPHA: 'sk-ant-oat01-FAKE-e2e-runtimes',
    // The runtime bundle a claude-code chief's gateway sidecar is built from
    // (refused at boot without it when both runtimes are offered).
    MUSTER_AGENT_GATEWAY: 'hooks-sha256',
    MUSTER_AGENT_GATEWAY_MODEL: 'e2e-sentinel',
    MUSTER_AGENT_RUNTIME_CONFIG: '{}',
    MUSTER_AGENT_RUNTIME_INSTALL: 'exec true',
    MUSTER_CHIEF_SESSION_RUNTIMES: 'claude-code,gateway',
    MUSTER_E2E_OPENROUTER_MODELS: fake + '/models',
  });
  mkchat(chief, null);
  mkchat(chief, [{ title: 'the original', hoursAgo: 2, messages: [{ role: 'user', content: 'hello chief' }] }], {
    MUSTER_E2E_DISPLAY_NAME: 'chief',
    MUSTER_E2E_THREADS: '1',
    MUSTER_E2E_KIND: 'claude-code',
    MUSTER_E2E_CC_ACCOUNT: 'alpha',
  });
});

test.afterAll(async () => {
  mkchat(chief, null);
  await server?.stop();
  await new Promise<void>((r) => (upstream ? upstream.close(() => r()) : r()));
});

async function signIn(page: Page) {
  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
}

// openThreads opens the chief panel (its toggle is delegated on the document)
// and then its thread list.
async function openThreads(page: Page) {
  if ((await page.locator('#chief-panel').getAttribute('data-chief-panel-open')) !== '1') {
    await page.evaluate(() => {
      const b = document.createElement('button');
      b.setAttribute('data-chief-panel-toggle', '');
      b.style.cssText = 'position:fixed;left:-9999px;top:0';
      document.body.appendChild(b);
      b.click();
    });
    await expect(page.locator('#chief-panel')).toHaveAttribute('data-chief-panel-open', '1');
  }
  await expect(page.locator('#chief-thread-search')).toBeAttached({ timeout: 15_000 });
  await page.locator('label[for="chief-threads"]').first().click();
  await expect(page.locator('#chief-thread-search')).toBeVisible();
}

// row is a thread's row: its button, or — on a thread that is not the default —
// the wrapper holding the button and its "Make default".
const row = (page: Page, text: string): Locator =>
  page.locator('#chief-thread-results > *').filter({ hasText: text });

test('a session is started on a picked runtime and model, listed with them, and can be made the default', async ({ page }) => {
  await signIn(page);
  await page.goto(origin + '/tasks');
  await openThreads(page);

  const picker = page.locator('[data-session-picker]');
  await expect(picker).toBeVisible();
  await expect(picker.locator('form[data-new-session]')).toHaveCount(2);
  // The original thread is the default, and names no runtime of its own.
  const original = row(page, 'the original');
  await expect(original.locator('[data-session-default]')).toHaveCount(1);
  await expect(original.locator('[data-session-runtime]')).toHaveCount(0);

  // Claude Code on sonnet.
  const cc = picker.locator('form[data-new-session="claude-code"]');
  await cc.locator('select[name="model"]').selectOption('sonnet');
  await cc.getByRole('button', { name: 'Start' }).click();
  await expect(page.locator('[data-chief-active-session]')).not.toHaveAttribute('data-chief-active-session', '');
  await openThreads(page);
  await expect(page.locator('#chief-thread-results [data-session-runtime="claude-code"]')).toHaveText('Claude Code · sonnet');

  // An OpenRouter model, picked from the model search.
  const gw = page.locator('[data-session-picker] form[data-new-session="gateway"]');
  await gw.locator('#chief-session-model-input').fill('model-y');
  const option = gw.locator('[data-combobox-option][data-value="openrouter/vendor-x/model-y"]');
  await expect(option).toBeVisible({ timeout: 15_000 });
  await option.click();
  await expect(gw.locator('input[name="model"][data-combobox-hidden]')).toHaveValue('openrouter/vendor-x/model-y');
  await gw.getByRole('button', { name: 'Start' }).click();
  await openThreads(page);
  const routed = page.locator('#chief-thread-results [data-session-runtime="gateway"]');
  await expect(routed).toHaveText('OpenRouter · vendor-x/model-y');
  // Both new sessions are listed, each once, and neither is the default.
  await expect(page.locator('#chief-thread-results [data-session-runtime]')).toHaveCount(2);
  await expect(page.locator('#chief-thread-results [data-session-default]')).toHaveCount(1);
  await expect(row(page, 'the original').locator('[data-session-default]')).toHaveCount(1);

  // Make the OpenRouter session the default: the badge moves to it.
  const gwRow = page.locator('#chief-thread-results > *').filter({ has: page.locator('[data-session-runtime="gateway"]') });
  await gwRow.locator('[data-make-default]').click();
  await openThreads(page);
  const gwRowAfter = page.locator('#chief-thread-results > *').filter({ has: page.locator('[data-session-runtime="gateway"]') });
  await expect(gwRowAfter.locator('[data-session-default]')).toHaveCount(1);
  await expect(page.locator('#chief-thread-results [data-session-default]')).toHaveCount(1);
  await expect(row(page, 'the original').locator('[data-make-default]')).toHaveCount(1);

  // It is stored, not only drawn: a fresh load of the panel draws the same.
  await page.reload();
  await openThreads(page);
  await expect(
    page.locator('#chief-thread-results > *').filter({ has: page.locator('[data-session-runtime="gateway"]') }).locator('[data-session-default]'),
  ).toHaveCount(1);
});

test('a bad model is refused and starts no session', async ({ page }) => {
  await signIn(page);
  const before = await page.request.get(origin + '/ui/chief/threads/search?q=');
  const count = ((await before.text()).match(/data-chief-thread=/g) || []).length;
  // The route the picker posts to, with a model the store refuses.
  const panel = await page.request.get(origin + '/ui/chief/panel');
  const post = /hx-post="(\/agents\/\d+\/sessions)\?surface=chief-panel"/.exec(await panel.text());
  expect(post, 'the panel names the session-create route').not.toBeNull();
  const r = await page.request.post(origin + post![1], {
    form: { runtime: 'gateway', model: 'not a model' },
    headers: { Origin: origin },
  });
  expect(r.status()).toBe(400);
  const after = await page.request.get(origin + '/ui/chief/threads/search?q=');
  expect(((await after.text()).match(/data-chief-thread=/g) || []).length).toBe(count);
});
