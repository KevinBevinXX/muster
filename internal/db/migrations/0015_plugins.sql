-- muster 0015 — PLUGINS: out-of-process connectors an owner mints a token for
-- (internal/plugins). A plugin POSTs inbound messages to muster (plugin_messages);
-- muster hands each to its owner's chief agent as a fenced turn. (Reply drafts
-- arrive in 0016.) muster NEVER calls a plugin: the plugin long-polls muster.
--
-- 🔴 BOTH TABLES ARE OWNED AND UNDER FORCED ROW-LEVEL SECURITY, IN 0012's
-- SHAPE. A plugin token is found by its hash under authz.System and then acts AS
-- ITS OWNER (authz.ViaPlugin), so owner_isolation is what keeps one owner's
-- plugin from ever seeing another owner's messages.
--
-- 🔴 A CHILD'S OWNER IS ITS PARENT'S OWNER, BY COMPOSITE FOREIGN KEY. A message
-- references (plugin_id, owner_id), so a row cannot claim a parent another owner
-- holds. The UNIQUE (id, owner_id) keys
-- those references need are declared here, on tables this migration owns — not
-- on agents, whose key 0006's re-runnable body drops and re-creates (see 0014).
-- The agent a row names is pinned by plugin_rows_owner_is_agents_owner, 0014's
-- trigger shape, for the same reason.
--
-- 🔴 WHAT ARRIVED IS IMMUTABLE. plugin_messages_input_immutable refuses an
-- UPDATE of who sent what (flow_jobs_input_immutable's shape).
--
-- Only the sha256 of a plugin token is stored; the token is shown once.

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS public.plugins (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id         BIGINT      NOT NULL
                     DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                     REFERENCES public.users (id),
    name             TEXT        NOT NULL,
    label            TEXT        NOT NULL,
    token_hash       TEXT        NOT NULL UNIQUE,
    -- The cairn scope anything the chief records durably from this plugin's
    -- content is told to go to (advisory: the preface names it).
    data_scope       TEXT        NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at     TIMESTAMPTZ,
    revoked_at       TIMESTAMPTZ,
    -- Delivery bookkeeping: the chief chat session this plugin's messages are
    -- delivered into, and which chief it belongs to (a re-provisioned chief is
    -- a new row, so a mismatch means "open a new session"). No foreign key: a
    -- deleted session or agent is simply not reused.
    chief_agent_id   BIGINT,
    chief_session_id BIGINT,
    CONSTRAINT plugins_id_owner_key UNIQUE (id, owner_id),
    CONSTRAINT plugins_name_check CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$'),
    CONSTRAINT plugins_label_check CHECK (char_length(label) BETWEEN 1 AND 80),
    CONSTRAINT plugins_data_scope_check CHECK (data_scope ~ '^[a-z0-9]([a-z0-9._-]{0,62}[a-z0-9])?$')
);

-- One LIVE plugin per (owner, name); a revoked one frees the name.
CREATE UNIQUE INDEX IF NOT EXISTS plugins_owner_name_live
    ON public.plugins (owner_id, name) WHERE revoked_at IS NULL;

CREATE TABLE IF NOT EXISTS public.plugin_messages (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id        BIGINT      NOT NULL
                    DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                    REFERENCES public.users (id),
    plugin_id       BIGINT      NOT NULL,
    external_id     TEXT        NOT NULL,
    sender          TEXT        NOT NULL DEFAULT '',
    subject         TEXT        NOT NULL DEFAULT '',
    body            TEXT        NOT NULL,
    received_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- queued: not yet handed to the chief (a provably-unsent attempt returns it
    -- here); delivering: the chief named by agent_id is on the turn that hands
    -- it over, so it may already cite it (a gated task, a reply draft);
    -- delivered: the turn completed; unconfirmed: the send failed after the text
    -- may have been pasted — NEVER resent. A `delivering` row left by a process
    -- that died mid-turn is settled `unconfirmed` by the next delivery to the
    -- same chief (internal/plugins Deliverer), never sent again.
    status          TEXT        NOT NULL DEFAULT 'queued',
    attempts        INTEGER     NOT NULL DEFAULT 0,
    last_attempt_at TIMESTAMPTZ,
    delivered_at    TIMESTAMPTZ,
    -- The chief the message was delivered to. SET NULL: deleting the agent must
    -- not delete what was received.
    agent_id        BIGINT      REFERENCES public.agents (id) ON DELETE SET NULL,
    CONSTRAINT plugin_messages_id_owner_key UNIQUE (id, owner_id),
    CONSTRAINT plugin_messages_external_unique UNIQUE (plugin_id, external_id),
    CONSTRAINT plugin_messages_plugin_owner_fkey FOREIGN KEY (plugin_id, owner_id)
        REFERENCES public.plugins (id, owner_id) ON DELETE CASCADE,
    CONSTRAINT plugin_messages_status_check CHECK (status IN ('queued', 'delivering', 'delivered', 'unconfirmed')),
    CONSTRAINT plugin_messages_settled_has_time CHECK ((status IN ('queued', 'delivering')) = (delivered_at IS NULL)),
    -- A queued message names no chief: a delivery that provably sent nothing
    -- clears the agent it was being delivered to, so nothing can cite it.
    CONSTRAINT plugin_messages_queued_has_no_agent CHECK (status <> 'queued' OR agent_id IS NULL),
    CONSTRAINT plugin_messages_external_id_check CHECK (char_length(external_id) BETWEEN 1 AND 200),
    CONSTRAINT plugin_messages_sender_cap CHECK (char_length(sender) <= 200),
    CONSTRAINT plugin_messages_subject_cap CHECK (char_length(subject) <= 200),
    CONSTRAINT plugin_messages_body_cap CHECK (octet_length(body) BETWEEN 1 AND 16384)
);

CREATE INDEX IF NOT EXISTS idx_plugin_messages_plugin_status ON public.plugin_messages (plugin_id, status, id);
CREATE INDEX IF NOT EXISTS idx_plugin_messages_plugin_created ON public.plugin_messages (plugin_id, created_at);

-- 🔴 0014's TRIGGER, PARAMETERISED. The row's owner IS the named agent's owner,
-- read as the writing principal (an agent it cannot see reads as no owner and is
-- refused). TG_ARGV[0] = 'nullable' admits a NULL agent_id on any write (a
-- message not yet delivered); otherwise (0016's drafts) a NEW row must name its
-- agent, and only the foreign key's own SET NULL may clear it.
CREATE OR REPLACE FUNCTION public.plugin_rows_owner_is_agents_owner() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.agent_id IS NULL THEN
        IF TG_ARGV[0] = 'nullable' THEN
            RETURN NEW;
        END IF;
        IF TG_OP = 'UPDATE' AND NEW.owner_id = OLD.owner_id THEN
            RETURN NEW;
        END IF;
    END IF;
    IF NEW.owner_id IS DISTINCT FROM (SELECT a.owner_id FROM public.agents a WHERE a.id = NEW.agent_id) THEN
        RAISE EXCEPTION '%: owner % is not the owner of agent %', TG_TABLE_NAME, NEW.owner_id, NEW.agent_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS plugin_messages_owner_is_agents_owner ON public.plugin_messages;
CREATE TRIGGER plugin_messages_owner_is_agents_owner BEFORE INSERT OR UPDATE OF owner_id, agent_id
    ON public.plugin_messages FOR EACH ROW EXECUTE FUNCTION public.plugin_rows_owner_is_agents_owner('nullable');

CREATE OR REPLACE FUNCTION public.plugin_messages_input_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.owner_id IS DISTINCT FROM OLD.owner_id
       OR NEW.plugin_id IS DISTINCT FROM OLD.plugin_id
       OR NEW.external_id IS DISTINCT FROM OLD.external_id
       OR NEW.sender IS DISTINCT FROM OLD.sender
       OR NEW.subject IS DISTINCT FROM OLD.subject
       OR NEW.body IS DISTINCT FROM OLD.body
       OR NEW.received_at IS DISTINCT FROM OLD.received_at
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'plugin_messages: a message''s input is immutable (id %)', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS plugin_messages_input_immutable ON public.plugin_messages;
CREATE TRIGGER plugin_messages_input_immutable BEFORE UPDATE ON public.plugin_messages
    FOR EACH ROW EXECUTE FUNCTION public.plugin_messages_input_immutable();

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set them to 1) restores the principal default.
ALTER TABLE public.plugins ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;
ALTER TABLE public.plugin_messages ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['plugins', 'plugin_messages'] LOOP
        EXECUTE format('ALTER TABLE public.%I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE public.%I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS owner_isolation ON public.%I', t);
        EXECUTE format($p$
            CREATE POLICY owner_isolation ON public.%I
                AS PERMISSIVE FOR ALL TO PUBLIC
                USING (
                    owner_id = nullif(current_setting('muster.uid', true), '')::bigint
                    OR current_setting('muster.scope', true) = 'all'
                )
                WITH CHECK (
                    owner_id = nullif(current_setting('muster.uid', true), '')::bigint
                    OR current_setting('muster.scope', true) = 'all'
                )
        $p$, t);
    END LOOP;
END
$$;

GRANT SELECT, INSERT, UPDATE, DELETE ON public.plugins TO muster_app;
GRANT USAGE, SELECT, UPDATE ON SEQUENCE public.plugins_id_seq TO muster_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON public.plugin_messages TO muster_app;
GRANT USAGE, SELECT, UPDATE ON SEQUENCE public.plugin_messages_id_seq TO muster_app;
