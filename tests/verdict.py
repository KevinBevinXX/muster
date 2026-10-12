#!/usr/bin/env python3
"""Read `go test -json` on stdin; re-render the log, and refuse a run that
skipped, ran nothing, or recorded a failure.

    set -o pipefail
    go test -json ./... | python3 tests/verdict.py --echo summary

🔴 WHY THIS EXISTS AT ALL: `go test` EXITS 0 WITH SKIPPED TESTS. So the exit
status of a test run cannot distinguish "everything passed" from "everything
skipped", and muster's Postgres-backed tests default to skipping when they
cannot find a database. MUSTER_TEST_REQUIRE_DB=1 is supposed to make a skip
impossible; this is the independent read that says whether it did.

🔴 IT MUST READ THE RUN THAT SET THE JOB'S STATUS, NOT A SECOND ONE. CI used to
run the suite once for the exit code and again with `-json` for this script,
on the theory that Go's test cache would answer the second run. MEASURED on
GitHub-hosted runners: the re-run cost 42 s in `test` and 65 s in `ccd-tmux`,
and every verdict was about a SECOND execution. CI now pipes ONE run into this
script under `set -o pipefail`, so the step fails if EITHER side does.

🔴 THE RUN'S OWN EXIT STATUS IS NOT THIS SCRIPT'S TO SEE — `pipefail` CARRIES
IT. What this script adds is a second, independent read: a `fail` event, for a
test, for a package whose tests all passed (a TestMain exiting non-zero), or a
`build-fail` (which can arrive with NO package `fail` beside it), is refused
and NAMED here. If someone drops `pipefail`, that read still stands;
what is lost is only a run that exits non-zero over a stream with no `fail`
event in it.

🔴 IT REPORTS THE PAIR, NEVER JUST THE ZERO. "0 skipped" is indistinguishable
from a parser wired to nothing, so the count of tests that produced a VERDICT is
printed beside it and a zero there is a failure. That is the positive control:
the number has to be able to move.

⚠ IT COUNTS TESTS, NOT PACKAGES. `go test -json` emits an event per package too,
with no "Test" key, and counting those would inflate every number here by the
package count — a plausible-looking total that is not what anybody means.

`--echo` (required) re-renders the human-readable log that `-json` replaced:
  summary  what plain `go test` prints: package lines (ok/FAIL/coverage), build
           output, and the full output of every test that did not pass
  all      every output line, which is what `go test -v` prints

⚠ WHAT THIS DOES NOT DO: in `summary` mode, a line a test binary writes raw to
stderr can be attributed by `go test -json` to a neighbouring test that then
passes, and is dropped with that test's output; `--echo all` shows everything.

Exit codes:
  0  every test produced a verdict, none skipped, nothing failed
  1  something skipped, nothing ran, or a test, package or build failed
  2  the input could not be read as `go test -json` output
"""

from __future__ import annotations

import argparse
import json
import sys


def main(stream, echo: str, out=None) -> int:
    out = out if out is not None else sys.stdout
    verdicts: list[str] = []
    failed: list[str] = []
    skipped: list[str] = []
    failed_pkgs: list[str] = []
    failed_builds: list[str] = []
    buffered: dict[str, list[str]] = {}
    lines = 0
    parsed = 0
    for line in stream:
        line = line.strip()
        if not line:
            continue
        lines += 1
        try:
            e = json.loads(line)
        except ValueError:
            # `go test -json` interleaves build output that is not JSON. That is
            # normal and not an error — but if NOTHING parses, the input is not
            # what this script thinks it is, and that is checked below.
            continue
        if not isinstance(e, dict):
            continue
        parsed += 1
        action = e.get("Action")
        pkg = e.get("Package", "?")
        test = e.get("Test")

        if action == "build-output":
            # Go >= 1.24 reports compiler output as events keyed by ImportPath.
            out.write(e.get("Output", ""))
            continue
        if action == "build-fail":
            # 🔴 NOT IMPLIED BY A PACKAGE `fail`. MEASURED with `go test -json -o
            # <unwritable dir>`: every test passes, the package event is `pass`,
            # go exits 1, and this is the only failure in the stream.
            failed_builds.append(e.get("ImportPath", "?"))
            continue

        if not test:
            # A package-level event. See the note in the docstring.
            if action == "output":
                out.write(e.get("Output", ""))
            elif action == "fail":
                failed_pkgs.append(pkg)
            continue

        name = f"{pkg}.{test}"
        if action == "output":
            if echo == "all":
                out.write(e.get("Output", ""))
            else:
                buffered.setdefault(name, []).append(e.get("Output", ""))
        elif action in ("pass", "fail", "skip"):
            if action == "skip":
                skipped.append(name)
            else:
                verdicts.append(name)
                if action == "fail":
                    failed.append(name)
            text = buffered.pop(name, [])
            if action != "pass":
                out.write("".join(text))

    # A test with output and no verdict was cut off mid-run (a timeout, a kill);
    # its output is the only account of why, so it is not dropped.
    for text in buffered.values():
        out.write("".join(text))

    if lines == 0:
        print("verdict: COULD NOT RUN — stdin was empty. Pipe `go test -json` into this.",
              file=sys.stderr)
        return 2
    if parsed == 0:
        print(f"verdict: COULD NOT RUN — {lines} line(s) on stdin and NONE of them "
              f"parsed as JSON. This is not `go test -json` output.", file=sys.stderr)
        return 2

    print(file=out)
    print(f"tests that produced a verdict: {len(verdicts)}", file=out)
    print(f"tests that FAILED:             {len(failed)}", file=out)
    print(f"tests that SKIPPED:            {len(skipped)}", file=out)
    print(f"packages that FAILED:          {len(failed_pkgs)}", file=out)
    print(f"builds that FAILED:            {len(failed_builds)}", file=out)

    refused = False

    if failed or failed_pkgs or failed_builds:
        refused = True
        print(file=out)
        print("FAIL: the stream records a failure. A package can fail with every one "
              "of its tests passing (a build error, a TestMain that exits non-zero, a "
              "panic outside a test):", file=out)
        for b in sorted(failed_builds):
            print(f"  FAILED BUILD    {b}", file=out)
        for p in sorted(failed_pkgs):
            print(f"  FAILED PACKAGE  {p}", file=out)
        for n in sorted(failed):
            print(f"  FAILED TEST     {n}", file=out)

    if not verdicts:
        refused = True
        print(file=out)
        print("FAIL: zero tests produced a verdict. Whatever the exit code of the run "
              "was, it is a claim about nothing.", file=out)

    if skipped:
        refused = True
        print(file=out)
        print("FAIL: tests skipped in a run that was told to require a database.", file=out)
        print("Every skip here is a test nobody ran and nobody was told about:", file=out)
        for n in sorted(skipped):
            print(f"  SKIPPED  {n}", file=out)
        print(file=out)
        print("Either MUSTER_TEST_REQUIRE_DB did not reach them — check that they take "
              "their DSN from dbtest.Base/dbtest.DSN and not from os.Getenv directly — "
              "or they skip for a reason of their own that needs stating.", file=out)

    if refused:
        return 1

    print(file=out)
    print("ok: every test produced a verdict, and the count above is what makes that "
          "a measurement rather than a silence", file=out)
    return 0


if __name__ == "__main__":
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--echo", choices=("summary", "all"), required=True,
                    help="how much of the human-readable log to re-render")
    raise SystemExit(main(sys.stdin, ap.parse_args().echo))
