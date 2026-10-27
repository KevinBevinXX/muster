#!/usr/bin/env bash
# kbx-quickstart.sh — bring muster up on a plain machine: NO root, NO docker, NO nix.
#
# Upstream's quickstart (README) wants `docker compose` for Postgres and a Nix
# flake for the host agent. This script is for a machine that has neither: it
# fetches a RELOCATABLE Postgres (the binaries the zonky embedded-postgres
# project publishes to Maven Central, which run as an ordinary user), puts its
# cluster under a state directory you own, builds the two binaries from this
# checkout, migrates, and mints a mirror token.
#
# It is idempotent: every step checks for its own result first, so re-running
# after a reboot just starts what is already there.
#
#   ./tools/kbx-quickstart.sh setup    # fetch Postgres, build, migrate, mint a token
#   ./tools/kbx-quickstart.sh start    # start Postgres, muster-server, muster-mirror
#   ./tools/kbx-quickstart.sh status   # what is up, and on which port
#   ./tools/kbx-quickstart.sh stop     # stop all three
#
# Everything lands in ONE directory you can delete (default ~/.local/share/muster):
#   pg/        the Postgres binaries
#   data/      the cluster
#   bin/       muster-server, muster-mirror
#   secrets/   the UI password and the mirror token, mode 0600
#   log/       server, mirror and Postgres logs
#   run/       pid files
set -euo pipefail

STATE="${MUSTER_STATE_DIR:-$HOME/.local/share/muster}"
PGPORT="${MUSTER_PG_PORT:-55433}"
PORT="${MUSTER_PORT:-8105}"
PGDB="${MUSTER_PG_DB:-muster}"
PGUSER=muster
# 🔴 muster LISTENS ON EVERY INTERFACE AND THERE IS NO SETTING THAT CHANGES IT.
# cmd/muster-server/main.go builds its listener as
# net.JoinHostPort("", cfg.Port) — an empty host, i.e. 0.0.0.0 — and MUSTER_PORT
# is the only knob. So the UI, the terminal door and the mirror endpoint are
# reachable in CLEAR TEXT from anything that can route to this machine, your LAN
# included, the moment the server starts. Treat the port as public: put a tunnel
# or a VPN in front of it for the phone, and firewall the port itself if the
# network is shared. This variable is the address the script CHECKS and prints,
# not an address it can make the server honour.
LOCAL="${MUSTER_LOCAL_HOST:-127.0.0.1}"
# The zonky build of Postgres 16. Pinned: an unpinned "latest" would change the
# cluster's major version under an existing data directory, which refuses to start.
PG_VERSION="${MUSTER_PG_VERSION:-16.4.0}"
PG_JAR_URL="https://repo1.maven.org/maven2/io/zonky/test/postgres/embedded-postgres-binaries-linux-amd64/${PG_VERSION}/embedded-postgres-binaries-linux-amd64-${PG_VERSION}.jar"

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DSN="postgres://${PGUSER}:${PGUSER}@127.0.0.1:${PGPORT}/${PGDB}?sslmode=disable"

say() { printf '\033[1m==>\033[0m %s\n' "$*"; }
die() { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

# go_bin finds a Go toolchain new enough for this module. A toolchain installed
# under ~/.local is used when PATH has none, which is the common shape on a box
# where Go was never installed system-wide.
go_bin() {
  if command -v go >/dev/null 2>&1; then command -v go; return; fi
  for c in "$HOME/.local/go-toolchain/go/bin/go" "$HOME/go/bin/go" /usr/local/go/bin/go; do
    [ -x "$c" ] && { echo "$c"; return; }
  done
  die "no Go toolchain. Install the version in go.mod (see https://go.dev/dl/), or set PATH to one."
}

pg_ctl_bin() { echo "$STATE/pg/bin/pg_ctl"; }

pg_running() { "$(pg_ctl_bin)" -D "$STATE/data" status >/dev/null 2>&1; }

# running answers for one of our two long-lived processes by its pid file.
#
# 🔴 IT VALIDATES THE PID BEFORE SIGNALLING IT. The first version of this read
# `kill -0 "$(cat pidfile || echo 0)"`, and with no pid file that becomes
# `kill -0 0` — which asks about EVERY PROCESS IN THE CALLER'S PROCESS GROUP and
# therefore SUCCEEDS. So a cold machine reported both daemons "already running"
# and `start` did nothing at all, while `status` (reading the same files a
# different way) said down. A pid must be a positive integer and name a live
# process before it means anything.
running() {
  local f="$STATE/run/$1.pid" p
  [ -s "$f" ] || return 1
  p="$(cat "$f" 2>/dev/null || true)"
  case "${p:-}" in ''|*[!0-9]*) return 1 ;; esac
  [ "$p" -gt 1 ] || return 1
  kill -0 "$p" 2>/dev/null
}

