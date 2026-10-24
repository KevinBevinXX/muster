#!/usr/bin/env bash
# with-test-pg.sh — run a command against a THROWAWAY Postgres with no Docker.
#
#     scripts/with-test-pg.sh go test -race ./...
#     scripts/with-test-pg.sh go test ./internal/agents/
#
# `make test-db` needs a Docker daemon. This needs only the Postgres binaries the
# dev shell already carries (`initdb`, `pg_ctl`; `pkgs.postgresql_16` in
# flake.nix), so it also works where there is no Docker socket: a CI pod, a
# container, a remote build workspace. Run it inside the dev shell
# (`nix develop -c scripts/with-test-pg.sh …`).
#
# What it does, in order:
#   1. initdb a cluster in a fresh temp directory (under $TMPDIR), superuser
#      `muster`, password `muster` — the SAME coordinates docker-compose.test.yml
#      uses, so every DSN this repo spells works unchanged apart from the port.
#   2. start it on 127.0.0.1 and a FREE port, retrying on a port that turns out
#      taken. TCP only: the Unix socket is turned off
#      (unix_socket_directories=''), so a long $TMPDIR cannot overflow the
#      ~107-byte socket-path limit, and nothing touches /run/postgresql.
#   3. create the NAMED template database `muster_test` (see
#      docker-compose.test.yml for why it must not be `postgres`) and migrate it
#      with `go run ./cmd/muster-migrate`, as `make test-db` does.
#   4. export MUSTER_TEST_DATABASE_URL, MUSTER_TEST_PG_PORT and
#      MUSTER_TEST_REQUIRE_DB=1 (a missing database FAILS rather than skips),
#      then run the command.
#   5. stop Postgres and delete the temp directory on EVERY exit: success,
#      failure, Ctrl-C, SIGTERM, SIGHUP. The command runs in its OWN process
#      group; on a signal the whole group gets TERM, then KILL after a 5 s grace
#      period, so a grandchild (`go test`'s compiled *.test binary, a `bash -c`
#      child) is not orphaned. The command's own exit code is the script's exit
#      code; a signal exits 128+N.
#
# ⚠ SIGKILL cannot be trapped. A KILLed run leaves its temp directory behind
# (and the postmaster, unless whatever killed the script killed its children
# too). Each directory is named `muster-test-pg.*` under $TMPDIR, so it is
# findable.
#
# ⚠ NOT AS ROOT: initdb refuses to run as root, so this helper does too (it
# fails at step 1 with initdb's own message, exit 70). Run it as an ordinary
# user; it does not try to drop privileges. A container that runs as root needs
# a non-root user for this.
#
# Env: MUSTER_TEST_PG_SKIP_MIGRATE=1 skips step 3's migrate (dbtest then
# bootstraps each package's copy itself — slower, never wrong).
#
# Exit codes: the command's, or 2 usage · 69 Postgres binaries missing ·
# 70 Postgres could not be started/prepared · 128+N on signal N.
set -uo pipefail

say() { printf 'with-test-pg: %s\n' "$*" >&2; }

if [ "$#" -eq 0 ]; then
  say "usage: scripts/with-test-pg.sh CMD [ARGS...]"
  exit 2
fi
for b in initdb pg_ctl psql; do
  if ! command -v "$b" >/dev/null 2>&1; then
    say "$b not on PATH — run inside the dev shell: nix develop -c scripts/with-test-pg.sh $*"
    exit 69
  fi
done

