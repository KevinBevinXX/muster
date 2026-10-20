# Design: Hermes mirroring and the `hermes` agent kind

**Status: proposed. Nothing here is implemented.** This page is for review
before any code. Where it states how muster works today, the code is the
authority (see [decisions.md](../decisions.md)). Where it states how Hermes
works, it cites upstream source by path at a commit; claims marked
**UNVERIFIED** were read from source but never exercised, and Phase 0 exists to
settle the ones a decision depends on.

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

The core recommendation, in one line each:

- **Version:** target the gateway-runtime line the hosts already run, pinned by
  **commit sha**, for both the host side and the pod. Support v0.22.2 hosts in a
  degraded profile. Move to the first stable `vX.Y.Z` that contains the gateway
  runtime once one ships. This departs from "latest stable" and is called out as
  a decision for the operator below.
- **Pane → session:** the CLI's tty breadcrumb, validated against the live
  process. `active_sessions.json` is used only to recognise a Hermes CLI pid.
- **Transcript:** a read-only SQLite reader with WAL, cursor by `messages.id`,
  following compression through `parent_session_id`.
- **Status:** a muster-installed Hermes **shell hook** writing one status file
  per session id, cross-checked against the database before it is believed.
- **Send:** pane sends gated on idle, re-checked on the host at the moment of
  acting, with the Enter as a separate step and a refusal while waiting. The
  gateway viewer's `prompt.submit` is the preferred path, behind a spike.
- **Pod:** `hermes gateway run` with the API server bound to loopback, driven
  through `/v1/runs` and its SSE event stream by `hermesd`.

## Goals

- A Hermes tmux window is a first-class mirrored window: rendered view,
  Terminal view, Send, Queue, Send now, status-driven attention, questions and
  approvals, focus, filter and host icon, with no regression for Claude Code.
- Every send path is at least as safe as Claude Code's. 🔴 Hermes has key
  bindings where a misplaced Enter approves a command or interrupts a turn; the
  design must make that unreachable from muster, not merely unlikely.
- A `hermes` agent can be dispatched, kicked off, chatted with, and approved,
  with the same isolation as `claude-code`: uid 1000, no ServiceAccount token,
  the same NetworkPolicy, no credentials on the volume that outrank the pod's.
- The Claude-specific code paths become one runtime behind a small interface,
  so a third runtime costs a locator and a renderer, not another rewrite.

## Non-goals

- Hermes's Ink TUI (`hermes --tui`, `ui-tui/`). Only the classic
  prompt_toolkit CLI is mirrored.
- Hermes messaging platforms, Bot Chats, cron, webhooks, `hermes send`,
  `hermes proxy`, `hermes mcp serve`.
- Running Hermes's own docker/ssh/modal terminal backends, or its `hermes egress`
  credential-injecting proxy, inside an agent pod.
