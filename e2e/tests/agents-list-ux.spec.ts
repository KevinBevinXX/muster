import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { Browser, BrowserContext, Locator, Page } from '@playwright/test';
import { test, expect, takeNew } from '../lib/fixtures';
import { binDir, dsn, hookToken, listUxPort, password, repoRoot, versionTerm } from '../lib/env';
import { serverBinary, startServer, Running } from '../lib/server';
import { mkchat } from '../lib/mirrorhost';
import { writeFileAtomic } from '../lib/atomic';
import { agentLink } from '../lib/agent-link';

// THE AGENTS LIST UX BATCH, in a real browser at a phone (390×844, touch) and
// a desktop (1280×800), against a real muster-server (its own port, the
// e2eterm binary, mirror plugin on) and a real muster-mirror over the tunnel
// (the `-tags e2emirror` fake plays tmux only):
//
//   1. ONE FLAT LIST, most recently active first: a mirrored window active a
//      minute ago is drawn ABOVE a dispatched agent active five minutes ago,
//      and the windows and agents interleave by activity from there down.
//   2. A RECENCY DOT on the operator's tmux idle-fade scale, one colour per
//      bucket (bright green, aqua, orange, grey).
//   3. A LONG TITLE is clamped; its "…" is a button that opens the whole
//      title — a tap (or Enter) never opens the agent.
//   4. PRESET CHIPS: Waiting on you (an agent's attention_ask, a Claude window
//      stopped at a prompt, a held AskUserQuestion registered by the real
//      `muster-mirror ask` hook — and NOT an agent with only a reply draft),
//      Idle >15m / >1h / >1d; combinable, kept across a list refresh and a
//      reload (localStorage), and a device that remembered the retired
//      "Waiting on ask" / "Waiting on question" chips gets "Waiting on you".
//      🔴 Red before: there was no "waiting" chip — "ask" and "question" were
//      two chips, each matching only part of the union.
//   5. MARK COMPLETE on an agent's page goes back OUT to the Agents list, with
//      JavaScript and without it.
//
// 🔴 REGRESSION TESTS: on the tree before, every agent was drawn above every
// window, no card had a dot, no title was clamped, there were no chips, and
// Mark complete opened /tasks (and did nothing at all without JavaScript).

const origin = `http://localhost:${listUxPort}`;
const stamp = Date.now().toString(36);
const host = 'e2e-lux-' + stamp;
const mirrorBin = path.join(binDir, 'muster-mirror-e2e');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'lux-'));
let server: Running | undefined;
let token = '';
const procs: ChildProcess[] = [];
let mirrorLog = '';

// The two agents on the list: one fresh with a LONG name, one three days idle.
const freshAgent = 'lux-fresh-' + stamp;
const oldAgent = 'lux-old-' + stamp;
// An agent waiting on an attention_ask, and one whose only attention item is a
// reply draft (which waits on nothing).
const askAgent = 'lux-ask-' + stamp;
const draftAgent = 'lux-draft-' + stamp;
const draftPlugin = 'lux-draft-' + stamp;
const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/pluginfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();
const LONG = 'Untangle the reconnect backoff so a flapping tunnel stops resetting its retry window every time';
// The old agent's focus title, past the same clamp.
const FOCUS = 'Sweeping the stale notes into the archive bucket and re-linking every task that still points at them';
// The agents whose task each test completes: one per viewport and path.
const doneAgent = (vp: string, js: boolean) => `lux-done-${vp}-${js ? 'js' : 'nojs'}-${stamp}`;
const doneTask = (vp: string, js: boolean) => `Lux task ${vp} ${js ? 'js' : 'nojs'} ${stamp}`;
const VPS = ['phone', 'desktop'];

// The windows: Claude windows luxnow (1 min, waiting at a prompt) and luxq (45
// min, holding an AskUserQuestion), and shells luxorange (3 h) and luxancient
// (2 days). session|id|index|active|command|activity|transcript
const now = Math.floor(Date.now() / 1000);
const sidNow = 'luxnow-' + stamp;
const sidQ = 'luxq-' + stamp;
const trNow = path.join(dir, sidNow + '.jsonl');
const trQ = path.join(dir, sidQ + '.jsonl');
const prompt = (t: string) => JSON.stringify({ type: 'user', promptId: 'p', origin: { kind: 'human' }, message: { role: 'user', content: t } }) + '\n';

