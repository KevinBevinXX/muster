-- muster 0016 — PLUGIN REPLIES: plugin_reply_drafts, the chief's reply to a plugin
-- message that its OWNER approves and the plugin then claims, sends and settles
-- (internal/plugins/drafts.go); and the `no_reply` mark a plugin sets on a
-- message nobody may answer.
--
-- 🔴 THE TABLE IS OWNED AND UNDER FORCED ROW-LEVEL SECURITY, IN 0015's SHAPE. A
-- plugin token acts AS ITS OWNER (authz.ViaPlugin), so owner_isolation is what
-- keeps one owner's plugin from ever claiming another owner's draft. A draft's
-- owner is its message's owner by composite foreign key (message_id, owner_id),
-- and the agent it names is pinned to the same owner by 0015's
-- plugin_rows_owner_is_agents_owner trigger, run here with 'required'.
--
-- 🔴 IT IS RE-APPLIED BY db.RLSOn (rollback.go) WHEN THE TABLE EXISTS, SO EVERY
-- STATEMENT IS IDEMPOTENT: CREATE … IF NOT EXISTS, CREATE OR REPLACE, DROP … IF
-- EXISTS before each trigger and policy. And NOTHING HERE REDEFINES AN OBJECT
-- 0015 DEFINES: RLSOn re-applies 0015's body too, and its CREATE OR REPLACE
-- would silently undo an edit made here to one of its functions.
--
-- 🔴 WHAT WAS DRAFTED IS IMMUTABLE. plugin_reply_drafts_body_immutable refuses an
-- UPDATE of the text, its message, its author's name or its expiry: an approval
-- is of THESE bytes, and nothing may rewrite them after it.
--
-- 🔴 no_reply IS ENFORCED BY THE DATABASE, NOT ONLY BY THE TOOL. A draft for a
-- message marked no_reply is refused by plugin_reply_drafts_message_replyable on
-- INSERT, whatever wrote it; and the mark is immutable once the message exists,
-- so it cannot be lifted to let a draft through.

SET LOCAL lock_timeout = '3s';

ALTER TABLE public.plugin_messages ADD COLUMN IF NOT EXISTS no_reply BOOLEAN NOT NULL DEFAULT false;

-- The mark is part of what ARRIVED: as immutable as the body.
CREATE OR REPLACE FUNCTION public.plugin_messages_no_reply_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.no_reply IS DISTINCT FROM OLD.no_reply THEN
        RAISE EXCEPTION 'plugin_messages: a message''s no_reply mark is immutable (id %)', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS plugin_messages_no_reply_immutable ON public.plugin_messages;
CREATE TRIGGER plugin_messages_no_reply_immutable BEFORE UPDATE OF no_reply ON public.plugin_messages
    FOR EACH ROW EXECUTE FUNCTION public.plugin_messages_no_reply_immutable();

