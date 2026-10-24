# Design: Hermes mirroring

> This page also designed a **`hermes` agent kind** (Phase 5: Hermes in a pod
> behind a supervisor, `hermesd`). It was built and later **removed**, unused;
> migration `0047` narrows `agents_kind_check` back to `gateway` and
> `claude-code`. The last tree that had the kind is **`d46b22e1`**; that tree's
> copy of this page holds the kind's design and its "as built" record.
> Mirroring (Phases 1–4) is unaffected.

**Status: Phases 0–4 done** (Phase 4 in three stacked parts, 4a–4c).
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
Phase 3 is implemented (see [Phase 3 as built](#phase-3-as-built)): the gated
send — the screen check, one Escape, the post-paste check and the
database-confirmed submit — behind the `-hermes-send` switch on both sides,
Send, Queue and Send now for Hermes windows, and its live harness,
`make test-livehermes`, whose fixtures and measurements are recorded there.
**Send ships OFF**: the operator enables it per host once the harness is
green against that host's Hermes; the host refuses vi mode itself. Phase 4
is implemented (see [Phase 4 as built](#phase-4-as-built)): 4a, the gateway
viewer itself — discovery, the pin check, attach, `approval.respond` and the
hygiene filters — and `-list -hermes`'s gateway line; 4b, approval and sudo
attention items and the approval's answer through the viewer, behind the
host's `-hermes-answer`; 4c, clarify questions read from the database, answered
in the terminal (the viewer's clarify answer was measured and is not wired). The
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
and a SQLite session store. This design adds it to muster as **mirroring**, at
full parity with Claude Code: a tmux window running `hermes chat` gets a
rendered conversation view, Send, the send queue, the host icon and listing, and
its questions and approvals in muster's asks UI. (It also added an in-cluster
`hermes` agent kind, since removed; see the note at the top.)

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
  remaining cases accepted as residual risk (operator decision). Phase 3
  found, at the pin, that all three DO mark the status bar, which the screen
  check now requires ([as built](#phase-3-as-built)).
- **Questions and approvals:** clarify cards from the database, approval and
  sudo items from the hooks. Approvals are answered through the gateway viewer's
  `approval.respond`; clarify through the viewer if Phase 4 measures it, else in
  the terminal; sudo is never answered from muster.
- **Every** paste into a pane holding a Hermes CLI, from any view or server
  version, is gated by the host on the pane's process identity. Until the gated
  send exists, it is refused. This closes a hazard in shipped muster.

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
- The Claude-specific code paths become one runtime interface, sized for the
  two runtimes it serves: Claude Code and Hermes.
  (As built later: `mirror.RuntimeSpec` on the server and
  `mirrorhost.Runtime` on the host, `internal/mirror/runtime.go` and
  `internal/mirrorhost/runtime.go`, with opencode as a third, stub runtime.)

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
| **S6** API server | confirmed, with version differences | P's approval body is exact; events are `message.delta`, `approval.settled`. An unanswered approval is denied after 300 s and the run still **completes**. 🔴 After a SIGKILL, later runs on the session report `run.failed unknown_execution` but are only **queued**; `/resolve-unknown` releases them. SIGTERM drains 30 s, then exits 1. | the removed agent kind |
| **S7** `-q` | confirmed | At P, `-q` leaves a detached gateway per home that inherits the caller's env, including `API_SERVER_*`. Not used. | the removed agent kind |
| **S8** image and env | confirmed, size caveat | A Nix image runs as uid 1000 with a read-only root and no init. With ffmpeg overridden out: 1,149 MiB closure, 425 MiB gzipped (upstream `minimal`: 3,278 MiB). Provider keys, `GH_TOKEN` and `API_SERVER_*` are stripped from the agent's shell; `MUSTER_*` pass. Terminal HOME is `$HERMES_HOME/home` only if it exists and `TERMINAL_HOME_MODE=profile` (or container detection). git through the connector proxy worked. | the removed agent kind |
| **S9** instructions | settled | `/v1/runs` `instructions` is appended to that run's system prompt and leaves identity and `AGENTS.md` intact. SOUL.md replaces only the identity paragraph. `.hermes.md` suppresses `AGENTS.md`. `agent.system_prompt` and `HERMES_EPHEMERAL_SYSTEM_PROMPT` are ignored for API runs. | the removed agent kind |
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
  drawn: after a resize, the status bar and rules are hidden
  (`P:hermes_cli/cli_tui_mixin.py:139-140,2177-2179`) — ⚠ for 0.35 s, cleared
  by a debounce (`P:hermes_cli/cli_terminal_mixin.py:395,436-452`), not
  "until the next keypress" as this page first said (Phase 3); with the status
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

### Phase 3 as built

Phase 3 (`internal/hermeshost`'s `screen.go` and `vilabels.go`,
`internal/mirrorhost`'s `hermes_send.go`, the hub's Hermes routing, the
rendered view's composer, and `make test-livehermes`) differs from the
sections above in these ways, each decided while building it:

- 🔴 **The status bar is a REQUIRED part of the idle layout, and the stash
  panel has a mark in it.** Read from the pinned source and then measured:
  `_get_status_bar_fragments` returns **nothing** while
  `_model_picker_state` or `_command_palette_state` is set
  (`P:hermes_cli/cli_status_bar_mixin.py:1128-1133`), and the stash
  indicator reads `📌 N ▲` while its panel is open
  (`P:hermes_cli/prompt_stash.py:68-71`). The screen check therefore
  requires the row above the upper input rule to be Hermes's status bar (its
  `☤ ` model marker) and refuses the `▲` mark — so the command palette and
  the model picker, at every stage measured, and the stash panel are
  **refused before any Escape**, which the design above said no passive
  signal could do. 🔴 That also closes the model picker's **hidden draft**
  case: the screen is refused before the Escape that would destroy the
  draft (measured: the injected picker over a typed draft; one Escape left
  the box empty, the draft gone — the mechanism the design names — but the
  gated send never sends it). The cost: a Hermes with its status bar turned
  off, hidden for 0.35 s after a resize, or in a pane under 64 columns
  (compact chrome, no lower rule) cannot be sent to; muster says why. This
  is a signal of the pin's code, not a contract: the overlay ledger (which
  now pins the blanking condition itself) and the harness re-check it on
  every pin bump.
- 🔴 **The stash panel is refused by its HEADER, not its status-bar mark**
  (round-1 audit F1; round 2). Once the session has a title, Hermes right-aligns the
  title badge and CLIPS the bar's left side to make room
  (`_right_align_status_title_fragments`), and the `📌 N ▲` mark is the first
  thing cut: measured clipped at 100 and 120 columns, kept at 160 — so an
  open panel was ADMITTED, and Enter would have restored the stashed draft
  over muster's message. The check finds the header row `╭─ 📌 …` above
  the bar (every bundled language starts the header so). Round 1 also
  required its `╰… ─╯` footer, and round 2 measured why that was wrong: the
  panel is an unbounded window drawn top-down, so on a short pane its
  BOTTOM is cut, footer first, and the panel was ADMITTED at 100x12 with 7
  drafts and at 100x24 and 80x24 with 20 (Hermes's cap). The harness varies
  the pane's height for it (`stash-short-*`). This also refuses the stash
  with the bar's stash field turned off, which had been a residual case. A
  reply line that starts `╭─ 📌` is refused too, which is a false refusal
  and so the safe way round.
- **ONE Escape, not three** (round-0 audit D1). Every overlay a second or
  third Escape closed (the filtered picker, the reasoning stage, palette and
  stash together) is refused before any Escape, and the stash panel now is
  too; one Escape, 1.5 s, a re-capture that must pass, else refused. It
  stays for an overlay a future pin adds before the ledger catches it.
- **The frame's upper rule is the HIGHEST same-width rule within the box's
  rows whose row above is the real status bar**, and rows are trimmed of
  every Unicode space (audit F2; round 2). A draft could type a status bar,
  a full-width rule and `❯ ` ended by an NBSP or U+3000 on its continuation
  rows and be read as an idle frame, so the closest rule is never taken. The
  highest rule alone was not right either: a reply ending in a line of `─`
  as wide as the pane landed in range above the bar, and every send was
  refused until the next turn (measured at 100 and 130 wide). "The real
  status bar" means its text AND its background (`class:status-bar`). A
  draft draws no background at the pin, and a pasted escape is drawn as `^[`,
  not obeyed. With the bar blank (palette, picker) no rule is anchored, the
  highest is taken, and the screen is refused as before. A skin whose bar
  has no background is never anchored, so the transcript-rule case is then
  refused, never admitted.
- **The bar is read by its exact start on its background, everywhere**
  (round 3). That means ` ☤ `, or the battery label (`🔋 N%`/`⚡ N%`) then
  ` │ ☤ `. This applies to the anchor and to the status-row check alike. The
  background alone is not enough: the subagent dock is drawn on the bar's own
  background and can show a goal a model wrote, such as ` x │ ☤ m`. With the
  bar hidden (`display.statusbar` off, or just after a resize), that row sits
  above the rule. The dock's rows start with two spaces or a localised
  heading, never so. The paste guard's recognition of Hermes keeps the
  looser form, where matching more is the safe way.
- 🟡 **A pane with no row above the bar is refused** (round 3, "the pane is
  too short …"). On a pane exactly as tall as the input chrome (4 rows), an
  open stash panel has no visible row. It was measured admitted at 100x4
  with 20 drafts, and at 100x14 with no key pressed its header was back. The
  harness shrinks a stash and an idle pane to 100x4
  (`stash-short-100x4-20`, `idle-100x4`). This refuses every 4-row pane,
  which costs only those.
- **`mirrorhost.Runtime` keeps `Name` and `LocatePanes`.** The gated send is
  its own code (`runHermesSend` over injected steps; `Tmux.SendHermes` fills
  them), reached only through `hostSide.hermesPaste` for a paste with
  `Msg.Runtime` `"hermes"` — the one deliberate exception to the paste
  guard, held by `TestEveryWayThisHostTypesIntoAPaneGoesThroughTheHermesGuard`.
  Claude Code's send was not moved behind an interface: nothing would call a
  Claude implementation of it. `Tmux.PastePane` was split into its two
  primitives (`pasteBracketed`, `pasteEnter`), which both sends share, so the
  Hermes send pastes exactly as the Enter margin was measured.
- **Wire:** `Msg.Runtime` and `Msg.Since` on `paste` (the window's
  `StatusSince` the hub decided on), and four refusal kinds on `pasted`:
  `hermes-not-submitted`, `hermes-superseded` (the hub makes it
  `ErrSuperseded`, so a queued message is decided on again),
  `hermes-pasted` and `hermes-unconfirmed` (the hub makes it
  `ErrPasteUnconfirmed`: never "not sent"). `Msg.Runtime` on `watch` and
  `answer` is still not sent (Phase 4).
- **The server's idle gate is in `Hub.PasteWindow`**, after the hermesSend
  switch: a rendered-view send (`PasteClaude`, which now means "the window's
  agent pane") to a Hermes window is refused unless the hub lists it idle
  with a since (`HermesNotIdleError`, its own words per status), and the
  Terminal composer's `PasteActivePane` stays a plain paste for the host's
  guard. The hub waits up to 40 s for a Hermes send, the API 50 s.
- **The text rules, plus two:** besides the leading `/` or `!` and the
  trailing `\`, a message that is only digits is refused — after a person's
  bare `/resume`, Hermes reads a number as the session to switch to
  (`_consume_pending_resume_selection`) — and a message Hermes would not collapse (fewer than 5 newlines
  and 2,000 characters — `paste_collapse_threshold` counts NEWLINES, so it
  collapses from 6 lines, not 5) whose wrapped height at the pane's width
  exceeds the input box's 8 rows is refused before anything: measured, a
  1,599-character line scrolls the box and the prompt row out of sight, so
  the post-paste check could not see it. Shorter, or 6+ lines, is sent.
- **The post-paste check compares the box with every space removed**, since
  the box wraps; a single collapse placeholder must carry the message's line
  count. A draft before the message, or anything else, is refused. A tab is
  drawn `^I` (two cells), so the message is compared — and its height
  counted — as drawn; before, every message with a tab was left pasted and
  unsubmitted (measured live).
- **The database confirm** reads the conversation's lineage past the newest
  row id taken before the paste, for a `user` row equal to the message, both
  stripped, polling every 100 ms for 8 s.
- **The gated send also refuses** a pane whose tree holds a terminal
  multiplexer client anywhere — below the Hermes CLI too, which the guard's
  walk stops at (`HermesIdentity.MultiplexerInTree`) — and a tmux without
  `paste-buffer -S` (it cannot type an Escape); and it takes the pane's drive
  lock, shared with Claude Code's picker answers.
- **Measured: the hook says idle before the CLI has drawn the turn's end.**
  A send in that moment is refused by the screen check ("not its idle
  prompt"), with nothing pasted. The queue's idle wait never meets it.
- **The paste guard's chrome check** (moved here from Phase 1a) is in
  `Tmux.PaneHermes`, after the process walk finds nothing: a screen showing
  Hermes's input frame with a `❯` row **and Hermes's `☤`** (its status bar,
  or the busy prompt) is refused. 🔴 The `☤` is required because Claude
  Code's idle screen is also a `❯` between two rules
  (`TestLooksLikeHermes` runs every `internal/askdrive` fixture through it).
  It fails open as the design says, and also while the palette or the
  picker blanks the status bar.
- 🔴 **Vi mode is refused STRUCTURALLY**, not only by the screen. The host
  reads `display.vim_mode` from the Hermes home's `config.yaml` — the
  `display:` section alone, decoded as Hermes reads it (Python `bool()` of
  the YAML 1.1 value, so a quoted `"no"` is on) — on every hello and every
  send, and with it on, or unreadable, neither advertises `hermesSend` nor
  sends; the startup log and `-list -hermes` (`vi-mode` line) say so. The
  screen's vi-label refusal stays as a second layer. This widens what the
  daemon reads (see [secret hygiene](#secret-hygiene)).
- **The queue's outcomes** (round-1 audit, #201): a queued paste into a
  Hermes window that turned busy after the queue decided is ErrSuperseded
  (decided on again), never the person-facing not-idle refusal; a delivery
  that may have landed, or sits pasted in Hermes's box, reads so on the row
  and on Send now — never "Not sent".
- 🔴 **No repeat by nonce after "may have been submitted".** The composer's
  duplicate-send guard KEEPS a nonce the host never confirmed; the same
  message sent again with it is refused, and the answer offers an explicit
  **Send again**, which mints a new nonce.
- **The rendered view** draws its composer enabled, with Queue, only when the
  host advertised `hermesSend` and is `-interactive`; otherwise disabled
  with `ui.MirrorHermesNoSendNote`. Routing stays Claude-only (a route pins a
  Claude session).

**The live harness (`make test-livehermes`, `tests/livehermes.sh`).** Build
tag `livehermes`; never in CI. It proves `hermes` on PATH is the pin (its
launcher's `HERMES_REVISION`), and runs everything in a scratch directory: its
own `HERMES_HOME`s and `HOME`, a private tmux server (`-S`, `-f /dev/null`),
a scripted OpenAI-compatible stub model on loopback (no credential; the
model's `api_key` is a literal stub string), and a `muster-mirror` built from
the tree for the hook. Every process it stops is stopped by PID after its own
`/proc/<pid>/environ` names one of the run's homes. It ends with
`HERMES HARNESS VERDICT: GREEN` or `RED` (required tests named to
`tests/verdict.py`, so a skipped or missing one is RED).

- `TestLiveHermesScreens` reaches every state below on the real CLI, measures
  the check and three Escapes on each, and with
  `MUSTER_LIVEHERMES_RECORD=1` writes the scrubbed fixtures
  (`internal/hermeshost/testdata/screens/`, expectations in `screens.json`,
  written by hand). Secret capture, the connection form, voice, an attached
  image and the picker over a hidden draft have no stub-model path at the
  pin; they are **injected**: a `sitecustomize.py` on the CLI's
  `PYTHONPATH` sets the state on the running CLI exactly as Hermes's own
  callback does (or calls the `/model` handler with a draft in the box), and
  the real renderer draws it.
- `TestLiveHermesTimings` re-measures under load (2 × NumCPU busy loops).
  **Opt-in** (`MUSTER_LIVEHERMES_TIMINGS=1 make test-livehermes`, required
  when set): run it on every pin bump; it is a measurement, not a per-run
  gate.
- `TestLiveHermesSend` runs the real `Tmux.SendHermes`: an idle line and an
  8-line (collapsed) message are sent and confirmed in `state.db` (about 5 s
  each); every blocking state is refused with nothing pasted, its state
  still on screen.
- `TestOverlayLedgerMatchesThePinnedSource`,
  `TestScreenConditionsMatchThePinnedSource` and
  `TestViLabelsMatchThePinnedLocales` hold the overlay ledger (every
  `self._*_state` of `cli_tui_mixin.py`/`cli_modal_mixin.py`; the layout's
  children in order; every condition the prompt prefix branches on; every
  condition that blanks the status bar; every attribute the chrome's
  visibility filters read) and the 17 languages' vi labels against the
  installed pin: grown, shrunk or reordered is RED. Not pinned: key
  bindings, skins and plugins — the fixtures' to catch.

**Measured against the pin `5fc490e`** (the screen check before any
Escape; then what three Escapes 1.5 s apart did; "overlay" is whether it was
still drawn after them):

| state | how | check | Escape 1 → 2 → 3 | overlay |
|---|---|---|---|---|
| idle; idle with profile `coder ❯`; idle in German | real | **admit** | admit ×3 | – |
| busy (`☤`) | real | NotIdle | NotIdle ×3 | – |
| approval (`⚠`), also in German | real | NotIdle | NotIdle ×3 | open |
| clarify with choices (`?`), also in German; free-text clarify (`✎`) | real | NotIdle | NotIdle ×3 | – |
| sudo (`🔐`) | real | NotIdle | admit ×3 (Escape 1 cancels it) | closed |
| `/new` confirm (`⚠`; no hook fires) | real | NotIdle | admit ×3 (Escape 1 dismisses it) | closed |
| hook consent `[y/N]`, before the UI | real | NoLayout | NoLayout ×3 | – |
| command palette; with a filter | real | NoStatusBar | admit ×3 | closed |
| command palette over a draft | real | NoStatusBar | Text ×3 (the draft restored: abort) | closed |
| model picker, provider stage | real | NoStatusBar | admit ×3 | closed |
| model picker, model stage with a filter; reasoning stage | real | NoStatusBar | NoStatusBar → admit → admit (Escape 1 clears the filter / steps back) | closed |
| model picker opened by `/model` during a turn | real | NoStatusBar | NotIdle ×3 (the turn) | closed |
| model picker over a hidden draft | injected | NoStatusBar | admit ×3 — **the draft destroyed by Escape 1** | closed |
| stash panel | real | Stash | admit ×3 | closed |
| palette and stash together | real | NoStatusBar | Stash → admit → admit (Escape 1 closes only the palette) | closed |
| stash panel, status bar's stash field off | real | Stash (its frame) | admit ×3 (Escape 1 closes it) | closed |
| after a turn (a title in the bar), stash panel at 100, 120 and 160 columns | real | Stash (its frame; the `▲` mark is clipped at 100 and 120) | admit ×3 | closed |
| after a turn, palette and model picker at 100 and 160 columns | real | NoStatusBar | closed | closed |
| secret capture (`🔑`) | injected | NotIdle | admit ×3 | – |
| connection form (`⚙`) | injected | NotIdle | admit ×3 | – |
| voice mode, recording (`●`), processing (`◉`) | injected | Below (the voice bar) | Below ×3 | – |
| vi mode, `INSERT`; after one Escape, `NORMAL`; German `EINFÜGEN` | real | Vi | Vi ×3 | – |
| vi mode after a turn: the session title trims the label off (160 and 100 columns) | real | **admit** (the host refuses vi mode from config.yaml instead) | admit ×3 | – |
| vi mode within 0.35 s of a narrowing resize | real | NoLayout (bar and rules hidden) or Below (tmux re-wrapping them), by when the capture lands | Vi ×3 | – |
| 60-column pane (compact chrome) | real | Below | Below ×3 | – |
| text typed in the box | real | Text | Text ×3 | – |
| an image attached | injected | Images | Images ×3 (one earlier run: admit after Escape 2) | – |
| slash completions menu | real | Below | Below ×3 | – |
| after a paste: one line, three lines, a 400-character wrapped line, 8 lines (placeholder) | real | the post-paste check **admits** | – | – |
| after a paste of a 1,599-character line | real | the post-paste check: NotIdle (prompt row scrolled away) | – | – |
| after a paste behind a typed draft | real | the post-paste check: NotPasted | – | – |

So no measured blocking state is admitted by the check except vi mode with
its label trimmed — which the host refuses from `config.yaml` before any
screen is read — and **no paste ever reached an approval, clarify, sudo,
`/new` or consent screen** in any run. (The table measures three Escapes to
show what more would do; the gated send sends one.)

**Timings under load** (48 busy loops on 24 CPUs; three runs):

- hook slack, 12 turns each: user row → `pre_llm_call` 45–963 ms; final row
  → `on_session_end` 16–839 ms. Every gap positive: the false-idle rule's
  slack 0 holds.
- the Enter margin, 5 tries each: Enter 0 ms after the paste submitted 0/5,
  20 ms 0–2/5, 50 ms 5/5, 100 ms 5/5, **150 ms (muster's) 5/5** every run.
- the queue gap (queue mode: idle written → the queued message's row), 6
  runs each: 305–1,915 ms, 364–506 ms, 433–543 ms — a residual window up to
  about 2 s under load, named below.

### Phase 4 as built

Phase 4 is stacked in pull requests. **4a** (`internal/hermeshost`'s
`gateway.go` and `viewer.go`, `-list -hermes`'s `gateway` line, and
`TestLiveHermesGateway` in `make test-livehermes`) differs from the sections
above in these ways, each decided while building it:

- **Measured against the pin** (a scratch home, the stub model, 2026-10-21):
  `identify` answers `kind: hermes-gateway`, `code_sha` the full pin,
  `state: ready`, `api_origin` `http://127.0.0.1:<port>`, `served_profiles`
  naming the home, and the capabilities `session-authority-v1`,
  `durable-admission-v1` and `event-replay-v1`; `session.resume` on a session
  waiting on an approval lists it in `prompts` (`kind: approval`, a 32-hex
  `prompt_id`, `execution_generation`, `command`, `description`, `choices`
  `once, deny, session, always`, `timeout_s: 300`); a viewer's
  `approval.respond` answers `{status: resolved, prompt_id}` and is
  authoritative — `deny` and the command did not run, `once` and it ran (the
  harness's victim directory) — while the CLI's modal stays drawn. 🔴 The
  hook's `request_id` is **not** the approval's `prompt_id` (measured: two
  different ids for one approval), so a card is keyed by the `prompt_id` the
  viewer read, never by the status file.
- 🔴 **`session.resume` carries the whole conversation**, `pending[].text`
  (what the person typed) and `info` (tools, skills); the events after it
  carry every delta and tool result, and `session.info` carries
  `system_prompt` and `credential_hint`. The pin has no `omit_messages` on
  the gateway's resume. So the hygiene filter is **structural**: a frame is
  decoded only into the fields an approval needs (`promptWire`,
  `resumeWire`, an event's `type`, `session_id` and payload); nothing else is
  kept, logged or quoted in an error, and a server request (`sudo`,
  `secret`, `clarify`) is dropped unanswered.
  `TestTheViewerDropsEverySecretBearingFrame` feeds every such frame with a
  canary; `TestTheViewerKeepsOnlyTheApprovalFields` pins the types' fields.
  The live harness types a canary into the prompt and checks it never reaches
  what the viewer hands out.
- **Discovery is stricter than Hermes's own** in one place: a
  group-writable home is refused outright (Hermes admits one whose group is
  the owner's private group). The rest is upstream's `_socket_path`
  (`gateway.sock`, else `gateway.sock.path` → `hermes-gw-<hash>/
  control.sock`, each owned by this uid and private, the pointer never a
  symlink or FIFO), then `SO_PEERCRED` on the connection. 🔴 On a platform
  without `SO_PEERCRED` (macOS) the viewer is refused, not attached on the
  path checks alone as Hermes's client does: every Hermes ask there is
  answered in the terminal.
- **Only a `ready` gateway is attached to.** Hermes's own client also attaches
  to a gateway draining for a restart (`restart_wait`) to answer a parked
  approval; muster does not, and answers in the terminal then (question for
  the operator).
- **`-list -hermes` asks `identify` only** — no ticket, no attach — and prints
  `gateway none running`, `code_sha <sha> (the pin)`, `NOT THE PIN` or
  `refused` with why.
- `ReadPaths` adds `gateway.sock` (connected to, never opened) and
  `gateway.sock.path` (read), as [secret hygiene](#secret-hygiene) listed
  them; the pointed-at `hermes-gw-<hash>/` directory is outside the home.

**4b** (`internal/mirrorhost`'s `hermes_asks.go`, `internal/mirror`'s
`hermes_answer.go` and `Pending`'s Hermes fields, `internal/api`'s
`attention_hermes.go`, `-hermes-answer`) differs from the sections above in
these ways:

- 🔴 **Answering is its own host switch, `-hermes-answer`
  (`MUSTER_MIRROR_HERMES_ANSWER`), OFF by default, with `-interactive`**, and
  the host advertises `hermesAnswer` in its hello; the hub believes it only
  from an `-interactive` v2 host. The design names no switch for answers;
  an answer of `once` lets a command run, so it ships off, like Send
  (**operator decision: kept, off by default in muster; the operator turns it
  on per host in the host configuration**). Its gates copy the Claude
  AskUserQuestion answer path (`internal/api`'s `handleMirrorAskAnswer`): the
  session's owner, same origin, an `-interactive` host that advertised the
  capability, and a fresh sign-in. **Without it no viewer is ever
  attached**: the card says "waiting for an approval in the terminal", with no
  command — the conservative reading of "attach only to answer".
- 🔴 **`once` only for a command the card shows whole.** The card's line is
  `terminal: <command>`, cleaned and capped at 120 runes, with what looks like
  a secret redacted. A command that does not survive that unchanged (several
  lines, too long, a tab or doubled space, a redaction) is offered **deny
  only**, and the card says to approve it in the terminal; the host checks it
  again at answer time.
- 🔴 **Round-1 audit (#212), built in:** `once` is never offered for a
  command the GATEWAY's own redactor already masked
  (`_redact_approval_command`, `P:gateway/run.py:577` →
  `P:agent/redact.py`): a mask can swallow shell syntax (measured: `ls -H
  X-Api-Key:a;rm -rf ~/work` arrives as `ls -H X-Api-Key:*** -rf ~/work`).
  `hermeshost.GatewayMasks` lists the forms the pinned redactor emits (`***`,
  `...`, `«redacted`, `[REDACTED`); the livehermes ledger
  `TestTheGatewayMasksCoverAPinnedSample` pins `agent/redact.py`'s and
  the wrapper's source and runs the real redactor over a corpus, and
  `TestLiveHermesMaskedApprovalIsDenyOnly` drives the case end to end. An
  approval carrying any field muster does not render (a file edit's `edit`
  diff, `P:gateway/session_local_editor.py:46`) and a command holding
  right-to-left text are deny only too; the card draws the command in a
  left-to-right `<bdi>`. ⚠ The edit path is not reachable from a mirrored
  CLI at the pin: the editor's approval requester is installed only for an
  ACP session (`editor_scope`, `policy.source == 'acp'`), never a `hermes
  chat` in a pane. The card never names why the gateway cannot be reached
  (no path, no uid): that is the host's log. `TimeoutS` is gone from the
  viewer's approval (nothing showed it).
- 🔴 **Round-2 audit (#212): "once" is an ALLOWLIST.** The pin asks for
  approval through more than one gate, and every one reaches the viewer with
  the same fields: the terminal gate (`check_dangerous_command`,
  `check_all_command_guards`), but also a file-write guard
  (`P:tools/file_tools_write_guards.py:389`, "command" = the label `<write to
  …/.ssh/config>`), a plugin's approval rule (`<tool> (plugin approval
  rule)`), computer use and execute_code. Measured live before the fix: the
  SSH-config write's card offered `once deny` (approving it writes a
  `ProxyCommand` muster never showed). Now ONE predicate (`approvable`, used
  by the listing and the answer check alike) offers "once" only when ALL hold:
  the description is one the pinned **terminal** gate emits
  (`hermeshost.TerminalDescriptions`, 111 descriptions re-extracted from
  `P:tools/approval_detection.py` by the livehermes ledger
  `TestTheTerminalGateLedgerMatchesThePin`, which also pins
  `approval_detection.py` and `approval.py` by sha256); the command EQUALS a
  pending `terminal` call's raw `command` in `state.db` (so it is what runs,
  unredacted, unlabelled); it is printable ASCII (a value of only control or
  zero-width characters the redactor drops without a mask, a homoglyph or a
  combining mark, all refused); one line within the cap; nothing muster's
  redactor would hide; none of the gateway's masks; no unrendered field.
  Everything else is deny only. `TestLiveHermesLabelApprovalIsDenyOnly`
  drives the SSH-config write live. ("arbitrary program execution via …",
  built at run time, is not in the ledger: such approvals are deny only.)
  The redactor sample (`TestTheGatewayMasksCoverAPinnedSample`) runs both
  passes an approval takes (`P:tools/approval.py:855`, then the gateway's);
  `[redacted` is no longer a listed mask (the pinned string redactor never
  emits it).
- 🔴 **Round-3 audit (#212): the allowlist alone could still be walked by a
  plugin rule.** A plugin's approval rule (`P:tools/approval.py`
  `request_tool_approval`, from `P:hermes_cli/plugins.py`'s
  `pre_tool_call`) takes its description from the PLUGIN's reason — free
  text that can equal a terminal description — and shows the label
  `<tool> (plugin approval rule)`. An injected model issuing
  `sql_query("DROP …")` and `terminal("<sql_query> (plugin approval rule)")`
  in one batch passes both checks, and "once" would run SQL the card never
  showed. So `approvable` also refuses "once" for any command shaped like a
  pinned non-terminal gate's label (`hermeshost.NonTerminalLabel`:
  `<…> (plugin approval rule)`, `<write to …>`, `computer_use: …`,
  `execute_code <<…`). The livehermes ledger
  `TestTheApprovalGatesMatchThePin` enumerates every approval site in the
  pinned distribution (19, from its `RECORD`), requires each to be one this
  was classified against, and evaluates each label's f-string in the pinned
  Python through `NonTerminalLabel` — a new gate or a changed label is RED.
  The remaining non-terminal gates (MCP elicitation, an untrusted MCP tool,
  a payment-card fill) show free text under a fixed-format description no
  terminal gate emits, so the description allowlist refuses them.
- **The wire** (no version bump): `Pending` gains `kind`
  (`approval`/`clarify`/`sudo`), `detail` (the description, 120 runes),
  `promptId` and `choices` (a space-separated string, so `Pending` stays
  comparable), on a waiting **Hermes** window only — a Claude window's Pending
  is stripped of them, so its bytes are unchanged, and a Hermes Pending without
  a kind is dropped. The worst-case window list with eight Hermes approvals at
  every bound measures 1,023,474 bytes (97.6% of a frame), below the Claude
  worst case (1,030,215, unchanged). An answer is `answer` with `runtime:
  hermes` and `hermesApproval {promptId, choice}`; refusals `hermes-gone`,
  `hermes-unreachable`, `hermes-unknown` ("may have been answered").
- **Three checks of one answer**: the hub against the window it lists (still
  waiting on that prompt, offering that choice), the host against what its
  viewer holds now (re-resolving the window's pane), and the gateway itself.
- **The attention queue lists every waiting Hermes window** (the Phase 1
  carve-out is gone): an approval with Allow once / Deny when its host answers
  and could read it, else "Answer it in the terminal"; sudo "muster never
  answers a password prompt"; clarify "Answer it in the terminal" (4c adds its
  questions). After an answer the page says the terminal will show a stale
  prompt until a key is pressed there. Its focus is keyed `hermes:<id>`.
- **The viewer is per session, per connection**: attached when a listed
  window first waits on an approval, detached as soon as none does, retried no
  sooner than 30 s after a refused attach (the card names why), re-attached
  when the gateway hangs up. A new approval while attached arrives as
  `approval.request` and replaces the shown one.
- Not built: the approval card in the rendered view's live region (the queue
  and the card badge show it); a push names the tool only ("terminal"), as
  today.
- **Measured against the pin** (`TestLiveHermesApprovalThroughTheHost`): the
  real listing (Tmux with the home's source) lists the approval through the
  host's viewer as `terminal: rm -r ./livehermes-victim-dir`, `once deny`,
  `recursive delete`; the host's answer `deny` lets the turn finish with the
  victim directory intact; the viewer is detached once the window stops
  waiting.

**4c** (`internal/hermeshost`'s `clarify.go`, the host's clarify listing):

- 🔴 **The viewer's clarify answer, MEASURED against the pin, and NOT wired.**
  A viewer attached BEFORE Hermes asks receives the `clarify` server request
  (`{id: "srq-…", method: "clarify", params: {questions: [{qid, question,
  choices, multi_select}]}}` — the recommended choice already suffixed
  "(Recommended)") and its answer `{answers: {q0: "blue"}, outcome:
  "submitted"}` is taken: the tool result records it, `request.cancel` releases
  the CLI's picker, and the CLI returns to its idle prompt cleanly. But a
  viewer attached AFTER — which is what attaching on the hook's `waiting` is —
  gets nothing: `session.resume` lists approvals only (`prompts: []`), carries
  no `open_requests` at the pin's gateway runtime, and `session.events.since`
  answers `snapshot_required` with no events. So the design's
  attach-to-answer cannot reach a clarify, and the card says to answer it in
  the terminal. Wiring it would need a viewer attached to every session while
  it is busy, before it asks — a standing attachment the design rules out
  ("attach only to answer"); question for the operator. (Measured with a
  scratch home, a private tmux server and the stub model, by a throwaway
  Python viewer using the pin's own `websockets`; not committed.)
- **The questions come from the database**, as the design says: the newest
  `clarify` call in the conversation's last 64 rows that no `tool` row answers
  (1–5 questions, at most 4 choices, as the tool bounds them), read once per
  wait (keyed by the status time; a failed read is cached too and retried no
  sooner than 30 s, so a failing read never rides every listing) with or
  without `-hermes-answer` — it is the
  read the rendered view already makes. The item shows the first question,
  its choices and how many more there are ("red · blue — and 1 more question —
  answer in the terminal"), each through the host's redaction and the wire's
  caps. Not `askq.Question` cards: there is nothing muster can answer, and the
  questions in full do not fit the window list's frame budget.
- **Measured against the pin** (`TestLiveHermesClarifyThroughTheHost`): the
  host lists "Which colour should it use?" with "red · blue" from the real
  `state.db`, and the CLI's picker stays up.

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
residual cases (operator decision). ⚠ **Superseded at the pin by Phase 3**:
the status bar renders nothing while the palette or the picker is open, and
marks an open stash panel; the screen check refuses both (see
[Phase 3 as built](#phase-3-as-built)). That is the pin's code, not a
contract — a skin, a turned-off bar or a future commit can remove it — so the
Escape sequence below stays.

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
    per-host switch; the host docs say so. ⚠ **Superseded by Phase 3**: the
    host reads `display.vim_mode` alone and refuses itself
    ([as built](#phase-3-as-built)).
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

  ⚠ Phase 3 set it to **one** ([as built](#phase-3-as-built)): every state a
  second or third Escape was for is refused before any Escape. The sequence length (proposed: 3, enough for picker-with-filter at the
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
  lists them by name in the host docs when it enables the send (measured:
  none; every overlay it reached is refused before any Escape);
- **measured by Phase 3:** the queue-mode gap, up to about 2 s under load, in
  which Hermes says idle and is about to submit its own queued message. (Vi
  mode with a trimmed label and the stash with its bar field off were on this
  list; the host now refuses vi mode from `config.yaml`, and the stash by its
  frame.)
- 🔴 **a draft hidden by the model picker** is destroyed by the Escape that
  closes the picker (mechanism above, measured in Phase 3). ⚠ At the pin it is
  **not reached**: the picker blanks the status bar, so the screen check
  refuses before any Escape. It stays on this list for a Hermes whose status
  bar is configured off — which the check also refuses — and for a pin that
  changes the bar.

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

Phase 3 recorded a fixture for every state, in English and German
(`internal/hermeshost/testdata/screens/`; the measurements are under
[Phase 3 as built](#phase-3-as-built)). Secret capture, the connection form,
voice and an attached image were drawn by the real renderer with their state
injected; they have no stub-model path at the pin.

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
| clarify | the terminal (Phase 4 measured the viewer's answer: it reaches only a viewer attached before Hermes asks; see [4c](#phase-4-as-built)) |
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
  timeout (`P:hermes_cli/config_defaults.py:1670-1713`). (Phase 4 does not
  show the timeout: an approval that timed out is simply gone from the card,
  and an answer to it is refused as no longer pending.)

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

⚠ **And `config.yaml`, two sections only** (Phase 3): `-list -hermes`
decodes its `hooks:` section, and the daemon, only when it runs with
`-hermes-send`, its `display:` section's `vim_mode` — each into a struct
with no other field, and no error quotes the file.

**Never** `auth.json`, `.env`, the rest of `config.yaml` (it can hold inline keys),
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

## Phased plan

Efforts are rough engineer-days, including tests and docs. Phase 0 is done.

| phase | what | effort | depends on |
|---|---|---|---|
| ~~0~~ | Spikes S1–S11 | done | — |
| ~~1~~ | Runtime seam: `mirrorhost.Runtime` for Claude Code and Hermes, Claude behind it, `Window.Runtime`/`AgentSession` with the wider id bound. 🔴 **The host paste guard**, first, with the broad process identity (**done**, 1a). The strict mirroring identity rule moves to Phase 2 and the chrome check to Phase 3 (operator decision); the strict rule was later pulled forward into 1b (coordinator decision, **done**). [Server changes](#server-changes): runtime-aware `AgentState` and waiting set, Send now refusing unknown and busy for Hermes, the `hermesSend` switch, the migration widening the session-key checks of 0020, 0021, 0022 and 0025 (**done**, 1b; see [as built](#phase-1-as-built)). No behaviour change for Claude Code. | done | — |
| ~~2~~ | (**done**, see [as built](#phase-2-as-built)) Read-only Hermes mirroring: the strict mirroring identity rule (moved from Phase 1; then pulled forward into 1b by coordinator decision, and landed there — Phase 2 builds on it), breadcrumb locator with the end-reason staleness rule (no view, unknown status), SQLite reader (compaction, compression-only lineage), renderer, the `hermes-hook` command and its status files with lease and newest-row checks, the Home Manager declaration and the locking allowlist writer, mismatch diagnostics, listing and kind chip, secret-path ledger, `-list -hermes` | 6–8 | 1 |
| ~~3~~ | (**done**, see [as built](#phase-3-as-built)) Gated pane send **and its live harness**. `make test-livehermes`: the pinned Hermes in a scratch tmux server and home; fixtures for every blocking state, and from them the paste guard's chrome check (moved from Phase 1), including the palette, model picker and stash; the overlay-state ledger; measuring the Escape sequence (with the abort rule and waits above `timeoutlen`) against every overlay at its non-default stages (filtered picker, reasoning stage, palette and stash together, the picker opened over a draft, vi mode); re-measuring under load the false-idle slack, the 150 ms Enter margin and the queue gap. Then the send: per-runtime server gates, host `CheckSend` with the image-region and input-line rules and the bounded Escape sequence, the `hermesSend` switch on both sides, the `/`, `!` and trailing-`\` refusals, collapse-aware post-paste check, database-confirmed submit with "may have been submitted", Queue and Send now for Hermes, host docs. Send stays off until these measurements are done and the harness is green; then the operator enables it per host, with the residual cases named in the host docs. | done | 2 |
| ~~4~~ | (**done**, see [as built](#phase-4-as-built)) Questions and approvals: clarify cards from the DB, approval and sudo attention items from the hooks, viewer discovery through `gateway.sock.path`, attach-to-answer, `approval.respond` with the stale-modal card, measuring the viewer's clarify answer (and wiring it if it passes), the hygiene filters; harness cases for each | 5–7 | 2, 3 |
| ~~5~~ | `hermes` agent kind: built, then **removed** (see the note at the top) | — | 1 |

Every phase is implemented. Each phase is released and shipped to the hosts as
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
- **Untested in Phase 0, carried as risk:**
  - a real pip/uv install (S10's row is from a simulated launcher), and the bare
    `hermes` launch form;
  - threshold-triggered auto-compression (only `/compress` was used);
  - pasting into the sudo and `/new` modals (deliberately not done; inferred
    from the highlighted choice);
  - the `session` and `always` approval choices, and a viewer declaring
    `server_requests: true`;
  - ~~answering a clarify from the viewer~~ (measured in Phase 4: only a
    viewer attached before the question can; not wired);
  - whether an attached viewer keeps an auto-started gateway alive past its
    idle exit;
  - skins or themes changing the input-line glyphs;
  - ~~the command palette, model picker, prompt-stash panel, secret capture,
    connection form and voice states~~ (fixtures recorded in Phase 3; secret,
    connection and voice injected);
  - ~~the Escape sequence against the palette, model picker and stash at their
    non-default stages~~ (measured in Phase 3);
  - a Hermes CLI run over `ssh` or `docker exec` in a pane, covered only by the
    best-effort chrome check;
  - ~~every timing under load: the false-idle slack, the Enter boundary, the
    ~0.3 s queue gap~~ (measured in Phase 3: the slack holds, 150 ms submits
    every time, the queue gap reaches about 2 s);
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
