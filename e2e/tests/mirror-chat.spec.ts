import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { binDir, dsn, mirrorChatPort, password, repoRoot, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';

// THE MIRRORED RENDERED VIEW'S CHAT, on a phone with touch (412×915), against a
// REAL muster-server and a REAL muster-mirror built with `-tags e2emirror`
// (cmd/muster-mirror/e2emirror.go): session main is a Claude Code pane whose
// transcript is a real JSONL file this spec writes record by record, read by
// the real tailer and parser. Every record shape is the CLI's own; the content
// is invented.
test.use({ viewport: { width: 412, height: 915 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

const origin = `http://localhost:${mirrorChatPort}`;
const host = 'e2e-chat-' + Date.now().toString(36);
const mirrorBin = path.join(binDir, 'muster-mirror-e2e');
let server: Running | undefined;
let mirrorProc: ChildProcess | undefined;
// mirrorLog is the CURRENT muster-mirror process's output only (startMirror
// resets it), so a line in it is about this test's host connection.
let mirrorLog = '';
let token = '';

function startMirror(transcript: string): ChildProcess {
  const env: NodeJS.ProcessEnv = {
    ...process.env,
    MUSTER_MIRROR_URL: origin,
    MUSTER_MIRROR_ALLOW_HTTP: '1',
    MUSTER_MIRROR_TOKEN: token,
    MUSTER_MIRROR_HOST: host,
    MUSTER_MIRROR_E2E_SESSIONS: 'main',
    MUSTER_MIRROR_E2E_CLAUDE: 'main=' + transcript,
  };
  mirrorLog = '';
  const p = spawn(mirrorBin, [], { env, stdio: ['ignore', 'pipe', 'pipe'] });
  p.stdout?.on('data', (d) => (mirrorLog += d));
  p.stderr?.on('data', (d) => (mirrorLog += d));
  return p;
}

async function stopMirror() {
  if (!mirrorProc || mirrorProc.exitCode !== null) return;
  const done = new Promise((r) => mirrorProc!.once('exit', r));
  mirrorProc.kill('SIGTERM');
  await done;
}

test.beforeAll(async () => {
  execFileSync('go', ['build', '-tags', 'e2emirror', '-o', mirrorBin, './cmd/muster-mirror'], { cwd: repoRoot, stdio: 'inherit' });
  server = await startServer(versionTerm, mirrorChatPort, {
    MUSTER_MIRROR: '1',
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
  });
  token = execFileSync(serverBinary(versionTerm), ['mirror', 'token', 'create', host], { env: { ...process.env, DATABASE_URL: dsn } })
    .toString()
    .trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
});

test.afterEach(async () => {
  await stopMirror();
});

test.afterAll(async () => {
  await server?.stop();
});

let signedIn: Awaited<ReturnType<BrowserContext['cookies']>> | undefined;
async function login(page: Page) {
  if (signedIn) {
    await page.context().addCookies(signedIn);
    return;
  }
  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
  signedIn = await page.context().cookies(origin);
}

const rec = (o: object) => JSON.stringify(o) + '\n';
const prompt = (t: string) => rec({ type: 'user', promptId: 'p', origin: { kind: 'human' }, message: { role: 'user', content: t } });
const reply = (t: string) => rec({ type: 'assistant', message: { content: [{ type: 'text', text: t }] } });
const taskNotice = (status: string, summary: string, result: string) =>
  rec({
    type: 'user',
    promptId: 'p',
    origin: { kind: 'task-notification' },
    message: {
      role: 'user',
      content: `<task-notification>\n<task-id>t1</task-id>\n<status>${status}</status>\n<summary>${summary}</summary>\n<result>${result}</result>\n</task-notification>`,
    },
  });
const command = (name: string) =>
  rec({ type: 'user', promptId: 'p', message: { role: 'user', content: `<command-name>${name}</command-name>\n<command-message>${name.slice(1)}</command-message>\n<command-args></command-args>` } });

function newTranscript(body: string): string {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'muster-e2e-chat-'));
  const p = path.join(dir, 'e2e-chat-0001.jsonl');
  fs.writeFileSync(p, body);
  return p;
}

const viewPath = `/mirror/view?host=${encodeURIComponent(host)}&session=main`;

// 🔴 openView WAITS FOR THIS TEST'S OWN HOST CONNECTION, NOT FOR A CLAUDE CARD.
// Every test kills its muster-mirror (afterEach), and muster then keeps that
// host's last list as OFFLINE — Claude chip included. So from the second test
// on, "the agents list shows data-mirror-claude" was already true before the
// new muster-mirror had connected, and when its connect was slower than the
// page load (a loaded host) /mirror/view found the session offline and
// REDIRECTED to the terminal page, where #mv-status never exists: the 15 s
// "Live" wait timed out, 1-3 tests a run.
//
// The signal is the new process's own "connected to" line: muster registers
// the connection BEFORE it sends the welcome that line follows, so once it is
// logged the view resolves to this connection. The card must also be online.
async function openView(page: Page, transcript: string) {
  mirrorProc = startMirror(transcript);
  await login(page);
  await expect
    .poll(
      async () => {
        if (!mirrorLog.includes('muster-mirror: connected to ')) return 'muster-mirror not connected yet';
        const html = await (await page.request.get(origin + '/ui/agents')).text();
        if (!html.includes('data-mirror-claude')) return 'main not reported as a Claude pane';
        if (html.includes('data-mirror-offline')) return 'the host is listed offline';
        return 'ready';
      },
      { message: `this test's muster-mirror connected, main online as a Claude pane (muster-mirror log:\n${mirrorLog})`, timeout: 20_000 },
    )
    .toBe('ready');
  await page.goto(origin + viewPath);
  // The rendered view, not a redirect to the terminal (an offline session).
  expect(new URL(page.url()).pathname, 'the rendered view opened, not the terminal').toBe('/mirror/view');
  await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 15_000 });
}

