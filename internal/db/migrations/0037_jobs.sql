-- muster 0037 — JOBS (internal/jobs): one-shot machine jobs an external
-- orchestrator submits over POST /api/jobs ("run this prompt on this agent,
-- optionally at this commit, and give me the result"), and reads back over
-- GET /api/jobs/{id}.
--
-- A row is the whole record of one job: what was asked (agent, prompt, repo,
-- sha, result_path, timeout_s), who asked (requested_by, the service caller's
-- asserted X-Muster-Actor), and how it ended (status, result, reason, error,
-- cc_account, the chat session the turn ran in, the timestamps).
--
-- 🔴 OWNED AND UNDER FORCED ROW-LEVEL SECURITY, IN 0017's SHAPE. The agent a
-- row names is pinned to the row's owner by 0015's
-- plugin_rows_owner_is_agents_owner trigger, run with 'nullable': agent_id is
-- SET NULL when the agent is deleted, and the job's record (with agent_name)
-- outlives it.
--
-- 🔴 A STATUS MOVES ONLY FORWARD, BY CONDITIONAL UPDATE. queued → running →
-- one of succeeded / failed / timed_out, or queued → failed (a job that never
-- started). The store's UPDATEs are conditional on the current status, so a
-- late finish cannot overwrite a terminal state written by someone else (the
-- reaper that fails abandoned jobs, below). finished_at is set exactly on a
-- terminal row; started_at on every row that ran.
--
-- 🔴 AN ABANDONED JOB IS FAILED, NEVER RE-RUN — AND ONLY ONCE IT IS PAST ITS
-- OWN DEADLINE. The turn runs in the server process; a process that dies
-- mid-job leaves its row queued or running. The reaper (jobs.Store.ReapOverdue,
-- at boot and periodically) moves such a row to failed with reason
-- 'interrupted' only once it is past started_at + timeout_s + a grace period
-- (running) or created_at + the longest queue wait (queued), so a job another
-- process is still running during a rolling update is never touched. The agent
-- may already have done the work, so re-running it is the caller's decision,
-- not the server's.
--
-- 🔴 idempotency_key IS UNIQUE PER OWNER. A second POST with the same key
-- answers the first job instead of creating another.
--
-- 🔴 result IS STORED VERBATIM. It is the agent's final reply, or the bytes of
-- result_path read from the agent's workspace; it is capped at 1 MiB here and
-- by the reader (jobs.MaxResultBytes), and a file that is not valid UTF-8 text
-- is refused by the reader rather than altered.
--
-- 🔴 IT IS RE-APPLIED BY db.RLSOn (rollback.go) WHEN THE TABLE EXISTS, SO EVERY
-- STATEMENT IS IDEMPOTENT, and nothing here redefines an object 0015 defines.
--
-- Rollback: an image from before 0037 never reads the table. To remove it:
-- DROP TABLE public.jobs; DELETE FROM schema_migrations WHERE version = 37.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.jobs (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id        BIGINT      NOT NULL
                    DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                    REFERENCES public.users (id),
    agent_id        BIGINT      REFERENCES public.agents (id) ON DELETE SET NULL,
    agent_name      TEXT        NOT NULL,
    prompt          TEXT        NOT NULL,
    repo            TEXT        NOT NULL DEFAULT '',
    sha             TEXT        NOT NULL DEFAULT '',
    result_path     TEXT        NOT NULL DEFAULT '',
    timeout_s       INTEGER     NOT NULL,
    idempotency_key TEXT,
    requested_by    TEXT        NOT NULL DEFAULT '',
    status          TEXT        NOT NULL DEFAULT 'queued',
    result          TEXT,
    -- A stable machine token for why a job did not succeed (rate_limited,
    -- auth_failed, timeout, interrupted, …); '' on success.
    reason          TEXT        NOT NULL DEFAULT '',
    error           TEXT        NOT NULL DEFAULT '',
    cc_account      TEXT        NOT NULL DEFAULT '',
    -- The chat session the turn ran in. No foreign key: a display link, like
    -- 0026's answer_queue_id.
    session_id      BIGINT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at      TIMESTAMPTZ,
    finished_at     TIMESTAMPTZ,
    CONSTRAINT jobs_status_check CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'timed_out')),
    CONSTRAINT jobs_agent_name_cap CHECK (char_length(agent_name) BETWEEN 1 AND 128),
    -- jobs.MaxPromptBytes.
    CONSTRAINT jobs_prompt_cap CHECK (octet_length(prompt) BETWEEN 1 AND 65536),
    CONSTRAINT jobs_repo_cap CHECK (octet_length(repo) <= 256),
    -- jobs.ValidSHA: a full object id; `git fetch origin <sha>` fetches no abbreviation.
    CONSTRAINT jobs_sha_check CHECK (sha = '' OR sha ~ '^[0-9a-f]{40}$'),
    CONSTRAINT jobs_result_path_cap CHECK (octet_length(result_path) <= 512),
    -- jobs.MaxTimeoutSeconds.
    CONSTRAINT jobs_timeout_check CHECK (timeout_s BETWEEN 1 AND 2100),
    CONSTRAINT jobs_idempotency_key_cap CHECK (idempotency_key IS NULL OR octet_length(idempotency_key) BETWEEN 1 AND 128),
    CONSTRAINT jobs_requested_by_cap CHECK (octet_length(requested_by) <= 128),
    -- jobs.MaxResultBytes.
    CONSTRAINT jobs_result_cap CHECK (result IS NULL OR octet_length(result) <= 1048576),
    CONSTRAINT jobs_reason_cap CHECK (octet_length(reason) <= 64),
    CONSTRAINT jobs_error_cap CHECK (octet_length(error) <= 4096),
    CONSTRAINT jobs_cc_account_cap CHECK (octet_length(cc_account) <= 128),
    CONSTRAINT jobs_result_only_on_success CHECK (status = 'succeeded' OR result IS NULL),
    CONSTRAINT jobs_succeeded_has_result CHECK (status <> 'succeeded' OR result IS NOT NULL),
    CONSTRAINT jobs_reason_iff_unsuccessful CHECK ((status IN ('failed', 'timed_out')) = (reason <> '')),
    CONSTRAINT jobs_started_check CHECK (status IN ('queued', 'failed') OR started_at IS NOT NULL),
    CONSTRAINT jobs_finished_check CHECK ((status IN ('queued', 'running')) = (finished_at IS NULL))
);

-- One job per (owner, idempotency key).
CREATE UNIQUE INDEX IF NOT EXISTS jobs_owner_idempotency_key
    ON public.jobs (owner_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
-- The reaper's scan: every unfinished row.
CREATE INDEX IF NOT EXISTS idx_jobs_unfinished ON public.jobs (id) WHERE status IN ('queued', 'running');
-- The SET NULL from agents looks rows up by agent.
CREATE INDEX IF NOT EXISTS idx_jobs_agent ON public.jobs (agent_id) WHERE agent_id IS NOT NULL;

DROP TRIGGER IF EXISTS jobs_owner_is_agents_owner ON public.jobs;
CREATE TRIGGER jobs_owner_is_agents_owner BEFORE INSERT OR UPDATE OF owner_id, agent_id
    ON public.jobs FOR EACH ROW EXECUTE FUNCTION public.plugin_rows_owner_is_agents_owner('nullable');

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.jobs ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.jobs FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.jobs;
CREATE POLICY owner_isolation ON public.jobs
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.jobs TO muster_app;
GRANT USAGE, SELECT ON SEQUENCE public.jobs_id_seq TO muster_app;
