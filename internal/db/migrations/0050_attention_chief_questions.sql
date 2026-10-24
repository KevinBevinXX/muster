-- muster 0050 — A MIRRORED SESSION'S QUESTION TO THE CHIEF (chief_ask,
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
--   route_id    the routed message a question is about (chief_ask's
--               route_id): read again when the question's turn is retried.
--   chief_turn_at  when the question's turn to the chief was started; NULL
--               while it has not reached the chief (it was busy or stopped),
--               which the attention sweep retries (QuestionsDue). Taken by one
--               conditional UPDATE (MarkQuestionTurn), so it is put once.
--
-- answered_by gains 'chief' (attention.AnswerByChief, which answers only a
-- to_chief ask); every other ask is still answered by the operator.
--
-- Must apply after 0049 (the contiguity test refuses a gap). Not re-applied by
-- db.RLSOn (it changes no policy, grant or default) and idempotent: ADD COLUMN
-- IF NOT EXISTS, CREATE INDEX IF NOT EXISTS, DROP/ADD CONSTRAINT. 0025's body,
-- which RLSOn may re-apply, names its answered_by CHECK only inside CREATE TABLE
-- IF NOT EXISTS, so it never puts the narrower rule back.
--
-- ROLLBACK. An image from before 0050 runs over this schema unchanged (it never
-- writes the columns; it would LIST a to_chief question on the Attention view,
-- answerable by the operator, and read a chief's answer as an answer). To
-- remove the schema itself, after rolling the image back:
--   UPDATE attention_asks SET status = 'expired', answer = NULL, answered_by = NULL, delivery = NULL
--     WHERE answered_by = 'chief';  -- 0025's CHECK allows only 'operator'
--   ALTER TABLE attention_asks DROP CONSTRAINT IF EXISTS attention_asks_answered_by_check;
--   ALTER TABLE attention_asks ADD CONSTRAINT attention_asks_answered_by_check
--     CHECK (answered_by IS NULL OR answered_by = 'operator');
--   DROP INDEX IF EXISTS idx_attention_asks_chief_due;
--   ALTER TABLE attention_asks DROP COLUMN IF EXISTS chief_turn_at, DROP COLUMN IF EXISTS route_id,
--       DROP COLUMN IF EXISTS to_chief;
--   DELETE FROM schema_migrations WHERE version = 50;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS to_chief      BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS route_id      TEXT;
ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS chief_turn_at TIMESTAMPTZ;

-- The sweep: open questions not yet put to the chief.
CREATE INDEX IF NOT EXISTS idx_attention_asks_chief_due ON public.attention_asks (id)
    WHERE to_chief AND status = 'open' AND chief_turn_at IS NULL;

ALTER TABLE public.attention_asks DROP CONSTRAINT IF EXISTS attention_asks_answered_by_check;
ALTER TABLE public.attention_asks
    ADD CONSTRAINT attention_asks_answered_by_check CHECK (
        answered_by IS NULL OR answered_by IN ('operator', 'chief'));
