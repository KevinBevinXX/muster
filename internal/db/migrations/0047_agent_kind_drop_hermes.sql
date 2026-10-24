-- muster 0047 — the `hermes` agent KIND is removed: agents_kind_check narrows
-- back to ('gateway', 'claude-code'), 0003's original list. 0038 (which widened
-- it) is left as it is; this migration undoes its effect.
--
-- 🔴 IT REFUSES, LOUDLY, WHILE ANY hermes ROW EXISTS. The application no longer
-- knows the kind, so such a row could neither be started nor rebuilt; the
-- operator destroys every hermes agent BEFORE this migration runs (with a
-- build that still has the kind). The explicit check below names the count and
-- what to do; without it the ADD CONSTRAINT would still fail (23514), but with
-- a message that names no remedy. Both run in the migration's transaction, so a
-- refusal changes nothing.
--
-- internal/agents.Kinds is pinned to THIS list by
-- TestKindsMatchTheMigrationsCheck, which reads the newest migration that
-- re-creates the check.
--
-- Not re-applied by db.RLSOn (it changes no policy, grant or default).

SET LOCAL lock_timeout = '3s';

DO $$
DECLARE n bigint;
BEGIN
    SELECT count(*) INTO n FROM public.agents WHERE kind = 'hermes';
    IF n > 0 THEN
        RAISE EXCEPTION 'migration 0047: % agent row(s) have kind ''hermes'', which this build removed; destroy them (with a build that still has the kind) and migrate again', n
            USING ERRCODE = 'check_violation';
    END IF;
END $$;

ALTER TABLE public.agents DROP CONSTRAINT IF EXISTS agents_kind_check;
ALTER TABLE public.agents ADD CONSTRAINT agents_kind_check
    CHECK (kind IN ('gateway', 'claude-code'));
