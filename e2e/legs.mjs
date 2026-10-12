// legs.mjs — the e2e suite's split into CI matrix legs (the data is legs.json).
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
// ⚠ TWO LEGS, NOT THREE. Split, the e2e path is no longer the run's longest:
// `test (postgres)` is, and three legs measured no faster than two at that job's
// median while costing a runner slot on an account whose concurrency cap other
// repositories already fill.
//
// 🔴 THE SEAM — every spec in exactly one leg — IS GUARDED BY WHAT RAN, NOT BY
// THIS FILE. `verdict.mjs --all` (the `e2e` job) sums every leg's report and
// refuses a spec file on disk that is absent from all of them (NOT RUN: a spec
// in no leg, or a legs.json entry misspelled so it matches nothing) and one that
// appears in two (RAN TWICE). An empty or unknown leg throws at config load
// (playwright.config.ts). A static check on legs.json alone was measured to add
// no refusal those two do not already make, in the same job, no later.
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));

// specFiles lists every file under e2e/<spec.dir>/, at any depth, whose name
// ends in <spec.suffix> — legs.json's `spec`, the one definition of a spec file
// that playwright.config.ts's whole-suite testMatch is built from too. Paths are
// relative to e2e/, the spelling the JSON report's `file` uses. It does NOT list
// anything else Playwright might collect under another rule (the `capture`
// project's specs, for one); the chromium project collects exactly this set.
export function specFiles(root = here) {
  const { spec } = JSON.parse(fs.readFileSync(path.join(root, 'legs.json'), 'utf8'));
  const out = [];
  const walk = (rel) => {
    for (const d of fs.readdirSync(path.join(root, rel), { withFileTypes: true })) {
      const r = `${rel}/${d.name}`;
      if (d.isDirectory()) walk(r);
      else if (d.name.endsWith(spec.suffix)) out.push(r);
    }
  };
  walk(spec.dir);
  return out.sort();
}
