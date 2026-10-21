-- muster 0040 — EDITING A QUEUED MESSAGE (internal/sendqueue Hold, SaveEdit,
-- ReleaseEdit): the ⋯ menu's Edit opens a queued message's text in its row,
-- and while it is open the row is HELD — the dispatcher never sends it.
--
-- ⚠ NUMBERED 0040 AHEAD OF ITS PREDECESSORS: 0038 (the Hermes agent kind, #192)
-- and 0039 (Android devices, #186) are in open pull requests and merge first
-- (or this is renumbered to the next free number);
-- TestEmbeddedMigrationsAreContiguousFrom0001 refuses a tree with a gap.
--
-- 🔴 A HOLD IS A LEASE ON THE DATABASE'S CLOCK, LIKE A CLAIM. held_until is
-- written as now() + the hold (sendqueue.EditHold, 10 minutes) and compared
-- with now(), so a tab that was closed mid-edit cannot freeze the window's queue:
-- once held_until has passed the row is queued again with its ORIGINAL text
-- (the body is only ever replaced by a Save, under a live hold).
--
-- 🔴 A LAPSED HOLD IS TAKEN OFF THE ROW by the dispatcher's next pass
-- (sendqueue ClearLapsedHolds) and by a claim: held_until and hold_token go
-- back to NULL and hold_lapsed_at records that the edit ran out, for the list
-- to say so. So a row carries the hold columns only while its hold is LIVE
-- (at most 10 minutes and a pass): an older image, whose claim, cancel and
-- sweeps do not clear them and so trip send_queue_hold_check, meets them only
-- on a row someone is editing right now. hold_lapsed_at has no CHECK.
--
-- 🔴 THE CLAIM SKIPS A HELD ROW IN THE SAME CONDITIONAL UPDATE. Hold (state
-- 'queued' → held) and Claim (queued and not held → 'sending') are each one
-- UPDATE of the one row, so the row lock orders them and the loser's WHERE is
-- re-checked against the winner's row: a held row is never claimed, and a
-- claimed row is never held. A Save replaces body only while its own hold
-- (hold_token) is live, so a claim reads either the old text or the whole new
-- one, never a half-edited one — the edit itself lives in the browser until Save.
--
-- 🔴 edit_stepup_at IS THE SAVE'S STEP-UP DECISION, STORED, AS stepup_at IS
-- THE QUEUE POST'S. An edit changes what will be pasted into the session, so
-- Save needs the same fresh sign-in a queue post needs, and the delivery
-- checks it again from the row (internal/api sendQueueAuthorization and
-- sendQueueOwnerRefusal) — the account's cutoffs against BOTH sign-ins.
--
-- Not re-applied by db.RLSOn (it changes no policy, grant or default), so it
-- need not be idempotent; it is anyway.

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.send_queue ADD COLUMN IF NOT EXISTS held_until TIMESTAMPTZ;
ALTER TABLE public.send_queue ADD COLUMN IF NOT EXISTS hold_token TEXT;
ALTER TABLE public.send_queue ADD COLUMN IF NOT EXISTS edited_at TIMESTAMPTZ;
ALTER TABLE public.send_queue ADD COLUMN IF NOT EXISTS edit_stepup_at TIMESTAMPTZ;
ALTER TABLE public.send_queue ADD COLUMN IF NOT EXISTS hold_lapsed_at TIMESTAMPTZ;

-- A hold and its token come and go together, and only a QUEUED row has one:
-- every transition out of 'queued' (claim, cancel, expiry) clears it.
ALTER TABLE public.send_queue DROP CONSTRAINT IF EXISTS send_queue_hold_check;
ALTER TABLE public.send_queue
    ADD CONSTRAINT send_queue_hold_check CHECK (
        (held_until IS NULL) = (hold_token IS NULL)
        AND (held_until IS NULL OR state = 'queued')
        AND (hold_token IS NULL OR octet_length(hold_token) BETWEEN 16 AND 64));
-- An edit is its time and the sign-in that saved it, together.
ALTER TABLE public.send_queue DROP CONSTRAINT IF EXISTS send_queue_edit_check;
ALTER TABLE public.send_queue
    ADD CONSTRAINT send_queue_edit_check CHECK ((edited_at IS NULL) = (edit_stepup_at IS NULL));
