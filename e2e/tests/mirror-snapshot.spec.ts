import fs from 'node:fs';
import { Page, WebSocketRoute } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { clsInit, readCls, settle } from '../lib/cls';
import { mirrorSnapPort, password } from '../lib/env';
import { cc, MirrorRig } from '../lib/mirrorhost';

// THE RENDERED VIEW'S SNAPSHOT (web/static/mirror-snap.js): a mirrored window's
// view is painted at once from this browser's copy of what it last drew, then
// resumes its socket from that copy's offset — against a REAL muster-server and
// a REAL muster-mirror built with `-tags e2emirror`, a per-window host whose
// three windows (work @1, @2, @3) each run Claude Code over a transcript this
// spec writes. @1's is long (200 messages).
//
// 🔴 "PAINTED FROM THE SNAPSHOT" IS PROVEN WITH THE SOCKET HELD. hold() routes
// the window's socket and keeps every server message from the page until
// release(): whatever the page shows meanwhile came from storage, because the
// server has not been heard. Each negative case (another user's record, an
// expired one, a purged one) runs beside a POSITIVE CONTROL through the same
// instrument — the same held socket, a record that MAY be painted — so a
// "nothing painted" cannot be the instrument seeing nothing.
//
// 🔴 A RESUME IS COUNTED ON THE HOST'S OWN LOG: `watching "work" window @1
// (resuming at <n>, …)` against `(from the start, …)`.
test.use({ viewport: { width: 390, height: 844 }, storageState: { cookies: [], origins: [] } });

const rig = new MirrorRig(mirrorSnapPort, 'e2e-snap');
const origin = rig.origin;
let trLong = '';
let trTwo = '';
let trThree = '';

const viewOf = (win: string) => `/mirror/view?host=${encodeURIComponent(rig.host)}&session=work&window=${encodeURIComponent(win)}`;
const lines = (re: RegExp) => rig.log.split('\n').filter((l) => re.test(l)).length;
const fromStart = (win: string) => lines(new RegExp(`watching "work" window ${win} \\(from the start,`));
const resuming = (win: string) => lines(new RegExp(`watching "work" window ${win} \\(resuming at \\d+,`));

// A long conversation: 100 prompts and 100 replies of uneven length, some with
// markdown. Invented text.
function longTranscript(): string {
  let out = '';
  for (let i = 0; i < 100; i++) {
    out += cc.prompt(`Question ${i}: what does step ${i} of the synthetic plan do?`);
    const body = Array.from({ length: 1 + (i % 5) }, (_, j) => `Step ${i}.${j} reads fixture file f${j}.txt and writes a summary line.`).join(' ');
    out += cc.reply(i % 3 === 0 ? `**Answer ${i}**\n\n- ${body}\n- done` : `Answer ${i}. ${body}`);
  }
  return out;
}

// --- the instruments --------------------------------------------------------

// hold routes the socket of window `win` (only that window's: the page's warm
// sockets for other windows are left alone) through to the server, and keeps
// what the server sends until release().
interface Hold {
  release: () => void;
  sockets: () => number;
}
async function hold(page: Page, win: string): Promise<Hold> {
  let held = true;
  const queue: { ws: WebSocketRoute; m: string | Buffer }[] = [];
  let n = 0;
  const want = `window=${encodeURIComponent(win)}`;
  await page.routeWebSocket(
    (u) => u.pathname === '/mirror/view/ws' && u.search.includes(want),
    (ws) => {
      n++;
      const server = ws.connectToServer();
      server.onMessage((m) => (held ? queue.push({ ws, m }) : ws.send(m)));
      ws.onMessage((m) => server.send(m));
    },
  );
  return {
    release: () => {
      held = false;
      for (const q of queue.splice(0)) q.ws.send(q.m);
    },
    sockets: () => n,
  };
}

// snaps is every record in the store, or null when there is no database.
async function snaps(page: Page): Promise<any[] | null> {
  return page.evaluate(async () => {
    const dbs = await indexedDB.databases();
    if (!dbs.some((d) => d.name === 'muster-snap')) return null;
    return await new Promise<any[]>((res, rej) => {
      const r = indexedDB.open('muster-snap');
      r.onsuccess = () => {
        const db = r.result;
        if (!db.objectStoreNames.contains('mirror')) {
          db.close();
          res([]);
          return;
        }
        // The read counter (readsInit) does not count this spec's own reads.
        const q = ((window as any).__probeGetAll || IDBObjectStore.prototype.getAll).call(db.transaction('mirror', 'readonly').objectStore('mirror'));
        q.onsuccess = () => {
          db.close();
          res(q.result);
        };
        q.onerror = () => {
          db.close();
          rej(q.error);
        };
      };
      r.onerror = () => rej(r.error);
    });
  });
}

