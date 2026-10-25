import { Browser, Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { authState, baseURL, clsLongMirrorPort } from '../lib/env';
import { clsInit, readCls, settle, ClsResult } from '../lib/cls';
import { cc, MirrorRig, mkchat } from '../lib/mirrorhost';
import { Profile, THROTTLED, throttle } from '../lib/throttle';

// A LONG TRANSCRIPT OPENS WITH ITS BUBBLES AND ITS COMPOSER STILL — no shift
// sourced to either, and under 0.01 in all — at a phone and a desktop, on an
// unthrottled loopback and under the throttled profile (lib/throttle.ts).
//
// cls.spec.ts measures every page, but its agent page is the seeded
// docs-sweeper: seven messages, which fit on one screen. An operator's agent
// holds up to chatHistoryCap (200, internal/api/agents.go) messages, and the
// page opens scrolled to the end of them. This spec opens agents of its own
// (e2e/mkchat) holding exactly that many, in two variants:
//
//   - markdown: assistant replies with a heading, emphasis, a list, inline code
//     and a fenced block, long enough for the bubble clamp (bubble_clamp.go);
//   - plain: the same lengths in plain sentences, so a shift that is not the
//     markdown conversion is still seen.
//
// 🔴 REGRESSION TEST. Before the fix, the transcript was converted, clamped and
// scrolled to its end only at DOMContentLoaded, which waits for the DEFERRED
// vendor scripts; whenever they landed after the document had parsed, the raw
// transcript was painted first, scrolled to the top, and then every bubble on
// screen moved (agentChatScript's window.__cgLayoutLog says how). Unthrottled
// at 1280px that is a race this harness usually wins (measured on main: 0.37,
// 0, 0 markdown; 0, 0, 0 plain), so the desktop is opened throttled only, where
// the race is lost every time (0.37 and 0.43, three of three each). The phone
// read red unthrottled every time (0.48 / 0.47, three of three), so it is
// opened both ways. The scripts-held cases below are the race-free red.
//
// 🔴 THE PHONE'S NUMBER IS total + excluded, NOT total. Under Playwright's
// mobile emulation (isMobile + hasTouch) Chromium reports a shift in the first
// moments after load with hadRecentInput=true though nothing was input —
// measured: a block injected 100 ms after load was all `excluded`, the same
// block 3 s after load all `total`; at 390px without the emulation, both
// `total`. This spec gives the page no input at all, so every excluded shift is
// that artefact, and it is counted. (cls.spec.ts's phone measurements count
// total only, and so do not see a shift in that window.)
//
// Every message is invented fixture data.
const BAR = 0.01;
const CAP = 200;
// How many times a case is opened, each in a fresh context (a cold HTTP
// cache): the unthrottled phone case three times, because the defect is a race
// with the first paint; a throttled case once, because throttled it loses that
// race every time (measured: three of three on main, every case).
const RUNS_UNTHROTTLED = 3;
const RUNS_THROTTLED = 1;
// The seeded short agent's opens: at the measured 8-in-16 rate, all OPENS of
// a broken tree pass by luck about once in 2^OPENS.
const OPENS = 8;
const stamp = Date.now().toString(36);
type Kind = 'markdown' | 'plain';
const agents: Record<Kind, string> = { markdown: `cls-long-md-${stamp}`, plain: `cls-long-plain-${stamp}` };

const sentences = (i: number, n: number) =>
  Array.from({ length: n }, (_, j) => `Sentence ${j + 1} about module-${i}: the value was checked against the expected default and matched after the last migration ran.`).join(' ');

function transcript(kind: Kind): { role: string; content: string }[] {
  const out: { role: string; content: string }[] = [];
  for (let i = 0; i < CAP; i++) {
    if (i % 2 === 0) {
      out.push({ role: 'user', content: `Step ${i / 2 + 1}: ${sentences(i, 1)}` });
    } else if (kind === 'markdown') {
      out.push({
        role: 'assistant',
        content: `## Module ${i}\n\nChecked **module-${i}**. ${sentences(i, 2)}\n\n- one \`key_a\`\n- two \`key_b\`\n- three\n\n\`\`\`sh\nmake build-${i}\nmake test-${i}\n\`\`\`\n\n${sentences(i, 3)}`,
      });
    } else {
      out.push({ role: 'assistant', content: `${sentences(i, 2)}\n\n${sentences(i, 3)}` });
    }
  }
  return out;
}

const viewports = [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
];
const profiles: { name: string; p: Profile | null }[] = [
  { name: 'unthrottled', p: null },
  { name: THROTTLED.name, p: THROTTLED },
];
// The opening cases: the desktop throttled only (see the header).
const openCases = (vp: (typeof viewports)[number]) =>
  profiles
    .filter((pr) => pr.p || vp.use.isMobile)
    .map((pr) => ({ ...pr, runs: pr.p ? RUNS_THROTTLED : RUNS_UNTHROTTLED }));

// moved is every shift measured, input-excluded or not (see the header).
const moved = (r: ClsResult) => +(r.total + r.excluded).toFixed(5);
const report = (label: string, r: ClsResult & { where?: string[] }) =>
  `${label}: ${moved(r)} (total ${r.total}, excluded ${r.excluded}) — ${JSON.stringify(r.shifts)} excluded: ${JSON.stringify(r.excludedShifts)}${r.where ? ' where: ' + JSON.stringify(r.where) : ''}`;
// formShifts are the shifts, counted or excluded, that moved the composer.
// 🔴 The composer must never move at all: a layout call placed between the log
// and #chat-form let a frame paint the form before its box was parsed (13px,
// then 57px) — under the bar, but a shift main did not have.
const formShifts = (r: ClsResult) => [...r.shifts, ...r.excludedShifts].filter((x) => x.sources.some((n) => n.startsWith('form#chat-form')));
// ⚠ ONE SHIFT IS NOT THIS PAGE'S TRANSCRIPT OR COMPOSER, AND IS ALLOWED HERE:
// the header's title box (agentTitle, `div.mr-auto.flex.min-w-0`) moving by
// 0.00026 at 390px on some opens of an agent whose name is clipped. It is on
// origin/main too (measured: present in the same cases' excluded entries there)
// and its cause is not established; it is under the bar and reported, not
// fixed, here. Anything else that moves fails these cases.
//
// The allowance is STRUCTURAL, not spelled: titleInit records, for each
// layout-shift entry, whether every one of its source nodes is inside the
// header's [data-agent-title] box (the box itself, or its chevron <svg>, whose
// spelled name is the same as the switcher toggle's). foreignShifts keeps
// every entry that is not.
const titleInit = `(() => {
  window.__titleOnly = [];
  // where: for each entry, each source's place — 'title' inside the header's
  // title box, else the id of its nearest ancestor that has one (diagnostic).
  window.__where = [];
  try {
    new PerformanceObserver((l) => {
      for (const e of l.getEntries()) {
        const ns = (e.sources || []).map((x) => x.node).filter(Boolean);
        if (ns.length && ns.every((n) => n.nodeType === 1 && n.closest('[data-agent-title]'))) window.__titleOnly.push(Math.round(e.startTime));
        window.__where.push(Math.round(e.startTime) + ':' + ns.map((n) => (n.nodeType === 1 && n.closest('[data-agent-title]')) ? 'title' : ((n.nodeType === 1 && n.closest('[id]')) || { id: '?' }).id).join(','));
      }
    }).observe({ type: 'layout-shift', buffered: true });
  } catch (e) {}
})()`;
type Moved = ClsResult & { titleOnly: number[]; where?: string[] };
async function readMoved(page: Page): Promise<Moved> {
  const r = await readCls(page);
  return { ...r, titleOnly: await page.evaluate(() => (window as any).__titleOnly || []), where: await page.evaluate(() => (window as any).__where || []) };
}
const foreignShifts = (r: Moved) => [...r.shifts, ...r.excludedShifts].filter((x) => !r.titleOnly.includes(x.at));

// heldOpen opens url with the deferred vendor scripts held unanswered (so
// DOMContentLoaded and start() wait on them), waits until the document has
// parsed and a few frames have painted, and returns started(): let them
// through and wait for start() and the page to settle.
async function heldOpen(browser: Browser, use: object, url: string) {
  const ctx = await browser.newContext({ ...use, baseURL, storageState: authState });
  const page = await ctx.newPage();
  let release: () => void = () => {};
  const gate = new Promise<void>((r) => (release = r));
  let held = 0;
  await page.route('**/static/vendor/*.js*', async (route) => {
    held++;
    await gate;
    await route.continue().catch(() => {});
  });
  await page.addInitScript(clsInit);
  await page.addInitScript(titleInit);
  await page.goto(url, { waitUntil: 'commit' });
  await expect.poll(() => page.evaluate(() => document.readyState), { message: 'the document parsed' }).toBe('interactive');
  await page.evaluate(() => new Promise<void>((r) => { let n = 0; const f = () => (++n < 10 ? requestAnimationFrame(f) : r()); requestAnimationFrame(f); }));
  expect(held, 'both deferred vendor scripts are being held').toBe(2);
  expect(await page.evaluate(() => !!(window as any).htmx), 'control: htmx has not run').toBe(false);
  const started = async () => {
    release();
    await expect.poll(() => page.evaluate(() => !!(window as any).__cgChat), { message: 'start() ran once the scripts landed' }).toBe(true);
    await settle(page);
  };
  return { ctx, page, started };
}

// open loads url in a fresh signed-in context under profile p and lets it
// settle. The caller closes the context.
async function open(browser: Browser, use: object, p: Profile | null, url: string, o: { origin?: string; init?: string; state?: string | object } = {}) {
  const origin = o.origin ?? baseURL;
  const ctx = await browser.newContext({ ...use, baseURL: origin, storageState: (o.state ?? authState) as never });
  const page = await ctx.newPage();
  await throttle(page, p);
  await page.addInitScript(clsInit);
  await page.addInitScript(titleInit);
  if (o.init) await page.addInitScript(o.init);
  await page.goto(origin + url);
  await settle(page);
  return { ctx, page };
}

async function atEnd(page: Page, log: string): Promise<boolean> {
  return page.locator(log).evaluate((l) => l.scrollHeight - l.scrollTop - l.clientHeight < 48);
}

test.describe('a 200-message agent transcript', () => {
  test.beforeAll(() => {
    for (const k of ['markdown', 'plain'] as const) {
      mkchat(agents[k], null);
      mkchat(agents[k], transcript(k));
    }
  });
  test.afterAll(() => {
    for (const k of ['markdown', 'plain'] as const) mkchat(agents[k], null);
  });

  for (const vp of viewports) {
    for (const pr of openCases(vp)) {
      for (const kind of ['markdown', 'plain'] as const) {
        test(`${vp.name}, ${pr.name}, ${kind}: opening the agent moves neither the transcript nor the composer, ${pr.runs}×`, async ({ browser }) => {
          test.setTimeout(120_000);
          const seen: number[] = [];
          const bad: string[] = [];
          for (let i = 0; i < pr.runs; i++) {
            const { ctx, page } = await open(browser, vp.use, pr.p, `/agents/${agents[kind]}`);
            try {
              const r = await readMoved(page);
              // What was measured is the drawn transcript, opened at its end.
              await expect(page.locator('#chat-log [data-bubble]')).toHaveCount(CAP);
              await expect(page.locator('#chat-log [data-md-done]'), 'every bubble was converted').toHaveCount(CAP);
              expect(await atEnd(page, '#chat-log'), 'the log opened at its end').toBe(true);
              seen.push(moved(r));
              if (moved(r) >= BAR || foreignShifts(r).length) bad.push(report(`run ${i + 1}`, r));
            } finally {
              await ctx.close();
            }
          }
          console.log(`[cls-long ${vp.name}, ${pr.name}] ${kind}: ${seen.join(' ')}`);
          expect(bad).toEqual([]);
        });
      }
    }

    // THE MECHANISM, WITHOUT THE RACE. The deferred vendor scripts are held
    // unanswered, so the document parses and paints with DOMContentLoaded (and
    // start()) still waiting on them — the window the defect painted in. In it,
    // the transcript must already be in its final layout: every bubble
    // converted, the log at its end (with #chat-form, parsed after the layout
    // call, under it), and nothing moved. Then the scripts are let through, and
    // the whole open still moves less than the bar.
    for (const kind of ['markdown', 'plain'] as const) {
      test(`${vp.name}, ${kind}: before the deferred scripts arrive, the transcript is already laid out`, async ({ browser }) => {
        const { ctx, page, started } = await heldOpen(browser, vp.use, `/agents/${agents[kind]}`);
        try {
          await expect(page.locator('#chat-log [data-md-done]'), 'every bubble converted before start()').toHaveCount(CAP);
          expect(await atEnd(page, '#chat-log'), 'the log is at its end before start()').toBe(true);
          const before = await readMoved(page);
          await started();
          const after = await readMoved(page);
          console.log(`[cls-long ${vp.name}] ${kind}, scripts held: ${moved(before)}, after they land: ${moved(after)}`);
          expect(moved(before), report('while the scripts were held', before)).toBeLessThan(BAR);
          expect(foreignShifts(before), report('while the scripts were held', before)).toEqual([]);
          expect(moved(after), report('the whole open', after)).toBeLessThan(BAR);
          expect(foreignShifts(after), report('the whole open', after)).toEqual([]);
          expect(formShifts(after), report('the composer moved', after)).toEqual([]);
          expect(await atEnd(page, '#chat-log'), 'still at its end').toBe(true);
        } finally {
          await ctx.close();
        }
      });
    }

    // 🔴 A PAINT BETWEEN THE LOG AND THE LAYOUT CALL MOVES NOTHING. The parser
    // can yield anywhere (under load it did, mid-form: 0.50 at 390px), so the
    // document is held here, straight after #chat-log and before #chat-form,
    // on a parser-blocking script that answers after 1.2 s: the browser paints
    // the log as it stands. The frames painted in that hold are the positive
    // control (they must show the log's bubbles in the DOM).
    // The same hold INSIDE the session switcher (#248), straight after its
    // <nav> tag: a 1px bar painted before its row, that grew to 45px, read as
    // a shift of the bar.
    const stalls = [
      { label: 'before the layout call', at: (html: string) => html.indexOf('<form id="chat-form"') },
      { label: 'inside the session switcher', at: (html: string) => { const n = html.indexOf('id="session-switcher"'); return n < 0 ? -1 : html.indexOf('>', n) + 1; } },
      // Inside the switcher's toggle, between its icon and its label.
      { label: "inside the switcher's toggle", at: (html: string) => html.indexOf('<span data-switcher-label') },
      // Inside #chat-form, straight after its opening tag: CI's frame (the
      // form 58px with only its first <input>, then 102px) made deterministic.
      { label: 'inside the composer', at: (html: string) => { const n = html.indexOf('<form id="chat-form"'); return n < 0 ? -1 : html.indexOf('>', n) + 1; } },
      // Straight after #chat-form: the composer is parsed, the scripts that
      // size its box (composer_fit.go, composer_draft.go) not yet.
      { label: 'after the composer, before its sizing scripts', at: (html: string) => { const n = html.indexOf('id="chat-form"'); return n < 0 ? -1 : html.indexOf('</form>', n) + '</form>'.length; } },
    ];
    for (const stall of stalls)
    test(`${vp.name}: a frame painted ${stall.label} moves nothing`, async ({ browser }) => {
      const ctx = await browser.newContext({ ...vp.use, baseURL, storageState: authState });
      try {
        const page = await ctx.newPage();
        await page.route(/\/agents\/[^/?]+(\?.*)?$/, async (r) => {
          if (r.request().resourceType() !== 'document') return r.continue();
          const res = await r.fetch();
          const html = await res.text();
          const at = stall.at(html);
          if (at <= 0) throw new Error(`no stall point ${stall.label} in the document`);
          await r.fulfill({ response: res, body: html.slice(0, at) + '<script src="/__e2e-stall.js"></script>' + html.slice(at) });
        });
        await page.route('**/__e2e-stall.js', async (r) => {
          await new Promise((res) => setTimeout(res, 1200));
          await r.fulfill({ contentType: 'text/javascript', body: 'window.__stallEnded = true;' });
        });
        await page.addInitScript(clsInit);
        await page.addInitScript(titleInit);
        await page.addInitScript(`(() => {
          const tick = () => {
            if (!window.__stallEnded && document.querySelectorAll('#chat-log [data-bubble]').length) window.__heldFrames = (window.__heldFrames || 0) + 1;
            if (document.readyState !== 'complete') requestAnimationFrame(tick);
          };
          requestAnimationFrame(tick);
        })()`);
        await page.goto(`/agents/${agents.markdown}`);
        await settle(page);
        const r = await readMoved(page);
        const held = await page.evaluate(() => (window as any).__heldFrames || 0);
        console.log(`[cls-long ${vp.name}] stalled ${stall.label}: ${moved(r)} (${held} frames held)`);
        expect(held, `control: frames were painted with the bubbles in the DOM, stalled ${stall.label}`).toBeGreaterThan(0);
        await expect(page.locator('#chat-log [data-md-done]')).toHaveCount(CAP);
        expect(await atEnd(page, '#chat-log'), 'at its end').toBe(true);
        expect(moved(r), report(`stalled ${stall.label}`, r)).toBeLessThan(BAR);
        expect(foreignShifts(r), report(`stalled ${stall.label}`, r)).toEqual([]);
      } finally {
        await ctx.close();
      }
    });

    // THE WHOLE PAGE: a 200-message transcript AND the session switcher below
    // the composer (#248: switcher.go, in the room the form reserves), opened
    // OPENS times. Nothing but the named title-box shift may move.
    test(`${vp.name}: the 200-message page with the session switcher, ${OPENS}×`, async ({ browser }) => {
      test.setTimeout(180_000);
      const seen: number[] = [];
      const bad: string[] = [];
      for (let i = 0; i < OPENS; i++) {
        const { ctx, page } = await open(browser, vp.use, null, `/agents/${agents.markdown}`);
        try {
          await expect(page.locator('#session-switcher'), 'the switcher is drawn').toBeVisible();
          await expect(page.locator('#chat-log [data-md-done]')).toHaveCount(CAP);
          expect(await atEnd(page, '#chat-log'), 'at its end').toBe(true);
          const r = await readMoved(page);
          seen.push(moved(r));
          if (moved(r) >= BAR || foreignShifts(r).length) bad.push(report(`open ${i + 1}`, r));
        } finally {
          await ctx.close();
        }
      }
      console.log(`[cls-long ${vp.name}] with the switcher: ${seen.join(' ')}`);
      expect(bad).toEqual([]);
    });

    // 🔴 A READER WHO SCROLLED UP BEFORE start() STAYS THERE. On a slow network
    // the transcript is readable (laid out, at its end) for as long as the
    // deferred scripts take; start() used to pin the log to its end
    // unconditionally when they landed, pulling a reader 3000px back down.
    test(`${vp.name}: a reader who scrolled up while the scripts were loading is not pulled back to the end`, async ({ browser }) => {
      const { ctx, page, started } = await heldOpen(browser, vp.use, `/agents/${agents.markdown}`);
      try {
        const gap = () => page.locator('#chat-log').evaluate((l) => Math.round(l.scrollHeight - l.scrollTop - l.clientHeight));
        expect(await gap(), 'at the end before scrolling').toBeLessThan(48);
        await page.locator('#chat-log').evaluate((l) => { l.scrollTop -= 3000; });
        await expect.poll(gap, { message: 'scrolled 3000px up' }).toBeGreaterThan(2900);
        const up = await gap();
        await started();
        expect(await gap(), 'the gap is kept after start()').toBe(up);
      } finally {
        await ctx.close();
      }
    });

    // THE COMPOSER IS NEVER PAINTED HALF-PARSED, on the seeded seven-message
    // agent too (the auditor's measurement: a #chat-form shift in 8 of 16 such
    // opens with the layout call between the log and the form, 0 of 16 on
    // main). Opened OPENS times, nothing may move at all.
    test(`${vp.name}: the seeded short agent opens with nothing moved, ${OPENS}×`, async ({ browser }) => {
      test.setTimeout(120_000);
      const bad: string[] = [];
      for (let i = 0; i < OPENS; i++) {
        const { ctx, page } = await open(browser, vp.use, null, '/agents/docs-sweeper');
        try {
          await expect(page.locator('#chat-log [data-tool-group]').first(), 'the transcript is drawn').toBeVisible();
          const r = await readCls(page);
          if (moved(r) > 0 || formShifts(r).length) bad.push(report(`open ${i + 1}`, r));
        } finally {
          await ctx.close();
        }
      }
      expect(bad).toEqual([]);
    });

    // The positive control for the counting: a shift the page did not ask for,
    // 100 ms after load (unthrottled, the window in which the phone's emulation
    // marks a shift as input), on this same page, MUST be measured.
    test(`${vp.name}: positive control: an unprovoked shift just after load is measured`, async ({ browser }) => {
      const init = `addEventListener('load', () => setTimeout(() => {
        const l = document.getElementById('chat-log');
        const d = document.createElement('div');
        d.style.height = '160px';
        l.parentElement.insertBefore(d, l);
      }, 100));`;
      const { ctx, page } = await open(browser, vp.use, null, `/agents/${agents.markdown}`, { init });
      try {
        const r = await readCls(page);
        console.log(`[cls-long ${vp.name}] positive control: ${report('injected', r)}`);
        expect(moved(r), report('injected shift', r)).toBeGreaterThan(BAR);
      } finally {
        await ctx.close();
      }
    });
  }
});

// A BOOSTED ARRIVAL: from one long agent to another by an hx-boost link, so
// the new body (and its scripts, and the layout call) is swapped in by htmx
// rather than parsed. One composer, wired, every bubble converted, at the end,
// and no page error.
test.describe('a boosted agent → agent arrival on a long transcript', () => {
  const from = `cls-long-from-${stamp}`;
  const to = `cls-long-to-${stamp}`;
  test.beforeAll(() => {
    for (const a of [from, to]) {
      mkchat(a, null);
      mkchat(a, transcript('markdown'));
    }
  });
  test.afterAll(() => {
    for (const a of [from, to]) mkchat(a, null);
  });

  test('the arriving agent is wired, laid out and at its end', async ({ page }) => {
    const errors: string[] = [];
    page.on('pageerror', (e) => errors.push(String(e)));
    await page.goto(`/agents/${from}`);
    await settle(page);
    const boosted = page.waitForResponse((r) => r.url().includes(`/agents/${to}`) && r.request().headers()['hx-boosted'] === 'true');
    await page.evaluate((name) => {
      const a = document.createElement('a');
      a.href = '/agents/' + name;
      a.id = 'e2e-boost-link';
      a.textContent = 'go';
      document.querySelector('main')!.appendChild(a);
      (window as any).htmx.process(a);
      a.click();
    }, to);
    await boosted;
    await expect(page).toHaveURL(new RegExp(`/agents/${to}`));
    await expect(page.locator('#chat-form[data-agent="' + to + '"]')).toHaveCount(1);
    await settle(page);
    await expect(page.locator('#chat-form'), 'one composer').toHaveCount(1);
    await expect(page.locator('#chat-form[data-cg-chat]'), 'wired by start()').toHaveCount(1);
    await expect(page.locator('#chat-log [data-bubble]')).toHaveCount(CAP);
    await expect(page.locator('#chat-log [data-md-done]'), 'every bubble converted').toHaveCount(CAP);
    expect(await atEnd(page, '#chat-log'), 'at its end').toBe(true);
    expect(errors, 'no page error').toEqual([]);
  });
});

// --- THE MIRRORED (tmux) RENDERED VIEW, with a long Claude Code transcript. Its
// transcript is not in the document: it arrives over /mirror/view/ws from the
// host after load (mirror_view.go), as server-rendered markdown. One fake host,
// whose `main` session is a Claude Code pane over CAP records — the markdown
// variant's prompts and replies.
//
// 🔴 INVARIANT GUARDS, NOT REGRESSION TESTS. These read 0 on origin/main and 0
// with the agent-page fix: the defect above never reached this view (its
// transcript is drawn by WS ops into a page already laid out). They pin that
// zero for the changes coming to this view (client snapshots: PR #250), with a
// positive control per size so the zero is a measurement.
const rig = new MirrorRig(clsLongMirrorPort, 'e2e-clslong');
const view = () => `/mirror/view?host=${encodeURIComponent(rig.host)}&session=main`;

test.describe('a 200-message mirrored transcript', () => {
  let state: object = {};
  test.beforeAll(async ({ browser }) => {
    test.setTimeout(180_000);
    await rig.start();
    const body = transcript('markdown').map((m) => (m.role === 'user' ? cc.prompt(m.content) : cc.reply(m.content))).join('');
    await rig.startHost({ MUSTER_MIRROR_INTERACTIVE: '1', MUSTER_MIRROR_E2E_SESSIONS: 'main', MUSTER_MIRROR_E2E_CLAUDE: 'main=' + rig.transcript(body) });
    // Signed in to THIS server: an empty state, or the suite's default (the
    // shared server's cookie) comes along and /login is never drawn.
    const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
    const page = await ctx.newPage();
    await rig.login(page);
    await expect
      .poll(async () => (await (await page.request.get(rig.origin + '/ui/agents')).text()).includes('data-mirror-claude'), {
        message: `main reported as a Claude pane (muster-mirror log:\n${rig.log})`,
        timeout: 20_000,
      })
      .toBe(true);
    state = await ctx.storageState();
    await ctx.close();
  });
  test.afterAll(async () => {
    await rig.stop();
  });

  for (const vp of viewports) {
    for (const pr of profiles) {
      test(`${vp.name}, ${pr.name}: opening the rendered view shifts less than ${BAR}`, async ({ browser }) => {
        test.setTimeout(120_000);
        const { ctx, page } = await open(browser, vp.use, pr.p, view(), { origin: rig.origin, state });
        try {
          // The transcript arrives after load: wait for all of it, then settle.
          await expect(page.locator('#mv-log [data-bubble]'), 'the view drew the whole transcript').toHaveCount(CAP, { timeout: 30_000 });
          await settle(page);
          const r = await readCls(page);
          expect(await atEnd(page, '#mv-log'), 'the view opened at its end').toBe(true);
          console.log(`[cls-long mirror ${vp.name}, ${pr.name}] ${moved(r)}`);
          expect(moved(r), report('mirrored view', r)).toBeLessThan(BAR);
        } finally {
          await ctx.close();
        }
      });
    }

    // The positive control on THIS page: a block put above the log, unasked,
    // once the transcript has landed, MUST be measured — so the zeros above are
    // not a page the instrument cannot see.
    test(`${vp.name}: positive control: an unprovoked shift on the rendered view is measured`, async ({ browser }) => {
      const { ctx, page } = await open(browser, vp.use, null, view(), { origin: rig.origin, state });
      try {
        await expect(page.locator('#mv-log [data-bubble]')).toHaveCount(CAP, { timeout: 30_000 });
        await page.evaluate(() => {
          const l = document.getElementById('mv-log')!;
          const d = document.createElement('div');
          d.style.height = '160px';
          l.parentElement!.insertBefore(d, l);
        });
        await settle(page, 500);
        const r = await readCls(page);
        console.log(`[cls-long mirror ${vp.name}] positive control: ${report('injected', r)}`);
        expect(moved(r), report('injected shift', r)).toBeGreaterThan(BAR);
      } finally {
        await ctx.close();
      }
    });
  }
});
