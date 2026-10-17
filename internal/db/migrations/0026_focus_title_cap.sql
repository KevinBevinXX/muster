-- muster 0026 — A FOCUS TITLE IS SHOWN WHOLE: its cap is a storage guard, not
-- a display truncation.
--
-- 0020 and 0022 capped a focus title at 80 characters (focus.MaxTitleRunes),
-- and the store clamped every title to fit, with an ellipsis — so a title the
-- summarizer wrote a little long reached the card cut off mid-thought. The
-- store no longer clamps a title to a display length: the summarizer is asked
-- for a short one (focus.SummaryInstructions), the card wraps it, and the only
-- bound left is this one — generous, against a runaway summary filling a row.
-- focus.MaxTitleRunes says the same number.
--
-- Titles already stored (some ending in "…") are left as they are.
--
-- Not re-applied by db.RLSOn (it changes no policy, grant or default), so it
-- need not be idempotent; it is anyway (DROP … IF EXISTS, then ADD).

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.agent_focus DROP CONSTRAINT IF EXISTS agent_focus_title_cap;
ALTER TABLE public.agent_focus
    ADD CONSTRAINT agent_focus_title_cap CHECK (char_length(title) <= 300);

ALTER TABLE public.agent_focus_history DROP CONSTRAINT IF EXISTS agent_focus_history_title_check;
ALTER TABLE public.agent_focus_history
    ADD CONSTRAINT agent_focus_history_title_check CHECK (char_length(title) BETWEEN 1 AND 300);