// The expected relative order of this spec's cards, newest first.
const ORDER = ['luxnow', askAgent, draftAgent, freshAgent, 'luxq', 'luxorange', 'luxancient', oldAgent];
// What the "Waiting on you" chip shows: the window at a prompt, the asking
// agent and the window holding a question — not the draft's agent.
const WAITING = ['luxnow', askAgent, 'luxq'];

const question = {
  question: 'Which region should the replica go to?',
  header: 'Region',
  options: [
    { label: 'North', description: 'Closer to the users' },
    { label: 'South', description: 'Cheaper' },
  ],
  multiSelect: false,
};

test.beforeAll(async () => {
  test.setTimeout(240_000);
  execFileSync('go', ['build', '-tags', 'e2emirror', '-o', mirrorBin, './cmd/muster-mirror'], { cwd: repoRoot, stdio: 'inherit' });
  server = await startServer(versionTerm, listUxPort, {
    MUSTER_MIRROR: '1',
    MUSTER_FOCUS_INTERVAL: '0',
    // Reply drafts are an attention item only with the plugin interface on.
    MUSTER_PLUGINS: '1',
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
  });
  token = execFileSync(serverBinary(versionTerm), ['mirror', 'token', 'create', host], { env: { ...process.env, DATABASE_URL: dsn } })
    .toString()
    .trim();
  expect(token.startsWith('mmt_'), 'a mirror token was minted').toBe(true);
  expect(LONG.length, 'the fixture title is past the 60-character clamp').toBeGreaterThan(60);

  mkchat(freshAgent, [{ role: 'user', content: 'Fix the tunnel backoff.' }], { MUSTER_E2E_DISPLAY_NAME: LONG, MUSTER_E2E_ACTIVE_MINUTES_AGO: '5' });
  mkchat(oldAgent, [{ role: 'user', content: 'Tidy the notes.' }], {
    MUSTER_E2E_ACTIVE_MINUTES_AGO: String(3 * 24 * 60),
    MUSTER_E2E_FOCUS: JSON.stringify([{ subject: 'self', title: FOCUS, description: 'Two buckets left.' }]),
  });
  mkchat(askAgent, [{ role: 'user', content: 'Roll the fleet?' }], { MUSTER_E2E_ACTIVE_MINUTES_AGO: '2' });
  mkchat(draftAgent, [{ role: 'user', content: 'Answer the mail.' }], { MUSTER_E2E_ACTIVE_MINUTES_AGO: '3' });
  fix('ask', '1', askAgent, 'Roll the fleet now?');
  fix('draft', '1', draftAgent, draftPlugin);
  for (const vp of VPS) {
    for (const js of [true, false]) {
      mkchat(doneAgent(vp, js), [{ role: 'user', content: 'Ship the fix.' }], {
        MUSTER_E2E_TASK: doneTask(vp, js),
        MUSTER_E2E_ACTIVE_MINUTES_AGO: String(5 * 24 * 60),
      });
    }
  }

  fs.writeFileSync(trNow, prompt('roll the fleet'));
  writeFileAtomic(trNow + '.status', JSON.stringify({ status: 'waiting', statusUpdatedAt: Date.now() - 30_000 }));
  fs.writeFileSync(trQ, prompt('set the replica up'));
  writeFileAtomic(trQ + '.status', JSON.stringify({ status: 'idle', statusUpdatedAt: Date.now() - 60_000 }));
  const windows = path.join(dir, 'windows');
  writeFileAtomic(
    windows,
    [
      `luxnow|@1|1|1|claude|${now - 60}|${trNow}`,
      `luxq|@2|1|1|claude|${now - 45 * 60}|${trQ}`,
      `luxorange|@3|1|1|zsh|${now - 3 * 3600}|`,
      `luxancient|@4|1|1|zsh|${now - 2 * 86400}|`,
    ].join('\n') + '\n',
  );
  const p = spawn(mirrorBin, [], {
    env: {
      ...process.env,
      MUSTER_MIRROR_URL: origin,
      MUSTER_MIRROR_ALLOW_HTTP: '1',
      MUSTER_MIRROR_TOKEN: token,
      MUSTER_MIRROR_HOST: host,
      MUSTER_MIRROR_INTERACTIVE: '1',
      MUSTER_MIRROR_E2E_WINDOWS_FILE: windows,
      MUSTER_MIRROR_E2E_REFRESH: '300ms',
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  p.stdout?.on('data', (d) => (mirrorLog += d));
  p.stderr?.on('data', (d) => (mirrorLog += d));
  procs.push(p);
});

// askQuestion registers luxq's AskUserQuestion through the REAL hook, as
// Claude Code would run it (the payload on stdin, the host's token).
async function askQuestion() {
  writeFileAtomic(trQ + '.picker.json', JSON.stringify({ toolUseId: 'toolu_lux_' + stamp, questions: [question] }));
  const h = spawn(mirrorBin, ['ask'], {
    env: { ...process.env, MUSTER_MIRROR_URL: origin, MUSTER_MIRROR_ALLOW_HTTP: '1', MUSTER_MIRROR_TOKEN: token },
    stdio: ['pipe', 'pipe', 'pipe'],
  });
  procs.push(h);
  let err = '';
  h.stderr?.on('data', (d) => (err += d));
  h.stdin?.end(
    JSON.stringify({
      session_id: sidQ,
      transcript_path: trQ,
      cwd: dir,
      permission_mode: 'default',
      hook_event_name: 'PreToolUse',
      tool_name: 'AskUserQuestion',
      tool_input: { questions: [question] },
      tool_use_id: 'toolu_lux_' + stamp,
    }),
  );
  const code = await new Promise((r) => h.once('exit', r));
  expect(code, `the ask hook ran (${err}; host log:\n${mirrorLog})`).toBe(0);
}
let asked = false;

test.afterAll(async () => {
  for (const p of procs) {
    if (p.exitCode === null && p.signalCode === null) {
      const done = new Promise((r) => p.once('exit', r));
      p.kill('SIGTERM');
      await done;
    }
  }
  await server?.stop();
  mkchat(freshAgent, null);
  mkchat(oldAgent, null);
  fix('rmplugin', '1', draftPlugin);
  mkchat(askAgent, null);
  mkchat(draftAgent, null);
  for (const vp of VPS) for (const js of [true, false]) mkchat(doneAgent(vp, js), null, { MUSTER_E2E_TASK: doneTask(vp, js) });
  fs.rmSync(dir, { recursive: true, force: true });
});

test.use({ storageState: { cookies: [], origins: [] } });

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

// card is one of this spec's cards: a window by its tmux session, an agent by
// the link to its page.
function card(page: Page, key: string): Locator {
  if (key.startsWith('lux-')) return page.locator(`#agents-list > article[id^="agent-"]:has(${agentLink(key)})`);
  return page.locator(`#agents-list > article[data-mirror-card][data-f-session="${key}"]`);
}

// openList signs in, opens the Agents list with no chip chosen, and waits
// until every card of this spec is drawn (applying anything held back).
async function openList(page: Page, opts: { keepChips?: boolean } = {}) {
  await login(page);
  if (!asked) {
    await askQuestion();
    asked = true;
  }
  await page.goto(origin + '/agents');
  if (!opts.keepChips) {
    await page.evaluate(() => localStorage.removeItem('muster:agents:chips'));
    await page.goto(origin + '/agents');
  }
  await expect
    .poll(
      async () => {
        await takeNew(page);
        let n = 0;
        for (const k of ORDER) n += await card(page, k).count();
        return n;
      },
      { timeout: 30_000, message: `every card of the spec is listed (log:\n${mirrorLog})` },
    )
    .toBe(ORDER.length);
}

// order is this spec's cards in the order the list draws them.
async function order(page: Page): Promise<string[]> {
  // Each card named the way card() finds it.
  const keys: string[] = await page.locator('#agents-list > article').evaluateAll((els) =>
    els.map((a) =>
      a.hasAttribute('data-mirror-card')
        ? a.getAttribute('data-f-session') || ''
        : (a.querySelector('a[href^="/agents/"]')?.getAttribute('href') || '').split('?')[0].slice('/agents/'.length),
    ),
  );
  return keys.filter((k) => ORDER.includes(k));
}

const dotOf = (c: Locator) => c.locator('[data-card-active] [data-recency]');
const chip = (page: Page, id: string) => page.locator(`#agents-chips [data-chip="${id}"]`);

// shown is which of this spec's cards the list shows now.
async function shown(page: Page): Promise<string[]> {
  const out: string[] = [];
  for (const k of ORDER) if (await card(page, k).isVisible()) out.push(k);
  return out;
}

for (const vp of [
  { name: 'phone', title: 'phone 390×844 touch', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop', title: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.title, () => {
    test.use(vp.use);
    const press = (l: Locator) => (vp.use.hasTouch ? l.tap() : l.click());

    test('one flat list, most recently active first: a window above an agent, interleaved by activity', async ({ page }) => {
      await openList(page);
      expect(await order(page)).toEqual(ORDER);
      // The host is still on each card: the window's host icon and the host
      // chip in its body, the agent's cluster mark.
      await expect(card(page, 'luxnow').locator('[data-mirror-host]')).toHaveText(host);
      await expect(card(page, freshAgent).locator('[data-host-icon]').first()).toBeVisible();
    });

    test('each card draws a recency dot in its tmux idle-fade colour', async ({ page }) => {
      await openList(page);
      const want: Record<string, [string, string]> = {
        luxnow: ['0', 'rgb(184, 187, 38)'],
        [freshAgent]: ['0', 'rgb(184, 187, 38)'],
        luxq: ['2', 'rgb(104, 157, 106)'],
        luxorange: ['4', 'rgb(214, 93, 14)'],
        luxancient: ['7', 'rgb(102, 92, 84)'],
        [oldAgent]: ['7', 'rgb(102, 92, 84)'],
      };
      for (const [k, [bucket, colour]] of Object.entries(want)) {
        const dot = dotOf(card(page, k));
        await expect(dot, k).toHaveAttribute('data-recency', bucket);
        await expect(dot, k).toHaveCSS('background-color', colour);
        const box = (await dot.boundingBox())!;
        expect(Math.round(box.width), `${k}: the dot is 8px`).toBe(8);
      }
      // The words carry the same fact, beside the dot.
      await expect(card(page, 'luxorange').locator('[data-card-active]')).toContainText(/active\s*3h\s*ago/);
    });

    test('a long title is clamped; its "…" opens the whole title and never opens the agent', async ({ page }) => {
      await openList(page);
      const c = card(page, freshAgent);
      const title = c.locator('[data-card-title]');
      await expect(title).toHaveText(LONG.slice(0, 60).trimEnd());
      await expect(title).toHaveAttribute('title', LONG);
      const more = c.locator('button[data-title-more]');
      await expect(more).toHaveAttribute('aria-expanded', 'false');
      const box = (await more.boundingBox())!;
      expect(box.height, 'a 44px target').toBeGreaterThanOrEqual(44);
      expect(box.width, 'a 44px target').toBeGreaterThanOrEqual(44);
      const before = page.url();
      if (vp.use.hasTouch) await more.tap();
      else {
        await more.focus();
        await page.keyboard.press('Enter');
      }
      const pop = page.locator(`#${await more.getAttribute('aria-controls')}`);
      await expect(pop).toBeVisible();
      await expect(pop).toContainText(LONG);
      await expect(more).toHaveAttribute('aria-expanded', 'true');
      await page.waitForTimeout(300);
      expect(page.url(), 'the tap did not navigate').toBe(before);
      // The popover fits the screen.
      const pb = (await pop.boundingBox())!;
      expect(pb.x).toBeGreaterThanOrEqual(0);
      expect(pb.x + pb.width).toBeLessThanOrEqual(vp.use.viewport.width);
      await page.keyboard.press('Escape');
      await expect(pop).toBeHidden();
      await expect(more).toHaveAttribute('aria-expanded', 'false');
      // The clamped words themselves still open the agent.
      await Promise.all([page.waitForURL((u) => u.pathname === `/agents/${freshAgent}`), press(title)]);
    });

    test('each chip filters the list, they combine, and the choice survives a refresh and a reload', async ({ page }) => {
      await openList(page);
      await expect(page.locator('#agents-chips')).toHaveAttribute('role', 'group');
      // Each chip on its own (and off again).
      // The merged chip's four kinds of card, each really on the list with its
      // attention badge — and the draft's agent badged too, so that it is NOT
      // shown below is the chip's rule, not a card without the item.
      await expect(card(page, askAgent).locator('[data-attention-badge]')).toHaveAttribute('data-kind', 'ask');
      await expect(card(page, 'luxnow').locator('[data-attention-badge]')).toHaveAttribute('data-kind', 'session');
      await expect(card(page, 'luxq').locator('[data-attention-badge]')).toHaveAttribute('data-kind', 'question');
      await expect(card(page, draftAgent).locator('[data-attention-badge]')).toHaveAttribute('data-kind', 'plugin');
      // ONE wait chip: the two it replaced are gone.
      await expect(page.locator('#agents-chips [data-chip-group="wait"]')).toHaveCount(1);
      await expect(chip(page, 'waiting')).toHaveText('Waiting on you');
      await expect(chip(page, 'ask')).toHaveCount(0);
      await expect(chip(page, 'question')).toHaveCount(0);
      const cases: [string[], string[]][] = [
        [['waiting'], WAITING],
        [['idle-15m'], ['luxq', 'luxorange', 'luxancient', oldAgent]],
        [['idle-1h'], ['luxorange', 'luxancient', oldAgent]],
        [['idle-1d'], ['luxancient', oldAgent]],
        // The chips OR: a card matching ANY chosen chip is shown.
        [['waiting', 'idle-1d'], ['luxnow', askAgent, 'luxq', 'luxancient', oldAgent]],
        [['waiting', 'idle-1h'], ['luxnow', askAgent, 'luxq', 'luxorange', 'luxancient', oldAgent]],
      ];
      for (const [chips, want] of cases) {
        for (const id of chips) await press(chip(page, id));
        for (const id of chips) await expect(chip(page, id)).toHaveAttribute('aria-pressed', 'true');
        await expect.poll(() => shown(page), { message: `chips ${chips.join('+')}` }).toEqual(want);
        for (const id of chips) await press(chip(page, id));
        await expect.poll(() => shown(page), { message: `chips ${chips.join('+')} off again` }).toEqual(ORDER);
      }
      // The idle chips are one at a time.
      await press(chip(page, 'idle-15m'));
      await press(chip(page, 'idle-1d'));
      await expect(chip(page, 'idle-15m')).toHaveAttribute('aria-pressed', 'false');
      await expect(chip(page, 'idle-1d')).toHaveAttribute('aria-pressed', 'true');
      await press(chip(page, 'idle-1d'));

      // Remembered: choose two, refresh the list in the background, then reload.
      const kept = ['luxnow', askAgent, 'luxq', 'luxancient', oldAgent];
      await press(chip(page, 'waiting'));
      await press(chip(page, 'idle-1d'));
      await expect.poll(() => shown(page)).toEqual(kept);
      const swapped = page.waitForResponse((r) => new URL(r.url()).pathname === '/ui/agents');
      await page.evaluate(() => document.body.dispatchEvent(new Event('muster:resync', { bubbles: true })));
      await swapped;
      await page.waitForTimeout(300);
      await expect(chip(page, 'waiting'), 'a list refresh keeps the chips').toHaveAttribute('aria-pressed', 'true');
      expect(await shown(page), 'a list refresh keeps the filter').toEqual(kept);
      expect(JSON.parse((await page.evaluate(() => localStorage.getItem('muster:agents:chips'))) || '[]')).toEqual(['waiting', 'idle-1d']);
      await page.reload();
      await expect(chip(page, 'waiting')).toHaveAttribute('aria-pressed', 'true');
      await expect(chip(page, 'idle-1d')).toHaveAttribute('aria-pressed', 'true');
      await expect.poll(() => shown(page), { timeout: 20_000 }).toEqual(kept);
      await press(chip(page, 'waiting'));
      await press(chip(page, 'idle-1d'));
      await expect.poll(() => page.evaluate(() => localStorage.getItem('muster:agents:chips'))).toBeNull();
      // The chip row never scrolls the page sideways.
      const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
      expect(sw).toBeLessThanOrEqual(cw);
    });

    // A device that remembered a retired chip ("ask" — Waiting on ask — or
    // "question") opens with "Waiting on you" pressed: the page neither breaks
    // nor silently drops the choice. Red before: the remembered ids were the
    // two chips themselves, and no "waiting" chip existed to press.
    test('a remembered "ask" or "question" chip opens as "Waiting on you"', async ({ page }) => {
      await openList(page);
      for (const [stored, want, pressed] of [
        [['ask'], WAITING, ['waiting']],
        [['question', 'idle-1d'], ['luxnow', askAgent, 'luxq', 'luxancient', oldAgent], ['waiting', 'idle-1d']],
        [['ask', 'question', 'nonsense'], WAITING, ['waiting']],
      ] as [string[], string[], string[]][]) {
        await page.evaluate((v) => localStorage.setItem('muster:agents:chips', JSON.stringify(v)), stored);
        await page.reload();
        for (const id of pressed) await expect(chip(page, id), `stored ${stored}`).toHaveAttribute('aria-pressed', 'true');
        await expect.poll(() => shown(page), { message: `stored ${stored}`, timeout: 20_000 }).toEqual(want);
      }
      // Toggling writes the chip's own id from then on.
      await press(chip(page, 'waiting'));
      expect(JSON.parse((await page.evaluate(() => localStorage.getItem('muster:agents:chips'))) || '[]')).toEqual([]);
      await expect.poll(() => shown(page)).toEqual(ORDER);
    });

    // 🔴 A WINDOW HOLDING ANOTHER BUILD'S RULES TABLE. window.__musterAgentsRules
    // (agents_filter_head.go) outlives a boosted navigation, so a window whose
    // table an OLDER build defined kept it on a page this build drew: "Waiting
    // on you" (data-chip="waiting") looked up nothing in it and filtered
    // nothing, and a stored ["waiting"] was dropped as unknown. Here the window
    // is given an older build's table (ids ask/question, no version), then
    // makes a boosted navigation out to an agent's page and back to the list:
    // the table must be this build's again and the chip must filter. Red
    // before: the table was defined once per window and the bound script kept
    // the one it started with.
    test("a window holding an older build's rules table is put right by a boosted navigation: the chip filters", async ({ page }) => {
      await openList(page);
      await page.addInitScript(() => {
        let real: unknown;
        const w = window as unknown as Record<string, unknown>;
        w.__keepStale = true;
        const sel: Record<string, string> = {
          ask: '[data-wait~="ask"]', question: '[data-wait~="question"]',
          'idle-15m': '[data-idle~="15m"]', 'idle-1h': '[data-idle~="1h"]', 'idle-1d': '[data-idle~="1d"]',
        };
        Object.defineProperty(window, '__musterAgentsRules', {
          configurable: true,
          get: () => real,
          set: (t: Record<string, unknown>) => {
            if (w.__keepStale) {
              t = Object.assign({}, t, {
                v: 'older-build',
                chipsOf: (raw: string) => {
                  try { const v = JSON.parse(raw || '[]'); return Array.isArray(v) ? v.filter((c) => c in sel) : []; } catch { return []; }
                },
                chipSel: (cs: string[]) => {
                  const ps = cs.filter((c) => c in sel).map((c) => sel[c]);
                  return ps.length === 0 ? '' : ps.length === 1 ? ps[0] : ':is(' + ps.join(',') + ')';
                },
              });
            }
            real = t;
          },
        });
      });
      await page.evaluate(() => localStorage.setItem('muster:agents:chips', JSON.stringify(['waiting'])));
      await page.goto(origin + '/agents');
      const v = () => page.evaluate(() => (window as unknown as { __musterAgentsRules: { v?: string } }).__musterAgentsRules.v);
      expect(await v(), "control: the window holds the older build's table").toBe('older-build');
      await page.evaluate(() => {
        const w = window as unknown as Record<string, unknown>;
        w.__keepStale = false;
        w.__sameWindow = 1;
      });
      // Out to an agent's page and back to the list, both by boosted links:
      // one window throughout.
      // boostedTo follows a link drawn inside the page's boosted region and
      // processed by htmx, as one the page drew itself would be.
      const boostedTo = (href: string) =>
        page.evaluate((h) => {
          const a = document.createElement('a');
          a.href = h;
          a.textContent = 'go';
          document.querySelector('[hx-boost="true"]')!.prepend(a);
          (window as unknown as { htmx: { process: (e: Element) => void } }).htmx.process(a);
          a.click();
        }, href);
      const same = () => page.evaluate(() => (window as unknown as { __sameWindow?: number }).__sameWindow);
      await Promise.all([page.waitForURL((u) => u.pathname === `/agents/${freshAgent}`), boostedTo(`/agents/${freshAgent}`)]);
      expect(await same(), 'control: boosted out, the same window').toBe(1);
      await Promise.all([page.waitForURL((u) => u.pathname === '/agents'), boostedTo('/agents')]);
      expect(await same(), 'control: boosted back, the same window').toBe(1);
      await expect.poll(v, { message: "the table is this build's again" }).not.toBe('older-build');
      await expect(chip(page, 'waiting'), 'the stored chip survived').toHaveAttribute('aria-pressed', 'true');
      await expect.poll(async () => { await takeNew(page); return shown(page); }, { timeout: 20_000 }).toEqual(WAITING);
      // And a press now goes through this build's table too.
      await press(chip(page, 'waiting'));
      await expect.poll(() => shown(page)).toEqual(ORDER);
      await press(chip(page, 'waiting'));
      await expect.poll(() => shown(page)).toEqual(WAITING);
      await press(chip(page, 'waiting'));
    });

    test('Clear turns every chip off, and is shown only while a chip is on', async ({ page }) => {
      await openList(page);
      const clear = page.locator('#agents-chips-clear');
      await expect(clear, 'no chip on: no Clear').toBeHidden();
      await press(chip(page, 'waiting'));
      await press(chip(page, 'idle-1h'));
      await expect.poll(() => shown(page)).toEqual(['luxnow', askAgent, 'luxq', 'luxorange', 'luxancient', oldAgent]);
      await expect(clear).toBeVisible();
      const box = (await clear.boundingBox())!;
      expect(box.height, 'a 44px target').toBeGreaterThanOrEqual(44);
      await press(clear);
      await expect(chip(page, 'waiting')).toHaveAttribute('aria-pressed', 'false');
      await expect(chip(page, 'idle-1h')).toHaveAttribute('aria-pressed', 'false');
      await expect.poll(() => shown(page)).toEqual(ORDER);
      await expect(clear).toBeHidden();
      expect(await page.evaluate(() => localStorage.getItem('muster:agents:chips'))).toBeNull();
      await page.reload();
      await expect(clear, 'still cleared after a reload').toBeHidden();
    });

    test('a long focus title is clamped too; its "…" opens the whole title and never opens the agent', async ({ page }) => {
      await openList(page);
      const c = card(page, oldAgent);
      const heading = c.locator('[data-card-focus]');
      await expect(heading.locator('span').first()).toHaveText(FOCUS.slice(0, 60).trimEnd());
      const more = c.locator('[data-card-slot="focus"] button[data-title-more]');
      const box = (await more.boundingBox())!;
      expect(box.height).toBeGreaterThanOrEqual(44);
      expect(box.width).toBeGreaterThanOrEqual(44);
      const before = page.url();
      await press(more);
      const pop = c.locator('[data-focus-popover]');
      await expect(pop).toBeVisible();
      await expect(pop.locator('[data-focus-title]')).toHaveText(FOCUS);
      await expect(more).toHaveAttribute('aria-expanded', 'true');
      await page.waitForTimeout(300);
      expect(page.url(), 'the tap did not navigate').toBe(before);
      await page.keyboard.press('Escape');
      await expect(pop).toBeHidden();
    });

    test('a card crossing an idle threshold under an idle chip waits behind the pill: nothing visible moves', async ({ page }) => {
      // The page's clock is installed at the real time, so the server's
      // activity times and the script's agree; then fifteen minutes pass with
      // no input, carrying luxnow (1 min) and the fresh agent (5 min) past 15.
      await page.clock.install({ time: new Date() });
      await openList(page);
      await press(chip(page, 'idle-15m'));
      const visible = ['luxq', 'luxorange', 'luxancient', oldAgent];
      await expect.poll(() => shown(page)).toEqual(visible);
      const tops = async () => {
        const out: number[] = [];
        for (const k of visible) out.push(Math.round((await card(page, k).boundingBox())!.y));
        return out;
      };
      const before = await tops();
      await page.clock.fastForward('16:00');
      await page.waitForTimeout(200);
      expect(await tops(), 'no visible card moved').toEqual(before);
      expect(await shown(page), 'the cards that crossed are not shown yet').toEqual(visible);
      const pill = page.locator('#agents-new-pill');
      await expect(pill).toBeVisible();
      expect(Number(await pill.getAttribute('data-count')), 'both crossings wait on the pill').toBeGreaterThanOrEqual(2);
      // The tap is input: what crossed is applied.
      await pill.click();
      await expect.poll(() => shown(page)).toEqual(ORDER);
      await expect(pill).toBeHidden();
      await press(chip(page, 'idle-15m'));
    });

    test('Mark complete on an agent page goes back out to the Agents list (with JavaScript)', async ({ page }) => {
      await login(page);
      await page.goto(origin + `/agents/${doneAgent(vp.name, true)}`);
      const btn = page.getByRole('button', { name: 'Mark complete' });
      await expect(btn).toBeVisible({ timeout: 20_000 });
      await Promise.all([page.waitForURL((u) => u.pathname === '/agents', { timeout: 20_000 }), press(btn)]);
      await expect(page.locator('#agents-list')).toBeVisible();
      await expectTaskComplete(doneTask(vp.name, true));
    });

    test('Mark complete on an agent page goes back out to the Agents list (without JavaScript)', async ({ browser }) => {
      await noJS(browser, vp.use, async (page) => {
        await page.goto(origin + `/agents/${doneAgent(vp.name, false)}`);
        const btn = page.getByRole('button', { name: 'Mark complete' });
        await expect(btn).toBeVisible();
        await Promise.all([page.waitForURL((u) => u.pathname === '/agents', { timeout: 15_000 }), btn.click()]);
      });
      await expectTaskComplete(doneTask(vp.name, false));
    });
  });
}

// noJS runs fn in a signed-in context with JavaScript OFF.
async function noJS(browser: Browser, use: object, fn: (page: Page) => Promise<void>) {
  const ctx = await browser.newContext({ ...use, javaScriptEnabled: false, baseURL: origin });
  try {
    const p = await ctx.newPage();
    await p.goto(origin + '/login');
    await p.fill('#muster-password', password);
    await Promise.all([p.waitForURL((u) => !u.pathname.startsWith('/login')), p.click('button[type=submit]')]);
    await fn(p);
  } finally {
    await ctx.close();
  }
}

// expectTaskComplete reads the task back through the machine API.
async function expectTaskComplete(title: string) {
  const r = await fetch(origin + '/api/tasks', { headers: { Authorization: `Bearer ${hookToken}` } });
  expect(r.status).toBe(200);
  const tasks = (await r.json()) as { title: string; status: string }[];
  const t = tasks.find((x) => x.title === title);
  expect(t, `the task ${title} exists`).toBeTruthy();
  expect(t!.status).toBe('complete');
}
