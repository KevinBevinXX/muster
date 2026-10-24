-- muster 0048 — A REPLY DRAFT'S DECISION REMEMBERS WHERE THE OWNER MADE IT
-- (plugin_draft_decide, internal/api/plugin_draft_tools.go).
--
-- Until now the owner decided a reply draft in exactly one place: its card on
-- the Asks page's Reply drafts tab, in a browser. Now the owner's own MIRRORED
-- Claude Code sessions can decide one too, over muster-mirror's loopback MCP
-- (plugin_draft_decide) — 0035's shape for connector requests. The row records
-- which door the decision came through:
--
--   decided_via          NULL      no decision recorded: pending, settled by time
--                                  (expired / superseded), auto-approved by a
--                                  session route's allowlist, and every row
--                                  decided before this migration (no backfill)
--                        'browser' the Asks page (an approval, a rejection, or
--                                  the withdrawal of an unclaimed approval)
--                        'mcp'     plugin_draft_decide, from a mirrored session
--   decided_via_host     that session's mirror host (the host's token proves it)
--   decided_via_session  the Claude session id the caller named (a claim)
--
-- 🔴 THE CHECK STATES THE RULES THE SERVER ENFORCES (plugins.DecideDraftFrom),
-- SO A BUG THERE CANNOT WRITE A ROW THAT BREAKS THEM: a host and session only
-- with 'mcp', and then both, in the shapes the mirror wire accepts
-- (mirror.ValidHost, mirror.ValidClaudeSession — the same patterns as 0035's),
-- so the audit line and any view render them as inert text; and no source on a
-- PENDING row (nothing moves a row back to pending, so this only refuses a
-- write that names a decision it did not make).
--
-- Row-level security is unchanged: the columns are on plugin_reply_drafts,
-- whose owner policy already covers every column of the row.
--
-- Must apply after 0047 (the contiguity test refuses a gap). Not re-applied by
-- db.RLSOn (it changes no policy, grant or default) and idempotent: ADD COLUMN
-- IF NOT EXISTS, DROP/ADD CONSTRAINT.
--
-- THE ROLLING-DEPLOY OVERLAP, AND ROLLBACK. A pre-0048 image never names the
-- columns: its decisions write NULL (allowed), and its withdrawal of an approval
-- a new image recorded keeps that approval's source on a now-'rejected' row —
-- which the CHECK allows (it constrains the shape, not the status beyond
-- pending), so nothing an old image runs is refused. Rolling the image back
-- therefore needs no SQL. To remove the schema itself afterwards:
--   ALTER TABLE public.plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_decided_via_check;
--   ALTER TABLE public.plugin_reply_drafts DROP COLUMN IF EXISTS decided_via_session,
--       DROP COLUMN IF EXISTS decided_via_host, DROP COLUMN IF EXISTS decided_via;
--   DELETE FROM schema_migrations WHERE version = 48;
-- That loses only where decisions were made; the decisions stay, and the
-- audit_log rows that name the source stay too.

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS decided_via         TEXT;
ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS decided_via_host    TEXT;
ALTER TABLE public.plugin_reply_drafts ADD COLUMN IF NOT EXISTS decided_via_session TEXT;

ALTER TABLE public.plugin_reply_drafts DROP CONSTRAINT IF EXISTS plugin_reply_drafts_decided_via_check;
ALTER TABLE public.plugin_reply_drafts
    ADD CONSTRAINT plugin_reply_drafts_decided_via_check CHECK (
        (decided_via IS NULL AND decided_via_host IS NULL AND decided_via_session IS NULL)
        -- 🔴 EVERY TERM IS NULL-SAFE (0035's rule): a CHECK passes on NULL, so
        -- each branch spells IS NOT NULL / IS NULL before it compares.
        OR (decided_via IS NOT NULL AND decided_via = 'browser' AND status <> 'pending'
            AND decided_via_host IS NULL AND decided_via_session IS NULL)
        OR (decided_via IS NOT NULL AND decided_via = 'mcp' AND status <> 'pending'
            AND decided_via_host IS NOT NULL AND decided_via_session IS NOT NULL
            AND decided_via_host ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'
            AND decided_via_session ~ '^[0-9A-Za-z][0-9A-Za-z_-]{0,63}$'));
