import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { binDir, dsn, mirrorAskPort, password, repoRoot, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';

// ASKUSERQUESTION ANSWERED FROM THE RENDERED VIEW, in a real browser at phone
// width, against a real muster-server (MUSTER_MIRROR=1, its own port), a real
// muster-mirror over the tunnel (the `-tags e2emirror` per-window fake, its
// window -interactive and running Claude session `ask-<stamp>`), and the REAL
// hook: `muster-mirror ask`, fed the PreToolUse payload Claude Code 2.1.289
// sends (the shape captured in internal/askq/testdata), playing the CLI's side.
// Its stdout is what the CLI would read as the call's answer.
//
// What is measured about the CLI and so played here: while the hook holds the
// call, the transcript does NOT have it (the tool_use record is written once the
// hook returns) — so the live card is the only place the question is; and the
// call and its result (with toolUseResult) are written together afterwards.

const origin = `http://localhost:${mirrorAskPort}`;
const stamp = Date.now().toString(36);
const host = 'e2e-ask-' + stamp;
const claudeSession = 'ask-' + stamp;
const mirrorBin = path.join(binDir, 'muster-mirror-e2e');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'mirror-ask-'));
const tr = path.join(dir, claudeSession + '.jsonl');
let server: Running | undefined;
let token = '';
const procs: ChildProcess[] = [];
let mirrorLog = '';

const rec = (o: object) => JSON.stringify(o) + '\n';
const prompt = (t: string) => rec({ type: 'user', promptId: 'p', origin: { kind: 'human' }, message: { role: 'user', content: t } });

const questions = [
  {
    question: 'Which color do you prefer?',
    header: 'Color',
    options: [
      { label: 'Red', description: 'Warm and loud' },
      { label: 'Blue', description: 'Cool and calm' },
      { label: 'Green', description: 'Somewhere between' },
    ],
    multiSelect: false,
  },
  {
    question: 'Which pets do you have?',
    header: 'Pets',
    options: [
      { label: 'Cat', description: 'Independent' },
      { label: 'Dog', description: 'Needs walks' },
      { label: 'Fish', description: 'Quiet' },
    ],
    multiSelect: true,
  },
  {
    question: 'What should the project be called?',
    header: 'Name',
    options: [
      { label: 'Alpha', description: 'First' },
      { label: 'Beta', description: 'Second' },
    ],
    multiSelect: false,
  },
];

// payload is the PreToolUse hook payload Claude Code sends for the call.
const payload = (toolUseID: string) =>
  JSON.stringify({
    session_id: claudeSession,
    transcript_path: tr,
    cwd: dir,
    permission_mode: 'default',
    hook_event_name: 'PreToolUse',
    tool_name: 'AskUserQuestion',
    tool_input: { questions },
    tool_use_id: toolUseID,
  });

// The call and its result as the CLI writes them once answered.
const askUse = (id: string) => rec({ type: 'assistant', message: { content: [{ type: 'tool_use', id, name: 'AskUserQuestion', input: { questions } }] } });
const askResult = (id: string, answers: Record<string, string>) =>
  rec({
    type: 'user',
    promptId: 'p',
    message: { content: [{ type: 'tool_result', tool_use_id: id, content: 'Your questions have been answered.' }] },
    toolUseResult: { questions, answers },
  });

interface Hook {
  done: Promise<{ code: number | null; out: string; err: string }>;
}

// hook runs `muster-mirror ask` as the CLI would: the payload on stdin, the
// mirror token, and the hold bound.
function hook(toolUseID: string, timeout = '60s'): Hook {
  const p = spawn(mirrorBin, ['ask', '-timeout', timeout], {
    env: { ...process.env, MUSTER_MIRROR_URL: origin, MUSTER_MIRROR_ALLOW_HTTP: '1', MUSTER_MIRROR_TOKEN: token },
    stdio: ['pipe', 'pipe', 'pipe'],
  });
  procs.push(p);
  let out = '';
  let err = '';
  p.stdout?.on('data', (d) => (out += d));
  p.stderr?.on('data', (d) => (err += d));
  p.stdin?.end(payload(toolUseID));
  return { done: new Promise((r) => p.once('exit', (code) => r({ code, out, err }))) };
}

