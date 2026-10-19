-- muster 0030 — A CHAT SESSION'S RUNTIME AND MODEL, AND THE AGENT'S DEFAULT
-- SESSION (internal/agents' ChatSession.Runtime/Model/IsDefault).
--
-- 1. RUNTIME + MODEL, FIXED AT CREATION. A chief chat session may be started on
--    a runtime other than its agent's kind — Claude Code (the pod's ccd) or the
--    model-gateway runtime that runs beside it — with a model chosen for that
--    session alone. Both are written once, by the INSERT that creates the
--    session; no statement in muster updates them.
--      runtime ''            the LEGACY session: the agent's own kind, and for a
--                            claude-code agent the ONE CLI ccd has always run.
--                            Every row that exists before this migration is one,
--                            so nothing an agent is doing today changes route.
--      runtime 'claude-code' a session ccd runs in a CLI of its own (--model).
--      runtime 'gateway'     a session the model-gateway runtime answers.
--    A legacy session names no model (the CHECK below); an explicit one may
--    (empty = the runtime's own default).
--
-- 2. ONE DEFAULT SESSION PER AGENT. is_default marks the session plugin
--    deliveries and `chief ask` go to. The partial unique index makes "at most
--    one" a property of the table, not of a code path. The backfill elects, for
--    every agent with sessions, its LEGACY-KEY session (agent:<name>:webchat —
--    the conversation a pre-sessions agent always had) when it has one, else its
--    oldest session; an agent with no session gets one elected lazily, the first
--    time one is needed (PGStore.DefaultSession).
--
-- Not re-applied by db.RLSOn (it changes no policy, grant or default) and
-- idempotent: ADD COLUMN IF NOT EXISTS, DROP/ADD CONSTRAINT, CREATE INDEX IF NOT
-- EXISTS, and a backfill that only touches agents with no default yet.

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.chat_sessions ADD COLUMN IF NOT EXISTS runtime    TEXT    NOT NULL DEFAULT '';
ALTER TABLE public.chat_sessions ADD COLUMN IF NOT EXISTS model      TEXT    NOT NULL DEFAULT '';
ALTER TABLE public.chat_sessions ADD COLUMN IF NOT EXISTS is_default BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE public.chat_sessions DROP CONSTRAINT IF EXISTS chat_sessions_runtime_check;
ALTER TABLE public.chat_sessions ADD CONSTRAINT chat_sessions_runtime_check CHECK (
    runtime IN ('', 'claude-code', 'gateway')
    AND (runtime <> '' OR model = '')
    AND char_length(model) <= 200);

-- "Fixed for the session's life" is the database's rule, not only the API's: a
-- session's turns so far ran on ONE runtime and model, and a row that later named
-- another would misdescribe its own transcript.
CREATE OR REPLACE FUNCTION public.chat_sessions_runtime_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.runtime IS DISTINCT FROM OLD.runtime OR NEW.model IS DISTINCT FROM OLD.model THEN
        RAISE EXCEPTION 'chat_sessions: a session''s runtime and model are fixed when it is created (id %)', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS chat_sessions_runtime_immutable ON public.chat_sessions;
CREATE TRIGGER chat_sessions_runtime_immutable BEFORE UPDATE OF runtime, model ON public.chat_sessions
    FOR EACH ROW EXECUTE FUNCTION public.chat_sessions_runtime_immutable();

CREATE UNIQUE INDEX IF NOT EXISTS chat_sessions_one_default
    ON public.chat_sessions (agent_id) WHERE is_default;

UPDATE public.chat_sessions s
SET is_default = true
FROM (
    SELECT DISTINCT ON (cs.agent_id) cs.id
    FROM public.chat_sessions cs
    JOIN public.agents a ON a.id = cs.agent_id
    WHERE NOT EXISTS (SELECT 1 FROM public.chat_sessions d WHERE d.agent_id = cs.agent_id AND d.is_default)
    ORDER BY cs.agent_id, (cs.session_key = 'agent:' || a.name || ':webchat') DESC, cs.created_at, cs.id
) pick
WHERE s.id = pick.id;
