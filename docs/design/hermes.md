# Design: Hermes mirroring and the `hermes` agent kind

**Status: proposed, with Phases 0, 1, 2 and 5 done.** Phase 5, the `hermes`
agent kind, is implemented (`cmd/hermesd`, `images/hermes-agent`, the kind in
`internal/agents` and `internal/agentspec`, migration `0037`); what it built
differently from this page is under [Phase 5 as built](#phase-5-as-built).
Phase 1 is implemented: the
[host paste guard](#every-paste-into-a-hermes-pane-is-gated-on-the-host)
(Phase 1a, without its chrome check, moved to Phase 3), and the
[runtime seam](#a-runtime-seam-instead-of-claude-only-code), the
[server changes](#server-changes) and the session-key migration (Phase 1b;
what it built differently from this page is under
[Phase 1 as built](#phase-1-as-built)). The strict mirroring identity rule,
moved to Phase 2 by operator decision, was pulled forward from Phase 2 into 1b
by coordinator decision, to avoid churn; Phase 2 builds on it. Phase 2 is
implemented (see [Phase 2 as built](#phase-2-as-built)): a Hermes window's
session is resolved from its terminal's breadcrumb and its status from the
`hermes-hook` status files; it has a read-only rendered view, the Hermes kind
chip and `-list -hermes`; there is no send path. The Home Manager declaration
of the hook and its allowlist entries is the host configuration's (devrc).
Nothing of Phases 3–4 is implemented. The
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
  a screen check of the image region and the whole input line, and is
  re-checked on the host at the moment of acting, with the Enter as a separate
  step. The viewer's `prompt.submit` was refuted: the CLI never draws the turn.
  🔴 Three overlays (the palette, the model picker, the stash) leave no mark on
  the input line. A bounded Escape sequence reduces that risk without removing
  it; Send stays off until Phase 3 has measured it, then is enabled with the
  remaining cases accepted as residual risk (operator decision).
- **Questions and approvals:** clarify cards from the database, approval and
  sudo items from the hooks. Approvals are answered through the gateway viewer's
  `approval.respond`; clarify through the viewer if Phase 4 measures it, else in
  the terminal; sudo is never answered from muster.
- **Every** paste into a pane holding a Hermes CLI, from any view or server
  version, is gated by the host on the pane's process identity. Until the gated
  send exists, it is refused. This closes a hazard in shipped muster.
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
  the same NetworkPolicy, and no file on the volume that overrides the pod's
  provider credentials (no `.env`). The one credential hermesd keeps on the
  volume is the in-pod API key; see [credentials](#credentials) for what it
  grants.
- The Claude-specific code paths become one runtime interface, sized for the
  two runtimes it serves: Claude Code and Hermes.

## Non-goals

- Hermes's Ink TUI (`hermes --tui`, `ui-tui/`). Only the classic
  prompt_toolkit CLI is mirrored.
- Hermes messaging platforms, Bot Chats, cron, webhooks, `hermes send`,
  `hermes proxy`, `hermes mcp serve`.
- Running Hermes's own docker/ssh/modal terminal backends, or its `hermes egress`
  credential-injecting proxy, inside an agent pod.
- Writing Hermes's database or config. muster-mirror never writes Hermes
  data (its read-only SQLite connection does create the `-wal`/`-shm` files a
  WAL reader needs when they are missing, as any reader does).
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
- **The provider key is readable by the agent (accepted, "same as
  claude-code"):** a `hermes` pod's agent can read the OpenRouter key from the
  gateway's environment in `/proc`. Hermes strips it from the shell's own
  environment, which is not a boundary; the exposure is accepted as parity
  with `claude-code` pods, and the smoke test claims only the shell's own
  environment.
- **The kind merges dormant:** `hermes` is off unless `MUSTER_AGENT_KINDS`
  includes it **and** `MUSTER_AGENT_HERMES_IMAGE` is set. It is enabled only
  after a run in a real pod passes, which also measures the NetworkPolicy, the
  restricted admission and the `Idempotency-Key` replay live.
- **Send with a residual overlay risk (accepted):** Send and Queue for Hermes
  windows stay **off** until Phase 3's measurements are done. Then they are
  enabled with every mitigation in place, **even if** Phase 3 cannot prove the
  overlay handling complete. The cases left uncovered are named in
  [the screen check](#residual-cases-accepted) as a known, accepted residual
  risk. The real cure stays upstream (see
  [improvements](#recommended-improvements)).

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
  `make test-livehermes` (Phase 3) against it before moving the pin.
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
    for Claude Code, so an older muster never treats a Hermes window as
    Claude. 🔴 It does show it as a plain terminal window, and that is **not**
    safe on its own: see the next section.
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

### Every paste into a Hermes pane is gated on the host

🔴 **This hazard exists in shipped muster today.** The Terminal view's composer
(`POST /mirror/terminal/send`) reaches the host as a paste with `AnyPane` set,
which `lookupPaste` sets for any window on a pane-paste host
(`internal/mirror/transcript.go`). The host's `PastePane` then pastes and
presses Enter into whatever runs in the pane
(`internal/mirrorhost/transcript.go`, `hostSide.paste`), with no runtime check.
Into a Hermes approval modal, paste+Enter approves `once` (S11). Any muster
server, old or new, offers this path.

**The decision: the host refuses an `AnyPane` paste into a pane that holds a
Hermes CLI**, decided at the host's paste entry, never by `Msg.Runtime`, so the
guard holds whatever the server sends. It sits in the host's one way of typing
into a pane (`hostSide.typeInto`), so the rendered view's paste into Claude
Code and an answer typed into Claude Code's picker are refused the same way: a
Claude Code pane with Hermes in the foreground (`claude &`, then `hermes chat`)
is a Hermes pane. A ledger test fails when a new way of typing reaches tmux
without it.

Until a Hermes rendered view exists (Phase 2) with its gated send (Phase 3),
the refusal cannot point at it, so it reads: "Not sent: Hermes runs in this
pane. muster never pastes into Hermes, because Enter there can answer a prompt
Hermes is showing, such as a command approval. Type it in the terminal;
sending to Hermes from its own rendered view comes in a later release."
(`mirror.PasteRefusedHermesText`). The host names the refusal's kind
(`Msg.Refusal` on `pasted`: `hermes`, `multiplexer`, `unchecked`), so muster
shows the same words when the browser posted the form without script.

**Implemented (Phase 1a):** `internal/mirrorhost/hermes.go`.
`HermesIdentity.InTree` walks the tree from `#{pane_pid}` (read by the pane's
id at the moment of the paste) with `comm` and argv from `/proc`, following
both the parent links and the kernel's own children lists, and `hermesShape`
is the broad match below, which Phase 2's strict rule builds on. Deferred, by
operator decision: the chrome check below moves to **Phase 3** (it needs the
recorded fixtures, none of which are in the tree), and the strict mirroring
identity rule to **Phase 2**, where the locator needs it. (Later, the strict
rule was pulled forward from Phase 2 into 1b by coordinator decision, to avoid
churn: `hermesCLIShape`, beside this match. Phase 2 builds on it.)

- 🔴 **The guard uses the broad match, not the mirroring rule, and fails
  closed.** It refuses when the pane's process tree holds **any** of:
  - a process with `comm == "hermes"` and `basename(argv[1]) == "hermes"`,
    whatever follows;
  - a Python interpreter (`basename(argv[0])` starting with `python`) running
    Hermes's code another way: `-m hermes_cli.main` (or any `-m hermes_cli…`),
    or a script path containing `/hermes_cli/`, or a `cli.py` inside a Hermes
    install (a directory holding `hermes_cli/`). These run the interactive CLI
    with `comm=python`. As implemented this is a **flat scan** of every
    argument after `argv[0]`, never a parse of Python's options: any argument
    containing `hermes_cli`, or whose basename is `hermes` (the launcher run
    as `python …/bin/hermes`), refuses, and so does a `cli.py` argument beside
    a `hermes_cli/` directory. That covers `-c` code importing `hermes_cli`
    too, and can only refuse more;
  - 🔴 a **terminal multiplexer client** (`tmux`, `screen`, `dtach`, `abduco`,
    `zellij`). `TMUX= tmux -L other attach` in a pane shows a session of
    another server, whose processes are not in this pane's tree, so a Hermes
    there is invisible to the walk (an audit reproduced it). Every nested
    multiplexer is refused: a false refusal for one that runs no Hermes,
    accepted as safe;
  - a process in the tree whose `stat` or `cmdline` cannot be read, the root
    missing, an unreadable `/proc`, or a tree larger than the walk's bound
    (4,096): the guard cannot rule Hermes out, so it refuses. A zombie is
    skipped.

  The strict
  [mirroring rule](#pane-to-session-resolution) also wants `argv[2] == chat`,
  which fails open for a profile-launched CLI: `hermes -p coder chat` runs as
  `[python, hermes, -p, coder, chat]` (`P:hermes_cli/main.py:500,595-667`).
  **The gateway is not excluded.** Its argv
  (`[python, -m, hermes_cli.main, …, gateway, run, …]`) matches the patterns
  above, and upstream ships a hand-run foreground gateway ("Run gateway in
  foreground (recommended for WSL and Docker)",
  `P:hermes_cli/subcommands/gateway.py:64-65,110`; `P:hermes_cli/main.py:60-62`),
  so an operator can run `hermes gateway run` in a pane. Refusing a paste into
  that pane is a false refusal, which is safe; a false admit is not.
- **It also refuses when the screen shows Hermes's chrome** (the status bar and
  input rules around a `❯ ` input line, from the recorded fixtures), as a
  best-effort extra. ⚠ That check **fails open** exactly when the chrome is not
  drawn: after a resize, the status bar and rules stay hidden until the next
  keypress (`P:hermes_cli/cli_tui_mixin.py:139-140,2177-2179`); with the status
  bar turned off; and in a narrow pane.
- **Known gaps**, named: a Hermes CLI run over `ssh` or `docker exec` inside the
  pane (invisible to the local process walk), in one of the chrome-less states
  above; a Hermes launched under a renamed interpreter or a copied tree the
  argv patterns do not match; and, **ratified by the operator**, a host with no
  `/proc` (macOS), where the guard sees no processes and admits. Failing closed
  there would take the Terminal view's message box away from every macOS
  muster user, and this deployment has no macOS hosts running Hermes. The
  guard does not try to classify a pane as "not Hermes": it refuses on any
  Hermes-shaped process and admits otherwise, with these gaps documented.
- **Accepted false refusals:** a pane running a nested multiplexer (above);
  `hermes gateway run` in a pane (above); and a Claude Code pane whose tree
  holds a Hermes process Claude Code started itself, such as `hermes mcp serve`
  configured as an MCP server or `hermes acp`: every paste and answer into that
  Claude Code pane is refused.

- It ships in **Phase 1**, first, in the host binary, before any other Hermes
  work, with the broad process identity. The strict mirroring rule moves to
  Phase 2 and the chrome check to Phase 3 (operator decision). The strict rule
  was later pulled forward into Phase 1b (coordinator decision, see
  [as built](#phase-1-as-built)).
- From Phase 3 the rendered view's gated send exists. Every Hermes send goes
  through `Runtime.Submit`, and so through `CheckSend`; it is the one
  deliberate exception to this guard, added to the guard's ledger on purpose.
  The Terminal composer's refusal stays: one gated path, not two.
- A Hermes CLI started by hand in a shell pane is caught the same way, because
  the walk is over the pane's process tree, not its first command.

### Server changes

The server half needs changes too. Today:

- `Hub.AgentState` answers `ErrNotClaude` for any window with `Claude=false`
  (`internal/mirror/agentstate.go`);
- the hub's waiting set keys on `w.Claude` (`internal/mirror/hub.go`,
  `waitingWindows`);
- Send now refuses only `waiting`, and pastes when the state is unknown
  (`internal/api/send_queue_now.go`).

Phase 1 changes:

- `AgentState` and the waiting set become runtime-aware, keyed on
  `Window.Runtime`;
- for a Hermes window, Send now refuses `busy` and unknown as well as
  `waiting`. Claude Code keeps today's rule.
- 🔴 **One enable switch, checked on both sides.** The host advertises a
  capability, `hermesSend`, in its hello, **only** when it runs with the
  `-hermes-send` flag (`MUSTER_MIRROR_HERMES_SEND`), which defaults to off.
  The server checks it at the **single choke point every mirrored paste passes,
  `Hub.PasteWindow`** (`internal/mirror/transcript.go`; `Hub.Paste` delegates
  to it), next to the existing `-interactive` check. So Send, Send now, Queue,
  a routed message and any future caller are refused for a Hermes window unless
  that host advertised it; the host's Hermes `Runtime.Submit`
  refuses unless its own flag is on, whatever the server sent. Before Phase 3
  the flag does not exist in a release, so an idle Hermes window is refused on
  both sides; Phase 1's Send now change does **not** open it. Phase 3 adds the
  flag; the operator turns it on per host.
- **A migration widening every session-key check with the 64-character
  bound**, all of which admit `^[0-9A-Za-z][0-9A-Za-z_-]{0,63}$` today:
  - `agent_focus.subject_key` (0020) and `agent_focus_history.subject_key`
    (`0022_agent_focus_history.sql:48`): `internal/focus/pgstore.go` writes both
    in one transaction, so widening one alone fails every write;
  - `session_routes.target_key` (`0021_session_routes.sql:68`): a routed
    message is a Hermes send path;
  - `attention_asks.mirror_session` (`0025_attention_asks.sql:55`).

  After it a runtime-qualified `hermes:<id>` key is accepted.
  `internal/focus` and the wire pattern change with it.

Phase 1 is this refactor plus the host paste guard, with the Claude path
byte-identical, gated by the existing suite.

### Phase 1 as built

Phase 1b (`internal/mirrorhost/runtime.go`, `internal/mirror`, `internal/api`'s
Send now, `internal/focus`, migration `0036`) differs from the sections above
in these ways, each decided while building it:

- **`mirrorhost.Runtime` has `Name` and `LocatePanes` only** — what both
  runtimes use today. `Submit` and `CheckSend` join in Phase 3, `Stream` in
  Phase 2 and `Answer` in Phase 4, with the Hermes implementations that give
  them a second user; until then Claude Code's paste, stream and answer are
  its own code, byte-identical. `AgentPane` is `ClaudePane` with a `Runtime`
  field, `""` meaning Claude Code, so every existing `ClaudePane` keeps its
  meaning. The window listing reads the proc table **once** for every runtime
  (measured on a host of ~3,100 processes: a read is ~39–45 ms, and `Windows()`
  took ~81 ms with a second read for Hermes, ~45 ms with one), and the Hermes
  locator walks nothing when no process is named `hermes`.
- **No host `-hermes-send` flag.** The doc above has the flag arrive with the
  gated send, so it does not exist before Phase 3. The server half does
  exist: the `Msg.HermesSend` wire field and the gate in `Hub.PasteWindow`.
  With no host advertising it, the gate refuses every window a host reports as
  Hermes.
- **`Window.Runtime` is `"hermes"` or `""`, never `"claude"`.** Claude Code is
  still `Window.Claude`, so a Claude window's bytes on the wire are unchanged
  (measured: the worst-case list is 1,030,215 bytes before and after).
  `Window.AgentRuntime()` is the one reader. A window claiming both is
  Hermes, the more restrictive reading.
- **`Window.AgentSession` (1–128) carries a non-Claude id only**; a Claude id
  stays in `ClaudeSession`. An all-Hermes worst-case list measures 1,016,110
  bytes (96.9% of a frame), below the Claude one, because Hermes windows add
  nothing to the per-session `claude` list. In this release the host sends no
  Hermes id (the breadcrumb locator is Phase 2).
- **Stored session keys:** a Claude id as before (1–64), or `hermes:` and a
  Hermes id (1–128); a bare 70-character id is still refused, so a Hermes key
  can never be read as a Claude one. `mirror.SessionKeyPattern` is the one
  spelling; `internal/focus` validates with it and 0036 writes it into the
  four CHECKs. `attention_asks.answered_via_session` (0034) and
  `connector_grants.decided_via_session` (0035), both newer than this page,
  are left at 64: each names an MCP caller, which Hermes cannot be yet (see
  [listing](#listing-host-icons-and-the-rest-of-the-agents-list)).
- **The strict mirroring identity rule landed here, not in Phase 2.** It had
  been moved to Phase 2 by operator decision; it was pulled forward from
  Phase 2 into 1b by coordinator decision, to avoid churn, together with the
  Hermes `LocatePanes` and the `-list -windows` line. Phase 2 builds on it.
  (`hermesCLIShape`, `HermesIdentity.CLI`, beside the broad match.) The
  pane's terminal is read from its root process's fd 0, not `#{pane_tty}`,
  which the one window listing does not read; Phase 2's breadcrumb locator
  reads `#{pane_tty}` for the breadcrumb's file name. A root whose stdin is
  not the pane's terminal (`exec </dev/null`) can make a child with the same
  stdin a Hermes window, which only takes things away from it. A test pins
  that the strict rule never claims a process the broad guard admits.
- **The server switch's refusal** is a `PasteRefusedError` carrying the host
  guard's own words and kind (`PasteRefusedHermesText`, `hermes`), with
  `ErrHermesSendOff` as its cause, so a person sees one refusal whichever side
  made it. A host's `hermesSend` is believed only from an `-interactive` v2
  host.
- 🔴 **The server gate covers only windows a host REPORTS as Hermes** — a
  strict-rule match on a host of this release. A Hermes the strict rule does
  not recognise (`hermes -p coder chat`), and every Hermes on an older host,
  is not reported, so it is refused by the 1a host paste guard alone; a host
  older than 1a has no guard at all. Upgrading the hosts is the only cure.
- The hub's waiting set fires its hook for a waiting Hermes window, but the
  attention view it re-reads (`internal/api`'s `attention.go`) still skips
  every non-Claude window until Phase 4.
- **Send now** refuses a Hermes window unless it is idle (busy, waiting,
  unknown and any unnamed status each say why). Claude Code keeps today's
  rule.
- **Deferred:** `Msg.Runtime` on watch, paste and answer (nothing sends a
  Hermes message yet; Phase 3), the kind chip and listing (Phase 2), and
  focus refresh for Hermes windows (it needs the transcript, Phase 2).

### Phase 2 as built

Phase 2 (`internal/hermeshost`, `internal/mirrorhost`'s Hermes runtime,
`muster-mirror hermes-hook` and `hermes-allowlist`) differs from the sections
below in these ways, each decided while building it:

- **One package holds every Hermes path.** `internal/hermeshost/home.go` is
  the only code that names a path under the Hermes home
  (`TestOnlyHomeGoReadsTheHermesHome`), and `TestTheHermesHomeLedger` runs the
  whole read path under inotify against a home planted with every
  secret-bearing file, failing on any open outside `ReadPaths`.
  🔴 **`ReadPaths` adds `shell-hooks-allowlist.json`** to the list under
  [secret hygiene](#secret-hygiene): `-list -hermes` reads it to name a
  consent mismatch. It holds `(event, command)` pairs and timestamps, no
  secret.
- 🔴 **The allowlist writer is a muster-mirror verb, `hermes-allowlist`.** The
  operator decision has muster-mirror ship only `hermes-hook`, with the
  allowlist written by a Home Manager activation script. That script still
  does the writing — but it runs `muster-mirror hermes-allowlist -command
  "<muster-mirror> hermes-hook"` rather than re-implementing the lock, the
  merge and the rename in shell. It is the one Hermes file muster ever writes,
  and the daemon never does. It refuses to replace an allowlist that is not
  JSON, keeps every other APPROVAL and nothing else (only the approvals
  schema is carried over: another key or field is dropped, so foreign content
  is never copied), and removes entries for an older `muster-mirror
  hermes-hook` path — on Nix every upgrade changes the binary's store path,
  and each switch would otherwise leave a growing list of consents for
  binaries that no longer exist. It writes through no symlink except a Home
  Manager link into the Nix store, which it replaces with a real file.
  Measured against the pin: Hermes's own
  `allowlisted_pairs()` reads the five pairs it writes, and a later
  `_record_approval` under the same lock keeps them.
- **The status file's writer is the gateway.** The hook process exits at
  once, so `writer_pid`/`writer_start` record its parent, the gateway that
  runs every hook (S2); a busy status whose gateway is gone is unknown.
- **The driver is `modernc.org/sqlite` v1.59.0**, the newest release that
  builds with Go 1.25 (v1.60 needs 1.26). The stripped `muster-mirror` grows
  from 8,724,745 to 12,722,441 bytes (+46%) with it, and to 13,054,217
  (+50%) with the YAML decoder `-list -hermes` reads `config.yaml`'s hooks
  with.
- **The lineage walk is Hermes's own chain step**
  (`P:hermes_state_compression.py`, `_CHAIN_STEP_SQL`): a child continues a
  parent only when the parent ended by compression AND the child carries no
  `_branched_from`, `_delegate_from` or `_reset_from` marker and is not a
  `tool` session. ⚠ Hermes also recognises a *legacy* reset child by its end
  reason; this walk does not.
- **The breadcrumb's `ts` is compared with the process's start** as boot time
  (`btime`, 1 s resolution) plus its start ticks at 100 per second: up to a
  second early, so a breadcrumb the previous CLI on the same terminal wrote in
  that second could pass.
- **`#{pane_tty}` joins the one window listing**, so the strict rule matches
  the CLI's stdin against the pane's terminal as tmux reports it, and the
  breadcrumb is named by it. A pane looked up without it falls back to the
  root process's stdin, as in 1b.
- **An `on_human_input_request` of an unknown kind writes nothing**, so the
  status stays what it was; a `request_id` that is not 1–64 of
  `[0-9A-Za-z_-]` is dropped from the file.
- **The status files are never pruned** (round-0 decision D1). The gateway
  also runs API, cron and bot sessions, each leaving a ~200-byte file.
- **The later hook ARRIVAL wins.** Hermes runs a session's hooks
  synchronously, so arrival order is event order: a status written when the
  file already holds a `since` at or after the new one is written at that
  `since` + 1 ms (two events in one millisecond, or a clock stepped back),
  never dropped.
- **The hook decodes its payload streaming, with no size cap**:
  `pre_llm_call` carries the whole conversation, and the earlier 1 MiB cap
  made a long conversation never show busy. Only `hook_event_name`,
  `session_id`, `extra.kind` and `extra.request_id` are kept; every other
  value is skipped token by token. A payload it cannot read is dropped and
  said on stderr. With no `HOME` (and no recorded directory) it writes
  nothing, loudly, never a literal `~` directory.
- **No read follows a symlink or waits on a FIFO.** Breadcrumbs, status
  files and the allowlist are opened `O_NOFOLLOW|O_NONBLOCK|O_NOCTTY` and
  must be regular files; a symlinked `state.db` is refused (it would point the
  reader at another database); the listing's deadline bounds the reads; no
  error quotes a file's content (a JSON decoder's error does: "invalid
  character 'S'"). The ledger test plants a breadcrumb symlinked to
  `auth.json` and a FIFO.
- **A failed schema check is retried** once a minute, so a `state.db` that
  was not yet the expected schema recovers without a restart.
- **`state.db` replaced under the connection is reopened** (by inode): Hermes
  keeps backups, and a restore replaces the file.
- **The rendered view is read-only and has no `Runtime.Stream` yet.** The host
  routes a watch to the Hermes tail when the window it names runs Hermes NOW
  (`mirrorhost`'s `hermesTail`), and the hub asks for one only for a Hermes
  window whose host named its session (`Window.AgentSession`), so a Phase 1
  host is never asked. The page's composer is drawn disabled with its reason
  and no queue; every send is still refused at the hub (`hermesSend`) and on
  the host (the paste guard). `Stream` joins the seam when Phase 3 gives the
  rendered view a send to share it with.
- 🔴 **A Hermes view never resumes.** A reconnect always gets a Reset backfill:
  an in-place rewrite moves `transcript_revision` but no id, so a cursor the
  page kept cannot prove the rows before it unchanged.
- **The scrollback collapses compaction copies by `message_uid` ONLY**, the
  live copy first, then the newest, in first-seen order: Hermes's own
  `_dedupe_display_generations` without its removed-rows refinement (a rewound
  message's older copy can still show until the next reset) and without its
  display-identity key or `absorbed_message_uids` (a copy with a different
  uid shows twice). ⚠ **Ordered by id, not `display_order`**: this matches
  upstream's multi-segment projection (`display_order` is per segment and not
  comparable across a lineage), and departs from its single-segment paging,
  which groups by `display_order`.
- **Rows nobody typed are never drawn**: a compaction summary
  (`_compressed_summary=1`, a user-role row) and a model-only row
  (`display_metadata.model_only`, a micro-compaction merge; decoded as Hermes
  does, an object or a double-encoded string), as upstream never renders
  them (`P:hermes_state_messages.py:1090`, `P:gateway/session_controls.py:106`).
- 🔴 **A display kind stamped after a row was drawn is redrawn.** Hermes can
  write an `auto_continue` or `process_complete` row and mark it afterwards;
  no fingerprint column moves. The host re-reads the display keys
  (`display_kind`, `display_metadata`, the summary flag) of the newest 64
  drawn rows on every poll and starts the view over when one changed, so a
  turn the model wrote never stays the person's bubble. (Holding user rows
  back until the turn ends would delay every prompt, and a stamp can come
  later than that.)
- **One rendered-view loop** (`runView`) serves both streams; which one
  serves a watch is decided in the watch's own goroutine, never on the
  connection's read loop. The prompt index
  covers every prompt of the conversation, from a light pass over the first
  1,024 bytes of each row.
- **A tail read that the fingerprint does not explain resets the view**, and so
  does a lineage that changed (a rotation): reading a child's rows as appends
  would draw the re-inserted context twice.
- **`-list -hermes`** reports the hook's consent (the five pairs in the
  allowlist, per command `config.yaml` declares, and entries for another
  binary) and per window the breadcrumb,
  lineage, newest status file and the no-view and unknown reasons. It does not
  report the gateway's `code_sha`: that needs the viewer (Phase 4). The kind is
  `Hermes` for every Hermes window, with a session or without.
- **Measured once against the pin** (a scratch `HERMES_HOME` and tmux server,
  the stub model, 2026-10-20): with the hooks and the allowlist written by
  `hermes-allowlist`, a new CLI started with no `[y/N]` prompt; the window
  read idle, busy through an 8 s turn, `waiting` (kind `approval`, no command
  in the file) on the approval modal, idle after a typed deny; the real rows
  rendered as prompts, replies, a `terminal` call and its failed result; and
  after `/branch` the window had no session and an unknown status while the
  abandoned session's file still said idle.
- **Focus refresh for Hermes windows is built** (round-0 decision): keyed by
  the runtime-qualified session id (`mirror:hermes:<id>`), refreshed from the
  window's rendered-view backfill like a Claude Code window's, shown on its
  card and in `fleet_list`/`focus_search`. Routing, the send queue and the
  sessions overview stay Claude-only: each of those is a way to send.
- **The gateway's `code_sha` is not checked by `-list -hermes`**: deferred to
  Phase 4, which attaches the viewer that reports it (round-0 decision Q1).
- **`-list -hermes` reads the hooks Hermes ACTUALLY declares** (round-1 audit):
  `config.yaml`'s `hooks:` section only, decoded into a struct with no other
  field, by the diagnostic alone (`DiagnosticReadPaths`; the daemon never
  opens `config.yaml`). Consent is reported per declared muster-mirror
  command — "ok for <command>" — and a fix (`hermes-allowlist -hermes-home …
  -command <declared>`) is printed only for the one declared command: since
  the writer removes other muster-mirror entries, a fix naming any other
  command would remove the declared one's consent.
- **Notices** are a new `cctranscript.NoticeHermes` source, drawn as the
  harness's compact rows; a tool result is a failure only when its JSON says
  so (`error`, or a non-zero `exit_code`), since Hermes records no `is_error`.

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
   transiently). The window gets **no rendered view and an unknown status**,
   with the reason "the terminal switched sessions (`/resume` or `/branch`)".
   Never the old session above a composer that sends to the pane, and never the
   old tip's `idle` as this pane's status: after `/resume` the abandoned tip
   reads idle while the pane is running another session. Unknown holds the
   queue and refuses sends.
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
| `_compressed_summary=1` (a compaction summary), `display_metadata.model_only` (a merged turn) | nothing |
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
- **The hook's own contract** (`P:agent/shell_hooks.py`):
  - 🔴 it prints **nothing** to stdout. Hermes parses a hook's stdout as JSON
    (`{"decision": "block"}`, `{"context": …}`), so output could block an
    action or inject text into the prompt;
  - it always exits 0, never the "block" exit code (`shell_hooks.py:41`);
  - it never blocks: hooks run synchronously with a 60 s default timeout
    (`DEFAULT_TIMEOUT_SECONDS`), so the lock wait is bounded (proposed 1 s) and
    the declared `timeout` is short (proposed 5 s). On any failure it writes
    nothing, which reads as unknown;
  - it validates `session_id` against the session-id pattern before using it
    as a file name, and ignores anything else. The gateway also fires hooks for
    API, cron and bot sessions.
- **Installation.** muster-mirror ships only the `hermes-hook` command and
  never edits `~/.hermes/config.yaml`. The operator declares the rest in the
  host's Home Manager configuration. Upstream's `hermes-agent` Home Manager
  module (`P:nix/homeManagerModules.nix`, `P:nix/moduleCommon.nix`) has **no**
  hook or allowlist option, so:
  - the hook entries go through the module's generic `settings` (merged into
    `config.yaml`, as `settings.hooks`);
  - `shell-hooks-allowlist.json` needs its own writer: a Home Manager
    activation script, ordered after the module's own activation, that writes a
    **real file**, never a read-only store symlink. Hermes writes the same file
    itself (`save_allowlist`, when a person answers the `[y/N]` prompt), so a
    symlink would break that path. The script makes muster's
    `(event, command)` pairs present exactly and keeps any other entries.
  - 🔴 **It writes the way Hermes does** (`P:agent/shell_hooks.py:715-740`):
    it takes Hermes's lock on `shell-hooks-allowlist.json.lock` (`flock`), reads
    the current file under it, and writes a temp file then renames it into
    place, mode 0600. Without the lock a concurrent `[y/N]` answer is lost;
    without the rename a reader can see a half-written file.
  - **On a mismatch** (a pair missing or with a different command): the gateway
    skips that hook with a warning and every new foreground CLI stops on the
    `[y/N]` prompt. muster-mirror's `-list -hermes` reads the hooks
    `config.yaml` declares (its `hooks:` section only), compares each declared
    muster-mirror command with the allowlist and names the mismatch; the
    affected sessions read as unknown; the next Home Manager switch rewrites
    the file.
- **Consent** (S2). Pre-seeded allowlist entries hold in an auto-started
  gateway, with no prompt. 🔴 The declaration must allowlist **exactly** every
  `(event, command)` pair it configures: a configured hook that is not
  allowlisted is skipped by the gateway with a warning, and makes every new
  foreground CLI stop before its UI on `Allow this hook to run? [y/N]:`. A hook
  that is not executable, or fails, writes nothing, and that reads as
  **unknown**, never idle. `-list -hermes` surfaces the "not allowlisted"
  state.

**How it feeds muster.** The locator sets `AgentPane.Status` and
`StatusSince` from the file, as it does from Claude Code's sessions file. With
Phase 1's [server changes](#server-changes), the "status time newer than the
paste" rule and the 30-second idle wait apply as written. The hook stamps a
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
  that window. Measured once, on an idle host; Phase 3 re-measures it under
  load before the gated send ships.
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

**Large pastes collapse.** A paste of at least 5 lines
(`paste_collapse_threshold`) or 2000 characters
(`paste_collapse_char_threshold`) is saved to a file under `~/.hermes/pastes/`
and replaced in the input box by `[Pasted text #N: L lines → <file>]`
(`P:hermes_cli/cli_tui_mixin.py:1677-1693,1698-1730`). The submitted message
carries the full text, which is why S11's 3,229-character paste arrived whole as
one user row. (`:1472` inlines it for the input history only.) Two consequences:

- the screen cannot show muster's text after such a paste, so the post-paste
  check accepts either the exact text (a short paste) or exactly one
  placeholder whose line count `L` equals the message's, and the submit is
  confirmed from the database, not the screen (step 6);
- ⚠ the message is written to disk by Hermes, in `~/.hermes/pastes/`. muster
  never reads that directory; the host docs say the copy exists.

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
   - the [screen check](#the-screen-check)'s image-region and input-line rules
     pass; then the bounded Escape sequence (each Escape followed by a wait and
     a re-capture), and both rules pass again.
3. Paste the text, bracketed, with no Enter.
4. Wait `enterDelay`, then re-check the status file and the screen. The input
   area must now hold exactly muster's text, or its one collapse placeholder,
   under the idle prefix, with the rest of the input-line rule still holding.
   If anything moved, do not press Enter: report "pasted, not submitted; the
   text is in Hermes's input box".
5. Press Enter as today's pasted newline.
6. **Confirm by the database:** a new `user` row in the lineage whose content
   equals the message, with **both sides stripped** of leading and trailing
   whitespace before comparing: the CLI submits `raw_text.strip()`
   (`P:hermes_cli/cli_tui_mixin.py:1452`) and `SanitizePaste` does not trim,
   while a collapsed paste is expanded back **unstripped**
   (`P:hermes_cli/cli_stream_mixin.py:209-227`). If the row arrives, report "sent". If it does
   not arrive within a bounded wait (a stalled event loop can turn the Enter
   into a newline, item 5), report **"may have been submitted; check the
   conversation before sending again"** — never "not submitted", which would
   invite a resend and a duplicate turn. The pre-Enter refusals in steps 2 and
   4 are the only "not submitted" answers, because there no Enter was sent.

Text rules for Hermes, after `mirror.SanitizePaste`:

- a message whose stripped text starts with `/` or `!` is **refused**. This is
  wider than Hermes's own predicate (`_looks_like_slash_command`,
  `P:cli.py:701-705`, does not treat a path such as `/tmp/notes.txt` as a
  command), and the over-refusal is **accepted**: one simple rule, and a
  message starting with a path can be reworded. The CLI runs commands locally
  (`_looks_like_slash_command`, `_tui_enter_inline_command`),
  writes no user row, so a submit cannot be confirmed, and some open the very
  overlays the screen cannot see (`/model`). Claude Code keeps passing them
  through;
- a message whose **stripped** text ends in `\` is **refused**: with the
  multi-line shortcuts on, Enter after a trailing backslash continues the line
  instead of submitting. Hermes tests `\\[ \t]*$` against the raw buffer, and
  `$` also matches before a final newline
  (`P:hermes_cli/cli_terminal_input.py:348,427-430`), so the stripped text is
  the safe test.

⚠ **Residual risk.** A turn can start on its own between step 4's re-check and
step 5's Enter (`auto_continue`, a background delegation finishing). With
`queue` mode the CLI also holds its own queue for about 0.3 s after a turn ends,
while the hook already says idle (S11); check (b) catches it once the user row
is written. The host docs recommend `/busy queue`, which the CLI saves to
`config.yaml` itself: any such gap then queues rather than interrupts.
muster never writes Hermes's config.

### The screen check

The screen is the only signal for states no hook reports, and the backstop for
the ones the hook does. What it can see is set by the CLI's layout
(`_build_tui_layout_children`, `P:hermes_cli/cli_tui_mixin.py:395-441`). The
app is drawn at the bottom of the pane, top to bottom: the overlays (sudo,
secret, connection, approval, slash-confirm, clarify, model picker, command
palette), the spinner, the hint line, the pet, the stash panel, the subagent
dock, **the status bar**, the upper input rule, the image bar, **the input
area**, the lower input rule, the voice bar, the completions menu. The printed
transcript is above the app.

So:

- **Between the status bar and the input area there is only the input rule**
  (and the image bar), in every state; the S11 idle, busy and approval fixtures
  all show the same single rule there. That region cannot tell states apart.
  An earlier revision of this page said a structural check of it refuses the
  overlays. That was false.
- **States that change the input line** are told apart reliably. The prompt
  prefix (`_get_tui_prompt_fragments`, `:309-339`) and the placeholder
  (`_tui_placeholder_text`, `:798-833`) change for sudo (🔐), secret capture
  (🔑), the connection form (⚙), approval and slash-confirm (⚠), clarify (`?`,
  ✎), voice (●, ◉) and a running turn.
- 🔴 **Three states do not: the command palette (Ctrl+P), the model picker
  (`/model`) and the prompt-stash panel (Ctrl+S).** Their panels are drawn
  above the status bar, in rows the transcript also fills. The prefix stays
  `❯ `, the placeholder is unchanged and the buffer is empty. Enter goes to the
  overlay first (`_tui_enter_overlay`). In the palette it **prefills** the
  highlighted command into the input box instead of submitting
  (`:1365`; `P:hermes_cli/cli_modal_mixin.py:490-509`); in the model picker it
  selects and persists a model (`:1614-1624`).

**Signals for those three, evaluated against the source:**

| candidate | verdict |
|---|---|
| tmux `#{cursor_x}`, `#{cursor_y}`, `#{cursor_flag}` | **No.** The overlays are non-focusable `FormattedTextControl` windows (`_tui_overlay_widget`, `:2117-2121`), and keys reach them by key-binding filters on the state (`:2050-2062`). Focus and the cursor stay on the input line. |
| the overlay's frame above the status bar | **Not proof.** Its chrome is localised text (`t("cli.tui.palette_title")`) in rows the transcript also fills, and the transcript has boxes of its own. At best a deny-list, and a missed match proves nothing. |
| a CLI-side plugin or hook exposing UI state | **None.** At the pin, hooks and plugins run in the gateway (S2); the CLI loads no user code that sees its UI. |
| the terminal title | **None.** The CLI sets no title. |

🔴 **No passive signal proves those three closed.** The send is therefore off
until Phase 3 has measured the mitigations below, and then on with the named
residual cases (operator decision).

- **muster never opens them itself.** A Hermes send refuses a message that
  starts with `/` or `!` (see [the send path](#the-send-path)), so `/model` and
  the palette's commands come only from a person at the terminal.
- **Escape reduces the risk; it does not remove it.** After the input line and
  the image region are verified idle (below), the host sends a **bounded
  sequence of Escapes**, each separated by a wait and a re-capture, then
  re-verifies and pastes. What one Escape does at the pin:
  - the palette closes (an eager binding, `:2060`);
  - the stash panel closes, unless the palette is open too: the palette's eager
    binding wins (`:2001` against `:2060`), so with both open the first Escape
    closes only the palette;
  - 🔴 the **model picker does not always close**. With a typed filter, Escape
    clears only the filter; at the reasoning stage it steps back to the model
    stage (`:1263-1275`). Only a further Escape closes it. If it is still open
    when muster pastes, Enter goes to the picker (`:1614-1624`): it selects a
    model and the pasted text is lost.
  - 🔴 **closing the model picker destroys a hidden draft.** Opening the picker
    snapshots the input buffer and clears it
    (`P:hermes_cli/cli_model_switch_mixin.py:620-622`,
    `P:hermes_cli/cli_modal_mixin.py:1241-1248`). The picker's Escape calls
    `_close_model_picker()`, which restores the draft
    (`cli_model_switch_mixin.py:682-684`), and then immediately runs
    `event.app.current_buffer.reset()` (`cli_tui_mixin.py:1274-1275`): the
    draft is gone, and nothing saves it to history. Scenario: a `/model` queued
    during a turn opens the picker over a draft; the screen looks idle (empty
    `❯ ` line); muster's first Escape wipes the draft; muster pastes and reports
    "sent". **There is no cheap way to detect the hidden draft first**: the
    snapshot lives only in the CLI's memory, and the picker leaves the input line
    and placeholder unchanged. It is an [accepted residual
    case](#residual-cases-accepted). Contrast the **palette**, which restores its
    draft on close (`cli_modal_mixin.py:448-451`): the line is then non-empty, and
    the abort rule below refuses;
  - approval and clarify have **no** Escape binding; they are harmless only
    because the input-line rule refuses them before any Escape is sent;
  - ⚠ **vi mode changes what Escape does.** With `display.vim_mode`, the CLI
    runs prompt_toolkit in `EditingMode.VI`
    (`P:hermes_cli/cli_tui_runtime_mixin.py:414-417`), and one Escape on an
    idle, empty line enters navigation mode, where the paste's characters can be
    read as vi commands, and the abort rule would not notice.
    🔴 **A host whose Hermes uses vi mode (`display.vim_mode`) must not enable
    `-hermes-send`, unconditionally.** muster cannot read `config.yaml` (secret
    hygiene), so this is the operator's responsibility when turning on the
    per-host switch; the host docs say so.
    - **Defence in depth only:** upstream draws a vi label (`_vim_mode_label`,
      `P:hermes_cli/cli_status_bar_mixin.py:82-97`), and the host refuses
      whenever it sees one. The label is **localised**
      (`t("cli.status_bar.vim_*")`): English `INSERT`/`NORMAL`/`REPLACE`,
      German `EINFÜGEN`/`NORMAL`/`ERSETZEN`, and so on. So the matcher, and the
      `-list -hermes` warning, use the bundled translations of every language
      Hermes ships at the pin (the 17 of `SUPPORTED_LANGUAGES`,
      `P:agent/i18n.py`), extracted from `P:locales/*.yaml` when the pin is set.
      It cannot be relied on as a gate: it is only the **last** fragment of the
      status bar (`:1169-1171`); it is absent when the bar is hidden
      (`:1129-1132`, after a resize or with the bar off); it is the first thing
      `_trim_status_bar_text` cuts in a narrow pane (`:434` onward); and a
      plugin language pack or a user overlay (`$HERMES_HOME/locales/`, which
      muster does not read) can **localise or override** it to text the matcher
      does not know.
    - `-list -hermes` warns when the label has ever been seen on a host that has
      `-hermes-send` enabled;
  - ⚠ two Escapes in a row move the draft into the Up-arrow history and clear
    attached images (`:1072-1086`). That is why
    the image-region check runs **before** any Escape, and refuses if images are
    attached, rather than letting the Escapes silently drop them.

  The sequence length (proposed: 3, enough for picker-with-filter at the
  reasoning stage) is set by Phase 3's measurements, and every Escape in it must
  first be shown, by fixture, to be harmless on an idle, empty line. Two rules
  bind it:
  - 🔴 **abort at the first bad re-capture.** After each Escape the host
    re-captures; the first capture that shows a non-idle input line, a non-empty
    one (a restored palette draft) or an image bar ends the sequence, and the
    send is refused with nothing pasted. Without this, Escapes #2 and #3 would
    form a double-Escape, which moves a restored draft out of the input box into
    the Up-arrow history and clears any attached images (`:1072-1086`).
  - **every wait exceeds prompt_toolkit's `timeoutlen`** (1 s by default; Hermes
    does not override it), proposed 1.5 s, so two consecutive Escapes are never
    read as a double-Escape. The cost is a send that takes about 4.5 s longer, and
    a wider window for the "person opens an overlay meanwhile" residual case
    below; the abort rule's final re-capture just before the paste bounds it.
- **Phase 3 measures every overlay at its non-default stages**, not just the
  defaults: the palette with a filter and over a draft, the model picker with a
  typed filter, at the reasoning stage and **opened over a draft**, the stash
  alone, the palette and stash open together, each after the Escape sequence,
  and an idle line in **vi mode** before and after one Escape, with the status
  bar's vi label shown, trimmed and hidden, in English and in at least one
  other bundled language.

#### Residual cases, accepted

Under the operator's decision, these remain after the mitigations and are
accepted as known risk:

- an overlay **deeper than the Escape sequence** (a stage Phase 3 did not
  measure, or one a pin bump adds before the ledger test catches it);
- a person **opening an overlay at the terminal between the last re-capture and
  muster's Enter** (well under a second);
- any overlay state Phase 3 measures as **not closed** by the sequence; Phase 3
  lists them by name in the host docs when it enables the send;
- 🔴 **a draft hidden by the model picker** is destroyed by the Escape that
  closes the picker (mechanism above), with no way to detect it first. The
  person loses the draft they had typed; muster's own message is then sent
  normally.

The worst outcome in the first three is that Enter acts on the overlay and
nothing is submitted; the database check then reports "may have been
submitted". Per overlay:

- **palette:** Enter prefills the highlighted command into the input box;
  muster's text is lost;
- **model picker:** Enter selects and persists a model; muster's text is lost;
- **stash panel:** Enter restores the highlighted stashed draft, and its
  images, into the input box, overwriting muster's text
  (`_tui_handle_stash_panel_restore`, `P:hermes_cli/cli_tui_mixin.py:1802-1805`,
  `:1020-1032`).

**The image region and the input-line rule.** On the captured app region,
**before any Escape**:

- the rows between the status bar and the input area are exactly the idle
  layout: the upper input rule (`input_rule_top`) and nothing else. An image
  bar there (`:436`, `:2162-2164`) means images are attached, and Enter would
  send them with muster's message (`:1460-1464`): refuse;

- every row of the input area, from the first input row down to the lower
  input rule, is checked, not only the first. The first row is the idle prefix
  `❯ `, optionally preceded by a profile name (`<profile> ❯ `), followed by
  nothing or only the placeholder. Every further row up to the lower rule is
  empty. A multi-line buffer with text on any row refuses;
- "empty" is defined by style: the placeholder is drawn in italic
  (`idle.ansi`), so any non-italic character after the prefix is text in the
  box;
- nothing is drawn below the lower rule: no voice bar, and no completions menu
  (Enter there accepts a completion).

**Localisation.** Hermes localises its user-facing text: `t()` resolves
plugin packs, then the user overlay `$HERMES_HOME/locales/<lang>.yaml`, then the
bundled `locales/<lang>.yaml`, with the language from `HERMES_LANGUAGE`, then
`display.language`, then English (`P:agent/i18n.py:1-13`). What the screen
check matches, at the pin:

- **localised, and matched by style instead:** the idle placeholder (a random
  `t()` example, `P:hermes_cli/tips.py:72-78`) and the busy placeholder: the
  rule needs only the italic placeholder style, never its words;
- **localised, and not used as a gate:** the overlay and modal titles and hints
  in the table below (for example `approval_title`, `clarify_title`,
  `sudo_title`, `palette_title`, `hint_approval`, `destructive_title` in
  `P:locales/en.yaml`). The table's marker column describes the English
  fixtures only; the refusal comes from the input line's prefix glyph and
  style. The palette title being localised is one more reason the frame cannot
  be proof (the signals table above);
- **localised, matched against every bundled translation:** the vi label
  (above);
- **not localised:** the prompt prefix glyphs (`❯ `, `⚠`, `?`, `✎`, `🔐`,
  `☤`), which come from the skin, not `t()` (`_get_tui_prompt_symbols`,
  `P:hermes_cli/cli_tui_mixin.py:272-288`), and the hook-consent prompt, a
  plain English `input()` (`P:agent/shell_hooks.py:762`). Skins that change the
  glyphs remain UNVERIFIED (below);
- **the chrome fallback** of the [paste guard](#every-paste-into-a-hermes-pane-is-gated-on-the-host)
  matches the rule rows and the prefix glyph, not the status bar's words.

Phase 3 records the blocking-state fixtures in English and in at least one
other bundled language; that the non-English screens still refuse on structure
and style alone is UNVERIFIED until then.

The states known to block, with their S11 fixtures (English):

| state | marker | input line | why it blocks |
|---|---|---|---|
| approval | `Dangerous Command`, `↑/↓ to select, Enter to confirm` | `⚠ ❯` | 🔴 Enter approves `once` |
| confirm (`/new` and similar) | `destroys conversation state`, `Type 1/2/3` | `⚠ ❯ type 1/2/3, …` | Enter approves; **no hook fires** |
| clarify, choices | `needs your input`, `Enter to lock, Tab next question` | `? ❯` | Enter locks the recommended choice |
| clarify, free text | `needs your input`, `Type your answer in the prompt below` | `✎ ❯` | the paste becomes the answer; muster never answers by pasting |
| sudo | `Sudo Password Required` | `type password (hidden), …` | 🔴 the paste becomes a password |
| hook consent | `Allow this hook to run? [y/N]:`, before the UI | none | the CLI has not started |
| busy | spinner | `☤ ❯` | Enter interrupts the turn |
| palette, model picker, stash | a panel above the status bar | **idle `❯ `** | 🔴 Enter runs a command or persists a model; see above |

A modal still drawn after the hook or viewer reported it resolved is still a
modal: a viewer-answered approval leaves it on screen until a key is pressed
(S1).

Not yet captured (UNVERIFIED until Phase 3 records a fixture for each): the
command palette, the model picker, the stash panel, secret capture, the
connection form, and the voice states.

**A ledger of overlay states.** Phase 3 keeps a list of every overlay or modal
state attribute the pinned `cli_tui_mixin.py` and `cli_modal_mixin.py` define,
each marked "changes the input line" or "does not", with its fixture. On a pin
bump, a test that extracts the attributes from the new source fails when the set
grows or shrinks, so a new overlay cannot arrive unseen. The fixtures are
re-recorded by `make test-livehermes` on every pin bump. Skins or themes
changing the glyphs are UNVERIFIED; an unrecognised screen refuses.

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
- **A pending approval is in no row** (S5). The hook says only that one is
  pending (`on_human_input_request kind=approval`, with its `request_id`); it
  records no command, by the hook's contract above. **The command and its
  description come from the viewer only** (`approval.request`, `prompts` on
  resume), which muster attaches on that `waiting`. From the DB, an unmatched `terminal` call under a
  live lease means "a tool is running **or** an approval is pending", nothing
  more.

**Mapping.**

- A Hermes **clarify** becomes muster's existing question card
  (`askq.Question`): question as header, choices as options, `multi_select` as
  checkboxes, the **Other** box for Hermes's always-present "Other (type your
  answer)". It follows the recorded answer from the `tool` row, as Claude
  Code's does.
- A Hermes **approval** is a new attention item: the command (capped,
  redacted), its description, the choices, all from the viewer. `Pending` is
  `{Summary: "terminal: <command>", Tool: "terminal", Last: <last assistant
  text>}`. When the viewer cannot attach, the item says "Hermes is waiting for
  an approval in the terminal", with no command.
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
`profiles/*/.env`, `memories/`, `skills/`, `logs/`, `telemetry/`,
`pastes/` (Hermes's on-disk copies of large pastes, muster's messages
included), or
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
| the agent's own credential | none | an `API_SERVER_KEY` generated **once** and kept on the volume, `/data/hermesd/api-server-key`, mode 0600; passed only to the child; stripped from the agent's shell by Hermes (S8). It must be **stable across restarts**: see [restart](#restart-and-resume) |
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
| seed | `ccd seed` | done by `hermesd` itself on every start, before the gateway (see [volume](#volume-layout)); no subcommand |

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
  - 🔴 **keeps the API key stable.** Runs, their idempotency keys and
    `resolve-unknown` are scoped by a hash of the profile and the **API key**
    (`P:gateway/platforms/api_server_runs.py:375-386`); `_request_owns_run`
    (`:1168-1180`) gates `GET /v1/runs/{id}` and `resolve-unknown`
    (`:1487-1492`) on it. A key minted per start, as an earlier revision
    proposed, would make every recovery call 404 and let a same-key re-post
    admit a **second** run. So the key is generated once, kept on the volume
    (0600) and reused by every start. (Deriving it from the hooks token was
    rejected: muster may rotate that token, which would orphan every run.)
  - **records intent before it posts.** For each turn it writes to its map on
    the volume, **before** `POST /v1/runs`: the session, a fresh idempotency
    key, the **exact request body bytes**, a **fingerprint of the API key** it
    posts with (a hash, never the key), and "posting". It sends the key as
    the `Idempotency-Key` header (`:738`; P keeps keys in the durable
    `runs_idempotency.db`) and records the run id from the 202. A crash between
    the POST and that record would otherwise leave a run hermesd cannot name,
    parking the session for good. On recovery an intent with no run id is
    re-posted with the **same key and the same bytes**: the request fingerprint
    hashes the body (`:660-666`), so only an identical body replays the existing
    run instead of starting a second. That replay across a gateway restart is
    UNVERIFIED; Phase 5 measures it.
  - **409 `idempotency_key_conflict`** means the stored key saw a different
    body. hermesd never retries it with a new key: the turn is reported as
    "may have run; check the conversation", and the intent is closed.
  - **Key retention** is 24 h. An intent older than that is never re-posted
    (a replay could start a second run); it is resolved by reading the
    session's messages, and otherwise reported as "may have run".
  - **An intent is never re-posted under a different API key.** If the key's
    fingerprint differs from the intent's (the key file was lost or replaced),
    the old run is out of scope for the new key, and a re-post would admit a
    second run. The intent is resolved from the session's messages, else
    reported as "may have run".
  - 🔴 **After a key change, every session posted under the old key is
    retired.** Scope is a hash of the profile and the key
    (`P:gateway/platforms/api_server_runs.py:375-386`), and `GET /v1/runs/{id}`
    and `resolve-unknown` answer only the owner (`:1168-1180`, `:1487-1492`).
    So under a new key hermesd **cannot see** whether an old run is `unknown`
    and blocking its session: both calls 404. It cannot retire only the blocked
    sessions. The session map therefore records, per session, the fingerprint
    of the key its runs were posted with, and at start hermesd **retires every
    session whose fingerprint differs from the current key**, **after** first
    resolving every old-key intent it can from the session's messages (resolve
    first, then retire, so an intent's outcome is decided against the session it
    was posted to). Retiring maps the muster
    session to a fresh Hermes `session_id`. This deliberately costs the history
    of sessions that were not blocked too; a session that might be blocked
    forever is worse. The next turn's reply on each starts with: "The previous
    Hermes session could not be recovered after the agent's API key changed;
    this turn starts a new session without its history." The key only changes
    if its file on the volume is deleted or replaced (a lost volume loses
    `state.db` too), so this is rare; it is logged and counted.
  - **recovers on every gateway start, not only its own.** The likely SIGKILL is
    the gateway alone being OOM-killed while hermesd lives on. So after every
    start of the child (hermesd's own start, and each supervised restart), and
    before it answers any turn (`/healthz` reports `recovering`), hermesd
    settles every session with an OPEN intent: it `GET`s that intent's run and
    calls `POST /v1/runs/{id}/resolve-unknown {admission_id,
    execution_generation}` when it is `unknown` (outcome `interrupted`),
    logging what it resolved. A session with no open intent is not called
    about at all: a run becomes a session's last run only once it has ended.
    A failure that may pass (transport, timeout, 5xx, 429) — and a 401 or
    403, which refuses the API key and so every session alike — is retried
    and never retires a session; if it persists, `/healthz` fails at the
    7-minute stuck bound. Only a definite 4xx refusal of an open intent's
    recovery, persisting 5 minutes with no other outcome between, retires that
    one session — 2 minutes before the stuck bound, more than the liveness
    probe's 90 s, so the pod is never restarted for a session it would retire;
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
| `/data/hermes/home` | the agent shell's HOME; **created by hermesd's start-up seed step**; git config lives here |
| `/data/hermes/cache/scratch` | the agent shell's `TMPDIR`, set by Hermes, pruned after 24 h idle: on the volume |
| `/data/workspace` | the agent's workspace and checkout |
| `/data/hermesd` | hermesd's session map |

- 🔴 **`state.db` needs a real local filesystem.** NFS and virtiofs are not
  supported (`website/docs/user-guide/docker.md:216-225`). A deployment
  requirement to document.
- 🔴 **No `.env` on the volume, ever.** `$HERMES_HOME/.env` loads with
  `override=True`, beating pod environment, and **clears** known keys it does
  not list (`P:hermes_cli/env_loader.py:510-518`). hermesd's start-up seed step
  refuses to start when one exists (naming it, deleting nothing it did not write), and the
  supervisor checks again before every in-pod restart, because the agent can
  create one with `hermes config set`. The analogue of ccd's
  `settingsForbidden` strip.
- **The agent shell's HOME** is `$HERMES_HOME/home` only if that directory
  exists **and** either container detection succeeds or
  `TERMINAL_HOME_MODE=profile` is set (`P:hermes_constants.py:743-768`). So
  hermesd's start-up seed step creates the directory and the spec sets
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
- **The agent's shell environment does not carry it** (S8): provider keys
  (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY` measured), `GH_TOKEN`, `GITHUB_TOKEN`
  and every `API_SERVER_*` variable are stripped from terminal subprocesses
  (`P:tools/environments/local_env_policy.py:38,380`). 🔴 **That is a claim
  about the shell's own environment only, NOT a boundary:** the agent runs as
  the gateway's uid and can read the gateway's environment, provider key
  included, from `/proc/<pid>/environ`. This exposure is **accepted** (operator
  decision, [above](#operator-decisions)) as parity with `claude-code` pods,
  whose agent can read its own OAuth token the same way.
- **The in-pod API key** (`/data/hermesd/api-server-key`, 0600) grants the
  loopback API server: starting runs on any session, reading them, and
  answering their approvals. The agent runs as the same uid and **can read
  it**, as it can already read the gateway's environment in `/proc`. That is
  acceptable because it grants nothing the agent does not already have: the
  agent can run any command in its own shell, so approving its own commands
  through the API is no new power, and the API is bound to loopback inside a
  pod reachable only by muster. Approvals in a pod are a policy aid, not a
  boundary against the agent (as for `claude-code`). The key is not a provider
  credential and does not leave the pod.
- **`MUSTER_HOOK_TOKEN` and `MUSTER_CONNECT_URL` pass through unchanged** (S8);
  no `env_passthrough` configuration is needed.
- **OAuth providers are not offered for pods**: some rotate single-use refresh
  tokens into `auth.json`.
- **GitHub: no token in the pod**, as for `claude-code`. git reaches GitHub
  through muster's connector proxy, which injects a GitHub App token limited to
  the agent's repository ([github-app.md](../github-app.md)). hermesd's start-up
  seed step writes ccd's `url.<proxy>.insteadOf`, `followRedirects` and
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
  instructions takes effect on the next turn. agentspec places the text
  **outside every volume**, at a fixed read-only path such as
  `/etc/hermesd/instructions.md` (as `ClaudeCodeInstructionsPath` does for
  `claude-code`), never in the workspace. It is a
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

### Phase 5 as built

`cmd/hermesd`, `images/hermes-agent` (the image flake and `smoke/`), the kind
(`agents.KindHermes`, migration `0037`, `agentspec.buildHermes`, the
`MUSTER_AGENT_HERMES_*` variables) differ from the sections above in these
ways, each decided while building it:

- **The kind ships dormant** (operator decision): off unless
  `MUSTER_AGENT_KINDS` includes `hermes` and `MUSTER_AGENT_HERMES_IMAGE` is
  set; enabled only after a real-pod run passes (NetworkPolicy, restricted
  admission, a live `Idempotency-Key` replay).
- **The pieces ccd and hermesd share moved to `internal/podagent`** — git
  routing through the connector proxy, the repository checkout (with its
  self-filed `github` request), the bearer, the `/v1/responses` wire (failure
  shape, native tool items, the response writer, the prompt reader), the
  approval-queue client (`ApprovalRelay`: post, poll, fallback on timeout or
  no relay) and the crash latch with its backoff (`RecordExit`) — unchanged in
  behaviour; ccd keeps its names as thin delegations, and its untagged and
  tmux suites pass unchanged. One rule, one implementation.
- **The turn budget STOPS the run** (`POST /v1/runs/{id}/stop`) and the turn
  answers a typed `turn_timeout`. Chosen over following the run in the
  background and storing its outcome for the next reply: a run nobody follows
  can still ask for approvals nobody sees, spend provider credit unbounded, and
  hold its session busy with no deadline; a stop ends all three, and the
  person resends knowing the turn did not finish. 🔴 **Nothing is followed in
  the background**: the watcher and its stored outcome notice are gone. A run
  recovery finds still going keeps its intent open (the session answers busy)
  until the session's next turn settles it under the same rules.
- **Recovery is bounded, and never forgets a flaky API**: a deadline on every
  recovery call (15 s); only sessions with an open intent are recovered (an
  idle session makes no call); a failure that may pass (transport, timeout,
  5xx, 429) is retried and never retires a session, so a persistent one turns
  `/healthz` non-200 at the 5-minute stuck bound and the liveness probe acts;
  only a definite 4xx refusal of a session's recovery, persisting 5 minutes,
  retires that session. The
  Hermes API client has a 30 s timeout (the run's event stream has its own
  client, bounded by the turn budget).
- **The store writes before it changes memory**: a failed write leaves no
  intent behind (no phantom `busy`). **Written is written**: once the rename
  has put the new map in place, memory takes it too, even when the
  directory's fsync then fails (logged) — memory and disk never disagree on
  whether an intent exists.
- **A definite refusal is never replayed**: when Hermes refuses a run (a 4xx)
  and the intent's close cannot be written, the key is tombstoned in memory
  (never re-posted), the close is retried until it lands, and the turn answers
  `may_have_run` — a crash before the close lands would replay it.
- **A run that completed as the budget ran out is the reply**, not
  `turn_timeout`.
- **Approvals are relayed from both paths**: the SSE `approval.request` and
  the run's `pending_controls` when the status is polled (field names from
  `P:gateway/session_pending_controls.py`); `tool.failed` ends a native tool
  row as failed.
- **Fixed, not configurable** (D6–D8): no `HERMESD_TURN_TIMEOUT`,
  `HERMESD_HERMES_BIN` or `HERMESD_STATE_DIR`; no separate seed subcommand (`serve`
  seeds the volume itself on every start); no
  flake `checks.hermesd` (its tests run in `go test ./...`).
  `HERMESD_MODEL_BASE_URL` stays as a **test-only** override (the smoke test's
  fake model), logged at boot when set; `agentspec` never sets it, and a test
  holds it to that.
- **The pod's root filesystem is read-only**, with an emptyDir at `/tmp`;
  hermes only. The field renders nothing for other kinds, and a test pins the
  gateway and `claude-code` fingerprints and rendered Deployments to their
  bytes before the field existed, so no existing pod rolls.
- **`/data` must be a local filesystem**, not NFS or virtiofs: Hermes's sessions and
  its run records are SQLite (`state.db` in WAL mode; documented in
  [provisioner.md](../provisioner.md) and [configuration.md](../configuration.md)).
- **The model is checked where it is set**: a hermes agent's model must be a
  case-sensitive `openrouter/<vendor>/<model>` slug (or a one-part OpenRouter
  id such as `openrouter/auto`), at
  dispatch and at a model change, refused before anything is created or
  stored.
- **`task_dispatch` does not dispatch `hermes`** yet: the chief's tool refuses
  the kind until Hermes's MCP client against muster's `/mcp` is verified.
- **tini is PID 1, not hermesd.** The gateway's terminal tool leaves processes
  behind when the gateway is killed; they are reparented to PID 1, which must
  reap them, and a Go process that also `exec`s git cannot safely reap all
  children. tini forwards SIGTERM to hermesd, which forwards it to the gateway.
- **`gateway run --external-supervisor`**: hermesd is the process manager that
  owns the foreground gateway, so an in-chat restart or update exits back to it
  rather than spawning a detached replacement (read from the pin's
  `gateway run --help` and `P:hermes_cli/gateway.py`).
- **The termination grace is 60 s, not 45 s.** The smoke test measured a stop
  during a run at 34 s on an idle host and 53 s under load (the 30 s drain, the
  interrupt, hermesd's own shutdown).
- **A recovery decision about a turn nobody waits for any more is said on the
  session's next reply** (a notice): an old-key intent resolved from the
  messages, an intent past the key retention, a key conflict, a retired
  session. A timed-out turn is not one of them: it was stopped and answered
  `turn_timeout`.
- **No relay, an unreachable relay or a dismissed card is a `deny` too**, not
  only the relay timeout: there is no terminal prompt to fall back to in a pod,
  and an unanswered prompt would end `run.completed` at Hermes's 300 s.
- **Busy is one rule**: a session with an open intent (a live turn, or a run
  recovery is still following) answers `409 busy`; `beginIntent` is the one
  place it is checked.
- **`config.yaml` is written as JSON** (which YAML reads), rewritten before every
  start of the gateway; `agent.disabled_toolsets` removes browser, computer use
  and TTS; `${OPENROUTER_API_KEY}` and `${MUSTER_HOOK_TOKEN}` are expanded by
  Hermes, so no credential is written to the volume.
- **The model** is muster's slug with its `openrouter/` provider prefix removed
  (Hermes runs the openrouter provider); `openrouter/auto` keeps its id.
- **SOUL.md is not written.** The per-run `instructions` path is measured (S9)
  and is the only one used; the SOUL.md fallback has no caller.
- **Not in this release**, each refused rather than half-built: a `hermes`
  chief (every chief tool is an MCP tool, and Hermes's MCP client against
  muster's `/mcp` is unverified — the smoke test's fake `/mcp` answered 404 and
  Hermes parked the server and carried on), the Terminal view, and `clarify`
  (open question 3). The memo relay still skips the kind
  (`ResultKindUnsupported`), although hermesd answers a second turn with the
  typed `409 busy` that rule waits for.
- **Measured by the smoke test (34 checks), against the real pin in a
  container** (uid 1000, read-only root, no capabilities): the exact approval
  body, approved and denied through the relay, each with an effect that can
  fail (the approved command's directory, the denied one's `blocked` status);
  the agent shell's OWN environment (no bearer, hooks token, provider or API
  key there — `/proc` exposure is accepted, see the operator decisions; `HOME`
  and `TMPDIR` on the volume); a real `git ls-remote` through muster's proxy
  (the fake muster records the `/connect/` path and the bearer); a SIGKILL mid-turn (the turn ends `interrupted`,
  recovery's `resolve-unknown` with the exact body releases the session); the
  `Idempotency-Key` replay across a restart (above); SIGTERM during a run; a
  container restart on the same volume (same API key, history kept). Not
  measured: a Kubernetes pod, a real provider, a cold image build.

## Phased plan

Efforts are rough engineer-days, including tests and docs. Phase 0 is done.

| phase | what | effort | depends on |
|---|---|---|---|
| ~~0~~ | Spikes S1–S11 | done | — |
| ~~1~~ | Runtime seam: `mirrorhost.Runtime` for Claude Code and Hermes, Claude behind it, `Window.Runtime`/`AgentSession` with the wider id bound. 🔴 **The host paste guard**, first, with the broad process identity (**done**, 1a). The strict mirroring identity rule moves to Phase 2 and the chrome check to Phase 3 (operator decision); the strict rule was later pulled forward into 1b (coordinator decision, **done**). [Server changes](#server-changes): runtime-aware `AgentState` and waiting set, Send now refusing unknown and busy for Hermes, the `hermesSend` switch, the migration widening the session-key checks of 0020, 0021, 0022 and 0025 (**done**, 1b; see [as built](#phase-1-as-built)). No behaviour change for Claude Code. | done | — |
| ~~2~~ | (**done**, see [as built](#phase-2-as-built)) Read-only Hermes mirroring: the strict mirroring identity rule (moved from Phase 1; then pulled forward into 1b by coordinator decision, and landed there — Phase 2 builds on it), breadcrumb locator with the end-reason staleness rule (no view, unknown status), SQLite reader (compaction, compression-only lineage), renderer, the `hermes-hook` command and its status files with lease and newest-row checks, the Home Manager declaration and the locking allowlist writer, mismatch diagnostics, listing and kind chip, secret-path ledger, `-list -hermes` | 6–8 | 1 |
| **3** | Gated pane send **and its live harness**. `make test-livehermes`: the pinned Hermes in a scratch tmux server and home; fixtures for every blocking state, and from them the paste guard's chrome check (moved from Phase 1), including the palette, model picker and stash; the overlay-state ledger; measuring the Escape sequence (with the abort rule and waits above `timeoutlen`) against every overlay at its non-default stages (filtered picker, reasoning stage, palette and stash together, the picker opened over a draft, vi mode); re-measuring under load the false-idle slack, the 150 ms Enter margin and the queue gap. Then the send: per-runtime server gates, host `CheckSend` with the image-region and input-line rules and the bounded Escape sequence, the `hermesSend` switch on both sides, the `/`, `!` and trailing-`\` refusals, collapse-aware post-paste check, database-confirmed submit with "may have been submitted", Queue and Send now for Hermes, host docs. Send stays off until these measurements are done and the harness is green; then the operator enables it per host, with the residual cases named in the host docs. | 10–12 | 2 |
| **4** | Questions and approvals: clarify cards from the DB, approval and sudo attention items from the hooks, viewer discovery through `gateway.sock.path`, attach-to-answer, `approval.respond` with the stale-modal card, measuring the viewer's clarify answer (and wiring it if it passes), the hygiene filters; harness cases for each | 5–7 | 2, 3 |
| ~~5~~ | `hermes` kind: `hermesd` (runs, events, exact approval body, relay timeout, a stable API key on the volume, intent-before-POST with the exact body and `Idempotency-Key`, 409 and retention handling, `resolve-unknown` recovery on every gateway start, SIGTERM handling), the Nix image without ffmpeg, `agentspec`, migration, config, smoke test like `cc-smoke` (**done**; see [as built](#phase-5-as-built)). A first run in a real pod is the release's (muster-ops), not this phase's code. | done | 1 |

Remaining: about 31–39 days. Each phase is released and shipped to the hosts as
it merges. Phases 2–4 and 5 are independent after Phase 1. The shortest useful
slice is Phases 1–2: read-only Hermes windows with status, with the paste
guard already in place. `make test-livehermes` re-runs on every pin bump.

## Risks

- 🔴 **A misplaced Enter approves, picks a choice, sends a password or
  interrupts.** Shipped muster can do it today through the Terminal composer;
  Phase 1's host paste guard closes that first. Mitigated afterwards by the
  image-region and input-line rules, the bounded Escape sequence, the status and
  DB checks, and the post-paste re-check; the residual window is named, and
  `/busy queue` narrows it. The palette, model picker and stash are invisible
  to the screen; the cases the Escape sequence leaves are
  [accepted residual risk](#residual-cases-accepted).
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
  - the command palette, model picker, prompt-stash panel, secret capture,
    connection form and voice states (found in source; fixtures in Phase 3);
  - ~~an `Idempotency-Key` replay across a gateway restart~~: **measured** in
    Phase 5's image smoke test: the same key and bytes after a SIGKILL and
    restart answer `202` with the ORIGINAL run id and `replayed: true`; another
    key with the same body admits a new run (the control); the same key with
    another body is `409 idempotency_key_conflict`;
  - the Escape sequence against the palette, model picker and stash at their
    non-default stages (Phase 3); Send stays off until it is measured;
  - a Hermes CLI run over `ssh` or `docker exec` in a pane, covered only by the
    best-effort chrome check;
  - in a pod, the agent shares a uid with the gateway and can read its
    environment and the API key file, so approvals there are a policy aid, not a
    boundary against the agent itself (as for `claude-code`);
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
  modal when another client settles it; mark the command palette, the model
  picker and the stash on the input line, as every other modal already is; a
  documented per-session status file
  for the CLI; a hook-reported event for confirm modals such as `/new`.
- **A runtime-neutral identity for the host MCP endpoint**, so Hermes sessions
  can call muster's attention and task tools.
- **A decisions.md entry** recording why muster pins Hermes by sha, refuses
  what it cannot read instead of supporting several versions, and does not send
  through the viewer.
