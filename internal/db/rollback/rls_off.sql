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
END
$$;

COMMIT;