const snapOf = async (page: Page, win: string) => ((await snaps(page)) || []).find((r) => r.window === win && r.session === 'work');

// putSnap writes rec into the store, and its meta record (the database must
// exist: a page that ran the store has made it). meta overrides fields of the
// meta written — a meta that disagrees with its record isolates the READ's
// own check from the load-time sweep, which reads meta only.
async function putSnap(page: Page, rec: any, meta: object = {}): Promise<void> {
  // From /tasks: a rendered view left open would save its own snapshot over
  // this one when it is left (pagehide).
  await page.goto(origin + '/tasks');
  const m = { k: rec.k, user: rec.user, build: rec.build, host: rec.host, session: rec.session, window: rec.window, at: rec.at, firstAt: rec.firstAt, bytes: 1, ...meta };
  await page.evaluate(
    ([rec, m]) =>
      new Promise<void>((res, rej) => {
        const r = indexedDB.open('muster-snap');
        r.onsuccess = () => {
          const db = r.result;
          const t = db.transaction(['mirror', 'meta'], 'readwrite');
          t.objectStore('mirror').put(rec);
          t.objectStore('meta').put(m);
          t.oncomplete = () => {
            db.close();
            res();
          };
          t.onerror = () => rej(t.error);
        };
        r.onerror = () => rej(r.error);
      }),
    [rec, m],
  );
}

// wipe deletes the store. Run from /tasks, a page that loads the store but
// never warms, so no warm write can land after it.
async function wipe(page: Page): Promise<void> {
  await page.goto(origin + '/tasks');
  await page.evaluate(
    () =>
      new Promise<void>((res) => {
        const r = indexedDB.deleteDatabase('muster-snap');
        r.onsuccess = r.onerror = r.onblocked = () => res();
      }),
  );
}

// The page's own identity, as the server rendered it on the store's tag.
const identity = (page: Page) =>
  page.evaluate(() => {
    const s = document.querySelector('script[data-snap-user]');
    return { user: s?.getAttribute('data-snap-user') || '', build: s?.getAttribute('data-snap-build') || '' };
  });

const keyOf = (user: string, build: string, win: string) => JSON.stringify([user, build, rig.host, 'work', win]);

// When the first bubble appeared, the first snapshot paint happened, and the
// "Updating…" chip was first shown — each performance.now() since navigation.
const stamps = `(() => {
  window.__t = { bubble: null, snapshot: null, chip: null };
  const T = window.__t;
  const mark = () => {
    const now = performance.now();
    if (T.bubble === null && document.querySelector('#mv-log [data-bubble]')) T.bubble = now;
    const log = document.getElementById('mv-log');
    if (T.snapshot === null && log && log.hasAttribute('data-snapshot')) T.snapshot = now;
    const chip = document.getElementById('mv-updating');
    if (T.chip === null && chip && !chip.classList.contains('invisible')) T.chip = now;
  };
  new MutationObserver(mark).observe(document, { subtree: true, childList: true, attributes: true });
})()`;
const times = (page: Page) => page.evaluate(() => (window as any).__t as { bubble: number | null; snapshot: number | null; chip: number | null });

// readsInit counts the page's reads of SNAPSHOT records (the 'mirror'
// store's get / getAll / openCursor) — never its keys (getAllKeys) or the
// 'meta' store. snaps() reads through the original getAll, uncounted.
const readsInit = `(() => {
  window.__reads = 0;
  const P = IDBObjectStore.prototype;
  window.__probeGetAll = P.getAll;
  for (const name of ['get', 'getAll', 'openCursor']) {
    const orig = P[name];
    P[name] = function (...a) { if (this.name === 'mirror') window.__reads++; return orig.apply(this, a); };
  }
})()`;

async function ready(page: Page): Promise<void> {
  await rig.login(page);
  await expect.poll(() => rig.log.includes('connected to '), { message: `the host connected (log:\n${rig.log})`, timeout: 20_000 }).toBe(true);
  await expect
    .poll(async () => ((await (await page.request.get(origin + '/ui/agents')).text()).match(/data-mirror-claude/g) || []).length, {
      message: `all three windows listed as Claude Code (log:\n${rig.log})`,
      timeout: 20_000,
    })
    .toBe(3);
}

// openLive opens window win's view, waits until it is live and its snapshot is
// stored, and returns the number of bubbles it drew.
async function openLive(page: Page, win: string): Promise<number> {
  await page.goto(origin + viewOf(win));
  await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 15_000 });
  await expect(page.locator('#mv-log [data-bubble]').first()).toBeVisible();
  const n = await page.locator('#mv-log [data-bubble]').count();
  await expect.poll(async () => (await snapOf(page, win))?.batches?.length ?? 0, { message: `window ${win}'s snapshot was stored` }).toBeGreaterThan(0);
  return n;
}

