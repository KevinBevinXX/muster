#!/usr/bin/env python3
"""scripts/with-test-pg.sh's contract, against a REAL Postgres.

    nix develop -c python3 tests/with_test_pg_test.py

Needs the dev shell's initdb/pg_ctl/psql (it fails rather than skips without
them — a helper whose test cannot run has not been tested). Migration is skipped
(MUSTER_TEST_PG_SKIP_MIGRATE=1) so the cases measure the helper, not the schema —
except one case that runs the migrate path and checks the template has tables.
The orphan cases read /proc, so this suite is Linux-only.

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
import uuid
from pathlib import Path

REPO = Path(__file__).resolve().parents[1]
HELPER = REPO / "scripts" / "with-test-pg.sh"


def pids_with_marker(marker: str) -> list[int]:
    """PIDs whose argv contains `marker` — a string THIS test minted, so the
    match can only be a process this test started (Linux /proc)."""
    found = []
    for d in Path("/proc").iterdir():
        if not d.name.isdigit():
            continue
        try:
            argv = (d / "cmdline").read_bytes().split(b"\0")
        except OSError:
            continue
        if marker.encode() in argv:
            found.append(int(d.name))
    return found


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

    def sigterm_with_grandchild(self, script: str, marker: str) -> tuple[int, float]:
        """Start the helper on `bash -c script` (whose grandchild carries
        `marker` as argv[0]), SIGTERM the helper once the grandchild exists,
        and return (exit code, seconds the helper took to exit)."""
        proc = subprocess.Popen([str(HELPER), "bash", "-c", script], env=self.env,
                                stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)
        try:
            deadline = time.monotonic() + 120
            while not pids_with_marker(marker):
                if proc.poll() is not None:
                    self.fail(f"helper exited early ({proc.returncode}): {proc.stderr.read() if proc.stderr else ''}")
                self.assertLess(time.monotonic(), deadline, "grandchild never started")
                time.sleep(0.2)
            # Positive control: the grandchild is a GRANDchild — its parent is the
            # `bash -c`, not the helper — so killing only the direct child misses it.
            gc = pids_with_marker(marker)[0]
            ppid = int(Path(f"/proc/{gc}/stat").read_text().rsplit(")", 1)[1].split()[1])
            self.assertNotEqual(ppid, proc.pid, "positive control: marker process is not the helper's direct child")
            t0 = time.monotonic()
            proc.send_signal(signal.SIGTERM)
            rc = proc.wait(timeout=60)
            return rc, time.monotonic() - t0
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.wait()
            proc.stderr.close()

    def assert_no_orphan(self, marker: str):
        # Reparenting + exit is not instantaneous; allow a short settle, then
        # kill (by resolved PID, this test's own marker) anything left so a red
        # run does not leak a 300 s sleep.
        deadline = time.monotonic() + 5
        while pids_with_marker(marker) and time.monotonic() < deadline:
            time.sleep(0.1)
        left = pids_with_marker(marker)
        for pid in left:
            os.kill(pid, signal.SIGKILL)
        self.assertEqual(left, [], "grandchild orphaned after the helper exited")

    def test_sigterm_leaves_no_grandchild(self):
        marker = f"wtp-orphan-{uuid.uuid4().hex}"
        # The marker is the inner bash's $0 (an argv element of its own). The
        # outer `& wait` keeps the direct child alive, so the marked bash — and
        # its sleep — are the helper's grandchildren.
        rc, _ = self.sigterm_with_grandchild(f"bash -c 'sleep 300; :' {marker} & wait", marker)
        self.assertEqual(rc, 128 + signal.SIGTERM)
        self.assert_no_orphan(marker)
        self.assertEqual(self.leftovers(), [])

    def test_sigterm_kills_a_grandchild_that_ignores_term(self):
        marker = f"wtp-stubborn-{uuid.uuid4().hex}"
        # `trap '' TERM` ignores TERM in the marked bash (and its sleep inherits
        # that), so it survives TERM: only the KILL after the grace period removes it.
        rc, took = self.sigterm_with_grandchild(f"bash -c \"trap '' TERM; sleep 300; :\" {marker} & wait", marker)
        self.assertEqual(rc, 128 + signal.SIGTERM)
        self.assertLess(took, 30, "the grace period must be short, not the sleep's 300 s")
        self.assert_no_orphan(marker)
        self.assertEqual(self.leftovers(), [])

    def start_with_tmpdir(self, tmpdir: Path):
        tmpdir.mkdir()
        self.env["TMPDIR"] = str(tmpdir)
        report = Path(self.tmp) / "report"
        p = self.run_helper('psql "$MUSTER_TEST_DATABASE_URL" -tAc "select current_database()" > ' + str(report))
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(report.read_text().strip(), "muster_test")
        self.assertEqual([q.name for q in tmpdir.iterdir()], [], "temp dir cleaned up")

    def test_long_tmpdir_starts(self):
        # A Unix socket path is limited to ~107 bytes; the helper uses TCP only,
        # so it must not put a socket in the temp dir at all.
        long_tmp = Path(self.tmp) / ("x" * 110)
        self.assertGreater(len(str(long_tmp).encode()), 107, "positive control: the TMPDIR really is long")
        self.start_with_tmpdir(long_tmp)

    def test_tmpdir_with_a_space_starts(self):
        self.start_with_tmpdir(Path(self.tmp) / "with space")

    def test_migrate_path_populates_the_template(self):
        self.env.pop("MUSTER_TEST_PG_SKIP_MIGRATE")
        report = Path(self.tmp) / "report"
        p = self.run_helper(
            'psql "$MUSTER_TEST_DATABASE_URL" -tAc '
            '"select count(*) from information_schema.tables where table_schema = \'public\'" > ' + str(report))
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertGreater(int(report.read_text()), 0, "muster_test has no tables: the migrate did not run")

    def test_no_command_is_a_usage_error(self):
        p = subprocess.run([str(HELPER)], env=self.env, capture_output=True, text=True)
        self.assertEqual(p.returncode, 2)
        self.assertEqual(self.leftovers(), [])


if __name__ == "__main__":
    unittest.main()
