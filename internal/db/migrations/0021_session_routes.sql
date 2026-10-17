-- muster 0021 — SESSION ROUTES (internal/plugins/routes.go): the chief hands a
-- plugin message to the SESSION best placed to answer it (route_to_session), the
-- session answers (reply_to_chief), and the answer becomes the plugin reply.
--
-- A route names the message, the chief that routed it (agent_id), its target
-- session — a dispatched agent (by id) or a mirrored Claude Code session (by
-- its Claude session id), exactly as agent_focus (0020) keys them — and how far
-- it got:
--
--   routed ──reply──▶ answered ──▶ relayed   (an AUTO-APPROVED reply draft)
--      │                     └────▶ fallback  (a PENDING draft the owner approves)
--      ├─ deadline ─▶ timed_out (handed back to the chief)
--      └─ the injection, or the relay, could not be made ─▶ failed
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, IN 0016's SHAPE. A route's owner
-- is its message's owner by composite foreign key, and the agent it names is
-- pinned to the same owner by 0015's plugin_rows_owner_is_agents_owner trigger.
--
-- 🔴 EXACTLY ONCE, BY THE SCHEMA. route_id is unique; at most one route per
-- message is open (routed or answered: session_routes_one_open); and a reply
-- draft names at most one route (plugin_reply_drafts_one_per_route), so a relay
-- retried, raced or replayed can never file a second reply for one route.
--
-- 🔴 send_queue_id HAS NO FOREIGN KEY ON PURPOSE: the send queue prunes finished
-- rows after 7 days (sendqueue.Retention), and the route is an audit record
-- that outlives them (schema.md's "some columns deliberately have no foreign key").
--
-- 🔴 IT IS RE-APPLIED BY db.RLSOn (rollback.go) WHEN THE TABLE EXISTS, SO EVERY
-- STATEMENT IS IDEMPOTENT, and nothing here redefines an object 0015 or 0016
-- defines.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.session_routes (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id          BIGINT      NOT NULL
                      DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                      REFERENCES public.users (id),
    route_id          TEXT        NOT NULL UNIQUE,
    plugin_message_id BIGINT      NOT NULL,
    -- The chief that routed it. SET NULL, NOT CASCADE (0016's reason): the
    -- record of a route outlives the agent.
    agent_id          BIGINT      REFERENCES public.agents (id) ON DELETE SET NULL,
    target_kind       TEXT        NOT NULL,
    target_key        TEXT        NOT NULL,
    -- What the target was called when it was routed to (a window title, an
    -- agent name), for the audit trail; display only.
    target_label      TEXT        NOT NULL DEFAULT '',
    send_queue_id     BIGINT,
    state             TEXT        NOT NULL DEFAULT 'routed',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    timeout_at        TIMESTAMPTZ NOT NULL,
    answered_at       TIMESTAMPTZ,
    finished_at       TIMESTAMPTZ,
    reply_body        TEXT,
    draft_id          BIGINT      REFERENCES public.plugin_reply_drafts (id) ON DELETE SET NULL,
    error             TEXT        NOT NULL DEFAULT '',
    -- 🔴 A HAND-BACK OWED TO THE CHIEF: why the message is back with it (the
    -- route timed out, its window moved on, its answer could not be sent),
    -- written in the SAME transaction that ended the route, and cleared by the
    -- one sweep that delivers it — so every ended route is handed back exactly
    -- once, and no request path runs the chief's turn itself. NULL: none owed.
    handback          TEXT,
    CONSTRAINT session_routes_message_owner_fkey FOREIGN KEY (plugin_message_id, owner_id)
        REFERENCES public.plugin_messages (id, owner_id) ON DELETE CASCADE,
    CONSTRAINT session_routes_route_id_check CHECK (route_id ~ '^rt_[0-9a-f]{24}$'),
    CONSTRAINT session_routes_kind_check CHECK (target_kind IN ('agent', 'mirror')),
    CONSTRAINT session_routes_key_check CHECK (target_key ~ '^[0-9A-Za-z][0-9A-Za-z_-]{0,63}$'),
    CONSTRAINT session_routes_label_cap CHECK (char_length(target_label) <= 200),
    CONSTRAINT session_routes_state_check CHECK (state IN ('routed', 'answered', 'relayed', 'timed_out', 'fallback', 'failed')),
    -- Open (routed, answered) is exactly "not finished".
    CONSTRAINT session_routes_finished_check CHECK ((state IN ('routed', 'answered')) = (finished_at IS NULL)),
    -- An answer is a body and its time, and every state past it carries both.
    CONSTRAINT session_routes_answer_check CHECK (
        (state IN ('answered', 'relayed', 'fallback')) <= (reply_body IS NOT NULL AND answered_at IS NOT NULL)),
    CONSTRAINT session_routes_body_cap CHECK (reply_body IS NULL OR octet_length(reply_body) BETWEEN 1 AND 16384),
    CONSTRAINT session_routes_error_cap CHECK (octet_length(error) <= 1024),
    CONSTRAINT session_routes_handback_cap CHECK (handback IS NULL OR octet_length(handback) BETWEEN 1 AND 2048)
);

-- At most one OPEN route per message: a message is routed to one session at a time.
CREATE UNIQUE INDEX IF NOT EXISTS session_routes_one_open
    ON public.session_routes (plugin_message_id) WHERE state IN ('routed', 'answered');
-- The timeout sweep: open routes by deadline.
CREATE INDEX IF NOT EXISTS idx_session_routes_due ON public.session_routes (timeout_at) WHERE state = 'routed';
CREATE INDEX IF NOT EXISTS idx_session_routes_message ON public.session_routes (plugin_message_id);
CREATE INDEX IF NOT EXISTS idx_session_routes_agent ON public.session_routes (agent_id) WHERE agent_id IS NOT NULL;
-- The sweep: hand-backs still owed.
CREATE INDEX IF NOT EXISTS idx_session_routes_handback ON public.session_routes (id) WHERE handback IS NOT NULL;

DROP TRIGGER IF EXISTS session_routes_owner_is_agents_owner ON public.session_routes;
CREATE TRIGGER session_routes_owner_is_agents_owner BEFORE INSERT OR UPDATE OF owner_id, agent_id
    ON public.session_routes FOR EACH ROW EXECUTE FUNCTION public.plugin_rows_owner_is_agents_owner('nullable');

-- 🔴 sender_verified: the PLUGIN's assertion that it authenticated the sender
-- (DKIM/DMARC-aligned mail, a signature it verified, the platform's
-- equivalent) — what a routed answer may be auto-approved on, together with
-- the owner's allowlist naming that plugin. Absent (every plugin today) is
-- false. Part of what arrived: immutable, like the body (0015) and no_reply.
ALTER TABLE public.plugin_messages ADD COLUMN IF NOT EXISTS sender_verified BOOLEAN NOT NULL DEFAULT false;

CREATE OR REPLACE FUNCTION public.plugin_messages_sender_verified_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.sender_verified IS DISTINCT FROM OLD.sender_verified THEN
        RAISE EXCEPTION 'plugin_messages: a message''s sender_verified mark is immutable (id %)', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS plugin_messages_sender_verified_immutable ON public.plugin_messages;
CREATE TRIGGER plugin_messages_sender_verified_immutable BEFORE UPDATE OF sender_verified ON public.plugin_messages
    FOR EACH ROW EXECUTE FUNCTION public.plugin_messages_sender_verified_immutable();

-- 🔴 send_queue.route_id MARKS A ROUTED ROW (internal/api/session_routes.go):
-- its delivery checks the route is still waiting and still bound to the same
-- Claude session, and that check — not a browser sign-in — is its gate. NULL is
-- a person's queued message, gated by its sign-in as before (0019).
ALTER TABLE public.send_queue ADD COLUMN IF NOT EXISTS route_id TEXT;

-- The reply draft a route became names it, for the audit trail — and at most
-- one draft may name a route: the exactly-once relay, held by the schema.
ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS route_id TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS plugin_reply_drafts_one_per_route
    ON public.plugin_reply_drafts (route_id) WHERE route_id IS NOT NULL;

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.session_routes ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.session_routes ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.session_routes FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.session_routes;
CREATE POLICY owner_isolation ON public.session_routes
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.session_routes TO muster_app;
GRANT USAGE, SELECT ON SEQUENCE public.session_routes_id_seq TO muster_app;
