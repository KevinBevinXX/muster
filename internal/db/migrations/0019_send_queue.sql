-- muster 0019 — THE SEND QUEUE (internal/sendqueue): messages a user queued
-- for a mirrored Claude Code window, delivered by the server ONE AT A TIME,
-- each only once that window's Claude Code session has been idle for a while.
-- The queue is server-side and per user, so it survives the page closing and is
-- visible (and cancellable) from any of the owner's devices.
--
-- A row names a mirrored window only: (host, tmux session, tmux window id).
-- Dispatched agents are not queued for, so there is no agent column and no
-- kind column; a second kind of target would add them in its own migration.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, IN 0017's SHAPE. owner_isolation
-- keeps one user from reading, queueing into or cancelling another's rows. The
-- dispatcher reads every owner's heads under the system scope and DELIVERS each
-- row as its owner (internal/api).
--
-- 🔴 A ROW IS CLAIMED BY A STATE TRANSITION, AND A CLAIM IS NEVER RETRIED.
-- queued → sending happens in one UPDATE conditional on the row still being
-- queued; at most one row per window is 'sending' (the partial unique index),
-- so two dispatchers cannot both deliver one window's head. A row left
-- 'sending' by a process that died mid-send is moved to 'failed' (with
-- may_have_delivered = true), never back to 'queued': the message may have
-- been pasted, and pasting it twice is the worse failure.
--
-- 🔴 A CLAIM IS A LEASE. The claiming process writes its instance id
-- (claimed_by) and lease_until, and renews lease_until while it sends; only a
-- row whose lease has EXPIRED is failed as abandoned, and only the claimant
-- finishes its row. During a rolling update two muster pods run at once, and
-- the new one must not fail the old one's live send.
--
-- 🔴 after_since IS ON THE HOST'S CLOCK. It is the window's Claude Code
-- status time (statusUpdatedAt, Unix ms, as the host reports it) that the hub
-- held when a message was pasted into the window: the next message is released
-- only after a NEWER one. It is compared only with other host times, never with
-- muster's clock, so a host clock that is ahead or behind cannot stall the queue
-- or release it early.
--
-- 🔴 stepup_at IS THE STEP-UP DECISION, STORED. A mirror send types into the
-- operator's session and needs a FRESH sign-in (internal/stepup). It is checked
-- when the message is QUEUED, and the sign-in time of the session that queued it
-- is recorded here; the delivery presents exactly that time to the same gate, so
-- a queued message is sent only while that sign-in is still within the step-up
-- window — after that it FAILS, saying so, and is not sent.
--
-- 🔴 IT IS RE-APPLIED BY db.RLSOn (rollback.go) WHEN THE TABLE EXISTS, SO EVERY
-- STATEMENT IS IDEMPOTENT.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.send_queue (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id           BIGINT      NOT NULL
                       DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                       REFERENCES public.users (id),
    mirror_host        TEXT        NOT NULL,
    mirror_session     TEXT        NOT NULL,
    -- '' is a per-session host's session (no window id).
    mirror_window      TEXT        NOT NULL,
    body               TEXT        NOT NULL,
    stepup_at          TIMESTAMPTZ NOT NULL,
    state              TEXT        NOT NULL DEFAULT 'queued',
    detail             TEXT        NOT NULL DEFAULT '',
    -- Whether a send was ATTEMPTED far enough that the message may have
    -- reached the window: what the next message waits behind (a turn).
    may_have_delivered BOOLEAN     NOT NULL DEFAULT false,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_at         TIMESTAMPTZ,
    finished_at        TIMESTAMPTZ,
    -- The host status time held at the latest paste into the window that this
    -- row knows of (0: none): on a sent row, its own paste's; on a queued row,
    -- the latest manual Send's while it waited.
    after_since        BIGINT      NOT NULL DEFAULT 0,
    -- The dispatcher instance that claimed the row, and until when its claim
    -- holds unless renewed. Set on every claim.
    claimed_by         TEXT,
    lease_until        TIMESTAMPTZ,
    -- The wire's own bounds (internal/mirror ValidHost / ValidSession /
    -- ValidWindowID), as 0017 spells them; the store validates first.
    CONSTRAINT send_queue_mirror_host_cap CHECK (char_length(mirror_host) BETWEEN 1 AND 63),
    CONSTRAINT send_queue_mirror_session_cap CHECK (octet_length(mirror_session) BETWEEN 1 AND 128),
    CONSTRAINT send_queue_mirror_window_check CHECK (mirror_window = '' OR mirror_window ~ '^@[0-9]{1,10}$'),
    -- mirror.MaxPaste (16 KiB).
    CONSTRAINT send_queue_body_cap CHECK (octet_length(body) BETWEEN 1 AND 16384),
    CONSTRAINT send_queue_state_check CHECK (state IN ('queued', 'sending', 'sent', 'failed', 'cancelled')),
    CONSTRAINT send_queue_detail_cap CHECK (octet_length(detail) <= 1024),
    CONSTRAINT send_queue_claimed_check CHECK (state IN ('queued', 'cancelled') OR claimed_at IS NOT NULL),
    CONSTRAINT send_queue_finished_check CHECK ((state IN ('queued', 'sending')) = (finished_at IS NULL)),
    CONSTRAINT send_queue_after_since_check CHECK (after_since >= 0),
    CONSTRAINT send_queue_claimed_by_cap CHECK (claimed_by IS NULL OR octet_length(claimed_by) BETWEEN 1 AND 128),
    CONSTRAINT send_queue_lease_check CHECK (state <> 'sending' OR (claimed_by IS NOT NULL AND lease_until IS NOT NULL))
);

-- At most ONE row per window is being sent: the claim's own guarantee, held by
-- the schema so that two dispatchers cannot both claim a window's head.
CREATE UNIQUE INDEX IF NOT EXISTS send_queue_one_sending
    ON public.send_queue (owner_id, mirror_host, mirror_session, mirror_window)
    WHERE state = 'sending';
-- The dispatcher's scan: every window's open rows, oldest first.
CREATE INDEX IF NOT EXISTS idx_send_queue_open ON public.send_queue (id) WHERE state IN ('queued', 'sending');

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.send_queue ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.send_queue ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.send_queue FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.send_queue;
CREATE POLICY owner_isolation ON public.send_queue
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.send_queue TO muster_app;
GRANT USAGE, SELECT ON SEQUENCE public.send_queue_id_seq TO muster_app;