// B1 round 1: a record the CLI stamps as the HUMAN's is their bubble even when
// it is wholly a pasted notice block, and a command record carrying any block
// beyond the command's own is shown raw — never rewritten to "/name" with the
// extra block's text dropped.
test('a human-origin pasted notice is a user bubble; a command record with an extra block is shown raw', async ({ page }) => {
  const pasted = '<task-notification>\n<task-id>t9</task-id>\n<status>completed</status>\n<summary>Pasted summary</summary>\n</task-notification>';
  const extra = '<command-name>/model</command-name>\n<note>secret extra instruction</note>';
  const tr = newTranscript(prompt(pasted) + reply('ok') + prompt('<local-command-stdout>Set model to Example</local-command-stdout>') + prompt(extra));
  await openView(page, tr);
  const log = page.locator('#mv-log');
  const users = log.locator('[data-bubble="user"]');
  await expect(users).toHaveCount(3);
  await expect(log.locator('[data-notice-row]')).toHaveCount(0);
  await expect(users.nth(0)).toContainText('Pasted summary');
  await expect(users.nth(1)).toContainText('Set model to Example');
  await expect(users.nth(2)).toContainText('secret extra instruction');
  await expect(users.nth(2)).toContainText('<command-name>/model</command-name>');
});

// B1: a harness notice is its own compact row — status and summary, detail on
// expand — and never the human's bubble; a human who MENTIONS the tag, or types
// a slash command, stays one.
test('a task notification is a compact collapsible row, not a user bubble; a prompt that mentions the tag, or a slash command, is still a bubble', async ({ page }) => {
  const tr = newTranscript(
    prompt('audit the widget in the background') +
      reply('Started an agent on it.') +
      taskNotice('completed', 'Agent "Audit the widget" finished', 'The widget is fine. Two nits.') +
      prompt('why did the <task-notification> say completed?'),
  );
  await openView(page, tr);
  const log = page.locator('#mv-log');

  const row = log.locator('[data-notice-row="task"]');
  await expect(row).toHaveCount(1);
  await expect(row).toHaveAttribute('data-notice-status', 'completed');
  await expect(row.locator('[data-notice-text]')).toHaveText('Agent "Audit the widget" finished');
  await expect(row.locator('summary [aria-label="status: completed"]')).toHaveText('✓');
  // Collapsed: the result is hidden until the row is tapped.
  await expect(row).not.toHaveAttribute('open', '');
  await expect(row.locator('pre')).toBeHidden();
  await row.locator('summary').tap();
  await expect(row).toHaveAttribute('open', '');
  await expect(row.locator('pre')).toContainText('The widget is fine.');

  // Exactly the human's two prompts are user bubbles — the notice is not one,
  // and the prompt that merely MENTIONS the tag is.
  const users = log.locator('[data-bubble="user"]');
  await expect(users).toHaveCount(2);
  await expect(users.nth(0)).toContainText('audit the widget in the background');
  await expect(users.nth(1)).toContainText('why did the <task-notification> say completed?');
  await expect(users.filter({ hasText: 'Audit the widget" finished' })).toHaveCount(0);
  // The row fits the phone, like a tool row.
  const box = (await row.boundingBox())!;
  expect(box.x + box.width, 'the notice row fits 412px').toBeLessThanOrEqual(412);

  // Live: a slash command and a failed task arrive while the view is open. The
  // command is the HUMAN's (they typed it): their bubble, reading what they
  // typed, never a notice row and never the raw tags.
  fs.appendFileSync(tr, command('/model') + taskNotice('failed', 'Agent "Second" failed', 'boom'));
  const failed = log.locator('[data-notice-row="task"][data-notice-status="failed"]');
  await expect(failed.locator('summary [aria-label="status: failed"]')).toHaveText('✗');
  await expect(users).toHaveCount(3);
  await expect(users.nth(2)).toHaveText('/model');
  await expect(log.locator('[data-notice-row]')).toHaveCount(2);
});

