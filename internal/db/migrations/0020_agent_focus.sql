-- muster 0020 — SESSION FOCUS (internal/focus): what each of a user's sessions
-- is working on, as a short title and a one-to-three-sentence description, for
-- the Agents list's cards and the chief's focus_search tool.
--
-- ONE TABLE FOR BOTH KINDS OF SESSION (operator decision): a dispatched agent
-- (subject_kind 'agent', keyed by its agents.id) and a mirrored Claude Code
-- window (subject_kind 'mirror', keyed by the CLAUDE CODE SESSION ID the host
-- reports for the window — never the tmux window id "@n", which tmux reissues,
-- so a focus survives window churn and follows the conversation, not the pane).
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, IN 0017's SHAPE. owner_isolation
-- keeps one user from reading or writing another's focus. The agent an agent
-- row names is pinned to the same owner by 0015's
-- plugin_rows_owner_is_agents_owner trigger, run here with 'nullable' (a mirror
-- row names no agent), and the row dies with its agent (ON DELETE CASCADE).
--
-- 🔴 search IS A GENERATED tsvector, so the index cannot drift from the text.
-- Title weighs A and description B; the GIN index serves focus_search's
-- `search @@ query` and ts_rank orders the matches.
--
-- 🔴 A FAILED REFRESH KEEPS THE LAST GOOD FOCUS. last_error / error_at record
-- the failure beside it — a short, fixed, non-secret reason (focus.Reason),
-- which the card and agents_list show as why a focus is missing or stale; title and description are only ever written with a
-- validated summary. title may be '' only on a row that has never held one (a
-- first refresh that failed), and the readers skip such rows.
--
-- 🔴 IT IS RE-APPLIED BY db.RLSOn (rollback.go) WHEN THE TABLE EXISTS, SO EVERY
-- STATEMENT IS IDEMPOTENT, and nothing here redefines an object 0015 defines.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.agent_focus (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id     BIGINT      NOT NULL
                 DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                 REFERENCES public.users (id),
    subject_kind TEXT        NOT NULL,
    subject_key  TEXT        NOT NULL,
    agent_id     BIGINT      REFERENCES public.agents (id) ON DELETE CASCADE,
    title        TEXT        NOT NULL DEFAULT '',
    description  TEXT        NOT NULL DEFAULT '',
    source       TEXT        NOT NULL DEFAULT 'summarizer',
    -- sha256 (hex) of the bounded transcript tail the focus was computed from:
    -- a refresh whose tail hashes the same skips the summarizer call.
    fingerprint  TEXT        NOT NULL DEFAULT '',
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error   TEXT        NOT NULL DEFAULT '',
    error_at     TIMESTAMPTZ,
    search       tsvector GENERATED ALWAYS AS (
                     setweight(to_tsvector('english', coalesce(title, '')), 'A') ||
                     setweight(to_tsvector('english', coalesce(description, '')), 'B')) STORED,
    CONSTRAINT agent_focus_kind_check CHECK (subject_kind IN ('agent', 'mirror')),
    -- One writer: muster's own refresh (internal/focus.Refresher).
    CONSTRAINT agent_focus_source_check CHECK (source = 'summarizer'),
    -- An agent row names its agent, and its key IS that id; a mirror row names
    -- no agent and is keyed by a Claude Code session id (mirror.ValidClaudeSession).
    CONSTRAINT agent_focus_subject_check CHECK (
        (subject_kind = 'agent' AND agent_id IS NOT NULL AND subject_key = agent_id::text)
        OR (subject_kind = 'mirror' AND agent_id IS NULL AND subject_key ~ '^[0-9A-Za-z][0-9A-Za-z_-]{0,63}$')),
    -- focus.MaxTitleRunes / MaxDescriptionRunes; the store clamps first.
    CONSTRAINT agent_focus_title_cap CHECK (char_length(title) <= 80),
    CONSTRAINT agent_focus_description_cap CHECK (char_length(description) <= 600),
    CONSTRAINT agent_focus_fingerprint_check CHECK (fingerprint = '' OR fingerprint ~ '^[0-9a-f]{64}$'),
    CONSTRAINT agent_focus_error_cap CHECK (octet_length(last_error) <= 1024)
);

-- One focus per (owner, subject): every write is an upsert on it.
CREATE UNIQUE INDEX IF NOT EXISTS agent_focus_owner_subject
    ON public.agent_focus (owner_id, subject_kind, subject_key);
CREATE INDEX IF NOT EXISTS idx_agent_focus_search ON public.agent_focus USING GIN (search);
-- The cascade from agents looks rows up by agent.
CREATE INDEX IF NOT EXISTS idx_agent_focus_agent ON public.agent_focus (agent_id) WHERE agent_id IS NOT NULL;

DROP TRIGGER IF EXISTS agent_focus_owner_is_agents_owner ON public.agent_focus;
CREATE TRIGGER agent_focus_owner_is_agents_owner BEFORE INSERT OR UPDATE OF owner_id, agent_id
    ON public.agent_focus FOR EACH ROW EXECUTE FUNCTION public.plugin_rows_owner_is_agents_owner('nullable');

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.agent_focus ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.agent_focus ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.agent_focus FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.agent_focus;
CREATE POLICY owner_isolation ON public.agent_focus
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.agent_focus TO muster_app;
GRANT USAGE, SELECT ON SEQUENCE public.agent_focus_id_seq TO muster_app;
