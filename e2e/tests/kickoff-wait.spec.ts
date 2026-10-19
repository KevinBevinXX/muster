import { execFileSync } from 'node:child_process';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { dsn, repoRoot } from '../lib/env';

// A HELD FIRST TURN, in a real browser on the shared server: a claude-code agent
// whose repository cannot be checked out until its owner approves the agent's
// github connector request. The kickoff deliverer holds its first turn, unsent and
// unstamped, until then (internal/agentkickoff, agents.DecideFirstTurn), and the
// card says so — "waiting for your github approval" — with a link to Connectors,
// where the request is. The click path is the point: the owner's one move.
//
// The row is the fixture e2e/heldfix writes (the state the deliverer leaves).

const held = 'held-kelp-tern';
const heldfix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/heldfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } });

test.beforeAll(() => {
  heldfix('create', held);
});
test.afterAll(() => {
  heldfix('delete', held);
});

const card = (page: Page, name: string) => page.locator('#agents-list > article', { has: page.locator(`a[href="/agents/${name}"]`) });

for (const vp of [
  { label: 'phone', viewport: { width: 390, height: 844 }, touch: true },
  { label: 'desktop', viewport: { width: 1280, height: 900 }, touch: false },
]) {
  test.describe(vp.label, () => {
    test.use({ viewport: vp.viewport, hasTouch: vp.touch, isMobile: vp.touch });

    // 🔴 A REGRESSION TEST: on the commit before, a held agent's card says only
    // "kickoff owed", and nothing links the owner to the approval it is waiting on.
    test('a held kickoff says it waits for the github approval and links to Connectors', async ({ page }) => {
      await page.goto('/agents');
      const c = card(page, held);
      await expect(c).toBeVisible();
      await expect(c.locator('[data-kickoff-owed]')).toBeVisible();
      const line = c.locator('[data-kickoff-awaiting-grant]');
      await expect(line).toBeVisible();
      await expect(line).toContainText('Waiting for your github approval');
      // Only the fixed line crosses: not the recorded detail.
      await expect(line).not.toContainText('could not be cloned');
      // A card that is not held has no such line (the control: the locator CAN miss).
      await expect(card(page, 'docs-sweeper')).toBeVisible();
      await expect(card(page, 'docs-sweeper').locator('[data-kickoff-awaiting-grant]')).toHaveCount(0);

      const link = line.getByRole('link', { name: 'Open Connectors' });
      if (vp.touch) await link.tap();
      else await link.click();
      await page.waitForURL((u) => u.pathname === '/connectors');
    });
  });
}
