#!/usr/bin/env bash
# Drive the PINNED Hermes CLI for real (`make test-livehermes`):
# docs/design/hermes.md, Phase 3.
#
# 🔴 RUN IT ON EVERY HERMES PIN BUMP, AND BEFORE ENABLING -hermes-send ON A
# HOST. muster's send to Hermes reads Hermes's SCREEN (the screen check) and
# leans on its key bindings and timings. Those are Hermes's, not ours: a new
# commit can move them, and this is where it shows.
#
# What it runs (go test, build tag livehermes):
#   - internal/mirrorhost TestLiveHermesScreens: every screen state the design
#     lists, reached on the real CLI, the screen check measured on each and
#     what Escape does there (MUSTER_LIVEHERMES_RECORD=1 re-records the
#     fixtures into internal/hermeshost/testdata/screens/);
#   - internal/mirrorhost TestLiveHermesTimings: the hook slack, the Enter
#     margin and the queue gap, re-measured under CPU load;
#   - internal/mirrorhost TestLiveHermesSend: the gated send end to end, and
#     every blocking state refused with nothing pasted (when present);
#   - internal/hermeshost TestOverlayLedgerMatchesThePinnedSource and
#     TestViLabelsMatchThePinnedLocales: the ledgers held against the pin.
#
# 🔴 ISOLATION. Each run makes its own HERMES_HOME and HOME under a fresh
# temporary directory, a PRIVATE tmux server (-S <socket> there, -f /dev/null)
# and a local stub model on 127.0.0.1. It never reads or writes ~/.hermes, never
# talks to your tmux server or your Hermes gateway, and holds no credential: the
# model's api_key is a literal stub string. Processes are stopped by resolved
# PID, each only after its own environment names this run's home.
#
# Needs: `hermes` on PATH at the pinned commit (its Nix launcher's
# HERMES_REVISION), tmux >= 3.7 (paste-buffer -S), go. Never in CI.
set -euo pipefail

cd "$(dirname "$0")/.."
command -v hermes >/dev/null || { echo "livehermes: no hermes on PATH" >&2; exit 2; }
command -v tmux >/dev/null || { echo "livehermes: no tmux on PATH" >&2; exit 2; }
rev="$(grep -o "HERMES_REVISION='[0-9a-f]*'" "$(readlink -f "$(command -v hermes)")" | cut -d"'" -f2 || true)"

required=(TestLiveHermesScreens TestLiveHermesTimings TestOverlayLedgerMatchesThePinnedSource TestViLabelsMatchThePinnedLocales)
if grep -q 'func TestLiveHermesSend' internal/mirrorhost/*_test.go 2>/dev/null; then
  required+=(TestLiveHermesSend)
fi
req=()
for r in "${required[@]}"; do req+=(--require "$r"); done

set +e
set -o pipefail
go test -tags livehermes -count=1 -timeout 60m -json \
  -run '^(TestLiveHermes|TestOverlayLedgerMatchesThePinnedSource|TestViLabelsMatchThePinnedLocales)' \
  ./internal/mirrorhost/ ./internal/hermeshost/ |
  python3 -u tests/verdict.py --echo all "${req[@]}"
rc=$?
set -e

echo
if [[ $rc -eq 0 ]]; then
  echo "HERMES HARNESS VERDICT: GREEN — every required live test ran and passed against Hermes ${rev:0:7}."
  echo "  -hermes-send MAY be enabled on a host running this Hermes, one host at a time,"
  echo "  and NEVER on a host whose Hermes uses vi mode (display.vim_mode)."
else
  echo "HERMES HARNESS VERDICT: RED (exit $rc) — do NOT enable -hermes-send on any host until it is green."
fi
exit $rc