const lines = (n: number, what: string) => Array.from({ length: n }, (_, i) => `${what} line ${i + 1}`).join('\n\n');

// The log's distance from its bottom, in px.
const fromBottom = (page: Page, sel = '#mv-log') =>
  page.locator(sel).evaluate((l) => l.scrollHeight - l.scrollTop - l.clientHeight);

// B2: a bubble over ~12 lines is clamped with a fade and a control; a tap opens
// it to its full height (animated), another closes it; a short one is untouched;
// opening the newest at the bottom keeps the log at the bottom.
test('a long bubble is clamped, a tap expands it to full height and back, a short one is not clamped, and the log stays at the bottom', async ({ page }) => {
  const tr = newTranscript(prompt('a short question') + reply(lines(40, 'answer')) + prompt(lines(30, 'my long message')));
  await openView(page, tr);
  const log = page.locator('#mv-log');
  const short = log.locator('[data-bubble="user"]').first();
  const answer = log.locator('[data-bubble="assistant"]').first();
  const newest = log.locator('[data-bubble="user"]').last();

  // Short: not clamped, no control.
  await expect(short).toContainText('a short question');
  await expect(short).not.toHaveAttribute('data-clamp', /.*/);
  expect(await short.evaluate((e) => e.nextElementSibling?.hasAttribute('data-clamp-toggle') ?? false), 'no control under a short bubble').toBe(false);

  // Long: clamped to about twelve lines — content hidden below, a control under it.
  for (const b of [answer, newest]) {
    await expect(b).toHaveAttribute('data-clamp', 'closed');
    const [h, sh, lh] = await b.evaluate((e) => [e.clientHeight, e.scrollHeight, parseFloat(getComputedStyle(e).lineHeight)]);
    expect(sh, 'there is more content than is shown').toBeGreaterThan(h + lh);
    expect(h, 'about 12 lines tall').toBeGreaterThanOrEqual(lh * 11);
    expect(h, 'about 12 lines tall').toBeLessThanOrEqual(lh * 14);
  }
  const toggle = newest.locator('xpath=following-sibling::*[1]');
  await expect(toggle).toHaveAttribute('data-clamp-toggle', '');
  await expect(toggle).toHaveText('Show more');
  await expect(toggle).toHaveAttribute('aria-expanded', 'false');

  // The view opened at the bottom; tapping the newest (a real touch tap) opens
  // it to its scrollHeight and the log is STILL at the bottom.
  expect(await newest.evaluate((e) => getComputedStyle(e).transitionDuration), 'the expand is animated').not.toBe('0s');
  await expect.poll(() => fromBottom(page), { message: 'the view opened at the bottom' }).toBeLessThan(2);
  const before = await newest.evaluate((e) => e.clientHeight);
  await newest.tap();
  await expect(newest).toHaveAttribute('data-clamp', 'open');
  const [after, full] = await newest.evaluate((e) => [e.clientHeight, e.scrollHeight]);
  expect(after, 'it grew').toBeGreaterThan(before);
  expect(Math.abs(after - full), 'measured height equals its scrollHeight').toBeLessThanOrEqual(1);
  expect(await fromBottom(page), 'still at the bottom after the expand').toBeLessThan(2);
  await expect(toggle).toHaveText('Show less');
  await expect(toggle).toHaveAttribute('aria-expanded', 'true');

  // Tap again (the control this time) collapses it back to the clamp.
  await toggle.tap();
  await expect(newest).toHaveAttribute('data-clamp', 'closed');
  await expect.poll(() => newest.evaluate((e) => e.clientHeight)).toBe(before);
  await expect(toggle).toHaveText('Show more');

  // The control is a 44px thumb target and nothing scrolls sideways.
  const tb = (await toggle.boundingBox())!;
  expect(tb.height).toBeGreaterThanOrEqual(44);
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw).toBeLessThanOrEqual(cw);

  // Live: a long reply streamed in later is clamped as it lands.
  fs.appendFileSync(tr, reply(lines(25, 'later')));
  await expect(log.locator('[data-bubble="assistant"]').last()).toHaveAttribute('data-clamp', 'closed');
});

