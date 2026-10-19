import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { BrowserContext, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { binDir, dsn, mirrorAskPort, password, repoRoot, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';

// ASKUSERQUESTION IN BOTH PLACES AT ONCE, in a real browser at phone width,
// against a real muster-server (MUSTER_MIRROR=1, its own port), a real
// muster-mirror over the tunnel (the `-tags e2emirror` per-window fake, its
// window -interactive and running Claude session `ask-<stamp>`), and the REAL
// hook: `muster-mirror ask`, fed the PreToolUse payload Claude Code 2.1.289
// sends (the shape captured in internal/askq/testdata), playing the CLI's side.
//
// The hook only REGISTERS the question and returns at once, printing nothing,
// so the terminal draws its own picker while the rendered view draws the card.
// The terminal's picker is played by the fake host: while T.picker.json exists
// the picker is on screen; an answer from muster is typed into askdrive's
// FakePicker through the same verified driver the shipped binary uses, and the
// CLI's record of the result (the call and its tool_result with toolUseResult)
// is appended to the transcript. An answer "in the terminal" is the spec
// appending that record itself. T.picker.touched is someone having started to
// answer in the terminal.

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

type Question = (typeof questions)[number];

// longQuestions: the shape of a live failure — the second question too long
// for a line, so the CLI (and the fake host's picker) draws it wrapped inside a
// box, "│ " down its left edge.
const longQuestions: Question[] = [
  questions[0],
  {
    question:
      'The shared widget package renders every tool call the same way regardless of its status, so the spinner on a ' +
      'half-finished call never stops (see the notes in #12/#13). Should we fix it upstream, or patch our copy for now?',
    header: 'Where',
    options: [
      { label: 'Upstream', description: 'Fix the package' },
      { label: 'Patch', description: 'Fix our copy' },
    ],
    multiSelect: false,
  },
  questions[2],
];

// payload is the PreToolUse hook payload Claude Code sends for the call.
const payload = (toolUseID: string, qs: Question[] = questions) =>
  JSON.stringify({
    session_id: claudeSession,
    transcript_path: tr,
    cwd: dir,
    permission_mode: 'default',
    hook_event_name: 'PreToolUse',
    tool_name: 'AskUserQuestion',
    tool_input: { questions: qs },
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
  done: Promise<{ code: number | null; out: string; err: string; ms: number }>;
}

// picker puts the call's picker on the terminal's screen (the CLI draws it as
// soon as the hook returns); touched says someone has started answering it;
// shown is what the picker shows (the questions asked, unless a spec says
// otherwise).
const pickerFile = () => tr + '.picker.json';
const touchedFile = () => tr + '.picker.touched';
function picker(toolUseId: string, touched = false, shown: Question[] = questions) {
  fs.writeFileSync(pickerFile(), JSON.stringify({ toolUseId, questions: shown }));
  if (touched) fs.writeFileSync(touchedFile(), '');
  else fs.rmSync(touchedFile(), { force: true });
}
// answerInTerminal is the CLI recording an answer given in the terminal.
function answerInTerminal(id: string, answers: Record<string, string>) {
  fs.rmSync(pickerFile(), { force: true });
  fs.appendFileSync(tr, askUse(id) + askResult(id, answers));
}

// registered runs the hook for a question shown in both places and checks it
// behaved as the CLI needs: quickly, with no output, exit 0.
async function registered(toolUseID: string, opts: { touched?: boolean; qs?: Question[]; shown?: Question[] } = {}) {
  picker(toolUseID, opts.touched, opts.shown ?? opts.qs ?? questions);
  const got = await hook(toolUseID, opts.qs).done;
  expect(got.code, got.err).toBe(0);
  expect(got.out, 'the hook printed something — the CLI would read it as the call\'s answer').toBe('');
  expect(got.ms, 'the hook held the call: the terminal waits on it').toBeLessThan(3000);
}

// hook runs `muster-mirror ask` as the CLI would: the payload on stdin and
// the mirror token.
function hook(toolUseID: string, qs: Question[] = questions): Hook {
  const p = spawn(mirrorBin, ['ask'], {
    env: { ...process.env, MUSTER_MIRROR_URL: origin, MUSTER_MIRROR_ALLOW_HTTP: '1', MUSTER_MIRROR_TOKEN: token },
    stdio: ['pipe', 'pipe', 'pipe'],
  });
  procs.push(p);
  let out = '';
  let err = '';
  p.stdout?.on('data', (d) => (out += d));
  p.stderr?.on('data', (d) => (err += d));
  const start = Date.now();
  p.stdin?.end(payload(toolUseID, qs));
  return { done: new Promise((r) => p.once('exit', (code) => r({ code, out, err, ms: Date.now() - start }))) };
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

  const card = (page: Page, id: string) => page.locator(`#mv-asks [data-ask-live][data-ask-tool="${id}"]`);

  // 🔴 SHOWN AT ONCE, ANSWERED HERE: the hook returns at once with no output;
  // the view's socket says the window's questions changed and the card is
  // drawn; a three-question answer — one single-select by keyboard, one
  // multi-select by touch, one in the person's own words — is typed into the
  // terminal's picker, and the CLI's record of it flips the card to answered.
  test('a question shows at once and an answer given here is typed into the terminal', async ({ page }) => {
    await openView(page);
    const told: number[] = [];
    page.on('websocket', (ws) =>
      ws.on('framereceived', (f) => {
        if (typeof f.payload === 'string' && f.payload.includes('"type":"asks"')) told.push(Date.now());
      }),
    );
    // 🔴 The page's timers are paused, so its fallback poll never runs: the
    // card can only be drawn because the socket said so.
    await page.clock.install();
    await page.reload();
    await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 20_000 });
    await page.clock.pauseAt(new Date(Date.now() + 1000));
    const toolID = 'toolu_e2e_' + stamp;
    await registered(toolID);
    const c = card(page, toolID);
    await expect(c, `the question is drawn (log:\n${mirrorLog})`).toHaveAttribute('data-ask-state', 'pending', { timeout: 10_000 });
    expect(told.length, 'the view\'s socket said the questions changed').toBeGreaterThan(0);
    await page.clock.resume();
    await expect(c.locator('fieldset')).toHaveCount(3);
    await expect(c.getByText('Cool and calm')).toBeVisible();
    await expect(c).toContainText('whichever is answered first counts');
    const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
    expect(sw, 'no horizontal scroll with the form drawn').toBeLessThanOrEqual(cw);

    const q1 = c.locator('fieldset').nth(0);
    await q1.getByRole('radio', { name: 'Red' }).focus();
    await page.keyboard.press('ArrowDown');
    await expect(q1.getByRole('radio', { name: 'Blue' })).toBeChecked();
    const q2 = c.locator('fieldset').nth(1);
    await q2.getByText('Fish', { exact: true }).tap();
    await q2.getByText('Cat', { exact: true }).tap();
    const q3 = c.locator('fieldset').nth(2);
    await q3.getByRole('textbox').fill('Gamma, the third');
    await expect(q3.getByRole('radio', { name: 'Other' })).toBeChecked();

    // 🔴 A poll that finds the question still pending leaves the form alone.
    await page.waitForResponse((r) => r.url().includes('/mirror/view/asks'));
    await expect(q1.getByRole('radio', { name: 'Blue' })).toBeChecked();
    await expect(q2.getByRole('checkbox', { name: 'Fish' })).toBeChecked();
    await expect(q3.getByRole('textbox')).toHaveValue('Gamma, the third');

    await c.getByRole('button', { name: 'Submit answers' }).tap();
    await expect(c).toHaveAttribute('data-ask-state', 'answered', { timeout: 10_000 });
    await expect(c).toContainText('Answered here');
    await expect(c.locator('form')).toHaveCount(0);
    // What the terminal's picker submitted — the CLI's record.
    const lines = fs.readFileSync(tr, 'utf8').trim().split('\n').map((l) => JSON.parse(l));
    const result = lines.find((l) => l.toolUseResult && l.message?.content?.[0]?.tool_use_id === toolID);
    expect(result?.toolUseResult?.answers).toEqual({
      'Which color do you prefer?': 'Blue',
      'Which pets do you have?': 'Cat, Fish',
      'What should the project be called?': 'Gamma, the third',
    });
    expect(fs.existsSync(pickerFile()), 'the picker closed').toBe(false);
    await expect(page.locator('#mv-log [data-ask][data-ask-state="history"]', { hasText: 'Gamma, the third' })).toBeVisible({ timeout: 20_000 });
  });

  // ANSWERED IN THE TERMINAL: the card, form and all, flips to "answered in
  // the terminal" with the answers chosen there, and nothing is typed.
  test('an answer given in the terminal flips the card', async ({ page }) => {
    await openView(page);
    const toolID = 'toolu_e2e_term_' + stamp;
    await registered(toolID);
    const c = card(page, toolID);
    await expect(c).toHaveAttribute('data-ask-state', 'pending', { timeout: 10_000 });
    await c.locator('fieldset').nth(0).getByText('Red', { exact: true }).tap();
    const before = fs.readFileSync(tr, 'utf8').length;
    answerInTerminal(toolID, { 'Which color do you prefer?': 'Green', 'Which pets do you have?': 'Dog', 'What should the project be called?': 'Alpha' });
    const added = fs.readFileSync(tr, 'utf8').length - before;
    await expect(c).toHaveAttribute('data-ask-state', 'answered-terminal', { timeout: 10_000 });
    await expect(c).toContainText('Answered in the terminal');
    await expect(c).toContainText('Green');
    await expect(c.locator('form')).toHaveCount(0);
    expect(fs.readFileSync(tr, 'utf8').length - before, 'muster typed into an answered picker').toBe(added);
  });

  // 🔴 THE RACE: someone has started answering in the terminal when the answer
  // from here arrives. Nothing is typed into that picker; the card says to
  // finish it there; the terminal's answer then resolves it.
  test('an answer that loses the race to the terminal types nothing', async ({ page }) => {
    await openView(page);
    const toolID = 'toolu_e2e_race_' + stamp;
    await registered(toolID, { touched: true });
    const c = card(page, toolID);
    await expect(c).toHaveAttribute('data-ask-state', 'pending', { timeout: 10_000 });
    await c.locator('fieldset').nth(0).getByText('Blue', { exact: true }).tap();
    await c.locator('fieldset').nth(1).getByText('Dog', { exact: true }).tap();
    await c.locator('fieldset').nth(2).getByText('Beta', { exact: true }).tap();
    const before = fs.readFileSync(tr, 'utf8');
    await c.getByRole('button', { name: 'Submit answers' }).tap();
    await expect(c).toHaveAttribute('data-ask-state', 'failed', { timeout: 10_000 });
    await expect(c).toContainText('Someone has started answering it in the terminal');
    expect(fs.readFileSync(tr, 'utf8'), 'something was submitted into a touched picker').toBe(before);
    expect(fs.existsSync(pickerFile()), 'the picker is still the terminal\'s').toBe(true);
    answerInTerminal(toolID, { 'Which color do you prefer?': 'Green', 'Which pets do you have?': 'Cat', 'What should the project be called?': 'Beta' });
    await expect(c).toHaveAttribute('data-ask-state', 'answered-terminal', { timeout: 10_000 });
    await expect(c).toContainText('Answered in the terminal');
  });

  // 🔴 A LONG QUESTION, AND A LONG ANSWER IN ONE'S OWN WORDS: the picker
  // draws the question boxed and wraps the typed answer over the text row's
  // lines (as Claude Code 2.1.289 does); both are read back as what muster
  // asked and typed, and the answer is submitted — not refused as someone
  // answering in the terminal, nor as the picker changing while typing.
  test('a long boxed question with a long answer of its own is typed into the terminal', async ({ page }) => {
    await openView(page);
    const toolID = 'toolu_e2e_long_' + stamp;
    await registered(toolID, { qs: longQuestions });
    const c = card(page, toolID);
    await expect(c, `the question is drawn (log:\n${mirrorLog})`).toHaveAttribute('data-ask-state', 'pending', { timeout: 10_000 });
    const own = 'The build agent needs to support running nightly batch-jobs, alongside the interactive sessions, where we are using models via a proxy.';
    // (Not Red: the historical-ask test finds ITS card by "Answer: Red".)
    await c.locator('fieldset').nth(0).getByText('Green', { exact: true }).tap();
    await c.locator('fieldset').nth(1).getByRole('textbox').fill(own);
    await c.locator('fieldset').nth(2).getByText('Beta', { exact: true }).tap();
    await c.getByRole('button', { name: 'Submit answers' }).tap();
    await expect(c, `typed (log:\n${mirrorLog})`).toHaveAttribute('data-ask-state', 'answered', { timeout: 10_000 });
    const lines = fs.readFileSync(tr, 'utf8').trim().split('\n').map((l) => JSON.parse(l));
    const result = lines.find((l) => l.toolUseResult && l.message?.content?.[0]?.tool_use_id === toolID);
    expect(result?.toolUseResult?.answers).toEqual({
      [longQuestions[0].question]: 'Green',
      [longQuestions[1].question]: own,
      [longQuestions[2].question]: 'Beta',
    });
  });

  // 🔴 A QUESTION MUSTER CANNOT READ IS NOT BLAMED ON THE TERMINAL: the
  // picker is untouched but shows the question in other words than the hook
  // registered. Nothing is typed, and the card says muster could not read it —
  // NOT that someone has started answering it in the terminal.
  test('a question the terminal shows in other words is refused as unreadable, not as touched', async ({ page }) => {
    await openView(page);
    const toolID = 'toolu_e2e_unread_' + stamp;
    const shown = [{ ...questions[0], question: 'Which colour do you prefer?' }, questions[1], questions[2]];
    await registered(toolID, { shown });
    const c = card(page, toolID);
    await expect(c).toHaveAttribute('data-ask-state', 'pending', { timeout: 10_000 });
    await c.locator('fieldset').nth(0).getByText('Blue', { exact: true }).tap();
    await c.locator('fieldset').nth(1).getByText('Dog', { exact: true }).tap();
    await c.locator('fieldset').nth(2).getByText('Beta', { exact: true }).tap();
    const before = fs.readFileSync(tr, 'utf8');
    await c.getByRole('button', { name: 'Submit answers' }).tap();
    await expect(c).toHaveAttribute('data-ask-state', 'failed', { timeout: 10_000 });
    await expect(c).toContainText("Couldn't read this question off the terminal's screen as the one muster was sent — answer it in the terminal.");
    await expect(c).not.toContainText('Someone has started answering it');
    expect(fs.readFileSync(tr, 'utf8'), 'something was submitted into a picker muster could not read').toBe(before);
    expect(fs.existsSync(pickerFile()), "the picker is still the terminal's").toBe(true);
    answerInTerminal(toolID, { 'Which color do you prefer?': 'Green', 'Which pets do you have?': 'Cat', 'What should the project be called?': 'Beta' });
    await expect(c).toHaveAttribute('data-ask-state', 'answered-terminal', { timeout: 10_000 });
  });

  // An ask answered earlier is drawn from the transcript, read-only.
  test('a historical ask renders read-only with its answers', async ({ page }) => {
    const id = 'toolu_e2e_hist_' + stamp;
    fs.appendFileSync(tr, askUse(id) + askResult(id, { 'Which color do you prefer?': 'Red', 'Which pets do you have?': 'Dog', 'What should the project be called?': 'Alpha' }));
    await openView(page);
    const hc = page.locator(`#mv-log [data-ask][data-ask-state="history"]`, { hasText: 'Answer: Red' });
    await expect(hc).toBeVisible({ timeout: 20_000 });
    await expect(hc).toContainText('Answer: Dog');
    await expect(hc).toContainText('Answer: Alpha');
    await expect(hc.locator('input, textarea, button, form')).toHaveCount(0);
  });
});
