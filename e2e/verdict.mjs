#!/usr/bin/env node
// verdict.mjs — read Playwright's JSON report and refuse a run that proves
// nothing.
//
// 🔴 A COUNT, NOT AN EXIT CODE. `playwright test` exits 0 when every test
// skipped, and a suite pointed at no database skips by design. This prints the
// PAIR (how many passed, how many skipped) and fails when fewer than FLOOR tests
// passed or more than CAP skipped.
//
// FLOOR is the number of tests in e2e/tests/ (measured: every one passes
// against a seeded database — see the PR). A floor below the real count is
// indistinguishable from no floor, so when a spec is added, raise it in the
// same change.
import fs from 'node:fs';

const FLOOR = Number(process.env.E2E_PASS_FLOOR ?? 31);
const CAP = Number(process.env.E2E_SKIP_CAP ?? 0);
const file = process.argv[2] ?? 'test-results/results.json';

const report = JSON.parse(fs.readFileSync(file, 'utf8'));
const s = report.stats ?? {};
const passed = (s.expected ?? 0) + (s.flaky ?? 0);
const skipped = s.skipped ?? 0;
const failed = s.unexpected ?? 0;
console.log(`e2e verdict: ${passed} passed (${s.flaky ?? 0} flaky), ${failed} failed, ${skipped} skipped — floor ${FLOOR}, skip cap ${CAP}`);
let bad = false;
if (failed > 0) { console.error('FAIL: tests failed'); bad = true; }
if (passed < FLOOR) { console.error(`FAIL: ${passed} passed is under the floor of ${FLOOR}`); bad = true; }
if (skipped > CAP) { console.error(`FAIL: ${skipped} skipped exceeds the cap of ${CAP}`); bad = true; }
process.exit(bad ? 1 : 0);
