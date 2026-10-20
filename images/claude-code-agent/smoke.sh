#!/usr/bin/env bash
# The image's built-in smoke test:
#
#   docker run --rm --tmpfs /data:rw,exec,mode=2777,uid=0,gid=1000 -e CC_SMOKE_VOLUME=fresh <image> cc-smoke
#
# 🔴 IT NEEDS A VOLUME AT /data, as an agent's pod has one, and refuses to run
# without it (step 0). The `legacy` shape (a volume an image up to 0.49.0 left a
# root-owned /data/workspace on) is in .github/workflows/ci.yml's ccd-image job.
#
# It asserts, INSIDE the built image:
#   0. on that volume, an agent's FIRST start with a repository (the old step 11,
#      now at the production path /data/workspace/<name>) checks it out and starts
#      the real CLI in it, leaving /data/workspace and the checkout owned and
#      writable by uid 1000 — and on a fresh volume ccd had nothing to replace, on
#      a legacy one it replaced the root-owned /data/workspace and said so;
#   1. the container runs as uid 1000, not root;
#   2. the pinned CLI is the one on PATH;
#   3. ccd's /healthz answers 200 with JSON, and GET / answers the same status;
#   4. the TUI reached its prompt: /healthz reports session "started", which only a
#      SessionStart hook can set — so this also proves the seeded hook wiring
#      reaches ccd through `ccd hook`, over the hook socket and with the credential
#      ccd put in the CLI's environment; and ccd's supervisor started it as a FRESH
#      session (no transcript on a new volume); a hook WITHOUT the credential is
#      refused 401, the socket is private, and nothing listens on 127.0.0.1:18790;
#   5. `/exit` typed into the pane is followed by an in-pod restart that RESUMES:
#      the supervisor's start count goes to 2 with `--resume <id>` (the CLI records
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
#      an unreachable muster fails fast and writes no git routing; curl is present;
#  10. the seeded settings pin default permission mode and the approval hooks,
#      and `ccd approve` with muster unreachable makes no decision (prints nothing).
#  11. (runs FIRST, as part of step 0) with CCD_REPO (a local bare repository
#      standing in for GitHub), ccd clones it into CCD_WORKSPACE before starting
#      the CLI; the real CLI runs in the checkout and applies the repository's
#      own .claude/ project settings (its SessionStart hook sees
#      CLAUDE_PROJECT_DIR = the checkout);
#  12. with a repository that cannot be cloned, /healthz stays 200 and reports
#      why, and no CLI session is started.
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

# 0. The VOLUME at /data, as an agent's pod has one. 🔴 The smoke refuses to run
# without it: with no volume mounted, /data is the image's own uid-1000
# directories and the first-start ownership bug below cannot happen, so a pass
# would say nothing about a real pod. CC_SMOKE_VOLUME names the shape:
#   fresh  — an empty volume as local-path + fsGroup 1000 makes it (root:1000,
#            mode 2777 at its root), on which nothing may have pre-created
#            /data/workspace (the image has no WORKDIR there any more: a container
#            runtime creates a missing WORKDIR as ROOT, after mounting volumes);
#   legacy — a volume a claude-code-agent image up to 0.49.0 already started on:
#            /data/workspace exists, root-owned, mode 0755 — `ccd seed` must
#            replace it (seed.go ensureWritableDir) and say so;
#   legacy-resume — the same, holding a prior transcript, and no repository:
#            only the resume after the replacement is checked (below), then exit.
awk '$5 == "/data" { found = 1 } END { exit !found }' /proc/self/mountinfo \
  || fail "/data is not a mount point: run with a volume there (see images/claude-code-agent/Dockerfile's header)"
case "${CC_SMOKE_VOLUME:-}" in
  fresh)
    [[ "$(stat -c '%u:%g %a' /data)" == "0:1000 2777" ]] || fail "fresh volume: /data is $(stat -c '%u:%g %a' /data), want 0:1000 2777"
    ;;
  legacy)
    [[ "$(stat -c '%u %a' /data/workspace 2>/dev/null)" == "0 2755" && ! -w /data/workspace ]] \
      || fail "legacy volume: /data/workspace is '$(stat -c '%u %a' /data/workspace 2>&1)', want root-owned 2755 and unwritable"
    ;;
  legacy-resume)
    [[ "$(stat -c '%u %a' /data/workspace 2>/dev/null)" == "0 2755" && ! -w /data/workspace ]] \
      || fail "legacy-resume volume: /data/workspace is '$(stat -c '%u %a' /data/workspace 2>&1)', want root-owned 2755 and unwritable"
    ;;
  *) fail "CC_SMOKE_VOLUME must be fresh, legacy or legacy-resume, not '${CC_SMOKE_VOLUME:-}'" ;;
