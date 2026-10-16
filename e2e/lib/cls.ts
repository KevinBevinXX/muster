// Cumulative layout shift, measured in the page under test.
//
// clsInit is an init script: it records every `layout-shift` entry from the
// first paint, EXCEPT those with hadRecentInput (a shift within 500 ms of the
// reader's own tap or keystroke is the page answering them, which is what CLS
// itself excludes). It also counts htmx requests in flight and stamps the last
// DOM change, so settle() can wait for the async loads to land rather than for
// a fixed time.
//
// 🔴 THE NUMBER IS THE SUM OF EVERY SHIFT, NOT web-vitals' WORST SESSION WINDOW.
// The sum is never smaller, so a page under the bar here is under it by
// web-vitals' rule too; the converse does not hold.
import { Page } from '@playwright/test';

export const clsInit = `(() => {
  window.__cls = { total: 0, excluded: 0, shifts: [], inflight: 0, last: performance.now() };
  const S = window.__cls;
  const name = (n) => {
    if (!n || n.nodeType !== 1) return n && n.nodeName ? n.nodeName : '?';
    let s = n.tagName.toLowerCase();
    if (n.id) s += '#' + n.id;
    const c = (n.getAttribute('class') || '').trim().split(/\\s+/).slice(0, 3).join('.');
    if (c) s += '.' + c;
    return s;
  };
  try {
    new PerformanceObserver((list) => {
      for (const e of list.getEntries()) {
        if (e.hadRecentInput) { S.excluded += e.value; S.last = performance.now(); continue; }
        S.total += e.value;
        S.last = performance.now();
        S.shifts.push({ value: +e.value.toFixed(5), at: Math.round(e.startTime), sources: (e.sources || []).map((x) => name(x.node)) });
      }
    }).observe({ type: 'layout-shift', buffered: true });
  } catch (e) { S.unsupported = String(e); }
  document.addEventListener('htmx:beforeRequest', () => { S.inflight++; S.last = performance.now(); });
  document.addEventListener('htmx:afterRequest', () => { S.inflight = Math.max(0, S.inflight - 1); S.last = performance.now(); });
  document.addEventListener('htmx:afterSettle', () => { S.last = performance.now(); });
  const watch = () => new MutationObserver(() => { S.last = performance.now(); }).observe(document.documentElement, { subtree: true, childList: true, attributes: true, characterData: true });
  if (document.documentElement) watch(); else document.addEventListener('DOMContentLoaded', watch);
})()`;

export interface ClsResult {
  total: number;
  // excluded is the shift that hadRecentInput kept out of total: the page
  // answering the reader's own tap.
  excluded: number;
  shifts: { value: number; at: number; sources: string[] }[];
  unsupported?: string;
}

// settle waits for the page's async loads to land: the load event, no htmx
// request in flight, and quiet (no DOM change, no shift) for quietMs — capped
// at maxMs, after which whatever was measured is what is reported.
export async function settle(page: Page, quietMs = 1000, maxMs = 12_000): Promise<void> {
  await page.waitForLoadState('load');
  const t0 = Date.now();
  for (;;) {
    const quiet = await page.evaluate((q) => {
      const S = (window as any).__cls;
      return !!S && S.inflight === 0 && performance.now() - S.last >= q;
    }, quietMs);
    if (quiet || Date.now() - t0 > maxMs) return;
    await page.waitForTimeout(150);
  }
}

export async function readCls(page: Page): Promise<ClsResult> {
  return page.evaluate(() => {
    const S = (window as any).__cls;
    return { total: +S.total.toFixed(5), excluded: +S.excluded.toFixed(5), shifts: S.shifts, unsupported: S.unsupported };
  });
}

// resetCls zeroes the record, for measuring a state reached after the load
// (the chief panel opening) on its own.
export async function resetCls(page: Page): Promise<void> {
  await page.evaluate(() => {
    const S = (window as any).__cls;
    S.total = 0;
    S.excluded = 0;
    S.shifts = [];
    S.last = performance.now();
  });
}
