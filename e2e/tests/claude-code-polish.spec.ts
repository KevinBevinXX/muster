import { request as pwRequest, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { hookToken, password, polishPort, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';

// Four operator reports, driven through the real UI against a REAL muster-server
// with both agent kinds enabled over the noop driver (which reports a created
// instance ready). Nothing here provisions a pod or uses a real token: the
// setup-tokens are obvious fakes and never leave this process.
//
//   1. a Claude Code agent shows no model selector / OpenRouter controls;
//   2. the Agents list is ordered by most recent activity, and re-orders LIVE;
//   3. the Tasks board prints the activity time it is ordered by;
//   4. a Started agent's STORED status reaches `running` once its instance is ready.
//
// Every test runs at a phone width (412×915) and at a desktop width.

const origin = `http://localhost:${polishPort}`;
let server: Running | undefined;

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  server = await startServer(versionA, polishPort, {
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
    MUSTER_AGENT_KINDS: 'gateway,claude-code',
    MUSTER_AGENT_CC_IMAGE: 'ghcr.io/example-org/claude-code-agent:e2e',
    MUSTER_AGENT_CC_ACCOUNTS: 'alpha,beta',
    MUSTER_AGENT_CC_TOKEN_ALPHA: 'sk-ant-oat01-FAKE-e2e-polish-alpha',
    MUSTER_AGENT_CC_TOKEN_BETA: 'sk-ant-oat01-FAKE-e2e-polish-beta',
  });
});
test.afterAll(async () => {
  await server?.stop();
});

interface AgentRow {
  id: number;
  name: string;
  kind: string;
  status: string;
}

async function machine() {
  return pwRequest.newContext({ baseURL: origin, extraHTTPHeaders: { Authorization: `Bearer ${hookToken}` } });
}

async function agentRows(): Promise<AgentRow[]> {
  const ctx = await machine();
  const r = await ctx.get('/api/agents');
  expect(r.status(), await r.text()).toBe(200);
  const rows = (await r.json()) as AgentRow[];
  await ctx.dispose();
  return rows;
}

async function login(page: Page) {
  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
}

// saveAgent fills the FAB form and presses "Save for later" (nothing provisioned);
// it returns the ONE row that appeared.
async function saveAgent(page: Page, note: string, kind: 'gateway' | 'claude-code' = 'gateway'): Promise<AgentRow> {
  await page.goto(origin + '/agents');
  await page.click('#fab-agents');
  const sheet = page.locator('#agent-modal form');
  await expect(sheet.locator('[data-kind-picker]')).toBeVisible();
  if (kind === 'claude-code') await sheet.locator('[data-kind-option="claude-code"]').click();
  const before = new Set((await agentRows()).map((a) => a.name));
  await sheet.locator('textarea[name="note_text"]').fill(note);
  const resp = page.waitForResponse((r) => r.url().endsWith('/agents') && r.request().method() === 'POST');
  await sheet.locator('button[value="save"]').click();
  expect((await resp).status()).toBe(200);
  const created = (await agentRows()).filter((a) => !before.has(a.name));
  expect(created.length, 'exactly one agent created').toBe(1);
  return created[0];
}

// cardOrder reads the Agents list's card ids, top to bottom, restricted to ids.
async function cardOrder(page: Page, ids: number[]): Promise<number[]> {
  const all = await page.locator('#agents-list article[id^="agent-"]').evaluateAll((els) =>
    els.map((e) => Number(e.id.replace('agent-', ''))),
  );
  return all.filter((id) => ids.includes(id));
}

