// node --test e2e/pick-legs.test.mjs — pick-legs.mjs's choice and refusals,
// each on a synthetic artifact directory.
import { after, test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { pick } from './pick-legs.mjs';

const RUN = { id: '4242', attempt: 3, sha: 'c0ffee1' };

// fixture builds a legs.json root and an artifact dir. Each artifact is
// [dirName, provenance | null, hasResults]; its results.json names the
// artifact, so the chosen path is checked by CONTENT, not just by name.
// Every fixture's temp dir is removed when the file's tests are done.
const roots = [];
after(() => roots.forEach((r) => fs.rmSync(r, { recursive: true, force: true })));
function fixture(artifacts) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'pick-legs-'));
  roots.push(root);
  fs.writeFileSync(path.join(root, 'legs.json'), JSON.stringify({ legs: { 1: ['tests/a.spec.ts'], 2: ['tests/b.spec.ts'] } }));
  const dir = path.join(root, 'legs');
  fs.mkdirSync(dir);
  for (const [name, prov, results = true] of artifacts) {
    fs.mkdirSync(path.join(dir, name));
    if (prov) fs.writeFileSync(path.join(dir, name, 'provenance.json'), JSON.stringify(prov));
    if (results) fs.writeFileSync(path.join(dir, name, 'results.json'), JSON.stringify({ from: name }));
  }
  return { root, dir };
}
const prov = (leg, attempt, over = {}) => ({ leg: String(leg), run_id: RUN.id, run_attempt: String(attempt), sha: RUN.sha, ...over });
const art = (leg, attempt, over) => [`e2e-results-leg-${leg}-attempt-${attempt}`, prov(leg, attempt, over)];
const chosen = (paths) => paths.map((p) => JSON.parse(fs.readFileSync(p, 'utf8')).from);

test('a partial rerun: each leg is read from its HIGHEST attempt, never a stale one', () => {
  // Leg 1 passed in attempt 1 and was not rerun; leg 2 failed in 1, was rerun
  // in 2 (failed again) and in 3. Attempts are deliberately non-monotonic in
  // directory order.
  const { root, dir } = fixture([art(2, 3), art(1, 1), art(2, 1), art(2, 2)]);
  const { paths, lines } = pick(dir, RUN, root);
  assert.deepEqual(chosen(paths), ['e2e-results-leg-1-attempt-1', 'e2e-results-leg-2-attempt-3']);
  assert.match(lines[1], /^leg 2: attempt 3 of 3 \(ignoring the older /);
});

test('an artifact without an attempt in its name is refused, not read', () => {
  const { root, dir } = fixture([art(1, 1), art(2, 1), ['e2e-results-leg-2', prov(2, 1)]]);
  assert.throws(() => pick(dir, RUN, root), /UNBOUND ARTIFACT: e2e-results-leg-2 is not/);
});

test('a leg with no report is refused', () => {
  const { root, dir } = fixture([art(1, 1)]);
  assert.throws(() => pick(dir, RUN, root), /NO LEG REPORT: leg 2 has no artifact/);
});

test('a leg legs.json does not have is refused', () => {
  const { root, dir } = fixture([art(1, 1), art(2, 1), art(7, 1)]);
  assert.throws(() => pick(dir, RUN, root), /UNKNOWN LEG: e2e-results-leg-7-attempt-1 is leg 7/);
});

test('an attempt above this run\'s is refused', () => {
  const { root, dir } = fixture([art(1, 1), art(2, 4)]);
  assert.throws(() => pick(dir, RUN, root), /FUTURE ATTEMPT: e2e-results-leg-2-attempt-4 is attempt 4, this is attempt 3/);
});

for (const [field, value] of [['sha', 'deadbee'], ['run_id', '999'], ['run_attempt', '1'], ['leg', '1']]) {
  test(`a provenance whose ${field} disagrees is refused`, () => {
    const { root, dir } = fixture([art(1, 1), art(2, 2, { [field]: value })]);
    assert.throws(() => pick(dir, RUN, root), new RegExp(`PROVENANCE MISMATCH: e2e-results-leg-2-attempt-2 says ${field}="${value}"`));
  });
}

test('a missing provenance or report is refused', () => {
  const a = fixture([art(1, 1), ['e2e-results-leg-2-attempt-2', null]]);
  assert.throws(() => pick(a.dir, RUN, a.root), /NO PROVENANCE: e2e-results-leg-2-attempt-2\/provenance.json/);
  const b = fixture([art(1, 1), ['e2e-results-leg-2-attempt-2', prov(2, 2), false]]);
  assert.throws(() => pick(b.dir, RUN, b.root), /NO REPORT: e2e-results-leg-2-attempt-2\/results.json is missing/);
});

test('a stale older attempt with no report (a leg that died before Playwright wrote one) does not block a complete newer attempt', () => {
  // Leg 2's attempt 1 failed in `npm ci` / `playwright install`: its upload
  // ran (if: always()) with provenance.json and no results.json. Attempt 2 is
  // complete. Only the newest attempt is the leg's verdict.
  const { root, dir } = fixture([art(1, 1), ['e2e-results-leg-2-attempt-1', prov(2, 1), false], art(2, 2)]);
  const { paths } = pick(dir, RUN, root);
  assert.deepEqual(chosen(paths), ['e2e-results-leg-1-attempt-1', 'e2e-results-leg-2-attempt-2']);
});

test('a stale older attempt with a bad or missing provenance does not block a complete newer attempt', () => {
  const { root, dir } = fixture([art(1, 1), ['e2e-results-leg-2-attempt-1', null, false], art(2, 2)]);
  assert.deepEqual(chosen(pick(dir, RUN, root).paths), ['e2e-results-leg-1-attempt-1', 'e2e-results-leg-2-attempt-2']);
  const b = fixture([art(1, 1), art(2, 1, { sha: 'deadbee' }), art(2, 2)]);
  assert.deepEqual(chosen(pick(b.dir, RUN, b.root).paths), ['e2e-results-leg-1-attempt-1', 'e2e-results-leg-2-attempt-2']);
});

test('the NEWEST attempt is still checked: one with no report is refused, whatever an older one held', () => {
  const { root, dir } = fixture([art(1, 1), art(2, 1), ['e2e-results-leg-2-attempt-2', prov(2, 2), false]]);
  assert.throws(() => pick(dir, RUN, root), /NO REPORT: e2e-results-leg-2-attempt-2\/results.json is missing/);
});

// A partial rerun: leg 1 was not rerun, so its chosen artifact is from an
// OLDER attempt than the run's newest. It is still the leg's verdict and is
// checked like any other chosen artifact.
test('a leg chosen from an older attempt than the newest is still checked: no report is refused', () => {
  const { root, dir } = fixture([['e2e-results-leg-1-attempt-1', prov(1, 1), false], art(2, 2)]);
  assert.throws(() => pick(dir, RUN, root), /NO REPORT: e2e-results-leg-1-attempt-1\/results.json is missing/);
});

test('a leg chosen from an older attempt than the newest is still checked: a foreign sha is refused', () => {
  const { root, dir } = fixture([art(1, 1, { sha: 'deadbee' }), art(2, 2)]);
  assert.throws(() => pick(dir, RUN, root), /PROVENANCE MISMATCH: e2e-results-leg-1-attempt-1 says sha="deadbee"/);
});
