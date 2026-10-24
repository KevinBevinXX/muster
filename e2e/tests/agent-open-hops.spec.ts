import { Page } from '@playwright/test';
import { test, expect } from '../lib/fixtures';
import { authState, baseURL } from '../lib/env';
import { mkchat } from '../lib/mirrorhost';
import { agentLink, agentPageRe } from '../lib/agent-link';

// OPENING AN AGENT COSTS ONE DOCUMENT AND ONE TRANSCRIPT, on the shared server,
// phone-sized. Operator ask: "cache things better so that I don't have to wait
// for full reloads … every time I open an agent session."
//
// Two round trips were spent on every open, and neither bought anything:
//
//   1. Every in-app link named the bare /agents/{name}, which answers 303 →
//      /agents/{name}?session=<id>. Measured on an emulated phone (350 ms RTT,
//      4x CPU) the first bubble landed at 828/1203 ms through the bare link and
//      476/488 ms through the session URL. Links now name the session
//      (ui.AgentSessionHref); the bare URL still redirects, for bookmarks.
//   2. The page then re-downloaded its whole transcript (~72 KB at 200
//      messages) the moment its event stream connected (chatLogTrigger's
//      htmx:sseOpen arm). That refetch now sends the version it was rendered
//      with (ui.ChatLogBody) and gets a 204 unless something changed.
//
// The agent is this spec's own (e2e/mkchat), with 200 synthetic messages —
// chatHistoryCap, the most the page draws.

const name = 'e2e-hops-' + Date.now().toString(36);
const messages = Array.from({ length: 200 }, (_, i) => ({
  role: i % 2 ? 'assistant' : 'user',
  content: `Synthetic message ${i}: the quick brown fox jumps over the lazy dog, and then reports back.`,
}));
let agentID = '';

test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

test.beforeAll(() => mkchat(name, messages));

async function pin(page: Page, on: boolean) {
  const r = await page.request.post(on ? '/agents/pins' : '/agents/pins/delete', {
    form: { kind: 'agent', agent_id: agentID },
    headers: { Origin: baseURL, 'HX-Request': 'true' },
  });
  expect(r.status(), await r.text()).toBe(200);
}

test.afterAll(async ({ browser }) => {
  if (agentID) {
    const page = await browser.newPage({ baseURL, storageState: authState });
    await pin(page, false).catch(() => undefined);
    await page.close();
  }
  mkchat(name, null);
});

// The main frame's document requests, from now on (a 303 is two of them:
// Playwright reports the redirected request as a request of its own).
function documents(page: Page): string[] {
  const out: string[] = [];
  page.on('request', (r) => {
    if (r.resourceType() === 'document' && r.frame() === page.mainFrame()) out.push(new URL(r.url()).pathname + new URL(r.url()).search);
  });
  return out;
}

// The bare URL still redirects — the link must land exactly there.
async function canonical(page: Page): Promise<string> {
  const r = await page.request.get('/agents/' + name, { maxRedirects: 0 });
  expect(r.status(), 'the bare URL still redirects').toBe(303);
  const loc = r.headers()['location'];
  expect(loc).toMatch(agentPageRe(name));
  return loc;
}

async function openList(page: Page) {
  await page.goto('/agents');
  await expect(page.locator('#agents-list > article').first()).toBeVisible();
}

// 🔴 A REGRESSION TEST: on origin/main each tap below made TWO document
// requests (/agents/<name>, then its 303's target).
test('a card, a pinned chip and a Recent entry each open the page in ONE document request', async ({ page }) => {
  const canon = await canonical(page);
  // A list link is the 303 target plus the from=list marker (ui.AgentListHref),
  // which only moves a STALE link to the latest session.
  const want = canon + '&from=list';
  // A visit puts the agent in the Recent strip.
  await page.goto('/agents/' + name);
  await page.waitForURL((u) => u.pathname + u.search === canon);

  await openList(page);
  const card = page.locator(`#agents-list > article[id^="agent-"]:has(${agentLink(name)})`);
  await expect(card).toHaveCount(1);
  agentID = (await card.getAttribute('id'))!.replace('agent-', '');
  let docs = documents(page);
  await Promise.all([page.waitForURL((u) => u.pathname === '/agents/' + name), card.locator('a[data-card-slot="body"]').tap()]);
  await page.waitForLoadState('domcontentloaded');
  expect(docs, 'the card: one document, the session URL').toEqual([want]);

  await openList(page);
  const recent = page.locator('#agents-recent a[data-recent-entry="agent"]').and(page.locator(agentLink(name)));
  await expect(recent).toHaveCount(1);
  docs = documents(page);
  await Promise.all([page.waitForURL((u) => u.pathname === '/agents/' + name), recent.tap()]);
  await page.waitForLoadState('domcontentloaded');
  expect(docs, 'the Recent entry: one document, the session URL').toEqual([want]);

  await pin(page, true);
  try {
    await openList(page);
    const chip = page.locator('#agents-list a[data-pinned-chip-link]').and(page.locator(agentLink(name)));
    await expect(chip).toHaveCount(1);
    docs = documents(page);
    await Promise.all([page.waitForURL((u) => u.pathname === '/agents/' + name), chip.tap()]);
    await page.waitForLoadState('domcontentloaded');
    expect(docs, 'the pinned chip: one document, the session URL').toEqual([want]);
  } finally {
    await pin(page, false);
  }
});