test.beforeAll(async () => {
  test.setTimeout(180_000);
  await rig.start();
  trLong = rig.transcript(longTranscript(), 'long');
  trTwo = rig.transcript(cc.prompt('the prompt of window TWO') + cc.reply('Window two answered.'), 'two');
  trThree = rig.transcript(cc.prompt('the prompt of window THREE') + cc.reply('Window three answered.'), 'three');
  const now = Math.floor(Date.now() / 1000);
  const windows = rig.transcript(
    [`work|@1|1|1|claude|${now - 60}|${trLong}`, `work|@2|2|0|claude|${now - 120}|${trTwo}`, `work|@3|3|0|claude|${now - 180}|${trThree}`].join('\n') + '\n',
    'windows',
  );
  await rig.startHost({ MUSTER_MIRROR_E2E_WINDOWS_FILE: windows, MUSTER_MIRROR_E2E_REFRESH: '300ms' });
});

test.afterAll(async () => {
  await rig.stop();
});

test('a second open paints the conversation from the snapshot before the server answers, then resumes from its offset', async ({ page }) => {
  await ready(page);
  await wipe(page);
  await page.addInitScript(stamps);

  // Cold: nothing stored, the host replays the transcript.
  const n = await openLive(page, '@1');
  const cold = (await times(page)).bubble!;
  expect(n, 'the long conversation drawn').toBe(200);
  const rec = await snapOf(page, '@1');
  expect(rec.offset, 'the snapshot holds the socket offset').toBeGreaterThan(0);
  expect(rec.gen, 'and its transcript').toBeTruthy();

  // Warm: the same window again, its socket HELD — nothing from the server.
  await page.goto(origin + '/agents');
  const resumedBefore = resuming('@1');
  const h = await hold(page, '@1');
  await page.goto(origin + viewOf('@1'));
  await expect(page.locator('#mv-log [data-bubble]'), 'every bubble painted with the socket held').toHaveCount(200);
  await expect(page.locator('#mv-log')).toHaveAttribute('data-snapshot', '');
  await expect(page.locator('#mv-status'), 'not claimed live').not.toHaveText('Live');
  const warm = (await times(page)).bubble!;
  // The view is scrolled to the end, as a live open is.
  const atEnd = await page.locator('#mv-log').evaluate((l) => l.scrollHeight - l.scrollTop - l.clientHeight < 80);
  expect(atEnd, 'the snapshot opens at the end of the conversation').toBe(true);

  h.release();
  await expect(page.locator('#mv-status')).toHaveText('Live');
  await expect(page.locator('#mv-log')).not.toHaveAttribute('data-snapshot', '');
  await expect.poll(() => resuming('@1'), { message: `the socket resumed from the snapshot's offset (log:\n${rig.log})` }).toBeGreaterThan(resumedBefore);
  await expect(page.locator('#mv-log [data-bubble]'), 'nothing drawn twice').toHaveCount(200);
  console.log(`time to first message on @1 (200 messages): cold ${cold.toFixed(0)} ms, from the snapshot ${warm.toFixed(0)} ms`);
});

test('a page painted from a snapshot converges to live: messages written since are drawn once, after it', async ({ page }) => {
  await ready(page);
  await page.goto(origin + '/agents');
  await openLive(page, '@2');
  await page.goto(origin + '/agents');
  // Two new messages while the page was away.
  fs.appendFileSync(trTwo, cc.prompt('a NEWER prompt for two') + cc.reply('A newer answer from two.'));
  const resumedBefore = resuming('@2');
  const h = await hold(page, '@2');
  await page.goto(origin + viewOf('@2'));
  const bubbles = page.locator('#mv-log [data-bubble]');
  await expect(bubbles, 'the snapshot: the two messages it held').toHaveCount(2);
  await expect(page.locator('#mv-log')).not.toContainText('NEWER');
  h.release();
  await expect(bubbles).toHaveCount(4);
  await expect(bubbles.nth(2)).toContainText('a NEWER prompt for two');
  await expect(bubbles.nth(3)).toContainText('A newer answer from two.');
  expect(resuming('@2'), 'resumed, not replayed from the start').toBeGreaterThan(resumedBefore);
  // And while live: one more.
  fs.appendFileSync(trTwo, cc.reply('Live after the resume.'));
  await expect(bubbles).toHaveCount(5);
  const dupes = await page.evaluate(() => {
    const seen: Record<string, number> = {};
    document.querySelectorAll('#mv-log [id]').forEach((e) => (seen[e.id] = (seen[e.id] || 0) + 1));
    return Object.keys(seen).filter((k) => seen[k] > 1);
  });
  expect(dupes, 'no element id is drawn twice').toEqual([]);
});

