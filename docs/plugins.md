# Plugins

A **plugin** is a separate process that connects muster to another system, such
as an inbox. It brings that system's messages to your **chief** agent, and it
sends the replies you approve back. muster never calls a plugin. The plugin
calls muster, for everything (see [decisions](decisions.md#plugins-pull-muster-never-calls-out)).

Plugins are **off** unless the server runs with `MUSTER_PLUGINS=1` and a
database. While they are off, every `/plugin/*` route and the `/plugins` page
answer 404.

The whole loop:

1. The plugin posts an inbound message (`POST /plugin/v1/messages`).
2. muster hands it to your chief as a fenced turn.
3. The chief may file a gated task (`task_create_gated`), draft a reply
   (`reply_draft`), [route it to a session](#routing-a-message-to-a-session)
   (`route_to_session`, when routing is on), or do nothing.
4. You approve or reject each draft on `/plugins`.
5. The plugin claims an approved draft (`POST /plugin/v1/drafts/claim`), sends
   it, and reports the result (`POST /plugin/v1/drafts/{id}/result`).

## Two ways in

| door | who | routes |
|---|---|---|
| The **page** | you, signed in | `/plugins`: create and revoke plugin tokens; approve or reject reply drafts |
| The **plugin** | an `mpl_` plugin token | `POST /plugin/v1/messages`, `POST /plugin/v1/drafts/claim`, `POST /plugin/v1/drafts/{id}/result`, `POST /plugin/v1/tasks` |

A plugin token is shown once, when you create it. muster stores only its
sha256. The token acts as you, and only on `/plugin/v1/*`:

- Every other route refuses an `mpl_` token, even beside a valid session.
- The plugin routes refuse every other credential: the shared hook and service
  secrets, personal `mst_` tokens, runner tokens, agent tokens and browser
  sessions.

Each plugin has a **name** (a slug), a **label** (what your chief's conversation
for it is called) and a **data scope**. The page lists each plugin with when it
was last seen (its last authenticated call) and how many of its messages are
still waiting to reach your chief.

## The plugin author's contract

Every route takes `Authorization: Bearer mpl_…` and a JSON body, and answers
JSON. On every route:

| answer | meaning |
|---|---|
| `401` | not a live plugin token (unknown, revoked, malformed, another kind of credential, or the owner's account is disabled) |
| `404` | plugins are off on this server |

### `POST /plugin/v1/messages`: an inbound message

```json
{"external_id": "msg-0001", "sender": "someone@example.invalid",
 "subject": "hello", "body": "…", "received_at": "2000-01-02T03:04:05Z",
 "no_reply": false, "sender_verified": false}
```

| field | required | rule |
|---|---|---|
| `external_id` | yes | 1 to 200 bytes, no spaces or control characters. The idempotency key, unique per plugin. |
| `body` | yes | 1 byte to 16 KiB |
| `sender`, `subject` | no | 200 characters each |
| `received_at` | no | RFC 3339 |
| `no_reply` | no | `true` marks a message nobody may answer (see below). Default `false`. |
| `sender_verified` | no | `true` asserts that YOU authenticated `sender` (see below). Default `false`. |

| answer | meaning |
|---|---|
| `202 {"id", "status": "queued"}` | stored; delivery to your chief has started |
| `200 {"id", "status"}` | this plugin already sent this `external_id`. The stored message is the answer, and nothing is stored or delivered again, **whatever the new body or `no_reply` says**. `status` is `queued`, `delivered` or `unconfirmed`. |
| `400 {"error"}` | a field breaks a rule above |
| `429 {"error"}` | 60 new messages in the last hour, or 200 not yet delivered. Retry after the `Retry-After` header. Task events ([routing a task](#routing-a-task-to-the-chief)) and the mentions muster routes have their own allowance — 120 an hour, and 300 task events undelivered (mentions count toward the hour only) — so they never count here, nor these toward them. |

A plugin that did not see an answer should send the same message again. A
repeat is answered even while the plugin is rate-limited. Advance your upstream
cursor only after a `2xx`.

Limits are refusals, never truncations.

#### `no_reply`

Set `no_reply: true` on a message nobody can answer, such as a notification
from an address that does not take replies. Then:

- the chief's turn says that no reply is possible;
- `reply_draft` refuses that message;
- the database refuses any reply draft for it, whatever writes it;
- the mark cannot be changed once the message is stored.

#### `sender_verified`

Set `sender_verified: true` only when your plugin has **authenticated** the
sender: the address in `sender` is proven to be the one that sent the message,
not merely what its From line says. It is what lets an answer to that sender be
relayed without the owner's approval (`MUSTER_ROUTE_RELAY_ALLOW`, below); a
message without it always waits for approval. Before setting it, check:

- **mail**: the message passed DMARC, or DKIM with a signing domain aligned to
  the From domain (SPF alone authenticates the envelope, not From), and you are
  reporting that From address;
- **a signed protocol**: the message's signature verified against keys the
  sender's owner published, and `sender` is the identity those keys belong to;
- **anything else**: an equivalent proof bound to the exact address you report.

When in doubt, leave it `false`. muster cannot check your claim: an entry on
the allowlist names your plugin, so only messages **your** plugin marked
verified match it, and the mark cannot be changed once the message is stored.

### `POST /plugin/v1/drafts/claim`: collect an approved reply

```json
{"wait_seconds": 25}
```

`wait_seconds` is 0 to 25; a larger value is treated as 25, and an empty body as
0. The call waits up to that long for a reply of **this plugin's** that you have
approved.

| answer | meaning |
|---|---|
| `200` | one approved reply, now **leased to you for 2 minutes** (below) |
| `204` (no body) | nothing to send within the wait; call again |
| `400 {"error"}` | the body is not `{"wait_seconds": n}` |

```json
{"draft_id": 17, "message_external_id": "msg-0001",
 "body": "Thanks, noon works.", "approved_at": "2000-01-02T03:04:05Z"}
```

`message_external_id` is the `external_id` you posted the message with: the
reply answers that message. `body` is exactly the text your owner approved.

**Each claim also retries delivery.** At the start of every claim, muster tries
again to hand your owner's queued messages to the chief, oldest first, stopping
at the first one the chief cannot take yet. There is no other retry, so a plugin
that long-polls keeps delivery moving. The retry runs in the background: a
delivery is a full chat turn and can take minutes, so the claim answers without
waiting for it, and a message already being delivered is not handed over twice.

### `POST /plugin/v1/drafts/{id}/result`: report what happened

```json
{"ok": true, "external_ref": "upstream-id-123", "detail": ""}
```

| field | required | rule |
|---|---|---|
| `ok` | yes | `true`: sent. `false`: it was not sent. |
| `external_ref` | no | up to 200 characters: the upstream's id for what you sent |
| `detail` | no | up to 4 KiB: anything your owner should read, such as the upstream's error |

| answer | meaning |
|---|---|
| `200 {"draft_id", "status"}` | recorded: `sent` or `failed`. The same result again is also `200` and changes nothing. |
| `400 {"error"}` | `ok` is missing, or a field breaks a rule |
| `404 {"error"}` | no such reply claimed by this plugin (another plugin's, another owner's, or none) |
| `409 {"error", "draft_id", "status"}` | the reply already has the **other** result, or it was never claimed |

### At most once

muster hands an approved reply to a claim **once**. Two concurrent claims never
receive the same reply.

If no result arrives within the 2-minute lease, the reply becomes
**unconfirmed**. muster does not know whether you sent it, so it **never offers
it again**, and your owner cannot approve it again either. If the sender still
needs an answer, the chief can draft a new reply to the message; its card warns
your owner that the earlier one may already have gone.

A result that arrives after the lease lapsed is still recorded, on the draft it
was claimed as. A `"ok": true` (late or not) also **supersedes every other reply
to the same message** that is still pending or approved and not yet claimed:
the sender has an answer, so no second one goes. A reply some claim already
holds is left to that claim's own result. `"ok": false` supersedes nothing.

Only one reply to a message is in flight at a time: while one is approved and
either unclaimed or within its lease, no other draft for that message can be
filed or approved.

Set your HTTP client's timeout to `wait_seconds` plus at least 10 seconds. A
claim whose answer you never received is still a claim: the reply becomes
unconfirmed when its lease lapses and is never offered again.

So a plugin should:

1. record the `draft_id` durably **before** sending upstream;
2. never send the same `draft_id` twice, even after a restart (muster never
   offers one twice either; a new reply always has a new `draft_id`);
3. post the result as soon as it knows, and post it again if it did not see an
   answer (a repeat is safe).

### `POST /plugin/v1/tasks`: one task per upstream record

Some upstreams send **notices about a record** rather than messages for your
chief to judge, such as "ticket 4711 was assigned to you". Each notice maps
to exactly one task, so the plugin files it here directly. Nothing reaches
the chief, unless the plugin asks for it with `route_to_chief` and the server
runs task routing ([below](#routing-a-task-to-the-chief)).

```json
{"key": "tracker:4711",
 "create": {"title": "Rotate the build key", "body": "…"},
 "comment": "status: open → in progress (upstream message 41)"}
```

| field | required | rule |
|---|---|---|
| `key` | yes | `<namespace>:<id>`, already a normalized tag, in one of the namespaces `MUSTER_PLUGIN_TASK_NAMESPACES` lists. It becomes the task's tag. |
| `create` | no | File a task if no **open** task of yours carries `key` (below). `body` must not be blank. |
| `comment` | no | Recorded on the task for `key`: the open one that already carries it, or the one `create` files. Surrounding whitespace is trimmed; a blank comment is no comment. |
| `route_to_chief` | no | `true` asks muster to route the notice ([below](#routing-a-task-to-the-chief)): to your chief, to the agent working the task, or to your attention queue, by `notice`. Needs a non-blank `comment`, which names the event, and a `notice` (a `400` without either, whether or not the server runs task routing). When muster routes it nowhere the task is still filed and the answer says `"routed": false`. |
| `notice` | with `route_to_chief` | What the notice is, by the plugin's reading: exactly `assigned`, `mentioned` or `update` (a comment or a status change). Anything else, or a `notice` without `route_to_chief`, is a `400`. |

Give `create`, a non-blank `comment`, or both. Unknown fields are refused.
Text over the limits is **cut, never refused**: `body` and `comment` at
16 KiB (on a character boundary), `title` at 512 characters.

**The open task** for a key is the oldest of yours that carries it and is not
dismissed, not `complete`, and not the loser of a merge (`superseded-by:`).
After a merge, the winner carries the loser's tags, so later notices go to
the winner.

| answer | meaning |
|---|---|
| `201 {"id", "created": true, "commented"}` | A task was filed, tagged with `key`, with the comment (if given) in the same step. |
| `200 {"id", "created": false, "commented": true\|false}` | An open task already carries `key`. `commented` says whether the comment was appended. |
| `200 {"id": null, "created": false, "commented": false}` | No open task carries `key` and `create` was not given. Nothing was written. |
| `400 {"error"}` | A field breaks a rule above. The same request will never be accepted. |
| `503 {"error"}` | This server lists no namespaces (`MUSTER_PLUGIN_TASK_NAMESPACES` is unset). Not permanent: try the notice again later. How your plugin holds or sets it aside meanwhile is up to it. |

With `route_to_chief`, every `2xx` answer also carries `"routed"`: where the
notice went — `"chief"` (a task event is stored for your chief, or already
was), `"agent"` (delivered to the agent working the task), `"attention"` (an
ask on your attention queue) — or `false`: there was no task, the rule routes
this notice nowhere, or the server does not run task routing. A task that
was filed but whose notice could not be routed answers `503` (or `429` when
your plugin's task-event caps are full) with its `id`: send the same request again. A
`409` means your plugin already posted a message under the event's own
external_id; resending will not help.
The task is not filed twice, and the event is not handed over twice.

The namespaces are the deployment's, set in
[configuration](configuration.md). Each one names an upstream's records, like
`clickup:`, so its tags are kept out of the board's filter row. A namespace
muster already uses (`gate`, `auto`, `initiative`, `project`,
`superseded-by`) is refused at boot.

It is idempotent, so a plugin can always resend:

- **One open task per key.** Two calls racing on one key file one task.
- **A create with a comment is one step.** Sending the same request again
  finds the task and the comment, and adds neither.
- **A comment is added once.** If the thread already holds a comment from
  this plugin with the same text, it is not added again. That includes a
  comment you retracted. Make each comment unique per upstream event, and
  put that unique part (for example the upstream's message id) at the
  **start**: a comment over 16 KiB is cut, so two long comments that differ
  only after their first 16 KiB are the same comment.
- **Closed tasks do not count.** After the task is dismissed, completed or
  merged away (and the winner does not carry the key), the next `create` for
  its key files a new one, and a comment alone writes nothing.

The task is filed the way `POST /api/tasks` files one, and you get the usual
"New task" push. It has no `gate:` or `auto:` tag, so nothing dispatches it
until you do. The comment's author is `plugin:<plugin name>`.

## Routing a task to the chief

With `MUSTER_TASK_ROUTING=1`, a plugin that files a task with
`route_to_chief: true` hands it to your chief, which decides what happens to
it. The flow:

1. The plugin calls `POST /plugin/v1/tasks` as usual, with `route_to_chief`
   and the `notice` kind (a plugin may send it on every notice).
2. muster files or finds the task and routes the notice by its kind. "A live
   agent" is one bound to the task that is pending, provisioning or running.
   - **`assigned`** goes to your **chief** as a task event, whether or not the
     task already existed (a task a mention filed is routed when it is later
     assigned). The chief picks attention, forward or dispatch.
   - **`mentioned`** does **not** go through the chief. With an agent on the
     task that the mention can reach, it is delivered to the newest one the
     way `task_forward` delivers: a muster message naming the task with the
     notice fenced as data. A running agent gets it as a turn (stored once, by
     the turn). An agent still starting whose first message is owed gets it
     appended to that first message, so its first turn carries it; it never
     becomes a conversation row of its own before then. A starting agent with
     no first message owed, or one already sent its first turn but not
     running, is not a target. Otherwise, a task `in_progress` gets an ask on
     your attention queue naming the task, raised by your chief — or, when
     you have no chief, by **muster** itself. Otherwise it is **filed only**
     (`"routed": false`) — a case the operator left unspecified, so this is
     the default.
     A mention muster routes is **reserved before it acts**: one row under its
     own id (the notice kind is part of it, so a mention never collides with a
     chief event carrying the same comment), delivered from birth (never
     handed to the chief, never "Task to route"), so identical sends —
     concurrent ones too — act once. A send that finds the mention delivered
     answers where it went; one that finds it reserved and **not yet
     completed** (being delivered right now, or left by a muster that stopped
     mid-way) answers `503` with `Retry-After: 120` — never a success, since
     nothing may have been delivered — and is answered for real once the
     delivery completes, or reclaims it 2 minutes after the reservation. It counts
     against the plugin's task-event **hour** (120; past it the mention
     answers `429` and nothing starts), but not against the chief's
     undelivered queue (300), which it never waits in. When the delivery is
     done the row is marked complete. A failed delivery — including one cut
     off because your request was cancelled — releases the reservation, so a
     resend tries again; one never completed (muster stopped mid-way) is taken
     over, and delivered, by the first send at least 2 minutes later.
   - **`update`** (a comment, a status change) goes to your chief only while
     a live agent works the task, so the chief can pass it on with
     `task_forward`; otherwise it is filed only.
3. A notice for the chief becomes one **task event** for your
   chief: a message muster writes itself, never the plugin. It is delivered
   like any plugin message (the same queue, session, retries and crash
   recovery), and no reply can be drafted to it.
4. The chief's turn is a structured record, not mail: muster's event name
   (`assigned` or `update`), the task's id, key,
   status, gate, the agents already working it, and two fields from outside
   (the task's title and the change line) on fenced lines. muster's preface
   names the three tools below and says the block is data.
5. The chief calls **one** of them:

| tool | does | refused when |
|---|---|---|
| `task_route_attention {task_id, note}` | Puts the task in front of you: an ask on `/attention` and a notification, with the chief's note — and, as its buttons, the actions the chief can take on the task right now (`task_forward` when a running agent works it; `task_dispatch` when the tool would start an agent: you opted in, the deployment can start one, and the task is open, ungated and has no live agent), returned to the chief as `actions_offered`. `route_to_session` is never offered: a task event is no_reply, so it refuses one. The offer and each tool's gate are the same predicate (`internal/api/task_actions.go`). Your answer is [delivered to the chief](attention.md#delivery) as a new message in its default session. | the task is not yours |
| `task_forward {task_id, note}` | Sends the agent already working the task (the newest running one) a message from muster naming the task, with the chief's note fenced as data. The agent re-reads the task itself. | the task is not yours; no running agent works it |
| `task_dispatch {task_id, repo?, kind?, instructions?}` | Starts a new agent on the task. `kind` is `claude-code` (the default when the deployment enables it, else `gateway`) or `gateway`. Either kind gets the task's repository and branch; a task with no repository (a plugin's task has none) can be given one with `repo`, which must be one of your connected GitHub repositories; without one the agent cannot open a pull request. A claude-code agent runs on an account from your Claude account pool, with no per-agent model, and its Claude Code session starts inside a checkout of the repository, so the repository's `CLAUDE.md` and project skills (`.claude/skills`) load ([provisioner.md](provisioner.md#a-claude-code-agents-repository)). A gateway agent also gets the task's model. `instructions` (optional, at most 1 KiB) opens the agent's first message, and is accepted **only** when it is, verbatim, your answer to an attention ask about this task (see guard 5). Offered only when `MUSTER_CHIEF_DISPATCH_OWNERS` lists at least one user. | see below |

All three are chief-only, on the agent gateway and over `/mcp`. Handing a task
to the chief never starts anything by itself: only `task_dispatch` does.

**Until your chief routes the task** the task is listed on `/attention` as
**Task to route**: while you have no chief or it is not running, busy or
unreachable (the event stays queued and is retried at each plugin claim), and
also when the chief took the event and did not act on it (its turn failed, it
called no tool, or its call was refused). It leaves the list once the chief
routes the task (`task_route_attention`, `task_forward`, or a `task_dispatch`
answered `already_dispatched` while an agent is running on the task,
recorded as a `task.chief_routed` audit row) or starts an agent on it, or once the task is
complete or dismissed. "Routes" is measured per event, from the event's latest
delivery attempt, successful or not, or from when it was stored if it has had
none: an action before that does not count. So a new event for a task already
in progress is listed again until the chief routes it, and a queued event can
reappear on the list after a later failed attempt to hand it over.

### `task_dispatch`: the guards

Each refusal has its own sentence, in this order:

1. `MUSTER_CHIEF_DISPATCH_OWNERS` is empty: the tool is not offered, and
   refused if called by name.
2. Your user id is not in `MUSTER_CHIEF_DISPATCH_OWNERS`: you have not opted
   in.
3. The deployment cannot start agents (no provisioner, or a first message
   could not be delivered).
4. `instructions` are over 1 KiB.
5. `instructions` are given and are not **your own words**: not, verbatim,
   your answer to an attention ask linked to **this** task. An ask is linked
   to its task only when muster filed it on the task's behalf
   (`task_route_attention`, or a mention's route to `/attention`) — never by
   its question's text, which any asker can write. The answer must be given
   **through that ask** — on `/attention`, or from one of your mirrored
   sessions' `attention_answer`, which counts the same (operator decision;
   the accepted risk is in [attention.md](attention.md#mirrored-claude-code-sessions)) —
   while a message you write into the
   chief's chat also settles its open asks, but every one of them at once, on
   every task, so it counts for none. The comparison is exact
   after trimming surrounding whitespace and normalising line endings: one
   word added, a changed letter case or a doubled space is refused. Your
   answer on another task, another user's answer, the task's own text and
   the chief's note are all refused. The chief should pass your answer as you
   typed it; to get one, it calls `task_route_attention` and waits.
6. `kind` is unknown, not enabled on the deployment (`MUSTER_AGENT_KINDS`), or
   one you have no credential for.
7. The **effective** repository — `repo`, or else the task's own — is not
   `owner/name`, you have no GitHub connection, or it is not one of your
   connected repositories. This holds for every kind. The chief picks `repo`
   while reading outside text, and a task's repository may have been written
   by an agent (`task_create_gated`), so either may only be one of your own.
   The task is re-read under the lock below, and a repository changed in
   between is refused.
8. Under a per-owner lock: **one start per task** — until you destroy the
   agent it made. A task the chief already started an agent for answers with
   that start (`already_dispatched`) and starts nothing, whatever became of the
   agent, **unless you destroyed that agent** (the Agents page's Delete, or
   `agent_destroy` from your mirrored session): that destroy records a release
   (`task.chief_dispatch_released`; nothing is deleted) and the chief may
   start **one** new agent on the task (operator decision: *"Owner's destroy
   resets the chief's allowance"*). The chief cannot release its own start,
   destroying an agent it did not start releases nothing, and a failed
   teardown releases nothing. A second agent alongside a live one is yours to
   start, from its card.
9. The task is not yours, is gated (`gate:` tag), names a different
   repository than `repo`, is not `open`, or already has a live agent (use
   `task_forward`).
10. **2** of the agents the chief started are still live (pending,
   provisioning or running), counting a start from the last 10 minutes whose
   agent is not recorded yet.
11. The chief started **10** agents in the last 24 hours.

A start that passed every check is recorded (a `task.chief_dispatch` audit row,
with the kind, any instructions and the id of the attention ask they are your
answer to (`instructions_ask`) — and, for an answer given over MCP,
`instructions_via: "mcp"` and `instructions_from: "<host>/<session>"` — so you
can read what the chief told the agent and where it came from) **before** the agent is created, so a start that then fails still
spends the day's unit and the task's one start. That includes a claude-code
start whose account pool has no usable account: the pool is asked only at the
create. The agent's id is recorded beside it (`task.chief_dispatch_agent`); the
caps are counted from those rows.

Only a start whose agent was **recorded** (`task.chief_dispatch_agent`) can be
released, by destroying that agent. ⚠ A start that failed before any agent
existed stays spent for good — there is nothing to destroy — and
`task_dispatch` answers `already_dispatched` with no `agent_id`: a new agent on
that task is yours to start, from its card.

The agent's first message is the chief's `instructions`, if any (control
characters removed, fenced under a line saying the chief relayed them — and,
for an MCP answer, that they were your answer given over MCP from
`<host>`/`<session>` rather than on the Attention view), then
the task's text, then muster's rules for the run, which come last and win:
open any pull request as a **draft**, never merge, never mark the task complete
(set it `ready_for_review` with the pull request's URL in a comment). Marking a
task complete is refused to every agent anyway. ⚠ The draft rule is an
instruction, not enforced by muster: pushing and opening the pull request go
through the `github` connector, which you approve per agent.

## Delivery to your chief

Right after a new message is stored, muster hands it to **your** chief agent as
a turn of its own, in a conversation dedicated to that plugin and named after
its label. The turn is a fenced block, the same structure as the
[memo relay](memo-relay.md)'s: a preface line saying it is untrusted data from
an outside sender, the message id and the data scope (and, for a `no_reply`
message, that no reply is possible), then every message field behind a content
prefix so that no text in the message can close the block.

The turn is an ordinary chat turn: your chief has the same instructions and the
same tools on it as when you chat with it, so it can act on the message in the
turn that tells it about the message.

While the turn runs the message is **delivering** to that chief. This state is
written before the turn starts, so on that same turn the chief's tools can cite
the message as delivered to it.

A delivery ends in one of three states:

- **delivered**: the chief's turn completed.
- **unconfirmed**: the send failed after the text may have reached the chief. It
  is never sent again on its own; you can [retry it](#retrying-an-unconfirmed-message). A tool turn that fails on any request after its first
  ends here too, because the runtime had already answered and the chief's tools
  had already run.
- **pending**: nothing was sent (you have no chief, it is not running, or it was
  busy). The message goes back to queued and no longer names a chief, and a
  later claim retries it.

If muster stops in the middle of a turn, the message stays **delivering**. The
next delivery to the same chief settles it as unconfirmed, and it is never sent
again. A message delivering to a chief that never gets another delivery stays
delivering, and is not sent again either.

Each settled delivery writes a `plugin.delivered` row to the audit log. A
delivering message settled after a stop is recorded with `"recovered": true`.

### Retrying an unconfirmed message

A turn cut off by a rollout or a runtime error leaves its message
**unconfirmed**, and muster does not send it again by itself, because your chief
may already have acted on it. You can decide otherwise:

- On `/plugins`, **Not confirmed by your chief** lists those messages, each with a
  **Retry** button (a signed-in, same-origin form, like Revoke and Reject; no
  fresh sign-in, since nothing leaves muster at that point). Your chief then
  handles the message like any other: a reply it drafts waits for your approval,
  **except** an answer it routes through one of your sessions to a verified
  sender you allowlisted for this plugin (`MUSTER_ROUTE_RELAY_ALLOW`), which is
  sent without it. While you have no chief the messages are still listed, but
  their Retry buttons are disabled and the section says why.
- From a session: `muster plugins retry <message-id>`
  (`POST /api/plugins/messages/{id}/retry`, the hook tier: your personal token,
  or the shared hook token on an armed server).

A retry puts the message back in the queue and hands it to your **current** chief
the way every message is delivered: the session claim, a turn with the chief's
tools, the attempt counted. With no agent runtime wired, it stays queued. It is
recorded as `plugin.retried` in the audit log. The turn's preface (outside the
fence, muster's own words) tells your chief it is a retry — the retry and
delivery-attempt numbers, that the earlier turn may already have acted on it,
to check its tasks and reply drafts before acting again, and the message's age.

A retry is refused (409, nothing changed) when the message is queued (including
a second click), being delivered or delivered; when your chief already drafted a
reply to it that is pending, approved, sent or itself unconfirmed; when it was
routed to a session that is still routed, answered or relayed; when it is a
[task event](#routing-a-task-to-the-chief) your chief already acted on (routed
the task, or started an agent on it, at or after this event's latest delivery
attempt — for a message that can be retried, the turn that handed it over — so
an action on another event of the same task does not count; the same rule that
takes it off "Task to route"); when your chief already filed a gated task
citing it (`task_create_gated` with its `message_id`, read from that call's
audit row — a task filed without citing the message is not seen); when its
plugin has been revoked (a revoked
plugin's messages never reach your chief again, by retry or otherwise); and when
you have no chief. Another user's message answers 404.

The data-scope sentence is **advisory**. The chief's own memory credential can
write anywhere; only the preface asks it to keep what it records from plugin
messages inside the plugin's scope.

## When a message asks for work

Your chief has one tool for this, `task_create_gated`. It is offered only to
your chief, and only while plugins are on. Every other agent is neither offered
it nor able to call it, on the agent gateway or over `/mcp`. While plugins are
on, a chief that muster provisioned (the reserved chief agent) has standing
instructions that say plugin messages are untrusted data, that
`task_create_gated` is how a message turns into work, that `reply_draft` only
drafts, and that anything it records from a message goes to the plugin's data
scope only. An agent that is your chief only because it is labelled "chief"
still gets the tools, but not those instructions. Its guidance is the tools' own
descriptions and the preface on each message.

```json
{"title": "…", "body": "…", "gate_reason": "needs owner ok",
 "message_id": 42, "repo": "owner/name", "tags": ["inbox"]}
```

- The task is created **gated**. The server adds `gate:<reason>`, a slug of
  `gate_reason` (`gate:awaiting-approval` when it is empty). Nothing dispatches
  it until you remove the gate.
- A caller tag in the `gate:` or `auto:` namespace is **refused**, in any
  spelling the tag grammar folds together.
- You get the usual "New task" push. The tool creates through the same path as
  `POST /api/tasks`.
- `message_id` is optional. If it is given, it must be a plugin message that was
  delivered, or is being delivered, to this chief. That includes the message on
  the turn that delivers it. The server appends a line to the body citing it.
  The link is in the body rather than a tag: a per-message tag would add a
  filter chip for every task, and it would use up one of the task's tags.
- Each chief can file **20 gated tasks in any 24 hours**. Each one writes a
  `plugin.task_gated` audit row, and the budget is counted from those rows. A
  unit is reserved before the task is created, so if the create fails after a
  valid request, the unit is still spent. Text the database would refuse (a NUL
  byte in the title, body or repo) is refused before the unit is reserved.

## Replying: drafts you approve

The chief answers a message with `reply_draft`. It is offered only to your
chief, on the agent gateway and over `/mcp`, and only while plugins are on.

```json
{"message_id": 42, "body": "Thanks, noon works."}
```

- **Nothing is sent.** The tool files a draft. You read it on `/plugins` and
  approve or reject it.
- `message_id` must be a plugin message delivered, or being delivered, to this
  chief (so the chief can draft on the turn that delivers it), and not marked
  `no_reply`.
- The body is at most 16 KiB. It is folded to NFKC and screened like an agent's
  browser-flow text: a character the card could not show, an IP address, a host
  name, a credential-shaped string, an infrastructure word, or one of the
  deployment's own names (`MUSTER_FLOW_AGENT_DENY_WORDS`) is refused. The screen
  is a heuristic; your approval is the control.
- A new draft for the same message **supersedes** the pending one. While a
  reply to the message is approved and in flight (not yet claimed, or claimed
  within its lease), no new draft is accepted.
- At most **20** drafts per agent may await you at once.

You get a `plugin` push, "A reply draft awaits your approval". Its words are
muster's own: it never quotes the message or the draft. A tap opens the draft's
card.

### The card

Each draft is a card on `/plugins`:

- The message it answers, from the outside sender, is shown escaped in its own
  box, labelled untrusted.
- The reply is shown **exactly as it will be sent**: printable ASCII as itself,
  and every other character spelled out (`\u{e9}` for é, `\n` for a new line),
  with its length in characters. Nothing invisible can ride along.
- **Approve and send** needs a sign-in from the last 12 hours (the
  [step-up window](decisions.md#step-up-twelve-hours-per-browser-session),
  `MUSTER_STEPUP_WINDOW`). If yours is older, the page asks you to confirm with
  a passkey or your password first. The server checks again whatever the page
  did.
- **Reject** needs no fresh sign-in: refusing to send is never the risky
  direction. An approved draft no plugin has collected yet shows **Withdraw the
  approval**, which rejects it the same way; once a plugin has collected it, it
  cannot be withdrawn.
- If an earlier reply to the same message is unconfirmed, a pending card says
  the sender may get both.

### What happens to a draft

| state | meaning |
|---|---|
| `pending` | awaiting your decision |
| `approved` | the next claim will hand it to the plugin, or already has (leased) |
| `sent` / `failed` | the plugin's result |
| `unconfirmed` | the plugin claimed it and never reported; it may have been sent. Never offered again, and it cannot be approved again. |
| `rejected` | you said no |
| `superseded` | the chief wrote a newer draft for the same message; or another reply to the same message was reported sent before a plugin collected this one (it may then still carry its approval time) |
| `expired` | not sent within 7 days of being written, whether pending or approved |

Filing, every decision, claim, result, lapse and expiry writes a
`plugin.draft.*` row to the audit log. A draft superseded by a newer one is
recorded as a count on the filing that replaced it; one superseded because
another reply was sent has its own `plugin.draft.superseded` row naming that
reply.

## Sessions that may already be on it

When session focus is on, muster looks up, on every delivery, which of your
live sessions (running agents and online mirrored windows, not the chief)
wrote a focus in the last six hours that shares at least two terms with the
message's subject and body, at least one of them an **identifier**: a word
with letters and digits (`node-7k`), a reference (`#137`, `PR 137`, `PR-137` —
one term however it is spelled; a bare `137` is not one), a version
(`v1.2.3`), a code name (`match_history`, `matchHistory`) or file name
(`hints.go`), or a hyphenated word with a part that is not a dictionary word
(`follow-up` and `sign-up` are not identifiers). An identifier counts once,
not again as the words it is made of, and a message contributes at most its
first 20. Common words alone ("update", "next week", "review") name nothing: an
ordinary mail or a newsletter shares several with almost any session. It appends what it finds to the chief's
turn, after the message, as a block headed **Sessions that may already be on
this**: up to three sessions, each with its id, name, host and up to three
matching focus titles with their times, within 2 KiB. No match, no block.

The block is muster's own, not the message's. It follows the message's
closing marker and is fenced under its own nonce, and every message line sits
behind the message fence's content prefix, so a message that spells the
block's header cannot reach column 0 or add a session. The titles inside it are
still a summarizer's output, so the block says to treat them as data. The
chief's standing instructions tell it to check those sessions, and
`sessions_overview`, before filing a task or drafting a reply, and to route
with `route_to_session` only when one is clearly on it.

This is pushed rather than left to the chief because a chief that was not
told called no session tool at all. See [focus](focus.md#why-a-time-window-over-history-and-not-the-current-focus)
for why the lookup reads a time window of history.

## Routing a message to a session

With `MUSTER_SESSION_ROUTING=1` (and plugins on), your chief can hand a plugin
message to the session best placed to answer it, instead of drafting the reply
itself. It finds the session by [focus](focus.md) (`focus_search`,
`fleet_list`) and calls `route_to_session(message_id, subject)`. The chief's
turn for each message says when to do this.

1. **The route.** muster records a route (`session_routes`, migration `0021`):
   the message, the chief, the target and a deadline, 30 minutes away. The
   message must be one delivered to that chief, replyable, not already routed,
   and with no reply already approved and on its way.
2. **Into the session.** Only a **mirrored Claude Code window** can take a
   routed message (`mirror:<claude session id>`). muster queues it through the
   window's [send queue](sending-and-step-up.md#queue-send-when-claude-code-is-idle),
   so it is pasted only once Claude Code has been idle for
   `MUSTER_SEND_QUEUE_IDLE`, one at a time, never into a busy or waiting
   session. What is pasted is the **stored message**, never text the chief
   wrote: a preface saying it is an untrusted external message, its `route_id`
   and how to answer, then the message behind the fence's content prefix. It
   appears in that window's queue like any queued message, and you can Cancel
   it there.
3. **The answer.** The session answers with `reply_to_chief`: from a mirrored
   window, `muster route reply <route_id> --body-file -` (the answer on stdin),
   which posts to `POST /api/routes/{route_id}/reply` with that host's muster
   credential — your personal API token, or the hook token. The route must be
   yours; the same answer again changes nothing; a different second answer is
   refused.
4. **Back to the sender**, exactly once, as the message's reply draft:
   - when the message's sender is on your allowlist (`MUSTER_ROUTE_RELAY_ALLOW`)
     **for the plugin that delivered it**, and that plugin marked the sender
     verified (`sender_verified`), the draft is filed **already approved** —
     you are not asked — and the plugin's next claim sends it through the usual
     claim and result routes;
   - otherwise — an unverified sender, the same address arriving through
     another plugin, an address that is not plain ASCII — it is an ordinary
     **pending** draft that you approve on `/plugins`, with the usual push. It
     counts toward the chief's 20 pending drafts, like any draft; at that limit
     the route fails and the message is handed back to the chief (a turn, or
     a note when it is not running), as when a route times out. A relay that
     cannot be made for another reason (the message became `no_reply`, a reply
     to it is already on its way) is handed back the same way.

   Allowlist entries are `<user id>:<plugin name>:<address>` or
   `<user id>:<plugin name>:*@<domain>`. Addresses are compared in ASCII only
   (no Unicode case folding), so a look-alike such as the Kelvin sign `K` never
   matches `k`.

   A draft names its route (`plugin_reply_drafts.route_id`, unique), so a route
   can never become two replies. The chief's conversation for the plugin gets a
   short note saying what happened. If the relay fails for a passing reason
   (the database is briefly unavailable), the reply call answers `202` with the
   answer kept, and the timeout sweep relays it a minute later.
5. **No answer by the deadline:** the route times out, a still-queued injection
   is cancelled, and the message is handed back to the chief in a turn (or a
   note, when it is not running or already mid-turn).

   Every route that ends without the chief already knowing why owes it exactly
   one hand-back: it timed out, its window moved on, or its answer could not
   be sent. The debt is recorded in the same transaction that ends the route.
   The route sweep, which runs every 30 seconds and at once when a route
   fails, is the only place it is delivered.

Every step writes a `session_route.*` audit row (`routed`, `answered`,
`relayed`, `fallback`, `timed_out`, `failed`), plus the draft's own
`plugin.draft.filed` / `plugin.draft.approved` (with `"auto": true`).

🔴 **What stands in for the step-up.** A message you queue carries the sign-in
time of the browser session that queued it, and is sent only while that sign-in
is fresh. A routed message has no browser session, so its queue row is
**marked** with its route (`send_queue.route_id`) and is exempt from the
step-up window. Instead, at the moment of the paste, muster checks that:

- the route is still waiting for an answer — so a row is never pasted after its
  route answered, timed out or failed, even if cancelling the row failed;
- the window still holds the Claude Code conversation it was routed to — if it
  now holds another, nothing is pasted, the route fails and the message goes
  back to the chief.

The other gates hold as for anyone: routing is off unless the deployment turns
it on, only your chief can route, only an `-interactive` host that reports
Claude Code's status is a target, the account check at delivery still applies
(the row carries the route's time, so a sign-out everywhere after the route
refuses it), and the row is visible and cancellable in the window's queue.

The answer comes back on `POST /api/routes/{route_id}/reply` with a personal
API token or the hook token. With `MUSTER_HOOK_TOKEN` unset, only a personal
token works, and the boot banner warns.

⚠ **The send queue does not see a half-typed prompt.** Claude Code reports
`idle` while text sits unsent in its input box; a routed paste then lands after
that text, and Enter sends both. The idle delay is the only protection.

⚠ **An answer is sent as written.** It is screened like any reply draft (no
hidden characters, addresses, credentials or the deployment's own names), and
for a verified, allowlisted sender that screen is the only check before it goes.
