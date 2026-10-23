#!/usr/bin/env python3
"""scripts/with-test-pg.sh's contract, against a REAL Postgres.

    nix develop -c python3 tests/with_test_pg_test.py

Needs the dev shell's initdb/pg_ctl/psql (it fails rather than skips without
them — a helper whose test cannot run has not been tested). Migration is skipped
(MUSTER_TEST_PG_SKIP_MIGRATE=1) so the cases measure the helper, not the schema.

Each case asserts the helper's OWN observable: the exported DSN reaches a live
server, the command's exit code passes through unchanged (7, a value nothing in
the helper produces), and after success, failure AND a SIGTERM the server is
gone and the temp directory is deleted. Every cleanup check first asserts the
directory EXISTED while the command ran (positive control), so a helper that
never created one cannot pass by having nothing to clean.
"""

from __future__ import annotations

import os
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

REPO = Path(__file__).resolve().parents[1]
HELPER = REPO / "scripts" / "with-test-pg.sh"


def port_open(port: int) -> bool:
    with socket.socket() as s:
        s.settimeout(1)
        return s.connect_ex(("127.0.0.1", port)) == 0


class WithTestPg(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        missing = [b for b in ("initdb", "pg_ctl", "psql") if shutil.which(b) is None]
        if missing:
            raise AssertionError(f"{missing} not on PATH — run inside `nix develop`; this test must not skip")

    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="wtp-test-")
        self.env = dict(os.environ, TMPDIR=self.tmp, MUSTER_TEST_PG_SKIP_MIGRATE="1")
        self.env.pop("MUSTER_TEST_DATABASE_URL", None)

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def leftovers(self) -> list[str]:
        return sorted(p.name for p in Path(self.tmp).iterdir() if p.name.startswith("muster-test-pg."))

    def run_helper(self, script: str) -> subprocess.CompletedProcess:
        return subprocess.run([str(HELPER), "bash", "-c", script], env=self.env,
                              capture_output=True, text=True, timeout=180)

    def test_exports_a_live_dsn_and_cleans_up_after_success(self):
        report = Path(self.tmp) / "report"
        p = self.run_helper(
            f'set -e; echo "$MUSTER_TEST_PG_PORT $MUSTER_TEST_REQUIRE_DB" > {report}; '
            f'ls -d "$TMPDIR"/muster-test-pg.* >> {report}; '
            'psql "$MUSTER_TEST_DATABASE_URL" -tAc "select current_database()" >> ' + str(report))
        self.assertEqual(p.returncode, 0, p.stderr)
        port, require, existed, db = report.read_text().split()
        self.assertEqual(require, "1")
        self.assertTrue(existed.startswith(self.tmp), "positive control: the temp dir existed during the run")
        self.assertEqual(db, "muster_test")
        self.assertEqual(self.leftovers(), [])
        self.assertFalse(port_open(int(port)), f"Postgres still listening on {port}")

    def test_command_exit_code_passes_through_and_failure_cleans_up(self):
        report = Path(self.tmp) / "report"
        p = self.run_helper(f'echo "$MUSTER_TEST_PG_PORT" > {report}; ls -d "$TMPDIR"/muster-test-pg.* >> {report}; exit 7')
        self.assertEqual(p.returncode, 7, p.stderr)
        port, existed = report.read_text().split()
        self.assertTrue(existed.startswith(self.tmp), "positive control")
        self.assertEqual(self.leftovers(), [])
        self.assertFalse(port_open(int(port)))

    def test_sigterm_stops_postgres_and_cleans_up(self):
        ready = Path(self.tmp) / "ready"
        proc = subprocess.Popen([str(HELPER), "bash", "-c",
                                 f'echo "$MUSTER_TEST_PG_PORT" > {ready}.t && mv {ready}.t {ready}; sleep 300'],
                                env=self.env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            deadline = time.monotonic() + 120
            while not ready.exists():
                # The message is built only on failure: reading stderr blocks until EOF.
                if proc.poll() is not None:
                    self.fail(f"helper exited early ({proc.returncode}): {proc.stderr.read() if proc.stderr else ''}")
                self.assertLess(time.monotonic(), deadline, "command never started")
                time.sleep(0.2)
            port = int(ready.read_text())
            self.assertTrue(port_open(port), "positive control: Postgres was listening before the signal")
            self.assertEqual(len(self.leftovers()), 1, "positive control: one temp dir before the signal")
            t0 = time.monotonic()
            proc.send_signal(signal.SIGTERM)
            rc = proc.wait(timeout=60)
            # Not after the 300 s sleep: the trap must run at once.
            self.assertLess(time.monotonic() - t0, 30)
        finally:
            if proc.poll() is None:
                proc.kill()
        self.assertEqual(rc, 128 + signal.SIGTERM)
        self.assertEqual(self.leftovers(), [])
        self.assertFalse(port_open(port))

    def test_no_command_is_a_usage_error(self):
        p = subprocess.run([str(HELPER)], env=self.env, capture_output=True, text=True)
        self.assertEqual(p.returncode, 2)
        self.assertEqual(self.leftovers(), [])


if __name__ == "__main__":
    unittest.main()
