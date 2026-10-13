-- muster 0014 — BROWSER-FLOW REQUESTS FROM AGENTS: the per-run approval row an
-- agent files through the MCP tool `browser_flow_request` (internal/flows), and
-- that the agent's OWNER approves or denies on /flows.
--
-- 🔴 AN AGENT NEVER ENQUEUES. Its request is a row here, `pending`; only the
-- owner's browser session turns it into a flow_jobs row (approve), and only then
-- can a runner see it. Deny and expiry never produce a job.
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, IN 0008's SHAPE, and owner_id is
-- the AGENT's owner, pinned by flow_requests_owner_is_agents_owner (below):
-- a request cannot claim to belong to anyone but the agent's owner, so a
-- member's agent can only ever produce a job in that member's own queue — which
-- only that member's runner tokens can claim.
--
-- ⚠ VERSION 0014, NOT 0013: 0013 is muster web push's (a parallel arc). If this
-- migration is APPLIED to a database before 0013 is, a later image carrying
-- 0013 refuses to boot (db.checkNoSkippedMigration). Ship 0013 first, or
-- renumber.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.flow_requests (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id     BIGINT      NOT NULL
                 DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                 REFERENCES public.users (id),
    -- 🔴 SET NULL, NOT CASCADE: deleting an agent must not delete the record of
    -- what it asked for and what its owner decided. agent_name stays.
    agent_id     BIGINT      REFERENCES public.agents (id) ON DELETE SET NULL,
    agent_name   TEXT        NOT NULL,
    flow         TEXT        NOT NULL,
    host         TEXT        NOT NULL,
    instance     TEXT        NOT NULL,
    params       JSONB       NOT NULL DEFAULT '{}'::jsonb,
    reason       TEXT        NOT NULL DEFAULT '',
    status       TEXT        NOT NULL DEFAULT 'pending',
    job_id       BIGINT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    decided_at   TIMESTAMPTZ,
    CONSTRAINT flow_requests_status_check CHECK (status IN ('pending', 'approved', 'denied', 'expired')),
    CONSTRAINT flow_requests_approved_has_job CHECK ((status = 'approved') = (job_id IS NOT NULL)),
    CONSTRAINT flow_requests_reason_cap CHECK (octet_length(reason) <= 2000)
);

-- 🔴 A TRIGGER, NOT THE COMPOSITE FOREIGN KEY 0006 USES FOR ITS CHILDREN. A
-- foreign key to agents(id, owner_id) would depend on agents_id_owner_key,
-- which 0006's (idempotent, re-runnable) body drops and re-creates — and a
-- later table depending on it makes that re-run fail. The check is the same
-- relationship: the row's owner IS the agent's owner. It reads agents as the
-- inserting principal, so an agent the principal cannot see reads as no owner
-- and is refused too.
CREATE OR REPLACE FUNCTION public.flow_requests_owner_is_agents_owner() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    -- An agent's deletion nulls agent_id (the foreign key's action): the row
    -- keeps its owner and is no longer anyone's to re-point. A NEW row must
    -- name its agent.
    IF TG_OP = 'UPDATE' AND NEW.agent_id IS NULL AND NEW.owner_id = OLD.owner_id THEN
        RETURN NEW;
    END IF;
    IF NEW.owner_id IS DISTINCT FROM (SELECT a.owner_id FROM public.agents a WHERE a.id = NEW.agent_id) THEN
        RAISE EXCEPTION 'flow_requests: owner % is not the owner of agent %', NEW.owner_id, NEW.agent_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS flow_requests_owner_is_agents_owner ON public.flow_requests;
CREATE TRIGGER flow_requests_owner_is_agents_owner BEFORE INSERT OR UPDATE OF owner_id, agent_id
    ON public.flow_requests FOR EACH ROW EXECUTE FUNCTION public.flow_requests_owner_is_agents_owner();

CREATE INDEX IF NOT EXISTS idx_flow_requests_owner_status ON public.flow_requests (owner_id, status, id);
CREATE INDEX IF NOT EXISTS idx_flow_requests_agent ON public.flow_requests (agent_id, status);

ALTER TABLE public.flow_requests ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.flow_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.flow_requests FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.flow_requests;
CREATE POLICY owner_isolation ON public.flow_requests
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.flow_requests TO muster_app;
GRANT USAGE, SELECT, UPDATE ON SEQUENCE public.flow_requests_id_seq TO muster_app;
