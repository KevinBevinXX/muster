# Design: live work counts and tmux identity colours on agent cards

**Status: proposed. Nothing on this page is implemented.** It scopes two
additions to the Agents view's cards:

- **(a) Live work counts.** A card shows how many subagents and background
  shells its session has running now, for example `2 subagents · 1 shell`.
- **(b) The tmux identity colour.** A mirrored card's border and title are
  tinted with the colour the operator's tmux setup gives that window, so the
  card and the terminal look like the same thing.

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
| (a) subagents, mirrored Claude Code | **Yes.** Exact when the session is idle; inferred from the transcript while busy | the sessions file's status, plus a transcript ledger of Agent calls | same message | 5–6.5 days for (a) in all |
| (a) dispatched `claude-code` agent (ccd) | **Yes**, the same code inside the pod | the same | `POST /agent/work` | (included above) |
| (a) Hermes | **Partly**, and only after Hermes Phase 2 | `subagent_start`/`subagent_stop` hooks; the process checkpoint file | the same message | 1–1.5 days after Phase 2 |
| (b) identity colour | **Yes**, but tmux does not hold the colour today: the host's tmux config must declare it | a tmux user option, `@muster-color`, read in the listing muster-mirror already runs | new `colors` message | 2.5–3 days, plus about 2 hours of host config |

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
  The comment at `internal/mirror/wire.go:446` still says 96%, which is out of
  date. The repo chip met the same limit and added a message of its own,
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

🔴 **Side finding, outside this design's scope.** `busy` also covers a
**background subagent running while no turn is.** muster's send queue waits for
`idle` (`internal/mirror/wire.go:410-420`). A queued message therefore waits for
every background agent to finish, even though the prompt would take it. This
should be filed separately.

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

Two limits, both measured on one sample each:

- 🔴 **A foreground Bash call looks the same** (stdout on
  `tasks/b<8>.output`). The only difference seen was stdin: a socket for the
  foreground call, `/dev/null` for the background one. One sample is not a
  discriminator, and a command backgrounded with Ctrl+B keeps its socket. Phase 0
  measures it. Until then the count covers **all** running shell tasks, and while
  a turn runs that includes the command in the foreground.
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
| shells, count | **exact** while the CLI lives (process tree); includes a foreground command during a turn | **exact**, same code in the pod | **UNVERIFIED**: `~/.hermes/processes.json` lists running background processes with `pid`, `host_start_time` and `session_key` (installed build: `tools/process_registry.py:44`, fields at `:653-658`) |
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
   `CLAUDE_CODE_TMPDIR` override still works. If the status is `shell` and the
   count is 0 (the files are unreadable, or the layout changed), report
   `Shells: 1, ShellsAtLeast: true`. The card then says `1+ shell`, never `0`.
3. **Subagents:** 0 when the status is `idle` or `shell`. When it is `busy` or
   `waiting`, use the count of **running** entries in the transcript ledger.
4. **The transcript ledger** follows the parent transcript incrementally from a
   per-transcript byte cursor, the way the watch tailer does. It tracks Agent
   calls by `tool_use` id and applies **today's state rule unchanged**
   (`subagentState`, `internal/mirrorhost/subagents.go:113`). It drops any call
   whose `tool_use` time is earlier than the sessions file's `startedAt`: a
   resumed CLI cannot host the previous process's in-process agents. A Gen change
   (`/clear`, a rotation) resets the ledger. The first read is bounded by the
   existing `BackfillWindow` (4 MiB tail). 🔴 **The ledger is read only while the
   status is `busy` or `waiting`.** An idle window costs no transcript read.

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
| A foreground command during a turn | `shells` includes it | documented. Phase 0 decides whether stdin can tell them apart |
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
| Phase 0 spike: the stdin discriminator, a sandboxed Bash, one older Claude Code build | 0.5–1 |
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

### Recommended design: a tmux user option, `@muster-color`

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

#### What a host's tmux config adds

A host whose tmux config gives each scratch slot a colour declares that colour
once more, as the option. It should be generated from the same slot table that
generates the popup bindings, so the two cannot drift:

```tmux
# One global option: tmux evaluates it per window (#{E:…}), so it needs no hook,
# survives a server restart and a session restore, and holds no runtime state.
# Write each colour with '##' (a literal '#') and lowercase hex.
set -g @muster-color '#{?#{==:#{session_name},<slot-1-session>},##<hex1>,#{?#{==:#{session_name},<slot-2-session>},##<hex2>,…}}'
```

An alternative is `set-option -t <session> @muster-color '<hex>'` per slot
session, from a `session-created` hook. It works, but it is runtime state that a
missed hook loses. The single global option is preferred. A host with no slot
table needs nothing: its cards stay neutral. One window can be tinted by hand
with `tmux set -w @muster-color '#3a7bd5'`.

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

In the dark theme the tint stays close to the terminal's colour. In the light
theme, pale colours (cream, light yellow, light green) darken a lot, and the
title more than the border. That is the cost of readable text, and is decision 3
below.

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
  muster-mirror start, and per session only. It is a reasonable second choice,
  since the host already generates that file from the same table. Rejected as
  the primary because a tmux option is the generic, live mechanism any tmux user
  can set by hand.
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

## Decisions for the operator

1. **(b) Mechanism:** the tmux user option (recommended), or a colour field in
   the labels file?
2. **(b) Scope:** the slot colour tints **every window** of that slot's session.
   Windows outside any slot stay neutral, with no hash colour. Is that right?
3. **(b) Fidelity versus readability:** may muster shift lightness so the
   border meets 3:1 and the title meets 4.5:1? In the light theme this darkens
   pale colours by up to 0.33 L. The alternative is to tint the border only and
   keep the title in the normal text colour, which needs a shift of at most 0.23
   L in the light theme.
4. **(a) Foreground commands:** count every running shell task (recommended
   until Phase 0 shows stdin can tell them apart), or show only `1+ shell` from
   the status while a turn runs?
5. **(a) Monitors:** the bundle classifies a `local_bash` task with
   `shell_kind === "monitor"` as a monitor, not a shell (`function BT`, offset
   **226476532**). Such a task still runs as a shell process with a task output
   file, so the process count includes it unless the ledger excludes it. Count
   it as a shell (recommended, simpler), or separately?
6. **Side finding:** the send queue waits through background subagents, because
   their activity reads as `busy`. Should this be filed as its own issue?
