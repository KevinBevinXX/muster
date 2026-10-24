-- muster 0049 — SUGGESTED REPLIES' PER-USER USAGE COUNTS (internal/suggest).
--
-- One row per user who has been shown a suggestion set: how many sets were
-- shown, how many a chip was tapped in (filled), how many were then sent, the
-- sum of the edit distances between what a tap filled and what was sent, and
-- whether the auto-pause paused the user (under 5% filled after 200 shown).
-- Persisted so a pause, and the Account readout, survive a restart.
--
-- 🔴 NUMBERS ONLY. No column can hold a suggestion's text, the session, the
-- window or anything from the conversation: the composer reports an event
-- name, a set's fingerprint and a distance, and only the counts land here
-- (the fingerprints that keep "a set counts once" stay in memory).
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, in 0048's shape:
-- owner_isolation keeps one user from reading or writing another's counts.
-- rollback/rls_off.sql turns it off with the rest, and --rls-on re-applies
-- this file (db.RLSOn), so every statement is idempotent.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.suggest_usage (
    owner_id    BIGINT      PRIMARY KEY
                DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                REFERENCES public.users (id),
    shown       BIGINT      NOT NULL DEFAULT 0 CHECK (shown >= 0),
    filled      BIGINT      NOT NULL DEFAULT 0 CHECK (filled >= 0),
    sent        BIGINT      NOT NULL DEFAULT 0 CHECK (sent >= 0),
    edit_sum    BIGINT      NOT NULL DEFAULT 0 CHECK (edit_sum >= 0),
    paused      BOOLEAN     NOT NULL DEFAULT false,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.suggest_usage ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.suggest_usage ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.suggest_usage FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.suggest_usage;
CREATE POLICY owner_isolation ON public.suggest_usage
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.suggest_usage TO muster_app;
