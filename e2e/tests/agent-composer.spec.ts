import { request as pwRequest, Page } from '@playwright/test';
import { test, expect, expectAgentsDestroyed } from '../lib/fixtures';
import { agentComposerPort, hookToken, password, versionTerm } from '../lib/env';
import { startServer, Running } from '../lib/server';

// A DISPATCHED AGENT'S TWO VIEWS, in a real browser against a REAL muster-server
// (the e2eterm build: the noop driver, whose terminal is played by
// cmd/muster-server/e2eterm.go). Its card opens the RENDERED view (its chat) by
// default; a claude-code agent has the same Rendered | Terminal switch a
// mirrored Claude Code window has, on both pages; and both views have a message
// box — the Terminal's is the shared composer, posting to the agent's send
// route.
//
// ⚠ WHAT THIS CANNOT SHOW: a real agent answering. The noop driver starts no
// pod, so a turn handed to the gateway fails — and that failure, stored in the
// conversation where the reply would be, is what these tests read to know the
// message went all the way to the gateway. Delivery into a real pod is not
// covered by any test here.
//
// Each test runs at a phone's size (390×844, touch) and at a desktop's.

test.use({ storageState: { cookies: [], origins: [] } });

const origin = `http://localhost:${agentComposerPort}`;
let server: Running | undefined;

test.beforeAll(async () => {
  server = await startServer(versionTerm, agentComposerPort, {
    MUSTER_AGENT_PROVISIONER: 'noop',
    MUSTER_AGENT_IMAGE_REPO: 'registry.example.test/runtime',
    MUSTER_AGENT_API_URL: 'http://muster.example.test:8105',
    MUSTER_AGENT_KINDS: 'gateway,claude-code',
    MUSTER_AGENT_CC_IMAGE: 'ghcr.io/example-org/claude-code-agent:e2e',
    MUSTER_AGENT_CC_ACCOUNTS: 'alpha',
    MUSTER_AGENT_CC_TOKEN_ALPHA: 'sk-ant-oat01-FAKE-e2e-composer-alpha',
    // A chat wire, so an agent CAN be sent a message. Nothing listens behind it
    // (see the note above): the sentinel and the bundle are the three values a
    // named runtime is refused at boot without, and none is ever used here.
    MUSTER_AGENT_GATEWAY: 'hooks-sha256',
    MUSTER_AGENT_GATEWAY_MODEL: 'e2e-sentinel',
    MUSTER_AGENT_RUNTIME_CONFIG: '{}',
    MUSTER_AGENT_RUNTIME_INSTALL: 'true',
  });
});
// 🔴 THIS SPEC SHARES ITS DATABASE WITH EVERY OTHER, and two of them read the
// agents table as they found it (agent-menu counts the seed's three cards;
// kinds reads which pooled account is least used). So every agent made here is
// deleted again, whatever the tests did — and the deletion is checked.
const made: AgentRow[] = [];
test.afterAll(async ({ browser }) => {
  try {
    if (made.length) {
      const ctx = await browser.newContext();
      const page = await ctx.newPage();
      await login(page);
      for (const a of made) {
        const r = await page.request.delete(`${origin}/agents/${a.id}`, { headers: { Origin: origin } });
        expect(r.status(), `deleting ${a.name}: ${await r.text()}`).toBeLessThan(300);
      }
      await expectAgentsDestroyed(
        agentRows,
        made.map((m) => m.id),
        'agents this spec made are still there',
      );
      await ctx.close();
    }
  } finally {
    await server?.stop();
  }
});

interface AgentRow {
  id: number;
  name: string;
  status: string;
}

async function agentRows(): Promise<AgentRow[]> {
  const ctx = await pwRequest.newContext({ baseURL: origin, extraHTTPHeaders: { Authorization: `Bearer ${hookToken}` } });
  const r = await ctx.get('/api/agents');
  expect(r.status(), await r.text()).toBe(200);
  const rows = (await r.json()) as AgentRow[];
  await ctx.dispose();
  return rows;
}

