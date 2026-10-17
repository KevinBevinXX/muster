import fs from 'node:fs';
import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { password, toolGroupsPort } from '../lib/env';
import { cc, MirrorRig, mkchat } from '../lib/mirrorhost';

// CONSECUTIVE TOOL CALLS COLLAPSE INTO ONE "N tool calls" ROW (internal/ui/
// tool_groups.go). One sequence — three calls, text, one call, text, two calls —
// is drawn three ways and must come out the same:
//
//   history — the agent chat's persisted transcript (server-rendered);
//   live    — the same turn streamed over the chat's socket (built in the page,
//             the count ticking up as calls arrive);
//   mirror  — a mirrored Claude Code pane whose transcript holds it.
//
// Opening a group shows its rows (each still opens to its own args/result),
// with a short animation that reduced motion turns off.

const rig = new MirrorRig(toolGroupsPort, 'e2e-groups');
const agent = 'e2e-groups-' + Date.now().toString(36);

test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

// The sequence, as the parts of one assistant turn.
type Part = { call: string } | { result: string; ok: boolean } | { text: string };
const SEQ: Part[] = [
  { call: 'alpha' }, { call: 'beta' }, { call: 'gamma' },
  { result: 'alpha', ok: true }, { result: 'beta', ok: true }, { result: 'gamma', ok: false },
  { text: 'Three ran; one failed.' },
  { call: 'delta' }, { result: 'delta', ok: true },
  { text: 'Then one more.' },
  { call: 'eps' }, { call: 'zeta' }, { result: 'eps', ok: true }, { result: 'zeta', ok: true },
];
// What every renderer must draw: G<n>[state](targets), R:<target>, T.
const WANT = 'G3[error](alpha,beta,gamma) T R:delta T G2[ok](eps,zeta)';

const cmd = (id: string) => `echo ${id}`;

test.beforeAll(async () => {
  test.setTimeout(120_000);
  // The persisted transcript: the user's message, then the turn's parts.
  const msgs: object[] = [{ role: 'user', content: 'run the checks' }];
  for (const p of SEQ) {
    if ('call' in p) msgs.push({ role: 'assistant', kind: 'tool_call', toolId: p.call, toolName: 'Bash', content: JSON.stringify({ command: cmd(p.call) }) });
    else if ('result' in p) msgs.push({ role: 'assistant', kind: 'tool_result', toolId: p.result, toolName: 'Bash', toolOk: p.ok, content: 'out ' + p.result });
    else msgs.push({ role: 'assistant', kind: 'text', content: p.text });
  }
  mkchat(agent, null);
  mkchat(agent, msgs);
  // The mirrored pane: the same turn as Claude Code records it — plus, after
  // it, a call, a task notice, and a call: a notice ENDS a run, like text.
  await rig.start();
  let body = cc.prompt('run the checks');
  for (const p of SEQ) {
    if ('call' in p) body += cc.toolUse(p.call, 'Bash', { command: cmd(p.call) });
    else if ('result' in p) body += cc.toolResult(p.result, 'out ' + p.result, !p.ok);
    else body += cc.reply(p.text);
  }
  body += cc.prompt('and the rest') + cc.toolUse('eta', 'Bash', { command: cmd('eta') }) + cc.taskNotice('completed', 'A background task finished') +
    cc.toolUse('theta', 'Bash', { command: cmd('theta') });
  const tr = rig.transcript(body);
  await rig.startHost({ MUSTER_MIRROR_E2E_SESSIONS: 'main', MUSTER_MIRROR_E2E_CLAUDE: 'main=' + tr });
});

test.afterAll(async () => {
  mkchat(agent, null);
  await rig.stop();
});

// shapeJS is the layout of a container's children, one token each — the
// browser twin of internal/ui/tool_groups_test.go's shape(): a group's count is
// read from its summary AND checked against the rows in its body.
const shapeJS = (root: Element) => {
  const target = (r: Element) => (r.querySelector('[data-tool-target]')?.textContent || '').trim().replace(/^echo /, '');
  const out: string[] = [];
  for (const c of Array.from(root.children)) {
    const el = c as HTMLElement;
    if (el.hidden || el.id === 'chat-working' || el.id === 'chat-provisioning' || el.classList.contains('hidden')) continue;
    if (el.hasAttribute('data-tool-row')) out.push('R:' + target(el));
    else if (el.hasAttribute('data-tool-group')) {
      const body = el.querySelector(':scope > [data-tool-group-body]')!;
      const rows = Array.from(body.children).filter((r) => r.hasAttribute('data-tool-row'));
      const count = el.querySelector(':scope > summary [data-tool-group-count]')!;
      const n = count.getAttribute('data-tool-group-count');
      if (n !== String(rows.length) || count.textContent!.trim() !== `${n} tool calls`) out.push(`BAD-COUNT(${n}/${rows.length})`);
      out.push(`G${rows.length}[${el.getAttribute('data-tool-status')}](${rows.map(target).join(',')})`);
    } else if (el.hasAttribute('data-notice-row')) out.push('N');
    else if (el.textContent!.trim()) out.push('T');
  }
  return out.join(' ');
};