// prefers-reduced-motion: the expand is instant (no transition), and still
// reaches the full height.
test('with reduced motion the clamp opens with no transition', async ({ page }) => {
  await page.emulateMedia({ reducedMotion: 'reduce' });
  const tr = newTranscript(prompt(lines(30, 'calm')));
  await openView(page, tr);
  const b = page.locator('#mv-log [data-bubble="user"]').first();
  await expect(b).toHaveAttribute('data-clamp', 'closed');
  expect(await b.evaluate((e) => getComputedStyle(e).transitionDuration)).toBe('0s');
  await b.tap();
  await expect(b).toHaveAttribute('data-clamp', 'open');
  const [h, sh] = await b.evaluate((e) => [e.clientHeight, e.scrollHeight]);
  expect(Math.abs(h - sh)).toBeLessThanOrEqual(1);
});

// B2 round 1: a tap on a control INSIDE a bubble is the control's. A fenced
// code block in a mirrored reply is a collapsed <details> (internal/ui/
// markdown.go); tapping its summary opens the code block and leaves the
// clamped bubble alone.
test('tapping a code block summary inside a clamped bubble toggles only the code block', async ({ page }) => {
  const tr = newTranscript(prompt('show me') + reply('```\ncode line one\ncode line two\n```\n\n' + lines(30, 'after the code')));
  await openView(page, tr);
  const b = page.locator('#mv-log [data-bubble="assistant"]').first();
  await expect(b).toHaveAttribute('data-clamp', 'closed');
  const code = b.locator('details').first();
  await expect(code).not.toHaveAttribute('open', '');
  await code.locator('summary').tap();
  await expect(code).toHaveAttribute('open', '');
  await expect(code.locator('pre')).toContainText('code line two');
  // Past the expand's own 600ms finish timer: the bubble never moved.
  await page.waitForTimeout(700);
  await expect(b).toHaveAttribute('data-clamp', 'closed');
  // A tap in the open code block's body is the code block's too.
  await code.locator('pre').tap();
  await page.waitForTimeout(700);
  await expect(b).toHaveAttribute('data-clamp', 'closed');
  await code.locator('summary').tap();
  await expect(code).not.toHaveAttribute('open', '');
  await expect(b).toHaveAttribute('data-clamp', 'closed');
  // The bubble itself still toggles.
  await b.locator('p').last().tap();
  await expect(b).toHaveAttribute('data-clamp', 'open');
});

