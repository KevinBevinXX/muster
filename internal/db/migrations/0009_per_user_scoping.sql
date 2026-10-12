-- muster 0009 — per-user scoping (multi-user accounts PR 3b of 6).
--
-- Two things, both small, both enforced by the database rather than by a
-- handler remembering:
--
-- 1. ONE RESERVED CHIEF PER OWNER. Every user gets their own chief (plan §7):
--    POST /chief/provision mints agents.ChiefNameFor(user) — `chief` for the
--    operator (user 1, unchanged) and `chief-<id>` for anyone else. agents.name
--    is already globally unique; this partial unique index adds that an owner
--    holds AT MOST ONE reserved chief slug, whatever minted it.
--
-- 2. ROW-LEVEL SECURITY ON THE USER-KEYED ACCOUNT TABLES (users, invites,
--    api_tokens, audit_log). 0007 (and 0008) left them policy-less because nothing read
--    them yet, so muster_app could read every row from any request. The first
--    reader arrives with this PR (the principal's ROLE, read from users.role),
--    and PR 5/6 add invites and personal tokens; the default must be closed
--    before they do. Same settings as 0007 (`muster.uid`, `muster.scope`):
--      users       a user reads ITS OWN row; nothing but scope-all writes
--                  (role/disable changes are /admin/* in PR 5, which runs with
--                  the admin scope, and redeeming an invite runs before any
--                  principal exists).
--      invites     scope-all only: admin surfaces and pre-auth redemption.
--      api_tokens  a user's own tokens (user_id), read and write.
--      audit_log   a user reads and appends its own entries; only scope-all
--                  rewrites or prunes.
--    "Admin sees all" is the scope-all arm, which only authz.System (and the
--    migrator) sets today: the operator decision keeps an admin's see-all to
--    /admin/* routes, and there are none yet. The api package reads a role
--    under authz.System for exactly that reason (internal/api/principal.go).
--
-- ⚠ NOT IN rollback/rls_off.sql, DELIBERATELY. That file exists so an image
-- built before 0007 can run; no image before this one reads any of these four
-- tables, so their policies cannot change an older image's behaviour, and an
-- `--rls-off` that also opened them would only widen what a rollback exposes.
SET LOCAL lock_timeout = '3s';

CREATE UNIQUE INDEX IF NOT EXISTS agents_one_chief_per_owner
    ON public.agents (owner_id)
    WHERE name = 'chief' OR name ~ '^chief-[1-9][0-9]*$';

ALTER TABLE public.users      ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.users      FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.invites    ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.invites    FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.api_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.api_tokens FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.audit_log  ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.audit_log  FORCE  ROW LEVEL SECURITY;

DROP POLICY IF EXISTS self_read ON public.users;
CREATE POLICY self_read ON public.users AS PERMISSIVE FOR SELECT TO PUBLIC
    USING (
        id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

DROP POLICY IF EXISTS user_isolation ON public.api_tokens;
CREATE POLICY user_isolation ON public.api_tokens AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        user_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        user_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

DROP POLICY IF EXISTS self_read ON public.audit_log;
CREATE POLICY self_read ON public.audit_log AS PERMISSIVE FOR SELECT TO PUBLIC
    USING (
        user_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );
DROP POLICY IF EXISTS self_append ON public.audit_log;
CREATE POLICY self_append ON public.audit_log AS PERMISSIVE FOR INSERT TO PUBLIC
    WITH CHECK (
        user_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['users', 'invites', 'api_tokens', 'audit_log'] LOOP
        EXECUTE format('DROP POLICY IF EXISTS scope_all ON public.%I', t);
        EXECUTE format($p$
            CREATE POLICY scope_all ON public.%I AS PERMISSIVE FOR ALL TO PUBLIC
                USING (current_setting('muster.scope', true) = 'all')
                WITH CHECK (current_setting('muster.scope', true) = 'all')$p$, t);
    END LOOP;
END
$$;
