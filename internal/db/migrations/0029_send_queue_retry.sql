-- muster 0029 — A FAILED QUEUED MESSAGE CAN BE RETRIED (internal/sendqueue
-- Retry, internal/api handleSendQueueRetry).
--
-- A Retry queues the failed row's body again as a NEW row (a new id, so a new
-- delivery nonce: the send path's repeat guard keys on the row) under the
-- sign-in of the request that retried it. The failed row itself stays failed —
-- what the dispatcher reads from it (a may-have-delivered row's turn) is
-- unchanged — and records the row it was retried as, so its Retry is drawn
-- once and a second Retry (a double tap, a second device) is refused rather
-- than queueing the message twice.
--
-- 🔴 retried_as IS SET ONLY ON A FAILED ROW (the check), and only once: the
-- store sets it in the same transaction as the insert, under the row's lock
-- (SELECT … FOR UPDATE), after checking it is NULL. No foreign key: Prune deletes finished rows by age, and a retry row
-- going first must not make the old row retryable again.
--
-- ⚠ 0019's note on stepup_at ("the delivery presents exactly that time to the
-- same gate") no longer describes the delivery: since this change the
-- delivery checks that stepup_at was inside the step-up window when the row
-- was created and that the row is at most 7 days old (sendqueue.AuthorizedFor,
-- the operator's decision), then sends (internal/api sendQueueAuthorization).
--
-- Not re-applied by db.RLSOn (no policy, grant or default) and idempotent.

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.send_queue ADD COLUMN IF NOT EXISTS retried_as BIGINT;
ALTER TABLE public.send_queue DROP CONSTRAINT IF EXISTS send_queue_retried_as_check;
ALTER TABLE public.send_queue ADD CONSTRAINT send_queue_retried_as_check CHECK (retried_as IS NULL OR state = 'failed');