dir=""
child=""   # the command's PID, which is also its process-group ID (set -m below)
# TERM the command's whole process group, then KILL whatever is still in it
# after the grace period. Targets the group ID of a process this script started.
stop_group() {
  [ -n "$child" ] || return 0
  kill -TERM -- "-$child" 2>/dev/null || return 0
  local i
  for ((i = 0; i < 50; i++)); do
    kill -0 -- "-$child" 2>/dev/null || break
    sleep 0.1
  done
  kill -KILL -- "-$child" 2>/dev/null
  wait "$child" 2>/dev/null
}
cleanup() {
  stop_group
  if [ -n "$dir" ] && [ -d "$dir" ]; then
    if [ -f "$dir/data/postmaster.pid" ]; then
      pg_ctl -D "$dir/data" -m immediate -w -t 30 stop >/dev/null 2>&1 \
        || say "pg_ctl stop failed; postmaster may still be running (see $dir/log)"
    fi
    rm -rf -- "$dir"
  fi
}
on_signal() {
  local n=$1
  trap - EXIT HUP INT TERM
  cleanup
  exit $((128 + n))
}
trap cleanup EXIT
trap 'on_signal 1' HUP
trap 'on_signal 2' INT
trap 'on_signal 15' TERM

dir=$(mktemp -d "${TMPDIR:-/tmp}/muster-test-pg.XXXXXX") || { say "mktemp failed"; exit 70; }
pwfile="$dir/pw"
printf 'muster\n' > "$pwfile"
if ! initdb -D "$dir/data" -U muster --pwfile="$pwfile" --auth=scram-sha-256 \
    -E UTF8 --locale=C >"$dir/log" 2>&1; then
  say "initdb failed:"; cat "$dir/log" >&2
  exit 70
fi
rm -f "$pwfile"

# A free port: a random pick in the dynamic range, skipping one that already
# answers, and retried if the postmaster still cannot bind it (another process
# took it between the probe and the start).
port=""
for _ in $(seq 1 20); do
  p=$(( 20000 + RANDOM % 40000 ))
  if (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then
    continue
  fi
  if pg_ctl -D "$dir/data" -l "$dir/log" -w -t 60 \
      -o "-c listen_addresses=127.0.0.1 -c port=$p -c unix_socket_directories='' -c fsync=off -c full_page_writes=off -c synchronous_commit=off" \
      start >/dev/null 2>&1; then
    port=$p
    break
  fi
done
if [ -z "$port" ]; then
  say "could not start Postgres on a free port; last log:"; tail -n 20 "$dir/log" >&2
  exit 70
fi

export PGPASSWORD=muster
if ! psql -h 127.0.0.1 -p "$port" -U muster -d postgres -v ON_ERROR_STOP=1 -q \
    -c 'CREATE DATABASE muster_test' >/dev/null 2>>"$dir/log"; then
  say "CREATE DATABASE muster_test failed:"; tail -n 20 "$dir/log" >&2
  exit 70
fi
unset PGPASSWORD

export MUSTER_TEST_PG_PORT=$port
export MUSTER_TEST_DATABASE_URL="postgres://muster:muster@127.0.0.1:$port/muster_test?sslmode=disable"
export MUSTER_TEST_REQUIRE_DB=1

if [ "${MUSTER_TEST_PG_SKIP_MIGRATE:-}" != 1 ]; then
  # From the repo root, whatever the caller's cwd: the command may run in a subdirectory.
  if ! (cd "$(dirname -- "${BASH_SOURCE[0]}")/.." && DATABASE_URL="$MUSTER_TEST_DATABASE_URL" go run ./cmd/muster-migrate) >>"$dir/log" 2>&1; then
    say "migrating the template failed:"; tail -n 20 "$dir/log" >&2
    exit 70
  fi
fi
say "Postgres up on 127.0.0.1:$port (template muster_test migrated); running: $*"

# Backgrounded + `wait` so a signal runs its trap NOW rather than after the
# command finishes; the trap stops the command's process group before stopping
# Postgres. `set -m` gives the background job its own process group (PGID = its
# PID), which is what lets stop_group reach grandchildren. ⚠ stdin is /dev/null:
# this is for test suites, not interactive tools.
set -m
"$@" </dev/null &
child=$!
set +m
wait "$child"
rc=$?
# The command exited by itself; anything it left running in its group is
# stopped by cleanup's stop_group on the way out.
exit "$rc"