# record_pid finds the pid of the binary we just launched and stores it.
#
# ⚠ $! IS NOT USABLE HERE: the programs are started through setsid, which forks
# when it is already a process group leader, so $! can name a parent that has
# already exited. The binary is matched by its FULL PATH under the state
# directory, which no other process on the machine carries — and never by a bare
# "muster" pattern, which would also match the shell running this script.
record_pid() {
  local name="$1" exe="$2" p=
  for _ in $(seq 1 40); do
    p="$(pgrep -f "^${exe}( |$)" 2>/dev/null | head -1 || true)"
    [ -n "$p" ] && break
    sleep 0.25
  done
  [ -n "$p" ] || return 1
  echo "$p" > "$STATE/run/$name.pid"
}

# ---------------------------------------------------------------------------

fetch_postgres() {
  if [ -x "$STATE/pg/bin/postgres" ]; then say "Postgres binaries already here"; return; fi
  command -v curl >/dev/null || die "curl is needed to fetch Postgres"
  command -v tar  >/dev/null || die "tar is needed to unpack Postgres"
  say "fetching a rootless Postgres $PG_VERSION"
  mkdir -p "$STATE/pg" "$STATE/tmp"
  curl -fsSL -o "$STATE/tmp/pg.jar" "$PG_JAR_URL" || die "could not download Postgres"
  # The jar is a zip holding one .txz. unzip is usual; python3 is the fallback
  # so this works on a box with no unzip.
  if command -v unzip >/dev/null 2>&1; then
    (cd "$STATE/tmp" && unzip -o -q pg.jar)
  elif command -v python3 >/dev/null 2>&1; then
    python3 -c "import zipfile,sys; zipfile.ZipFile(sys.argv[1]).extractall(sys.argv[2])" "$STATE/tmp/pg.jar" "$STATE/tmp"
  else
    die "need unzip or python3 to unpack the Postgres archive"
  fi
  tar -C "$STATE/pg" -xJf "$STATE/tmp"/postgres-linux-*.txz || die "could not unpack Postgres"
  rm -rf "$STATE/tmp"
  "$STATE/pg/bin/postgres" --version
}

init_cluster() {
  if [ -f "$STATE/data/PG_VERSION" ]; then say "cluster already initialised"; return; fi
  say "initialising the cluster"
  mkdir -p "$STATE/data"
  # trust auth on loopback only: the socket and the port are both local, and the
  # password below exists so the DSN has one, not as the security boundary.
  "$STATE/pg/bin/initdb" -D "$STATE/data" -U "$PGUSER" --auth=trust -E UTF8 >/dev/null
  chmod 700 "$STATE/data"
}

start_pg() {
  if pg_running; then say "Postgres already running on $PGPORT"; return; fi
  say "starting Postgres on 127.0.0.1:$PGPORT"
  mkdir -p "$STATE/log"
  "$(pg_ctl_bin)" -D "$STATE/data" \
    -o "-p $PGPORT -k $STATE/data -c listen_addresses=127.0.0.1" \
    -l "$STATE/log/postgres.log" start >/dev/null
  for _ in $(seq 1 30); do pg_running && return; sleep 0.5; done
  die "Postgres did not come up; see $STATE/log/postgres.log"
}

# create_db makes the database. The zonky bundle ships initdb, pg_ctl and
# postgres and NO client (no psql, no createdb), so the database is created
# through this module's own Postgres driver: a throwaway program built inside
# the checkout, where the dependency already resolves.
create_db() {
  local go; go="$(go_bin)"
  local tmp="$REPO/cmd/.kbx-createdb"
  mkdir -p "$tmp"
  # ⚠ NO `trap ... RETURN` HERE. A RETURN trap is not scoped to the function
  # that installs it: it stayed armed and fired on a LATER function's return,
  # where $tmp is unset, and `set -u` then killed the script after setup had
  # already succeeded. The directory is removed explicitly below instead.
  cat > "$tmp/main.go" <<'GO'
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
)

