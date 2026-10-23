-- muster 0045 — WHEN AN AGENT ENTERED `stopped`, and the reaper's claim on it
-- (the stopped-agent reaper, internal/api agent_reap.go;
-- MUSTER_AGENT_REAP_STOPPED_AFTER).
--
-- agents.stopped_at is the moment the row's status last moved INTO 'stopped'.
-- It is NULL for a row that has not entered 'stopped' since this migration,
-- and it is KEPT when the row leaves 'stopped' (it reads "last stopped at"):
-- the reaper selects on status = 'stopped' as well, so a running or errored
-- row's old value is never read as a stop. agents.reap_claimed_at is the
-- stopped-agent reaper's claim on the row: set by one conditional UPDATE just
-- before the reaper starts the agent's teardown, so two pods sweeping at once
-- never both destroy it.
--
-- 🔴 THE TRIGGER, NOT THE APPLICATION, OWNS THE STAMP. A row reaches 'stopped'
-- through several writers — the provisioner's Stop and a saved Dispatch, and
-- the reconciler persisting a computed status (UpdateStatus) — so stamping in
-- each writer would be a rule spelled at every site and missed at the next one.
-- So on every INSERT, and every UPDATE that writes status: when the status
-- becomes 'stopped' from anything else (or a row is inserted 'stopped'),
-- stopped_at := now() and reap_claimed_at := NULL — a fresh clock. Every other write leaves both as the
-- statement wrote them. That is what makes a Start reset the clock: Start
-- moves the row out of 'stopped', so its next Stop is a new transition and
-- stamps a new time; a restarted then re-stopped agent waits a full window.
--
-- BACKFILL: every row already 'stopped' gets stopped_at = now(), the moment
-- this migration runs. Nothing before 0045 recorded when a row stopped:
-- updated_at is also moved by a model or display-name change and by kickoff
-- bookkeeping, so it is not the stop time, and reading it as one would reap
-- every agent stopped long ago on the first sweep after deploy. Starting the
-- clock at migration time means nothing already stopped is reaped until a full
-- MUSTER_AGENT_REAP_STOPPED_AFTER after this migration.
--
-- Rollback: an image from before 0045 never reads these columns, and the
-- trigger only writes them. To remove them:
--   DROP TRIGGER agents_stopped_at ON public.agents;
--   DROP FUNCTION public.muster_agent_stopped_at();
--   ALTER TABLE public.agents DROP COLUMN stopped_at, DROP COLUMN reap_claimed_at;
--   DELETE FROM schema_migrations WHERE version = 45;

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.agents ADD COLUMN IF NOT EXISTS stopped_at TIMESTAMPTZ;
ALTER TABLE public.agents ADD COLUMN IF NOT EXISTS reap_claimed_at TIMESTAMPTZ;

UPDATE public.agents SET stopped_at = now() WHERE status = 'stopped' AND stopped_at IS NULL;

CREATE OR REPLACE FUNCTION public.muster_agent_stopped_at() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $$
BEGIN
    IF NEW.status = 'stopped' AND (TG_OP = 'INSERT' OR OLD.status IS DISTINCT FROM 'stopped') THEN
        NEW.stopped_at := now();
        NEW.reap_claimed_at := NULL;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS agents_stopped_at ON public.agents;
CREATE TRIGGER agents_stopped_at
    BEFORE INSERT OR UPDATE OF status ON public.agents
    FOR EACH ROW
    EXECUTE FUNCTION public.muster_agent_stopped_at();

-- The reaper's scan: stopped rows by when they stopped.
CREATE INDEX IF NOT EXISTS agents_stopped_at_idx ON public.agents (stopped_at) WHERE status = 'stopped';
