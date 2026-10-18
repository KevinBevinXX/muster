-- muster 0025 — THE ATTENTION QUEUE's explicit asks (internal/attention): a
-- question an agent put to its owner (attention_ask), waiting for an answer.
--
-- 🔴 ONLY THE EXPLICIT ASKS ARE STORED. The attention view also lists mirrored
-- sessions waiting on the person (the hub's live window status) and reply
-- drafts awaiting approval (plugin_reply_drafts) — both are DERIVED, live, from
-- the state that already owns them, and never copied here, so neither can
-- disagree with its source.
--
-- An ask is raised by exactly ONE asker: a dispatched agent (agent_id, deleted
-- with it) or a mirrored Claude Code session (mirror_host + the Claude session
-- id). asker_label is the name shown beside it, captured when it was asked.
--
-- 🔴 A STATE MACHINE WITH ONE EXIT. open → answered | cancelled | expired, each
-- written by one conditional UPDATE on status = 'open', so an answer and a
-- cancel cannot both land. answer / answered_by are set exactly when answered;
-- resolved_at exactly when it left open. expires_at is fixed at ask time
-- (internal/attention bounds it); the expiry sweep moves a lapsed open ask to
-- expired.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, IN 0019's SHAPE, with the
-- agent an ask names pinned to the same owner by 0015's
-- plugin_rows_owner_is_agents_owner trigger ('nullable': a mirrored session's
-- ask names no agent).
--
-- 🔴 IT IS RE-APPLIED BY db.RLSOn (rollback.go) WHEN THE TABLE EXISTS, SO EVERY
-- STATEMENT IS IDEMPOTENT.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.attention_asks (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id       BIGINT      NOT NULL
                   DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                   REFERENCES public.users (id),
    agent_id       BIGINT      REFERENCES public.agents (id) ON DELETE CASCADE,
    mirror_host    TEXT,
    mirror_session TEXT,
    asker_label    TEXT        NOT NULL DEFAULT '',
    question       TEXT        NOT NULL,
    -- The answers the asker offered, as a JSON array of strings ([] = free text only).
    options        JSONB       NOT NULL DEFAULT '[]'::jsonb,
    status         TEXT        NOT NULL DEFAULT 'open',
    answer         TEXT,
    -- Who answered: 'operator' (the attention view) — the only answerer.
    answered_by    TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at     TIMESTAMPTZ NOT NULL,
    resolved_at    TIMESTAMPTZ,
    CONSTRAINT attention_asks_asker_check CHECK (
        (agent_id IS NOT NULL AND mirror_host IS NULL AND mirror_session IS NULL)
        OR (agent_id IS NULL AND mirror_host IS NOT NULL AND mirror_session IS NOT NULL)),
    -- internal/mirror's ValidHost bound and ValidClaudeSession pattern.
    CONSTRAINT attention_asks_mirror_host_cap CHECK (mirror_host IS NULL OR char_length(mirror_host) BETWEEN 1 AND 63),
    CONSTRAINT attention_asks_mirror_session_check CHECK (mirror_session IS NULL OR mirror_session ~ '^[0-9A-Za-z][0-9A-Za-z_-]{0,63}$'),
    CONSTRAINT attention_asks_label_cap CHECK (char_length(asker_label) <= 200),
    -- attention.MaxQuestionRunes / MaxOptions / MaxAnswerRunes.
    CONSTRAINT attention_asks_question_cap CHECK (char_length(question) BETWEEN 1 AND 4000),
    CONSTRAINT attention_asks_options_check CHECK (jsonb_typeof(options) = 'array' AND jsonb_array_length(options) <= 10),
    CONSTRAINT attention_asks_answer_cap CHECK (answer IS NULL OR char_length(answer) BETWEEN 1 AND 8000),
    CONSTRAINT attention_asks_status_check CHECK (status IN ('open', 'answered', 'cancelled', 'expired')),
    CONSTRAINT attention_asks_answered_check CHECK ((status = 'answered') = (answer IS NOT NULL AND answered_by IS NOT NULL)),
    CONSTRAINT attention_asks_answered_by_check CHECK (answered_by IS NULL OR answered_by = 'operator'),
    CONSTRAINT attention_asks_resolved_check CHECK ((status = 'open') = (resolved_at IS NULL)),
    CONSTRAINT attention_asks_expiry_check CHECK (expires_at > created_at)
);

-- The view and the sweep: open asks, oldest first.
CREATE INDEX IF NOT EXISTS idx_attention_asks_open ON public.attention_asks (owner_id, id) WHERE status = 'open';
CREATE INDEX IF NOT EXISTS idx_attention_asks_expiry ON public.attention_asks (expires_at) WHERE status = 'open';
-- The cascade from agents looks rows up by agent.
CREATE INDEX IF NOT EXISTS idx_attention_asks_agent ON public.attention_asks (agent_id) WHERE agent_id IS NOT NULL;

DROP TRIGGER IF EXISTS attention_asks_owner_is_agents_owner ON public.attention_asks;
CREATE TRIGGER attention_asks_owner_is_agents_owner BEFORE INSERT OR UPDATE OF owner_id, agent_id
    ON public.attention_asks FOR EACH ROW EXECUTE FUNCTION public.plugin_rows_owner_is_agents_owner('nullable');

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.attention_asks ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.attention_asks ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.attention_asks FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.attention_asks;
CREATE POLICY owner_isolation ON public.attention_asks
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE ON public.attention_asks TO muster_app;
GRANT USAGE, SELECT ON SEQUENCE public.attention_asks_id_seq TO muster_app;
