// Builds both server versions, migrates and seeds the e2e database, starts the
// shared server, and saves a signed-in browser state. Returns the teardown.
import { chromium, FullConfig } from '@playwright/test';
import fs from 'node:fs';
import { authState, baseURL, dsn, password, port, requireFull, stateDir, versionA, versionB, versionC, versionTerm } from './lib/env';
import { buildServer, goRun, startServer } from './lib/server';

export default async function globalSetup(_config: FullConfig) {
  fs.mkdirSync(stateDir, { recursive: true });
  const skipFile = `${stateDir}/skip`;
  fs.rmSync(skipFile, { force: true });
  if (!dsn) {
    if (requireFull) {
      throw new Error('MUSTER_E2E_DATABASE_URL is unset and MUSTER_E2E_REQUIRE_FULL=1: refusing to report a suite that ran nothing');
    }
    // Every spec checks this file and skips — loudly, in the verdict's count.
    fs.writeFileSync(skipFile, 'no MUSTER_E2E_DATABASE_URL');
    return async () => {};
  }

  buildServer(versionA);
  buildServer(versionB);
  buildServer(versionC);
  buildServer(versionTerm, ['e2eterm']);
  goRun('./cmd/muster-migrate', { DATABASE_URL: dsn });
  goRun('./e2e/seed', { DATABASE_URL: dsn });

  const server = await startServer(versionA, port);

  const browser = await chromium.launch();
  const page = await browser.newPage();
  await page.goto(baseURL + '/login');
  await page.fill('#muster-password', password);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login')), page.click('button[type=submit]')]);
  await page.context().storageState({ path: authState });
  await browser.close();

  return async () => {
    await server.stop();
  };
}