func main() {
	admin, db := os.Args[1], os.Args[2]
	ctx := context.Background()
	c, err := pgx.Connect(ctx, admin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer c.Close(ctx)
	if _, err := c.Exec(ctx, "ALTER USER muster WITH PASSWORD 'muster'"); err != nil {
		fmt.Fprintln(os.Stderr, "set password:", err)
		os.Exit(1)
	}
	if _, err := c.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{db}.Sanitize()); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			fmt.Fprintln(os.Stderr, "create database:", err)
			os.Exit(1)
		}
	}
}
GO
  say "ensuring the $PGDB database exists"
  local rc=0
  (cd "$REPO" && "$go" run ./cmd/.kbx-createdb \
    "postgres://${PGUSER}@127.0.0.1:${PGPORT}/postgres?sslmode=disable" "$PGDB") || rc=$?
  rm -rf "$tmp"
  [ "$rc" -eq 0 ] || die "could not create the $PGDB database"
}

build_binaries() {
  local go; go="$(go_bin)"
  say "building muster-server and muster-mirror"
  mkdir -p "$STATE/bin"
  (cd "$REPO" && "$go" build -o "$STATE/bin/muster-server" ./cmd/muster-server)
  (cd "$REPO" && "$go" build -o "$STATE/bin/muster-mirror" ./cmd/muster-mirror)
  # muster-server has no `migrate` subcommand (its only one is `mirror`), so the
  # migrator is its own binary.
  (cd "$REPO" && "$go" build -o "$STATE/bin/muster-migrate" ./cmd/muster-migrate)
}

migrate() {
  say "migrating the database"
  DATABASE_URL="$DSN" "$STATE/bin/muster-migrate"
}

# host_label is this host's name as muster will accept it: a lowercase DNS label
# ([a-z0-9-], 1-63, no leading or trailing '-'). `hostname -s` is NOT already
# one — this was written on a box whose short hostname is "kevinsDell", and
# `mirror token create` refuses that outright — so it is folded to lower case and
# anything else is replaced. MUSTER_HOST overrides it.
host_label() {
  if [ -n "${MUSTER_HOST:-}" ]; then printf '%s' "$MUSTER_HOST"; return; fi
  local h
  h="$(hostname -s 2>/dev/null || hostname)"
  h="$(printf '%s' "$h" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9-' '-')"
  h="$(printf '%s' "$h" | sed -e 's/^-*//' -e 's/-*$//' -e 's/--*/-/g')"
  h="${h:0:63}"
  h="$(printf '%s' "$h" | sed -e 's/-*$//')"
  [ -n "$h" ] || h=host
  printf '%s' "$h"
}

secrets() {
  mkdir -p "$STATE/secrets"; chmod 700 "$STATE/secrets"
  if [ ! -s "$STATE/secrets/ui-password" ]; then
    say "minting a UI password"
    # 24 bytes of urandom, base64: comfortably over muster's 12-character floor.
    (umask 077; head -c 24 /dev/urandom | base64 | tr -d '\n=' > "$STATE/secrets/ui-password")
  fi
  if [ ! -s "$STATE/secrets/mirror-token" ]; then
    local host; host="$(host_label)"
    say "minting a mirror token for host '$host'"
    start_pg
    (umask 077; DATABASE_URL="$DSN" "$STATE/bin/muster-server" mirror token create "$host" \
      > "$STATE/secrets/mirror-token")
    # A refused mint still leaves the file, so an empty one is the failure.
    if [ ! -s "$STATE/secrets/mirror-token" ]; then
      rm -f "$STATE/secrets/mirror-token"
      die "the mirror token was not minted (see the message above)"
    fi
    (umask 077; printf '%s' "$host" > "$STATE/secrets/mirror-host")
  fi
}

start_server() {
  mkdir -p "$STATE/log" "$STATE/run"
  if running server; then say "muster-server already running"; return; fi
  say "starting muster-server on port $PORT (ALL interfaces — see the note at the top)"
  # setsid: the server must outlive the shell that started it, and must not take
  # a Ctrl-C meant for that shell.
  DATABASE_URL="$DSN" \
  MUSTER_UI_PASSWORD="$(cat "$STATE/secrets/ui-password")" \
  MUSTER_STANDALONE=1 \
  MUSTER_MIRROR=1 \
  MUSTER_PORT="$PORT" \
    setsid "$STATE/bin/muster-server" >> "$STATE/log/server.log" 2>&1 &
  record_pid server "$STATE/bin/muster-server" || true
  for _ in $(seq 1 40); do
    if (exec 3<>"/dev/tcp/$LOCAL/$PORT") 2>/dev/null; then exec 3>&- 2>/dev/null; return; fi
    sleep 0.25
  done
  die "muster-server did not open port $PORT; see $STATE/log/server.log"
}