async function signIn(page: Page, baseURL: string) {
  await page.goto(`${baseURL}/login`);
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
}

test('history: a run of calls is one row; text and a single call stay where they were; a group opens to its rows', async ({ page, baseURL }) => {
  await signIn(page, baseURL!);
  await page.goto(`${baseURL}/agents/${agent}`);
  const log = page.locator('#chat-log');
  await expect(log.locator('[data-tool-group]')).toHaveCount(2);
  // The user's bubble, then the turn.
  expect(await log.evaluate(shapeJS)).toBe('T ' + WANT);

  const g3 = log.locator('[data-tool-group]').first();
  await expect(g3.locator('summary [data-tool-group-count]')).toHaveText('3 tool calls');
  // A closed group hides its rows — and still says one of them failed.
  await expect(g3).not.toHaveAttribute('open', '');
  await expect(g3.locator('[data-tool-row]').first()).toBeHidden();
  await expect(g3.locator(':scope > summary [data-tool-group-status]')).toHaveText('✗');

  // Open: the rows appear, with the opening animation.
  await g3.locator(':scope > summary').tap();
  await expect(g3).toHaveAttribute('open', '');
  const anim = await g3.locator(':scope > [data-tool-group-body]').evaluate((b) => getComputedStyle(b).animationName);
  expect(anim, 'the body animates in').toBe('tool-group-open');
  const rows = g3.locator('[data-tool-row]');
  await expect(rows).toHaveCount(3);
  await expect(rows.first()).toBeVisible();
  // Each row still opens on its own, to its args and result.
  await expect(rows.nth(1)).not.toHaveAttribute('open', '');
  await rows.nth(1).locator('summary').tap();
  await expect(rows.nth(1)).toHaveAttribute('open', '');
  await expect(rows.nth(1).locator('pre').last()).toContainText('out beta');
  await expect(g3, 'opening a row leaves its group open').toHaveAttribute('open', '');
  // Close the group: its rows are hidden again.
  await g3.locator(':scope > summary').tap();
  await expect(g3).not.toHaveAttribute('open', '');
  await expect(rows.first()).toBeHidden();
  // Rows fit the phone inside the group.
  await g3.locator(':scope > summary').tap();
  for (let i = 0; i < 3; i++) {
    const b = (await rows.nth(i).boundingBox())!;
    expect(b.x + b.width).toBeLessThanOrEqual(390);
  }
});

// The group's HEIGHT animates, opening and closing (tool_groups.go). The
// animation is paused half-way and the box measured there: strictly between
// the closed and the open height — a snap would read one or the other.
test('a group grows and shrinks smoothly: half-way its height is between closed and open', async ({ page, baseURL }) => {
  await signIn(page, baseURL!);
  await page.goto(`${baseURL}/agents/${agent}`);
  const g3 = page.locator('#chat-log [data-tool-group]').first();
  const height = () => g3.evaluate((d) => d.getBoundingClientRect().height);
  // half pauses the group's own height animation at its midpoint and reads the
  // box there, then lets it finish.
  const half = () =>
    g3.evaluate((d) => {
      const a = d.getAnimations().find((x) => !(x as CSSAnimation).animationName);
      if (!a) return -1;
      a.pause();
      a.currentTime = Number((a.effect!.getTiming().duration as number) / 2);
      const h = d.getBoundingClientRect().height;
      a.finish();
      return h;
    });
  const closed = await height();
  await g3.locator(':scope > summary').tap();
  const midOpen = await half();
  await expect(g3).toHaveAttribute('open', '');
  const open = await height();
  expect(open, 'open is taller than closed').toBeGreaterThan(closed + 60);
  expect(midOpen, `opening, half-way (${closed} → ${open})`).toBeGreaterThan(closed + 5);
  expect(midOpen).toBeLessThan(open - 5);

  await g3.locator(':scope > summary').tap();
  const midClose = await half();
  await expect(g3).not.toHaveAttribute('open', '');
  expect(midClose, `closing, half-way (${open} → ${closed})`).toBeGreaterThan(closed + 5);
  expect(midClose).toBeLessThan(open - 5);
  expect(Math.abs((await height()) - closed), 'closed again at its closed height').toBeLessThanOrEqual(1);
});