// An emulated phone: 350 ms of round trip and 9 / 1.5 Mbps on every request
// (CDP), 4× slower CPU, and each socket message of window `win` held 350 ms
// more — the host relay's leg, which CDP's emulation may not reach.
async function phone(page: Page, win: string): Promise<void> {
  const cdp = await page.context().newCDPSession(page);
  await cdp.send('Network.enable');
  await cdp.send('Network.emulateNetworkConditions', { offline: false, latency: 350, downloadThroughput: (9e6 / 8) | 0, uploadThroughput: (1.5e6 / 8) | 0 });
  await cdp.send('Emulation.setCPUThrottlingRate', { rate: 4 });
  const want = `window=${encodeURIComponent(win)}`;
  await page.routeWebSocket(
    (u) => u.pathname === '/mirror/view/ws' && u.search.includes(want),
    (ws) => {
      const server = ws.connectToServer();
      server.onMessage((m) => setTimeout(() => ws.send(m), 350));
      ws.onMessage((m) => server.send(m));
    },
  );
}

test('on an emulated phone the second open of a long conversation is painted from the snapshot (time to first message logged)', async ({ page }) => {
  test.setTimeout(120_000);
  await ready(page);
  await wipe(page);
  await page.addInitScript(stamps);
  await phone(page, '@1');
  await page.goto(origin + viewOf('@1'));
  await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 30_000 });
  const cold = (await times(page)).bubble!;
  await expect.poll(async () => (await snapOf(page, '@1'))?.batches?.length ?? 0, { timeout: 15_000 }).toBeGreaterThan(0);
  await page.goto(origin + '/agents');
  await page.goto(origin + viewOf('@1'));
  await expect(page.locator('#mv-log [data-bubble]').first()).toBeVisible({ timeout: 30_000 });
  const t = await times(page);
  const warm = t.bubble!;
  expect(t.snapshot, 'the second open was painted from the snapshot').not.toBeNull();
  await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 30_000 });
  console.log(`emulated phone, time to first message on @1: cold ${cold.toFixed(0)} ms, from the snapshot ${warm.toFixed(0)} ms`);
  // ⚠ NO WALL-CLOCK ASSERTION: on a shared host load inflates both numbers
  // unevenly, so the comparison is a measurement (logged above), not a gate.
});

for (const size of [
  { name: '390px', width: 390, height: 844 },
  { name: 'desktop', width: 1280, height: 800 },
]) {
  test(`opening a long conversation from its snapshot shifts no layout (CLS < 0.01) at ${size.name}`, async ({ page }) => {
    await page.setViewportSize({ width: size.width, height: size.height });
    await ready(page);
    await wipe(page);
    await page.addInitScript(clsInit);
    await page.addInitScript(stamps);
    // For the record: the same open with nothing stored (the path main has).
    await openLive(page, '@1');
    await settle(page);
    const coldCls = await readCls(page);
    console.log(`CLS cold (no snapshot) at ${size.name}: ${coldCls.total} ${JSON.stringify(coldCls.shifts)}`);
    await page.goto(origin + '/agents');
    await page.goto(origin + viewOf('@1'));
    await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 15_000 });
    await settle(page);
    const t = await times(page);
    expect(t.snapshot, 'the page was painted from the snapshot').not.toBeNull();
    const cls = await readCls(page);
    console.log(`CLS from the snapshot at ${size.name}: ${cls.total} ${JSON.stringify(cls.shifts)}`);
    expect(cls.unsupported).toBeUndefined();
    expect(cls.total, JSON.stringify(cls.shifts)).toBeLessThan(0.01);
    await expect(page.locator('#mv-log [data-bubble]')).toHaveCount(200);
    // POSITIVE CONTROL: the same instrument reads a shift the page did not ask
    // for (the header pushed down by script, no input).
    await page.evaluate(() => {
      const d = document.createElement('div');
      d.style.height = '120px';
      document.querySelector('header')!.before(d);
    });
    await expect.poll(async () => (await readCls(page)).total, { message: 'the instrument can read a shift' }).toBeGreaterThan(0.01);
  });
}

