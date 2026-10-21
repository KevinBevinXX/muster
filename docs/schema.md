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
| jobs | `jobs`: one-shot machine jobs submitted over `POST /api/jobs` and their outcomes ([jobs.md](jobs.md)) |
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
| `0025` | `attention_asks`: one row per question an agent (`agent_id`, deleted with it) or a mirrored Claude Code session (`mirror_host` + its Claude session id) put to its owner — the question, the options it offered, its state (`open`, `answered`, `cancelled`, `expired`), the answer (only the operator answers; see `0034` for where), when it expires. Each move out of `open` is one conditional update, so an answer and a cancel cannot both land. Owned, under row-level security. |
| `0026` | `attention_asks` gains the state `answered_in_session`: the operator wrote to the asker directly (a message into its mirrored session, or to the agent's chat) after it asked; the message, capped, is the answer, and `answered_by` is still `operator`. `answer_queue_id`: the send-queue row whose (queued) message answered it, for that row's "already answered" note (no foreign key: a display link). |
| `0031` | `attention_asks` gains the answer's DELIVERY to its asker: `delivery` (`queued` → `delivering` → `delivered` \| `unconfirmed`; `waited` when the asker read it with `attention_wait`; `pull` for a mirrored session; NULL when there is nothing to deliver, including every answer recorded before 0031 — no backfill), its lease (`delivery_claimed_by`, `delivery_lease_until`, set exactly while delivering), `delivery_detail` (why it waits or failed), `delivery_attempts`, `delivered_at` and `delivered_session_id` (a display link, no foreign key). Each move is one conditional update ([attention.md](attention.md#delivery)). |
| `0032` | `attention_asks.task_id`: the task an ask is about, written only by muster's own task path (`task_route_attention`, a mention's route to `/attention`), never from an asker's text. A plain foreign key to `notes` (`ON DELETE SET NULL`), its owner checked by a trigger (the ask's owner must own the task). Backfilled for asks the chief routed, matched by their `task.chief_routed` audit row. `task_dispatch` accepts `instructions` only as the owner's answer to such an ask. Must apply after `0031`. |
| `0033` | `connector_grants.source` (`stored` \| `app`, default `stored`): which credential an approval was about — a stored connector row, or the [GitHub App](github-app.md). A grant whose source no longer matches what its connector is backed by (the App configured over a stored `github` token, or removed from under one) is put back to `pending` and must be approved again. Must apply after `0032`. |
| `0034` | `attention_asks` records where the operator gave an answer that did not come from `/attention`: `answered_via` (`mcp` for [`attention_answer`](attention.md#the-agent-tools), NULL for the view, an answer in the session and every answer recorded before 0034 — no backfill), `answered_via_host` and `answered_via_session` (the mirrored session it came from). `answered_by` stays `operator`. Its CHECK makes provenance all-or-nothing, only on an `answered` ask, with the mirror wire's host and session shapes, and never from the host a mirrored asker is on (`mirror_host = answered_via_host` is refused, whichever session answers). `task_dispatch` accepts an answer with `answered_via` set as instructions like any other (operator decision), and its audit row then records `instructions_via` / `instructions_from`. Must apply after `0033`. Rollback: an image from before 0034 runs unchanged over it (it never writes the columns); to remove them, drop the constraint and the three columns and delete `schema_migrations` row 34 (the SQL is in the migration's header). |
| `0035` | `connector_grants` records where the operator decided a request that was not decided on the Connectors tab: `decided_via` (`mcp` for [`connector_request_decide`](connectors.md#deciding-a-request-from-your-own-session), NULL for the tab, a pending or revoked grant and every decision recorded before 0035 — no backfill), `decided_via_host` and `decided_via_session` (the mirrored session it came from). `decided_by` stays `operator`. Its CHECK makes provenance all-or-nothing, only on an `approved` or `denied` grant, with the mirror wire's host and session shapes; re-opening and revoking a grant clear it in the same statement. Must apply after `0034`. During a rolling deploy, an old pod's revoke or re-open of a grant a new pod just decided over MCP is refused by the CHECK (a 500 until that pod is replaced; measured at the SQL level, not on a live deploy). Rollback, in order, with the exact SQL in the migration's header (`[rollback-sql]`, `[restore-sql]`; `TestMigration0035RollbackAndRestore` runs both as written): (1) **before** rolling the image back, drop the CHECK **and** set all three columns to NULL — an older image never clears them, so a grant it re-opens and approves on the tab would otherwise keep a stale "via MCP" source; (2) roll the image back — `schema_migrations` keeps row 35, so a roll-forward does not re-apply 0035; (3) at roll-forward, before the new image takes traffic, set the columns to NULL again and re-add the CHECK. To remove the columns instead, after (1) and the image rollback drop them and delete `schema_migrations` row 35. |
| `0036` | The four stored session keys — `agent_focus.subject_key`, `agent_focus_history.subject_key`, `session_routes.target_key`, `attention_asks.mirror_session` — admit a [Hermes](design/hermes.md) conversation's key as well as a Claude Code id: `hermes:` and an id of 1–128 characters (Hermes ids are `local-<64 hex>`, 70). A bare id stays at 64, so every key stored before is still valid and a Hermes key is never read as a Claude one. The expression is `mirror.SessionKeyPattern`; a test pins that the migration spells it. `attention_asks.answered_via_session` (0034) and `connector_grants.decided_via_session` (0035) are deliberately left at 64: each names an MCP caller. Must apply after `0035`. Rollback: an image from before 0036 runs unchanged over it (it writes only Claude ids, which still match); to restore the old CHECKs, delete any `hermes:` rows, re-add each constraint with `'^[0-9A-Za-z][0-9A-Za-z_-]{0,63}$'`, and delete `schema_migrations` row 36. |
| `0037` | `jobs`: one row per machine job ([jobs.md](jobs.md)) — the agent (`agent_id`, set NULL when the agent is deleted, and `agent_name`, kept), the prompt, `repo`, `sha` (a full 40-hex commit id, or empty), `result_path`, `timeout_s`, `idempotency_key` (unique per owner), `requested_by` (the service caller's `X-Muster-Actor`), `status` (`queued` → `running` → `succeeded` \| `failed` \| `timed_out`), `result` (verbatim, at most 1 MiB, only on `succeeded`), `reason` and `error` (only on `failed` / `timed_out`), `cc_account`, `session_id` (the chat session the turn ran in; a display link, no foreign key) and the timestamps. Each move is one conditional update; a `queued` or `running` row past its own deadline is failed with reason `interrupted` by the reaper (`jobs.Store.ReapOverdue`, at boot and every minute), never one still inside it. Owned, under row-level security. Rollback: an image from before 0037 never reads the table; to remove it, `DROP TABLE public.jobs` and delete `schema_migrations` row 37. |

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
