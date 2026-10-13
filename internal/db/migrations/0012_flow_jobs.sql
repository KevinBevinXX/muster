-- muster 0012 — BROWSER FLOWS: the owner-scoped job queue a host-side runner
-- drains (internal/flows), and the per-(owner, host) RUNNER TOKENS it drains it
-- with.
--
-- 🔴 BOTH TABLES ARE OWNED AND UNDER FORCED ROW-LEVEL SECURITY, IN 0008's SHAPE.
-- owner_id defaults to the acting principal's muster.uid, so an insert with no
-- principal is refused rather than landing as somebody. A runner token is
-- (owner, host): the runner door authenticates it under authz.System (it must be
-- found by its hash before anyone is known) and then acts AS ITS OWNER, so the
-- owner_isolation policy below is what keeps one user's runner from ever seeing,
-- claiming or reporting another user's job — the scoping is the database's, not
-- a WHERE clause a handler could forget.
--
-- Only the sha256 of a runner token is stored; the token is shown once.
--
-- 🔴 AT MOST ONE RUNNING JOB PER (owner, host, instance) IS A UNIQUE INDEX, NOT A
-- CONVENTION: one browser profile is one operator, and two flows typing into
-- it at once is the hazard the host-side flock exists for. The claim's own
-- NOT EXISTS is the fast path; this index is what makes a race lose.
--
-- 🔴 A JOB'S INPUT IS IMMUTABLE. flow_jobs_input_immutable refuses an UPDATE
-- that changes who asked for what (owner, flow, class, host, instance, params,
-- requested_via, requested_by, created_at, expires_at): the row is the audit
-- record, and a compromised runner can lie about a RESULT but never rewrite
-- what it was asked to do.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.flow_runner_tokens (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id     BIGINT      NOT NULL
                 DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                 REFERENCES public.users (id),
    host         TEXT        NOT NULL,
    token_hash   TEXT        NOT NULL UNIQUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at   TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    CONSTRAINT flow_runner_tokens_host_check CHECK (host ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$')
);

CREATE INDEX IF NOT EXISTS idx_flow_runner_tokens_owner ON public.flow_runner_tokens (owner_id);

CREATE TABLE IF NOT EXISTS public.flow_jobs (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id        BIGINT      NOT NULL
                    DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                    REFERENCES public.users (id),
    flow            TEXT        NOT NULL,
    class           TEXT        NOT NULL,
    host            TEXT        NOT NULL,
    instance        TEXT        NOT NULL,
    params          JSONB       NOT NULL DEFAULT '{}'::jsonb,
    status          TEXT        NOT NULL DEFAULT 'queued',
    -- Which door enqueued it (authz.Via: session, api-token) and a label for
    -- who (a token's name). Audit facts, not authorization inputs.
    requested_via   TEXT        NOT NULL,
    requested_by    TEXT        NOT NULL DEFAULT '',
    attempts        INTEGER     NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A queued job past this instant becomes `expired` and is never claimed.
    expires_at      TIMESTAMPTZ NOT NULL,
    claimed_at      TIMESTAMPTZ,
    lease_until     TIMESTAMPTZ,
    finished_at     TIMESTAMPTZ,
    runner_token_id BIGINT,
    exit_code       INTEGER,
    output          TEXT        NOT NULL DEFAULT '',
    detail          TEXT        NOT NULL DEFAULT '',
    CONSTRAINT flow_jobs_flow_check CHECK (flow ~ '^[a-z][a-z0-9]*(\.[a-z][a-z0-9_]*)+$'),
    CONSTRAINT flow_jobs_class_check CHECK (class IN ('read', 'write')),
    CONSTRAINT flow_jobs_host_check CHECK (host ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
    CONSTRAINT flow_jobs_instance_check CHECK (instance ~ '^[a-z0-9][a-z0-9_-]{0,31}$'),
    CONSTRAINT flow_jobs_status_check CHECK (status IN
        ('queued', 'running', 'done', 'failed', 'needs_operator', 'expired', 'cancelled')),
    CONSTRAINT flow_jobs_output_cap CHECK (octet_length(output) <= 65536),
    CONSTRAINT flow_jobs_detail_cap CHECK (octet_length(detail) <= 4096),
    CONSTRAINT flow_jobs_running_has_lease CHECK (status <> 'running' OR lease_until IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS idx_flow_jobs_owner_status ON public.flow_jobs (owner_id, status, id);
CREATE UNIQUE INDEX IF NOT EXISTS flow_jobs_one_running
    ON public.flow_jobs (owner_id, host, instance) WHERE status = 'running';

CREATE OR REPLACE FUNCTION public.flow_jobs_input_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.owner_id IS DISTINCT FROM OLD.owner_id
       OR NEW.flow IS DISTINCT FROM OLD.flow
       OR NEW.class IS DISTINCT FROM OLD.class
       OR NEW.host IS DISTINCT FROM OLD.host
       OR NEW.instance IS DISTINCT FROM OLD.instance
       OR NEW.params IS DISTINCT FROM OLD.params
       OR NEW.requested_via IS DISTINCT FROM OLD.requested_via
       OR NEW.requested_by IS DISTINCT FROM OLD.requested_by
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR NEW.expires_at IS DISTINCT FROM OLD.expires_at THEN
        RAISE EXCEPTION 'flow_jobs: a job''s input is immutable (id %)', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS flow_jobs_input_immutable ON public.flow_jobs;
CREATE TRIGGER flow_jobs_input_immutable BEFORE UPDATE ON public.flow_jobs
    FOR EACH ROW EXECUTE FUNCTION public.flow_jobs_input_immutable();

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set them to 1) restores the principal default.
ALTER TABLE public.flow_runner_tokens ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;
ALTER TABLE public.flow_jobs ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['flow_runner_tokens', 'flow_jobs'] LOOP
        EXECUTE format('ALTER TABLE public.%I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE public.%I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS owner_isolation ON public.%I', t);
        EXECUTE format($p$
            CREATE POLICY owner_isolation ON public.%I
                AS PERMISSIVE FOR ALL TO PUBLIC
                USING (
                    owner_id = nullif(current_setting('muster.uid', true), '')::bigint
                    OR current_setting('muster.scope', true) = 'all'
                )
                WITH CHECK (
                    owner_id = nullif(current_setting('muster.uid', true), '')::bigint
                    OR current_setting('muster.scope', true) = 'all'
                )
        $p$, t);
    END LOOP;
END
$$;

GRANT SELECT, INSERT, UPDATE, DELETE ON public.flow_runner_tokens TO muster_app;
GRANT USAGE, SELECT, UPDATE ON SEQUENCE public.flow_runner_tokens_id_seq TO muster_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON public.flow_jobs TO muster_app;
GRANT USAGE, SELECT, UPDATE ON SEQUENCE public.flow_jobs_id_seq TO muster_app;
