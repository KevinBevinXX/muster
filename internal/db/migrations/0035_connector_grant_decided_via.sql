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
-- 🔴 THE ROLLING-DEPLOY OVERLAP. While a deploy of this migration rolls, a pod
-- still running a pre-0035 image can revoke, or re-open (ReconcileGrant: the
-- App configured over a stored token, or removed), a grant a NEW pod has just
-- decided over MCP. The old image's statement does not clear the provenance,
-- so the CHECK refuses it: that revocation answers 500 and the agent's
-- connector read fails, until the pod is replaced (then retrying works). Only
-- grants decided over MCP during the overlap are affected, and no row is
-- written wrongly. Measured at the SQL level only — the pre-0035 statements
-- run against this schema (TestMigration0035RollbackAndRestore) — not on a
-- live rolling deploy.
--
-- ROLLBACK. The columns are nullable and named by no older statement, but a
-- pre-0035 image's RevokeGrant / ReopenGrant / DecideGrant never touch them, so
-- the CHECK must go before the image is rolled back (else revoking or
-- re-opening an MCP-decided grant fails, as above), and the columns must be
-- emptied with it (else a grant the old image re-opens and then approves on the
-- tab keeps the stale 'mcp' host/session, and after a roll-forward the tab
-- shows "via MCP from …" for a decision made on the tab). In this order:
--
-- 1. BEFORE rolling the image back (the CHECK is dropped and every provenance
--    emptied; the audit lines keep where MCP decisions came from):
-- [rollback-sql]
--   ALTER TABLE public.connector_grants DROP CONSTRAINT IF EXISTS connector_grants_decided_via_check;
--   UPDATE public.connector_grants SET decided_via = NULL, decided_via_host = NULL, decided_via_session = NULL
--       WHERE decided_via IS NOT NULL OR decided_via_host IS NOT NULL OR decided_via_session IS NOT NULL;
-- [/rollback-sql]
-- 2. Roll the image back. schema_migrations keeps row 35, so a roll-forward
--    does NOT re-apply this file: the CHECK comes back only through step 3.
--    Until the last new pod is gone it may still record an MCP decision; step 3
--    empties those too (they are rows the rolled-back image may since have
--    re-decided, so they cannot be trusted).
-- 3. AT roll-forward, BEFORE the new image takes traffic (an old image never
--    writes the columns, so this loses nothing it recorded):
-- [restore-sql]
--   UPDATE public.connector_grants SET decided_via = NULL, decided_via_host = NULL, decided_via_session = NULL
--       WHERE decided_via IS NOT NULL OR decided_via_host IS NOT NULL OR decided_via_session IS NOT NULL;
--   ALTER TABLE public.connector_grants DROP CONSTRAINT IF EXISTS connector_grants_decided_via_check;
--   ALTER TABLE public.connector_grants ADD CONSTRAINT connector_grants_decided_via_check CHECK (
--       (decided_via IS NULL AND decided_via_host IS NULL AND decided_via_session IS NULL)
--       OR (decided_via IS NOT NULL AND decided_via = 'mcp'
--           AND status IN ('approved', 'denied')
--           AND decided_via_host IS NOT NULL AND decided_via_session IS NOT NULL
--           AND decided_via_host ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'
--           AND decided_via_session ~ '^[0-9A-Za-z][0-9A-Za-z_-]{0,63}$'));
-- [/restore-sql]
--    (The CHECK is the one below, verbatim; TestMigration0035RollbackAndRestore
--    runs both blocks above as written and compares the restored CHECK with
--    the migrated one.)
--
-- To remove the schema itself instead (after step 1 and the image rollback):
--   ALTER TABLE public.connector_grants DROP COLUMN IF EXISTS decided_via_session,
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