// A second tap MID-WAY reverses the box from where it is: opening then tapped
// half-way closes (and ends closed), closing then tapped half-way opens (and
// ends open). The handler must know a box that is still [open] can be on its
// way CLOSED — `opening = !d.open` alone would read a half-closed box as open
// and close it again.
test('a tap mid-way reverses the group, from where it is, both ways', async ({ page, baseURL }) => {
  await signIn(page, baseURL!);
  await page.goto(`${baseURL}/agents/${agent}`);
  const g3 = page.locator('#chat-log [data-tool-group]').first();
  const height = () => g3.evaluate((d) => d.getBoundingClientRect().height);
  // pauseHalf holds the group's height animation at its midpoint.
  const pauseHalf = () =>
    g3.evaluate((d) => {
      const a = d.getAnimations().find((x) => !(x as CSSAnimation).animationName)!;
      a.pause();
      a.currentTime = Number((a.effect!.getTiming().duration as number) / 2);
      return d.getBoundingClientRect().height;
    });
  // finish ends the animations and waits for their finish handlers (which is
  // where a close removes [open]) before anything is read.
  const finish = () => g3.evaluate(async (d) => {
    const anims = d.getAnimations();
    anims.forEach((a) => a.finish());
    await Promise.all(anims.map((a) => a.finished.catch(() => undefined)));
    await new Promise((r) => setTimeout(r, 50));
  });
  const isOpen = () => g3.evaluate((d) => (d as HTMLDetailsElement).open);

  const closed = await height();
  await g3.locator(':scope > summary').tap(); // open…
  const mid = await pauseHalf();
  await g3.locator(':scope > summary').tap(); // …reversed half-way
  const from = await g3.evaluate((d) => d.getBoundingClientRect().height);
  expect(Math.abs(from - mid), 'the reversal starts from where the box was').toBeLessThanOrEqual(2);
  await finish();
  expect(await isOpen(), 'opening, reversed: it ends closed').toBe(false);
  await expect.poll(height).toBeLessThanOrEqual(closed + 1);

  await g3.locator(':scope > summary').tap(); // open fully
  await finish();
  await expect(g3).toHaveAttribute('open', '');
  const open = await height();
  await g3.locator(':scope > summary').tap(); // close…
  await pauseHalf();
  await g3.locator(':scope > summary').tap(); // …reversed half-way
  await finish();
  expect(await isOpen(), 'closing, reversed: it ends open').toBe(true);
  await expect.poll(height).toBeGreaterThanOrEqual(open - 1);
});

test('reduced motion: a group opens with no animation', async ({ page, baseURL }) => {
  await page.emulateMedia({ reducedMotion: 'reduce' });
  await signIn(page, baseURL!);
  await page.goto(`${baseURL}/agents/${agent}`);
  const g3 = page.locator('#chat-log [data-tool-group]').first();
  await g3.locator(':scope > summary').tap();
  await expect(g3).toHaveAttribute('open', '');
  expect(await g3.locator(':scope > [data-tool-group-body]').evaluate((b) => getComputedStyle(b).animationName)).toBe('none');
  expect(await g3.evaluate((d) => d.getAnimations().length), 'and its height is not animated').toBe(0);
});

test('live: the streamed turn groups the same way as the history, its count ticking up as calls arrive', async ({ page, baseURL }) => {
  let sock: { send: (m: string) => void } | undefined;
  await page.routeWebSocket(/\/agents\/[^/]+\/ws/, (ws) => {
    sock = ws;
  });
  await signIn(page, baseURL!);
  await page.goto(`${baseURL}/agents/${agent}`);
  const log = page.locator('#chat-log');
  await expect(log.locator('[data-tool-group]')).toHaveCount(2); // the history
  await expect.poll(() => !!sock, { message: 'the chat socket opened' }).toBe(true);
  await page.locator('#chat-input').fill('run the checks');
  await page.locator('#chat-input').press('Enter');

  // The streamed turn: agentChatScript's turn container, the last one.
  const live = () => log.locator('[data-chat-turn]').last().evaluate(shapeJS);
  const send = (m: object) => sock!.send(JSON.stringify(m));

  const steps: [Part, string][] = [
    [SEQ[0], 'R:alpha'],
    [SEQ[1], 'G2[running](alpha,beta)'],
    [SEQ[2], 'G3[running](alpha,beta,gamma)'],
  ];
  for (const [p, want] of steps) {
    const id = (p as { call: string }).call;
    send({ type: 'tool_call', id, name: 'Bash', target: cmd(id), args: JSON.stringify({ command: cmd(id) }) });
    await expect.poll(live, { message: `after ${id}` }).toBe(want);
  }
  for (const p of SEQ.slice(3)) {
    if ('call' in p) send({ type: 'tool_call', id: p.call, name: 'Bash', target: cmd(p.call), args: JSON.stringify({ command: cmd(p.call) }) });
    else if ('result' in p) send({ type: 'tool_result', id: p.result, ok: p.ok, output: 'out ' + p.result });
    else send({ type: 'delta', content: p.text });
  }
  await expect.poll(live, { message: 'the whole streamed turn' }).toBe(WANT);

  // A live group opens like a persisted one.
  const liveGroup = log.locator('[data-tool-group]').nth(2);
  await liveGroup.locator(':scope > summary').tap();
  await expect(liveGroup.locator('[data-tool-row]').first()).toBeVisible();

  // The turn ends; its layout stands. (The persisted transcript it would be
  // replaced by on the server's chat.reply is the history above — the same
  // WANT, asserted by the first test against the same rows.)
  send({ type: 'done' });
  await expect(page.locator('#chat-send')).toBeEnabled();
  expect(await live()).toBe(WANT);
});

