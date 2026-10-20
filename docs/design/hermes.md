# Design: Hermes mirroring and the `hermes` agent kind

**Status: proposed, with Phase 0 done.** Nothing here is implemented. The
version target is **decided** (below). Every other section describes the design
the Phase 0 spikes support; the [Phase 0 results](#phase-0-results) table names
what each spike changed. Where this page states how muster works today, the
code is the authority (see [decisions.md](../decisions.md)). Where it states
how Hermes works, it cites upstream source by path at a commit, or a Phase 0
measurement. Claims still marked **UNVERIFIED** were read from source and never
exercised; they are listed under [Risks](#risks).

## Summary

[Hermes](https://github.com/NousResearch/hermes-agent) (MIT) is a coding agent
with a terminal CLI, a local gateway process, an OpenAI-compatible API server
and a SQLite session store. This design adds it to muster in two places:

1. **Mirroring**, at full parity with Claude Code: a tmux window running
   `hermes chat` gets a rendered conversation view, Send, the send queue, the
   host icon and listing, and its questions and approvals in muster's asks UI.
2. **A new in-cluster agent kind, `hermes`**: a pod running Hermes in API
   server mode behind a small supervisor, `hermesd`, that speaks muster's
   existing agent wire, the way `ccd` does for `claude-code`.

The design, in one line each:

- **Version (decided):** one upstream commit, pinned by **sha**, for hosts and
  pods: `5fc490e2b341…`, the gateway-runtime line the hosts already run.
- **Pane → session:** the CLI's tty breadcrumb, validated against the live
  process and against how the breadcrumb's session ended.
- **Transcript:** a read-only SQLite reader with WAL, cursor by `messages.id`,
  `active=1` and `display_order`; parent links followed only for compression.
- **Status:** a muster-installed Hermes **shell hook** writing one status file
  per physical session id, cross-checked against the turn lease and the
  newest message row before it is believed.
- **Send:** a **pane paste** is the only send path. It is gated on idle and on
  a screen check that admits exactly one state, an idle, empty input line, and
  is re-checked on the host at the moment of acting, with the Enter as a
  separate step. The viewer's `prompt.submit` was refuted: the CLI never draws
  the turn.
- **Questions and approvals:** clarify cards from the database, approval and
  sudo items from the hooks. Approvals are answered through the gateway viewer's
  `approval.respond`; clarify through the viewer if Phase 4 measures it, else in
  the terminal; sudo is never answered from muster.
- **Pod:** `hermes gateway run` with the API server on loopback, driven
  through `/v1/runs` and its SSE events by `hermesd`, which also resolves
  executions left unknown by a crash before it accepts turns.

## Goals

- A Hermes tmux window is a first-class mirrored window: rendered view,
  Terminal view, Send, Queue, Send now, status-driven attention, focus, filter
  and host icon, with no regression for Claude Code.
- **Questions and approvals are in scope.** The operator chose full parity, and
  Claude Code windows have asks, so a Hermes window's clarify questions and
  approvals reach muster's asks UI, each with a defined answer path
  ([questions and approvals](#questions-and-approvals)).
- Every send path is at least as safe as Claude Code's. 🔴 Measured in Phase 0:
  a paste followed by Enter on Hermes's approval prompt **approves the command**,
  whatever the pasted text says. The design must make that unreachable from
  muster, not merely unlikely.
- A `hermes` agent can be dispatched, kicked off, chatted with, and approved,
  with the same isolation as `claude-code`: uid 1000, no ServiceAccount token,
  the same NetworkPolicy, no credentials on the volume that outrank the pod's.
- The Claude-specific code paths become one runtime interface, sized for the
  two runtimes it serves: Claude Code and Hermes.

## Non-goals

- Hermes's Ink TUI (`hermes --tui`, `ui-tui/`). Only the classic
  prompt_toolkit CLI is mirrored.
- Hermes messaging platforms, Bot Chats, cron, webhooks, `hermes send`,
  `hermes proxy`, `hermes mcp serve`.
- Running Hermes's own docker/ssh/modal terminal backends, or its `hermes egress`
  credential-injecting proxy, inside an agent pod.
- Writing Hermes's database or config. muster-mirror is read-only on every
  Hermes file.
- Sending through the gateway viewer (refuted, S1).
- A Terminal view for `hermes` agent pods in the first release.
- Supporting more than one Hermes commit. There is one pinned sha.

## Operator decisions

- **Version:** one Hermes commit, pinned by sha, for hosts and pods
  ([versions](#versions)).
- **Hook installation:** the status hook and its exact consent allowlist are
  declared in the host's Home Manager configuration, through upstream's
  `hermes-agent` Home Manager module. muster-mirror ships only the
  `hermes-hook` command and never edits `~/.hermes/config.yaml`.
- **Release cadence:** each phase is released, and shipped to the hosts, as it
  merges.
- **Pod credentials:** `hermes` pods use the OpenRouter credential muster's
  gateway runtime already uses.

## Sources and notation

Upstream is `github.com/NousResearch/hermes-agent`. Two refs are cited:

| short | ref | commit |
|---|---|---|
| **P** | the pin, canary `v0.22.2+canary.20261019T071120Z` | `5fc490e2b34123212d73ef316086f939567a725a` |
| **S** | `v0.22.2`, the latest stable release | `d923a5eb487a17c0a68801bba9ef0f56c81040a8` |

A citation `P:hermes_cli/cli_tui_mixin.py:1597` means
`https://github.com/NousResearch/hermes-agent/blob/5fc490e2b34123212d73ef316086f939567a725a/hermes_cli/cli_tui_mixin.py#L1597`.
Upstream user and developer docs are cited by their path under
`website/docs/` at the same commit. **PIN-ONLY** marks code present at P and
absent at S, checked by tree (file present, `git grep` at each ref), never by
ancestry. **Sn** names a Phase 0 spike.

Phase 0 ran against P, in a scratch `HERMES_HOME`, a scratch tmux server and a
local scripted model stub; the pod spikes ran in a local container with
pod-like restrictions, not in Kubernetes. Timings are from an idle host. S was
compared by source, and for the API server at runtime; S was not run as a
mirrored CLI.

## Versions

### Decision: pin P by commit sha, for hosts and pods

**Decided by the operator:** hosts and pods both run `5fc490e2b341…`, pinned
by sha. The spikes support it:

- the hosts already run P, and moving them back to S is hazardous (below);
- the pod's restart path needs two things only P has: run records that survive
  a gateway restart, and `POST /v1/runs/{id}/resolve-unknown` (S6);
- P's API server ran 790 of 790 runs to `run.completed`, 263 of them approval
  round trips, over a four-minute soak (S6).

Rules that follow:

- **Pin by sha, never by tag.** Canary tags are deleted 14 days after they are
  cut (`P:scripts/release.py:575-580`). A flake input is
  `github:NousResearch/hermes-agent?rev=<40-hex sha>`.
- **Record the sha, not a version.** Committed source always reports `0.0.0`
  (`website/docs/developer-guide/stable-releases.md:8`). The gateway's own
  `identify` reply carries `code_sha` (S1), which muster-mirror logs and checks.
- **Move to a stable release only by tree.** When a `vX.Y.Z` ships, check by
  tree that it contains the gateway runtime (`hermes_cli/gateway_agent.py`
  present) and the P-only pieces this design uses, then run
  `make test-livehermes` (Phase 6) against it before moving the pin.
- **Refuse what it cannot read.** There is no multi-version design.
  muster-mirror checks the gateway's `identify` `code_sha` against the pin when
  it attaches, and the schema's columns with `PRAGMA table_info`; it refuses a
  schema or a screen it cannot read rather than guessing.

### What the pin is

P contains upstream's **unified gateway runtime** (PR #106742), which v0.22.2
does not. 🔴 Commit ancestry misleads here: S records an older main commit as a
second parent only to satisfy the release script, so
`git merge-base --is-ancestor X v0.22.2` does not mean X's code is in S. A
check of "is feature F in release R" tests the **tree**, never ancestry.

At P, `hermes chat` is a thin client. The turn runs in a per-`HERMES_HOME`
gateway (`hermes gateway run --quiet --idle-exit`), auto-started by the first
client (`P:hermes_cli/gateway_agent.py:1-9`,
`P:hermes_cli/gateway_client.py:257-301`). One gateway serves every CLI on that
home; a second pane's CLI joins it (S2). It outlives its clients, even
`tmux kill-server`, until its idle exit (S10).

| concern | at the pin |
|---|---|
| who runs hooks | the gateway, with the **first** client's environment (S2) |
| who writes `state.db` | the gateway only |
| `sessions.source` of a CLI session | `cli` at creation, `local` after the first turn; `created_source` stays `cli` (S3) |
| turn-in-flight signal in the DB | `session_turn_leases`, plus `session_admissions` |
| answering approvals out of band | the gateway viewer (S1) |
| `SCHEMA_VERSION` | 32 |
| API approval body | exactly `{request_id, execution_generation, choice}` (S6) |
| API run records across a restart | kept (S6) |
| `/v1/runs/{id}/resolve-unknown`, `/clarify` | present (S6) |
| `chat -q --format stream-json` | spawns a detached gateway per home (S7) |

### Moving a host from P to S is hazardous

- S opens P's v32 database (its migrations are only `< N`-gated,
  `S:hermes_state_schema.py:1110`), and then P's
  `sessions_runtime_ledger_delete_guard` trigger aborts deletes and prunes of
  sessions with gateway-runtime history.
- P's `hermes update` refuses an older target
  (`P:hermes_cli/update_downgrade_guard.py`; upstream issue #141051 reports it
  misfiring against the S tag), and a gateway whose checkout moves back exits
  with code 75 (`P:gateway/runtime_downgrade_exit.py`).
- The documented route back is restoring a pre-update snapshot
  (`P:website/docs/getting-started/updating.md:174-200`).

## Phase 0 results

| spike | verdict | key observation | changed |
|---|---|---|---|
| **S1** viewer protocol | partially | Attach, `session.resume`, `prompt.submit` and `approval.respond` work, and a passive viewer does not disturb the CLI's modal. 🔴 The CLI **never draws** a viewer-submitted turn. A viewer's `approval.respond` wins, but the CLI's modal stays drawn and later prints a misleading "allowed once". Passive viewers receive `clarify` and `sudo` server requests. | [send path](#the-send-path), [viewer](#the-gateway-viewer), [questions](#questions-and-approvals), [hygiene](#secret-hygiene), Phase 4 |
| **S2** hooks | confirmed, one correction | Hooks carry the **physical** session id (the child id after a rotation); the lease and breadcrumb keep the logical root. All three `on_human_input_*` kinds arrive. Pre-seeded consent holds in an auto-started gateway; an extra hook not on the allowlist makes every new CLI stop on a `[y/N]` prompt. Hook env has the first pane's `TMUX_PANE`. Slack: 0 (the final row precedes `on_session_end` by 19–33 ms). | [status](#busy-idle-and-waiting) |
| **S3** breadcrumb | confirmed | Follows `/new`, keeps the logical id across compression. `/resume` and `/branch` leave it stale, and mark the abandoned session `end_reason` `resumed_other` / `branched`. `parent_session_id` is shared by compression and branching. | [locator](#pane-to-session-resolution), [transcript](#transcript-reading) |
| **S4** turn lease | confirmed | TTL 300 s, renewed every 60 s, keyed by the logical root, held through approval and clarify waits, deleted at turn end. | [status](#busy-idle-and-waiting) |
| **S5** pending approval in the DB | refuted | No row records it; only the hooks, the viewer and the screen show it. | [questions](#questions-and-approvals) |
| **S6** API server | confirmed, with version differences | P's approval body is exact; events are `message.delta`, `approval.settled`. An unanswered approval is denied after 300 s and the run still **completes**. 🔴 After a SIGKILL, later runs on the session report `run.failed unknown_execution` but are only **queued**; `/resolve-unknown` releases them. SIGTERM drains 30 s, then exits 1. | [pod API](#mode-api-server-driven-by-a-supervisor), [restart](#restart-and-resume) |
| **S7** `-q` | confirmed | At P, `-q` leaves a detached gateway per home that inherits the caller's env, including `API_SERVER_*`. Not used. | [pod mode](#mode-api-server-driven-by-a-supervisor) |
| **S8** image and env | confirmed, size caveat | A Nix image runs as uid 1000 with a read-only root and no init. With ffmpeg overridden out: 1,149 MiB closure, 425 MiB gzipped (upstream `minimal`: 3,278 MiB). Provider keys, `GH_TOKEN` and `API_SERVER_*` are stripped from the agent's shell; `MUSTER_*` pass. Terminal HOME is `$HERMES_HOME/home` only if it exists and `TERMINAL_HOME_MODE=profile` (or container detection). git through the connector proxy worked. | [image](#image), [volume](#volume-layout), [credentials](#credentials) |
| **S9** instructions | settled | `/v1/runs` `instructions` is appended to that run's system prompt and leaves identity and `AGENTS.md` intact. SOUL.md replaces only the identity paragraph. `.hermes.md` suppresses `AGENTS.md`. `agent.system_prompt` and `HERMES_EPHEMERAL_SYSTEM_PROMPT` are ignored for API runs. | [instructions](#instructions) |
| **S10** process identity | confirmed (Nix measured; pip/uv simulated) | `comm=hermes`, `argv[1]` a `hermes` script run by `python*`, `argv[2]=chat`. `#{pane_current_command}` is `python3.14` or `python`: unusable. The gateway is also `comm=hermes`. | [locator](#pane-to-session-resolution) |
| **S11** paste mechanics | confirmed, two corrections | muster's `PastePane` submits once, idle, at both busy modes. The 50 ms guard applies to bracketed pastes too; 150 ms clears it. 🔴 Paste+Enter on the approval modal approved **once** with `4` pasted. Paste+Enter on a choices clarify picks the recommended choice. Seven screens captured as fixtures. | [send path](#the-send-path), [screen check](#the-screen-check) |

## Mirroring design

### A runtime seam instead of Claude-only code

Today the host side and the wire are Claude-shaped:
`mirrorhost.ClaudeBackend`/`ClaudeLocator`/`ClaudePane`,
`mirror.Window.{Claude, ClaudeSession, OtherClaude}`, the transcript stream as
`cctranscript.Event` at a byte `Offset`, `ui.SessionView.Kind()` keyed on
`Claude`, the hub's waiting set keyed on `w.Claude && w.Status == waiting`, and
the send queue's reasons ("Claude Code is working.").

Proposed, in muster's existing terms:

- **`mirrorhost.Runtime`**, one implementation per agent CLI:
  - `Locate(panes, procs) map[paneID]AgentPane` — today's `LocatePanes`,
    generalised. `AgentPane` is `ClaudePane` plus `Runtime string`; `Gen`,
    `Status`, `StatusSince`, `Pending`, `Source` keep their meanings.
  - `Stream(ctx, pane, resumeGen, resumeCursor) (EventSource, error)` — the
    rendered view's backfill and tail. For Claude it is today's file tail; the
    cursor is the byte offset. For Hermes the cursor is `messages.id`.
  - `CheckSend(ctx, pane, expectedSince) error` — the at-the-moment-of-acting
    gate. Claude's is a no-op, which preserves today's behaviour.
  - `Submit(ctx, pane, text) error` — paste then Enter, through `CheckSend`.
  - `Answer(ctx, pane, ask, choices) error` — today's `AnswerAsk` for Claude.
- The tmux backend asks each runtime in turn; the first that claims a pane
  wins. A pane is claimed by **process identity**, never by
  `#{pane_current_command}` (S10).
- **Wire**, with no version bump, in the established style (an older muster
  ignores unknown fields; an older host never sends them):
  - `Window.Runtime` (`"claude"`, `"hermes"`). `Window.Claude` stays true only
    for Claude Code, so an older muster shows a Hermes window as a plain
    terminal window and never treats it as Claude.
  - `Window.AgentSession`, the session id for any runtime.
    🔴 **A Hermes id does not fit today's field.** P's ids are
    `local-<64 hex>`, 70 characters; `claudeSessionPattern` (wire.go, also
    `internal/focus` and migration 0020) admits at most 64, and
    `NormalizeWindows` blanks an invalid id. The new field needs its own bound
    (proposed 1–128, `[0-9A-Za-z][0-9A-Za-z_-]*`, which also admits a rotated
    child's bare 32-hex id), with `TestTheLargestWindowListFitsOneFrame`
    re-measured. Focus keys become runtime-qualified (`hermes:<id>`).
  - `Msg.Runtime` on `watch`/`paste`/`answer`, so the host can refuse a
    message meant for one runtime landing in another.
- `cctranscript.Event` stays the render model. Hermes rows are translated into
  it on the host.

Phase 1 is this refactor alone, with the Claude path byte-identical, gated by
the existing suite.

### Pane to session resolution

**Recognising a Hermes CLI** (S10). A process is a Hermes CLI only if all hold:

- `comm == "hermes"`;
- `basename(argv[0])` starts with `python` and `basename(argv[1]) == "hermes"`
  (the Nix wrapper `exec`s, leaving no shell; a pip/uv console script has the
  same shape);
- `argv[2]` is `chat`, or absent (the bare form is UNVERIFIED);
- `gateway` is **not** in argv: the gateway is also `comm=hermes`, long-lived,
  and outlives the panes;
- its fd 0 is a tty equal to `#{pane_tty}`.

`#{pane_current_command}` reads `python3.14` (Nix) or `python` (venv) and is
never used. `HERMES_REVISION` is Nix-only and is not keyed on.

**`~/.hermes/runtime/active_sessions.json` — not authoritative.** Measured
(S3): the CLI's entry holds a locally minted `YYYYMMDD_HHMMSS_<6hex>` id,
never the session's `local-…` id, and it never changes through `/new`,
`/resume` or `/branch`. Dead owners are pruned lazily
(`hermes_cli/active_sessions.py:335-394`). Not used for mapping.

**`~/.hermes/terminal-sessions/tty-<sanitised tty>` — the mapping.** Written
by `hermes_cli/terminal_breadcrumbs.py:77-94`, atomically, `{session_id, cwd,
ts}`; the name comes from `os.ttyname`, so `/dev/pts/3` becomes
`tty-dev-pts-3`. Measured behaviour (S3):

| action | breadcrumb |
|---|---|
| CLI start | the session's logical id (`ts` is the first gateway attach, some seconds after start) |
| `/compress`, in place (the default) | unchanged |
| `/new` | rewritten to the new id |
| `/compress` with rotation | unchanged: stays the logical root |
| `/resume`, `/branch` | **stale**: still the previous session |

It is never deleted on exit (pruned after 30 days) and is gated by
`session.terminal_continue` (default true).

**Resolution** (the `HermesLocator`):

1. Walk the pane's process tree from `#{pane_pid}` to the nearest Hermes CLI.
   Record its pid and `/proc` start time.
2. Read the breadcrumb for `#{pane_tty}`. Believe it only if the CLI has that
   tty on fd 0 and the breadcrumb's `ts` is not older than the process's
   start; otherwise no rendered view.
3. Look the session up in `state.db`, with no `source` filter (it flips from
   `cli` to `local` after the first turn). It must not be `archived`/`hidden`.
4. Follow the lineage to its tip (see [transcript](#transcript-reading)).
5. 🔴 **A breadcrumb whose session ended for any reason other than
   compression is stale.** If the tip has `ended_at` set and an `end_reason`
   other than `compression`, the terminal switched sessions (`/resume`,
   `/branch`; measured values `resumed_other`, `branched`, and `new_session`
   transiently). The window gets **no rendered view**, with the reason "the
   terminal switched sessions (`/resume` or `/branch`)" — never the old
   session above a composer that sends to the pane.
6. `Source` reports `breadcrumb`; `muster-mirror -list -windows` prints it.

There is no guess fallback, and no pane map: at P a hook cannot write one,
because it runs in the gateway with the first client's `TMUX_PANE` (S2).
The real cure for step 5's gap is upstream: rewrite the breadcrumb on
`/resume` and `/branch` (one call each in `cli_commands_mixin.py`).

### Transcript reading

The store is `~/.hermes/state.db`, SQLite in WAL mode. Reading it externally
read-only is supported upstream (`website/docs/developer-guide/session-storage.md:292-309`)
and was used throughout Phase 0.

**Opening it.**

- One long-lived connection per host, `mode=ro`, never `immutable=1` (which
  ignores the WAL).
- A read-only connection never checkpoints, but the `-shm` must be writable,
  so muster-mirror runs as the uid that owns `~/.hermes`, as it already must.
- `busy_timeout`; retry `SQLITE_BUSY` and transient `SQLITE_IOERR`
  (`P:hermes_state_readpool.py:28-34`). Keep read transactions short so the
  writer's checkpoints are not held back.
- A pure-Go driver, so muster-mirror needs no C toolchain. It is a new
  dependency, the largest the host binary would carry; measure the binary.
- Columns are feature-detected with `PRAGMA table_info`. A required column
  that is missing disables the rendered view for that host, with a logged
  reason.

**Schema used** (`P:hermes_state_common.py:444-546`):

- `sessions`: `id`, `parent_session_id`, `ended_at`, `end_reason`, `cwd`,
  `title`, `model`, `last_activity_at`, `archived`, `hidden`.
- `messages`: `id`, `session_id`, `role`, `content`, `tool_call_id`,
  `tool_calls` (JSON), `tool_name`, `timestamp`, `finish_reason`, `active`,
  `compacted`, `display_kind`, `display_order`, `message_uid`.
- Never selected: `reasoning*`, `api_content`. muster never shows thinking.

**Write timing** is per message: the assistant row carrying `tool_calls` is
flushed before the tools run, each tool result after it completes, the final
reply at the end (`agent/turn_tool_round.py:131-133`,
`agent/tool_executor.py:199-210`, `agent/turn_final_response.py:364`).
Streaming deltas are not persisted.

**Compaction and lineage.**

- **In place is the default** (`compression.in_place: true`): the
  session id is unchanged, old rows become `active=0, compacted=1`, and the
  retained context is re-inserted as `active=1` (S2: 50 rows to 12 live). Live
  view: `active=1`. Scrollback: `active=1 OR compacted=1`, ordered by
  `display_order`. De-duplicate on `message_uid`, which survives compaction.
- **Rotation** (the legacy `in_place: false`) ends the session with
  `end_reason='compression'` and continues in a child whose
  `parent_session_id` points back.
- 🔴 **`parent_session_id` is shared by compression and branching** (S3). The
  lineage walk follows a parent link only when the **parent's** `end_reason`
  is `compression`; otherwise a branch would merge its parent's transcript
  into its view.
- The view's `Gen` is the lineage **root**, so a rotation does not reset the
  page.

**Incremental recipe.**

1. Backfill: the last `MaxBackfillEvents` rendered rows, by `display_order`.
2. Tail: `… WHERE session_id IN (<lineage>) AND id > :cursor ORDER BY id`,
   polled every `ViewPoll`. The cursor travels in the existing `Msg.Offset`.
3. Track `COUNT(active)`, `MAX(id)` and `SUM(transcript_revision)`.
   `transcript_revision` (PIN-ONLY) moves only on in-place rewrites
   (`P:hermes_state_common.py:771-780`); upstream's own fingerprint combines
   all three (`P:hermes_state_messages.py:1486-1497`). A change appends do not
   explain is a rewind, compaction or rewrite: send a `Reset` backfill.

**Rendering into `cctranscript.Event`.**

| Hermes row | Event |
|---|---|
| `role='user'`, `display_kind` NULL | `prompt`; `Line` = `messages.id`, so the prompt index works |
| `role='assistant'`, `content` non-empty | `text` |
| each `tool_calls[]` entry `{id, function:{name, arguments}}` | `tool_call`, `ToolID` = `id`, `Target` from the arguments, `Input` capped |
| `role='tool'` with `tool_call_id` | `tool_result` |
| `display_kind='failed_turn'`, or an error `finish_reason` | `error` |
| `display_kind` `process_complete`, `async_delegation_complete`, `model_switch`, `steer`, `auto_continue`, `skill_invocation`, `personality_switch`, `internal_notification` | `notice`, naming the kind |
| `display_kind='hidden'`, `role='system'`, `role='session_meta'` | nothing |
| a reasoning-only reply (empty `content`) | nothing |

An unknown `display_kind` renders as a `notice` naming it, never as user text.
Every field goes through the host's redaction (`redactLines`) and caps.

### Busy, idle and waiting

Hermes has no status file or IPC for the CLI. **muster installs a shell hook,
`muster-mirror hermes-hook`, writing one status file per session.** Shell
hooks (`hooks:` in `config.yaml`) receive JSON on stdin with
`hook_event_name`, `session_id`, `cwd`, `extra`
(`website/docs/user-guide/features/hooks.md:445-498`). Registered events:

| event | writes |
|---|---|
| `pre_llm_call` | `busy` |
| `on_human_input_request` (`kind` ∈ `approval`, `clarify`, `sudo`; all three measured, S2) | `waiting`, with `kind` and `request_id` |
| `on_human_input_resolved` | `busy` |
| `on_session_end` (every turn finalisation) | `idle` |
| `on_session_finalize` | `idle` |

```json
{"v":1,"session_id":"local-…","status":"idle","since":1700000000000,"kind":"","request_id":"","writer_pid":123,"writer_start":"4567"}
```

- **Keyed by the physical session id** (S2). The hook's `session_id` is the
  session the turn ran in: the logical `local-…` id, or after a rotation the
  child's 32-hex id. The locator reads the status files of **every** id in the
  lineage and keeps the newest `since`.
- **The lease is looked up by the original id.** `session_turn_leases` is
  keyed by `conversation_id`, the logical root, also after a rotation (S2, S4).
- 🔴 **The hook never reads `TMUX_PANE` or the tty.** At P the hook runs in the
  gateway, whose environment holds the `TMUX_PANE` of the pane that
  auto-started it — wrong for every other pane, which is worse than absent
  (S2). A test pins that the hook's output does not depend on either.
- **Where.** `~/.muster/hermes-status/<session_id>.json` (directory 0700, file
  0600), written by rename. The hook finds the directory from
  `~/.muster/mirror-hook.json`, written by the managed daemon, as
  `muster-mirror ask` does; the gateway's environment is whichever client
  started it.
- **Monotonic.** Written under a lock, only when `since` is newer than the
  file's.
- **Contents.** Status, time, request kind and id. 🔴 Never the prompt text,
  never tool arguments.
- **Installation.** The hook entries and their consent allowlist are declared
  in the host's Home Manager configuration, through upstream's `hermes-agent`
  Home Manager module. muster-mirror ships only the `hermes-hook` command and
  never edits `~/.hermes/config.yaml`.
- **Consent** (S2). Pre-seeded allowlist entries hold in an auto-started
  gateway, with no prompt. 🔴 The declaration must allowlist **exactly** every
  `(event, command)` pair it configures: a configured hook that is not
  allowlisted is skipped by the gateway with a warning, and makes every new
  foreground CLI stop before its UI on `Allow this hook to run? [y/N]:`. A hook
  that is not executable, or fails, writes nothing, and that reads as
  **unknown**, never idle. `-list -hermes` surfaces the "not allowlisted"
  state.

**How it feeds muster.** The locator sets `AgentPane.Status` and
`StatusSince` from the file, as it does from Claude Code's sessions file. The
server half needs no new rule: `Hub.AgentState`, the "status time newer than
the paste" rule and the 30-second idle wait apply as written. The hook stamps a
fresh `since` at every turn end, so a short turn still moves it.

🔴 **Believing a status file.**

- **Stale busy.** A `busy` file is stale if its writer is gone, or if the
  lineage root has no live lease (`expires_at > now`). The lease's TTL is
  300 s, renewed every 60 s, and it is held while the turn waits on an
  approval or clarify (S4), so a live lease means "a turn is in flight,
  possibly waiting on a person", never "working". Stale busy reads as
  **unknown**.
- **False idle**, the dangerous direction. `idle` is believed only if (a) no
  lease is live for the root and (b) no `messages` row in the lineage has a
  `timestamp` newer than `since`. Slack is **0**: across 29 measured turns the
  final row preceded `on_session_end` by 19–33 ms, and a new turn's user row
  preceded its `pre_llm_call` by 36–182 ms (S2) — (b) is exactly what catches
  that window. Measured once, on an idle host; re-measure under load in the
  live test before relying on 0.
- **Unhooked states.** Some input-capturing states fire no hook at all: the
  `/new` confirm modal, the hook-consent prompt (S11). The status reads `idle`
  while they are up. The [screen check](#the-screen-check) is what refuses
  them.
- A host with no status file for a session reports unknown, naming the
  missing hook, as `ErrNoStatus` does for Claude.

Status comes from the hook, the lease and the newest row only. The
[gateway viewer](#the-gateway-viewer) is used for answering, not for status.

### The send path

🔴 **The pane paste is the only send path.** The viewer's `prompt.submit` was
tested and refuted (S1): it is admitted and the turn runs, but the CLI in the
pane never draws it, live or later, so the person at the terminal never sees
the turn. It is not used. It may become usable if upstream makes the CLI render
foreign turns (upstream issue #103748 asks for an official way to deliver a
message into a live session).

**What Hermes does with keys** (S11; the classic CLI):

1. 🔴 **Paste+Enter on the approval modal approves `once`.** A bracketed paste
   of `4` (deny) landed in the input buffer, then muster's pasted Enter
   produced "allowed once" and **the command ran**. The pasted text does not
   matter. Only a **typed** digit picks a choice (1 once, 2 session, 3 always,
   4 deny; `hermes_cli/cli_tui_mixin.py:1301-1306`).
2. **Paste+Enter on a clarify with choices** locks the highlighted choice,
   which is the "(Recommended)" one, and leaves the pasted text in the input
   box for the next Enter to submit.
3. **Paste+Enter on a free-text clarify** answers it with the text: intended
   only when the person is answering it.
4. **Enter during a turn**, at the default `display.busy_input_mode:
   interrupt`, redirects the running turn; at `queue` it queues the message for
   the next turn.
5. **Enter within 50 ms of any buffer change is a newline**, bracketed paste
   included (`_tui_last_text_change`, `cli_tui_mixin.py:1751`). muster's
   `enterDelay` of 150 ms clears it (submitted at 40 ms and above, not at
   0–20 ms; idle host).
6. 🔴 **The sudo prompt** takes typed text as a password: a paste would be sent
   to `sudo`.

muster's `Tmux.PastePane` already sends the text as one bracketed paste and the
Enter as a separately pasted unbracketed newline, 150 ms later. It must stay a
paste, not `send-keys`: tmux refuses `send-keys` while muster's own read-only
viewer is attached. Measured: it submits once, idle, at both busy modes, and a
3,229-character paste arrives as one message.

**The gated send**, for every Hermes path: Send, Queue, Send now, a routed
message.

1. **Server.** A Hermes window accepts a send only while its status is `idle`.
   `busy` is refused ("Hermes is working; Enter would interrupt the turn. Queue
   it instead."), and so are `waiting` and unknown. Send now refuses `busy`
   too, unlike Claude Code's (which queues input itself). The paste message
   carries the `StatusSince` the decision was made on.
2. **Host, at the moment of acting**, `Runtime.CheckSend`:
   - the status file still reads `idle` with exactly that `since`, else refuse
     as superseded (nothing pasted, so the queue's Send-wins path puts the
     message back);
   - the false-idle checks above;
   - the [screen check](#the-screen-check) passes.
3. Paste the text, bracketed, with no Enter.
4. Wait `enterDelay`, then re-check the status file and the screen. The input
   line must now hold muster's text and nothing else may be drawn. If anything
   moved, do not press Enter: report "pasted, not submitted; the text is in
   Hermes's input box".
5. Press Enter as today's pasted newline.
6. After the Enter, if the input line still holds the text (a stalled event
   loop turned the Enter into a newline, item 5), report "pasted, not
   submitted". That direction is fail-safe.

Text rules: `mirror.SanitizePaste` as today. A leading `/` is passed through, as
for Claude Code; `/new` opens a confirm modal, which the screen check then
holds.

⚠ **Residual risk.** A turn can start on its own between step 4's re-check and
step 5's Enter (`auto_continue`, a background delegation finishing). With
`queue` mode the CLI also holds its own queue for about 0.3 s after a turn ends,
while the hook already says idle (S11); check (b) catches it once the user row
is written. The host docs recommend `/busy queue`, which the CLI saves to
`config.yaml` itself: any such gap then queues rather than interrupts.
muster never writes Hermes's config.

### The screen check

The screen is the only signal for states no hook reports, and the backstop for
the ones the hook does. The host captures the pane (`capture-pane -p -e`, the
bottom rows) and **admits exactly one state: an idle, empty `❯ ` input line
with no modal marker on screen.** Everything else refuses. The blocking states,
with their fixtures from S11:

| state | marker | input-line prefix | why it blocks |
|---|---|---|---|
| approval | `Dangerous Command`, `↑/↓ to select, Enter to confirm` | `⚠ ❯` | 🔴 Enter approves `once` |
| confirm (`/new` and similar) | `destroys conversation state`, `Type 1/2/3` | `⚠ ❯ type 1/2/3, …` | Enter approves; **no hook fires** |
| clarify, choices | `needs your input`, `Enter to lock, Tab next question` | `? ❯` | Enter locks the recommended choice |
| clarify, free text | `needs your input`, `Type your answer in the prompt below` | `✎ ❯` | the paste becomes the answer (allowed only through the asks UI) |
| sudo | `Sudo Password Required` | `type password (hidden), …` | 🔴 the paste becomes a password |
| hook consent | `Allow this hook to run? [y/N]:`, before the UI | none | the CLI has not started |

Also refused: a busy screen (spinner; `☤ ❯` prefix), and an idle line that is
**not empty** (leftover text would be submitted with muster's). A modal still
drawn after the hook or viewer reported it resolved is still a modal: a
viewer-answered approval leaves it on screen until a key is pressed (S1).

Markers and prefixes are pinned per pin bump by `make test-livehermes` against
the recorded fixtures. Skins or themes changing the glyphs are UNVERIFIED; an
unrecognised screen refuses.

### The gateway viewer

A second client can attach to the gateway as a **viewer** (S1). muster uses it
for one thing: **answering** an ask the person answers in muster. It is not a
send path and not a status source.

**Discovery.** 🔴 The control socket is at `$HERMES_HOME/gateway.sock` only
when that path is short enough (`gateway/control_socket.py`,
`_MAX_UNIX_PATH = 100`). Otherwise the gateway writes
`$HERMES_HOME/gateway.sock.path`, a 0600 pointer to
`$XDG_RUNTIME_DIR` or `$TMPDIR` `/hermes-gw-<16 hex>/control.sock`. muster-mirror
reads the pointer and applies upstream's checks before connecting: owner uid,
mode, and a directory name equal to the hash of the home
(`hermes_cli/gateway_runtime_discovery.py`, `_socket_path`). The socket rejects
a peer with a different uid.

**Attach.**

1. `identify` → `{kind:"hermes-gateway", code_sha, instance_id, state,
   api_origin, capabilities, served_profiles, …}`. `code_sha` is checked
   against the pin.
2. `session-ticket {profile_id, instance_id, purpose:"interactive"}` → a
   ticket valid for 30 s.
3. WebSocket `<api_origin>/api/ws`, subprotocols `hermes-gateway-v1` and
   `hermes-gateway-ticket.<ticket>`; the gateway answers `gateway.ready`.
4. `session.resume({session_id})` returns `prompts`, the pending approvals:
   `prompt_id`, `execution_generation`, `command`, `description`, `choices` in
   the order `once, deny, session, always`, `timeout_s`. That is what an answer
   needs. (It also returns `pending`, started admissions **with prompt text**,
   which is dropped.)

**Events used.** `approval.request` / `approval.settled` and `request.cancel`,
to keep the identifiers of what is pending current, and the `clarify` server
request (`method:"clarify"`, an `srq-…` id, the questions), which a passive
viewer **receives** although it declared no capabilities. Everything else is
dropped on receipt, `sudo` frames included.

**Rules.**

- 🔴 **Never answer a server request** unless the person is answering it
  through muster's asks UI, and **never answer, log or forward a `sudo`
  frame**.
- **Attach only to answer:** when the hook reports a listed window `waiting`
  on an approval or clarify that muster is showing, and detach once it is
  settled. The gateway idles out after 600 s without clients; whether an
  attached viewer keeps it alive is UNVERIFIED, and muster must not be the
  reason a gateway outlives its panes.
- No payload says which client or terminal shows a session, so the viewer
  never replaces the [locator](#pane-to-session-resolution).

### Questions and approvals

**What is visible where.**

- **Clarify** questions are persisted on the assistant row before the tool
  runs, and readable while pending (a `tool_call_id` with no `tool` row yet):
  `{"questions":[{"question","choices"?,"multi_select"?}]}`, 1–5 questions, at
  most 4 choices each (`tools/clarify_tool.py:95-108`). The answer is the later
  `tool` row.
- **A pending approval is in no row** (S5). The only sources are the hooks
  (`on_human_input_request kind=approval`, `pre_approval_request` with the
  command and description), the viewer (`approval.request`, `prompts` on
  resume), and the screen. From the DB, an unmatched `terminal` call under a
  live lease means "a tool is running **or** an approval is pending", nothing
  more.

**Mapping.**

- A Hermes **clarify** becomes muster's existing question card
  (`askq.Question`): question as header, choices as options, `multi_select` as
  checkboxes, the **Other** box for Hermes's always-present "Other (type your
  answer)". It follows the recorded answer from the `tool` row, as Claude
  Code's does.
- A Hermes **approval** is a new attention item: the command (capped,
  redacted), its description, the choices. `Pending` is `{Summary:
  "terminal: <command>", Tool: "terminal", Last: <last assistant text>}`,
  taken from the hook's `pre_approval_request` or the viewer.
- **A `sudo` request** is shown as "Hermes is asking for a sudo password in
  the terminal", with nothing to answer in muster.

**Answering.** Every ask kind has one path:

| ask | answered by |
|---|---|
| approval | the viewer's `approval.respond` |
| clarify | the viewer, if Phase 4 measures it; else the terminal |
| sudo | never from muster |

- **Approvals: the viewer's `approval.respond({session_id,
  execution_generation, prompt_id, choice})`.** Measured authoritative: the
  command did not run, the lease was released, the turn finished (S1). ⚠ The
  CLI's modal **stays drawn**, countdown running, until a key is pressed, and
  then prints a misleading `→ allowed once` line followed by "You denied this
  command — it did not run." The card says so: "answered here; the terminal
  will show a stale prompt until a key is pressed there". The screen check
  keeps treating that modal as blocking.
- **Clarify: through the viewer if measured, else in the terminal.** The
  protocol carries it: a passive viewer receives the `clarify` server request
  with its id and questions, and the viewers that did not answer get
  `request.cancel` (S1). Answering it from a viewer was **not** measured.
  Phase 4 measures it, including what the CLI's picker shows afterwards; until
  it passes, the card shows the questions and says "answer in the terminal".
- **Sudo: never answered from muster.** The card says Hermes is asking for a
  password in the terminal.
- **When the viewer cannot attach** (the socket fails its checks, `code_sha`
  is not the pin): every card says "answer in the terminal". muster types
  nothing into a pane to answer a Hermes ask.
- 🔴 **Choices offered from muster: `once` and `deny` only** by default.
  `session` and `always` widen what the agent may do beyond the moment being
  approved (open question 1).
- Approval defaults shape what muster ever sees: `approvals.mode: smart` lets
  an auxiliary model decide without prompting, and `timeout: 300` denies on
  timeout (`P:hermes_cli/config_defaults.py:1670-1713`). The card says when it
  timed out.

Questions and approvals live in muster's memory, as Claude Code's do.

### Listing, host icons and the rest of the Agents list

- **Kind.** `SessionView.Kind()` gains `"Hermes"` from `Window.Runtime`, so the
  card chip, `kind:hermes` in the filter and `type:mirrored` all work. Host
  icons and host tabs are per host and need no change.
- **Rendered view by default** for a Hermes window, Terminal one tap away.
- **Attention and push.** The hub's waiting set (`hub.go`, today
  `w.Claude && w.Status == waiting`) becomes runtime-neutral. A push names the
  tool only, as today.
- **Focus.** Keyed by the runtime-qualified session id.
- **Diagnostics.** `muster-mirror -list -windows` prints Hermes windows with
  `hermes <pane> <state> <session id> <source>`. `-list -hermes` adds the
  status file's state, hook consent, the gateway's `code_sha`, and why a window
  has no rendered view (a stale breadcrumb, a switched session).
- **The host's MCP endpoint** (`-mcp-listen`) identifies a caller by
  `X-Muster-Pane: ${TMUX_PANE}`. 🔴 At P, Hermes runs MCP clients in the
  gateway, whose `TMUX_PANE` is the first pane's, so that identity is wrong for
  Hermes. Listed under improvements.

### Secret hygiene

🔴 **muster-mirror reads only these Hermes paths**, read-only:

- `~/.hermes/state.db` (SQLite manages its `-wal`/`-shm`);
- `~/.hermes/terminal-sessions/tty-*`;
- `~/.hermes/gateway.sock`, or `~/.hermes/gateway.sock.path` and the hashed
  `hermes-gw-<16 hex>/` directory it points to (validated as above);
- muster's own `~/.muster/hermes-status/*.json`.

**Never** `auth.json`, `.env`, `config.yaml` (it can hold inline keys),
`profiles/*/.env`, `memories/`, `skills/`, `logs/`, `telemetry/`, or
`runtime/active_sessions.json` (no longer needed). Every Hermes path the host
opens goes through one function holding that allowlist, and a ledger test fails
when the set grows or shrinks.

From the viewer, before anything leaves the host, muster-mirror **drops**
`session.info.system_prompt`, `session.info.credential_hint` and
`pending[].text`, and every `sudo` frame. What remains passes the rendered
view's redaction and caps. Nothing is stored on the server.

## The hermes agent kind

### Mode: API server, driven by a supervisor

| mode | verdict |
|---|---|
| **API server** (`hermes gateway run`, `API_SERVER_ENABLED`, `gateway/platforms/api_server.py`) | **Chosen.** Structured tool, approval and terminal events; steer and stop; sessions persisted; no TTY. |
| `hermes chat -q … --format stream-json` | Rejected. No approval events (dangerous commands are blocked); at P it leaves a detached gateway per home that **inherits the caller's environment**, `API_SERVER_*` included, and so serves the API too (S7). hermesd must own the only gateway. |
| ACP (`hermes acp`) | Not chosen. Upstream issue #138949 reports ACP failing with a per-task `HERMES_HOME`. |
| muster calling Hermes's `/v1/responses` directly | Rejected. Different session header and continuity model; approvals are on `/v1/runs`; the API key would leave the pod; muster's health path is `/`. |

**The API surface hermesd uses**, at P (S6):

- `POST /v1/runs {input, session_id, instructions}` → `202 {run_id, status:
  "started"}`. `session_id` defaults to the run id when omitted, so hermesd
  always sends one.
- `GET /v1/runs/{id}/events`, SSE with `: keepalive` about every 10 s. Events
  consumed: `message.delta` (streaming text), `tool.started`,
  `tool.completed`, `tool.failed`, `approval.request`, `approval.settled`, and
  the terminal `run.completed`, `run.failed`, `run.cancelled`,
  `run.interrupted`. `run.queued`/`run.started` exist in source but were never
  observed and are not relied on.
- `POST /v1/runs/{id}/approval` with **exactly**
  `{"request_id": <approval.request.prompt_id>, "execution_generation": <n>,
  "choice": "once"|"deny"}`; any other key set is `400 invalid_params`
  (`P:gateway/platforms/api_server_authority_runs.py:113-133`).
- `POST /v1/runs/{id}/stop`, `/steer`; `POST /v1/runs/{id}/resolve-unknown
  {admission_id, execution_generation}`; `GET /v1/runs/{id}`;
  `GET /api/sessions/{id}/messages`.
- 🔴 **Only `/v1/runs` and streaming chat completions can be approved.** They
  register an approval notifier before the turn; any other API turn resolves
  from `approvals.unattended_mode` (default deny) without asking
  (`P:tools/approval.py`, `_presence`).
- 🔴 **An unanswered approval is denied after `approvals.timeout` (300 s), and
  the run still ends `run.completed`**, with a tool result starting
  `BLOCKED: Command timed out without user response`. muster would read that
  as success. So hermesd's relay timeout is **shorter** than the rendered
  `approvals.timeout` (proposed: relay 240 s, Hermes 300 s, both rendered
  explicitly), and a relay timeout is answered `deny` by hermesd itself.
- **The `clarify` tool is not offered to API-server turns**: the
  model's call gets "Tool 'clarify' does not exist" (S6). Question cards from
  pods need a toolset config that adds `clarify`, and then
  `POST /v1/runs/{id}/clarify` (P-only, untested end to end). Not in the first
  release of the kind.

### hermesd: the ccd equivalent

A new binary, `cmd/hermesd`, built from this module like `ccd`, so its wire is
tested against muster's own client. It is the pod's PID 1 parent of the
gateway.

| responsibility | ccd today | hermesd |
|---|---|---|
| serve muster's wire | `POST /v1/responses` on :18789, bearer `sha256("gw-"+HOOKS_TOKEN)` | the same port, bearer and wire; `GET /` and `/healthz` |
| run the agent | the `claude` TUI in tmux, supervised | `hermes gateway run` with the API server on **127.0.0.1** only, supervised with ccd's backoff, crash-loop latch and `/healthz` failure |
| the agent's own credential | none | a random `API_SERVER_KEY` per start, in memory, passed only to the child; stripped from the agent's shell by Hermes (S8) |
| one turn | paste, read the transcript, end on the Stop hook | `POST /v1/runs`, stream `/events` |
| streaming text | output_text deltas | `message.delta` → `response.output_text.delta` |
| the agent's own tools | `NativeToolItem` items, streamed as `output_item.done`, never in `response.completed` | the same: `tool.*` → `native_tool_call` items, so muster draws them and never dispatches them |
| permission prompts | `ccd approve` → `MUSTER_APPROVAL_GATE_URL` | `approval.request` → the same relay → the exact P body, `once` or `deny`; relay timeout → `deny` |
| instructions | the managed `CLAUDE.md` | sent as `instructions` on **every** run (see [instructions](#instructions)) |
| failure | typed `{"error":{type,code,…}}` | the same shape: `run.failed` → `runtime_error`, `run.interrupted` → `interrupted`, a provider 429 → `rate_limited` |
| busy | 409 `busy` | 409 `busy` while a run for that session is live |
| sessions | lanes, one CLI per explicit session | a muster session key → Hermes `session_id` (and last run id) map on the volume; one gateway serves all |
| readiness | the CLI's SessionStart hook | Hermes `GET /health` 200 (about 2 s after start, S8) **and** start-up recovery done |
| repository | `CCD_REPO` clone before the CLI starts | the same checkout and `not_ready` reasons; the rendered `terminal.cwd` is the checkout |
| seed | `ccd seed` | `hermesd seed` (see [volume](#volume-layout)) |

A muster message arriving while a run is live is a 409, never a steer, until
muster has a steer concept.

### Restart and resume

No run resumes (S6).

- **SIGTERM** (a rollout): the gateway drains for 30 s, then interrupts the run
  (`run.interrupted`, status `cancelled` at P) and **exits 1**. The transcript
  gets a `failed_turn` row. P prefixes the session's next user message with a
  system note that the previous turn was interrupted. hermesd returns the typed
  `interrupted` failure and does not retry.
  - The pod's `terminationGracePeriodSeconds` is **over 30 s** (proposed 45 s),
    or the kubelet's SIGKILL turns every in-flight run into the unknown case
    below.
  - hermesd forwards SIGTERM, and a gateway exit 1 **after hermesd sent
    SIGTERM** does not count toward the crash latch.
- 🔴 **SIGKILL or OOM** (P): the run is left `unknown`, and every later run on
  that session is admitted but **parks behind it**, while its SSE reports
  `run.failed {error: "unknown_execution"}`. That event is not terminal: once
  the unknown run is resolved, the queued run executes. A client that treats it
  as a failure and lets the person resend would run the turn **twice**.
  hermesd therefore:
  - at start, before answering any turn, `GET`s the last run id of every
    session in its map and calls `POST /v1/runs/{id}/resolve-unknown
    {admission_id, execution_generation}` for each `unknown` one (outcome
    `interrupted`), logging what it resolved;
  - treats `run.failed` with `unknown_execution` as "queued behind an unknown
    run": it keeps streaming, resolves the predecessor, and never reports it as
    a failure.
- Sessions and their messages survive any restart; the next run on the same
  `session_id` gets the full history. Run records survive a restart
  (`GET /v1/runs/{old}` answers with its final status).
- The `hermes sessions discard` step of the earlier draft is superseded by
  `resolve-unknown`.

### Image

The upstream image starts through s6-overlay as root and accepts `runAsUser`
only as 0 or its own uid 10000 (`docker/main-wrapper.sh:40-58`), which
conflicts with the restricted profile, and a commit pin has no published image.

**A Nix-built image** (measured, S8):

- upstream's flake at the pin, `packages.minimal` **with ffmpeg overridden
  out**: `minimal.override { ffmpeg = <empty>; }` (`P:nix/hermes-agent.nix`
  takes `ffmpeg` as a `callPackage` argument). Upstream `minimal` is a
  3,278 MiB closure, mostly a C toolchain pulled in by ffmpeg; `ffmpeg-headless`
  still references it. Overridden: **1,149 MiB closure, a 1.2 GB image, 425 MiB
  gzipped**. ffmpeg serves audio and voice, which the pod does not offer.
- built with `dockerTools.streamLayeredImage`, with `hermesd`, bash,
  coreutils, git, curl, cacert, ripgrep and procps;
- `/etc/passwd` and `/etc/group` entries for uid 1000 (without one,
  Hermes's real-home lookup falls back to a scratch directory);
- `/data` created owned by uid 1000; `User = "1000:1000"`;
- no init: Python as PID 1 handled SIGTERM; with hermesd in front, hermesd is
  PID 1.

It ran with a read-only root, `--cap-drop ALL` and `no-new-privileges`; the API
answered about 2 s after start, and a session survived a container restart.
Not measured: a cold build from an empty Nix store.

### Volume layout

One PVC at `/data`, as for `claude-code`:

| path | what |
|---|---|
| `/data/hermes` | `HERMES_HOME`: `state.db` (+ `-wal`/`-shm`), `response_store.db`, `runs_idempotency.db`, `config.yaml` (rendered), `SOUL.md` (fallback only), `memories/`, `skills/`, `sessions/`, `logs/` |
| `/data/hermes/home` | the agent shell's HOME; **created by `hermesd seed`**; git config lives here |
| `/data/hermes/cache/scratch` | the agent shell's `TMPDIR`, set by Hermes, pruned after 24 h idle: on the volume |
| `/data/workspace` | the agent's workspace and checkout |
| `/data/hermesd` | hermesd's session map |

- 🔴 **`state.db` needs a real local filesystem.** NFS and virtiofs are not
  supported (`website/docs/user-guide/docker.md:216-225`). A deployment
  requirement to document.
- 🔴 **No `.env` on the volume, ever.** `$HERMES_HOME/.env` loads with
  `override=True`, beating pod environment, and **clears** known keys it does
  not list (`P:hermes_cli/env_loader.py:510-518`). `hermesd seed` refuses to
  start when one exists (naming it, deleting nothing it did not write), and the
  supervisor checks again before every in-pod restart, because the agent can
  create one with `hermes config set`. The analogue of ccd's
  `settingsForbidden` strip.
- **The agent shell's HOME** is `$HERMES_HOME/home` only if that directory
  exists **and** either container detection succeeds or
  `TERMINAL_HOME_MODE=profile` is set (`P:hermes_constants.py:743-768`). So
  `hermesd seed` creates the directory and the spec sets
  `TERMINAL_HOME_MODE=profile` explicitly rather than relying on detection.

**The rendered `config.yaml`**, rewritten on every start so nothing the session
writes into it survives a restart:

- `_config_version: 51` (without it every start logs that the config predates
  auto-migration);
- `terminal.backend: local`; `terminal.cwd`: the checkout, else
  `/data/workspace` (the shell's cwd is `terminal.cwd`, else HOME — never the
  process cwd; it is also what makes the repository's `AGENTS.md` load);
- `approvals.mode: manual`, `approvals.timeout: 300` (longer than hermesd's
  relay timeout);
- `updates.check: false`; telemetry and OTLP export explicitly off (already
  default off);
- `sessions.auto_prune` explicit; toolsets without browser, computer use and
  voice.

### Credentials

- **The provider is OpenRouter, with the credential muster's gateway runtime
  already uses** (operator decision): the admin's `MUSTER_AGENT_OPENROUTER_KEY`,
  or a member's own stored OpenRouter key ([connectors.md](../connectors.md)).
  It goes into the agent's own Secret as `OPENROUTER_API_KEY`, consumed by
  `envFrom`, as `CLAUDE_CODE_OAUTH_TOKEN` does for `claude-code`, with the same
  seam-ledger test holding the name to that one place.
- **The agent's shell never sees it** (S8): provider keys (`OPENAI_API_KEY`,
  `ANTHROPIC_API_KEY` measured), `GH_TOKEN`, `GITHUB_TOKEN` and every `API_SERVER_*`
  variable are stripped from terminal subprocesses
  (`P:tools/environments/local_env_policy.py:38,380`). So the agent cannot call
  Hermes's own API.
- **`MUSTER_HOOK_TOKEN` and `MUSTER_CONNECT_URL` pass through unchanged** (S8);
  no `env_passthrough` configuration is needed.
- **OAuth providers are not offered for pods**: some rotate single-use refresh
  tokens into `auth.json`.
- **GitHub: no token in the pod**, as for `claude-code`. git reaches GitHub
  through muster's connector proxy, which injects a GitHub App token limited to
  the agent's repository ([github-app.md](../github-app.md)). `hermesd seed`
  writes ccd's `url.<proxy>.insteadOf`, `followRedirects` and
  `extraHeader = X-Muster-Token: …` lines with
  `HOME=$HERMES_HOME/home git config --global`. Measured: the agent's
  `git clone` went through a stand-in proxy with the token header (S8). The
  token then sits in `$HERMES_HOME/home/.gitconfig` on the volume, as it sits
  in ccd's HOME today.
- **muster's MCP server**, when the connector surface is offered, goes into
  `mcp_servers:` as an HTTP server with the hook token by `${MUSTER_HOOK_TOKEN}`
  expansion.

### Instructions

Measured (S9):

- **`/v1/runs` `instructions` is the path.** It is appended at the end of that
  run's system prompt, and leaves Hermes's identity and the repository's
  `AGENTS.md` intact (`P:gateway/platforms/api_server_runs.py:825`). It is
  **not** persisted: hermesd sends it on every run, so a change to muster's
  instructions takes effect on the next turn. agentspec mounts the text as a
  read-only file (`Options.Instructions`), which hermesd reads.
- **Fallback: `SOUL.md`** = the seeded default + muster's text. SOUL.md replaces
  only the ~670-character identity paragraph; the seeded file *is* that
  paragraph, so appending keeps it.
- **Never `.hermes.md`**: when present, the repository's `AGENTS.md` is not
  loaded.
- **Never `agent.system_prompt` or `HERMES_EPHEMERAL_SYSTEM_PROMPT`**: both are
  ignored for API-server turns.

### Sandboxing

Hermes's terminal tool in a pod is the `local` backend, so **the pod is the
sandbox**, exactly the `claude-code` profile:

- `RunAsUser`/`FSGroup` 1000, `RunAsNonRoot`, `Restricted`,
  `NoServiceAccountToken`, a read-only root filesystem except `/data` and
  `/tmp`;
- the same NetworkPolicy: ingress from muster only, egress to DNS, TCP 443 on
  public addresses and muster's connect port;
- `HERMES_WRITE_SAFE_ROOT=/data`.

`hermes egress` (iron-proxy) works with the docker backend only
(`P:hermes_cli/proxy_cli.py:1-30`). Hermes's hardline blocks and
`approvals.deny` still apply; `--yolo` and `HERMES_YOLO_MODE` are never set.
[limits.md](../limits.md)'s statement for `claude-code` holds: "public internet
on 443" is not "the provider and GitHub".

### muster-side changes for the kind

- `agents.KindHermes = "hermes"`, its `KindLabel`, `KindTurnTimeout` (longer
  than hermesd's own turn budget, as `ClaudeCodeTurnTimeout` documents), a
  migration widening `agents_kind_check`, and `Kinds`.
- `agentspec.buildHermes`, mirroring `buildClaudeCode`: every missing input a
  refusal naming it; `TERMINAL_HOME_MODE=profile`; a termination grace over
  30 s; a memory limit (the four-minute soak's RSS rose from about 275 MiB to
  328 MiB, undiagnosed).
- Configuration: `MUSTER_AGENT_HERMES_IMAGE` (pinned digest) and
  `MUSTER_AGENT_HERMES_STORAGE_SIZE`; the OpenRouter key is the existing one; documented in
  [configuration.md](../configuration.md), which a test checks.
- No Terminal view for `hermes` agents in the first release: `agents.terminal`
  refuses kinds other than `claude-code`.

## Phased plan

Efforts are rough engineer-days, including tests and docs. Phase 0 is done.

| phase | what | effort | depends on |
|---|---|---|---|
| ~~0~~ | Spikes S1–S11 | done | — |
| **1** | Runtime seam: `mirrorhost.Runtime` for Claude Code and Hermes, Claude behind it, `Window.Runtime`/`AgentSession` with the wider id bound, runtime-neutral waiting set and queue reasons. No behaviour change for Claude Code. | 3–4 | — |
| **2** | Read-only Hermes mirroring: process identity, breadcrumb locator with the end-reason staleness rule, SQLite reader (compaction, compression-only lineage), renderer, the `hermes-hook` command and its status files with lease and newest-row checks, consent diagnostics, listing and kind chip, secret-path ledger, `-list -hermes` | 6–8 | 1 |
| **3** | Gated pane send: per-runtime server gates, host `CheckSend`, the six-state screen check against recorded fixtures, post-paste and post-Enter checks, Queue and Send now for Hermes, host docs (the Home Manager hook declaration, `/busy queue`) | 5–6 | 2 |
| **4** | Questions and approvals: clarify cards from the DB, approval and sudo attention items from the hooks, viewer discovery through `gateway.sock.path`, attach-to-answer, `approval.respond` with the stale-modal card, measuring the viewer's clarify answer (and wiring it if it passes), the hygiene filters | 5–7 | 2, 3 |
| **5** | `hermes` kind: `hermesd` (runs, events, exact approval body, relay timeout, start-up `resolve-unknown`, SIGTERM handling), the Nix image without ffmpeg, `agentspec`, migration, config, smoke test like `cc-smoke`, a first run in a real pod | 8–10 | 1 |
| **6** | `make test-livehermes`: the pinned Hermes in a scratch tmux server and home, driving every screen-check state, the queue, the clarify picker, and the timings under load; re-run on every pin bump | 3–4 | 3, 4 |

Remaining: about 30–39 days. Each phase is released and shipped to the hosts as
it merges. Phases 2–4 and 5 are independent after Phase 1. The shortest useful
slice is Phases 1–2: read-only Hermes windows with status.

## Risks

- 🔴 **A misplaced Enter approves, picks a choice, sends a password or
  interrupts.** Mitigated by the one-state screen check, the status and DB
  checks, and the post-paste re-check; the residual window is named, and
  `/busy queue` narrows it.
- **Upstream churn.** Main moves daily; the rendered view parses Hermes's
  schema and the send path depends on its key bindings and glyphs. Every pin
  bump re-runs `make test-livehermes` and re-records the fixtures;
  muster-mirror refuses a schema or a screen it cannot read.
- **The pod's crash path.** An unknown execution blocks a session until
  resolved, and a mishandled `unknown_execution` doubles a turn. hermesd's
  start-up recovery is the mitigation; a pod killed before recovery runs leaves
  the session parked until the next start.
- **Untested in Phase 0, carried as risk:**
  - a real pip/uv install (S10's row is from a simulated launcher), and the bare
    `hermes` launch form;
  - threshold-triggered auto-compression (only `/compress` was used);
  - pasting into the sudo and `/new` modals (deliberately not done; inferred
    from the highlighted choice);
  - the `session` and `always` approval choices, and a viewer declaring
    `server_requests: true`;
  - answering a clarify from the viewer (measured in Phase 4);
  - whether an attached viewer keeps an auto-started gateway alive past its
    idle exit;
  - skins or themes changing the input-line glyphs;
  - every timing under load: the false-idle slack, the Enter boundary, the
    ~0.3 s queue gap;
  - a Kubernetes pod: container detection, the NetworkPolicy and restricted
    admission, a cold image build;
  - `/clarify` end to end on the API server;
  - the RSS growth during the soak (leak or history growth, not told apart);
  - a real provider: rate limits and compression in a pod;
- **Hermes's own concurrency issues** at P (upstream #140622, #139959,
  #135487, #133253, #131838, #120888): turns aborted by lock contention or a lost
  lease surface as failed turns in muster.
- **`smart` approvals** decide some prompts without a person; muster cannot show
  what it never sees.
- **Privacy.** `state.db` holds whole conversations, tool outputs included, as
  Claude Code's transcripts do. The host sends only bounded, redacted events;
  the viewer's `system_prompt`, `credential_hint`, pending text and `sudo`
  frames never leave it.

## Open questions for the operator

1. **Approval scopes from muster:** `once`/`deny` only, or also `session` and
   `always`?
2. **Host config advice:** recommend `/busy queue` on mirrored hosts? It changes
   what Enter does at the terminal for the person too.
3. **Question cards from pods:** add the `clarify` toolset to the pod (it is
   off for API-server turns), and with it the P-only `/clarify` route?
4. **A Terminal view for `hermes` pods** (a TUI viewer of the in-pod gateway):
   in scope for a later release?

## Recommended improvements

- **Upstream, small:** rewrite the terminal breadcrumb on `/resume` and
  `/branch`; have the CLI draw turns other viewers submit (which would make
  `prompt.submit` a safe send path, see #103748); clear the CLI's approval
  modal when another client settles it; a documented per-session status file
  for the CLI; a hook-reported event for confirm modals such as `/new`.
- **A runtime-neutral identity for the host MCP endpoint**, so Hermes sessions
  can call muster's attention and task tools.
- **A decisions.md entry** recording why muster pins Hermes by sha, refuses
  what it cannot read instead of supporting several versions, and does not send
  through the viewer.
