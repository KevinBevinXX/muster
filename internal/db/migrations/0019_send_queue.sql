-- muster 0019 — THE SEND QUEUE (internal/sendqueue): messages a user queued
-- for a target, delivered by the server ONE AT A TIME, each only once the
-- target has been idle for a while. A target is a dispatched agent (kind
-- 'agent') or a mirrored tmux window (kind 'mirror'). The queue is server-side
-- and per user, so it survives the page closing and is visible (and
-- cancellable) from any of the owner's devices.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, IN 0017's SHAPE. owner_isolation
-- keeps one user from reading, queueing into or cancelling another's rows; the
-- agent an agent row names is pinned to the same owner by 0015's
-- plugin_rows_owner_is_agents_owner trigger, run with 'nullable' (a mirror row
-- names no agent). The dispatcher reads every owner's heads under the system
-- scope and DELIVERS each row as its owner (internal/api).
--
-- 🔴 A ROW IS CLAIMED BY A STATE TRANSITION, AND A CLAIM IS NEVER RETRIED.
-- queued → sending happens in one UPDATE conditional on the row still being
-- queued; at most one row per target is 'sending' (the two partial unique
-- indexes), so two dispatchers cannot both deliver one target's head. A row
-- left 'sending' by a process that died mid-send is moved to 'failed' at the
-- next start (with may_have_delivered = true), never back to 'queued': the
-- message may have been pasted, and pasting it twice is the worse failure.
--
-- 🔴 stepup_at IS THE STEP-UP DECISION, STORED. A mirror send types into the
-- operator's session and needs a FRESH sign-in (internal/stepup). It is checked
-- when the message is QUEUED, and the sign-in time of the session that queued it
-- is recorded here; the delivery presents exactly that time to the same gate, so
-- a queued message is sent only while that sign-in is still within the step-up
-- window — after that it FAILS, saying so, and is not sent.
--
-- 🔴 IT IS RE-APPLIED BY db.RLSOn (rollback.go) WHEN THE TABLE EXISTS, SO EVERY
-- STATEMENT IS IDEMPOTENT, and nothing here redefines an object 0015 defines.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.send_queue (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id           BIGINT      NOT NULL
                       DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                       REFERENCES public.users (id),
    kind               TEXT        NOT NULL,
    agent_id           BIGINT      REFERENCES public.agents (id) ON DELETE CASCADE,
    -- The chat session the message was typed in; NULL = the agent's latest.
    -- No foreign key: a deleted session is simply not used (the latest is).
    agent_session      BIGINT,
    mirror_host        TEXT,
    mirror_session     TEXT,
    mirror_window      TEXT,
    -- Which pane of the window a mirror message may land in: 'claude' (the
    -- Rendered view's composer: a Claude Code pane only) or 'active' (the
    -- Terminal view's: the window's active pane).
    mirror_pane        TEXT,
    body               TEXT        NOT NULL,
    stepup_at          TIMESTAMPTZ,
    state              TEXT        NOT NULL DEFAULT 'queued',
    detail             TEXT        NOT NULL DEFAULT '',
    -- Whether a send was ATTEMPTED far enough that the message may have
    -- reached the target: what the next message waits behind (a turn).
    may_have_delivered BOOLEAN     NOT NULL DEFAULT false,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_at         TIMESTAMPTZ,
    finished_at        TIMESTAMPTZ,
    CONSTRAINT send_queue_kind_check CHECK (kind IN ('agent', 'mirror')),
    CONSTRAINT send_queue_subject_check CHECK (
        (kind = 'agent' AND agent_id IS NOT NULL
            AND mirror_host IS NULL AND mirror_session IS NULL AND mirror_window IS NULL
            AND mirror_pane IS NULL)
        OR (kind = 'mirror' AND agent_id IS NULL AND agent_session IS NULL
            AND mirror_host IS NOT NULL AND mirror_session IS NOT NULL AND mirror_window IS NOT NULL
            AND mirror_pane IN ('claude', 'active') AND stepup_at IS NOT NULL)),
    -- The wire's own bounds (internal/mirror ValidHost / ValidSession /
    -- ValidWindowID), as 0017 spells them; the store validates first.
    CONSTRAINT send_queue_mirror_host_cap CHECK (char_length(mirror_host) BETWEEN 1 AND 63),
    CONSTRAINT send_queue_mirror_session_cap CHECK (octet_length(mirror_session) BETWEEN 1 AND 128),
    CONSTRAINT send_queue_mirror_window_check CHECK (mirror_window = '' OR mirror_window ~ '^@[0-9]{1,10}$'),
    -- An agent message is bounded by the send form (256 KiB); a mirror one by
    -- mirror.MaxPaste (16 KiB).
    CONSTRAINT send_queue_body_cap CHECK (octet_length(body) BETWEEN 1 AND 262144
        AND (kind = 'agent' OR octet_length(body) <= 16384)),
    CONSTRAINT send_queue_state_check CHECK (state IN ('queued', 'sending', 'sent', 'failed', 'cancelled')),
    CONSTRAINT send_queue_detail_cap CHECK (octet_length(detail) <= 1024),
    CONSTRAINT send_queue_claimed_check CHECK (state IN ('queued', 'cancelled') OR claimed_at IS NOT NULL),
    CONSTRAINT send_queue_finished_check CHECK ((state IN ('queued', 'sending')) = (finished_at IS NULL))
);

-- At most ONE row per target is being sent: the claim's own guarantee, held by
-- the schema so that two dispatchers cannot both claim a target's head.
CREATE UNIQUE INDEX IF NOT EXISTS send_queue_one_sending_agent
    ON public.send_queue (owner_id, agent_id) WHERE kind = 'agent' AND state = 'sending';
CREATE UNIQUE INDEX IF NOT EXISTS send_queue_one_sending_mirror
    ON public.send_queue (owner_id, mirror_host, mirror_session, mirror_window)
    WHERE kind = 'mirror' AND state = 'sending';
-- The dispatcher's scan: every target's open rows, oldest first.
CREATE INDEX IF NOT EXISTS idx_send_queue_open ON public.send_queue (id) WHERE state IN ('queued', 'sending');
-- The cascade from agents looks rows up by agent.
CREATE INDEX IF NOT EXISTS idx_send_queue_agent ON public.send_queue (agent_id) WHERE agent_id IS NOT NULL;

DROP TRIGGER IF EXISTS send_queue_owner_is_agents_owner ON public.send_queue;
CREATE TRIGGER send_queue_owner_is_agents_owner BEFORE INSERT OR UPDATE OF owner_id, agent_id
    ON public.send_queue FOR EACH ROW EXECUTE FUNCTION public.plugin_rows_owner_is_agents_owner('nullable');

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.send_queue ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.send_queue ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.send_queue FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.send_queue;
CREATE POLICY owner_isolation ON public.send_queue
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.send_queue TO muster_app;
GRANT USAGE, SELECT ON SEQUENCE public.send_queue_id_seq TO muster_app;
