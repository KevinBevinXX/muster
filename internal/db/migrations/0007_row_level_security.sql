-- muster 0007 — row-level security (multi-user accounts PR 3a of 6).
--
-- Every row in an OWNED table (0006's list) is visible to, and writable by,
-- exactly one user: the one whose id the connection's `muster.uid` setting
-- names. internal/db sets that setting — and `muster.scope` — on EVERY pool
-- acquire from the request's authz.Principal (internal/db/scope.go), and
-- clears them on release. A context with no principal gets empty settings,
-- which select NO row and make every INSERT fail (owner_id defaults to NULL,
-- which no policy's WITH CHECK admits — 42501; NOT NULL backs it up):
-- this file FAILS CLOSED.
--
-- 🔴 THE PRODUCTION ROLE IS A SUPERUSER, AND A SUPERUSER BYPASSES ROW-LEVEL
-- SECURITY UNCONDITIONALLY — FORCE INCLUDED. Measured on the live database
-- (2026-10-12): `muster` is rolsuper AND rolbypassrls, and it is the bootstrap
-- role (oid 10; the postgres image's POSTGRES_USER), which Postgres refuses to
-- demote. FORCE alone would therefore be INERT in production: every policy
-- below would be skipped for every query the application makes. So this file
-- also creates `muster_app` — NOLOGIN, NOSUPERUSER, NOBYPASSRLS — and the pool
-- switches every application connection to it (`set_config('role', ...)`) on
-- every acquire. RLS applies to muster_app because it is NOT the tables' owner
-- and holds no bypass. FORCE is still set, so a deployment whose LOGIN role is
-- an ordinary table OWNER (not a superuser) is covered too.
--   Consequences, all deliberate:
--   * psql / pg_dump / the backup CronJob connect as `muster` and do NOT set the
--     role, so they still see (and dump) every row. Backups are unaffected.
--   * The migration runner does NOT switch role (it needs DDL), and runs with
--     muster.scope = 'all' (internal/db/migrate.go).
--   * muster_app is granted DML on every table in `public` and USAGE on every
--     sequence, now and — via ALTER DEFAULT PRIVILEGES — for every table a later
--     migration creates. Shared tables (cc_account_marks, agent_retired_names)
--     and the not-yet-used account tables carry no policy: muster_app reads
--     and writes them as before.
--
-- OWNED TABLES (ENABLE + FORCE + one policy each) — 0006's list, all eleven:
--   notes, note_comments, note_attachments, task_sessions, agents,
--   chat_sessions, chat_messages, connectors, connector_grants,
--   connector_audit, github_connection.
-- Pinned as a ledger by TestRLSCoversExactlyTheOwnedTables (fails if any table
-- with an owner_id column lacks FORCE + the policy, or if a policy appears on
-- a table that has none).
--
-- THE POLICY (USING and WITH CHECK, FOR ALL commands):
--   owner_id = nullif(current_setting('muster.uid', true), '')::bigint
--   OR current_setting('muster.scope', true) = 'all'
-- `muster.scope = 'all'` is set ONLY for authz.System (the background loops)
-- and the migration runner. The operator's own browser session is uid 1 with
-- scope '' — it is NOT 'all' — so it sees user 1's rows, which today is every
-- row. Admin "see all" belongs to /admin/* routes; none exist yet.
--
-- OWNER_ID DEFAULT. 0006's temporary `DEFAULT 1` becomes the principal's id:
--   nullif(current_setting('muster.uid', true), '')::bigint
-- so an INSERT with no principal (or as System, which names no user) yields
-- NULL and is refused (WITH CHECK, then NOT NULL) rather than landing as
-- somebody. Inserts into CHILD
-- tables name owner_id from the parent row (internal/notes, internal/agents,
-- internal/connectors), because the composite FKs from 0006 already require
-- child.owner = parent.owner, and a background loop writing a chat message for
-- an agent has no user of its own to default to.
--
-- SECURITY DEFINER FUNCTIONS — the complete list, each tested:
--   muster_agent_name_taken(text) -> boolean
--     agents.name is ONE GLOBAL NAMESPACE (it becomes a Kubernetes namespace),
--     but under RLS a user can only see their own agents. The name-taken check
--     must see everybody's names; it returns a boolean and nothing else.
--     It runs as the function's owner AND sets muster.scope = 'all' for its own
--     duration, so it also works where the owner is an ordinary (FORCE-bound)
--     table owner rather than a superuser. search_path is pinned to pg_catalog
--     and every relation is schema-qualified (0002's hijack-guard convention).
--   (0002's retire-name trigger function is the other SECURITY DEFINER function
--    in the schema. It is unchanged: it writes agent_retired_names, a SHARED
--    table with no policy, so row-level security does not reach it.)
-- (The per-agent token lookup — how an agent's request learns its owner — runs
--  under authz.System in internal/api, not through a function; it is listed in
--  the System ledger in internal/authz/ledger_test.go.)
--
-- ⚠ ROLLBACK HAZARD. An image built before this migration does not set
-- muster.uid, so against this schema EVERY INSERT it makes into an owned table
-- fails (owner_id NULL), and — if it connects as anything but a superuser — it
-- reads an empty board. (In production it connects as the superuser `muster`,
-- which bypasses the policies, so the board stays visible but every create
-- fails.) Before rolling the image back below this migration, run
--     /muster-migrate --rls-off      (from THIS image; see internal/db/rollback)
-- which disables RLS and restores DEFAULT 1. Re-running this file re-enables it.
--
-- Convention: idempotent (IF [NOT] EXISTS, DROP ... IF EXISTS before CREATE),
-- one transaction (internal/db/migrate.go).

SET LOCAL lock_timeout = '3s';

-- ---------------------------------------------------------------------------
-- 1. The application role. Roles are CLUSTER-global, so another database on
--    the same server (a test database, a scratch copy) may already have created
--    it — or be creating it right now, hence the duplicate_object handler.
-- ---------------------------------------------------------------------------

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'muster_app') THEN
        BEGIN
            CREATE ROLE muster_app NOLOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT;
        EXCEPTION WHEN duplicate_object THEN
            NULL;
        END;
    END IF;
END
$$;

-- The login role must be able to SET ROLE to it. A superuser always can; an
-- ordinary owner needs the membership.
DO $$
BEGIN
    IF NOT pg_has_role(current_user, 'muster_app', 'MEMBER') THEN
        EXECUTE format('GRANT muster_app TO %I', current_user);
    END IF;
END
$$;

GRANT USAGE ON SCHEMA public TO muster_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO muster_app;
GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA public TO muster_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO muster_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO muster_app;

-- ---------------------------------------------------------------------------
-- 2. RLS on every owned table: ENABLE + FORCE, one policy, owner_id default.
-- ---------------------------------------------------------------------------

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'notes', 'note_comments', 'note_attachments', 'task_sessions', 'agents',
        'chat_sessions', 'chat_messages', 'connectors', 'connector_grants',
        'connector_audit', 'github_connection'
    ] LOOP
        EXECUTE format('ALTER TABLE public.%I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE public.%I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS owner_isolation ON public.%I', t);
        EXECUTE format($p$
            CREATE POLICY owner_isolation ON public.%I
                AS PERMISSIVE FOR ALL TO PUBLIC
                USING (
                    owner_id = nullif(current_setting('muster.uid', true), '')::bigint
                    OR current_setting('muster.scope', true) = 'all'
                )
                WITH CHECK (
                    owner_id = nullif(current_setting('muster.uid', true), '')::bigint
                    OR current_setting('muster.scope', true) = 'all'
                )$p$, t);
        EXECUTE format($d$
            ALTER TABLE public.%I ALTER COLUMN owner_id
                SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint$d$, t);
    END LOOP;
END
$$;

-- ---------------------------------------------------------------------------
-- 3. SECURITY DEFINER functions (see the header for the complete list).
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION public.muster_agent_name_taken(candidate text)
    RETURNS boolean
    LANGUAGE sql
    STABLE
    SECURITY DEFINER
    SET search_path = pg_catalog
    SET muster.scope = 'all'
AS $$
    SELECT EXISTS (SELECT 1 FROM public.agents WHERE name = candidate)
        OR EXISTS (SELECT 1 FROM public.agent_retired_names WHERE name = candidate)
$$;

-- EXECUTE to PUBLIC, deliberately (CREATE FUNCTION's default, stated): the
-- function answers one boolean about the GLOBAL agent-name namespace, which
-- 0002 already exposes to PUBLIC for retired names (GRANT SELECT ... TO PUBLIC),
-- and which any role able to INSERT an agent learns anyway from the UNIQUE
-- index. Restricting it to muster_app would break a deployment whose
-- connecting role is not a member — the exact case 0002's
-- TestANonOwnerRoleCanBothRetireANameAndReadTheLedger pins.
GRANT EXECUTE ON FUNCTION public.muster_agent_name_taken(text) TO PUBLIC;