test("a record that is not this user's is never painted, and is deleted", async ({ page }) => {
  await ready(page);
  await page.goto(origin + '/agents');
  await openLive(page, '@3');
  const real = await snapOf(page, '@3');
  const { user, build } = await identity(page);
  expect(user, 'the page names its user').toMatch(/^[1-9]\d*$/);
  const sentinel = (text: string) => [
    { type: 'batch', reset: true, gen: real.gen, offset: real.offset, ops: [{ op: 'append', html: `<div data-bubble="user">${text}</div>` }] },
  ];
  const rec = (text: string, recUser: string, keyUser: string) => ({
    ...real,
    k: keyOf(keyUser, build, '@3'),
    user: recUser,
    at: Date.now(),
    batches: sentinel(text),
  });

  // POSITIVE CONTROL: this user's own record, through the same held socket.
  await putSnap(page, rec('OWN-SENTINEL', user, user));
  await page.goto(origin + '/agents');
  let h = await hold(page, '@3');
  await page.goto(origin + viewOf('@3'));
  await expect(page.locator('#mv-log'), 'the instrument sees a painted record').toContainText('OWN-SENTINEL');
  h.release();
  await expect(page.locator('#mv-status')).toHaveText('Live');
  await page.unrouteAll();

  // Another user's record under THIS user's key (a tampered or colliding key),
  // and one under the other user's own key.
  const other = String(Number(user) + 1000);
  await putSnap(page, rec('FOREIGN-SAME-KEY', other, user));
  await putSnap(page, rec('FOREIGN-OWN-KEY', other, other));
  await page.goto(origin + '/agents');
  h = await hold(page, '@3');
  await page.goto(origin + viewOf('@3'));
  // Given longer than the control needed to paint.
  await page.waitForTimeout(1500);
  await expect(page.locator('#mv-log')).not.toContainText('FOREIGN');
  await expect(page.locator('#mv-log [data-bubble]'), 'nothing painted').toHaveCount(0);
  const left = (await snaps(page)) || [];
  expect(left.filter((r) => r.user === other), "the other user's records were deleted").toEqual([]);
  h.release();
  await expect(page.locator('#mv-log [data-bubble]').first()).toContainText('window THREE');
});

test('a snapshot past its TTL is ignored and deleted; one inside it is painted', async ({ page }) => {
  await ready(page);
  await page.goto(origin + '/agents');
  await openLive(page, '@3');
  const real = await snapOf(page, '@3');
  const aged = (min: number, text: string) => ({
    ...real,
    at: Date.now() - min * 60_000,
    batches: [{ type: 'batch', reset: true, gen: real.gen, offset: real.offset, ops: [{ op: 'append', html: `<div data-bubble="user">${text}</div>` }] }],
  });
  for (const c of [
    { min: 4.5, text: 'AGED-4.5-MIN', painted: true },
    { min: 5.5, text: 'AGED-5.5-MIN', painted: false },
    { min: 30, text: 'AGED-30-MIN', painted: false },
  ]) {
    await putSnap(page, aged(c.min, c.text));
    await page.goto(origin + '/agents');
    const h = await hold(page, '@3');
    await page.goto(origin + viewOf('@3'));
    if (c.painted) {
      await expect(page.locator('#mv-log'), `${c.min} min old: painted`).toContainText(c.text);
    } else {
      await page.waitForTimeout(1500);
      await expect(page.locator('#mv-log [data-bubble]'), `${c.min} min old: not painted`).toHaveCount(0);
      expect(await snapOf(page, '@3'), `${c.min} min old: deleted`).toBeUndefined();
    }
    h.release();
    await expect(page.locator('#mv-status')).toHaveText('Live');
    await page.unrouteAll();
    // The live page stores a fresh one; the next case overwrites it.
    await expect.poll(async () => (await snapOf(page, '@3'))?.batches?.length ?? 0).toBeGreaterThan(0);
  }
});

test('the TTL is checked on the read itself, not only by the load-time sweep', async ({ page }) => {
  // 🔴 INVARIANT GUARD: the read checked the TTL before this round too. The
  // record is expired but its META is fresh, so the sweep (which reads meta
  // only) keeps it, and only the read's own check can refuse it.
  await ready(page);
  await page.goto(origin + '/agents');
  await openLive(page, '@3');
  const real = await snapOf(page, '@3');
  const batches = (text: string) => [{ type: 'batch', reset: true, gen: real.gen, offset: real.offset, ops: [{ op: 'append', html: `<div data-bubble="user">${text}</div>` }] }];
  for (const c of [
    { at: Date.now() - 60_000, text: 'READ-FRESH', painted: true },
    { at: Date.now() - 6 * 60_000, text: 'READ-EXPIRED', painted: false },
  ]) {
    await putSnap(page, { ...real, at: c.at, batches: batches(c.text) }, { at: Date.now() });
    await page.goto(origin + '/agents');
    const h = await hold(page, '@3');
    await page.goto(origin + viewOf('@3'));
    if (c.painted) await expect(page.locator('#mv-log'), 'POSITIVE CONTROL: painted').toContainText(c.text);
    else {
      await page.waitForTimeout(1500);
      await expect(page.locator('#mv-log [data-bubble]'), 'expired on read: not painted').toHaveCount(0);
    }
    h.release();
    await expect(page.locator('#mv-status')).toHaveText('Live');
    await page.unrouteAll();
  }
});