test.beforeAll(async () => {
  execFileSync('go', ['build', '-tags', 'e2emirror', '-o', mirrorBin, './cmd/muster-mirror'], { cwd: repoRoot, stdio: 'inherit' });
  server = await startServer(versionTerm, mirrorAskPort, {
    MUSTER_MIRROR: '1',
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
  });
  token = execFileSync(serverBinary(versionTerm), ['mirror', 'token', 'create', host], { env: { ...process.env, DATABASE_URL: dsn } })
    .toString()
    .trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
  fs.writeFileSync(tr, prompt('help me set the project up'));
  const windows = path.join(dir, 'windows');
  fs.writeFileSync(windows, `work|@1|0|1|claude|${Math.floor(Date.now() / 1000) - 60}|${tr}\n`);
  const p = spawn(mirrorBin, [], {
    env: {
      ...process.env,
      MUSTER_MIRROR_URL: origin,
      MUSTER_MIRROR_ALLOW_HTTP: '1',
      MUSTER_MIRROR_TOKEN: token,
      MUSTER_MIRROR_HOST: host,
      MUSTER_MIRROR_INTERACTIVE: '1',
      MUSTER_MIRROR_E2E_WINDOWS_FILE: windows,
      MUSTER_MIRROR_E2E_REFRESH: '250ms',
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  p.stdout?.on('data', (d) => (mirrorLog += d));
  p.stderr?.on('data', (d) => (mirrorLog += d));
  procs.push(p);
});

test.afterAll(async () => {
  for (const p of procs) {
    if (p.exitCode === null) {
      const done = new Promise((r) => p.once('exit', r));
      p.kill('SIGTERM');
      await done;
    }
  }
  await server?.stop();
  fs.rmSync(dir, { recursive: true, force: true });
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

async function openView(page: Page) {
  await login(page);
  await page.goto(origin + `/mirror/view?host=${encodeURIComponent(host)}&session=work&window=%401`);
  await expect(page.locator('#mv-status'), `the rendered view is live (log:\n${mirrorLog})`).toHaveText('Live', { timeout: 20_000 });
}

test.describe('phone 412×915 touch', () => {
  test.use({ viewport: { width: 412, height: 915 }, hasTouch: true, isMobile: true, storageState: { cookies: [], origins: [] } });

  // 🔴 THE WHOLE CYCLE: a held three-question ask — one single-select, one
  // multi-select, one answered in the person's own words — is drawn as a form,
  // answered, and its answers reach the waiting hook, which prints them as the
  // call's updatedInput; the card flips to answered; and once the CLI writes the
  // call and its result, the conversation draws it read-only in its place.
  test('a held multi-question ask is answered from the rendered view and the hook prints the answers', async ({ page }) => {
    await openView(page);
    const toolID = 'toolu_e2e_' + stamp;
    const h = hook(toolID);
    const card = page.locator(`#mv-asks [data-ask-live][data-ask-tool="${toolID}"]`);
    await expect(card, `the held question is drawn (log:\n${mirrorLog})`).toHaveAttribute('data-ask-state', 'pending', { timeout: 20_000 });
    await expect(card.locator('fieldset')).toHaveCount(3);
    await expect(card.getByText('Cool and calm')).toBeVisible();
    await expect(card.getByRole('radio')).toHaveCount(4 + 3); // Q1: 3 + Other, Q3: 2 + Other
    await expect(card.getByRole('checkbox')).toHaveCount(4); // Q2: 3 + Other
    const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
    expect(sw, 'no horizontal scroll with the form drawn').toBeLessThanOrEqual(cw);

    // Q1 by KEYBOARD: focus Red, arrow to Blue.
    const q1 = card.locator('fieldset').nth(0);
    await q1.getByRole('radio', { name: 'Red' }).focus();
    await page.keyboard.press('ArrowDown');
    await expect(q1.getByRole('radio', { name: 'Blue' })).toBeChecked();
    // Q2 by touch: two options.
    const q2 = card.locator('fieldset').nth(1);
    await q2.getByText('Fish', { exact: true }).tap();
    await q2.getByText('Cat', { exact: true }).tap();
    // Q3 in the person's own words: typing chooses Other.
    const q3 = card.locator('fieldset').nth(2);
    await q3.getByRole('textbox').fill('Gamma, the third');
    await expect(q3.getByRole('radio', { name: 'Other' })).toBeChecked();

    // 🔴 A poll that finds the question still pending must leave the form
    // alone: what was chosen and typed survives the next poll.
    await page.waitForResponse((r) => r.url().includes('/mirror/view/asks'));
    await page.waitForResponse((r) => r.url().includes('/mirror/view/asks'));
    await expect(q1.getByRole('radio', { name: 'Blue' })).toBeChecked();
    await expect(q2.getByRole('checkbox', { name: 'Fish' })).toBeChecked();
    await expect(q3.getByRole('textbox')).toHaveValue('Gamma, the third');

    await card.getByRole('button', { name: 'Submit answers' }).tap();
    const answered = page.locator(`#mv-asks [data-ask-live][data-ask-tool="${toolID}"]`);
    await expect(answered).toHaveAttribute('data-ask-state', 'answered');
    await expect(answered).toContainText('Answered here');

    const got = await h.done;
    expect(got.code, got.err).toBe(0);
    const out = JSON.parse(got.out);
    expect(out.hookSpecificOutput.hookEventName).toBe('PreToolUse');
    expect(out.hookSpecificOutput.permissionDecision).toBe('allow');
    expect(out.hookSpecificOutput.updatedInput.questions).toEqual(questions);
    expect(out.hookSpecificOutput.updatedInput.answers).toEqual({
      'Which color do you prefer?': 'Blue',
      'Which pets do you have?': 'Cat, Fish',
      'What should the project be called?': 'Gamma, the third',
    });

    // The CLI writes the call and its result: the conversation draws it, and
    // the live card makes way.
    fs.appendFileSync(tr, askUse(toolID) + askResult(toolID, out.hookSpecificOutput.updatedInput.answers));
    const drawn = page.locator('#mv-log [data-ask][data-ask-state="history"]');
    await expect(drawn).toContainText('Gamma, the third', { timeout: 20_000 });
    await expect(page.locator('#mv-asks [data-ask-live]')).toHaveCount(0);
    await expect(page.locator('#mv-asks')).toBeHidden();
  });

  // Nobody answers before the hold ends: the hook prints NOTHING (the CLI then
  // shows its own picker) and the card says the terminal has it.
  test('an ask nobody answers goes back to the terminal', async ({ page }) => {
    await openView(page);
    const toolID = 'toolu_e2e_late_' + stamp;
    const h = hook(toolID, '3s');
    const card = page.locator(`#mv-asks [data-ask-live][data-ask-tool="${toolID}"]`);
    await expect(card).toHaveAttribute('data-ask-state', 'pending', { timeout: 20_000 });
    const got = await h.done;
    expect(got.code).toBe(0);
    expect(got.out, 'the hook printed an answer nobody gave').toBe('');
    await expect(card).toHaveAttribute('data-ask-state', 'withdrawn', { timeout: 10_000 });
    await expect(card).toContainText('went back to the terminal');
    await expect(card.locator('form')).toHaveCount(0);
  });

  // An ask answered earlier is drawn from the transcript, read-only.
  test('a historical ask renders read-only with its answers', async ({ page }) => {
    const id = 'toolu_e2e_hist_' + stamp;
    fs.appendFileSync(tr, askUse(id) + askResult(id, { 'Which color do you prefer?': 'Red', 'Which pets do you have?': 'Dog', 'What should the project be called?': 'Alpha' }));
    await openView(page);
    const card = page.locator(`#mv-log [data-ask][data-ask-state="history"]`, { hasText: 'Answer: Red' });
    await expect(card).toBeVisible({ timeout: 20_000 });
    await expect(card).toContainText('Answer: Dog');
    await expect(card).toContainText('Answer: Alpha');
    await expect(card.locator('input, textarea, button, form')).toHaveCount(0);
  });
});
