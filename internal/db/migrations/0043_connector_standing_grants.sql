-- muster 0043 — AN APPROVAL IS SHARED BY ALL OF THE OWNER'S AGENTS
-- (connector standing grants; docs/connectors.md "Shared approvals").
--
-- Until now a connector grant was per agent: every newly dispatched agent asked
-- again and its work blocked until the owner approved it, though the owner had
-- approved the same connector for their earlier agents. Now approving a request
-- also records a STANDING GRANT for the owner: a later request from ANY of the
-- owner's agents that matches it is approved at request time, without a pending
-- row anyone waits on.
--
--   connector_standing_grants   one row per shared approval
--     connector_name            the connector, by name (like connector_grants:
--                               not a foreign key; DeleteConnector deletes both)
--     source                    'stored' | 'app' (0033): which credential the
--                               approval was about
--     repo                      for source 'app' (the GitHub App's one-repository
--                               token) the agent's repository, lowercased — an
--                               App approval for one repository never approves
--                               another; '' for a stored credential, which is
--                               keyed on the connector alone
--     origin_grant_id           the per-agent grant whose approval created it (a
--                               display link, no foreign key: that grant goes
--                               with its agent)
--     revoked_at                set by a revoke on the Connectors tab; a revoked
--                               row is kept so the grants it approved still name it
--
--   connector_grants.standing_grant_id   the standing grant that approved this
--                               grant automatically; set exactly when
--                               decided_via = 'standing'
--
-- 🔴 THE KEY IS (owner, connector, source, repo), AND ONE LIVE ROW PER KEY
-- (connector_standing_grants_one_live). Source is in the key so the GitHub App
-- configured over a stored `github` token (or removed from under one) never
-- lets an approval of one credential approve the other — the same rule 0033
-- applies to a per-agent grant.
--
-- 🔴 PROVENANCE IS NEVER A FAKE HUMAN DECISION. An automatic approval is
-- decided_by 'standing-grant', decided_via 'standing', standing_grant_id the
-- row — and 0035's CHECK, replaced below, admits 'standing' only on an
-- APPROVED grant that names its standing grant and no mirror host or session.
-- Re-opening and revoking a grant clear standing_grant_id with the rest of the
-- provenance, in the same statement (connectors.PGStore).
--
-- Owned, under row-level security (owner_isolation, as 0037's jobs), and
-- re-applied by db.RLSOn. The composite foreign key (standing_grant_id,
-- owner_id) keeps a grant to a standing grant of the SAME owner.
--
-- Must apply after 0042 (the contiguity test refuses a gap). Idempotent.
--
-- ROLLBACK. An image from before 0043 never reads the table or the column, but
-- its RevokeGrant / ReopenGrant do not clear standing_grant_id, so the CHECK
-- refuses revoking or re-opening an automatically approved grant (a 500) until
-- the CHECK is put back to 0035's. Before rolling the image back:
--   UPDATE public.connector_grants SET decided_via = NULL, standing_grant_id = NULL
--       WHERE decided_via = 'standing' OR standing_grant_id IS NOT NULL;
--   ALTER TABLE public.connector_grants DROP CONSTRAINT IF EXISTS connector_grants_decided_via_check;
--   ALTER TABLE public.connector_grants ADD CONSTRAINT connector_grants_decided_via_check CHECK (
--       (decided_via IS NULL AND decided_via_host IS NULL AND decided_via_session IS NULL)
--       OR (decided_via IS NOT NULL AND decided_via = 'mcp'
--           AND status IN ('approved', 'denied')
--           AND decided_via_host IS NOT NULL AND decided_via_session IS NOT NULL
--           AND decided_via_host ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'
--           AND decided_via_session ~ '^[0-9A-Za-z][0-9A-Za-z_-]{0,63}$'));
-- (the approvals stay; only where they came from is lost — the audit lines keep
-- it). The older image then asks again per agent, as it always did. To remove
-- the schema too: drop connector_grants_standing_grant_owner_fkey, the column
-- and the table, and delete schema_migrations row 43.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.connector_standing_grants (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id        BIGINT      NOT NULL
                    DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                    REFERENCES public.users (id),
    connector_name  TEXT        NOT NULL,
    source          TEXT        NOT NULL,
    repo            TEXT        NOT NULL DEFAULT '',
    origin_grant_id BIGINT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at      TIMESTAMPTZ,
    CONSTRAINT connector_standing_grants_id_owner UNIQUE (id, owner_id),
    -- connectors.ValidateName.
    CONSTRAINT connector_standing_grants_name_check
        CHECK (connector_name ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
    CONSTRAINT connector_standing_grants_source_check CHECK (source IN ('stored', 'app')),
    -- An App approval names exactly one repository; a stored one names none.
    CONSTRAINT connector_standing_grants_repo_check CHECK (
        (source = 'app' AND repo <> '' AND repo = lower(repo) AND octet_length(repo) <= 256)
        OR (source = 'stored' AND repo = ''))
);

CREATE UNIQUE INDEX IF NOT EXISTS connector_standing_grants_one_live
    ON public.connector_standing_grants (owner_id, connector_name, source, repo) WHERE revoked_at IS NULL;

ALTER TABLE public.connector_standing_grants ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.connector_standing_grants ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.connector_standing_grants FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.connector_standing_grants;
CREATE POLICY owner_isolation ON public.connector_standing_grants
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.connector_standing_grants TO muster_app;
GRANT USAGE, SELECT ON SEQUENCE public.connector_standing_grants_id_seq TO muster_app;

ALTER TABLE public.connector_grants ADD COLUMN IF NOT EXISTS standing_grant_id BIGINT;

ALTER TABLE public.connector_grants DROP CONSTRAINT IF EXISTS connector_grants_standing_grant_owner_fkey;
ALTER TABLE public.connector_grants ADD CONSTRAINT connector_grants_standing_grant_owner_fkey
    FOREIGN KEY (standing_grant_id, owner_id) REFERENCES public.connector_standing_grants (id, owner_id);

CREATE INDEX IF NOT EXISTS idx_connector_grants_standing
    ON public.connector_grants (standing_grant_id) WHERE standing_grant_id IS NOT NULL;

ALTER TABLE public.connector_grants DROP CONSTRAINT IF EXISTS connector_grants_decided_via_check;
ALTER TABLE public.connector_grants
    ADD CONSTRAINT connector_grants_decided_via_check CHECK (
        (decided_via IS NULL AND decided_via_host IS NULL AND decided_via_session IS NULL
            AND standing_grant_id IS NULL)
        -- 🔴 EVERY TERM IS NULL-SAFE (see 0035): a CHECK passes on NULL.
        OR (decided_via IS NOT NULL AND decided_via = 'mcp'
            AND status IN ('approved', 'denied')
            AND decided_via_host IS NOT NULL AND decided_via_session IS NOT NULL
            AND decided_via_host ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'
            AND decided_via_session ~ '^[0-9A-Za-z][0-9A-Za-z_-]{0,63}$'
            AND standing_grant_id IS NULL)
        OR (decided_via IS NOT NULL AND decided_via = 'standing'
            AND status = 'approved'
            AND standing_grant_id IS NOT NULL
            AND decided_via_host IS NULL AND decided_via_session IS NULL));
