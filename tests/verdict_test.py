#!/usr/bin/env python3
"""verdict.py's refusals, one synthetic `go test -json` stream each.

    python3 tests/verdict_test.py

Every refusing case differs from GREEN in exactly ONE respect, and asserts the
message of the refusal it is about — so a case cannot pass because a different
guard happened to reject the same stream. The streams are synthetic: the shape
of `go test -json` events, no captured output.
"""

from __future__ import annotations

import io
import json
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import verdict  # noqa: E402

PKG = "example.test/pkg"


def ev(action: str, test: str | None = None, output: str | None = None) -> str:
    e: dict = {"Action": action, "Package": PKG}
    if test:
        e["Test"] = test
    if output is not None:
        e["Output"] = output
    return json.dumps(e)


def stream(*tests: tuple[str, str], pkg_end: str = "pass") -> list[str]:
    """One package: `start`, each test's run/output/verdict, then the package's."""
    lines = [ev("start")]
    for name, action in tests:
        lines += [ev("run", name), ev("output", name, f"=== RUN   {name}\n"),
                  ev("output", name, f"--- {action.upper()}: {name}\n"), ev(action, name)]
    return lines + [ev("output", output=f"{'ok' if pkg_end == 'pass' else 'FAIL'}\t{PKG}\n"),
                    ev(pkg_end)]


GREEN = stream(("TestA", "pass"), ("TestB", "pass"))

# What Go >= 1.24 emits for a build that failed: keyed by ImportPath, no Package.
BUILD_OUTPUT = json.dumps({"ImportPath": f"{PKG}.test", "Action": "build-output",
                           "Output": "x.go:1:1: synthetic compiler error\n"})
BUILD_FAIL = json.dumps({"ImportPath": f"{PKG}.test", "Action": "build-fail"})


def run(lines: list[str], echo: str = "summary") -> tuple[int, str]:
    out = io.StringIO()
    return verdict.main(iter(lines), echo, out=out), out.getvalue()


class Verdict(unittest.TestCase):
    def refuses(self, lines: list[str], needle: str) -> str:
        rc, text = run(lines)
        self.assertEqual(rc, 1, text)
        self.assertIn(needle, text)
        return text

    def test_green_reports_the_pair(self):
        rc, text = run(GREEN)
        self.assertEqual(rc, 0, text)
        self.assertIn("tests that produced a verdict: 2", text)
        self.assertIn("tests that SKIPPED:            0", text)

    def test_a_skip_is_refused_and_named(self):
        text = self.refuses(stream(("TestA", "pass"), ("TestB", "skip")), "FAIL: tests skipped")
        self.assertIn(f"SKIPPED  {PKG}.TestB", text)
        # The skip's own output carries its REASON; the list line above does not.
        self.assertIn("--- SKIP: TestB\n", text)
        self.assertNotIn("records a failure", text)

    def test_zero_verdicts_is_refused(self):
        self.refuses(stream(), "FAIL: zero tests produced a verdict")

    def test_a_failed_test_is_refused_and_named(self):
        # The package event says `pass` here, which go never emits beside a failed
        # test — it isolates the test-level read from the package-level one.
        text = self.refuses(stream(("TestA", "fail")), f"FAILED TEST     {PKG}.TestA")
        self.assertNotIn("FAILED PACKAGE", text)

    def test_a_failed_package_with_passing_tests_is_refused(self):
        text = self.refuses(stream(("TestA", "pass"), pkg_end="fail"), f"FAILED PACKAGE  {PKG}")
        self.assertNotIn("FAILED TEST", text)

    def test_a_bare_build_fail_is_refused_and_named(self):
        # MEASURED with `go test -json -o <unwritable>`: the tests pass, the package
        # says `pass`, and the ONLY failure in the stream is a `build-fail`.
        text = self.refuses(GREEN + [BUILD_FAIL], f"FAILED BUILD    {PKG}.test")
        self.assertNotIn("FAILED PACKAGE", text)
        self.assertNotIn("FAILED TEST", text)

    def test_build_output_is_echoed_in_both_modes(self):
        # Under `-json` this is the only way a compiler error reaches the log.
        for echo in ("summary", "all"):
            _, text = run(GREEN + [BUILD_OUTPUT], echo=echo)
            self.assertIn("x.go:1:1: synthetic compiler error\n", text, echo)

    def test_empty_and_non_json_input_cannot_vouch(self):
        self.assertEqual(run([])[0], 2)
        self.assertEqual(run(["", "  "])[0], 2)
        self.assertEqual(run(["not json", "ok  \tpkg\t0.1s"])[0], 2)

    def test_echo_summary_shows_package_lines_and_non_passing_output_only(self):
        lines = stream(("TestA", "pass"), ("TestB", "fail"), pkg_end="fail")
        _, text = run(lines, echo="summary")
        self.assertIn("--- FAIL: TestB\n", text)
        self.assertIn(f"FAIL\t{PKG}\n", text)
        self.assertNotIn("=== RUN   TestA", text)

    def test_echo_summary_keeps_the_output_of_a_test_cut_off_mid_run(self):
        cut = [ev("start"), ev("run", "TestA"), ev("output", "TestA", "panic: test timed out\n")]
        _, text = run(cut, echo="summary")
        self.assertIn("panic: test timed out\n", text)

    def test_echo_all_shows_every_line(self):
        _, text = run(GREEN, echo="all")
        self.assertIn("=== RUN   TestA\n", text)
        self.assertIn("--- PASS: TestB\n", text)
        self.assertIn(f"ok\t{PKG}\n", text)


if __name__ == "__main__":
    unittest.main()
