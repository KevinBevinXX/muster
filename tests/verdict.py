#!/usr/bin/env python3
"""Read `go test -json` and refuse a run that skipped, ran nothing, failed, or
was cut off.

    rc=0
    go test -json ./... > out.json || rc=$?
    python3 tests/verdict.py --exit-code "$rc" --echo summary out.json

    go test -json ./... | python3 tests/verdict.py      # still works: stdin

🔴 WHY THIS EXISTS AT ALL: `go test` EXITS 0 WITH SKIPPED TESTS. So the exit
status of a test run cannot distinguish "everything passed" from "everything
skipped", and muster's Postgres-backed tests default to skipping when they
cannot find a database. MUSTER_TEST_REQUIRE_DB=1 is supposed to make a skip
impossible; this is the independent read that says whether it did.

🔴 IT MUST READ THE RUN THAT SET THE JOB'S STATUS, NOT A SECOND ONE. CI used to
run the suite once for the exit code and again with `-json` for this script,
on the theory that Go's test cache would answer the second run. MEASURED on
GitHub-hosted runners: it did not — the re-run cost 42 s in `test` and 65 s in
`ccd-tmux` (the latter matching the suite's own real time), so every verdict
was about a SECOND execution, which a flaky skip or failure can differ on. CI
now writes ONE `-json` run to a file, keeps its exit code, and hands both here.

🔴 THE EXIT CODE WINS. `--exit-code` is that run's status, and a non-zero one is
a refusal whatever the events say. A `fail` event — for a test, or for a
package whose tests all passed (a TestMain exiting non-zero) — is refused on
its own too, so a red run is red by two independent reads; the status is the
one that still holds when the events look clean.

🔴 IT REPORTS THE PAIR, NEVER JUST THE ZERO. "0 skipped" is indistinguishable
from a parser wired to nothing, so the count of tests that produced a VERDICT is
printed beside it and a zero there is a failure. That is the positive control:
the number has to be able to move. The same goes for completeness: the count of
packages that STARTED is printed beside the count that FINISHED, and zero
started is a refusal.

⚠ IT REFUSES A STREAM THAT WAS CUT OFF. A package that emitted `start` with no
package-level pass/fail/skip, or a test that emitted `run` with no verdict, is
an output that ended early (a killed process, a truncated file). Without this,
a file truncated after N verdicts would pass on those N. ⚠ Its blind spot: a
cut that lands exactly BETWEEN packages, before the next one's `start`, leaves
a complete-looking stream of fewer packages. A killed run is still refused by
its exit code; a file truncated after the fact at that boundary is not.

⚠ IT COUNTS TESTS, NOT PACKAGES. `go test -json` emits an event per package too,
with no "Test" key, and counting those would inflate every number here by the
package count — a plausible-looking total that is not what anybody means.

`--echo` re-renders the human-readable log from the same file, so the step's
output is not lost by capturing `-json`:
  none     (default) print only the accounting below
  summary  what plain `go test` prints: package lines (ok/FAIL/coverage), build
           output, and the full output of every test that failed or skipped
  all      every output line, which is what `go test -v` prints

Exit codes:
  0  every test produced a verdict, none skipped, the stream is complete, and
     the run's exit code (if given) was 0
  1  something skipped, nothing ran, the stream was cut off, or the run failed
     (by its exit code, or by a `fail` event for a test or a package)
  2  the input could not be read as `go test -json` output
"""

from __future__ import annotations

import argparse
import json
import sys


