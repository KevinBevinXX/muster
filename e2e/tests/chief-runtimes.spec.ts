import { Locator, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { chiefRuntimesPort, password, versionTerm } from '../lib/env';
import { mkchat } from '../lib/mirrorhost';
import { startServer, Running } from '../lib/server';

// THE CHIEF'S SESSIONS ON MORE THAN ONE RUNTIME (internal/api/session_runtime.go),
// in a real browser, against a real muster-server: the chief panel's thread list
// offers a "New session" picker — Claude Code with a Claude model — and a
// session started there is listed with its runtime and model; one thread is the
// DEFAULT (where plugin messages and `chief ask` go), and "Make default" moves
// it.
//
// The chief is a claude-code agent of this spec's own (e2e/mkchat), on a server
// of its own over the noop driver. Its MUSTER_CHIEF_SESSION_RUNTIMES still names
// the retired `gateway` beside claude-code, as a deployment from before its
// removal does: the server boots, ignores it, and offers claude-code alone.
// Nothing is provisioned and no turn is sent: what is under test is the picker,
// the stored session and the list.

const origin = `http://localhost:${chiefRuntimesPort}`;
const chief = 'e2e-runtimes-chief-' + Date.now().toString(36);

let server: Running | undefined;

test.use({ storageState: { cookies: [], origins: [] }, viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

test.beforeAll(async () => {
  test.setTimeout(120_000);
  server = await startServer(versionTerm, chiefRuntimesPort, {
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
    MUSTER_AGENT_KINDS: 'gateway,claude-code',
    MUSTER_AGENT_CC_IMAGE: 'ghcr.io/example-org/claude-code-agent:e2e',
    MUSTER_AGENT_CC_ACCOUNTS: 'alpha',
    MUSTER_AGENT_CC_TOKEN_ALPHA: 'sk-ant-oat01-FAKE-e2e-runtimes',
    // The retired `gateway` entry: ignored with one warning, never refused.
    MUSTER_CHIEF_SESSION_RUNTIMES: 'claude-code,gateway',
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
  // claude-code alone: the retired gateway entry draws no form and no model search.
  await expect(picker.locator('form[data-new-session]')).toHaveCount(1);
  await expect(picker.locator('form[data-new-session="gateway"]')).toHaveCount(0);
  await expect(picker.locator('[data-combobox]')).toHaveCount(0);
  // The original thread is the default, and names no runtime of its own.
  const original = row(page, 'the original');
  await expect(original.locator('[data-session-default]')).toHaveCount(1);
  await expect(original.locator('[data-session-runtime]')).toHaveCount(0);

  // Claude Code on sonnet, then on haiku.
  for (const model of ['sonnet', 'haiku']) {
    const cc = page.locator('[data-session-picker] form[data-new-session="claude-code"]');
    await cc.locator('select[name="model"]').selectOption(model);
    await cc.getByRole('button', { name: 'Start' }).click();
    await expect(page.locator('[data-chief-active-session]')).not.toHaveAttribute('data-chief-active-session', '');
    await openThreads(page);
  }
  const haiku = page.locator('#chief-thread-results [data-session-runtime="claude-code"]').filter({ hasText: 'haiku' });
  await expect(haiku).toHaveText('Claude Code · haiku');
  await expect(
    page.locator('#chief-thread-results [data-session-runtime="claude-code"]').filter({ hasText: 'sonnet' }),
  ).toHaveText('Claude Code · sonnet');
  // Both new sessions are listed, each once, and neither is the default.
  await expect(page.locator('#chief-thread-results [data-session-runtime]')).toHaveCount(2);
  await expect(page.locator('#chief-thread-results [data-session-default]')).toHaveCount(1);
  await expect(row(page, 'the original').locator('[data-session-default]')).toHaveCount(1);

  // Make the haiku session the default: the badge moves to it.
  const haikuRow = () => page.locator('#chief-thread-results > *').filter({ has: page.locator('[data-session-runtime]', { hasText: 'haiku' }) });
  await haikuRow().locator('[data-make-default]').click();
  await openThreads(page);
  await expect(haikuRow().locator('[data-session-default]')).toHaveCount(1);
  await expect(page.locator('#chief-thread-results [data-session-default]')).toHaveCount(1);
  await expect(row(page, 'the original').locator('[data-make-default]')).toHaveCount(1);

  // It is stored, not only drawn: a fresh load of the panel draws the same.
  await page.reload();
  await openThreads(page);
  await expect(haikuRow().locator('[data-session-default]')).toHaveCount(1);
});

test('a bad model is refused and starts no session', async ({ page }) => {
  await signIn(page);
  const before = await page.request.get(origin + '/ui/chief/threads/search?q=');
  const count = ((await before.text()).match(/data-chief-thread=/g) || []).length;
  // The route the picker posts to, with a model the store refuses.
  const panel = await page.request.get(origin + '/ui/chief/panel');
  const post = /hx-post="(\/agents\/\d+\/sessions)\?surface=chief-panel"/.exec(await panel.text());
  expect(post, 'the panel names the session-create route').not.toBeNull();
  // A bad Claude model, and the retired gateway runtime with a model it once took.
  for (const form of [{ runtime: 'claude-code', model: 'not a model' }, { runtime: 'gateway', model: 'openrouter/vendor-x/model-y' }]) {
    const r = await page.request.post(origin + post![1], { form, headers: { Origin: origin } });
    expect(r.status()).toBe(400);
  }
  const after = await page.request.get(origin + '/ui/chief/threads/search?q=');
  expect(((await after.text()).match(/data-chief-thread=/g) || []).length).toBe(count);
});
