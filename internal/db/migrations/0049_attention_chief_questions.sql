-- muster 0049 — A MIRRORED SESSION'S QUESTION TO THE CHIEF (chief_ask,
-- internal/api/chief_ask_tool.go; docs/attention.md#asking-the-chief).
--
-- A mirrored Claude Code session can now put a clarifying question to its
-- owner's CHIEF instead of to the owner. The question is an attention ask like
-- any other — the session's own row, waited on with attention_wait, expiring at
-- expires_at — addressed to the chief:
--
--   to_chief    true: the chief may answer it (answer_session_question), and it
--               is not listed in the owner's Attention queue (the chief's
--               thread, where the question arrives as a turn, is where it
--               shows); false: every ask before this migration, and every ask
--               raised with attention_ask — which no agent may answer.
--   escalates   on the CHIEF's own ask (escalate_session_question): the id of
--               the session question it puts to the owner. The owner's answer
--               to it is relayed to that question as the owner's answer. No
--               foreign key: muster writes it from a question it just read, and
--               a link to a question that is gone relays nothing (the chief is
--               sent the answer instead).
--   route_id    the routed message a question is about (chief_ask's
--               route_id): read again when the question's turn is retried.
--   chief_turn_at  when the question's turn to the chief was started; NULL
--               while it has not reached the chief (it was busy or stopped),
--               which the attention sweep retries (QuestionsDue). Taken by one
--               conditional UPDATE (MarkQuestionTurn), so it is put once.
--
-- 🔴 A QUESTION'S ESCALATION ENDS WITH THE QUESTION, IN THE SAME STATEMENT: the
-- attention_asks_close_escalations trigger cancels every open ask that
-- escalates a to_chief question the moment that question leaves 'open'
-- (answered by the chief or the owner, cancelled, answered in the session,
-- expired) — whichever path moved it. And a question has at most ONE open
-- escalation (attention_asks_one_open_escalation).
--
-- answered_by gains 'chief' (attention.AnswerByChief, which answers only a
-- to_chief ask); every other ask is still answered by the operator.
--
-- Must apply after 0048 (the contiguity test refuses a gap). Not re-applied by
-- db.RLSOn (it changes no policy, grant or default) and idempotent: ADD COLUMN
-- IF NOT EXISTS, DROP/ADD CONSTRAINT. 0025's body, which RLSOn may re-apply,
-- names its answered_by CHECK only inside CREATE TABLE IF NOT EXISTS, so it
-- never puts the narrower rule back.
--
-- ROLLBACK. An image from before 0049 runs over this schema unchanged (it never
-- writes the columns; it would LIST a to_chief question on the Attention view,
-- answerable by the operator, and read a chief's answer as an answer). To
-- remove the schema itself, after rolling the image back:
--   UPDATE attention_asks SET status = 'expired', answer = NULL, answered_by = NULL, delivery = NULL
--     WHERE answered_by = 'chief';  -- 0025's CHECK allows only 'operator'
--   ALTER TABLE attention_asks DROP CONSTRAINT IF EXISTS attention_asks_answered_by_check;
--   ALTER TABLE attention_asks ADD CONSTRAINT attention_asks_answered_by_check
--     CHECK (answered_by IS NULL OR answered_by = 'operator');
--   DROP TRIGGER IF EXISTS attention_asks_close_escalations ON attention_asks;
--   DROP FUNCTION IF EXISTS attention_asks_close_escalations();
--   DROP INDEX IF EXISTS attention_asks_one_open_escalation;
--   ALTER TABLE attention_asks DROP COLUMN IF EXISTS chief_turn_at, DROP COLUMN IF EXISTS route_id,
--       DROP COLUMN IF EXISTS escalates, DROP COLUMN IF EXISTS to_chief;
--   DELETE FROM schema_migrations WHERE version = 49;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS to_chief  BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS escalates BIGINT;
ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS route_id TEXT;
ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS chief_turn_at TIMESTAMPTZ;

-- The sweep: open questions not yet put to the chief.
CREATE INDEX IF NOT EXISTS idx_attention_asks_chief_due ON public.attention_asks (id)
    WHERE to_chief AND status = 'open' AND chief_turn_at IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS attention_asks_one_open_escalation ON public.attention_asks (escalates)
    WHERE escalates IS NOT NULL AND status = 'open';

CREATE OR REPLACE FUNCTION public.attention_asks_close_escalations() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    UPDATE public.attention_asks SET status = 'cancelled', resolved_at = now()
     WHERE escalates = NEW.id AND status = 'open';
    RETURN NULL;
END
$$;

DROP TRIGGER IF EXISTS attention_asks_close_escalations ON public.attention_asks;
CREATE TRIGGER attention_asks_close_escalations AFTER UPDATE OF status ON public.attention_asks
    FOR EACH ROW WHEN (NEW.to_chief AND OLD.status = 'open' AND NEW.status <> 'open')
    EXECUTE FUNCTION public.attention_asks_close_escalations();

ALTER TABLE public.attention_asks DROP CONSTRAINT IF EXISTS attention_asks_answered_by_check;
ALTER TABLE public.attention_asks
    ADD CONSTRAINT attention_asks_answered_by_check CHECK (
        answered_by IS NULL OR answered_by IN ('operator', 'chief'));
