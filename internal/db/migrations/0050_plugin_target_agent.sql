-- muster 0050 — A PLUGIN NAMES THE AGENT ITS MESSAGES GO TO (internal/plugins).
--
-- muster has no privileged agent any more. Delivery used to resolve its
-- recipient by LABEL — the owner's lowest-id agent whose display name was
-- "chief" — so a plugin's messages went wherever that label happened to sit,
-- and relabelling an agent silently moved every plugin's inbox. The recipient
-- is now a CHOICE, stored per plugin.
--
-- 🔴 THE TWO OLD COLUMNS WERE NEVER THE CHOICE, WHICH IS WHY THEY ARE RENAMED
-- RATHER THAN REUSED. chief_agent_id/chief_session_id are DELIVERY
-- BOOKKEEPING: the Deliverer writes them AFTER a turn to record which agent and
-- which chat session that message landed in, so a routed reply can be handed
-- back to the same conversation (internal/api/session_routes.go). A column the
-- deliverer overwrites on every turn cannot also hold what the operator picked.
-- So they become session_agent_id/session_id, and the choice gets a column of
-- its own.
--
-- ⚠ target_agent_id IS NULLABLE AND STARTS NULL, INCLUDING FOR PLUGINS THAT
-- WERE WORKING BEFORE THIS MIGRATION. There is no honest way to backfill it:
-- the old recipient was whatever carried a mutable label at delivery time, and
-- that is not recoverable from this table. A plugin with no target leaves its
-- messages QUEUED with reason `no_target` — the same shape as a deployment with
-- no deliverer wired — and the retry seam picks them up once a target is set.
-- Nothing is lost and nothing is sent to the wrong agent; after upgrading,
-- pick a target for each plugin on the plugins page.
--
-- ⚠ NO FOREIGN KEY, matching the columns it sits beside: a deleted agent is
-- simply not reused, and delivery re-reads the agent row anyway (it must, to
-- check the owner and that the pod is running). A FK would also make deleting
-- an agent fail on a plugin still pointing at it, and nothing protects any
-- agent from deletion now.

SET LOCAL lock_timeout = '3s';

-- The renames are guarded, not bare: ALTER ... RENAME COLUMN has no IF EXISTS,
-- and this file must survive being applied twice (db.RLSOn re-applies).
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_schema = 'public' AND table_name = 'plugins'
                 AND column_name = 'chief_agent_id') THEN
        ALTER TABLE public.plugins RENAME COLUMN chief_agent_id TO session_agent_id;
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_schema = 'public' AND table_name = 'plugins'
                 AND column_name = 'chief_session_id') THEN
        ALTER TABLE public.plugins RENAME COLUMN chief_session_id TO session_id;
    END IF;
END $$;

ALTER TABLE public.plugins ADD COLUMN IF NOT EXISTS target_agent_id BIGINT;