start_mirror() {
  if running mirror; then say "muster-mirror already running"; return; fi
  command -v tmux >/dev/null || die "tmux is not installed: there would be no windows to mirror"
  say "starting muster-mirror (-interactive)"
  # ⚠ -interactive IS A SHELL ON THIS MACHINE, by muster's own flag help: an
  # interactive tmux client can switch to any session or open a new window. It is
  # what makes a phone able to TYPE into a session rather than only watch one.
  # Drop it if you want the read-only view.
  # -host is passed explicitly: a token minted for one host cannot register
  # another, and muster-mirror's own default is the short hostname, which is
  # exactly the value that may not be a valid label (see host_label).
  # ⚠ -allow-http IS REQUIRED HERE AND IS NOT A WEAKENING. muster-mirror refuses
  # a plain-http URL because the mirror token would cross the network in clear
  # text; this leg never leaves the machine (the host agent and the server are
  # both on 127.0.0.1), so there is no network to sniff. The flag's own help
  # calls it test-and-local-development only, which is exactly this case. A
  # REMOTE host agent must use https — do not copy this line to one.
  setsid "$STATE/bin/muster-mirror" \
    -url "http://$LOCAL:$PORT" \
    -allow-http \
    -token-file "$STATE/secrets/mirror-token" \
    -host "$(cat "$STATE/secrets/mirror-host" 2>/dev/null || host_label)" \
    -interactive >> "$STATE/log/mirror.log" 2>&1 &
  record_pid mirror "$STATE/bin/muster-mirror" \
    || die "muster-mirror did not start; see $STATE/log/mirror.log"
}

cmd_setup() {
  fetch_postgres
  init_cluster
  start_pg
  create_db
  build_binaries
  migrate
  secrets
  say "setup done"
  cmd_status
}

cmd_start() {
  [ -x "$STATE/bin/muster-server" ] || die "run '$0 setup' first"
  start_pg
  start_server
  start_mirror
  cmd_status
  cat <<EOF

Open http://$LOCAL:$PORT and sign in with:

    $(cat "$STATE/secrets/ui-password" 2>/dev/null)

Then run Claude Code inside tmux, so muster can mirror it:

    tmux new -s work
    claude

It appears on the Agents list as a card, rendered as a chat.

TO REACH IT FROM A PHONE, put a tunnel in front of port $PORT — the phone needs
HTTPS for the installable app, web push and passkeys. With cloudflared, add a
hostname to the tunnel's ingress (ABOVE the catch-all 404):

    - hostname: muster.example.com
      service: http://localhost:$PORT

then 'cloudflared tunnel route dns <tunnel> muster.example.com' and restart it.

⚠ muster binds EVERY interface and cannot be told not to, so port $PORT is open
to your LAN as plain HTTP right now. Firewall it if the network is shared.
Do NOT put muster behind an auth proxy without exempting GET /mirror/connect:
that route checks its own token, and the host agent cannot sign in to a proxy.
EOF
}

cmd_stop() {
  for n in mirror server; do
    if running "$n"; then
      say "stopping muster-$n"
      # By PID, never by pattern: a pkill -f for "muster" would also match the
      # shell running this script.
      kill "$(cat "$STATE/run/$n.pid")" 2>/dev/null || true
    fi
    rm -f "$STATE/run/$n.pid"
  done
  if pg_running; then say "stopping Postgres"; "$(pg_ctl_bin)" -D "$STATE/data" stop >/dev/null; fi
}

cmd_status() {
  printf 'state dir : %s\n' "$STATE"
  if pg_running; then printf 'postgres  : up on 127.0.0.1:%s\n' "$PGPORT"; else printf 'postgres  : down\n'; fi
  for n in server mirror; do
    if running "$n"; then
      printf '%-10s: up (pid %s)\n' "$n" "$(cat "$STATE/run/$n.pid")"
    else
      printf '%-10s: down\n' "$n"
    fi
  done
  printf 'url       : http://%s:%s  (also served on every other interface)\n' "$LOCAL" "$PORT"
}

case "${1:-}" in
  setup)  cmd_setup ;;
  start)  cmd_start ;;
  stop)   cmd_stop ;;
  status) cmd_status ;;
  *) printf 'usage: %s {setup|start|stop|status}\n' "$0"; exit 2 ;;
esac
