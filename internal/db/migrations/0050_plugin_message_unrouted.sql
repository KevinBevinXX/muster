-- muster 0050 — A PLUGIN MESSAGE THE CHIEF DID NOT DISPOSITION BECOMES AN ASK
-- (internal/plugins/unrouted.go, internal/api/unrouted.go;
-- docs/plugins.md#a-message-the-chief-does-not-act-on).
--
-- A repliable inbound message (no_reply false) delivered to the chief is
-- DISPOSITIONED when one of these exists for it: a session_routes row (it was
-- routed to a session), a plugin_reply_drafts row (a reply was drafted), or
-- plugin_messages.settle_reason (it was settled). The attention sweep raises
-- one operator ask for every delivered, repliable message still not
-- dispositioned plugins.UnroutedAskAfter (15 minutes) after its delivery.
--
--   plugin_messages.settle_reason  why the message needs nothing more: the
--               chief's plugin_message_settle reason, the owner's answer to the
--               ask raised for it, or 'pre-existing' (the backfill below). NULL:
--               not settled. Read by the sweep (a settled message raises no ask,
--               and closes an open one) and shown with the message.
--   attention_asks.plugin_message_id  the message an ask was raised for. Only
--               the sweep writes it. The sweep reads it to raise no second ask
--               and to find the open ask a later disposition closes; answering
--               that ask settles the message through it.
--
-- 🔴 ONE ASK PER MESSAGE, EVER, BY THE SCHEMA: attention_asks_one_per_plugin_message
-- is unique, so a sweep run twice, two processes sweeping at once, or a sweep
-- retried after a crash cannot raise a second ask — whatever the first ask's
-- state (open, answered, cancelled or expired) is now.
--
-- 🔴 MESSAGES FROM BEFORE THIS MIGRATION DO NOT FIRE: every repliable message
-- already delivered is settled 'pre-existing' here, so the first sweep after
-- the deploy raises nothing for them. A message still queued now is delivered
-- after the deploy and is judged like any other.
--
-- No foreign-key owner pin on plugin_message_id: its one writer raises the ask
-- as the message's own owner (row-level security on both tables). ON DELETE
-- SET NULL: an ask is a record and outlives its message (only a notice muster
-- routed itself is ever deleted, and those are no_reply, so never linked).
--
-- Must apply after 0049 (the contiguity test refuses a gap; nothing here
-- depends on what 0049 adds, so if this lands first it is renumbered 0049). Not
-- re-applied by db.RLSOn (it changes no policy, grant or default) and
-- idempotent.
--
-- ROLLBACK. An image from before 0050 runs over this schema unchanged (it never
-- reads or writes the columns). To remove them, after rolling the image back:
--   DROP INDEX IF EXISTS attention_asks_one_per_plugin_message;
--   DROP INDEX IF EXISTS idx_plugin_messages_unsettled;
--   ALTER TABLE attention_asks DROP COLUMN IF EXISTS plugin_message_id;
--   ALTER TABLE plugin_messages DROP COLUMN IF EXISTS settle_reason;
--   DELETE FROM schema_migrations WHERE version = 50;
SET LOCAL lock_timeout = '3s';

ALTER TABLE public.plugin_messages ADD COLUMN IF NOT EXISTS settle_reason TEXT;
ALTER TABLE public.plugin_messages DROP CONSTRAINT IF EXISTS plugin_messages_settle_reason_cap;
ALTER TABLE public.plugin_messages ADD CONSTRAINT plugin_messages_settle_reason_cap
    CHECK (settle_reason IS NULL OR char_length(settle_reason) BETWEEN 1 AND 2000);

ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS plugin_message_id BIGINT
    REFERENCES public.plugin_messages (id) ON DELETE SET NULL;

CREATE UNIQUE INDEX IF NOT EXISTS attention_asks_one_per_plugin_message
    ON public.attention_asks (plugin_message_id) WHERE plugin_message_id IS NOT NULL;

-- The sweep: delivered, repliable messages not yet settled, by delivery time.
CREATE INDEX IF NOT EXISTS idx_plugin_messages_unsettled ON public.plugin_messages (delivered_at)
    WHERE status = 'delivered' AND NOT no_reply AND settle_reason IS NULL;

UPDATE public.plugin_messages SET settle_reason = 'pre-existing'
 WHERE settle_reason IS NULL AND delivered_at IS NOT NULL AND NOT no_reply;