// The agent chat specs below drive an agent of their OWN (e2e/mkchat) with a
// persisted chat, so their bubbles are the server's real chatBubble markup
// (data-md, data-msg-id) without adding rows to a seeded agent other specs
// count. It is deleted after.
//
// The delete is armed BEFORE mkchat runs, and the delete is idempotent: a run
// that fails or is killed after its commit still has its agent removed (mkchat
// writes the agent and its messages in one transaction, so a refused message
// leaves nothing to remove).
const chatAgent = 'e2e-clamp-' + Date.now().toString(36);
const longAgentMsg = Array.from({ length: 30 }, (_, i) => `agent line ${i + 1}`).join('\n');
let chatAgentMade = false;
let chatAgentCleanup = false;
function mkchat(extra: NodeJS.ProcessEnv, input = '') {
  execFileSync('go', ['run', './e2e/mkchat'], { cwd: repoRoot, input, env: { ...process.env, DATABASE_URL: dsn, MUSTER_E2E_AGENT: chatAgent, ...extra } });
}
function makeChatAgent() {
  if (chatAgentMade) return;
  chatAgentCleanup = true;
  mkchat({}, JSON.stringify([
    { role: 'user', content: 'Check the docs build.' },
    { role: 'assistant', content: 'The build passes.' },
    { role: 'user', content: longAgentMsg },
  ]));
  chatAgentMade = true;
}
test.afterAll(() => {
  if (chatAgentCleanup) mkchat({ MUSTER_E2E_DELETE: '1' });
});

// refreshChatLog fires task:changed on <body>, as the completion banner's
// Mark-complete does, and resolves once #chat-log has been swapped.
async function refreshChatLog(page: Page) {
  await page.evaluate(
    () =>
      new Promise<void>((resolve) => {
        const on = (e: Event) => {
          if ((e as CustomEvent).detail?.target?.id !== 'chat-log') return;
          document.body.removeEventListener('htmx:afterSwap', on);
          // After the swap's own mutation callbacks (the clamp's measure) ran.
          requestAnimationFrame(() => resolve());
        };
        document.body.addEventListener('htmx:afterSwap', on);
        document.body.dispatchEvent(new Event('task:changed'));
      }),
  );
}