const storedStatus = async (name: string) => (await agentRows()).find((a) => a.name === name)?.status ?? '(missing)';

async function login(page: Page) {
  await page.goto(origin + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
}

// saveAgent creates one agent of the kind with no note through the dispatch
// sheet's "Save for later" and returns its row (diffed against what was there).
async function saveAgent(page: Page, kind: 'gateway' | 'claude-code'): Promise<AgentRow> {
  const before = new Set((await agentRows()).map((a) => a.name));
  await page.goto(origin + '/agents');
  await page.click('#fab-agents');
  const sheet = page.locator('#agent-modal form');
  await expect(sheet.locator('[data-kind-picker]')).toBeVisible();
  await sheet.locator(`[data-kind-option="${kind}"]`).click();
  await sheet.locator('textarea[name="note_text"]').fill('');
  const resp = page.waitForResponse((r) => r.url().endsWith('/agents') && r.request().method() === 'POST');
  await sheet.locator('button[value="save"]').click();
  expect((await resp).status()).toBe(200);
  const created = (await agentRows()).filter((a) => !before.has(a.name));
  expect(created.length, 'exactly one agent created').toBe(1);
  made.push(created[0]);
  return created[0];
}

async function noHorizontalScroll(page: Page) {
  const [sw, cw] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(sw, 'document scrollWidth').toBeLessThanOrEqual(cw);
}

const screen = (page: Page) => page.locator('#term .xterm-rows');

for (const vp of [
  { name: 'phone 390×844 touch', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1280×800', use: { viewport: { width: 1280, height: 800 }, hasTouch: false, isMobile: false } },
]) {
  test.describe(vp.name, () => {
    test.use({ ...vp.use, storageState: { cookies: [], origins: [] } });

    // 🔴 A REGRESSION TEST: before this the chat page had a lone "Terminal"
    // link, the terminal page had no way back but the arrow, and the terminal
    // had no message box at all.
    test('an agent opens on its rendered view; both views have the switch and a message box; a stopped agent’s says why it cannot be sent to', async ({ page }) => {
      await login(page);
      const agent = await saveAgent(page, 'claude-code');

      // The card opens the RENDERED view: the chat.
      await page.goto(origin + '/agents');
      const card = page.locator(`#agents-list article:has(a[href="/agents/${agent.name}"])`);
      await expect(card).toBeVisible();
      await card.locator(`a[href="/agents/${agent.name}"]`).click();
      await page.waitForURL((u) => u.pathname === '/agents/' + agent.name);

      // The switch: Rendered is where we are, Terminal one tap away, thumb-sized.
      await expect(page.locator('#view-rendered')).toHaveAttribute('aria-current', 'page');
      const toTerm = page.locator('#view-terminal');
      await expect(toTerm).toHaveText('Terminal');
      const tb = (await toTerm.boundingBox())!;
      expect(tb.height, 'a 44px target').toBeGreaterThanOrEqual(44);
      expect(tb.x + tb.width, 'the switch fits the viewport').toBeLessThanOrEqual(vp.use.viewport.width);
      // The chat's own message box wears the composer's clothes: 16px, 44px.
      const chatInput = page.locator('#chat-input');
      await expect(chatInput).toBeVisible();
      expect(await chatInput.evaluate((e) => parseFloat(getComputedStyle(e).fontSize)), 'a 16px input').toBeGreaterThanOrEqual(16);
      expect((await chatInput.boundingBox())!.height).toBeGreaterThanOrEqual(44);
      expect((await page.locator('#chat-send').boundingBox())!.height).toBeGreaterThanOrEqual(44);
      await noHorizontalScroll(page);

      // The Terminal: the same switch, the other way round — and a message box.
      await toTerm.click();
      await page.waitForURL(origin + '/agents/' + agent.name + '/terminal');
      await expect(screen(page)).toContainText('FAKE-TUI mode=ro');
      await expect(page.locator('#view-terminal')).toHaveAttribute('aria-current', 'page');
      await expect(page.locator('#view-rendered')).toHaveAttribute('href', '/agents/' + agent.name);
      const input = page.locator('#mv-input');
      const send = page.locator('#mv-send');
      await expect(input).toBeVisible();
      for (const el of [input, send]) {
        const b = (await el.boundingBox())!;
        expect(b.height, 'a 44px target').toBeGreaterThanOrEqual(44);
        expect(b.x + b.width, 'the composer fits the viewport').toBeLessThanOrEqual(vp.use.viewport.width);
        expect(b.y + b.height, 'the composer is on screen').toBeLessThanOrEqual(vp.use.viewport.height);
      }
      // This agent was only saved: it is not running, so it cannot be sent a
      // message — the box is there, disabled, and says why.
      await expect(input).toBeDisabled();
      await expect(send).toBeDisabled();
      await expect(page.locator('#mv-readonly')).toContainText('not running');
      await noHorizontalScroll(page);

      // And back.
      await page.locator('#view-rendered').click();
      await page.waitForURL((u) => u.pathname === '/agents/' + agent.name);
      await expect(page.locator('#chat-input')).toBeVisible();
    });

    test('a running agent is sent a message from its Terminal view, and the conversation shows it', async ({ page }) => {
      test.setTimeout(120_000);
      await login(page);
      const agent = await saveAgent(page, 'claude-code');
      const started = await page.request.post(`${origin}/agents/${agent.id}/start`);
      expect(started.status(), await started.text()).toBe(200);
      // The status promoter ticks every 10 s.
      await expect.poll(() => storedStatus(agent.name), { timeout: 45_000 }).toBe('running');

      await page.goto(`${origin}/agents/${agent.name}/terminal`);
      await expect(screen(page)).toContainText('FAKE-TUI mode=ro');
      const input = page.locator('#mv-input');
      await expect(input).toBeEnabled();
      const sends: number[] = [];
      page.on('response', (r) => {
        if (new URL(r.url()).pathname === `/agents/${agent.name}/send`) sends.push(r.status());
      });
      const text = `please build it (${vp.name})`;
      await input.fill(text);
      // 🔴 ITS DRAFT IS KEPT (composer_draft.go): away to the Rendered view and
      // back, the text is still in the box.
      await page.locator('#view-rendered').click();
      await page.waitForURL((u) => u.pathname === '/agents/' + agent.name);
      await page.goto(`${origin}/agents/${agent.name}/terminal`);
      await expect(input, 'the Terminal box restores its draft').toHaveValue(text);
      await page.locator('#mv-send').click();
      await expect(input).toHaveValue('');
      await expect(page.locator('#mv-note')).toBeHidden();
      expect(sends, 'one send, accepted, with no navigation').toEqual([204]);
      expect(new URL(page.url()).pathname).toBe(`/agents/${agent.name}/terminal`);
      // Sent: its draft is gone.
      await page.goto(`${origin}/agents/${agent.name}/terminal`);
      await expect(page.locator('#mv-input'), 'a sent draft is not restored').toHaveValue('');

      // The rendered view has the message — and, since no pod exists to answer
      // it here, the stored note that it was not answered (which is what shows
      // the turn reached the gateway).
      await page.locator('#view-rendered').click();
      await page.waitForURL((u) => u.pathname === '/agents/' + agent.name);
      const log = page.locator('#chat-log');
      await expect(log.locator('[data-bubble="user"]').last()).toContainText(text, { timeout: 30_000 });
      await expect(log).toContainText('This message was not answered', { timeout: 60_000 });
      await noHorizontalScroll(page);

      // 🔴 THE CHAT'S OWN FORM WITH ITS SOCKET NOT OPEN. It used to return and
      // do nothing. Every socket this page opens is closed on arrival here, so
      // Send has only the form to go by: the browser posts it, and the page
      // comes back saying so, with the message in the conversation.
      let closed = 0;
      await page.routeWebSocket(/\/agents\/[^/]+\/ws(\?|$)/, (ws) => {
        closed++;
        void ws.close({ code: 1011, reason: 'e2e: no socket' });
      });
      await page.goto(`${origin}/agents/${agent.name}`);
      await expect.poll(() => closed, 'control: the chat socket was refused').toBeGreaterThan(0);
      const viaForm = `by the form, no socket (${vp.name})`;
      await page.locator('#chat-input').fill(viaForm);
      // 🔴 THE WAIT IS ARMED BEFORE THE CLICK. The page this lands on takes
      // ?notice=sent out of its address as soon as its script runs (a reload
      // must not repeat the notice). A waitForURL started after the click
      // found an address that had already lost it, so it waited for a SECOND
      // navigation that never comes, until the test timed out (CI, run
      // 39502271983). Armed first, it sees the landing itself.
      await Promise.all([
        page.waitForURL((u) => u.pathname === '/agents/' + agent.name && u.searchParams.get('notice') === 'sent'),
        page.locator('#chat-send').click(),
      ]);
      await expect(page.locator('#chat-note')).toHaveText('Sent.');
      // The browser sent it, and the page it came back to says so: its draft
      // is not put back in the box.
      await expect(page.locator('#chat-input'), 'a sent draft is not restored').toHaveValue('');
      // …and the notice is taken out of the address once shown.
      await expect.poll(() => new URL(page.url()).searchParams.get('notice')).toBeNull();
      await expect(page.locator('#chat-log')).toContainText(viaForm);
    });

    // 🔴 A CHAT DRAFT, PER DEVICE (composer_draft.go): what is typed into an
    // agent's chat box is kept as it is typed, restored on coming back, and
    // never offered in ANOTHER agent's chat. On origin/main nothing is kept.
    test('the chat box keeps its draft across navigation, for that agent only', async ({ page }) => {
      await login(page);
      const a = await saveAgent(page, 'gateway');
      const b = await saveAgent(page, 'gateway');
      await page.goto(`${origin}/agents/${a.name}`);
      const draft = `a chat draft (${vp.name})\nwith a second line`;
      await page.locator('#chat-input').fill(draft);
      // Away straight away (no wait for the debounce), and back.
      await page.goto(`${origin}/agents`);
      await page.goto(`${origin}/agents/${a.name}`);
      await expect(page.locator('#chat-input'), 'the draft is back').toHaveValue(draft);
      await page.goto(`${origin}/agents/${b.name}`);
      await expect(page.locator('#chat-input')).toBeVisible();
      await expect(page.locator('#chat-input'), "another agent's chat starts empty").toHaveValue('');
      // CONTROL: the agent it was typed for still has it; emptied, it is gone.
      await page.goto(`${origin}/agents/${a.name}`);
      await expect(page.locator('#chat-input')).toHaveValue(draft);
      await page.locator('#chat-input').fill('');
      await page.goto(`${origin}/agents`);
      await page.goto(`${origin}/agents/${a.name}`);
      await expect(page.locator('#chat-input'), 'an emptied box keeps no draft').toHaveValue('');
      await noHorizontalScroll(page);
    });

    // 🔴 A REPLY THAT LANDS BEFORE THE PAGE'S EVENT STREAM IS CONNECTED IS
    // STILL SHOWN. The conversation re-fetches on `chat.reply`, which the server
    // broadcasts once, when the turn is stored; a page whose /events was not
    // connected yet missed it and showed "working" for ever (a CI failure of
    // the test above: the failed turn's note was stored in the ~200 ms between
    // the page rendering and its stream opening). Here the stream is HELD until
    // the reply is stored, so the miss is certain, not a race; the page must
    // catch up when the stream opens.
    test('a reply stored before the event stream connects is still shown', async ({ page }) => {
      test.setTimeout(120_000);
      await login(page);
      const agent = await saveAgent(page, 'claude-code');
      const started = await page.request.post(`${origin}/agents/${agent.id}/start`);
      expect(started.status(), await started.text()).toBe(200);
      await expect.poll(() => storedStatus(agent.name), { timeout: 45_000 }).toBe('running');

      let release!: () => void;
      const held = new Promise<void>((r) => (release = r));
      let heldStreams = 0;
      await page.route(/\/events(\?|$)/, async (route) => {
        heldStreams++;
        await held;
        await route.continue();
      });
      await page.goto(`${origin}/agents/${agent.name}`);
      await expect.poll(() => heldStreams, 'control: the event stream was held').toBeGreaterThan(0);

      // A message sent the Terminal composer's way: the turn runs on the server
      // and is stored (with its "not answered" note, no pod being there) while
      // this page's stream is still held.
      const text = `before the stream (${vp.name})`;
      const sent = await page.request.post(`${origin}/agents/${agent.name}/send?from=terminal`, {
        form: { text },
        headers: { Origin: origin, 'X-Requested-With': 'XMLHttpRequest' },
      });
      expect(sent.status(), await sent.text()).toBe(204);
      await expect
        .poll(async () => (await (await page.request.get(`${origin}/ui/agents/${agent.name}/chat-log`)).text()).includes('This message was not answered'), {
          message: 'the failed turn is stored',
          timeout: 60_000,
        })
        .toBe(true);
      await expect(page.locator('#chat-log'), 'the page has not seen it yet').not.toContainText('This message was not answered');

      release();
      await expect(page.locator('#chat-log')).toContainText('This message was not answered', { timeout: 15_000 });
      await expect(page.locator('#chat-log')).toContainText(text);
    });

    // The chat form without its script: it used to have no action and no method,
    // so a page whose script had not run could not send at all.
    test('with no script at all, the chat form and the Terminal’s composer still send', async ({ page, browser }) => {
      test.setTimeout(120_000);
      await login(page);
      const agent = await saveAgent(page, 'claude-code');
      expect((await page.request.post(`${origin}/agents/${agent.id}/start`)).status()).toBe(200);
      await expect.poll(() => storedStatus(agent.name), { timeout: 45_000 }).toBe('running');

      const ctx = await browser.newContext({ ...vp.use, javaScriptEnabled: false });
      await ctx.addCookies(await page.context().cookies(origin));
      const plain = await ctx.newPage();
      try {
        await plain.goto(`${origin}/agents/${agent.name}`);
        await plain.locator('#chat-input').fill('from the chat form, no script');
        await plain.locator('#chat-send').click();
        await plain.waitForURL((u) => u.pathname === '/agents/' + agent.name && u.searchParams.get('notice') === 'sent', { timeout: 20_000 });
        await expect(plain.locator('#chat-note')).toHaveText('Sent.');
        // The page is server-rendered: the message it just sent is in the transcript.
        await expect(plain.locator('#chat-log')).toContainText('from the chat form, no script');

        await plain.goto(`${origin}/agents/${agent.name}/terminal`);
        await plain.locator('#mv-input').fill('from the terminal, no script');
        await plain.locator('#mv-send').click();
        await plain.waitForURL((u) => u.pathname === `/agents/${agent.name}/terminal` && u.searchParams.get('notice') === 'sent', { timeout: 20_000 });
        await expect(plain.locator('#mv-note')).toHaveText('Sent.');
        await plain.goto(`${origin}/agents/${agent.name}`);
        await expect(plain.locator('#chat-log')).toContainText('from the terminal, no script');
      } finally {
        await ctx.close();
      }
    });
  });
}

// A gateway agent has no terminal: one view, and no switch to a page that
// would 404.
test('an agent of a kind with no terminal has no view switch', async ({ page }) => {
  await login(page);
  const agent = await saveAgent(page, 'gateway');
  await page.goto(`${origin}/agents/${agent.name}`);
  await expect(page.locator('#chat-input')).toBeVisible();
  await expect(page.locator('#view-terminal')).toHaveCount(0);
  await expect(page.locator('#view-rendered')).toHaveCount(0);
});
