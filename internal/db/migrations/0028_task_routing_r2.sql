-- muster 0028 — TASK ROUTING, ROUND 2 (internal/api/task_routing.go).
--
-- 1. AN ASK MUSTER ITSELF RAISES. attention_asks had exactly two askers: a
--    dispatched agent or a mirrored session. A plugin's mention of an
--    in-progress task, for an owner with no chief, must still reach the owner's
--    attention queue, so an ask may now be raised by muster (system_asker):
--    no agent, no session. Existing rows default to false and keep their asker,
--    and the check still allows exactly ONE kind of asker per row.
--
-- 2. A NOTICE ROUTED AROUND THE CHIEF IS A RESERVED ROW. A mention muster takes
--    to the agent on the task or to the attention queue is recorded as a
--    plugin_messages row under the notice's own external_id BEFORE it is acted
--    on: the (plugin_id, external_id) unique key makes the reservation atomic
--    (two identical sends act once), and the row counts against the plugin's
--    task-event hour like a task event. routed_to says where it went; it is
--    'delivered' from birth, so no delivery ever hands it to the chief, and
--    UnroutedTaskEvents leaves it out (it is not a chief event).
--
-- Not re-applied by db.RLSOn (no policy, grant or default) and idempotent.

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS system_asker BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE public.attention_asks DROP CONSTRAINT IF EXISTS attention_asks_asker_check;
ALTER TABLE public.attention_asks ADD CONSTRAINT attention_asks_asker_check CHECK (
    (agent_id IS NOT NULL AND mirror_host IS NULL AND mirror_session IS NULL AND NOT system_asker)
    OR (agent_id IS NULL AND mirror_host IS NOT NULL AND mirror_session IS NOT NULL AND NOT system_asker)
    OR (agent_id IS NULL AND mirror_host IS NULL AND mirror_session IS NULL AND system_asker));

ALTER TABLE public.plugin_messages ADD COLUMN IF NOT EXISTS routed_to TEXT;
ALTER TABLE public.plugin_messages DROP CONSTRAINT IF EXISTS plugin_messages_routed_to_check;
ALTER TABLE public.plugin_messages ADD CONSTRAINT plugin_messages_routed_to_check CHECK (
    routed_to IS NULL OR (routed_to IN ('agent', 'attention') AND task_id IS NOT NULL AND status = 'delivered'));
