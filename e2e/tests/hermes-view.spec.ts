import fs from 'node:fs';
import path from 'node:path';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { hermesViewPort } from '../lib/env';
import { MirrorRig } from '../lib/mirrorhost';

// A HERMES WINDOW, ON A REAL SERVER (the e2eterm binary, mirror plugin on) AND
// A REAL muster-mirror whose only fake is tmux (docs/design/hermes.md,
// Phase 2). The conversation is a REAL state.db created from Hermes's schema
// at the pin, which the fake host writes as Hermes's gateway would
// (cmd/muster-mirror/e2ehermes.go: rows.jsonl → messages), and the rendered
// view reads it with the real reader.
//
// 🔴 REGRESSION TEST. On origin/main a Hermes window has no rendered view: its
// card opens the Terminal, /mirror/view redirects there, and the card's kind
// is the pane's command (python3.14).

const rig = new MirrorRig(hermesViewPort, 'e2e-hermes');
const sid = 'local-' + 'ab'.repeat(32);
const t0 = Math.floor(Date.now() / 1000);
let home = '';
let rows = '';

function row(r: Record<string, string>) {
  fs.appendFileSync(rows, JSON.stringify({ session: sid, ...r }) + '\n');
}

test.beforeAll(async () => {
  test.setTimeout(180_000);
  await rig.start();
  home = fs.mkdtempSync(path.join(rig.dir, 'hermes-'));
  rows = path.join(home, 'rows.jsonl');
  row({ role: 'user', content: 'list the hermes fixtures' });
  row({
    role: 'assistant',
    content: 'Listing them now.',
    tool_calls: JSON.stringify([{ id: 'call_e2e_1', type: 'function', function: { name: 'terminal', arguments: JSON.stringify({ command: 'ls fixtures/' }) } }]),
  });
  row({ role: 'tool', tool_call_id: 'call_e2e_1', content: JSON.stringify({ output: 'a.txt\nb.txt', exit_code: 0 }) });
  const windows = path.join(rig.dir, 'windows');
  fs.writeFileSync(windows, `plain|@1|0|1|zsh|${t0 - 50}|\n`);
  const hermesWindows = path.join(rig.dir, 'hermes-windows');
  // sess|id|index|active|activity|hermes session (empty: a stale breadcrumb)|status
  fs.writeFileSync(hermesWindows, `herm|@2|0|1|${t0 - 10}|${sid}|idle\nherm|@3|1|0|${t0 - 20}||\n`);
  await rig.startHost({
    MUSTER_MIRROR_E2E_WINDOWS_FILE: windows,
    MUSTER_MIRROR_E2E_HERMES_WINDOWS_FILE: hermesWindows,
    MUSTER_MIRROR_E2E_HERMES_HOME: home,
    MUSTER_MIRROR_E2E_REFRESH: '200ms',
  });
});

test.afterAll(async () => {
  await rig.stop();
});

test.use({ storageState: { cookies: [], origins: [] } });

const viewOf = (window: string) => `${rig.viewPath('herm')}&window=${encodeURIComponent(window)}`;
const card = (page: Page, window: string) =>
  page.locator('#agents-list > article[data-mirror-card]').filter({ has: page.locator(`a[href*="session=herm"][href*="window=${encodeURIComponent(window)}"]`) });

test('a Hermes window opens a read-only rendered view of its conversation, live', async ({ page }) => {
  await rig.login(page);
  await rig.waitListed(page, 'herm');
  await page.goto(rig.origin + '/agents');

  // The card with a session opens the rendered view; the stale one, the Terminal.
  await expect(card(page, '@2').locator('a[href*="/mirror/view?"]')).toHaveCount(1);
  await expect(card(page, '@3').locator('a[href*="/mirror/terminal?"]')).toHaveCount(1);
  // Both are Hermes cards — the kind chip, never the pane's command — and the
  // filter finds them by kind.
  for (const w of ['@2', '@3']) await expect(card(page, w).locator('[data-mirror-kind]')).toContainText('Hermes');
  await page.locator('#agents-search').fill('kind:hermes');
  await expect(page.locator('#agents-list > article[data-mirror-card]:visible')).toHaveCount(2);
  await page.locator('#agents-search').fill('');

  await page.goto(rig.origin + viewOf('@2'));
  await expect(page).toHaveURL(/\/mirror\/view\?/);
  const log = page.locator('#mv-log');
  await expect(log.locator('[data-bubble="user"]').first()).toContainText('list the hermes fixtures', { timeout: 20_000 });
  await expect(log).toContainText('Listing them now.');
  await expect(log.locator('[data-tool-row]').first()).toContainText('ls fixtures/');

  // 🔴 Read-only: the composer is disabled and says why; there is no queue.
  await expect(page.locator('#mv-input')).toBeDisabled();
  await expect(page.locator('#mv-readonly')).toContainText('muster does not send to Hermes yet');
  await expect(page.locator('#mv-queue, #mv-queue-send')).toHaveCount(0);

  // A turn written while the view is open arrives without a reload.
  row({ role: 'user', content: 'and the second prompt, LIVE' });
  row({ role: 'assistant', content: 'Second reply, also live.' });
  await expect(log.locator('[data-bubble="user"]').last()).toContainText('and the second prompt, LIVE', { timeout: 20_000 });
  await expect(log).toContainText('Second reply, also live.');

  // The stale window has no view: its address goes to the Terminal.
  await page.goto(rig.origin + viewOf('@3'));
  await expect(page).toHaveURL(/\/mirror\/terminal\?/);
});