test('mirror: the pane transcript groups the same way, and a notice ends a run', async ({ page }) => {
  await rig.login(page);
  await rig.waitListed(page, 'data-mirror-claude');
  await page.goto(rig.origin + rig.viewPath('main'));
  const log = page.locator('#mv-log');
  await expect(log.locator('[data-tool-group]')).toHaveCount(2, { timeout: 15_000 });
  // The prompt, the same turn, then the second prompt, a call, a notice, a call.
  await expect.poll(() => log.evaluate(shapeJS)).toBe('T ' + WANT + ' T R:eta N R:theta');
  const g3 = log.locator('[data-tool-group]').first();
  await g3.locator(':scope > summary').tap();
  await expect(g3.locator('[data-tool-row]').first()).toBeVisible();
});

// 🔴 A RESUMED SOCKET MUST NOT SCRAMBLE A RUN (ui.MirrorTranscript.Resumed).
// Calls a, b, c are drawn as one group; the view's socket drops and resumes
// (gen + offset, no reset); THEN their results arrive, then a call d. The
// results go into the rows where they are; nothing is moved, no group id is
// drawn twice, and the order stays a, b, c, d.
test('mirror: a socket that drops and resumes keeps the run in place, with no duplicate ids', async ({ page }) => {
  const tr = rig.transcript(cc.prompt('resume me') + cc.toolUse('ra', 'Bash', { command: cmd('ra') }) +
    cc.toolUse('rb', 'Bash', { command: cmd('rb') }) + cc.toolUse('rc', 'Bash', { command: cmd('rc') }), 'resume');
  await rig.startHost({ MUSTER_MIRROR_E2E_SESSIONS: 'main', MUSTER_MIRROR_E2E_CLAUDE: 'main=' + tr });
  const sockets: { close: () => Promise<void> }[] = [];
  await page.routeWebSocket(/\/mirror\/view\/ws/, (ws) => {
    ws.connectToServer();
    sockets.push(ws);
  });
  await rig.login(page);
  await rig.waitListed(page, 'data-mirror-claude');
  await page.goto(rig.origin + rig.viewPath('main'));
  const log = page.locator('#mv-log');
  await expect.poll(() => log.evaluate(shapeJS), { timeout: 15_000 }).toBe('T G3[running](ra,rb,rc)');

  // Drop the socket; the page reconnects on its own, resuming from its offset.
  await sockets[0].close();
  await expect.poll(() => sockets.length, { message: 'the page reconnected', timeout: 15_000 }).toBe(2);
  await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 15_000 });

  fs.appendFileSync(tr, cc.toolResult('ra', 'out ra') + cc.toolResult('rb', 'out rb') + cc.toolResult('rc', 'out rc', true) +
    cc.toolUse('rd', 'Bash', { command: cmd('rd') }));
  await expect.poll(() => log.evaluate(shapeJS), { message: 'the run stayed where it was; d after it' }).toBe('T G3[error](ra,rb,rc) R:rd');
  const dupes = await page.evaluate(() => {
    const seen: Record<string, number> = {};
    document.querySelectorAll('[id]').forEach((e) => (seen[e.id] = (seen[e.id] || 0) + 1));
    return Object.keys(seen).filter((k) => seen[k] > 1);
  });
  expect(dupes, 'no element id is drawn twice').toEqual([]);
  // The rows kept what only the first socket knew (their targets), and took the results.
  const rb = log.locator('[data-tool-row]').nth(1);
  await expect(rb.locator('[data-tool-target]')).toHaveText('echo rb');
  await expect(rb).toHaveAttribute('data-tool-status', 'ok');
  await expect(log.locator('[data-tool-group] > summary [data-tool-group-count]')).toHaveText('3 tool calls');
});
