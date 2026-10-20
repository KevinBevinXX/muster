-- muster 0037 — PAIRED PHONES: the native Android app's device credential and
-- its pairing codes (internal/devices; docs/design/android.md, server changes
-- 5, 6 and 8).
--
--   devices                one row per paired phone. Only the sha256 of its
--                          `mdv_` credential is stored. stepup_pubkey is the
--                          SubjectPublicKeyInfo DER of the EC P-256 key the app
--                          generated in the Android Keystore at pairing, with
--                          its type (biometric | biometric_or_credential) and
--                          reported Keystore security level (NULL, all three,
--                          for a phone with no acceptable key, which pairs
--                          read-only). paired_signin_at is the pairing
--                          session's SIGN-IN time: every request compares it
--                          with users.sessions_not_before and users.signed_out_at
--                          (devices.PGStore.Authenticate), so a disable, a
--                          password reset or a Sign out everywhere cuts the
--                          phone off on its next request.
--   device_pairing_codes   one-time codes a signed-in session mints and the app
--                          redeems for the credential: single use, 60 s, stored
--                          as a hash.
--   push_subscriptions     gains `kind` ('browser' | 'android') and the device a
--                          native subscription belongs to. Removing the device
--                          removes its subscription (ON DELETE CASCADE), and a
--                          device holds at most one.
--
-- 🔴 A DEVICE IS REMOVED BY DELETING ITS ROW, ON EVERY REVOCATION: Remove,
-- unpair, a disable, a password reset and Sign out everywhere
-- (internal/users.deleteDevicesTx). The design's schema listed a revoked_at
-- column; there is none, on purpose: a revoked-but-present row was what kept a
-- signed-out phone receiving pushes, counting toward the cap and listed as
-- live, and the cascade removes its push subscription only on a delete.
--
-- Both new tables are under FORCE row-level security, owner-isolated, exactly as
-- push_subscriptions (0013): a request sees only its own user's rows. The two
-- lookups that cannot run as a user — redeeming a code and authenticating a
-- credential, each of which is how the owner is LEARNED — run under System in
-- one ledgered helper (internal/devices.sys).

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.devices (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id          BIGINT      NOT NULL
                      DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                      REFERENCES public.users (id),
    name              TEXT        NOT NULL DEFAULT '',
    app_version       TEXT        NOT NULL DEFAULT '',
    token_sha256      BYTEA       NOT NULL UNIQUE,
    stepup_pubkey     BYTEA,
    stepup_key_type   TEXT,
    stepup_key_level  TEXT,
    paired_signin_at  TIMESTAMPTZ NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at      TIMESTAMPTZ,
    CONSTRAINT devices_token_sha256_check CHECK (octet_length(token_sha256) = 32),
    CONSTRAINT devices_name_check CHECK (char_length(name) <= 100),
    CONSTRAINT devices_app_version_check CHECK (char_length(app_version) <= 64),
    CONSTRAINT devices_stepup_pubkey_check CHECK (stepup_pubkey IS NULL OR octet_length(stepup_pubkey) BETWEEN 1 AND 512),
    -- A key always has its type and a non-software level; no key has neither
    -- (devices.checkKey is the same rule, with the PIN-key policy on top).
    CONSTRAINT devices_stepup_key_check CHECK (
        (stepup_pubkey IS NULL AND stepup_key_type IS NULL AND stepup_key_level IS NULL) OR
        (stepup_pubkey IS NOT NULL
         AND stepup_key_type IN ('biometric', 'biometric_or_credential')
         AND stepup_key_level IN ('trusted_environment', 'strongbox', 'unknown_secure'))),
    -- The composite key a push subscription's (device_id, owner_id) points at,
    -- so a subscription can never name another user's device (the 0006 rule).
    CONSTRAINT devices_id_owner_key UNIQUE (id, owner_id)
);

CREATE INDEX IF NOT EXISTS idx_devices_owner ON public.devices (owner_id);

CREATE TABLE IF NOT EXISTS public.device_pairing_codes (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id     BIGINT      NOT NULL
                 DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                 REFERENCES public.users (id),
    code_sha256  BYTEA       NOT NULL UNIQUE,
    signin_at    TIMESTAMPTZ NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL,
    used_at      TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT device_pairing_codes_code_sha256_check CHECK (octet_length(code_sha256) = 32)
);

CREATE INDEX IF NOT EXISTS idx_device_pairing_codes_owner ON public.device_pairing_codes (owner_id);

ALTER TABLE public.push_subscriptions
    ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'browser',
    ADD COLUMN IF NOT EXISTS device_id BIGINT;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'push_subscriptions_device_owner_fkey') THEN
        ALTER TABLE public.push_subscriptions ADD CONSTRAINT push_subscriptions_device_owner_fkey
            FOREIGN KEY (device_id, owner_id) REFERENCES public.devices (id, owner_id) ON DELETE CASCADE;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'push_subscriptions_kind_check') THEN
        -- A native subscription always names its device, and a browser one never does.
        ALTER TABLE public.push_subscriptions ADD CONSTRAINT push_subscriptions_kind_check
            CHECK ((kind = 'browser' AND device_id IS NULL) OR (kind = 'android' AND device_id IS NOT NULL));
    END IF;
END
$$;

CREATE UNIQUE INDEX IF NOT EXISTS idx_push_subscriptions_device ON public.push_subscriptions (device_id)
    WHERE device_id IS NOT NULL;

-- Re-stated so a re-run (db.RLSOn after rollback/rls_off.sql) restores them.
ALTER TABLE public.devices ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;
ALTER TABLE public.device_pairing_codes ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['devices', 'device_pairing_codes'] LOOP
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

GRANT SELECT, INSERT, UPDATE, DELETE ON public.devices TO muster_app;
GRANT USAGE, SELECT, UPDATE ON SEQUENCE public.devices_id_seq TO muster_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON public.device_pairing_codes TO muster_app;
GRANT USAGE, SELECT, UPDATE ON SEQUENCE public.device_pairing_codes_id_seq TO muster_app;
