# The attention queue

One place showing everything that is blocked on you, and a way for any agent
to ask you something and wait for the answer instead of stopping.

## What is in it

`/attention` (the **Attention** link in the menu, with a live count) lists,
**newest first** across every kind — by when each item started waiting (an
ask's or a draft's creation, a session's own status time, a held question's
creation), ties broken by the item's key:

| kind | what | where it comes from | how you act on it |
|---|---|---|---|
| Question | An agent called `attention_ask`. | `attention_asks` (migration 0025) | Tap one of the agent's options, or type an answer. **Dismiss without answering** withdraws it. |
| Session waiting | A mirrored Claude Code window whose session reports `waiting` (a permission prompt, a question). Shown with **what it waits on** (below), its focus title, its window @ host and its age. | The mirror hub's live window list; nothing is stored. | **Open the session** (the rendered view). It leaves the list when the session stops waiting. |
| Session question | A mirrored session's `AskUserQuestion`, registered by the `muster-mirror ask` hook and shown in the terminal and the rendered view at once. | The open question (`internal/askq`), live; it leaves the queue once answered in either place, dismissed, replaced by a newer question, or its session is gone. Such a window is this ONE item — its question, or, when it asks several, each one's first line, numbered — never also "Session waiting". | **Answer in the session**: the rendered view, where the question card is. |
| Task to route | A plugin handed a task to your chief ([task routing](plugins.md#routing-a-task-to-the-chief)) and the chief has not routed it: there is no chief, it is not running or busy, or it took the event and did not act. Shown as the task's number and title. | `plugin_messages` task events with no routing or start recorded for their task since (audit rows), whose task is not complete or dismissed; nothing is copied. | **Open the task**. It leaves the list when the chief routes the task or starts an agent on it, or the task is complete or dismissed. No notification of its own: the task's "New task" push already went. |
| Reply draft | Your chief drafted a reply to a plugin message. Shown as the message's sender and its first 160 characters, and the draft reply's first 160. | `plugin_reply_drafts`, pending ones; nothing is copied. | **Review the draft** on `/plugins`. |

**What a waiting session waits on** is read by its host's `muster-mirror`
from the session's own transcript (muster holds none): the last tool call since
the last prompt that has no result yet, as one line — `Bash: <the command's
first line>`, `Edit <path>` / `Write <path>` / `Read <path>` (a long path keeps
its end), `WebFetch <the URL's host>`, `MCP <server>/<tool>`, an
`AskUserQuestion`'s first question, else the tool's name — the tool's name on
its own, and the last thing Claude said. Each is one line with control and
format characters (bidi controls, zero-width characters) removed, at most 120,
40 and 200 characters; nothing more of the tool's input is sent. **Before it
leaves the host, anything that looks like a secret is replaced by
`«redacted»`:** Authorization / Bearer / Basic credentials, the values of
`-p`, `--password`, `--token`-style flags (and a `-u user:password`),
assignments to `*_KEY`, `*_TOKEN`, `*_SECRET`, `PASSWORD`, `PGPASSWORD` and the
like, a URL's userinfo and its `token`/`key`/`secret`/`sig`/`password` query
parameters, and tokens with well-known prefixes (`ghp_`, `github_pat_`, `sk-`,
`xoxb-`, `AKIA…`, `glpat-`, `mst_`, `mmt_`, `mpl_`). It is a heuristic: a
secret in a shape it does not know passes, which is one reason no notification
carries any of this. It rides the host's
window report (`mirror.Window.Pending`) for at most 8 waiting windows per host
(the most recently waiting). An older `muster-mirror` sends none, and the item
then reads "Waiting on you: a permission prompt or a question in the session."

Only a host whose `muster-mirror` reports Claude Code's status counts toward
"Session waiting": an online one, or one that disconnected **less than 5
minutes ago** — its windows are kept as last seen, marked *host offline*, so a
blip neither drops them nor clears their notification. Past that they leave
the list and their notification is cleared. The list and the notifications
follow this one rule.

The list updates live (the `attention_queue.changed` event on `/events`). It
does not refresh while you are typing an answer; it catches up when you leave
the field.

**Notifications** (event `attention` on Account → Notifications, on by default):
one per new question or newly waiting session, withdrawn when the item is
answered (on `/attention` or in the session), dismissed, expires or stops
waiting. **A notification says WHO and WHERE, never WHAT** — a lock screen shows
it to anyone holding the phone: `<asker> is asking` for a question, `Waiting:
<tool name> · <window> @ <host>` for a session (no command, path or URL). The
question, the command and the message are on the page. Each item has one tag, so a
notification is replaced, never stacked. A session is not re-notified:

- when muster restarts or a host reconnects: the first time this process sees
  a host — and again each time it comes back after disconnecting — its
  sessions that are already waiting are recorded silently. While a host is
  disconnected for less than 5 minutes its items stay (no clear);
- when it goes waiting again within 2 minutes of its last notification or of
  being recorded (busy and back is one episode).

A reply draft keeps its own `plugin` notification — `Reply draft: <sender's
name> (<plugin>)`, the name only when the plugin verified the sender
(`unverified sender` otherwise), never a word of the message or the draft —
which is withdrawn when you
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
| `attention_wait {id, wait_seconds?}` | Blocks until the ask is answered (`status: "answered"`, `answer`), answered in the session (`status: "answered_in_session"`, `answer` — see [Answering](#answering)), cancelled or expired, or `wait_seconds` (default 240, at most 600) pass — then answers `status: "pending"` and the agent calls it again. `wait_seconds: 0` reads it without waiting. At most 2 waits per agent and 20 per owner are open at once; past that a wait is refused at once. |
| `attention_cancel {id}` | Withdraws an open ask. |
| `tasks_list {status?}`, `task_get {id}`, `task_comment {id, body}`, `task_set_status {id, status}` | The agent's OWN task: the one it was dispatched for. The chief's: the owner's whole board. Any other task answers exactly what a task that does not exist answers. An agent may set `open`, `in_progress` or `ready_for_review` — never `complete`, and never on a task that is already complete (checked in the update itself). The same rule holds on `PATCH /api/tasks/{id}/status` when the caller is an agent (the chief asserting its name through the service credential); the operator's tokens are unchanged. |

An ask belongs to the agent that raised it: another agent (even the same
owner's) reads "no such ask" for it.

**The chief only** (refused to any other agent, on both transports, even when
called by name):

| tool | does |
|---|---|
| `attention_list` | The whole queue, as the view shows it, newest first: each item's `text` (the question; what a session waits on; a draft's inbound message), `detail` (a session's last words; the draft) and `focus`. Read-only: only the operator answers an ask. |
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

## Mirrored Claude Code sessions

A Claude Code session in a mirrored tmux window gets the **every agent** tools
too, through its host's `muster-mirror`:

1. `muster-mirror -mcp-listen 127.0.0.1:47823` (or `MUSTER_MIRROR_MCP_LISTEN`)
   serves MCP at `http://127.0.0.1:47823/mcp`. Only a literal loopback address
   is accepted. Every call must carry the host's local secret
   (`Authorization: Bearer <secret>`), kept in `-mcp-secret-file` (default
   `$XDG_CONFIG_HOME/muster-mirror/mcp-secret`, created 0600 on first start —
   written to a temp file and linked into place, so nobody reads a partial
   one), so another user on the machine cannot use it. A request with an
   `Origin` header is refused. A symlink at the secret's path is refused. The
   endpoint is optional and set up beside mirroring, never before it: if it
   cannot start (the port is taken, the secret file is a symlink or readable by
   others), the host logs `the MCP endpoint is OFF … <reason>` — in its log
   only — and mirrors without it.
2. The session names itself with its tmux pane and tmux server:
   `X-Muster-Pane: ${TMUX_PANE:-}` and `X-Muster-Tmux: ${TMUX:-}`. The host
   refuses any request from a server that is not the one it mirrors (pane ids
   are per server).
3. **The session is bound at each tool CALL, not when Claude Code connects.**
   Claude Code connects as it starts, before its new session has written its
   first message — so before anything can say which session runs in the pane —
   and it does not retry a connection that failed. So `initialize`,
   `tools/list` and notifications are forwarded with no session. For a
   `tools/call` the host reads its panes afresh and maps the pane to the Claude
   session running in it now — any pane of a window, not only its active one,
   up to 4 other panes per window and 32 per host (what the host reports to
   muster) — then sends muster its window list at once, and forwards the call.
   A call it cannot bind (no Claude session in that pane yet, a pane past the
   bound) is answered as a **tool error** naming the pane as received, which
   the model reads; the connection stays up.
4. The host forwards the call to muster's `POST /mirror/mcp` with its mirror
   token and that Claude session id. muster acts as the token's owner, and runs
   a call only if **that host** lists a window running **that session** (in
   any pane) — waiting up to 3 seconds for the host's list to arrive. Another
   host's session, a session no window shows, or another owner's token gets a
   tool error and the tool does not run. The tool set is exactly
   `attention_ask`, `attention_wait`, `attention_cancel` and the task tools; the
   chief's tools are not on it. The token is checked on every call (a revoked
   one is refused at its next call, `initialize` included). After 10 failed checks of one token from
   one address within a minute, that token from that address is answered 429 —
   without a token lookup — until the minute has passed. Another token from the
   same address (hosts behind one NAT or Cloudflare edge share one) is checked
   as usual, so a host with a stale token never locks out another host.

An ask made this way is shown under the window's title and host, and only that
session can wait on or cancel it. The task tools act on **the tasks that
session created** (its `created` link, which `muster task create` records from
the session's id) and nothing else.

**What a leaked mirror token reaches** (see [auth.md](auth.md)): beyond the
host's tunnel, the asks of that host's own Claude Code sessions (file, wait,
cancel) and the tasks those sessions created. Never another task, another
host's session, or a chief tool.

### Client configuration

In the Claude Code MCP configuration of the user who runs the sessions (for
example `~/.claude.json`'s `mcpServers`, or a project's `.mcp.json`):

```json
{
  "mcpServers": {
    "muster": {
      "type": "http",
      "url": "http://127.0.0.1:47823/mcp",
      "headers": {
        "Authorization": "Bearer ${MUSTER_MIRROR_MCP_SECRET:-}",
        "X-Muster-Pane": "${TMUX_PANE:-}",
        "X-Muster-Tmux": "${TMUX:-}"
      }
    }
  }
}
```

with `MUSTER_MIRROR_MCP_SECRET` exported from the secret file in the shell that
starts Claude Code (`export MUSTER_MIRROR_MCP_SECRET="$(cat ~/.config/muster-mirror/mcp-secret)"`).

The `:-` defaults matter: Claude Code expands `${VAR}` and `${VAR:-default}` in
an HTTP server's `url` and `headers`, and a `${VAR}` with no value is a
configuration error ("Missing environment variables") that disables the
server — so a Claude Code started outside tmux, or without the secret, would
otherwise lose the server instead of being refused by it
([managed MCP](https://code.claude.com/docs/en/managed-mcp), [plugin
troubleshooting](https://code.claude.com/docs/en/plugins/troubleshooting),
[plugin reference](https://code.claude.com/docs/en/plugins/manifest-reference)).

No `CLAUDE.md` paragraph is needed: the endpoint's `initialize` answer carries
the ask-and-wait instructions, and Claude Code hands an MCP server's
instructions to the model ("server instructions … surfaced to the model as an
MCP instructions block", [Agent SDK
reference](https://code.claude.com/docs/en/agent-sdk/typescript); "only tool
names and server instructions load at startup",
[glossary](https://code.claude.com/docs/en/glossary)).

If muster sits behind an auth proxy, `POST /mirror/mcp` must bypass it, as
`GET /mirror/connect` does.

## Answering

Only the operator answers, on `/attention`: `POST /attention/asks/{id}/answer`
(`option` or `answer`) and `POST /attention/asks/{id}/cancel`,
session-authenticated and same-origin. No agent tool answers an ask.

**Or in the session.** Writing to the asker directly answers it too: a message
you send into a mirrored session's Claude Code pane (the rendered view's Send —
not the Terminal's, which types into whatever pane is active, possibly a
shell) or queue for it (the send queue — at the moment you queue it, since a
session blocked in `attention_wait` is busy and a queued message waits for
idle; the queued message then says *Already answered …'s question*, and
cancelling it warns that the answer was delivered and can't be un-sent), or a
message to a dispatched agent's chat (its page or its Terminal's composer). Every ask that asker raised before your message and that
is still open becomes `answered_in_session` (migration 0026): it leaves the
queue, its notification is cleared, and the asker's `attention_wait` returns
`{status: "answered_in_session", answer: <your message>, note}` (capped at 8000
characters), where `note` — and the tool's description — tells the agent this
is your next message, not necessarily an answer and not consent, and to ask
again if it does not clearly address the question. The message also reaches the session or agent as an ordinary
message. Only your own messages count: a message routed by the chief, the
chief's hand-back turn, a message over the machine door
(`POST /api/agents/{name}/messages`) and anything an agent sends do not. A
reply draft decided outside muster is not cleared by this (it is not an ask).
