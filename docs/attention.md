# The attention queue

One place showing everything that is blocked on you, and a way for any agent
to ask you something and wait for the answer instead of stopping.

## What is in it

`/attention` (the **Attention** link in the menu, with a live count) lists, oldest
first:

| kind | what | where it comes from | how you act on it |
|---|---|---|---|
| Question | An agent called `attention_ask`. | `attention_asks` (migration 0025) | Tap one of the agent's options, or type an answer. **Dismiss without answering** withdraws it. |
| Session waiting | A mirrored Claude Code window whose session reports `waiting` (a permission prompt, a question). | The mirror hub's live window list; nothing is stored. | **Open the session** (the rendered view). It leaves the list when the session stops waiting. |
| Session question | A mirrored session's `AskUserQuestion` held by the `muster-mirror ask` hook. | The held question (`internal/askq`), live. Such a window is this ONE item — its first question, and how many more — never also "Session waiting". | **Answer in the session**: the rendered view, where the question card is. |
| Reply draft | Your chief drafted a reply to a plugin message. | `plugin_reply_drafts`, pending ones; nothing is copied. | **Review the draft** on `/plugins`. |

Only a host whose `muster-mirror` reports Claude Code's status (and is online)
counts toward "Session waiting".

The list updates live (the `attention_queue.changed` event on `/events`). It
does not refresh while you are typing an answer; it catches up when you leave
the field.

**Notifications** (event `attention` on Account → Notifications, on by default):
one per new question or newly waiting session, withdrawn when the item is
answered, dismissed, expires or stops waiting. Each item has one tag, so a
notification is replaced, never stacked. A session is not re-notified:

- when muster restarts (or a host reconnects): the first time this process
  sees a host, its sessions that are already waiting are recorded silently;
- when it goes waiting again within 2 minutes of its last notification (busy
  and back is one episode).

A reply draft keeps its own `plugin` notification, which is withdrawn when you
decide the draft, when a newer draft for the same message supersedes it, and
when it expires (checked every 30 seconds, including drafts notified before a
restart).

## The agent tools

Served on both transports — a gateway agent's native tools and `POST /mcp` for a
claude-code agent — whenever muster has a database.

**Every agent:**

| tool | does |
|---|---|
| `attention_ask {question, options?, timeout_seconds?}` | Files a question. `options` (at most 10) become buttons; you can still answer in free text. Open for 24 hours by default (60 s to 7 days), then it expires. Returns `{id, status: "pending"}`. |
| `attention_wait {id, wait_seconds?}` | Blocks until the ask is answered, cancelled or expired, or `wait_seconds` (default 240, at most 600) pass — then answers `status: "pending"` and the agent calls it again. `wait_seconds: 0` reads it without waiting. At most 2 waits per agent and 20 per owner are open at once; past that a wait is refused at once. |
| `attention_cancel {id}` | Withdraws an open ask. |
| `tasks_list {status?}`, `task_get {id}`, `task_comment {id, body}`, `task_set_status {id, status}` | The agent's OWN tasks: the one it was dispatched for and the ones it created (no agent tool creates a task today, so a worker's is its dispatched task). The chief's: the owner's whole board. Any other task answers exactly what a task that does not exist answers. An agent may set `open`, `in_progress` or `ready_for_review` — never `complete`, and never on a task that is already complete. |

An ask belongs to the agent that raised it: another agent (even the same
owner's) reads "no such ask" for it.

**The chief only** (refused to any other agent, on both transports, even when
called by name):

| tool | does |
|---|---|
| `attention_list` | The whole queue, as the view shows it. Read-only: only the operator answers an ask. |
| `session_transcript {subject, max_entries?}` | The recent conversation of one of the owner's sessions (`agent:<id>` or `mirror:<claude session id>`, as `agents_list` returns them): oldest first, at most 200 entries, each at most 2000 characters, 48 KiB in all; tool calls by name, tool output left out. |
| `agents_list` | (Existing.) Now also carries a mirrored session's `host`. |

A long `attention_wait` keeps its connection alive: after 15 seconds the MCP
response's headers are sent and a newline follows every 15 seconds until the
answer (leading whitespace is valid JSON). Claude Code abandons an HTTP MCP call
that has answered nothing for `CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT` (5 minutes by
default); whether those newlines reset it is not documented, which is why the
default wait is 240 seconds.

### What agents are told

The gateway system prompt and the MCP `initialize` instructions both carry this
paragraph:

> If you need your owner's input — a decision, a missing fact, an approval — do
> NOT stop and wait for a reply in the chat: call attention_ask{question,
> options?} and then attention_wait{id} (repeat it while it answers "pending")
> until it returns the answer, then continue. Your owner sees the question in
> muster's Attention view and on their phone.

## Answering

Only the operator answers, on `/attention`: `POST /attention/asks/{id}/answer`
(`option` or `answer`) and `POST /attention/asks/{id}/cancel`,
session-authenticated and same-origin. No agent tool answers an ask.
