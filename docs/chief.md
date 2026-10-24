# The chief

The **chief** is your fleet agent: the one agent with no task and no repository,
whose job is answering you about the state of the work. Plugin messages are
delivered to it, and it alone is offered the chief tools (see below).

## Which agent is the chief

Two rules, both in `internal/api/chief_agent.go`:

- The **reserved slug**: `chief` for user 1, `chief-<id>` for any other user.
  Only `POST /chief/provision` and `POST /api/chief/replace` create one.
- The **label**: everything that *talks* to the chief (the panel, recaps,
  threads, plugin delivery, `muster chief ask`) resolves it by display name
  `chief`, the lowest id winning among your agents.

Both are delete-protected: `DELETE /agents/{id}` and a task dismissal refuse
them. Replacing the chief (below) is the one path that deletes one.

Neither rule looks at the agent's **kind**. A chief can run on either kind.

## The chief on each kind

| | `gateway` | `claude-code` |
|---|---|---|
| Standing instructions | `AGENTS.md` in its workspace (`agentspec.ChiefInstructions`) | the Claude Code CLI's managed-policy memory file, `/etc/claude-code/CLAUDE.md` (`agentspec.ChiefInstructionsClaudeCode`) |
| How it reaches muster's tools | native function tools on each turn, plus the in-pod `muster` CLI | muster's MCP server only, as `mcp__muster__<tool>`; the image has no `muster` CLI |
| The subsystem store (cairn) | when `MUSTER_AGENT_CAIRN_URL`/`_TOKEN` are set | never: the client is not in the image and the network policy cannot reach a store. A configured store is skipped with a log line. |
| Needs | an OpenRouter key | a Claude account, and `MUSTER_AGENT_CONNECT_URL` (no MCP server without it; the build refuses) |

