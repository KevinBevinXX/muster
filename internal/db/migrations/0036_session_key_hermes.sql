-- muster 0036 — A STORED SESSION KEY MAY NAME A HERMES CONVERSATION
-- (docs/design/hermes.md, "Server changes").
--
-- Every column that stores a mirrored session's key admitted a Claude Code
-- session id only: '^[0-9A-Za-z][0-9A-Za-z_-]{0,63}$', 64 characters at most.
-- A Hermes session id is "local-<64 hex>", 70 characters, so it fits none of
-- them. From here a key is either a Claude Code id exactly as before, or a
-- RUNTIME-QUALIFIED Hermes one, "hermes:<id>" with an id of 1-128 characters
-- (mirror.SessionKeyPattern, which spells the same expression; a test in
-- internal/db reads this file and fails when the two differ):
--
--   ^([0-9A-Za-z][0-9A-Za-z_-]{0,63}|hermes:[0-9A-Za-z][0-9A-Za-z_-]{0,127})$
--
-- 🔴 A BARE 70-CHARACTER ID IS STILL REFUSED. Only the qualified form is
-- wider, so a Hermes key can never be read as a Claude one, and every key
-- stored before this migration still matches.
--
-- The four checks, each re-created under its own name:
--   - agent_focus.subject_key (0020) and agent_focus_history.subject_key
--     (0022): internal/focus writes both in ONE transaction, so widening one
--     alone would fail every Hermes write;
--   - session_routes.target_key (0021): a routed message is a send path;
--   - attention_asks.mirror_session (0025).
-- Deliberately NOT attention_asks.answered_via_session (0034) nor
-- connector_grants.decided_via_session (0035): each names the Claude Code
-- session that answered or decided through the host's MCP endpoint, which
-- identifies a caller by its tmux pane — and at the Hermes pin MCP clients run
-- in the gateway, whose pane is the wrong one (the design's "Listing" section).
--
-- Not re-applied by db.RLSOn (it changes no policy, grant or default), so it
-- need not be idempotent; it is anyway (DROP … IF EXISTS, then ADD).

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.agent_focus DROP CONSTRAINT IF EXISTS agent_focus_subject_check;
ALTER TABLE public.agent_focus
    ADD CONSTRAINT agent_focus_subject_check CHECK (
        (subject_kind = 'agent' AND agent_id IS NOT NULL AND subject_key = agent_id::text)
        OR (subject_kind = 'mirror' AND agent_id IS NULL
            AND subject_key ~ '^([0-9A-Za-z][0-9A-Za-z_-]{0,63}|hermes:[0-9A-Za-z][0-9A-Za-z_-]{0,127})$'));

ALTER TABLE public.agent_focus_history DROP CONSTRAINT IF EXISTS agent_focus_history_subject_check;
ALTER TABLE public.agent_focus_history
    ADD CONSTRAINT agent_focus_history_subject_check CHECK (
        (subject_kind = 'agent' AND agent_id IS NOT NULL AND subject_key = agent_id::text)
        OR (subject_kind = 'mirror' AND agent_id IS NULL
            AND subject_key ~ '^([0-9A-Za-z][0-9A-Za-z_-]{0,63}|hermes:[0-9A-Za-z][0-9A-Za-z_-]{0,127})$'));

ALTER TABLE public.session_routes DROP CONSTRAINT IF EXISTS session_routes_key_check;
ALTER TABLE public.session_routes
    ADD CONSTRAINT session_routes_key_check
    CHECK (target_key ~ '^([0-9A-Za-z][0-9A-Za-z_-]{0,63}|hermes:[0-9A-Za-z][0-9A-Za-z_-]{0,127})$');

ALTER TABLE public.attention_asks DROP CONSTRAINT IF EXISTS attention_asks_mirror_session_check;
ALTER TABLE public.attention_asks
    ADD CONSTRAINT attention_asks_mirror_session_check
    CHECK (mirror_session IS NULL
        OR mirror_session ~ '^([0-9A-Za-z][0-9A-Za-z_-]{0,63}|hermes:[0-9A-Za-z][0-9A-Za-z_-]{0,127})$');
