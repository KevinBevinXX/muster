-- muster 0022 — SESSION FOCUS HISTORY (internal/focus): every focus a session
-- has had, newest first, so the rendered view and an agent's page can scroll
-- back through what it was working on.
--
-- APPEND-ONLY, ONE ROW PER CHANGE (operator decision: "every change, 30 days").
-- focus.PGStore.Upsert writes a row in the SAME TRANSACTION as the agent_focus
-- upsert, and only when the title or description differs from the subject's
-- newest history row: an identical refresh writes nothing. Rows older than
-- MUSTER_FOCUS_HISTORY_RETENTION (default 720h) are deleted by the hourly focus
-- retention sweep (focus.Retainer).
--
-- 🔴 A SUBJECT'S HISTORY DIES WITH ITS FOCUS ROW, BY FOREIGN KEY. (owner_id,
-- subject_kind, subject_key) references agent_focus's unique key ON DELETE
-- CASCADE, so both ways a focus row goes — an agent deleted (0020's cascade
-- from agents) and a mirrored session's row pruned by the retention sweep —
-- take its history with them, with no second delete to forget. The same key
-- pins a history row to its focus row's OWNER: a row cannot name a subject its
-- owner has no focus for.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, IN 0020's SHAPE, with the agent
-- an agent row names pinned to the same owner by 0015's
-- plugin_rows_owner_is_agents_owner trigger ('nullable': a mirror row names no
-- agent).
--
-- 🔴 IT IS RE-APPLIED BY db.RLSOn (rollback.go) WHEN THE TABLE EXISTS, SO EVERY
-- STATEMENT IS IDEMPOTENT — the seed below included (NOT EXISTS) — and nothing
-- here redefines an object 0015 or 0020 defines.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.agent_focus_history (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id     BIGINT      NOT NULL
                 DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                 REFERENCES public.users (id),
    subject_kind TEXT        NOT NULL,
    subject_key  TEXT        NOT NULL,
    agent_id     BIGINT      REFERENCES public.agents (id) ON DELETE CASCADE,
    title        TEXT        NOT NULL,
    description  TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT agent_focus_history_focus_fkey FOREIGN KEY (owner_id, subject_kind, subject_key)
        REFERENCES public.agent_focus (owner_id, subject_kind, subject_key) ON DELETE CASCADE,
    CONSTRAINT agent_focus_history_kind_check CHECK (subject_kind IN ('agent', 'mirror')),
    -- 0020's agent_focus_subject_check, row for row.
    CONSTRAINT agent_focus_history_subject_check CHECK (
        (subject_kind = 'agent' AND agent_id IS NOT NULL AND subject_key = agent_id::text)
        OR (subject_kind = 'mirror' AND agent_id IS NULL AND subject_key ~ '^[0-9A-Za-z][0-9A-Za-z_-]{0,63}$')),
    -- A history row is a focus that was shown: it always has a title.
    CONSTRAINT agent_focus_history_title_check CHECK (char_length(title) BETWEEN 1 AND 80),
    CONSTRAINT agent_focus_history_description_cap CHECK (char_length(description) <= 600)
);

-- The read: one subject's rows, newest first (keyset on created_at, id).
CREATE INDEX IF NOT EXISTS idx_agent_focus_history_subject
    ON public.agent_focus_history (owner_id, subject_kind, subject_key, created_at DESC, id DESC);
-- The retention sweep: rows by age.
CREATE INDEX IF NOT EXISTS idx_agent_focus_history_created ON public.agent_focus_history (created_at);
-- The cascade from agents looks rows up by agent.
CREATE INDEX IF NOT EXISTS idx_agent_focus_history_agent ON public.agent_focus_history (agent_id) WHERE agent_id IS NOT NULL;

DROP TRIGGER IF EXISTS agent_focus_history_owner_is_agents_owner ON public.agent_focus_history;
CREATE TRIGGER agent_focus_history_owner_is_agents_owner BEFORE INSERT OR UPDATE OF owner_id, agent_id
    ON public.agent_focus_history FOR EACH ROW EXECUTE FUNCTION public.plugin_rows_owner_is_agents_owner('nullable');

-- Seed: a session that already has a focus starts its history with it, so the
-- history tab is not empty until the focus next changes. Runs as the migrator
-- (every row); NOT EXISTS keeps a re-application from adding a second row.
INSERT INTO public.agent_focus_history (owner_id, subject_kind, subject_key, agent_id, title, description, created_at)
SELECT f.owner_id, f.subject_kind, f.subject_key, f.agent_id, f.title, f.description, f.updated_at
  FROM public.agent_focus f
 WHERE f.title <> ''
   AND NOT EXISTS (SELECT 1 FROM public.agent_focus_history h
                    WHERE h.owner_id = f.owner_id AND h.subject_kind = f.subject_kind AND h.subject_key = f.subject_key);

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.agent_focus_history ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.agent_focus_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.agent_focus_history FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.agent_focus_history;
CREATE POLICY owner_isolation ON public.agent_focus_history
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, DELETE ON public.agent_focus_history TO muster_app;
GRANT USAGE, SELECT ON SEQUENCE public.agent_focus_history_id_seq TO muster_app;