// B2 round 1: the agent chat page swaps #chat-log's whole content on
// sse:chat.reply / task:changed / agent.changed (chatLogTrigger), so every
// bubble is a new element after it. A persisted message the reader opened is
// drawn open again at once, by its row id, without animating.
test('an opened persisted message stays open across a chat-log refresh', async ({ page }) => {
  makeChatAgent();
  await login(page);
  await page.goto(origin + '/agents/' + chatAgent);
  const log = page.locator('#chat-log');
  const long = log.locator('[data-bubble="user"]').last();
  await expect(long).toHaveAttribute('data-msg-id', /^[1-9][0-9]*$/);
  const id = await long.getAttribute('data-msg-id');
  await expect(long).toHaveAttribute('data-clamp', 'closed');
  await long.tap();
  await expect(long).toHaveAttribute('data-clamp', 'open');

  // Mark the drawn element, record every clamp state the redrawn one passes
  // through, then refresh the way the completion banner does.
  await log.evaluate((l, id) => {
    (l.querySelector(`[data-msg-id="${id}"]`) as any).__old = true;
    (window as any).__states = [];
    new MutationObserver((ms) => ms.forEach((m) => {
      const t = m.target as Element;
      if (m.type === 'attributes' && t.hasAttribute('data-msg-id')) (window as any).__states.push(t.getAttribute('data-clamp'));
    })).observe(l, { subtree: true, attributes: true, attributeFilter: ['data-clamp'] });
  }, id);
  await refreshChatLog(page);
  const fresh = log.locator(`[data-msg-id="${id}"]`);
  await expect(fresh).toHaveCount(1);
  expect(await fresh.evaluate((e) => !(e as any).__old), 'the bubble was redrawn').toBe(true);
  await expect(fresh).toHaveAttribute('data-clamp', 'open');
  const [h, sh] = await fresh.evaluate((e) => [e.clientHeight, e.scrollHeight]);
  expect(Math.abs(h - sh), 'drawn at its full height').toBeLessThanOrEqual(1);
  expect(await page.evaluate(() => (window as any).__states), 'no animation: never "moving"').not.toContain('moving');
  await expect(fresh.locator('xpath=following-sibling::*[1]')).toHaveText('Show less');

  // Collapsing it is remembered too.
  await fresh.tap();
  await expect(fresh).toHaveAttribute('data-clamp', 'closed');
  await refreshChatLog(page);
  await expect(log.locator(`[data-msg-id="${id}"]`)).toHaveAttribute('data-clamp', 'closed');
});

// The SAME clamp on the dispatched-agent chat (bubbleClampScript is on that
// page too), on real server-rendered bubbles: the short persisted ones are
// drawn as they are — no clamp — and the long one is clamped and opened by a
// tap.
test('the agent chat clamps a long bubble and leaves its short seeded ones alone', async ({ page }) => {
  makeChatAgent();
  await login(page);
  await page.goto(origin + '/agents/' + chatAgent);
  const log = page.locator('#chat-log');
  const seeded = log.locator('[data-bubble="user"]').first();
  await expect(seeded).toContainText('Check the docs build.');
  await expect(seeded).toHaveAttribute('data-md-done', '');
  await expect(seeded).not.toHaveAttribute('data-clamp', /.*/);
  const long = log.locator('[data-bubble="user"]').last();
  await expect(long).toHaveAttribute('data-md', '');
  await expect(long).toContainText('agent line 30');
  await expect(long).toHaveAttribute('data-clamp', 'closed');
  await expect.poll(() => fromBottom(page, '#chat-log'), { message: 'the agent log opened at the bottom' }).toBeLessThan(2);
  await long.tap();
  await expect(long).toHaveAttribute('data-clamp', 'open');
  const [h, sh] = await long.evaluate((e) => [e.clientHeight, e.scrollHeight]);
  expect(Math.abs(h - sh)).toBeLessThanOrEqual(1);
  expect(await fromBottom(page, '#chat-log'), 'the agent log stays at the bottom').toBeLessThan(2);
});

// A prompt record with the CLI's timestamp field.
const promptAt = (t: string, at: string) =>
  rec({ type: 'user', promptId: 'p', origin: { kind: 'human' }, timestamp: at, message: { role: 'user', content: t } });

// Is el's box inside its log's visible box?
const inLogView = (page: Page, el: ReturnType<Page['locator']>, logSel: string) =>
  el.evaluate((e, sel) => {
    const l = document.querySelector(sel)!.getBoundingClientRect();
    const r = e.getBoundingClientRect();
    return r.top >= l.top - 1 && r.top < l.bottom && r.bottom > l.top;
  }, logSel);

