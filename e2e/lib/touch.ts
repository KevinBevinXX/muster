import { Page } from '@playwright/test';

// 🔴 REAL TOUCH: a drag as Chrome touch events (CDP Input.dispatchTouchEvent),
// which go through the browser's own gesture handling — touch-action, native
// panning of a scroll area — exactly as a finger does. A scripted scrollTop or a
// synthetic TouchEvent would bypass precisely what a gesture test is about.
export type Pt = { x: number; y: number };

// held, when given, runs with the finger down and not yet moved: a spec changes
// the page under the finger there (a redraw), as the host can mid-gesture.
export async function touchDrag(page: Page, from: Pt, to: Pt, steps = 12, held?: () => Promise<void>) {
  const cdp = await page.context().newCDPSession(page);
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x: from.x, y: from.y }] });
  if (held) await held();
  for (let i = 1; i <= steps; i++) {
    const x = from.x + ((to.x - from.x) * i) / steps;
    const y = from.y + ((to.y - from.y) * i) / steps;
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x, y }] });
    await page.waitForTimeout(16);
  }
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
  await cdp.detach();
}