esac
echo "cc-smoke: $CC_SMOKE_VOLUME volume at /data: $(stat -c '%U:%G %a' /data)"

# legacy-resume — an agent WITHOUT a repository on a legacy volume that already
# holds a conversation: the root-owned, EMPTY /data/workspace an image up to
# 0.49.0 ran the CLI in, and that CLI's transcript under
# /data/claude/projects/-data-workspace/. CI makes it from the `legacy` run's own
# volume (whose steps 1-5 left a real transcript from the real CLI) by putting
# the root-owned /data/workspace back. `ccd seed` replaces the directory — same
# path, so the same transcript key — and the CLI must RESUME that transcript,
# not start fresh: the replacement must not cost the agent its conversation.
if [[ "$CC_SMOKE_VOLUME" == legacy-resume ]]; then
  tdir=/data/claude/projects/-data-workspace
  shopt -s nullglob
  before=("$tdir"/*.jsonl)
  shopt -u nullglob
  (( ${#before[@]} > 0 )) || fail "legacy-resume volume: no prior transcript in $tdir"
  /usr/local/bin/cc-entrypoint >/tmp/cc-entrypoint.log 2>&1 &
  ep=$!
  wait_for '200 {*"session":"started"*'
  [[ "$body" == *'"supervisor":{"cli":"running","cli_mode":"continue","cli_starts":1'* ]] \
    || fail "the first start on a legacy volume holding a transcript did not resume it: $body"
  grep -q 'ccd seed: replaced /data/workspace: it was an empty directory' /tmp/cc-entrypoint.log \
    || fail "ccd did not say it replaced the root-owned /data/workspace: $(cat /tmp/cc-entrypoint.log)"
  pid="$(tmux list-panes -t cc -F '#{pane_pid}')"
  argv="$(tr '\0' ' ' <"/proc/$pid/cmdline")"
  [[ "$(readlink "/proc/$pid/cwd")" == /data/workspace && -O /data/workspace && -w /data/workspace ]] \
    || fail "the CLI (pid $pid) runs in $(readlink "/proc/$pid/cwd"), want the replaced /data/workspace owned and writable by uid 1000"
  # The SAME transcript: --resume names one that existed before this start
  # (ccd's primary record), or --continue (the newest, the only one here).
  resumed=""
  if [[ "$argv" =~ --resume\ ([^ ]+) ]]; then
    resumed="${BASH_REMATCH[1]}"
    [[ " ${before[*]} " == *"/$resumed.jsonl "* ]] \
      || fail "the CLI resumed $resumed, which is not a transcript the volume held before: ${before[*]}"
  elif [[ "$argv" != *' --continue'* ]]; then
    fail "the CLI's argv neither resumes nor continues: $argv"
  fi
  echo "cc-smoke: legacy volume with a transcript: replaced the root-owned /data/workspace and resumed (${resumed:---continue}): $argv"
  kill "$ep" 2>/dev/null || true
  tmux kill-server 2>/dev/null || true
  echo "cc-smoke: PASS"
  exit 0
fi

# 0 (continued). A repository (CCD_REPO) — an agent's FIRST start, on that volume,
# at the production paths: CCD_WORKSPACE=/data/workspace/<name> (agentspec
# claudeCodeRepo). This ran as the old step 11 against a /tmp workspace and
# passed while every real agent with a repository crash-looped on
# `ccd seed: mkdir /data/workspace/<repo>: permission denied`.
#
# With the REAL CLI. A local bare repository stands in for GitHub through git's
# url.<x>.insteadOf — the mechanism ccd's connector routing itself uses for
# muster's proxy. The repository carries a CLAUDE.md, a project skill
# (.claude/skills/smoke/SKILL.md) and a PROJECT settings file whose SessionStart
# hook records $CLAUDE_PROJECT_DIR. ccd must clone it into CCD_WORKSPACE before
# it starts the CLI, start the CLI there (trusted: SessionStart reaches ccd),
# and the CLI must apply the repository's own .claude/ — its hook fires with
# CLAUDE_PROJECT_DIR = the checkout. ⚠ That the CLI LOADS the skill is not
# observable without a model turn; what is shown is that it reads project
# configuration from the checkout's .claude/, where the skill lives.
export GIT_CONFIG_GLOBAL=/tmp/smoke-gitconfig
fake=/tmp/smoke-github src=/tmp/smoke-src ws=/data/workspace/app
mkdir -p "$src/.claude/skills/smoke" "$fake/example-org"
printf '# smoke project\n' >"$src/CLAUDE.md"
printf -- '---\nname: smoke\ndescription: a fixture skill\n---\nbody\n' >"$src/.claude/skills/smoke/SKILL.md"
cat >"$src/.claude/settings.json" <<'JSON'
{"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "printf %s \"$CLAUDE_PROJECT_DIR\" >/tmp/smoke-project-dir"}]}]}}
JSON
printf '[url "file://%s/"]\n\tinsteadOf = https://github.com/\n' "$fake" >"$GIT_CONFIG_GLOBAL"
git -C "$src" init -q -b trunk
git -C "$src" add CLAUDE.md .claude
git -C "$src" -c user.name=smoke -c user.email=smoke@example.test commit -q -m fixture
git clone -q --bare "$src" "$fake/example-org/app.git"
CLAUDE_CONFIG_DIR=/tmp/smoke-repo-cfg CCD_WORKSPACE="$ws" CCD_REPO=example-org/app CCD_REPO_BRANCH=trunk \
  /usr/local/bin/cc-entrypoint >/tmp/cc-entrypoint.log 2>&1 &
ep=$!
wait_for '200 {*"session":"started"*'
[[ "$body" == *'"repo":{"repo":"example-org/app","branch":"trunk","dir":"'"$ws"'","state":"ready"}'* ]] \
  || fail "/healthz does not report the checkout ready: $body"
pid="$(tmux list-panes -t cc -F '#{pane_pid}')"
[[ "$(readlink "/proc/$pid/cwd")" == "$ws" ]] || fail "the CLI (pid $pid) runs in $(readlink "/proc/$pid/cwd"), not the checkout $ws"
[[ -f "$ws/CLAUDE.md" && -f "$ws/.claude/skills/smoke/SKILL.md" ]] || fail "the checkout lacks CLAUDE.md or the skill"
for _ in $(seq 1 30); do [[ -s /tmp/smoke-project-dir ]] && break; sleep 1; done
[[ "$(cat /tmp/smoke-project-dir 2>/dev/null)" == "$ws" ]] \
  || fail "the repository's own SessionStart hook did not run with CLAUDE_PROJECT_DIR=$ws (got '$(cat /tmp/smoke-project-dir 2>/dev/null)')"
[[ -O /data/workspace && -w /data/workspace && -O "$ws" && -w "$ws" ]] \
  || fail "/data/workspace or the checkout is not owned and writable by uid 1000: $(stat -c '%n %U:%G %a' /data/workspace "$ws")"
case "$CC_SMOKE_VOLUME" in
  fresh)
    # Nothing pre-created /data/workspace, so ccd had nothing to replace.
    if grep -q 'ccd seed: replaced' /tmp/cc-entrypoint.log; then
      fail "on a fresh volume ccd had to replace a directory something else created: $(grep 'ccd seed: replaced' /tmp/cc-entrypoint.log)"
    fi ;;
  legacy)
    grep -q 'ccd seed: replaced /data/workspace: it was an empty directory' /tmp/cc-entrypoint.log \
      || fail "ccd did not say it replaced the root-owned /data/workspace: $(cat /tmp/cc-entrypoint.log)" ;;
esac
echo "cc-smoke: first start with a repository on the $CC_SMOKE_VOLUME volume: checked out in $ws; the real CLI runs in it and applied its project settings"
kill "$ep" 2>/dev/null || true
tmux kill-server 2>/dev/null || true
# Steps 1-10 run with the operator's own (empty) git configuration: step 9
# asserts connect-git wrote no url.* routing.
unset GIT_CONFIG_GLOBAL

# 1-6. No repository: the CLI in /data/workspace itself.
/usr/local/bin/cc-entrypoint >/tmp/cc-entrypoint.log 2>&1 &
ep=$!
wait_for '200 {*"session":"started"*'
echo "cc-smoke: /healthz -> $body"
[[ "$body" == *'"supervisor":{"cli":"running","cli_mode":"fresh","cli_starts":1'* ]] \
  || fail "expected one fresh supervised start: $body"
# The CLI's working directory is the key its transcripts are filed under
# (projects/-data-workspace): it must stay exactly /data/workspace.
pid="$(tmux list-panes -t cc -F '#{pane_pid}')"
[[ "$(readlink "/proc/$pid/cwd")" == /data/workspace && -O /data/workspace && -w /data/workspace ]] \
  || fail "the CLI (pid $pid) runs in $(readlink "/proc/$pid/cwd"), want /data/workspace owned and writable by uid 1000"
root="$(get /)"
[[ "${root%% *}" == "200" && "${root#* }" == "{"* ]] || fail "GET / -> $root, expected 200 JSON like /healthz"
echo "cc-smoke: session started (fresh); GET / ok"

# 4b. The hook channel. The session started above only because the real CLI's
# hooks reached ccd WITH the credential ccd put in its environment; here, a hook
# WITHOUT it is refused, the socket is private to uid 1000, and nothing listens
# on the old TCP port (which every container in a pod could reach).
sock="$(tmux show-environment -t cc CCD_HOOK_SOCKET | cut -d= -f2-)"
[[ -S "$sock" && "$(stat -c '%a' "$sock")" == 600 && "$(stat -c '%a' "$(dirname "$sock")")" == 700 ]] \
  || fail "the hook socket '$sock' is not a 0600 socket in a 0700 directory"
if (exec 3<>/dev/tcp/127.0.0.1/18790) 2>/dev/null; then fail "something listens on 127.0.0.1:18790"; fi
code="$(curl -s -o /dev/null -w '%{http_code}' --unix-socket "$sock" -X POST -d '{}' http://ccd/hook/Stop)"
[[ "$code" == 401 ]] || fail "a hook without the credential got $code, want 401"
echo "cc-smoke: hook channel private ($sock); a hook without the credential is refused"

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
[[ "$body" == *'"cli_mode":"continue"'* ]] || fail "the restart after /exit did not resume: $body"
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
    CCD_LISTEN=127.0.0.1:18799 CCD_TMUX_SOCKET=refuse \
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
echo "cc-smoke: connector store wiring ok"

# 10. Approvals: the seeded settings pin default permission mode (never auto,
# never bypass) and route PermissionRequest to `ccd approve`; and with the relay
# unreachable (port 9 above) `ccd approve` prints NOTHING and exits 0 — the CLI's
# "no decision", i.e. the normal in-terminal prompt. Never an allow.
node -e '
  const s = JSON.parse(require("fs").readFileSync(process.argv[1], "utf8"));
  const p = JSON.stringify(s.permissions);
  const want = JSON.stringify({defaultMode: "default", disableAutoMode: "disable", disableBypassPermissionsMode: "disable"});
  if (p !== want) { console.error("permissions " + p + " want " + want); process.exit(1); }
  const h = s.hooks.PermissionRequest[0].hooks[0];
  if (h.command !== "ccd approve") { console.error("PermissionRequest hook " + JSON.stringify(h)); process.exit(1); }
  if (s.hooks.Notification[0].matcher !== "permission_prompt") { console.error("Notification matcher"); process.exit(1); }
' "$CLAUDE_CONFIG_DIR/settings.json" || fail "seeded settings do not pin the permission posture / approval hook"
rc=0
out="$(printf '%s' '{"tool_name":"Bash","tool_input":{"command":"rm -rf x"},"permission_mode":"default"}' \
  | timeout 30 ccd approve 2>/tmp/approve.err)" || rc=$?
[[ "$rc" == "0" && -z "$out" ]] || fail "ccd approve with the relay unreachable: exit $rc stdout '$out', want 0 and nothing"
echo "cc-smoke: approval hook seeded; relay unreachable -> no decision (terminal prompt)"
unset MUSTER_CONNECT_URL MUSTER_HOOK_TOKEN

# 11. is step 0, at the top: a repository's first start on the fresh volume.

# 12. A repository that cannot be cloned: /healthz stays 200 and says so, and no
# CLI is started. (Step 0's git routing to the local fake GitHub, so nothing
# here reaches the network.)
export GIT_CONFIG_GLOBAL=/tmp/smoke-gitconfig
CLAUDE_CONFIG_DIR=/tmp/smoke-missing-cfg CCD_WORKSPACE=/tmp/smoke-ws/missing CCD_REPO=example-org/missing \
  /usr/local/bin/cc-entrypoint >/tmp/cc-entrypoint.log 2>&1 &
ep=$!
wait_for '200 {*"state":"failed"*'
[[ "$body" == *'could not be cloned'* ]] || fail "the failed checkout does not say why: $body"
if tmux has-session -t cc 2>/dev/null; then fail "a CLI session was started without the checkout"; fi
echo "cc-smoke: an unclonable repository keeps the CLI stopped and /healthz 200"
kill "$ep" 2>/dev/null || true
tmux kill-server 2>/dev/null || true
unset GIT_CONFIG_GLOBAL
echo "cc-smoke: PASS"
