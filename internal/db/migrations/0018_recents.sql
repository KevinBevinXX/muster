-- muster 0018 — RECENTS: the Agents-list entries a user has recently OPENED
-- (internal/recents), drawn as a "recently viewed" strip above the list. Per
-- user and server-stored, so the strip follows its owner across devices. An
-- entry names a dispatched agent (kind 'agent') or a mirrored tmux window
-- (kind 'mirror'), keyed exactly as 0017's pins are.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, IN 0017's SHAPE. owner_isolation
-- keeps one user from reading or writing another's rows; the agent an agent row
-- names is pinned to the same owner by 0015's plugin_rows_owner_is_agents_owner
-- trigger, run with 'nullable' (a mirror row names no agent).
--
-- 🔴 A VISIT IS AN UPSERT OF visited_at, NOT A ROW PER VISIT: one row per
-- (owner, subject), so opening the same window ten times is one entry, moved
-- to the front. clock_timestamp(), not now(), so two visits in one transaction
-- still order.
--
-- 🔴 BOUNDED PER OWNER (recents.MaxPerOwner): the store prunes the oldest rows
-- past the bound in the visit's own transaction. An agent row dies with its
-- agent (ON DELETE CASCADE); a mirror row whose window is gone is kept and not
-- drawn, and ages out under the bound.
--
-- 🔴 IT IS RE-APPLIED BY db.RLSOn (rollback.go) WHEN THE TABLE EXISTS, SO EVERY
-- STATEMENT IS IDEMPOTENT, and nothing here redefines an object 0015 defines.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.recents (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id       BIGINT      NOT NULL
                   DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                   REFERENCES public.users (id),
    kind           TEXT        NOT NULL,
    agent_id       BIGINT      REFERENCES public.agents (id) ON DELETE CASCADE,
    mirror_host    TEXT,
    mirror_session TEXT,
    mirror_window  TEXT,
    visited_at     TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT recents_kind_check CHECK (kind IN ('agent', 'mirror')),
    -- Exactly one subject, spelled by the kind's own columns (0017's rule).
    CONSTRAINT recents_subject_check CHECK (
        (kind = 'agent' AND agent_id IS NOT NULL
            AND mirror_host IS NULL AND mirror_session IS NULL AND mirror_window IS NULL)
        OR (kind = 'mirror' AND agent_id IS NULL
            AND mirror_host IS NOT NULL AND mirror_session IS NOT NULL AND mirror_window IS NOT NULL)),
    CONSTRAINT recents_mirror_host_cap CHECK (char_length(mirror_host) BETWEEN 1 AND 63),
    CONSTRAINT recents_mirror_session_cap CHECK (octet_length(mirror_session) BETWEEN 1 AND 128),
    CONSTRAINT recents_mirror_window_check CHECK (mirror_window = '' OR mirror_window ~ '^@[0-9]{1,10}$')
);

-- One row per (owner, subject): the upsert's conflict targets.
CREATE UNIQUE INDEX IF NOT EXISTS recents_owner_agent
    ON public.recents (owner_id, agent_id) WHERE kind = 'agent';
CREATE UNIQUE INDEX IF NOT EXISTS recents_owner_mirror
    ON public.recents (owner_id, mirror_host, mirror_session, mirror_window) WHERE kind = 'mirror';
-- The strip's read and the prune: one owner's rows, newest first.
CREATE INDEX IF NOT EXISTS idx_recents_owner_visited ON public.recents (owner_id, visited_at DESC, id DESC);
-- The cascade from agents looks rows up by agent.
CREATE INDEX IF NOT EXISTS idx_recents_agent ON public.recents (agent_id) WHERE agent_id IS NOT NULL;

DROP TRIGGER IF EXISTS recents_owner_is_agents_owner ON public.recents;
CREATE TRIGGER recents_owner_is_agents_owner BEFORE INSERT OR UPDATE OF owner_id, agent_id
    ON public.recents FOR EACH ROW EXECUTE FUNCTION public.plugin_rows_owner_is_agents_owner('nullable');

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.recents ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.recents ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.recents FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.recents;
CREATE POLICY owner_isolation ON public.recents
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

-- UPDATE as well as 0017's grants: a visit to an entry already recorded moves
-- its visited_at (ON CONFLICT DO UPDATE).
GRANT SELECT, INSERT, UPDATE, DELETE ON public.recents TO muster_app;
GRANT USAGE, SELECT ON SEQUENCE public.recents_id_seq TO muster_app;
