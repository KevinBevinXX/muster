-- muster 0026 — AN ASK ANSWERED IN THE SESSION (internal/attention's
-- AnsweredInSession).
--
-- When the operator writes to an asker directly — a message into the mirrored
-- Claude Code session's window, or to the dispatched agent's chat — after it
-- raised an ask, that ask leaves the queue as 'answered_in_session', the
-- operator's message (capped) recorded as its answer. The state machine gains
-- one exit: open → answered | answered_in_session | cancelled | expired, still
-- each one conditional UPDATE on status = 'open'.
--
-- answer / answered_by are set exactly when the ask was answered either way:
-- answered_by is still 'operator' — the operator wrote it, just elsewhere.
--
-- Not re-applied by db.RLSOn (it changes no policy, grant or default); it is
-- idempotent anyway (DROP … IF EXISTS, then ADD). 0025's body, which RLSOn may
-- re-apply, states these constraints only inside CREATE TABLE IF NOT EXISTS,
-- so a re-apply does not undo them.

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.attention_asks DROP CONSTRAINT IF EXISTS attention_asks_status_check;
ALTER TABLE public.attention_asks
    ADD CONSTRAINT attention_asks_status_check
    CHECK (status IN ('open', 'answered', 'answered_in_session', 'cancelled', 'expired'));

ALTER TABLE public.attention_asks DROP CONSTRAINT IF EXISTS attention_asks_answered_check;
ALTER TABLE public.attention_asks
    ADD CONSTRAINT attention_asks_answered_check
    CHECK ((status IN ('answered', 'answered_in_session')) = (answer IS NOT NULL AND answered_by IS NOT NULL));
