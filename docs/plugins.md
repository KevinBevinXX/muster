# Plugins

A **plugin** is a separate process that brings messages from another system,
such as an inbox, to your **chief** agent. muster never calls a plugin. The
plugin calls muster.

Plugins are **off** unless the server runs with `MUSTER_PLUGINS=1` and a
database. While they are off, every `/plugin/*` route and the `/plugins` page
answer 404.

> This page describes what is built so far: tokens, inbound messages and
> delivery to the chief. Reply drafts that you approve, and the routes a plugin
> uses to collect them, come later.

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

While the turn runs the message is **delivering** to that chief. This state is
written before the turn starts, so on that same turn the chief's tools can cite
the message as delivered to it.

A delivery ends in one of three states:

- **delivered**: the chief's turn completed.
- **unconfirmed**: the send failed after the text may have reached the chief. It
  is never sent again. A tool turn that fails on any request after its first
  ends here too, because the runtime had already answered and the chief's tools
  had already run.
- **pending**: nothing was sent (you have no chief, it is not running, or it was
  busy). The message goes back to queued and no longer names a chief. Pending
  messages are retried by a later change.

If muster stops in the middle of a turn, the message stays **delivering**. The
next delivery to the same chief settles it as unconfirmed, and it is never sent
again. A message delivering to a chief that never gets another delivery stays
delivering, and is not sent again either.

Each settled delivery writes a `plugin.delivered` row to the audit log. A
delivering message settled after a stop is recorded with `"recovered": true`.

The data-scope sentence is **advisory**. The chief's own memory credential can
write anywhere; only the preface asks it to keep what it records from plugin
messages inside the plugin's scope.
