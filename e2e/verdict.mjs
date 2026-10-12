#!/usr/bin/env node
// verdict.mjs — read Playwright's JSON report(s) and refuse a run that proves
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
//
// 🔴 CI RUNS THE SUITE IN LEGS (e2e/legs.mjs), SO THERE ARE THREE MODES:
//
//   node verdict.mjs REPORT                  the whole suite in one run (make e2e)
//   node verdict.mjs --leg N REPORT          one CI leg
//   node verdict.mjs --all REPORT REPORT…    every leg's report, summed
//
// The failed == 0 and skipped <= CAP rules hold in every mode. FLOOR is a TOTAL,
// so it holds in the whole-suite and `--all` modes; a leg's floor defaults to 1.
//
// 🔴 `--leg` AND `--all` ALSO PIN THE FILES. legs.mjs proves legs.json covers
// every spec once; it cannot prove a leg RAN what legs.json says (a testMatch
// that matches a subset is a smaller, green run). So here the spec files present
// in the report, and the files with at least one passing test, must each EQUAL
// the assigned set — that leg's files for `--leg`, every spec on disk for
// `--all` — and under `--all` no file may appear in two reports (run twice).
import fs from 'node:fs';
import { loadLegs, specFiles } from './legs.mjs';

const args = process.argv.slice(2);
let mode = 'whole';
let leg = '';
if (args[0] === '--leg') {
  mode = 'leg';
  leg = args[1] ?? '';
  args.splice(0, 2);
} else if (args[0] === '--all') {
  mode = 'all';
  args.splice(0, 1);
}
const files = args.length ? args : ['test-results/results.json'];
if (mode !== 'all' && files.length !== 1) {
  console.error(`usage: one report per run unless --all (got ${files.length})`);
  process.exit(2);
}

const FLOOR = Number(process.env.E2E_PASS_FLOOR ?? (mode === 'leg' ? 1 : 31));
const CAP = Number(process.env.E2E_SKIP_CAP ?? 0);

// perFile walks a report's suite tree: file -> { tests, passed }.
function perFile(report) {
  const out = new Map();
  const visit = (suite, file) => {
    for (const spec of suite.specs ?? []) {
      for (const t of spec.tests ?? []) {
        const e = out.get(file) ?? { tests: 0, passed: 0 };
        e.tests++;
        if (t.status === 'expected' || t.status === 'flaky') e.passed++;
        out.set(file, e);
      }
    }
    for (const s of suite.suites ?? []) visit(s, file);
  };
  for (const s of report.suites ?? []) visit(s, s.file);
  return out;
}

const sum = { expected: 0, flaky: 0, unexpected: 0, skipped: 0 };
const seen = new Map(); // file -> report path
const overlaps = [];
const ran = new Map();
for (const f of files) {
  const report = JSON.parse(fs.readFileSync(f, 'utf8'));
  const s = report.stats ?? {};
  for (const k of Object.keys(sum)) sum[k] += s[k] ?? 0;
  for (const [file, e] of perFile(report)) {
    if (seen.has(file)) overlaps.push(`${file} is in ${seen.get(file)} AND ${f}`);
    seen.set(file, f);
    const prev = ran.get(file) ?? { tests: 0, passed: 0 };
    ran.set(file, { tests: prev.tests + e.tests, passed: prev.passed + e.passed });
  }
}

const passed = sum.expected + sum.flaky;
const label = mode === 'leg' ? `e2e verdict (leg ${leg})` : mode === 'all' ? `e2e verdict (${files.length} reports)` : 'e2e verdict';
console.log(`${label}: ${passed} passed (${sum.flaky} flaky), ${sum.unexpected} failed, ${sum.skipped} skipped — floor ${FLOOR}, skip cap ${CAP}`);
let bad = false;
if (sum.unexpected > 0) { console.error('FAIL: tests failed'); bad = true; }
if (passed < FLOOR) { console.error(`FAIL: ${passed} passed is under the floor of ${FLOOR}`); bad = true; }
if (sum.skipped > CAP) { console.error(`FAIL: ${sum.skipped} skipped exceeds the cap of ${CAP}`); bad = true; }

if (mode !== 'whole') {
  let want;
  if (mode === 'leg') {
    want = loadLegs().legs[leg];
    if (!Array.isArray(want)) {
      console.error(`FAIL: no leg ${JSON.stringify(leg)} in e2e/legs.json`);
      process.exit(1);
    }
  } else {
    want = specFiles();
  }
  const wantSet = new Set(want);
  const passing = [...ran].filter(([, e]) => e.passed > 0).length;
  console.log(`spec files: ${want.length} assigned, ${ran.size} in the report(s), ${passing} with a passing test`);
  for (const o of overlaps) { console.error(`FAIL: RAN TWICE: ${o}`); bad = true; }
  for (const f of want) {
    if (!ran.has(f)) { console.error(`FAIL: NOT RUN: ${f} is assigned but absent from the report(s)`); bad = true; }
    else if (ran.get(f).passed === 0) { console.error(`FAIL: NO PASS: ${f} ran but no test in it passed`); bad = true; }
  }
  for (const f of ran.keys()) {
    if (!wantSet.has(f)) { console.error(`FAIL: STRAY: ${f} ran but is not assigned here`); bad = true; }
  }
}
process.exit(bad ? 1 : 0);
