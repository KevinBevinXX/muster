-- muster 0050 — A STORED SESSION KEY MAY NAME AN OPENCODE SESSION
-- (docs/design/opencode.md; 0036 is the template).
--
-- An opencode window with a rendered view names its session (Window.
-- AgentSession, "ses_…"), and its key is RUNTIME-QUALIFIED like a Hermes one:
-- "opencode:<id>" with an id of 1-128 characters. mirror.SessionKeyPattern is
-- built from the runtime table, so the opencode runtime's KeyPrefix widened it;
-- a test in internal/db reads the LATEST migration that writes these CHECKs and
-- fails until it spells the same expression:
--
--   ^([0-9A-Za-z][0-9A-Za-z_-]{0,63}|hermes:[0-9A-Za-z][0-9A-Za-z_-]{0,127}|opencode:[0-9A-Za-z][0-9A-Za-z_-]{0,127})$
--
-- Every key 0036 admitted still matches: the Claude Code and Hermes branches
-- are unchanged. Only the qualified opencode form is new, so an opencode key
-- can never be read as a Claude one.
--
-- The same four checks as 0036, each re-created under its own name:
-- agent_focus.subject_key and agent_focus_history.subject_key (written in
-- ONE transaction by internal/focus), session_routes.target_key and
-- attention_asks.mirror_session. Deliberately not answered_via_session nor
-- decided_via_session, for 0036's reason.
--
-- Not re-applied by db.RLSOn; idempotent anyway (DROP … IF EXISTS, then ADD).

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.agent_focus DROP CONSTRAINT IF EXISTS agent_focus_subject_check;
ALTER TABLE public.agent_focus
    ADD CONSTRAINT agent_focus_subject_check CHECK (
        (subject_kind = 'agent' AND agent_id IS NOT NULL AND subject_key = agent_id::text)
        OR (subject_kind = 'mirror' AND agent_id IS NULL
            AND subject_key ~ '^([0-9A-Za-z][0-9A-Za-z_-]{0,63}|hermes:[0-9A-Za-z][0-9A-Za-z_-]{0,127}|opencode:[0-9A-Za-z][0-9A-Za-z_-]{0,127})$'));

ALTER TABLE public.agent_focus_history DROP CONSTRAINT IF EXISTS agent_focus_history_subject_check;
ALTER TABLE public.agent_focus_history
    ADD CONSTRAINT agent_focus_history_subject_check CHECK (
        (subject_kind = 'agent' AND agent_id IS NOT NULL AND subject_key = agent_id::text)
        OR (subject_kind = 'mirror' AND agent_id IS NULL
            AND subject_key ~ '^([0-9A-Za-z][0-9A-Za-z_-]{0,63}|hermes:[0-9A-Za-z][0-9A-Za-z_-]{0,127}|opencode:[0-9A-Za-z][0-9A-Za-z_-]{0,127})$'));

ALTER TABLE public.session_routes DROP CONSTRAINT IF EXISTS session_routes_key_check;
ALTER TABLE public.session_routes
    ADD CONSTRAINT session_routes_key_check
    CHECK (target_key ~ '^([0-9A-Za-z][0-9A-Za-z_-]{0,63}|hermes:[0-9A-Za-z][0-9A-Za-z_-]{0,127}|opencode:[0-9A-Za-z][0-9A-Za-z_-]{0,127})$');

ALTER TABLE public.attention_asks DROP CONSTRAINT IF EXISTS attention_asks_mirror_session_check;
ALTER TABLE public.attention_asks
    ADD CONSTRAINT attention_asks_mirror_session_check
    CHECK (mirror_session IS NULL
        OR mirror_session ~ '^([0-9A-Za-z][0-9A-Za-z_-]{0,63}|hermes:[0-9A-Za-z][0-9A-Za-z_-]{0,127}|opencode:[0-9A-Za-z][0-9A-Za-z_-]{0,127})$');
