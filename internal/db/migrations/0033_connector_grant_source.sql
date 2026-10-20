-- muster 0033 — A CONNECTOR GRANT REMEMBERS WHERE ITS CREDENTIAL CAME FROM.
--
-- A `github` grant can now be served by two different credentials: a stored
-- connector row (a personal access token: whatever repositories and scopes the
-- token has) or the GitHub App (internal/githubapp: a 1-hour token for the
-- agent's one repository). An approval is a decision about ONE of them.
--
-- 🔴 WITHOUT THIS COLUMN, REMOVING THE APP WOULD SILENTLY WIDEN EVERY GRANT
-- APPROVED UNDER IT: the hidden stored `github` row reappears with its full
-- scope and the grant, still `approved`, starts serving it. `source` records
-- which credential the owner approved ('stored' | 'app'); a grant whose source
-- no longer matches what the connector is backed by is put back to `pending`
-- (connectors.ReconcileGrant) and needs approving again.
--
-- Existing rows were all approved against stored connectors: 'stored'.
--
-- Must apply after 0032 (the contiguity test refuses a gap). Not re-applied by
-- db.RLSOn (no policy, grant or default change) and idempotent.

SET LOCAL lock_timeout = '3s';

ALTER TABLE connector_grants
    ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'stored';
