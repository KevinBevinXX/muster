-- muster 0037 — the `hermes` agent KIND (docs/design/hermes.md, "The hermes
-- agent kind"): a Hermes pod behind hermesd, on the OpenRouter credential the
-- gateway kind uses.
--
-- Only agents_kind_check widens. A hermes row names no Claude account, which
-- 0003's agents_cc_account_matches_kind already requires of every kind but
-- claude-code, and 0003's trigger keeps its kind fixed for life like every
-- other row's. internal/agents.Kinds is pinned to THIS list by
-- TestKindsMatchTheMigrationsCheck, which reads the newest migration that
-- re-creates the check.
--
-- Not re-applied by db.RLSOn (it changes no policy, grant or default), so it
-- need not be idempotent; it is anyway (DROP … IF EXISTS, then ADD).

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.agents DROP CONSTRAINT IF EXISTS agents_kind_check;
ALTER TABLE public.agents ADD CONSTRAINT agents_kind_check
    CHECK (kind IN ('gateway', 'claude-code', 'hermes'));
