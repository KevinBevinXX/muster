-- muster 0017 — PINS: the Agents-list entries a user has pinned to the top
-- (internal/pins). Per user and server-stored, so a pin follows its owner
-- across devices. A pin names either a dispatched agent (kind 'agent') or a
-- mirrored tmux window (kind 'mirror').
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, IN 0015's SHAPE. owner_isolation
-- is what keeps one user from reading, pinning or unpinning another's rows; the
-- agent an agent pin names is pinned to the same owner by 0015's
-- plugin_rows_owner_is_agents_owner trigger, run here with 'nullable' (a mirror
-- pin names no agent).
--
-- 🔴 AN AGENT PIN DIES WITH ITS AGENT (ON DELETE CASCADE). A re-dispatched agent
-- of the same name is a new row and is not pinned.
--
-- 🔴 A MIRROR PIN IS KEYED ON THE WINDOW'S ADDRESS: (host, session, tmux window
-- id "@n"), with window '' for a per-session host's entry — exactly the
-- address the card links by (ui.MirrorRef). Never the window INDEX, which tmux
-- renumbers. The id is stable for the life of the tmux SERVER, so the pin
-- survives a list refresh, a muster restart and a muster-mirror reconnect. A
-- tmux-server restart (a reboot) reissues ids from the start, so the pin then
-- names whatever window holds that id in a same-named session, or nothing.
-- The wire carries no per-window identity that outlives the tmux server (the
-- Claude session id is not in the window list), so no rebind hint is stored.
-- A pin whose window is not listed is kept and simply not drawn (dormant).
--
-- 🔴 IT IS RE-APPLIED BY db.RLSOn (rollback.go) WHEN THE TABLE EXISTS, SO EVERY
-- STATEMENT IS IDEMPOTENT, and nothing here redefines an object 0015 defines.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.pins (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id       BIGINT      NOT NULL
                   DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                   REFERENCES public.users (id),
    kind           TEXT        NOT NULL,
    agent_id       BIGINT      REFERENCES public.agents (id) ON DELETE CASCADE,
    mirror_host    TEXT,
    mirror_session TEXT,
    mirror_window  TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT pins_kind_check CHECK (kind IN ('agent', 'mirror')),
    -- Exactly one subject, spelled by the kind's own columns.
    CONSTRAINT pins_subject_check CHECK (
        (kind = 'agent' AND agent_id IS NOT NULL
            AND mirror_host IS NULL AND mirror_session IS NULL AND mirror_window IS NULL)
        OR (kind = 'mirror' AND agent_id IS NULL
            AND mirror_host IS NOT NULL AND mirror_session IS NOT NULL AND mirror_window IS NOT NULL)),
    -- The wire's own bounds (internal/mirror ValidHost / ValidSession /
    -- ValidWindowID); the store validates the spelling first.
    CONSTRAINT pins_mirror_host_cap CHECK (char_length(mirror_host) BETWEEN 1 AND 63),
    CONSTRAINT pins_mirror_session_cap CHECK (octet_length(mirror_session) BETWEEN 1 AND 128),
    CONSTRAINT pins_mirror_window_check CHECK (mirror_window = '' OR mirror_window ~ '^@[0-9]{1,10}$')
);

-- One pin per (owner, subject): pinning twice is a no-op, not a second row.
CREATE UNIQUE INDEX IF NOT EXISTS pins_owner_agent
    ON public.pins (owner_id, agent_id) WHERE kind = 'agent';
CREATE UNIQUE INDEX IF NOT EXISTS pins_owner_mirror
    ON public.pins (owner_id, mirror_host, mirror_session, mirror_window) WHERE kind = 'mirror';
-- The cascade from agents looks pins up by agent.
CREATE INDEX IF NOT EXISTS idx_pins_agent ON public.pins (agent_id) WHERE agent_id IS NOT NULL;

DROP TRIGGER IF EXISTS pins_owner_is_agents_owner ON public.pins;
CREATE TRIGGER pins_owner_is_agents_owner BEFORE INSERT OR UPDATE OF owner_id, agent_id
    ON public.pins FOR EACH ROW EXECUTE FUNCTION public.plugin_rows_owner_is_agents_owner('nullable');

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.pins ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.pins ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.pins FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.pins;
CREATE POLICY owner_isolation ON public.pins
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, DELETE ON public.pins TO muster_app;
GRANT USAGE, SELECT ON SEQUENCE public.pins_id_seq TO muster_app;