test('a snapshot is aged from its last FULL read: a resume keeps that age, and past 30 minutes it is not painted', async ({ page }) => {
  await ready(page);
  await page.goto(origin + '/agents');
  await openLive(page, '@2');
  const first = await snapOf(page, '@2');
  expect(typeof first.firstAt, 'the snapshot records when it was read in full').toBe('number');
  // Opened again: painted, resumed (not read in full), saved again.
  await page.goto(origin + '/agents');
  await page.goto(origin + viewOf('@2'));
  await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 15_000 });
  fs.appendFileSync(trTwo, cc.reply('Two, once more.'));
  await expect(page.locator('#mv-log')).toContainText('Two, once more.');
  await expect.poll(async () => (await snapOf(page, '@2'))?.at ?? 0, { message: 'saved again' }).toBeGreaterThan(first.at);
  expect((await snapOf(page, '@2')).firstAt, 'a resume does not make the content younger').toBe(first.firstAt);

  const real = await snapOf(page, '@2');
  const batches = (text: string) => [{ type: 'batch', reset: true, gen: real.gen, offset: real.offset, ops: [{ op: 'append', html: `<div data-bubble="user">${text}</div>` }] }];
  for (const c of [
    { min: 29, text: 'FULL-29-MIN', painted: true, meta: {} },
    { min: 31, text: 'FULL-31-MIN', painted: false, meta: {} },
    // The meta says young: only the read's own check refuses it.
    { min: 31, text: 'FULL-31-MIN-READ', painted: false, meta: { firstAt: Date.now() } },
  ]) {
    await putSnap(page, { ...real, at: Date.now(), firstAt: Date.now() - c.min * 60_000, batches: batches(c.text) }, c.meta);
    await page.goto(origin + '/agents');
    const starts = fromStart('@2');
    const h = await hold(page, '@2');
    await page.goto(origin + viewOf('@2'));
    if (c.painted) await expect(page.locator('#mv-log'), `${c.min} min since the full read: painted`).toContainText(c.text);
    else {
      await page.waitForTimeout(1500);
      await expect(page.locator('#mv-log [data-bubble]'), `${c.text}: not painted`).toHaveCount(0);
    }
    h.release();
    await expect(page.locator('#mv-status')).toHaveText('Live');
    if (!c.painted) expect(fromStart('@2'), `${c.text}: read from the start again`).toBeGreaterThan(starts);
    await page.unrouteAll();
    await expect.poll(async () => (await snapOf(page, '@2'))?.batches?.length ?? 0).toBeGreaterThan(0);
  }
});

test('a save reads no snapshot back: eviction works from keys and meta', async ({ page }) => {
  await ready(page);
  await page.goto(origin + '/agents');
  // Two other windows' snapshots in the store, so there is something to read.
  await openLive(page, '@3');
  await openLive(page, '@1');
  await page.goto(origin + '/agents');
  await page.addInitScript(readsInit);
  await page.goto(origin + viewOf('@2'));
  await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 15_000 });
  // The open's own read of @2 and the page's warm pass (1.5 s after load) are
  // done; count from here.
  await page.waitForTimeout(3500);
  await page.evaluate(() => ((window as any).__reads = 0));
  const before = (await snapOf(page, '@2'))?.at ?? 0;
  fs.appendFileSync(trTwo, cc.reply('A save to count.'));
  await expect(page.locator('#mv-log')).toContainText('A save to count.');
  await expect.poll(async () => (await snapOf(page, '@2'))?.at ?? 0, { message: 'the save landed' }).toBeGreaterThan(before);
  await page.waitForTimeout(300);
  expect(await page.evaluate(() => (window as any).__reads), 'snapshot records read by a save').toBe(0);
  // POSITIVE CONTROL: the counter sees a read of the store.
  await page.evaluate(
    () =>
      new Promise<void>((res) => {
        const r = indexedDB.open('muster-snap');
        r.onsuccess = () => {
          const q = r.result.transaction('mirror', 'readonly').objectStore('mirror').getAll();
          q.onsuccess = () => {
            r.result.close();
            res();
          };
        };
      }),
  );
  expect(await page.evaluate(() => (window as any).__reads), 'the counter counts').toBe(1);
});

