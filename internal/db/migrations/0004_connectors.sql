-- muster 0004 — the secure connector store.
--
-- A CONNECTOR is a named credential for one platform (an API token, a basic-auth
-- pair) plus the hosts it may be sent to. Agents never receive the secret: they
-- call muster's proxy (/connect/{connector}/{host}/...), and muster injects the
-- credential on the way out. See internal/connectors.
--
-- 🔴 THE SECRET IS STORED ONLY AS AES-256-GCM CIPHERTEXT (secret_ciphertext),
-- sealed with MUSTER_CONNECTOR_ENCRYPTION_KEY and bound to the connector's NAME
-- as additional data, so a ciphertext copied onto another row does not open.
--
-- 🔴 GRANTS ARE KEYED BY CONNECTOR *NAME*, NOT BY connectors.id, BECAUSE AN
-- AGENT MAY ASK FOR A CONNECTOR THAT DOES NOT EXIST YET — the operator's answer
-- to that request is the form that creates it. Deleting a connector deletes its
-- grants in the same transaction (internal/connectors.PGStore.DeleteConnector),
-- so a later connector re-created under the same name (other hosts, other
-- secret) inherits no approval. Agent deletion cascades through the FK.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS connectors (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name              TEXT        NOT NULL UNIQUE
        CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
    platform          TEXT        NOT NULL DEFAULT 'custom',
    hosts             TEXT[]      NOT NULL CHECK (cardinality(hosts) > 0),
    auth_scheme       TEXT        NOT NULL CHECK (auth_scheme IN ('bearer', 'header', 'basic')),
    auth_header       TEXT        NOT NULL DEFAULT '',
    auth_username     TEXT        NOT NULL DEFAULT '',
    secret_ciphertext BYTEA       NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    rotated_at        TIMESTAMPTZ,
    last_used_at      TIMESTAMPTZ,
    CONSTRAINT connectors_header_scheme CHECK ((auth_scheme = 'header') = (auth_header <> ''))
);

CREATE TABLE IF NOT EXISTS connector_grants (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    agent_id       BIGINT      NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    connector_name TEXT        NOT NULL,
    status         TEXT        NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'approved', 'denied', 'revoked')),
    reason         TEXT        NOT NULL DEFAULT '',
    requested_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at     TIMESTAMPTZ,
    decided_by     TEXT        NOT NULL DEFAULT '',
    last_used_at   TIMESTAMPTZ
);

-- At most ONE live (pending or approved) grant per agent and connector: a second
-- request while one is pending returns the pending one rather than prompting the
-- operator twice. Denied and revoked rows are history and may repeat.
CREATE UNIQUE INDEX IF NOT EXISTS connector_grants_one_live
    ON connector_grants (agent_id, connector_name)
    WHERE status IN ('pending', 'approved');

CREATE INDEX IF NOT EXISTS connector_grants_pending
    ON connector_grants (requested_at) WHERE status = 'pending';

-- The audit trail: one row per proxied request and per grant decision. Method,
-- host, path and status only — never a body, never a header.
CREATE TABLE IF NOT EXISTS connector_audit (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    kind           TEXT        NOT NULL CHECK (kind IN ('proxy', 'grant')),
    agent_id       BIGINT,
    agent_name     TEXT        NOT NULL DEFAULT '',
    connector_name TEXT        NOT NULL,
    method         TEXT        NOT NULL DEFAULT '',
    host           TEXT        NOT NULL DEFAULT '',
    path           TEXT        NOT NULL DEFAULT '',
    status         INT         NOT NULL DEFAULT 0,
    detail         TEXT        NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS connector_audit_at ON connector_audit (at);