// chatLogResponses records every response to this agent's chat-log partial.
function chatLogResponses(page: Page): number[] {
  const out: number[] = [];
  page.on('response', (r) => {
    if (new URL(r.url()).pathname === `/ui/agents/${name}/chat-log`) out.push(r.status());
  });
  return out;
}

// 🔴 A REGRESSION TEST: on origin/main the stream's open re-fetched the whole
// transcript (one 200, ~72 KB). Now the same refetch is a 204.
test('opening the page does not download its transcript a second time', async ({ page }) => {
  const want = await canonical(page);
  const statuses = chatLogResponses(page);
  let streams = 0;
  page.on('request', (r) => {
    if (new URL(r.url()).pathname === '/events') streams++;
  });
  await page.goto(want);
  await expect(page.locator('#chat-log')).toContainText('Synthetic message 199');
  // The refetch still happens (it is what catches a missed broadcast) …
  await expect.poll(() => statuses.length, { message: 'control: the stream-open refetch ran' }).toBeGreaterThan(0);
  await page.waitForTimeout(1000); // … and nothing else follows it.
  expect(statuses.filter((s) => s === 200), `transcript downloads after the render (statuses ${statuses})`).toHaveLength(0);
  expect(statuses.every((s) => s === 204), `every refetch was answered "unchanged" (statuses ${statuses})`).toBe(true);
  await expect(page.locator('#chat-log')).toContainText('Synthetic message 0');
  // 🔴 ONE /events stream, not two (red at 9e0125c): the second was a raw
  // EventSource listening for "agent.stream", which nothing publishes.
  expect(streams, 'event streams the page opened').toBe(1);
});

// 🔴 A REGRESSION TEST (red at 9e0125c): a WS turn draws bubbles the server
// never stored when it fails — the user's message and the "[error]" — and the
// page's version still matches the server's, so the next refetch was a 204 and
// the made-up bubbles stayed for ever. With local nodes present the refetch
// asks for the full transcript, which redraws the log without them.
test('a failed turn’s local bubbles are replaced by the server’s transcript on the next refetch', async ({ page }) => {
  const want = await canonical(page);
  // The page's socket talks to this stand-in, which fails every turn and stores
  // nothing on the server.
  await page.routeWebSocket(new RegExp(`/agents/${name}/ws`), (ws) => {
    ws.onMessage(() => ws.send(JSON.stringify({ type: 'error', content: 'synthetic turn failure' })));
  });
  const statuses = chatLogResponses(page);
  await page.goto(want);
  await expect.poll(() => statuses.length, { message: 'control: the stream-open refetch ran' }).toBeGreaterThan(0);
  const before = statuses.length;
  const made = 'Synthetic message the server never stored';
  await page.locator('#chat-input').fill(made);
  await page.locator('#chat-send').click();
  const log = page.locator('#chat-log');
  await expect(log, 'control: the failed turn drew its bubbles').toContainText('synthetic turn failure');
  await expect(log).toContainText(made);
  // The stream (re)opens: the refetch htmx:sseOpen drives.
  await page.evaluate(() => document.body.dispatchEvent(new Event('htmx:sseOpen')));
  await expect.poll(() => statuses.length).toBeGreaterThan(before);
  expect(statuses[statuses.length - 1], `the refetch after a failed turn (statuses ${statuses})`).toBe(200);
  await expect(log).not.toContainText(made);
  await expect(log).not.toContainText('synthetic turn failure');
  await expect(log).toContainText('Synthetic message 199');
});

// The positive control for the counter above, and the convergence it must not
// lose: a message stored between the render and the stream connecting is
// fetched — exactly once — and shown.
test('a message stored before the stream connects is fetched once and shown', async ({ page }) => {
  const want = await canonical(page);
  let release!: () => void;
  const held = new Promise<void>((r) => (release = r));
  let heldStreams = 0;
  await page.route(/\/events(\?|$)/, async (route) => {
    heldStreams++;
    await held;
    await route.continue();
  });
  const statuses = chatLogResponses(page);
  await page.goto(want);
  await expect.poll(() => heldStreams, { message: 'control: the event stream was held' }).toBeGreaterThan(0);
  const text = 'Synthetic late reply, stored while the stream was held';
  mkchat(name, [{ role: 'assistant', content: text }], { MUSTER_E2E_APPEND: '1' });
  await expect(page.locator('#chat-log'), 'the page has not seen it yet').not.toContainText(text);
  release();
  await expect(page.locator('#chat-log')).toContainText(text, { timeout: 15_000 });
  await page.waitForTimeout(1000);
  expect(statuses.filter((s) => s === 200), `transcript downloads (statuses ${statuses})`).toHaveLength(1);
});
