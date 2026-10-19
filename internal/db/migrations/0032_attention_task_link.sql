-- muster 0032 — AN ATTENTION ASK NAMES THE TASK IT IS ABOUT (task_dispatch's
-- instructions provenance, internal/api/task_routing.go).
--
-- task_dispatch's `instructions` are accepted only when they are, verbatim, the
-- owner's answer to an ask about THAT task. Until now the only trace of which
-- task an ask was about was its question's text ("Task #N needs you: …"), which
-- any asker can write. task_id is the structural link: written only by
-- muster's own task path (askOwnerAboutTask: task_route_attention and a
-- mention's attention route), never by an agent's attention_ask.
--
-- 🔴 THE TASK MUST BE THE ASK OWNER'S, CHECKED BY A TRIGGER, NOT A COMPOSITE
-- KEY — 0027's shape and for 0027's reason: a composite FK onto notes (id,
-- owner_id) would depend on notes_id_owner_key, which 0006's body drops when it
-- is re-run (TestMigration0006BodyReRunsCleanly). So a plain FK to notes (id)
-- and a BEFORE trigger reading the task's owner as the writing principal (a
-- task it cannot see reads as no owner and is refused). Deleting the task
-- un-links the ask (SET NULL); the ask itself stays.

-- BACKFILL: an ask filed before this column existed is linked to task N only
-- when BOTH hold — its question begins "Task #N needs you: " AND the same owner
-- has the chief's `task.chief_routed` audit row for note:N, tool
-- task_route_attention, written within 5 seconds of the ask (the two are
-- written by one call, ask first). The text alone is never enough.
--
-- Must apply after 0031 (feat/attention-answer-delivery; the contiguity test
-- refuses a gap). Not
-- re-applied by db.RLSOn (no policy, grant or default) and idempotent.

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS task_id BIGINT;

ALTER TABLE public.attention_asks DROP CONSTRAINT IF EXISTS attention_asks_task_id_fkey;
ALTER TABLE public.attention_asks
    ADD CONSTRAINT attention_asks_task_id_fkey
    FOREIGN KEY (task_id) REFERENCES public.notes (id) ON DELETE SET NULL;

CREATE OR REPLACE FUNCTION public.attention_asks_task_is_owners() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.task_id IS NOT NULL
       AND NEW.owner_id IS DISTINCT FROM (SELECT n.owner_id FROM public.notes n WHERE n.id = NEW.task_id) THEN
        RAISE EXCEPTION 'attention_asks: owner % is not the owner of task %', NEW.owner_id, NEW.task_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS attention_asks_task_is_owners ON public.attention_asks;
CREATE TRIGGER attention_asks_task_is_owners BEFORE INSERT OR UPDATE OF owner_id, task_id
    ON public.attention_asks FOR EACH ROW EXECUTE FUNCTION public.attention_asks_task_is_owners();

CREATE INDEX IF NOT EXISTS idx_attention_asks_task ON public.attention_asks (task_id) WHERE task_id IS NOT NULL;

UPDATE public.attention_asks a
   SET task_id = r.task
  FROM (
    SELECT a2.id AS ask, substring(a2.question FROM '^Task #([0-9]+) needs you: ')::bigint AS task
      FROM public.attention_asks a2
     WHERE a2.task_id IS NULL AND a2.question ~ '^Task #[0-9]+ needs you: '
  ) r
 WHERE a.id = r.ask
   AND EXISTS (SELECT 1 FROM public.notes n WHERE n.id = r.task AND n.owner_id = a.owner_id)
   AND EXISTS (
       SELECT 1 FROM public.audit_log l
        WHERE l.action = 'task.chief_routed'
          AND l.object = 'note:' || r.task
          AND l.user_id = a.owner_id
          AND l.detail ->> 'tool' = 'task_route_attention'
          AND l.at BETWEEN a.created_at AND a.created_at + interval '5 seconds');
