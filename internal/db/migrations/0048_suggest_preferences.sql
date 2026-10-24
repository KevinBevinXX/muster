-- muster 0048 — SUGGESTED REPLIES' PER-USER TOGGLE (internal/suggest).
--
-- One row per owner who has turned suggested replies OFF (or back on after
-- turning them off). Suggestions are ON by default (operator decision), so a
-- user with no row gets them; the toggle writes `disabled`.
--
-- 🔴 NOTHING ELSE ABOUT SUGGESTIONS IS STORED. Their text lives only in the
-- server's in-memory cache, for at most an hour (suggest.CacheTTL); this table
-- holds a boolean and a time, and no column can hold a suggestion.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, in 0013's push_preferences
-- shape: owner_isolation keeps one user from reading or flipping another's
-- toggle. rollback/rls_off.sql turns it off with the rest, and --rls-on
-- re-applies this file (db.RLSOn), so every statement is idempotent.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.suggest_preferences (
    owner_id    BIGINT      PRIMARY KEY
                DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                REFERENCES public.users (id),
    disabled    BOOLEAN     NOT NULL DEFAULT false,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.suggest_preferences ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.suggest_preferences ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.suggest_preferences FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.suggest_preferences;
CREATE POLICY owner_isolation ON public.suggest_preferences
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.suggest_preferences TO muster_app;
