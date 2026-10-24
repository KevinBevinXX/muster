-- muster 0050 — A PLUGIN MESSAGE THE CHIEF DID NOT ACT ON BECOMES AN ASK.
--
--   plugin_messages.settle_reason      why the message needs nothing more; NULL: not settled.
--   attention_asks.plugin_message_id   the message muster raised the ask for; unique.
--
-- The backfill settles every message already awaiting a disposition as
-- 'pre-existing', so nothing fires at the deploy; its WHERE is the sweep's own
-- predicate (internal/plugins awaitsDisposition). The rules, and why:
-- docs/plugins.md#a-message-the-chief-does-not-act-on.
--
-- Must apply after 0049. Not re-applied by db.RLSOn; idempotent.
--
-- ROLLBACK (after rolling the image back; an older image never reads these):
--   DROP INDEX IF EXISTS attention_asks_one_per_plugin_message;
--   ALTER TABLE attention_asks DROP COLUMN IF EXISTS plugin_message_id;
--   ALTER TABLE plugin_messages DROP COLUMN IF EXISTS settle_reason;
--   DELETE FROM schema_migrations WHERE version = 50;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.plugin_messages ADD COLUMN IF NOT EXISTS settle_reason TEXT;

ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS plugin_message_id BIGINT
    REFERENCES public.plugin_messages (id) ON DELETE SET NULL;

CREATE UNIQUE INDEX IF NOT EXISTS attention_asks_one_per_plugin_message
    ON public.attention_asks (plugin_message_id) WHERE plugin_message_id IS NOT NULL;

UPDATE public.plugin_messages m SET settle_reason = 'pre-existing'
 WHERE m.settle_reason IS NULL AND m.status = 'delivered' AND NOT m.no_reply;
