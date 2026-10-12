import { defineConfig, devices } from '@playwright/test';
import fs from 'node:fs';
import path from 'node:path';
import { baseURL } from './lib/env';

// One worker, in order: every spec shares one server and one seeded database,
// and several of them write to it (a share creates a task, the badge spec flips
// statuses). Parallel workers would race those writes.
//
// The `capture` project is the manifest-screenshot generator, not a test. It
// runs only when MUSTER_E2E_CAPTURE=1 (`npm run capture`), so a CI run never
// rewrites a committed image.
const capture = process.env.MUSTER_E2E_CAPTURE === '1';

// 🔴 CI RUNS THE SUITE IN LEGS: MUSTER_E2E_LEG=N runs only leg N's spec files
// from legs.json, each leg on its own runner with its own database. Unset, every
// spec runs (make e2e). `verdict.mjs --all` over every leg's report refuses a
// spec that ran in no leg or in two. An unknown or empty leg THROWS rather than
// matching nothing, because a run of zero files is a green run.
const leg = process.env.MUSTER_E2E_LEG ?? '';
function legMatch(): RegExp | RegExp[] {
  if (!leg) return /tests\/.*\.spec\.ts/;
  const { legs } = JSON.parse(fs.readFileSync(path.join(__dirname, 'legs.json'), 'utf8'));
  const files: unknown = legs[leg];
  if (!Array.isArray(files) || files.length === 0) {
    throw new Error(`MUSTER_E2E_LEG=${leg}: no such leg (or an empty one) in e2e/legs.json`);
  }
  // Anchored on the whole relative path, so tests/sw.spec.ts cannot also match
  // a file whose name merely ends in sw.spec.ts.
  const esc = (f: string) => f.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  return files.map((f: string) => new RegExp(`(^|/)${esc(f)}$`));
}

export default defineConfig({
  testDir: '.',
  globalSetup: './global-setup.ts',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 60_000,
  reporter: [['list'], ['json', { outputFile: 'test-results/results.json' }]],
  use: {
    baseURL,
    trace: 'retain-on-failure',
  },
  projects: capture
    ? [{ name: 'capture', testMatch: /capture\/.*\.spec\.ts/, use: { ...devices['Desktop Chrome'] } }]
    : [
        {
          name: 'chromium',
          testMatch: legMatch(),
          use: {
            ...devices['Desktop Chrome'],
            // Phone-class viewport by default (Pixel/Galaxy width); specs that
            // need another size set their own.
            viewport: { width: 390, height: 844 },
          },
        },
      ],
});
