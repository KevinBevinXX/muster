-- muster 0035 — A CONNECTOR DECISION REMEMBERS WHERE THE OPERATOR MADE IT
-- (connector_request_decide, internal/api/connector_decide_tools.go;
-- docs/connectors.md).
--
-- Until now the operator decided a connector request in exactly one place: the
-- request card on the Connectors tab, in a browser. Now the operator's own
-- MIRRORED Claude Code sessions can decide one too, over muster-mirror's
-- loopback MCP (connector_request_decide) — the same flow as attention_answer
-- (0034). Both are still the operator — decided_by stays 'operator' — but the
-- Connectors tab and the audit trail must tell the two apart, so the row
-- records which door it came through:
--
--   decided_via          NULL   the Connectors tab — and every row decided before
--                               this migration (no backfill), every pending row,
--                               and every revoked one (a revocation is made on
--                               the tab and replaces the decision)
--                        'mcp'  connector_request_decide, from a mirrored session
--   decided_via_host     that session's mirror host (the host's token proves it)
--   decided_via_session  the Claude session id the caller named (a claim)
--
-- 🔴 THE CHECK STATES THE RULES THE SERVER ENFORCES, SO A BUG THERE CANNOT
-- WRITE A ROW THAT BREAKS THEM: provenance is all-or-nothing; only a DECIDED
-- grant ('approved' or 'denied') carries it — a re-opened (pending) or revoked
-- grant must have it cleared in the same statement (connectors.PGStore's
-- ReopenGrant / RevokeGrant do); and the host and session have the shapes the
-- mirror wire accepts (mirror.ValidHost, mirror.ValidClaudeSession), so the
-- Connectors tab and the audit line render them as inert text.
--
-- Row-level security is unchanged: the columns are on connector_grants, whose
-- owner_isolation policy (0007) already covers every column of the row.
--
-- Must apply after 0034 (the contiguity test refuses a gap). Not re-applied by
-- db.RLSOn (it changes no policy, grant or default) and idempotent: ADD COLUMN
-- IF NOT EXISTS, DROP/ADD CONSTRAINT.
--
-- ROLLBACK. The columns are nullable and named by no older statement, so an
-- image from before 0035 runs unchanged over this schema — with ONE caveat: an
-- older image's RevokeGrant and ReopenGrant do not clear the columns, so
-- revoking (or the App/stored source switch re-opening) a grant that was
-- decided over MCP would be refused by the CHECK. Drop the CHECK before rolling
-- the image back (that alone makes an old image fully safe; the columns then
-- only go stale). To remove the schema itself, after rolling the image back:
--   ALTER TABLE connector_grants DROP CONSTRAINT IF EXISTS connector_grants_decided_via_check;
--   ALTER TABLE connector_grants DROP COLUMN IF EXISTS decided_via_session,
--       DROP COLUMN IF EXISTS decided_via_host, DROP COLUMN IF EXISTS decided_via;
--   DELETE FROM schema_migrations WHERE version = 35;
-- That loses only where MCP decisions were made; the decisions stay, and the
-- connector_audit lines that name the host and session stay too.

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.connector_grants ADD COLUMN IF NOT EXISTS decided_via         TEXT;
ALTER TABLE public.connector_grants ADD COLUMN IF NOT EXISTS decided_via_host    TEXT;
ALTER TABLE public.connector_grants ADD COLUMN IF NOT EXISTS decided_via_session TEXT;

ALTER TABLE public.connector_grants DROP CONSTRAINT IF EXISTS connector_grants_decided_via_check;
ALTER TABLE public.connector_grants
    ADD CONSTRAINT connector_grants_decided_via_check CHECK (
        (decided_via IS NULL AND decided_via_host IS NULL AND decided_via_session IS NULL)
        -- 🔴 EVERY TERM IS NULL-SAFE: a CHECK passes on NULL, so a bare
        -- `decided_via = 'mcp'` or a regex over a NULL host would admit a
        -- half-written row. The IS NOT NULLs make each term TRUE or FALSE.
        OR (decided_via IS NOT NULL AND decided_via = 'mcp'
            AND status IN ('approved', 'denied')
            AND decided_via_host IS NOT NULL AND decided_via_session IS NOT NULL
            AND decided_via_host ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'
            AND decided_via_session ~ '^[0-9A-Za-z][0-9A-Za-z_-]{0,63}$'));
