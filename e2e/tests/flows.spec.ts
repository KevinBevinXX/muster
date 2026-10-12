import { request as pwRequest } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { baseURL, hookToken } from '../lib/env';

// Browser flows (muster 0012), end to end at a phone's width: the owner mints a
// RUNNER token and queues a flow on /flows, a runner — a plain HTTP client
// holding only that token — claims the job and reports, and the owner's page
// shows the result. The shared HOOK token is refused at the runner door (the
// negative control: it acts as the admin and every hook script holds it).
//
// The host label is unique per run, so a job left queued by an earlier run
// (same database) can never be the one this run's runner claims.

test.use({ viewport: { width: 412, height: 915 } });

test('queue a flow on /flows, a runner claims and reports, the page shows it (412px)', async ({ page }) => {
  const host = `e2e${Date.now()}`;
  await page.goto('/flows');
  await expect(page.getByRole('heading', { name: 'Browser flows' })).toBeVisible();

  // 1. Mint a runner token for this host; it is shown once.
  await page.fill('#runner-host', host);
  await page.getByRole('button', { name: 'Create runner token' }).click();
  const token = await page.locator('#minted-runner-token').inputValue();
  expect(token).toMatch(/^mfr_[A-Za-z0-9_-]{43}$/);

  // 2. Queue a read flow for that host's `personal` browser.
  await page.goto('/flows');
  await expect(page.locator('#minted-runner-token')).toHaveCount(0);
  await page.selectOption('#flow-name', 'muse.poll');
  await page.fill('#flow-host', host);
  await page.fill('#flow-instance', 'personal');
  await page.fill('#flow-param-thread', 'E2E side chat');
  await page.getByRole('button', { name: 'Queue flow' }).click();
  await expect(page).toHaveURL(/\/flows$/);
  const job = page.locator('[data-flow-job-id]').filter({ hasText: 'E2E side chat' }).first();
  await expect(job).toHaveAttribute('data-flow-status', 'queued');
  const id = await job.getAttribute('data-flow-job-id');

  // 3. The runner door: the hook token is refused; the runner token claims
  // this job and reports it.
  const asHook = await pwRequest.newContext({ baseURL, extraHTTPHeaders: { Authorization: `Bearer ${hookToken}` } });
  const refused = await asHook.post('/runner/flows/claim', { data: { instances: ['personal'], waitSeconds: 1 } });
  expect(refused.status()).toBe(401);
  await asHook.dispose();

  const runner = await pwRequest.newContext({ baseURL, extraHTTPHeaders: { Authorization: `Bearer ${token}` } });
  const claim = await runner.post('/runner/flows/claim', { data: { instances: ['personal'], waitSeconds: 5 } });
  expect(claim.status(), await claim.text()).toBe(200);
  const claimed = await claim.json();
  expect(String(claimed.id)).toBe(id);
  expect(claimed.status).toBe('running');
  const result = await runner.post(`/runner/flows/${id}/result`, { data: { outcome: 'done', exitCode: 0, output: 'e2e muse reply' } });
  expect(result.status(), await result.text()).toBe(200);
  await runner.dispose();

  // 4. The page shows it done, with the output, and nothing overflows 412px.
  await page.reload();
  const done = page.locator(`[data-flow-job-id="${id}"]`);
  await expect(done).toHaveAttribute('data-flow-status', 'done');
  await expect(done).toContainText('e2e muse reply');
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  expect(overflow).toBeLessThanOrEqual(0);
});
