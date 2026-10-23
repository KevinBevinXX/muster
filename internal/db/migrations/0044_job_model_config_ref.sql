-- muster 0044 — a job's MODEL and CONFIG REF (the machine job API,
-- internal/api jobs.go).
--
-- jobs.model is the Claude model the job's turn runs on ('' = the CLI's
-- default, as before this migration). The job's own Claude Code session is
-- opened with it, and ccd starts that session's CLI with --model; the agent's
-- other sessions are not touched. Its CHECK is internal/agents
-- claudeModelPattern, the one a Claude Code chat session's model is held to.
--
-- jobs.config_ref is the commit a sha job's worktree takes its agent
-- configuration from ('' = the default branch, origin/HEAD, as before). The
-- worktree refuses one that is not reachable from the default branch
-- (agentprovision jobOverlayScript). A full object id, and only on a sha job:
-- a job without a sha has no worktree and no overlay.
--
-- Rollback: an image from before 0044 never reads these columns. To remove
-- them: ALTER TABLE public.jobs DROP COLUMN model, DROP COLUMN config_ref;
-- DELETE FROM schema_migrations WHERE version = 44.

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.jobs ADD COLUMN IF NOT EXISTS model TEXT NOT NULL DEFAULT '';
ALTER TABLE public.jobs DROP CONSTRAINT IF EXISTS jobs_model_check;
ALTER TABLE public.jobs ADD CONSTRAINT jobs_model_check
    CHECK (model = '' OR model ~ '^[a-z0-9][a-z0-9.-]{0,79}(\[1m\])?$');

ALTER TABLE public.jobs ADD COLUMN IF NOT EXISTS config_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE public.jobs DROP CONSTRAINT IF EXISTS jobs_config_ref_check;
ALTER TABLE public.jobs ADD CONSTRAINT jobs_config_ref_check
    CHECK (config_ref = '' OR (sha <> '' AND config_ref ~ '^[0-9a-f]{40}$'));