def main(stream, exit_code: int | None = None, echo: str = "none", out=None) -> int:
    out = out if out is not None else sys.stdout
    verdicts: list[str] = []
    failed: list[str] = []
    skipped: list[str] = []
    started_pkgs: set[str] = set()
    finished_pkgs: set[str] = set()
    failed_pkgs: set[str] = set()
    running: set[str] = set()
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
            if echo != "none":
                out.write(e.get("Output", ""))
            continue

        if not test:
            # A package-level event. See the note in the docstring.
            if action == "start":
                started_pkgs.add(pkg)
            elif action in ("pass", "fail", "skip"):
                finished_pkgs.add(pkg)
                if action == "fail":
                    failed_pkgs.add(pkg)
            elif action == "output" and echo != "none":
                out.write(e.get("Output", ""))
            continue

        name = f"{pkg}.{test}"
        if action == "run":
            running.add(name)
        elif action == "output":
            if echo == "all":
                out.write(e.get("Output", ""))
            elif echo == "summary":
                buffered.setdefault(name, []).append(e.get("Output", ""))
        elif action in ("pass", "fail", "skip"):
            running.discard(name)
            if action == "skip":
                skipped.append(name)
            else:
                verdicts.append(name)
                if action == "fail":
                    failed.append(name)
            text = buffered.pop(name, [])
            if echo == "summary" and action != "pass":
                out.write("".join(text))

    if lines == 0:
        print("verdict: COULD NOT RUN — the input was empty. Give this `go test -json` output.",
              file=sys.stderr)
        return 2
    if parsed == 0:
        print(f"verdict: COULD NOT RUN — {lines} line(s) of input and NONE of them "
              f"parsed as JSON. This is not `go test -json` output.", file=sys.stderr)
        return 2

    unfinished_pkgs = sorted(started_pkgs - finished_pkgs)
    if echo == "summary":
        # A test cut off mid-run has no verdict to trigger the flush above.
        for name in sorted(running):
            out.write("".join(buffered.get(name, [])))

    print(file=out)
    print(f"tests that produced a verdict: {len(verdicts)}", file=out)
    print(f"tests that FAILED:             {len(failed)}", file=out)
    print(f"tests that SKIPPED:            {len(skipped)}", file=out)
    print(f"packages started / finished:   {len(started_pkgs)} / "
          f"{len(finished_pkgs & started_pkgs)}", file=out)
    print(f"packages that FAILED:          {len(failed_pkgs)}", file=out)
    if exit_code is not None:
        print(f"go test exit code:             {exit_code}", file=out)

    refused = False

    if exit_code:
        refused = True
        print(file=out)
        print(f"FAIL: go test exited {exit_code}. The exit code wins: whatever the "
              "counts above say, the run that set this job's status failed.", file=out)

    if failed or failed_pkgs:
        refused = True
        print(file=out)
        print("FAIL: the stream itself records a failure. A package can fail with "
              "every one of its tests passing (a build error, a TestMain that exits "
              "non-zero, a panic outside a test):", file=out)
        for p in sorted(failed_pkgs):
            print(f"  FAILED PACKAGE  {p}", file=out)
        for n in sorted(failed):
            print(f"  FAILED TEST     {n}", file=out)

    if not verdicts:
        refused = True
        print(file=out)
        print("FAIL: zero tests produced a verdict. Whatever the exit code of the run "
              "was, it is a claim about nothing.", file=out)

    if not started_pkgs:
        refused = True
        print(file=out)
        print("FAIL: no package emitted a `start` event, so whether this stream is "
              "complete cannot be checked. Every `go test -json` since Go 1.20 emits one.",
              file=out)

    if unfinished_pkgs or running:
        refused = True
        print(file=out)
        print("FAIL: the stream was CUT OFF — these started and never finished. A "
              "truncated file or a killed run, not a verdict:", file=out)
        for p in unfinished_pkgs:
            print(f"  UNFINISHED PACKAGE  {p}", file=out)
        for n in sorted(running):
            print(f"  UNFINISHED TEST     {n}", file=out)

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


def cli(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("file", nargs="?", help="`go test -json` output (default: stdin)")
    ap.add_argument("--exit-code", type=int, default=None,
                    help="the exit status of the go test run that wrote the input")
    ap.add_argument("--echo", choices=("none", "summary", "all"), default="none",
                    help="re-render the human-readable log from the JSON")
    args = ap.parse_args(argv)
    if args.file is None:
        return main(sys.stdin, args.exit_code, args.echo)
    try:
        f = open(args.file, encoding="utf-8", errors="replace")
    except OSError as err:
        print(f"verdict: COULD NOT RUN — {err}", file=sys.stderr)
        return 2
    with f:
        return main(f, args.exit_code, args.echo)


if __name__ == "__main__":
    raise SystemExit(cli(sys.argv[1:]))
