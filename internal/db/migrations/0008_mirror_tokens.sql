-- muster 0008 — per-host MIRROR TOKENS for the optional mirrored-agents plugin
-- (internal/mirror, MUSTER_MIRROR=1).
--
-- 🔴 THIS IS THE PLUGIN'S ONLY TABLE. A host's tmux sessions are NOT persisted:
-- the hub keeps each connected host's session list in memory, keyed by the
-- token's owner and host, and the Agents view renders them from there. Nothing
-- about a mirrored session ever becomes an agents row, so it can never be
-- dispatched, chatted with, deleted, or mistaken for an agent by name (a tmux
-- session called "chief" is not the chief).
--
-- 🔴 OWNED, IN 0006/0007's SHAPE, AND UNDER THE SAME ROW-LEVEL SECURITY.
-- owner_id is the user who minted the token (the operator, user 1, for
-- `muster-server mirror token create`), defaulting to the acting principal's
-- muster.uid exactly as 0007 set every other owned table up, so an insert with
-- no principal is refused rather than landing as somebody. A host's sessions are
-- visible only to that owner. Token AUTHENTICATION has to find a token by its
-- hash before anyone is known, so it runs under authz.System (scope 'all'); the
-- policy is otherwise the standard owner_isolation.
--
-- Only the sha256 of a token is stored; the token is printed once, at minting.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.mirror_tokens (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id     BIGINT      NOT NULL
                 DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                 REFERENCES public.users (id),
    host         TEXT        NOT NULL,
    token_hash   TEXT        NOT NULL UNIQUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at   TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    CONSTRAINT mirror_tokens_host_check CHECK (host ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$')
);

CREATE INDEX IF NOT EXISTS idx_mirror_tokens_owner ON public.mirror_tokens (owner_id);

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.mirror_tokens ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.mirror_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.mirror_tokens FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.mirror_tokens;
CREATE POLICY owner_isolation ON public.mirror_tokens
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

-- 0007's ALTER DEFAULT PRIVILEGES already covers a table the migrator creates;
-- stated explicitly so this table's access does not depend on which role ran
-- 0007.
GRANT SELECT, INSERT, UPDATE, DELETE ON public.mirror_tokens TO muster_app;
GRANT USAGE, SELECT, UPDATE ON SEQUENCE public.mirror_tokens_id_seq TO muster_app;
