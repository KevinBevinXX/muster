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
    -- 0016 (plugin_reply_drafts), when this schema has it; same reason as 0011.
    IF to_regclass('public.plugin_reply_drafts') IS NOT NULL THEN
        ALTER TABLE public.plugin_reply_drafts NO FORCE ROW LEVEL SECURITY;
        ALTER TABLE public.plugin_reply_drafts DISABLE ROW LEVEL SECURITY;
        ALTER TABLE public.plugin_reply_drafts ALTER COLUMN owner_id SET DEFAULT 1;
    END IF;
    -- 0017 (pins), when this schema has it; same reason as 0011.
    IF to_regclass('public.pins') IS NOT NULL THEN
        ALTER TABLE public.pins NO FORCE ROW LEVEL SECURITY;
        ALTER TABLE public.pins DISABLE ROW LEVEL SECURITY;
        ALTER TABLE public.pins ALTER COLUMN owner_id SET DEFAULT 1;
    END IF;
    -- 0018 (recents), when this schema has it; same reason as 0011.
    IF to_regclass('public.recents') IS NOT NULL THEN
        ALTER TABLE public.recents NO FORCE ROW LEVEL SECURITY;
        ALTER TABLE public.recents DISABLE ROW LEVEL SECURITY;
        ALTER TABLE public.recents ALTER COLUMN owner_id SET DEFAULT 1;
    END IF;
    -- 0019 (send_queue), when this schema has it; same reason as 0011.
    IF to_regclass('public.send_queue') IS NOT NULL THEN
        ALTER TABLE public.send_queue NO FORCE ROW LEVEL SECURITY;
        ALTER TABLE public.send_queue DISABLE ROW LEVEL SECURITY;
        ALTER TABLE public.send_queue ALTER COLUMN owner_id SET DEFAULT 1;
    END IF;
    -- 0020 (agent_focus), when this schema has it; same reason as 0011.
    IF to_regclass('public.agent_focus') IS NOT NULL THEN
        ALTER TABLE public.agent_focus NO FORCE ROW LEVEL SECURITY;
        ALTER TABLE public.agent_focus DISABLE ROW LEVEL SECURITY;
        ALTER TABLE public.agent_focus ALTER COLUMN owner_id SET DEFAULT 1;
    END IF;
    -- 0021 (session_routes), when this schema has it; same reason as 0011.
    IF to_regclass('public.session_routes') IS NOT NULL THEN
        ALTER TABLE public.session_routes NO FORCE ROW LEVEL SECURITY;
        ALTER TABLE public.session_routes DISABLE ROW LEVEL SECURITY;
        ALTER TABLE public.session_routes ALTER COLUMN owner_id SET DEFAULT 1;
    END IF;
    -- 0022 (agent_focus_history), when this schema has it; same reason as 0011.
    IF to_regclass('public.agent_focus_history') IS NOT NULL THEN
        ALTER TABLE public.agent_focus_history NO FORCE ROW LEVEL SECURITY;
        ALTER TABLE public.agent_focus_history DISABLE ROW LEVEL SECURITY;
        ALTER TABLE public.agent_focus_history ALTER COLUMN owner_id SET DEFAULT 1;
    END IF;
    -- 0024 (uploads), when this schema has it; same reason as 0011.
    IF to_regclass('public.uploads') IS NOT NULL THEN
        ALTER TABLE public.uploads NO FORCE ROW LEVEL SECURITY;
        ALTER TABLE public.uploads DISABLE ROW LEVEL SECURITY;
        ALTER TABLE public.uploads ALTER COLUMN owner_id SET DEFAULT 1;
    END IF;
    -- 0025 (attention_asks), when this schema has it; same reason as 0011.
    IF to_regclass('public.attention_asks') IS NOT NULL THEN
        ALTER TABLE public.attention_asks NO FORCE ROW LEVEL SECURITY;
        ALTER TABLE public.attention_asks DISABLE ROW LEVEL SECURITY;
        ALTER TABLE public.attention_asks ALTER COLUMN owner_id SET DEFAULT 1;
    END IF;
    -- 0037 (jobs), when this schema has it; same reason as 0011.
    IF to_regclass('public.jobs') IS NOT NULL THEN
        ALTER TABLE public.jobs NO FORCE ROW LEVEL SECURITY;
        ALTER TABLE public.jobs DISABLE ROW LEVEL SECURITY;
        ALTER TABLE public.jobs ALTER COLUMN owner_id SET DEFAULT 1;
    END IF;
    -- 0037 (devices, device_pairing_codes), when this schema has them; same
    -- reason as 0011.
    IF to_regclass('public.devices') IS NOT NULL THEN
        ALTER TABLE public.devices NO FORCE ROW LEVEL SECURITY;
        ALTER TABLE public.devices DISABLE ROW LEVEL SECURITY;
        ALTER TABLE public.devices ALTER COLUMN owner_id SET DEFAULT 1;
    END IF;
    IF to_regclass('public.device_pairing_codes') IS NOT NULL THEN
        ALTER TABLE public.device_pairing_codes NO FORCE ROW LEVEL SECURITY;
        ALTER TABLE public.device_pairing_codes DISABLE ROW LEVEL SECURITY;
        ALTER TABLE public.device_pairing_codes ALTER COLUMN owner_id SET DEFAULT 1;
    END IF;
END
$$;

COMMIT;
