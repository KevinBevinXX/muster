-- muster 0041 — WORKER POOLS for the machine job API (internal/api jobs.go).
--
-- A worker pool is a NAME carried by long-lived claude-code agents: an agent
-- created with worker_pool = 'reviewers' is a member of the pool 'reviewers'.
-- A job submitted with {"worker_pool": "reviewers"} instead of an agent is
-- given one of the members (internal/api pickJobAgent). There is no pool
-- table: a pool exists while some agent carries its name.
--
-- 🔴 ONLY A claude-code AGENT IS EVER A MEMBER. Jobs run on that kind only
-- (their result, their typed failures and their workspace are ccd's), so
-- agents_worker_pool_check refuses a pool on any other kind — the same shape
-- as 0003's agents_cc_account_matches_kind.
--
-- jobs.worker_pool records the pool a job was submitted to ('' for a job that
-- named its agent); the agent the pool chose is agent_id / agent_name, as for
-- every job.
--
-- Both names are internal/agents.ValidWorkerPool's expression.
--
-- jobs.pr is the pull request a sha job's commit is on (0 for a job without a
-- sha): the job fetches refs/pull/<pr>/head anonymously and runs only at that
-- head or an ancestor of it. The API requires it with a sha; the CHECK keeps
-- it a positive number when set, and does not pair it with sha so that a sha
-- job recorded before this migration (pr 0) stays valid.
--
-- Rollback: an image from before 0041 never reads these columns. To remove
-- them: ALTER TABLE public.agents DROP COLUMN worker_pool; ALTER TABLE
-- public.jobs DROP COLUMN worker_pool, DROP COLUMN pr; DELETE FROM schema_migrations WHERE
-- version = 41.

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.agents ADD COLUMN IF NOT EXISTS worker_pool TEXT NOT NULL DEFAULT '';
ALTER TABLE public.agents DROP CONSTRAINT IF EXISTS agents_worker_pool_check;
ALTER TABLE public.agents ADD CONSTRAINT agents_worker_pool_check
    CHECK (worker_pool = '' OR (kind = 'claude-code' AND worker_pool ~ '^[a-z0-9][a-z0-9-]{0,62}$'));
CREATE INDEX IF NOT EXISTS idx_agents_worker_pool ON public.agents (worker_pool) WHERE worker_pool <> '';

ALTER TABLE public.jobs ADD COLUMN IF NOT EXISTS worker_pool TEXT NOT NULL DEFAULT '';
ALTER TABLE public.jobs DROP CONSTRAINT IF EXISTS jobs_worker_pool_check;
ALTER TABLE public.jobs ADD CONSTRAINT jobs_worker_pool_check
    CHECK (worker_pool = '' OR worker_pool ~ '^[a-z0-9][a-z0-9-]{0,62}$');

ALTER TABLE public.jobs ADD COLUMN IF NOT EXISTS pr INTEGER NOT NULL DEFAULT 0;
ALTER TABLE public.jobs DROP CONSTRAINT IF EXISTS jobs_pr_check;
ALTER TABLE public.jobs ADD CONSTRAINT jobs_pr_check CHECK (pr >= 0);
