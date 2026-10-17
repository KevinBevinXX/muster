import { test, expect } from '../lib/fixtures';

// An agent's chat draws each tool call as ONE collapsed row — tool, target,
// status — that expands to its args and result. docs-sweeper's seeded turn
// (e2e/seed) has two PARALLEL calls whose results arrive after both: each pairs
// with its own result by id, so there are two rows, not four. The two are
// consecutive, so they sit inside one "2 tool calls" group (tool-groups.spec.ts),
// opened first.
test.use({ viewport: { width: 412, height: 915 }, hasTouch: true, isMobile: true });

test('an agent chat shows collapsed tool rows that expand', async ({ page }) => {
  await page.goto('/agents/docs-sweeper');
  const rows = page.locator('#chat-log [data-tool-row]');
  await expect(rows).toHaveCount(2);
  const group = page.locator('#chat-log [data-tool-group]');
  await expect(group).toHaveCount(1);
  await expect(group.locator('summary [data-tool-group-count]')).toHaveText('2 tool calls');
  await group.locator(':scope > summary').click();

  const make = rows.nth(0);
  await expect(make.locator('summary')).toContainText('Bash');
  await expect(make.locator('[data-tool-target]')).toHaveText('make docs');
  await expect(make).toHaveAttribute('data-tool-status', 'ok');
  const read = rows.nth(1);
  await expect(read.locator('[data-tool-target]')).toHaveText('/srv/site/links.txt');
  await expect(read).toHaveAttribute('data-tool-status', 'error');

  // Collapsed until tapped; then the args and the result.
  await expect(make).not.toHaveAttribute('open', '');
  await expect(make.locator('pre').first()).toBeHidden();
  await make.locator('summary').click();
  await expect(make).toHaveAttribute('open', '');
  await expect(make.locator('pre').first()).toContainText('make docs');
  await expect(make.locator('pre').last()).toContainText('built 42 pages');

  // The rows sit between the narration they belong to, and fit the phone.
  await expect(page.locator('#chat-log')).toContainText('The build passes; the link report is missing.');
  for (const r of [make, read]) {
    const b = (await r.boundingBox())!;
    expect(b.x + b.width, 'the row fits 412px').toBeLessThanOrEqual(412);
  }
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw).toBeLessThanOrEqual(cw);
});