test('a sign-out in another tab turns the store off in this one: it keeps streaming and stores nothing', async ({ page, context }) => {
  await ready(page);
  await page.goto(origin + '/agents');
  await openLive(page, '@2');
  const other = await context.newPage();
  await other.goto(origin + '/account');
  await Promise.all([other.waitForURL(/\/login$/), other.getByRole('button', { name: 'Sign out', exact: true }).click()]);
  await expect.poll(() => snaps(other), { message: 'the sign-out deleted the database' }).toBeNull();
  // This tab is still on its window: new messages arrive and are drawn…
  fs.appendFileSync(trTwo, cc.reply('After the sign-out elsewhere.'));
  await expect(page.locator('#mv-log')).toContainText('After the sign-out elsewhere.');
  // …and its save (800 ms after a batch) writes nothing.
  await page.waitForTimeout(2000);
  expect(await snaps(other), 'no database re-created by the tab that stayed').toBeNull();
  expect(await snaps(page)).toBeNull();
  await other.close();
});

test('a resume refused before its first batch takes the snapshot off the page and out of storage', async ({ page }) => {
  await ready(page);
  await page.goto(origin + '/agents');
  await openLive(page, '@3');
  const real = await snapOf(page, '@3');
  await putSnap(page, {
    ...real,
    at: Date.now(),
    batches: [{ type: 'batch', reset: true, gen: real.gen, offset: real.offset, ops: [{ op: 'append', html: '<div data-bubble="user">REFUSED-SENTINEL</div>' }] }],
  });
  await page.goto(origin + '/agents');
  // The window's FIRST socket is closed without a word once the snapshot is
  // on screen (what a 401 or a 404 on the upgrade looks like to the page);
  // every later one goes through.
  let n = 0;
  let refuse: () => void = () => {};
  await page.routeWebSocket(
    (u) => u.pathname === '/mirror/view/ws' && u.search.includes('window=%403'),
    (ws) => {
      if (n++ === 0) {
        refuse = () => void ws.close({ code: 1008, reason: 'refused' });
        return;
      }
      const server = ws.connectToServer();
      server.onMessage((m) => ws.send(m));
      ws.onMessage((m) => server.send(m));
    },
  );
  const startsBefore = fromStart('@3');
  await page.goto(origin + viewOf('@3'));
  await expect(page.locator('#mv-log'), 'POSITIVE CONTROL: the snapshot was painted').toContainText('REFUSED-SENTINEL');
  refuse();
  await expect(page.locator('#mv-log'), 'taken off the page').not.toContainText('REFUSED-SENTINEL');
  await expect(page.locator('#mv-log [data-bubble]').first(), 'the next socket drew the window afresh').toContainText('window THREE');
  await expect(page.locator('#mv-status')).toHaveText('Live');
  expect(fromStart('@3'), 'from the start, not resumed from the refused snapshot').toBeGreaterThan(startsBefore);
  const after = await snapOf(page, '@3');
  expect(JSON.stringify(after?.batches ?? []), 'the refused snapshot is not stored').not.toContain('REFUSED-SENTINEL');
});

test('signing out deletes every snapshot in the browser', async ({ page }) => {
  await ready(page);
  await page.goto(origin + '/agents');
  await openLive(page, '@3');
  expect((await snaps(page))?.length, 'POSITIVE CONTROL: there is a snapshot to delete').toBeGreaterThan(0);

  await page.goto(origin + '/account');
  await Promise.all([page.waitForURL(/\/login$/), page.getByRole('button', { name: 'Sign out', exact: true }).click()]);
  await expect.poll(() => snaps(page), { message: 'no snapshot database after the sign-out' }).toBeNull();

  // Signed in again: the window opens cold.
  await rig.login(page);
  await page.goto(origin + '/agents');
  if (new URL(page.url()).pathname === '/login') {
    await page.fill('#muster-password', password);
    await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
    await page.goto(origin + '/agents');
  }
  const h = await hold(page, '@3');
  await page.goto(origin + viewOf('@3'));
  await page.waitForTimeout(1500);
  await expect(page.locator('#mv-log [data-bubble]'), 'nothing painted after signing out').toHaveCount(0);
  h.release();
  await expect(page.locator('#mv-log [data-bubble]').first()).toContainText('window THREE');
});

