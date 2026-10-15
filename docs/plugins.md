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
   (`reply_draft`), or do nothing.
4. You approve or reject each draft on `/plugins`.
5. The plugin claims an approved draft (`POST /plugin/v1/drafts/claim`), sends
   it, and reports the result (`POST /plugin/v1/drafts/{id}/result`).

## Two ways in

| door | who | routes |
|---|---|---|
| The **page** | you, signed in | `/plugins`: create and revoke plugin tokens; approve, reject or re-approve reply drafts |
| The **plugin** | an `mpl_` plugin token | `POST /plugin/v1/messages`, `POST /plugin/v1/drafts/claim`, `POST /plugin/v1/drafts/{id}/result` |

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
 "no_reply": false}
```

| field | required | rule |
|---|---|---|
| `external_id` | yes | 1 to 200 bytes, no spaces or control characters. The idempotency key, unique per plugin. |
| `body` | yes | 1 byte to 16 KiB |
| `sender`, `subject` | no | 200 characters each |
| `received_at` | no | RFC 3339 |
| `no_reply` | no | `true` marks a message nobody may answer (see below). Default `false`. |

| answer | meaning |
|---|---|
| `202 {"id", "status": "queued"}` | stored; delivery to your chief has started |
| `200 {"id", "status"}` | this plugin already sent this `external_id`. The stored message is the answer, and nothing is stored or delivered again, **whatever the new body or `no_reply` says**. `status` is `queued`, `delivered` or `unconfirmed`. |
| `400 {"error"}` | a field breaks a rule above |
| `429 {"error"}` | 60 new messages in the last hour, or 200 not yet delivered. Retry after the `Retry-After` header. |

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
that long-polls keeps delivery moving.

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
it again** on its own. Your owner sees it on `/plugins`, can check the upstream,
and may approve it again. That is a new decision, recorded in the audit log, and
it makes a **new draft with a new `draft_id`** and the same text, which the next
claim receives.

A result that arrives after the lease lapsed is still recorded, on the draft it
was claimed as. It never settles the re-approved copy, but a late `ok: true`
**cancels** the copy if no claim has taken it yet: the text has reached the
sender, so it must not go twice.

Only one reply to a message is in flight at a time: while one is approved and
either unclaimed or within its lease, no other draft for that message can be
filed or approved.

Set your HTTP client's timeout to `wait_seconds` plus at least 10 seconds. A
claim whose answer you never received is still a claim: the reply becomes
unconfirmed when its lease lapses, and your owner has to approve it again.

So a plugin should:

1. record the `draft_id` durably **before** sending upstream;
2. never send the same `draft_id` twice, even after a restart (a re-approved
   reply arrives under a new `draft_id`, so this never blocks one);
3. post the result as soon as it knows, and post it again if it did not see an
   answer (a repeat is safe).

## Delivery to your chief

Right after a new message is stored, muster hands it to **your** chief agent as
a turn of its own, in a conversation dedicated to that plugin and named after
its label. The turn is a fenced block, the same structure as the
[memo relay](memo-relay.md)'s: a preface line saying it is untrusted data from
an outside sender, the message id and the data scope (and, for a `no_reply`
message, that no reply is possible), then every message field behind a content
prefix so that no text in the message can close the block.

A delivery ends in one of three states:

- **delivered**: the chief's turn completed.
- **unconfirmed**: the send failed after the text may have reached the chief. It
  is never sent again.
- **pending**: nothing was sent (you have no chief, it is not running, or it was
  busy). The message stays queued until a later claim retries it.

Each settled delivery writes a `plugin.delivered` row to the audit log.

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
  delivered to this chief, and the server appends a line to the body citing it.
  The link is in the body rather than a tag: a per-message tag would add a
  filter chip for every task, and it would use up one of the task's tags.
- Each chief can file **20 gated tasks in any 24 hours**. Each one writes a
  `plugin.task_gated` audit row, and the budget is counted from those rows. A
  unit is reserved before the task is created, so if the create fails after a
  valid request, the unit is still spent.

## Replying: drafts you approve

The chief answers a message with `reply_draft`. It is offered only to your
chief, on the agent gateway and over `/mcp`, and only while plugins are on.

```json
{"message_id": 42, "body": "Thanks, noon works."}
```

- **Nothing is sent.** The tool files a draft. You read it on `/plugins` and
  approve or reject it.
- `message_id` must be a plugin message delivered to this chief and not marked
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
- If an earlier reply to the same message is unconfirmed, the card says the
  sender may get both.

### What happens to a draft

| state | meaning |
|---|---|
| `pending` | awaiting your decision |
| `approved` | the next claim will hand it to the plugin, or already has (leased) |
| `sent` / `failed` | the plugin's result |
| `unconfirmed` | the plugin claimed it and never reported; it may have been sent. Never offered again. **Approve again** (fresh sign-in, audited) makes a new approved draft with the same text, within the original 7 days. |
| `rejected` | you said no |
| `superseded` | the chief wrote a newer draft for the same message |
| `expired` | not sent within 7 days of being written, whether pending or approved |

Filing, every decision, claim, result, lapse and expiry writes a
`plugin.draft.*` row to the audit log. A superseded draft is recorded as a
count on the filing that replaced it.
