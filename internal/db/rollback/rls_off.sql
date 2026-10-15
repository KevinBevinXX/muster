-- muster rollback: turn OFF migration 0007's row-level security.
--
-- WHEN: before rolling the muster IMAGE back below accounts PR 3a (the first
-- image carrying migration 0007_row_level_security.sql). An older image never
-- sets muster.uid, so against 0007's schema every INSERT it makes into an owned
-- table fails (owner_id defaults to NULL), and — unless it connects as a
-- superuser, which production's `muster` role is — it reads an empty board.
--
-- HOW: `/muster-migrate --rls-off` (embedded from this file; run it from an
-- image that HAS the flag, i.e. this one or newer, BEFORE switching the
-- Deployment's image), or paste this file into psql as the database owner:
--
--     kubectl -n muster exec -i deploy/muster-postgres -- \
--         psql -U muster -d muster -v ON_ERROR_STOP=1 < internal/db/rollback/rls_off.sql
--
-- 🔴 THE OLDER IMAGE'S /muster-migrate IGNORES THE FLAG. It predates --rls-off,
-- logs "schema is at version 7; nothing to apply", EXITS 0 — and RLS stays on
-- (measured on 0.11.0). So revert the image pin LAST: run --rls-off with a
-- 3a-or-newer binary (or the psql path) FIRST, then check it took:
--     select relrowsecurity from pg_class where relname='notes';   -- must be f
-- (The 3a+ binary prints this read-back for every owned table and exits non-zero
-- if any is wrong.)
--
-- WHAT IT DOES, per owned table: NO FORCE, DISABLE row-level security, and
-- owner_id DEFAULT 1 again (0006's single-user default). It leaves the policies,
-- the muster_app role, its grants and the SECURITY DEFINER function in place —
-- all inert with RLS disabled — and does NOT touch schema_migrations, so a
-- newer image does not re-run 0007 on boot. To turn RLS back ON, run 0007's
-- body again (it is idempotent): `/muster-migrate --rls-on`.
--
-- It is idempotent and runs in one transaction.

BEGIN;
SET LOCAL lock_timeout = '3s';

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'notes', 'note_comments', 'note_attachments', 'task_sessions', 'agents',
        'chat_sessions', 'chat_messages', 'connectors', 'connector_grants',
        'connector_audit', 'github_connection'
    ] LOOP
        EXECUTE format('ALTER TABLE public.%I NO FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE public.%I DISABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE public.%I ALTER COLUMN owner_id SET DEFAULT 1', t);
    END LOOP;
    -- 0008 (the mirror plugin's tokens), when this schema has it.
    IF to_regclass('public.mirror_tokens') IS NOT NULL THEN
        ALTER TABLE public.mirror_tokens NO FORCE ROW LEVEL SECURITY;
        ALTER TABLE public.mirror_tokens DISABLE ROW LEVEL SECURITY;
        ALTER TABLE public.mirror_tokens ALTER COLUMN owner_id SET DEFAULT 1;
    END IF;
    -- 0011 (bring-your-own credentials: user_credentials, and cc_account_marks
    -- made owned), when this schema has it. Both carry owner_id, so the
    -- read-back (RLSStatus) checks them with the rest; leaving them on would
    -- make --rls-off exit non-zero.
    IF to_regclass('public.user_credentials') IS NOT NULL THEN
        ALTER TABLE public.user_credentials NO FORCE ROW LEVEL SECURITY;
        ALTER TABLE public.user_credentials DISABLE ROW LEVEL SECURITY;
        ALTER TABLE public.user_credentials ALTER COLUMN owner_id SET DEFAULT 1;
        ALTER TABLE public.cc_account_marks NO FORCE ROW LEVEL SECURITY;
        ALTER TABLE public.cc_account_marks DISABLE ROW LEVEL SECURITY;
        ALTER TABLE public.cc_account_marks ALTER COLUMN owner_id SET DEFAULT 1;
    END IF;
    -- 0012 (browser flows: flow_jobs and flow_runner_tokens), when this schema
    -- has it. Both carry owner_id, so RLSStatus reads them back with the rest.
    IF to_regclass('public.flow_jobs') IS NOT NULL THEN
        ALTER TABLE public.flow_jobs NO FORCE ROW LEVEL SECURITY;
        ALTER TABLE public.flow_jobs DISABLE ROW LEVEL SECURITY;
        ALTER TABLE public.flow_jobs ALTER COLUMN owner_id SET DEFAULT 1;
        ALTER TABLE public.flow_runner_tokens NO FORCE ROW LEVEL SECURITY;
        ALTER TABLE public.flow_runner_tokens DISABLE ROW LEVEL SECURITY;
        ALTER TABLE public.flow_runner_tokens ALTER COLUMN owner_id SET DEFAULT 1;
    END IF;
    -- 0013 (muster-native Web Push: push_subscriptions, push_preferences),
    -- when this schema has it. Both carry owner_id; same reason as 0011.
    IF to_regclass('public.push_subscriptions') IS NOT NULL THEN
        ALTER TABLE public.push_subscriptions NO FORCE ROW LEVEL SECURITY;
        ALTER TABLE public.push_subscriptions DISABLE ROW LEVEL SECURITY;
        ALTER TABLE public.push_subscriptions ALTER COLUMN owner_id SET DEFAULT 1;
        ALTER TABLE public.push_preferences NO FORCE ROW LEVEL SECURITY;
        ALTER TABLE public.push_preferences DISABLE ROW LEVEL SECURITY;
        ALTER TABLE public.push_preferences ALTER COLUMN owner_id SET DEFAULT 1;
    END IF;
    -- 0014 (browser flows: agent requests), when this schema has it.
    IF to_regclass('public.flow_requests') IS NOT NULL THEN
        ALTER TABLE public.flow_requests NO FORCE ROW LEVEL SECURITY;
        ALTER TABLE public.flow_requests DISABLE ROW LEVEL SECURITY;
        ALTER TABLE public.flow_requests ALTER COLUMN owner_id SET DEFAULT 1;
    END IF;
    -- 0015 (plugins: plugins, plugin_messages), when this schema has it. Both
    -- carry owner_id; same reason as 0011.
    IF to_regclass('public.plugins') IS NOT NULL THEN
        ALTER TABLE public.plugins NO FORCE ROW LEVEL SECURITY;
        ALTER TABLE public.plugins DISABLE ROW LEVEL SECURITY;
        ALTER TABLE public.plugins ALTER COLUMN owner_id SET DEFAULT 1;
        ALTER TABLE public.plugin_messages NO FORCE ROW LEVEL SECURITY;
        ALTER TABLE public.plugin_messages DISABLE ROW LEVEL SECURITY;
        ALTER TABLE public.plugin_messages ALTER COLUMN owner_id SET DEFAULT 1;
    END IF;
END
$$;

COMMIT;
