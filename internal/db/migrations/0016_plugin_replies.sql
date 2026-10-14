-- muster 0016 — PLUGIN REPLIES: what 0015's plugin_reply_drafts needs before a
-- draft can be approved, claimed and settled (internal/plugins/drafts.go), and the
-- `no_reply` mark a plugin sets on a message nobody may answer.
--
-- 🔴 NOTHING HERE REDEFINES AN OBJECT 0015 DEFINES. db.RLSOn re-applies 0015's
-- body (rollback.go), and its CREATE OR REPLACE FUNCTION would silently undo an
-- edit made to one of 0015's functions here. So the no_reply column gets a trigger
-- and a function of its OWN, and the constraints changed below are ones 0015's
-- CREATE TABLE IF NOT EXISTS never re-creates on an existing table.
--
-- 🔴 no_reply IS ENFORCED BY THE DATABASE, NOT ONLY BY THE TOOL. A draft for a
-- message marked no_reply is refused by plugin_reply_drafts_message_replyable on
-- INSERT, whatever wrote it; and the mark is immutable once the message exists,
-- so it cannot be lifted to let a draft through.

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.plugin_messages ADD COLUMN IF NOT EXISTS no_reply BOOLEAN NOT NULL DEFAULT false;

-- The mark is part of what ARRIVED: as immutable as the body.
CREATE OR REPLACE FUNCTION public.plugin_messages_no_reply_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.no_reply IS DISTINCT FROM OLD.no_reply THEN
        RAISE EXCEPTION 'plugin_messages: a message''s no_reply mark is immutable (id %)', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS plugin_messages_no_reply_immutable ON public.plugin_messages;
CREATE TRIGGER plugin_messages_no_reply_immutable BEFORE UPDATE OF no_reply ON public.plugin_messages
    FOR EACH ROW EXECUTE FUNCTION public.plugin_messages_no_reply_immutable();

-- A draft may only answer a message that may be answered. The message is read
-- as the writing principal: one it cannot see reads as "not replyable" and is
-- refused (the composite foreign key would refuse it anyway).
CREATE OR REPLACE FUNCTION public.plugin_reply_drafts_message_replyable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF coalesce((SELECT NOT m.no_reply FROM public.plugin_messages m WHERE m.id = NEW.message_id), false) IS NOT TRUE THEN
        RAISE EXCEPTION 'plugin_reply_drafts: message % may not be replied to (no_reply)', NEW.message_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS plugin_reply_drafts_message_replyable ON public.plugin_reply_drafts;
CREATE TRIGGER plugin_reply_drafts_message_replyable BEFORE INSERT ON public.plugin_reply_drafts
    FOR EACH ROW EXECUTE FUNCTION public.plugin_reply_drafts_message_replyable();

-- 0015 tied approved_at to exactly the approved-and-later states, which left no
-- way to EXPIRE an approved draft nobody claimed without erasing when it was
-- approved. Now: those four states need it, the three undecided-or-refused ones
-- must not have it, and `expired` may carry it either way (expired from pending,
-- or from approved).
ALTER TABLE public.plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_approved_has_time;
ALTER TABLE public.plugin_reply_drafts ADD CONSTRAINT plugin_reply_drafts_approved_has_time CHECK (
    (status IN ('approved', 'sent', 'failed', 'unconfirmed') AND approved_at IS NOT NULL)
    OR (status IN ('pending', 'rejected', 'superseded') AND approved_at IS NULL)
    OR status = 'expired');

-- A claim is a claimed_at AND a lease, together; a claimed draft is approved or
-- past it; and a draft settled as sent or failed was claimed (only a plugin that
-- claimed it can report a result, so a result without a claim is a bug).
ALTER TABLE public.plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_claim_has_lease;
ALTER TABLE public.plugin_reply_drafts ADD CONSTRAINT plugin_reply_drafts_claim_has_lease CHECK (
    (claimed_at IS NULL) = (lease_until IS NULL));
ALTER TABLE public.plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_claimed_is_approved;
ALTER TABLE public.plugin_reply_drafts ADD CONSTRAINT plugin_reply_drafts_claimed_is_approved CHECK (
    claimed_at IS NULL OR status IN ('approved', 'sent', 'failed', 'unconfirmed'));
ALTER TABLE public.plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_result_was_claimed;
ALTER TABLE public.plugin_reply_drafts ADD CONSTRAINT plugin_reply_drafts_result_was_claimed CHECK (
    status NOT IN ('sent', 'failed') OR (claimed_at IS NOT NULL AND finished_at IS NOT NULL));

-- The claim's lookup: a plugin's approved, unclaimed drafts, oldest approval first.
CREATE INDEX IF NOT EXISTS idx_plugin_reply_drafts_claimable
    ON public.plugin_reply_drafts (message_id) WHERE status = 'approved' AND claimed_at IS NULL;