for (const vp of [
  { label: 'phone', viewport: { width: 412, height: 915 }, isMobile: true, hasTouch: true },
  { label: 'desktop', viewport: { width: 1280, height: 800 }, isMobile: false, hasTouch: false },
]) {
  test.describe(`${vp.label} ${vp.viewport.width}x${vp.viewport.height}`, () => {
    test.use({ viewport: vp.viewport, isMobile: vp.isMobile, hasTouch: vp.hasTouch });

    test('1a. picking Claude Code removes the model selector and the OpenRouter options from the dispatch form', async ({ page }) => {
      await login(page);
      await page.goto(origin + '/agents');
      await page.click('#fab-agents');
      const sheet = page.locator('#agent-modal form');
      await expect(sheet.locator('input[name="kind"][value="gateway"]')).toBeChecked();
      // Standard: open Advanced, the model selector is there.
      await sheet.locator('summary[aria-label="Advanced options"]').click();
      const model = sheet.locator('#model-input');
      await expect(model).toBeVisible();
      // Claude Code: the whole gateway-only block leaves the screen — the
      // model selector and the repository — and nothing submits.
      await sheet.locator('[data-kind-option="claude-code"]').click();
      await expect(model).toBeHidden();
      await expect(sheet.locator('summary[aria-label="Advanced options"]')).toBeHidden();
      await expect(sheet.locator('input[name="repo"]')).toBeHidden();
      await expect(sheet.locator('select[name="cc_account"]')).toBeVisible();
      await expect(sheet.locator('input[name="model"]')).toBeDisabled();
      // Back to Standard: it returns.
      await sheet.locator('[data-kind-option="gateway"]').click();
      await expect(model).toBeVisible();
      await expect(sheet.locator('input[name="model"]')).toBeEnabled();
      const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
      expect(sw).toBeLessThanOrEqual(cw);
    });

    test('1b. the task-scoped dispatch confirm hides its Model/Repository rows for Claude Code', async ({ page }) => {
      const ctx = await machine();
      const r = await ctx.post('/api/tasks', { data: { title: `polish ${vp.label} task-dispatch`, body: 'task-scoped dispatch' } });
      expect(r.status(), await r.text()).toBeLessThan(300);
      const task = (await r.json()) as { id: number };
      await ctx.dispose();

      await login(page);
      await page.goto(origin + '/tasks');
      await page.locator(`#task-${task.id} button[aria-label="Dispatch agent for this task"]`).click();
      const sheet = page.locator('#agent-modal form');
      const modelRow = sheet.locator('[data-dispatch-row]').filter({ hasText: 'Model' });
      await expect(modelRow).toBeVisible();
      await sheet.locator('[data-kind-option="claude-code"]').click();
      await expect(modelRow).toBeHidden();
      await expect(sheet.locator('[data-dispatch-row]').filter({ hasText: 'Repository' })).toBeHidden();
      await sheet.locator('[data-kind-option="gateway"]').click();
      await expect(modelRow).toBeVisible();
    });

    test('1c. a Claude Code agent page shows its account where the model would be and never asks OpenRouter', async ({ page }) => {
      await login(page);
      const cc = await saveAgent(page, `polish ${vp.label}: cc detail`, 'claude-code');
      const openrouter: string[] = [];
      page.on('request', (r) => {
        if (r.url().includes('/api/openrouter/')) openrouter.push(r.url());
      });
      await page.goto(origin + '/agents/' + cc.name);
      await expect(page.getByText(/Claude Code · (alpha|beta)/).first()).toBeVisible();
      await expect(page.locator('#model-input')).toHaveCount(0);
      await expect(page.locator('[data-combobox-remote]')).toHaveCount(0);
      await page.waitForTimeout(1000);
      expect(openrouter, 'requests to the OpenRouter proxy').toEqual([]);
      // Server side: a model write on a claude-code agent is refused.
      const res = await page.request.post(`${origin}/agents/${cc.id}/model`, {
        form: { model: 'openrouter/vendor/some-model' },
        headers: { Origin: origin },
      });
      expect(res.status()).toBe(409);
      expect(await page.content()).not.toContain('sk-ant-oat01');
    });

    test('2+4. the Agents list is most-recently-active first and re-orders live; a Started agent is stored running', async ({ page }) => {
      await login(page);
      const oldest = await saveAgent(page, `polish ${vp.label}: oldest`);
      const middle = await saveAgent(page, `polish ${vp.label}: middle`);
      const newest = await saveAgent(page, `polish ${vp.label}: newest`);
      const ids = [oldest.id, middle.id, newest.id];

      await page.goto(origin + '/agents');
      await expect.poll(() => cardOrder(page, ids)).toEqual([newest.id, middle.id, oldest.id]);

      // Start the OLDEST from outside this page. Nothing in this tab makes a
      // request: the list must re-order off the server's own event, once the
      // started agent's instance is ready and its stored status is promoted.
      const res = await page.request.post(`${origin}/agents/${oldest.id}/start`, { headers: { Origin: origin } });
      expect(res.status()).toBe(200);

      await expect
        .poll(async () => (await agentRows()).find((a) => a.id === oldest.id)?.status, { timeout: 30_000 })
        .toBe('running');
      await expect.poll(() => cardOrder(page, ids), { timeout: 20_000 }).toEqual([oldest.id, newest.id, middle.id]);
      // Live, not reloaded: the page never navigated.
      expect(new URL(page.url()).pathname).toBe('/agents');
      // The now-running card's recent-log slot must not be left as a skeleton by
      // the SSE-driven re-render (it is refreshed after the list settles).
      const slot = page.locator(`#agent-recent-${oldest.id}`);
      await expect(slot).toHaveCount(1);
      await expect(slot.locator('.animate-pulse')).toHaveCount(0, { timeout: 10_000 });

      // A SECOND server-driven re-render, while that slot is already loaded: the
      // list re-renders every slot as a skeleton and morphs it in, so a slot that
      // is not re-fetched afterwards is stranded as a skeleton for good.
      const res2 = await page.request.post(`${origin}/agents/${middle.id}/start`, { headers: { Origin: origin } });
      expect(res2.status()).toBe(200);
      await expect.poll(() => cardOrder(page, ids), { timeout: 30_000 }).toEqual([middle.id, oldest.id, newest.id]);
      await expect(slot.locator('.animate-pulse')).toHaveCount(0, { timeout: 10_000 });
    });

    test('3. the Tasks board prints the activity time it is ordered by', async ({ page }) => {
      const ctx = await machine();
      const make = async (title: string) => {
        const r = await ctx.post('/api/tasks', { data: { title, body: title } });
        expect(r.status(), await r.text()).toBeLessThan(300);
        return ((await r.json()) as { id: number }).id;
      };
      const older = await make(`polish ${vp.label} older task`);
      const newer = await make(`polish ${vp.label} newer task`);

      await login(page);
      await page.goto(origin + '/tasks');
      const pos = async () =>
        (await page.locator('[data-tasks-flat] > [id^="task-"]').evaluateAll((els) => els.map((e) => e.id))).filter((id) =>
          [`task-${older}`, `task-${newer}`].includes(id),
        );
      await expect.poll(pos).toEqual([`task-${newer}`, `task-${older}`]);

      // Activity on the OLDER task floats it — and its card prints that time.
      const c = await ctx.post(`/api/tasks/${older}/comments`, { data: { body: 'progress on the older task' } });
      expect(c.status(), await c.text()).toBeLessThan(300);
      await ctx.dispose();
      await expect.poll(pos, { timeout: 15_000 }).toEqual([`task-${older}`, `task-${newer}`]);
      const at = async (id: number) => Number(await page.locator(`#task-${id} [data-task-activity]`).getAttribute('data-task-activity'));
      const [o, n] = [await at(older), await at(newer)];
      expect(o, 'the floated card prints a NEWER activity time than the one below it').toBeGreaterThan(n);
    });
  });
}
