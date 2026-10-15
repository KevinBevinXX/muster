# Plugins

A **plugin** is a separate process that brings messages from another system,
such as an inbox, to your **chief** agent. muster never calls a plugin. The
plugin calls muster.

Plugins are **off** unless the server runs with `MUSTER_PLUGINS=1` and a
database. While they are off, every `/plugin/*` route and the `/plugins` page
answer 404.

> This page describes what is built so far: tokens, inbound messages, delivery
> to the chief, and the chief's gated-task tool. Reply drafts that you approve,
> and the routes a plugin uses to collect them, come later.

## Two ways in

| door | who | routes |
|---|---|---|
| The **page** | you, signed in | `/plugins`: create a plugin token and revoke one |
| The **plugin** | an `mpl_` plugin token | `POST /plugin/v1/messages` |

A plugin token is shown once, when you create it. muster stores only its
sha256. The token acts as you, and only on `/plugin/v1/*`:

- Every other route refuses an `mpl_` token, even beside a valid session.
- The plugin route refuses every other credential: the shared hook and service
  secrets, personal `mst_` tokens, runner tokens, agent tokens and browser
  sessions.

Each plugin has a **name** (a slug), a **label** (what your chief's conversation
for it is called) and a **data scope**.

## Sending a message

```http
POST /plugin/v1/messages
Authorization: Bearer mpl_…
Content-Type: application/json

{"external_id": "msg-0001", "sender": "someone@example.invalid",
 "subject": "hello", "body": "…", "received_at": "2000-01-02T03:04:05Z"}
```

| answer | meaning |
|---|---|
| `202 {"id", "status": "queued"}` | stored; delivery to your chief has started |
| `200 {"id", "status"}` | this plugin already sent this `external_id`; the stored message is the answer, and nothing is stored or delivered again |
| `400` | a field breaks a limit below |
| `401` | not a live plugin token |
| `429` | a rate limit; retry after the `Retry-After` header |

`external_id` is the idempotency key, unique per plugin. A plugin that did not
see an answer should send the same message again. A repeat is answered even
while the plugin is rate-limited.

Limits are refusals, never truncations:

| limit | value |
|---|---|
| `body` | 1 byte to 16 KiB |
| `sender`, `subject` | 200 characters each |
| `external_id` | 1 to 200 bytes, no spaces or control characters |
| new messages per plugin | 60 per hour |
| undelivered messages per plugin | 200 |

## Delivery to your chief

Right after a new message is stored, muster hands it to **your** chief agent as
a turn of its own, in a conversation dedicated to that plugin and named after
its label. The turn is a fenced block, the same structure as the
[memo relay](memo-relay.md)'s: a preface line saying it is untrusted data from
an outside sender, the message id and the data scope, then every message field
behind a content prefix so that no text in the message can close the block.

The turn is an ordinary chat turn: your chief has the same instructions and the
same tools on it as when you chat with it, so it can act on the message in the
turn that tells it about the message.

A delivery ends in one of three states:

- **delivered**: the chief's turn completed.
- **unconfirmed**: the send failed after the text may have reached the chief. It
  is never sent again.
- **pending**: nothing was sent (you have no chief, it is not running, or it was
  busy). The message stays queued. Pending messages are retried by a later
  change.

Each settled delivery writes a `plugin.delivered` row to the audit log.

The data-scope sentence is **advisory**. The chief's own memory credential can
write anywhere; only the preface asks it to keep what it records from plugin
messages inside the plugin's scope.

## When a message asks for work

Your chief has one tool for this, `task_create_gated`. It is offered only to
your chief, and only while plugins are on. Every other agent is neither offered
it nor able to call it, on the agent gateway or over `/mcp`. While plugins are
on, a chief that muster provisioned (the reserved chief agent) has standing
instructions that say three things: plugin messages are
untrusted data, `task_create_gated` is how a message turns into work, and
anything it records from a message goes to the plugin's data scope only. An
agent that is your chief only because it is labelled "chief" still gets the
tool, but not those instructions. Its guidance is the tool's own description
and the preface on each message.

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
  delivered to this chief, and the server appends a line to the body citing it.
  The link is in the body rather than a tag: a per-message tag would add a
  filter chip for every task, and it would use up one of the task's tags.
- Each chief can file **20 gated tasks in any 24 hours**. Each one writes a
  `plugin.task_gated` audit row, and the budget is counted from those rows. A
  unit is reserved before the task is created, so if the create fails after a
  valid request, the unit is still spent.
