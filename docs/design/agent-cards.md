# Design: live work counts and tmux identity colours on agent cards

**Status: (a) and (b) decided, not implemented; (c) deferred.** The operator
settled every open question. The decisions are recorded under
[Operator decisions](#operator-decisions), and the sections on (a) and (b)
describe the design as decided. Nothing on this page is implemented yet. It
covers two additions to the Agents view's cards, plus one send-queue change that
was scoped and then **deferred**:

- **(a) Live work counts.** A card shows how many subagents and background
  shells its session has running now, for example `2 subagents · 1 shell`.
- **(b) The tmux identity colour.** A mirrored card's border and title are
  tinted with the colour the operator's tmux setup gives that window, so the
  card and the terminal look like the same thing.
- **(c) The send queue and background agents. DEFERRED.** This would have let
  the send queue treat a session that is busy only because background agents
  run as idle. The operator deferred it after the round-2 audit; the analysis
  is kept under [Deferred](#deferred-c-the-send-queue-and-background-agents).

A separate change, built elsewhere, adds a small dot showing recency. This page
does not cover it, except to say where (b) must not overlap with it.

Where this page states how muster works today, the code is the authority (see
[decisions.md](../decisions.md)), and the page cites it as `path:line` at the
commit it was written against. Claims about Claude Code cite the minified bundle
of **Claude Code 2.1.289** by byte offset and by the pattern found there. Those
offsets belong to that build only; the pattern is what to search for in another
build. Claims about Hermes cite an **installed build**, not the commit
[hermes.md](hermes.md) pins, and are marked **UNVERIFIED** against the pin.
Record shapes come from real transcripts, read for shape only; no transcript
content is reproduced here.

## Summary

| | Feasible? | Recommended source | Wire | Effort |
|---|---|---|---|---|
| (a) shells, mirrored Claude Code | **Yes, exact** while the CLI is alive | the CLI's own process tree: live task processes whose stdout is a task output file | new `work` message | |
| (a) subagents, mirrored Claude Code | **Yes.** Exact when the session is idle; inferred from the transcript while busy | the sessions file's status, plus a transcript ledger of Agent calls and turn ends | same message | 5–6.5 days for (a) in all |
| (a) dispatched `claude-code` agent (ccd) | **Yes**, the same code inside the pod | the same | `POST /agent/work` | (included above) |
| (a) Hermes | **Partly**, and only after Hermes Phase 2 | `subagent_start`/`subagent_stop` hooks; the process checkpoint file | the same message | 1–1.5 days after Phase 2 |
| (b) identity colour | **Yes**, but tmux does not hold the colour today: the host's tmux config must declare it | a tmux user option, `@muster-color`, read in the listing muster-mirror already runs | new `colors` message | 2.5–3 days, plus about 0.25 days of host config |
| (c) send queue through background agents | **Deferred.** A background agent's permission prompt can meet muster's automatic Enter, a race that cannot be closed | — | — | not scheduled |

The full effort table, with totals, is under [Total effort](#total-effort).

## Operator decisions

The operator decided every open question on the first draft. They are binding
on the sections below.

1. **The tmux colour comes from a tmux user option, `@muster-color`.** The
   host's tmux config generates it from its slot table. Every window of a slot's
   session gets that slot's colour. Windows outside any slot stay neutral, with
   no invented colour. A colour field in the labels file was **not chosen**; it
   is kept only as a rejected alternative.
2. **Contrast: tint the border and the title.** The border meets 3:1 and the
   title meets 4.5:1 against the card surface, in both themes, by adjusting
   lightness per theme. **In the light theme, 12 of the 16 distinct slot colours
   visibly darken** (the largest shift is 0.33 in OKLCH lightness), so pale
   colours such as cream, light yellow and light green no longer look like
   their terminal colour there. The operator accepts that.
3. **Shell counts: exact when idle, `1+ shell` during a running turn.** While a
   turn runs, the process tree cannot yet tell a foreground command from a
   background shell, so the card shows `1+ shell` until the Phase 0 stdin spike
   settles it. **Monitor tasks count as shells.**
4. **New scope, section (c):** the send queue treats "busy only because
   background agents are running" as idle, using the transcript to tell a real
   turn from background-only busyness. This covers mirrored windows and ccd
   where either is affected.
   - **Amended after the round-1 audit:** (c) is reworked with **Send-now
     semantics**. muster cannot rule out that a turn starts between its last
     check and the Enter. The design states that worst case honestly instead of
     claiming to prevent it.
   - The worst case of a misread `background` is that the message behaves like
     muster's **Send now**: Claude Code queues it natively behind the running
     turn.
   - (c) ships disabled, behind a switch, until a live gate passes.
5. **(c) is deferred (after the round-2 audit).** A background agent can open a
   permission prompt on the main screen at any moment. muster's automatic Enter
   could then approve it with nobody watching, and the gap between the last
   screen check and the Enter can be narrowed but never closed. Queued messages
   keep waiting while background agents run, and **Send now** stays the manual
   path. See [Deferred](#deferred-c-the-send-queue-and-background-agents).

## Background: what muster has today

- **Subagent cards in the rendered view.** At each Agent (or Task) call, the
  rendered view draws a card with the subagent's description, its state
  (running, done or failed), its elapsed time and its latest step
  (`internal/mirror/subagents.go:12-47`). The host decides the state from the
  parent's tool result, then from the parent's task notification, then from the
  subagent's own transcript going quiet (`internal/mirrorhost/subagents.go:98-132`).
  🔴 **The summaries exist only while a rendered view is open.** They ride that
  view's transcript stream and are deliberately kept out of the window list
  (`internal/mirror/subagents.go:30-33`). So nothing about subagents reaches a
  card in the list today.
- **The `shell` status.** Claude Code writes `shell` instead of `idle` in its
  sessions file while a background Bash task runs and no turn does. muster reads
  it as idle, because the prompt takes input (`internal/mirror/wire.go:585-607`).
  That status is a yes/no answer with no count, and muster turns it into idle
  before anything else sees it.
- **The window list has almost no room left.** It travels as one message
  bounded by `MaxFrame` (1 MiB). Its worst case measures **1,030,215 bytes, 98%
  of the frame**. Only 18,361 bytes are free
  (`go test ./internal/mirrorhost/ -run TestTheLargestWindowListFitsOneFrame -v`).
  🔴 **The comment at `internal/mirror/wire.go:446` is stale:** it says 96%
  (1,012,575 bytes), but the list now measures 98%. This page does not change
  code; the first implementation PR updates that comment to the measured figure. The repo chip met the same limit and added a message of its own,
  `repos`, sent only when the names change (`internal/mirror/repos.go:14-22`,
  `internal/mirrorhost/client.go:873`). Both features on this page follow that
  pattern.
- **One listing per refresh.** Every 5 s (`internal/mirrorhost/client.go:194`),
  muster-mirror runs one `tmux list-panes -a` with one format
  (`internal/mirrorhost/perwindow.go:128`) and reads the process table once for
  every runtime. That read takes about 39 ms on a host with about 3,000
  processes (`internal/mirrorhost/perwindow.go:149-151`).
- **Change detection.** When a host message changes what a card draws, the hub
  calls `changed`, which debounces for 1 s per owner
  (`internal/mirror/hub.go:180`, `:264`). Each open Agents view then re-fetches
  the list once.

---

## (a) Live subagents and background shells

### What Claude Code exposes

#### The sessions file: one status, no counts

`<config>/sessions/<pid>.json` has these keys, measured over every live file on
one host: `pid`, `sessionId`, `cwd`, `startedAt` (Unix ms), `procStart`,
`version`, `kind`, `entrypoint`, `tmux` (`<session>:@<window>.%<pane>`),
`name`, `status`, `statusUpdatedAt`, `updatedAt`, `peerProtocol`,
`peerFeatures`, `messagingSocketPath`, and sometimes `waitingFor` and
`bridgeSessionId`. **None of them is a count of tasks.**

The status is computed like this (2.1.289):

- `function Cye(h){… return{status:h.isLoading||h.delegatedActive?"busy":"idle", …}}`,
  at offset **229767817**. `delegatedActive` is
  `rB(state.tasks)`, which is true when any task of type `local_agent`,
  `remote_agent`, `in_process_teammate` or `local_workflow` has not reached
  `completed`, `failed` or `killed` (offset **205445827**).
- `an=ro==="idle"&&Co?"shell":ro`, at offset **229774701**. `Co` is
  `i2r(state.tasks)`, which is true when any `local_bash` task is still running
  (`function i2r(e){return Object.values(e).some(ZXn)}`, offset **205446303**).
- A waiting state (a dialog, an elicitation) is checked first, so `waiting`
  says nothing about tasks.

The status therefore gives these facts for free:

| status | subagents running | shells running |
|---|---|---|
| `idle` | **0** (exact) | **0** (exact) |
| `shell` | **0** (exact) | **≥ 1** |
| `busy` | unknown: a turn, or a running subagent, or both | unknown |
| `waiting` | unknown | unknown |

🔴 `busy` also covers a **background subagent running while no turn is.**
muster's send queue waits for `idle` (`internal/mirror/wire.go:410-420`), so a
queued message waits for every background agent to finish, even though the
prompt would take it. Section (c) was scoped to fix this and is **deferred**
(see [Deferred](#deferred-c-the-send-queue-and-background-agents)). Until
then, **Send now** is the manual path.

#### The turn-end record counts background agents

Claude Code writes a `system` record with subtype `turn_duration` when a turn
ends. Its constructor in 2.1.289 is
`function lpt(e,n,r,s,g){return{type:"system",subtype:"turn_duration",durationMs:e,…,messageCount:r,pendingBackgroundAgentCount:s,pendingWorkflowCount:g,timestamp:…}}`,
at offset **211750728**. Measured over the newest 60 transcripts on one host:

- Of the 2,738 `turn_duration` records written by 2.1.289, 1,549 carry
  `pendingBackgroundAgentCount` with values 1 to 12. The rest omit it. **No
  record carries 0**, so an absent field reads as "no background agents pending"
  (the constructor passes `undefined`, which JSON drops). This is an inference
  from those 1,189 records with no value, to be confirmed by the live-Claude
  test.
- An older build, 2.1.232, wrote the field on 1 of 18 records. **A missing field
  on an older build is not proof of zero**, so (a) never treats it as one.
- Turn starts: a user record that is not a tool result and not `isMeta`, with
  `origin.kind` of `human` (826), `task-notification` (2,016) or none (116). A
  background agent's completion therefore starts a turn of its own. 178 starts
  came while a turn was already open; these are messages that join the running
  turn, which ends once.
- 🔴 **Interrupted turns usually write no `turn_duration`.** Of 22 interrupts, 19
  were followed by a new turn start with no end record between them.

The record gives (a) an exact anchor: the number of background agents pending
when the last turn ended. It was also the signal of the deferred (c).

#### Task ids name their kind

Claude Code generates a task id as a one-letter prefix and 8 base-36
characters. `local_bash` uses `b` and `local_agent` uses `a`
(`var r={local_bash:"b",local_agent:"a",…}`, offset **205446303** and the bytes
after it). Subagent ids as they appear in transcripts are `a` and 16 hex
characters (measured: every agent task id seen had 17 characters, every bash
task id had 9).

#### The process tree gives an exact shell count

Every Bash tool call, background or foreground, runs as a child of the CLI
process, with stdout and stderr opened on its task output file,
`<tmp>/claude-<uid>/<project-slug>/<session-id>/tasks/<task-id>.output`. This
was measured on one host by launching a background `sleep` and reading its
`/proc/<pid>/fd`. The output directory also holds a symlink per subagent,
`a<16 hex>.output -> …/subagents/agent-<id>.jsonl`, which is not a process and
is not counted.

A Bash that a **subagent** started in the background is still a descendant of
the same CLI process, under the same task directory. So the process tree counts
it under the window, as it should.

Three limits, each measured on one sample only:

- 🔴 **A foreground Bash call looks the same** (stdout on
  `tasks/b<8>.output`). The only difference seen was stdin: a socket for the
  foreground call, `/dev/null` for the background one. One sample is not a
  discriminator, and a command backgrounded with Ctrl+B keeps its socket. Phase 0
  measures it. **Until then, whenever a foreground call could be running, the
  card shows `1+ shell` rather than a number** (operator decision 3). That
  includes a running turn, and also a `busy` caused only by background agents:
  a running **subagent's** foreground Bash call looks exactly like a background
  shell (fd 1 on `tasks/b<8>.output`, stdin a socket).
- 🔴 **A Monitor task has no task output file.** Measured on a live Monitor:
  - Its shell wrapper is a direct child of the CLI. Its argv names the CLI's
    shell snapshot (`<config>/shell-snapshots/snapshot-…`), as a Bash call's
    wrapper does.
  - Its fd 0 is `/dev/null`, and fds 1 and 2 are **sockets** to the CLI. There
    is no `tasks/<id>.output`.
  - The transcript records its start as a tool result whose `toolUseResult` is
    `{taskId, timeoutMs, persistent}`. Stopping it returns
    `task_type: "local_bash"`.

  A count that looks only for task output files misses every Monitor.
- **A process that outlives its task** (a daemon started with `nohup … &`) is
  reparented away from the CLI. It is no longer a descendant, so it is not
  counted. That is correct: Claude Code has marked the task done.
- **A process that outlives its task** (a daemon started with `nohup … &`) is
  reparented away from the CLI. It is no longer a descendant, so it is not
  counted. That is correct: Claude Code has marked the task done.

#### The transcript: start and end of every task, but not all in one file

The record shapes, from the parent and subagent transcripts of one host:

- **Agent call:** an assistant `tool_use` named `Agent` (`Task` in older
  builds), whose input may carry `run_in_background`.
- **Agent result:** a user `tool_result` whose `toolUseResult` has `agentId`,
  `status` and `isAsync`. `isAsync: true` means only that a background agent was
  **launched**.
- **Task end:** a `<task-notification>` with `<task-id>`, `<status>`
  (`completed`, `failed`, `killed` or `stopped` were seen), `<summary>`,
  `<output-file>` and `<tool-use-id>`. Most of these arrive as the `content` of a
  `queue-operation` record (`operation: enqueue`). Others arrive inline in a user
  message. 🔴 **The enqueue is written when the task finishes, not when it is
  delivered.** Measured: a 240 s background `sleep` started by a subagent had
  its enqueue record written 240 s after the Bash result.
- **Background Bash start:** the Bash `tool_result`'s `toolUseResult` carries
  `backgroundTaskId`.
- **Kills:** a `TaskStop`/`KillShell` result (`toolUseResult` with `task_id`,
  `task_type`), and a `system` record with subtype `agents_killed`.
- **Task snapshots:** `attachment` records of type `task_status` (`taskId`,
  `taskType`, `status`, `description`). They are rare (38 in 40 transcripts) and
  are not a stream.

🔴 **A subagent's background shell starts in the subagent's transcript, but its
completion is enqueued in the parent's.** Measured with the same `sleep`: the
`backgroundTaskId` result was in `subagents/agent-<id>.jsonl`, and the enqueue
was in the parent `<session>.jsonl`. A shell count built from the transcript
would have to read every subagent file. This is the second reason (a) takes
shells from the process tree.

#### Hooks: subagents yes, shells no

The hook events of 2.1.289 (offset **200190615**) include `SubagentStart` and
`SubagentStop` (with `agent_id`, `agent_type`; offset **209770018**), and
`TaskCreated`/`TaskCompleted`. The task hooks are for todo and team tasks
(`task_subject`, `teammate_name`; offset **209773444**), not background tasks.
**No hook fires when a background shell ends.**

### What is knowable, per window

| | mirrored Claude Code | dispatched `claude-code` (ccd) | Hermes |
|---|---|---|---|
| shells, count | **exact** while no foreground call can run (`idle`/`shell`, no Monitor); otherwise `1+` (process tree) | the same, same code in the pod | **UNVERIFIED**: `~/.hermes/processes.json` lists running background processes with `pid`, `host_start_time` and `session_key` (installed build: `tools/process_registry.py:44`, fields at `:653-658`) |
| shells, yes/no | **exact** (`shell` status, or the count) | exact | as above |
| subagents, count | **exact 0** when `idle`/`shell`; while `busy`/`waiting`, the transcript ledger (below) | same | **UNVERIFIED**: `subagent_start`/`subagent_stop` hooks exist and shell hooks may register them (installed build: `tools/delegate_tool.py:329`, `tools/delegate_tool_results.py:365`, allow-list `hermes_cli/plugins.py:135`). Needs the Phase 2 status hook, which is not built |
| nested subagents (a subagent's own subagents) | **not counted**: their calls are in the subagent's transcript, as today's cards already say (`internal/mirrorhost/subagents.go:36-38`) | same | unknown |
| cluster agents of other kinds | n/a | n/a | n/a: no badge |

### Recommended design

#### The work rule (host side, one function for both mirror and ccd)

A new package, `internal/cctasks`, is called by both muster-mirror and ccd so
that the two cannot disagree. Per Claude Code process it returns
`Work{Subagents, Shells int; ShellsAtLeast bool}`:

1. **No live sessions file → no Work at all.** The window has no Claude pane,
   and the card shows nothing (it never shows a stale count).
2. **Shells** = the number of distinct task ids matching `^b[0-9a-z]{8}$` among
   the CLI's **live descendants** whose fd 1 or fd 2 is a symlink ending in
   `/tasks/<id>.output`. Only the suffix is matched, never the root, so a
   `CLAUDE_CODE_TMPDIR` override still works.
   - **Monitors** (operator decision 3: they count as shells). A Monitor is a
     **direct child** of the CLI whose argv names `<config>/shell-snapshots/`,
     with fd 0 `/dev/null`, fds 1 and 2 sockets, and no task output file. Any
     Monitor makes the result `ShellsAtLeast: true` (`1+ shell`), never an exact
     number, because the signature rests on one sample. Phase 0 confirms it on
     at least two Claude Code builds and on a Monitor started by a subagent;
     only then may Monitors be counted exactly.
   - **Exact** only when no foreground Bash call can be running: status `idle`
     or `shell`, and no Monitor seen.
   - **`1+ shell`** (`Shells: 1, ShellsAtLeast: true`) when the count is 1 or
     more and a foreground call may be among them: status `busy` or `waiting`.
     This includes a `busy` caused only by background agents: running subagents
     make foreground calls of their own, and those look exactly like background
     shells. This lasts until the Phase 0 stdin spike can tell foreground from
     background.
   - If the status is `shell` and the count is 0 (the files are unreadable, or
     the layout changed), report `Shells: 1, ShellsAtLeast: true`. The card then
     says `1+ shell`, never `0`.
3. **Subagents:** 0 when the status is `idle` or `shell`. When it is `busy` or
   `waiting`, use the count of **running** entries in the transcript ledger.
4. **The transcript ledger** follows the parent transcript incrementally from a
   per-transcript byte cursor, the way the watch tailer does. It tracks Agent
   calls by `tool_use` id and applies **today's state rule unchanged**
   (`subagentState`, `internal/mirrorhost/subagents.go:113`). It drops any call
   whose `tool_use` time is earlier than the sessions file's `startedAt`: a
   resumed CLI cannot host the previous process's in-process agents. A Gen change
   (`/clear`, a rotation) resets the ledger. The first read is bounded by the
   existing `BackfillWindow` (4 MiB tail). 🔴 **The ledger is
   read only while the status is `busy` or `waiting`.** An idle window costs no
   transcript read.

#### Wire: a `work` message, not a field on Window

The window list cannot hold it. `,"subagents":64,"shells":64` adds 27 bytes per
window, 13.8 KB at 512 windows. The repo chip proves the replacement pattern:

```go
// TypeWork (host → muster): Msg.Work, the WHOLE map of window id → work,
// sent after the window list whenever it changed. An empty map clears it.
const TypeWork = "work"

type WindowWork struct {
	Subagents     int  `json:"subagents,omitempty"`
	Shells        int  `json:"shells,omitempty"`
	ShellsAtLeast bool `json:"shellsAtLeast,omitempty"`
}

// Msg gains:  Work map[string]WindowWork `json:"work,omitempty"`
```

- `NormalizeWork` is applied by both halves. It restricts the map to listed
  windows, clamps each count to `[0, MaxWorkCount=99]` (the card shows `99+`),
  and drops entries that are all zero.
- **No version bump, backward-compatible both ways**, exactly like `repos`. An
  older muster's read loop drops the unknown type. An older host never sends it,
  and its cards show no chip.
- **Absent means "nothing to show"**, whether that is zero or unknown. muster
  never shows a number the host did not send.

#### ccd: `POST /agent/work`

ccd runs the same `cctasks` over the primary CLI and each lane, and sums them.
It posts `{"subagents":n,"shells":n,"shellsAtLeast":b}` when the sum changes,
and re-sends it every minute, like `/agent/blocked` (`cmd/ccd/blocked.go:30`,
`:175`). The route uses the same relay-agent auth as `/agent/blocked`
(`internal/api/approvals.go:319`), joins `ConnectSurfacePrefixes`
(`internal/api/connectors.go:51`), and is kept in memory with a TTL, the same as
`blockedReportTTL` (`internal/api/approvals.go:289`). A pod that stops reporting
loses its chip within the TTL.

#### UI

- **Card:** a chip in the header row next to the repo chip
  (`internal/ui/mirror.go:207`), for example `2 subagents · 1 shell`,
  `1 subagent` or `1+ shell`. There is no chip when both counts are zero. It has
  an accessible label ("2 subagents and 1 background shell running") and is a
  filter field (`work:subagents`, `work:shells`) in `agents_filter.go`. Agent
  cards (ccd) get the same chip.
- **Rendered view:** the same chip in the header. The per-agent cards stay as
  they are. A list of the shells themselves is out of scope.

#### Freshness and cost

- **Freshness:** within one host refresh (5 s), plus the hub's 1 s debounce,
  for mirrored windows. ccd is near-instant on a change, with a 1 min heartbeat.
- **Host cost per 5 s refresh:** the process table is already read. Added work
  is an in-memory descendant walk per Claude PID, and two `readlink`s per
  descendant. That is microseconds against the existing 39 ms. The transcript
  read only happens for busy or waiting windows, and only for the bytes since
  the last cursor.
- **Network:** the `work` map is sent only when it changes. Its worst case is
  512 entries of about 45 bytes, about 23 KB, well inside a frame. A test pins
  this: `TestTheLargestWorkMessageFitsOneFrame`.
- **Server cost per listing poll:** one map lookup per card. A change in the
  counts costs each open Agents view one list re-fetch, debounced.

#### Failure modes

| Failure | Effect | Handling |
|---|---|---|
| CLI crashes | sessions file is no longer live | rule 1: no Work, no chip. Orphaned shells are reparented, so not counted |
| CLI resumed after a crash | ledger holds launches from the dead process | the `startedAt` cutoff drops them |
| Agent killed, notification missing | ledger over-counts while busy | kill records (`TaskStop` result, `agents_killed`) close entries, and the next `idle` clamps to 0. The over-count lasts at most until the turn ends |
| Agent launched before the 4 MiB backfill window | ledger under-counts | documented. The `idle` clamp is still exact |
| Claude Code changes its task layout or id prefix | process count goes to 0 | rule 2 keeps `1+ shell` from the status. The live-Claude test catches it on upgrade |
| A foreground command during a turn | it would be counted | while a turn runs the card shows `1+ shell`, never a number. Phase 0 decides whether stdin can tell them apart |
| Sandboxed Bash | descendants may live in another PID or mount namespace | Phase 0 measures it. If it fails, fall back to the status's yes/no |
| Host older than this | no `work` messages | no chip |
| ccd stops reporting | report goes stale | TTL expiry removes the chip |

#### Alternatives considered

- **Hooks (`SubagentStart`/`SubagentStop`).** Rejected as the main source.
  Using them means asking every host to register two more hook events. No hook
  covers shells. A hook that misses an event (a crash, a hook timeout) leaves a
  count with nothing to correct it. The transcript records the same facts, and
  muster already reads it.
- **Transcript-only shells.** Rejected. A subagent's shell starts in another
  file, and a CLI crash leaves a start with no end. The process tree is
  authoritative and cheap.
- **Counts on `Window`.** Rejected because the frame is full (above).
- **Reusing the open view's subagent summaries.** Rejected because they exist
  only while a view is open (`internal/mirror/subagents.go:30-33`).

#### Test plan

All counts in fixtures are pairwise distinct, for example 3 subagents and
2 shells, so that a mutant that swaps the fields or hardcodes one fails.

- **Unit, `internal/cctasks`:**
  - The rule table: every status crossed with process count crossed with
    ledger, including `idle` with a stale ledger entry, which must report 0.
    Red/green: deleting the idle clamp turns that case red. Deleting the
    `ShellsAtLeast` fallback turns `shell` with 0 processes red.
  - Decision 3, one case per state:
    - `busy` with 3 shell processes reports `1+`, not 3.
    - `busy` with no turn open, one background agent running and 2 shell
      processes reports `1+`, not 2.
    - `shell` with 3 and no Monitor reports exactly 3.

    A mutant that reports the exact count while `busy` turns its case red.
  - **Monitor, in its real layout:**
    - A direct child whose argv holds `…/shell-snapshots/snapshot-zsh-…`, fd 0
      `/dev/null`, fds 1 and 2 `socket:[…]`, and no task file: `shell` with that
      alone reports `1+`.
    - The same process beside one `tasks/b…` shell reports `1+`, not 1. That
      catches a mutant that ignores Monitors.
    - Negative controls, neither counted: a socket-stdout child **without** the
      snapshot argv (an MCP server's shape), and a snapshot-argv child that is
      a grandchild rather than a direct child.
  - The process fixture uses the `ClaudeLocator.Proc` fixture tree with `fd/1`
    symlinks: a background task counts, a reparented orphan does not, a
    subagent's shell counts, a non-`b` task file does not, and a duplicate task
    id counts once.
  - The ledger, from shape-only synthetic records: a sync Agent call (running
    until its result); an async one (running until the notification in a
    `queue-operation` enqueue, and also inline); `TaskStop`; `agents_killed`;
    the `startedAt` cutoff; a Gen reset; a torn last line, which is not read.
- **Wire:**
  - `NormalizeWork`: a host cannot report a window it does not list, and counts
    are clamped.
  - `TestTheLargestWorkMessageFitsOneFrame`.
  - An older-host case: a hello without the field produces no chip.
- **Real tmux, private socket (`Tmux{Socket: …}`, as the ask integration tests
  do).** A pane runs a fake `claude` (argv[0] `claude`) that writes a sessions
  file with status `shell` and starts `sleep` with stdout on
  `…/tasks/b0000000a.output`. `Windows()` must report 1 shell (positive
  control). Kill the `sleep` and it must report 0 (negative control).
- **Live Claude (opt-in, like `make test-liveclaude`, run on every Claude Code
  upgrade):**
  - A real CLI runs `sleep 20` in the background. Expect `1+`, then `1`, then
    `0`.
  - A background Agent. Expect 1 subagent, then 0.
  - This is the only test that proves the bundle facts above still hold.
- **e2e (fake host, `-tags e2emirror`):**
  - A host sending `work` produces a card that reads `2 subagents · 1 shell`.
  - An empty map makes the chip disappear.
  - A host without the message shows no chip.
  - A ccd fake posting `/agent/work` gives the agent card the chip, and the chip
    expires after the TTL.

#### Effort

| Piece | Days |
|---|---|
| Phase 0 spike: the stdin discriminator, a sandboxed Bash, the Monitor signature (two builds, and from a subagent), one older Claude Code build | 0.5–1 |
| `internal/cctasks` (process count, ledger, rule) and unit tests | 2 |
| Mirror wire, hub, card chip, rendered-view chip, e2e | 1.5–2 |
| ccd reporter, `/agent/work`, agent-card chip, tests | 1–1.5 |
| **Total for Claude Code** | **5–6.5** |
| Hermes, after Hermes Phase 2: hook events added to the status file, process checkpoint read with PID and start-time validation | 1–1.5 |

---

## (b) The tmux identity colour

### What colour the operator sees

The tmux config this was scoped against assigns each scratch **slot** (a tmux
session reached by a hotkey) a colour and a codename from one table. It gives
three things colours:

1. **The slot popup.** The hotkey opens the session in `display-popup -S
   "fg=<colour>" -T " <codename> "`. 🔴 **This is the operator's "title and
   border" colour.** It is per **session**, so every window of a slot shares it.
   It exists **only as an argument inside a key binding**: `tmux list-keys -T
   root` shows it, but no tmux option holds it. Measured on tmux 3.7c, read-only:
   there are no `@` user options on any session; `popup-border-style` is one
   global neutral grey; and the only per-window options set are
   `automatic-rename` and `window-status-format`.
2. **The idle fade.** A status-line script sets each background window's
   `window-status-format` to a colour on an 8-step scale by idle time, using
   `set-window-option`. **This changes with time. It is recency, not identity.**
   It belongs to the recency dot and is never read for (b).
3. **Global chrome.** `pane-border-style`, `pane-active-border-style` and
   `popup-border-style` are the same for every window, so they identify nothing.

So the identity colour is stable: it does not change with the idle fade. But
muster-mirror cannot read it from tmux today. Parsing `list-keys` output for
`display-popup -S fg=…` next to an `attach-session -t <name>` would bind muster
to one person's key bindings, and was rejected.

The measured slot table has **20 slots but 16 distinct colours**. Four colours
are used twice. A colour is therefore **not unique**, and the card's title text
stays the identity.

### Design (decided): a tmux user option, `@muster-color`

muster-mirror reads a user option per window through the listing it already
runs. tmux resolves user options window, then session, then global. So one
mechanism covers a per-window colour, a per-session (slot) colour and a host-wide
default, on any host. Nothing is specific to one setup.

#### What was measured (tmux 3.7c, a private socket, `-f /dev/null`)

| setting | `#{E:@muster-color}` per window |
|---|---|
| `set -g @muster-color '#{?#{==:#{session_name},A},##aabbcc,#{?#{==:#{session_name},B},##ddeeff,}}'` | session A: `#aabbcc` on every window; B: `#ddeeff`; others: empty |
| plus `set -t C @muster-color '#123abc'` | C: `#123abc` (session literal) |
| plus `set -w -t A:1 @muster-color '#456def'` | A:1 `#456def`, A:0 unchanged (window override) |

🔴 **`E:` expands a literal colour as a format, and uppercase hex can collide
with tmux's one-letter aliases.** Measured: `#D79921` expanded to `%079921`
(`#D` is the pane id), and `#Fabd2f` to `*abd2f` (`#F` is the window flags).
Escaping as `##` works. The host therefore reads **both** forms and takes the
first that is a valid colour:

```go
// windowsFormat gains two fields BEFORE #{session_name}, which stays LAST:
//   …\t#{E:@muster-color}\t#{@muster-color}\t#{session_name}
// colour = first of (expanded, raw) matching ^#?[0-9a-fA-F]{6}$, lowercased
// with a leading '#'; else "".
```

- **Configurable:** `-color-option` / `MUSTER_MIRROR_COLOR_OPTION`, default
  `@muster-color`, must match `^@[A-Za-z0-9_-]{1,64}$`. A host whose config
  already keeps a colour in another user option can point muster at it.
- **Cost:** zero extra processes. It is two more fields in the one
  `list-panes -a` per refresh.
- **Freshness:** within one refresh (5 s) of a `set-option`, with no restart.

#### What a host's tmux config must add (exact spec)

This is written generically for any host whose tmux config is generated from a
**slot table**: rows of (tmux session name, colour, codename) that already
generate the slot popup bindings. The operator's host is one such host.

**The output.** The generator emits exactly **one** line into the generated
tmux config, after the popup bindings:

```tmux
set -g @muster-color '<F>'
```

**How `<F>` is built.** `<F>` is a right-nested tmux conditional folded over the
table's rows **in table order**, ending in the empty string:

```text
F(rows)          = ""                                  when rows is empty
F([r, rest...])  = "#{?#{==:#{session_name}," + r.session + "},##" + lower(hex(r.colour)) + "," + F(rest) + "}"
```

- `hex(c)` is the colour's 6 hex digits without the `#`. `lower` lowercases it.
  `##` is tmux's escaped `#`.
- For a two-row table, (`alpha`, `#AABBCC`) and (`beta`, `#ddeeff`), the line is
  exactly:

  ```tmux
  set -g @muster-color '#{?#{==:#{session_name},alpha},##aabbcc,#{?#{==:#{session_name},beta},##ddeeff,}}'
  ```

  The generator's own test pins this string.

**What the generator refuses.** It stops the build instead of emitting a line
that could mis-tint a window:

- a session name containing `,`, `}`, `{`, `#` or `'` (format syntax, or the
  shell quote that wraps the line);
- a session name that appears twice;
- a colour that is not exactly 6 hex digits after the `#`. The 3-, 9- and
  12-digit forms that some table readers accept are refused: muster accepts only
  `#rrggbb`. If the table allows the short form, the generator may expand `#abc`
  to `#aabbcc` instead of refusing.

**Why this form:**

- It holds **no runtime state**. tmux evaluates it per window when muster-mirror
  lists panes, so it needs no `session-created` hook.
- It is correct for a session that does not exist yet, and after a server
  restart or a session restore.
- The colour and the popup binding come from one table, so they cannot drift.
- Only exact session names match. A window in any other session gets an empty
  value, which leaves it neutral.

**Applying it.** It takes effect when the generated config is sourced (a new
server, or `tmux source-file`). muster-mirror sees it on its next 5 s refresh,
with no restart.

**Checking it.** For each slot session,
`tmux list-panes -a -F '#{session_name} #{E:@muster-color}'` prints its
lowercase colour. A session outside the table prints an empty colour.

**Not part of the spec:**

- a per-session `set-option -t <session> @muster-color …` from a
  `session-created` hook. It works, but it is runtime state that a missed hook
  loses.
- per-window overrides. A user may still set one by hand with
  `tmux set -w @muster-color '#3a7bd5'`, and muster honours it.

A host with no slot table adds nothing, and its cards stay neutral.

#### Wire: a `colors` message

The same pattern as `repos`: `TypeColors`, with
`Msg.Colors map[string]string` mapping window id to lowercase `#rrggbb`. The
whole map is sent after the window list whenever it changes, and an empty map
clears it. `NormalizeColors` is applied by both halves; it restricts entries to
listed windows and accepts only `^#[0-9a-f]{6}$`. The worst case is
512 × about 22 bytes, about 11 KB. There is no version bump and it is
backward-compatible both ways. **Inline it does not fit:** 18 bytes per window,
9.2 KB, against 18.3 KB free, with (a)'s 13.8 KB also asking for room.

#### UI, contrast and themes

The card's border is `border-line` today (`internal/ui/mirror.go:160-175`).
With a colour:

- The server computes **two variants per theme**, in Go
  (`internal/ui/identity_color.go`). Each keeps the colour's hue and chroma in
  OKLCH and moves only its lightness, toward the contrast side, until it meets:
  - **3:1** against the card surface `s1` for the border (WCAG 1.4.11);
  - **4.5:1** for the tinted title text (WCAG 1.4.3).

  The surfaces are the theme tokens: dark `s1` `#19162A`, light `s1` `#FFFFFF`
  (`web/css/input.css:49`, `:81`).
- It emits them as CSS custom properties in a `style` attribute, built from
  parsed integers and never from host text. The CSP allows inline styles
  (`internal/api/middleware.go:103`). `input.css` maps them under `.dark` and
  `:root:not(.dark)`, so the theme switch needs no JavaScript.

How far that moves the measured palette (16 distinct colours; prototype, not
gamut-mapped):

| target | dark: colours moved, largest ΔL | light: colours moved, largest ΔL |
|---|---|---|
| border 3:1 | 2 of 16, 0.04 | 7 of 16, 0.23 |
| title 4.5:1 | 6 of 16, 0.13 | 12 of 16, 0.33 |

**Decided (operator decision 2):** tint the border and the title, at 3:1 and
4.5:1, in both themes.

- In the dark theme, the tint stays close to the terminal's colour.
- 🔴 **In the light theme, 12 of the 16 colours visibly darken.**
  - Pale colours (cream, light yellow, light green) shift by up to 0.33 in OKLCH
    lightness.
  - They no longer look like their terminal colour there.
  - The title shifts more than the border.
- The operator accepts this as the price of readable titles.

Other rules:

- **The colour is never the only identifier.** Title, label and host text are
  unchanged (WCAG 1.4.1), which is also why duplicate slot colours are harmless.
- **It does not overlap the recency dot or the status glyph.** The tint goes on
  the border and title only. Recency stays on the dot, and status stays on the
  glyph and its text. Some slot colours share a hue with status colours (red,
  green), so the status must never be inferred from the border.
- **No colour:** the card looks exactly as it does today.
- **Cluster agents and ccd:** no tmux, no colour, neutral border. A colour in the
  agent spec could come later; it is out of scope here.
- **Hermes and plain shell windows** are tinted the same way. The colour belongs
  to the tmux window, whatever runs in it.
- **The rendered view and the terminal page** tint their header title with the
  same variables.

#### Failure modes

| Failure | Effect |
|---|---|
| Option unset or invalid | no tint |
| Uppercase literal hex collides under `E:` | the raw-form fallback recovers it |
| Option set from a hook that missed a session | that session is untinted. The global-format recipe avoids it |
| Host config and slot table drift | not possible when both are generated from the table |
| Older host | no `colors` message, no tint |
| Same colour on several slots | expected; titles disambiguate |

#### Alternatives considered

- **Extending the labels file** (`-labels-file`, `internal/mirrorhost/labels.go:24`)
  to `{"<session>":{"label":…,"color":…}}`. It is static, read once per
  muster-mirror start, and per session only. **Rejected by the operator**
  (decision 1). A tmux option is the generic, live mechanism any tmux user can
  set by hand.
- **Parsing `list-keys`.** Rejected: it is bound to one config's binding shape.
- **Reading `window-status-format`.** Rejected: that is the idle fade, which is
  recency.
- **A hash colour for untinted windows.** Rejected: it would invent an identity
  the operator never assigned.

#### Test plan

- **Unit, host:**
  - The colour field parser: an expanded valid value; an expanded invalid value
    with a raw valid one (the `#D…` collision, red without the fallback); both
    invalid; uppercase normalised; a field holding a tab or a newline. The
    session name must stay the last field: a session named with a tab still
    parses (`parseWindows`).
  - The `-color-option` name is validated.
- **Host-config generator** (in the host's own repository, not in muster):
  - The two-row example above, as an exact-string test.
  - Each refusal case (a name with `,`, a duplicated name, a 7-digit colour)
    fails the build.
  - Uppercase hex is lowercased.
- **Real tmux, private socket:** the measured table above, as a test. A global
  conditional; a session literal; a window override; a positive control (a
  colour is reported) and a negative control (no option, "").
- **Wire:**
  - `NormalizeColors`.
  - `TestTheLargestColorsMessageFitsOneFrame`.
  - The older host and older muster cases.
- **UI unit:**
  - For 1,000 random colours plus edge cases (white, black, pure yellow, pure
    blue, the measured palette), both variants meet 3:1 and 4.5:1 against `s1`,
    which is parsed from `input.css` by the same parser
    `TestThemeTokensMeetWCAG` uses, never restated.
  - Hue stays within a tolerance.
  - Red/green: deleting the adjustment fails the pale-yellow-on-white case.
  - A host value that is not `#rrggbb` never reaches the `style` attribute.
- **e2e:**
  - A fake host sends `colors`. In both the dark and the light theme, the card's
    computed `border-color` equals the server's variant for that theme.
  - A card without a colour keeps `border-line`.
  - An agent card is never tinted.

#### Effort

| Piece | Days |
|---|---|
| Host: format fields, parser, option flag, real-tmux test | 0.5 |
| Wire and hub (`colors`) | 0.5 |
| UI: contrast variants, CSS, card, rendered-view and terminal headers | 1 |
| Tests and e2e | 0.5–1 |
| **Total** | **2.5–3** |
| A host's tmux config: generate the one option from its slot table | about 0.25 |

---


## Total effort

Revised after the round-2 audit. (c) is deferred and not counted.

| Piece | Days |
|---|---|
| (a) Phase 0 spike: stdin, sandboxed Bash, the Monitor signature on two builds and from a subagent, an older Claude Code, and whether ccd sees notification turns (below) | 0.5–1 |
| (a) `internal/cctasks`: process count (task files and Monitors), ledger, work rule (`1+ shell` whenever a foreground call may run) | 2 |
| (a) mirror wire, hub, chips, e2e | 1.5–2 |
| (a) ccd reporter, `/agent/work`, agent-card chip | 1–1.5 |
| (b) host, wire, contrast variants, UI, tests | 2.5–3 |
| **Total in muster: (a) + (b)** | **7.5–9.5** |
| Host tmux config: generate `@muster-color` from the slot table | about 0.25 |
| Later, after Hermes Phase 2: Hermes work counts | 1–1.5 |

The ccd item in the spike concerns a possible existing ccd hazard, described in
the deferred section under "Who is affected": a notification turn may fire no
`UserPromptSubmit`. The spike only measures it. Any fix is separate work.

---

## Deferred: (c) The send queue and background agents

**Operator decision: DEFERRED (after the round-2 audit).** (c) is not built.

**Why: a dialog race that cannot be closed.**

- A background agent can open a **permission prompt on the main screen at any
  moment** while a window would read as `background`. Evidence:
  - the agent setup in 2.1.289 around offset **215162609**
    (`awaitAutomatedChecksBeforeDialog`);
  - the prompt opens with the cursor on `❯ 1. Yes`
    (`cmd/ccd/testdata/pane_permission_2.1.296.txt`).
- muster's automatic Enter could therefore **approve a permission prompt with
  nobody watching**.
- The screen checks below can make the gap between the last check and the
  Enter smaller. They cannot close it: Claude Code has no "submit only if idle".
- That breaks the premise of the round-1 rework, that the worst case of a
  misread `background` is Send-now behaviour. Send now's worst case is queued
  input. This one's is an approval.

**What stays true meanwhile:**

- Queued messages **keep waiting** while background agents run, as they do
  today.
- **Send now** is the manual path (`docs/sending-and-step-up.md:104-121`). A
  person pressing it is looking at the window.

**When to revisit:** only if Claude Code adds either of these:

- a submit-only-if-idle mechanism (an atomic way to submit input that is
  refused when a turn or a dialog is active);
- a permission mode in which background agents cannot raise prompts on the main
  screen.

The analysis below is kept as it stood after round 1, for that day. Its claims
about Send-now semantics are **superseded** by the race above.

### Open problems for any future attempt (round-2 audit)

These were found against the round-1 design and are not resolved:

- **Pairing drift over the whole transcript, and `popAll`.** Condition 5 pairs
  every `enqueue` with a later `dequeue` or `remove` by count, from the start of
  the transcript. A single unpaired or mis-ordered record, or a bulk drain (the
  CLI's `popAll`, which empties the queue without one record per item), shifts
  the count for the rest of the session.
  - The count can be wrong in either direction.
  - A future design must pair by an identifier, or reset the count at a known
    boundary, and must handle bulk drains explicitly.
- **A registered hook is not proof that the hook fires.** The compaction guard
  trusted that a `muster-mirror compact-hook` entry in the settings files means
  `PreCompact` reaches it. Things that break that:
  - hooks disabled (`settingsHooksOff`);
  - managed-only hook policy;
  - a hook timeout;
  - a settings layer the host did not read;
  - an untrusted workspace.

  A future design needs evidence that the hook **fired**, such as a heartbeat
  the hook writes on every event, checked against the transcript, not evidence
  that it is registered.
- **The gates must accept `absorbed_mid_turn`.** A message pasted during a turn
  can be absorbed into that turn as queued input instead of being delivered
  after it. The round-1 gates' pass criteria ("delivered after it") would score
  that outcome wrongly. A future gate must:
  - recognise `absorbed_mid_turn` as its own outcome;
  - decide in advance whether it passes;
  - assert on it explicitly.


### The problem

Claude Code reports `busy` while any background agent or workflow task runs,
even when no turn does (`Cye`, offset **229767817**; see
[(a)](#the-sessions-file-one-status-no-counts)). The prompt takes input then.

muster's send queue sends only to an `idle` window
(`internal/sendqueue/dispatcher.go:87-107`, fed by `Hub.AgentState`,
`internal/mirror/agentstate.go:79-122`). So a queued message waits until every
background agent has finished, which can take tens of minutes. The current
workaround is to press **Send now** by hand
(`docs/sending-and-step-up.md:104-121` names this case).

🔴 **The status time does not move either.** Claude Code writes the sessions
file only when the status value changes. A turn that starts and ends while a
background agent keeps the status at `busy` leaves `statusUpdatedAt` where it
was. The queue's rule that the next message waits for "a status time newer than
the last paste" (`internal/sendqueue/dispatcher.go:98-102`) therefore waits for
the agents too. The fix needs its own turn-end time.

### What (c) promises, and what it does not

🔴 **(c) cannot guarantee that a queued message never meets a running turn.**
Claude Code offers no atomic "submit only if no turn is running". Three gaps no
check by muster can close:

- **Transcript flush delay.** The CLI writes transcript records in batches every
  100 ms (`FLUSH_INTERVAL_MS=100`, offset **211812153**), so a turn can be
  running for up to about 100 ms before its first record is in the file.
- **muster's own delay.** muster waits 150 ms between the paste and the Enter
  (`enterDelay`, `internal/mirrorhost/claude.go:659`). A turn can start inside
  that gap.
- **Turns that start with no warning.** Some notification turns start with no
  `queue-operation` enqueue before them.
  - The round-1 audit measured **65 of 3,013** notification turn starts like
    that. This is the canonical example.
  - A re-measurement over a different window (the newest 60 transcripts on one
    host) found **51 of 2,029**: 30 for `b` tasks (Bash and Monitor), 21 for
    agents.
  - The transcript gives no warning before such a turn, so no condition below
    can see it coming.

**So (c) has Send-now semantics.** Its worst case is exactly what muster's
**Send now** does on purpose: the text is pasted and Entered while a turn runs.
In Claude Code 2.1.289, Enter during a running turn **queues** the input
natively, so the turn is not interrupted. The consequence of a misread
`background` is that **the queued message joins that turn as queued input,
usually the turn a notification started, instead of getting a turn of its
own.**

The conditions below make that rare. They do not make it impossible, and this
page does not claim they do. Whether Claude Code really queues in each of these
situations is not assumed: it is the [ship gate](#ship-gate-and-the-switch).

### Who is affected

- **Mirrored Claude Code windows: affected.** This is the case above.
- **ccd: not affected by the over-hold.** Its `busy` comes from hooks
  (`UserPromptSubmit` sets it, `Stop`/`StopFailure` clear it;
  `cmd/ccd/server.go:392-409`), not from the sessions file, and `Stop` fires
  when the turn ends whatever background agents still run.
  - 🔴 **UNVERIFIED, the opposite hazard.** A notification turn may fire no
    `UserPromptSubmit`. ccd would then treat a running turn as idle and paste
    into it. Under native queuing that also means Send-now semantics, and ccd
    may also attribute the reply to the wrong prompt (it binds replies by
    `prompt_id`).
  - The (a) Phase 0 spike measures it. If it is confirmed, ccd refuses while the
    transcript shows a turn open (condition 2 below).

### The signal

The host derives a status value, `background`, which means "busy, and the
transcript shows no turn open or about to start". It derives it per Claude
window **only when every condition holds**. Otherwise, or when one cannot be
read, the window stays `busy`.

1. **Status.** The live sessions file says `busy`. Its PID and `procStart` match
   a live process (`liveSessions`), it names no `parkedJobId`, its `sessionId`
   is the window's Gen, and its `version` is on the
   [switch's list](#ship-gate-and-the-switch).
2. **The last boundary is a turn end.** The newest main-thread (`isSidechain`
   false) boundary record is a `system` `turn_duration`.
   - Turn starts: a `user` record that is not a tool result and not `isMeta`,
     including `origin.kind` `human` and `task-notification`.
   - Turn ends: `turn_duration`.
   - An interrupt usually writes no end record (see
     [(a)](#the-turn-end-record-counts-background-agents)), so the turn reads as
     open and the window stays `busy` until the next real end.
3. **That end left background work pending.** Its `pendingBackgroundAgentCount`
   plus `pendingWorkflowCount` is at least 1. An absent field counts as 0.
   🔴 **This condition is load-bearing.** The deferred `turn_duration` records
   written for swarm and teammate turns carry **no** pending count. Without
   this condition, a `busy` caused by a teammate would read as `background`.
4. **Something is still running.** The (a) ledger has at least one background
   agent still running that was launched before that turn end.
   `pendingBackgroundAgentCount` also counts agents that **finished but are not
   yet delivered**, so a positive count alone does not mean anything runs.
5. **Nothing is queued to start a turn.** Over the transcript's
   `queue-operation` records, both hold:
   - every `enqueue` has a matching later `dequeue` or `remove`, paired first in,
     first out by count;
   - there is **no `dequeue` after the last turn start**.

   An unmatched enqueue is a finished task's notification or a queued command
   that the CLI will deliver as a turn. A dequeue after the last start is a turn
   being started from the queue.
6. **No agent is about to finish.** No running background agent's own transcript
   ends in a final reply (rule 3 of `subagentState`,
   `internal/mirrorhost/subagents.go:113-131`).
7. **The status has not moved since.** `statusUpdatedAt` is no later than the
   `turn_duration`'s `timestamp`.
8. **No compaction can be running.** See below.

With all eight true, the window reports `Status: "background"` and
`StatusSince` set to the `turn_duration`'s timestamp in Unix ms. That is the
CLI's clock, the same one `statusUpdatedAt` uses, so the queue's next-message
rule works on turn ends again.

#### Compaction

🔴 **A manual `/compact` writes nothing to the transcript until it finishes.**
The round-1 audit measured 17–107 s, median 51 s. While background agents keep
the status at `busy`, nothing in conditions 1 to 7 changes during it. The
signals that do exist (2.1.289):

| signal | when | reliable? |
|---|---|---|
| `PreCompact` hook (`trigger`: `manual`/`auto`), offset **209758278** | before it starts | fires on every compaction, but only if the hook is registered |
| `PostCompact` hook, offset **209759464** | after it succeeds | 🔴 **success path only.** Its call sites follow the summary (offsets **209376892**, **209380039**), so a failed or cancelled compaction fires none |
| spinner text `Compacting conversation`, set on `compact_start` (offset **217653599**); also `Running PreCompact hooks…` and `Running PostCompact hooks…` | while it runs | on screen, above the input box |
| `system` `compact_boundary` record | when it finishes | after the fact |
| sessions file | — | no field for it; status already `busy` |

The design uses **two** of them, and both must agree that no compaction is
running:

- **A marker from muster's own hook.** muster-mirror ships
  `muster-mirror compact-hook`, registered for `PreCompact` and `PostCompact`.
  - It follows `muster-mirror ask`'s contract (`docs/mirroring.md:280-325`):
    nothing on stdout, always exit 0.
  - On `PreCompact` it writes `<state>/compacting/<session-id>` holding the
    hook's time.
  - The marker is cleared by `PostCompact`, or by any main-thread transcript
    record newer than it (`compact_boundary`, a turn start or a turn end).
    **Never by time alone**, so a cancelled compaction holds the window until
    the transcript moves.
- **The screen.** The input-line check below refuses when the area above the
  input box shows any of the three spinner texts.

🔴 **A host whose hook is not registered never reports `background`.** The host
reads Claude Code's settings files (user, then project) for a command matching
`muster-mirror compact-hook` under **both** `PreCompact` and `PostCompact`. If
either is missing, the host cannot see a compaction, so it reports `busy`, and
`-list -windows` says why.

What Claude Code does with a pasted Enter **during** `/compact` is not known.
It is gate 3.

### The input line, before the paste and before the Enter

Like the Hermes gated send ([hermes.md](hermes.md)), the host reads the pane
with `capture-pane` at two moments and acts only on what it sees.

**The reader.** It reuses ccd's bottom-of-screen anchoring (`isRule` and
`dialogOnScreen`, `cmd/ccd/dialog.go:49`, `:85`, read against real captures in
`cmd/ccd/testdata/pane_*_2.1.296.txt`), moved to a shared package. The input box
is the region between the screen's last two horizontal rules; the status line is
below it, and the spinner line is above it.

**Before the paste**, refuse (nothing typed, the message is queued again, and
the reason is shown) unless all of these hold:

- the box holds only the prompt glyph, with no draft (a half-typed draft is
  never appended to);
- `dialogOnScreen` is false;
- the spinner line shows no compaction text.

**Before the Enter**, after the paste and the 150 ms `enterDelay`:

- the box must hold exactly the pasted text, after wrapping is normalised. A
  long paste may show as Claude Code's `[Pasted text #N …]` placeholder, which
  also counts as a match;
- conditions 1 to 8 are re-derived from fresh reads and must still give the same
  `StatusSince`.

**If anything fails there:** a draft appeared, a person pressed Enter, the box
vanished, or a turn started. Then the host **does not press Enter** and reports
`pasted, not submitted: the text is in the input box`.

- 🔴 The message is **not** queued again. It is in the box; queueing it again
  would paste it twice.
- The queue marks it `failed`, with that detail, and the owner finishes it in
  the terminal.
- 🔴 **The host sends no Escape and no Ctrl+C to clear the box.** In 2.1.289,
  Escape is `chat:cancel` and Ctrl+C is `app:interrupt` (keybinding table,
  offset **205387445**). Both interrupt a running turn, which is exactly the
  situation in which the check failed.
- `ctrl+l` is `chat:clearInput` in the same table. It is a candidate to measure
  live, **not part of this design**.

### Ship gate and the switch

**The switch:** `-background-versions` / `MUSTER_MIRROR_BACKGROUND_VERSIONS` on
muster-mirror.

- It is a comma-separated list of exact Claude Code versions (as the sessions
  file's `version` spells them) for which the host may report `background`.
- **Default: empty, so (c) is off.** An empty list, or a version not on it,
  means the host reports `busy` exactly as today.
- **What flips it:** a version goes on the list only after the live gate suite
  below passes, **every gate**, against that exact version, with the result
  recorded in `docs/testing.md` (the version, the date and the outcome of each
  gate).
- **What unflips it:** a Claude Code upgrade is a new version, so it is off
  again until the suite passes for it. A failed re-run removes the version.

**The gates** (opt-in suite, `make test-liveclaude-background`, real CLI in a
private tmux server). Each is pass or fail, and any failure keeps (c) disabled
for that version:

1. **Paste during a streaming turn.** Paste and Enter while the assistant is
   streaming. Pass: the turn is not interrupted (its `turn_duration` is written
   with no interrupt record), and the message is delivered after it, as queued
   input.
2. **Paste at a notification-turn start.** A background agent finishes, and the
   paste lands in the first 0–300 ms of the notification turn, swept in 50 ms
   steps. Pass: same as gate 1, and the notification is also processed.
3. **Paste during manual `/compact`.** Pass: the compaction completes (a
   `compact_boundary` is written) and the message is delivered after it, not
   lost and not aborting the compaction. Also check that the marker and the
   screen check both reported a compaction at the paste time; that is required
   too.
4. **Paste with a half-typed draft in the input line.** Pass: the host refuses
   before typing anything, and the draft is byte-for-byte unchanged
   (`capture-pane` before and after).

### Wire and server

- **No new field.** `background` rides `Window.Status`: +6 bytes per window,
  3 KB at 512 windows, inside the 18 KB free.
- **Older muster.** muster's `welcome` gains `backgroundStatus: true`. A host
  sends `background` only to a muster that advertised it; an older muster gets
  `busy`. If an older muster did receive `background`, `NormalizeStatus` would
  read it as unknown, which holds the queue.
- **Queued pastes.** A queued paste to a `background` window carries
  `ExpectBackgroundSince`. The host re-derives everything before the paste and
  before the Enter, as above. A refusal before typing answers `pasted` with
  `Refusal: "turn"` and is queued again, like `ErrSuperseded`. A failure after
  typing is `Refusal: "unsubmitted"` and ends the message `failed`.
- **muster.** `Hub.AgentState` gives `Idle: true` for `background`, for the queue
  only. The status glyph and attention read it as working, and the card says
  `working · background agents`.
- **A person's own Send** and **Send now** are unchanged.

### Failure modes

| Failure | Effect | Handling |
|---|---|---|
| A turn starts in the flush delay, during `enterDelay`, or with no warning (65 of 3,013) | message joins that turn as queued input (Send-now semantics) | stated, not prevented. Gates 1 and 2 must show Claude Code queues |
| Draft typed, or Enter pressed, between paste and Enter | — | no Enter; `pasted, not submitted`; not queued again |
| Manual `/compact` in progress | — | marker plus screen check, both required; gate 3 |
| Compact hook not registered | — | host never reports `background` |
| Compaction cancelled or failed (no `PostCompact`) | marker stays | cleared only by a newer transcript record; holds until then |
| Interrupt with background agents running | no `turn_duration` | turn reads as open; stays `busy` |
| Teammate or swarm `busy` | no pending count | condition 3 fails, so stays `busy` |
| Finished agent not yet delivered | its enqueue is unmatched | condition 5 fails, so stays `busy` |
| Claude Code upgraded | new version | not on the list, so (c) is off until the gates pass |
| Record shapes change | conditions stop matching | stays `busy`; the gate suite on the new version catches it |
| Older host or older muster | — | `busy`, as today |

### Test plan

Fixtures are synthetic records and screen captures in the **real shapes**
described on this page. They contain no transcript text. Times are pairwise
distinct.

- **Unit, the derivation (`internal/cctasks`)**, as a table. Each row is red
  when its own condition is deleted:
  - **Positive:** start, then `turn_duration` with
    `pendingBackgroundAgentCount: 2`, one agent running in the ledger, every
    enqueue matched, status `busy` with an earlier `statusUpdatedAt`, no marker,
    version listed. Expect `background`.
  - A later human start: `busy`. A later `task-notification` start: `busy`.
  - An interrupt with no end: `busy`.
  - Pending count absent: `busy`. Pending count 0: `busy`.
  - A teammate-style `turn_duration` with no pending fields: `busy`.
  - Pending 1 whose agent's notification is enqueued and not dequeued: `busy`.
    This is the finished-but-undelivered case.
  - An enqueue matched by `remove`: allowed. A `dequeue` after the last start:
    `busy`.
  - An agent whose transcript ends in a final reply: `busy`.
  - `statusUpdatedAt` later than the end: `busy`.
  - A compaction marker present: `busy`. A marker older than a newer transcript
    record: cleared.
  - Version not listed: `busy`. Empty list: `busy`.
  - Sidechain records ignored. A torn last line not read.
- **Unit, the compact hook:** no stdout and exit 0 on every input; the marker
  written on `PreCompact` and removed on `PostCompact`; an invalid session id
  ignored. The settings check is red when either event's registration is
  removed from a fixture settings file.
- **Unit, the input-line reader**, against captured fixtures from 2.1.289 and
  2.1.296 at 80 and 120 columns:
  - the existing `pane_idle_*` captures: an empty box passes;
  - `pane_idle_dialogwords_inputbox` and `pane_idle_quoted_inputbox`: a draft,
    refused;
  - new captures: a one-line draft, a multi-line draft, a streaming spinner,
    `Compacting conversation`, `Running PreCompact hooks…`, a pasted text
    wrapped, and the `[Pasted text #N …]` placeholder;
  - each refusal is red when its check is deleted.
- **Unit, the host send:**
  - A start appended between decision and paste: refused, nothing typed (the
    fake backend records no keys).
  - A draft appearing between paste and Enter: no Enter key recorded,
    `unsubmitted`, and the queue row `failed` and not requeued.
  - A ledger asserts that the send path never emits Escape or `C-c`.
- **Unit, the queue:** `Decide` treats `background` as idle; the next message
  waits for a strictly newer `turn_duration`; `unsubmitted` ends the row
  `failed`.
- **Wire:**
  - Without `welcome.backgroundStatus`, the host sends `busy`.
  - An older muster reads `background` as unknown.
  - The window-list size test is re-measured.
- **Real tmux, private socket:** a fake `claude` pane that draws an input box
  from fixture text and appends transcript records.
  - The positive case shows `background`.
  - A draft typed into the pane is refused, and the pane is unchanged.
  - A start appended mid-send: no Enter.
- **Live gates:** the four gates above, and the switch is only flipped on their
  record.
- **ccd:** the Phase 0 measurement; if the hazard is confirmed, a refusal test
  while the transcript shows a notification turn open.

### Effort (as estimated before the deferral; not scheduled)

| Piece | Days |
|---|---|
| Derivation in `internal/cctasks` (reuses the (a) ledger), queue-record pairing, table tests | 1–1.5 |
| `muster-mirror compact-hook`, marker, settings check, tests | 1 |
| Shared input-line reader, new fixture captures, tests | 1–1.5 |
| Host send: both re-checks, `turn`/`unsubmitted` outcomes, no-cancel-keys ledger | 0.5–1 |
| Wire (`background`, welcome flag), hub, queue, card text, the version switch | 0.5–1 |
| Live gate suite (four gates against a real CLI) and its record in `docs/testing.md` | 1–1.5 |
| ccd guard, if the spike confirms the hazard (the spike is in (a)'s Phase 0) | 0–0.5 |
| **Total** | **5–8** |
