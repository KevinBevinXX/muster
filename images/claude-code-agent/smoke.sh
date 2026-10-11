#!/usr/bin/env bash
# The image's built-in smoke test:
#
#   docker run --rm <image> cc-smoke
#
# It asserts, INSIDE the built image:
#   1. the container runs as uid 1000, not root;
#   2. the pinned CLI is the one on PATH;
#   3. ccd's /healthz answers 200 with JSON, and GET / answers the same status;
#   4. the TUI reached its prompt: /healthz reports session "started", which only a
#      SessionStart hook can set — so this also proves the seeded hook wiring
#      reaches ccd through `ccd hook`; and ccd's supervisor started it as a FRESH
#      session (no transcript on a new volume);
#   5. `/exit` typed into the pane is followed by an in-pod restart that RESUMES:
#      the supervisor's start count goes to 2 with `--continue` (the CLI records
#      the /exit itself in a transcript, measured, so there is now one to resume)
#      and SessionStart arrives again, with the container never restarting — and
#      `env` / `apiKeyHelper` planted in settings.json while the first CLI ran are
#      gone after that restart, removed by ccd's supervisor (the entrypoint's seed
#      ran only once, before the first start);
#   6. the entrypoint's seed produced the onboarding/trust/hook shape (`ccd seed --check`);
#   7. the entrypoint REFUSES to start (exit 1, naming the variable) with each of
#      ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, CLAUDE_CODE_USE_BEDROCK,
#      CLAUDE_CODE_USE_VERTEX, ANTHROPIC_BASE_URL set to a FAKE value — before it
#      seeds or starts anything;
#   8. `ccd seed` removes `env` and `apiKeyHelper` planted in settings.json;
#   9. with muster's connector store offered (FAKE url and token), seed registers
#      muster's MCP server and `seed --check` agrees; `ccd connect-git` against
#      an unreachable muster fails fast and writes no git routing; curl is present.
#
# ⚠ IT NEEDS NO REAL CREDENTIAL AND MUST NEVER BE GIVEN ONE, AND IT SENDS NO TURN:
# nothing here needs the provider's API. Without any token the CLI stops at its
# login-method screen and never fires SessionStart, so a deliberately INVALID
# token is supplied when none is set. Resuming a conversation that has real turns
# in it is the operator live check's step 5.
set -euo pipefail

fail() { echo "cc-smoke: FAIL: $*" >&2; exit 1; }

[[ "$(id -u)" == "1000" ]] || fail "running as uid $(id -u), expected 1000"
echo "cc-smoke: uid $(id -u) ok"

want="${CLAUDE_CODE_VERSION:?the image sets CLAUDE_CODE_VERSION}"
got="$(claude --version 2>/dev/null || true)"
[[ "$got" == "$want "* ]] || fail "claude --version is '$got', expected '$want …'"
echo "cc-smoke: claude $got ok"

export CLAUDE_CODE_OAUTH_TOKEN="${CLAUDE_CODE_OAUTH_TOKEN:-sk-ant-oat01-SMOKE-DELIBERATELY-INVALID}"
export HOOKS_TOKEN="${HOOKS_TOKEN:-smoke-hooks-token}"

/usr/local/bin/cc-entrypoint >/tmp/cc-entrypoint.log 2>&1 &
ep=$!

# get PATH: prints "<status code> <body>". bash's /dev/tcp: the image carries no
# curl, and needs none for this.
get() {
  exec 3<>/dev/tcp/127.0.0.1/18789 || return 1
  printf 'GET %s HTTP/1.0\r\nHost: localhost\r\n\r\n' "$1" >&3
  local resp
  resp="$(cat <&3)"
  exec 3<&-
  local status="${resp#HTTP/* }"
  printf '%s %s' "${status%% *}" "${resp#*$'\r\n\r\n'}"
}

# wait_for PATTERN: poll /healthz until "<status> <body>" matches, or fail after 120s.
body=""
wait_for() {
  for _ in $(seq 1 120); do
    kill -0 "$ep" 2>/dev/null || { cat /tmp/cc-entrypoint.log >&2; fail "the entrypoint exited"; }
    body="$(get /healthz 2>/dev/null || true)"
    # shellcheck disable=SC2053 # $1 is a glob pattern on purpose
    [[ "$body" == $1 ]] && return 0
    sleep 1
  done
  tmux capture-pane -p -t cc >&2 || true
  cat /tmp/cc-entrypoint.log >&2
  fail "/healthz never matched $1 within 120s; last: $body (the pane and ccd's log are above)"
}

wait_for '200 {*"session":"started"*'
echo "cc-smoke: /healthz -> $body"
[[ "$body" == *'"supervisor":{"cli":"running","cli_mode":"fresh","cli_starts":1'* ]] \
  || fail "expected one fresh supervised start: $body"
root="$(get /)"
[[ "${root%% *}" == "200" && "${root#* }" == "{"* ]] || fail "GET / -> $root, expected 200 JSON like /healthz"
echo "cc-smoke: session started (fresh); GET / ok"

# While the first CLI runs, plant env / apiKeyHelper in the persisted settings.json
# (harmless values; every other key, the hooks included, kept as seeded).
node -e '
  const f = process.argv[1], s = JSON.parse(require("fs").readFileSync(f, "utf8"));
  s.env = {CCD_SMOKE_PLANTED: "1"}; s.apiKeyHelper = "/bin/false";
  require("fs").writeFileSync(f, JSON.stringify(s));
