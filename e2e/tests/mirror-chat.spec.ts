import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { dsn, mirrorChatPort, password, repoRoot } from '../lib/env';
import { MirrorRig } from '../lib/mirrorhost';

// THE MIRRORED RENDERED VIEW'S CHAT, on a phone with touch (412×915), against a
// REAL muster-server and a REAL muster-mirror built with `-tags e2emirror`
// (cmd/muster-mirror/e2emirror.go): session main is a Claude Code pane whose
// transcript is a real JSONL file this spec writes record by record, read by
// the real tailer and parser. Every record shape is the CLI's own; the content
// is invented.
test.use({ viewport: { width: 412, height: 915 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

// The server, its mirror token and the fake host are e2e/lib/mirrorhost.ts's.
const rig = new MirrorRig(mirrorChatPort, 'e2e-chat');
const origin = rig.origin;
const host = rig.host;

// startMirror (re)starts the host with session main as a Claude Code pane
// writing transcript. rig.log is the CURRENT process's output only (a restart
// clears it), so a wait on "connected" cannot be satisfied by an earlier one.
async function startMirror(transcript: string, extra: NodeJS.ProcessEnv = {}): Promise<void> {
  await rig.startHost({ ...extra, MUSTER_MIRROR_E2E_SESSIONS: 'main', MUSTER_MIRROR_E2E_CLAUDE: 'main=' + transcript });
}

const stopMirror = () => rig.stopHost();

test.beforeAll(async () => {
  await rig.start();
});

test.afterEach(async () => {
  await stopMirror();
});

test.afterAll(async () => {
  await rig.stop();
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
async function openView(page: Page, transcript: string, extra: NodeJS.ProcessEnv = {}) {
  await startMirror(transcript, extra);
  await login(page);
  await expect
    .poll(
      async () => {
        if (!rig.log.includes('muster-mirror: connected to ')) return 'muster-mirror not connected yet';
        const html = await (await page.request.get(origin + '/ui/agents')).text();
        if (!html.includes('data-mirror-claude')) return 'main not reported as a Claude pane';
        if (html.includes('data-mirror-offline')) return 'the host is listed offline';
        return 'ready';
      },
      { message: `this test's muster-mirror connected, main online as a Claude pane (muster-mirror log:\n${rig.log})`, timeout: 20_000 },
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

// Is el's box inside its log's visible box?
const inLogView = (page: Page, el: ReturnType<Page['locator']>, logSel: string) =>
  el.evaluate((e, sel) => {
    const l = document.querySelector(sel)!.getBoundingClientRect();
    const r = e.getBoundingClientRect();
    return r.top >= l.top - 1 && r.top < l.bottom && r.bottom > l.top;
  }, logSel);

// A transcript of n prompt/reply pairs: 2n events, so with n > 150 the view's
// backfill (the newest 300 events) loads only the newest 150 prompts.
const pairs = (n: number, text: (i: number) => string) => {
  let body = '';
  for (let i = 1; i <= n; i++) body += prompt(text(i)) + reply(`reply to ${i}`);
  return body;
};

// An unloaded prompt with a first line far wider than the phone.
const wide = 'an early prompt whose first line is much wider than a phone ' + 'and keeps going '.repeat(8);

// B3: the header button opens EVERY message the reader sent in the whole
// transcript — not only the ones the backfill loaded. A loaded one is a tap
// target that scrolls to its bubble, opens it if clamped and highlights it; an
// earlier one shows its text in full, marked "earlier — not loaded"; a slash
// command is listed; and the list keeps up with messages that stream in.
test('the message index lists every sent message in the transcript: loaded ones jump, earlier ones show their text', async ({ page }) => {
  const body = pairs(199, (i) =>
    i === 2 ? wide : i === 51 ? `message number 51\n\n` + lines(30, 'long fifty-one') : `message number ${i}`,
  ) + command('/model');
  const tr = newTranscript(body);
  await openView(page, tr);
  const log = page.locator('#mv-log');
  // 399 events; the backfill (the newest 300) loads the slash command and
  // prompts 51–199, as bubbles.
  await expect(log.locator('[data-bubble="user"]')).toHaveCount(150);

  const openBtn = page.locator('#chat-index-open');
  const sheet = page.locator('#chat-index');
  const items = sheet.locator('[data-chat-index-item]');
  const unloaded = sheet.locator('[data-chat-index-unloaded]');
  await expect(sheet).toBeHidden();
  await openBtn.tap();
  await expect(sheet).toBeVisible();
  await expect(openBtn).toHaveAttribute('aria-expanded', 'true');
  // ALL of them: 50 earlier ones the page never loaded, then the 150 it did.
  await expect(sheet.locator('#chat-index-list > li')).toHaveCount(200);
  await expect(unloaded).toHaveCount(50);
  await expect(items).toHaveCount(150);
  await expect(sheet.locator('#chat-index-partial')).toBeHidden();
  await expect(sheet.locator('#chat-index-more')).toBeHidden();
  // An earlier one: marked, its text whole — wrapped, not cut to one line.
  const early = unloaded.nth(1);
  await expect(early.locator('[data-chat-index-mark]')).toHaveText('earlier — not loaded');
  await expect(early).toContainText(wide.trim());
  const [ew, esw, eh] = await early.evaluate((e) => [e.clientWidth, e.scrollWidth, e.getBoundingClientRect().height]);
  expect(esw, 'the earlier prompt wraps inside the sheet').toBeLessThanOrEqual(ew);
  expect(eh, 'it wraps onto several lines').toBeGreaterThan(60);
  expect(await early.locator('button').count(), 'an unloaded item is not a jump target').toBe(0);
  await expect(unloaded.first()).toContainText('message number 1');
  // The slash command is the newest, as the human typed it.
  await expect(items.last()).toHaveText(/\/model$/);
  // A loaded one: first line only, numbered in the whole transcript, 44px.
  await expect(items.first().locator('span').first()).toHaveText('51');
  await expect(items.first()).toContainText('message number 51');
  await expect(items.first()).not.toContainText('long fifty-one');
  expect((await items.first().boundingBox())!.height).toBeGreaterThanOrEqual(44);

  // Jump to message 51: far above the bottom the view opened at, and clamped.
  const target = log.locator('[data-bubble="user"]').first();
  await expect(target).toContainText('message number 51');
  await expect(target).toHaveAttribute('data-clamp', 'closed');
  expect(await inLogView(page, target, '#mv-log'), 'message 51 starts out of view').toBe(false);
  await items.first().tap();
  await expect(sheet).toBeHidden();
  // Highlighted briefly (asserted first: it is gone 1.6s later).
  await expect(target).toHaveAttribute('data-flash', '');
  await expect.poll(() => inLogView(page, target, '#mv-log'), { message: 'message 51 is in the viewport' }).toBe(true);
  await expect(target).toHaveAttribute('data-clamp', 'open');
  await expect(target).not.toHaveAttribute('data-flash', /.*/, { timeout: 5_000 });
  // The bubble's id is the one the item named: its place in the transcript.
  const id = await target.getAttribute('id');
  expect(id).toMatch(/^mv-p-\d+$/);

  // Messages stream in while the sheet is open: the list keeps up.
  await openBtn.tap();
  await expect(sheet.locator('#chat-index-list > li')).toHaveCount(200);
  fs.appendFileSync(tr, prompt('a fresh message') + reply('ok'));
  await expect(sheet.locator('#chat-index-list > li')).toHaveCount(201);
  await expect(items.last()).toContainText('a fresh message');
  await expect(unloaded).toHaveCount(50);
  // …and the earlier bubble kept its id.
  await expect(target).toHaveAttribute('id', id!);
  // Jump to the newest.
  await items.last().tap();
  await expect.poll(() => inLogView(page, log.locator('[data-bubble="user"]').last(), '#mv-log')).toBe(true);
  // The backdrop closes it, and nothing scrolls sideways.
  await openBtn.tap();
  await page.locator('#chat-index > [data-chat-index-close]').tap({ position: { x: 20, y: 20 } });
  await expect(sheet).toBeHidden();
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw).toBeLessThanOrEqual(cw);
});

// The host bounds its index (mirror.MaxIndexPrompts, the newest kept): what it
// left out is SAID, with a count, never silently dropped — and the numbering
// still counts from the transcript's first prompt.
test('a transcript with more prompts than the index holds says how many earlier ones are not listed', async ({ page }) => {
  await openView(page, newTranscript(pairs(2030, (i) => `message number ${i}`)));
  await page.locator('#chat-index-open').tap();
  const sheet = page.locator('#chat-index');
  await expect(sheet.locator('#chat-index-more')).toBeVisible();
  await expect(sheet.locator('#chat-index-more')).toHaveText('30 more earlier, not listed');
  await expect(sheet.locator('#chat-index-list > li')).toHaveCount(2000);
  const first = sheet.locator('#chat-index-list > li').first();
  await expect(first.locator('span').first()).toHaveText('31');
  await expect(first).toContainText('message number 31');
  await expect(sheet.locator('[data-chat-index-item]')).toHaveCount(150);
});

// AN OLDER HOST sends no index: the list is what the page loaded, under a line
// saying so — never presented as the whole conversation.
test('against a host that predates the index, the list is the loaded messages and says so', async ({ page }) => {
  await openView(page, newTranscript(pairs(200, (i) => `message number ${i}`)), { MUSTER_MIRROR_E2E_NO_PROMPT_INDEX: '1' });
  await expect(page.locator('#mv-log [data-bubble="user"]')).toHaveCount(150);
  await page.locator('#chat-index-open').tap();
  const sheet = page.locator('#chat-index');
  await expect(sheet.locator('#chat-index-partial')).toBeVisible();
  await expect(sheet.locator('#chat-index-partial')).toContainText('Only loaded messages shown');
  await expect(sheet.locator('[data-chat-index-item]')).toHaveCount(150);
  await expect(sheet.locator('[data-chat-index-unloaded]')).toHaveCount(0);
  await expect(sheet.locator('[data-chat-index-item]').first()).toContainText('message number 51');
  // Its bubbles carry no transcript place, so the index named them itself.
  expect(await sheet.locator('[data-chat-index-item]').first().getAttribute('data-chat-index-item')).toMatch(/^chat-umsg-\d+$/);
});

// An assistant record with the CLI's usage block (context = input + cache
// read + cache creation: 3 + 497 + cacheRead).
const replyUsage = (t: string, model: string, cacheRead: number) =>
  rec({
    type: 'assistant',
    message: { model, content: [{ type: 'text', text: t }], usage: { input_tokens: 3, cache_creation_input_tokens: 497, cache_read_input_tokens: cacheRead, output_tokens: 900 } },
  });
// The session's model attachment: it names a 1M window, and the readout still
// shows a count and never a share of a window.
const modelAttachment = (id: string) => rec({ type: 'attachment', attachment: { type: 'model', identity: { modelId: id } } });

// B4: the header shows the context the latest turn used as a token count —
// read by the real host and carried over the real tunnel — follows it live,
// and shows nothing at all until a turn has reported usage.
test('the header shows the context token count from the host, follows it live, and is hidden when absent', async ({ page }) => {
  const tr = newTranscript(prompt('hello') + reply('a reply with no usage block'));
  await openView(page, tr);
  const ctx = page.locator('#mv-context');
  await expect(page.locator('#mv-log [data-bubble="assistant"]')).toHaveCount(1);
  await expect(ctx).toBeHidden();

  fs.appendFileSync(tr, modelAttachment('claude-opus-5-5[1m]') + prompt('more') + replyUsage('first', 'claude-opus-5-5', 249_500));
  await expect(ctx).toBeVisible();
  await expect(ctx).toHaveText('250k context');
  // Live: the next turn's usage replaces it.
  fs.appendFileSync(tr, prompt('again') + replyUsage('second', 'claude-opus-5-5', 611_500));
  await expect(ctx).toHaveText('612k context');
  // It fits the phone's header with nothing scrolling sideways.
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw).toBeLessThanOrEqual(cw);
});
