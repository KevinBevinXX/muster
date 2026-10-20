#!/usr/bin/env bash
# The claude-code-agent image's entrypoint: seed the CLI's config, start the
# interactive session in tmux, then run ccd in the foreground.
#
# 🔴 ccd IS THE FOREGROUND PROCESS, SO ITS EXIT IS THE CONTAINER'S EXIT. The TUI
# lives in a detached tmux session beside it, which ccd creates and SUPERVISES
# (CCD_SUPERVISE=1, cmd/ccd/supervise.go): a TUI that exits (an operator typing
# /exit, a crash) is restarted in the same pane with backoff; repeated exits in a
# short window are reported as crash_loop and fail /healthz, so Kubernetes
# restarts the pod. So does a tmux server that went away or was replaced
# (terminal_lost): ccd never starts a second CLI beside one it can no longer see.
#
# 🔴 EVERY START RESUMES THE SAME CONVERSATION WHEN THERE IS ONE: ccd runs
# `claude --resume <id>` — the primary's recorded session id, else the newest
# interactive, non-lane transcript for the workspace (never a bare `--continue`,
# which could pick a lane's conversation) — and plain `claude` only when there is
# none. CLAUDE_CONFIG_DIR must
# therefore be on a persistent volume (as must the workspace) — wiring that volume
# is the provisioner's job, not this image's.
set -euo pipefail

# `docker run <image> cc-smoke` (or any command): run it instead.
if [[ $# -gt 0 ]]; then
  exec "$@"
fi

: "${CLAUDE_CONFIG_DIR:?CLAUDE_CONFIG_DIR must be set}"
: "${CCD_WORKSPACE:?CCD_WORKSPACE must be set}"
CCD_TMUX_TARGET="${CCD_TMUX_TARGET:-cc}"

# A subscription token is the ONLY credential this image is meant to carry. The
# entrypoint refuses to start when any of these is set (non-empty) in ITS
# environment:
#   ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN — outrank CLAUDE_CODE_OAUTH_TOKEN in
#     the CLI's precedence and would silently move the session onto API billing;
#   CLAUDE_CODE_USE_BEDROCK, CLAUDE_CODE_USE_VERTEX — move the session onto a
#     cloud provider's credentials and billing;
#   ANTHROPIC_BASE_URL — sends every request, the subscription token included, to
#     another endpoint. No deployment of this image documents a gateway, so there
#     is no reason here to allow it.
# The same variables set through settings.json's `env` are covered by removing
# `env` and `apiKeyHelper` from <CLAUDE_CONFIG_DIR>/settings.json: here by `ccd
# seed`, and by ccd's supervisor again before every in-pod restart of the CLI
# (cmd/ccd/seed.go settingsForbidden). NOT covered: a .claude/settings*.json in
# the workspace, a key written while a CLI is running (until its next start), and
# any other variable the CLI may read.
for v in ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN CLAUDE_CODE_USE_BEDROCK CLAUDE_CODE_USE_VERTEX ANTHROPIC_BASE_URL; do
  if [[ -n "${!v:-}" ]]; then
    echo "cc-entrypoint: $v is set; only CLAUDE_CODE_OAUTH_TOKEN may select the session's credential and endpoint. Refusing." >&2
    exit 1
  fi
done

ccd seed --config-dir "$CLAUDE_CONFIG_DIR" --workspace "$CCD_WORKSPACE" \
  --settings-template /etc/ccd/settings.json

# muster's connector store (cmd/ccd/connect.go): when this pod is offered it,
# route git through muster's proxy for the `github` connector IF this agent
# already holds an approved grant (a pod restarted after an approval comes back
# wired). Best effort and bounded: it never blocks or fails the start, and it
# never files a request — asking is the session's decision. From then on
# `ccd serve` keeps the routing in step with the grants (approve, revoke).
if [[ -n "${MUSTER_CONNECT_URL:-}" ]]; then
  ccd connect-git --timeout 5s github || true
fi

cd "$CCD_WORKSPACE"
# ccd creates the tmux session itself, AFTER its hook listener is bound — the
# TUI's SessionStart hook is ccd's readiness signal for turns.
export CCD_TMUX_TARGET
export CCD_SUPERVISE=1
exec ccd serve
