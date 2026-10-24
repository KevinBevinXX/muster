// A throttled profile: network delay plus a CPU slowdown, applied to one page
// over CDP (Chromium only), for specs whose result depends on TIMING — when a
// request lands relative to the first paint, how long an open takes.
//
// 🔴 THE UNTHROTTLED HARNESS HIDES ORDERING BUGS. On this suite's loopback
// server every static asset arrives within a millisecond of being asked for, so
// a page whose final layout waits for a deferred script almost always gets it
// before its first paint, and a layout shift that a phone on a mobile network
// sees on every open reads 0 here. Measured on origin/main's 200-message agent
// page at 1280px (cls-long-transcript.spec.ts, three fresh contexts a case):
// unthrottled, markdown 0.37 / 0 / 0 and plain 0 / 0 / 0; under THROTTLED,
// markdown 0.37 three times and plain 0.43 three times.
//
// ⚠ IT IS A PROFILE, NOT A PHONE. The latency is a hold on every request the
// page makes before it is sent (page.route), so there is no bandwidth limit;
// the CPU rate multiplies this host's speed, not a device's. Use it to make an
// ordering race deterministic, not to quote a time a reader would see.
//
// 🔴 THE LATENCY IS NOT CDP's Network.emulateNetworkConditions, BECAUSE THAT
// HUNG. With it (150 ms, 9/1.5 Mbit/s) a run of this suite's agent page twice
// left both deferred vendor scripts without a response for the whole 120 s test
// (the trace's network log: htmx.min.js and sse.js at status -1, app.css 200),
// in a case that passed on the next two runs. A hold in page.route is the same
// delay with nothing of the browser's in between.
//
// A spec applies it with throttle(page, THROTTLED).
import { Page } from '@playwright/test';

export interface Profile {
  name: string;
  // How long every request is held before it is sent, in ms.
  latencyMs: number;
  // 1 = no slowdown; 4 = four times slower.
  cpuRate: number;
}

// A phone on a mobile network, roughly: 150 ms on every request, a 4x CPU.
export const THROTTLED: Profile = { name: 'throttled (150 ms per request, 4x CPU)', latencyMs: 150, cpuRate: 4 };

// throttle applies p to page (and nothing, for null). It lasts for the page's
// life, across its navigations. A WebSocket is not held (page.route does not
// see one).
export async function throttle(page: Page, p: Profile | null): Promise<void> {
  if (!p) return;
  await page.route('**/*', async (route) => {
    await new Promise((r) => setTimeout(r, p.latencyMs));
    // The page may be gone by then (a test ending mid-load).
    await route.continue().catch(() => {});
  });
  const cdp = await page.context().newCDPSession(page);
  await cdp.send('Emulation.setCPUThrottlingRate', { rate: p.cpuRate });
}
