-- muster 0013 — muster-native Web Push: per-owner push subscriptions and
-- per-owner notification preferences (internal/push).
--
-- Until this migration muster held NO push subscriptions: the browser
-- subscribed against the permission router (its VAPID key, its table), and
-- with MUSTER_STANDALONE=1 that router does not exist, so nothing reached
-- anyone. muster now signs with its OWN VAPID keypair (MUSTER_VAPID_*), and a
-- subscription is bound to the key it was made with — so these rows are new,
-- not copied from the router.
--
-- 1. push_subscriptions — OWNED, in 0008's shape and under 0007's row-level
--    security (ENABLE + FORCE + owner_isolation, owner_id defaulting to the
--    principal). One row per browser push endpoint.
--
--    🔴 endpoint IS UNIQUE ACROSS OWNERS, AND THAT IS THE SHARED-DEVICE CASE.
--    A browser has ONE subscription per origin, so when user B signs in on a
--    browser where user A subscribed, B presents A's endpoint. Under RLS B
--    cannot see A's row, so a plain upsert would fail on the unique index (or,
--    without it, the device would buzz for BOTH users — A's notifications on
--    B's screen). The fix is one System-ledgered CLAIM (internal/push's PGStore
--    .Claim): the endpoint moves to the presenting user, and A's row goes. The
--    owner is taken from the request principal, never from the body.
--
--    🔴 AN ENDPOINT IS A CAPABILITY. Anyone holding one can make that device
--    buzz with no further credential, which is why internal/push never logs
--    one whole (push.Redact / push.Short).
--
-- 2. push_preferences — OWNED, same policy. One row per owner holding the
--    notification events that owner has MUTED. Every event is ON by default
--    (operator decision), so a user with no row gets everything; a toggle on
--    /account adds or removes one name. The vocabulary is a Go constant set
--    (internal/push.Events); an unknown name in `muted` mutes nothing.
--
-- rollback/rls_off.sql turns both tables' RLS off with the rest (both carry
-- owner_id, so --rls-off's read-back checks them), and --rls-on re-applies this
-- file, which is idempotent.
--
-- Convention: idempotent, one transaction (internal/db/migrate.go).

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.push_subscriptions (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id    BIGINT      NOT NULL
                DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                REFERENCES public.users (id),
    endpoint    TEXT        NOT NULL UNIQUE,
    p256dh      TEXT        NOT NULL,
    auth        TEXT        NOT NULL,
    user_agent  TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_ok_at  TIMESTAMPTZ,
    fail_count  INTEGER     NOT NULL DEFAULT 0,
    CONSTRAINT push_subscriptions_endpoint_check CHECK (endpoint ~ '^https://[^/[:space:]]+/'),
    CONSTRAINT push_subscriptions_keys_check CHECK (p256dh <> '' AND auth <> '')
);

CREATE INDEX IF NOT EXISTS idx_push_subscriptions_owner ON public.push_subscriptions (owner_id);

CREATE TABLE IF NOT EXISTS public.push_preferences (
    owner_id    BIGINT      PRIMARY KEY
                DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                REFERENCES public.users (id),
    muted       TEXT[]      NOT NULL DEFAULT '{}',
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.push_subscriptions ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;
ALTER TABLE public.push_preferences ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['push_subscriptions', 'push_preferences'] LOOP
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
    END LOOP;
END
$$;

GRANT SELECT, INSERT, UPDATE, DELETE ON public.push_subscriptions TO muster_app;
GRANT USAGE, SELECT, UPDATE ON SEQUENCE public.push_subscriptions_id_seq TO muster_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON public.push_preferences TO muster_app;
