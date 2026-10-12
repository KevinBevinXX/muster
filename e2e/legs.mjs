#!/usr/bin/env node
// legs.mjs — the e2e suite's split into CI matrix legs, and the seam guard on it.
//
// 🔴 WHY THERE ARE LEGS. The specs share one Postgres and write to it, so they
// run in ONE worker (playwright.config.ts) — and one worker was the longest job
// in CI. Each leg is its own runner with its OWN Postgres and its own servers, so
// the legs cannot race each other's writes the way parallel workers would.
//
// 🔴 THE PARTITION IS STATIC AND BALANCED ON MEASURED TIME, NOT `--shard`. With
// `fullyParallel: false`, Playwright shards whole files balanced by TEST COUNT,
// and count is a poor proxy here: claude-code-polish.spec.ts is 10 tests and
// ~42 s, provisioning-indicator.spec.ts is 2 tests and ~20 s, sw.spec.ts is 2
// tests and under 1 s. legs.json's `seconds` are per-file wall times read off a
// main run's list reporter (the delta between successive completion timestamps,
// so a file's own beforeAll server start is in its number), pinned to the commit
// in `measured_at`. They are a balancing input only — nothing asserts them.
//
// ⚠ THREE LEGS, AND A FOURTH WOULD BUY NOTHING. claude-code-polish.spec.ts alone
// is ~42 s and a file is the unit, so leg 1 is that one file and the other two
// are balanced to just under it. Every leg also pays ~60 s of fixed cost (runner,
// Postgres service, setup-go, chromium install, global-setup's three server
// builds). Two legs were measured first: 129 s / 138 s per leg, which left the
// e2e path still the longest in the run.
//
// 🔴 THE SEAM THIS GUARDS: a static partition goes stale SILENTLY. A spec added
// to tests/ and not to a leg runs in NO leg, and every leg is still green. So
// `check` refuses, each with its own message:
//   UNASSIGNED  a spec file on disk is in no leg            (the stale-partition case)
//   OVERLAP     a spec file is in more than one leg         (run twice)
//   UNKNOWN     a leg names a file that is not on disk      (a rename; the leg would
//                                                            silently run one file fewer)
//   EMPTY LEG   a leg names no files                        (a runner that proves nothing)
// The union of the legs must equal the spec list, exactly once each.
//
// ⚠ WHAT THIS CANNOT SEE: that the legs RAN what legs.json says. That is the
// other half of the seam, and verdict.mjs closes it from the reports themselves
// (`--leg N` / `--all`): the files a report shows must equal the files assigned.
//
// Usage:
//   node legs.mjs check         # the guard, against e2e/tests/ on disk
//   node legs.mjs --self-test   # prove each refusal fires, and a clean split passes
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));

export function loadLegs(file = path.join(here, 'legs.json')) {
  return JSON.parse(fs.readFileSync(file, 'utf8'));
}

// Every spec Playwright's chromium project would collect: tests/**/*.spec.ts,
// relative to e2e/ — the same spelling the JSON report's `file` uses.
export function specFiles(root = here) {
  const out = [];
  const walk = (rel) => {
    for (const d of fs.readdirSync(path.join(root, rel), { withFileTypes: true })) {
      const r = `${rel}/${d.name}`;
      if (d.isDirectory()) walk(r);
      else if (d.name.endsWith('.spec.ts')) out.push(r);
    }
  };
  walk('tests');
  return out.sort();
}

// checkLegs returns one message per violation; empty means the union of the
// legs is exactly `specs`, each once.
export function checkLegs(legs, specs) {
  const errs = [];
  const owner = new Map();
  for (const [leg, files] of Object.entries(legs)) {
    if (!Array.isArray(files) || files.length === 0) {
      errs.push(`EMPTY LEG: leg ${leg} names no spec files`);
      continue;
    }
    for (const f of files) {
      if (owner.has(f)) errs.push(`OVERLAP: ${f} is in leg ${owner.get(f)} AND leg ${leg} — it would run twice`);
      else owner.set(f, leg);
    }
  }
  const onDisk = new Set(specs);
  for (const f of specs) {
    if (!owner.has(f)) errs.push(`UNASSIGNED: ${f} is in no leg — add it to e2e/legs.json or it runs nowhere in CI`);
  }
  for (const f of owner.keys()) {
    if (!onDisk.has(f)) errs.push(`UNKNOWN: leg ${owner.get(f)} names ${f}, which is not a spec file on disk`);
  }
  return errs;
}

