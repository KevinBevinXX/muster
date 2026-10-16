import path from 'node:path';
import { test, expect } from '../lib/fixtures';
import { dsn, repoRoot } from '../lib/env';

// Generates the manifest's install-sheet screenshots from the SEEDED fixture
// board (no real data): 360×640 CSS px at 3× = 1080×1920, the size the
// manifest declares (TestManifestImagesShipAtTheirDeclaredSize checks the
// files). Run with `npm run capture`; never part of the test run.
//
// 🔴 SEEDED FIXTURE DATA ONLY, EVER. The PNGs land in /static/icons/, which is
// planned to be reachable WITHOUT auth (an edge bypass for the manifest's
// images), so whatever this photographs is published. global-setup runs the
// seed before starting the server, and the seed refuses any database not named
// *_e2e (e2edb's TestCheckRefusesAnythingButAnE2EDatabase); the check below refuses the
// same thing again here, so a capture can never run against a real board.
//
// ⚠ ONE THING IS HIDDEN, AND IT IS SAID HERE: the push-permission status line.
// A headless browser has notifications blocked, so the page shows "Notifications
// are blocked…" — true of the harness, not of the app a person installs.
test.use({ viewport: { width: 360, height: 640 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true });

for (const [route, file, ready] of [
  ['/tasks', 'screenshot-tasks-narrow.png', '#tasks-list article'],
  ['/agents', 'screenshot-agents-narrow.png', '#agents-list article'],
] as const) {
  test(`capture ${file}`, async ({ page }) => {
    // pgx connects to ?dbname= / ?database= over the path, so every name the
    // DSN carries must be an e2e one (the Go side asks pgx: e2edb.Check).
    const u = new URL(dsn);
    const names = [u.pathname.slice(1), ...u.searchParams.getAll('dbname'), ...u.searchParams.getAll('database')];
    for (const name of names) expect(name, 'captures run only against the seeded *_e2e database').toMatch(/_e2e$/);
    await page.goto(route);
    await page.addStyleTag({ content: '#push-status{display:none!important}' });
    await expect(page.locator(ready).first()).toBeVisible();
    await page.waitForTimeout(800);
    await page.screenshot({ path: path.join(repoRoot, 'web/static/icons', file) });
  });
}
