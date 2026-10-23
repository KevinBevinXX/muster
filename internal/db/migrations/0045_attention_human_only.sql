-- muster 0045 — A HUMAN-ONLY ASK, AND A DISPATCH THAT NEEDS ONE.
--
-- Operator decision (muse dispatch): "each dispatch raises a muster Attention
-- ask (phone) showing the brief; queued only after you tap approve; denied or
-- unanswered = never sent." Two halves, both here:
--
-- (1) attention_asks.human_only. Such an ask is raised through the personal-
--     token API (POST /api/attention/asks) and can be answered ONLY on a human
--     surface: the /attention view in a browser session, which is also what
--     the phone opens from its notification. answered_via is NULL there. An
--     MCP attention_answer (answered_via 'mcp') and the answered-in-session
--     path are refused by the store, and the CHECK below states the rule again,
--     so a bug in the store cannot write a non-human answer onto one.
--     payload is the exact text the approval covers (a muse.send brief,
--     shown on the card), and payload_sha256 is its hex sha256, computed by
--     the server.
--
-- (2) flow_jobs.approval_ask_id, plus attention_asks.consumed_job_id and
--     consumed_at. A muse.send that carries `marker` (a dispatch) is enqueued
--     only together with an approved, unexpired, UNUSED human-only ask whose
--     payload_sha256 is sha256(the job's text). The ask is consumed in the
--     SAME transaction as the insert, so one approval queues at most one job.
--     The personal token can POST /api/flows/jobs directly, so this is
--     enforced in the store, not in the client.
--
-- Row-level security is unchanged: both tables' owner policies cover every
-- column. Additive; ROLLBACK: an older image ignores the columns, which means
-- it ALSO stops requiring an approval for a dispatch, and attention_answer can
-- again answer a human-only ask (the CHECK below would refuse that write and
-- the older image would surface a 500). To remove: drop the constraints and
-- columns, and delete schema_migrations row 45.

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.attention_asks
    ADD COLUMN IF NOT EXISTS human_only      BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS payload         TEXT,
    ADD COLUMN IF NOT EXISTS payload_sha256  TEXT,
    ADD COLUMN IF NOT EXISTS consumed_job_id BIGINT,
    ADD COLUMN IF NOT EXISTS consumed_at     TIMESTAMPTZ;

ALTER TABLE public.attention_asks DROP CONSTRAINT IF EXISTS attention_asks_human_only_check;
ALTER TABLE public.attention_asks
    ADD CONSTRAINT attention_asks_human_only_check CHECK (
        -- a human-only ask is answered on a human surface only
        (NOT human_only OR answered_via IS NULL)
        -- and never "answered in session" (a message to an asker it has none of)
        AND (NOT human_only OR status <> 'answered_in_session')
        -- the payload belongs to a human-only ask, with its digest
        AND (payload IS NULL OR human_only)
        AND ((payload IS NULL) = (payload_sha256 IS NULL))
        AND (payload_sha256 IS NULL OR payload_sha256 ~ '^[0-9a-f]{64}$')
        AND (payload IS NULL OR char_length(payload) BETWEEN 1 AND 8000)
        -- consumption is all-or-nothing, and only of an answered human-only ask
        AND ((consumed_at IS NULL) = (consumed_job_id IS NULL))
        AND (consumed_at IS NULL OR (human_only AND status = 'answered'))
    );

ALTER TABLE public.flow_jobs ADD COLUMN IF NOT EXISTS approval_ask_id BIGINT;
-- One approval, one job, as an index as well as the store's conditional UPDATE.
CREATE UNIQUE INDEX IF NOT EXISTS flow_jobs_one_job_per_approval
    ON public.flow_jobs (approval_ask_id) WHERE approval_ask_id IS NOT NULL;
