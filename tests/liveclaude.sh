#!/usr/bin/env bash
# Drive a REAL Claude Code's AskUserQuestion pickers (`make test-liveclaude`).
#
# 🔴 RUN IT ON EVERY CLAUDE CODE UPGRADE. muster answers a mirrored session's
# question by typing into the terminal's picker (internal/askdrive), reading
# its screen before every key. That screen is Claude Code's, not ours: a new
# release can change it, and muster then refuses to type ("couldn't recognise
# the terminal picker") until askdrive is updated. This is where that shows up
# first — before an operator meets it.
#
# It starts a scratch `claude` in a PRIVATE tmux server (its own -L socket,
# nothing of yours attached) in a fresh temp directory, runs
# internal/mirrorhost's liveclaude test against it — every shape of question,
# driven and checked against the CLI's own record, and the picker's redraw lag
# re-measured — and tears the server and the directory down.
#
# Needs: a logged-in `claude` on PATH, tmux with `paste-buffer -S`. Spends a
# few model turns. Never in CI.
set -euo pipefail

command -v claude >/dev/null || { echo "liveclaude: no claude on PATH" >&2; exit 2; }
command -v tmux >/dev/null || { echo "liveclaude: no tmux on PATH" >&2; exit 2; }

sock="muster-liveclaude-$$"
dir="$(mktemp -d "${TMPDIR:-/tmp}/muster-liveclaude.XXXXXX")"
dir="$(cd "$dir" && pwd -P)"
# Claude Code keeps a project's transcripts under ~/.claude/projects/<the
# directory, every non-alphanumeric character a dash> — this run's own, named
# after its fresh temp directory, removed with it.
proj="$HOME/.claude/projects/$(printf '%s' "$dir" | sed 's/[^A-Za-z0-9]/-/g')"
cleanup() {
  # Only this run's private server (-L muster-liveclaude-<pid>), never yours.
  tmux -L "$sock" kill-server 2>/dev/null || true
  rm -rf "$dir" "$proj"
}
trap cleanup EXIT

tmux -L "$sock" -f /dev/null new-session -d -s live -x 80 -y 23 -c "$dir" claude

# Wait for the prompt; accept the folder-trust question if it is asked.
for _ in $(seq 120); do
  screen="$(tmux -L "$sock" capture-pane -p -t live)"
  if grep -q 'Yes, I trust this folder' <<<"$screen"; then
    # The default is "No, exit": move to "Yes" first, then confirm.
    tmux -L "$sock" send-keys -t live Down
    sleep 0.3
    tmux -L "$sock" send-keys -t live Enter
    sleep 1
  elif grep -q '^❯' <<<"$screen"; then
    break
  fi
  sleep 0.5
done
grep -q '^❯' <<<"$(tmux -L "$sock" capture-pane -p -t live)" || {
  echo "liveclaude: claude never showed its prompt:" >&2
  tmux -L "$sock" capture-pane -p -t live >&2
  exit 1
}

out="$(mktemp)"
rc=0
MUSTER_LIVE_SOCKET="$sock" MUSTER_LIVE_SESSION=live MUSTER_LIVE_PROJECT="$proj" \
  go test -tags liveclaude -count=1 -v -run '^TestLiveClaudePicker$' ./internal/mirrorhost/ | tee "$out" || rc=$?
# A skipped or empty run is not a pass: count the subtests that passed.
passed="$(grep -c -- '--- PASS: TestLiveClaudePicker/' "$out" || true)"
failed="$(grep -c -- '--- FAIL' "$out" || true)"
skipped="$(grep -c -- '--- SKIP' "$out" || true)"
rm -f "$out"
echo "liveclaude: $passed case(s) passed, $failed failed, $skipped skipped"
if [ "$rc" -ne 0 ] || [ "$passed" -lt 7 ] || [ "$skipped" -ne 0 ]; then
  exit 1
fi
