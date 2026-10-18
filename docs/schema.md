# The schema

The schema is a series of numbered migrations in
[`internal/db/migrations/`](../internal/db/migrations), applied in order.
`muster-server` applies them at boot. `muster-migrate` applies them on demand,
with `DATABASE_URL`.

The public tables they leave are pinned as a ledger by
`tablesThisSchemaMustHave` in `internal/db/migrate_test.go`. That test fails if
the set grows **or** shrinks.

| area | tables |
|---|---|
| tasks | `notes`, `note_comments`, `note_attachments`, `task_sessions` |
| agents | `agents`, `chat_sessions`, `chat_messages`, `agent_retired_names`, `cc_account_marks` |
| connectors | `connectors`, `connector_grants`, `connector_audit` |
| accounts | `users`, `invites`, `api_tokens`, `audit_log`, `user_credentials` |
| mirroring | `mirror_tokens` |
| browser flows | `flow_jobs`, `flow_runner_tokens`, `flow_requests` |
| push | `push_subscriptions`, `push_preferences` |
| plugins | `plugins`, `plugin_messages`, `plugin_reply_drafts` |
| Agents list | `pins`: the entries each user pinned to the top; `recents`: the entries each user recently opened |
| send queue | `send_queue`: messages each user queued for a mirrored Claude Code window, sent one at a time when it is idle |
| session focus | `agent_focus`: what each of a user's sessions is working on ([focus.md](focus.md)); `agent_focus_history`: every focus each session has had |
| session routing | `session_routes`: plugin messages the chief routed to a session, and how each route ended ([plugins.md](plugins.md#routing-a-message-to-a-session)) |
| attention queue | `attention_asks`: questions agents put to their owner and the answers ([attention.md](attention.md)) |
| GitHub | `github_connection`, one row per owner |

`notes` is the task table. The name is historical. It is kept because every id in
it is a task number people cite, and renaming it would be a data migration that
buys only a word.

| migration | what it does |
|---|---|
| `0001` | The initial schema, stated as its **end state**, not as a replay of history. It is never edited. It still creates the runbook and privilege tables that `0005` drops. |
| `0002` | `agent_retired_names`: a retired agent name can never be reused. |
| `0003` | Agent kinds (existing rows become `gateway`) and the Claude Code account pool. |
| `0004` | The connector store: AES-GCM secrets, grants and an audit table. |
| `0005` | Drops the privilege and runbook tables. Privileges were superseded by connectors. |
| `0006` | Accounts: `users`, with id 1 as admin. Adds an `owner_id` to every user-data table, and composite foreign keys over `(parent_id, owner_id)`, so a row can never point at another user's row. |
| `0007` | Row-level security, plus the `muster_app` role. |
| `0008` | `mirror_tokens`. |
| `0009` | One chief per owner, and row-level security on `users`, `invites`, `api_tokens` and `audit_log`. |
| `0010` | `users.sessions_not_before`, a per-user session cutoff. |
| `0011` | `user_credentials` (bring-your-own Claude and OpenRouter keys). `cc_account_marks` becomes per owner. |
| `0012` | `flow_jobs` and `flow_runner_tokens`. |
| `0013` | `push_subscriptions` and `push_preferences`. |
| `0014` | `flow_requests`: an agent's request for a flow, which its owner approves. |
| `0015` | `plugins` and `plugin_messages`: the [plugin interface](plugins.md). A message's input cannot be changed after it is written. |
| `0016` | `plugin_reply_drafts` (a draft's body cannot be changed after it is written; the claim, lease and result rules) and `plugin_messages.no_reply`, which cannot be changed and refuses any reply draft for its message. |
| `0017` | `pins`: a user's pinned Agents-list entries, each naming an agent (deleted with it) or a mirrored window by `(host, session, tmux window id)`. Owned, under row-level security; the agent a pin names must be the pin's owner's. |
| `0018` | `recents`: a user's recently opened Agents-list entries, keyed as `pins` are, one row per entry (a visit moves its time), at most 50 per user (the oldest pruned). Owned, under row-level security. |
| `0019` | `send_queue`: the server-side send queue. Each row names a mirrored window (host, tmux session, window id) and carries the message, its state (`queued`, `sending`, `sent`, `failed`, `cancelled`) and the sign-in time of the session that queued it. At most one row per window is `sending`; its claim is a lease kept on the database's clock. Owned, under row-level security. Also `users.signed_out_at`: the latest "sign out everywhere", which the queue's delivery checks. |
| `0020` | `agent_focus`: each session's [focus](focus.md), one row per (owner, subject). A subject is an agent (by id, deleted with it) or a mirrored Claude Code session (by its Claude session id). Title, description, its source (`summarizer`, the only writer), the fingerprint of the transcript tail it was computed from, and the last refresh failure as a short non-secret reason, with its time. A generated `tsvector` over title (weight A) and description (B) with a GIN index serves `focus_search`. Owned, under row-level security. |
| `0021` | `session_routes`: one row per routed plugin message — its route id, the chief, the target session, the send-queue row that carried it, its state (`routed`, `answered`, `relayed`, `timed_out`, `fallback`, `failed`), the answer and the reply draft it became. At most one open route per message. Also `plugin_reply_drafts.route_id`, unique, so a route becomes at most one draft; `send_queue.route_id`, which marks a routed queue row (gated on its route at delivery); `session_routes.handback`, the hand-back an ended route still owes its chief (cleared by the sweep that delivers it); and `plugin_messages.sender_verified`, the plugin's immutable claim that it authenticated the sender. Owned, under row-level security. |
| `0022` | `agent_focus_history`: append-only, one row each time a session's focus title or description changes (title, description, when), written in the focus upsert's transaction. It has no foreign key to `agent_focus`: a session's history outlives its focus row, and is deleted after 30 days by the hourly sweep (a subject's newest row is kept while its focus row exists) or with its agent (`agent_id`, `ON DELETE CASCADE`). Existing focus rows seed one history row each. Owned, under row-level security. |
| `0025` | `attention_asks`: one row per question an agent (`agent_id`, deleted with it) or a mirrored Claude Code session (`mirror_host` + its Claude session id) put to its owner — the question, the options it offered, its state (`open`, `answered`, `cancelled`, `expired`), the answer (only the operator answers), when it expires. Each move out of `open` is one conditional update, so an answer and a cancel cannot both land. Owned, under row-level security. |

To change the schema, add a new numbered file.

## Row-level security

Every owned table has row-level security turned on (`ENABLE` + `FORCE`) with an
`owner_isolation` policy. A connection sees and writes only the rows whose
`owner_id` matches its `muster.uid` setting, unless `muster.scope = 'all'`.

On **every pool acquire**, `internal/db` sets both settings from the request's
principal. It also switches to the `muster_app` role, because a superuser login
bypasses row-level security. A context with no principal sees nothing and inserts
nothing. `psql`, `pg_dump` and backups connect as the login role and still see
every row.

🔴 **Restoring a backup into a fresh Postgres.** `muster_app` is a cluster role,
and `pg_dump` does not carry it. So the dump's `GRANT … TO muster_app` lines fail,
and `ON_ERROR_STOP` stops at the first one. You can avoid this either way:

- restore without `ON_ERROR_STOP`;
- or run `CREATE ROLE muster_app NOLOGIN` first.

Then boot: the migrator re-creates the role and its grants on every run
(`internal/db/approle.go`).

🔴 **Rolling the image back below `0007`.** An older image never sets
`muster.uid`, so every insert it makes fails. Before you change the image, turn
row-level security off **from the current binary**:

- run `/muster-migrate --rls-off` in the current pod;
- or apply `internal/db/rollback/rls_off.sql` with psql.

The older image's `/muster-migrate` does not know the flag. It says "nothing to
apply", exits 0, and leaves row-level security on. `--rls-off` and `--rls-on`
each print the state of every owned table, and fail if any table is wrong.

## Two properties worth knowing before you change it

- 🔴 **Every `id` is `GENERATED ALWAYS AS IDENTITY`.** `internal/db/migrate_test.go`
  asserts this for every identity column in the schema, not a list of named ones.
  Weakening a column to `BY DEFAULT` would go unnoticed for months: it changes
  nothing about how muster writes, and only matters when someone assumes a
  hand-written `INSERT` importer is fine. Existing rows are meant to arrive by
  `pg_dump --data-only`, whose `COPY` path is exempt from the restriction.
- ⚠ **Some columns deliberately have no foreign key**, such as
  `task_sessions.session_id` and `notes.source_session_id`. They name things in
  other systems, or they are audit rows that must outlive their subjects. Adding
  a foreign key to one of them is not a cleanup.