' "$CLAUDE_CONFIG_DIR/settings.json" || fail "could not plant env/apiKeyHelper"
grep -q '"apiKeyHelper"' "$CLAUDE_CONFIG_DIR/settings.json" || fail "control: the plant did not land"

# /exit in the pane: the CLI exits, and the supervisor restarts it in the same pane.
tmux send-keys -t cc -l '/exit'
sleep 0.3
tmux send-keys -t cc Enter
wait_for '200 {*"cli_starts":2*"session":"started"*'
[[ "$body" == *'"cli_mode":"continue"'* ]] || fail "the restart after /exit did not --continue: $body"
echo "cc-smoke: /exit -> resumed in-pod: $body"
s="$(cat "$CLAUDE_CONFIG_DIR/settings.json")"
[[ "$s" != *'"env"'* && "$s" != *'"apiKeyHelper"'* && "$s" == *'"hooks"'* ]] \
  || fail "after the in-pod restart settings.json still holds env/apiKeyHelper (or lost its hooks): $s"
grep -q 'removed "apiKeyHelper"' /tmp/cc-entrypoint.log \
  || fail "ccd did not log removing apiKeyHelper before the restart: $(cat /tmp/cc-entrypoint.log)"
echo "cc-smoke: the in-pod restart removed env and apiKeyHelper planted since the pod started"

ccd seed --check --config-dir "$CLAUDE_CONFIG_DIR" --workspace "$CCD_WORKSPACE" \
  --settings-template /etc/ccd/settings.json || fail "seeded config has the wrong shape"

kill "$ep" 2>/dev/null || true
tmux kill-server 2>/dev/null || true

# 7. Each refused variable, with a FAKE value, alone. `timeout` bounds the case
# where the refusal is missing and the entrypoint goes on to serve; a separate
# config dir and port keep a non-refused start from touching the one above.
for v in ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN CLAUDE_CODE_USE_BEDROCK CLAUDE_CODE_USE_VERTEX ANTHROPIC_BASE_URL; do
  rc=0
  out="$(env "$v=fake-smoke-value" CLAUDE_CONFIG_DIR=/tmp/refuse-cfg CCD_WORKSPACE=/tmp/refuse-ws \
    CCD_LISTEN=127.0.0.1:18799 CCD_HOOK_LISTEN=127.0.0.1:18798 CCD_TMUX_SOCKET=refuse \
    timeout 15 /usr/local/bin/cc-entrypoint 2>&1)" || rc=$?
  [[ "$rc" == "1" && "$out" == *"$v is set"*"Refusing."* ]] \
    || fail "with $v set the entrypoint exited $rc, want 1 naming $v: $out"
  [[ ! -e /tmp/refuse-cfg/settings.json ]] || fail "with $v set the entrypoint seeded before refusing"
  echo "cc-smoke: $v refused"
done

# 8. env / apiKeyHelper planted in the persisted settings.json are removed by seed.
cat >"$CLAUDE_CONFIG_DIR/settings.json" <<'JSON'
{"env": {"ANTHROPIC_BASE_URL": "https://fake.invalid"}, "apiKeyHelper": "/bin/echo fake", "model": "kept"}
JSON
ccd seed --config-dir "$CLAUDE_CONFIG_DIR" --workspace "$CCD_WORKSPACE" \
  --settings-template /etc/ccd/settings.json 2>/tmp/seed.err || fail "re-seed failed: $(cat /tmp/seed.err)"
s="$(cat "$CLAUDE_CONFIG_DIR/settings.json")"
[[ "$s" != *'"env"'* && "$s" != *'"apiKeyHelper"'* && "$s" == *'"model": "kept"'* ]] \
  || fail "seed did not remove env/apiKeyHelper (or dropped another key): $s"
ccd seed --check --config-dir "$CLAUDE_CONFIG_DIR" --workspace "$CCD_WORKSPACE" \
  --settings-template /etc/ccd/settings.json || fail "re-seeded config has the wrong shape"
echo "cc-smoke: seed removed env and apiKeyHelper"

# 9. The connector store, offered with FAKE values (nothing listens on port 9).
export MUSTER_CONNECT_URL=http://127.0.0.1:9 MUSTER_HOOK_TOKEN=smoke-agent-token-not-real
ccd seed --config-dir "$CLAUDE_CONFIG_DIR" --workspace "$CCD_WORKSPACE" \
  --settings-template /etc/ccd/settings.json 2>/tmp/seed.err || fail "seed with the store offered: $(cat /tmp/seed.err)"
grep -q '"url": "http://127.0.0.1:9/mcp"' "$CLAUDE_CONFIG_DIR/.claude.json" \
  || fail "seed did not register muster's MCP server: $(cat "$CLAUDE_CONFIG_DIR/.claude.json")"
ccd seed --check --config-dir "$CLAUDE_CONFIG_DIR" --workspace "$CCD_WORKSPACE" \
  --settings-template /etc/ccd/settings.json || fail "seed --check disagrees with the registered MCP server"
rc=0; timeout 15 ccd connect-git --timeout 3s github >/tmp/cg.out 2>&1 || rc=$?
[[ "$rc" == "1" ]] || fail "connect-git against an unreachable muster exited $rc, want 1: $(cat /tmp/cg.out)"
if git config --global --get-regexp '^url\.' >/dev/null 2>&1; then fail "connect-git wrote git routing without a grant"; fi
curl --version >/dev/null || fail "curl is missing"
unset MUSTER_CONNECT_URL MUSTER_HOOK_TOKEN
echo "cc-smoke: connector store wiring ok"
echo "cc-smoke: PASS"