CREATE TABLE IF NOT EXISTS public.plugin_reply_drafts (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id     BIGINT      NOT NULL
                 DEFAULT nullif(current_setting('muster.uid', true), '')::bigint
                 REFERENCES public.users (id),
    message_id   BIGINT      NOT NULL,
    -- SET NULL, NOT CASCADE (0014's reason): the record of what an agent drafted
    -- and what its owner decided outlives the agent. agent_name stays.
    agent_id     BIGINT      REFERENCES public.agents (id) ON DELETE SET NULL,
    agent_name   TEXT        NOT NULL,
    body         TEXT        NOT NULL,
    status       TEXT        NOT NULL DEFAULT 'pending',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    decided_at   TIMESTAMPTZ,
    approved_at  TIMESTAMPTZ,
    claimed_at   TIMESTAMPTZ,
    lease_until  TIMESTAMPTZ,
    finished_at  TIMESTAMPTZ,
    external_ref TEXT        NOT NULL DEFAULT '',
    detail       TEXT        NOT NULL DEFAULT '',
    CONSTRAINT plugin_reply_drafts_message_owner_fkey FOREIGN KEY (message_id, owner_id)
        REFERENCES public.plugin_messages (id, owner_id) ON DELETE CASCADE,
    CONSTRAINT plugin_reply_drafts_status_check CHECK (status IN
        ('pending', 'approved', 'rejected', 'superseded', 'sent', 'failed', 'unconfirmed', 'expired')),
    -- The approved-and-later states carry the approval's time; the undecided and
    -- refused ones (pending, rejected — a withdrawn approval clears it) must not.
    -- `expired` and `superseded` may carry it either way: each is reached from
    -- pending (never approved) OR from an approved, unclaimed draft (expired by
    -- age; superseded because another reply to its message was reported sent).
    CONSTRAINT plugin_reply_drafts_approved_has_time CHECK (
        (status IN ('approved', 'sent', 'failed', 'unconfirmed') AND approved_at IS NOT NULL)
        OR (status IN ('pending', 'rejected') AND approved_at IS NULL)
        OR status IN ('expired', 'superseded')),
    -- A claim is a claimed_at AND a lease, together; a claimed draft is approved or
    -- past it; and a draft settled as sent or failed was claimed (only a plugin that
    -- claimed it can report a result, so a result without a claim is a bug).
    CONSTRAINT plugin_reply_drafts_claim_has_lease CHECK ((claimed_at IS NULL) = (lease_until IS NULL)),
    CONSTRAINT plugin_reply_drafts_claimed_is_approved CHECK (
        claimed_at IS NULL OR status IN ('approved', 'sent', 'failed', 'unconfirmed')),
    CONSTRAINT plugin_reply_drafts_result_was_claimed CHECK (
        status NOT IN ('sent', 'failed') OR (claimed_at IS NOT NULL AND finished_at IS NOT NULL)),
    CONSTRAINT plugin_reply_drafts_body_cap CHECK (octet_length(body) BETWEEN 1 AND 16384),
    CONSTRAINT plugin_reply_drafts_external_ref_cap CHECK (char_length(external_ref) <= 200),
    CONSTRAINT plugin_reply_drafts_detail_cap CHECK (octet_length(detail) <= 4096)
);

-- At most one pending draft per message: a new one supersedes it.
CREATE UNIQUE INDEX IF NOT EXISTS plugin_reply_drafts_one_pending
    ON public.plugin_reply_drafts (message_id) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_plugin_reply_drafts_owner_status ON public.plugin_reply_drafts (owner_id, status, id);
CREATE INDEX IF NOT EXISTS idx_plugin_reply_drafts_agent ON public.plugin_reply_drafts (agent_id, status);

-- 0015's owner trigger: a NEW draft must name its agent, and that agent's owner
-- is the draft's owner; only the foreign key's own SET NULL may clear it.
DROP TRIGGER IF EXISTS plugin_reply_drafts_owner_is_agents_owner ON public.plugin_reply_drafts;
CREATE TRIGGER plugin_reply_drafts_owner_is_agents_owner BEFORE INSERT OR UPDATE OF owner_id, agent_id
    ON public.plugin_reply_drafts FOR EACH ROW EXECUTE FUNCTION public.plugin_rows_owner_is_agents_owner('required');

-- A draft may only answer a message that may be answered. The message is read
-- as the writing principal: one it cannot see reads as "not replyable" and is
-- refused (the composite foreign key would refuse it anyway).
CREATE OR REPLACE FUNCTION public.plugin_reply_drafts_message_replyable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF coalesce((SELECT NOT m.no_reply FROM public.plugin_messages m WHERE m.id = NEW.message_id), false) IS NOT TRUE THEN
        RAISE EXCEPTION 'plugin_reply_drafts: message % may not be replied to (no_reply)', NEW.message_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS plugin_reply_drafts_message_replyable ON public.plugin_reply_drafts;
CREATE TRIGGER plugin_reply_drafts_message_replyable BEFORE INSERT ON public.plugin_reply_drafts
    FOR EACH ROW EXECUTE FUNCTION public.plugin_reply_drafts_message_replyable();

CREATE OR REPLACE FUNCTION public.plugin_reply_drafts_body_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.owner_id IS DISTINCT FROM OLD.owner_id
       OR NEW.message_id IS DISTINCT FROM OLD.message_id
       OR NEW.agent_name IS DISTINCT FROM OLD.agent_name
       OR NEW.body IS DISTINCT FROM OLD.body
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR NEW.expires_at IS DISTINCT FROM OLD.expires_at THEN
        RAISE EXCEPTION 'plugin_reply_drafts: a draft''s body is immutable (id %)', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS plugin_reply_drafts_body_immutable ON public.plugin_reply_drafts;
CREATE TRIGGER plugin_reply_drafts_body_immutable BEFORE UPDATE ON public.plugin_reply_drafts
    FOR EACH ROW EXECUTE FUNCTION public.plugin_reply_drafts_body_immutable();

-- Stated again outside CREATE TABLE so re-applying this body (db.RLSOn, after
-- the rls_off rollback set it to 1) restores the principal default.
ALTER TABLE public.plugin_reply_drafts ALTER COLUMN owner_id
    SET DEFAULT nullif(current_setting('muster.uid', true), '')::bigint;

ALTER TABLE public.plugin_reply_drafts ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.plugin_reply_drafts FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS owner_isolation ON public.plugin_reply_drafts;
CREATE POLICY owner_isolation ON public.plugin_reply_drafts
    AS PERMISSIVE FOR ALL TO PUBLIC
    USING (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    )
    WITH CHECK (
        owner_id = nullif(current_setting('muster.uid', true), '')::bigint
        OR current_setting('muster.scope', true) = 'all'
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON public.plugin_reply_drafts TO muster_app;
GRANT USAGE, SELECT, UPDATE ON SEQUENCE public.plugin_reply_drafts_id_seq TO muster_app;