- Writing Hermes's database. muster-mirror is read-only on every Hermes file.
- A Terminal view for `hermes` agent pods in the first release (see
  [Agent kind](#the-hermes-agent-kind)).

## Sources and notation

Upstream is `github.com/NousResearch/hermes-agent`. Two refs are cited:

| short | ref | commit |
|---|---|---|
| **S** | `v0.22.2`, the latest stable release | `d923a5eb487a17c0a68801bba9ef0f56c81040a8` |
| **P** | the hosts' pin, canary `v0.22.2+canary.20261019T071120Z` | `5fc490e2b34123212d73ef316086f939567a725a` |

A citation `P:hermes_cli/cli_tui_mixin.py:1597` means
`https://github.com/NousResearch/hermes-agent/blob/5fc490e2b34123212d73ef316086f939567a725a/hermes_cli/cli_tui_mixin.py#L1597`.
Upstream user and developer docs are cited by their path under
`website/docs/` at the same commit. **PIN-ONLY** marks code present at P and
absent at S, checked by tree (file present, `git grep` at each ref), never by
ancestry.

## Versions: what differs, and why it matters

🔴 **P is newer than S, not older, and commit ancestry says otherwise.** S is a
release-branch commit; it records an older main commit as a second parent only
to satisfy the release script's ancestry check, and its message says the branch
must never be merged into main. So `git merge-base --is-ancestor X v0.22.2`
does not mean X's code is in S. The tree diff from S to P is about 3550 files.
Any check of "is feature F in version V" in muster's tooling must test the
**tree** (a file or symbol present), never ancestry.

The architectural difference is the **unified gateway runtime** (upstream PR
#106742), merged just after S's release branch was cut, so S does not have it:

- **At P**, `hermes chat` (the REPL and `-q`) is a thin client. The turn runs in
  a per-`HERMES_HOME` gateway process (`hermes gateway run --quiet --idle-exit`),
  auto-started by the first client and exiting after
  `gateway.unmanaged_idle_exit_seconds` (default 600). The CLI never builds an
  agent, runs a tool or writes `state.db` (`P:hermes_cli/gateway_agent.py:1-9`,
  `P:hermes_cli/gateway_client.py:257-301`,
  `P:website/docs/getting-started/updating.md:151-172`).
- **At S**, the CLI runs the agent in its own process
  (`S:hermes_cli/oneshot.py:514-520`; `hermes_cli/gateway_chat.py` is absent).

What that changes for muster:

| concern | S (v0.22.2) | P (gateway runtime) |
|---|---|---|
| who runs hooks | the CLI process (has the pane's tty and `TMUX_PANE`) | the gateway (has neither) |
| who writes `state.db` | the CLI | the gateway only (`P:hermes_cli/gateway_session_store.py:1-14`) |
| `sessions.source` of a CLI session | `cli` | `local`, ids `local-<64 hex>` |
| out-of-band submit into a live session | **none exists** | viewer `prompt.submit` (UNVERIFIED) |
| approvals and clarify as structured events | hooks only | viewer `approval.request` / `clarify.request` (UNVERIFIED) |
| turn-in-flight signal in the DB | `session_turn_leases` | `session_turn_leases`, plus `session_admissions` |
| `SCHEMA_VERSION` | 31 | 32 |
| API server routes (`/v1/runs`, events, `/approval`) | present | present, same env vars |
| `chat -q --format stream-json` | present, in-process | present, via an auto-started gateway |
| breadcrumbs, shell hooks, `on_human_input_*` | present | present |

### Downgrading a host from P to S is hazardous

"Latest stable" on the hosts means going **back** from P to S:

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

### Recommendation

1. **Hosts: support both, target P.** muster-mirror chooses its behaviour by
   **feature probes**, never by a version string: a live gateway socket it can
   attach to, the status files the hook writes, the columns `PRAGMA table_info`
   reports. A v0.22.2 host gets the degraded profile (pane sends only, approvals
   by keys or not at all); a gateway-runtime host gets the viewer paths if
   Phase 0 shows they work.
2. **Pod: pin by commit sha, to the same commit as the hosts.** The API server
   surface is the same at S and P, so either works for the pod; one commit
   across the fleet means one set of recorded fixtures and one live test. If the
   Phase 0 pod spike finds P's gateway runtime unstable headless (upstream issues
   #140622 and #139959 are open lock-contention and lease-starvation reports),
   fall back to S for the pod only.
3. **Pin by sha, never by canary tag.** Canary tags are deleted 14 days after
   they are cut (`P:scripts/release.py:575-580`). A flake input is
   `github:NousResearch/hermes-agent?rev=<40-hex sha>`. Committed source always
   reports version `0.0.0` (`website/docs/developer-guide/stable-releases.md:8`),
   so muster records the sha, not a version.
4. **Move both to the first stable `vX.Y.Z` that contains the gateway runtime**,
   checked by tree (`hermes_cli/gateway_agent.py` present at the tag), once one
   ships. Until then "stable" and "what the hosts run" are different programs.

🔴 **This is a fork from the operator's stated "latest stable".** The reason is
that the hosts already run P and moving them to S loses the gateway runtime and
risks the database. Your call: (a) the recommendation above; (b) pod on S now,
hosts stay on P; (c) downgrade hosts to S and drop every PIN-ONLY path from
this design (no viewer, so no safe structured send or approval).

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
    gate (below). Claude's is a no-op, which preserves today's behaviour.
  - `Submit(ctx, pane, text) error` — paste-and-Enter for Claude; for Hermes,
    viewer `prompt.submit` when available, else the gated pane send.
  - `Answer(ctx, pane, ask, choices) error` — today's `AnswerAsk` for Claude.
- The tmux backend holds a list of runtimes and asks each in turn; the first
  that claims a pane wins. A pane is claimed by process identity, never by
  `#{pane_current_command}`: a Hermes CLI is a Python process, and what tmux
  reports as its command under a Nix wrapper is not a stable name (Phase 0).
- **Wire**, with no version bump, in the established style (an older muster
  ignores unknown fields; an older host never sends them):
  - `Window.Runtime` (`"claude"`, `"hermes"`). `Window.Claude` stays true only
    for Claude Code, so an older muster shows a Hermes window as a plain
    terminal window and never treats it as Claude.
  - `Window.AgentSession`, the session id for any runtime.
    🔴 **A Hermes id does not fit today's field.** P's ids are
    `local-<64 hex>`, 70 characters; `claudeSessionPattern` (wire.go, also
    `internal/focus` and migration 0020) admits at most 64. `NormalizeWindows`
    blanks an invalid id, so Hermes windows would silently lose focus and
    attention identity. The new field needs its own bound (proposed 1–128,
    `[0-9A-Za-z][0-9A-Za-z_-]*`) and `TestTheLargestWindowListFitsOneFrame`
    re-measured; focus keys become runtime-qualified (`hermes:<id>`).
  - `Msg.Runtime` on `watch`/`paste`/`answer`, so the host can refuse a
    message meant for one runtime landing in another.
- `cctranscript.Event` stays the render model. Hermes rows are translated into
  it on the host; the page needs no new kinds for the first release.

The Claude path must stay byte-identical through the refactor; Phase 1 is that
refactor alone, gated by the existing suite.

### Pane → session resolution

Three Hermes artefacts could name a pane's session. Only one is usable.

**`~/.hermes/runtime/active_sessions.json` — not authoritative.**
Written by `hermes_cli/active_sessions.py` (identical at S and P) under a flock,
one lease per process: `{lease_id, session_id, surface, pid,
process_start_time, started_at, updated_at, …}`.

- No heartbeat: `updated_at` moves only on a transfer (`:625`, `:692`).
- Dead owners are pruned lazily, only when another process acquires or
  releases; after a SIGKILL the entry stays. Readers must re-check `pid` and
  `process_start_time` themselves. Upstream issue #105714: a clock step prunes
  live leases.
- The CLI never calls `transfer_active_session`, so `/new`, `/resume`,
  `/branch` and compression all leave the entry's `session_id` stale.
- At P the lease is claimed with a locally minted id and the CLI then adopts the
  gateway's stored id (`P:hermes_cli/cli_init_mixin.py:381,446-448`), so even a
  fresh CLI's entry can name the wrong session. Inferred from code and
  consistent with one host's three live entries (two fresh: mismatched; one
  resumed: matched). UNVERIFIED at runtime.

Use: "this pid is a Hermes CLI", and nothing more.

**`~/.hermes/terminal-sessions/tty-<sanitised tty>` — the mapping.**
Written by `hermes_cli/terminal_breadcrumbs.py:77-94` (S and P), atomically,
`{session_id, cwd, ts}`. The file name comes from `os.ttyname(stdin|stdout)`,
so `/dev/pts/3` becomes `tty-dev-pts-3`; environment variables are used only
without a tty (`:37-51`). It is rewritten on init, on gateway-id adoption, on
`/new` and on compression sync; **not** on `/resume` or `/branch`. It is never
deleted on exit (pruned after 30 days, `:20-22,64-74`) and is gated by
`session.terminal_continue` (default true;
`website/docs/user-guide/sessions.md:126-128`).

**`~/.hermes/sessions/sessions.json` — irrelevant.** Its own `_README` calls
it a legacy mirror of the gateway routing index, not a session list.

**Recommended resolution** (the `HermesLocator`):

1. Walk the pane's process tree from `#{pane_pid}` to the nearest Hermes CLI
   process (argv identity, Phase 0 S10). Record its pid and `/proc` start time.
2. Read `#{pane_tty}`, sanitise it as upstream does, read that breadcrumb.
3. Believe it only if the CLI process has that tty on fd 0 **and** the
   breadcrumb's `ts` is not older than the process's start. A breadcrumb older
   than its process is a previous occupant's: no rendered view.
4. Confirm the session exists in `state.db` and is not `archived`/`hidden`.
   Follow compression to the tip (below).
5. `Source` reports `breadcrumb`, as `pane-map`/`sessions`/`guessed` do today;
   `muster-mirror -list -windows` prints it.

There is no "guess" fallback: two panes in one `cwd` are common, and a guessed
session above a composer that sends to the right pane is the failure
`LocatePanes` already refuses for Claude.

⚠ **Known gap: `/resume` and `/branch` inside a running CLI** leave the
breadcrumb naming the previous session. Mitigations, in order of preference:

- an upstream change to rewrite the breadcrumb on `/resume` and `/branch` (one
  call each in `cli_commands_mixin.py`; recommended, small);
- **at S only**, a muster hook can write a real pane map, because hooks run in
  the CLI process and see `TMUX_PANE` and the tty: `hermes-p<N>.json` with the
  same validation as `claude-p<N>.json`. 🔴 **At P this does not work**: hooks
  run in the gateway, whose environment and tty are whichever client
  auto-started it. A P pane map must come from the breadcrumb.
- at P, the viewer protocol may name the session a client is attached to
  (Phase 0 S1).

### Transcript reading

The store is `~/.hermes/state.db`, SQLite in WAL mode. Reading it externally
read-only is documented as supported: the dashboard and `hermes sessions list`
do so (`website/docs/developer-guide/session-storage.md:292-309,344-348`).

**Opening it.**

- One long-lived connection per host, `mode=ro`, never `immutable=1` (which
  would ignore the WAL and show stale data).
- A read-only connection never checkpoints. It may still create `-wal`/`-shm`
  sidecars, and the `-shm` must be writable for WAL reads, so muster-mirror must
  run as the uid that owns `~/.hermes`. It already must, to read `/proc` and
  attach to tmux.
- `busy_timeout`, and retry `SQLITE_BUSY` and transient `SQLITE_IOERR` (which
  upstream's own reader retries around checkpoints,
  `P:hermes_state_readpool.py:28-34`).
- The driver must be pure Go so muster-mirror needs no C toolchain. It is a new
  dependency, and the largest one the host binary would carry; measure the
  binary size in Phase 2.
- Columns are feature-detected with `PRAGMA table_info`, so one reader serves
  schema 31 (S) and 32 (P). A required column that is missing disables the
  rendered view for that host with a logged reason; it is never guessed.

**Schema used** (`P:hermes_state_common.py:444-546`; the
`website/docs/developer-guide/session-storage.md` page describes the same):

- `sessions`: `id`, `source`, `parent_session_id`, `end_reason`, `cwd`,
  `title`, `model`, `last_activity_at`, `archived`, `hidden`.
- `messages`: `id` (INTEGER AUTOINCREMENT), `session_id`, `role`, `content`,
  `tool_call_id`, `tool_calls` (JSON text), `tool_name`, `timestamp`,
  `finish_reason`, `active`, `compacted`, `display_kind`, `display_order`,
  `message_uid`, `tool_call_uid(s)`.
- Never read: `reasoning`, `reasoning_content`, `reasoning_details`,
  `api_content`. muster never shows thinking, and the reader does not select
  those columns at all.

**Write timing** is per message, not per turn: the assistant row carrying
`tool_calls` is flushed before the tools run (`agent/turn_tool_round.py:131-133`),
each tool result after it completes (`agent/tool_executor.py:199-210`), the
final reply at the end (`agent/turn_final_response.py:364`). Streaming text
deltas are not persisted, so the rendered view shows a reply when it is
written, not token by token. That matches Claude Code's transcript.

**Lineage and compression.** Compression ends a session with
`end_reason='compression'` and continues in a child whose `parent_session_id`
points back (`P:hermes_state_compression.py:587-611`). The view's `Gen` is the
lineage **root** id, so a compression does not reset the page. The reader
follows the chain to the tip and reads `session_id IN (<lineage>)`.

**Live rows, scrollback and rewrites.**

- In-place compaction marks old rows `active=0, compacted=1` and re-inserts the
  retained context as new `active=1` rows, so the same logical message appears
  more than once across generations. Live view: `active=1`. Scrollback:
  `active=1 OR compacted=1`, ordered by `display_order`
  (`session-storage.md:79-84`).
- `message_uid` is a stable logical id that survives compaction, clone and
  rotation while `id` changes (`session-storage.md:269`). The view de-duplicates
  on it.

**Incremental recipe.**

1. Backfill: the last `MaxBackfillEvents` rendered rows, the same bound the
   Claude view uses, ordered by `display_order`.
2. Tail: `… WHERE session_id IN (<lineage>) AND id > :cursor ORDER BY id`,
   polled every `ViewPoll`. The cursor is `MAX(id)` seen; it travels in the
   existing `Msg.Offset`, and `Gen` identifies the lineage, so the page's
   resume logic is unchanged.
3. Also track `COUNT(active)`, `MAX(id)` and, at P, `SUM(transcript_revision)`.
   `transcript_revision` (PIN-ONLY) moves only on in-place rewrites, not on
   appends (`P:hermes_state_common.py:771-780`); upstream's own fingerprint
   combines all three (`P:hermes_state_messages.py:1486-1497`). A change that
   appends do not explain is a rewind, undo, compaction or rewrite: send a
   `Reset` backfill.

**Rendering into `cctranscript.Event`.**

| Hermes row | Event |
|---|---|
| `role='user'`, `display_kind` NULL | `prompt`; `Line` = `messages.id`, so the prompt index works |
| `role='assistant'`, `content` non-empty | `text` |
| each `tool_calls[]` entry `{id, function:{name, arguments}}` | `tool_call`, `ToolID` = `id`, `Tool` = name, `Target` from the arguments, `Input` capped |
| `role='tool'` with `tool_call_id` | `tool_result`, `OK` from the stored status when present |
| assistant row with `finish_reason` an error, or `display_kind='failed_turn'` | `error` |
| `display_kind` `process_complete`, `async_delegation_complete`, `model_switch`, `steer`, `auto_continue`, `skill_invocation`, `personality_switch`, `internal_notification` | `notice`, `Notice` = the kind |
| `display_kind='hidden'`, `role='system'`, `role='session_meta'` | nothing |
| a reasoning-only reply (empty `content`) | nothing |

`Target` extraction needs a per-tool table, like Claude's (`terminal` →
the command, file tools → the path). The `display_kind` set was taken from a
grep of upstream `agent/`, `gateway/` and `tools/`; an unknown kind renders as a
`notice` naming it, never as user text. Every field goes through the host's
existing redaction (`redactLines`) and caps before it is framed.

### Busy, idle and waiting

Claude Code writes its own status and the time it wrote it; the send queue's
correctness rests on that time. **Hermes has no status file or IPC for the CLI
in either version.** The candidates:

- `~/.hermes/hooks/` (HOOK.yaml + handler.py) are gateway **messaging** hooks;
  the CLI does not load them (`website/docs/user-guide/features/hooks.md:28,84-98,368`).
- `gateway_state.json` holds an aggregate `active_agents` count per home, not
  per session (`gateway/status.py:41,828-829,1090-1130`).
- `session_turn_leases` (S and P): a row with `expires_at > now` means a turn
  is in flight (`P:agent/turn_facade_lease.py:25-26,52,113-117,322-326`). It
  stays held while the turn waits on an approval, so it means "in flight", not
  "working", and it carries no transition time. Its TTL is UNVERIFIED (300 s in
  code, about 900 s on a live row).
- **Shell hooks** (`hooks:` in `config.yaml`) fire in CLI, gateway, desktop,
  TUI and dashboard sessions (`hooks.md:1849`; valid events in
  `hermes_cli/plugins.py:107-149`). They receive JSON on stdin:
  `hook_event_name`, `session_id`, `cwd`, `profile`, `extra`
  (`hooks.md:445-498,1847-1941`).

**Recommended: a muster-installed shell hook writing a per-session status
file.** One command, `muster-mirror hermes-hook`, registered for:

| event | writes |
|---|---|
| `pre_llm_call` | `busy` |
| `on_human_input_request` (`kind` ∈ `approval`, `clarify`, `sudo`) | `waiting`, with `kind` and `request_id` |
| `on_human_input_resolved` | `busy` |
| `on_session_end` (fires at every turn finalisation: completed, failed or interrupted) | `idle`, with the reason |
| `on_session_finalize` | `idle` |

Payload sources: `tools/human_input_hooks.py:61-72`,
`hooks.md:1515-1577`. The file:

```json
{"v":1,"session_id":"local-…","status":"idle","since":1700000000000,"kind":"","request_id":"","writer_pid":123,"writer_start":"4567"}
```

- **Where.** `~/.muster/hermes-status/<session_id>.json` (directory 0700, file
  0600), written by rename. The hook finds the directory the same way
  `muster-mirror ask` finds its config: from `~/.muster/mirror-hook.json`,
  written by the managed daemon, because a hook does not inherit the service's
  environment. At P the hook's environment is the gateway's, which is whichever
  client auto-started it, so `$XDG_RUNTIME_DIR` is not a dependable location.
- 🔴 **Keyed by session id, never by pid or pane.** At P the hook runs in the
  gateway, not in the pane's process. The session id is the join key between
  the status file and the breadcrumb. Whether the hook's `session_id` is the
  logical id the breadcrumb holds, or the physical id after compression, is
  UNVERIFIED (Phase 0 S2). The locator looks up every id in the lineage.
- **Monotonic.** The hook writes under a lock and only when its `since` is newer
  than the file's. Two hook processes for one session must not reorder `idle`
  behind `busy`.
- **Contents.** The status, its time, the human-input kind and request id.
  🔴 Never the prompt text (it arrives redacted anyway) and never tool
  arguments. What a waiting session waits on is read from the database.
- **Consent.** Shell hooks need consent: an allowlist entry,
  `--accept-hooks` or `HERMES_ACCEPT_HOOKS`. At P the gateway re-reads `hooks:`
  about every 30 s and never prompts; only a foreground CLI shows the prompt
  (`P:agent/shell_hooks.py`, `P:hermes_cli/hooks.py:263-265`). The install
  instructions pre-seed the allowlist. A hook that is not consented writes
  nothing, and that must read as **unknown**, never as idle.

**How it feeds muster.** The locator sets `AgentPane.Status` and
`StatusSince` from the file, exactly as it does from Claude Code's sessions
file. The server half needs no new rule: `Hub.AgentState`, the "status time
newer than the paste" rule and the 30-second idle wait (`MUSTER_SEND_QUEUE_IDLE`)
apply as written. The hook stamps a fresh `since` at every turn end, so a turn
shorter than the host's 5-second refresh still moves it, which is the property
the send queue's comment in `agentstate.go` depends on.

🔴 **Believing a status file: stale `busy` and false `idle`.**

- A `busy` file is stale if the writer (`writer_pid` + start time) is gone, or
  if no turn lease exists for the lineage. Stale busy reads as **unknown**
  (holds the queue), never as idle.
- A false `idle` is the dangerous direction: a turn that started without its
  `pre_llm_call` hook running (a hook timeout, consent lost, a turn started by
  `auto_continue` or a background delegation completing). `idle` is believed
  only if (a) no turn lease is live, and (b) no `messages` row in the
  lineage has a `timestamp` newer than `since` plus a small slack measured in
  Phase 0. Otherwise the status is **unknown**.
- A host whose status directory holds no file for a session reports unknown,
  with a reason naming the missing hook, as `ErrNoStatus` does for Claude.

The richer alternative, at P only, is the viewer event stream (below), which
carries turn and request events directly. If S1 shows a passive viewer is safe,
the status can come from it, with the hook kept as the S path and as a
cross-check.

### Sending, and why it is gated

🔴 **Three Hermes key behaviours make an ungated paste unsafe.** All are in
the classic CLI, at S and P alike.

1. **Enter on an approval prompt approves.** While the approval modal is up,
   Enter selects the highlighted choice whatever the input buffer holds
   (`hermes_cli/cli_tui_mixin.py:1597-1599`), and the highlight starts on
   `once` (`hermes_cli/cli_modal_mixin.py:1104`). Digit keys pick at once:
   1 = once, 2 = session, **3 = always**, 4 = deny
   (`cli_tui_mixin.py:1301-1306,2064-2066`). So a paste plus Enter approves the
   command, and a pasted digit can approve it **for always**.
2. **Enter during a turn interrupts it.** `display.busy_input_mode` defaults to
   `interrupt` (others: `queue`, `steer`;
   `P:hermes_cli/config_defaults.py:845`, `cli_tui_mixin.py:1530-1555`).
3. **Enter within 50 ms of a text change is a newline**, not a submit, when the
   text did not arrive as a bracketed paste (`cli_tui_mixin.py:47,1445-1450`).

A free-text clarify question also takes typed text plus Enter, so a paste
answers it. That is intended only when the person is answering that question.

**What muster does today, and what changes.**

- `Tmux.PastePane` already sends the text as one bracketed paste
  (`paste-buffer -p`) and the Enter as a **separate** unbracketed pasted
  newline, 150 ms later (`enterDelay`). That clears behaviour 3 by a factor of
  three. It is a paste, not `send-keys`, because tmux refuses `send-keys` while
  muster's own read-only viewer is attached; that must stay.
- Today's Send to a Claude Code window does **not** consult status, and Send
  now refuses only `waiting`, deliberately sending while busy. Both are right
  for Claude Code, which queues input during a turn. For a Hermes window they
  are wrong, so the gates below are per runtime.

**The gated pane send**, for every Hermes path: Send, Queue, Send now, a routed
message.

1. **Server.** A Hermes window accepts a send only while its status is `idle`.
   `busy` is refused ("Hermes is working; Enter would interrupt the turn. Queue
   it instead."), and so are `waiting` and unknown. Send now refuses `busy` too,
   unlike Claude Code's. The paste message carries the `StatusSince` the
   decision was made on.
2. **Host, at the moment of acting**, `Runtime.CheckSend`:
   - re-read the status file: still `idle`, with exactly the `since` muster
     decided on, or refuse as superseded (nothing pasted, so the queue's
     Send-wins path puts the message back);
   - the false-idle checks above (no live lease, no newer row);
   - a **screen check**: capture the pane and refuse if the approval or
     clarify modal is drawn. The modal's shape is pinned by the live test.
     This is the fallback detector for a modal the hook did not report.
3. Paste the text, bracketed, with no Enter.
4. Wait at least `enterDelay`, then **re-check** the status file and the
   screen. If anything moved, do not press Enter: report "pasted, not
   submitted; the text is in Hermes's input box", which the page shows as it
   shows an unconfirmed paste.
5. Press Enter as today's pasted newline.

Text rules: `mirror.SanitizePaste` as today. A leading `/` is passed through, as
for Claude Code; Hermes slash commands such as `/new` and `/resume` change the
session, and the locator follows (with the `/resume` gap above).

⚠ **Residual risk.** Between step 4's re-check and step 5's Enter (well under a
millisecond) a turn can still start on its own (`auto_continue`, a background
delegation finishing). With the default `busy_input_mode` that Enter
interrupts it. The install instructions recommend `display.busy_input_mode:
queue` on mirrored hosts, which turns that case into a queued message. muster
never writes Hermes's config.

**The preferred path: the gateway viewer's `prompt.submit` (PIN-ONLY,
UNVERIFIED).** At P every local session is owned by the gateway, and a second
client can attach as a **viewer**:

1. connect to `$HERMES_HOME/gateway.sock`, which rejects a peer with a
   different uid (`P:hermes_cli/gateway_runtime_discovery.py:124-148`);
2. ask for a `session-ticket`;
3. open the WebSocket `…/api/ws` with the ticket as the subprotocol
   (`P:hermes_cli/gateway_client.py:34-40,220-254`);
4. `session.resume({session_id})` returns history plus `inflight` and
   `open_requests`; `prompt.submit({session_id, submission_id, text})` is a
   durable admission with FIFO queueing
   (`P:website/docs/developer-guide/gateway-local-sessions.md:29-33`,
   `website/docs/developer-guide/programmatic-integration.md:58-105`).

No keystrokes reach the pane, so behaviours 1–3 cannot happen. A submission
made mid-turn is queued, not an interrupt. `submission_id` can be muster's
send nonce, so a retried send is idempotent upstream as well as in muster.
Writing `session_admissions` rows directly is not an option: they are fenced
by epoch, generation and principal (`P:hermes_state_runtime.py:146-170,212-228`).

What must be true before it replaces the pane send (Phase 0 S1):

- the CLI in the pane **renders** a turn another viewer submitted, so the
  person at the terminal sees it;
- attaching a viewer that does not declare `client.capabilities
  {server_requests: true}` does not withdraw or disturb the CLI's own approval
  prompt (`programmatic-integration.md:99`);
- the session the viewer resumes is the one the pane shows.

Upstream issue #103748 asks for an official way to deliver a message into a
live session; this path may become that, or be superseded by it.

### Questions and approvals in the asks UI

**Sources, best first.**

1. **Viewer events (PIN-ONLY, UNVERIFIED).** `approval.request {command,
   description, choices, prompt_id, execution_generation}`, answered by
   `approval.respond({session_id, execution_generation, prompt_id, choice})`;
   a choice not in `choices` becomes deny. `clarify.request {prompt_id,
   question, choices, multi_select}`. Pending items appear in `open_requests`
   on resume (`P:hermes_cli/gateway_agent.py:990-1024,1290-1315`).
2. **The database (S and P), for clarify.** The `clarify` tool's arguments,
   `{"questions":[{"question","choices"?,"multi_select"?}]}` with 1–5 questions
   and at most 4 choices each (`tools/clarify_tool.py:95-108,136-187`), are
   persisted on the assistant row **before** the tool runs, and are readable
   while the question is pending (a `tool_call_id` with no `tool` row yet). The
   answer arrives as the later `tool` row: `{"responses":[{question,
   choices_offered, status, user_response}], "outcome"}`.
3. **Hooks.** `on_human_input_request` gives `kind`, `request_id` and a
   redacted prompt only, with no choices; `pre_approval_request` gives the
   command and description.

A pending **approval** is not a message row: the dangerous command is inside a
`terminal` tool call's arguments, and whether any row records the pending
decision is UNVERIFIED (Phase 0 S5).

**Mapping.**

- A Hermes **clarify** becomes muster's existing question card
  (`askq.Question`): the question as the header, its choices as options,
  `multi_select` as checkboxes, and the **Other** box for Hermes's always-present
  "Other (type your answer)". Questions come from the database row, so they work
  at S and P. The card follows the recorded answer exactly as it follows Claude
  Code's (*Answered here*, *Answered in the terminal*), from the `tool` row.
- A Hermes **approval** is a new attention item, not a question card: the
  command (capped, redacted), its description, and the choices. `Pending` for the
  attention queue is `{Summary: "terminal: <command>", Tool: "terminal",
  Last: <last assistant text>}`.
- **Answering**, best first: viewer `clarify`/`approval.respond` (P, after S1);
  else typed keys through a Hermes picker driver built like
  `internal/askdrive` (read the pane twice before every key, stop on any
  divergence, never half an answer), pinned by a live test; else show it and say
  "answer in the terminal".
- 🔴 **Approval choices offered from muster: `once` and `deny` only** by
  default. `session` and `always` widen what the agent may do for longer than
  the moment being approved; offering them on a phone is a separate decision
  (open question 2). A key-driven approval types `1` or `4` only, after the
  screen check proves the modal is up with that command.
- Approval defaults matter to what muster ever sees: `approvals.mode: smart`
  lets an auxiliary model approve or deny without prompting
  (`tools/approval_smart.py`), and `timeout: 300` denies on timeout
  (`P:hermes_cli/config_defaults.py:1680-1713`). In `smart` mode some approvals
  never reach the person or muster. The card must say when it timed out.

Questions and approvals live in muster's memory, as Claude Code's do.

### Listing, host icons and the rest of the Agents list

- **Kind.** `SessionView.Kind()` gains `"Hermes"` from `Window.Runtime`, so the
  card chip, `kind:hermes` in the filter and `type:mirrored` all work. Host
  icons and host tabs are per host and need no change: a Hermes window draws its
  host's icon like any other.
- **Rendered view by default** for a Hermes window, with Terminal one tap away,
  as for Claude Code.
- **Attention and push.** The hub's waiting set (`hub.go`, today
  `w.Claude && w.Status == waiting`) becomes runtime-neutral. A push names the
  tool only, as today.
- **Focus.** Keyed by the runtime-qualified session id (above).
- **Diagnostics.** `muster-mirror -list -windows` prints Hermes windows with
  `hermes <pane> <state> <session id> <source>`. A `-list -hermes` row adds the
  status file's state and whether the hook is consented.
- **The host's MCP endpoint** (`-mcp-listen`) identifies a caller by
  `X-Muster-Pane: ${TMUX_PANE}`. 🔴 At P, Hermes runs MCP clients in the gateway,
  so that header names the wrong pane or none. Offering muster's attention and
  task tools to Hermes sessions needs a session-id identity instead; it is
  listed under improvements, not in this design's phases.

### Secret hygiene

🔴 **muster-mirror reads only these Hermes paths**, and opens them read-only:

- `~/.hermes/state.db` (and its `-wal`/`-shm`, which SQLite manages);
- `~/.hermes/terminal-sessions/tty-*`;
- `~/.hermes/runtime/active_sessions.json` (optional, pid recognition only);
- muster's own `~/.muster/hermes-status/*.json`;
- at P, `~/.hermes/gateway.sock` (a socket, for the viewer).

**Never** `auth.json` (OAuth tokens, some single-use refresh tokens), `.env`
(provider keys), `config.yaml` (can hold inline keys), `profiles/*/.env`,
`memories/`, `skills/`, `logs/` or `telemetry/`. Every Hermes path the host
opens goes through one function holding that allowlist, and a ledger test fails
when the set grows or shrinks. A `profiles/<name>/` home is a separate
`HERMES_HOME` and is out of scope until asked for.

What is read is a conversation, the same trust as a Claude Code transcript: the
rendered view's redaction and caps apply, nothing is stored on the server, and
the database is never sent whole.

## The `hermes` agent kind

### Mode: API server, driven by a supervisor

| mode | verdict |
|---|---|
| **API server** (`hermes gateway run` with `API_SERVER_ENABLED`, port 8642; `gateway/platforms/api_server.py`) | **Recommended.** Same HTTP surface at S and P; structured tool, approval and terminal events; steer and stop; sessions persisted in `state.db`; no TTY. |
| `hermes chat -q … --format stream-json` (`hermes_cli/stream_json.py`) | Rejected for turns. No approval or clarify events; `-q` uses `approvals.single_query_mode` (default deny); one process per turn; at P it auto-starts a gateway anyway and refuses several flags (`P:hermes_cli/gateway_chat.py:16-46`). |
| ACP (`hermes acp`, JSON-RPC on stdio) | Not chosen. Upstream issue #138949 reports ACP failing with a per-task `HERMES_HOME`. |
| muster talking to Hermes's `/v1/responses` directly | Rejected. Continuity uses `previous_response_id`/`conversation` or `X-Hermes-Session-Id` (chat completions only), not muster's session header; approvals are on `/v1/runs`, not the responses wire; the API key would leave the pod; muster's health path is `/`. |

API server details (`gateway/config_env.py:112-120`, `gateway/platforms/api_server.py:1646`,
`gateway/platforms/api_server_runs.py:243-249`, `website/docs/user-guide/features/api-server.md`):

- `API_SERVER_KEY` must be at least 16 characters and not a placeholder, or the
  server refuses to start (`gateway/platforms/api_server.py:4426-4435`, `gateway/config.py:544-552`).
- `POST /v1/runs` with `session_id` for continuity (`gateway/platforms/api_server_runs.py:577-591`);
  `GET /v1/runs/{id}/events` is SSE with a `: keepalive` every 10 s; events
  `run.queued`, `run.started`, `assistant.delta`, `assistant.completed`,
  `tool.started`, `tool.progress`, `tool.completed`, `tool.failed`,
  `approval.request`, `approval.responded`, `run.steered`, `run.stopping`, and
  terminal `run.completed|failed|cancelled|interrupted`
  (`programmatic-integration.md:187-201`).
- `POST /v1/runs/{id}/approval`, `/steer`, `/stop`. A run waiting on approval
  parks as `waiting_for_approval` (`gateway/platforms/api_server_runs.py:964-965`).
- 🔴 **Only `/v1/runs` and streaming chat completions can be approved.** They
  register an approval notifier before the turn runs; any other API-server turn
  is an unattended platform and resolves from `approvals.unattended_mode`
  (default deny) without asking (`P:tools/approval.py`, `_presence`). hermesd
  therefore drives every turn through `/v1/runs`. The `approval.request` event
  carries a redacted command, by upstream design.
- UNVERIFIED at S: `/v1/runs/{id}/clarify`, `/resolve-unknown`,
  `/api/sessions/{id}/model`; whether run records survive a restart.

### `hermesd`: the `ccd` equivalent

A new binary, `cmd/hermesd`, built from this module like `ccd`, so its wire is
tested against muster's own client. It is the pod's foreground process.

| responsibility | ccd today | hermesd |
|---|---|---|
| serve muster's wire | `POST /v1/responses` on :18789, bearer `sha256("gw-"+HOOKS_TOKEN)` | the same port, bearer and wire; `GET /` and `/healthz` |
| run the agent | the `claude` TUI in tmux, supervised, `--resume <id>` | `hermes gateway run` with the API server on **127.0.0.1** only, supervised with ccd's backoff, crash-loop latch and `/healthz` failure |
| the agent's own credential | none | a random `API_SERVER_KEY` minted per start, held in memory, passed only to the child; never on the volume, never sent to muster |
| one turn | paste the prompt, read the transcript, end on the Stop hook | `POST /v1/runs` for the session's Hermes id, stream `/events` |
| streaming text | output_text deltas | `assistant.delta` → `response.output_text.delta` |
| the agent's own tools | `NativeToolItem` items, streamed as `output_item.done`, never in `response.completed` | the same: `tool.started`/`completed`/`failed` → `native_tool_call` items, so muster draws them and never dispatches them |
| permission prompts | `ccd approve` relays to `MUSTER_APPROVAL_GATE_URL` | `approval.request` → the same relay → `POST …/approval` with `once` or `deny`; a relay timeout is `deny` |
| failure | typed `{"error":{type,code,…}}` (failure.go) | the same shape: `run.failed` → `runtime_error`, `run.interrupted` → `interrupted`, provider 429 → `rate_limited` |
| busy | 409 `busy` | 409 `busy` while a run for that session is live |
| sessions | lanes, one CLI per explicit muster session | a muster session key → Hermes `session_id` map on the volume; one gateway serves all of them |
| readiness | the CLI's SessionStart hook | Hermes `GET /health` 200 |
| repository | `CCD_REPO` clone before the CLI starts | the same checkout code and `not_ready` reasons; Hermes's working directory is the checkout |
| seed | `ccd seed`: settings, trust, hooks, MCP | `hermesd seed` (below) |

Steer and stop map to muster's existing cancel. A muster message that arrives
while a run is live is a 409, never a steer, until muster has a steer concept.

### Image

The upstream image (`nousresearch/hermes-agent`, Debian 13 pinned by digest)
starts through s6-overlay as root and accepts `runAsUser` only as 0 or its own
`hermes` uid 10000 (`docker/main-wrapper.sh:40-58`, `Dockerfile:175,542-543`).
That conflicts with the restricted profile every `claude-code` pod runs under.
Published tags move only on a release, so a commit pin has no upstream image.

**Recommended: a Nix-built image** from upstream's flake at the pinned sha
(`packages.minimal` or `default`, `nix/packages.nix:73-93`), with `hermesd`,
`git`, `curl`, `ca-certificates` and `ripgrep`, built with
`dockerTools.buildLayeredImage` as muster's own image already is, running as
uid 1000 with no init system.

- Upstream's flake has no OCI output, and its NixOS `container.enable` mode runs
  Ubuntu under docker or podman (`nix/nixosModules.nix:299-327`), so the image
  definition is muster's.
- The `default` package does not bring ffmpeg, node, npm or ripgrep at runtime
  (a Nix install warns "install out of sync"). The pod disables the browser and
  computer-use toolsets rather than shipping Chromium.
- Python `>=3.11,<3.15` (`pyproject.toml:15`).
- The fallback, if the Nix build proves costly, is the upstream image by digest
  with the entrypoint replaced by `hermesd` and `runAsUser: 10000`. That needs a
  per-kind uid in `agentspec`, and only works for a commit that has a published
  image.

### Volume layout

One PVC at `/data`, as for `claude-code`:

| path | what |
|---|---|
| `/data/hermes` | `HERMES_HOME`: `state.db` (+ `-wal`/`-shm`), `response_store.db`, `runs_idempotency.db`, `config.yaml` (rendered), `memories/`, `skills/`, `sessions/`, `logs/`, `cache/` |
| `/data/hermes/home` | the HOME Hermes gives terminal and code subprocesses (`website/docs/user-guide/docker.md:200-214`): git config lives here |
| `/data/workspace` | the agent's workspace and checkout |
| `/data/hermesd` | hermesd's session map |

- 🔴 **`state.db` needs a real local filesystem.** NFS and virtiofs are not
  supported (`docker.md:216-225`). The provisioner's StorageClass must be a
  local one; this is a deployment requirement to document, not something muster
  can check.
- 🔴 **No `.env` on the volume, ever.** `$HERMES_HOME/.env` loads with
  `override=True`, beating pod environment, and it **clears** known keys that it
  does not list (`P:hermes_cli/env_loader.py:510-518`). An empty `.env` would
  silently remove the pod's provider key. `hermesd seed` refuses to start when
  one exists (it names the file and does not delete data it did not write), and
  the supervisor checks again before every in-pod restart, because the agent can
  create one with `hermes config set`. This is the analogue of ccd's
  `settingsForbidden` strip.
- `config.yaml` is rendered from a template on every start, so nothing the
  session writes into it survives a restart. Values pinned there:
  `updates.check: false`; telemetry and OTLP export explicitly off (already
  default off, `P:hermes_cli/config_defaults.py:2092-2100,2340-2353`);
  `terminal.backend: local`; `approvals.mode: manual` (the values are `manual`, `smart`, `off`;
  `P:hermes_cli/config_defaults.py:1670-1671`); `sessions.auto_prune`
  explicit (its default differs between S and P); `agent.clarify_timeout`.
- `auth.json` is not used in pods (below).

### Credentials

- **Provider keys** go into the agent's own Secret, consumed by `envFrom`, as
  `CLAUDE_CODE_OAUTH_TOKEN` does for `claude-code`, with the same seam-ledger
  test holding each name to that one place. Hermes reads `OPENROUTER_API_KEY`,
  `ANTHROPIC_API_KEY`, `OPENAI_API_KEY` + `OPENAI_BASE_URL` and others
  (`website/docs/reference/environment-variables.md:15-95`). Which keys a
  deployment offers, and whether members bring their own, follows
  [auth.md](../auth.md)'s credential model (open question 3).
- **OAuth providers are not offered for pods.** Some rotate single-use refresh
  tokens into `auth.json`, which would then have to live on a writable volume
  (`website/docs/user-guide/profiles.md:105`).
- **GitHub: no token in the pod**, as for `claude-code`. git reaches GitHub
  through muster's connector proxy, which injects a GitHub App token limited to
  the agent's repository ([github-app.md](../github-app.md)). Two Hermes facts
  shape the wiring:
  - `GH_TOKEN` and `GITHUB_TOKEN` are always stripped from terminal and code
    subprocesses and cannot be re-allowed
    (`P:tools/environments/local_env_policy.py:38,380`). muster never sets them,
    so this costs nothing.
  - The terminal's HOME is `$HERMES_HOME/home`, so the `url.<proxy>.insteadOf`
    and `extraheader` lines `ccd connect-git` writes with `git config --global`
    must be written into **that** HOME.
  - Whether `MUSTER_HOOK_TOKEN` and `MUSTER_CONNECT_URL` reach terminal
    subprocesses (the agent's `curl` to the connector proxy needs them) is
    UNVERIFIED; if Hermes strips them, they need `env_passthrough` or P's
    `client_env_passthrough` (Phase 0 S8).
- **muster's MCP server**, when the connector surface is offered, goes into
  `mcp_servers:` as an HTTP server (`url`, `headers`;
  `website/docs/user-guide/features/mcp.md:34-39`), with the hook token by
  `${MUSTER_HOOK_TOKEN}` expansion rather than written into the file.

### Sandboxing

Hermes's terminal tool in a pod is the `local` backend, so **the pod is the
sandbox**, exactly the `claude-code` profile:

- `RunAsUser`/`FSGroup` 1000, `RunAsNonRoot`, `Restricted`,
  `NoServiceAccountToken`;
- the same NetworkPolicy: ingress from muster only, egress to DNS, TCP 443 on
  public addresses and muster's connect port;
- `HERMES_WRITE_SAFE_ROOT=/data`.

`hermes egress` (iron-proxy) works with the docker backend only
(`P:hermes_cli/proxy_cli.py:1-30`), so it adds nothing here. Hermes's hardline
blocks and `approvals.deny` still apply in every mode
(`tools/approval.py:985-990`); `--yolo` and `HERMES_YOLO_MODE` are never set.
[limits.md](../limits.md)'s statement for `claude-code` holds unchanged: "public
internet on 443" is not "the provider and GitHub".

### Restart and resume

- The volume holds `state.db` and hermesd's session map, so a restarted pod
  continues each muster session in the same Hermes session.
- A run in flight at shutdown ends as `run.interrupted`
  (`programmatic-integration.md:197`). hermesd returns the typed failure; it
  does not retry the turn.
- At P, mid-turn work left at a cutover becomes `unknown` and needs
  `hermes sessions discard` (`P:website/docs/getting-started/updating.md:151-172`).
  hermesd runs the discard for its own sessions at start and logs what it
  discarded.
- Run records may be in memory only (UNVERIFIED), so hermesd re-derives state
  from `state.db` through `/api/sessions/{id}/messages`, never from a run id it
  held before the restart.

### muster-side changes for the kind

- `agents.KindHermes = "hermes"`, its `KindLabel`, `KindTurnTimeout` (longer
  than hermesd's own turn budget, for the reason `ClaudeCodeTurnTimeout`
  documents), a migration widening `agents_kind_check`, and `Kinds`.
- `agentspec.buildHermes`, mirroring `buildClaudeCode`: every missing input a
  refusal naming it; `Instructions` placed where Hermes loads persistent
  instructions (the file is an open question: Phase 0 S9).
- Configuration: `MUSTER_AGENT_HERMES_IMAGE` (pinned digest), the provider key
  variables, `MUSTER_AGENT_HERMES_STORAGE_SIZE`; documented in
  [configuration.md](../configuration.md), which a test checks.
- No Terminal view for `hermes` agents in the first release: `agents.terminal`
  already refuses kinds other than `claude-code`. At P a later release could run
  `hermes chat --resume <id>` in tmux as a viewer of the in-pod gateway.

## Phased plan

Efforts are rough engineer-days, including tests and docs.

| phase | what | effort | depends on |
|---|---|---|---|
| **0** | Spikes (below) | 4–6 | — |
| **1** | Runtime seam: `mirrorhost.Runtime`, Claude behind it, `Window.Runtime`/`AgentSession` with the wider id pattern, runtime-neutral waiting set and queue reasons. No behaviour change for Claude Code. | 3–4 | — |
| **2** | Hermes read-only mirroring: `HermesLocator` (breadcrumb), SQLite reader, renderer, `hermes-hook` status files, listing and kind chip, secret-path ledger, `-list` diagnostics | 6–8 | 0 (S2, S3, S10), 1 |
| **3** | Gated pane send: server gates per runtime, host `CheckSend` with screen check, Queue and Send now for Hermes, the install docs (hook consent, `busy_input_mode`) | 4–5 | 2, 0 (S11) |
| **4** | Questions and approvals: clarify cards from the DB, approval attention items; answering through the viewer, or a key driver with a live test | 5–7 | 2, 0 (S1, S5) |
| **5** | Viewer path at P: `prompt.submit` send, viewer status and requests | 3–5 | 0 (S1 positive) |
| **6** | `hermes` kind: `hermesd`, the Nix image, `agentspec`, migration, config, smoke test like `cc-smoke` | 8–10 | 0 (S6, S8, S9) |
| **7** | `make test-livehermes`: a real pinned Hermes in a scratch tmux server and a scratch `HERMES_HOME`, driving the modal, the queue and the clarify picker; re-run on every pin bump | 2–3 | 3, 4 |

Phases 2–5 and 6 are independent after Phase 1 and can run in parallel. The
shortest useful slice is Phases 0–2: read-only Hermes windows with status.

### Phase 0: spikes

Each settles an UNVERIFIED item a decision rests on. All run against a scratch
`HERMES_HOME` and a scratch tmux server, never the operator's live sessions.

| # | question | decides |
|---|---|---|
| **S1** | At P: does gateway.sock → `session-ticket` → `/api/ws` attach work for a second client? Does `session.resume` return `open_requests`? Does the pane's CLI **render** a turn another viewer submitted with `prompt.submit`? Does attaching a viewer without `server_requests` disturb the CLI's own approval prompt? | whether Phase 5 exists; the approval answer path |
| **S2** | Is the hook's `session_id` the logical id or the post-compression physical id? Do shell hooks receive `on_human_input_request`/`resolved` end to end? Does pre-seeded consent hold in an auto-started gateway? Order and spacing of `on_session_end` versus the final row's `timestamp` (the false-idle slack). | the status source and its key |
| **S3** | Does the breadcrumb track compression and `/new`? Confirm `/resume` and `/branch` leave it stale. Does the CLI's id stay logical across compression at P? | the locator; the upstream change to ask for |
| **S4** | `session_turn_leases`: is the TTL 300 s or about 900 s, and is a lease held across an approval wait? | stale-busy and false-idle checks |
| **S5** | Is a pending **approval** recorded in any row? | whether approvals can be shown at S |
| **S6** | At S: do `/v1/runs/{id}/clarify`, `/resolve-unknown`, `/api/sessions/{id}/model` exist? Do run records survive a gateway restart? Is P's gateway runtime stable headless under one API client? | the pod's version and resume design |
| **S8** | In a pod as uid 1000: the Nix image builds and starts; which env vars reach a terminal subprocess (`MUSTER_HOOK_TOKEN`, `MUSTER_CONNECT_URL`); git through the connector proxy with config in `$HERMES_HOME/home` | image and credential wiring |
| **S9** | Where muster's `Options.Instructions` go. `$HERMES_HOME/SOUL.md` is always loaded but **replaces** the built-in identity; a workspace `.hermes.md` would win over, and so suppress, a repository's own `AGENTS.md` (only one project context type loads; `P:website/docs/user-guide/configuration.md:3015-3035`). Measure SOUL.md as the upstream default identity plus muster's text. | the instructions path |
| **S10** | How a Hermes CLI process identifies itself under the Nix wrapper and under pip/uv (`comm`, `argv[0]`, the entry script), and what `#{pane_current_command}` shows | pane detection |
| **S11** | With muster's own `PastePane` (bracketed text, pasted CR 150 ms later): does Hermes submit, at default and `queue` busy modes? Capture the approval and clarify modals' screens for the screen check. Does a large paste's file-reference placeholder still submit? | the send path's mechanics |

Not blocking, left open: whether Nous Portal refresh tokens are single-use
(pods avoid OAuth anyway); what `~/.hermes/sandboxes/` holds; LSP packaging;
(S7) which process `-q` auto-starts at P and how it is shared (muster does not use
`-q`); whether a `+` in a canary tag ref works in a flake input (muster pins by
sha, which sidesteps it).

## Risks

- 🔴 **A misplaced Enter approves or interrupts.** Mitigated by the gates above;
  the residual window is named, and `busy_input_mode: queue` narrows it.
- **Upstream churn.** About 2000 commits separate S and P, and main moves daily.
  The rendered view parses Hermes's schema and the send path depends on its key
  bindings. Every pin bump re-runs `make test-livehermes` and re-records the
  fixtures; muster-mirror refuses a schema it cannot read rather than guessing.
- **Unverified viewer protocol.** The preferred send and approval paths may not
  work as documented. The design ships without them (pane send, DB clarify) and
  adds them only on a positive S1.
- **False idle from a missed hook.** The DB cross-checks hold; a consent or
  hook failure reads as unknown, so the queue holds rather than sends.
- **Two programs called "stable".** S and the hosts' P differ architecturally.
  Feature probes, not version strings, keep muster honest about which it faces.
- **Hermes's own concurrency issues** at P (upstream #140622, #139959,
  #135487, #133253, #131838, #120888): turns aborted by lock contention or a lost
  lease surface as failed turns in muster. A reader holding a long read
  transaction can delay checkpoints; the reader keeps transactions short.
- **`smart` approvals** decide some prompts without a person; muster cannot show
  what it never sees.
- **Privacy.** `state.db` holds whole conversations, tool outputs included, as
  Claude Code's transcripts do. The host sends only bounded, redacted events.

## Open questions for the operator

1. **Version target** (above): the pinned gateway-runtime line now and the first
   stable that contains it later, or v0.22.2 now?
2. **Approval scopes from muster:** `once`/`deny` only, or also `session` and
   `always`?
3. **Provider credentials for `hermes` pods:** one admin key set, a pool like
   the Claude accounts, or members' own stored keys only?
4. **Host config advice:** recommend `display.busy_input_mode: queue` on
   mirrored hosts? It changes what Enter does at the terminal for the person too.
5. **A Terminal view for `hermes` pods** (a TUI viewer of the in-pod gateway, P
   only): in scope for a later release?

## Recommended improvements

- **Upstream, small:** rewrite the terminal breadcrumb on `/resume` and
  `/branch`; a heartbeat (or `transfer_active_session`) in the CLI's
  `active_sessions` entry; a documented per-session status file for the CLI.
  Each removes a heuristic from this design. Upstream #103748 (an official way
  to deliver a message into a live session) is the long-term send path.
- **A runtime-neutral identity for the host MCP endpoint**, so Hermes sessions
  can call muster's attention and task tools the way Claude
  Code sessions do.
- **`make test-livehermes`** on every pin bump, as `make test-liveclaude` is on
  every Claude Code upgrade, plus fixtures recorded from the pinned commit
  (schema DDL, a compressed lineage, a clarify call, each modal's screen).
- **Metrics**: `muster_mirror_hermes_status_total{state}`,
  `…_send_refused_total{reason}` (busy, waiting, modal_on_screen, superseded,
  pasted_not_submitted), `…_schema_unreadable_total`.
- **A decisions.md entry** once the version question is settled, recording why
  muster pins Hermes by sha and probes features instead of reading versions.