test('warming from the Agents list stores the recent windows without opening their pages or moving Recent', async ({ page }) => {
  await ready(page);
  // Recent becomes @2, @3 (newest first)…
  await page.goto(origin + viewOf('@3'));
  await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 15_000 });
  await page.goto(origin + viewOf('@2'));
  await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 15_000 });
  const recent = async () => {
    const html = await (await page.request.get(origin + '/ui/agents')).text();
    return [...html.matchAll(/<a href="([^"]+)"[^>]*data-recent-entry="mirror"/g)].map((m) => decodeURIComponent(m[1].replace(/&amp;/g, '&')).replace(/.*window=/, ''));
  };
  // …and nothing is stored.
  await wipe(page);
  const before = await recent();
  expect(before.slice(0, 2), 'Recent before').toEqual(['@2', '@3']);

  const docs: string[] = [];
  page.on('request', (r) => {
    if (r.resourceType() === 'document') docs.push(new URL(r.url()).pathname);
  });
  const startsBefore = { two: fromStart('@2'), three: fromStart('@3') };
  await page.goto(origin + '/agents');
  await expect
    .poll(async () => ((await snaps(page)) || []).map((r) => r.window).sort().join(','), { message: 'the recent windows were warmed', timeout: 20_000 })
    .toContain('@2,@3');
  expect(fromStart('@2') > startsBefore.two && fromStart('@3') > startsBefore.three, 'each warmed through its own transcript socket').toBe(true);
  expect(docs, 'no page but the Agents list was loaded').toEqual(['/agents']);
  expect((await recent()).slice(0, 2), 'Recent did not move').toEqual(before.slice(0, 2));

  // And the warmed window opens from it.
  const h = await hold(page, '@3');
  await page.goto(origin + viewOf('@3'));
  await expect(page.locator('#mv-log'), 'painted from the warmed snapshot with the socket held').toContainText('window THREE');
  h.release();
  await expect(page.locator('#mv-status')).toHaveText('Live');
});

test("the session switcher's Rendered-view entries feed warming, at most three a pass", async ({ page }) => {
  await ready(page);
  // Visits, so the switcher's Recent row lists the other windows.
  for (const w of ['@3', '@2']) {
    await page.goto(origin + viewOf(w));
    await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 15_000 });
  }
  // Nothing stored: on a rendered view the only other source is this
  // browser's own snapshots, so whatever is warmed came from the page's links.
  await wipe(page);
  const warmSockets: string[] = [];
  page.on('websocket', (ws) => {
    if (ws.url().includes('warm=1')) warmSockets.push(new URL(ws.url()).searchParams.get('window') || '');
  });
  await page.goto(origin + viewOf('@1'));
  await expect(page.locator('#mv-status')).toHaveText('Live', { timeout: 15_000 });
  const linked = await page.evaluate(() =>
    [...document.querySelectorAll('[data-switcher-entry] , a[data-snap-warm]')]
      .filter((a) => a.hasAttribute('data-snap-warm'))
      .map((a) => new URL(a.getAttribute('href')!, location.href).searchParams.get('window')),
  );
  expect(await page.locator('a[data-snap-warm][data-switcher-entry]').count(), 'POSITIVE CONTROL: the switcher draws warmable entries').toBeGreaterThan(0);
  await expect
    .poll(async () => ((await snaps(page)) || []).filter((r) => r.window !== '@1').length, { message: 'a switcher entry was warmed', timeout: 20_000 })
    .toBeGreaterThan(0);
  await page.waitForTimeout(3000);
  const warmed = ((await snaps(page)) || []).map((r) => r.window).filter((w) => w !== '@1');
  for (const w of warmed) expect(linked, `${w} was warmed from a data-snap-warm link`).toContain(w);
  expect(new Set(warmSockets).size, `at most three windows warmed (${warmSockets.join(',')})`).toBeLessThanOrEqual(3);
  expect(warmSockets, 'the window on screen is never warmed').not.toContain('@1');
});

test('the "Updating…" chip shows only after a second of catching up, in room it always had', async ({ page }) => {
  await ready(page);
  await page.goto(origin + '/agents');
  await openLive(page, '@1');
  await page.goto(origin + '/agents');
  await page.addInitScript(clsInit);
  await page.addInitScript(stamps);
  const h = await hold(page, '@1');
  await page.goto(origin + viewOf('@1'));
  const chip = page.locator('#mv-updating');
  await expect(page.locator('#mv-log')).toHaveAttribute('data-snapshot', '');
  const box0 = await chip.boundingBox();
  expect(await chip.evaluate((e) => getComputedStyle(e).visibility), 'hidden at first').toBe('hidden');
  expect(box0 && box0.width > 0 && box0.height > 0, 'its room is taken while hidden').toBe(true);
  await expect(chip).toBeVisible({ timeout: 5000 });
  const t = await times(page);
  expect(t.chip! - t.snapshot!, 'shown a second after the paint, not before').toBeGreaterThanOrEqual(950);
  expect(await chip.boundingBox(), 'shown where its room was').toEqual(box0);
  const cls = await readCls(page);
  expect(cls.total, `no shift while catching up: ${JSON.stringify(cls.shifts)}`).toBeLessThan(0.01);
  h.release();
  await expect(chip).toBeHidden();
  expect(await chip.boundingBox(), 'hidden where it was').toEqual(box0);
});
