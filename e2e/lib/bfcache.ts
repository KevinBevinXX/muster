// A browser with the back/forward cache ON, for specs about Back and Forward.
//
// 🔴 THE SUITE'S OWN BROWSER CANNOT RESTORE A PAGE, FOR TWO REASONS, AND
// REMOVING ONE IS NOT ENOUGH. Playwright launches Chromium with
// --disable-back-forward-cache; and the suite's default headless browser is
// chromium-headless-shell, whose embedder turns the cache off by itself.
// Measured with a two-page static site (Chromium 153, Playwright 1.63): the
// shell with the flag removed still reloads, and CDP's
// Page.backForwardCacheNotUsed names `BackForwardCacheDisabledForDelegate`;
// the full Chromium build (channel "chromium", new headless) with the flag
// removed restores it (pageshow persisted, the same document). So every
// goBack/goForward elsewhere in this suite measures the RELOAD path, while a
// reader's browser takes the restore path.
//
// The suite as a whole stays on the shell: switching every spec's browser is a
// change to what every other spec measures. A spec about history navigation
// launches this browser instead. tests/bfcache.spec.ts is its control — a
// static page MUST come back persisted here — so a spec built on it cannot pass
// because nothing could ever be restored.
//
// ⚠ A RESTORE FIRES NO `load`. page.goBack() waits for load by default and
// times out on a restore; pass { waitUntil: 'commit' } and wait for pageshow.
import { Browser, BrowserType } from '@playwright/test';

export const bfcacheLaunch = {
  channel: 'chromium',
  ignoreDefaultArgs: ['--disable-back-forward-cache'],
};

export function launchWithBfcache(chromium: BrowserType): Promise<Browser> {
  return chromium.launch(bfcacheLaunch);
}
