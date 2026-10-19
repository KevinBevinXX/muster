-- muster 0031 — AN ANSWER IS DELIVERED TO THE AGENT THAT ASKED
-- (internal/attention's delivery half; internal/api/attention_delivery.go).
--
-- Until now an agent's ask was answered PULL-ONLY: the answer reached the agent
-- only through attention_wait. An agent that asked and ended its turn (the chief
-- routing a task to its owner, "I'm not waiting on an answer") never saw the
-- answer at all. Now, when the operator answers an AGENT's ask on /attention,
-- muster delivers the answer into that agent's DEFAULT chat session as a turn
-- (0030's is_default), once — unless the agent read it through attention_wait
-- first.
--
-- delivery is the answer's delivery state, NULL for every ask with nothing to
-- deliver (an open, cancelled, expired or answered-in-session ask; a system
-- asker's; and EVERY ROW ANSWERED BEFORE THIS MIGRATION — there is no backfill,
-- so no old answer is ever pushed):
--   queued       answered; a delivery is owed (Answer writes it for an agent asker)
--   delivering   one process holds the delivery lease (claimed_by, lease_until)
--   delivered    the turn completed in session delivered_session_id
--   unconfirmed  the turn failed after the text may have reached the agent, the
--                lease lapsed mid-turn (a crash), or it was still queued a day
--                after delivery_queued_at (the agent never took it): NEVER sent
--                again automatically — the operator's Retry moves it back to queued
--   waited       the asker read the answer through attention_wait: nothing to push
--   pull         a mirrored session's answer: muster cannot type into its window
--                unasked, so it waits for the session's attention_wait (→ waited)
--
-- 🔴 EXACTLY ONCE IS A PROPERTY OF THE TRANSITIONS: every move is one UPDATE
-- conditional on the state it leaves (queued → delivering by one claimer;
-- queued/pull → waited by the reader; delivering → done only by the lease holder),
-- so a reader and a pusher, or two pushers, cannot both win.
--
-- Not re-applied by db.RLSOn (it changes no policy, grant or default) and
-- idempotent: ADD COLUMN IF NOT EXISTS, DROP/ADD CONSTRAINT, CREATE INDEX IF NOT
-- EXISTS. 0025's body, which RLSOn may re-apply, does not name these columns.

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS delivery             TEXT;
ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS delivery_detail      TEXT        NOT NULL DEFAULT '';
ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS delivery_attempts    INTEGER     NOT NULL DEFAULT 0;
ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS delivery_claimed_by  TEXT;
ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS delivery_lease_until TIMESTAMPTZ;
ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS delivered_at         TIMESTAMPTZ;
-- When the delivery was last queued (the answer, or the operator's Retry): an
-- answer still queued a day later (its agent never took it) is given up as
-- unconfirmed, with Retry.
ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS delivery_queued_at   TIMESTAMPTZ;
-- The chat session the answer went to. A display link, like answer_queue_id: no
-- foreign key (a deleted session leaves a harmless dangling number).
ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS delivered_session_id BIGINT;

ALTER TABLE public.attention_asks DROP CONSTRAINT IF EXISTS attention_asks_delivery_check;
ALTER TABLE public.attention_asks
    ADD CONSTRAINT attention_asks_delivery_check CHECK (
        (delivery IS NULL OR delivery IN ('queued', 'delivering', 'delivered', 'unconfirmed', 'waited', 'pull'))
        -- Only an answer is delivered.
        AND (delivery IS NULL OR status = 'answered')
        -- A lease exactly while delivering.
        AND ((delivery IS NOT DISTINCT FROM 'delivering') = (delivery_claimed_by IS NOT NULL AND delivery_lease_until IS NOT NULL))
        AND char_length(delivery_detail) <= 300);

-- The sweep: owed and in-flight deliveries, oldest first.
CREATE INDEX IF NOT EXISTS idx_attention_asks_delivery ON public.attention_asks (resolved_at)
    WHERE delivery IN ('queued', 'delivering');