The managed-policy path is documented in
[Claude Code's memory docs](https://code.claude.com/docs/en/memory) and was
checked against the pinned CLI. It is outside the agent's `/data` volume, so it
never shadows a `CLAUDE.md` in the agent's workspace. The file is rebuilt from
the spec on every apply, and a running pod picks it up at its next start, the
same as the gateway kind's `AGENTS.md`.

A `claude-code` **worker** gets no standing instructions: the worker prose
describes the in-pod CLI and the autosave log, and that image has neither.

### The chief tools over MCP

The chief is offered these tools, and no other agent is:

| tool | needs |
|---|---|
| `task_create_gated`, `reply_draft` | plugins on |
| `fleet_list`, `focus_search`, `sessions_overview` | session focus |
| `route_to_session` | session routing |
| `attention_list` | the attention queue |
| `task_route_attention`, `task_forward` | task routing (`MUSTER_TASK_ROUTING`) |
| `task_dispatch` | task routing, and you opted in (`MUSTER_CHIEF_DISPATCH_OWNERS`) |

The other way round, the chief is the one agent **not** offered
`connector_request`, `connector_status`, `browser_flow_request` or
`browser_flow_status`: its connectors are the standing grants you give it
(`connectors_list` shows them), and it does not ask for browser flows.

`task_dispatch` starts a **claude-code** agent unless the chief passes
`kind: "gateway"` or the deployment does not enable claude-code: what you
usually want from the chief is a Claude Code pod. That agent runs on an
account from your Claude account pool and has no per-agent model. It works in
the task's repository (or the `repo` the chief passes, one of your connected
repositories): its session starts inside a checkout, so the repository's
`CLAUDE.md` and project skills load. Its optional `instructions` (at most
1 KiB) open the agent's first message, fenced as relayed by the chief, before
the task's text and muster's rules (which come last and win). They are
accepted **only** as your own words: verbatim, your answer to an attention ask
about that same task ("start a Claude Code agent and have it open with skill
X") — given on `/attention`, or through one of your mirrored sessions'
`attention_answer` (operator decision "Just remove the filter": a session fed
outside text could therefore write a dispatched agent's prompt — the accepted
risk is spelled out in [attention.md](attention.md#mirrored-claude-code-sessions)).
Instructions from an MCP answer say so: the agent's first message names them
as your answer given over MCP from `<host>`/`<session>`, and the audit row
records `instructions_via` / `instructions_from`. Anything else is refused, and the chief is told to ask you with
`task_route_attention`. The `task.chief_dispatch` audit row records the kind,
the instructions and the ask they came from (and, for an MCP answer, where).
No muster page reads that row back yet: query it
(`SELECT detail FROM audit_log WHERE action = 'task.chief_dispatch' AND object = 'note:<task id>'`).
What the agent was told is also its first message, on its transcript. Guards: [plugins.md](plugins.md#task_dispatch-the-guards).

`tasks_list` and its siblings are offered to every agent. The chief's
`tasks_list` covers your whole board, where a worker's covers only its own
tasks. That scope, like the chief-only filter, is decided by the chief
predicate, never by the kind.

## Sessions on more than one runtime

With `MUSTER_CHIEF_SESSION_RUNTIMES=claude-code`, each new chief chat session
can be started on a Claude Code CLI of its own, with a model of its own, both
fixed for the session's life:

| runtime | model | what runs the session |
|---|---|---|
| (none) | the chief's own | the chief's own runtime. On a `claude-code` chief, the one CLI `ccd` has always run. Every session that existed before migration 0030 is one of these. |
| `claude-code` | a Claude model (`opus`, `sonnet`, `haiku`, or a model id) | a Claude Code CLI of its own in the chief's pod, started with `--model`. Needs a `claude-code` chief. |

Open one from the chief panel: its thread list has a **New session** picker.
Each thread names its runtime and model. The route underneath is
`POST /agents/{id}/sessions` with `runtime` and `model` form fields. A session's
runtime and model never change; open a new session to use another.

A turn for a `claude-code` session runs in a CLI of its own (tmux session
`cc-<id>`, started with the session's `--model`); a turn for a session with no
runtime runs in the original CLI, as before. At most `CCD_MAX_SESSIONS` (3) such
CLIs run at once. `ccd` on port 18789 is still the only port the pod exposes.

**The retired `gateway` runtime.** A `gateway` entry (an OpenRouter model on
the gateway runtime, run as a second container beside `ccd` in a
`claude-code` chief's pod, with a memory request of 512 MiB) used to be
accepted here. It has been removed — for every chief, not only a
`claude-code` one: a `gateway`-kind chief can no longer open sessions with a
per-session OpenRouter model either; its sessions all run on its own runtime
and model. muster now ignores a `gateway` entry in
`MUSTER_CHIEF_SESSION_RUNTIMES` with one warning at boot, so a deployment
that still names it keeps booting; drop it from the deployment. A chief started
before the removal keeps that container until its next **Stop → Start**, which
re-applies its spec without it (and deletes its `<name>-sidecar-env` Secret).

**The default session.** One session per agent is its default. Plugin
deliveries and `muster chief ask` go to it, so they share one conversation
instead of one per plugin. This is so whether or not
`MUSTER_CHIEF_SESSION_RUNTIMES` is set; that setting only adds the runtime
picker. Until you choose one, it is the chief's original
session (the one keyed `agent:<name>:webchat`, or the oldest). Move it with
`POST /agents/{id}/sessions/{sid}/default`.

**Tool names.** The gateway runtime refuses a whole turn when one of the tools
muster offers has the name of one of its built-in tools. `agents_list` was such
a name and is now `fleet_list` on every runtime; a test pins every chief tool
against the runtime's built-in names.

## Replacing the chief on another kind

An agent's kind is fixed for life. To move the chief to another kind, replace
it: `POST /api/chief/replace`, or `muster chief replace`.

```sh
muster chief replace --kind claude-code --cc-account <account>               # the plan; changes nothing
muster chief replace --kind claude-code --cc-account <account> --confirm chief
```

It **deletes** the agent holding your reserved slug (its chat history, focus,
connector grants and workspace volume go with it) and creates a new chief under
the same slug, labelled `chief`, on the kind you name. It is a hook-tier route:
the shared hook token (on an armed server) or your personal token.

- **Without `--confirm <slug>`** nothing changes. The server answers 428 with
  the plan, and the CLI prints it and exits 2.
- **It refuses (409, nothing changed)** when:
  - a turn is in flight to the chief: a plugin delivery, its kickoff or a memo
    holds the chief's session claim, or one of your chat turns to it is
    running. While the swap runs, new chat turns to the old chief are refused;
  - another of your agents is also labelled `chief`. The new row would have the
    highest id, so the older one would keep answering;
  - the create would be refused anyway: a kind that is not enabled, a model
    for a `claude-code` chief, or a `claude-code` chief on a server with no
    MCP door;
  - the Claude account is marked auth-failed for its current token (a pin
    skips that check at dispatch, so the swap checks it itself);
  - the chief is already on the requested kind and account, and `--model`
    names a different model. A kept chief is not rebuilt with a new model;
    change it with the agent's model setting instead;
  - another replacement removed the chief after this call read it.
- **A plugin message left `delivering` by a crashed process** does not block
  the swap. Holding the chief's session claim, the swap knows no delivery is in
  progress, so it settles such a message as `unconfirmed` (as the next
  delivery's crash recovery would: never resent) and reports its id under
  `deleted.settledUnconfirmed`. Deleting the chief without that would leave the
  message with no agent, and nothing could settle it.
- **It is safe to re-run.** Each call works from the agent table:
  - the slug is on another kind or account: delete it, then create;
  - the slug is already on the requested kind and account: re-apply it, delete
    nothing;
  - there is no slug: create.

  So a run whose destroy failed (the row is kept) or whose create failed (the
  old chief is gone) is finished by running the same command again.

Under a namespace per instance, the new chief reuses the old one's namespace
name. The Kubernetes driver waits for the old namespace to finish terminating
before it creates the new one, bounded by the operation's timeout.

### Runbook

**Before.**

1. The server runs a release that has this route. `muster chief replace --help`
   answers, and an unconfirmed run does not exit 7.
2. `MUSTER_AGENT_KINDS` includes the target kind. For `claude-code`,
   `MUSTER_AGENT_CC_ACCOUNTS` (or your own credentials) names the account, and
   `MUSTER_AGENT_CONNECT_URL` is set.
3. `muster agent ls`: note the current chief's id and kind. No other agent is
   labelled `chief`.
4. Run the command **without** `--confirm` and read the plan. It names the id it
   would delete.

**Swap.**

```sh
muster chief replace --kind claude-code --cc-account <account> --confirm chief
```

The output reports `deleted` (the old row) and `agent` (the new one). On a
409, read the reason and retry after it clears. On a 502, re-run the same
command.

**After.**

1. `muster agent ls`: `chief` has a new id and kind `claude-code`. Its status
   reaches `running` once the first turn is delivered.
2. The pod is Ready: `kubectl -n <prefix>chief rollout status deploy/chief`.
3. The instructions are in place:
   `kubectl -n <prefix>chief exec deploy/chief -- head -5 /etc/claude-code/CLAUDE.md`
   shows `# muster supervisor`.
4. The MCP server is registered and serves the chief tools:
   - `kubectl -n <prefix>chief exec deploy/chief -- ccd seed --check --settings-template /etc/ccd/settings.json`
     should print `ok`.
   - `kubectl -n <prefix>chief exec deploy/chief -- sh -c 'curl -s -X POST "$MUSTER_CONNECT_URL/mcp" -H "X-Muster-Token: $MUSTER_HOOK_TOKEN" -H "Content-Type: application/json" -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}"'`
     should list `task_create_gated`, `reply_draft`, `attention_list` and the
     others above.
5. A plugin message reaches it. Post one with a plugin token
   (`POST /plugin/v1/messages`). It settles `delivered` with `agent_id` = the
   new chief, and the chief's default session shows the turn.
