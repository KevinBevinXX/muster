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
| Question | An agent called `attention_ask`, or muster raised one itself: a plugin's mention of an in-progress task when you have no chief ([task routing](plugins.md#routing-a-task-to-the-chief)), shown as asked by `muster`; nothing waits on its answer. | `attention_asks` (migration 0025; muster's own asker, 0028) | Tap one of the agent's options, or type an answer. **Dismiss without answering** withdraws it. |
| Session waiting | A mirrored Claude Code **or Hermes** window whose session reports `waiting` (a permission prompt, a question; for Hermes an approval, a clarify question or a sudo password). Shown with **what it waits on** (below), its focus title, its window @ host and its age. | The mirror hub's live window list; nothing is stored. | **Open the session** (the rendered view). A Hermes **approval** on a host with `-hermes-answer` also offers **Allow once** / **Deny**, answered through Hermes's gateway ([mirroring.md](mirroring.md)). It leaves the list when the session stops waiting. |
| Session question | A mirrored session's `AskUserQuestion`, registered by the `muster-mirror ask` hook and shown in the terminal and the rendered view at once. | The open question (`internal/askq`), live; it leaves the queue once answered in either place, dismissed, replaced by a newer question, or its session is gone. Such a window is this ONE item — its question, or, when it asks several, each one's first line, numbered — never also "Session waiting". | **Answer in the session**: the rendered view, where the question card is. |
| Task to route | A plugin handed a task to your chief ([task routing](plugins.md#routing-a-task-to-the-chief)) and the chief has not routed it: there is no chief, it is not running or busy, or it took the event and did not act. Shown as the task's number and title. | `plugin_messages` task events with no routing or start recorded for their task at or after the event's latest delivery attempt, or its creation if it has had none (audit rows), whose task is not complete or dismissed; nothing is copied. | **Open the task**. It leaves the list when the chief routes the task or starts an agent on it, or the task is complete or dismissed. No notification of its own: the task's "New task" push already went. |
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
| `attention_ask {question, options?, timeout_seconds?}` | Files a question. `options` (at most 10) become buttons; you can still answer in free text. Open for 24 hours by default (60 s to 7 days), then it expires. Returns `{id, status: "pending", next}`. A dispatched agent's answer is [delivered to it](#delivery) as a new message, so it need not wait; a mirrored session must call `attention_wait`. |
| `attention_wait {id, wait_seconds?}` | Blocks until the ask is answered (`status: "answered"`, `answer`), answered in the session (`status: "answered_in_session"`, `answer` — see [Answering](#answering)), cancelled or expired, or `wait_seconds` (default 240, at most 600) pass — then answers `status: "pending"` and the agent calls it again. `wait_seconds: 0` reads it without waiting. At most 2 waits per agent and 20 per owner are open at once; past that a wait is refused at once. |
| `attention_cancel {id}` | Withdraws an open ask. |
| `tasks_list {status?}`, `task_get {id}`, `task_comment {id, body}`, `task_set_status {id, status}` | The agent's OWN task: the one it was dispatched for. The chief's: the owner's whole board. Any other task answers exactly what a task that does not exist answers. An agent may set `open`, `in_progress` or `ready_for_review` — never `complete`, and never on a task that is already complete (checked in the update itself). The same rule holds on `PATCH /api/tasks/{id}/status` when the caller is an agent (the chief asserting its name through the service credential); the operator's tokens are unchanged. |

An ask belongs to the agent that raised it: another agent (even the same
owner's) reads "no such ask" for it.

**The chief only** (refused to any other agent, on both transports, even when
called by name):

| tool | does |
|---|---|
| `attention_list` | The whole queue, as the view shows it, newest first: each item's `text` (the question; what a session waits on; a draft's inbound message), `detail` (a session's last words; the draft) and `focus`. Read-only: the chief cannot answer an ask (no dispatched agent can); only the operator does — on `/attention`, or through one of their own mirrored sessions' `attention_answer` (below). |
| `session_transcript {subject, max_entries?}` | The recent conversation of one of the owner's sessions (`agent:<id>` or `mirror:<claude session id>`, as `fleet_list` returns them): oldest first, at most 200 entries, each at most 2000 characters, 48 KiB in all; tool calls by name, tool output left out. |
| `fleet_list` | (Existing.) Now also carries a mirrored session's `host`. |

**Mirrored Claude Code sessions only** (the operator's own sessions, through
their host's `muster-mirror` — [below](#mirrored-claude-code-sessions)):

| tool | does |
|---|---|
| `attention_answer {id \| task_id, answer}` | Answers one of the owner's OPEN asks as the operator: free text or one of its options, the same validation as the `/attention` form (blank refused, at most 8000 characters). The ask is named by **exactly one** of `id` (the ask's id) or `task_id` (the task it is about — the chief's *"Task #776 needs you"* is `{task_id: 776}`); `task_id` must have exactly one open ask of yours (*"task N has no open ask"* — also what another owner's task reads — or *"task N has K open asks (ids …): answer one by id"*). It takes the form's own answer path, so the notification is withdrawn, waiters wake and an agent's answer is [delivered](#delivery) exactly as a browser answer is. The row records where it came from (`answered_via = 'mcp'`, the calling host and the Claude session it named, migration 0034); `answered_by` is still `operator`. **It counts as your own words for `task_dispatch` instructions**, exactly as a browser answer does (operator decision; see the risk [below](#mirrored-claude-code-sessions)); the started agent's first message and the `task.chief_dispatch` audit row then say it came via MCP from `<host>`/`<session>` ([chief.md](chief.md)). Refused: an open ask raised by **any mirrored session on the calling host** (*"was raised by a mirrored session on this host"*), an ask that is no longer open (*"is no longer open (it is …)"* — also for a same-host ask once it has closed), and another owner's ask or a missing id (both *"no such ask: N"*). |
| `agent_destroy {name \| id}` | Destroys one of the owner's agents through the Agents page's own teardown ([provisioner.md](provisioner.md#destroying-an-agent)), named by **exactly one** of `name` (its slug) or `id`. The pod and the row go in the background; its open asks go with the row; a running turn is cut off. **The task is kept**: once the agent is gone, a task still `in_progress` with no other live agent is back to `open`, so the chief can `task_dispatch` it — and if the chief started the destroyed agent, the chief may start **one** new agent on the task again (see [chief.md](chief.md); the browser's Delete releases it too). **No audit row** is written: where the call came from (the host the token proves, the session the caller named) is in muster's server log only. Find a name with `muster agent ls`. Refused: the chief (*"the chief agent cannot be deleted…; to replace the chief use `muster chief replace`"*), and another owner's agent, an unknown name or a missing id (all *"no such agent: X"*). Operator decision ("Build agent_destroy MCP": mirrored owner sessions only). |
| `route_reply {route_id, body}` | With [session routing](plugins.md#routing-a-message-to-a-session) on: answers a plugin message the chief routed to **this** session, through the same path as `muster route reply` (screened, relayed once — sent at once to a verified, allowlisted sender, otherwise a draft the owner approves). Refused: a route sent to another session (*"route X was not routed to this session…"* — whatever the caller holds, including the route id), a route no longer waiting (*"route X is no longer waiting for an answer (it is …)"* — answered, timed out back to the chief or failed; the same body again is refused too), and another owner's route or a missing id (both *"no such route: X"*). Not offered, and refused, to every dispatched agent, the chief included. |

🔴 **Who can answer, and how that is enforced.** `attention_answer` is listed
only on `POST /mirror/mcp`; no dispatched agent's tool list (gateway or `POST
/mcp`) has it, and `POST /mcp` refuses an unlisted name. Because a gateway
runtime can name any tool, the handler itself also refuses every caller that is
not a mirrored session — the chief included: *"attention_answer is available
only to your owner's own mirrored Claude Code sessions"*. `POST /mirror/mcp`
admits only a mirror host token (the hook token, an agent's token and the
chief's are 401), acts as that token's owner (row-level security), and binds
each call to a Claude session **that host** lists now.

🔴 **The host is the principal, not the session.** The token proves the host;
the session is the tmux pane the caller names (`X-Muster-Pane`, which the
caller sets — and the host's MCP secret is readable by every session of its
user), so one session can claim a sibling's pane. So an answer is refused for
an ask raised by **any** mirrored session on the calling host, not only the
asking one; an ask raised on another host, or by an agent, the chief or
muster, is answerable. The rule is part of the answer's own conditional
update, and 0034's CHECK states it again. The recorded session is the one the
caller named: provenance of the host is proven, of the session it is a claim.

A long `attention_wait` keeps its connection alive: after 15 seconds the MCP
response's headers are sent and a newline follows every 15 seconds until the
answer (leading whitespace is valid JSON). Claude Code abandons an HTTP MCP call
that has answered nothing for `CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT` (5 minutes by
default); whether those newlines reset it is not documented, which is why the
default wait is 240 seconds.

### What agents are told

A dispatched agent's gateway system prompt and MCP `initialize` instructions
carry this paragraph (`attentionInstruction`):

> If you need your owner's input — a decision, a missing fact, an approval —
> call attention_ask{question, options?}. Your owner sees the question in
> muster's Attention view and on their phone. When they answer, muster delivers
> the answer to you as a new message in your default session, so you do NOT
> need to wait for it: you may end your turn. If you cannot continue without
> it, you MAY instead block on it with attention_wait{id} (repeat it while it
> answers "pending"); an answer you read there is not delivered a second time.

A mirrored session's (`mirrorAttentionInstruction`) still says to call
`attention_wait` until it returns the answer: its answer is never pushed.

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
   `attention_ask`, `attention_wait`, `attention_cancel`, the task tools,
   `attention_answer` (the operator answering an ask through this session),
   `agent_destroy` (the operator tearing down one of their agents) and,
   with a connector store, `connector_requests_list` / `connector_request_decide`
   (the operator deciding a connector request through this session —
   [connectors.md](connectors.md#deciding-a-request-from-your-own-session)); the
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

An answer given with `attention_answer` from one of these sessions is the
operator's answer (see [The agent tools](#the-agent-tools)): the session can
answer any of its owner's open asks except one raised by a mirrored session on
its own host.

**What a leaked mirror token reaches** (see [auth.md](auth.md)): beyond the
host's tunnel, the asks of that host's own Claude Code sessions (file, wait,
cancel), the tasks those sessions created — and, through `attention_answer`,
**an answer as the operator to any of the owner's open asks** not raised by a
mirrored session on that host, including the chief's. Such an answer is
recorded and shown as coming via MCP from that host, and it is accepted as
the operator's answer everywhere — **including as `task_dispatch`
`instructions`**, so a leaked token can write a dispatched agent's prompt —
and, through `connector_request_decide`, **an approval as the operator of any
of the owner's agents' pending connector requests** (recorded and shown as via
MCP from that host; the accepted risk is in
[connectors.md](connectors.md#deciding-a-request-from-your-own-session)) — and,
through `agent_destroy`, it can **destroy any of the owner's agents but the
chief** — with no audit row: the only record of which host did it is muster's
server log.

🔴 **Accepted risk: a mirror token can destroy agents.** The operator chose it
("Build agent_destroy MCP", *"Mirrored owner sessions only, same pattern"*) so
a stuck agent can be torn down without a browser. Concretely: a leaked token,
or a mirrored session fed outside text, can tear down every non-chief agent of
the owner (their running work and open asks go with them). The tasks survive
and the chief is refused, but there is **no per-destroy audit trail** (operator
decision): which host did it is in muster's server log only, and nothing
restores a destroyed agent. A destroy of an agent the chief started also frees
the chief to start one new agent on that task. Revoke a token you suspect (below).

🔴 **Accepted risk: an MCP answer can become a dispatched agent's
instructions.** The operator decided this explicitly after being told the risk
("Just remove the filter": *"MCP answers count as instructions again. Simplest;
accepts that a session fed outside text could write a dispatched agent's
prompt."*). Concretely: a mirrored Claude Code session that has been fed
outside text — for example a plugin message the chief routed to it with
`route_to_session` — could call `attention_answer` with injected text on a
task's ask, and the chief may then pass that text, verbatim, as a dispatched
agent's `instructions`; a leaked mirror token can do the same directly. What
muster does about it is say so: the agent's first message states the
instructions were your answer given over MCP from `<host>`/`<session>`, not on
the Attention view, and the `task.chief_dispatch` audit row records
`instructions_via: "mcp"` and `instructions_from: "<host>/<session>"` (the
session is the one the caller named; only the host is proven).
Never another task, another host's session, another owner's ask, or a chief
tool. Revoke a token you suspect (`muster-server mirror token revoke`).

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

## Delivery

When you answer an ask — on `/attention`, or through a mirrored session's
`attention_answer` — the answer goes to whoever asked (migration 0031,
`internal/api/attention_delivery.go`):

| asker | what happens |
|---|---|
| a dispatched agent (the chief included) | muster pushes the answer into the agent's **default** chat session (the chief's pinned one) as a new turn: muster's line naming the ask by id (no question text outside the fence), then the question and your answer, fenced (`<<<attention-answer untrusted …>>>`). The agent acts on it without polling. An answer given through `attention_answer` says so in muster's line: *"— answered by the operator via MCP from `<host>`/`<claude session id>`, their mirrored Claude Code session, not on the Attention view"* (the host and id are re-checked against the mirror wire's shapes before they are written outside the fence). |
| a mirrored Claude Code session | nothing is pushed: muster types into a mirrored window only for your own message (the send queue's step-up gate). The session reads the answer with `attention_wait`; an answer given through `attention_answer` carries `answered_via: "mcp"` and `answered_from: "<host>/<session>"` there. |
| muster itself (a system ask) | nothing to deliver. |

**Exactly once.** The delivery is a state on the ask's row, and every move is
one conditional update: queued → delivering (one claimer, with a lease) →
delivered, or unconfirmed. An agent blocked in `attention_wait` on the ask gets
the answer THERE instead — a wait in flight is never pushed over, the push only
takes an answer a few seconds old, and a wait that returns the answer marks it
read — so it never gets the answer twice. A restart re-reads the owed answers
on the attention sweep (every 30 s) and delivers each once.

**The turn's locks are a delivered plugin message's.** It holds the agent's
session claim (so it never overlaps a plugin delivery, a first turn or a route
hand-back) and waits for the agent to be idle in this process (an operator's
chat turn). An agent that is busy, not running or not yet started keeps the
answer queued, saying why, and the sweep retries it.

**Unconfirmed is never resent on its own.** A turn that failed after the text
may have reached the agent, or whose process died mid-turn (its lease lapsed),
settles *unconfirmed* — the plugin deliverer's rule. So does an answer still
queued a day after it was given or retried (its agent stopped, never started,
or stayed busy), saying why; unconfirmed answers stay listed until retried. A turn the runtime provably
did not take (busy, not ready) stays queued.

**Where it went.** Below the open items, `/attention` lists the last day's
answers and where each went: *Queued for …* (with why, while it waits),
*Delivering…*, *Delivered to …*, *… read it while waiting for it*, *Waiting for …
to read it* (a mirrored session), or *Delivery failed: …* with **Retry delivery**
(`POST /attention/asks/{id}/redeliver`, your own asks only), which queues it
once more. An answer given through `attention_answer` also reads *Given via MCP
from your mirrored session `<host>`/`<session>`*.

**No backfill.** An answer recorded before 0031 has no delivery state and is
never pushed.

## Answering

Only the operator answers: on `/attention` — `POST /attention/asks/{id}/answer`
(`option` or `answer`) and `POST /attention/asks/{id}/cancel`,
session-authenticated and same-origin — or through one of their own mirrored
Claude Code sessions' `attention_answer` ([who can, and how](#the-agent-tools)).
No dispatched agent's tool answers an ask, the chief's included.

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
