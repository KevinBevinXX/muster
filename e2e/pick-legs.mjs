#!/usr/bin/env node
// pick-legs.mjs — bind the `e2e` aggregate to the leg reports of THIS run's
// latest attempt of each leg, and refuse anything it cannot bind.
//
//   node pick-legs.mjs DIR    prints one results.json path per leg (stdout)
//
// DIR holds the downloaded leg artifacts, one directory each, named
// e2e-results-leg-<leg>-attempt-<run attempt>; each carries the provenance.json
// its leg wrote (leg, run_id, run_attempt, sha). The run is read from
// GITHUB_RUN_ID, GITHUB_RUN_ATTEMPT and GITHUB_SHA.
//
// 🔴 WHY. A partial rerun (`gh run rerun --failed`) reruns only the failed legs
// and the aggregate; the legs that passed keep their earlier attempt's
// artifact. When every attempt uploaded under ONE name per leg, the run held
// two artifacts named e2e-results-leg-1 (measured: run 39534918620 after its
// rerun), download-artifact extracted both into the same directory, and the
// aggregate read whichever results.json landed last — a stale failure, or a
// stale pass. Now each attempt's artifact has a name of its own and the
// aggregate takes, per leg, the HIGHEST attempt: a leg's latest run is its
// highest attempt, and the `every leg succeeded` step has already refused the
// run unless every leg's latest run passed — and a leg that passed uploaded
// (the upload is one of its steps).
//
// Refused (exit 1, each named), over EVERY artifact (by its name): one whose
// name is not leg-<n>-attempt-<k> (e.g. one from before this binding); a leg
// not in legs.json; an attempt above this run's; a leg in legs.json with no
// artifact. Over each leg's CHOSEN (highest-attempt) artifact only: a
// provenance.json that is missing or disagrees with its artifact's name or
// with this run's id or sha; a missing results.json. Older attempts are not
// read at all.
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));

// pick returns { paths, lines } on success and throws an Error listing every
// refusal otherwise. It is the whole decision; main only prints.
export function pick(dir, run, root = here) {
  const legs = Object.keys(JSON.parse(fs.readFileSync(path.join(root, 'legs.json'), 'utf8')).legs);
  const bad = [];
  const best = new Map(); // leg -> { attempt, dir }
  const entries = fs.existsSync(dir) ? fs.readdirSync(dir, { withFileTypes: true }).filter((d) => d.isDirectory()) : [];
  // 1. Date every artifact by its NAME, and take each leg's highest attempt.
  //    An artifact that cannot be dated could be the newest, so it is refused.
  for (const d of entries) {
    const m = /^e2e-results-leg-(\d+)-attempt-(\d+)$/.exec(d.name);
    if (!m) {
      bad.push(`UNBOUND ARTIFACT: ${d.name} is not e2e-results-leg-<leg>-attempt-<attempt>; its run attempt cannot be known`);
      continue;
    }
    const [leg, attempt] = [m[1], Number(m[2])];
    if (!legs.includes(leg)) bad.push(`UNKNOWN LEG: ${d.name} is leg ${leg}, not one of legs.json's [${legs.join(', ')}]`);
    if (attempt > run.attempt) bad.push(`FUTURE ATTEMPT: ${d.name} is attempt ${attempt}, this is attempt ${run.attempt}`);
    const prev = best.get(leg);
    if (!prev || attempt > prev.attempt) best.set(leg, { attempt, dir: path.join(dir, d.name), name: d.name });
  }
  for (const leg of legs) {
    if (!best.has(leg)) bad.push(`NO LEG REPORT: leg ${leg} has no artifact in ${dir}`);
  }
  // 2. Check ONLY the chosen artifact of each leg. 🔴 An older attempt is not
  //    the leg's verdict and is never read: a leg that died in attempt 1
  //    before Playwright wrote a report (npm ci, the browser install) still
  //    uploads provenance.json alone (if: always()), and checking it would
  //    refuse every later attempt of the run, a rerun of all jobs included.
  for (const [leg, b] of best) {
    if (!legs.includes(leg)) continue;
    let prov;
    try {
      prov = JSON.parse(fs.readFileSync(path.join(b.dir, 'provenance.json'), 'utf8'));
    } catch (e) {
      bad.push(`NO PROVENANCE: ${b.name}/provenance.json: ${e.message}`);
      continue;
    }
    const want = { leg, run_id: run.id, run_attempt: String(b.attempt), sha: run.sha };
    for (const [k, v] of Object.entries(want)) {
      if (String(prov[k]) !== v) bad.push(`PROVENANCE MISMATCH: ${b.name} says ${k}=${JSON.stringify(prov[k])}, want ${JSON.stringify(v)}`);
    }
    if (!fs.existsSync(path.join(b.dir, 'results.json'))) bad.push(`NO REPORT: ${b.name}/results.json is missing`);
  }
  if (bad.length) throw new Error(bad.join('\n'));
  const lines = legs.map((leg) => {
    const b = best.get(leg);
    const older = entries.filter((d) => d.name.startsWith(`e2e-results-leg-${leg}-attempt-`) && path.join(dir, d.name) !== b.dir).map((d) => d.name);
    return `leg ${leg}: attempt ${b.attempt} of ${run.attempt}${older.length ? ` (ignoring the older ${older.join(', ')})` : ''}`;
  });
  return { paths: legs.map((leg) => path.join(best.get(leg).dir, 'results.json')), lines };
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const dir = process.argv[2];
  const run = { id: process.env.GITHUB_RUN_ID ?? '', attempt: Number(process.env.GITHUB_RUN_ATTEMPT ?? NaN), sha: process.env.GITHUB_SHA ?? '' };
  if (!dir || !run.id || !Number.isInteger(run.attempt) || run.attempt < 1 || !run.sha) {
    console.error('usage: GITHUB_RUN_ID=… GITHUB_RUN_ATTEMPT=… GITHUB_SHA=… node pick-legs.mjs DIR');
    process.exit(2);
  }
  try {
    const { paths, lines } = pick(dir, run);
    for (const l of lines) console.error(l);
    for (const p of paths) console.log(p);
  } catch (e) {
    for (const l of e.message.split('\n')) console.error(`FAIL: ${l}`);
    process.exit(1);
  }
}
