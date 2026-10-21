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

let hostEnv: NodeJS.ProcessEnv = {};

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
  hostEnv = {
    MUSTER_MIRROR_E2E_WINDOWS_FILE: windows,
    MUSTER_MIRROR_E2E_HERMES_WINDOWS_FILE: hermesWindows,
    MUSTER_MIRROR_E2E_HERMES_HOME: home,
    MUSTER_MIRROR_E2E_REFRESH: '200ms',
  };
  await rig.startHost(hostEnv);
});

test.afterAll(async () => {
  await rig.stop();
});

test.use({ storageState: { cookies: [], origins: [] } });

const viewOf = (window: string) => `${rig.viewPath('herm')}&window=${encodeURIComponent(window)}`;
const card = (page: Page, window: string) =>
  page
    .locator('#agents-list > article[data-mirror-card]')
    .filter({ has: page.locator(`a[href*="session=${window === '@1' ? 'plain' : 'herm'}"][href*="window=${encodeURIComponent(window)}"]`) });

test('a Hermes window opens a read-only rendered view of its conversation, live', async ({ page }) => {
  await rig.login(page);
  await rig.waitListed(page, 'herm');
  await page.goto(rig.origin + '/agents');

  // The card with a session opens the rendered view; the stale one, the Terminal.
  await expect(card(page, '@2').locator('a[href*="/mirror/view?"]')).toHaveCount(1);
  await expect(card(page, '@3').locator('a[href*="/mirror/terminal?"]')).toHaveCount(1);
  // Both are Hermes cards — the kind chip, never the pane's command — and the
  // filter finds them by kind.
  // This host lists exactly three windows: the plain shell and the two Hermes.
  const hostCards = page.locator(`#agents-list > article[data-mirror-card][data-f-host="${rig.host}"]`);
  await expect(hostCards).toHaveCount(3);
  await expect(card(page, '@1').locator('[data-mirror-kind]')).toContainText('shell');
  for (const w of ['@2', '@3']) await expect(card(page, w).locator('[data-mirror-kind]')).toContainText('Hermes');
  await page.locator('#agents-search').fill('kind:hermes');
  await expect(page.locator(`#agents-list > article[data-mirror-card][data-f-host="${rig.host}"]:visible`)).toHaveCount(2);
  await page.locator('#agents-search').fill('');

  await page.goto(rig.origin + viewOf('@2'));
  await expect(page).toHaveURL(/\/mirror\/view\?/);
  const log = page.locator('#mv-log');
  await expect(log.locator('[data-bubble="user"]').first()).toContainText('list the hermes fixtures', { timeout: 20_000 });
  await expect(log).toContainText('Listing them now.');
  await expect(log.locator('[data-tool-row]').first()).toContainText('ls fixtures/');

  // 🔴 Read-only: the composer is disabled and says why; there is no queue.
  await expect(page.locator('#mv-input')).toBeDisabled();
  await expect(page.locator('#mv-readonly')).toContainText('This host does not send to Hermes');
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

// HERMES PHASE 3: the same window on a host that runs with -hermes-send (and
// -interactive). The host's REAL side runs — the switch, the window
// re-resolved, the server's since — and the fake plays Hermes's gated send
// (cmd/muster-mirror/e2ehermes.go: idle at that since, then the user row).
//
// 🔴 REGRESSION TEST. On origin/main the composer stays disabled whatever the
// host runs, and the hub refuses every paste into a Hermes window.
test('on a host with -hermes-send, a Hermes window sends from its rendered view, and is refused while busy', async ({ page }) => {
  // A fresh Hermes home: the fake creates state.db once per home.
  home = fs.mkdtempSync(path.join(rig.dir, 'hermes-send-'));
  rows = path.join(home, 'rows.jsonl');
  row({ role: 'user', content: 'an earlier prompt' });
  row({ role: 'assistant', content: 'An earlier reply.' });
  await rig.startHost({ ...hostEnv, MUSTER_MIRROR_E2E_HERMES_HOME: home, MUSTER_MIRROR_INTERACTIVE: '1', MUSTER_MIRROR_HERMES_SEND: '1' });
  await rig.login(page);
  // The stopped host's listing outlives it for a while (offline retention),
  // so "listed" is not "the new host is connected": wait for the view itself.
  await expect
    .poll(async () => {
      await page.goto(rig.origin + viewOf('@2'));
      return page.locator('#mv-input').count();
    }, { message: `the Hermes view (mirror log:\n${rig.log})`, timeout: 30_000 })
    .toBe(1);
  const input = page.locator('#mv-input');
  await expect(input, `the composer (mirror log:\n${rig.log})`).toBeEnabled({ timeout: 20_000 });
  await expect(page.locator('#mv-readonly')).toHaveCount(0);
  await expect(input).toHaveAttribute('placeholder', 'Message Hermes…');

  const sends: number[] = [];
  page.on('response', (r) => {
    if (new URL(r.url()).pathname === '/mirror/view/send') sends.push(r.status());
  });
  await input.fill('hello Hermes, from muster');
  await page.locator('#mv-send').click();
  await expect(input).toHaveValue('');
  const log = page.locator('#mv-log');
  await expect(log.locator('[data-bubble="user"]').last()).toContainText('hello Hermes, from muster', { timeout: 20_000 });
  expect(sends).toEqual([204]);

  // Busy: refused, saying why; nothing reaches Hermes. The host reports the
  // new status on its next refresh, so each attempt waits for its own answer:
  //   204 — the hub still listed idle AND the host saw idle: rightly sent;
  //   409 "status changed" — the hub still listed idle, the HOST's own
  //     re-check at the moment of acting saw busy (the gated send's step 2);
  //   409 "Hermes is working" — the hub's idle gate, once it has the status.
  // Every refused attempt's text must never reach Hermes; the loop ends at
  // the hub's own refusal.
  const hermesWindows = path.join(rig.dir, 'hermes-windows');
  fs.writeFileSync(hermesWindows, `herm|@2|0|1|${t0 - 10}|${sid}|busy\nherm|@3|1|0|${t0 - 20}||\n`);
  const refused: string[] = [];
  let hubRefused = false;
  for (let i = 0; i < 30 && !hubRefused; i++) {
    const text = `while Hermes works ${i}`;
    await input.fill(text);
    const [resp] = await Promise.all([
      page.waitForResponse((r) => new URL(r.url()).pathname === '/mirror/view/send'),
      page.locator('#mv-send').click(),
    ]);
    if (resp.status() === 204) {
      await page.waitForTimeout(200);
      continue;
    }
    expect(resp.status()).toBe(409);
    const body = await resp.text();
    refused.push(text);
    if (body.includes('Hermes is working')) {
      hubRefused = true;
      await expect(page.locator('#mv-note')).toContainText('Hermes is working');
    } else {
      expect(body).toContain("Hermes's status changed after muster decided to send. Nothing was pasted.");
    }
  }
  expect(hubRefused, 'the hub never refused the busy window').toBe(true);
  const written = fs.readFileSync(path.join(home, 'rows.jsonl'), 'utf8');
  for (const text of refused) expect(written).not.toContain(text);
  fs.writeFileSync(hermesWindows, `herm|@2|0|1|${t0 - 10}|${sid}|idle\nherm|@3|1|0|${t0 - 20}||\n`);
});

// 🔴 A SEND THAT MAY HAVE LANDED IS NEVER REPEATED BY ITS NONCE. The fake
// answers the first try of an UNCONFIRMED-ONCE message "may have been
// submitted" (an Enter whose row never came). Pressing Send again with the
// same message (the same nonce) is refused and delivers nothing; the
// explicit "Send again" mints a new nonce and is delivered.
test('a send that may have been submitted offers Send again, and the same nonce is refused', async ({ page }) => {
  await rig.login(page);
  await expect
    .poll(async () => {
      await page.goto(rig.origin + viewOf('@2'));
      return page.locator('#mv-input').isEnabled();
    }, { message: `the Hermes view (mirror log:\n${rig.log})`, timeout: 30_000 })
    .toBe(true);
  const hermesWindows = path.join(rig.dir, 'hermes-windows');
  fs.writeFileSync(hermesWindows, `herm|@2|0|1|${t0 - 10}|${sid}|idle\nherm|@3|1|0|${t0 - 20}||\n`);
  const input = page.locator('#mv-input');
  const text = 'please, UNCONFIRMED-ONCE';
  // Until the host's refresh reports idle, the hub refuses as busy (nothing
  // delivered, the nonce released): wait for each answer, as above.
  let first = '';
  for (let i = 0; i < 30; i++) {
    await input.fill(text);
    const [resp] = await Promise.all([
      page.waitForResponse((r) => new URL(r.url()).pathname === '/mirror/view/send'),
      page.locator('#mv-send').click(),
    ]);
    first = await resp.text();
    if (!first.includes('Hermes is working')) break;
    await page.waitForTimeout(200);
  }
  expect(first).toContain('May have been submitted');
  await expect(page.locator('#mv-note')).toContainText('May have been submitted');
  const again = page.locator('#mv-send-again');
  await expect(again).toBeVisible();
  await expect(input).toHaveValue(text);

  // The same message, the same nonce: refused, nothing delivered.
  await page.locator('#mv-send').click();
  await expect(page.locator('#mv-note')).toContainText('Not sent again: the first try may already have been submitted');
  expect(fs.readFileSync(path.join(home, 'rows.jsonl'), 'utf8')).not.toContain(text);

  // The explicit retry: a new nonce, delivered once.
  await again.click();
  await expect(input).toHaveValue('');
  await expect(again).toBeHidden();
  await expect(page.locator('#mv-log [data-bubble="user"]').last()).toContainText(text, { timeout: 20_000 });
  const written = fs.readFileSync(path.join(home, 'rows.jsonl'), 'utf8');
  expect(written.split(text).length - 1).toBe(1);
});
