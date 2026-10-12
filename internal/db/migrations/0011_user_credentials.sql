-- muster 0011 — bring-your-own model credentials (accounts, after PR 3b).
--
-- The operator decision (accounts plan, "Operator decisions", 2026-10-11):
-- members BRING THEIR OWN Claude setup-tokens and OpenRouter API key,
-- stored sealed per owner; a member's agents only ever use that member's
-- credentials; the environment pool stays the admin's; and cc_account_marks
-- becomes per (owner, account).
--
-- 1. user_credentials — OWNED, in 0006's shape and under 0007's row-level
--    security (ENABLE + FORCE + the owner_isolation policy, owner_id defaulting
--    to the principal). One row per (owner, kind, name):
--      kind 'claude'      name = the account name (ccpool's [a-z0-9-] rule);
--                         secret_ciphertext = the sealed setup-token.
--      kind 'openrouter'  name = 'default' (one per user);
--                         secret_ciphertext = the sealed OpenRouter API key (an
--                         ordinary key, used as entered — operator decision on
--                         #69; no key is minted from it).
--    Ciphertexts are sealed with the owner, kind and name in the AEAD's
--    associated data (internal/credentials/seal.go), so a ciphertext moved to
--    another owner's row does not open. fingerprint is a 48-bit one-way hash
--    for display; nothing in this table is plaintext.
--
-- 2. cc_account_marks becomes OWNED: (owner_id, account) is the key. Until now
--    it was a SHARED table naming accounts of the one environment pool; with
--    per-member accounts an account NAME is only unique within its owner, so a
--    member's "work" and the admin's "work" are different accounts and must not
--    share a rate-limit mark. Every existing row is a mark on the environment
--    pool, which is the admin's (user 1) — backfilled so.
--
-- 🔴 ROLLBACK PAST THIS IMAGE LENDS THE ADMIN'S CREDENTIALS TO MEMBERS.
-- An image older than this one has no per-owner credentials: every spec it
-- builds — a dispatch, a Start, a model change, a chief provision — places the
-- ENVIRONMENT's Claude pool token and MUSTER_AGENT_OPENROUTER_KEY, whoever owns
-- the agent. With GoTrue live, members exist, so a rollback without the steps
-- below runs members' agents on the operator's subscriptions. (It also never
-- reads user_credentials, and its mark writes use `ON CONFLICT (account)`,
-- which no longer names a unique constraint, so marks stop being recorded.)
-- Before rolling the image back below this one, as the database owner:
--
--   1. List every member and their live agents (psql as `muster`, which
--      bypasses row-level security; the query is pinned by
--      TestTheRollbackQueryFindsExactlyTheMembersLiveAgents):
--
-- BEGIN rollback-member-agents
--   SELECT u.id AS user_id, u.email, a.id AS agent_id, a.kind, a.status,
--          a.namespace AS namespace, a.name AS deployment
--     FROM public.users u
--     LEFT JOIN public.agents a
--       ON a.owner_id = u.id AND a.status IN ('pending', 'provisioning', 'running')
--    WHERE u.role <> 'admin' AND u.disabled_at IS NULL
--    ORDER BY u.id, a.id;
-- END rollback-member-agents
--
--   2. Disable each member listed, so nobody can dispatch or Start on the old
--      image (`disabled_at` + GoTrue ban; the old image honours both):
--        kubectl -n muster exec deploy/muster -- /muster-server user disable <user_id>
--   3. Stop each listed agent — ONLY that agent's Deployment, never `--all`:
--        kubectl -n <namespace> scale deployment/<deployment> --replicas=0
--      <deployment> is the agent's name (the driver names the Deployment after
--      it). <namespace> is the row's in the default per-agent layout; with
--      MUSTER_AGENT_NAMESPACE_SHARED=true every agent — the admin's too — is in
--      MUSTER_AGENT_NAMESPACE instead, which is exactly why `--all` would stop
--      the admin's agents as well.
--   4. Roll the image back. Re-enable members only after rolling forward again
--      (`muster-server user enable <user_id>`).
--
-- rollback/rls_off.sql turns both tables' RLS off with the rest (both carry
-- owner_id, so --rls-off's read-back checks them), and --rls-on re-applies this
-- file, which is idempotent.
--
-- Convention: idempotent, one transaction (internal/db/migrate.go).

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.user_credentials (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id           BIGINT      NOT NULL
                       DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                       REFERENCES public.users (id),
    kind               TEXT        NOT NULL,
    name               TEXT        NOT NULL,
    secret_ciphertext  BYTEA       NOT NULL,
    fingerprint        TEXT        NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT user_credentials_owner_kind_name_key UNIQUE (owner_id, kind, name),
    CONSTRAINT user_credentials_kind_check CHECK (kind IN ('claude', 'openrouter')),
    CONSTRAINT user_credentials_name_check CHECK (
        (kind = 'openrouter' AND name = 'default')
        OR (kind = 'claude' AND name ~ '^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$')
    )
);

ALTER TABLE public.user_credentials ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

-- ---------------------------------------------------------------------------
-- cc_account_marks: shared → owned.
-- ---------------------------------------------------------------------------

ALTER TABLE public.cc_account_marks ADD COLUMN IF NOT EXISTS owner_id BIGINT;
UPDATE public.cc_account_marks SET owner_id = 1 WHERE owner_id IS NULL;
ALTER TABLE public.cc_account_marks ALTER COLUMN owner_id SET NOT NULL;
ALTER TABLE public.cc_account_marks ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'cc_account_marks_owner_id_fkey') THEN
        ALTER TABLE public.cc_account_marks
            ADD CONSTRAINT cc_account_marks_owner_id_fkey FOREIGN KEY (owner_id) REFERENCES public.users (id);
    END IF;
    -- The key moves from (account) to (owner_id, account).
    IF EXISTS (SELECT 1 FROM pg_constraint c
               WHERE c.conrelid = 'public.cc_account_marks'::regclass AND c.contype = 'p'
                 AND pg_get_constraintdef(c.oid) = 'PRIMARY KEY (account)') THEN
        EXECUTE (SELECT format('ALTER TABLE public.cc_account_marks DROP CONSTRAINT %I', c.conname)
                 FROM pg_constraint c
                 WHERE c.conrelid = 'public.cc_account_marks'::regclass AND c.contype = 'p');
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint c
                   WHERE c.conrelid = 'public.cc_account_marks'::regclass AND c.contype = 'p') THEN
        ALTER TABLE public.cc_account_marks
            ADD CONSTRAINT cc_account_marks_pkey PRIMARY KEY (owner_id, account);
    END IF;
END
$$;

-- ---------------------------------------------------------------------------
-- Row-level security on both: 0007's policy, verbatim.
-- ---------------------------------------------------------------------------

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['user_credentials', 'cc_account_marks'] LOOP
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

GRANT SELECT, INSERT, UPDATE, DELETE ON public.user_credentials TO muster_app;
GRANT USAGE, SELECT, UPDATE ON SEQUENCE public.user_credentials_id_seq TO muster_app;
