-- muster 0027 — A TASK EVENT FOR THE CHIEF (internal/api/task_routing.go).
--
-- With MUSTER_TASK_ROUTING=1, a plugin that files work through
-- POST /plugin/v1/tasks may ask for the event to be handed to the owner's chief
-- (`route_to_chief`). muster then stores ONE plugin message for it, written by
-- muster itself, and delivers it to the chief through the ordinary plugin
-- delivery (queue, session claim, retry on claim, crash recovery). The chief
-- decides what happens to the task: the attention queue, a forward to the agent
-- already working it, or a new agent.
--
-- 🔴 task_id IS WHAT MAKES A ROW A TASK EVENT, AND ONLY muster WRITES IT. The
-- inbound-message route (POST /plugin/v1/messages) never names task_id in what
-- it stores, so no plugin can make its own text look like a task event;
-- internal/plugins' SubmitTaskEvent is the one writer. A row with a task_id is
-- rendered by muster's task-event renderer, never as mail.
--
-- 🔴 THE TASK MUST BE THE MESSAGE OWNER'S, CHECKED BY A TRIGGER, NOT A
-- COMPOSITE KEY. Every owned link since 0006 is a composite (id, owner_id) FK,
-- but one onto notes (id, owner_id) would depend on notes_id_owner_key, which
-- 0006's body drops and re-adds when it is re-run (it drops only the child FKs
-- it knows) — TestMigration0006BodyReRunsCleanly measured exactly that refusal.
-- So: a plain FK to notes (id), and 0015's shape for agent_id — a BEFORE
-- trigger reading the task's owner as the writing principal (a task it cannot
-- see reads as no owner and is refused). ON DELETE CASCADE: a deleted task
-- leaves nothing to route. A NULL task_id (every ordinary message) passes.
--
-- Not re-applied by db.RLSOn (it changes no policy, grant or default) and
-- idempotent anyway. 0015's body, which RLSOn may re-apply, does not name the
-- column, so a re-apply leaves it in place.

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.plugin_messages ADD COLUMN IF NOT EXISTS task_id BIGINT;

ALTER TABLE public.plugin_messages DROP CONSTRAINT IF EXISTS plugin_messages_task_id_fkey;
ALTER TABLE public.plugin_messages
    ADD CONSTRAINT plugin_messages_task_id_fkey
    FOREIGN KEY (task_id) REFERENCES public.notes (id) ON DELETE CASCADE;

CREATE OR REPLACE FUNCTION public.plugin_messages_task_is_owners() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.task_id IS NOT NULL
       AND NEW.owner_id IS DISTINCT FROM (SELECT n.owner_id FROM public.notes n WHERE n.id = NEW.task_id) THEN
        RAISE EXCEPTION 'plugin_messages: owner % is not the owner of task %', NEW.owner_id, NEW.task_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS plugin_messages_task_is_owners ON public.plugin_messages;
CREATE TRIGGER plugin_messages_task_is_owners BEFORE INSERT OR UPDATE OF owner_id, task_id
    ON public.plugin_messages FOR EACH ROW EXECUTE FUNCTION public.plugin_messages_task_is_owners();

-- The cascade from notes, and the attention view's "not yet routed" read.
CREATE INDEX IF NOT EXISTS idx_plugin_messages_task ON public.plugin_messages (task_id) WHERE task_id IS NOT NULL;