// B3: a header button opens every message the reader sent (first line, time);
// a tap on one scrolls to it, opens it if clamped and highlights it — and the
// list keeps up with messages that stream in.
test('the message index lists every sent message and a tap jumps to it, as messages stream in', async ({ page }) => {
  let body = '';
  for (let i = 1; i <= 20; i++) {
    const text = i === 3 ? `message number ${i}\n\n` + lines(30, 'long three') : `message number ${i}`;
    body += promptAt(text, `2026-01-02T10:${String(i).padStart(2, '0')}:00.000Z`) + reply(lines(6, `reply to ${i}`));
  }
  const tr = newTranscript(body);
  await openView(page, tr);
  const log = page.locator('#mv-log');
  await expect(log.locator('[data-bubble="user"]')).toHaveCount(20);

  const openBtn = page.locator('#chat-index-open');
  const sheet = page.locator('#chat-index');
  const items = sheet.locator('[data-chat-index-item]');
  await expect(sheet).toBeHidden();
  await openBtn.tap();
  await expect(sheet).toBeVisible();
  await expect(openBtn).toHaveAttribute('aria-expanded', 'true');
  await expect(items).toHaveCount(20);
  // First line and a time; every item a 44px target.
  await expect(items.nth(2)).toContainText('message number 3');
  await expect(items.nth(2)).not.toContainText('long three');
  expect((await items.nth(2).locator('span').last().textContent())!.trim(), 'a time is shown').toMatch(/\d/);
  expect((await items.nth(2).boundingBox())!.height).toBeGreaterThanOrEqual(44);

  // Jump to N = 3: far above the bottom the view opened at, and clamped.
  const third = log.locator('[data-bubble="user"]').nth(2);
  await expect(third).toHaveAttribute('data-clamp', 'closed');
  expect(await inLogView(page, third, '#mv-log'), 'message 3 starts out of view').toBe(false);
  await items.nth(2).tap();
  await expect(sheet).toBeHidden();
  // Highlighted briefly (asserted first: it is gone 1.6s later).
  await expect(third).toHaveAttribute('data-flash', '');
  await expect.poll(() => inLogView(page, third, '#mv-log'), { message: 'message 3 is in the viewport' }).toBe(true);
  await expect(third).toHaveAttribute('data-clamp', 'open');
  await expect(third).not.toHaveAttribute('data-flash', /.*/, { timeout: 5_000 });
  // The bubble's id is the one the item named.
  const id = await third.getAttribute('id');
  expect(id).toBeTruthy();

  // Messages stream in while the sheet is open: the list keeps up.
  await openBtn.tap();
  await expect(items).toHaveCount(20);
  fs.appendFileSync(tr, promptAt('a fresh message', '2026-01-02T11:00:00.000Z') + reply('ok'));
  await expect(items).toHaveCount(21);
  await expect(items.last()).toContainText('a fresh message');
  // …and the earlier bubble kept its id.
  await expect(third).toHaveAttribute('id', id!);
  // Jump to N = 1 (the top), then to the newest.
  await items.first().tap();
  const first = log.locator('[data-bubble="user"]').first();
  await expect.poll(() => inLogView(page, first, '#mv-log'), { message: 'message 1 is in the viewport' }).toBe(true);
  await openBtn.tap();
  await items.last().tap();
  await expect.poll(() => inLogView(page, log.locator('[data-bubble="user"]').last(), '#mv-log')).toBe(true);
  // The backdrop closes it, and nothing scrolls sideways.
  await openBtn.tap();
  await page.locator('#chat-index > [data-chat-index-close]').tap({ position: { x: 20, y: 20 } });
  await expect(sheet).toBeHidden();
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw).toBeLessThanOrEqual(cw);
});

// The same index on the dispatched-agent chat, over its REAL persisted
// transcript: the seeded message carries its row id.
test('the agent chat has the message index, keyed by the persisted message id', async ({ page }) => {
  await login(page);
  await page.goto(origin + '/agents/docs-sweeper');
  await page.locator('#chat-index-open').tap();
  const items = page.locator('#chat-index [data-chat-index-item]');
  await expect(items).toHaveCount(1);
  await expect(items.first()).toContainText('Check the docs build.');
  expect(await items.first().getAttribute('data-chat-index-item')).toMatch(/^chat-msg-\d+$/);
  await items.first().tap();
  const b = page.locator('#chat-log [data-bubble="user"]').first();
  await expect(b).toHaveAttribute('data-flash', '');
  await expect.poll(() => inLogView(page, b, '#chat-log')).toBe(true);
});
