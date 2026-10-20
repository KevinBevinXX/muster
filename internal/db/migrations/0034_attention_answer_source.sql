-- muster 0034 — AN ANSWER REMEMBERS WHERE THE OPERATOR GAVE IT
-- (attention_answer, internal/api/attention_tools.go; docs/attention.md).
--
-- Until now the operator answered an ask in exactly one place: the /attention
-- view in a browser. Now the operator's own MIRRORED Claude Code sessions can
-- answer one too, over muster-mirror's loopback MCP (attention_answer). Both are
-- still the operator — answered_by stays 'operator', and 0025's CHECK on it is
-- unchanged — but the asker (the chief, above all) must be able to tell the two
-- apart, so the answer records which door it came through:
--
--   answered_via          NULL   the /attention view (or, for answered_in_session,
--                                the operator's message to the asker) — and every
--                                row answered before this migration: no backfill
--                         'mcp'  attention_answer, from a mirrored session
--   answered_via_host     that session's mirror host (the host's token names it)
--   answered_via_session  that session's Claude session id
--
-- 🔴 THE CHECK STATES THE RULES THE SERVER ENFORCES, SO A BUG THERE CANNOT
-- WRITE A ROW THAT BREAKS THEM: provenance is all-or-nothing; only an
-- 'answered' ask carries it; the host and session have the shapes the mirror
-- wire accepts (mirror.ValidHost, mirror.ValidClaudeSession — the delivered
-- message names them outside its fence, so they must be inert text); and no
-- MCP answer is given to an ask a mirrored session on the SAME HOST raised
-- (mirror_host = answered_via_host). The HOST, not the session, because the
-- host is what the caller's token proves; the session is the pane the caller
-- names, which any session of that host's user can name for a sibling.
--
-- Row-level security is unchanged: the columns are on attention_asks, whose
-- owner_isolation policy (0025) already covers every column of the row.
--
-- Must apply after 0033 (the contiguity test refuses a gap). Not re-applied by
-- db.RLSOn (it changes no policy, grant or default) and idempotent: ADD COLUMN
-- IF NOT EXISTS, DROP/ADD CONSTRAINT.
--
-- ROLLBACK. The columns are nullable and named by no older statement, so an
-- image from before 0034 runs unchanged over this schema (it never writes them;
-- an answer it records leaves them NULL, which the CHECK allows). To remove the
-- schema itself, after rolling the image back:
--   ALTER TABLE attention_asks DROP CONSTRAINT IF EXISTS attention_asks_answered_via_check;
--   ALTER TABLE attention_asks DROP COLUMN IF EXISTS answered_via_session,
--       DROP COLUMN IF EXISTS answered_via_host, DROP COLUMN IF EXISTS answered_via;
--   DELETE FROM schema_migrations WHERE version = 34;
-- That loses only the provenance of MCP answers; the answers themselves stay.

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS answered_via         TEXT;
ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS answered_via_host    TEXT;
ALTER TABLE public.attention_asks ADD COLUMN IF NOT EXISTS answered_via_session TEXT;

ALTER TABLE public.attention_asks DROP CONSTRAINT IF EXISTS attention_asks_answered_via_check;
ALTER TABLE public.attention_asks
    ADD CONSTRAINT attention_asks_answered_via_check CHECK (
        (answered_via IS NULL AND answered_via_host IS NULL AND answered_via_session IS NULL)
        -- 🔴 EVERY TERM IS NULL-SAFE: a CHECK passes on NULL, so a bare
        -- `answered_via = 'mcp'` or a regex over a NULL host would admit a
        -- half-written row. The IS NOT NULLs make each term TRUE or FALSE.
        OR (answered_via IS NOT NULL AND answered_via = 'mcp'
            AND status = 'answered'
            AND answered_via_host IS NOT NULL AND answered_via_session IS NOT NULL
            AND answered_via_host ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'
            AND answered_via_session ~ '^[0-9A-Za-z][0-9A-Za-z_-]{0,63}$'
            -- Never an ask raised on the answering host (any of its sessions).
            AND mirror_host IS DISTINCT FROM answered_via_host));
