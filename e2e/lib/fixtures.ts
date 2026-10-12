// The suite's shared test object: signed in, phone-sized, with a machine API
// helper. Every spec imports `test` and `expect` from here.
import { test as base, expect, APIRequestContext, Page, request as pwRequest } from '@playwright/test';
import fs from 'node:fs';
import { authState, baseURL, hookToken, stateDir } from './env';

export { expect };

export interface Task {
  id: number;
  title: string;
  body: string;
  status: string;
}

export class Api {
  constructor(private readonly ctx: APIRequestContext) {}
  async tasks(): Promise<Task[]> {
    const r = await this.ctx.get('/api/tasks');
    expect(r.status(), await r.text()).toBe(200);
    return (await r.json()) as Task[];
  }
  async createTask(title: string, body = ''): Promise<Task> {
    const r = await this.ctx.post('/api/tasks', { data: { title, body } });
    expect(r.status(), await r.text()).toBeLessThan(300);
    return (await r.json()) as Task;
  }
  async setStatus(id: number, status: string): Promise<void> {
    const r = await this.ctx.patch(`/api/tasks/${id}/status`, { data: { status } });
    expect(r.status(), await r.text()).toBeLessThan(300);
  }
}

export const test = base.extend<{ api: Api; requireServer: void }>({
  storageState: authState,
  baseURL,
  api: async ({}, use) => {
    const ctx = await pwRequest.newContext({ baseURL, extraHTTPHeaders: { Authorization: `Bearer ${hookToken}` } });
    await use(new Api(ctx));
    await ctx.dispose();
  },
  // 🔴 A SUITE WITH NO DATABASE SKIPS — AND SAYS SO. global-setup writes this
  // file when MUSTER_E2E_DATABASE_URL is unset (and refuses outright when
  // MUSTER_E2E_REQUIRE_FULL=1). The verdict step caps skips, so a CI run cannot
  // go green on a suite that executed nothing.
  requireServer: [
    async ({}, use, testInfo) => {
      testInfo.skip(fs.existsSync(`${stateDir}/skip`), 'no e2e database (MUSTER_E2E_DATABASE_URL unset)');
      await use();
    },
    { auto: true },
  ],
});

// effectiveThemeColor is the colour the browser would tint the status bar
// with: the FIRST theme-color meta whose media matches.
export const effectiveThemeColorJS = `(() => {
  for (const m of document.querySelectorAll('meta[name="theme-color"]')) {
    const q = m.getAttribute('media');
    if (!q || window.matchMedia(q).matches) return m.getAttribute('content');
  }
  return null;
})()`;

// navTabKey is the localStorage key appScript saves the last tab under.
export const navTabKey = 'muster.nav.tab';

// shellStateJS reads what the shell is SHOWING, by geometry rather than by
// presence: which tabpanels and which FABs have a box, what the heading says,
// and which sidebar link claims to be the current page.
//
// It reads boxes because the defect it exists for was a shell whose panels and
// FABs were all in the DOM and all `hidden`: every text and count assertion on
// that document passes, and the page is blank.
const shellStateJS = `(() => {
  const boxed = (e) => e.getClientRects().length > 0 && getComputedStyle(e).visibility !== 'hidden';
  return {
    panels: [...document.querySelectorAll('[role="tabpanel"][id^="panel-"]')].filter(boxed).map((e) => e.id.slice(6)),
    drawn: document.querySelectorAll('[role="tabpanel"][id^="panel-"]').length,
    fabs: [...document.querySelectorAll('[data-fab]')].filter(boxed).map((e) => e.getAttribute('data-fab')),
    heading: (document.getElementById('page-heading') || {}).textContent || '',
    current: [...document.querySelectorAll('.sidebar-tab[aria-current="page"]')].map((e) => e.getAttribute('data-tab')),
  };
})()`;

// expectShellShows asserts the shell is showing exactly ONE tab, and that the
// panel, the FAB, the heading and aria-current all name the same one.
export async function expectShellShows(page: Page, tab: string, heading: string, why: string): Promise<void> {
  await expect
    .poll(() => page.evaluate(shellStateJS), { message: why })
    .toMatchObject({ panels: [tab], fabs: [tab], heading, current: [tab] });
  const drawn = ((await page.evaluate(shellStateJS)) as { drawn: number }).drawn;
  expect(drawn, `${why}: CONTROL — the shell drew more than one panel, so "exactly one shown" hid the rest`).toBeGreaterThan(1);
}
