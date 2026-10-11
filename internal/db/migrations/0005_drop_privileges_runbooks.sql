-- muster 0005 — drop the privilege and runbook schema.
--
-- Both subsystems were removed from the code: privileges were superseded by
-- connectors (0004), runbooks were unused. This drops what 0001 created for them:
--
--   privilege_profiles, agent_privileges, privilege_requests   (PRIVILEGE)
--   runbooks, runbook_runs                                     (RUNBOOKS)
--   notes.grant_profiles   (the per-task "profiles to grant on dispatch" list)
--
-- 🔴 IT IS IRREVERSIBLE, AND THE DEPLOY TAKES A pg_dump FIRST. Nothing here keeps
-- a copy: a rollback to an image that still reads these tables needs the dump
-- restored. The binary that applies this migration no longer references any of
-- them, so the drop is safe for the code that runs it — and an OLDER image that
-- is still serving during a rollout will error on task reads (notes.grant_profiles
-- is in its column list) until it is replaced. See the PR for the deploy order.
--
-- ⚠ ORDER: the referencing tables go first. agent_privileges references
-- privilege_profiles and runbook_runs references runbooks; dropping the referenced
-- table first would need CASCADE, and CASCADE is deliberately not used so a
-- dependency nobody listed here (a view, a foreign key added by hand) fails the
-- migration loudly instead of being dropped with it.
--
-- Idempotent (IF EXISTS), like the rest of the set, and 0001 is NOT edited: a
-- fresh database still creates these tables in 0001 and drops them here.

SET LOCAL lock_timeout = '3s';

DROP TABLE IF EXISTS public.agent_privileges;
DROP TABLE IF EXISTS public.privilege_requests;
DROP TABLE IF EXISTS public.privilege_profiles;

DROP TABLE IF EXISTS public.runbook_runs;
DROP TABLE IF EXISTS public.runbooks;

ALTER TABLE public.notes DROP COLUMN IF EXISTS grant_profiles;
