import { request as pwRequest } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { hookToken, kindsPort, password, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';

// The dispatch form's agent-kind picker and the Claude account pin, at a phone
// width (412×915), against a REAL muster-server with the claude-code kind
// enabled over the noop driver. "Save for later" creates the row and provisions
// nothing, so no pod, no image and no token is ever used — the tokens below are
// obvious fakes and never leave this process.
test.use({ viewport: { width: 412, height: 915 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

const origin = `http://localhost:${kindsPort}`;
let server: Running | undefined;

test.beforeAll(async () => {
  server = await startServer(versionA, kindsPort, {
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
    MUSTER_AGENT_KINDS: 'gateway,claude-code',
    MUSTER_AGENT_CC_IMAGE: 'ghcr.io/example-org/claude-code-agent:e2e',
    MUSTER_AGENT_CC_ACCOUNTS: 'alpha,beta',
    MUSTER_AGENT_CC_TOKEN_ALPHA: 'sk-ant-oat01-FAKE-e2e-alpha',
    MUSTER_AGENT_CC_TOKEN_BETA: 'sk-ant-oat01-FAKE-e2e-beta',
  });
});
test.afterAll(async () => {
  await server?.stop();
});

interface AgentRow {
  id: number;
  name: string;
  kind: string;
  ccAccount: string;
  workerPool: string;
}

async function agentRows(): Promise<AgentRow[]> {
  const ctx = await pwRequest.newContext({ baseURL: origin, extraHTTPHeaders: { Authorization: `Bearer ${hookToken}` } });
  const r = await ctx.get('/api/agents');
  expect(r.status(), await r.text()).toBe(200);
  const rows = (await r.json()) as AgentRow[];
  await ctx.dispose();
  return rows;
}

test('the kind picker dispatches a Claude Code agent onto a pinned or pooled account, and Standard stays the default', async ({ page }) => {
  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);

  const open = async () => {
    await page.goto(origin + '/agents');
    await page.click('#fab-agents');
    await expect(page.locator('#agent-modal [data-kind-picker]')).toBeVisible();
  };
  const sheet = page.locator('#agent-modal form');
  const account = sheet.locator('select[name="cc_account"]');
  const model = sheet.locator('input[name="model"]');
  const pool = sheet.locator('input[name="worker_pool"]');
  // save submits the form and returns the ONE agent row it created (the set of
  // rows is diffed, so a row left by an earlier run cannot answer).
  const save = async (note: string): Promise<AgentRow> => {
    const before = new Set((await agentRows()).map((a) => a.name));
    await sheet.locator('textarea[name="note_text"]').fill(note);
    const resp = page.waitForResponse((r) => r.url().endsWith('/agents') && r.request().method() === 'POST');
    await sheet.locator('button[value="save"]').click();
    expect((await resp).status()).toBe(200);
    const created = (await agentRows()).filter((a) => !before.has(a.name));
    expect(created.length, 'exactly one agent created').toBe(1);
    return created[0];
  };

  // 1. Default: Standard checked, the account pin hidden AND disabled (so it is
  //    not submitted), the model field live. Nothing scrolls sideways at 412px,
  //    and both options are thumb-sized.
  await open();
  await expect(sheet.locator('input[name="kind"][value="gateway"]')).toBeChecked();
  await expect(account).toBeHidden();
  await expect(account).toBeDisabled();
  await expect(pool).toBeHidden();
  await expect(pool).toBeDisabled();
  await expect(model).toBeEnabled();
  for (const kind of ['gateway', 'claude-code']) {
    const box = await sheet.locator(`[data-kind-option="${kind}"]`).boundingBox();
    expect(box!.height, `${kind} option height`).toBeGreaterThanOrEqual(44);
    expect(box!.x + box!.width, `${kind} option fits the viewport`).toBeLessThanOrEqual(412);
  }
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw).toBeLessThanOrEqual(cw);

  // 2. Claude Code + a pinned account.
  await sheet.locator('[data-kind-option="claude-code"]').click();
  await expect(account).toBeVisible();
  await expect(account).toBeEnabled();
  await expect(model).toBeDisabled();
  // A claude-code agent takes a repository (its session starts in a checkout),
  // so the repo field stays live.
  await expect(sheet.locator('input[name="repo"]')).toBeEnabled();
  await expect(account.locator('option')).toHaveText(['Auto (least rate-limited)', 'alpha', 'beta']);
  await expect(pool).toBeVisible();
  await expect(pool).toBeEnabled();
  await expect(sheet.locator('#worker-pool-help')).toContainText('never holds a GitHub grant');
  await account.selectOption('beta');
  const pinned = await save('e2e kinds: pinned to beta');

  // 3. Claude Code with the pool choosing: no marks and no LIVE agent anywhere
  //    (a saved agent is `stopped`), so the name tiebreak picks alpha — NOT the
  //    beta pinned above, which is the control that this one was not pinned.
  await open();
  await sheet.locator('[data-kind-option="claude-code"]').click();
  const pooled = await save('e2e kinds: pool choice');

  // 3b. Claude Code in a worker pool. The server is the only check on the
  //     name: a name it refuses is a 409 whose sentence the operator reads in
  //     the toast, and no row is made.
  await open();
  await sheet.locator('[data-kind-option="claude-code"]').click();
  await sheet.locator('textarea[name="note_text"]').fill('e2e kinds: bad pool');
  await pool.fill('-bad');
  const beforeBad = (await agentRows()).length;
  const refused = page.waitForResponse((r) => r.url().endsWith('/agents') && r.request().method() === 'POST');
  await sheet.locator('button[value="save"]').click();
  expect((await refused).status()).toBe(409);
  await expect(page.locator('#action-toasts')).toContainText(`worker_pool "-bad" is not 1-63 of [a-z0-9-], not starting with '-'`);
  expect((await agentRows()).length, 'a refused pool creates no agent').toBe(beforeBad);

  await open();
  await sheet.locator('[data-kind-option="claude-code"]').click();
  await pool.fill('reviewers');
  const worker = await save('e2e kinds: pool worker');

  // 4. Back to Standard after toggling: the pin and the pool are disabled again
  //    and the row is a gateway agent with no account and no pool.
  await open();
  await sheet.locator('[data-kind-option="claude-code"]').click();
  await account.selectOption('alpha');
  await pool.fill('reviewers');
  await sheet.locator('[data-kind-option="gateway"]').click();
  await expect(account).toBeDisabled();
  await expect(pool).toBeHidden();
  await expect(pool).toBeDisabled();
  await expect(model).toBeEnabled();
  await expect(sheet.locator('input[name="repo"]')).toBeEnabled();
  const standard = await save('e2e kinds: standard');

  expect([pinned.kind, pinned.ccAccount]).toEqual(['claude-code', 'beta']);
  expect([pooled.kind, pooled.ccAccount]).toEqual(['claude-code', 'alpha']);
  expect([standard.kind, standard.ccAccount]).toEqual(['gateway', '']);
  expect([worker.kind, worker.ccAccount, worker.workerPool]).toEqual(['claude-code', 'alpha', 'reviewers']);
  expect([pinned.workerPool, pooled.workerPool, standard.workerPool]).toEqual(['', '', '']);

  // 5. The card says which kind and account, and nothing on the page carries a token.
  await page.goto(origin + '/agents');
  await expect(page.getByText('Claude Code · beta').first()).toBeVisible();
  // …and a pool worker's card says which pool.
  await expect(page.locator(`#agent-${worker.id} [data-worker-pool="reviewers"]`)).toBeVisible();
  expect(await page.content()).not.toContain('sk-ant-oat01');
});
