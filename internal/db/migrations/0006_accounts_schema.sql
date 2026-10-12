-- muster 0006 — accounts, schema only (multi-user accounts PR 1 of 6).
--
-- This file adds the SHAPE of per-user ownership and changes no behaviour:
--
--   * `users`, seeded with id 1 = the operator (role admin). Every existing row
--     in every user-data table is backfilled to owner 1.
--   * `owner_id BIGINT NOT NULL DEFAULT 1 REFERENCES users` on every table that
--     holds user data (list below). 🔴 THE `DEFAULT 1` IS TEMPORARY AND IS WHAT
--     KEEPS TODAY'S BINARY UNCHANGED: no INSERT in the tree names owner_id, so
--     every one lands as the operator, exactly as before. Accounts PR 3 replaces
--     the default with the request principal's setting (and RLS); until then a
--     second user cannot exist through the application.
--   * COMPOSITE FOREIGN KEYS so a cross-user link is structurally impossible:
--     every parent carries UNIQUE (id, owner_id), and every child references
--     (parent_id, owner_id). A child row can therefore only point at a parent
--     with the SAME owner — no handler, store or hand-run psql can attach user
--     B's comment to user A's task.
--   * `connectors` is unique per (owner_id, name), not globally by name.
--   * `github_connection` is keyed by owner_id; the id = 1 singleton CHECK and the
--     `id` column are gone (internal/github.PGStore reads/writes owner 1).
--   * Three new, EMPTY tables: invites, api_tokens, audit_log. Nothing reads or
--     writes them yet.
--
-- OWNED (owner_id added): notes, note_comments, note_attachments, task_sessions,
-- agents, chat_sessions, chat_messages, connectors, connector_grants,
-- connector_audit, github_connection.
-- SHARED (deliberately NO owner): cc_account_marks (the operator's account pool;
-- becomes per-(owner, account) in the bring-your-own-credentials PR),
-- agent_retired_names (agent names are one global namespace), schema_migrations.
--
-- 🔴 THE COMPOSITE-FK SET IS EVERY parent->child FK AMONG THE OWNED TABLES —
-- eight, enumerated from 0001..0005, and pinned as a ledger by
-- TestAccountsCompositeForeignKeyLedger (fails if the set grows or shrinks):
--
--   note_comments    (note_id,    owner_id) -> notes         ON DELETE CASCADE
--   note_attachments (note_id,    owner_id) -> notes         ON DELETE CASCADE
--   task_sessions    (note_id,    owner_id) -> notes         ON DELETE CASCADE
--   agents           (note_id,    owner_id) -> notes         ON DELETE SET NULL (note_id)
--   chat_sessions    (agent_id,   owner_id) -> agents        ON DELETE CASCADE
--   chat_messages    (agent_id,   owner_id) -> agents        ON DELETE CASCADE
--   chat_messages    (session_id, owner_id) -> chat_sessions ON DELETE CASCADE
--   connector_grants (agent_id,   owner_id) -> agents        ON DELETE CASCADE
--
-- Each REPLACES the single-column FK it extends, with the same delete action, so
-- there is one rule per relationship rather than two that could disagree.
--
-- ⚠ TWO REFERENCES AMONG THESE TABLES ARE *NOT* FOREIGN KEYS, AND THIS FILE
-- DOES NOT MAKE THEM ONE:
--   * connector_audit.agent_id has no FK (0004): an audit row outlives its agent.
--     The row carries its own owner_id; nothing ties it to the agent's owner.
--   * connector_grants.connector_name names a connector BY NAME and may name one
--     that does not exist yet (0004's header). An (owner_id, connector_name) FK
--     would forbid exactly that. Grant-to-connector owner agreement is therefore
--     NOT structural; it is enforced where a grant is resolved (accounts PR 3b).
--
-- 🔴 NULLABLE PARENT REFERENCES AND MATCH SIMPLE. agents.note_id and
-- chat_messages.session_id are nullable while owner_id is NOT NULL. Under the
-- default MATCH SIMPLE a composite FK is not checked when ANY referencing column
-- is NULL — so an agent with no task, or a message with no session, is legal, as
-- it was before. MATCH FULL would REJECT those rows (a NULL beside a non-NULL),
-- which is why it is not used. Pinned by the NULL cases in the 0006 tests.
--
-- 🔴 `ON DELETE SET NULL (note_id)` — WITH THE COLUMN LIST — IS LOAD-BEARING.
-- A bare SET NULL on a composite FK nulls EVERY referencing column, owner_id
-- included, and owner_id is NOT NULL: deleting a task would then FAIL for any
-- task an agent worked, instead of detaching the agent as 0001 intends. The
-- column-list form (Postgres 15+; muster runs 16 in prod, CI and the test
-- compose) nulls only note_id. Pinned by the delete-a-task test.
--
-- ⚠ ON UPDATE IS LEFT AT NO ACTION. Changing a parent's owner_id while it has
-- children is refused. Ownership transfer is a later feature and will move a
-- parent and its children in one statement set; nothing does it today.
--
-- ⚠ ROLLOUT AND ROLLBACK. muster rolls with maxUnavailable 0, so the previous
-- image serves briefly against this schema; every INSERT it makes still works
-- (DEFAULT 1). 🔴 BUT ANY IMAGE BUILT BEFORE THIS MIGRATION (0.10.0 and older)
-- NAMES THE DROPPED github_connection.id COLUMN IN EVERY GitHub QUERY, and this
-- file is not reversed by rolling the image back. So with GitHub ENABLED
-- (MUSTER_GITHUB_ENCRYPTION_KEY set), an old image against this schema is
-- broken for as long as it runs, not just for a drain: Save/Clear error, and
-- Get errors — which internal/api's resolveRepoBranch swallows, silently
-- cloning `main` instead of a repo's real default branch (e.g. `trunk`). It is
-- harmless only while GitHub is disabled, as it is in production today. A
-- rollback below this migration with GitHub enabled needs this table restored
-- by hand (re-add `id INTEGER DEFAULT 1`, set it to 1 for owner 1's row, make it
-- the key again).
--
-- Convention: idempotent where Postgres allows it (IF [NOT] EXISTS, DROP
-- CONSTRAINT IF EXISTS before ADD), one transaction (internal/db/migrate.go).

SET LOCAL lock_timeout = '3s';

-- ---------------------------------------------------------------------------
-- 1. users
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS public.users (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- The GoTrue `sub` this row is bound to. NULL until claimed (user 1 is
    -- claimed by the admin bootstrap invite in accounts PR 4).
    gotrue_sub   UUID        UNIQUE,
    email        TEXT,
    display_name TEXT        NOT NULL DEFAULT '',
    role         TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    disabled_at  TIMESTAMPTZ,
    CONSTRAINT users_role_check CHECK (role IN ('admin', 'member'))
);

-- Email uniqueness is CASE-INSENSITIVE: `Ann@x.io` and `ann@x.io` are one
-- mailbox, and a case-sensitive UNIQUE would let an invite bound to one be
-- redeemed as a second account under the other. NULLs (no email) do not
-- collide.
CREATE UNIQUE INDEX IF NOT EXISTS users_email_lower_key ON public.users (lower(email));

-- The operator. OVERRIDING SYSTEM VALUE because id is GENERATED ALWAYS (0001's
-- header: the restriction stays; this one seed row is the documented exception).
INSERT INTO public.users (id, role, display_name)
    OVERRIDING SYSTEM VALUE VALUES (1, 'admin', 'operator')
    ON CONFLICT (id) DO NOTHING;

-- Continue the identity AFTER the seeded row, or the first real INSERT would
-- mint id 1 again and fail on the primary key. setval(.., n) with is_called
-- true makes the next value n+1.
SELECT setval(pg_get_serial_sequence('public.users', 'id'),
              GREATEST((SELECT max(id) FROM public.users), 1));

-- ---------------------------------------------------------------------------
-- 2. owner_id on every owned table — backfilled to 1, NOT NULL, DEFAULT 1.
--
-- `ADD COLUMN … NOT NULL DEFAULT 1` is the backfill: Postgres stores a constant
-- default for every existing row without a rewrite, then enforces NOT NULL.
-- The FK to users is checked against those rows in the same statement.
-- ---------------------------------------------------------------------------

ALTER TABLE public.notes             ADD COLUMN IF NOT EXISTS owner_id BIGINT NOT NULL DEFAULT 1 REFERENCES public.users (id);
ALTER TABLE public.note_comments     ADD COLUMN IF NOT EXISTS owner_id BIGINT NOT NULL DEFAULT 1 REFERENCES public.users (id);
ALTER TABLE public.note_attachments  ADD COLUMN IF NOT EXISTS owner_id BIGINT NOT NULL DEFAULT 1 REFERENCES public.users (id);
ALTER TABLE public.task_sessions     ADD COLUMN IF NOT EXISTS owner_id BIGINT NOT NULL DEFAULT 1 REFERENCES public.users (id);
ALTER TABLE public.agents            ADD COLUMN IF NOT EXISTS owner_id BIGINT NOT NULL DEFAULT 1 REFERENCES public.users (id);
ALTER TABLE public.chat_sessions     ADD COLUMN IF NOT EXISTS owner_id BIGINT NOT NULL DEFAULT 1 REFERENCES public.users (id);
ALTER TABLE public.chat_messages     ADD COLUMN IF NOT EXISTS owner_id BIGINT NOT NULL DEFAULT 1 REFERENCES public.users (id);
ALTER TABLE public.connectors        ADD COLUMN IF NOT EXISTS owner_id BIGINT NOT NULL DEFAULT 1 REFERENCES public.users (id);
ALTER TABLE public.connector_grants  ADD COLUMN IF NOT EXISTS owner_id BIGINT NOT NULL DEFAULT 1 REFERENCES public.users (id);
ALTER TABLE public.connector_audit   ADD COLUMN IF NOT EXISTS owner_id BIGINT NOT NULL DEFAULT 1 REFERENCES public.users (id);
ALTER TABLE public.github_connection ADD COLUMN IF NOT EXISTS owner_id BIGINT NOT NULL DEFAULT 1 REFERENCES public.users (id);

-- ---------------------------------------------------------------------------
-- 3. Drop every child FK this file replaces — the single-column originals
--    (Postgres's default names for 0001/0004's inline REFERENCES) AND the
--    composite ones, so a re-run can drop and re-add the parent keys below,
--    which those composite FKs depend on. The FK ledger test fails if an
--    original survives.
-- ---------------------------------------------------------------------------

ALTER TABLE public.note_comments DROP CONSTRAINT IF EXISTS note_comments_note_id_fkey;
ALTER TABLE public.note_comments DROP CONSTRAINT IF EXISTS note_comments_note_owner_fkey;
ALTER TABLE public.note_attachments DROP CONSTRAINT IF EXISTS note_attachments_note_id_fkey;
ALTER TABLE public.note_attachments DROP CONSTRAINT IF EXISTS note_attachments_note_owner_fkey;
ALTER TABLE public.task_sessions DROP CONSTRAINT IF EXISTS task_sessions_note_id_fkey;
ALTER TABLE public.task_sessions DROP CONSTRAINT IF EXISTS task_sessions_note_owner_fkey;
ALTER TABLE public.agents DROP CONSTRAINT IF EXISTS agents_note_id_fkey;
ALTER TABLE public.agents DROP CONSTRAINT IF EXISTS agents_note_owner_fkey;
ALTER TABLE public.chat_sessions DROP CONSTRAINT IF EXISTS chat_sessions_agent_id_fkey;
ALTER TABLE public.chat_sessions DROP CONSTRAINT IF EXISTS chat_sessions_agent_owner_fkey;
ALTER TABLE public.chat_messages DROP CONSTRAINT IF EXISTS chat_messages_agent_id_fkey;
ALTER TABLE public.chat_messages DROP CONSTRAINT IF EXISTS chat_messages_agent_owner_fkey;
ALTER TABLE public.chat_messages DROP CONSTRAINT IF EXISTS chat_messages_session_id_fkey;
ALTER TABLE public.chat_messages DROP CONSTRAINT IF EXISTS chat_messages_session_owner_fkey;
ALTER TABLE public.connector_grants DROP CONSTRAINT IF EXISTS connector_grants_agent_id_fkey;
ALTER TABLE public.connector_grants DROP CONSTRAINT IF EXISTS connector_grants_agent_owner_fkey;

-- ---------------------------------------------------------------------------
-- 4. Composite keys on the parents.
-- ---------------------------------------------------------------------------

ALTER TABLE public.notes         DROP CONSTRAINT IF EXISTS notes_id_owner_key;
ALTER TABLE public.notes         ADD  CONSTRAINT notes_id_owner_key         UNIQUE (id, owner_id);
ALTER TABLE public.agents        DROP CONSTRAINT IF EXISTS agents_id_owner_key;
ALTER TABLE public.agents        ADD  CONSTRAINT agents_id_owner_key        UNIQUE (id, owner_id);
ALTER TABLE public.chat_sessions DROP CONSTRAINT IF EXISTS chat_sessions_id_owner_key;
ALTER TABLE public.chat_sessions ADD  CONSTRAINT chat_sessions_id_owner_key UNIQUE (id, owner_id);

-- ---------------------------------------------------------------------------
-- 5. Child FKs: the composite replacements, same delete actions.
-- ---------------------------------------------------------------------------

ALTER TABLE public.note_comments ADD CONSTRAINT note_comments_note_owner_fkey
    FOREIGN KEY (note_id, owner_id) REFERENCES public.notes (id, owner_id) ON DELETE CASCADE;

ALTER TABLE public.note_attachments ADD CONSTRAINT note_attachments_note_owner_fkey
    FOREIGN KEY (note_id, owner_id) REFERENCES public.notes (id, owner_id) ON DELETE CASCADE;

ALTER TABLE public.task_sessions ADD CONSTRAINT task_sessions_note_owner_fkey
    FOREIGN KEY (note_id, owner_id) REFERENCES public.notes (id, owner_id) ON DELETE CASCADE;

-- MATCH SIMPLE (default): an agent with no task (note_id NULL) is not checked.
-- SET NULL (note_id): see the header — a bare SET NULL would null owner_id too.
ALTER TABLE public.agents ADD CONSTRAINT agents_note_owner_fkey
    FOREIGN KEY (note_id, owner_id) REFERENCES public.notes (id, owner_id) ON DELETE SET NULL (note_id);

ALTER TABLE public.chat_sessions ADD CONSTRAINT chat_sessions_agent_owner_fkey
    FOREIGN KEY (agent_id, owner_id) REFERENCES public.agents (id, owner_id) ON DELETE CASCADE;

ALTER TABLE public.chat_messages ADD CONSTRAINT chat_messages_agent_owner_fkey
    FOREIGN KEY (agent_id, owner_id) REFERENCES public.agents (id, owner_id) ON DELETE CASCADE;

-- MATCH SIMPLE (default): a message with no session (session_id NULL) is not checked.
ALTER TABLE public.chat_messages ADD CONSTRAINT chat_messages_session_owner_fkey
    FOREIGN KEY (session_id, owner_id) REFERENCES public.chat_sessions (id, owner_id) ON DELETE CASCADE;

ALTER TABLE public.connector_grants ADD CONSTRAINT connector_grants_agent_owner_fkey
    FOREIGN KEY (agent_id, owner_id) REFERENCES public.agents (id, owner_id) ON DELETE CASCADE;

-- ---------------------------------------------------------------------------
-- 6. connectors: a name is unique per owner, not globally.
--    CreateConnector's 23505 -> "already exists" mapping is unchanged: the new
--    constraint raises the same SQLSTATE for the same (single-owner) collision.
-- ---------------------------------------------------------------------------

ALTER TABLE public.connectors DROP CONSTRAINT IF EXISTS connectors_name_key;
ALTER TABLE public.connectors DROP CONSTRAINT IF EXISTS connectors_owner_name_key;
ALTER TABLE public.connectors ADD CONSTRAINT connectors_owner_name_key UNIQUE (owner_id, name);

-- ---------------------------------------------------------------------------
-- 7. github_connection: one connection per owner, replacing the id = 1
--    singleton. The existing row (if any) is owner 1 from step 2.
-- ---------------------------------------------------------------------------

ALTER TABLE public.github_connection DROP CONSTRAINT IF EXISTS github_connection_id_check;
ALTER TABLE public.github_connection DROP CONSTRAINT IF EXISTS github_connection_pkey;
ALTER TABLE public.github_connection DROP COLUMN IF EXISTS id;
ALTER TABLE public.github_connection ADD CONSTRAINT github_connection_pkey PRIMARY KEY (owner_id);

-- ---------------------------------------------------------------------------
-- 8. Owner-leading indexes, one per owned table, keyed on what that table's
--    main list query orders by — so a per-owner list (accounts PR 3) is an index
--    range scan, not a filter over everyone's rows.
--    connectors needs none: connectors_owner_name_key IS (owner_id, name), and
--    ListConnectors orders by name. github_connection's PK is owner_id.
-- ---------------------------------------------------------------------------

-- notes: the board (ListPage) orders by an activity GREATEST() no index can
-- serve; the indexable key underneath it, and the order of the other list
-- reads, is updated_at DESC over live rows (cf. idx_notes_live_updated_at).
CREATE INDEX IF NOT EXISTS idx_notes_owner_live_updated_at
    ON public.notes (owner_id, updated_at DESC) WHERE deleted_at IS NULL;
-- comments/attachments are read per task: ORDER BY note_id, created_at, id / note_id, id.
CREATE INDEX IF NOT EXISTS idx_note_comments_owner_note
    ON public.note_comments (owner_id, note_id, created_at);
CREATE INDEX IF NOT EXISTS idx_note_attachments_owner_note
    ON public.note_attachments (owner_id, note_id, id);
-- task_sessions: the reverse lookup "what has this session done", ORDER BY last_seen_at DESC.
CREATE INDEX IF NOT EXISTS idx_task_sessions_owner_session
    ON public.task_sessions (owner_id, session_id, last_seen_at DESC);
-- agents: Agents.List, ORDER BY created_at DESC.
CREATE INDEX IF NOT EXISTS idx_agents_owner_created_at
    ON public.agents (owner_id, created_at DESC);
-- chat_sessions: per agent, ORDER BY updated_at DESC, id DESC.
CREATE INDEX IF NOT EXISTS idx_chat_sessions_owner_agent
    ON public.chat_sessions (owner_id, agent_id, updated_at DESC);
-- chat_messages: per session, ORDER BY id / created_at, id.
CREATE INDEX IF NOT EXISTS idx_chat_messages_owner_session
    ON public.chat_messages (owner_id, session_id, id);
-- connector_grants: ListPendingGrants, ORDER BY requested_at, id.
CREATE INDEX IF NOT EXISTS idx_connector_grants_owner_requested
    ON public.connector_grants (owner_id, requested_at);
-- connector_audit: ListAudit, ORDER BY id DESC.
CREATE INDEX IF NOT EXISTS idx_connector_audit_owner_id
    ON public.connector_audit (owner_id, id DESC);

-- ---------------------------------------------------------------------------
-- 9. New tables, empty. No code reads or writes them in this PR.
-- ---------------------------------------------------------------------------

-- A link invite (accounts PR 5). The TOKEN is never stored — only its sha256,
-- shown once at creation. `claim_user_id` binds a redeem to an EXISTING user
-- (the admin bootstrap: a new GoTrue sub becomes user 1) instead of creating one.
CREATE TABLE IF NOT EXISTS public.invites (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    token_hash    BYTEA       NOT NULL UNIQUE,
    email         TEXT,
    role          TEXT        NOT NULL DEFAULT 'member',
    created_by    BIGINT      NOT NULL REFERENCES public.users (id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ NOT NULL,
    redeemed_at   TIMESTAMPTZ,
    redeemed_by   BIGINT      REFERENCES public.users (id),
    revoked_at    TIMESTAMPTZ,
    claim_user_id BIGINT      REFERENCES public.users (id),
    CONSTRAINT invites_role_check CHECK (role IN ('admin', 'member')),
    -- sha256 is 32 bytes; anything else is a plaintext token or a different hash.
    CONSTRAINT invites_token_hash_len CHECK (octet_length(token_hash) = 32),
    -- Redeemed means redeemed BY someone; the two columns move together.
    CONSTRAINT invites_redeemed_pair CHECK ((redeemed_at IS NULL) = (redeemed_by IS NULL))
);

-- Personal API tokens `mst_…` (accounts PR 6). Hash only, shown once.
CREATE TABLE IF NOT EXISTS public.api_tokens (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id      BIGINT      NOT NULL REFERENCES public.users (id),
    name         TEXT        NOT NULL DEFAULT '',
    token_hash   BYTEA       NOT NULL UNIQUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ,
    CONSTRAINT api_tokens_token_hash_len CHECK (octet_length(token_hash) = 32)
);

CREATE INDEX IF NOT EXISTS idx_api_tokens_user ON public.api_tokens (user_id, created_at DESC);

-- Who did what to accounts (invites, role changes, disables, token issue/revoke).
--
-- ⚠ `detail` IS JSONB, NOT TEXT — unlike connector_audit.detail. That column is
-- a truncated free-text note about one proxied request. These rows describe
-- heterogeneous admin actions whose fields differ per action (invite id + role,
-- role before/after, target user, token name), and the admin audit page filters
-- on them; JSONB keeps them structured and queryable, and refuses a malformed
-- value at write time instead of at read time. Never a secret, never a token.
--
-- user_id is NULL for actions with no principal (a system loop, a failed
-- anonymous redeem). `via` is how the actor authenticated (session, api-token,
-- agent-token, machine, system).
CREATE TABLE IF NOT EXISTS public.audit_log (
    id      BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    user_id BIGINT      REFERENCES public.users (id),
    via     TEXT        NOT NULL DEFAULT '',
    action  TEXT        NOT NULL,
    object  TEXT        NOT NULL DEFAULT '',
    detail  JSONB       NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX IF NOT EXISTS idx_audit_log_at   ON public.audit_log (at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_log_user ON public.audit_log (user_id, at DESC);