function selfTest() {
  const specs = ['tests/a.spec.ts', 'tests/b.spec.ts', 'tests/c.spec.ts'];
  const cases = [
    ['a clean split', { 1: ['tests/a.spec.ts'], 2: ['tests/b.spec.ts', 'tests/c.spec.ts'] }, null],
    ['a spec in no leg', { 1: ['tests/a.spec.ts'], 2: ['tests/b.spec.ts'] }, 'UNASSIGNED: tests/c.spec.ts'],
    ['a spec in two legs', { 1: ['tests/a.spec.ts', 'tests/c.spec.ts'], 2: ['tests/b.spec.ts', 'tests/c.spec.ts'] }, 'OVERLAP: tests/c.spec.ts'],
    ['a leg naming a missing file', { 1: ['tests/a.spec.ts', 'tests/z.spec.ts'], 2: ['tests/b.spec.ts', 'tests/c.spec.ts'] }, 'UNKNOWN: leg 1 names tests/z.spec.ts'],
    ['an empty leg', { 1: ['tests/a.spec.ts', 'tests/b.spec.ts', 'tests/c.spec.ts'], 2: [] }, 'EMPTY LEG: leg 2'],
  ];
  let bad = 0;
  for (const [name, legs, want] of cases) {
    const errs = checkLegs(legs, specs);
    // Exactly the one expected refusal — so a case cannot pass on a DIFFERENT
    // rule's message (green for the wrong reason).
    const ok = want === null ? errs.length === 0 : errs.length === 1 && errs[0].startsWith(want);
    console.log(`${ok ? 'ok  ' : 'FAIL'} ${name}: ${errs.length ? errs.join(' | ') : '(no refusal)'}`);
    if (!ok) bad++;
  }
  // Positive control on the disk walk: the real tree must yield spec files, or
  // `check` below would compare an empty list and say nothing.
  const real = specFiles();
  const okWalk = real.length > 0;
  console.log(`${okWalk ? 'ok  ' : 'FAIL'} the disk walk finds spec files: ${real.length}`);
  if (!okWalk) bad++;
  if (bad) {
    console.error(`legs.mjs self-test: ${bad} control(s) misbehaved — the guard cannot vouch`);
    process.exit(2);
  }
  console.log(`legs.mjs self-test: ${cases.length + 1} controls behaved`);
}

function check() {
  const { legs, seconds = {} } = loadLegs();
  const specs = specFiles();
  const errs = checkLegs(legs, specs);
  for (const [leg, files] of Object.entries(legs)) {
    const est = (files ?? []).reduce((s, f) => s + (seconds[f] ?? 0), 0);
    console.log(`leg ${leg}: ${(files ?? []).length} spec files, ~${est.toFixed(1)} s measured`);
  }
  const assigned = Object.values(legs).reduce((n, f) => n + (f?.length ?? 0), 0);
  console.log(`${specs.length} spec files on disk, ${assigned} assignments across ${Object.keys(legs).length} legs`);
  if (specs.length === 0) {
    console.error('FAIL: no spec files found under e2e/tests — the guard is comparing nothing');
    process.exit(2);
  }
  if (errs.length) {
    for (const e of errs) console.error(`FAIL: ${e}`);
    process.exit(1);
  }
  console.log('ok: the legs cover every spec file exactly once');
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const arg = process.argv[2];
  if (arg === '--self-test') selfTest();
  else if (arg === 'check') check();
  else {
    console.error('usage: node legs.mjs check | --self-test');
    process.exit(2);
  }
}
